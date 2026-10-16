//! PREWARM tenu par la façade (lot 33) — port de `chat_prewarm.go`.
//!
//! Préparer le prochain tour pendant que l'utilisateur lit. Certains
//! recalculs sont inévitables (prompt réécrit par une compaction, dernier
//! message assistant rendu autrement, discussion reprise après une tâche qui
//! a pris le slot) ; sans la clé, ils tombent sur le premier jeton du message
//! suivant.
//!
//! Avec la clé, la façade envoie au moteur local, dès qu'il est libre, la
//! requête du prochain tour telle qu'elle partira — assemblée par les MÊMES
//! fonctions que le tour — suivie d'un message utilisateur sentinelle « . »,
//! avec `max_tokens` 1. Le moteur calcule tout le préfixe ; le vrai tour ne
//! calcule plus que son propre message. Rien n'est persisté.
//!
//! Le Go garde ce qui décide si le préchauffage peut partir et ce qui
//! l'annule (`facade_prewarm.go`) : il l'inscrit comme LA requête en vol, et
//! toute requête de Loki qui part ensuite l'annule — sauf un tour de la
//! façade dont la requête le PROLONGE exactement (empreintes des messages,
//! des outils et des arguments du gabarit), qui le suit dans le même slot.
//! Aucune erreur ne remonte.

use std::collections::{HashMap, HashSet};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use serde_json::{Value, json};
use tokio::sync::watch;

use super::compact::{compact_needed, ctx_pending, strip_context_updates};
use super::generate::ChatService;
use super::history::inject_prefix;
use super::perf::{self, wire_stats};
use super::turn::{PerfRec, perf_prefix};
use super::types::Message;
use super::wire::{
    ChatPayloadOpts, build_chat_payload, turn_echo_policy, turn_reasoning, wire_messages,
};
use crate::engstatus::is_external;

/// Le message utilisateur qui tient la place du vrai.
pub const SENTINEL: &str = ".";

/// Déclencheurs (pour le journal).
pub const TURN_END: &str = "fin-tour";
pub const AFTER_TASK: &str = "après-tâche";
pub const SWITCH: &str = "discussion";

/// PREWARM=full : préparer la discussion ouverte si l'utilisateur y reste.
pub const SWITCH_DELAY: Duration = Duration::from_secs(3);
/// Borne d'un préchauffage (un MoE aux experts en RAM met des minutes sur 20k
/// jetons). Toute vraie requête l'annule bien avant.
pub const TIMEOUT: Duration = Duration::from_secs(15 * 60);
/// Attente de la fin d'un préchauffage remplacé.
const STOP_WAIT: Duration = Duration::from_secs(5);
/// Capacités gardées, une par discussion.
const CAPS_MAX: usize = 64;

/// `prewarmMode` : « on », « full » ou "" (off). Jamais pour un preset externe.
pub fn prewarm_mode(cfg: &HashMap<String, String>) -> &'static str {
    if is_external(cfg) {
        return "";
    }
    match cfg
        .get("PREWARM")
        .map(|v| v.trim().to_lowercase())
        .as_deref()
    {
        Some("on" | "1" | "true" | "yes" | "oui") => "on",
        Some("full") => "full",
        _ => "",
    }
}

/// `prewarmOptsKeys` : les champs du corps, hors messages, qui changent le
/// prompt rendu par le gabarit (l'échantillonnage n'y touche pas).
const OPTS_KEYS: [&str; 5] = [
    "tools",
    "parallel_tool_calls",
    "tool_choice",
    "chat_template_kwargs",
    "reasoning_effort",
];

/// `prewarmOptsHash` : FNV-1a 64 de ces champs. Comparée seulement aux
/// empreintes calculées ici, d'un corps à l'autre.
pub fn opts_hash(payload: &Value) -> u64 {
    const OFFSET: u64 = 0xcbf2_9ce4_8422_2325;
    const PRIME: u64 = 0x0000_0100_0000_01b3;
    let mut h = OFFSET;
    let mut eat = |b: &[u8]| {
        for &c in b {
            h = (h ^ u64::from(c)).wrapping_mul(PRIME);
        }
    };
    for k in OPTS_KEYS {
        eat(k.as_bytes());
        eat(&[0]);
        if let Some(v) = payload.get(k) {
            eat(&serde_json::to_vec(v).unwrap_or_default());
        }
        eat(&[0]);
    }
    h
}

