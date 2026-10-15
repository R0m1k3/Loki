//! `/api/backends/devices` : les GPU tels que les voit UN moteur
//! (`llama-server --list-devices`), port de `web_devices.go` — caches
//! mémoire (10 min) et disque (`devices.json`, dernière énumération réussie).

use std::collections::{BTreeMap, HashMap};
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::{LazyLock, Mutex};
use std::time::{Duration, Instant};

use regex::Regex;
use serde_json::{Map, Value, json};

use super::probe::{GpuInfo, Probe};

/// `devCacheTTL`.
const CACHE_TTL: Duration = Duration::from_secs(600);

/// Délai de l'énumération.
const LIST_TIMEOUT: Duration = Duration::from_secs(20);

// `\s` du Go : blancs ASCII seulement.
static DEVICE_LINE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(
        &r"^\s*([A-Za-z]+[0-9]+):\s*(.+?)\s*\(([0-9]+)\s*MiB,\s*([0-9]+)\s*MiB free\)\s*$"
            .replace(r"\s", r"[\t\n\f\r ]"),
    )
    .expect("motif")
});

/// `parseListDevices`.
pub fn parse_list_devices(out: &str) -> Vec<Map<String, Value>> {
    let mut devs = Vec::new();
    for line in out.split('\n') {
        let Some(m) = DEVICE_LINE.captures(line.trim_end_matches('\r')) else {
            continue;
        };
        let mut d = Map::new();
        d.insert("id".into(), json!(&m[1]));
        d.insert("name".into(), json!(&m[2]));
        d.insert("total_mib".into(), json!(atoi(&m[3])));
        d.insert("free_mib".into(), json!(atoi(&m[4])));
        devs.push(d);
    }
    devs
}

/// `strconv.Atoi` d'une suite de chiffres : 0 si elle déborde.
fn atoi(s: &str) -> i64 {
    s.parse().unwrap_or(0)
}

/// Mémoire totale annoncée (`d["total_mib"].(int)`) : seuls les entiers
/// issus de l'énumération comptent ; 0 sinon.
fn total_mib(d: &Map<String, Value>) -> i64 {
    d.get("total_mib").and_then(Value::as_i64).unwrap_or(0)
}

/// `annotateDevices` : mémoire manquante et liaison PCIe, par nom de carte.
pub fn annotate(devs: &mut [Map<String, Value>], gpus: &[GpuInfo]) {
    let mut by_name: HashMap<String, &GpuInfo> = HashMap::new();
    let mut dup: HashMap<String, bool> = HashMap::new();
    for g in gpus {
        let name = g.name.trim().to_string();
        if by_name.contains_key(&name) {
            dup.insert(name, true);
            continue;
        }
        by_name.insert(name, g);
    }
    for d in devs.iter_mut() {
        let name = d
            .get("name")
            .and_then(Value::as_str)
            .unwrap_or("")
            .trim()
            .to_string();
        let Some(g) = by_name.get(&name) else {
            continue;
        };
        if dup.contains_key(&name) {
            continue;
        }
        if total_mib(d) <= 0
            && let Ok(mb) = g.mem_total.trim().parse::<i64>()
        {
            d.insert("total_mib".into(), json!(mb));
        }
        if !d
            .get("id")
            .and_then(Value::as_str)
            .unwrap_or("")
            .starts_with("CUDA")
        {
            continue;
        }
        if g.link_gen_max == 0 && g.link_width_max == 0 && g.mem_clock_max == 0 {
            continue;
        }
        d.insert(
            "link".into(),
            json!({
                "gen": g.link_gen, "gen_max": g.link_gen_max,
                "width": g.link_width, "width_max": g.link_width_max,
                "mem_clock_mhz": g.mem_clock_max,
            }),
        );
    }
}

/// `hasZeroMemory`.
pub fn has_zero_memory(devs: &[Map<String, Value>]) -> bool {
    devs.iter().any(|d| total_mib(d) <= 0)
}

