//! Serveurs MCP tenus par la façade (lot 20 de la migration,
//! `LOKI_MCP=rust` ; expérimental, le Go reste le défaut) — port de
//! `mcp_client.go`, `mcp_config.go`, `mcp_catalog.go` et des routes
//! `/api/mcp*` de `web_api.go`.
//!
//! Le pool de sessions vit ici : une session reste ouverte d'un tour à
//! l'autre (un serveur stdio garde son processus), les connexions se font en
//! parallèle, deux appelants simultanés partagent la même tentative. Le Go,
//! qui exécute encore les outils de l'agent, demande à la façade la liste des
//! outils MCP et leurs appels (`/internal/facade/v1/mcp/*`) : un seul pool,
//! quelle que soit la conversation qui tourne.

pub mod client;
pub mod config;

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::{Arc, LazyLock, Mutex};

use futures_util::future::join_all;
use http_body_util::{BodyExt, Limited};
use hyper::body::Incoming;
use hyper::{Request, Response, StatusCode};
use serde_json::{Map, Value, json};

use crate::api::Ctx;
use crate::auth::json as send_json;
use crate::gojson::{self, Kind};
use crate::proxy::Body;
use crate::store::{BK_STATE, Store};
use client::Session;
use config::{ServerConfig, Servers};

/// Clé de la configuration (`state/mcp`).
pub const ST_MCP: &str = "mcp";

/// Chemins de l'API interne de la façade, pour le Go.
pub const INTERNAL_TOOLS: &str = "/internal/facade/v1/mcp/tools";
pub const INTERNAL_CALL: &str = "/internal/facade/v1/mcp/call";
pub const INTERNAL_PREWARM: &str = "/internal/facade/v1/mcp/prewarm";

/// Erreur d'un appel au délai dépassé (`context.DeadlineExceeded` du Go).
const CALL_DEADLINE: &str = "context deadline exceeded";

/// Issue d'une tentative de connexion.
#[derive(Default)]
struct SlotState {
    sess: Option<Arc<Session>>,
    tools: Vec<Value>,
    err: Option<String>,
}

/// Une entrée du pool, publiée AVANT la connexion : un second appelant attend
/// son issue au lieu de lancer un deuxième processus.
struct Slot {
    cfg: ServerConfig,
    state: tokio::sync::OnceCell<SlotState>,
}

/// Le pool des sessions MCP et le registre nom exposé → (serveur, outil).
pub struct Mcp {
    home: PathBuf,
    slots: Mutex<HashMap<String, Arc<Slot>>>,
    registry: Mutex<HashMap<String, (String, String)>>,
    cfg_lock: tokio::sync::Mutex<()>,
}

fn lock<T>(m: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    m.lock().unwrap_or_else(|e| e.into_inner())
}

impl Mcp {
    pub fn new(home: PathBuf) -> Self {
        Mcp {
            home,
            slots: Mutex::new(HashMap::new()),
            registry: Mutex::new(HashMap::new()),
            cfg_lock: tokio::sync::Mutex::new(()),
        }
    }

    /// `LoadMCPConfig`.
    pub async fn load(store: &Store) -> Servers {
        config::decode_servers(store.get(BK_STATE, ST_MCP).await.ok().flatten().as_deref())
    }

    async fn save(store: &Store, servers: &Servers) -> Result<(), String> {
        store
            .put(BK_STATE, ST_MCP, gojson::marshal(servers).into_bytes())
            .await
    }

    /// Une mutation de la configuration, sérialisée, puis l'invalidation du
    /// serveur touché.
    async fn mutate(
        &self,
        store: &Store,
        name: &str,
        f: impl FnOnce(&mut Servers) -> Result<(), String>,
    ) -> Result<(), String> {
        {
            let _g = self.cfg_lock.lock().await;
            let mut servers = Self::load(store).await;
            f(&mut servers)?;
            Self::save(store, &servers).await?;
        }
        self.invalidate(name).await;
        Ok(())
    }

