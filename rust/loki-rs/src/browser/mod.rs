//! Navigateur piloté (lot 44 de la migration) — port de `computer_cdp.go`,
//! `computer_use.go`, `browser_grid.go` et `cu_autoshot.go`.
//!
//! Un Chromium/Chrome local piloté par le protocole DevTools (CDP) : la
//! façade tient la session (une seule, lancée à la demande et gardée entre
//! les appels), exécute les outils `browser_*` de ses tours et garde
//! l'anneau des aperçus montrés à l'utilisateur. Le Go, quand elle tient la
//! conversation, lui passe ses propres appels (sous-agents, tâches IA,
//! ligne de commande) : un seul navigateur.

pub mod grid;
pub mod js;

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicI64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use base64::Engine as _;
use futures_util::{SinkExt as _, StreamExt as _};
use serde_json::{Map, Value, json};
use tokio::sync::{mpsc, oneshot};
use tokio_tungstenite::tungstenite::Message as WsMessage;

/// `cuViewW`, `cuViewH` : le viewport (et donc la capture), fixe.
pub const VIEW_W: i64 = 1280;
pub const VIEW_H: i64 = 800;
/// `cuMaxOutput`.
pub const MAX_OUTPUT: usize = 8000;
/// `cuShotRing` : aperçus gardés en mémoire.
const SHOT_RING: usize = 8;

/// Les outils servis ici.
pub fn is_tool(name: &str) -> bool {
    matches!(
        name,
        "browser_open"
            | "browser_snapshot"
            | "browser_find"
            | "browser_click"
            | "browser_click_xy"
            | "browser_type"
            | "browser_key"
            | "browser_scroll"
            | "browser_screenshot"
    )
}

/// `cuAutoShotTool` : les outils après lesquels la page est capturée pour
/// l'aperçu.
pub fn auto_shot_tool(name: &str) -> bool {
    matches!(
        name,
        "browser_open"
            | "browser_click"
            | "browser_click_xy"
            | "browser_type"
            | "browser_key"
            | "browser_scroll"
            | "browser_find"
    )
}

/// `capCUOutput`.
pub fn cap_output(s: &str) -> String {
    match s.char_indices().nth(MAX_OUTPUT) {
        None => s.to_string(),
        Some((i, _)) => format!("{}\n…[tronqué]", &s[..i]),
    }
}

/// Un nombre comme `json.Marshal` l'écrit (`640`, pas `640.0`).
fn go_num(f: f64) -> Value {
    if f.is_finite() && f.fract() == 0.0 && f.abs() < 1e15 {
        json!(f as i64)
    } else {
        json!(f)
    }
}

// --- Session CDP ---------------------------------------------------------------

type Reply = Result<Value, String>;

/// `cdpSession` : le processus du navigateur, la connexion à la cible
/// « page » et le multiplexage requête/réponse.
pub struct Session {
    child: Mutex<Option<tokio::process::Child>>,
    user_dir: PathBuf,
    out: mpsc::UnboundedSender<String>,
    pending: Arc<Mutex<HashMap<i64, oneshot::Sender<Reply>>>>,
    next: AtomicI64,
    /// Requêtes réseau en vol et dernier événement réseau (`settle`).
    net: Arc<Mutex<(i64, Option<Instant>)>>,
    last_snap: Mutex<String>,
    closed: Arc<AtomicBool>,
    tasks: Mutex<Vec<tokio::task::JoinHandle<()>>>,
}

fn lock<T>(m: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    m.lock().unwrap_or_else(|e| e.into_inner())
}

/// `sweepStaleCUDirs` : les profils temporaires abandonnés (plus d'une heure).
fn sweep_stale(tmp: &Path) {
    let Ok(rd) = std::fs::read_dir(tmp) else {
        return;
    };
    for e in rd.flatten() {
        let name = e.file_name().to_string_lossy().into_owned();
        if !name.starts_with("loki-cu-") || !e.file_type().is_ok_and(|t| t.is_dir()) {
            continue;
        }
        if e.metadata()
            .and_then(|m| m.modified())
            .is_ok_and(|t| t.elapsed().is_ok_and(|d| d > Duration::from_secs(3600)))
        {
            let _ = std::fs::remove_dir_all(e.path());
        }
    }
}

/// `os.MkdirTemp(tmp, "loki-cu-")`.
fn mkdtemp(tmp: &Path) -> Result<PathBuf, String> {
    for _ in 0..16 {
        let mut b = [0u8; 5];
        let _ = getrandom::fill(&mut b);
        let n = u64::from_le_bytes([b[0], b[1], b[2], b[3], b[4], 0, 0, 0]) % 10_000_000_000;
        let p = tmp.join(format!("loki-cu-{n}"));
        match std::fs::create_dir(&p) {
            Ok(()) => return Ok(p),
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => continue,
            Err(e) => {
                return Err(crate::paths::os_err(
                    "mkdirtemp",
                    &tmp.join("loki-cu-*").to_string_lossy(),
                    &e,
                ));
            }
        }
    }
    Err("mkdirtemp: trop d'essais".into())
}

