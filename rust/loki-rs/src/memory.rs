//! Mémoire de Loki — port de `chat_memory.go` (pages), `mem_index.go`
//! (index MEMORY.md tenu par le code), `mem_io.go` / `mem_fsutil.go`
//! (écritures sûres), `moveMemPage` (projects.go), `tracker.go` +
//! `web_tracker.go` (trackers) et des routes du mode agent (`handleAgent`,
//! `handleAgentToggle`, `handleCompactToggle`, `handleMemoryMode`) — lot 13
//! de la migration, servi par la façade quand `LOKI_MEM=rust` (expérimental ;
//! le Go par défaut) :
//!
//! - `GET /api/mem?name=` : contenu d'une page ; `POST /api/mem/save`,
//!   `/api/mem/delete`, `/api/mem/move` : éditeur web des pages ;
//! - `GET /api/agent` (alias `/api/tools`, `/api/skills`) : mode agent,
//!   compactage, mode mémoire et liste des pages ; `POST /api/agent/toggle`
//!   (et alias), `/api/agent/compact`, `GET|POST /api/memory` ;
//! - `GET|POST /api/tracker` : panneau des trackers.
//!
//! Les pages restent des fichiers `memory/<projet>/<nom>.md` : le Go (outils
//! `mem_*` de sa boucle, index injecté au modèle) les lit et les écrit sur le
//! même disque. Le chiffrement reste au Go, seul à tenir la clé en mémoire :
//! une page qui porte le magic `LOKIMEMv1` est ouverte par
//! `/internal/v1/crypt/open`, et toute écriture passe par
//! `/internal/v1/crypt/seal` (clair rendu tel quel quand le chiffrement est
//! coupé). Les trackers vivent dans le bucket `tracker`, scellés de même.
//!
//! Hors périmètre (restent au Go) : coffre et chiffrement
//! (`/api/mem/encrypt|decrypt|unlock|lock`), santé, instantanés, sauvegarde,
//! hub des projets (`/api/projects`), dont la bascule recharge la discussion.

use std::collections::HashMap;
use std::io::Write as _;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{SystemTime, UNIX_EPOCH};

use http_body_util::{BodyExt, Limited};
use hyper::body::Incoming;
use hyper::{Method, Request, Response, StatusCode};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use crate::api::Ctx;
use crate::auth::json as send_json;
use crate::gojson::{self, Kind};
use crate::proxy::Body;
use crate::store::{BK_CONFIG, BK_STATE, CryptError, Store};

pub mod tools;

/// Index du projet, tenu par le code.
pub const MEM_INDEX_FILE: &str = "MEMORY.md";
/// Magic des contenus chiffrés (`memPageMagic`).
pub const PAGE_MAGIC: &[u8] = b"LOKIMEMv1";
pub const BK_TRACKER: &str = "tracker";
pub(crate) const ST_PROJECTS: &str = "projects";
const ST_ACTIVE_PROJECT: &str = "active_project";
const ST_AGENT: &str = "agent";
const DEFAULT_PROJECT_SLUG: &str = "generale";
const DEFAULT_PROJECT_NAME: &str = "Générale";
const NEW_PAGE: &str = "# nouvelle page\n\nNote ici ce que loki doit retenir entre les sessions.\n";
const LOCKED_TITLE: &str = "🔒 (chiffré — mémoire verrouillée)";
const ERR_MEM_LOCKED: &str = "page chiffrée, mémoire verrouillée";
const ERR_STORE_LOCKED: &str = "valeur chiffrée, mémoire verrouillée";

/// Routes servies ici.
pub fn handles(path: &str) -> bool {
    matches!(
        path,
        "/api/mem"
            | "/api/mem/save"
            | "/api/mem/delete"
            | "/api/mem/move"
            | "/api/agent"
            | "/api/tools"
            | "/api/skills"
            | "/api/agent/toggle"
            | "/api/tools/toggle"
            | "/api/skills/toggle"
            | "/api/agent/compact"
            | "/api/memory"
            | "/api/tracker"
    )
}

/// `strings.TrimSpace`.
fn go_trim(s: &str) -> &str {
    s.trim_matches(char::is_whitespace)
}

// ---------------------------------------------------------------------------
// Fonctions pures
// ---------------------------------------------------------------------------

/// `memFileName` : ajoute `.md` au besoin et valide le nom.
pub fn mem_file_name(name: &str) -> Result<String, String> {
    let mut name = go_trim(name).to_string();
    if name.is_empty() {
        return Err("nom vide".into());
    }
    if !name.to_lowercase().ends_with(".md") {
        name.push_str(".md");
    }
    if !name
        .bytes()
        .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'-'))
    {
        return Err("nom invalide (alphanum, ._-)".into());
    }
    Ok(name)
}

/// `titleOf` : 1re ligne non vide, sans les `#` de tête.
pub fn title_of(content: &str) -> String {
    for line in content.split('\n') {
        let s = go_trim(go_trim(line).trim_start_matches('#'));
        if !s.is_empty() {
            return s.to_string();
        }
    }
    String::new()
}

/// `isIndexFile`.
pub fn is_index_file(name: &str) -> bool {
    go_trim(name).eq_ignore_ascii_case(MEM_INDEX_FILE)
}

/// `memorySeed` : en-tête de l'index d'un projet.
pub fn memory_seed(name: &str) -> String {
    format!(
        "# Index mémoire — {name}\n\nUne ligne par page de ce projet (`- [titre](fichier.md) — accroche`). \
         Tenu à jour automatiquement à la création et à la suppression d'une page ; \
         l'accroche après le titre, elle, est libre.\n"
    )
}

/// `normalizeMemMode`.
pub fn normalize_mem_mode(v: &str) -> &'static str {
    match go_trim(v).to_lowercase().as_str() {
        "off" => "off",
        "ondemand" => "ondemand",
        "search" => "search",
        _ => "always",
    }
}

