//! Persistance des discussions — port de `chat_persist.go` et de la partie
//! « index » de `chat_sessions.go`.
//!
//! Même format en base que le Go, pour qu'une discussion écrite par l'un se
//! relise par l'autre (et pour la comparaison Go / Rust du lot 6) :
//!
//! ```text
//! chat/index      → liste des discussions (ConvMeta, sans les messages), scellée
//! chat/active     → identifiant de la discussion ouverte (en clair)
//! chat/conv:<id>  → état complet d'une discussion (ConvState), scellé
//! toolres/<id>.<aléa> → résultat complet d'un outil (« voir plus »), scellé
//! ```
//!
//! Trois parties :
//! - le FORMAT : [`ConvState`] (JSON de `Conversation`), son instantané
//!   [`take_snapshot`] et sa relecture tolérante [`decode_state`] ;
//! - l'INDEX : [`ConvMeta`], titres ([`conv_summary`]), tri, projets,
//!   rafraîchissement d'une entrée ([`touch_meta_in`]) ;
//! - l'ÉCRIVAIN UNIQUE : [`Persister`], qui sort l'écriture du chemin du
//!   premier jeton, fusionne ce qui s'accumule (dernier instantané de chaque
//!   discussion + résultats d'outils) en un lot, n'écrit jamais un état plus
//!   ancien qu'un état déjà écrit et ne fait jamais renaître une discussion
//!   supprimée. Le lot part dans un [`PersistSink`] ; [`StoreSink`] l'écrit
//!   par l'API interne du Go ([`crate::store::Store`]), scellé par lui.
//!
//! Écarts assumés avec le Go :
//! - une seule base : le chemin (`path`, `withDBAt`) disparaît des instantanés ;
//! - pas de transaction bbolt à travers l'API interne : [`StoreSink`] écrit les
//!   résultats, puis chaque contenu, puis l'index. Un échec au milieu laisse
//!   des contenus à jour sous un index en retard — le prochain instantané le
//!   rattrape ([`touch_meta_in`] ajoute une entrée absente). Tant que la
//!   conversation est tenue par Rust, le Go ne touche plus à l'index ;
//! - images en base64 → références (`refImagesInMessages`) : à faire par
//!   l'appelant avant [`take_snapshot`], qui ne touche pas au disque ;
//! - relecture : un champ mal typé est ignoré seul (comme `json.Unmarshal`) ;
//!   un message ou un événement mal typé est écarté entier (le Go le garderait
//!   à moitié rempli).

use std::collections::{BTreeMap, HashMap, HashSet};
use std::future::Future;
use std::panic::AssertUnwindSafe;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use futures_util::FutureExt;
use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize, Serializer};
use serde_json::Value;
use tokio::sync::watch;

use super::compact::{IMAGE_LOST_MARKER, is_loki_injected, without_ctx_update};
use super::journal::compact_log;
use super::types::{LogEvent, Message};
use crate::store::{CryptError, Store};

// ---- clés en base ----------------------------------------------------------

/// Bucket des discussions (`bkChat`).
pub const BK_CHAT: &str = "chat";
/// Bucket des résultats d'outils complets (`bkToolRes`).
pub const BK_TOOL_RES: &str = "toolres";
/// Liste des discussions (`ckIndex`).
pub const CK_INDEX: &str = "index";
/// Discussion ouverte (`ckActive`).
pub const CK_ACTIVE: &str = "active";
/// Fil unique d'avant le multi-discussions (`ckLegacy`).
pub const CK_LEGACY: &str = "conversation";
/// Projet d'une discussion qui n'en a pas (`defaultProjectSlug`).
pub const DEFAULT_PROJECT_SLUG: &str = "generale";

/// Délai d'attente de l'écrivain avant une sortie (`persistExitWait`).
pub const PERSIST_EXIT_WAIT: Duration = Duration::from_secs(6);

/// Longueur d'un titre déduit, en caractères (`convSummary`).
const TITLE_RUNES: usize = 60;

/// `convKey` : clé du contenu d'une discussion.
pub fn conv_key(id: &str) -> String {
    format!("conv:{id}")
}

/// `newConvID` : horodatage en nanosecondes — monotone, lisible au tri.
pub fn new_conv_id() -> String {
    let ns = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(0);
    format!("c{ns}")
}

/// `toolResConv` : identifiant de discussion d'une clé « <id>.<aléa> ».
pub fn tool_res_conv(key: &str) -> &str {
    match key.rfind('.') {
        Some(i) if i > 0 => &key[..i],
        _ => key,
    }
}

fn now_unix() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

// ---- format : état d'une discussion ----------------------------------------

/// Bloc projet figé d'une discussion (`projSnapshot`, PROJ_SNAPSHOT). Porté
/// tel quel : la façade ne le calcule pas encore, elle le garde intact.
#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize)]
pub struct ProjSnapshot {
    #[serde(default)]
    pub at: i64,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub msgs: Vec<Message>,
    #[serde(default)]
    pub state: ProjState,
    #[serde(default)]
    pub key: String,
    #[serde(default)]
    pub sys_hash: String,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub snap_tok: i64,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub delta_tok: i64,
}

/// Dernier état du projet annoncé au modèle (`projState`). Les tables sont
/// triées par clé, comme une `map` Go marshalée.
#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize)]
pub struct ProjState {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub day: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub env: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub code: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub desc: String,
    #[serde(default, skip_serializing_if = "is_false")]
    pub mem_on: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub mem_prose: String,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub mem: BTreeMap<String, String>,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub trk: BTreeMap<String, String>,
}

fn is_zero(v: &i64) -> bool {
    *v == 0
}
fn is_false(b: &bool) -> bool {
    !*b
}

/// Ce qui est persisté d'une `Conversation` : la vue du modèle, le journal
/// d'affichage, le compteur de séquence, le contexte mesuré et le bloc projet.
/// Le reste (génération en cours, epoch, file d'attente…) vit en mémoire.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct ConvState {
    pub messages: Vec<Message>,
    pub log: Vec<LogEvent>,
    pub seq: i64,
    pub ctx_used: i64,
    pub proj_snap: Option<ProjSnapshot>,
}

