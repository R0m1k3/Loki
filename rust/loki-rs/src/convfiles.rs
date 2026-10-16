//! Fichiers de la discussion — port de `web_files.go`, `web_files_zip.go`,
//! `web_upload.go` (dépôt, téléchargement, image), `web_thumb.go`,
//! `web_upload_orient.go` (orientation, réduction) et des chemins de
//! `chat_convfiles.go` (lot 11 de la migration), servis par la façade quand
//! `LOKI_FILES=rust` (expérimental ; le Go par défaut) :
//!
//! - `POST /api/chat/upload` : dépôt en un ou plusieurs morceaux base64 ;
//! - `GET /api/chat/files` : panneau Fichiers ;
//! - `GET /api/chat/files/zip` : toute la discussion en .zip (fiche, tranches
//!   base64, ou brut) ;
//! - `POST /api/chat/file/delete` : suppression ;
//! - `GET /api/chat/file` : téléchargement (fiche, tranche base64, ou brut) ;
//! - `GET /api/chat/image` : affichage d'une image (vignette `thumb=<px>`).
//!
//! Le dossier de la discussion active reste décidé par le Go
//! (`/internal/v1/conv/workspace`), qui la connaît dans tous les cas.
//!
//! Écarts assumés : octets des vignettes JPEG (encodeur différent : mêmes
//! dimensions, même redressement, même réduction par moyennage) ; horodatage
//! des entrées .zip en UTC ; une seule plage d'octets par requête de
//! téléchargement (le Go en acceptait plusieurs, en multipart).

use std::collections::HashMap;
use std::io::{Read, Seek, Write};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use base64::Engine;
use bytes::Bytes;
use http_body_util::{BodyExt, Full, Limited, StreamBody};
use hyper::body::{Frame, Incoming};
use hyper::header::{self, HeaderMap, HeaderValue};
use hyper::{Method, Request, Response, StatusCode};
use serde::Deserialize;
use serde_json::{Value, json};

use crate::api::Ctx;
use crate::auth::json as send_json;
use crate::downloads::{disk_free, human_bytes};
use crate::paths::{go_clean, go_join};
use crate::proxy::Body;

const CONV_FILES_ROOT: &str = "discussions";
const UPLOADS_SUB: &str = "uploads";
const CAPTURE_DIR: &str = "captures";
const UPLOAD_MAX_BYTES: i64 = 1 << 30;
const UPLOAD_CHUNK_MAX: usize = 8 << 20;
const DOWNLOAD_CHUNK_MAX: i64 = 8 << 20;
const UPLOAD_SPACE_MARGIN: i64 = 512 << 20;
const UP_SESSION_TTL: Duration = Duration::from_secs(600);
const WS_WALK_LIMIT: usize = 20000;
const THUMB_MAX_SOURCE: u64 = 40 << 20;
const THUMB_CACHE_MAX: usize = 300;
/// Plus grand côté d'une image redressée pour l'affichage (vignette).
const E2E_HEADER: &str = "x-loki-e2e";

/// Routes servies ici.
pub fn handles(path: &str) -> bool {
    matches!(
        path,
        "/api/chat/upload"
            | "/api/chat/files"
            | "/api/chat/files/zip"
            | "/api/chat/file/delete"
            | "/api/chat/file"
            | "/api/chat/image"
    )
}

// ---------------------------------------------------------------------------
// Fonctions pures
// ---------------------------------------------------------------------------

/// `safeUploadName` : un nom de fichier simple, sans chemin ni caractère
/// interdit, tronqué à 120 octets en gardant l'extension.
pub fn safe_upload_name(name: &str) -> String {
    let name = name.replace('\\', "/");
    let base = name.trim_end_matches('/').rsplit('/').next().unwrap_or("");
    let base = if base.is_empty() { "." } else { base };
    let mut b = String::new();
    for r in base.chars() {
        if r.is_control() || matches!(r, '/' | '\\' | ':' | '*' | '?' | '"' | '<' | '>' | '|') {
            b.push('_');
        } else {
            b.push(r);
        }
    }
    let mut name = b
        .trim_matches(char::is_whitespace)
        .trim_matches('.')
        .to_string();
    if name.is_empty() {
        name = "fichier".into();
    }
    if name.len() > 120 {
        let mut ext = go_ext(&name).to_string();
        if ext.len() > 16 {
            ext.clear();
        }
        let mut cut = 120 - ext.len();
        while !name.is_char_boundary(cut) {
            cut -= 1;
        }
        name = format!("{}{ext}", &name[..cut]);
    }
    name
}

/// `filepath.Ext`.
fn go_ext(name: &str) -> &str {
    match name.rfind(['.', '/']) {
        Some(i) if name.as_bytes()[i] == b'.' => &name[i..],
        _ => "",
    }
}

/// `slugify` : minuscules ASCII et tirets, 40 caractères au plus.
pub fn slugify(s: &str) -> String {
    let mut b = String::new();
    for r in s.trim_matches(char::is_whitespace).to_lowercase().chars() {
        if r.is_ascii_lowercase() || r.is_ascii_digit() {
            b.push(r);
        } else if matches!(r, ' ' | '-' | '_' | '.') && !b.is_empty() && !b.ends_with('-') {
            b.push('-');
        }
        if b.len() >= 40 {
            break;
        }
    }
    b.trim_matches('-').to_string()
}

/// `safeConvID`.
fn safe_conv_id(id: &str) -> String {
    let id = id.trim();
    if id.is_empty()
        || id.len() > 64
        || !id
            .chars()
            .all(|r| r == '-' || r == '_' || r.is_numeric() || r.is_ascii_alphabetic())
    {
        return String::new();
    }
    id.to_string()
}

/// `convDirFor` : le dossier d'une discussion sous la racine de travail (la
/// racine elle-même pour un identifiant douteux).
pub fn conv_dir(root: &str, id: &str) -> String {
    let id = safe_conv_id(id);
    if id.is_empty() {
        root.to_string()
    } else {
        go_join(&[root, CONV_FILES_ROOT, &id])
    }
}

/// `exifOrientation` : le tag Orientation (1..8) d'un JPEG, 0 sinon.
pub fn exif_orientation(b: &[u8]) -> i32 {
    if b.len() < 4 || b[0] != 0xFF || b[1] != 0xD8 {
        return 0;
    }
    let mut pos = 2;
    while pos + 4 <= b.len() {
        if b[pos] != 0xFF {
            return 0;
        }
        let marker = b[pos + 1];
        if marker == 0xDA || marker == 0xD9 {
            return 0;
        }
        let seg_len = u16::from_be_bytes([b[pos + 2], b[pos + 3]]) as usize;
        if seg_len < 2 || pos + 2 + seg_len > b.len() {
            return 0;
        }
        let seg = &b[pos + 4..pos + 2 + seg_len];
        if marker == 0xE1 && seg.len() >= 6 && &seg[..6] == b"Exif\0\0" {
            return exif_from_tiff(&seg[6..]);
        }
        pos += 2 + seg_len;
    }
    0
}

