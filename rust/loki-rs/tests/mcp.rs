//! Lot 20 : serveurs MCP tenus par la façade (`LOKI_MCP=rust`).
//!
//! Le journal tiré du Go (`internal/loki/parity_mcp_test.go` →
//! `tests/parity_mcp/mcp.json`) est rejoué : fonctions pures, lecture et
//! mutations de la configuration, puis un faux serveur MCP (Streamable HTTP)
//! qui resert ce qu'un vrai serveur du SDK Go a mis sur le fil — l'état, les
//! outils annoncés au modèle et les résultats d'appel tirés par la façade
//! doivent être ceux du client Go. Le transport stdio, les routes de
//! configuration et l'API interne pour le Go sont vérifiés de bout en bout.

mod common;

use std::collections::BTreeMap;
use std::convert::Infallible;
use std::sync::{Arc, Mutex};

use bytes::Bytes;
use common::fakego;
use http_body_util::{BodyExt, Full};
use hyper::service::service_fn;
use hyper::{Request, Response};
use hyper_util::rt::TokioIo;
use loki_rs::mcp::{self, Mcp, config as cf};
use serde_json::{Map, Value, json};
use tokio::net::TcpListener;

fn fixture() -> Value {
    serde_json::from_str(include_str!("parity_mcp/mcp.json")).expect("journal")
}

fn s(v: &Value) -> &str {
    v.as_str().unwrap()
}

#[test]
fn parite_des_fonctions_pures() {
    let fx = fixture();
    for (k, v) in fx["sanitize"].as_object().unwrap() {
        assert_eq!(cf::sanitize(k), s(v), "{k:?}");
    }
    for c in fx["schemas"].as_array().unwrap() {
        let v: Value = serde_json::from_str(s(&c["in"])).unwrap();
        assert_eq!(cf::normalize_schema(Some(&v)), c["out"], "{}", c["in"]);
    }
    assert_eq!(
        cf::normalize_schema(None),
        json!({"type":"object","properties":{}})
    );
    for c in fx["labels"].as_array().unwrap() {
        let m: Map<String, Value> = serde_json::from_str(s(&c["in"])).unwrap();
        assert_eq!(cf::arg_label(&m), s(&c["out"]), "{}", c["in"]);
    }
    for c in fx["tool_server"].as_array().unwrap() {
        let got = cf::tool_server(s(&c["name"]), s(&c["desc"]));
        assert_eq!(got.is_some(), c["ok"].as_bool().unwrap(), "{c}");
        assert_eq!(got.unwrap_or_default(), s(&c["server"]), "{c}");
    }
    for c in fx["prompt_line"].as_array().unwrap() {
        let tools: Vec<(String, String)> = c["tools"]
            .as_array()
            .map(|a| {
                a.iter()
                    .map(|p| (s(&p[0]).to_string(), s(&p[1]).to_string()))
                    .collect()
            })
            .unwrap_or_default();
        assert_eq!(cf::prompt_line(&tools), s(&c["out"]), "{c}");
    }
    let def = cf::Timeouts::default();
    for (cmd, v) in fx["timeouts"].as_object().unwrap() {
        let cfg = cf::ServerConfig {
            command: cmd.clone(),
            ..Default::default()
        };
        assert_eq!(
            cf::timeout_for(&cfg, &def).as_secs() as i64,
            v["secs"].as_i64().unwrap(),
            "{cmd}"
        );
        assert_eq!(cf::timeout_error(&cfg, &def), s(&v["err"]), "{cmd}");
    }
    // Délais globaux : lecture de la base, puis leur effet.
    let tm = |v: &Value| cf::Timeouts {
        connect: v["connect"].as_i64().unwrap(),
        call: v["call"].as_i64().unwrap(),
    };
    for c in fx["timeouts_stored"].as_array().unwrap() {
        let raw = s(&c["in"]);
        let got = cf::decode_timeouts((!raw.is_empty()).then_some(raw.as_bytes()));
        assert_eq!(got, tm(&c["out"]), "{raw}");
    }
    for c in fx["timeouts_effects"].as_array().unwrap() {
        let t = tm(&c["timeouts"]);
        let cfg: cf::ServerConfig = serde_json::from_str(s(&c["cfg"])).unwrap();
        let args: Map<String, Value> = serde_json::from_str(s(&c["args"])).unwrap();
        assert_eq!(
            cf::timeout_for(&cfg, &t).as_millis() as i64,
            c["connect"].as_i64().unwrap(),
            "{c}"
        );
        assert_eq!(cf::timeout_error(&cfg, &t), s(&c["err"]), "{c}");
        assert_eq!(
            cf::call_timeout_for(&cfg, &t, &args).as_millis() as i64,
            c["call"].as_i64().unwrap(),
            "{c}"
        );
    }
    for c in fx["flatten"].as_array().unwrap() {
        let v: Value = serde_json::from_str(s(&c["in"])).unwrap();
        assert_eq!(cf::flatten_content(&v), s(&c["out"]), "{}", c["in"]);
    }
}

