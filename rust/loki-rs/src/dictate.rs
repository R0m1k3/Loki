//! Dictée vocale — port de `dictate_config.go`, `dictate_models.go`,
//! `dictate_server.go` et `web_transcribe.go` (lot 9 de la migration), servie
//! par la façade quand `LOKI_DICTATE=rust` (expérimental ; le Go par défaut) :
//!
//! - `POST /api/transcribe` : un WAV entre, le texte sort ;
//! - `GET|POST /api/dictate/config` : modèle et réactivité ;
//! - `GET /api/dictate/models` : catalogue, présence sur le disque ;
//! - `GET|POST /api/dictate/models/download` : téléchargement d'un modèle ;
//! - `GET /api/dictate/state` : serveur allumé, modèle, téléchargement.
//!
//! La transcription est locale : Parakeet servi par
//! `sherpa-onnx-offline-websocket-server`, lancé à la demande, arrêté après
//! dix minutes d'inactivité ou quand les réglages changent. Le Go demande son
//! arrêt (libération de la VRAM, optimiseur) par
//! `POST /internal/facade/v1/dictate/stop`.
//!
//! Écarts assumés : messages d'erreur de transport (WebSocket, HTTP) et de
//! décompression au format des crates Rust ; corps JSON illisible : « requête
//! illisible » comme le Go, quelle que soit la faute.

use std::io::Read;
use std::path::{Path, PathBuf};
use std::process::{ExitStatus, Stdio};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use bytes::Bytes;
use futures_util::{SinkExt, StreamExt};
use http_body_util::{BodyExt, Limited};
use hyper::body::Incoming;
use hyper::{Method, Request, Response, StatusCode};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tokio::io::AsyncReadExt;
use tokio_tungstenite::tungstenite::Message;

use crate::api::Ctx;
use crate::auth::json as send_json;
use crate::proxy::Body;
use crate::store::BK_STATE;

pub const DICTATE_CFG_KEY: &str = "dictate_cfg";
/// Les archives sherpa-onnx officielles.
pub const ASR_MODEL_BASE: &str =
    "https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/";
/// Les quatre fichiers d'un modèle utilisable.
pub const ASR_FICHIERS: [&str; 4] = [
    "encoder.int8.onnx",
    "decoder.int8.onnx",
    "joiner.int8.onnx",
    "tokens.txt",
];
const DEFAULT_MODEL: &str = "parakeet-tdt-0.6b-v3";
/// En-tête du protocole : fréquence, puis nombre d'octets d'audio.
const ENTETE: usize = 8;
/// Taille d'une trame WebSocket envoyée (celle du client de référence).
const TRAME_MAX: usize = 10240;
/// Le serveur s'éteint après ce délai sans dictée.
pub const ASR_IDLE: Duration = Duration::from_secs(600);
/// Plafond d'attente au démarrage (modèle de 600 Mo sur un disque lent).
pub const ASR_READY: Duration = Duration::from_secs(120);

/// Routes servies ici.
pub fn handles(path: &str) -> bool {
    matches!(
        path,
        "/api/transcribe"
            | "/api/dictate/config"
            | "/api/dictate/models"
            | "/api/dictate/models/download"
            | "/api/dictate/state"
    )
}

/// La route interne par laquelle le Go demande l'arrêt du serveur.
pub const INTERNAL_STOP: &str = "/internal/facade/v1/dictate/stop";

// ---------------------------------------------------------------------------
// Catalogue et réglages
// ---------------------------------------------------------------------------

/// Un modèle du catalogue (`asrModel`).
pub struct AsrModel {
    pub id: &'static str,
    pub nom: &'static str,
    pub dossier: &'static str,
    pub octets: i64,
    pub langues: &'static str,
}

pub const CATALOGUE: [AsrModel; 2] = [
    AsrModel {
        id: "parakeet-tdt-0.6b-v3",
        nom: "Parakeet v3 — recommandé",
        dossier: "sherpa-onnx-nemo-parakeet-tdt-0.6b-v3-int8",
        octets: 487_170_055,
        langues: "25 langues européennes, français inclus — détection automatique",
    },
    AsrModel {
        id: "parakeet-tdt-0.6b-v2",
        nom: "Parakeet v2 — anglais seul",
        dossier: "sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8",
        octets: 482_468_385,
        langues: "anglais uniquement",
    },
];

fn model(id: &str) -> Option<&'static AsrModel> {
    CATALOGUE.iter().find(|m| m.id == id)
}

