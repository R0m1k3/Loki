//! Emplacements de Loki (port de `sys_paths.go`) : ce que sert
//! `GET /api/paths` au bloc « Emplacements » de l'UI.
//!
//! « Où sont mes fichiers ? » — un binaire unique qui s'auto-installe rend les
//! emplacements invisibles : on ne sait plus si le binaire lancé EST
//! l'application ou une simple copie, ni où atterrissent la config, les
//! modèles et les fichiers de l'agent. La réponse est centralisée ici.
//!
//! Ce que le Go lit dans son environnement est passé en paramètre : `$LOKI_HOME`
//! (`home`), le binaire en cours (`go_exe` : tant que la migration n'est pas
//! finie, « binaire en cours » désigne le serveur Go supervisé), le projet
//! actif (bucket `state`, clé `active_project`) et `$LOKI_WORKSPACE`.
//!
//! Le module porte aussi les équivalents de `path/filepath` dont les ports
//! ont besoin pour produire EXACTEMENT les mêmes chaînes que le Go
//! (`Clean`, `Join`, `Dir`, `Abs`, `IsAbs`) et `baseName`.

use serde::Serialize;
use std::collections::HashMap;
use std::io;
use std::path::Path;
use std::sync::{Mutex, OnceLock};

/// Bucket `state`, clé du slug du projet actif (`stActiveProjectKey`).
pub const ST_ACTIVE_PROJECT_KEY: &str = "active_project";
/// Projet par défaut quand aucun n'est enregistré (`defaultProjectSlug`).
pub const DEFAULT_PROJECT_SLUG: &str = "generale";
/// Variable qui impose le dossier de travail de l'agent (`workspaceEnv`).
pub const WORKSPACE_ENV: &str = "LOKI_WORKSPACE";

/// Réponse de `GET /api/paths` (type `lokiPaths` du Go, mêmes noms JSON, même
/// ordre).
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct LokiPaths {
    /// LOKI_HOME : racine des données
    #[serde(rename = "home")]
    pub home: String,
    /// loki.db : config, préférences, conversation, clés
    #[serde(rename = "database")]
    pub database: String,
    /// binaire en cours d'exécution (le serveur Go pendant la migration)
    #[serde(rename = "exe")]
    pub exe: String,
    /// binaire installé (peut différer de `exe`)
    #[serde(rename = "installed")]
    pub installed: String,
    /// dossier de travail du mode agent
    #[serde(rename = "workspace")]
    pub workspace: String,
    #[serde(rename = "memory")]
    pub memory: String,
    /// scripts durables de l'agent (hors workspace jetable)
    #[serde(rename = "scripts")]
    pub scripts: String,
    #[serde(rename = "presets")]
    pub presets: String,
    #[serde(rename = "models")]
    pub models: String,
    #[serde(rename = "backends")]
    pub backends: String,
}

/// `currentPaths()` du Go.
///
/// - `home` : `$LOKI_HOME` (rendu tel quel, comme `LokiHome()`) ;
/// - `go_exe` : binaire Go supervisé (équivalent de `os.Executable()`),
///   symlinks résolus s'il existe ;
/// - `active_project` : valeur brute de `state/active_project` (vide ou
///   absente → projet « generale ») ;
/// - `workspace_env` : `$LOKI_WORKSPACE`, s'il est défini.
///
/// Fait des entrées/sorties (création et sonde du dossier de travail, comme
/// `agentWorkspace`) : à appeler hors du runtime async (`spawn_blocking`).
pub fn current_paths(
    home: &Path,
    go_exe: &Path,
    active_project: &str,
    workspace_env: Option<&str>,
) -> LokiPaths {
    let h = home.to_string_lossy().into_owned();
    let exe = eval_symlinks(go_exe).unwrap_or_else(|| go_exe.to_string_lossy().into_owned());
    LokiPaths {
        home: h.clone(),
        database: go_join(&[&h, "loki.db"]),
        exe,
        installed: installed_exe_path(home),
        workspace: agent_workspace(home, workspace_env),
        memory: project_memory_dir(home, active_project),
        scripts: go_join(&[&h, "scripts"]),
        presets: go_join(&[&h, "presets"]),
        models: go_join(&[&h, "models"]),
        backends: go_join(&[&h, "backends"]),
    }
}

