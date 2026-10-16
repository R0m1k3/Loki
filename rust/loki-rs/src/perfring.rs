//! L'anneau de télémétrie des complétions (lot 39) — port de `perf_log.go`,
//! tenu par la façade quand elle tient la conversation.
//!
//! Chaque complétion qui passe par le moteur y est rangée : celles des tours
//! de la façade directement, celles que le Go fait encore (sous-agents,
//! tâches IA, bench, compaction de ses tours, clients externes de /v1) par
//! `/internal/facade/v1/perf/record`. L'anneau calcule la perte de cache
//! d'une complétion contre la précédente de la même discussion et de la même
//! nature, nomme ce qui s'est intercalé, et nourrit `/api/perf/summary` et
//! les poids de l'optimiseur. Des identifiants et des compteurs, jamais de
//! texte.

use std::collections::{BTreeMap, BTreeSet, HashMap};
use std::path::PathBuf;
use std::sync::Mutex;

use serde::{Deserialize, Serialize};
use serde_json::Value;

use crate::bench::ser_go_f64;

/// `perfRingMax` : ~1 Mio en mémoire au pire.
pub const RING_MAX: usize = 5000;
/// `perfConvMax` : conversations suivies pour le calcul de la perte.
pub const CONV_MAX: usize = 128;
/// `perfBetween` : entrées remontées au plus pour nommer ce qui s'est
/// intercalé.
pub const BETWEEN: usize = 500;

fn is_zero_i(v: &i64) -> bool {
    *v == 0
}

fn is_zero_f(v: &f64) -> bool {
    *v == 0.0
}

/// `perfRec` : une complétion (mêmes champs JSON que le Go).
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct PerfRec {
    /// `time.Time` du Go (RFC 3339) ; vide ou nul : posé à l'enregistrement.
    #[serde(default)]
    pub at: String,
    #[serde(default)]
    pub seq: u64,
    #[serde(default)]
    pub kind: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub conv: String,
    #[serde(default)]
    pub iter: i64,
    #[serde(default)]
    pub complete: bool,
    /// usage.prompt_tokens : la taille du prompt entier.
    #[serde(default, skip_serializing_if = "is_zero_i")]
    pub total: i64,
    /// cache_n ; absent si inconnu.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cached: Option<i64>,
    /// prompt_n : les jetons réellement recalculés.
    #[serde(default, skip_serializing_if = "is_zero_i")]
    pub new: i64,
    #[serde(
        default,
        skip_serializing_if = "is_zero_f",
        serialize_with = "ser_go_f64"
    )]
    pub pp_ms: f64,
    #[serde(
        default,
        skip_serializing_if = "is_zero_f",
        serialize_with = "ser_go_f64"
    )]
    pub pp_tps: f64,
    #[serde(default, rename = "gen", skip_serializing_if = "is_zero_i")]
    pub gen_tokens: i64,
    #[serde(
        default,
        skip_serializing_if = "is_zero_f",
        serialize_with = "ser_go_f64"
    )]
    pub tg_tps: f64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub draft_n: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub draft_accepted: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub ttft_ms: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub lost: Option<i64>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub lost_after: String,
}

/// `time.Time` nul du Go, tel que l'encode son JSON.
const GO_ZERO_TIME: &str = "0001-01-01T00:00:00Z";

/// `perfConvState` : la dernière complétion terminée d'une conversation (et
/// d'une nature).
#[derive(Clone, Copy, Debug)]
struct ConvState {
    seq: u64,
    n: usize,
    hash: u64,
    total: i64,
}

#[derive(Default)]
struct Inner {
    seq: u64,
    ring: Vec<PerfRec>,
    /// Prochaine case à écraser quand l'anneau est plein.
    head: usize,
    convs: HashMap<String, ConvState>,
}

/// `perfStore`.
pub struct PerfRing {
    inner: Mutex<Inner>,
    /// LOKI_HOME : le modèle du preset, pour le seuil d'alerte d'un hybride.
    pub home: PathBuf,
    /// L'heure (RFC 3339) — remplaçable en test.
    pub clock: fn() -> String,
}

/// Maintenant, comme `time.Now()` encodé par le Go.
pub fn now_rfc3339() -> String {
    crate::chat::tplprobe::rfc3339(std::time::SystemTime::now())
}

