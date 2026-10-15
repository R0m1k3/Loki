//! Chemins et garde-fous des outils de l'agent — port de `code_policy.go`,
//! `chat_workspace.go` (`resolveAgentPath`) et `chat_scripts.go` (dossier
//! mémoire réservé à ses outils).
//!
//! - [`dangerous_command`] : commandes CATASTROPHIQUES refusées (effacement
//!   de disque, arrêt machine, fork bomb), mode chat compris ;
//! - [`code_path_allowed`] : en mode Code, les chemins des outils fichiers
//!   sont bornés au dossier de la discussion, liens symboliques résolus ;
//! - [`guard_tool_only_path`] / [`guard_tool_only_command`] : le dossier
//!   mémoire n'est jamais touché par write/edit/bash, seulement par mem_*.

use std::path::{Component, Path, PathBuf};
use std::sync::LazyLock;

use regex::Regex;

/// `filepath.Clean` : forme lexicale la plus courte (`.` et `..` résolus sans
/// toucher au disque, séparateurs doublés retirés). Vide → `.`.
pub fn clean(p: &Path) -> PathBuf {
    let mut out: Vec<Component> = Vec::new();
    let mut rooted = false;
    for c in p.components() {
        match c {
            Component::Prefix(_) => out.push(c),
            Component::RootDir => {
                rooted = true;
                out.push(c);
            }
            Component::CurDir => {}
            Component::ParentDir => match out.last() {
                Some(Component::Normal(_)) => {
                    out.pop();
                }
                // `/..` reste `/`.
                _ if rooted => {}
                _ => out.push(c),
            },
            Component::Normal(_) => out.push(c),
        }
    }
    if out.is_empty() {
        return PathBuf::from(".");
    }
    out.iter().collect()
}

/// `os.UserHomeDir` : `$HOME` (`%USERPROFILE%` sous Windows).
fn home_dir() -> Option<PathBuf> {
    let var = if cfg!(windows) { "USERPROFILE" } else { "HOME" };
    std::env::var_os(var)
        .filter(|h| !h.is_empty())
        .map(PathBuf::from)
}

/// `resolveAgentPath` : chemin fourni par le modèle. Absolu → inchangé ;
/// `~/x` → dans le home ; relatif → dans le dossier de travail `cwd`.
pub fn resolve_agent_path(cwd: &Path, p: &str) -> PathBuf {
    let p = p.trim();
    if p.is_empty() {
        return PathBuf::new();
    }
    if (p == "~" || p.starts_with("~/") || p.starts_with("~\\"))
        && let Some(home) = home_dir()
    {
        let rest = &p[1..];
        let rest = rest.strip_prefix('/').unwrap_or(rest);
        return clean(&home.join(from_slash(rest)));
    }
    let path = Path::new(p);
    if path.is_absolute() {
        return path.to_path_buf();
    }
    clean(&cwd.join(from_slash(p)))
}

/// `filepath.FromSlash`.
fn from_slash(p: &str) -> PathBuf {
    if cfg!(windows) {
        PathBuf::from(p.replace('/', "\\"))
    } else {
        PathBuf::from(p)
    }
}

/// `filepath.ToSlash`.
pub fn to_slash(p: &Path) -> String {
    let s = p.to_string_lossy();
    if cfg!(windows) {
        s.replace('\\', "/")
    } else {
        s.into_owned()
    }
}

/// `filepath.EvalSymlinks`, à défaut le chemin tel quel.
fn eval_symlinks(p: &Path) -> PathBuf {
    match std::fs::canonicalize(p) {
        Ok(c) => strip_verbatim(c),
        Err(_) => p.to_path_buf(),
    }
}

