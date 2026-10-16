//! Faux Chrome pour les tests du navigateur piloté (lot 44) : un script qui
//! annonce le port d'un faux serveur CDP dans DevToolsActivePort, et ce
//! serveur, qui répond selon un scénario et note chaque commande reçue
//! (comme `parity_browser_test.go`).
#![allow(dead_code)]

use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use base64::Engine as _;
use futures_util::{SinkExt as _, StreamExt as _};
use serde_json::{Map, Value, json};
use sha2::Digest as _;
use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};
use tokio::net::TcpListener;
use tokio_tungstenite::tungstenite::Message;

/// Le faux navigateur : il annonce `port`, range ses arguments dans
/// `args`, et attend.
pub fn chrome_script(dir: &Path, port: u16, args: &Path) -> PathBuf {
    let p = dir.join("chrome");
    let script = format!(
        r#"#!/bin/sh
for a in "$@"; do
  case "$a" in
    --user-data-dir=*) dir="${{a#--user-data-dir=}}" ;;
  esac
done
printf '%s\n/devtools/browser/faux\n' '{port}' > "$dir/DevToolsActivePort"
printf '%s\n' "$@" > '{}'
exec sleep 600
"#,
        args.display()
    );
    // Écrit par un `sh` à part : un `fork` d'un test voisin hériterait sinon
    // du fichier ouvert en écriture, et son `exec` échouerait sur ETXTBSY.
    let ok = std::process::Command::new("sh")
        .args(["-c", r#"printf '%s' "$1" > "$2" && chmod 755 "$2""#, "sh"])
        .arg(script)
        .arg(&p)
        .status()
        .unwrap()
        .success();
    assert!(ok, "faux Chrome non écrit");
    p
}

/// `parityCDPAbbrev`.
pub fn abbrev(s: &str) -> String {
    if s.len() <= 60 {
        return s.to_string();
    }
    let h = sha2::Sha256::digest(s.as_bytes());
    let head: String = s.chars().take(40).collect();
    format!("{head}…[{}:{}]", s.len(), hex::encode(&h[..6]))
}

/// `parityCDPCall`.
pub fn call_line(method: &str, params: Option<&Value>) -> String {
    let Some(Value::Object(p)) = params else {
        return method.to_string();
    };
    let mut q = Map::new();
    for (k, v) in p {
        let v = match v {
            Value::String(s) if k == "expression" || k == "source" => json!(abbrev(s)),
            v => v.clone(),
        };
        q.insert(k.clone(), v);
    }
    format!("{method} {}", loki_rs::gojson::marshal(&Value::Object(q)))
}

/// La capture unie du faux CDP (`parityPNG`).
pub fn png(w: u32, h: u32) -> String {
    let img = image::RgbaImage::from_pixel(w, h, image::Rgba([40, 80, 120, 255]));
    let mut out = std::io::Cursor::new(Vec::new());
    img.write_to(&mut out, image::ImageFormat::Png).unwrap();
    base64::engine::general_purpose::STANDARD.encode(out.into_inner())
}

pub type Scene = Arc<Mutex<Value>>;
pub type Calls = Arc<Mutex<Vec<String>>>;

/// La réponse du faux CDP à une commande (`reply` du journal Go).
pub fn reply(scene: &Value, method: &str, params: &Value) -> Result<Value, String> {
    if let Some(msg) = scene["errors"].get(method).and_then(Value::as_str) {
        return Err(msg.to_string());
    }
    let val = |v: &Value| {
        if v == "<absent>" {
            json!({ "result": { "type": "undefined" } })
        } else {
            json!({ "result": { "type": "object", "value": v } })
        }
    };
    match method {
        "Runtime.evaluate" => {
            let expr = params["expression"].as_str().unwrap_or("");
            match expr {
                "1" => return Ok(val(&json!(1))),
                "document.readyState" => return Ok(val(&json!("complete"))),
                "location.href" => return Ok(val(&json!(scene["href"].as_str().unwrap_or("")))),
                _ => {}
            }
            if scene["throw"] == true {
                return Ok(json!({ "result": { "type": "object" },
                    "exceptionDetails": { "text": "Uncaught TypeError: x is not a function" } }));
            }
            let pick = |k: &str| scene.get(k).cloned().unwrap_or(Value::Null);
            Ok(if expr.contains("window.__lokiRefs=map") {
                val(&pick("snap"))
            } else if expr.contains("var pool=ajQueryAll();") {
                val(&pick("find"))
            } else if expr.contains("inline:'center'") {
                val(&pick("center"))
            } else if expr.contains("return 'TEXT';") {
                val(&pick("typed"))
            } else if expr.starts_with("history.") || expr.starts_with("window.scrollBy(") {
                val(&json!("ok"))
            } else {
                val(&json!("<absent>"))
            })
        }
        "Page.navigate" => {
            let mut r = json!({ "frameId": "F1", "loaderId": "L1" });
            if let Some(e) = scene["nav_error"].as_str().filter(|e| !e.is_empty()) {
                r["errorText"] = json!(e);
            }
            Ok(r)
        }
        "Page.captureScreenshot" => Ok(json!({ "data": png(1280, 800) })),
        _ => Ok(json!({})),
    }
}

/// Le faux serveur CDP : `/json` puis la cible « page » en websocket.
pub async fn start(scene: Scene, calls: Calls) -> u16 {
    let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = l.local_addr().unwrap().port();
    tokio::spawn(async move {
        while let Ok((mut st, _)) = l.accept().await {
            let (scene, calls) = (scene.clone(), calls.clone());
            tokio::spawn(async move {
                let mut head = [0u8; 16];
                let n = st.peek(&mut head).await.unwrap_or(0);
                if head[..n].starts_with(b"GET /json") {
                    let mut buf = vec![0u8; 4096];
                    let _ = st.read(&mut buf).await;
                    let body = json!([
                        { "type": "service_worker", "webSocketDebuggerUrl": format!("ws://127.0.0.1:{port}/devtools/sw/0") },
                        { "type": "page", "webSocketDebuggerUrl": format!("ws://127.0.0.1:{port}/devtools/page/1") },
                    ])
                    .to_string();
                    let _ = st
                        .write_all(
                            format!(
                                "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                                body.len()
                            )
                            .as_bytes(),
                        )
                        .await;
                    return;
                }
                let Ok(mut ws) = tokio_tungstenite::accept_async(st).await else {
                    return;
                };
                while let Some(Ok(m)) = ws.next().await {
                    let Message::Text(t) = m else { continue };
                    let v: Value = serde_json::from_str(t.as_str()).unwrap_or_default();
                    let method = v["method"].as_str().unwrap_or("").to_string();
                    calls
                        .lock()
                        .unwrap()
                        .push(call_line(&method, v.get("params")));
                    let sc = scene.lock().unwrap().clone();
                    let out = match reply(&sc, &method, &v["params"]) {
                        Ok(r) => json!({ "id": v["id"], "result": r }),
                        Err(e) => {
                            json!({ "id": v["id"], "error": { "code": -32000, "message": e } })
                        }
                    };
                    if ws
                        .send(Message::Text(out.to_string().into()))
                        .await
                        .is_err()
                    {
                        return;
                    }
                }
            });
        }
    });
    port
}

/// Dimensions et pixels témoins de l'image relayée (`parityImageFacts`).
pub fn image_facts(part: &Value) -> Value {
    let u = part["image_url"]["url"].as_str().unwrap();
    let raw = base64::engine::general_purpose::STANDARD
        .decode(u.strip_prefix("data:image/png;base64,").unwrap())
        .unwrap();
    let img = image::load_from_memory(&raw).unwrap().to_rgba8();
    let (w, h) = img.dimensions();
    let mut px = Map::new();
    for (x, y) in [
        (0, 0),
        (100, 50),
        (101, 50),
        (50, 100),
        (2, 2),
        (104, 4),
        (110, 12),
        (300, 799),
        (1279, 700),
        (4, 204),
    ] {
        if x >= w || y >= h {
            continue;
        }
        let p = img.get_pixel(x, y).0;
        px.insert(
            format!("{x},{y}"),
            json!(format!("{},{},{},{}", p[0], p[1], p[2], p[3])),
        );
    }
    json!({ "w": w, "h": h, "px": px })
}