/// Réglages (`DictateCfg`), rangés dans `state/dictate_cfg`.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct DictateCfg {
    #[serde(default)]
    pub model: String,
    #[serde(default)]
    pub reactivity: String,
}

impl DictateCfg {
    /// `withDefauts` : modèle hors catalogue → v3 ; réactivité vide → moyen.
    pub fn with_defauts(mut self) -> Self {
        if model(&self.model).is_none() {
            self.model = DEFAULT_MODEL.into();
        }
        if self.reactivity.is_empty() {
            self.reactivity = "moyen".into();
        }
        self
    }
}

/// `asrRoot` : `LOKI_HOME/asr`.
pub fn asr_root(home: &Path) -> PathBuf {
    home.join("asr")
}

/// `asrModelDirFor` : `None` si l'identifiant est inconnu.
pub fn model_dir_for(home: &Path, id: &str) -> Option<PathBuf> {
    model(id).map(|m| asr_root(home).join(m.dossier))
}

/// `asrModelURLFor` : "" si l'identifiant est inconnu.
pub fn model_url_for(base: &str, id: &str) -> String {
    model(id).map_or(String::new(), |m| format!("{base}{}.tar.bz2", m.dossier))
}

/// `asrModelPresent` : les quatre fichiers, non vides.
pub fn model_present(home: &Path, id: &str) -> bool {
    let Some(dir) = model_dir_for(home, id) else {
        return false;
    };
    ASR_FICHIERS
        .iter()
        .all(|f| std::fs::metadata(dir.join(f)).is_ok_and(|m| m.len() > 0))
}

/// `asrCatalogueTrie` : du plus léger au plus lourd.
pub fn catalogue_trie(home: &Path) -> Value {
    let mut v: Vec<&AsrModel> = CATALOGUE.iter().collect();
    v.sort_by_key(|m| m.octets);
    Value::Array(
        v.into_iter()
            .map(|m| {
                json!({ "id": m.id, "nom": m.nom, "octets": m.octets, "langues": m.langues, "present": model_present(home, m.id) })
            })
            .collect(),
    )
}

/// `asrServerArgs` : `--clef=valeur`, écoute sur la boucle locale seulement.
pub fn server_args(home: &Path, cfg: &DictateCfg, port: u16) -> Result<Vec<String>, String> {
    let dir = model_dir_for(home, &cfg.model)
        .ok_or_else(|| format!("modèle de dictée inconnu : {}", cfg.model))?;
    let f = |n: &str| dir.join(n).to_string_lossy().into_owned();
    Ok(vec![
        format!("--encoder={}", f("encoder.int8.onnx")),
        format!("--decoder={}", f("decoder.int8.onnx")),
        format!("--joiner={}", f("joiner.int8.onnx")),
        format!("--tokens={}", f("tokens.txt")),
        "--model-type=nemo_transducer".into(),
        format!("--port={port}"),
        "--host=127.0.0.1".into(),
    ])
}

/// `asrMarqueur` : vide, ou annotation entre crochets ou parenthèses.
pub fn marqueur(s: &str) -> bool {
    let s = s.trim();
    s.is_empty()
        || (s.starts_with('[') && s.ends_with(']'))
        || (s.starts_with('(') && s.ends_with(')'))
}

/// `lastLine` : la dernière ligne non vide.
pub fn last_line(s: &str) -> String {
    s.trim()
        .split('\n')
        .rev()
        .map(str::trim)
        .find(|l| !l.is_empty())
        .unwrap_or("")
        .to_string()
}

// ---------------------------------------------------------------------------
// WAV
// ---------------------------------------------------------------------------

fn u16le(b: &[u8], i: usize) -> u16 {
    u16::from_le_bytes([b[i], b[i + 1]])
}

fn u32le(b: &[u8], i: usize) -> u32 {
    u32::from_le_bytes([b[i], b[i + 1], b[i + 2], b[i + 3]])
}

