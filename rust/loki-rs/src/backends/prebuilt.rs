//! Binaires précompilés officiels de llama.cpp : port de `backend_prebuilt.go`
//! (dernière release, choix de l'asset, téléchargement, extraction avec les
//! liens, marqueur VERSION, ménage des anciennes versions).

use std::io::Read;
use std::path::{Path, PathBuf};
use std::sync::LazyLock;

use futures_util::StreamExt;
use regex::Regex;
use serde::Deserialize;
use serde_json::Value;

use crate::gojson::Kind;
use tokio::io::AsyncWriteExt;

use super::probe::Probe;

/// `llamaReleasesAPI`.
pub const RELEASES_API: &str = "https://api.github.com/repos/ggml-org/llama.cpp/releases/latest";

/// La struct anonyme que `fetchLlamaLatest` décode.
const RELEASE_TYPE: &str =
    r#"struct { TagName string "json:\"tag_name\""; Assets []loki.ghAsset "json:\"assets\"" }"#;

/// `prebuiltFormat` : extraction qui recrée les liens des archives.
pub const FORMAT: &str = "fmt2";

/// `ghAsset`.
#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct Asset {
    #[serde(default)]
    pub name: String,
    #[serde(default, rename = "browser_download_url")]
    pub url: String,
    #[serde(default)]
    pub size: i64,
}

/// `prebuiltDir`.
pub fn dir(home: &Path) -> PathBuf {
    home.join("backends").join("llama.cpp-prebuilt")
}

fn version_fields(home: &Path) -> Vec<String> {
    std::fs::read_to_string(dir(home).join("VERSION"))
        .map(|s| s.split_whitespace().map(str::to_string).collect())
        .unwrap_or_default()
}

/// `prebuiltVersion` : (tag, version CUDA). La version CUDA est vide hors
/// CUDA Windows : « b300  fmt2 » n'a alors que deux champs, le format en
/// second.
pub fn version(home: &Path) -> (String, String) {
    let f = version_fields(home);
    let cuda = f
        .get(1)
        .filter(|c| !c.starts_with("fmt"))
        .cloned()
        .unwrap_or_default();
    (f.first().cloned().unwrap_or_default(), cuda)
}

/// `prebuiltVersionFormat` : le dernier champ du marqueur, s'il en est un.
pub fn version_format(home: &Path) -> String {
    let f = version_fields(home);
    match f.last() {
        Some(last) if f.len() > 1 && last.starts_with("fmt") => last.clone(),
        _ => String::new(),
    }
}

/// `prebuiltServerBin`.
pub fn server_bin(home: &Path) -> String {
    crate::serve::run::EnginePaths {
        home: home.to_path_buf(),
    }
    .prebuilt_server_bin()
}

/// `prebuiltPrune` : les extractions d'autres releases supprimées.
pub fn prune(home: &Path, keep: &str, logf: &mut dyn FnMut(String)) {
    let keep_dir = if keep.is_empty() {
        String::new()
    } else {
        crate::paths::go_clean(keep)
    };
    let d = dir(home);
    let Ok(rd) = std::fs::read_dir(&d) else {
        return;
    };
    let mut names: Vec<String> = rd
        .flatten()
        .filter(|e| e.file_type().is_ok_and(|t| t.is_dir()))
        .map(|e| e.file_name().to_string_lossy().into_owned())
        .filter(|n| n.starts_with("llama-b"))
        .collect();
    names.sort();
    for name in names {
        let p = d.join(&name);
        let ps = crate::paths::go_clean(&p.to_string_lossy());
        if !keep_dir.is_empty() && keep_dir.starts_with(&format!("{ps}/")) {
            continue;
        }
        if std::fs::remove_dir_all(&p).is_ok() {
            logf(format!("ancienne version supprimée : {name}"));
        }
    }
}

