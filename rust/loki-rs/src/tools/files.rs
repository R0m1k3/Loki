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

/// Booléen d'un argument JSON ; un modèle envoie parfois `"true"` en texte.
pub fn arg_bool(args: &Map<String, Value>, k: &str) -> bool {
    match args.get(k) {
        Some(Value::Bool(b)) => *b,
        Some(Value::String(s)) => s.trim().eq_ignore_ascii_case("true"),
        _ => false,
    }
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

/// `fileEdit` : UN remplacement exact (`old` doit apparaître une seule fois),
/// ou toutes les occurrences exactes avec `replace_all`.
/// Un fichier en CRLF est comparé en LF et réécrit en CRLF.
///
/// Sans correspondance exacte (et sans `replace_all`), des correspondances
/// approchées sont tentées de la plus stricte à la plus lâche (voir
/// [`Strategie`]) sur un fichier UTF-8 ; à chaque niveau, plusieurs candidats
/// = refus, un seul = remplacement. « Déjà à jour » ne vient qu'après.
pub fn file_edit(
    cwd: &Path,
    memory_dir: &Path,
    path: &str,
    old: &str,
    new: &str,
    replace_all_occ: bool,
) -> String {
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
    let (old_s, new_s) = (old, new);
    let (old, new) = (old.as_bytes(), new.as_bytes());
    let crlf = contains(&content, b"\r\n");
    let search = if crlf && !contains(old, b"\r\n") {
        replace_all(&content, b"\r\n", b"\n")
    } else {
        content.clone()
    };
    let n = count(&search, old);
    if n == 0 {
        if !replace_all_occ && let Ok(text) = std::str::from_utf8(&content) {
            match edition_approchee(text, old_s, new_s, crlf) {
                Approche::Aucune => {}
                Approche::Ambigue(s, n) => {
                    return format!(
                        "[erreur] old introuvable tel quel ; en correspondance {} il apparaît {n} fois — ajoute du contexte pour le rendre unique",
                        s.nom()
                    );
                }
                Approche::Appliquee(s, updated) => {
                    if let Err(e) = write_file(&path, updated.as_bytes()) {
                        return e;
                    }
                    return format!(
                        "[ok] {} modifié (1 remplacement, correspondance : {})",
                        path.display(),
                        s.nom()
                    );
                }
            }
        }
        if deja_a_jour(&search, old_s, new_s) {
            return "[ok] déjà à jour — le fichier contient déjà cette modification".into();
        }
        if replace_all_occ {
            return "[erreur] old introuvable dans le fichier (replace_all exige une correspondance exacte)".into();
        }
        return "[erreur] old introuvable dans le fichier".into();
    }
    if n > 1 && !replace_all_occ {
        return format!(
            "[erreur] old apparaît {n} fois — ajoute du contexte pour le rendre unique"
        );
    }
    let mut updated = if replace_all_occ {
        replace_all(&search, old, new)
    } else {
        let i = find(&search, old).unwrap_or(0);
        [&search[..i], new, &search[i + old.len()..]].concat()
    };
    if search != content {
        updated = replace_all(&updated, b"\n", b"\r\n");
    }
    if let Err(e) = write_file(&path, &updated) {
        return e;
    }
    if n == 1 {
        format!("[ok] {} modifié (1 remplacement)", path.display())
    } else {
        format!("[ok] {} modifié ({n} remplacements)", path.display())
    }
}

/// « Déjà à jour » : `new` est dans le fichier ET ce constat dit quelque chose.
/// Un `new` contenu dans `old` (suppression, « } ») ou sans lettre ni chiffre
/// se trouve partout : on rend alors l'erreur « introuvable ».
fn deja_a_jour(search: &[u8], old: &str, new: &str) -> bool {
    !new.is_empty()
        && !old.contains(new)
        && new.chars().any(char::is_alphanumeric)
        && contains(search, new.as_bytes())
}

/// Correspondances approchées de `file_edit`, de la plus stricte à la plus
/// lâche (idée des « replacers » d'opencode). Toutes travaillent par lignes
/// entières : la zone trouvée est une suite contiguë de lignes du fichier, de
/// même nombre que celles de `old`.
#[derive(Clone, Copy, Debug, PartialEq)]
enum Strategie {
    /// Espaces de fin de ligne ignorés.
    EspacesFin,
    /// Lignes comparées sans leur indentation (ni espaces de fin) ; `new` est
    /// réindenté sur le bloc trouvé.
    Indentation,
    /// Comme `Indentation`, après normalisation Unicode (guillemets
    /// typographiques, tirets, espaces insécables).
    Unicode,
    /// `old` de 3 lignes au moins : première et dernière lignes égales
    /// (normalisées), lignes intermédiaires proches en moyenne (similarité de
    /// Levenshtein normalisée ≥ [`SEUIL_ANCRES`]).
    Ancres,
}

const STRATEGIES: [Strategie; 4] = [
    Strategie::EspacesFin,
    Strategie::Indentation,
    Strategie::Unicode,
    Strategie::Ancres,
];

/// Similarité moyenne minimale des lignes intermédiaires (stratégie Ancres) :
/// 1 − distance/longueur max, moyennée sur les lignes du milieu.
const SEUIL_ANCRES: f64 = 0.7;

impl Strategie {
    fn nom(self) -> &'static str {
        match self {
            Strategie::EspacesFin => "espaces de fin ignorés",
            Strategie::Indentation => "indentation souple",
            Strategie::Unicode => "Unicode normalisé",
            Strategie::Ancres => "ancres de bloc",
        }
    }

    /// Le bloc `f` (lignes du fichier) correspond-il à `o` (lignes de old) ?
    fn accepte(self, f: &Lignes, o: &Lignes, at: usize) -> bool {
        let k = o.raw.len();
        let fr = &f.raw[at..at + k];
        let fnorm = &f.norm[at..at + k];
        match self {
            Strategie::EspacesFin => fr
                .iter()
                .zip(&o.raw)
                .all(|(a, b)| a.trim_end() == b.trim_end()),
            Strategie::Indentation => fr.iter().zip(&o.raw).all(|(a, b)| a.trim() == b.trim()),
            Strategie::Unicode => fnorm.iter().zip(&o.norm).all(|(a, b)| a == b),
            Strategie::Ancres => {
                if k < 3 {
                    return false;
                }
                let (premiere, derniere) = (&o.norm[0], &o.norm[k - 1]);
                if premiere.is_empty()
                    || derniere.is_empty()
                    || fnorm[0] != *premiere
                    || fnorm[k - 1] != *derniere
                {
                    return false;
                }
                let somme: f64 = (1..k - 1).map(|i| similarite(&fnorm[i], &o.norm[i])).sum();
                somme / (k - 2) as f64 >= SEUIL_ANCRES
            }
        }
    }
}

