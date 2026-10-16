//! Télémétrie des complétions de la façade (lot 29) — port de la partie
//! « relevé » de `perf_log.go` : `perfRecFromStats`, `perfRecFromWire`,
//! `perfPrefix`. L'anneau, le calcul de la perte de cache et son seuil
//! d'alerte restent au Go (`/internal/v1/perf/record`) : les complétions de
//! la façade rejoignent celles du Go dans `/api/perf/summary`.
//!
//! Des identifiants et des compteurs, jamais de texte. Rien ne retourne au
//! moteur.

use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use super::types::{Message, StatsEvent};
use crate::store::Store;

/// Nature d'une complétion.
pub const KIND_MAIN: &str = "main";
pub const KIND_VERIFY: &str = "verify";
pub const KIND_COMPACT: &str = "compact";

fn is_zero_i(v: &i64) -> bool {
    *v == 0
}

fn is_zero_f(v: &f64) -> bool {
    *v == 0.0
}

/// `perfRec` (sans `at` ni `seq`, posés par le Go).
#[derive(Clone, Debug, Default, PartialEq, Serialize)]
pub struct PerfRec {
    pub kind: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub conv: String,
    pub iter: i64,
    pub complete: bool,
    /// usage.prompt_tokens : la taille du prompt entier.
    #[serde(skip_serializing_if = "is_zero_i")]
    pub total: i64,
    /// cache_n ; `None` : inconnu.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cached: Option<i64>,
    /// prompt_n : les jetons réellement recalculés.
    #[serde(skip_serializing_if = "is_zero_i")]
    pub new: i64,
    #[serde(skip_serializing_if = "is_zero_f")]
    pub pp_ms: f64,
    #[serde(skip_serializing_if = "is_zero_f")]
    pub pp_tps: f64,
    #[serde(rename = "gen", skip_serializing_if = "is_zero_i")]
    pub generated: i64,
    #[serde(skip_serializing_if = "is_zero_f")]
    pub tg_tps: f64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub draft_n: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub draft_accepted: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub ttft_ms: Option<i64>,
}

impl PerfRec {
    /// `perfRecFromStats` : la complétion telle que la boucle l'a vue.
    pub fn from_stats(kind: &str, conv: &str, iter: i64, complete: bool, s: &StatsEvent) -> Self {
        PerfRec {
            kind: kind.into(),
            conv: conv.into(),
            iter,
            complete,
            total: s.prompt_tokens_total,
            cached: s.cache_tokens,
            new: s.prompt_tokens,
            pp_ms: s.prompt_ms,
            pp_tps: s.prompt_per_second,
            generated: s.gen_tokens,
            tg_tps: s.gen_per_second,
            draft_n: s.draft_n,
            draft_accepted: s.draft_accepted,
            ttft_ms: s.ttft_ms,
        }
    }

