//! État du moteur pour `/api/status` et `/api/service/log` (lot 4) — port des
//! aides Go :
//!
//! - `modelLoadErrorFrom` / `isBacktraceFrame` (`web_api.go`) : la cause d'un
//!   chargement raté, lue dans le journal du moteur ;
//! - `serviceLogTail` en conteneur (`tailFile(logFilePath(), n)`) ;
//! - `engineLoadPct` (`sys_loadpct_linux.go`, -1 ailleurs) : le pourcentage
//!   de chargement estimé d'après `/proc/<pid>/io` et `VmRSS` ;
//! - `LLMPort` (`backend_config.go`) et `isExternalConfig`
//!   (`backend_external.go`) ;
//! - `healthCheck` (`llm_client.go`) : sonde `/health` du llama-server local.
//!
//! La configuration (`ReadConfig()` du Go) est passée par l'appelant
//! (`Store::config`) : ce module ne parle pas à la base.

use std::collections::HashMap;
use std::path::Path;
use std::time::Duration;

use crate::engine::Paths;

/// Fin du message de refus de LOAD_GUARD (`loadGuardMarker`,
/// `backend_serve_moe.go`), reconnue dans le journal du service.
pub const LOAD_GUARD_MARKER: &str = "LOAD_GUARD=off pour lancer quand même";

/// Première ligne d'un lancement de Strata (`strataServeMarker`,
/// `backend_strata.go`).
pub const STRATA_SERVE_MARKER: &str = "[loki serve] Strata";

/// Clé de configuration d'un preset externe (`extKeyFlag`) : "1" = externe.
pub const EXT_KEY_FLAG: &str = "EXTERNAL";

/// Port du llama-server quand la configuration n'en donne pas (ou un invalide).
pub const DEFAULT_PORT: u16 = 8080;

// ---------------------------------------------------------------------------
// Cause d'un chargement raté
// ---------------------------------------------------------------------------

const REASON_QUANT: &str = "format de quantification non reconnu par ce moteur";
const REASON_ARCH: &str = "type/architecture de modèle inconnu de ce moteur";

