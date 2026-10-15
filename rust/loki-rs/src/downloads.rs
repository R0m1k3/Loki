//! Téléchargements de modèles depuis Hugging Face (ou un lien direct) — port
//! des téléchargements de `backend_models.go`, `backend_models_shards.go`,
//! `backend_model_dirs.go` et `backend_hf.go` (lot 8 de la migration).
//!
//! Tenus par la façade quand `LOKI_DOWNLOADS=rust` (expérimental ; le Go
//! par défaut) : `POST /api/models/download`, `/probe`, `/cancel`,
//! `GET /api/models/download/status`. Le Go remet alors à la façade ces
//! routes quand elles lui arrivent par le tunnel du relais, et ne vide plus
//! les `.part` au démarrage (c'est la façade qui le fait).
//!
//! Comme le Go : un modèle découpé en tranches se rapatrie EN ENTIER (la
//! première désigne le modèle), les tranches déjà là ne se retéléchargent
//! pas, la place disque se vérifie sur le total, chaque tranche se répartit
//! sur plusieurs connexions quand la source honore les plages d'octets, une
//! connexion coupée reprend où elle en était, et une annulation ou un échec
//! efface ce que CE téléchargement avait écrit.
//!
//! Écarts assumés : une source qui ignore les plages (200 à la sonde) n'est
//! plus lue en entier pour rien pendant la sonde ; message d'une erreur de
//! transport (réseau, TLS) au format de reqwest ; hôte d'un lien collé en
//! majuscules converti comme les autres (`/blob/` → `/resolve/`) ; espace
//! libre inconnu hors Unix (rien n'est alors refusé, comme pour une mesure
//! ratée côté Go) ; `/status` trié par date de début (le Go rendait l'ordre
//! d'une map).

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicI64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use futures_util::StreamExt;
use http_body_util::{BodyExt, Limited};
use hyper::body::Incoming;
use hyper::{Request, Response, StatusCode};
use serde::Serialize;
use serde_json::{Value, json};
use tokio::sync::watch;

use crate::api::Ctx;
use crate::auth::json as send_json;
use crate::gguf;
use crate::paths::{base_name, go_abs, go_clean, go_join};
use crate::proxy::Body;
use crate::store::{BK_STATE, Store};

/// Connexions parallèles par tranche (`LOKI_DL_CONNS`, 16 au plus).
pub fn dl_conns() -> usize {
    std::env::var("LOKI_DL_CONNS")
        .ok()
        .and_then(|v| v.trim().parse::<usize>().ok())
        .filter(|n| *n > 0)
        .unwrap_or(8)
        .min(16)
}

/// Plus petite part qui mérite sa propre connexion (16 Mio).
const DL_MIN_CHUNK: i64 = 16 << 20;
/// Marge laissée libre après le téléchargement.
const DL_SPACE_MARGIN: i64 = 256 << 20;
/// Essais d'une part avant d'abandonner.
const DL_ATTEMPTS: u64 = 4;
/// Écrit par blocs de 1 Mio.
const DL_WRITE_BUF: usize = 1 << 20;

/// Routes servies ici quand la façade tient les téléchargements.
pub fn handles(path: &str) -> bool {
    matches!(
        path,
        "/api/models/download"
            | "/api/models/download/probe"
            | "/api/models/download/status"
            | "/api/models/download/cancel"
    )
}

// ---------------------------------------------------------------------------
// Fonctions pures (journal de référence : tests/parity_models/download.json)
// ---------------------------------------------------------------------------

/// `path.Base` sur un chemin d'URL (séparateur `/` seul).
fn url_path_base(p: &str) -> String {
    if p.is_empty() {
        return ".".into();
    }
    let t = p.trim_end_matches('/');
    if t.is_empty() {
        return "/".into();
    }
    t.rsplit('/').next().unwrap_or(t).to_string()
}

/// Décodage `%XX` d'un segment d'URL (le `u.Path` du Go est décodé).
fn percent_decode(s: &str) -> String {
    let b = s.as_bytes();
    let mut out = Vec::with_capacity(b.len());
    let mut i = 0;
    while i < b.len() {
        if b[i] == b'%'
            && i + 2 < b.len()
            && let (Some(h), Some(l)) = (hex(b[i + 1]), hex(b[i + 2]))
        {
            out.push(h << 4 | l);
            i += 3;
            continue;
        }
        out.push(b[i]);
        i += 1;
    }
    String::from_utf8_lossy(&out).into_owned()
}

fn hex(c: u8) -> Option<u8> {
    match c {
        b'0'..=b'9' => Some(c - b'0'),
        b'a'..=b'f' => Some(c - b'a' + 10),
        b'A'..=b'F' => Some(c - b'A' + 10),
        _ => None,
    }
}

/// `normalizeHFURL` : lien de page Hugging Face (`/blob/`) → lien direct
/// (`/resolve/`) ; rend l'URL à télécharger et le nom du fichier.
pub fn normalize_hf_url(raw: &str) -> Result<(String, String), String> {
    let raw = raw.trim();
    if raw.is_empty() {
        return Err("lien vide".into());
    }
    let mut u = match reqwest::Url::parse(raw) {
        Ok(u) => u,
        // Pas de schéma : le Go l'accepte et refuse ensuite le schéma vide.
        Err(url::ParseError::RelativeUrlWithoutBase) => {
            return Err("lien invalide (http/https attendu)".into());
        }
        Err(e) => return Err(format!("lien invalide: {e}")),
    };
    if u.scheme() != "http" && u.scheme() != "https" {
        return Err("lien invalide (http/https attendu)".into());
    }
    if u.host_str().is_some_and(|h| h.contains("huggingface.co")) {
        let p = u.path().replacen("/blob/", "/resolve/", 1);
        u.set_path(&p);
    }
    let name = percent_decode(&url_path_base(u.path()));
    if name.is_empty() || name == "/" || name == "." {
        return Err("impossible de déduire le nom du fichier depuis le lien".into());
    }
    if !name.to_lowercase().ends_with(".gguf") {
        return Err("le lien doit pointer vers un fichier .gguf".into());
    }
    Ok((u.to_string(), name))
}

