//! Tests unitaires des outils d'image (lot 45) ; la parité avec le Go est
//! dans `tests/imagetools.rs`.

use super::*;

#[test]
fn types_d_image() {
    assert_eq!(image_mime("a.PNG"), "image/png");
    assert_eq!(image_mime("/x/b.jpeg"), "image/jpeg");
    assert_eq!(image_mime("c.Jpg"), "image/jpeg");
    assert_eq!(image_mime("d.gif"), "image/gif");
    assert_eq!(image_mime("e.webp"), "image/webp");
    assert_eq!(image_mime("f.bmp"), "image/bmp");
    assert_eq!(image_mime("g.svg"), "");
    assert_eq!(image_mime("png"), "");
    assert_eq!(image_mime(""), "");
}

#[test]
fn noms_de_capture() {
    assert_eq!(safe_slug("localhost:3000"), "localhost-3000");
    assert_eq!(safe_slug("www.exemple.test"), "www.exemple.test");
    assert_eq!(safe_slug("..--a b--.."), "a-b");
    assert_eq!(safe_slug("été"), "t");
    assert_eq!(safe_slug("::"), "page");
    assert_eq!(safe_slug(""), "page");
    assert_eq!(safe_slug(&"x".repeat(60)).len(), 40);
}

#[test]
fn adresses_locales() {
    for u in [
        "http://localhost:3000/app",
        "http://127.0.0.1/",
        "http://[::1]:8080/",
        "http://0.0.0.0:5173",
        "https://app.localhost/",
        "http://moi:secret@localhost/",
    ] {
        assert!(is_local_url(u), "{u}");
    }
    for u in [
        "https://exemple.test/",
        "http://localhost.exemple.test/",
        "http://LOCALHOST/",
        "http://127.0.0.2/",
        "pas une url",
        "http://localhost:abc/",
    ] {
        assert!(!is_local_url(u), "{u}");
    }
}

#[test]
fn dernieres_lignes() {
    assert_eq!(last_lines("a\n\nb\n  \nc\nd\n", 3), "b · c · d");
    assert_eq!(last_lines("seule", 6), "seule");
    assert_eq!(last_lines("", 6), "");
    assert_eq!(last_lines("\n \n", 6), "");
}

#[test]
fn image_preparee() {
    // Petite PNG : rien à faire, les octets d'origine.
    let mut png = Vec::new();
    image::RgbaImage::from_pixel(10, 5, image::Rgba([1, 2, 3, 255]))
        .write_to(&mut std::io::Cursor::new(&mut png), image::ImageFormat::Png)
        .unwrap();
    assert_eq!(
        prepare_image_for_model(&png, "image/png"),
        (png.clone(), "image/png".into())
    );
    // Indécodable : telle quelle.
    assert_eq!(
        prepare_image_for_model(b"pas une image", "image/png"),
        (b"pas une image".to_vec(), "image/png".into())
    );
    // Trop grande : réduite sous 1568 px, en PNG.
    let mut big = Vec::new();
    image::RgbaImage::from_pixel(3136, 100, image::Rgba([9, 9, 9, 255]))
        .write_to(&mut std::io::Cursor::new(&mut big), image::ImageFormat::Png)
        .unwrap();
    let (b, mime) = prepare_image_for_model(&big, "image/png");
    assert_eq!(mime, "image/png");
    let img = image::load_from_memory(&b).unwrap();
    assert_eq!((img.width(), img.height()), (1568, 50));
    let part = image_url_part(b"ab", "image/gif");
    assert_eq!(part["image_url"]["url"], "data:image/gif;base64,YWI=");
}

#[test]
fn menage_des_captures() {
    let d = tempfile::tempdir().unwrap();
    let base = SystemTime::now() - Duration::from_secs(1000);
    for i in 0..25u64 {
        let p = d.path().join(format!("c{i:02}.jpg"));
        std::fs::write(&p, "x").unwrap();
        std::fs::File::options()
            .write(true)
            .open(&p)
            .unwrap()
            .set_modified(base + Duration::from_secs(i))
            .unwrap();
    }
    // La plus ancienne est celle qu'on vient de prendre : elle reste.
    let keep = d.path().join("c00.jpg");
    prune_captures(d.path(), &keep);
    let mut left: Vec<String> = std::fs::read_dir(d.path())
        .unwrap()
        .flatten()
        .map(|e| e.file_name().to_string_lossy().into_owned())
        .collect();
    left.sort();
    assert_eq!(left.len(), 20);
    assert_eq!(left[0], "c00.jpg");
    assert_eq!(left[1], "c06.jpg");
}

#[tokio::test]
async fn capture_refusee_avant_playwright() {
    let d = tempfile::tempdir().unwrap();
    let args = |v: Value| serde_json::from_value::<Map<String, Value>>(v).unwrap();
    let s = web_screenshot(&args(json!({"url": "http://x/"})), "", d.path(), true, "t").await;
    assert!(
        s.result.contains("Playwright n'est pas installé"),
        "{}",
        s.result
    );
    let s = web_screenshot(&args(json!({"url": 3})), "/bin/false", d.path(), true, "t").await;
    assert_eq!(s.result, "[erreur] url manquante");
    assert!(s.rel.is_empty());
    assert!(!d.path().join("captures").exists());
}

#[tokio::test]
async fn voir_sans_poste_distant() {
    let d = tempfile::tempdir().unwrap();
    let (r, img) = see_image("a.png", d.path(), "nœud", true, async { true }).await;
    assert!(r.contains("poste distant"), "{r}");
    assert!(img.is_none());
    let (r, _) = see_image("", d.path(), "", true, async { true }).await;
    assert_eq!(r, "[erreur] chemin de fichier manquant");
}

#[test]
fn horodatage_local() {
    let s = local_stamp();
    assert_eq!(s.len(), 15);
    assert_eq!(&s[8..9], "-");
}
