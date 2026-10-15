//! Sonde de gabarit (lot 23 de la migration) — port de `chat_tplprobe.go`.
//!
//! Que fait le gabarit de chat du modèle chargé de ce qu'on lui envoie ?
//! - `preserves_history` : le raisonnement d'un tour PASSÉ est-il rendu ?
//! - `prefix_stable` : après un tour d'outil, le rendu précédent reste-t-il un
//!   préfixe exact du suivant ?
//! - `renders_reasoning` : `reasoning_content` est-il rendu quelque part ?
//!
//! Mêmes règles de conduite que le Go : moteur local seulement, rien d'autre
//! que `POST /apply-template` (sans état, sans slot), lancée après une
//! complétion terminée, avec les outils et `chat_template_kwargs` de cette
//! complétion ; tout échec donne « unknown ».
//!
//! Le relevé du rendu (`tplRenderPrint`) et sa comparaison (`tplRenderDiff`)
//! servent la vérification autour d'une mise à jour du moteur.

use std::collections::HashMap;
use std::sync::Mutex;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use serde::Serialize;
use serde_json::value::RawValue;
use serde_json::{Map, Value, json};

use super::types::{Tool, ToolCall, ToolCallFunc};
use super::wire::TplTri;
use crate::gojson;

/// Délai de chaque requête (`tplProbeTimeout`).
pub const PROBE_TIMEOUT: Duration = Duration::from_secs(3);
/// Au plus une vérification par minute (`tplProbeEvery`).
pub const PROBE_EVERY: Duration = Duration::from_secs(60);
const CACHE_MAX: usize = 16;
const TRIES_503: u32 = 3;
const BODY_MAX: usize = 8 << 20;

const MARK_SYS: &str = "loki-sonde-systeme";
const MARK_A: &str = "loki-sonde-question-a";
const MARK_R1: &str = "loki-sonde-raisonnement-r1";
const MARK_C1: &str = "loki-sonde-reponse-c1";
const MARK_T1: &str = "loki-sonde-resultat-t1";
const MARK_B: &str = "loki-sonde-question-b";
const CALL_ID: &str = "a1B2c3D4e";

/// `tplRenderSep`.
pub const RENDER_SEP: &str = "\n\x00--- ";

impl TplTri {
    pub fn as_str(self) -> &'static str {
        match self {
            TplTri::Yes => "yes",
            TplTri::No => "no",
            TplTri::Unknown => "unknown",
        }
    }
}

/// `tplProbeResult`.
#[derive(Debug, Clone, PartialEq)]
pub struct ProbeResult {
    pub preserves_history: TplTri,
    pub prefix_stable: TplTri,
    pub renders_reasoning: TplTri,
    pub build: String,
    pub model: String,
    pub template_hash: String,
    pub tools: usize,
    pub kwargs: Option<Map<String, Value>>,
    pub effort: String,
    pub note: String,
    pub at: SystemTime,
    /// La réponse tient tant que moteur, modèle et gabarit restent les mêmes.
    pub cacheable: bool,
}

impl ProbeResult {
    /// `tplUnknownResult`.
    pub fn unknown(note: &str) -> Self {
        ProbeResult {
            preserves_history: TplTri::Unknown,
            prefix_stable: TplTri::Unknown,
            renders_reasoning: TplTri::Unknown,
            build: String::new(),
            model: String::new(),
            template_hash: String::new(),
            tools: 0,
            kwargs: None,
            effort: String::new(),
            note: note.into(),
            at: SystemTime::now(),
            cacheable: false,
        }
    }

    /// Le JSON du Go (champs dans l'ordre de la struct, `omitempty` compris).
    pub fn to_json(&self) -> Value {
        let mut m = Map::new();
        m.insert(
            "preserves_history".into(),
            json!(self.preserves_history.as_str()),
        );
        m.insert("prefix_stable".into(), json!(self.prefix_stable.as_str()));
        m.insert(
            "renders_reasoning".into(),
            json!(self.renders_reasoning.as_str()),
        );
        for (k, v) in [
            ("build", &self.build),
            ("model", &self.model),
            ("template_hash", &self.template_hash),
        ] {
            if !v.is_empty() {
                m.insert(k.into(), json!(v));
            }
        }
        m.insert("tools".into(), json!(self.tools));
        if let Some(k) = self.kwargs.as_ref().filter(|k| !k.is_empty()) {
            m.insert("chat_template_kwargs".into(), Value::Object(k.clone()));
        }
        if !self.effort.is_empty() {
            m.insert("reasoning_effort".into(), json!(self.effort));
        }
        if !self.note.is_empty() {
            m.insert("note".into(), json!(self.note));
        }
        m.insert("at".into(), json!(rfc3339(self.at)));
        Value::Object(m)
    }