/// `fetchLlamaLatest` : (tag, assets) de la dernière release.
pub async fn fetch_latest(
    client: &reqwest::Client,
    url: &str,
) -> Result<(String, Vec<Asset>), String> {
    let resp = client
        .get(url)
        .header("Accept", "application/vnd.github+json")
        .header("User-Agent", "Go-http-client/1.1")
        .timeout(std::time::Duration::from_secs(30))
        .send()
        .await
        .map_err(|e| e.to_string())?;
    if resp.status().as_u16() != 200 {
        return Err(format!("GitHub API : HTTP {}", resp.status().as_u16()));
    }
    let body = resp.bytes().await.map_err(|e| e.to_string())?;
    // Erreurs de décodage au mot près du Go (`json.NewDecoder(…).Decode`).
    let d = crate::gojson::decode_struct(
        &body,
        RELEASE_TYPE,
        &[("tag_name", Kind::Str), ("assets", Kind::Any)],
    );
    if let Some(e) = d.err {
        return Err(e);
    }
    let assets: Vec<Asset> = match d.fields.get("assets") {
        Some(Value::Null) | None => Vec::new(),
        Some(v) => serde_json::from_value(v.clone()).map_err(|e| e.to_string())?,
    };
    let tag = d.str("tag_name");
    if tag.is_empty() {
        return Err("release invalide (tag vide)".into());
    }
    Ok((tag, assets))
}

/// `assetMatch` : les assets dont le nom contient TOUS les fragments.
pub fn asset_match(assets: &[Asset], frags: &[&str]) -> Vec<Asset> {
    assets
        .iter()
        .filter(|a| frags.iter().all(|f| a.name.contains(f)))
        .cloned()
        .collect()
}

static RE_CUDA_ASSET_VER: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"cuda-([0-9]+\.[0-9]+)").expect("motif"));

/// Le choix de `pickPrebuilt`.
#[derive(Clone, Debug, PartialEq)]
pub struct Pick {
    pub main: Asset,
    pub cudart: Option<Asset>,
    pub label: String,
    pub cuda_ver: String,
}

/// Système et architecture visés (`runtime.GOOS`, `runtime.GOARCH`).
#[derive(Clone, Copy, Debug)]
pub struct Target {
    pub os: &'static str,
    pub arm64: bool,
}

impl Target {
    pub fn current() -> Self {
        Target {
            os: if cfg!(windows) {
                "windows"
            } else if cfg!(target_os = "macos") {
                "darwin"
            } else {
                "linux"
            },
            arm64: cfg!(target_arch = "aarch64"),
        }
    }
}

/// `pickPrebuilt` : l'asset principal (+ cudart pour CUDA Windows) adapté à
/// la machine, et un libellé lisible du variant.
pub async fn pick(assets: &[Asset], probe: &Probe, t: Target) -> Result<Pick, String> {
    let first = |l: Vec<Asset>| l.into_iter().next();
    let mut cudart = None;
    let mut cuda_ver = String::new();
    let (main, label): (Option<Asset>, String) = match t.os {
        "windows" => 'w: {
            if t.arm64 {
                break 'w (
                    first(asset_match(assets, &["llama-", "bin-win-cpu-arm64"])),
                    "CPU (Windows arm64)".into(),
                );
            }
            if probe.has_nvidia_gpu().await {
                let cand: Vec<Asset> =
                    asset_match(assets, &["llama-", "bin-win-cuda-", "-x64.zip"])
                        .into_iter()
                        .filter(|a| !a.name.starts_with("cudart"))
                        .collect();
                let max_v = probe.driver_cuda_version().await;
                let mut best: Option<Asset> = None;
                let mut best_v = 0.0;
                for a in cand {
                    let Some(m) = RE_CUDA_ASSET_VER.captures(&a.name) else {
                        continue;
                    };
                    let v: f64 = m[1].parse().unwrap_or(0.0);
                    let ok = (max_v == 0.0 && (best.is_none() || v < best_v))
                        || (max_v > 0.0 && v <= max_v && v > best_v);
                    if ok {
                        cuda_ver = m[1].to_string();
                        best = Some(a);
                        best_v = v;
                    }
                }
                if let Some(b) = best {
                    cudart = first(asset_match(
                        assets,
                        &["cudart-", &format!("win-cuda-{cuda_ver}")],
                    ));
                    break 'w (Some(b), format!("CUDA {cuda_ver} (Windows x64)"));
                }
            }
            if let Some(a) = first(asset_match(assets, &["llama-", "bin-win-vulkan-x64"])) {
                break 'w (Some(a), "Vulkan (Windows x64)".into());
            }
            (
                first(asset_match(assets, &["llama-", "bin-win-cpu-x64"])),
                "CPU (Windows x64)".into(),
            )
        }
        "darwin" => {
            let arch = if t.arm64 { "arm64" } else { "x64" };
            (
                first(asset_match(
                    assets,
                    &["llama-", &format!("bin-macos-{arch}")],
                )),
                format!("Metal (macOS {arch})"),
            )
        }
        _ => 'l: {
            let arch = if t.arm64 { "arm64" } else { "x64" };
            if (probe.has_tool("hipcc") || super::probe::is_dir(&probe.sys("/opt/rocm")))
                && let Some(a) = first(asset_match(assets, &["llama-", "bin-ubuntu-rocm-", arch]))
            {
                break 'l (Some(a), format!("ROCm (Linux {arch})"));
            }
            let nvidia = probe.has_nvidia_gpu().await;
            if (nvidia || probe.has_tool("vulkaninfo"))
                && let Some(a) = first(asset_match(
                    assets,
                    &["llama-", &format!("bin-ubuntu-vulkan-{arch}")],
                ))
            {
                let mut label = format!("Vulkan (Linux {arch})");
                // Le Go relance nvidia-smi pour ce suffixe : même résultat.
                if probe.has_nvidia_gpu().await {
                    label.push_str(" — pas de build CUDA officiel Linux ; compile localement pour du CUDA natif");
                }
                break 'l (Some(a), label);
            }
            (
                first(asset_match(
                    assets,
                    &["llama-", &format!("bin-ubuntu-{arch}.tar.gz")],
                )),
                format!("CPU (Linux {arch})"),
            )
        }
    };
    match main {
        Some(main) => Ok(Pick {
            main,
            cudart,
            label,
            cuda_ver,
        }),
        None => {
            Err("aucun binaire précompilé adapté à cette machine dans la release officielle".into())
        }
    }
}

