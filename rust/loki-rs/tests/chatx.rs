//! Lot 19 : export, mode Code et critères de la discussion tenus par la
//! façade quand elle tient la conversation (`LOKI_CHAT=rust`).
//!
//! Le journal tiré du Go (`internal/loki/parity_chatx_test.go` →
//! `tests/parity_chatx/chatx.json`) est rejoué : l'export Markdown et JSON de
//! la même conversation sous chaque jeu d'options, puis les appels aux routes
//! du mode et des critères par la façade, avec la base et les événements du
//! fil après chacun.

mod common;

use common::fakego::{self, CryptMode};
use loki_rs::chat::persist::decode_state;
use loki_rs::chatx::{self as cx, ExportOpts};
use serde_json::{Value, json};

fn fixture() -> Value {
    serde_json::from_str(include_str!("parity_chatx/chatx.json")).expect("journal")
}

fn s(v: &Value) -> &str {
    v.as_str().unwrap()
}

#[test]
fn parite_de_l_export() {
    let fx = fixture();
    // La conversation VIVANTE, images comprises (decode_state, comme le
    // rechargement du Go, les retire d'un fil relu).
    let c = &fx["conversation"];
    let conv = decode_state(c.to_string().as_bytes());
    let messages: Vec<loki_rs::chat::types::Message> =
        serde_json::from_value(c["messages"].clone()).unwrap();
    let log: Vec<loki_rs::chat::types::LogEvent> =
        serde_json::from_value(c["log"].clone()).unwrap();
    assert_eq!(conv.log, log);
    for c in fx["exports"].as_array().unwrap() {
        let q = s(&c["query"]);
        let o = cx::opts_from_query(q);
        let want = &c["opts"];
        assert_eq!(
            o,
            ExportOpts {
                format: s(&want["format"]).into(),
                reasoning: want["reasoning"].as_bool().unwrap(),
                tools: want["tools"].as_bool().unwrap(),
                results: want["results"].as_bool().unwrap(),
                turns: want["turns"].as_i64().unwrap(),
            },
            "options de {q:?}"
        );
        let body = if o.format == "json" {
            String::from_utf8(cx::export_json(
                &messages,
                &log,
                conv.ctx_used,
                &o,
                "{{DATE}}",
                "{{VERSION}}",
            ))
            .unwrap()
        } else {
            cx::export_markdown(&log, &o, "{{DATE}}", "{{VERSION}}")
        };
        assert_eq!(body, s(&c["body"]), "export de {q:?}");
    }
    let o = ExportOpts::default();
    assert_eq!(
        cx::export_markdown(&[], &o, "{{DATE}}", "{{VERSION}}"),
        s(&fx["empty_md"])
    );
    assert_eq!(
        String::from_utf8(cx::export_json(&[], &[], 0, &o, "{{DATE}}", "{{VERSION}}")).unwrap(),
        s(&fx["empty_json"])
    );
}

#[tokio::test]
async fn parite_du_mode_et_des_criteres() {
    let tmp = tempfile::tempdir().unwrap();
    let (facade, go, _stop, chat) = fakego::start_with(
        tmp.path().to_path_buf(),
        fakego::Opts {
            chat: true,
            ..Default::default()
        },
    )
    .await;
    let chat = chat.unwrap();
    *go.crypt_mode.lock().unwrap() = CryptMode::Plain;
    let conv = chat.sessions.conv();
    let id = conv.id();
    let log_len = || conv.with_state(|st| st.log.len());
    for st in fixture()["steps"].as_array().unwrap() {
        let name = s(&st["name"]);
        let before = log_len();
        let (code, body) = fakego::send(
            facade,
            s(&st["method"]),
            s(&st["path"]),
            &[],
            st["body"].as_str().unwrap_or(""),
        )
        .await;
        assert_eq!(
            code.as_u16() as i64,
            st["status"].as_i64().unwrap(),
            "{name}"
        );
        let out: Value = serde_json::from_slice(&body).unwrap();
        assert_eq!(out, st["out"], "{name}");
        assert_eq!(
            go.get("chat", &format!("mode:{id}")).unwrap_or_default(),
            s(&st["mode"]),
            "{name} : mode"
        );
        let crit: Value = go
            .get("chat", &format!("crit:{id}"))
            .map(|v| serde_json::from_str(&v).unwrap())
            .unwrap_or(Value::Null);
        assert_eq!(crit, st["criteria"], "{name} : critères");
        let deltas: Vec<Value> = conv.with_state(|s| {
            s.log[before..]
                .iter()
                .map(|e| {
                    let mut d = e.delta.clone();
                    d.remove("ts");
                    Value::Object(d)
                })
                .collect()
        });
        let want = st["deltas"].as_array().cloned().unwrap_or_default();
        assert_eq!(deltas, want, "{name} : fil");
    }
}

#[tokio::test]
async fn export_par_la_route() {
    let tmp = tempfile::tempdir().unwrap();
    let (facade, _go, _stop, chat) = fakego::start_with(
        tmp.path().to_path_buf(),
        fakego::Opts {
            chat: true,
            ..Default::default()
        },
    )
    .await;
    let chat = chat.unwrap();
    let conv = chat.sessions.conv();
    let mut d = serde_json::Map::new();
    d.insert("user".into(), json!("Salut"));
    conv.append_delta(conv.epoch(), d);
    let mut d = serde_json::Map::new();
    d.insert("content".into(), json!("Bonjour !"));
    conv.append_delta(conv.epoch(), d);
    let (code, headers, body) =
        fakego::get(facade, "/api/chat/export?format=json&reasoning=0", &[]).await;
    assert_eq!(code, 200);
    assert_eq!(headers["content-type"], "application/json; charset=utf-8");
    let disp = headers["content-disposition"].to_str().unwrap();
    assert!(
        disp.starts_with("attachment; filename=\"loki-conversation-") && disp.ends_with(".json\""),
        "{disp}"
    );
    assert_eq!(headers["content-length"], body.len().to_string().as_str());
    let v: Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(v["log"].as_array().unwrap().len(), 2);
    assert!(v["exported_at"].as_str().unwrap().contains('T'));
    let (_, headers, body) = fakego::get(facade, "/api/chat/export", &[]).await;
    assert_eq!(headers["content-type"], "text/markdown; charset=utf-8");
    let md = String::from_utf8(body.to_vec()).unwrap();
    assert!(
        md.contains("## Vous\n\nSalut\n") && md.contains("## Loki\n\nBonjour !\n"),
        "{md}"
    );
}

#[tokio::test]
async fn sans_la_conversation_ces_routes_restent_au_go() {
    let tmp = tempfile::tempdir().unwrap();
    let (facade, _go, _stop) = fakego::start(tmp.path().to_path_buf()).await;
    for p in ["/api/chat/export", "/api/chat/mode", "/api/chat/criteria"] {
        let (_, _, b) = fakego::get(facade, p, &[]).await;
        assert_eq!(&b[..], format!("go:{p}").as_bytes());
    }
}
