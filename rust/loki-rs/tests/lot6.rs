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
