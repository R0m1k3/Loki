//! Télémétrie des complétions de la façade (lot 29). Toutes rejoignent
//! l'anneau du Go (`perf_log.go`) par `/internal/v1/turn/perf` : le Go les
//! range, calcule la perte de cache contre la précédente de la même
//! discussion et l'alerte au seuil du preset actif. `/api/perf/summary`
//! reste au Go.
//!
//! Ce module porte ce que la façade relève elle-même : la réponse entière
//! d'un résumé de compaction (`perfRecFromWire`), et l'envoi au Go. Des
//! identifiants et des compteurs, jamais de texte. Rien ne retourne au
//! moteur.

use serde_json::{Value, json};

use super::turn::{PerfMarks, PerfRec};
use super::types::StatsEvent;
use crate::store::Store;

/// Nature d'une complétion.
pub const KIND_MAIN: &str = "main";
pub const KIND_VERIFY: &str = "verify";
pub const KIND_COMPACT: &str = "compact";

/// `perfRecFromWire` : une réponse NON streamée (compaction), lue d'un
/// bloc, au format des stats d'une complétion streamée. Jamais d'échec : un
/// compteur illisible reste inconnu. Rend aussi si la complétion est
/// complète (un usage, ou des timings).
pub fn wire_stats(raw: &[u8]) -> (bool, StatsEvent) {
    let mut s = StatsEvent::default();
    // Seule la première valeur JSON compte (`json.Decoder`).
    let v = serde_json::Deserializer::from_slice(raw)
        .into_iter::<Value>()
        .next()
        .and_then(Result::ok)
        .unwrap_or(Value::Null);
    // Une valeur non nulle d'un autre type (chaîne, nombre) alloue quand
    // même la structure côté Go avant l'erreur de type : présente, vide.
    let has_timings = v.get("timings").is_some_and(|t| !t.is_null());
    if let Some(t) = v.get("timings").filter(|t| t.is_object()) {
        let n = |k: &str| perf_num(t.get(k));
        s.prompt_tokens = n("prompt_n").unwrap_or(0.0) as i64;
        s.prompt_ms = n("prompt_ms").unwrap_or(0.0);
        s.prompt_per_second = n("prompt_per_second").unwrap_or(0.0);
        s.gen_tokens = n("predicted_n").unwrap_or(0.0) as i64;
        s.gen_per_second = n("predicted_per_second").unwrap_or(0.0);
        s.cache_tokens = n("cache_n").map(|f| f as i64);
        s.draft_n = n("draft_n").map(|f| f as i64);
        s.draft_accepted = n("draft_n_accepted").map(|f| f as i64);
    }
    let usage = v.get("usage").filter(|u| u.is_object());
    if let Some(u) = usage {
        s.prompt_tokens_total = perf_num(u.get("prompt_tokens")).unwrap_or(0.0) as i64;
        if s.gen_tokens == 0 {
            s.gen_tokens = perf_num(u.get("completion_tokens")).unwrap_or(0.0) as i64;
        }
    }
    if s.cache_tokens.is_none() {
        s.cache_tokens = usage
            .and_then(|u| u.get("prompt_tokens_details"))
            .filter(|d| d.is_object())
            .and_then(|d| perf_num(d.get("cached_tokens")))
            .map(|f| f as i64);
    }
    (s.prompt_tokens_total > 0 || has_timings, s)
}

/// `perfNum` : un nombre (ou une chaîne qui en contient un), fini, positif
/// et < 1e12 ; sinon inconnu.
fn perf_num(v: Option<&Value>) -> Option<f64> {
    let f = match v? {
        Value::Number(n) => n.as_f64()?,
        Value::String(s) => s.parse::<f64>().ok()?,
        _ => return None,
    };
    (f.is_finite() && (0.0..1e12).contains(&f)).then_some(f)
}

