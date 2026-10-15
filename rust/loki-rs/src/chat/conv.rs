//! La discussion en mémoire — port de l'état de `Conversation`
//! (`chat_conversation.go`) et de l'abonnement `subscribeSink`.
//!
//! Une seule discussion ouverte, partagée par tous les appareils. Elle porte :
//! - l'état persisté ([`ConvState`] : vue du modèle, journal d'affichage,
//!   Seq, contexte mesuré, bloc projet) et l'identifiant de la discussion
//!   chargée ;
//! - l'epoch, incrémenté à chaque reset ou bascule : il invalide les abonnés
//!   (qui reçoivent `{reset}` et rejouent) et les deltas d'un tour périmé ;
//! - le verrou du tour (`Generating`) et de quoi l'arrêter.
//!
//! Les abonnés ([`Subscription`]) lisent le journal : rejeu coalescé, puis
//! direct. Ils sont réveillés par un compteur de version (`watch`), marqué lu
//! AVANT de relire l'état : aucun réveil n'est perdu, contrairement à une
//! simple notification qui peut partir entre la lecture et l'attente.
//!
//! Hors de ce module : la boucle d'un tour, la file d'attente des messages
//! envoyés pendant un tour, les discussions multiples (index, bascules) et la
//! base — l'appelant charge et enregistre (voir [`super::persist`]).

use std::sync::{Arc, Mutex, MutexGuard};
use std::time::{SystemTime, UNIX_EPOCH};

use serde_json::{Map, Value};
use tokio::sync::watch;

use super::journal::{self, Delta, live_batch, live_reset_plan, replay_plan};
use super::persist::{ConvSnap, ConvState, PersistSink, Persister, decode_state, take_snapshot};

/// Arrêt du tour en cours (`c.cancel`).
pub type CancelTurn = Box<dyn FnOnce() + Send>;

/// Un tour est déjà en cours (`ErrBusy`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Busy;

impl std::fmt::Display for Busy {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("génération en cours")
    }
}

#[derive(Default)]
struct Inner {
    st: ConvState,
    /// Discussion chargée (`ckActive`) : celle qu'annoncent `{reset}` et
    /// `{caught_up}`, et sous laquelle s'enregistrent les instantanés.
    id: String,
    generating: bool,
    epoch: i64,
    cancel: Option<CancelTurn>,
}

/// La discussion ouverte. À partager dans un [`Arc`] (abonnés, tour).
pub struct Conversation {
    inner: Mutex<Inner>,
    version: watch::Sender<u64>,
}

impl Default for Conversation {
    fn default() -> Self {
        Conversation {
            inner: Mutex::new(Inner::default()),
            version: watch::Sender::new(0),
        }
    }
}

fn now_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

impl Conversation {
    pub fn new() -> Arc<Self> {
        Arc::new(Self::default())
    }

    fn lock(&self) -> MutexGuard<'_, Inner> {
        self.inner.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Réveille les abonnés (`cond.Broadcast`).
    fn bump(&self) {
        self.version.send_modify(|v| *v = v.wrapping_add(1));
    }

    /// Identifiant de la discussion chargée.
    pub fn id(&self) -> String {
        self.lock().id.clone()
    }

    pub fn epoch(&self) -> i64 {
        self.lock().epoch
    }

    /// `isGenerating`.
    pub fn is_generating(&self) -> bool {
        self.lock().generating
    }

    /// `loadFrom` : remplace l'état par la discussion `id` (`body` vide = fil
    /// neuf), arrête le tour en cours et invalide les abonnés, qui vident
    /// l'écran et rejouent le nouveau fil.
    pub fn load_from(&self, id: &str, body: &[u8]) {
        self.stop();
        let st = if body.is_empty() {
            ConvState::default()
        } else {
            decode_state(body)
        };
        {
            let mut g = self.lock();
            g.st = st;
            g.id = id.to_string();
            g.generating = false;
            g.cancel = None;
            g.epoch += 1;
        }
        self.bump();
    }

    /// `Reset` : fil vide pour tous les appareils, tour arrêté et verrou
    /// rendu — même un tour coincé (moteur redémarré sous ses pieds) : c'est
    /// le geste qui débloque. L'enregistrement et les fichiers de la
    /// discussion restent à l'appelant.
    pub fn reset(&self) {
        self.stop();
        {
            let mut g = self.lock();
            g.st = ConvState::default();
            g.generating = false;
            g.cancel = None;
            g.epoch += 1;
        }
        self.bump();
    }