fn exif_from_tiff(t: &[u8]) -> i32 {
    if t.len() < 8 {
        return 0;
    }
    let be = match &t[..2] {
        b"II" => false,
        b"MM" => true,
        _ => return 0,
    };
    let u16_at = |i: usize| {
        let x = [t[i], t[i + 1]];
        if be {
            u16::from_be_bytes(x)
        } else {
            u16::from_le_bytes(x)
        }
    };
    let u32_at = |i: usize| {
        let x = [t[i], t[i + 1], t[i + 2], t[i + 3]];
        if be {
            u32::from_be_bytes(x)
        } else {
            u32::from_le_bytes(x)
        }
    };
    if u16_at(2) != 0x2A {
        return 0;
    }
    let ifd = u32_at(4) as usize;
    if ifd < 8 || ifd + 2 > t.len() {
        return 0;
    }
    let n = u16_at(ifd) as usize;
    let mut entry = ifd + 2;
    for _ in 0..n {
        if entry + 12 > t.len() {
            return 0;
        }
        if u16_at(entry) == 0x0112 {
            return u16_at(entry + 8) as i32;
        }
        entry += 12;
    }
    0
}

/// Une image RGBA non prémultipliée (`*image.NRGBA`).
#[derive(Debug, Clone, PartialEq)]
pub struct Rgba {
    pub w: usize,
    pub h: usize,
    pub pix: Vec<u8>,
}

/// `applyOrientation` : rotations et miroirs du tag EXIF cuits dans les pixels.
pub fn apply_orientation(src: &Rgba, orient: i32) -> Rgba {
    let (w, h) = (src.w, src.h);
    let (dw, dh) = if orient >= 5 { (h, w) } else { (w, h) };
    let mut dst = vec![0u8; dw * dh * 4];
    for y in 0..h {
        for x in 0..w {
            let (nx, ny) = match orient {
                2 => (w - 1 - x, y),
                3 => (w - 1 - x, h - 1 - y),
                4 => (x, h - 1 - y),
                5 => (y, x),
                6 => (h - 1 - y, x),
                7 => (h - 1 - y, w - 1 - x),
                8 => (y, w - 1 - x),
                _ => (x, y),
            };
            let s = (y * w + x) * 4;
            let d = (ny * dw + nx) * 4;
            dst[d..d + 4].copy_from_slice(&src.pix[s..s + 4]);
        }
    }
    Rgba {
        w: dw,
        h: dh,
        pix: dst,
    }
}

/// `scaleDownToMax` : réduction par moyennage d'aire, ratio conservé.
pub fn scale_down_to_max(src: &Rgba, max: usize) -> Rgba {
    let (w, h) = (src.w, src.h);
    if w <= max && h <= max {
        return src.clone();
    }
    let scale = if h > w {
        max as f64 / h as f64
    } else {
        max as f64 / w as f64
    };
    let nw = ((w as f64 * scale).round() as usize).max(1);
    let nh = ((h as f64 * scale).round() as usize).max(1);
    let mut dst = vec![0u8; nw * nh * 4];
    for dy in 0..nh {
        let sy0 = dy * h / nh;
        let sy1 = ((dy + 1) * h / nh).max(sy0 + 1);
        for dx in 0..nw {
            let sx0 = dx * w / nw;
            let sx1 = ((dx + 1) * w / nw).max(sx0 + 1);
            let mut acc = [0u64; 4];
            let mut cnt = 0u64;
            for sy in sy0..sy1 {
                for sx in sx0..sx1 {
                    let i = (sy * w + sx) * 4;
                    for (a, p) in acc.iter_mut().zip(&src.pix[i..i + 4]) {
                        *a += *p as u64;
                    }
                    cnt += 1;
                }
            }
            let di = (dy * nw + dx) * 4;
            for c in 0..4 {
                dst[di + c] = (acc[c] / cnt) as u8;
            }
        }
    }
    Rgba {
        w: nw,
        h: nh,
        pix: dst,
    }
}

/// `http.DetectContentType`, réduit aux signatures d'images (le reste n'est
/// jamais servi comme image) ; "" si ce n'en est pas une.
pub fn sniff_image(head: &[u8]) -> &'static str {
    let starts = |sig: &[u8]| head.starts_with(sig);
    if starts(b"\x00\x00\x01\x00") || starts(b"\x00\x00\x02\x00") {
        "image/x-icon"
    } else if starts(b"BM") {
        "image/bmp"
    } else if starts(b"GIF87a") || starts(b"GIF89a") {
        "image/gif"
    } else if head.len() >= 14 && &head[..4] == b"RIFF" && &head[8..14] == b"WEBPVP" {
        "image/webp"
    } else if starts(b"\x89PNG\r\n\x1a\n") {
        "image/png"
    } else if starts(b"\xff\xd8\xff") {
        "image/jpeg"
    } else {
        ""
    }
}

/// `relWithin` : `abs` est-il DANS `root` (liens résolus des deux côtés) ? Rend
/// le chemin relatif en `/`.
pub fn rel_within(root: &str, abs: &str) -> Option<String> {
    if root.is_empty() {
        return None;
    }
    let resolve = |p: &str| {
        std::fs::canonicalize(p)
            .map(|p| p.to_string_lossy().into_owned())
            .unwrap_or_else(|_| go_clean(p))
    };
    let (root, abs) = (resolve(root), resolve(abs));
    if abs == root {
        return None;
    }
    let prefix = if root.ends_with('/') {
        root.clone()
    } else {
        format!("{root}/")
    };
    abs.strip_prefix(&prefix).map(str::to_string)
}

/// `wsResolve` : un chemin relatif du client sous la racine du panneau.
pub fn ws_resolve(root: &str, rel: &str) -> Option<String> {
    let rel = rel.trim().replace('\\', "/");
    let rel = rel.trim_start_matches('/');
    if rel.is_empty() || rel == "." {
        return Some(root.to_string());
    }
    let abs = go_join(&[root, rel]);
    rel_within(root, &abs).map(|_| abs)
}

/// `dirSize` : somme du contenu (liens non suivis), bornée.
pub fn dir_size(root: &Path) -> (i64, usize) {
    fn walk(dir: &Path, total: &mut i64, count: &mut usize) {
        if *count >= WS_WALK_LIMIT {
            return;
        }
        let Ok(rd) = std::fs::read_dir(dir) else {
            return;
        };
        let mut ents: Vec<_> = rd.filter_map(Result::ok).collect();
        ents.sort_by_key(|e| e.file_name());
        for e in ents {
            if *count >= WS_WALK_LIMIT {
                return;
            }
            let Ok(ft) = e.file_type() else { continue };
            if ft.is_symlink() {
                continue;
            }
            if ft.is_dir() {
                walk(&e.path(), total, count);
                continue;
            }
            if let Ok(m) = e.metadata() {
                *total += m.len() as i64;
                *count += 1;
            }
        }
    }
    let (mut total, mut count) = (0, 0);
    walk(root, &mut total, &mut count);
    (total, count)
}

