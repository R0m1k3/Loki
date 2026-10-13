//! Lecture du fichier SHA256SUMS publié avec chaque release (format
//! `sha256sum` : "<hex>  <nom>", ou "<hex> *<nom>" en mode binaire).

use std::collections::HashMap;

/// Associe chaque nom de fichier à son empreinte SHA-256 en hexadécimal
/// minuscule. Les lignes vides ou mal formées sont ignorées : une entrée
/// manquante fera échouer la vérification du fichier concerné, pas la lecture.
pub fn parse(text: &str) -> HashMap<String, String> {
    let mut out = HashMap::new();
    for line in text.lines() {
        let mut it = line.split_whitespace();
        let (Some(hash), Some(name), None) = (it.next(), it.next(), it.next()) else {
            continue;
        };
        if hash.len() != 64 || !hash.bytes().all(|b| b.is_ascii_hexdigit()) {
            continue;
        }
        out.insert(
            name.trim_start_matches('*').to_string(),
            hash.to_ascii_lowercase(),
        );
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    const A: &str = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";

    #[test]
    fn lit_les_deux_formats() {
        let up = A.to_ascii_uppercase();
        let text = format!("{A}  loki-linux\n{up} *loki-rs-linux\n");
        let m = parse(&text);
        assert_eq!(m.get("loki-linux").map(String::as_str), Some(A));
        assert_eq!(m.get("loki-rs-linux").map(String::as_str), Some(A));
    }

    #[test]
    fn ignore_les_lignes_cassees() {
        let text = format!("\nzz  loki-linux\n{A}\n{A}  a  b\n{A}  ok\n");
        let m = parse(&text);
        assert_eq!(m.len(), 1);
        assert!(m.contains_key("ok"));
    }
}
