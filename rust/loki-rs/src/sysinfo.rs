//! Mesures et diagnostics système servis à l'interface — port (lot 4) de :
//!
//! - `ramUsageMB` (`sys_platform_unix.go`, branche Linux : `/proc/meminfo`) ;
//! - la lecture des jauges GPU de `web_vram.go` (`gpuStat`, requête et analyse
//!   nvidia-smi, cache partagé `gpuStatsCached`) — PAS le déchargement de VRAM ;
//! - `dataVolumeWarning` (`sys_datavolume.go`) : `/data` n'est pas un volume
//!   monté, les données seront perdues à la recréation du conteneur ;
//! - `storageFuseHint` (`sys_storagefuse.go`) : `/data` ou les modèles passent
//!   par la couche FUSE des partages Unraid (fuse.shfs).
//!
//! Les fonctions pures (analyse de `/proc/meminfo`, de la sortie nvidia-smi, de
//! `/proc/self/mountinfo`) sont exposées pour les tests, portés un pour un des
//! tests Go.

use std::future::Future;
use std::io;
use std::sync::LazyLock;
use std::time::{Duration, Instant};

use serde::Serialize;
use tokio::sync::Mutex;

/// Racine des données : `$LOKI_HOME`, `/data` à défaut — la résolution du
/// reste de la façade (`engine::Paths`). Ces diagnostics ne servent qu'en
/// conteneur, où l'image et `docker-entrypoint.sh` fixent toujours LOKI_HOME.
fn loki_home() -> String {
    crate::engine::Paths::from_env()
        .home
        .to_string_lossy()
        .into_owned()
}

/// `os.Getenv("LOKI_CONTAINER") != ""`.
fn in_container() -> bool {
    std::env::var_os("LOKI_CONTAINER").is_some_and(|v| !v.is_empty())
}

// --- RAM ---------------------------------------------------------------------

/// `ramUsageMB` : (utilisée, totale) en Mo pour `/api/ram`. Linux :
/// `/proc/meminfo` ; ailleurs (0, 0), l'UI masque alors le bloc.
pub fn ram_usage_mb() -> (i64, i64) {
    #[cfg(target_os = "linux")]
    {
        match std::fs::read("/proc/meminfo") {
            Ok(b) => parse_meminfo(&String::from_utf8_lossy(&b)),
            Err(_) => (0, 0),
        }
    }
    #[cfg(not(target_os = "linux"))]
    {
        (0, 0)
    }
}

/// Analyse de `/proc/meminfo` : (MemTotal − MemAvailable, MemTotal) en Mo.
/// Comme le Go : une valeur illisible compte pour 0, la dernière ligne d'une
/// clé l'emporte, un total nul donne (0, 0).
pub fn parse_meminfo(text: &str) -> (i64, i64) {
    let (mut total_kb, mut avail_kb) = (0i64, 0i64);
    for line in text.split('\n') {
        let f: Vec<&str> = line.split_whitespace().collect();
        if f.len() < 2 {
            continue;
        }
        let v = f[1].parse::<i64>().unwrap_or(0); // kB
        match f[0] {
            "MemTotal:" => total_kb = v,
            "MemAvailable:" => avail_kb = v,
            _ => {}
        }
    }
    if total_kb == 0 {
        return (0, 0);
    }
    ((total_kb - avail_kb) / 1024, total_kb / 1024)
}

// --- GPU (nvidia-smi) --------------------------------------------------------

/// `gpuStat` : une carte vue par nvidia-smi. Mémoire en Mo (unité de
/// nvidia-smi, et celle que `/api/vram` expose à l'interface). Les noms JSON
/// sont ceux de `handleVram` : name, used, total, util, temp.
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct GpuStat {
    pub name: String,
    pub used: i64,
    pub total: i64,
    pub util: i64,
    pub temp: i64,
}

/// Borne de la requête nvidia-smi : un pilote coincé peut faire pendre
/// nvidia-smi, et le cache tient son verrou pendant la requête. Sans borne, un
/// seul nvidia-smi pendu figerait les jauges de TOUS les onglets.
pub const NVIDIA_SMI_TIMEOUT: Duration = Duration::from_secs(10);
/// Une lecture vieille de deux secondes suffit largement à une jauge.
pub const GPU_CACHE_TTL: Duration = Duration::from_secs(2);
/// Un nvidia-smi INTROUVABLE (Mac, CPU seul) est mémorisé une minute.
pub const GPU_CACHE_ABSENT: Duration = Duration::from_secs(60);

