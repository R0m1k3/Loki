//! Estimation du contexte et compactage de l'historique — port fidèle de
//! `chat_compact.go` et `chat_compact_cont.go`.
//!
//! Compactage façon Hermes Agent : au lieu de vider la conversation quand la
//! fenêtre se remplit, on la scinde en trois zones — tête (messages système,
//! protégée), queue (tours récents dans un budget de jetons, protégée) et
//! torse (tout le milieu, SEULE zone compactée). Le torse est dégraissé sans
//! IA (repli), résumé par le modèle en UN appel, et remplacé par le résumé.
//!
//! Ce qui, en Go, venait d'un état global devient ici un paramètre explicite :
//! - la configuration (`ReadConfig()` : COMPACT, CTX, COMPACT_CONTINUATION,
//!   EXTERNAL, MODEL) et la politique de raisonnement renvoyé
//!   (`currentEchoPolicy()`) → [`CompactConfig`] ;
//! - l'appel au moteur pour le résumé (`postSummary` par la porte du moteur),
//!   la relecture du tampon du slot (`engineSlotHolds`) et la détection d'un
//!   appel d'outil écrit en texte (`textualToolCallSnippet`) → trait
//!   [`Summarizer`] ;
//! - l'archive recall (`archiveRecallBlock`) → trait [`RecallArchive`] ;
//! - la mémoire des refus et des échecs de la continuation (globales Go
//!   `compactRefused`, `compactContFails`) → [`CompactMemory`] ;
//! - l'état des critères du mode Code (`critRender(critList(conv))`) → champ
//!   [`Compactor::criteria`] ;
//! - la vue d'envoi du tour (`turnViewDry`, `turnReasoning`, `wireMessages`)
//!   → fermetures passées à [`compact_opts_for`] / [`compact_opts_mid`].
//!
//! Les traces de diagnostic partent sur la sortie d'erreur, comme en Go.

use std::collections::HashMap;
use std::fmt;
use std::future::Future;
use std::sync::{Mutex, MutexGuard};

use serde::Deserialize;
use serde_json::{Map, Value};

use super::types::{Message, Tool};

// ---- constantes ----------------------------------------------------------

/// Seuil de déclenchement proactif : on compacte quand l'historique estimé
/// dépasse cette fraction de la fenêtre de contexte.
pub const COMPACT_TRIGGER_FRAC: f64 = 0.75;
/// Budget de la queue : fraction de la CONVERSATION gardée intacte.
pub const COMPACT_TAIL_FRAC: f64 = 0.20;
/// Un résultat d'outil du torse plus long que ça (octets) devient un marqueur
/// dans le torse DÉGRAISSÉ (repli si le résumé échoue).
pub const COMPACT_TOOL_PRUNE_LEN: usize = 200;
/// Longueur (caractères) à laquelle on raccourcit un résultat d'outil avant de
/// le donner au résumeur.
pub const COMPACT_TOOL_SUMMARY_LEN: usize = 1200;

/// Remplace un vieux résultat d'outil dans le torse dégraissé.
pub const COMPACT_PRUNED_MARKER: &str = "[Old tool result removed to save context. The important content is in the summary above — do NOT call this tool again to fetch it back.]";

/// Ouvre le message `user` synthétique qui porte le résumé.
pub const COMPACT_SUMMARY_PREFIX: &str = "[CONTEXT COMPACTED]";

/// Remplace la partie `image_url` d'un message multimodal dans [`msg_text`].
pub const IMAGE_LOST_MARKER: &str = " [image — not kept in the conversation past this point; look at it again (see_image on the file, or a new screenshot) if you still need to see it]";

/// La réponse assistant synthétique qui suit le résumé.
pub const COMPACT_ACK_MSG: &str = "Understood. I'll resume from exactly where I left off, using the findings above, and call recall(id) if I need the full content of an archived block, without redoing work that is already done.";

/// Ce qu'un résumé de mode Code doit garder en plus.
pub const CODE_SUMMARY_RULES: &str = "\nThis is a CODING session. Also keep:\n- Every file created or modified (path) and what changed in it\n- Errors hit (build, tests, tools) and how each was resolved — or that it is still open\n- The commands that build and test the project";

/// Le rôle du résumeur sur transcription.
pub const COMPACT_TRANSCRIPT_INTRO: &str = "You are a context compactor. You are given the transcript of the older turns of a conversation between a user and an AI assistant (with its tools). The PURPOSE of your summary is to let the conversation continue in a fresh, smaller context WITHOUT losing any information that is useful or important to understand what came before and keep working — preserve everything that matters, drop only what is redundant.\n\nThe assistant is MID-TASK: it will read your summary and must resume exactly where it left off, WITHOUT redoing work it has already done. Its own internal reasoning is NOT part of the transcript and is lost — your summary is the only memory it keeps.\n\n";

/// Ce que tout résumé de compaction doit garder (transcription et continuation).
pub const COMPACT_SUMMARY_RULES: &str = "Summarize densely and faithfully, keeping ONLY the essentials:\n- The user's CURRENT request, goal(s) and constraints\n- FINDINGS: the concrete information already gathered — facts, figures, dates, names, URLs, file paths, values, config. This is the most important part: whatever is not here is lost and will have to be looked up again.\n- Sources already consulted (URLs opened, files read, commands run) — so they are not consulted a second time\n- Decisions made and established facts\n- STATE OF PROGRESS: what is already answered, what is still missing, and the next concrete step\nStrict rules: no preamble or conclusion, no verbatim or long quotes, no throwaway detail. Use short bullet points. Be as concise as you can WHILE keeping every fact, decision and still-open task: a detail you drop here is lost for good, so when in doubt keep it. This is a dense compression summary, not a report. Always write ACTUAL prose sentences/bullets — never answer with just an id or a reference.\nWrite the summary in the SAME language as the conversation.";

/// Température de toute requête de résumé.
pub const SUMMARY_TEMPERATURE: f64 = 0.2;

/// Marque de troncature de [`shrink_to_fit`].
pub const SHRINK_TRUNC_MARKER: &str = "\n[…tronqué : contexte plein. Relance l'outil de façon plus ciblée si tu as besoin de la suite]";

/// Borne de TOUT résultat d'outil dans la vue du modèle (caractères).
pub const TOOL_RESULT_MAX: usize = 30000;

/// Ouvre la demande de résumé en continuation : jamais rangée.
pub const COMPACT_CONT_PREFIX: &str = "[COMPACTION]";

/// Conversations suivies au plus par la mémoire des refus.
pub const COMPACT_REFUSED_MAX: usize = 64;
/// Échecs de la continuation au-delà desquels elle est suspendue pour le modèle.
pub const COMPACT_CONT_MAX_FAILS: i64 = 2;

/// Un bloc du torse plus long que ça (caractères) est archivé (`chat_recall.go`).
pub const RECALL_ARCHIVE_MIN_LEN: usize = 800;
/// Extrait de tête laissé au résumeur pour un bloc archivé (`chat_recall.go`).
pub const RECALL_SUMMARY_HEAD_LEN: usize = 400;

const THINK_OPEN: &str = "<think>";
const THINK_CLOSE: &str = "</think>";

// Rappels de loki (llm_injected.go) : reconnus pour ne pas les prendre pour
// une demande de l'utilisateur.
const LOKI_NOTE_PREFIX: &str = "[system] ";
const THINK_NUDGE_FIRST: &str = "You reasoned but did not call a tool or answer. Act NOW: call the appropriate tool directly (e.g. mem_search/mem_read/bash), or give your final answer if you already have the info. Don't explain, act.";
const THINK_NUDGE_STUCK: &str = "You are stuck re-describing the same plan without executing it. Stop reasoning. In your NEXT message, either call ONE tool right now, or write your final answer in plain text using only what you already know — no more planning, no more thinking, act or answer this instant.";
const RETRY_CORRECTIVE_LEAD: &str =
    "Your last answer contained a TOOL CALL WRITTEN AS TEXT — it was never executed:\n";
const TOOLS_OFF_HINT: &str = "Do not call any more tools. Answer now, directly, in the user's language, using only the information already gathered.";
const BUDGET_NUDGE_ENDS: [&str; 3] = [
    "unless exactly one specific call is genuinely still missing.",
    "say so in the answer instead of investigating further.",
    "Write your final answer in this message. Do not call another tool.",
];

// Blocs <context_update> (chat_projsnap.go).
const CTX_UPD_OPEN: &str = "<context_update from=\"loki\"";
const CTX_UPD_CLOSE: &str = "</context_update>";

// ---- configuration -------------------------------------------------------

/// Politique de raisonnement renvoyé : celle de `wire.rs` (même port de
/// `echoPolicy`).
pub use super::wire::EchoPolicy;

/// Instantané de l'état global Go que lit la compaction.
#[derive(Debug, Clone, PartialEq)]
pub struct CompactConfig {
    /// `compactEnabled()` (COMPACT).
    pub enabled: bool,
    /// `ctxWindow()` (CTX) — aussi la fenêtre de `engineMainNow()`.
    pub ctx_window: i64,
    /// `compactContinuationOn(cfg)` (COMPACT_CONTINUATION, moteur local).
    pub continuation: bool,
    /// Preset externe (`isExternalConfig` / `resolveChatEndpoint().External`).
    pub external: bool,
    /// Modèle de `engineMainNow()` : base du chemin MODEL.
    pub engine_model: String,
    /// `currentEchoPolicy()`.
    pub echo: EchoPolicy,
}

impl Default for CompactConfig {
    fn default() -> Self {
        CompactConfig {
            enabled: true,
            ctx_window: 32768,
            continuation: false,
            external: false,
            engine_model: ".".into(),
            echo: EchoPolicy::default(),
        }
    }
}

impl CompactConfig {
    /// Construit l'instantané depuis la carte de configuration (`ReadConfig()`)
    /// et la politique d'écho courante (calculée ailleurs : sonde de gabarit,
    /// mémoire des refus).
    pub fn from_config(cfg: &HashMap<String, String>, echo: EchoPolicy) -> Self {
        CompactConfig {
            enabled: compact_enabled(cfg),
            ctx_window: ctx_window(cfg),
            continuation: compact_continuation_on(cfg),
            external: is_external_config(cfg),
            engine_model: path_base(cfg.get("MODEL").map_or("", |s| s.trim())),
            echo,
        }
    }
}

fn cfg_get<'a>(cfg: &'a HashMap<String, String>, k: &str) -> &'a str {
    cfg.get(k).map_or("", String::as_str)
}

/// `compactEnabled` : actif par défaut ; seul off/false/0/no/non le coupe.
pub fn compact_enabled(cfg: &HashMap<String, String>) -> bool {
    !matches!(
        cfg_get(cfg, "COMPACT").trim().to_lowercase().as_str(),
        "off" | "false" | "0" | "no" | "non"
    )
}

/// `ctxWindow` : CTX, 32768 par défaut.
pub fn ctx_window(cfg: &HashMap<String, String>) -> i64 {
    match cfg_get(cfg, "CTX").parse::<i64>() {
        Ok(n) if n > 0 => n,
        _ => 32768,
    }
}

/// `isExternalConfig` : EXTERNAL = "1".
pub fn is_external_config(cfg: &HashMap<String, String>) -> bool {
    cfg_get(cfg, "EXTERNAL").trim() == "1"
}

/// `compactContinuationOn` : clé posée, moteur local.
pub fn compact_continuation_on(cfg: &HashMap<String, String>) -> bool {
    if is_external_config(cfg) {
        return false;
    }
    matches!(
        cfg_get(cfg, "COMPACT_CONTINUATION")
            .trim()
            .to_lowercase()
            .as_str(),
        "on" | "1" | "true" | "yes" | "oui"
    )
}

/// `filepath.Base` : dernier élément du chemin, "." pour un chemin vide.
fn path_base(p: &str) -> String {
    let sep = |c: char| c == '/' || (cfg!(windows) && c == '\\');
    if p.is_empty() {
        return ".".into();
    }
    let t = p.trim_end_matches(sep);
    if t.is_empty() {
        return if cfg!(windows) && p.starts_with('\\') {
            "\\".into()
        } else {
            "/".into()
        };
    }
    t.rsplit(sep).next().unwrap_or(t).to_string()
}

/// `compactSummaryBudget` : budget du résumé, ~4 % de la fenêtre, borné à
/// 700..1600.
pub fn compact_summary_budget(ctx_window: i64) -> i64 {
    (ctx_window * 4 / 100).clamp(700, 1600)
}

/// `summaryRuneCap` : filet de sécurité en caractères du résumé.
pub fn summary_rune_cap(ctx_window: i64) -> i64 {
    compact_summary_budget(ctx_window) * 6
}

// ---- texte et estimation ---------------------------------------------------

fn text_value(s: impl Into<String>) -> Option<Value> {
    Some(Value::String(s.into()))
}

fn rune_count(s: &str) -> usize {
    s.chars().count()
}

fn rune_prefix(s: &str, n: usize) -> &str {
    match s.char_indices().nth(n) {
        Some((i, _)) => &s[..i],
        None => s,
    }
}

/// `msgText` : texte d'un message (chaîne, ou parties `text` recollées avec
/// [`IMAGE_LOST_MARKER`] à la place de chaque partie `image_url`).
pub fn msg_text(m: &Message) -> String {
    match &m.content {
        Some(Value::String(s)) => s.clone(),
        Some(Value::Array(parts)) => {
            let mut b = String::new();
            for p in parts {
                let Some(part) = p.as_object() else { continue };
                match part.get("type").and_then(Value::as_str) {
                    Some("text") => {
                        if let Some(Value::String(s)) = part.get("text") {
                            b.push_str(s);
                        }
                    }
                    Some("image_url") => b.push_str(IMAGE_LOST_MARKER),
                    _ => {}
                }
            }
            b
        }
        _ => String::new(),
    }
}

/// `msgTokens` : ~4 octets par jeton plus un forfait de 4 par message, plus
/// le coût mesuré d'une image gardée. Sans le raisonnement renvoyé.
pub fn msg_tokens(m: &Message) -> i64 {
    let mut n = 4 + msg_text(m).len() as i64 / 4;
    for tc in &m.tool_calls {
        n += (tc.function.name.len() + tc.function.arguments.len()) as i64 / 4;
    }
    n + m.img_tokens
}

/// `echoTokens` : jetons que le raisonnement renvoyé ajoute, message par
/// message (`None` = aucun).
fn echo_tokens(msgs: &[Message], p: &EchoPolicy) -> Option<Vec<i64>> {
    if !msgs.iter().any(|m| !m.reasoning_content.is_empty()) {
        return None;
    }
    echo_tokens_for(msgs, p)
}

/// `echoTokensFor` de `wire.rs`, en i64 (unité des comptes de ce module).
fn echo_tokens_for(msgs: &[Message], p: &EchoPolicy) -> Option<Vec<i64>> {
    super::wire::echo_tokens_for(msgs, p).map(|v| v.into_iter().map(|n| n as i64).collect())
}

/// `estimateTokens` : taille estimée de l'historique, raisonnement renvoyé
/// compris quand il part réellement.
pub fn estimate_tokens(msgs: &[Message], echo: &EchoPolicy) -> i64 {
    let mut total: i64 = msgs.iter().map(msg_tokens).sum();
    if let Some(e) = echo_tokens(msgs, echo) {
        total += e.iter().sum::<i64>();
    }
    total
}

/// `compactWouldTrigger` : compactage actif ET contexte au-dessus du seuil.
/// `known_tokens` ≤ 0 : repli sur l'estimation.
pub fn compact_would_trigger(cfg: &CompactConfig, msgs: &[Message], known_tokens: i64) -> bool {
    if !cfg.enabled {
        return false;
    }
    let mut used = known_tokens;
    if used <= 0 {
        used = estimate_tokens(msgs, &cfg.echo);
    }
    used >= (cfg.ctx_window as f64 * COMPACT_TRIGGER_FRAC) as i64
}

