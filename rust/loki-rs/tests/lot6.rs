//! Lot 6 : clients Rust des API internes du chat (chiffrement, outils).

mod common;

use common::fakego;
use loki_rs::store::{CryptError, Store};

async fn store() -> (
    Store,
    std::sync::Arc<fakego::FakeGo>,
    tokio::sync::oneshot::Sender<()>,
    tempfile::TempDir,
) {
    let t = tempfile::tempdir().unwrap();
    let (_f, go, stop) = fakego::start(t.path().into()).await;
    let backend = go.backend.lock().unwrap().unwrap();
    (Store::new(backend, fakego::JETON.into()), go, stop, t)
}

#[tokio::test]
async fn sceller_ouvrir_et_verrou() {
    let (st, go, _stop, _t) = store().await;
    let sealed = st.seal(b"bonjour".to_vec()).await.unwrap();
    assert_eq!(sealed, b"SCELLE:bonjour");
    assert_eq!(st.open(sealed).await.unwrap(), b"bonjour");
    *go.locked.lock().unwrap() = true;
    assert_eq!(st.seal(b"x".to_vec()).await, Err(CryptError::Locked));
    assert_eq!(st.open(b"x".to_vec()).await, Err(CryptError::Locked));
}

#[tokio::test]
async fn outil_execute_par_le_go() {
    let (st, _go, _stop, _t) = store().await;
    let v = st
        .tool_exec(
            "bash",
            &serde_json::json!({ "command": "ls" }),
            &serde_json::json!({ "Agent": true }),
        )
        .await
        .unwrap();
    assert_eq!(v["result"], "exec:bash");
    assert_eq!(v["label"], "ls");
    assert_eq!(v["caps_agent"], true);
}

// ---- persistance des discussions par l'API interne ----

use loki_rs::chat::persist::{
    BK_CHAT, BK_TOOL_RES, CK_INDEX, ConvSnap, ConvState, Persister, StoreSink, ToolResJob,
    decode_index, decode_state,
};
use std::sync::Arc;

fn snap(id: &str, seq: u64, title: &str, body: Vec<u8>) -> ConvSnap {
    ConvSnap {
        id: id.into(),
        seq,
        body,
        title: title.into(),
        turns: 1,
        project: "generale".into(),
    }
}

/// Valeur brute en base, sans le préfixe du faux scellement.
fn opened(go: &fakego::FakeGo, bucket: &str, key: &str) -> Option<Vec<u8>> {
    let raw = go.store.lock().unwrap().get(bucket)?.get(key)?.clone();
    assert!(raw.starts_with(b"SCELLE:"), "{bucket}/{key} écrit en clair");
    Some(raw[b"SCELLE:".len()..].to_vec())
}

#[tokio::test]
async fn discussion_scellee_contenu_et_index() {
    let (st, go, _stop, _t) = store().await;
    let p = Persister::new(StoreSink::new(st));
    let body = ConvState {
        seq: 3,
        ..Default::default()
    }
    .to_json();
    p.enqueue_tool_res(ToolResJob {
        key: "c1.ab".into(),
        plain: Arc::from("sortie complète"),
    });
    p.persist(snap("c1", p.next_seq(), "premier titre", body.clone()))
        .await;

    assert_eq!(
        opened(&go, BK_TOOL_RES, "c1.ab").unwrap(),
        "sortie complète".as_bytes()
    );
    assert!(p.tool_res_pending("c1.ab").is_none());
    let content = opened(&go, BK_CHAT, "conv:c1").unwrap();
    assert_eq!(content, body);
    assert_eq!(decode_state(&content).seq, 3);
    let idx = decode_index(&opened(&go, BK_CHAT, CK_INDEX).unwrap());
    assert_eq!(idx.len(), 1);
    assert_eq!(
        (idx[0].id.as_str(), idx[0].title.as_str(), idx[0].turns),
        ("c1", "premier titre", 1)
    );
    assert_eq!(idx[0].project, "generale");

    // Une seconde discussion s'ajoute à l'index sans effacer la première.
    p.persist(snap("c2", p.next_seq(), "autre", b"{}".to_vec()))
        .await;
    let idx = decode_index(&opened(&go, BK_CHAT, CK_INDEX).unwrap());
    let mut ids: Vec<&str> = idx.iter().map(|m| m.id.as_str()).collect();
    ids.sort();
    assert_eq!(ids, ["c1", "c2"]);
}

#[tokio::test]
async fn memoire_verrouillee_rien_en_clair() {
    let (st, go, _stop, _t) = store().await;
    let p = Persister::new(StoreSink::new(st));
    *go.locked.lock().unwrap() = true;
    p.enqueue_tool_res(ToolResJob {
        key: "c1.ab".into(),
        plain: Arc::from("secret"),
    });
    p.persist(snap("c1", p.next_seq(), "t", b"{}".to_vec()))
        .await;
    assert!(
        go.store.lock().unwrap().is_empty(),
        "écrit mémoire verrouillée"
    );
    // Le résultat reste servi depuis la mémoire ; l'instantané repart au
    // déverrouillage.
    assert_eq!(p.tool_res_pending("c1.ab").as_deref(), Some("secret"));
    *go.locked.lock().unwrap() = false;
    p.persist(snap("c1", p.next_seq(), "t", b"{}".to_vec()))
        .await;
    assert!(opened(&go, BK_CHAT, "conv:c1").is_some());
}