/// `wavVersFloat32` : fréquence et échantillons dans [-1, 1], en parcourant
/// les blocs (`LIST` & co peuvent précéder `data`).
pub fn wav_to_f32(wav: &[u8]) -> Result<(u32, Vec<f32>), String> {
    if wav.len() < 12 || &wav[0..4] != b"RIFF" || &wav[8..12] != b"WAVE" {
        return Err("audio illisible : ce n'est pas un WAV".into());
    }
    let (mut canaux, mut bits, mut format, mut taux) = (0usize, 0u16, 0u16, 0u32);
    let mut pos = 12usize;
    while pos + 8 <= wav.len() {
        let id = &wav[pos..pos + 4];
        let mut taille = u32le(wav, pos + 4) as usize;
        let corps = pos + 8;
        if corps + taille > wav.len() {
            // Bloc plus grand que le fichier : ce qui reste.
            taille = wav.len() - corps;
        }
        match id {
            b"fmt " => {
                if taille < 16 {
                    return Err("audio illisible : bloc fmt tronqué".into());
                }
                format = u16le(wav, corps);
                canaux = u16le(wav, corps + 2) as usize;
                taux = u32le(wav, corps + 4);
                bits = u16le(wav, corps + 14);
            }
            b"data" => {
                if canaux == 0 || taux == 0 {
                    return Err("audio illisible : bloc data avant fmt".into());
                }
                let ech = pcm_to_f32(&wav[corps..corps + taille], format, bits, canaux)?;
                return Ok((taux, ech));
            }
            _ => {}
        }
        // Blocs alignés sur un nombre pair d'octets.
        pos = corps + taille + taille % 2;
    }
    Err("audio illisible : aucun bloc data".into())
}

/// `pcmVersFloat32` : canaux moyennés (calcul en f64 comme le Go).
pub fn pcm_to_f32(data: &[u8], format: u16, bits: u16, canaux: usize) -> Result<Vec<f32>, String> {
    match (format, bits) {
        (1, 16) => {
            let n = data.len() / 2 / canaux;
            Ok((0..n)
                .map(|i| {
                    let somme: f64 = (0..canaux)
                        .map(|c| {
                            i16::from_le_bytes([
                                data[2 * (i * canaux + c)],
                                data[2 * (i * canaux + c) + 1],
                            ]) as f64
                                / 32768.0
                        })
                        .sum();
                    (somme / canaux as f64) as f32
                })
                .collect())
        }
        (3, 32) => {
            let n = data.len() / 4 / canaux;
            Ok((0..n)
                .map(|i| {
                    let somme: f64 = (0..canaux)
                        .map(|c| f32::from_bits(u32le(data, 4 * (i * canaux + c))) as f64)
                        .sum();
                    (somme / canaux as f64) as f32
                })
                .collect())
        }
        _ => Err(format!(
            "format audio non géré (format {format}, {bits} bits) — attendu PCM 16 bits"
        )),
    }
}

// ---------------------------------------------------------------------------
// Protocole
// ---------------------------------------------------------------------------

/// `asrInferSur` : une trame binaire (en-tête + échantillons f32, découpée
/// en trames de 10 Kio), une trame texte en retour.
pub async fn infer_at(adresse: &str, wav: &[u8]) -> Result<String, String> {
    let (taux, ech) = wav_to_f32(wav)?;
    if ech.is_empty() {
        return Ok(String::new());
    }
    let cfg = tokio_tungstenite::tungstenite::protocol::WebSocketConfig::default()
        .max_message_size(None)
        .max_frame_size(None);
    let (mut ws, _) = tokio_tungstenite::connect_async_with_config(adresse, Some(cfg), false)
        .await
        .map_err(|e| format!("connexion au serveur de dictée : {e}"))?;
    let mut corps = Vec::with_capacity(ENTETE + 4 * ech.len());
    corps.extend_from_slice(&taux.to_le_bytes());
    corps.extend_from_slice(&((4 * ech.len()) as u32).to_le_bytes());
    for v in &ech {
        corps.extend_from_slice(&v.to_bits().to_le_bytes());
    }
    for trame in corps.chunks(TRAME_MAX) {
        ws.send(Message::Binary(Bytes::copy_from_slice(trame)))
            .await
            .map_err(|e| format!("envoi de l'audio : {e}"))?;
    }
    let brut = loop {
        match ws.next().await {
            Some(Ok(Message::Text(t))) => break t.as_bytes().to_vec(),
            Some(Ok(Message::Binary(b))) => break b.to_vec(),
            Some(Ok(Message::Close(_))) | None => {
                return Err("lecture du résultat : connexion fermée".into());
            }
            Some(Ok(_)) => continue,
            Some(Err(e)) => return Err(format!("lecture du résultat : {e}")),
        }
    };
    let _ = ws.close(None).await;
    #[derive(Deserialize)]
    struct R {
        #[serde(default)]
        text: String,
    }
    let r: R = serde_json::from_slice(&brut)
        .map_err(|e| format!("réponse illisible du serveur de dictée : {e}"))?;
    if marqueur(&r.text) {
        return Ok(String::new());
    }
    Ok(r.text.trim().to_string())
}

// ---------------------------------------------------------------------------
// Archives
// ---------------------------------------------------------------------------