/// `nvidiaSmiQuery` : lance la requête nvidia-smi, bornée dans le temps. Un
/// binaire absent du PATH rend une erreur de genre [`io::ErrorKind::NotFound`]
/// (l'`exec.ErrNotFound` du Go), que le cache mémorise une minute.
pub async fn nvidia_smi_query() -> io::Result<Vec<u8>> {
    nvidia_smi_query_bin("nvidia-smi").await
}

/// [`nvidia_smi_query`] avec un exécutable choisi (tests).
async fn nvidia_smi_query_bin(bin: &str) -> io::Result<Vec<u8>> {
    let mut cmd = tokio::process::Command::new(bin);
    cmd.args([
        "--query-gpu=name,memory.used,memory.total,utilization.gpu,temperature.gpu",
        "--format=csv,noheader,nounits",
    ])
    .stdin(std::process::Stdio::null())
    .stderr(std::process::Stdio::null())
    // Délai dépassé : le futur abandonné tue nvidia-smi, et les tubes fermés
    // avec lui ne retiennent plus personne (le `WaitDelay` du Go).
    .kill_on_drop(true);
    #[cfg(windows)]
    cmd.creation_flags(0x0800_0000); // CREATE_NO_WINDOW, comme hideCmd
    let out = match tokio::time::timeout(NVIDIA_SMI_TIMEOUT, cmd.output()).await {
        Ok(r) => r?,
        Err(_) => {
            return Err(io::Error::new(
                io::ErrorKind::TimedOut,
                "nvidia-smi : délai dépassé",
            ));
        }
    };
    if !out.status.success() {
        return Err(io::Error::other(format!("nvidia-smi : {}", out.status)));
    }
    Ok(out.stdout)
}

/// Analyse de la sortie `--format=csv,noheader,nounits` : une carte par
/// ligne, exactement 5 champs (sinon la ligne est ignorée) ; un nombre illisible
/// (« [N/A] ») compte pour 0.
pub fn parse_nvidia_smi(out: &str) -> Vec<GpuStat> {
    let mut gpus = Vec::new();
    for line in out.trim().split('\n') {
        let parts: Vec<&str> = line.split(',').map(str::trim).collect();
        if parts.len() != 5 {
            continue;
        }
        let n = |s: &str| s.parse::<i64>().unwrap_or(0);
        gpus.push(GpuStat {
            name: parts[0].to_string(),
            used: n(parts[1]),
            total: n(parts[2]),
            util: n(parts[3]),
            temp: n(parts[4]),
        });
    }
    gpus
}

/// `gpuStatsErr` : lecture FRAÎCHE, erreur de la requête comprise.
pub async fn gpu_stats_err() -> io::Result<Vec<GpuStat>> {
    let out = nvidia_smi_query().await?;
    Ok(parse_nvidia_smi(&String::from_utf8_lossy(&out)))
}

/// `gpuStats` : lecture FRAÎCHE, vide quand nvidia-smi est absent ou échoue
/// (l'absence de mesure n'est pas une erreur). Le bilan d'un déchargement doit
/// passer par ici, jamais par le cache.
pub async fn gpu_stats() -> Vec<GpuStat> {
    gpu_stats_err().await.unwrap_or_default()
}

/// `gpuCache` : la lecture des jauges, partagée. Le verrou tenu pendant la
/// requête fait que des appels simultanés attendent la même lecture au lieu
/// d'en lancer chacun une. Absence de nvidia-smi : une minute ; autre erreur
/// (carte en réinitialisation) : retentée au délai normal.
#[derive(Default)]
pub struct GpuCache {
    state: Mutex<GpuCacheState>,
}

#[derive(Default)]
struct GpuCacheState {
    exp: Option<Instant>,
    gpus: Vec<GpuStat>,
}

impl GpuCache {
    pub fn new() -> Self {
        Self::default()
    }

