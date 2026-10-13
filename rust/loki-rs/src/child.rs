//! Supervision du serveur Go (`loki web`) lancé derrière la façade.
//!
//! Le Go reste propriétaire de presque tout au lot 1 : s'il meurt, la façade
//! le relance (attente croissante 1 s → 30 s, remise à 1 s après une minute
//! de vie) et répond 502 entre-temps au lieu de tomber avec lui.

use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::time::{Duration, Instant};
use tokio::process::{Child, Command};
use tokio::sync::oneshot;

const MIN_DELAY: Duration = Duration::from_secs(1);
const MAX_DELAY: Duration = Duration::from_secs(30);
const STABLE_AFTER: Duration = Duration::from_secs(60);

/// Attente avant la relance suivante, connaissant la précédente et la durée
/// de vie du processus qui vient de mourir.
pub fn next_delay(prev: Duration, lived: Duration) -> Duration {
    if lived >= STABLE_AFTER {
        MIN_DELAY
    } else {
        (prev * 2).clamp(MIN_DELAY, MAX_DELAY)
    }
}

/// Binaire Go à lancer : `LOKI_GO_BIN`, sinon `loki` à côté de l'exécutable
/// courant (les deux viennent toujours du même dossier, cf. `which-bin`).
pub fn go_bin(env_value: Option<String>, exe: &Path) -> PathBuf {
    if let Some(p) = env_value.filter(|v| !v.trim().is_empty()) {
        return PathBuf::from(p);
    }
    let name = if cfg!(windows) { "loki.exe" } else { "loki" };
    exe.parent().unwrap_or(Path::new(".")).join(name)
}

pub struct Supervisor {
    stop: Option<oneshot::Sender<Duration>>,
    done: tokio::task::JoinHandle<()>,
}

impl Supervisor {
    /// Lance `<go_bin> web <port>` en écoute locale et le maintient en vie.
    /// `env` : variables ajoutées à l'environnement du Go (jeton interne…).
    pub fn spawn(go_bin: PathBuf, port: u16, env: Vec<(String, String)>) -> Supervisor {
        let (tx, mut rx) = oneshot::channel::<Duration>();
        let done = tokio::spawn(async move {
            let mut delay = Duration::ZERO;
            loop {
                let started = Instant::now();
                let mut child = match launch(&go_bin, port, &env) {
                    Ok(c) => c,
                    Err(e) => {
                        eprintln!(
                            "[loki-rs] lancement de {} impossible : {e}",
                            go_bin.display()
                        );
                        delay = next_delay(delay, Duration::ZERO);
                        tokio::select! {
                            _ = tokio::time::sleep(delay) => continue,
                            _ = &mut rx => return,
                        }
                    }
                };
                tokio::select! {
                    status = child.wait() => {
                        let lived = started.elapsed();
                        delay = next_delay(delay, lived);
                        let status = match status {
                            Ok(s) => s.to_string(),
                            Err(e) => e.to_string(),
                        };
                        eprintln!(
                            "[loki-rs] serveur Go arrêté ({status}) après {lived:?} — relance dans {delay:?}"
                        );
                        tokio::select! {
                            _ = tokio::time::sleep(delay) => {}
                            _ = &mut rx => return,
                        }
                    }
                    grace = &mut rx => {
                        stop_child(&mut child, grace.unwrap_or(MIN_DELAY)).await;
                        return;
                    }
                }
            }
        });
        Supervisor {
            stop: Some(tx),
            done,
        }
    }

    /// Arrête le Go : SIGTERM, puis SIGKILL passé `grace`.
    pub async fn shutdown(mut self, grace: Duration) {
        if let Some(tx) = self.stop.take() {
            let _ = tx.send(grace);
        }
        let _ = (&mut self.done).await;
    }
}

fn launch(go_bin: &Path, port: u16, env: &[(String, String)]) -> std::io::Result<Child> {
    Command::new(go_bin)
        .envs(env.iter().map(|(k, v)| (k, v)))
        .arg("web")
        .arg(port.to_string())
        .env("LOKI_WEB_BIND", "127.0.0.1")
        // stdin fermé : `loki web` ne doit jamais attendre une réponse au
        // clavier (résolution de conflit de port).
        .stdin(Stdio::null())
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit())
        .kill_on_drop(true)
        .spawn()
}

async fn stop_child(child: &mut Child, grace: Duration) {
    #[cfg(unix)]
    if let Some(pid) = child.id() {
        // SAFETY: kill(2) sur le PID de notre propre enfant, pas encore récolté.
        unsafe {
            libc::kill(pid as libc::pid_t, libc::SIGTERM);
        }
        if tokio::time::timeout(grace, child.wait()).await.is_ok() {
            return;
        }
    }
    #[cfg(not(unix))]
    let _ = grace;
    let _ = child.kill().await;
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn attente_croissante_puis_remise_a_zero() {
        let s = Duration::from_secs;
        assert_eq!(next_delay(Duration::ZERO, s(0)), s(1));
        assert_eq!(next_delay(s(1), s(2)), s(2));
        assert_eq!(next_delay(s(16), s(2)), s(30));
        assert_eq!(next_delay(s(30), s(2)), s(30));
        assert_eq!(next_delay(s(30), s(61)), s(1));
    }

    #[test]
    fn binaire_go_par_defaut_a_cote_de_l_exe() {
        let exe = Path::new("/usr/local/bin/loki-rs");
        let name = if cfg!(windows) { "loki.exe" } else { "loki" };
        let dir = Path::new("/usr/local/bin");
        assert_eq!(go_bin(None, exe), dir.join(name));
        assert_eq!(go_bin(Some(" ".into()), exe), dir.join(name));
        assert_eq!(
            go_bin(Some("/x/loki".into()), exe),
            PathBuf::from("/x/loki")
        );
    }
}