/// `withTurnRole(caps, ".")` : en mode Code, le rôle d'un message ordinaire.
pub fn sentinel_caps(caps: &Value) -> Value {
    let mut c = caps.clone();
    let code = c.get("Code").and_then(Value::as_bool).unwrap_or(false);
    let role_empty = c
        .get("Role")
        .and_then(Value::as_str)
        .unwrap_or("")
        .is_empty();
    if code
        && role_empty
        && let Some(o) = c.as_object_mut()
    {
        o.insert("Role".into(), Value::from("builder"));
    }
    c
}

/// Un préchauffage inscrit au Go : `prefix` couvre ses `n` premiers messages
/// (sans la sentinelle), `opts` les champs qui façonnent le rendu.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Run {
    pub id: u64,
    pub n: usize,
    pub prefix: u64,
    pub opts: u64,
}

impl Run {
    /// `matches` : la requête dont `prefix` est l'empreinte cumulée des
    /// messages prolonge-t-elle exactement ce préchauffage ?
    pub fn matches(&self, prefix: &[u64], opts: u64) -> bool {
        self.n > 0
            && prefix.len() > self.n
            && prefix[self.n - 1] == self.prefix
            && opts == self.opts
    }
}

struct Current {
    /// Rempli une fois inscrit au Go.
    run: Option<Run>,
    cancel: watch::Sender<bool>,
    done: watch::Receiver<bool>,
}

/// L'état partagé du préchauffage (un exemplaire par processus).
#[derive(Default)]
pub struct Prewarm {
    cur: Mutex<Option<Current>>,
    /// Capacités du dernier tour de chaque discussion, en mémoire seulement.
    caps: Mutex<HashMap<String, Value>>,
    /// Raisons d'échec déjà journalisées.
    logged: Mutex<HashSet<String>>,
}

impl Prewarm {
    fn cur(&self) -> std::sync::MutexGuard<'_, Option<Current>> {
        self.cur.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Un préchauffage est-il en vol (ou en train de partir) ?
    pub fn live(&self) -> bool {
        self.cur().is_some()
    }

    /// La requête d'un tour le prolonge-t-elle exactement ?
    pub fn keeps(&self, prefix: &[u64], opts: u64) -> bool {
        self.cur()
            .as_ref()
            .and_then(|c| c.run)
            .is_some_and(|r| r.matches(prefix, opts))
    }

    /// `prewarmStop` : annule le préchauffage en vol et rend de quoi attendre
    /// sa fin (`None` : rien en vol).
    pub fn stop(&self) -> Option<watch::Receiver<bool>> {
        let c = self.cur().take()?;
        let _ = c.cancel.send(true);
        Some(c.done)
    }

    /// Annulation relayée par le Go (une autre requête est partie). Un
    /// préchauffage pas encore inscrit est annulé aussi : il n'y en a qu'un.
    pub fn cancel_id(&self, id: u64) -> bool {
        let g = self.cur();
        match g.as_ref() {
            Some(c) if c.run.is_none_or(|r| r.id == id) => {
                let _ = c.cancel.send(true);
                true
            }
            _ => false,
        }
    }

    /// `prewarmNoteCaps`.
    pub fn note_caps(&self, conv: &str, caps: Value) {
        if conv.is_empty() {
            return;
        }
        let mut m = self.caps.lock().unwrap_or_else(|e| e.into_inner());
        if !m.contains_key(conv)
            && m.len() >= CAPS_MAX
            && let Some(k) = m.keys().next().cloned()
        {
            m.remove(&k);
        }
        m.insert(conv.to_string(), caps);
    }

    pub fn caps_for(&self, conv: &str) -> Option<Value> {
        self.caps
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .get(conv)
            .cloned()
    }

    fn log_once(&self, key: &str, msg: &str) {
        if self
            .logged
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .insert(key.to_string())
        {
            eprintln!("[prewarm] {msg}");
        }
    }

    /// Inscrit le préchauffage qui part ; `None` si un autre est déjà là.
    fn register(&self) -> Option<(watch::Receiver<bool>, watch::Sender<bool>)> {
        let mut g = self.cur();
        if g.is_some() {
            return None;
        }
        let (cancel, cancelled) = watch::channel(false);
        let (done_tx, done) = watch::channel(false);
        *g = Some(Current {
            run: None,
            cancel,
            done,
        });
        Some((cancelled, done_tx))
    }

    fn set_run(&self, run: Run) {
        if let Some(c) = self.cur().as_mut() {
            c.run = Some(run);
        }
    }

    /// Fin du préchauffage `run` (ou de celui qui partait) : la place est
    /// rendue s'il est toujours le courant.
    fn release(&self, id: Option<u64>) {
        let mut g = self.cur();
        if g.as_ref().is_some_and(|c| c.run.map(|r| r.id) == id) {
            *g = None;
        }
    }
}

