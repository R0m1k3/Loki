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
    BK_CHAT, BK_TOOL_RES, CK_INDEX, ConvSnap, ConvState, DEFAULT_PROJECT_SLUG, Persister,
    StoreSink, ToolResJob, decode_index, decode_state,
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

fn dire<H: SessionHooks>(s: &Sessions<H>, texte: &str) {
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

fn textes<H: SessionHooks>(s: &Sessions<H>) -> Vec<String> {
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

/// Qui tient la conversation : le Go en décide (réglage qu'il garde).
#[tokio::test]
async fn detenteur_de_la_conversation() {
    let (st, go, _stop, _t) = store().await;
    assert_eq!(loki_rs::chat::generate::go_keeps_chat(&st).await, Ok(None));
    *go.go_keeps_chat.lock().unwrap() = Some("PROJ_SNAPSHOT".into());
    assert_eq!(
        loki_rs::chat::generate::go_keeps_chat(&st).await,
        Ok(Some("PROJ_SNAPSHOT".into()))
    );
}

/// Changer de projet : la discussion quittée est rangée avec SON projet, la
/// plus récente du nouveau s'ouvre (une neuve s'il n'en a pas) ; supprimer un
/// projet emporte ses discussions.
#[tokio::test]
async fn changer_de_projet_et_en_supprimer_un() {
    let (st, go, _stop, _t) = store().await;
    let s = Sessions::new(st.clone(), Conversation::new(), GoSessionHooks(st.clone()));
    s.load().await.unwrap();
    let a = s.conv().id();
    dire(&s, "dans le projet par défaut");
    s.project_switch("deux").await.unwrap();
    assert_eq!(go.get("state", "active_project").unwrap(), "deux");
    let b = s.conv().id();
    assert_ne!(a, b);
    assert!(textes(&s).is_empty());
    let idx = s.read_index().await;
    let proj = |id: &str| idx.iter().find(|m| m.id == id).unwrap().project.clone();
    assert_eq!(proj(&a), DEFAULT_PROJECT_SLUG);
    assert_eq!(proj(&b), "deux");
    let body = opened(&go, BK_CHAT, &format!("conv:{a}")).unwrap();
    assert_eq!(decode_state(&body).messages.len(), 1);
    dire(&s, "dans deux");
    s.project_switch(DEFAULT_PROJECT_SLUG).await.unwrap();
    assert_eq!(s.conv().id(), a);
    assert_eq!(textes(&s), ["dans le projet par défaut"]);
    // Même projet : rien ; projet refusé par le Go : rien ne change.
    s.project_switch(DEFAULT_PROJECT_SLUG).await.unwrap();
    assert!(matches!(
        s.project_switch("absent").await,
        Err(SessionError::Store(_))
    ));
    assert_eq!(s.project_switch(" ").await, Err(SessionError::Missing));
    assert_eq!(s.conv().id(), a);
    assert_eq!(
        go.get("state", "active_project").unwrap(),
        DEFAULT_PROJECT_SLUG
    );
    // Les discussions de « deux » partent : contenu, index, nettoyage du Go.
    assert_eq!(s.drop_project("deux").await.unwrap(), 1);
    assert!(s.read_index().await.iter().all(|m| m.id != b));
    assert!(go.get(BK_CHAT, &format!("conv:{b}")).is_none());
    assert!(
        go.conv_log
            .lock()
            .unwrap()
            .contains(&format!("dropped {b}"))
    );
    assert_eq!(s.conv().id(), a);
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
use loki_rs::chat::tplprobe::{ProbeResult, Shape};
use loki_rs::chat::turn::{Cancel, TurnConfig, run_turn};
use loki_rs::chat::types::{Message, StreamEvent, Tool, ToolFunction};
use loki_rs::chat::wire::{ChatEndpoint, EchoRefusals, EffortFallbacks, TplTri, TurnReasoning};

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
                            // Sonde de gabarit (lancée après chaque complétion
                            // du fil principal) : un gabarit qui rend toujours
                            // la même chose. Ses rendus ne comptent pas parmi
                            // les requêtes vues.
                            if req.uri().path() == "/props" {
                                return ok(r#"{"build_info":"b1-x","model_path":"/m/m1.gguf","chat_template":"t"}"#.into());
                            }
                            if req.uri().path() == "/apply-template" {
                                let body = req.into_body().collect().await.unwrap().to_bytes();
                                let b: serde_json::Value = serde_json::from_slice(&body).unwrap();
                                PROBE_RENDERS.lock().unwrap().push(b.clone());
                                return ok(
                                    serde_json::json!({ "prompt": render_without_reasoning(&b) })
                                        .to_string(),
                                );
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
                            // « STATUS 400\n<corps> » : refus du moteur.
                            if let Some(rest) = out.strip_prefix("STATUS ")
                                && let Some((code, body)) = rest.split_once('\n')
                            {
                                let mut r = hyper::Response::new(Full::new(bytes::Bytes::from(
                                    body.to_string(),
                                )));
                                *r.status_mut() =
                                    hyper::StatusCode::from_u16(code.parse().unwrap()).unwrap();
                                return Ok(r);
                            }
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

/// Un gabarit qui ne rend jamais `reasoning_content` : rôle, texte, appels
/// d'outils, et l'amorce de réponse si elle est demandée.
fn render_without_reasoning(b: &serde_json::Value) -> String {
    let mut out: Vec<String> = Vec::new();
    for m in b["messages"].as_array().into_iter().flatten() {
        let calls: Vec<&str> = m["tool_calls"]
            .as_array()
            .into_iter()
            .flatten()
            .filter_map(|c| c["function"]["name"].as_str())
            .collect();
        out.push(format!(
            "{}:{}{}",
            m["role"].as_str().unwrap_or(""),
            m["content"].as_str().unwrap_or(""),
            calls.join(",")
        ));
    }
    let mut p = out.join("\n");
    if b["add_generation_prompt"] == true {
        p.push_str("\nassistant:");
    }
    p
}

/// Rendus demandés par la sonde de gabarit, tous moteurs confondus.
static PROBE_RENDERS: StdMutex<Vec<serde_json::Value>> = StdMutex::new(Vec::new());

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
        tpl: Arc::new(loki_rs::chat::tplprobe::TplProbe::new()),
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
            go: None,
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
        perf_kind: "main",
        side_slot: Default::default(),
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
    // Télémétrie : chaque complétion rangée par le Go, rattachée à la
    // discussion ; la première, sans usage, reste incomplète.
    let perfs = go.perf_records.lock().unwrap().clone();
    assert_eq!(perfs.len(), 2);
    assert_eq!(perfs[0]["complete"], false);
    assert_eq!(perfs[0]["prefix"], serde_json::json!([]));
    let r1 = &perfs[1];
    assert_eq!(
        (
            r1["conv"].clone(),
            r1["kind"].clone(),
            r1["iter"].clone(),
            r1["stats"]["prompt_tokens_total"].clone()
        ),
        (
            serde_json::json!("c7"),
            serde_json::json!("main"),
            serde_json::json!(1),
            serde_json::json!(50)
        )
    );
    assert_eq!(perfs[1]["prefix"].as_array().unwrap().len(), 3);
    assert_eq!(perfs[1]["external"], false);
    let last = evs
        .iter()
        .rev()
        .find_map(|e| match e {
            StreamEvent::Stats(s) if !s.kind.is_empty() => Some(s.clone()),
            _ => None,
        })
        .unwrap();
    assert_eq!(
        (last.kind.as_str(), last.lost, last.lost_after.as_str()),
        ("main", Some(3), "tâche")
    );
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
    // Chaque requête au moteur local est comptée en vol par le Go (ticket
    // ouvert, servi, rendu), et sa complétion rangée dans la télémétrie ;
    // la copie finale des stats porte ce que le Go en dit.
    until(|| {
        go.oai_log
            .lock()
            .unwrap()
            .iter()
            .filter(|l| l.starts_with("end"))
            .count()
            == 2
    })
    .await;
    let log = go.oai_log.lock().unwrap().clone();
    let want_begin = format!("begin main_conv={id}");
    assert_eq!(
        log.iter().filter(|l| **l == want_begin).count(),
        2,
        "{log:?}"
    );
    assert_eq!(
        log.iter().filter(|l| *l == "served 7").count(),
        2,
        "{log:?}"
    );
    // Appel d'outil sans usage ni timings : complétion incomplète (comme le
    // Go : ni empreintes) ; la réponse finale, avec usage, complète. La
    // sonde du gabarit reste à la façade (`sonde_de_gabarit_apres_un_tour`).
    for want in [
        format!("perf kind=main conv={id} iter=0 complete=false prefix=0"),
        format!("perf kind=main conv={id} iter=1 complete=true prefix=4"),
    ] {
        assert!(log.contains(&want), "{want} absent de {log:?}");
    }
    let finals: Vec<_> = deltas(&svc, "stats")
        .into_iter()
        .filter(|s| s["lost_alert"] == true)
        .collect();
    assert_eq!(finals.len(), 1, "stats finales sans les marques du Go");
    assert_eq!(finals[0]["lost"], 3);
    assert_eq!(finals[0]["kind"], "main");
    let body = opened(&go, BK_CHAT, &format!("conv:{id}")).unwrap();
    assert_eq!(decode_state(&body).messages.len(), 4);
    // Réessai du même envoi : reconnu.
    let again = svc
        .send(serde_json::json!({"message": "liste", "cid": "a1"}))
        .await
        .unwrap();
    assert!(again.dup);
}

/// Lot 23 : après un tour, la façade sonde le gabarit du moteur avec la
/// forme de la requête (mêmes outils), et range le verdict que le Go relit
/// pour son résumé des performances.
#[tokio::test]
async fn sonde_de_gabarit_apres_un_tour() {
    let (url, _seen) = fake_engine(tool_then_answer("fini")).await;
    let (svc, _go, _stop, _t) = service(Some(&url)).await;
    svc.send(serde_json::json!({"message": "liste", "cid": "s1"}))
        .await
        .unwrap();
    until_done(&svc, 1).await;
    until(|| svc.shared.tpl.caps_current(false).is_some()).await;
    let r = svc.shared.tpl.caps_current(false).unwrap();
    assert_eq!(r.tools, 1);
    assert_eq!(r.build, "b1-x");
    assert_eq!(r.model, "m1.gguf");
    assert_eq!(r.note, "reasoning_content jamais rendu");
    assert_eq!(
        (r.preserves_history, r.prefix_stable, r.renders_reasoning),
        (TplTri::No, TplTri::Yes, TplTri::No)
    );
    let renders = PROBE_RENDERS.lock().unwrap().clone();
    assert!(
        renders
            .iter()
            .any(|b| b["tools"][0]["function"]["name"] == "bash"
                && b["parallel_tool_calls"] == false),
        "{renders:?}"
    );
    // Un preset externe : rien d'affiché.
    assert!(svc.shared.tpl.caps_current(true).is_none());
}

/// Le verdict de la sonde décide pour l'hôte : rappel dans le résultat
/// d'outil seulement si le préfixe est instable pour CE modèle ; sonde
/// lancée pour le fil principal, jamais pour une tâche.
#[tokio::test]
async fn hote_et_sonde_de_gabarit() {
    let (svc, _go, _stop, _t) = service(None).await;
    let shared = svc.shared.clone();
    let host = |main: &str, model: &str| {
        let cfg = std::collections::HashMap::from([("MODEL".to_string(), model.to_string())]);
        let ep = ChatEndpoint {
            url: "http://127.0.0.1:1/v1/chat/completions".into(),
            model: "loki".into(),
            key: String::new(),
            external: false,
        };
        GoTurnHost {
            shared: shared.clone(),
            conv_id: "c1".into(),
            caps: serde_json::json!({}),
            cfg: cfg.clone(),
            port: 1,
            summarizer: EngineSummarizer {
                http: shared.http.clone(),
                ep,
                cfg,
                patterns: shared.patterns.clone(),
                port: 1,
                go: None,
            },
            compact: TurnCompact {
                cfg: CompactConfig::default(),
                caps: CompactCaps::default(),
                criteria: String::new(),
                main_conv: main.into(),
                tools: vec![],
                effort: String::new(),
                kwargs: Default::default(),
            },
            queued: Box::new(Vec::new),
            perf_kind: "main",
            side_slot: Default::default(),
        }
    };
    let bash = Tool {
        kind: "function".into(),
        function: ToolFunction {
            name: "bash".into(),
            description: String::new(),
            parameters: serde_json::json!({"type":"object"}),
        },
    };
    let tools = vec![bash];
    let shape = Shape::new(&tools, None, "");
    let mut r = ProbeResult::unknown("");
    r.model = "m1.gguf".into();
    r.prefix_stable = TplTri::No;
    shared.tpl.remember(&shape, r.clone());
    use loki_rs::chat::turn::TurnHost;
    assert!(host("c1", "/m/m1.gguf").prefix_unstable(&tools, None, ""));
    assert!(!host("c1", "/m/autre.gguf").prefix_unstable(&tools, None, ""));
    assert!(!host("c1", "/m/m1.gguf").prefix_unstable(&[], None, ""));
    r.prefix_stable = TplTri::Yes;
    shared.tpl.remember(&shape, r.clone());
    assert!(!host("c1", "/m/m1.gguf").prefix_unstable(&tools, None, ""));
    r.prefix_stable = TplTri::No;
    r.model = String::new();
    shared.tpl.remember(&shape, r);
    assert!(!host("c1", "").prefix_unstable(&tools, None, ""));

    // Une tâche (pas de discussion principale) ne lance pas la sonde.
    host("", "").completed(&tools, None, "");
    tokio::time::sleep(std::time::Duration::from_millis(100)).await;
    assert_eq!(shared.tpl.last_shape(), Shape::default());
    host("c1", "").completed(&tools, None, "");
    until(|| shared.tpl.last_shape() == shape).await;
}

/// REASONING_ECHO : un gabarit qui ne rend jamais le raisonnement (verdict de
/// la sonde, après le premier tour) coupe l'écho au tour suivant.
#[tokio::test]
async fn echo_coupe_par_le_verdict_de_la_sonde() {
    let reasoned = sse(&[
        serde_json::json!({"choices":[{"delta":{"reasoning_content": "je réfléchis"}}]}),
        serde_json::json!({"choices":[{"delta":{"content": "un"}}]}),
        serde_json::json!({"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":40,"completion_tokens":2}}),
    ]);
    let (url, seen) = fake_engine(vec![reasoned.clone(), reasoned]).await;
    let (svc, go, _stop, _t) = service(Some(&url)).await;
    go.set("config", "REASONING_ECHO", "on");
    go.set("config", "MODEL", "/m/m1.gguf");
    svc.send(serde_json::json!({"message": "q1", "cid": "e1"}))
        .await
        .unwrap();
    until_done(&svc, 1).await;
    until(|| svc.shared.tpl.caps_current(false).is_some()).await;
    assert_eq!(
        svc.shared
            .tpl
            .caps_current(false)
            .unwrap()
            .renders_reasoning,
        TplTri::No
    );
    svc.send(serde_json::json!({"message": "q2", "cid": "e2"}))
        .await
        .unwrap();
    until_done(&svc, 2).await;
    let seen = seen.lock().unwrap().clone();
    let second = &seen.last().unwrap().1;
    let past = second["messages"]
        .as_array()
        .unwrap()
        .iter()
        .find(|m| m["role"] == "assistant")
        .unwrap();
    assert!(past.get("reasoning_content").is_none(), "{past}");
}

/// Contenu seul, fin normale, avec l'usage (une mesure par complétion).
fn answer(text: &str) -> String {
    sse(&[
        serde_json::json!({"choices":[{"delta":{"content": text}}]}),
        serde_json::json!({"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":40,"completion_tokens":2}}),
    ])
}

fn contents_of(msgs: &[loki_rs::chat::types::Message]) -> Vec<String> {
    msgs.iter()
        .map(|m| {
            let c = m.content.as_ref().and_then(|c| c.as_str()).unwrap_or("");
            format!("{}:{}", m.role, c.lines().next().unwrap_or(""))
        })
        .collect()
}

/// Lot 7 : contrat du mode Code tenu. Critère déclaré fait par le builder →
/// passe de vérification sur un contexte isolé, qui le marque « passed » ; sa
/// trace ne rejoint pas l'historique.
#[tokio::test]
async fn contrat_verifie_par_la_facade() {
    let (url, seen) = fake_engine(vec![
        answer("fait"),
        sse(&[
            serde_json::json!({"choices":[{"delta":{"tool_calls":[{"index":0,"id":"v1","type":"function","function":{"name":"criteria","arguments":"{\"action\":\"set\",\"id\":1,\"status\":\"passed\"}"}}]}}]}),
            serde_json::json!({"choices":[{"delta":{},"finish_reason":"tool_calls"}]}),
        ]),
        answer("vérifié"),
    ])
    .await;
    let (svc, go, _stop, _t) = service(Some(&url)).await;
    *go.code.lock().unwrap() = true;
    *go.turn_side_live.lock().unwrap() = true;
    let id = svc.sessions.conv().id();
    go.set(
        "chat",
        &format!("crit:{id}"),
        r#"[{"id":1,"text":"les tests passent","status":"completed"}]"#,
    );
    svc.send(serde_json::json!({"message": "fais-le", "cid": "v1"}))
        .await
        .unwrap();
    until_done(&svc, 1).await;
    until(|| go.turn_log.lock().unwrap().contains(&"done".to_string())).await;

    let seen = seen.lock().unwrap().clone();
    assert_eq!(seen.len(), 3);
    // SIDE_SLOT (lot 31) : le tour sur le slot 0 (rechargement SLOT_PERSIST
    // demandé pour la discussion), la vérification sur le 1, comptée en vol
    // sur le second slot ; la passe est un travail annexe pour le Go.
    let slots: Vec<_> = seen.iter().map(|(_, b)| b["id_slot"].clone()).collect();
    assert_eq!(
        slots,
        [
            serde_json::json!(0),
            serde_json::json!(1),
            serde_json::json!(1)
        ]
    );
    assert_eq!(
        *go.turn_slots.lock().unwrap(),
        [format!("{id} true"), " false".into(), " false".into()]
    );
    let begins: Vec<String> = go
        .oai_log
        .lock()
        .unwrap()
        .iter()
        .filter(|l| l.starts_with("begin"))
        .cloned()
        .collect();
    assert_eq!(
        begins,
        [
            format!("begin main_conv={id}"),
            "begin main_conv= side".into(),
            "begin main_conv= side".into()
        ]
    );
    assert_eq!(*go.side_jobs.lock().unwrap(), ["begin 1", "end 1 false"]);
    let v = &seen[1].1;
    assert_eq!(v["messages"][0]["content"], "VÉRIFICATEUR");
    assert_eq!(
        v["messages"][1]["content"],
        "Task:\nfais-le\n\nAcceptance criteria:\n[~] #1 les tests passent\n\n\
         Verify each non-passed criterion now (git_diff first), and record every verdict with the criteria tool."
    );
    assert_eq!(v["tools"][0]["function"]["name"], "criteria");
    assert_eq!(*go.tool_log.lock().unwrap(), ["criteria verifier"]);
    assert_eq!(
        deltas(&svc, "role"),
        [serde_json::json!("verifier"), serde_json::json!("")]
    );
    assert_eq!(deltas(&svc, "verify_done"), [serde_json::json!("passed")]);
    // Mesures du vérificateur, et elles seules : à part (la jauge de la
    // discussion n'en dit rien).
    let (inside, outside) = svc.sessions.conv().with_state(|st| {
        let (mut verifier, mut inside, mut outside) = (false, 0, 0);
        for e in &st.log {
            if let Some(r) = e.delta.get("role") {
                verifier = r == "verifier";
            }
            if e.delta.contains_key("stats") {
                let iso = e.delta.get("ctx_isolated") == Some(&serde_json::json!(true));
                assert_eq!(iso, verifier, "{:?}", e.delta);
                if verifier {
                    inside += 1;
                } else {
                    outside += 1;
                }
            }
        }
        (inside, outside)
    });
    assert!(inside > 0 && outside > 0);
    let msgs = svc.sessions.conv().with_state(|st| st.messages.clone());
    assert_eq!(contents_of(&msgs), ["user:fais-le", "assistant:fait"]);
}

/// Lot 7 : contrat jamais tenu. Deux relances du builder sur les critères
/// ouverts, puis trois vérifications séparées par deux corrections, et
/// l'abandon dit dans le fil. Les relances et corrections rejoignent
/// l'historique ; les vérifications non.
#[tokio::test]
async fn contrat_abandonne_apres_deux_corrections() {
    let script = ["r0", "r1", "r2", "v1", "r3", "v2", "r4", "v3"]
        .iter()
        .map(|t| answer(t))
        .collect();
    let (url, seen) = fake_engine(script).await;
    let (svc, go, _stop, _t) = service(Some(&url)).await;
    *go.code.lock().unwrap() = true;
    let id = svc.sessions.conv().id();
    go.set(
        "chat",
        &format!("crit:{id}"),
        r#"[{"id":1,"text":"build ok","status":"pending"}]"#,
    );
    svc.send(serde_json::json!({"message": "construis", "cid": "v2"}))
        .await
        .unwrap();
    until_done(&svc, 1).await;
    until(|| go.turn_log.lock().unwrap().contains(&"done".to_string())).await;

    let seen = seen.lock().unwrap().clone();
    assert_eq!(seen.len(), 8);
    // Relance : historique + consigne, même tête (préambule fusionné).
    let r1 = &seen[1].1["messages"];
    assert_eq!(r1[0]["content"], "PRÉAMBULE\n\nTu es Loki.");
    let last = r1.as_array().unwrap().last().unwrap()["content"]
        .as_str()
        .unwrap();
    assert!(
        last.starts_with(
            "Not done yet — these acceptance criteria are still open:\n[ ] #1 build ok\n\n"
        ),
        "{last}"
    );
    assert_eq!(seen[3].1["messages"][0]["content"], "VÉRIFICATEUR");
    let fix = seen[4].1["messages"].as_array().unwrap().last().unwrap()["content"]
        .as_str()
        .unwrap()
        .to_string();
    assert!(
        fix.starts_with("Verification failed on these criteria:\n[ ] #1 build ok\n\nFix the code"),
        "{fix}"
    );

    assert_eq!(deltas(&svc, "verify_done"), [serde_json::json!("gave_up")]);
    let roles: Vec<String> = deltas(&svc, "role")
        .iter()
        .map(|r| r.as_str().unwrap().to_string())
        .collect();
    assert_eq!(
        roles,
        [
            "builder", "", "builder", "", "verifier", "", "builder", "", "verifier", "", "builder",
            "", "verifier", ""
        ]
    );
    let content = deltas(&svc, "content");
    assert_eq!(
        content.last().unwrap(),
        "\n\n_(vérification : 1 critère(s) non satisfaits après 2 corrections — voir le panneau des critères)_"
    );
    let msgs = svc.sessions.conv().with_state(|st| st.messages.clone());
    assert_eq!(
        contents_of(&msgs),
        [
            "user:construis",
            "assistant:r0",
            "user:Not done yet — these acceptance criteria are still open:",
            "assistant:r1",
            "user:Not done yet — these acceptance criteria are still open:",
            "assistant:r2",
            "user:Verification failed on these criteria:",
            "assistant:r3",
            "user:Verification failed on these criteria:",
            "assistant:r4",
        ]
    );
    // Enregistrée telle quelle.
    let body = opened(&go, BK_CHAT, &format!("conv:{id}")).unwrap();
    assert_eq!(decode_state(&body).messages.len(), 10);
}

/// Lot 7 : un `read` s'exécute dans la façade, dans le dossier de travail que
/// lui donne le Go, et la lecture est notée dans le suivi du Go.
#[tokio::test]
async fn outil_execute_par_la_facade() {
    let ws = tempfile::tempdir().unwrap();
    std::fs::write(ws.path().join("notes.txt"), "première\nseconde\n").unwrap();
    let script = vec![
        sse(&[
            serde_json::json!({"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"read","arguments":"{\"file\":\"notes.txt\"}"}}]}}]}),
            serde_json::json!({"choices":[{"delta":{},"finish_reason":"tool_calls"}]}),
        ]),
        sse(&[
            serde_json::json!({"choices":[{"delta":{"content": "lu"}}]}),
            serde_json::json!({"choices":[{"delta":{},"finish_reason":"stop"}]}),
        ]),
    ];
    let (url, seen) = fake_engine(script).await;
    let (svc, go, _stop, _t) = service(Some(&url)).await;
    *go.tool_ctx.lock().unwrap() = Some(serde_json::json!({
        "cwd": ws.path(),
        "memory_dir": ws.path().join("memory"),
        "target": "",
    }));
    svc.send(serde_json::json!({"message": "lis", "cid": "b1"}))
        .await
        .unwrap();
    until_done(&svc, 1).await;
    let tool = deltas(&svc, "tool_used")
        .into_iter()
        .find(|t| t["done"] == true)
        .unwrap();
    assert_eq!(tool["result"], "1: première\n2: seconde");
    // Le résultat est reparti vers le moteur, sans passer par /tool/exec.
    let seen = seen.lock().unwrap().clone();
    assert_eq!(
        seen[1].1["messages"].as_array().unwrap().last().unwrap()["content"],
        "1: première\n2: seconde"
    );
    let notes = ws.path().join("notes.txt");
    assert_eq!(
        *go.tracker_log.lock().unwrap(),
        [format!("read {}", notes.display())]
    );
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

    // Mode Code : servi par la façade (lot 19), son événement au journal
    // d'ici, sans passer par le Go.
    assert_eq!(
        call(facade, "POST", "/api/chat/mode", r#"{"mode":"code"}"#)
            .await
            .0,
        200
    );
    assert!(
        !go.relayed
            .lock()
            .unwrap()
            .contains(&"/api/chat/mode".to_string())
    );
    assert_eq!(deltas(&chat, "mode"), [serde_json::json!("code")]);
    // Le Go pousse encore ses propres événements (indice de code, outil
    // criteria) par l'API interne de la façade (jeton interne exigé).
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
    assert_eq!(
        deltas(&chat, "mode"),
        [serde_json::json!("code"), serde_json::json!("code")]
    );
    // Fil déjà rempli : le déverrouillage ne le recharge pas.
    assert_eq!(
        call_internal(facade, "/internal/facade/v1/reload", "").await,
        200
    );
    assert!(!chat.sessions.conv().with_state(|st| st.log.is_empty()));

    // Changement de projet remis par le Go : la discussion part avec son
    // projet, une neuve s'ouvre dans le nouveau.
    assert_eq!(
        call_internal(
            facade,
            "/internal/facade/v1/project/switch",
            r#"{"slug":"deux"}"#
        )
        .await,
        200
    );
    assert_ne!(chat.sessions.conv().id(), a);
    assert_eq!(
        call_internal(
            facade,
            "/internal/facade/v1/project/switch",
            r#"{"slug":""}"#
        )
        .await,
        400
    );
    assert_eq!(
        call_internal(
            facade,
            "/internal/facade/v1/project/switch",
            r#"{"slug":"generale"}"#
        )
        .await,
        200
    );
    assert_eq!(chat.sessions.conv().id(), a);

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

/// Parité d'un tour COMPLET avec le Go (`tests/parity_turn/*.json`, produits
/// par `TestPariteTourComplet` en passant par la vraie route /api/chat/send) :
/// le Go de test sert les mêmes réponses de /turn/start et /turn/view que le
/// vrai, le moteur les mêmes flux, les outils les mêmes résultats. Doivent se
/// retrouver : le journal d'affichage du tour (mesures masquées), l'historique
/// rangé, et les corps envoyés au moteur.
#[tokio::test]
async fn parite_du_tour_complet() {
    let dir = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("tests/parity_turn");
    let mut paths: Vec<_> = std::fs::read_dir(&dir)
        .unwrap()
        .map(|e| e.unwrap().path())
        .filter(|p| p.extension().is_some_and(|x| x == "json"))
        .collect();
    paths.sort();
    assert!(!paths.is_empty());
    let mut failures = Vec::new();
    for p in &paths {
        let fx: serde_json::Value = serde_json::from_slice(&std::fs::read(p).unwrap()).unwrap();
        let name = fx["name"].as_str().unwrap().to_string();
        let replies: Vec<String> = fx["replies"]
            .as_array()
            .unwrap()
            .iter()
            .map(|r| {
                let body: String = r["lines"]
                    .as_array()
                    .unwrap()
                    .iter()
                    .map(|l| format!("{}\n\n", l.as_str().unwrap()))
                    .collect();
                match r["status"].as_u64().unwrap() {
                    200 => body,
                    s => format!("STATUS {s}\n{body}"),
                }
            })
            .collect();
        let (url, seen) = fake_engine(replies).await;
        let (svc, go, _stop, _t) = service(Some(&url)).await;
        *go.turn_start.lock().unwrap() = Some(fx["start"].clone());
        *go.turn_view.lock().unwrap() = Some(fx["view"].clone());
        *go.tool_results.lock().unwrap() =
            serde_json::from_value(fx["tool_results"].clone()).unwrap();
        if !fx["verifier"].is_null() {
            *go.turn_verifier.lock().unwrap() = Some(fx["verifier"].clone());
        }
        if let Some(c) = fx.get("criteria") {
            let id = svc.sessions.conv().id();
            go.set("chat", &format!("crit:{id}"), &c.to_string());
        }
        let sent = svc.send(fx["send"].clone()).await;
        let status = if sent.is_ok() { 200 } else { 0 };
        until_done(&svc, 1).await;
        until(|| go.turn_log.lock().unwrap().contains(&"done".to_string())).await;

        let exp = &fx["expect"];
        let mut diffs = Vec::new();
        if status != exp["status"].as_u64().unwrap() {
            diffs.push(format!("statut {status}, attendu {}", exp["status"]));
        }
        let requests: Vec<serde_json::Value> = seen
            .lock()
            .unwrap()
            .iter()
            .map(|(_, b)| b.clone())
            .collect();
        if serde_json::Value::Array(requests.clone()) != exp["requests"] {
            diffs.push(format!(
                "requêtes :\n  obtenu  {}\n  attendu {}",
                serde_json::Value::Array(requests),
                exp["requests"]
            ));
        }
        let log: Vec<serde_json::Value> = svc.sessions.conv().with_state(|st| {
            st.log
                .iter()
                .map(|e| {
                    let mut d = e.delta.clone();
                    d.remove("ts0");
                    if d.contains_key("stats") {
                        d.insert("stats".into(), serde_json::json!("…"));
                    }
                    serde_json::Value::Object(d)
                })
                .collect()
        });
        if serde_json::Value::Array(log.clone()) != exp["log"] {
            diffs.push(format!(
                "journal :\n  obtenu  {}\n  attendu {}",
                serde_json::Value::Array(log),
                exp["log"]
            ));
        }
        let msgs = svc.sessions.conv().with_state(|st| st.messages.clone());
        let msgs = serde_json::to_value(&msgs).unwrap();
        if msgs != exp["messages"] {
            diffs.push(format!(
                "historique :\n  obtenu  {msgs}\n  attendu {}",
                exp["messages"]
            ));
        }
        if !diffs.is_empty() {
            failures.push(format!("{name} :\n{}", diffs.join("\n")));
        }
    }
    assert!(
        failures.is_empty(),
        "{} tour(s) sur {} divergent du Go :\n\n{}",
        failures.len(),
        paths.len(),
        failures.join("\n\n")
    );
}

/// Lot 29 : le résumé d'une compaction rejoint la télémétrie du Go
/// (kind=compact, rattaché à la discussion), dès que la réponse se lit.
#[tokio::test]
async fn telemetrie_des_resumes() {
    use loki_rs::chat::compact::{Summarizer, TranscriptRequest};
    let (st, go, _stop, _t) = store().await;
    let (url, seen) = fake_engine(vec![
        serde_json::json!({
            "choices": [{"message": {"content": "résumé"}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 3000, "completion_tokens": 200},
            "timings": {"prompt_n": 3000, "prompt_ms": 1500, "predicted_n": 200, "predicted_per_second": 40}
        })
        .to_string(),
        r#"{"choices":[]}"#.into(),
        "pas du json".into(),
        serde_json::json!({
            "choices": [{"message": {"content": "suite"}, "finish_reason": "stop"}]
        })
        .to_string(),
    ])
    .await;
    let s = EngineSummarizer {
        http: EngineHttp::new(),
        ep: ChatEndpoint {
            url,
            model: "loki".into(),
            key: String::new(),
            external: false,
        },
        cfg: Default::default(),
        patterns: RetryPatterns::builtin(),
        port: 8080,
        go: Some((st, "c9".into())),
    };
    *go.turn_side_live.lock().unwrap() = true;
    let req = || TranscriptRequest {
        system: "s".into(),
        transcript: "t".into(),
        max_tokens: 500,
    };
    assert_eq!(
        s.summarize_transcript(req()).await.unwrap().content,
        "résumé"
    );
    assert!(s.summarize_transcript(req()).await.is_err(), "réponse vide");
    assert!(s.summarize_transcript(req()).await.is_err(), "illisible");
    // Lot 31 : la transcription, prompt neuf, sur le second slot, comptée en
    // vol sur lui et rendue à la fin ; jamais de rechargement SLOT_PERSIST.
    let sent = seen.lock().unwrap().clone();
    assert!(sent.iter().all(|(_, b)| b["id_slot"] == 1), "{sent:?}");
    assert_eq!(
        *go.turn_slots.lock().unwrap(),
        [" false", " false", " false"]
    );
    until(|| {
        go.oai_log
            .lock()
            .unwrap()
            .iter()
            .filter(|l| l.starts_with("end"))
            .count()
            == 3
    })
    .await;
    assert_eq!(
        go.oai_log
            .lock()
            .unwrap()
            .iter()
            .filter(|l| *l == "begin main_conv= side")
            .count(),
        3
    );
    // En continuation, il prolonge le prompt de la discussion : le slot 0.
    use loki_rs::chat::compact::ContinuationRequest;
    let cont = s
        .summarize_continuation(ContinuationRequest {
            messages: vec![],
            tools: vec![],
            effort: String::new(),
            kwargs: Default::default(),
            max_tokens: 500,
            conv: "c9".into(),
        })
        .await
        .unwrap();
    assert_eq!(cont.content, "suite");
    assert_eq!(seen.lock().unwrap()[3].1["id_slot"], 0);
    assert_eq!(go.turn_slots.lock().unwrap()[3], " true");
    until(|| {
        go.oai_log
            .lock()
            .unwrap()
            .contains(&"begin main_conv=".to_string())
    })
    .await;
    let perfs = go.perf_records.lock().unwrap().clone();
    assert_eq!(perfs.len(), 3, "la réponse illisible n'est pas rangée");
    assert_eq!(
        perfs[0],
        serde_json::json!({ "kind": "compact", "conv": "c9", "iter": 0, "complete": true,
            "external": false, "prefix": [],
            "stats": { "prompt_tokens_total": 3000, "prompt_tokens": 3000, "prompt_ms": 1500.0,
                       "gen_tokens": 200, "gen_per_second": 40.0 } })
    );
    assert_eq!(perfs[1]["complete"], false);
}

/// Lot 32 : KEEP_TURN_IMAGES tenu par la façade. L'image relayée par un
/// outil est rangée par le Go, gardée au coût mesuré par le moteur ; au tour
/// suivant, clé retirée, elle part d'abord (légende et marque gardées).
#[tokio::test]
async fn images_gardees_par_la_facade() {
    use loki_rs::chat::compact::{IMAGE_LOST_MARKER, msg_tokens};
    use loki_rs::chat::types::Message;
    let tool = Message {
        role: "tool".into(),
        tool_call_id: "c1".into(),
        content: Some(serde_json::json!("exec:bash")),
        ..Default::default()
    };
    let prompt2 = 1020 + msg_tokens(&tool) + 300;
    let (url, seen) = fake_engine(vec![
        sse(&[
            serde_json::json!({"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}]}}]}),
            serde_json::json!({"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1000,"completion_tokens":20}}),
        ]),
        sse(&[
            serde_json::json!({"choices":[{"delta":{"content": "vu"}}]}),
            serde_json::json!({"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":prompt2,"completion_tokens":5}}),
        ]),
        answer("ensuite"),
    ])
    .await;
    let (svc, go, _stop, t) = service(Some(&url)).await;
    // Le fichier que le Go range (ici, le faux ne l'écrit pas).
    std::fs::create_dir_all(t.path().join("chatimg")).unwrap();
    std::fs::write(t.path().join("chatimg/k1.png"), b"\x89PNG\r\n").unwrap();
    go.set("config", "KEEP_TURN_IMAGES", "on");
    *go.keep_vision.lock().unwrap() = true;
    *go.tool_relay.lock().unwrap() = Some(serde_json::json!({
        "role": "user",
        "content": [
            {"type": "text", "text": "Voici la capture demandée."},
            {"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw0K"}}
        ]
    }));
    svc.send(serde_json::json!({"message": "capture", "cid": "k1"}))
        .await
        .unwrap();
    until_done(&svc, 1).await;
    until(|| go.turn_log.lock().unwrap().contains(&"done".to_string())).await;
    // Rangée par le Go, vue en data-URL par le moteur.
    assert_eq!(go.keep_stores.lock().unwrap().len(), 1);
    let sent = seen.lock().unwrap()[1].1.clone();
    let relay = &sent["messages"].as_array().unwrap()[4];
    assert_eq!(
        relay["content"][1]["image_url"]["url"],
        "data:image/png;base64,iVBORw0K"
    );
    assert!(relay.get("img_relay").is_none(), "marque envoyée au moteur");
    let msgs = svc.sessions.conv().with_state(|st| st.messages.clone());
    let kept = msgs.iter().find(|m| m.img_relay).expect("relais gardé");
    assert_eq!(kept.img_tokens, 300);
    assert_eq!(
        kept.content.as_ref().unwrap()[1]["image_url"]["url"],
        "loki-img:k1.png"
    );

    // Clé retirée : l'image gardée part au début du tour suivant.
    go.set("config", "KEEP_TURN_IMAGES", "off");
    svc.send(serde_json::json!({"message": "et ensuite ?", "cid": "k2"}))
        .await
        .unwrap();
    until_done(&svc, 2).await;
    let msgs = svc.sessions.conv().with_state(|st| st.messages.clone());
    let stub = msgs
        .iter()
        .find(|m| m.img_relay)
        .expect("relais gardé en légende");
    assert_eq!(stub.img_tokens, 0);
    assert_eq!(
        stub.content,
        Some(serde_json::json!(format!(
            "Voici la capture demandée.{IMAGE_LOST_MARKER}"
        )))
    );
    let sent = seen.lock().unwrap()[2].1.clone();
    assert!(
        sent["messages"]
            .as_array()
            .unwrap()
            .iter()
            .all(|m| !m["content"].to_string().contains("image_url")),
        "image encore envoyée : {sent}"
    );
}
