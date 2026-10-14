//! Lot 4 : liste des presets, en LECTURE seule (portage de
//! `internal/loki/backend_presets.go`).
//!
//! Sert `/api/status` (`activePresetID()` de `web_sysprompt.go`) et, plus
//! tard, `GET /api/presets`. L'écriture (création, bascule, suppression…)
//! reste au Go.
//!
//! Différences de forme avec le Go, toutes dues au fait que le Go lit lui-même
//! son environnement et sa base :
//! - `LOKI_HOME` est passé en paramètre (`home`) au lieu d'être lu ici ;
//! - la configuration active (`ReadConfig()`, seau `config`) est passée en
//!   paramètre (`cfg`) : l'appelant la lit par `Store::config()` ;
//! - l'ordre choisi par glisser-déposer (seau `state`, clé `preset_order`,
//!   un tableau JSON d'ids) est passé en paramètre (`order`) : l'appelant lit
//!   la valeur brute par `Store::get(BK_STATE, PRESET_ORDER_KEY)` et la
//!   décode par `parse_preset_order`, qui reproduit `getJSON`.

use serde::{Serialize, Serializer};
use std::collections::HashMap;
use std::path::{Path, PathBuf};

/// Clé de l'ordre d'affichage des presets dans le seau `state`
/// (`crate::store::BK_STATE`) — `presetOrderKey` du Go.
pub const PRESET_ORDER_KEY: &str = "preset_order";

/// Réglages « appareil » (`preservedKeys` du Go) : préférences de machine,
/// pas paramètres de modèle. Réappliqués par la bascule de preset, donc
/// ignorés par l'empreinte — sans quoi aucun preset ne serait jamais actif.
pub const PRESERVED_KEYS: &[&str] = &[
    "MEM_MODE",
    "CRAWL4AI_URL",
    "WEB_ENGINE",
    "CUDA_VISIBLE_DEVICES",
    "HOST",
    "CUDA_LAUNCH_QUEUES",
    "SLOT_PERSIST",
];

/// Un preset. Son IDENTITÉ est le nom de fichier (sans `.env`), toujours
/// unique ; son nom d'AFFICHAGE vient d'une ligne `# NAME=` facultative.
///
/// Sérialisation identique au `Preset` du Go, qui n'a pas d'étiquettes
/// JSON : noms de champs Go (`ID`, `Name`, `Path`, `Active`), aucun omis.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct Preset {
    /// Nom de fichier sans `.env` — identité stable et unique.
    #[serde(rename = "ID")]
    pub id: String,
    /// Nom d'affichage (ligne `# NAME=`, à défaut l'id).
    #[serde(rename = "Name")]
    pub name: String,
    #[serde(rename = "Path", serialize_with = "path_lossy")]
    pub path: PathBuf,
    /// Le preset correspond-il à la configuration active ?
    #[serde(rename = "Active")]
    pub active: bool,
}

fn path_lossy<S: Serializer>(p: &Path, s: S) -> Result<S::Ok, S::Error> {
    s.serialize_str(&p.to_string_lossy())
}

/// `presetsDir()` du Go : `$LOKI_HOME/presets`.
pub fn presets_dir(home: &Path) -> PathBuf {
    home.join("presets")
}

/// `unquoteValue` du Go : retire les guillemets ENTOURANTS d'une valeur, et
/// seulement eux (une paire ouvrante/fermante du même caractère).
pub fn unquote_value(v: &str) -> &str {
    let b = v.as_bytes();
    if b.len() >= 2 {
        let q = b[0];
        if (q == b'"' || q == b'\'') && b[b.len() - 1] == q {
            return &v[1..v.len() - 1];
        }
    }
    v
}

/// `parseEnv` du Go : lit le format clé=valeur des presets. Lignes vides et
/// commentaires ignorés, préfixe `export ` toléré, guillemets entourants
/// retirés ; une clé répétée garde sa dernière valeur.
pub fn parse_env(text: &str) -> HashMap<String, String> {
    let mut m = HashMap::new();
    for line in text.split('\n') {
        let s = line.trim();
        if s.is_empty() || s.starts_with('#') {
            continue;
        }
        let s = s.strip_prefix("export ").unwrap_or(s);
        let Some((k, v)) = s.split_once('=') else {
            continue;
        };
        m.insert(k.trim().to_string(), unquote_value(v.trim()).to_string());
    }
    m
}

