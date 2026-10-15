//! Export, mode Code et critères de la discussion, tenus par la façade quand
//! elle tient la conversation (`LOKI_CHAT=rust`, lot 19 de la migration) :
//!
//! - `/api/chat/export` — port de `chat_export.go` : Markdown (fait pour être
//!   lu) ou JSON (fait pour être retraité), mêmes options pour les deux
//!   (raisonnements, outils, sorties, derniers échanges), tirés de la
//!   conversation VIVANTE (le Go relisait la copie enregistrée) ;
//! - `/api/chat/mode` — `handleChatMode` (`code_mode.go`) : mode de la
//!   discussion ouverte (`chat/mode:<id>`) et ses critères ;
//! - `/api/chat/criteria` — `handleChatCriteria` : critères édités à la main
//!   (`chat/crit:<id>`, scellés comme le contenu des discussions).
//!
//! Les changements partent sur le fil de la façade (deltas `mode`,
//! `criteria`), comme le Go les lui envoyait. Le reste du mode Code (indice,
//! travaux de fond, outil `criteria`) reste au Go, qui lit les mêmes clés.

use std::time::{SystemTime, UNIX_EPOCH};

use http_body_util::{BodyExt, Full, Limited};
use hyper::body::Incoming;
use hyper::header::{CONTENT_DISPOSITION, CONTENT_LENGTH, CONTENT_TYPE, HeaderValue};
use hyper::{Method, Request, Response, StatusCode};
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value, json};

use crate::api::Ctx;
use crate::auth::json as send_json;
use crate::chat::compact::{is_loki_injected, strip_context_updates};
use crate::chat::generate::ChatService;
use crate::chat::journal::coalesce_replay;
use crate::chat::persist::BK_CHAT;
use crate::chat::types::{LogEvent, Message};
use crate::gojson::{self, Kind};
use crate::proxy::Body;

// ---------------------------------------------------------------------------
// Export
// ---------------------------------------------------------------------------

/// `exportOpts`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ExportOpts {
    /// « md » ou « json ».
    pub format: String,
    pub reasoning: bool,
    pub tools: bool,
    pub results: bool,
    /// 0 = tout le fil ; N = les N derniers échanges.
    pub turns: i64,
}

impl Default for ExportOpts {
    /// `defaultExportOpts`.
    fn default() -> Self {
        ExportOpts {
            format: "md".into(),
            reasoning: true,
            tools: true,
            results: true,
            turns: 0,
        }
    }
}

/// `exportOptsFromQuery` : toute clé absente garde sa valeur par défaut (la
/// première valeur d'une clé répétée compte).
pub fn opts_from_query(query: &str) -> ExportOpts {
    let mut first: std::collections::HashMap<String, String> = std::collections::HashMap::new();
    for (k, v) in url::form_urlencoded::parse(query.as_bytes()) {
        first
            .entry(k.into_owned())
            .or_insert_with(|| v.into_owned());
    }
    let mut o = ExportOpts::default();
    let flag = |k: &str, cur: bool| match first.get(k) {
        None => cur,
        Some(v) => v == "1" || v == "true" || v == "on",
    };
    if first.get("format").is_some_and(|v| v == "json") {
        o.format = "json".into();
    }
    o.reasoning = flag("reasoning", o.reasoning);
    o.tools = flag("tools", o.tools);
    o.results = flag("results", o.results);
    if let Some(n) = first
        .get("turns")
        .and_then(|v| go_atoi(v))
        .filter(|n| *n > 0)
    {
        o.turns = n;
    }
    // Sans les bulles d'outils, leur sortie n'a nulle part où aller.
    if !o.tools {
        o.results = false;
    }
    o
}

/// `strconv.Atoi` (signe optionnel, chiffres ASCII seulement).
fn go_atoi(s: &str) -> Option<i64> {
    let digits = s.strip_prefix(['+', '-']).unwrap_or(s);
    if digits.is_empty() || !digits.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    s.parse().ok()
}