/// `slugifyIdent` : slug de fichier sûr (accents courants translittérés).
pub fn slugify_ident(name: &str) -> String {
    let mut b = String::new();
    for c in go_trim(name).chars() {
        // unicode.ToLower, rune par rune (pas de minuscule à plusieurs runes).
        let lc = if c == 'İ' {
            'i'
        } else {
            let mut l = c.to_lowercase();
            match (l.next(), l.next()) {
                (Some(x), None) => x,
                _ => c,
            }
        };
        let rep = match lc {
            'à' | 'â' | 'ä' | 'á' | 'ã' => "a",
            'é' | 'è' | 'ê' | 'ë' => "e",
            'î' | 'ï' | 'í' => "i",
            'ô' | 'ö' | 'ó' | 'õ' => "o",
            'ù' | 'û' | 'ü' | 'ú' => "u",
            'ç' => "c",
            'ñ' => "n",
            _ => {
                b.push(lc);
                continue;
            }
        };
        b.push_str(rep);
    }
    // [^a-z0-9]+ → « - »
    let mut slug = String::with_capacity(b.len());
    let mut in_run = false;
    for c in b.chars() {
        if c.is_ascii_lowercase() || c.is_ascii_digit() {
            slug.push(c);
            in_run = false;
        } else if !in_run {
            slug.push('-');
            in_run = true;
        }
    }
    let mut slug = slug.trim_matches('-').to_string();
    if slug.len() > 40 {
        slug = slug[..40].trim_matches('-').to_string();
    }
    slug
}

// ---------------------------------------------------------------------------
// Dates des trackers
// ---------------------------------------------------------------------------

/// Fuseau des dates saisies (`time.Local` du Go).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Tz {
    /// Fuseau de la machine (mktime).
    Local,
    /// Décalage fixe, en secondes à l'est d'UTC (essais).
    Fixed(i64),
}

/// Date-heure civile lue dans une saisie.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Civil {
    pub year: i64,
    pub month: u32,
    pub day: u32,
    pub hour: u32,
    pub min: u32,
    pub sec: u32,
    pub nanos: u32,
}

fn days_from_civil(y: i64, m: u32, d: u32) -> i64 {
    let y = if m <= 2 { y - 1 } else { y };
    let era = y.div_euclid(400);
    let yoe = y.rem_euclid(400);
    let m = i64::from(m);
    let mp = if m > 2 { m - 3 } else { m + 9 };
    let doy = (153 * mp + 2) / 5 + i64::from(d) - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    era * 146097 + doe - 719468
}

fn is_leap(y: i64) -> bool {
    y % 4 == 0 && (y % 100 != 0 || y % 400 == 0)
}

fn days_in(m: u32, y: i64) -> u32 {
    match m {
        2 if is_leap(y) => 29,
        2 => 28,
        4 | 6 | 9 | 11 => 30,
        _ => 31,
    }
}

impl Tz {
    /// Secondes Unix de la date civile dans ce fuseau.
    pub fn to_unix(self, c: &Civil) -> i64 {
        let naive = days_from_civil(c.year, c.month, c.day) * 86400
            + i64::from(c.hour) * 3600
            + i64::from(c.min) * 60
            + i64::from(c.sec);
        match self {
            Tz::Fixed(off) => naive - off,
            Tz::Local => local_to_unix(c).unwrap_or(naive),
        }
    }
}

#[cfg(unix)]
fn local_to_unix(c: &Civil) -> Option<i64> {
    let mut tm: libc::tm = unsafe { std::mem::zeroed() };
    tm.tm_year = i32::try_from(c.year - 1900).ok()?;
    tm.tm_mon = c.month as i32 - 1;
    tm.tm_mday = c.day as i32;
    tm.tm_hour = c.hour as i32;
    tm.tm_min = c.min as i32;
    tm.tm_sec = c.sec as i32;
    tm.tm_isdst = -1;
    // SAFETY : structure initialisée, pointeur valide.
    let t = unsafe { libc::mktime(&mut tm) };
    (t != -1).then_some(t as i64)
}

#[cfg(not(unix))]
fn local_to_unix(_: &Civil) -> Option<i64> {
    None
}

/// `getnum` du paquet time : 1 ou 2 chiffres (`fixed` : exactement 2).
fn getnum(v: &[u8], i: &mut usize, fixed: bool) -> Option<u32> {
    let d0 = *v.get(*i)?;
    if !d0.is_ascii_digit() {
        return None;
    }
    match v.get(*i + 1) {
        Some(d1) if d1.is_ascii_digit() => {
            *i += 2;
            Some(u32::from(d0 - b'0') * 10 + u32::from(d1 - b'0'))
        }
        _ if fixed => None,
        _ => {
            *i += 1;
            Some(u32::from(d0 - b'0'))
        }
    }
}

fn expect(v: &[u8], i: &mut usize, c: u8) -> Option<()> {
    (v.get(*i) == Some(&c)).then(|| *i += 1)
}