/// `recommendedMode` : {mode, why}.
pub fn recommended_mode(backend: &str, os: &str) -> serde_json::Value {
    if os == "linux" && backend == "cuda" {
        return serde_json::json!({
            "mode": "opt",
            "why": "seule voie vers du CUDA natif : llama.cpp ne publie aucun binaire CUDA pour Linux, le précompilé ne donnerait que du Vulkan. Compte plusieurs minutes de compilation.",
        });
    }
    serde_json::json!({ "mode": "fast", "why": "" })
}

/// `downloadWithProgress` : progression journalisée par tranches de ~25 Mo.
pub async fn download(
    client: &reqwest::Client,
    url: &str,
    dest: &Path,
    total: i64,
    logf: &mut (dyn FnMut(String) + Send),
) -> Result<(), String> {
    let rq = client.get(url).header("User-Agent", "Go-http-client/1.1");
    download_request(rq, dest, total, logf).await
}

/// `downloadRequest` : la même chose pour une requête déjà construite — un
/// registre OCI exige un en-tête d'autorisation, même en lecture anonyme.
pub async fn download_request(
    rq: reqwest::RequestBuilder,
    dest: &Path,
    total: i64,
    logf: &mut (dyn FnMut(String) + Send),
) -> Result<(), String> {
    let resp = rq.send().await.map_err(|e| e.to_string())?;
    if resp.status().as_u16() != 200 {
        return Err(format!("HTTP {}", resp.status().as_u16()));
    }
    let total = if total <= 0 {
        resp.content_length().map_or(-1, |n| n as i64)
    } else {
        total
    };
    let mut f = tokio::fs::File::create(dest)
        .await
        .map_err(|e| crate::paths::os_err("open", &dest.to_string_lossy(), &e))?;
    let (mut done, mut last_log) = (0i64, 0i64);
    let mut stream = resp.bytes_stream();
    while let Some(chunk) = stream.next().await {
        let chunk = chunk.map_err(|e| e.to_string())?;
        f.write_all(&chunk)
            .await
            .map_err(|e| crate::paths::os_err("write", &dest.to_string_lossy(), &e))?;
        done += chunk.len() as i64;
        if done - last_log >= 25 << 20 {
            last_log = done;
            if total > 0 {
                logf(format!(
                    "⬇ {} / {} Mo ({}%)",
                    done / 1_000_000,
                    total / 1_000_000,
                    done * 100 / total
                ));
            } else {
                logf(format!("⬇ {} Mo", done / 1_000_000));
            }
        }
    }
    f.flush()
        .await
        .map_err(|e| crate::paths::os_err("write", &dest.to_string_lossy(), &e))?;
    Ok(())
}