/// Lignes brutes et leur forme normalisée (Unicode puis `trim`).
struct Lignes<'a> {
    raw: Vec<&'a str>,
    norm: Vec<String>,
}

impl<'a> Lignes<'a> {
    fn new(raw: Vec<&'a str>) -> Self {
        let norm = raw
            .iter()
            .map(|l| normalise(l).trim().to_string())
            .collect();
        Lignes { raw, norm }
    }
}

/// Guillemets typographiques → ASCII, tirets → `-`, espaces insécables → espace.
fn normalise(s: &str) -> String {
    s.chars()
        .map(|c| match c {
            '\u{2018}' | '\u{2019}' | '\u{201A}' | '\u{201B}' => '\'',
            '\u{201C}' | '\u{201D}' | '\u{201E}' | '\u{201F}' => '"',
            '\u{2010}' | '\u{2011}' | '\u{2012}' | '\u{2013}' | '\u{2014}' | '\u{2015}'
            | '\u{2212}' => '-',
            '\u{00A0}' | '\u{2007}' | '\u{202F}' => ' ',
            c => c,
        })
        .collect()
}

/// 1 − distance de Levenshtein / longueur max (en caractères) ; 1 si vides.
fn similarite(a: &str, b: &str) -> f64 {
    let (a, b): (Vec<char>, Vec<char>) = (a.chars().collect(), b.chars().collect());
    let max = a.len().max(b.len());
    if max == 0 {
        return 1.0;
    }
    let mut prev: Vec<usize> = (0..=b.len()).collect();
    for (i, ca) in a.iter().enumerate() {
        let mut cur = vec![i + 1; b.len() + 1];
        for (j, cb) in b.iter().enumerate() {
            let sub = prev[j] + usize::from(ca != cb);
            cur[j + 1] = sub.min(prev[j + 1] + 1).min(cur[j] + 1);
        }
        prev = cur;
    }
    1.0 - prev[b.len()] as f64 / max as f64
}

