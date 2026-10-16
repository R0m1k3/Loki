//! Verrou de l'optimiseur — port de `backend_tune_lock.go`, au même format.
//!
//! Pendant une optimisation, la VRAM appartient au moteur d'ESSAI : relancer
//! le vrai moteur à côté chargerait deux modèles sur des cartes qui n'en
//! tiennent qu'un. Le verrou est un FICHIER de LOKI_HOME, créé de façon
//! exclusive, que tout le monde lit : le Go (`tuneGuard`, tous ses refus), un
//! « loki tune » en ligne de commande, et cette façade.
//!
//! Le fichier porte l'identité du processus qui le tient (PID, instant de
//! démarrage, binaire) et celle de l'essai en cours. Un verrou dont le
//! propriétaire est mort est périmé : l'essai orphelin est arrêté — seulement
//! si PID, instant de démarrage ET binaire concordent — puis le fichier est
//! retiré. L'identité est écrite comme le Go l'écrit (`procIdentity`) : la
//! reprise du Go (`tuneRecoverWatch`, toutes les 30 s) doit reconnaître la
//! façade vivante, sans quoi elle écarterait son verrou en pleine mesure.

use std::collections::HashSet;
use std::path::{Path, PathBuf};
use std::sync::Mutex;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use serde::{Deserialize, Serialize};

/// `tuneLockName`.
pub const LOCK_NAME: &str = "tune.lock";

/// `tuneLockFreshness` : un verrou illisible plus jeune que ça est en cours
/// d'écriture, pas abandonné.
pub const FRESHNESS: Duration = Duration::from_secs(10);

/// `errTuneBusy`.
pub const ERR_TUNE_BUSY: &str = "optimisation en cours (loki tune) : le moteur est réservé aux essais — attends la fin ou annule-la";

/// `tuneProc` : un processus désigné sans ambiguïté.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Proc {
    pub pid: i64,
    /// `procIdentity` ; vide = illisible au moment de l'écrire.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub start: String,
    /// Binaires admis (un « loki serve » devient llama-server par exec).
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub exes: Vec<String>,
}

/// `tuneOwner` : le contenu du verrou.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Owner {
    pub owner: Proc,
    /// « web », « cli » ou « apply ».
    #[serde(default)]
    pub via: String,
    /// « essais », « application », « relance », « rendu ».
    #[serde(default)]
    pub phase: String,
    #[serde(default)]
    pub since: i64,
    /// Distingue deux verrous pris par le même processus dans la même seconde.
    #[serde(default, skip_serializing_if = "is_zero")]
    pub nonce: i64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub trial: Option<Proc>,
    /// Le vrai moteur tournait avant l'optimisation.
    #[serde(default)]
    pub main_was_active: bool,
    /// Application en cours : la version d'avant du preset, sauvée avant
    /// toute écriture.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub backup: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub preset_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub preset_name: String,
}

fn is_zero(v: &i64) -> bool {
    *v == 0
}

/// `tuneHeldKey`.
pub fn held_key(o: &Owner) -> String {
    format!("{}|{}|{}", o.via, o.since, o.nonce)
}

/// `sameTuneLock` : la même prise de verrou, quelle que soit sa phase.
pub fn same_lock(a: &Owner, b: &Owner) -> bool {
    a.owner.pid == b.owner.pid && a.owner.start == b.owner.start && held_key(a) == held_key(b)
}

/// `sameExe` : deux binaires comparés par leur nom de fichier (sans .exe,
/// sans casse).
pub fn same_exe(a: &str, b: &str) -> bool {
    let norm = |s: &str| {
        let s = s.trim();
        let s = s.strip_suffix(" (deleted)").unwrap_or(s);
        let s = s.replace('\\', "/");
        let base = go_base(&s).to_lowercase();
        base.strip_suffix(".exe").unwrap_or(&base).to_string()
    };
    !a.is_empty() && !b.is_empty() && norm(a) == norm(b)
}

/// `filepath.Base` (séparateur `/`).
fn go_base(p: &str) -> String {
    let t = p.trim_end_matches('/');
    if t.is_empty() {
        return if p.is_empty() { ".".into() } else { "/".into() };
    }
    t.rsplit('/').next().unwrap_or(t).to_string()
}

