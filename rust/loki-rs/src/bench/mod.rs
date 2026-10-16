//! Banc d'essai tenu par la façade (lot 27 de la migration, `LOKI_BENCH=rust`
//! ; expérimental, le Go reste le défaut) — port de `llm_bench.go` et
//! `llm_bench_job.go`.
//!
//! Le bench mesure le moteur LOCAL tel que le chat s'en sert, sur
//! `/v1/chat/completions`, avec le gabarit, le raisonnement et
//! l'échantillonnage du preset :
//!
//! - « rapide » : un préchauffage jeté, puis la ligne courte (~2000 jetons de
//!   prompt, ~300 générés) ;
//! - « complet » : la ligne courte, puis une discussion à profondeur réelle —
//!   un prefill à froid de D jetons et trois tours qui y ajoutent chacun
//!   ~2048 jetons neufs. La reprise du cache est LUE dans la réponse.
//!
//! Une réponse non-200 ou sans timings fait échouer la phase : rien n'est
//! inventé, rien n'est enregistré. Le job, les routes et l'enregistrement
//! sont dans [`job`].

pub mod job;

use std::collections::HashMap;
use std::time::Duration;

use serde::{Deserialize, Serialize};
use serde_json::{Map, Value, json};
use tokio::sync::watch;

use crate::chat::wire::{
    EffortFallbacks, apply_sampling_from, effort_from_rejection, reasoning_effort_value,
    reasoning_explicitly_off, reasoning_template_kwargs,
};

type Cfg = HashMap<String, String>;

/// `benchProtocol`.
pub const PROTOCOL: i64 = 2;
pub const MODE_QUICK: &str = "quick";
pub const MODE_FULL: &str = "full";

const TURNS: i64 = 3;
const APPEND_TOKENS: i64 = 2048;
const GEN_TOKENS: i64 = 256;
const DEPTH_MAX: i64 = 32768;
const DEPTH_CPU: i64 = 16384;
const DEPTH_MIN: i64 = 4096;
const MARGIN: i64 = 512;
pub(crate) const WARM_GEN: i64 = 32;
const SEED: i64 = 1234;
const REUSE_SLACK: i64 = 16;
pub(crate) const SAMPLE_BYTES: usize = 160 << 10;
const FALLBACK_RATIO: f64 = 2.5;
const THRASH_BYTES: i64 = 512 << 20;

/// `benchReqTimeout` : budget d'une requête.
pub const REQ_TIMEOUT: Duration = Duration::from_secs(6 * 60);

/// `errBenchNoTimings`.
pub const ERR_NO_TIMINGS: &str =
    "le moteur n'a pas renvoyé de mesures (timings) : rien à enregistrer";

/// `benchCorpus` : la prose de la ligne courte (le texte même du Go).
pub const CORPUS: &str = include_str!("corpus.txt");

const ASK_PROSE: &str = "\n\nContinue this passage with another 1000+ words of original varied prose, mixing French and English narrative paragraphs on different topics.";
pub(crate) const ASK_FIRST: &str = "Résume en quelques phrases le rôle de ce fichier.";
const ASK_NEXT: &str =
    "Écris en JavaScript une fonction courte inspirée de cette partie, sans explication.";

/// `benchArgEnv` : les LLAMA_ARG_* qui décident des types de cache et du
/// placement des poids.
pub const ARG_ENV: [&str; 6] = [
    "LLAMA_ARG_CACHE_TYPE_K",
    "LLAMA_ARG_CACHE_TYPE_V",
    "LLAMA_ARG_OVERRIDE_TENSOR",
    "LLAMA_ARG_CPU_MOE",
    "LLAMA_ARG_N_CPU_MOE",
    "LLAMA_ARG_N_CPU_FFN",
];

/// Un flottant écrit comme le `json.Marshal` du Go : entier sans « .0 ».
pub fn ser_go_f64<S: serde::Serializer>(v: &f64, s: S) -> Result<S::Ok, S::Error> {
    if v.is_finite() && v.fract() == 0.0 && v.abs() < 1e15 {
        s.serialize_i64(*v as i64)
    } else {
        s.serialize_f64(*v)
    }
}

fn is_zero_f(v: &f64) -> bool {
    *v == 0.0
}

fn is_zero_i(v: &i64) -> bool {
    *v == 0
}

fn is_false(v: &bool) -> bool {
    !*v
}

/// `benchDraft` : acceptation du brouillon.
#[derive(Clone, Copy, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct Draft {
    #[serde(default)]
    pub n: i64,
    #[serde(default)]
    pub accepted: i64,
}

/// `(*benchDraft).add`.
pub fn draft_add(d: Option<Draft>, o: Option<Draft>) -> Option<Draft> {
    let Some(o) = o else { return d };
    let mut d = d.unwrap_or_default();
    d.n += o.n;
    d.accepted += o.accepted;
    Some(d)
}

