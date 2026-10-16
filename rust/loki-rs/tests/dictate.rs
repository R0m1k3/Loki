//! Lot 9 : dictée vocale tenue par la façade (`LOKI_DICTATE=rust`).
//!
//! - le journal tiré du Go (`internal/loki/parity_dictate_test.go` →
//!   `tests/parity_dictate/dictate.json`) : lecture des WAV, chemins
//!   d'archive, réglages, catalogue, arguments du serveur, routes ;
//! - le protocole WebSocket de sherpa-onnx contre un faux serveur ;
//! - l'extraction d'une archive et le téléchargement d'un modèle ;
//! - la supervision d'un faux sherpa-onnx (script Python) par la façade.

mod common;

use std::convert::Infallible;
use std::net::SocketAddr;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use bytes::Bytes;
use common::fakego;
use futures_util::{SinkExt, StreamExt};
use http_body_util::Full;
use hyper::body::Incoming;
use hyper::{Request, Response, StatusCode, service::service_fn};
use hyper_util::rt::TokioIo;
use loki_rs::dictate::{self, DictateCfg, Dictation};
use serde_json::Value;
use tokio_tungstenite::tungstenite::Message;

fn fixture() -> Value {
    serde_json::from_str(include_str!("parity_dictate/dictate.json")).expect("journal")
}

fn b64(s: &str) -> Vec<u8> {
    const T: &[u8] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut out = Vec::new();
    let mut acc = 0u32;
    let mut n = 0;
    for c in s.bytes().filter(|c| *c != b'=') {
        acc = acc << 6 | T.iter().position(|t| *t == c).unwrap() as u32;
        n += 6;
        if n >= 8 {
            n -= 8;
            out.push((acc >> n) as u8);
            acc &= (1 << n) - 1;
        }
    }
    out
}

/// v2 installé (quatre fichiers non vides), v3 à moitié — comme le journal.
fn install_models(home: &Path) {
    for f in dictate::ASR_FICHIERS {
        let p = dictate::model_dir_for(home, "parakeet-tdt-0.6b-v2")
            .unwrap()
            .join(f);
        std::fs::create_dir_all(p.parent().unwrap()).unwrap();
        std::fs::write(p, b"x").unwrap();
    }
    let v3 = dictate::model_dir_for(home, "parakeet-tdt-0.6b-v3").unwrap();
    std::fs::create_dir_all(&v3).unwrap();
    std::fs::write(v3.join("encoder.int8.onnx"), b"x").unwrap();
    std::fs::write(v3.join("tokens.txt"), b"").unwrap();
}

