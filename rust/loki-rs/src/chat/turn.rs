//! La boucle d'un tour — port de `runChatLoop` (`llm_client.go`).
//!
//! Un tour = des complétions enchaînées jusqu'à une réponse : envoi de la
//! requête, lecture du flux ([`StreamParser`]), exécution des appels d'outils
//! puis nouvelle complétion, avec les filets du Go :
//! - reprises réseau AVANT toute réponse (connexion refusée, délai, coupure ;
//!   statuts 429/502/503/504), et flux coupé en cours de réponse vers une API
//!   externe (rejoué, ou poursuivi après le texte déjà affiché) ;
//! - refus du moteur : intensité de raisonnement refusée par le gabarit,
//!   raisonnement renvoyé refusé ou trop long, débordement de contexte
//!   (compaction réactive, puis réduction forcée), 500 sur un appel mal formé
//!   (`tool_choice: none` puis outils retirés) ;
//! - réponse vide : compaction et rejeu si la fenêtre est pleine, sinon
//!   relances « pensé sans agir » ; appel d'outil écrit en texte : relance
//!   corrective ; budget souple d'appels d'outils ; appels identiques
//!   court-circuités ; arguments illisibles jamais exécutés ;
//! - compaction en cours de tour quand la boucle d'outils remplit la fenêtre ;
//! - messages mis en file par l'utilisateur injectés entre deux étapes.
//!
//! La boucle ne fait AUCUNE entrée-sortie elle-même : tout passe par
//! [`TurnHost`] (envoi au moteur, outils, compaction, résultats complets,
//! file d'attente, mémoires des refus). Les événements partent dans le rappel
//! `cb` (comme le `ChatCallback` Go) ; le tour rend ses messages à ajouter à
//! l'historique ([`TurnResult::extra`]).
//!
//! Télémétrie : chaque complétion lue est rangée par le Go (`perfRecord`, par
//! [`TurnHost::perf_record`]) ; la dernière statistique d'une complétion
//! complète porte la nature et la perte de cache qu'il a calculées.
//!
//! Slots du moteur (lot 31) : chaque requête part sur le slot que rend
//! [`TurnHost::engine_slot`] (SIDE_SLOT : 0 pour le fil de la discussion, 1
//! pour une vérification ; aucun sans second slot en service), et un cache KV
//! à court de place en plein calcul est rejoué tel quel. Le rechargement
//! SLOT_PERSIST et le comptage des requêtes en vol passent par le Go (l'hôte,
//! autour de [`TurnHost::engine_slot`] et de [`TurnHost::send`]).
//!
//! Images gardées (KEEP_TURN_IMAGES, lot 32) : une image relayée par un outil
//! est gardée à l'essai jusqu'à la mesure de son coût par le moteur
//! ([`super::keepimg`]) ; les images gardées partent avant tout résumé.
//!
//! Non portés (clés opt-in, ou machinerie du moteur local restée au Go) :
//! PROJ_SNAPSHOT, préchauffage. Avec l'une de ces clés, le Go garde la
//! conversation (`facadeChatBlocker`, décidé au démarrage).

use std::collections::HashMap;
use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::time::Instant;

use bytes::Bytes;
use futures_util::{Stream, StreamExt};
use serde_json::{Map, Value};
use tokio::sync::watch;

use super::compact::cap_tool_result;
use super::compact::{
    CompactConfig, compact_needed, compact_would_trigger, context_overflow, drop_kept_images,
    estimate_tokens, log_compact, log_ctx, log_ctx_estimate, log_kept_images_dropped,
    overflow_tokens, shrink_to_fit,
};
use super::engine::{
    LineError, Lines, NET_RETRIES, NetKind, net_delay, retryable, retryable_status,
    stream_cut_error,
};
use super::keepimg::{KeepImages, has_kept_images, publishable};
use super::nudges::{
    MAX_NUDGES, THINK_NUDGE_FIRST, THINK_NUDGE_STUCK, append_nudge, budget_nudge, dedupable_tool,
    log_budget, repeated_call_result, steer_system, tool_result_preview,
};
use super::stream::{
    CacheHintFn, MAX_PATTERN_RETRIES, PreflightFn, RetryPatterns, StreamConfig, StreamParser,
    call_args, retry_corrective, tool_call_label,
};
use super::types::{
    AskEvent, DiffLine, Message, ReasoningEcho, StatsEvent, StreamEvent, Tool, ToolCall,
    ToolCallFunc, ToolUsedEvent,
};
use super::wire::{
    ChatEndpoint, ChatPayloadOpts, EchoPolicy, TOOLS_OFF_HINT, TurnReasoning, build_chat_payload,
    echo_template_error, effort_from_rejection, log_effort_fallback, reasoning_active,
    reasoning_echo_model, reasoning_template_kwargs, strip_reasoning, turn_echo_policy,
    wire_messages, with_echo,
};

/// Reprises d'un flux coupé en cours de réponse (`maxStreamRetries`).
pub const MAX_STREAM_RETRIES: u32 = 5;
/// Corps d'un refus du moteur lu au plus (octets).
const ERROR_BODY_MAX: usize = 2000;
/// Consigne qui suit le texte déjà affiché quand un flux externe est coupé.
pub const STREAM_CUT_CONTINUE: &str = "The connection was cut during your answer. Continue exactly where you stopped, without repeating what you already wrote.";

/// Corps d'une réponse du moteur.
pub type BodyStream = Pin<Box<dyn Stream<Item = Result<Bytes, String>> + Send>>;

/// Réponse du moteur (statut et corps).
pub struct Reply {
    pub status: u16,
    pub body: BodyStream,
}

/// Échec d'envoi (rien n'est encore parti à l'écran).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SendError {
    pub kind: NetKind,
    /// Message pour l'utilisateur (`friendlyLLMError`).
    pub message: String,
}