    /// Le sous-ensemble que lit la politique d'écho.
    pub fn verdict(&self) -> super::wire::TplVerdict {
        super::wire::TplVerdict {
            model: self.model.clone(),
            renders_reasoning: self.renders_reasoning,
            preserves_history: self.preserves_history,
        }
    }
}

/// Instant au format RFC 3339 (UTC, nanosecondes), comme `time.Time`.
pub fn rfc3339(t: SystemTime) -> String {
    let d = t.duration_since(UNIX_EPOCH).unwrap_or_default();
    let secs = d.as_secs() as i64;
    let (days, rem) = (secs.div_euclid(86_400), secs.rem_euclid(86_400));
    // Jours depuis 1970 → date civile (algorithme de Howard Hinnant).
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z - era * 146_097;
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let day = doy - (153 * mp + 2) / 5 + 1;
    let month = if mp < 10 { mp + 3 } else { mp - 9 };
    let year = yoe + era * 400 + i64::from(month <= 2);
    let mut s = format!(
        "{year:04}-{month:02}-{day:02}T{:02}:{:02}:{:02}",
        rem / 3600,
        rem % 3600 / 60,
        rem % 60
    );
    let ns = d.subsec_nanos();
    if ns > 0 {
        let frac = format!("{ns:09}");
        s.push('.');
        s.push_str(frac.trim_end_matches('0'));
    }
    s.push('Z');
    s
}

/// `tplShape` : la forme de la requête à reproduire.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct Shape {
    /// Outils figés en JSON ; `None` = aucun outil envoyé.
    pub tools: Option<String>,
    pub n_tools: usize,
    pub first: String,
    pub kwargs: Option<Map<String, Value>>,
    pub effort: String,
}

impl Shape {
    /// `newTplShape`.
    pub fn new(tools: &[Tool], kwargs: Option<&Map<String, Value>>, effort: &str) -> Self {
        let mut s = Shape {
            n_tools: tools.len(),
            effort: effort.into(),
            ..Shape::default()
        };
        if !tools.is_empty() {
            s.tools = Some(gojson::marshal(tools));
            s.first = tools[0].function.name.clone();
        }
        s.kwargs = kwargs.cloned();
        s
    }

    /// `tplShape.hash` : FNV-64a des outils, des kwargs et de l'effort.
    pub fn hash(&self) -> String {
        let mut h = Fnv::new();
        if let Some(t) = &self.tools {
            h.write(t.as_bytes());
        }
        h.write(&[0]);
        let kw = match &self.kwargs {
            Some(k) => gojson::marshal(&Value::Object(k.clone())),
            None => "null".into(),
        };
        h.write(kw.as_bytes());
        h.write(&[0]);
        h.write(self.effort.as_bytes());
        format!("{:016x}", h.0)
    }
}

/// FNV-1a 64 bits (`hash/fnv.New64a`).
struct Fnv(u64);

impl Fnv {
    fn new() -> Self {
        Fnv(0xcbf2_9ce4_8422_2325)
    }
    fn write(&mut self, b: &[u8]) {
        for &c in b {
            self.0 ^= u64::from(c);
            self.0 = self.0.wrapping_mul(0x0000_0100_0000_01b3);
        }
    }
}

fn fnv_hex(b: &[u8]) -> String {
    let mut h = Fnv::new();
    h.write(b);
    format!("{:016x}", h.0)
}

/// `tplMsg`.
#[derive(Serialize, Clone)]
struct Msg {
    role: &'static str,
    content: &'static str,
    #[serde(skip_serializing_if = "str::is_empty")]
    reasoning_content: &'static str,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    tool_calls: Vec<ToolCall>,
    #[serde(skip_serializing_if = "str::is_empty")]
    tool_call_id: &'static str,
}

fn msg(role: &'static str, content: &'static str) -> Msg {
    Msg {
        role,
        content,
        reasoning_content: "",
        tool_calls: Vec::new(),
        tool_call_id: "",
    }
}