/// `presetDisplayName` du Go : la valeur de la ligne `# NAME=` (le `#` est
/// facultatif, la clé insensible à la casse), sinon `fallback` (l'id).
pub fn preset_display_name(content: &str, fallback: &str) -> String {
    for line in content.split('\n') {
        let t = line.trim();
        let s = t.strip_prefix('#').unwrap_or(t).trim();
        if let Some((k, v)) = s.split_once('=') {
            // EqualFold du Go : « NAME » n'a pas d'équivalent Unicode hors ASCII.
            if k.trim().eq_ignore_ascii_case("NAME") {
                let v = unquote_value(v.trim());
                if !v.is_empty() {
                    return v.to_string();
                }
            }
        }
    }
    fallback.to_string()
}

/// `isPreservedKey` du Go.
pub fn is_preserved_key(key: &str) -> bool {
    PRESERVED_KEYS.contains(&key)
}

/// `configFingerprint` du Go : l'ENSEMBLE TRIÉ des affectations `K=V`
/// effectives (clés « appareil » exclues), joint par `\n` puis haché en
/// SHA-1 hexadécimal. Même valeur, octet pour octet, que le Go : elle est
/// aussi stockée ailleurs (réglage auto, résultats de tune).
pub fn config_fingerprint(m: &HashMap<String, String>) -> String {
    let mut pairs: Vec<String> = m
        .iter()
        .filter(|(k, _)| !is_preserved_key(k))
        .map(|(k, v)| format!("{k}={v}"))
        .collect();
    // sort.Strings : ordre des octets, comme Ord sur String.
    pairs.sort();
    hex::encode(sha1(pairs.join("\n").as_bytes()))
}

/// `presetFingerprint` du Go : `config_fingerprint` du contenu d'un preset.
pub fn preset_fingerprint(content: &str) -> String {
    config_fingerprint(&parse_env(content))
}

/// Décode la valeur brute de `state/preset_order` comme `getJSON` du Go dans
/// un `[]string` : absente, vide ou illisible → aucun ordre ; un élément qui
/// n'est pas une chaîne devient `""` (le Go poursuit le décodage sur une
/// erreur de type et laisse la valeur zéro).
pub fn parse_preset_order(raw: Option<&[u8]>) -> Vec<String> {
    let Some(raw) = raw.filter(|r| !r.is_empty()) else {
        return Vec::new();
    };
    match serde_json::from_slice::<serde_json::Value>(raw) {
        Ok(serde_json::Value::Array(items)) => items
            .into_iter()
            .map(|v| match v {
                serde_json::Value::String(s) => s,
                _ => String::new(),
            })
            .collect(),
        _ => Vec::new(),
    }
}

/// `applyPresetOrder` du Go : réordonne `list` (déjà triée par nom) selon
/// l'ordre stocké — d'abord les presets classés, dans l'ordre choisi, puis
/// les non classés (fraîchement créés) dans leur ordre alphabétique. Tri
/// STABLE. Un id répété dans `order` prend sa DERNIÈRE position (map Go).
pub fn apply_preset_order(mut list: Vec<Preset>, order: &[String]) -> Vec<Preset> {
    if order.is_empty() {
        return list;
    }
    let pos: HashMap<&str, usize> = order
        .iter()
        .enumerate()
        .map(|(i, id)| (id.as_str(), i))
        .collect();
    list.sort_by(|a, b| {
        match (pos.get(a.id.as_str()), pos.get(b.id.as_str())) {
            (Some(pa), Some(pb)) => pa.cmp(pb),
            // Un preset classé passe avant un non classé.
            (Some(_), None) => std::cmp::Ordering::Less,
            (None, Some(_)) => std::cmp::Ordering::Greater,
            // Deux non classés : l'ordre alphabétique reste.
            (None, None) => std::cmp::Ordering::Equal,
        }
    });
    list
}

