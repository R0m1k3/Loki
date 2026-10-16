//! Plusieurs discussions — port des opérations de `chat_sessions.go` et du
//! chargement au démarrage (`LoadConversation`, `chat_conversation.go`).
//!
//! Une seule discussion en mémoire ([`Conversation`]), rangée dans un tiroir
//! nommé (`chat/conv:<id>`), listée dans `chat/index`, ouverte selon
//! `chat/active` — partagée par tous les appareils. Basculer = enregistrer la
//! courante, charger l'autre et incrémenter l'epoch : les abonnés reçoivent
//! `{reset}` et rejouent le nouveau fil.
//!
//! Les opérations de haut niveau (création, bascule, renommage, suppression)
//! sont sérialisées (`convOpMu`) ; les lectures-modifications-écritures de
//! l'index partagent leur verrou avec l'écrivain ([`StoreSink::index_lock`],
//! `convIndexMu`).
//!
//! Ce qui vit hors de la conversation reste derrière [`SessionHooks`] : les
//! fichiers, travaux, critères et mode d'une discussion supprimée, ses
//! résultats d'outils, le préchauffage après une bascule, et la discussion
//! active vue par les outils du Go ([`GoSessionHooks`]). Le changement de
//! projet ([`Sessions::project_switch`]) se fait ici : la discussion du
//! projet quitté est enregistrée avant que le Go ne change le projet actif
//! ([`SessionHooks::project`]) ; les discussions d'un projet supprimé
//! partent par ici aussi ([`Sessions::drop_project`]).

use std::future::Future;
use std::sync::Arc;
use std::time::Duration;

use super::conv::Conversation;
use super::persist::{
    BK_CHAT, CK_ACTIVE, CK_INDEX, CK_LEGACY, ConvMeta, DEFAULT_PROJECT_SLUG, IndexError, Persister,
    StoreSink, conv_key, decode_index, encode_index, index_for_project, new_conv_id, now_unix,
    remove_from, rename_in,
};
use crate::store::{BK_STATE, Store};

/// Projet actif (`stActiveProjectKey`, bucket `state`).
pub const ST_ACTIVE_PROJECT: &str = "active_project";

/// Essais de lecture au démarrage, et attente entre deux (`loadConvAttempts`,
/// `loadConvRetryWait`) : l'ancien process peut encore tenir la base.
pub const LOAD_ATTEMPTS: u32 = 4;
pub const LOAD_RETRY_WAIT: Duration = Duration::from_millis(250);

/// Ce qui, autour d'une discussion, ne vit pas dans la conversation.
pub trait SessionHooks: Send + Sync + 'static {
    /// Discussion supprimée : ses fichiers, travaux d'arrière-plan, critères,
    /// mode et résultats d'outils partent avec elle.
    fn dropped(&self, id: &str) -> impl Future<Output = ()> + Send;
    /// Bascule faite (`prewarmAfterSwitch`).
    fn switched(&self, id: &str) -> impl Future<Output = ()> + Send;
    /// La discussion ouverte a changé (chargement, création, bascule) : ce
    /// qui lit l'identifiant actif ailleurs (outils du Go) doit suivre.
    fn activated(&self, _id: &str) -> impl Future<Output = ()> + Send {
        async {}
    }
    /// `setActiveProject` : le projet actif change (le Go le range et tient
    /// le cache de ceux qui le lisent : mémoire, trackers, outils).
    fn project(&self, _slug: &str) -> impl Future<Output = Result<(), String>> + Send {
        async { Ok(()) }
    }
}

/// Les effets réels, par l'API interne du Go.
pub struct GoSessionHooks(pub Store);

impl SessionHooks for GoSessionHooks {
    async fn dropped(&self, id: &str) {
        // Ses jobs d'arrière-plan s'arrêtent avec elle (`stopConvJobs`).
        crate::tools::jobs::global().stop_conv(id);
        let body = serde_json::json!({ "id": id });
        if let Err(e) = self
            .0
            .internal_json(
                hyper::Method::POST,
                "/internal/v1/conv/dropped",
                Some(&body),
            )
            .await
        {
            eprintln!("[conv] nettoyage de la discussion {id} non fait : {e}");
        }
    }

    async fn switched(&self, _id: &str) {}