impl PerfRing {
    pub fn new(home: PathBuf) -> Self {
        PerfRing {
            inner: Mutex::new(Inner::default()),
            home,
            clock: now_rfc3339,
        }
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, Inner> {
        self.inner.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// `record` : range la complétion et calcule sa perte de cache
    ///
    /// `lost = max(0, total précédent − cache_n)`
    ///
    /// seulement quand le cache est connu, que la complétion est allée au
    /// bout, et que ses messages PROLONGENT strictement ceux de la précédente
    /// complétion de la même conversation et de la même nature. Ailleurs, la
    /// perte reste inconnue plutôt que fausse. `prefix` : empreintes cumulées
    /// des messages envoyés (`perfPrefix`).
    pub fn record(&self, mut r: PerfRec, prefix: &[u64]) -> PerfRec {
        let mut p = self.lock();
        p.seq += 1;
        r.seq = p.seq;
        if r.at.is_empty() || r.at == GO_ZERO_TIME {
            r.at = (self.clock)();
        }
        let key = format!("{}\0{}", r.kind, r.conv);
        if r.complete && !prefix.is_empty() {
            if let Some(prev) = p.convs.get(&key).copied()
                && let Some(cached) = r.cached
                && prev.total > 0
                && prev.n > 0
                && prefix.len() > prev.n
                && prefix[prev.n - 1] == prev.hash
            {
                let lost = (prev.total - cached).max(0);
                r.lost = Some(lost);
                if lost > 0 {
                    r.lost_after = p.between(prev.seq, &key);
                }
            }
            if r.total > 0 {
                p.convs.insert(
                    key,
                    ConvState {
                        seq: r.seq,
                        n: prefix.len(),
                        hash: prefix[prefix.len() - 1],
                        total: r.total,
                    },
                );
                p.evict_convs();
            }
        }
        if p.ring.len() < RING_MAX {
            p.ring.push(r.clone());
        } else {
            let h = p.head;
            p.ring[h] = r.clone();
            p.head = (h + 1) % RING_MAX;
        }
        drop(p);
        if perf_log_on() {
            eprintln!("{}", perf_line(&r));
        }
        r
    }

    /// `snapshot` : copie de l'anneau, de la plus ancienne à la plus récente.
    pub fn snapshot(&self) -> Vec<PerfRec> {
        let p = self.lock();
        let mut out = Vec::with_capacity(p.ring.len());
        out.extend_from_slice(&p.ring[p.head..]);
        out.extend_from_slice(&p.ring[..p.head]);
        out
    }

    /// Conversations suivies (tests).
    pub fn convs_len(&self) -> usize {
        self.lock().convs.len()
    }

    /// `perfLostAlertAt` du preset actif `cfg` (le modèle résolu comme au
    /// lancement : dossiers rangés, `LOKI_MODEL_DIRS`).
    pub fn lost_alert_at(&self, cfg: &HashMap<String, String>, stored_dirs: &[String]) -> i64 {
        lost_alert_at(cfg, &|model| {
            let env = std::env::var(crate::gguf::MODEL_DIRS_ENV).ok();
            let p = crate::gguf::resolve_serve_model_path(
                model,
                &self.home,
                stored_dirs,
                env.as_deref(),
            )
            .ok()?;
            let g = crate::gguf::gguf_meta(&p).ok()?;
            Some(crate::serve::ckpt::gguf_hybrid(Some(&g)))
        })
    }
}

impl Inner {
    /// `between` : les natures des complétions passées par le moteur depuis
    /// `seq` (un sous-agent, un client externe…), cause probable d'une perte.
    fn between(&self, seq: u64, key: &str) -> String {
        let mut seen = BTreeSet::new();
        let n = self.ring.len();
        for i in 0..n.min(BETWEEN) {
            let idx = ((self.head as i64 - 1 - i as i64).rem_euclid(n as i64)) as usize;
            let e = &self.ring[idx];
            if e.seq <= seq {
                break;
            }
            if format!("{}\0{}", e.kind, e.conv) != key {
                seen.insert(e.kind.clone());
            }
        }
        seen.into_iter().collect::<Vec<_>>().join(",")
    }

    /// `evictConvs` : on oublie la conversation la plus ancienne.
    fn evict_convs(&mut self) {
        while self.convs.len() > CONV_MAX {
            let Some(old) = self
                .convs
                .iter()
                .min_by_key(|(_, s)| s.seq)
                .map(|(k, _)| k.clone())
            else {
                return;
            };
            self.convs.remove(&old);
        }
    }
}

/// `perfLogOn` : LOKI_PERF_LOG posée (une ligne par complétion sur stderr).
pub fn perf_log_on() -> bool {
    let v = std::env::var("LOKI_PERF_LOG").unwrap_or_default();
    let v = v.trim().to_lowercase();
    !v.is_empty() && !matches!(v.as_str(), "0" | "false" | "off" | "no")
}

/// `perfLine`.
pub fn perf_line(r: &PerfRec) -> String {
    let opt = |p: Option<i64>| p.map_or_else(|| "?".to_string(), |v| v.to_string());
    let draft = match r.draft_n {
        Some(n) => format!("{}/{n}", opt(r.draft_accepted)),
        None => "?".into(),
    };
    let mut lost = opt(r.lost);
    if !r.lost_after.is_empty() {
        lost.push_str(&format!("({})", r.lost_after));
    }
    let conv = if r.conv.is_empty() { "-" } else { &r.conv };
    format!(
        "[perf] kind={} conv={conv} iter={} total={} cached={} new={} pp_ms={:.0} pp_tps={:.1} gen={} tg_tps={:.1} draft={draft} ttft_ms={} lost={lost} complete={}",
        r.kind,
        r.iter,
        r.total,
        opt(r.cached),
        r.new,
        r.pp_ms,
        r.pp_tps,
        r.gen_tokens,
        r.tg_tps,
        opt(r.ttft_ms),
        r.complete
    )
}

/// `perfLostAlertAt` : la perte au-delà de laquelle l'interface la signale.
/// Sur un modèle hybride, le cache ne reprend qu'à un point de reprise :
/// perdre jusqu'à un espacement de points plus un micro-batch est normal.
/// `hybrid` : le modèle du preset est-il hybride (`None` : illisible) ?
pub fn lost_alert_at(cfg: &HashMap<String, String>, hybrid: &dyn Fn(&str) -> Option<bool>) -> i64 {
    const BASE: i64 = 1000;
    let get = |k: &str| cfg.get(k).map_or("", String::as_str);
    let extra = crate::models::split_args(get("EXTRA_ARGS"));
    let num = |vals: &[String]| -> i64 {
        vals.iter()
            .filter_map(|v| v.trim().parse::<i64>().ok())
            .find(|n| *n > 0)
            .unwrap_or(0)
    };
    let env = std::env::var("LLAMA_ARG_CHECKPOINT_MIN_SPACING_NT").unwrap_or_default();
    let mut step = num(&[
        crate::serve::flag_value(&extra, &["-cms", "--checkpoint-min-step"]),
        env,
        get("CKPT_MIN_STEP").to_string(),
    ]);
    let model = get("MODEL").trim();
    if step == 0 && !model.is_empty() && hybrid(model) == Some(true) {
        step = 2048;
    }
    if step == 0 {
        return BASE;
    }
    let mut ub = num(&[
        crate::serve::flag_value(&extra, &["-ub", "--ubatch-size"]),
        get("UBATCH").to_string(),
    ]);
    if ub == 0 {
        ub = 512;
    }
    BASE.max(step + ub)
}

/// `perfAlert` : une perte au-delà du seuil, ou un gros prompt repris de
/// zéro alors que le cache est connu.
pub fn perf_alert(r: &PerfRec, threshold: i64) -> bool {
    if r.lost.is_some_and(|l| l > threshold) {
        return true;
    }
    r.cached == Some(0) && r.total >= 8 * threshold
}

// ---------------------------------------------------------------------------
// /api/perf/summary
// ---------------------------------------------------------------------------

fn ser_opt_f64<S: serde::Serializer>(v: &Option<f64>, s: S) -> Result<S::Ok, S::Error> {
    match v {
        Some(f) => ser_go_f64(f, s),
        None => s.serialize_none(),
    }
}

/// `perfKindSum`.
#[derive(Clone, Debug, Default, PartialEq, Serialize)]
pub struct KindSum {
    pub completions: i64,
    pub incomplete: i64,
    /// Premières complétions (iter 0) : un tour, un sous-agent…
    pub turns: i64,
    pub new_tokens: i64,
    #[serde(serialize_with = "ser_go_f64")]
    pub prefill_sec: f64,
    #[serde(skip_serializing_if = "is_zero_f", serialize_with = "ser_go_f64")]
    pub new_tokens_per_turn: f64,
    #[serde(skip_serializing_if = "is_zero_f", serialize_with = "ser_go_f64")]
    pub prefill_sec_per_turn: f64,
    pub lost_events: i64,
    pub lost_tokens: i64,
    #[serde(
        skip_serializing_if = "Option::is_none",
        serialize_with = "ser_opt_f64"
    )]
    pub ttft_median_ms: Option<f64>,
    #[serde(skip_serializing_if = "is_zero_i")]
    pub draft_n: i64,
    #[serde(skip_serializing_if = "is_zero_i")]
    pub draft_accepted: i64,
    #[serde(
        skip_serializing_if = "Option::is_none",
        serialize_with = "ser_opt_f64"
    )]
    pub draft_rate: Option<f64>,
}