/// `readTuneOwner` : illisible, ou sans PID : pas de verrou.
pub fn read_owner(path: &Path) -> Option<Owner> {
    let b = std::fs::read(path).ok()?;
    let o: Owner = serde_json::from_slice(&b).ok()?;
    (o.owner.pid > 0).then_some(o)
}

// ---------------------------------------------------------------------------
// Identité d'un processus
// ---------------------------------------------------------------------------

/// `procIdentity` (Linux) : instant de démarrage (champ 22 de
/// `/proc/<pid>/stat`, préfixé de l'identifiant du démarrage de la machine)
/// et binaire. Un zombie a fini de tourner : il ne compte plus.
#[cfg(target_os = "linux")]
pub fn proc_identity(pid: i64) -> Option<(String, String)> {
    proc_identity_in(Path::new("/"), pid)
}

/// [`proc_identity`] sous la racine `root` (les tests).
#[cfg(target_os = "linux")]
pub fn proc_identity_in(root: &Path, pid: i64) -> Option<(String, String)> {
    if pid <= 0 {
        return None;
    }
    let s = std::fs::read_to_string(root.join(format!("proc/{pid}/stat"))).ok()?;
    let st = crate::engine::proctree::parse_proc_stat(&s)?;
    if st.state == "Z" {
        return None;
    }
    let exe = std::fs::read_link(root.join(format!("proc/{pid}/exe")))
        .map(|p| p.to_string_lossy().into_owned())
        .unwrap_or_default();
    // Binaire remplacé par une mise à jour pendant que le processus tourne :
    // le lien porte « (deleted) », c'est pourtant bien le même processus.
    let exe = exe.strip_suffix(" (deleted)").unwrap_or(&exe).to_string();
    let mut start = st.start;
    if let Ok(b) = std::fs::read_to_string(root.join("proc/sys/kernel/random/boot_id")) {
        let id = b.trim();
        if !id.is_empty() {
            start = format!("{id}/{start}");
        }
    }
    Some((start, exe))
}

/// `procIdentity` (autres Unix) : `ps`, langue et fuseau fixés — le
/// processus web et un terminal en français doivent lire la même chaîne.
#[cfg(all(unix, not(target_os = "linux")))]
pub fn proc_identity(pid: i64) -> Option<(String, String)> {
    if pid <= 0 || !pid_alive(pid) {
        return None;
    }
    let p = pid.to_string();
    let out = std::process::Command::new("ps")
        .args(["-o", "lstart=", "-p", &p])
        .env("LC_ALL", "C")
        .env("LANG", "C")
        .env("TZ", "UTC")
        .output()
        .ok()?;
    if !out.status.success() {
        return None;
    }
    let start = String::from_utf8_lossy(&out.stdout)
        .split_whitespace()
        .collect::<Vec<_>>()
        .join(" ");
    if start.is_empty() {
        return None;
    }
    let exe = std::process::Command::new("ps")
        .args(["-o", "comm=", "-p", &p])
        .output()
        .ok()
        .filter(|o| o.status.success())
        .map(|o| String::from_utf8_lossy(&o.stdout).trim().to_string())
        .unwrap_or_default();
    Some((start, exe))
}

/// Hors Unix, la façade ne tient pas l'optimiseur (`main.rs`).
#[cfg(not(unix))]
pub fn proc_identity(_pid: i64) -> Option<(String, String)> {
    None
}

/// `processAlive` : `kill(pid, 0)`.
#[cfg(unix)]
pub fn pid_alive(pid: i64) -> bool {
    let Ok(p) = i32::try_from(pid) else {
        return false;
    };
    // SAFETY: kill(2) au signal 0 n'envoie rien ; un PID absent rend -1.
    p > 0 && unsafe { libc::kill(p, 0) == 0 }
}

#[cfg(not(unix))]
pub fn pid_alive(_pid: i64) -> bool {
    false
}

/// `tuneProcAlive` : `p` désigne-t-il encore le MÊME processus ?
pub fn proc_alive(p: &Proc) -> bool {
    same_proc(p, proc_identity(p.pid))
}

/// [`proc_alive`], l'identité lue (`None` : processus absent).
pub fn same_proc(p: &Proc, identity: Option<(String, String)>) -> bool {
    let Some((start, exe)) = identity else {
        return false;
    };
    if !p.start.is_empty() && start != p.start {
        return false;
    }
    if exe.is_empty() || p.exes.is_empty() {
        return true;
    }
    p.exes.iter().any(|e| same_exe(e, &exe))
}