/// `asrSafeRel` : retire le dossier de premier niveau, refuse ce qui sort
/// du dossier ; "" pour le dossier racine.
pub fn safe_rel(name: &str) -> Result<String, String> {
    let n = name.trim().replace('\\', "/");
    if n.is_empty() || n.starts_with('/') {
        return Err(format!(
            "entrée d'archive au chemin absolu : {}",
            go_quote(name)
        ));
    }
    let parts: Vec<&str> = n.trim_matches('/').split('/').collect();
    if parts.contains(&"..") {
        return Err(format!(
            "entrée d'archive qui remonte hors du dossier : {}",
            go_quote(name)
        ));
    }
    if parts.len() <= 1 {
        return Ok(String::new());
    }
    Ok(crate::paths::go_join(&parts[1..]))
}

/// `%q` du Go pour une chaîne simple.
fn go_quote(s: &str) -> String {
    serde_json::to_string(s).unwrap_or_default()
}

/// `asrExtract` : `.tar.bz2` décompressé dans `dest.part`, renommé à la fin ;
/// seuls dossiers et fichiers ordinaires (pas de liens).
pub fn extract<R: Read>(r: R, dest: &Path) -> Result<(), String> {
    let tmp = PathBuf::from(format!("{}.part", dest.display()));
    remove_all(&tmp)?;
    std::fs::create_dir_all(&tmp).map_err(|e| e.to_string())?;
    let res = (|| -> Result<(), String> {
        let mut ar = tar::Archive::new(bzip2::read::MultiBzDecoder::new(r));
        for entry in ar.entries().map_err(|e| e.to_string())? {
            let mut e = entry.map_err(|e| e.to_string())?;
            let name = String::from_utf8_lossy(&e.path_bytes()).into_owned();
            let rel = safe_rel(&name)?;
            if rel.is_empty() {
                continue;
            }
            let cible = tmp.join(&rel);
            match e.header().entry_type() {
                tar::EntryType::Directory => {
                    std::fs::create_dir_all(&cible).map_err(|e| e.to_string())?;
                }
                tar::EntryType::Regular => {
                    if let Some(p) = cible.parent() {
                        std::fs::create_dir_all(p).map_err(|e| e.to_string())?;
                    }
                    let mut f = std::fs::File::create(&cible).map_err(|e| e.to_string())?;
                    std::io::copy(&mut e, &mut f).map_err(|e| e.to_string())?;
                }
                _ => continue,
            }
        }
        Ok(())
    })();
    if let Err(e) = res {
        let _ = std::fs::remove_dir_all(&tmp);
        return Err(e);
    }
    if let Err(e) = remove_all(dest) {
        let _ = std::fs::remove_dir_all(&tmp);
        return Err(e);
    }
    std::fs::rename(&tmp, dest).map_err(|e| e.to_string())
}

/// `os.RemoveAll` : absent n'est pas une erreur.
fn remove_all(p: &Path) -> Result<(), String> {
    match std::fs::remove_dir_all(p) {
        Ok(()) => Ok(()),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(e) => match std::fs::remove_file(p) {
            Ok(()) => Ok(()),
            Err(_) => Err(e.to_string()),
        },
    }
}

/// Lecteur bloquant alimenté par le téléchargement (canal tokio).
struct ChanReader {
    rx: tokio::sync::mpsc::Receiver<Result<Bytes, String>>,
    cur: Bytes,
}

impl Read for ChanReader {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        while self.cur.is_empty() {
            match self.rx.blocking_recv() {
                Some(Ok(b)) => self.cur = b,
                Some(Err(e)) => return Err(std::io::Error::other(e)),
                None => return Ok(0),
            }
        }
        let n = buf.len().min(self.cur.len());
        buf[..n].copy_from_slice(&self.cur.split_to(n));
        Ok(n)
    }
}

// ---------------------------------------------------------------------------
// Supervision
// ---------------------------------------------------------------------------

/// `ProcessState.String()` du Go, et l'explication d'une mort par signal.
pub fn exit_reason(st: ExitStatus) -> String {
    #[cfg(unix)]
    {
        use std::os::unix::process::ExitStatusExt;
        if let Some(sig) = st.signal() {
            let nom = match sig {
                1 => "hangup".to_string(),
                2 => "interrupt".into(),
                3 => "quit".into(),
                4 => "illegal instruction".into(),
                6 => "aborted".into(),
                7 => "bus error".into(),
                8 => "floating point exception".into(),
                9 => "killed".into(),
                11 => "segmentation fault".into(),
                13 => "broken pipe".into(),
                15 => "terminated".into(),
                n => format!("signal {n}"),
            };
            return format!(
                "signal: {nom} (binaire compilé pour un autre processeur : image à reconstruire)"
            );
        }
    }
    match st.code() {
        Some(c) => format!("exit status {c}"),
        None => "arrêt sans erreur".into(),
    }
}

