//! Threads CPU de llama-server (`backend_serve_threads.go`) : sans réglage,
//! AUCUN drapeau — llama.cpp prend ses cœurs physiques. Un conteneur à
//! l'étroit (cpuset, quota CFS) est sondé : on impose alors -t.

use std::collections::BTreeSet;
use std::path::Path;

use super::{atoi, flag_value, has_any_flag, trim};

/// Ce que la sonde de conteneur a conclu (`cpuBudget`). `n` = 0 : rien.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct CpuBudget {
    pub n: i64,
    pub engine: i64,
    pub why: String,
}

/// `threadCount` : vide ou 0 = auto ; illisible ou négatif ignoré, en le
/// disant.
pub fn thread_count(key: &str, v: &str) -> (i64, String) {
    let v = trim(v);
    if v.is_empty() {
        return (0, String::new());
    }
    match atoi(v) {
        Some(n) if n >= 0 => (n, String::new()),
        _ => (
            0,
            format!("{key}={v} ignoré (pas un nombre de threads ≥ 0) → automatique"),
        ),
    }
}

/// `batchCPUFlags`.
pub const BATCH_CPU_FLAGS: &[&str] = &[
    "-Cb",
    "--cpu-mask-batch",
    "-Crb",
    "--cpu-range-batch",
    "--cpu-strict-batch",
    "--prio-batch",
    "--poll-batch",
];

/// `threadArgs` : -t / -tb.
pub fn thread_args(
    threads: &str,
    threads_batch: &str,
    extra: &[String],
    auto: &CpuBudget,
) -> (Vec<String>, Vec<String>) {
    let mut args = Vec::new();
    let mut notes = Vec::new();
    let (t, note) = thread_count("THREADS", threads);
    if !note.is_empty() {
        notes.push(note);
    }
    let mut t_val = String::new();
    if has_any_flag(extra, &["-t", "--threads"]) {
        t_val = flag_value(extra, &["-t", "--threads"]);
    } else if t > 0 {
        t_val = t.to_string();
        args.push("-t".into());
        args.push(t_val.clone());
    } else if auto.n > 0
        && !has_any_flag(
            extra,
            &["-C", "--cpu-mask", "-Cr", "--cpu-range", "--cpu-strict"],
        )
    {
        t_val = auto.n.to_string();
        args.push("-t".into());
        args.push(t_val.clone());
        notes.push(format!(
            "threads auto → -t {} : {} (llama.cpp seul en lancerait {})",
            auto.n, auto.why, auto.engine
        ));
    }
    let (tb, note) = thread_count("THREADS_BATCH", threads_batch);
    if !note.is_empty() {
        notes.push(note);
    }
    if has_any_flag(extra, &["-tb", "--threads-batch"]) {
    } else if tb > 0 {
        args.push("-tb".into());
        args.push(tb.to_string());
    } else if has_any_flag(extra, BATCH_CPU_FLAGS) {
        if t_val.is_empty() {
            t_val = "0".into();
            notes.push("EXTRA_ARGS règle le CPU du batch (-Cb, --poll-batch…) : -tb 0 posé pour que llama.cpp le garde (tous les threads logiques) ; THREADS_BATCH pour choisir le nombre".into());
        }
        args.push("-tb".into());
        args.push(t_val);
    }
    (args, notes)
}

fn read(root: &Path, rel: &str) -> Option<String> {
    std::fs::read(root.join(rel))
        .ok()
        .map(|b| String::from_utf8_lossy(&b).into_owned())
}