/// Forme écrite, empruntée : l'instantané en plein tour écrit un journal
/// compacté sans toucher à celui que suivent les abonnés.
#[derive(Serialize)]
struct StateOut<'a> {
    #[serde(serialize_with = "nil_if_empty")]
    messages: &'a [Message],
    #[serde(serialize_with = "nil_if_empty")]
    log: &'a [LogEvent],
    seq: i64,
    ctx_used: i64,
    #[serde(skip_serializing_if = "Option::is_none")]
    proj_snap: Option<&'a ProjSnapshot>,
}

/// Une tranche Go vide (nil) s'écrit `null` : même octets que le Go.
fn nil_if_empty<T: Serialize, S: Serializer>(v: &&[T], s: S) -> Result<S::Ok, S::Error> {
    if v.is_empty() {
        s.serialize_none()
    } else {
        v.serialize(s)
    }
}

impl ConvState {
    fn out<'a>(&'a self, log: &'a [LogEvent]) -> StateOut<'a> {
        StateOut {
            messages: &self.messages,
            log,
            seq: self.seq,
            ctx_used: self.ctx_used,
            proj_snap: self.proj_snap.as_ref(),
        }
    }

    /// JSON de la discussion, tel que `json.Marshal(conv)`.
    pub fn to_json(&self) -> Vec<u8> {
        serde_json::to_vec(&self.out(&self.log)).unwrap_or_default()
    }
}

impl Serialize for ConvState {
    fn serialize<S: Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        self.out(&self.log).serialize(s)
    }
}

/// Relit une discussion persistée — `json.Unmarshal` dans une `Conversation`
/// remise à zéro (`loadFrom`), puis la guérison `stripImageParts`. Contenu
/// vide ou illisible → fil neuf ; un champ mal typé est ignoré seul.
pub fn decode_state(b: &[u8]) -> ConvState {
    let mut st = ConvState::default();
    let Ok(Value::Object(mut obj)) = serde_json::from_slice::<Value>(b) else {
        return st;
    };
    if let Some(v) = obj.remove("messages") {
        st.messages = decode_list(v);
    }
    if let Some(v) = obj.remove("log") {
        st.log = decode_list(v);
    }
    if let Some(n) = obj
        .remove("seq")
        .and_then(|v| serde_json::from_value(v).ok())
    {
        st.seq = n;
    }
    if let Some(n) = obj
        .remove("ctx_used")
        .and_then(|v| serde_json::from_value(v).ok())
    {
        st.ctx_used = n;
    }
    if let Some(p) = obj
        .remove("proj_snap")
        .and_then(|v| serde_json::from_value(v).ok())
    {
        st.proj_snap = p;
    }
    strip_image_parts(&mut st.messages);
    st
}

/// Tableau JSON → éléments décodables ; `null` ou autre chose → vide.
fn decode_list<T: DeserializeOwned>(v: Value) -> Vec<T> {
    match v {
        Value::Array(items) => items
            .into_iter()
            .filter_map(|x| serde_json::from_value(x).ok())
            .collect(),
        _ => Vec::new(),
    }
}

/// `stripImageParts` : un message multimodal persisté (base64 d'une capture
/// d'avant les captures éphémères) est aplati en son texte suivi de
/// [`IMAGE_LOST_MARKER`] — rejoué à chaque tour, il dépassait le contexte.
pub fn strip_image_parts(msgs: &mut [Message]) {
    for m in msgs.iter_mut() {
        let Some(Value::Array(parts)) = &m.content else {
            continue;
        };
        let mut texts: Vec<&str> = Vec::new();
        let mut dropped = false;
        for p in parts.iter().filter_map(Value::as_object) {
            if p.get("type").and_then(Value::as_str) == Some("image_url") {
                dropped = true;
                continue;
            }
            if let Some(Value::String(t)) = p.get("text") {
                texts.push(t);
            }
        }
        if dropped {
            let text = texts.join("\n") + IMAGE_LOST_MARKER;
            m.content = Some(Value::String(text));
        }
    }
}

/// Un instantané complet d'une discussion, prêt à écrire (`convSnap`).
#[derive(Debug, Clone, PartialEq)]
pub struct ConvSnap {
    /// Discussion à laquelle appartient le contenu.
    pub id: String,
    /// Ordre de prise : le plus grand l'emporte toujours.
    pub seq: u64,
    /// JSON de la discussion.
    pub body: Vec<u8>,
    /// Titre déduit du premier message ([`conv_summary`]).
    pub title: String,
    /// Échanges : événements `user` du journal.
    pub turns: i64,
    /// Projet d'une entrée d'index à créer.
    pub project: String,
}

/// `snapshot` : instantané de `state` sous la discussion `id`. En plein tour
/// (`generating`), le journal porte encore un événement par jeton : on écrit
/// la forme qu'aura le journal en fin de tour (`compactLog`). `seq` vient de
/// [`Persister::next_seq`], pris sous le même verrou que l'état.
pub fn take_snapshot(
    state: &ConvState,
    generating: bool,
    id: &str,
    seq: u64,
    project: &str,
) -> ConvSnap {
    let body = if generating {
        let compacted = compact_log(&state.log);
        serde_json::to_vec(&state.out(&compacted)).unwrap_or_default()
    } else {
        state.to_json()
    };
    let turns = state
        .log
        .iter()
        .filter(|ev| ev.delta.contains_key("user"))
        .count() as i64;
    ConvSnap {
        id: id.to_string(),
        seq,
        body,
        title: conv_summary(&state.messages),
        turns,
        project: project.to_string(),
    }
}

// ---- index des discussions -------------------------------------------------

/// Une discussion SANS ses messages : ce que liste l'UI (`convMeta`).
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
pub struct ConvMeta {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub created: i64,
    #[serde(default)]
    pub updated: i64,
    #[serde(default)]
    pub turns: i64,
    /// Slug du projet ; vide sur les discussions d'avant les projets.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub project: String,
}

/// `convProjectOf` : projet d'une discussion, l'absence valant le projet par
/// défaut — une discussion sans projet ne doit jamais devenir invisible.
pub fn conv_project_of(m: &ConvMeta) -> &str {
    match m.project.trim() {
        "" => DEFAULT_PROJECT_SLUG,
        s => s,
    }
}