/// Les arguments de lancement du navigateur.
pub fn launch_args(user_dir: &Path, headful: bool) -> Vec<String> {
    let mut a: Vec<String> = Vec::new();
    if !headful {
        a.push("--headless=new".into());
    }
    a.extend([
        "--remote-debugging-port=0".to_string(),
        format!("--user-data-dir={}", user_dir.to_string_lossy()),
        "--no-first-run".into(),
        "--no-default-browser-check".into(),
        "--disable-gpu".into(),
        "--disable-extensions".into(),
        "--disable-background-networking".into(),
        "--disable-features=Translate,BackForwardCache".into(),
        "--no-sandbox".into(),
        "--disable-dev-shm-usage".into(),
        "--window-size=1280,900".into(),
        "about:blank".into(),
    ]);
    a
}

/// `cdpPageWS` : l'URL websocket d'une cible « page ».
async fn page_ws(port: &str) -> Result<String, String> {
    crate::chat::engine::install_crypto();
    let cl = reqwest::Client::builder()
        .timeout(Duration::from_secs(5))
        .build()
        .unwrap_or_default();
    let deadline = Instant::now() + Duration::from_secs(10);
    while Instant::now() < deadline {
        if let Ok(resp) = cl.get(format!("http://127.0.0.1:{port}/json")).send().await {
            let body = resp.bytes().await.unwrap_or_default();
            let targets: Vec<Value> = serde_json::from_slice(&body).unwrap_or_default();
            for t in &targets {
                let ws = t
                    .get("webSocketDebuggerUrl")
                    .and_then(Value::as_str)
                    .unwrap_or("");
                if t.get("type").and_then(Value::as_str) == Some("page") && !ws.is_empty() {
                    return Ok(ws.to_string());
                }
            }
        }
        tokio::time::sleep(Duration::from_millis(150)).await;
    }
    Err("aucune cible 'page' exposée par Chrome".into())
}