/// `filterLog` : les mêmes cases que le Markdown, appliquées au journal (sur
/// une copie : les événements appartiennent à la conversation vivante).
pub fn filter_log(log: &[LogEvent], o: &ExportOpts) -> Vec<LogEvent> {
    if o.reasoning && o.tools && o.results {
        return log.to_vec();
    }
    let mut out = Vec::with_capacity(log.len());
    for ev in log {
        if ev.delta.contains_key("reasoning_content") && !o.reasoning {
            continue;
        }
        if let Some(Value::Object(tu)) = ev.delta.get("tool_used") {
            if !o.tools {
                continue;
            }
            if !o.results {
                let mut cp = tu.clone();
                cp.remove("result");
                let mut ev = ev.clone();
                ev.delta.insert("tool_used".into(), Value::Object(cp));
                out.push(ev);
                continue;
            }
        }
        out.push(ev.clone());
    }
    out
}

/// `lastTurnsMessages` : les `n` derniers échanges, un échange commençant à un
/// message `user` (un rappel de Loki n'en ouvre pas).
pub fn last_turns_messages(msgs: &[Message], n: i64) -> Vec<Message> {
    let mut seen = 0;
    for i in (0..msgs.len()).rev() {
        if msgs[i].role != "user" || is_loki_injected(&msgs[i]) {
            continue;
        }
        seen += 1;
        if seen == n {
            return msgs[i..].to_vec();
        }
    }
    msgs.to_vec()
}

/// `lastTurnsLog` : idem sur le journal, où un échange commence à un événement
/// `user`.
pub fn last_turns_log(log: &[LogEvent], n: i64) -> Vec<LogEvent> {
    let mut seen = 0;
    for i in (0..log.len()).rev() {
        if !log[i].delta.contains_key("user") {
            continue;
        }
        seen += 1;
        if seen == n {
            return log[i..].to_vec();
        }
    }
    log.to_vec()
}

/// `elideDataURI` : l'en-tête « data:<mime>;base64, » gardé, le corps
/// remplacé par sa taille.
pub fn elide_data_uri(url: &str) -> String {
    match url.find(',') {
        Some(i) => format!(
            "{}<élidé — {}>",
            &url[..=i],
            crate::downloads::human_bytes((url.len() - i - 1) as i64)
        ),
        None => "<image élidée>".into(),
    }
}

/// `stripInlineImages` : les images base64 des messages multimodaux élidées
/// (un export JSON embarquait plusieurs Mo par image).
pub fn strip_inline_images(msgs: Vec<Message>) -> Vec<Message> {
    msgs.into_iter()
        .map(|mut m| {
            if let Some(Value::Array(parts)) = &mut m.content {
                for p in parts.iter_mut() {
                    if let Value::Object(part) = p
                        && let Some(Value::Object(iu)) = part.get_mut("image_url")
                        && let Some(Value::String(url)) = iu.get("url")
                        && url.starts_with("data:")
                    {
                        let e = elide_data_uri(url);
                        iu.insert("url".into(), Value::String(e));
                    }
                }
            }
            m
        })
        .collect()
}

/// `exportNote` : ce que l'export ne contient PAS, en une ligne.
pub fn export_note(o: &ExportOpts) -> String {
    let mut parts = Vec::new();
    if o.turns > 0 {
        parts.push(format!("{} derniers échanges seulement", o.turns));
    }
    if !o.reasoning {
        parts.push("raisonnements retirés".into());
    }
    if !o.tools {
        parts.push("appels d'outils retirés".into());
    } else if !o.results {
        parts.push("sorties d'outils retirées".into());
    }
    if parts.is_empty() {
        return String::new();
    }
    format!("_Export partiel : {}._", parts.join(", "))
}

/// `mdText`.
fn md_text(v: Option<&Value>) -> String {
    v.and_then(Value::as_str)
        .unwrap_or("")
        .trim_end_matches('\n')
        .to_string()
}

/// `mdInline` : ce qui casserait une ligne Markdown neutralisé.
pub fn md_inline(s: &str) -> String {
    let s = s.replace('\n', " ");
    let mut out = String::with_capacity(s.len());
    for c in s.chars() {
        if matches!(c, '*' | '_' | '`' | '[' | ']') {
            out.push('\\');
        }
        out.push(c);
    }
    out.trim().to_string()
}

/// `exportFileNames`.
fn export_file_names(v: Option<&Value>) -> Vec<String> {
    match v {
        Some(Value::Array(files)) => files
            .iter()
            .filter_map(|f| f.get("name").and_then(Value::as_str))
            .filter(|n| !n.is_empty())
            .map(str::to_string)
            .collect(),
        _ => Vec::new(),
    }
}

fn present<'a>(ev: &'a Map<String, Value>, k: &str) -> Option<&'a Value> {
    ev.get(k).filter(|v| !v.is_null())
}