/// `modelLoadErrorFrom` : la cause du DERNIER chargement raté visible dans le
/// journal, en clair pour l'UI ; "" quand ce chargement a réussi ou qu'aucune
/// tentative n'est visible.
pub fn model_load_error_from(log: &str) -> String {
    if log.is_empty() {
        return String::new();
    }
    let lines: Vec<&str> = log.split('\n').collect();
    // On ne considère que ce qui suit la DERNIÈRE tentative de chargement.
    let mut start = 0;
    for (i, l) in lines.iter().enumerate() {
        // Une pile d'appels cite les fonctions traversées
        // (…server_context_impl10load_model…) : ce n'est pas une nouvelle
        // tentative, et repartir de là perdait la cause écrite juste avant.
        if is_backtrace_frame(l) {
            continue;
        }
        // Un lancement refusé par LOAD_GUARD s'arrête avant tout chargement :
        // c'est lui, la dernière tentative.
        if l.contains("loading model")
            || l.contains("load_model")
            || l.contains(LOAD_GUARD_MARKER)
            || l.contains(STRATA_SERVE_MARKER)
        {
            start = i;
        }
    }
    let mut loaded = false;
    let mut reason: &str = "";
    for l in &lines[start..] {
        let l: &str = l;
        let low = l.to_lowercase();
        let low = low.as_str();
        // Même ordre que le `switch` du Go : le premier cas vrai l'emporte.
        if low.contains("model loaded")
            || low.contains("server is listening")
            || l.starts_with("ready: http://")
        {
            // serveur de Strata prêt
            loaded = true;
        } else if l.contains("Traceback (most recent call last)") {
            // Serveur de Strata (Python) : une trace = il s'est arrêté.
            if reason.is_empty() {
                reason = "le serveur de Strata s'est arrêté (trace Python dans le journal)";
            }
        } else if l.contains("[strata] config") {
            reason =
                "configuration de Strata refusée (détail dans le journal) — réinstalle le modèle";
        } else if low.contains("failed to create mtp context")
            || low.contains("failed to load draft model")
            || low.contains("failed to initialize speculative decoding context")
        {
            // Avant l'échec générique : c'est le brouillon qui manque, pas le modèle.
            reason = "le décodage spéculatif n'a pas pu démarrer (tête MTP ou modèle brouillon) — SPEC=off dans le preset";
        } else if low.contains("split_mode_tensor requires flash_attn")
            || low.contains("split_mode_tensor not implemented")
        {
            reason =
                "le mode tensor (-sm tensor) n'a pas pu démarrer — SPLIT_MODE=off dans le preset";
        } else if l.contains(LOAD_GUARD_MARKER) {
            reason = "lancement refusé : trop de poids resteraient en RAM avec ce mode de chargement — --load-mode mmap dans le preset, ou LOAD_GUARD=off pour passer outre";
        } else if l.contains("has offset") && l.contains("expected") {
            reason = REASON_QUANT;
        } else if low.contains("unknown model architecture")
            || low.contains("unknown ftype")
            || low.contains("unknown type")
            || low.contains("unsupported")
        {
            if reason.is_empty() {
                reason = REASON_ARCH;
            }
        } else if low.contains("error loading model")
            || low.contains("failed to load model")
            || low.contains("failed to read tensor data")
        {
            if reason.is_empty() {
                reason = "échec du chargement du modèle";
            }
        } else if low.contains("out of memory")
            || low.contains("cudamalloc failed")
            || low.contains("failed to allocate")
        {
            // Prioritaire sur l'échec générique : la cause mémoire doit gagner.
            reason = "mémoire GPU insuffisante (VRAM) — réduis le contexte (CTX) ou prends une quantification plus petite, et vérifie qu'aucun autre programme n'occupe la carte (nvidia-smi)";
        } else if low.contains("ggml_cuda error") || low.contains("cuda error") {
            if reason.is_empty() {
                reason = "erreur CUDA — GPU indisponible ? (après un redémarrage du serveur : docker restart loki)";
            }
        } else if low.contains("__libc_start_main")
            || l.contains("SIGSEGV")
            || l.contains("SIGABRT")
            || low.contains("segmentation fault")
            || low.contains("aborted (core dumped)")
        {
            // Backtrace = le moteur a PLANTÉ, pas « il charge encore ». PAS de
            // motif « libggml » : les logs normaux citent les .so.
            if reason.is_empty() {
                reason = "le moteur a planté au démarrage (backtrace dans le journal)";
            }
        }
    }
    if loaded || reason.is_empty() {
        return String::new();
    }
    // N'affirmer l'incompatibilité que quand le journal la montre vraiment
    // (quant/architecture) ; sinon renvoyer vers le journal.
    if reason == REASON_QUANT || reason == REASON_ARCH {
        format!(
            "Modèle incompatible avec le moteur : {reason}. Choisis un autre backend (édite le modèle → Moteur)."
        )
    } else {
        format!(
            "Le modèle n'a pas pu être chargé ({reason}). Ouvre le journal du moteur pour le détail."
        )
    }
}

/// `isBacktraceFrame` : une ligne de pile d'appels de glibc
/// (« /chemin/lib.so(symbole+0x1c)[0x14bd…] »).
pub fn is_backtrace_frame(l: &str) -> bool {
    l.contains(")[0x")
}

// ---------------------------------------------------------------------------
// Journal du service
// ---------------------------------------------------------------------------

/// `serviceLogTail(n)` en conteneur : les `n` dernières lignes du journal du
/// moteur (`$LOKI_HOME/<service>.log`), "" si illisible. Octets invalides en
/// UTF-8 remplacés par U+FFFD (le Go les gardait bruts ; son encodage JSON
/// fait le même remplacement à la sortie).
pub fn service_log_tail(paths: &Paths, n: usize) -> String {
    match std::fs::read(paths.log()) {
        Ok(b) => crate::engine::tail_lines(&String::from_utf8_lossy(&b), n),
        Err(_) => String::new(),
    }
}

// ---------------------------------------------------------------------------
// Configuration : port et preset externe
// ---------------------------------------------------------------------------

/// Valeur brute de PORT comme `LLMPort` du Go (`strconv.Atoi`, > 0), sans
/// borne haute : un port > 65535 doit faire échouer la sonde, pas viser 8080.
fn llm_port_raw(cfg: &HashMap<String, String>) -> i64 {
    if let Some(p) = cfg.get("PORT")
        && let Ok(n) = p.parse::<i64>()
        && n > 0
    {
        return n;
    }
    i64::from(DEFAULT_PORT)
}

