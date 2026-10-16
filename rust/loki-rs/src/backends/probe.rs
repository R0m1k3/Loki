//! Sondes de la machine : port de `detectBuildPlan` (le seul plan exposé :
//! backend, architectures CUDA, parallélisme), `findNvcc`, `hasNvidiaGPU`,
//! `detectCudaArch`, `hasTool`, `hasVulkanLib`, `driverCudaVersion` et
//! `detectGPUs` (`backend_build.go`, `backend_prebuilt.go`, `backend_gpu.go`).
//!
//! Les outils sont cherchés dans un PATH et les chemins système sous une
//! racine, tous deux injectables : les tests rejouent une machine sans toucher
//! à l'environnement du processus.

use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::LazyLock;
use std::time::Duration;

use regex::Regex;
use tokio::process::Command;

/// Délai de `nvidiaSmiGPUQuery` (`nvidiaSmiTimeout`).
const NVIDIA_SMI_TIMEOUT: Duration = Duration::from_secs(10);

/// `gpuBaseFields` / `gpuLinkFields`.
const GPU_BASE_FIELDS: &str = "index,name,memory.total,memory.used,compute_cap";
const GPU_LINK_FIELDS: &str = "pci.bus_id,pcie.link.gen.current,pcie.link.gen.max,pcie.link.width.current,pcie.link.width.max,clocks.max.memory";

/// La machine telle que les sondes la voient.
#[derive(Clone, Debug, Default)]
pub struct Probe {
    /// Dossiers où chercher les outils ; `None` : le PATH du processus, relu
    /// à chaque recherche (comme `exec.LookPath`).
    pub path: Option<Vec<PathBuf>>,
    /// Racine des chemins système (`/usr/local/cuda`, `/opt/rocm`…) ; vide :
    /// la vraie racine.
    pub root: PathBuf,
}

/// Le plan de compilation tel que le statut l'expose (`buildPlan`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct BuildPlan {
    pub backend: &'static str,
    pub cuda_arch: String,
    pub jobs: usize,
}

/// `gpuInfo` : une carte vue par nvidia-smi.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct GpuInfo {
    pub index: i64,
    pub name: String,
    pub mem_total: String,
    pub mem_used: String,
    pub cap: String,
    pub bus_id: String,
    pub link_gen: i64,
    pub link_gen_max: i64,
    pub link_width: i64,
    pub link_width_max: i64,
    pub mem_clock_max: i64,
}

impl Probe {
    /// La vraie machine.
    pub fn system() -> Self {
        Probe::default()
    }

    /// Un chemin système sous la racine des sondes.
    pub fn sys(&self, p: &str) -> PathBuf {
        if self.root.as_os_str().is_empty() {
            PathBuf::from(p)
        } else {
            self.root.join(p.trim_start_matches('/'))
        }
    }

    fn dirs(&self) -> Vec<PathBuf> {
        match &self.path {
            Some(p) => p.clone(),
            None => std::env::var_os("PATH")
                .map(|p| std::env::split_paths(&p).collect())
                .unwrap_or_default(),
        }
    }

    /// `exec.LookPath` dans le PATH des sondes.
    pub fn look_path(&self, name: &str) -> Option<PathBuf> {
        if name.contains('/') {
            let p = PathBuf::from(name);
            return is_exec(&p).then_some(p);
        }
        self.dirs()
            .into_iter()
            .map(|d| {
                if d.as_os_str().is_empty() {
                    PathBuf::from(".").join(name)
                } else {
                    d.join(name)
                }
            })
            .find(|p| is_exec(p))
    }

    /// `hasTool`.
    pub fn has_tool(&self, name: &str) -> bool {
        self.look_path(name).is_some()
    }

    /// `exec.Command(name, args…).Output()` : la sortie standard si le
    /// programme est trouvé et sort en succès.
    pub async fn output(&self, name: &str, args: &[&str]) -> Option<Vec<u8>> {
        self.output_in(None, name, args).await
    }

    /// [`Probe::output`] lancé dans `dir`.
    pub async fn output_in(
        &self,
        dir: Option<&Path>,
        name: &str,
        args: &[&str],
    ) -> Option<Vec<u8>> {
        let bin = self.look_path(name)?;
        let mut c = Command::new(bin);
        if let Some(d) = dir {
            c.current_dir(d);
        }
        let out = c
            .args(args)
            .stdin(Stdio::null())
            .stderr(Stdio::null())
            .kill_on_drop(true)
            .output()
            .await
            .ok()?;
        out.status.success().then_some(out.stdout)
    }

    /// `findNvcc` (Linux, macOS) : un vrai toolkit avant le PATH.
    pub fn find_nvcc(&self) -> Option<PathBuf> {
        let p = self.sys("/usr/local/cuda/bin/nvcc");
        if is_file(&p) {
            return Some(p);
        }
        let local = self.sys("/usr/local");
        let mut versioned: Vec<PathBuf> = std::fs::read_dir(&local)
            .map(|rd| {
                rd.flatten()
                    .filter(|e| e.file_name().to_string_lossy().starts_with("cuda-"))
                    .map(|e| e.path().join("bin").join("nvcc"))
                    .filter(|p| std::fs::symlink_metadata(p).is_ok())
                    .collect()
            })
            .unwrap_or_default();
        versioned.sort();
        if let Some(p) = versioned.pop() {
            return Some(p);
        }
        self.look_path("nvcc")
    }