/// `perfDepthSum`.
#[derive(Clone, Debug, Default, PartialEq, Serialize)]
pub struct DepthSum {
    pub bucket: String,
    pub n: i64,
    #[serde(
        skip_serializing_if = "Option::is_none",
        serialize_with = "ser_opt_f64"
    )]
    pub pp_median_tps: Option<f64>,
    #[serde(
        skip_serializing_if = "Option::is_none",
        serialize_with = "ser_opt_f64"
    )]
    pub tg_median_tps: Option<f64>,
}

/// `perfSummary`.
#[derive(Clone, Debug, Default, PartialEq, Serialize)]
pub struct Summary {
    pub entries: i64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub since: Option<String>,
    /// Σ cached / Σ total sur les complétions où les deux sont connus.
    #[serde(
        skip_serializing_if = "Option::is_none",
        serialize_with = "ser_opt_f64"
    )]
    pub cache_hit_ratio: Option<f64>,
    pub kinds: BTreeMap<String, KindSum>,
    pub depth: Vec<DepthSum>,
    pub draft_n: i64,
    pub draft_accepted: i64,
    #[serde(
        skip_serializing_if = "Option::is_none",
        serialize_with = "ser_opt_f64"
    )]
    pub draft_rate: Option<f64>,
    /// Dernier verdict de la sonde de gabarit, absent tant qu'aucune n'a
    /// tourné.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub template: Option<Value>,
}