/// Ce qu'a produit un appel d'outil (`toolExecResult` du Go).
#[derive(Debug, Clone, PartialEq, Default)]
pub struct ToolOutcome {
    pub result: String,
    pub diff: Vec<DiffLine>,
    pub added: i64,
    pub removed: i64,
    pub ask: Option<AskEvent>,
    /// Capture à afficher dans la bulle (web_screenshot).
    pub image: String,
    /// Aperçu en direct du navigateur piloté.
    pub live_shot: String,
    /// Image relayée au modèle (capture, see_image) : ÉPHÉMÈRE, elle entre dans
    /// la vue du tour mais jamais dans l'historique.
    pub relay: Option<Message>,
}

/// Où en est le tour quand il demande une compaction.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CompactPhase {
    /// Le moteur a refusé un prompt trop long.
    Reactive,
    /// Entre deux étapes : `used` = contexte réel estimé ; `rescue` : relance
    /// de secours (outils coupés ou neutralisés).
    Mid { used: i64, rescue: bool },
    /// Complétion coupée par la fenêtre pleine (finish_reason « length »).
    FullWindow,
}

/// Les effets d'un tour, fournis par l'appelant.
pub trait TurnHost: Send + Sync {
    /// POST du corps vers l'endpoint (sa clé à lui).
    fn send(
        &self,
        ep: &ChatEndpoint,
        body: Vec<u8>,
    ) -> impl Future<Output = Result<Reply, SendError>> + Send;
    /// Exécute un appel d'outil (`execTool`).
    fn exec_tool(
        &self,
        name: &str,
        args: &Map<String, Value>,
        label: &str,
    ) -> impl Future<Output = ToolOutcome> + Send;
    /// Range un résultat complet (« voir plus ») et rend son id ("" : échec).
    fn save_tool_result(&self, result: &str) -> String;
    /// Compacte l'historique ; `None` : rien n'a changé (ou refus mémorisé).
    fn compact(
        &self,
        phase: CompactPhase,
        msgs: &[Message],
    ) -> impl Future<Output = Option<Vec<Message>>> + Send;
    /// Compaction en cours de tour déjà refusée pour cet état
    /// (`compactRefusedSkip`) : on ne la retente pas.
    fn compact_refused(&self, _used: i64, _msgs: &[Message]) -> bool {
        false
    }
    /// Messages mis en file par l'utilisateur, à injecter maintenant.
    fn drain_queued(&self) -> Vec<Message>;
    /// Complétion lue en entier sur le moteur local, outils ni coupés ni
    /// neutralisés : la sonde de gabarit peut rejouer sa forme
    /// (`tplProbeKick`).
    fn completed(&self, _tools: &[Tool], _kwargs: Option<&Map<String, Value>>, _effort: &str) {}
    /// La sonde a-t-elle conclu « préfixe instable » pour CE modèle et pour
    /// cette forme de requête (`nudgeInToolOn`, hors clé et preset externe) ?
    fn prefix_unstable(
        &self,
        _tools: &[Tool],
        _kwargs: Option<&Map<String, Value>>,
        _effort: &str,
    ) -> bool {
        false
    }
    /// Prépare ce que [`Self::image_data_url`] devra rendre pour ces messages
    /// (lecture et déchiffrement des images rangées), avant chaque requête.
    fn prepare_images(&self, _msgs: &[Message]) -> impl Future<Output = ()> + Send {
        async {}
    }
    /// Image rangée (`loki-img:…`) → data-URL.
    fn image_data_url(&self, r: &str) -> Option<String>;
    /// Intensité refusée par le gabarit : traduction retenue.
    fn effort_remember(&self, want: &str, got: &str);
    /// Gabarit qui refuse le raisonnement renvoyé : retenu pour ce modèle.
    fn echo_refuse(&self, model: &str);
    /// `noteEnginePrompt` (moteur local).
    fn cache_hint(&self) -> Option<CacheHintFn> {
        None
    }
    /// Garde d'écriture du mode Code (`codeWriteGuard`).
    fn preflight(&self) -> Option<PreflightFn> {
        None
    }
    /// KEEP_TURN_IMAGES : le budget des images gardées si la clé vaut pour
    /// ce tour (fil principal, vision active) ; `None` : images éphémères.
    fn keep_images(&self) -> impl Future<Output = Option<i64>> + Send {
        async { None }
    }
    /// Range par référence l'image d'un relais (`refRelayImage`) : le
    /// message qui la porte ainsi et son poids en octets ; `None` : elle
    /// reste éphémère.
    fn keep_image(&self, _m: &Message) -> impl Future<Output = Option<(Message, i64)>> + Send {
        async { None }
    }
    /// Slot du moteur où envoyer la requête qui part (`engineSlotFor`) ;
    /// `None` : aucun `id_slot`. Appelé avant chaque requête, moteur local ou
    /// non : l'hôte y recharge aussi l'état SLOT_PERSIST de la discussion.
    fn engine_slot(&self, _ep: &ChatEndpoint) -> impl Future<Output = Option<u8>> + Send {
        async { None }
    }
    /// Télémétrie d'une complétion (`perfRecord`) : la rend au Go, qui la
    /// range dans l'anneau de `/api/perf/summary` et dit quoi poser sur la
    /// copie finale des stats (nature, jetons perdus, alerte).
    fn perf_record(&self, _rec: PerfRec) -> impl Future<Output = PerfMarks> + Send {
        async { PerfMarks::default() }
    }
}

/// Une complétion telle que la boucle l'a vue, pour la télémétrie.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct PerfRec {
    /// Rang de la complétion dans le tour (`perfIter`).
    pub iter: i64,
    pub complete: bool,
    pub external: bool,
    pub stats: StatsEvent,
    /// Empreintes cumulées des messages envoyés ([`perf_prefix`]), si complète.
    pub prefix: Vec<u64>,
}