    /// `mcpInvalidate` : la session du serveur fermée et oubliée, ses outils
    /// retirés du registre.
    pub async fn invalidate(&self, name: &str) {
        let slot = lock(&self.slots).remove(name);
        lock(&self.registry).retain(|_, (server, _)| server != name);
        // Une session encore en cours de connexion se refermera d'elle-même en
        // constatant qu'elle n'est plus dans le pool.
        if let Some(s) = slot
            && let Some(st) = s.state.get()
            && let Some(sess) = &st.sess
        {
            sess.close().await;
        }
    }

    /// `mcpCloseAll`.
    pub async fn close_all(&self) {
        let slots: Vec<_> = lock(&self.slots).drain().map(|(_, s)| s).collect();
        lock(&self.registry).clear();
        for s in slots {
            if let Some(st) = s.state.get()
                && let Some(sess) = &st.sess
            {
                sess.close().await;
            }
        }
    }

    /// `ensure` : la session du serveur, connectée ou en erreur.
    async fn ensure(&self, name: &str, cfg: &ServerConfig, version: &str) -> Arc<Slot> {
        let slot = lock(&self.slots)
            .entry(name.to_string())
            .or_insert_with(|| {
                Arc::new(Slot {
                    cfg: cfg.clone(),
                    state: tokio::sync::OnceCell::new(),
                })
            })
            .clone();
        slot.state
            .get_or_init(|| async {
                let cfg = &slot.cfg;
                let mut st = SlotState::default();
                let work = async {
                    let sess = Session::connect(cfg, version).await?;
                    let tools = sess.list_tools().await;
                    Ok::<_, String>((sess, tools))
                };
                match tokio::time::timeout(config::timeout_for(cfg), work).await {
                    Err(_) => st.err = Some(config::timeout_error(cfg)),
                    Ok(Err(e)) => st.err = Some(e),
                    Ok(Ok((sess, tools))) => {
                        match tools {
                            Ok(t) => st.tools = t,
                            Err(e) => st.err = Some(e),
                        }
                        st.sess = Some(Arc::new(sess));
                    }
                }
                // Invalidée pendant la connexion : jamais republiée, refermée.
                let current = lock(&self.slots)
                    .get(name)
                    .is_some_and(|s| Arc::ptr_eq(s, &slot));
                if current {
                    let mut reg = lock(&self.registry);
                    reg.retain(|_, (server, _)| server != name);
                    for t in &st.tools {
                        let tn = t.get("name").and_then(Value::as_str).unwrap_or("");
                        reg.insert(
                            config::exposed_name(name, tn),
                            (name.to_string(), tn.to_string()),
                        );
                    }
                } else if let Some(sess) = st.sess.take() {
                    sess.close().await;
                }
                st
            })
            .await;
        slot
    }

    /// `mcpEnsureAll` : les serveurs activés connectés en parallèle.
    async fn ensure_all(&self, servers: &Servers, version: &str) {
        join_all(
            servers
                .iter()
                .filter(|(_, c)| c.enabled && !c.transport().is_empty())
                .map(|(n, c)| self.ensure(n, c, version)),
        )
        .await;
    }

    /// `mcpTools` : les outils des serveurs activés, prêts à être annoncés au
    /// modèle (forme `Tool` du Go).
    pub async fn tools(&self, store: &Store, version: &str) -> Vec<Value> {
        let servers = Self::load(store).await;
        if servers.is_empty() {
            return Vec::new();
        }
        self.ensure_all(&servers, version).await;
        let mut out = Vec::new();
        for (name, cfg) in &servers {
            if !cfg.enabled || cfg.transport().is_empty() {
                continue;
            }
            let slot = self.ensure(name, cfg, version).await;
            let Some(st) = slot.state.get() else { continue };
            if st.sess.is_none() {
                continue;
            }
            for t in &st.tools {
                let tn = t.get("name").and_then(Value::as_str).unwrap_or("");
                if cfg.tool_disabled(tn) {
                    continue;
                }
                let mut desc = t.get("description").and_then(Value::as_str).unwrap_or("");
                if desc.is_empty() {
                    desc = t.get("title").and_then(Value::as_str).unwrap_or("");
                }
                out.push(json!({
                    "type": "function",
                    "function": {
                        "name": config::exposed_name(name, tn),
                        "description": config::tag_description(name, desc),
                        "parameters": config::normalize_schema(t.get("inputSchema")),
                    },
                }));
            }
        }
        out
    }