/// Index relu (`convIndex`) : JSON déjà ouvert ; illisible → vide. Plus
/// récemment modifiée en tête, tri stable.
pub fn decode_index(b: &[u8]) -> Vec<ConvMeta> {
    let mut idx: Vec<ConvMeta> = serde_json::from_slice::<Option<Vec<ConvMeta>>>(b)
        .ok()
        .flatten()
        .unwrap_or_default();
    sort_index(&mut idx);
    idx
}

/// Plus récemment modifiée en tête (tri stable, comme `sort.SliceStable`).
pub fn sort_index(idx: &mut [ConvMeta]) {
    idx.sort_by_key(|m| std::cmp::Reverse(m.updated));
}

/// JSON de l'index, tel que `json.Marshal([]convMeta)` (vide → `null`).
pub fn encode_index(idx: &[ConvMeta]) -> Vec<u8> {
    if idx.is_empty() {
        return b"null".to_vec();
    }
    serde_json::to_vec(idx).unwrap_or_default()
}

/// `convIndexForProject` : les discussions d'un projet.
pub fn index_for_project<'a>(idx: &'a [ConvMeta], slug: &str) -> Vec<&'a ConvMeta> {
    idx.iter().filter(|m| conv_project_of(m) == slug).collect()
}

/// `tagOrphanConversations` : rattache au projet les discussions qui n'en ont
/// pas. Rend vrai si l'index a changé (à réécrire).
pub fn tag_orphans(idx: &mut [ConvMeta], slug: &str) -> bool {
    let mut changed = false;
    for m in idx.iter_mut().filter(|m| m.project.trim().is_empty()) {
        m.project = slug.to_string();
        changed = true;
    }
    changed
}

/// Refus d'une opération sur l'index (messages du Go).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum IndexError {
    /// « identifiant ou titre manquant »
    Missing,
    /// « discussion introuvable »
    NotFound,
}

impl std::fmt::Display for IndexError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            IndexError::Missing => "identifiant ou titre manquant",
            IndexError::NotFound => "discussion introuvable",
        })
    }
}

/// `convRename`, sur l'index : titre choisi par l'utilisateur (rogné).
pub fn rename_in(idx: &mut [ConvMeta], id: &str, title: &str) -> Result<(), IndexError> {
    let title = title.trim();
    if id.is_empty() || title.is_empty() {
        return Err(IndexError::Missing);
    }
    let m = idx
        .iter_mut()
        .find(|m| m.id == id)
        .ok_or(IndexError::NotFound)?;
    m.title = title.to_string();
    Ok(())
}

/// Retire une discussion de l'index (`convDelete`) ; faux si absente.
pub fn remove_from(idx: &mut Vec<ConvMeta>, id: &str) -> bool {
    let n = idx.len();
    idx.retain(|m| m.id != id);
    idx.len() != n
}

/// `convTouchMetaIn` : rafraîchit l'entrée d'un instantané (date, échanges,
/// titre déduit si l'utilisateur n'en a pas choisi). Une discussion absente
/// est ajoutée — c'est ce qui recolle une discussion créée mémoire
/// verrouillée.
pub fn touch_meta_in(idx: &mut Vec<ConvMeta>, s: &ConvSnap, now: i64) {
    if let Some(m) = idx.iter_mut().find(|m| m.id == s.id) {
        m.updated = now;
        m.turns = s.turns;
        if m.title.is_empty() {
            m.title = s.title.clone();
        }
        return;
    }
    idx.push(ConvMeta {
        id: s.id.clone(),
        title: s.title.clone(),
        created: now,
        updated: now,
        turns: s.turns,
        project: s.project.clone(),
    });
}

/// `convSummary` : le premier message de l'utilisateur fait le titre (rappels
/// de loki et bloc <context_update> exclus), 60 caractères au plus. Vide sans
/// message : l'UI affiche « Nouvelle discussion ».
pub fn conv_summary(msgs: &[Message]) -> String {
    for m in msgs {
        if m.role != "user" || is_loki_injected(m) {
            continue;
        }
        let stripped = without_ctx_update(m);
        let m = stripped.as_ref().unwrap_or(m);
        let Some(Value::String(s)) = &m.content else {
            continue;
        };
        let s = s.replace('\n', " ");
        let s = s.trim();
        if s.is_empty() {
            continue;
        }
        if s.chars().count() > TITLE_RUNES {
            let head: String = s.chars().take(TITLE_RUNES).collect();
            return format!("{}…", head.trim());
        }
        return s.to_string();
    }
    String::new()
}

// ---- écrivain unique -------------------------------------------------------

/// Un résultat d'outil complet à ranger dans [`BK_TOOL_RES`] (`toolResJob`).
#[derive(Debug, Clone)]
pub struct ToolResJob {
    /// « <discussion>.<aléa> ».
    pub key: String,
    pub plain: Arc<str>,
}

/// Bilan d'un lot : instantanés écrits (discussion, seq) et première erreur.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct CommitOutcome {
    pub wrote: Vec<(String, u64)>,
    pub err: Option<String>,
}

/// Écrit un lot (`persistCommit`). Les résultats d'outils d'abord, chacun
/// sauté si [`ResGate::wanted`] dit qu'une suppression l'a annulé.
pub trait PersistSink: Send + Sync + 'static {
    fn commit(
        &self,
        snaps: Vec<ConvSnap>,
        res: Vec<ToolResJob>,
        gate: ResGate,
    ) -> impl Future<Output = CommitOutcome> + Send;
}

#[derive(Default)]
struct PState {
    /// Dernier instantané en attente, par discussion.
    snaps: HashMap<String, ConvSnap>,
    /// Résultats d'outils en attente, dans l'ordre.
    res: Vec<ToolResJob>,
    /// Résultats pas encore écrits : « voir plus » les sert d'ici.
    mem: HashMap<String, Arc<str>>,
    /// Seq du dernier instantané écrit, par discussion.
    written: HashMap<String, u64>,
    /// Discussions supprimées : plus rien ne doit les réécrire.
    deleted: HashSet<String>,
    queued: u64,
    done: u64,
    running: bool,
    /// Résultats du lot en cours d'écriture, et ceux qu'une suppression a
    /// annulés depuis.
    inflight: HashSet<String>,
    cancelled: HashSet<String>,
}