    async fn project(&self, slug: &str) -> Result<(), String> {
        let (status, v) = self
            .0
            .internal_post(
                "/internal/v1/project/activate",
                &serde_json::json!({ "slug": slug }),
            )
            .await?;
        if status.is_success() {
            return Ok(());
        }
        Err(v["error"]
            .as_str()
            .map(str::to_string)
            .unwrap_or_else(|| format!("projet non activé : {status}")))
    }

    async fn activated(&self, id: &str) {
        let body = serde_json::json!({ "id": id });
        if let Err(e) = self
            .0
            .internal_json(hyper::Method::POST, "/internal/v1/conv/active", Some(&body))
            .await
        {
            eprintln!("[conv] discussion active non transmise au Go ({id}) : {e}");
        }
    }
}

/// Refus d'une opération sur les discussions.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SessionError {
    /// « identifiant manquant » / « identifiant ou titre manquant »
    Missing,
    /// « discussion introuvable »
    NotFound,
    /// Base illisible.
    Store(String),
}

impl std::fmt::Display for SessionError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            SessionError::Missing => f.write_str("identifiant ou titre manquant"),
            SessionError::NotFound => f.write_str("discussion introuvable"),
            SessionError::Store(e) => write!(f, "base illisible : {e}"),
        }
    }
}

impl From<IndexError> for SessionError {
    fn from(e: IndexError) -> Self {
        match e {
            IndexError::Missing => SessionError::Missing,
            IndexError::NotFound => SessionError::NotFound,
        }
    }
}

/// Les discussions de l'utilisateur, autour de la discussion ouverte.
pub struct Sessions<H: SessionHooks> {
    store: Store,
    conv: Arc<Conversation>,
    persister: Persister<StoreSink>,
    hooks: H,
    /// `convOpMu`.
    op: tokio::sync::Mutex<()>,
}

impl<H: SessionHooks> Sessions<H> {
    pub fn new(store: Store, conv: Arc<Conversation>, hooks: H) -> Self {
        let persister = Persister::new(StoreSink::new(store.clone()));
        Sessions {
            store,
            conv,
            persister,
            hooks,
            op: tokio::sync::Mutex::new(()),
        }
    }

    pub fn conv(&self) -> &Arc<Conversation> {
        &self.conv
    }

    pub fn persister(&self) -> &Persister<StoreSink> {
        &self.persister
    }

    /// `activeProjectSlug` : projet actif, le projet par défaut à défaut.
    pub async fn active_project(&self) -> String {
        match self.store.get_str(BK_STATE, ST_ACTIVE_PROJECT).await.trim() {
            "" => DEFAULT_PROJECT_SLUG.to_string(),
            s => s.to_string(),
        }
    }

    /// `convIndex` : l'index, plus récente en tête. Illisible ou verrouillé :
    /// vide — la liste est masquée, rien n'est perdu.
    pub async fn read_index(&self) -> Vec<ConvMeta> {
        match self.store.get_open(BK_CHAT, CK_INDEX).await {
            Ok(Some(b)) => decode_index(&b),
            _ => Vec::new(),
        }
    }

    async fn save_index(&self, idx: &[ConvMeta]) {
        if let Err(e) = self
            .store
            .put_seal(BK_CHAT, CK_INDEX, encode_index(idx))
            .await
        {
            eprintln!("[conv] index non enregistré : {e:?}");
        }
    }

    /// Ajoute une entrée à l'index (création, migration).
    async fn index_add(&self, id: &str) {
        let now = now_unix();
        let project = self.active_project().await;
        let _g = self.persister.sink().index_lock.lock().await;
        let mut idx = self.read_index().await;
        idx.push(ConvMeta {
            id: id.to_string(),
            title: String::new(),
            created: now,
            updated: now,
            turns: 0,
            project,
        });
        self.save_index(&idx).await;
    }

    /// `LoadConversation` : recharge la discussion active au démarrage. Une
    /// lecture RATÉE n'est pas une absence : on réessaie, et en cas d'échec
    /// durable on ne touche à rien (ni identifiant forgé, ni fil écrasé).
    pub async fn load(&self) -> Result<(), String> {
        let _op = self.op.lock().await;
        let mut last_err = String::new();
        for attempt in 0..LOAD_ATTEMPTS {
            if attempt > 0 {
                tokio::time::sleep(LOAD_RETRY_WAIT).await;
            }
            // Sonde : tant qu'elle échoue, surtout ne pas créer de discussion.
            if let Err(e) = self.store.get(BK_CHAT, CK_ACTIVE).await {
                last_err = e;
                continue;
            }
            let id = self.ensure_active_locked().await?;
            match self.store.get_open(BK_CHAT, &conv_key(&id)).await {
                Ok(body) => {
                    self.conv.load_from(&id, &body.unwrap_or_default());
                    self.hooks.activated(&id).await;
                    return Ok(());
                }
                Err(e) => last_err = e,
            }
        }
        eprintln!(
            "[conv] base illisible au démarrage ({last_err}) — aucune discussion chargée ; rien n'a été écrasé, l'historique est intact"
        );
        Err(last_err)
    }