/// `ExportMarkdown` : un bloc par tour, le raisonnement replié dans un
/// `<details>`, les outils en liste. `date` : « 02/01/2006 à 15:04 ».
pub fn export_markdown(log: &[LogEvent], o: &ExportOpts, date: &str, version: &str) -> String {
    let snapshot = if o.turns > 0 {
        last_turns_log(log, o.turns)
    } else {
        log.to_vec()
    };
    let mut b = format!("# Conversation Loki\n\nExportée le {date} par Loki {version}.\n");
    let note = export_note(o);
    if !note.is_empty() {
        b.push_str(&format!("\n{note}\n"));
    }
    let mut open_bubble = false;
    let head = |b: &mut String, open: &mut bool| {
        if !*open {
            b.push_str("\n## Loki\n");
            *open = true;
        }
    };
    for ev in coalesce_replay(&snapshot, 0) {
        if let Some(u) = present(&ev, "user") {
            open_bubble = false;
            b.push_str(&format!("\n---\n\n## Vous\n\n{}\n", md_text(Some(u))));
            let names = export_file_names(ev.get("files"));
            if !names.is_empty() {
                b.push_str(&format!("\nFichiers joints : {}\n", names.join(", ")));
            }
        } else if let Some(r) = present(&ev, "reasoning_content") {
            if o.reasoning {
                head(&mut b, &mut open_bubble);
                b.push_str(&format!(
                    "\n<details>\n<summary>Raisonnement</summary>\n\n{}\n\n</details>\n",
                    md_text(Some(r))
                ));
            }
        } else if let Some(c) = present(&ev, "content") {
            head(&mut b, &mut open_bubble);
            b.push_str(&format!("\n{}\n", md_text(Some(c))));
        } else if let Some(tu) = present(&ev, "tool_used") {
            if !o.tools {
                continue;
            }
            let Value::Object(tu) = tu else {
                continue;
            };
            head(&mut b, &mut open_bubble);
            let name = tu.get("name").and_then(Value::as_str).unwrap_or("");
            let label = match tu.get("label").and_then(Value::as_str).unwrap_or("") {
                "" => name,
                l => l,
            };
            b.push_str(&format!(
                "\n> 🔧 **{}** — {}\n",
                md_inline(name),
                md_inline(label)
            ));
            if let Some(res) = tu.get("result").and_then(Value::as_str)
                && o.results
                && !res.trim().is_empty()
            {
                b.push_str(&format!("\n```\n{}\n```\n", res.trim_end_matches('\n')));
            }
        } else if present(&ev, "compacted").is_some() {
            b.push_str("\n_(contexte compacté à cet endroit : les tours précédents ont été résumés pour le modèle, le fil ci-dessus reste complet)_\n");
        }
    }
    if !open_bubble && snapshot.is_empty() {
        b.push_str("\n_Conversation vide._\n");
    }
    b
}

#[derive(Serialize)]
struct ExportPayload<'a> {
    loki_version: &'a str,
    exported_at: &'a str,
    ctx_used: i64,
    messages: Vec<Message>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    log: Vec<LogEvent>,
}

/// `json.MarshalIndent(v, "", "  ")` : indenté de deux espaces, HTML échappé.
pub fn marshal_indent<T: Serialize>(v: &T) -> Vec<u8> {
    let s = serde_json::to_string_pretty(v).unwrap_or_default();
    let mut out = String::with_capacity(s.len());
    for c in s.chars() {
        match c {
            '<' => out.push_str("\\u003c"),
            '>' => out.push_str("\\u003e"),
            '&' => out.push_str("\\u0026"),
            '\u{2028}' => out.push_str("\\u2028"),
            '\u{2029}' => out.push_str("\\u2029"),
            c => out.push(c),
        }
    }
    out.into_bytes()
}

/// `ExportJSON` : la vue modèle (sans mises à jour de contexte ni images
/// base64) et le journal d'affichage filtré.
pub fn export_json(
    messages: &[Message],
    log: &[LogEvent],
    ctx_used: i64,
    o: &ExportOpts,
    exported_at: &str,
    version: &str,
) -> Vec<u8> {
    let (msgs, log) = if o.turns > 0 {
        (
            last_turns_messages(messages, o.turns),
            last_turns_log(log, o.turns),
        )
    } else {
        (messages.to_vec(), log.to_vec())
    };
    let p = ExportPayload {
        loki_version: version,
        exported_at,
        ctx_used,
        messages: strip_inline_images(strip_context_updates(msgs)),
        log: filter_log(&log, o),
    };
    marshal_indent(&p)
}