/// `ListPresets()` du Go : tous les `presets/*.env`, en marquant ceux qui
/// correspondent à la configuration active `cfg` (clés « appareil »
/// ignorées), triés par nom puis selon l'ordre stocké `order`.
///
/// Comme le Go, crée le dossier des presets s'il manque (erreur ignorée) ;
/// un fichier illisible est sauté ; seul un dossier illisible est une erreur.
pub fn list_presets(
    home: &Path,
    cfg: &HashMap<String, String>,
    order: &[String],
) -> Result<Vec<Preset>, String> {
    let dir = presets_dir(home);
    let _ = std::fs::create_dir_all(&dir);
    let cur = config_fingerprint(cfg);
    let rd = std::fs::read_dir(&dir).map_err(|e| format!("open {}: {e}", dir.display()))?;
    // os.ReadDir renvoie les entrées triées par nom de fichier ; read_dir non.
    let mut entries: Vec<_> = rd.filter_map(Result::ok).collect();
    entries.sort_by_key(|e| e.file_name());
    let mut out = Vec::new();
    for e in entries {
        // e.IsDir() du Go ne suit pas les liens symboliques : file_type non plus.
        if e.file_type().map(|t| t.is_dir()).unwrap_or(false) {
            continue;
        }
        let fname = e.file_name().to_string_lossy().into_owned();
        let Some(id) = fname.strip_suffix(".env") else {
            continue;
        };
        let path = dir.join(e.file_name());
        let Ok(bytes) = std::fs::read(&path) else {
            continue;
        };
        let content = String::from_utf8_lossy(&bytes);
        out.push(Preset {
            id: id.to_string(),
            name: preset_display_name(&content, id),
            active: preset_fingerprint(&content) == cur,
            path,
        });
    }
    // sort.Slice par Name (octets). Le tri Rust est stable, celui du Go ne
    // l'est pas au-delà de 12 éléments : seul l'ordre entre noms ÉGAUX peut
    // différer, et le Go ne le garantit de toute façon pas.
    out.sort_by(|a, b| a.name.cmp(&b.name));
    Ok(apply_preset_order(out, order))
}

/// `activePresetID()` du Go (`web_sysprompt.go`) : l'id du PREMIER preset
/// actif dans l'ordre de la liste, `""` si aucun ou en cas d'erreur (config
/// bricolée à la main, aucun preset créé).
pub fn active_preset_id(home: &Path, cfg: &HashMap<String, String>, order: &[String]) -> String {
    match list_presets(home, cfg, order) {
        Ok(list) => list
            .into_iter()
            .find(|p| p.active)
            .map(|p| p.id)
            .unwrap_or_default(),
        Err(_) => String::new(),
    }
}

/// `presetCtx` du Go : la taille de contexte d'un preset formatée pour la
/// pastille de la liste (« 32K », « 128K », « 1M »). `CTX` absent = 32768,
/// le défaut de serve ; illisible ou ≤ 0 → `""`.
pub fn preset_ctx(content: &str) -> String {
    let env = parse_env(content);
    let mut raw = env.get("CTX").map(|s| s.trim()).unwrap_or("");
    if raw.is_empty() {
        raw = "32768";
    }
    // strconv.Atoi : entier signé 64 bits, signe facultatif.
    let n: i64 = match raw.parse() {
        Ok(n) if n > 0 => n,
        _ => return String::new(),
    };
    if n >= 1024 && n % 1024 == 0 && n < 1 << 20 {
        return format!("{}K", n / 1024);
    }
    if n >= 1_000_000 {
        // FormatFloat(…, 'f', 1, 64) : arrondi exact de la valeur binaire,
        // comme le formatage à précision fixe de Rust.
        let s = format!("{:.1}", n as f64 / 1_000_000.0);
        return format!("{}M", s.strip_suffix(".0").unwrap_or(&s));
    }
    if n >= 1000 {
        return format!("{}K", (n + 500) / 1000);
    }
    n.to_string()
}

