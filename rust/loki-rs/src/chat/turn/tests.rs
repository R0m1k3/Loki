//! Tests de la boucle d'un tour, sur un faux hôte aux réponses scriptées.

use super::*;
use crate::chat::compact::CompactConfig;
use crate::chat::types::{StatsEvent, ToolFunction};
use crate::chat::wire::resolve_chat_endpoint;
use serde_json::json;
use std::collections::VecDeque;
use std::sync::Mutex;

/// Une réponse scriptée du moteur.
enum Script {
    Ok(Vec<String>),
    Status(u16, String),
    Net(NetKind),
    /// Corps qui s'interrompt sur une erreur de lecture après ces lignes.
    Cut(Vec<String>),
}

/// Une complétion signalée à la sonde : (outils, kwargs, effort).
type Completion = (usize, Option<Map<String, Value>>, String);

#[derive(Default)]
struct Fake {
    script: Mutex<VecDeque<Script>>,
    bodies: Mutex<Vec<Value>>,
    tools: Mutex<HashMap<String, String>>,
    execs: Mutex<Vec<String>>,
    compacted: Mutex<Option<Vec<Message>>>,
    phases: Mutex<Vec<CompactPhase>>,
    queued: Mutex<Vec<Message>>,
    efforts: Mutex<Vec<(String, String)>>,
    /// L'arrêt à déclencher pendant l'exécution d'un outil.
    stop_on_exec: Mutex<Option<watch::Sender<bool>>>,
    /// Verdict « préfixe instable » de la sonde (NUDGE_IN_TOOL).
    unstable: Mutex<bool>,
    /// Complétions signalées à la sonde : (outils, kwargs, effort).
    completions: Mutex<Vec<Completion>>,
    /// Télémétrie rangée ; et ce que le Go en tire.
    perfs: Mutex<Vec<PerfRec>>,
    marks: Mutex<PerfMarks>,
    /// Slot rendu par l'hôte (SIDE_SLOT) ; demandes reçues.
    slot: Mutex<Option<u8>>,
    slot_asks: Mutex<usize>,
}

impl Fake {
    fn new(script: Vec<Script>) -> Self {
        Fake {
            script: Mutex::new(script.into()),
            ..Default::default()
        }
    }
    fn body(&self, i: usize) -> Value {
        self.bodies.lock().unwrap()[i].clone()
    }
    fn sent(&self) -> usize {
        self.bodies.lock().unwrap().len()
    }
}

fn stream_of(lines: Vec<String>, cut: bool) -> BodyStream {
    let mut items: Vec<Result<Bytes, String>> = lines
        .into_iter()
        .map(|l| Ok(Bytes::from(format!("{l}\n"))))
        .collect();
    if cut {
        items.push(Err("connection reset".into()));
    }
    Box::pin(futures_util::stream::iter(items))
}