/// `compactNeeded` : seuil, ou marge de génération insuffisante.
pub fn compact_needed(cfg: &CompactConfig, msgs: &[Message], used: i64, peak_gen: i64) -> bool {
    if compact_would_trigger(cfg, msgs, used) {
        return true;
    }
    cfg.enabled && gen_headroom_short(used, peak_gen, cfg.ctx_window)
}

/// `genHeadroomShort` : la fenêtre ne laisse plus la place d'une génération de
/// 1,2 × la plus longue récente (plancher 8 k ramené à 1/8 de la fenêtre),
/// plus une marge — jamais avant la moitié de la fenêtre.
pub fn gen_headroom_short(used: i64, peak_gen: i64, window: i64) -> bool {
    if used <= 0 || peak_gen <= 0 || window <= 0 {
        return false;
    }
    let need = (peak_gen * 6 / 5).max(8192.min(window / 8)) + 1024.min(window / 32);
    used + need.min(window / 2) > window
}

/// `logCtx` : trace un fait du comptage de contexte.
pub fn log_ctx(msg: &str) {
    eprintln!("[ctx] {msg}");
}

/// `logCtxEstimate` : ne parle qu'au-delà de 2 % d'écart.
pub fn log_ctx_estimate(phase: &str, est: i64, actual: i64) {
    if actual <= 0 {
        return;
    }
    let d = est - actual;
    if d * 50 > actual || -d * 50 > actual {
        log_ctx(&format!("{phase} estimé={est} réel={actual} écart={d:+}"));
    }
}

/// `ctxPending` : dernier contexte mesuré (valable pour les `measured`
/// premiers messages) plus l'estimation des messages arrivés depuis.
pub fn ctx_pending(used: i64, measured: usize, msgs: &[Message], echo: &EchoPolicy) -> i64 {
    if used <= 0 || measured == 0 || measured > msgs.len() {
        return used;
    }
    used + estimate_tokens(&msgs[measured..], echo)
}

/// `logCompact` : une ligne par décision de compaction.
pub fn log_compact(
    cfg: &CompactConfig,
    phase: &str,
    used: i64,
    before: &[Message],
    after: &[Message],
    changed: bool,
) {
    eprintln!(
        "[compact] {} ctx={} seuil={}/{} est_avant={} est_apres={} msgs={}→{} changé={}",
        phase,
        used,
        (cfg.ctx_window as f64 * COMPACT_TRIGGER_FRAC) as i64,
        cfg.ctx_window,
        estimate_tokens(before, &cfg.echo),
        estimate_tokens(after, &cfg.echo),
        before.len(),
        after.len(),
        changed
    );
}

// ---- rappels de loki, images gardées, blocs projet ------------------------

/// `isLokiInjected` : un message `user` venu de loki, pas de l'utilisateur.
fn is_loki_injected(m: &Message) -> bool {
    if m.role != "user" {
        return false;
    }
    if m.img_relay {
        return true;
    }
    let Some(Value::String(s)) = &m.content else {
        return false;
    };
    if s == THINK_NUDGE_FIRST
        || s == THINK_NUDGE_STUCK
        || s.strip_prefix(LOKI_NOTE_PREFIX) == Some(TOOLS_OFF_HINT)
        || s.starts_with(RETRY_CORRECTIVE_LEAD)
    {
        return true;
    }
    if !s.starts_with(LOKI_NOTE_PREFIX) {
        return false;
    }
    BUDGET_NUDGE_ENDS.iter().any(|e| s.ends_with(e))
}

/// `dropKeptImages` : copie où chaque image gardée devient sa légende suivie
/// d'[`IMAGE_LOST_MARKER`], et coût libéré. `None` : aucune image gardée.
fn drop_kept_images(msgs: &[Message]) -> Option<(Vec<Message>, i64)> {
    if !msgs.iter().any(|m| m.img_tokens > 0) {
        return None;
    }
    let mut out = msgs.to_vec();
    let mut freed = 0;
    for m in out.iter_mut().filter(|m| m.img_tokens > 0) {
        let mut texts: Vec<&str> = Vec::new();
        if let Some(Value::Array(parts)) = &m.content {
            for p in parts.iter().filter_map(Value::as_object) {
                if let (Some(Value::String(t)), Some("text")) =
                    (p.get("text"), p.get("type").and_then(Value::as_str))
                {
                    texts.push(t);
                }
            }
        }
        freed += m.img_tokens;
        let text = texts.join("\n") + IMAGE_LOST_MARKER;
        m.content = text_value(text);
        m.img_tokens = 0;
    }
    Some((out, freed))
}

fn log_kept_images_dropped(phase: &str, freed: i64) {
    eprintln!("[images] {phase} : images gardées retirées ({freed} jetons)");
}

/// `stripCtxUpdText` : retire le(s) bloc(s) posé(s) en TÊTE du texte.
fn strip_ctx_upd_text(s: &str) -> (String, bool) {
    let mut s = s;
    let mut stripped = false;
    while s.starts_with(CTX_UPD_OPEN) {
        let Some(i) = s.find(CTX_UPD_CLOSE) else {
            break;
        };
        let rest = &s[i + CTX_UPD_CLOSE.len()..];
        s = rest.strip_prefix("\n\n").unwrap_or(rest);
        stripped = true;
    }
    (s.to_string(), stripped)
}

/// `withoutCtxUpdate` : le message sans son bloc <context_update> de tête
/// (seulement un message `user` marqué `ctx_upd`).
fn without_ctx_update(m: &Message) -> Option<Message> {
    if m.role != "user" || !m.ctx_upd {
        return None;
    }
    let mut out = m.clone();
    match &m.content {
        Some(Value::String(c)) => {
            let (s, ok) = strip_ctx_upd_text(c);
            if !ok {
                return None;
            }
            out.content = text_value(s);
        }
        Some(Value::Array(parts)) => {
            let first = parts.first()?.as_object()?;
            if first.get("type").and_then(Value::as_str) != Some("text") {
                return None;
            }
            let t = first.get("text").and_then(Value::as_str).unwrap_or("");
            if t.is_empty() {
                return None;
            }
            let (s, ok) = strip_ctx_upd_text(t);
            if !ok {
                return None;
            }
            let mut v: Vec<Value> = Vec::with_capacity(parts.len());
            if !s.is_empty() {
                let mut q = first.clone();
                q.insert("text".into(), Value::String(s));
                v.push(Value::Object(q));
            }
            v.extend(parts[1..].iter().cloned());
            out.content = Some(Value::Array(v));
        }
        _ => return None,
    }
    out.ctx_upd = false;
    Some(out)
}

/// `stripContextUpdates` : l'historique sans aucun bloc <context_update>.
fn strip_context_updates(msgs: Vec<Message>) -> Vec<Message> {
    msgs.into_iter()
        .map(|m| without_ctx_update(&m).unwrap_or(m))
        .collect()
}

// ---- bornes et assemblage --------------------------------------------------

/// `compactBounds` : frontières tête/queue. Tête = messages système de tête ;
/// queue = depuis la fin jusqu'à remplir `tail_budget`, reculée jusqu'à une
/// frontière sûre (`user` vrai ou `assistant`). Le torse est
/// `[head, tail_start)`, vide si `tail_start <= head`.
pub fn compact_bounds(msgs: &[Message], tail_budget: i64, echo: &EchoPolicy) -> (usize, usize) {
    let mut head = 0;
    while head < msgs.len() && msgs[head].role == "system" {
        head += 1;
    }
    let mut tail_start = msgs.len();
    let mut acc = 0;
    let et = echo_tokens(msgs, echo);
    for i in (head..msgs.len()).rev() {
        acc += msg_tokens(&msgs[i]);
        if let Some(e) = &et {
            acc += e[i];
        }
        tail_start = i;
        if acc >= tail_budget {
            break;
        }
    }
    while tail_start > head
        && ((msgs[tail_start].role != "user" && msgs[tail_start].role != "assistant")
            || is_loki_injected(&msgs[tail_start]))
    {
        tail_start -= 1;
    }
    (head, tail_start)
}

/// `compactPending` : la demande en cours à réinjecter avant la queue (quand
/// un résumé a été obtenu) ; vide si la queue porte déjà une vraie demande.
pub fn compact_pending(msgs: &[Message], torso: &[Message], tail_start: usize) -> Vec<Message> {
    if msgs[tail_start..]
        .iter()
        .any(|m| m.role == "user" && !is_loki_injected(m))
    {
        return Vec::new();
    }
    for m in torso.iter().rev() {
        if m.role != "user" || is_loki_injected(m) {
            continue;
        }
        if msg_text(m).starts_with(COMPACT_SUMMARY_PREFIX) {
            continue; // résumé d'une compaction précédente, pas une demande
        }
        return vec![m.clone()];
    }
    Vec::new()
}

/// `compactAssemble` : tête, zone résumée, demande réinjectée, queue.
pub fn compact_assemble(
    msgs: &[Message],
    head: usize,
    tail_start: usize,
    mid: &[Message],
    pending: &[Message],
) -> Vec<Message> {
    let mut out = Vec::with_capacity(head + mid.len() + pending.len() + msgs.len() - tail_start);
    out.extend_from_slice(&msgs[..head]);
    out.extend_from_slice(mid);
    out.extend_from_slice(pending);
    out.extend_from_slice(&msgs[tail_start..]);
    out
}

/// `compactHopeless` : même un résumé VIDE ne passerait pas la garantie de
/// réduction (20 %).
pub fn compact_hopeless(
    msgs: &[Message],
    head: usize,
    tail_start: usize,
    echo: &EchoPolicy,
) -> bool {
    let torso = &msgs[head..tail_start];
    let mid = [
        Message {
            role: "user".into(),
            content: text_value(compact_summary_user_msg("", &[])),
            ..Default::default()
        },
        Message {
            role: "assistant".into(),
            content: text_value(COMPACT_ACK_MSG),
            ..Default::default()
        },
    ];
    let floor = compact_assemble(
        msgs,
        head,
        tail_start,
        &mid,
        &compact_pending(msgs, torso, tail_start),
    );
    estimate_tokens(&floor, echo) > estimate_tokens(msgs, echo) * 4 / 5
}

/// `summaryLooksEmpty` : résumé raté (moins de 40 caractères de VRAI texte,
/// hors références recall).
pub fn summary_looks_empty(s: &str) -> bool {
    let s = s.trim();
    if rune_count(s) < 40 {
        return true;
    }
    const CUT: &str = ".,;:()[]-•*";
    let mut real = String::new();
    for f in s.split_whitespace() {
        let low = f.trim_matches(|c| CUT.contains(c)).to_lowercase();
        if low.starts_with("recall:r") || low.starts_with("recall(") {
            continue;
        }
        real.push_str(f);
        real.push(' ');
    }
    rune_count(real.trim()) < 40
}

// ---- recall ----------------------------------------------------------------

/// Un id d'archive et son libellé court (`recallEntry`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RecallEntry {
    pub id: String,
    pub label: String,
}

/// L'archive recall telle que la compaction l'appelle (`archiveRecallBlock`).
pub trait RecallArchive {
    /// Range `content` VERBATIM et rend son id (« r7 »). Une erreur (base
    /// absente…) laisse le bloc non archivé : repli transparent.
    fn archive(&self, label: &str, role: &str, content: &str) -> Result<String, String>;
}

/// Archive absente : rien n'est jamais archivé.
#[derive(Debug, Clone, Copy, Default)]
pub struct NoRecall;

impl RecallArchive for NoRecall {
    fn archive(&self, _label: &str, _role: &str, _content: &str) -> Result<String, String> {
        Err("archive recall indisponible".into())
    }
}

/// `recallEligible` : bloc assez gros d'un rôle réel, qui n'est pas déjà un
/// résumé de compaction.
pub fn recall_eligible(m: &Message) -> bool {
    if !matches!(m.role.as_str(), "tool" | "user" | "assistant") {
        return false;
    }
    let t = msg_text(m);
    if rune_count(&t) <= RECALL_ARCHIVE_MIN_LEN {
        return false;
    }
    !t.starts_with(COMPACT_SUMMARY_PREFIX)
}

/// `torsoToolNames` : ToolCallID → nom d'outil.
fn torso_tool_names(torso: &[Message]) -> HashMap<String, String> {
    let mut names = HashMap::new();
    for m in torso {
        for tc in &m.tool_calls {
            if !tc.id.is_empty() {
                names.insert(tc.id.clone(), tc.function.name.clone());
            }
        }
    }
    names
}

/// `recallLabel` (`chat_recall.go`) : rôle (ou nom d'outil) et première ligne.
fn recall_label(role: &str, tool_name: &str, content: &str) -> String {
    let mut head = content
        .split('\n')
        .map(str::trim)
        .find(|s| !s.is_empty())
        .unwrap_or("")
        .to_string();
    if rune_count(&head) > 70 {
        head = rune_prefix(&head, 70).trim().to_string() + "…";
    }
    let tag = if role == "tool" && !tool_name.is_empty() {
        tool_name
    } else {
        role
    };
    if head.is_empty() {
        return tag.to_string();
    }
    format!("{tag}: {head}")
}

/// `recallMarker` : remplace un bloc archivé dans le torse dégraissé.
pub fn recall_marker(id: &str, label: &str) -> String {
    format!(
        "[Block archived to save context — full content retrievable with recall(\"{id}\") ({label}). Do NOT re-run a tool to fetch it back.]"
    )
}

/// `compactSummaryUserMsg` : le message `user` qui porte le résumé et l'index
/// des blocs rappelables créés à cette passe.
pub fn compact_summary_user_msg(summary: &str, index: &[RecallEntry]) -> String {
    let mut b = String::from(COMPACT_SUMMARY_PREFIX);
    if !index.is_empty() {
        b.push_str(" The earlier turns of this conversation were summarized to save context, but nothing is lost: any block referenced below (or in the summary) as recall:rN can be brought back verbatim with the recall(id) tool, and recall_search(\"keywords\") finds older archived blocks not listed here. Here is the summary:\n\n");
    } else {
        b.push_str(
            " The earlier turns of this conversation were summarized to save context. Here is the summary:\n\n",
        );
    }
    b.push_str(summary);
    if !index.is_empty() {
        b.push_str("\n\nArchived blocks you can bring back with recall(id):\n");
        for e in index {
            b.push_str(&format!("- {} — {}\n", e.id, e.label));
        }
        b.push_str("Only recall a block when you actually need its full content.");
    }
    b
}

/// `renderTranscript` : le torse en texte pour le résumeur, plafonné à
/// ~2,8 octets par jeton de la fenêtre (la FIN est gardée).
///
/// Écart assumé : Go coupe à l'octet, quitte à couper un caractère multioctet
/// (ses octets orphelins deviennent chacun U+FFFD à l'encodage JSON). Une
/// `String` Rust devant rester valide, chaque octet orphelin est remplacé ici
/// par U+FFFD — ce qui part au moteur est identique.
pub fn render_transcript(msgs: &[Message], ctx_window: i64) -> String {
    let mut b = String::new();
    for m in msgs {
        match m.role.as_str() {
            "user" => {
                if is_loki_injected(m) {
                    b.push_str(&format!("System: {}\n", msg_text(m)));
                    continue;
                }
                b.push_str(&format!("User: {}\n", msg_text(m)));
            }
            "assistant" => {
                let t = msg_text(m);
                if !t.is_empty() {
                    b.push_str(&format!("Assistant: {t}\n"));
                }
                for tc in &m.tool_calls {
                    b.push_str(&format!(
                        "Assistant → tool {}({})\n",
                        tc.function.name, tc.function.arguments
                    ));
                }
            }
            "tool" => b.push_str(&format!("Tool result: {}\n", msg_text(m))),
            "system" => b.push_str(&format!("System: {}\n", msg_text(m))),
            _ => {}
        }
    }
    let max_chars = (ctx_window as f64 * 2.8) as usize;
    if max_chars > 0 && b.len() > max_chars {
        let cut = b.len() - max_chars;
        let mut k = cut;
        while !b.is_char_boundary(k) {
            k += 1;
        }
        return format!(
            "[…start truncated…]\n{}{}",
            "\u{FFFD}".repeat(k - cut),
            &b[k..]
        );
    }
    b
}

