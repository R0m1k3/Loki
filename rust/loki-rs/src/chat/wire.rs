//! Ce qui part vers le moteur : messages et corps de la requête de chat.
//!
//! Port fidèle du côté « envoi » du client de chat Go :
//! - `wireMessages` / `buildChatPayload` / `chatPayloadOpts` / `turnReasoning`
//!   / `turnEchoPolicy` / `normalizeSystemMessages` / `isProjectSystem` /
//!   `prependToFirstUser` (`llm_client.go`) ;
//! - `stripRelayTags` (`chat_keep_images.go`), `withTrailingHint` et
//!   `toolsOffHint` (`llm_injected.go`), `expandImageRefs` / `contentParts` /
//!   `partImageURL` (`chat_images.go`) ;
//! - `llm_reasoning_echo.go` (REASONING_ECHO : politique, point de sortie,
//!   comptage, mémoire des refus, erreur de gabarit) ;
//! - `llm_effort.go` (repli de `reasoning_effort` refusé par le gabarit) et
//!   `reasoningEffortValue` / `reasoningTemplateKwargs` /
//!   `reasoningExplicitlyOff` (`backend_models.go`) ;
//! - `applySamplingFrom` (`backend_config.go`) ;
//! - `chatEndpoint` / `completionsURL` / `resolveChatEndpoint`
//!   (`backend_external.go`).
//!
//! État global Go devenu paramètre explicite :
//! - `ReadConfig()` → `cfg: &HashMap<String, String>` (une clé absente vaut "",
//!   comme une map Go) ;
//! - `readAPIKey()` (clé locale rangée en base) → `local_key` ;
//! - `effortFallbacks` (sync.Map) → [`EffortFallbacks`], tenu par l'appelant ;
//! - `echoRefused` (sync.Map) → [`EchoRefusals`], tenu par l'appelant ;
//! - `tplCapsCurrent()` (dernier verdict de la sonde de gabarit) →
//!   `Option<&TplVerdict>` ;
//! - `chatImageDataURL` (disque + cache + mémoire chiffrée) → fermeture
//!   `image_data_url(ref) -> Option<data-URL>`.
//!
//! Le corps est rendu en [`serde_json::Value`] : ses clés sont triées comme
//! celles d'une `map[string]any` Go marshalée.

use std::borrow::Cow;
use std::collections::{HashMap, HashSet};
use std::sync::Mutex;

use serde_json::{Map, Value, json};

use super::types::{Message, ReasoningEcho, Tool};
use crate::engstatus::is_external;

// ---------------------------------------------------------------------------
// Constantes partagées avec le Go
// ---------------------------------------------------------------------------

/// `extKeyURL` : base ou URL complète des complétions d'un preset externe.
pub const EXT_KEY_URL: &str = "EXTERNAL_URL";
/// `extKeyModel` : nom du modèle envoyé dans le payload.
pub const EXT_KEY_MODEL: &str = "EXTERNAL_MODEL";
/// `extKeyToken` : clé API (Bearer), peut être vide.
pub const EXT_KEY_TOKEN: &str = "EXTERNAL_KEY";

/// `lokiNotePrefix` : préfixe des consignes posées par Loki.
pub const LOKI_NOTE_PREFIX: &str = "[system] ";

/// `toolsOffHint` : consigne de la relance après un 500 (appel d'outil que
/// llama.cpp n'a pas su parser). Jamais persistée.
pub const TOOLS_OFF_HINT: &str = "Do not call any more tools. Answer now, directly, in the user's language, using only the information already gathered.";

/// `imgRefScheme` : adresse d'une image rangée sur disque (chat_images.go).
pub const IMG_REF_SCHEME: &str = "loki-img:";

/// Préfixes des messages système propres au projet ou à la conversation
/// (`isProjectSystem`).
pub const PROJECT_CONTEXT_PREFIX: &str = "Project context";
pub const MEM_INDEX_PREFIX: &str = "Project memory index";
pub const TRACKER_INDEX_PREFIX: &str = "Trackers (tracker tool)";
pub const MEM_REMINDER_PREFIX: &str = "[MEMORY PAGES REMINDER]";
pub const CODE_INSTRUCTIONS_PREFIX: &str = "Repository instructions";
pub const ENV_CONTEXT_PREFIX: &str = "Environment (from Loki)";

/// Lecture d'une clé de configuration comme une map Go : absente = "".
fn cfg_str<'a>(cfg: &'a HashMap<String, String>, k: &str) -> &'a str {
    cfg.get(k).map(String::as_str).unwrap_or("")
}

/// `filepath.Base` du Go, séparateurs de la plateforme (`/`, plus `\` et le
/// volume `X:` sous Windows). Un chemin UNC n'est pas reconnu comme volume.
fn go_filepath_base(path: &str) -> String {
    if path.is_empty() {
        return ".".into();
    }
    let is_sep = |c: char| c == '/' || (cfg!(windows) && c == '\\');
    let mut p = path.trim_end_matches(is_sep);
    if cfg!(windows) {
        let b = p.as_bytes();
        if b.len() >= 2 && b[1] == b':' && b[0].is_ascii_alphabetic() {
            p = &p[2..];
        }
    }
    let p = match p.rfind(is_sep) {
        Some(i) => &p[i + 1..],
        None => p,
    };
    if p.is_empty() {
        return std::path::MAIN_SEPARATOR.to_string();
    }
    p.to_string()
}

/// Enchaîne une étape « copie seulement si besoin » sur une tranche qui est
/// peut-être déjà une copie : sans changement, la copie existante est gardée.
fn then<'a>(
    c: Cow<'a, [Message]>,
    f: impl for<'b> FnOnce(&'b [Message]) -> Cow<'b, [Message]>,
) -> Cow<'a, [Message]> {
    match c {
        Cow::Borrowed(b) => f(b),
        Cow::Owned(v) => {
            let changed = match f(&v) {
                Cow::Borrowed(_) => None,
                Cow::Owned(w) => Some(w),
            };
            Cow::Owned(changed.unwrap_or(v))
        }
    }
}

// ---------------------------------------------------------------------------
// Messages envoyés (wireMessages)
// ---------------------------------------------------------------------------

/// `wireMessages` : les messages tels qu'ils partent au moteur — un seul
/// système en tête, la consigne de fin si les outils sont neutralisés, le
/// raisonnement renvoyé selon `pol` — et le nombre de raisonnements renvoyés.
/// Sans rien à changer, la tranche d'origine ressort empruntée (aucune copie).
pub fn wire_messages<'a>(
    messages: &'a [Message],
    tool_choice_none: bool,
    pol: &EchoPolicy,
) -> (Cow<'a, [Message]>, usize) {
    let sent = normalize_system_messages(messages);
    // Marque de relais d'image (KEEP_TURN_IMAGES) : propre à Loki, jamais au
    // moteur. Sans la clé, aucune : la tranche repart telle quelle.
    let mut sent = then(sent, strip_relay_tags);
    if tool_choice_none {
        sent = Cow::Owned(with_trailing_hint(&sent, TOOLS_OFF_HINT));
    }
    match sent {
        Cow::Borrowed(b) => echo_messages(b, pol),
        Cow::Owned(v) => {
            let (r, n) = echo_messages(&v, pol);
            let changed = match r {
                Cow::Borrowed(_) => None,
                Cow::Owned(w) => Some(w),
            };
            (Cow::Owned(changed.unwrap_or(v)), n)
        }
    }
}

/// `normalizeSystemMessages` : un seul message système, en tête, fusion des
/// systèmes textuels dans l'ordre ; les systèmes propres au projet partent en
/// tête du premier message utilisateur (bloc `<project_context>`). Aucun
/// système : la tranche ressort telle quelle. L'entrée n'est jamais modifiée.
pub fn normalize_system_messages(msgs: &[Message]) -> Cow<'_, [Message]> {
    let mut sys: Vec<&str> = Vec::new();
    let mut proj: Vec<&str> = Vec::new();
    let mut saw_system = false;
    let mut rest: Vec<&Message> = Vec::with_capacity(msgs.len());
    for m in msgs {
        if m.role == "system"
            && let Some(Value::String(s)) = &m.content
        {
            saw_system = true;
            if s.trim().is_empty() {
                continue;
            }
            if is_project_system(m) {
                proj.push(s);
            } else {
                sys.push(s);
            }
            continue;
        }
        // Contenu système non textuel (cas théorique) : préservé tel quel.
        rest.push(m);
    }
    if !saw_system {
        return Cow::Borrowed(msgs);
    }
    let mut rest: Vec<Message> = rest.into_iter().cloned().collect();
    if !proj.is_empty() && !prepend_to_first_user(&mut rest, &proj.join("\n\n")) {
        // Pas encore de message user (cas théorique) : gardés en système.
        sys.extend(proj);
    }
    if sys.is_empty() {
        return Cow::Owned(rest);
    }
    let mut out = Vec::with_capacity(rest.len() + 1);
    out.push(Message {
        role: "system".into(),
        content: Some(Value::String(sys.join("\n\n"))),
        ..Default::default()
    });
    out.extend(rest);
    Cow::Owned(out)
}

