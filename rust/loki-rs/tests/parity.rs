//! Parité de la boucle d'un tour avec le Go (lot 6).
//!
//! `tests/parity/*.json` : journaux de référence produits par la boucle Go
//! (`internal/loki/parity_test.go`, régénérés par
//! `LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteJournaux`).
//! Chaque scénario est rejoué ici dans `chat::turn::run_turn`, sur un hôte qui
//! sert les mêmes réponses du moteur et les mêmes résultats d'outils. Doivent
//! se retrouver à l'identique : les corps envoyés au moteur (le cache de
//! préfixe en dépend), les événements d'affichage (texte regroupé, mesures
//! ignorées), les messages ajoutés à l'historique et l'erreur.

use std::collections::{HashMap, VecDeque};
use std::path::PathBuf;
use std::sync::{Arc, Mutex};

use bytes::Bytes;
use serde::Deserialize;
use serde_json::{Map, Value, json};

use loki_rs::chat::compact::CompactConfig;
use loki_rs::chat::stream::RetryPatterns;
use loki_rs::chat::turn::{
    Cancel, CompactPhase, Reply, SendError, ToolOutcome, TurnConfig, TurnHost, run_turn,
};
use loki_rs::chat::types::{Message, StreamEvent, Tool};
use loki_rs::chat::wire::{
    ChatEndpoint, EchoRefusals, EffortFallbacks, echo_policy_for, resolve_chat_endpoint,
    turn_reasoning,
};

#[derive(Deserialize)]
struct ReplyFx {
    status: u16,
    lines: Vec<String>,
}

#[derive(Deserialize)]
struct Expect {
    requests: Vec<Value>,
    events: Vec<Value>,
    extra: Value,
    err: String,
}

#[derive(Deserialize)]
struct Fixture {
    name: String,
    cfg: HashMap<String, String>,
    messages: Vec<Message>,
    tools: Vec<Tool>,
    temperature: f64,
    replies: Vec<ReplyFx>,
    tool_results: HashMap<String, String>,
    expect: Expect,
}

/// Hôte du rejeu : le moteur répond selon le script, les outils selon la
/// table du scénario — exactement ce que voit la boucle Go.
struct ParityHost {
    replies: Mutex<VecDeque<ReplyFx>>,
    tool_results: HashMap<String, String>,
    requests: Mutex<Vec<Value>>,
}

impl TurnHost for ParityHost {
    async fn send(
        &self,
        _ep: &ChatEndpoint,
        body: Vec<u8>,
        _keep: bool,
    ) -> Result<Reply, SendError> {
        self.requests
            .lock()
            .unwrap()
            .push(serde_json::from_slice(&body).expect("corps JSON"));
        let r = self
            .replies
            .lock()
            .unwrap()
            .pop_front()
            .expect("requête non scriptée");
        let chunks: Vec<Result<Bytes, String>> = r
            .lines
            .into_iter()
            .map(|l| Ok(Bytes::from(format!("{l}\n\n"))))
            .collect();
        Ok(Reply {
            status: r.status,
            body: Box::pin(futures_util::stream::iter(chunks)),
        })
    }

    async fn exec_tool(&self, name: &str, _args: &Map<String, Value>, _label: &str) -> ToolOutcome {
        ToolOutcome {
            result: self
                .tool_results
                .get(name)
                .cloned()
                .unwrap_or_else(|| format!("[erreur] outil inconnu : {name}")),
            ..Default::default()
        }
    }

    fn save_tool_result(&self, _result: &str) -> String {
        String::new()
    }

    async fn compact(&self, _phase: CompactPhase, _msgs: &[Message]) -> Option<Vec<Message>> {
        None
    }

    fn drain_queued(&self) -> Vec<Message> {
        Vec::new()
    }

    fn image_data_url(&self, _r: &str) -> Option<String> {
        None
    }

    fn effort_remember(&self, _want: &str, _got: &str) {}

    fn echo_refuse(&self, _model: &str) {}
}

/// Forme comparable d'un événement — `parityEvent` du Go (`None` : mesures).
fn event(ev: &StreamEvent) -> Option<Value> {
    Some(match ev {
        StreamEvent::Content(s) if !s.is_empty() => json!({ "content": s }),
        StreamEvent::Reasoning(s) if !s.is_empty() => json!({ "reasoning": s }),
        StreamEvent::Content(_) | StreamEvent::Reasoning(_) | StreamEvent::Stats(_) => {
            return None;
        }
        StreamEvent::ToolUsed(t) => {
            let mut m = Map::new();
            m.insert("name".into(), json!(t.name));
            let mut put = |k: &str, v: Value, keep: bool| {
                if keep {
                    m.insert(k.into(), v);
                }
            };
            put("label", json!(t.label), !t.label.is_empty());
            put("result", json!(t.result), !t.result.is_empty());
            put("done", json!(true), t.done);
            put("typing", json!(true), t.typing);
            put("body", json!(t.body), !t.body.is_empty());
            put("body_tail", json!(true), t.body_tail);
            put("body_lines", json!(t.body_lines), t.body_lines != 0);
            put("diff", json!(t.diff), !t.diff.is_empty());
            put("added", json!(t.added), t.added != 0);
            put("removed", json!(t.removed), t.removed != 0);
            put("image", json!(t.image), !t.image.is_empty());
            put("shot", json!(t.shot), !t.shot.is_empty());
            put("result_chars", json!(t.result_chars), t.result_chars != 0);
            put("result_id", json!(t.result_id), !t.result_id.is_empty());
            json!({ "tool_used": m })
        }
        StreamEvent::Ask(a) => json!({ "ask": a }),
        StreamEvent::Err(e) => json!({ "err": e }),
        StreamEvent::DropReasoning => json!({ "drop_reasoning": true }),
        StreamEvent::Compacting(b) => json!({ "compacting": b }),
        StreamEvent::NewHistory(h) => json!({ "new_history": h }),
        StreamEvent::Echo(e) => json!({ "echo": { "text": e.text, "model": e.model } }),
    })
}