fn lock(m: &Mutex<PState>) -> MutexGuard<'_, PState> {
    m.lock().unwrap_or_else(|e| e.into_inner())
}

/// Ce que l'écrivain consulte DANS son lot (`toolResWanted`).
#[derive(Clone)]
pub struct ResGate(Arc<Mutex<PState>>);

impl ResGate {
    /// Ce résultat du lot en cours doit-il encore être écrit ?
    pub fn wanted(&self, key: &str) -> bool {
        !lock(&self.0).cancelled.contains(key)
    }
}

struct Shared<S> {
    sink: S,
    st: Arc<Mutex<PState>>,
    done: watch::Sender<u64>,
    seq: AtomicU64,
}

/// L'écrivain unique (`convPersister`). Un seul lot à la fois : l'ordre des
/// instantanés ne dépend que de leur seq. Les méthodes qui confient un
/// travail démarrent l'écrivain sur le runtime tokio courant.
pub struct Persister<S: PersistSink> {
    sh: Arc<Shared<S>>,
}

impl<S: PersistSink> Clone for Persister<S> {
    fn clone(&self) -> Self {
        Persister {
            sh: self.sh.clone(),
        }
    }
}

impl<S: PersistSink> Persister<S> {
    pub fn new(sink: S) -> Self {
        Persister {
            sh: Arc::new(Shared {
                sink,
                st: Arc::new(Mutex::new(PState::default())),
                done: watch::Sender::new(0),
                seq: AtomicU64::new(0),
            }),
        }
    }

    pub fn sink(&self) -> &S {
        &self.sh.sink
    }

    /// Numéro du prochain instantané (`persistSeq`). À prendre sous le verrou
    /// de l'état, pour que l'ordre des numéros soit celui des états.
    pub fn next_seq(&self) -> u64 {
        self.sh.seq.fetch_add(1, Ordering::SeqCst) + 1
    }

    /// Confie un instantané et rend son ticket. Un instantané en attente pour
    /// la même discussion est remplacé par le plus récent.
    pub fn enqueue_snap(&self, s: ConvSnap) -> u64 {
        let mut st = lock(&self.sh.st);
        if !st.deleted.contains(&s.id) {
            let newer = st.snaps.get(&s.id).is_none_or(|cur| s.seq > cur.seq);
            if newer {
                st.snaps.insert(s.id.clone(), s);
            }
        }
        self.kick(&mut st)
    }

    /// Confie un résultat d'outil ; il est servi depuis la mémoire tant qu'il
    /// n'est pas sur disque.
    pub fn enqueue_tool_res(&self, j: ToolResJob) -> u64 {
        let mut st = lock(&self.sh.st);
        if !st.deleted.contains(tool_res_conv(&j.key)) {
            st.mem.insert(j.key.clone(), j.plain.clone());
            st.res.push(j);
        }
        self.kick(&mut st)
    }

    fn kick(&self, st: &mut PState) -> u64 {
        st.queued += 1;
        if !st.running {
            st.running = true;
            tokio::spawn(run(self.sh.clone()));
        }
        st.queued
    }

    /// Attend que le ticket (et tous ceux d'avant) soit traité.
    pub async fn wait(&self, ticket: u64) {
        let mut rx = self.sh.done.subscribe();
        let _ = rx.wait_for(|d| *d >= ticket).await;
    }

    /// Attend que tout ce qui a été confié jusqu'ici soit traité.
    pub async fn flush(&self) {
        let t = lock(&self.sh.st).queued;
        self.wait(t).await;
    }

    /// `flushPersister` : [`Self::flush`] borné, pour ne jamais retenir un
    /// arrêt sur une base bloquée. Faux si le délai a expiré.
    pub async fn flush_timeout(&self, d: Duration) -> bool {
        tokio::time::timeout(d, self.flush()).await.is_ok()
    }

    /// Prend l'instantané et ATTEND son écriture (`persist`) : fin de tour,
    /// reset, bascules. Tout ce qui était en attente avant est écrit aussi.
    pub async fn persist(&self, s: ConvSnap) {
        let t = self.enqueue_snap(s);
        self.wait(t).await;
    }

    /// Marque une discussion supprimée : ses instantanés et résultats en
    /// attente sont jetés, et plus rien ne la réécrira. L'appelant attend
    /// ensuite l'écriture en vol ([`Self::flush`]) avant d'effacer en base.
    pub fn forget(&self, id: &str) {
        let mut st = lock(&self.sh.st);
        st.deleted.insert(id.to_string());
        st.snaps.remove(id);
        drop_tool_res_locked(&mut st, &format!("{id}."));
    }

    /// Oublie les résultats en attente dont la clé commence par `prefix`
    /// (`dropToolRes`). Ceux du lot en vol sont annulés : l'écrivain les
    /// saute, ou les a déjà écrits — l'appelant fait alors [`Self::flush`]
    /// avant d'effacer, et rien ne renaît.
    pub fn drop_tool_res(&self, prefix: &str) {
        drop_tool_res_locked(&mut lock(&self.sh.st), prefix);
    }

    /// Relit un résultat pas encore sur disque (`toolResPending`).
    pub fn tool_res_pending(&self, key: &str) -> Option<Arc<str>> {
        lock(&self.sh.st).mem.get(key).cloned()
    }
}

fn drop_tool_res_locked(st: &mut PState, prefix: &str) {
    let hit: Vec<String> = st
        .inflight
        .iter()
        .filter(|k| k.starts_with(prefix))
        .cloned()
        .collect();
    st.cancelled.extend(hit);
    st.mem.retain(|k, _| !k.starts_with(prefix));
    st.res.retain(|j| !j.key.starts_with(prefix));
}

