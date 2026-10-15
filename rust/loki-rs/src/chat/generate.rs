//! Un tour de discussion de bout en bout — port de `EnqueueOrStart`,
//! `StartTurn`, `generate`, `drainQueued`, `startQueuedIfAny`, `dropQueued`
//! et `compactAndPublish` (`chat_conversation.go`).
//!
//! Envoi : doublon d'un réessai reconnu (`cid`), tour préparé par le Go
//! (`/internal/v1/turn/start` : pièces jointes, capacités, rôle du mode Code,
//! puce), mis en file si un tour tourne déjà, sinon lancé : moteur prêt
//! (`/health`), verrou du tour en Rust puis bail du moteur du Go (partagé
//! avec tâches, bench, optimiseur), bulle utilisateur journalisée et
//! enregistrée tout de suite.
//!
//! Le tour (`generate`) : compaction de début de tour, vue d'envoi
//! (`/internal/v1/turn/view` + historique), boucle ([`run_turn`]) dont chaque
//! événement devient un delta du journal, historique rangé (outils, réponse,
//! raisonnement renvoyé, rappels en porte-à-faux retirés), contexte estimé
//! quand le moteur ne le compte pas, compaction de fin de tour, puis fin :
//! `turn_done`, journal coalescé, verrou et bail rendus, discussion
//! enregistrée, et la file — abandonnée après un stop, sinon le message
//! suivant part (ou la notification Web Push s'il n'y en a pas).
//!
//! Mode Code : la boucle du contrat (vérification isolée, relances et
//! corrections du builder) suit le tour, avant la compaction de fin
//! ([`super::verify`]) ; toute compaction rend l'état des critères au résumé.
//!
//! Écarts assumés (restent au Go) : PROJ_SNAPSHOT et KEEP_TURN_IMAGES
//! (refusés par `/turn/view`), préchauffage, télémétrie, motifs
//! `retry_patterns` personnalisés.

use std::collections::VecDeque;
use std::future::Future;
use std::pin::Pin;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use serde::Deserialize;
use serde_json::{Map, Value, json};
use tokio::sync::watch;

use super::compact::{
    CompactCaps, CompactConfig, CompactOpts, Compactor, TurnView, compact_needed, compact_opts_for,
    ctx_pending, estimate_tokens, log_compact, log_ctx_estimate, strip_context_updates,
};
use super::engine::EngineHttp;
use super::history::{
    drop_stray_nudges, history_from_view, inject_prefix, remind_read_mem_pages, tool_used_delta,
};
use super::host::{ChatShared, EngineSummarizer, GoTurnHost, StoreRecall, TurnCompact};
use super::journal::Delta;
use super::lease::{EngineLease, LEASE_TTL, LeaseError, LeaseSignal, engine_stop};
use super::sessions::{GoSessionHooks, Sessions};
use super::turn::{Cancel, TurnConfig, run_turn};
use super::types::{Message, ReasoningEcho, StreamEvent, Tool};
use super::wire::{echo_policy_for, resolve_chat_endpoint, turn_reasoning, with_echo};
use crate::engstatus::llm_port;
use crate::store::BK_STATE;

/// `errModelLoading` : une attente, pas un défaut.
pub const ERR_MODEL_LOADING: &str =
    "⏳ Le modèle est encore en train de charger — réessaie dans quelques secondes.";
/// `ErrBusy`.
pub const ERR_BUSY: &str = "génération en cours";
/// Identifiants d'envoi retenus (`recentCIDs`).
pub const RECENT_CIDS: usize = 64;

/// Un tour préparé par le Go (`/internal/v1/turn/start`).
#[derive(Debug, Clone, Default, Deserialize)]
pub struct Prepared {
    #[serde(default)]
    pub files: Value,
    /// Capacités demandées.
    #[serde(default)]
    pub caps: Value,
    /// Capacités du tour (rôle du mode Code figé).
    #[serde(default)]
    pub turn_caps: Value,
    /// Le message utilisateur vu par le modèle.
    #[serde(default)]
    pub content: Value,
    #[serde(default)]
    pub model: String,
    #[serde(default)]
    pub code_hint: bool,
    /// Texte tapé (bulle du journal).
    #[serde(skip)]
    pub text: String,
    #[serde(skip)]
    pub temperature: f64,
}

impl Prepared {
    fn cap(&self, k: &str) -> bool {
        self.turn_caps
            .get(k)
            .and_then(Value::as_bool)
            .unwrap_or(false)
    }

