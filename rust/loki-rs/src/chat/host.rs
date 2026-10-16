//! Le tour branché sur le vrai monde : le [`TurnHost`] de la façade.
//!
//! - envoi au moteur par [`EngineHttp`] (clé de CET endpoint), pannes
//!   classées et formulées comme `friendlyLLMError` ;
//! - outils fichiers et shell exécutés ici ([`crate::tools`], état partagé
//!   tenu par le Go), outils MCP par le pool de la façade quand elle le tient
//!   (lot 36), outils task_* quand elle tient les tâches (lot 37), critères
//!   du mode Code (lot 38), outils mémoire quand elle tient la mémoire
//!   (lot 40) ; les autres
//!   par le Go (`/internal/v1/tool/exec`), avec
//!   ce qu'il tire du résultat (capture de la bulle, aperçu du navigateur,
//!   image relayée) ;
//! - résultats complets (« voir plus ») confiés à l'écrivain des discussions ;
//! - compaction par [`Compactor`] : résumé demandé au moteur
//!   ([`EngineSummarizer`]), blocs archivés pour recall par le Go
//!   ([`StoreRecall`]) ;
//! - images jointes (`loki-img:…`) lues sur disque et ouvertes par le Go
//!   (déchiffrées), gardées en cache comme `chatImageDataURL` ;
//! - slots du moteur (lot 31) : avant chaque requête au moteur local, le Go
//!   rend le slot où l'envoyer (SIDE_SLOT) et recharge au besoin l'état
//!   gardé de la discussion (SLOT_PERSIST) ; la requête est comptée en vol,
//!   sur ce slot, jusqu'à la fin de sa lecture.
//!
//! Écarts assumés : le résumé en continuation (COMPACT_CONTINUATION) demande
//! l'état du slot du moteur, tenu par le Go — `slot_holds` rend faux et la
//! compaction résume la transcription. Ni conseil de cache ni garde
//! d'écriture du mode Code (état du Go) : `cache_hint` et `preflight` absents.

use std::collections::{HashMap, VecDeque};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use futures_util::StreamExt;
use serde_json::{Map, Value, json};

use super::compact::{
    CompactCaps, CompactConfig, CompactMemory, CompactOpts, Compactor, ContinuationRequest,
    RecallArchive, Summarizer, SummaryChoice, SummaryError, TranscriptRequest, compact_opts_mid,
    parse_summary_response,
};
use super::engine::{EngineHttp, classify, friendly_error};
use super::persist::{Persister, ProjSnapshot, StoreSink, ToolResJob};
use super::stream::{CacheHintFn, PreflightFn, RetryPatterns};
use super::turn::{CompactPhase, PerfMarks, PerfRec, Reply, SendError, ToolOutcome, TurnHost};
use super::types::{AskEvent, DiffLine, Message, Tool};
use super::wire::{
    ChatEndpoint, ChatPayloadOpts, EchoRefusals, EffortFallbacks, IMG_REF_SCHEME,
    build_chat_payload, content_parts, part_image_url, wire_messages,
};
use crate::store::{BK_STATE, Store};

/// Taille au-delà de laquelle le cache d'images oublie les plus anciennes
/// (`imgCacheMax`).
pub const IMG_CACHE_MAX: usize = 64 << 20;
/// Corps d'un refus de résumé gardé au plus (`postSummary`).
const SUMMARY_ERR_MAX: usize = 500;

/// Base64 standard (avec remplissage), comme `base64.StdEncoding`.
pub fn b64(data: &[u8]) -> String {
    const T: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut out = String::with_capacity(data.len().div_ceil(3) * 4);
    for c in data.chunks(3) {
        let n = (c[0] as u32) << 16
            | (*c.get(1).unwrap_or(&0) as u32) << 8
            | *c.get(2).unwrap_or(&0) as u32;
        out.push(T[(n >> 18) as usize & 63] as char);
        out.push(T[(n >> 12) as usize & 63] as char);
        out.push(if c.len() > 1 {
            T[(n >> 6) as usize & 63] as char
        } else {
            '='
        });
        out.push(if c.len() > 2 {
            T[n as usize & 63] as char
        } else {
            '='
        });
    }
    out
}

/// Type MIME d'une image rangée, d'après son extension (`imgExtByMime`).
fn image_mime(name: &str) -> &'static str {
    for (ext, mime) in [
        (".png", "image/png"),
        (".gif", "image/gif"),
        (".webp", "image/webp"),
    ] {
        if name.ends_with(ext) {
            return mime;
        }
    }
    "image/jpeg"
}

/// Nom d'une image rangée, si la référence est sûre (ni chemin ni `..`).
fn image_name(r: &str) -> Option<&str> {
    let name = r.strip_prefix(IMG_REF_SCHEME)?;
    (!name.is_empty() && !name.contains(['/', '\\']) && !name.contains("..")).then_some(name)
}

#[derive(Default)]
struct ImgCacheInner {
    map: HashMap<String, String>,
    order: VecDeque<String>,
    bytes: usize,
}

/// Images jointes en data-URL, bornées à [`IMG_CACHE_MAX`] (la plus ancienne
/// part en premier, jamais la dernière).
pub struct ImageCache {
    dir: PathBuf,
    inner: Mutex<ImgCacheInner>,
}

impl ImageCache {
    /// `LOKI_HOME` (le parent du dossier des images).
    pub fn home(&self) -> PathBuf {
        self.dir.parent().map(Path::to_path_buf).unwrap_or_default()
    }

    /// `dir` : `LOKI_HOME/chatimg`.
    pub fn new(dir: PathBuf) -> Self {
        ImageCache {
            dir,
            inner: Mutex::new(ImgCacheInner::default()),
        }
    }