/// Ce que l'assemblage rend : le corps, et l'empreinte du préfixe préchauffé.
#[derive(Debug, Clone, PartialEq)]
pub struct Body {
    pub payload: Value,
    pub n: usize,
    pub prefix: u64,
    pub opts: u64,
}

impl ChatService {
    /// `prewarmKick` : un déclencheur. Sans la clé, rien — ni tâche, ni
    /// requête.
    pub async fn prewarm_kick(self: &Arc<Self>, why: &'static str) {
        let cfg = self.shared.store.config().await;
        if prewarm_mode(&cfg).is_empty() {
            return;
        }
        let me = self.clone();
        tokio::spawn(async move { me.prewarm(why).await });
    }

    /// `prewarmAfterSwitch` : changement de discussion. Le préchauffage de
    /// l'ancienne est annulé ; avec PREWARM=full, la nouvelle est préparée si
    /// l'utilisateur y reste 3 s.
    pub async fn prewarm_after_switch(self: &Arc<Self>) {
        self.shared.prewarm.stop(); // sa fin n'est pas attendue
        let cfg = self.shared.store.config().await;
        if prewarm_mode(&cfg) != "full" {
            return;
        }
        let id = self.sessions.conv().id();
        let me = self.clone();
        tokio::spawn(async move {
            tokio::time::sleep(SWITCH_DELAY).await;
            if me.sessions.conv().id() == id {
                me.prewarm_kick(SWITCH).await;
            }
        });
    }