#[test]
fn parite_de_la_configuration() {
    let fx = fixture();
    for c in fx["decode"].as_array().unwrap() {
        let m = cf::decode_servers(Some(s(&c["in"]).as_bytes()));
        let out = if s(&c["in"]) == "null" && m.is_empty() {
            "null".to_string()
        } else {
            loki_rs::gojson::marshal(&m)
        };
        assert_eq!(out, s(&c["out"]), "{}", c["in"]);
        for (n, want) in c["transports"].as_object().unwrap() {
            let cfg = &m[n];
            let got = format!(
                "{}|{}",
                cfg.transport(),
                cfg.validate().err().unwrap_or_default()
            );
            assert_eq!(got, s(want), "{n}");
        }
    }
    let mut servers = cf::Servers::new();
    let mut saved = false;
    for m in fx["mutations"].as_array().unwrap() {
        let name = s(&m["name"]);
        let res = match s(&m["op"]) {
            "set" => {
                let cfg: cf::ServerConfig = serde_json::from_str(s(&m["cfg"])).unwrap();
                cf::set_server(&mut servers, name, cfg)
            }
            "enable" => cf::set_enabled(&mut servers, name, m["on"].as_bool().unwrap_or(false)),
            "tool" => cf::set_tool_enabled(
                &mut servers,
                name,
                s(&m["tool"]),
                m["on"].as_bool().unwrap_or(false),
            ),
            "delete" => cf::delete_server(&mut servers, name),
            op => panic!("{op}"),
        };
        saved |= res.is_ok();
        assert_eq!(res.err().unwrap_or_default(), s(&m["error"]), "{m}");
        let state = if saved {
            loki_rs::gojson::marshal(&servers)
        } else {
            String::new()
        };
        assert_eq!(state, s(&m["state"]), "{m}");
    }
}

/// Un faux serveur MCP en Streamable HTTP : resert ce que le serveur du SDK
/// Go a répondu. `tools/call` en flux SSE, le reste en JSON.
async fn fake_server(wire: Value, seen: Arc<Mutex<Vec<String>>>) -> String {
    let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = l.local_addr().unwrap();
    tokio::spawn(async move {
        while let Ok((st, _)) = l.accept().await {
            let (wire, seen) = (wire.clone(), seen.clone());
            tokio::spawn(async move {
                let svc = service_fn(move |req: Request<hyper::body::Incoming>| {
                    let (wire, seen) = (wire.clone(), seen.clone());
                    async move {
                        let sid = req
                            .headers()
                            .get("mcp-session-id")
                            .map(|v| v.to_str().unwrap().to_string());
                        let auth = req
                            .headers()
                            .get("x-cle")
                            .map(|v| v.to_str().unwrap().to_string());
                        let method = req.method().clone();
                        let b = req.into_body().collect().await.unwrap().to_bytes();
                        let mut r = Response::new(Full::new(Bytes::new()));
                        if method == hyper::Method::DELETE {
                            seen.lock().unwrap().push("DELETE".into());
                            return Ok::<_, Infallible>(r);
                        }
                        let v: Value = serde_json::from_slice(&b).unwrap();
                        let m = v["method"].as_str().unwrap_or("").to_string();
                        seen.lock().unwrap().push(format!(
                            "{m} {} {}",
                            sid.as_deref().unwrap_or("-"),
                            auth.as_deref().unwrap_or("-")
                        ));
                        let result = match m.as_str() {
                            "initialize" => {
                                r.headers_mut()
                                    .insert("Mcp-Session-Id", "sess-1".parse().unwrap());
                                json!({ "protocolVersion": "2025-06-18",
                                    "capabilities": { "tools": {} },
                                    "serverInfo": { "name": "faux", "version": "1" } })
                            }
                            "tools/list" => wire["tools/list"].clone(),
                            "tools/call" => {
                                let name = v["params"]["name"].as_str().unwrap();
                                let res = wire["tools/call"][name].clone();
                                let body = format!(
                                    "event: message\ndata: {}\n\n",
                                    json!({ "jsonrpc": "2.0", "id": v["id"], "result": res })
                                );
                                r.headers_mut()
                                    .insert("content-type", "text/event-stream".parse().unwrap());
                                *r.body_mut() = Full::new(Bytes::from(body));
                                return Ok(r);
                            }
                            _ => {
                                *r.status_mut() = hyper::StatusCode::ACCEPTED;
                                return Ok(r);
                            }
                        };
                        r.headers_mut()
                            .insert("content-type", "application/json".parse().unwrap());
                        *r.body_mut() = Full::new(Bytes::from(
                            json!({ "jsonrpc": "2.0", "id": v["id"], "result": result })
                                .to_string(),
                        ));
                        Ok(r)
                    }
                });
                let _ = hyper::server::conn::http1::Builder::new()
                    .serve_connection(TokioIo::new(st), svc)
                    .await;
            });
        }
    });
    format!("http://{addr}")
}