/// `installedExePath()` : sous Unix les installateurs posent
/// `/usr/local/bin/loki` ; sous Windows, `LOKI_HOME\bin\loki.exe`.
pub fn installed_exe_path(home: &Path) -> String {
    if cfg!(windows) {
        go_join(&[&home.to_string_lossy(), "bin", "loki.exe"])
    } else {
        "/usr/local/bin/loki".to_string()
    }
}

/// `memoryDir()` = `projectMemoryDir(activeProjectSlug())` : dossier mémoire
/// du projet actif. `active_project` est la valeur stockée, nettoyée ici comme
/// dans `activeProjectSlug` (espaces retirés, vide → « generale »).
pub fn project_memory_dir(home: &Path, active_project: &str) -> String {
    let mut slug = active_project.trim();
    if slug.is_empty() {
        slug = DEFAULT_PROJECT_SLUG;
    }
    go_join(&[&home.to_string_lossy(), "memory", slug])
}

/// `workspaceCandidates()` : `$LOKI_WORKSPACE`, `LOKI_HOME/workspace`,
/// `~/loki/workspace`, puis `<tmp>/loki-workspace`.
pub fn workspace_candidates(home: &Path, workspace_env: Option<&str>) -> Vec<String> {
    let mut c = Vec::new();
    if let Some(v) = workspace_env.map(str::trim).filter(|v| !v.is_empty()) {
        c.push(v.to_string());
    }
    c.push(go_join(&[&home.to_string_lossy(), "workspace"]));
    if let Some(h) = user_home_dir() {
        c.push(go_join(&[&h, "loki", "workspace"]));
    }
    c.push(go_join(&[
        &std::env::temp_dir().to_string_lossy(),
        "loki-workspace",
    ]));
    c
}

/// `agentWorkspace()` : premier candidat qu'on peut créer ET où l'on peut
/// écrire (sonde réelle : un dossier existant peut être en lecture seule),
/// sinon le répertoire courant. Calculé une fois par processus et par
/// entrée, comme le `sync.Once` du Go.
pub fn agent_workspace(home: &Path, workspace_env: Option<&str>) -> String {
    type Cache = Mutex<HashMap<(String, Option<String>), String>>;
    static CACHE: OnceLock<Cache> = OnceLock::new();
    let key = (
        home.to_string_lossy().into_owned(),
        workspace_env.map(str::to_string),
    );
    let cache = CACHE.get_or_init(Default::default);
    if let Some(v) = cache.lock().unwrap_or_else(|e| e.into_inner()).get(&key) {
        return v.clone();
    }
    let found = workspace_candidates(home, workspace_env)
        .into_iter()
        .filter(|d| !d.is_empty())
        .find(|d| mkdir_all(d).is_ok() && probe_writable(d))
        .unwrap_or_else(|| {
            // Dernier recours : le répertoire courant, comportement historique.
            std::env::current_dir()
                .map(|d| d.to_string_lossy().into_owned())
                .unwrap_or_default()
        });
    cache
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .entry(key)
        .or_insert(found)
        .clone()
}

fn mkdir_all(dir: &str) -> io::Result<()> {
    let mut b = std::fs::DirBuilder::new();
    b.recursive(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::DirBuilderExt;
        b.mode(0o755);
    }
    b.create(dir)
}

/// Équivalent de `os.CreateTemp(dir, ".probe-*")` suivi de sa suppression.
fn probe_writable(dir: &str) -> bool {
    for _ in 0..16 {
        let mut r = [0u8; 8];
        if getrandom::fill(&mut r).is_err() {
            return false;
        }
        let p = Path::new(dir).join(format!(".probe-{}", hex::encode(r)));
        match std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&p)
        {
            Ok(f) => {
                drop(f);
                let _ = std::fs::remove_file(&p);
                return true;
            }
            Err(e) if e.kind() == io::ErrorKind::AlreadyExists => continue,
            Err(_) => return false,
        }
    }
    false
}

