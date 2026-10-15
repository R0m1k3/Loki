//! Client MCP (Model Context Protocol) : JSON-RPC 2.0 sur deux transports,
//! comme le SDK Go qu'utilisait `mcp_client.go` :
//!
//! - stdio : un processus local, un message JSON par ligne sur stdin/stdout ;
//! - Streamable HTTP : un POST par message, réponse en JSON ou en flux SSE,
//!   session suivie par l'en-tête `Mcp-Session-Id`.
//!
//! La session s'ouvre par `initialize` puis `notifications/initialized` ; le
//! client ne sert que `tools/list` (première page, comme le Go) et
//! `tools/call`. Une requête du serveur (`ping`) reçoit une réponse vide, les
//! autres « méthode inconnue ».

use std::collections::{BTreeMap, HashMap};
use std::sync::atomic::{AtomicBool, AtomicI64, Ordering};
use std::sync::{Arc, Mutex};

use serde_json::{Map, Value, json};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::process::{Child, ChildStdin};
use tokio::sync::oneshot;

use super::config::ServerConfig;

/// Version du protocole demandée (celle du SDK Go 1.6).
pub const PROTOCOL_VERSION: &str = "2025-11-25";

type Pending = Arc<Mutex<HashMap<i64, oneshot::Sender<Value>>>>;

struct Stdio {
    stdin: Arc<tokio::sync::Mutex<ChildStdin>>,
    pending: Pending,
    child: tokio::sync::Mutex<Option<Child>>,
    closed: Arc<AtomicBool>,
}

struct Http {
    client: reqwest::Client,
    url: String,
    headers: BTreeMap<String, String>,
    session_id: Mutex<Option<String>>,
    protocol: Mutex<Option<String>>,
}

enum Transport {
    Stdio(Stdio),
    Http(Http),
}

/// Une session MCP ouverte.
pub struct Session {
    transport: Transport,
    next_id: AtomicI64,
}

/// Le message d'erreur d'`exec` du Go pour un binaire introuvable.
fn spawn_error(cmd: &str, e: &std::io::Error) -> String {
    if e.kind() == std::io::ErrorKind::NotFound {
        if cmd.contains('/') {
            return format!("fork/exec {cmd}: no such file or directory");
        }
        return format!("exec: \"{cmd}\": executable file not found in $PATH");
    }
    e.to_string()
}

/// Message d'une erreur JSON-RPC (`{code, message}`).
fn rpc_error(method: &str, err: &Value) -> String {
    let msg = err
        .get("message")
        .and_then(Value::as_str)
        .unwrap_or("erreur JSON-RPC");
    format!("calling \"{method}\": {msg}")
}

impl Session {
    /// Ouvre la session : processus ou point d'accès HTTP, puis la poignée de
    /// main. `version` : celle annoncée dans `clientInfo`.
    pub async fn connect(cfg: &ServerConfig, version: &str) -> Result<Session, String> {
        let transport = match cfg.transport() {
            "stdio" => Transport::Stdio(spawn(cfg)?),
            "http" => {
                crate::chat::engine::install_crypto();
                Transport::Http(Http {
                    client: reqwest::Client::builder()
                        .build()
                        .map_err(|e| e.to_string())?,
                    url: cfg.url.trim().to_string(),
                    headers: cfg.headers.clone(),
                    session_id: Mutex::new(None),
                    protocol: Mutex::new(None),
                })
            }
            _ => return Err("serveur MCP mal configuré (ni command ni url)".into()),
        };
        let s = Session {
            transport,
            next_id: AtomicI64::new(1),
        };
        let init = s
            .request(
                "initialize",
                json!({
                    "protocolVersion": PROTOCOL_VERSION,
                    "capabilities": {},
                    "clientInfo": { "name": "loki", "version": version },
                }),
            )
            .await;
        let init = match init {
            Ok(v) => v,
            Err(e) => {
                s.close().await;
                return Err(e);
            }
        };
        if let Transport::Http(h) = &s.transport {
            let v = init
                .get("protocolVersion")
                .and_then(Value::as_str)
                .unwrap_or(PROTOCOL_VERSION)
                .to_string();
            *h.protocol.lock().unwrap_or_else(|e| e.into_inner()) = Some(v);
        }
        if let Err(e) = s.notify("notifications/initialized", json!({})).await {
            s.close().await;
            return Err(e);
        }
        Ok(s)
    }

    /// `tools/list` (première page, comme le client Go).
    pub async fn list_tools(&self) -> Result<Vec<Value>, String> {
        let r = self.request("tools/list", json!({})).await?;
        Ok(r.get("tools")
            .and_then(Value::as_array)
            .cloned()
            .unwrap_or_default())
    }