    pub fn get(&self, r: &str) -> Option<String> {
        let name = image_name(r)?;
        self.inner.lock().ok()?.map.get(name).cloned()
    }

    fn put(&self, name: &str, url: String) {
        let Ok(mut g) = self.inner.lock() else { return };
        if g.map.contains_key(name) {
            return;
        }
        g.bytes += url.len();
        g.map.insert(name.to_string(), url);
        g.order.push_back(name.to_string());
        while g.bytes > IMG_CACHE_MAX && g.order.len() > 1 {
            if let Some(old) = g.order.pop_front()
                && let Some(u) = g.map.remove(&old)
            {
                g.bytes -= u.len();
            }
        }
    }

    /// Lit et ouvre (par le Go) les images référencées pas encore en cache.
    /// Une image illisible reste absente : la requête dira « indisponible ».
    pub async fn prefetch(&self, store: &Store, msgs: &[Message]) {
        for m in msgs {
            for p in content_parts(m.content.as_ref()) {
                let Some(name) = image_name(part_image_url(p)) else {
                    continue;
                };
                if self.inner.lock().is_ok_and(|g| g.map.contains_key(name)) {
                    continue;
                }
                let Ok(raw) = tokio::fs::read(self.dir.join(name)).await else {
                    continue;
                };
                let Ok(b) = store.open(raw).await else {
                    continue;
                };
                self.put(
                    name,
                    format!("data:{};base64,{}", image_mime(name), b64(&b)),
                );
            }
        }
    }
}

/// Le moteur vu de la compaction (`postSummary`).
#[derive(Clone)]
pub struct EngineSummarizer {
    pub http: EngineHttp,
    pub ep: ChatEndpoint,
    pub cfg: HashMap<String, String>,
    pub patterns: Arc<RetryPatterns>,
    pub port: u16,
    /// Le Go et la discussion des résumés (requêtes en vol, slot du
    /// moteur), et l'anneau de télémétrie (kind=compact). `None` sans base
    /// (tests).
    pub go: Option<(Store, String, Arc<crate::perfring::PerfRing>)>,
}

impl EngineSummarizer {
    /// `postSummary`. `main` : le résumé prolonge le prompt du slot de la
    /// discussion (continuation) ; sinon c'est un prompt neuf, sur le second
    /// slot quand il est en service (SIDE_SLOT).
    async fn post(&self, mut payload: Value, main: bool) -> Result<SummaryChoice, SummaryError> {
        let mut _gate = None;
        if !self.ep.external
            && let Some((store, _, _)) = &self.go
        {
            let slot = engine_slot_for(store, "", main).await;
            if let Some(n) = slot
                && let Some(o) = payload.as_object_mut()
            {
                o.insert("id_slot".into(), Value::from(n));
            }
            _gate = engine_begin(store, "", slot == Some(1), false).await;
        }
        let body = serde_json::to_vec(&payload).unwrap_or_default();
        let resp = self
            .http
            .post(&self.ep.url, &self.ep.key, body)
            .await
            .map_err(|e| {
                SummaryError::Other(friendly_error(classify(&e), &e.to_string(), self.port, ""))
            })?;
        let status = resp.status().as_u16();
        let raw = resp
            .bytes()
            .await
            .map_err(|e| SummaryError::Other(e.to_string()))?;
        if status != 200 {
            let b = &raw[..raw.len().min(SUMMARY_ERR_MAX)];
            return Err(SummaryError::Http {
                who: if self.ep.external {
                    "API externe"
                } else {
                    "llama-server"
                }
                .into(),
                status,
                body: String::from_utf8_lossy(b).trim().to_string(),
            });
        }
        let out = parse_summary_response(&raw);
        // Télémétrie de la compaction : réponse non streamée, timings et usage
        // à la racine, lus sans échec possible — dès que la réponse se lit.
        let decoded = match &out {
            Ok(_) => true,
            Err(SummaryError::Other(e)) => e == super::compact::ERR_EMPTY_SUMMARY,
            Err(_) => false,
        };
        if decoded && let Some((store, conv, ring)) = &self.go {
            let (complete, stats) = super::perf::wire_stats(&raw);
            let rec = PerfRec {
                complete,
                external: self.ep.external,
                stats,
                ..Default::default()
            };
            super::perf::record(store, ring, super::perf::KIND_COMPACT, conv, &rec).await;
        }
        out
    }
}

impl Summarizer for EngineSummarizer {
    async fn summarize_transcript(
        &self,
        req: TranscriptRequest,
    ) -> Result<SummaryChoice, SummaryError> {
        let mut p = json!({
            "model": self.ep.model,
            "messages": [
                {"role": "system", "content": req.system},
                {"role": "user", "content": req.transcript},
            ],
            "stream": false,
            "temperature": 0.2,
            "max_tokens": req.max_tokens,
            "chat_template_kwargs": {"enable_thinking": false},
        });
        // Propre à llama.cpp : une API distante stricte refuse l'argument.
        if self.ep.external
            && let Some(o) = p.as_object_mut()
        {
            o.remove("chat_template_kwargs");
        }
        self.post(p, false).await
    }

    async fn summarize_continuation(
        &self,
        req: ContinuationRequest,
    ) -> Result<SummaryChoice, SummaryError> {
        let mut p = build_chat_payload(
            &self.ep,
            &req.messages,
            0.2,
            &ChatPayloadOpts {
                tools: &req.tools,
                disable_tools: false,
                tool_choice_none: true,
                effort: &req.effort,
                kwargs: Some(&req.kwargs),
                no_sampling: true,
            },
            &self.cfg,
            &|_| None,
        );
        if let Some(o) = p.as_object_mut() {
            o.insert("stream".into(), Value::Bool(false));
            o.remove("stream_options");
            o.insert("max_tokens".into(), json!(req.max_tokens));
        }
        self.post(p, true).await
    }