/// Range la complétion dans l'anneau du Go et rend ce qu'il en tire. Le Go
/// injoignable : rien (la télémétrie ne fait jamais échouer un tour).
pub async fn record(store: &Store, kind: &str, conv: &str, rec: &PerfRec) -> PerfMarks {
    let body = json!({
        "kind": kind,
        "conv": conv,
        "iter": rec.iter,
        "complete": rec.complete,
        "external": rec.external,
        "stats": rec.stats,
        "prefix": rec.prefix,
    });
    match store
        .internal_json(hyper::Method::POST, "/internal/v1/turn/perf", Some(&body))
        .await
    {
        Ok(v) => PerfMarks {
            kind: v["kind"].as_str().unwrap_or("").to_string(),
            lost: v["lost"].as_i64(),
            lost_after: v["lost_after"].as_str().unwrap_or("").to_string(),
            lost_alert: v["lost_alert"].as_bool().unwrap_or(false),
        },
        Err(e) => {
            eprintln!("[loki-rs] télémétrie : {e}");
            PerfMarks::default()
        }
    }
}

#[cfg(test)]
mod tests {
    use super::super::turn::perf_prefix;
    use super::super::types::Message;
    use super::*;

    #[test]
    fn releve_d_une_reponse_entiere() {
        let (complete, s) = wire_stats(
            br#"{"timings":{"prompt_n":"120","prompt_ms":50.5,"prompt_per_second":2376,"predicted_n":30,"predicted_per_second":40,"cache_n":900,"draft_n":-1},"usage":{"prompt_tokens":1020,"completion_tokens":31}} parasite"#,
        );
        assert!(complete);
        assert_eq!(
            s,
            StatsEvent {
                prompt_tokens_total: 1020,
                cache_tokens: Some(900),
                prompt_tokens: 120,
                prompt_ms: 50.5,
                prompt_per_second: 2376.0,
                gen_tokens: 30,
                gen_per_second: 40.0,
                ..Default::default()
            }
        );
        // Sans timings : le cache d'OpenAI, les jetons générés de l'usage.
        let (complete, s) = wire_stats(
            br#"{"usage":{"prompt_tokens":10,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":4}}}"#,
        );
        assert_eq!(
            (
                complete,
                s.prompt_tokens_total,
                s.gen_tokens,
                s.cache_tokens
            ),
            (true, 10, 3, Some(4))
        );
        // Rien de lisible : incomplète, tout inconnu.
        let (complete, s) = wire_stats(b"pas du json");
        assert!(!complete);
        assert_eq!(s.cache_tokens, None);
        let (complete, s) = wire_stats(br#"{"timings":null,"usage":{"prompt_tokens":1e13}}"#);
        assert_eq!((complete, s.prompt_tokens_total), (false, 0));
        // Des timings d'un autre type : présents mais vides, comme le Go.
        let (complete, s) = wire_stats(br#"{"timings":"x"}"#);
        assert_eq!((complete, s.prompt_tokens), (true, 0));
    }

    #[test]
    fn empreintes_cumulees() {
        let m = |c: &str| Message {
            role: "user".into(),
            content: Some(json!(c)),
            ..Default::default()
        };
        let a = perf_prefix(&[m("a"), m("b")]);
        let b = perf_prefix(&[m("a"), m("b"), m("c")]);
        assert_eq!(a.len(), 2);
        assert_eq!(b[..2], a[..]);
        assert_ne!(perf_prefix(&[m("x"), m("b")])[1], a[1]);
        assert!(perf_prefix(&[]).is_empty());
        // FNV-1a de référence : le JSON du message, puis l'octet nul.
        let mut h = 0xcbf2_9ce4_8422_2325u64;
        for c in serde_json::to_vec(&m("a")).unwrap().into_iter().chain([0]) {
            h = (h ^ u64::from(c)).wrapping_mul(0x0000_0100_0000_01b3);
        }
        assert_eq!(perf_prefix(&[m("a")]), vec![h]);
        let mut h = 0xcbf2_9ce4_8422_2325u64;
        for c in *b"a" {
            h = (h ^ u64::from(c)).wrapping_mul(0x0000_0100_0000_01b3);
        }
        assert_eq!(h, 0xaf63_dc4c_8601_ec8c);
    }
}