    /// `tools/call` : le résultat brut (`content`, `structuredContent`,
    /// `isError`).
    pub async fn call_tool(&self, name: &str, args: &Map<String, Value>) -> Result<Value, String> {
        self.request(
            "tools/call",
            json!({ "name": name, "arguments": Value::Object(args.clone()) }),
        )
        .await
    }

    /// La session est-elle tombée (processus mort) ?
    pub fn is_closed(&self) -> bool {
        match &self.transport {
            Transport::Stdio(s) => s.closed.load(Ordering::SeqCst),
            Transport::Http(_) => false,
        }
    }

    /// Ferme la session : processus tué, ou session HTTP terminée.
    pub async fn close(&self) {
        match &self.transport {
            Transport::Stdio(s) => {
                s.closed.store(true, Ordering::SeqCst);
                if let Some(mut c) = s.child.lock().await.take() {
                    let _ = c.kill().await;
                }
            }
            Transport::Http(h) => {
                let sid = h
                    .session_id
                    .lock()
                    .unwrap_or_else(|e| e.into_inner())
                    .clone();
                if let Some(sid) = sid {
                    let mut rq = h.client.delete(&h.url).header("Mcp-Session-Id", sid);
                    for (k, v) in &h.headers {
                        rq = rq.header(k.as_str(), v.as_str());
                    }
                    let _ =
                        tokio::time::timeout(std::time::Duration::from_secs(5), rq.send()).await;
                }
            }
        }
    }

    async fn request(&self, method: &str, params: Value) -> Result<Value, String> {
        let id = self.next_id.fetch_add(1, Ordering::SeqCst);
        let msg = json!({ "jsonrpc": "2.0", "id": id, "method": method, "params": params });
        let resp = match &self.transport {
            Transport::Stdio(s) => s.request(id, method, &msg).await?,
            Transport::Http(h) => h.request(id, method, &msg).await?,
        };
        if let Some(err) = resp.get("error").filter(|e| !e.is_null()) {
            return Err(rpc_error(method, err));
        }
        Ok(resp.get("result").cloned().unwrap_or(Value::Null))
    }

    async fn notify(&self, method: &str, params: Value) -> Result<(), String> {
        let msg = json!({ "jsonrpc": "2.0", "method": method, "params": params });
        match &self.transport {
            Transport::Stdio(s) => s.write(&msg).await,
            Transport::Http(h) => h.post(&msg).await.map(|_| ()),
        }
    }
}

impl Drop for Session {
    fn drop(&mut self) {
        if let Transport::Stdio(s) = &self.transport {
            s.closed.store(true, Ordering::SeqCst);
            if let Ok(mut g) = s.child.try_lock()
                && let Some(c) = g.as_mut()
            {
                let _ = c.start_kill();
            }
        }
    }
}

// ---------------------------------------------------------------------------
// stdio
// ---------------------------------------------------------------------------

fn spawn(cfg: &ServerConfig) -> Result<Stdio, String> {
    let mut cmd = tokio::process::Command::new(&cfg.command);
    cmd.args(&cfg.args)
        .envs(&cfg.env)
        .stdin(std::process::Stdio::piped())
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::null())
        .kill_on_drop(true);
    let mut child = cmd.spawn().map_err(|e| spawn_error(&cfg.command, &e))?;
    let stdin = Arc::new(tokio::sync::Mutex::new(
        child.stdin.take().ok_or("stdin indisponible")?,
    ));
    let stdout = child.stdout.take().ok_or("stdout indisponible")?;
    let pending: Pending = Arc::new(Mutex::new(HashMap::new()));
    let closed = Arc::new(AtomicBool::new(false));
    let (p2, c2, w2) = (pending.clone(), closed.clone(), stdin.clone());
    tokio::spawn(async move {
        let mut lines = BufReader::new(stdout).lines();
        while let Ok(Some(line)) = lines.next_line().await {
            let Ok(v) = serde_json::from_str::<Value>(&line) else {
                continue;
            };
            if let Some(reply) = dispatch(v, &p2) {
                let mut b = serde_json::to_vec(&reply).unwrap_or_default();
                b.push(b'\n');
                let mut w = w2.lock().await;
                let _ = w.write_all(&b).await;
                let _ = w.flush().await;
            }
        }
        c2.store(true, Ordering::SeqCst);
        // Les appels en attente échouent : leur émetteur disparaît.
        p2.lock().unwrap_or_else(|e| e.into_inner()).clear();
    });
    Ok(Stdio {
        stdin,
        pending,
        child: tokio::sync::Mutex::new(Some(child)),
        closed,
    })
}

