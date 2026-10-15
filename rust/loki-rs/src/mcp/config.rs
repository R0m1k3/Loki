//! Configuration des serveurs MCP (`mcp_config.go`) et fonctions pures du
//! client (`mcp_client.go`) : nommage des outils, schémas, libellés, ligne du
//! préambule, délais.

use std::collections::BTreeMap;
use std::time::Duration;

use serde::{Deserialize, Serialize};
use serde_json::{Map, Value, json};

/// `MCPServerConfig` : un seul transport renseigné, `command` (stdio) ou
/// `url` (Streamable HTTP).
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct ServerConfig {
    #[serde(skip_serializing_if = "String::is_empty")]
    pub command: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub args: Vec<String>,
    #[serde(skip_serializing_if = "BTreeMap::is_empty")]
    pub env: BTreeMap<String, String>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub url: String,
    #[serde(skip_serializing_if = "BTreeMap::is_empty")]
    pub headers: BTreeMap<String, String>,
    pub enabled: bool,
    #[serde(rename = "disabledTools", skip_serializing_if = "Vec::is_empty")]
    pub disabled_tools: Vec<String>,
}

impl Default for ServerConfig {
    fn default() -> Self {
        ServerConfig {
            command: String::new(),
            args: Vec::new(),
            env: BTreeMap::new(),
            url: String::new(),
            headers: BTreeMap::new(),
            enabled: true,
            disabled_tools: Vec::new(),
        }
    }
}

/// Lecture tolérante : `enabled` vaut vrai quand la clé est absente (fichiers
/// à la Claude Desktop), les champs mal typés restent vides.
impl<'de> Deserialize<'de> for ServerConfig {
    fn deserialize<D: serde::Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        let v = Map::<String, Value>::deserialize(d)?;
        let s = |k: &str| v.get(k).and_then(Value::as_str).unwrap_or("").to_string();
        let list = |k: &str| -> Vec<String> {
            v.get(k)
                .and_then(Value::as_array)
                .map(|a| {
                    a.iter()
                        .filter_map(|x| x.as_str().map(str::to_string))
                        .collect()
                })
                .unwrap_or_default()
        };
        let map = |k: &str| -> BTreeMap<String, String> {
            v.get(k)
                .and_then(Value::as_object)
                .map(|o| {
                    o.iter()
                        .filter_map(|(k, x)| x.as_str().map(|s| (k.clone(), s.to_string())))
                        .collect()
                })
                .unwrap_or_default()
        };
        Ok(ServerConfig {
            command: s("command"),
            args: list("args"),
            env: map("env"),
            url: s("url"),
            headers: map("headers"),
            enabled: match v.get("enabled") {
                None => true,
                Some(x) => x.as_bool().unwrap_or(false),
            },
            disabled_tools: list("disabledTools"),
        })
    }
}

impl ServerConfig {
    /// `Transport` : « stdio », « http » ou « » (mal configuré).
    pub fn transport(&self) -> &'static str {
        if !self.command.trim().is_empty() {
            "stdio"
        } else if !self.url.trim().is_empty() {
            "http"
        } else {
            ""
        }
    }

    /// `Validate` : exactement un transport.
    pub fn validate(&self) -> Result<(), String> {
        let (cmd, url) = (!self.command.trim().is_empty(), !self.url.trim().is_empty());
        match (cmd, url) {
            (true, true) => Err(
                "un serveur MCP ne peut avoir à la fois 'command' (stdio) et 'url' (http)".into(),
            ),
            (false, false) => {
                Err("un serveur MCP doit avoir soit 'command' (stdio) soit 'url' (http)".into())
            }
            _ => Ok(()),
        }
    }

    /// `ToolDisabled`.
    pub fn tool_disabled(&self, tool: &str) -> bool {
        self.disabled_tools.iter().any(|t| t == tool)
    }
}

/// Les serveurs déclarés (`state/mcp`), triés par nom.
pub type Servers = BTreeMap<String, ServerConfig>;

