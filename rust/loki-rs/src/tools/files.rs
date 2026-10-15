//! Outils fichiers de l'agent — port de `code_tools.go` (read, grep, glob,
//! ask) et de `fileWrite` / `fileEdit` (`chat_tools.go`).
//!
//! Fonctions synchrones (disque) : l'appelant les lance hors du runtime
//! (`spawn_blocking`) et se charge de ce qui est partagé avec le Go (suivi
//! « lu avant d'écrire », diagnostics LSP — voir `tools::exec_local`).

use std::collections::HashMap;
use std::fs;
use std::io::Read;
use std::path::{Path, PathBuf};
use std::sync::{Arc, LazyLock, Mutex};

use regex::Regex;
use serde_json::{Map, Value};

use super::policy::{
    code_path_allowed, guard_tool_only_path, refused_path_result, resolve_agent_path, to_slash,
};

/// Lignes par appel (offset/limit pour la suite).
pub const READ_MAX_LINES: usize = 1000;
pub const READ_DEF_LINES: usize = 400;
/// Une ligne minifiée ne doit pas manger le contexte.
const READ_MAX_LINE_LEN: usize = 500;
const GREP_MAX_HITS: usize = 60;
const GLOB_MAX_HITS: usize = 200;

/// `skipDirs` : dépendances et artefacts, jamais du code à lire.
const SKIP_DIRS: [&str; 11] = [
    ".git",
    "node_modules",
    "vendor",
    "dist",
    "build",
    "target",
    "__pycache__",
    ".venv",
    "venv",
    ".next",
    ".cache",
];

fn skip_dir(name: &str) -> bool {
    SKIP_DIRS.contains(&name)
}

/// Texte d'erreur du système à la manière de Go (`no such file or
/// directory`) : le message de l'OS, sans « (os error N) », minuscule en tête
/// hors Windows.
pub fn os_err_text(e: &std::io::Error) -> String {
    let s = e.to_string();
    let s = match s.rfind(" (os error ") {
        Some(i) => s[..i].to_string(),
        None => s,
    };
    if cfg!(windows) {
        return s;
    }
    let mut c = s.chars();
    match c.next() {
        Some(f) => f.to_lowercase().chain(c).collect(),
        None => s,
    }
}

/// `*PathError` de Go : « op chemin: texte ».
fn path_err(op: &str, p: &Path, e: &std::io::Error) -> String {
    format!("[erreur] {op} {}: {}", p.display(), os_err_text(e))
}

/// `os.ReadFile`, erreurs au format Go (`open …` / `read …`).
fn read_file(p: &Path) -> Result<Vec<u8>, String> {
    let mut f = fs::File::open(p).map_err(|e| path_err("open", p, &e))?;
    let mut b = Vec::new();
    f.read_to_end(&mut b).map_err(|e| path_err("read", p, &e))?;
    Ok(b)
}

/// `isBinary` : un NUL dans les premiers 8 Kio.
pub fn is_binary(b: &[u8]) -> bool {
    b[..b.len().min(8192)].contains(&0)
}

/// Ligne bornée à `READ_MAX_LINE_LEN` caractères (suivie de « … » si coupée).
fn cap_line(l: &str) -> String {
    match l.char_indices().nth(READ_MAX_LINE_LEN) {
        Some((i, _)) => format!("{}…", &l[..i]),
        None => l.to_string(),
    }
}

/// Entier positif d'un argument JSON (`args["x"].(float64)` puis `int(v)`).
pub fn arg_int(args: &Map<String, Value>, k: &str) -> Option<i64> {
    args.get(k).and_then(Value::as_f64).map(|v| v as i64)
}

pub fn arg_str<'a>(args: &'a Map<String, Value>, k: &str) -> &'a str {
    args.get(k).and_then(Value::as_str).unwrap_or("")
}

/// Ce qu'un outil fichier rend : le résultat, et le chemin LU à noter dans
/// le suivi (read réussi).
pub struct ReadOut {
    pub result: String,
    pub noted: Option<PathBuf>,
}

