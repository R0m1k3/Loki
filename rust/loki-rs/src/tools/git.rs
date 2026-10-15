//! Outils git du mode Code — port de `code_git.go` : `git_status`,
//! `git_diff`, `git_clone`. Sortie bornée, jamais d'invite interactive, et un
//! clone qui atterrit TOUJOURS dans le dossier de la discussion.

use std::io::Read;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::{Arc, LazyLock, Mutex};
use std::time::Duration;

use regex::Regex;
use serde_json::{Map, Value};

use super::files::arg_str;
use super::policy::clean;

/// Sortie gardée (sa fin).
const GIT_MAX_OUTPUT: usize = 8000;
const STATUS_TIMEOUT: Duration = Duration::from_secs(20);
const DIFF_TIMEOUT: Duration = Duration::from_secs(30);
const CLONE_TIMEOUT: Duration = Duration::from_secs(5 * 60);

/// `str` du Go : chaîne telle quelle, nombre sans décimales, rien → "".
fn go_str(v: Option<&Value>) -> String {
    match v {
        None | Some(Value::Null) => String::new(),
        Some(Value::String(s)) => s.clone(),
        Some(Value::Number(n)) => match n.as_f64() {
            Some(f) => format!("{f:.0}"),
            None => n.to_string(),
        },
        Some(other) => other.to_string(),
    }
}

/// `exec.LookPath` : le programme est-il dans le PATH ?
fn look_path(name: &str) -> bool {
    let Some(path) = std::env::var_os("PATH") else {
        return false;
    };
    let exts: Vec<String> = if cfg!(windows) {
        std::env::var("PATHEXT")
            .unwrap_or_else(|_| ".EXE;.CMD;.BAT;.COM".into())
            .split(';')
            .map(str::to_string)
            .collect()
    } else {
        vec![String::new()]
    };
    std::env::split_paths(&path).any(|dir| {
        exts.iter().any(|e| {
            let p = dir.join(format!("{name}{e}"));
            p.is_file() && is_executable(&p)
        })
    })
}

#[cfg(unix)]
fn is_executable(p: &Path) -> bool {
    use std::os::unix::fs::PermissionsExt;
    std::fs::metadata(p).is_ok_and(|m| m.permissions().mode() & 0o111 != 0)
}

#[cfg(not(unix))]
fn is_executable(_p: &Path) -> bool {
    true
}

/// `(*exec.ExitError).Error()` : « exit status N », « signal: killed ».
fn exit_text(st: &std::process::ExitStatus) -> String {
    if let Some(c) = st.code() {
        return format!("exit status {c}");
    }
    #[cfg(unix)]
    {
        use std::os::unix::process::ExitStatusExt;
        if let Some(sig) = st.signal() {
            let name = match sig {
                2 => "interrupt".to_string(),
                9 => "killed".to_string(),
                15 => "terminated".to_string(),
                n => format!("signal {n}"),
            };
            return format!("signal: {name}");
        }
    }
    st.to_string()
}

/// La fin d'un texte, `n` caractères au plus (`tailRunes`).
fn tail_chars(s: &str, n: usize) -> &str {
    let count = s.chars().count();
    if count <= n {
        return s;
    }
    let start = s.char_indices().nth(count - n).map_or(0, |(i, _)| i);
    &s[start..]
}