// ---- le moteur de résumé ---------------------------------------------------

/// Premier choix d'une réponse de résumé (`summaryChoice`).
#[derive(Debug, Clone, PartialEq, Default)]
pub struct SummaryChoice {
    pub content: String,
    pub reasoning_content: String,
    /// Appels d'outil émis malgré tool_choice « none » : jamais un résumé.
    pub tool_calls: Vec<Value>,
    pub finish_reason: String,
}

/// Échec d'une requête de résumé.
#[derive(Debug, Clone, PartialEq)]
pub enum SummaryError {
    /// `summaryHTTPError` : le moteur a refusé la requête (corps tronqué à
    /// 500 octets et rogné). `who` : « llama-server » ou « API externe ».
    Http {
        who: String,
        status: u16,
        body: String,
    },
    /// Toute autre erreur (réseau, décodage…).
    Other(String),
}

impl fmt::Display for SummaryError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            SummaryError::Http { who, status, body } => {
                write!(f, "résumé: {who} {status}: {body}")
            }
            SummaryError::Other(s) => f.write_str(s),
        }
    }
}

impl std::error::Error for SummaryError {}

#[derive(Deserialize)]
struct WireResp {
    #[serde(default)]
    choices: Option<Vec<WireChoice>>,
}

#[derive(Deserialize)]
struct WireChoice {
    #[serde(default)]
    message: WireMsg,
    #[serde(default)]
    finish_reason: Option<String>,
}

#[derive(Deserialize, Default)]
struct WireMsg {
    #[serde(default)]
    content: Option<String>,
    #[serde(default)]
    reasoning_content: Option<String>,
    #[serde(default)]
    tool_calls: Option<Vec<Value>>,
}

/// Décodage d'une réponse non streamée de /v1/chat/completions, comme
/// `postSummary` : seule la PREMIÈRE valeur JSON compte (un octet parasite
/// après elle est ignoré), aucun choix → « résumé: réponse vide ».
pub fn parse_summary_response(raw: &[u8]) -> Result<SummaryChoice, SummaryError> {
    let first = serde_json::Deserializer::from_slice(raw)
        .into_iter::<WireResp>()
        .next();
    let resp = match first {
        None => return Err(SummaryError::Other("EOF".into())),
        Some(Err(e)) => return Err(SummaryError::Other(e.to_string())),
        Some(Ok(r)) => r,
    };
    let Some(ch) = resp.choices.and_then(|c| c.into_iter().next()) else {
        return Err(SummaryError::Other("résumé: réponse vide".into()));
    };
    Ok(SummaryChoice {
        content: ch.message.content.unwrap_or_default(),
        reasoning_content: ch.message.reasoning_content.unwrap_or_default(),
        tool_calls: ch.message.tool_calls.unwrap_or_default(),
        finish_reason: ch.finish_reason.unwrap_or_default(),
    })
}

/// Résumé sur transcription (`summarizeTranscriptFor`). L'implémentation
/// envoie au point de chat résolu (`resolveChatEndpoint`) :
/// `{"model", "messages": [system, user(transcript)], "stream": false,
/// "temperature": 0.2, "max_tokens", "chat_template_kwargs":
/// {"enable_thinking": false}}` — sans `chat_template_kwargs` sur un preset
/// externe ; sur le slot annexe (SIDE_SLOT) s'il y en a un ; télémétrie
/// kind=compact rattachée à la discussion courante.
#[derive(Debug, Clone, PartialEq)]
pub struct TranscriptRequest {
    pub system: String,
    pub transcript: String,
    pub max_tokens: i64,
}

/// Résumé en continuation (`summarizeContinuation`). L'implémentation
/// construit la charge comme le tour (`buildChatPayload(ep, messages, 0.2,
/// {tools, toolChoiceNone: true, effort, kwargs, noSampling: true})`), puis
/// `stream: false`, sans `stream_options`, `max_tokens`, sur le slot de la
/// discussion ; télémétrie kind=compact rattachée à `conv`.
#[derive(Debug, Clone, PartialEq)]
pub struct ContinuationRequest {
    /// La vue d'envoi du tour suivie de la demande de résumé.
    pub messages: Vec<Message>,
    pub tools: Vec<Tool>,
    pub effort: String,
    /// Arguments du gabarit du tour, `enable_thinking` forcé à false.
    pub kwargs: Map<String, Value>,
    pub max_tokens: i64,
    pub conv: String,
}

/// Le moteur vu de la compaction : remplace `postSummary` (HTTP par la porte
/// du moteur) et les lectures d'état global du chemin en continuation.
pub trait Summarizer {
    /// Poste un résumé sur transcription et rend le premier choix.
    fn summarize_transcript(
        &self,
        req: TranscriptRequest,
    ) -> impl Future<Output = Result<SummaryChoice, SummaryError>> + Send;
    /// Poste un résumé en continuation et rend le premier choix.
    fn summarize_continuation(
        &self,
        req: ContinuationRequest,
    ) -> impl Future<Output = Result<SummaryChoice, SummaryError>> + Send;
    /// `engineSlotHolds(conv)`, relu à l'envoi.
    fn slot_holds(&self, conv: &str) -> bool;
    /// `textualToolCallSnippet(content) != ""` (code_retry.go).
    fn textual_tool_call(&self, content: &str) -> bool;
}

/// `cleanSummary` : texte final d'une réponse de résumé. Bloc(s) <think> de
/// tête retirés ; raisonnement seul → erreur ; coupé (filet ou
/// finish_reason=length) → marqué « […] ».
pub fn clean_summary(
    content: &str,
    reasoning: &str,
    finish: &str,
    ctx_window: i64,
) -> Result<String, String> {
    let mut c = content.trim().to_string();
    while let Some(i) = c.find(THINK_CLOSE) {
        if c[..i].find(THINK_OPEN).is_some_and(|o| o > 0) {
            break;
        }
        c = c[i + THINK_CLOSE.len()..].trim().to_string();
        if !c.starts_with(THINK_OPEN) {
            break;
        }
    }
    if c.starts_with(THINK_OPEN) {
        return Err("résumé: raisonnement jamais refermé, aucun résumé".into());
    }
    if c.is_empty() {
        if !reasoning.trim().is_empty() || !content.trim().is_empty() {
            return Err("résumé: rien hors du raisonnement".into());
        }
        return Err("résumé: texte vide".into());
    }
    let mut cut = String::new();
    let cap = summary_rune_cap(ctx_window) as usize;
    if rune_count(&c) > cap {
        c = rune_prefix(&c, cap).trim().to_string();
        cut = format!("par le filet de {cap} caractères");
    }
    if finish == "length" {
        cut = format!("par max_tokens={}", compact_summary_budget(ctx_window));
    }
    if !cut.is_empty() {
        eprintln!(
            "[compact] résumé coupé {} ({} caractères gardés)",
            cut,
            rune_count(&c)
        );
        c.push_str(" […]");
    }
    Ok(c)
}

/// `contextOverflow` : le refus du moteur signale-t-il un débordement de la
/// fenêtre ? Libellés connus, sinon conversation déjà à 90 % de la fenêtre.
pub fn context_overflow(cfg: &CompactConfig, msg: &str, msgs: &[Message]) -> bool {
    let m = msg.to_lowercase();
    for k in [
        "context size",
        "context_size",
        "context length",
        "context_length",
        "context window",
        "n_ctx",
        "too many tokens",
        "prompt is too long",
        "exceeds the context",
    ] {
        if m.contains(k) {
            return true;
        }
    }
    estimate_tokens(msgs, &cfg.echo) >= (cfg.ctx_window as f64 * 0.9) as i64
}

/// `overflowTokens` : taille réelle de la requête refusée par llama.cpp
/// (« request (132291 tokens) exceeds… »), 0 si absente.
pub fn overflow_tokens(msg: &str) -> i64 {
    const LEAD: &str = "request (";
    const TAIL: &str = " tokens)";
    for (i, _) in msg.match_indices(LEAD) {
        let rest = &msg[i + LEAD.len()..];
        let digits = rest.bytes().take_while(u8::is_ascii_digit).count();
        if digits > 0 && rest[digits..].starts_with(TAIL) {
            // Atoi : au-delà de l'entier, la valeur maximale.
            return rest[..digits].parse::<i64>().unwrap_or(i64::MAX);
        }
    }
    0
}

/// `shrinkToFit` : dernier recours quand le moteur a refusé le prompt et que
/// la compaction n'a rien pu faire. Vise 60 % de la fenêtre, corrigés par
/// l'écart entre estimation et taille réelle (`actual`, lue dans l'erreur) :
/// images gardées, puis raisonnement renvoyé (du plus ancien), puis gros
/// résultats d'outils tronqués, puis les plus vieux échanges retirés.
pub fn shrink_to_fit(cfg: &CompactConfig, msgs: &[Message], actual: i64) -> (Vec<Message>, bool) {
    let echo = &cfg.echo;
    let est = estimate_tokens(msgs, echo);
    if est <= 0 {
        return (msgs.to_vec(), false);
    }
    let mut ratio = 1.0;
    if actual > est {
        ratio = actual as f64 / est as f64;
    }
    let target = (cfg.ctx_window as f64 * 0.6 / ratio) as i64;
    let mut out = msgs.to_vec();
    if let Some((s, freed)) = drop_kept_images(&out) {
        log_kept_images_dropped("réduction", freed);
        out = s;
    }
    for i in 0..out.len() {
        if out[i].reasoning_content.is_empty() && out[i].reasoning_model.is_empty() {
            continue;
        }
        if estimate_tokens(&out, echo) <= target {
            break;
        }
        out[i].reasoning_content.clear();
        out[i].reasoning_model.clear();
    }
    for i in 0..out.len() {
        if estimate_tokens(&out, echo) <= target {
            break;
        }
        if out[i].role != "tool" {
            continue;
        }
        let t = msg_text(&out[i]);
        if rune_count(&t) > 2000 {
            out[i].content = text_value(rune_prefix(&t, 1500).to_string() + SHRINK_TRUNC_MARKER);
        }
    }
    let mut head = 0;
    while head < out.len() && out[head].role == "system" {
        head += 1;
    }
    while estimate_tokens(&out, echo) > target {
        // Coupe d'abord aux vraies demandes ; un rappel de loki seulement à
        // défaut.
        let mut next = None;
        let mut nudge = None;
        for (k, m) in out.iter().enumerate().skip(head + 1) {
            if m.role != "user" {
                continue;
            }
            if !is_loki_injected(m) {
                next = Some(k);
                break;
            }
            nudge.get_or_insert(k);
        }
        let Some(next) = next.or(nudge) else { break };
        out.drain(head..next);
    }
    let changed = estimate_tokens(&out, echo) < est;
    (out, changed)
}

/// `capToolResult` : borne tout résultat d'outil à [`TOOL_RESULT_MAX`]
/// caractères.
pub fn cap_tool_result(s: &str) -> String {
    if rune_count(s) > TOOL_RESULT_MAX {
        return rune_prefix(s, TOOL_RESULT_MAX).to_string()
            + "\n[…tronqué : résultat trop long. Cible une partie plus précise si tu as besoin de la suite]";
    }
    s.to_string()
}

// ---- COMPACT_CONTINUATION : options ----------------------------------------

/// `compactView` : la requête du tour telle qu'elle est partie ; `wire` couvre
/// les `n` premiers messages de la vue modèle.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct CompactView {
    pub wire: Vec<Message>,
    pub n: usize,
    pub tools: Vec<Tool>,
    pub effort: String,
    pub kwargs: Map<String, Value>,
    /// Contexte réel (dernier compte du moteur + ce qu'il n'a pas vu).
    pub used: i64,
    /// Discussion, pour la télémétrie et le tampon du slot.
    pub conv: String,
}

/// `compactOpts` : options d'une compaction. Défaut : le chemin d'avant.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct CompactOpts {
    /// `None` : résumé sur transcription.
    pub view: Option<CompactView>,
    /// Pas de résumé quand même un vide serait refusé.
    pub precheck: bool,
    /// Discussion, pour la mémoire des refus.
    pub conv: String,
    pub used: i64,
}

/// Vue d'envoi d'un tour (`turnViewDry` + `turnReasoning` + `wireMessages`),
/// fournie par l'appelant à [`compact_opts_for`].
#[derive(Debug, Clone, PartialEq, Default)]
pub struct TurnView {
    pub wire: Vec<Message>,
    pub tools: Vec<Tool>,
    pub effort: String,
    pub kwargs: Map<String, Value>,
}

/// `compactViewLen` : la vue s'arrête avant les messages `user` de fin.
pub fn compact_view_len(msgs: &[Message]) -> usize {
    let mut k = msgs.len();
    while k > 0 && msgs[k - 1].role == "user" {
        k -= 1;
    }
    k
}

/// `compactOptsFor` : options des compactions de début et de fin de tour.
/// `slot_holds` = `engineSlotHolds(conv)` ; `build` reçoit `msgs[..k]` et
/// rend la vue d'envoi du tour (n'est appelée que si une vue est possible).
pub fn compact_opts_for(
    cfg: &CompactConfig,
    memory: &CompactMemory,
    slot_holds: bool,
    msgs: &[Message],
    used: i64,
    conv: &str,
    build: impl FnOnce(&[Message]) -> TurnView,
) -> CompactOpts {
    if conv.is_empty() || !cfg.continuation {
        return CompactOpts::default();
    }
    let mut opt = CompactOpts {
        precheck: true,
        conv: conv.into(),
        used,
        ..Default::default()
    };
    if used <= 0 || !slot_holds || memory.cont_disabled(cfg) {
        return opt;
    }
    let k = compact_view_len(msgs);
    if k == 0 {
        return opt;
    }
    let tv = build(&msgs[..k]);
    opt.view = Some(CompactView {
        wire: tv.wire,
        n: k,
        tools: tv.tools,
        effort: tv.effort,
        kwargs: tv.kwargs,
        used,
        conv: conv.into(),
    });
    opt
}

/// `compactOptsMid` : options d'une compaction entre deux étapes d'un tour.
/// `messages` porte déjà le prompt système et le bloc projet ; `wire` rend
/// `wireMessages(messages[..k], false, pol)` ; `rescue` : étape de secours.
#[allow(clippy::too_many_arguments)]
pub fn compact_opts_mid(
    cfg: &CompactConfig,
    memory: &CompactMemory,
    slot_holds: bool,
    ep_external: bool,
    conv: &str,
    messages: &[Message],
    used: i64,
    tools: Vec<Tool>,
    effort: &str,
    kwargs: Map<String, Value>,
    rescue: bool,
    wire: impl FnOnce(&[Message]) -> Vec<Message>,
) -> CompactOpts {
    if conv.is_empty() || ep_external || !cfg.continuation {
        return CompactOpts::default();
    }
    let mut opt = CompactOpts {
        precheck: true,
        conv: conv.into(),
        used,
        ..Default::default()
    };
    if rescue || used <= 0 || !slot_holds || memory.cont_disabled(cfg) {
        return opt;
    }
    let k = compact_view_len(messages);
    if k == 0 {
        return opt;
    }
    opt.view = Some(CompactView {
        wire: wire(&messages[..k]),
        n: k,
        tools,
        effort: effort.into(),
        kwargs,
        used,
        conv: conv.into(),
    });
    opt
}

// ---- mémoire des refus et des échecs ---------------------------------------

#[derive(Debug, Clone, PartialEq)]
struct CompactRefusal {
    at: i64,
    window: i64,
    model: String,
    n: usize,
    hash: u64,
}