impl Session {
    /// `cdpLaunch`.
    async fn launch(bin: &str, tmp: &Path) -> Result<Arc<Session>, String> {
        if bin.is_empty() {
            return Err("navigateur Chromium/Chrome/Edge introuvable — installe-le ou définis LOKI_CHROME=<chemin du binaire>".into());
        }
        sweep_stale(tmp);
        let user_dir = mkdtemp(tmp)?;
        let headful = std::env::var("LOKI_CU_HEADFUL").is_ok_and(|v| !v.is_empty());
        let mut cmd = tokio::process::Command::new(bin);
        cmd.args(launch_args(&user_dir, headful))
            .stdin(std::process::Stdio::null())
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .kill_on_drop(true);
        // Tué avec la façade, même brutalement (`prepareBoundChild`).
        #[cfg(target_os = "linux")]
        // SAFETY: prctl(2) est async-signal-safe, seul appel entre fork et exec.
        unsafe {
            cmd.pre_exec(|| {
                libc::prctl(libc::PR_SET_PDEATHSIG, libc::SIGKILL);
                Ok(())
            });
        }
        let mut child = match cmd.spawn() {
            Ok(c) => c,
            Err(e) => {
                let _ = std::fs::remove_dir_all(&user_dir);
                return Err(crate::paths::os_err("fork/exec", bin, &e));
            }
        };
        // Le port réel, en tête de DevToolsActivePort.
        let port_file = user_dir.join("DevToolsActivePort");
        let deadline = Instant::now() + Duration::from_secs(15);
        let mut port = String::new();
        while Instant::now() < deadline {
            if let Ok(b) = std::fs::read_to_string(&port_file) {
                let line = b.trim().split('\n').next().unwrap_or("").to_string();
                if !line.is_empty() {
                    port = line;
                    break;
                }
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
        let fail = |child: &mut tokio::process::Child, e: String| {
            let _ = child.start_kill();
            let _ = std::fs::remove_dir_all(&user_dir);
            Err(e)
        };
        if port.is_empty() {
            return fail(
                &mut child,
                "Chrome n'a pas exposé son port de debug (DevToolsActivePort)".into(),
            );
        }
        let ws_url = match page_ws(&port).await {
            Ok(u) => u,
            Err(e) => return fail(&mut child, e),
        };
        let cfg = tokio_tungstenite::tungstenite::protocol::WebSocketConfig::default()
            .max_message_size(None)
            .max_frame_size(None);
        let ws = match tokio_tungstenite::connect_async_with_config(&ws_url, Some(cfg), false).await
        {
            Ok((ws, _)) => ws,
            Err(e) => return fail(&mut child, format!("failed to WebSocket dial: {e}")),
        };
        let (mut sink, mut stream) = ws.split();
        let (out, mut out_rx) = mpsc::unbounded_channel::<String>();
        let pending: Arc<Mutex<HashMap<i64, oneshot::Sender<Reply>>>> = Default::default();
        let net: Arc<Mutex<(i64, Option<Instant>)>> = Default::default();
        let closed = Arc::new(AtomicBool::new(false));
        let writer = tokio::spawn(async move {
            while let Some(m) = out_rx.recv().await {
                if sink.send(WsMessage::Text(m.into())).await.is_err() {
                    break;
                }
            }
            let _ = sink.close().await;
        });
        let (p2, n2, c2) = (pending.clone(), net.clone(), closed.clone());
        let reader = tokio::spawn(async move {
            while let Some(Ok(msg)) = stream.next().await {
                let data = match msg {
                    WsMessage::Text(t) => t.as_bytes().to_vec(),
                    WsMessage::Binary(b) => b.to_vec(),
                    WsMessage::Close(_) => break,
                    _ => continue,
                };
                let Ok(m) = serde_json::from_slice::<Value>(&data) else {
                    continue;
                };
                let id = m.get("id").and_then(Value::as_i64).unwrap_or(0);
                if id != 0 {
                    let tx = lock(&p2).remove(&id);
                    if let Some(tx) = tx {
                        let reply = match m.get("error").filter(|e| !e.is_null()) {
                            Some(e) => Err(e
                                .get("message")
                                .and_then(Value::as_str)
                                .unwrap_or("")
                                .to_string()),
                            None => Ok(m.get("result").cloned().unwrap_or(Value::Null)),
                        };
                        let _ = tx.send(reply);
                    }
                    continue;
                }
                match m.get("method").and_then(Value::as_str) {
                    Some("Network.requestWillBeSent") => {
                        let mut n = lock(&n2);
                        n.0 += 1;
                        n.1 = Some(Instant::now());
                    }
                    Some("Network.loadingFinished" | "Network.loadingFailed") => {
                        let mut n = lock(&n2);
                        if n.0 > 0 {
                            n.0 -= 1;
                        }
                        n.1 = Some(Instant::now());
                    }
                    _ => {}
                }
            }
            c2.store(true, Ordering::SeqCst);
            lock(&p2).clear();
        });
        let s = Arc::new(Session {
            child: Mutex::new(Some(child)),
            user_dir,
            out,
            pending,
            next: AtomicI64::new(0),
            net,
            last_snap: Mutex::new(String::new()),
            closed,
            tasks: Mutex::new(vec![writer, reader]),
        });
        for m in [
            "Page.enable",
            "Runtime.enable",
            "DOM.enable",
            "Network.enable",
        ] {
            let _ = s.call(m, None).await;
        }
        let _ = s
            .call(
                "Emulation.setDeviceMetricsOverride",
                Some(json!({ "width": VIEW_W, "height": VIEW_H, "deviceScaleFactor": 1, "mobile": false })),
            )
            .await;
        let _ = s
            .call(
                "Page.addScriptToEvaluateOnNewDocument",
                Some(json!({ "source": js::SAME_TAB_JS })),
            )
            .await;
        Ok(s)
    }

    /// `call` : une commande et sa réponse (30 s au plus).
    pub async fn call(&self, method: &str, params: Option<Value>) -> Result<Value, String> {
        let id = self.next.fetch_add(1, Ordering::SeqCst) + 1;
        let (tx, rx) = oneshot::channel();
        lock(&self.pending).insert(id, tx);
        let mut payload = Map::new();
        payload.insert("id".into(), json!(id));
        payload.insert("method".into(), json!(method));
        if let Some(p) = params {
            payload.insert("params".into(), p);
        }
        if self.closed.load(Ordering::SeqCst)
            || self
                .out
                .send(crate::gojson::marshal(&Value::Object(payload)))
                .is_err()
        {
            lock(&self.pending).remove(&id);
            return Err("connexion CDP fermée".into());
        }
        match tokio::time::timeout(Duration::from_secs(30), rx).await {
            Ok(Ok(Ok(v))) => Ok(v),
            Ok(Ok(Err(msg))) => Err(format!("{method}: {msg}")),
            Ok(Err(_)) => Err("connexion CDP fermée".into()),
            Err(_) => {
                lock(&self.pending).remove(&id);
                Err(format!("{method}: délai dépassé"))
            }
        }
    }

    /// `close`.
    fn close(&self) {
        for t in lock(&self.tasks).drain(..) {
            t.abort();
        }
        if let Some(mut c) = lock(&self.child).take() {
            let _ = c.start_kill();
        }
        let _ = std::fs::remove_dir_all(&self.user_dir);
    }

    /// `eval` : la valeur d'une expression (`None` : pas de valeur).
    pub async fn eval(&self, expr: &str) -> Result<Option<Value>, String> {
        let res = self
            .call(
                "Runtime.evaluate",
                Some(json!({ "expression": expr, "returnByValue": true, "awaitPromise": true })),
            )
            .await?;
        if let Some(d) = res.get("exceptionDetails").filter(|d| d.is_object()) {
            return Err(format!(
                "JS: {}",
                d.get("text").and_then(Value::as_str).unwrap_or("")
            ));
        }
        Ok(res.get("result").and_then(|r| r.get("value")).cloned())
    }

    /// `snapshot` : les éléments interactifs visibles, numérotés.
    pub async fn snapshot(&self) -> Result<String, String> {
        let val = self.eval(&js::snapshot_js()).await?;
        let snap = match val {
            None => return Err("snapshot illisible: unexpected end of JSON input".into()),
            Some(Value::Null) => Value::Object(Map::new()),
            Some(o @ Value::Object(_)) => o,
            Some(other) => {
                return Err(format!(
                    "snapshot illisible: json: cannot unmarshal {} into Go value of type loki.cuSnap",
                    go_kind(&other)
                ));
            }
        };
        let s = |k: &str| {
            snap.get(k)
                .and_then(Value::as_str)
                .unwrap_or("")
                .to_string()
        };
        let n = |k: &str| snap.get(k).and_then(Value::as_i64).unwrap_or(0);
        let refs: Vec<String> = snap
            .get("refs")
            .and_then(Value::as_array)
            .into_iter()
            .flatten()
            .map(|r| r.as_str().unwrap_or("").to_string())
            .collect();
        let mut b = format!("# {}\n{}\n\n", s("title"), s("url"));
        if refs.is_empty() {
            b.push_str("(aucun élément interactif détecté — page vide, en cours de chargement, ou rendue sur canvas ; réessaie browser_snapshot, ou utilise browser_screenshot pour voir)");
        } else {
            b.push_str(&format!(
                "{} élément(s) interactif(s) VISIBLE(s) — clique par numéro avec browser_click(ref) :\n",
                refs.len()
            ));
            b.push_str(&refs.join("\n"));
        }
        let (above, below) = (n("above"), n("below"));
        if below > 20 || above > 20 {
            b.push_str("\n\n");
            if below > 20 {
                b.push_str(&format!("↓ encore ~{below} px sous la vue — browser_scroll(down) pour révéler d'autres éléments. "));
            }
            if above > 20 {
                b.push_str(&format!(
                    "↑ ~{above} px au-dessus — browser_scroll(up) pour y revenir."
                ));
            }
        }
        Ok(b.trim_end_matches(' ').to_string())
    }

    /// `snapshotDedup` : une ligne courte si rien n'a changé.
    pub async fn snapshot_dedup(&self) -> Result<String, String> {
        let full = self.snapshot().await?;
        let mut last = lock(&self.last_snap);
        if *last == full {
            return Ok(
                "(page inchangée depuis le dernier affichage — les mêmes numéros restent valides)"
                    .into(),
            );
        }
        *last = full.clone();
        Ok(full)
    }

    /// `find` : les éléments dont le rôle ou le nom contient la requête.
    pub async fn find(&self, query: &str) -> Result<String, String> {
        let expr = format!(
            "{}({})({})",
            js::HELPERS_JS,
            js::FIND_JS,
            crate::gojson::marshal(query)
        );
        let val = self.eval(&expr).await?;
        let refs: Vec<String> = match val {
            None => return Err("recherche illisible: unexpected end of JSON input".into()),
            Some(v) => v
                .get("refs")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
                .map(|r| r.as_str().unwrap_or("").to_string())
                .collect(),
        };
        if refs.is_empty() {
            return Ok(format!(
                "Aucun élément contenant « {query} » sur cette page. Essaie un autre mot, ou c'est peut-être ailleurs (autre page/lien)."
            ));
        }
        Ok(format!(
            "{} élément(s) contenant « {query} » (le 1er est amené dans la vue) — clique par numéro :\n{}",
            refs.len(),
            refs.join("\n")
        ))
    }

    /// `waitLoad` : chargement fini, puis réseau au calme.
    async fn wait_load(&self) {
        let deadline = Instant::now() + Duration::from_secs(10);
        while Instant::now() < deadline {
            if let Ok(Some(Value::String(s))) = self.eval("document.readyState").await
                && s == "complete"
            {
                break;
            }
            tokio::time::sleep(Duration::from_millis(150)).await;
        }
        self.settle().await;
    }

    /// `settle` : plus de requête en vol depuis 600 ms (6 s au plus).
    async fn settle(&self) {
        let quiet = Duration::from_millis(600);
        let deadline = Instant::now() + Duration::from_secs(6);
        while Instant::now() < deadline {
            let (inflight, last) = *lock(&self.net);
            if inflight <= 0 && last.is_none_or(|t| t.elapsed() >= quiet) {
                return;
            }
            tokio::time::sleep(Duration::from_millis(120)).await;
        }
    }

    /// `navigate` : une URL, « back » ou « forward ».
    pub async fn navigate(&self, url: &str) -> Result<(), String> {
        lock(&self.last_snap).clear();
        match url.trim().to_lowercase().as_str() {
            "back" => {
                let r = self.eval("history.back();'ok'").await;
                self.wait_load().await;
                return r.map(|_| ());
            }
            "forward" => {
                let r = self.eval("history.forward();'ok'").await;
                self.wait_load().await;
                return r.map(|_| ());
            }
            _ => {}
        }
        let url = if url.contains("://") {
            url.to_string()
        } else {
            format!("https://{url}")
        };
        let res = self
            .call("Page.navigate", Some(json!({ "url": url })))
            .await?;
        let error_text = res
            .get("errorText")
            .and_then(Value::as_str)
            .unwrap_or("")
            .to_string();
        self.wait_load().await;
        let loc = self
            .eval("location.href")
            .await
            .ok()
            .flatten()
            .map(|v| v.to_string())
            .unwrap_or_default();
        let failed = loc.contains("chrome-error")
            || (!error_text.is_empty() && error_text != "net::ERR_ABORTED");
        if failed {
            let reason = if error_text.is_empty() {
                "page injoignable".to_string()
            } else {
                error_text
            };
            return Err(format!(
                "chargement de {url} échoué ({reason}) — URL/hôte invalide, DNS, ou site bloqué ; corrige l'URL, essaie 'back', ou passe par un lien de la page"
            ));
        }
        Ok(())
    }

    /// `refCenter` : le centre d'un élément marqué, amené au milieu.
    async fn ref_center(&self, r: i64) -> Result<(f64, f64), String> {
        let expr = format!(
            "{}(function(){{var el=ajByRef({r});if(!el){{return null;}}el.scrollIntoView({{block:'center',inline:'center'}});var r=el.getBoundingClientRect();return {{x:r.left+r.width/2,y:r.top+r.height/2}};}})()",
            js::HELPERS_JS
        );
        let val = self.eval(&expr).await?;
        let p = match val {
            None | Some(Value::Null) => {
                return Err(format!(
                    "élément [{r}] introuvable — refais un browser_snapshot (la page a peut-être changé)"
                ));
            }
            Some(p) => p,
        };
        let f = |k: &str| {
            p.as_object()
                .and_then(|o| {
                    o.get(k).or_else(|| {
                        o.iter()
                            .find(|(n, _)| n.eq_ignore_ascii_case(k))
                            .map(|(_, v)| v)
                    })
                })
                .and_then(Value::as_f64)
                .unwrap_or(0.0)
        };
        Ok((f("x"), f("y")))
    }

    async fn mouse_click(&self, x: f64, y: f64) -> Result<(), String> {
        for typ in ["mousePressed", "mouseReleased"] {
            self.call(
                "Input.dispatchMouseEvent",
                Some(json!({ "type": typ, "x": go_num(x), "y": go_num(y), "button": "left", "clickCount": 1 })),
            )
            .await?;
        }
        self.wait_load().await;
        Ok(())
    }

    /// `clickRef`.
    pub async fn click_ref(&self, r: i64) -> Result<(), String> {
        let (x, y) = self.ref_center(r).await?;
        self.mouse_click(x, y).await
    }

    /// `clickXY`.
    pub async fn click_xy(&self, x: f64, y: f64) -> Result<(), String> {
        self.mouse_click(x, y).await
    }

    /// `typeText`.
    pub async fn type_text(&self, text: &str) -> Result<(), String> {
        self.call("Input.insertText", Some(json!({ "text": text })))
            .await
            .map(|_| ())
    }

    /// `typeInto` : un champ, ou l'option d'une liste déroulante native.
    pub async fn type_into(&self, r: i64, text: &str) -> Result<(), String> {
        let expr = format!(
            r#"{}(function(ref,val){{
  var el=ajByRef(ref);
  if(!el)return 'NOTFOUND';
  el.scrollIntoView({{block:'center'}});
  if(el.tagName.toLowerCase()==='select'){{
    var o=el.options,lv=val.toLowerCase(),k=-1;
    for(var i=0;i<o.length;i++){{var tx=(o[i].text||'').toLowerCase();if(tx===lv||tx.indexOf(lv)>=0||(o[i].value||'').toLowerCase()===lv){{k=i;break;}}}}
    if(k<0)return 'NOOPT';
    el.selectedIndex=k;el.dispatchEvent(new Event('input',{{bubbles:true}}));el.dispatchEvent(new Event('change',{{bubbles:true}}));
    return 'SELECT';
  }}
  el.focus();
  try{{if(typeof el.select==='function')el.select();else if(window.getSelection){{var r=document.createRange();r.selectNodeContents(el);var s=getSelection();s.removeAllRanges();s.addRange(r);}}}}catch(e){{}}
  return 'TEXT';
}})({r},{})"#,
            js::HELPERS_JS,
            crate::gojson::marshal(text)
        );
        let val = self.eval(&expr).await?;
        let v = val.map(|v| v.to_string()).unwrap_or_default();
        match v.trim_matches('"') {
            "NOTFOUND" => Err(format!(
                "champ [{r}] introuvable — refais un browser_snapshot"
            )),
            "NOOPT" => Err(format!(
                "aucune option de la liste [{r}] ne correspond à « {text} » — fais un browser_screenshot pour voir les choix"
            )),
            "SELECT" => Ok(()),
            _ => self.type_text(text).await,
        }
    }

    /// `pressKey`.
    pub async fn press_key(&self, name: &str) -> Result<(), String> {
        let (key, code, vk) = match name.trim().to_lowercase().as_str() {
            "enter" => ("Enter", "Enter", 13),
            "tab" => ("Tab", "Tab", 9),
            "backspace" => ("Backspace", "Backspace", 8),
            "escape" | "esc" => ("Escape", "Escape", 27),
            "delete" => ("Delete", "Delete", 46),
            "up" => ("ArrowUp", "ArrowUp", 38),
            "down" => ("ArrowDown", "ArrowDown", 40),
            "left" => ("ArrowLeft", "ArrowLeft", 37),
            "right" => ("ArrowRight", "ArrowRight", 39),
            "home" => ("Home", "Home", 36),
            "end" => ("End", "End", 35),
            "pageup" => ("PageUp", "PageUp", 33),
            "pagedown" => ("PageDown", "PageDown", 34),
            _ => {
                return Err(format!(
                    "touche inconnue « {name} » — connues : enter, tab, backspace, escape, delete, up/down/left/right, home, end, pageup, pagedown"
                ));
            }
        };
        for typ in ["rawKeyDown", "keyUp"] {
            self.call(
                "Input.dispatchKeyEvent",
                Some(json!({ "type": typ, "key": key, "code": code,
                    "windowsVirtualKeyCode": vk, "nativeVirtualKeyCode": vk })),
            )
            .await?;
        }
        self.wait_load().await;
        Ok(())
    }

    /// `scroll`.
    pub async fn scroll(&self, dir: &str) -> Result<(), String> {
        let (dx, dy) = match dir.trim().to_lowercase().as_str() {
            "down" | "" => (0, 600),
            "up" => (0, -600),
            "right" => (600, 0),
            "left" => (-600, 0),
            _ => {
                return Err(format!(
                    "direction inconnue « {dir} » (up, down, left, right)"
                ));
            }
        };
        self.eval(&format!("window.scrollBy({dx},{dy});'ok'"))
            .await?;
        tokio::time::sleep(Duration::from_millis(200)).await;
        Ok(())
    }

    /// `screenshot` : le viewport en PNG.
    pub async fn screenshot(&self) -> Result<Vec<u8>, String> {
        let res = self
            .call("Page.captureScreenshot", Some(json!({ "format": "png" })))
            .await?;
        let data = res.get("data").and_then(Value::as_str).unwrap_or("");
        base64::engine::general_purpose::STANDARD
            .decode(data)
            .map_err(|e| format!("illegal base64 data: {e}"))
    }
}

