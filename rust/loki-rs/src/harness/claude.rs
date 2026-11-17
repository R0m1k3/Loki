//! Un tour du mode Code confié à Claude Code (`claude -p … --output-format
//! stream-json`).
//!
//! [`command`] compose la ligne de commande et l'environnement (modèle local
//! par le relais `/v1` de Loki, fenêtre du moteur, télémétrie coupée) ;
//! [`Translator`] lit le flux JSON ligne à ligne et en fait les mêmes
//! [`StreamEvent`] que la boucle de Loki, pour les cartes existantes de
//! l'UI ; [`run`] lance le processus, suit l'arrêt du tour et rend la session
//! à reprendre au tour suivant (`--resume`).
//!
//! Format relevé sur Claude Code 2.1.318 (fixtures `tests/harness/`) : un
//! message `assistant` par bloc (texte, réflexion ou `tool_use`), un message
//! `user` par `tool_result`, des `stream_event` (deltas de texte et de
//! réflexion) avec `--include-partial-messages`, un `result` final. Une
//! erreur sort en `result` avec `is_error: true` — `subtype` peut valoir
//! `success`. Les événements d'un sous-agent portent `parent_tool_use_id`.

use std::collections::{HashMap, VecDeque};
use std::path::PathBuf;
use std::process::Stdio;
use std::time::Duration;

use serde_json::{Map, Value};
use tokio::io::{AsyncBufReadExt, BufReader};
use tokio::process::Command;

use crate::chat::turn::Cancel;
use crate::chat::types::{StatsEvent, StreamEvent, ToolUsedEvent};

/// Texte d'erreur de Claude Code quand la session à reprendre n'existe pas.
pub const NO_CONVERSATION: &str = "No conversation found";
/// Résultat d'outil affiché au plus (caractères).
pub const RESULT_MAX: usize = 8000;
/// Délai laissé au SIGTERM avant le SIGKILL du groupe.
const KILL_GRACE: Duration = Duration::from_secs(3);

/// Ce qu'il faut pour lancer un tour.
#[derive(Debug, Clone, Default)]
pub struct ClaudeTurn {
    pub bin: PathBuf,
    /// `HOME` de Claude Code (`LOKI_HOME/harness/claude`).
    pub home: PathBuf,
    /// Dossier de travail des outils.
    pub cwd: PathBuf,
    /// Dossier mémoire de Loki : Edit/Write/NotebookEdit y sont refusés.
    pub memory_dir: PathBuf,
    pub prompt: String,
    /// Session à reprendre (`--resume`).
    pub session: Option<String>,
    /// `http://127.0.0.1:<port de l'interface>` (relais `/v1`).
    pub base_url: String,
    /// Clé `/v1` (jamais en argument).
    pub token: String,
    pub model: String,
    pub ctx_window: i64,
    pub auto_update: bool,
}

/// Arguments de `claude` (sans le binaire).
pub fn args(t: &ClaudeTurn) -> Vec<String> {
    let mut a: Vec<String> = [
        "-p",
        &t.prompt,
        "--output-format",
        "stream-json",
        "--verbose",
        "--include-partial-messages",
        "--permission-mode",
        "bypassPermissions",
        "--disallowedTools",
        "WebSearch",
    ]
    .iter()
    .map(|s| s.to_string())
    .collect();
    if !t.memory_dir.as_os_str().is_empty() {
        let m = t.memory_dir.display();
        for tool in ["Edit", "Write", "NotebookEdit"] {
            a.push(format!("{tool}({m}/**)"));
        }
    }
    if let Some(s) = t.session.as_deref().filter(|s| !s.is_empty()) {
        a.push("--resume".into());
        a.push(s.to_string());
    }
    a
}