/// `LLMPort` : le port du serveur (clé PORT), 8080 par défaut. Un PORT hors de
/// 1..=65535 (que le Go aurait rendu tel quel) donne aussi 8080.
pub fn llm_port(cfg: &HashMap<String, String>) -> u16 {
    u16::try_from(llm_port_raw(cfg)).unwrap_or(DEFAULT_PORT)
}

/// `isExternalConfig` : la configuration décrit-elle un endpoint externe ?
pub fn is_external(cfg: &HashMap<String, String>) -> bool {
    cfg.get(EXT_KEY_FLAG).is_some_and(|v| v.trim() == "1")
}

// ---------------------------------------------------------------------------
// Sonde /health
// ---------------------------------------------------------------------------

/// Délai de la sonde (`healthClient`, 3 s) : un moteur qui accepte la
/// connexion sans jamais répondre ne doit pas bloquer l'appelant.
const HEALTH_TIMEOUT: Duration = Duration::from_secs(3);

/// `healthCheck` : preset externe → prêt sans sonder ; sinon GET
/// `http://localhost:<port>/health`, prêt seulement sur un 200.
///
/// « localhost » comme le Go : le connecteur de hyper résout par le système
/// et essaie chaque adresse (::1 puis 127.0.0.1, « happy eyeballs »), comme
/// le dialer de Go.
pub async fn health_check(cfg: &HashMap<String, String>) -> bool {
    if is_external(cfg) {
        return true;
    }
    let port = llm_port_raw(cfg);
    if port > i64::from(u16::MAX) {
        // Go : « invalid port » au dial → false. hyper, lui, prendrait le port 80.
        return false;
    }
    health_probe(&format!("http://localhost:{port}/health")).await
}

async fn health_probe(url: &str) -> bool {
    use bytes::Bytes;
    use http_body_util::Empty;
    use hyper_util::client::legacy::Client;
    use hyper_util::rt::TokioExecutor;

    let Ok(uri) = url.parse::<hyper::Uri>() else {
        return false;
    };
    let client = Client::builder(TokioExecutor::new()).build_http::<Empty<Bytes>>();
    match tokio::time::timeout(HEALTH_TIMEOUT, client.get(uri)).await {
        Ok(Ok(resp)) => resp.status() == hyper::StatusCode::OK,
        _ => false,
    }
}

// ---------------------------------------------------------------------------
// Pourcentage de chargement
// ---------------------------------------------------------------------------

/// `engineLoadPct` : pourcentage estimé de chargement du modèle, ou -1 quand
/// il n'est pas mesurable (pas de PID, /proc illisible, taille inconnue ; et
/// toujours hors Linux). Dossiers de modèles : `models/` et `$LOKI_MODEL_DIRS`
/// — ceux déclarés dans l'UI (base) passent par [`engine_load_pct_dirs`].
pub fn engine_load_pct(paths: &Paths, cfg: &HashMap<String, String>) -> i32 {
    engine_load_pct_dirs(paths, cfg, &[])
}

/// [`engine_load_pct`] avec les dossiers de modèles enregistrés en base
/// (`getJSON(bkState, "model_dirs")` du Go), à passer par l'appelant.
pub fn engine_load_pct_dirs(
    paths: &Paths,
    cfg: &HashMap<String, String>,
    stored_dirs: &[String],
) -> i32 {
    #[cfg(target_os = "linux")]
    {
        linux::engine_load_pct(paths, cfg, stored_dirs)
    }
    #[cfg(not(target_os = "linux"))]
    {
        // Hors Linux, pas de /proc/<pid>/io : pas mesurable.
        let _ = (paths, cfg, stored_dirs);
        -1
    }
}

#[cfg(target_os = "linux")]
mod linux {
    use super::*;

    pub fn engine_load_pct(
        paths: &Paths,
        cfg: &HashMap<String, String>,
        stored_dirs: &[String],
    ) -> i32 {
        let pid = crate::engine::read_pid(&paths.pid());
        if pid <= 0 {
            return -1;
        }
        let model = cfg.get("MODEL").map(String::as_str).unwrap_or("");
        if model.is_empty() {
            return -1;
        }
        let env_dirs = std::env::var(crate::gguf::MODEL_DIRS_ENV).ok();
        let Ok(path) = crate::gguf::resolve_serve_model_path(
            model,
            &paths.home,
            stored_dirs,
            env_dirs.as_deref(),
        ) else {
            return -1;
        };
        let size = model_total_size(&path);
        if size <= 0 {
            return -1;
        }
        let Ok(io) = std::fs::read_to_string(format!("/proc/{pid}/io")) else {
            return -1;
        };
        let read = parse_read_bytes(&io);
        if read < 0 {
            return -1;
        }
        let rss = std::fs::read_to_string(format!("/proc/{pid}/status"))
            .map(|s| parse_vm_rss(&s))
            .unwrap_or(0);
        load_pct(read, rss, size)
    }
}