#[tokio::test]
async fn index_illisible_jamais_reecrit() {
    let (st, go, _stop, _t) = store().await;
    go.set(BK_CHAT, CK_INDEX, "SCELLE:pas du json");
    let p = Persister::new(StoreSink::new(st));
    p.persist(snap("c1", p.next_seq(), "t", b"{}".to_vec()))
        .await;
    assert_eq!(opened(&go, BK_CHAT, "conv:c1").unwrap(), b"{}");
    assert_eq!(go.get(BK_CHAT, CK_INDEX).unwrap(), "SCELLE:pas du json");
}

// ---- bail du moteur ----

use loki_rs::chat::lease::{EngineLease, LeaseError, LeaseSignal, engine_stop};
use std::time::Duration;

async fn until(mut f: impl FnMut() -> bool) {
    tokio::time::timeout(Duration::from_secs(5), async {
        while !f() {
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
    })
    .await
    .expect("condition jamais atteinte");
}

#[tokio::test]
async fn bail_pris_renouvele_rendu() {
    let (st, go, _stop, _t) = store().await;
    // TTL de 300 ms : renouvelé toutes les 100 ms.
    let lease = EngineLease::acquire(st.clone(), Duration::from_millis(300))
        .await
        .unwrap();
    assert_eq!(lease.id(), "bail0");
    assert_eq!(go.lease_log.lock().unwrap()[0], "acquire 300");
    // Un second bail est refusé tant que le premier tient.
    match EngineLease::acquire(st.clone(), Duration::from_secs(30)).await {
        Err(LeaseError::Busy { error, .. }) => assert_eq!(error, "génération en cours"),
        other => panic!("second bail : {:?}", other.map(|l| l.id().to_string())),
    }
    until(|| {
        go.lease_log
            .lock()
            .unwrap()
            .iter()
            .any(|l| l == "renew bail0")
    })
    .await;
    assert_eq!(lease.signal(), LeaseSignal::Held);
    lease.release().await;
    assert_eq!(*go.lease.lock().unwrap(), None);
    assert!(
        go.lease_log
            .lock()
            .unwrap()
            .contains(&"release bail0".to_string())
    );
}

#[tokio::test]
async fn bail_stop_puis_perdu() {
    let (st, go, _stop, _t) = store().await;
    let mut lease = EngineLease::acquire(st, Duration::from_millis(300))
        .await
        .unwrap();
    *go.lease_stop.lock().unwrap() = true;
    let got = tokio::time::timeout(Duration::from_secs(5), lease.interrupted()).await;
    assert_eq!(got.unwrap(), LeaseSignal::Stop);
    // Le Go rompt le bail (Reset) : perdu.
    *go.lease.lock().unwrap() = None;
    until(|| lease.signal() == LeaseSignal::Lost).await;
    assert_eq!(lease.interrupted().await, LeaseSignal::Lost);
}

#[tokio::test]
async fn bail_lache_sans_release_est_rendu() {
    let (st, go, _stop, _t) = store().await;
    let lease = EngineLease::acquire(st, Duration::from_secs(30))
        .await
        .unwrap();
    drop(lease);
    until(|| go.lease.lock().unwrap().is_none()).await;
}

#[tokio::test]
async fn moteur_tenu_par_une_tache_go() {
    let (st, go, _stop, _t) = store().await;
    *go.go_busy.lock().unwrap() = Some("la tâche planifiée « veille » occupe le modèle".into());
    match EngineLease::acquire(st.clone(), Duration::from_secs(30)).await {
        Err(LeaseError::Busy {
            error,
            task_id,
            task_name,
        }) => {
            assert!(error.contains("veille"));
            assert_eq!(task_id.as_deref(), Some("t1"));
            assert_eq!(task_name.as_deref(), Some("veille"));
        }
        other => panic!("bail accordé : {:?}", other.map(|l| l.id().to_string())),
    }
    engine_stop(&st).await.unwrap();
    let lease = EngineLease::acquire(st, Duration::from_secs(30))
        .await
        .unwrap();
    lease.release().await;
}

// ---- plusieurs discussions ----

use loki_rs::chat::conv::Conversation;
use loki_rs::chat::sessions::{SessionError, SessionHooks, Sessions};
use std::sync::Mutex as StdMutex;

#[derive(Default, Clone)]
struct Crochets(Arc<StdMutex<Vec<String>>>);

impl SessionHooks for Crochets {
    async fn dropped(&self, id: &str) {
        self.0.lock().unwrap().push(format!("dropped {id}"));
    }
    async fn switched(&self, id: &str) {
        self.0.lock().unwrap().push(format!("switched {id}"));
    }
    async fn activated(&self, id: &str) {
        self.0.lock().unwrap().push(format!("activated {id}"));
    }
}

async fn sessions() -> (
    Sessions<Crochets>,
    Crochets,
    Arc<fakego::FakeGo>,
    tokio::sync::oneshot::Sender<()>,
    tempfile::TempDir,
) {
    let (st, go, stop, t) = store().await;
    let h = Crochets::default();
    (
        Sessions::new(st, Conversation::new(), h.clone()),
        h,
        go,
        stop,
        t,
    )
}

fn dire(s: &Sessions<Crochets>, texte: &str) {
    let c = s.conv();
    c.append_delta(
        c.epoch(),
        serde_json::from_value(serde_json::json!({"user": texte})).unwrap(),
    );
    c.with_state(|st| {
        st.messages.push(loki_rs::chat::types::Message {
            role: "user".into(),
            content: Some(serde_json::json!(texte)),
            ..Default::default()
        })
    });
}

fn textes(s: &Sessions<Crochets>) -> Vec<String> {
    s.conv().with_state(|st| {
        st.messages
            .iter()
            .filter_map(|m| m.content.as_ref()?.as_str().map(str::to_string))
            .collect()
    })
}

#[tokio::test]
async fn demarrage_sur_base_neuve() {
    let (s, _h, go, _stop, _t) = sessions().await;
    s.load().await.unwrap();
    let id = s.conv().id();
    assert!(id.starts_with('c'));
    assert_eq!(go.get(BK_CHAT, "active").unwrap(), id);
    let (list, active) = s.list().await.unwrap();
    assert_eq!(active, id);
    assert_eq!(list.len(), 1);
    assert_eq!(list[0].project, "generale");
    // Rechargé : même discussion, rien de recréé.
    s.load().await.unwrap();
    assert_eq!(s.read_index().await.len(), 1);
}

#[tokio::test]
async fn demarrage_reprend_le_fil_unique() {
    let (s, _h, go, _stop, _t) = sessions().await;
    go.set(
        BK_CHAT,
        "conversation",
        r#"{"messages":[{"role":"user","content":"ancien fil"}],"log":null,"seq":0,"ctx_used":0}"#,
    );
    s.load().await.unwrap();
    assert_eq!(textes(&s), ["ancien fil"]);
    assert!(
        go.get(BK_CHAT, "conversation").is_some(),
        "clé d'origine gardée"
    );
}

#[tokio::test]
async fn demarrage_base_illisible_n_ecrit_rien() {
    let (s, _h, go, _stop, _t) = sessions().await;
    *go.broken.lock().unwrap() = true;
    assert!(s.load().await.is_err());
    *go.broken.lock().unwrap() = false;
    assert!(
        go.store.lock().unwrap().is_empty(),
        "écrit malgré la base illisible"
    );
    assert_eq!(s.conv().id(), "");
}

#[tokio::test]
async fn creer_basculer_renommer() {
    let (s, h, _go, _stop, _t) = sessions().await;
    s.load().await.unwrap();
    let a = s.conv().id();
    dire(&s, "seulement dans A");
    let b = s.new_conv().await.unwrap();
    assert_ne!(a, b);
    assert!(textes(&s).is_empty());
    dire(&s, "seulement dans B");
    s.switch(&a).await.unwrap();
    assert_eq!(textes(&s), ["seulement dans A"]);
    s.switch(&b).await.unwrap();
    assert_eq!(textes(&s), ["seulement dans B"]);
    let switched: Vec<String> =
        h.0.lock()
            .unwrap()
            .iter()
            .filter(|l| l.starts_with("switched"))
            .cloned()
            .collect();
    assert_eq!(switched, [format!("switched {a}"), format!("switched {b}")]);
    let log = h.0.lock().unwrap().clone();
    assert_eq!(
        log[log.len() - 2..],
        [format!("activated {b}"), format!("switched {b}")],
        "la discussion active suit chaque bascule"
    );
    // L'index porte les titres déduits.
    let idx = s.read_index().await;
    let titre = |id: &str| idx.iter().find(|m| m.id == id).unwrap().title.clone();
    assert_eq!(titre(&a), "seulement dans A");
    assert_eq!(s.switch("inconnue").await, Err(SessionError::NotFound));
    assert_eq!(s.switch("").await, Err(SessionError::Missing));
    s.rename(&a, "  Mon fil ").await.unwrap();
    assert_eq!(
        s.read_index()
            .await
            .iter()
            .find(|m| m.id == a)
            .unwrap()
            .title,
        "Mon fil"
    );
    assert_eq!(s.rename(&a, " ").await, Err(SessionError::Missing));
}

#[tokio::test]
async fn supprimer_l_ouverte_puis_la_derniere() {
    let (s, h, go, _stop, _t) = sessions().await;
    s.load().await.unwrap();
    let a = s.conv().id();
    dire(&s, "fil A");
    s.persist().await;
    let b = s.new_conv().await.unwrap();
    dire(&s, "fil B");
    s.persist_async().await; // une écriture en attente qui vise B
    s.delete(&b).await.unwrap();
    assert!(
        go.get(BK_CHAT, &format!("conv:{b}")).is_none(),
        "contenu de B resté"
    );
    assert!(!s.read_index().await.iter().any(|m| m.id == b));
    assert_eq!(s.conv().id(), a, "bascule sur la plus récente restante");
    assert_eq!(textes(&s), ["fil A"]);
    assert!(h.0.lock().unwrap().contains(&format!("dropped {b}")));
    // B ne renaît pas, même après une écriture.
    s.persist().await;
    assert!(go.get(BK_CHAT, &format!("conv:{b}")).is_none());

    s.delete(&a).await.unwrap();
    let c = s.conv().id();
    assert!(c != a && !c.is_empty(), "toujours une discussion active");
    let idx = s.read_index().await;
    assert_eq!(idx.len(), 1);
    assert_eq!(idx[0].id, c);
    assert_eq!(s.delete("inconnue").await, Err(SessionError::NotFound));
}

// ---- un tour complet, branché ----

use loki_rs::chat::compact::{CompactCaps, CompactConfig, CompactMemory};
use loki_rs::chat::engine::EngineHttp;
use loki_rs::chat::host::{ChatShared, EngineSummarizer, GoTurnHost, ImageCache, TurnCompact};
use loki_rs::chat::stream::RetryPatterns;
use loki_rs::chat::turn::{Cancel, TurnConfig, run_turn};
use loki_rs::chat::types::{Message, StreamEvent, Tool, ToolFunction};
use loki_rs::chat::wire::{ChatEndpoint, EchoRefusals, EffortFallbacks, TurnReasoning};

/// Faux moteur : rend les réponses SSE dans l'ordre, garde les requêtes.
async fn fake_engine(
    replies: Vec<String>,
) -> (String, Arc<StdMutex<Vec<(String, serde_json::Value)>>>) {
    fake_engine_gated(replies, None).await
}

type Seen = Arc<StdMutex<Vec<(String, serde_json::Value)>>>;

/// Comme [`fake_engine`] ; avec `gate`, chaque complétion attend un jeton.
/// `/health` répond toujours 200.
async fn fake_engine_gated(
    replies: Vec<String>,
    gate: Option<Arc<tokio::sync::Semaphore>>,
) -> (String, Seen) {
    use http_body_util::{BodyExt, Full};
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let seen: Seen = Arc::new(StdMutex::new(Vec::new()));
    let replies = Arc::new(StdMutex::new(std::collections::VecDeque::from(replies)));
    let s = seen.clone();
    tokio::spawn(async move {
        loop {
            let Ok((stream, _)) = listener.accept().await else {
                return;
            };
            let (s, replies, gate) = (s.clone(), replies.clone(), gate.clone());
            tokio::spawn(async move {
                let svc = hyper::service::service_fn(
                    move |req: hyper::Request<hyper::body::Incoming>| {
                        let (s, replies, gate) = (s.clone(), replies.clone(), gate.clone());
                        async move {
                            let ok = |b: String| {
                                Ok::<_, std::convert::Infallible>(hyper::Response::new(Full::new(
                                    bytes::Bytes::from(b),
                                )))
                            };
                            if req.uri().path() == "/health" {
                                return ok("ok".into());
                            }
                            let auth = req
                                .headers()
                                .get("authorization")
                                .map(|v| v.to_str().unwrap().to_string())
                                .unwrap_or_default();
                            let body = req.into_body().collect().await.unwrap().to_bytes();
                            s.lock()
                                .unwrap()
                                .push((auth, serde_json::from_slice(&body).unwrap()));
                            if let Some(g) = &gate {
                                g.acquire().await.unwrap().forget();
                            }
                            let out = replies.lock().unwrap().pop_front().unwrap_or_default();
                            ok(out)
                        }
                    },
                );
                let _ = hyper::server::conn::http1::Builder::new()
                    .serve_connection(hyper_util::rt::TokioIo::new(stream), svc)
                    .await;
            });
        }
    });
    (format!("http://{addr}/v1/chat/completions"), seen)
}

fn sse(chunks: &[serde_json::Value]) -> String {
    chunks
        .iter()
        .map(|c| format!("data: {c}\n\n"))
        .collect::<String>()
        + "data: [DONE]\n\n"
}

#[tokio::test]
async fn tour_complet_par_le_go() {
    let (st, go, _stop, t) = store().await;
    let long = "ligne\n".repeat(400); // > aperçu : rangé pour « voir plus »
    *go.tool_result.lock().unwrap() = Some(long.clone());
    let imgdir = t.path().join("chatimg");
    std::fs::create_dir_all(&imgdir).unwrap();
    std::fs::write(imgdir.join("p.png"), b"SCELLE:PNG").unwrap();
    let (url, seen) = fake_engine(vec![
        sse(&[
            serde_json::json!({"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}]}}]}),
            serde_json::json!({"choices":[{"delta":{},"finish_reason":"tool_calls"}]}),
        ]),
        sse(&[
            serde_json::json!({"choices":[{"delta":{"content":"Voilà."}}]}),
            serde_json::json!({"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":2}}),
        ]),
    ])
    .await;
    let ep = ChatEndpoint {
        url,
        model: "loki".into(),
        key: "cle-locale".into(),
        external: false,
    };
    let shared = Arc::new(ChatShared {
        http: EngineHttp::new(),
        store: st.clone(),
        persister: Persister::new(StoreSink::new(st.clone())),
        effort: EffortFallbacks::new(),
        echo_refusals: EchoRefusals::new(),
        compact_memory: CompactMemory::new(),
        images: ImageCache::new(imgdir),
        patterns: RetryPatterns::builtin(),
    });
    let cfg = std::collections::HashMap::new();
    let host = GoTurnHost {
        shared: shared.clone(),
        conv_id: "c7".into(),
        caps: serde_json::json!({"Agent": true}),
        cfg: cfg.clone(),
        port: 8080,
        summarizer: EngineSummarizer {
            http: shared.http.clone(),
            ep: ep.clone(),
            cfg: cfg.clone(),
            patterns: shared.patterns.clone(),
            port: 8080,
        },
        compact: TurnCompact {
            cfg: CompactConfig::default(),
            caps: CompactCaps::default(),
            criteria: String::new(),
            main_conv: "c7".into(),
            tools: vec![],
            effort: String::new(),
            kwargs: Default::default(),
        },
        queued: Box::new(Vec::new),
    };
    let bash = Tool {
        kind: "function".into(),
        function: ToolFunction {
            name: "bash".into(),
            description: String::new(),
            parameters: serde_json::json!({"type":"object"}),
        },
    };
    let tc = TurnConfig {
        cfg,
        ep,
        tools: vec![bash],
        temperature: 0.7,
        code: false,
        reasoning: TurnReasoning::default(),
        echo: Default::default(),
        compact: CompactConfig::default(),
        patterns: shared.patterns.clone(),
    };
    let question = Message {
        role: "user".into(),
        content: Some(serde_json::json!([
            {"type": "text", "text": "Regarde"},
            {"type": "image_url", "image_url": {"url": "loki-img:p.png"}}
        ])),
        ..Default::default()
    };
    let mut evs = Vec::new();
    let mut cb = |e: StreamEvent| {
        evs.push(e);
        true
    };
    let r = run_turn(&host, &tc, vec![question], &Cancel::never(), &mut cb).await;
    assert_eq!(r.err, None);
    assert_eq!(r.extra.len(), 2);

    let seen = seen.lock().unwrap().clone();
    assert_eq!(seen.len(), 2);
    assert_eq!(seen[0].0, "Bearer cle-locale");
    // Image jointe : lue sur disque, ouverte par le Go, envoyée en data-URL.
    assert_eq!(
        seen[0].1["messages"][0]["content"][1]["image_url"]["url"],
        "data:image/png;base64,UE5H"
    );
    // Le modèle reçoit le résultat (plafonné), l'UI un aperçu et un id.
    assert_eq!(seen[1].1["messages"][2]["role"], "tool");
    let done = evs
        .iter()
        .find_map(|e| match e {
            StreamEvent::ToolUsed(t) if t.done => Some(t.clone()),
            _ => None,
        })
        .unwrap();
    assert_eq!(done.result_chars, long.chars().count() as i64);
    assert!(done.result.len() < long.len());
    assert!(done.result_id.starts_with("c7."));
    shared.persister.flush().await;
    assert_eq!(
        opened(&go, BK_TOOL_RES, &done.result_id).unwrap(),
        long.as_bytes()
    );
    let answer: String = evs
        .iter()
        .filter_map(|e| match e {
            StreamEvent::Content(s) => Some(s.as_str()),
            _ => None,
        })
        .collect();
    assert_eq!(answer, "Voilà.");
}

#[tokio::test]
async fn crochets_go_des_discussions() {
    let (st, go, _stop, _t) = store().await;
    let s = Sessions::new(
        st.clone(),
        Conversation::new(),
        loki_rs::chat::sessions::GoSessionHooks(st),
    );
    s.load().await.unwrap();
    let a = s.conv().id();
    let b = s.new_conv().await.unwrap();
    s.delete(&b).await.unwrap();
    let log = go.conv_log.lock().unwrap().clone();
    assert_eq!(
        log,
        [
            format!("active {a}"),
            format!("active {b}"),
            format!("dropped {b}"),
            format!("active {a}")
        ]
    );
}

// ---- un tour de bout en bout, par le service ----

use loki_rs::chat::generate::{ChatService, ERR_MODEL_LOADING, Refused, Sent};
use loki_rs::chat::sessions::GoSessionHooks;

fn port_of(url: &str) -> String {
    url.trim_start_matches("http://127.0.0.1:")
        .split('/')
        .next()
        .unwrap()
        .to_string()
}

async fn service(
    engine_url: Option<&str>,
) -> (
    Arc<ChatService>,
    Arc<fakego::FakeGo>,
    tokio::sync::oneshot::Sender<()>,
    tempfile::TempDir,
) {
    let (st, go, stop, t) = store().await;
    go.set(
        "config",
        "PORT",
        &engine_url.map(port_of).unwrap_or_else(|| "1".into()),
    );
    go.set("state", "api_key", "cle-du-moteur");
    let sessions = Sessions::new(st.clone(), Conversation::new(), GoSessionHooks(st.clone()));
    sessions.load().await.unwrap();
    let shared = ChatShared::for_sessions(&sessions, st, t.path().join("chatimg"));
    (
        ChatService::new(Arc::new(sessions), Arc::new(shared)),
        go,
        stop,
        t,
    )
}

async fn until_done(svc: &ChatService, turns: usize) {
    tokio::time::timeout(std::time::Duration::from_secs(10), async {
        loop {
            let c = svc.sessions.conv();
            let done = c.with_state(|st| {
                st.log
                    .iter()
                    .filter(|e| e.delta.contains_key("turn_done"))
                    .count()
            });
            if done >= turns && !c.is_generating() {
                return;
            }
            tokio::time::sleep(std::time::Duration::from_millis(10)).await;
        }
    })
    .await
    .expect("tour jamais terminé");
}

fn deltas(svc: &ChatService, key: &str) -> Vec<serde_json::Value> {
    svc.sessions.conv().with_state(|st| {
        st.log
            .iter()
            .filter_map(|e| e.delta.get(key).cloned())
            .collect()
    })
}

fn tool_then_answer(answer: &str) -> Vec<String> {
    vec![
        sse(&[
            serde_json::json!({"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}]}}]}),
            serde_json::json!({"choices":[{"delta":{},"finish_reason":"tool_calls"}]}),
        ]),
        sse(&[
            serde_json::json!({"choices":[{"delta":{"content": answer}}]}),
            serde_json::json!({"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":80,"completion_tokens":3}}),
        ]),
    ]
}

#[tokio::test]
async fn tour_de_bout_en_bout() {
    let (url, seen) = fake_engine(tool_then_answer("fini")).await;
    let (svc, go, _stop, _t) = service(Some(&url)).await;
    let sent = svc
        .send(serde_json::json!({"message": "liste", "cid": "a1"}))
        .await
        .unwrap();
    assert_eq!(sent, Sent::default());
    until_done(&svc, 1).await;
    // La fin est signalée en tout dernier (après l'enregistrement) : on l'attend.
    until(|| go.turn_log.lock().unwrap().contains(&"done".to_string())).await;

    // Vue modèle rangée : la demande, l'appel, son résultat, la réponse.
    let msgs = svc.sessions.conv().with_state(|st| st.messages.clone());
    let roles: Vec<&str> = msgs.iter().map(|m| m.role.as_str()).collect();
    assert_eq!(roles, ["user", "assistant", "tool", "assistant"]);
    assert_eq!(msgs[3].content, Some(serde_json::json!("fini")));
    // Journal : bulle avec le modèle, outil, réponse, fin.
    assert_eq!(deltas(&svc, "user"), [serde_json::json!("liste")]);
    assert_eq!(deltas(&svc, "model"), [serde_json::json!("m1")]);
    assert!(
        deltas(&svc, "tool_used")
            .iter()
            .any(|t| t["done"] == true && t["result"] == "exec:bash")
    );
    assert_eq!(deltas(&svc, "content"), [serde_json::json!("fini")]);
    assert!(!deltas(&svc, "stats").is_empty());
    // Requête : préambule fusionné au prompt du preset, clé du moteur, outils.
    let seen = seen.lock().unwrap().clone();
    assert_eq!(seen[0].0, "Bearer cle-du-moteur");
    assert_eq!(
        seen[0].1["messages"][0]["content"],
        "PRÉAMBULE\n\nTu es Loki."
    );
    assert_eq!(seen[0].1["messages"][1]["content"], "liste");
    assert_eq!(seen[0].1["tools"][0]["function"]["name"], "bash");
    // Bail rendu, fin signalée, discussion enregistrée.
    assert_eq!(*go.lease.lock().unwrap(), None);
    let id = svc.sessions.conv().id();
    let body = opened(&go, BK_CHAT, &format!("conv:{id}")).unwrap();
    assert_eq!(decode_state(&body).messages.len(), 4);
    // Réessai du même envoi : reconnu.
    let again = svc
        .send(serde_json::json!({"message": "liste", "cid": "a1"}))
        .await
        .unwrap();
    assert!(again.dup);
}

#[tokio::test]
async fn message_en_file_injecte_dans_le_tour() {
    let gate = Arc::new(tokio::sync::Semaphore::new(0));
    let (url, seen) = fake_engine_gated(tool_then_answer("les deux"), Some(gate.clone())).await;
    let (svc, _go, _stop, _t) = service(Some(&url)).await;
    svc.send(serde_json::json!({"message": "premier", "cid": "p1"}))
        .await
        .unwrap();
    until(|| seen.lock().unwrap().len() == 1).await;
    let q = svc
        .send(serde_json::json!({"message": "deuxième", "cid": "p2"}))
        .await
        .unwrap();
    assert!(q.queued, "un tour tourne : en file");
    gate.add_permits(10);
    until_done(&svc, 1).await;
    assert_eq!(
        deltas(&svc, "user"),
        [serde_json::json!("premier"), serde_json::json!("deuxième")]
    );
    let second = seen.lock().unwrap()[1].1.clone();
    let last = second["messages"]
        .as_array()
        .unwrap()
        .last()
        .unwrap()
        .clone();
    assert_eq!(last["content"], "deuxième", "injecté entre deux étapes");
    let users = svc
        .sessions
        .conv()
        .with_state(|st| st.messages.iter().filter(|m| m.role == "user").count());
    assert_eq!(users, 2);
}

#[tokio::test]
async fn moteur_tenu_par_une_tache_planifiee() {
    let (url, _seen) = fake_engine(vec![]).await;
    let (svc, go, _stop, _t) = service(Some(&url)).await;
    *go.go_busy.lock().unwrap() = Some("la tâche planifiée « veille » occupe le modèle".into());
    let body = serde_json::json!({"message": "salut", "cid": "b1"});
    let want = Refused {
        status: 409,
        error: "la tâche planifiée « veille » occupe le modèle".into(),
    };
    assert_eq!(svc.send(body.clone()).await, Err(want.clone()));
    // Refusé : le même envoi peut être renvoyé (pas pris pour un doublon).
    assert_eq!(svc.send(body).await, Err(want));
    assert!(!svc.sessions.conv().is_generating());
    assert!(deltas(&svc, "user").is_empty());
    // Le stop du chat arrête le détenteur Go.
    svc.stop().await;
    assert!(go.lease_log.lock().unwrap().contains(&"stop".to_string()));
}

#[tokio::test]
async fn moteur_pas_pret_et_message_vide() {
    let (svc, _go, _stop, _t) = service(None).await;
    let r = svc
        .send(serde_json::json!({"message": "salut", "cid": "c1"}))
        .await;
    assert_eq!(
        r,
        Err(Refused {
            status: 503,
            error: ERR_MODEL_LOADING.into()
        })
    );
    let r = svc
        .send(serde_json::json!({"message": " ", "cid": "c2"}))
        .await;
    assert_eq!(r.unwrap_err().status, 400);
}

// ---- les routes du chat servies par la façade (LOKI_CHAT=rust) ----

async fn call(
    facade: std::net::SocketAddr,
    method: &str,
    path: &str,
    body: &str,
) -> (u16, serde_json::Value) {
    use http_body_util::{BodyExt, Full};
    let client = hyper_util::client::legacy::Client::builder(hyper_util::rt::TokioExecutor::new())
        .build_http::<Full<bytes::Bytes>>();
    let req = hyper::Request::builder()
        .method(method)
        .uri(format!("http://{facade}{path}"))
        .body(Full::new(bytes::Bytes::from(body.to_string())))
        .unwrap();
    let resp = client.request(req).await.unwrap();
    let status = resp.status().as_u16();
    let b = resp.into_body().collect().await.unwrap().to_bytes();
    (
        status,
        serde_json::from_slice(&b).unwrap_or(serde_json::Value::String(
            String::from_utf8_lossy(&b).into(),
        )),
    )
}

/// POST porteur du jeton interne (requête relayée par le Go) : le statut.
async fn call_internal(facade: std::net::SocketAddr, path: &str, body: &str) -> u16 {
    use http_body_util::Full;
    let client = hyper_util::client::legacy::Client::builder(hyper_util::rt::TokioExecutor::new())
        .build_http::<Full<bytes::Bytes>>();
    let req = hyper::Request::builder()
        .method("POST")
        .uri(format!("http://{facade}{path}"))
        .header("x-loki-internal", fakego::JETON)
        .body(Full::new(bytes::Bytes::from(body.to_string())))
        .unwrap();
    client.request(req).await.unwrap().status().as_u16()
}

/// Ouvre le flux SSE et lit jusqu'à `caught_up` : rend les événements.
async fn read_stream(
    facade: std::net::SocketAddr,
    body: &str,
) -> (hyper::HeaderMap, Vec<serde_json::Value>) {
    use http_body_util::{BodyExt, Full};
    let client = hyper_util::client::legacy::Client::builder(hyper_util::rt::TokioExecutor::new())
        .build_http::<Full<bytes::Bytes>>();
    let req = hyper::Request::builder()
        .method("POST")
        .uri(format!("http://{facade}/api/chat"))
        .body(Full::new(bytes::Bytes::from(body.to_string())))
        .unwrap();
    let resp = client.request(req).await.unwrap();
    let headers = resp.headers().clone();
    let mut body = resp.into_body();
    let mut text = String::new();
    let mut events = Vec::new();
    tokio::time::timeout(std::time::Duration::from_secs(5), async {
        while let Some(Ok(f)) = body.frame().await {
            if let Some(d) = f.data_ref() {
                text.push_str(&String::from_utf8_lossy(d));
            }
            while let Some(i) = text.find("\n\n") {
                let frame: String = text.drain(..i + 2).collect();
                if let Some(data) = frame.trim().strip_prefix("data: ") {
                    let v: serde_json::Value = serde_json::from_str(data).unwrap();
                    let delta = v["choices"][0]["delta"].clone();
                    let done = delta.get("caught_up").is_some();
                    events.push(delta);
                    if done {
                        return;
                    }
                }
            }
        }
    })
    .await
    .expect("flux sans caught_up");
    (headers, events)
}

#[tokio::test]
async fn routes_du_chat_par_la_facade() {
    let (url, _seen) = fake_engine(tool_then_answer("voilà")).await;
    let t = tempfile::tempdir().unwrap();
    let port: u16 = port_of(&url).parse().unwrap();
    let (facade, go, _stop, chat) = fakego::start_chat(t.path().into(), port).await;

    // Envoi, puis le flux rejoue le tour jusqu'à caught_up.
    let (code, v) = call(
        facade,
        "POST",
        "/api/chat/send",
        r#"{"message":"salut","cid":"r1"}"#,
    )
    .await;
    assert_eq!(
        (code, v.clone()),
        (200, serde_json::json!({"ok": true, "queued": false})),
        "{v}"
    );
    until_done(&chat, 1).await;
    let (headers, evs) = read_stream(facade, r#"{"from":0,"tail":20}"#).await;
    assert_eq!(headers["content-type"], "text/event-stream");
    assert!(evs.iter().any(|e| e["user"] == "salut"));
    assert!(evs.iter().any(|e| e["content"] == "voilà"));
    assert!(evs.iter().any(|e| e.get("turn_done").is_some()));
    // Les routes du chat ne partent plus au Go.
    assert!(
        !go.relayed
            .lock()
            .unwrap()
            .iter()
            .any(|p| p.starts_with("/api/chat"))
    );

    let (_, st) = call(facade, "GET", "/api/chat/state", "").await;
    assert_eq!(
        (st["generating"].clone(), st["turns"].clone()),
        (serde_json::json!(false), serde_json::json!(1))
    );
    let (code, v) = call(facade, "POST", "/api/chat/send", "{").await;
    assert_eq!((code, v["ok"].clone()), (400, serde_json::json!(false)));
    assert_eq!(
        call(facade, "GET", "/api/chat/tool-result", "").await.0,
        400
    );
    assert_eq!(
        call(facade, "GET", "/api/chat/tool-result?id=c9.zz", "")
            .await
            .0,
        404
    );

    // Discussions.
    let (_, list) = call(facade, "GET", "/api/conversations", "").await;
    let a = list["active"].as_str().unwrap().to_string();
    assert_eq!(list["conversations"].as_array().unwrap().len(), 1);
    assert_eq!(list["busy"], false);
    let (_, n) = call(facade, "POST", "/api/conversations/new", "").await;
    let b = n["id"].as_str().unwrap().to_string();
    assert_ne!(a, b);
    let (code, _) = call(
        facade,
        "POST",
        "/api/conversations/switch",
        &format!(r#"{{"id":"{a}"}}"#),
    )
    .await;
    assert_eq!(code, 200);
    let (code, v) = call(
        facade,
        "POST",
        "/api/conversations/switch",
        r#"{"id":"inconnue"}"#,
    )
    .await;
    assert_eq!(
        (code, v["error"].clone()),
        (400, serde_json::json!("discussion introuvable"))
    );
    assert_eq!(
        call(
            facade,
            "POST",
            "/api/conversations/rename",
            &format!(r#"{{"id":"{a}","title":"Mon fil"}}"#)
        )
        .await
        .0,
        200
    );
    assert_eq!(
        call(
            facade,
            "POST",
            "/api/conversations/delete",
            &format!(r#"{{"id":"{b}"}}"#)
        )
        .await
        .0,
        200
    );
    let (_, list) = call(facade, "GET", "/api/conversations", "").await;
    assert_eq!(list["conversations"][0]["title"], "Mon fil");

    // Mode Code : servi par le Go, qui pousse son événement au journal d'ici
    // par l'API interne de la façade (jeton interne exigé).
    assert_eq!(
        call(facade, "POST", "/api/chat/mode", r#"{"mode":"code"}"#)
            .await
            .0,
        200
    );
    assert!(
        go.relayed
            .lock()
            .unwrap()
            .contains(&"/api/chat/mode".to_string())
    );
    let (code, _) = call(
        facade,
        "POST",
        "/internal/facade/v1/delta",
        r#"{"mode":"code"}"#,
    )
    .await;
    assert_eq!(code, 404, "sans le jeton, l'API interne n'existe pas");
    let code = call_internal(facade, "/internal/facade/v1/delta", r#"{"mode":"code"}"#).await;
    assert_eq!(code, 204);
    assert_eq!(deltas(&chat, "mode"), [serde_json::json!("code")]);
    // Fil déjà rempli : le déverrouillage ne le recharge pas.
    assert_eq!(
        call_internal(facade, "/internal/facade/v1/reload", "").await,
        200
    );
    assert!(!chat.sessions.conv().with_state(|st| st.log.is_empty()));

    // Trafic remis par le Go (tunnel) : déjà authentifié par lui.
    go.set("state", "web_key", "cle-web");
    assert_eq!(call(facade, "GET", "/api/chat/state", "").await.0, 401);
    assert_eq!(call_internal(facade, "/api/chat/state", "").await, 200);

    // Reset : fil vide, fichiers de la discussion effacés par le Go.
    assert_eq!(call_internal(facade, "/api/chat/reset", "").await, 200);
    assert!(chat.sessions.conv().with_state(|st| st.log.is_empty()));
    assert!(
        go.conv_log
            .lock()
            .unwrap()
            .contains(&format!("dropped {a}"))
    );
}

#[tokio::test]
async fn deverrouillage_recharge_la_discussion_chargee_vide() {
    let (s, _h, go, _stop, _t) = sessions().await;
    go.set(BK_CHAT, "active", "c1");
    go.set(
        BK_CHAT,
        "conv:c1",
        r#"SCELLE:{"messages":[{"role":"user","content":"fil chiffré"}],"log":[{"seq":1,"ts":0,"delta":{"user":"fil chiffré"}}],"seq":1,"ctx_used":0}"#,
    );
    // Mémoire verrouillée au démarrage : fil vide, rien d'écrasé.
    *go.locked.lock().unwrap() = true;
    s.load().await.unwrap();
    assert_eq!(s.conv().id(), "c1");
    assert!(textes(&s).is_empty());
    assert_eq!(s.reload_if_empty().await, Ok(false), "toujours verrouillée");
    // Déverrouillée : le fil revient.
    *go.locked.lock().unwrap() = false;
    assert_eq!(s.reload_if_empty().await, Ok(true));
    assert_eq!(textes(&s), ["fil chiffré"]);
    // Déjà rempli : jamais rechargé par-dessus.
    assert_eq!(s.reload_if_empty().await, Ok(false));
}