    /// `hasNvidiaGPU`.
    pub async fn has_nvidia_gpu(&self) -> bool {
        if !self.has_tool("nvidia-smi") {
            return false;
        }
        self.output("nvidia-smi", &["-L"])
            .await
            .is_some_and(|o| String::from_utf8_lossy(&o).contains("GPU"))
    }

    /// `detectCudaArch` : « 8.6 » → « 86 », dédoublonnés, joints par « ; ».
    pub async fn detect_cuda_arch(&self) -> String {
        let Some(out) = self
            .output(
                "nvidia-smi",
                &["--query-gpu=compute_cap", "--format=csv,noheader"],
            )
            .await
        else {
            return String::new();
        };
        cuda_archs(&String::from_utf8_lossy(&out))
    }

    /// `hasVulkanLib` : le cache de ldconfig, puis des chemins connus.
    pub async fn has_vulkan_lib(&self) -> bool {
        if let Some(out) = self.output("ldconfig", &["-p"]).await
            && String::from_utf8_lossy(&out).contains("libvulkan.so")
        {
            return true;
        }
        [
            "/usr/lib/x86_64-linux-gnu/libvulkan.so.1",
            "/usr/lib64/libvulkan.so.1",
            "/usr/lib/libvulkan.so.1",
            "/lib64/libvulkan.so.1",
        ]
        .iter()
        .any(|p| is_file(&self.sys(p)))
    }

    /// `detectBuildPlan`, réduit à ce que le statut expose.
    pub async fn detect_build_plan(&self) -> BuildPlan {
        let mut p = BuildPlan {
            backend: "cpu",
            cuda_arch: String::new(),
            jobs: num_jobs(),
        };
        if cfg!(target_os = "macos") {
            p.backend = "metal";
            return p;
        }
        if self.find_nvcc().is_some() && self.has_nvidia_gpu().await {
            p.backend = "cuda";
            p.cuda_arch = self.detect_cuda_arch().await;
            return p;
        }
        if self.has_tool("hipcc") || is_dir(&self.sys("/opt/rocm")) {
            p.backend = "hip";
            return p;
        }
        if self.has_tool("glslc") && (self.has_vulkan_lib().await || self.has_tool("vulkaninfo")) {
            p.backend = "vulkan";
        }
        p
    }

    /// `driverCudaVersion` : la version CUDA du bandeau de nvidia-smi, 0 si
    /// inconnue.
    pub async fn driver_cuda_version(&self) -> f64 {
        let Some(out) = self.output("nvidia-smi", &[]).await else {
            return 0.0;
        };
        static RE: LazyLock<Regex> =
            LazyLock::new(|| Regex::new(r"CUDA Version:\s*([0-9]+\.[0-9]+)").expect("motif"));
        RE.captures(&String::from_utf8_lossy(&out))
            .and_then(|m| m[1].parse().ok())
            .unwrap_or(0.0)
    }

    /// `nvidiaSmiGPUQuery` : (sortie, code de sortie) ; `None` = pas lancé ou
    /// délai dépassé.
    async fn smi_query(&self, fields: &str) -> Option<(Vec<u8>, i32)> {
        let bin = self.look_path("nvidia-smi")?;
        let child = Command::new(bin)
            .arg(format!("--query-gpu={fields}"))
            .arg("--format=csv,noheader,nounits")
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .kill_on_drop(true)
            .spawn()
            .ok()?;
        let out = tokio::time::timeout(NVIDIA_SMI_TIMEOUT, child.wait_with_output())
            .await
            .ok()?
            .ok()?;
        Some((out.stdout, out.status.code().unwrap_or(-1)))
    }

    /// `detectGPUs` : les cartes NVIDIA, champs de liaison compris quand le
    /// pilote les connaît.
    pub async fn detect_gpus(&self) -> Result<Vec<GpuInfo>, String> {
        if !self.has_tool("nvidia-smi") {
            return Err(
                "nvidia-smi introuvable — sélection GPU disponible uniquement sur NVIDIA".into(),
            );
        }
        let mut res = self
            .smi_query(&format!("{GPU_BASE_FIELDS},{GPU_LINK_FIELDS}"))
            .await;
        if let Some((out, code)) = &res
            && *code != 0
            && (String::from_utf8_lossy(out).contains("valid field") || *code == 2)
        {
            res = self.smi_query(GPU_BASE_FIELDS).await;
        }
        let out = match res {
            Some((out, 0)) => out,
            Some((_, code)) => return Err(format!("nvidia-smi a échoué: exit status {code}")),
            None => return Err("nvidia-smi a échoué".into()),
        };
        let gpus = parse_gpu_query(&String::from_utf8_lossy(&out));
        if gpus.is_empty() {
            return Err("aucun GPU NVIDIA détecté".into());
        }
        Ok(gpus)
    }
}