/// `runGitIn` : git dans `dir`, sorties mêlées (comme `CombinedOutput`),
/// délai borné, jamais d'invite (mot de passe, hôte inconnu).
pub async fn run_git_in<S: AsRef<str>>(dir: &Path, timeout: Duration, args: &[S]) -> String {
    let args: Vec<&str> = args.iter().map(AsRef::as_ref).collect();
    if !look_path("git") {
        return "[erreur] git n'est pas installé sur cette machine".into();
    }
    // Dossier absent : le message de Go (exec, Dir).
    match std::fs::metadata(dir) {
        Err(e) => {
            return format!(
                "[erreur] chdir {}: {}",
                dir.display(),
                super::files::os_err_text(&e)
            );
        }
        Ok(m) if !m.is_dir() => {
            return format!("[erreur] chdir {}: not a directory", dir.display());
        }
        Ok(_) => {}
    }
    let (reader, writer) = match std::io::pipe() {
        Ok(p) => p,
        Err(e) => return format!("[erreur] {e}"),
    };
    let writer2 = match writer.try_clone() {
        Ok(w) => w,
        Err(e) => return format!("[erreur] {e}"),
    };
    let mut cmd = tokio::process::Command::new("git");
    cmd.args(&args)
        .current_dir(dir)
        .stdin(Stdio::null())
        .stdout(writer)
        .stderr(writer2)
        .kill_on_drop(true)
        .env("GIT_TERMINAL_PROMPT", "0")
        .env("GIT_SSH_COMMAND", "ssh -oBatchMode=yes")
        .env_remove(crate::auth::INTERNAL_ENV);
    let child = cmd.spawn();
    // Les extrémités d'écriture gardées par la commande : fermées, sinon la
    // lecture n'aurait jamais de fin.
    drop(cmd);
    let mut child = match child {
        Ok(c) => c,
        Err(e) => return format!("[erreur] {e}"),
    };
    let buf = Arc::new(Mutex::new(Vec::new()));
    let b = buf.clone();
    let read = tokio::task::spawn_blocking(move || {
        let mut r = reader;
        let mut chunk = [0u8; 8192];
        while let Ok(n) = r.read(&mut chunk) {
            if n == 0 {
                break;
            }
            b.lock()
                .unwrap_or_else(|e| e.into_inner())
                .extend_from_slice(&chunk[..n]);
        }
    });
    let status = match tokio::time::timeout(timeout, child.wait()).await {
        Ok(st) => st,
        Err(_) => {
            let _ = child.start_kill();
            let _ = child.wait().await;
            return format!("[timeout] git {}", args.join(" "));
        }
    };
    // Sortie fermée par git ; un petit-enfant qui la tiendrait encore ne
    // bloque pas le tour.
    let _ = tokio::time::timeout(Duration::from_secs(2), read).await;
    let out = String::from_utf8_lossy(&buf.lock().unwrap_or_else(|e| e.into_inner())).into_owned();
    let mut s = tail_chars(out.trim(), GIT_MAX_OUTPUT).to_string();
    match status {
        Ok(st) if st.success() => {}
        Ok(st) => {
            if s.is_empty() {
                s = exit_text(&st);
            }
            return format!("[erreur] {s}");
        }
        Err(e) => return format!("[erreur] {e}"),
    }
    if s.is_empty() {
        s = "(aucune sortie)".into();
    }
    s
}

/// `gitRepoDir` : le sous-dossier demandé, la racine si c'est un dépôt,
/// sinon l'unique sous-dossier qui en est un (plusieurs : à préciser).
pub fn git_repo_dir(base: &Path, dir: &str) -> Result<PathBuf, String> {
    let dir = dir.trim();
    if !dir.is_empty() {
        if dir.contains("..") || Path::new(dir).is_absolute() {
            return Err(format!("dossier invalide : {dir}"));
        }
        return Ok(clean(&base.join(dir)));
    }
    if base.join(".git").exists() {
        return Ok(base.to_path_buf());
    }
    let mut repos: Vec<String> = std::fs::read_dir(base)
        .map(|rd| {
            rd.filter_map(Result::ok)
                .filter(|e| e.file_type().is_ok_and(|t| t.is_dir()))
                .filter(|e| e.path().join(".git").exists())
                .map(|e| e.file_name().to_string_lossy().into_owned())
                .collect()
        })
        .unwrap_or_default();
    repos.sort();
    match repos.len() {
        0 => Ok(base.to_path_buf()), // git dira « not a git repository »
        1 => Ok(base.join(&repos[0])),
        _ => Err(format!(
            "plusieurs dépôts ici ({}) : précise dir",
            repos.join(", ")
        )),
    }
}

/// `git_status`.
pub async fn tool_git_status(cwd: &Path, args: &Map<String, Value>) -> String {
    match git_repo_dir(cwd, &go_str(args.get("dir"))) {
        Ok(dir) => {
            run_git_in(
                &dir,
                STATUS_TIMEOUT,
                &["status", "--porcelain=v1", "--branch"],
            )
            .await
        }
        Err(e) => format!("[erreur] {e}"),
    }
}

