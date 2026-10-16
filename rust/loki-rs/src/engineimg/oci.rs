//! Le moteur llama.cpp pris dans l'image de conteneur officielle — port de
//! `backend_engine_oci.go` : jeton anonyme, manifeste (index multi-
//! architecture puis manifeste de la plateforme), couches du sommet vers la
//! base jusqu'à tenir un moteur complet, extraction à plat de `/app` avec ses
//! liens, bascule atomique du dossier, et lecture du numéro de build.

use std::collections::HashSet;
use std::io::Read;
use std::path::{Path, PathBuf};
use std::sync::LazyLock;
use std::time::Duration;

use regex::Regex;
use serde::Deserialize;
use serde_json::Value;

use crate::backends::Log;
use crate::gojson::{self, Kind};

/// `ociEngineRepo`.
pub const ENGINE_REPO: &str = "ggml-org/llama.cpp";

/// Au-delà, une couche est le runtime de l'image, pas le moteur (`ociLayerCap`).
pub const LAYER_CAP: i64 = 600 << 20;

/// `ociAccept`.
const ACCEPT: &str = "application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json";

/// `ociDoc` décodé : index ou manifeste.
const DOC_TYPE: &str = "ociDoc";

/// `ociBase` : `https://ghcr.io`, ou `LOKI_OCI_REGISTRY` (schéma ajouté au
/// besoin, `/` final retiré).
pub fn base_from_env(v: Option<&str>) -> String {
    let v = v.unwrap_or("").trim().trim_end_matches('/');
    if v.is_empty() {
        return "https://ghcr.io".into();
    }
    if v.contains("://") {
        v.into()
    } else {
        format!("https://{v}")
    }
}

/// `ociHost`.
pub fn host_of(base: &str) -> String {
    base.trim_start_matches("https://")
        .trim_start_matches("http://")
        .trim_end_matches('/')
        .to_string()
}

/// `ociArch`.
pub fn oci_arch(arm64: bool) -> &'static str {
    if arm64 { "arm64" } else { "amd64" }
}

/// Une couche du manifeste.
#[derive(Clone, Debug, Deserialize, PartialEq)]
pub struct Layer {
    #[serde(default, rename = "mediaType")]
    pub media_type: String,
    #[serde(default)]
    pub digest: String,
    #[serde(default)]
    pub size: i64,
}

/// Le client du registre.
#[derive(Clone)]
pub struct Registry {
    pub base: String,
    pub arm64: bool,
    client: reqwest::Client,
}

impl Registry {
    pub fn new(base: String, arm64: bool) -> Self {
        crate::chat::engine::install_crypto();
        Registry {
            base,
            arm64,
            client: reqwest::Client::builder().build().unwrap_or_default(),
        }
    }

    pub fn host(&self) -> String {
        host_of(&self.base)
    }

    /// `ociToken` : un jeton de lecture anonyme.
    pub async fn token(&self, repo: &str) -> Result<String, String> {
        let u = format!(
            "{}/token?scope=repository:{repo}:pull&service={}",
            self.base,
            self.host()
        );
        let resp = self
            .client
            .get(&u)
            .timeout(Duration::from_secs(30))
            .send()
            .await
            .map_err(|e| format!("registre {} injoignable : {e}", self.host()))?;
        if resp.status().as_u16() != 200 {
            return Err(format!(
                "{}/token : HTTP {}",
                self.host(),
                resp.status().as_u16()
            ));
        }
        let body = resp.bytes().await.unwrap_or_default();
        let d = gojson::decode_struct(
            &body,
            "struct { Token string \"json:\\\"token\\\"\" }",
            &[("token", Kind::Str)],
        );
        let t = d.str("token");
        if d.err.is_some() || t.is_empty() {
            return Err("jeton de lecture illisible".into());
        }
        Ok(t)
    }