/// `toolRead` : fichier avec numéros de ligne.
pub fn tool_read(cwd: &Path, args: &Map<String, Value>, code: bool) -> ReadOut {
    let err = |result: String| ReadOut {
        result,
        noted: None,
    };
    let file = arg_str(args, "file");
    if file.trim().is_empty() {
        return err("[erreur] chemin vide".into());
    }
    let path = resolve_agent_path(cwd, file);
    if code && !code_path_allowed(cwd, &path) {
        return err(refused_path_result(file));
    }
    let offset = arg_int(args, "offset").filter(|v| *v > 0).unwrap_or(1) as usize;
    let limit = (arg_int(args, "limit")
        .filter(|v| *v > 0)
        .unwrap_or(READ_DEF_LINES as i64) as usize)
        .min(READ_MAX_LINES);
    let b = match read_file(&path) {
        Ok(b) => b,
        Err(e) => return err(e),
    };
    if is_binary(&b) {
        let size = fs::metadata(&path)
            .map(|m| m.len())
            .unwrap_or(b.len() as u64);
        return err(format!(
            "[binaire] {file} ({size} octets) — pas un fichier texte"
        ));
    }
    let text = String::from_utf8_lossy(&b);
    let lines: Vec<&str> = text.split('\n').collect();
    let mut total = lines.len();
    // Fichier fini par \n : la dernière entrée vide n'est pas une ligne.
    if total > 0 && lines[total - 1].is_empty() {
        total -= 1;
    }
    if offset > total {
        return err(format!("[erreur] offset {offset} > {total} lignes"));
    }
    let end = (offset - 1 + limit).min(total);
    let mut out = String::new();
    for (i, l) in lines.iter().enumerate().take(end).skip(offset - 1) {
        out.push_str(&format!("{}: {}\n", i + 1, cap_line(l)));
    }
    if end < total {
        out.push_str(&format!(
            "… ({} lignes restantes — relis avec offset={})\n",
            total - end,
            end + 1
        ));
    }
    ReadOut {
        result: out.trim_end_matches('\n').to_string(),
        noted: Some(path),
    }
}

/// Parcours à la `filepath.WalkDir` : ordre lexical, liens non suivis,
/// dossiers ignorés (`skipDirs`, la racine comprise). `visit` rend `false`
/// pour tout arrêter.
fn walk(root: &Path, visit: &mut dyn FnMut(&Path, &fs::Metadata) -> bool) {
    let Ok(meta) = fs::symlink_metadata(root) else {
        return;
    };
    walk_entry(root, &meta, visit);
}

fn walk_entry(
    path: &Path,
    meta: &fs::Metadata,
    visit: &mut dyn FnMut(&Path, &fs::Metadata) -> bool,
) -> bool {
    if !meta.is_dir() {
        return visit(path, meta);
    }
    if path
        .file_name()
        .and_then(|n| n.to_str())
        .is_some_and(skip_dir)
    {
        return true;
    }
    let Ok(rd) = fs::read_dir(path) else {
        return true; // dossier illisible : on continue ailleurs
    };
    let mut entries: Vec<(String, PathBuf)> = rd
        .filter_map(Result::ok)
        .map(|e| (e.file_name().to_string_lossy().into_owned(), e.path()))
        .collect();
    entries.sort();
    for (_, p) in entries {
        let Ok(m) = fs::symlink_metadata(&p) else {
            continue;
        };
        if !walk_entry(&p, &m, visit) {
            return false;
        }
    }
    true
}

/// Chemin relatif à la racine du parcours, en `/` (`.` pour la racine).
fn rel_slash(root: &Path, p: &Path) -> String {
    match p.strip_prefix(root) {
        Ok(r) if r.as_os_str().is_empty() => ".".into(),
        Ok(r) => to_slash(r),
        Err(_) => to_slash(p),
    }
}