enum Approche {
    Aucune,
    Ambigue(Strategie, usize),
    Appliquee(Strategie, String),
}

/// Tente les [`STRATEGIES`] dans l'ordre sur `text` (fichier UTF-8). Le
/// travail se fait en LF ; un fichier CRLF est réécrit en CRLF.
fn edition_approchee(text: &str, old: &str, new: &str, crlf: bool) -> Approche {
    let lf = |s: &str| s.replace("\r\n", "\n");
    let (text, old, new) = (lf(text), lf(old), lf(new));
    // Un saut de ligne final de old ne compte pas comme une ligne vide de plus ;
    // celui de new tombe avec lui (la zone remplacée garde son propre saut).
    let (old_body, new_body) = match old.strip_suffix('\n') {
        Some(o) => (o, new.strip_suffix('\n').unwrap_or(&new)),
        None => (old.as_str(), new.as_str()),
    };
    let o = Lignes::new(old_body.split('\n').collect());
    if o.raw.iter().all(|l| l.trim().is_empty()) {
        return Approche::Aucune;
    }
    let f = Lignes::new(text.split('\n').collect());
    let k = o.raw.len();
    if k > f.raw.len() {
        return Approche::Aucune;
    }
    for s in STRATEGIES {
        let trouves: Vec<usize> = (0..=f.raw.len() - k)
            .filter(|&at| s.accepte(&f, &o, at))
            .collect();
        match trouves[..] {
            [] => continue,
            [at] => {
                let updated = remplace_lignes(
                    &text,
                    &f.raw,
                    at,
                    k,
                    &reindente(new_body, &o.raw, &f.raw[at..at + k]),
                );
                let updated = if crlf {
                    updated.replace('\n', "\r\n")
                } else {
                    updated
                };
                return Approche::Appliquee(s, updated);
            }
            _ => return Approche::Ambigue(s, trouves.len()),
        }
    }
    Approche::Aucune
}

/// Remplace les lignes `at..at+k` de `text` par `by` ; un `by` vide retire
/// les lignes avec leur saut de ligne.
fn remplace_lignes(text: &str, lines: &[&str], at: usize, k: usize, by: &str) -> String {
    // Début en octets de chaque ligne (`split('\n')` : un octet par séparateur).
    let debut = |i: usize| lines[..i].iter().map(|l| l.len() + 1).sum::<usize>();
    let mut start = debut(at);
    let mut end = debut(at + k - 1) + lines[at + k - 1].len();
    if by.is_empty() {
        if at + k < lines.len() {
            end += 1;
        } else {
            start = start.saturating_sub(1);
        }
    }
    [&text[..start], by, &text[end..]].concat()
}