/// `shardURLSet` : l'URL d'UNE tranche → les URLs de toute la famille, dans
/// l'ordre, avec leurs noms.
pub fn shard_url_set(dl_url: &str, name: &str) -> (Vec<String>, Vec<String>) {
    let fam = gguf::shard_family(name);
    if fam.len() == 1 {
        return (vec![dl_url.to_string()], fam);
    }
    let base = base_name(name);
    let Some(i) = dl_url.rfind(&base) else {
        return (vec![dl_url.to_string()], vec![base]);
    };
    let urls = fam
        .iter()
        .map(|n| format!("{}{n}{}", &dl_url[..i], &dl_url[i + base.len()..]))
        .collect();
    (urls, fam)
}

/// `contentRangeTotal` : la taille totale d'un « bytes 0-0/12345 ».
pub fn content_range_total(v: &str) -> i64 {
    match v.rfind('/') {
        Some(i) => v[i + 1..]
            .trim()
            .parse::<i64>()
            .ok()
            .filter(|n| *n > 0)
            .unwrap_or(0),
        None => 0,
    }
}

/// `isHFHost` : l'hôte appartient à Hugging Face (seul à recevoir le jeton).
pub fn is_hf_host(host: &str) -> bool {
    let h = host.to_lowercase();
    let h = h.split(':').next().unwrap_or("");
    h == "huggingface.co" || h.ends_with(".huggingface.co") || h == "hf.co" || h.ends_with(".hf.co")
}

fn hf_repo_ok(repo: &str) -> bool {
    let part = |s: &str| {
        let mut c = s.chars();
        c.next().is_some_and(|f| f.is_ascii_alphanumeric())
            && c.all(|ch| ch.is_ascii_alphanumeric() || matches!(ch, '.' | '_' | '-'))
    };
    match repo.split_once('/') {
        Some((a, b)) => part(a) && part(b),
        None => false,
    }
}

/// `hfRepoFromURL` : « auteur/dépôt » d'un lien de téléchargement, "" ailleurs.
pub fn hf_repo_from_url(raw: &str) -> String {
    let Ok(u) = reqwest::Url::parse(raw) else {
        return String::new();
    };
    if !u.host_str().unwrap_or("").contains("huggingface.co") {
        return String::new();
    }
    let segs: Vec<&str> = u.path().trim_matches('/').split('/').collect();
    if segs.len() < 4 || (segs[2] != "resolve" && segs[2] != "blob") {
        return String::new();
    }
    let repo = format!("{}/{}", segs[0], segs[1]);
    if hf_repo_ok(&repo) {
        repo
    } else {
        String::new()
    }
}

/// `dlFileLabel` : « nom.gguf » pour un message, "" sans nom lisible.
fn dl_file_label(dl_url: &str) -> String {
    let Ok(u) = reqwest::Url::parse(dl_url) else {
        return String::new();
    };
    let name = percent_decode(&url_path_base(u.path()));
    if name.is_empty() || name == "/" || name == "." {
        return String::new();
    }
    format!("« {name} »")
}

fn dl_access_reason(code: &str, repo: &str) -> String {
    match (code, repo.is_empty()) {
        ("GatedRepo", true) => {
            "dépôt à accès restreint : ses conditions doivent être acceptées sur huggingface.co"
                .into()
        }
        ("GatedRepo", false) => {
            format!("dépôt à accès restreint : accepte ses conditions sur huggingface.co/{repo}")
        }
        ("RepoNotFound", true) => "dépôt privé ou inexistant".into(),
        ("RepoNotFound", false) => format!("dépôt privé ou inexistant : {repo}"),
        (_, false) => format!("accès refusé par Hugging Face sur {repo}"),
        (_, true) => "accès refusé par la source".into(),
    }
}

fn dl_token_hint(token_set: bool) -> &'static str {
    if token_set {
        " — le jeton Hugging Face enregistré n'y donne pas accès (expiré, ou conditions non acceptées avec ce compte)"
    } else {
        " — puis renseigne le jeton Hugging Face (réglage « Jeton Hugging Face », ou variable d'environnement HF_TOKEN)"
    }
}

/// `dlSourceError` : un refus HTTP en phrase qui dit quoi faire.
pub fn dl_source_error(status: u16, error_code: &str, dl_url: &str, token_set: bool) -> String {
    match status {
        401 | 403 => format!(
            "{}{}",
            dl_access_reason(error_code, &hf_repo_from_url(dl_url)),
            dl_token_hint(token_set)
        ),
        404 => match error_code {
            "EntryNotFound" => format!(
                "fichier {} absent du dépôt (HTTP 404) — dépôt réorganisé depuis la copie du lien, ou fragment jamais publié (table PLE livrée à part ?)",
                dl_file_label(dl_url)
            ),
            "RevisionNotFound" => "révision introuvable dans le dépôt (HTTP 404)".into(),
            _ => "lien introuvable (HTTP 404)".into(),
        },
        416 => "la source refuse la plage d'octets demandée (HTTP 416) — fichier modifié pendant le transfert ?".into(),
        429 => "trop de requêtes vers la source (HTTP 429) — réessaie dans quelques minutes".into(),
        s if s >= 500 => format!("la source est en panne (HTTP {s}) — réessaie plus tard"),
        s => format!("HTTP {s} depuis la source"),
    }
}