    /// `get` : `now` est l'horloge (`Instant::now` hors tests), `query` la
    /// requête lancée seulement quand la lecture en cache a expiré.
    pub async fn get<N, Q, F>(&self, now: N, query: Q) -> Vec<GpuStat>
    where
        N: Fn() -> Instant,
        Q: FnOnce() -> F,
        F: Future<Output = io::Result<Vec<GpuStat>>>,
    {
        let mut c = self.state.lock().await;
        if c.exp.is_some_and(|exp| now() < exp) {
            return c.gpus.clone();
        }
        let (gpus, ttl) = match query().await {
            Ok(g) => (g, GPU_CACHE_TTL),
            Err(e) if e.kind() == io::ErrorKind::NotFound => (Vec::new(), GPU_CACHE_ABSENT),
            Err(_) => (Vec::new(), GPU_CACHE_TTL),
        };
        // L'échéance part de la FIN de la requête : un nvidia-smi lent ne doit
        // pas laisser une lecture déjà vieille passer pour neuve.
        c.gpus = gpus;
        c.exp = Some(now() + ttl);
        c.gpus.clone()
    }

    /// `invalidate` : la prochaine lecture repart de nvidia-smi.
    pub async fn invalidate(&self) {
        self.state.lock().await.exp = None;
    }
}

/// `gpuMonitor` : le cache des jauges du moniteur.
pub static GPU_MONITOR: LazyLock<GpuCache> = LazyLock::new(GpuCache::new);

/// `gpuStatsCached` : la lecture des jauges du moniteur (`/api/vram`),
/// SEULEMENT. Le déchargement garde [`gpu_stats`] : une valeur servie deux fois
/// par le cache fausserait son bilan.
pub async fn gpu_stats_cached() -> Vec<GpuStat> {
    GPU_MONITOR.get(Instant::now, gpu_stats_err).await
}

// --- Montages (/proc/self/mountinfo) ------------------------------------------

/// `path.Clean` du Go (chemins Linux, `/` seul séparateur) : traitement
/// purement lexical, « » → « . ».
pub fn clean_path(p: &str) -> String {
    if p.is_empty() {
        return ".".into();
    }
    let rooted = p.starts_with('/');
    let mut parts: Vec<&str> = Vec::new();
    for el in p.split('/') {
        match el {
            "" | "." => {}
            ".." => {
                if parts.last().is_some_and(|l| *l != "..") {
                    parts.pop();
                } else if !rooted {
                    parts.push("..");
                }
            }
            _ => parts.push(el),
        }
    }
    let joined = parts.join("/");
    if rooted {
        format!("/{joined}")
    } else if joined.is_empty() {
        ".".into()
    } else {
        joined
    }
}

/// `mountEntry` : ce qu'on retient d'une ligne de `/proc/self/mountinfo`.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct MountEntry {
    /// Point de montage (5ᵉ champ), échappements décodés, nettoyé.
    pub point: String,
    /// Type de système de fichiers (1ᵉʳ champ après « - »).
    pub fs_type: String,
}

/// `parseMountinfo` : le nombre de champs optionnels (shared:N, master:N…)
/// varie d'une ligne à l'autre ; le type de système de fichiers se lit donc
/// APRÈS le séparateur « - », jamais à un index fixe. Lignes malformées
/// ignorées.
pub fn parse_mountinfo(text: &str) -> Vec<MountEntry> {
    let mut out = Vec::new();
    for line in text.lines() {
        let fields: Vec<&str> = line.split_whitespace().collect();
        if fields.len() < 5 {
            continue;
        }
        let Some(sep) = (5..fields.len()).find(|&i| fields[i] == "-") else {
            continue;
        };
        if sep + 1 >= fields.len() {
            continue;
        }
        out.push(MountEntry {
            point: clean_path(&unescape_mountinfo(fields[4])),
            fs_type: fields[sep + 1].to_string(),
        });
    }
    out
}

