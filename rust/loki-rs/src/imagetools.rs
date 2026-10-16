//! Outils d'image de l'agent (lot 45) — port de `chat_vision_tool.go`
//! (`see_image`), de `chat_screenshot.go` (`web_screenshot`, Playwright) et
//! de `prepareImageForModel` (`web_upload_orient.go`).
//!
//! `see_image` charge une image du disque dans la vision du modèle ;
//! `web_screenshot` photographie une page rendue par Playwright, la range
//! dans le dossier de la discussion et la montre au modèle s'il voit les
//! images. Les deux passent par la même préparation que les pièces jointes :
//! orientation EXIF redressée, grand côté ramené sous 1568 px (mêmes pixels
//! que le Go ; l'encodeur, lui, diffère).

use std::path::{Path, PathBuf};
use std::time::{Duration, Instant, SystemTime};

use base64::Engine as _;
use serde_json::{Map, Value, json};

use crate::convfiles::{Rgba, apply_orientation, exif_orientation, scale_down_to_max};
use crate::store::{BK_STATE, Store};

/// `maxImageDim`.
pub const MAX_IMAGE_DIM: usize = 1568;
/// `imageJPEGQuality`.
const JPEG_QUALITY: u8 = 88;
/// `maxVisionBytes`.
pub const MAX_VISION_BYTES: i64 = 12 << 20;
/// `screenshotTimeout`.
const SCREENSHOT_TIMEOUT: Duration = Duration::from_secs(90);
/// `maxCaptureFiles`, `maxCaptureBytes`.
const MAX_CAPTURE_FILES: usize = 20;
const MAX_CAPTURE_BYTES: u64 = 40 << 20;

/// `imageMimes` : les images qu'on sait montrer au modèle.
pub fn image_mime(name: &str) -> &'static str {
    let ext = Path::new(name)
        .extension()
        .map(|e| format!(".{}", e.to_string_lossy().to_lowercase()))
        .unwrap_or_default();
    match ext.as_str() {
        ".png" => "image/png",
        ".jpg" | ".jpeg" => "image/jpeg",
        ".gif" => "image/gif",
        ".webp" => "image/webp",
        ".bmp" => "image/bmp",
        _ => "",
    }
}

/// `prepareImageForModel` : orientation cuite dans les pixels, grand côté
/// sous [`MAX_IMAGE_DIM`] ; rien à faire ou image indécodable : les octets
/// d'origine.
pub fn prepare_image_for_model(raw: &[u8], mime: &str) -> (Vec<u8>, String) {
    let orient = if mime == "image/jpeg" {
        exif_orientation(raw)
    } else {
        0
    };
    let need_orient = (2..=8).contains(&orient);
    // Les formats que décode la bibliothèque standard du Go.
    let fmt = match image::guess_format(raw) {
        Ok(f @ (image::ImageFormat::Png | image::ImageFormat::Jpeg | image::ImageFormat::Gif)) => f,
        _ => return (raw.to_vec(), mime.to_string()),
    };
    let Ok(src) = image::load_from_memory_with_format(raw, fmt) else {
        return (raw.to_vec(), mime.to_string());
    };
    let (w, h) = (src.width() as usize, src.height() as usize);
    let need_resize = w > MAX_IMAGE_DIM || h > MAX_IMAGE_DIM;
    if !need_orient && !need_resize {
        return (raw.to_vec(), mime.to_string());
    }
    let mut img = Rgba {
        w,
        h,
        pix: src.to_rgba8().into_raw(),
    };
    if need_orient {
        img = apply_orientation(&img, orient);
    }
    if img.w > MAX_IMAGE_DIM || img.h > MAX_IMAGE_DIM {
        img = scale_down_to_max(&img, MAX_IMAGE_DIM);
    }
    let Some(buf) = image::RgbaImage::from_raw(img.w as u32, img.h as u32, img.pix) else {
        return (raw.to_vec(), mime.to_string());
    };
    let mut out = std::io::Cursor::new(Vec::new());
    if mime == "image/png" {
        if buf.write_to(&mut out, image::ImageFormat::Png).is_err() {
            return (raw.to_vec(), mime.to_string());
        }
        return (out.into_inner(), "image/png".into());
    }
    let enc = image::codecs::jpeg::JpegEncoder::new_with_quality(&mut out, JPEG_QUALITY);
    let rgb = image::DynamicImage::ImageRgba8(buf).to_rgb8();
    if rgb.write_with_encoder(enc).is_err() {
        return (raw.to_vec(), mime.to_string());
    }
    (out.into_inner(), "image/jpeg".into())
}