/// `git_diff` : index et arbre de travail ensemble (`diff HEAD`) ; dépôt
/// sans commit : le diff simple.
pub async fn tool_git_diff(cwd: &Path, args: &Map<String, Value>) -> String {
    let dir = match git_repo_dir(cwd, &go_str(args.get("dir"))) {
        Ok(d) => d,
        Err(e) => return format!("[erreur] {e}"),
    };
    let file = arg_str(args, "file");
    let with_file = |head: &[&str]| -> Vec<String> {
        let mut a: Vec<String> = head.iter().map(|s| s.to_string()).collect();
        if !file.trim().is_empty() {
            a.extend(["--".to_string(), file.to_string()]);
        }
        a
    };
    let out = run_git_in(&dir, DIFF_TIMEOUT, &with_file(&["diff", "HEAD"])).await;
    if out.starts_with("[erreur]") && out.contains("HEAD") {
        return run_git_in(&dir, DIFF_TIMEOUT, &with_file(&["diff"])).await;
    }
    out
}

/// https:// ou git@hôte: — ni chemin local ni protocole exotique (file://,
/// ext:: qui exécute une commande).
static GIT_URL: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"^(https://[A-Za-z0-9_.\-]+(:[0-9]+)?/|git@[A-Za-z0-9_.\-]+:)[A-Za-z0-9_.\-~/]+$")
        .expect("motif d'URL git")
});

/// `filepath.Base`.
fn go_base(p: &str) -> String {
    let seps: &[char] = if cfg!(windows) { &['/', '\\'] } else { &['/'] };
    let t = p.trim_end_matches(seps);
    if t.is_empty() {
        return if p.is_empty() { ".".into() } else { "/".into() };
    }
    t.rsplit(seps).next().unwrap_or(t).to_string()
}

/// `git_clone` : dans le dossier de la discussion, quoi qu'il arrive.
pub async fn tool_git_clone(cwd: &Path, args: &Map<String, Value>) -> String {
    let url = arg_str(args, "url").trim();
    let url = format!("{}.git", url.strip_suffix(".git").unwrap_or(url).trim());
    let bare = url.strip_suffix(".git").unwrap_or(&url);
    if !GIT_URL.is_match(bare) && !GIT_URL.is_match(&url) {
        return "[erreur] URL refusée — seuls https:// et git@hôte: sont acceptés".into();
    }
    let mut dir = arg_str(args, "dir").trim().to_string();
    if dir.is_empty() {
        dir = go_base(bare);
    }
    if dir.contains("..") || Path::new(&dir).is_absolute() {
        return "[erreur] dossier cible invalide".into();
    }
    let target = clean(&cwd.join(&dir));
    if std::fs::metadata(&target).is_ok() {
        return format!("[erreur] le dossier {dir} existe déjà");
    }
    let target_s = target.to_string_lossy().into_owned();
    let out = run_git_in(cwd, CLONE_TIMEOUT, &["clone", "--", &url, &target_s]).await;
    if out.starts_with("[erreur]") || out.starts_with("[timeout]") {
        return out;
    }
    format!("[ok] cloné dans {dir}/\n{out}")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn base_et_str_comme_le_go() {
        assert_eq!(go_base("https://h/u/depot"), "depot");
        assert_eq!(go_base("git@h:u/depot"), "depot");
        assert_eq!(go_base("a/"), "a");
        assert_eq!(go_base(""), ".");
        assert_eq!(go_str(Some(&serde_json::json!(3.0))), "3");
        assert_eq!(go_str(None), "");
    }

    #[test]
    fn urls_acceptees() {
        for u in [
            "https://github.com/a/b",
            "https://h:8443/a/b.git",
            "git@github.com:a/b.git",
        ] {
            assert!(GIT_URL.is_match(u), "{u}");
        }
        for u in [
            "file:///etc",
            "ext::sh -c id",
            "/srv/depot",
            "https://h/a b",
            "https://hé/a",
        ] {
            assert!(!GIT_URL.is_match(u), "{u}");
        }
    }

    #[test]
    fn depot_trouve() {
        let d = tempfile::tempdir().unwrap();
        let b = d.path();
        assert_eq!(git_repo_dir(b, "").unwrap(), b);
        std::fs::create_dir_all(b.join("un/.git")).unwrap();
        assert_eq!(git_repo_dir(b, "").unwrap(), b.join("un"));
        std::fs::create_dir_all(b.join("deux/.git")).unwrap();
        assert_eq!(
            git_repo_dir(b, "").unwrap_err(),
            "plusieurs dépôts ici (deux, un) : précise dir"
        );
        assert_eq!(git_repo_dir(b, "deux").unwrap(), b.join("deux"));
        assert!(git_repo_dir(b, "../x").is_err() && git_repo_dir(b, "/x").is_err());
    }
}