/// Le calcul de `engineLoadPct` : octets lus (`read_bytes`, -1 = inconnu),
/// mémoire résidente et taille du modèle → pourcentage plafonné à 99.
///
/// Modèle déjà dans le cache disque : read_bytes reste à zéro, mais VmRSS
/// grandit quoi qu'il arrive — on prend le plus avancé des deux. 100 %
/// n'existe pas ici : c'est /health qui dit « prêt ».
pub fn load_pct(read: i64, rss: i64, size: i64) -> i32 {
    if read < 0 || size <= 0 {
        return -1;
    }
    let read = read.max(rss);
    // Arithmétique 64 bits qui déborde comme celle du Go (inatteignable).
    let pct = read.wrapping_mul(100) / size;
    pct.clamp(i64::from(i32::MIN), 99) as i32
}

/// Ligne `read_bytes:` de `/proc/<pid>/io` ; -1 si absente. Valeur illisible
/// → 0, débordement → borne (comme `read, _ = strconv.ParseInt(…)`).
pub fn parse_read_bytes(io: &str) -> i64 {
    for line in io.split('\n') {
        if let Some(v) = line.strip_prefix("read_bytes:") {
            return go_parse_int64(v.trim());
        }
    }
    -1
}

/// `procVmRSS` sur le texte de `/proc/<pid>/status` : « VmRSS:  N kB » en
/// octets, 0 si absent ou illisible.
pub fn parse_vm_rss(status: &str) -> i64 {
    for line in status.split('\n') {
        if let Some(v) = line.strip_prefix("VmRSS:") {
            let v = v.trim();
            let v = v.strip_suffix("kB").unwrap_or(v).trim();
            if let Ok(n) = v.parse::<i64>() {
                return n.wrapping_mul(1024);
            }
        }
    }
    0
}

/// `strconv.ParseInt(s, 10, 64)` dont l'erreur est ignorée : 0 sur une
/// syntaxe invalide, la borne sur un débordement.
fn go_parse_int64(s: &str) -> i64 {
    use std::num::IntErrorKind;
    match s.parse::<i64>() {
        Ok(n) => n,
        Err(e) => match e.kind() {
            IntErrorKind::PosOverflow => i64::MAX,
            IntErrorKind::NegOverflow => i64::MIN,
            _ => 0,
        },
    }
}

// --- Résolution du modèle (port local de backend_model_dirs.go, le temps que
// `crate::paths` le porte en entier) -----------------------------------------

/// `shardFamilySize` : somme des tranches PRÉSENTES dans `dir`.
pub fn shard_family_size(dir: &Path, name: &str) -> i64 {
    crate::gguf::shard_family(name)
        .iter()
        .filter_map(|n| std::fs::metadata(dir.join(n)).ok())
        .filter(|m| !m.is_dir())
        .map(|m| m.len() as i64)
        .sum()
}