fn call(name: &str) -> ToolCall {
    ToolCall {
        index: None,
        id: CALL_ID.into(),
        kind: "function".into(),
        function: ToolCallFunc {
            name: name.into(),
            arguments: "{}".into(),
        },
    }
}

/// Réponse du moteur : non-200 (`tplHTTPError`) ou autre échec.
#[derive(Debug, Clone, PartialEq)]
pub enum ProbeError {
    Http { status: u16, body: String },
    Other(String),
}

impl std::fmt::Display for ProbeError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ProbeError::Http { status, body } if !body.is_empty() => {
                write!(f, "HTTP {status} : {body}")
            }
            ProbeError::Http { status, .. } => write!(f, "HTTP {status}"),
            ProbeError::Other(e) => f.write_str(e),
        }
    }
}

impl ProbeError {
    /// `tplDefinitive` : l'échec se reproduira à l'identique.
    fn definitive(&self) -> bool {
        match self {
            ProbeError::Http { status, .. } => {
                !matches!(status, 401 | 403 | 503 | 429) && (400..600).contains(status)
            }
            ProbeError::Other(_) => false,
        }
    }
}

/// `/props` : ce que la sonde en retient.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Props {
    pub build_info: String,
    pub model_path: String,
    pub chat_template: String,
}

/// `tplProber` : le moteur local.
#[derive(Clone)]
pub struct Prober {
    pub base: String,
    /// Clé Bearer du moteur ("" = aucune).
    pub key: String,
    pub client: reqwest::Client,
    /// Attente entre deux essais sur un 503 (`tplProbeBackoff`).
    pub backoff: Duration,
}

impl Prober {
    /// `tplLocalProber`.
    pub fn local(port: i64, key: &str, backoff: Duration) -> Self {
        crate::chat::engine::install_crypto();
        Prober {
            base: format!("http://localhost:{port}"),
            key: key.into(),
            client: reqwest::Client::builder()
                .timeout(PROBE_TIMEOUT)
                .build()
                .unwrap_or_default(),
            backoff,
        }
    }

    async fn call(
        &self,
        method: reqwest::Method,
        path: &str,
        body: Option<Vec<u8>>,
    ) -> Result<Vec<u8>, ProbeError> {
        let mut rq = self
            .client
            .request(method, format!("{}{path}", self.base))
            .timeout(PROBE_TIMEOUT);
        if let Some(b) = body {
            rq = rq.header("Content-Type", "application/json").body(b);
        }
        if !self.key.is_empty() {
            rq = rq.header("Authorization", format!("Bearer {}", self.key));
        }
        let mut resp = rq
            .send()
            .await
            .map_err(|e| ProbeError::Other(e.to_string()))?;
        let status = resp.status().as_u16();
        let mut data = Vec::new();
        while data.len() < BODY_MAX {
            match resp.chunk().await {
                Ok(Some(c)) => data.extend_from_slice(&c),
                Ok(None) => break,
                Err(e) => return Err(ProbeError::Other(e.to_string())),
            }
        }
        data.truncate(BODY_MAX);
        if status != 200 {
            let msg = String::from_utf8_lossy(&data).trim().to_string();
            let msg = if msg.chars().count() > 160 {
                format!("{}…", msg.chars().take(160).collect::<String>())
            } else {
                msg
            };
            return Err(ProbeError::Http { status, body: msg });
        }
        Ok(data)
    }

    /// `/health` à 200 ?
    pub async fn healthy(&self) -> bool {
        self.call(reqwest::Method::GET, "/health", None)
            .await
            .is_ok()
    }

    /// `/props`.
    pub async fn props(&self) -> Result<Props, ProbeError> {
        let data = self.call(reqwest::Method::GET, "/props", None).await?;
        let bad = || ProbeError::Other("réponse non JSON".into());
        let v: Value = serde_json::from_slice(&data).map_err(|_| bad())?;
        let mut p = Props::default();
        match v {
            Value::Null => return Ok(p),
            Value::Object(m) => {
                for (k, dst) in [
                    ("build_info", &mut p.build_info),
                    ("model_path", &mut p.model_path),
                    ("chat_template", &mut p.chat_template),
                ] {
                    match m.get(k) {
                        None | Some(Value::Null) => {}
                        Some(Value::String(s)) => *dst = s.clone(),
                        Some(_) => return Err(bad()),
                    }
                }
            }
            _ => return Err(bad()),
        }
        Ok(p)
    }