    fn slot_holds(&self, _conv: &str) -> bool {
        false
    }

    fn textual_tool_call(&self, content: &str) -> bool {
        self.patterns.textual_tool_call(content)
    }
}

/// Archive recall tenue par le Go.
pub struct StoreRecall(pub Store);

impl RecallArchive for StoreRecall {
    async fn archive(&self, label: &str, role: &str, content: &str) -> Result<String, String> {
        self.0.recall_archive(label, role, content).await
    }
}

/// Ce qui survit d'un tour à l'autre (un exemplaire par processus).
pub struct ChatShared {
    pub http: EngineHttp,
    pub store: Store,
    pub persister: Persister<StoreSink>,
    pub effort: EffortFallbacks,
    pub echo_refusals: EchoRefusals,
    pub compact_memory: CompactMemory,
    pub images: ImageCache,
    pub patterns: Arc<RetryPatterns>,
    /// Sonde de gabarit (`chat_tplprobe.go`) : derniers verdicts et cache.
    pub tpl: Arc<super::tplprobe::TplProbe>,
    /// Préchauffage (PREWARM) : celui en vol, capacités des discussions.
    pub prewarm: super::prewarm::Prewarm,
    /// L'anneau de télémétrie des complétions (lot 39) : celles de la
    /// façade, et celles que le Go lui confie.
    pub perf: Arc<crate::perfring::PerfRing>,
    /// Le pool MCP, quand la façade le tient (LOKI_MCP=rust) : les appels
    /// `mcp__*` du tour y vont directement (lot 36).
    pub mcp: Option<Arc<crate::mcp::Mcp>>,
    /// Les tâches, quand la façade les tient (LOKI_TASKS=rust) : les outils
    /// `task_*` du tour s'exécutent ici (lot 37).
    pub tasks: Option<Arc<crate::tasks::Tasks>>,
    /// La mémoire, quand la façade la tient (LOKI_MEM=rust) : les outils
    /// `mem_*` et `tracker` du tour s'exécutent ici (lot 40).
    pub memory: Option<Arc<crate::memory::Memory>>,
    /// Les outils web (lot 42) : cache des pages, Crawl4AI, DuckDuckGo.
    pub web: Arc<crate::webtools::Web>,
    /// Les jobs d'arrière-plan du mode Code (lot 43).
    pub jobs: Arc<crate::tools::jobs::BgJobs>,
    /// Le navigateur piloté (lot 44).
    pub browser: Arc<crate::browser::Browser>,
    /// La sonde « le moteur voit-il les images » (lot 45).
    pub vision: Arc<crate::imagetools::VisionProbe>,
    /// La version du Go, annoncée aux serveurs MCP à une reconnexion ; lue
    /// une fois.
    pub go_version: tokio::sync::OnceCell<String>,
}

impl ChatShared {
    /// [`Self::go_version`] : `/internal/v1/auth`, retenue une fois lue.
    pub async fn go_version(&self) -> String {
        if let Some(v) = self.go_version.get() {
            return v.clone();
        }
        let v = self
            .store
            .internal_json(hyper::Method::GET, "/internal/v1/auth", None)
            .await
            .ok()
            .and_then(|v| v.get("version").and_then(Value::as_str).map(str::to_string))
            .unwrap_or_default();
        if !v.is_empty() {
            let _ = self.go_version.set(v.clone());
        }
        v
    }
}

/// Ce que la compaction sait du tour.
pub struct TurnCompact {
    pub cfg: CompactConfig,
    pub caps: CompactCaps,
    /// Mode Code : critères rendus de la discussion.
    pub criteria: String,
    /// Discussion du tour (vide : tâche ou sous-agent).
    pub main_conv: String,
    /// Forme de la requête du tour, pour les options de la compaction en
    /// cours de tour.
    pub tools: Vec<Tool>,
    pub effort: String,
    pub kwargs: Map<String, Value>,
}

/// Le [`TurnHost`] de la façade.
pub struct GoTurnHost {
    pub shared: Arc<ChatShared>,
    /// Discussion dont relèvent les résultats complets.
    pub conv_id: String,
    /// Capacités du tour, telles que l'API d'outils les attend (champs Go).
    pub caps: Value,
    pub cfg: HashMap<String, String>,
    pub port: u16,
    pub summarizer: EngineSummarizer,
    pub compact: TurnCompact,
    /// Messages mis en file par l'utilisateur pendant le tour.
    pub queued: Box<dyn Fn() -> Vec<Message> + Send + Sync>,
    /// Un événement pour l'écran, hors du flux du tour (la liste des
    /// critères après l'outil `criteria`).
    pub deltas: Arc<dyn Fn(super::journal::Delta) + Send + Sync>,
    /// Nature des complétions pour la télémétrie : `main` (tour de la
    /// discussion), `verify` (passe de vérification du mode Code).
    pub perf_kind: &'static str,
    /// La requête qui part va sur le second slot (posé par `engine_slot`,
    /// lu par `send` pour le ticket « en vol »).
    pub side_slot: AtomicBool,
    /// PROJ_SNAPSHOT : la clé et le système du tour (`None` sans la clé), et
    /// l'instantané posé par un rafraîchissement en cours de tour.
    pub projsnap: Option<SnapTurn>,
    pub snap_refreshed: Mutex<Option<ProjSnapshot>>,
    /// Ce que le tour prête à l'outil `subagent` (lot 47) ; `None` : l'outil
    /// passe par le Go.
    pub sub: Option<super::subagent::SubTurn>,
}

