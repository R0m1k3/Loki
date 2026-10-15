//! Faux serveur Go : API interne (`/internal/v1/auth`, `/internal/v1/store`,
//! `/internal/v1/store/all`) sur un store en mémoire, et `go:<chemin>` pour
//! toute autre route (ce qui prouve qu'elle a été relayée).

use bytes::Bytes;
use http_body_util::{BodyExt, Full};
use hyper::body::Incoming;
use hyper::{Method, Request, Response, StatusCode, service::service_fn};
use hyper_util::rt::TokioIo;
use std::collections::{BTreeMap, HashMap};
use std::convert::Infallible;
use std::net::SocketAddr;
use std::sync::{Arc, Mutex};
use tokio::net::TcpListener;
use tokio::sync::oneshot;

pub const JETON: &str = "0123456789abcdef0123456789abcdef";

/// Scellement simulé par le faux Go.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum CryptMode {
    /// « SCELLE: » + clair (historique des essais de la conversation).
    #[default]
    Scelle,
    /// Chiffrement coupé : rien ne change.
    Plain,
    /// Magic des pages mémoire (`LOKIMEMv1`) + clair.
    Magic,
}

#[derive(Default)]
pub struct FakeGo {
    /// bucket → clé → valeur.
    pub store: Mutex<HashMap<String, BTreeMap<String, Vec<u8>>>>,
    /// Base illisible : toute l'API interne répond 503.
    pub broken: Mutex<bool>,
    /// Chemins relayés (hors API interne).
    pub relayed: Mutex<Vec<String>>,
    /// /v1 : port du faux moteur, second slot en service, refus à renvoyer
    /// par begin, journal des begin/end.
    pub engine_port: Mutex<u16>,
    pub side_live: Mutex<bool>,
    pub refuse: Mutex<Option<serde_json::Value>>,
    pub oai_log: Mutex<Vec<String>>,
    /// Mémoire verrouillée : seal/open répondent 423.
    pub locked: Mutex<bool>,
    /// Bail du moteur : bail courant, stop signalé, détenteur Go, journal.
    pub lease: Mutex<Option<String>>,
    pub lease_stop: Mutex<bool>,
    pub go_busy: Mutex<Option<String>>,
    pub lease_log: Mutex<Vec<String>>,
    /// Résultat imposé à tout outil (sinon « exec:<nom> »), et archive recall.
    pub tool_result: Mutex<Option<String>>,
    pub recall: Mutex<Vec<String>>,
    /// Discussion active annoncée et discussions supprimées.
    pub conv_log: Mutex<Vec<String>>,
    /// Tours : débuts, vues et fins demandés.
    pub turn_log: Mutex<Vec<String>>,
    /// Adresse d'écoute du faux Go (pour un client `Store` direct).
    pub backend: Mutex<Option<SocketAddr>>,
    /// Contexte des outils exécutés par la façade (None : 404, le Go les
    /// exécute), et journal du suivi (lectures, écritures, contrôles).
    pub tool_ctx: Mutex<Option<serde_json::Value>>,
    pub tracker_log: Mutex<Vec<String>>,
    /// Tours en mode Code, et outils exécutés (« nom rôle »). L'outil
    /// criteria (action=set) modifie les critères rangés (chat/crit:*).
    pub code: Mutex<bool>,
    pub tool_log: Mutex<Vec<String>>,
    /// Le Go garde la conversation (raison), sinon la façade la tient.
    pub go_keeps_chat: Mutex<Option<String>>,
    /// Réponses imposées de /turn/start et /turn/view (journaux de référence
    /// du Go), et résultat par outil.
    pub turn_start: Mutex<Option<serde_json::Value>>,
    pub turn_view: Mutex<Option<serde_json::Value>>,
    pub turn_verifier: Mutex<Option<serde_json::Value>>,
    pub tool_results: Mutex<HashMap<String, String>>,
    /// /internal/v1/tune/guard : message d'une optimisation en cours.
    pub tune_busy: Mutex<Option<String>>,
    /// /internal/v1/preset/apply reçus : (chemin, mode).
    pub preset_applies: Mutex<Vec<(String, String)>>,
    /// /internal/v1/preset/apply écrit-il le preset dans la configuration
    /// (`applyPresetFile` : clés « appareil » et BIN de la machine gardés) ?
    pub apply_presets: Mutex<bool>,
    /// /internal/v1/chat/generating : le Go écrit une réponse.
    pub generating: Mutex<bool>,
    /// /internal/v1/service/prestart reçus.
    pub prestarts: Mutex<u32>,
    /// /internal/v1/dictate/stop reçus.
    pub dictate_stops: Mutex<u32>,
    /// /internal/v1/web/reach/reset reçus.
    pub reach_resets: Mutex<u32>,
    /// /internal/v1/cdp/shutdown reçus.
    pub cdp_shutdowns: Mutex<u32>,
    /// /internal/v1/service/action reçus : (action, if_active).
    pub service_actions: Mutex<Vec<(String, bool)>>,
    /// Le moteur tourne-t-il (`if_active` des actions de service) ?
    pub service_active: Mutex<bool>,
    /// Comportement de /internal/v1/crypt/* (préfixe « SCELLE: » par défaut).
    pub crypt_mode: Mutex<CryptMode>,
    /// Réponse de /internal/v1/conv/workspace (None : 404).
    pub conv_workspace: Mutex<Option<serde_json::Value>>,
}