/// Réindente `new` sur le bloc trouvé : l'indentation de la première ligne
/// non vide de old est remplacée par celle de la ligne correspondante du
/// fichier, sur chaque ligne de `new` qui commence par elle (les autres
/// restent telles quelles).
fn reindente(new: &str, old: &[&str], bloc: &[&str]) -> String {
    let lead = |l: &str| l.len() - l.trim_start().len();
    let Some(i) = old.iter().position(|l| !l.trim().is_empty()) else {
        return new.to_string();
    };
    let (oi, fi) = (&old[i][..lead(old[i])], &bloc[i][..lead(bloc[i])]);
    if oi == fi {
        return new.to_string();
    }
    new.split('\n')
        .map(|l| match l.strip_prefix(oi) {
            Some(rest) if !l.trim().is_empty() => format!("{fi}{rest}"),
            _ => l.to_string(),
        })
        .collect::<Vec<_>>()
        .join("\n")
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
            file_edit(d.path(), &mem, "w.txt", "a\nb", "a\nc", false),
            format!(
                "[ok] {} modifié (1 remplacement)",
                d.path().join("w.txt").display()
            )
        );
        assert_eq!(
            fs::read_to_string(d.path().join("w.txt")).unwrap(),
            "a\r\nc\r\n"
        );
        assert!(
            file_edit(d.path(), &mem, "w.txt", "a\nb", "a\nc", false)
                .starts_with("[ok] déjà à jour")
        );
        assert!(file_write(d.path(), &mem, "memory/x.md", "x").starts_with("[refusé]"));
    }

    /// Cas d'édition : (nom, fichier avant, old, new, replace_all, début du
    /// résultat attendu, fichier après — `None` : inchangé).
    type CasEdition = (
        &'static str,
        &'static [u8],
        &'static str,
        &'static str,
        bool,
        &'static str,
        Option<&'static [u8]>,
    );

    #[test]
    fn edition_tolerante() {
        let cas: &[CasEdition] = &[
            (
                "exacte inchangée",
                b"a\nb\nc\n",
                "b",
                "B",
                false,
                "[ok] {P} modifié (1 remplacement)",
                Some(b"a\nB\nc\n"),
            ),
            (
                "espaces de fin",
                b"fn a() {  \n    x();\t\n}\n",
                "fn a() {\n    x();\n}",
                "fn a() {\n    y();\n}",
                false,
                "[ok] {P} modifié (1 remplacement, correspondance : espaces de fin ignorés)",
                Some(b"fn a() {\n    y();\n}\n"),
            ),
            (
                "espaces de fin ambigus",
                b"x \ny\nx\t\ny\n",
                "x\ny",
                "z",
                false,
                "[erreur] old introuvable tel quel ; en correspondance espaces de fin ignorés il apparaît 2 fois",
                None,
            ),
            (
                "indentation réindentée",
                b"mod m {\n        if a {\n            b();\n        }\n}\n",
                "if a {\n    b();\n}",
                "if a {\n    c();\n    d();\n}",
                false,
                "[ok] {P} modifié (1 remplacement, correspondance : indentation souple)",
                Some(
                    b"mod m {\n        if a {\n            c();\n            d();\n        }\n}\n",
                ),
            ),
            (
                "indentation ambiguë",
                b"  a();\n  b();\n\ta();\n\tb();\n",
                "a();\nb();",
                "c();",
                false,
                "[erreur] old introuvable tel quel ; en correspondance indentation souple il apparaît 2 fois",
                None,
            ),
            (
                "unicode normalisé",
                "say(\u{201C}l\u{2019}\u{e9}t\u{e9}\u{201D}) \u{2014} ok\u{a0}\nz\n".as_bytes(),
                "say(\"l'été\") - ok",
                "say(\"hiver\")",
                false,
                "[ok] {P} modifié (1 remplacement, correspondance : Unicode normalisé)",
                Some(b"say(\"hiver\")\nz\n"),
            ),
            (
                "unicode ambigu",
                "\u{2018}a\u{2019}\n\u{201C}x\u{201D}\n\u{2018}a\u{2019}\n".as_bytes(),
                "'a'",
                "b",
                false,
                "[erreur] old introuvable tel quel ; en correspondance Unicode normalisé il apparaît 2 fois",
                None,
            ),
            (
                "ancres de bloc",
                b"fn f() {\n    let total = compute(a, b);\n    log(total);\n}\nfin\n",
                "fn f() {\n    let total = compute(a, c);\n    log(totals);\n}",
                "fn f() {}",
                false,
                "[ok] {P} modifié (1 remplacement, correspondance : ancres de bloc)",
                Some(b"fn f() {}\nfin\n"),
            ),
            (
                "ancres trop éloignées",
                b"fn f() {\n    tout autre chose ici\n    vraiment\n}\n",
                "fn f() {\n    let total = compute(a, c);\n    log(totals);\n}",
                "fn f() {}",
                false,
                "[erreur] old introuvable dans le fichier",
                None,
            ),
            (
                "ancres ambiguës",
                b"{\n  a = 1;\n}\n{\n  a = 2;\n}\n",
                "{\n  a = 3;\n}",
                "{}",
                false,
                "[erreur] old introuvable tel quel ; en correspondance ancres de bloc il apparaît 2 fois",
                None,
            ),
            (
                "ancres sous 3 lignes",
                b"fn x() {\n  foo();\n}\n",
                "fn x() {\n}",
                "fn x() {}",
                false,
                "[erreur] old introuvable dans le fichier",
                None,
            ),
            (
                // « } » est partout : ce n'est pas un « déjà à jour ».
                "new court pas déjà à jour",
                b"fn a() {\n  x();\n}\n",
                "y();",
                "}",
                false,
                "[erreur] old introuvable dans le fichier",
                None,
            ),
            (
                "déjà à jour après les replis",
                b"let v = 2;\n",
                "let v = 1;",
                "let v = 2;",
                false,
                "[ok] déjà à jour",
                None,
            ),
            (
                "crlf avec repli",
                b"  a  \r\n  b\r\nc\r\n",
                "a\nb",
                "a\nB",
                false,
                "[ok] {P} modifié (1 remplacement, correspondance : indentation souple)",
                Some(b"  a\r\n  B\r\nc\r\n"),
            ),
            (
                "suppression de lignes",
                b"a\n  b  \nc\n",
                "b\n",
                "",
                false,
                "[ok] {P} modifié (1 remplacement, correspondance : indentation souple)",
                Some(b"a\nc\n"),
            ),
            (
                "replace_all",
                b"x=1; x=1;\nx=1\n",
                "x=1",
                "x=2",
                true,
                "[ok] {P} modifié (3 remplacements)",
                Some(b"x=2; x=2;\nx=2\n"),
            ),
            (
                "replace_all sans repli",
                b"a  \nb\n",
                "a\nb",
                "c",
                true,
                "[erreur] old introuvable dans le fichier (replace_all exige une correspondance exacte)",
                None,
            ),
            (
                "ambiguïté exacte",
                b"x\nx\n",
                "x",
                "y",
                false,
                "[erreur] old apparaît 2 fois",
                None,
            ),
            (
                "hors utf-8 exacte",
                b"caf\xe9\nligne\n",
                "ligne",
                "LIGNE",
                false,
                "[ok] {P} modifié (1 remplacement)",
                Some(b"caf\xe9\nLIGNE\n"),
            ),
            (
                "hors utf-8 sans repli",
                b"caf\xe9\nligne  \nz\n",
                "ligne\nz",
                "y",
                false,
                "[erreur] old introuvable dans le fichier",
                None,
            ),
        ];
        for (nom, avant, old, new, all, want, apres) in cas {
            let d = tempfile::tempdir().unwrap();
            let mem = d.path().join("memory");
            let f = d.path().join("f.txt");
            fs::write(&f, avant).unwrap();
            let got = file_edit(d.path(), &mem, "f.txt", old, new, *all);
            let want = want.replace("{P}", &f.display().to_string());
            assert!(got.starts_with(&want), "{nom} : {got}");
            let now = fs::read(&f).unwrap();
            assert_eq!(&now[..], apres.unwrap_or(avant), "{nom}");
        }
    }

    #[test]
    fn similarite_levenshtein() {
        assert_eq!(similarite("", ""), 1.0);
        assert_eq!(similarite("abc", "abc"), 1.0);
        assert_eq!(similarite("abc", ""), 0.0);
        assert!((similarite("kitten", "sitting") - (1.0 - 3.0 / 7.0)).abs() < 1e-9);
    }

    #[test]
    fn booleen_d_argument() {
        assert!(arg_bool(&args(json!({"r": true})), "r"));
        assert!(arg_bool(&args(json!({"r": "True"})), "r"));
        assert!(!arg_bool(&args(json!({"r": 1})), "r"));
        assert!(!arg_bool(&args(json!({})), "r"));
    }
}
