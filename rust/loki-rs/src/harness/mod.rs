//! Harnais de code externes du mode Code.
//!
//! Aujourd'hui un seul : Claude Code ([`claude`]), lancé par la façade à la
//! place de la boucle d'outils de Loki quand la discussion l'a choisi
//! (`/api/chat/harness`). Il parle au modèle local par le relais `/v1` de
//! Loki ([`crate::oai`]) ; [`install`] l'installe dans `LOKI_HOME` et le met
//! à jour au démarrage de la façade.

use std::sync::OnceLock;

pub mod claude;
pub mod install;

/// En-tête par lequel Claude Code se fait reconnaître du relais `/v1` : ses
/// requêtes vont sur le slot de la discussion (0), pas sur le second slot des
/// clients externes. Il n'ouvre aucun droit : la clé Bearer reste contrôlée.
pub const HEADER: &str = "x-loki-harness";

/// Secret de [`HEADER`] : 32 octets tirés une fois par processus, en
/// hexadécimal, jamais écrits sur disque.
pub fn secret() -> &'static str {
    static S: OnceLock<String> = OnceLock::new();
    S.get_or_init(|| {
        let mut b = [0u8; 32];
        if getrandom::fill(&mut b).is_err() {
            // Sans aléa, pas de secret devinable : l'en-tête ne correspondra
            // jamais (chaîne vide refusée par `matches`).
            return String::new();
        }
        hex::encode(b)
    })
}

/// L'en-tête reçu est-il celui du harnais ?
pub fn matches(got: &str) -> bool {
    let s = secret();
    !s.is_empty() && crate::auth::constant_time_eq(got.trim().as_bytes(), s.as_bytes())
}

/// Clé de `BK_CHAT` : le harnais d'une discussion (`claude`, absente = Loki).
pub fn harness_key(conv: &str) -> String {
    format!("harness:{conv}")
}

/// Clé de `BK_CHAT` : la dernière session Claude Code d'une discussion.
pub fn sid_key(conv: &str) -> String {
    format!("harness_sid:{conv}")
}

/// `LOKI_HARNESS_AUTO_UPDATE` : la mise à jour automatique (oui par défaut).
pub fn auto_update() -> bool {
    std::env::var("LOKI_HARNESS_AUTO_UPDATE")
        .map(|v| v.trim() != "0")
        .unwrap_or(true)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn secret_stable_et_reconnu() {
        let s = secret();
        assert_eq!(s.len(), 64);
        assert_eq!(s, secret());
        assert!(matches(s));
        assert!(matches(&format!(" {s} ")));
        assert!(!matches("faux"));
        assert!(!matches(""));
    }

    #[test]
    fn cles() {
        assert_eq!(harness_key("abc"), "harness:abc");
        assert_eq!(sid_key("abc"), "harness_sid:abc");
    }
}