    /// `mcpCall` : un outil par son nom exposé ; une reconnexion si la
    /// session est morte.
    pub async fn call(
        &self,
        store: &Store,
        name: &str,
        args: &Map<String, Value>,
        version: &str,
    ) -> String {
        let Some((server, tool)) = lock(&self.registry).get(name).cloned() else {
            return format!("[erreur] outil MCP inconnu ou serveur non connecté: {name}");
        };
        let cap = config::output_cap(config::ctx_window(&store.config().await));
        let servers = Self::load(store).await;
        let timeout =
            config::call_timeout_for(&servers.get(&server).cloned().unwrap_or_default(), args);
        let mut err = match self.call_once(&server, &tool, args, cap, timeout).await {
            Ok(r) => return r,
            Err(e) => e,
        };
        if err == CALL_DEADLINE {
            return config::call_deadline_error(timeout);
        }
        if let Some(cfg) = servers.get(&server).filter(|c| c.enabled) {
            self.invalidate(&server).await;
            self.ensure(&server, cfg, version).await;
            match self.call_once(&server, &tool, args, cap, timeout).await {
                Ok(r) => return r,
                Err(e) => err = e,
            }
        }
        format!("[erreur MCP] {err}")
    }

    async fn call_once(
        &self,
        server: &str,
        tool: &str,
        args: &Map<String, Value>,
        cap: usize,
        timeout: std::time::Duration,
    ) -> Result<String, String> {
        let slot = lock(&self.slots).get(server).cloned();
        let sess = slot
            .as_ref()
            .and_then(|s| s.state.get())
            .and_then(|st| st.sess.clone())
            .filter(|s| !s.is_closed())
            .ok_or_else(|| format!("serveur {server} non connecté"))?;
        let res = tokio::time::timeout(timeout, sess.call_tool(tool, args))
            .await
            .map_err(|_| CALL_DEADLINE.to_string())??;
        let out = config::cap_output(config::flatten_content(&res), cap);
        if res.get("isError").and_then(Value::as_bool) == Some(true) {
            return Ok(format!("[l'outil a renvoyé une erreur]\n{out}"));
        }
        if out.trim().is_empty() {
            return Ok("[ok] (aucune sortie)".into());
        }
        Ok(out)
    }

    /// `MCPStatus` : l'état de chaque serveur, connexion tentée pour les
    /// activés.
    pub async fn status(&self, store: &Store, version: &str) -> Vec<Value> {
        let servers = Self::load(store).await;
        self.ensure_all(&servers, version).await;
        let mut out = Vec::new();
        for (name, cfg) in &servers {
            let mut st = json!({
                "name": name,
                "transport": cfg.transport(),
                "enabled": cfg.enabled,
                "connected": false,
                "tools": [],
                "disabled": cfg.disabled_tools,
            });
            for (k, v) in [
                ("command", json!(cfg.command)),
                ("args", json!(cfg.args)),
                ("env", json!(cfg.env)),
                ("url", json!(cfg.url)),
                ("headers", json!(cfg.headers)),
            ] {
                let empty = match &v {
                    Value::String(s) => s.is_empty(),
                    Value::Array(a) => a.is_empty(),
                    Value::Object(o) => o.is_empty(),
                    _ => false,
                };
                if !empty {
                    st[k] = v;
                }
            }
            if cfg.enabled && !cfg.transport().is_empty() {
                let slot = self.ensure(name, cfg, version).await;
                if let Some(s) = slot.state.get() {
                    if s.sess.is_some() {
                        st["connected"] = json!(true);
                        st["tools"] = json!(
                            s.tools
                                .iter()
                                .map(|t| t.get("name").and_then(Value::as_str).unwrap_or(""))
                                .collect::<Vec<_>>()
                        );
                    }
                    if let Some(e) = &s.err {
                        st["error"] = json!(e);
                    }
                }
            }
            out.push(st);
        }
        out
    }