/// PROJ_SNAPSHOT (lot 34) : ce qu'un tour avec la clé garde pour rafraîchir
/// le bloc projet quand une compaction réécrit l'historique en plein tour
/// (`projSnapTurn`).
#[derive(Debug, Clone)]
pub struct SnapTurn {
    pub caps: Value,
    pub key: String,
    pub sys_hash: String,
}

impl GoTurnHost {
    /// L'instantané posé en cours de tour (`takeRefreshed`), s'il y en a un.
    pub fn take_refreshed_snap(&self) -> Option<ProjSnapshot> {
        self.snap_refreshed
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .take()
    }
}

/// Délai d'une demande de slot : le rechargement SLOT_PERSIST relit jusqu'à
/// 8 Gio (`slotPersistRestoreTimeout`, deux minutes).
const SLOT_ASK_TIMEOUT: Duration = Duration::from_secs(180);

/// `engineSlotFor` demandé au Go (`/internal/v1/turn/slot`). `main` : fil de
/// la discussion ; `conv` (étape d'un tour seulement) : le Go recharge
/// d'abord l'état SLOT_PERSIST de cette discussion. Go injoignable : aucun
/// `id_slot`, la requête d'avant la clé.
pub async fn engine_slot_for(store: &Store, conv: &str, main: bool) -> Option<u8> {
    match store
        .internal_post_within(
            "/internal/v1/turn/slot",
            &json!({ "conv": conv, "main": main }),
            SLOT_ASK_TIMEOUT,
        )
        .await
    {
        Ok((code, v)) if code.is_success() => v["slot"].as_i64().and_then(|n| u8::try_from(n).ok()),
        Ok((code, v)) => {
            eprintln!("[loki-rs] moteur : slot non demandé ({code} {v})");
            None
        }
        Err(e) => {
            eprintln!("[loki-rs] moteur : slot non demandé ({e})");
            None
        }
    }
}

/// Ouvre un ticket « en vol » auprès du Go (`engineRequestBeginSide`) : le
/// slot de la discussion n'est pas effacé pendant la requête, et un
/// préchauffage est annulé — sauf `keep_prewarm`, une étape de tour qui le
/// prolonge. `main_conv` : étape d'un tour de cette discussion ; `side` :
/// requête sur le second slot. `None` si le Go ne répond pas — la requête
/// part quand même.
pub async fn engine_begin(
    store: &Store,
    main_conv: &str,
    side: bool,
    keep_prewarm: bool,
) -> Option<GateTicket> {
    match store
        .internal_json(
            hyper::Method::POST,
            "/internal/v1/oai/begin",
            Some(&json!({ "main_conv": main_conv, "side": side, "prewarm_keep": keep_prewarm })),
        )
        .await
    {
        Ok(v) => v["ticket"].as_u64().map(|ticket| GateTicket {
            store: store.clone(),
            ticket,
        }),
        Err(e) => {
            eprintln!("[loki-rs] moteur : requête non comptée en vol ({e})");
            None
        }
    }
}

/// KEEP_TURN_IMAGES (lot 32) : la vision est-elle réellement active
/// (projecteur déclaré et vu par le moteur) et le budget des images gardées.
/// Go injoignable : `None` (images éphémères).
pub async fn keep_images_state(store: &Store) -> Option<(bool, i64)> {
    match store
        .internal_post("/internal/v1/turn/keepimg", &json!({}))
        .await
    {
        Ok((code, v)) if code.is_success() => Some((
            v["vision"].as_bool().unwrap_or(false),
            v["budget"].as_i64().unwrap_or(0),
        )),
        _ => None,
    }
}

/// Rend le ticket « en vol » au Go quand la réponse du moteur est lue ou
/// abandonnée (`engineRequestBeginSide` … `end`).
pub struct GateTicket {
    store: Store,
    ticket: u64,
}

impl Drop for GateTicket {
    fn drop(&mut self) {
        let (store, ticket) = (self.store.clone(), self.ticket);
        if let Ok(rt) = tokio::runtime::Handle::try_current() {
            rt.spawn(async move {
                let _ = store
                    .internal_json(
                        hyper::Method::POST,
                        "/internal/v1/oai/end",
                        Some(&json!({ "ticket": ticket })),
                    )
                    .await;
            });
        }
    }
}

impl GoTurnHost {
    async fn gate_begin(&self, keep_prewarm: bool) -> Option<GateTicket> {
        let main_conv = if self.perf_kind == super::perf::KIND_MAIN {
            self.conv_id.as_str()
        } else {
            ""
        };
        let side = self.side_slot.load(Ordering::SeqCst);
        engine_begin(&self.shared.store, main_conv, side, keep_prewarm).await
    }
}

/// `toolResID` : « <discussion>.<24 hex> ».
fn tool_res_id(conv: &str) -> String {
    let mut b = [0u8; 12];
    let _ = getrandom::fill(&mut b);
    let conv = match conv.trim() {
        "" => "nosession",
        c => c,
    };
    format!("{conv}.{}", hex::encode(b))
}

/// Réponse de `/internal/v1/tool/exec` → [`ToolOutcome`].
pub fn tool_outcome(v: &Value, label: &str) -> ToolOutcome {
    let s = |k: &str| v.get(k).and_then(Value::as_str).unwrap_or("").to_string();
    let n = |k: &str| v.get(k).and_then(Value::as_i64).unwrap_or(0);
    let mut relay: Option<Message> = v
        .get("relay")
        .and_then(|r| serde_json::from_value(r.clone()).ok());
    if relay.is_none()
        && let Some(img) = v.get("vision_img").filter(|i| i.is_object())
    {
        relay = Some(see_image_message(label, img.clone()));
    }
    ToolOutcome {
        result: s("result"),
        diff: v
            .get("diff")
            .and_then(|d| serde_json::from_value::<Vec<DiffLine>>(d.clone()).ok())
            .unwrap_or_default(),
        added: n("added"),
        removed: n("removed"),
        ask: v
            .get("ask")
            .and_then(|a| serde_json::from_value::<AskEvent>(a.clone()).ok()),
        image: s("image"),
        live_shot: s("shot"),
        relay,
    }
}