/// `benchTurn`.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct Turn {
    #[serde(default)]
    pub new: i64,
    #[serde(default)]
    pub cached: i64,
    #[serde(default)]
    pub expected: i64,
    #[serde(default, serialize_with = "ser_go_f64")]
    pub prompt_ms: f64,
    #[serde(default, serialize_with = "ser_go_f64")]
    pub prompt_per_second: f64,
    #[serde(default)]
    pub predicted_n: i64,
    #[serde(default, serialize_with = "ser_go_f64")]
    pub predicted_ms: f64,
    #[serde(default, serialize_with = "ser_go_f64")]
    pub predicted_per_second: f64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub draft: Option<Draft>,
}

/// `benchDepth`.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct Depth {
    #[serde(default)]
    pub ctx: i64,
    #[serde(default)]
    pub target: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub skipped: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cold: Option<Turn>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub turns: Vec<Turn>,
    #[serde(
        default,
        skip_serializing_if = "is_zero_f",
        serialize_with = "ser_go_f64"
    )]
    pub cached_per_second: f64,
    #[serde(
        default,
        skip_serializing_if = "is_zero_f",
        serialize_with = "ser_go_f64"
    )]
    pub decode_per_second: f64,
    #[serde(default, serialize_with = "ser_go_f64")]
    pub reuse: f64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub reuse_note: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub draft: Option<Draft>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub partial: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub hint: String,
}

/// `benchResult`.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct BenchResult {
    #[serde(default)]
    pub prompt_n: i64,
    #[serde(default, serialize_with = "ser_go_f64")]
    pub prompt_ms: f64,
    #[serde(default, serialize_with = "ser_go_f64")]
    pub prompt_per_second: f64,
    #[serde(default)]
    pub predicted_n: i64,
    #[serde(default, serialize_with = "ser_go_f64")]
    pub predicted_ms: f64,
    #[serde(default, serialize_with = "ser_go_f64")]
    pub predicted_per_second: f64,
    #[serde(default, rename = "elapsed_sec", serialize_with = "ser_go_f64")]
    pub elapsed: f64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub mode: String,
    #[serde(default, skip_serializing_if = "is_zero_i")]
    pub protocol: i64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub draft: Option<Draft>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub kv: String,
    #[serde(default, skip_serializing_if = "is_false")]
    pub kv_quantized: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub engine: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub gpus: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub depth: Option<Depth>,
}

/// `benchOpts`.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct Opts {
    pub mode: String,
    pub prompt: i64,
    pub predict: i64,
    /// Profondeur imposée (0 : choisie selon le contexte).
    pub depth: i64,
}

impl Opts {
    pub fn full(&self) -> bool {
        self.mode == MODE_FULL
    }
}

/// `benchCorpora` : prose pour la ligne courte, code pour la profondeur.
pub struct Corpora {
    pub prose: String,
    pub code: String,
}

impl Corpora {
    /// `defaultBenchCorpora` : le code est l'interface web embarquée.
    pub fn default_set() -> Self {
        Corpora {
            prose: CORPUS.to_string(),
            code: String::from_utf8_lossy(crate::ui::index_bytes()).into_owned(),
        }
    }
}

// ---------------------------------------------------------------------------
// Le moteur, vu du bench
// ---------------------------------------------------------------------------

/// Erreur d'une requête au moteur.
#[derive(Clone, Debug, PartialEq)]
pub enum ReqError {
    /// Réponse non-200 (`benchHTTPError`), corps gardé en entier.
    Http {
        method: String,
        path: String,
        status: String,
        body: String,
    },
    /// Le budget de la requête est dépassé (`context.DeadlineExceeded`).
    Timeout(String),
    /// Le bench est annulé.
    Canceled(String),
    /// Autre (réseau, réponse sans timings…).
    Other(String),
}

impl std::fmt::Display for ReqError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ReqError::Http {
                method,
                path,
                status,
                body,
            } => {
                let snip = body.trim();
                let snip = if snip.len() > 300 {
                    let mut cut = 300;
                    while cut > 0 && !snip.is_char_boundary(cut) {
                        cut -= 1;
                    }
                    &snip[..cut]
                } else {
                    snip
                };
                write!(f, "{method} {path} : {status} {snip}")
            }
            ReqError::Timeout(e) | ReqError::Canceled(e) | ReqError::Other(e) => f.write_str(e),
        }
    }
}

/// `benchEngine` : le moteur local.
#[derive(Clone)]
pub struct Engine {
    pub base: String,
    pub key: String,
    pub client: reqwest::Client,
    /// Annulation du bench.
    pub cancel: watch::Receiver<bool>,
    /// Budget d'une requête de chat.
    pub req_timeout: Duration,
    /// Requêtes de chat servies (le slot porte alors l'état du bench).
    pub served: std::sync::Arc<std::sync::atomic::AtomicU64>,
}

