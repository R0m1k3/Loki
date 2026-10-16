//! Analyse du flux SSE renvoyé par le moteur — port de la partie « protocole »
//! de `runChatLoop` (`llm_client.go`, lecture du flux ~l. 1695–2117), de
//! `streamChunk`, `toolCallLabel`, `argPreview`/`previewArgDone`, `bodyTail`,
//! de la télémétrie tolérante (`perfWire`, `perf_log.go`), de la détection des
//! appels d'outils écrits en texte (`code_retry.go`) et de `repairToolCall`
//! (`code_alias.go`).
//!
//! Machine à états PURE et incrémentale : ni HTTP, ni conversation, ni horloge
//! implicite. Une instance de [`StreamParser`] = une complétion (une tentative
//! de la boucle Go). On lui donne les lignes du corps une à une
//! ([`StreamParser::feed_line`]), puis [`StreamParser::finish`] rend le bilan
//! ([`StreamOutcome`]). Les événements partent dans un rappel qui, comme le
//! `ChatCallback` Go, renvoie `false` pour interrompre le flux.
//!
//! Ce qui reste à la boucle : requête HTTP, statut non-200, lecture et
//! découpage des lignes (`bufio.Scanner`, plafond de 8 Mio : voir
//! [`MAX_LINE_BYTES`]), erreur de lecture (`sc.Err()`), reprises, compactage,
//! télémétrie `perfRecord`, exécution des outils.
//!
//! État global Go devenu paramètre explicite (voir [`StreamConfig`]) :
//! `REASONING` (`reasoning_on`), outils annoncés/coupés/neutralisés
//! (`calls_on`, `tool_choice_none`), preset externe (`external`), politique
//! REASONING_ECHO (`echo_on`, `echo_keeps_past`), compteur de relances
//! « appel textuel » (`pattern_retries`), surcharge `retry_patterns` de la base
//! (`patterns`), instant d'envoi de la requête (`t_req`), `noteEnginePrompt`
//! (`cache_hint`) et la garde d'écriture du mode Code `codeWriteGuard`
//! (`preflight`).

use std::collections::{BTreeMap, HashSet};
use std::sync::{Arc, OnceLock};
use std::time::Instant;

use serde_json::{Map, Value};

use super::types::{StatsEvent, StreamEvent, Tool, ToolCall, ToolUsedEvent};

/// Plafond d'une ligne du flux (`sc.Buffer(…, 8<<20)` en Go). Au-delà, la
/// lecture échoue (`bufio.ErrTooLong`) : c'est à la boucle de le détecter.
pub const MAX_LINE_BYTES: usize = 8 << 20;

/// Balise fermante d'un raisonnement laissé en ligne par le moteur.
pub const THINK_CLOSE: &str = "</think>";

/// Ce qu'un événement de frappe montre du corps : 40 dernières lignes…
pub const BODY_TAIL_LINES: usize = 40;
/// … et 4 Kio au plus.
pub const BODY_TAIL_BYTES: usize = 4096;

/// Relances « appel écrit en texte » consécutives (`maxPatternRetries`).
pub const MAX_PATTERN_RETRIES: i64 = 3;

/// Ouverture de la consigne de relance (`retryCorrectiveLead`, llm_injected.go).
pub const RETRY_CORRECTIVE_LEAD: &str =
    "Your last answer contained a TOOL CALL WRITTEN AS TEXT — it was never executed:\n";

// ───────────────────────── Décodage des chunks ─────────────────────────

/// Le sous-ensemble utile d'un chunk `/v1/chat/completions` (`streamChunk`).
#[derive(Debug, Clone, PartialEq, Default)]
pub struct StreamChunk {
    pub choices: Vec<ChunkChoice>,
    /// `timings` llama.cpp (chunk final).
    pub timings: Option<ChunkTimings>,
    /// `usage` (include_usage), hors `choices`.
    pub usage: Option<ChunkUsage>,
}

#[derive(Debug, Clone, PartialEq, Default)]
pub struct ChunkChoice {
    pub delta: ChunkDelta,
    pub finish_reason: String,
}

#[derive(Debug, Clone, PartialEq, Default)]
pub struct ChunkDelta {
    pub content: String,
    pub reasoning_content: String,
    pub tool_calls: Vec<ToolCall>,
}

#[derive(Debug, Clone, PartialEq, Default)]
pub struct ChunkTimings {
    pub prompt_n: i64,
    pub prompt_ms: f64,
    pub prompt_per_second: f64,
    pub predicted_n: i64,
    pub predicted_ms: f64,
    pub predicted_per_second: f64,
}

#[derive(Debug, Clone, PartialEq, Default)]
pub struct ChunkUsage {
    pub prompt_tokens: i64,
    pub completion_tokens: i64,
    pub total_tokens: i64,
}

/// Erreur de type façon `encoding/json` : le chunk ENTIER est sauté.
struct TypeErr;
type Dec<T> = Result<T, TypeErr>;

/// Repli de casse des clés JSON comme `encoding/json` (EqualFold) : ASCII,
/// plus « ſ » → s et « K » (Kelvin) → k, seuls non-ASCII qui se replient sur
/// une lettre ASCII.
fn fold_char(c: char) -> char {
    match c {
        '\u{17F}' => 's',
        '\u{212A}' => 'k',
        c => c.to_ascii_lowercase(),
    }
}

/// Champ d'un objet : nom exact d'abord, sinon insensible à la casse (Go).
fn field<'a>(obj: &'a Map<String, Value>, name: &str) -> Option<&'a Value> {
    obj.get(name).or_else(|| {
        obj.iter()
            .find(|(k, _)| k.chars().map(fold_char).eq(name.chars().map(fold_char)))
            .map(|(_, v)| v)
    })
}

fn dec_string(v: Option<&Value>) -> Dec<String> {
    match v {
        None | Some(Value::Null) => Ok(String::new()),
        Some(Value::String(s)) => Ok(s.clone()),
        _ => Err(TypeErr),
    }
}

/// `int` Go : un littéral entier seulement (12.0 ou 1e3 font échouer).
fn dec_int(v: Option<&Value>) -> Dec<i64> {
    Ok(dec_opt_int(v)?.unwrap_or(0))
}

fn dec_opt_int(v: Option<&Value>) -> Dec<Option<i64>> {
    match v {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Number(n)) => n.as_i64().map(Some).ok_or(TypeErr),
        _ => Err(TypeErr),
    }
}

fn dec_float(v: Option<&Value>) -> Dec<f64> {
    match v {
        None | Some(Value::Null) => Ok(0.0),
        Some(Value::Number(n)) => n.as_f64().ok_or(TypeErr),
        _ => Err(TypeErr),
    }
}

fn dec_object(v: Option<&Value>) -> Dec<Option<&Map<String, Value>>> {
    match v {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Object(o)) => Ok(Some(o)),
        _ => Err(TypeErr),
    }
}

fn dec_array(v: Option<&Value>) -> Dec<&[Value]> {
    match v {
        None | Some(Value::Null) => Ok(&[]),
        Some(Value::Array(a)) => Ok(a),
        _ => Err(TypeErr),
    }
}

fn dec_tool_call(v: &Value) -> Dec<ToolCall> {
    let mut tc = ToolCall::default();
    let Some(o) = dec_object(Some(v))? else {
        return Ok(tc);
    };
    tc.index = dec_opt_int(field(o, "index"))?;
    tc.id = dec_string(field(o, "id"))?;
    tc.kind = dec_string(field(o, "type"))?;
    if let Some(f) = dec_object(field(o, "function"))? {
        tc.function.name = dec_string(field(f, "name"))?;
        tc.function.arguments = dec_string(field(f, "arguments"))?;
    }
    Ok(tc)
}

fn dec_choice(v: &Value) -> Dec<ChunkChoice> {
    let mut ch = ChunkChoice::default();
    let Some(o) = dec_object(Some(v))? else {
        return Ok(ch);
    };
    if let Some(d) = dec_object(field(o, "delta"))? {
        ch.delta.content = dec_string(field(d, "content"))?;
        ch.delta.reasoning_content = dec_string(field(d, "reasoning_content"))?;
        ch.delta.tool_calls = dec_array(field(d, "tool_calls"))?
            .iter()
            .map(dec_tool_call)
            .collect::<Dec<_>>()?;
    }
    ch.finish_reason = dec_string(field(o, "finish_reason"))?;
    Ok(ch)
}

fn dec_chunk(o: &Map<String, Value>) -> Dec<StreamChunk> {
    let choices = dec_array(field(o, "choices"))?
        .iter()
        .map(dec_choice)
        .collect::<Dec<_>>()?;
    let timings = match dec_object(field(o, "timings"))? {
        None => None,
        Some(t) => Some(ChunkTimings {
            prompt_n: dec_int(field(t, "prompt_n"))?,
            prompt_ms: dec_float(field(t, "prompt_ms"))?,
            prompt_per_second: dec_float(field(t, "prompt_per_second"))?,
            predicted_n: dec_int(field(t, "predicted_n"))?,
            predicted_ms: dec_float(field(t, "predicted_ms"))?,
            predicted_per_second: dec_float(field(t, "predicted_per_second"))?,
        }),
    };
    let usage = match dec_object(field(o, "usage"))? {
        None => None,
        Some(u) => Some(ChunkUsage {
            prompt_tokens: dec_int(field(u, "prompt_tokens"))?,
            completion_tokens: dec_int(field(u, "completion_tokens"))?,
            total_tokens: dec_int(field(u, "total_tokens"))?,
        }),
    };
    Ok(StreamChunk {
        choices,
        timings,
        usage,
    })
}

/// `json.Unmarshal(data, &streamChunk)` : `None` = le chunk est sauté (JSON
/// invalide, octets en trop, ou un champ connu mal typé — y compris dans un
/// `choices[i]` autre que le premier). `null` donne un chunk vide, comme en Go.
pub fn decode_stream_chunk(data: &str) -> Option<StreamChunk> {
    let v: Value = serde_json::from_str(data).ok()?;
    match &v {
        Value::Null => Some(StreamChunk::default()),
        Value::Object(o) => dec_chunk(o).ok(),
        _ => None,
    }
}

// ───────────────────────── Télémétrie tolérante ─────────────────────────

/// Champs de télémétrie du chunk final (`perfWire`), lus sans JAMAIS échouer.
/// `None` = inconnu (absent, mal typé, hors bornes) — ce n'est pas 0.
#[derive(Debug, Clone, Copy, PartialEq, Default)]
pub struct PerfWire {
    pub cache_n: Option<f64>,
    pub draft_n: Option<f64>,
    pub draft_n_accepted: Option<f64>,
    /// `usage.prompt_tokens_details.cached_tokens` (format OpenAI).
    pub cached_tokens: Option<f64>,
}

/// `perfNum.UnmarshalJSON` : un nombre, ou une chaîne qui en contient un ;
/// fini, positif et < 1e12.
fn perf_num(v: Option<&Value>) -> Option<f64> {
    let f = match v? {
        Value::Number(n) => n.as_f64()?,
        Value::String(s) => s.parse::<f64>().ok()?,
        _ => return None,
    };
    (f.is_finite() && (0.0..1e12).contains(&f)).then_some(f)
}

fn perf_ptr(n: Option<f64>) -> Option<i64> {
    n.map(|f| f as i64)
}

/// `decodePerfWire` : seule la PREMIÈRE valeur JSON compte (des octets
/// parasites derrière ne rendent pas tout inconnu) ; une structure de travers
/// laisse sa partie inconnue sans toucher au reste.
pub fn decode_perf_wire(data: &str) -> PerfWire {
    let mut w = PerfWire::default();
    let mut it = serde_json::Deserializer::from_str(data).into_iter::<Value>();
    let Some(Ok(Value::Object(o))) = it.next() else {
        return w;
    };
    if let Some(Value::Object(t)) = field(&o, "timings") {
        w.cache_n = perf_num(field(t, "cache_n"));
        w.draft_n = perf_num(field(t, "draft_n"));
        w.draft_n_accepted = perf_num(field(t, "draft_n_accepted"));
    }
    if let Some(Value::Object(u)) = field(&o, "usage")
        && let Some(Value::Object(d)) = field(u, "prompt_tokens_details")
    {
        w.cached_tokens = perf_num(field(d, "cached_tokens"));
    }
    w
}

/// `applyPerf` : `timings.cache_n` a la priorité ; `cached_tokens` ne comble
/// qu'un cache encore inconnu.
pub fn apply_perf(s: &mut StatsEvent, w: &PerfWire) {
    if let Some(p) = perf_ptr(w.cache_n) {
        s.cache_tokens = Some(p);
    }
    if let Some(p) = perf_ptr(w.draft_n) {
        s.draft_n = Some(p);
    }
    if let Some(p) = perf_ptr(w.draft_n_accepted) {
        s.draft_accepted = Some(p);
    }
    if s.cache_tokens.is_none() {
        s.cache_tokens = perf_ptr(w.cached_tokens);
    }
}

// ───────────────────────── Lecture des arguments en flux ─────────────────────────

/// `writeBodyKey` : l'argument qui porte le texte qu'un outil d'écriture va
/// poser (montré en direct), `None` pour les autres outils.
pub fn write_body_key(tool: &str) -> Option<&'static str> {
    match tool {
        "write" | "mem_add" => Some("content"),
        "edit" | "mem_edit" => Some("new"),
        _ => None,
    }
}

/// Argument montré pendant la frappe d'un appel (table propre à la boucle,
/// distincte de [`tool_call_label`] : pas de see_image, web_grep sur l'URL).
fn typing_label_key(name: &str) -> &'static str {
    match name {
        "mem_search" | "web_search" | "web_images" | "recall_search" => "query",
        "mem_read" | "mem_add" | "mem_edit" | "edit" | "write" | "read" | "git_diff" => "file",
        "recall" | "bash_tail" => "id",
        "web_open" | "web_read" | "web_grep" | "git_clone" => "url",
        "grep" | "glob" => "pattern",
        "ask" => "question",
        "criteria" => "action",
        _ => "command",
    }
}

/// `previewArgDone` : valeur (peut-être incomplète) de `key` dans un JSON
/// d'arguments en cours de flux, et si elle est COMPLÈTE (guillemet fermant
/// reçu). Best-effort : première occurrence de `"key"`, premier ':', premier
/// '"' ; `\r` ignoré, échappement inconnu recopié sans la barre.
pub fn preview_arg_done(args: &str, key: &str) -> (String, bool) {
    let pat = format!("\"{key}\"");
    let Some(i) = args.find(&pat) else {
        return (String::new(), false);
    };
    let rest = &args.as_bytes()[i + pat.len()..];
    let Some(j) = rest.iter().position(|&c| c == b':') else {
        return (String::new(), false);
    };
    let rest = &rest[j + 1..];
    let Some(q) = rest.iter().position(|&c| c == b'"') else {
        return (String::new(), false);
    };
    let rest = &rest[q + 1..];
    let mut b = Vec::new();
    let mut closed = false;
    let mut x = 0;
    while x < rest.len() {
        let c = rest[x];
        if c == b'\\' && x + 1 < rest.len() {
            match rest[x + 1] {
                b'n' => b.push(b'\n'),
                b't' => b.push(b'\t'),
                b'r' => {}
                e => b.push(e),
            }
            x += 2;
            continue;
        }
        if c == b'"' {
            closed = true;
            break;
        }
        b.push(c);
        x += 1;
    }
    (String::from_utf8_lossy(&b).into_owned(), closed)
}

/// `argPreview` : [`preview_arg_done`] au fil du flux, sans relire les
/// arguments à chaque morceau (seuls les octets neufs sont décodés). Seule
/// différence, sur un préfixe : une barre oblique finale reste en attente de
/// son second octet au lieu d'être écrite.
#[derive(Debug, Clone)]
pub struct ArgPreview {
    key: String,
    pat: Vec<u8>,
    /// Octets de `args` déjà consommés.
    pos: usize,
    /// 0 clé, 1 ':', 2 '"' ouvrant, 3 valeur, 4 valeur fermée.
    phase: u8,
    val: Vec<u8>,
    /// '\n' de la valeur décodée.
    lines: usize,
}

impl ArgPreview {
    pub fn new(key: &str) -> Self {
        ArgPreview {
            key: key.to_string(),
            pat: format!("\"{key}\"").into_bytes(),
            pos: 0,
            phase: 0,
            val: Vec::new(),
            lines: 0,
        }
    }

