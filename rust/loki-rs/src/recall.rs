//! Outils `recall` et `recall_search` de l'agent (lot 46) — port de
//! `toolRecall` / `toolRecallSearch` (`chat_recall.go`).
//!
//! La compaction archive verbatim les gros blocs qu'elle résume ; l'archive
//! reste au Go (numérotée par la séquence de sa base), qui sert les blocs
//! lisibles (`/internal/v1/recall/block`, `/internal/v1/recall/blocks`).
//! Ici : l'id tolérant, le score par mots-clés, la mise en forme.

use std::collections::BTreeSet;

use serde::Deserialize;
use serde_json::{Map, Value};

use crate::store::Store;

/// `recallSearchDefault`, `recallSearchMax`.
const SEARCH_DEFAULT: usize = 5;
const SEARCH_MAX: usize = 20;

/// `recallBlock`.
#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(default)]
pub struct Block {
    pub id: String,
    pub seq: u64,
    pub label: String,
    pub role: String,
    pub content: String,
}

/// Les outils de ce module.
pub fn is_tool(name: &str) -> bool {
    name == "recall" || name == "recall_search"
}

/// `recallIDToSeq` : « r7 », « R7 », « 7 », espaces tolérés.
pub fn id_to_seq(id: &str) -> Option<u64> {
    let s = id.trim();
    let s = s.strip_prefix('r').unwrap_or(s);
    let s = s.strip_prefix('R').unwrap_or(s);
    let s = s.trim();
    // `strconv.ParseUint(s, 10, 64)` : des chiffres seulement (ni signe).
    if s.is_empty() || !s.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    s.parse().ok()
}

/// `strings.ToLower` du Go : rune par rune, sans casse contextuelle.
pub fn go_lower(s: &str) -> String {
    s.chars()
        .map(|c| {
            let mut l = c.to_lowercase();
            match (l.next(), l.next()) {
                (Some(a), None) => a,
                // « İ » (U+0130), seule minuscule de deux runes : `i` en Go.
                _ => 'i',
            }
        })
        .collect()
}

/// `recallTokens` : mots d'au moins deux runes, coupés sur tout ce qui n'est
/// ni a–z, ni 0–9, ni à–ÿ.
pub fn tokens(s: &str) -> BTreeSet<String> {
    go_lower(s)
        .split(|c: char| {
            !(c.is_ascii_lowercase() || c.is_ascii_digit() || ('à'..='ÿ').contains(&c))
        })
        .filter(|w| w.chars().count() >= 2)
        .map(str::to_string)
        .collect()
}

/// `recallSearch` : score = termes présents, le libellé comptant double ; à
/// égalité, le plus récent d'abord.
pub fn search(blocks: Vec<Block>, query: &str, limit: i64) -> Vec<Block> {
    let terms = tokens(query);
    if terms.is_empty() {
        return Vec::new();
    }
    let limit = if limit <= 0 {
        SEARCH_DEFAULT
    } else {
        (limit as usize).min(SEARCH_MAX)
    };
    let mut hits: Vec<(Block, usize)> = blocks
        .into_iter()
        .filter_map(|b| {
            let label = go_lower(&b.label);
            let body = go_lower(&b.content);
            let score: usize = terms
                .iter()
                .map(|t| {
                    2 * usize::from(label.contains(t.as_str()))
                        + usize::from(body.contains(t.as_str()))
                })
                .sum();
            (score > 0).then_some((b, score))
        })
        .collect();
    hits.sort_by(|a, b| b.1.cmp(&a.1).then(b.0.seq.cmp(&a.0.seq)));
    hits.truncate(limit);
    hits.into_iter().map(|h| h.0).collect()
}

/// `recallSnippet` : une ligne, bornée à `n` runes.
pub fn snippet(s: &str, n: usize) -> String {
    let s = s.split_whitespace().collect::<Vec<_>>().join(" ");
    if s.chars().count() > n {
        let head: String = s.chars().take(n).collect();
        return format!("{}…", head.trim());
    }
    s
}

/// `toolRecall`, le bloc lu (`None` : absent ou illisible).
pub fn recall_text(args: &Map<String, Value>, blk: Option<Block>) -> String {
    let id = args.get("id").and_then(Value::as_str).unwrap_or("");
    let Some(b) = blk else {
        return format!(
            "[erreur] bloc {} introuvable — vérifie l'id, ou utilise recall_search pour le retrouver par mots-clés",
            crate::gojson::quote(id)
        );
    };
    format!("[{} — {}, rappelé verbatim]\n{}", b.id, b.label, b.content)
}

/// `toolRecallSearch`, sur les résultats déjà triés.
pub fn search_text(hits: &[Block]) -> String {
    if hits.is_empty() {
        return "[aucun bloc archivé ne correspond]".into();
    }
    let mut out = String::from("Blocs trouvés (rappelle le bon avec recall(id)) :\n");
    for h in hits {
        out += &format!("- {} — {}\n  {}\n", h.id, h.label, snippet(&h.content, 160));
    }
    out.trim_end_matches('\n').to_string()
}

/// `limit` du Go : `args["limit"].(float64)` tronqué.
fn arg_limit(args: &Map<String, Value>) -> i64 {
    args.get("limit")
        .and_then(Value::as_f64)
        .map_or(0, |f| f as i64)
}

/// Exécute `recall` ou `recall_search` sur l'archive du Go. `None` : le Go
/// n'a pas répondu (l'appel passe alors par lui).
pub async fn exec(store: &Store, name: &str, args: &Map<String, Value>) -> Option<String> {
    match name {
        "recall" => {
            let id = args.get("id").and_then(Value::as_str).unwrap_or("");
            if id.trim().is_empty() {
                return Some("[erreur] id manquant".into());
            }
            let Some(seq) = id_to_seq(id) else {
                return Some(recall_text(args, None));
            };
            let (status, v) = store
                .internal_get(&format!("/internal/v1/recall/block?seq={seq}"))
                .await
                .ok()?;
            let blk = match status.as_u16() {
                200 => serde_json::from_value(v).ok(),
                404 => None,
                _ => return None,
            };
            Some(recall_text(args, blk))
        }
        "recall_search" => {
            let q = args.get("query").and_then(Value::as_str).unwrap_or("");
            if q.trim().is_empty() {
                return Some("[erreur] query manquante".into());
            }
            if tokens(q).is_empty() {
                return Some(search_text(&[]));
            }
            let (status, v) = store
                .internal_get("/internal/v1/recall/blocks")
                .await
                .ok()?;
            if status.as_u16() != 200 {
                return None;
            }
            let blocks: Vec<Block> = serde_json::from_value(v).ok()?;
            Some(search_text(&search(blocks, q, arg_limit(args))))
        }
        _ => None,
    }
}

#[cfg(test)]
mod tests;