/// Vide la file par lots, jusqu'à ce qu'elle soit vide.
async fn run<S: PersistSink>(sh: Arc<Shared<S>>) {
    loop {
        let (upto, mut snaps, res) = {
            let mut st = lock(&sh.st);
            if st.snaps.is_empty() && st.res.is_empty() {
                st.running = false;
                st.done = st.queued;
                sh.done.send_replace(st.done);
                return;
            }
            let pending = std::mem::take(&mut st.snaps);
            let snaps: Vec<ConvSnap> = pending
                .into_values()
                .filter(|s| {
                    !st.deleted.contains(&s.id)
                        && s.seq > st.written.get(&s.id).copied().unwrap_or(0)
                })
                .collect();
            let queued = std::mem::take(&mut st.res);
            let mut res = Vec::with_capacity(queued.len());
            for j in queued {
                if !st.deleted.contains(tool_res_conv(&j.key)) {
                    st.inflight.insert(j.key.clone());
                    res.push(j);
                }
            }
            (st.queued, snaps, res)
        };

        snaps.sort_by_key(|s| s.seq);
        let n_snaps = snaps.len();
        let jobs: Vec<(String, Arc<str>)> = res
            .iter()
            .map(|j| (j.key.clone(), j.plain.clone()))
            .collect();
        let gate = ResGate(sh.st.clone());
        // Un écrivain qui panique ne doit pas laisser la file bloquée : les
        // attentes (fin de tour, reset) ne reviendraient jamais.
        let out = AssertUnwindSafe(sh.sink.commit(snaps, res, gate))
            .catch_unwind()
            .await
            .unwrap_or_else(|_| CommitOutcome {
                wrote: Vec::new(),
                err: Some("écrivain en panique".into()),
            });

        let mut st = lock(&sh.st);
        for (key, _) in &jobs {
            st.inflight.remove(key);
            st.cancelled.remove(key);
        }
        for (id, seq) in &out.wrote {
            let w = st.written.entry(id.clone()).or_insert(0);
            if *seq > *w {
                *w = *seq;
            }
        }
        match &out.err {
            Some(e) => {
                // Les résultats restent servis depuis la mémoire ; un
                // instantané raté sera remplacé par le suivant.
                eprintln!(
                    "[persist] écriture différée échouée ({} instantané(s), {} résultat(s) d'outil gardé(s) en mémoire) : {e}",
                    n_snaps.saturating_sub(out.wrote.len()),
                    jobs.len()
                );
            }
            None => {
                for (key, plain) in &jobs {
                    if st.mem.get(key).is_some_and(|m| Arc::ptr_eq(m, plain)) {
                        st.mem.remove(key);
                    }
                }
            }
        }
        st.done = upto;
        sh.done.send_replace(upto);
    }
}

// ---- écriture par l'API interne du Go --------------------------------------

/// Écrit les lots par l'API interne du Go : scellés par lui (la clé de la
/// mémoire ne sort jamais), rangés par lui dans bbolt.
#[derive(Clone)]
pub struct StoreSink {
    pub store: Store,
}

impl PersistSink for StoreSink {
    fn commit(
        &self,
        snaps: Vec<ConvSnap>,
        res: Vec<ToolResJob>,
        gate: ResGate,
    ) -> impl Future<Output = CommitOutcome> + Send {
        commit_batch(&self.store, snaps, res, gate, now_unix())
    }
}

/// `commitPersistBatch` à travers l'API interne. Mémoire verrouillée : rien
/// n'est écrit en clair — les instantanés sont sautés (le fil reste en RAM),
/// les résultats d'outils rendent une erreur (ils restent servis depuis la
/// mémoire). Index illisible (autre clé, abîmé) : les contenus sont écrits,
/// l'index n'est PAS touché — le réécrire depuis une lecture vide effacerait
/// les autres discussions de la liste.
pub async fn commit_batch(
    store: &Store,
    snaps: Vec<ConvSnap>,
    res: Vec<ToolResJob>,
    gate: ResGate,
    now: i64,
) -> CommitOutcome {
    let mut out = CommitOutcome::default();
    for j in res {
        let sealed = match store.seal(j.plain.as_bytes().to_vec()).await {
            Ok(b) => b,
            Err(e) => {
                out.err = Some(crypt_msg(e));
                return out;
            }
        };
        if !gate.wanted(&j.key) {
            continue; // supprimé avec sa discussion pendant que le lot partait
        }
        if let Err(e) = store.put(BK_TOOL_RES, &j.key, sealed).await {
            out.err = Some(e);
            return out;
        }
    }
    if snaps.is_empty() {
        return out;
    }
    let mut idx: Option<Vec<ConvMeta>> = match store.get(BK_CHAT, CK_INDEX).await {
        Ok(None) => Some(Vec::new()),
        Ok(Some(raw)) if raw.is_empty() => Some(Vec::new()),
        Ok(Some(raw)) => match store.open(raw).await {
            Ok(dec) => serde_json::from_slice::<Option<Vec<ConvMeta>>>(&dec)
                .ok()
                .map(Option::unwrap_or_default),
            Err(CryptError::Locked) => return out,
            Err(CryptError::Other(_)) => None,
        },
        Err(e) => {
            out.err = Some(e);
            return out;
        }
    };
    for s in &snaps {
        let sealed = match store.seal(s.body.clone()).await {
            Ok(b) => b,
            Err(CryptError::Locked) => return out,
            Err(e) => {
                out.err = Some(crypt_msg(e));
                return out;
            }
        };
        if let Err(e) = store.put(BK_CHAT, &conv_key(&s.id), sealed).await {
            out.err = Some(e);
            return out;
        }
        if let Some(idx) = idx.as_mut() {
            touch_meta_in(idx, s, now);
        }
        out.wrote.push((s.id.clone(), s.seq));
    }
    let Some(idx) = idx else {
        return out;
    };
    match store.seal(encode_index(&idx)).await {
        Ok(sealed) => {
            if let Err(e) = store.put(BK_CHAT, CK_INDEX, sealed).await {
                out.err = Some(e);
            }
        }
        Err(CryptError::Locked) => {}
        Err(e) => out.err = Some(crypt_msg(e)),
    }
    out
}