/// `os.UserHomeDir()` : `$HOME` sous Unix, `%USERPROFILE%` sous Windows ;
/// vide = inconnu.
fn user_home_dir() -> Option<String> {
    let var = if cfg!(windows) { "USERPROFILE" } else { "HOME" };
    std::env::var(var).ok().filter(|h| !h.is_empty())
}

/// `filepath.EvalSymlinks` : chemin résolu, ou `None` s'il n'existe pas.
pub fn eval_symlinks(p: &Path) -> Option<String> {
    let r = std::fs::canonicalize(p).ok()?;
    let s = r.to_string_lossy().into_owned();
    // Windows : canonicalize rend un chemin « verbatim » (\\?\C:\…) que le Go
    // ne produit jamais.
    let s = if let Some(rest) = s.strip_prefix(r"\\?\UNC\") {
        format!(r"\\{rest}")
    } else if let Some(rest) = s.strip_prefix(r"\\?\") {
        rest.to_string()
    } else {
        s
    };
    Some(go_clean(&s))
}

// --- équivalents de path/filepath -----------------------------------------

/// Séparateur natif (`filepath.Separator`).
const SEP: u8 = if cfg!(windows) { b'\\' } else { b'/' };

fn is_sep(c: u8) -> bool {
    c == b'/' || (cfg!(windows) && c == b'\\')
}

fn from_slash(s: &str) -> String {
    if cfg!(windows) {
        s.replace('/', "\\")
    } else {
        s.to_string()
    }
}

/// Longueur du volume en tête du chemin (`C:` ou `\\hôte\partage`) ; toujours
/// 0 hors Windows.
pub fn volume_name_len(p: &str) -> usize {
    if !cfg!(windows) {
        return 0;
    }
    let b = p.as_bytes();
    let l = b.len();
    if l >= 2 && b[1] == b':' && b[0].is_ascii_alphabetic() {
        return 2;
    }
    if l >= 5 && is_sep(b[0]) && is_sep(b[1]) && !is_sep(b[2]) && b[2] != b'.' {
        let mut n = 3;
        while n < l - 1 {
            if is_sep(b[n]) {
                n += 1;
                if !is_sep(b[n]) {
                    if b[n] == b'.' {
                        break;
                    }
                    while n < l && !is_sep(b[n]) {
                        n += 1;
                    }
                    return n;
                }
                break;
            }
            n += 1;
        }
    }
    0
}

/// `filepath.Clean` : forme lexicale la plus courte (séparateurs répétés,
/// `.` et `..` résolus sans toucher au disque).
pub fn go_clean(path: &str) -> String {
    let vol_len = volume_name_len(path);
    let vol = &path[..vol_len];
    let rest = &path.as_bytes()[vol_len..];
    if rest.is_empty() {
        let b = path.as_bytes();
        if vol_len > 1 && is_sep(b[0]) && is_sep(b[1]) {
            return from_slash(path); // UNC
        }
        return format!("{path}.");
    }
    let rooted = is_sep(rest[0]);
    let n = rest.len();
    let mut out: Vec<u8> = Vec::with_capacity(n);
    let (mut r, mut dotdot) = (0usize, 0usize);
    if rooted {
        out.push(SEP);
        r = 1;
        dotdot = 1;
    }
    while r < n {
        if is_sep(rest[r]) || (rest[r] == b'.' && (r + 1 == n || is_sep(rest[r + 1]))) {
            // séparateur vide ou élément « . »
            r += 1;
        } else if rest[r] == b'.'
            && r + 1 < n
            && rest[r + 1] == b'.'
            && (r + 2 == n || is_sep(rest[r + 2]))
        {
            r += 2;
            if out.len() > dotdot {
                let mut w = out.len() - 1;
                while w > dotdot && !is_sep(out[w]) {
                    w -= 1;
                }
                out.truncate(w);
            } else if !rooted {
                if !out.is_empty() {
                    out.push(SEP);
                }
                out.extend_from_slice(b"..");
                dotdot = out.len();
            }
        } else {
            if (rooted && out.len() != 1) || (!rooted && !out.is_empty()) {
                out.push(SEP);
            }
            while r < n && !is_sep(rest[r]) {
                out.push(rest[r]);
                r += 1;
            }
        }
    }
    if out.is_empty() {
        out.push(b'.');
    }
    from_slash(&format!("{vol}{}", String::from_utf8_lossy(&out)))
}