/// `isProjectSystem` : message système propre au projet actif ou à la
/// conversation, à sortir du bloc système commun.
pub fn is_project_system(m: &Message) -> bool {
    let Some(Value::String(s)) = &m.content else {
        return false;
    };
    if m.role != "system" {
        return false;
    }
    [
        PROJECT_CONTEXT_PREFIX,
        MEM_INDEX_PREFIX,
        TRACKER_INDEX_PREFIX,
        MEM_REMINDER_PREFIX,
        CODE_INSTRUCTIONS_PREFIX,
        ENV_CONTEXT_PREFIX,
    ]
    .iter()
    .any(|p| s.starts_with(p))
}

/// `prependToFirstUser` : place `ctx` en tête du premier message user de
/// `msgs` (déjà une copie). `false` s'il n'y a aucun message user, ou si le
/// premier n'a ni texte ni parties.
pub fn prepend_to_first_user(msgs: &mut [Message], ctx: &str) -> bool {
    let block = format!("<project_context>\n{ctx}\n</project_context>\n\n");
    let Some(m) = msgs.iter_mut().find(|m| m.role == "user") else {
        return false;
    };
    match &mut m.content {
        Some(Value::String(c)) => {
            c.insert_str(0, &block);
            true
        }
        Some(Value::Array(parts)) => {
            parts.insert(0, json!({"type": "text", "text": block}));
            true
        }
        _ => false,
    }
}

/// `stripRelayTags` : retire les marques ImgRelay / CtxUpd, propres à Loki.
/// Aucune marque : la tranche ressort telle quelle, sans copie.
pub fn strip_relay_tags(msgs: &[Message]) -> Cow<'_, [Message]> {
    if !msgs.iter().any(|m| m.img_relay || m.ctx_upd) {
        return Cow::Borrowed(msgs);
    }
    let mut out = msgs.to_vec();
    for m in &mut out {
        m.img_relay = false;
        m.ctx_upd = false;
    }
    Cow::Owned(out)
}

/// `withTrailingHint` : COPIE de `msgs` dont le dernier message (résultat
/// d'outil ou message user) porte en plus la consigne, à la fin ; sinon un
/// message user à part.
pub fn with_trailing_hint(msgs: &[Message], hint: &str) -> Vec<Message> {
    let note = format!("\n\n{LOKI_NOTE_PREFIX}{hint}");
    let mut out = msgs.to_vec();
    if let Some(last) = out.last_mut()
        && (last.role == "tool" || last.role == "user")
    {
        match &mut last.content {
            Some(Value::String(c)) => {
                c.push_str(&note);
                return out;
            }
            Some(Value::Array(parts)) => {
                parts.push(json!({"type": "text", "text": note}));
                return out;
            }
            _ => {}
        }
    }
    // Dernier message d'une autre forme (cas théorique) : un message à part.
    out.push(Message {
        role: "user".into(),
        content: Some(Value::String(format!("{LOKI_NOTE_PREFIX}{hint}"))),
        ..Default::default()
    });
    out
}

// ---------------------------------------------------------------------------
// Images par référence (expandImageRefs)
// ---------------------------------------------------------------------------

/// `contentParts` : les parties (objets) d'un contenu multimodal ; aucune pour
/// un texte ou un contenu absent.
pub fn content_parts(content: Option<&Value>) -> Vec<&Map<String, Value>> {
    match content {
        Some(Value::Array(a)) => a.iter().filter_map(Value::as_object).collect(),
        _ => Vec::new(),
    }
}

/// `partImageURL` : l'URL d'une partie `image_url` ("" sinon).
pub fn part_image_url(p: &Map<String, Value>) -> &str {
    if p.get("type").and_then(Value::as_str) != Some("image_url") {
        return "";
    }
    p.get("image_url")
        .and_then(Value::as_object)
        .and_then(|iu| iu.get("url"))
        .and_then(Value::as_str)
        .unwrap_or("")
}

/// `expandImageRefs` : COPIE des messages prête pour le modèle — chaque
/// référence `loki-img:` redevient sa data-URL (`image_data_url`, qui tient
/// lieu de `chatImageDataURL` : disque, cache, déchiffrement). Les messages
/// sans référence sont repris tels quels ; aucune référence : pas de copie.
/// Une image introuvable devient une mention textuelle.
pub fn expand_image_refs<'a>(
    msgs: &'a [Message],
    image_data_url: &dyn Fn(&str) -> Option<String>,
) -> Cow<'a, [Message]> {
    let mut out: Option<Vec<Message>> = None;
    for (i, m) in msgs.iter().enumerate() {
        let parts = content_parts(m.content.as_ref());
        if !parts
            .iter()
            .any(|p| part_image_url(p).starts_with(IMG_REF_SCHEME))
        {
            continue;
        }
        let mut np: Vec<Value> = Vec::with_capacity(parts.len());
        for p in parts {
            let u = part_image_url(p);
            if !u.starts_with(IMG_REF_SCHEME) {
                np.push(Value::Object(p.clone()));
                continue;
            }
            match image_data_url(u) {
                Some(d) => np.push(json!({"type": "image_url", "image_url": {"url": d}})),
                None => np.push(
                    json!({"type": "text", "text": "[image indisponible : fichier supprimé]"}),
                ),
            }
        }
        let o = out.get_or_insert_with(|| msgs.to_vec());
        o[i].content = Some(Value::Array(np));
    }
    match out {
        Some(v) => Cow::Owned(v),
        None => Cow::Borrowed(msgs),
    }
}

// ---------------------------------------------------------------------------
// REASONING_ECHO (llm_reasoning_echo.go)
// ---------------------------------------------------------------------------

/// `reasoningEchoEnabled` : la clé REASONING_ECHO demande-t-elle l'écho ?
pub fn reasoning_echo_enabled(cfg: &HashMap<String, String>) -> bool {
    matches!(
        cfg_str(cfg, "REASONING_ECHO")
            .trim()
            .to_lowercase()
            .as_str(),
        "on" | "1" | "true" | "yes" | "oui"
    )
}

/// `reasoningEchoModel` : l'étiquette du modèle chargé (nom du fichier, pas
/// le chemin). Vide = inconnu, rien ne part.
pub fn reasoning_echo_model(cfg: &HashMap<String, String>) -> String {
    let m = cfg_str(cfg, "MODEL").trim();
    if m.is_empty() {
        return String::new();
    }
    go_filepath_base(m)
}

/// `echoRefused` : modèles dont le gabarit a refusé un raisonnement renvoyé,
/// pour toute la vie du processus. Tenu par l'appelant (état global en Go).
#[derive(Debug, Default)]
pub struct EchoRefusals {
    set: Mutex<HashSet<String>>,
}

impl EchoRefusals {
    pub fn new() -> Self {
        Self::default()
    }

    /// `echoRefuse` : retient le refus ; la première fois, le dit sur stderr.
    pub fn refuse(&self, model: &str) {
        if model.is_empty() {
            return;
        }
        let new = self
            .set
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .insert(model.to_string());
        if new {
            eprintln!(
                "[reasoning] gabarit de {model} : raisonnement renvoyé refusé — REASONING_ECHO suspendu pour ce modèle jusqu'au redémarrage"
            );
        }
    }

    /// `echoIsRefused`.
    pub fn is_refused(&self, model: &str) -> bool {
        self.set
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .contains(model)
    }
}

/// `tplTri` : verdict à trois valeurs de la sonde de gabarit.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum TplTri {
    Yes,
    No,
    #[default]
    Unknown,
}

/// Ce que `echoPolicyFor` lit du dernier verdict de la sonde
/// (`tplProbeResult`, chat_tplprobe.go) — le sous-ensemble utile ici.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct TplVerdict {
    pub model: String,
    pub renders_reasoning: TplTri,
    pub preserves_history: TplTri,
}

/// `echoPolicy` : ce qui part au moteur pour cette requête.
/// - `on` : renvoyer le raisonnement ;
/// - `model` : seul le raisonnement étiqueté de ce modèle repart ;
/// - `keeps_past` : le gabarit rend aussi le raisonnement d'avant la dernière
///   question (ne sert qu'au comptage).
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct EchoPolicy {
    pub on: bool,
    pub model: String,
    pub keeps_past: bool,
}

/// `echoPolicyFor` : la politique d'après la configuration, la mémoire des
/// refus et le dernier verdict de la sonde (`tplCapsCurrent`, `None` = aucun ;
/// pour un preset externe l'écho est de toute façon coupé avant).
pub fn echo_policy_for(
    cfg: &HashMap<String, String>,
    refusals: &EchoRefusals,
    verdict: Option<&TplVerdict>,
) -> EchoPolicy {
    let mut p = EchoPolicy {
        model: reasoning_echo_model(cfg),
        keeps_past: true,
        on: false,
    };
    if !reasoning_echo_enabled(cfg)
        || is_external(cfg)
        || p.model.is_empty()
        || refusals.is_refused(&p.model)
    {
        return EchoPolicy::default();
    }
    p.on = true;
    if let Some(r) = verdict
        && r.model == p.model
    {
        if r.renders_reasoning == TplTri::No {
            p.on = false;
        }
        if r.preserves_history == TplTri::No {
            p.keeps_past = false;
        }
    }
    p
}

/// `turnEchoPolicy` : off = preset externe ou repli en cours, rien ne repart ;
/// sinon `current()` (`currentEchoPolicy`), lu seulement dans ce cas.
pub fn turn_echo_policy(off: bool, current: impl FnOnce() -> EchoPolicy) -> EchoPolicy {
    if off {
        return EchoPolicy::default();
    }
    current()
}