/// `selfProc` : ce processus, tel que le verrou le note.
pub fn self_proc() -> Proc {
    let pid = i64::from(std::process::id());
    let mut p = Proc {
        pid,
        ..Default::default()
    };
    if let Some((start, exe)) = proc_identity(pid) {
        p.start = start;
        if !exe.is_empty() {
            p.exes = vec![exe];
        }
    }
    if p.exes.is_empty()
        && let Ok(me) = std::env::current_exe()
    {
        p.exes = vec![me.to_string_lossy().into_owned()];
    }
    p
}

/// `tuneKillTree` (Unix) : SIGTERM au groupe de l'essai, puis SIGKILL s'il
/// traîne. `gone` dit si le processus a disparu (récolté par l'appelant, ou
/// sondé pour un orphelin d'un ancien Loki).
#[cfg(unix)]
pub fn kill_tree(pid: i64, gone: &dyn Fn() -> bool) {
    kill_tree_within(pid, gone, Duration::from_secs(10));
}

/// [`kill_tree`], `grace` laissé au SIGTERM.
#[cfg(unix)]
pub fn kill_tree_within(pid: i64, gone: &dyn Fn() -> bool, grace: Duration) {
    let Ok(p) = i32::try_from(pid) else {
        return;
    };
    if p <= 0 {
        return;
    }
    let wait = |d: Duration| {
        let deadline = std::time::Instant::now() + d;
        while std::time::Instant::now() < deadline {
            if gone() {
                return true;
            }
            std::thread::sleep(Duration::from_millis(100));
        }
        false
    };
    // SAFETY: kill(2) n'a pas d'effet mémoire.
    unsafe {
        if libc::kill(-p, libc::SIGTERM) != 0 {
            libc::kill(p, libc::SIGTERM);
        }
    }
    if wait(grace) {
        return;
    }
    // SAFETY: idem.
    unsafe {
        libc::kill(-p, libc::SIGKILL);
    }
    wait(Duration::from_secs(5));
}

#[cfg(not(unix))]
pub fn kill_tree(_pid: i64, _gone: &dyn Fn() -> bool) {}

// ---------------------------------------------------------------------------
// Le verrou
// ---------------------------------------------------------------------------

/// Qui est vivant : [`proc_alive`] hors tests.
pub type AliveFn = Box<dyn Fn(&Proc) -> bool + Send + Sync>;

/// Le fichier de verrou de CE processus : les prises en cours (`tuneHeld`)
/// et le verrou des opérations sur le fichier (`tuneFileMu`).
pub struct LockFile {
    pub path: PathBuf,
    held: Mutex<HashSet<String>>,
    file_mu: Mutex<()>,
    alive: AliveFn,
    pid: i64,
    me: Proc,
}

/// Une prise de verrou à défaire après le ménage d'un verrou périmé : l'essai
/// orphelin à arrêter et l'application à défaire sont à l'appelant.
pub type Claimed = Owner;

impl LockFile {
    pub fn new(home: &Path) -> Self {
        Self::with(home, Box::new(proc_alive), self_proc())
    }

    /// `alive` et `me` remplaçables par les tests.
    pub fn with(home: &Path, alive: AliveFn, me: Proc) -> Self {
        LockFile {
            path: home.join(LOCK_NAME),
            held: Mutex::new(HashSet::new()),
            file_mu: Mutex::new(()),
            alive,
            pid: me.pid,
            me,
        }
    }