    /// La bulle utilisateur du journal : texte, modèle qui répond, fichiers.
    fn user_delta(&self) -> Delta {
        let mut d = Map::new();
        d.insert("user".into(), self.text.clone().into());
        if !self.model.is_empty() {
            d.insert("model".into(), self.model.clone().into());
        }
        if self.files.as_array().is_some_and(|f| !f.is_empty()) {
            d.insert("files".into(), self.files.clone());
        }
        d
    }

    fn user_message(&self) -> Message {
        Message {
            role: "user".into(),
            content: Some(self.content.clone()),
            ..Default::default()
        }
    }
}

/// Envoi refusé : statut HTTP de `/api/chat/send` et message.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Refused {
    pub status: u16,
    pub error: String,
}

/// Envoi accepté.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct Sent {
    /// Mis en file derrière le tour en cours.
    pub queued: bool,
    /// Réessai d'un envoi déjà accepté.
    pub dup: bool,
}

enum StartError {
    /// Le tour de la discussion tient déjà le verrou.
    ConvBusy,
    /// Le moteur est tenu par le Go (tâche, bench, optimiseur) : 409.
    Busy(String),
    /// Moteur pas prêt : 503.
    NotReady(String),
}

#[derive(Default)]
struct Queue {
    items: VecDeque<Prepared>,
    recent: VecDeque<String>,
}

pub(super) fn obj(k: &str, v: impl Into<Value>) -> Delta {
    let mut d = Map::new();
    d.insert(k.into(), v.into());
    d
}

type BoxFut = Pin<Box<dyn Future<Output = ()> + Send>>;

/// Ce qu'un tour lit une fois au départ.
pub(super) struct TurnEnv {
    pub(super) cfg: std::collections::HashMap<String, String>,
    pub(super) ep: super::wire::ChatEndpoint,
    pub(super) echo: super::wire::EchoPolicy,
    pub(super) ccfg: CompactConfig,
    pub(super) port: u16,
    pub(super) summarizer: EngineSummarizer,
}

/// Le chat tenu par la façade : la discussion ouverte, ses tours et sa file.
pub struct ChatService {
    pub sessions: Arc<Sessions<GoSessionHooks>>,
    pub shared: Arc<ChatShared>,
    q: Mutex<Queue>,
    lease_ttl: Duration,
}

impl ChatService {
    pub fn new(sessions: Arc<Sessions<GoSessionHooks>>, shared: Arc<ChatShared>) -> Arc<Self> {
        Arc::new(ChatService {
            sessions,
            shared,
            q: Mutex::new(Queue::default()),
            lease_ttl: LEASE_TTL,
        })
    }

    fn queue(&self) -> std::sync::MutexGuard<'_, Queue> {
        self.q.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// `seenCIDLocked` : retient `cid` et dit s'il avait déjà été vu.
    fn seen_cid(&self, cid: &str) -> bool {
        if cid.is_empty() {
            return false;
        }
        let mut q = self.queue();
        if q.recent.iter().any(|c| c == cid) {
            return true;
        }
        q.recent.push_back(cid.to_string());
        while q.recent.len() > RECENT_CIDS {
            q.recent.pop_front();
        }
        false
    }

    /// Un envoi refusé peut être renvoyé : son cid ne reste pas marqué.
    fn forget_cid(&self, cid: &str) {
        self.queue().recent.retain(|c| c != cid);
    }

    /// `/api/chat/send` (`EnqueueOrStart`) : corps tel que l'UI l'envoie.
    pub async fn send(self: &Arc<Self>, body: Value) -> Result<Sent, Refused> {
        let cid = body
            .get("cid")
            .and_then(Value::as_str)
            .unwrap_or("")
            .to_string();
        if self.seen_cid(&cid) {
            return Ok(Sent {
                queued: false,
                dup: true,
            });
        }
        let refuse = |me: &Self, status: u16, error: String| {
            me.forget_cid(&cid);
            Err(Refused { status, error })
        };
        let prep = match self
            .shared
            .store
            .internal_post("/internal/v1/turn/start", &body)
            .await
        {
            Ok((s, v)) if s.as_u16() == 200 => {
                serde_json::from_value::<Prepared>(v).unwrap_or_default()
            }
            Ok((s, v)) => {
                let e = v
                    .get("error")
                    .and_then(Value::as_str)
                    .unwrap_or("")
                    .to_string();
                return refuse(self, if s.as_u16() == 400 { 400 } else { 503 }, e);
            }
            Err(e) => return refuse(self, 503, e),
        };
        let prep = Prepared {
            text: body
                .get("message")
                .and_then(Value::as_str)
                .unwrap_or("")
                .to_string(),
            temperature: body
                .get("temperature")
                .and_then(Value::as_f64)
                .unwrap_or(0.0),
            ..prep
        };
        let conv = self.sessions.conv();
        if conv.is_generating() {
            self.queue().items.push_back(prep);
            return Ok(Sent {
                queued: true,
                dup: false,
            });
        }
        match self.start(prep.clone()).await {
            Ok(()) => Ok(Sent::default()),
            // Un autre tour a démarré entre-temps (deux appareils) : en file.
            Err(StartError::ConvBusy) => {
                self.queue().items.push_back(prep);
                self.start_queued_if_any().await;
                Ok(Sent {
                    queued: true,
                    dup: false,
                })
            }
            Err(StartError::Busy(e)) => refuse(self, 409, e),
            Err(StartError::NotReady(e)) => refuse(self, 503, e),
        }
    }