/// `toolGrep` : motif regexp dans les fichiers.
pub fn tool_grep(cwd: &Path, args: &Map<String, Value>, code: bool) -> String {
    let pattern = arg_str(args, "pattern");
    if pattern.trim().is_empty() {
        return "[erreur] motif vide".into();
    }
    let re = match Regex::new(pattern) {
        Ok(re) => re,
        Err(e) => return format!("[erreur] motif invalide : {e}"),
    };
    let p = arg_str(args, "path");
    let root = if !p.trim().is_empty() && p != "." {
        resolve_agent_path(cwd, p)
    } else {
        cwd.to_path_buf()
    };
    if code && !code_path_allowed(cwd, &root) {
        return refused_path_result(&root.display().to_string());
    }
    let glob_pat = arg_str(args, "glob");
    let mut hits: Vec<String> = Vec::new();
    walk(&root, &mut |path, _| {
        if !glob_pat.is_empty() {
            let name = path
                .file_name()
                .map(|n| n.to_string_lossy().into_owned())
                .unwrap_or_default();
            if !go_match(glob_pat, &name).unwrap_or(false) {
                return true;
            }
        }
        if hits.len() >= GREP_MAX_HITS {
            return false;
        }
        let Ok(b) = fs::read(path) else {
            return true;
        };
        if is_binary(&b) {
            return true;
        }
        let rel = rel_slash(&root, path);
        let text = String::from_utf8_lossy(&b);
        for (i, line) in text.split('\n').enumerate() {
            if re.is_match(line) {
                hits.push(format!("{rel}:{}: {}", i + 1, cap_line(line).trim()));
                if hits.len() >= GREP_MAX_HITS {
                    return false;
                }
            }
        }
        true
    });
    if hits.is_empty() {
        return "[aucun résultat]".into();
    }
    let mut out = hits.join("\n");
    if hits.len() >= GREP_MAX_HITS {
        out.push_str(&format!(
            "\n… (plafond de {GREP_MAX_HITS} résultats atteint — affine le motif)"
        ));
    }
    out
}

/// `toolGlob` : fichiers d'un motif (`**` compris), plus récents d'abord.
pub fn tool_glob(cwd: &Path, args: &Map<String, Value>) -> String {
    let pattern = arg_str(args, "pattern");
    if pattern.trim().is_empty() {
        return "[erreur] motif vide".into();
    }
    let re = match glob_to_regex(pattern) {
        Ok(re) => re,
        Err(e) => return format!("[erreur] motif invalide : {e}"),
    };
    let mut hits: Vec<(String, std::time::SystemTime)> = Vec::new();
    walk(cwd, &mut |path, meta| {
        let rel = rel_slash(cwd, path);
        if re.is_match(&rel) {
            hits.push((rel, meta.modified().unwrap_or(std::time::UNIX_EPOCH)));
        }
        true
    });
    if hits.is_empty() {
        return "[aucun fichier]".into();
    }
    hits.sort_by_key(|h| std::cmp::Reverse(h.1));
    hits.truncate(GLOB_MAX_HITS);
    hits.into_iter().map(|h| h.0).collect::<Vec<_>>().join("\n")
}

/// `globToRegexp` : glob (avec `**`) → expression sur des chemins en `/`.
/// `**` = n'importe quelle profondeur, `*` = tout sauf `/`, `?` = un
/// caractère sauf `/`.
pub fn glob_to_regex(pattern: &str) -> Result<Regex, regex::Error> {
    let p = if cfg!(windows) {
        pattern.replace('\\', "/")
    } else {
        pattern.to_string()
    };
    let p = p.strip_prefix("./").unwrap_or(&p);
    let mut b = String::from("^");
    let mut rest = p;
    while let Some(c) = rest.chars().next() {
        if rest.starts_with("**/") {
            b.push_str("(?:[^/]+/)*");
            rest = &rest[3..];
            continue;
        }
        if rest.starts_with("**") {
            b.push_str(".*");
            rest = &rest[2..];
            continue;
        }
        match c {
            '*' => b.push_str("[^/]*"),
            '?' => b.push_str("[^/]"),
            '.' | '+' | '(' | ')' | '|' | '[' | ']' | '{' | '}' | '^' | '$' | '\\' => {
                b.push('\\');
                b.push(c);
            }
            _ => b.push(c),
        }
        rest = &rest[c.len_utf8()..];
    }
    b.push('$');
    Regex::new(&b)
}

/// `filepath.Match` de Go (`*`, `?`, `[classes]`, `\` d'échappement hors
/// Windows), sur un nom sans séparateur. `None` : motif mal formé.
pub fn go_match(pattern: &str, name: &str) -> Option<bool> {
    let sep = std::path::MAIN_SEPARATOR;
    let mut pattern = pattern;
    let mut name = name;
    'pat: while !pattern.is_empty() {
        let (star, chunk, rest) = scan_chunk(pattern);
        pattern = rest;
        if star && chunk.is_empty() {
            return Some(!name.contains(sep));
        }
        let (t, ok, err) = match_chunk(chunk, name);
        if ok && (t.is_empty() || !pattern.is_empty()) {
            name = t;
            continue;
        }
        if err {
            return None;
        }
        if star {
            for (i, ch) in name.char_indices() {
                if ch == sep {
                    break;
                }
                let skip = i + ch.len_utf8();
                // Go avance d'un OCTET ; sur du texte valide, les positions
                // intermédiaires d'un caractère ne peuvent rien faire correspondre.
                let (t, ok, err) = match_chunk(chunk, &name[skip..]);
                if ok {
                    if pattern.is_empty() && !t.is_empty() {
                        continue;
                    }
                    name = t;
                    continue 'pat;
                }
                if err {
                    return None;
                }
            }
        }
        while !pattern.is_empty() {
            let (_, chunk, rest) = scan_chunk(pattern);
            pattern = rest;
            if match_chunk(chunk, "").2 {
                return None;
            }
        }
        return Some(false);
    }
    Some(name.is_empty())
}