    /// `MCPPrewarm` : les sessions ouvertes en tâche de fond, en mode agent
    /// seulement (sans lui, aucun outil MCP n'est annoncé).
    pub fn prewarm(self: &Arc<Self>, store: Store, version: String) {
        let me = self.clone();
        tokio::spawn(async move {
            if store.get_str(BK_STATE, "agent").await != "1" {
                return;
            }
            let servers = Self::load(&store).await;
            if !servers.is_empty() {
                me.ensure_all(&servers, &version).await;
            }
        });
    }

    /// `MCPCatalog` : le catalogue embarqué, chemins résolus, entrées déjà
    /// configurées marquées.
    pub async fn catalog(&self, store: &Store) -> Result<Vec<Value>, String> {
        let entries = catalog_entries()?;
        let configured = Self::load(store).await;
        let ws = workspace_dir(&self.home);
        let expand = |s: &str| s.replace("{workspace}", &ws);
        let mut out = Vec::new();
        for e in entries {
            let mut e = e.clone();
            let obj = e.as_object_mut().ok_or("entrée du catalogue illisible")?;
            if let Some(Value::Array(a)) = obj.get_mut("args") {
                for x in a.iter_mut() {
                    if let Value::String(s) = x {
                        *s = expand(s);
                    }
                }
            }
            if let Some(Value::Object(m)) = obj.get_mut("env") {
                for x in m.values_mut() {
                    if let Value::String(s) = x {
                        *s = expand(s);
                    }
                }
            }
            for k in ["homepage", "runtime"] {
                if obj.get(k).and_then(Value::as_str) == Some("") {
                    obj.remove(k);
                }
            }
            for k in ["args", "secrets"] {
                if obj
                    .get(k)
                    .and_then(Value::as_array)
                    .is_some_and(|a| a.is_empty())
                {
                    obj.remove(k);
                }
            }
            if obj
                .get("env")
                .and_then(Value::as_object)
                .is_some_and(|m| m.is_empty())
            {
                obj.remove("env");
            }
            let id = obj.get("id").and_then(Value::as_str).unwrap_or("");
            let runtime = obj.get("runtime").and_then(Value::as_str).unwrap_or("");
            let (installed, runtime_ok) = (configured.contains_key(id), runtime_available(runtime));
            obj.insert("installed".into(), json!(installed));
            obj.insert("runtimeOk".into(), json!(runtime_ok));
            out.push(e);
        }
        Ok(out)
    }
}

/// `workspaceDir` : le dossier de travail de l'agent.
fn workspace_dir(home: &Path) -> String {
    crate::paths::go_join(&[&home.to_string_lossy(), "workspace"])
}

/// `mcpRuntimeAvailable`.
fn runtime_available(runtime: &str) -> bool {
    match runtime {
        "node" => crate::paths::look_path("npx").is_some(),
        "python" => crate::paths::look_path("uvx").is_some(),
        _ => true,
    }
}

/// Le catalogue embarqué (celui du Go, `mcp_catalog.json`).
static CATALOG: LazyLock<Result<Vec<Value>, String>> = LazyLock::new(|| {
    let raw = include_str!("../../../../internal/loki/mcp_catalog.json");
    let v: Value =
        serde_json::from_str(raw).map_err(|e| format!("catalogue MCP illisible : {e}"))?;
    Ok(v.get("servers")
        .and_then(Value::as_array)
        .cloned()
        .unwrap_or_default()
        .into_iter()
        .map(|mut e| {
            // Les champs du type Go, dans son ordre (le reste est ignoré).
            let mut o = Map::new();
            for (k, def) in [
                ("id", json!("")),
                ("name", json!("")),
                ("category", json!("")),
                ("description", json!("")),
                ("homepage", json!("")),
                ("runtime", json!("")),
                ("command", json!("")),
                ("args", json!([])),
                ("env", json!({})),
                ("secrets", json!([])),
            ] {
                o.insert(k.into(), e.get_mut(k).map(Value::take).unwrap_or(def));
            }
            Value::Object(o)
        })
        .collect())
});

