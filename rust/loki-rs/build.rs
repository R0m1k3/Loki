//! Embarque l'UI web dans le binaire, depuis la MÊME source que le `go:embed`
//! du serveur Go (`internal/loki/ui/`) : les deux binaires d'un build servent
//! donc toujours la même interface. Génère `$OUT_DIR/ui_assets.rs`.

use std::fmt::Write as _;
use std::path::PathBuf;

fn main() {
    let manifest = PathBuf::from(std::env::var("CARGO_MANIFEST_DIR").unwrap());
    let ui = manifest.join("../../internal/loki/ui");
    let ui = ui
        .canonicalize()
        .unwrap_or_else(|e| panic!("UI introuvable ({}) : {e}", ui.display()));
    println!("cargo:rerun-if-changed={}", ui.display());

    let mut out = String::new();
    for name in [
        "index.html",
        "marked.min.js",
        "purify.min.js",
        "sw.js",
        "manifest.webmanifest",
    ] {
        let p = ui.join(name);
        assert!(p.is_file(), "fichier d'UI manquant : {}", p.display());
        println!("cargo:rerun-if-changed={}", p.display());
        let ident = name.replace(['.', '-'], "_").to_uppercase();
        writeln!(out, "pub static {ident}: &[u8] = include_bytes!({:?});", p).unwrap();
    }

    let fonts_dir = ui.join("fonts");
    println!("cargo:rerun-if-changed={}", fonts_dir.display());
    let mut fonts: Vec<PathBuf> = std::fs::read_dir(&fonts_dir)
        .unwrap_or_else(|e| panic!("{} : {e}", fonts_dir.display()))
        .filter_map(|e| e.ok().map(|e| e.path()))
        .filter(|p| p.extension().is_some_and(|x| x == "woff2"))
        .collect();
    fonts.sort();
    out.push_str("pub static FONTS: &[(&str, &[u8])] = &[\n");
    for p in &fonts {
        let name = p.file_name().unwrap().to_str().unwrap();
        writeln!(out, "    ({name:?}, include_bytes!({p:?})),").unwrap();
    }
    out.push_str("];\n");

    let dst = PathBuf::from(std::env::var("OUT_DIR").unwrap()).join("ui_assets.rs");
    std::fs::write(dst, out).unwrap();
}