/// SHA-1 (FIPS 180-4). Le Go hache l'empreinte en SHA-1 et aucune crate SHA-1
/// n'est disponible : implémentation directe, vérifiée par les vecteurs
/// officiels dans les tests. Usage d'identification, pas de sécurité.
fn sha1(data: &[u8]) -> [u8; 20] {
    let mut h: [u32; 5] = [0x67452301, 0xEFCDAB89, 0x98BADCFE, 0x10325476, 0xC3D2E1F0];
    let mut msg = data.to_vec();
    let bit_len = (data.len() as u64).wrapping_mul(8);
    msg.push(0x80);
    while msg.len() % 64 != 56 {
        msg.push(0);
    }
    msg.extend_from_slice(&bit_len.to_be_bytes());
    // Le bourrage aligne `msg` sur 64 octets : le reste est vide.
    let (blocks, _) = msg.as_chunks::<64>();
    for block in blocks {
        let mut w = [0u32; 80];
        let (words, _) = block.as_chunks::<4>();
        for (wi, word) in w.iter_mut().zip(words) {
            *wi = u32::from_be_bytes(*word);
        }
        for i in 16..80 {
            w[i] = (w[i - 3] ^ w[i - 8] ^ w[i - 14] ^ w[i - 16]).rotate_left(1);
        }
        let [mut a, mut b, mut c, mut d, mut e] = h;
        for (i, wi) in w.iter().enumerate() {
            let (f, k) = match i {
                0..=19 => ((b & c) | (!b & d), 0x5A827999),
                20..=39 => (b ^ c ^ d, 0x6ED9EBA1),
                40..=59 => ((b & c) | (b & d) | (c & d), 0x8F1BBCDC),
                _ => (b ^ c ^ d, 0xCA62C1D6),
            };
            let t = a
                .rotate_left(5)
                .wrapping_add(f)
                .wrapping_add(e)
                .wrapping_add(k)
                .wrapping_add(*wi);
            e = d;
            d = c;
            c = b.rotate_left(30);
            b = a;
            a = t;
        }
        for (hv, v) in h.iter_mut().zip([a, b, c, d, e]) {
            *hv = hv.wrapping_add(v);
        }
    }
    let mut out = [0u8; 20];
    let (chunks, _) = out.as_chunks_mut::<4>();
    for (chunk, v) in chunks.iter_mut().zip(h) {
        *chunk = v.to_be_bytes();
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;

    fn write_preset(home: &Path, id: &str, body: &str) {
        let dir = presets_dir(home);
        fs::create_dir_all(&dir).unwrap();
        fs::write(dir.join(format!("{id}.env")), body).unwrap();
    }

    /// `writeTestPreset` de `web_sysprompt_test.go` : un preset minimal dont
    /// le contenu diffère d'un preset à l'autre.
    fn write_test_preset(home: &Path, id: &str, model: &str) -> String {
        write_preset(home, id, &format!("# NAME={id}\nMODEL={model}\nCTX=8192\n"));
        id.to_string()
    }

    /// `applyPresetFile` réduit à son effet sur l'empreinte : la config devient
    /// le preset, plus les réglages « appareil » de l'ancienne config.
    fn applied(home: &Path, id: &str, prev: &HashMap<String, String>) -> HashMap<String, String> {
        let body = fs::read_to_string(presets_dir(home).join(format!("{id}.env"))).unwrap();
        let mut next = parse_env(&body);
        for k in PRESERVED_KEYS {
            if let Some(v) = prev.get(*k) {
                next.insert(k.to_string(), v.clone());
            }
        }
        next
    }

    #[test]
    fn sha1_vecteurs_officiels() {
        assert_eq!(
            hex::encode(sha1(b"")),
            "da39a3ee5e6b4b0d3255bfef95601890afd80709"
        );
        assert_eq!(
            hex::encode(sha1(b"abc")),
            "a9993e364706816aba3e25717850c26c9cd0d89d"
        );
        assert_eq!(
            hex::encode(sha1(
                b"abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq"
            )),
            "84983e441c3bd26ebaae4aa1f95129e5e54670f1"
        );
        let million = vec![b'a'; 1_000_000];
        assert_eq!(
            hex::encode(sha1(&million)),
            "34aa973cd4c4daa4f61eeb2bdbad27316534016f"
        );
    }

    /// Empreinte identique, octet pour octet, à celle du Go (valeur calculée
    /// par `sha1sum` sur `CTX=4096\nMODEL=foo.gguf\nNGL=999`).
    #[test]
    fn empreinte_identique_au_go() {
        let fp = preset_fingerprint("# NAME=Mon preset\nMODEL=foo.gguf\nCTX=4096\nNGL=999\n");
        assert_eq!(fp, "67b0ec4e1d4edc24c8068c15308426144c4a51fc");
    }

    /// TestPresetFingerprintIgnoresCosmetics : la détection du preset actif ne
    /// dépend QUE de la config effective, pas de la mise en forme ni des clés
    /// « appareil ».
    #[test]
    fn preset_fingerprint_ignores_cosmetics() {
        let base = "# NAME=Mon preset\nMODEL=foo.gguf\nCTX=4096\nNGL=999\n";
        // Même config effective, présentée autrement.
        let variant =
            "export NGL=999\nMEM_MODE=off\n\nCTX=4096\n# un commentaire\nMODEL=foo.gguf\n";
        assert_eq!(
            preset_fingerprint(base),
            preset_fingerprint(variant),
            "empreintes différentes alors que la config effective est identique"
        );
        // Un vrai changement de valeur doit, lui, produire une empreinte différente.
        let changed = "MODEL=foo.gguf\nCTX=8192\nNGL=999\n";
        assert_ne!(
            preset_fingerprint(base),
            preset_fingerprint(changed),
            "empreintes identiques alors que CTX diffère"
        );
    }

    /// TestPresetOrder : les presets classés d'abord, dans l'ordre choisi ;
    /// les nouveaux (non classés) ensuite, par nom.
    #[test]
    fn preset_order() {
        let home = tempfile::tempdir().unwrap();
        for id in ["alpha", "bravo", "charlie", "delta"] {
            write_preset(home.path(), id, &format!("# NAME={id}\nCTX=4096\n"));
        }
        let cfg = HashMap::new();
        let ids = |order: &[String]| -> Vec<String> {
            list_presets(home.path(), &cfg, order)
                .unwrap()
                .into_iter()
                .map(|p| p.id)
                .collect()
        };
        let got = ids(&[]);
        assert!(
            got[0] == "alpha" && got[3] == "delta",
            "sans ordre enregistré, tri par nom attendu : {got:?}"
        );
        // savePresetOrder écrit le JSON que relit loadPresetOrder.
        let order = parse_preset_order(Some(br#"["charlie","alpha"]"#));
        assert_eq!(ids(&order), ["charlie", "alpha", "bravo", "delta"]);
    }

    /// TestPresetCtx.
    #[test]
    fn preset_ctx_cases() {
        let cases = [
            ("", "32K"), // défaut de serve : 32768
            ("CTX=40000\n", "40K"),
            ("CTX=\"131072\"\n", "128K"),
            ("CTX=1000000\n", "1M"),
            ("CTX=1500000\n", "1.5M"),
            ("CTX=512\n", "512"),
            ("CTX=abc\n", ""),
            // Arrondis vérifiés contre strconv.FormatFloat du Go.
            ("CTX=1250000\n", "1.2M"),
            ("CTX=1350000\n", "1.4M"),
            ("CTX=1050000\n", "1.1M"),
            ("CTX=2450000\n", "2.5M"),
        ];
        for (input, want) in cases {
            assert_eq!(preset_ctx(input), want, "preset_ctx({input:?})");
        }
    }

    /// TestSysPromptSuitLePresetActif (partie activePresetID) : après
    /// application d'un preset, c'est lui qui est actif ; puis l'autre.
    #[test]
    fn active_preset_suit_le_preset_applique() {
        let home = tempfile::tempdir().unwrap();
        let a = write_test_preset(home.path(), "modele-a", "/models/a.gguf");
        let b = write_test_preset(home.path(), "modele-b", "/models/b.gguf");
        let cfg = applied(home.path(), &a, &HashMap::new());
        assert_eq!(active_preset_id(home.path(), &cfg, &[]), a);
        let cfg = applied(home.path(), &b, &cfg);
        assert_eq!(active_preset_id(home.path(), &cfg, &[]), b);
        let cfg = applied(home.path(), &a, &cfg);
        assert_eq!(active_preset_id(home.path(), &cfg, &[]), a);
    }

    /// TestSysPromptReplieSurLeGlobal (partie activePresetID) : aucun preset →
    /// "" ; un preset apparaît et est appliqué → il devient actif.
    #[test]
    fn active_preset_vide_sans_preset() {
        let home = tempfile::tempdir().unwrap();
        let id = active_preset_id(home.path(), &HashMap::new(), &[]);
        assert_eq!(id, "", "preset actif {id:?} alors qu'aucun n'existe");
        // ListPresets crée le dossier, comme le Go.
        assert!(presets_dir(home.path()).is_dir());
        let a = write_test_preset(home.path(), "modele-a", "/models/a.gguf");
        let cfg = applied(home.path(), &a, &HashMap::new());
        assert_eq!(active_preset_id(home.path(), &cfg, &[]), a);
    }

    /// TestModifierLePresetActifLApplique (partie détection) : une config qui
    /// porte en plus un réglage « appareil » (MEM_MODE) désigne toujours son
    /// preset ; l'autre preset reste inactif ; le nom vient de `# NAME=`.
    #[test]
    fn preset_actif_malgre_reglage_machine() {
        let home = tempfile::tempdir().unwrap();
        write_preset(
            home.path(),
            "actif",
            "# NAME=Actif\nMODEL=foo.gguf\nCTX=32768\n",
        );
        write_preset(
            home.path(),
            "autre",
            "# NAME=Autre\nMODEL=bar.gguf\nCTX=4096\n",
        );
        let cfg = parse_env("MODEL=foo.gguf\nCTX=32768\nMEM_MODE=always\n");
        let list = list_presets(home.path(), &cfg, &[]).unwrap();
        let summary: Vec<_> = list
            .iter()
            .map(|p| (p.id.as_str(), p.name.as_str(), p.active))
            .collect();
        assert_eq!(
            summary,
            [("actif", "Actif", true), ("autre", "Autre", false)]
        );
        // Un vrai changement de réglage de modèle : plus aucun preset actif.
        let cfg = parse_env("MODEL=foo.gguf\nCTX=100000\nMEM_MODE=always\n");
        assert_eq!(active_preset_id(home.path(), &cfg, &[]), "");
    }

    /// Le dossier contient aussi des sous-dossiers et des fichiers d'autres
    /// extensions : seuls les `*.env` (fichiers) sont des presets ; le nom
    /// d'affichage retombe sur l'id sans `# NAME=`.
    #[test]
    fn liste_ignore_le_reste() {
        let home = tempfile::tempdir().unwrap();
        let dir = presets_dir(home.path());
        fs::create_dir_all(dir.join("sous.env")).unwrap();
        fs::write(dir.join("notes.txt"), "MODEL=x\n").unwrap();
        write_preset(home.path(), "Zeta", "MODEL=z.gguf\n");
        write_preset(home.path(), "b", "#name = 'Alpha'\nMODEL=a.gguf\n");
        let list = list_presets(home.path(), &HashMap::new(), &[]).unwrap();
        let summary: Vec<_> = list
            .iter()
            .map(|p| (p.id.as_str(), p.name.as_str()))
            .collect();
        assert_eq!(summary, [("b", "Alpha"), ("Zeta", "Zeta")]);
        assert_eq!(list[0].path, dir.join("b.env"));
    }

    /// Sérialisation : mêmes noms de champs que le `Preset` Go (sans étiquettes).
    #[test]
    fn serialisation_comme_le_go() {
        let p = Preset {
            id: "x".into(),
            name: "X".into(),
            path: PathBuf::from("/h/presets/x.env"),
            active: false,
        };
        assert_eq!(
            serde_json::to_string(&p).unwrap(),
            r#"{"ID":"x","Name":"X","Path":"/h/presets/x.env","Active":false}"#
        );
    }

    /// Décodage de `state/preset_order` comme `getJSON` dans un `[]string`
    /// (comportements vérifiés contre encoding/json).
    #[test]
    fn decodage_de_l_ordre() {
        assert!(parse_preset_order(None).is_empty());
        assert!(parse_preset_order(Some(b"")).is_empty());
        assert!(parse_preset_order(Some(b"null")).is_empty());
        assert!(parse_preset_order(Some(b"pas du json")).is_empty());
        assert!(parse_preset_order(Some(br#"{"x":1}"#)).is_empty());
        assert_eq!(parse_preset_order(Some(br#"["a",1,"b"]"#)), ["a", "", "b"]);
    }

    #[test]
    fn parse_env_et_guillemets() {
        let m = parse_env(
            "  export A = \"x y\" \n# B=1\nC='z'\nD=\"un\nE=--f \"/t.jinja\"\nF\nA=2\r\n",
        );
        assert_eq!(m.get("A").map(String::as_str), Some("2"));
        assert_eq!(m.get("C").map(String::as_str), Some("z"));
        assert_eq!(m.get("D").map(String::as_str), Some("\"un"));
        assert_eq!(m.get("E").map(String::as_str), Some("--f \"/t.jinja\""));
        assert!(!m.contains_key("B") && !m.contains_key("F"));
    }
}
