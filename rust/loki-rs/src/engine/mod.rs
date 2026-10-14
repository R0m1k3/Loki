//! Supervision du moteur en conteneur — port de `sys_service_container.go`
//! (lot 3 de la migration).
//!
//! `loki-rs engine start|stop|restart|status` lance `loki serve` (Go : il
//! construit toujours la ligne de commande de llama-server ou de Strata) en
//! chef de session, détaché, et l'arrête avec tout son arbre. Fichier PID et
//! journal aux MÊMES chemins que le Go (`$LOKI_HOME/<service>.pid|.log`) :
//! `serviceIsActive`, l'UI et `loki logs` lisent les mêmes fichiers.
//!
//! Le Go garde ses garde-fous (optimisation en cours, preset externe) et
//! délègue ici ensuite (`sys_service_rust_linux.go`).

pub mod proctree;

use std::path::{Path, PathBuf};
use std::time::Duration;

#[derive(Debug, Clone)]
pub struct Paths {
    pub home: PathBuf,
    pub service: String,
}

impl Paths {
    pub fn from_env() -> Self {
        let env = |k: &str| std::env::var(k).ok().filter(|v| !v.trim().is_empty());
        Paths {
            home: PathBuf::from(env("LOKI_HOME").unwrap_or_else(|| "/data".into())),
            service: env("LOKI_SERVICE").unwrap_or_else(|| "loki-engine".into()),
        }
    }
    pub fn pid(&self) -> PathBuf {
        self.home.join(format!("{}.pid", self.service))
    }
    pub fn log(&self) -> PathBuf {
        self.home.join(format!("{}.log", self.service))
    }
}

#[derive(Debug, Clone)]
pub struct Opts {
    /// Binaire Go à lancer en `serve`.
    pub go_bin: PathBuf,
    /// Délai avant de juger que le démarrage a tenu (2 s comme le Go).
    pub check_delay: Duration,
    /// SIGTERM → SIGKILL (4 s comme le Go).
    pub grace: Duration,
    /// Attente maximale après SIGKILL (20 s comme le Go).
    pub settle: Duration,
}

impl Opts {
    pub fn new(go_bin: PathBuf) -> Self {
        Opts {
            go_bin,
            check_delay: Duration::from_secs(2),
            grace: Duration::from_secs(4),
            settle: Duration::from_secs(20),
        }
    }
}

pub fn read_pid(path: &Path) -> i32 {
    std::fs::read_to_string(path)
        .ok()
        .and_then(|s| s.trim().parse().ok())
        .unwrap_or(0)
}

/// Les `n` dernières lignes de `text`, comme `tailFile` du Go.
pub fn tail_lines(text: &str, n: usize) -> String {
    let lines: Vec<&str> = text.trim_end_matches('\n').split('\n').collect();
    let from = lines.len().saturating_sub(n);
    lines[from..].join("\n") + "\n"
}

/// Exécute une action ; les messages vont sur la sortie standard, l'erreur
/// éventuelle est rendue à l'appelant (le Go l'affiche telle quelle).
pub fn run(action: &str, paths: &Paths, opts: &Opts) -> Result<(), String> {
    imp::run(action, paths, opts)
}

#[cfg(target_os = "linux")]
mod imp {
    use super::proctree::linux::{pid_alive, stop_proc_tree};
    use super::*;
    use std::fs::OpenOptions;
    use std::os::unix::process::CommandExt;
    use std::process::{Command, Stdio};

    pub fn run(action: &str, paths: &Paths, opts: &Opts) -> Result<(), String> {
        match action {
            "start" => start(paths, opts),
            "stop" => stop(paths, opts, true),
            "restart" => {
                stop(paths, opts, false)?;
                std::thread::sleep(Duration::from_millis(500));
                start(paths, opts)
            }
            "status" => {
                let pid = read_pid(&paths.pid());
                if pid_alive(pid) {
                    println!("[ok] {}: actif (PID {pid})", paths.service);
                } else {
                    println!("[info] {}: arrêté", paths.service);
                }
                println!("  logs   : {}", paths.log().display());
                Ok(())
            }
            other => Err(format!("action inconnue: {other}")),
        }
    }