    /// Un manifeste (index ou image) par référence.
    async fn doc(&self, repo: &str, reference: &str, token: &str) -> Result<Value, String> {
        let resp = self
            .client
            .get(format!("{}/v2/{repo}/manifests/{reference}", self.base))
            .header("Authorization", format!("Bearer {token}"))
            .header("Accept", ACCEPT)
            .send()
            .await
            .map_err(|e| e.to_string())?;
        match resp.status().as_u16() {
            404 => return Err(format!("version inconnue dans le registre : {reference}")),
            200 => {}
            s => return Err(format!("manifeste {reference} : HTTP {s}")),
        }
        let body = resp.bytes().await.map_err(|e| e.to_string())?;
        let d = gojson::decode_struct(
            &body,
            DOC_TYPE,
            &[
                ("mediaType", Kind::Str),
                ("manifests", Kind::Any),
                ("layers", Kind::Any),
                ("annotations", Kind::Any),
            ],
        );
        if let Some(e) = d.err {
            return Err(format!("manifeste {reference} illisible : {e}"));
        }
        Ok(Value::Object(d.fields))
    }

    /// `ociResolve` : les couches de l'image pour cette architecture, et le
    /// build qu'elle annonce.
    pub async fn resolve(&self, repo: &str, tag: &str) -> Result<(Vec<Layer>, String), String> {
        let token = self.token(repo).await?;
        let version_of = |d: &Value| -> String {
            d.get("annotations")
                .and_then(|a| a.get("org.opencontainers.image.version"))
                .and_then(Value::as_str)
                .unwrap_or("")
                .to_string()
        };
        let mut doc = self.doc(repo, tag, &token).await?;
        let mut version = version_of(&doc);
        let manifests = doc
            .get("manifests")
            .and_then(Value::as_array)
            .cloned()
            .unwrap_or_default();
        if !manifests.is_empty() {
            let want = oci_arch(self.arm64);
            let digest = manifests
                .iter()
                .find(|m| m["platform"]["os"] == "linux" && m["platform"]["architecture"] == want)
                .and_then(|m| m.get("digest").and_then(Value::as_str))
                .unwrap_or("")
                .to_string();
            if digest.is_empty() {
                return Err(format!("aucune image linux/{want} pour {tag}"));
            }
            doc = self.doc(repo, &digest, &token).await?;
            let v = version_of(&doc);
            if !v.is_empty() {
                version = v;
            }
        }
        let layers: Vec<Layer> = doc
            .get("layers")
            .cloned()
            .and_then(|l| serde_json::from_value(l).ok())
            .unwrap_or_default();
        if layers.is_empty() {
            return Err(format!("manifeste {tag} sans couche"));
        }
        Ok((layers, version))
    }

    /// `engineOCILatest` : le dernier build publié pour la variante (numéro et
    /// tag figé), lu sur l'annotation du tag mouvant.
    pub async fn latest(&self, variant: &str) -> Result<(i64, String), String> {
        let (_, version) = self.resolve(ENGINE_REPO, variant).await?;
        if version.is_empty() {
            return Ok((0, variant.into()));
        }
        let n = version.trim_start_matches('b').parse().unwrap_or(0);
        Ok((n, format!("{variant}-{version}")))
    }
}

/// `engineDir`.
pub fn engine_dir(home: &Path) -> PathBuf {
    home.join("engine")
}

static RE_TAG_BUILD: LazyLock<Regex> = LazyLock::new(|| Regex::new(r"-b([0-9]+)$").expect("motif"));

/// `engineTagBuild` : « server-cuda-b10450 » → 10450, 0 sans numéro.
pub fn tag_build(tag: &str) -> i64 {
    RE_TAG_BUILD
        .captures(tag)
        .and_then(|m| m[1].parse().ok())
        .unwrap_or(0)
}