impl TurnHost for Fake {
    async fn send(&self, _ep: &ChatEndpoint, body: Vec<u8>) -> Result<Reply, SendError> {
        self.bodies
            .lock()
            .unwrap()
            .push(serde_json::from_slice(&body).unwrap());
        match self
            .script
            .lock()
            .unwrap()
            .pop_front()
            .expect("requête non scriptée")
        {
            Script::Ok(l) => Ok(Reply {
                status: 200,
                body: stream_of(l, false),
            }),
            Script::Cut(l) => Ok(Reply {
                status: 200,
                body: stream_of(l, true),
            }),
            Script::Status(s, b) => Ok(Reply {
                status: s,
                body: stream_of(vec![b], false),
            }),
            Script::Net(kind) => Err(SendError {
                kind,
                message: format!("panne {kind:?}"),
            }),
        }
    }
    async fn exec_tool(&self, name: &str, args: &Map<String, Value>, _label: &str) -> ToolOutcome {
        self.execs
            .lock()
            .unwrap()
            .push(format!("{name} {}", Value::Object(args.clone())));
        let stop = self.stop_on_exec.lock().unwrap().take();
        if let Some(tx) = stop {
            let _ = tx.send(true);
            std::future::pending::<()>().await;
        }
        let result = self
            .tools
            .lock()
            .unwrap()
            .get(name)
            .cloned()
            .unwrap_or_else(|| format!("résultat de {name}"));
        ToolOutcome {
            result,
            ..Default::default()
        }
    }
    fn save_tool_result(&self, _result: &str) -> String {
        "res1".into()
    }
    async fn compact(&self, phase: CompactPhase, _msgs: &[Message]) -> Option<Vec<Message>> {
        self.phases.lock().unwrap().push(phase);
        self.compacted.lock().unwrap().clone()
    }
    fn drain_queued(&self) -> Vec<Message> {
        std::mem::take(&mut *self.queued.lock().unwrap())
    }
    fn image_data_url(&self, _r: &str) -> Option<String> {
        None
    }
    fn effort_remember(&self, want: &str, got: &str) {
        self.efforts.lock().unwrap().push((want.into(), got.into()));
    }
    fn echo_refuse(&self, _model: &str) {}
    fn completed(&self, tools: &[Tool], kwargs: Option<&Map<String, Value>>, effort: &str) {
        self.completions
            .lock()
            .unwrap()
            .push((tools.len(), kwargs.cloned(), effort.into()));
    }
    fn prefix_unstable(&self, _t: &[Tool], _k: Option<&Map<String, Value>>, _e: &str) -> bool {
        *self.unstable.lock().unwrap()
    }
    async fn perf_record(&self, rec: PerfRec) -> PerfMarks {
        self.perfs.lock().unwrap().push(rec);
        self.marks.lock().unwrap().clone()
    }
    async fn engine_slot(&self, _ep: &ChatEndpoint) -> Option<u8> {
        *self.slot_asks.lock().unwrap() += 1;
        *self.slot.lock().unwrap()
    }
}

fn content(s: &str) -> String {
    format!("data: {}", json!({"choices":[{"delta":{"content": s}}]}))
}
fn finish(reason: &str) -> String {
    format!(
        "data: {}",
        json!({"choices":[{"delta":{},"finish_reason": reason}],"usage":{"prompt_tokens":100,"completion_tokens":5}})
    )
}
fn call(id: &str, name: &str, args: &str) -> String {
    format!(
        "data: {}",
        json!({"choices":[{"delta":{"tool_calls":[{"index":0,"id":id,"type":"function","function":{"name":name,"arguments":args}}]}}]})
    )
}
fn answer(s: &str) -> Script {
    Script::Ok(vec![content(s), finish("stop")])
}
fn tool_step(id: &str, name: &str, args: &str) -> Script {
    Script::Ok(vec![call(id, name, args), finish("tool_calls")])
}

fn tool(name: &str) -> Tool {
    Tool {
        kind: "function".into(),
        function: ToolFunction {
            name: name.into(),
            description: String::new(),
            parameters: json!({"type":"object"}),
        },
    }
}

fn config(tools: &[&str]) -> TurnConfig {
    let cfg = HashMap::new();
    TurnConfig {
        ep: resolve_chat_endpoint(&cfg, ""),
        cfg,
        tools: tools.iter().map(|n| tool(n)).collect(),
        temperature: 0.7,
        code: false,
        reasoning: TurnReasoning::default(),
        echo: EchoPolicy::default(),
        compact: CompactConfig::default(),
        patterns: RetryPatterns::builtin(),
    }
}

fn um(s: &str) -> Message {
    text_msg("user", s)
}

/// Lance un tour et rend (résultat, événements).
async fn run(host: &Fake, tc: &TurnConfig, cancel: &Cancel) -> (TurnResult, Vec<StreamEvent>) {
    let mut evs = Vec::new();
    let mut cb = |ev: StreamEvent| {
        evs.push(ev);
        true
    };
    let r = run_turn(host, tc, vec![um("question")], cancel, &mut cb).await;
    (r, evs)
}

fn text(evs: &[StreamEvent]) -> String {
    evs.iter()
        .filter_map(|e| match e {
            StreamEvent::Content(s) => Some(s.as_str()),
            _ => None,
        })
        .collect()
}