/// Mémoire en processus (globales Go `compactRefused` et `compactContFails`) :
/// les refus de compaction par discussion et les échecs de la continuation par
/// modèle. Un redémarrage oublie tout.
#[derive(Debug, Default)]
pub struct CompactMemory {
    refused: Mutex<HashMap<String, CompactRefusal>>,
    cont_fails: Mutex<HashMap<String, i64>>,
}

fn lock<T>(m: &Mutex<T>) -> MutexGuard<'_, T> {
    m.lock().unwrap_or_else(|e| e.into_inner())
}

/// `historyHash` : empreinte FNV-1a 64 cumulée des messages (celle de
/// `perfPrefix`), 0 pour un historique vide.
fn history_hash(msgs: &[Message]) -> u64 {
    if msgs.is_empty() {
        return 0;
    }
    let mut h: u64 = 0xcbf2_9ce4_8422_2325;
    let mut feed = |b: u8| {
        h ^= u64::from(b);
        h = h.wrapping_mul(0x0000_0100_0000_01b3);
    };
    for m in msgs {
        for b in serde_json::to_vec(m).unwrap_or_default() {
            feed(b);
        }
        feed(0);
    }
    h
}

impl CompactMemory {
    pub fn new() -> Self {
        Self::default()
    }

    /// `compactRefusedSkip` : la dernière compaction de `conv` a été refusée,
    /// `msgs` ne fait que prolonger l'historique d'alors, et le contexte n'a
    /// pas grossi de 10 % depuis. Jamais à 90 % de la fenêtre ou plus.
    pub fn refused_skip(
        &self,
        cfg: &CompactConfig,
        conv: &str,
        used: i64,
        msgs: &[Message],
    ) -> bool {
        if conv.is_empty() || used <= 0 || !cfg.continuation {
            return false;
        }
        let (model, window) = (&cfg.engine_model, cfg.ctx_window);
        let Some(r) = lock(&self.refused).get(conv).cloned() else {
            return false;
        };
        if &r.model != model
            || r.window != window
            || used < r.at
            || used >= r.at + r.at / 10
            || msgs.len() < r.n
            || history_hash(&msgs[..r.n]) != r.hash
        {
            self.refused_clear(conv);
            return false;
        }
        if used * 10 >= window * 9 {
            return false;
        }
        eprintln!(
            "[compact] pas de nouvel essai : refusée à {} jetons, contexte à {}",
            r.at, used
        );
        true
    }

    /// `compactRefusedNote`.
    pub fn refused_note(&self, cfg: &CompactConfig, conv: &str, used: i64, msgs: &[Message]) {
        if conv.is_empty() || used <= 0 {
            return;
        }
        let hash = history_hash(msgs);
        let mut m = lock(&self.refused);
        if !m.contains_key(conv)
            && m.len() >= COMPACT_REFUSED_MAX
            && let Some(k) = m.keys().next().cloned()
        {
            m.remove(&k);
        }
        m.insert(
            conv.into(),
            CompactRefusal {
                at: used,
                window: cfg.ctx_window,
                model: cfg.engine_model.clone(),
                n: msgs.len(),
                hash,
            },
        );
    }

    /// `compactRefusedClear`.
    pub fn refused_clear(&self, conv: &str) {
        lock(&self.refused).remove(conv);
    }

    /// `compactContDisabled` : continuation suspendue pour le modèle courant.
    pub fn cont_disabled(&self, cfg: &CompactConfig) -> bool {
        lock(&self.cont_fails)
            .get(&cfg.engine_model)
            .is_some_and(|&n| n >= COMPACT_CONT_MAX_FAILS)
    }

    /// `compactContFail` : `hard` = refus du gabarit ou appel d'outil, qui se
    /// reproduiraient à l'identique — suspendue tout de suite.
    pub fn cont_fail(&self, cfg: &CompactConfig, hard: bool) {
        let model = &cfg.engine_model;
        let mut m = lock(&self.cont_fails);
        let prev = m.get(model).copied().unwrap_or(0);
        let mut n = prev + 1;
        if hard {
            n = COMPACT_CONT_MAX_FAILS;
        }
        if n >= COMPACT_CONT_MAX_FAILS && prev < COMPACT_CONT_MAX_FAILS {
            eprintln!(
                "[compact] continuation suspendue pour {model} jusqu'au redémarrage : résumés sur transcription"
            );
        }
        m.insert(model.clone(), n);
    }

    /// `compactContOK` : un résumé réussi remet le compte à zéro.
    pub fn cont_ok(&self, cfg: &CompactConfig) {
        lock(&self.cont_fails).remove(&cfg.engine_model);
    }
}

// ---- COMPACT_CONTINUATION : la requête -------------------------------------

/// `compactWireIndex` : position dans `view.wire` du message `msgs[i]` de la
/// vue modèle (`wire.len()` pour `i == view.n`), `None` si les deux vues ne se
/// correspondent pas (les non-système se suivent un pour un).
pub fn compact_wire_index(msgs: &[Message], view: Option<&CompactView>, i: usize) -> Option<usize> {
    let view = view?;
    if view.n > msgs.len() || i > view.n {
        return None;
    }
    let a: Vec<usize> = (0..view.n).filter(|&j| msgs[j].role != "system").collect();
    let b: Vec<usize> = (0..view.wire.len())
        .filter(|&j| view.wire[j].role != "system")
        .collect();
    if a.len() != b.len() {
        return None;
    }
    let mut at = None;
    for (&ja, &jb) in a.iter().zip(&b) {
        let (m, w) = (&msgs[ja], &view.wire[jb]);
        if m.role != w.role
            || m.tool_call_id != w.tool_call_id
            || m.tool_calls.len() != w.tool_calls.len()
        {
            return None;
        }
        if ja == i {
            at = Some(jb);
        }
    }
    if i == view.n {
        return Some(view.wire.len());
    }
    at
}

fn fields_join(s: &str) -> String {
    s.split_whitespace().collect::<Vec<_>>().join(" ")
}

/// `clipRunes` : `s` sur une ligne, coupé à `n` caractères.
pub fn clip_runes(s: &str, n: usize) -> String {
    let s = fields_join(s);
    if rune_count(&s) <= n {
        return s;
    }
    rune_prefix(&s, n).to_string() + "…"
}

/// `compactBoundaryDesc` : désigne le premier message gardé `m`, qui est
/// `wire[wi]` ; un début déjà vu plus haut est allongé.
pub fn compact_boundary_desc(wire: &[Message], wi: usize, m: &Message) -> String {
    if m.role == "assistant" && msg_text(m).trim().is_empty() && !m.tool_calls.is_empty() {
        let f = &m.tool_calls[0].function;
        return format!(
            "your call to the tool {}({})",
            f.name,
            clip_runes(&f.arguments, 120)
        );
    }
    let who = match m.role.as_str() {
        "assistant" => "your message",
        "tool" => "the tool result",
        _ => "the user message",
    };
    let m = without_ctx_update(m).unwrap_or_else(|| m.clone());
    let text = msg_text(&m);
    let mut snip = clip_runes(&text, 80);
    let probe = snip.strip_suffix('…').unwrap_or(&snip).to_string();
    for w in &wire[..wi] {
        if fields_join(&msg_text(w)).contains(&probe) {
            snip = clip_runes(&text, 200);
            break;
        }
    }
    format!("{who} that starts with «{snip}»")
}

/// `compactContInstruction` : la demande ajoutée en fin de requête. `kept` =
/// messages gardés tels quels à partir de la frontière (0 : tout est résumé).
pub fn compact_cont_instruction(kept: usize, desc: &str, code: bool) -> String {
    let mut b = String::from(COMPACT_CONT_PREFIX);
    b.push_str(" Pause the task: do not answer and do not call any tool. The context is full: the older part of this conversation will be replaced by the summary you write now, and your past reasoning will be lost — this summary is the only memory kept of that part.\n");
    if kept > 0 {
        b.push_str(&format!("Summarize every message BEFORE {desc}. That message and the ones after it (the last {kept}) stay verbatim after your summary: mention them briefly at most, but never leave out anything that comes before. Write the STATE OF PROGRESS as of that message, not later.\n"));
    } else {
        b.push_str("Summarize the whole conversation above.\n");
    }
    b.push_str("Leave out the system instructions and any <project_context> or <context_update> block: they are sent again separately.\n\n");
    b.push_str(COMPACT_SUMMARY_RULES);
    if code {
        b.push_str(CODE_SUMMARY_RULES);
    }
    b
}

// ---- la compaction ---------------------------------------------------------

/// Sous-ensemble des capacités du tour (`Caps` Go) que lit la compaction.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct CompactCaps {
    /// Mode agent : le modèle a ses outils (dont recall) — on archive.
    pub agent: bool,
    /// Mode Code : règles de résumé du code, critères rendus après résumé.
    pub code: bool,
}

/// La compaction avec ses dépendances explicites.
pub struct Compactor<'a, S, R> {
    pub cfg: &'a CompactConfig,
    pub summarizer: &'a S,
    pub recall: &'a R,
    pub memory: &'a CompactMemory,
    /// Mode Code : `critRender(critList(conv))` de la discussion, "" si aucun
    /// critère.
    pub criteria: &'a str,
}

