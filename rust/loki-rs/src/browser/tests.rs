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

/// Pages d'erreur ou de blocage : reconnues à leur titre, dans les premières
/// lignes seulement.
#[test]
fn pages_de_blocage() {
    for t in [
        "# Access Denied\nhttps://boutique.test/",
        "# Just a moment… captcha\nhttps://x.test/",
        "[ok] cliqué [4]\n\n# Accès refusé\nhttps://x.test/",
        "# 404 - Page introuvable\nhttps://x.test/a",
        "# Are you human?\nhttps://x.test/",
        "# Attention Required! | Cloudflare — you have been blocked\nu",
    ] {
        assert!(looks_blocked(t), "{t}");
        assert!(guard(t.to_string(), 1).contains("page d'erreur ou de blocage"));
    }
    for t in [
        "# Boutique — robes d'été\nhttps://x.test/",
        "[ok] cliqué [2]\n\n(page inchangée depuis le dernier affichage — les mêmes numéros restent valides)",
        // Le mot dans le corps, pas dans un titre.
        "# Panier\nhttps://x.test/\n\n[1] link \"Accès refusé ? aide\"",
        // Titre trop loin : au-delà des six premières lignes.
        "a\nb\nc\nd\ne\nf\n# Access denied",
    ] {
        assert!(!looks_blocked(t), "{t}");
    }
    assert_eq!(guard("# Accueil\nu".into(), 3), "# Accueil\nu");
}

/// Le rappel de conclure arrive à la 25e action du tour, et ensuite.
#[test]
fn rappel_de_conclure() {
    assert_eq!(ACTION_BUDGET, 25);
    assert_eq!(guard("[ok]".into(), 24), "[ok]");
    let r = guard("[ok]".into(), 25);
    assert!(
        r.starts_with("[ok]\n\n⏱ 25 actions dans le navigateur"),
        "{r}"
    );
    assert!(guard("[ok]".into(), 31).contains("⏱ 31 actions"));
    // Les deux rappels ensemble, le blocage d'abord.
    let r = guard("# Forbidden\nu".into(), 30);
    let (b, c) = (r.find("⚠ Le site").unwrap(), r.find("⏱ 30").unwrap());
    assert!(b < c);
}

/// Même action sans effet : avertie dès la deuxième fois ; un effet, ou une
/// autre action, remet le compte à zéro.
#[test]
fn action_repetee() {
    let mut r = Repeat::default();
    let same = "[ok] cliqué [3]\n\n(page inchangée depuis le dernier affichage — les mêmes numéros restent valides)";
    assert_eq!(r.note("click 3", same.into()), same);
    assert!(r.note("click 3", same.into()).ends_with("ce qui bloque)."));
    assert!(r.note("click 3", same.into()).contains("sans effet 3 fois"));
    // Une autre action sans effet repart de 1.
    assert_eq!(r.note("click 4", same.into()), same);
    let refused = format!("[refusé] [4] {CLICK_DISABLED}");
    assert!(
        r.note("click 4", refused.clone())
            .contains("sans effet 2 fois")
    );
    // Une action qui change la page efface tout.
    assert_eq!(
        r.note("click 4", "[ok] cliqué [4]\n\n# A".into()),
        "[ok] cliqué [4]\n\n# A"
    );
    assert_eq!(r.note("click 4", refused.clone()), refused);
}

/// `browser_find` : éléments (avec leurs états) puis texte simple à lire.
#[test]
fn recherche_elements_et_texte() {
    let refs = vec![
        "[12] radio \"256 Go\" [coché]".to_string(),
        "[13] radio \"512 Go\"".to_string(),
        "[20] button \"Continuer\" [désactivé]".to_string(),
    ];
    let texts = vec!["Livraison estimée : 12 nov.".to_string()];
    assert_eq!(
        format_find("go", &refs, &[]),
        "3 élément(s) contenant « go » (le 1er est amené dans la vue) — clique par numéro :\n[12] radio \"256 Go\" [coché]\n[13] radio \"512 Go\"\n[20] button \"Continuer\" [désactivé]"
    );
    assert_eq!(
        format_find("livraison", &[], &texts),
        "Texte de la page contenant « livraison » (à lire, pas à cliquer) :\n- Livraison estimée : 12 nov."
    );
    let both = format_find("x", &refs[..1], &[texts[0].clone(), "Prix : 9 €".into()]);
    assert!(
        both.contains("[coché]\n\nTexte de la page contenant « x »"),
        "{both}"
    );
    assert!(both.ends_with(":\n- Livraison estimée : 12 nov.\n- Prix : 9 €"));
    assert!(format_find("zz", &[], &[]).starts_with("Rien ne contient « zz » sur cette page"));
}

/// Les scripts injectés : la case cachée passe par sa cible visible, et
/// chaque ligne de la liste porte l'état de l'élément.
#[test]
fn scripts_cases_cachees() {
    let snap = js::snapshot_js();
    assert!(snap.contains("function ajTarget(el)"));
    assert!(snap.contains("ajActionable(el)&&ajVis(ajTarget(el))"));
    assert!(snap.contains("var r=ajTarget(el).getBoundingClientRect()"));
    assert!(snap.contains("+ajState(el));"));
    for st in ["' [coché]'", "' [sélectionné]'", "' [désactivé]'"] {
        assert!(js::HELPERS_JS.contains(st), "{st}");
    }
    assert!(js::FIND_JS.contains("return {refs:refs,texts:texts};"));
    assert!(js::FIND_JS.contains("+ajState(el));"));
}