    /// `render` : POST /apply-template, relancé sur 503.
    async fn render(
        &self,
        shape: &Shape,
        msgs: &[Msg],
        gen_prompt: bool,
    ) -> Result<String, ProbeError> {
        #[derive(Serialize)]
        struct Body<'a> {
            add_generation_prompt: bool,
            #[serde(skip_serializing_if = "Option::is_none")]
            chat_template_kwargs: Option<&'a Map<String, Value>>,
            messages: &'a [Msg],
            #[serde(skip_serializing_if = "Option::is_none")]
            parallel_tool_calls: Option<bool>,
            #[serde(skip_serializing_if = "Option::is_none")]
            reasoning_effort: Option<&'a str>,
            #[serde(skip_serializing_if = "Option::is_none")]
            tools: Option<&'a RawValue>,
        }
        let raw_tools = shape
            .tools
            .as_deref()
            .and_then(|t| serde_json::from_str::<&RawValue>(t).ok());
        let raw = gojson::marshal(&Body {
            add_generation_prompt: gen_prompt,
            chat_template_kwargs: shape.kwargs.as_ref(),
            messages: msgs,
            parallel_tool_calls: raw_tools.map(|_| false),
            reasoning_effort: (!shape.effort.is_empty()).then_some(shape.effort.as_str()),
            tools: raw_tools,
        });
        let mut tries = 1;
        loop {
            match self
                .call(
                    reqwest::Method::POST,
                    "/apply-template",
                    Some(raw.clone().into_bytes()),
                )
                .await
            {
                Err(ProbeError::Http { status: 503, .. }) if tries < TRIES_503 => {
                    tokio::time::sleep(self.backoff * tries).await;
                    tries += 1;
                }
                Err(e) => return Err(e),
                Ok(data) => {
                    let prompt = serde_json::from_slice::<Value>(&data)
                        .ok()
                        .and_then(|v| v.get("prompt").cloned());
                    return match prompt {
                        Some(Value::String(p)) => Ok(p),
                        _ => Err(ProbeError::Http {
                            status: 200,
                            body: "réponse sans prompt".into(),
                        }),
                    };
                }
            }
        }
    }

    /// `probe` : les rendus et leurs verdicts.
    pub async fn probe(&self, shape: &Shape) -> ProbeResult {
        let mut r = ProbeResult::unknown("");
        let mut definitive = true;
        let mut notes: Vec<String> = Vec::new();
        let mut fail = |what: &str, e: &ProbeError, notes: &mut Vec<String>| {
            if !e.definitive() {
                definitive = false;
            }
            notes.push(format!("{what} : {e}"));
        };
        let sys = msg("system", MARK_SYS);
        let user_a = msg("user", MARK_A);
        let user_b = msg("user", MARK_B);
        let mut past = msg("assistant", MARK_C1);
        past.reasoning_content = MARK_R1;

        // 1) Raisonnement d'un tour passé.
        match self
            .render(
                shape,
                &[sys.clone(), user_a.clone(), past, user_b.clone()],
                false,
            )
            .await
        {
            Err(e) => fail("historique", &e, &mut notes),
            Ok(h) if !h.contains(MARK_C1) || !h.contains(MARK_B) => {
                notes.push("historique : rendu incohérent".into());
            }
            Ok(h) if h.contains(MARK_R1) => r.preserves_history = TplTri::Yes,
            Ok(_) => r.preserves_history = TplTri::No,
        }

        // 2) Stabilité du préfixe à travers un tour d'outil.
        let name = if shape.first.is_empty() {
            "lookup"
        } else {
            &shape.first
        };
        let mut c = msg("assistant", MARK_C1);
        c.reasoning_content = MARK_R1;
        c.tool_calls = vec![call(name)];
        let mut result = msg("tool", MARK_T1);
        result.tool_call_id = CALL_ID;
        let before = vec![sys, user_a, c, result];
        let mut after = before.clone();
        after.push(user_b);

        let r1 = self.render(shape, &before, false).await;
        let mut r1g: Result<String, ProbeError> = Err(ProbeError::Other(String::new()));
        let mut r2: Result<String, ProbeError> = Err(ProbeError::Other(String::new()));
        if r1.is_ok() {
            r1g = self.render(shape, &before, true).await;
        }
        if r1.is_ok() && r1g.is_ok() {
            r2 = self.render(shape, &after, false).await;
        }
        match (&r1, &r1g, &r2) {
            (Err(e), _, _) | (Ok(_), Err(e), _) | (Ok(_), Ok(_), Err(e)) => {
                fail("préfixe", e, &mut notes)
            }
            (Ok(a), Ok(g), _) if a == g => {
                notes.push("add_generation_prompt ignoré par le moteur".into())
            }
            (Ok(a), Ok(_), Ok(b)) if !a.contains(MARK_T1) || !b.contains(MARK_B) => {
                notes.push("préfixe : rendu incohérent".into())
            }
            (Ok(a), Ok(_), Ok(b)) if b.starts_with(a.as_str()) => r.prefix_stable = TplTri::Yes,
            _ => r.prefix_stable = TplTri::No,
        }
        let r1_has = |m: &str| r1.as_ref().is_ok_and(|s| s.contains(m));
        if r.preserves_history == TplTri::Yes || r1_has(MARK_R1) {
            r.renders_reasoning = TplTri::Yes;
        } else if r1_has(MARK_T1) && r.preserves_history == TplTri::No {
            r.renders_reasoning = TplTri::No;
            notes.push("reasoning_content jamais rendu".into());
        }
        r.note = notes.join(" ; ");
        r.cacheable = definitive;
        r
    }

    /// `tplRenderPrint` : les rendus concaténés de trois conversations
    /// synthétiques sur la forme donnée.
    pub async fn render_print(&self, shape: &Shape) -> Result<String, String> {
        let sys = msg("system", MARK_SYS);
        let user_a = msg("user", MARK_A);
        let user_b = msg("user", MARK_B);
        let name = if shape.first.is_empty() {
            "lookup"
        } else {
            &shape.first
        };
        let mut with_r = msg("assistant", MARK_C1);
        with_r.reasoning_content = MARK_R1;
        let mut c = msg("assistant", MARK_C1);
        c.tool_calls = vec![call(name)];
        let mut result = msg("tool", MARK_T1);
        result.tool_call_id = CALL_ID;
        let convs: [(&str, Vec<Msg>); 3] = [
            (
                "historique sans raisonnement",
                vec![
                    sys.clone(),
                    user_a.clone(),
                    msg("assistant", MARK_C1),
                    user_b.clone(),
                ],
            ),
            (
                "historique avec raisonnement",
                vec![sys.clone(), user_a.clone(), with_r, user_b.clone()],
            ),
            ("tour d'outil", vec![sys, user_a, c, result, user_b]),
        ];
        let mut b = String::new();
        for (name, msgs) in convs {
            let out = self
                .render(shape, &msgs, true)
                .await
                .map_err(|e| format!("{name} : {e}"))?;
            b.push_str(&format!("{RENDER_SEP}{name} ---\n"));
            b.push_str(&out);
        }
        Ok(b)
    }
}