/// Variables d'environnement de `claude` (l'environnement est vidé avant).
pub fn envs(t: &ClaudeTurn) -> Vec<(String, String)> {
    let mut e: Vec<(String, String)> = Vec::new();
    let mut put = |k: &str, v: String| e.push((k.to_string(), v));
    put(
        "PATH",
        std::env::var("PATH").unwrap_or_else(|_| "/usr/local/bin:/usr/bin:/bin".into()),
    );
    put(
        "LANG",
        std::env::var("LANG").unwrap_or_else(|_| "C.UTF-8".into()),
    );
    put("HOME", t.home.display().to_string());
    // Le conteneur tourne en root : sans IS_SANDBOX, Claude Code refuse
    // bypassPermissions (« cannot be used with root/sudo privileges »).
    put("IS_SANDBOX", "1".into());
    put("ANTHROPIC_BASE_URL", t.base_url.clone());
    put("ANTHROPIC_AUTH_TOKEN", t.token.clone());
    for k in [
        "ANTHROPIC_MODEL",
        "ANTHROPIC_DEFAULT_HAIKU_MODEL",
        "ANTHROPIC_DEFAULT_SONNET_MODEL",
        "ANTHROPIC_DEFAULT_OPUS_MODEL",
        "CLAUDE_CODE_SUBAGENT_MODEL",
    ] {
        put(k, t.model.clone());
    }
    put(
        "ANTHROPIC_CUSTOM_HEADERS",
        format!("{}: {}", super::HEADER, super::secret()),
    );
    put("CLAUDE_CODE_AUTO_COMPACT_WINDOW", t.ctx_window.to_string());
    put(
        "CLAUDE_CODE_MAX_OUTPUT_TOKENS",
        (t.ctx_window / 4).clamp(1, 32000).to_string(),
    );
    put("DISABLE_TELEMETRY", "1".into());
    put("DISABLE_ERROR_REPORTING", "1".into());
    put("CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS", "1".into());
    if !t.auto_update {
        put("DISABLE_AUTOUPDATER", "1".into());
    }
    e
}

/// La commande prête à lancer.
pub fn command(t: &ClaudeTurn) -> Command {
    let mut c = Command::new(&t.bin);
    c.args(args(t))
        .env_clear()
        .envs(envs(t))
        .current_dir(&t.cwd)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(true);
    #[cfg(unix)]
    c.process_group(0);
    c
}

/// Un outil demandé par Claude Code, en attente de sa carte.
#[derive(Debug, Clone)]
struct Pending {
    id: String,
    ev: ToolUsedEvent,
}

/// Traduction du flux `stream-json` en [`StreamEvent`].
#[derive(Debug, Default)]
pub struct Translator {
    pub session_id: String,
    /// Texte du dernier message de Claude Code (la réponse finale).
    pub final_text: String,
    /// La session à reprendre n'existe plus.
    pub resume_missing: bool,
    /// Un `result` est arrivé.
    pub saw_result: bool,
    queue: VecDeque<Pending>,
    results: HashMap<String, (String, bool)>,
}

fn s<'a>(v: &'a Value, k: &str) -> &'a str {
    v.get(k).and_then(Value::as_str).unwrap_or("")
}

fn i(v: &Value, k: &str) -> i64 {
    v.get(k).and_then(Value::as_i64).unwrap_or(0)
}

/// Texte d'un contenu : chaîne, ou parties `text` jointes.
pub fn content_text(v: &Value) -> String {
    match v {
        Value::String(s) => s.clone(),
        Value::Array(a) => a
            .iter()
            .filter(|p| s(p, "type") == "text")
            .map(|p| s(p, "text"))
            .collect::<Vec<_>>()
            .join("\n"),
        _ => String::new(),
    }
}

fn cap(t: &str) -> String {
    if t.chars().count() <= RESULT_MAX {
        return t.to_string();
    }
    let mut out: String = t.chars().take(RESULT_MAX).collect();
    out.push_str("\n[…tronqué]");
    out
}