    /// `healthCheck` : preset externe, toujours prêt ; sinon `/health`.
    async fn engine_ready(&self) -> bool {
        let cfg = self.shared.store.config().await;
        if crate::engstatus::is_external(&cfg) {
            return true;
        }
        let url = format!("http://localhost:{}/health", llm_port(&cfg));
        self.shared.http.health(&url).await
    }

    /// `StartTurn`. Futur en boîte : la fin d'un tour relance la file, donc
    /// `start` → `generate` → `start`, un cycle que le type doit couper.
    fn start(
        self: &Arc<Self>,
        prep: Prepared,
    ) -> Pin<Box<dyn Future<Output = Result<(), StartError>> + Send + '_>> {
        Box::pin(self.start_inner(prep))
    }

    async fn start_inner(self: &Arc<Self>, prep: Prepared) -> Result<(), StartError> {
        if !self.engine_ready().await {
            return Err(StartError::NotReady(ERR_MODEL_LOADING.into()));
        }
        let conv = self.sessions.conv().clone();
        let (tx, cancel) = Cancel::new();
        let tx = Arc::new(tx);
        let t = tx.clone();
        let epoch = conv
            .begin_turn(Box::new(move || {
                let _ = t.send(true);
            }))
            .map_err(|_| StartError::ConvBusy)?;
        let lease = match EngineLease::acquire(self.shared.store.clone(), self.lease_ttl).await {
            Ok(l) => l,
            Err(e) => {
                conv.release_turn(epoch);
                return Err(match e {
                    LeaseError::Busy { error, .. } => StartError::Busy(error),
                    LeaseError::Other(e) => StartError::NotReady(e),
                });
            }
        };
        conv.with_state_if(epoch, |st| st.messages.push(prep.user_message()));
        conv.append_delta(epoch, prep.user_delta());
        if prep.code_hint && !prep.cap("Code") {
            conv.append_delta(epoch, obj("code_hint", true));
        }
        // Instantané tout de suite, écriture par l'écrivain : un crash en
        // pleine génération ne perd pas le message de l'utilisateur.
        self.sessions.persist_async().await;
        let me = self.clone();
        let fut: BoxFut =
            Box::pin(async move { me.generate(prep, epoch, cancel, tx, lease).await });
        tokio::spawn(fut);
        Ok(())
    }

    /// `drainQueued` : messages en file injectés dans le tour en cours,
    /// journalisés pour tous les appareils.
    fn drain_queued(&self, epoch: i64) -> Vec<Message> {
        let conv = self.sessions.conv();
        if conv.epoch() != epoch {
            return Vec::new();
        }
        let items: Vec<Prepared> = self.queue().items.drain(..).collect();
        items
            .into_iter()
            .map(|p| {
                conv.append_delta(epoch, p.user_delta());
                p.user_message()
            })
            .collect()
    }

    /// `startQueuedIfAny` : le prochain message de la file part, s'il en reste
    /// et que rien ne tourne. Rend vrai si un tour a démarré.
    async fn start_queued_if_any(self: &Arc<Self>) -> bool {
        loop {
            let conv = self.sessions.conv();
            if conv.is_generating() {
                return false;
            }
            let Some(next) = self.queue().items.pop_front() else {
                return false;
            };
            let epoch = conv.epoch();
            match self.start(next.clone()).await {
                Ok(()) => return true,
                // Un tour vient de partir : le message garde sa place, ce tour
                // le reprendra (injection ou fin de tour).
                Err(StartError::ConvBusy) => {
                    self.queue().items.push_front(next);
                    return false;
                }
                Err(StartError::Busy(e) | StartError::NotReady(e)) => {
                    conv.append_delta(
                        epoch,
                        obj("error", format!("message en attente non envoyé : {e}")),
                    );
                }
            }
        }
    }

    /// `dropQueued` : bouton stop, l'utilisateur reprend la main.
    fn drop_queued(&self, epoch: i64) {
        let n = std::mem::take(&mut self.queue().items).len();
        if n > 0 {
            self.sessions
                .conv()
                .append_delta(epoch, obj("queue_dropped", n));
        }
    }

    /// `/api/chat/stop` : le tour en cours, ou le détenteur Go du moteur
    /// (tâche planifiée, bench, optimiseur).
    pub async fn stop(&self) {
        let conv = self.sessions.conv();
        if conv.is_generating() {
            conv.stop();
        } else if let Err(e) = engine_stop(&self.shared.store).await {
            eprintln!("[chat] arrêt du détenteur du moteur impossible : {e}");
        }
    }

    /// `compactAndPublish` : bannière, journal, installation du résultat,
    /// jauge de contexte. Rend l'historique compacté, `None` s'il n'a pas
    /// changé.
    #[allow(clippy::too_many_arguments)]
    async fn compact_and_publish(
        &self,
        summarizer: &EngineSummarizer,
        ccfg: &CompactConfig,
        caps: CompactCaps,
        mem_off: bool,
        epoch: i64,
        phase: &str,
        msgs: &[Message],
        ctx_used: i64,
        opt: &CompactOpts,
    ) -> Option<Vec<Message>> {
        let conv = self.sessions.conv();
        conv.append_delta(epoch, obj("compacting", true));
        let recall = StoreRecall(self.shared.store.clone());
        let criteria = if caps.code {
            super::host::live_criteria(&self.shared.store, &conv.id()).await
        } else {
            String::new()
        };
        let c = Compactor {
            cfg: ccfg,
            summarizer,
            recall: &recall,
            memory: &self.shared.compact_memory,
            criteria: &criteria,
        };
        let (compacted, changed) = c.compact_messages_noted(msgs, caps, opt).await;
        conv.append_delta(epoch, obj("compacting", false));
        log_compact(ccfg, phase, ctx_used, msgs, &compacted, changed);
        if !changed {
            conv.append_delta(epoch, obj("compact_noop", true));
            return None;
        }
        let compacted = if caps.agent {
            remind_read_mem_pages(compacted, msgs, mem_off)
        } else {
            compacted
        };
        let compacted = strip_context_updates(compacted);
        let overhead = (ctx_used - estimate_tokens(msgs, &ccfg.echo)).max(0);
        let est = estimate_tokens(&compacted, &ccfg.echo) + overhead;
        conv.with_state_if(epoch, |st| {
            st.messages = compacted.clone();
            st.proj_snap = None;
            st.ctx_used = est;
            st.ctx_used_len = compacted.len();
        });
        conv.append_delta(epoch, obj("compacted", true));
        conv.append_delta(epoch, obj("ctx_used", est));
        Some(compacted)
    }

    /// `/internal/v1/turn/view` : tête (prompt du preset, consignes,
    /// projet), préambule et outils du tour ; `Err` : le message à journaliser.
    pub(super) async fn turn_view(
        &self,
        caps: &Value,
    ) -> Result<(Vec<Message>, String, Vec<Tool>), String> {
        match self
            .shared
            .store
            .internal_post("/internal/v1/turn/view", &json!({ "caps": caps }))
            .await
        {
            Ok((s, v)) if s.as_u16() == 200 => Ok((
                serde_json::from_value::<Vec<Message>>(v["head"].clone()).unwrap_or_default(),
                v["prefix"].as_str().unwrap_or("").to_string(),
                serde_json::from_value::<Vec<Tool>>(v["tools"].clone()).unwrap_or_default(),
            )),
            Ok((_, v)) => Err(v["error"]
                .as_str()
                .unwrap_or("vue du tour refusée")
                .to_string()),
            Err(e) => Err(e),
        }
    }

    /// Configuration, point de chat et résumeur d'un tour (lus une fois).
    pub(super) async fn turn_env(&self) -> TurnEnv {
        let store = &self.shared.store;
        let cfg = store.config().await;
        let local_key = store.get_str(BK_STATE, "api_key").await;
        let ep = resolve_chat_endpoint(&cfg, &local_key);
        let echo = echo_policy_for(&cfg, &self.shared.echo_refusals, None);
        let ccfg = CompactConfig::from_config(&cfg, echo.clone());
        let port = llm_port(&cfg);
        let summarizer = EngineSummarizer {
            http: self.shared.http.clone(),
            ep: ep.clone(),
            cfg: cfg.clone(),
            patterns: self.shared.patterns.clone(),
            port,
        };
        TurnEnv {
            cfg,
            ep,
            echo,
            ccfg,
            port,
            summarizer,
        }
    }

    /// `/api/chat/reset` : fil vide pour tous les appareils, tour arrêté et
    /// verrou rendu, file vidée, fichiers de la discussion effacés.
    pub async fn reset(&self) {
        let conv = self.sessions.conv();
        conv.reset();
        self.queue().items.clear();
        let body = json!({ "id": conv.id(), "files_only": true });
        if let Err(e) = self
            .shared
            .store
            .internal_json(
                hyper::Method::POST,
                "/internal/v1/conv/dropped",
                Some(&body),
            )
            .await
        {
            eprintln!("[chat] fichiers de la discussion non effacés : {e}");
        }
        self.sessions.persist().await;
    }

    /// `/api/chat/compact` (`CompactNow`) : compaction manuelle, en tâche de
    /// fond ; la progression passe par le journal.
    pub async fn compact_now(self: &Arc<Self>) -> Result<(), Refused> {
        if !self.engine_ready().await {
            return Err(Refused {
                status: 503,
                error: ERR_MODEL_LOADING.into(),
            });
        }
        let conv = self.sessions.conv().clone();
        let epoch = conv.begin_turn(Box::new(|| {})).map_err(|_| Refused {
            status: 409,
            error: ERR_BUSY.into(),
        })?;
        let lease = match EngineLease::acquire(self.shared.store.clone(), self.lease_ttl).await {
            Ok(l) => l,
            Err(e) => {
                conv.release_turn(epoch);
                return Err(match e {
                    LeaseError::Busy { error, .. } => Refused { status: 409, error },
                    LeaseError::Other(error) => Refused { status: 503, error },
                });
            }
        };
        let me = self.clone();
        tokio::spawn(async move {
            let env = me.turn_env().await;
            let local = !env.ep.external;
            let snap = conv.with_state_if(epoch, |st| {
                let msgs = st.messages.clone();
                let used = if local {
                    ctx_pending(st.ctx_used, st.ctx_used_len, &msgs, &env.echo)
                } else {
                    st.ctx_used
                };
                (msgs, used)
            });
            if let Some((msgs, used)) = snap {
                let changed = me
                    .compact_and_publish(
                        &env.summarizer,
                        &env.ccfg,
                        CompactCaps::default(),
                        true,
                        epoch,
                        "manuel",
                        &msgs,
                        used,
                        &CompactOpts::default(),
                    )
                    .await
                    .is_some();
                if changed {
                    me.sessions.persist().await;
                }
            }
            conv.release_turn(epoch);
            lease.release().await;
        });
        Ok(())
    }

    /// `/api/chat/tool-result` (`loadToolResult`) : le résultat complet d'un
    /// outil — en mémoire tant qu'il n'est pas écrit, sinon en base.
    pub async fn tool_result(&self, id: &str) -> Option<String> {
        if id.contains(['/', '\\']) {
            return None;
        }
        if let Some(s) = self.shared.persister.tool_res_pending(id) {
            return Some(s.to_string());
        }
        let b = self
            .shared
            .store
            .get_open(super::persist::BK_TOOL_RES, id)
            .await
            .ok()
            .flatten()?;
        Some(String::from_utf8_lossy(&b).into_owned())
    }

    /// Changement de projet (`/api/projects`, action switch), remis par le
    /// Go : refusé pendant un tour.
    pub async fn project_switch(&self, slug: &str) -> Result<(), Refused> {
        if self.sessions.conv().is_generating() {
            return Err(Refused {
                status: 409,
                error: "une génération est en cours".into(),
            });
        }
        self.sessions
            .project_switch(slug)
            .await
            .map_err(|e| Refused {
                status: 400,
                error: e.to_string(),
            })
    }

    /// Suppression d'un projet (`deleteProject`) : ses discussions d'abord,
    /// refusé pendant un tour.
    pub async fn drop_project(&self, slug: &str) -> Result<usize, Refused> {
        if self.sessions.conv().is_generating() {
            return Err(Refused {
                status: 409,
                error: "une génération est en cours".into(),
            });
        }
        self.sessions.drop_project(slug).await.map_err(|e| Refused {
            status: 400,
            error: e.to_string(),
        })
    }

    /// Un événement d'affichage né hors d'un tour (bascule du mode Code,
    /// critères édités), sur le fil courant.
    pub fn push_delta(&self, d: Delta) {
        let conv = self.sessions.conv();
        conv.append_delta(conv.epoch(), d);
    }

    /// `generate` : un tour complet, détaché — seul l'arrêt l'interrompt.
    async fn generate(
        self: Arc<Self>,
        prep: Prepared,
        epoch: i64,
        cancel: Cancel,
        cancel_tx: Arc<watch::Sender<bool>>,
        mut lease: EngineLease,
    ) {
        let turn_start = Instant::now();
        let conv = self.sessions.conv().clone();
        let env = self.turn_env().await;
        let TurnEnv {
            cfg,
            ep,
            echo,
            ccfg,
            port,
            summarizer,
        } = &env;
        let (cfg, ep, echo, ccfg, port, summarizer) = (
            cfg.clone(),
            ep.clone(),
            echo.clone(),
            ccfg.clone(),
            *port,
            summarizer.clone(),
        );
        let local = !ep.external;
        let conv_id = conv.id();
        let agent = prep.cap("Agent");
        let mem_off = prep.turn_caps.get("Mem").and_then(Value::as_str) == Some("off");
        let ccaps = CompactCaps {
            agent,
            code: prep.cap("Code"),
        };
        let ctx_now = |st: &super::persist::ConvState, msgs: &[Message]| {
            if local {
                ctx_pending(st.ctx_used, st.ctx_used_len, msgs, &echo)
            } else {
                st.ctx_used
            }
        };

        // Instantané de la vue modèle et du contexte.
        let snap = conv.with_state_if(epoch, |st| {
            let msgs = st.messages.clone();
            let used = ctx_now(st, &msgs);
            let peak = std::mem::take(&mut st.gen_peak);
            (msgs, used, peak)
        });
        let Some((mut msgs, ctx_used, peak)) = snap else {
            lease.release().await;
            return;
        };

        // Compaction de début de tour.
        let mut start_est = ctx_used;
        if compact_needed(&ccfg, &msgs, ctx_used, peak)
            && !self
                .shared
                .compact_memory
                .refused_skip(&ccfg, &conv_id, ctx_used, &msgs)
        {
            let opt = compact_opts_for(
                &ccfg,
                &self.shared.compact_memory,
                false,
                &msgs,
                ctx_used,
                &conv_id,
                |_| TurnView::default(),
            );
            if let Some(out) = self
                .compact_and_publish(
                    &summarizer,
                    &ccfg,
                    ccaps,
                    mem_off,
                    epoch,
                    "début-tour",
                    &msgs,
                    ctx_used,
                    &opt,
                )
                .await
            {
                msgs = out;
                start_est = conv
                    .with_state_if(epoch, |st| st.ctx_used)
                    .unwrap_or(start_est);
            }
        }

        // Vue d'envoi : tête du Go (prompt du preset, consignes, projet),
        // l'historique sans bloc <context_update>, le préambule.
        let (head, prefix, tools) = match self.turn_view(&prep.turn_caps).await {
            Ok(v) => v,
            Err(e) => {
                conv.append_delta(epoch, obj("error", e));
                self.finish(epoch, &cancel, lease, turn_start).await;
                return;
            }
        };
        if agent {
            // Une discussion qui porte encore des blocs d'une époque où
            // PROJ_SNAPSHOT servait en est nettoyée (comme assembleTurn).
            conv.with_state_if(epoch, |st| {
                if st.proj_snap.is_some() || st.messages.iter().any(|m| m.ctx_upd) {
                    st.proj_snap = None;
                    st.messages = strip_context_updates(std::mem::take(&mut st.messages));
                }
            });
        }
        let hist = strip_context_updates(msgs);
        let mut sent = head;
        sent.extend(hist.iter().cloned());
        let sent = inject_prefix(sent, &prefix);

        let temperature = if prep.temperature == 0.0 {
            0.7
        } else {
            prep.temperature
        };
        let reasoning = turn_reasoning(&cfg, &self.shared.effort);
        let tc = TurnConfig {
            cfg: cfg.clone(),
            ep: ep.clone(),
            tools: tools.clone(),
            temperature,
            code: prep.cap("Code"),
            reasoning: reasoning.clone(),
            echo: echo.clone(),
            compact: ccfg.clone(),
            patterns: self.shared.patterns.clone(),
        };
        let me = self.clone();
        let host = GoTurnHost {
            shared: self.shared.clone(),
            conv_id: conv_id.clone(),
            caps: prep.turn_caps.clone(),
            cfg: cfg.clone(),
            port,
            summarizer: summarizer.clone(),
            compact: TurnCompact {
                cfg: ccfg.clone(),
                caps: ccaps,
                criteria: String::new(),
                main_conv: conv_id.clone(),
                tools,
                effort: reasoning.effort.clone(),
                kwargs: reasoning.kwargs.clone().unwrap_or_default(),
            },
            queued: Box::new(move || me.drain_queued(epoch)),
            perf_kind: "main",
        };

        // Chaque événement devient un delta du journal.
        let mut content = String::new();
        let mut new_base: Option<Vec<Message>> = None;
        let mut final_echo: Option<ReasoningEcho> = None;
        let mut saw_usage = false;
        let mut turn_peak = 0i64;
        let mut asked = false;
        let mut cb = |ev: StreamEvent| -> bool {
            match ev {
                StreamEvent::Err(e) => {
                    conv.append_delta(epoch, obj("error", e));
                }
                StreamEvent::ToolUsed(t) => {
                    // Le texte écrit avant l'appel est déjà dans le message
                    // assistant qui porte l'appel : pas deux fois.
                    content.clear();
                    conv.append_delta(epoch, tool_used_delta(&t));
                }
                StreamEvent::NewHistory(h) => new_base = Some(history_from_view(&h, &hist)),
                StreamEvent::Echo(e) => final_echo = Some(e),
                StreamEvent::Compacting(b) => {
                    conv.append_delta(epoch, obj("compacting", b));
                }
                StreamEvent::Stats(s) => {
                    if s.prompt_tokens_total > 0 {
                        if !saw_usage && local && start_est > 0 {
                            log_ctx_estimate("début-tour", start_est, s.prompt_tokens_total);
                        }
                        saw_usage = true;
                        conv.with_state_if(epoch, |st| st.ctx_used = s.ctx_after());
                    }
                    if local {
                        turn_peak = turn_peak.max(s.gen_tokens);
                    }
                    conv.append_delta(
                        epoch,
                        obj("stats", serde_json::to_value(&s).unwrap_or_default()),
                    );
                }
                StreamEvent::Ask(a) => {
                    // Question structurée (outil ask) : carte à boutons.
                    asked = true;
                    conv.append_delta(
                        epoch,
                        obj("ask", serde_json::to_value(&a).unwrap_or_default()),
                    );
                }
                StreamEvent::DropReasoning => {
                    conv.append_delta(epoch, obj("drop_reasoning", true));
                }
                StreamEvent::Reasoning(r) if !r.is_empty() => {
                    conv.append_delta(epoch, obj("reasoning_content", r));
                }
                StreamEvent::Content(c) if !c.is_empty() => {
                    content.push_str(&c);
                    conv.append_delta(epoch, obj("content", c));
                }
                _ => {}
            }
            true // génération détachée : jamais interrompue par un abonné
        };

        let res = {
            let turn = run_turn(&host, &tc, sent, &cancel, &mut cb);
            tokio::pin!(turn);
            tokio::select! {
                r = &mut turn => r,
                sig = lease.interrupted() => {
                    if sig == LeaseSignal::Lost {
                        eprintln!("[chat] bail du moteur perdu en plein tour : arrêt");
                    }
                    let _ = cancel_tx.send(true);
                    turn.await
                }
            }
        };
        drop(host);

        // Vue modèle rangée : outils puis réponse finale — sauf si un reset
        // ou une bascule est passé entre-temps.
        let after = conv.with_state_if(epoch, |st| {
            if let Some(base) = new_base.take() {
                st.messages = base;
            }
            st.messages.extend(res.extra.iter().cloned());
            if !content.trim().is_empty() {
                let a = Message {
                    role: "assistant".into(),
                    content: Some(Value::String(content.clone())),
                    ..Default::default()
                };
                st.messages.push(with_echo(a, final_echo.as_ref()));
            }
            st.messages = drop_stray_nudges(std::mem::take(&mut st.messages));
            if saw_usage {
                st.ctx_used_len = st.messages.len();
            }
            st.gen_peak = st.gen_peak.max(turn_peak);
            let msgs = st.messages.clone();
            (ctx_now(st, &msgs), st.gen_peak, msgs)
        });
        let Some((mut ctx_used, mut peak, mut msgs)) = after else {
            lease.release().await;
            return; // reset pendant le tour : il a déjà enregistré le fil vide
        };

        // Moteur muet sur l'usage : l'estimation, sinon la jauge reste à zéro.
        if !saw_usage {
            let est = estimate_tokens(&msgs, &echo);
            conv.with_state_if(epoch, |st| {
                st.ctx_used = est;
                st.ctx_used_len = msgs.len();
            });
            conv.append_delta(epoch, obj("ctx_used", est));
            ctx_used = est;
        }

        // Boucle du contrat (mode Code) : vérification indépendante des
        // critères, corrections, re-vérification — AVANT de rendre la main
        // (verify.rs). Rien hors mode Code ou sans critères.
        if !cancel.is_cancelled() {
            let pe = super::verify::PassEnv {
                env: &env,
                turn_caps: &prep.turn_caps,
                temperature,
                reasoning: &reasoning,
                ccaps,
                conv_id: &conv_id,
            };
            let mut run = super::verify::PassRun {
                lease: &mut lease,
                cancel: &cancel,
                cancel_tx: &cancel_tx,
            };
            self.code_verify_loop(&pe, epoch, asked, &mut run).await;
            let after = conv.with_state_if(epoch, |st| {
                let m = st.messages.clone();
                (ctx_now(st, &m), st.gen_peak, m)
            });
            let Some(a) = after else {
                lease.release().await;
                return; // reset pendant la vérification
            };
            (ctx_used, peak, msgs) = a;
        }

        // Compaction de fin de tour : la jauge retombe tout de suite, le tour
        // suivant démarre avec de la marge. Pas après un stop.
        if !cancel.is_cancelled()
            && compact_needed(&ccfg, &msgs, ctx_used, peak)
            && !self
                .shared
                .compact_memory
                .refused_skip(&ccfg, &conv_id, ctx_used, &msgs)
        {
            let opt = compact_opts_for(
                &ccfg,
                &self.shared.compact_memory,
                false,
                &msgs,
                ctx_used,
                &conv_id,
                |_| TurnView::default(),
            );
            self.compact_and_publish(
                &summarizer,
                &ccfg,
                ccaps,
                mem_off,
                epoch,
                "fin-tour",
                &msgs,
                ctx_used,
                &opt,
            )
            .await;
        }
        self.finish(epoch, &cancel, lease, turn_start).await;
    }

    /// Fin d'un tour (le `defer` de `generate`).
    async fn finish(
        self: &Arc<Self>,
        epoch: i64,
        cancel: &Cancel,
        lease: EngineLease,
        turn_start: Instant,
    ) {
        let conv = self.sessions.conv();
        conv.append_delta(epoch, obj("turn_done", true));
        let current = conv.end_turn(epoch);
        lease.release().await;
        if !current {
            return; // reset pendant le tour : il a déjà enregistré le fil vide
        }
        self.sessions.persist().await;
        if cancel.is_cancelled() {
            self.drop_queued(epoch);
            return;
        }
        if !self.start_queued_if_any().await {
            let ms = turn_start.elapsed().as_millis() as u64;
            if let Err(e) = self
                .shared
                .store
                .internal_json(
                    hyper::Method::POST,
                    "/internal/v1/turn/done",
                    Some(&json!({ "ms": ms })),
                )
                .await
            {
                eprintln!("[chat] fin de tour non signalée : {e}");
            }
        }
    }
}