/// `tplRenderDiff` : identiques → (true, "") ; sinon la conversation où les
/// relevés divergent et un extrait de part et d'autre du premier écart.
pub fn render_diff(before: &str, after: &str) -> (bool, String) {
    if before == after {
        return (true, String::new());
    }
    let (bb, ab) = (before.as_bytes(), after.as_bytes());
    let mut i = 0;
    while i < bb.len() && i < ab.len() && bb[i] == ab[i] {
        i += 1;
    }
    let mut section = String::new();
    if let Some(k) = find_last(&bb[..i], RENDER_SEP.as_bytes()) {
        let rest = &bb[k + RENDER_SEP.len()..];
        if let Some(e) = find(rest, b" ---") {
            section = String::from_utf8_lossy(&rest[..e]).into_owned();
        }
    }
    let excerpt = |s: &[u8]| -> String {
        let mut from = i.saturating_sub(40);
        let mut to = (i + 60).min(s.len());
        while from > 0 && !rune_start(s[from]) {
            from -= 1;
        }
        while to < s.len() && !rune_start(s[to]) {
            to += 1;
        }
        let mut x = String::from_utf8_lossy(&s[from..to])
            .replace('\n', "⏎")
            .replace('\x00', "");
        if from > 0 {
            x = format!("…{x}");
        }
        if to < s.len() {
            x.push('…');
        }
        x
    };
    let mut d = format!("avant « {} » / après « {} »", excerpt(bb), excerpt(ab));
    if !section.is_empty() {
        d = format!("{section} : {d}");
    }
    (false, d)
}