/// `loadMCPConfigLocked` : illisible ou absent → aucun serveur.
pub fn decode_servers(raw: Option<&[u8]>) -> Servers {
    raw.and_then(|b| serde_json::from_slice::<Option<Servers>>(b).ok())
        .flatten()
        .unwrap_or_default()
}

/// `SetMCPServer` (sans l'invalidation du pool) : nom contrôlé, transport
/// validé.
pub fn set_server(servers: &mut Servers, name: &str, cfg: ServerConfig) -> Result<(), String> {
    let name = name.trim();
    if name.is_empty() {
        return Err("nom de serveur vide".into());
    }
    if name.contains("__") {
        return Err("le nom ne peut pas contenir '__' (réservé au namespacing des outils)".into());
    }
    cfg.validate()?;
    servers.insert(name.to_string(), cfg);
    Ok(())
}

fn unknown(name: &str) -> String {
    format!("serveur MCP inconnu: {name}")
}

/// `DeleteMCPServer`.
pub fn delete_server(servers: &mut Servers, name: &str) -> Result<(), String> {
    servers
        .remove(name)
        .map(|_| ())
        .ok_or_else(|| unknown(name))
}

/// `SetMCPServerEnabled`.
pub fn set_enabled(servers: &mut Servers, name: &str, on: bool) -> Result<(), String> {
    let cfg = servers.get_mut(name).ok_or_else(|| unknown(name))?;
    cfg.enabled = on;
    Ok(())
}