fn mtime_secs(m: &std::fs::Metadata) -> i64 {
    m.modified()
        .ok()
        .and_then(|t| t.duration_since(UNIX_EPOCH).ok())
        .map_or(0, |d| d.as_secs() as i64)
}

/// Les extensions présentées comme images dans le panneau.
fn is_image_ext(name: &str) -> bool {
    matches!(
        go_ext(name).to_lowercase().as_str(),
        ".png" | ".jpg" | ".jpeg" | ".gif" | ".webp" | ".bmp" | ".svg" | ".avif"
    )
}

/// La discussion active vue du Go.
#[derive(Debug, Clone, Deserialize)]
pub struct Workspace {
    pub dir: String,
    pub root: String,
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub title: String,
}

impl Workspace {
    fn conv_dir_for(&self, id: &str) -> String {
        let id = safe_conv_id(id);
        if id.is_empty() {
            self.root.clone()
        } else {
            go_join(&[&self.root, CONV_FILES_ROOT, &id])
        }
    }

    /// `filesRoot` : la discussion, ou la racine pour `scope=legacy`.
    fn files_root(&self, scope: &str) -> (String, bool) {
        if scope.trim() == "legacy" {
            (self.root.clone(), true)
        } else {
            (self.dir.clone(), false)
        }
    }

    /// `workspaceFile` : discussion d'abord, emplacements d'avant ensuite.
    pub fn workspace_file(&self, rel: &str) -> Option<String> {
        let rel = rel.trim().replace('\\', "/");
        let rel = rel.trim_start_matches('/');
        if rel.is_empty() {
            return None;
        }
        let p = go_join(&[&self.dir, rel]);
        rel_within(&self.dir, &p)?;
        if std::fs::metadata(&p).is_ok() {
            return Some(p);
        }
        if let Some(l) = self.legacy_file(rel) {
            return Some(l);
        }
        Some(p)
    }

    fn legacy_file(&self, rel: &str) -> Option<String> {
        let file = |p: &str| std::fs::metadata(p).is_ok_and(|m| !m.is_dir());
        let parts: Vec<&str> = rel.splitn(3, '/').collect();
        if parts.len() == 3 && parts[0] == CAPTURE_DIR && !safe_conv_id(parts[1]).is_empty() {
            let p = go_join(&[&self.conv_dir_for(parts[1]), CAPTURE_DIR, parts[2]]);
            if file(&p) {
                return Some(p);
            }
        }
        let p = go_join(&[&self.root, rel]);
        rel_within(&self.root, &p)?;
        file(&p).then_some(p)
    }

    /// `legacyCount`.
    fn legacy_count(&self) -> usize {
        std::fs::read_dir(&self.root)
            .map(|rd| {
                rd.filter_map(Result::ok)
                    .filter(|e| {
                        let n = e.file_name().to_string_lossy().into_owned();
                        n != CONV_FILES_ROOT && !n.starts_with('.')
                    })
                    .count()
            })
            .unwrap_or(0)
    }
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

struct UpSession {
    f: std::fs::File,
    path: PathBuf,
    name: String,
    written: i64,
}

struct UpEntry {
    sess: tokio::sync::Mutex<Option<UpSession>>,
    last: Mutex<Instant>,
    busy: std::sync::atomic::AtomicBool,
}

/// Les fichiers de la discussion, tenus par la façade.
pub struct ConvFiles {
    tmp_dir: PathBuf,
    uploads: Mutex<HashMap<String, Arc<UpEntry>>>,
    thumbs: Mutex<HashMap<String, (SystemTime, u64, Bytes)>>,
}

impl Default for ConvFiles {
    fn default() -> Self {
        Self::new(std::env::temp_dir())
    }
}

fn lock<T>(m: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    m.lock().unwrap_or_else(|e| e.into_inner())
}

impl ConvFiles {
    /// `tmp_dir` : où se construisent les archives .zip.
    pub fn new(tmp_dir: PathBuf) -> Self {
        ConvFiles {
            tmp_dir,
            uploads: Mutex::new(HashMap::new()),
            thumbs: Mutex::new(HashMap::new()),
        }
    }

    /// `sweepUploadSessions` : les envois abandonnés sont fermés et effacés.
    fn sweep(&self) {
        let mut map = lock(&self.uploads);
        let stale: Vec<String> = map
            .iter()
            .filter(|(_, e)| {
                !e.busy.load(std::sync::atomic::Ordering::SeqCst)
                    && lock(&e.last).elapsed() > UP_SESSION_TTL
            })
            .map(|(k, _)| k.clone())
            .collect();
        for k in stale {
            if let Some(e) = map.remove(&k)
                && let Ok(mut s) = e.sess.try_lock()
                && let Some(s) = s.take()
            {
                let _ = std::fs::remove_file(&s.path);
            }
        }
    }