/// Une énumération en cache, et quand.
type Cached = (Vec<Map<String, Value>>, Instant);

/// Les deux caches de l'énumération.
pub struct Devices {
    home: PathBuf,
    mem: Mutex<HashMap<String, Cached>>,
    persist: Mutex<()>,
}

impl Devices {
    pub fn new(home: PathBuf) -> Self {
        Devices {
            home,
            mem: Mutex::new(HashMap::new()),
            persist: Mutex::new(()),
        }
    }

    fn cache_get(&self, key: &str) -> Option<Vec<Map<String, Value>>> {
        let m = self.mem.lock().unwrap_or_else(|e| e.into_inner());
        m.get(key)
            .filter(|(_, at)| at.elapsed() <= CACHE_TTL)
            .map(|(d, _)| d.clone())
    }

    fn cache_put(&self, key: &str, devs: &[Map<String, Value>]) {
        self.mem
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .insert(key.into(), (devs.to_vec(), Instant::now()));
    }

    /// `devPersistPath`.
    pub fn persist_path(&self) -> PathBuf {
        self.home.join("devices.json")
    }

    fn persist_load(&self) -> BTreeMap<String, Value> {
        std::fs::read(self.persist_path())
            .ok()
            .and_then(|b| serde_json::from_slice::<BTreeMap<String, Value>>(&b).ok())
            .unwrap_or_default()
    }

    fn persist_get(&self, key: &str) -> Option<Value> {
        let _g = self.persist.lock().unwrap_or_else(|e| e.into_inner());
        self.persist_load()
            .remove(key)
            .filter(|d| d.as_array().is_some_and(|a| !a.is_empty()))
    }

    fn persist_put(&self, key: &str, devs: &[Map<String, Value>]) {
        let _g = self.persist.lock().unwrap_or_else(|e| e.into_inner());
        let mut m = self.persist_load();
        m.insert(key.into(), json!(devs));
        let _ = std::fs::write(self.persist_path(), crate::chatx::marshal_indent(&m));
    }

    /// `handleBackendDevices`, une fois `bin` résolu et vérifié : la réponse.
    pub async fn list(&self, probe: &Probe, bin: &str, cvd: &str) -> Value {
        let key = format!("{bin}\x00{cvd}");
        if let Some(devs) = self.cache_get(&key) {
            return json!({ "ok": true, "devices": devs });
        }
        let (out, err) = list_devices(bin, cvd).await;
        let mut devs = parse_list_devices(&out);
        if let Some(err) = err {
            if let Some(good) = self.persist_get(&key) {
                return json!({ "ok": true, "devices": good, "stale": true });
            }
            if devs.is_empty() {
                return json!({ "ok": false, "error": format!("le moteur n'a pas répondu : {err}") });
            }
            return json!({ "ok": true, "devices": devs, "stale": true });
        }
        if let Ok(gpus) = probe.detect_gpus().await {
            annotate(&mut devs, &gpus);
        }
        if !has_zero_memory(&devs) {
            self.cache_put(&key, &devs);
        }
        if !devs.is_empty() {
            self.persist_put(&key, &devs);
        }
        json!({ "ok": true, "devices": devs })
    }
}

/// `bin --list-devices`, sortie standard et d'erreur mêlées ; l'erreur au
/// format d'`exec` (`exit status N`, `signal: killed`…).
async fn list_devices(bin: &str, cvd: &str) -> (String, Option<String>) {
    let dir = crate::paths::go_dir(bin);
    let env = crate::serve::run::Env::default();
    let (k, v) = crate::serve::run::library_path_value(&env, &dir);
    let mut c = tokio::process::Command::new(bin);
    c.arg("--list-devices").env(k, v);
    if !cvd.is_empty() {
        c.env("CUDA_VISIBLE_DEVICES", cvd)
            .env("CUDA_DEVICE_ORDER", "PCI_BUS_ID");
    }
    c.stdin(Stdio::null()).kill_on_drop(true);
    combined(c, bin, Some(LIST_TIMEOUT)).await
}

