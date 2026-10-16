//! Tests unitaires du sous-agent (lot 47) ; la parité avec le Go est dans
//! `tests/lot6.rs` (`subagent_comme_le_go`).

use super::*;

fn args(v: Value) -> Map<String, Value> {
    serde_json::from_value(v).unwrap()
}

#[test]
fn refus_avant_le_moteur() {
    let e = |v, p| precheck(&args(v), p).unwrap_err();
    assert!(
        e(json!({"role": "verifier", "task": "x"}), "")
            .starts_with("[erreur] rôle inconnu : verifier")
    );
    assert!(e(json!({"role": 3, "task": "x"}), "").starts_with("[erreur] rôle inconnu :  ("));
    assert!(e(json!({"role": "planner", "task": " \n"}), "").contains("tâche vide"));
    // Le rôle et la tâche d'abord, la récursion ensuite.
    assert!(e(json!({"role": "x", "task": "y"}), "explorer").contains("rôle inconnu"));
    assert!(
        e(json!({"role": "explorer", "task": "y"}), "explorer")
            .contains("ne peut pas en appeler un autre")
    );
    assert_eq!(
        precheck(
            &args(json!({"role": " planner ", "task": " découpe "})),
            "builder"
        ),
        Ok(("planner".into(), "découpe".into()))
    );
    assert!(precheck(&args(json!({"role": "explorer", "task": "t"})), "").is_ok());
}

#[test]
fn rapports() {
    assert_eq!(report("explorer", "  vu  \n", None), "[explorer]\nvu");
    assert_eq!(
        report("planner", " ", None),
        "[erreur] le sous-agent planner n'a rien répondu"
    );
    assert_eq!(
        report("explorer", "x", Some("panne")),
        "[erreur] sous-agent explorer : panne"
    );
    let long = "é".repeat(MAX_OUTPUT + 1);
    let r = report("explorer", &long, None);
    assert!(r.ends_with("\n…[tronqué]"));
    let kept = r
        .trim_start_matches("[explorer]\n")
        .trim_end_matches("\n…[tronqué]");
    assert_eq!(kept.chars().count(), MAX_OUTPUT);
    let pile = "é".repeat(MAX_OUTPUT);
    assert_eq!(
        report("explorer", &pile, None),
        format!("[explorer]\n{pile}")
    );
}

#[test]
fn cadrages() {
    assert!(framing("planner").contains("criteria tool"));
    assert!(framing("explorer").contains("file:line"));
    assert_eq!(framing("code-reviewer"), framing("explorer"));
}