fn rune_start(b: u8) -> bool {
    b & 0xC0 != 0x80
}

fn find(h: &[u8], n: &[u8]) -> Option<usize> {
    h.windows(n.len()).position(|w| w == n)
}

fn find_last(h: &[u8], n: &[u8]) -> Option<usize> {
    if h.len() < n.len() {
        return None;
    }
    (0..=h.len() - n.len())
        .rev()
        .find(|&i| &h[i..i + n.len()] == n)
}

/// `tplProbeLine`.
pub fn probe_line(r: &ProbeResult) -> String {
    let dash = |s: &str| {
        if s.is_empty() {
            "-".to_string()
        } else {
            s.to_string()
        }
    };
    let mut s = format!(
        "[tplprobe] build={} modèle={} gabarit={} outils={} historique_raisonnement={} préfixe_stable={} raisonnement_rendu={}",
        dash(&r.build),
        dash(&r.model),
        dash(&r.template_hash),
        r.tools,
        r.preserves_history.as_str(),
        r.prefix_stable.as_str(),
        r.renders_reasoning.as_str()
    );
    if !r.note.is_empty() {
        s.push_str(&format!(" ({})", r.note));
    }
    s
}

/// Où sonder : le moteur local, sa clé, et si un preset externe est actif.
#[derive(Debug, Clone, Default)]
pub struct Env {
    pub external: bool,
    pub port: i64,
    pub key: String,
}

#[derive(Default)]
struct State {
    cache: HashMap<String, ProbeResult>,
    last: Option<ProbeResult>,
    by_shape: HashMap<String, ProbeResult>,
    running: bool,
    tried: Option<Instant>,
    logged: String,
    shape_last: Option<Shape>,
}

/// La sonde : son cache et ses derniers verdicts (l'état global du Go).
pub struct TplProbe {
    state: Mutex<State>,
    /// Déclenchement automatique après les complétions (`tplProbeAuto`).
    pub auto: bool,
    /// Attente entre deux essais sur un 503.
    pub backoff: Duration,
    /// Intervalle minimal entre deux vérifications automatiques.
    pub every: Duration,
}

impl Default for TplProbe {
    fn default() -> Self {
        TplProbe::new()
    }
}

/// Le plus ancien verdict d'une table (pour lui faire de la place).
fn oldest(m: &HashMap<String, ProbeResult>) -> Option<String> {
    m.iter().min_by_key(|(_, v)| v.at).map(|(k, _)| k.clone())
}

impl TplProbe {
    pub fn new() -> Self {
        TplProbe {
            state: Mutex::new(State::default()),
            auto: true,
            backoff: Duration::from_secs(1),
            every: PROBE_EVERY,
        }
    }

    fn st(&self) -> std::sync::MutexGuard<'_, State> {
        self.state.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// `tplLastShape` : la forme de la dernière requête sondée, ou une
    /// requête nue.
    pub fn last_shape(&self) -> Shape {
        self.st().shape_last.clone().unwrap_or_default()
    }

    /// `tplCapsCurrent` : le dernier verdict, rien pour un preset externe.
    pub fn caps_current(&self, external: bool) -> Option<ProbeResult> {
        if external {
            return None;
        }
        self.st().last.clone()
    }

    /// `tplCapsFor` : le dernier verdict pour CETTE forme.
    pub fn caps_for(&self, external: bool, shape: &Shape) -> Option<ProbeResult> {
        if external {
            return None;
        }
        self.st().by_shape.get(&shape.hash()).cloned()
    }

    /// Pose le verdict d'une forme comme si la sonde venait de conclure
    /// (tests, et reprise d'un verdict connu).
    pub fn remember(&self, shape: &Shape, r: ProbeResult) {
        let mut st = self.st();
        st.last = Some(r.clone());
        Self::keep_shape(&mut st, &shape.hash(), &r);
    }