/// `imageURLPart`.
pub fn image_url_part(b: &[u8], mime: &str) -> Value {
    json!({
        "type": "image_url",
        "image_url": { "url": format!("data:{mime};base64,{}", base64::engine::general_purpose::STANDARD.encode(b)) },
    })
}

/// `engineSeesImages` : le moteur en service accepte-t-il des images ?
/// Sonde `/props` (authentifiée), gardée 10 s ; preset externe : sa case.
#[derive(Default)]
pub struct VisionProbe {
    last: tokio::sync::Mutex<Option<(Instant, bool)>>,
}

impl VisionProbe {
    pub async fn sees(&self, store: &Store, cfg: &crate::serve::Cfg) -> bool {
        if crate::serve::is_external(cfg) {
            return crate::system::vision_enabled(cfg);
        }
        let mut last = self.last.lock().await;
        if let Some((at, seen)) = *last
            && at.elapsed() < Duration::from_secs(10)
        {
            return seen;
        }
        *last = Some((Instant::now(), false));
        crate::chat::engine::install_crypto();
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(2))
            .build()
            .unwrap_or_default();
        let url = format!("http://localhost:{}/props", crate::engstatus::llm_port(cfg));
        let mut rq = client.get(url);
        let key = store.get_str(BK_STATE, "api_key").await;
        if !key.is_empty() {
            rq = rq.header("Authorization", format!("Bearer {key}"));
        }
        let Ok(resp) = rq.send().await else {
            return false;
        };
        if resp.status().as_u16() != 200 {
            return false;
        }
        let v: Value = match resp.bytes().await {
            Ok(b) => serde_json::from_slice(&b).unwrap_or_default(),
            Err(_) => return false,
        };
        let seen = v["modalities"]["vision"].as_bool().unwrap_or(false);
        *last = Some((Instant::now(), seen));
        seen
    }

    /// Oublie la dernière sonde (tests).
    pub async fn reset(&self) {
        *self.last.lock().await = None;
    }
}

/// `toolSeeImage` : (accusé, partie image). `cwd` et `target` : le contexte
/// des outils ; `sees` : la sonde du moteur, appelée en dernier.
pub async fn see_image(
    path: &str,
    cwd: &Path,
    target: &str,
    vision: bool,
    sees: impl std::future::Future<Output = bool>,
) -> (String, Option<Value>) {
    let err = |m: String| (m, None);
    if !vision {
        return err("[erreur] la vision n'est pas active sur ce preset (aucun projecteur MMPROJ configuré) — impossible de voir une image".into());
    }
    if !target.is_empty() {
        return err("[erreur] voir une image n'est pas possible sur un poste distant (le fichier est sur l'autre machine)".into());
    }
    if path.is_empty() {
        return err("[erreur] chemin de fichier manquant".into());
    }
    let abs = crate::tools::policy::resolve_agent_path(cwd, path);
    let mime = image_mime(&abs.to_string_lossy());
    if mime.is_empty() {
        return err(format!(
            "[erreur] format non reconnu comme image (attendu : png, jpg, gif, webp, bmp) : {path}"
        ));
    }
    let st = match std::fs::metadata(&abs) {
        Ok(m) => m,
        Err(_) => return err(format!("[erreur] fichier introuvable : {path}")),
    };
    if st.is_dir() {
        return err(format!("[erreur] c'est un dossier, pas une image : {path}"));
    }
    if st.len() as i64 > MAX_VISION_BYTES {
        return err(format!(
            "[erreur] image trop lourde ({}, max {}) : {path}",
            crate::downloads::human_bytes(st.len() as i64),
            crate::downloads::human_bytes(MAX_VISION_BYTES)
        ));
    }
    if !sees.await {
        return err(format!(
            "[erreur] le moteur en service ne traite pas les images (projecteur non chargé ?) — impossible de voir {path}"
        ));
    }
    let b = match std::fs::read(&abs) {
        Ok(b) => b,
        Err(e) => {
            return err(format!(
                "[erreur] lecture impossible : {}",
                crate::paths::os_err("open", &abs.to_string_lossy(), &e)
            ));
        }
    };
    if b.is_empty() {
        return err(format!("[erreur] fichier vide : {path}"));
    }
    let (b, mime) = prepare_image_for_model(&b, mime);
    let base = abs
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_default();
    (
        format!("[ok] image chargée : {base}"),
        Some(image_url_part(&b, &mime)),
    )
}