    /// `prewarmBodyFor` : le corps du préchauffage de la discussion ouverte,
    /// ou la raison de s'abstenir. Lecture seule.
    pub async fn prewarm_body(&self) -> Result<Body, &'static str> {
        let env = self.turn_env().await;
        let conv_id = self.sessions.conv().id();
        let caps = self
            .shared
            .prewarm
            .caps_for(&conv_id)
            .ok_or("capacités du dernier tour inconnues")?;
        let turn_caps = sentinel_caps(&caps);
        let conv = self.sessions.conv();
        if conv.is_generating() {
            return Err("moteur occupé");
        }
        let (mut msgs, ctx_now, peak, snap) = conv.with_state(|st| {
            let msgs = st.messages.clone();
            let used = ctx_pending(st.ctx_used, st.ctx_used_len, &msgs, &env.echo);
            (msgs, used, st.gen_peak, st.proj_snap.clone())
        });
        // Sans message utilisateur, le contexte projet irait en tête de la
        // sentinelle : rien à préparer.
        if !msgs.iter().any(|m| m.role == "user") {
            return Err("discussion vide");
        }
        msgs.push(Message {
            role: "user".into(),
            content: Some(Value::from(SENTINEL)),
            ..Default::default()
        });
        // Le prochain tour compactera d'abord : son prompt ne sera pas celui-ci.
        if compact_needed(&env.ccfg, &msgs, ctx_now, peak) {
            return Err("compaction attendue au prochain tour");
        }
        // Mêmes décisions que le vrai tour (PROJ_SNAPSHOT compris), sans rien
        // ranger : bloc figé gardé avec ses mises à jour, ou rafraîchi.
        let v = self
            .turn_view(&turn_caps, snap.as_ref(), true)
            .await
            .map_err(|_| "vue du tour refusée")?;
        let tools = v.tools;
        let hist = match &v.snap {
            Some(d) if !d.refresh => msgs,
            _ => strip_context_updates(msgs),
        };
        let mut sent = v.head;
        sent.extend(hist);
        let sent = inject_prefix(sent, &v.prefix);
        let reasoning = turn_reasoning(&env.cfg, &self.shared.effort);
        let pol = turn_echo_policy(false, || env.echo.clone());
        self.shared.images.prefetch(&self.shared.store, &sent).await;
        let (wire, _) = wire_messages(&sent, false, &pol);
        let n = wire.len();
        let last_ok = wire.last().is_some_and(|m| {
            m.role == "user" && m.content.as_ref().and_then(Value::as_str) == Some(SENTINEL)
        });
        if n < 2 || !last_ok {
            return Err("assemblage inattendu");
        }
        let mut payload = build_chat_payload(
            &env.ep,
            &wire,
            0.7,
            &ChatPayloadOpts {
                tools: &tools,
                disable_tools: false,
                tool_choice_none: false,
                effort: &reasoning.effort,
                kwargs: reasoning.kwargs.as_ref(),
                no_sampling: false,
            },
            &env.cfg,
            &|r| self.shared.images.get(r),
        );
        // Seuls les champs de réponse changent : un seul jeton, non streamé.
        if let Some(o) = payload.as_object_mut() {
            o.insert("stream".into(), Value::Bool(false));
            o.remove("stream_options");
            o.insert("max_tokens".into(), json!(1));
        }
        let opts = opts_hash(&payload);
        Ok(Body {
            payload,
            n: n - 1,
            prefix: perf_prefix(&wire)[n - 2],
            opts,
        })
    }

    /// `prewarm` : un préchauffage complet, ou rien. Jamais d'erreur rendue.
    pub async fn prewarm(self: &Arc<Self>, why: &str) {
        let cfg = self.shared.store.config().await;
        if prewarm_mode(&cfg).is_empty() {
            return;
        }
        let env = self.turn_env().await;
        if env.ep.external {
            return;
        }
        // Un seul en vol : le nouveau déclencheur remplace l'ancien.
        if let Some(mut done) = self.shared.prewarm.stop() {
            let ended = tokio::time::timeout(STOP_WAIT, done.wait_for(|d| *d)).await;
            if !matches!(ended, Ok(Ok(_))) {
                return;
            }
        }
        let store = &self.shared.store;
        let slot = match store
            .internal_post("/internal/v1/prewarm/check", &json!({}))
            .await
        {
            Ok((code, v)) if code.is_success() && v["ok"] == true => {
                v["slot"].as_i64().unwrap_or(-1)
            }
            _ => return,
        };
        let Ok(mut body) = self.prewarm_body().await else {
            return;
        };
        if slot >= 0
            && let Some(o) = body.payload.as_object_mut()
        {
            o.insert("id_slot".into(), json!(slot));
        }
        let Ok(raw_body) = serde_json::to_vec(&body.payload) else {
            return;
        };
        let Some((mut cancelled, done)) = self.shared.prewarm.register() else {
            return;
        };
        let conv_id = self.sessions.conv().id();
        let begun = store
            .internal_post(
                "/internal/v1/prewarm/begin",
                &json!({ "conv": conv_id, "n": body.n, "prefix": body.prefix, "opts": body.opts }),
            )
            .await;
        let id = match begun {
            Ok((code, v)) if code.is_success() && v["ok"] == true => v["id"].as_u64(),
            _ => None,
        };
        let Some(id) = id else {
            // Une requête de Loki est partie entre-temps.
            self.shared.prewarm.release(None);
            let _ = done.send(true);
            return;
        };
        self.shared.prewarm.set_run(Run {
            id,
            n: body.n,
            prefix: body.prefix,
            opts: body.opts,
        });
        let (mut total, mut cached) = (0i64, None::<i64>);
        if !*cancelled.borrow() {
            let send = async {
                let resp = self
                    .shared
                    .http
                    .post(&env.ep.url, &env.ep.key, raw_body)
                    .await;
                match resp {
                    Err(e) => Err(format!("moteur injoignable ({why}) : {e}")),
                    Ok(r) => {
                        let status = r.status();
                        let raw = r.bytes().await.map_err(|e| e.to_string())?;
                        let raw = &raw[..raw.len().min(1 << 20)];
                        Ok((status, raw.to_vec()))
                    }
                }
            };
            let out: Option<Result<(reqwest::StatusCode, Vec<u8>), String>> = tokio::select! {
                o = tokio::time::timeout(TIMEOUT, send) => o.ok(),
                _ = cancelled.wait_for(|c| *c) => None,
            };
            match out {
                Some(Err(e)) => self.shared.prewarm.log_once("net", &e),
                Some(Ok((status, raw))) if status.as_u16() != 200 => {
                    let txt = String::from_utf8_lossy(&raw[..raw.len().min(200)])
                        .trim()
                        .to_string();
                    self.shared.prewarm.log_once(
                        status.as_str(),
                        &format!("refusé par le moteur ({why}) : {status} {txt}"),
                    );
                }
                Some(Ok((_, raw))) => {
                    // Télémétrie seulement (kind=prewarm) : la réponse est jetée.
                    let (complete, stats) = wire_stats(&raw);
                    total = stats.prompt_tokens_total;
                    cached = stats.cache_tokens;
                    let rec = PerfRec {
                        complete,
                        stats,
                        ..Default::default()
                    };
                    perf::record(store, &self.shared.perf, perf::KIND_PREWARM, &conv_id, &rec)
                        .await;
                }
                None => {}
            }
        }
        let _ = store
            .internal_post(
                "/internal/v1/prewarm/end",
                &json!({ "id": id, "total": total, "cached": cached }),
            )
            .await;
        self.shared.prewarm.release(Some(id));
        let _ = done.send(true);
    }
}

#[cfg(test)]
mod tests;