    pub fn key(&self) -> &str {
        &self.key
    }

    /// Consomme la suite de `args`, qui doit prolonger celui des appels
    /// précédents (les arguments d'un appel ne font que grandir).
    pub fn update(&mut self, args: &str) {
        let a = args.as_bytes();
        while self.pos < a.len() && self.phase < 4 {
            match self.phase {
                0 => {
                    // La clé peut être coupée entre deux morceaux : on reprend un peu avant.
                    let s = self.pos.saturating_sub(self.pat.len() - 1);
                    match a[s..]
                        .windows(self.pat.len())
                        .position(|w| w == self.pat.as_slice())
                    {
                        None => {
                            self.pos = a.len();
                            return;
                        }
                        Some(i) => {
                            self.pos = s + i + self.pat.len();
                            self.phase = 1;
                        }
                    }
                }
                1 | 2 => {
                    let sep = if self.phase == 2 { b'"' } else { b':' };
                    match a[self.pos..].iter().position(|&c| c == sep) {
                        None => {
                            self.pos = a.len();
                            return;
                        }
                        Some(j) => {
                            self.pos += j + 1;
                            self.phase += 1;
                        }
                    }
                }
                _ => {
                    let seg = &a[self.pos..];
                    let k = seg
                        .iter()
                        .position(|&c| c == b'\\' || c == b'"')
                        .unwrap_or(seg.len());
                    self.val.extend_from_slice(&seg[..k]);
                    self.lines += seg[..k].iter().filter(|&&c| c == b'\n').count();
                    self.pos += k;
                    if k == seg.len() {
                        return;
                    }
                    if seg[k] == b'"' {
                        self.pos += 1;
                        self.phase = 4;
                        return;
                    }
                    if k + 1 >= seg.len() {
                        return; // échappement coupé : on attend son second octet
                    }
                    match seg[k + 1] {
                        b'n' => {
                            self.val.push(b'\n');
                            self.lines += 1;
                        }
                        b't' => self.val.push(b'\t'),
                        b'r' => {}
                        e => {
                            // '"', '\\', '/', et l'inconnu recopié tel quel
                            self.val.push(e);
                            if e == b'\n' {
                                self.lines += 1;
                            }
                        }
                    }
                    self.pos += 2;
                }
            }
        }
    }

    /// La valeur décodée jusqu'ici (sans copie quand elle est valide).
    pub fn value(&self) -> std::borrow::Cow<'_, str> {
        String::from_utf8_lossy(&self.val)
    }

    /// Guillemet fermant reçu.
    pub fn done(&self) -> bool {
        self.phase == 4
    }

    /// Nombre de '\n' de la valeur.
    pub fn newlines(&self) -> usize {
        self.lines
    }

    /// `body_line_count(value())` sans relire la valeur.
    pub fn line_count(&self) -> usize {
        match self.val.len() {
            0 => 0,
            n if self.val[n - 1] != b'\n' => self.lines + 1,
            1 => 0, // "\n" seul : body_line_count le compte vide
            _ => self.lines,
        }
    }
}

/// `bodyTail` : la fin du corps en cours d'écriture, ses 40 dernières lignes
/// et 4 Kio au plus, coupée en début de ligne (en début de caractère pour une
/// ligne seule plus longue). Le booléen dit que le début a été omis.
pub fn body_tail(s: &str) -> (&str, bool) {
    let start = body_tail_start(s.as_bytes());
    (&s[start..], start > 0)
}

/// Début de [`body_tail`] sur des octets (toujours un début de caractère).
fn body_tail_start(b: &[u8]) -> usize {
    let lo = b.len().saturating_sub(BODY_TAIL_BYTES);
    let (mut start, mut n, mut end) = (None, 0, b.len());
    loop {
        match b[..end].iter().rposition(|&c| c == b'\n') {
            None => {
                if lo == 0 {
                    start = Some(0);
                }
                break;
            }
            Some(i) => {
                if i + 1 < lo {
                    break;
                }
                n += 1;
                start = Some(i + 1);
                if n == BODY_TAIL_LINES {
                    break;
                }
                end = i;
            }
        }
    }
    start.unwrap_or_else(|| {
        let mut st = lo;
        while st < b.len() && (b[st] & 0xC0) == 0x80 {
            st += 1;
        }
        st
    })
}

/// `bodyLineCount` : lignes du corps comme les compte l'UI — un saut de ligne
/// final n'ouvre pas de ligne de plus.
pub fn body_line_count(s: &str) -> usize {
    let s = s.strip_suffix('\n').unwrap_or(s);
    if s.is_empty() {
        return 0;
    }
    s.matches('\n').count() + 1
}

// ───────────────────────── Libellé d'un appel ─────────────────────────

/// Arguments d'un appel décodés comme `json.Unmarshal(…, &map[string]any)` :
/// vide si ce n'est pas un objet JSON.
pub fn call_args(arguments: &str) -> Map<String, Value> {
    serde_json::from_str::<Map<String, Value>>(arguments).unwrap_or_default()
}

fn arg_str(args: &Map<String, Value>, k: &str) -> String {
    match args.get(k) {
        Some(Value::String(s)) => s.clone(),
        _ => String::new(),
    }
}

/// `str` (tracker.go) : chaîne d'un argument, tolérante au type.
fn go_str(v: Option<&Value>) -> String {
    match v {
        None | Some(Value::Null) => String::new(),
        Some(Value::String(s)) => s.clone(),
        Some(Value::Number(n)) => format!("{:.0}", n.as_f64().unwrap_or(0.0)),
        Some(v) => go_fmt_v(v),
    }
}

/// `isMCPTool` : nom routé vers MCP.
pub fn is_mcp_tool(name: &str) -> bool {
    name.starts_with("mcp__")
}

/// `mcpArgLabel` : libellé court des arguments d'un appel MCP.
pub fn mcp_arg_label(args: &Map<String, Value>) -> String {
    if args.is_empty() {
        return String::new();
    }
    for k in ["path", "file", "query", "command", "url", "name"] {
        if let Some(Value::String(v)) = args.get(k)
            && !v.is_empty()
        {
            return v.clone();
        }
    }
    let mut s = String::new();
    go_marshal_object(args, &mut s);
    if s.chars().count() > 120 {
        s = s.chars().take(120).collect::<String>() + "…";
    }
    s
}

/// `toolCallLabel` : le libellé HUMAIN d'un appel (commande, chemin,
/// requête…), annoncé avant l'exécution et argument principal de plusieurs
/// outils. Outil absent de la table : libellé vide.
pub fn tool_call_label(name: &str, args: &Map<String, Value>) -> String {
    match name {
        "mem_search" | "web_search" | "web_images" | "recall_search" => arg_str(args, "query"),
        "mem_read" | "mem_add" | "mem_edit" | "edit" | "write" | "read" | "git_diff"
        | "see_image" => arg_str(args, "file"),
        "recall" | "bash_tail" => arg_str(args, "id"),
        "tracker" => (go_str(args.get("action")) + " " + &go_str(args.get("name")))
            .trim()
            .to_string(),
        "bash" | "bash_bg" => arg_str(args, "command"),
        "grep" | "glob" => arg_str(args, "pattern"),
        "ask" => arg_str(args, "question"),
        "criteria" => arg_str(args, "action"),
        "git_clone" | "web_open" | "web_read" => arg_str(args, "url"),
        "web_grep" => arg_str(args, "pattern") + " @ " + &arg_str(args, "url"),
        _ if is_mcp_tool(name) => mcp_arg_label(args),
        _ => String::new(),
    }
}

// ───────────────────────── Réparation des appels ─────────────────────────

/// `toolNameAliases` : nom halluciné → outil de Loki.
const TOOL_NAME_ALIASES: &[(&str, &str)] = &[
    ("read_file", "read"),
    ("view", "read"),
    ("cat", "read"),
    ("open_file", "read"),
    ("write_file", "write"),
    ("create_file", "write"),
    ("edit_file", "edit"),
    ("str_replace", "edit"),
    ("replace_in_file", "edit"),
    ("str_replace_editor", "edit"),
    ("run_command", "bash"),
    ("shell", "bash"),
    ("run_shell", "bash"),
    ("execute_command", "bash"),
    ("terminal", "bash"),
    ("search", "grep"),
    ("grep_search", "grep"),
    ("search_files", "grep"),
    ("find_files", "glob"),
    ("list_files", "glob"),
    ("file_search", "glob"),
];

const FILE_ALIASES: [(&str, &str); 4] = [
    ("path", "file"),
    ("file_path", "file"),
    ("filename", "file"),
    ("filepath", "file"),
];

/// `toolArgAliases` : par outil, argument halluciné → argument attendu. Ordre
/// fixe (en Go, ordre aléatoire d'une map : voir [`repair_tool_call`]).
fn tool_arg_aliases(tool: &str) -> Vec<(&'static str, &'static str)> {
    match tool {
        "read" => FILE_ALIASES.to_vec(),
        "write" => [
            FILE_ALIASES.as_slice(),
            &[("contents", "content"), ("text", "content")],
        ]
        .concat(),
        "edit" => [
            FILE_ALIASES.as_slice(),
            &[
                ("old_string", "old"),
                ("old_str", "old"),
                ("old_text", "old"),
                ("search", "old"),
                ("new_string", "new"),
                ("new_str", "new"),
                ("new_text", "new"),
                ("replace", "new"),
            ],
        ]
        .concat(),
        "bash" | "bash_bg" => vec![("cmd", "command")],
        "grep" => vec![("query", "pattern"), ("regex", "pattern")],
        "glob" => vec![("query", "pattern")],
        _ => Vec::new(),
    }
}

/// `subagentRoles` (code_subagent.go).
pub const SUBAGENT_ROLES: [&str; 3] = ["explorer", "code-reviewer", "planner"];

/// `repairToolCall` : corrige en place le nom et les arguments d'un appel
/// quand ils visent, sous un autre nom, un outil disponible dans ce tour
/// (read_file → read, path → file, un rôle de sous-agent → subagent{role}).
/// `true` si l'appel a été modifié. Les arguments doivent déjà être du JSON
/// valide ; réécrits comme `json.Marshal` (clés triées, HTML échappé).
///
/// Écart assumé : quand deux alias d'un même argument sont présents
/// (`path` et `file_path`), Go garde celui que l'ordre ALÉATOIRE de sa map
/// visite d'abord ; ici, le premier de la table.
pub fn repair_tool_call(tc: &mut ToolCall, tools: &[Tool]) -> bool {
    let have: HashSet<&str> = tools.iter().map(|t| t.function.name.as_str()).collect();
    let mut name = tc.function.name.clone();
    let mut args = call_args(&tc.function.arguments);
    let mut changed = false;
    if !have.contains(name.as_str()) {
        let lower = name.trim().to_lowercase();
        let alias = TOOL_NAME_ALIASES
            .iter()
            .find(|(from, _)| *from == lower)
            .map(|(_, to)| *to);
        if let Some(t) = alias.filter(|t| have.contains(t)) {
            name = t.to_string();
            changed = true;
        } else if have.contains("subagent") {
            // Un sous-agent appelé comme un outil (« explorer », « code_reviewer »).
            let role = lower.replace('_', "-");
            if let Some(r) = SUBAGENT_ROLES.iter().find(|r| **r == role) {
                if !args.contains_key("role") {
                    args.insert("role".into(), Value::String((*r).into()));
                }
                if !args.contains_key("task") {
                    for k in ["prompt", "query", "question", "description"] {
                        if let Some(Value::String(v)) = args.get(k)
                            && !v.is_empty()
                        {
                            let v = Value::String(v.clone());
                            args.insert("task".into(), v);
                            args.remove(k);
                            break;
                        }
                    }
                }
                name = "subagent".into();
                changed = true;
            }
        }
    }
    for (from, to) in tool_arg_aliases(&name) {
        let Some(v) = args.remove(from) else {
            continue;
        };
        if !args.contains_key(to) {
            args.insert(to.into(), v);
            changed = true;
        }
    }
    if !changed {
        return false;
    }
    let mut out = String::new();
    go_marshal_object(&args, &mut out);
    tc.function.name = name;
    tc.function.arguments = out;
    true
}

/// `json.Valid` : syntaxe seulement (tout type de valeur, blancs autour).
fn json_valid(s: &str) -> bool {
    serde_json::from_str::<serde::de::IgnoredAny>(s).is_ok()
}

/// Appels d'outils d'une complétion, prêts pour l'historique.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct AssembledToolCalls {
    /// Triés par index de flux ; `id` posé (`call_<iter>_<i>` s'il manquait).
    pub calls: Vec<ToolCall>,
    /// Ids des appels dont les arguments étaient du JSON CASSÉ (non vide) :
    /// remplacés par `{}`, à NE PAS exécuter (`badArgs`).
    pub bad_args: HashSet<String>,
}

/// Assemblage des appels accumulés (llm_client.go ~l. 2262–2297) : tri par
/// index, id de secours, arguments invalides neutralisés en `{}` (un vide
/// n'est pas une faute : outil sans paramètre), [`repair_tool_call`] sinon.
/// `iter` : rang de la complétion dans la boucle du tour.
pub fn assemble_tool_calls(
    calls: &BTreeMap<i64, ToolCall>,
    iter: i64,
    tools: &[Tool],
) -> AssembledToolCalls {
    let mut out = AssembledToolCalls::default();
    for (i, tc) in calls.values().enumerate() {
        let mut tc = tc.clone();
        if tc.id.is_empty() {
            tc.id = format!("call_{iter}_{i}");
        }
        if !json_valid(&tc.function.arguments) {
            if !tc.function.arguments.trim().is_empty() {
                out.bad_args.insert(tc.id.clone());
            }
            tc.function.arguments = "{}".into();
        } else {
            repair_tool_call(&mut tc, tools);
        }
        out.calls.push(tc);
    }
    out
}

// ───────────────────────── Appels d'outils écrits en texte ─────────────────────────

/// Un motif d'appel textuel. `find` rend la PREMIÈRE correspondance (la plus
/// à gauche, sémantique RE2/Go) en octets. Les motifs embarqués sont codés à
/// la main ; la surcharge `retry_patterns` (regex quelconques) est fournie par
/// l'appelant.
pub trait RetryPattern: Send + Sync {
    fn find(&self, haystack: &str) -> Option<(usize, usize)>;
}

/// Jeu de motifs (`retryPatterns()`), et s'il s'agit de la surcharge
/// (`retryCustom` : pas de balayage fenêtré, rien ne borne leur portée).
pub struct RetryPatterns {
    pats: Vec<Box<dyn RetryPattern>>,
    custom: bool,
}

impl RetryPatterns {
    /// Les motifs embarqués (`defaultRetryPatterns`), tous en `(?mi)`.
    pub fn builtin() -> Arc<RetryPatterns> {
        static B: OnceLock<Arc<RetryPatterns>> = OnceLock::new();
        B.get_or_init(|| {
            let pats: Vec<Box<dyn RetryPattern>> = vec![
                Box::new(Builtin::Lit("<tool_call>")),
                Box::new(Builtin::Lit("</tool_call>")),
                Box::new(Builtin::Lit("<|tool_call|>")),
                Box::new(Builtin::Lit("```tool_code")),
                Box::new(Builtin::Lit("```tool_call")),
                Box::new(Builtin::DefaultApi),
                Box::new(Builtin::FunctionsCall),
                Box::new(Builtin::JsonName),
                Box::new(Builtin::Lit("[TOOL_REQUEST]")),
                // Format XML de Qwen3-Coder resté en texte.
                Box::new(Builtin::Tag("<function=")),
                Box::new(Builtin::Tag("<parameter=")),
            ];
            Arc::new(RetryPatterns {
                pats,
                custom: false,
            })
        })
        .clone()
    }

    /// La surcharge `retry_patterns` (motifs déjà compilés par l'appelant).
    pub fn custom(pats: Vec<Box<dyn RetryPattern>>) -> RetryPatterns {
        RetryPatterns { pats, custom: true }
    }

    pub fn is_custom(&self) -> bool {
        self.custom
    }

    /// `textualToolCall` : le texte contient un appel d'outil écrit en toutes
    /// lettres au lieu d'être émis par le protocole.
    pub fn textual_tool_call(&self, content: &str) -> bool {
        !self.snippet(content).is_empty()
    }