#[test]
fn parite_des_fonctions_pures() {
    let fx = fixture();
    for w in fx["wav"].as_array().unwrap() {
        let input = b64(w["in"].as_str().unwrap_or(""));
        let name = w["name"].as_str().unwrap();
        match dictate::wav_to_f32(&input) {
            Err(e) => assert_eq!(e, w["err"].as_str().unwrap_or("<ok attendu>"), "{name}"),
            Ok((rate, ech)) => {
                assert!(w.get("err").is_none(), "{name} : erreur attendue");
                assert_eq!(rate as u64, w["rate"].as_u64().unwrap(), "{name}");
                let want: Vec<f32> = w
                    .get("ech")
                    .and_then(Value::as_array)
                    .map(|a| a.iter().map(|x| x.as_f64().unwrap() as f32).collect())
                    .unwrap_or_default();
                assert_eq!(ech, want, "{name}");
            }
        }
    }
    for c in fx["safe_rel"].as_array().unwrap() {
        let input = c["in"].as_str().unwrap();
        match dictate::safe_rel(input) {
            Ok(o) => assert_eq!(o, c["out"].as_str().unwrap_or(""), "{input:?}"),
            Err(e) => assert_eq!(e, c["err"].as_str().unwrap_or("<ok attendu>"), "{input:?}"),
        }
    }
    for (s, want) in fx["marker"].as_object().unwrap() {
        assert_eq!(dictate::marqueur(s), want.as_bool().unwrap(), "{s:?}");
    }
    for c in fx["defaults"].as_array().unwrap() {
        let d = DictateCfg {
            model: c["model"].as_str().unwrap().into(),
            reactivity: c["reactivity"].as_str().unwrap().into(),
        }
        .with_defauts();
        assert_eq!(d.model, c["out_model"].as_str().unwrap());
        assert_eq!(d.reactivity, c["out_reactivity"].as_str().unwrap());
    }
    for (s, want) in fx["last_line"].as_object().unwrap() {
        assert_eq!(dictate::last_line(s), want.as_str().unwrap(), "{s:?}");
    }
    let home = PathBuf::from("/maison");
    let cfg = DictateCfg {
        model: "parakeet-tdt-0.6b-v3".into(),
        ..Default::default()
    };
    let args: Vec<String> = dictate::server_args(&home, &cfg, 4567)
        .unwrap()
        .iter()
        .map(|a| a.replace("/maison", "{{HOME}}"))
        .collect();
    let want: Vec<String> = serde_json::from_value(fx["args"].clone()).unwrap();
    assert_eq!(args, want);
    let bad = DictateCfg {
        model: "x".into(),
        ..Default::default()
    };
    assert_eq!(
        dictate::server_args(&home, &bad, 1).unwrap_err(),
        fx["args_err"].as_str().unwrap()
    );
    for (id, url) in fx["urls"].as_object().unwrap() {
        assert_eq!(
            dictate::model_url_for(dictate::ASR_MODEL_BASE, id),
            url.as_str().unwrap(),
            "{id}"
        );
    }
    let tmp = tempfile::tempdir().unwrap();
    install_models(tmp.path());
    for (id, want) in fx["present"].as_object().unwrap() {
        assert_eq!(
            dictate::model_present(tmp.path(), id),
            want.as_bool().unwrap(),
            "{id}"
        );
    }
}

#[tokio::test]
async fn parite_des_routes() {
    let fx = fixture();
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().canonicalize().unwrap();
    install_models(&home);
    let d = Arc::new(Dictation::with(
        &home,
        "http://127.0.0.1:1/",
        Some("/nexiste/pas".into()),
        Duration::from_secs(60),
        Duration::from_secs(5),
    ));
    let (facade, _go, _stop) = fakego::start_dictate(home.clone(), d).await;
    let h = home.to_string_lossy().into_owned();
    for (i, c) in fx["calls"].as_array().unwrap().iter().enumerate() {
        let (m, p) = (c["method"].as_str().unwrap(), c["path"].as_str().unwrap());
        let body = c.get("body").and_then(Value::as_str).unwrap_or("");
        let (code, b) = fakego::send(facade, m, p, &[], body).await;
        let got: Value =
            serde_json::from_str(&String::from_utf8_lossy(&b).replace(&h, "{{HOME}}")).unwrap();
        assert_eq!(
            code.as_u16() as u64,
            c["status"].as_u64().unwrap(),
            "appel {i} : {m} {p} {body}"
        );
        assert_eq!(got, c["out"], "appel {i} : {m} {p} {body}");
    }
}

// ---------------------------------------------------------------------------
// Protocole
// ---------------------------------------------------------------------------

fn wav16(n: usize) -> Vec<u8> {
    let data: Vec<u8> = (0..n)
        .flat_map(|i| ((i as i16).wrapping_mul(7)).to_le_bytes())
        .collect();
    let mut b = b"RIFF\0\0\0\0WAVEfmt ".to_vec();
    b.extend_from_slice(&16u32.to_le_bytes());
    b.extend_from_slice(&1u16.to_le_bytes());
    b.extend_from_slice(&1u16.to_le_bytes());
    b.extend_from_slice(&16000u32.to_le_bytes());
    b.extend_from_slice(&32000u32.to_le_bytes());
    b.extend_from_slice(&2u16.to_le_bytes());
    b.extend_from_slice(&16u16.to_le_bytes());
    b.extend_from_slice(b"data");
    b.extend_from_slice(&(data.len() as u32).to_le_bytes());
    b.extend_from_slice(&data);
    let len = (b.len() - 8) as u32;
    b[4..8].copy_from_slice(&len.to_le_bytes());
    b
}