/// `humanBytes`.
pub fn human_bytes(n: i64) -> String {
    if n >= 1 << 30 {
        format!("{:.1} Go", n as f64 / (1u64 << 30) as f64)
    } else if n >= 1 << 20 {
        format!("{:.0} Mo", n as f64 / (1u64 << 20) as f64)
    } else {
        format!("{n} o")
    }
}

/// `filepath.Base` (séparateur de la plateforme).
fn filepath_base(p: &str) -> String {
    if p.is_empty() {
        return ".".into();
    }
    let seps: &[char] = if cfg!(windows) { &['/', '\\'] } else { &['/'] };
    let t = p.trim_end_matches(seps);
    if t.is_empty() {
        return std::path::MAIN_SEPARATOR.to_string();
    }
    t.rsplit(seps).next().unwrap_or(t).to_string()
}

/// Les dossiers de modèles du moment (`modelsDir` + déclarés + variable).
#[derive(Debug, Clone)]
pub struct ModelDirs {
    pub home: PathBuf,
    pub stored: Vec<String>,
    pub env: Option<String>,
}

impl ModelDirs {
    pub fn models_dir(&self) -> String {
        gguf::models_dir(&self.home)
    }

    pub fn all(&self) -> Vec<String> {
        gguf::model_dirs(&self.home, &self.stored, self.env.as_deref())
    }

    /// `resolveDownloadDir` : vide = models/, sinon un des dossiers déclarés.
    pub fn resolve_download_dir(&self, dir: &str) -> Result<String, String> {
        let dir = dir.trim();
        if dir.is_empty() {
            return Ok(self.models_dir());
        }
        let abs = go_abs(dir).ok_or_else(|| "chemin invalide".to_string())?;
        self.all()
            .into_iter()
            .find(|d| gguf::norm_dir(d) == gguf::norm_dir(&abs))
            .ok_or_else(|| {
                format!("dossier non autorisé : {abs} — ajoute-le dans « Dossiers de modèles »")
            })
    }

    /// `downloadDestPath`.
    pub fn download_dest_path(&self, name: &str, dir: &str) -> Result<String, String> {
        let base = filepath_base(name.trim());
        if base.is_empty() || base == "." || base == std::path::MAIN_SEPARATOR_STR {
            return Err("nom de modèle invalide".into());
        }
        if !base.to_lowercase().ends_with(".gguf") {
            return Err("seuls les fichiers .gguf sont acceptés".into());
        }
        let d = self.resolve_download_dir(dir)?;
        Ok(go_join(&[&d, &base]))
    }

    /// `modelPickerValue` : le nom seul dans models/ (preset portable), le
    /// chemin complet ailleurs.
    pub fn model_picker_value(&self, dir: &str, base: &str) -> String {
        if gguf::norm_dir(dir) == gguf::norm_dir(&self.models_dir()) {
            base.to_string()
        } else {
            go_join(&[dir, base])
        }
    }

    /// `modelPresence` : déjà sur le disque, famille complète ? Rend la
    /// valeur de MODEL= et le chemin.
    pub fn model_presence(&self, name: &str, dir: &str) -> Option<(String, String)> {
        let base = base_name(name.trim());
        if base.is_empty() || !base.to_lowercase().ends_with(".gguf") {
            return None;
        }
        let dirs = match self.resolve_download_dir(dir) {
            Ok(d) if !dir.trim().is_empty() => vec![d],
            _ => self.all(),
        };
        for d in dirs {
            let p = go_join(&[&d, &base]);
            if !present(&p) {
                continue;
            }
            // Famille incomplète : le téléchargement doit pouvoir la compléter.
            if gguf::shard_family(&base)
                .iter()
                .any(|n| !present(&go_join(&[&d, n])))
            {
                continue;
            }
            return Some((self.model_picker_value(&d, &base), p));
        }
        None
    }
}

/// `os.Stat` sans erreur et pas un dossier.
fn present(p: &str) -> bool {
    std::fs::metadata(p).is_ok_and(|m| !m.is_dir())
}

/// `modelFamilySize` : toutes les tranches présentes.
pub fn model_family_size(p: &str) -> i64 {
    let path = Path::new(p);
    let dir = path.parent().unwrap_or(Path::new("."));
    gguf::shard_family(&base_name(p))
        .iter()
        .filter_map(|n| std::fs::metadata(dir.join(n)).ok())
        .filter(|m| !m.is_dir())
        .map(|m| m.len() as i64)
        .sum()
}

// ---------------------------------------------------------------------------
// Espace disque
// ---------------------------------------------------------------------------

#[cfg(unix)]
fn statfs(dir: &str) -> Option<libc::statfs> {
    let c = std::ffi::CString::new(dir).ok()?;
    let mut st: libc::statfs = unsafe { std::mem::zeroed() };
    // SAFETY : chemin terminé par NUL, structure de sortie initialisée.
    if unsafe { libc::statfs(c.as_ptr(), &mut st) } != 0 {
        return None;
    }
    Some(st)
}

/// `diskFreeAt` : octets libres pour l'utilisateur, -1 si inconnu.
fn disk_free_at(dir: &str) -> i64 {
    #[cfg(unix)]
    {
        match statfs(dir) {
            Some(st) => {
                // Types selon la plateforme (u32 ou u64) : les conversions ne
                // sont pas toutes superflues.
                #[allow(clippy::unnecessary_cast)]
                let free = (st.f_bavail as u64).saturating_mul(st.f_bsize as u64);
                if free > 1 << 62 { -1 } else { free as i64 }
            }
            None => -1,
        }
    }
    #[cfg(not(unix))]
    {
        let _ = dir;
        -1
    }
}