async fn internal(facade: std::net::SocketAddr, path: &str, body: Value) -> Value {
    let (code, b) = fakego::send(
        facade,
        "POST",
        path,
        &[(loki_rs::auth::INTERNAL_HEADER, fakego::JETON)],
        &body.to_string(),
    )
    .await;
    assert_eq!(code, 200, "{path}");
    serde_json::from_slice(&b).unwrap()
}

#[tokio::test]
async fn parite_contre_le_serveur_du_sdk() {
    let fx = fixture();
    let seen = Arc::new(Mutex::new(Vec::new()));
    let url = fake_server(fx["wire"].clone(), seen.clone()).await;
    let tmp = tempfile::tempdir().unwrap();
    let pool = Arc::new(Mcp::new(tmp.path().to_path_buf()));
    let (facade, go, _stop, _) = fakego::start_with(
        tmp.path().to_path_buf(),
        fakego::Opts {
            mcp: Some(pool.clone()),
            ..Default::default()
        },
    )
    .await;
    go.set("state", "mcp", &s(&fx["store"]).replace("{{URL}}", &url));
    let mask = |v: &Value| -> Value {
        serde_json::from_str(&v.to_string().replace(&url, "{{URL}}")).unwrap()
    };

    let (_, b) = fakego::send(facade, "GET", "/api/mcp", &[], "").await;
    let v: Value = serde_json::from_slice(&b).unwrap();
    assert_eq!(v["ok"], true);
    assert_eq!(mask(&v["servers"]), fx["status"]);

    let tools = internal(facade, mcp::INTERNAL_TOOLS, json!({})).await;
    assert_eq!(tools["tools"], fx["tools"]);
    let pairs: Vec<(String, String)> = tools["tools"]
        .as_array()
        .unwrap()
        .iter()
        .map(|t| {
            (
                s(&t["function"]["name"]).to_string(),
                s(&t["function"]["description"]).to_string(),
            )
        })
        .collect();
    assert_eq!(cf::prompt_line(&pairs), s(&fx["prompt_line_live"]));

    for (name, want) in fx["calls"].as_object().unwrap() {
        let args = if name == "mcp__alpha__echo" {
            json!({ "text": "salut <toi>" })
        } else {
            json!({})
        };
        let r = internal(
            facade,
            mcp::INTERNAL_CALL,
            json!({ "name": name, "args": args }),
        )
        .await;
        assert_eq!(r["result"], *want, "{name}");
    }
    // Une seule session, partagée par l'état, les outils et les appels.
    let seen = seen.lock().unwrap().clone();
    assert_eq!(
        seen.iter().filter(|l| l.starts_with("initialize")).count(),
        1,
        "{seen:?}"
    );
    assert!(seen.iter().any(|l| l == "tools/call sess-1 -"), "{seen:?}");
}