/// `time.ParseInLocation` restreint aux mises en page de `parseWhen`
/// (`2006[-01[-02[ 15[:04[:05]]]]]`). `parts` : nombre d'éléments après
/// l'année (0 à 5).
fn parse_layout(s: &str, parts: u8) -> Option<Civil> {
    let v = s.as_bytes();
    if v.len() < 4 || !v[0].is_ascii_digit() || !v[..4].iter().all(u8::is_ascii_digit) {
        return None;
    }
    let mut c = Civil {
        year: s[..4].parse().ok()?,
        month: 1,
        day: 1,
        ..Civil::default()
    };
    let mut i = 4;
    if parts >= 1 {
        expect(v, &mut i, b'-')?;
        c.month = getnum(v, &mut i, true)?;
        if !(1..=12).contains(&c.month) {
            return None;
        }
    }
    if parts >= 2 {
        expect(v, &mut i, b'-')?;
        c.day = getnum(v, &mut i, true)?;
    }
    if parts >= 3 {
        // `skip` du paquet time : une espace de la mise en page vaut une
        // suite d'espaces (ou rien en fin de saisie).
        if i < v.len() && v[i] != b' ' {
            return None;
        }
        while i < v.len() && v[i] == b' ' {
            i += 1;
        }
        c.hour = getnum(v, &mut i, false)?;
        if c.hour >= 24 {
            return None;
        }
    }
    if parts >= 4 {
        expect(v, &mut i, b':')?;
        c.min = getnum(v, &mut i, true)?;
        if c.min >= 60 {
            return None;
        }
    }
    if parts >= 5 {
        expect(v, &mut i, b':')?;
        c.sec = getnum(v, &mut i, true)?;
        if c.sec >= 60 {
            return None;
        }
        // Fraction de seconde acceptée sans figurer dans la mise en page.
        if v.len() >= i + 2 && matches!(v[i], b'.' | b',') && v[i + 1].is_ascii_digit() {
            let mut n = i + 1;
            while n < v.len() && v[n].is_ascii_digit() {
                n += 1;
            }
            let digits = &s[i + 1..n.min(i + 10)];
            let mut ns: u32 = digits.parse().ok()?;
            for _ in digits.len()..9 {
                ns *= 10;
            }
            c.nanos = ns;
            i = n;
        }
    }
    if i != v.len() {
        return None;
    }
    if c.day < 1 || c.day > days_in(c.month, c.year) {
        return None;
    }
    Some(c)
}

/// `parseWhen` : date à précision variable → (instant en ms, sans heure).
/// Vide ou « now » = `now_ms`.
pub fn parse_when(when: &str, now_ms: i64, tz: Tz) -> Result<(i64, bool), String> {
    let w = go_trim(when);
    if w.is_empty() || w.eq_ignore_ascii_case("now") || w.eq_ignore_ascii_case("maintenant") {
        return Ok((now_ms, false));
    }
    let w = w.replacen('T', " ", 1);
    for (parts, date_only) in [
        (5, false),
        (4, false),
        (3, false),
        (2, true),
        (1, true),
        (0, true),
    ] {
        if let Some(c) = parse_layout(&w, parts) {
            return Ok((
                tz.to_unix(&c) * 1000 + i64::from(c.nanos / 1_000_000),
                date_only,
            ));
        }
    }
    Err(format!(
        "date non comprise : {} (attendu AAAA, AAAA-MM, AAAA-MM-JJ ou AAAA-MM-JJ HH:MM)",
        gojson::quote(when)
    ))
}

pub(crate) fn now_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| d.as_millis() as i64)
}

fn now_nanos() -> u128 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| d.as_nanos())
}

// ---------------------------------------------------------------------------
// Écritures sûres (mem_fsutil.go)
// ---------------------------------------------------------------------------

static TMP_SEQ: AtomicU64 = AtomicU64::new(0);

#[cfg(unix)]
fn set_mode(path: &Path, perm: u32) -> std::io::Result<()> {
    use std::os::unix::fs::PermissionsExt;
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(perm))
}

#[cfg(not(unix))]
fn set_mode(_: &Path, _: u32) -> std::io::Result<()> {
    Ok(())
}

/// `memWriteFileAtomic` : temporaire + fsync + rename.
pub fn write_file_atomic(path: &Path, data: &[u8], perm: u32) -> std::io::Result<()> {
    let dir = path.parent().unwrap_or(Path::new("."));
    std::fs::create_dir_all(dir)?;
    let base = path
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_default();
    let (tmp_path, mut f) = loop {
        let n = TMP_SEQ.fetch_add(1, Ordering::Relaxed);
        let p = dir.join(format!(".tmp-{base}-{}{n}", now_nanos() % 1_000_000_000));
        let mut o = std::fs::OpenOptions::new();
        o.write(true).create_new(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            o.mode(0o600);
        }
        match o.open(&p) {
            Ok(f) => break (p, f),
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => continue,
            Err(e) => return Err(e),
        }
    };
    let res = (|| {
        f.write_all(data)?;
        f.sync_all()?;
        drop(f);
        set_mode(&tmp_path, perm)?;
        std::fs::rename(&tmp_path, path)
    })();
    if res.is_err() {
        let _ = std::fs::remove_file(&tmp_path);
    }
    res
}

/// `memWriteFileVerified` : écriture atomique puis relecture octet par octet.
pub fn write_file_verified(path: &Path, data: &[u8], perm: u32) -> Result<(), String> {
    write_file_atomic(path, data, perm).map_err(|e| e.to_string())?;
    let back =
        std::fs::read(path).map_err(|e| format!("relecture de vérification impossible : {e}"))?;
    if back.len() != data.len() {
        let _ = std::fs::remove_file(path);
        return Err(format!(
            "vérification échouée : {} octets relus, {} écrits",
            back.len(),
            data.len()
        ));
    }
    if let Some(i) = back.iter().zip(data).position(|(a, b)| a != b) {
        let _ = std::fs::remove_file(path);
        return Err(format!("vérification échouée : octet {i} diffère"));
    }
    Ok(())
}

/// `os.WriteFile` : création avec `perm`, sinon le fichier garde ses droits.
fn write_file(path: &Path, data: &[u8], perm: u32) -> std::io::Result<()> {
    let mut o = std::fs::OpenOptions::new();
    o.write(true).create(true).truncate(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        o.mode(perm);
    }
    #[cfg(not(unix))]
    let _ = perm;
    o.open(path)?.write_all(data)
}

// ---------------------------------------------------------------------------
// Projets (lecture du registre du Go)
// ---------------------------------------------------------------------------

/// `Project` (bucket state, clé `projects`).
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
pub struct Project {
    pub slug: String,
    pub name: String,
    #[serde(default)]
    pub created_at: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub desc: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub mem_mode: String,
}

