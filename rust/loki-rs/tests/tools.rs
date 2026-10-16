//! Parité des outils fichiers et du shell avec le Go (lot 7).
//!
//! `tests/parity_tools/outils.json` : journal produit par `execTool`
//! (`internal/loki/parity_tools_test.go`, régénéré par
//! `LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteOutils`). La
//! même arborescence est recréée ici et les mêmes appels passent, dans le même
//! ordre, par `tools::exec_local` (`common::toolsfx`) : libellés, résultats,
//! diffs, questions et arborescence finale doivent être ceux du Go.

mod common;

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Mutex;

use serde_json::{Value, json};

use common::toolsfx::{FakeState, replay};
use loki_rs::chat::stream::tool_call_label;
use loki_rs::tools::{ToolCtx, exec_local};

#[tokio::test]
async fn outils_comme_le_go() {
    let diffs = replay("outils").await;
    assert!(
        diffs.is_empty(),
        "{} écart(s) avec le Go :\n\n{}",
        diffs.len(),
        diffs.join("\n\n")
    );
}

/// Outils non portés, ou cible distante : c'est au Go de les exécuter.
#[tokio::test]
async fn le_reste_va_au_go() {
    let st = FakeState {
        ctx: ToolCtx {
            cwd: PathBuf::from("/nulle-part"),
            memory_dir: PathBuf::from("/nulle-part/memory"),
            target: "poste".into(),
            code: false,
        },
        reads: Mutex::new(HashMap::new()),
    };
    let a = |v: Value| v.as_object().unwrap().clone();
    for (name, args) in [
        ("mem_read", json!({"file":"x"})),
        ("web_search", json!({"query":"x"})),
        ("write", json!({"file":"a","content":"x"})),
        ("edit", json!({"file":"a","old":"x","new":"y"})),
        ("bash", json!({"command":"ls"})),
    ] {
        let args = a(args);
        let label = tool_call_label(name, &args);
        assert!(
            exec_local(&st, false, name, &args, &label).await.is_none(),
            "{name}"
        );
    }
    // Une commande catastrophique est refusée ici, cible distante comprise.
    let args = a(json!({"command":"rm -rf /"}));
    let out = exec_local(&st, false, "bash", &args, "rm -rf /")
        .await
        .unwrap();
    assert!(out.result.starts_with("[refusé] Commande bloquée"));
    // read/grep/glob lisent toujours cette machine.
    let args = a(json!({"pattern":"*"}));
    assert!(exec_local(&st, false, "glob", &args, "*").await.is_some());
}

/// Lot 38 : l'outil criteria du mode Code, rejoué contre le journal du Go
/// (`internal/loki/parity_criteria_test.go` → `parity_tools/criteria.json`) :
/// résultat de chaque appel, critères rangés après coup (scellés), et la
/// liste poussée à l'écran quand elle change.
#[tokio::test]
async fn criteria_comme_le_go() {
    use loki_rs::chat::crittool::{CRIT_MAX_COUNT, exec};
    let fx: Value =
        serde_json::from_str(include_str!("parity_tools/criteria.json")).expect("journal");
    let t = tempfile::tempdir().unwrap();
    let (_f, go, _stop) = common::fakego::start(t.path().into()).await;
    let backend = go.backend.lock().unwrap().unwrap();
    let store = loki_rs::store::Store::new(backend, common::fakego::JETON.into());
    assert_eq!(CRIT_MAX_COUNT, 20);
    let conv = "c1";
    for st in fx.as_array().unwrap() {
        let name = st["name"].as_str().unwrap();
        if let Some(put) = st["put"].as_str() {
            store
                .put_seal("chat", &format!("crit:{conv}"), put.as_bytes().to_vec())
                .await
                .unwrap();
        }
        let args = st["args"].as_object().cloned().unwrap_or_default();
        let verifier = st["verifier"].as_bool() == Some(true);
        let before = store
            .get_open("chat", &format!("crit:{conv}"))
            .await
            .unwrap();
        let out = exec(&store, conv, &args, verifier).await;
        assert_eq!(out.result, st["out"].as_str().unwrap(), "{name}");
        let raw = store
            .get_open("chat", &format!("crit:{conv}"))
            .await
            .unwrap();
        let stored = raw
            .as_deref()
            .filter(|b| !b.is_empty())
            .map(|b| serde_json::from_slice::<Value>(b).unwrap())
            .unwrap_or(Value::Null);
        assert_eq!(stored, st["stored"], "{name}");
        // Scellés comme le contenu des discussions.
        if let Some(sealed) = go.get("chat", &format!("crit:{conv}")) {
            assert!(sealed.starts_with("SCELLE:"), "{name} : {sealed}");
        }
        // L'écran reçoit la liste quand elle change (vide : effacée).
        match &out.notify {
            Some(list) => {
                let want = if stored.is_null() {
                    json!([])
                } else {
                    stored.clone()
                };
                assert_eq!(json!(list), want, "{name}");
            }
            None => assert_eq!(raw, before, "{name} : rien ne change sans notification"),
        }
    }
}