/// `engineInstalled` : les versions présentes, de la plus ancienne à la plus
/// récente.
pub fn installed(home: &Path) -> Vec<String> {
    let dir = engine_dir(home);
    let Ok(rd) = std::fs::read_dir(&dir) else {
        return Vec::new();
    };
    let mut tags: Vec<String> = rd
        .flatten()
        .filter(|e| e.file_type().is_ok_and(|t| t.is_dir()))
        .map(|e| e.file_name().to_string_lossy().into_owned())
        .filter(|t| crate::backends::probe::is_file(&dir.join(t).join("llama-server")))
        .collect();
    tags.sort();
    tags.sort_by_key(|t| tag_build(t));
    tags
}

/// `engineBinFor` : le llama-server d'une version installée, "" sinon.
pub fn bin_for(home: &Path, tag: &str) -> String {
    let p = engine_dir(home)
        .join(crate::paths::go_base(tag))
        .join("llama-server");
    if crate::backends::probe::is_file(&p) {
        p.to_string_lossy().into_owned()
    } else {
        String::new()
    }
}

/// `enginePrune` : les versions installées hors `keep`, supprimées.
pub fn prune(home: &Path, keep: &HashSet<String>) -> Vec<String> {
    let mut out = Vec::new();
    for tag in installed(home) {
        if keep.contains(&tag) {
            continue;
        }
        if std::fs::remove_dir_all(engine_dir(home).join(&tag)).is_ok() {
            out.push(format!("ancienne version supprimée : {tag}"));
        }
    }
    out
}

/// `appEntryName` : « app/libllama.so » → « libllama.so » ; rien pour tout ce
/// qui n'est pas un fichier direct de /app (ni marqueur de suppression).
pub fn app_entry_name(raw: &str) -> Option<String> {
    let p = raw.trim().replace('\\', "/");
    let p = p.strip_prefix("./").unwrap_or(&p);
    let rest = p.strip_prefix("app/")?;
    if rest.is_empty() || rest.contains('/') || rest.starts_with(".wh.") {
        return None;
    }
    Some(rest.to_string())
}

/// `engineSetComplete` : le binaire ET ses bibliothèques.
pub fn set_complete(seen: &HashSet<String>) -> bool {
    seen.contains("llama-server")
        && seen.iter().any(|n| n.starts_with("libggml-base.so"))
        && seen.iter().any(|n| n.starts_with("libllama.so"))
}

/// `extractAppLayer` : les entrées `app/…` d'une couche, à plat dans `dir` ;
/// une couche supérieure fait foi (`seen`). Rend le nombre de fichiers.
pub fn extract_app_layer(
    archive: &Path,
    dir: &Path,
    seen: &mut HashSet<String>,
    links: &mut Vec<(PathBuf, String)>,
) -> Result<usize, String> {
    let f = std::fs::File::open(archive)
        .map_err(|e| crate::paths::os_err("open", &archive.to_string_lossy(), &e))?;
    let gz = flate2::read::GzDecoder::new(f);
    let mut tr = tar::Archive::new(gz);
    let mut count = 0;
    for entry in tr.entries().map_err(|e| e.to_string())? {
        let mut entry = entry.map_err(|e| e.to_string())?;
        let raw = String::from_utf8_lossy(&entry.path_bytes()).into_owned();
        let Some(name) = app_entry_name(&raw) else {
            continue;
        };
        if seen.contains(&name) {
            continue;
        }
        let p = dir.join(&name);
        let kind = entry.header().entry_type();
        if kind.is_file() {
            let mut buf = Vec::new();
            entry.read_to_end(&mut buf).map_err(|e| e.to_string())?;
            std::fs::write(&p, &buf)
                .map_err(|e| crate::paths::os_err("open", &p.to_string_lossy(), &e))?;
            #[cfg(unix)]
            if let Ok(mode) = entry.header().mode() {
                use std::os::unix::fs::PermissionsExt;
                let _ = std::fs::set_permissions(&p, std::fs::Permissions::from_mode(mode & 0o777));
            }
            seen.insert(name);
            count += 1;
        } else if kind.is_symlink() || kind.is_hard_link() {
            let link = entry
                .link_name_bytes()
                .map(|b| String::from_utf8_lossy(&b).into_owned())
                .unwrap_or_default();
            let target = app_entry_name(&link).unwrap_or_else(|| crate::paths::go_base(&link));
            links.push((p, target));
            seen.insert(name);
            count += 1;
        }
    }
    Ok(count)
}