    /// `textualToolCallFrom` : idem pendant le flux, quand `content[..scanned]`
    /// a DÉJÀ été balayé sans rien trouver — seule la fin est relue (à partir
    /// d'un vrai début de ligne). Surcharge : balayage complet.
    pub fn textual_tool_call_from(&self, content: &str, scanned: usize) -> bool {
        if content.is_empty() {
            return false;
        }
        let start = if self.custom {
            0
        } else {
            retry_scan_start(content, scanned)
        };
        let w = &content[start..];
        self.pats.iter().any(|p| p.find(w).is_some())
    }

    /// `textualToolCallSnippet` : l'extrait fautif (le motif reconnu et ~80
    /// octets de ce qui suit), "" si aucun. Premier motif de la liste qui
    /// correspond, à sa première position.
    pub fn snippet(&self, content: &str) -> String {
        if content.is_empty() {
            return String::new();
        }
        for p in &self.pats {
            if let Some((a, b)) = p.find(content) {
                let mut end = (b + 80).min(content.len());
                while end < content.len() && !content.is_char_boundary(end) {
                    end += 1;
                }
                return content[a..end].trim().to_string();
            }
        }
        String::new()
    }
}

impl RetryPatterns {
    /// `compileRetryPatterns` : la surcharge `retry_patterns` de la base —
    /// un tableau JSON de regex du Go, chacune en `(?mi)`. `None` : absente,
    /// illisible ou vide (les motifs embarqués s'appliquent). Une regex qui
    /// ne se compile pas est sautée, comme le Go le fait.
    pub fn from_override(raw: &str) -> Option<RetryPatterns> {
        if raw.is_empty() {
            return None;
        }
        let over: Vec<String> = serde_json::from_str(raw).ok()?;
        if over.is_empty() {
            return None;
        }
        let pats = over
            .iter()
            .filter_map(|p| compile_go_regex(&format!("(?mi){p}")))
            .map(|r| Box::new(GoRegex(r)) as Box<dyn RetryPattern>)
            .collect();
        Some(RetryPatterns::custom(pats))
    }

    /// Nombre de motifs.
    pub fn len(&self) -> usize {
        self.pats.len()
    }

    pub fn is_empty(&self) -> bool {
        self.pats.is_empty()
    }
}

/// `regexp.Compile` du Go : la regex traduite ([`go_regex`]), octal permis
/// (`\\101`) ; `None` si l'un ou l'autre la refuse.
pub fn compile_go_regex(p: &str) -> Option<regex::Regex> {
    regex::RegexBuilder::new(&go_regex(p)?)
        .octal(true)
        .build()
        .ok()
}

/// Un motif de la surcharge, compilé.
struct GoRegex(regex::Regex);

impl RetryPattern for GoRegex {
    fn find(&self, haystack: &str) -> Option<(usize, usize)> {
        self.0.find(haystack).map(|m| (m.start(), m.end()))
    }
}

/// Une regex du Go (RE2, paquet `regexp`) réécrite pour `regex` : les
/// classes Perl et les limites de mot du Go sont ASCII (`\w`, `\d`, `\s`,
/// `\b`), celles de `regex` Unicode ; `\Q…\E` (texte littéral) n'existe
/// pas en Rust ; dans une classe, `[`, `&&`, `--` et `~~` y ont un sens que
/// le Go ne leur donne pas. `None` : une regex que le Go refuse et que
/// `regex` accepterait (`\\Q` dans une classe).
pub fn go_regex(p: &str) -> Option<String> {
    let cs: Vec<char> = p.chars().collect();
    let mut out = String::with_capacity(p.len() + 16);
    let (mut i, mut class, mut class_start) = (0, false, false);
    while i < cs.len() {
        let c = cs[i];
        if c == '\\' && i + 1 < cs.len() {
            let n = cs[i + 1];
            i += 2;
            if n == 'Q' {
                // Texte littéral jusqu'à \E, ou jusqu'au bout.
                let mut lit = String::new();
                while i < cs.len() && !(cs[i] == '\\' && cs.get(i + 1) == Some(&'E')) {
                    lit.push(cs[i]);
                    i += 1;
                }
                if i < cs.len() {
                    i += 2;
                }
                if class {
                    return None;
                }
                out.push_str(&regex::escape(&lit));
                class_start = false;
                continue;
            }
            // Un chiffre seul est une référence arrière pour le Go, qui la
            // refuse ; avec l'octal permis, `regex` l'accepterait.
            if ('1'..='7').contains(&n) && !cs.get(i).is_some_and(|d| ('0'..='7').contains(d)) {
                return None;
            }
            let rep: &str = match (class, n) {
                (false, 'w') => "[[:word:]]",
                (false, 'W') => "[[:^word:]]",
                (false, 'd') => "[0-9]",
                (false, 'D') => "[^0-9]",
                (false, 's') => r"[\t\n\f\r ]",
                (false, 'S') => r"[^\t\n\f\r ]",
                (false, 'b') => r"(?-u:\b)",
                (false, 'B') => r"(?-u:\B)",
                (true, 'w') => "[:word:]",
                (true, 'W') => "[:^word:]",
                (true, 'd') => "0-9",
                (true, 'D') => "[:^digit:]",
                (true, 's') => r"\t\n\f\r ",
                (true, 'S') => "[:^space:]",
                _ => {
                    out.push('\\');
                    out.push(n);
                    class_start = false;
                    continue;
                }
            };
            out.push_str(rep);
            class_start = false;
            continue;
        }
        if class {
            match c {
                ']' if !class_start => {
                    class = false;
                    out.push(']');
                }
                '[' if cs.get(i + 1) == Some(&':') => {
                    // Classe POSIX : recopiée jusqu'à « :] ».
                    let rest: String = cs[i..].iter().collect();
                    match rest.find(":]") {
                        Some(end) => {
                            out.push_str(&rest[..end + 2]);
                            i += rest[..end + 2].chars().count();
                            class_start = false;
                            continue;
                        }
                        None => out.push_str(r"\["),
                    }
                }
                '[' | '&' | '~' => {
                    out.push('\\');
                    out.push(c);
                }
                '-' if cs.get(i + 1) == Some(&'-') => {
                    out.push_str(r"-\-");
                    i += 1;
                }
                _ => out.push(c),
            }
            class_start = false;
            i += 1;
            continue;
        }
        out.push(c);
        i += 1;
        if c == '[' {
            class = true;
            class_start = true;
            if cs.get(i) == Some(&'^') {
                out.push('^');
                i += 1;
            }
        }
    }
    Some(out)
}

/// Marge relue avant la fin du balayage précédent.
const RETRY_SCAN_MARGIN: usize = 512;
/// Recul au-delà duquel on retombe sur le balayage complet.
const RETRY_SCAN_MAX_BACK: usize = 16 << 10;

/// `retryScanStart` : début de la fenêtre à relire. `scanned − marge`, puis
/// recul tant qu'on est dans une suite que les motifs embarqués peuvent
/// traverser (\w+, \s*, `{"name":`, multioctet), enfin début de ligne. Recul
/// trop long ou ligne sans fin : 0.
pub fn retry_scan_start(content: &str, scanned: usize) -> usize {
    let b = content.as_bytes();
    let scanned = scanned.min(b.len()) as isize;
    let mut p = scanned - RETRY_SCAN_MARGIN as isize;
    if p <= 0 {
        return 0;
    }
    let floor = scanned - RETRY_SCAN_MAX_BACK as isize;
    while p > 0 && retry_run_byte(b[p as usize - 1]) {
        p -= 1;
        if p < floor {
            return 0;
        }
    }
    match b[..p as usize].iter().rposition(|&c| c == b'\n') {
        Some(i) if i as isize + 1 >= floor => i + 1,
        _ => 0,
    }
}

fn retry_run_byte(c: u8) -> bool {
    c >= 0x80
        || c.is_ascii_alphanumeric()
        || matches!(
            c,
            b'_' | b' ' | b'\t' | b'\n' | b'\r' | 0x0c | b'"' | b':' | b'{'
        )
}

/// Motifs embarqués, en sémantique Go `(?mi)` : lettres insensibles à la
/// casse (« s » accepte aussi « ſ », « k » aussi « K » Kelvin, et `\w` les
/// accepte donc aussi), `\b` et `\s` ASCII, `^` en début de texte ou après '\n'.
enum Builtin {
    /// Littéral (premier caractère non alphabétique).
    Lit(&'static str),
    /// `\bdefault_api[.:]\w+`
    DefaultApi,
    /// `\bfunctions\.\w+\s*\(`
    FunctionsCall,
    /// `^\s*\{"name":\s*"(bash|read|grep|glob|edit|write|mem_\w+|web_\w+|git_\w+|criteria|ask)"`
    JsonName,
    /// `<function=\w+>` / `<parameter=\w+>` (préfixe donné).
    Tag(&'static str),
}

/// Littéral ASCII du motif à la position `i` du texte (frontière de
/// caractère), lettres repliées comme `(?i)`. Rend la fin.
fn lit_fold_at(s: &str, mut i: usize, lit: &str) -> Option<usize> {
    let b = s.as_bytes();
    for &p in lit.as_bytes() {
        let c = *b.get(i)?;
        if p.is_ascii_alphabetic() {
            if c.eq_ignore_ascii_case(&p) {
                i += 1;
                continue;
            }
            let lp = p.to_ascii_lowercase();
            if lp == b's' && s[i..].starts_with('\u{17F}') {
                i += 2;
                continue;
            }
            if lp == b'k' && s[i..].starts_with('\u{212A}') {
                i += 3;
                continue;
            }
            return None;
        }
        if c != p {
            return None;
        }
        i += 1;
    }
    Some(i)
}

/// Longueur du caractère de `\w` replié à la position `i`, s'il y en a un.
fn word_fold_len(s: &str, i: usize) -> Option<usize> {
    let c = *s.as_bytes().get(i)?;
    if c.is_ascii_alphanumeric() || c == b'_' {
        return Some(1);
    }
    if s[i..].starts_with('\u{17F}') {
        return Some(2);
    }
    if s[i..].starts_with('\u{212A}') {
        return Some(3);
    }
    None
}

/// `\w+` gourmand depuis `i` : la fin, si au moins un caractère.
fn word_run(s: &str, i: usize) -> Option<usize> {
    let mut j = i;
    while let Some(n) = word_fold_len(s, j) {
        j += n;
    }
    (j > i).then_some(j)
}

fn is_go_space(c: u8) -> bool {
    matches!(c, b'\t' | b'\n' | 0x0c | b'\r' | b' ')
}

/// `\b` avant un caractère de mot : le précédent n'est pas un mot ASCII.
fn word_boundary_before(s: &str, i: usize) -> bool {
    i == 0 || {
        let c = s.as_bytes()[i - 1];
        !(c.is_ascii_alphanumeric() || c == b'_')
    }
}

/// Positions où le premier caractère d'un motif peut commencer. Ce premier
/// caractère est '<', '`', '[', '{', 'd' ou 'f' : aucun n'a de repli non
/// ASCII, un octet ASCII suffit (toujours une frontière de caractère).
fn starts(s: &str, first: u8) -> impl Iterator<Item = usize> + '_ {
    let fl = first.to_ascii_lowercase();
    s.as_bytes()
        .iter()
        .enumerate()
        .filter_map(move |(i, &c)| (c.to_ascii_lowercase() == fl).then_some(i))
}

const JSON_NAME_ALTS: [&str; 8] = [
    "bash", "read", "grep", "glob", "edit", "write", "criteria", "ask",
];
const JSON_NAME_PREFIX_ALTS: [&str; 3] = ["mem_", "web_", "git_"];

impl Builtin {
    fn json_name_end(s: &str, p: usize) -> Option<usize> {
        let b = s.as_bytes();
        let mut i = lit_fold_at(s, p, "{\"name\":")?;
        while i < b.len() && is_go_space(b[i]) {
            i += 1;
        }
        if b.get(i) != Some(&b'"') {
            return None;
        }
        i += 1;
        let quote = |e: usize| (b.get(e) == Some(&b'"')).then_some(e + 1);
        for alt in JSON_NAME_ALTS {
            if let Some(e) = lit_fold_at(s, i, alt).and_then(quote) {
                return Some(e);
            }
        }
        for alt in JSON_NAME_PREFIX_ALTS {
            if let Some(e) = lit_fold_at(s, i, alt)
                .and_then(|e| word_run(s, e))
                .and_then(quote)
            {
                return Some(e);
            }
        }
        None
    }
}

impl RetryPattern for Builtin {
    fn find(&self, s: &str) -> Option<(usize, usize)> {
        let b = s.as_bytes();
        match self {
            Builtin::Lit(lit) => {
                starts(s, lit.as_bytes()[0]).find_map(|i| lit_fold_at(s, i, lit).map(|e| (i, e)))
            }
            Builtin::Tag(prefix) => starts(s, b'<').find_map(|i| {
                let e = lit_fold_at(s, i, prefix)?;
                let e = word_run(s, e)?;
                (b.get(e) == Some(&b'>')).then_some((i, e + 1))
            }),
            Builtin::DefaultApi => starts(s, b'd').find_map(|i| {
                if !word_boundary_before(s, i) {
                    return None;
                }
                let e = lit_fold_at(s, i, "default_api")?;
                if !matches!(b.get(e), Some(b'.' | b':')) {
                    return None;
                }
                word_run(s, e + 1).map(|e| (i, e))
            }),
            Builtin::FunctionsCall => starts(s, b'f').find_map(|i| {
                if !word_boundary_before(s, i) {
                    return None;
                }
                let e = lit_fold_at(s, i, "functions.")?;
                let mut e = word_run(s, e)?;
                while e < b.len() && is_go_space(b[e]) {
                    e += 1;
                }
                (b.get(e) == Some(&b'(')).then_some((i, e + 1))
            }),
            Builtin::JsonName => starts(s, b'{').find_map(|p| {
                let end = Builtin::json_name_end(s, p)?;
                // `^\s*` : le début de ligne le plus à gauche dans les blancs
                // qui précèdent l'accolade.
                let mut ws = p;
                while ws > 0 && is_go_space(b[ws - 1]) {
                    ws -= 1;
                }
                (ws..=p)
                    .find(|&j| j == 0 || b[j - 1] == b'\n')
                    .map(|j| (j, end))
            }),
        }
    }
}

/// `retryCorrective` : le message réinjecté pour relancer le tour, avec
/// l'extrait fautif.
pub fn retry_corrective(snippet: &str) -> String {
    RETRY_CORRECTIVE_LEAD.to_string()
        + snippet
        + "\nTool calls must go through the tool-call protocol, never in the answer text. "
        + "Redo it now: emit the real tool call, or answer directly without pretending to call a tool."
}

// ───────────────────────── Le parseur ─────────────────────────

/// `noteEnginePrompt(prompt_tokens, cached, cachedKnown)` → conseil de cache
/// (`StatsEvent::cache_hint`). Appelé sur le moteur local seulement.
pub type CacheHintFn = Box<dyn FnMut(i64, i64, bool) -> String + Send>;

/// `codeWriteGuard(caps, file, isWrite)` → refus ("" = permis). Sa présence
/// vaut `caps.Code`.
pub type PreflightFn = Box<dyn FnMut(&str, bool) -> String + Send>;

/// Ce que la boucle Go sait de la complétion avant de la lire.
pub struct StreamConfig {
    /// `reasoningActive(REASONING)` : un `<think>` ouvert par le gabarit est
    /// découpé chez nous sur `</think>` (moteur qui le laisse en ligne).
    pub reasoning_on: bool,
    /// Appels exécutables : outils annoncés, ni coupés ni neutralisés.
    pub calls_on: bool,
    /// Relance `tool_choice: "none"` : tout appel (protocole ou texte) est une
    /// fuite qui coupe le flux.
    pub tool_choice_none: bool,
    /// Preset externe (pas de comptage du raisonnement, pas de conseil de cache).
    pub external: bool,
    /// REASONING_ECHO actif pour cette requête (`pol.on`).
    pub echo_on: bool,
    /// Le gabarit garde le raisonnement passé (`pol.keepsPast`).
    pub echo_keeps_past: bool,
    /// Relances « appel textuel » déjà faites dans ce tour (`patternRetries`).
    pub pattern_retries: i64,
    /// Motifs d'appels textuels (embarqués, ou surcharge `retry_patterns`).
    pub patterns: Arc<RetryPatterns>,
    /// Envoi de la requête (`tReq`) : base du TTFT et des stats de secours.
    pub t_req: Instant,
    pub cache_hint: Option<CacheHintFn>,
    pub preflight: Option<PreflightFn>,
}

impl Default for StreamConfig {
    fn default() -> Self {
        StreamConfig {
            reasoning_on: false,
            calls_on: false,
            tool_choice_none: false,
            external: false,
            echo_on: false,
            echo_keeps_past: false,
            pattern_retries: 0,
            patterns: RetryPatterns::builtin(),
            t_req: Instant::now(),
            cache_hint: None,
            preflight: None,
        }
    }
}

/// write/edit refusé en plein flux dès que son chemin est complet.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct PreflightRefusal {
    pub name: String,
    pub id: String,
    pub file: String,
    pub msg: String,
}