/// `cmd.CombinedOutput()` : sorties standard et d'erreur dans le même tube ;
/// l'erreur au format d'`exec` (`exit status N`, `signal: killed`,
/// `fork/exec …`). `limit` : délai au-delà duquel le processus est tué.
pub async fn combined(
    mut c: tokio::process::Command,
    bin: &str,
    limit: Option<Duration>,
) -> (String, Option<String>) {
    // Sortie standard et d'erreur dans le même tube, comme CombinedOutput.
    let (reader, writer) = match std::io::pipe() {
        Ok(p) => p,
        Err(e) => return (String::new(), Some(e.to_string())),
    };
    let writer2 = match writer.try_clone() {
        Ok(w) => w,
        Err(e) => return (String::new(), Some(e.to_string())),
    };
    c.stdout(Stdio::from(writer)).stderr(Stdio::from(writer2));
    let mut child = match c.spawn() {
        Ok(ch) => ch,
        Err(e) => {
            return (
                String::new(),
                Some(crate::paths::os_err("fork/exec", bin, &e)),
            );
        }
    };
    drop(c);
    let read = tokio::task::spawn_blocking(move || {
        let mut r = reader;
        let mut buf = Vec::new();
        let _ = std::io::Read::read_to_end(&mut r, &mut buf);
        buf
    });
    let status = match limit {
        None => child.wait().await,
        Some(d) => match tokio::time::timeout(d, child.wait()).await {
            Ok(s) => s,
            Err(_) => {
                let _ = child.start_kill();
                let _ = child.wait().await;
                Err(std::io::Error::other("signal: killed"))
            }
        },
    };
    let out = read.await.unwrap_or_default();
    let out = crate::system::go_lossy(&out);
    let err = match status {
        Ok(s) if s.success() => None,
        Ok(s) => Some(exit_text(s)),
        Err(e) => Some(e.to_string()),
    };
    (out, err)
}

/// Texte d'un `*exec.ExitError`.
fn exit_text(s: std::process::ExitStatus) -> String {
    if let Some(code) = s.code() {
        return format!("exit status {code}");
    }
    #[cfg(unix)]
    {
        use std::os::unix::process::ExitStatusExt;
        if let Some(sig) = s.signal() {
            return format!("signal: {}", signal_name(sig));
        }
    }
    s.to_string()
}

/// Nom d'un signal comme `syscall.Signal.String()`.
#[cfg(unix)]
fn signal_name(sig: i32) -> String {
    match sig {
        1 => "hangup".into(),
        2 => "interrupt".into(),
        3 => "quit".into(),
        4 => "illegal instruction".into(),
        6 => "aborted".into(),
        7 => "bus error".into(),
        8 => "floating point exception".into(),
        9 => "killed".into(),
        11 => "segmentation fault".into(),
        13 => "broken pipe".into(),
        15 => "terminated".into(),
        n => format!("signal {n}"),
    }
}

/// Le binaire est-il un serveur llama.cpp (garde-fou de la route) ?
pub fn is_llama_server(bin: &str) -> bool {
    let base = Path::new(bin)
        .file_name()
        .map(|b| b.to_string_lossy().to_lowercase())
        .unwrap_or_default();
    base == "llama-server" || base == "llama-server.exe"
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse() {
        let d = parse_list_devices(
            "Available devices:\n  CUDA0: NVIDIA GeForce RTX 5060 Ti (15849 MiB, 15579 MiB free)\r\n  bruit\n",
        );
        assert_eq!(d.len(), 1);
        assert_eq!(d[0]["id"], "CUDA0");
        assert_eq!(d[0]["total_mib"], 15849);
    }

    #[test]
    fn serveur() {
        assert!(is_llama_server("/x/LLAMA-SERVER"));
        assert!(!is_llama_server("/bin/sh"));
    }
}
