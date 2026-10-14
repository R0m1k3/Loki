//! Lecteur des métadonnées GGUF (port de `backend_gguf.go`), résolution du
//! modèle à lancer (`resolveServeModelPath`, `backend_model_dirs.go`), familles
//! de tranches (`backend_models_shards.go`) et réponse de
//! `GET /api/model/layers` (`handleModelLayers`, `engine_cmdline.go`).
//!
//! Plusieurs réglages du moteur dépendent de ce que le fichier contient
//! VRAIMENT : une tête MTP, le nombre de têtes KV par couche, une architecture
//! hybride à état récurrent. On ne lit que l'en-tête, la section clé/valeur et
//! la table des tenseurs : les poids ne sont jamais touchés. Le fichier peut
//! être en cours de téléchargement, tronqué, ou pas un GGUF : chaque longueur
//! lue est bornée par ce qui reste du fichier, et la moindre incohérence donne
//! une erreur, jamais un plantage ni une allocation géante. Une erreur veut
//! dire « on ne sait pas » : l'appelant reste sur le comportement prudent.
//!
//! Données du store : la liste des dossiers de modèles ajoutés vit dans le
//! bucket `state`, clé `model_dirs` (JSON `[]string`, voir
//! [`STATE_MODEL_DIRS_KEY`] et [`parse_stored_model_dirs`]) ; l'appelant la lit
//! par `crate::store::Store` et la passe aux fonctions de résolution, avec
//! `$LOKI_MODEL_DIRS`.

use serde_json::{Value, json};
use std::collections::HashMap;
use std::fmt::Write as _;
use std::fs::File;
use std::io::{self, BufReader, Read};
use std::path::{Path, PathBuf};
use std::sync::{Mutex, OnceLock};
use std::time::{SystemTime, UNIX_EPOCH};

use crate::paths::{base_name, go_abs, go_clean, go_dir, go_is_abs, go_join, os_err};

/// Bucket `state`, clé des dossiers de modèles ajoutés (`extraModelDirs`).
pub const STATE_MODEL_DIRS_KEY: &str = "model_dirs";
/// Variable d'environnement des dossiers de modèles supplémentaires.
pub const MODEL_DIRS_ENV: &str = "LOKI_MODEL_DIRS";

/// Ce que Loki retient d'un modèle GGUF (`GGUFInfo` du Go). Les champs absents
/// du fichier restent à zéro.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct GgufMeta {
    /// version du format (1 à 3)
    pub version: i64,
    /// general.architecture
    pub arch: String,
    /// `<arch>.block_count`
    pub block_count: i64,
    /// `<arch>.nextn_predict_layers` (couches MTP annoncées)
    pub next_n: i64,
    /// `<arch>.context_length` (contexte d'entraînement)
    pub context_length: i64,
    /// `<arch>.expert_count` (0 = modèle dense)
    pub expert_count: i64,
    /// `<arch>.attention.key_length`
    pub key_len: i64,
    /// `<arch>.attention.value_length`
    pub val_len: i64,
    /// Valeur unique, ou maximum sur les couches quand le fichier donne un
    /// tableau par couche (`head_count_kv_layers`, `None` sinon). Sur un
    /// hybride, les couches récurrentes y valent 0.
    pub head_count_kv: i64,
    pub head_count_kv_layers: Option<Vec<i64>>,
    /// `<arch>.full_attention_interval`, si présent
    pub full_attn_interval: i64,
    /// au moins une clé `<arch>.ssm.*` : couches à état récurrent
    pub hybrid: bool,
    /// `<arch>.embedding_length` (repli de llama.cpp : n_embd / n_head)
    pub embedding_length: i64,
    /// `<arch>.attention.head_count` (valeur unique seulement)
    pub head_count: i64,
    /// `<arch>.ssm.conv_kernel`
    pub ssm_conv: i64,
    /// `<arch>.ssm.inner_size`
    pub ssm_inner: i64,
    /// `<arch>.ssm.state_size`
    pub ssm_state: i64,
    /// `<arch>.ssm.group_count`
    pub ssm_groups: i64,
    /// Le tenseur `blk.{block_count-1}.nextn.eh_proj.weight` existe dans l'une
    /// des tranches : le test de llama.cpp lui-même pour reconnaître une tête
    /// MTP (la clé nextn_predict_layers seule ne suffit pas).
    pub has_next_n_tensor: bool,
    /// total sur toutes les tranches
    pub tensor_count: i64,
}

// Bornes de lecture : larges devant les vrais modèles, elles servent seulement
// à refuser un fichier aberrant avant d'allouer quoi que ce soit.
const GGUF_MAX_STRING: u64 = 64 << 20;
const GGUF_MAX_KEY_LEN: u64 = 64 << 10;
const GGUF_MAX_TENSOR_NAME: u64 = 4096; // GGML_MAX_NAME vaut 64
const GGUF_MAX_KV: u64 = 1 << 20;
const GGUF_MAX_TENSORS: u64 = 1 << 22;
const GGUF_MAX_DIMS: u32 = 8; // GGML_MAX_DIMS vaut 4
const GGUF_MAX_ARRAY_DEPTH: u32 = 4;
const GGUF_MAX_LAYER_ARRAY: u64 = 1 << 16;

// Types de valeur GGUF (gguf.h).
pub const GGUF_UINT8: u32 = 0;
pub const GGUF_INT8: u32 = 1;
pub const GGUF_UINT16: u32 = 2;
pub const GGUF_INT16: u32 = 3;
pub const GGUF_UINT32: u32 = 4;
pub const GGUF_INT32: u32 = 5;
pub const GGUF_FLOAT32: u32 = 6;
pub const GGUF_BOOL: u32 = 7;
pub const GGUF_STRING: u32 = 8;
pub const GGUF_ARRAY: u32 = 9;
pub const GGUF_UINT64: u32 = 10;
pub const GGUF_INT64: u32 = 11;
pub const GGUF_FLOAT64: u32 = 12;

const ERR_TRUNCATED: &str = "GGUF tronqué";

fn truncated<T>() -> Result<T, String> {
    Err(ERR_TRUNCATED.to_string())
}

/// Taille en octets d'un type de taille fixe, 0 sinon.
fn scalar_size(t: u32) -> i64 {
    match t {
        GGUF_UINT8 | GGUF_INT8 | GGUF_BOOL => 1,
        GGUF_UINT16 | GGUF_INT16 => 2,
        GGUF_UINT32 | GGUF_INT32 | GGUF_FLOAT32 => 4,
        GGUF_UINT64 | GGUF_INT64 | GGUF_FLOAT64 => 8,
        _ => 0,
    }
}

/// Lecture séquentielle en comptant ce qui reste du fichier : toute longueur
/// annoncée au-delà est une incohérence, détectée AVANT de lire.
struct Reader<R: Read> {
    r: BufReader<R>,
    be: bool,  // gros-boutiste
    v1: bool,  // GGUF v1 : compteurs et longueurs sur 32 bits
    size: i64, // taille totale du fichier
    left: i64, // octets restants dans le fichier
}

impl<R: Read> Reader<R> {
    fn fixed(&mut self, n: usize) -> Result<[u8; 8], String> {
        if n as i64 > self.left {
            return truncated();
        }
        let mut b = [0u8; 8];
        if self.r.read_exact(&mut b[..n]).is_err() {
            return truncated();
        }
        self.left -= n as i64;
        Ok(b)
    }