/// `extractArchive` : .zip ou .tar.gz dans `dir`, sans entrée qui
/// s'échapperait du dossier ; les liens des tar posés après coup.
pub fn extract(path: &Path, dir: &Path) -> Result<(), String> {
    let dir_s = dir.to_string_lossy().into_owned();
    let safe = |name: &str| -> Result<PathBuf, String> {
        let p = crate::paths::go_join(&[&dir_s, name]);
        let rel = go_rel(&crate::paths::go_clean(&dir_s), &p);
        match rel {
            Some(r) if !r.starts_with("..") => Ok(PathBuf::from(p)),
            _ => Err(format!("entrée d'archive suspecte : {name}")),
        }
    };
    let ps = path.to_string_lossy();
    if ps.ends_with(".zip") || ps.ends_with(".zip.part") {
        let f = std::fs::File::open(path).map_err(|e| crate::paths::os_err("open", &ps, &e))?;
        let mut zr = zip::ZipArchive::new(f).map_err(|e| e.to_string())?;
        for i in 0..zr.len() {
            let mut e = zr.by_index(i).map_err(|e| e.to_string())?;
            let name = e.name().to_string();
            let p = safe(&name)?;
            if e.is_dir() {
                mkdir_all(&p)?;
                continue;
            }
            if let Some(parent) = p.parent() {
                mkdir_all(parent)?;
            }
            let mut w = std::fs::File::create(&p)
                .map_err(|err| crate::paths::os_err("open", &p.to_string_lossy(), &err))?;
            std::io::copy(&mut e, &mut w).map_err(|err| err.to_string())?;
        }
        return Ok(());
    }
    let f = std::fs::File::open(path).map_err(|e| crate::paths::os_err("open", &ps, &e))?;
    let gz = flate2::read::GzDecoder::new(f);
    let mut tr = tar::Archive::new(gz);
    let mut links: Vec<(PathBuf, String)> = Vec::new();
    let entries = tr.entries().map_err(|e| e.to_string())?;
    for entry in entries {
        let mut entry = entry.map_err(|e| e.to_string())?;
        let name = String::from_utf8_lossy(&entry.path_bytes()).into_owned();
        let p = safe(&name)?;
        let kind = entry.header().entry_type();
        if kind.is_dir() {
            mkdir_all(&p)?;
        } else if kind.is_file() {
            if let Some(parent) = p.parent() {
                mkdir_all(parent)?;
            }
            let mut w = std::fs::File::create(&p)
                .map_err(|err| crate::paths::os_err("open", &p.to_string_lossy(), &err))?;
            let mut buf = Vec::new();
            entry.read_to_end(&mut buf).map_err(|e| e.to_string())?;
            std::io::Write::write_all(&mut w, &buf).map_err(|e| e.to_string())?;
            drop(w);
            #[cfg(unix)]
            if let Ok(mode) = entry.header().mode() {
                use std::os::unix::fs::PermissionsExt;
                let _ = std::fs::set_permissions(&p, std::fs::Permissions::from_mode(mode & 0o777));
            }
        } else if kind.is_symlink() || kind.is_hard_link() {
            let target = entry
                .link_name_bytes()
                .map(|b| String::from_utf8_lossy(&b).into_owned())
                .unwrap_or_default();
            let target = if kind.is_hard_link() {
                safe(&target)?.to_string_lossy().into_owned()
            } else {
                target
            };
            links.push((p, target));
        }
    }
    apply_links(&links)
}

fn mkdir_all(p: &Path) -> Result<(), String> {
    std::fs::create_dir_all(p).map_err(|e| crate::paths::os_err("mkdir", &p.to_string_lossy(), &e))
}