/// Le nom de méthode d'une `url.Error` du Go (« Post », « Get »).
fn go_op(method: &str) -> String {
    let m = method.to_ascii_lowercase();
    let mut c = m.chars();
    match c.next() {
        Some(f) => f.to_uppercase().collect::<String>() + c.as_str(),
        None => String::new(),
    }
}

impl Engine {
    /// `do` : le corps d'une réponse 200 (lu jusqu'à 8 Mo) ; toute autre
    /// réponse est une erreur.
    async fn send(
        &self,
        method: &str,
        path: &str,
        payload: Option<&Value>,
        limit: Duration,
    ) -> Result<Vec<u8>, ReqError> {
        let url = format!("{}{path}", self.base);
        let m = reqwest::Method::from_bytes(method.as_bytes()).unwrap_or(reqwest::Method::GET);
        let mut rq = self.client.request(m, &url);
        if let Some(p) = payload {
            rq = rq
                .header("Content-Type", "application/json")
                .body(serde_json::to_vec(p).unwrap_or_default());
        }
        if !self.key.is_empty() {
            rq = rq.header("Authorization", format!("Bearer {}", self.key));
        }
        let op = go_op(method);
        let fut = async {
            let resp = rq
                .send()
                .await
                .map_err(|e| ReqError::Other(format!("{op} \"{url}\": {e}")))?;
            let status = resp.status();
            let mut data = Vec::new();
            let mut resp = resp;
            while let Some(chunk) = resp
                .chunk()
                .await
                .map_err(|e| ReqError::Other(format!("{op} \"{url}\": {e}")))?
            {
                let room = (8usize << 20).saturating_sub(data.len());
                data.extend_from_slice(&chunk[..chunk.len().min(room)]);
                if room <= chunk.len() {
                    break;
                }
            }
            if status != reqwest::StatusCode::OK {
                return Err(ReqError::Http {
                    method: method.to_string(),
                    path: path.to_string(),
                    status: format!(
                        "{} {}",
                        status.as_u16(),
                        status.canonical_reason().unwrap_or("")
                    ),
                    body: String::from_utf8_lossy(&data).into_owned(),
                });
            }
            Ok(data)
        };
        let mut cancel = self.cancel.clone();
        if *cancel.borrow() {
            return Err(ReqError::Canceled(format!(
                "{op} \"{url}\": context canceled"
            )));
        }
        tokio::select! {
            r = tokio::time::timeout(limit, fut) => match r {
                Ok(r) => r,
                Err(_) => Err(ReqError::Timeout(format!(
                    "{op} \"{url}\": context deadline exceeded"
                ))),
            },
            _ = async {
                // Émetteur disparu : plus personne pour annuler.
                if cancel.wait_for(|c| *c).await.is_err() {
                    std::future::pending::<()>().await;
                }
            } => {
                Err(ReqError::Canceled(format!("{op} \"{url}\": context canceled")))
            }
        }
    }

    /// `idle` : un slot qui travaille, c'est la requête d'un autre. `/slots`
    /// coupé ou illisible : on ne sait pas, on n'empêche rien.
    pub async fn idle(&self) -> Result<(), String> {
        let Ok(data) = self
            .send("GET", "/slots", None, Duration::from_secs(3))
            .await
        else {
            return Ok(());
        };
        let Ok(Value::Array(slots)) = serde_json::from_slice::<Value>(&data) else {
            return Ok(());
        };
        let busy = slots
            .iter()
            .any(|s| s.get("is_processing").and_then(Value::as_bool) == Some(true));
        if busy {
            return Err(
                "le moteur traite déjà une requête : relance le bench une fois libre".into(),
            );
        }
        Ok(())
    }

    /// `props` : la taille de contexte et le build. Absents : 0 et "".
    pub async fn props(&self) -> (i64, String) {
        let Ok(data) = self
            .send("GET", "/props", None, Duration::from_secs(5))
            .await
        else {
            return (0, String::new());
        };
        let Ok(Value::Object(p)) = serde_json::from_slice::<Value>(&data) else {
            return (0, String::new());
        };
        let int = |v: Option<&Value>| v.and_then(Value::as_i64).unwrap_or(0);
        let mut n = int(p
            .get("default_generation_settings")
            .and_then(|d| d.get("n_ctx")));
        if n <= 0 {
            n = int(p.get("n_ctx"));
        }
        let build = p
            .get("build_info")
            .and_then(Value::as_str)
            .unwrap_or("")
            .to_string();
        (n, build)
    }

