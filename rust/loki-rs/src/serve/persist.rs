//! État du slot gardé à travers une bascule de preset
//! (`backend_serve_persist.go`, `SLOT_PERSIST`, off par défaut) : la clé du
//! moteur (empreinte de tout ce qui décide du calcul du cache), le marqueur
//! lu par le process web, le ménage des états gardés.

use std::path::{Path, PathBuf};

use sha2::{Digest, Sha256};

use super::{
    Cfg, SysInfo, cpu_weights, flag_value, get, has_any_flag, is_external, loopback_host,
    spec_mode, spec_user_set, split_mode_key, trim,
};

/// `slotPersistOn`.
pub fn slot_persist_on(cfg: &Cfg) -> bool {
    matches!(
        trim(get(cfg, "SLOT_PERSIST")).to_lowercase().as_str(),
        "on" | "1" | "true" | "yes" | "oui"
    )
}

/// `slotPersistArgEnv`.
pub const SLOT_PERSIST_ARG_ENV: &[&str] = &[
    "LLAMA_ARG_SLOT_SAVE_PATH",
    "LLAMA_ARG_SPEC_TYPE",
    "LLAMA_ARG_SPEC_DRAFT_MODEL",
    "LLAMA_ARG_SPEC_DRAFT_HF_REPO",
];

/// `slotPersistPlan` : (accepté, refus — vide = clé absente).
pub fn slot_persist_plan(cfg: &Cfg, extra: &[String], si: &SysInfo) -> (bool, String) {
    if !slot_persist_on(cfg) {
        return (false, String::new());
    }
    let no = |w: &str| (false, w.to_string());
    if is_external(cfg) {
        return no("preset externe");
    }
    if !si.help.contains("--slot-save-path") {
        return no("ce moteur ne connaît pas --slot-save-path");
    }
    if has_any_flag(extra, &["--slot-save-path"]) || !si.env("LLAMA_ARG_SLOT_SAVE_PATH").is_empty()
    {
        return no("--slot-save-path est déjà réglé à la main");
    }
    if si.slot_dir.is_empty() {
        return no("dossier des slots indisponible");
    }
    if spec_mode(cfg) != "off"
        || spec_user_set(extra, &si.arg_env)
        || !si.env("LLAMA_ARG_SPEC_TYPE").is_empty()
    {
        return no(
            "décodage spéculatif : la sauvegarde du moteur ne garde pas l'état du brouillon",
        );
    }
    if !split_mode_key(cfg).is_empty() {
        return no("SPLIT_MODE : état réparti entre cartes, sauvegarde non validée");
    }
    if !slot_save_safe(cfg, extra, si) {
        return no(
            "moteur joignable par d'autres sans clé d'API (HOST=127.0.0.1 ou une clé d'API)",
        );
    }
    let r = cpu_weights(cfg, extra, &si.arg_env, si.blocks());
    if !r.is_empty() {
        return (false, r);
    }
    (true, String::new())
}

/// `slotSaveSafe` : boucle locale, ou clé d'API exigée.
pub fn slot_save_safe(cfg: &Cfg, extra: &[String], si: &SysInfo) -> bool {
    let mut host = flag_value(extra, &["--host"]);
    if host.is_empty() {
        host = get(cfg, "HOST").into();
    }
    if host.is_empty() {
        host = "0.0.0.0".into();
    }
    loopback_host(&host) || !si.api_key.is_empty()
}

/// `slotPersistNote`.
pub fn slot_persist_note(ok: bool, why: &str) -> String {
    if ok || why.is_empty() {
        return String::new();
    }
    format!("SLOT_PERSIST refusé ({why}) : l'état du slot ne sera pas gardé à la bascule de preset")
}

/// `slotPersistFileRe`.
pub fn slot_persist_file(name: &str) -> bool {
    let Some(rest) = name.strip_prefix("persist-") else {
        return false;
    };
    let Some(rest) = rest.strip_suffix(".bin") else {
        return false;
    };
    let hex = |s: &str, n: usize| {
        s.len() == n && s.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f'))
    };
    match rest.split_once('-') {
        Some((a, b)) => hex(a, 16) && hex(b, 12),
        None => false,
    }
}

pub const SLOT_PERSIST_MAX_FILES: usize = 2;

fn mod_nanos(m: &std::fs::Metadata) -> i128 {
    m.modified()
        .ok()
        .and_then(|t| t.duration_since(std::time::UNIX_EPOCH).ok())
        .map_or(0, |d| d.as_nanos() as i128)
}