// --- web_screenshot -------------------------------------------------------------

/// Le résultat de `web_screenshot` : le texte, et la capture rangée
/// (chemin relatif à la discussion, et absolu).
#[derive(Debug, Default, Clone, PartialEq)]
pub struct Shot {
    pub result: String,
    pub rel: String,
    pub abs: PathBuf,
}

/// `safeSlug`.
pub fn safe_slug(s: &str) -> String {
    let re = regex::Regex::new(r"[^a-zA-Z0-9._-]+").expect("regex");
    let s = re.replace_all(s, "-");
    let s = s.trim_matches(['-', '.']);
    let s: String = s.chars().take(40).collect();
    if s.is_empty() { "page".into() } else { s }
}

/// `isLocalURL`. Le nom d'hôte tel qu'écrit (`url.Hostname()` du Go
/// garde la casse : `LOCALHOST` n'est pas local).
pub fn is_local_url(raw: &str) -> bool {
    if url::Url::parse(raw).is_err() {
        return false;
    }
    let host = go_hostname(raw);
    matches!(host, "localhost" | "127.0.0.1" | "::1" | "0.0.0.0") || host.ends_with(".localhost")
}

/// `url.Hostname()` : l'hôte de l'autorité, sans identifiants ni port,
/// sans crochets pour une IPv6.
fn go_hostname(raw: &str) -> &str {
    let Some(i) = raw.find("://") else {
        return "";
    };
    let rest = &raw[i + 3..];
    let auth = &rest[..rest.find(['/', '?', '#']).unwrap_or(rest.len())];
    let hp = auth.rfind('@').map_or(auth, |j| &auth[j + 1..]);
    if let Some(v6) = hp.strip_prefix('[') {
        return v6.find(']').map_or(v6, |j| &v6[..j]);
    }
    match hp.rfind(':') {
        Some(c) if hp[c + 1..].bytes().all(|b| b.is_ascii_digit()) => &hp[..c],
        _ => hp,
    }
}

/// `lastLines` : les n dernières lignes non vides, jointes par « · ».
pub fn last_lines(s: &str, n: usize) -> String {
    let keep: Vec<&str> = s.split('\n').filter(|l| !l.trim().is_empty()).collect();
    keep[keep.len().saturating_sub(n)..].join(" · ")
}

/// `pruneCaptures` : les plus anciennes captures partent tant que le
/// dossier dépasse 20 fichiers ou 40 Mo ; celle qui vient d'être prise reste.
pub fn prune_captures(dir: &Path, keep: &Path) {
    let Ok(rd) = std::fs::read_dir(dir) else {
        return;
    };
    let mut shots: Vec<(PathBuf, SystemTime, u64)> = Vec::new();
    let mut total = 0u64;
    for e in rd.flatten() {
        let Ok(m) = e.metadata() else { continue };
        if m.is_dir() {
            continue;
        }
        let p = e.path();
        total += m.len();
        if p == keep {
            continue;
        }
        shots.push((p, m.modified().unwrap_or(SystemTime::UNIX_EPOCH), m.len()));
    }
    shots.sort_by_key(|s| s.1);
    // Tous les fichiers comptent, celle qui reste comprise.
    let mut nb = shots.len() + usize::from(!keep.as_os_str().is_empty());
    for (p, _, size) in shots {
        if nb <= MAX_CAPTURE_FILES && total <= MAX_CAPTURE_BYTES {
            break;
        }
        if std::fs::remove_file(&p).is_ok() {
            total -= size;
            nb -= 1;
        }
    }
}