impl<S: Summarizer, R: RecallArchive> Compactor<'_, S, R> {
    /// `MaybeCompact` : compacte si (et seulement si) le seuil proactif est
    /// dépassé. `known_tokens` ≤ 0 : repli sur l'estimation.
    pub async fn maybe_compact(
        &self,
        msgs: &[Message],
        caps: CompactCaps,
        known_tokens: i64,
    ) -> (Vec<Message>, bool) {
        if !compact_would_trigger(self.cfg, msgs, known_tokens) {
            return (msgs.to_vec(), false);
        }
        self.compact_messages(msgs, caps).await
    }

    /// `compactMessages` : compaction sans tenir compte du seuil (filet
    /// réactif, bouton manuel). Rend l'historique et s'il a changé.
    pub async fn compact_messages(
        &self,
        msgs: &[Message],
        caps: CompactCaps,
    ) -> (Vec<Message>, bool) {
        let (out, changed, _) = self
            .compact_messages_opt(msgs, caps, &CompactOpts::default())
            .await;
        (out, changed)
    }

    /// `compactMessagesNoted` : `compact_messages_opt`, plus la mémoire des
    /// refus.
    pub async fn compact_messages_noted(
        &self,
        msgs: &[Message],
        caps: CompactCaps,
        opt: &CompactOpts,
    ) -> (Vec<Message>, bool) {
        let (out, changed, refused) = self.compact_messages_opt(msgs, caps, opt).await;
        if !opt.conv.is_empty() {
            if changed {
                self.memory.refused_clear(&opt.conv);
            } else if refused {
                self.memory
                    .refused_note(self.cfg, &opt.conv, opt.used, msgs);
            }
        }
        (out, changed)
    }

    /// `compactMessagesOpt` : les images gardées sont retirées D'ABORD (leur
    /// retrait suffit à changer l'historique), puis la compaction elle-même.
    /// Rend (historique, changé, refusé) — refusé : un résumé a été obtenu
    /// mais la réduction reste sous les 20 %.
    pub async fn compact_messages_opt(
        &self,
        msgs: &[Message],
        caps: CompactCaps,
        opt: &CompactOpts,
    ) -> (Vec<Message>, bool, bool) {
        let Some((stripped, freed)) = drop_kept_images(msgs) else {
            return self.compact_messages_core(msgs, caps, opt).await;
        };
        log_kept_images_dropped("compaction", freed);
        let (out, changed, _) = self.compact_messages_core(&stripped, caps, opt).await;
        if changed {
            return (out, true, false);
        }
        (stripped, true, false)
    }

    /// `compactMessagesCore` : la compaction elle-même.
    pub async fn compact_messages_core(
        &self,
        msgs: &[Message],
        caps: CompactCaps,
        opt: &CompactOpts,
    ) -> (Vec<Message>, bool, bool) {
        let echo = &self.cfg.echo;
        let tail_budget = (estimate_tokens(msgs, echo) as f64 * COMPACT_TAIL_FRAC) as i64;
        let (head, tail_start) = compact_bounds(msgs, tail_budget, echo);
        if tail_start <= head {
            return (msgs.to_vec(), false, false);
        }
        let torso = &msgs[head..tail_start];

        if opt.precheck && compact_hopeless(msgs, head, tail_start, echo) {
            eprintln!("[compact] résumé non demandé : même vide, la réduction resterait sous 20 %");
            return (msgs.to_vec(), false, false);
        }

        // Archivage des gros blocs (mode agent seulement).
        let mut archived: Vec<Option<RecallEntry>> = vec![None; torso.len()];
        let mut index: Vec<RecallEntry> = Vec::new();
        if caps.agent && self.cfg.enabled {
            let names = torso_tool_names(torso);
            for (i, m) in torso.iter().enumerate() {
                if !recall_eligible(m) {
                    continue;
                }
                let content = msg_text(m);
                let tool = names.get(&m.tool_call_id).map_or("", String::as_str);
                let label = recall_label(&m.role, tool, &content);
                let Ok(id) = self.recall.archive(&label, &m.role, &content) else {
                    continue;
                };
                let e = RecallEntry { id, label };
                archived[i] = Some(e.clone());
                index.push(e);
            }
        }

        // Dégraissage sans IA : repli seulement si le résumé échoue.
        let pruned: Vec<Message> = torso
            .iter()
            .zip(&archived)
            .map(|(m, a)| {
                let mut p = m.clone();
                p.reasoning_content.clear();
                p.reasoning_model.clear();
                if let Some(e) = a {
                    p.content = text_value(recall_marker(&e.id, &e.label));
                } else if m.role == "tool" && msg_text(m).len() > COMPACT_TOOL_PRUNE_LEN {
                    p.content = text_value(COMPACT_PRUNED_MARKER);
                }
                p
            })
            .collect();

        // Entrée du résumeur : le torse ORIGINAL, blocs archivés réduits à leur
        // tête, résultats d'outils raccourcis.
        let for_summary: Vec<Message> = torso
            .iter()
            .zip(&archived)
            .map(|(m, a)| {
                let mut f = m.clone();
                let t = msg_text(m);
                if a.is_some() {
                    let head_txt = if rune_count(&t) > RECALL_SUMMARY_HEAD_LEN {
                        rune_prefix(&t, RECALL_SUMMARY_HEAD_LEN).to_string() + "…"
                    } else {
                        t
                    };
                    f.content = text_value(head_txt + "\n[…reste de ce bloc omis ici]");
                } else if m.role == "tool" && rune_count(&t) > COMPACT_TOOL_SUMMARY_LEN {
                    f.content = text_value(
                        rune_prefix(&t, COMPACT_TOOL_SUMMARY_LEN).to_string() + "\n[…suite coupée]",
                    );
                }
                f
            })
            .collect();
        let for_summary = strip_context_updates(for_summary);
        let window = self.cfg.ctx_window;
        let res = match &opt.view {
            Some(view) => match self
                .summarize_continuation(msgs, tail_start, view, caps.code)
                .await
            {
                Ok(s) => Ok(s),
                Err(e) => {
                    eprintln!("[compact] continuation écartée ({e}) : résumé sur transcription");
                    self.summarize_transcript_for(
                        render_transcript(&for_summary, window),
                        caps.code,
                    )
                    .await
                }
            },
            None => {
                self.summarize_transcript_for(render_transcript(&for_summary, window), caps.code)
                    .await
            }
        };

        let summarized = matches!(&res, Ok(s) if !summary_looks_empty(s));
        let mid = match res {
            Ok(summary) if summarized => {
                let mut sum_msg = compact_summary_user_msg(&summary, &index);
                if caps.code && !self.criteria.is_empty() {
                    sum_msg.push_str("\n\nAcceptance criteria (current state):\n");
                    sum_msg.push_str(self.criteria);
                }
                vec![
                    Message {
                        role: "user".into(),
                        content: text_value(sum_msg),
                        ..Default::default()
                    },
                    Message {
                        role: "assistant".into(),
                        content: text_value(COMPACT_ACK_MSG),
                        ..Default::default()
                    },
                ]
            }
            _ => pruned,
        };

        // La demande EN COURS n'est jamais diluée dans le résumé.
        let pending = if summarized {
            compact_pending(msgs, torso, tail_start)
        } else {
            Vec::new()
        };
        let out = compact_assemble(msgs, head, tail_start, &mid, &pending);

        // Garantie de réduction : au moins ~20 % du contexte estimé.
        let (before, after) = (estimate_tokens(msgs, echo), estimate_tokens(&out, echo));
        if after > before * 4 / 5 {
            return (msgs.to_vec(), false, summarized);
        }
        (out, true, false)
    }

    /// `summarizeTranscript` : résumé sur transcription, hors mode Code.
    pub async fn summarize_transcript(&self, transcript: String) -> Result<String, String> {
        self.summarize_transcript_for(transcript, false).await
    }

    /// `summarizeTranscriptFor` : `code` = résumé d'une session du mode Code.
    pub async fn summarize_transcript_for(
        &self,
        transcript: String,
        code: bool,
    ) -> Result<String, String> {
        let mut system = String::from(COMPACT_TRANSCRIPT_INTRO);
        system.push_str(COMPACT_SUMMARY_RULES);
        if code {
            system.push_str(CODE_SUMMARY_RULES);
        }
        let window = self.cfg.ctx_window;
        let ch = self
            .summarizer
            .summarize_transcript(TranscriptRequest {
                system,
                transcript,
                max_tokens: compact_summary_budget(window),
            })
            .await
            .map_err(|e| e.to_string())?;
        clean_summary(
            &ch.content,
            &ch.reasoning_content,
            &ch.finish_reason,
            window,
        )
    }

    /// `summarizeContinuation` : résumé du torse `[.., tail_start)` en
    /// prolongeant la requête du tour (`view`). Toute erreur fait retomber
    /// l'appelant sur la transcription.
    pub async fn summarize_continuation(
        &self,
        msgs: &[Message],
        tail_start: usize,
        view: &CompactView,
        code: bool,
    ) -> Result<String, String> {
        let cfg = self.cfg;
        if cfg.external {
            return Err("preset externe".into());
        }
        if tail_start > view.n {
            return Err("frontière hors de la vue".into());
        }
        if !self.summarizer.slot_holds(&view.conv) {
            return Err("slot repris entre-temps".into());
        }
        let Some(wi) = compact_wire_index(msgs, Some(view), tail_start) else {
            return Err("vue d'envoi désalignée".into());
        };
        let kept = view.wire.len() - wi;
        let desc = if kept > 0 {
            compact_boundary_desc(&view.wire, wi, &msgs[tail_start])
        } else {
            String::new()
        };
        let instr = Message {
            role: "user".into(),
            content: text_value(compact_cont_instruction(kept, &desc, code)),
            ..Default::default()
        };
        let window = cfg.ctx_window;
        let budget = compact_summary_budget(window);
        let need = view.used + msg_tokens(&instr) + budget + window * 5 / 100;
        if need > window {
            return Err(format!(
                "marge insuffisante : {need} jetons pour une fenêtre de {window}"
            ));
        }
        let mut kwargs = view.kwargs.clone();
        kwargs.insert("enable_thinking".into(), Value::Bool(false));
        let mut sent = view.wire.clone();
        sent.push(instr);
        let res = self
            .summarizer
            .summarize_continuation(ContinuationRequest {
                messages: sent,
                tools: view.tools.clone(),
                effort: view.effort.clone(),
                kwargs,
                max_tokens: budget,
                conv: view.conv.clone(),
            })
            .await;
        let ch = match res {
            Ok(ch) => ch,
            Err(e) => {
                // Refus de la requête elle-même : il se reproduirait à chaque
                // compaction. Une fenêtre pleine ou une coupure réseau, non.
                if let SummaryError::Http { status, body, .. } = &e
                    && matches!(status, 400 | 422 | 500)
                    && !context_overflow(cfg, body, &[])
                {
                    self.memory.cont_fail(cfg, true);
                }
                return Err(e.to_string());
            }
        };
        if !ch.tool_calls.is_empty() {
            self.memory.cont_fail(cfg, true);
            return Err("appel d'outil émis au lieu d'un résumé".into());
        }
        if self.summarizer.textual_tool_call(&ch.content) {
            self.memory.cont_fail(cfg, false);
            return Err("appel d'outil écrit en texte".into());
        }
        let s = match clean_summary(
            &ch.content,
            &ch.reasoning_content,
            &ch.finish_reason,
            window,
        ) {
            Ok(s) => s,
            Err(e) => {
                self.memory.cont_fail(cfg, false);
                return Err(e);
            }
        };
        if summary_looks_empty(&s) {
            self.memory.cont_fail(cfg, false);
            return Err("résumé vide".into());
        }
        self.memory.cont_ok(cfg);
        Ok(s)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::chat::types::{ToolCall, ToolCallFunc, ToolFunction};
    use serde_json::json;
    use std::sync::atomic::{AtomicBool, Ordering};

    // Helpers pour construire des historiques de test lisibles (comme en Go).
    fn um(s: &str) -> Message {
        Message {
            role: "user".into(),
            content: text_value(s),
            ..Default::default()
        }
    }
    fn am(s: &str) -> Message {
        Message {
            role: "assistant".into(),
            content: text_value(s),
            ..Default::default()
        }
    }
    fn atc(name: &str) -> Message {
        Message {
            role: "assistant".into(),
            tool_calls: vec![ToolCall {
                id: "c1".into(),
                function: ToolCallFunc {
                    name: name.into(),
                    arguments: "{}".into(),
                },
                ..Default::default()
            }],
            ..Default::default()
        }
    }
    fn tm(s: &str) -> Message {
        Message {
            role: "tool".into(),
            tool_call_id: "c1".into(),
            content: text_value(s),
            ..Default::default()
        }
    }
    fn sys(s: &str) -> Message {
        Message {
            role: "system".into(),
            content: text_value(s),
            ..Default::default()
        }
    }
    fn letter(n: usize) -> char {
        (b'a' + n as u8) as char
    }
    fn win(w: i64) -> CompactConfig {
        CompactConfig {
            ctx_window: w,
            ..Default::default()
        }
    }
    fn off() -> EchoPolicy {
        EchoPolicy::default()
    }
    fn page(n: usize) -> Message {
        tm(&format!(
            "contenu de page web très long {}{}",
            letter(n),
            "x".repeat(400)
        ))
    }
    fn budget_nudge(calls: i64, sent: i64) -> String {
        match sent {
            0 => format!(
                "[system] You have made {calls} tool calls in this single turn. That is a lot: you almost certainly have enough to answer already. Stop exploring and write your final answer now, unless exactly one specific call is genuinely still missing."
            ),
            1 => format!(
                "[system] {calls} tool calls now, and you are going in circles. Re-reading a file you have already read, or re-deriving a conclusion you have already written, adds nothing. Answer NOW with what you have. If something remains uncertain, say so in the answer instead of investigating further."
            ),
            _ => format!(
                "[system] {calls} tool calls. Stop. Write your final answer in this message. Do not call another tool."
            ),
        }
    }
    fn retry_corrective(snippet: &str) -> String {
        format!(
            "{RETRY_CORRECTIVE_LEAD}{snippet}\nTool calls must go through the tool-call protocol, never in the answer text. Redo it now: emit the real tool call, or answer directly without pretending to call a tool."
        )
    }
    fn choice(content: &str) -> SummaryChoice {
        SummaryChoice {
            content: content.into(),
            finish_reason: "stop".into(),
            ..Default::default()
        }
    }

    // ---- faux moteur et fausse archive ----

    #[derive(Debug, Clone)]
    enum Call {
        Transcript(TranscriptRequest),
        Cont(ContinuationRequest),
    }

    /// Faux moteur : `transcript` None = pas de llama-server (erreur réseau).
    struct FakeEngine {
        transcript: Mutex<Option<SummaryChoice>>,
        cont_status: u16,
        cont_reply: Option<SummaryChoice>,
        slot: AtomicBool,
        calls: Mutex<Vec<Call>>,
    }

    impl FakeEngine {
        fn none() -> Self {
            FakeEngine {
                transcript: Mutex::new(None),
                cont_status: 0,
                cont_reply: None,
                slot: AtomicBool::new(true),
                calls: Mutex::new(Vec::new()),
            }
        }
        fn replying(content: &str) -> Self {
            let e = Self::none();
            *e.transcript.lock().unwrap() = Some(choice(content));
            e
        }
        fn set_reply(&self, c: SummaryChoice) {
            *self.transcript.lock().unwrap() = Some(c);
        }
        fn calls(&self) -> Vec<Call> {
            self.calls.lock().unwrap().clone()
        }
    }

    impl Summarizer for FakeEngine {
        async fn summarize_transcript(
            &self,
            req: TranscriptRequest,
        ) -> Result<SummaryChoice, SummaryError> {
            self.calls.lock().unwrap().push(Call::Transcript(req));
            match self.transcript.lock().unwrap().clone() {
                Some(c) => Ok(c),
                None => Err(SummaryError::Other("connexion refusée".into())),
            }
        }
        async fn summarize_continuation(
            &self,
            req: ContinuationRequest,
        ) -> Result<SummaryChoice, SummaryError> {
            self.calls.lock().unwrap().push(Call::Cont(req));
            if self.cont_status != 0 {
                return Err(SummaryError::Http {
                    who: "llama-server".into(),
                    status: self.cont_status,
                    body: r#"{"error":{"message":"template error"}}"#.into(),
                });
            }
            Ok(self
                .cont_reply
                .clone()
                .unwrap_or_else(|| choice(CONT_SUMMARY)))
        }
        fn slot_holds(&self, conv: &str) -> bool {
            !conv.is_empty() && self.slot.load(Ordering::SeqCst)
        }
        fn textual_tool_call(&self, content: &str) -> bool {
            content.contains("<tool_call>")
        }
    }

    #[derive(Default)]
    struct MemRecall {
        blocks: Mutex<Vec<(String, String, String)>>,
    }

    impl RecallArchive for MemRecall {
        fn archive(&self, label: &str, role: &str, content: &str) -> Result<String, String> {
            let mut b = self.blocks.lock().unwrap();
            b.push((label.into(), role.into(), content.into()));
            Ok(format!("r{}", b.len()))
        }
    }

    struct Env {
        cfg: CompactConfig,
        engine: FakeEngine,
        recall: MemRecall,
        memory: CompactMemory,
        criteria: String,
    }

    impl Env {
        fn new(cfg: CompactConfig, engine: FakeEngine) -> Self {
            Env {
                cfg,
                engine,
                recall: MemRecall::default(),
                memory: CompactMemory::new(),
                criteria: String::new(),
            }
        }
        fn c(&self) -> Compactor<'_, FakeEngine, MemRecall> {
            Compactor {
                cfg: &self.cfg,
                summarizer: &self.engine,
                recall: &self.recall,
                memory: &self.memory,
                criteria: &self.criteria,
            }
        }
    }

    const NONE: CompactCaps = CompactCaps {
        agent: false,
        code: false,
    };
    const AGENT: CompactCaps = CompactCaps {
        agent: true,
        code: false,
    };

    // ---- chat_compact_test.go ----

    #[test]
    fn compact_bounds_protects_head() {
        let msgs = vec![
            sys("sys"),
            um("premier"),
            am("r1"),
            um("q2"),
            am("r2"),
            um("q3"),
            am("r3"),
        ];
        let (head, tail) = compact_bounds(&msgs, 1, &off());
        assert_eq!(head, 1, "head = systèmes uniquement");
        assert_eq!(
            msgs[head].role, "user",
            "le 1er message user doit être compactable"
        );
        let r = &msgs[tail].role;
        assert!(r == "user" || r == "assistant", "queue sur {r}");
        assert!(tail > head, "torse vide (tail={tail} head={head})");
    }

    fn assert_no_orphan_tool(msgs: &[Message], tail: usize) {
        for i in tail..msgs.len() {
            if msgs[i].role == "tool" {
                assert!(
                    i != tail && (msgs[i - 1].role == "assistant" || msgs[i - 1].role == "tool"),
                    "message tool orphelin à l'index {i} de la queue"
                );
            }
        }
    }

    #[test]
    fn compact_bounds_keeps_tool_pairs() {
        let msgs = vec![
            um("q1"),
            am("r1"),
            um("q2"),
            atc("bash"),
            tm("sortie longue"),
            am("r2"),
            um("q3"),
            am("r3"),
        ];
        let budget = msg_tokens(&msgs[6]) + msg_tokens(&msgs[7]) + msg_tokens(&msgs[5]) + 1;
        let (_, tail) = compact_bounds(&msgs, budget, &off());
        let r = &msgs[tail].role;
        assert!(r == "user" || r == "assistant", "frontière non sûre {r}");
        assert_no_orphan_tool(&msgs, tail);
    }

    #[test]
    fn compact_bounds_splits_long_tool_loop() {
        let mut msgs = vec![um("premier"), am("ok"), um("cherche des trucs")];
        for i in 0..10 {
            msgs.push(atc("web_read"));
            msgs.push(page(i));
        }
        let (head, tail) = compact_bounds(&msgs, estimate_tokens(&msgs, &off()) / 4, &off());
        assert!(tail > head, "torse vide (tail={tail} head={head})");
        let r = &msgs[tail].role;
        assert!(r == "user" || r == "assistant", "frontière non sûre {r}");
        assert_no_orphan_tool(&msgs, tail);
        assert!(
            msgs[head..tail].iter().any(|m| m.role == "tool"),
            "aucun résultat d'outil dans le torse"
        );
    }

    fn train_history() -> Vec<Message> {
        let mut msgs = vec![
            um("première question de la conversation"),
            am("ok"),
            um("cherche les horaires du train pour Lyon"),
        ];
        for i in 0..10 {
            msgs.push(atc("web_read"));
            msgs.push(page(i));
        }
        msgs
    }

    #[tokio::test]
    async fn compact_keeps_pending_request() {
        // Pas de moteur : torse dégraissé, la demande doit survivre.
        let env = Env::new(CompactConfig::default(), FakeEngine::none());
        let (out, _) = env.c().compact_messages(&train_history(), NONE).await;
        assert!(
            out.iter()
                .any(|m| m.role == "user" && msg_text(m).contains("horaires du train")),
            "la demande en cours a disparu de l'historique compacté"
        );
    }

    #[test]
    fn summary_input_keeps_tool_findings() {
        let fait = "le train de 14h12 part quai 3";
        let torso = [
            atc("web_read"),
            tm(&(fait.to_string() + &" blabla de remplissage".repeat(300))),
        ];
        let for_summary: Vec<Message> = torso
            .iter()
            .map(|m| {
                let mut f = m.clone();
                let t = msg_text(m);
                if m.role == "tool" && rune_count(&t) > COMPACT_TOOL_SUMMARY_LEN {
                    f.content = text_value(
                        rune_prefix(&t, COMPACT_TOOL_SUMMARY_LEN).to_string() + "\n[…suite coupée]",
                    );
                }
                f
            })
            .collect();
        let tr = render_transcript(&for_summary, 32768);
        assert!(
            tr.contains(fait),
            "le fait trouvé n'atteint pas le résumeur"
        );
        assert!(!tr.contains(COMPACT_PRUNED_MARKER));
        assert!(
            rune_count(&tr) <= 4000,
            "transcription non bornée ({})",
            rune_count(&tr)
        );
    }

    #[test]
    fn estimate_tokens_grows() {
        let small = estimate_tokens(&[um("court")], &off());
        let big = estimate_tokens(
            &[um(
                "un message nettement plus long que le précédent pour dépasser",
            )],
            &off(),
        );
        assert!(big > small, "small={small} big={big}");
    }

    #[tokio::test]
    async fn compact_sans_resume_ne_double_pas_la_demande() {
        let env = Env::new(CompactConfig::default(), FakeEngine::none());
        let (out, _) = env.c().compact_messages(&train_history(), NONE).await;
        let n = out
            .iter()
            .filter(|m| m.role == "user" && msg_text(m).contains("horaires du train"))
            .count();
        assert_eq!(n, 1, "demande en cours présente {n} fois après compactage");
    }

    #[tokio::test]
    async fn compactage_mode_code_garde_l_etat() {
        let mut env = Env::new(
            CompactConfig::default(),
            FakeEngine::replying(
                "- main.go modifié : la fonction parse corrigée, go build et go test passent",
            ),
        );
        env.criteria = "[ ] #1 les tests passent".into();
        let mut msgs = vec![um("corrige le build"), am("ok"), um("continue")];
        for i in 0..10 {
            msgs.push(atc("bash"));
            msgs.push(tm(&format!(
                "sortie de commande {}{}",
                letter(i),
                "x".repeat(400)
            )));
        }
        let (out, changed) = env
            .c()
            .compact_messages(
                &msgs,
                CompactCaps {
                    agent: true,
                    code: true,
                },
            )
            .await;
        assert!(changed, "pas compacté");
        let calls = env.engine.calls();
        let Some(Call::Transcript(req)) = calls.first() else {
            panic!("résumé sur transcription attendu");
        };
        assert!(
            req.system.contains("CODING session"),
            "consigne du mode Code absente"
        );
        assert!(
            out.iter().any(|m| {
                let t = msg_text(m);
                t.contains("Acceptance criteria (current state)") && t.contains("les tests passent")
            }),
            "critères non rendus après compactage"
        );
    }

    #[test]
    fn clean_summary_cases() {
        let w = 65536;
        let cap = summary_rune_cap(w) as usize;
        let long = "é".repeat(7000) + " ÉTAT D'AVANCEMENT";
        let huge = "x".repeat(cap + 500);
        let huge_want = "x".repeat(cap) + " […]";
        let cases: Vec<(&str, &str, &str, &str, &str, bool)> = vec![
            (
                "simple",
                "  - fait A\n- fait B ",
                "",
                "stop",
                "- fait A\n- fait B",
                false,
            ),
            ("long gardé entier", &long, "", "stop", &long, false),
            (
                "think retiré",
                "<think>je réfléchis</think>\n- résumé",
                "",
                "stop",
                "- résumé",
                false,
            ),
            (
                "length gardé et marqué",
                "- début du résumé",
                "",
                "length",
                "- début du résumé […]",
                false,
            ),
            ("au-delà du filet", &huge, "", "stop", &huge_want, false),
            (
                "balise citée en milieu",
                "- le parseur retire <think> en tête",
                "",
                "stop",
                "- le parseur retire <think> en tête",
                false,
            ),
            (
                "bloc cité en milieu gardé entier",
                "- A\n- le parseur retire <think>x</think> en tête\n- B",
                "",
                "stop",
                "- A\n- le parseur retire <think>x</think> en tête\n- B",
                false,
            ),
            (
                "think retiré, balise citée après",
                "<think>r</think>\n- A\n- le parseur coupe à </think>\n- B",
                "",
                "stop",
                "- A\n- le parseur coupe à </think>\n- B",
                false,
            ),
            (
                "ouverture dans le prompt",
                "je réfléchis</think>\n- résumé",
                "",
                "stop",
                "- résumé",
                false,
            ),
            (
                "deux blocs en tête",
                "<think>a</think><think>b</think>\n- résumé",
                "",
                "stop",
                "- résumé",
                false,
            ),
            (
                "second bloc jamais refermé",
                "<think>a</think>\n<think>b",
                "",
                "length",
                "",
                true,
            ),
            (
                "think jamais refermé",
                "<think>je réfléchis encore",
                "",
                "length",
                "",
                true,
            ),
            (
                "que du reasoning_content",
                "",
                "je réfléchis",
                "length",
                "",
                true,
            ),
            (
                "think vide de réponse",
                "<think>x</think>  ",
                "",
                "stop",
                "",
                true,
            ),
            ("vide", "", "", "stop", "", true),
        ];
        for (name, content, reasoning, finish, want, is_err) in cases {
            let got = clean_summary(content, reasoning, finish, w);
            assert_eq!(got.is_err(), is_err, "{name} : {got:?}");
            assert_eq!(got.unwrap_or_default(), want, "{name}");
        }
    }

    #[tokio::test]
    async fn summarize_long_not_truncated() {
        let long = "ü".repeat(7000);
        let env = Env::new(win(65536), FakeEngine::replying(&long));
        let got = env
            .c()
            .summarize_transcript_for("transcript".into(), false)
            .await;
        assert_eq!(got.as_deref(), Ok(long.as_str()), "résumé tronqué");
        env.engine.set_reply(SummaryChoice {
            reasoning_content: "hmm".into(),
            finish_reason: "length".into(),
            ..Default::default()
        });
        let got = env
            .c()
            .summarize_transcript_for("transcript".into(), false)
            .await;
        assert!(
            got.is_err(),
            "raisonnement seul accepté comme résumé : {got:?}"
        );
    }

    // ---- chat_compact_overflow_test.go ----

    #[test]
    fn context_overflow_cases() {
        let cfg = CompactConfig::default();
        let small = [um("salut")];
        for m in [
            r#"{"error":{"code":400,"message":"request (70000 tokens) exceeds the available context size (65536 tokens), try increasing it","type":"exceed_context_size_error"}}"#,
            r#"{"error":{"message":"This model's maximum context length is 8192 tokens","code":"context_length_exceeded"}}"#,
        ] {
            assert!(
                context_overflow(&cfg, m, &small),
                "débordement non reconnu : {m}"
            );
        }
        for m in [
            r#"{"error":{"code":500,"message":"Failed to parse tool call arguments as JSON","type":"server_error"}}"#,
            r#"{"error":{"code":503,"message":"Loading model","type":"unavailable_error"}}"#,
            "Unable to generate parser for this template",
        ] {
            assert!(!context_overflow(&cfg, m, &small), "faux débordement : {m}");
        }
    }

    #[test]
    fn shrink_to_fit_single_turn() {
        let cfg = CompactConfig::default();
        let big = "x".repeat(200000);
        let msgs = vec![
            sys("ctx"),
            um("vieux"),
            am("ok"),
            um("lis tout"),
            Message {
                role: "assistant".into(),
                content: text_value(""),
                tool_calls: vec![ToolCall {
                    id: "a".into(),
                    ..Default::default()
                }],
                ..Default::default()
            },
            Message {
                tool_call_id: "a".into(),
                ..tm(&big)
            },
            Message {
                tool_call_id: "b".into(),
                ..tm(&big)
            },
        ];
        let (out, changed) = shrink_to_fit(&cfg, &msgs, 132291);
        assert!(changed && out[0].role == "system", "pas réduit");
        let est = estimate_tokens(&out, &off());
        assert!(
            est <= (cfg.ctx_window as f64 * 0.6) as i64,
            "encore trop gros : {est}"
        );
        assert_eq!(
            overflow_tokens(r#"{"message":"request (132291 tokens) exceeds"}"#),
            132291
        );
    }

    #[test]
    fn cap_tool_result_cases() {
        assert_eq!(cap_tool_result("court"), "court");
        let got = cap_tool_result(&"é".repeat(TOOL_RESULT_MAX + 500));
        assert!(rune_count(&got) <= TOOL_RESULT_MAX + 200 && got.contains("tronqué"));
    }

    // ---- ctx_accounting_test.go ----

    #[test]
    fn gen_headroom_short_cases() {
        let cases = [
            ("65k, inconnu", 60000, 0, 65536, false, true),
            (
                "65k, courte : sous le seuil, pas d'avance",
                49000,
                2000,
                65536,
                false,
                false,
            ),
            (
                "65k, courte : plancher 8k + marge, sous 84 %",
                55000,
                2000,
                65536,
                false,
                true,
            ),
            (
                "65k, longue : 1,2×15k ne tient plus",
                47000,
                15000,
                65536,
                true,
                false,
            ),
            (
                "65k, longue mais la place y est",
                40000,
                15000,
                65536,
                false,
                false,
            ),
            (
                "65k, énorme (45k) mais historique tout juste compacté",
                20000,
                45000,
                65536,
                false,
                false,
            ),
            (
                "65k, énorme (45k) au-delà de la moitié",
                33000,
                45000,
                65536,
                true,
                false,
            ),
            (
                "131k, courte : rien avant le seuil",
                98000,
                4000,
                131072,
                false,
                false,
            ),
            (
                "131k, très longue génération",
                95000,
                30000,
                131072,
                true,
                false,
            ),
            ("8k, plancher ramené à 1/8", 6000, 0, 8192, false, false),
            ("contexte inconnu", 0, 30000, 65536, false, false),
        ];
        for (name, used, peak, window, want, ratio_triggers) in cases {
            assert_eq!(gen_headroom_short(used, peak, window), want, "{name}");
            let ratio = used >= (window as f64 * COMPACT_TRIGGER_FRAC) as i64;
            assert_eq!(ratio, ratio_triggers, "{name} : cas mal posé");
        }
        for w in [4096, 8192, 16384, 32768, 65536, 131072, 262144] {
            let below = (w as f64 * COMPACT_TRIGGER_FRAC) as i64 - 1;
            assert!(
                !gen_headroom_short(below, 1, w),
                "fenêtre {w} : devance le seuil"
            );
            assert!(
                !gen_headroom_short(w / 2, w, w),
                "fenêtre {w} : part à 50 %"
            );
            assert!(
                gen_headroom_short(w / 2 + 1, w, w),
                "fenêtre {w} : ne part pas au-delà de 50 %"
            );
        }
    }

    #[test]
    fn ctx_pending_cases() {
        let msgs = [um("bonjour"), am("salut"), um(&"x".repeat(4000))];
        let added = estimate_tokens(&msgs[2..], &off());
        let cases = [
            ("nouveau message estimé", 30000, 2, 30000 + added),
            ("rien de nouveau", 30000, 3, 30000),
            ("mesure inconnue (rechargement)", 30000, 0, 30000),
            ("historique raccourci depuis", 30000, 5, 30000),
            ("aucun compte : repli ailleurs", 0, 2, 0),
        ];
        for (name, used, measured, want) in cases {
            assert_eq!(ctx_pending(used, measured, &msgs, &off()), want, "{name}");
        }
    }

    // ---- chat_screenshot_test.go ----

    #[test]
    fn msg_text_signale_la_presence_d_une_image() {
        // En Rust, message vivant et message relu ont la même forme (tableau).
        let m = Message {
            role: "user".into(),
            content: Some(json!([
                {"type": "text", "text": "Capture demandée :"},
                {"type": "image_url", "image_url": {"url": "data:image/jpeg;base64,AAAA"}},
            ])),
            ..Default::default()
        };
        let got = msg_text(&m);
        assert!(got.contains("Capture demandée :"));
        assert!(got.contains(IMAGE_LOST_MARKER));
        assert!(!got.contains("base64"));
    }

    // ---- llm_injected_test.go ----

    #[test]
    fn is_loki_injected_cases() {
        let cases = [
            ("budget 1", um(&budget_nudge(24, 0)), true),
            ("budget 2", um(&budget_nudge(48, 1)), true),
            ("budget 3", um(&budget_nudge(72, 2)), true),
            ("budget suivants", um(&budget_nudge(150, 5)), true),
            (
                "consigne du 500 en message à part",
                um(&format!("{LOKI_NOTE_PREFIX}{TOOLS_OFF_HINT}")),
                true,
            ),
            (
                "demande qui commence comme un rappel",
                um(&format!("{LOKI_NOTE_PREFIX}réécris ce prompt système")),
                false,
            ),
            ("pensé sans agir", um(THINK_NUDGE_FIRST), true),
            ("pensé sans agir, bis", um(THINK_NUDGE_STUCK), true),
            (
                "appel écrit en texte",
                um(&retry_corrective("<tool_call>x")),
                true,
            ),
            ("vraie demande", um("cherche les horaires"), false),
            (
                "demande qui cite un rappel",
                um(&format!("pourquoi « {THINK_NUDGE_FIRST} » ?")),
                false,
            ),
            ("assistant", am(&format!("{LOKI_NOTE_PREFIX}x")), false),
            (
                "user multimodal",
                Message {
                    role: "user".into(),
                    content: Some(json!([{"type": "text", "text": LOKI_NOTE_PREFIX}])),
                    ..Default::default()
                },
                false,
            ),
        ];
        for (name, m, want) in cases {
            assert_eq!(is_loki_injected(&m), want, "{name}");
        }
    }

    #[test]
    fn rappels_pas_pris_pour_la_demande() {
        // Seule la partie de ce test qui relève de la compaction.
        let tr = render_transcript(&[um(THINK_NUDGE_FIRST)], 32768);
        assert!(
            tr.starts_with("System: "),
            "rappel présenté comme parole de l'utilisateur : {tr}"
        );
    }

    fn assert_alternance(msgs: &[Message]) {
        for i in 1..msgs.len() {
            assert!(
                !(msgs[i].role == "user" && msgs[i - 1].role == "user"),
                "deux user d'affilée en {i}"
            );
        }
    }

    #[tokio::test]
    async fn compaction_reinjecte_la_vraie_demande_pas_le_rappel() {
        let env = Env::new(
            CompactConfig::default(),
            FakeEngine::replying(
                "- pages lues : horaires trouvés pour le matin, reste l'après-midi",
            ),
        );
        let (torso_nudge, tail_nudge) = (budget_nudge(24, 0), budget_nudge(48, 1));
        let mut msgs = vec![
            um("première question"),
            am("ok"),
            um("cherche les horaires du train pour Lyon"),
        ];
        for i in 0..12 {
            msgs.push(atc("web_read"));
            msgs.push(page(i));
            if i == 3 {
                msgs.push(um(&torso_nudge));
            }
        }
        msgs.push(um(&tail_nudge));
        msgs.push(atc("web_read"));
        msgs.push(page(13));
        let budget = (estimate_tokens(&msgs, &off()) as f64 * COMPACT_TAIL_FRAC) as i64;
        let (_, tail_start) = compact_bounds(&msgs, budget, &off());
        assert!(
            msg_text(&msgs[msgs.len() - 3]) == tail_nudge && tail_start <= msgs.len() - 3,
            "montage : le rappel de queue n'est pas dans la queue (tailStart={tail_start})"
        );
        let (out, changed) = env.c().compact_messages(&msgs, NONE).await;
        assert!(changed, "pas compacté");
        let count = |s: &str| {
            out.iter()
                .filter(|m| m.role == "user" && msg_text(m) == s)
                .count()
        };
        assert_eq!(count("cherche les horaires du train pour Lyon"), 1);
        assert_eq!(count(&torso_nudge), 0, "le rappel du torse a été réinjecté");
        assert_eq!(count(&tail_nudge), 1, "le rappel de la queue a disparu");
        assert_alternance(&out);
    }

    #[test]
    fn compact_bounds_pas_sur_un_rappel() {
        let nudge = um(&budget_nudge(24, 0));
        let msgs = [
            um("cherche"),
            atc("web_read"),
            tm(&"x".repeat(800)),
            nudge,
            atc("web_read"),
            tm("court"),
        ];
        let budget = msg_tokens(&msgs[3]) + msg_tokens(&msgs[4]) + msg_tokens(&msgs[5]);
        let (_, tail_start) = compact_bounds(&msgs, budget, &off());
        assert_eq!(tail_start, 1, "l'appel d'outil avant le rappel");
        let plain = [
            um("cherche"),
            atc("web_read"),
            tm(&"x".repeat(800)),
            um("et ensuite"),
            atc("web_read"),
            tm("court"),
        ];
        let budget = msg_tokens(&plain[3]) + msg_tokens(&plain[4]) + msg_tokens(&plain[5]);
        assert_eq!(
            compact_bounds(&plain, budget, &off()).1,
            3,
            "frontière sans rappel déplacée"
        );
    }

    #[test]
    fn shrink_coupe_aux_vraies_demandes() {
        let cfg = win(4096);
        let big = am(&"raisonnement long ".repeat(400));
        let s = sys("SYS");
        let (out, changed) = shrink_to_fit(
            &cfg,
            &[
                s.clone(),
                um("demande A"),
                big.clone(),
                um(THINK_NUDGE_FIRST),
                big.clone(),
                um("demande B"),
                am("ok"),
            ],
            0,
        );
        assert!(
            changed && out.len() >= 2 && msg_text(&out[1]) == "demande B",
            "{out:?}"
        );
        let (out, changed) = shrink_to_fit(
            &cfg,
            &[s, um("demande A"), big.clone(), um(THINK_NUDGE_FIRST), big],
            0,
        );
        assert!(
            changed && out.len() >= 2 && msg_text(&out[1]) == THINK_NUDGE_FIRST,
            "{out:?}"
        );
    }

    // ---- llm_reasoning_echo_test.go ----

    const ECHO_TEST_MODEL: &str = "Qwen3.6-27B-Q8_0.gguf";

    fn echo_on() -> EchoPolicy {
        EchoPolicy {
            on: true,
            model: ECHO_TEST_MODEL.into(),
            keeps_past: true,
        }
    }

    #[test]
    fn echo_tokens_suit_le_rendu() {
        let r = "r".repeat(400); // 100 jetons
        let reasoned = |c: &str| Message {
            reasoning_content: r.clone(),
            reasoning_model: ECHO_TEST_MODEL.into(),
            ..am(c)
        };
        let msgs = [um("q1"), reasoned("a"), um("q2"), reasoned("b")];
        let sum = |v: Option<Vec<i64>>| v.unwrap_or_default().iter().sum::<i64>();
        assert_eq!(
            sum(echo_tokens_for(&msgs, &echo_on())),
            200,
            "garde le passé"
        );
        let no_past = EchoPolicy {
            keeps_past: false,
            ..echo_on()
        };
        assert_eq!(
            sum(echo_tokens_for(&msgs, &no_past)),
            100,
            "retire le passé"
        );
        assert_eq!(echo_tokens_for(&msgs, &off()), None, "écho coupé");
        let without = estimate_tokens(&msgs, &off());
        assert_eq!(estimate_tokens(&msgs, &echo_on()), without + 200);
    }

    #[test]
    fn shrink_retire_d_abord_le_raisonnement() {
        let cfg = CompactConfig {
            ctx_window: 8192,
            echo: echo_on(),
            ..Default::default()
        };
        let r = "r".repeat(8000); // 2000 jetons chacun
        let mut msgs = vec![um("corrige le build")];
        for _ in 0..4 {
            let mut a = atc("bash");
            a.tool_calls[0].kind = "function".into();
            a.reasoning_content = r.clone();
            a.reasoning_model = ECHO_TEST_MODEL.into();
            msgs.push(a);
            msgs.push(tm("sortie courte"));
        }
        let (out, changed) = shrink_to_fit(&cfg, &msgs, 0);
        assert!(changed, "rien retiré");
        assert_eq!(out.len(), msgs.len(), "des messages ont été retirés");
        assert!(
            out[1].reasoning_content.is_empty() && !out[out.len() - 2].reasoning_content.is_empty(),
            "ordre : le plus ancien raisonnement part en premier"
        );
        for (i, m) in out.iter().enumerate() {
            assert!(
                m.role != "tool" || msg_text(m) == "sortie courte",
                "outil {i} tronqué"
            );
        }
        assert!(
            !msgs[1].reasoning_content.is_empty(),
            "l'original a été modifié"
        );
    }

    // ---- chat_keep_images_test.go ----

    fn kept_relay(tokens: i64) -> Message {
        Message {
            role: "user".into(),
            img_relay: true,
            img_tokens: tokens,
            content: Some(json!([
                {"type": "text", "text": "Voici la capture demandée."},
                {"type": "image_url", "image_url": {"url": "img://abc.png"}},
            ])),
            ..Default::default()
        }
    }

    fn has_kept_images(msgs: &[Message]) -> bool {
        msgs.iter().any(|m| m.img_tokens > 0)
    }

    #[test]
    fn drop_kept_images_cases() {
        assert!(
            drop_kept_images(&[um("q"), tm("r")]).is_none(),
            "retrait sans image gardée"
        );
        let msgs = [
            um("q"),
            atc("see_image"),
            tm("[ok]"),
            kept_relay(500),
            am("vu"),
        ];
        let (out, freed) = drop_kept_images(&msgs).expect("image gardée");
        assert_eq!(freed, 500);
        let r = &out[3];
        assert_eq!(
            r.content,
            text_value(format!("Voici la capture demandée.{IMAGE_LOST_MARKER}"))
        );
        assert!(r.img_relay && r.img_tokens == 0, "relais retiré = {r:?}");
        assert_eq!(
            msgs[3].img_tokens, 500,
            "l'historique d'origine a été modifié"
        );
        assert!(estimate_tokens(&msgs, &off()) - estimate_tokens(&out, &off()) >= 400);
    }

    #[tokio::test]
    async fn compaction_retire_les_images_d_abord() {
        let env = Env::new(CompactConfig::default(), FakeEngine::none());
        let msgs = [
            sys("sys"),
            um("q"),
            atc("see_image"),
            tm("[ok]"),
            kept_relay(5000),
            am("vu"),
        ];
        let (out, changed, _) = env
            .c()
            .compact_messages_opt(&msgs, NONE, &CompactOpts::default())
            .await;
        assert!(
            changed && !has_kept_images(&out),
            "images non retirées par la compaction"
        );
        let (out, changed) = shrink_to_fit(&env.cfg, &msgs, 0);
        assert!(
            changed && !has_kept_images(&out),
            "images non retirées par la réduction"
        );
        let plain = [sys("sys"), um("q"), am("r")];
        let (_, changed, _) = env
            .c()
            .compact_messages_opt(&plain, NONE, &CompactOpts::default())
            .await;
        assert!(!changed, "compaction d'un fil sans torse");
    }

    #[test]
    fn relais_pas_pris_pour_la_demande() {
        let r = kept_relay(10);
        let msgs = [
            um("ancienne"),
            am("ok"),
            um("vraie demande"),
            atc("see_image"),
            tm("[ok]"),
            r.clone(),
        ];
        let p = compact_pending(&msgs, &msgs[..3], 3);
        assert!(p.len() == 1 && msg_text(&p[0]) == "vraie demande", "{p:?}");
        let back: Message = serde_json::from_str(&serde_json::to_string(&r).unwrap()).unwrap();
        assert!(
            is_loki_injected(&back),
            "relais rouvert pris pour une demande"
        );
    }

    // ---- chat_recall_test.go ----

    fn big_block(marker: &str) -> String {
        format!(
            "{marker}\n{}",
            "ligne de contenu volumineux à conserver verbatim\n".repeat(40)
        )
    }

    #[tokio::test]
    async fn compact_archive_les_gros_blocs() {
        let env = Env::new(CompactConfig::default(), FakeEngine::none());
        let mut msgs = vec![um("cherche les horaires du train pour Lyon")];
        for i in 0..10 {
            msgs.push(atc("web_read"));
            msgs.push(tm(&big_block(&format!("page numéro {}", letter(i)))));
        }
        let (out, changed) = env.c().compact_messages(&msgs, AGENT).await;
        assert!(changed, "aucune compaction");
        let joined: String = out.iter().map(|m| msg_text(m) + "\n").collect();
        assert!(
            joined.contains("recall("),
            "aucun bloc rappelable :\n{joined}"
        );
        assert!(
            !joined.contains(COMPACT_PRUNED_MARKER),
            "gros bloc effacé sans adresse"
        );
        let blocks = env.recall.blocks.lock().unwrap();
        let hit = blocks.iter().find(|(_, _, c)| c.contains("page numéro"));
        assert!(
            hit.is_some_and(|(_, _, c)| c.contains("ligne de contenu volumineux")),
            "les blocs cités ne sont pas dans l'archive"
        );
        assert!(
            blocks[0].0.starts_with("web_read: page numéro"),
            "libellé {}",
            blocks[0].0
        );
    }

    #[tokio::test]
    async fn compact_n_archive_pas_sans_agent() {
        let env = Env::new(CompactConfig::default(), FakeEngine::none());
        let mut msgs = vec![um("question")];
        for _ in 0..10 {
            msgs.push(atc("web_read"));
            msgs.push(tm(&big_block("page")));
        }
        let (out, _) = env.c().compact_messages(&msgs, NONE).await;
        assert!(out.iter().all(|m| !msg_text(m).contains("recall(")));
        assert!(
            env.recall.blocks.lock().unwrap().is_empty(),
            "archivé hors mode agent"
        );
    }

    #[test]
    fn recall_eligible_cases() {
        let gros = "x".repeat(RECALL_ARCHIVE_MIN_LEN + 1);
        assert!(recall_eligible(&tm(&gros)));
        assert!(!recall_eligible(&tm("court")));
        assert!(!recall_eligible(&sys(&gros)));
        assert!(!recall_eligible(&um(&format!(
            "{COMPACT_SUMMARY_PREFIX} {gros}"
        ))));
    }

    #[test]
    fn summary_looks_empty_cases() {
        let vrai = "L'utilisateur cherche les horaires du train pour Lyon ; le train de 14h12 part quai 3, reste à confirmer le tarif.";
        assert!(!summary_looks_empty(vrai));
        for d in [
            "",
            "   ",
            "recall:r7",
            "recall:r7 recall:r8 recall:r9",
            "voir recall(r7).",
        ] {
            assert!(summary_looks_empty(d), "résumé dégénéré accepté : {d:?}");
        }
    }

    #[test]
    fn compact_summary_user_msg_cases() {
        let sans = compact_summary_user_msg("résumé quelconque", &[]);
        assert!(sans.starts_with(COMPACT_SUMMARY_PREFIX));
        assert!(!sans.contains("recall("));
        let avec = compact_summary_user_msg(
            "résumé quelconque",
            &[RecallEntry {
                id: "r7".into(),
                label: "web_read: horaires".into(),
            }],
        );
        assert!(
            avec.contains("r7") && avec.contains("web_read: horaires"),
            "{avec}"
        );
    }

    // ---- chat_compact_cont_test.go ----

    /// Le prompt du résumeur d'avant le partage de ses règles — copie figée.
    const OLD_COMPACTOR_SYS: &str = r#"You are a context compactor. You are given the transcript of the older turns of a conversation between a user and an AI assistant (with its tools). The PURPOSE of your summary is to let the conversation continue in a fresh, smaller context WITHOUT losing any information that is useful or important to understand what came before and keep working — preserve everything that matters, drop only what is redundant.

The assistant is MID-TASK: it will read your summary and must resume exactly where it left off, WITHOUT redoing work it has already done. Its own internal reasoning is NOT part of the transcript and is lost — your summary is the only memory it keeps.

Summarize densely and faithfully, keeping ONLY the essentials:
- The user's CURRENT request, goal(s) and constraints
- FINDINGS: the concrete information already gathered — facts, figures, dates, names, URLs, file paths, values, config. This is the most important part: whatever is not here is lost and will have to be looked up again.
- Sources already consulted (URLs opened, files read, commands run) — so they are not consulted a second time
- Decisions made and established facts
- STATE OF PROGRESS: what is already answered, what is still missing, and the next concrete step
Strict rules: no preamble or conclusion, no verbatim or long quotes, no throwaway detail. Use short bullet points. Be as concise as you can WHILE keeping every fact, decision and still-open task: a detail you drop here is lost for good, so when in doubt keep it. This is a dense compression summary, not a report. Always write ACTUAL prose sentences/bullets — never answer with just an id or a reference.
Write the summary in the SAME language as the conversation."#;

    const CONT_SUMMARY: &str = "- L'utilisateur prépare un voyage à Lyon ; horaires trouvés : départ 8 h 12, arrivée 10 h 05.";

    fn cont_cfg(continuation: bool) -> CompactConfig {
        CompactConfig {
            ctx_window: 8192,
            continuation,
            engine_model: ECHO_TEST_MODEL.into(),
            ..Default::default()
        }
    }

    fn cont_history() -> Vec<Message> {
        let mut h = Vec::new();
        for i in 0..10 {
            h.push(um(&format!("question {} {}", letter(i), "u".repeat(1500))));
            h.push(am(&format!("réponse {} {}", letter(i), "r".repeat(1500))));
        }
        h
    }

    fn cont_mid_msgs() -> Vec<Message> {
        let mut m = vec![sys("SYS")];
        m.extend(cont_history());
        m
    }

    /// contViewOpts : options en cours de tour, slot tamponné pour conv-test.
    fn cont_view_opts(env: &Env, msgs: &[Message], used: i64) -> CompactOpts {
        compact_opts_mid(
            &env.cfg,
            &env.memory,
            true,
            false,
            "conv-test",
            msgs,
            used,
            Vec::new(),
            "",
            Map::new(),
            false,
            <[Message]>::to_vec,
        )
    }

    fn is_cont(c: &Call) -> bool {
        matches!(c, Call::Cont(r) if msg_text(r.messages.last().unwrap()).starts_with(COMPACT_CONT_PREFIX))
    }
    fn is_transcript(c: &Call) -> bool {
        matches!(c, Call::Transcript(r) if r.system.starts_with("You are a context compactor."))
    }

    /// Sans la clé : résumé sur transcription, prompt du résumeur identique à
    /// l'octet près, budget de la fenêtre, aucune mémoire des refus.
    /// (Partiel : le tour complet — tampon du slot, requête du tour — relève
    /// de la boucle de chat, pas de ce module.)
    #[tokio::test]
    async fn compact_cont_defaut_identique() {
        assert_eq!(
            format!("{COMPACT_TRANSCRIPT_INTRO}{COMPACT_SUMMARY_RULES}"),
            OLD_COMPACTOR_SYS
        );
        let env = Env::new(cont_cfg(false), FakeEngine::replying(CONT_SUMMARY));
        let mut msgs = cont_history();
        msgs.push(um("deuxième demande"));
        let opt = compact_opts_for(&env.cfg, &env.memory, true, &msgs, 6200, "c1", |_| {
            unreachable!("aucune vue sans la clé")
        });
        assert_eq!(opt, CompactOpts::default());
        let (_, changed) = env.c().compact_messages_noted(&msgs, AGENT, &opt).await;
        assert!(changed);
        let calls = env.engine.calls();
        assert_eq!(calls.len(), 1);
        let Call::Transcript(req) = &calls[0] else {
            panic!("transcription attendue");
        };
        assert_eq!(req.system, OLD_COMPACTOR_SYS, "prompt du résumeur changé");
        assert_eq!(req.max_tokens, 700);
        assert!(!env.cfg.continuation || !env.memory.refused_skip(&env.cfg, "c1", 7000, &[]));
        assert!(!env.memory.refused_skip(&env.cfg, "c1", 7000, &[]));
    }

    /// Avec la clé : la requête de résumé prolonge EXACTEMENT la vue d'envoi
    /// du tour, plus une seule demande de résumé ; outils, effort et arguments
    /// du gabarit repris (réflexion coupée) ; rien d'injecté n'entre dans
    /// l'historique. (Partiel : la vue est fournie ici, pas rejouée d'un tour.)
    #[tokio::test]
    async fn compact_cont_prefixe_et_historique() {
        let env = Env::new(cont_cfg(true), FakeEngine::replying("ne doit pas servir"));
        let mut msgs = cont_history();
        msgs.push(um("deuxième demande"));
        let tool = Tool {
            kind: "function".into(),
            function: ToolFunction {
                name: "bash".into(),
                description: "shell".into(),
                parameters: json!({"type": "object"}),
            },
        };
        let mut kw = Map::new();
        kw.insert("reasoning_effort".into(), json!("high"));
        let mut wire_seen = Vec::new();
        let opt = compact_opts_for(&env.cfg, &env.memory, true, &msgs, 6200, "c1", |m| {
            let mut wire = vec![sys("SYS\n\n<project_context>p</project_context>")];
            wire.extend_from_slice(m);
            wire_seen = wire.clone();
            TurnView {
                wire,
                tools: vec![tool.clone()],
                effort: "high".into(),
                kwargs: kw.clone(),
            }
        });
        let view = opt.view.as_ref().expect("vue attendue");
        assert_eq!(view.n, 20, "la vue s'arrête avant le message user de fin");
        assert!(opt.precheck);
        let (out, changed) = env.c().compact_messages_noted(&msgs, AGENT, &opt).await;
        assert!(changed);
        let calls = env.engine.calls();
        assert_eq!(calls.len(), 1, "continuation seule attendue");
        let Call::Cont(req) = &calls[0] else {
            panic!("continuation attendue");
        };
        assert_eq!(&req.messages[..wire_seen.len()], &wire_seen[..]);
        assert_eq!(req.messages.len(), wire_seen.len() + 1);
        let last = req.messages.last().unwrap();
        assert!(last.role == "user" && msg_text(last).contains("STATE OF PROGRESS"));
        assert_eq!(req.tools, vec![tool]);
        assert_eq!(req.effort, "high");
        assert_eq!(req.max_tokens, 700);
        assert_eq!(req.conv, "c1");
        assert_eq!(req.kwargs.get("enable_thinking"), Some(&json!(false)));
        assert_eq!(req.kwargs.get("reasoning_effort"), Some(&json!("high")));
        let mut found = false;
        for m in &out {
            let t = msg_text(m);
            assert!(
                m.role != "system"
                    && !t.starts_with(COMPACT_CONT_PREFIX)
                    && !t.contains("<project_context>"),
                "message injecté rangé dans l'historique"
            );
            found |= t.contains(CONT_SUMMARY);
        }
        assert!(found, "résumé absent de l'historique compacté");
    }

    #[tokio::test]
    async fn compact_cont_replis() {
        // (nom, statut, réponse de continuation, used, slot pris, continuation
        // tentée, suspendue)
        type Case = (
            &'static str,
            u16,
            Option<SummaryChoice>,
            i64,
            bool,
            bool,
            bool,
        );
        let reply = |c: SummaryChoice| Some(c);
        let cases: Vec<Case> = vec![
            ("gabarit 500", 500, None, 5000, false, true, true),
            (
                "appel d'outil",
                0,
                reply(SummaryChoice {
                    tool_calls: vec![json!({"id": "x", "type": "function"})],
                    finish_reason: "tool_calls".into(),
                    ..Default::default()
                }),
                5000,
                false,
                true,
                true,
            ),
            (
                "raisonnement seul",
                0,
                reply(SummaryChoice {
                    reasoning_content: "je réfléchis".into(),
                    finish_reason: "length".into(),
                    ..Default::default()
                }),
                5000,
                false,
                true,
                false,
            ),
            ("marge", 0, None, 7500, false, false, false),
            ("slot pris", 0, None, 5000, true, false, false),
        ];
        for (name, status, cont_reply, used, stale, want_cont, disabled) in cases {
            let mut engine = FakeEngine::replying(CONT_SUMMARY);
            engine.cont_status = status;
            engine.cont_reply = cont_reply;
            let env = Env::new(cont_cfg(true), engine);
            let msgs = cont_mid_msgs();
            let opt = cont_view_opts(&env, &msgs, used);
            assert!(opt.view.is_some(), "{name} : vue attendue");
            if stale {
                env.engine.slot.store(false, Ordering::SeqCst);
            }
            let (out, changed) = env.c().compact_messages_noted(&msgs, NONE, &opt).await;
            assert!(changed, "{name} : compaction refusée");
            assert!(
                out[0].role == "system"
                    && msg_text(&out[0]) == "SYS"
                    && msg_text(&out[1]).contains(CONT_SUMMARY),
                "{name} : sortie inattendue"
            );
            let calls = env.engine.calls();
            let cont = calls.len() == 2 && is_cont(&calls[0]) && is_transcript(&calls[1]);
            let direct = calls.len() == 1 && is_transcript(&calls[0]);
            assert!(
                if want_cont { cont } else { direct },
                "{name} : requêtes inattendues ({})",
                calls.len()
            );
            assert_eq!(
                env.memory.cont_disabled(&env.cfg),
                disabled,
                "{name} : suspension"
            );
        }
    }

    #[test]
    fn compact_cont_suspension() {
        let env = Env::new(cont_cfg(true), FakeEngine::none());
        let (cfg, mem) = (&env.cfg, &env.memory);
        mem.cont_fail(cfg, false);
        assert!(!mem.cont_disabled(cfg), "suspendue après un seul échec");
        mem.cont_ok(cfg);
        mem.cont_fail(cfg, false);
        assert!(
            !mem.cont_disabled(cfg),
            "le succès n'a pas remis le compte à zéro"
        );
        mem.cont_fail(cfg, false);
        assert!(mem.cont_disabled(cfg), "pas suspendue après deux échecs");
        let o = cont_view_opts(&env, &cont_mid_msgs(), 5000);
        assert!(
            o.view.is_none() && o.precheck,
            "vue construite pour un modèle suspendu"
        );
    }

    #[test]
    fn compact_wire_index_cases() {
        let msgs = vec![
            sys("[pages lues] pages"),
            um("a"),
            atc("bash"),
            tm("r"),
            am("b"),
            um("c"),
        ];
        // normalizeSystemMessages : systèmes fusionnés en tête, les autres un
        // pour un.
        let mut wire = vec![sys("SYS\n\n<project_context> p\n\n[pages lues] pages")];
        wire.extend_from_slice(&msgs[1..5]);
        let v = CompactView {
            wire: wire.clone(),
            n: 5,
            ..Default::default()
        };
        for (i, want) in [
            (1, Some(1)),
            (2, Some(2)),
            (3, Some(3)),
            (4, Some(4)),
            (5, Some(5)),
            (0, None),
            (6, None),
        ] {
            assert_eq!(compact_wire_index(&msgs, Some(&v), i), want, "index {i}");
        }
        let mut bad = wire.clone();
        bad[3] = am("x");
        let vb = CompactView {
            wire: bad,
            n: 5,
            ..Default::default()
        };
        assert_eq!(
            compact_wire_index(&msgs, Some(&vb), 2),
            None,
            "vue désalignée acceptée"
        );
        let vt = CompactView {
            wire: wire[..4].to_vec(),
            n: 5,
            ..Default::default()
        };
        assert_eq!(
            compact_wire_index(&msgs, Some(&vt), 2),
            None,
            "vue tronquée acceptée"
        );
        assert_eq!(compact_wire_index(&msgs, None, 2), None);
    }

    #[test]
    fn compact_boundary_desc_cases() {
        let mut call = atc("web_read");
        call.tool_calls[0].function.arguments = r#"{"url":"https://exemple.fr"}"#.into();
        let d = compact_boundary_desc(&[], 0, &call);
        assert!(
            d.contains(r#"web_read({"url":"https://exemple.fr"})"#),
            "{d}"
        );
        let long = "même début ".repeat(10) + "fin unique";
        let wire = [um(&("même début ".repeat(10) + "autre fin")), um(&long)];
        let d = compact_boundary_desc(&wire, 1, &wire[1]);
        assert!(d.contains("fin unique"), "début ambigu non allongé : {d}");
        assert_eq!(
            compact_boundary_desc(&[], 0, &um("court")),
            "the user message that starts with «court»"
        );
    }

    #[test]
    fn compact_cont_instruction_cases() {
        let s = compact_cont_instruction(3, "the user message that starts with «x»", true);
        for want in [
            COMPACT_CONT_PREFIX,
            COMPACT_SUMMARY_RULES,
            CODE_SUMMARY_RULES,
            "the last 3",
            "BEFORE the user message",
        ] {
            assert!(s.contains(want), "demande sans {want:?}");
        }
        let s = compact_cont_instruction(0, "", false);
        assert!(s.contains("whole conversation") && !s.contains("CODING"));
    }

    #[tokio::test]
    async fn compact_hopeless_cases() {
        let mut env = Env::new(cont_cfg(false), FakeEngine::replying(CONT_SUMMARY));
        let msgs = [um("a"), am("b"), um("c"), am(&"r".repeat(40000))];
        let budget = (estimate_tokens(&msgs, &off()) as f64 * COMPACT_TAIL_FRAC) as i64;
        let (head, tail_start) = compact_bounds(&msgs, budget, &off());
        assert!(
            compact_hopeless(&msgs, head, tail_start, &off()),
            "cas sans issue non reconnu"
        );
        let pre = CompactOpts {
            precheck: true,
            ..Default::default()
        };
        let (_, changed, _) = env.c().compact_messages_opt(&msgs, AGENT, &pre).await;
        assert!(!changed, "compaction acceptée");
        assert_eq!(
            env.engine.calls().len(),
            0,
            "résumé demandé malgré le pré-contrôle"
        );
        let (_, changed, refused) = env
            .c()
            .compact_messages_opt(&msgs, AGENT, &CompactOpts::default())
            .await;
        assert!(!changed && refused && env.engine.calls().len() == 1);
        // Un refus après résumé est noté.
        let noted = CompactOpts {
            conv: "c9".into(),
            used: 7000,
            ..Default::default()
        };
        let (_, changed) = env.c().compact_messages_noted(&msgs, NONE, &noted).await;
        assert!(!changed, "compaction acceptée");
        assert!(
            env.memory.refused.lock().unwrap().contains_key("c9"),
            "refus non noté"
        );
        // Le chemin réactif ne tient pas compte des refus mémorisés.
        env.cfg.continuation = true;
        let before = env.engine.calls().len();
        env.c().compact_messages(&msgs, NONE).await;
        assert_eq!(env.engine.calls().len(), before + 1);
        // Un cas qui réussit n'est jamais déclaré sans issue.
        let h = cont_history();
        let budget = (estimate_tokens(&h, &off()) as f64 * COMPACT_TAIL_FRAC) as i64;
        let (head, tail_start) = compact_bounds(&h, budget, &off());
        assert!(!compact_hopeless(&h, head, tail_start, &off()));
    }

    #[test]
    fn compact_refused_skip_cases() {
        let mut cfg = CompactConfig {
            ctx_window: 65536,
            ..Default::default()
        };
        let mem = CompactMemory::new();
        let h = cont_history();
        let mut grown = h.clone();
        grown.push(um("suite"));
        mem.refused_note(&cfg, "c1", 50000, &h);
        assert!(
            !mem.refused_skip(&cfg, "c1", 52000, &grown),
            "sans la clé, rien de sauté"
        );
        cfg.continuation = true;
        assert!(
            mem.refused_skip(&cfg, "c1", 52000, &grown)
                && !mem.refused_skip(&cfg, "c2", 52000, &grown)
        );
        assert!(!mem.refused_skip(&cfg, "c1", 59000, &grown), "sauté à 90 %");
        assert!(
            !mem.refused_skip(&cfg, "c1", 55000, &grown),
            "sauté après +10 %"
        );
        assert!(
            !mem.refused_skip(&cfg, "c1", 52000, &grown),
            "refus non oublié après +10 %"
        );
        mem.refused_note(&cfg, "c1", 50000, &h);
        assert!(
            !mem.refused_skip(&cfg, "c1", 30000, &grown)
                && !mem.refused_skip(&cfg, "c1", 50000, &grown),
            "refus non oublié quand le contexte baisse"
        );
        mem.refused_note(&cfg, "c1", 50000, &h);
        let mut edited = grown.clone();
        edited[3] = um("message modifié");
        assert!(
            !mem.refused_skip(&cfg, "c1", 51000, &edited)
                && !mem.refused_skip(&cfg, "c1", 51000, &grown),
            "refus gardé pour un historique réécrit"
        );
        mem.refused_note(&cfg, "c1", 50000, &h);
        assert!(
            !mem.refused_skip(&cfg, "c1", 51000, &h[..5]),
            "historique raccourci"
        );
        mem.refused_note(&cfg, "c1", 50000, &h);
        mem.refused_clear("c1");
        assert!(
            !mem.refused_skip(&cfg, "c1", 51000, &grown),
            "refus non effacé"
        );
    }

    // ---- compléments propres au port ----

    #[test]
    fn config_depuis_la_carte() {
        let mut m = HashMap::new();
        let c = CompactConfig::from_config(&m, off());
        assert!(c.enabled && c.ctx_window == 32768 && !c.continuation && c.engine_model == ".");
        assert_eq!(compact_summary_budget(c.ctx_window), 1310);
        m.insert("COMPACT".into(), " Non ".into());
        m.insert("CTX".into(), "8192".into());
        m.insert("COMPACT_CONTINUATION".into(), "oui".into());
        m.insert("MODEL".into(), " /models/a.gguf ".into());
        let c = CompactConfig::from_config(&m, off());
        assert!(!c.enabled && c.ctx_window == 8192 && c.continuation && c.engine_model == "a.gguf");
        m.insert("EXTERNAL".into(), "1".into());
        m.insert("CTX".into(), "-3".into());
        let c = CompactConfig::from_config(&m, off());
        assert!(c.external && !c.continuation && c.ctx_window == 32768);
    }

    #[test]
    fn transcription_tronquee_en_tete_sans_casser_l_utf8() {
        let msgs = [um(&"é".repeat(100))];
        // Fenêtre 10 → 28 octets gardés : la coupe tombe au milieu d'un « é ».
        // « User: » + 100 « é » + « \n » = 207 octets ; coupe à 179, au milieu
        // d'un « é » : son octet orphelin devient U+FFFD, comme en JSON côté Go.
        let tr = render_transcript(&msgs, 10);
        let want = format!("[…start truncated…]\n\u{FFFD}{}\n", "é".repeat(13));
        assert_eq!(tr, want);
    }

    #[test]
    fn reponse_de_resume_decodee() {
        let raw = br#"{"choices":[{"message":{"content":"ok","reasoning_content":null,"tool_calls":[{"id":"x"}]},"finish_reason":"stop"}]} parasite"#;
        let ch = parse_summary_response(raw).unwrap();
        assert_eq!(ch.content, "ok");
        assert_eq!(ch.tool_calls.len(), 1);
        assert_eq!(ch.finish_reason, "stop");
        assert_eq!(
            parse_summary_response(br#"{"choices":[]}"#),
            Err(SummaryError::Other("résumé: réponse vide".into()))
        );
        let e = SummaryError::Http {
            who: "llama-server".into(),
            status: 400,
            body: "x".into(),
        };
        assert_eq!(e.to_string(), "résumé: llama-server 400: x");
    }

    #[test]
    fn bloc_projet_retire_du_message() {
        let mut m = um("<context_update from=\"loki\" at=\"t\">\nx\n</context_update>\n\nsuite");
        assert!(
            without_ctx_update(&m).is_none(),
            "message non marqué retouché"
        );
        m.ctx_upd = true;
        let s = without_ctx_update(&m).unwrap();
        assert_eq!(msg_text(&s), "suite");
        assert!(!s.ctx_upd);
        let mut p = Message {
            role: "user".into(),
            ctx_upd: true,
            content: Some(json!([
                {"type": "text", "text": "<context_update from=\"loki\">a</context_update>\n\n"},
                {"type": "image_url", "image_url": {"url": "u"}},
            ])),
            ..Default::default()
        };
        let s = without_ctx_update(&p).unwrap();
        assert_eq!(
            s.content,
            Some(json!([{"type": "image_url", "image_url": {"url": "u"}}]))
        );
        p.role = "assistant".into();
        assert!(without_ctx_update(&p).is_none());
    }
}