fn catalog_entries() -> Result<&'static Vec<Value>, String> {
    CATALOG.as_ref().map_err(Clone::clone)
}

// ---------------------------------------------------------------------------
// Routes
// ---------------------------------------------------------------------------

/// Les routes servies par ce module.
pub fn handles(path: &str) -> bool {
    matches!(
        path,
        "/api/mcp"
            | "/api/mcp/catalog"
            | "/api/mcp/save"
            | "/api/mcp/delete"
            | "/api/mcp/toggle"
            | "/api/mcp/tool"
            | "/api/mcp/test"
    )
}

fn fail(code: StatusCode, e: impl Into<String>) -> Response<Body> {
    send_json(code, &json!({ "ok": false, "error": e.into() }))
}

const SAVE_REQ_TYPE: &str = "mcpSaveReq";
const NAME_REQ_TYPE: &str = r#"struct { Name string "json:\"name\"" }"#;
const TOGGLE_REQ_TYPE: &str = r#"struct { Name string "json:\"name\""; On bool "json:\"on\"" }"#;
const TOOL_REQ_TYPE: &str =
    r#"struct { Name string "json:\"name\""; Tool string "json:\"tool\""; On bool "json:\"on\"" }"#;

async fn read_body(body: Incoming) -> Vec<u8> {
    Limited::new(body, 1 << 20)
        .collect()
        .await
        .map(|b| b.to_bytes().to_vec())
        .unwrap_or_default()
}

/// La version du Go (annoncée aux serveurs dans `clientInfo`).
async fn go_version(ctx: &Ctx) -> String {
    ctx.internal
        .auth()
        .await
        .map(|i| i.version)
        .unwrap_or_default()
}