fn scan_chunk(mut pattern: &str) -> (bool, &str, &str) {
    let mut star = false;
    while let Some(r) = pattern.strip_prefix('*') {
        pattern = r;
        star = true;
    }
    let b = pattern.as_bytes();
    let mut inrange = false;
    let mut i = 0;
    while i < b.len() {
        match b[i] {
            b'\\' if !cfg!(windows) => {
                if i + 1 < b.len() {
                    i += 1;
                }
            }
            b'[' => inrange = true,
            b']' => inrange = false,
            b'*' if !inrange => break,
            _ => {}
        }
        i += 1;
    }
    (star, &pattern[..i], &pattern[i..])
}

/// `matchChunk` : (reste du nom, correspond ?, motif mal formé ?).
fn match_chunk<'a>(chunk: &str, s: &'a str) -> (&'a str, bool, bool) {
    let sep = std::path::MAIN_SEPARATOR;
    let mut chunk = chunk;
    let mut s = s;
    let mut failed = false;
    while let Some(c0) = chunk.chars().next() {
        if !failed && s.is_empty() {
            failed = true;
        }
        match c0 {
            '[' => {
                let mut r = '\0';
                if !failed {
                    let ch = s.chars().next().unwrap_or('\0');
                    r = ch;
                    s = &s[ch.len_utf8()..];
                }
                chunk = &chunk[1..];
                let mut negated = false;
                if let Some(rest) = chunk.strip_prefix('^') {
                    negated = true;
                    chunk = rest;
                }
                let mut matched = false;
                let mut nrange = 0;
                loop {
                    if nrange > 0
                        && let Some(rest) = chunk.strip_prefix(']')
                    {
                        chunk = rest;
                        break;
                    }
                    let Some((lo, rest)) = get_esc(chunk) else {
                        return ("", false, true);
                    };
                    chunk = rest;
                    let mut hi = lo;
                    if let Some(rest) = chunk.strip_prefix('-') {
                        let Some((h, rest)) = get_esc(rest) else {
                            return ("", false, true);
                        };
                        hi = h;
                        chunk = rest;
                    }
                    if lo <= r && r <= hi {
                        matched = true;
                    }
                    nrange += 1;
                }
                if matched == negated {
                    failed = true;
                }
            }
            '?' => {
                if !failed {
                    let ch = s.chars().next().unwrap_or('\0');
                    if ch == sep {
                        failed = true;
                    }
                    s = &s[ch.len_utf8()..];
                }
                chunk = &chunk[1..];
            }
            _ => {
                let mut lit = c0;
                if c0 == '\\' && !cfg!(windows) {
                    chunk = &chunk[1..];
                    match chunk.chars().next() {
                        Some(c) => lit = c,
                        None => return ("", false, true),
                    }
                }
                if !failed {
                    // Go compare OCTET par octet : même chose, caractère
                    // entier contre caractère entier, sur du texte valide.
                    match s.chars().next() {
                        Some(c) if c == lit => s = &s[c.len_utf8()..],
                        Some(c) => {
                            failed = true;
                            s = &s[c.len_utf8()..];
                        }
                        None => failed = true,
                    }
                }
                chunk = &chunk[lit.len_utf8()..];
            }
        }
    }
    if failed {
        ("", false, false)
    } else {
        (s, true, false)
    }
}

/// `getEsc` : un caractère de classe (échappé ou non) ; `None` : motif mal
/// formé (vide, `-` ou `]` en tête, ou rien après).
fn get_esc(chunk: &str) -> Option<(char, &str)> {
    let mut chunk = chunk;
    match chunk.chars().next() {
        None | Some('-') | Some(']') => return None,
        Some('\\') if !cfg!(windows) => {
            chunk = &chunk[1..];
            chunk.chars().next()?;
        }
        _ => {}
    }
    let r = chunk.chars().next()?;
    let rest = &chunk[r.len_utf8()..];
    if rest.is_empty() {
        return None;
    }
    Some((r, rest))
}