/// `listProjects` : le registre, le plus ancien d'abord ; au moins le
/// projet par défaut.
pub async fn list_projects(store: &Store) -> Vec<Project> {
    let mut out: Vec<Project> = match store.get(BK_STATE, ST_PROJECTS).await {
        Ok(Some(b)) if !b.is_empty() => serde_json::from_slice(&b).unwrap_or_default(),
        _ => Vec::new(),
    };
    if out.is_empty() {
        out.push(Project {
            slug: DEFAULT_PROJECT_SLUG.into(),
            name: DEFAULT_PROJECT_NAME.into(),
            created_at: now_ms(),
            desc: String::new(),
            mem_mode: String::new(),
        });
    }
    out.sort_by_key(|p| p.created_at);
    out
}

/// `activeProjectSlug` (valeur rangée ; le Go seul connaît la bascule
/// temporaire d'une tâche planifiée).
pub async fn active_project(store: &Store) -> String {
    match go_trim(&store.get_str(BK_STATE, ST_ACTIVE_PROJECT).await) {
        "" => DEFAULT_PROJECT_SLUG.into(),
        s => s.into(),
    }
}

fn project_name(list: &[Project], slug: &str) -> String {
    list.iter()
        .find(|p| p.slug == slug)
        .map_or_else(|| slug.to_string(), |p| p.name.clone())
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

/// La mémoire tenue par la façade.
pub struct Memory {
    home: PathBuf,
    tz: Tz,
}

/// Lecture d'une page.
#[derive(Debug)]
enum ReadErr {
    Invalid,
    Missing,
    Locked,
    Other,
}

impl Memory {
    pub fn new(home: PathBuf) -> Self {
        Memory {
            home,
            tz: Tz::Local,
        }
    }

    /// Fuseau des dates de trackers (essais : décalage fixe).
    pub fn with_tz(home: PathBuf, tz: Tz) -> Self {
        Memory { home, tz }
    }

    /// `projectMemoryDir`.
    fn project_dir(&self, slug: &str) -> PathBuf {
        let slug = if slug.is_empty() {
            DEFAULT_PROJECT_SLUG
        } else {
            slug
        };
        self.home.join("memory").join(slug)
    }

    /// `safeMemPath`.
    fn safe_path(dir: &Path, name: &str) -> Result<PathBuf, String> {
        let f = mem_file_name(name)?;
        let p = dir.join(&f);
        if p.parent() != Some(dir) {
            return Err("path invalide".into());
        }
        Ok(p)
    }

    /// `decodeMemContent`.
    async fn decode(store: &Store, raw: Vec<u8>) -> Result<Vec<u8>, ReadErr> {
        if !raw.starts_with(PAGE_MAGIC) {
            return Ok(raw);
        }
        match store.open(raw).await {
            Ok(b) => Ok(b),
            Err(CryptError::Locked) => Err(ReadErr::Locked),
            Err(CryptError::Other(_)) => Err(ReadErr::Other),
        }
    }

    /// `memReadPage`.
    async fn read_page(store: &Store, dir: &Path, name: &str) -> Result<Vec<u8>, ReadErr> {
        let p = Self::safe_path(dir, name).map_err(|_| ReadErr::Invalid)?;
        let raw = std::fs::read(&p).map_err(|_| ReadErr::Missing)?;
        Self::decode(store, raw).await
    }

    /// `MemContent` : contenu clair, vide si illisible.
    async fn content(store: &Store, dir: &Path, name: &str) -> String {
        match Self::read_page(store, dir, name).await {
            Ok(b) => String::from_utf8_lossy(&b).into_owned(),
            Err(_) => String::new(),
        }
    }

    /// `writeMemFile` : scellé selon le chiffrement, `.bak` de la version
    /// précédente, écriture atomique vérifiée.
    async fn write_page(store: &Store, dir: &Path, name: &str, plain: &[u8]) -> Result<(), String> {
        let p = Self::safe_path(dir, name)?;
        let out = store.seal(plain.to_vec()).await.map_err(|e| match e {
            CryptError::Locked => ERR_MEM_LOCKED.to_string(),
            CryptError::Other(e) => e,
        })?;
        std::fs::create_dir_all(dir).map_err(|e| e.to_string())?;
        if let Ok(old) = std::fs::read(&p) {
            let mut bak = p.clone().into_os_string();
            bak.push(".bak");
            let _ = write_file_atomic(Path::new(&bak), &old, 0o600);
        }
        write_file_verified(&p, &out, 0o600)
    }

    /// `MemList` : pages (nom, titre), triées par nom.
    async fn list(store: &Store, dir: &Path) -> Vec<(String, String)> {
        let Ok(rd) = std::fs::read_dir(dir) else {
            return Vec::new();
        };
        let mut names: Vec<String> = rd
            .filter_map(Result::ok)
            .filter(|e| !e.file_type().is_ok_and(|t| t.is_dir()))
            .map(|e| e.file_name().to_string_lossy().into_owned())
            .filter(|n| !n.starts_with('.') && n.to_lowercase().ends_with(".md"))
            .collect();
        names.sort();
        let mut out = Vec::with_capacity(names.len());
        for n in names {
            let title = match Self::read_page(store, dir, &n).await {
                Ok(b) => title_of(&String::from_utf8_lossy(&b)),
                Err(_) => LOCKED_TITLE.to_string(),
            };
            out.push((n, title));
        }
        out
    }

    // --- index ----------------------------------------------------------

    /// `writeIndex` : en clair, comme le Go.
    fn write_index(dir: &Path, body: &str) {
        if std::fs::create_dir_all(dir).is_err() {
            return;
        }
        let _ = write_file(&dir.join(MEM_INDEX_FILE), body.as_bytes(), 0o644);
    }

    /// `ensureIndexSeed`.
    async fn ensure_index_seed(store: &Store, dir: &Path, project: &str) {
        if !go_trim(&Self::content(store, dir, MEM_INDEX_FILE).await).is_empty() {
            return;
        }
        Self::write_index(dir, &memory_seed(project));
    }

    /// `indexLineFor`.
    async fn index_line(store: &Store, dir: &Path, name: &str) -> String {
        let t = title_of(&Self::content(store, dir, name).await);
        let title = if t.is_empty() { name.to_string() } else { t };
        format!("- [{title}]({name})")
    }

    /// `memIndexAdd`.
    async fn index_add(store: &Store, dir: &Path, name: &str, project: &str) {
        let Ok(f) = mem_file_name(name) else { return };
        if is_index_file(&f) {
            return;
        }
        Self::ensure_index_seed(store, dir, project).await;
        let content = Self::content(store, dir, MEM_INDEX_FILE).await;
        let r = format!("]({f})");
        if content.split('\n').any(|l| l.contains(&r)) {
            return;
        }
        let line = Self::index_line(store, dir, &f).await;
        Self::write_index(
            dir,
            &format!("{}\n{line}\n", content.trim_end_matches('\n')),
        );
    }

    /// `memIndexRemove`.
    async fn index_remove(store: &Store, dir: &Path, name: &str) {
        let Ok(f) = mem_file_name(name) else { return };
        if is_index_file(&f) {
            return;
        }
        let content = Self::content(store, dir, MEM_INDEX_FILE).await;
        if content.is_empty() {
            return;
        }
        let r = format!("]({f})");
        let lines: Vec<&str> = content.split('\n').collect();
        let kept: Vec<&str> = lines.iter().copied().filter(|l| !l.contains(&r)).collect();
        if kept.len() == lines.len() {
            return;
        }
        Self::write_index(
            dir,
            &format!("{}\n", kept.join("\n").trim_end_matches('\n')),
        );
    }

    // --- pages ----------------------------------------------------------

    /// `MemSave` : crée ou écrase ; `old` différent : l'ancienne est retirée.
    async fn save(
        store: &Store,
        dir: &Path,
        project: &str,
        name: &str,
        old: &str,
        content: &str,
    ) -> Result<(), String> {
        if !old.is_empty()
            && let Ok(of) = mem_file_name(old)
            && let Ok(nf) = mem_file_name(name)
            && of != nf
            && let Ok(od) = Self::safe_path(dir, old)
        {
            let _ = std::fs::remove_file(od);
        }
        let body = format!("{}\n", content.trim_end_matches('\n'));
        Self::write_page(store, dir, name, body.as_bytes()).await?;
        // memIndexRename : ancienne ligne retirée (même nom compris), nouvelle
        // ajoutée.
        if !old.is_empty() {
            Self::index_remove(store, dir, old).await;
        }
        Self::index_add(store, dir, name, project).await;
        Ok(())
    }

    /// `MemDelete`.
    async fn delete(store: &Store, dir: &Path, name: &str) -> Result<(), String> {
        let p = Self::safe_path(dir, name)?;
        if std::fs::metadata(&p).is_err() {
            return Err("introuvable".into());
        }
        std::fs::remove_file(&p).map_err(|e| e.to_string())?;
        Self::index_remove(store, dir, name).await;
        Ok(())
    }

    /// `moveMemPage`.
    async fn move_page(
        &self,
        store: &Store,
        name: &str,
        from: &str,
        to: &str,
    ) -> Result<(), String> {
        if go_trim(from) == go_trim(to) {
            return Err("déjà dans ce projet".into());
        }
        let projects = list_projects(store).await;
        if !projects.iter().any(|p| p.slug == to) {
            return Err("projet cible introuvable".into());
        }
        let f = mem_file_name(name)?;
        if is_index_file(&f) {
            return Err("l'index du projet n'est pas déplaçable".into());
        }
        let (fd, td) = (self.project_dir(from), self.project_dir(to));
        let raw = std::fs::read(fd.join(&f)).map_err(|_| "page introuvable".to_string())?;
        if std::fs::metadata(td.join(&f)).is_ok() {
            return Err("une page du même nom existe déjà dans le projet cible".into());
        }
        std::fs::create_dir_all(&td).map_err(|e| e.to_string())?;
        write_file(&td.join(&f), &raw, 0o600).map_err(|e| e.to_string())?;
        let _ = std::fs::remove_file(fd.join(&f));
        Self::index_remove(store, &fd, &f).await;
        Self::index_add(store, &td, &f, &project_name(&projects, to)).await;
        Ok(())
    }

    // --- mode mémoire ---------------------------------------------------

    /// `memMode` : mode du projet actif, repli sur `MEM_MODE`.
    pub(crate) async fn mem_mode(store: &Store) -> &'static str {
        let active = active_project(store).await;
        let projects = list_projects(store).await;
        if let Some(p) = projects.iter().find(|p| p.slug == active)
            && !p.mem_mode.is_empty()
        {
            return normalize_mem_mode(&p.mem_mode);
        }
        normalize_mem_mode(
            store
                .config()
                .await
                .get("MEM_MODE")
                .map_or("", String::as_str),
        )
    }

    /// `setMemMode` → `setProjectMemMode(activeProjectSlug(), mode)`.
    async fn set_mem_mode(store: &Store, mode: &str) -> Result<(), String> {
        let mode = go_trim(mode).to_lowercase();
        if !matches!(mode.as_str(), "" | "off" | "ondemand" | "always" | "search") {
            return Err(format!("mode mémoire invalide: {mode}"));
        }
        let active = active_project(store).await;
        let mut list = list_projects(store).await;
        let Some(p) = list.iter_mut().find(|p| p.slug == active) else {
            return Err("projet introuvable".into());
        };
        p.mem_mode = mode;
        store
            .put(BK_STATE, ST_PROJECTS, gojson::marshal(&list).into_bytes())
            .await
    }

    // --- trackers -------------------------------------------------------

    async fn tracker_load(store: &Store, proj: &str, slug: &str) -> Option<Tracker> {
        let b = store
            .get_open(BK_TRACKER, &format!("{proj}/{slug}"))
            .await
            .ok()
            .flatten()?;
        serde_json::from_slice(&b).ok()
    }

    async fn tracker_save(
        store: &Store,
        proj: &str,
        slug: &str,
        t: &mut Tracker,
    ) -> Result<(), String> {
        if let Some(ev) = t.events.as_mut() {
            ev.sort_by_key(|e| e.ts);
        }
        store
            .put_seal(
                BK_TRACKER,
                &format!("{proj}/{slug}"),
                gojson::marshal(t).into_bytes(),
            )
            .await
            .map_err(|e| match e {
                CryptError::Locked => ERR_STORE_LOCKED.to_string(),
                CryptError::Other(e) => e,
            })
    }

    /// `trackerList` : métadonnées, le plus récemment alimenté d'abord.
    async fn tracker_list(store: &Store, proj: &str) -> Vec<Value> {
        let prefix = format!("{proj}/");
        let keys: Vec<String> = store
            .all(BK_TRACKER)
            .await
            .unwrap_or_default()
            .into_keys()
            .filter(|k| k.starts_with(&prefix))
            .collect();
        let mut rows: Vec<(i64, String, String, Value)> = Vec::new();
        for k in keys {
            let slug = k[prefix.len()..].to_string();
            let Some(t) = Self::tracker_load(store, proj, &slug).await else {
                continue;
            };
            let d = TrackerDto::of(&slug, &t, false);
            rows.push((d.last_ts, d.name.clone(), slug, json!(d)));
        }
        rows.sort_by(|a, b| {
            b.0.cmp(&a.0)
                .then_with(|| a.1.cmp(&b.1))
                .then_with(|| a.2.cmp(&b.2))
        });
        rows.into_iter().map(|r| r.3).collect()
    }

    async fn tracker_action(
        &self,
        store: &Store,
        proj: &str,
        req: &gojson::Decoded,
    ) -> Result<(), String> {
        let slug = go_trim(&req.str("slug")).to_string();
        match go_trim(&req.str("action")).to_lowercase().as_str() {
            "add" => self
                .tracker_add(
                    store,
                    proj,
                    &req.str("name"),
                    &req.str("when"),
                    &req.str("text"),
                )
                .await
                .map(|_| ()),
            "edit" => {
                let mut t = Self::tracker_load(store, proj, &slug)
                    .await
                    .ok_or("tracker introuvable")?;
                let id = req.str("id");
                let (when, text) = (req.str("when"), req.str("text"));
                let ev = t.events.get_or_insert_with(Vec::new);
                let Some(e) = ev.iter_mut().find(|e| e.id == id) else {
                    return Err("événement introuvable".into());
                };
                if !go_trim(&text).is_empty() {
                    e.text = go_trim(&text).to_string();
                }
                if !go_trim(&when).is_empty() {
                    let (ts, date_only) = parse_when(&when, now_ms(), self.tz)?;
                    e.ts = ts;
                    e.date_only = date_only;
                }
                Self::tracker_save(store, proj, &slug, &mut t).await
            }
            "delete" => {
                let mut t = Self::tracker_load(store, proj, &slug)
                    .await
                    .ok_or("tracker introuvable")?;
                let id = req.str("id");
                let Some(ev) = t.events.as_mut() else {
                    return Err("événement introuvable".into());
                };
                let n = ev.len();
                ev.retain(|e| e.id != id);
                if ev.len() == n {
                    return Err("événement introuvable".into());
                }
                Self::tracker_save(store, proj, &slug, &mut t).await
            }
            "delete_tracker" => store.del(BK_TRACKER, &format!("{proj}/{slug}")).await,
            "rename" => {
                let name = go_trim(&req.str("name")).to_string();
                if name.is_empty() {
                    return Err("nom vide".into());
                }
                let mut t = Self::tracker_load(store, proj, &slug)
                    .await
                    .ok_or("tracker introuvable")?;
                let new_slug = slugify_ident(&name);
                if new_slug.is_empty() {
                    return Err("nom invalide".into());
                }
                t.name = name;
                if new_slug == slug {
                    return Self::tracker_save(store, proj, &slug, &mut t).await;
                }
                if Self::tracker_load(store, proj, &new_slug).await.is_some() {
                    return Err("un tracker du même nom existe déjà".into());
                }
                Self::tracker_save(store, proj, &new_slug, &mut t).await?;
                store.del(BK_TRACKER, &format!("{proj}/{slug}")).await
            }
            "move" => {
                let to = go_trim(&req.str("to")).to_string();
                if !list_projects(store).await.iter().any(|p| p.slug == to) {
                    return Err("projet cible introuvable".into());
                }
                if proj == to {
                    return Ok(());
                }
                let (fk, tk) = (format!("{proj}/{slug}"), format!("{to}/{slug}"));
                let raw = store.get(BK_TRACKER, &fk).await?.unwrap_or_default();
                if raw.is_empty() {
                    return Err("tracker introuvable".into());
                }
                if store
                    .get(BK_TRACKER, &tk)
                    .await?
                    .is_some_and(|v| !v.is_empty())
                {
                    return Err("un tracker du même nom existe déjà dans le projet cible".into());
                }
                store.put(BK_TRACKER, &tk, raw).await?;
                store.del(BK_TRACKER, &fk).await
            }
            _ => Err(format!(
                "action inconnue : {}",
                gojson::quote(&req.str("action"))
            )),
        }
    }

    /// `trackerAdd` : ajoute un point (tracker créé au besoin) ; rend l'id.
    async fn tracker_add(
        &self,
        store: &Store,
        proj: &str,
        name: &str,
        when: &str,
        text: &str,
    ) -> Result<String, String> {
        self.tracker_add_at(store, proj, name, when, text, now_ms())
            .await
    }

    /// [`Self::tracker_add`], « maintenant » donné (`when` vide).
    async fn tracker_add_at(
        &self,
        store: &Store,
        proj: &str,
        name: &str,
        when: &str,
        text: &str,
        now: i64,
    ) -> Result<String, String> {
        let name = go_trim(name).to_string();
        if name.is_empty() {
            return Err("nom de tracker vide".into());
        }
        if go_trim(text).is_empty() {
            return Err("contenu vide".into());
        }
        let (ts, date_only) = parse_when(when, now, self.tz)?;
        let mut slug = slugify_ident(&name);
        if slug.is_empty() {
            slug = "tracker".into();
        }
        let mut t = Self::tracker_load(store, proj, &slug)
            .await
            .unwrap_or_else(|| Tracker {
                name: name.clone(),
                events: None,
            });
        if go_trim(&t.name).is_empty() {
            t.name = name;
        }
        let id = now_nanos().to_string();
        t.events.get_or_insert_with(Vec::new).push(TrackerEvent {
            id: id.clone(),
            ts,
            text: go_trim(text).to_string(),
            date_only,
        });
        Self::tracker_save(store, proj, &slug, &mut t).await?;
        Ok(id)
    }
}