    /// `tplProbeKeepShape` (verrou tenu).
    fn keep_shape(st: &mut State, shape: &str, r: &ProbeResult) {
        if !st.by_shape.contains_key(shape)
            && st.by_shape.len() >= CACHE_MAX
            && let Some(k) = oldest(&st.by_shape)
        {
            st.by_shape.remove(&k);
        }
        st.by_shape.insert(shape.into(), r.clone());
    }

    /// `tplProbeStore`.
    fn store(&self, key: &str, shape: &str, r: &ProbeResult) {
        let mut st = self.st();
        st.last = Some(r.clone());
        Self::keep_shape(&mut st, shape, r);
        if key.is_empty() || !r.cacheable {
            return;
        }
        if st.cache.len() >= CACHE_MAX
            && let Some(k) = oldest(&st.cache)
        {
            st.cache.remove(&k);
        }
        st.cache.insert(key.into(), r.clone());
    }

    /// Le prober du moteur local.
    pub fn prober(&self, env: &Env) -> Prober {
        Prober::local(env.port, &env.key, self.backoff)
    }

    /// `tplProbeEnsure` : verdict pour la forme, depuis le cache ou une sonde.
    pub async fn ensure(&self, env: &Env, shape: &Shape) -> ProbeResult {
        if env.external {
            return ProbeResult::unknown("preset externe : pas de sonde");
        }
        self.st().shape_last = Some(shape.clone());
        let p = self.prober(env);
        if !p.healthy().await {
            return ProbeResult::unknown("moteur pas prêt (/health)");
        }
        let props = match p.props().await {
            Ok(pr) => pr,
            Err(e) => {
                let r = ProbeResult::unknown(&format!("/props illisible : {e}"));
                self.store("", &shape.hash(), &r);
                return r;
            }
        };
        let tmpl_hash = fnv_hex(props.chat_template.as_bytes());
        let key = format!(
            "{}\x00{}\x00{tmpl_hash}\x00{}",
            props.build_info,
            props.model_path,
            shape.hash()
        );
        {
            let mut st = self.st();
            if let Some(r) = st.cache.get(&key).cloned() {
                st.last = Some(r.clone());
                Self::keep_shape(&mut st, &shape.hash(), &r);
                return r;
            }
        }
        let mut r = p.probe(shape).await;
        r.build = props.build_info.clone();
        r.template_hash = tmpl_hash;
        r.tools = shape.n_tools;
        r.kwargs = shape.kwargs.clone();
        r.effort = shape.effort.clone();
        if !props.model_path.is_empty() {
            r.model = crate::paths::base_name(&props.model_path);
        }
        let line = probe_line(&r);
        let fresh = {
            let mut st = self.st();
            let fresh = line != st.logged;
            st.logged = line.clone();
            fresh
        };
        if fresh {
            eprintln!("{line}");
        }
        self.store(&key, &shape.hash(), &r);
        r
    }

    /// `tplProbeKick` : après une complétion complète du fil principal, en
    /// tâche de fond ; au plus une sonde en vol, au plus une par `every`.
    pub fn kick(self: &std::sync::Arc<Self>, env: Env, shape: Shape) {
        if !self.auto {
            return;
        }
        {
            let mut st = self.st();
            if st.running || st.tried.is_some_and(|t| t.elapsed() < self.every) {
                return;
            }
            st.running = true;
            st.tried = Some(Instant::now());
        }
        let me = self.clone();
        tokio::spawn(async move {
            let _ = tokio::time::timeout(Duration::from_secs(30), me.ensure(&env, &shape)).await;
            me.st().running = false;
        });
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn fnv64a() {
        // Vecteurs de référence FNV-1a 64 bits.
        assert_eq!(fnv_hex(b""), "cbf29ce484222325");
        assert_eq!(fnv_hex(b"a"), "af63dc4c8601ec8c");
    }

    #[test]
    fn instant_rfc3339() {
        assert_eq!(rfc3339(UNIX_EPOCH), "1970-01-01T00:00:00Z");
        assert_eq!(
            rfc3339(UNIX_EPOCH + Duration::new(1_709_251_200, 500_000_000)),
            "2024-03-01T00:00:00.5Z"
        );
    }

    #[test]
    fn json_omitempty() {
        let mut r = ProbeResult::unknown("");
        r.kwargs = Some(Map::new());
        let v = r.to_json();
        assert!(v.get("chat_template_kwargs").is_none());
        assert!(v.get("note").is_none());
        assert_eq!(v["tools"], 0);
    }
}
