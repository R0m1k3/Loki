//! L'outil `criteria` du mode Code (lot 38) — port de `toolCriteria`
//! (`code_criteria.go`) : le modèle pose, met à jour, liste ou efface les
//! critères d'acceptation de la discussion (`chat/crit:<id>`, scellés comme
//! le contenu des discussions). Seule la passe de vérification peut marquer
//! un critère `passed`. Chaque changement part aussi à l'écran (delta
//! `criteria`, `critNotify`).

use serde_json::{Map, Value, json};

use super::persist::BK_CHAT;
use super::verify::{Criterion, crit_list, crit_render};
use crate::store::Store;

/// `critMaxCount`.
pub const CRIT_MAX_COUNT: usize = 20;

/// L'issue d'un appel : le texte rendu au modèle, et la liste à pousser à
/// l'écran quand elle a changé (vide : effacée).
#[derive(Debug, Default, PartialEq)]
pub struct CritOut {
    pub result: String,
    pub notify: Option<Vec<Criterion>>,
}

impl CritOut {
    fn text(s: impl Into<String>) -> Self {
        CritOut {
            result: s.into(),
            notify: None,
        }
    }
}

/// `strconv.Atoi` (signe accepté) ; 0 sinon, comme l'id ignoré du Go.
fn go_atoi(s: &str) -> i64 {
    let digits = s.strip_prefix(['+', '-']).unwrap_or(s);
    if digits.is_empty() || !digits.bytes().all(|c| c.is_ascii_digit()) {
        return 0;
    }
    s.parse().unwrap_or(0)
}

/// `strings.TrimSpace`.
fn go_trim(s: &str) -> &str {
    s.trim_matches(char::is_whitespace)
}

/// `critSave` : une liste vide supprime la clé ; une erreur est ignorée,
/// comme dans le Go.
async fn crit_save(store: &Store, conv: &str, list: &[Criterion]) {
    let key = format!("crit:{conv}");
    if list.is_empty() {
        let _ = store.del(BK_CHAT, &key).await;
        return;
    }
    let _ = store
        .put_seal(
            BK_CHAT,
            &key,
            crate::gojson::marshal(&list.to_vec()).into_bytes(),
        )
        .await;
}

/// `toolCriteria` : `allow_pass`, la passe de vérification seule.
pub async fn exec(
    store: &Store,
    conv: &str,
    args: &Map<String, Value>,
    allow_pass: bool,
) -> CritOut {
    let s = |k: &str| args.get(k).and_then(Value::as_str).unwrap_or("");
    let action = s("action");
    let mut list = crit_list(store, conv).await;
    match action {
        "list" | "" => {
            if list.is_empty() {
                return CritOut::text("[aucun critère]");
            }
            CritOut::text(crit_render(&list))
        }
        "add" => {
            let raw: &[Value] = args
                .get("texts")
                .and_then(Value::as_array)
                .map_or(&[], Vec::as_slice);
            let mut next = 1;
            for c in &list {
                if c.id >= next {
                    next = c.id + 1;
                }
            }
            let mut added = 0;
            for r in raw {
                let Some(t) = r.as_str().map(go_trim).filter(|t| !t.is_empty()) else {
                    continue;
                };
                if list.len() >= CRIT_MAX_COUNT {
                    break;
                }
                list.push(Criterion {
                    id: next,
                    text: t.to_string(),
                    status: "pending".into(),
                    note: String::new(),
                });
                next += 1;
                added += 1;
            }
            if added == 0 {
                return CritOut::text("[erreur] aucun critère fourni (texts)");
            }
            crit_save(store, conv, &list).await;
            CritOut {
                result: format!("[ok] {added} critère(s) ajouté(s)\n{}", crit_render(&list)),
                notify: Some(list),
            }
        }
        "set" => {
            // id numérique ou chaîne (« "2" », « "#2" »).
            let id = match args.get("id") {
                Some(Value::Number(n)) => n.as_f64().map_or(0, |v| v as i64),
                Some(Value::String(v)) => {
                    let t = go_trim(v);
                    go_atoi(t.strip_prefix('#').unwrap_or(t))
                }
                _ => 0,
            };
            let status = s("status");
            let note = s("note");
            if status == "passed" && !allow_pass {
                return CritOut::text(
                    "[refusé] Seule la passe de vérification peut marquer un critère passed. Termine ton implémentation ; la vérification suivra.",
                );
            }
            let Some(i) = list.iter().position(|c| c.id == id) else {
                return CritOut::text(format!("[erreur] critère #{id} inconnu"));
            };
            if !status.is_empty() {
                list[i].status = status.to_string();
            }
            if !note.is_empty() {
                list[i].note = go_trim(note).to_string();
            }
            crit_save(store, conv, &list).await;
            let result = format!("[ok] critère #{id} → {}", list[i].status);
            CritOut {
                result,
                notify: Some(list),
            }
        }
        "clear" => {
            crit_save(store, conv, &[]).await;
            CritOut {
                result: "[ok] critères effacés".into(),
                notify: Some(Vec::new()),
            }
        }
        a => CritOut::text(format!("[erreur] action inconnue : {a}")),
    }
}

/// Le delta `critNotify` : la liste (vide : `[]`) et l'heure (ms).
pub fn notify_delta(list: &[Criterion], ts_ms: i64) -> Map<String, Value> {
    let mut d = Map::new();
    d.insert("criteria".into(), json!(list));
    d.insert("ts".into(), json!(ts_ms));
    d
}

/// Maintenant, en ms Unix.
pub fn now_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| d.as_millis() as i64)
}