    /// `bytesPerToken` : octets par jeton d'un échantillon (`/tokenize`).
    pub async fn bytes_per_token(&self, sample: &str) -> f64 {
        if sample.is_empty() {
            return FALLBACK_RATIO;
        }
        let Ok(data) = self
            .send(
                "POST",
                "/tokenize",
                Some(&json!({ "content": sample })),
                Duration::from_secs(30),
            )
            .await
        else {
            return FALLBACK_RATIO;
        };
        match serde_json::from_slice::<Value>(&data) {
            Ok(v) => match v.get("tokens").and_then(Value::as_array) {
                Some(t) if !t.is_empty() => sample.len() as f64 / t.len() as f64,
                _ => FALLBACK_RATIO,
            },
            Err(_) => FALLBACK_RATIO,
        }
    }

    /// `chat` : une complétion et ses timings ; sans timings, pas de mesure.
    pub async fn chat(&self, payload: &Value) -> Result<Reply, ReqError> {
        let data = self
            .send(
                "POST",
                "/v1/chat/completions",
                Some(payload),
                self.req_timeout,
            )
            .await?;
        self.served
            .fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        parse_reply(&data)
    }
}

/// `perfNum` : un nombre (ou une chaîne qui en contient un), fini, positif et
/// < 1e12 ; sinon inconnu.
fn perf_num(v: Option<&Value>) -> Option<f64> {
    let f = match v? {
        Value::Number(n) => n.as_f64()?,
        Value::String(s) => s.parse::<f64>().ok()?,
        _ => return None,
    };
    (f.is_finite() && (0.0..1e12).contains(&f)).then_some(f)
}

/// `benchReply` : une réponse du chat. `total` : usage.prompt_tokens (0 =
/// inconnu).
#[derive(Clone, Debug, Default, PartialEq)]
pub struct Reply {
    pub turn: Turn,
    pub total: i64,
    pub content: String,
}

impl Reply {
    /// `promptTotal`.
    pub fn prompt_total(&self) -> i64 {
        if self.total > 0 {
            return self.total;
        }
        self.turn.new + self.turn.cached.max(0)
    }
}

/// La réponse du moteur en mesure (`decodePerfWire` et le contenu).
pub fn parse_reply(data: &[u8]) -> Result<Reply, ReqError> {
    let first = serde_json::Deserializer::from_slice(data)
        .into_iter::<Value>()
        .next();
    let o = match first {
        Some(Ok(Value::Object(o))) => o,
        _ => Map::new(),
    };
    let t = o.get("timings").and_then(Value::as_object);
    let num = |k: &str| t.and_then(|t| perf_num(t.get(k)));
    let (Some(pn), Some(pms), Some(gn), Some(gms)) = (
        num("prompt_n"),
        num("prompt_ms"),
        num("predicted_n"),
        num("predicted_ms"),
    ) else {
        return Err(ReqError::Other(ERR_NO_TIMINGS.into()));
    };
    let rate = |n: f64, ms: f64, given: Option<f64>| match given {
        Some(g) if g > 0.0 => g,
        _ if ms > 0.0 => n / (ms / 1000.0),
        _ => 0.0,
    };
    let mut turn = Turn {
        new: pn as i64,
        cached: -1,
        prompt_ms: pms,
        prompt_per_second: rate(pn, pms, num("prompt_per_second")),
        predicted_n: gn as i64,
        predicted_ms: gms,
        predicted_per_second: rate(gn, gms, num("predicted_per_second")),
        ..Default::default()
    };
    let usage = o.get("usage").and_then(Value::as_object);
    let cached_tokens = usage
        .and_then(|u| u.get("prompt_tokens_details"))
        .and_then(Value::as_object)
        .and_then(|d| perf_num(d.get("cached_tokens")));
    if let Some(c) = num("cache_n") {
        turn.cached = c as i64;
    } else if let Some(c) = cached_tokens {
        turn.cached = c as i64;
    }
    if let (Some(n), Some(a)) = (num("draft_n"), num("draft_n_accepted"))
        && n as i64 > 0
    {
        turn.draft = Some(Draft {
            n: n as i64,
            accepted: a as i64,
        });
    }
    let total = usage
        .and_then(|u| perf_num(u.get("prompt_tokens")))
        .map_or(0, |f| f as i64);
    // json.Unmarshal du message entier : un octet de trop, pas de contenu.
    let content = serde_json::from_slice::<Value>(data)
        .ok()
        .and_then(|v| {
            v.get("choices")?
                .get(0)?
                .get("message")?
                .get("content")?
                .as_str()
                .map(str::to_string)
        })
        .unwrap_or_default();
    Ok(Reply {
        turn,
        total,
        content,
    })
}

// ---------------------------------------------------------------------------
// Réglages du preset
// ---------------------------------------------------------------------------

/// `benchSetup` : ce que le bench tire du preset, une fois.
pub struct Setup {
    pub cfg: Cfg,
    pub want: String,
    pub off: bool,
    pub effort: String,
    pub kwargs: Option<Map<String, Value>>,
    pub retried: bool,
    pub ub: i64,
    pub cpu_placed: bool,
    pub hybrid: bool,
    pub kv: String,
    pub kv_quant: bool,
    pub gpus: String,
    pub side_slot: bool,
}