/// `unescapeMountinfo` : décode les séquences octales du noyau (\040 espace,
/// \011 tabulation, \012 saut de ligne, \134 antislash). Une séquence tronquée,
/// non octale ou au-delà d'un octet est gardée telle quelle.
pub fn unescape_mountinfo(s: &str) -> String {
    if !s.contains('\\') {
        return s.to_string();
    }
    let b = s.as_bytes();
    let mut out = Vec::with_capacity(b.len());
    let mut i = 0;
    while i < b.len() {
        if b[i] == b'\\'
            && i + 3 < b.len()
            && let Some(v) = octal_byte(&b[i + 1..i + 4])
        {
            out.push(v);
            i += 4;
            continue;
        }
        out.push(b[i]);
        i += 1;
    }
    String::from_utf8_lossy(&out).into_owned()
}

/// `strconv.ParseUint(s, 8, 8)` sur trois caractères : chiffres octaux et
/// valeur tenant sur un octet.
fn octal_byte(d: &[u8]) -> Option<u8> {
    let mut v: u32 = 0;
    for &c in d {
        if !(b'0'..=b'7').contains(&c) {
            return None;
        }
        v = v * 8 + u32::from(c - b'0');
    }
    u8::try_from(v).ok()
}

/// `mountFSType` : type du système de fichiers qui porte `target` (règle de
/// [`mount_of`]) ; "" quand aucun montage ne le couvre.
pub fn mount_fs_type(mounts: &[MountEntry], target: &str) -> String {
    mount_of(mounts, target)
        .map(|m| m.fs_type.clone())
        .unwrap_or_default()
}

/// `mountOf` : le montage au point de montage le plus LONG qui contient
/// `target` (/data/models peut être un montage imbriqué distinct de /data). À
/// longueur égale, la dernière ligne gagne — c'est le montage empilé
/// par-dessus, celui qu'on voit vraiment.
pub fn mount_of<'a>(mounts: &'a [MountEntry], target: &str) -> Option<&'a MountEntry> {
    let p = clean_path(target);
    let mut best: Option<(usize, &MountEntry)> = None;
    for m in mounts {
        let mp = m.point.as_str();
        if mp != "/" && p != mp && !p.starts_with(&format!("{mp}/")) {
            continue;
        }
        if best.is_none_or(|(len, _)| mp.len() >= len) {
            best = Some((mp.len(), m));
        }
    }
    best.map(|(_, m)| m)
}

/// `shfsPaths` : parmi `paths`, ceux qui passent par fuse.shfs, dans l'ordre
/// reçu et un seul par montage (/data/models, sur le montage de /data,
/// n'apprendrait rien de plus à l'utilisateur).
pub fn shfs_paths(mounts: &[MountEntry], paths: &[String]) -> Vec<String> {
    let mut out = Vec::new();
    let mut seen = std::collections::HashSet::new();
    for p in paths {
        if p.trim().is_empty() {
            continue;
        }
        let Some(m) = mount_of(mounts, p) else {
            continue;
        };
        if m.fs_type != "fuse.shfs" || !seen.insert(m.point.clone()) {
            continue;
        }
        out.push(clean_path(p));
    }
    out
}

/// `storageHintText` : le conseil pour les chemins concernés ("" si aucun).
/// Conseil uniquement : les fichiers sont intacts, seule la vitesse en pâtit.
pub fn storage_hint_text(paths: &[String]) -> String {
    if paths.is_empty() {
        return String::new();
    }
    format!(
        "ℹ️ Stockage via la couche FUSE des partages Unraid (fuse.shfs) : {}. \
         Les écritures de la base et les pages du modèle relues depuis le disque (gros modèle qui ne tient pas en RAM) \
         traversent un démon en espace utilisateur, ce qui ralentit le décodage. Rien n'est perdu. \
         Remède sans changer de chemin (Unraid 6.12+) : Global Share Settings → « Permit exclusive shares » = Yes, \
         partages appdata/modèles entièrement sur un pool sans stockage secondaire, jusqu'à lire « Exclusive access : Yes », \
         puis redémarrer le conteneur. Voir docker-compose.unraid.yml.",
        paths.join(", ")
    )
}

/// Cache de [`storage_fuse_hint`] : `/api/status` est interrogé en boucle, et
/// la liste des dossiers de modèles passe par la base. Une minute suffit à
/// refléter un dossier ajouté dans l'UI.
#[cfg(target_os = "linux")]
static STORAGE_HINT_CACHE: LazyLock<Mutex<(Option<Instant>, String)>> =
    LazyLock::new(|| Mutex::new((None, String::new())));

