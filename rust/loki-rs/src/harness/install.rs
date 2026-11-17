//! Installation et mise à jour de Claude Code dans `LOKI_HOME/harness/claude`.
//!
//! Claude Code est un logiciel propriétaire d'Anthropic : il n'est pas dans
//! l'image Loki (publique). Son installeur officiel le pose sur la machine de
//! l'utilisateur la première fois qu'une discussion le choisit ; `claude
//! update` le met à jour au démarrage de la façade. Tout se passe sous un
//! `HOME` propre (`LOKI_HOME/harness/claude`, volume persistant) : binaire,
//! sessions et réglages ne touchent pas le reste du conteneur.

use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::Mutex;
use std::time::Duration;

use tokio::process::Command;

/// Installeur officiel.
pub const INSTALL_URL: &str = "https://claude.ai/install.sh";
/// Délai d'une installation ou d'une mise à jour.
const SLOW: Duration = Duration::from_secs(300);
/// Délai de `claude --version`.
const FAST: Duration = Duration::from_secs(10);

/// Où en est Claude Code.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum State {
    Absent,
    Installing,
    Updating,
    Ready(String),
    Failed(String),
}

impl State {
    /// Nom pour l'UI (`/api/chat/harness`).
    pub fn name(&self) -> &'static str {
        match self {
            State::Absent => "absent",
            State::Installing => "installing",
            State::Updating => "updating",
            State::Ready(_) => "ready",
            State::Failed(_) => "failed",
        }
    }
}

static STATE: Mutex<Option<State>> = Mutex::new(None);
/// Jamais deux installations ou mises à jour en même temps.
static WORK: tokio::sync::Mutex<()> = tokio::sync::Mutex::const_new(());

fn set(s: State) {
    *STATE.lock().unwrap_or_else(|e| e.into_inner()) = Some(s);
}

fn get() -> Option<State> {
    STATE.lock().unwrap_or_else(|e| e.into_inner()).clone()
}

#[cfg(test)]
pub(crate) fn reset_state() {
    *STATE.lock().unwrap_or_else(|e| e.into_inner()) = None;
}

/// `LOKI_HOME/harness/claude` : le `HOME` de Claude Code.
pub fn home(loki_home: &Path) -> PathBuf {
    loki_home.join("harness").join("claude")
}

/// Le binaire installé, s'il existe et est exécutable.
pub fn bin(loki_home: &Path) -> Option<PathBuf> {
    let p = home(loki_home).join(".local").join("bin").join("claude");
    is_exec(&p).then_some(p)
}

#[cfg(unix)]
fn is_exec(p: &Path) -> bool {
    use std::os::unix::fs::PermissionsExt;
    std::fs::metadata(p).is_ok_and(|m| m.is_file() && m.permissions().mode() & 0o111 != 0)
}

#[cfg(not(unix))]
fn is_exec(p: &Path) -> bool {
    p.is_file()
}

/// `claude --version` : premier mot de la première ligne.
pub async fn version(bin: &Path, home: &Path) -> Option<String> {
    let mut cmd = Command::new(bin);
    cmd.arg("--version")
        .env("HOME", home)
        .stdin(Stdio::null())
        .stderr(Stdio::null())
        .kill_on_drop(true);
    let out = tokio::time::timeout(FAST, cmd.output()).await.ok()?.ok()?;
    let s = String::from_utf8_lossy(&out.stdout);
    s.lines()
        .next()?
        .split_whitespace()
        .next()
        .map(str::to_string)
}

/// L'état connu ; au premier appel, lu sur le disque.
pub async fn state(loki_home: &Path) -> State {
    if let Some(s) = get() {
        return s;
    }
    let s = match bin(loki_home) {
        Some(b) => State::Ready(
            version(&b, &home(loki_home))
                .await
                .unwrap_or_else(|| "?".into()),
        ),
        None => State::Absent,
    };
    // Une installation a pu commencer pendant la lecture : elle l'emporte.
    let mut g = STATE.lock().unwrap_or_else(|e| e.into_inner());
    g.get_or_insert(s).clone()
}

/// Lance `script` sous `bash -c` avec `HOME`, au plus [`SLOW`] ; la sortie va
/// au journal. `Err` : la fin de la sortie d'erreur.
async fn run_logged(script: &str, home: &Path, what: &str) -> Result<(), String> {
    let _ = std::fs::create_dir_all(home);
    let mut cmd = Command::new("bash");
    cmd.arg("-c")
        .arg(script)
        .env("HOME", home)
        .current_dir(home)
        .stdin(Stdio::null())
        .kill_on_drop(true);
    let out = match tokio::time::timeout(SLOW, cmd.output()).await {
        Err(_) => return Err(format!("{what} : délai de {} s dépassé", SLOW.as_secs())),
        Ok(Err(e)) => return Err(format!("{what} : {e}")),
        Ok(Ok(o)) => o,
    };
    for l in String::from_utf8_lossy(&out.stdout)
        .lines()
        .chain(String::from_utf8_lossy(&out.stderr).lines())
    {
        if !l.trim().is_empty() {
            eprintln!("[harness] {what} : {l}");
        }
    }
    if out.status.success() {
        Ok(())
    } else {
        let err = String::from_utf8_lossy(&out.stderr);
        let tail: Vec<&str> = err.lines().filter(|l| !l.trim().is_empty()).collect();
        let tail = tail[tail.len().saturating_sub(3)..].join(" ");
        Err(format!("{what} : {} {tail}", out.status).trim().to_string())
    }
}