/// `canonicalize` rend `\\?\C:\…` sous Windows ; `EvalSymlinks` non.
fn strip_verbatim(p: PathBuf) -> PathBuf {
    if cfg!(windows)
        && let Some(s) = p.to_str()
        && let Some(rest) = s.strip_prefix(r"\\?\")
        && !rest.starts_with("UNC\\")
    {
        return PathBuf::from(rest);
    }
    p
}

/// `p` est `root` lui-même ou dedans (lexicalement, chemins nettoyés).
fn within(root: &Path, p: &Path) -> bool {
    clean(p).starts_with(clean(root))
}

/// `sameOrWithin` : liens symboliques résolus des deux côtés.
fn same_or_within(root: &Path, p: &Path) -> bool {
    within(&eval_symlinks(root), &eval_symlinks(p))
}

/// `codePathAllowed` : un chemin (déjà résolu) est-il DANS le dossier de la
/// discussion `ws` ? Le fichier peut ne pas exister encore (write) : c'est son
/// ancêtre existant le plus proche qui doit y être, liens résolus ; et le
/// chemin final ne doit pas en ressortir par des `..` lexicaux.
pub fn code_path_allowed(ws: &Path, abs: &Path) -> bool {
    if ws.as_os_str().is_empty() {
        return false;
    }
    let abs = clean(abs);
    let mut probe = abs.clone();
    loop {
        if std::fs::metadata(&probe).is_ok() {
            break;
        }
        match probe.parent() {
            Some(parent) if parent != probe => probe = parent.to_path_buf(),
            _ => break,
        }
    }
    same_or_within(ws, &probe) && within(ws, &abs)
}

/// `refusedPathResult` : refus d'un chemin hors du bac à sable du mode Code.
pub fn refused_path_result(path: &str) -> String {
    format!(
        "[refusé] {path} est hors du dossier de cette discussion. \
         En mode code, tous les fichiers vivent dans le dossier de travail — utilise des chemins relatifs."
    )
}

/// `dangerousPatterns` : motifs de commandes refusées, et pourquoi.
/// Volontairement ÉTROIT : la perte de données irréversible et l'arrêt de la
/// machine, pas la moindre écriture.
static DANGEROUS: LazyLock<Vec<(Regex, &'static str)>> = LazyLock::new(|| {
    let root = "efface la racine du disque ou le home";
    let disk = "écrit directement sur un périphérique disque";
    let drive = "efface la racine d'un lecteur";
    [
        (
            r#"(?i)\brm\s+(-[a-z]*r[a-z]*f[a-z]*|-[a-z]*f[a-z]*r[a-z]*)\s+("?/"?|~/?|/\*|[a-z]:\\?)\s*$"#,
            root,
        ),
        (
            r#"(?i)\brm\s+(-[a-z]*r[a-z]*f[a-z]*|-[a-z]*f[a-z]*r[a-z]*)\s+("?/"?|~/?|/\*|[a-z]:\\?)\s"#,
            root,
        ),
        (r"(?i)\bmkfs(\.[a-z0-9]+)?\b", "formate un système de fichiers"),
        (r"(?i)\bdd\b[^\n]*\bof=/dev/", disk),
        (r"(?i)>\s*/dev/sd[a-z]\b", disk),
        (
            r"(?i)(^|[;&|]\s*|\bsudo\s+)(shutdown|poweroff|reboot|halt)\b",
            "arrête ou redémarre la machine",
        ),
        (r":\(\)\s*\{\s*:\|:", "fork bomb"),
        (
            r"(?i)\bch(mod|own)\b[^\n]*-[a-z]*R[a-z]*\s+[^\s]+\s+/\s*$",
            "change récursivement les droits de la racine",
        ),
        (r"(?i)\b(rd|rmdir)\s+/s\b[^\n]*\s[a-z]:\\?\s*$", drive),
        (r"(?i)\bdel\s+(/[fsq]\s+)*[a-z]:\\\*", drive),
        (r"(?i)\bformat\s+[a-z]:", "formate un lecteur"),
    ]
    .into_iter()
    .map(|(re, why)| (Regex::new(re).expect("motif de commande dangereuse"), why))
    .collect()
});

/// `dangerousCommand` : la raison du refus, `None` si la commande passe.
pub fn dangerous_command(cmd: &str) -> Option<&'static str> {
    DANGEROUS
        .iter()
        .find(|(re, _)| re.is_match(cmd))
        .map(|(_, why)| *why)
}

/// `refusedCommandResult` : dire POURQUOI et QUOI FAIRE, sinon le modèle
/// réessaie en boucle avec des variantes.
pub fn refused_command_result(reason: &str) -> String {
    format!(
        "[refusé] Commande bloquée par la politique de sécurité : {reason}. \
         Ne la réessaie pas, même reformulée. Si l'utilisateur la veut vraiment, dis-lui de la lancer lui-même dans un terminal."
    )
}

/// `normPath` : absolu, nettoyé, en minuscules sous Windows.
fn norm_path(p: &Path) -> String {
    let abs = if p.is_absolute() {
        p.to_path_buf()
    } else {
        std::env::current_dir().unwrap_or_default().join(p)
    };
    let s = clean(&abs).to_string_lossy().into_owned();
    if cfg!(windows) { s.to_lowercase() } else { s }
}