    fn held(&self) -> std::sync::MutexGuard<'_, HashSet<String>> {
        self.held.lock().unwrap_or_else(|e| e.into_inner())
    }

    fn file(&self) -> std::sync::MutexGuard<'_, ()> {
        self.file_mu.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// `tuneOwnerAlive` : un verrou qui porte NOTRE PID sans être l'un des
    /// nôtres vient d'un processus mort dont le PID nous est échu.
    pub fn owner_alive(&self, o: &Owner) -> bool {
        if o.owner.pid == self.pid && !self.held().contains(&held_key(o)) {
            return false;
        }
        (self.alive)(&o.owner)
    }

    pub fn proc_alive(&self, p: &Proc) -> bool {
        (self.alive)(p)
    }

    /// `tuneActive` : une optimisation tient-elle le moteur ? Un verrou
    /// périmé ne compte pas.
    pub fn active(&self) -> Option<Owner> {
        let o = read_owner(&self.path)?;
        self.owner_alive(&o).then_some(o)
    }

    /// `tuneLockAcquire`, premier temps : la prise, ou qui la tient. `Err(None)`
    /// : un verrou périmé à écarter ([`Self::reap_claim`]) avant de réessayer.
    fn try_acquire(&self, owner: &Owner) -> Result<(), Option<String>> {
        let _f = self.file();
        let mut opts = std::fs::OpenOptions::new();
        opts.write(true).create_new(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            opts.mode(0o644);
        }
        match opts.open(&self.path) {
            Ok(mut f) => {
                use std::io::Write as _;
                let b = serde_json::to_vec(owner).unwrap_or_default();
                let w = f.write_all(&b).and_then(|_| f.sync_all());
                drop(f);
                if let Err(e) = w {
                    let _ = std::fs::remove_file(&self.path);
                    return Err(Some(format!("verrou de l'optimiseur : {e}")));
                }
                Ok(())
            }
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => Err(None),
            Err(e) => Err(Some(e.to_string())),
        }
    }

    /// `tuneLockAcquire` : prend le verrou, ou dit qui le tient. `reap` écarte
    /// un verrou périmé (essai orphelin et application interrompue compris).
    pub async fn acquire<F, Fut>(
        &self,
        via: &str,
        main_active: bool,
        reap: F,
    ) -> Result<Held<'_>, String>
    where
        F: Fn() -> Fut,
        Fut: std::future::Future<Output = bool>,
    {
        if let Some(dir) = self.path.parent() {
            std::fs::create_dir_all(dir).map_err(|e| e.to_string())?;
        }
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default();
        let owner = Owner {
            owner: self.me.clone(),
            via: via.into(),
            phase: "essais".into(),
            since: now.as_secs() as i64,
            nonce: now.as_nanos() as i64,
            main_was_active: main_active,
            ..Default::default()
        };
        // Inscrit comme nôtre AVANT que le fichier n'existe : la reprise tient
        // pour mort un verrou qui porte notre PID sans être inscrit.
        let key = held_key(&owner);
        self.held().insert(key.clone());
        for _ in 0..3 {
            match self.try_acquire(&owner) {
                Ok(()) => {
                    return Ok(Held {
                        file: self,
                        owner: Mutex::new(owner),
                        held: Mutex::new(true),
                    });
                }
                Err(Some(e)) => {
                    self.held().remove(&key);
                    return Err(e);
                }
                Err(None) => {}
            }
            if let Some(o) = self.active() {
                self.held().remove(&key);
                let c = crate::tz::Location::local().civil(o.since);
                return Err(format!(
                    "une optimisation tourne déjà ({}, PID {}, depuis {:02}:{:02})",
                    o.via, o.owner.pid, c.hour, c.minute
                ));
            }
            reap().await;
        }
        self.held().remove(&key);
        Err("verrou de l'optimiseur indisponible : réessaie".into())
    }

    /// `tuneReapClaim` : la partie du ménage qui touche au fichier — lecture,
    /// déplacement, relecture, remise en place. Rend le verrou périmé
    /// effectivement retiré (`None` : rien à écarter).
    pub fn reap_claim(&self) -> Option<Claimed> {
        let _f = self.file();
        let Some(o) = read_owner(&self.path) else {
            // Illisible (écriture interrompue) : périmé lui aussi — mais pas
            // tout juste créé (un verrou neuf est vide quelques instants).
            if let Ok(md) = std::fs::metadata(&self.path)
                && md
                    .modified()
                    .ok()
                    .and_then(|m| m.elapsed().ok())
                    .is_some_and(|age| age > FRESHNESS)
            {
                let _ = std::fs::remove_file(&self.path);
            }
            return None;
        };
        if self.owner_alive(&o) {
            return None;
        }
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos();
        let stale = PathBuf::from(format!(
            "{}.stale-{}-{nanos}",
            self.path.display(),
            self.pid
        ));
        std::fs::rename(&self.path, &stale).ok()?;
        let back = |stale: &Path| {
            // Un autre verrou, vivant, déplacé par erreur : remis en place (un
            // lien : jamais par-dessus un verrou posé entre-temps).
            if let Err(e) = std::fs::hard_link(stale, &self.path) {
                eprintln!(
                    "[loki tune] verrou vivant déplacé par erreur, remise en place impossible : {e}"
                );
            }
        };
        let got = read_owner(&stale);
        let claimed = match got {
            Some(g) if same_lock(&g, &o) && !self.owner_alive(&g) => Some(o),
            _ => {
                back(&stale);
                None
            }
        };
        let _ = std::fs::remove_file(&stale);
        claimed
    }
}