/// Un faux serveur MCP en stdio : sh, un message par ligne.
fn stdio_server(dir: &std::path::Path) -> String {
    let p = dir.join("faux-mcp");
    let body = r#"#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","method":"notifications/message","params":{}}\n'
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"sh","version":"1"}}}\n' "$id" ;;
    *'"method":"tools/list"'*)
      printf '{"jsonrpc":"2.0","id":99,"method":"ping"}\n'
      printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"date","description":"Heure","inputSchema":{"type":"object"}},{"name":"crash","inputSchema":{"type":"object"}}]}}\n' "$id" ;;
    *'"name":"crash"'*) exit 3 ;;
    *'"method":"tools/call"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"%s"}]}}\n' "$id" "$FAUX_ENV" ;;
    *'"result":{}'*) echo "pong reçu" >> "$TRACE" ;;
  esac
done
"#;
    let ok = std::process::Command::new("sh")
        .args(["-c", "printf %s \"$1\" > \"$2\" && chmod 755 \"$2\""])
        .arg("sh")
        .arg(body)
        .arg(&p)
        .status()
        .unwrap()
        .success();
    assert!(ok);
    p.to_string_lossy().into_owned()
}

#[tokio::test]
async fn transport_stdio_et_reconnexion() {
    let tmp = tempfile::tempdir().unwrap();
    let bin = stdio_server(tmp.path());
    let trace = tmp.path().join("trace");
    let pool = Arc::new(Mcp::new(tmp.path().to_path_buf()));
    let (facade, go, _stop, _) = fakego::start_with(
        tmp.path().to_path_buf(),
        fakego::Opts {
            mcp: Some(pool.clone()),
            ..Default::default()
        },
    )
    .await;
    let mut env = BTreeMap::new();
    env.insert("FAUX_ENV".to_string(), "valeur d'env".to_string());
    env.insert("TRACE".to_string(), trace.to_string_lossy().into_owned());
    let mut servers = cf::Servers::new();
    servers.insert(
        "sh".into(),
        cf::ServerConfig {
            command: bin,
            env,
            ..Default::default()
        },
    );
    go.set("state", "mcp", &loki_rs::gojson::marshal(&servers));
    let (_, b) = fakego::send(facade, "GET", "/api/mcp", &[], "").await;
    let v: Value = serde_json::from_slice(&b).unwrap();
    assert_eq!(v["servers"][0]["connected"], true, "{v}");
    assert_eq!(v["servers"][0]["tools"], json!(["date", "crash"]));
    let r = internal(
        facade,
        mcp::INTERNAL_CALL,
        json!({ "name": "mcp__sh__date", "args": {} }),
    )
    .await;
    assert_eq!(r["result"], "valeur d'env");
    // Le serveur meurt en plein appel : une reconnexion, puis l'erreur.
    let r = internal(
        facade,
        mcp::INTERNAL_CALL,
        json!({ "name": "mcp__sh__crash", "args": {} }),
    )
    .await;
    let out = s(&r["result"]);
    assert!(
        out.starts_with("[erreur MCP] calling \"tools/call\""),
        "{out}"
    );
    // Après la reconnexion, le serveur répond de nouveau.
    let r = internal(
        facade,
        mcp::INTERNAL_CALL,
        json!({ "name": "mcp__sh__date", "args": {} }),
    )
    .await;
    assert_eq!(r["result"], "valeur d'env");
    // Le ping du serveur a reçu sa réponse.
    let t = std::fs::read_to_string(&trace).unwrap_or_default();
    assert!(t.contains("pong reçu"), "{t:?}");
    pool.close_all().await;
}