/// `storageFuseHint` : le conseil « /data ou /models sur fuse.shfs » ("" si
/// sans objet). Linux en conteneur seulement, comme [`data_volume_warning`].
///
/// `model_dirs` rend `modelDirs()` du Go (`<LOKI_HOME>/models` puis les
/// dossiers supplémentaires de la base et de `LOKI_MODEL_DIRS`) ; il n'est
/// appelé qu'à l'expiration du cache, et seulement si mountinfo est lisible.
pub async fn storage_fuse_hint<F, Fut>(model_dirs: F) -> String
where
    F: FnOnce() -> Fut,
    Fut: Future<Output = Vec<String>>,
{
    #[cfg(target_os = "linux")]
    {
        if !in_container() {
            return String::new();
        }
        let mut c = STORAGE_HINT_CACHE.lock().await;
        if c.0.is_some_and(|at| at.elapsed() < Duration::from_secs(60)) {
            return c.1.clone();
        }
        *c = (Some(Instant::now()), String::new());
        let Ok(b) = tokio::fs::read("/proc/self/mountinfo").await else {
            return String::new(); // illisible : on ne conseille rien plutôt que de deviner
        };
        let mounts = parse_mountinfo(&String::from_utf8_lossy(&b));
        let mut paths = vec![loki_home()];
        paths.extend(model_dirs().await);
        c.1 = storage_hint_text(&shfs_paths(&mounts, &paths));
        c.1.clone()
    }
    #[cfg(not(target_os = "linux"))]
    {
        let _ = model_dirs;
        String::new()
    }
}

// --- Volume de données ----------------------------------------------------------

/// `dataDirIsMounted` : `home` est-il un point de montage d'après le texte de
/// `/proc/self/mountinfo` ? Seul le 5ᵉ champ compte, `\040` décodé en espace
/// (et seulement lui, comme le Go), comparaison après nettoyage des chemins.
pub fn data_dir_is_mounted(mountinfo: &str, home: &str) -> bool {
    let target = clean_path(home);
    mountinfo.lines().any(|line| {
        let fields: Vec<&str> = line.split_whitespace().collect();
        fields.len() >= 5 && clean_path(&fields[4].replace("\\040", " ")) == target
    })
}

/// Le message d'alerte pour `home` d'après mountinfo ("" si `home` est monté).
pub fn data_volume_warning_for(mountinfo: &str, home: &str) -> String {
    if data_dir_is_mounted(mountinfo, home) {
        return String::new();
    }
    format!(
        "⚠️ {home} n'est PAS un volume monté : modèles, discussions et fichiers seront PERDUS à la recréation du conteneur (mise à jour, redémarrage du serveur). \
         Ajoute un mapping de volume — compose : « - /mnt/user/appdata/loki/data:/data » (et « …/models:/models ») ; \
         docker run : « -v /chemin/hote/data:/data ». Voir docker-compose.unraid.yml du dépôt."
    )
}