/// Ce que la télémétrie pose sur la copie finale des stats.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct PerfMarks {
    pub kind: String,
    pub lost: Option<i64>,
    pub lost_after: String,
    pub lost_alert: bool,
}

/// `perfPrefix` : empreinte FNV-1a 64 cumulée de chaque message envoyé (son
/// JSON suivi d'un octet nul). Comparée seulement aux empreintes des envois
/// précédents de la même discussion, toutes calculées ici.
pub fn perf_prefix(msgs: &[Message]) -> Vec<u64> {
    const OFFSET: u64 = 0xcbf2_9ce4_8422_2325;
    const PRIME: u64 = 0x0000_0100_0000_01b3;
    let fnv = |h: u64, b: u8| (h ^ u64::from(b)).wrapping_mul(PRIME);
    let mut h = OFFSET;
    let mut out = Vec::with_capacity(msgs.len());
    for m in msgs {
        for b in serde_json::to_vec(m).unwrap_or_default() {
            h = fnv(h, b);
        }
        h = fnv(h, 0);
        out.push(h);
    }
    out
}

/// Ce que le tour sait avant de commencer (lu UNE fois).
#[derive(Clone)]
pub struct TurnConfig {
    /// `ReadConfig()`.
    pub cfg: HashMap<String, String>,
    pub ep: ChatEndpoint,
    pub tools: Vec<Tool>,
    pub temperature: f64,
    /// Mode Code : palier du budget triplé, garde d'écriture.
    pub code: bool,
    pub reasoning: TurnReasoning,
    /// Politique d'écho courante (`currentEchoPolicy`).
    pub echo: EchoPolicy,
    pub compact: CompactConfig,
    pub patterns: Arc<RetryPatterns>,
}

/// Fin du tour : messages à ajouter à l'historique, et l'erreur s'il y en a
/// une (déjà émise dans le rappel).
#[derive(Debug, Clone, PartialEq, Default)]
pub struct TurnResult {
    pub extra: Vec<Message>,
    pub err: Option<String>,
}

/// Arrêt du tour (bouton stop, reset, bascule).
#[derive(Clone)]
pub struct Cancel {
    rx: watch::Receiver<bool>,
    _keep: Option<Arc<watch::Sender<bool>>>,
}

impl Cancel {
    /// Un arrêt et ce qui le déclenche (`send(true)`).
    pub fn new() -> (watch::Sender<bool>, Cancel) {
        let (tx, rx) = watch::channel(false);
        (tx, Cancel { rx, _keep: None })
    }

    /// Jamais arrêté.
    pub fn never() -> Cancel {
        let (tx, rx) = watch::channel(false);
        Cancel {
            rx,
            _keep: Some(Arc::new(tx)),
        }
    }

    pub fn is_cancelled(&self) -> bool {
        *self.rx.borrow()
    }

    /// Se résout à l'arrêt.
    pub async fn cancelled(&self) {
        let mut rx = self.rx.clone();
        if rx.wait_for(|c| *c).await.is_err() {
            std::future::pending::<()>().await;
        }
    }

    /// `f`, ou `None` si l'arrêt arrive avant.
    async fn or_stop<F: Future>(&self, f: F) -> Option<F::Output> {
        tokio::select! {
            biased;
            _ = self.cancelled() => None,
            v = f => Some(v),
        }
    }

    /// `llmNetBackoff` : attend le délai de la n-ième reprise ; faux si arrêté.
    async fn backoff(&self, n: u32) -> bool {
        self.or_stop(tokio::time::sleep(net_delay(n as i64)))
            .await
            .is_some()
    }
}

fn log_llm_retry(attempt: u32, cause: &str) {
    eprintln!("[llm] {cause} — nouvelle tentative {attempt}/{NET_RETRIES}");
}

fn log_stream_retry(attempt: u32, cause: &str) {
    eprintln!(
        "[llm] flux coupé en cours de réponse ({cause}) — reprise {attempt}/{MAX_STREAM_RETRIES}"
    );
}

/// `sideSlotPoolError` : « Context size has been exceeded. » sans le nombre
/// de jetons de la requête n'est pas un prompt trop long, c'est le cache KV
/// qui a manqué de place pendant le calcul — avec deux slots, la faute du
/// voisin, pas de la conversation.
pub fn side_slot_pool_error(msg: &str) -> bool {
    static REQ_EXCEEDS: std::sync::LazyLock<regex::Regex> = std::sync::LazyLock::new(|| {
        regex::Regex::new(r"(?i)exceeds the available context size|request \([0-9]+ tokens\)")
            .expect("motif fixe")
    });
    msg.to_lowercase()
        .contains("context size has been exceeded")
        && !REQ_EXCEEDS.is_match(msg)
}

fn text_msg(role: &str, text: impl Into<String>) -> Message {
    Message {
        role: role.into(),
        content: Some(Value::String(text.into())),
        ..Default::default()
    }
}

/// `LLMPort` pour les messages d'erreur.
fn engine_port(cfg: &HashMap<String, String>) -> u16 {
    cfg.get("PORT")
        .and_then(|p| p.trim().parse::<u16>().ok())
        .filter(|p| *p > 0)
        .unwrap_or(8080)
}

/// Lit au plus [`ERROR_BODY_MAX`] octets d'un corps de refus.
async fn read_error_body(mut body: BodyStream) -> String {
    let mut buf = Vec::new();
    while buf.len() < ERROR_BODY_MAX {
        match body.next().await {
            Some(Ok(b)) => buf.extend_from_slice(&b),
            _ => break,
        }
    }
    buf.truncate(ERROR_BODY_MAX);
    String::from_utf8_lossy(&buf).trim().to_string()
}

fn status_text(status: u16) -> String {
    let reason = hyper::StatusCode::from_u16(status)
        .ok()
        .and_then(|s| s.canonical_reason())
        .unwrap_or("");
    format!("{status} {reason}").trim().to_string()
}