/// `time.Duration.String()` pour des durées entières en millisecondes.
fn go_duration(d: Duration) -> String {
    let ms = d.as_millis();
    if ms < 1000 {
        return format!("{ms}ms");
    }
    let s = d.as_secs();
    let (h, m, s) = (s / 3600, (s / 60) % 60, s % 60);
    if h > 0 {
        format!("{h}h{m}m{s}s")
    } else if m > 0 {
        format!("{m}m{s}s")
    } else {
        format!("{s}s")
    }
}

type SharedChild = Arc<Mutex<tokio::process::Child>>;

#[derive(Default)]
struct Server {
    child: Option<SharedChild>,
    port: u16,
    cfg: DictateCfg,
    since: Option<Instant>,
}

/// État du téléchargement d'un modèle.
#[derive(Debug, Clone, Default, Serialize)]
pub struct DlState {
    pub id: String,
    pub running: bool,
    pub pct: i64,
    #[serde(rename = "error")]
    pub err: String,
}

/// La dictée de ce lancement.
pub struct Dictation {
    home: PathBuf,
    model_base: String,
    server_bin: Option<String>,
    idle: Duration,
    ready: Duration,
    srv: tokio::sync::Mutex<Server>,
    /// Le processus en cours de démarrage, visible sans le verrou.
    starting: Mutex<Option<SharedChild>>,
    idle_gen: AtomicU64,
    dl: Mutex<DlState>,
    client: reqwest::Client,
}

fn lock<T>(m: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    m.lock().unwrap_or_else(|e| e.into_inner())
}

impl Dictation {
    pub fn new(home: &Path) -> Self {
        Self::with(home, ASR_MODEL_BASE, None, ASR_IDLE, ASR_READY)
    }

    /// Pour les tests : source des modèles, binaire du serveur, délais.
    pub fn with(
        home: &Path,
        model_base: &str,
        server_bin: Option<String>,
        idle: Duration,
        ready: Duration,
    ) -> Self {
        crate::chat::engine::install_crypto();
        Dictation {
            home: home.to_path_buf(),
            model_base: model_base.into(),
            server_bin,
            idle,
            ready,
            srv: tokio::sync::Mutex::new(Server::default()),
            starting: Mutex::new(None),
            idle_gen: AtomicU64::new(0),
            dl: Mutex::new(DlState::default()),
            client: reqwest::Client::builder()
                .connect_timeout(Duration::from_secs(20))
                .build()
                .unwrap_or_default(),
        }
    }

    pub fn home(&self) -> &Path {
        &self.home
    }

    /// `asrServerBin`.
    fn server_bin(&self) -> String {
        if let Some(b) = &self.server_bin {
            return b.clone();
        }
        for p in [
            std::env::var("LOKI_ASR_SERVER").unwrap_or_default(),
            "/usr/local/bin/sherpa-onnx-offline-websocket-server".into(),
        ] {
            if !p.is_empty() && std::fs::metadata(&p).is_ok() {
                return p;
            }
        }
        crate::paths::look_path("sherpa-onnx-offline-websocket-server").unwrap_or_default()
    }

    /// `asrEnsure` : un serveur vivant pour ces réglages ; son port.
    pub async fn ensure(self: &Arc<Self>, cfg: DictateCfg) -> Result<u16, String> {
        let mut s = self.srv.lock().await;
        if let Some(c) = &s.child
            && s.cfg == cfg
            && matches!(lock(c).try_wait(), Ok(None))
        {
            self.touch();
            return Ok(s.port);
        }
        self.stop_locked(&mut s);
        self.start_locked(&mut s, cfg).await
    }