/// `SetMCPToolEnabled` : la liste reconstruite sans l'outil, puis l'outil
/// ajouté en fin s'il est masqué.
pub fn set_tool_enabled(
    servers: &mut Servers,
    server: &str,
    tool: &str,
    on: bool,
) -> Result<(), String> {
    let cfg = servers.get_mut(server).ok_or_else(|| unknown(server))?;
    cfg.disabled_tools.retain(|t| t != tool);
    if !on {
        cfg.disabled_tools.push(tool.to_string());
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// Fonctions pures du client
// ---------------------------------------------------------------------------

/// `mcpSanitize` : `[^a-zA-Z0-9_-]+` → `_`, puis `_` retirés aux bords.
pub fn sanitize(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    let mut in_run = false;
    for c in s.chars() {
        if c.is_ascii_alphanumeric() || c == '_' || c == '-' {
            out.push(c);
            in_run = false;
        } else if !in_run {
            out.push('_');
            in_run = true;
        }
    }
    out.trim_matches('_').to_string()
}

/// `mcpExposedName` : `mcp__<serveur>__<outil>`.
pub fn exposed_name(server: &str, tool: &str) -> String {
    format!("mcp__{}__{}", sanitize(server), sanitize(tool))
}

/// `isMCPTool`.
pub fn is_mcp_tool(name: &str) -> bool {
    name.starts_with("mcp__")
}

/// `mcpNormalizeSchema` : un objet tel quel, sinon un objet sans propriété.
pub fn normalize_schema(schema: Option<&Value>) -> Value {
    match schema {
        Some(Value::Object(m)) => Value::Object(m.clone()),
        _ => json!({ "type": "object", "properties": {} }),
    }
}

/// `mcpArgLabel` : une clé parlante, sinon le JSON des arguments (120 runes).
pub fn arg_label(args: &Map<String, Value>) -> String {
    if args.is_empty() {
        return String::new();
    }
    for k in ["path", "file", "query", "command", "url", "name"] {
        if let Some(Value::String(v)) = args.get(k)
            && !v.is_empty()
        {
            return v.clone();
        }
    }
    let s = crate::gojson::marshal(args);
    if s.chars().count() > 120 {
        return s.chars().take(120).collect::<String>() + "…";
    }
    s
}

/// `mcpTagDescription`.
pub fn tag_description(server: &str, desc: &str) -> String {
    format!("[MCP: {server}] {desc}")
}

/// `mcpToolServer` : le serveur d'origine d'un outil annoncé, d'après le
/// préfixe de sa description (la première coupure dont la forme assainie
/// figure bien dans le nom de l'outil).
pub fn tool_server(name: &str, description: &str) -> Option<String> {
    if !name.starts_with("mcp__") {
        return None;
    }
    let rest = description.strip_prefix("[MCP: ")?;
    let mut i = 0;
    loop {
        let j = rest[i..].find("] ")?;
        let server = &rest[..i + j];
        if name.starts_with(&format!("mcp__{}__", sanitize(server))) {
            return Some(server.to_string());
        }
        i += j + 1;
    }
}

/// `mcpPromptLine` : les serveurs des outils annoncés ce tour-ci, et leur
/// nombre d'outils. `tools` : (nom, description).
pub fn prompt_line(tools: &[(String, String)]) -> String {
    let mut counts: BTreeMap<String, usize> = BTreeMap::new();
    for (n, d) in tools {
        if let Some(s) = tool_server(n, d) {
            *counts.entry(s).or_default() += 1;
        }
    }
    if counts.is_empty() {
        return String::new();
    }
    let mut names: Vec<String> = counts.iter().map(|(s, n)| format!("{s} ({n})")).collect();
    names.sort();
    format!(
        "\n\nMCP servers connected (their tools are prefixed mcp__<server>__<tool>): {}.",
        names.join(", ")
    )
}

/// `mcpConnectTimeout`.
pub const CONNECT_TIMEOUT: Duration = Duration::from_secs(20);
/// `mcpConnectTimeoutCold` : npx et uvx téléchargent le paquet au premier
/// lancement.
pub const CONNECT_TIMEOUT_COLD: Duration = Duration::from_secs(180);
/// `mcpCallTimeout`.
pub const CALL_TIMEOUT: Duration = Duration::from_secs(120);

/// Nom du lanceur (`filepath.Base`), en minuscules, sans `.exe` puis sans
/// `.cmd`.
fn launcher(cfg: &ServerConfig) -> String {
    let t = cfg.command.trim_end_matches('/');
    let base = if t.is_empty() {
        if cfg.command.is_empty() { "." } else { "/" }
    } else {
        t.rsplit('/').next().unwrap_or(t)
    };
    let base = base.to_lowercase();
    let b = base.strip_suffix(".exe").unwrap_or(&base);
    b.strip_suffix(".cmd").unwrap_or(b).to_string()
}

/// `mcpTimeoutFor`.
pub fn timeout_for(cfg: &ServerConfig) -> Duration {
    match launcher(cfg).as_str() {
        "npx" | "uvx" => CONNECT_TIMEOUT_COLD,
        _ => CONNECT_TIMEOUT,
    }
}

/// `mcpFriendlyErr` pour un délai dépassé.
pub fn timeout_error(cfg: &ServerConfig) -> String {
    let l = launcher(cfg);
    if l == "npx" || l == "uvx" {
        return format!(
            "délai dépassé — {l} télécharge le paquet du serveur au premier lancement, ce qui peut être long ; clique « tester » pour réessayer (le paquet reste en cache), et vérifie que le conteneur atteint le registre (npm/PyPI)"
        );
    }
    "délai dépassé — le serveur n'a pas répondu au handshake en 20s".into()
}

/// `mcpOutputCap` : environ un caractère par jeton de contexte, jamais sous
/// 12 000.
pub fn output_cap(ctx: i64) -> usize {
    12000.max(ctx.max(0) as usize)
}

/// `ctxWindow` : `CTX` de la configuration, 32 768 à défaut.
pub fn ctx_window(cfg: &std::collections::HashMap<String, String>) -> i64 {
    cfg.get("CTX")
        .and_then(|v| v.parse::<i64>().ok())
        .filter(|n| *n > 0)
        .unwrap_or(32768)
}

/// Le texte d'un résultat d'outil, tronqué au plafond (en runes).
pub fn cap_output(out: String, lim: usize) -> String {
    let n = out.chars().count();
    if n <= lim {
        return out;
    }
    let kept: String = out.chars().take(lim).collect();
    format!("{kept}\n…[tronqué : {n} caractères au total, {lim} gardés]")
}

/// `flattenMCPContent` : blocs texte concaténés, images et sons résumés, les
/// autres rendus en JSON ; le contenu structuré en JSON indenté s'il n'y a pas
/// de texte.
pub fn flatten_content(result: &Value) -> String {
    let mut parts: Vec<String> = Vec::new();
    for c in result
        .get("content")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
    {
        let kind = c.get("type").and_then(Value::as_str).unwrap_or("");
        let mime = c.get("mimeType").and_then(Value::as_str).unwrap_or("");
        match kind {
            "text" => parts.push(c.get("text").and_then(Value::as_str).unwrap_or("").into()),
            "image" => parts.push(format!("[image {mime}]")),
            "audio" => parts.push(format!("[audio {mime}]")),
            _ => parts.push(wire_content_json(c)),
        }
    }
    let text = parts.join("\n").trim().to_string();
    if text.is_empty()
        && let Some(sc) = result.get("structuredContent").filter(|v| !v.is_null())
    {
        return String::from_utf8(crate::chatx::marshal_indent(sc)).unwrap_or_default();
    }
    text
}

/// `json.Marshal` d'un contenu « autre » par le SDK Go (`wireContent`) :
/// champs dans l'ordre de sa struct, vides omis.
fn wire_content_json(c: &Value) -> String {
    let order = [
        "type",
        "text",
        "mimeType",
        "data",
        "resource",
        "uri",
        "name",
        "title",
        "description",
        "size",
        "_meta",
        "annotations",
        "icons",
        "id",
        "input",
        "toolUseId",
        "content",
        "structuredContent",
        "isError",
    ];
    let mut out = String::from("{");
    let mut first = true;
    for k in order {
        let Some(v) = c.get(k) else { continue };
        let empty = match v {
            Value::Null => true,
            Value::String(s) => s.is_empty(),
            Value::Bool(b) => !b,
            Value::Array(a) => a.is_empty(),
            Value::Object(o) => o.is_empty() && k != "resource",
            Value::Number(_) => false,
        };
        if empty && k != "type" {
            continue;
        }
        if !first {
            out.push(',');
        }
        first = false;
        out.push_str(&crate::gojson::marshal(k));
        out.push(':');
        if k == "resource" {
            out.push_str(&resource_contents_json(v));
        } else {
            out.push_str(&crate::gojson::marshal(v));
        }
    }
    out.push('}');
    out
}

/// `ResourceContents` du SDK Go : `uri` toujours, puis `mimeType`, `text`,
/// `blob`, `_meta` s'ils sont renseignés.
fn resource_contents_json(v: &Value) -> String {
    let mut out = String::from("{");
    let mut first = true;
    for (k, always) in [
        ("uri", true),
        ("mimeType", false),
        ("text", false),
        ("blob", false),
        ("_meta", false),
    ] {
        let x = v.get(k).cloned().unwrap_or(Value::Null);
        let empty = match &x {
            Value::Null => true,
            Value::String(s) => s.is_empty(),
            Value::Object(o) => o.is_empty(),
            _ => false,
        };
        if empty && !always {
            continue;
        }
        if !first {
            out.push(',');
        }
        first = false;
        out.push_str(&crate::gojson::marshal(k));
        out.push(':');
        let x = if x.is_null() && always { json!("") } else { x };
        out.push_str(&crate::gojson::marshal(&x));
    }
    out.push('}');
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn noms() {
        assert_eq!(sanitize("mon serveur!"), "mon_serveur");
        assert_eq!(exposed_name("a b", "c.d"), "mcp__a_b__c_d");
    }

    #[test]
    fn lanceurs() {
        let c = |cmd: &str| ServerConfig {
            command: cmd.into(),
            ..Default::default()
        };
        assert_eq!(timeout_for(&c("/usr/bin/npx")), CONNECT_TIMEOUT_COLD);
        assert_eq!(timeout_for(&c("UVX.EXE")), CONNECT_TIMEOUT_COLD);
        assert_eq!(timeout_for(&c("npx.cmd")), CONNECT_TIMEOUT_COLD);
        assert_eq!(timeout_for(&c("node")), CONNECT_TIMEOUT);
    }
}