enum Preflight {
    Unchecked,
    Checked,
    Refused(PreflightRefusal),
}

/// Bilan d'une complétion lue (en entier ou coupée).
#[derive(Debug, Clone, PartialEq, Default)]
pub struct StreamOutcome {
    /// Tout le `content` reçu (`assistantContent`), `<think>` en ligne compris.
    pub content: String,
    /// Texte de réponse réellement affiché (rendu au modèle après une coupure).
    pub shown: String,
    /// Appels accumulés, par index de flux (voir [`assemble_tool_calls`]).
    pub tool_calls: BTreeMap<i64, ToolCall>,
    pub finish_reason: String,
    /// Stats de la complétion : `echoed` et `ttft_ms` posés, stats de secours
    /// mesurées s'il n'y a pas eu de `timings`.
    pub stats: StatsEvent,
    pub saw_timings: bool,
    pub saw_reasoning_field: bool,
    /// Raisonnement séparé PAR LE SERVEUR, à renvoyer (REASONING_ECHO).
    pub echo_text: String,
    /// Du texte ou un outil est parti vers l'UI.
    pub sent_answer: bool,
    /// Du raisonnement est parti vers l'UI.
    pub sent_reasoning: bool,
    /// Dernier événement d'outil émis (appel en cours d'écriture à l'écran).
    pub typing_tool: Option<ToolUsedEvent>,
    /// Le rappel a demandé l'arrêt.
    pub aborted: bool,
    /// Flux coupé sur un appel d'outil écrit en texte.
    pub pattern_hit: bool,
    /// Sous `tool_choice: "none"`, le modèle a tenté un appel quand même.
    pub none_leak: bool,
    /// write/edit refusé en plein flux.
    pub preflight: Option<PreflightRefusal>,
}

impl StreamOutcome {
    /// Complétion « complète » pour la télémétrie : ni arrêtée, lue jusqu'au
    /// bout (ou finish_reason reçu avant la coupure), avec timings ou usage.
    /// `read_failed` : la lecture du corps a échoué (`sc.Err() != nil`).
    pub fn is_complete(&self, read_failed: bool) -> bool {
        !self.aborted
            && (!read_failed || !self.finish_reason.is_empty())
            && (self.saw_timings || self.stats.prompt_tokens_total > 0)
    }

    /// Une erreur de lecture est sans conséquence : coupure APRÈS le dernier
    /// chunk (finish_reason reçu), ou flux coupé par NOUS (appel textuel,
    /// fuite sous tool_choice none, refus anticipé).
    pub fn read_error_benign(&self) -> bool {
        !self.finish_reason.is_empty()
            || self.pattern_hit
            || self.none_leak
            || self.preflight.is_some()
    }

    /// [`assemble_tool_calls`] sur les appels de cette complétion.
    pub fn assemble_tool_calls(&self, iter: i64, tools: &[Tool]) -> AssembledToolCalls {
        assemble_tool_calls(&self.tool_calls, iter, tools)
    }
}

/// Analyse incrémentale d'UNE complétion streamée.
pub struct StreamParser {
    cfg: StreamConfig,
    tool_calls: BTreeMap<i64, ToolCall>,
    content: String,
    /// Longueur de `content` déjà balayée sans appel textuel.
    retry_scanned: usize,
    finish_reason: String,
    stats: StatsEvent,
    saw_timings: bool,
    t_first: Option<Instant>,
    t_last: Option<Instant>,
    usage_gen: i64,
    gen_chunks: i64,
    last_preview: String,
    last_body_lines: i64,
    label_dec: Option<ArgPreview>,
    body_dec: Option<ArgPreview>,
    sent_answer: bool,
    sent_reasoning: bool,
    shown: String,
    typing_tool: Option<ToolUsedEvent>,
    saw_reasoning_field: bool,
    echo_buf: String,
    think_open: bool,
    think_tail: String,
    aborted: bool,
    pattern_hit: bool,
    none_leak: bool,
    preflight: Preflight,
    stopped: bool,
}

impl StreamParser {
    pub fn new(cfg: StreamConfig) -> Self {
        let think_open = cfg.reasoning_on;
        StreamParser {
            cfg,
            tool_calls: BTreeMap::new(),
            content: String::new(),
            retry_scanned: 0,
            finish_reason: String::new(),
            stats: StatsEvent::default(),
            saw_timings: false,
            t_first: None,
            t_last: None,
            usage_gen: 0,
            gen_chunks: 0,
            last_preview: String::new(),
            last_body_lines: -1,
            label_dec: None,
            body_dec: None,
            sent_answer: false,
            sent_reasoning: false,
            shown: String::new(),
            typing_tool: None,
            saw_reasoning_field: false,
            echo_buf: String::new(),
            think_open,
            think_tail: String::new(),
            aborted: false,
            pattern_hit: false,
            none_leak: false,
            preflight: Preflight::Unchecked,
            stopped: false,
        }
    }

    /// Le flux a été coupé (rappel, appel textuel, fuite, refus anticipé) :
    /// la boucle doit cesser de lire.
    pub fn is_stopped(&self) -> bool {
        self.stopped
    }

    /// `scb` : note ce qui part vers l'UI, puis le rappel.
    fn send<F: FnMut(StreamEvent) -> bool>(&mut self, ev: StreamEvent, emit: &mut F) -> bool {
        match &ev {
            StreamEvent::Content(c) if !c.is_empty() => {
                self.sent_answer = true;
                self.shown.push_str(c);
            }
            StreamEvent::ToolUsed(t) => {
                self.sent_answer = true;
                self.typing_tool = Some(t.clone());
            }
            StreamEvent::Reasoning(r) if !r.is_empty() => self.sent_reasoning = true,
            _ => {}
        }
        emit(ev)
    }

    /// Une ligne du corps (sans son '\n' ; un '\r' final est toléré). `now` :
    /// instant de réception. Rend `false` quand le flux est coupé : la boucle
    /// cesse alors de lire. Les lignes qui ne sont pas `data:`, vides,
    /// `[DONE]` ou indécodables sont ignorées, comme en Go (y compris un
    /// objet `{"error":…}` glissé dans le flux).
    pub fn feed_line<F: FnMut(StreamEvent) -> bool>(
        &mut self,
        line: &str,
        now: Instant,
        emit: &mut F,
    ) -> bool {
        if self.stopped {
            return false;
        }
        let Some(rest) = line.trim().strip_prefix("data:") else {
            return true;
        };
        let data = rest.trim();
        if data.is_empty() || data == "[DONE]" {
            return true;
        }
        let Some(chunk) = decode_stream_chunk(data) else {
            return true;
        };
        if self.on_chunk(chunk, data, now, emit) {
            true
        } else {
            self.stopped = true;
            false
        }
    }

    fn on_chunk<F: FnMut(StreamEvent) -> bool>(
        &mut self,
        chunk: StreamChunk,
        data: &str,
        now: Instant,
        emit: &mut F,
    ) -> bool {
        // Timings ET usage arrivent sur le chunk final, souvent `choices:[]` :
        // traités AVANT le garde de choices.
        let mut pw = PerfWire::default();
        if chunk.timings.is_some() || chunk.usage.is_some() {
            pw = decode_perf_wire(data);
            apply_perf(&mut self.stats, &pw);
        }
        if let Some(t) = &chunk.timings {
            self.saw_timings = true;
            self.stats.prompt_tokens = t.prompt_n;
            self.stats.prompt_per_second = t.prompt_per_second;
            self.stats.prompt_ms = t.prompt_ms;
            self.stats.gen_tokens = t.predicted_n;
            self.stats.gen_per_second = t.predicted_per_second;
            self.stats.gen_ms = t.predicted_ms;
            let s = self.stats.clone();
            self.send(StreamEvent::Stats(s), emit);
        }
        if let Some(u) = &chunk.usage {
            if u.completion_tokens > 0 {
                self.usage_gen = u.completion_tokens;
            }
            if u.prompt_tokens > 0 {
                self.stats.prompt_tokens_total = u.prompt_tokens;
                let mut s = self.stats.clone();
                if !self.cfg.external
                    && let Some(h) = self.cfg.cache_hint.as_mut()
                {
                    let c = pw.cached_tokens;
                    s.cache_hint = h(u.prompt_tokens, perf_ptr(c).unwrap_or(0), c.is_some());
                }
                self.send(StreamEvent::Stats(s), emit);
            }
        }
        let Some(ch) = chunk.choices.into_iter().next() else {
            return true;
        };
        let ChunkChoice {
            delta,
            finish_reason,
        } = ch;
        if !delta.content.is_empty()
            || !delta.reasoning_content.is_empty()
            || !delta.tool_calls.is_empty()
        {
            if self.t_first.is_none() {
                self.t_first = Some(now);
            }
            self.t_last = Some(now);
            self.gen_chunks += 1;
        }
        if !finish_reason.is_empty() {
            self.finish_reason = finish_reason;
        }
        // Relance tool_choice « none » : un appel émis malgré tout n'est pas
        // exécuté, on coupe.
        if self.cfg.tool_choice_none && !delta.tool_calls.is_empty() {
            self.none_leak = true;
            return false;
        }
        if self.cfg.calls_on && !delta.tool_calls.is_empty() {
            return self.on_tool_calls(delta.tool_calls, emit);
        }
        // Outils non annoncés : un appel parsé quand même par le moteur est ignoré.
        if !delta.reasoning_content.is_empty() {
            // Le moteur sépare déjà le raisonnement : plus de découpage chez nous.
            self.saw_reasoning_field = true;
            self.think_open = false;
            if !self.cfg.external {
                self.stats.reasoning_tokens += 1;
            }
            if self.cfg.echo_on {
                self.echo_buf.push_str(&delta.reasoning_content);
            }
            if !self.send(StreamEvent::Reasoning(delta.reasoning_content), emit) {
                self.aborted = true;
                return false;
            }
        }
        if !delta.content.is_empty() {
            return self.on_content(delta.content, emit);
        }
        true
    }

    fn on_tool_calls<F: FnMut(StreamEvent) -> bool>(
        &mut self,
        deltas: Vec<ToolCall>,
        emit: &mut F,
    ) -> bool {
        // Un appel clôt le texte : le reliquat retenu par la garde « </think> »
        // part MAINTENANT, avant l'événement d'outil.
        if self.think_open && !self.think_tail.is_empty() {
            let tail = std::mem::take(&mut self.think_tail);
            if !self.send(StreamEvent::Content(tail), emit) {
                self.aborted = true;
                return false;
            }
        }
        for (i, tc) in deltas.into_iter().enumerate() {
            // Le champ index dit à quel appel appartient ce morceau ; sans lui,
            // la position dans le chunk.
            let idx = tc.index.unwrap_or(i as i64);
            let cur = self.tool_calls.entry(idx).or_insert_with(|| ToolCall {
                kind: "function".into(),
                ..Default::default()
            });
            if !tc.id.is_empty() {
                cur.id = tc.id;
            }
            if !tc.function.name.is_empty() {
                cur.function.name = tc.function.name;
            }
            cur.function.arguments.push_str(&tc.function.arguments);
        }
        // Frappe en direct de l'appel n°0 : son argument parlant, et pour une
        // écriture la FIN de son corps, à la ligne.
        let Some(cur) = self.tool_calls.get(&0) else {
            return true;
        };
        let name = cur.function.name.clone();
        let key = typing_label_key(&name);
        if self.label_dec.as_ref().is_some_and(|d| d.key != key) {
            self.label_dec = None;
        }
        let ld = self.label_dec.get_or_insert_with(|| ArgPreview::new(key));
        ld.update(&cur.function.arguments);
        let p_nonempty = !ld.val.is_empty();
        let p_changed = p_nonempty && ld.val.as_slice() != self.last_preview.as_bytes();
        let mut full_nonempty = false;
        let mut body_nl = 0;
        if let Some(bk) = write_body_key(&name) {
            if self.body_dec.as_ref().is_some_and(|d| d.key != bk) {
                self.body_dec = None;
            }
            let bd = self.body_dec.get_or_insert_with(|| ArgPreview::new(bk));
            bd.update(&cur.function.arguments);
            full_nonempty = !bd.val.is_empty();
            body_nl = bd.lines as i64;
        }
        let grew = full_nonempty && body_nl > self.last_body_lines;
        if p_changed || grew {
            if p_nonempty && let Some(ld) = &self.label_dec {
                self.last_preview = ld.value().into_owned();
            }
            if grew {
                self.last_body_lines = body_nl;
            }
            let mut tu = ToolUsedEvent {
                name: name.clone(),
                label: self.last_preview.clone(),
                typing: true,
                ..Default::default()
            };
            if full_nonempty && let Some(bd) = &self.body_dec {
                // Seule la fin est copiée : relire tout le corps à chaque
                // ligne serait quadratique.
                let start = body_tail_start(&bd.val);
                tu.body = String::from_utf8_lossy(&bd.val[start..]).into_owned();
                tu.body_tail = start > 0;
                tu.body_lines = bd.line_count() as i64;
            }
            if !self.send(StreamEvent::ToolUsed(tu), emit) {
                self.aborted = true;
                return false;
            }
        }
        // Pré-vol du mode Code : dès que le chemin d'un write/edit est
        // complet, la garde décide ; refusé, on coupe la génération.
        if matches!(self.preflight, Preflight::Unchecked)
            && (name == "write" || name == "edit")
            && let Some(guard) = self.cfg.preflight.as_mut()
            && let Some(cur) = self.tool_calls.get(&0)
        {
            let (file, done) = preview_arg_done(&cur.function.arguments, "file");
            if done && !file.trim().is_empty() {
                let msg = guard(&file, name == "write");
                if !msg.is_empty() {
                    self.preflight = Preflight::Refused(PreflightRefusal {
                        name,
                        id: cur.id.clone(),
                        file,
                        msg,
                    });
                    return false;
                }
                self.preflight = Preflight::Checked;
            }
        }
        true
    }

    fn on_content<F: FnMut(StreamEvent) -> bool>(&mut self, c: String, emit: &mut F) -> bool {
        self.content.push_str(&c);
        if !self.think_open || self.saw_reasoning_field {
            let trigger = c.contains(['<', '`', '{', '[', '_', '.']);
            if !self.send(StreamEvent::Content(c), emit) {
                self.aborted = true;
                return false;
            }
            // Appel d'outil écrit en texte, repéré PENDANT le flux : on coupe
            // tout de suite. Balayage fenêtré après le précédent, seulement
            // quand le morceau peut ouvrir un motif.
            let mut hit = false;
            if ((self.cfg.calls_on && self.cfg.pattern_retries < MAX_PATTERN_RETRIES)
                || self.cfg.tool_choice_none)
                && trigger
            {
                hit = self
                    .cfg
                    .patterns
                    .textual_tool_call_from(&self.content, self.retry_scanned);
                self.retry_scanned = self.content.len();
            }
            if hit {
                if self.cfg.tool_choice_none {
                    self.none_leak = true;
                } else {
                    self.pattern_hit = true;
                }
                return false;
            }
            return true;
        }
        // <think> ouvert par le gabarit : le contenu part EN DIRECT comme
        // réponse, sauf une fin qui pourrait commencer un « </think> ».
        self.think_tail.push_str(&c);
        if let Some(i) = self.think_tail.find(THINK_CLOSE) {
            let s = std::mem::take(&mut self.think_tail);
            let reason = &s[..i];
            let after = s[i + THINK_CLOSE.len()..].trim_start_matches(['\r', '\n']);
            self.think_open = false;
            if !reason.is_empty() && !self.send(StreamEvent::Reasoning(reason.to_string()), emit) {
                self.aborted = true;
                return false;
            }
            if !after.is_empty() && !self.send(StreamEvent::Content(after.to_string()), emit) {
                self.aborted = true;
                return false;
            }
            return true;
        }
        let mut cut = self.think_tail.len() as isize - (THINK_CLOSE.len() as isize - 1);
        while cut > 0 && !self.think_tail.is_char_boundary(cut as usize) {
            cut -= 1;
        }
        if cut > 0 {
            let rest = self.think_tail.split_off(cut as usize);
            let out = std::mem::replace(&mut self.think_tail, rest);
            if !self.send(StreamEvent::Content(out), emit) {
                self.aborted = true;
                return false;
            }
        }
        true
    }