/// Verrou par fichier : sérialise deux écritures du même fichier.
fn file_mu(path: &Path) -> Arc<Mutex<()>> {
    static MAP: LazyLock<Mutex<HashMap<PathBuf, Arc<Mutex<()>>>>> =
        LazyLock::new(|| Mutex::new(HashMap::new()));
    MAP.lock()
        .unwrap_or_else(|e| e.into_inner())
        .entry(super::policy::clean(path))
        .or_default()
        .clone()
}

/// `os.WriteFile` : écrit, crée au besoin (0644) ; un fichier existant garde
/// son mode (un script 0755 réécrit reste exécutable).
fn write_file(path: &Path, content: &[u8]) -> Result<(), String> {
    let mut opts = fs::OpenOptions::new();
    opts.write(true).create(true).truncate(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        opts.mode(0o644);
    }
    let mut f = opts.open(path).map_err(|e| path_err("open", path, &e))?;
    std::io::Write::write_all(&mut f, content).map_err(|e| path_err("write", path, &e))?;
    Ok(())
}

/// `fileWrite` : écrit le contenu tel quel, dossiers parents compris.
pub fn file_write(cwd: &Path, memory_dir: &Path, path: &str, content: &str) -> String {
    if path.trim().is_empty() {
        return "[erreur] chemin vide".into();
    }
    let path = resolve_agent_path(cwd, path);
    if let Some(msg) = guard_tool_only_path(&path, memory_dir) {
        return msg;
    }
    let mu = file_mu(&path);
    let _g = mu.lock().unwrap_or_else(|e| e.into_inner());
    if let Some(dir) = path.parent()
        && !dir.as_os_str().is_empty()
        && dir != Path::new(".")
        && let Err(e) = fs::create_dir_all(dir)
    {
        return path_err("mkdir", dir, &e);
    }
    let existed = fs::metadata(&path).is_ok();
    if let Err(e) = write_file(&path, content.as_bytes()) {
        return e;
    }
    let verb = if existed { "réécrit" } else { "créé" };
    format!("[ok] {} {verb} ({} octets)", path.display(), content.len())
}

/// `fileEdit` : UN remplacement exact (`old` doit apparaître une seule fois).
/// Un fichier en CRLF est comparé en LF et réécrit en CRLF.
pub fn file_edit(cwd: &Path, memory_dir: &Path, path: &str, old: &str, new: &str) -> String {
    if path.trim().is_empty() {
        return "[erreur] chemin vide".into();
    }
    if old.is_empty() {
        return "[erreur] old vide".into();
    }
    let path = resolve_agent_path(cwd, path);
    if let Some(msg) = guard_tool_only_path(&path, memory_dir) {
        return msg;
    }
    let mu = file_mu(&path);
    let _g = mu.lock().unwrap_or_else(|e| e.into_inner());
    // Octets, pas texte : un fichier hors UTF-8 (latin-1…) garde intact ce que
    // le remplacement ne touche pas.
    let content = match read_file(&path) {
        Ok(b) => b,
        Err(e) => return e,
    };
    let (old, new) = (old.as_bytes(), new.as_bytes());
    let crlf = contains(&content, b"\r\n");
    let search = if crlf && !contains(old, b"\r\n") {
        replace_all(&content, b"\r\n", b"\n")
    } else {
        content.clone()
    };
    let n = count(&search, old);
    if n == 0 {
        if !new.is_empty() && contains(&search, new) {
            return "[ok] déjà à jour — le fichier contient déjà cette modification".into();
        }
        return "[erreur] old introuvable dans le fichier".into();
    }
    if n > 1 {
        return format!(
            "[erreur] old apparaît {n} fois — ajoute du contexte pour le rendre unique"
        );
    }
    let i = find(&search, old).unwrap_or(0);
    let mut updated = [&search[..i], new, &search[i + old.len()..]].concat();
    if search != content {
        updated = replace_all(&updated, b"\n", b"\r\n");
    }
    if let Err(e) = write_file(&path, &updated) {
        return e;
    }
    format!("[ok] {} modifié (1 remplacement)", path.display())
}

fn find(h: &[u8], n: &[u8]) -> Option<usize> {
    if n.is_empty() {
        return Some(0);
    }
    h.windows(n.len()).position(|w| w == n)
}