fn go_kind(v: &Value) -> &'static str {
    match v {
        Value::Null => "null",
        Value::Bool(_) => "bool",
        Value::Number(_) => "number",
        Value::String(_) => "string",
        Value::Array(_) => "array",
        Value::Object(_) => "object",
    }
}

// --- Le navigateur de la façade -------------------------------------------------

/// Le résultat d'un outil du navigateur.
#[derive(Debug, Default, Clone, PartialEq)]
pub struct Outcome {
    pub result: String,
    /// `browser_screenshot` : la partie image relayée au modèle.
    pub image: Option<Value>,
}

/// L'anneau des aperçus : ordre d'arrivée, JPEG par identifiant.
type ShotRing = (Vec<String>, HashMap<String, Vec<u8>>);

/// La session du navigateur et l'anneau des aperçus.
#[derive(Default)]
pub struct Browser {
    cur: tokio::sync::Mutex<Option<Arc<Session>>>,
    shots: Mutex<ShotRing>,
    /// Binaire et dossier des profils imposés (tests) ; sinon
    /// `chrome_path` et le dossier temporaire du système.
    imposed: Mutex<(Option<String>, Option<PathBuf>)>,
}

impl Browser {
    /// Pour les tests : binaire et dossier des profils imposés.
    pub fn with(chrome: &str, tmp: PathBuf) -> Self {
        let b = Browser::default();
        b.impose(chrome, tmp);
        b
    }