/// Les architectures CUDA d'une sortie `compute_cap` (`detectCudaArch`).
pub fn cuda_archs(out: &str) -> String {
    let mut archs: Vec<String> = Vec::new();
    for line in out.trim().split('\n') {
        let cap = line.trim();
        if cap.is_empty() || cap.to_lowercase().contains("not supported") {
            continue;
        }
        let code = cap.replace('.', "");
        if !code.is_empty() && !archs.contains(&code) {
            archs.push(code);
        }
    }
    archs.join(";")
}

/// `parseGPUQuery`.
pub fn parse_gpu_query(out: &str) -> Vec<GpuInfo> {
    let mut gpus = Vec::new();
    for line in out.trim().split('\n') {
        let parts: Vec<&str> = line.split(',').map(str::trim).collect();
        if parts.len() < 5 {
            continue;
        }
        let mut g = GpuInfo {
            index: parts[0].parse().unwrap_or(0),
            name: parts[1].into(),
            mem_total: parts[2].into(),
            mem_used: parts[3].into(),
            cap: parts[4].into(),
            ..GpuInfo::default()
        };
        if parts.len() >= 11 {
            let b = parts[5];
            if !b.is_empty() && b != "N/A" && !b.starts_with('[') {
                g.bus_id = b.into();
            }
            g.link_gen = smi_num(parts[6]);
            g.link_gen_max = smi_num(parts[7]);
            g.link_width = smi_num(parts[8]);
            g.link_width_max = smi_num(parts[9]);
            g.mem_clock_max = smi_num(parts[10]);
        }
        gpus.push(g);
    }
    gpus
}

/// `smiNum` : « [N/A] », vide ou négatif → 0.
pub fn smi_num(s: &str) -> i64 {
    match s.trim().parse::<i64>() {
        Ok(n) if n >= 0 => n,
        _ => 0,
    }
}

/// `numJobs` : `runtime.NumCPU()`, c'est-à-dire les processeurs de
/// l'affinité du processus (sans les quotas de cgroup, que le Go ignore).
pub fn num_jobs() -> usize {
    #[cfg(target_os = "linux")]
    {
        // SAFETY: cpu_set_t est une simple table de bits, initialisée à zéro
        // avant que sched_getaffinity ne la remplisse.
        unsafe {
            let mut set: libc::cpu_set_t = std::mem::zeroed();
            if libc::sched_getaffinity(0, std::mem::size_of::<libc::cpu_set_t>(), &mut set) == 0 {
                let n = libc::CPU_COUNT(&set);
                if n > 0 {
                    return n as usize;
                }
            }
        }
    }
    std::thread::available_parallelism().map_or(1, |n| n.get())
}

fn is_exec(p: &Path) -> bool {
    std::fs::metadata(p).is_ok_and(|m| {
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            m.is_file() && m.permissions().mode() & 0o111 != 0
        }
        #[cfg(not(unix))]
        {
            m.is_file()
        }
    })
}

/// `isFile`.
pub fn is_file(p: &Path) -> bool {
    std::fs::metadata(p).is_ok_and(|m| !m.is_dir())
}

/// `isDir`.
pub fn is_dir(p: &Path) -> bool {
    std::fs::metadata(p).is_ok_and(|m| m.is_dir())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn archs() {
        assert_eq!(cuda_archs("8.6\n8.9\n8.6\n"), "86;89");
        assert_eq!(cuda_archs("[Not Supported]\n12.0"), "120");
        assert_eq!(cuda_archs(""), "");
    }

    #[test]
    fn requete_gpu() {
        let g = parse_gpu_query(
            "0, A, 100, 1, 8.6, 00000000:01:00.0, 1, 4, 16, 16, 9751\n1, B, 8, 0, 7.5, [N/A], 2, 3, 8, 16, 7001\n2, C, 8, 0, 7.5, N/A, x, 3, 8, 16, 1\n3, D, 1, 0\n4, E, 2, 1, 6.1",
        );
        assert_eq!(g.len(), 4);
        assert_eq!(g[0].bus_id, "00000000:01:00.0");
        assert_eq!((g[0].link_gen, g[0].mem_clock_max), (1, 9751));
        assert_eq!(g[1].bus_id, "");
        assert_eq!(g[2].bus_id, "");
        assert_eq!(g[2].link_gen, 0);
        assert_eq!(
            (g[3].index, g[3].name.as_str(), g[3].bus_id.as_str()),
            (4, "E", "")
        );
    }

    #[test]
    fn smi() {
        assert_eq!(smi_num(" 4 "), 4);
        assert_eq!(smi_num("[N/A]"), 0);
        assert_eq!(smi_num("-1"), 0);
    }

    #[test]
    fn jobs_positif() {
        assert!(num_jobs() >= 1);
    }
}