    fn u16_of(&self, b: &[u8; 8]) -> u16 {
        let a = [b[0], b[1]];
        if self.be {
            u16::from_be_bytes(a)
        } else {
            u16::from_le_bytes(a)
        }
    }

    fn u32_of(&self, b: &[u8; 8]) -> u32 {
        let a = [b[0], b[1], b[2], b[3]];
        if self.be {
            u32::from_be_bytes(a)
        } else {
            u32::from_le_bytes(a)
        }
    }

    fn u64_of(&self, b: &[u8; 8]) -> u64 {
        if self.be {
            u64::from_be_bytes(*b)
        } else {
            u64::from_le_bytes(*b)
        }
    }

    fn u32(&mut self) -> Result<u32, String> {
        let b = self.fixed(4)?;
        Ok(self.u32_of(&b))
    }

    fn u64(&mut self) -> Result<u64, String> {
        let b = self.fixed(8)?;
        Ok(self.u64_of(&b))
    }

    /// Compteur ou longueur : 32 bits en v1, 64 bits ensuite.
    fn count(&mut self) -> Result<u64, String> {
        if self.v1 {
            self.u32().map(u64::from)
        } else {
            self.u64()
        }
    }

    /// Taille sur disque d'un compteur, pour borner les tableaux de chaînes.
    fn count_size(&self) -> i64 {
        if self.v1 { 4 } else { 8 }
    }

    fn skip(&mut self, n: i64) -> Result<(), String> {
        if n < 0 || n > self.left {
            return truncated();
        }
        let mut lim = Read::by_ref(&mut self.r).take(n as u64);
        match io::copy(&mut lim, &mut io::sink()) {
            Ok(c) if c == n as u64 => {}
            _ => return truncated(),
        }
        self.left -= n;
        Ok(())
    }

    /// Chaîne d'au plus `max` octets.
    fn str(&mut self, max: u64) -> Result<String, String> {
        let n = self.count()?;
        if n > max {
            return Err(format!("chaîne GGUF de {n} octets"));
        }
        if n as i64 > self.left {
            return truncated();
        }
        let mut b = vec![0u8; n as usize];
        if self.r.read_exact(&mut b).is_err() {
            return truncated();
        }
        self.left -= n as i64;
        Ok(String::from_utf8_lossy(&b).into_owned())
    }

    fn skip_str(&mut self) -> Result<(), String> {
        let n = self.count()?;
        if n > GGUF_MAX_STRING {
            return Err(format!("chaîne GGUF de {n} octets"));
        }
        self.skip(n as i64)
    }

    /// Valeur entière (ou booléenne) de type `t`. `None` pour les flottants,
    /// lus et ignorés : aucune des clés retenues n'en est un.
    fn int_val(&mut self, t: u32) -> Result<Option<i64>, String> {
        let size = scalar_size(t);
        if size == 0 {
            return Err(format!("type GGUF {t} inattendu"));
        }
        let b = self.fixed(size as usize)?;
        Ok(match t {
            GGUF_UINT8 | GGUF_BOOL => Some(i64::from(b[0])),
            GGUF_INT8 => Some(i64::from(b[0] as i8)),
            GGUF_UINT16 => Some(i64::from(self.u16_of(&b))),
            GGUF_INT16 => Some(i64::from(self.u16_of(&b) as i16)),
            GGUF_UINT32 => Some(i64::from(self.u32_of(&b))),
            GGUF_INT32 => Some(i64::from(self.u32_of(&b) as i32)),
            GGUF_UINT64 => Some(self.u64_of(&b).min(i64::MAX as u64) as i64),
            GGUF_INT64 => Some(self.u64_of(&b) as i64),
            _ => None, // flottant
        })
    }

    /// Saute `n` éléments de type `et`, tableaux imbriqués compris (profondeur
    /// bornée). Le vocabulaire (150 000 chaînes) passe par ici sans rien
    /// allouer.
    fn skip_array(&mut self, et: u32, n: u64, depth: u32) -> Result<(), String> {
        if depth > GGUF_MAX_ARRAY_DEPTH {
            return Err("tableaux GGUF trop imbriqués".to_string());
        }
        let size = scalar_size(et);
        if size > 0 {
            if n > (self.left / size) as u64 {
                return truncated();
            }
            return self.skip(n as i64 * size);
        }
        match et {
            GGUF_STRING => {
                if n > (self.left / self.count_size()) as u64 {
                    return truncated();
                }
                for _ in 0..n {
                    self.skip_str()?;
                }
                Ok(())
            }
            GGUF_ARRAY => {
                // Chaque sous-tableau coûte au moins son type et son compteur.
                if n > (self.left / (4 + self.count_size())) as u64 {
                    return truncated();
                }
                for _ in 0..n {
                    let sub = self.u32()?;
                    let sn = self.count()?;
                    self.skip_array(sub, sn, depth + 1)?;
                }
                Ok(())
            }
            _ => Err(format!("type GGUF {et} inconnu")),
        }
    }
}

/// Ce qu'on retient d'UNE tranche avant de résoudre les clés selon
/// l'architecture (general.architecture n'est pas forcément la première clé).
#[derive(Debug, Clone, Default, PartialEq)]
pub struct GgufFile {
    pub version: i64,
    pub arch: String,
    pub ints: HashMap<String, i64>,
    /// tableaux d'entiers retenus (têtes KV par couche)
    pub arrays: HashMap<String, Vec<i64>>,
    /// noms de toutes les clés, pour repérer `<arch>.ssm.*`
    pub keys: Vec<String>,
    pub tensors: i64,
    /// noms des tenseurs `*.nextn.eh_proj.weight`
    pub nextn: Vec<String>,
}

/// Les seuls tableaux qu'on lit au lieu de les sauter.
fn keep_int_array(key: &str) -> bool {
    key.ends_with(".attention.head_count_kv")
}

/// Lit l'en-tête, les clés et la table des tenseurs d'un fichier.
fn read_gguf_header(path: &str) -> Result<GgufFile, String> {
    let fh = match File::open(path) {
        Ok(f) => f,
        // Windows refuse d'ouvrir un dossier là où le Go y arrive : même
        // message que le Go dans ce cas.
        Err(e) => {
            if std::fs::metadata(path).is_ok_and(|m| m.is_dir()) {
                return Err(format!("{} est un dossier", base_name(path)));
            }
            return Err(os_err("open", path, &e));
        }
    };
    let st = fh.metadata().map_err(|e| os_err("stat", path, &e))?;
    if st.is_dir() {
        return Err(format!("{} est un dossier", base_name(path)));
    }
    parse_gguf(fh, st.len() as i64).map_err(|e| format!("{} : {e}", base_name(path)))
}

/// Lit `size` octets au plus de `r`. Un panic éventuel (fichier pathologique
/// non prévu par les bornes) est rattrapé en erreur : lire des métadonnées ne
/// doit jamais faire tomber Loki.
pub fn parse_gguf<R: Read>(r: R, size: i64) -> Result<GgufFile, String> {
    match std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| parse_inner(r, size))) {
        Ok(res) => res,
        Err(p) => {
            let msg = p
                .downcast_ref::<&str>()
                .map(|s| s.to_string())
                .or_else(|| p.downcast_ref::<String>().cloned())
                .unwrap_or_default();
            Err(format!("GGUF illisible : {msg}"))
        }
    }
}