/// `fillToolResult` : aperçu pour l'UI, taille réelle, et l'id du résultat
/// complet s'il est coupé (envoyé entier si l'enregistrement échoue).
fn fill_tool_result<H: TurnHost>(host: &H, mut ev: ToolUsedEvent, result: &str) -> ToolUsedEvent {
    let (prev, n, cut) = tool_result_preview(result);
    ev.result_chars = n as i64;
    ev.result = result.to_string();
    if cut {
        let id = host.save_tool_result(result);
        if !id.is_empty() {
            ev.result = prev.to_string();
            ev.result_id = id;
        }
    }
    ev
}

/// État d'un tour, d'une complétion à l'autre.
#[derive(Default)]
struct Loop {
    disable_tools: bool,
    tool_choice_none: bool,
    compacted_retry: bool,
    shrink_retries: u32,
    effort_retried: bool,
    done_calls: HashMap<String, String>,
    repeat_count: HashMap<String, i64>,
    nudge_count: i64,
    peak_gen: i64,
    length_replays: u32,
    last_est: i64,
    pattern_retries: i64,
    net_retries: u32,
    stream_retries: u32,
    echo_off_turn: bool,
    echo_tpl_retry: bool,
    tool_runs: i64,
    budget_nudges: i64,
    /// `perfIter` : complétions de ce tour, complètes ou non.
    perf_iter: i64,
    /// Rejeux « cache KV plein » (SIDE_SLOT).
    pool_retries: u32,
}

/// `runChatTools` : le tour, de la première requête à la réponse finale.
/// KEEP_TURN_IMAGES : en fin de tour, une image gardée à l'essai mais jamais
/// mesurée redevient éphémère, quelle que soit la sortie de la boucle.
pub async fn run_turn<H: TurnHost>(
    host: &H,
    tc: &TurnConfig,
    messages: Vec<Message>,
    cancel: &Cancel,
    cb: &mut (dyn FnMut(StreamEvent) -> bool + Send),
) -> TurnResult {
    let mut kt = host.keep_images().await.map(KeepImages::new);
    let mut r = run_turn_loop(host, tc, messages, cancel, cb, &mut kt).await;
    if let Some(k) = kt.as_mut() {
        k.finish(&mut r.extra);
    }
    r
}

/// `publishHistory` : publie l'historique réécrit en cours de tour
/// (compaction, réduction, retrait des images gardées). KEEP_TURN_IMAGES :
/// une image non gardée n'y entre pas ; sans la clé, tel quel.
fn publish_history(
    cb: &mut (dyn FnMut(StreamEvent) -> bool + Send),
    kt: &mut Option<KeepImages>,
    m: &[Message],
) {
    if let Some(k) = kt.as_mut() {
        k.rewritten();
    }
    cb(StreamEvent::NewHistory(publishable(kt.as_ref(), m)));
}

