//! LE job des moteurs (un seul à la fois) : port de `lcJob` et de sa
//! persistance (`web_llamacpp.go`, `web_llamacpp_job.go`).
//!
//! Quand la façade tient les backends, c'est elle qui détient le job : le
//! sien (binaires précompilés) comme ceux que le Go fait encore tourner
//! (compilation, moteur de l'image, Strata), qui lui rapportent chaque étape
//! par l'API interne (`/internal/facade/v1/build-job/*`). L'entête vit en base
//! (`state/build_job`), les lignes dans `$LOKI_HOME/loki-build.log`.

use std::io::Write;
use std::path::PathBuf;
use std::sync::LazyLock;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use regex::Regex;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tokio::sync::Mutex;

use crate::gojson;
use crate::store::{BK_STATE, Store};

/// `lcMaxLines`.
pub const MAX_LINES: usize = 4000;

/// Clé de l'entête en base.
pub const STATE_KEY: &str = "build_job";

/// Progression throttlée : une écriture au plus toutes les 2 s.
const SAVE_EVERY: Duration = Duration::from_secs(2);

/// Le job courant (`lcJob`).
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Job {
    pub action: String,
    pub phase: String,
    pub compiled: i64,
    pub running: bool,
    pub err: String,
    pub started_at: i64,
    pub ended_at: i64,
    pub old_commit: String,
    pub new_commit: String,
    pub lines: Vec<String>,
    pub base: usize,
}

/// `lcPersisted` : l'entête sur disque, dans l'ordre des champs du Go.
#[derive(Serialize, Deserialize, Default)]
struct Persisted {
    #[serde(default)]
    action: String,
    #[serde(default)]
    phase: String,
    #[serde(default)]
    compiled: i64,
    #[serde(default)]
    running: bool,
    #[serde(default, rename = "error")]
    err: String,
    #[serde(default)]
    started_at: i64,
    #[serde(default)]
    ended_at: i64,
    #[serde(default, rename = "old")]
    old_commit: String,
    #[serde(default, rename = "new")]
    new_commit: String,
}

struct State {
    cur: Option<Job>,
    last_save: Option<Instant>,
}

/// Le détenteur du job.
pub struct Jobs {
    home: PathBuf,
    state: Mutex<State>,
}

fn now() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| d.as_secs() as i64)
}

impl Jobs {
    pub fn new(home: PathBuf) -> Self {
        Jobs {
            home,
            state: Mutex::new(State {
                cur: None,
                last_save: None,
            }),
        }
    }

    /// `lcJobLogPath`.
    pub fn log_path(&self) -> PathBuf {
        self.home.join("loki-build.log")
    }

    /// `lcSaveLine` : une ligne de plus au journal (append).
    fn save_line(&self, line: &str) {
        if let Ok(mut f) = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(self.log_path())
        {
            let _ = f.write_all(format!("{line}\n").as_bytes());
        }
    }

    /// `lcSave` (appelée sous le verrou, comme au Go).
    async fn save(&self, st: &mut State, store: &Store, force: bool) {
        let Some(j) = &st.cur else { return };
        if !force && st.last_save.is_some_and(|t| t.elapsed() < SAVE_EVERY) {
            return;
        }
        st.last_save = Some(Instant::now());
        let p = Persisted {
            action: j.action.clone(),
            phase: j.phase.clone(),
            compiled: j.compiled,
            running: j.running,
            err: j.err.clone(),
            started_at: j.started_at,
            ended_at: j.ended_at,
            old_commit: j.old_commit.clone(),
            new_commit: j.new_commit.clone(),
        };
        if let Err(e) = store
            .put(BK_STATE, STATE_KEY, gojson::marshal(&p).into_bytes())
            .await
        {
            eprintln!("[moteurs] état du job non enregistré : {e}");
        }
    }

    /// `startLcJob` (sans le garde de l'optimiseur, posé par l'appelant).
    pub async fn start(&self, store: &Store, action: &str) -> Result<(), String> {
        let mut st = self.state.lock().await;
        if let Some(j) = &st.cur
            && j.running
        {
            return Err(format!("un job {} est déjà en cours", j.action));
        }
        st.cur = Some(Job {
            action: action.into(),
            running: true,
            phase: "démarrage…".into(),
            started_at: now(),
            ..Job::default()
        });
        let _ = std::fs::remove_file(self.log_path());
        self.save(&mut st, store, true).await;
        Ok(())
    }