    /// `chatThumb` : vignette JPEG (grand côté borné 64..1600), en cache tant
    /// que le fichier ne change pas.
    fn thumb(&self, path: &str, meta: &std::fs::Metadata, px: i64) -> Result<Bytes, String> {
        let px = px.clamp(64, 1600) as usize;
        if meta.len() > THUMB_MAX_SOURCE {
            return Err("image trop lourde pour une vignette".into());
        }
        let key = format!("{path}\0{px}");
        let modt = meta.modified().unwrap_or(UNIX_EPOCH);
        if let Some((m, s, d)) = lock(&self.thumbs).get(&key)
            && *m == modt
            && *s == meta.len()
        {
            return Ok(d.clone());
        }
        let raw = std::fs::read(path).map_err(|e| e.to_string())?;
        let img = image::load_from_memory(&raw)
            .map_err(|_| "format non pris en charge pour une vignette".to_string())?
            .to_rgba8();
        let mut src = Rgba {
            w: img.width() as usize,
            h: img.height() as usize,
            pix: img.into_raw(),
        };
        let o = exif_orientation(&raw);
        if (2..=8).contains(&o) {
            src = apply_orientation(&src, o);
        }
        if src.w > px || src.h > px {
            src = scale_down_to_max(&src, px);
        }
        // Fond blanc sous la transparence.
        let mut rgb = Vec::with_capacity(src.w * src.h * 3);
        for p in src.pix.as_chunks::<4>().0 {
            let a = p[3] as u32;
            for c in &p[..3] {
                rgb.push(((*c as u32 * a + 255 * (255 - a) + 127) / 255) as u8);
            }
        }
        let mut out = Vec::new();
        image::codecs::jpeg::JpegEncoder::new_with_quality(&mut out, 78)
            .encode(
                &rgb,
                src.w as u32,
                src.h as u32,
                image::ExtendedColorType::Rgb8,
            )
            .map_err(|e| e.to_string())?;
        let data = Bytes::from(out);
        let mut cache = lock(&self.thumbs);
        if cache.len() >= THUMB_CACHE_MAX {
            let keys: Vec<String> = cache
                .keys()
                .take(THUMB_CACHE_MAX / 2 + 1)
                .cloned()
                .collect();
            for k in keys {
                cache.remove(&k);
            }
        }
        cache.insert(key, (modt, meta.len(), data.clone()));
        Ok(data)
    }
}

/// `cleanStaleUploadParts` : au démarrage, les envois interrompus.
pub fn clean_stale_upload_parts(root: &str) {
    let mut dirs = vec![go_join(&[root, UPLOADS_SUB])];
    if let Ok(rd) = std::fs::read_dir(go_join(&[root, CONV_FILES_ROOT])) {
        for e in rd.filter_map(Result::ok) {
            if e.file_type().is_ok_and(|t| t.is_dir()) {
                dirs.push(e.path().join(UPLOADS_SUB).to_string_lossy().into_owned());
            }
        }
    }
    for d in dirs {
        let Ok(rd) = std::fs::read_dir(&d) else {
            continue;
        };
        for e in rd.filter_map(Result::ok) {
            let n = e.file_name().to_string_lossy().into_owned();
            if !e.file_type().is_ok_and(|t| t.is_dir())
                && n.starts_with(".upload-")
                && n.ends_with(".part")
            {
                let _ = std::fs::remove_file(e.path());
            }
        }
    }
}

/// `uniqueUploadPath` : rapport.pdf, rapport-2.pdf…
fn unique_upload_path(dir: &str, name: &str) -> String {
    let p = go_join(&[dir, name]);
    if std::fs::symlink_metadata(&p).is_err() {
        return p;
    }
    let ext = go_ext(name);
    let base = &name[..name.len() - ext.len()];
    let mut p = p;
    for i in 2..1000 {
        p = go_join(&[dir, &format!("{base}-{i}{ext}")]);
        if std::fs::symlink_metadata(&p).is_err() {
            return p;
        }
    }
    p
}

/// La date locale (`2006-01-02`).
fn local_date() -> String {
    #[cfg(unix)]
    {
        let t = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map_or(0, |d| d.as_secs() as libc::time_t);
        let mut tm: libc::tm = unsafe { std::mem::zeroed() };
        // SAFETY : pointeurs valides, structure de sortie initialisée.
        if !unsafe { libc::localtime_r(&t, &mut tm) }.is_null() {
            return format!(
                "{:04}-{:02}-{:02}",
                tm.tm_year + 1900,
                tm.tm_mon + 1,
                tm.tm_mday
            );
        }
    }
    let days = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| d.as_secs() / 86400) as i64;
    let (y, m, d) = civil(days);
    format!("{y:04}-{m:02}-{d:02}")
}

/// Jours depuis 1970 → (année, mois, jour), calendrier grégorien.
fn civil(days: i64) -> (i64, u32, u32) {
    let z = days + 719468;
    let era = z.div_euclid(146097);
    let doe = z.rem_euclid(146097);
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = (doy - (153 * mp + 2) / 5 + 1) as u32;
    let m = if mp < 10 { mp + 3 } else { mp - 9 } as u32;
    (era * 400 + yoe + i64::from(m <= 2), m, d)
}

/// Date HTTP (`Last-Modified`).
fn http_date(secs: i64) -> String {
    let (y, m, d) = civil(secs.div_euclid(86400));
    let rem = secs.rem_euclid(86400);
    let wd = ["Thu", "Fri", "Sat", "Sun", "Mon", "Tue", "Wed"]
        [secs.div_euclid(86400).rem_euclid(7) as usize];
    let mon = [
        "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
    ][m as usize - 1];
    format!(
        "{wd}, {d:02} {mon} {y:04} {:02}:{:02}:{:02} GMT",
        rem / 3600,
        (rem / 60) % 60,
        rem % 60
    )
}

/// `url.PathEscape`.
fn path_escape(s: &str) -> String {
    crate::hf::go_path_escape(s)
}

// ---------------------------------------------------------------------------
// Archives
// ---------------------------------------------------------------------------

/// `buildZip` : liens ignorés, parcours borné, chemins relatifs en `/` ;
/// le dossier des discussions exclu de la vue « hors discussion ».
pub fn build_zip(root: &Path, dest: &Path, exclude_convs: bool) -> Result<usize, String> {
    let out = std::fs::File::create(dest).map_err(|e| e.to_string())?;
    let mut zw = zip::ZipWriter::new(out);
    let mut files = 0usize;
    fn walk<W: Write + Seek>(
        zw: &mut zip::ZipWriter<W>,
        dir: &Path,
        rel: &str,
        exclude: bool,
        files: &mut usize,
    ) -> Result<(), String> {
        let Ok(rd) = std::fs::read_dir(dir) else {
            return Ok(());
        };
        let mut ents: Vec<_> = rd.filter_map(Result::ok).collect();
        ents.sort_by_key(|e| e.file_name());
        for e in ents {
            if *files >= WS_WALK_LIMIT {
                return Ok(());
            }
            let Ok(ft) = e.file_type() else { continue };
            if ft.is_symlink() {
                continue;
            }
            let name = e.file_name().to_string_lossy().into_owned();
            let child = if rel.is_empty() {
                name.clone()
            } else {
                format!("{rel}/{name}")
            };
            if ft.is_dir() {
                if exclude && rel.is_empty() && name == CONV_FILES_ROOT {
                    continue;
                }
                walk(zw, &e.path(), &child, exclude, files)?;
                continue;
            }
            let Ok(meta) = e.metadata() else { continue };
            let Ok(mut f) = std::fs::File::open(e.path()) else {
                continue;
            };
            let mut opts = zip::write::SimpleFileOptions::default()
                .compression_method(zip::CompressionMethod::Deflated)
                .large_file(meta.len() >= 0xFFFF_FFFF);
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt;
                opts = opts.unix_permissions(meta.permissions().mode() & 0o777);
            }
            let secs = mtime_secs(&meta);
            let (y, mo, d) = civil(secs.div_euclid(86400));
            let rem = secs.rem_euclid(86400);
            if let Ok(dt) = zip::DateTime::from_date_and_time(
                y.clamp(1980, 2107) as u16,
                mo as u8,
                d as u8,
                (rem / 3600) as u8,
                ((rem / 60) % 60) as u8,
                (rem % 60) as u8,
            ) {
                opts = opts.last_modified_time(dt);
            }
            zw.start_file(child, opts).map_err(|e| e.to_string())?;
            std::io::copy(&mut f, zw).map_err(|e| e.to_string())?;
            *files += 1;
        }
        Ok(())
    }
    walk(&mut zw, root, "", exclude_convs, &mut files)?;
    zw.finish().map_err(|e| e.to_string())?;
    Ok(files)
}