/// `engineOCIInstall` : le moteur du tag dans `engine/<tag>` ; rend le chemin
/// du llama-server. Une version déjà là est rendue telle quelle.
pub async fn install(
    home: &Path,
    reg: &Registry,
    tag: &str,
    log: &Log<'_>,
) -> Result<String, String> {
    let tag = crate::paths::go_base(tag.trim());
    if tag.is_empty() || tag == "." {
        return Err("version du moteur non précisée".into());
    }
    let bin = bin_for(home, &tag);
    if !bin.is_empty() {
        log.line(&format!("déjà installé : {tag}")).await;
        return Ok(bin);
    }
    log.phase(&format!("lecture du manifeste {tag}…")).await;
    let (layers, version) = reg.resolve(ENGINE_REPO, &tag).await?;
    if !version.is_empty() {
        log.line(&format!(
            "image {ENGINE_REPO}:{tag} — llama.cpp {version}, {} couches",
            layers.len()
        ))
        .await;
    }
    let dest = engine_dir(home).join(&tag);
    let tmp = PathBuf::from(format!("{}.part", dest.to_string_lossy()));
    let _ = std::fs::remove_dir_all(&tmp);
    std::fs::create_dir_all(&tmp)
        .map_err(|e| crate::paths::os_err("mkdir", &tmp.to_string_lossy(), &e))?;
    let res = install_into(reg, &tag, &layers, &tmp, &dest, log).await;
    let _ = std::fs::remove_dir_all(&tmp);
    res
}

async fn install_into(
    reg: &Registry,
    tag: &str,
    layers: &[Layer],
    tmp: &Path,
    dest: &Path,
    log: &Log<'_>,
) -> Result<String, String> {
    let token = reg.token(ENGINE_REPO).await?;
    let mut seen = HashSet::new();
    let mut links = Vec::new();
    for (i, l) in layers.iter().enumerate().rev() {
        if l.size > LAYER_CAP {
            log.line(&format!(
                "couche {i} ignorée ({} Mo) : c'est le runtime de l'image, pas le moteur",
                l.size / 1_000_000
            ))
            .await;
            break;
        }
        let part = tmp.join("layer.tgz");
        if l.size > 4 << 20 {
            log.phase(&format!(
                "téléchargement du moteur ({} Mo)…",
                l.size / 1_000_000
            ))
            .await;
        }
        let rq = reg
            .client
            .get(format!("{}/v2/{ENGINE_REPO}/blobs/{}", reg.base, l.digest))
            .header("Authorization", format!("Bearer {token}"));
        crate::backends::log_download(log, rq, &part, l.size)
            .await
            .map_err(|e| format!("téléchargement de la couche {i} : {e}"))?;
        let (p2, d2) = (part.clone(), tmp.to_path_buf());
        let mut s2 = std::mem::take(&mut seen);
        let mut l2 = std::mem::take(&mut links);
        let (n, s2, l2) = tokio::task::spawn_blocking(move || {
            let r = extract_app_layer(&p2, &d2, &mut s2, &mut l2);
            (r, s2, l2)
        })
        .await
        .map_err(|e| e.to_string())?;
        seen = s2;
        links = l2;
        let _ = std::fs::remove_file(&part);
        let n = n.map_err(|e| format!("extraction de la couche {i} : {e}"))?;
        if n > 0 {
            log.line(&format!("couche {i} : {n} fichiers du moteur"))
                .await;
        }
        if set_complete(&seen) {
            break;
        }
    }
    crate::backends::prebuilt::apply_links(&links)?;
    if !seen.contains("llama-server") {
        return Err(format!(
            "llama-server absent de l'image {tag} — variante inattendue ?"
        ));
    }
    let _ = std::fs::remove_dir_all(dest);
    if let Some(parent) = dest.parent() {
        std::fs::create_dir_all(parent)
            .map_err(|e| crate::paths::os_err("mkdir", &parent.to_string_lossy(), &e))?;
    }
    std::fs::rename(tmp, dest).map_err(|e| e.to_string())?;
    let bin = dest.join("llama-server");
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&bin, std::fs::Permissions::from_mode(0o755))
            .map_err(|e| crate::paths::os_err("chmod", &bin.to_string_lossy(), &e))?;
    }
    let bin = bin.to_string_lossy().into_owned();
    log.line(&format!("moteur installé : {bin}")).await;
    Ok(bin)
}