/// `slotPersistKey` : empreinte (16 hex) de la ligne finale (sans la clé
/// d'API), des variables LLAMA_ARG_/GGML_/CUDA_ (environnement puis `env`),
/// des fichiers (taille, date) et du build.
pub fn slot_persist_key(
    args: &[String],
    env: &std::collections::BTreeMap<String, String>,
    proc_env: &[(String, String)],
    si: &SysInfo,
    bin: &str,
) -> String {
    let mut h = Sha256::new();
    let mut w = |s: &str| {
        h.update(s.as_bytes());
        h.update([0u8]);
    };
    let mut i = 0;
    while i < args.len() {
        if args[i] == "--api-key" {
            i += 2;
            continue;
        }
        w(&args[i]);
        i += 1;
    }
    let mut vars = std::collections::BTreeMap::new();
    for (k, v) in proc_env {
        if k.starts_with("LLAMA_ARG_") || k.starts_with("GGML_") || k.starts_with("CUDA_") {
            vars.insert(k.clone(), v.clone());
        }
    }
    for (k, v) in env {
        vars.insert(k.clone(), v.clone());
    }
    vars.remove("LLAMA_ARG_API_KEY");
    for (k, v) in &vars {
        w(&format!("{k}={v}"));
    }
    let mut stat = |p: &str| {
        if p.is_empty() {
            return;
        }
        match std::fs::metadata(p) {
            Ok(m) => w(&format!("{p}|{}|{}", m.len(), mod_nanos(&m))),
            Err(_) => w(&format!("{p}|?")),
        }
    };
    if !si.model.is_empty() {
        let mp = Path::new(&si.model);
        let dir = mp.parent().unwrap_or(Path::new("."));
        let base = mp
            .file_name()
            .map(|n| n.to_string_lossy().into_owned())
            .unwrap_or_default();
        for n in crate::gguf::shard_family(&base) {
            stat(&dir.join(n).to_string_lossy());
        }
        stat(&si.model);
    }
    stat(&si.mmproj);
    stat(&si.draft);
    stat(bin);
    let bdir = Path::new(bin).parent().unwrap_or(Path::new("."));
    if let Ok(rd) = std::fs::read_dir(bdir) {
        let mut names: Vec<String> = rd
            .flatten()
            .map(|e| e.file_name().to_string_lossy().into_owned())
            .collect();
        names.sort();
        for name in names {
            let n = name.to_lowercase();
            if n.ends_with(".so")
                || n.contains(".so.")
                || n.ends_with(".dll")
                || n.ends_with(".dylib")
            {
                stat(&bdir.join(&name).to_string_lossy());
            }
        }
    }
    w(&format!("build={}", si.engine_build));
    let d = h.finalize();
    d.iter().map(|b| format!("{b:02x}")).collect::<String>()[..16].to_string()
}

/// `slotPersistMarker`.
pub fn slot_persist_marker(home: &Path) -> PathBuf {
    home.join("slots-engine.key")
}

/// `writeSlotPersistMarker`.
pub fn write_slot_persist_marker(home: &Path, key: &str) {
    let p = slot_persist_marker(home);
    if key.is_empty() {
        let _ = std::fs::remove_file(p);
        return;
    }
    let _ = super::run::write_private(&p, format!("{key}\n").as_bytes());
}

/// `slotPersistPrune` : garde les deux états les plus récents.
pub fn slot_persist_prune(dir: &Path) {
    let Ok(rd) = std::fs::read_dir(dir) else {
        return;
    };
    let mut fs: Vec<(String, i128)> = rd
        .flatten()
        .filter(|e| e.file_type().is_ok_and(|t| t.is_file()))
        .filter_map(|e| {
            let n = e.file_name().to_string_lossy().into_owned();
            if !slot_persist_file(&n) {
                return None;
            }
            e.metadata().ok().map(|m| (n, mod_nanos(&m)))
        })
        .collect();
    fs.sort_by_key(|f| std::cmp::Reverse(f.1));
    for (n, _) in fs.iter().skip(SLOT_PERSIST_MAX_FILES) {
        let _ = std::fs::remove_file(dir.join(n));
    }
}

/// `slotPersistPurge`.
pub fn slot_persist_purge(dir: &Path) {
    let Ok(rd) = std::fs::read_dir(dir) else {
        return;
    };
    for e in rd.flatten() {
        let n = e.file_name().to_string_lossy().into_owned();
        if e.file_type().is_ok_and(|t| t.is_file()) && slot_persist_file(&n) {
            let _ = std::fs::remove_file(dir.join(n));
        }
    }
}

/// `prepareSlotDir` : LOKI_HOME/slots (0700) vidé, sauf les états gardés
/// quand SLOT_PERSIST est demandé ; vide = indisponible.
pub fn prepare_slot_dir(home: &Path, keep_persist: bool) -> String {
    let dir = home.join("slots");
    let mut b = std::fs::DirBuilder::new();
    b.recursive(true);
    #[cfg(unix)]
    {
        // MkdirAll(0700) : seuls les dossiers créés reçoivent le mode.
        use std::os::unix::fs::DirBuilderExt;
        b.mode(0o700);
    }
    if b.create(&dir).is_err() {
        return String::new();
    }
    match std::fs::metadata(&dir) {
        Ok(m) if m.is_dir() => {}
        _ => return String::new(),
    }
    if let Ok(rd) = std::fs::read_dir(&dir) {
        for e in rd.flatten() {
            let n = e.file_name().to_string_lossy().into_owned();
            if e.file_type().is_ok_and(|t| t.is_file()) && !(keep_persist && slot_persist_file(&n))
            {
                let _ = std::fs::remove_file(dir.join(n));
            }
        }
    }
    dir.to_string_lossy().into_owned()
}