/// Point daté d'un tracker (`TrackerEvent`).
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
pub struct TrackerEvent {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub ts: i64,
    #[serde(default)]
    pub text: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub date_only: bool,
}

/// `Tracker` : `events` à `None` = `nil` du Go (`null` en JSON).
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
pub struct Tracker {
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub events: Option<Vec<TrackerEvent>>,
}

/// `trackerDTO`.
#[derive(Serialize)]
struct TrackerDto {
    slug: String,
    name: String,
    count: usize,
    first_ts: i64,
    last_ts: i64,
    last_text: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    events: Option<Vec<TrackerEvent>>,
}

impl TrackerDto {
    fn of(slug: &str, t: &Tracker, with_events: bool) -> Self {
        let ev = t.events.as_deref().unwrap_or(&[]);
        TrackerDto {
            slug: slug.into(),
            name: t.name.clone(),
            count: ev.len(),
            first_ts: ev.first().map_or(0, |e| e.ts),
            last_ts: ev.last().map_or(0, |e| e.ts),
            last_text: ev.last().map_or_else(String::new, |e| e.text.clone()),
            events: (with_events && !ev.is_empty()).then(|| ev.to_vec()),
        }
    }
}

// ---------------------------------------------------------------------------
// Routes
// ---------------------------------------------------------------------------