// ---------------------------------------------------------------------------
// Routes
// ---------------------------------------------------------------------------

fn fail(code: StatusCode, msg: impl Into<String>) -> Response<Body> {
    send_json(code, &json!({ "ok": false, "error": msg.into() }))
}

fn query(uri: &hyper::Uri) -> HashMap<String, String> {
    let mut out = HashMap::new();
    for kv in uri
        .query()
        .unwrap_or("")
        .split('&')
        .filter(|s| !s.is_empty())
    {
        let (k, v) = kv.split_once('=').unwrap_or((kv, ""));
        // r.URL.Query().Get : la première valeur.
        out.entry(form_decode(k)).or_insert_with(|| form_decode(v));
    }
    out
}

fn form_decode(s: &str) -> String {
    let b = s.as_bytes();
    let mut out = Vec::with_capacity(b.len());
    let mut i = 0;
    while i < b.len() {
        match b[i] {
            b'+' => out.push(b' '),
            b'%' if i + 2 < b.len() => {
                match std::str::from_utf8(&b[i + 1..i + 3])
                    .ok()
                    .and_then(|h| u8::from_str_radix(h, 16).ok())
                {
                    Some(v) => {
                        out.push(v);
                        i += 3;
                        continue;
                    }
                    None => out.push(b'%'),
                }
            }
            c => out.push(c),
        }
        i += 1;
    }
    String::from_utf8_lossy(&out).into_owned()
}

/// `strconv.ParseInt` ou 0.
fn parse_i64(s: Option<&String>) -> i64 {
    s.and_then(|v| v.parse::<i64>().ok()).unwrap_or(0)
}

async fn workspace(ctx: &Ctx) -> Result<Workspace, String> {
    let v = ctx
        .store
        .internal_json(
            Method::POST,
            "/internal/v1/conv/workspace",
            Some(&json!({})),
        )
        .await?;
    serde_json::from_value(v).map_err(|e| e.to_string())
}

async fn blocking<T: Send + 'static>(f: impl FnOnce() -> T + Send + 'static) -> T {
    tokio::task::spawn_blocking(f)
        .await
        .expect("tâche bloquante interrompue")
}

/// Une tranche base64 d'un fichier (`b64=1&offset=&len=`).
fn b64_slice(path: &str, name: &str, size: i64, q: &HashMap<String, String>) -> Response<Body> {
    let off = parse_i64(q.get("offset"));
    let mut length = parse_i64(q.get("len"));
    if length <= 0 || length > DOWNLOAD_CHUNK_MAX {
        length = DOWNLOAD_CHUNK_MAX;
    }
    if off < 0 || off > size {
        return fail(StatusCode::BAD_REQUEST, "position hors du fichier");
    }
    if off + length > size {
        length = size - off;
    }
    let mut buf = vec![0u8; length as usize];
    let n = match std::fs::File::open(path).and_then(|mut f| {
        f.seek(std::io::SeekFrom::Start(off as u64))?;
        let mut n = 0;
        while n < buf.len() {
            match f.read(&mut buf[n..])? {
                0 => break,
                k => n += k,
            }
        }
        Ok(n)
    }) {
        Ok(n) => n,
        Err(e) => return fail(StatusCode::INTERNAL_SERVER_ERROR, e.to_string()),
    };
    send_json(
        StatusCode::OK,
        &json!({
            "ok": true, "name": name, "size": size, "offset": off,
            "data": base64::engine::general_purpose::STANDARD.encode(&buf[..n]),
            "eof": off + n as i64 >= size,
        }),
    )
}