/// `seeImageMessage` : l'image relayée au modèle.
pub fn see_image_message(label: &str, img: Value) -> Message {
    let text = if label.is_empty() {
        "Image :".to_string()
    } else {
        format!("Image demandée ({label}) :")
    };
    Message {
        role: "user".into(),
        content: Some(json!([{"type": "text", "text": text}, img])),
        ..Default::default()
    }
}

impl GoTurnHost {
    /// Les outils exécutés par la façade, sauf `subagent` ; le reste par le
    /// Go (`/tool/exec`). Aussi celui des tours d'un sous-agent.
    pub(crate) async fn exec_tool_base(
        &self,
        name: &str,
        args: &Map<String, Value>,
        label: &str,
    ) -> ToolOutcome {
        // Outils MCP, quand la façade tient le pool : ce que ferait le Go
        // (`mcpCall` → /internal/facade/v1/mcp/call), sans ses deux
        // allers-retours. Le Go n'en tire rien d'autre que le texte.
        // Critères d'acceptation du mode Code (lot 38) : ceux de la
        // discussion du tour ; seule la vérification marque `passed`.
        if name == "criteria" {
            let verifier = self.caps.get("Role").and_then(Value::as_str) == Some("verifier");
            let out =
                super::crittool::exec(&self.shared.store, &self.compact.main_conv, args, verifier)
                    .await;
            if let Some(list) = &out.notify {
                (self.deltas)(super::crittool::notify_delta(
                    list,
                    super::crittool::now_ms(),
                ));
            }
            return ToolOutcome {
                result: out.result,
                ..Default::default()
            };
        }
        // Outils mémoire, quand la façade tient la mémoire (lot 40).
        if crate::memory::tools::is_tool(name)
            && let Some(mem) = &self.shared.memory
        {
            let env = crate::memory::tools::MemEnv {
                store: &self.shared.store,
                mem,
                now_ms: crate::memory::now_ms(),
            };
            if let Some(o) = crate::memory::tools::exec(&env, name, args, label).await {
                return ToolOutcome {
                    result: o.result,
                    diff: o.diff,
                    added: o.added,
                    removed: o.removed,
                    ..Default::default()
                };
            }
        }
        // Outils web (lot 42) : le cache des pages est ici.
        if let Some(result) = self.shared.web.exec(&self.shared.store, name, args).await {
            return ToolOutcome {
                result,
                ..Default::default()
            };
        }
        // Mémoire longue de la conversation (lot 46) : l'archive est au Go,
        // l'id, le score et la mise en forme ici ; Go injoignable : par lui.
        if crate::recall::is_tool(name)
            && let Some(result) = crate::recall::exec(&self.shared.store, name, args).await
        {
            return ToolOutcome {
                result,
                ..Default::default()
            };
        }
        // Outils d'image (lot 45) : see_image dans le dossier de la
        // discussion, web_screenshot par Playwright ; l'image relayée au
        // modèle s'il la voit.
        if name == "see_image" || name == "web_screenshot" {
            let cfg = self.shared.store.config().await;
            let vision = crate::system::vision_enabled(&cfg);
            if name == "see_image" {
                let st = crate::tools::GoToolState {
                    store: self.shared.store.clone(),
                    caps: self.caps.clone(),
                };
                match crate::tools::ToolState::context(&st).await {
                    Ok(ctx) => {
                        let sees = self.shared.vision.sees(&self.shared.store, &cfg);
                        let (result, img) = crate::imagetools::see_image(
                            label,
                            &ctx.cwd,
                            &ctx.target,
                            vision,
                            sees,
                        )
                        .await;
                        return ToolOutcome {
                            result,
                            relay: img.map(|i| see_image_message(label, i)),
                            ..Default::default()
                        };
                    }
                    Err(e) => {
                        eprintln!("[outils] contexte indisponible, see_image par le Go : {e}")
                    }
                }
            } else {
                let home = self.shared.images.home();
                let ws = tokio::task::spawn_blocking(move || {
                    let env = std::env::var(crate::paths::WORKSPACE_ENV).ok();
                    crate::paths::agent_workspace(&home, env.as_deref())
                })
                .await
                .unwrap_or_default();
                let conv_dir = crate::convfiles::conv_dir(&ws, &self.compact.main_conv);
                let internet = self.caps.get("Internet").and_then(Value::as_bool) == Some(true);
                let pw = crate::paths::look_path("playwright").unwrap_or_default();
                let shot = crate::imagetools::web_screenshot(
                    args,
                    &pw,
                    Path::new(&conv_dir),
                    internet,
                    &crate::imagetools::local_stamp(),
                )
                .await;
                let relay = if !shot.rel.is_empty()
                    && vision
                    && self.shared.vision.sees(&self.shared.store, &cfg).await
                {
                    crate::imagetools::screenshot_message(&shot.abs)
                        .and_then(|m| serde_json::from_value::<Message>(m).ok())
                } else {
                    None
                };
                return ToolOutcome {
                    result: shot.result,
                    image: shot.rel,
                    relay,
                    ..Default::default()
                };
            }
        }
        // Navigateur piloté (lot 44) : la session est ici ; aperçu de la page
        // après une action qui la change (jamais après une erreur), capture
        // relayée au modèle.
        if crate::browser::is_tool(name) {
            let cfg = self.shared.store.config().await;
            let vision = crate::system::vision_enabled(&cfg);
            let home = self.shared.images.home();
            let ws = tokio::task::spawn_blocking(move || {
                let env = std::env::var(crate::paths::WORKSPACE_ENV).ok();
                crate::paths::agent_workspace(&home, env.as_deref())
            })
            .await
            .unwrap_or_default();
            if let Some(o) = self.shared.browser.exec(name, args, vision, &ws).await {
                let live_shot =
                    if !o.result.starts_with("[erreur]") && crate::browser::auto_shot_tool(name) {
                        self.shared.browser.auto_shot().await
                    } else {
                        String::new()
                    };
                return ToolOutcome {
                    relay: o.image.map(|img| see_image_message(label, img)),
                    result: o.result,
                    live_shot,
                    ..Default::default()
                };
            }
        }
        // Jobs d'arrière-plan du mode Code (lot 43), dans le dossier de la
        // discussion ; contexte indisponible : le Go s'en charge.
        if name == "bash_tail" {
            return ToolOutcome {
                result: self.shared.jobs.bash_tail(args).await,
                ..Default::default()
            };
        }
        if name == "bash_bg" {
            let st = crate::tools::GoToolState {
                store: self.shared.store.clone(),
                caps: self.caps.clone(),
            };
            match crate::tools::ToolState::context(&st).await {
                Ok(ctx) => {
                    let busy = crate::backends::tune_busy(&self.shared.store).await;
                    let result = self
                        .shared
                        .jobs
                        .bash_bg(&ctx.cwd, &self.compact.main_conv, args, busy)
                        .await;
                    return ToolOutcome {
                        result,
                        ..Default::default()
                    };
                }
                Err(e) => eprintln!("[outils] contexte indisponible, bash_bg par le Go : {e}"),
            }
        }
        // Outils task_*, quand la façade tient les tâches (lot 37).
        if crate::tasks::tools::is_tool(name)
            && let Some(tasks) = &self.shared.tasks
        {
            let dir = tasks.scripts_dir();
            let env = crate::tasks::tools::ToolEnv {
                store: &self.shared.store,
                scripts_dir: &dir,
                now_ns: tasks.now(),
            };
            if let Some(result) = crate::tasks::tools::exec(&env, name, args).await {
                return ToolOutcome {
                    result,
                    ..Default::default()
                };
            }
        }
        if crate::mcp::config::is_mcp_tool(name)
            && let Some(mcp) = &self.shared.mcp
        {
            let version = self.shared.go_version().await;
            return ToolOutcome {
                result: mcp.call(&self.shared.store, name, args, &version).await,
                ..Default::default()
            };
        }
        // Outils fichiers et shell : ici (lot 7), l'état partagé restant au Go.
        if crate::tools::is_local(name) {
            let st = crate::tools::GoToolState {
                store: self.shared.store.clone(),
                caps: self.caps.clone(),
            };
            let code = self
                .caps
                .get("Code")
                .and_then(Value::as_bool)
                .unwrap_or(false);
            if let Some(out) = crate::tools::exec_local(&st, code, name, args, label).await {
                return out;
            }
        }
        match self
            .shared
            .store
            .tool_exec(name, &Value::Object(args.clone()), &self.caps)
            .await
        {
            Ok(v) => tool_outcome(&v, label),
            Err(e) => ToolOutcome {
                result: format!("[erreur] exécution de l'outil impossible : {e}"),
                ..Default::default()
            },
        }
    }
}