fn parse_inner<R: Read>(r: R, size: i64) -> Result<GgufFile, String> {
    let mut g = Reader {
        r: BufReader::with_capacity(1 << 20, r),
        be: false,
        v1: false,
        size,
        left: size,
    };
    let magic = g.fixed(4)?;
    if &magic[..4] != b"GGUF" {
        return Err("pas un fichier GGUF".to_string());
    }
    let mut ver = g.u32()?;
    // Les GGUF gros-boutistes (s390x) gardent la même signature : seule la
    // version lue à l'envers trahit l'ordre des octets.
    if ver & 0xFFFF == 0 {
        let sw = ver.swap_bytes();
        if (1..=3).contains(&sw) {
            g.be = true;
            ver = sw;
        }
    }
    if !(1..=3).contains(&ver) {
        return Err(format!("version GGUF {ver} non prise en charge"));
    }
    g.v1 = ver == 1;
    let n_tensors = g.count()?;
    let n_kv = g.count()?;
    if n_kv > GGUF_MAX_KV || n_tensors > GGUF_MAX_TENSORS {
        return Err(format!(
            "compteurs GGUF aberrants ({n_kv} clés, {n_tensors} tenseurs)"
        ));
    }
    let mut f = GgufFile {
        version: i64::from(ver),
        tensors: n_tensors as i64,
        ..Default::default()
    };

    for _ in 0..n_kv {
        let key = g.str(GGUF_MAX_KEY_LEN)?;
        let t = g.u32()?;
        f.keys.push(key.clone());
        match t {
            GGUF_STRING => {
                if key == "general.architecture" {
                    f.arch = g.str(GGUF_MAX_KEY_LEN)?;
                } else {
                    g.skip_str()?;
                }
            }
            GGUF_ARRAY => {
                let et = g.u32()?;
                let n = g.count()?;
                if keep_int_array(&key)
                    && et != GGUF_FLOAT32
                    && et != GGUF_FLOAT64
                    && scalar_size(et) > 0
                    && n <= GGUF_MAX_LAYER_ARRAY
                {
                    let mut vals = Vec::with_capacity(n.min(g.left.max(0) as u64) as usize);
                    for _ in 0..n {
                        vals.push(g.int_val(et)?.unwrap_or(0));
                    }
                    f.arrays.insert(key, vals);
                } else {
                    g.skip_array(et, n, 1)?;
                }
            }
            _ => {
                if let Some(v) = g.int_val(t)? {
                    f.ints.insert(key, v);
                }
            }
        }
    }

    let dim_size: i64 = if g.v1 { 4 } else { 8 };
    let mut max_off: u64 = 0;
    for _ in 0..n_tensors {
        let name = g.str(GGUF_MAX_TENSOR_NAME)?;
        let nd = g.u32()?;
        if nd > GGUF_MAX_DIMS {
            return Err(format!("tenseur {name:?} à {nd} dimensions"));
        }
        // dimensions et type (u32), puis position des données (u64)
        g.skip(i64::from(nd) * dim_size + 4)?;
        let off = g.u64()?;
        max_off = max_off.max(off);
        if name.ends_with(".nextn.eh_proj.weight") {
            f.nextn.push(name);
        }
    }

    // Les données commencent au prochain multiple de general.alignment. Le
    // début du dernier tenseur doit tenir dans le fichier : un GGUF coupé dans
    // ses poids a un en-tête intact, il faut le voir quand même.
    let mut align: i64 = 32;
    if let Some(&a) = f.ints.get("general.alignment") {
        if a <= 0 || a > 1 << 20 || a & (a - 1) != 0 {
            return Err(format!("alignement GGUF {a} invalide"));
        }
        align = a;
    }
    if n_tensors > 0 {
        let pos = g.size - g.left;
        let data_start = (pos + align - 1) / align * align;
        if max_off >= g.size as u64 || data_start + max_off as i64 >= g.size {
            return truncated();
        }
    }
    Ok(f)
}

/// Ramène une valeur de métadonnée dans [0, i32::MAX] : un champ négatif ou
/// démesuré n'a pas de sens pour un compte de couches ou de têtes.
fn clamp_int(v: i64) -> i64 {
    v.clamp(0, i64::from(i32::MAX))
}

/// Résout les clés de la première tranche selon l'architecture et cherche la
/// tête MTP dans les tenseurs de toutes les tranches (`all` inclut `first`).
pub fn gguf_info_from(first: &GgufFile, all: &[GgufFile]) -> GgufMeta {
    let p = format!("{}.", first.arch);
    let get = |k: &str| clamp_int(first.ints.get(&format!("{p}{k}")).copied().unwrap_or(0));
    let mut m = GgufMeta {
        version: first.version,
        arch: first.arch.clone(),
        block_count: get("block_count"),
        next_n: get("nextn_predict_layers"),
        context_length: get("context_length"),
        expert_count: get("expert_count"),
        key_len: get("attention.key_length"),
        val_len: get("attention.value_length"),
        full_attn_interval: get("full_attention_interval"),
        embedding_length: get("embedding_length"),
        head_count: get("attention.head_count"),
        ssm_conv: get("ssm.conv_kernel"),
        ssm_inner: get("ssm.inner_size"),
        ssm_state: get("ssm.state_size"),
        ssm_groups: get("ssm.group_count"),
        head_count_kv: get("attention.head_count_kv"),
        ..Default::default()
    };
    if let Some(arr) = first.arrays.get(&format!("{p}attention.head_count_kv")) {
        let layers: Vec<i64> = arr.iter().map(|&v| clamp_int(v)).collect();
        m.head_count_kv = layers.iter().copied().max().unwrap_or(0);
        m.head_count_kv_layers = Some(layers);
    }
    if !first.arch.is_empty() {
        let ssm = format!("{p}ssm.");
        m.hybrid = first.keys.iter().any(|k| k.starts_with(&ssm));
    }
    let want = if m.block_count > 0 {
        format!("blk.{}.nextn.eh_proj.weight", m.block_count - 1)
    } else {
        String::new()
    };
    for f in all {
        m.tensor_count += f.tensors;
        if f.nextn.contains(&want) {
            m.has_next_n_tensor = true;
        }
    }
    m
}

// Cache par chemin : la même liste de modèles est consultée à chaque démarrage
// et à chaque affichage. L'empreinte (taille et date de chaque tranche) suffit
// à voir un fichier remplacé ou un téléchargement terminé ; les erreurs ne sont
// pas gardées.
const GGUF_CACHE_MAX: usize = 256;

type GgufCache = Mutex<HashMap<String, (String, GgufMeta)>>;

fn gguf_cache() -> &'static GgufCache {
    static CACHE: OnceLock<GgufCache> = OnceLock::new();
    CACHE.get_or_init(Default::default)
}

fn unix_nanos(t: SystemTime) -> i128 {
    match t.duration_since(UNIX_EPOCH) {
        Ok(d) => d.as_nanos() as i128,
        Err(e) => -(e.duration().as_nanos() as i128),
    }
}