/// `perfDepthBuckets`.
pub const DEPTH_BUCKETS: [(&str, i64); 4] = [
    ("0-8k", 8 << 10),
    ("8-16k", 16 << 10),
    ("16-32k", 32 << 10),
    ("32k+", i64::MAX),
];

/// `perfMedian`.
fn median(mut v: Vec<f64>) -> Option<f64> {
    if v.is_empty() {
        return None;
    }
    v.sort_by(f64::total_cmp);
    let n = v.len();
    Some(if n.is_multiple_of(2) {
        (v[n / 2 - 1] + v[n / 2]) / 2.0
    } else {
        v[n / 2]
    })
}

/// `perfSummarize`.
pub fn summarize(recs: &[PerfRec]) -> Summary {
    let mut s = Summary {
        entries: recs.len() as i64,
        since: recs.first().map(|r| r.at.clone()),
        ..Default::default()
    };
    let (mut sum_cached, mut sum_total) = (0i64, 0i64);
    let mut pp: Vec<Vec<f64>> = vec![Vec::new(); DEPTH_BUCKETS.len()];
    let mut tg: Vec<Vec<f64>> = vec![Vec::new(); DEPTH_BUCKETS.len()];
    let mut ttft: HashMap<String, Vec<f64>> = HashMap::new();
    for r in recs {
        let k = s.kinds.entry(r.kind.clone()).or_default();
        if !r.complete {
            k.incomplete += 1;
            continue;
        }
        k.completions += 1;
        if r.iter == 0 {
            k.turns += 1;
        }
        k.new_tokens += r.new;
        k.prefill_sec += r.pp_ms / 1000.0;
        if let Some(l) = r.lost.filter(|l| *l > 0) {
            k.lost_events += 1;
            k.lost_tokens += l;
        }
        if let Some(t) = r.ttft_ms {
            ttft.entry(r.kind.clone()).or_default().push(t as f64);
        }
        if let Some(c) = r.cached
            && r.total > 0
        {
            sum_cached += c.min(r.total);
            sum_total += r.total;
        }
        let depth = if r.total == 0 { r.new } else { r.total };
        for (i, (_, max)) in DEPTH_BUCKETS.iter().enumerate() {
            if depth < *max {
                // Débit de lecture : seulement sur un vrai prefill.
                if r.pp_tps > 0.0 && r.new >= 64 {
                    pp[i].push(r.pp_tps);
                }
                if r.tg_tps > 0.0 && r.gen_tokens > 1 {
                    tg[i].push(r.tg_tps);
                }
                break;
            }
        }
        if let Some(n) = r.draft_n {
            s.draft_n += n;
            k.draft_n += n;
            if let Some(a) = r.draft_accepted {
                s.draft_accepted += a;
                k.draft_accepted += a;
            }
        }
    }
    for (kind, k) in s.kinds.iter_mut() {
        if k.turns > 0 {
            k.new_tokens_per_turn = k.new_tokens as f64 / k.turns as f64;
            k.prefill_sec_per_turn = k.prefill_sec / k.turns as f64;
        }
        k.ttft_median_ms = median(ttft.remove(kind).unwrap_or_default());
        if k.draft_n > 0 {
            k.draft_rate = Some(k.draft_accepted as f64 / k.draft_n as f64);
        }
    }
    if sum_total > 0 {
        s.cache_hit_ratio = Some(sum_cached as f64 / sum_total as f64);
    }
    if s.draft_n > 0 {
        s.draft_rate = Some(s.draft_accepted as f64 / s.draft_n as f64);
    }
    for (i, (name, _)) in DEPTH_BUCKETS.iter().enumerate() {
        s.depth.push(DepthSum {
            bucket: name.to_string(),
            n: pp[i].len().max(tg[i].len()) as i64,
            pp_median_tps: median(std::mem::take(&mut pp[i])),
            tg_median_tps: median(std::mem::take(&mut tg[i])),
        });
    }
    s
}

#[cfg(test)]
mod tests;