static RE_BUILD_LONG: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"build[\t\n\f\r ]+([0-9]+),[\t\n\f\r ]*commit[\t\n\f\r ]+([0-9a-fA-F]+)")
        .expect("motif")
});
static RE_BUILD_SHORT: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(
        r"(?m)^[\t\n\f\r ]*(?:version|build)[\t\n\f\r ]*:[\t\n\f\r ]*([0-9]+)[\t\n\f\r ]*\(([0-9a-fA-F]+)\)",
    )
    .expect("motif")
});

/// `parseEngineBuild` : (build, commit) des trois formes de la bannière.
pub fn parse_build(out: &str) -> (i64, String) {
    for re in [&*RE_BUILD_LONG, &*RE_BUILD_SHORT] {
        if let Some(m) = re.captures(out) {
            return (m[1].parse().unwrap_or(0), m[2].to_string());
        }
    }
    (0, String::new())
}

/// Un binaire lancé avec ses bibliothèques à côté : (sortie mêlée, erreur au
/// format d'`exec`). `limit` : délai (tué au-delà).
pub async fn run_bin(bin: &str, arg: &str, limit: Option<Duration>) -> (String, Option<String>) {
    let dir = crate::paths::go_dir(bin);
    let env = crate::serve::run::Env::default();
    let (k, v) = crate::serve::run::library_path_value(&env, &dir);
    let mut c = tokio::process::Command::new(bin);
    c.arg(arg)
        .env(k, v)
        .stdin(std::process::Stdio::null())
        .kill_on_drop(true);
    crate::backends::devices::combined(c, bin, limit).await
}

/// `engineBuildOf` : le build d'un llama-server (0 s'il ne répond pas).
pub async fn build_of(bin: &str) -> (i64, String) {
    if bin.is_empty() || !crate::backends::probe::is_file(Path::new(bin)) {
        return (0, String::new());
    }
    let (out, _) = run_bin(bin, "--version", Some(Duration::from_secs(15))).await;
    parse_build(&out)
}

/// `tailLines` : les n dernières lignes.
pub fn tail_lines(s: &str, n: usize) -> String {
    let lines: Vec<&str> = s.trim_end_matches('\n').split('\n').collect();
    let from = lines.len().saturating_sub(n);
    lines[from..].join("\n")
}

/// `engineListDevices`.
pub async fn list_devices(bin: &str) -> Result<usize, String> {
    let (out, err) = run_bin(bin, "--list-devices", None).await;
    if let Some(e) = err {
        return Err(format!(
            "le moteur ne démarre pas ici : {e}\n{}",
            tail_lines(&out, 12)
        ));
    }
    Ok(crate::backends::devices::parse_list_devices(&out).len())
}

