//! Commandes shell d'arrière-plan du mode Code (lot 43) — port de
//! `code_jobs.go` : `bash_bg` lance un processus détaché (serveur de dev,
//! watcher, build long) et rend la main ; `bash_tail` relit sa sortie et son
//! état, et l'arrête sur demande.
//!
//! La sortie va dans `.loki/jobs/<id>.log` du dossier de la discussion : elle
//! survit au tour, et le panneau Fichiers la montre. Les jobs d'une
//! discussion supprimée sont arrêtés ; l'optimiseur refuse de mesurer tant
//! qu'il en tourne (le Go demande leur nombre à la façade).

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant};

use serde_json::{Map, Value};

use super::policy;
use super::shell::{TOOL_MAX_OUTPUT, go_duration, shell_cmd};
use crate::system::go_lossy;

/// Un job lancé par `bash_bg`.
struct Job {
    id: String,
    command: String,
    log_path: PathBuf,
    /// Discussion du tour qui l'a lancé.
    conv: String,
    started: Instant,
    /// `None` tant qu'il tourne, puis son code de sortie.
    exit: Mutex<Option<i32>>,
    kill: tokio::sync::Notify,
}

impl Job {
    fn running(&self) -> bool {
        self.exit
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .is_none()
    }
}

/// Les jobs d'arrière-plan de la façade.
#[derive(Default)]
pub struct BgJobs {
    state: Mutex<(u64, HashMap<String, Arc<Job>>)>,
}

/// Les jobs de ce processus (les tours et la suppression des discussions).
pub fn global() -> Arc<BgJobs> {
    static G: OnceLock<Arc<BgJobs>> = OnceLock::new();
    G.get_or_init(Default::default).clone()
}

/// `args[k].(string)`.
fn arg_str<'a>(args: &'a Map<String, Value>, k: &str) -> &'a str {
    args.get(k).and_then(Value::as_str).unwrap_or("")
}

impl BgJobs {
    /// `toolBashBg` : `tune_busy`, le refus de l'optimiseur en cours s'il y
    /// en a un.
    pub async fn bash_bg(
        &self,
        cwd: &Path,
        conv: &str,
        args: &Map<String, Value>,
        tune_busy: Option<String>,
    ) -> String {
        let command = arg_str(args, "command");
        if command.trim().is_empty() {
            return "[erreur] commande vide".into();
        }
        if let Some(reason) = policy::dangerous_command(command) {
            return policy::refused_command_result(reason);
        }
        // Un serveur ou un build lancé pendant une optimisation fausserait
        // ses mesures.
        if let Some(e) = tune_busy {
            return format!("[erreur] {e}");
        }
        let log_dir = cwd.join(".loki").join("jobs");
        if let Err(e) = crate::paths::go_mkdir_all(&log_dir) {
            return format!("[erreur] {e}");
        }
        let id = {
            let mut st = self.state.lock().unwrap_or_else(|e| e.into_inner());
            st.0 += 1;
            format!("job{}", st.0)
        };
        let log_path = log_dir.join(format!("{id}.log"));
        let log = match std::fs::File::create(&log_path) {
            Ok(f) => f,
            Err(e) => {
                return format!(
                    "[erreur] {}",
                    crate::paths::os_err("open", &log_path.to_string_lossy(), &e)
                );
            }
        };
        let err_log = match log.try_clone() {
            Ok(f) => f,
            Err(e) => return format!("[erreur] {e}"),
        };
        // Pas le contexte du tour : le job lui survit, c'est sa raison d'être.
        let mut cmd = shell_cmd(command);
        cmd.current_dir(cwd)
            .stdin(std::process::Stdio::null())
            .stdout(log)
            .stderr(err_log)
            .kill_on_drop(false);
        let mut child = match cmd.spawn() {
            Ok(c) => c,
            Err(e) => {
                return format!(
                    "[erreur] {}",
                    crate::paths::os_err("fork/exec", "/bin/bash", &e)
                );
            }
        };
        let pid = child.id().unwrap_or(0);
        let job = Arc::new(Job {
            id: id.clone(),
            command: command.to_string(),
            log_path,
            conv: conv.to_string(),
            started: Instant::now(),
            exit: Mutex::new(None),
            kill: tokio::sync::Notify::new(),
        });
        self.state
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .1
            .insert(id.clone(), job.clone());
        tokio::spawn(async move {
            let status = tokio::select! {
                s = child.wait() => s,
                _ = job.kill.notified() => {
                    let _ = child.start_kill();
                    child.wait().await
                }
            };
            // `ExitCode()` du Go : -1 pour un processus tué par un signal ;
            // 0 si l'attente elle-même a échoué.
            let code = match status {
                Ok(s) => s.code().unwrap_or(-1),
                Err(_) => 0,
            };
            *job.exit.lock().unwrap_or_else(|e| e.into_inner()) = Some(code);
        });
        format!(
            "[ok] job {id} lancé (pid {pid}) — sortie dans .loki/jobs/{id}.log, suis-la avec bash_tail"
        )
    }