/// Heure locale : (année, mois, jour, heure, minute, seconde, décalage en s).
fn local_now() -> (i64, u32, u32, u32, u32, u32, i64) {
    let secs = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| d.as_secs() as i64);
    #[cfg(unix)]
    {
        let t = secs as libc::time_t;
        let mut tm: libc::tm = unsafe { std::mem::zeroed() };
        // SAFETY : pointeurs valides, structure de sortie initialisée.
        if !unsafe { libc::localtime_r(&t, &mut tm) }.is_null() {
            return (
                i64::from(tm.tm_year) + 1900,
                (tm.tm_mon + 1) as u32,
                tm.tm_mday as u32,
                tm.tm_hour as u32,
                tm.tm_min as u32,
                tm.tm_sec as u32,
                tm.tm_gmtoff as i64,
            );
        }
    }
    let days = secs.div_euclid(86400);
    let rem = secs.rem_euclid(86400);
    let (y, m, d) = civil(days);
    (
        y,
        m,
        d,
        (rem / 3600) as u32,
        (rem % 3600 / 60) as u32,
        (rem % 60) as u32,
        0,
    )
}

/// Jours depuis 1970 → (année, mois, jour).
fn civil(days: i64) -> (i64, u32, u32) {
    let z = days + 719468;
    let era = z.div_euclid(146097);
    let doe = z.rem_euclid(146097);
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = (doy - (153 * mp + 2) / 5 + 1) as u32;
    let m = if mp < 10 { mp + 3 } else { mp - 9 } as u32;
    (era * 400 + yoe + i64::from(m <= 2), m, d)
}

/// `time.RFC3339` en heure locale (« Z » à l'heure UTC, comme le Go).
fn rfc3339_now() -> String {
    let (y, mo, d, h, mi, s, off) = local_now();
    let zone = if off == 0 {
        "Z".to_string()
    } else {
        let sign = if off < 0 { '-' } else { '+' };
        let a = off.abs();
        format!("{sign}{:02}:{:02}", a / 3600, a % 3600 / 60)
    };
    format!("{y:04}-{mo:02}-{d:02}T{h:02}:{mi:02}:{s:02}{zone}")
}

/// « 02/01/2006 à 15:04 ».
fn fr_date_now() -> String {
    let (y, mo, d, h, mi, _, _) = local_now();
    format!("{d:02}/{mo:02}/{y:04} à {h:02}:{mi:02}")
}

/// `exportFilename` : horodaté pour que deux exports ne s'écrasent pas.
fn export_filename(ext: &str) -> String {
    let (y, mo, d, h, mi, _, _) = local_now();
    format!("loki-conversation-{y:04}-{mo:02}-{d:02}-{h:02}{mi:02}.{ext}")
}

// ---------------------------------------------------------------------------
// Mode et critères
// ---------------------------------------------------------------------------

/// `Criterion`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, Default)]
pub struct Criterion {
    pub id: i64,
    pub text: String,
    pub status: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub note: String,
}

/// `critMaxCount`.
pub const CRIT_MAX_COUNT: usize = 20;

fn mode_key(id: &str) -> String {
    format!("mode:{id}")
}

fn crit_key(id: &str) -> String {
    format!("crit:{id}")
}

/// Nom de type JSON d'une valeur, comme `UnmarshalTypeError.Value`.
fn json_kind(v: &Value) -> &'static str {
    match v {
        Value::String(_) => "string",
        Value::Number(_) => "number",
        Value::Bool(_) => "bool",
        Value::Array(_) => "array",
        Value::Object(_) => "object",
        Value::Null => "null",
    }
}

const MODE_REQ_TYPE: &str = r#"struct { Mode string "json:\"mode\"" }"#;
const CRIT_REQ_TYPE: &str = r#"struct { Criteria []loki.Criterion "json:\"criteria\"" }"#;