/// `filepath.Join` : éléments vides en tête ignorés, résultat nettoyé ; ""
/// si tout est vide.
pub fn go_join(elems: &[&str]) -> String {
    match elems.iter().position(|e| !e.is_empty()) {
        Some(i) => go_clean(&elems[i..].join(&(SEP as char).to_string())),
        None => String::new(),
    }
}

/// `filepath.Dir` : tout sauf le dernier élément, nettoyé.
pub fn go_dir(path: &str) -> String {
    let vol_len = volume_name_len(path);
    let b = path.as_bytes();
    let mut i = b.len();
    while i > vol_len && !is_sep(b[i - 1]) {
        i -= 1;
    }
    let dir = go_clean(&path[vol_len..i]);
    if dir == "." && vol_len > 2 {
        return path[..vol_len].to_string(); // UNC
    }
    format!("{}{dir}", &path[..vol_len])
}

/// `filepath.IsAbs`.
pub fn go_is_abs(path: &str) -> bool {
    if !cfg!(windows) {
        return path.starts_with('/');
    }
    let l = volume_name_len(path);
    if l == 0 {
        return false;
    }
    let b = path.as_bytes();
    if is_sep(b[0]) && is_sep(b[1]) {
        return true;
    }
    b.get(l).is_some_and(|&c| is_sep(c))
}

/// `filepath.Abs` : chemin absolu nettoyé ; `None` si le répertoire courant
/// est illisible.
pub fn go_abs(path: &str) -> Option<String> {
    if go_is_abs(path) && !cfg!(windows) {
        return Some(go_clean(path));
    }
    let p = if path.is_empty() { "." } else { path };
    let abs = std::path::absolute(p).ok()?;
    Some(go_clean(&abs.to_string_lossy()))
}

/// `baseName` du Go : nom de fichier d'un chemin en acceptant les DEUX
/// séparateurs (un config.env écrit sous Windows peut être relu sous Linux).
pub fn base_name(p: &str) -> String {
    let p = p.trim().trim_end_matches(['/', '\\']);
    match p.rfind(['/', '\\']) {
        Some(i) => p[i + 1..].to_string(),
        None => p.to_string(),
    }
}