    async fn start_locked(
        self: &Arc<Self>,
        s: &mut Server,
        cfg: DictateCfg,
    ) -> Result<u16, String> {
        if !model_present(&self.home, &cfg.model) {
            return Err(format!(
                "modèle de dictée absent : {} (à télécharger dans Paramètres → Dictée)",
                cfg.model
            ));
        }
        let bin = self.server_bin();
        if bin.is_empty() {
            return Err("serveur de dictée introuvable (image à reconstruire)".into());
        }
        let port = std::net::TcpListener::bind("127.0.0.1:0")
            .and_then(|l| l.local_addr())
            .map(|a| a.port())
            .map_err(|e| format!("aucun port libre pour le serveur de dictée : {e}"))?;
        let args = server_args(&self.home, &cfg, port)?;
        let mut cmd = tokio::process::Command::new(&bin);
        cmd.args(&args)
            // Tous les GPU masqués (le binaire livré est CPU).
            .env("CUDA_VISIBLE_DEVICES", "")
            .env_remove(crate::auth::INTERNAL_ENV)
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .kill_on_drop(true);
        let mut child = cmd
            .spawn()
            .map_err(|e| format!("démarrage du serveur de dictée : {e}"))?;
        let log = Arc::new(Mutex::new(Vec::<u8>::new()));
        for out in [
            child
                .stdout
                .take()
                .map(|o| Box::new(o) as Box<dyn tokio::io::AsyncRead + Unpin + Send>),
            child
                .stderr
                .take()
                .map(|o| Box::new(o) as Box<dyn tokio::io::AsyncRead + Unpin + Send>),
        ]
        .into_iter()
        .flatten()
        {
            let log = log.clone();
            tokio::spawn(async move {
                let mut out = out;
                let mut buf = [0u8; 4096];
                while let Ok(n) = out.read(&mut buf).await {
                    if n == 0 {
                        break;
                    }
                    let mut l = lock(&log);
                    l.extend_from_slice(&buf[..n]);
                    // La fin seule renseigne.
                    let excess = l.len().saturating_sub(64 << 10);
                    l.drain(..excess);
                }
            });
        }
        let child: SharedChild = Arc::new(Mutex::new(child));
        s.child = Some(child.clone());
        s.port = port;
        s.cfg = cfg;
        s.since = Some(Instant::now());
        *lock(&self.starting) = Some(child.clone());
        let res = self.wait_ready(&child, port, &log).await;
        *lock(&self.starting) = None;
        if let Err(e) = res {
            self.stop_locked(s);
            return Err(e);
        }
        self.touch();
        Ok(port)
    }

    /// `asrAttendPret` : le serveur ouvre sa socket une fois le modèle chargé.
    async fn wait_ready(
        &self,
        child: &SharedChild,
        port: u16,
        log: &Arc<Mutex<Vec<u8>>>,
    ) -> Result<(), String> {
        let fin = Instant::now() + self.ready;
        while Instant::now() < fin {
            tokio::time::sleep(Duration::from_millis(200)).await;
            let mort = lock(child).try_wait().ok().flatten();
            if let Some(st) = mort {
                // Laisse les lecteurs du journal finir.
                tokio::time::sleep(Duration::from_millis(50)).await;
                let mut msg = exit_reason(st);
                let l = last_line(&String::from_utf8_lossy(&lock(log)));
                if !l.is_empty() {
                    msg += &format!(" — dernière sortie : {l}");
                }
                return Err(format!(
                    "le serveur de dictée s'est arrêté au démarrage : {msg}"
                ));
            }
            let conn = tokio::time::timeout(
                Duration::from_secs(2),
                tokio::net::TcpStream::connect(("127.0.0.1", port)),
            )
            .await;
            if matches!(conn, Ok(Ok(_))) {
                return Ok(());
            }
        }
        Err(format!(
            "le serveur de dictée n'a pas répondu en {}",
            go_duration(self.ready)
        ))
    }

    /// `asrTouchLocked` : l'arrêt pour inactivité repart de zéro.
    fn touch(self: &Arc<Self>) {
        let tour = self.idle_gen.fetch_add(1, Ordering::SeqCst) + 1;
        let me = Arc::downgrade(self);
        let idle = self.idle;
        tokio::spawn(async move {
            tokio::time::sleep(idle).await;
            if let Some(me) = me.upgrade()
                && me.idle_gen.load(Ordering::SeqCst) == tour
            {
                me.shutdown().await;
            }
        });
    }

    fn stop_locked(&self, s: &mut Server) {
        self.idle_gen.fetch_add(1, Ordering::SeqCst);
        if let Some(c) = s.child.take() {
            let _ = lock(&c).start_kill();
        }
        *s = Server::default();
    }

    /// `asrShutdown` : inactivité, réglages changés.
    pub async fn shutdown(&self) {
        let mut s = self.srv.lock().await;
        self.stop_locked(&mut s);
    }