/// `ggufMeta` : métadonnées du modèle désigné par `path`. Pour un modèle en
/// tranches, `path` peut désigner n'importe laquelle : les clés viennent de la
/// première, les tenseurs de toutes. Une tranche manquante est une erreur.
///
/// Entrées/sorties bloquantes : à appeler hors du runtime async.
pub fn gguf_meta(path: &Path) -> Result<GgufMeta, String> {
    let path = go_clean(&path.to_string_lossy());
    let dir = go_dir(&path);
    let fam = shard_family(&path);
    let mut paths = Vec::with_capacity(fam.len());
    let mut stamp = String::new();
    for n in &fam {
        let p = go_join(&[&dir, n]);
        let st = std::fs::metadata(&p).map_err(|e| os_err("stat", &p, &e))?;
        if st.is_dir() {
            return Err(format!("{n} est un dossier"));
        }
        let mt = st.modified().map(unix_nanos).unwrap_or(0);
        let _ = write!(stamp, "{}:{};", st.len(), mt);
        paths.push(p);
    }
    let key = paths[0].clone();

    let cached = gguf_cache()
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .get(&key)
        .cloned();
    if let Some((s, info)) = cached
        && s == stamp
    {
        return Ok(info);
    }

    let files = paths
        .iter()
        .map(|p| read_gguf_header(p))
        .collect::<Result<Vec<_>, _>>()?;
    let info = gguf_info_from(&files[0], &files);

    let mut c = gguf_cache().lock().unwrap_or_else(|e| e.into_inner());
    if c.len() >= GGUF_CACHE_MAX {
        c.clear();
    }
    c.insert(key, (stamp, info.clone()));
    Ok(info)
}

// --- tranches (backend_models_shards.go) -----------------------------------

/// Équivalent de `(?i)-(\d{2,5})-of-(\d{2,5})\.gguf$` : positions (début, fin)
/// du rang et du total.
fn shard_match(name: &str) -> Option<((usize, usize), (usize, usize))> {
    let b = name.as_bytes();
    let n = b.len();
    if n < 5 || !b[n - 5..].eq_ignore_ascii_case(b".gguf") {
        return None;
    }
    let e2 = n - 5;
    let mut s2 = e2;
    while s2 > 0 && b[s2 - 1].is_ascii_digit() {
        s2 -= 1;
    }
    if !(2..=5).contains(&(e2 - s2)) || s2 < 4 || !b[s2 - 4..s2].eq_ignore_ascii_case(b"-of-") {
        return None;
    }
    let e1 = s2 - 4;
    let mut s1 = e1;
    while s1 > 0 && b[s1 - 1].is_ascii_digit() {
        s1 -= 1;
    }
    if !(2..=5).contains(&(e1 - s1)) || s1 < 1 || b[s1 - 1] != b'-' {
        return None;
    }
    Some(((s1, e1), (s2, e2)))
}

/// `shardInfo` : rang (à partir de 1) et nombre total ; `None` pour un modèle
/// d'un seul fichier ou une numérotation incohérente.
pub fn shard_info(name: &str) -> Option<(i64, i64)> {
    let base = base_name(name);
    let ((s1, e1), (s2, e2)) = shard_match(&base)?;
    let idx: i64 = base[s1..e1].parse().ok()?;
    let total: i64 = base[s2..e2].parse().ok()?;
    if idx < 1 || total < 1 || idx > total {
        return None;
    }
    Some((idx, total))
}

/// `shardNameAt` : nom de la tranche de rang `i` dans la même famille, en
/// conservant la largeur de la numérotation d'origine.
fn shard_name_at(name: &str, i: i64) -> String {
    match shard_match(name) {
        Some(((s1, e1), (s2, e2))) => format!(
            "{}{:0width$}-of-{}{}",
            &name[..s1],
            i,
            &name[s2..e2],
            &name[e2..],
            width = e1 - s1
        ),
        None => name.to_string(),
    }
}

/// `isFollowerShard` : tranche 2..N.
pub fn is_follower_shard(name: &str) -> bool {
    shard_info(name).is_some_and(|(idx, _)| idx > 1)
}

/// `shardFamily` : noms de TOUTES les tranches de la famille de `name` (y
/// compris lui), dans l'ordre. Un fichier simple se renvoie lui-même.
pub fn shard_family(name: &str) -> Vec<String> {
    let base = base_name(name);
    match shard_info(&base) {
        None => vec![base],
        Some((_, total)) => (1..=total).map(|i| shard_name_at(&base, i)).collect(),
    }
}

// --- dossiers de modèles (backend_model_dirs.go) ---------------------------

/// `modelsDir()` : `LOKI_HOME/models`.
pub fn models_dir(home: &Path) -> String {
    go_join(&[&home.to_string_lossy(), "models"])
}

/// Séparateur de `$LOKI_MODEL_DIRS` (comme le PATH).
fn model_dirs_list_sep() -> char {
    if cfg!(windows) { ';' } else { ':' }
}

/// Décodage de `state/model_dirs` comme `getJSON` dans un `[]string` :
/// absent, vide ou pas un tableau → rien ; éléments non textuels ignorés (le
/// Go les laisse vides, et `dedup_dirs` écarte les vides).
pub fn parse_stored_model_dirs(raw: Option<&[u8]>) -> Vec<String> {
    match raw.and_then(|b| serde_json::from_slice::<Value>(b).ok()) {
        Some(Value::Array(a)) => a
            .into_iter()
            .filter_map(|v| v.as_str().map(str::to_string))
            .collect(),
        _ => Vec::new(),
    }
}

/// `normDir` : chemin sous forme comparable (séparateurs unifiés en `/`, casse
/// neutralisée sous Windows, slash final retiré).
pub fn norm_dir(p: &str) -> String {
    let p = go_clean(p.trim()).replace('\\', "/");
    if cfg!(windows) { p.to_lowercase() } else { p }
}

/// `dedupDirs` : vides retirés, chemins absolus normalisés, doublons et `skip`
/// écartés.
pub fn dedup_dirs(list: &[String], skip: &str) -> Vec<String> {
    let mut seen = std::collections::HashSet::from([norm_dir(skip)]);
    let mut out = Vec::new();
    for d in list {
        let d = d.trim();
        if d.is_empty() {
            continue;
        }
        let Some(abs) = go_abs(d) else { continue };
        if seen.insert(norm_dir(&abs)) {
            out.push(abs);
        }
    }
    out
}

/// `extraModelDirs` : dossiers déclarés par l'utilisateur (base puis
/// `$LOKI_MODEL_DIRS`), nettoyés et dédoublonnés, models/ exclu.
pub fn extra_model_dirs(home: &Path, stored: &[String], env_dirs: Option<&str>) -> Vec<String> {
    let mut raw = stored.to_vec();
    if let Some(v) = env_dirs.filter(|v| !v.is_empty()) {
        raw.extend(v.split(model_dirs_list_sep()).map(str::to_string));
    }
    dedup_dirs(&raw, &models_dir(home))
}

/// `modelDirs` : models/ d'abord, puis les dossiers ajoutés.
pub fn model_dirs(home: &Path, stored: &[String], env_dirs: Option<&str>) -> Vec<String> {
    let mut v = vec![models_dir(home)];
    v.extend(extra_model_dirs(home, stored, env_dirs));
    v
}

/// `pathWithin` : le fichier `p` est-il dans le dossier `dir` (ou dessous) ?
pub fn path_within(dir: &str, p: &str) -> bool {
    let (d, f) = (norm_dir(dir), norm_dir(p));
    f == d || f.starts_with(&format!("{d}/"))
}

fn trim_model_name(name: &str) -> &str {
    name.trim().trim_matches('"').trim()
}