/// La carte (annoncée) d'un `tool_use` de Claude Code.
pub fn tool_card(name: &str, input: &Map<String, Value>) -> ToolUsedEvent {
    let get = |k: &str| {
        input
            .get(k)
            .and_then(Value::as_str)
            .unwrap_or("")
            .to_string()
    };
    let mut ev = ToolUsedEvent {
        name: name.to_lowercase(),
        ..Default::default()
    };
    match name {
        "Bash" => {
            ev.name = "bash".into();
            ev.label = get("command");
        }
        "Edit" => {
            ev.name = "edit".into();
            ev.label = get("file_path");
            let (d, a, r) = crate::tools::diff::line_diff(&get("old_string"), &get("new_string"));
            (ev.diff, ev.added, ev.removed) = (d, a, r);
        }
        "MultiEdit" => {
            ev.name = "edit".into();
            ev.label = get("file_path");
            for e in input
                .get("edits")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
            {
                let (d, a, r) =
                    crate::tools::diff::line_diff(s(e, "old_string"), s(e, "new_string"));
                ev.diff.extend(d);
                ev.added += a;
                ev.removed += r;
            }
        }
        "Write" => {
            ev.name = "write".into();
            ev.label = get("file_path");
            let (d, a) = crate::tools::diff::added_diff(&get("content"));
            (ev.diff, ev.added) = (d, a);
        }
        "Read" => {
            ev.name = "read".into();
            ev.label = get("file_path");
        }
        "Grep" | "Glob" => ev.label = get("pattern"),
        "WebFetch" => {
            ev.name = "web_fetch".into();
            ev.label = get("url");
        }
        "Task" | "Agent" => {
            ev.name = "subagent".into();
            ev.label = get("description");
        }
        "TodoWrite" => {
            ev.name = "todo".into();
            let todos = input.get("todos").and_then(Value::as_array);
            let n = todos.map_or(0, Vec::len);
            ev.label = format!("{n} tâches");
            ev.body = todos
                .into_iter()
                .flatten()
                .map(|t| {
                    let done = s(t, "status") == "completed";
                    format!("{} {}", if done { "[x]" } else { "[ ]" }, s(t, "content"))
                })
                .collect::<Vec<_>>()
                .join("\n");
            ev.body_lines = n as i64;
        }
        _ => {
            ev.label = input
                .values()
                .find_map(Value::as_str)
                .unwrap_or("")
                .to_string();
        }
    }
    ev
}

impl Translator {
    pub fn new() -> Self {
        Self::default()
    }

    fn announce_head(&self, out: &mut Vec<StreamEvent>) {
        if let Some(p) = self.queue.front() {
            let mut ev = p.ev.clone();
            ev.done = false;
            out.push(StreamEvent::ToolUsed(ev));
        }
    }

    /// Ferme les outils de tête qui ont leur résultat, dans l'ordre.
    fn drain(&mut self, out: &mut Vec<StreamEvent>) {
        while let Some(p) = self.queue.front() {
            let Some((text, err)) = self.results.remove(&p.id) else {
                break;
            };
            let mut ev = p.ev.clone();
            ev.done = true;
            ev.result = if err {
                format!("[erreur] {}", cap(&text))
            } else {
                cap(&text)
            };
            out.push(StreamEvent::ToolUsed(ev));
            self.queue.pop_front();
            self.announce_head(out);
        }
    }