    /// Impose le binaire et le dossier des profils (tests).
    pub fn impose(&self, chrome: &str, tmp: PathBuf) {
        *lock(&self.imposed) = (Some(chrome.to_string()), Some(tmp));
    }

    /// Le dernier aperçu de l'anneau.
    pub fn last_shot(&self) -> Option<String> {
        lock(&self.shots).0.last().cloned()
    }
}

/// Le navigateur de ce processus.
pub fn global() -> Arc<Browser> {
    static G: std::sync::OnceLock<Arc<Browser>> = std::sync::OnceLock::new();
    G.get_or_init(Default::default).clone()
}

fn int_arg(args: &Map<String, Value>, k: &str) -> Option<i64> {
    args.get(k)
        .and_then(Value::as_f64)
        .map(crate::webtools::go_int)
}

fn arg_str(args: &Map<String, Value>, k: &str) -> String {
    args.get(k)
        .and_then(Value::as_str)
        .unwrap_or("")
        .to_string()
}

impl Browser {
    /// `cdpGet` : la session, (re)lancée au besoin ; un ping vérifie
    /// qu'une session existante répond encore.
    pub async fn get(&self) -> Result<Arc<Session>, String> {
        let mut cur = self.cur.lock().await;
        if let Some(s) = cur.as_ref() {
            if s.call(
                "Runtime.evaluate",
                Some(json!({ "expression": "1", "returnByValue": true })),
            )
            .await
            .is_ok()
            {
                return Ok(s.clone());
            }
            s.close();
            *cur = None;
        }
        let (chrome, tmp) = lock(&self.imposed).clone();
        let bin = chrome.unwrap_or_else(crate::system::chrome_path);
        let tmp = tmp.unwrap_or_else(std::env::temp_dir);
        let s = Session::launch(&bin, &tmp).await?;
        *cur = Some(s.clone());
        Ok(s)
    }