fn is_file(p: &str) -> bool {
    std::fs::metadata(p).is_ok_and(|m| !m.is_dir())
}

/// `resolveModelPath` : valeur MODEL= (nom de fichier OU chemin absolu) →
/// chemin absolu d'un .gguf. Un chemin absolu n'est accepté que dans un des
/// dossiers déclarés ; un simple nom est cherché dans chaque dossier, models/
/// en premier (introuvable : chemin dans models/, l'appelant décidera).
pub fn resolve_model_path(
    name: &str,
    home: &Path,
    stored_dirs: &[String],
    env_model_dirs: Option<&str>,
) -> Result<PathBuf, String> {
    let s = trim_model_name(name);
    if s.is_empty() {
        return Err("nom de modèle invalide".to_string());
    }
    if !s.to_lowercase().ends_with(".gguf") {
        return Err("le modèle doit être un fichier .gguf".to_string());
    }
    let dirs = model_dirs(home, stored_dirs, env_model_dirs);
    if go_is_abs(s) {
        let abs = go_clean(s);
        if dirs.iter().any(|d| path_within(d, &abs)) {
            return Ok(PathBuf::from(abs));
        }
        return Err(format!(
            "dossier non autorisé : {} — ajoute-le dans « Dossiers de modèles »",
            go_dir(&abs)
        ));
    }
    let base = base_name(s);
    if base.is_empty() || base == "." {
        return Err("nom de modèle invalide".to_string());
    }
    for d in &dirs {
        let p = go_join(&[d, &base]);
        if is_file(&p) {
            return Ok(PathBuf::from(p));
        }
    }
    Ok(PathBuf::from(go_join(&[&models_dir(home), &base])))
}

/// `resolveServeModelPath` : comme [`resolve_model_path`], mais un chemin
/// absolu qui pointe sur un .gguf existant est accepté même hors des dossiers
/// déclarés (lancer le moteur n'est qu'une lecture ; l'allowlist protège les
/// opérations destructrices pilotées depuis l'UI).
///
/// - `stored_dirs` : [`parse_stored_model_dirs`] de `state/model_dirs` ;
/// - `env_model_dirs` : `$LOKI_MODEL_DIRS`.
pub fn resolve_serve_model_path(
    model: &str,
    home: &Path,
    stored_dirs: &[String],
    env_model_dirs: Option<&str>,
) -> Result<PathBuf, String> {
    let err = match resolve_model_path(model, home, stored_dirs, env_model_dirs) {
        Ok(p) => return Ok(p),
        Err(e) => e,
    };
    let s = trim_model_name(model);
    if go_is_abs(s) && s.to_lowercase().ends_with(".gguf") {
        let abs = go_clean(s);
        if is_file(&abs) {
            return Ok(PathBuf::from(abs));
        }
    }
    Err(err)
}