    /// Fin de lecture (EOF, coupure ou arrêt) : le reliquat retenu par la
    /// garde « </think> » part comme réponse, puis les stats sont complétées
    /// (`echoed`, TTFT, et sans `timings` un débit mesuré ici, émis par le
    /// rappel).
    pub fn finish<F: FnMut(StreamEvent) -> bool>(mut self, emit: &mut F) -> StreamOutcome {
        if !self.aborted && self.think_open && !self.think_tail.is_empty() {
            let tail = std::mem::take(&mut self.think_tail);
            let t = tail.trim_start_matches(['\r', '\n']).to_string();
            self.send(StreamEvent::Content(t), emit);
        }
        let echo_text = if self.cfg.echo_on && self.saw_reasoning_field {
            std::mem::take(&mut self.echo_buf)
        } else {
            String::new()
        };
        if !echo_text.is_empty() && (!self.tool_calls.is_empty() || self.cfg.echo_keeps_past) {
            self.stats.echoed = true;
        }
        let ms = |d: std::time::Duration| d.as_millis() as i64;
        if let Some(tf) = self.t_first {
            let ttft = ms(tf.saturating_duration_since(self.cfg.t_req));
            self.stats.ttft_ms = Some(ttft);
            if !self.saw_timings {
                // Décodage mesuré ici ; le 1er jeton tombe dans la lecture du prompt.
                let tl = self.t_last.unwrap_or(tf);
                let generated = if self.usage_gen == 0 {
                    self.gen_chunks // sans usage : un chunk ≈ un jeton
                } else {
                    self.usage_gen
                };
                let span = tl.saturating_duration_since(tf);
                self.stats.prompt_ms = ttft as f64;
                self.stats.gen_tokens = generated;
                self.stats.gen_ms = ms(span) as f64;
                let sec = span.as_secs_f64();
                if sec > 0.0 && generated > 1 {
                    self.stats.gen_per_second = (generated - 1) as f64 / sec;
                }
                emit(StreamEvent::Stats(self.stats.clone()));
            }
        }
        StreamOutcome {
            content: self.content,
            shown: self.shown,
            tool_calls: self.tool_calls,
            finish_reason: self.finish_reason,
            stats: self.stats,
            saw_timings: self.saw_timings,
            saw_reasoning_field: self.saw_reasoning_field,
            echo_text,
            sent_answer: self.sent_answer,
            sent_reasoning: self.sent_reasoning,
            typing_tool: self.typing_tool,
            aborted: self.aborted,
            pattern_hit: self.pattern_hit,
            none_leak: self.none_leak,
            preflight: match self.preflight {
                Preflight::Refused(r) => Some(r),
                _ => None,
            },
        }
    }
}

// ───────────────────────── Encodages façon Go ─────────────────────────

/// Mantisse (signe compris) et exposant décimal de la forme la plus courte.
fn sci_parts(f: f64) -> (String, i32) {
    let s = format!("{f:e}");
    match s.split_once('e') {
        Some((m, e)) => (m.to_string(), e.parse().unwrap_or(0)),
        None => (s, 0),
    }
}

fn go_exp(m: &str, e: i32, min_digits: usize) -> String {
    let sign = if e < 0 { '-' } else { '+' };
    format!("{m}e{sign}{:0w$}", e.unsigned_abs(), w = min_digits)
}

/// Nombre JSON comme `encoding/json` (float64) : 'f' le plus court, 'e' hors
/// de [1e-6, 1e21[ (« e-7 », « e+21 »).
fn go_json_float(f: f64) -> String {
    let a = f.abs();
    if a != 0.0 && !(1e-6..1e21).contains(&a) {
        let (m, e) = sci_parts(f);
        return go_exp(&m, e, if e < 0 { 1 } else { 2 });
    }
    format!("{f}")
}

/// `%v` d'un float64 (`%g` le plus court de strconv : forme 'e' si
/// l'exposant est < -4 ou >= 6, « 1e+06 »).
fn go_fmt_float(f: f64) -> String {
    let (m, e) = sci_parts(f);
    if !(-4..6).contains(&e) {
        return go_exp(&m, e, 2);
    }
    format!("{f}")
}

/// `fmt.Sprintf("%v", v)` d'une valeur décodée en `any`.
fn go_fmt_v(v: &Value) -> String {
    match v {
        Value::Null => "<nil>".into(),
        Value::Bool(b) => b.to_string(),
        Value::Number(n) => go_fmt_float(n.as_f64().unwrap_or(0.0)),
        Value::String(s) => s.clone(),
        Value::Array(a) => format!("[{}]", a.iter().map(go_fmt_v).collect::<Vec<_>>().join(" ")),
        Value::Object(o) => {
            let mut keys: Vec<&String> = o.keys().collect();
            keys.sort();
            let parts: Vec<String> = keys
                .into_iter()
                .map(|k| format!("{k}:{}", go_fmt_v(&o[k])))
                .collect();
            format!("map[{}]", parts.join(" "))
        }
    }
}

/// Chaîne JSON comme `encoding/json` (échappement HTML, U+2028/2029).
fn go_json_string(s: &str, out: &mut String) {
    out.push('"');
    for c in s.chars() {
        match c {
            '\\' => out.push_str("\\\\"),
            '"' => out.push_str("\\\""),
            '\u{8}' => out.push_str("\\b"),
            '\u{c}' => out.push_str("\\f"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '<' | '>' | '&' => out.push_str(&format!("\\u{:04x}", c as u32)),
            c if (c as u32) < 0x20 => out.push_str(&format!("\\u{:04x}", c as u32)),
            '\u{2028}' => out.push_str("\\u2028"),
            '\u{2029}' => out.push_str("\\u2029"),
            c => out.push(c),
        }
    }
    out.push('"');
}

/// `json.Marshal` d'une valeur décodée en `any` (nombres en float64, clés
/// triées).
fn go_marshal(v: &Value, out: &mut String) {
    match v {
        Value::Null => out.push_str("null"),
        Value::Bool(b) => out.push_str(if *b { "true" } else { "false" }),
        Value::Number(n) => out.push_str(&go_json_float(n.as_f64().unwrap_or(0.0))),
        Value::String(s) => go_json_string(s, out),
        Value::Array(a) => {
            out.push('[');
            for (i, x) in a.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                go_marshal(x, out);
            }
            out.push(']');
        }
        Value::Object(o) => go_marshal_object(o, out),
    }
}