fn fail(code: StatusCode, e: impl Into<String>) -> Response<Body> {
    send_json(code, &json!({ "ok": false, "error": e.into() }))
}

async fn read_body(body: Incoming) -> Vec<u8> {
    Limited::new(body, 8 << 20)
        .collect()
        .await
        .map(|b| b.to_bytes().to_vec())
        .unwrap_or_default()
}

/// Types des structs anonymes des handlers Go (messages d'erreur de type).
const MOVE_REQ_TYPE: &str = r#"struct { Name string "json:\"name\""; To string "json:\"to\"" }"#;
const TRACKER_REQ_TYPE: &str = r#"struct { Action string "json:\"action\""; Slug string "json:\"slug\""; Name string "json:\"name\""; When string "json:\"when\""; Text string "json:\"text\""; ID string "json:\"id\""; To string "json:\"to\"" }"#;

const SAVE_REQ: &[(&str, Kind)] = &[
    ("name", Kind::Str),
    ("old", Kind::Str),
    ("content", Kind::Str),
];

/// `from_go` : remise par le Go (tunnel), déjà authentifiée par lui.
pub async fn serve(
    ctx: &Ctx,
    mem: &Memory,
    req: Request<Incoming>,
    from_go: bool,
) -> Response<Body> {
    let (parts, body) = req.into_parts();
    if !from_go && let Err(refus) = ctx.guard(&parts.headers).await {
        return *refus;
    }
    let store = &ctx.store;
    let path = parts.uri.path();
    // `r.URL.Query().Get` : la première valeur d'un paramètre.
    let mut query: HashMap<String, String> = HashMap::new();
    for (k, v) in url::form_urlencoded::parse(parts.uri.query().unwrap_or("").as_bytes()) {
        query
            .entry(k.into_owned())
            .or_insert_with(|| v.into_owned());
    }
    let active = active_project(store).await;
    let dir = mem.project_dir(&active);
    match path {
        "/api/mem" => {
            let name = go_trim(query.get("name").map_or("", String::as_str)).to_string();
            if name.is_empty() {
                return send_json(StatusCode::OK, &json!({ "name": "", "content": NEW_PAGE }));
            }
            let c = Memory::content(store, &dir, &name).await;
            if c.is_empty() {
                return send_json(StatusCode::NOT_FOUND, &json!({ "error": "not found" }));
            }
            send_json(StatusCode::OK, &json!({ "name": name, "content": c }))
        }
        "/api/mem/save" => {
            let d = gojson::decode_struct(&read_body(body).await, "saveReq", SAVE_REQ);
            if let Some(e) = d.err {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            let pname = project_name(&list_projects(store).await, &active);
            match Memory::save(
                store,
                &dir,
                &pname,
                &d.str("name"),
                &d.str("old"),
                &d.str("content"),
            )
            .await
            {
                Ok(()) => send_json(
                    StatusCode::OK,
                    &json!({ "ok": true, "name": d.str("name") }),
                ),
                Err(e) => fail(StatusCode::BAD_REQUEST, e),
            }
        }
        "/api/mem/delete" => {
            let d = gojson::decode_struct(&read_body(body).await, "saveReq", SAVE_REQ);
            if let Some(e) = d.err {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            match Memory::delete(store, &dir, &d.str("name")).await {
                Ok(()) => send_json(StatusCode::OK, &json!({ "ok": true })),
                Err(e) => fail(StatusCode::BAD_REQUEST, e),
            }
        }
        "/api/mem/move" => {
            if parts.method != Method::POST {
                return fail(StatusCode::METHOD_NOT_ALLOWED, "méthode non autorisée");
            }
            let d = gojson::decode_struct(
                &read_body(body).await,
                MOVE_REQ_TYPE,
                &[("name", Kind::Str), ("to", Kind::Str)],
            );
            if let Some(e) = d.err {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            match mem
                .move_page(store, &d.str("name"), &active, go_trim(&d.str("to")))
                .await
            {
                Ok(()) => send_json(StatusCode::OK, &json!({ "ok": true })),
                Err(e) => fail(StatusCode::BAD_REQUEST, e),
            }
        }
        "/api/agent" | "/api/tools" | "/api/skills" => {
            let pages: Vec<Value> = Memory::list(store, &dir)
                .await
                .into_iter()
                .map(|(n, t)| json!({ "name": n, "desc": t }))
                .collect();
            send_json(
                StatusCode::OK,
                &json!({
                    "enabled": agent_enabled(store).await,
                    "compact": compact_enabled(store).await,
                    "mem_mode": Memory::mem_mode(store).await,
                    "pages": pages,
                    "skills": pages,
                }),
            )
        }
        "/api/agent/toggle" | "/api/tools/toggle" | "/api/skills/toggle" => {
            let d = gojson::decode_struct(&read_body(body).await, "", &[("on", Kind::Bool)]);
            let res = if d.bool("on") {
                store.put(BK_STATE, ST_AGENT, b"1".to_vec()).await
            } else {
                store.del(BK_STATE, ST_AGENT).await
            };
            if let Err(e) = res {
                return fail(StatusCode::INTERNAL_SERVER_ERROR, e);
            }
            send_json(
                StatusCode::OK,
                &json!({ "ok": true, "enabled": agent_enabled(store).await }),
            )
        }
        "/api/agent/compact" => {
            let d = gojson::decode_struct(&read_body(body).await, "", &[("on", Kind::Bool)]);
            let res = if d.bool("on") {
                store.del(BK_CONFIG, "COMPACT").await
            } else {
                store.put(BK_CONFIG, "COMPACT", b"off".to_vec()).await
            };
            if let Err(e) = res {
                return fail(StatusCode::INTERNAL_SERVER_ERROR, e);
            }
            send_json(
                StatusCode::OK,
                &json!({ "ok": true, "compact": compact_enabled(store).await }),
            )
        }
        "/api/memory" => {
            if parts.method == Method::POST {
                let d = gojson::decode_struct(&read_body(body).await, "", &[("mode", Kind::Str)]);
                if let Err(e) =
                    Memory::set_mem_mode(store, normalize_mem_mode(&d.str("mode"))).await
                {
                    return fail(StatusCode::INTERNAL_SERVER_ERROR, e);
                }
            }
            send_json(
                StatusCode::OK,
                &json!({ "ok": true, "mode": Memory::mem_mode(store).await }),
            )
        }
        "/api/tracker" => match parts.method {
            Method::GET => {
                let slug = go_trim(query.get("slug").map_or("", String::as_str)).to_string();
                if slug.is_empty() {
                    return send_json(
                        StatusCode::OK,
                        &json!({ "ok": true, "trackers": Memory::tracker_list(store, &active).await }),
                    );
                }
                match Memory::tracker_load(store, &active, &slug).await {
                    Some(t) => send_json(
                        StatusCode::OK,
                        &json!({ "ok": true, "tracker": TrackerDto::of(&slug, &t, true) }),
                    ),
                    None => send_json(
                        StatusCode::NOT_FOUND,
                        &json!({ "ok": false, "error": "tracker introuvable" }),
                    ),
                }
            }
            Method::POST => {
                let d = gojson::decode_struct(
                    &read_body(body).await,
                    TRACKER_REQ_TYPE,
                    &[
                        ("action", Kind::Str),
                        ("slug", Kind::Str),
                        ("name", Kind::Str),
                        ("when", Kind::Str),
                        ("text", Kind::Str),
                        ("id", Kind::Str),
                        ("to", Kind::Str),
                    ],
                );
                if let Some(e) = d.err.clone() {
                    return fail(StatusCode::BAD_REQUEST, e);
                }
                if let Err(e) = mem.tracker_action(store, &active, &d).await {
                    return fail(StatusCode::BAD_REQUEST, e);
                }
                send_json(
                    StatusCode::OK,
                    &json!({ "ok": true, "trackers": Memory::tracker_list(store, &active).await }),
                )
            }
            _ => fail(StatusCode::METHOD_NOT_ALLOWED, "méthode non autorisée"),
        },
        _ => fail(StatusCode::NOT_FOUND, "route inconnue"),
    }
}

/// `agentEnabled`.
async fn agent_enabled(store: &Store) -> bool {
    store.get_str(BK_STATE, ST_AGENT).await == "1"
}

/// `compactEnabled`.
async fn compact_enabled(store: &Store) -> bool {
    !matches!(
        go_trim(
            store
                .config()
                .await
                .get("COMPACT")
                .map_or("", String::as_str)
        )
        .to_lowercase()
        .as_str(),
        "off" | "false" | "0" | "no" | "non"
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn noms_et_titres() {
        assert_eq!(mem_file_name(" notes ").unwrap(), "notes.md");
        assert_eq!(mem_file_name("A.MD").unwrap(), "A.MD");
        assert!(mem_file_name("a/b").is_err());
        assert_eq!(title_of("\n\n## Titre  \nx"), "Titre");
        assert_eq!(slugify_ident("Réseau NAS"), "reseau-nas");
        assert_eq!(slugify_ident("İstanbul"), "istanbul");
    }

    #[test]
    fn dates() {
        let tz = Tz::Fixed(7200);
        assert_eq!(
            parse_when("2024-03-10", 0, tz).unwrap(),
            (1_710_021_600_000, true)
        );
        assert!(!parse_when("2024-03-10T08:15", 0, tz).unwrap().1);
        assert!(parse_when("2024-02-30", 0, tz).is_err());
        assert_eq!(parse_when(" now ", 42, tz).unwrap(), (42, false));
    }
}