/// `benchSetupFrom`.
pub fn setup_from(
    cfg: &Cfg,
    arg_env: &HashMap<String, String>,
    fallbacks: &EffortFallbacks,
) -> Setup {
    let get = |k: &str| cfg.get(k).map(String::as_str).unwrap_or("");
    let extra = crate::models::split_args(get("EXTRA_ARGS"));
    let want = reasoning_effort_value(get("REASONING_EFFORT"));
    let effort = fallbacks.resolve(cfg, &want);
    let off = reasoning_explicitly_off(get("REASONING")) || want == "none";
    let kwargs = reasoning_template_kwargs(off, &effort);
    let mut ub = 512;
    for v in [
        crate::serve::flag_value(&extra, &["-ub", "--ubatch-size"]),
        get("UBATCH").to_string(),
    ] {
        if let Some(n) = go_atoi(v.trim())
            && n > 0
        {
            ub = n;
            break;
        }
    }
    let cpu_placed = !crate::serve::queues::tensor_override(&extra, arg_env).is_empty();
    let (k, v, _) = crate::serve::fidelity::effective_kv_types(cfg, &extra, arg_env);
    let (kv, kv_quant) = kv_label(&k, &v);
    Setup {
        cfg: cfg.clone(),
        want,
        off,
        effort,
        kwargs,
        retried: false,
        ub,
        cpu_placed,
        // Le modèle hybride se lit dans le GGUF : posé par l'appelant, qui sait
        // où sont les modèles.
        hybrid: false,
        kv,
        kv_quant,
        gpus: get("CUDA_VISIBLE_DEVICES").trim().to_string(),
        side_slot: false,
    }
}

/// `strconv.Atoi`.
fn go_atoi(s: &str) -> Option<i64> {
    let digits = s.strip_prefix(['+', '-']).unwrap_or(s);
    if digits.is_empty() || !digits.bytes().all(|c| c.is_ascii_digit()) {
        return None;
    }
    s.parse().ok()
}

/// `benchKVLabel` : les types de cache (« f16 », « q8_0/f16 »…), quantifiés
/// ou non (rang 2 et plus de kvFidelity).
pub fn kv_label(k: &str, v: &str) -> (String, bool) {
    let norm = |s: &str| match s.trim().to_lowercase() {
        s if s.is_empty() => "f16".to_string(),
        s => s,
    };
    let (k, v) = (norm(k), norm(v));
    let (rk, _) = crate::serve::fidelity::kv_fidelity(&k);
    let (rv, _) = crate::serve::fidelity::kv_fidelity(&v);
    let label = if k == v { k } else { format!("{k}/{v}") };
    (label, rk.max(rv) >= 2)
}

impl Setup {
    /// `payload` : une requête de chat du bench.
    pub fn payload(&self, msgs: &[(String, String)], max_tokens: i64, cache: bool) -> Value {
        let messages: Vec<Value> = msgs
            .iter()
            .map(|(r, c)| json!({ "role": r, "content": c }))
            .collect();
        let mut p = Map::new();
        p.insert("model".into(), json!("loki"));
        p.insert("messages".into(), Value::Array(messages));
        p.insert("max_tokens".into(), json!(max_tokens));
        p.insert("stream".into(), json!(false));
        p.insert("temperature".into(), json!(0.7));
        p.insert("seed".into(), json!(SEED));
        p.insert("cache_prompt".into(), json!(cache));
        apply_sampling_from(&mut p, &self.cfg);
        if !self.effort.is_empty() {
            p.insert("reasoning_effort".into(), json!(self.effort));
        }
        if let Some(k) = &self.kwargs {
            p.insert("chat_template_kwargs".into(), Value::Object(k.clone()));
        }
        if self.side_slot {
            p.insert("id_slot".into(), json!(1));
        }
        Value::Object(p)
    }

    /// `ask` : un gabarit qui refuse le niveau de raisonnement est rejoué une
    /// fois avec celui qu'il accepte (ou sans le champ), traduction retenue.
    pub async fn ask(
        &mut self,
        e: &Engine,
        msgs: &[(String, String)],
        max_tokens: i64,
        cache: bool,
        learn: &(dyn Fn(&str, &str) + Send + Sync),
    ) -> Result<Reply, ReqError> {
        let r = e.chat(&self.payload(msgs, max_tokens, cache)).await;
        let body = match &r {
            Err(ReqError::Http { body, .. }) if !self.retried && !self.want.is_empty() => {
                body.clone()
            }
            _ => return r,
        };
        let Some(fixed) = effort_from_rejection(&body, &self.want) else {
            return r;
        };
        self.retried = true;
        learn(&self.want, &fixed);
        self.kwargs = reasoning_template_kwargs(self.off, &fixed);
        self.effort = fixed;
        e.chat(&self.payload(msgs, max_tokens, cache)).await
    }
}