/// Le verrou tenu par CE processus (`tuneLock`).
pub struct Held<'a> {
    file: &'a LockFile,
    owner: Mutex<Owner>,
    held: Mutex<bool>,
}

impl Held<'_> {
    fn owner(&self) -> std::sync::MutexGuard<'_, Owner> {
        self.owner.lock().unwrap_or_else(|e| e.into_inner())
    }

    pub fn snapshot(&self) -> Owner {
        self.owner().clone()
    }

    /// `writeLocked` : réécrit d'un bloc (fichier voisin puis renommage) — un
    /// lecteur ne voit jamais un JSON à moitié écrit.
    fn write_locked(&self, o: &Owner) -> std::io::Result<()> {
        let b = serde_json::to_vec(o).map_err(std::io::Error::other)?;
        let tmp = PathBuf::from(format!(
            "{}.tmp-{}",
            self.file.path.display(),
            self.file.pid
        ));
        std::fs::write(&tmp, b)?;
        let mut i = 0;
        loop {
            match std::fs::rename(&tmp, &self.file.path) {
                Ok(()) => return Ok(()),
                Err(e) if i == 4 => {
                    let _ = std::fs::remove_file(&tmp);
                    return Err(e);
                }
                Err(_) => {
                    i += 1;
                    std::thread::sleep(Duration::from_millis(50));
                }
            }
        }
    }

    fn update(&self, f: impl FnOnce(&mut Owner)) {
        let mut o = self.owner();
        f(&mut o);
        let _f = self.file.file();
        let _ = self.write_locked(&o);
    }

    /// `setTrial` : l'essai en cours (`None` : aucun), noté AVANT qu'il ne
    /// charge quoi que ce soit — c'est ce qui permet de l'arrêter après un
    /// redémarrage.
    pub fn set_trial(&self, p: Option<Proc>) {
        self.update(|o| o.trial = p);
    }

    pub fn set_phase(&self, phase: &str) {
        self.update(|o| o.phase = phase.into());
    }

    /// `setBackup` : la sauvegarde du preset avant son écriture.
    pub fn set_backup(&self, backup: &str, id: &str, name: &str) {
        self.update(|o| {
            o.backup = backup.into();
            o.preset_id = id.into();
            o.preset_name = name.into();
        });
    }

    /// `release` : rend le verrou — seulement s'il est encore le nôtre. Réécrit
    /// d'abord NEUTRE (rien à défaire, rien à relancer, aucun essai), puis
    /// retiré ; l'inscription ne part qu'après.
    pub fn release(&self) {
        let mut o = self.owner();
        let _f = self.file.file();
        let mut held = self.held.lock().unwrap_or_else(|e| e.into_inner());
        if *held {
            let mut ours = false;
            for _ in 0..5 {
                if let Some(cur) = read_owner(&self.file.path) {
                    ours = same_lock(&cur, &o);
                    break;
                }
                std::thread::sleep(Duration::from_millis(20));
            }
            if ours {
                o.phase = "rendu".into();
                o.trial = None;
                o.main_was_active = false;
                o.backup.clear();
                o.preset_id.clear();
                o.preset_name.clear();
                let _ = self.write_locked(&o);
                for _ in 0..5 {
                    match std::fs::remove_file(&self.file.path) {
                        Ok(()) => break,
                        Err(e) if e.kind() == std::io::ErrorKind::NotFound => break,
                        Err(_) => std::thread::sleep(Duration::from_millis(20)),
                    }
                }
            }
            *held = false;
            self.file.held().remove(&held_key(&o));
            return;
        }
        if read_owner(&self.file.path).is_some_and(|cur| same_lock(&cur, &o)) {
            let _ = std::fs::remove_file(&self.file.path);
        }
    }
}

impl Drop for Held<'_> {
    fn drop(&mut self) {
        if *self.held.lock().unwrap_or_else(|e| e.into_inner()) {
            self.release();
        }
    }
}
