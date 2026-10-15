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