/// Une ligne lue : réponse à un appel en attente (rien à renvoyer), requête
/// du serveur (la réponse à lui écrire), ou notification (ignorée).
fn dispatch(v: Value, pending: &Pending) -> Option<Value> {
    let has_method = v.get("method").is_some();
    match (v.get("id"), has_method) {
        (Some(id), false) => {
            if let Some(n) = id.as_i64()
                && let Some(tx) = pending.lock().unwrap_or_else(|e| e.into_inner()).remove(&n)
            {
                let _ = tx.send(v);
            }
            None
        }
        (Some(id), true) => Some(if v.get("method").and_then(Value::as_str) == Some("ping") {
            json!({ "jsonrpc": "2.0", "id": id, "result": {} })
        } else {
            json!({ "jsonrpc": "2.0", "id": id,
                    "error": { "code": -32601, "message": "method not found" } })
        }),
        _ => None,
    }
}

impl Stdio {
    async fn write(&self, msg: &Value) -> Result<(), String> {
        if self.closed.load(Ordering::SeqCst) {
            return Err("connection closed".into());
        }
        let mut line = serde_json::to_vec(msg).map_err(|e| e.to_string())?;
        line.push(b'\n');
        let mut w = self.stdin.lock().await;
        w.write_all(&line).await.map_err(|e| e.to_string())?;
        w.flush().await.map_err(|e| e.to_string())
    }

    async fn request(&self, id: i64, method: &str, msg: &Value) -> Result<Value, String> {
        let (tx, rx) = oneshot::channel();
        self.pending
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .insert(id, tx);
        if let Err(e) = self.write(msg).await {
            self.pending
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .remove(&id);
            return Err(format!("calling \"{method}\": {e}"));
        }
        rx.await
            .map_err(|_| format!("calling \"{method}\": connection closed"))
    }
}

// ---------------------------------------------------------------------------
// Streamable HTTP
// ---------------------------------------------------------------------------

impl Http {
    fn builder(&self, msg: &Value) -> reqwest::RequestBuilder {
        let mut rq = self
            .client
            .post(&self.url)
            .header("Content-Type", "application/json")
            .header("Accept", "application/json, text/event-stream")
            .body(serde_json::to_vec(msg).unwrap_or_default());
        if let Some(sid) = self
            .session_id
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone()
        {
            rq = rq.header("Mcp-Session-Id", sid);
        }
        if let Some(p) = self
            .protocol
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone()
        {
            rq = rq.header("MCP-Protocol-Version", p);
        }
        for (k, v) in &self.headers {
            rq = rq.header(k.as_str(), v.as_str());
        }
        rq
    }

    async fn post(&self, msg: &Value) -> Result<reqwest::Response, String> {
        let resp = self.builder(msg).send().await.map_err(|e| e.to_string())?;
        if let Some(sid) = resp
            .headers()
            .get("mcp-session-id")
            .and_then(|v| v.to_str().ok())
        {
            *self.session_id.lock().unwrap_or_else(|e| e.into_inner()) = Some(sid.to_string());
        }
        if !resp.status().is_success() {
            let st = resp.status();
            let body = resp.text().await.unwrap_or_default();
            let body = body.trim();
            return Err(if body.is_empty() {
                st.to_string()
            } else {
                format!("{st}: {body}")
            });
        }
        Ok(resp)
    }

    async fn request(&self, id: i64, method: &str, msg: &Value) -> Result<Value, String> {
        let resp = self
            .post(msg)
            .await
            .map_err(|e| format!("calling \"{method}\": {e}"))?;
        let ctype = resp
            .headers()
            .get("content-type")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("")
            .to_ascii_lowercase();
        let body = resp
            .text()
            .await
            .map_err(|e| format!("calling \"{method}\": {e}"))?;
        let mut candidates: Vec<Value> = Vec::new();
        if ctype.starts_with("text/event-stream") {
            let mut data = String::new();
            for line in body.lines().chain(std::iter::once("")) {
                if line.is_empty() {
                    if !data.is_empty() {
                        if let Ok(v) = serde_json::from_str::<Value>(&data) {
                            candidates.push(v);
                        }
                        data.clear();
                    }
                } else if let Some(d) = line.strip_prefix("data:") {
                    if !data.is_empty() {
                        data.push('\n');
                    }
                    data.push_str(d.strip_prefix(' ').unwrap_or(d));
                }
            }
        } else if let Ok(v) = serde_json::from_str::<Value>(&body) {
            match v {
                Value::Array(a) => candidates.extend(a),
                v => candidates.push(v),
            }
        }
        candidates
            .into_iter()
            .find(|v| v.get("id").and_then(Value::as_i64) == Some(id) && v.get("method").is_none())
            .ok_or_else(|| format!("calling \"{method}\": réponse absente"))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn erreurs_comme_go() {
        let e = std::io::Error::from(std::io::ErrorKind::NotFound);
        assert_eq!(
            spawn_error("bidon", &e),
            "exec: \"bidon\": executable file not found in $PATH"
        );
        assert_eq!(
            rpc_error(
                "tools/call",
                &json!({"code": -32602, "message": "outil inconnu"})
            ),
            "calling \"tools/call\": outil inconnu"
        );
    }
}