/// Décode `{criteria: []Criterion}` comme `json.NewDecoder(r.Body).Decode`
/// (liste absente ou nulle : vide).
pub fn decode_criteria(body: &[u8]) -> Result<Vec<Criterion>, String> {
    let d = gojson::decode_struct(body, CRIT_REQ_TYPE, &[("criteria", Kind::Any)]);
    if let Some(e) = d.err.clone() {
        return Err(e);
    }
    let Some(raw) = d.fields.get("criteria") else {
        return Ok(Vec::new());
    };
    let items = match raw {
        Value::Null => return Ok(Vec::new()),
        Value::Array(a) => a.clone(),
        v => {
            return Err(format!(
                "json: cannot unmarshal {} into Go struct field .criteria of type []loki.Criterion",
                json_kind(v)
            ));
        }
    };
    let mut out = Vec::with_capacity(items.len());
    let mut first_err: Option<String> = None;
    for it in items {
        match it {
            Value::Null => out.push(Criterion::default()),
            Value::Object(_) => {
                let b = serde_json::to_vec(&it).unwrap_or_default();
                let d = gojson::decode_struct(
                    &b,
                    "Criterion",
                    &[
                        ("id", Kind::Int),
                        ("text", Kind::Str),
                        ("status", Kind::Str),
                        ("note", Kind::Str),
                    ],
                );
                if let Some(e) = d.err.clone()
                    && first_err.is_none()
                {
                    first_err = Some(e.replacen(
                        "Go struct field Criterion.",
                        "Go struct field Criterion.criteria.",
                        1,
                    ));
                }
                out.push(Criterion {
                    id: d.int("id"),
                    text: d.str("text"),
                    status: d.str("status"),
                    note: d.str("note"),
                });
            }
            v => {
                if first_err.is_none() {
                    first_err = Some(format!(
                        "json: cannot unmarshal {} into Go struct field .criteria of type loki.Criterion",
                        json_kind(&v)
                    ));
                }
                out.push(Criterion::default());
            }
        }
    }
    match first_err {
        Some(e) => Err(e),
        None => Ok(out),
    }
}

/// `handleChatCriteria` : vides écartés, vingt au plus, ids posés,
/// statuts ramenés à pending/passed/failed.
pub fn clean_criteria(list: Vec<Criterion>) -> Vec<Criterion> {
    let mut clean: Vec<Criterion> = Vec::new();
    let mut next = 1;
    for mut c in list {
        if c.text.trim().is_empty() || clean.len() >= CRIT_MAX_COUNT {
            continue;
        }
        if c.id <= 0 {
            c.id = next;
        }
        if c.id >= next {
            next = c.id + 1;
        }
        if !matches!(c.status.as_str(), "pending" | "passed" | "failed") {
            c.status = "pending".into();
        }
        clean.push(c);
    }
    clean
}

/// `critList` : `null` sans critères.
async fn crit_list(ctx: &Ctx, conv_id: &str) -> Value {
    match ctx.store.get_open(BK_CHAT, &crit_key(conv_id)).await {
        Ok(Some(b)) if !b.is_empty() => serde_json::from_slice(&b).unwrap_or(Value::Null),
        _ => Value::Null,
    }
}

fn now_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| d.as_millis() as i64)
}

// ---------------------------------------------------------------------------
// Routes
// ---------------------------------------------------------------------------

/// Les routes servies par ce module (avec la conversation).
pub fn handles(path: &str) -> bool {
    matches!(
        path,
        "/api/chat/export" | "/api/chat/mode" | "/api/chat/criteria"
    )
}

fn fail(code: StatusCode, e: impl Into<String>) -> Response<Body> {
    send_json(code, &json!({ "ok": false, "error": e.into() }))
}