    /// Une ligne du flux → les événements à émettre.
    pub fn line(&mut self, l: &str) -> Vec<StreamEvent> {
        let mut out = Vec::new();
        let Ok(v) = serde_json::from_str::<Value>(l.trim()) else {
            return out;
        };
        if !v.get("parent_tool_use_id").is_none_or(Value::is_null) {
            return out; // sous-agent : seule sa carte Task compte
        }
        match s(&v, "type") {
            "system" if s(&v, "subtype") == "init" => {
                self.session_id = s(&v, "session_id").to_string();
            }
            "stream_event" => {
                let e = v.get("event").cloned().unwrap_or_default();
                match s(&e, "type") {
                    "message_start" => self.final_text.clear(),
                    "content_block_delta" => {
                        let d = e.get("delta").cloned().unwrap_or_default();
                        match s(&d, "type") {
                            "text_delta" => {
                                let t = s(&d, "text");
                                if !t.is_empty() {
                                    self.final_text.push_str(t);
                                    out.push(StreamEvent::Content(t.to_string()));
                                }
                            }
                            "thinking_delta" => {
                                let t = s(&d, "thinking");
                                if !t.is_empty() {
                                    out.push(StreamEvent::Reasoning(t.to_string()));
                                }
                            }
                            _ => {}
                        }
                    }
                    _ => {}
                }
            }
            "assistant" => {
                let blocks = v.pointer("/message/content").and_then(Value::as_array);
                for b in blocks.into_iter().flatten() {
                    if s(b, "type") != "tool_use" {
                        continue;
                    }
                    let input = b
                        .get("input")
                        .and_then(Value::as_object)
                        .cloned()
                        .unwrap_or_default();
                    let was_empty = self.queue.is_empty();
                    self.queue.push_back(Pending {
                        id: s(b, "id").to_string(),
                        ev: tool_card(s(b, "name"), &input),
                    });
                    if was_empty {
                        self.announce_head(&mut out);
                    }
                }
            }
            "user" => {
                let blocks = v.pointer("/message/content").and_then(Value::as_array);
                for b in blocks.into_iter().flatten() {
                    if s(b, "type") != "tool_result" {
                        continue;
                    }
                    let text = content_text(b.get("content").unwrap_or(&Value::Null));
                    let err = b.get("is_error").and_then(Value::as_bool).unwrap_or(false);
                    self.results
                        .insert(s(b, "tool_use_id").to_string(), (text, err));
                }
                self.drain(&mut out);
            }
            "result" => {
                self.saw_result = true;
                let err = v.get("is_error").and_then(Value::as_bool).unwrap_or(false);
                let mut text = s(&v, "result").to_string();
                if text.is_empty() {
                    text = v
                        .get("errors")
                        .and_then(Value::as_array)
                        .map(|a| {
                            a.iter()
                                .filter_map(Value::as_str)
                                .collect::<Vec<_>>()
                                .join(" ; ")
                        })
                        .unwrap_or_default();
                }
                if text.is_empty() {
                    text = s(&v, "terminal_reason").to_string();
                }
                if err && text.contains(NO_CONVERSATION) {
                    self.resume_missing = true;
                    return out;
                }
                let sid = s(&v, "session_id");
                if !sid.is_empty() {
                    self.session_id = sid.to_string();
                }
                if let Some(u) = v.get("usage") {
                    let prompt = i(u, "input_tokens")
                        + i(u, "cache_read_input_tokens")
                        + i(u, "cache_creation_input_tokens");
                    let gen_tokens = i(u, "output_tokens");
                    if prompt > 0 || gen_tokens > 0 {
                        out.push(StreamEvent::Stats(StatsEvent {
                            prompt_tokens: prompt,
                            gen_tokens,
                            ..Default::default()
                        }));
                    }
                }
                if err {
                    let msg = if text.is_empty() {
                        "erreur de Claude Code".to_string()
                    } else {
                        text
                    };
                    out.push(StreamEvent::Err(format!("Claude Code : {msg}")));
                }
            }
            _ => {}
        }
        out
    }

    /// Fin du flux : les outils restés sans résultat sont fermés.
    pub fn finish(&mut self) -> Vec<StreamEvent> {
        let mut out = Vec::new();
        while let Some(p) = self.queue.pop_front() {
            let mut ev = p.ev;
            ev.done = true;
            ev.result = "[interrompu]".into();
            out.push(StreamEvent::ToolUsed(ev));
        }
        out
    }
}

/// Fin d'un tour.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Outcome {
    pub session_id: String,
    pub final_text: String,
    pub resume_missing: bool,
    /// Erreur hors flux (lancement, sortie sans `result`) — déjà émise.
    pub err: Option<String>,
}