impl FakeGo {
    pub fn set(&self, bucket: &str, key: &str, val: &str) {
        self.store
            .lock()
            .unwrap()
            .entry(bucket.into())
            .or_default()
            .insert(key.into(), val.as_bytes().to_vec());
    }
    pub fn del(&self, bucket: &str, key: &str) {
        if let Some(b) = self.store.lock().unwrap().get_mut(bucket) {
            b.remove(key);
        }
    }
    pub fn get(&self, bucket: &str, key: &str) -> Option<String> {
        let s = self.store.lock().unwrap();
        s.get(bucket)?
            .get(key)
            .map(|v| String::from_utf8_lossy(v).into_owned())
    }
}

fn query(req: &Request<Incoming>) -> HashMap<String, String> {
    req.uri()
        .query()
        .unwrap_or("")
        .split('&')
        .filter_map(|kv| kv.split_once('='))
        .map(|(k, v)| (k.to_string(), decode(v)))
        .collect()
}

fn decode(s: &str) -> String {
    let b = s.as_bytes();
    let mut out = Vec::new();
    let mut i = 0;
    while i < b.len() {
        if b[i] == b'%' && i + 2 < b.len() {
            let h = std::str::from_utf8(&b[i + 1..i + 3]).unwrap();
            out.push(u8::from_str_radix(h, 16).unwrap());
            i += 3;
        } else {
            out.push(b[i]);
            i += 1;
        }
    }
    String::from_utf8(out).unwrap()
}

fn resp(code: u16, body: impl Into<Bytes>) -> Response<Full<Bytes>> {
    let mut r = Response::new(Full::new(body.into()));
    *r.status_mut() = StatusCode::from_u16(code).unwrap();
    r
}