/// Message d'une `*os.PathError` du Go : « op chemin: raison ». La raison
/// suit la forme des `syscall.Errno` du Go sous Unix (minuscule en tête, sans
/// le « (os error N) » de Rust).
pub fn os_err(op: &str, path: &str, e: &io::Error) -> String {
    let mut msg = e.to_string();
    if let Some(i) = msg.rfind(" (os error ") {
        msg.truncate(i);
    }
    if cfg!(unix) {
        let mut c = msg.chars();
        if let Some(f) = c.next() {
            msg = f.to_lowercase().chain(c).collect();
        }
    }
    format!("{op} {path}: {msg}")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn json_identique_au_go() {
        let p = LokiPaths {
            home: "h".into(),
            database: "d".into(),
            exe: "e".into(),
            installed: "i".into(),
            workspace: "w".into(),
            memory: "m".into(),
            scripts: "s".into(),
            presets: "p".into(),
            models: "mo".into(),
            backends: "b".into(),
        };
        assert_eq!(
            serde_json::to_string(&p).unwrap(),
            r#"{"home":"h","database":"d","exe":"e","installed":"i","workspace":"w","memory":"m","scripts":"s","presets":"p","models":"mo","backends":"b"}"#
        );
    }

    #[test]
    fn chemins_derives_de_home() {
        let home = tempfile::tempdir().unwrap();
        let h = home.path().to_string_lossy().into_owned();
        let exe = home.path().join("loki-go");
        std::fs::write(&exe, b"x").unwrap();
        let ws = home.path().join("ws-impose");
        let ws_s = ws.to_string_lossy().into_owned();
        let p = current_paths(home.path(), &exe, "  projet-x \n", Some(&ws_s));
        assert_eq!(p.home, h);
        assert_eq!(p.database, go_join(&[&h, "loki.db"]));
        assert_eq!(p.models, go_join(&[&h, "models"]));
        assert_eq!(p.presets, go_join(&[&h, "presets"]));
        assert_eq!(p.scripts, go_join(&[&h, "scripts"]));
        assert_eq!(p.backends, go_join(&[&h, "backends"]));
        assert_eq!(p.memory, go_join(&[&h, "memory", "projet-x"]));
        assert_eq!(p.workspace, ws_s);
        assert!(ws.is_dir(), "dossier de travail non créé");
        assert_eq!(
            std::fs::read_dir(&ws).unwrap().count(),
            0,
            "sonde non supprimée"
        );
        assert!(p.exe.ends_with("loki-go"), "{}", p.exe);
        assert!(go_is_abs(&p.exe), "{}", p.exe);
        if cfg!(windows) {
            assert_eq!(p.installed, go_join(&[&h, "bin", "loki.exe"]));
        } else {
            assert_eq!(p.installed, "/usr/local/bin/loki");
        }
        // projet absent → « generale » ; binaire absent → gardé tel quel
        let absent = home.path().join("absent");
        let q = current_paths(home.path(), &absent, "", None);
        assert_eq!(q.memory, go_join(&[&h, "memory", "generale"]));
        assert_eq!(q.exe, absent.to_string_lossy());
        assert_eq!(q.workspace, go_join(&[&h, "workspace"]));
    }

    #[test]
    fn candidats_dans_l_ordre() {
        let c = workspace_candidates(Path::new("/srv/loki"), Some("  "));
        assert_eq!(c[0], go_join(&["/srv/loki", "workspace"]));
        assert!(c.last().unwrap().ends_with("loki-workspace"));
        let c = workspace_candidates(Path::new("/srv/loki"), Some(" /x/ws "));
        assert_eq!(c[0], "/x/ws");
    }

    #[test]
    fn base_name_deux_separateurs() {
        assert_eq!(base_name(r"C:\models\x.gguf"), "x.gguf");
        assert_eq!(base_name("/m/y.gguf/ "), "y.gguf");
        assert_eq!(base_name("z.gguf"), "z.gguf");
        assert_eq!(base_name(""), "");
    }

    #[cfg(unix)]
    #[test]
    fn clean_comme_go() {
        for (i, o) in [
            ("", "."),
            ("abc", "abc"),
            ("abc/def/", "abc/def"),
            ("a//b", "a/b"),
            ("/../abc", "/abc"),
            ("abc/./../def", "def"),
            ("../../abc", "../../abc"),
            ("abc/../../..", "../.."),
            ("/", "/"),
            ("//abc//", "/abc"),
        ] {
            assert_eq!(go_clean(i), o, "Clean({i:?})");
        }
        assert_eq!(go_dir("/a/b/c.gguf"), "/a/b");
        assert_eq!(go_dir("c.gguf"), ".");
        assert_eq!(go_dir("/c.gguf"), "/");
        assert_eq!(go_join(&["", "a", "", "b/"]), "a/b");
        assert_eq!(go_join(&["", ""]), "");
        assert!(go_is_abs("/x") && !go_is_abs("x"));
        assert_eq!(go_abs("/a/../b").as_deref(), Some("/b"));
    }

    #[cfg(windows)]
    #[test]
    fn clean_comme_go() {
        for (i, o) in [
            ("", "."),
            ("abc/def/", r"abc\def"),
            (r"C:\a\..\b", r"C:\b"),
            ("C:", "C:."),
            (r"C:\", r"C:\"),
            (r"\\srv\share", r"\\srv\share"),
            (r"\\srv\share\x\..", r"\\srv\share\"),
            (r"c:/a//b/./", r"c:\a\b"),
        ] {
            assert_eq!(go_clean(i), o, "Clean({i:?})");
        }
        assert_eq!(go_dir(r"C:\m\x.gguf"), r"C:\m");
        assert_eq!(go_dir(r"\\srv\share\x.gguf"), r"\\srv\share\");
        assert!(go_is_abs(r"C:\x") && go_is_abs(r"\\srv\share") && !go_is_abs(r"\x"));
        assert!(!go_is_abs("C:x"));
    }
}