/// `diskFree` : remonte jusqu'au premier parent qui existe.
pub fn disk_free(dir: &str) -> i64 {
    let Some(mut d) = go_abs(dir.trim()) else {
        return -1;
    };
    loop {
        if Path::new(&d).is_dir() {
            return disk_free_at(&d);
        }
        let parent = crate::paths::go_dir(&d);
        if parent == d {
            return -1;
        }
        d = parent;
    }
}

/// `diskFreeReliable` : l'espace annoncé peut-il servir à REFUSER ? Non sur
/// FUSE (shfs d'Unraid…), overlayfs, NFS, SMB, Ceph, GlusterFS.
pub fn disk_free_reliable(dir: &str) -> bool {
    #[cfg(target_os = "linux")]
    {
        const UNRELIABLE: [u32; 7] = [
            0x6573_5546, // FUSE
            0x794c_7630, // overlayfs
            0x6969,      // NFS
            0x517b,      // SMB1
            0xfe53_4d42, // SMB2+
            0x00c3_6400, // Ceph
            0x5846_5342, // GlusterFS
        ];
        match statfs(dir) {
            Some(st) => !UNRELIABLE.contains(&(st.f_type as u32)),
            None => false,
        }
    }
    #[cfg(not(target_os = "linux"))]
    {
        let _ = dir;
        true
    }
}