// ---------------------------------------------------------------------------
// Le protocole
// ---------------------------------------------------------------------------

/// `benchDepthFor` : la moitié du contexte, plafonnée, et assez basse pour
/// que les tours y tiennent. Trop petit : la phase est sautée.
pub fn depth_for(n_ctx: i64, cpu_placed: bool, warm: i64) -> (i64, String) {
    if n_ctx <= 0 {
        return (0, "taille de contexte inconnue".into());
    }
    let mut d = (n_ctx / 2).min(DEPTH_MAX);
    if cpu_placed {
        d = d.min(DEPTH_CPU);
    }
    d = d.min(n_ctx - TURNS * (APPEND_TOKENS + GEN_TOKENS) - GEN_TOKENS - warm - MARGIN);
    if d < DEPTH_MIN {
        return (
            0,
            format!("contexte de {n_ctx} jetons trop petit pour la mesure en profondeur"),
        );
    }
    (d, String::new())
}

/// `benchSlice` : à partir de `off`, environ `n` octets arrêtés à une fin de
/// ligne — jamais au milieu d'un caractère. "" : corpus épuisé.
pub fn slice(text: &str, off: usize, n: i64) -> (&str, usize) {
    if off >= text.len() || n <= 0 {
        return ("", off);
    }
    let n = n as usize;
    let mut end = off.saturating_add(n);
    if end >= text.len() {
        return (&text[off..], text.len());
    }
    if let Some(i) = text.as_bytes()[off..end].iter().rposition(|&b| b == b'\n')
        && i > n / 2
    {
        end = off + i + 1;
    }
    while end > off && !text.is_char_boundary(end) {
        end -= 1;
    }
    (&text[off..end], end)
}

/// `benchLineStart` : le début de la ligne qui suit `off`.
pub fn line_start(text: &str, off: i64) -> usize {
    if off <= 0 {
        return 0;
    }
    let off = off as usize;
    if off >= text.len() {
        return text.len();
    }
    let b = text.as_bytes();
    if b[off - 1] == b'\n' {
        return off;
    }
    match b[off..].iter().position(|&c| c == b'\n') {
        Some(i) => off + i + 1,
        None => text.len(),
    }
}

/// `benchReuse` : la part du prompt précédent reprise du cache, et ce
/// qu'elle veut dire.
pub fn reuse(turns: &[Turn], hybrid: bool) -> (f64, String) {
    let (mut cached, mut expected) = (0i64, 0i64);
    let mut full = true;
    for t in turns {
        if t.cached < 0 || t.expected <= 0 {
            continue;
        }
        cached += t.cached.min(t.expected);
        expected += t.expected;
        if t.cached < t.expected - REUSE_SLACK {
            full = false;
        }
    }
    if expected == 0 {
        return (
            -1.0,
            "le moteur ne dit pas combien de jetons il reprend du cache".into(),
        );
    }
    let ratio = cached as f64 / expected as f64;
    if full {
        (ratio, String::new())
    } else if hybrid {
        (
            ratio,
            "reprise partielle : modèle hybride, le cache ne reprend qu'aux points de contrôle"
                .into(),
        )
    } else {
        (
            ratio,
            "reprise partielle : le gabarit réécrit sans doute la fin de l'historique (raisonnement retiré)"
                .into(),
        )
    }
}

/// `benchThrashHint` : un moteur qui relit beaucoup le disque pendant les
/// tours n'a pas le modèle en RAM (indication seulement).
pub fn thrash_hint(read: i64, generated: i64, rss: i64) -> String {
    if read <= 0 || generated <= 0 {
        return String::new();
    }
    let per = read * 256 / generated;
    if per <= THRASH_BYTES {
        return String::new();
    }
    format!(
        "possible thrash : {:.1} Go relus du disque par 256 jetons générés (RSS {:.1} Go) — le modèle ne tient sans doute pas en RAM",
        per as f64 / (1u64 << 30) as f64,
        rss as f64 / (1u64 << 30) as f64
    )
}

/// `benchDraftText`.
pub fn draft_text(d: Option<&Draft>) -> String {
    match d {
        Some(d) if d.n != 0 => format!(
            "   brouillon accepté {:.0} % ({}/{})",
            d.accepted as f64 * 100.0 / d.n as f64,
            d.accepted,
            d.n
        ),
        _ => String::new(),
    }
}