/// Faux sherpa-onnx : relit l'en-tête, attend le compte annoncé, répond.
async fn fake_ws(reply: &'static str) -> (String, Arc<Mutex<Vec<usize>>>) {
    let frames = Arc::new(Mutex::new(Vec::new()));
    let l = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = l.local_addr().unwrap();
    let f = frames.clone();
    tokio::spawn(async move {
        while let Ok((s, _)) = l.accept().await {
            let f = f.clone();
            tokio::spawn(async move {
                let Ok(mut ws) = tokio_tungstenite::accept_async(s).await else {
                    return;
                };
                let mut buf = Vec::new();
                while let Some(Ok(m)) = ws.next().await {
                    if let Message::Binary(b) = m {
                        f.lock().unwrap().push(b.len());
                        buf.extend_from_slice(&b);
                        if buf.len() >= 8 {
                            let n = u32::from_le_bytes(buf[4..8].try_into().unwrap()) as usize;
                            if buf.len() - 8 >= n {
                                let _ = ws.send(Message::Text(reply.into())).await;
                                return;
                            }
                        }
                    }
                }
            });
        }
    });
    (format!("ws://{addr}"), frames)
}

#[tokio::test]
async fn protocole_websocket() {
    let (adr, frames) = fake_ws(r#"{"text":"  bonjour ceci est un test  "}"#).await;
    // 6000 échantillons : 8 + 24000 octets → trois trames de 10 Kio au plus.
    let txt = dictate::infer_at(&adr, &wav16(6000)).await.unwrap();
    assert_eq!(txt, "bonjour ceci est un test");
    assert_eq!(*frames.lock().unwrap(), [10240, 10240, 3528]);

    let (adr, _) = fake_ws(r#"{"text":" [musique] "}"#).await;
    assert_eq!(dictate::infer_at(&adr, &wav16(10)).await.unwrap(), "");
    let (adr, _) = fake_ws("pas du json").await;
    let e = dictate::infer_at(&adr, &wav16(10)).await.unwrap_err();
    assert!(
        e.starts_with("réponse illisible du serveur de dictée : "),
        "{e}"
    );
    // Audio vide : rien à transcrire, aucune connexion.
    assert_eq!(
        dictate::infer_at("ws://127.0.0.1:1", &wav16(0))
            .await
            .unwrap(),
        ""
    );
    let e = dictate::infer_at("ws://127.0.0.1:1", &wav16(10))
        .await
        .unwrap_err();
    assert!(e.starts_with("connexion au serveur de dictée : "), "{e}");
    let e = dictate::infer_at("ws://127.0.0.1:1", b"pas un wav")
        .await
        .unwrap_err();
    assert_eq!(e, "audio illisible : ce n'est pas un WAV");
}

// ---------------------------------------------------------------------------
// Archives et téléchargement
// ---------------------------------------------------------------------------

/// Une archive `.tar.bz2` de modèle : dossier racine, quatre fichiers, un
/// sous-dossier et un lien (ignoré).
fn archive(root: &str, evil: bool) -> Vec<u8> {
    let enc = bzip2::write::BzEncoder::new(Vec::new(), bzip2::Compression::fast());
    let mut tb = tar::Builder::new(enc);
    let mut dir = tar::Header::new_gnu();
    dir.set_entry_type(tar::EntryType::Directory);
    dir.set_mode(0o755);
    dir.set_size(0);
    tb.append_data(&mut dir, format!("{root}/"), std::io::empty())
        .unwrap();
    for (i, f) in dictate::ASR_FICHIERS
        .iter()
        .chain(["sous/notes.txt"].iter())
        .enumerate()
    {
        let body = format!("contenu {i}");
        let mut h = tar::Header::new_gnu();
        h.set_mode(0o644);
        h.set_size(body.len() as u64);
        tb.append_data(&mut h, format!("{root}/{f}"), body.as_bytes())
            .unwrap();
    }
    let mut l = tar::Header::new_gnu();
    l.set_entry_type(tar::EntryType::Symlink);
    l.set_size(0);
    tb.append_link(&mut l, format!("{root}/lien"), "/etc/passwd")
        .unwrap();
    if evil {
        let mut h = tar::Header::new_old();
        let name = format!("{root}/../evade.txt");
        h.as_old_mut().name[..name.len()].copy_from_slice(name.as_bytes());
        h.set_mode(0o644);
        h.set_size(1);
        h.set_cksum();
        tb.append(&h, &b"x"[..]).unwrap();
    }
    let enc = tb.into_inner().unwrap();
    enc.finish().unwrap()
}

#[test]
fn extraction_d_une_archive() {
    let tmp = tempfile::tempdir().unwrap();
    let dest = tmp.path().join("asr/modele");
    std::fs::create_dir_all(dest.parent().unwrap()).unwrap();
    dictate::extract(&archive("racine", false)[..], &dest).unwrap();
    assert_eq!(
        std::fs::read_to_string(dest.join("tokens.txt")).unwrap(),
        "contenu 3"
    );
    assert_eq!(
        std::fs::read_to_string(dest.join("sous/notes.txt")).unwrap(),
        "contenu 4"
    );
    assert!(!dest.join("lien").exists(), "lien suivi");
    assert!(!tmp.path().join("asr/modele.part").exists());

    // Entrée qui remonte : refus, rien de partiel ni d'évadé, l'ancien gardé.
    let e = dictate::extract(&archive("racine", true)[..], &dest).unwrap_err();
    assert_eq!(
        e,
        r#"entrée d'archive qui remonte hors du dossier : "racine/../evade.txt""#
    );
    assert!(!tmp.path().join("asr/modele.part").exists());
    assert!(!tmp.path().join("asr/evade.txt").exists());
    assert!(dest.join("tokens.txt").exists());
    // Archive corrompue.
    assert!(dictate::extract(&b"pas du bzip2"[..], &dest).is_err());
    assert!(!tmp.path().join("asr/modele.part").exists());
}

async fn file_server(files: Vec<(String, Vec<u8>)>) -> SocketAddr {
    let files = Arc::new(files);
    let l = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = l.local_addr().unwrap();
    tokio::spawn(async move {
        while let Ok((s, _)) = l.accept().await {
            let files = files.clone();
            tokio::spawn(async move {
                let svc = service_fn(move |req: Request<Incoming>| {
                    let files = files.clone();
                    async move {
                        let p = req.uri().path().to_string();
                        let r = match files.iter().find(|(n, _)| *n == p) {
                            Some((_, b)) => Response::new(Full::new(Bytes::from(b.clone()))),
                            None => {
                                let mut r = Response::new(Full::new(Bytes::from_static(b"absent")));
                                *r.status_mut() = StatusCode::NOT_FOUND;
                                r
                            }
                        };
                        Ok::<_, Infallible>(r)
                    }
                });
                let _ = hyper::server::conn::http1::Builder::new()
                    .serve_connection(TokioIo::new(s), svc)
                    .await;
            });
        }
    });
    addr
}

async fn wait_dl(d: &Dictation) -> dictate::DlState {
    for _ in 0..500 {
        let st = d.dl_state();
        if !st.running {
            return st;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    panic!("téléchargement jamais terminé");
}

#[tokio::test]
async fn telechargement_d_un_modele() {
    let dossier = "sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8";
    let addr = file_server(vec![(
        format!("/m/{dossier}.tar.bz2"),
        archive(dossier, false),
    )])
    .await;
    let tmp = tempfile::tempdir().unwrap();
    let d = Arc::new(Dictation::with(
        tmp.path(),
        &format!("http://{addr}/m/"),
        None,
        Duration::from_secs(60),
        Duration::from_secs(5),
    ));
    d.start_download("parakeet-tdt-0.6b-v2");
    // Un seul à la fois : le second est ignoré.
    d.start_download("parakeet-tdt-0.6b-v3");
    let st = wait_dl(&d).await;
    assert_eq!(
        (st.id.as_str(), st.pct, st.err.as_str()),
        ("parakeet-tdt-0.6b-v2", 100, "")
    );
    assert!(dictate::model_present(tmp.path(), "parakeet-tdt-0.6b-v2"));

    d.start_download("parakeet-tdt-0.6b-v3");
    let st = wait_dl(&d).await;
    assert_eq!(st.err, "téléchargement du modèle de dictée : HTTP 404");
    assert!(!dictate::model_present(tmp.path(), "parakeet-tdt-0.6b-v3"));
    d.start_download("inconnu");
    assert_eq!(d.dl_state().err, "modèle de dictée inconnu : inconnu");
}

// ---------------------------------------------------------------------------
// Supervision d'un faux sherpa-onnx
// ---------------------------------------------------------------------------

/// Faux `sherpa-onnx-offline-websocket-server` : poignée de main WebSocket,
/// lit l'utterance, répond ce qu'il a vu.
const FAKE_SHERPA: &str = r#"#!/usr/bin/env python3
import base64, hashlib, json, os, socket, struct, sys
port = int([a for a in sys.argv if a.startswith('--port=')][0][7:])
print("faux sherpa prêt", flush=True)
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(('127.0.0.1', port)); s.listen(8)
def lire(c, n):
    b = b''
    while len(b) < n:
        x = c.recv(n - len(b))
        if not x: raise EOFError
        b += x
    return b
while True:
    c, _ = s.accept()
    try:
        req = b''
        while b'\r\n\r\n' not in req:
            x = c.recv(4096)
            if not x: raise EOFError
            req += x
        key = [l.split(b':', 1)[1].strip() for l in req.split(b'\r\n') if l.lower().startswith(b'sec-websocket-key')][0]
        acc = base64.b64encode(hashlib.sha1(key + b'258EAFA5-E914-47DA-95CA-C5AB0DC85B11').digest())
        c.sendall(b'HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ' + acc + b'\r\n\r\n')
        buf = b''; trames = 0
        while True:
            h = lire(c, 2); op = h[0] & 15; n = h[1] & 127
            if n == 126: n = struct.unpack('>H', lire(c, 2))[0]
            elif n == 127: n = struct.unpack('>Q', lire(c, 8))[0]
            m = lire(c, 4); d = bytes(b ^ m[i % 4] for i, b in enumerate(lire(c, n)))
            if op == 8: break
            trames += 1; buf += d
            if len(buf) >= 8 and len(buf) - 8 >= struct.unpack('<I', buf[4:8])[0]:
                taux = struct.unpack('<I', buf[0:4])[0]; ech = struct.unpack('<I', buf[4:8])[0] // 4
                cuda = os.environ.get('CUDA_VISIBLE_DEVICES', 'absent')
                jeton = 'LOKI_INTERNAL_TOKEN' in os.environ
                t = json.dumps({"text": " vu %d échantillons à %d Hz en %d trames, cuda=[%s], jeton=%s " % (ech, taux, trames, cuda, jeton)}).encode()
                c.sendall(bytes([0x81]) + (bytes([len(t)]) if len(t) < 126 else bytes([126]) + struct.pack('>H', len(t))) + t)
                break
    except Exception:
        pass
    finally:
        c.close()
"#;

/// Écrit par un `sh` à part : si ce process tenait lui-même le fichier ouvert
/// en écriture, un `fork` lancé au même moment par un test voisin en
/// hériterait, et l'`exec` du script échouerait sur ETXTBSY.
fn script(dir: &Path, name: &str, body: &str) -> String {
    let p = dir.join(name);
    let ok = std::process::Command::new("sh")
        .args(["-c", r#"printf '%s' "$1" > "$2" && chmod 755 "$2""#, "sh"])
        .arg(body)
        .arg(&p)
        .status()
        .unwrap()
        .success();
    assert!(ok, "script non écrit");
    p.to_string_lossy().into_owned()
}

fn python3() -> bool {
    std::process::Command::new("python3")
        .arg("-c")
        .arg("pass")
        .status()
        .is_ok_and(|s| s.success())
}

fn install_v3(home: &Path) {
    for f in dictate::ASR_FICHIERS {
        let p = dictate::model_dir_for(home, "parakeet-tdt-0.6b-v3")
            .unwrap()
            .join(f);
        std::fs::create_dir_all(p.parent().unwrap()).unwrap();
        std::fs::write(p, b"x").unwrap();
    }
}

async fn transcribe(facade: SocketAddr, wav: &[u8]) -> (StatusCode, Value) {
    use http_body_util::BodyExt;
    use hyper_util::client::legacy::Client;
    use hyper_util::rt::TokioExecutor;
    let client: Client<_, Full<Bytes>> = Client::builder(TokioExecutor::new()).build_http();
    let r = client
        .request(
            Request::post(format!("http://{facade}/api/transcribe"))
                .body(Full::new(Bytes::copy_from_slice(wav)))
                .unwrap(),
        )
        .await
        .unwrap();
    let code = r.status();
    let b = r.into_body().collect().await.unwrap().to_bytes();
    (code, serde_json::from_slice(&b).unwrap())
}

#[tokio::test]
async fn supervision_du_serveur_de_dictee() {
    if !python3() {
        eprintln!("python3 absent : test sauté");
        return;
    }
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().canonicalize().unwrap();
    install_v3(&home);
    let bin = script(&home, "faux-sherpa", FAKE_SHERPA);
    let d = Arc::new(Dictation::with(
        &home,
        "http://127.0.0.1:1/",
        Some(bin),
        Duration::from_secs(60),
        Duration::from_secs(20),
    ));
    let (facade, go, _stop) = fakego::start_dictate(home.clone(), d.clone()).await;

    // 3000 échantillons : 8 + 12000 octets → deux trames. GPU masqués, jeton
    // interne absent de l'environnement du serveur.
    let (code, v) = transcribe(facade, &wav16(3000)).await;
    assert_eq!(code, StatusCode::OK, "{v}");
    assert_eq!(
        v["text"],
        "vu 3000 échantillons à 16000 Hz en 2 trames, cuda=[], jeton=False"
    );
    let (_, _, b) = fakego::get(facade, "/api/dictate/state", &[]).await;
    let st: Value = serde_json::from_slice(&b).unwrap();
    assert_eq!(
        (st["actif"].as_bool(), st["present"].as_bool()),
        (Some(true), Some(true))
    );
    assert!(st["depuis_s"].is_u64());
    // Même réglage : même serveur (pas de nouveau démarrage).
    let port = d
        .ensure(DictateCfg::default().with_defauts())
        .await
        .unwrap();
    assert_eq!(
        d.ensure(DictateCfg::default().with_defauts())
            .await
            .unwrap(),
        port
    );

    // Le Go demande l'arrêt (libération de la VRAM).
    let (code, _) = fakego::send(
        facade,
        "POST",
        dictate::INTERNAL_STOP,
        &[("x-loki-internal", fakego::JETON)],
        "{}",
    )
    .await;
    assert_eq!(code, StatusCode::NO_CONTENT);
    let (_, _, b) = fakego::get(facade, "/api/dictate/state", &[]).await;
    assert_eq!(serde_json::from_slice::<Value>(&b).unwrap()["actif"], false);
    // Sans le jeton : la route interne n'est pas joignable.
    let (code, _) = fakego::send(facade, "POST", dictate::INTERNAL_STOP, &[], "{}").await;
    assert_eq!(code, StatusCode::NOT_FOUND);

    // Changer de réglage éteint le serveur.
    let (code, _) = transcribe(facade, &wav16(1000)).await;
    assert_eq!(code, StatusCode::OK);
    let (code, _) = fakego::send(
        facade,
        "POST",
        "/api/dictate/config",
        &[],
        r#"{"model":"parakeet-tdt-0.6b-v3","reactivity":"court"}"#,
    )
    .await;
    assert_eq!(code, StatusCode::OK);
    assert_eq!(
        go.get("state", "dictate_cfg").as_deref(),
        Some(r#"{"model":"parakeet-tdt-0.6b-v3","reactivity":"court"}"#)
    );
    let (_, _, b) = fakego::get(facade, "/api/dictate/state", &[]).await;
    assert_eq!(serde_json::from_slice::<Value>(&b).unwrap()["actif"], false);
}

#[tokio::test]
async fn demarrages_rates() {
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().canonicalize().unwrap();
    install_v3(&home);
    let cfg = DictateCfg::default().with_defauts();
    // Mort au démarrage : la raison et la dernière ligne.
    let mort = script(
        &home,
        "mort",
        "#!/bin/sh\necho chargement\necho boom >&2\nexit 3\n",
    );
    let d = Arc::new(Dictation::with(
        &home,
        "x",
        Some(mort),
        Duration::from_secs(60),
        Duration::from_secs(10),
    ));
    let e = d.ensure(cfg.clone()).await.unwrap_err();
    assert_eq!(
        e,
        "le serveur de dictée s'est arrêté au démarrage : exit status 3 — dernière sortie : boom"
    );
    // Muet : le délai, à la Go.
    let muet = script(&home, "muet", "#!/bin/sh\nsleep 30\n");
    let d = Arc::new(Dictation::with(
        &home,
        "x",
        Some(muet),
        Duration::from_secs(60),
        Duration::from_secs(1),
    ));
    let e = d.ensure(cfg.clone()).await.unwrap_err();
    assert_eq!(e, "le serveur de dictée n'a pas répondu en 1s");
    // Un arrêt « vite » pendant un démarrage tue le processus qui démarre.
    let d2 = d.clone();
    let lent = Arc::new(Dictation::with(
        &home,
        "x",
        Some(script(&home, "lent", "#!/bin/sh\nsleep 30\n")),
        Duration::from_secs(60),
        Duration::from_secs(60),
    ));
    let l2 = lent.clone();
    let c2 = cfg.clone();
    let h = tokio::spawn(async move { l2.ensure(c2).await });
    tokio::time::sleep(Duration::from_millis(300)).await;
    let t0 = std::time::Instant::now();
    lent.shutdown_fast().await;
    let e = tokio::time::timeout(Duration::from_secs(5), h)
        .await
        .unwrap()
        .unwrap()
        .unwrap_err();
    assert!(
        e.starts_with("le serveur de dictée s'est arrêté au démarrage : signal: killed"),
        "{e}"
    );
    assert!(t0.elapsed() < Duration::from_secs(4));
    drop(d2);
    // Binaire introuvable, modèle absent.
    let d = Arc::new(Dictation::with(
        &home,
        "x",
        Some(String::new()),
        Duration::from_secs(60),
        Duration::from_secs(1),
    ));
    assert_eq!(
        d.ensure(cfg).await.unwrap_err(),
        "serveur de dictée introuvable (image à reconstruire)"
    );
    let v2 = DictateCfg {
        model: "parakeet-tdt-0.6b-v2".into(),
        ..Default::default()
    };
    assert_eq!(
        d.ensure(v2).await.unwrap_err(),
        "modèle de dictée absent : parakeet-tdt-0.6b-v2 (à télécharger dans Paramètres → Dictée)"
    );
}

#[tokio::test]
async fn sans_la_variable_la_dictee_reste_au_go() {
    let tmp = tempfile::tempdir().unwrap();
    let (facade, _go, _stop) = fakego::start(tmp.path().to_path_buf()).await;
    for p in [
        "/api/transcribe",
        "/api/dictate/config",
        "/api/dictate/state",
    ] {
        let (_, _, b) = fakego::get(facade, p, &[]).await;
        assert_eq!(&b[..], format!("go:{p}").as_bytes());
    }
    let (code, _) = fakego::send(
        facade,
        "POST",
        dictate::INTERNAL_STOP,
        &[("x-loki-internal", fakego::JETON)],
        "{}",
    )
    .await;
    assert_eq!(code, StatusCode::NOT_FOUND);
}