/// `modelTotalSize` : taille du modèle, tranches comprises ; 0 si inconnue.
pub fn model_total_size(path: &Path) -> i64 {
    let dir = path.parent().unwrap_or(Path::new(""));
    let base = path
        .file_name()
        .map(|s| s.to_string_lossy().into_owned())
        .unwrap_or_default();
    let total = shard_family_size(dir, &base);
    if total > 0 {
        return total;
    }
    std::fs::metadata(path).map(|m| m.len() as i64).unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn cfg(kv: &[(&str, &str)]) -> HashMap<String, String> {
        kv.iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect()
    }

    // --- modelLoadErrorFrom -------------------------------------------------

    /// TestModelLoadErrorLoadGuard (backend_serve_moe_test.go). Le refus est
    /// celui que rend `loadModeRisk` pour `--load-mode none`, modèle 82 Gio,
    /// VRAM moteur 16 Gio, RAM 60 Gio.
    #[test]
    fn model_load_error_load_guard() {
        let refuse = "--load-mode none : au moins 66,0 Gio de poids restent en RAM (modèle 82,0 Gio) pour 60,0 Gio de RAM, limite du conteneur comprise — échec certain : swap et décodage effondré, ou processus tué (llama.cpp #26110). --load-mode mmap laisse le système relire les pages au besoin ; LOAD_GUARD=off pour lancer quand même";
        let log = format!(
            "llama_model_load: loading model\nmain: model loaded\nserver is listening\n[ERREUR] {refuse}\n"
        );
        let got = model_load_error_from(&log);
        assert!(
            got.contains("LOAD_GUARD=off"),
            "refus non signalé : {got:?}"
        );
        // Une tentative suivante qui charge efface le refus.
        let got = model_load_error_from(&format!("{log}loading model\nmodel loaded\n"));
        assert_eq!(got, "", "refus ancien encore signalé");
    }

    /// TestModelLoadErrorStrata (backend_strata_test.go).
    #[test]
    fn model_load_error_strata() {
        let ok = format!(
            "{STRATA_SERVE_MARKER} (paquet AJEAN MoE 1.0)\nloading the model (the first start takes a minute or two) ...\nready: http://127.0.0.1:8080/v1  (OpenAI: /v1/chat/completions)\n"
        );
        assert_eq!(
            model_load_error_from(&ok),
            "",
            "Strata prêt signalé en échec"
        );
        let crash = format!(
            "{STRATA_SERVE_MARKER}\nTraceback (most recent call last):\n  File \"server.py\", line 1\nRuntimeError: boom\n"
        );
        let got = model_load_error_from(&crash);
        assert!(
            got.contains("Strata"),
            "trace Python non signalée : {got:?}"
        );
        // une trace d'un lancement PRÉCÉDENT ne compte pas
        let got = model_load_error_from(&format!(
            "{crash}{STRATA_SERVE_MARKER}\nloading the model ...\n"
        ));
        assert_eq!(got, "", "ancienne trace prise pour l'échec en cours");
    }

    /// TestModelLoadErrorOOMBeforeBacktrace (backend_strata_test.go).
    #[test]
    fn model_load_error_oom_before_backtrace() {
        let log = "0.00.247.285 I srv    load_model: loading model '/data/models/Swift.gguf'\n"
            .to_string()
            + "4.47.957.764 E ggml_backend_cuda_buffer_type_alloc_buffer: allocating 884.62 MiB on device 0: cudaMalloc failed: out of memory\n"
            + "/app/ggml/src/ggml-backend.cpp:328: GGML_ASSERT(buffer) failed\n"
            + "/data/engine/libmtmd.so.0(mtmd_init_from_file+0x31)[0x14bd4fea4c01]\n"
            + "/data/engine/libllama-server-impl.so(_ZN19server_context_impl10load_modelER13common_params+0x7ff)[0x14bd50b9f1ff]\n"
            + "/lib/x86_64-linux-gnu/libc.so.6(__libc_start_main+0x8b)[0x14bd5059a28b]\n";
        let got = model_load_error_from(&log);
        assert!(
            got.contains("mémoire GPU insuffisante"),
            "cause mémoire perdue derrière la pile d'appels : {got:?}"
        );
    }

    /// Libellés complets (pas de test Go dédié) : les deux formes du message.
    #[test]
    fn model_load_error_messages_exacts() {
        assert_eq!(model_load_error_from(""), "");
        assert_eq!(model_load_error_from("rien à voir\n"), "");
        assert_eq!(
            model_load_error_from("loading model\ntensor 'x' has offset 12, expected 16\n"),
            "Modèle incompatible avec le moteur : format de quantification non reconnu par ce moteur. Choisis un autre backend (édite le modèle → Moteur)."
        );
        assert_eq!(
            model_load_error_from("loading model\nUnknown Model Architecture: 'bitnet'\n"),
            "Modèle incompatible avec le moteur : type/architecture de modèle inconnu de ce moteur. Choisis un autre backend (édite le modèle → Moteur)."
        );
        assert_eq!(
            model_load_error_from("loading model\nerror loading model: boom\n"),
            "Le modèle n'a pas pu être chargé (échec du chargement du modèle). Ouvre le journal du moteur pour le détail."
        );
        // La cause mémoire l'emporte sur l'échec générique écrit avant.
        assert_eq!(
            model_load_error_from("loading model\nfailed to load model\nout of memory\n"),
            "Le modèle n'a pas pu être chargé (mémoire GPU insuffisante (VRAM) — réduis le contexte (CTX) ou prends une quantification plus petite, et vérifie qu'aucun autre programme n'occupe la carte (nvidia-smi)). Ouvre le journal du moteur pour le détail."
        );
        // Sensible à la casse : « sigsegv » en minuscules n'est pas un crash.
        assert_eq!(model_load_error_from("loading model\nsigsegv\n"), "");
        assert_eq!(
            model_load_error_from("loading model\nSIGSEGV\n"),
            "Le modèle n'a pas pu être chargé (le moteur a planté au démarrage (backtrace dans le journal)). Ouvre le journal du moteur pour le détail."
        );
        assert!(is_backtrace_frame("/lib/libc.so.6(abort+0x1)[0x7f]"));
        assert!(!is_backtrace_frame("srv load_model: loading model"));
    }

    // --- serviceLogTail -----------------------------------------------------

    #[test]
    fn service_log_tail_fichier() {
        let dir = tempfile::tempdir().unwrap();
        let paths = Paths {
            home: dir.path().to_path_buf(),
            service: "loki-engine".into(),
        };
        assert_eq!(service_log_tail(&paths, 10), "", "journal absent");
        std::fs::write(paths.log(), "a\nb\nc\n").unwrap();
        assert_eq!(service_log_tail(&paths, 2), "b\nc\n");
        assert_eq!(service_log_tail(&paths, 10), "a\nb\nc\n");
    }

    // --- LLMPort / isExternalConfig -----------------------------------------

    /// La partie LLMPort de TestStore… (web_internal_test.go : PORT=9000).
    #[test]
    fn port_du_moteur() {
        assert_eq!(llm_port(&cfg(&[("PORT", "9000")])), 9000);
        assert_eq!(llm_port(&cfg(&[])), 8080);
        for bad in ["", "abc", "0", "-1", " 9000", "70000"] {
            assert_eq!(llm_port(&cfg(&[("PORT", bad)])), 8080, "PORT={bad:?}");
        }
        assert_eq!(
            llm_port(&cfg(&[("PORT", "+8081")])),
            8081,
            "Atoi accepte le +"
        );
    }

    /// TestReconnaissanceDunPresetExterne (backend_external_test.go).
    #[test]
    fn reconnaissance_dun_preset_externe() {
        assert!(
            is_external(&cfg(&[(EXT_KEY_FLAG, "1")])),
            "EXTERNAL=1 non reconnu"
        );
        for v in ["", "0", "off", "false"] {
            assert!(
                !is_external(&cfg(&[(EXT_KEY_FLAG, v)])),
                "EXTERNAL={v:?} pris pour un preset externe"
            );
        }
        // parseEnv("MODEL=\"m.gguf\"\nCTX=4096\n")
        assert!(
            !is_external(&cfg(&[("MODEL", "m.gguf"), ("CTX", "4096")])),
            "un preset local est pris pour un externe"
        );
        assert!(is_external(&cfg(&[(EXT_KEY_FLAG, " 1 ")])));
    }

    // --- engineLoadPct ------------------------------------------------------

    #[test]
    fn calcul_du_pourcentage() {
        assert_eq!(load_pct(-1, 1 << 30, 100), -1, "read_bytes absent");
        assert_eq!(load_pct(50, 0, 200), 25);
        assert_eq!(load_pct(0, 150, 200), 75, "cache disque : VmRSS l'emporte");
        assert_eq!(load_pct(500, 0, 200), 99, "jamais 100 %");
        assert_eq!(load_pct(0, 0, 0), -1);

        let io = "rchar: 1\nwchar: 2\nread_bytes: 4096\nwrite_bytes: 0\n";
        assert_eq!(parse_read_bytes(io), 4096);
        assert_eq!(parse_read_bytes("rchar: 1\n"), -1);
        assert_eq!(parse_read_bytes("read_bytes: x\n"), 0);
        let st = "Name:\tllama-server\nVmPeak:\t 9 kB\nVmRSS:\t   2048 kB\n";
        assert_eq!(parse_vm_rss(st), 2048 * 1024);
        assert_eq!(parse_vm_rss("VmRSS: x kB\nVmRSS: 3 kB\n"), 3 * 1024);
        assert_eq!(parse_vm_rss("Name: x\n"), 0);
    }

    #[test]
    fn hors_mesure_sans_pid() {
        let dir = tempfile::tempdir().unwrap();
        let paths = Paths {
            home: dir.path().to_path_buf(),
            service: "loki-engine".into(),
        };
        assert_eq!(engine_load_pct(&paths, &cfg(&[("MODEL", "m.gguf")])), -1);
    }

    /// Le process de test lui-même tient lieu de moteur : il a lu plus d'un
    /// octet, un modèle d'un octet est donc « chargé » à 99 %.
    #[cfg(target_os = "linux")]
    #[test]
    fn pourcentage_sur_le_process_courant() {
        let dir = tempfile::tempdir().unwrap();
        let paths = Paths {
            home: dir.path().to_path_buf(),
            service: "loki-engine".into(),
        };
        std::fs::create_dir_all(dir.path().join("models")).unwrap();
        std::fs::write(dir.path().join("models/m.gguf"), b"x").unwrap();
        std::fs::write(paths.pid(), std::process::id().to_string()).unwrap();
        assert_eq!(engine_load_pct(&paths, &cfg(&[("MODEL", "m.gguf")])), 99);
        assert_eq!(engine_load_pct(&paths, &cfg(&[])), -1, "MODEL vide");
        assert_eq!(
            engine_load_pct(&paths, &cfg(&[("MODEL", "absent.gguf")])),
            -1
        );
    }

    /// TestShardFamilyTailleEtManquantes, partie taille
    /// (backend_models_shards_test.go) ; et modelTotalSize sur la famille.
    #[test]
    fn shard_family_taille() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("m-00001-of-00003.gguf"), vec![0u8; 100]).unwrap();
        std::fs::write(dir.path().join("m-00003-of-00003.gguf"), vec![0u8; 300]).unwrap();
        assert_eq!(shard_family_size(dir.path(), "m-00001-of-00003.gguf"), 400);
        assert_eq!(
            model_total_size(&dir.path().join("m-00001-of-00003.gguf")),
            400
        );
    }

    // --- healthCheck --------------------------------------------------------

    /// Serveur local qui répond `status` sur /health (404 ailleurs).
    async fn serveur(status: u16) -> u16 {
        use bytes::Bytes;
        use http_body_util::Full;
        use hyper::body::Incoming;
        use hyper::{Request, Response};
        use hyper_util::rt::TokioIo;

        let l = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = l.local_addr().unwrap().port();
        tokio::spawn(async move {
            while let Ok((s, _)) = l.accept().await {
                tokio::spawn(async move {
                    let svc =
                        hyper::service::service_fn(move |req: Request<Incoming>| async move {
                            let code = if req.uri().path() == "/health" {
                                status
                            } else {
                                404
                            };
                            Ok::<_, std::convert::Infallible>(
                                Response::builder()
                                    .status(code)
                                    .body(Full::new(Bytes::from_static(b"{\"status\":\"ok\"}")))
                                    .unwrap(),
                            )
                        });
                    let _ = hyper::server::conn::http1::Builder::new()
                        .serve_connection(TokioIo::new(s), svc)
                        .await;
                });
            }
        });
        port
    }

    #[tokio::test]
    async fn sonde_health() {
        let ok = serveur(200).await;
        assert!(
            health_check(&cfg(&[("PORT", &ok.to_string())])).await,
            "200 = prêt"
        );
        let loading = serveur(503).await;
        assert!(
            !health_check(&cfg(&[("PORT", &loading.to_string())])).await,
            "503 = chargement"
        );
        // Port fermé : on réserve un port puis on le libère.
        let closed = {
            let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
            l.local_addr().unwrap().port()
        };
        let c = cfg(&[("PORT", &closed.to_string())]);
        assert!(!health_check(&c).await, "port fermé");
        // Preset externe : prêt sans sonder, même moteur éteint.
        let mut ext = c.clone();
        ext.insert(EXT_KEY_FLAG.into(), "1".into());
        assert!(health_check(&ext).await);
        assert!(
            !health_check(&cfg(&[("PORT", "70000")])).await,
            "port invalide"
        );
    }

    /// Un moteur qui accepte la connexion sans jamais répondre : la sonde rend
    /// la main au bout de 3 s.
    #[tokio::test]
    async fn sonde_health_bornee() {
        let l = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = l.local_addr().unwrap().port();
        tokio::spawn(async move {
            let mut held = Vec::new();
            while let Ok((s, _)) = l.accept().await {
                held.push(s);
            }
        });
        let t0 = std::time::Instant::now();
        assert!(!health_check(&cfg(&[("PORT", &port.to_string())])).await);
        assert!(t0.elapsed() < Duration::from_secs(5));
    }
}