    /// `cdpShutdown`.
    pub async fn shutdown(&self) {
        if let Some(s) = self.cur.lock().await.take() {
            s.close();
        }
    }

    /// Une session est-elle ouverte ?
    pub async fn running(&self) -> bool {
        self.cur.lock().await.is_some()
    }

    /// Un outil `browser_*` (`None` pour un autre). `vision` : le modèle
    /// voit-il les images ; `workspace` : où ranger la capture.
    pub async fn exec(
        &self,
        name: &str,
        args: &Map<String, Value>,
        vision: bool,
        workspace: &str,
    ) -> Option<Outcome> {
        let result = match name {
            "browser_open" => self.tool_open(args).await,
            "browser_snapshot" => self.tool_snapshot().await,
            "browser_find" => self.tool_find(args).await,
            "browser_click" => self.tool_click(args).await,
            "browser_click_xy" => self.tool_click_xy(args).await,
            "browser_type" => self.tool_type(args).await,
            "browser_key" => self.tool_key(args).await,
            "browser_scroll" => self.tool_scroll(args).await,
            "browser_screenshot" => return Some(self.tool_screenshot(vision, workspace).await),
            _ => return None,
        };
        Some(Outcome {
            result: cap_output(&result),
            image: None,
        })
    }

    async fn tool_open(&self, args: &Map<String, Value>) -> String {
        let url = arg_str(args, "url");
        if url.trim().is_empty() {
            return "[erreur] url manquante".into();
        }
        let s = match self.get().await {
            Ok(s) => s,
            Err(e) => return format!("[erreur] {e}"),
        };
        if let Err(e) = s.navigate(&url).await {
            return format!("[erreur] navigation : {e}");
        }
        s.snapshot_dedup()
            .await
            .unwrap_or_else(|e| format!("[erreur] {e}"))
    }