/// `engineSmokeTest` : le nouveau moteur démarre, et voit les cartes que
/// voyait l'actuel.
pub async fn smoke_test(bin: &str, ancien: &str) -> Result<(), String> {
    if list_devices(bin).await? > 0 {
        return Ok(());
    }
    if ancien.is_empty() || ancien == bin {
        return Ok(());
    }
    match list_devices(ancien).await {
        Ok(n) if n > 0 => Err(format!(
            "le moteur téléchargé ne voit aucune carte graphique, alors que le moteur actuel en voit {n} — il tournerait sur le processeur, beaucoup plus lentement"
        )),
        _ => Ok(()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn entrees_app() {
        assert_eq!(
            app_entry_name("app/libllama.so").as_deref(),
            Some("libllama.so")
        );
        assert_eq!(
            app_entry_name("./app/llama-server").as_deref(),
            Some("llama-server")
        );
        assert_eq!(app_entry_name("app/"), None);
        assert_eq!(app_entry_name("app/sous/x"), None);
        assert_eq!(app_entry_name("app/.wh.x"), None);
        assert_eq!(app_entry_name("usr/app/x"), None);
    }

    #[test]
    fn builds() {
        assert_eq!(
            parse_build("version: 0.1.0-dev (build 10450, commit ece963f41)"),
            (10450, "ece963f41".into())
        );
        assert_eq!(
            parse_build("version: 10450 (ece963f)"),
            (10450, "ece963f".into())
        );
        assert_eq!(
            parse_build("build: 4589 (a1b2c3d)"),
            (4589, "a1b2c3d".into())
        );
        assert_eq!(parse_build("rien"), (0, String::new()));
        assert_eq!(tag_build("server-cuda-b10450"), 10450);
        assert_eq!(tag_build("server-cuda"), 0);
    }

    #[test]
    fn moteur_complet() {
        let set = |v: &[&str]| v.iter().map(|s| s.to_string()).collect::<HashSet<_>>();
        assert!(!set_complete(&set(&["libggml-base.so", "libllama.so"])));
        assert!(!set_complete(&set(&["llama-server", "libggml-base.so.0"])));
        assert!(!set_complete(&set(&["llama-server", "libllama.so.0"])));
        assert!(set_complete(&set(&[
            "llama-server",
            "libggml-base.so.0",
            "libllama.so"
        ])));
        assert_eq!(tail_lines("a\nb\nc\n", 2), "b\nc");
        assert_eq!(tail_lines("a", 5), "a");
    }

    /// Ne voir aucune carte n'est un échec que si le moteur courant en voyait.
    #[tokio::test]
    async fn essai_a_blanc() {
        use std::os::unix::fs::PermissionsExt;
        let d = tempfile::tempdir().unwrap();
        let script = |name: &str, body: &str| {
            let p = d.path().join(name);
            std::fs::write(&p, format!("#!/bin/sh\n{body}\n")).unwrap();
            std::fs::set_permissions(&p, std::fs::Permissions::from_mode(0o755)).unwrap();
            p.to_string_lossy().into_owned()
        };
        let none = script("aucune", "echo 'Available devices:'");
        let one = script(
            "une",
            "echo 'Available devices:'; echo '  CUDA0: Carte (1000 MiB, 900 MiB free)'",
        );
        let ko = script("ko", "echo panne; exit 3");
        assert_eq!(smoke_test(&none, "").await, Ok(()));
        assert_eq!(smoke_test(&none, &none).await, Ok(()));
        assert_eq!(smoke_test(&none, &ko).await, Ok(()));
        assert_eq!(smoke_test(&one, &ko).await, Ok(()));
        let e = smoke_test(&none, &one).await.unwrap_err();
        assert!(e.contains("en voit 1"), "{e}");
        let e = smoke_test(&ko, &one).await.unwrap_err();
        assert_eq!(e, "le moteur ne démarre pas ici : exit status 3\npanne");
    }

    #[test]
    fn registre() {
        assert_eq!(base_from_env(None), "https://ghcr.io");
        assert_eq!(
            base_from_env(Some(" miroir.local:5000/ ")),
            "https://miroir.local:5000"
        );
        assert_eq!(base_from_env(Some("http://x")), "http://x");
        assert_eq!(host_of("https://ghcr.io/"), "ghcr.io");
    }
}