fn tools_done(evs: &[StreamEvent]) -> Vec<(String, String)> {
    evs.iter()
        .filter_map(|e| match e {
            StreamEvent::ToolUsed(t) if t.done => Some((t.name.clone(), t.result.clone())),
            _ => None,
        })
        .collect()
}

fn roles(m: &[Message]) -> Vec<&str> {
    m.iter().map(|m| m.role.as_str()).collect()
}

#[tokio::test]
async fn reponse_directe() {
    let host = Fake::new(vec![answer("Bonjour")]);
    let (r, evs) = run(&host, &config(&[]), &Cancel::never()).await;
    assert_eq!(r, TurnResult::default());
    assert_eq!(text(&evs), "Bonjour");
    assert!(evs.iter().any(|e| matches!(e, StreamEvent::Stats(_))));
    let b = host.body(0);
    assert_eq!(b["messages"][0]["content"], "question");
    assert_eq!(b["stream"], true);
}

#[tokio::test]
async fn outil_puis_reponse() {
    let host = Fake::new(vec![
        tool_step("c1", "glob", r#"{"pattern":"*.go"}"#),
        answer("Trouvé"),
    ]);
    let (r, evs) = run(&host, &config(&["glob"]), &Cancel::never()).await;
    assert_eq!(r.err, None);
    assert_eq!(roles(&r.extra), ["assistant", "tool"]);
    assert_eq!(r.extra[0].tool_calls[0].id, "c1");
    assert_eq!(r.extra[1].tool_call_id, "c1");
    assert_eq!(r.extra[1].content, Some(json!("résultat de glob")));
    assert_eq!(*host.execs.lock().unwrap(), [r#"glob {"pattern":"*.go"}"#]);
    assert_eq!(
        tools_done(&evs),
        [("glob".to_string(), "résultat de glob".to_string())]
    );
    assert_eq!(text(&evs), "Trouvé");
    // La seconde requête porte l'appel et son résultat.
    assert_eq!(roles_of(&host.body(1)), ["user", "assistant", "tool"]);
}

fn roles_of(b: &Value) -> Vec<String> {
    b["messages"]
        .as_array()
        .unwrap()
        .iter()
        .map(|m| m["role"].as_str().unwrap().to_string())
        .collect()
}

#[tokio::test]
async fn appel_identique_court_circuite_et_arguments_illisibles() {
    let host = Fake::new(vec![
        tool_step("a", "edit", r#"{"file":"x"}"#),
        tool_step("b", "edit", r#"{"file":"x"}"#),
        tool_step("c", "edit", r#"{"file":"#),
        answer("fini"),
    ]);
    let (r, evs) = run(&host, &config(&["edit"]), &Cancel::never()).await;
    assert_eq!(
        host.execs.lock().unwrap().len(),
        1,
        "rejoué ou exécuté sur JSON cassé"
    );
    let done = tools_done(&evs);
    assert!(done[1].1.starts_with("[déjà fait]"));
    assert!(
        done[2]
            .1
            .starts_with("[erreur] arguments de l'appel illisibles")
    );
    assert_eq!(r.extra.len(), 6);
    assert_eq!(r.extra[4].tool_calls[0].function.arguments, "{}");
}

#[tokio::test(start_paused = true)]
async fn reprise_reseau_puis_reponse() {
    let host = Fake::new(vec![
        Script::Net(NetKind::Refused),
        Script::Status(503, "occupé".into()),
        answer("ok"),
    ]);
    let (r, evs) = run(&host, &config(&[]), &Cancel::never()).await;
    assert_eq!(r.err, None);
    assert_eq!(text(&evs), "ok");
    assert_eq!(host.sent(), 3);
}

#[tokio::test(start_paused = true)]
async fn panne_reseau_durable_rend_l_erreur() {
    let host = Fake::new((0..4).map(|_| Script::Net(NetKind::Timeout)).collect());
    let (r, evs) = run(&host, &config(&[]), &Cancel::never()).await;
    assert_eq!(r.err.as_deref(), Some("panne Timeout"));
    assert!(matches!(evs.last(), Some(StreamEvent::Err(e)) if e == "panne Timeout"));
    assert_eq!(host.sent(), 1 + NET_RETRIES as usize);
}

#[tokio::test]
async fn refus_definitif_nomme_la_machine() {
    let host = Fake::new(vec![Script::Status(401, "clé refusée".into())]);
    let (r, _) = run(&host, &config(&[]), &Cancel::never()).await;
    assert_eq!(
        r.err.as_deref(),
        Some("llama-server a renvoyé 401 : clé refusée")
    );
    let host = Fake::new(vec![Script::Status(404, String::new())]);
    let mut tc = config(&[]);
    tc.ep.external = true;
    let (r, _) = run(&host, &tc, &Cancel::never()).await;
    assert_eq!(
        r.err.as_deref(),
        Some("l'API externe a renvoyé 404 : 404 Not Found")
    );
}

#[tokio::test]
async fn erreur_500_tool_choice_none_puis_outils_retires() {
    let host = Fake::new(vec![
        Script::Status(500, "parse error".into()),
        Script::Status(500, "encore".into()),
        answer("en texte"),
    ]);
    let (r, evs) = run(&host, &config(&["glob"]), &Cancel::never()).await;
    assert_eq!(r.err, None);
    assert_eq!(text(&evs), "en texte");
    assert_eq!(host.body(1)["tool_choice"], "none");
    assert!(
        host.body(1)["tools"].is_array(),
        "outils gardés : prompt intact"
    );
    assert!(host.body(2).get("tools").is_none(), "puis retirés");
    assert_eq!(host.body(2)["messages"][0]["role"], "system");
    // Une relance outils coupés n'est pas la forme normale : pas de sonde.
    assert!(host.completions.lock().unwrap().is_empty());
    // Pas davantage outils neutralisés (tool_choice « none »).
    let host = Fake::new(vec![
        Script::Status(500, "parse error".into()),
        answer("texte"),
    ]);
    let (r, _) = run(&host, &config(&["glob"]), &Cancel::never()).await;
    assert_eq!(r.err, None);
    assert_eq!(host.body(1)["tool_choice"], "none");
    assert!(host.completions.lock().unwrap().is_empty());
}

/// SIDE_SLOT : chaque requête porte le slot que rend l'hôte, demandé avant
/// chaque envoi ; sans slot, aucun `id_slot`.
#[tokio::test]
async fn slot_du_moteur_dans_chaque_requete() {
    let host = Fake::new(vec![
        tool_step("c1", "glob", r#"{"pattern":"*.go"}"#),
        answer("Trouvé"),
    ]);
    *host.slot.lock().unwrap() = Some(1);
    let (r, _) = run(&host, &config(&["glob"]), &Cancel::never()).await;
    assert_eq!(r.err, None);
    assert_eq!(
        (
            host.body(0)["id_slot"].clone(),
            host.body(1)["id_slot"].clone()
        ),
        (json!(1), json!(1))
    );
    assert_eq!(*host.slot_asks.lock().unwrap(), 2);
    let host = Fake::new(vec![answer("ok")]);
    let (r, _) = run(&host, &config(&[]), &Cancel::never()).await;
    assert_eq!(r.err, None);
    assert!(host.body(0).get("id_slot").is_none());
}

/// Cache KV plein en plein calcul avec deux slots : rejouée telle quelle,
/// deux fois au plus, sans compaction ; au-delà, l'erreur. Sans second slot,
/// c'est un débordement comme un autre.
#[tokio::test(start_paused = true)]
async fn cache_kv_plein_rejoue_sans_compaction() {
    let plein = r#"{"error":{"code":500,"message":"Context size has been exceeded.","type":"server_error"}}"#;
    let host = Fake::new(vec![
        Script::Status(500, plein.into()),
        Script::Status(500, plein.into()),
        answer("ok"),
    ]);
    *host.slot.lock().unwrap() = Some(0);
    *host.compacted.lock().unwrap() = Some(vec![um("résumé")]);
    let (r, evs) = run(&host, &config(&[]), &Cancel::never()).await;
    assert_eq!(r.err, None);
    assert_eq!(text(&evs), "ok");
    assert_eq!(host.sent(), 3);
    assert!(host.phases.lock().unwrap().is_empty(), "pas de compaction");
    assert_eq!(host.body(2)["messages"], host.body(0)["messages"]);
    // Trois fois de suite : l'erreur telle quelle, sans compaction.
    let host = Fake::new((0..3).map(|_| Script::Status(500, plein.into())).collect());
    *host.slot.lock().unwrap() = Some(0);
    *host.compacted.lock().unwrap() = Some(vec![um("résumé")]);
    let (r, _) = run(&host, &config(&[]), &Cancel::never()).await;
    // Le corps du refus tel quel, comme le Go.
    assert_eq!(r.err, Some(format!("llama-server a renvoyé 500 : {plein}")));
    assert_eq!(host.sent(), 3);
    assert!(host.phases.lock().unwrap().is_empty());
    // Sans second slot : compaction réactive.
    let host = Fake::new(vec![Script::Status(500, plein.into()), answer("ok")]);
    *host.compacted.lock().unwrap() = Some(vec![um("résumé")]);
    let (r, _) = run(&host, &config(&[]), &Cancel::never()).await;
    assert_eq!(r.err, None);
    assert_eq!(*host.phases.lock().unwrap(), [CompactPhase::Reactive]);
    // Moteur externe : jamais (le slot vient d'un moteur local).
    let host = Fake::new(vec![Script::Status(500, plein.into()), answer("ok")]);
    *host.slot.lock().unwrap() = Some(0);
    *host.compacted.lock().unwrap() = Some(vec![um("résumé")]);
    let mut tc = config(&[]);
    tc.ep.external = true;
    let (r, _) = run(&host, &tc, &Cancel::never()).await;
    assert_eq!(r.err, None);
    assert_eq!(*host.phases.lock().unwrap(), [CompactPhase::Reactive]);
}

#[test]
fn cache_kv_plein_ou_prompt_trop_long() {
    for (msg, want) in [
        (
            r#"{"error":{"code":500,"message":"Context size has been exceeded.","type":"server_error"}}"#,
            true,
        ),
        (
            r#"{"error":{"message":"request (40000 tokens) exceeds the available context size (32768 tokens), try increasing it","type":"exceed_context_size_error"}}"#,
            false,
        ),
        ("the request exceeds the available context size", false),
        ("CONTEXT SIZE HAS BEEN EXCEEDED, Request (12 TOKENS)", false),
        ("failed to parse tool call", false),
    ] {
        assert_eq!(side_slot_pool_error(msg), want, "{msg}");
    }
}

#[tokio::test]
async fn debordement_compaction_reactive() {
    let host = Fake::new(vec![
        Script::Status(400, "the request exceeds the available context size".into()),
        answer("après compaction"),
    ]);
    *host.compacted.lock().unwrap() = Some(vec![um("résumé")]);
    let (r, evs) = run(&host, &config(&[]), &Cancel::never()).await;
    assert_eq!(r.err, None);
    assert_eq!(*host.phases.lock().unwrap(), [CompactPhase::Reactive]);
    assert!(
        evs.iter()
            .any(|e| matches!(e, StreamEvent::NewHistory(h) if h == &vec![um("résumé")]))
    );
    assert_eq!(host.body(1)["messages"][0]["content"], "résumé");
}

#[tokio::test]
async fn intensite_refusee_par_le_gabarit() {
    let refus = "Jinja Exception: Unexpected reasoning effort high. Supported types are xhigh (default), medium, and low.";
    let host = Fake::new(vec![Script::Status(500, refus.into()), answer("ok")]);
    let mut tc = config(&[]);
    tc.reasoning = TurnReasoning {
        effort_wanted: "high".into(),
        effort: "high".into(),
        ..Default::default()
    };
    let (r, _) = run(&host, &tc, &Cancel::never()).await;
    assert_eq!(r.err, None);
    assert_eq!(host.body(0)["reasoning_effort"], "high");
    assert_eq!(host.body(1)["reasoning_effort"], "xhigh");
    assert_eq!(
        *host.efforts.lock().unwrap(),
        [("high".to_string(), "xhigh".to_string())]
    );
}

#[tokio::test]
async fn reponse_vide_relances_puis_message() {
    let empty = || Script::Ok(vec![finish("stop")]);
    let host = Fake::new(vec![empty(), empty(), empty()]);
    let (r, evs) = run(&host, &config(&["glob"]), &Cancel::never()).await;
    assert_eq!(r.err, None);
    let drops = evs
        .iter()
        .filter(|e| matches!(e, StreamEvent::DropReasoning))
        .count();
    assert_eq!(drops, 2);
    let last = host.body(2)["messages"]
        .as_array()
        .unwrap()
        .last()
        .unwrap()
        .clone();
    assert_eq!(last["content"], THINK_NUDGE_STUCK);
    assert_eq!(
        text(&evs),
        "_(le modèle n'a pas produit de réponse — finish: stop)_"
    );
    // Sans outils, pas de relance.
    let host = Fake::new(vec![empty()]);
    let (_, evs) = run(&host, &config(&[]), &Cancel::never()).await;
    assert_eq!(host.sent(), 1);
    assert!(text(&evs).contains("pas produit de réponse"));
}

#[tokio::test]
async fn appel_ecrit_en_texte_relance_corrective() {
    let host = Fake::new(vec![
        answer("<tool_call>{\"name\":\"glob\",\"arguments\":{}}</tool_call>"),
        answer("vraie réponse"),
    ]);
    let (r, _) = run(&host, &config(&["glob"]), &Cancel::never()).await;
    assert_eq!(roles(&r.extra), ["assistant", "user"]);
    assert!(
        r.extra[1]
            .content
            .as_ref()
            .unwrap()
            .as_str()
            .unwrap()
            .starts_with("Your last answer contained a TOOL CALL WRITTEN AS TEXT")
    );
    assert_eq!(host.sent(), 2);
}

#[tokio::test]
async fn file_d_attente_injectee_entre_deux_etapes() {
    let host = Fake::new(vec![tool_step("c1", "glob", "{}"), answer("ok")]);
    *host.queued.lock().unwrap() = vec![um("et aussi ça")];
    let (r, _) = run(&host, &config(&["glob"]), &Cancel::never()).await;
    assert_eq!(roles(&r.extra), ["assistant", "tool", "user"]);
    assert_eq!(
        roles_of(&host.body(1)),
        ["user", "assistant", "tool", "user"]
    );
}

#[tokio::test]
async fn budget_d_outils_rappel() {
    let mut script: Vec<Script> = (0..3)
        .map(|i| {
            tool_step(
                &format!("c{i}"),
                "bash",
                &format!("{{\"command\":\"ls {i}\"}}"),
            )
        })
        .collect();
    script.push(answer("fin"));
    let host = Fake::new(script);
    let mut tc = config(&["bash"]);
    tc.cfg.insert("AGENT_BUDGET".into(), "2".into());
    let (r, _) = run(&host, &tc, &Cancel::never()).await;
    // Après 2 appels, le rappel part en fin de fil.
    let last = host.body(2)["messages"]
        .as_array()
        .unwrap()
        .last()
        .unwrap()
        .clone();
    assert!(last["content"].as_str().unwrap().contains("2 tool calls"));
    assert!(r.extra.iter().any(|m| {
        m.content
            .as_ref()
            .and_then(Value::as_str)
            .is_some_and(|s| s.contains("2 tool calls"))
    }));
}

/// NUDGE_IN_TOOL : avec la clé et un gabarit « préfixe instable », le rappel
/// part au bout du dernier résultat d'outil (envoyé et gardé à l'identique),
/// sans message user à part ; sans verdict, ou sans la clé, à part.
#[tokio::test]
async fn rappel_de_budget_dans_le_resultat() {
    for (key, unstable, inside) in [
        ("on", true, true),
        ("on", false, false),
        ("", true, false),
        ("Oui", true, true),
        ("external", true, false),
    ] {
        let mut script: Vec<Script> = (0..3)
            .map(|i| {
                tool_step(
                    &format!("c{i}"),
                    "bash",
                    &format!("{{\"command\":\"ls {i}\"}}"),
                )
            })
            .collect();
        script.push(answer("fin"));
        let host = Fake::new(script);
        *host.unstable.lock().unwrap() = unstable;
        let mut tc = config(&["bash"]);
        tc.cfg.insert("AGENT_BUDGET".into(), "2".into());
        if key == "external" {
            // Preset externe : le rappel reste à part, quoi que dise la sonde.
            let ext = HashMap::from([
                ("EXTERNAL".to_string(), "1".to_string()),
                (
                    "EXTERNAL_URL".to_string(),
                    "http://127.0.0.1:1/v1".to_string(),
                ),
            ]);
            tc.ep = resolve_chat_endpoint(&ext, "");
            tc.cfg.insert("NUDGE_IN_TOOL".into(), "on".into());
        } else {
            tc.cfg.insert("NUDGE_IN_TOOL".into(), key.into());
        }
        let (r, _) = run(&host, &tc, &Cancel::never()).await;
        let msgs = host.body(2)["messages"].as_array().unwrap().clone();
        let last = msgs.last().unwrap();
        let ctx = format!("{key}/{unstable}");
        if inside {
            assert_eq!(last["role"], "tool", "{ctx}");
            let c = last["content"].as_str().unwrap();
            assert!(
                c.starts_with("résultat de bash\n\n") && c.contains("2 tool calls"),
                "{ctx} : {c}"
            );
            assert!(
                !msgs.iter().any(|m| m["role"] == "user"
                    && m["content"].as_str().unwrap_or("").contains("2 tool calls")),
                "{ctx}"
            );
            let kept = r.extra.iter().filter(|m| m.role == "tool").nth(1).unwrap();
            assert_eq!(
                kept.content.as_ref().and_then(Value::as_str),
                Some(c),
                "{ctx} : historique"
            );
        } else {
            assert_eq!(last["role"], "user", "{ctx}");
            assert!(
                last["content"].as_str().unwrap().contains("2 tool calls"),
                "{ctx}"
            );
        }
    }
}

/// Chaque complétion lue en entier est signalée à la sonde, avec la forme
/// de la requête ; pas sur un preset externe.
#[tokio::test]
async fn completions_signalees_a_la_sonde() {
    let host = Fake::new(vec![tool_step("c1", "bash", "{}"), answer("fin")]);
    let mut tc = config(&["bash"]);
    tc.reasoning.effort = "low".into();
    let (_, _) = run(&host, &tc, &Cancel::never()).await;
    assert_eq!(
        *host.completions.lock().unwrap(),
        vec![(1, None, "low".to_string()), (1, None, "low".to_string())]
    );
    let host = Fake::new(vec![answer("fin")]);
    let mut tc = config(&["bash"]);
    let mut cfg = HashMap::new();
    cfg.insert("EXTERNAL".to_string(), "1".to_string());
    cfg.insert(
        "EXTERNAL_URL".to_string(),
        "http://127.0.0.1:1/v1".to_string(),
    );
    tc.ep = resolve_chat_endpoint(&cfg, "");
    assert!(tc.ep.external);
    let (_, _) = run(&host, &tc, &Cancel::never()).await;
    assert!(host.completions.lock().unwrap().is_empty());
}

#[tokio::test]
async fn arret_pendant_un_outil() {
    let host = Fake::new(vec![tool_step("c1", "bash", r#"{"command":"sleep 100"}"#)]);
    let (tx, cancel) = Cancel::new();
    *host.stop_on_exec.lock().unwrap() = Some(tx);
    let (r, evs) = run(&host, &config(&["bash"]), &cancel).await;
    assert_eq!(r.err, None, "un arrêt n'est pas une erreur");
    assert_eq!(roles(&r.extra), ["assistant"]);
    assert!(!evs.iter().any(|e| matches!(e, StreamEvent::Err(_))));
}

#[tokio::test(start_paused = true)]
async fn flux_externe_coupe_repris_apres_le_texte_affiche() {
    let host = Fake::new(vec![Script::Cut(vec![content("Début ")]), answer("suite")]);
    let mut tc = config(&[]);
    tc.ep.external = true;
    let (r, evs) = run(&host, &tc, &Cancel::never()).await;
    assert_eq!(r.err, None);
    assert_eq!(text(&evs), "Début suite");
    let msgs = host.body(1)["messages"].as_array().unwrap().clone();
    assert_eq!(msgs[1]["content"], "Début ");
    assert_eq!(msgs[2]["content"], STREAM_CUT_CONTINUE);
    // Moteur local coupé : erreur, pas de reprise.
    let host = Fake::new(vec![Script::Cut(vec![content("x")])]);
    let (r, _) = run(&host, &config(&[]), &Cancel::never()).await;
    assert!(r.err.unwrap().contains("a été coupé en cours de route"));
}

/// Télémétrie : chaque complétion rangée, numérotée dans le tour, avec les
/// empreintes des messages envoyés ; la dernière statistique porte la
/// nature et la perte de cache vue par le Go.
#[tokio::test]
async fn telemetrie_des_completions() {
    let host = Fake::new(vec![
        tool_step("c1", "glob", r#"{"pattern":"*.go"}"#),
        answer("Trouvé"),
    ]);
    *host.marks.lock().unwrap() = PerfMarks {
        kind: "verify".into(),
        lost: Some(40),
        lost_after: "foreign".into(),
        lost_alert: true,
    };
    let (r, evs) = run(&host, &config(&["glob"]), &Cancel::never()).await;
    assert_eq!(r.err, None);
    let perfs = host.perfs.lock().unwrap().clone();
    assert_eq!(perfs.len(), 2);
    for (i, rec) in perfs.iter().enumerate() {
        assert_eq!(rec.iter, i as i64);
        assert!(rec.complete);
        assert_eq!(
            (rec.stats.prompt_tokens_total, rec.stats.gen_tokens),
            (100, 5)
        );
        assert!(!rec.external);
        assert!(!rec.prefix.is_empty());
    }
    // La seconde requête prolonge la première : mêmes empreintes en tête.
    let (a, b) = (&perfs[0].prefix, &perfs[1].prefix);
    assert_eq!(a.len(), 1);
    assert_eq!(b.len(), 3);
    assert_eq!(b[..1], a[..]);
    let last: Vec<&StatsEvent> = evs
        .iter()
        .filter_map(|e| match e {
            StreamEvent::Stats(s) if !s.kind.is_empty() => Some(s),
            _ => None,
        })
        .collect();
    assert_eq!(last.len(), 2);
    assert_eq!(
        (
            last[1].kind.as_str(),
            last[1].lost,
            last[1].lost_after.as_str(),
            last[1].lost_alert
        ),
        ("verify", Some(40), "foreign", true)
    );
}

/// Une complétion coupée avant son chunk final : rangée incomplète, sans
/// empreintes ; un stop pendant la lecture aussi.
#[tokio::test]
async fn telemetrie_incomplete() {
    let host = Fake::new(vec![Script::Cut(vec![content("Bon")])]);
    host.marks.lock().unwrap().kind = "main".into();
    let (_, evs) = run(&host, &config(&[]), &Cancel::never()).await;
    let perfs = host.perfs.lock().unwrap().clone();
    assert_eq!(perfs.len(), 1);
    assert!(!perfs[0].complete);
    assert!(perfs[0].prefix.is_empty());
    assert!(
        !evs.iter()
            .any(|e| matches!(e, StreamEvent::Stats(s) if !s.kind.is_empty())),
        "pas de statistique finale"
    );
}