/// Le processus est-il fini (absent, ou zombie non récolté) ?
#[cfg(unix)]
fn gone(pid: i64) -> bool {
    match std::fs::read_to_string(format!("/proc/{pid}/stat")) {
        Err(_) => true,
        // « pid (comm) S … » : l'état suit la dernière parenthèse.
        Ok(st) => st
            .rsplit_once(')')
            .and_then(|(_, rest)| rest.split_whitespace().next())
            .is_some_and(|c| c == "Z" || c == "X"),
    }
}

/// SIGTERM au groupe, puis SIGKILL au groupe dans tous les cas (un enfant
/// qui ignore SIGTERM ne survit pas à la mort du chef).
#[cfg(unix)]
async fn kill_group(pid: u32) {
    let pid = pid as i64;
    let _ = tokio::task::spawn_blocking(move || {
        crate::tune::lock::kill_tree_within(pid, &|| gone(pid), KILL_GRACE);
        // SAFETY: kill(2) n'a pas d'effet mémoire.
        unsafe {
            libc::kill(-(pid as i32), libc::SIGKILL);
        }
    })
    .await;
}

#[cfg(not(unix))]
async fn kill_group(_pid: u32) {}

/// Lance le tour et suit son flux jusqu'au bout (ou jusqu'à l'arrêt).
pub async fn run(
    turn: &ClaudeTurn,
    cancel: &Cancel,
    cb: &mut (dyn FnMut(StreamEvent) -> bool + Send),
) -> Outcome {
    let shown: Vec<String> = args(turn)
        .iter()
        .enumerate()
        .map(|(n, a)| {
            if n == 1 {
                format!("<{} car.>", a.chars().count())
            } else {
                a.clone()
            }
        })
        .collect();
    eprintln!(
        "[harness] claude {} (cwd {}, fenêtre {})",
        shown.join(" "),
        turn.cwd.display(),
        turn.ctx_window
    );
    let mut child = match command(turn).spawn() {
        Ok(c) => c,
        Err(e) => {
            let msg = format!("Claude Code ne démarre pas : {e}");
            cb(StreamEvent::Err(msg.clone()));
            return Outcome {
                err: Some(msg),
                ..Default::default()
            };
        }
    };
    let pid = child.id();
    let stdout = child.stdout.take();
    let stderr = child.stderr.take();
    // stderr : les 20 dernières lignes, lues à part pour ne jamais bloquer.
    let err_task = tokio::spawn(async move {
        let mut tail: VecDeque<String> = VecDeque::new();
        if let Some(e) = stderr {
            let mut lines = BufReader::new(e).lines();
            while let Ok(Some(l)) = lines.next_line().await {
                if tail.len() == 20 {
                    tail.pop_front();
                }
                tail.push_back(l);
            }
        }
        tail.into_iter().collect::<Vec<_>>().join("\n")
    });

    let mut tr = Translator::new();
    let mut stopped = false;
    if let Some(o) = stdout {
        let mut lines = BufReader::new(o).lines();
        loop {
            tokio::select! {
                biased;
                _ = cancel.cancelled() => {
                    stopped = true;
                    if let Some(p) = pid {
                        kill_group(p).await;
                    }
                    break;
                }
                l = lines.next_line() => match l {
                    Ok(Some(l)) => {
                        for ev in tr.line(&l) {
                            cb(ev);
                        }
                    }
                    _ => break,
                },
            }
        }
    }
    for ev in tr.finish() {
        cb(ev);
    }
    let status = child.wait().await.ok();
    let err_tail = err_task.await.unwrap_or_default();
    if err_tail.contains(NO_CONVERSATION) {
        tr.resume_missing = true;
    }
    let mut out = Outcome {
        session_id: if tr.resume_missing {
            String::new()
        } else {
            tr.session_id.clone()
        },
        final_text: tr.final_text.clone(),
        resume_missing: tr.resume_missing,
        err: None,
    };
    if !stopped && !tr.saw_result && !tr.resume_missing {
        let code = status.map(|s| s.to_string()).unwrap_or_default();
        let msg = format!(
            "Claude Code s'est arrêté sans réponse ({code}) : {}",
            err_tail
                .lines()
                .rev()
                .take(3)
                .collect::<Vec<_>>()
                .into_iter()
                .rev()
                .collect::<Vec<_>>()
                .join(" ")
        );
        cb(StreamEvent::Err(msg.clone()));
        out.err = Some(msg);
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    const TOUR: &str = include_str!("../../tests/harness/tour_outils.jsonl");
    const REPRISE: &str = include_str!("../../tests/harness/reprise.jsonl");
    const ABSENTE: &str = include_str!("../../tests/harness/reprise_absente.jsonl");

    fn all(tr: &mut Translator, text: &str) -> Vec<StreamEvent> {
        let mut v: Vec<StreamEvent> = text.lines().flat_map(|l| tr.line(l)).collect();
        v.extend(tr.finish());
        v
    }

    fn tools(evs: &[StreamEvent]) -> Vec<(String, bool, String, String)> {
        evs.iter()
            .filter_map(|e| match e {
                StreamEvent::ToolUsed(t) => {
                    Some((t.name.clone(), t.done, t.label.clone(), t.result.clone()))
                }
                _ => None,
            })
            .collect()
    }

    fn turn() -> ClaudeTurn {
        ClaudeTurn {
            bin: "/x/claude".into(),
            home: "/data/harness/claude".into(),
            cwd: "/data/w".into(),
            memory_dir: "/data/memory".into(),
            prompt: "fais-le".into(),
            session: None,
            base_url: "http://127.0.0.1:8090".into(),
            token: "cle-secrete".into(),
            model: "qwen".into(),
            ctx_window: 65536,
            auto_update: true,
        }
    }

    #[test]
    fn arguments_et_environnement() {
        let mut t = turn();
        let a = args(&t);
        assert_eq!(a[0], "-p");
        assert_eq!(a[1], "fais-le");
        assert!(a.contains(&"stream-json".to_string()));
        assert!(a.contains(&"Edit(/data/memory/**)".to_string()));
        assert!(a.contains(&"Write(/data/memory/**)".to_string()));
        assert!(!a.contains(&"--resume".to_string()));
        assert!(
            !a.iter().any(|x| x.contains("cle-secrete")),
            "clé hors des arguments"
        );
        t.session = Some("s1".into());
        let a = args(&t);
        let p = a.iter().position(|x| x == "--resume").unwrap();
        assert_eq!(a[p + 1], "s1");

        let e: HashMap<String, String> = envs(&t).into_iter().collect();
        assert_eq!(e["ANTHROPIC_AUTH_TOKEN"], "cle-secrete");
        assert_eq!(e["ANTHROPIC_BASE_URL"], "http://127.0.0.1:8090");
        assert_eq!(e["IS_SANDBOX"], "1");
        for k in [
            "ANTHROPIC_MODEL",
            "ANTHROPIC_DEFAULT_HAIKU_MODEL",
            "ANTHROPIC_DEFAULT_SONNET_MODEL",
            "ANTHROPIC_DEFAULT_OPUS_MODEL",
            "CLAUDE_CODE_SUBAGENT_MODEL",
        ] {
            assert_eq!(e[k], "qwen", "{k}");
        }
        assert_eq!(e["CLAUDE_CODE_AUTO_COMPACT_WINDOW"], "65536");
        assert_eq!(e["CLAUDE_CODE_MAX_OUTPUT_TOKENS"], "16384");
        assert_eq!(
            e["ANTHROPIC_CUSTOM_HEADERS"],
            format!("x-loki-harness: {}", super::super::secret())
        );
        assert_eq!(e["DISABLE_TELEMETRY"], "1");
        assert!(!e.contains_key("DISABLE_AUTOUPDATER"));
        t.auto_update = false;
        t.ctx_window = 400_000;
        let e: HashMap<String, String> = envs(&t).into_iter().collect();
        assert_eq!(e["DISABLE_AUTOUPDATER"], "1");
        assert_eq!(e["CLAUDE_CODE_MAX_OUTPUT_TOKENS"], "32000");
        // Sans dossier mémoire : pas de règle vide.
        t.memory_dir = PathBuf::new();
        assert!(!args(&t).iter().any(|x| x.starts_with("Edit(")));
    }

    #[test]
    fn tour_reel_avec_outils() {
        let mut tr = Translator::new();
        let evs = all(&mut tr, TOUR);
        assert_eq!(tr.session_id, "f95fb9ca-1fba-495a-acc8-87400e60d4df");
        assert_eq!(tr.final_text, "fini");
        assert!(!tr.resume_missing);
        let t = tools(&evs);
        let names: Vec<(&str, bool)> = t.iter().map(|(n, d, _, _)| (n.as_str(), *d)).collect();
        // Bash annoncé puis fini ; Read et Bash en parallèle : Read annoncé,
        // fini, puis Bash annoncé, fini ; Edit annoncé, fini.
        assert_eq!(
            names,
            vec![
                ("bash", false),
                ("bash", true),
                ("read", false),
                ("read", true),
                ("bash", false),
                ("bash", true),
                ("edit", false),
                ("edit", true),
            ]
        );
        assert_eq!(t[0].2, "echo salut > a.txt");
        assert!(t[3].3.contains("salut"), "{:?}", t[3]);
        assert_eq!(t[5].3, "a.txt");
        let edit = evs
            .iter()
            .rev()
            .find_map(|e| match e {
                StreamEvent::ToolUsed(t) if t.name == "edit" => Some(t.clone()),
                _ => None,
            })
            .unwrap();
        assert_eq!((edit.added, edit.removed), (1, 1));
        // Texte en flux, réflexion, stats, aucune erreur.
        let content: String = evs
            .iter()
            .filter_map(|e| match e {
                StreamEvent::Content(c) => Some(c.as_str()),
                _ => None,
            })
            .collect();
        assert_eq!(content, "Je crée le fichier.fini");
        assert!(
            evs.iter()
                .any(|e| matches!(e, StreamEvent::Reasoning(r) if r.contains("modifié")))
        );
        assert!(evs.iter().any(|e| matches!(e, StreamEvent::Stats(s) if s.prompt_tokens > 0 && s.prompt_tokens_total == 0)));
        assert!(!evs.iter().any(|e| matches!(e, StreamEvent::Err(_))));
    }

    #[test]
    fn reprise_garde_la_session() {
        let mut tr = Translator::new();
        all(&mut tr, REPRISE);
        assert_eq!(tr.session_id, "f95fb9ca-1fba-495a-acc8-87400e60d4df");
    }

    #[test]
    fn reprise_absente() {
        let mut tr = Translator::new();
        let evs = all(&mut tr, ABSENTE);
        assert!(tr.resume_missing);
        assert!(evs.iter().all(|e| !matches!(e, StreamEvent::Err(_))));
    }

    #[test]
    fn erreur_avec_subtype_success() {
        let mut tr = Translator::new();
        let evs = tr.line(r#"{"type":"result","subtype":"success","is_error":true,"result":"Not logged in","session_id":"x"}"#);
        assert!(matches!(&evs[..], [StreamEvent::Err(e)] if e.contains("Not logged in")));
        let evs = tr.line(r#"{"type":"result","is_error":true,"terminal_reason":"api_error"}"#);
        assert!(matches!(&evs[..], [StreamEvent::Err(e)] if e.contains("api_error")));
    }

    #[test]
    fn resultats_dans_le_desordre_et_interruption() {
        let mut tr = Translator::new();
        let mut evs = tr.line(r#"{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"tool_use","id":"a","name":"Bash","input":{"command":"un"}}]}}"#);
        evs.extend(tr.line(r#"{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"tool_use","id":"b","name":"Bash","input":{"command":"deux"}}]}}"#));
        evs.extend(tr.line(r#"{"type":"assistant","message":{"content":[{"type":"tool_use","id":"c","name":"Write","input":{"file_path":"f","content":"x\ny\n"}}]}}"#));
        // b avant a : b attend.
        evs.extend(tr.line(r#"{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"b","content":"B"}]}}"#));
        assert_eq!(tools(&evs).len(), 1);
        evs.extend(tr.line(r#"{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"a","content":[{"type":"text","text":"A"}],"is_error":true}]}}"#));
        evs.extend(tr.finish());
        let t = tools(&evs);
        let got: Vec<(&str, bool, &str)> = t
            .iter()
            .map(|(_, d, l, r)| (l.as_str(), *d, r.as_str()))
            .collect();
        assert_eq!(
            got,
            vec![
                ("un", false, ""),
                ("un", true, "[erreur] A"),
                ("deux", false, ""),
                ("deux", true, "B"),
                ("f", false, ""),
                ("f", true, "[interrompu]"),
            ]
        );
    }

    #[test]
    fn sous_agent_et_lignes_illisibles_ignores() {
        let mut tr = Translator::new();
        assert!(tr.line("pas du json").is_empty());
        assert!(tr.line(r#"{"type":"inconnu"}"#).is_empty());
        let sub = r#"{"type":"stream_event","parent_tool_use_id":"t1","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"sous"}}}"#;
        assert!(tr.line(sub).is_empty());
        assert_eq!(tr.final_text, "");
    }

    #[test]
    fn final_text_dernier_message() {
        let mut tr = Translator::new();
        let d = |t: &str| {
            format!(
                r#"{{"type":"stream_event","event":{{"type":"content_block_delta","delta":{{"type":"text_delta","text":"{t}"}}}}}}"#
            )
        };
        tr.line(&d("avant"));
        tr.line(r#"{"type":"stream_event","event":{"type":"message_start"}}"#);
        tr.line(&d("après"));
        assert_eq!(tr.final_text, "après");
    }

    #[test]
    fn cartes_des_outils() {
        let m = |v: Value| v.as_object().unwrap().clone();
        let t = tool_card(
            "TodoWrite",
            &m(
                serde_json::json!({"todos":[{"content":"a","status":"completed"},{"content":"b","status":"pending"}]}),
            ),
        );
        assert_eq!(
            (t.name.as_str(), t.label.as_str(), t.body.as_str()),
            ("todo", "2 tâches", "[x] a\n[ ] b")
        );
        let t = tool_card(
            "Write",
            &m(serde_json::json!({"file_path":"f","content":"1\n2\n"})),
        );
        assert_eq!((t.name.as_str(), t.added, t.removed), ("write", 2, 0));
        let t = tool_card(
            "MultiEdit",
            &m(
                serde_json::json!({"file_path":"f","edits":[{"old_string":"a","new_string":"b"},{"old_string":"c","new_string":"d"}]}),
            ),
        );
        assert_eq!((t.name.as_str(), t.added, t.removed), ("edit", 2, 2));
        let t = tool_card("Grep", &m(serde_json::json!({"pattern":"fn x"})));
        assert_eq!((t.name.as_str(), t.label.as_str()), ("grep", "fn x"));
        let t = tool_card("Mystere", &m(serde_json::json!({"n":1,"q":"quoi"})));
        assert_eq!((t.name.as_str(), t.label.as_str()), ("mystere", "quoi"));
        assert_eq!(
            cap(&"é".repeat(RESULT_MAX + 5)).chars().count(),
            RESULT_MAX + "\n[…tronqué]".chars().count()
        );
    }
}