/// Fragments de texte consécutifs regroupés — `parityMerge` du Go.
fn merge(evs: Vec<Value>) -> Vec<Value> {
    let mut out: Vec<Value> = Vec::new();
    for e in evs {
        if let Some(last) = out.last_mut() {
            let (lo, eo) = (last.as_object().unwrap(), e.as_object().unwrap());
            if lo.len() == 1 && eo.len() == 1 {
                let k = ["content", "reasoning"].into_iter().find(|k| {
                    lo.get(*k).is_some_and(Value::is_string)
                        && eo.get(*k).is_some_and(Value::is_string)
                });
                if let Some(k) = k {
                    let s = format!("{}{}", lo[k].as_str().unwrap(), eo[k].as_str().unwrap());
                    *last = json!({ k: s });
                    continue;
                }
            }
        }
        out.push(e);
    }
    out
}

async fn replay(fx: Fixture) -> Result<(), String> {
    let cfg = fx.cfg;
    let refusals = EchoRefusals::default();
    let echo = echo_policy_for(&cfg, &refusals, None);
    let tc = TurnConfig {
        ep: resolve_chat_endpoint(&cfg, ""),
        tools: fx.tools,
        temperature: fx.temperature,
        code: false,
        reasoning: turn_reasoning(&cfg, &EffortFallbacks::default()),
        compact: CompactConfig::from_config(&cfg, echo.clone()),
        echo,
        patterns: RetryPatterns::builtin(),
        cfg,
    };
    let host = ParityHost {
        replies: Mutex::new(fx.replies.into()),
        tool_results: fx.tool_results,
        requests: Mutex::new(Vec::new()),
    };
    let evs = Arc::new(Mutex::new(Vec::new()));
    let sink = evs.clone();
    let res = run_turn(&host, &tc, fx.messages, &Cancel::never(), &mut |ev| {
        if let Some(e) = event(&ev) {
            sink.lock().unwrap().push(e);
        }
        true
    })
    .await;

    let mut diffs = Vec::new();
    let requests = host.requests.into_inner().unwrap();
    if requests.len() != fx.expect.requests.len() {
        diffs.push(format!(
            "requêtes : {} envoyée(s), {} attendue(s)",
            requests.len(),
            fx.expect.requests.len()
        ));
    }
    for (i, (got, want)) in requests.iter().zip(&fx.expect.requests).enumerate() {
        if got != want {
            diffs.push(format!(
                "requête {} :\n  obtenu  {}\n  attendu {}",
                i + 1,
                got,
                want
            ));
        }
    }
    let events = merge(std::mem::take(&mut *evs.lock().unwrap()));
    if events != fx.expect.events {
        diffs.push(format!(
            "événements :\n  obtenu  {}\n  attendu {}",
            Value::Array(events),
            Value::Array(fx.expect.events)
        ));
    }
    let extra = serde_json::to_value(&res.extra).unwrap();
    if extra != fx.expect.extra {
        diffs.push(format!(
            "historique :\n  obtenu  {extra}\n  attendu {}",
            fx.expect.extra
        ));
    }
    let err = res.err.unwrap_or_default();
    if err != fx.expect.err {
        diffs.push(format!("erreur : {err:?}, attendu {:?}", fx.expect.err));
    }
    if diffs.is_empty() {
        Ok(())
    } else {
        Err(format!("{} :\n{}", fx.name, diffs.join("\n")))
    }
}

#[tokio::test]
async fn journaux_du_go() {
    let dir = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("tests/parity");
    let mut paths: Vec<PathBuf> = std::fs::read_dir(&dir)
        .expect("tests/parity")
        .map(|e| e.unwrap().path())
        .filter(|p| p.extension().is_some_and(|x| x == "json"))
        .collect();
    paths.sort();
    assert!(!paths.is_empty(), "aucun journal de référence");
    let mut failures = Vec::new();
    for p in &paths {
        let fx: Fixture = serde_json::from_slice(&std::fs::read(p).unwrap())
            .unwrap_or_else(|e| panic!("{} illisible : {e}", p.display()));
        if let Err(e) = replay(fx).await {
            failures.push(e);
        }
    }
    assert!(
        failures.is_empty(),
        "{} scénario(s) sur {} divergent du Go :\n\n{}",
        failures.len(),
        paths.len(),
        failures.join("\n\n")
    );
}