async fn handle(st: Arc<FakeGo>, req: Request<Incoming>) -> Response<Full<Bytes>> {
    let path = req.uri().path().to_string();
    if !path.starts_with("/internal/") {
        st.relayed.lock().unwrap().push(path.clone());
        if path == "/api/chat/mode" && req.method() == Method::POST {
            let body = req.into_body().collect().await.unwrap().to_bytes();
            let v: serde_json::Value = serde_json::from_slice(&body).unwrap_or_default();
            return resp(
                200,
                serde_json::json!({"ok": true, "mode": v["mode"]}).to_string(),
            );
        }
        return resp(200, format!("go:{path}"));
    }
    if req.headers().get("x-loki-internal").map(|v| v.as_bytes()) != Some(JETON.as_bytes()) {
        return resp(404, "");
    }
    if *st.broken.lock().unwrap() {
        return resp(503, r#"{"error":"base illisible"}"#);
    }
    let q = query(&req);
    let method = req.method().clone();
    match path.as_str() {
        "/internal/v1/auth" => {
            let key = st.get("state", "web_key").unwrap_or_default();
            resp(200, format!(r#"{{"web_key":"{key}","version":"0.15.0"}}"#))
        }
        "/internal/v1/crypt/seal" | "/internal/v1/crypt/open" => {
            if *st.locked.lock().unwrap() {
                return resp(423, r#"{"error":"mémoire verrouillée"}"#);
            }
            let body = req.into_body().collect().await.unwrap().to_bytes();
            match *st.crypt_mode.lock().unwrap() {
                // Chiffrement coupé : clair rendu tel quel.
                CryptMode::Plain => return resp(200, body),
                // Enveloppe au magic des pages (`LOKIMEMv1`).
                CryptMode::Magic => {
                    return if path.ends_with("seal") {
                        let mut out = b"LOKIMEMv1".to_vec();
                        out.extend_from_slice(&body);
                        resp(200, out)
                    } else {
                        resp(
                            200,
                            body.strip_prefix(b"LOKIMEMv1").unwrap_or(&body).to_vec(),
                        )
                    };
                }
                CryptMode::Scelle => {}
            }
            if path.ends_with("seal") {
                let mut out = b"SCELLE:".to_vec();
                out.extend_from_slice(&body);
                resp(200, out)
            } else {
                resp(200, body.strip_prefix(b"SCELLE:").unwrap_or(&body).to_vec())
            }
        }
        "/internal/v1/engine/lease" => {
            let body = req.into_body().collect().await.unwrap().to_bytes();
            let v: serde_json::Value = serde_json::from_slice(&body).unwrap_or_default();
            if let Some(why) = st.go_busy.lock().unwrap().clone() {
                let out = serde_json::json!({"error": why, "task_id": "t1", "task_name": "veille"});
                return resp(409, out.to_string());
            }
            let mut lease = st.lease.lock().unwrap();
            if lease.is_some() {
                return resp(409, r#"{"error":"génération en cours"}"#);
            }
            let n = st.lease_log.lock().unwrap().len();
            let id = format!("bail{n}");
            *lease = Some(id.clone());
            st.lease_log
                .lock()
                .unwrap()
                .push(format!("acquire {}", v["ttl_ms"]));
            resp(
                200,
                serde_json::json!({"lease": id, "ttl_ms": v["ttl_ms"]}).to_string(),
            )
        }
        "/internal/v1/engine/lease/renew" | "/internal/v1/engine/lease/release" => {
            let body = req.into_body().collect().await.unwrap().to_bytes();
            let v: serde_json::Value = serde_json::from_slice(&body).unwrap_or_default();
            let id = v["lease"].as_str().unwrap_or_default().to_string();
            let mut lease = st.lease.lock().unwrap();
            let held = lease.as_deref() == Some(id.as_str());
            if path.ends_with("release") {
                st.lease_log.lock().unwrap().push(format!("release {id}"));
                if held {
                    *lease = None;
                }
                return resp(204, "");
            }
            st.lease_log.lock().unwrap().push(format!("renew {id}"));
            if !held {
                return resp(410, r#"{"error":"bail perdu"}"#);
            }
            let stop = *st.lease_stop.lock().unwrap();
            resp(200, serde_json::json!({"stop": stop}).to_string())
        }
        "/internal/v1/recall/archive" => {
            let body = req.into_body().collect().await.unwrap().to_bytes();
            let v: serde_json::Value = serde_json::from_slice(&body).unwrap_or_default();
            let mut r = st.recall.lock().unwrap();
            r.push(v["content"].as_str().unwrap_or_default().to_string());
            resp(
                200,
                serde_json::json!({"id": format!("r{}", r.len())}).to_string(),
            )
        }
        "/internal/v1/conv/active" | "/internal/v1/conv/dropped" => {
            let body = req.into_body().collect().await.unwrap().to_bytes();
            let v: serde_json::Value = serde_json::from_slice(&body).unwrap_or_default();
            let what = path.rsplit('/').next().unwrap();
            st.conv_log
                .lock()
                .unwrap()
                .push(format!("{what} {}", v["id"].as_str().unwrap_or_default()));
            resp(204, "")
        }
        "/internal/v1/turn/start" => {
            let body = req.into_body().collect().await.unwrap().to_bytes();
            let v: serde_json::Value = serde_json::from_slice(&body).unwrap_or_default();
            let msg = v["message"].as_str().unwrap_or_default().to_string();
            st.turn_log.lock().unwrap().push(format!("start {msg}"));
            if msg.trim().is_empty() {
                return resp(400, r#"{"error":"message vide"}"#);
            }
            if let Some(v) = st.turn_start.lock().unwrap().clone() {
                return resp(200, v.to_string());
            }
            let code = *st.code.lock().unwrap();
            let caps = serde_json::json!({"Agent": true, "Mem": "off", "Code": code, "Role": ""});
            let out = serde_json::json!({
                "files": [], "caps": caps, "turn_caps": caps,
                "content": msg, "model": "m1", "code_hint": false,
            });
            resp(200, out.to_string())
        }
        "/internal/v1/turn/view" => {
            st.turn_log.lock().unwrap().push("view".into());
            if let Some(v) = st.turn_view.lock().unwrap().clone() {
                return resp(200, v.to_string());
            }
            let out = serde_json::json!({
                "head": [{"role": "system", "content": "Tu es Loki."}],
                "prefix": "PRÉAMBULE",
                "tools": [{"type": "function", "function": {"name": "bash", "description": "", "parameters": {"type": "object"}}}],
            });
            resp(200, out.to_string())
        }
        "/internal/v1/chat/holder" => match st.go_keeps_chat.lock().unwrap().clone() {
            None => resp(200, r#"{"holder":"rust"}"#),
            Some(why) => resp(
                200,
                serde_json::json!({"holder": "go", "reason": why}).to_string(),
            ),
        },
        "/internal/v1/project/activate" => {
            let body = req.into_body().collect().await.unwrap().to_bytes();
            let v: serde_json::Value = serde_json::from_slice(&body).unwrap_or_default();
            let slug = v["slug"].as_str().unwrap_or_default().to_string();
            if slug.is_empty() || slug == "absent" {
                return resp(400, r#"{"error":"projet introuvable"}"#);
            }
            st.conv_log.lock().unwrap().push(format!("project {slug}"));
            st.set("state", "active_project", &slug);
            resp(204, "")
        }
        // Conversation tenue par le Go (lot 18) : bascule (projectSwitch) et
        // discussions d'un projet supprimé (convDelete), sur l'index en base.
        "/internal/v1/project/switch" | "/internal/v1/project/drop-convs" => {
            let body = req.into_body().collect().await.unwrap().to_bytes();
            let v: serde_json::Value = serde_json::from_slice(&body).unwrap_or_default();
            let slug = v["slug"].as_str().unwrap_or_default().trim().to_string();
            if *st.generating.lock().unwrap() {
                return resp(409, r#"{"error":"une génération est en cours"}"#);
            }
            let mut idx: Vec<serde_json::Value> = st
                .get("chat", "index")
                .and_then(|s| serde_json::from_str(&s).ok())
                .unwrap_or_default();
            let proj = |m: &serde_json::Value| match m["project"].as_str().unwrap_or("").trim() {
                "" => "generale".to_string(),
                s => s.to_string(),
            };
            if path.ends_with("drop-convs") {
                let gone: Vec<String> = idx
                    .iter()
                    .filter(|m| proj(m) == slug)
                    .map(|m| m["id"].as_str().unwrap_or("").to_string())
                    .collect();
                idx.retain(|m| proj(m) != slug);
                st.set("chat", "index", &serde_json::to_string(&idx).unwrap());
                for id in &gone {
                    st.del("chat", &format!("conv:{id}"));
                }
                return resp(
                    200,
                    serde_json::json!({ "dropped": gone.len() }).to_string(),
                );
            }
            let known = st
                .get("state", "projects")
                .and_then(|s| serde_json::from_str::<Vec<serde_json::Value>>(&s).ok())
                .unwrap_or_default()
                .iter()
                .any(|p| p["slug"] == slug.as_str());
            if !known {
                return resp(400, r#"{"error":"projet introuvable"}"#);
            }
            if st.get("state", "active_project").as_deref() == Some(slug.as_str()) {
                return resp(204, "");
            }
            st.set("state", "active_project", &slug);
            if !idx.iter().any(|m| proj(m) == slug) {
                let id = format!("c{}", idx.len() + 1_000_000);
                idx.push(serde_json::json!({ "id": id, "title": "", "created": 0, "updated": 0, "turns": 0, "project": slug }));
                st.set("chat", "index", &serde_json::to_string(&idx).unwrap());
            }
            resp(204, "")
        }
        "/internal/v1/turn/verifier" => {
            let body = req.into_body().collect().await.unwrap().to_bytes();
            let v: serde_json::Value = serde_json::from_slice(&body).unwrap_or_default();
            st.turn_log.lock().unwrap().push("verifier".into());
            if let Some(out) = st.turn_verifier.lock().unwrap().clone() {
                return resp(200, out.to_string());
            }
            let mut caps = v["caps"].clone();
            caps["Role"] = "verifier".into();
            let out = serde_json::json!({
                "system": "VÉRIFICATEUR",
                "tools": [{"type": "function", "function": {"name": "criteria", "description": "", "parameters": {"type": "object"}}}],
                "caps": caps,
            });
            resp(200, out.to_string())
        }
        "/internal/v1/turn/done" => {
            st.turn_log.lock().unwrap().push("done".into());
            resp(204, "")
        }
        "/internal/v1/engine/stop" => {
            st.lease_log.lock().unwrap().push("stop".into());
            *st.go_busy.lock().unwrap() = None;
            resp(204, "")
        }
        "/internal/v1/tune/guard" => match st.tune_busy.lock().unwrap().clone() {
            Some(e) => resp(
                200,
                serde_json::json!({ "busy": true, "error": e }).to_string(),
            ),
            None => resp(200, r#"{"busy":false}"#),
        },
        "/internal/v1/service/prestart" => {
            *st.prestarts.lock().unwrap() += 1;
            let e = st.tune_busy.lock().unwrap().clone().unwrap_or_default();
            resp(200, serde_json::json!({ "error": e }).to_string())
        }
        "/internal/v1/chat/generating" => resp(
            200,
            serde_json::json!({ "generating": *st.generating.lock().unwrap() }).to_string(),
        ),
        "/internal/v1/web/reach/reset" => {
            *st.reach_resets.lock().unwrap() += 1;
            resp(204, "")
        }
        "/internal/v1/cdp/shutdown" => {
            *st.cdp_shutdowns.lock().unwrap() += 1;
            resp(204, "")
        }
        "/internal/v1/service/action" => {
            let body = req.into_body().collect().await.unwrap().to_bytes();
            let v: serde_json::Value = serde_json::from_slice(&body).unwrap_or_default();
            let if_active = v["if_active"].as_bool().unwrap_or(false);
            st.service_actions
                .lock()
                .unwrap()
                .push((v["action"].as_str().unwrap_or("").to_string(), if_active));
            if if_active && !*st.service_active.lock().unwrap() {
                return resp(200, r#"{"ran":false,"error":""}"#);
            }
            resp(200, r#"{"ran":true,"error":""}"#)
        }
        "/internal/v1/dictate/stop" => {
            *st.dictate_stops.lock().unwrap() += 1;
            resp(204, "")
        }
        "/internal/v1/preset/apply" => {
            let body = req.into_body().collect().await.unwrap().to_bytes();
            let v: serde_json::Value = serde_json::from_slice(&body).unwrap_or_default();
            let path = v["path"].as_str().unwrap_or("").to_string();
            let mode = v["mode"].as_str().unwrap_or("").to_string();
            if mode == "refus" || path.ends_with("refus.env") {
                return resp(500, r#"{"error":"application refusée"}"#);
            }
            if *st.apply_presets.lock().unwrap() {
                let text = std::fs::read_to_string(&path).unwrap_or_default();
                let mut next = loki_rs::presets::parse_env(&text);
                let mut store = st.store.lock().unwrap();
                let cur = store.entry("config".into()).or_default();
                for k in loki_rs::presets::PRESERVED_KEYS {
                    if let Some(v) = cur.get(*k) {
                        next.insert(k.to_string(), String::from_utf8_lossy(v).into_owned());
                    }
                }
                if let Some(bin) = cur.get("BIN").filter(|v| !v.is_empty()) {
                    next.insert("BIN".into(), String::from_utf8_lossy(bin).into_owned());
                }
                *cur = next.into_iter().map(|(k, v)| (k, v.into_bytes())).collect();
            }
            st.preset_applies.lock().unwrap().push((path, mode));
            resp(204, "")
        }
        "/internal/v1/conv/workspace" => match st.conv_workspace.lock().unwrap().clone() {
            Some(v) => resp(200, v.to_string()),
            None => resp(404, "404 page not found"),
        },
        "/internal/v1/tool/context" => match st.tool_ctx.lock().unwrap().clone() {
            Some(v) => resp(200, v.to_string()),
            None => resp(404, "404 page not found"),
        },
        "/internal/v1/tool/tracker" | "/internal/v1/tool/wrote" => {
            let body = req.into_body().collect().await.unwrap().to_bytes();
            let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
            let op = v["op"].as_str().unwrap_or("wrote").to_string();
            let path = v["path"].as_str().unwrap_or("").to_string();
            st.tracker_log.lock().unwrap().push(format!("{op} {path}"));
            let out = if op == "check" {
                serde_json::json!({ "refusal": "" })
            } else {
                serde_json::json!({ "lsp": "" })
            };
            resp(200, out.to_string())
        }
        "/internal/v1/tool/exec" => {
            let body = req.into_body().collect().await.unwrap().to_bytes();
            let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
            let name = v["name"].as_str().unwrap_or_default();
            st.tool_log.lock().unwrap().push(format!(
                "{name} {}",
                v["caps"]["Role"].as_str().unwrap_or_default()
            ));
            if name == "criteria" && v["args"]["action"] == "set" {
                let mut chat = st.store.lock().unwrap();
                for (k, val) in chat.entry("chat".into()).or_default().iter_mut() {
                    if !k.starts_with("crit:") {
                        continue;
                    }
                    let mut list: Vec<serde_json::Value> =
                        serde_json::from_slice(val).unwrap_or_default();
                    for c in &mut list {
                        if c["id"] == v["args"]["id"] {
                            c["status"] = v["args"]["status"].clone();
                        }
                    }
                    *val = serde_json::to_vec(&list).unwrap();
                }
            }
            let by_name = st.tool_results.lock().unwrap().get(name).cloned();
            let result = by_name
                .or_else(|| st.tool_result.lock().unwrap().clone())
                .unwrap_or_else(|| format!("exec:{}", v["name"].as_str().unwrap()));
            let out = serde_json::json!({
                "label": v["args"]["command"],
                "result": result,
                "caps_agent": v["caps"]["Agent"],
            });
            resp(200, out.to_string())
        }
        "/internal/v1/oai/route" => {
            let key = st.get("state", "api_key").unwrap_or_default();
            let v = serde_json::json!({
                "port": *st.engine_port.lock().unwrap(),
                "strata": false,
                "key": key,
                "key_ok": true,
                "max_body": 1024,
                "side_live": *st.side_live.lock().unwrap(),
            });
            resp(200, v.to_string())
        }
        "/internal/v1/oai/begin" => {
            let body = req.into_body().collect().await.unwrap().to_bytes();
            let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
            st.oai_log.lock().unwrap().push(format!(
                "begin read={} side={} post_v1={}",
                v["read"], v["side"], v["post_v1"]
            ));
            if let Some(r) = st.refuse.lock().unwrap().clone() {
                return resp(200, serde_json::json!({ "refuse": r }).to_string());
            }
            resp(200, r#"{"ticket":7}"#)
        }
        "/internal/v1/oai/end" => {
            let body = req.into_body().collect().await.unwrap().to_bytes();
            let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
            st.oai_log
                .lock()
                .unwrap()
                .push(format!("end {}", v["ticket"]));
            resp(204, "")
        }
        "/internal/v1/store/all" => {
            let s = st.store.lock().unwrap();
            let m: BTreeMap<String, String> = s
                .get(&q["bucket"])
                .map(|b| {
                    b.iter()
                        .map(|(k, v)| (k.clone(), String::from_utf8_lossy(v).into_owned()))
                        .collect()
                })
                .unwrap_or_default();
            resp(200, serde_json::to_string(&m).unwrap())
        }
        "/internal/v1/store" => {
            let (bucket, key) = (q["bucket"].clone(), q["key"].clone());
            match method {
                Method::GET => match st
                    .store
                    .lock()
                    .unwrap()
                    .get(&bucket)
                    .and_then(|b| b.get(&key))
                {
                    Some(v) => resp(200, v.clone()),
                    None => resp(404, ""),
                },
                Method::PUT => {
                    let body = req.into_body().collect().await.unwrap().to_bytes();
                    st.store
                        .lock()
                        .unwrap()
                        .entry(bucket)
                        .or_default()
                        .insert(key, body.to_vec());
                    resp(204, "")
                }
                Method::DELETE => {
                    if let Some(b) = st.store.lock().unwrap().get_mut(&bucket) {
                        b.remove(&key);
                    }
                    resp(204, "")
                }
                _ => resp(405, ""),
            }
        }
        _ => resp(404, ""),
    }
}

/// Lance le faux Go puis la façade devant lui ; rend (adresse de la façade,
/// état du faux Go, garde d'arrêt).
pub async fn start(home: std::path::PathBuf) -> (SocketAddr, Arc<FakeGo>, oneshot::Sender<()>) {
    let (f, st, tx, _) = start_with(home, Opts::default()).await;
    (f, st, tx)
}

/// Comme [`start`], avec les routes des modèles tenues par la façade
/// (`LOKI_MODELS=rust`) ; `hf` : adresse du faux Hugging Face.
pub async fn start_models(
    home: std::path::PathBuf,
    hf: &str,
) -> (SocketAddr, Arc<FakeGo>, oneshot::Sender<()>) {
    let opts = Opts {
        models_hf: Some(hf.to_string()),
        ..Opts::default()
    };
    let (f, st, tx, _) = start_with(home, opts).await;
    (f, st, tx)
}

/// Comme [`start`], avec la dictée tenue par la façade (`LOKI_DICTATE=rust`).
pub async fn start_dictate(
    home: std::path::PathBuf,
    d: Arc<loki_rs::dictate::Dictation>,
) -> (SocketAddr, Arc<FakeGo>, oneshot::Sender<()>) {
    let opts = Opts {
        dictate: Some(d),
        ..Opts::default()
    };
    let (f, st, tx, _) = start_with(home, opts).await;
    (f, st, tx)
}

/// Ce que la façade tient en plus du proxy.
#[derive(Default)]
pub struct Opts {
    pub chat: bool,
    pub engine_port: Option<u16>,
    pub models_hf: Option<String>,
    pub dictate: Option<Arc<loki_rs::dictate::Dictation>>,
    pub settings: bool,
    pub files: Option<Arc<loki_rs::convfiles::ConvFiles>>,
    pub push: Option<Arc<loki_rs::push::Push>>,
    pub mem: Option<Arc<loki_rs::memory::Memory>>,
    pub presets: bool,
    pub engine_ctl: Option<Arc<loki_rs::enginectl::EngineCtl>>,
    pub system: Option<Arc<loki_rs::system::System>>,
    pub projects: bool,
    pub mcp: Option<Arc<loki_rs::mcp::Mcp>>,
    pub update: bool,
    pub backends: Option<Arc<loki_rs::backends::Backends>>,
    pub engineimg: Option<Arc<loki_rs::engineimg::EngineImg>>,
    pub strata: Option<Arc<loki_rs::strata::api::Strata>>,
}

/// Comme [`start`], avec la conversation tenue par la façade
/// (`LOKI_CHAT=rust`) ; `engine_port` : port du faux moteur (config PORT).
pub async fn start_chat(
    home: std::path::PathBuf,
    engine_port: u16,
) -> (
    SocketAddr,
    Arc<FakeGo>,
    oneshot::Sender<()>,
    Arc<loki_rs::chat::generate::ChatService>,
) {
    let (f, st, tx, chat) = start_with(
        home,
        Opts {
            chat: true,
            engine_port: Some(engine_port),
            ..Opts::default()
        },
    )
    .await;
    (f, st, tx, chat.unwrap())
}

pub async fn start_with(
    home: std::path::PathBuf,
    opts: Opts,
) -> (
    SocketAddr,
    Arc<FakeGo>,
    oneshot::Sender<()>,
    Option<Arc<loki_rs::chat::generate::ChatService>>,
) {
    let st = Arc::new(FakeGo::default());
    if let Some(p) = opts.engine_port {
        st.set("config", "PORT", &p.to_string());
    }
    let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let backend = l.local_addr().unwrap();
    *st.backend.lock().unwrap() = Some(backend);
    let st2 = st.clone();
    tokio::spawn(async move {
        while let Ok((s, _)) = l.accept().await {
            let st = st2.clone();
            tokio::spawn(async move {
                let svc = service_fn(move |req| {
                    let st = st.clone();
                    async move { Ok::<_, Infallible>(handle(st, req).await) }
                });
                let _ = hyper::server::conn::http1::Builder::new()
                    .serve_connection(TokioIo::new(s), svc)
                    .await;
            });
        }
    });
    let chat = if opts.chat {
        use loki_rs::chat::{
            conv::Conversation, generate::ChatService, host::ChatShared, sessions,
        };
        let store = loki_rs::store::Store::new(backend, JETON.into());
        let s = sessions::Sessions::new(
            store.clone(),
            Conversation::new(),
            sessions::GoSessionHooks(store.clone()),
        );
        s.load().await.unwrap();
        let shared = ChatShared::for_sessions(&s, store, home.join("chatimg"));
        Some(ChatService::new(Arc::new(s), Arc::new(shared)))
    } else {
        None
    };
    let fl = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let facade = fl.local_addr().unwrap();
    let (tx, rx) = oneshot::channel::<()>();
    let cfg = loki_rs::proxy::Config {
        backend,
        internal_token: JETON.into(),
        host_env: loki_rs::auth::HostEnv::default(),
        engine: loki_rs::engine::Paths {
            home,
            service: "loki-engine".into(),
        },
        chat: chat.clone(),
        models: opts.models_hf.map(|hf| {
            Arc::new(loki_rs::models::Models {
                downloads: Arc::new(loki_rs::downloads::Downloads::new()),
                hf: loki_rs::hf::HfClient::new(&hf),
            })
        }),
        dictate: opts.dictate,
        settings: opts.settings,
        files: opts.files,
        push: opts.push,
        mem: opts.mem,
        presets: opts.presets,
        engine_ctl: opts.engine_ctl,
        system: opts.system,
        projects: opts.projects,
        mcp: opts.mcp,
        update: opts.update,
        backends: opts.backends,
        engineimg: opts.engineimg,
        strata: opts.strata,
    };
    tokio::spawn(loki_rs::proxy::serve(fl, cfg, async move {
        let _ = rx.await;
    }));
    (facade, st, tx, chat)
}

/// Requête via la façade (corps JSON) : (code, corps).
pub async fn send(
    facade: SocketAddr,
    method: &str,
    path: &str,
    headers: &[(&str, &str)],
    body: &str,
) -> (StatusCode, Bytes) {
    use hyper_util::client::legacy::Client;
    use hyper_util::rt::TokioExecutor;
    let mut req = Request::builder()
        .method(method)
        .uri(format!("http://{facade}{path}"))
        .header("content-type", "application/json");
    for (k, v) in headers {
        req = req.header(*k, *v);
    }
    let client: Client<_, Full<Bytes>> = Client::builder(TokioExecutor::new()).build_http();
    let r = client
        .request(req.body(Full::new(Bytes::from(body.to_string()))).unwrap())
        .await
        .unwrap();
    let (parts, body) = r.into_parts();
    (parts.status, body.collect().await.unwrap().to_bytes())
}

/// GET via la façade : (code, en-têtes, corps).
pub async fn get(
    facade: SocketAddr,
    path: &str,
    headers: &[(&str, &str)],
) -> (StatusCode, hyper::HeaderMap, Bytes) {
    use hyper_util::client::legacy::Client;
    use hyper_util::rt::TokioExecutor;
    let mut req = Request::get(format!("http://{facade}{path}"));
    for (k, v) in headers {
        req = req.header(*k, *v);
    }
    let client: Client<_, Full<Bytes>> = Client::builder(TokioExecutor::new()).build_http();
    let r = client
        .request(req.body(Full::new(Bytes::new())).unwrap())
        .await
        .unwrap();
    let (parts, body) = r.into_parts();
    (
        parts.status,
        parts.headers,
        body.collect().await.unwrap().to_bytes(),
    )
}