    /// `Stop` : arrête le tour en cours (le verrou est rendu par sa fin).
    pub fn stop(&self) {
        let cancel = self.lock().cancel.take();
        if let Some(c) = cancel {
            c();
        }
    }

    /// Prend le verrou du tour ; rend l'epoch du tour, que ses deltas portent.
    pub fn begin_turn(&self, cancel: CancelTurn) -> Result<i64, Busy> {
        let mut g = self.lock();
        if g.generating {
            return Err(Busy);
        }
        g.generating = true;
        g.cancel = Some(cancel);
        Ok(g.epoch)
    }

    /// Fin d'un tour : le journal du tour est coalescé (`compactLogLocked`)
    /// et le verrou rendu. Un tour dont l'epoch est périmé (reset, bascule)
    /// ne touche à rien : le verrou a déjà été rendu, peut-être repris.
    pub fn end_turn(&self, epoch: i64) -> bool {
        {
            let mut g = self.lock();
            if g.epoch != epoch {
                return false;
            }
            let log = std::mem::take(&mut g.st.log);
            g.st.log = journal::compact_log_owned(log);
            g.generating = false;
            g.cancel = None;
        }
        self.bump();
        true
    }

    /// `appendDelta` : journalise un événement d'affichage et réveille les
    /// abonnés. Un delta d'un epoch périmé appartient à l'ancien fil : jeté.
    pub fn append_delta(&self, epoch: i64, delta: Delta) -> bool {
        {
            let mut g = self.lock();
            if g.epoch != epoch {
                return false;
            }
            let Inner { st, .. } = &mut *g;
            journal::append_delta(&mut st.log, &mut st.seq, now_ms(), delta);
        }
        self.bump();
        true
    }

    /// Accès à l'état (vue du modèle, contexte…) sous le verrou, pour le tour.
    /// Ne réveille personne : le journal se modifie par [`Self::append_delta`].
    pub fn with_state<R>(&self, f: impl FnOnce(&mut ConvState) -> R) -> R {
        f(&mut self.lock().st)
    }

    /// Instantané à enregistrer, numéroté sous le même verrou que l'état.
    pub fn snapshot<S: PersistSink>(&self, p: &Persister<S>, project: &str) -> ConvSnap {
        let g = self.lock();
        take_snapshot(&g.st, g.generating, &g.id, p.next_seq(), project)
    }

    /// `state` : instantané léger pour `/api/chat/state`. La tâche Go qui
    /// tiendrait le moteur est ajoutée par l'appelant.
    pub fn state_json(&self) -> Map<String, Value> {
        let g = self.lock();
        let turns =
            g.st.log
                .iter()
                .filter(|ev| ev.delta.contains_key("user"))
                .count();
        let mut m = Map::new();
        m.insert("seq".into(), g.st.seq.into());
        m.insert("generating".into(), g.generating.into());
        m.insert("ctx_used".into(), g.st.ctx_used.into());
        m.insert("turns".into(), turns.into());
        m
    }

    /// Abonnement au journal. `from` : dernier Seq vu par le client ; `tail` :
    /// échanges à rejouer pour un client paginé (`< 0` : tout) ; `conv_id` :
    /// discussion affichée par le client (vide : inconnue).
    pub fn subscribe(self: &Arc<Self>, from: i64, tail: i64, conv_id: &str) -> Subscription {
        Subscription {
            conv: self.clone(),
            rx: self.version.subscribe(),
            from,
            tail,
            conv_id: conv_id.to_string(),
            epoch: 0,
            last: 0,
            started: false,
        }
    }
}

/// Un abonné au journal (`subscribeSink`). Chaque [`Self::next`] rend le lot
/// suivant à émettre, dans l'ordre : d'abord le rejeu (padding, reset éventuel,
/// pagination, rejeu coalescé, `caught_up`, contexte, padding), puis les
/// événements du direct dès qu'il y en a. Lâcher l'abonnement l'arrête.
pub struct Subscription {
    conv: Arc<Conversation>,
    rx: watch::Receiver<u64>,
    from: i64,
    tail: i64,
    conv_id: String,
    epoch: i64,
    last: i64,
    started: bool,
}