    /// `toolBashTail` : la fin du journal d'un job et son état ; `kill`
    /// l'arrête d'abord.
    pub async fn bash_tail(&self, args: &Map<String, Value>) -> String {
        let id = arg_str(args, "id");
        let job = self
            .state
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .1
            .get(id)
            .cloned();
        let Some(job) = job else {
            return format!("[erreur] job inconnu : {id}");
        };
        // Tué puis relu aussitôt, comme le Go : l'état peut encore dire « en
        // cours » le temps que l'attente constate la fin.
        if args.get("kill").and_then(Value::as_bool) == Some(true) && job.running() {
            job.kill.notify_one();
        }
        let mut lines = 40usize;
        if let Some(v) = args.get("lines").and_then(Value::as_f64) {
            let n = crate::webtools::go_int(v);
            if n > 0 {
                lines = n.min(200) as usize;
            }
        }
        let b = match std::fs::read(&job.log_path) {
            Ok(b) => b,
            Err(e) => {
                return format!(
                    "[erreur] {}",
                    crate::paths::os_err("open", &job.log_path.to_string_lossy(), &e)
                );
            }
        };
        let text = go_lossy(&b);
        let all: Vec<&str> = text.trim_end_matches('\n').split('\n').collect();
        let all = &all[all.len().saturating_sub(lines)..];
        let status = match *job.exit.lock().unwrap_or_else(|e| e.into_inner()) {
            None => "en cours".to_string(),
            Some(c) => format!("terminé (exit {c})"),
        };
        let since = Duration::from_secs((job.started.elapsed().as_millis() as u64 + 500) / 1000);
        let out = format!(
            "job {} — {status} — depuis {}\ncommande : {}\n",
            job.id,
            go_duration(since),
            job.command
        );
        let mut tail = all.join("\n");
        if tail.trim().is_empty() {
            tail = "(aucune sortie pour l'instant)".into();
        }
        format!(
            "{out}\n{}",
            crate::webtools::tail_runes(&tail, TOOL_MAX_OUTPUT)
        )
    }

    /// `bgJobsRunning` : les jobs encore en vie.
    pub fn running(&self) -> usize {
        self.state
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .1
            .values()
            .filter(|j| j.running())
            .count()
    }

    /// `stopConvJobs` : les jobs d'une discussion supprimée, arrêtés et
    /// oubliés.
    pub fn stop_conv(&self, conv: &str) {
        let mut st = self.state.lock().unwrap_or_else(|e| e.into_inner());
        st.1.retain(|_, j| {
            if j.conv != conv {
                return true;
            }
            if j.running() {
                j.kill.notify_one();
            }
            false
        });
    }
}

#[cfg(test)]
mod tests;