impl TurnHost for GoTurnHost {
    async fn send(
        &self,
        ep: &ChatEndpoint,
        body: Vec<u8>,
        keep_prewarm: bool,
    ) -> Result<Reply, SendError> {
        // Moteur local : requête comptée en vol jusqu'à la fin de lecture du
        // flux (le ticket voyage avec le corps). Rien pour une API externe.
        // Un préchauffage qu'elle ne prolonge pas est coupé tout de suite (le
        // Go l'annulerait aussi, un aller-retour plus tard).
        let gate = if ep.external {
            None
        } else {
            if !keep_prewarm {
                self.shared.prewarm.stop();
            }
            self.gate_begin(keep_prewarm).await
        };
        match self.shared.http.post(&ep.url, &ep.key, body).await {
            Ok(resp) => {
                let status = resp.status().as_u16();
                if status == 200
                    && let Some(g) = &gate
                {
                    // Le slot porte désormais cette requête (engineServed,
                    // engineMarkMain pour une étape de la discussion).
                    let _ = self
                        .shared
                        .store
                        .internal_json(
                            hyper::Method::POST,
                            "/internal/v1/oai/served",
                            Some(&json!({ "ticket": g.ticket })),
                        )
                        .await;
                }
                let body = resp.bytes_stream().map(move |r| {
                    let _keep = &gate;
                    r.map_err(|e| e.to_string())
                });
                Ok(Reply {
                    status,
                    body: Box::pin(body),
                })
            }
            Err(e) => {
                let kind = classify(&e);
                Err(SendError {
                    kind,
                    message: friendly_error(kind, &e.to_string(), self.port, ""),
                })
            }
        }
    }

    async fn exec_tool(&self, name: &str, args: &Map<String, Value>, label: &str) -> ToolOutcome {
        // Sous-agent du mode Code (lot 47) : un tour imbriqué, sur un
        // contexte isolé, dont seul le rapport revient.
        if name == "subagent"
            && let Some(sub) = &self.sub
        {
            return ToolOutcome {
                result: super::subagent::run(self, sub, args).await,
                ..Default::default()
            };
        }
        self.exec_tool_base(name, args, label).await
    }