/// `applyLinks` : liens symboliques, ou copie de la cible quand le système
/// les refuse.
pub(crate) fn apply_links(links: &[(PathBuf, String)]) -> Result<(), String> {
    for (path, target) in links {
        let _ = std::fs::remove_file(path);
        if let Some(parent) = path.parent() {
            mkdir_all(parent)?;
        }
        #[cfg(unix)]
        if std::os::unix::fs::symlink(target, path).is_ok() {
            continue;
        }
        let src = if Path::new(target).is_absolute() {
            PathBuf::from(target)
        } else {
            path.parent().unwrap_or(Path::new(".")).join(target)
        };
        let Ok(data) = std::fs::read(&src) else {
            continue;
        };
        std::fs::write(path, data)
            .map_err(|e| crate::paths::os_err("open", &path.to_string_lossy(), &e))?;
    }
    Ok(())
}

/// `filepath.Rel` pour deux chemins nettoyés, `base` absolu : `None` si
/// `targ` n'est pas exprimable relativement (l'un absolu, l'autre non).
fn go_rel(base: &str, targ: &str) -> Option<String> {
    if base.starts_with('/') != targ.starts_with('/') {
        return None;
    }
    if base == targ {
        return Some(".".into());
    }
    let b: Vec<&str> = base.split('/').filter(|s| !s.is_empty()).collect();
    let t: Vec<&str> = targ.split('/').filter(|s| !s.is_empty()).collect();
    let common = b.iter().zip(&t).take_while(|(x, y)| x == y).count();
    let mut parts: Vec<&str> = vec![".."; b.len() - common];
    parts.extend(&t[common..]);
    Some(parts.join("/"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rel() {
        assert_eq!(go_rel("/a/b", "/a/b/c").as_deref(), Some("c"));
        assert_eq!(go_rel("/a/b", "/a/c").as_deref(), Some("../c"));
        assert_eq!(go_rel("/a/b", "/a/b").as_deref(), Some("."));
        assert_eq!(go_rel("/a/b", "/a/b..x").as_deref(), Some("../b..x"));
    }

    #[test]
    fn marqueur_version() {
        let tmp = tempfile::tempdir().unwrap();
        let d = dir(tmp.path());
        std::fs::create_dir_all(&d).unwrap();
        for (content, tag, cuda, fmt) in [
            ("b300  fmt2\n", "b300", "", "fmt2"),
            ("b300 12.4 fmt2\n", "b300", "12.4", "fmt2"),
            ("b300 12.4\n", "b300", "12.4", ""),
            ("b300 \n", "b300", "", ""),
            ("fmt2\n", "fmt2", "", ""),
            ("", "", "", ""),
        ] {
            std::fs::write(d.join("VERSION"), content).unwrap();
            assert_eq!(
                version(tmp.path()),
                (tag.into(), cuda.into()),
                "{content:?}"
            );
            assert_eq!(version_format(tmp.path()), fmt, "{content:?}");
        }
    }

    /// ROCm reconnu à son dossier (/opt/rocm), sans hipcc dans le PATH.
    #[tokio::test]
    async fn rocm_par_son_dossier() {
        let root = tempfile::tempdir().unwrap();
        let tools = tempfile::tempdir().unwrap();
        let probe = Probe {
            path: Some(vec![tools.path().to_path_buf()]),
            root: root.path().to_path_buf(),
        };
        let asset = |n: &str| Asset {
            name: n.into(),
            url: String::new(),
            size: 1,
        };
        let assets = [
            asset("llama-b1-bin-ubuntu-rocm-7.0-x64.tar.gz"),
            asset("llama-b1-bin-ubuntu-x64.tar.gz"),
        ];
        let linux = Target {
            os: "linux",
            arm64: false,
        };
        assert_eq!(
            pick(&assets, &probe, linux).await.unwrap().label,
            "CPU (Linux x64)"
        );
        assert_eq!(probe.detect_build_plan().await.backend, "cpu");
        std::fs::create_dir_all(root.path().join("opt/rocm")).unwrap();
        assert_eq!(
            pick(&assets, &probe, linux).await.unwrap().label,
            "ROCm (Linux x64)"
        );
        assert_eq!(probe.detect_build_plan().await.backend, "hip");
    }

    #[test]
    fn reco() {
        assert_eq!(recommended_mode("cuda", "linux")["mode"], "opt");
        assert_eq!(recommended_mode("cuda", "windows")["mode"], "fast");
        assert_eq!(recommended_mode("vulkan", "linux")["why"], "");
    }
}
