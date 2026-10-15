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
    let p = Persister::new(StoreSink { store: st });
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
    let p = Persister::new(StoreSink { store: st });
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
    let p = Persister::new(StoreSink { store: st });
    p.persist(snap("c1", p.next_seq(), "t", b"{}".to_vec()))
        .await;
    assert_eq!(opened(&go, BK_CHAT, "conv:c1").unwrap(), b"{}");
    assert_eq!(go.get(BK_CHAT, CK_INDEX).unwrap(), "SCELLE:pas du json");
}