/// `hasBody` : le message porte du texte ou des appels d'outils.
pub fn has_body(m: &Message) -> bool {
    if !m.tool_calls.is_empty() {
        return true;
    }
    match &m.content {
        None | Some(Value::Null) => false,
        Some(Value::String(s)) => !s.is_empty(),
        Some(_) => true,
    }
}

/// `echoable` : ce raisonnement-là peut-il partir sous cette politique ?
pub fn echoable(m: &Message, p: &EchoPolicy) -> bool {
    p.on && m.role == "assistant"
        && !m.reasoning_content.is_empty()
        && m.reasoning_model == p.model
        && has_body(m)
}

/// `echoMessages` : LE point de sortie du raisonnement. Aucun message n'en
/// porte : la tranche ressort telle quelle, sans copie. Sinon, copie :
/// l'étiquette du modèle est toujours retirée, le raisonnement l'est quand il
/// ne doit pas partir. Rend aussi le nombre de raisonnements renvoyés.
pub fn echo_messages<'a>(msgs: &'a [Message], p: &EchoPolicy) -> (Cow<'a, [Message]>, usize) {
    if !msgs
        .iter()
        .any(|m| !m.reasoning_content.is_empty() || !m.reasoning_model.is_empty())
    {
        return (Cow::Borrowed(msgs), 0);
    }
    let mut n = 0;
    let out = msgs
        .iter()
        .map(|m| {
            let keep = echoable(m, p);
            let mut m = m.clone();
            m.reasoning_model.clear();
            if keep {
                n += 1;
            } else {
                m.reasoning_content.clear();
            }
            m
        })
        .collect();
    (Cow::Owned(out), n)
}

/// `echoTokens` : jetons que le raisonnement renvoyé ajoute au prompt, message
/// par message (`None` = aucun). `current` (`currentEchoPolicy`) n'est appelé
/// que si un message porte du raisonnement.
pub fn echo_tokens(msgs: &[Message], current: impl FnOnce() -> EchoPolicy) -> Option<Vec<usize>> {
    if !msgs.iter().any(|m| !m.reasoning_content.is_empty()) {
        return None;
    }
    echo_tokens_for(msgs, &current())
}

/// `echoTokensFor` : après la dernière question toujours ; avant elle
/// seulement si le gabarit conserve le passé (ou qu'on l'ignore).
pub fn echo_tokens_for(msgs: &[Message], p: &EchoPolicy) -> Option<Vec<usize>> {
    if !p.on {
        return None;
    }
    let last_user = msgs.iter().rposition(|m| m.role == "user");
    let mut out: Option<Vec<usize>> = None;
    for (i, m) in msgs.iter().enumerate() {
        let before_last = last_user.is_some_and(|u| i < u);
        if !echoable(m, p) || (before_last && !p.keeps_past) {
            continue;
        }
        out.get_or_insert_with(|| vec![0; msgs.len()])[i] = m.reasoning_content.len() / 4;
    }
    out
}

/// `stripReasoning` : copie des messages sans aucun raisonnement (étiquette
/// comprise) — ce que Loki envoyait avant REASONING_ECHO.
pub fn strip_reasoning(msgs: &[Message]) -> Cow<'_, [Message]> {
    echo_messages(msgs, &EchoPolicy::default()).0
}

/// `echoTemplateError` : le moteur a-t-il refusé la requête à cause du gabarit,
/// d'une façon que le raisonnement renvoyé peut expliquer ?
pub fn echo_template_error(status: u16, body: &str) -> bool {
    if status != 500 && status != 400 {
        return false;
    }
    if effort_rejection(body).is_some() {
        return false;
    }
    let low = body.to_lowercase();
    if low.contains("failed to parse input") {
        return false;
    }
    if [
        "raise_exception",
        "expected 'content' or 'tool_calls'",
        "failed to parse messages",
    ]
    .iter()
    .any(|k| low.contains(k))
    {
        return true;
    }
    low.contains("thinking") && (low.contains("jinja") || low.contains("template"))
}

/// `withEcho` : pose le raisonnement final sur le message de réponse.
pub fn with_echo(mut m: Message, e: Option<&ReasoningEcho>) -> Message {
    if let Some(e) = e
        && !e.text.is_empty()
        && has_body(&m)
    {
        m.reasoning_content = e.text.clone();
        m.reasoning_model = e.model.clone();
    }
    m
}

// ---------------------------------------------------------------------------
// Intensité du raisonnement (backend_models.go, llm_effort.go)
// ---------------------------------------------------------------------------

/// `reasoningActive`.
pub fn reasoning_active(v: &str) -> bool {
    !matches!(
        v.trim().to_lowercase().as_str(),
        "" | "off" | "none" | "false" | "0" | "no" | "disable" | "disabled"
    )
}

/// `reasoningExplicitlyOff` : REASONING interdit-il le raisonnement ? Absente
/// ou vide = pas de consigne.
pub fn reasoning_explicitly_off(v: &str) -> bool {
    !v.trim().is_empty() && !reasoning_active(v)
}

/// `reasoningEffortValue` : REASONING_EFFORT normalisé ("" = ne rien envoyer).
pub fn reasoning_effort_value(v: &str) -> String {
    let s = v.trim().to_lowercase();
    match s.as_str() {
        "" | "auto" | "default" => String::new(),
        "off" | "false" | "0" | "no" | "disable" | "disabled" | "none" => "none".into(),
        // low / medium / high, et toute valeur inconnue : passée telle quelle.
        _ => s,
    }
}

/// `reasoningTemplateKwargs` : les `chat_template_kwargs` qui font que
/// « aucune » coupe VRAIMENT le raisonnement. `None` = rien à imposer.
pub fn reasoning_template_kwargs(off: bool, effort: &str) -> Option<Map<String, Value>> {
    let mut m = Map::new();
    if off || effort == "none" {
        m.insert("enable_thinking".into(), Value::Bool(false));
        return Some(m);
    }
    if !effort.is_empty() {
        m.insert("reasoning_effort".into(), Value::String(effort.into()));
        return Some(m);
    }
    None
}

/// `effortLadder` : niveaux connus, du plus faible au plus fort.
pub const EFFORT_LADDER: [&str; 6] = ["none", "minimal", "low", "medium", "high", "xhigh"];

/// `effortRank` : position sur l'échelle, -1 si inconnu.
pub fn effort_rank(v: &str) -> i32 {
    let v = v.trim().to_lowercase();
    EFFORT_LADDER
        .iter()
        .position(|s| *s == v)
        .map_or(-1, |i| i as i32)
}

/// `effortWords` (`\b(none|minimal|low|medium|high|xhigh)\b`, bornes de mot
/// ASCII comme RE2) : les niveaux cités, dans l'ordre, sans chevauchement.
fn effort_words(s: &str) -> Vec<&'static str> {
    let b = s.as_bytes();
    let is_w = |c: u8| c.is_ascii_alphanumeric() || c == b'_';
    let mut out = Vec::new();
    let mut i = 0;
    while i < b.len() {
        if i == 0 || !is_w(b[i - 1]) {
            let hit = EFFORT_LADDER.iter().find(|w| {
                b[i..].starts_with(w.as_bytes()) && b.get(i + w.len()).is_none_or(|&c| !is_w(c))
            });
            if let Some(w) = hit {
                out.push(*w);
                i += w.len();
                continue;
            }
        }
        i += 1;
    }
    out
}

/// `effortRejection` : le corps d'erreur est-il un refus de la valeur de
/// `reasoning_effort` par le gabarit ? `Some(niveaux acceptés)` (liste
/// éventuellement vide) si oui, `None` sinon.
pub fn effort_rejection(body: &str) -> Option<Vec<String>> {
    let low = body.to_lowercase();
    if !low.contains("reasoning effort") && !low.contains("reasoning_effort") {
        return None;
    }
    if ![
        "unexpected",
        "unsupported",
        "not supported",
        "invalid",
        "unknown",
    ]
    .iter()
    .any(|w| low.contains(w))
    {
        return None;
    }
    let mut supported: Vec<String> = Vec::new();
    // Les niveaux acceptés sont annoncés APRÈS « supported ».
    if let Some(i) = low.rfind("supported") {
        for m in effort_words(&low[i..]) {
            if effort_rank(m) >= 0 && !supported.iter().any(|s| s == m) {
                supported.push(m.to_string());
            }
        }
    }
    Some(supported)
}

/// `nearestEffort` : le niveau accepté le plus proche du niveau demandé ("" =
/// retirer le champ). À égalité, le plus fort ; « none » jamais traduit.
pub fn nearest_effort(want: &str, supported: &[String]) -> String {
    let want = want.trim().to_lowercase();
    if want.is_empty() || want == "none" {
        return String::new();
    }
    let rank = effort_rank(&want);
    if rank < 0 {
        return String::new();
    }
    let mut best = "";
    let mut best_rank = -1;
    for s in supported {
        let r = effort_rank(s);
        if r < 0 || *s == want {
            continue;
        }
        let (d, bd) = ((r - rank).abs(), (best_rank - rank).abs());
        if best.is_empty() || d < bd || (d == bd && r > best_rank) {
            best = s.as_str();
            best_rank = r;
        }
    }
    best.to_string()
}

/// `effortFromRejection` : `Some(niveau à réessayer)` ("" = ne rien envoyer)
/// si le refus est reconnu.
pub fn effort_from_rejection(body: &str, want: &str) -> Option<String> {
    effort_rejection(body).map(|sup| nearest_effort(want, &sup))
}