impl Subscription {
    /// Lot suivant ; attend s'il n'y a rien de neuf. Jamais vide.
    pub async fn next(&mut self) -> Vec<Delta> {
        loop {
            // Marqué lu AVANT de relire l'état : un ajout qui suit réveille.
            self.rx.borrow_and_update();
            if let Some(events) = self.poll() {
                return events;
            }
            if self.rx.changed().await.is_err() {
                // Discussion détruite : plus rien ne viendra.
                std::future::pending::<()>().await;
            }
        }
    }

    fn poll(&mut self) -> Option<Vec<Delta>> {
        let g = self.conv.lock();
        if !self.started {
            self.started = true;
            // Le journal est copié sous le verrou ; le rejeu se bâtit hors.
            let snapshot = g.st.log.clone();
            let (id, ctx_used) = (g.id.clone(), g.st.ctx_used);
            self.epoch = g.epoch;
            drop(g);
            let plan = replay_plan(
                &snapshot,
                self.from,
                self.tail,
                &self.conv_id,
                &id,
                ctx_used,
            );
            self.last = plan.last;
            return Some(plan.events);
        }
        if g.epoch != self.epoch {
            self.epoch = g.epoch;
            let plan = live_reset_plan(&g.st.log, self.tail, &g.id, g.st.ctx_used);
            self.last = plan.last;
            return Some(plan.events);
        }
        let plan = live_batch(&g.st.log, self.last);
        if plan.events.is_empty() {
            return None;
        }
        self.last = plan.last;
        Some(plan.events)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::chat::persist::{CommitOutcome, ResGate, ToolResJob};
    use serde_json::json;
    use std::sync::atomic::{AtomicBool, Ordering};
    use std::time::Duration;

    fn d(v: Value) -> Delta {
        let Value::Object(m) = v else {
            panic!("objet attendu")
        };
        m
    }

    async fn next(sub: &mut Subscription) -> Vec<Delta> {
        tokio::time::timeout(Duration::from_secs(5), sub.next())
            .await
            .expect("aucun lot")
    }

    fn seqs(evs: &[Delta]) -> Vec<i64> {
        evs.iter()
            .filter_map(|e| e.get("seq").and_then(Value::as_i64))
            .collect()
    }

    #[tokio::test]
    async fn rejeu_puis_direct() {
        let c = Conversation::new();
        c.append_delta(0, d(json!({"user": "salut"})));
        c.append_delta(0, d(json!({"content": "bon"})));
        let mut sub = c.subscribe(0, -1, "");
        let first = next(&mut sub).await;
        assert_eq!(seqs(&first), vec![1, 2]);
        assert!(
            first
                .iter()
                .any(|e| e.get("caught_up") == Some(&json!(true)))
        );
        c.append_delta(0, d(json!({"content": "jour"})));
        assert_eq!(seqs(&next(&mut sub).await), vec![3]);
    }

    #[tokio::test]
    async fn abonne_depuis_un_seq() {
        let c = Conversation::new();
        c.append_delta(0, d(json!({"user": "a"})));
        c.append_delta(0, d(json!({"user": "b"})));
        let mut sub = c.subscribe(1, -1, "");
        assert_eq!(seqs(&next(&mut sub).await), vec![2]);
    }

    #[tokio::test]
    async fn curseur_d_une_autre_discussion_rejoue_tout() {
        let c = Conversation::new();
        c.load_from("c1", b"");
        let e = c.epoch();
        c.append_delta(e, d(json!({"user": "premier message"})));
        c.append_delta(e, d(json!({"content": "réponse"})));
        let mut sub = c.subscribe(9999, -1, "c0");
        let evs = next(&mut sub).await;
        assert_eq!(evs[1], d(json!({"reset": true, "id": "c1"})));
        assert!(
            evs.iter()
                .any(|e| e.get("user") == Some(&json!("premier message")))
        );
    }

    #[tokio::test]
    async fn reset_prevenu_et_deltas_perimes_jetes() {
        let c = Conversation::new();
        let e0 = c.epoch();
        c.append_delta(e0, d(json!({"user": "x"})));
        let mut sub = c.subscribe(0, -1, "");
        next(&mut sub).await;
        c.reset();
        let evs = next(&mut sub).await;
        assert_eq!(evs[0].get("reset"), Some(&json!(true)));
        assert!(!c.append_delta(e0, d(json!({"content": "fantôme"}))));
        assert_eq!(c.state_json()["seq"], 0);
        assert!(c.append_delta(c.epoch(), d(json!({"user": "nouveau"}))));
        assert_eq!(seqs(&next(&mut sub).await), vec![1]);
    }

    #[tokio::test]
    async fn bascule_charge_le_fil_et_previent() {
        let c = Conversation::new();
        c.load_from("a", b"");
        c.append_delta(c.epoch(), d(json!({"user": "dans A"})));
        let mut sub = c.subscribe(0, 20, "a");
        next(&mut sub).await;
        let body = ConvState {
            log: vec![crate::chat::types::LogEvent {
                seq: 1,
                ts: 5,
                delta: d(json!({"user": "dans B"})),
            }],
            seq: 1,
            ctx_used: 77,
            ..Default::default()
        }
        .to_json();
        c.load_from("b", &body);
        let evs = next(&mut sub).await;
        assert_eq!(evs[0], d(json!({"reset": true, "id": "b"})));
        assert_eq!(c.id(), "b");
        assert_eq!(c.state_json()["ctx_used"], 77);
        // Fil court : rien de masqué, le direct rejoue tout depuis 0.
        assert_eq!(seqs(&next(&mut sub).await), vec![1]);
    }

    #[tokio::test]
    async fn verrou_du_tour() {
        let c = Conversation::new();
        let stopped = Arc::new(AtomicBool::new(false));
        let s = stopped.clone();
        let e = c
            .begin_turn(Box::new(move || s.store(true, Ordering::SeqCst)))
            .unwrap();
        assert!(c.is_generating());
        assert_eq!(c.begin_turn(Box::new(|| {})), Err(Busy));
        c.append_delta(e, d(json!({"content": "Bon"})));
        c.append_delta(e, d(json!({"content": "jour"})));
        assert!(c.end_turn(e));
        assert!(!c.is_generating());
        // Fin de tour : journal coalescé.
        c.with_state(|st| {
            assert_eq!(st.log.len(), 1);
            assert_eq!(st.log[0].delta["content"], "Bonjour");
        });
        // Reset pendant un tour : arrêté, verrou rendu, fin périmée sans effet.
        let s = stopped.clone();
        let e = c
            .begin_turn(Box::new(move || s.store(true, Ordering::SeqCst)))
            .unwrap();
        c.reset();
        assert!(stopped.load(Ordering::SeqCst));
        assert!(!c.is_generating());
        let e2 = c.begin_turn(Box::new(|| {})).unwrap();
        assert!(!c.end_turn(e), "fin d'un tour périmé acceptée");
        assert!(c.is_generating());
        assert!(c.end_turn(e2));
    }

    #[tokio::test]
    async fn etat_et_instantane() {
        struct Rien;
        impl PersistSink for Rien {
            async fn commit(
                &self,
                _: Vec<ConvSnap>,
                _: Vec<ToolResJob>,
                _: ResGate,
            ) -> CommitOutcome {
                CommitOutcome::default()
            }
        }
        let p = Persister::new(Rien);
        let c = Conversation::new();
        c.load_from("c9", b"");
        let e = c.begin_turn(Box::new(|| {})).unwrap();
        c.append_delta(e, d(json!({"user": "q"})));
        c.append_delta(e, d(json!({"content": "a"})));
        c.append_delta(e, d(json!({"content": "b"})));
        c.with_state(|st| st.ctx_used = 12);
        assert_eq!(
            Value::Object(c.state_json()),
            json!({"seq": 3, "generating": true, "ctx_used": 12, "turns": 1})
        );
        let s1 = c.snapshot(&p, "generale");
        let s2 = c.snapshot(&p, "generale");
        assert_eq!((s1.id.as_str(), s1.turns), ("c9", 1));
        assert!(s2.seq > s1.seq);
        // En plein tour, l'instantané porte le journal compacté.
        assert_eq!(decode_state(&s1.body).log.len(), 2);
    }
}