    /// `reloadEncryptedStores` : après le déverrouillage de la mémoire,
    /// recharge la discussion si elle a été chargée vide (mémoire verrouillée
    /// au démarrage). Jamais un fil déjà rempli : écraser le tour en cours le
    /// perdrait. Rend vrai si elle a été rechargée.
    pub async fn reload_if_empty(&self) -> Result<bool, String> {
        let _op = self.op.lock().await;
        if self.conv.is_generating() || !self.conv.with_state(|st| st.log.is_empty()) {
            return Ok(false);
        }
        let id = self.ensure_active_locked().await?;
        let Some(body) = self.store.get_open(BK_CHAT, &conv_key(&id)).await? else {
            return Ok(false);
        };
        self.conv.load_from(&id, &body);
        self.hooks.activated(&id).await;
        Ok(true)
    }

    /// `convEnsureActive` : identifiant de la discussion active, créée au
    /// besoin (reprise silencieuse du fil unique d'avant les discussions).
    pub async fn ensure_active(&self) -> Result<String, String> {
        let _op = self.op.lock().await;
        self.ensure_active_locked().await
    }

    async fn ensure_active_locked(&self) -> Result<String, String> {
        let cached = self.conv.id();
        if !cached.is_empty() {
            return Ok(cached);
        }
        if let Some(raw) = self.store.get(BK_CHAT, CK_ACTIVE).await? {
            let id = String::from_utf8_lossy(&raw).trim().to_string();
            if !id.is_empty() {
                return Ok(id);
            }
        }
        let id = new_conv_id();
        if let Some(legacy) = self.store.get(BK_CHAT, CK_LEGACY).await?
            && !legacy.is_empty()
        {
            // Le fil unique devient la première discussion ; la clé d'origine
            // reste intacte (retour arrière sans perte).
            if let Err(e) = self.store.put_seal(BK_CHAT, &conv_key(&id), legacy).await {
                eprintln!("[conv] reprise du fil unique non écrite : {e:?}");
            }
        }
        self.store
            .put(BK_CHAT, CK_ACTIVE, id.as_bytes().to_vec())
            .await?;
        self.index_add(&id).await;
        Ok(id)
    }

    /// `ConvList` : les discussions du projet actif, et l'active.
    pub async fn list(&self) -> Result<(Vec<ConvMeta>, String), String> {
        let project = self.active_project().await;
        let idx = self.read_index().await;
        let list = index_for_project(&idx, &project)
            .into_iter()
            .cloned()
            .collect();
        Ok((list, self.ensure_active().await?))
    }

    /// `convActivate` : pointeur en base, identifiant et contenu changent
    /// ensemble.
    async fn activate(&self, id: &str) -> Result<(), SessionError> {
        self.store
            .put(BK_CHAT, CK_ACTIVE, id.as_bytes().to_vec())
            .await
            .map_err(SessionError::Store)?;
        let body = self
            .store
            .get_open(BK_CHAT, &conv_key(id))
            .await
            .ok()
            .flatten()
            .unwrap_or_default();
        self.conv.load_from(id, &body);
        self.hooks.activated(id).await;
        Ok(())
    }

    /// `persist` : enregistre la discussion ouverte et ATTEND l'écriture.
    pub async fn persist(&self) {
        let project = self.active_project().await;
        let snap = self.conv.snapshot(&self.persister, &project);
        if snap.id.is_empty() {
            self.persister.flush().await;
            return;
        }
        self.persister.persist(snap).await;
    }

    /// `persistAsync` : instantané tout de suite, écriture par l'écrivain.
    pub async fn persist_async(&self) {
        let project = self.active_project().await;
        let snap = self.conv.snapshot(&self.persister, &project);
        if !snap.id.is_empty() {
            self.persister.enqueue_snap(snap);
        }
    }

    /// `convCreate` : discussion vide ouverte SANS enregistrer la courante.
    async fn create_locked(&self) -> Result<String, SessionError> {
        let id = new_conv_id();
        self.index_add(&id).await;
        self.activate(&id).await?;
        Ok(id)
    }