/// Corps JSON de `GET /api/model/layers` (`handleModelLayers`, toujours en
/// 200) : nombre de blocs du modèle ; llama.cpp en offloade `block_count + 1`
/// (la couche de sortie en plus), d'où `ngl_max`.
///
/// - `query_model` : paramètre `?model=` ; s'il est vide après nettoyage des
///   espaces, `config_model` (clé `MODEL` du bucket `config`) prend le relais ;
/// - `stored_dirs` / `env_model_dirs` : voir [`resolve_serve_model_path`].
///
/// Entrées/sorties bloquantes : à appeler hors du runtime async.
pub fn model_layers_json(
    query_model: Option<&str>,
    config_model: Option<&str>,
    home: &Path,
    stored_dirs: &[String],
    env_model_dirs: Option<&str>,
) -> Value {
    let mut model = query_model.unwrap_or("").trim();
    if model.is_empty() {
        model = config_model.unwrap_or("").trim();
    }
    if model.is_empty() {
        return json!({"ok": false, "error": "aucun modèle"});
    }
    let path = match resolve_serve_model_path(model, home, stored_dirs, env_model_dirs) {
        Ok(p) => p,
        Err(e) => return json!({"ok": false, "error": e}),
    };
    match gguf_meta(&path) {
        Ok(info) if info.block_count > 0 => json!({
            "ok": true,
            "layers": info.block_count,
            "ngl_max": info.block_count + 1,
        }),
        Ok(_) => json!({"ok": false, "error": "nombre de couches absent de l'en-tête"}),
        Err(e) => json!({"ok": false, "error": e}),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs::FileTimes;
    use std::time::Duration;

    /// Écrit un GGUF synthétique : en-tête, clés typées, table des tenseurs,
    /// sans aucune donnée de poids (le lecteur n'en lit jamais).
    struct Builder {
        be: bool,
        version: u32,
        n_kv: usize,
        n_t: usize,
        kv: Vec<u8>,
        tensors: Vec<u8>,
    }

    fn new_gguf() -> Builder {
        Builder {
            be: false,
            version: 3,
            n_kv: 0,
            n_t: 0,
            kv: Vec::new(),
            tensors: Vec::new(),
        }
    }

    fn put_u32(be: bool, w: &mut Vec<u8>, v: u32) {
        w.extend_from_slice(&if be { v.to_be_bytes() } else { v.to_le_bytes() });
    }

    fn put_u64(be: bool, w: &mut Vec<u8>, v: u64) {
        w.extend_from_slice(&if be { v.to_be_bytes() } else { v.to_le_bytes() });
    }

    impl Builder {
        fn cnt(&self, w: &mut Vec<u8>, n: u64) {
            if self.version == 1 {
                put_u32(self.be, w, n as u32);
            } else {
                put_u64(self.be, w, n);
            }
        }

        fn str_to(&self, w: &mut Vec<u8>, s: &str) {
            self.cnt(w, s.len() as u64);
            w.extend_from_slice(s.as_bytes());
        }

        fn key(&mut self, k: &str, t: u32) {
            self.n_kv += 1;
            let mut kv = std::mem::take(&mut self.kv);
            self.str_to(&mut kv, k);
            put_u32(self.be, &mut kv, t);
            self.kv = kv;
        }

        fn u32(mut self, k: &str, v: u32) -> Self {
            self.key(k, GGUF_UINT32);
            put_u32(self.be, &mut self.kv, v);
            self
        }

        fn i64(mut self, k: &str, v: i64) -> Self {
            self.key(k, GGUF_INT64);
            put_u64(self.be, &mut self.kv, v as u64);
            self
        }

        fn f32(mut self, k: &str, v: f32) -> Self {
            self.key(k, GGUF_FLOAT32);
            put_u32(self.be, &mut self.kv, v.to_bits());
            self
        }

        fn boolean(mut self, k: &str, v: bool) -> Self {
            self.key(k, GGUF_BOOL);
            self.kv.push(u8::from(v));
            self
        }

        fn s(mut self, k: &str, v: &str) -> Self {
            self.key(k, GGUF_STRING);
            let mut kv = std::mem::take(&mut self.kv);
            self.str_to(&mut kv, v);
            self.kv = kv;
            self
        }

        fn str_arr(mut self, k: &str, vs: &[&str]) -> Self {
            self.key(k, GGUF_ARRAY);
            let mut kv = std::mem::take(&mut self.kv);
            put_u32(self.be, &mut kv, GGUF_STRING);
            self.cnt(&mut kv, vs.len() as u64);
            for v in vs {
                self.str_to(&mut kv, v);
            }
            self.kv = kv;
            self
        }

        fn i32_arr(mut self, k: &str, vs: &[i32]) -> Self {
            self.key(k, GGUF_ARRAY);
            let mut kv = std::mem::take(&mut self.kv);
            put_u32(self.be, &mut kv, GGUF_INT32);
            self.cnt(&mut kv, vs.len() as u64);
            for &v in vs {
                put_u32(self.be, &mut kv, v as u32);
            }
            self.kv = kv;
            self
        }

        /// Tableau de deux tableaux de u8, pour vérifier le saut récursif.
        fn nested(mut self, k: &str) -> Self {
            self.key(k, GGUF_ARRAY);
            let mut kv = std::mem::take(&mut self.kv);
            put_u32(self.be, &mut kv, GGUF_ARRAY);
            self.cnt(&mut kv, 2);
            for _ in 0..2 {
                put_u32(self.be, &mut kv, GGUF_UINT8);
                self.cnt(&mut kv, 3);
                kv.extend_from_slice(&[1, 2, 3]);
            }
            self.kv = kv;
            self
        }

        fn tensor(mut self, name: &str, dims: &[u64]) -> Self {
            let mut t = std::mem::take(&mut self.tensors);
            self.str_to(&mut t, name);
            put_u32(self.be, &mut t, dims.len() as u32);
            for &d in dims {
                if self.version == 1 {
                    put_u32(self.be, &mut t, d as u32);
                } else {
                    put_u64(self.be, &mut t, d);
                }
            }
            put_u32(self.be, &mut t, 0); // type F32
            put_u64(self.be, &mut t, (self.n_t * 32) as u64); // position des données
            self.tensors = t;
            self.n_t += 1;
            self
        }

        fn bytes(&self) -> Vec<u8> {
            let mut out = b"GGUF".to_vec();
            put_u32(self.be, &mut out, self.version);
            self.cnt(&mut out, self.n_t as u64);
            self.cnt(&mut out, self.n_kv as u64);
            out.extend_from_slice(&self.kv);
            out.extend_from_slice(&self.tensors);
            // bourrage jusqu'à l'alignement par défaut, puis 32 octets de
            // « poids » par tenseur
            if self.n_t > 0 {
                let pad = (32 - out.len() % 32) % 32 + 32 * self.n_t;
                out.resize(out.len() + pad, 0);
            }
            out
        }
    }

    fn write_gguf(dir: &Path, name: &str, data: &[u8]) -> PathBuf {
        let p = dir.join(name);
        std::fs::write(&p, data).unwrap();
        p
    }

    /// Modèle hybride à 4 couches, têtes KV par couche (0 sur les couches
    /// récurrentes), tête MTP sur la dernière couche, vocabulaire à sauter.
    fn qwen_mtp(b: Builder) -> Builder {
        let long = "c".repeat(300);
        b.u32("general.alignment", 32)
            .str_arr("tokenizer.ggml.tokens", &["<s>", "a", "bb", &long])
            .s("general.architecture", "qwen35")
            .s("general.name", "Qwen synthétique")
            .u32("qwen35.block_count", 4)
            .u32("qwen35.nextn_predict_layers", 1)
            .i64("qwen35.context_length", 262144)
            .u32("qwen35.attention.key_length", 256)
            .u32("qwen35.attention.value_length", 256)
            .i32_arr("qwen35.attention.head_count_kv", &[0, 0, 0, 4])
            .u32("qwen35.full_attention_interval", 4)
            .u32("qwen35.ssm.conv_kernel", 4)
            .f32("qwen35.rope.freq_base", 1e7)
            .boolean("tokenizer.ggml.add_bos_token", false)
            .nested("bidon.imbrique")
            .tensor("token_embd.weight", &[1024, 32000])
            .tensor("blk.0.attn_norm.weight", &[1024])
            .tensor("blk.3.nextn.eh_proj.weight", &[2048, 1024])
    }

    #[test]
    fn gguf_meta_qwen_hybride_mtp() {
        let dir = tempfile::tempdir().unwrap();
        let p = write_gguf(dir.path(), "qwen.gguf", &qwen_mtp(new_gguf()).bytes());
        let got = gguf_meta(&p).unwrap();
        let want = GgufMeta {
            version: 3,
            arch: "qwen35".into(),
            block_count: 4,
            next_n: 1,
            context_length: 262144,
            key_len: 256,
            val_len: 256,
            head_count_kv: 4,
            head_count_kv_layers: Some(vec![0, 0, 0, 4]),
            full_attn_interval: 4,
            hybrid: true,
            ssm_conv: 4,
            has_next_n_tensor: true,
            tensor_count: 3,
            ..Default::default()
        };
        assert_eq!(got, want);
    }

    /// La clé nextn_predict_layers ne prouve rien : seul le tenseur de la
    /// DERNIÈRE couche compte, comme dans llama.cpp.
    #[test]
    fn gguf_meta_tete_mtp_absente_ou_mal_placee() {
        let dir = tempfile::tempdir().unwrap();
        let base = || {
            new_gguf()
                .s("general.architecture", "glm4moe")
                .u32("glm4moe.block_count", 47)
                .u32("glm4moe.nextn_predict_layers", 1)
                .u32("glm4moe.expert_count", 128)
                .u32("glm4moe.attention.head_count_kv", 8)
        };
        let cases = [
            (
                "sans-tenseur.gguf",
                base().tensor("blk.45.ffn_up.weight", &[8, 8]),
            ),
            (
                "mauvaise-couche.gguf",
                base().tensor("blk.45.nextn.eh_proj.weight", &[8, 8]),
            ),
        ];
        for (name, b) in cases {
            let got = gguf_meta(&write_gguf(dir.path(), name, &b.bytes()))
                .unwrap_or_else(|e| panic!("{name} : {e}"));
            assert!(
                !(got.has_next_n_tensor
                    || got.next_n != 1
                    || got.expert_count != 128
                    || got.head_count_kv != 8
                    || got.head_count_kv_layers.is_some()
                    || got.hybrid),
                "{name} : {got:?}"
            );
        }
        let got = gguf_meta(&write_gguf(
            dir.path(),
            "ok.gguf",
            &base()
                .tensor("blk.46.nextn.eh_proj.weight", &[8, 8])
                .bytes(),
        ));
        assert!(
            got.as_ref().is_ok_and(|g| g.has_next_n_tensor),
            "tête MTP en dernière couche non vue : {got:?}"
        );
    }

    /// Modèle en tranches : les clés sont dans la première, la tête MTP dans
    /// la dernière. N'importe quelle tranche peut être désignée.
    #[test]
    fn gguf_meta_tranches() {
        let dir = tempfile::tempdir().unwrap();
        let d = dir.path();
        let first = || {
            new_gguf()
                .s("general.architecture", "qwen35")
                .u32("qwen35.block_count", 4)
                .u32("split.count", 2)
                .tensor("token_embd.weight", &[8, 8])
        };
        let second = new_gguf()
            .u32("split.no", 1)
            .u32("split.count", 2)
            .tensor("blk.3.nextn.eh_proj.weight", &[8, 8])
            .tensor("output.weight", &[8, 8]);
        let p1 = write_gguf(d, "m-00001-of-00002.gguf", &first().bytes());
        write_gguf(d, "m-00002-of-00002.gguf", &second.bytes());
        for p in [p1, d.join("m-00002-of-00002.gguf")] {
            let got = gguf_meta(&p).unwrap();
            assert!(
                got.arch == "qwen35" && got.has_next_n_tensor && got.tensor_count == 3,
                "{} : {got:?}",
                base_name(&p.to_string_lossy())
            );
        }
        // tranche manquante : on ne sait pas, donc erreur
        write_gguf(d, "n-00001-of-00002.gguf", &first().bytes());
        assert!(
            gguf_meta(&d.join("n-00001-of-00002.gguf")).is_err(),
            "tranche manquante acceptée"
        );
        // gguf-split --no-tensor-first-split : la première tranche ne porte
        // que les clés, aucun tenseur ni aucune donnée. Elle doit passer.
        let meta = new_gguf()
            .s("general.architecture", "glm4moe")
            .u32("glm4moe.block_count", 47)
            .u32("split.count", 2);
        let rest = new_gguf()
            .u32("split.no", 1)
            .tensor("blk.46.nextn.eh_proj.weight", &[8, 8]);
        write_gguf(d, "s-00001-of-00002.gguf", &meta.bytes());
        write_gguf(d, "s-00002-of-00002.gguf", &rest.bytes());
        let got = gguf_meta(&d.join("s-00001-of-00002.gguf"));
        assert!(
            got.as_ref()
                .is_ok_and(|g| g.arch == "glm4moe" && g.has_next_n_tensor && g.tensor_count == 1),
            "première tranche sans tenseur : {got:?}"
        );
    }

    /// v1 (compteurs 32 bits) et gros-boutiste : même résultat que le v3.
    #[test]
    fn gguf_meta_versions_et_ordre() {
        let dir = tempfile::tempdir().unwrap();
        let mut v1 = new_gguf();
        v1.version = 1;
        let mut v2 = new_gguf();
        v2.version = 2;
        let mut be = new_gguf();
        be.be = true;
        for (name, b) in [("v1.gguf", v1), ("v2.gguf", v2), ("be.gguf", be)] {
            let version = i64::from(b.version);
            let got = gguf_meta(&write_gguf(dir.path(), name, &qwen_mtp(b).bytes()))
                .unwrap_or_else(|e| panic!("{name} : {e}"));
            assert!(
                got.arch == "qwen35"
                    && got.block_count == 4
                    && got.has_next_n_tensor
                    && got.head_count_kv_layers == Some(vec![0, 0, 0, 4])
                    && got.version == version,
                "{name} : {got:?}"
            );
        }
    }

    /// Fichier en cours de téléchargement : chaque coupure donne une erreur,
    /// jamais un panic ni un faux résultat.
    #[test]
    fn gguf_meta_tronque() {
        let dir = tempfile::tempdir().unwrap();
        let full = qwen_mtp(new_gguf()).bytes();
        // Toutes les coupures jusqu'au premier octet du dernier tenseur.
        for cut in 0..=full.len() - 32 {
            assert!(
                parse_gguf(&full[..cut], cut as i64).is_err(),
                "coupure à {cut}/{} acceptée",
                full.len()
            );
        }
        if let Err(e) = parse_gguf(&full[..], full.len() as i64) {
            panic!("fichier complet refusé : {e}");
        }
        // et par le chemin fichier, pour une coupure dans l'en-tête
        let p = write_gguf(dir.path(), "cut.gguf", &full[..full.len() / 3]);
        assert!(gguf_meta(&p).is_err(), "fichier coupé accepté");
    }

    #[test]
    fn gguf_meta_fichiers_aberrants() {
        let dir = tempfile::tempdir().unwrap();
        let hdr = |ver: u32, n_t: u64, n_kv: u64| {
            let mut w = b"GGUF".to_vec();
            put_u32(false, &mut w, ver);
            put_u64(false, &mut w, n_t);
            put_u64(false, &mut w, n_kv);
            w
        };
        // clé annoncée à 1 Eo
        let mut huge_key = hdr(3, 0, 1);
        put_u64(false, &mut huge_key, 1 << 60);
        // tableau de chaînes annoncé à 2^62 éléments
        let mut huge_arr = hdr(3, 0, 1);
        put_u64(false, &mut huge_arr, 1);
        huge_arr.push(b'k');
        put_u32(false, &mut huge_arr, GGUF_ARRAY);
        put_u32(false, &mut huge_arr, GGUF_STRING);
        put_u64(false, &mut huge_arr, 1 << 62);
        // type de valeur inconnu
        let mut bad_type = hdr(3, 0, 1);
        put_u64(false, &mut bad_type, 1);
        bad_type.push(b'k');
        put_u32(false, &mut bad_type, 99);
        put_u64(false, &mut bad_type, 0);
        // tenseur à 1000 dimensions
        let mut bad_dims = hdr(3, 1, 0);
        put_u64(false, &mut bad_dims, 1);
        bad_dims.push(b't');
        put_u32(false, &mut bad_dims, 1000);
        bad_dims.extend_from_slice(&[0u8; 64]);

        let mut signature = b"GGML\x03\x00\x00\x00".to_vec();
        signature.extend_from_slice(&[0u8; 16]);
        let cases: Vec<(&str, Vec<u8>)> = vec![
            ("vide", Vec::new()),
            (
                "alignement",
                new_gguf()
                    .u32("general.alignment", 24)
                    .tensor("t", &[1])
                    .bytes(),
            ),
            ("signature", signature),
            ("version", hdr(4, 0, 0)),
            ("compteurs", hdr(3, 1 << 40, 1 << 40)),
            ("cle-geante", huge_key),
            ("tableau", huge_arr),
            ("type-inconnu", bad_type),
            ("dimensions", bad_dims),
        ];
        for (name, data) in cases {
            let p = write_gguf(dir.path(), &format!("{name}.gguf"), &data);
            assert!(gguf_meta(&p).is_err(), "{name} : accepté");
        }
        assert!(
            gguf_meta(&dir.path().join("absent.gguf")).is_err(),
            "fichier absent accepté"
        );
    }

    /// Octet corrompu n'importe où dans l'en-tête (v1 et v3) : le lecteur doit
    /// rendre la main vite, sans panic remonté ni allocation démesurée.
    #[test]
    fn gguf_meta_octet_corrompu() {
        let mut v1 = new_gguf();
        v1.version = 1;
        for b in [new_gguf(), v1] {
            let version = b.version;
            let full = qwen_mtp(b).bytes();
            let hdr = full.len() - 3 * 32; // bourrage et « poids » jamais lus
            for i in 0..hdr {
                for v in [0xFFu8, 0x7F, 0x00] {
                    let mut m = full.clone();
                    m[i] = v;
                    // le filet de sécurité existe, mais aucune entrée ne doit
                    // y tomber
                    if let Err(e) = parse_gguf(&m[..], m.len() as i64) {
                        assert!(
                            !e.contains("illisible"),
                            "v{version}, octet {i} = {v:#x} : {e}"
                        );
                    }
                }
            }
        }
    }

    fn set_mtime(p: &Path, t: SystemTime) {
        let f = File::options().write(true).open(p).unwrap();
        f.set_times(FileTimes::new().set_accessed(t).set_modified(t))
            .unwrap();
    }

    /// Le cache tient tant que taille et date sont les mêmes, et se renouvelle
    /// dès qu'elles changent.
    #[test]
    fn gguf_meta_cache() {
        let dir = tempfile::tempdir().unwrap();
        let data = qwen_mtp(new_gguf()).bytes();
        let p = write_gguf(dir.path(), "c.gguf", &data);
        // 2026-01-02T03:04:05Z
        let stamp = UNIX_EPOCH + Duration::from_secs(1_767_323_045);
        set_mtime(&p, stamp);
        let mut a = gguf_meta(&p).unwrap();
        a.head_count_kv_layers.as_mut().unwrap()[0] = 99; // ne doit pas atteindre le cache

        // même taille, même date, contenu illisible : le cache répond
        write_gguf(dir.path(), "c.gguf", &vec![0u8; data.len()]);
        set_mtime(&p, stamp);
        let b = gguf_meta(&p);
        assert!(
            b.as_ref().is_ok_and(
                |b| b.head_count_kv_layers.as_ref().unwrap()[0] == 0 && b.has_next_n_tensor
            ),
            "cache non utilisé ou altéré : {b:?}"
        );

        // date différente : relecture, donc erreur sur le contenu illisible
        set_mtime(&p, stamp + Duration::from_secs(1));
        assert!(
            gguf_meta(&p).is_err(),
            "fichier modifié servi depuis le cache"
        );
    }

    // --- tranches (backend_models_shards_test.go) ---

    #[test]
    fn shard_info_cas() {
        let cases = [
            ("Laguna-S-2.1-UD-IQ4_XS-00001-of-00003.gguf", Some((1, 3))),
            ("Laguna-S-2.1-UD-IQ4_XS-00002-of-00003.gguf", Some((2, 3))),
            ("m-01-of-03.gguf", Some((1, 3))),
            ("Qwen2.5-7B-Instruct-Q4_K_M.gguf", None),
            ("bidon-00004-of-00003.gguf", None), // numérotation incohérente
            ("pas-un-gguf-00001-of-00002.bin", None),
        ];
        for (name, want) in cases {
            assert_eq!(shard_info(name), want, "shard_info({name:?})");
        }
    }

    #[test]
    fn shard_family_garde_la_largeur() {
        assert_eq!(
            shard_family("Laguna-00002-of-00003.gguf"),
            [
                "Laguna-00001-of-00003.gguf",
                "Laguna-00002-of-00003.gguf",
                "Laguna-00003-of-00003.gguf",
            ]
        );
        assert_eq!(shard_family("simple.gguf"), ["simple.gguf"]);
        assert!(is_follower_shard("x-02-OF-03.GGUF"));
        assert!(!is_follower_shard("x-01-of-03.gguf"));
        assert!(shard_info("x-000001-of-00003.gguf").is_none());
    }

    // --- résolution du modèle (backend_model_dirs_test.go) ---

    /// Un modèle posé hors de models/ doit être utilisable dès que son
    /// dossier est déclaré — et refusé tant qu'il ne l'est pas.
    #[test]
    fn resolve_model_path_extra_dir() {
        let home = tempfile::tempdir().unwrap();
        let ext = tempfile::tempdir().unwrap();
        let ext_s = ext.path().to_string_lossy().into_owned();
        let ext_model = go_join(&[&ext_s, "gros.gguf"]);
        std::fs::write(&ext_model, b"x").unwrap();

        assert!(
            resolve_model_path(&ext_model, home.path(), &[], Some("")).is_err(),
            "un dossier non déclaré devrait être refusé"
        );
        // … sauf pour LANCER le moteur : le fichier existe
        assert_eq!(
            resolve_serve_model_path(&ext_model, home.path(), &[], None),
            Ok(PathBuf::from(&ext_model))
        );

        let stored =
            parse_stored_model_dirs(Some(serde_json::to_string(&[&ext_s]).unwrap().as_bytes()));
        let got = resolve_model_path(&ext_model, home.path(), &stored, Some(""));
        assert_eq!(got, Ok(PathBuf::from(go_clean(&ext_model))));
        // Le simple nom de fichier doit aussi être retrouvé dans le dossier ajouté.
        let got = resolve_model_path("gros.gguf", home.path(), &stored, Some(""));
        assert_eq!(got, Ok(PathBuf::from(&ext_model)));
    }

    /// models/ garde la priorité et les non-.gguf restent refusés.
    #[test]
    fn resolve_model_path_home_first() {
        let home = tempfile::tempdir().unwrap();
        let ext = tempfile::tempdir().unwrap();
        let ext_s = ext.path().to_string_lossy().into_owned();
        let models = models_dir(home.path());
        std::fs::create_dir_all(&models).unwrap();
        for d in [&models, &ext_s] {
            std::fs::write(go_join(&[d, "m.gguf"]), b"x").unwrap();
        }
        let got = resolve_model_path("m.gguf", home.path(), &[], Some(&ext_s));
        assert_eq!(got, Ok(PathBuf::from(go_join(&[&models, "m.gguf"]))));
        assert!(resolve_model_path("/etc/passwd", home.path(), &[], Some(&ext_s)).is_err());
        assert!(resolve_serve_model_path("/etc/passwd", home.path(), &[], None).is_err());
    }

    #[test]
    fn dossiers_stockes() {
        assert!(parse_stored_model_dirs(None).is_empty());
        assert!(parse_stored_model_dirs(Some(b"")).is_empty());
        assert!(parse_stored_model_dirs(Some(b"{}")).is_empty());
        assert_eq!(
            parse_stored_model_dirs(Some(br#"["/a", 1, "/b"]"#)),
            ["/a", "/b"]
        );
        let home = Path::new(if cfg!(windows) { r"C:\h" } else { "/h" });
        let models = models_dir(home);
        let extra = extra_model_dirs(
            home,
            &[models.clone(), " ".into(), "/x/".into(), "/x".into()],
            None,
        );
        assert_eq!(extra, [go_abs("/x").unwrap()]);
        assert!(path_within(&models, &go_join(&[&models, "sous", "m.gguf"])));
        assert!(!path_within(&models, &format!("{models}2/m.gguf")));
    }

    #[test]
    fn couches_du_modele() {
        let home = tempfile::tempdir().unwrap();
        let models = models_dir(home.path());
        std::fs::create_dir_all(&models).unwrap();
        write_gguf(Path::new(&models), "q.gguf", &qwen_mtp(new_gguf()).bytes());
        write_gguf(
            Path::new(&models),
            "sans.gguf",
            &new_gguf().s("general.architecture", "x").bytes(),
        );
        let h = home.path();
        assert_eq!(
            model_layers_json(Some(" "), Some(" q.gguf "), h, &[], None),
            json!({"ok": true, "layers": 4, "ngl_max": 5})
        );
        assert_eq!(
            model_layers_json(Some("q.gguf"), None, h, &[], None),
            json!({"ok": true, "layers": 4, "ngl_max": 5})
        );
        assert_eq!(
            model_layers_json(None, Some(""), h, &[], None),
            json!({"ok": false, "error": "aucun modèle"})
        );
        assert_eq!(
            model_layers_json(Some("x.bin"), None, h, &[], None),
            json!({"ok": false, "error": "le modèle doit être un fichier .gguf"})
        );
        assert_eq!(
            model_layers_json(Some("sans.gguf"), None, h, &[], None),
            json!({"ok": false, "error": "nombre de couches absent de l'en-tête"})
        );
        let absent = model_layers_json(Some("absent.gguf"), None, h, &[], None);
        assert_eq!(absent["ok"], false);
        assert!(
            absent["error"].as_str().unwrap().starts_with("stat "),
            "{absent}"
        );
    }
}
