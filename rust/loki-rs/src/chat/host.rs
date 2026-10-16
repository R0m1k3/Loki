//! Le tour branché sur le vrai monde : le [`TurnHost`] de la façade.
//!
//! - envoi au moteur par [`EngineHttp`] (clé de CET endpoint), pannes
//!   classées et formulées comme `friendlyLLMError` ;
//! - outils fichiers et shell exécutés ici ([`crate::tools`], état partagé
//!   tenu par le Go) ; les autres par le Go (`/internal/v1/tool/exec`), avec
//!   ce qu'il tire du résultat (capture de la bulle, aperçu du navigateur,
//!   image relayée) ;
//! - résultats complets (« voir plus ») confiés à l'écrivain des discussions ;
//! - compaction par [`Compactor`] : résumé demandé au moteur
//!   ([`EngineSummarizer`]), blocs archivés pour recall par le Go
//!   ([`StoreRecall`]) ;
//! - images jointes (`loki-img:…`) lues sur disque et ouvertes par le Go
//!   (déchiffrées), gardées en cache comme `chatImageDataURL`.
//!
//! Écarts assumés : le résumé en continuation (COMPACT_CONTINUATION) demande
//! l'état du slot du moteur, tenu par le Go — `slot_holds` rend faux et la
//! compaction résume la transcription. Ni conseil de cache ni garde
//! d'écriture du mode Code (état du Go) : `cache_hint` et `preflight` absents.

use std::collections::{HashMap, VecDeque};
use std::path::PathBuf;
use std::sync::{Arc, Mutex};

use futures_util::StreamExt;
use serde_json::{Map, Value, json};

use super::compact::{
    CompactCaps, CompactConfig, CompactMemory, CompactOpts, Compactor, ContinuationRequest,
    RecallArchive, Summarizer, SummaryChoice, SummaryError, TranscriptRequest, compact_opts_mid,
    parse_summary_response,
};
use super::engine::{EngineHttp, classify, friendly_error};
use super::persist::{Persister, StoreSink, ToolResJob};
use super::stream::{CacheHintFn, PreflightFn, RetryPatterns};
use super::turn::{CompactPhase, Reply, SendError, ToolOutcome, TurnHost};
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
    /// Télémétrie des résumés (kind=compact), rattachée à cette discussion :
    /// `None` sans base (tests).
    pub perf: Option<(Store, String)>,
}

impl EngineSummarizer {
    async fn post(&self, payload: Value) -> Result<SummaryChoice, SummaryError> {
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
        if decoded && let Some((store, conv)) = &self.perf {
            let rec = super::perf::PerfRec::from_wire(super::perf::KIND_COMPACT, conv, &raw);
            super::perf::record(store, &rec, &[], self.ep.external).await;
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
        self.post(p).await
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
        self.post(p).await
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

impl TurnHost for GoTurnHost {
    async fn send(&self, ep: &ChatEndpoint, body: Vec<u8>) -> Result<Reply, SendError> {
        match self.shared.http.post(&ep.url, &ep.key, body).await {
            Ok(resp) => {
                let status = resp.status().as_u16();
                let body = resp.bytes_stream().map(|r| r.map_err(|e| e.to_string()));
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

    async fn perf(
        &self,
        rec: &super::perf::PerfRec,
        prefix: &[u64],
        external: bool,
    ) -> super::perf::PerfNoted {
        let mut rec = rec.clone();
        rec.conv = self.conv_id.clone();
        super::perf::record(&self.shared.store, &rec, prefix, external).await
    }

    async fn exec_tool(&self, name: &str, args: &Map<String, Value>, label: &str) -> ToolOutcome {
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

    fn cache_hint(&self) -> Option<CacheHintFn> {
        None
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