    /// `perfRecFromWire` : une réponse NON streamée (compaction), lue d'un
    /// bloc. Jamais d'échec : un compteur illisible reste inconnu.
    pub fn from_wire(kind: &str, conv: &str, raw: &[u8]) -> Self {
        let mut r = PerfRec {
            kind: kind.into(),
            conv: conv.into(),
            ..Default::default()
        };
        // Seule la première valeur JSON compte (`json.Decoder`).
        let v = serde_json::Deserializer::from_slice(raw)
            .into_iter::<Value>()
            .next()
            .and_then(Result::ok)
            .unwrap_or(Value::Null);
        // Une valeur non nulle d'un autre type (chaîne, nombre) alloue quand
        // même la structure côté Go avant l'erreur de type : présente, vide.
        let has_timings = v.get("timings").is_some_and(|t| !t.is_null());
        let timings = v.get("timings").filter(|t| t.is_object());
        if let Some(t) = timings {
            let n = |k: &str| perf_num(t.get(k));
            r.new = n("prompt_n").unwrap_or(0.0) as i64;
            r.pp_ms = n("prompt_ms").unwrap_or(0.0);
            r.pp_tps = n("prompt_per_second").unwrap_or(0.0);
            r.generated = n("predicted_n").unwrap_or(0.0) as i64;
            r.tg_tps = n("predicted_per_second").unwrap_or(0.0);
            r.cached = n("cache_n").map(|f| f as i64);
            r.draft_n = n("draft_n").map(|f| f as i64);
            r.draft_accepted = n("draft_n_accepted").map(|f| f as i64);
        }
        let usage = v.get("usage").filter(|u| u.is_object());
        if let Some(u) = usage {
            r.total = perf_num(u.get("prompt_tokens")).unwrap_or(0.0) as i64;
            if r.generated == 0 {
                r.generated = perf_num(u.get("completion_tokens")).unwrap_or(0.0) as i64;
            }
        }
        if r.cached.is_none() {
            r.cached = usage
                .and_then(|u| u.get("prompt_tokens_details"))
                .filter(|d| d.is_object())
                .and_then(|d| perf_num(d.get("cached_tokens")))
                .map(|f| f as i64);
        }
        r.complete = r.total > 0 || has_timings;
        r
    }
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

/// FNV-1a 64 bits.
fn fnv1a(h: &mut u64, b: &[u8]) {
    for &c in b {
        *h ^= u64::from(c);
        *h = h.wrapping_mul(0x0000_0100_0000_01b3);
    }
}

/// `perfPrefix` : empreintes cumulées des messages envoyés (l'empreinte i
/// couvre les messages 0..i). Une requête PROLONGE la précédente de la même
/// discussion quand l'empreinte de la précédente est un de ses préfixes.
pub fn prefix(msgs: &[Message]) -> Vec<u64> {
    let mut h = 0xcbf2_9ce4_8422_2325u64;
    msgs.iter()
        .map(|m| {
            fnv1a(&mut h, &serde_json::to_vec(m).unwrap_or_default());
            fnv1a(&mut h, &[0]);
            h
        })
        .collect()
}

/// Ce que le Go a tiré de la complétion rangée.
#[derive(Clone, Debug, Default, PartialEq, Deserialize)]
pub struct PerfNoted {
    #[serde(default)]
    pub lost: Option<i64>,
    #[serde(default)]
    pub lost_after: String,
    #[serde(default)]
    pub alert: bool,
}

/// Range la complétion dans l'anneau du Go. Le Go injoignable : rien (la
/// télémétrie ne fait jamais échouer un tour).
pub async fn record(store: &Store, rec: &PerfRec, prefix: &[u64], external: bool) -> PerfNoted {
    match store
        .internal_post(
            "/internal/v1/perf/record",
            &json!({ "rec": rec, "prefix": prefix, "external": external }),
        )
        .await
    {
        Ok((code, v)) if code.is_success() => serde_json::from_value(v).unwrap_or_default(),
        _ => PerfNoted::default(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn releve_d_une_reponse_entiere() {
        let r = PerfRec::from_wire(
            KIND_COMPACT,
            "c1",
            br#"{"timings":{"prompt_n":"120","prompt_ms":50.5,"prompt_per_second":2376,"predicted_n":30,"predicted_per_second":40,"cache_n":900,"draft_n":-1},"usage":{"prompt_tokens":1020,"completion_tokens":31}} parasite"#,
        );
        assert_eq!(
            r,
            PerfRec {
                kind: "compact".into(),
                conv: "c1".into(),
                complete: true,
                total: 1020,
                cached: Some(900),
                new: 120,
                pp_ms: 50.5,
                pp_tps: 2376.0,
                generated: 30,
                tg_tps: 40.0,
                ..Default::default()
            }
        );
        // Sans timings : le cache d'OpenAI, les jetons générés de l'usage.
        let r = PerfRec::from_wire(
            KIND_COMPACT,
            "",
            br#"{"usage":{"prompt_tokens":10,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":4}}}"#,
        );
        assert_eq!(
            (r.complete, r.total, r.generated, r.cached),
            (true, 10, 3, Some(4))
        );
        // Rien de lisible : incomplète, tout inconnu.
        let r = PerfRec::from_wire(KIND_COMPACT, "", b"pas du json");
        assert!(!r.complete);
        assert_eq!(r.cached, None);
        let r = PerfRec::from_wire(
            KIND_COMPACT,
            "",
            br#"{"timings":null,"usage":{"prompt_tokens":1e13}}"#,
        );
        assert_eq!((r.complete, r.total), (false, 0));
        // Des timings d'un autre type : présents mais vides, comme le Go.
        let r = PerfRec::from_wire(KIND_COMPACT, "", br#"{"timings":"x"}"#);
        assert_eq!((r.complete, r.new), (true, 0));
    }

    #[test]
    fn empreintes_cumulees() {
        let m = |c: &str| Message {
            role: "user".into(),
            content: Some(json!(c)),
            ..Default::default()
        };
        let a = prefix(&[m("a"), m("b")]);
        let b = prefix(&[m("a"), m("b"), m("c")]);
        assert_eq!(a.len(), 2);
        assert_eq!(b[..2], a[..]);
        assert_ne!(prefix(&[m("x"), m("b")])[1], a[1]);
        assert!(prefix(&[]).is_empty());
        // FNV-1a de référence : « a » seul.
        let mut h = 0xcbf2_9ce4_8422_2325u64;
        fnv1a(&mut h, b"a");
        assert_eq!(h, 0xaf63_dc4c_8601_ec8c);
    }

    #[test]
    fn releve_au_format_du_go() {
        let s = StatsEvent {
            prompt_tokens: 5,
            prompt_tokens_total: 100,
            cache_tokens: Some(95),
            gen_tokens: 7,
            gen_per_second: 20.5,
            ttft_ms: Some(30),
            ..Default::default()
        };
        let r = PerfRec::from_stats(KIND_MAIN, "c", 2, true, &s);
        assert_eq!(
            serde_json::to_value(&r).unwrap(),
            json!({ "kind": "main", "conv": "c", "iter": 2, "complete": true, "total": 100,
                    "cached": 95, "new": 5, "gen": 7, "tg_tps": 20.5, "ttft_ms": 30 })
        );
    }
}