/// `cpusetThreads` : sonde de conteneur (Linux) ; `root` = la racine « / ».
pub fn cpuset_threads(root: &Path) -> CpuBudget {
    let Some(online) = read_cpu_list(root, "sys/devices/system/cpu/online") else {
        return CpuBudget::default();
    };
    if online.is_empty() {
        return CpuBudget::default();
    }
    let Some(allowed) = proc_allowed_cpus(root) else {
        return CpuBudget::default();
    };
    let allowed = intersect_cpus(&allowed, &online);
    if allowed.is_empty() {
        return CpuBudget::default();
    }
    let restricted = allowed.len() < online.len();
    let quota = cgroup_cpu_quota(root);
    if !restricted && quota == 0 {
        return CpuBudget::default();
    }
    let atom = read_cpu_list(root, "sys/devices/cpu_atom/cpus").unwrap_or_default();
    let Some(host_all) = physical_cores(root, &online, &[]) else {
        return CpuBudget::default();
    };
    let Some(mut host_math) = physical_cores(root, &online, &atom) else {
        return CpuBudget::default();
    };
    if host_math == 0 {
        host_math = host_all;
    }
    let (mut n, engine, mut why);
    if restricted {
        engine = host_all;
        let Some(x) = physical_cores(root, &allowed, &atom) else {
            return CpuBudget::default();
        };
        n = x;
        if n == 0 {
            let Some(x) = physical_cores(root, &allowed, &[]) else {
                return CpuBudget::default();
            };
            n = x;
        }
        why = format!("cpuset du conteneur, {n} cœurs physiques permis sur {host_all}");
        if !atom.is_empty() {
            why.push_str(" (cœurs E écartés)");
        }
    } else {
        engine = host_math;
        n = host_math;
        why = String::new();
    }
    if quota > 0 && quota < n {
        n = quota;
        why = format!("quota cgroup de {quota} CPU");
    }
    if n <= 0 || n >= engine {
        return CpuBudget::default();
    }
    CpuBudget { n, engine, why }
}

/// `physicalCores` : une chaîne `thread_siblings` distincte par cœur.
pub fn physical_cores(root: &Path, cpus: &[i64], exclude: &[i64]) -> Option<i64> {
    let mut seen = BTreeSet::new();
    for c in cpus {
        if exclude.contains(c) {
            continue;
        }
        let b = read(
            root,
            &format!("sys/devices/system/cpu/cpu{c}/topology/thread_siblings"),
        )?;
        seen.insert(trim(&b).to_string());
    }
    Some(seen.len() as i64)
}

/// `procAllowedCPUs` : `Cpus_allowed_list` de /proc/self/status.
fn proc_allowed_cpus(root: &Path) -> Option<Vec<i64>> {
    let b = read(root, "proc/self/status")?;
    for line in b.split('\n') {
        if let Some(v) = line.strip_prefix("Cpus_allowed_list:") {
            return parse_cpu_list(v);
        }
    }
    None
}

/// `cgroupCPUQuota` : quota arrondi au CPU supérieur, 0 sans quota.
pub fn cgroup_cpu_quota(root: &Path) -> i64 {
    if let Some(b) = read(root, "sys/fs/cgroup/cpu.max") {
        let f: Vec<&str> = b.split_whitespace().collect();
        if f.is_empty() || f[0] == "max" {
            return 0;
        }
        let period = f.get(1).copied().unwrap_or("100000");
        return ceil_quota(f[0], period);
    }
    for dir in ["sys/fs/cgroup/cpu", "sys/fs/cgroup/cpu,cpuacct"] {
        let Some(q) = read(root, &format!("{dir}/cpu.cfs_quota_us")) else {
            continue;
        };
        let Some(p) = read(root, &format!("{dir}/cpu.cfs_period_us")) else {
            return 0;
        };
        return ceil_quota(trim(&q), trim(&p));
    }
    0
}

fn ceil_quota(quota: &str, period: &str) -> i64 {
    match (atoi(quota), atoi(period)) {
        (Some(q), Some(p)) if q > 0 && p > 0 => ((q + p - 1) / p).max(1),
        _ => 0,
    }
}

fn read_cpu_list(root: &Path, rel: &str) -> Option<Vec<i64>> {
    parse_cpu_list(&read(root, rel)?)
}

/// `parseCPUList` : « 0-3,8-11 » → liste triée sans doublon ; syntaxe
/// inconnue → `None`.
pub fn parse_cpu_list(s: &str) -> Option<Vec<i64>> {
    let mut set = BTreeSet::new();
    for part in trim(s).split(',') {
        let part = trim(part);
        if part.is_empty() {
            continue;
        }
        let (lo, hi) = match part.split_once('-') {
            Some((l, h)) => (l, Some(h)),
            None => (part, None),
        };
        let a = atoi(lo)?;
        let b = match hi {
            Some(h) => atoi(h)?,
            None => a,
        };
        if a < 0 || b < a || b - a > 1 << 16 {
            return None;
        }
        for c in a..=b {
            set.insert(c);
        }
    }
    Some(set.into_iter().collect())
}

fn intersect_cpus(a: &[i64], b: &[i64]) -> Vec<i64> {
    a.iter().copied().filter(|c| b.contains(c)).collect()
}