/// Corps d'un fichier, en flux (une plage d'octets au plus).
fn serve_file(
    path: &str,
    meta: &std::fs::Metadata,
    req_headers: &HeaderMap,
    extra: &[(&'static str, String)],
) -> Response<Body> {
    let size = meta.len();
    let modified = mtime_secs(meta);
    if let Some(ims) = req_headers
        .get(header::IF_MODIFIED_SINCE)
        .and_then(|v| v.to_str().ok())
        && ims == http_date(modified)
    {
        let mut r = Response::new(Full::new(Bytes::new()).map_err(|n| match n {}).boxed());
        *r.status_mut() = StatusCode::NOT_MODIFIED;
        return r;
    }
    let (mut start, mut end, mut status) = (0u64, size.saturating_sub(1), StatusCode::OK);
    if let Some(range) = req_headers
        .get(header::RANGE)
        .and_then(|v| v.to_str().ok())
        .and_then(|r| r.strip_prefix("bytes="))
        && !range.contains(',')
        && size > 0
    {
        let (a, b) = range.split_once('-').unwrap_or((range, ""));
        let parsed = match (a.trim().parse::<u64>(), b.trim().parse::<u64>()) {
            (Ok(a), Ok(b)) if a <= b && a < size => Some((a, b.min(size - 1))),
            (Ok(a), Err(_)) if b.trim().is_empty() && a < size => Some((a, size - 1)),
            (Err(_), Ok(n)) if a.trim().is_empty() && n > 0 => {
                Some((size.saturating_sub(n), size - 1))
            }
            _ => None,
        };
        match parsed {
            Some((a, b)) => {
                (start, end, status) = (a, b, StatusCode::PARTIAL_CONTENT);
            }
            None => {
                let mut r = fail(StatusCode::RANGE_NOT_SATISFIABLE, "plage invalide");
                if let Ok(v) = HeaderValue::from_str(&format!("bytes */{size}")) {
                    r.headers_mut().insert(header::CONTENT_RANGE, v);
                }
                return r;
            }
        }
    }
    let len = if size == 0 { 0 } else { end - start + 1 };
    let path = path.to_string();
    let stream = futures_util::stream::unfold(
        (None::<std::fs::File>, start, len),
        move |(f, pos, left)| {
            let path = path.clone();
            async move {
                if left == 0 {
                    return None;
                }
                let res = tokio::task::spawn_blocking(move || {
                    let mut f = match f {
                        Some(f) => f,
                        None => {
                            let mut f = std::fs::File::open(&path)?;
                            f.seek(std::io::SeekFrom::Start(pos))?;
                            f
                        }
                    };
                    let mut buf = vec![0u8; left.min(64 << 10) as usize];
                    let n = f.read(&mut buf)?;
                    buf.truncate(n);
                    Ok::<_, std::io::Error>((f, buf))
                })
                .await
                .ok()?;
                match res {
                    Ok((_, buf)) if buf.is_empty() => None,
                    Ok((f, buf)) => {
                        let n = buf.len() as u64;
                        Some((
                            Ok::<_, hyper::Error>(Frame::data(Bytes::from(buf))),
                            (Some(f), pos + n, left - n),
                        ))
                    }
                    Err(_) => None,
                }
            }
        },
    );
    let mut r = Response::new(BodyExt::boxed(StreamBody::new(stream)));
    *r.status_mut() = status;
    let h = r.headers_mut();
    for (k, v) in extra {
        if let Ok(v) = HeaderValue::from_str(v) {
            h.insert(*k, v);
        }
    }
    h.insert(header::ACCEPT_RANGES, HeaderValue::from_static("bytes"));
    if let Ok(v) = HeaderValue::from_str(&len.to_string()) {
        h.insert(header::CONTENT_LENGTH, v);
    }
    if let Ok(v) = HeaderValue::from_str(&http_date(modified)) {
        h.insert(header::LAST_MODIFIED, v);
    }
    if status == StatusCode::PARTIAL_CONTENT
        && let Ok(v) = HeaderValue::from_str(&format!("bytes {start}-{end}/{size}"))
    {
        h.insert(header::CONTENT_RANGE, v);
    }
    r
}

#[derive(Deserialize, Default)]
struct UploadReq {
    #[serde(default)]
    name: String,
    #[serde(default)]
    data: String,
    #[serde(default)]
    id: String,
    #[serde(default)]
    more: bool,
    #[serde(default)]
    size: i64,
}

/// `from_go` : remise par le Go (tunnel), déjà authentifiée par lui.
pub async fn serve(
    ctx: &Ctx,
    cf: &Arc<ConvFiles>,
    req: Request<Incoming>,
    from_go: bool,
) -> Response<Body> {
    let (parts, body) = req.into_parts();
    if !from_go && let Err(refus) = ctx.guard(&parts.headers).await {
        return *refus;
    }
    let ws = match workspace(ctx).await {
        Ok(w) => w,
        Err(e) => {
            return fail(
                StatusCode::SERVICE_UNAVAILABLE,
                format!("fichiers indisponibles : {e}"),
            );
        }
    };
    let q = query(&parts.uri);
    let e2e = parts.headers.contains_key(E2E_HEADER);
    match parts.uri.path() {
        "/api/chat/files" => {
            let scope = q.get("scope").cloned().unwrap_or_default();
            let dir = q.get("dir").cloned().unwrap_or_default();
            blocking(move || list_files(&ws, &scope, &dir)).await
        }
        "/api/chat/file/delete" => {
            let v = read_json(body, 1 << 20).await.unwrap_or(Value::Null);
            let s = |k: &str| v.get(k).and_then(Value::as_str).unwrap_or("").to_string();
            let (path, scope) = (s("path"), s("scope"));
            blocking(move || delete_file(&ws, &path, &scope)).await
        }
        "/api/chat/file" => {
            let rel = q.get("path").cloned().unwrap_or_default();
            if rel.trim().is_empty() {
                return fail(StatusCode::BAD_REQUEST, "chemin manquant");
            }
            let Some(abs) = ws.workspace_file(&rel) else {
                return fail(StatusCode::FORBIDDEN, "hors du dossier de travail");
            };
            let meta = match std::fs::metadata(&abs) {
                Ok(m) if !m.is_dir() => m,
                _ => return fail(StatusCode::NOT_FOUND, "fichier introuvable"),
            };
            let name = crate::paths::base_name(&abs);
            if q.contains_key("meta") && !q["meta"].is_empty() {
                return send_json(
                    StatusCode::OK,
                    &json!({ "ok": true, "name": name, "size": meta.len(), "e2e": e2e }),
                );
            }
            if q.get("b64").is_some_and(|v| !v.is_empty()) {
                let size = meta.len() as i64;
                return blocking(move || b64_slice(&abs, &name, size, &q)).await;
            }
            serve_file(
                &abs,
                &meta,
                &parts.headers,
                &[
                    ("content-type", "application/octet-stream".into()),
                    ("x-content-type-options", "nosniff".into()),
                    (
                        "content-disposition",
                        format!("attachment; filename*=UTF-8''{}", path_escape(&name)),
                    ),
                ],
            )
        }
        "/api/chat/image" => {
            let rel = q.get("path").cloned().unwrap_or_default();
            if rel.trim().is_empty() {
                return fail(StatusCode::BAD_REQUEST, "chemin manquant");
            }
            let Some(abs) = ws.workspace_file(&rel) else {
                return fail(StatusCode::FORBIDDEN, "hors du dossier de travail");
            };
            let meta = match std::fs::metadata(&abs) {
                Ok(m) if !m.is_dir() => m,
                _ => return fail(StatusCode::NOT_FOUND, "fichier introuvable"),
            };
            if let Some(t) = q.get("thumb").filter(|t| !t.is_empty()) {
                let px = t.parse::<i64>().unwrap_or(0);
                let (cf2, abs2, meta2) = (cf.clone(), abs.clone(), meta.clone());
                if let Ok(data) = blocking(move || cf2.thumb(&abs2, &meta2, px)).await {
                    let mut r = Response::new(Full::new(data).map_err(|n| match n {}).boxed());
                    let h = r.headers_mut();
                    h.insert(header::CONTENT_TYPE, HeaderValue::from_static("image/jpeg"));
                    h.insert(
                        "x-content-type-options",
                        HeaderValue::from_static("nosniff"),
                    );
                    h.insert(
                        "content-security-policy",
                        HeaderValue::from_static("default-src 'none'; sandbox"),
                    );
                    h.insert(
                        header::CACHE_CONTROL,
                        HeaderValue::from_static("private, max-age=3600"),
                    );
                    return r;
                }
            }
            let mut head = [0u8; 512];
            let n = match std::fs::File::open(&abs).and_then(|mut f| {
                let mut n = 0;
                while n < head.len() {
                    match f.read(&mut head[n..])? {
                        0 => break,
                        k => n += k,
                    }
                }
                Ok(n)
            }) {
                Ok(n) => n,
                Err(e) => return fail(StatusCode::INTERNAL_SERVER_ERROR, e.to_string()),
            };
            let mime = sniff_image(&head[..n]);
            if mime.is_empty() {
                return fail(
                    StatusCode::UNSUPPORTED_MEDIA_TYPE,
                    "ce fichier n'est pas une image",
                );
            }
            let name = crate::paths::base_name(&abs);
            serve_file(
                &abs,
                &meta,
                &parts.headers,
                &[
                    ("content-type", mime.into()),
                    ("x-content-type-options", "nosniff".into()),
                    (
                        "content-security-policy",
                        "default-src 'none'; sandbox".into(),
                    ),
                    (
                        "content-disposition",
                        format!("inline; filename*=UTF-8''{}", path_escape(&name)),
                    ),
                ],
            )
        }
        "/api/chat/files/zip" => {
            let scope = q.get("scope").cloned().unwrap_or_default();
            let legacy = scope.trim() == "legacy";
            let (root, _) = ws.files_root(&scope);
            let id = if legacy {
                "legacy".to_string()
            } else {
                let id = safe_conv_id(&ws.id);
                if id.is_empty() { "conv".into() } else { id }
            };
            let tmp = cf.tmp_dir.join(format!("loki-files-{id}.zip"));
            let name = if legacy {
                format!("loki-hors-discussion-{}.zip", local_date())
            } else {
                let slug = slugify(&ws.title);
                let slug = if slug.is_empty() { ws.id.clone() } else { slug };
                format!("loki-{slug}-{}.zip", local_date())
            };
            if q.get("meta").is_some_and(|v| !v.is_empty()) {
                let tmp2 = tmp.clone();
                let res = blocking(move || build_zip(Path::new(&root), &tmp2, legacy)).await;
                let files = match res {
                    Ok(n) => n,
                    Err(e) => return fail(StatusCode::INTERNAL_SERVER_ERROR, e),
                };
                if files == 0 {
                    let _ = std::fs::remove_file(&tmp);
                    return fail(StatusCode::NOT_FOUND, "aucun fichier à archiver");
                }
                let size = std::fs::metadata(&tmp).map(|m| m.len()).unwrap_or(0);
                return send_json(
                    StatusCode::OK,
                    &json!({ "ok": true, "name": name, "size": size, "files": files, "e2e": e2e }),
                );
            }
            let meta = match std::fs::metadata(&tmp) {
                Ok(m) => m,
                Err(_) => {
                    return fail(
                        StatusCode::NOT_FOUND,
                        "archive absente — redemande la fiche (meta=1) d'abord",
                    );
                }
            };
            let tmp_s = tmp.to_string_lossy().into_owned();
            if q.get("b64").is_some_and(|v| !v.is_empty()) {
                let size = meta.len() as i64;
                return blocking(move || b64_slice(&tmp_s, &name, size, &q)).await;
            }
            serve_file(
                &tmp_s,
                &meta,
                &HeaderMap::new(),
                &[
                    ("content-type", "application/zip".into()),
                    (
                        "content-disposition",
                        format!(
                            "attachment; filename={}",
                            serde_json::to_string(&name).unwrap_or_default()
                        ),
                    ),
                ],
            )
        }
        "/api/chat/upload" => upload(cf, &ws, body).await,
        _ => send_json(
            StatusCode::NOT_FOUND,
            &json!({ "error": "404 page not found" }),
        ),
    }
}

async fn read_json(body: Incoming, max: usize) -> Result<Value, String> {
    let b = Limited::new(body, max)
        .collect()
        .await
        .map_err(|e| e.to_string())?
        .to_bytes();
    serde_json::from_slice(&b).map_err(|e| e.to_string())
}

/// `handleChatFiles`.
fn list_files(ws: &Workspace, scope: &str, dir: &str) -> Response<Body> {
    let (root, legacy) = ws.files_root(scope);
    if !legacy {
        let _ = std::fs::create_dir_all(&root);
    }
    let Some(abs) = ws_resolve(&root, dir) else {
        return fail(StatusCode::FORBIDDEN, "hors du dossier de travail");
    };
    if !std::fs::metadata(&abs).is_ok_and(|m| m.is_dir()) {
        return fail(StatusCode::NOT_FOUND, "dossier introuvable");
    }
    let rd = match std::fs::read_dir(&abs) {
        Ok(rd) => rd,
        Err(e) => return fail(StatusCode::INTERNAL_SERVER_ERROR, e.to_string()),
    };
    let mut ents: Vec<_> = rd.filter_map(Result::ok).collect();
    ents.sort_by_key(|e| e.file_name());
    let cur = rel_within(&root, &abs).unwrap_or_default();
    let mut entries: Vec<(bool, i64, Value)> = Vec::new();
    for e in ents {
        let name = e.file_name().to_string_lossy().into_owned();
        if legacy && cur.is_empty() && name == CONV_FILES_ROOT {
            continue;
        }
        let Ok(meta) = e.metadata() else { continue };
        let is_dir = e.file_type().is_ok_and(|t| t.is_dir());
        let p = if cur.is_empty() {
            name.clone()
        } else {
            format!("{cur}/{name}")
        };
        let m = mtime_secs(&meta);
        let mut it = json!({ "name": name, "path": p, "dir": is_dir, "mod": m });
        if is_dir {
            let (size, items) = dir_size(&e.path());
            it["size"] = json!(size);
            it["image"] = json!(false);
            if items > 0 {
                it["items"] = json!(items);
            }
        } else {
            it["size"] = json!(meta.len());
            it["image"] = json!(is_image_ext(&name));
        }
        entries.push((is_dir, m, it));
    }
    // Dossiers d'abord, puis du plus récent au plus ancien (tri stable).
    entries.sort_by(|a, b| b.0.cmp(&a.0).then(b.1.cmp(&a.1)));
    let parent = match cur.rfind('/') {
        Some(i) => cur[..i].to_string(),
        None => String::new(),
    };
    let (total, count) = dir_size(Path::new(&root));
    send_json(
        StatusCode::OK,
        &json!({
            "ok": true, "dir": cur, "parent": parent, "at_root": cur.is_empty(),
            "entries": entries.into_iter().map(|e| e.2).collect::<Vec<_>>(),
            "total": total, "count": count, "root": root,
            "scope": if legacy { "legacy" } else { "" }, "legacy": ws.legacy_count(),
        }),
    )
}

/// `handleChatFileDelete`.
fn delete_file(ws: &Workspace, path: &str, scope: &str) -> Response<Body> {
    if path.trim().is_empty() {
        return fail(StatusCode::BAD_REQUEST, "chemin manquant");
    }
    let (root, legacy) = ws.files_root(scope);
    let Some(abs) = ws_resolve(&root, path) else {
        return fail(StatusCode::FORBIDDEN, "hors du dossier de travail");
    };
    if abs == root {
        return fail(
            StatusCode::FORBIDDEN,
            "le dossier de travail lui-même ne se supprime pas",
        );
    }
    if legacy && abs == go_join(&[&ws.root, CONV_FILES_ROOT]) {
        return fail(
            StatusCode::FORBIDDEN,
            "les dossiers des discussions se suppriment discussion par discussion",
        );
    }
    let meta = match std::fs::symlink_metadata(&abs) {
        Ok(m) => m,
        Err(_) => return fail(StatusCode::NOT_FOUND, "introuvable"),
    };
    let (freed, res) = if meta.is_dir() {
        (dir_size(Path::new(&abs)).0, std::fs::remove_dir_all(&abs))
    } else {
        (meta.len() as i64, std::fs::remove_file(&abs))
    };
    if let Err(e) = res {
        return fail(
            StatusCode::INTERNAL_SERVER_ERROR,
            crate::paths::os_err("unlinkat", &abs, &e),
        );
    }
    send_json(StatusCode::OK, &json!({ "ok": true, "freed": freed }))
}

/// `handleChatUpload` : un morceau ; le dernier donne au fichier son nom.
async fn upload(cf: &Arc<ConvFiles>, ws: &Workspace, body: Incoming) -> Response<Body> {
    let req: UploadReq = match Limited::new(body, 2 * UPLOAD_CHUNK_MAX)
        .collect()
        .await
        .ok()
        .and_then(|b| serde_json::from_slice(&b.to_bytes()).ok())
    {
        Some(r) => r,
        None => {
            return fail(
                StatusCode::BAD_REQUEST,
                "morceau trop gros ou requête invalide",
            );
        }
    };
    let mut data = req.data.as_str();
    if data.starts_with("data:")
        && let Some(i) = data.find(',')
    {
        data = &data[i + 1..];
    }
    // StdEncoding.DecodeString ignore les retours à la ligne.
    let cleaned: String = data
        .trim()
        .chars()
        .filter(|c| *c != '\r' && *c != '\n')
        .collect();
    let raw = match base64::engine::general_purpose::STANDARD.decode(cleaned) {
        Ok(r) => r,
        Err(_) => {
            return fail(
                StatusCode::BAD_REQUEST,
                "contenu illisible (base64 attendu)",
            );
        }
    };
    // Le verrou de la table ne couvre que la recherche ou la création de la
    // session : jamais d'attente sous lui.
    let found: Result<(String, Arc<UpEntry>), Box<Response<Body>>> = (|| {
        cf.sweep();
        let mut map = lock(&cf.uploads);
        if let Some(e) = map.get(&req.id) {
            return Ok((req.id.clone(), e.clone()));
        }
        if !req.id.is_empty() {
            return Err(Box::new(fail(
                StatusCode::CONFLICT,
                "envoi expiré — recommence le fichier",
            )));
        }
        let dir = go_join(&[&ws.dir, UPLOADS_SUB]);
        if let Err(e) = std::fs::create_dir_all(&dir) {
            return Err(Box::new(fail(
                StatusCode::INTERNAL_SERVER_ERROR,
                e.to_string(),
            )));
        }
        let free = disk_free(&dir);
        if free > 0 && req.size > 0 && free < req.size + UPLOAD_SPACE_MARGIN {
            return Err(Box::new(send_json(
                StatusCode::INSUFFICIENT_STORAGE,
                &json!({ "ok": false, "error": format!(
                    "espace insuffisant : {} libres, {} nécessaires",
                    human_bytes(free), human_bytes(req.size + UPLOAD_SPACE_MARGIN)) }),
            )));
        }
        let (f, path) =
            create_part(&dir).map_err(|e| Box::new(fail(StatusCode::INTERNAL_SERVER_ERROR, e)))?;
        let id = path
            .file_name()
            .map(|n| n.to_string_lossy().into_owned())
            .unwrap_or_default();
        let e = Arc::new(UpEntry {
            sess: tokio::sync::Mutex::new(Some(UpSession {
                f,
                path,
                name: safe_upload_name(&req.name),
                written: 0,
            })),
            last: Mutex::new(Instant::now()),
            busy: std::sync::atomic::AtomicBool::new(false),
        });
        map.insert(id.clone(), e.clone());
        Ok((id, e))
    })();
    let (id, entry) = match found {
        Ok(x) => x,
        Err(r) => return *r,
    };
    finish_chunk(cf, &id, entry, raw, req.more).await
}

/// `.upload-<nombre>.part`, créé exclusivement (`os.CreateTemp`).
fn create_part(dir: &str) -> Result<(std::fs::File, PathBuf), String> {
    for _ in 0..10000 {
        let mut b = [0u8; 4];
        getrandom::fill(&mut b).map_err(|e| e.to_string())?;
        let p = PathBuf::from(go_join(&[
            dir,
            &format!(".upload-{}.part", u32::from_le_bytes(b)),
        ]));
        match std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&p)
        {
            Ok(f) => return Ok((f, p)),
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => continue,
            Err(e) => return Err(e.to_string()),
        }
    }
    Err("aucun nom de fichier temporaire libre".into())
}

async fn finish_chunk(
    cf: &Arc<ConvFiles>,
    id: &str,
    entry: Arc<UpEntry>,
    raw: Vec<u8>,
    more: bool,
) -> Response<Body> {
    use std::sync::atomic::Ordering;
    *lock(&entry.last) = Instant::now();
    entry.busy.store(true, Ordering::SeqCst);
    let mut guard = entry.sess.lock().await;
    let res = (|| {
        let Some(s) = guard.as_mut() else {
            return Err((
                StatusCode::CONFLICT,
                "envoi expiré — recommence le fichier".to_string(),
            ));
        };
        let drop_sess = |s: &UpSession| {
            lock(&cf.uploads).remove(id);
            let _ = std::fs::remove_file(&s.path);
        };
        if s.written + raw.len() as i64 > UPLOAD_MAX_BYTES {
            drop_sess(s);
            return Err((
                StatusCode::PAYLOAD_TOO_LARGE,
                format!("fichier trop gros (max {})", human_bytes(UPLOAD_MAX_BYTES)),
            ));
        }
        if !raw.is_empty() {
            if let Err(e) = s.f.write_all(&raw) {
                drop_sess(s);
                return Err((StatusCode::INTERNAL_SERVER_ERROR, e.to_string()));
            }
            s.written += raw.len() as i64;
        }
        if more {
            return Ok(json!({ "ok": true, "id": id, "received": s.written }));
        }
        let sync = s.f.sync_all();
        lock(&cf.uploads).remove(id);
        let part = s.path.clone();
        if let Err(e) = sync {
            let _ = std::fs::remove_file(&part);
            return Err((StatusCode::INTERNAL_SERVER_ERROR, e.to_string()));
        }
        if s.written == 0 {
            let _ = std::fs::remove_file(&part);
            return Err((StatusCode::BAD_REQUEST, "fichier vide".to_string()));
        }
        let dir = part
            .parent()
            .map(|p| p.to_string_lossy().into_owned())
            .unwrap_or_default();
        let dest = unique_upload_path(&dir, &s.name);
        if let Err(e) = std::fs::rename(&part, &dest) {
            let _ = std::fs::remove_file(&part);
            return Err((StatusCode::INTERNAL_SERVER_ERROR, e.to_string()));
        }
        Ok(json!({
            "ok": true,
            "path": format!("uploads/{}", crate::paths::base_name(&dest)),
            "abs": dest,
            "size": s.written,
        }))
    })();
    if !more || res.is_err() {
        guard.take();
    }
    drop(guard);
    entry.busy.store(false, Ordering::SeqCst);
    match res {
        Ok(v) => send_json(StatusCode::OK, &v),
        Err((code, msg)) => fail(code, msg),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn dates() {
        assert_eq!(civil(0), (1970, 1, 1));
        assert_eq!(civil(19723), (2024, 1, 1));
        assert_eq!(http_date(1700000000), "Tue, 14 Nov 2023 22:13:20 GMT");
        assert_eq!(local_date().len(), 10);
    }

    #[test]
    fn extensions() {
        assert_eq!(go_ext("a.tar.gz"), ".gz");
        assert_eq!(go_ext("rep.d/fichier"), "");
        assert_eq!(go_ext("sans"), "");
    }
}