    fn save_tool_result(&self, result: &str) -> String {
        let id = tool_res_id(&self.conv_id);
        self.shared.persister.enqueue_tool_res(ToolResJob {
            key: id.clone(),
            plain: Arc::from(result),
        });
        id
    }

    async fn compact(&self, phase: CompactPhase, msgs: &[Message]) -> Option<Vec<Message>> {
        let recall = StoreRecall(self.shared.store.clone());
        // Mode Code : l'état des critères au moment du résumé (le Go les relit
        // aussi à chaque compaction — l'outil criteria a pu les changer).
        let criteria = if self.compact.caps.code {
            live_criteria(&self.shared.store, &self.compact.main_conv).await
        } else {
            self.compact.criteria.clone()
        };
        let c = Compactor {
            cfg: &self.compact.cfg,
            summarizer: &self.summarizer,
            recall: &recall,
            memory: &self.shared.compact_memory,
            criteria: &criteria,
        };
        let (out, changed) = match phase {
            CompactPhase::Reactive | CompactPhase::FullWindow => {
                c.compact_messages(msgs, self.compact.caps).await
            }
            CompactPhase::Mid { used, rescue } => {
                let conv = self.compact.main_conv.as_str();
                let opt: CompactOpts = compact_opts_mid(
                    &self.compact.cfg,
                    &self.shared.compact_memory,
                    self.summarizer.slot_holds(conv),
                    self.summarizer.ep.external,
                    conv,
                    msgs,
                    used,
                    self.compact.tools.clone(),
                    &self.compact.effort,
                    self.compact.kwargs.clone(),
                    rescue,
                    |m| {
                        wire_messages(m, false, &self.compact.cfg.echo)
                            .0
                            .into_owned()
                    },
                );
                c.compact_messages_noted(msgs, self.compact.caps, &opt)
                    .await
            }
        };
        changed.then_some(out)
    }

    fn compact_refused(&self, used: i64, msgs: &[Message]) -> bool {
        self.shared.compact_memory.refused_skip(
            &self.compact.cfg,
            &self.compact.main_conv,
            used,
            msgs,
        )
    }

    fn drain_queued(&self) -> Vec<Message> {
        (self.queued)()
    }

    fn prefix_unstable(
        &self,
        tools: &[Tool],
        kwargs: Option<&Map<String, Value>>,
        effort: &str,
    ) -> bool {
        let shape = super::tplprobe::Shape::new(tools, kwargs, effort);
        self.shared.tpl.caps_for(false, &shape).is_some_and(|r| {
            r.prefix_stable == super::wire::TplTri::No
                && !r.model.is_empty()
                && r.model == super::wire::reasoning_echo_model(&self.cfg)
        })
    }

    /// Fil principal seulement (`perfMain`) : une tâche ou un sous-agent ne
    /// déclenche pas la sonde.
    fn completed(&self, tools: &[Tool], kwargs: Option<&Map<String, Value>>, effort: &str) {
        if self.compact.main_conv.is_empty() {
            return;
        }
        let shape = super::tplprobe::Shape::new(tools, kwargs, effort);
        let (shared, port) = (self.shared.clone(), self.port);
        tokio::spawn(async move {
            let key = shared.store.get_str(BK_STATE, "api_key").await;
            let env = super::tplprobe::Env {
                external: false,
                port: port.into(),
                key,
            };
            shared.tpl.kick(env, shape);
        });
    }

    async fn prepare_images(&self, msgs: &[Message]) {
        self.shared.images.prefetch(&self.shared.store, msgs).await;
    }

    fn image_data_url(&self, r: &str) -> Option<String> {
        self.shared.images.get(r)
    }

    fn effort_remember(&self, want: &str, got: &str) {
        self.shared.effort.remember(&self.cfg, want, got);
    }

    fn echo_refuse(&self, model: &str) {
        self.shared.echo_refusals.refuse(model);
    }

    async fn project_refresh(&self, msgs: Vec<Message>) -> Vec<Message> {
        let Some(t) = &self.projsnap else {
            return msgs;
        };
        let got = self
            .shared
            .store
            .internal_post(
                "/internal/v1/turn/projsnap/refresh",
                &json!({ "caps": t.caps, "key": t.key, "sys_hash": t.sys_hash }),
            )
            .await;
        let (block, snap) = match got {
            Ok((code, v)) if code.is_success() => (
                serde_json::from_value::<Vec<Message>>(v["block"].clone()).ok(),
                serde_json::from_value::<ProjSnapshot>(v["snap"].clone()).ok(),
            ),
            _ => (None, None),
        };
        // Go injoignable : le bloc figé reste, sans les mises à jour (le début
        // du prompt change de toute façon) ; l'instantané n'est pas remplacé.
        let msgs = super::compact::strip_context_updates(msgs);
        match (block, snap) {
            (Some(block), Some(snap)) => {
                *self
                    .snap_refreshed
                    .lock()
                    .unwrap_or_else(|e| e.into_inner()) = Some(snap);
                super::compact::replace_project_head(msgs, &block)
            }
            _ => msgs,
        }
    }

    fn prewarm_keeps(&self, prefix: &[u64], payload: &Value) -> bool {
        self.perf_kind == super::perf::KIND_MAIN
            && self.shared.prewarm.live()
            && self
                .shared
                .prewarm
                .keeps(prefix, super::prewarm::opts_hash(payload))
    }

    async fn keep_images(&self) -> Option<i64> {
        // Fil principal d'une discussion seulement : la vérification ne
        // persiste pas son historique.
        if !super::keepimg::keep_turn_images_on(&self.cfg)
            || self.perf_kind != super::perf::KIND_MAIN
            || self.conv_id.is_empty()
        {
            return None;
        }
        match keep_images_state(&self.shared.store).await {
            Some((true, budget)) => Some(budget),
            _ => None,
        }
    }