    /// Fin du corps du job (le `defer` de `startLcJob`).
    pub async fn end(&self, store: &Store) {
        let mut st = self.state.lock().await;
        if let Some(j) = &mut st.cur {
            j.running = false;
            j.ended_at = now();
        }
        self.save(&mut st, store, true).await;
    }

    /// `lcAppend` : une ligne, et la phase qu'elle annonce.
    pub async fn append(&self, store: &Store, line: &str) {
        let mut st = self.state.lock().await;
        let Some(j) = &mut st.cur else { return };
        j.lines.push(line.into());
        if j.lines.len() > MAX_LINES {
            let drop = j.lines.len() - MAX_LINES;
            j.lines.drain(..drop);
            j.base += drop;
        }
        if !compiled_file(line).is_empty() {
            j.compiled += 1;
            j.phase = format!("compilation… {} fichiers", j.compiled);
        } else {
            let p = phase_label(line);
            if !p.is_empty() {
                j.phase = p;
            }
        }
        self.save_line(line);
        self.save(&mut st, store, false).await;
    }

    /// Une ligne marquée (phase, échec, fin) : posée sans troncature, comme
    /// au Go, et écrite tout de suite.
    async fn mark(&self, store: &Store, line: String, f: impl FnOnce(&mut Job)) {
        let mut st = self.state.lock().await;
        let Some(j) = &mut st.cur else { return };
        f(j);
        j.lines.push(line.clone());
        self.save_line(&line);
        self.save(&mut st, store, true).await;
    }

    /// `lcPhase`.
    pub async fn phase(&self, store: &Store, phase: &str) {
        let p = phase.to_string();
        self.mark(store, format!("▶ {phase}"), |j| j.phase = p)
            .await;
    }

    /// `lcFail`.
    pub async fn fail(&self, store: &Store, err: &str) {
        let e = err.to_string();
        self.mark(store, format!("✗ {err}"), |j| {
            j.err = e;
            j.phase = "échec".into();
        })
        .await;
    }

    /// `lcDone`.
    pub async fn done(&self, store: &Store, msg: &str) {
        let m = msg.to_string();
        self.mark(store, format!("✓ {msg}"), |j| j.phase = m).await;
    }

    /// `lcCur.OldCommit = …` / `lcCur.NewCommit = …` (sans écriture : la
    /// prochaine les emporte).
    pub async fn set_commits(&self, old: Option<&str>, new: Option<&str>) {
        let mut st = self.state.lock().await;
        if let Some(j) = &mut st.cur {
            if let Some(o) = old {
                j.old_commit = o.into();
            }
            if let Some(n) = new {
                j.new_commit = n.into();
            }
        }
    }

    /// `lcJobSnapshot`.
    pub async fn snapshot(&self, from: i64, with_lines: bool) -> Value {
        let st = self.state.lock().await;
        let Some(j) = &st.cur else {
            return json!({ "exists": false });
        };
        let mut out = json!({
            "exists": true,
            "action": j.action,
            "running": j.running,
            "phase": j.phase,
            "compiled": j.compiled,
            "error": j.err,
            "started_at": j.started_at,
            "ended_at": j.ended_at,
            "old": j.old_commit,
            "new": j.new_commit,
        });
        if with_lines {
            let start = (from - j.base as i64).clamp(0, j.lines.len() as i64) as usize;
            let rest = &j.lines[start..];
            // `append([]string(nil), …)` d'une tranche vide : nil → null.
            out["lines"] = if rest.is_empty() {
                Value::Null
            } else {
                json!(rest)
            };
            out["next"] = json!(j.base + j.lines.len());
        }
        out
    }

    /// Le job court-il ?
    pub async fn running(&self) -> bool {
        self.state
            .lock()
            .await
            .cur
            .as_ref()
            .is_some_and(|j| j.running)
    }