/// Qui tient la conversation, d'après le Go (`/internal/v1/chat/holder`) :
/// `None` : la façade ; `Some(raison)` : le Go la garde (réglage que la façade
/// ne sait pas honorer). Décidé une fois par process, côté Go.
pub async fn go_keeps_chat(store: &crate::store::Store) -> Result<Option<String>, String> {
    let v = store
        .internal_json(hyper::Method::GET, "/internal/v1/chat/holder", None)
        .await?;
    match v["holder"].as_str() {
        Some("rust") => Ok(None),
        Some("go") => Ok(Some(v["reason"].as_str().unwrap_or("").to_string())),
        _ => Err(format!("détenteur de la conversation illisible : {v}")),
    }
}

impl ChatShared {
    /// L'état partagé des tours, autour de l'écrivain des discussions.
    /// `img_dir` : `LOKI_HOME/chatimg`.
    pub fn for_sessions(
        sessions: &Sessions<GoSessionHooks>,
        store: crate::store::Store,
        img_dir: std::path::PathBuf,
    ) -> Self {
        ChatShared {
            http: EngineHttp::new(),
            store,
            persister: sessions.persister().clone(),
            effort: super::wire::EffortFallbacks::new(),
            echo_refusals: super::wire::EchoRefusals::new(),
            compact_memory: super::compact::CompactMemory::new(),
            images: super::host::ImageCache::new(img_dir),
            patterns: super::stream::RetryPatterns::builtin(),
        }
    }
}