#[tokio::test]
async fn routes_de_configuration() {
    let tmp = tempfile::tempdir().unwrap();
    let seen = Arc::new(Mutex::new(Vec::new()));
    let fx = fixture();
    let url = fake_server(fx["wire"].clone(), seen.clone()).await;
    let pool = Arc::new(Mcp::new(tmp.path().to_path_buf()));
    let (facade, go, _stop, _) = fakego::start_with(
        tmp.path().to_path_buf(),
        fakego::Opts {
            mcp: Some(pool.clone()),
            ..Default::default()
        },
    )
    .await;
    let post = |path: &'static str, body: String| async move {
        let (c, b) = fakego::send(facade, "POST", path, &[], &body).await;
        (c.as_u16(), serde_json::from_slice::<Value>(&b).unwrap())
    };
    // Ajout : actif par défaut, en-têtes envoyés, état rendu.
    let (c, v) = post(
        "/api/mcp/save",
        json!({ "name": " web ", "url": format!(" {url} "), "headers": { "X-Cle": "k" } })
            .to_string(),
    )
    .await;
    assert_eq!(c, 200, "{v}");
    assert_eq!(v["servers"][0]["name"], "web");
    assert_eq!(v["servers"][0]["connected"], true);
    assert!(
        seen.lock().unwrap().iter().any(|l| l == "initialize - k"),
        "{:?}",
        seen.lock().unwrap()
    );
    let stored: Value = serde_json::from_str(&go.get("state", "mcp").unwrap()).unwrap();
    assert_eq!(stored["web"]["url"], json!(url));
    assert!(stored["web"].get("callTimeoutSec").is_none());
    // Délai d'un appel propre au serveur : rangé, rendu, puis retiré quand le
    // champ revient vide.
    let body = |ct: Option<i64>| {
        let mut b = json!({ "name": "web", "url": url, "headers": { "X-Cle": "k" } });
        if let Some(n) = ct {
            b["callTimeoutSec"] = json!(n);
        }
        b.to_string()
    };
    let (c, v) = post("/api/mcp/save", body(Some(900))).await;
    assert_eq!(
        (c, v["servers"][0]["callTimeoutSec"].clone()),
        (200, json!(900))
    );
    let stored: Value = serde_json::from_str(&go.get("state", "mcp").unwrap()).unwrap();
    assert_eq!(stored["web"]["callTimeoutSec"], 900);
    let (c, v) = post("/api/mcp/save", body(None)).await;
    assert_eq!(c, 200);
    assert!(v["servers"][0].get("callTimeoutSec").is_none(), "{v}");
    for n in [-1, 3601] {
        let (c, v) = post("/api/mcp/save", body(Some(n))).await;
        assert_eq!(
            (c, v["error"].clone()),
            (
                400,
                json!("délai d'un appel : 0 à 3600 s (vide = réglage global)")
            )
        );
    }
    let (c, v) = post(
        "/api/mcp/save",
        r#"{"name":"x","url":"http://h","callTimeoutSec":"9"}"#.into(),
    )
    .await;
    assert_eq!(
        (c, v["error"].clone()),
        (
            400,
            json!(
                "json: cannot unmarshal string into Go struct field mcpSaveReq.callTimeoutSec of type int"
            )
        )
    );
    // Erreurs de la configuration et du JSON.
    let (c, v) = post("/api/mcp/save", r#"{"name":"a__b","command":"x"}"#.into()).await;
    assert_eq!(
        (c, v["error"].clone()),
        (
            400,
            json!("le nom ne peut pas contenir '__' (réservé au namespacing des outils)")
        )
    );
    let (c, v) = post(
        "/api/mcp/save",
        r#"{"name":"x","args":"pas une liste"}"#.into(),
    )
    .await;
    assert_eq!(
        (c, v["error"].clone()),
        (
            400,
            json!(
                "json: cannot unmarshal string into Go struct field mcpSaveReq.args of type []string"
            )
        )
    );
    // Un outil masqué : connu de l'état, absent des outils annoncés.
    let (c, v) = post(
        "/api/mcp/tool",
        r#"{"name":"web","tool":"secret","on":false}"#.into(),
    )
    .await;
    assert_eq!(
        (c, v["servers"][0]["disabled"].clone()),
        (200, json!(["secret"]))
    );
    let tools = internal(facade, mcp::INTERNAL_TOOLS, json!({})).await;
    assert!(
        !tools["tools"]
            .as_array()
            .unwrap()
            .iter()
            .any(|t| t["function"]["name"] == "mcp__web__secret")
    );
    // Éteint : plus de connexion, plus d'outils ; la session est fermée.
    let (c, v) = post("/api/mcp/toggle", r#"{"name":"web","on":false}"#.into()).await;
    assert_eq!(
        (c, v["servers"][0]["connected"].clone()),
        (200, json!(false))
    );
    assert!(seen.lock().unwrap().iter().any(|l| l == "DELETE"));
    let tools = internal(facade, mcp::INTERNAL_TOOLS, json!({})).await;
    assert_eq!(tools["tools"], json!([]));
    let (c, v) = post("/api/mcp/toggle", r#"{"name":"nope","on":true}"#.into()).await;
    assert_eq!(
        (c, v["error"].clone()),
        (400, json!("serveur MCP inconnu: nope"))
    );
    // Tester : reconnexion.
    post("/api/mcp/toggle", r#"{"name":"web","on":true}"#.into()).await;
    let before = seen
        .lock()
        .unwrap()
        .iter()
        .filter(|l| l.starts_with("initialize"))
        .count();
    let (c, _) = post("/api/mcp/test", r#"{"name":"web"}"#.into()).await;
    assert_eq!(c, 200);
    let after = seen
        .lock()
        .unwrap()
        .iter()
        .filter(|l| l.starts_with("initialize"))
        .count();
    assert_eq!(after, before + 1);
    // Suppression.
    let (c, v) = post("/api/mcp/delete", r#"{"name":"web"}"#.into()).await;
    assert_eq!((c, v), (200, json!({"ok": true})));
    assert_eq!(go.get("state", "mcp").unwrap(), "{}");
    let (c, v) = post("/api/mcp/delete", r#"{"name":"web"}"#.into()).await;
    assert_eq!(
        (c, v["error"].clone()),
        (400, json!("serveur MCP inconnu: web"))
    );
    pool.close_all().await;
}

/// Les routes des délais globaux rejouées contre la façade : mêmes codes,
/// mêmes réponses, même valeur rangée ; un enregistrement réussi ferme les
/// sessions ouvertes.
#[tokio::test]
async fn routes_des_delais() {
    let tmp = tempfile::tempdir().unwrap();
    let seen = Arc::new(Mutex::new(Vec::new()));
    let fx = fixture();
    let url = fake_server(fx["wire"].clone(), seen.clone()).await;
    let pool = Arc::new(Mcp::new(tmp.path().to_path_buf()));
    let (facade, go, _stop, _) = fakego::start_with(
        tmp.path().to_path_buf(),
        fakego::Opts {
            mcp: Some(pool.clone()),
            ..Default::default()
        },
    )
    .await;
    go.set(
        "state",
        "mcp",
        &json!({ "web": { "url": url, "enabled": true } }).to_string(),
    );
    let initialize = || {
        seen.lock()
            .unwrap()
            .iter()
            .filter(|l| l.starts_with("initialize"))
            .count()
    };
    let (_, b) = fakego::send(facade, "GET", "/api/mcp", &[], "").await;
    let v: Value = serde_json::from_slice(&b).unwrap();
    assert_eq!(v["servers"][0]["connected"], true, "{v}");
    let mut connections = initialize();
    for c in fx["timeouts_routes"].as_array().unwrap() {
        let path = s(&c["path"]);
        let method = if path.ends_with("/save") {
            "POST"
        } else {
            "GET"
        };
        let (code, b) = fakego::send(facade, method, path, &[], s(&c["body"])).await;
        assert_eq!(code.as_u16() as i64, c["code"].as_i64().unwrap(), "{c}");
        let got: Value = serde_json::from_slice(&b).unwrap();
        let want: Value = serde_json::from_str(s(&c["resp"])).unwrap();
        assert_eq!(got, want, "{c}");
        assert_eq!(
            go.get("state", cf::ST_TIMEOUTS).unwrap_or_default(),
            s(&c["state"]),
            "{c}"
        );
        // Session fermée : la liste suivante se reconnecte.
        fakego::send(facade, "GET", "/api/mcp", &[], "").await;
        let now = initialize();
        let saved = method == "POST" && code.as_u16() == 200;
        assert_eq!(now, connections + usize::from(saved), "{c}");
        connections = now;
    }
    pool.close_all().await;
}

#[tokio::test]
async fn catalogue_embarque() {
    let tmp = tempfile::tempdir().unwrap();
    let pool = Arc::new(Mcp::new(tmp.path().to_path_buf()));
    let (facade, go, _stop, _) = fakego::start_with(
        tmp.path().to_path_buf(),
        fakego::Opts {
            mcp: Some(pool),
            ..Default::default()
        },
    )
    .await;
    go.set("state", "mcp", r#"{"filesystem":{"command":"npx"}}"#);
    let (_, b) = fakego::send(facade, "GET", "/api/mcp/catalog", &[], "").await;
    let v: Value = serde_json::from_slice(&b).unwrap();
    let cat = v["catalog"].as_array().unwrap();
    assert!(cat.len() > 10);
    let fs = cat.iter().find(|e| e["id"] == "filesystem").unwrap();
    assert_eq!(fs["installed"], true);
    let ws = format!("{}/workspace", tmp.path().display());
    assert!(fs["args"].as_array().unwrap().contains(&json!(ws)), "{fs}");
    assert!(
        fs.get("env").is_none() && fs.get("secrets").is_none(),
        "{fs}"
    );
    assert!(cat.iter().all(|e| e.get("runtimeOk").is_some()));
    assert!(cat.iter().filter(|e| e["installed"] == true).count() == 1);
}

#[tokio::test]
async fn sans_la_variable_mcp_reste_au_go() {
    let tmp = tempfile::tempdir().unwrap();
    let (facade, _go, _stop) = fakego::start(tmp.path().to_path_buf()).await;
    for p in [
        "/api/mcp",
        "/api/mcp/save",
        "/api/mcp/catalog",
        "/api/mcp/test",
        "/api/mcp/timeouts",
        "/api/mcp/timeouts/save",
    ] {
        let (_, _, b) = fakego::get(facade, p, &[]).await;
        assert_eq!(&b[..], format!("go:{p}").as_bytes());
    }
    // L'API interne MCP de la façade n'existe pas sans le lot.
    let (c, _) = fakego::send(
        facade,
        "POST",
        mcp::INTERNAL_TOOLS,
        &[(loki_rs::auth::INTERNAL_HEADER, fakego::JETON)],
        "{}",
    )
    .await;
    assert_eq!(c, 404);
}

/// Processus vivants dont la ligne de commande contient `needle`.
fn alive(needle: &str) -> usize {
    std::fs::read_dir("/proc")
        .unwrap()
        .flatten()
        .filter(|e| {
            e.file_name()
                .to_string_lossy()
                .bytes()
                .all(|b| b.is_ascii_digit())
        })
        .filter(|e| {
            std::fs::read(e.path().join("cmdline"))
                .map(|b| String::from_utf8_lossy(&b).contains(needle))
                .unwrap_or(false)
        })
        .filter(|e| {
            // Un zombie n'est plus un serveur vivant.
            std::fs::read_to_string(e.path().join("stat"))
                .map(|s| !s.contains(") Z "))
                .unwrap_or(false)
        })
        .count()
}

#[tokio::test]
async fn session_invalidee_pendant_la_connexion() {
    let tmp = tempfile::tempdir().unwrap();
    let p = tmp.path().join("lent-mcp");
    let body = r#"#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      sleep 1
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"lent","version":"1"}}}\n' "$id" ;;
    *'"method":"tools/list"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"t","inputSchema":{"type":"object"}}]}}\n' "$id" ;;
  esac
done
"#;
    assert!(
        std::process::Command::new("sh")
            .args(["-c", "printf %s \"$1\" > \"$2\" && chmod 755 \"$2\""])
            .arg("sh")
            .arg(body)
            .arg(&p)
            .status()
            .unwrap()
            .success()
    );
    let bin = p.to_string_lossy().into_owned();
    let pool = Arc::new(Mcp::new(tmp.path().to_path_buf()));
    let (facade, go, _stop, _) = fakego::start_with(
        tmp.path().to_path_buf(),
        fakego::Opts {
            mcp: Some(pool.clone()),
            ..Default::default()
        },
    )
    .await;
    let mut servers = cf::Servers::new();
    servers.insert(
        "lent".into(),
        cf::ServerConfig {
            command: bin.clone(),
            ..Default::default()
        },
    );
    go.set("state", "mcp", &loki_rs::gojson::marshal(&servers));
    // Une première connexion part, puis le serveur est invalidé avant qu'elle
    // aboutisse (« tester ») : elle ne doit jamais être republiée.
    let first = tokio::spawn(async move { fakego::send(facade, "GET", "/api/mcp", &[], "").await });
    tokio::time::sleep(std::time::Duration::from_millis(300)).await;
    let (c, b) = fakego::send(facade, "POST", "/api/mcp/test", &[], r#"{"name":"lent"}"#).await;
    assert_eq!(c, 200);
    let v: Value = serde_json::from_slice(&b).unwrap();
    assert_eq!(v["servers"][0]["connected"], true);
    let _ = first.await;
    tokio::time::sleep(std::time::Duration::from_millis(200)).await;
    assert_eq!(
        alive(&bin),
        1,
        "un seul serveur vivant après l'invalidation"
    );
    pool.close_all().await;
    tokio::time::sleep(std::time::Duration::from_millis(200)).await;
    assert_eq!(alive(&bin), 0);
}