/// `effortFallbacks` : traductions apprises, par modèle (fichier de MODEL) et
/// niveau demandé. La valeur "" est significative (= ne rien envoyer). Tenu
/// par l'appelant (état global en Go) ; le modèle est lu dans `cfg`.
#[derive(Debug, Default)]
pub struct EffortFallbacks {
    map: Mutex<HashMap<String, String>>,
}

impl EffortFallbacks {
    pub fn new() -> Self {
        Self::default()
    }

    /// `effortKey`.
    fn key(cfg: &HashMap<String, String>, want: &str) -> String {
        format!(
            "{}\x00{want}",
            go_filepath_base(cfg_str(cfg, "MODEL").trim())
        )
    }

    /// `effortResolve` : le niveau réellement envoyable pour `want`.
    pub fn resolve(&self, cfg: &HashMap<String, String>, want: &str) -> String {
        if want.is_empty() {
            return String::new();
        }
        let map = self.map.lock().unwrap_or_else(|e| e.into_inner());
        match map.get(&Self::key(cfg, want)) {
            Some(v) => v.clone(),
            None => want.to_string(),
        }
    }

    /// `effortRemember` : enregistre la traduction pour les messages suivants.
    pub fn remember(&self, cfg: &HashMap<String, String>, want: &str, got: &str) {
        if want.is_empty() {
            return;
        }
        self.map
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .insert(Self::key(cfg, want), got.to_string());
    }
}

/// `logEffortFallback` : trace le repli sur stderr.
pub fn log_effort_fallback(want: &str, got: &str) {
    if got.is_empty() {
        eprintln!(
            "[reasoning] gabarit du modèle : intensité {want:?} refusée — champ retiré pour ce modèle"
        );
        return;
    }
    eprintln!(
        "[reasoning] gabarit du modèle : intensité {want:?} refusée — repli sur {got:?} pour ce modèle"
    );
}

/// Résultat de `turnReasoning` (quadruplet en Go).
#[derive(Debug, Clone, PartialEq, Default)]
pub struct TurnReasoning {
    /// Niveau demandé par la configuration (`effortWanted`).
    pub effort_wanted: String,
    /// Niveau envoyé (traduction apprise comprise).
    pub effort: String,
    pub think_off: bool,
    pub kwargs: Option<Map<String, Value>>,
}

/// `turnReasoning` : ce que la requête dit du raisonnement, tiré de `cfg` et
/// des traductions apprises.
pub fn turn_reasoning(cfg: &HashMap<String, String>, fallbacks: &EffortFallbacks) -> TurnReasoning {
    let effort_wanted = reasoning_effort_value(cfg_str(cfg, "REASONING_EFFORT"));
    let effort = fallbacks.resolve(cfg, &effort_wanted);
    let think_off = reasoning_explicitly_off(cfg_str(cfg, "REASONING")) || effort_wanted == "none";
    let kwargs = reasoning_template_kwargs(think_off, &effort);
    TurnReasoning {
        effort_wanted,
        effort,
        think_off,
        kwargs,
    }
}

// ---------------------------------------------------------------------------
// Échantillonnage du preset (applySamplingFrom)
// ---------------------------------------------------------------------------

/// `samplingKeys` : clé de config → champ du corps (entier ou flottant).
const SAMPLING_KEYS: [(&str, &str, bool); 6] = [
    ("TEMP", "temperature", false),
    ("TOP_P", "top_p", false),
    ("TOP_K", "top_k", true),
    ("MIN_P", "min_p", false),
    ("PRESENCE_PENALTY", "presence_penalty", false),
    ("REPEAT_PENALTY", "repeat_penalty", false),
];

/// `applySamplingFrom` : superpose l'échantillonnage du preset au corps. Clé
/// vide ou illisible : ignorée ; TEMP l'emporte sur la température posée.
/// Écart : un flottant non fini (« inf », « nan ») est ignoré — en Go il
/// faisait échouer le marshal de toute la requête.
pub fn apply_sampling_from(payload: &mut Map<String, Value>, cfg: &HashMap<String, String>) {
    for (key, field, integer) in SAMPLING_KEYS {
        let v = cfg_str(cfg, key).trim();
        if v.is_empty() {
            continue;
        }
        if integer {
            if let Ok(n) = v.parse::<i64>() {
                payload.insert(field.into(), json!(n));
            }
            continue;
        }
        if let Ok(f) = v.parse::<f64>()
            && f.is_finite()
        {
            payload.insert(field.into(), json!(f));
        }
    }
}

// ---------------------------------------------------------------------------
// Endpoint (backend_external.go)
// ---------------------------------------------------------------------------

/// `chatEndpoint` : où partent les appels /v1/chat/completions de ce tour.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct ChatEndpoint {
    /// URL complète des complétions.
    pub url: String,
    /// Nom de modèle envoyé dans le payload.
    pub model: String,
    /// Bearer ("" = aucun).
    pub key: String,
    /// API distante (pas le llama-server local).
    pub external: bool,
}

impl ChatEndpoint {
    /// `auth` : pose l'en-tête d'autorisation de CET endpoint (jamais la clé
    /// locale vers un tiers).
    pub fn auth(&self, mut set: impl FnMut(&str, &str)) {
        if !self.key.is_empty() {
            set("Authorization", &format!("Bearer {}", self.key));
        }
    }
}

/// `completionsURL` : normalise l'URL saisie en URL de complétions complète.
pub fn completions_url(raw: &str) -> String {
    let u = raw.trim().trim_end_matches('/');
    if u.is_empty() {
        return String::new();
    }
    if u.ends_with("/chat/completions") {
        return u.to_string();
    }
    if u.ends_with("/v1") {
        return format!("{u}/chat/completions");
    }
    format!("{u}/v1/chat/completions")
}

/// `LLMPort` tel quel (`strconv.Atoi`, > 0, sinon 8080), sans borne haute.
fn llm_port_go(cfg: &HashMap<String, String>) -> i64 {
    match cfg.get("PORT").map(|p| p.parse::<i64>()) {
        Some(Ok(n)) if n > 0 => n,
        _ => 8080,
    }
}

/// `resolveChatEndpoint` : l'API externe si `cfg` décrit un preset externe,
/// sinon le llama-server local (port de `cfg`, clé `local_key` — `readAPIKey`).
pub fn resolve_chat_endpoint(cfg: &HashMap<String, String>, local_key: &str) -> ChatEndpoint {
    if is_external(cfg) {
        return ChatEndpoint {
            url: completions_url(cfg_str(cfg, EXT_KEY_URL)),
            model: cfg_str(cfg, EXT_KEY_MODEL).trim().to_string(),
            key: cfg_str(cfg, EXT_KEY_TOKEN).trim().to_string(),
            external: true,
        };
    }
    ChatEndpoint {
        url: format!("http://localhost:{}/v1/chat/completions", llm_port_go(cfg)),
        model: "loki".into(),
        key: local_key.to_string(),
        external: false,
    }
}

// ---------------------------------------------------------------------------
// Corps de la requête (buildChatPayload)
// ---------------------------------------------------------------------------

/// `chatPayloadOpts` : ce qui, en plus des messages, façonne une requête.
#[derive(Debug, Clone, Default)]
pub struct ChatPayloadOpts<'a> {
    pub tools: &'a [Tool],
    pub disable_tools: bool,
    pub tool_choice_none: bool,
    pub effort: &'a str,
    pub kwargs: Option<&'a Map<String, Value>>,
    /// Sans l'échantillonnage du preset (résumé en continuation).
    pub no_sampling: bool,
}

