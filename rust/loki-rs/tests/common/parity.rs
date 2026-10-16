//! Aides des rejeux de parité qui posent des fichiers et lisent une
//! arborescence (`parityApplyFiles`, `parityTarGz`, `parityTree` du Go).

use std::path::Path;

use serde_json::Value;

fn s(v: &Value) -> &str {
    v.as_str().unwrap_or("")
}

/// Une archive .tar.gz ; les noms sont écrits tels quels (« ../x » compris).
pub fn tar_gz(entries: &[Value]) -> Vec<u8> {
    let gz = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::default());
    let mut b = tar::Builder::new(gz);
    for e in entries {
        let mut h = tar::Header::new_ustar();
        let name = s(&e["name"]).as_bytes();
        h.as_old_mut().name[..name.len()].copy_from_slice(name);
        let link = s(&e["link"]).as_bytes();
        h.as_old_mut().linkname[..link.len()].copy_from_slice(link);
        h.set_mtime(1_700_000_000);
        h.set_mode(e["mode"].as_u64().unwrap_or(0) as u32);
        let body = s(&e["body"]).as_bytes().to_vec();
        match s(&e["type"]) {
            "dir" => h.set_entry_type(tar::EntryType::Directory),
            "symlink" => {
                h.set_entry_type(tar::EntryType::Symlink);
                h.set_mode(0o777);
            }
            "hardlink" => h.set_entry_type(tar::EntryType::Link),
            _ => {
                h.set_entry_type(tar::EntryType::Regular);
                h.set_size(body.len() as u64);
            }
        }
        if s(&e["type"]) != "file" {
            h.set_size(0);
        }
        h.set_cksum();
        let data: &[u8] = if s(&e["type"]) == "file" { &body } else { &[] };
        b.append(&h, data).unwrap();
    }
    b.into_inner().unwrap().finish().unwrap()
}

pub fn write_file(p: &Path, content: &str, mode: u32) {
    use std::os::unix::fs::OpenOptionsExt;
    let _ = std::fs::remove_file(p);
    let mut f = std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(mode)
        .open(p)
        .unwrap();
    std::io::Write::write_all(&mut f, content.as_bytes()).unwrap();
}

pub fn remove_all(p: &Path) {
    match std::fs::symlink_metadata(p) {
        Ok(m) if m.is_dir() => std::fs::remove_dir_all(p).unwrap(),
        Ok(_) => std::fs::remove_file(p).unwrap(),
        Err(_) => {}
    }
}

/// Les fichiers d'une étape (`parityApplyFiles`).
pub fn apply_files(home: &Path, files: &Value) {
    for (n, v) in files.as_object().into_iter().flatten() {
        let v = s(v);
        let p = home.join(n.trim_end_matches('/'));
        match v {
            "-" => {
                remove_all(&p);
                continue;
            }
            "/" => {
                std::fs::create_dir_all(&p).unwrap();
                continue;
            }
            _ => {}
        }
        std::fs::create_dir_all(p.parent().unwrap()).unwrap();
        if let Some(target) = v.strip_prefix("-> ") {
            let _ = std::fs::remove_file(&p);
            std::os::unix::fs::symlink(target, &p).unwrap();
            continue;
        }
        write_file(&p, v, if v.starts_with("#!") { 0o755 } else { 0o644 });
    }
}

/// `info.Mode().Perm().String()`.
pub fn perm_string(mode: u32) -> String {
    let mut out = String::from("-");
    for shift in [6, 3, 0] {
        let b = (mode >> shift) & 7;
        out.push(if b & 4 != 0 { 'r' } else { '-' });
        out.push(if b & 2 != 0 { 'w' } else { '-' });
        out.push(if b & 1 != 0 { 'x' } else { '-' });
    }
    out
}

/// `parityTree`.
pub fn tree(home: &Path) -> Vec<String> {
    use std::os::unix::fs::PermissionsExt;
    fn walk(home: &Path, p: &Path, out: &mut Vec<String>) {
        let Ok(m) = std::fs::symlink_metadata(p) else {
            return;
        };
        let rel = p.strip_prefix(home).unwrap().to_string_lossy().into_owned();
        if rel.contains("/.git/") || rel.ends_with("/.git") {
            return;
        }
        if m.file_type().is_symlink() {
            let t = std::fs::read_link(p).unwrap();
            out.push(format!("l {rel} -> {}", t.to_string_lossy()));
        } else if m.is_dir() {
            out.push(format!("d {rel}"));
            for e in std::fs::read_dir(p).unwrap().flatten() {
                walk(home, &e.path(), out);
            }
        } else {
            let b = std::fs::read(p).unwrap();
            out.push(format!(
                "f {rel} {} {}",
                perm_string(m.permissions().mode() & 0o777),
                String::from_utf8_lossy(&b)
            ));
        }
    }
    let mut out = Vec::new();
    for root in ["backends", "engine"] {
        walk(home, &home.join(root), &mut out);
    }
    out.sort();
    out
}