/// `checkDiskSpace` : refus si ça ne tient pas (mesure fiable seulement).
pub fn check_disk_space(dir: &str, size: i64) -> Result<(), String> {
    let free = disk_free(dir);
    if size <= 0 || free < 0 || !disk_free_reliable(dir) {
        return Ok(());
    }
    if free < size + DL_SPACE_MARGIN {
        return Err(format!(
            "espace insuffisant sur {dir} : {} libres, {} nécessaires",
            human_bytes(free),
            human_bytes(size + DL_SPACE_MARGIN)
        ));
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// Les téléchargements
// ---------------------------------------------------------------------------

/// Un téléchargement (`dlState`), tel que `/status` le rend.
#[derive(Debug, Clone, Default, Serialize, PartialEq)]
pub struct DlState {
    pub filename: String,
    pub url: String,
    pub dir: String,
    pub total: i64,
    pub done: i64,
    pub speed: i64,
    pub conns: i64,
    pub parts: i64,
    pub part: i64,
    pub finished: bool,
    pub canceled: bool,
    #[serde(rename = "error")]
    pub err: String,
    pub started_at: i64,
}

struct Entry {
    state: Mutex<DlState>,
    cancel: watch::Sender<bool>,
}

impl Entry {
    fn with<R>(&self, f: impl FnOnce(&mut DlState) -> R) -> R {
        f(&mut self.state.lock().unwrap_or_else(|e| e.into_inner()))
    }
}

/// Une annulation, attendue ou constatée.
#[derive(Clone)]
struct Stop(watch::Receiver<bool>);

impl Stop {
    fn is_set(&self) -> bool {
        *self.0.borrow()
    }
    async fn wait(&self) {
        let mut rx = self.0.clone();
        let _ = rx.wait_for(|v| *v).await;
    }
}

/// Ce qu'un téléchargement doit savoir de la machine.
#[derive(Clone)]
pub struct DlEnv {
    /// Jeton Hugging Face ("" : aucun).
    pub hf_token: String,
    /// Version de Loki (User-Agent).
    pub version: String,
}

/// Les téléchargements de ce lancement, par nom de fichier.
pub struct Downloads {
    map: Mutex<HashMap<String, Arc<Entry>>>,
    client: reqwest::Client,
}

impl Default for Downloads {
    fn default() -> Self {
        Self::new()
    }
}

/// Fin anticipée d'une part : annulation, ou erreur (message).
enum Halt {
    Canceled,
    Err(String),
}

impl Downloads {
    pub fn new() -> Self {
        // HTTP/1.1 : de vraies connexions parallèles, sans fenêtre de flux h2
        // partagée ; pas de délai global (gros fichiers).
        crate::chat::engine::install_crypto();
        let client = reqwest::Client::builder()
            .http1_only()
            .pool_max_idle_per_host(64)
            .connect_timeout(Duration::from_secs(20))
            .build()
            .unwrap_or_default();
        Downloads {
            map: Mutex::new(HashMap::new()),
            client,
        }
    }

    fn map(&self) -> std::sync::MutexGuard<'_, HashMap<String, Arc<Entry>>> {
        self.map.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// `handleModelDownloadStatus` : tous les téléchargements connus.
    pub fn status(&self) -> Vec<DlState> {
        let mut v: Vec<DlState> = self.map().values().map(|e| e.with(|s| s.clone())).collect();
        v.sort_by_key(|s| (s.started_at, s.filename.clone()));
        v
    }

    /// Annule ; `false` : inconnu.
    pub fn cancel(&self, name: &str) -> bool {
        match self.map().get(name) {
            Some(e) => {
                let _ = e.cancel.send(true);
                true
            }
            None => false,
        }
    }

    fn request(&self, env: &DlEnv, url: &str, range: Option<&str>) -> reqwest::RequestBuilder {
        let mut rb = self
            .client
            .get(url)
            .header("User-Agent", format!("loki/{}", env.version))
            // Jamais gzip sur un .gguf : ça casse les plages d'octets.
            .header("Accept-Encoding", "identity");
        // Le jeton n'accompagne QUE les liens Hugging Face (reqwest le retire
        // quand une redirection change d'hôte).
        if !env.hf_token.is_empty()
            && reqwest::Url::parse(url)
                .ok()
                .and_then(|u| u.host_str().map(is_hf_host))
                .unwrap_or(false)
        {
            rb = rb.header("Authorization", format!("Bearer {}", env.hf_token));
        }
        if let Some(r) = range {
            rb = rb.header("Range", r);
        }
        rb
    }

    fn source_error(env: &DlEnv, resp: &reqwest::Response, url: &str) -> String {
        let code = resp
            .headers()
            .get("x-error-code")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("");
        dl_source_error(resp.status().as_u16(), code, url, !env.hf_token.is_empty())
    }

    /// `dlProbe` : un octet demandé, pour la taille et le support des plages.
    pub async fn probe(&self, env: &DlEnv, url: &str) -> Result<(i64, bool), String> {
        let resp = self
            .request(env, url, Some("bytes=0-0"))
            .send()
            .await
            .map_err(|e| e.to_string())?;
        match resp.status().as_u16() {
            206 => Ok(resp
                .headers()
                .get("content-range")
                .and_then(|v| v.to_str().ok())
                .map(content_range_total)
                .filter(|t| *t > 0)
                .map_or((0, false), |t| (t, true))),
            200 => Ok((resp.content_length().map_or(-1, |n| n as i64), false)),
            _ => Err(Self::source_error(env, &resp, url)),
        }
    }

    /// `handleModelDownload` : lance en arrière-plan ; rend la réponse.
    pub fn start(
        self: &Arc<Self>,
        env: DlEnv,
        dirs: &ModelDirs,
        url: &str,
        dir: &str,
    ) -> (StatusCode, Value) {
        let bad = |e: String| (StatusCode::BAD_REQUEST, json!({ "ok": false, "error": e }));
        let (dl_url, name) = match normalize_hf_url(url) {
            Ok(v) => v,
            Err(e) => return bad(e),
        };
        let (urls, names) = shard_url_set(&dl_url, &name);
        let name = names[0].clone();
        let mut dests = Vec::new();
        for n in &names {
            match dirs.download_dest_path(n, dir) {
                Ok(d) => dests.push(d),
                Err(e) => return bad(e),
            }
        }
        let dest_dir = crate::paths::go_dir(&dests[0]);
        if let Err(e) = std::fs::create_dir_all(&dest_dir) {
            return bad(format!(
                "dossier de destination inaccessible : {}",
                crate::paths::os_err("mkdir", &dest_dir, &e)
            ));
        }
        // Tranches déjà là : pas retéléchargées.
        let todo: Vec<(String, String)> = urls
            .into_iter()
            .zip(dests.iter().cloned())
            .filter(|(_, d)| !present(d))
            .collect();
        let mut map = self.map();
        if map.get(&name).is_some_and(|e| !e.with(|s| s.finished)) {
            return (
                StatusCode::CONFLICT,
                json!({ "ok": false, "error": format!("téléchargement déjà en cours pour {name}") }),
            );
        }
        if todo.is_empty() {
            drop(map);
            // Rien à faire : le modèle est installé ; l'UI le sélectionne.
            let value = dirs
                .model_presence(&name, dir)
                .map(|(v, _)| v)
                .unwrap_or_else(|| dirs.model_picker_value(&dest_dir, &name));
            return (
                StatusCode::OK,
                json!({ "ok": true, "filename": name, "exists": true, "value": value, "parts": 0 }),
            );
        }
        let (tx, rx) = watch::channel(false);
        let entry = Arc::new(Entry {
            state: Mutex::new(DlState {
                filename: name.clone(),
                url: todo[0].0.clone(),
                dir: dest_dir,
                parts: todo.len() as i64,
                started_at: crate::chat::persist::now_unix(),
                ..Default::default()
            }),
            cancel: tx,
        });
        map.insert(name.clone(), entry.clone());
        drop(map);
        let parts = todo.len();
        let me = self.clone();
        tokio::spawn(async move { me.run_set(env, entry, Stop(rx), todo).await });
        (
            StatusCode::OK,
            json!({ "ok": true, "filename": name, "parts": parts }),
        )
    }

    /// `runDownloadSet` : les tranches l'une après l'autre ; progression sur
    /// l'ensemble ; annulation ou échec : tout ce que CE téléchargement a
    /// écrit est effacé.
    async fn run_set(
        self: Arc<Self>,
        env: DlEnv,
        entry: Arc<Entry>,
        stop: Stop,
        todo: Vec<(String, String)>,
    ) {
        let done = Arc::new(AtomicI64::new(0));
        let finish = |res: Result<(), Halt>| {
            entry.with(|s| {
                match &res {
                    Err(Halt::Canceled) => {
                        s.canceled = true;
                        s.speed = 0;
                    }
                    _ if stop.is_set() => {
                        s.canceled = true;
                        s.speed = 0;
                    }
                    Err(Halt::Err(e)) => {
                        s.err = e.clone();
                        s.speed = 0;
                    }
                    Ok(()) => s.done = done.load(Ordering::Relaxed),
                }
                s.finished = true;
            });
        };
        // Sonde de TOUTES les tranches avant d'écrire quoi que ce soit.
        let mut sizes = Vec::new();
        let mut grand = 0i64;
        for (u, _) in &todo {
            let r = tokio::select! {
                r = self.probe(&env, u) => r.map_err(Halt::Err),
                _ = stop.wait() => Err(Halt::Canceled),
            };
            match r {
                Ok((n, ranged)) => {
                    grand += n;
                    sizes.push((n, ranged));
                }
                Err(h) => return finish(Err(h)),
            }
        }
        let dir = entry.with(|s| s.dir.clone());
        if let Err(e) = check_disk_space(&dir, grand) {
            return finish(Err(Halt::Err(e)));
        }
        entry.with(|s| {
            s.total = grand;
            s.parts = todo.len() as i64;
        });
        // Progression et vitesse lissée, une fois par seconde.
        let ticker = {
            let (entry, done) = (entry.clone(), done.clone());
            tokio::spawn(async move {
                let mut t = tokio::time::interval(Duration::from_secs(1));
                t.tick().await;
                let (mut last, mut last_at) = (0i64, tokio::time::Instant::now());
                loop {
                    let now = t.tick().await;
                    let cur = done.load(Ordering::Relaxed);
                    let dt = now.duration_since(last_at).as_secs_f64();
                    entry.with(|s| {
                        s.done = cur;
                        if dt > 0.0 {
                            let inst = ((cur - last) as f64 / dt) as i64;
                            s.speed = if s.speed == 0 {
                                inst
                            } else {
                                (s.speed * 2 + inst) / 3
                            };
                        }
                    });
                    (last, last_at) = (cur, now);
                }
            })
        };
        let mut written: Vec<String> = Vec::new();
        for (i, ((u, dest), (total, ranged))) in todo.iter().zip(sizes).enumerate() {
            entry.with(|s| s.part = i as i64 + 1);
            if let Err(h) = self
                .one_part(&env, &entry, &stop, u, dest, total, ranged, &done)
                .await
            {
                ticker.abort();
                for p in &written {
                    let _ = std::fs::remove_file(p);
                }
                return finish(Err(h));
            }
            written.push(dest.clone());
        }
        ticker.abort();
        finish(Ok(()));
    }

    /// `dlOnePart` : un fichier dans un `.part`, renommé au succès.
    #[allow(clippy::too_many_arguments)]
    async fn one_part(
        &self,
        env: &DlEnv,
        entry: &Entry,
        stop: &Stop,
        url: &str,
        dest: &str,
        total: i64,
        ranged: bool,
        done: &Arc<AtomicI64>,
    ) -> Result<(), Halt> {
        let tmp = format!("{dest}.part");
        let mut conns = 1usize;
        if ranged && total > 0 {
            let max = ((total + DL_MIN_CHUNK - 1) / DL_MIN_CHUNK) as usize;
            conns = dl_conns().min(max).max(1);
        }
        entry.with(|s| s.conns = conns as i64);
        let file = std::fs::File::create(&tmp)
            .map_err(|e| Halt::Err(crate::paths::os_err("open", &tmp, &e)))?;
        if conns > 1
            && let Err(e) = file.set_len(total as u64)
        {
            let _ = std::fs::remove_file(&tmp);
            return Err(Halt::Err(crate::paths::os_err("truncate", &tmp, &e)));
        }
        let file = Arc::new(PartFile {
            f: file,
            path: tmp.clone(),
        });
        let res = if conns <= 1 {
            self.chunk(env, stop, &file, url, 0, total - 1, total <= 0, done)
                .await
        } else {
            let size = total / conns as i64;
            let parts = (0..conns).map(|i| {
                let start = i as i64 * size;
                let end = if i == conns - 1 {
                    total - 1
                } else {
                    start + size - 1
                };
                self.chunk(env, stop, &file, url, start, end, false, done)
            });
            futures_util::future::join_all(parts)
                .await
                .into_iter()
                .find(Result::is_err)
                .unwrap_or(Ok(()))
        };
        if let Err(h) = res {
            drop(file);
            let _ = std::fs::remove_file(&tmp);
            return Err(h);
        }
        drop(file);
        std::fs::rename(&tmp, dest).map_err(|e| {
            let _ = std::fs::remove_file(&tmp);
            Halt::Err(format!(
                "rename {tmp} {dest}: {}",
                crate::tools::files::os_err_text(&e)
            ))
        })
    }

    /// `dlChunk` : [start, end] écrit en place, reprise où la connexion a
    /// lâché ; `whole` : tout le corps d'un flux (source sans plages).
    #[allow(clippy::too_many_arguments)]
    async fn chunk(
        &self,
        env: &DlEnv,
        stop: &Stop,
        file: &Arc<PartFile>,
        url: &str,
        start: i64,
        end: i64,
        whole: bool,
        done: &Arc<AtomicI64>,
    ) -> Result<(), Halt> {
        let mut pos = start;
        let mut last_err = String::new();
        for attempt in 0..DL_ATTEMPTS {
            if stop.is_set() {
                return Err(Halt::Canceled);
            }
            if attempt > 0 {
                tokio::select! {
                    _ = tokio::time::sleep(Duration::from_secs(attempt)) => {}
                    _ = stop.wait() => return Err(Halt::Canceled),
                }
            }
            let range = if !whole {
                if pos > end {
                    return Ok(());
                }
                Some(format!("bytes={pos}-{end}"))
            } else if pos > start {
                Some(format!("bytes={pos}-")) // reprise au mieux
            } else {
                None
            };
            let resp = tokio::select! {
                r = self.request(env, url, range.as_deref()).send() => r,
                _ = stop.wait() => return Err(Halt::Canceled),
            };
            let resp = match resp {
                Ok(r) => r,
                Err(e) => {
                    last_err = e.to_string();
                    continue;
                }
            };
            let status = resp.status().as_u16();
            if status != 200 && status != 206 {
                return Err(Halt::Err(Self::source_error(env, &resp, url)));
            }
            if status == 200 && pos > start {
                // Reprise refusée : le corps repart de zéro.
                done.fetch_add(start - pos, Ordering::Relaxed);
                pos = start;
            }
            match copy_at(stop, file, resp, pos, done).await {
                (n, Ok(())) => {
                    let _ = n;
                    return Ok(());
                }
                (_, Err(Halt::Canceled)) => return Err(Halt::Canceled),
                (n, Err(Halt::Err(e))) => {
                    pos += n;
                    last_err = e;
                }
            }
        }
        Err(Halt::Err(last_err))
    }
}

/// `dlCopy` : le corps écrit à partir de `off` ; rend les octets écrits,
/// même en cas d'erreur (pour reprendre).
async fn copy_at(
    stop: &Stop,
    file: &Arc<PartFile>,
    resp: reqwest::Response,
    off: i64,
    done: &Arc<AtomicI64>,
) -> (i64, Result<(), Halt>) {
    let mut stream = resp.bytes_stream();
    let mut written = 0i64;
    let mut buf: Vec<u8> = Vec::with_capacity(DL_WRITE_BUF);
    loop {
        let next = tokio::select! {
            n = stream.next() => n,
            _ = stop.wait() => return (written, Err(Halt::Canceled)),
        };
        let eof = next.is_none();
        match next {
            Some(Ok(b)) => buf.extend_from_slice(&b),
            Some(Err(e)) => {
                let n = flush(file, &mut buf, off + written).await;
                return match n {
                    Ok(k) => {
                        written += k;
                        done.fetch_add(k, Ordering::Relaxed);
                        (written, Err(Halt::Err(e.to_string())))
                    }
                    Err(we) => (written, Err(Halt::Err(we))),
                };
            }
            None => {}
        }
        if buf.len() >= DL_WRITE_BUF || (eof && !buf.is_empty()) {
            match flush(file, &mut buf, off + written).await {
                Ok(k) => {
                    written += k;
                    done.fetch_add(k, Ordering::Relaxed);
                }
                Err(e) => return (written, Err(Halt::Err(e))),
            }
        }
        if eof {
            return (written, Ok(()));
        }
    }
}

/// Écrit le tampon à sa place, hors du runtime ; le vide.
async fn flush(file: &Arc<PartFile>, buf: &mut Vec<u8>, at: i64) -> Result<i64, String> {
    let data = std::mem::take(buf);
    let n = data.len() as i64;
    let f = file.clone();
    tokio::task::spawn_blocking(move || {
        write_all_at(&f.f, &data, at as u64).map_err(|e| crate::paths::os_err("write", &f.path, &e))
    })
    .await
    .map_err(|e| e.to_string())??;
    Ok(n)
}

/// Le `.part` en cours d'écriture, et son chemin (messages d'erreur).
struct PartFile {
    f: std::fs::File,
    path: String,
}

#[cfg(unix)]
fn write_all_at(f: &std::fs::File, data: &[u8], at: u64) -> std::io::Result<()> {
    use std::os::unix::fs::FileExt;
    f.write_all_at(data, at)
}

#[cfg(windows)]
fn write_all_at(f: &std::fs::File, mut data: &[u8], mut at: u64) -> std::io::Result<()> {
    use std::os::windows::fs::FileExt;
    while !data.is_empty() {
        let n = f.seek_write(data, at)?;
        data = &data[n..];
        at += n as u64;
    }
    Ok(())
}

/// `cleanStalePartFiles` : au démarrage, les `.part` d'un téléchargement tué
/// (crash, redémarrage) ne reprendront jamais : effacés.
pub fn clean_stale_part_files(dirs: &[String]) {
    for d in dirs {
        let Ok(rd) = std::fs::read_dir(d) else {
            continue;
        };
        for e in rd.filter_map(Result::ok) {
            let name = e.file_name().to_string_lossy().into_owned();
            // `filepath.Glob("*.gguf.part")` : sensible à la casse.
            if name.ends_with(".gguf.part") && std::fs::remove_file(e.path()).is_ok() {
                println!(
                    "[models] téléchargement incomplet supprimé : {}",
                    go_clean(&e.path().to_string_lossy())
                );
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Routes
// ---------------------------------------------------------------------------

/// Les dossiers de modèles et l'environnement du téléchargement, lus en base.
async fn dirs_and_env(ctx: &Ctx, version: String) -> (ModelDirs, DlEnv) {
    let stored = gguf::parse_stored_model_dirs(
        ctx.store
            .get(BK_STATE, gguf::STATE_MODEL_DIRS_KEY)
            .await
            .ok()
            .flatten()
            .as_deref(),
    );
    let dirs = ModelDirs {
        home: ctx.engine.home.clone(),
        stored,
        env: std::env::var(gguf::MODEL_DIRS_ENV).ok(),
    };
    (
        dirs,
        DlEnv {
            hf_token: hf_token(&ctx.store).await,
            version,
        },
    )
}

/// `hfToken` : le jeton enregistré d'abord, `HF_TOKEN` ensuite.
pub async fn hf_token(store: &Store) -> String {
    let t = store.get_str(BK_STATE, "hf_token").await;
    if !t.trim().is_empty() {
        return t.trim().to_string();
    }
    std::env::var("HF_TOKEN")
        .unwrap_or_default()
        .trim()
        .to_string()
}

async fn body_json(body: Incoming) -> Result<Value, String> {
    let b = Limited::new(body, 1 << 20)
        .collect()
        .await
        .map_err(|e| e.to_string())?
        .to_bytes();
    if b.iter().all(u8::is_ascii_whitespace) {
        return Err("EOF".into());
    }
    serde_json::from_slice(&b).map_err(|e| e.to_string())
}

fn str_of<'a>(v: &'a Value, k: &str) -> &'a str {
    v.get(k).and_then(Value::as_str).unwrap_or("")
}

/// `from_go` : remise par le Go (tunnel), déjà authentifiée par lui.
pub async fn serve(
    ctx: &Ctx,
    dl: &Arc<Downloads>,
    req: Request<Incoming>,
    from_go: bool,
) -> Response<Body> {
    let (parts, body) = req.into_parts();
    // La version du Go (User-Agent) vient avec l'autorisation ; une remise du
    // Go la redemande.
    let version = if from_go {
        ctx.internal
            .auth()
            .await
            .map(|i| i.version)
            .unwrap_or_default()
    } else {
        match ctx.guard(&parts.headers).await {
            Ok(v) => v,
            Err(refus) => return *refus,
        }
    };
    let bad = |e: String| send_json(StatusCode::BAD_REQUEST, &json!({ "ok": false, "error": e }));
    match parts.uri.path() {
        "/api/models/download/status" => send_json(
            StatusCode::OK,
            &serde_json::to_value(dl.status()).unwrap_or_default(),
        ),
        "/api/models/download/cancel" => {
            let v = match body_json(body).await {
                Ok(v) => v,
                Err(e) => return bad(e),
            };
            let name = filepath_base(str_of(&v, "filename").trim());
            if dl.cancel(&name) {
                send_json(StatusCode::OK, &json!({ "ok": true }))
            } else {
                send_json(
                    StatusCode::NOT_FOUND,
                    &json!({ "ok": false, "error": format!("aucun téléchargement pour {name}") }),
                )
            }
        }
        "/api/models/download" => {
            let v = match body_json(body).await {
                Ok(v) => v,
                Err(e) => return bad(e),
            };
            let (dirs, env) = dirs_and_env(ctx, version).await;
            let (url, dir) = (str_of(&v, "url").to_string(), str_of(&v, "dir").to_string());
            let dl = dl.clone();
            // Disque (dossiers, présence, mkdir) hors du runtime ; le
            // téléchargement lui-même y repart (tokio::spawn).
            let (code, out) = tokio::task::spawn_blocking(move || dl.start(env, &dirs, &url, &dir))
                .await
                .unwrap_or_else(|e| {
                    (
                        StatusCode::INTERNAL_SERVER_ERROR,
                        json!({ "ok": false, "error": e.to_string() }),
                    )
                });
            send_json(code, &out)
        }
        "/api/models/download/probe" => {
            let v = match body_json(body).await {
                Ok(v) => v,
                Err(e) => return bad(e),
            };
            let (dirs, env) = dirs_and_env(ctx, version).await;
            let (dl_url, name) = match normalize_hf_url(str_of(&v, "url")) {
                Ok(x) => x,
                Err(e) => return bad(e),
            };
            let dir = match dirs.resolve_download_dir(str_of(&v, "dir")) {
                Ok(d) => d,
                Err(e) => return bad(e),
            };
            let (urls, names) = shard_url_set(&dl_url, &name);
            let name = names[0].clone();
            // Déjà là (dans N'IMPORTE quel dossier déclaré) : rien à sonder,
            // un modèle à sélectionner.
            if let Some((value, path)) = dirs.model_presence(&name, "") {
                return send_json(
                    StatusCode::OK,
                    &json!({
                        "ok": true, "filename": name, "dir": crate::paths::go_dir(&path),
                        "exists": true, "value": value, "size": model_family_size(&path),
                        "free": disk_free(&dir), "free_exact": disk_free_reliable(&dir),
                        "enough": true, "parts": urls.len(),
                    }),
                );
            }
            let mut total = 0i64;
            let probe_all = async {
                for u in &urls {
                    let (n, _) = dl.probe(&env, u).await?;
                    total += n;
                }
                Ok::<(), String>(())
            };
            match tokio::time::timeout(Duration::from_secs(30), probe_all).await {
                Ok(Ok(())) => {}
                Ok(Err(e)) => return bad(e),
                Err(_) => return bad("context deadline exceeded".into()),
            }
            let mut out = json!({
                "ok": true, "filename": name, "dir": dir, "size": total,
                "free": disk_free(&dir), "free_exact": disk_free_reliable(&dir),
                "enough": true, "parts": urls.len(),
            });
            if let Err(e) = check_disk_space(&dir, total) {
                out["enough"] = json!(false);
                out["error"] = json!(e);
            }
            send_json(StatusCode::OK, &out)
        }
        _ => send_json(
            StatusCode::NOT_FOUND,
            &json!({ "error": "404 page not found" }),
        ),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn noms_et_tailles() {
        assert_eq!(url_path_base("/a/b/"), "b");
        assert_eq!(url_path_base(""), ".");
        assert_eq!(url_path_base("///"), "/");
        assert_eq!(percent_decode("a%20b%zz%4"), "a b%zz%4");
        assert_eq!(human_bytes(3 << 30), "3.0 Go");
        assert_eq!(human_bytes(5 << 20), "5 Mo");
        assert_eq!(human_bytes(12), "12 o");
        assert_eq!(filepath_base(""), ".");
    }

    #[test]
    fn espace_disque() {
        let d = tempfile::tempdir().unwrap();
        let p = d.path().join("pas/encore/la");
        let free = disk_free(&p.to_string_lossy());
        assert!(free != 0, "{free}");
        assert!(check_disk_space(&d.path().to_string_lossy(), 1).is_ok());
        if free > 0 && disk_free_reliable(&d.path().to_string_lossy()) {
            let e = check_disk_space(&d.path().to_string_lossy(), free).unwrap_err();
            assert!(e.starts_with("espace insuffisant sur "), "{e}");
        }
    }

    #[test]
    fn part_perimes_effaces() {
        let d = tempfile::tempdir().unwrap();
        std::fs::write(d.path().join("m.gguf.part"), b"x").unwrap();
        std::fs::write(d.path().join("m.gguf"), b"x").unwrap();
        clean_stale_part_files(&[d.path().to_string_lossy().into_owned()]);
        assert!(!d.path().join("m.gguf.part").exists() && d.path().join("m.gguf").exists());
    }
}
