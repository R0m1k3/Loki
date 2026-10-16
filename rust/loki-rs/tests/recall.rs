//! Lot 46 : outils `recall` et `recall_search` de l'agent exécutés par la
//! façade, rejoués sur le journal du Go (`internal/loki/parity_recall_test.go`
//! → `tests/parity_recall/recall.json`) : la même archive, servie par le faux
//! Go comme le vrai la sert, les mêmes appels, les mêmes textes.

mod common;

use common::fakego;
use loki_rs::store::Store;
use serde_json::{Map, Value};

#[tokio::test]
async fn recall_comme_le_go() {
    let fx: Value =
        serde_json::from_str(include_str!("parity_recall/recall.json")).expect("journal");
    let tmp = tempfile::tempdir().unwrap();
    let (_, go, _stop) = fakego::start(tmp.path().join("home")).await;
    *go.recall_blocks.lock().unwrap() = fx["blocks"].as_array().unwrap().clone();
    let backend = go.backend.lock().unwrap().unwrap();
    let store = Store::new(backend, fakego::JETON.into());
    for st in fx["steps"].as_array().unwrap() {
        let name = st["name"].as_str().unwrap();
        let args: Map<String, Value> = serde_json::from_value(st["args"].clone()).unwrap();
        let got = loki_rs::recall::exec(&store, st["tool"].as_str().unwrap(), &args)
            .await
            .expect("le Go répond");
        assert_eq!(got, st["result"].as_str().unwrap(), "{name}");
    }
    // Une recherche lit l'archive une fois ; un id mal formé ne la lit pas.
    let log = go.recall_log.lock().unwrap().clone();
    assert!(log.contains(&"block 7".to_string()), "{log:?}");
    assert!(!log.iter().any(|l| l == "block "), "{log:?}");
}

/// Go injoignable : `None`, l'appel passe alors par lui (`/tool/exec`).
#[tokio::test]
async fn go_injoignable() {
    let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let addr = l.local_addr().unwrap();
    drop(l);
    let store = Store::new(addr, "x".into());
    let mut args = Map::new();
    args.insert("id".into(), Value::from("r1"));
    assert_eq!(loki_rs::recall::exec(&store, "recall", &args).await, None);
    let mut args = Map::new();
    args.insert("query".into(), Value::from("train"));
    assert_eq!(
        loki_rs::recall::exec(&store, "recall_search", &args).await,
        None
    );
    // Les refus avant lecture, eux, ne demandent rien au Go.
    assert_eq!(
        loki_rs::recall::exec(&store, "recall", &Map::new())
            .await
            .as_deref(),
        Some("[erreur] id manquant")
    );
}