/// Installe Claude Code (installeur officiel). Rien si une installation ou
/// une mise à jour tourne déjà, ou s'il est déjà prêt.
pub async fn install(loki_home: PathBuf) {
    install_with(loki_home, &format!("curl -fsSL {INSTALL_URL} | bash")).await
}

async fn install_with(loki_home: PathBuf, script: &str) {
    let Ok(_w) = WORK.try_lock() else { return };
    match state(&loki_home).await {
        State::Absent | State::Failed(_) => {}
        _ => return,
    }
    set(State::Installing);
    eprintln!("[harness] installation de Claude Code ({INSTALL_URL})");
    let h = home(&loki_home);
    let r = run_logged(script, &h, "installation").await;
    let s = match (r, bin(&loki_home)) {
        (Ok(()), Some(b)) => State::Ready(version(&b, &h).await.unwrap_or_else(|| "?".into())),
        (Ok(()), None) => {
            State::Failed("installation : binaire introuvable après l'installeur".into())
        }
        (Err(e), _) => State::Failed(e),
    };
    eprintln!("[harness] Claude Code : {s:?}");
    set(s);
}

/// `claude update` : rien si `LOKI_HARNESS_AUTO_UPDATE=0`, si Claude Code
/// n'est pas installé ou si une installation tourne. Jamais bloquant.
pub async fn update(loki_home: PathBuf) {
    if !super::auto_update() {
        return;
    }
    let Ok(_w) = WORK.try_lock() else { return };
    let State::Ready(before) = state(&loki_home).await else {
        return;
    };
    let Some(b) = bin(&loki_home) else { return };
    set(State::Updating);
    let h = home(&loki_home);
    let script = format!(
        "'{}' update",
        b.display().to_string().replace('\'', r"'\''")
    );
    let r = run_logged(&script, &h, "mise à jour").await;
    let v = version(&b, &h).await.unwrap_or(before);
    match r {
        Ok(()) => eprintln!("[harness] Claude Code {v}"),
        // Une mise à jour ratée laisse la version en place utilisable.
        Err(e) => eprintln!("[harness] {e} (Claude Code {v} gardé)"),
    }
    set(State::Ready(v));
}

#[cfg(all(test, unix))]
mod tests {
    use super::*;

    fn fake_claude(loki_home: &Path, version: &str) {
        let dir = home(loki_home).join(".local/bin");
        std::fs::create_dir_all(&dir).unwrap();
        let p = dir.join("claude");
        std::fs::write(&p, format!("#!/bin/sh\nif [ \"$1\" = --version ]; then echo '{version} (Claude Code)'; fi\nif [ \"$1\" = update ]; then echo maj; fi\n")).unwrap();
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&p, std::fs::Permissions::from_mode(0o755)).unwrap();
    }

    // Un seul test : STATE est global au processus.
    #[tokio::test]
    async fn etats_installation_et_mise_a_jour() {
        let d = tempfile::tempdir().unwrap();
        let lh = d.path().to_path_buf();

        reset_state();
        assert_eq!(state(&lh).await, State::Absent);
        // Mise à jour sans installation : rien.
        update(lh.clone()).await;
        assert_eq!(state(&lh).await, State::Absent);

        // Installeur qui échoue.
        install_with(lh.clone(), "echo raté >&2; exit 3").await;
        match state(&lh).await {
            State::Failed(m) => assert!(m.contains("raté"), "{m}"),
            s => panic!("{s:?}"),
        }

        // Installeur qui réussit sans poser le binaire.
        install_with(lh.clone(), "true").await;
        assert!(matches!(state(&lh).await, State::Failed(m) if m.contains("introuvable")));

        // Installeur qui pose le binaire.
        let bin_dir = home(&lh).join(".local/bin");
        let script = format!(
            "mkdir -p '{d}' && printf '#!/bin/sh\\necho 9.9.9 \\(Claude Code\\)\\n' > '{d}/claude' && chmod +x '{d}/claude'",
            d = bin_dir.display()
        );
        install_with(lh.clone(), &script).await;
        assert_eq!(state(&lh).await, State::Ready("9.9.9".into()));
        // Déjà prêt : une nouvelle installation ne fait rien.
        install_with(lh.clone(), "exit 9").await;
        assert_eq!(state(&lh).await, State::Ready("9.9.9".into()));

        // Mise à jour : la version relue.
        fake_claude(&lh, "10.0.0");
        update(lh.clone()).await;
        assert_eq!(state(&lh).await, State::Ready("10.0.0".into()));

        // Lecture du disque au premier appel.
        reset_state();
        assert_eq!(state(&lh).await, State::Ready("10.0.0".into()));
        reset_state();
    }

    #[test]
    fn noms_des_etats() {
        assert_eq!(State::Absent.name(), "absent");
        assert_eq!(State::Ready("1".into()).name(), "ready");
        assert_eq!(State::Failed("x".into()).name(), "failed");
    }
}
