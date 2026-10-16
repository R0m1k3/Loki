use super::*;

#[test]
fn outils_et_plafond() {
    for n in [
        "browser_open",
        "browser_snapshot",
        "browser_find",
        "browser_click",
        "browser_click_xy",
        "browser_type",
        "browser_key",
        "browser_scroll",
        "browser_screenshot",
    ] {
        assert!(is_tool(n), "{n}");
    }
    assert!(!is_tool("web_screenshot"));
    assert!(!auto_shot_tool("browser_snapshot") && !auto_shot_tool("browser_screenshot"));
    assert!(auto_shot_tool("browser_find"));
    let s = "é".repeat(MAX_OUTPUT);
    assert_eq!(cap_output(&s), s);
    assert_eq!(cap_output(&format!("{s}x")), format!("{s}\n…[tronqué]"));
}

#[test]
fn nombres_comme_le_go() {
    assert_eq!(go_num(640.0).to_string(), "640");
    assert_eq!(go_num(412.5).to_string(), "412.5");
    assert_eq!(go_num(-3.0).to_string(), "-3");
}

#[test]
fn arguments_de_lancement() {
    let a = launch_args(Path::new("/tmp/p"), false);
    assert_eq!(a[0], "--headless=new");
    assert!(a.contains(&"--user-data-dir=/tmp/p".to_string()));
    assert_eq!(a.last().unwrap(), "about:blank");
    let h = launch_args(Path::new("/tmp/p"), true);
    assert_eq!(h.len(), a.len() - 1);
    assert_eq!(h[0], "--remote-debugging-port=0");
}

/// La grille : lignes translucides tous les 100 px, étiquettes opaques,
/// rien d'autre de touché.
#[test]
fn grille() {
    let mut img = image::RgbaImage::from_pixel(250, 150, image::Rgba([40, 80, 120, 255]));
    grid::draw(&mut img);
    let px = |x, y| img.get_pixel(x, y).0;
    assert_eq!(px(0, 0), [40, 80, 120, 255]);
    // 255×130/255 + 40×125/255 → 149 ; 0 + 80×(1−a) → 39 ; 200a + 120(1−a) → 160.
    assert_eq!(px(100, 50), [149, 39, 160, 255]);
    assert_eq!(px(200, 140), [149, 39, 160, 255]);
    assert_eq!(px(50, 100), [149, 39, 160, 255]);
    assert_eq!(px(101, 50), [40, 80, 120, 255]);
    // « 100 » en haut de la ligne : fond sombre, chiffre jaune.
    assert_eq!(px(100, 1), [0, 0, 0, 255]);
    assert_eq!(px(104, 4), [255, 255, 0, 255]);
    // Une image illisible passe telle quelle.
    assert_eq!(grid::overlay_grid(b"pas une image"), b"pas une image");
}

#[test]
fn profils_temporaires() {
    let tmp = tempfile::tempdir().unwrap();
    let p = mkdtemp(tmp.path()).unwrap();
    assert!(
        p.file_name()
            .unwrap()
            .to_string_lossy()
            .starts_with("loki-cu-")
    );
    assert!(p.is_dir());
    // Récent : gardé ; un autre dossier : jamais touché.
    std::fs::create_dir(tmp.path().join("autre")).unwrap();
    sweep_stale(tmp.path());
    assert!(p.is_dir() && tmp.path().join("autre").is_dir());
}

#[tokio::test]
async fn lancement_impossible() {
    let tmp = tempfile::tempdir().unwrap();
    let b = Browser::with("", tmp.path().to_path_buf());
    let mut a = Map::new();
    a.insert("url".into(), json!("x.test"));
    let o = b.exec("browser_open", &a, false, "").await.unwrap();
    // Sans binaire imposé ni trouvé, ou introuvable : l'erreur dit quoi faire.
    assert!(o.result.starts_with("[erreur] "), "{}", o.result);
    let b = Browser::with("/nulle/part/chrome", tmp.path().to_path_buf());
    let o = b
        .exec("browser_snapshot", &Map::new(), false, "")
        .await
        .unwrap();
    assert_eq!(
        o.result,
        "[erreur] fork/exec /nulle/part/chrome: no such file or directory"
    );
    // Le profil du lancement raté est retiré.
    assert_eq!(std::fs::read_dir(tmp.path()).unwrap().count(), 0);
    assert_eq!(b.exec("web_open", &a, false, "").await, None);
    assert!(b.shot("rien").is_none());
}