    async fn keep_image(&self, m: &Message) -> Option<(Message, i64)> {
        let (code, v) = self
            .shared
            .store
            .internal_post("/internal/v1/turn/keepimg/store", &json!({ "msg": m }))
            .await
            .ok()?;
        if !code.is_success() || v["ok"] != true {
            return None;
        }
        let msg: Message = serde_json::from_value(v["msg"].clone()).ok()?;
        Some((msg, v["weight"].as_i64().unwrap_or(0)))
    }

    async fn engine_slot(&self, ep: &ChatEndpoint) -> Option<u8> {
        if ep.external {
            self.side_slot.store(false, Ordering::SeqCst);
            return None;
        }
        // Étape d'un tour de la discussion : le slot 0, après rechargement de
        // l'état gardé (SLOT_PERSIST). Vérification : le second slot.
        let main = self.perf_kind == super::perf::KIND_MAIN;
        let conv = if main { self.conv_id.as_str() } else { "" };
        let slot = engine_slot_for(&self.shared.store, conv, main).await;
        self.side_slot.store(slot == Some(1), Ordering::SeqCst);
        slot
    }

    fn cache_hint(&self) -> Option<CacheHintFn> {
        None
    }

    async fn perf_record(&self, rec: PerfRec) -> PerfMarks {
        super::perf::record(
            &self.shared.store,
            &self.shared.perf,
            self.perf_kind,
            &self.conv_id,
            &rec,
        )
        .await
    }

    fn preflight(&self) -> Option<PreflightFn> {
        None
    }
}

/// Les critères du mode Code rendus pour le résumé ("" : aucun).
pub(crate) async fn live_criteria(store: &crate::store::Store, conv_id: &str) -> String {
    super::verify::crit_render(&super::verify::crit_list(store, conv_id).await)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn base64_standard() {
        assert_eq!(b64(b""), "");
        assert_eq!(b64(b"f"), "Zg==");
        assert_eq!(b64(b"fo"), "Zm8=");
        assert_eq!(b64(b"foo"), "Zm9v");
        assert_eq!(b64(b"foobar"), "Zm9vYmFy");
        assert_eq!(b64(&[0xff, 0xfe, 0x00]), "//4A");
    }

    #[test]
    fn references_d_images() {
        assert_eq!(image_name("loki-img:ab.png"), Some("ab.png"));
        for bad in [
            "loki-img:",
            "loki-img:../x",
            "loki-img:a/b",
            "loki-img:a\\b",
            "data:x",
            "ab.png",
        ] {
            assert_eq!(image_name(bad), None, "{bad}");
        }
        assert_eq!(image_mime("a.png"), "image/png");
        assert_eq!(image_mime("a.webp"), "image/webp");
        assert_eq!(image_mime("a.jpg"), "image/jpeg");
        assert_eq!(image_mime("a.img"), "image/jpeg");
    }

    #[test]
    fn cache_d_images_borne() {
        let c = ImageCache::new(PathBuf::from("/nulle-part"));
        let big = "x".repeat(IMG_CACHE_MAX / 2 + 1);
        c.put("a.png", big.clone());
        c.put("b.png", big.clone());
        assert_eq!(c.get("loki-img:a.png"), None, "la plus ancienne part");
        assert_eq!(c.get("loki-img:b.png").map(|u| u.len()), Some(big.len()));
        // Une seule image, même trop grosse, reste.
        let c = ImageCache::new(PathBuf::from("/nulle-part"));
        c.put("g.png", "y".repeat(IMG_CACHE_MAX + 1));
        assert!(c.get("loki-img:g.png").is_some());
    }

    #[test]
    fn reponse_de_l_api_d_outils() {
        let v = json!({
            "label": "ls", "result": "a\nb",
            "diff": [{"op": "+", "text": "x"}], "added": 1, "removed": 2,
            "ask": {"question": "On continue ?", "options": ["oui"]},
            "image": "captures/a.png", "shot": "s1",
            "relay": {"role": "user", "content": [{"type": "text", "text": "Voici la capture demandée."}]}
        });
        let o = tool_outcome(&v, "ls");
        assert_eq!(o.result, "a\nb");
        assert_eq!((o.added, o.removed, o.diff.len()), (1, 2, 1));
        assert_eq!(o.ask.unwrap().question, "On continue ?");
        assert_eq!(
            (o.image.as_str(), o.live_shot.as_str()),
            ("captures/a.png", "s1")
        );
        assert_eq!(o.relay.unwrap().role, "user");
        // vision_img seul : le message relayé est bâti ici (seeImageMessage).
        let img = json!({"type": "image_url", "image_url": {"url": "data:image/png;base64,AA=="}});
        let o = tool_outcome(&json!({"result": "ok", "vision_img": img}), "photo.png");
        assert_eq!(
            o.relay.unwrap().content,
            Some(json!([{"type": "text", "text": "Image demandée (photo.png) :"}, img]))
        );
        assert_eq!(
            see_image_message("", json!({})).content.unwrap()[0]["text"],
            "Image :"
        );
        assert_eq!(tool_outcome(&json!({}), "").result, "");
    }

    #[test]
    fn identifiant_d_un_resultat_complet() {
        let id = tool_res_id("c42");
        let (conv, rand) = id.split_once('.').unwrap();
        assert_eq!((conv, rand.len()), ("c42", 24));
        assert!(tool_res_id(" ").starts_with("nosession."));
        assert_ne!(tool_res_id("c"), tool_res_id("c"));
    }
}