fn go_marshal_object(o: &Map<String, Value>, out: &mut String) {
    let mut keys: Vec<&String> = o.keys().collect();
    keys.sort();
    out.push('{');
    for (i, k) in keys.into_iter().enumerate() {
        if i > 0 {
            out.push(',');
        }
        go_json_string(k, out);
        out.push(':');
        go_marshal(&o[k], out);
    }
    out.push('}');
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::chat::types::ToolFunction;
    use serde_json::json;
    use std::time::Duration;

    // ── Flux enregistrés des tests Go (sseChunk, sseReasoning, sseFinal…) ──

    fn sse_chunk(text: &str) -> String {
        format!("data: {{\"choices\":[{{\"delta\":{{\"content\":\"{text}\"}}}}]}}\n\n")
    }

    fn sse_reasoning(text: &str) -> String {
        format!("data: {{\"choices\":[{{\"delta\":{{\"reasoning_content\":\"{text}\"}}}}]}}\n\n")
    }

    fn sse_final(finish: &str, prompt: i64, generated: i64) -> String {
        format!(
            "data: {{\"choices\":[{{\"delta\":{{}},\"finish_reason\":\"{finish}\"}}]}}\n\n\
             data: {{\"choices\":[],\"timings\":{{\"prompt_n\":10,\"predicted_n\":{generated}}},\
             \"usage\":{{\"prompt_tokens\":{prompt},\"completion_tokens\":{generated}}}}}\n\n"
        )
    }

    const SSE_STOP: &str = "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n";
    const SSE_DONE: &str = "data: [DONE]\n\n";
    const SSE_GLOB_STEP: &str = concat!(
        r#"data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"g1","type":"function","function":{"name":"glob","arguments":"{\"pattern\":\"*.go\"}"}}]}}]}"#,
        "\n\n",
        r#"data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}"#,
        "\n\n"
    );

    fn agent() -> StreamConfig {
        StreamConfig {
            calls_on: true,
            ..Default::default()
        }
    }

    /// Rejoue un corps SSE ligne par ligne (bufio.Scanner coupe sur '\n'),
    /// jusqu'à la coupure éventuelle, puis `finish`.
    fn replay(cfg: StreamConfig, body: &str) -> (Vec<StreamEvent>, StreamOutcome) {
        let mut evs = Vec::new();
        let mut emit = |ev: StreamEvent| {
            evs.push(ev);
            true
        };
        let mut p = StreamParser::new(cfg);
        for line in body.split('\n') {
            if !p.feed_line(line, Instant::now(), &mut emit) {
                break;
            }
        }
        let out = p.finish(&mut emit);
        (evs, out)
    }

    fn contents(evs: &[StreamEvent]) -> String {
        evs.iter()
            .filter_map(|e| match e {
                StreamEvent::Content(c) => Some(c.as_str()),
                _ => None,
            })
            .collect()
    }

    fn reasonings(evs: &[StreamEvent]) -> Vec<String> {
        evs.iter()
            .filter_map(|e| match e {
                StreamEvent::Reasoning(r) => Some(r.clone()),
                _ => None,
            })
            .collect()
    }

    fn last_stats(evs: &[StreamEvent]) -> Option<StatsEvent> {
        evs.iter().rev().find_map(|e| match e {
            StreamEvent::Stats(s) => Some(s.clone()),
            _ => None,
        })
    }

    fn tool_events(evs: &[StreamEvent]) -> Vec<ToolUsedEvent> {
        evs.iter()
            .filter_map(|e| match e {
                StreamEvent::ToolUsed(t) => Some(t.clone()),
                _ => None,
            })
            .collect()
    }

    fn obj(v: Value) -> Map<String, Value> {
        match v {
            Value::Object(o) => o,
            _ => Map::new(),
        }
    }

    fn marshal(v: &Value) -> String {
        let mut s = String::new();
        go_marshal(v, &mut s);
        s
    }

    const BACKSLASH: char = '\x5c';

    /// Échappement JSON `\uXXXX` attendu.
    fn esc(hex: &str) -> String {
        format!("{BACKSLASH}u{hex}")
    }

    /// Générateur reproductible (les tests Go utilisent math/rand à graine fixe).
    struct Rng(u64);

    impl Rng {
        fn new(seed: u64) -> Self {
            Rng(seed.wrapping_mul(0x9E37_79B9_7F4A_7C15) | 1)
        }
        fn intn(&mut self, n: usize) -> usize {
            self.0 ^= self.0 << 13;
            self.0 ^= self.0 >> 7;
            self.0 ^= self.0 << 17;
            (self.0 % n as u64) as usize
        }
    }

    /// `chunked` : morceaux de 1 à max octets — prolongés jusqu'à une
    /// frontière de caractère (un &str ne peut pas couper un caractère ; les
    /// arguments réels arrivent toujours en chaînes JSON entières).
    fn chunked(s: &str, r: &mut Rng, max: usize) -> Vec<String> {
        let mut out = Vec::new();
        let mut s = s;
        while !s.is_empty() {
            let mut n = (1 + r.intn(max)).min(s.len());
            while !s.is_char_boundary(n) {
                n += 1;
            }
            out.push(s[..n].to_string());
            s = &s[n..];
        }
        out
    }

    // ── code_alias_test.go ──

    fn outils(noms: &[&str]) -> Vec<Tool> {
        noms.iter()
            .map(|n| Tool {
                kind: "function".into(),
                function: ToolFunction {
                    name: n.to_string(),
                    ..Default::default()
                },
            })
            .collect()
    }

    /// TestReparationDesAppelsHallucines
    #[test]
    fn reparation_des_appels_hallucines() {
        let tools = outils(&["read", "edit", "bash", "subagent"]);
        let cas = [
            (
                "read_file",
                r#"{"path":"a.go"}"#,
                "read",
                json!({"file": "a.go"}),
            ),
            (
                "str_replace",
                r#"{"file_path":"a.go","old_string":"x","new_string":"y"}"#,
                "edit",
                json!({"file": "a.go", "old": "x", "new": "y"}),
            ),
            (
                "run_command",
                r#"{"cmd":"ls"}"#,
                "bash",
                json!({"command": "ls"}),
            ),
            (
                "edit",
                r#"{"file":"a.go","old_str":"x","new":"y"}"#,
                "edit",
                json!({"file": "a.go", "old": "x", "new": "y"}),
            ),
            (
                "code_reviewer",
                r#"{"prompt":"relis"}"#,
                "subagent",
                json!({"role": "code-reviewer", "task": "relis"}),
            ),
        ];
        for (name, args, want_name, want_args) in cas {
            let mut tc = ToolCall::default();
            tc.function.name = name.into();
            tc.function.arguments = args.into();
            assert!(repair_tool_call(&mut tc, &tools), "{name} : non réparé");
            assert_eq!(tc.function.name, want_name, "{name}");
            assert_eq!(call_args(&tc.function.arguments), obj(want_args), "{name}");
        }
        // Outil cible absent de ce tour, ou appel déjà correct : intact.
        for (name, args) in [
            ("write_file", r#"{"path":"a"}"#),
            ("read", r#"{"file":"a"}"#),
        ] {
            let mut tc = ToolCall::default();
            tc.function.name = name.into();
            tc.function.arguments = args.into();
            let before = tc.clone();
            assert!(
                !repair_tool_call(&mut tc, &tools),
                "modifié à tort : {tc:?}"
            );
            assert_eq!(tc, before);
        }
    }

    // ── code_retry_test.go ──

    /// TestAppelOutilTextuelDetecte
    #[test]
    fn appel_outil_textuel_detecte() {
        let p = RetryPatterns::builtin();
        for s in [
            "<tool_call>{\"name\":\"bash\"}</tool_call>",
            "Je vais lancer default_api:bash pour vérifier.",
            "default_api.run_shell(command='ls')",
            "functions.bash({\"command\": \"ls\"})",
            "```tool_code\nprint('x')\n```",
            "{\"name\": \"bash\", \"arguments\": {\"command\": \"ls\"}}",
        ] {
            assert!(p.textual_tool_call(s), "non détecté : {s:?}");
        }
    }

    /// TestReponseNormaleNonRelancee
    #[test]
    fn reponse_normale_non_relancee() {
        let p = RetryPatterns::builtin();
        for s in [
            "Voici la réponse : le fichier contient trois fonctions.",
            "```go\nfunc main() {}\n```",
            "Le JSON de config : {\"name\": \"loki\", \"port\": 8090}",
            "La fonction bash() de ce script accepte un paramètre.",
            "",
        ] {
            assert!(!p.textual_tool_call(s), "relance à tort : {s:?}");
        }
    }

    /// TestAppelOutilQwenXMLDetecte
    #[test]
    fn appel_outil_qwen_xml_detecte() {
        let p = RetryPatterns::builtin();
        let s = "Je regarde.\n<function=bash>\n<parameter=command>ls -la</parameter>\n</function>";
        let snip = p.snippet(s);
        assert!(snip.starts_with("<function=bash>"), "extrait = {snip:?}");
        assert!(
            !p.textual_tool_call("Un <div class=\"x\"> et un <param name=\"a\"> en HTML."),
            "HTML ordinaire pris pour un appel d'outil"
        );
    }

    /// TestAppelTextuelCoupeLeFlux (partie analyse) : le flux est coupé sur
    /// l'appel écrit en texte, la suite n'est jamais émise, et la relance cite
    /// l'extrait fautif.
    #[test]
    fn appel_textuel_coupe_le_flux() {
        let body = sse_chunk("Je lance <tool_call>") + &sse_chunk("FIN-INUTILE") + SSE_STOP;
        let (evs, out) = replay(agent(), &body);
        assert!(out.pattern_hit && !out.none_leak);
        assert!(!contents(&evs).contains("FIN-INUTILE"), "flux non coupé");
        assert!(out.read_error_benign());
        let second = retry_corrective(&RetryPatterns::builtin().snippet(&out.content));
        assert!(
            second.contains("<tool_call>") && second.contains("WRITTEN AS TEXT"),
            "{second:?}"
        );
        // Budget de relances épuisé : plus de coupure, le texte va au bout.
        let cfg = StreamConfig {
            pattern_retries: MAX_PATTERN_RETRIES,
            ..agent()
        };
        let (evs, out) = replay(cfg, &body);
        assert!(!out.pattern_hit && contents(&evs).ends_with("FIN-INUTILE"));
    }

    // ── perf_log_test.go ──

    const LLAMA_FINAL_CHUNK: &str = concat!(
        r#"{"choices":[],"created":1,"id":"x","model":"m","object":"chat.completion.chunk","#,
        r#""usage":{"completion_tokens":120,"prompt_tokens":41484,"total_tokens":41604,"prompt_tokens_details":{"cached_tokens":41230}},"#,
        r#""timings":{"cache_n":41230,"prompt_n":254,"prompt_ms":310.2,"prompt_per_token":1.2,"prompt_per_second":818.8,"#,
        r#""predicted_n":120,"predicted_ms":2400,"predicted_per_token":20,"predicted_per_second":50,"draft_n":90,"draft_n_accepted":71}}"#
    );

    /// TestPerfWireDecode
    #[test]
    fn perf_wire_decode() {
        /// (cas, chunk, cache, brouillon, accepté)
        type Cas = (&'static str, String, Option<i64>, Option<i64>, Option<i64>);
        let cas: [Cas; 8] = [
            (
                "chunk final llama.cpp",
                LLAMA_FINAL_CHUNK.into(),
                Some(41230),
                Some(90),
                Some(71),
            ),
            // Sans brouillon, llama.cpp n'émet pas draft_n : inconnu, pas 0.
            (
                "sans brouillon",
                r#"{"timings":{"cache_n":12,"prompt_n":3}}"#.into(),
                Some(12),
                None,
                None,
            ),
            // Tout recalculé : un 0 CONNU, qu'il faut pouvoir montrer.
            (
                "cache vide",
                r#"{"timings":{"cache_n":0,"prompt_n":3000}}"#.into(),
                Some(0),
                None,
                None,
            ),
            // API tierce : seulement le format OpenAI.
            (
                "usage seul",
                r#"{"usage":{"prompt_tokens":900,"prompt_tokens_details":{"cached_tokens":512}}}"#
                    .into(),
                Some(512),
                None,
                None,
            ),
            (
                "moteur muet",
                r#"{"usage":{"prompt_tokens":900}}"#.into(),
                None,
                None,
                None,
            ),
            // Types de travers : lu si c'est un nombre, sinon inconnu.
            (
                "types de travers",
                r#"{"timings":{"cache_n":"abc","draft_n":"12","draft_n_accepted":7.0}}"#.into(),
                None,
                Some(12),
                Some(7),
            ),
            // Octets parasites après la réponse : la première valeur compte.
            (
                "octets parasites",
                r#"{"timings":{"cache_n":12}}"#.to_string() + "\nxyz",
                Some(12),
                None,
                None,
            ),
            (
                "structure de travers",
                r#"{"timings":"nope","usage":{"prompt_tokens_details":{"cached_tokens":5.9}}}"#
                    .into(),
                Some(5),
                None,
                None,
            ),
        ];
        for (name, data, cache, draft, accept) in cas {
            let mut s = StatsEvent::default();
            apply_perf(&mut s, &decode_perf_wire(&data));
            assert_eq!(
                (s.cache_tokens, s.draft_n, s.draft_accepted),
                (cache, draft, accept),
                "{name}"
            );
        }
    }

    /// TestStreamChunkIgnoresOddTelemetry
    #[test]
    fn stream_chunk_ignores_odd_telemetry() {
        let data = concat!(
            r#"{"choices":[{"delta":{"content":"fin"},"finish_reason":"stop"}],"#,
            r#""usage":{"prompt_tokens":500,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":480.5}},"#,
            r#""timings":{"prompt_n":20,"predicted_n":3,"draft_n":"x","cache_n":480.0}}"#
        );
        let c = decode_stream_chunk(data).expect("chunk jeté");
        assert_eq!(c.choices.len(), 1);
        assert_eq!(c.choices[0].delta.content, "fin");
        assert_eq!(c.choices[0].finish_reason, "stop");
        assert_eq!(c.usage.as_ref().map(|u| u.prompt_tokens), Some(500));
        let mut s = StatsEvent::default();
        apply_perf(&mut s, &decode_perf_wire(data));
        assert_eq!((s.cache_tokens, s.draft_n), (Some(480), None));
    }

    /// TestRunChatPerfFinalStats (partie analyse) : le dernier Stats porte la
    /// télémétrie, le texte est intact, la complétion est complète.
    #[test]
    fn run_chat_perf_final_stats() {
        let body = sse_chunk("bonjour")
            + r#"data: {"choices":[{"delta":{"content":" toi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":500,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":480.5}},"timings":{"prompt_n":20,"prompt_ms":10,"prompt_per_second":2000,"predicted_n":2,"predicted_ms":40,"predicted_per_second":50,"draft_n":"x"}}"#
            + "\n\n"
            + SSE_DONE;
        let seen = Arc::new(std::sync::Mutex::new(Vec::new()));
        let seen2 = seen.clone();
        let cfg = StreamConfig {
            cache_hint: Some(Box::new(move |p, c, ok| {
                seen2.lock().unwrap().push((p, c, ok));
                "conseil".into()
            })),
            ..Default::default()
        };
        let (evs, out) = replay(cfg, &body);
        assert_eq!(contents(&evs), "bonjour toi");
        let last = last_stats(&evs).expect("stats");
        assert_eq!(last.prompt_tokens_total, 500);
        assert_eq!(last.cache_tokens, Some(480));
        assert_eq!(last.cache_hint, "conseil");
        assert_eq!(*seen.lock().unwrap(), vec![(500, 480, true)]);
        // Le conseil ne reste pas dans les stats accumulées.
        assert!(out.stats.cache_hint.is_empty());
        assert_eq!(out.stats.prompt_tokens, 20);
        assert_eq!(out.stats.gen_tokens, 2);
        assert!(out.stats.ttft_ms.is_some());
        assert!(out.is_complete(false));
    }

    /// TestRunChatPerfCutStreamIncomplete : flux coupé, ni cache ni total.
    #[test]
    fn run_chat_perf_cut_stream_incomplete() {
        let (_, out) = replay(StreamConfig::default(), &sse_chunk("début"));
        assert!(!out.is_complete(true));
        assert!(!out.read_error_benign());
    }

    // ── llm_stream_cut_test.go (partie analyse) ──

    /// TestFluxCoupeRemonteUneErreur / TestFluxCompletNeRemonteRien : le texte
    /// reçu avant la coupure est émis ; sans finish_reason, la coupure n'est
    /// pas bénigne (la boucle remonte l'erreur).
    #[test]
    fn flux_coupe_et_flux_complet() {
        let (evs, out) = replay(StreamConfig::default(), &sse_chunk("début de r"));
        assert_eq!(contents(&evs), "début de r");
        assert!(!out.read_error_benign());
        let (evs, out) = replay(
            StreamConfig::default(),
            &(sse_chunk("réponse complète") + SSE_DONE),
        );
        assert_eq!(contents(&evs), "réponse complète");
        assert_eq!(out.shown, "réponse complète");
        assert!(out.sent_answer && !out.sent_reasoning);
    }

    // ── chat_vision_tool_test.go ──

    /// TestLeCheminDeLImageRemonteJusquALOutil
    #[test]
    fn le_chemin_de_l_image_remonte_jusqu_a_l_outil() {
        let args = obj(json!({"file": "captures/bug.png"}));
        assert_eq!(tool_call_label("see_image", &args), "captures/bug.png");
    }

    /// TestLibellesDesAutresOutilsInchanges
    #[test]
    fn libelles_des_autres_outils_inchanges() {
        for (tool, key, val, want) in [
            ("bash", "command", "ls -la", "ls -la"),
            ("read", "file", "main.go", "main.go"),
            ("web_search", "query", "météo", "météo"),
            ("grep", "pattern", "TODO", "TODO"),
            ("recall", "id", "b12", "b12"),
            ("git_clone", "url", "https://x/y", "https://x/y"),
            ("outil_inconnu", "file", "x", ""),
        ] {
            let args = obj(json!({ key: val }));
            assert_eq!(tool_call_label(tool, &args), want, "{tool}");
        }
        let args = obj(json!({"url": "https://x", "pattern": "p"}));
        assert_eq!(tool_call_label("web_grep", &args), "p @ https://x");
        let args = obj(json!({"action": "add", "name": "poids"}));
        assert_eq!(tool_call_label("tracker", &args), "add poids");
        // Argument absent ou du mauvais type : chaîne vide, jamais de panique.
        assert_eq!(tool_call_label("read", &obj(json!({"file": 42}))), "");
        assert_eq!(tool_call_label("bash", &call_args("null")), "");
    }

    /// Libellés tolérants au type (`str`) et outils MCP (`mcpArgLabel`).
    #[test]
    fn libelles_tracker_et_mcp() {
        let l = |v: Value| tool_call_label("tracker", &obj(v));
        assert_eq!(l(json!({"action": 2.5, "name": 1e6})), "2 1000000");
        assert_eq!(l(json!({"action": true})), "true");
        assert_eq!(
            l(json!({"name": [1, "a", 1234567.0, {"b": null}]})),
            "[1 a 1.234567e+06 map[b:<nil>]]"
        );
        assert_eq!(l(json!({})), "");
        let m = |v: Value| tool_call_label("mcp__srv__x", &obj(v));
        assert_eq!(m(json!({})), "");
        assert_eq!(m(json!({"name": "n", "path": "p"})), "p");
        let want = format!(
            "{{\"a\":1e+21,\"path\":\"\",\"z\":\"{}{}{}\"}}",
            esc("003c"),
            esc("0026"),
            esc("003e")
        );
        assert_eq!(m(json!({"path": "", "z": "<&>", "a": 1e21})), want);
        let long = m(json!({"z": "é".repeat(200)}));
        assert_eq!(long.chars().count(), 121);
        assert!(long.ends_with('…'));
        assert_eq!(tool_call_label("autre__x", &obj(json!({"path": "p"}))), "");
    }

    // ── stream_hotpath_test.go ──

    /// checkArgPreview : à chaque préfixe, ArgPreview = preview_arg_done (à une
    /// barre oblique finale près).
    fn check_arg_preview(args: &str, key: &str, chunks: &[String]) {
        let mut a = ArgPreview::new(key);
        let mut prefix = String::new();
        for c in chunks {
            prefix.push_str(c);
            a.update(&prefix);
            let (want, want_done) = preview_arg_done(&prefix, key);
            let mut got = a.value().into_owned();
            if a.phase == 3 && a.pos + 1 == prefix.len() && prefix.as_bytes()[a.pos] == b'\\' {
                got.push('\\');
            }
            assert!(
                got == want && a.done() == want_done,
                "args={args:?} préfixe={prefix:?} : argPreview=({got:?},{}), previewArgDone=({want:?},{want_done})",
                a.done()
            );
            assert_eq!(a.value().matches('\n').count(), a.newlines());
            assert_eq!(
                body_line_count(&a.value()),
                a.line_count(),
                "valeur {:?}",
                a.value()
            );
        }
    }

    /// TestArgPreviewMatchesPreviewArgDone (+ graines de FuzzArgPreview)
    #[test]
    fn arg_preview_matches_preview_arg_done() {
        let mut r = Rng::new(7);
        let alphabet = [
            "a", "é", "\n", "\t", "\r", "\\", "\"", "<", "&", " ", "/", "\u{2028}", "x", "content",
            ":", "{",
        ];
        let mut cases = Vec::new();
        for _ in 0..300 {
            let v: String = (0..r.intn(200))
                .map(|_| alphabet[r.intn(alphabet.len())])
                .collect();
            cases.push(marshal(&json!({"file": "a/b.go", "content": v})));
            // Ordre inverse et clé citée avant la vraie : même règle de première occurrence.
            let esc = v.replace('"', "\\\"");
            cases.push(format!(r#"{{"content"  :  "{esc}","file":"x"}}"#));
            cases.push(format!(
                r#"{{"note":"le champ \"content\" vient après","content":"{esc}"}}"#
            ));
        }
        for s in [
            r#"{"content":"été \\u0041 fin"}"#,
            r#"{"content":"ligne1\nligne2\\"#,
            r#"{"content":"#,
            r#"{"cont"#,
            r#"{"content":""#,
            r#"{"content":"x"}"#,
            "",
        ] {
            cases.push(s.to_string());
        }
        for args in &cases {
            for max in [1, 3, 17, 4096] {
                check_arg_preview(args, "content", &chunked(args, &mut r, max));
            }
        }
        // Graines du fuzz Go.
        for (args, seed) in [
            (r#"{"file":"a","content":"x\ny\\\"zA"}"#, 1),
            (r#"{"content":"a\\"#, 2),
            (r#"{"content" : "x", "content":"y"}"#, 3),
        ] {
            check_arg_preview(args, "content", &chunked(args, &mut Rng::new(seed), 9));
        }
    }

    /// TestBodyTail
    #[test]
    fn body_tail_borne() {
        let lines = |n: usize, w: usize| -> String {
            (0..n)
                .map(|i| "x".repeat(w) + &i.to_string() + "\n")
                .collect()
        };
        let long = "é".repeat(5000); // une seule ligne de 10 000 octets
        for (name, input, cut, max_line) in [
            ("vide", String::new(), false, 0),
            ("court", "a\nb\nc".to_string(), false, 3),
            ("40 lignes pile", lines(39, 3) + "fin", false, 40),
            (
                "beaucoup de lignes",
                lines(500, 3) + "fin",
                true,
                BODY_TAIL_LINES,
            ),
            ("lignes larges", lines(100, 300), true, BODY_TAIL_LINES),
            ("ligne seule énorme", long, true, 1),
        ] {
            let (got, c) = body_tail(&input);
            assert!(c == cut && input.ends_with(got), "{name} : cut={c}");
            assert!(
                got.len() <= BODY_TAIL_BYTES,
                "{name} : {} octets",
                got.len()
            );
            let n = got.matches('\n').count() + 1;
            assert!(got.is_empty() || n <= max_line, "{name} : {n} lignes");
            if c {
                let start = input.len() - got.len();
                assert!(
                    input.as_bytes()[start - 1] == b'\n' || name.contains("seule"),
                    "{name} : coupé en milieu de ligne"
                );
                assert!(!got.contains('\u{FFFD}'), "{name} : caractère coupé");
            }
        }
    }

    /// TestBodyLineCount
    #[test]
    fn body_line_count_comme_l_ui() {
        for (input, want) in [
            ("", 0),
            ("\n", 0),
            ("a", 1),
            ("a\n", 1),
            ("a\nb", 2),
            ("a\nb\n", 2),
            ("\n\n", 2),
        ] {
            assert_eq!(body_line_count(input), want, "{input:?}");
        }
    }

    /// TestWriteTypingBodyBounded (partie analyse) : les événements de frappe
    /// d'un gros write ne portent qu'une fin bornée du corps, avec le vrai
    /// nombre de lignes ; les arguments assemblés restent entiers.
    #[test]
    fn write_typing_body_bounded() {
        let content: String = (0..3000)
            .map(|i| format!("ligne numéro {i} du fichier\n"))
            .collect();
        let args = marshal(&json!({"file": "gros.txt", "content": content}));
        let mut body = String::new();
        let mut s = args.as_str();
        while !s.is_empty() {
            let mut n = 37.min(s.len());
            while !s.is_char_boundary(n) {
                n += 1;
            }
            let piece = marshal(&Value::String(s[..n].to_string()));
            s = &s[n..];
            body += &format!(
                r#"data: {{"choices":[{{"delta":{{"tool_calls":[{{"index":0,"id":"w1","type":"function","function":{{"name":"write","arguments":{piece}}}}}]}}}}]}}"#
            );
            body += "\n\n";
        }
        body += r#"data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}"#;
        body += "\n\n";
        body += SSE_DONE;
        let (evs, out) = replay(agent(), &body);
        let (mut sent, mut last) = (0, 0);
        let mut last_ev = None;
        for tu in tool_events(&evs) {
            if !tu.typing || tu.body.is_empty() {
                continue;
            }
            assert!(
                tu.body.len() <= BODY_TAIL_BYTES,
                "corps de {} octets",
                tu.body.len()
            );
            assert!(
                tu.body_lines >= last,
                "body_lines recule : {} après {last}",
                tu.body_lines
            );
            last = tu.body_lines;
            sent += tu.body.len();
            last_ev = Some(tu);
        }
        let last_ev = last_ev.expect("aucun événement de frappe");
        assert!(
            last_ev.body_tail && last_ev.body_lines >= 2990,
            "{:?}",
            last_ev.body_lines
        );
        assert!(content.ends_with(&last_ev.body));
        assert!(
            sent <= 3000 * BODY_TAIL_BYTES,
            "{sent} octets de corps diffusés"
        );
        let calls = out.assemble_tool_calls(0, &outils(&["write"]));
        assert!(calls.bad_args.is_empty());
        assert_eq!(calls.calls.len(), 1);
        assert_eq!(calls.calls[0].function.arguments, args);
        assert_eq!(out.finish_reason, "tool_calls");
    }

    /// replayScan : un morceau sans caractère déclencheur n'est pas balayé.
    /// Rend l'indice du morceau qui coupe le flux.
    fn replay_scan(p: &RetryPatterns, chunks: &[String], windowed: bool) -> Option<usize> {
        let mut b = String::new();
        let mut scanned = 0;
        for (i, c) in chunks.iter().enumerate() {
            b.push_str(c);
            if !c.contains(['<', '`', '{', '[', '_', '.']) {
                continue;
            }
            let hit = if windowed {
                let h = p.textual_tool_call_from(&b, scanned);
                scanned = b.len();
                h
            } else {
                p.textual_tool_call(&b)
            };
            if hit {
                return Some(i);
            }
        }
        None
    }

    /// TestTextualToolCallWindowMatchesFullScan
    #[test]
    fn textual_tool_call_window_matches_full_scan() {
        let p = RetryPatterns::builtin();
        let mut r = Rng::new(11);
        let prose = [
            "Voici la suite de l'analyse, ligne par ligne. ",
            "Le fichier contient trois fonctions ; aucune n'est exportée.\n",
            "```go\nfunc main() { fmt.Println(\"x\") }\n```\n",
            "- point important\n",
            "le JSON {\"name\": \"loki\"} de config, ",
            "sans ponctuation du tout mais avec des mots très longs ",
            "\n\n",
            "Un <div class=\"x\"> en HTML. ",
            "été à la plage ",
        ];
        let filler = |r: &mut Rng, n: usize| {
            let mut b = String::new();
            while b.len() < n {
                b.push_str(prose[r.intn(prose.len())]);
            }
            b
        };
        let bad = [
            "<tool_call>{\"name\":\"bash\"}</tool_call>".to_string(),
            "\n{\"name\": \"bash\", \"arguments\": {}}".to_string(),
            "\n   \n  {\"name\":".to_string()
                + &" \n".repeat(400)
                + "\"mem_"
                + &"a".repeat(700)
                + "\"",
            "\n".to_string() + &" ".repeat(900) + "{\"name\": \"web_open\"",
            "<function=".to_string() + &"b".repeat(900) + ">",
            " functions.".to_string() + &"c".repeat(600) + &" ".repeat(300) + "(",
            "Je lance default_api:bash ".to_string(),
            "```tool_code\nprint(1)\n```".to_string(),
            "<parameter=command>".to_string(),
            "[TOOL_REQUEST]".to_string(),
        ];
        let neutral = [
            " milieu de ligne {\"name\": \"bash\"} ".to_string(), // `^` : début de ligne seulement
            "x".repeat(800) + " {\"name\": \"bash\"",
            "<tool_cal> presque".to_string(),
        ];
        let mut texts = Vec::new();
        for i in 0..120 {
            let hl = 500 + r.intn(5000);
            let head = filler(&mut r, hl);
            let tl = r.intn(1500);
            let tail = filler(&mut r, tl);
            match i % 3 {
                0 => texts.push(head + &bad[r.intn(bad.len())] + &tail),
                1 => texts.push(head + &neutral[r.intn(neutral.len())] + &tail),
                _ => texts.push(head + &tail),
            }
        }
        // Motif à cheval sur un morceau sans caractère déclencheur.
        let lead = filler(&mut r, 3000);
        texts.push(lead.clone() + "<tool_call> puis du texte sans rien");
        let (mut hits, mut misses) = (0, 0);
        for (ti, s) in texts.iter().enumerate() {
            for max in [3, 23, 200] {
                let ch = if ti == texts.len() - 1 {
                    let mut ch = chunked(&lead, &mut r, max);
                    for x in ["<tool", "_call", ">", " puis du texte", " sans rien"] {
                        ch.push(x.to_string());
                    }
                    ch
                } else {
                    chunked(s, &mut r, max)
                };
                let (w, f) = (replay_scan(&p, &ch, true), replay_scan(&p, &ch, false));
                assert_eq!(w, f, "fenêtré ≠ complet (taille {}, max {max})", s.len());
                if f.is_some() {
                    hits += 1;
                } else {
                    misses += 1;
                }
            }
        }
        assert!(
            hits > 0 && misses > 0,
            "corpus déséquilibré : {hits} coupures, {misses} sans"
        );
    }

    /// TestRetryScanStartWindows
    #[test]
    fn retry_scan_start_windows() {
        let s = "Une phrase ordinaire, avec sa ponctuation.\n".repeat(500);
        let start = retry_scan_start(&s, s.len());
        assert!(
            start != 0 && s.len() - start <= 2 * RETRY_SCAN_MARGIN,
            "fenêtre de {} octets",
            s.len() - start
        );
        assert_eq!(
            s.as_bytes()[start - 1],
            b'\n',
            "la fenêtre ne démarre pas en début de ligne"
        );
        // Une ligne sans fin plus longue que le recul permis : balayage complet.
        assert_eq!(retry_scan_start(&"a b ".repeat(20000), 80000), 0);
    }

    /// La surcharge `retry_patterns` : tableau JSON de regex du Go, chacune
    /// en `(?mi)`, sémantique du Go (classes et limites de mot ASCII).
    #[test]
    fn surcharge_retry_patterns() {
        for raw in ["", "null", "[]", "[\"a\", 1]", "\"x\"", "[\"a\"] x"] {
            assert!(RetryPatterns::from_override(raw).is_none(), "{raw}");
        }
        let p = RetryPatterns::from_override(r#"["^oups", "[", "\\bdo_\\w+\\("]"#).unwrap();
        assert!(p.is_custom());
        assert_eq!(p.len(), 2, "la regex illisible est sautée");
        assert!(p.textual_tool_call("Bonjour\nOUPS !"));
        assert!(p.textual_tool_call("éxécute do_it(1)"));
        assert_eq!(
            p.snippet("puis do_it(1) et la suite"),
            "do_it(1) et la suite"
        );
        assert!(!p.textual_tool_call("ado_it(1)"));
        // Classes du Go : ASCII.
        let m = |pat: &str, s: &str| {
            compile_go_regex(&format!("(?mi){pat}"))
                .unwrap()
                .find(s)
                .map(|m| m.as_str().to_string())
        };
        assert_eq!(m(r"\w+", "été"), Some("t".into()));
        assert_eq!(m(r"\bfoo\b", "éfoo"), Some("foo".into()));
        assert_eq!(m(r"\Bfoo", "éfoo"), None);
        assert_eq!(m(r"\d+", "x٣42"), Some("42".into()));
        assert_eq!(m(r"\s+", "a\u{a0} \tb"), Some(" \t".into()));
        assert_eq!(m(r"\S+", "\u{a0}a b"), Some("\u{a0}a".into()));
        assert_eq!(m(r"[\w-]+", "é-a_b c"), Some("-a_b".into()));
        assert_eq!(m(r"[^\s]+", " ab "), Some("ab".into()));
        assert_eq!(m(r"[\W]", "aé"), Some("é".into()));
        assert_eq!(m(r"[\d\s]+", "a1 2b"), Some("1 2".into()));
        assert_eq!(m(r"[\D]", "1x"), Some("x".into()));
        // Texte littéral, dans et hors d'une classe.
        assert_eq!(m(r"\Qa.b\E", "axb a.b"), Some("a.b".into()));
        assert_eq!(m(r"\Q(x", "a(x"), Some("(x".into()));
        // Le Go refuse \Q dans une classe.
        assert_eq!(go_regex(r"[\Q]-\E]+"), None);
        // Dans une classe, « [ », « && », « -- » et « ~~ » sont littéraux.
        assert_eq!(m(r"[a[]+", "x[a["), Some("[a[".into()));
        assert_eq!(m(r"[a&&b]+", "x&ab"), Some("&ab".into()));
        // « !-- » : l'intervalle de « ! » à « - » pour le Go.
        assert_eq!(m(r"[!--]+", "a!#-b"), Some("!#-".into()));
        assert_eq!(m(r"[~~a]+", "x~a"), Some("~a".into()));
        assert_eq!(m(r"[]a]+", "x]a"), Some("]a".into()));
        assert_eq!(m(r"[[:alpha:]]+", "1abc"), Some("abc".into()));
        assert_eq!(m(r"[^[:alpha:]]+", "ab12"), Some("12".into()));
        // Le reste passe tel quel.
        assert_eq!(
            go_regex(r"\pL\.x{2}(?s:.)").as_deref(),
            Some(r"\pL\.x{2}(?s:.)")
        );
    }

    /// `(?mi)\Aoups` : motif de surcharge ancré en tête.
    struct Oups;

    impl RetryPattern for Oups {
        fn find(&self, s: &str) -> Option<(usize, usize)> {
            (s.len() >= 4 && s.as_bytes()[..4].eq_ignore_ascii_case(b"oups")).then_some((0, 4))
        }
    }

    /// TestTextualToolCallFromCustomFullScan
    #[test]
    fn textual_tool_call_from_custom_full_scan() {
        let s = "oups.\n".to_string() + &"Une phrase, puis une autre.\n".repeat(400);
        let custom = RetryPatterns::custom(vec![Box::new(Oups)]);
        assert!(custom.is_custom());
        assert!(
            custom.textual_tool_call_from(&s, s.len() - 10),
            "motif ancré en tête manqué"
        );
        let windowed = RetryPatterns {
            pats: vec![Box::new(Oups)],
            custom: false,
        };
        assert!(
            !windowed.textual_tool_call_from(&s, s.len() - 10),
            "la fenêtre aurait dû le manquer (le test ne prouve rien sinon)"
        );
    }

    /// Repli de casse de `(?i)` : « ſ » vaut s, « K » (Kelvin) vaut k, y
    /// compris dans `\w` ; `\b` reste ASCII.
    #[test]
    fn motifs_repli_de_casse_go() {
        let p = RetryPatterns::builtin();
        assert!(p.textual_tool_call("FUNCTIONS.Bash (x)"));
        assert!(p.textual_tool_call("functionſ.x("));
        assert!(p.textual_tool_call("{\"NAME\": \"as\u{212A}\"}"));
        assert!(p.textual_tool_call("<TOOL_CALL>"));
        assert!(p.textual_tool_call("default_api:\u{17F}"));
        assert!(!p.textual_tool_call("mydefault_api:bash"), "\\b");
        assert!(p.textual_tool_call("é default_api:bash"));
        assert!(!p.textual_tool_call("default_api:"), "\\w+ vide");
        assert!(!p.textual_tool_call("<function=>"));
        assert!(!p.textual_tool_call("functions.x ;("));
        // `^\s*` traverse les lignes vides ; l'extrait part du début de ligne.
        assert_eq!(
            p.snippet("a\n \n\t{\"name\":\n \"git_log\"}"),
            "{\"name\":\n \"git_log\"}"
        );
        assert!(!p.textual_tool_call("a {\"name\": \"bash\"}"));
        assert!(!p.textual_tool_call("{\"name\": \"mem_\"}"));
        // \v n'est pas un blanc pour Go.
        assert!(!p.textual_tool_call("\u{b}{\"name\": \"bash\"}"));
    }

    // ── llm_resume_test.go (partie analyse) ──

    /// TestAppelsParallelesSuiventLIndex
    #[test]
    fn appels_paralleles_suivent_l_index() {
        let body = concat!(
            r#"data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"glob","arguments":"{\"pattern\":\"*.go\"}"}}]}}]}"#,
            "\n\n",
            r#"data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","type":"function","function":{"name":"grep","arguments":"{\"pattern\":\"rien\"}"}}]}}]}"#,
            "\n\n",
            r#"data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}"#,
            "\n\ndata: [DONE]\n\n"
        );
        let (_, out) = replay(agent(), body);
        let a = out.assemble_tool_calls(0, &outils(&["glob", "grep"]));
        let got: Vec<(&str, &str, &str)> = a
            .calls
            .iter()
            .map(|c| {
                (
                    c.id.as_str(),
                    c.function.name.as_str(),
                    c.function.arguments.as_str(),
                )
            })
            .collect();
        assert_eq!(
            got,
            vec![
                ("a", "glob", r#"{"pattern":"*.go"}"#),
                ("b", "grep", r#"{"pattern":"rien"}"#)
            ]
        );
        assert!(
            a.calls
                .iter()
                .all(|c| c.kind == "function" && c.index.is_none())
        );
    }

    /// TestDebitMesureSansTimings (partie analyse) : sans `timings`, le débit
    /// de décodage est mesuré ici.
    #[test]
    fn debit_mesure_sans_timings() {
        let t0 = Instant::now();
        let at = |ms: u64| t0 + Duration::from_millis(ms);
        let mut evs = Vec::new();
        let mut emit = |ev: StreamEvent| {
            evs.push(ev);
            true
        };
        let mut p = StreamParser::new(StreamConfig {
            external: true,
            t_req: t0,
            ..Default::default()
        });
        for (ms, s) in [(10, "un "), (30, "deux "), (50, "trois")] {
            assert!(p.feed_line(sse_chunk(s).trim(), at(ms), &mut emit));
        }
        p.feed_line(SSE_STOP.trim(), at(52), &mut emit);
        p.feed_line(
            r#"data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}"#,
            at(55),
            &mut emit,
        );
        p.feed_line("data: [DONE]", at(56), &mut emit);
        let out = p.finish(&mut emit);
        let last = last_stats(&evs).expect("stats");
        assert_eq!(last.gen_tokens, 3);
        assert!(
            (last.gen_per_second - 50.0).abs() < 1e-9,
            "{}",
            last.gen_per_second
        );
        assert_eq!(last.gen_ms, 40.0);
        assert_eq!(last.prompt_ms, 10.0);
        assert_eq!(last.ttft_ms, Some(10));
        assert_eq!(last.prompt_tokens_total, 12);
        assert_eq!(out.stats, last);
        // Sans timings mais avec usage : complétion complète.
        assert!(out.is_complete(false));
    }

    // ── ctx_accounting_test.go (partie analyse) ──

    /// TestRaisonnementSepareCompte
    #[test]
    fn raisonnement_separe_compte() {
        let body = sse_reasoning("je ")
            + &sse_reasoning("réfléchis ")
            + &sse_reasoning("ici")
            + &sse_chunk("Bon")
            + &sse_chunk("jour")
            + &sse_final("stop", 100, 7)
            + SSE_DONE;
        let (evs, out) = replay(StreamConfig::default(), &body);
        assert_eq!(out.stats.reasoning_tokens, 3);
        assert_eq!(out.stats.ctx_after(), 104);
        assert_eq!(contents(&evs), "Bonjour");
        assert_eq!(reasonings(&evs).concat(), "je réfléchis ici");
        // Preset externe : son découpage ne dit rien du nombre de jetons.
        let (_, out) = replay(
            StreamConfig {
                external: true,
                ..Default::default()
            },
            &body,
        );
        assert_eq!(out.stats.reasoning_tokens, 0);
    }

    /// TestRaisonnementEnLigneNonCompte : <think> en ligne découpé chez nous,
    /// il reste dans le contenu et n'est pas retiré du compte.
    #[test]
    fn raisonnement_en_ligne_non_compte() {
        let body = sse_chunk("je pense")
            + &sse_chunk("</think>")
            + &sse_chunk("Réponse")
            + &sse_final("stop", 100, 6)
            + SSE_DONE;
        let cfg = StreamConfig {
            reasoning_on: true,
            ..Default::default()
        };
        let (evs, out) = replay(cfg, &body);
        // Comme en Go : la garde ne retient que 7 octets, le « j » part tout
        // de suite comme réponse, le reste devient le raisonnement.
        assert_eq!(reasonings(&evs), vec!["e pense".to_string()]);
        assert_eq!(contents(&evs), "jRéponse");
        assert_eq!(out.content, "je pense</think>Réponse");
        assert_eq!(out.stats.reasoning_tokens, 0);
        assert_eq!(out.stats.ctx_after(), 106);
    }

    // ── llm_reasoning_echo_test.go (partie analyse) ──

    /// TestEchoRenvoieLeRaisonnementSepare : le raisonnement séparé de l'étape
    /// d'outil est gardé (et compté dans le contexte suivant), celui de la
    /// réponse finale aussi.
    #[test]
    fn echo_renvoie_le_raisonnement_separe() {
        let cfg = || StreamConfig {
            echo_on: true,
            ..agent()
        };
        let step = sse_reasoning("je cherche ")
            + &sse_reasoning("les fichiers")
            + SSE_GLOB_STEP
            + SSE_DONE;
        let (_, out) = replay(cfg(), &step);
        assert_eq!(out.echo_text, "je cherche les fichiers");
        assert!(out.stats.echoed);
        assert_eq!(out.tool_calls.len(), 1);
        let fin = sse_reasoning("j'ai trouvé")
            + &sse_chunk("Voilà.")
            + &sse_final("stop", 120, 5)
            + SSE_DONE;
        let (_, out) = replay(cfg(), &fin);
        assert_eq!(out.echo_text, "j'ai trouvé");
        assert!(
            !out.stats.echoed,
            "pas d'appel et le gabarit ne garde pas le passé"
        );
        let (_, out) = replay(
            StreamConfig {
                echo_keeps_past: true,
                ..cfg()
            },
            &fin,
        );
        assert!(out.stats.echoed);
        // Clé absente : rien n'est capturé.
        let (_, out) = replay(agent(), &step);
        assert!(out.echo_text.is_empty() && !out.stats.echoed);
    }

    /// TestEchoJamaisDepuisLeDecoupageClient
    #[test]
    fn echo_jamais_depuis_le_decoupage_client() {
        let cfg = StreamConfig {
            echo_on: true,
            reasoning_on: true,
            ..agent()
        };
        let body = sse_chunk("je pense")
            + &sse_chunk("</think>")
            + &sse_chunk("je regarde")
            + SSE_GLOB_STEP;
        let (_, out) = replay(cfg, &body);
        assert_eq!(out.content, "je pense</think>je regarde");
        assert!(out.echo_text.is_empty());
        assert_eq!(out.tool_calls.len(), 1);
    }

    // ── code_preflight_test.go (partie analyse) ──

    fn write_chunk(args: &str) -> String {
        let esc = args.replace('"', "\\\"");
        format!(
            "data: {{\"choices\":[{{\"delta\":{{\"tool_calls\":[{{\"index\":0,\"id\":\"w1\",\"type\":\"function\",\"function\":{{\"name\":\"write\",\"arguments\":\"{esc}\"}}}}]}}}}]}}\n\n"
        )
    }

    /// TestPreVolCoupeLEcritureRefusee : refusé dès que le chemin est
    /// complet, le contenu n'arrive jamais.
    #[test]
    fn pre_vol_coupe_l_ecriture_refusee() {
        let body = write_chunk(r#"{"file":"main.go","#)
            + &write_chunk(r#""content":"package main // début"#)
            + &write_chunk(r#" FIN-JAMAIS-LUE"}"#)
            + r#"data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}"#
            + "\n\n";
        let calls = Arc::new(std::sync::Mutex::new(Vec::new()));
        let c2 = calls.clone();
        let cfg = StreamConfig {
            preflight: Some(Box::new(move |file: &str, is_write: bool| {
                c2.lock().unwrap().push((file.to_string(), is_write));
                if is_write && file == "main.go" {
                    "[refusé] main.go existe et n'a pas été lu".into()
                } else {
                    String::new()
                }
            })),
            ..agent()
        };
        let (evs, out) = replay(cfg, &body);
        assert!(
            tool_events(&evs)
                .iter()
                .all(|t| !t.body.contains("FIN-JAMAIS-LUE"))
        );
        assert_eq!(
            out.preflight,
            Some(PreflightRefusal {
                name: "write".into(),
                id: "w1".into(),
                file: "main.go".into(),
                msg: "[refusé] main.go existe et n'a pas été lu".into(),
            })
        );
        assert_eq!(*calls.lock().unwrap(), vec![("main.go".to_string(), true)]);
        assert!(out.read_error_benign());
        // Chemin permis : vérifié une seule fois, le flux va au bout.
        let n = Arc::new(std::sync::Mutex::new(0));
        let n2 = n.clone();
        let cfg = StreamConfig {
            preflight: Some(Box::new(move |_: &str, _: bool| {
                *n2.lock().unwrap() += 1;
                String::new()
            })),
            ..agent()
        };
        let (evs, out) = replay(cfg, &body);
        assert_eq!(*n.lock().unwrap(), 1);
        assert!(out.preflight.is_none() && out.finish_reason == "tool_calls");
        // Le corps n'est diffusé qu'à la ligne : la fin sans saut de ligne
        // n'est pas réémise, mais l'appel assemblé est entier.
        assert!(
            !tool_events(&evs)
                .iter()
                .any(|t| t.body.contains("FIN-JAMAIS-LUE"))
        );
        assert!(
            out.tool_calls[&0]
                .function
                .arguments
                .ends_with(r#"FIN-JAMAIS-LUE"}"#)
        );
    }

    // ── llm_injected_test.go (partie analyse) ──

    /// TestRelance500ReplieSurLeCheminHistorique : sous tool_choice « none »,
    /// un appel (texte ou protocole) est une fuite qui coupe le flux, jamais
    /// un outil annoncé.
    #[test]
    fn relance_tool_choice_none_fuites() {
        let none = || StreamConfig {
            tool_choice_none: true,
            ..Default::default()
        };
        let leak = r#"data: {"choices":[{"delta":{"content":"<tool_call>"}}]}"#.to_string()
            + "\n\n"
            + SSE_STOP;
        let (_, out) = replay(none(), &leak);
        assert!(out.none_leak && !out.pattern_hit);
        let (evs, out) = replay(none(), SSE_GLOB_STEP);
        assert!(out.none_leak && tool_events(&evs).is_empty() && out.tool_calls.is_empty());
        let (_, out) = replay(none(), SSE_STOP);
        assert!(!out.none_leak && out.content.is_empty());
        let (_, out) = replay(none(), &(sse_chunk("voici") + SSE_STOP));
        assert!(!out.none_leak && out.content == "voici");
        // Outils coupés (pas de tool_choice none) : l'appel est ignoré, pas une fuite.
        let (evs, out) = replay(StreamConfig::default(), SSE_GLOB_STEP);
        assert!(!out.none_leak && out.tool_calls.is_empty() && tool_events(&evs).is_empty());
    }

    // ── Comportements de la boucle Go, sans test Go dédié ──

    /// `json.Unmarshal` dans streamChunk : un champ mal typé saute le chunk
    /// entier ; null est neutre ; clés insensibles à la casse.
    #[test]
    fn decodage_strict_comme_go() {
        for bad in [
            r#"{"choices":[{"delta":{"content":5}}]}"#,
            r#"{"choices":[{"delta":{"content":"a"}},{"delta":{"content":5}}]}"#,
            r#"{"timings":{"prompt_n":12.5}}"#,
            r#"{"usage":{"prompt_tokens":"500"}}"#,
            r#"{"choices":{}}"#,
            r#"{"choices":[{"delta":[]}]}"#,
            r#"{"choices":[{"delta":{"tool_calls":[{"index":1.0}]}}]}"#,
            r#"{"a":1} x"#,
            "[]",
            "\"x\"",
            "{",
        ] {
            assert_eq!(decode_stream_chunk(bad), None, "{bad}");
        }
        assert_eq!(decode_stream_chunk("null"), Some(StreamChunk::default()));
        let c = decode_stream_chunk(
            r#"{"choices":[{"delta":{"content":null},"finish_reason":null}],"timings":null}"#,
        )
        .unwrap();
        assert_eq!(c.choices[0], ChunkChoice::default());
        assert!(c.timings.is_none());
        let c = decode_stream_chunk(r#"{"Choices":[{"DELTA":{"Content":"x"}}]}"#).unwrap();
        assert_eq!(c.choices[0].delta.content, "x");
        // Une erreur glissée dans le flux est ignorée, comme en Go.
        let body =
            sse_chunk("a") + "data: {\"error\":{\"message\":\"boom\"}}\n\n" + &sse_chunk("b");
        let (evs, _) = replay(StreamConfig::default(), &body);
        assert_eq!(contents(&evs), "ab");
        // Lignes hors protocole.
        let (evs, _) = replay(
            StreamConfig::default(),
            ": ping\n\nevent: x\ndata:\n  data: {\"choices\":[{\"delta\":{\"content\":\"z\"}}]}\r\n",
        );
        assert_eq!(contents(&evs), "z");
    }

    /// Sans index, la position dans le chunk ; finish_reason « stop » garde
    /// les appels ; id de secours ; arguments cassés neutralisés (`badArgs`).
    #[test]
    fn assemblage_des_appels() {
        let body = concat!(
            r#"data: {"choices":[{"delta":{"tool_calls":[{"function":{"name":"bash","arguments":"{\"command\":\"python3 "}},{"function":{"name":"glob","arguments":""}}]}}]}"#,
            "\n\n",
            r#"data: {"choices":[{"delta":{"tool_calls":[{"index":2,"function":{"name":"bash","arguments":"{\"cmd\":"}},{"index":2,"function":{"arguments":"\"ls\"}"}}]}}]}"#,
            "\n\n",
            r#"data: {"choices":[{"delta":{"tool_calls":[{"index":3,"function":{"name":"x","arguments":"  "}}]}}]}"#,
            "\n\n",
            r#"data: {"choices":[{"delta":{},"finish_reason":"stop"}]}"#,
            "\n\n"
        );
        let (_, out) = replay(agent(), body);
        assert_eq!(out.finish_reason, "stop");
        let a = out.assemble_tool_calls(4, &outils(&["bash", "glob"]));
        let got: Vec<(&str, &str, &str)> = a
            .calls
            .iter()
            .map(|c| {
                (
                    c.id.as_str(),
                    c.function.name.as_str(),
                    c.function.arguments.as_str(),
                )
            })
            .collect();
        assert_eq!(
            got,
            vec![
                ("call_4_0", "bash", "{}"),
                ("call_4_1", "glob", "{}"),
                ("call_4_2", "bash", r#"{"command":"ls"}"#),
                ("call_4_3", "x", "{}"),
            ]
        );
        assert_eq!(a.bad_args, HashSet::from(["call_4_0".to_string()]));
    }

    /// Le rappel qui refuse arrête tout : plus rien n'est lu ni émis, le
    /// reliquat « </think> » n'est pas vidé.
    #[test]
    fn arret_par_le_rappel() {
        let mut evs = Vec::new();
        let mut emit = |ev: StreamEvent| {
            let stop = matches!(&ev, StreamEvent::Content(c) if c == "b");
            evs.push(ev);
            !stop
        };
        let mut p = StreamParser::new(StreamConfig::default());
        let now = Instant::now();
        assert!(p.feed_line(sse_chunk("a").trim(), now, &mut emit));
        assert!(!p.feed_line(sse_chunk("b").trim(), now, &mut emit));
        assert!(p.is_stopped());
        assert!(!p.feed_line(sse_chunk("c").trim(), now, &mut emit));
        let out = p.finish(&mut emit);
        assert!(out.aborted);
        assert_eq!(contents(&evs), "ab");
        assert_eq!(out.content, "ab");

        let mut evs = Vec::new();
        let mut emit = |ev: StreamEvent| {
            evs.push(ev);
            false
        };
        let mut p = StreamParser::new(StreamConfig {
            reasoning_on: true,
            ..Default::default()
        });
        // 12 octets : 5 partent, 7 retenus ; l'arrêt empêche le vidage final
        // (les stats de secours, elles, partent comme en Go).
        assert!(!p.feed_line(sse_chunk("Je vais lire").trim(), now, &mut emit));
        let out = p.finish(&mut emit);
        assert!(out.aborted);
        assert_eq!(contents(&evs), "Je va");
        assert!(last_stats(&evs).is_some());
    }

    /// Le reliquat retenu par la garde « </think> » part AVANT l'outil ; la
    /// frappe montre l'argument parlant au fil du flux.
    #[test]
    fn reliquat_avant_l_outil_et_frappe() {
        let cfg = StreamConfig {
            reasoning_on: true,
            ..agent()
        };
        let call = |args: &str| {
            let piece = marshal(&Value::String(args.into()));
            format!(
                "data: {{\"choices\":[{{\"delta\":{{\"tool_calls\":[{{\"index\":0,\"id\":\"c\",\"function\":{{\"name\":\"bash\",\"arguments\":{piece}}}}}]}}}}]}}\n\n"
            )
        };
        let body =
            sse_chunk("Je vais lire") + &call(r#"{"command":"ls"#) + &call(r#" -la"}"#) + &call("");
        let (evs, out) = replay(cfg, &body);
        let order: Vec<String> = evs
            .iter()
            .filter_map(|e| match e {
                StreamEvent::Content(c) => Some(format!("C:{c}")),
                StreamEvent::ToolUsed(t) => Some(format!("T:{}:{}", t.name, t.label)),
                _ => None,
            })
            .collect();
        assert_eq!(
            order,
            ["C:Je va", "C:is lire", "T:bash:ls", "T:bash:ls -la"]
        );
        assert_eq!(out.typing_tool.map(|t| t.label), Some("ls -la".to_string()));
        assert!(out.sent_answer);
        // Fin de flux sans outil : le reliquat est vidé, sauts de ligne de tête ôtés.
        let (evs, _) = replay(
            StreamConfig {
                reasoning_on: true,
                ..Default::default()
            },
            &sse_chunk("\\n\\nabc"),
        );
        assert_eq!(contents(&evs), "abc");
    }

    /// Encodage `json.Marshal` des arguments réparés.
    #[test]
    fn marshal_comme_go() {
        let v = json!({"b": 1e21, "a": 1.5e-7, "c": "<&>\u{2028}\u{1}\u{8}\u{7f}é", "d": [true, null, 100, 0.000001, 1e20, -0.0], "e": 12345678901234567890u64});
        let want = format!(
            "{{\"a\":1.5e-7,\"b\":1e+21,\"c\":\"{}{}{}{}{}{}b{}é\",\"d\":[true,null,100,0.000001,100000000000000000000,-0],\"e\":12345678901234567000}}",
            esc("003c"),
            esc("0026"),
            esc("003e"),
            esc("2028"),
            esc("0001"),
            BACKSLASH,
            char::from(0x7f)
        );
        assert_eq!(marshal(&v), want);
        assert_eq!(go_fmt_float(123456.0), "123456");
        assert_eq!(go_fmt_float(0.0001), "0.0001");
        assert_eq!(go_fmt_float(0.00001), "1e-05");
        assert_eq!(go_fmt_float(1e100), "1e+100");
    }
}