    fn start(paths: &Paths, opts: &Opts) -> Result<(), String> {
        let pid = read_pid(&paths.pid());
        if pid_alive(pid) {
            println!("[info] déjà démarré (PID {pid})");
            return Ok(());
        }
        std::fs::create_dir_all(&paths.home).map_err(|e| e.to_string())?;
        let log = OpenOptions::new()
            .create(true)
            .append(true)
            .open(paths.log())
            .map_err(|e| format!("ouverture du log {}: {e}", paths.log().display()))?;
        let log2 = log.try_clone().map_err(|e| e.to_string())?;
        let mut cmd = Command::new(&opts.go_bin);
        cmd.arg("serve")
            .stdin(Stdio::null())
            .stdout(log)
            .stderr(log2)
            // Même répertoire de travail que sous systemd : les chemins
            // relatifs de config.env se résolvent depuis LOKI_HOME.
            .current_dir(&paths.home);
        // SAFETY: setsid(2) est async-signal-safe, seul appel entre fork et exec.
        // Chef de session : l'enfant survit à ce processus, et un signal au
        // groupe (-pid) arrête aussi llama-server.
        unsafe {
            cmd.pre_exec(|| {
                if libc::setsid() < 0 {
                    return Err(std::io::Error::last_os_error());
                }
                Ok(())
            });
        }
        let mut child = cmd
            .spawn()
            .map_err(|e| format!("démarrage de 'loki serve': {e}"))?;
        let pid = child.id() as i32;
        std::fs::write(paths.pid(), pid.to_string())
            .map_err(|e| format!("écriture du PID: {e}"))?;

        std::thread::sleep(opts.check_delay);
        // try_wait et non kill(pid, 0) : l'enfant mort reste un zombie tant
        // que ce processus vit, et un zombie répond encore au signal 0.
        if matches!(child.try_wait(), Ok(None)) {
            println!("[ok] {}: démarré (PID {pid})", paths.service);
            println!(
                "       logs: {}  (loki logs pour suivre)",
                paths.log().display()
            );
            // Pas de wait : l'enfant est rattaché à PID 1 (tini) à notre sortie.
            return Ok(());
        }
        let tail = std::fs::read_to_string(paths.log())
            .map(|t| tail_lines(&t, 20))
            .unwrap_or_default();
        println!(
            "[ERREUR] {}: le processus s'est arrêté — derniers logs :",
            paths.service
        );
        println!("------------------------------------------------");
        print!("{tail}");
        println!("------------------------------------------------");
        println!("→ loki logs   pour plus de détails\n→ loki edit   pour corriger config.env");
        let _ = std::fs::remove_file(paths.pid());
        Err(format!("service {} non démarré", paths.service))
    }

    fn stop(paths: &Paths, opts: &Opts, verbose: bool) -> Result<(), String> {
        let pid = read_pid(&paths.pid());
        // Ni ce processus, ni le Go qui l'a lancé ne font partie de l'arbre.
        let me = std::process::id() as i32;
        // SAFETY: getppid(2) ne peut pas échouer.
        let parent = unsafe { libc::getppid() };
        let exclude = [me, parent];
        let tree = if pid > 0 {
            super::proctree::proc_tree_of(&super::proctree::linux::proc_table(), pid, &exclude)
        } else {
            Vec::new()
        };
        // Le chef parti, ses enfants peuvent tourner encore (moteur de
        // Strata) : « rien à arrêter » seulement si l'arbre entier est vide.
        if tree.is_empty() {
            let _ = std::fs::remove_file(paths.pid());
            if verbose {
                println!("[info] aucun service en cours d'exécution");
            }
            return Ok(());
        }
        let left = stop_proc_tree(pid, &exclude, opts.grace, opts.settle);
        let _ = std::fs::remove_file(paths.pid());
        if let Some(first) = left.first() {
            eprintln!(
                "[attention] {} processus du moteur toujours là après SIGKILL (PID {}…) : \
                 la mémoire du GPU peut ne pas être encore rendue",
                left.len(),
                first.pid
            );
        }
        if verbose {
            println!("[ok] arrêté");
        }
        Ok(())
    }
}

#[cfg(not(target_os = "linux"))]
mod imp {
    use super::*;

    pub fn run(_: &str, _: &Paths, _: &Opts) -> Result<(), String> {
        Err("supervision du moteur par loki-rs : Linux (conteneur) seulement".into())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn fin_de_journal() {
        assert_eq!(tail_lines("a\nb\nc\n", 2), "b\nc\n");
        assert_eq!(tail_lines("a\nb", 5), "a\nb\n");
        assert_eq!(tail_lines("", 3), "\n");
    }

    #[test]
    fn chemins() {
        let p = Paths {
            home: PathBuf::from("/data"),
            service: "loki-engine".into(),
        };
        assert_eq!(p.pid(), Path::new("/data/loki-engine.pid"));
        assert_eq!(p.log(), Path::new("/data/loki-engine.log"));
    }
}