fn contains(h: &[u8], n: &[u8]) -> bool {
    find(h, n).is_some()
}

/// Occurrences sans chevauchement (`strings.Count`, `old` non vide).
fn count(h: &[u8], n: &[u8]) -> usize {
    let (mut c, mut i) = (0, 0);
    while let Some(j) = find(&h[i..], n) {
        c += 1;
        i += j + n.len();
    }
    c
}

fn replace_all(h: &[u8], from: &[u8], to: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(h.len());
    let mut i = 0;
    while let Some(j) = find(&h[i..], from) {
        out.extend_from_slice(&h[i..i + j]);
        out.extend_from_slice(to);
        i += j + from.len();
    }
    out.extend_from_slice(&h[i..]);
    out
}

/// `toolAsk` : la question part vers l'UI par un événement dédié ; ici, dire
/// au modèle de clore son tour.
pub fn tool_ask(args: &Map<String, Value>) -> String {
    if arg_str(args, "question").trim().is_empty() {
        return "[erreur] question vide".into();
    }
    "[question posée à l'utilisateur] Termine ton tour MAINTENANT sans appeler d'autre outil : sa réponse arrivera comme prochain message.".into()
}

/// `askOptions` : options non vides, 4 au plus.
pub fn ask_options(args: &Map<String, Value>) -> Vec<String> {
    args.get("options")
        .and_then(Value::as_array)
        .map(|a| {
            a.iter()
                .filter_map(Value::as_str)
                .filter(|s| !s.trim().is_empty())
                .take(4)
                .map(str::to_string)
                .collect()
        })
        .unwrap_or_default()
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn args(v: Value) -> Map<String, Value> {
        v.as_object().unwrap().clone()
    }

    #[test]
    fn filepath_match_de_go() {
        for (p, n, want) in [
            ("*.go", "a.go", Some(true)),
            ("*.go", "a.rs", Some(false)),
            ("a?c", "abc", Some(true)),
            ("[a-c]x", "bx", Some(true)),
            ("[^a-c]x", "bx", Some(false)),
            ("*", "", Some(true)),
            ("*.é", "été.é", Some(true)),
            ("[", "a", None),
            ("a[", "b", None),
            ("\\*", "*", Some(true)),
        ] {
            assert_eq!(go_match(p, n), want, "{p} {n}");
        }
    }

    #[test]
    fn glob_en_regex() {
        let re = glob_to_regex("src/**/*.rs").unwrap();
        assert!(re.is_match("src/a.rs") && re.is_match("src/x/y/a.rs"));
        assert!(!re.is_match("src/a.rs.bak") && !re.is_match("a.rs"));
        assert!(glob_to_regex("./**").unwrap().is_match("a/b"));
    }

    #[test]
    fn lecture_numerotee() {
        let d = tempfile::tempdir().unwrap();
        fs::write(d.path().join("a.txt"), "un\ndeux\ntrois\n").unwrap();
        let r = tool_read(
            d.path(),
            &args(json!({"file":"a.txt","offset":2,"limit":1})),
            false,
        );
        assert_eq!(
            r.result,
            "2: deux\n… (1 lignes restantes — relis avec offset=3)"
        );
        assert!(r.noted.is_some());
        let r = tool_read(d.path(), &args(json!({"file":"absent"})), false);
        assert!(
            r.result.starts_with("[erreur] open ")
                && r.result.ends_with(": no such file or directory")
                || cfg!(windows)
        );
        assert!(r.noted.is_none());
        assert_eq!(
            tool_read(d.path(), &args(json!({"file":" "})), false).result,
            "[erreur] chemin vide"
        );
    }

    #[test]
    fn edition_crlf_et_deja_fait() {
        let d = tempfile::tempdir().unwrap();
        let mem = d.path().join("memory");
        fs::write(d.path().join("w.txt"), "a\r\nb\r\n").unwrap();
        assert_eq!(
            file_edit(d.path(), &mem, "w.txt", "a\nb", "a\nc"),
            format!(
                "[ok] {} modifié (1 remplacement)",
                d.path().join("w.txt").display()
            )
        );
        assert_eq!(
            fs::read_to_string(d.path().join("w.txt")).unwrap(),
            "a\r\nc\r\n"
        );
        assert!(file_edit(d.path(), &mem, "w.txt", "a\nb", "a\nc").starts_with("[ok] déjà à jour"));
        assert!(file_write(d.path(), &mem, "memory/x.md", "x").starts_with("[refusé]"));
    }
}