    async fn tool_snapshot(&self) -> String {
        let s = match self.get().await {
            Ok(s) => s,
            Err(e) => return format!("[erreur] {e}"),
        };
        s.snapshot_dedup()
            .await
            .unwrap_or_else(|e| format!("[erreur] {e}"))
    }

    async fn tool_find(&self, args: &Map<String, Value>) -> String {
        let text = arg_str(args, "text");
        if text.trim().is_empty() {
            return "[erreur] texte de recherche manquant".into();
        }
        let s = match self.get().await {
            Ok(s) => s,
            Err(e) => return format!("[erreur] {e}"),
        };
        s.find(&text)
            .await
            .unwrap_or_else(|e| format!("[erreur] {e}"))
    }

    async fn tool_click(&self, args: &Map<String, Value>) -> String {
        let Some(r) = int_arg(args, "ref") else {
            return "[erreur] ref manquant (numéro de l'élément)".into();
        };
        let s = match self.get().await {
            Ok(s) => s,
            Err(e) => return format!("[erreur] {e}"),
        };
        if let Err(e) = s.click_ref(r).await {
            return format!("[erreur] {e}");
        }
        let snap = s.snapshot_dedup().await.unwrap_or_default();
        format!("[ok] cliqué [{r}]\n\n{snap}")
    }

    async fn tool_type(&self, args: &Map<String, Value>) -> String {
        let text = arg_str(args, "text");
        let s = match self.get().await {
            Ok(s) => s,
            Err(e) => return format!("[erreur] {e}"),
        };
        let r = match int_arg(args, "ref") {
            Some(r) => s.type_into(r, &text).await,
            None => s.type_text(&text).await,
        };
        if let Err(e) = r {
            return format!("[erreur] {e}");
        }
        let snap = s.snapshot_dedup().await.unwrap_or_default();
        format!("[ok] saisi « {text} »\n\n{snap}")
    }

    async fn tool_key(&self, args: &Map<String, Value>) -> String {
        let key = arg_str(args, "key");
        let s = match self.get().await {
            Ok(s) => s,
            Err(e) => return format!("[erreur] {e}"),
        };
        if let Err(e) = s.press_key(&key).await {
            return format!("[erreur] {e}");
        }
        let snap = s.snapshot_dedup().await.unwrap_or_default();
        format!("[ok] touche {key}\n\n{snap}")
    }

    async fn tool_click_xy(&self, args: &Map<String, Value>) -> String {
        let (Some(x), Some(y)) = (
            args.get("x").and_then(Value::as_f64),
            args.get("y").and_then(Value::as_f64),
        ) else {
            return "[erreur] coordonnées x et y requises (en pixels du screenshot)".into();
        };
        let s = match self.get().await {
            Ok(s) => s,
            Err(e) => return format!("[erreur] {e}"),
        };
        if let Err(e) = s.click_xy(x, y).await {
            return format!("[erreur] {e}");
        }
        let snap = s.snapshot_dedup().await.unwrap_or_default();
        format!("[ok] clic en ({x:.0}, {y:.0})\n\n{snap}")
    }