/// `from_go` : remise par le Go (tunnel), déjà authentifiée par lui.
pub async fn serve(
    ctx: &Ctx,
    mcp: &Arc<Mcp>,
    req: Request<Incoming>,
    from_go: bool,
) -> Response<Body> {
    let (parts, body) = req.into_parts();
    let version = if from_go {
        go_version(ctx).await
    } else {
        match ctx.guard(&parts.headers).await {
            Ok(v) => v,
            Err(refus) => return *refus,
        }
    };
    let store = &ctx.store;
    let servers_ok =
        |list: Vec<Value>| send_json(StatusCode::OK, &json!({ "ok": true, "servers": list }));
    match parts.uri.path() {
        "/api/mcp" => servers_ok(mcp.status(store, &version).await),
        "/api/mcp/catalog" => match mcp.catalog(store).await {
            Ok(c) => send_json(StatusCode::OK, &json!({ "ok": true, "catalog": c })),
            Err(e) => fail(StatusCode::INTERNAL_SERVER_ERROR, e),
        },
        "/api/mcp/save" => {
            let raw = read_body(body).await;
            let d = gojson::decode_struct(
                &raw,
                SAVE_REQ_TYPE,
                &[
                    ("name", Kind::Str),
                    ("command", Kind::Str),
                    ("args", Kind::StrList),
                    ("env", Kind::StrMap),
                    ("url", Kind::Str),
                    ("headers", Kind::StrMap),
                    ("enabled", Kind::OptBool),
                ],
            );
            if let Some(e) = d.err.clone() {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            let cfg = ServerConfig {
                command: d.str("command").trim().to_string(),
                args: d.str_list("args").unwrap_or_default(),
                env: d.str_map("env").unwrap_or_default().into_iter().collect(),
                url: d.str("url").trim().to_string(),
                headers: d
                    .str_map("headers")
                    .unwrap_or_default()
                    .into_iter()
                    .collect(),
                enabled: d.opt_bool("enabled").unwrap_or(true),
                disabled_tools: Vec::new(),
                call_timeout_sec: 0,
            };
            let name = d.str("name");
            let res = mcp
                .mutate(store, name.trim(), |s| config::set_server(s, &name, cfg))
                .await;
            if let Err(e) = res {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            servers_ok(mcp.status(store, &version).await)
        }
        "/api/mcp/delete" => {
            let raw = read_body(body).await;
            let d = gojson::decode_struct(&raw, NAME_REQ_TYPE, &[("name", Kind::Str)]);
            if let Some(e) = d.err.clone() {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            let name = d.str("name");
            if let Err(e) = mcp
                .mutate(store, &name, |s| config::delete_server(s, &name))
                .await
            {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            send_json(StatusCode::OK, &json!({ "ok": true }))
        }
        "/api/mcp/toggle" => {
            let raw = read_body(body).await;
            let d = gojson::decode_struct(
                &raw,
                TOGGLE_REQ_TYPE,
                &[("name", Kind::Str), ("on", Kind::Bool)],
            );
            if let Some(e) = d.err.clone() {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            let (name, on) = (d.str("name"), d.bool("on"));
            if let Err(e) = mcp
                .mutate(store, &name, |s| config::set_enabled(s, &name, on))
                .await
            {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            servers_ok(mcp.status(store, &version).await)
        }
        "/api/mcp/tool" => {
            let raw = read_body(body).await;
            let d = gojson::decode_struct(
                &raw,
                TOOL_REQ_TYPE,
                &[("name", Kind::Str), ("tool", Kind::Str), ("on", Kind::Bool)],
            );
            if let Some(e) = d.err.clone() {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            let (name, tool, on) = (d.str("name"), d.str("tool"), d.bool("on"));
            if let Err(e) = mcp
                .mutate(store, &name, |s| {
                    config::set_tool_enabled(s, &name, &tool, on)
                })
                .await
            {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            servers_ok(mcp.status(store, &version).await)
        }
        "/api/mcp/test" => {
            let raw = read_body(body).await;
            let d = gojson::decode_struct(&raw, NAME_REQ_TYPE, &[("name", Kind::Str)]);
            if let Some(e) = d.err.clone() {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            mcp.invalidate(&d.str("name")).await;
            servers_ok(mcp.status(store, &version).await)
        }
        _ => fail(StatusCode::NOT_FOUND, "route inconnue"),
    }
}

/// L'API interne de la façade pour le Go (jeton interne vérifié par
/// l'appelant) : outils annoncés, appels, pré-chauffage.
pub async fn serve_internal(ctx: &Ctx, mcp: &Arc<Mcp>, req: Request<Incoming>) -> Response<Body> {
    let path = req.uri().path().to_string();
    let raw = read_body(req.into_body()).await;
    let version = go_version(ctx).await;
    match path.as_str() {
        INTERNAL_TOOLS => send_json(
            StatusCode::OK,
            &json!({ "tools": mcp.tools(&ctx.store, &version).await }),
        ),
        INTERNAL_CALL => {
            let v: Value = serde_json::from_slice(&raw).unwrap_or_default();
            let name = v.get("name").and_then(Value::as_str).unwrap_or("");
            let args = v
                .get("args")
                .and_then(Value::as_object)
                .cloned()
                .unwrap_or_default();
            let out = mcp.call(&ctx.store, name, &args, &version).await;
            send_json(StatusCode::OK, &json!({ "result": out }))
        }
        INTERNAL_PREWARM => {
            mcp.prewarm(ctx.store.clone(), version);
            send_json(StatusCode::OK, &json!({ "ok": true }))
        }
        _ => fail(StatusCode::NOT_FOUND, "route inconnue"),
    }
}

/// Les chemins de l'API interne de ce module.
pub fn is_internal(path: &str) -> bool {
    matches!(path, INTERNAL_TOOLS | INTERNAL_CALL | INTERNAL_PREWARM)
}