/// `time.Duration.String` du Go, exact (« 150ms », « 1.5µs », « 6m0s »).
pub fn go_dur(d: Duration) -> String {
    let u = d.as_nanos();
    if u == 0 {
        return "0s".into();
    }
    // La fraction de v / 10^prec, zéros de fin omis (sans point si nulle).
    let frac = |v: u128, prec: u32| -> (u128, String) {
        let p = 10u128.pow(prec);
        let f = v % p;
        if f == 0 {
            return (v / p, String::new());
        }
        let digits = format!("{f:0width$}", width = prec as usize);
        (v / p, format!(".{}", digits.trim_end_matches('0')))
    };
    if u < 1_000_000_000 {
        let (unit, prec) = match u {
            u if u < 1_000 => ("ns", 0),
            u if u < 1_000_000 => ("µs", 3),
            _ => ("ms", 6),
        };
        let (i, f) = frac(u, prec);
        return format!("{i}{f}{unit}");
    }
    let (secs, f) = frac(u, 9);
    let (h, m, s) = (secs / 3600, secs / 60 % 60, secs % 60);
    match (h, m) {
        (0, 0) => format!("{s}{f}s"),
        (0, m) => format!("{m}m{s}{f}s"),
        (h, m) => format!("{h}h{m}m{s}{f}s"),
    }
}

/// La lecture disque et la RSS du moteur (`engineIOSample`) ; `None` hors
/// Linux ou sans moteur.
pub type IoSample<'a> = &'a (dyn Fn() -> Option<(i64, i64)> + Send + Sync);

/// `benchRun` : le protocole contre un moteur. `Err` = une phase de mesure a
/// échoué, rien à enregistrer ; une phase en profondeur interrompue par son
/// budget ou par la taille du contexte rend un résultat PARTIEL.
pub async fn run(
    e: &Engine,
    mut opts: Opts,
    s: &mut Setup,
    corp: &Corpora,
    progress: &(dyn Fn(&str, i64, i64) + Send + Sync),
    learn: &(dyn Fn(&str, &str) + Send + Sync),
    io: IoSample<'_>,
) -> Result<BenchResult, String> {
    if opts.prompt <= 0 {
        opts.prompt = 2000;
    }
    if opts.predict <= 0 {
        opts.predict = 300;
    }
    e.idle().await?;
    let steps = if opts.full() { 2 + 1 + TURNS } else { 2 };
    let step = std::sync::atomic::AtomicI64::new(0);
    let next = |phase: &str| {
        let n = step.fetch_add(1, std::sync::atomic::Ordering::SeqCst) + 1;
        progress(phase, n, steps);
    };
    let t0 = std::time::Instant::now();
    let (mut n_ctx, build) = e.props().await;
    if n_ctx <= 0 {
        n_ctx = go_atoi(s.cfg.get("CTX").map(String::as_str).unwrap_or("").trim()).unwrap_or(0);
    }
    let mode = if opts.full() { MODE_FULL } else { MODE_QUICK };
    let mut res = BenchResult {
        mode: mode.into(),
        protocol: PROTOCOL,
        kv: s.kv.clone(),
        kv_quantized: s.kv_quant,
        engine: build,
        gpus: s.gpus.clone(),
        ..Default::default()
    };
    let code = corp.code.as_str();
    let (sample, _) = slice(code, 0, SAMPLE_BYTES as i64);
    let code_ratio = e.bytes_per_token(sample).await;
    let prose_ratio = e.bytes_per_token(&corp.prose).await;

    // 1. Préchauffage, jeté.
    next("préchauffage");
    let mut warm_tok = 2 * s.ub;
    if n_ctx > 0 {
        warm_tok = warm_tok.min(n_ctx / 4).max(64);
    }
    let warm = warm_tok + WARM_GEN;
    let from = line_start(
        code,
        code.len() as i64 - (warm_tok as f64 * code_ratio) as i64,
    );
    let (warm_text, _) = slice(code, from, code.len() as i64);
    let msgs = vec![("user".to_string(), format!("{warm_text}\n\n{ASK_FIRST}"))];
    if let Err(err) = s.ask(e, &msgs, WARM_GEN, false, learn).await {
        return Err(format!("préchauffage : {err}"));
    }

    // 2. La ligne courte : de la prose, puis du code inédit.
    next(&format!(
        "ligne courte ({} + {} jetons)",
        opts.prompt, opts.predict
    ));
    let mut prompt = corp.prose.clone();
    let want = (opts.prompt as f64 * prose_ratio) as i64;
    if want < prompt.len() as i64 {
        prompt = slice(&corp.prose, 0, want).0.to_string();
    } else {
        let rest = opts.prompt - (prompt.len() as f64 / prose_ratio) as i64;
        if rest > 0 {
            let (chunk, _) = slice(
                code,
                line_start(code, (code.len() / 2) as i64),
                (rest as f64 * code_ratio) as i64,
            );
            prompt = format!("{prompt}\n\n{chunk}");
        }
    }
    let msgs = vec![("user".to_string(), format!("{}{ASK_PROSE}", prompt.trim()))];
    let quick = s
        .ask(e, &msgs, opts.predict, false, learn)
        .await
        .map_err(|err| format!("ligne courte : {err}"))?;
    let q = quick.turn;
    res.prompt_n = q.new;
    res.prompt_ms = q.prompt_ms;
    res.prompt_per_second = q.prompt_per_second;
    res.predicted_n = q.predicted_n;
    res.predicted_ms = q.predicted_ms;
    res.predicted_per_second = q.predicted_per_second;
    res.draft = q.draft;
    if !opts.full() {
        res.elapsed = t0.elapsed().as_secs_f64();
        return Ok(res);
    }

    // 3. La profondeur.
    let (mut d, mut why) = depth_for(n_ctx, s.cpu_placed, warm);
    if opts.depth > 0 {
        d = opts.depth;
        why = String::new();
    }
    let mut depth = Depth {
        ctx: n_ctx,
        target: d,
        reuse: -1.0,
        ..Default::default()
    };
    if !why.is_empty() {
        depth.skipped = why;
        res.depth = Some(depth);
        res.elapsed = t0.elapsed().as_secs_f64();
        return Ok(res);
    }
    depth_run(e, s, code, code_ratio, &mut depth, &next, learn, io).await?;
    res.depth = Some(depth);
    res.elapsed = t0.elapsed().as_secs_f64();
    Ok(res)
}