/// `runChatLoop` : la boucle de [`run_turn`].
async fn run_turn_loop<H: TurnHost>(
    host: &H,
    tc: &TurnConfig,
    mut messages: Vec<Message>,
    cancel: &Cancel,
    cb: &mut (dyn FnMut(StreamEvent) -> bool + Send),
    kt: &mut Option<KeepImages>,
) -> TurnResult {
    let mut extra: Vec<Message> = Vec::new();
    let mut s = Loop::default();
    let ep = &tc.ep;
    let ccfg = &tc.compact;
    let port = engine_port(&tc.cfg);
    let reasoning_on = reasoning_active(tc.cfg.get("REASONING").map_or("", String::as_str));
    let effort_wanted = tc.reasoning.effort_wanted.clone();
    let think_off = tc.reasoning.think_off;
    let mut effort = tc.reasoning.effort.clone();
    let mut kwargs = tc.reasoning.kwargs.clone();
    let mut budget =
        super::nudges::agent_budget(tc.cfg.get("AGENT_BUDGET").map_or("", String::as_str));
    if tc.code {
        budget *= super::nudges::CODE_BUDGET_FACTOR;
    }
    let fail =
        |cb: &mut (dyn FnMut(StreamEvent) -> bool + Send), extra: Vec<Message>, err: String| {
            cb(StreamEvent::Err(err.clone()));
            TurnResult {
                extra,
                err: Some(err),
            }
        };

    let mut iter: i64 = -1;
    loop {
        iter += 1;
        // Messages mis en file : entre deux étapes, jamais après un stop.
        if iter > 0 && !cancel.is_cancelled() {
            let q = host.drain_queued();
            messages.extend(q.iter().cloned());
            extra.extend(q);
        }
        let nudge = budget_nudge(s.tool_runs, budget, s.budget_nudges);
        if !nudge.is_empty() {
            s.budget_nudges += 1;
            log_budget(s.tool_runs, budget, s.budget_nudges);
            // NUDGE_IN_TOOL (opt-in, gabarits dont le rendu bouge quand un
            // message user s'ajoute) : au bout du dernier résultat d'outil.
            // La sonde ne connaît que la forme normale : une relance d'une
            // autre forme se replie sur le message à part.
            let placed = !s.disable_tools
                && !s.tool_choice_none
                && super::nudges::nudge_in_tool_key(&tc.cfg)
                && !ep.external
                && host.prefix_unstable(&tc.tools, kwargs.as_ref(), &effort)
                && super::nudges::nudge_into_tool(&mut messages, &mut extra, &nudge);
            if !placed {
                append_nudge(&mut messages, &mut extra, &nudge);
            }
        }
        let calls_on = !tc.tools.is_empty() && !s.disable_tools && !s.tool_choice_none;
        let pol = turn_echo_policy(ep.external || s.echo_off_turn, || tc.echo.clone());
        host.prepare_images(&messages).await;
        let slot = host.engine_slot(ep).await;
        let (body, echo_sent, prefix) = {
            let (sent, echo_sent) = wire_messages(&messages, s.tool_choice_none, &pol);
            let mut payload = build_chat_payload(
                ep,
                &sent,
                tc.temperature,
                &ChatPayloadOpts {
                    tools: &tc.tools,
                    disable_tools: s.disable_tools,
                    tool_choice_none: s.tool_choice_none,
                    effort: &effort,
                    kwargs: kwargs.as_ref(),
                    no_sampling: false,
                },
                &tc.cfg,
                &|r| host.image_data_url(r),
            );
            // SIDE_SLOT : le slot rendu par l'hôte ; aucun, la requête d'avant.
            if let Some(n) = slot
                && let Some(o) = payload.as_object_mut()
            {
                o.insert("id_slot".into(), Value::from(n));
            }
            (
                serde_json::to_vec(&payload).unwrap_or_default(),
                echo_sent,
                perf_prefix(&sent),
            )
        };
        let t_req = Instant::now();
        // KEEP_TURN_IMAGES : rien sans la clé.
        if let Some(k) = kt.as_mut() {
            k.sending(messages.len());
        }
        let reply = match cancel.or_stop(host.send(ep, body)).await {
            None => return TurnResult { extra, err: None },
            Some(Ok(r)) => r,
            Some(Err(e)) => {
                // Rien n'est encore à l'écran : rejouable tel quel.
                if s.net_retries < NET_RETRIES && retryable(e.kind) && !cancel.is_cancelled() {
                    log_llm_retry(s.net_retries + 1, &e.message);
                    if cancel.backoff(s.net_retries).await {
                        s.net_retries += 1;
                        continue;
                    }
                }
                if cancel.is_cancelled() {
                    return TurnResult { extra, err: None };
                }
                return fail(cb, extra, e.message);
            }
        };

        if reply.status != 200 {
            let status = reply.status;
            let mut msg = read_error_body(reply.body).await;
            if msg.is_empty() {
                msg = status_text(status);
            }
            // La relance sans raisonnement est refusée à son tour : il n'y
            // était pour rien.
            s.echo_tpl_retry = false;
            // Intensité refusée par le gabarit : refus définitif, à traiter
            // avant toute compaction.
            if !s.effort_retried
                && !effort_wanted.is_empty()
                && let Some(fixed) = effort_from_rejection(&msg, &effort_wanted)
            {
                s.effort_retried = true;
                host.effort_remember(&effort_wanted, &fixed);
                log_effort_fallback(&effort_wanted, &fixed);
                kwargs = reasoning_template_kwargs(think_off, &fixed);
                effort = fixed;
                continue;
            }
            // SIDE_SLOT : un cache KV à court de place en plein calcul n'est
            // pas un prompt trop long. Rejouée telle quelle, sans compaction ni
            // réduction — deux fois au plus, le temps que l'autre slot finisse,
            // puis l'erreur telle quelle. Sans second slot en service, rien.
            if !ep.external && slot.is_some() && side_slot_pool_error(&msg) {
                if s.pool_retries < 2 {
                    s.pool_retries += 1;
                    log_ctx(
                        "cache KV plein pendant le calcul (deux slots) : requête rejouée telle quelle, sans compaction",
                    );
                    if cancel.backoff(s.pool_retries - 1).await {
                        continue;
                    }
                }
                return fail(
                    cb,
                    extra,
                    format!("llama-server a renvoyé {status} : {msg}"),
                );
            }
            if echo_sent > 0 && echo_template_error(status, &msg) {
                s.echo_off_turn = true;
                s.echo_tpl_retry = true;
                log_ctx(&format!(
                    "erreur de gabarit avec le raisonnement renvoyé ({status}) : relance sans lui"
                ));
                continue;
            }
            if echo_sent > 0 && context_overflow(ccfg, &msg, &messages) {
                s.echo_off_turn = true;
                log_ctx(
                    "prompt trop long avec le raisonnement renvoyé : relance sans lui avant toute compaction",
                );
                continue;
            }
            if s.echo_off_turn {
                messages = strip_reasoning(&messages).into_owned();
            }
            // KEEP_TURN_IMAGES : les images gardées partent d'abord, et la
            // requête est rejouée sans elles ; le résumé (avec perte) ne vient
            // qu'ensuite, s'il le faut encore.
            if context_overflow(ccfg, &msg, &messages)
                && let Some((c, freed)) = drop_kept_images(&messages)
            {
                log_kept_images_dropped("réactif", freed);
                messages = c;
                extra.clear();
                publish_history(cb, kt, &messages);
                continue;
            }
            let overflow = context_overflow(ccfg, &msg, &messages);
            if ccfg.enabled && !s.compacted_retry && overflow {
                let Some(c) = cancel
                    .or_stop(host.compact(CompactPhase::Reactive, &messages))
                    .await
                else {
                    return TurnResult { extra, err: None };
                };
                if let Some(c) = c {
                    s.compacted_retry = true;
                    log_compact(ccfg, "réactif", 0, &messages, &c, true);
                    messages = c;
                    extra.clear();
                    publish_history(cb, kt, &messages);
                    continue;
                }
            }
            if ccfg.enabled && s.shrink_retries < 2 && overflow {
                let actual = overflow_tokens(&msg);
                let (c, changed) = shrink_to_fit(ccfg, &messages, actual);
                if changed {
                    s.shrink_retries += 1;
                    log_compact(ccfg, "réduction", actual, &messages, &c, true);
                    messages = c;
                    extra.clear();
                    publish_history(cb, kt, &messages);
                    continue;
                }
            }
            // 500 : appel d'outil que llama.cpp n'a pas su lire. Moteur local :
            // d'abord tool_choice « none », prompt intact ; puis outils retirés.
            if !s.disable_tools
                && !tc.tools.is_empty()
                && (status == 500 || (s.tool_choice_none && (400..500).contains(&status)))
            {
                if !ep.external && !s.tool_choice_none {
                    s.tool_choice_none = true;
                    log_ctx("relance sans outil après un 500 : tool_choice=none, prompt conservé");
                    continue;
                }
                s.tool_choice_none = false;
                s.disable_tools = true;
                messages = steer_system(&messages, TOOLS_OFF_HINT);
                continue;
            }
            // « Pas maintenant » (passerelle, surcharge) : rejouable.
            if s.net_retries < NET_RETRIES && retryable_status(status) && !cancel.is_cancelled() {
                log_llm_retry(s.net_retries + 1, &format!("le moteur a renvoyé {status}"));
                if cancel.backoff(s.net_retries).await {
                    s.net_retries += 1;
                    continue;
                }
            }
            let who = if ep.external {
                "l'API externe"
            } else {
                "llama-server"
            };
            return fail(cb, extra, format!("{who} a renvoyé {status} : {msg}"));
        }

        // Une réponse est arrivée : le budget de reprise réseau repart à neuf.
        s.net_retries = 0;
        if s.echo_tpl_retry {
            s.echo_tpl_retry = false;
            host.echo_refuse(&reasoning_echo_model(&tc.cfg));
        }

        let mut parser = StreamParser::new(StreamConfig {
            reasoning_on,
            calls_on,
            tool_choice_none: s.tool_choice_none,
            external: ep.external,
            echo_on: pol.on,
            echo_keeps_past: pol.keeps_past,
            pattern_retries: s.pattern_retries,
            patterns: tc.patterns.clone(),
            t_req,
            cache_hint: if ep.external { None } else { host.cache_hint() },
            preflight: if tc.code { host.preflight() } else { None },
        });
        let mut emit = |ev: StreamEvent| cb(ev);
        let mut lines = Lines::new(reply.body);
        // (message, ligne trop longue)
        let mut read_err: Option<(String, bool)> = None;
        let mut stopped = false;
        loop {
            match cancel.or_stop(lines.next_line()).await {
                None => {
                    stopped = true;
                    break;
                }
                Some(None) => break,
                Some(Some(Ok(line))) => {
                    if !parser.feed_line(&line, Instant::now(), &mut emit) {
                        break;
                    }
                }
                Some(Some(Err(LineError::TooLong))) => {
                    read_err = Some(("bufio.Scanner: token too long".into(), true));
                    break;
                }
                Some(Some(Err(LineError::Io(e)))) => {
                    read_err = Some((e, false));
                    break;
                }
            }
        }
        // Arrêt pendant la lecture : le tour s'arrête là, sans erreur, et rien
        // de plus ne part vers l'écran. La complétion coupée reste dans la
        // télémétrie, incomplète.
        if stopped {
            host.perf_record(PerfRec {
                iter: s.perf_iter,
                external: ep.external,
                ..Default::default()
            })
            .await;
            return TurnResult { extra, err: None };
        }
        let out = parser.finish(&mut emit);
        let complete = out.is_complete(read_err.is_some());
        // Télémétrie (perf_log.go) : une complétion coupée avant son chunk
        // final est rangée comme incomplète, sans calcul de perte.
        let marks = host
            .perf_record(PerfRec {
                iter: s.perf_iter,
                complete,
                external: ep.external,
                stats: out.stats.clone(),
                prefix: if complete { prefix } else { Vec::new() },
            })
            .await;
        s.perf_iter += 1;
        if complete {
            // Dernier événement de la complétion : copie complète des stats,
            // avec ce que la télémétrie en dit (nature, perte, alerte).
            let mut st = out.stats.clone();
            if !marks.kind.is_empty() {
                st.kind = marks.kind;
            }
            st.lost = marks.lost;
            st.lost_after = marks.lost_after;
            st.lost_alert = marks.lost_alert;
            cb(StreamEvent::Stats(st));
            // Sonde de gabarit, en tâche de fond et après coup : rien n'en
            // revient dans la requête suivante.
            if !ep.external && !s.disable_tools && !s.tool_choice_none {
                host.completed(&tc.tools, kwargs.as_ref(), &effort);
            }
            if !ep.external {
                s.peak_gen = s.peak_gen.max(out.stats.gen_tokens);
                if s.last_est > 0 && out.stats.prompt_tokens_total > 0 {
                    log_ctx_estimate("en-tour", s.last_est, out.stats.prompt_tokens_total);
                }
            }
            s.last_est = 0;
        }
        // KEEP_TURN_IMAGES : coût des images de l'étape précédente, mesuré sur
        // cette complétion ; gardées ou rendues éphémères.
        if let Some(k) = kt.as_mut() {
            k.observe(complete, &out.stats, &mut messages, &mut extra);
        }
        if out.finish_reason == "length" {
            log_ctx(&format!(
                "finish=length prompt={} généré={} raisonnement={} fenêtre={}",
                out.stats.prompt_tokens_total,
                out.stats.gen_tokens,
                out.stats.reasoning_tokens,
                ccfg.ctx_window
            ));
        }
        if out.aborted {
            return TurnResult { extra, err: None };
        }
        if read_err.is_some() && out.read_error_benign() {
            read_err = None;
        }
        // Flux coupé vers une API distante : rejoué, ou poursuivi après le texte
        // déjà affiché. Jamais pour le moteur local : coupé, il a planté.
        if let Some((err, too_long)) = &read_err
            && !too_long
            && ep.external
            && s.stream_retries < MAX_STREAM_RETRIES
        {
            s.stream_retries += 1;
            if out.sent_reasoning && !out.sent_answer {
                cb(StreamEvent::DropReasoning);
            }
            if let Some(t) = &out.typing_tool {
                cb(StreamEvent::ToolUsed(ToolUsedEvent {
                    name: t.name.clone(),
                    label: t.label.clone(),
                    done: true,
                    result: "(appel interrompu par une coupure réseau, relancé)".into(),
                    ..Default::default()
                }));
            }
            log_stream_retry(s.stream_retries, err);
            if cancel.backoff(s.stream_retries).await {
                if !out.shown.trim().is_empty() {
                    messages.push(text_msg("assistant", out.shown.clone()));
                    messages.push(text_msg("user", STREAM_CUT_CONTINUE));
                }
                continue;
            }
        }
        if let Some((err, too_long)) = read_err {
            if cancel.is_cancelled() {
                return TurnResult { extra, err: None };
            }
            return fail(cb, extra, stream_cut_error(&err, too_long, port));
        }
        s.stream_retries = 0;

        let content = out.content.as_str();
        // Relance tool_choice « none » sans réponse exploitable : repli UNE fois
        // sur le chemin historique, outils retirés.
        if s.tool_choice_none
            && !cancel.is_cancelled()
            && (out.none_leak
                || tc.patterns.textual_tool_call(content)
                || content.trim().is_empty())
        {
            log_ctx("relance tool_choice=none sans réponse exploitable : repli sans outils");
            s.tool_choice_none = false;
            s.disable_tools = true;
            if out.sent_reasoning && !out.sent_answer {
                cb(StreamEvent::DropReasoning);
            }
            messages = steer_system(&messages, TOOLS_OFF_HINT);
            continue;
        }

        let echo = ReasoningEcho {
            text: out.echo_text.clone(),
            model: pol.model.clone(),
        };
        let assistant_base = |calls: Vec<ToolCall>| {
            let mut a = Message {
                role: "assistant".into(),
                tool_calls: calls,
                ..Default::default()
            };
            if !content.is_empty() {
                a.content = Some(Value::String(content.to_string()));
            }
            if !echo.text.is_empty() {
                a.reasoning_content = echo.text.clone();
                a.reasoning_model = echo.model.clone();
            }
            a
        };

        // Écriture refusée en plein flux (pré-vol) : l'appel, réduit à son
        // chemin, entre dans l'historique avec le refus pour résultat.
        if let Some(pf) = &out.preflight {
            let id = if pf.id.is_empty() {
                format!("call_{iter}_0")
            } else {
                pf.id.clone()
            };
            let file_arg = serde_json::json!({ "file": pf.file }).to_string();
            let assistant = assistant_base(vec![ToolCall {
                id: id.clone(),
                kind: "function".into(),
                function: ToolCallFunc {
                    name: pf.name.clone(),
                    arguments: file_arg,
                },
                ..Default::default()
            }]);
            let result = format!(
                "{} (écriture interrompue avant la fin : rien n'a été modifié)",
                pf.msg
            );
            cb(StreamEvent::ToolUsed(ToolUsedEvent {
                name: pf.name.clone(),
                label: pf.file.clone(),
                done: true,
                result: result.clone(),
                ..Default::default()
            }));
            let tool_msg = Message {
                role: "tool".into(),
                tool_call_id: id,
                content: Some(Value::String(result)),
                ..Default::default()
            };
            messages.extend([assistant.clone(), tool_msg.clone()]);
            extra.extend([assistant, tool_msg]);
            s.tool_runs += 1;
            continue;
        }

        // Appels d'outils (même si finish_reason dit « stop »).
        if !out.tool_calls.is_empty() {
            let assembled = out.assemble_tool_calls(iter, &tc.tools);
            let assistant = assistant_base(assembled.calls.clone());
            messages.push(assistant.clone());
            extra.push(assistant);
            // Début des résultats de CETTE étape, que le moteur n'a pas comptés.
            let step_start = messages.len();
            s.pattern_retries = 0;
            for call in &assembled.calls {
                if cancel.is_cancelled() {
                    return TurnResult { extra, err: None };
                }
                s.tool_runs += 1;
                let name = call.function.name.as_str();
                let args = call_args(&call.function.arguments);
                let label = tool_call_label(name, &args);
                cb(StreamEvent::ToolUsed(ToolUsedEvent {
                    name: name.into(),
                    label: label.clone(),
                    ..Default::default()
                }));
                let done_ev = || ToolUsedEvent {
                    name: name.into(),
                    label: label.clone(),
                    done: true,
                    ..Default::default()
                };
                let push_result =
                    |messages: &mut Vec<Message>, extra: &mut Vec<Message>, content: String| {
                        let m = Message {
                            role: "tool".into(),
                            tool_call_id: call.id.clone(),
                            content: Some(Value::String(content)),
                            ..Default::default()
                        };
                        messages.push(m.clone());
                        extra.push(m);
                    };
                if assembled.bad_args.contains(&call.id) {
                    let result = "[erreur] arguments de l'appel illisibles (JSON invalide ou tronqué) : l'outil n'a PAS été exécuté. Renvoie l'appel avec des arguments JSON complets et valides.";
                    cb(StreamEvent::ToolUsed(fill_tool_result(
                        host,
                        done_ev(),
                        result,
                    )));
                    push_result(&mut messages, &mut extra, result.into());
                    continue;
                }
                let call_key = format!("{name}\0{}", call.function.arguments);
                if let Some(prev) = s.done_calls.get(&call_key)
                    && dedupable_tool(name)
                {
                    let n = s.repeat_count.entry(call_key.clone()).or_insert(0);
                    *n += 1;
                    let result = repeated_call_result(prev, *n);
                    cb(StreamEvent::ToolUsed(fill_tool_result(
                        host,
                        done_ev(),
                        &result,
                    )));
                    push_result(&mut messages, &mut extra, result);
                    continue;
                }
                let Some(ex) = cancel.or_stop(host.exec_tool(name, &args, &label)).await else {
                    return TurnResult { extra, err: None };
                };
                if let Some(ask) = ex.ask.clone() {
                    cb(StreamEvent::Ask(ask));
                }
                if !ex.result.starts_with("[erreur]") {
                    s.done_calls.insert(call_key, ex.result.clone());
                }
                let ev = ToolUsedEvent {
                    diff: ex.diff.clone(),
                    added: ex.added,
                    removed: ex.removed,
                    image: ex.image.clone(),
                    shot: ex.live_shot.clone(),
                    ..done_ev()
                };
                cb(StreamEvent::ToolUsed(fill_tool_result(
                    host, ev, &ex.result,
                )));
                // Plafond pour le MODÈLE seulement : l'UI a reçu le résultat.
                push_result(&mut messages, &mut extra, cap_tool_result(&ex.result));
                if let Some(img) = ex.relay {
                    match kt.as_mut() {
                        // Éphémère : jamais dans l'historique.
                        None => messages.push(img),
                        // KEEP_TURN_IMAGES : gardée par référence, à l'essai
                        // jusqu'à la mesure de son coût.
                        Some(k) => {
                            let stored = if k.wants(&messages) {
                                host.keep_image(&img).await
                            } else {
                                None
                            };
                            k.relay(&mut messages, &mut extra, img, step_start, stored);
                        }
                    }
                }
            }
            // Compaction EN COURS DE TOUR, sur le contexte réel du dernier appel
            // plus les résultats de cette étape (le moteur ne les a pas vus).
            let mut used = 0;
            let base = out.stats.ctx_after();
            if base > 0 {
                used = base + estimate_tokens(&messages[step_start..], &ccfg.echo);
                s.last_est = used;
            }
            // KEEP_TURN_IMAGES : les images gardées partent AVANT tout résumé ;
            // la compaction ne suit que si elle reste nécessaire.
            if has_kept_images(&messages)
                && compact_needed(ccfg, &messages, used, s.peak_gen)
                && let Some((c, freed)) = drop_kept_images(&messages)
            {
                log_kept_images_dropped("en-tour", freed);
                messages = c;
                if used > 0 {
                    used = (used - freed).max(1);
                    s.last_est = used;
                }
                extra.clear();
                publish_history(cb, kt, &messages);
            }
            if compact_needed(ccfg, &messages, used, s.peak_gen)
                && !host.compact_refused(used, &messages)
            {
                cb(StreamEvent::Compacting(true));
                let phase = CompactPhase::Mid {
                    used,
                    rescue: s.disable_tools || s.tool_choice_none,
                };
                let c = cancel.or_stop(host.compact(phase, &messages)).await;
                cb(StreamEvent::Compacting(false));
                let Some(c) = c else {
                    return TurnResult { extra, err: None };
                };
                log_compact(
                    ccfg,
                    "en-tour",
                    used,
                    &messages,
                    c.as_deref().unwrap_or(&messages),
                    c.is_some(),
                );
                if let Some(c) = c {
                    s.last_est = 0;
                    messages = c;
                    extra.clear();
                    publish_history(cb, kt, &messages);
                }
            }
            continue;
        }

        // Appel d'outil écrit en texte : relance corrective (le texte fautif
        // reste affiché, l'historique garde la trace ET la correction).
        let snippet = tc.patterns.snippet(content);
        if !snippet.is_empty() && s.pattern_retries < MAX_PATTERN_RETRIES && calls_on {
            s.pattern_retries += 1;
            let bad = with_echo(text_msg("assistant", content), Some(&echo));
            let fix = text_msg("user", retry_corrective(&snippet));
            messages.extend([bad.clone(), fix.clone()]);
            extra.extend([bad, fix]);
            continue;
        }

        if content.trim().is_empty() {
            // Coupée par une fenêtre pleine : compacter et rejouer, une fois.
            let known = out.stats.prompt_tokens_total + out.stats.gen_tokens;
            if out.finish_reason == "length"
                && !ep.external
                && s.length_replays < 1
                && compact_would_trigger(ccfg, &messages, known)
            {
                s.length_replays += 1;
                // KEEP_TURN_IMAGES : les images gardées d'abord ; le résumé ne
                // suit que s'il reste nécessaire.
                if let Some((c, freed)) = drop_kept_images(&messages) {
                    log_kept_images_dropped("fenêtre-pleine", freed);
                    messages = c;
                    extra.clear();
                    publish_history(cb, kt, &messages);
                    if !compact_would_trigger(ccfg, &messages, (known - freed).max(1)) {
                        cb(StreamEvent::DropReasoning);
                        continue;
                    }
                }
                cb(StreamEvent::Compacting(true));
                let c = cancel
                    .or_stop(host.compact(CompactPhase::FullWindow, &messages))
                    .await;
                cb(StreamEvent::Compacting(false));
                let Some(c) = c else {
                    return TurnResult { extra, err: None };
                };
                log_compact(
                    ccfg,
                    "fenêtre-pleine",
                    known,
                    &messages,
                    c.as_deref().unwrap_or(&messages),
                    c.is_some(),
                );
                if let Some(c) = c {
                    cb(StreamEvent::DropReasoning);
                    messages = c;
                    extra.clear();
                    publish_history(cb, kt, &messages);
                    continue;
                }
            }
            if calls_on && s.nudge_count < MAX_NUDGES {
                s.nudge_count += 1;
                log_ctx(&format!(
                    "relance « pensé sans agir » n°{} (finish={})",
                    s.nudge_count, out.finish_reason
                ));
                cb(StreamEvent::DropReasoning);
                let nudge = if s.nudge_count > 1 {
                    THINK_NUDGE_STUCK
                } else {
                    THINK_NUDGE_FIRST
                };
                append_nudge(&mut messages, &mut extra, nudge);
                continue;
            }
            cb(StreamEvent::Content(format!(
                "_(le modèle n'a pas produit de réponse — finish: {})_",
                out.finish_reason
            )));
        }
        // Raisonnement de la réponse finale, rangé avec elle (jamais sans texte).
        if !echo.text.is_empty() && !content.trim().is_empty() {
            cb(StreamEvent::Echo(echo));
        }
        return TurnResult { extra, err: None };
    }
}

#[cfg(test)]
mod tests;