/// `toolWebScreenshot`. `playwright` : le binaire (vide : absent) ;
/// `conv_dir` : le dossier de la discussion ; `internet` : l'accès est-il
/// ouvert ; `stamp` : l'heure du nom de fichier.
pub async fn web_screenshot(
    args: &Map<String, Value>,
    playwright: &str,
    conv_dir: &Path,
    internet: bool,
    stamp: &str,
) -> Shot {
    let fail = |m: &str| Shot {
        result: m.to_string(),
        ..Default::default()
    };
    if playwright.is_empty() {
        return fail(
            "[erreur] Playwright n'est pas installé dans cette image (bâtie avec PLAYWRIGHT=0).",
        );
    }
    let url = args
        .get("url")
        .and_then(Value::as_str)
        .unwrap_or("")
        .trim()
        .to_string();
    if url.is_empty() {
        return fail("[erreur] url manquante");
    }
    if !url.starts_with("http://") && !url.starts_with("https://") {
        return fail("[erreur] url invalide : elle doit commencer par http:// ou https://");
    }
    if !internet && !is_local_url(&url) {
        return fail(
            "[refusé] L'accès internet est coupé : la capture ne marche que sur une adresse locale (localhost / 127.0.0.1), par exemple ton serveur de dev. Active l'accès internet pour le web.",
        );
    }
    let mut width = 1280i64;
    if let Some(v) = args.get("width").and_then(Value::as_f64)
        && (320.0..=3840.0).contains(&v)
    {
        width = v as i64;
    }
    let full_page = args.get("full_page").and_then(Value::as_bool) == Some(true);
    let dir = conv_dir.join("captures");
    if let Err(e) = crate::paths::go_mkdir_all(&dir) {
        return fail(&format!("[erreur] création du dossier de captures : {e}"));
    }
    let mut host = url.as_str();
    if let Some(i) = host.find("://") {
        host = &host[i + 3..];
    }
    if let Some(i) = host.find(['/', '?', '#']) {
        host = &host[..i];
    }
    let host = host.to_string();
    let name = format!("{}-{stamp}.jpg", safe_slug(&host));
    let out = dir.join(&name);
    let mut cmd_args: Vec<String> = [
        "screenshot",
        "--browser",
        "chromium",
        "--viewport-size",
        &format!("{width},800"),
        "--wait-for-timeout",
        "3500",
    ]
    .map(String::from)
    .to_vec();
    if full_page {
        cmd_args.push("--full-page".into());
    }
    cmd_args.push(url.clone());
    cmd_args.push(out.to_string_lossy().into_owned());
    let run = tokio::process::Command::new(playwright)
        .args(&cmd_args)
        .current_dir(&dir)
        .stdin(std::process::Stdio::null())
        .kill_on_drop(true)
        .output();
    match tokio::time::timeout(SCREENSHOT_TIMEOUT, run).await {
        Err(_) => return fail("[erreur] la page n'a pas fini de charger en 90 s"),
        // Lancement impossible : le Go ne rend que la fin de la sortie, vide.
        Ok(Err(_)) => return fail("[erreur] capture impossible : "),
        Ok(Ok(o)) if !o.status.success() => {
            let mut combined = o.stdout.clone();
            combined.extend_from_slice(&o.stderr);
            return fail(&format!(
                "[erreur] capture impossible : {}",
                last_lines(&crate::system::go_lossy(&combined), 3).trim()
            ));
        }
        Ok(Ok(_)) => {}
    }
    let size = match std::fs::metadata(&out) {
        Ok(m) if m.len() > 0 => m.len(),
        _ => return fail("[erreur] Playwright n'a produit aucune image"),
    };
    prune_captures(&dir, &out);
    let rel = format!("captures/{name}");
    Shot {
        result: format!(
            "Capture enregistrée ({rel}, {} Ko).\nPour la montrer à l'utilisateur, recopie TELLE QUELLE cette ligne markdown dans ta réponse :\n![capture de {host}](/api/chat/image?path={rel})",
            size / 1024
        ),
        rel,
        abs: out,
    }
}

/// `screenshotImageMessage` : le message qui porte la capture au modèle.
pub fn screenshot_message(abs: &Path) -> Option<Value> {
    let mime = image_mime(&abs.to_string_lossy());
    if mime.is_empty() {
        return None;
    }
    let b = std::fs::read(abs).ok().filter(|b| !b.is_empty())?;
    let (b, mime) = prepare_image_for_model(&b, mime);
    Some(json!({
        "role": "user",
        "content": [
            { "type": "text", "text": "Voici la capture demandée." },
            image_url_part(&b, &mime),
        ],
    }))
}

/// `time.Now().Format("20060102-150405")`, heure locale.
pub fn local_stamp() -> String {
    let sec = SystemTime::now()
        .duration_since(SystemTime::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or_default();
    let c = crate::tz::Location::local().civil(sec);
    format!(
        "{:04}{:02}{:02}-{:02}{:02}{:02}",
        c.year, c.month, c.day, c.hour, c.minute, c.second
    )
}

#[cfg(test)]
mod tests;