/// `from_go` : remise par le Go (tunnel), déjà authentifiée par lui.
pub async fn serve(
    ctx: &Ctx,
    chat: &ChatService,
    req: Request<Incoming>,
    from_go: bool,
) -> Response<Body> {
    let (parts, body) = req.into_parts();
    let version = if from_go {
        ctx.internal
            .auth()
            .await
            .map(|i| i.version)
            .unwrap_or_default()
    } else {
        match ctx.guard(&parts.headers).await {
            Ok(v) => v,
            Err(refus) => return *refus,
        }
    };
    let conv = chat.sessions.conv();
    let conv_id = conv.id();
    let read = || async {
        Limited::new(body, 1 << 20)
            .collect()
            .await
            .map(|b| b.to_bytes().to_vec())
            .unwrap_or_default()
    };
    match parts.uri.path() {
        "/api/chat/export" => {
            let o = opts_from_query(parts.uri.query().unwrap_or(""));
            let (messages, log, ctx_used) =
                conv.with_state(|st| (st.messages.clone(), st.log.clone(), st.ctx_used));
            let (body, ctype, ext) = if o.format == "json" {
                (
                    export_json(&messages, &log, ctx_used, &o, &rfc3339_now(), &version),
                    "application/json; charset=utf-8",
                    "json",
                )
            } else {
                (
                    export_markdown(&log, &o, &fr_date_now(), &version).into_bytes(),
                    "text/markdown; charset=utf-8",
                    "md",
                )
            };
            let len = body.len();
            let mut r = Response::new(
                Full::new(bytes::Bytes::from(body))
                    .map_err(|n| match n {})
                    .boxed(),
            );
            let h = r.headers_mut();
            h.insert(CONTENT_TYPE, HeaderValue::from_static(ctype));
            if let Ok(v) = HeaderValue::from_str(&format!(
                "attachment; filename=\"{}\"",
                export_filename(ext)
            )) {
                h.insert(CONTENT_DISPOSITION, v);
            }
            h.insert(CONTENT_LENGTH, HeaderValue::from(len));
            r
        }
        "/api/chat/mode" if parts.method == Method::POST => {
            let raw = read().await;
            let d = gojson::decode_struct(&raw, MODE_REQ_TYPE, &[("mode", Kind::Str)]);
            if let Some(e) = d.err.clone() {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            let mode = d.str("mode");
            if mode != "code" && mode != "chat" {
                return fail(StatusCode::BAD_REQUEST, "mode inconnu (chat|code)");
            }
            // chat = défaut = clé absente.
            let res = if mode == "code" {
                ctx.store
                    .put(BK_CHAT, &mode_key(&conv_id), b"code".to_vec())
                    .await
            } else {
                ctx.store.del(BK_CHAT, &mode_key(&conv_id)).await
            };
            if let Err(e) = res {
                return fail(StatusCode::INTERNAL_SERVER_ERROR, e);
            }
            let mut d = Map::new();
            d.insert("mode".into(), json!(mode));
            chat.push_delta(d);
            send_json(StatusCode::OK, &json!({ "ok": true, "mode": mode }))
        }
        "/api/chat/mode" => {
            let code = ctx.store.get_str(BK_CHAT, &mode_key(&conv_id)).await == "code";
            send_json(
                StatusCode::OK,
                &json!({
                    "ok": true,
                    "mode": if code { "code" } else { "chat" },
                    "criteria": crit_list(ctx, &conv_id).await,
                }),
            )
        }
        "/api/chat/criteria" => {
            if parts.method != Method::POST {
                return fail(StatusCode::METHOD_NOT_ALLOWED, "POST attendu");
            }
            let raw = read().await;
            let list = match decode_criteria(&raw) {
                Ok(l) => l,
                Err(e) => return fail(StatusCode::BAD_REQUEST, e),
            };
            let clean = clean_criteria(list);
            // critSave : une liste vide supprime la clé.
            let _ = if clean.is_empty() {
                ctx.store.del(BK_CHAT, &crit_key(&conv_id)).await
            } else {
                ctx.store
                    .put_seal(
                        BK_CHAT,
                        &crit_key(&conv_id),
                        gojson::marshal(&clean).into_bytes(),
                    )
                    .await
                    .map_err(|e| format!("{e:?}"))
            };
            // critNotify.
            let mut d = Map::new();
            d.insert("criteria".into(), json!(clean));
            d.insert("ts".into(), json!(now_ms()));
            chat.push_delta(d);
            let out = if clean.is_empty() {
                Value::Null
            } else {
                json!(clean)
            };
            send_json(StatusCode::OK, &json!({ "ok": true, "criteria": out }))
        }
        _ => fail(StatusCode::NOT_FOUND, "route inconnue"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn atoi_comme_go() {
        assert_eq!(go_atoi("12"), Some(12));
        assert_eq!(go_atoi("+3"), Some(3));
        assert_eq!(go_atoi("-2"), Some(-2));
        assert_eq!(go_atoi("1.5"), None);
        assert_eq!(go_atoi(" 1"), None);
        assert_eq!(go_atoi(""), None);
    }

    #[test]
    fn indentation_html_echappee() {
        let b = marshal_indent(&json!({"a": "<b> & c", "l": []}));
        assert_eq!(
            String::from_utf8(b).unwrap(),
            "{\n  \"a\": \"\\u003cb\\u003e \\u0026 c\",\n  \"l\": []\n}"
        );
    }
}