/// `buildChatPayload` : le corps d'une requête de complétion de chat, à partir
/// des messages déjà passés par [`wire_messages`]. `cfg` remplace le
/// `ReadConfig()` d'`applySampling` ; `image_data_url` celui de
/// `chatImageDataURL` (voir [`expand_image_refs`]).
pub fn build_chat_payload(
    ep: &ChatEndpoint,
    sent: &[Message],
    temperature: f64,
    o: &ChatPayloadOpts<'_>,
    cfg: &HashMap<String, String>,
    image_data_url: &dyn Fn(&str) -> Option<String>,
) -> Value {
    let messages = expand_image_refs(sent, image_data_url);
    let mut payload = Map::new();
    payload.insert("model".into(), Value::String(ep.model.clone()));
    payload.insert(
        "messages".into(),
        serde_json::to_value(&*messages).expect("messages sérialisables"),
    );
    payload.insert("stream".into(), Value::Bool(true));
    payload.insert("temperature".into(), json!(temperature));
    // include_usage → chunk final avec usage.prompt_tokens = taille TOTALE du
    // prompt (préfixe caché compris).
    payload.insert("stream_options".into(), json!({"include_usage": true}));
    if !o.no_sampling {
        apply_sampling_from(&mut payload, cfg);
    }
    if !o.effort.is_empty() {
        payload.insert("reasoning_effort".into(), Value::String(o.effort.into()));
    }
    // chat_template_kwargs est propre à llama.cpp (--jinja) : une API distante
    // stricte refuse un argument inconnu (400).
    if let Some(kw) = o.kwargs
        && !ep.external
    {
        payload.insert("chat_template_kwargs".into(), Value::Object(kw.clone()));
    }
    if !o.tools.is_empty() && !o.disable_tools {
        payload.insert(
            "tools".into(),
            serde_json::to_value(o.tools).expect("outils sérialisables"),
        );
        // Un seul appel d'outil par tour : ce llama.cpp sérialise mal les
        // appels parallèles (« {...}{...} », HTTP 500).
        payload.insert("parallel_tool_calls".into(), Value::Bool(false));
        if o.tool_choice_none {
            payload.insert("tool_choice".into(), Value::String("none".into()));
        }
    }
    Value::Object(payload)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::chat::types::{ToolCall, ToolCallFunc, ToolFunction};
    use crate::engstatus::EXT_KEY_FLAG;

    const ECHO_TEST_MODEL: &str = "Qwen3.6-27B-Q8_0.gguf";

    fn txt(role: &str, s: &str) -> Message {
        Message {
            role: role.into(),
            content: Some(Value::String(s.into())),
            ..Default::default()
        }
    }
    fn um(s: &str) -> Message {
        txt("user", s)
    }
    fn am(s: &str) -> Message {
        txt("assistant", s)
    }
    fn sys(s: &str) -> Message {
        txt("system", s)
    }
    fn tm(s: &str) -> Message {
        Message {
            tool_call_id: "c1".into(),
            ..txt("tool", s)
        }
    }
    fn tc(id: &str, kind: &str, name: &str, args: &str) -> ToolCall {
        ToolCall {
            id: id.into(),
            kind: kind.into(),
            function: ToolCallFunc {
                name: name.into(),
                arguments: args.into(),
            },
            ..Default::default()
        }
    }
    fn atc(name: &str) -> Message {
        Message {
            role: "assistant".into(),
            tool_calls: vec![tc("c1", "", name, "{}")],
            ..Default::default()
        }
    }
    fn cfg(kv: &[(&str, &str)]) -> HashMap<String, String> {
        kv.iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect()
    }
    fn msg_text(m: &Message) -> String {
        match &m.content {
            Some(Value::String(s)) => s.clone(),
            _ => String::new(),
        }
    }
    fn no_images(_: &str) -> Option<String> {
        None
    }

    // --- normalize_system_test.go -------------------------------------------

    #[test]
    fn normalize_system_messages_no_op_on_normal_conversation() {
        let input = vec![
            sys("Tu es Loki."),
            um("cherche la météo"),
            Message {
                tool_calls: vec![tc("call_1", "function", "", "")],
                ..am("")
            },
            Message {
                tool_call_id: "call_1".into(),
                ..txt("tool", "résultat brut")
            },
            am("Il fait beau."),
            Message {
                role: "user".into(),
                content: Some(json!([{"type": "text", "text": "et demain ?"}])),
                ..Default::default()
            },
        ];
        let reference = input.clone();
        let out = normalize_system_messages(&input);
        assert_eq!(&*out, &input[..], "séquence normale modifiée");
        assert_eq!(input, reference, "l'entrée a été mutée");
    }

    #[test]
    fn normalize_system_messages_preserves_order_and_tool_link() {
        let call = Message {
            tool_calls: vec![tc("call_9", "", "", "")],
            ..am("")
        };
        let tool = Message {
            tool_call_id: "call_9".into(),
            ..txt("tool", "r")
        };
        let input = vec![
            sys("S1"),
            um("q"),
            call.clone(),
            tool.clone(),
            sys("n'appelle plus d'outil"),
        ];
        let out = normalize_system_messages(&input);
        let want = [sys("S1\n\nn'appelle plus d'outil"), um("q"), call, tool];
        assert_eq!(&*out, &want[..]);
    }

    #[test]
    fn normalize_system_messages_cas() {
        fn first_role_system_once(out: &[Message]) {
            let mut count = 0;
            for (i, m) in out.iter().enumerate() {
                if m.role == "system" {
                    count += 1;
                    assert_eq!(i, 0, "system trouvé en position {i}");
                }
            }
            assert!(count <= 1, "{count} messages system");
        }
        // system en fin
        let out = normalize_system_messages(&[sys("A"), um("q"), sys("stop")]).into_owned();
        first_role_system_once(&out);
        assert_eq!(msg_text(&out[0]), "A\n\nstop");
        assert_eq!(out.len(), 2);
        // sans system, inchangé
        let input = [um("a"), um("b")];
        let out = normalize_system_messages(&input);
        assert!(matches!(out, Cow::Borrowed(_)));
        assert_eq!(out.len(), 2);
        assert_eq!(out[0].role, "user");
        // system vide ignoré
        let out = normalize_system_messages(&[sys("  "), um("q")]).into_owned();
        assert_eq!(out.len(), 1);
        assert_eq!(out[0].role, "user");
    }

    // --- normalize_project_test.go ------------------------------------------

    #[test]
    fn normalize_moves_project_context_to_first_user() {
        let input = vec![
            sys("Préambule commun"),
            sys(&format!("{PROJECT_CONTEXT_PREFIX} — projet A")),
            sys(&format!("{MEM_INDEX_PREFIX} for project A")),
            um("bonjour"),
            am("salut"),
            um("suite"),
        ];
        let reference = input.clone();
        let out = normalize_system_messages(&input);
        assert_eq!(out.len(), 4);
        assert_eq!(msg_text(&out[0]), "Préambule commun");
        let first = msg_text(&out[1]);
        assert!(
            first.starts_with(&format!("<project_context>\n{PROJECT_CONTEXT_PREFIX}"))
                && first.contains(MEM_INDEX_PREFIX)
                && first.ends_with("bonjour"),
            "contexte projet absent du 1er message user : {first:?}"
        );
        assert_eq!(msg_text(&out[3]), "suite");
        assert_eq!(input, reference);
    }

    // --- code_instructions_test.go (partie normalisation) -------------------

    #[test]
    fn consignes_du_depot() {
        let m = sys(&format!(
            "{CODE_INSTRUCTIONS_PREFIX} (projet/AGENTS.md):\nBuild : make test"
        ));
        let out = normalize_system_messages(&[sys("base"), m, um("fais-le")]).into_owned();
        assert_eq!(msg_text(&out[0]), "base");
        assert!(msg_text(&out[1]).contains("make test"));
    }

    // --- llm_injected_test.go -----------------------------------------------

    #[test]
    fn with_trailing_hint_cas() {
        let note = format!("\n\n{LOKI_NOTE_PREFIX}H");
        let cases: Vec<(&str, Vec<Message>, Message, bool)> = vec![
            (
                "résultat d'outil",
                vec![um("q"), atc("glob"), tm("r")],
                tm(&format!("r{note}")),
                false,
            ),
            (
                "message user",
                vec![um("q")],
                um(&format!("q{note}")),
                false,
            ),
            (
                "parties",
                vec![Message {
                    role: "user".into(),
                    content: Some(json!([{"type": "text", "text": "q"}])),
                    ..Default::default()
                }],
                Message {
                    role: "user".into(),
                    content: Some(json!([
                        {"type": "text", "text": "q"},
                        {"type": "text", "text": note}
                    ])),
                    ..Default::default()
                },
                false,
            ),
            (
                "assistant en dernier",
                vec![um("q"), am("a")],
                um(&format!("{LOKI_NOTE_PREFIX}H")),
                true,
            ),
        ];
        for (name, input, want, grow) in cases {
            let orig = serde_json::to_string(&input).unwrap();
            let out = with_trailing_hint(&input, "H");
            assert_eq!(
                serde_json::to_string(&input).unwrap(),
                orig,
                "{name} : historique modifié"
            );
            let want_len = input.len() + usize::from(grow);
            assert_eq!(out.len(), want_len, "{name}");
            assert_eq!(out[out.len() - 1], want, "{name}");
            assert_eq!(out[..input.len() - 1], input[..input.len() - 1], "{name}");
        }
    }

    // --- chat_keep_images_test.go / chat_projsnap_test.go (envoi) -----------

    #[test]
    fn relais_pas_pris_pour_la_demande() {
        let relay = Message {
            role: "user".into(),
            img_relay: true,
            img_tokens: 10,
            content: Some(json!([
                {"type": "text", "text": "Voici la capture demandée."},
                {"type": "image_url", "image_url": {"url": format!("{IMG_REF_SCHEME}abc.png")}}
            ])),
            ..Default::default()
        };
        let msgs = vec![
            um("ancienne"),
            am("ok"),
            um("vraie demande"),
            atc("see_image"),
            tm("[ok]"),
            relay,
        ];
        let (sent, _) = wire_messages(&msgs, false, &EchoPolicy::default());
        assert!(
            sent.iter().all(|m| !m.img_relay),
            "marque de relais dans la requête"
        );
        assert!(
            msgs[5].img_relay,
            "l'historique a perdu sa marque à l'envoi"
        );
    }

    #[test]
    fn proj_snap_texte_tape_intact() {
        let marked = Message {
            ctx_upd: true,
            ..um("<context_update at=\"t\">\n+ x\n</context_update>\n\nsuite")
        };
        let (wire, _) = wire_messages(std::slice::from_ref(&marked), false, &EchoPolicy::default());
        let b = serde_json::to_string(&*wire).unwrap();
        assert!(!b.contains("ctx_upd"), "marque envoyée au moteur : {b}");
        assert!(marked.ctx_upd);
    }

    // --- llm_reasoning_echo_test.go -----------------------------------------

    #[test]
    fn echo_messages_octet_pour_octet() {
        let plain = vec![
            sys("sys"),
            um("question"),
            Message {
                tool_calls: vec![tc("c1", "function", "bash", "{}")],
                ..am("je regarde")
            },
            tm("sortie"),
            am("réponse"),
        ];
        let on = EchoPolicy {
            on: true,
            model: ECHO_TEST_MODEL.into(),
            keeps_past: true,
        };
        let (out, n) = echo_messages(&plain, &on);
        assert!(
            n == 0 && matches!(out, Cow::Borrowed(_)),
            "aucun raisonnement : la tranche doit ressortir sans copie"
        );
        let want = serde_json::to_string(&plain).unwrap();
        let mut stored = plain.clone();
        stored[2].reasoning_content = "raisonnement".into();
        stored[2].reasoning_model = ECHO_TEST_MODEL.into();
        stored[4].reasoning_content = "autre".into();
        stored[4].reasoning_model = ECHO_TEST_MODEL.into();
        for p in [
            EchoPolicy::default(),
            EchoPolicy {
                on: true,
                model: "autre-modele.gguf".into(),
                keeps_past: true,
            },
        ] {
            let (got, n) = echo_messages(&stored, &p);
            let b = serde_json::to_string(&*got).unwrap();
            assert!(n == 0 && b == want, "politique {p:?} : {b}\nattendu {want}");
        }
        assert!(
            !stored[2].reasoning_content.is_empty(),
            "historique d'origine modifié"
        );
    }

    #[test]
    fn echo_messages_politique() {
        let p = EchoPolicy {
            on: true,
            model: ECHO_TEST_MODEL.into(),
            keeps_past: true,
        };
        let with = |m: Message, r: &str, model: &str| Message {
            reasoning_content: r.into(),
            reasoning_model: model.into(),
            ..m
        };
        let msgs = vec![
            um("q"),
            with(am("a"), "r1", ECHO_TEST_MODEL),
            with(am("b"), "r2", "Llama-3.gguf"),
            with(
                Message {
                    role: "assistant".into(),
                    ..Default::default()
                },
                "r3",
                ECHO_TEST_MODEL,
            ),
            with(
                Message {
                    role: "assistant".into(),
                    tool_calls: vec![tc("c1", "function", "bash", "{}")],
                    ..Default::default()
                },
                "r4",
                ECHO_TEST_MODEL,
            ),
            with(um("x"), "r5", ECHO_TEST_MODEL),
        ];
        let (out, n) = echo_messages(&msgs, &p);
        assert!(
            n == 2 && out[1].reasoning_content == "r1" && out[4].reasoning_content == "r4",
            "renvoyés : {n}, {out:?}"
        );
        for (i, m) in out.iter().enumerate() {
            assert!(m.reasoning_model.is_empty(), "étiquette envoyée ({i})");
            assert!(
                !(m.role == "assistant" && !m.reasoning_content.is_empty() && !has_body(m)),
                "pur raisonnement envoyé ({i})"
            );
        }
        assert!(
            out[2].reasoning_content.is_empty()
                && out[3].reasoning_content.is_empty()
                && out[5].reasoning_content.is_empty()
        );
    }

    #[test]
    fn echo_policy_for_cas() {
        let model_path = format!("/models/{ECHO_TEST_MODEL}");
        let base = cfg(&[("REASONING_ECHO", "on"), ("MODEL", &model_path)]);
        let with = |k: &str, v: &str| {
            let mut c = base.clone();
            c.insert(k.into(), v.into());
            c
        };
        let refusals = EchoRefusals::new();
        let p = echo_policy_for(&base, &refusals, None);
        assert!(
            p.on && p.model == ECHO_TEST_MODEL && p.keeps_past,
            "clé posée : {p:?}"
        );
        assert!(!echo_policy_for(&with("REASONING_ECHO", ""), &refusals, None).on);
        assert!(!echo_policy_for(&with(EXT_KEY_FLAG, "1"), &refusals, None).on);
        assert!(!echo_policy_for(&with("MODEL", ""), &refusals, None).on);
        refusals.refuse(ECHO_TEST_MODEL);
        assert!(!echo_policy_for(&base, &refusals, None).on);
        let refusals = EchoRefusals::new();
        let v = |model: &str, rr, ph| TplVerdict {
            model: model.into(),
            renders_reasoning: rr,
            preserves_history: ph,
        };
        let r = v(ECHO_TEST_MODEL, TplTri::No, TplTri::No);
        assert!(!echo_policy_for(&base, &refusals, Some(&r)).on);
        let r = v(ECHO_TEST_MODEL, TplTri::Yes, TplTri::No);
        let p = echo_policy_for(&base, &refusals, Some(&r));
        assert!(p.on && !p.keeps_past, "gabarit façon Qwen3.5 : {p:?}");
        let r = v("autre.gguf", TplTri::No, TplTri::No);
        let p = echo_policy_for(&base, &refusals, Some(&r));
        assert!(p.on && p.keeps_past, "verdict d'un autre modèle : {p:?}");
    }

    #[test]
    fn echo_tokens_suit_le_rendu() {
        let r = "r".repeat(400); // 100 jetons
        let with = |m: Message| Message {
            reasoning_content: r.clone(),
            reasoning_model: ECHO_TEST_MODEL.into(),
            ..m
        };
        let msgs = vec![um("q1"), with(am("a")), um("q2"), with(am("b"))];
        let sum = |v: Option<Vec<usize>>| v.unwrap_or_default().iter().sum::<usize>();
        let keeps = EchoPolicy {
            on: true,
            model: ECHO_TEST_MODEL.into(),
            keeps_past: true,
        };
        assert_eq!(sum(echo_tokens_for(&msgs, &keeps)), 200);
        let drops = EchoPolicy {
            keeps_past: false,
            ..keeps.clone()
        };
        assert_eq!(sum(echo_tokens_for(&msgs, &drops)), 100);
        assert_eq!(echo_tokens_for(&msgs, &EchoPolicy::default()), None);
        // echoTokens : sans raisonnement, la politique n'est même pas lue.
        assert_eq!(
            echo_tokens(&[um("q")], || panic!("politique lue sans raisonnement")),
            None
        );
        assert_eq!(sum(echo_tokens(&msgs, || keeps.clone())), 200);
    }

    #[test]
    fn echo_template_error_etroit() {
        let cases: [(u16, &str, bool); 6] = [
            (
                500,
                r#"{"error":{"message":"Jinja Exception: raise_exception('Cannot pass both content and thinking')"}}"#,
                true,
            ),
            (
                500,
                r#"{"error":{"message":"Error in template: thinking blocks not allowed here"}}"#,
                true,
            ),
            (
                400,
                r#"{"error":{"message":"Expected 'content' or 'tool_calls'"}}"#,
                true,
            ),
            (
                500,
                r#"{"error":{"message":"Failed to parse input at pos 12: <tool_call>{\"thinking\": 1}"}}"#,
                false,
            ),
            (
                500,
                r#"{"error":{"message":"the model kept thinking forever"}}"#,
                false,
            ),
            (502, r#"{"error":{"message":"raise_exception"}}"#, false),
        ];
        for (status, body, want) in cases {
            assert_eq!(echo_template_error(status, body), want, "{status} {body}");
        }
    }

    // --- llm_effort_test.go -------------------------------------------------

    const QWEN_REFUS: &str = r#"{"error":{"code":500,"message":"\n------------\nWhile executing CallExpression at line 49, column 28 in source:\n...', 'low') %}\n {{- raise_exception('Unexpected reasoning effort ' ~ reason...\n ^\nError: Jinja Exception: Unexpected reasoning effort high. Supported types are xhigh (default), medium, and low.","type":"server_error"}}"#;

    #[test]
    fn effort_rejection_cas() {
        let sup = effort_rejection(QWEN_REFUS).expect("refus non reconnu");
        let want = ["xhigh", "medium", "low"];
        assert_eq!(sup.len(), want.len(), "niveaux acceptés = {sup:?}");
        for s in &sup {
            assert!(want.contains(&s.as_str()), "niveau {s:?} lu à tort");
        }
        for body in [
            "",
            r#"{"error":{"code":500,"message":"the request exceeds the available context size"}}"#,
            r#"{"error":{"code":500,"message":"Failed to parse tool call"}}"#,
            r#"{"error":{"code":500,"message":"reasoning_effort applied"}}"#,
        ] {
            assert!(
                effort_rejection(body).is_none(),
                "corps {body:?} pris à tort"
            );
        }
    }

    #[test]
    fn effort_rejection_sans_liste() {
        let sup = effort_rejection("Error: Unknown reasoning effort 'xhigh'").expect("refus");
        assert!(sup.is_empty(), "niveaux acceptés = {sup:?}");
        assert_eq!(nearest_effort("xhigh", &sup), "");
    }

    #[test]
    fn nearest_effort_cas() {
        let v = |l: &[&str]| l.iter().map(|s| s.to_string()).collect::<Vec<_>>();
        let qwen = v(&["xhigh", "medium", "low"]);
        let oss = v(&["low", "medium", "high"]);
        let cases: [(&str, Vec<String>, &str); 8] = [
            ("high", qwen.clone(), "xhigh"),
            ("xhigh", oss, "high"),
            ("medium", qwen.clone(), "low"),
            ("low", v(&["medium", "high"]), "medium"),
            ("high", vec![], ""),
            ("", qwen.clone(), ""),
            ("none", qwen.clone(), ""),
            ("maximum", qwen, ""),
        ];
        for (want, sup, expect) in cases {
            assert_eq!(nearest_effort(want, &sup), expect, "{want} {sup:?}");
        }
        assert_eq!(nearest_effort("medium", &v(&["low", "high"])), "high");
    }

    #[test]
    fn effort_from_rejection_cas() {
        assert_eq!(
            effort_from_rejection(QWEN_REFUS, "high").as_deref(),
            Some("xhigh")
        );
        assert!(effort_from_rejection("boom", "high").is_none());
    }

    #[test]
    fn effort_memoire() {
        let fb = EffortFallbacks::new();
        let qwen = cfg(&[("MODEL", "/data/models/qwen.gguf")]);
        assert_eq!(fb.resolve(&qwen, "high"), "high");
        fb.remember(&qwen, "high", "xhigh");
        assert_eq!(fb.resolve(&qwen, "high"), "xhigh");
        let oss = cfg(&[("MODEL", "/data/models/gpt-oss.gguf")]);
        assert_eq!(fb.resolve(&oss, "high"), "high");
        fb.remember(&oss, "high", "");
        assert_eq!(fb.resolve(&oss, "high"), "");
    }

    // --- mcp_catalog_test.go (réglages du raisonnement) ---------------------

    #[test]
    fn reasoning_effort_value_cas() {
        for (input, want) in [
            ("", ""),
            ("  ", ""),
            ("auto", ""),
            ("default", ""),
            ("off", "none"),
            ("none", "none"),
            ("false", "none"),
            ("0", "none"),
            ("low", "low"),
            ("MEDIUM", "medium"),
            (" High ", "high"),
        ] {
            assert_eq!(reasoning_effort_value(input), want, "{input:?}");
        }
    }

    #[test]
    fn reasoning_explicitly_off_cas() {
        for v in ["", "   ", "on", "auto", "deepseek"] {
            assert!(!reasoning_explicitly_off(v), "{v:?}");
        }
        for v in ["off", "none", "0", "false", " Disabled "] {
            assert!(reasoning_explicitly_off(v), "{v:?}");
        }
    }

    #[test]
    fn reasoning_template_kwargs_cas() {
        let et = |m: Option<Map<String, Value>>| m.and_then(|m| m.get("enable_thinking").cloned());
        assert_eq!(
            et(reasoning_template_kwargs(false, "none")),
            Some(Value::Bool(false))
        );
        assert_eq!(
            et(reasoning_template_kwargs(true, "")),
            Some(Value::Bool(false))
        );
        let high = reasoning_template_kwargs(false, "high").expect("kwargs");
        assert_eq!(high.get("reasoning_effort"), Some(&json!("high")));
        assert!(!high.contains_key("enable_thinking"));
        assert_eq!(reasoning_template_kwargs(false, ""), None);
    }

    // --- backend_sampling_test.go -------------------------------------------

    #[test]
    fn apply_sampling_from_cas() {
        let mut payload = Map::new();
        payload.insert("temperature".into(), json!(0.7));
        apply_sampling_from(
            &mut payload,
            &cfg(&[
                ("TEMP", "1.0"),
                ("TOP_P", "0.95"),
                ("TOP_K", "20"),
                ("MIN_P", "0"),
                ("REPEAT_PENALTY", "1.05"),
                ("PRESENCE_PENALTY", ""),
            ]),
        );
        assert_eq!(payload["temperature"], json!(1.0));
        assert_eq!(payload["top_p"], json!(0.95));
        assert_eq!(payload["min_p"], json!(0.0));
        assert_eq!(payload["repeat_penalty"], json!(1.05));
        assert!(payload["top_k"].is_i64() && payload["top_k"] == json!(20));
        assert!(!payload.contains_key("presence_penalty"));
    }

    #[test]
    fn apply_sampling_ne_touche_pas_au_raisonnement() {
        let mut payload = Map::new();
        payload.insert(
            "chat_template_kwargs".into(),
            json!({"enable_thinking": false}),
        );
        apply_sampling_from(
            &mut payload,
            &cfg(&[("REASONING_EFFORT", "medium"), ("TOP_K", "20")]),
        );
        assert_eq!(
            payload["chat_template_kwargs"],
            json!({"enable_thinking": false})
        );
        assert!(!payload.contains_key("reasoning_effort"));
    }

    #[test]
    fn apply_sampling_empty() {
        let mut payload = Map::new();
        payload.insert("temperature".into(), json!(0.7));
        apply_sampling_from(&mut payload, &HashMap::new());
        assert_eq!(Value::Object(payload), json!({"temperature": 0.7}));
    }

    // --- backend_external_test.go -------------------------------------------

    #[test]
    fn normalisation_de_l_url_de_completions() {
        for (input, want) in [
            (
                "https://api.openai.com/v1",
                "https://api.openai.com/v1/chat/completions",
            ),
            (
                "https://api.groq.com/openai/v1",
                "https://api.groq.com/openai/v1/chat/completions",
            ),
            (
                "https://api.openai.com/v1/chat/completions",
                "https://api.openai.com/v1/chat/completions",
            ),
            (
                "https://api.openai.com/v1/",
                "https://api.openai.com/v1/chat/completions",
            ),
            (
                "http://192.168.1.20:8080",
                "http://192.168.1.20:8080/v1/chat/completions",
            ),
            ("", ""),
        ] {
            assert_eq!(completions_url(input), want, "{input:?}");
        }
    }

    /// `externalPresetContent` relu par `parseEnv` : la configuration d'un
    /// preset externe.
    fn external_cfg(url: &str, model: &str, key: &str, ctx: &str) -> HashMap<String, String> {
        let mut m = cfg(&[
            (EXT_KEY_FLAG, "1"),
            (EXT_KEY_URL, url.trim()),
            (EXT_KEY_MODEL, model.trim()),
        ]);
        if !key.trim().is_empty() {
            m.insert(EXT_KEY_TOKEN.into(), key.trim().into());
        }
        if !ctx.trim().is_empty() {
            m.insert("CTX".into(), ctx.trim().into());
        }
        m
    }

    #[test]
    fn endpoint_suit_le_preset_actif() {
        let ep = resolve_chat_endpoint(&cfg(&[("PORT", "9999")]), "");
        assert!(!ep.external);
        assert_eq!(ep.url, "http://localhost:9999/v1/chat/completions");
        assert_eq!(ep.model, "loki");
        let c = external_cfg(
            "https://api.groq.com/openai/v1",
            "llama-3.3-70b",
            "sk-secret",
            "65536",
        );
        let ep = resolve_chat_endpoint(&c, "");
        assert!(ep.external);
        assert_eq!(ep.url, "https://api.groq.com/openai/v1/chat/completions");
        assert_eq!(ep.model, "llama-3.3-70b");
        assert_eq!(ep.key, "sk-secret");
        assert_eq!(c["CTX"], "65536");
    }

    #[test]
    fn la_cle_locale_ne_part_jamais_chez_un_tiers() {
        let mut c = external_cfg("https://api.openai.com/v1", "gpt-4o-mini", "", "");
        c.insert("API_KEY".into(), "sk-loki-locale".into());
        let ep = resolve_chat_endpoint(&c, "sk-loki-locale");
        assert_eq!(ep.key, "");
        let mut got = HashMap::new();
        ep.auth(|k, v| {
            got.insert(k.to_string(), v.to_string());
        });
        assert!(got.is_empty(), "en-tête posé sans clé de preset : {got:?}");
        // Local : la clé du serveur local part bien en Bearer.
        let local = resolve_chat_endpoint(&HashMap::new(), "k");
        assert_eq!(local.url, "http://localhost:8080/v1/chat/completions");
        local.auth(|k, v| {
            got.insert(k.to_string(), v.to_string());
        });
        assert_eq!(got["Authorization"], "Bearer k");
    }

    #[test]
    fn reconnaissance_d_un_preset_externe() {
        assert!(is_external(&cfg(&[(EXT_KEY_FLAG, "1")])));
        for v in ["", "0", "off", "false"] {
            assert!(!is_external(&cfg(&[(EXT_KEY_FLAG, v)])), "{v:?}");
        }
        assert!(!is_external(&crate::presets::parse_env(
            "MODEL=\"m.gguf\"\nCTX=4096\n"
        )));
    }

    // --- chat_images_test.go (partie envoi) ---------------------------------

    #[test]
    fn image_refs_round_trip() {
        let url = "data:image/png;base64,iVBORw0KGgo=";
        let reference = format!("{IMG_REF_SCHEME}0123abcd.png");
        let set = vec![
            Message {
                role: "user".into(),
                content: Some(json!([
                    {"type": "text", "text": "regarde"},
                    {"type": "image_url", "image_url": {"url": reference}}
                ])),
                ..Default::default()
            },
            am("vu"),
        ];
        let resolver = |r: &str| (r == reference).then(|| url.to_string());
        let out = expand_image_refs(&set, &resolver);
        let parts = content_parts(out[0].content.as_ref());
        assert_eq!(part_image_url(parts[1]), url);
        assert_eq!(out[1], set[1]);
        let parts = content_parts(set[0].content.as_ref());
        assert!(
            part_image_url(parts[1]).starts_with(IMG_REF_SCHEME),
            "expand_image_refs ne doit pas toucher l'historique"
        );
        // Image introuvable : mention textuelle ; sans référence : pas de copie.
        let out = expand_image_refs(&set, &no_images);
        assert_eq!(
            content_parts(out[0].content.as_ref())[1]["text"],
            json!("[image indisponible : fichier supprimé]")
        );
        assert!(matches!(
            expand_image_refs(&set[1..], &no_images),
            Cow::Borrowed(_)
        ));
    }

    // --- chat_compact_cont_test.go ------------------------------------------

    #[test]
    fn build_chat_payload_no_sampling() {
        let c = cfg(&[("TOP_P", "0.9")]);
        let ep = ChatEndpoint {
            model: "m".into(),
            ..Default::default()
        };
        let p = build_chat_payload(
            &ep,
            &[um("x")],
            0.7,
            &ChatPayloadOpts::default(),
            &c,
            &no_images,
        );
        assert_eq!(p["top_p"], json!(0.9), "échantillonnage perdu : {p}");
        let p = build_chat_payload(
            &ep,
            &[um("x")],
            0.2,
            &ChatPayloadOpts {
                no_sampling: true,
                ..Default::default()
            },
            &c,
            &no_images,
        );
        assert!(p.get("top_p").is_none() && p["temperature"] == json!(0.2));
    }

    // --- chat_prewarm_test.go -----------------------------------------------

    /// `oldChatPayload` : la construction du corps d'avant buildChatPayload.
    #[allow(clippy::too_many_arguments)]
    fn old_chat_payload(
        ep: &ChatEndpoint,
        messages: &[Message],
        temperature: f64,
        tools: &[Tool],
        disable_tools: bool,
        tool_choice_none: bool,
        echo_off: bool,
        effort: &str,
        kwargs: Option<&Map<String, Value>>,
        c: &HashMap<String, String>,
        current: &EchoPolicy,
    ) -> Value {
        let mut sent = normalize_system_messages(messages).into_owned();
        if tool_choice_none {
            sent = with_trailing_hint(&sent, TOOLS_OFF_HINT);
        }
        let pol = if ep.external || echo_off {
            EchoPolicy::default()
        } else {
            current.clone()
        };
        let sent = echo_messages(&sent, &pol).0.into_owned();
        let mut payload = Map::new();
        payload.insert("model".into(), json!(ep.model));
        payload.insert(
            "messages".into(),
            serde_json::to_value(&*expand_image_refs(&sent, &no_images)).unwrap(),
        );
        payload.insert("stream".into(), json!(true));
        payload.insert("temperature".into(), json!(temperature));
        payload.insert("stream_options".into(), json!({"include_usage": true}));
        apply_sampling_from(&mut payload, c);
        if !effort.is_empty() {
            payload.insert("reasoning_effort".into(), json!(effort));
        }
        if let Some(kw) = kwargs
            && !ep.external
        {
            payload.insert("chat_template_kwargs".into(), Value::Object(kw.clone()));
        }
        if !tools.is_empty() && !disable_tools {
            payload.insert("tools".into(), serde_json::to_value(tools).unwrap());
            payload.insert("parallel_tool_calls".into(), json!(false));
            if tool_choice_none {
                payload.insert("tool_choice".into(), json!("none"));
            }
        }
        Value::Object(payload)
    }

    /// `oldTurnReasoning` : le calcul du raisonnement d'avant, recopié.
    fn old_turn_reasoning(c: &HashMap<String, String>, fb: &EffortFallbacks) -> TurnReasoning {
        let effort_wanted = reasoning_effort_value(cfg_str(c, "REASONING_EFFORT"));
        let effort = fb.resolve(c, &effort_wanted);
        let think_off =
            reasoning_explicitly_off(cfg_str(c, "REASONING")) || effort_wanted == "none";
        let kwargs = reasoning_template_kwargs(think_off, &effort);
        TurnReasoning {
            effort_wanted,
            effort,
            think_off,
            kwargs,
        }
    }

    #[test]
    fn build_chat_payload_identique_avant() {
        let model_path = format!("/models/{ECHO_TEST_MODEL}");
        let base = [
            ("TOP_P", "0.9"),
            ("TOP_K", "20"),
            ("REASONING_ECHO", "on"),
            ("MODEL", model_path.as_str()),
        ];
        let tools = [
            Tool {
                kind: "function".into(),
                function: ToolFunction {
                    name: "bash".into(),
                    description: "Run a command".into(),
                    parameters: json!({"type": "object", "properties": {"cmd": {"type": "string"}}}),
                },
            },
            Tool {
                kind: "function".into(),
                function: ToolFunction {
                    name: "read".into(),
                    description: "Read a file".into(),
                    parameters: Value::Null,
                },
            },
        ];
        let msgs = vec![
            sys("sys"),
            sys(&format!("{PROJECT_CONTEXT_PREFIX}: Projet de test")),
            um("bonjour"),
            Message {
                reasoning_content: "je pense".into(),
                reasoning_model: ECHO_TEST_MODEL.into(),
                ..am("salut")
            },
            Message {
                role: "user".into(),
                content: Some(json!([
                    {"type": "text", "text": "et ça ?"},
                    {"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}}
                ])),
                ..Default::default()
            },
        ];
        let eps = [
            ChatEndpoint {
                url: "http://localhost:1/v1/chat/completions".into(),
                model: "loki".into(),
                ..Default::default()
            },
            ChatEndpoint {
                url: "https://api.example/v1/chat/completions".into(),
                model: "gpt".into(),
                key: String::new(),
                external: true,
            },
        ];
        let fb = EffortFallbacks::new();
        let refusals = EchoRefusals::new();
        for effort_cfg in ["", "high", "none"] {
            let mut c = cfg(&base);
            c.insert("REASONING_EFFORT".into(), effort_cfg.into());
            c.insert("REASONING".into(), "on".into());
            let old = old_turn_reasoning(&c, &fb);
            let new = turn_reasoning(&c, &fb);
            assert_eq!(old, new, "turnReasoning({effort_cfg:?}) différent d'avant");
            let current = echo_policy_for(&c, &refusals, None);
            assert!(current.on);
            for ep in &eps {
                for tl in [&[][..], &tools[..]] {
                    for disable in [false, true] {
                        for none in [false, true] {
                            for echo_off in [false, true] {
                                let want = old_chat_payload(
                                    ep,
                                    &msgs,
                                    0.7,
                                    tl,
                                    disable,
                                    none,
                                    echo_off,
                                    &new.effort,
                                    new.kwargs.as_ref(),
                                    &c,
                                    &current,
                                );
                                let pol =
                                    turn_echo_policy(ep.external || echo_off, || current.clone());
                                let (sent, _) = wire_messages(&msgs, none, &pol);
                                let got = build_chat_payload(
                                    ep,
                                    &sent,
                                    0.7,
                                    &ChatPayloadOpts {
                                        tools: tl,
                                        disable_tools: disable,
                                        tool_choice_none: none,
                                        effort: &new.effort,
                                        kwargs: new.kwargs.as_ref(),
                                        no_sampling: false,
                                    },
                                    &c,
                                    &no_images,
                                );
                                assert_eq!(
                                    serde_json::to_string(&want).unwrap(),
                                    serde_json::to_string(&got).unwrap(),
                                    "corps différent (effort={effort_cfg:?} ext={} outils={} off={disable} none={none} echoOff={echo_off})",
                                    ep.external,
                                    tl.len()
                                );
                            }
                        }
                    }
                }
            }
        }
    }

    // --- Spécifiques au port ------------------------------------------------

    #[test]
    fn wire_messages_sans_changement_ne_copie_pas() {
        // Sans système (sinon normalize copie, comme le Go) ni marque.
        let msgs = vec![am("a"), um("q")];
        let (out, n) = wire_messages(&msgs, false, &EchoPolicy::default());
        assert!(matches!(out, Cow::Borrowed(_)) && n == 0);
        let (out, _) = wire_messages(&msgs, true, &EchoPolicy::default());
        assert_eq!(
            msg_text(&out[1]),
            format!("q\n\n{LOKI_NOTE_PREFIX}{TOOLS_OFF_HINT}")
        );
    }

    #[test]
    fn corps_complet_comme_le_go() {
        let kw = reasoning_template_kwargs(false, "high");
        let tools = [Tool {
            kind: "function".into(),
            function: ToolFunction {
                name: "bash".into(),
                description: "d".into(),
                parameters: Value::Null,
            },
        }];
        let p = build_chat_payload(
            &ChatEndpoint {
                model: "loki".into(),
                ..Default::default()
            },
            &[um("x")],
            0.7,
            &ChatPayloadOpts {
                tools: &tools,
                tool_choice_none: true,
                effort: "high",
                kwargs: kw.as_ref(),
                ..Default::default()
            },
            &cfg(&[("TEMP", "0.6")]),
            &no_images,
        );
        assert_eq!(
            serde_json::to_string(&p).unwrap(),
            r#"{"chat_template_kwargs":{"reasoning_effort":"high"},"messages":[{"content":"x","role":"user"}],"model":"loki","parallel_tool_calls":false,"reasoning_effort":"high","stream":true,"stream_options":{"include_usage":true},"temperature":0.6,"tool_choice":"none","tools":[{"function":{"description":"d","name":"bash","parameters":null},"type":"function"}]}"#
        );
    }

    #[test]
    fn base_de_chemin_comme_filepath() {
        assert_eq!(go_filepath_base(""), ".");
        assert_eq!(go_filepath_base("/models/x.gguf"), "x.gguf");
        assert_eq!(go_filepath_base("x.gguf"), "x.gguf");
        assert_eq!(go_filepath_base("/a/b/"), "b");
        assert_eq!(
            go_filepath_base("///"),
            std::path::MAIN_SEPARATOR.to_string()
        );
        assert_eq!(
            reasoning_echo_model(&cfg(&[("MODEL", " /m/q.gguf ")])),
            "q.gguf"
        );
        assert_eq!(reasoning_echo_model(&HashMap::new()), "");
    }
}