    async fn tool_scroll(&self, args: &Map<String, Value>) -> String {
        let dir = arg_str(args, "direction");
        let s = match self.get().await {
            Ok(s) => s,
            Err(e) => return format!("[erreur] {e}"),
        };
        if let Err(e) = s.scroll(&dir).await {
            return format!("[erreur] {e}");
        }
        s.snapshot_dedup().await.unwrap_or_default()
    }

    /// `toolCUScreenshot` : la capture, rangée propre dans le dossier de
    /// travail ; celle du modèle porte la grille.
    async fn tool_screenshot(&self, vision: bool, workspace: &str) -> Outcome {
        let err = |e: String| Outcome {
            result: format!("[erreur] {e}"),
            image: None,
        };
        if !vision {
            return err(
                "la vision n'est pas active sur ce modèle — impossible de capturer l'écran".into(),
            );
        }
        let s = match self.get().await {
            Ok(s) => s,
            Err(e) => return err(e),
        };
        let png = match s.screenshot().await {
            Ok(p) => p,
            Err(e) => return err(e),
        };
        let mut saved = String::new();
        let name = format!("capture-{}.png", local_stamp());
        if !workspace.is_empty() && std::fs::write(Path::new(workspace).join(&name), &png).is_ok() {
            saved = name;
        }
        // 1280×800 : sous la taille maximale de `prepareImageForModel`, la
        // capture quadrillée part telle quelle.
        let b = grid::overlay_grid(&png);
        let part = json!({
            "type": "image_url",
            "image_url": { "url": format!("data:image/png;base64,{}", base64::engine::general_purpose::STANDARD.encode(&b)) },
        });
        let mut msg = format!(
            "[ok] capture prise (tu la vois ci-dessous, {VIEW_W}x{VIEW_H} px, 1:1 avec la page). Une GRILLE de repères y est surimprimée (lignes + chiffres tous les 100 px) POUR TOI seulement, afin de viser précisément — le fichier enregistré, lui, est PROPRE (sans grille), donc l'aperçu ![capture] montré à l'utilisateur est net. Si un élément visible n'est PAS dans la liste numérotée (bandeau cookies en iframe, canvas), clique-le du premier coup avec browser_click_xy(x,y), coordonnées lues sur la grille."
        );
        if !saved.is_empty() {
            msg += &format!(
                " Enregistrée sous « {saved} ». Pour l'AFFICHER à l'utilisateur dans le chat, insère-la en IMAGE Markdown : ![capture]({saved}). Un simple lien [capture]({saved}) ne donne qu'un bouton de téléchargement, sans aperçu. Ne cherche pas de fichier sur le disque, c'est celui-ci."
            );
        }
        Outcome {
            result: msg,
            image: Some(part),
        }
    }

    /// `cuAutoShot` : la page capturée pour l'aperçu (JPEG en mémoire) ;
    /// "" si impossible, jamais bloquant.
    pub async fn auto_shot(&self) -> String {
        let s = match self.get().await {
            Ok(s) => s,
            Err(e) => {
                eprintln!("[cu] aperçu : navigateur indisponible : {e}");
                return String::new();
            }
        };
        let raw = match s.screenshot().await {
            Ok(r) => r,
            Err(e) => {
                eprintln!("[cu] aperçu : capture impossible : {e}");
                return String::new();
            }
        };
        let Ok(img) = image::load_from_memory(&raw) else {
            eprintln!("[cu] aperçu : image illisible ({} octets)", raw.len());
            return String::new();
        };
        let mut out = std::io::Cursor::new(Vec::new());
        let enc = image::codecs::jpeg::JpegEncoder::new_with_quality(&mut out, 90);
        if img.to_rgb8().write_with_encoder(enc).is_err() {
            return String::new();
        }
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_nanos())
            .unwrap_or_default();
        let id = format!("{nanos:x}");
        let mut shots = lock(&self.shots);
        shots.1.insert(id.clone(), out.into_inner());
        shots.0.push(id.clone());
        while shots.0.len() > SHOT_RING {
            let old = shots.0.remove(0);
            shots.1.remove(&old);
        }
        id
    }

    /// `handleCUShot` : un aperçu encore dans l'anneau.
    pub fn shot(&self, id: &str) -> Option<Vec<u8>> {
        lock(&self.shots).1.get(id.trim()).cloned()
    }
}

/// `time.Now().Format("20060102-150405")`, heure locale.
fn local_stamp() -> String {
    let sec = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or_default();
    let c = crate::tz::Location::local().civil(sec);
    format!(
        "{:04}{:02}{:02}-{:02}{:02}{:02}",
        c.year, c.month, c.day, c.hour, c.minute, c.second
    )
}

#[cfg(test)]
mod tests;