    /// `lcRestore` : le dernier job connu au démarrage ; un job noté « en
    /// cours » est mort avec le processus précédent.
    pub async fn restore(&self, store: &Store) {
        let Ok(Some(raw)) = store.get(BK_STATE, STATE_KEY).await else {
            return;
        };
        let Ok(p) = serde_json::from_slice::<Persisted>(&raw) else {
            return;
        };
        if p.action.is_empty() {
            return;
        }
        let mut j = Job {
            action: p.action,
            phase: p.phase,
            compiled: p.compiled,
            running: false,
            err: p.err,
            started_at: p.started_at,
            ended_at: p.ended_at,
            old_commit: p.old_commit,
            new_commit: p.new_commit,
            ..Job::default()
        };
        if p.running {
            j.err = "interrompu : le service a redémarré pendant l'opération — relancez-la".into();
            j.phase = "interrompu".into();
            j.ended_at = now();
        }
        if let Ok(b) = std::fs::read(self.log_path()) {
            let s = String::from_utf8_lossy(&b);
            let mut lines: Vec<String> = s
                .trim_end_matches('\n')
                .split('\n')
                .map(str::to_string)
                .collect();
            if lines.len() == 1 && lines[0].is_empty() {
                lines.clear();
            }
            if lines.len() > MAX_LINES {
                j.base = lines.len() - MAX_LINES;
                lines.drain(..j.base);
            }
            j.lines = lines;
        }
        if p.running {
            j.lines.push(
                "✗ interrompu : le service a redémarré (la compilation ne survit pas au redémarrage)"
                    .into(),
            );
        }
        let mut st = self.state.lock().await;
        if st.cur.is_none() {
            st.cur = Some(j);
        }
        if p.running {
            self.save(&mut st, store, true).await;
        }
    }
}

/// `truncLabel` : coupé à l'octet, comme le Go ; un caractère coupé devient
/// U+FFFD par octet, comme l'encodeur JSON du Go le rendra.
fn trunc_label(s: &str, n: usize) -> String {
    let s = s.trim();
    if s.len() > n {
        return format!("{}…", crate::system::go_lossy(&s.as_bytes()[..n - 1]));
    }
    s.to_string()
}

/// `phaseLabel`.
pub fn phase_label(line: &str) -> String {
    let t = line.trim();
    if let Some(rest) = t.strip_prefix("-- ") {
        return format!("configuration… {}", trunc_label(rest, 50));
    }
    if t.contains("Linking") || t.contains("Build files have been written") {
        return "édition de liens…".into();
    }
    String::new()
}

static RE_COMPILING_CUDA: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"Compiling .*?([0-9A-Za-z_.\-]+\.cu)(?-u:\b)").expect("motif"));
static RE_BARE_SOURCE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^[0-9A-Za-z_.\-]+\.(c|cc|cpp|cxx|cu|cuh)$").expect("motif"));
static RE_BUILDING_OBJ: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"Building (?:C|CXX|CUDA|ASM)[0-9A-Za-z_]* object .*?/([^/]+?)\.o(?:bj)?(?-u:\b)")
        .expect("motif")
});

/// `compiledFile` : le source qu'une ligne de build annonce, "" sinon. Les
/// classes `\w` et `\b` du Go sont ASCII : on les garde ASCII ici aussi.
pub fn compiled_file(line: &str) -> String {
    let t = line.trim();
    if let Some(m) = RE_COMPILING_CUDA.captures(t) {
        return m[1].to_string();
    }
    if let Some(m) = RE_BUILDING_OBJ.captures(t) {
        return m[1].to_string();
    }
    if RE_BARE_SOURCE.is_match(t) {
        return t.to_string();
    }
    String::new()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn labels() {
        assert_eq!(
            phase_label("-- The C compiler identification is GNU"),
            "configuration… The C compiler identification is GNU"
        );
        assert_eq!(
            phase_label("[100%] Linking CXX executable"),
            "édition de liens…"
        );
        assert_eq!(phase_label("rien"), "");
        assert_eq!(
            compiled_file("[ 45%] Building CXX object ggml/src/CMakeFiles/ggml.dir/ggml.cpp.o"),
            "ggml.cpp"
        );
        assert_eq!(compiled_file("foo.cu"), "foo.cu");
        assert_eq!(compiled_file("rien"), "");
    }
}
