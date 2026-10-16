//! Tests unitaires de recall (lot 46) ; la parité avec le Go est dans
//! `tests/recall.rs`.

use super::*;

fn blk(seq: u64, label: &str, content: &str) -> Block {
    Block {
        id: format!("r{seq}"),
        seq,
        label: label.into(),
        role: "tool".into(),
        content: content.into(),
    }
}

#[test]
fn id_tolerant() {
    for s in ["r7", "R7", " r7 ", "7", "rR7", "r 7", "007"] {
        assert_eq!(id_to_seq(s), Some(7), "{s}");
    }
    for s in [
        "",
        "rr",
        "abc",
        "r-1",
        "+7",
        "r+7",
        "7a",
        "Rr7",
        "99999999999999999999",
    ] {
        assert_eq!(id_to_seq(s), None, "{s}");
    }
}

#[test]
fn mots_cles() {
    let t: Vec<String> = tokens("Le TRAIN de 14h12, quai n°3 — Été ÿ x")
        .into_iter()
        .collect();
    assert_eq!(t, ["14h12", "de", "le", "quai", "train", "été"]);
    assert_eq!(go_lower("İSTANBUL Σ"), "istanbul σ");
    assert!(tokens("a b ! ?").is_empty());
}

#[test]
fn score_et_ordre() {
    let blocks = vec![
        blk(1, "web_read: recette", "gratin dauphinois"),
        blk(2, "web_read: horaires", "le train part quai 3"),
        blk(3, "bash", "train train"),
        blk(4, "autre", "rien"),
    ];
    let ids: Vec<String> = search(blocks.clone(), "train horaires", 0)
        .into_iter()
        .map(|b| b.id)
        .collect();
    assert_eq!(ids, ["r2", "r3"]);
    // À égalité, le plus récent d'abord ; limite respectée.
    let ids: Vec<String> = search(blocks, "web_read", 1)
        .into_iter()
        .map(|b| b.id)
        .collect();
    assert_eq!(ids, ["r2"]);
}

#[test]
fn extraits() {
    assert_eq!(snippet("  a\n\tb   c ", 160), "a b c");
    assert_eq!(snippet("abcdef", 3), "abc…");
    assert_eq!(snippet("ab cdef", 3), "ab…");
}

#[test]
fn textes() {
    let mut args = Map::new();
    args.insert("id".into(), Value::from("r\"9"));
    assert_eq!(
        recall_text(&args, None),
        "[erreur] bloc \"r\\\"9\" introuvable — vérifie l'id, ou utilise recall_search pour le retrouver par mots-clés"
    );
    assert_eq!(
        recall_text(&args, Some(blk(9, "bash: ls", "a\nb"))),
        "[r9 — bash: ls, rappelé verbatim]\na\nb"
    );
    assert_eq!(search_text(&[]), "[aucun bloc archivé ne correspond]");
    assert_eq!(
        search_text(&[blk(2, "x", "y  z")]),
        "Blocs trouvés (rappelle le bon avec recall(id)) :\n- r2 — x\n  y z"
    );
}