fn crypt_msg(e: CryptError) -> String {
    match e {
        CryptError::Locked => "mémoire verrouillée".into(),
        CryptError::Other(s) => s,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use std::sync::Mutex as StdMutex;
    use tokio::sync::{Semaphore, mpsc};

    fn um(s: &str) -> Message {
        Message {
            role: "user".into(),
            content: Some(json!(s)),
            ..Default::default()
        }
    }

    fn ev(seq: i64, delta: Value) -> LogEvent {
        let Value::Object(d) = delta else {
            panic!("delta objet attendu")
        };
        LogEvent {
            seq,
            ts: 0,
            delta: d,
        }
    }

    // ---- format ----

    #[test]
    fn fil_vide_ecrit_comme_le_go() {
        assert_eq!(
            String::from_utf8(ConvState::default().to_json()).unwrap(),
            r#"{"messages":null,"log":null,"seq":0,"ctx_used":0}"#
        );
    }

    #[test]
    fn etat_aller_retour() {
        let st = ConvState {
            messages: vec![um("bonjour")],
            log: vec![ev(1, json!({"user": "bonjour"}))],
            seq: 1,
            ctx_used: 42,
            proj_snap: Some(ProjSnapshot {
                at: 7,
                key: "k".into(),
                state: ProjState {
                    mem: BTreeMap::from([("b".into(), "2".into()), ("a".into(), "1".into())]),
                    ..Default::default()
                },
                ..Default::default()
            }),
        };
        let b = st.to_json();
        assert_eq!(
            String::from_utf8(b.clone()).unwrap(),
            r#"{"messages":[{"role":"user","content":"bonjour"}],"log":[{"seq":1,"ts":0,"delta":{"user":"bonjour"}}],"seq":1,"ctx_used":42,"proj_snap":{"at":7,"state":{"mem":{"a":"1","b":"2"}},"key":"k","sys_hash":""}}"#
        );
        assert_eq!(decode_state(&b), st);
    }

    #[test]
    fn relecture_tolerante() {
        assert_eq!(decode_state(b""), ConvState::default());
        assert_eq!(decode_state(b"pas du json"), ConvState::default());
        assert_eq!(decode_state(b"[1,2]"), ConvState::default());
        // Un champ mal typé est ignoré seul ; null vaut vide.
        let st = decode_state(
            br#"{"messages":[{"role":"user","content":"a"},{"role":5}],"log":null,"seq":"x","ctx_used":12,"inconnu":1}"#,
        );
        assert_eq!(st.messages, vec![um("a")]);
        assert!(st.log.is_empty());
        assert_eq!(st.seq, 0);
        assert_eq!(st.ctx_used, 12);
        assert_eq!(st.proj_snap, None);
    }

    #[test]
    fn guerison_des_images_persistees() {
        let b = json!({"messages": [
            {"role": "user", "content": "bonjour"},
            {"role": "user", "content": [
                {"type": "text", "text": "Voici la capture demandée."},
                {"type": "image_url", "image_url": {"url": "data:image/jpeg;base64,AAAA"}}
            ]},
            {"role": "user", "content": [{"type": "text", "text": "sans image"}]}
        ]});
        let st = decode_state(b.to_string().as_bytes());
        assert_eq!(st.messages[0].content, Some(json!("bonjour")));
        assert_eq!(
            st.messages[1].content,
            Some(json!(format!(
                "Voici la capture demandée.{IMAGE_LOST_MARKER}"
            )))
        );
        assert_eq!(
            st.messages[2].content,
            Some(json!([{"type": "text", "text": "sans image"}]))
        );
    }

    #[test]
    fn instantane_en_plein_tour_ecrit_le_journal_compacte() {
        let st = ConvState {
            messages: vec![um("titre de la discussion")],
            log: vec![
                ev(1, json!({"user": "titre de la discussion"})),
                ev(2, json!({"content": "Bon"})),
                ev(3, json!({"content": "jour"})),
            ],
            seq: 3,
            ..Default::default()
        };
        let s = take_snapshot(&st, true, "c1", 9, "generale");
        assert_eq!((s.id.as_str(), s.seq, s.turns), ("c1", 9, 1));
        assert_eq!(s.title, "titre de la discussion");
        assert_eq!(s.project, "generale");
        let back = decode_state(&s.body);
        assert_eq!(back.log, compact_log(&st.log));
        assert_eq!(back.log.len(), 2);
        // Le journal en mémoire n'a pas bougé ; hors tour, il est écrit tel quel.
        assert_eq!(st.log.len(), 3);
        let s = take_snapshot(&st, false, "c1", 10, "");
        assert_eq!(decode_state(&s.body).log, st.log);
    }

    // ---- index ----

    #[test]
    fn titre_deduit() {
        assert_eq!(conv_summary(&[]), "");
        let mut nudge = um(super::super::compact::THINK_NUDGE_FIRST);
        assert_eq!(conv_summary(&[nudge.clone(), um("bonjour")]), "bonjour");
        nudge.img_relay = true;
        assert_eq!(conv_summary(&[nudge, um("  a\nb  ")]), "a b");
        let assistant = Message {
            role: "assistant".into(),
            content: Some(json!("réponse")),
            ..Default::default()
        };
        let multi = Message {
            role: "user".into(),
            content: Some(json!([{"type": "text", "text": "image"}])),
            ..Default::default()
        };
        assert_eq!(conv_summary(&[assistant, multi, um("   "), um("ok")]), "ok");
        // 60 caractères, découpés sur les caractères et non les octets.
        let long = "é".repeat(59) + " suite";
        assert_eq!(conv_summary(&[um(&long)]), "é".repeat(59) + "…");
    }

    #[test]
    fn titre_sans_bloc_projet() {
        let mut m = um(&format!(
            "{}\n+ x\n{}\n\ncorrige le build",
            super::super::compact::CTX_UPD_OPEN,
            super::super::compact::CTX_UPD_CLOSE
        ));
        m.ctx_upd = true;
        assert_eq!(conv_summary(&[m]), "corrige le build");
    }

    #[test]
    fn entree_d_index_rafraichie_ou_ajoutee() {
        let snap = ConvSnap {
            id: "c1".into(),
            seq: 1,
            body: vec![],
            title: "déduit".into(),
            turns: 3,
            project: "p".into(),
        };
        let mut idx = vec![ConvMeta {
            id: "c1".into(),
            title: "choisi".into(),
            created: 1,
            updated: 1,
            turns: 1,
            project: String::new(),
        }];
        touch_meta_in(&mut idx, &snap, 50);
        assert_eq!(
            idx[0],
            ConvMeta {
                id: "c1".into(),
                title: "choisi".into(),
                created: 1,
                updated: 50,
                turns: 3,
                project: String::new()
            }
        );
        idx[0].title.clear();
        touch_meta_in(&mut idx, &snap, 60);
        assert_eq!(idx[0].title, "déduit");
        let other = ConvSnap {
            id: "c2".into(),
            ..snap
        };
        touch_meta_in(&mut idx, &other, 70);
        assert_eq!(
            idx[1],
            ConvMeta {
                id: "c2".into(),
                title: "déduit".into(),
                created: 70,
                updated: 70,
                turns: 3,
                project: "p".into()
            }
        );
    }

    #[test]
    fn index_tri_projets_et_operations() {
        let raw = br#"[{"id":"a","title":"","created":1,"updated":5,"turns":0},
            {"id":"b","title":"B","created":1,"updated":9,"turns":2,"project":"x"},
            {"id":"c","title":"","created":1,"updated":5,"turns":0,"project":"  "}]"#;
        let mut idx = decode_index(raw);
        let ids: Vec<&str> = idx.iter().map(|m| m.id.as_str()).collect();
        assert_eq!(ids, ["b", "a", "c"], "plus récente en tête, tri stable");
        assert_eq!(decode_index(b"null"), vec![]);
        assert_eq!(decode_index(b"{"), vec![]);

        assert_eq!(conv_project_of(&idx[1]), DEFAULT_PROJECT_SLUG);
        let generale: Vec<&str> = index_for_project(&idx, DEFAULT_PROJECT_SLUG)
            .iter()
            .map(|m| m.id.as_str())
            .collect();
        assert_eq!(generale, ["a", "c"]);
        assert!(tag_orphans(&mut idx, "generale"));
        assert!(!tag_orphans(&mut idx, "generale"));
        assert_eq!(idx[2].project, "generale");

        assert_eq!(rename_in(&mut idx, "a", "  Nom  "), Ok(()));
        assert_eq!(idx[1].title, "Nom");
        assert_eq!(rename_in(&mut idx, "a", "  "), Err(IndexError::Missing));
        assert_eq!(rename_in(&mut idx, "zz", "t"), Err(IndexError::NotFound));
        assert!(remove_from(&mut idx, "b"));
        assert!(!remove_from(&mut idx, "b"));
        assert_eq!(
            String::from_utf8(encode_index(&idx[..1])).unwrap(),
            r#"[{"id":"a","title":"Nom","created":1,"updated":5,"turns":0,"project":"generale"}]"#
        );
        assert_eq!(encode_index(&[]), b"null");
    }

    #[test]
    fn cles() {
        assert_eq!(conv_key("c1"), "conv:c1");
        assert_eq!(tool_res_conv("c123.ab12"), "c123");
        assert_eq!(tool_res_conv("c1.2.x"), "c1.2");
        assert_eq!(tool_res_conv("nosession"), "nosession");
        assert_eq!(tool_res_conv(".x"), ".x");
        let id = new_conv_id();
        assert!(
            id.starts_with('c') && id[1..].parse::<u128>().is_ok(),
            "{id}"
        );
    }

    // ---- écrivain ----

    /// Faux écrivain : enregistre ce qu'il écrit ; retenu au besoin (chaque
    /// lot s'annonce sur `entered` puis attend un jeton de `release`).
    #[derive(Default)]
    struct FakeSink {
        gate: Option<(mpsc::UnboundedSender<Vec<u64>>, Arc<Semaphore>)>,
        seqs: StdMutex<Vec<u64>>,
        disk: StdMutex<HashMap<String, Vec<u8>>>,
        res: StdMutex<HashMap<String, String>>,
        fail: StdMutex<bool>,
        delay: Option<Duration>,
    }

    impl PersistSink for Arc<FakeSink> {
        fn commit(
            &self,
            snaps: Vec<ConvSnap>,
            res: Vec<ToolResJob>,
            gate: ResGate,
        ) -> impl Future<Output = CommitOutcome> + Send {
            let me = self.clone();
            async move {
                if let Some((entered, release)) = &me.gate {
                    let _ = entered.send(snaps.iter().map(|s| s.seq).collect());
                    release.acquire().await.unwrap().forget();
                }
                if let Some(d) = me.delay {
                    tokio::time::sleep(d).await;
                }
                if *me.fail.lock().unwrap() {
                    return CommitOutcome {
                        wrote: vec![],
                        err: Some("disque plein".into()),
                    };
                }
                for j in res {
                    if gate.wanted(&j.key) {
                        me.res.lock().unwrap().insert(j.key, j.plain.to_string());
                    }
                }
                let mut out = CommitOutcome::default();
                for s in snaps {
                    me.seqs.lock().unwrap().push(s.seq);
                    me.disk.lock().unwrap().insert(s.id.clone(), s.body);
                    out.wrote.push((s.id, s.seq));
                }
                out
            }
        }
    }

    fn gated() -> (
        Arc<FakeSink>,
        mpsc::UnboundedReceiver<Vec<u64>>,
        Arc<Semaphore>,
    ) {
        let (tx, rx) = mpsc::unbounded_channel();
        let sem = Arc::new(Semaphore::new(0));
        let sink = Arc::new(FakeSink {
            gate: Some((tx, sem.clone())),
            ..Default::default()
        });
        (sink, rx, sem)
    }

    async fn entered(rx: &mut mpsc::UnboundedReceiver<Vec<u64>>) -> Vec<u64> {
        tokio::time::timeout(Duration::from_secs(5), rx.recv())
            .await
            .expect("aucun lot n'a atteint l'écriture")
            .unwrap()
    }

    fn snap(id: &str, seq: u64, body: &str) -> ConvSnap {
        ConvSnap {
            id: id.into(),
            seq,
            body: body.as_bytes().to_vec(),
            title: String::new(),
            turns: 0,
            project: String::new(),
        }
    }

    fn job(key: &str, plain: &str) -> ToolResJob {
        ToolResJob {
            key: key.into(),
            plain: Arc::from(plain),
        }
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    async fn jamais_un_etat_plus_ancien() {
        let sink = Arc::new(FakeSink {
            delay: Some(Duration::from_millis(2)),
            ..Default::default()
        });
        let p = Persister::new(sink.clone());
        // L'état et son numéro sont pris sous le même verrou, comme sous c.mu.
        let state = Arc::new(StdMutex::new(0u32));
        let mut tasks = Vec::new();
        for _ in 0..8 {
            let (p, state) = (p.clone(), state.clone());
            tasks.push(tokio::spawn(async move {
                for _ in 0..10 {
                    let s = {
                        let mut n = state.lock().unwrap();
                        *n += 1;
                        snap("c1", p.next_seq(), &n.to_string())
                    };
                    p.enqueue_snap(s);
                    tokio::task::yield_now().await;
                }
            }));
        }
        for t in tasks {
            t.await.unwrap();
        }
        p.flush().await;
        let seqs = sink.seqs.lock().unwrap().clone();
        assert!(
            seqs.windows(2).all(|w| w[1] > w[0]),
            "un état plus ancien a écrasé un plus récent : {seqs:?}"
        );
        assert_eq!(sink.disk.lock().unwrap()["c1"], b"80");
    }

    #[tokio::test]
    async fn persist_attend_l_ecriture_en_vol() {
        let (sink, mut rx, release) = gated();
        let p = Persister::new(sink.clone());
        p.enqueue_snap(snap("c1", 1, "avant"));
        assert_eq!(entered(&mut rx).await, vec![1]);
        let p2 = p.clone();
        let done = tokio::spawn(async move { p2.persist(snap("c1", 2, "vide")).await });
        tokio::time::sleep(Duration::from_millis(100)).await;
        assert!(
            !done.is_finished(),
            "revenu avant la fin de l'écriture en vol"
        );
        release.add_permits(100);
        tokio::time::timeout(Duration::from_secs(5), done)
            .await
            .expect("persist bloqué")
            .unwrap();
        assert_eq!(sink.disk.lock().unwrap()["c1"], b"vide");
    }

    #[tokio::test]
    async fn instantane_en_attente_remplace_et_ancien_ignore() {
        let (sink, mut rx, release) = gated();
        let p = Persister::new(sink.clone());
        p.enqueue_snap(snap("c1", 1, "un"));
        entered(&mut rx).await;
        p.enqueue_snap(snap("c1", 3, "trois"));
        p.enqueue_snap(snap("c1", 2, "deux")); // plus ancien que l'attente : écarté
        release.add_permits(100);
        assert_eq!(entered(&mut rx).await, vec![3]);
        p.flush().await;
        // Déjà écrit plus récent : un instantané plus ancien ne part pas.
        p.enqueue_snap(snap("c1", 2, "deux"));
        p.flush().await;
        assert_eq!(*sink.seqs.lock().unwrap(), vec![1, 3]);
        assert_eq!(sink.disk.lock().unwrap()["c1"], b"trois");
    }

    #[tokio::test]
    async fn resultat_servi_avant_l_ecriture() {
        let (sink, mut rx, release) = gated();
        let p = Persister::new(sink.clone());
        let long = "sortie\n".repeat(1000);
        p.enqueue_tool_res(job("c1.a", &long));
        entered(&mut rx).await;
        assert_eq!(p.tool_res_pending("c1.a").as_deref(), Some(long.as_str()));
        release.add_permits(100);
        p.flush().await;
        assert!(
            p.tool_res_pending("c1.a").is_none(),
            "copie en mémoire restée"
        );
        assert_eq!(sink.res.lock().unwrap()["c1.a"], long);
    }

    #[tokio::test]
    async fn resultat_supprime_avant_puis_pendant_l_ecriture() {
        let (sink, mut rx, release) = gated();
        let p = Persister::new(sink.clone());
        p.enqueue_tool_res(job("c1.a", "a")); // retient l'écrivain
        entered(&mut rx).await;
        p.enqueue_tool_res(job("c2.b", "b")); // en attente derrière
        p.drop_tool_res("c2.");
        p.drop_tool_res("c1."); // déjà pris par le lot en vol : annulé
        assert!(p.tool_res_pending("c1.a").is_none());
        release.add_permits(100);
        p.flush().await;
        assert!(
            sink.res.lock().unwrap().is_empty(),
            "un résultat supprimé a été écrit"
        );
        // L'annulation ne vaut que pour ce lot.
        p.enqueue_tool_res(job("c1.c", "c"));
        p.flush().await;
        assert!(sink.res.lock().unwrap().contains_key("c1.c"));
    }

    #[tokio::test]
    async fn suppression_ne_ressuscite_rien() {
        let (sink, mut rx, release) = gated();
        let p = Persister::new(sink.clone());
        p.enqueue_snap(snap("b", 1, "b"));
        entered(&mut rx).await; // un lot est en vol
        p.enqueue_snap(snap("a", 2, "a"));
        p.enqueue_tool_res(job("a.x", "x"));
        p.forget("a");
        p.enqueue_snap(snap("a", 3, "a encore")); // après la suppression : ignoré
        p.enqueue_tool_res(job("a.y", "y"));
        release.add_permits(100);
        p.flush().await;
        assert!(!sink.disk.lock().unwrap().contains_key("a"));
        assert!(sink.res.lock().unwrap().is_empty());
        assert!(p.tool_res_pending("a.y").is_none());
        assert_eq!(sink.disk.lock().unwrap()["b"], b"b");
    }

    #[tokio::test]
    async fn echec_d_ecriture_garde_les_resultats_en_memoire() {
        let sink = Arc::new(FakeSink::default());
        *sink.fail.lock().unwrap() = true;
        let p = Persister::new(sink.clone());
        p.enqueue_snap(snap("c1", 1, "un"));
        p.enqueue_tool_res(job("c1.a", "a"));
        p.flush().await;
        assert_eq!(p.tool_res_pending("c1.a").as_deref(), Some("a"));
        // Le même instantané repart au suivant (rien n'a été marqué écrit).
        *sink.fail.lock().unwrap() = false;
        p.enqueue_snap(snap("c1", 1, "un"));
        p.flush().await;
        assert_eq!(sink.disk.lock().unwrap()["c1"], b"un");
    }

    #[tokio::test]
    async fn ecrivain_en_panique_ne_bloque_pas_la_file() {
        struct Panique;
        impl PersistSink for Panique {
            async fn commit(
                &self,
                _: Vec<ConvSnap>,
                _: Vec<ToolResJob>,
                _: ResGate,
            ) -> CommitOutcome {
                panic!("boum")
            }
        }
        let p = Persister::new(Panique);
        p.enqueue_snap(snap("c1", 1, "un"));
        assert!(
            p.flush_timeout(Duration::from_secs(5)).await,
            "file bloquée"
        );
    }
}