/// `dataVolumeWarning` : l'alerte si les données sont en péril ("" si tout va
/// bien, hors conteneur, ou si mountinfo est illisible — on ne crie pas au
/// loup sur une simple erreur de lecture). Hors Linux, mountinfo n'existe pas :
/// toujours "".
pub fn data_volume_warning() -> String {
    if !in_container() {
        return String::new(); // hors conteneur : l'utilisateur gère ses chemins
    }
    match std::fs::read("/proc/self/mountinfo") {
        Ok(b) => data_volume_warning_for(&String::from_utf8_lossy(&b), &loki_home()),
        Err(_) => String::new(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;
    use std::sync::atomic::{AtomicU32, Ordering};

    // --- sys_storagefuse_test.go ---

    /// Extrait réaliste d'un conteneur Loki sur Unraid : racine overlay, /data
    /// et /models en bind depuis /mnt/user (fuse.shfs), un /data/models
    /// imbriqué sur un pool btrfs, champs optionnels de longueur variable, un
    /// chemin à espace.
    const MOUNTINFO_UNRAID: &str = r"612 553 0:156 / / rw,relatime master:221 - overlay overlay rw,lowerdir=/var/lib/docker/l
613 612 0:159 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
620 612 0:52 /appdata/loki/data /data rw,noatime - fuse.shfs shfs rw,user_id=0,group_id=0,allow_other
621 612 0:52 /appdata/loki/models /models rw,noatime shared:5 master:7 - fuse.shfs shfs rw,user_id=0
622 620 0:61 /appdata/loki/fast /data/models rw,noatime - btrfs /dev/nvme0n1p1 rw,ssd
623 612 0:52 /media /mnt/mes\040modeles rw,noatime - fuse.shfs shfs rw
garbage line
624 612 8:1 / /broken rw
";

    fn strs(v: &[&str]) -> Vec<String> {
        v.iter().map(|s| s.to_string()).collect()
    }

    #[test]
    fn parse_mountinfo_unraid() {
        let m = parse_mountinfo(MOUNTINFO_UNRAID);
        assert_eq!(m.len(), 6, "attendu 6 montages valides : {m:?}");
        // Champs optionnels (shared:5 master:7) : le type se lit après « - ».
        assert_eq!(m[3].point, "/models", "ligne à champs optionnels mal lue");
        assert_eq!(
            m[3].fs_type, "fuse.shfs",
            "ligne à champs optionnels mal lue"
        );
        assert_eq!(
            m[5].point, "/mnt/mes modeles",
            "échappement \\040 non décodé"
        );
    }

    #[test]
    fn mount_fs_type_cas() {
        let m = parse_mountinfo(MOUNTINFO_UNRAID);
        for (path, want) in [
            ("/data", "fuse.shfs"),
            ("/data/loki.db", "fuse.shfs"),
            ("/data/models", "btrfs"), // montage imbriqué : le plus long gagne
            ("/data/models/x.gguf", "btrfs"),
            ("/data/modelsX", "fuse.shfs"), // préfixe de chaîne ≠ préfixe de chemin
            ("/models", "fuse.shfs"),
            ("/mnt/mes modeles/a.gguf", "fuse.shfs"),
            ("/opt/loki", "overlay"),
        ] {
            assert_eq!(mount_fs_type(&m, path), want, "mount_fs_type({path:?})");
        }
        assert_eq!(mount_fs_type(&[], "/data"), "", "sans montage connu");
    }

    #[test]
    fn shfs_paths_cas() {
        let m = parse_mountinfo(MOUNTINFO_UNRAID);
        let got = shfs_paths(
            &m,
            &strs(&["/data", "/data/models", "/models", "/models/", "/opt"]),
        );
        assert_eq!(got, strs(&["/data", "/models"]));
        // Un sous-dossier du même montage (LOKI_MODEL_DIRS=/data/gguf) n'est pas
        // répété : un seul chemin par montage fuse.shfs.
        let got = shfs_paths(&m, &strs(&["/data/gguf", "/data", "/mnt/mes modeles/a"]));
        assert_eq!(
            got,
            strs(&["/data/gguf", "/mnt/mes modeles/a"]),
            "même montage"
        );
        // Partages en « Exclusive access » : plus de fuse.shfs, plus de conseil.
        let excl = "1 0 0:1 / / rw - overlay overlay rw
2 1 0:2 /appdata/loki/data /data rw - btrfs /dev/nvme0n1p1 rw
3 1 0:3 /loki/models /models rw - xfs /dev/md1p1 rw
";
        let p = shfs_paths(&parse_mountinfo(excl), &strs(&["/data", "/models"]));
        assert!(p.is_empty(), "aucun chemin ne devrait être signalé : {p:?}");
    }

    #[test]
    fn storage_hint_text_cas() {
        assert_eq!(storage_hint_text(&[]), "", "rien à signaler");
        let s = storage_hint_text(&strs(&["/data", "/models"]));
        for w in [
            "/data, /models",
            "fuse.shfs",
            "Permit exclusive shares",
            "Exclusive access",
            "Rien n'est perdu",
        ] {
            assert!(s.contains(w), "conseil sans {w:?} : {s}");
        }
    }

    // TestStorageFuseHintOutsideContainer touche à l'environnement : porté dans
    // tests/sysinfo.rs (processus à part, seul test qui modifie l'env).

    // --- web_vram_test.go (TestGpuCache) ---

    fn fake() -> Vec<GpuStat> {
        vec![GpuStat {
            name: "fake".into(),
            used: 1000,
            total: 0,
            util: 0,
            temp: 0,
        }]
    }

    #[tokio::test]
    async fn gpu_cache_sequences() {
        struct Step {
            at: Duration,               // horloge depuis le départ
            err: Option<io::ErrorKind>, // erreur rendue si la requête part
            calls: u32,                 // requêtes cumulées attendues
        }
        let st = |at: Duration, err: Option<io::ErrorKind>, calls| Step { at, err, calls };
        let s = Duration::from_secs;
        let other = Some(io::ErrorKind::Other); // « exit status 9 »
        let absent = Some(io::ErrorKind::NotFound);
        for (name, steps) in [
            (
                "lecture réutilisée puis rafraîchie",
                vec![
                    st(s(0), None, 1),
                    st(s(1), None, 1),
                    st(GPU_CACHE_TTL, None, 2),
                ],
            ),
            // Carte en réinitialisation : on retente au délai normal, pas plus tard.
            (
                "erreur retentée au délai normal",
                vec![
                    st(s(0), other, 1),
                    st(s(1), None, 1),
                    st(GPU_CACHE_TTL, None, 2),
                ],
            ),
            // Pas de nvidia-smi du tout : une recherche par minute suffit.
            (
                "binaire absent mémorisé",
                vec![
                    st(s(0), absent, 1),
                    st(s(30), absent, 1),
                    st(GPU_CACHE_ABSENT, absent, 2),
                ],
            ),
        ] {
            let cache = GpuCache::new();
            let calls = AtomicU32::new(0);
            let t0 = Instant::now();
            for (i, step) in steps.iter().enumerate() {
                cache
                    .get(
                        || t0 + step.at,
                        || async {
                            calls.fetch_add(1, Ordering::SeqCst);
                            match step.err {
                                Some(k) => Err(io::Error::new(k, "échec")),
                                None => Ok(fake()),
                            }
                        },
                    )
                    .await;
                let got = calls.load(Ordering::SeqCst);
                assert_eq!(got, step.calls, "{name}, étape {i} : {got} requêtes");
            }
        }
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    async fn gpu_cache_appels_simultanes() {
        let cache = Arc::new(GpuCache::new());
        let calls = Arc::new(AtomicU32::new(0));
        let mut tasks = Vec::new();
        for _ in 0..16 {
            let (cache, calls) = (cache.clone(), calls.clone());
            tasks.push(tokio::spawn(async move {
                cache
                    .get(Instant::now, || async {
                        calls.fetch_add(1, Ordering::SeqCst);
                        // nvidia-smi n'est pas instantané
                        tokio::time::sleep(Duration::from_millis(20)).await;
                        Ok(fake())
                    })
                    .await
            }));
        }
        for t in tasks {
            let g = t.await.unwrap();
            assert!(g.len() == 1 && g[0].used == 1000, "lecture partagée {g:?}");
        }
        let n = calls.load(Ordering::SeqCst);
        assert_eq!(n, 1, "{n} nvidia-smi lancés, attendu 1");
    }

    #[tokio::test]
    async fn gpu_cache_invalidate() {
        let cache = GpuCache::new();
        let calls = AtomicU32::new(0);
        let q = || async {
            calls.fetch_add(1, Ordering::SeqCst);
            Ok(fake())
        };
        cache.get(Instant::now, q).await;
        cache.get(Instant::now, q).await;
        assert_eq!(calls.load(Ordering::SeqCst), 1);
        cache.invalidate().await;
        cache.get(Instant::now, q).await;
        assert_eq!(
            calls.load(Ordering::SeqCst),
            2,
            "invalidate : relecture attendue"
        );
    }

    // TestNvidiaSmiQueryAbsent : un exécutable introuvable doit rendre
    // NotFound (sinon Mac et CPU seul relanceraient une recherche toutes les
    // deux secondes). Le Go vide le PATH ; ici, un nom qui n'existe nulle part.
    #[tokio::test]
    async fn nvidia_smi_query_absent() {
        let err = nvidia_smi_query_bin("loki-nvidia-smi-introuvable-7f3a")
            .await
            .unwrap_err();
        assert_eq!(err.kind(), io::ErrorKind::NotFound, "{err}");
    }

    // Lecture de TestGpuSettleSampleFrais (« Fake GPU, 19000, 24000, 0, 40 »)
    // et cas limites de l'analyse de gpuStatsErr.
    #[test]
    fn parse_nvidia_smi_cas() {
        let g = parse_nvidia_smi("Fake GPU, 19000, 24000, 0, 40\n");
        assert_eq!(
            g,
            vec![GpuStat {
                name: "Fake GPU".into(),
                used: 19000,
                total: 24000,
                util: 0,
                temp: 40
            }]
        );
        let g = parse_nvidia_smi(
            "\nNVIDIA RTX 4090, 1200, 24564, 7, 45\r\nligne cassée\nTesla, [N/A], 16384, [N/A], 50\n\n",
        );
        assert_eq!(g.len(), 2);
        assert_eq!(g[0].name, "NVIDIA RTX 4090");
        assert_eq!(
            (g[0].used, g[0].total, g[0].util, g[0].temp),
            (1200, 24564, 7, 45)
        );
        assert_eq!(
            (g[1].used, g[1].total, g[1].util, g[1].temp),
            (0, 16384, 0, 50)
        );
        assert!(parse_nvidia_smi("").is_empty());
        assert!(
            parse_nvidia_smi("a, 1, 2, 3, 4, 5").is_empty(),
            "6 champs : ignorée"
        );
    }

    #[test]
    fn gpu_stat_json() {
        let v = serde_json::to_value(&fake()[0]).unwrap();
        assert_eq!(
            v,
            serde_json::json!({"name": "fake", "used": 1000, "total": 0, "util": 0, "temp": 0})
        );
    }

    // --- ramUsageMB, dataVolumeWarning (sans test Go : fonctions pures) ---

    #[test]
    fn parse_meminfo_cas() {
        let t = "MemTotal:       32768000 kB\nMemFree:  1000 kB\nMemAvailable:   16384000 kB\n";
        assert_eq!(parse_meminfo(t), (16000, 32000));
        assert_eq!(parse_meminfo("MemFree: 12 kB\n"), (0, 0), "sans total");
        assert_eq!(parse_meminfo(""), (0, 0));
        // Sans MemAvailable : tout compte comme occupé.
        assert_eq!(parse_meminfo("MemTotal: 2048 kB"), (2, 2));
    }

    #[test]
    fn data_volume_cas() {
        assert!(data_dir_is_mounted(MOUNTINFO_UNRAID, "/data"));
        assert!(data_dir_is_mounted(MOUNTINFO_UNRAID, "/data/"));
        assert!(data_dir_is_mounted(MOUNTINFO_UNRAID, "/mnt/mes modeles"));
        assert!(!data_dir_is_mounted(MOUNTINFO_UNRAID, "/opt/loki"));
        assert_eq!(data_volume_warning_for(MOUNTINFO_UNRAID, "/data"), "");
        let w = data_volume_warning_for("1 0 0:1 / / rw - overlay overlay rw\n", "/data");
        assert!(w.starts_with("⚠️ /data n'est PAS un volume monté"), "{w}");
        assert!(
            w.ends_with("Voir docker-compose.unraid.yml du dépôt."),
            "{w}"
        );
    }

    #[test]
    fn clean_path_comme_go() {
        for (p, want) in [
            ("", "."),
            ("/", "/"),
            ("/models/", "/models"),
            ("/a//b/./c/..", "/a/b"),
            ("/..", "/"),
            ("a/../..", ".."),
            ("../a/..", ".."),
            ("./", "."),
        ] {
            assert_eq!(clean_path(p), want, "clean_path({p:?})");
        }
    }

    #[test]
    fn unescape_mountinfo_cas() {
        assert_eq!(unescape_mountinfo(r"/a\040b\011c\134d"), "/a b\tc\\d");
        // Séquence finale décodée, tronquée ou octale invalide gardée telle quelle.
        assert_eq!(unescape_mountinfo(r"/a\040"), "/a ");
        assert_eq!(unescape_mountinfo(r"/a\04"), r"/a\04");
        assert_eq!(unescape_mountinfo(r"/a\777x"), r"/a\777x");
        assert_eq!(unescape_mountinfo(r"/a\08x"), r"/a\08x");
    }
}