/// `underDir` : `path` est `dir` ou dessous.
fn under_dir(path: &Path, dir: &Path) -> bool {
    let (a, b) = (norm_path(path), norm_path(dir));
    a == b || a.starts_with(&format!("{b}{}", std::path::MAIN_SEPARATOR))
}

const TOOL_ONLY_LABEL: &str = "les outils mem_* (mem_search/mem_read/mem_add/mem_edit/mem_delete)";

/// `guardToolOnlyPath` : refus d'un write/edit dans le dossier mémoire.
pub fn guard_tool_only_path(path: &Path, memory_dir: &Path) -> Option<String> {
    if memory_dir.as_os_str().is_empty() || !under_dir(path, memory_dir) {
        return None;
    }
    Some(format!(
        "[refusé] pas d'accès direct au dossier {}. Utilise {TOOL_ONLY_LABEL}.",
        memory_dir.display()
    ))
}

/// `guardToolOnlyCommand` : refus d'une commande qui MENTIONNE le dossier
/// mémoire, lecture comprise.
pub fn guard_tool_only_command(command: &str, memory_dir: &Path) -> Option<String> {
    if memory_dir.as_os_str().is_empty()
        || !command
            .to_lowercase()
            .contains(&norm_path(memory_dir).to_lowercase())
    {
        return None;
    }
    Some(format!(
        "[refusé] pas d'accès direct au dossier {} via le shell. Utilise {TOOL_ONLY_LABEL}.",
        memory_dir.display()
    ))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn nettoyage_comme_filepath_clean() {
        for (input, want) in [
            ("", "."),
            ("a/./b", "a/b"),
            ("a/../..", ".."),
            ("/../a", "/a"),
            ("a//b/", "a/b"),
            ("../../x/../y", "../../y"),
            ("/", "/"),
        ] {
            assert_eq!(clean(Path::new(input)), PathBuf::from(want), "{input:?}");
        }
    }

    #[cfg(unix)]
    #[test]
    fn chemins_de_l_agent() {
        let cwd = Path::new("/w/conv");
        assert_eq!(
            resolve_agent_path(cwd, " a/../b.txt "),
            PathBuf::from("/w/conv/b.txt")
        );
        assert_eq!(resolve_agent_path(cwd, "/etc/x"), PathBuf::from("/etc/x"));
        assert_eq!(resolve_agent_path(cwd, ""), PathBuf::new());
    }

    #[test]
    fn commandes_dangereuses() {
        for c in [
            "rm -rf /",
            "sudo rm -fr ~ ",
            "mkfs.ext4 /dev/sda1",
            "dd if=x of=/dev/sda",
            "ls; reboot",
            ":(){ :|:& };:",
            "format c:",
        ] {
            assert!(dangerous_command(c).is_some(), "{c}");
        }
        for c in [
            "rm -rf build",
            "grep reboot log.txt",
            "dd if=a of=b",
            "echo format",
        ] {
            assert!(dangerous_command(c).is_none(), "{c}");
        }
    }

    #[test]
    fn bac_a_sable_du_mode_code() {
        let ws = tempfile::tempdir().unwrap();
        let w = ws.path();
        std::fs::create_dir(w.join("src")).unwrap();
        assert!(code_path_allowed(w, &w.join("src/neuf/a.rs")));
        assert!(code_path_allowed(w, w));
        assert!(!code_path_allowed(w, &w.join("../dehors")));
        assert!(!code_path_allowed(w, Path::new("/etc/passwd")));
        assert!(!code_path_allowed(Path::new(""), &w.join("a")));
        #[cfg(unix)]
        {
            std::os::unix::fs::symlink("/etc", w.join("lien")).unwrap();
            assert!(!code_path_allowed(w, &w.join("lien/passwd")));
        }
    }

    #[test]
    fn dossier_memoire_reserve() {
        let mem = std::env::temp_dir().join("loki-mem-test");
        assert!(guard_tool_only_path(&mem.join("a.md"), &mem).is_some());
        assert!(guard_tool_only_path(&mem, &mem).is_some());
        assert!(guard_tool_only_path(&std::env::temp_dir().join("loki-mem-testx"), &mem).is_none());
        let cmd = format!("cat {}/a.md", mem.display());
        assert!(guard_tool_only_command(&cmd, &mem).is_some());
        assert!(guard_tool_only_command("ls", &mem).is_none());
        assert!(guard_tool_only_command("ls", Path::new("")).is_none());
    }
}