/// `benchDepthRun` : prefill à froid à D, puis trois tours qui prolongent la
/// même discussion avec du code jamais vu.
#[allow(clippy::too_many_arguments)]
async fn depth_run(
    e: &Engine,
    s: &mut Setup,
    code: &str,
    ratio: f64,
    depth: &mut Depth,
    next: &(dyn Fn(&str) + Send + Sync),
    learn: &(dyn Fn(&str, &str) + Send + Sync),
    io: IoSample<'_>,
) -> Result<(), String> {
    next(&format!("prefill à froid ({} jetons)", depth.target));
    let (prefix, mut off) = slice(code, 0, (depth.target as f64 * ratio) as i64);
    let mut msgs = vec![(
        "user".to_string(),
        format!(
            "Voici un fichier source de l'interface web de Loki :\n\n```html\n{prefix}\n```\n\n{ASK_FIRST}"
        ),
    )];
    let cold = match s.ask(e, &msgs, GEN_TOKENS, false, learn).await {
        Err(ReqError::Timeout(_)) => {
            depth.partial = format!("prefill à froid interrompu après {}", go_dur(e.req_timeout));
            return Ok(());
        }
        Err(err) => return Err(format!("prefill à froid : {err}")),
        Ok(r) => r,
    };
    depth.cold = Some(cold.turn.clone());
    let (mut prev, mut answer) = (cold.prompt_total(), cold.content.clone());
    // Relectures du disque comptées sur les TOURS seulement.
    let before = io();
    let (mut new_n, mut generated) = (0i64, 0i64);
    let (mut prompt_ms, mut gen_ms) = (0f64, 0f64);
    for r in 1..=TURNS {
        if prev + 2 * GEN_TOKENS + APPEND_TOKENS * 5 / 4 + MARGIN > depth.ctx {
            depth.partial = format!("contexte plein après {} tour(s)", r - 1);
            break;
        }
        let (chunk, next_off) = slice(code, off, (APPEND_TOKENS as f64 * ratio) as i64);
        if chunk.is_empty() {
            depth.partial = format!("corpus épuisé après {} tour(s)", r - 1);
            break;
        }
        off = next_off;
        next(&format!("tour {r}/{TURNS} en profondeur"));
        msgs.push(("assistant".to_string(), answer.clone()));
        msgs.push((
            "user".to_string(),
            format!("Suite du fichier :\n\n```html\n{chunk}\n```\n\n{ASK_NEXT}"),
        ));
        let rep = match s.ask(e, &msgs, GEN_TOKENS, true, learn).await {
            Err(ReqError::Timeout(_)) => {
                depth.partial = format!("tour {r} interrompu après {}", go_dur(e.req_timeout));
                break;
            }
            Err(err) => return Err(format!("tour {r} en profondeur : {err}")),
            Ok(rep) => rep,
        };
        let mut t = rep.turn.clone();
        t.expected = prev;
        depth.draft = draft_add(depth.draft, t.draft);
        new_n += t.new;
        prompt_ms += t.prompt_ms;
        generated += t.predicted_n;
        gen_ms += t.predicted_ms;
        depth.turns.push(t);
        prev = rep.prompt_total();
        answer = rep.content;
    }
    if prompt_ms > 0.0 {
        depth.cached_per_second = new_n as f64 / (prompt_ms / 1000.0);
    }
    if gen_ms > 0.0 {
        depth.decode_per_second = generated as f64 / (gen_ms / 1000.0);
    }
    (depth.reuse, depth.reuse_note) = reuse(&depth.turns, s.hybrid);
    if let (Some((io0, rss0)), Some((io1, rss1))) = (before, io())
        && generated > 0
    {
        depth.hint = thrash_hint(io1 - io0, generated, rss0.max(rss1));
    }
    Ok(())
}

#[cfg(test)]
mod tests;