    /// `convNew` : met la courante de côté puis en ouvre une neuve.
    pub async fn new_conv(&self) -> Result<String, SessionError> {
        let _op = self.op.lock().await;
        self.persist().await;
        self.create_locked().await
    }

    /// `convSwitch` : enregistre la courante, charge la cible.
    pub async fn switch(&self, id: &str) -> Result<(), SessionError> {
        let _op = self.op.lock().await;
        if id.is_empty() {
            return Err(SessionError::Missing);
        }
        if !self.read_index().await.iter().any(|m| m.id == id) {
            return Err(SessionError::NotFound);
        }
        if id
            == self
                .ensure_active_locked()
                .await
                .map_err(SessionError::Store)?
        {
            return Ok(());
        }
        self.persist().await;
        self.activate(id).await?;
        self.hooks.switched(id).await;
        Ok(())
    }

    /// `projectSwitch` : la discussion du projet quitté est enregistrée (avec
    /// son projet), le projet actif change, puis la plus récente du nouveau
    /// projet s'ouvre — une neuve s'il n'en a pas. L'appelant vérifie
    /// qu'aucun tour ne tourne.
    pub async fn project_switch(&self, slug: &str) -> Result<(), SessionError> {
        let slug = slug.trim();
        if slug.is_empty() {
            return Err(SessionError::Missing);
        }
        let _op = self.op.lock().await;
        if slug == self.active_project().await {
            return Ok(());
        }
        self.persist().await; // fige la discussion du projet qu'on quitte
        self.hooks
            .project(slug)
            .await
            .map_err(SessionError::Store)?;
        let idx = self.read_index().await;
        match index_for_project(&idx, slug).first() {
            None => self.create_locked().await.map(|_| ()),
            Some(m) => {
                let id = m.id.clone();
                self.activate(&id).await?;
                self.hooks.switched(&id).await;
                Ok(())
            }
        }
    }

    /// Les discussions d'un projet supprimé (`deleteProject`), une à une
    /// comme [`Self::delete`] : supprimer l'ouverte bascule sur la plus
    /// récente restante. Rend leur nombre.
    pub async fn drop_project(&self, slug: &str) -> Result<usize, SessionError> {
        let ids: Vec<String> = index_for_project(&self.read_index().await, slug.trim())
            .into_iter()
            .map(|m| m.id.clone())
            .collect();
        for id in &ids {
            match self.delete(id).await {
                Ok(()) | Err(SessionError::NotFound) => {}
                Err(e) => return Err(e),
            }
        }
        Ok(ids.len())
    }

    /// `convRename`.
    pub async fn rename(&self, id: &str, title: &str) -> Result<(), SessionError> {
        let _op = self.op.lock().await;
        let _g = self.persister.sink().index_lock.lock().await;
        let mut idx = self.read_index().await;
        rename_in(&mut idx, id, title)?;
        self.save_index(&idx).await;
        Ok(())
    }

    /// `convDelete` : supprimer celle qui est ouverte bascule sur la plus
    /// récente restante, ou sur une neuve : il y a TOUJOURS une discussion
    /// active.
    pub async fn delete(&self, id: &str) -> Result<(), SessionError> {
        let _op = self.op.lock().await;
        if !self.read_index().await.iter().any(|m| m.id == id) {
            return Err(SessionError::NotFound);
        }
        // Rien ne doit plus la viser, puis l'écriture en vol est attendue :
        // sinon elle la ferait renaître, contenu et entrée d'index.
        self.persister.forget(id);
        self.persister.flush().await;
        let next = {
            let _g = self.persister.sink().index_lock.lock().await;
            let mut idx = self.read_index().await;
            remove_from(&mut idx, id);
            self.save_index(&idx).await;
            idx
        };
        if let Err(e) = self.store.del(BK_CHAT, &conv_key(id)).await {
            eprintln!("[conv] contenu de {id} non effacé : {e}");
        }
        self.hooks.dropped(id).await;
        if id
            != self
                .ensure_active_locked()
                .await
                .map_err(SessionError::Store)?
        {
            return Ok(());
        }
        match next.first() {
            // Surtout pas new_conv : il réenregistrerait la supprimée.
            None => self.create_locked().await.map(|_| ()),
            Some(m) => self.activate(&m.id.clone()).await,
        }
    }
}