    /// `asrShutdownVite` : jamais derrière un démarrage (qui tient le verrou
    /// jusqu'à deux minutes) — le processus qui démarre est tué.
    pub async fn shutdown_fast(&self) {
        for _ in 0..20 {
            if let Ok(mut s) = self.srv.try_lock() {
                self.stop_locked(&mut s);
                return;
            }
            if let Some(c) = lock(&self.starting).as_ref() {
                let _ = lock(c).start_kill();
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    }

    /// `asrInfer`.
    pub async fn infer(self: &Arc<Self>, cfg: DictateCfg, wav: &[u8]) -> Result<String, String> {
        let port = self.ensure(cfg).await?;
        infer_at(&format!("ws://127.0.0.1:{port}"), wav).await
    }

    /// `asrEtat`.
    pub async fn etat(&self, cfg: &DictateCfg) -> Value {
        let s = self.srv.lock().await;
        let mut e = json!({
            "actif": s.child.is_some(),
            "modele": cfg.model,
            "present": model_present(&self.home, &cfg.model),
            "device": "cpu",
        });
        if s.child.is_some()
            && let Some(t) = s.since
        {
            e["depuis_s"] = json!(t.elapsed().as_secs());
        }
        e
    }

    pub fn dl_state(&self) -> DlState {
        lock(&self.dl).clone()
    }

    fn clear_dl_err(&self) {
        lock(&self.dl).err.clear();
    }

    /// `asrStartDownload` : un seul à la fois.
    pub fn start_download(self: &Arc<Self>, id: &str) {
        let mut d = lock(&self.dl);
        if d.running {
            return;
        }
        if model_url_for(&self.model_base, id).is_empty() {
            d.err = format!("modèle de dictée inconnu : {id}");
            return;
        }
        *d = DlState {
            id: id.into(),
            running: true,
            pct: 0,
            err: String::new(),
        };
        drop(d);
        let me = self.clone();
        let id = id.to_string();
        tokio::spawn(async move {
            let res = me.download(&id).await;
            let mut d = lock(&me.dl);
            d.running = false;
            if let Err(e) = res {
                d.err = e;
            }
        });
    }

    /// `asrDownload` : décompressé au fil du téléchargement ; progression
    /// sur les octets reçus.
    async fn download(self: &Arc<Self>, id: &str) -> Result<(), String> {
        let dst = model_dir_for(&self.home, id)
            .ok_or_else(|| format!("modèle de dictée inconnu : {id}"))?;
        if let Some(p) = dst.parent() {
            std::fs::create_dir_all(p).map_err(|e| e.to_string())?;
        }
        let mut resp = self
            .client
            .get(model_url_for(&self.model_base, id))
            .send()
            .await
            .map_err(|e| e.to_string())?;
        if resp.status().as_u16() != 200 {
            return Err(format!(
                "téléchargement du modèle de dictée : HTTP {}",
                resp.status().as_u16()
            ));
        }
        let total = resp.content_length().unwrap_or(0) as i64;
        let (tx, rx) = tokio::sync::mpsc::channel::<Result<Bytes, String>>(32);
        let extractor = tokio::task::spawn_blocking(move || {
            extract(
                ChanReader {
                    rx,
                    cur: Bytes::new(),
                },
                &dst,
            )
        });
        let mut lus = 0i64;
        loop {
            match resp.chunk().await {
                Ok(Some(c)) => {
                    lus += c.len() as i64;
                    if total > 0 {
                        lock(&self.dl).pct = lus * 100 / total;
                    }
                    if tx.send(Ok(c)).await.is_err() {
                        break; // l'extraction a échoué : elle dira pourquoi
                    }
                }
                Ok(None) => break,
                Err(e) => {
                    let _ = tx.send(Err(e.to_string())).await;
                    break;
                }
            }
        }
        drop(tx);
        extractor.await.map_err(|e| e.to_string())?
    }
}

// ---------------------------------------------------------------------------
// Routes
// ---------------------------------------------------------------------------

/// `dictateCfgLoad`.
pub async fn cfg_load(ctx: &Ctx) -> DictateCfg {
    let raw = ctx
        .store
        .get(BK_STATE, DICTATE_CFG_KEY)
        .await
        .ok()
        .flatten();
    raw.and_then(|b| serde_json::from_slice::<DictateCfg>(&b).ok())
        .unwrap_or_default()
        .with_defauts()
}

async fn read_body(body: Incoming, max: usize) -> Result<Bytes, String> {
    Limited::new(body, max)
        .collect()
        .await
        .map(|c| c.to_bytes())
        .map_err(|e| e.to_string())
}

fn err(code: StatusCode, msg: impl Into<String>) -> Response<Body> {
    send_json(code, &json!({ "error": msg.into() }))
}

/// `from_go` : remise par le Go (tunnel), déjà authentifiée par lui.
pub async fn serve(
    ctx: &Ctx,
    d: &Arc<Dictation>,
    req: Request<Incoming>,
    from_go: bool,
) -> Response<Body> {
    let (parts, body) = req.into_parts();
    if !from_go && let Err(refus) = ctx.guard(&parts.headers).await {
        return *refus;
    }
    let post = parts.method == Method::POST;
    match parts.uri.path() {
        "/api/transcribe" => {
            let audio = match read_body(body, 32 << 20).await {
                Ok(a) if a.len() >= 1000 => a,
                _ => return err(StatusCode::BAD_REQUEST, "audio vide ou trop court"),
            };
            let cfg = cfg_load(ctx).await;
            if !model_present(&d.home, &cfg.model) {
                let st = d.dl_state();
                if !st.err.is_empty() {
                    // Échec précédent : on le dit ET on relance.
                    d.clear_dl_err();
                    d.start_download(&cfg.model);
                    return send_json(
                        StatusCode::SERVICE_UNAVAILABLE,
                        &json!({ "downloading": true, "pct": 0, "error": st.err }),
                    );
                }
                d.start_download(&cfg.model);
                return send_json(
                    StatusCode::SERVICE_UNAVAILABLE,
                    &json!({ "downloading": true, "pct": st.pct }),
                );
            }
            match tokio::time::timeout(Duration::from_secs(300), d.infer(cfg, &audio)).await {
                Ok(Ok(text)) => send_json(StatusCode::OK, &json!({ "text": text })),
                Ok(Err(e)) => err(StatusCode::INTERNAL_SERVER_ERROR, e),
                Err(_) => err(
                    StatusCode::INTERNAL_SERVER_ERROR,
                    "context deadline exceeded",
                ),
            }
        }
        "/api/dictate/config" if post => {
            let c: DictateCfg = match read_body(body, 1 << 20)
                .await
                .ok()
                .and_then(|b| serde_json::from_slice(&b).ok())
            {
                Some(c) => c,
                None => return err(StatusCode::BAD_REQUEST, "requête illisible"),
            };
            if model(&c.model).is_none() {
                return err(
                    StatusCode::BAD_REQUEST,
                    format!("modèle de dictée inconnu : {}", c.model),
                );
            }
            let raw = serde_json::to_vec(&c.with_defauts()).unwrap_or_default();
            if let Err(e) = ctx.store.put(BK_STATE, DICTATE_CFG_KEY, raw).await {
                return err(StatusCode::BAD_REQUEST, e);
            }
            // Sans ça, l'ancien modèle continuerait de servir.
            d.shutdown().await;
            send_json(StatusCode::OK, &json!({ "ok": true }))
        }
        "/api/dictate/config" => send_json(StatusCode::OK, &cfg_load(ctx).await),
        "/api/dictate/models" => {
            let home = d.home.clone();
            let v = tokio::task::spawn_blocking(move || catalogue_trie(&home))
                .await
                .unwrap_or(Value::Null);
            send_json(StatusCode::OK, &v)
        }
        "/api/dictate/models/download" => {
            if post {
                #[derive(Deserialize)]
                struct Req {
                    #[serde(default)]
                    id: String,
                }
                let r: Req = match read_body(body, 1 << 20)
                    .await
                    .ok()
                    .and_then(|b| serde_json::from_slice(&b).ok())
                {
                    Some(r) => r,
                    None => return err(StatusCode::BAD_REQUEST, "requête illisible"),
                };
                let id = r.id.trim().to_string();
                if model_url_for(&d.model_base, &id).is_empty() {
                    return err(
                        StatusCode::BAD_REQUEST,
                        format!("modèle de dictée inconnu : {id}"),
                    );
                }
                d.start_download(&id);
            }
            send_json(StatusCode::OK, &d.dl_state())
        }
        "/api/dictate/state" => {
            let cfg = cfg_load(ctx).await;
            let mut e = d.etat(&cfg).await;
            e["dl"] = json!(d.dl_state());
            send_json(StatusCode::OK, &e)
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
    fn durees_comme_le_go() {
        assert_eq!(go_duration(Duration::from_secs(120)), "2m0s");
        assert_eq!(go_duration(Duration::from_secs(1)), "1s");
        assert_eq!(go_duration(Duration::from_millis(500)), "500ms");
        assert_eq!(go_duration(Duration::from_secs(3725)), "1h2m5s");
    }

    #[test]
    fn citations_comme_le_go() {
        assert_eq!(go_quote("a\"b"), r#""a\"b""#);
    }
}
