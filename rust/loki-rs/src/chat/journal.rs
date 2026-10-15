//! Journal d'affichage du chat — port PUR de `chat_conversation.go` et des
//! trames SSE de `web_chat.go`.
//!
//! L'interface ne voit la discussion qu'à travers ce journal : une suite de
//! [`LogEvent`] (un delta SSE + un Seq monotone + un horodatage serveur en ms),
//! ajoutés par `appendDelta`, coalescés en fin de tour (`compactLog`) et
//! rejoués au rafraîchissement de la page (`subscribeSink`).
//!
//! Ce module ne contient QUE la logique déterministe : aucune goroutine, aucun
//! verrou, aucune base, aucun chiffrement. Ce que le Go lisait dans un état
//! global (discussion active `ckActive`, `c.CtxUsed`, horloge) arrive ici en
//! paramètre explicite, pour que l'acteur de conversation se bâtisse dessus :
//!
//! - ajout au journal : [`append_delta`] (plafond [`MAX_LOG_EVENTS`], [`cap_log`]) ;
//! - compaction de fin de tour : [`compact_log`] / [`compact_log_owned`] ;
//! - rejeu à l'abonnement : [`replay_plan`] (padding, reset, pagination,
//!   rejeu coalescé, caught_up), bâti sur [`coalesce_replay`], [`history_cut`],
//!   [`history_head`] ;
//! - direct : [`live_batch`] (dichotomie [`first_after`] + [`decorate_event`])
//!   et [`live_reset_plan`] (reset survenu pendant l'abonnement) ;
//! - trames SSE : [`sse_payload`], [`sse_data`], [`sse_data_frame`],
//!   [`SSE_HEARTBEAT`], et le regroupement [`SseBatcher`].
//!
//! Fidélité JSON : les deltas sont des `map[string]any` en Go ; l'UI lit les
//! clés par nom. Les valeurs (nombre / chaîne, présence / absence d'une clé)
//! sont reproduites à l'identique. Les nombres posés par ce module (`toks`,
//! `ts0`, `seq0`, `seq`, `ts`, `history_more`, `ctx_used`) sont des entiers.

use std::time::Duration;

use serde::Serialize;
use serde_json::{Map, Value};

use super::types::LogEvent;

/// Un delta d'affichage (`map[string]any` en Go).
pub type Delta = Map<String, Value>;

/// Plafond du journal d'AFFICHAGE (pas de la vue modèle), `maxLogEvents` en
/// Go. La coalescence de fin de tour réduit un tour terminé à une poignée
/// d'événements ; la marge sert à absorber UN tour en cours, streamé token par
/// token, sans en tronquer le début avant sa compaction.
pub const MAX_LOG_EVENTS: usize = 200_000;

/// Échanges rejoués à l'ouverture d'une session (reset) pour un client qui
/// gère la pagination de l'historique (`historyTailDefault`).
pub const HISTORY_TAIL_DEFAULT: i64 = 20;

/// Padding d'amorçage anti-buffering envoyé en tout premier : 2048 fois « · »
/// (U+00B7, 2 octets en UTF-8). Force un proxy (Cloudflare) à passer en mode
/// streaming tout de suite. Le client ignore la clé `pad`.
pub const PAD_OPEN_REPEAT: usize = 2048;

/// Padding envoyé APRÈS caught_up : 16384 fois « · ». Ce sont ses octets, et
/// non le dernier vrai message, qui restent coincés dans le tampon du proxy.
pub const PAD_CAUGHT_UP_REPEAT: usize = 16384;

/// Battement de cœur SSE : un commentaire, ignoré par le parseur navigateur,
/// sans contenu (donc rien à chiffrer).
pub const SSE_HEARTBEAT: &[u8] = b": ping\n\n";

/// Période du battement de cœur (4 s : borne le temps qu'un dernier bout de
/// flux reste coincé dans un tampon de proxy).
pub const SSE_HEARTBEAT_INTERVAL: Duration = Duration::from_secs(4);

/// Taille d'un envoi groupé en octets (`sseBatchBytes`, 128 Kio).
pub const SSE_BATCH_BYTES: usize = 128 << 10;

/// Taille d'un envoi groupé en événements (`sseBatchEvents`).
pub const SSE_BATCH_EVENTS: usize = 256;

// ---------------------------------------------------------------------------
// Lecture des champs d'un delta texte
// ---------------------------------------------------------------------------

/// Lit un nombre JSON comme entier (troncature, comme `int(float64)` en Go).
fn num_i64(v: Option<&Value>) -> Option<i64> {
    match v {
        Some(Value::Number(n)) => n.as_i64().or_else(|| n.as_f64().map(|f| f as i64)),
        _ => None,
    }
}

/// `evToks` : nombre de tokens porté par un événement texte — 1 pour un delta
/// brut (streaming), ou la valeur `toks` accumulée d'un événement coalescé.
pub fn ev_toks(d: &Delta) -> i64 {
    num_i64(d.get("toks")).unwrap_or(1)
}

/// `evTS0` : horodatage de DÉBUT d'un événement texte (présent seulement sur
/// les événements coalescés), sinon `fallback` (le TS de l'événement).
pub fn ev_ts0(d: &Delta, fallback: i64) -> i64 {
    num_i64(d.get("ts0")).unwrap_or(fallback)
}

/// `evSeq0` : seq de DÉBUT d'un bloc texte — sur un événement coalescé (dont
/// le Seq est celui du DERNIER delta fusionné), le seq du PREMIER delta ; sur
/// un delta brut, son propre Seq. Sert à poser `replace`.
pub fn ev_seq0(d: &Delta, fallback: i64) -> i64 {
    num_i64(d.get("seq0")).unwrap_or(fallback)
}

/// Clé texte d'un delta : `content` (chaîne) d'abord, puis
/// `reasoning_content` (chaîne).
fn text_key(d: &Delta) -> Option<&'static str> {
    if matches!(d.get("content"), Some(Value::String(_))) {
        Some("content")
    } else if matches!(d.get("reasoning_content"), Some(Value::String(_))) {
        Some("reasoning_content")
    } else {
        None
    }
}

/// `isTextDelta` : événement porteur de texte (content / reasoning_content).
pub fn is_text_delta(d: &Delta) -> bool {
    text_key(d).is_some()
}

fn text_of<'a>(d: &'a Delta, key: &str) -> &'a str {
    match d.get(key) {
        Some(Value::String(s)) => s,
        _ => "",
    }
}

/// `tool_used` est un objet : (est un outil, done == true).
fn tool_state(d: &Delta) -> Option<bool> {
    match d.get("tool_used") {
        Some(Value::Object(tu)) => Some(tu.get("done") == Some(&Value::Bool(true))),
        _ => None,
    }
}

fn obj1(k: &str, v: Value) -> Delta {
    let mut m = Delta::new();
    m.insert(k.to_owned(), v);
    m
}

/// `{"pad": "·"×n}` — événement de padding ignoré par le client.
pub fn pad_event(n: usize) -> Delta {
    obj1("pad", Value::String("·".repeat(n)))
}

// ---------------------------------------------------------------------------
// Ajout au journal
// ---------------------------------------------------------------------------

/// Partie pure de `appendDelta` : incrémente `seq`, ajoute l'événement
/// horodaté `ts` (ms, `time.Now().UnixMilli()` en Go) et applique le plafond
/// [`MAX_LOG_EVENTS`]. Rend le Seq attribué.
///
/// Le contrôle d'epoch (delta d'un tour périmé jeté après un reset) et le
/// réveil des abonnés restent à l'acteur.
pub fn append_delta(log: &mut Vec<LogEvent>, seq: &mut i64, ts: i64, delta: Delta) -> i64 {
    *seq += 1;
    log.push(LogEvent {
        seq: *seq,
        ts,
        delta,
    });
    cap_log(log, MAX_LOG_EVENTS);
    *seq
}

/// Troncature du journal à ses `max` DERNIERS événements (les plus vieux
/// partent). En Go, `Log[len-max:]` est O(1) ; ici le `drain` décale le
/// vecteur — coût payé seulement quand le plafond est atteint.
pub fn cap_log(log: &mut Vec<LogEvent>, max: usize) {
    if log.len() > max {
        let extra = log.len() - max;
        log.drain(..extra);
    }
}

/// Premier indice dont le Seq dépasse `last` (le journal est trié par Seq
/// croissant) : la dichotomie du direct (`sort.Search` en Go), au lieu d'un
/// balayage complet à chaque token.
pub fn first_after(log: &[LogEvent], last: i64) -> usize {
    log.partition_point(|e| e.seq <= last)
}

// ---------------------------------------------------------------------------
// Compaction de fin de tour
// ---------------------------------------------------------------------------

/// Bloc texte en cours de fusion.
struct TextAcc {
    key: &'static str,
    buf: String,
    seq: i64,
    ts: i64,
    toks: i64,
    ts0: i64,
    seq0: i64,
}

impl TextAcc {
    fn start(key: &'static str, ev: &LogEvent) -> Self {
        TextAcc {
            key,
            buf: String::new(),
            seq: ev.seq,
            ts: ev.ts,
            toks: 0,
            ts0: ev_ts0(&ev.delta, ev.ts),
            seq0: ev_seq0(&ev.delta, ev.seq),
        }
    }

    fn add(&mut self, ev: &LogEvent) {
        self.buf.push_str(text_of(&ev.delta, self.key));
        self.seq = ev.seq;
        self.ts = ev.ts;
        self.toks += ev_toks(&ev.delta);
    }
}

/// `compactLog` sur un journal possédé (pas de copie des événements non
/// fusionnés) : la forme de `compactLogLocked`.
///
/// Coalesce les suites d'événements content / reasoning_content en UN
/// événement chacune, `{clé: texte, toks: somme, ts0: premier, seq0: premier}`
/// avec le Seq/TS du DERNIER delta fusionné. Des `tool_used`, seul le
/// `done=true` reste (état final) ; on lui retire l'aperçu navigateur `shot`
/// (le delta devient alors `{tool_used}` seul). Un journal de moins de deux
/// événements est rendu tel quel.
pub fn compact_log_owned(log: Vec<LogEvent>) -> Vec<LogEvent> {
    if log.len() < 2 {
        return log;
    }
    let mut out = Vec::with_capacity(log.len());
    let mut acc: Option<TextAcc> = None;
    let flush = |acc: &mut Option<TextAcc>, out: &mut Vec<LogEvent>| {
        if let Some(a) = acc.take() {
            let mut d = Delta::new();
            d.insert(a.key.to_owned(), Value::String(a.buf));
            d.insert("toks".to_owned(), a.toks.into());
            d.insert("ts0".to_owned(), a.ts0.into());
            d.insert("seq0".to_owned(), a.seq0.into());
            out.push(LogEvent {
                seq: a.seq,
                ts: a.ts,
                delta: d,
            });
        }
    };
    for mut ev in log {
        if let Some(done) = tool_state(&ev.delta) {
            flush(&mut acc, &mut out);
            if done {
                let has_shot = matches!(ev.delta.get("tool_used"),
                    Some(Value::Object(tu)) if tu.contains_key("shot"));
                if has_shot && let Some(Value::Object(mut cp)) = ev.delta.remove("tool_used") {
                    cp.remove("shot");
                    ev.delta = obj1("tool_used", Value::Object(cp));
                }
                out.push(ev);
            }
            continue;
        }
        let Some(key) = text_key(&ev.delta) else {
            flush(&mut acc, &mut out);
            out.push(ev);
            continue;
        };
        if acc.as_ref().is_some_and(|a| a.key != key) {
            flush(&mut acc, &mut out);
        }
        acc.get_or_insert_with(|| TextAcc::start(key, &ev)).add(&ev);
    }
    flush(&mut acc, &mut out);
    out
}

/// `compactLog` : compaction PURE, sans toucher au journal passé (sert à
/// écrire en plein tour la forme qu'aura le journal à la fin du tour).
pub fn compact_log(log: &[LogEvent]) -> Vec<LogEvent> {
    compact_log_owned(log.to_vec())
}

// ---------------------------------------------------------------------------
// Rejeu
// ---------------------------------------------------------------------------

/// `{"seq", "ts"}` puis les clés du delta (qui l'emportent en cas de doublon).
fn flatten(ev: &LogEvent) -> Delta {
    let mut m = Delta::new();
    m.insert("seq".to_owned(), ev.seq.into());
    m.insert("ts".to_owned(), ev.ts.into());
    for (k, v) in &ev.delta {
        m.insert(k.clone(), v.clone());
    }
    m
}

/// `decorateEvent` : aplatit un événement pour l'émission SSE et pose
/// `replace: true` quand le client a DÉJÀ vu le début du bloc texte (`from`
/// tombe dedans) : le texte envoyé est le bloc entier, à substituer.
pub fn decorate_event(ev: &LogEvent, from: i64) -> Delta {
    let mut out = flatten(ev);
    if is_text_delta(&ev.delta) && ev_seq0(&ev.delta, ev.seq) <= from {
        out.insert("replace".to_owned(), Value::Bool(true));
    }
    out
}

/// `coalesceReplay` : rejeu léger des événements de Seq > `from`.
///
/// Les deltas texte consécutifs d'un même bloc deviennent UN événement
/// `{clé, seq, ts, ts0, toks, seq0}` (+ `replace` si `seq0 <= from`). Des
/// outils, seul l'état final compte : chaque `done=true` est émis ; le dernier
/// `tool_used` non terminé est retenu (sans être émis sur un événement
/// non-outil, sinon un outil apparaîtrait deux fois) et n'est émis qu'en fin
/// de fil si aucun `done` ne l'a remplacé. Le reste passe aplati tel quel.
pub fn coalesce_replay(events: &[LogEvent], from: i64) -> Vec<Delta> {
    let mut out = Vec::new();
    let mut acc: Option<TextAcc> = None;
    let mut pending_tool: Option<Delta> = None;
    let flush = |acc: &mut Option<TextAcc>, out: &mut Vec<Delta>| {
        if let Some(a) = acc.take() {
            let mut m = Delta::new();
            m.insert(a.key.to_owned(), Value::String(a.buf));
            m.insert("seq".to_owned(), a.seq.into());
            m.insert("ts".to_owned(), a.ts.into());
            m.insert("ts0".to_owned(), a.ts0.into());
            m.insert("toks".to_owned(), a.toks.into());
            m.insert("seq0".to_owned(), a.seq0.into());
            if a.seq0 <= from {
                m.insert("replace".to_owned(), Value::Bool(true));
            }
            out.push(m);
        }
    };
    for ev in events {
        if ev.seq <= from {
            continue;
        }
        let tool = tool_state(&ev.delta);
        if let Some(key) = text_key(&ev.delta) {
            if acc.as_ref().is_some_and(|a| a.key != key) {
                flush(&mut acc, &mut out);
            }
            acc.get_or_insert_with(|| TextAcc::start(key, ev)).add(ev);
            continue;
        }
        flush(&mut acc, &mut out);
        let m = flatten(ev);
        match tool {
            Some(true) => {
                pending_tool = None;
                out.push(m);
            }
            Some(false) => pending_tool = Some(m),
            None => out.push(m),
        }
    }
    flush(&mut acc, &mut out);
    out.extend(pending_tool);
    out
}

/// `historyCut` : où couper le rejeu pour ne garder que les `tail` derniers
/// échanges (un échange commence à un événement portant la clé `user`).
/// Rend le Seq à partir duquel rejouer (exclu) et le nombre d'échanges
/// masqués ; `(0, 0)` = tout rejouer.
pub fn history_cut(log: &[LogEvent], tail: i64) -> (i64, i64) {
    if tail <= 0 {
        return (0, 0);
    }
    let mut seen = 0;
    for i in (0..log.len()).rev() {
        if !log[i].delta.contains_key("user") {
            continue;
        }
        seen += 1;
        if seen == tail {
            let hidden = log[..i]
                .iter()
                .filter(|e| e.delta.contains_key("user"))
                .count() as i64;
            if hidden == 0 {
                return (0, 0);
            }
            return (log[i].seq - 1, hidden);
        }
    }
    (0, 0)
}

/// `historyHead` : événements qui annoncent un historique tronqué —
/// `{history_more: hidden}`, puis les derniers deltas `mode` et `criteria`
/// posés dans la partie masquée (Seq <= `cut`), tels quels.
pub fn history_head(log: &[LogEvent], cut: i64, hidden: i64) -> Vec<Delta> {
    let mut out = vec![obj1("history_more", hidden.into())];
    let mut mode = None;
    let mut crit = None;
    for ev in log {
        if ev.seq > cut {
            break;
        }
        if ev.delta.contains_key("mode") {
            mode = Some(&ev.delta);
        }
        if ev.delta.contains_key("criteria") {
            crit = Some(&ev.delta);
        }
    }
    out.extend([mode, crit].into_iter().flatten().cloned());
    out
}

/// Ce qu'un abonné doit émettre, dans l'ordre, et le Seq à partir duquel
/// suivre le direct ensuite.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct ReplayPlan {
    pub events: Vec<Delta>,
    /// Dernier Seq couvert : le direct reprend aux événements de Seq > `last`.
    pub last: i64,
}

fn reset_event(cur_id: &str) -> Delta {
    let mut m = obj1("reset", Value::Bool(true));
    m.insert("id".to_owned(), cur_id.into());
    m
}

/// Partie pure du début de `subscribeSink` (étapes 0 et 1) sur un instantané
/// du journal.
///
/// - `from` : dernier Seq vu par le client ; `tail` : échanges à rejouer pour
///   un client paginé (`< 0` = ancien client, tout rejouer) ; `conv_id` :
///   discussion affichée par le client (vide = inconnue) ;
/// - `cur_id` : discussion active (`getStr(bkChat, ckActive)` en Go) ;
/// - `ctx_used` : taille du contexte (`c.CtxUsed`, que le Go relit APRÈS le
///   rejeu : l'acteur passe la valeur courante).
///
/// Ordre émis : pad 2048, `{reset, id}` si le curseur vient d'une autre
/// discussion (id différent, ou `from` au-delà du dernier Seq), en-tête de
/// pagination, rejeu coalescé, `{caught_up, id}`, `{ctx_used}` si > 0, pad
/// 16384. Le client s'arrête au premier envoi raté.
pub fn replay_plan(
    snapshot: &[LogEvent],
    from: i64,
    tail: i64,
    conv_id: &str,
    cur_id: &str,
    ctx_used: i64,
) -> ReplayPlan {
    let mut events = vec![pad_event(PAD_OPEN_REPEAT)];
    let mut from = from;
    let mut stale = from > 0 && !conv_id.is_empty() && conv_id != cur_id;
    if let Some(last) = snapshot.last()
        && from > last.seq
    {
        from = 0;
        stale = stale || !conv_id.is_empty();
    }
    if stale {
        from = 0;
        events.push(reset_event(cur_id));
    }
    if from == 0 && tail > 0 {
        let (cut, hidden) = history_cut(snapshot, tail);
        if hidden > 0 {
            from = cut;
            events.extend(history_head(snapshot, cut, hidden));
        }
    }
    let mut last = from;
    for ev in coalesce_replay(snapshot, from) {
        if let Some(s) = ev.get("seq").and_then(Value::as_i64) {
            last = s;
        }
        events.push(ev);
    }
    let mut caught = obj1("caught_up", Value::Bool(true));
    caught.insert("id".to_owned(), cur_id.into());
    events.push(caught);
    if ctx_used > 0 {
        events.push(obj1("ctx_used", ctx_used.into()));
    }
    events.push(pad_event(PAD_CAUGHT_UP_REPEAT));
    ReplayPlan { events, last }
}

/// Reset survenu pendant l'abonnement (epoch changé) : `{reset, id}`, puis,
/// chez un client paginé (`tail >= 0`), l'en-tête des
/// [`HISTORY_TAIL_DEFAULT`] derniers échanges suivi de `{ctx_used}` (émis même
/// à 0, comme en Go). `last` = Seq de coupe, ou 0 pour tout suivre.
pub fn live_reset_plan(log: &[LogEvent], tail: i64, cur_id: &str, ctx_used: i64) -> ReplayPlan {
    let mut events = vec![reset_event(cur_id)];
    let mut last = 0;
    if tail >= 0 {
        let (cut, hidden) = history_cut(log, HISTORY_TAIL_DEFAULT);
        if hidden > 0 {
            last = cut;
            events.extend(history_head(log, cut, hidden));
            events.push(obj1("ctx_used", ctx_used.into()));
        }
    }
    ReplayPlan { events, last }
}

/// Un tour de la boucle du direct : les événements de Seq > `last`, décorés
/// avec `last` comme repère de `replace` (un bloc fusionné par la compaction
/// de fin de tour que le client suivait en direct arrive avec un Seq plus
/// grand que le sien). `last` du résultat = Seq du dernier événement ;
/// inchangé (et `events` vide) s'il n'y a rien de neuf — l'acteur attend.
pub fn live_batch(log: &[LogEvent], last: i64) -> ReplayPlan {
    let pending = &log[first_after(log, last)..];
    ReplayPlan {
        events: pending.iter().map(|ev| decorate_event(ev, last)).collect(),
        last: pending.last().map_or(last, |ev| ev.seq),
    }
}

// ---------------------------------------------------------------------------
// Trames SSE
// ---------------------------------------------------------------------------

#[derive(Serialize)]
struct SseChoice<'a> {
    delta: &'a Delta,
}

#[derive(Serialize)]
struct SsePayload<'a> {
    choices: [SseChoice<'a>; 1],
}

/// Échappement HTML de `encoding/json` : `<`, `>`, `&`, U+2028 et U+2029
/// deviennent `<`, `>`, `&`, ` `, ` `. Ces octets
/// n'apparaissent que dans des chaînes JSON : un remplacement global est sûr.
fn go_html_escape(b: Vec<u8>) -> Vec<u8> {
    let needs = b.iter().any(|&c| matches!(c, b'<' | b'>' | b'&'))
        || b.windows(3)
            .any(|w| w[0] == 0xE2 && w[1] == 0x80 && (w[2] == 0xA8 || w[2] == 0xA9));
    if !needs {
        return b;
    }
    let mut out = Vec::with_capacity(b.len() + 32);
    let mut i = 0;
    while i < b.len() {
        match b[i] {
            b'<' => out.extend_from_slice(b"\\u003c"),
            b'>' => out.extend_from_slice(b"\\u003e"),
            b'&' => out.extend_from_slice(b"\\u0026"),
            0xE2 if i + 2 < b.len() && b[i + 1] == 0x80 && matches!(b[i + 2], 0xA8 | 0xA9) => {
                out.extend_from_slice(if b[i + 2] == 0xA8 {
                    b"\\u2028"
                } else {
                    b"\\u2029"
                });
                i += 3;
                continue;
            }
            c => out.push(c),
        }
        i += 1;
    }
    out
}

/// Charge JSON d'un événement du flux : `{"choices":[{"delta":obj}]}`, clés
/// triées et échappement HTML comme `json.Marshal`. C'est ce que le relais
/// E2E scelle (un sceau par événement).
pub fn sse_payload(obj: &Delta) -> Vec<u8> {
    let p = SsePayload {
        choices: [SseChoice { delta: obj }],
    };
    // Une Map de Value se sérialise toujours.
    go_html_escape(serde_json::to_vec(&p).unwrap_or_default())
}

/// Ligne SSE `data: <corps>\n\n` (corps JSON en clair, ou base64 du sceau E2E).
pub fn sse_data(body: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(body.len() + 8);
    out.extend_from_slice(b"data: ");
    out.extend_from_slice(body);
    out.extend_from_slice(b"\n\n");
    out
}

/// Trame en clair d'un événement (`frame` de `handleChat`).
pub fn sse_data_frame(obj: &Delta) -> Vec<u8> {
    sse_data(&sse_payload(obj))
}

/// Regroupement des trames d'un abonné (`sseStream` sans l'écrivain) : un lot
/// = une écriture. Un client en retard peut trouver des milliers
/// d'événements en attente ; on écrit par tranches de [`SSE_BATCH_BYTES`]
/// octets ou [`SSE_BATCH_EVENTS`] événements.
///
/// Les lots sont RENDUS à l'appelant (qui écrit et pousse sous le verrou du
/// battement de cœur) : `queue` rend `Some(lot)` quand le tampon déborde,
/// `flush` rend ce qui attend. `emit` du Go = `queue` puis `flush`. Le lot
/// rendu emporte son tampon : la règle Go « un gros tampon (> 4 lots) n'est
/// pas gardé à vie » est satisfaite d'office.
#[derive(Debug, Default)]
pub struct SseBatcher {
    buf: Vec<u8>,
    n: usize,
}

impl SseBatcher {
    pub fn new() -> Self {
        Self::default()
    }

    /// Une trame de plus ; `Some(lot)` = tampon plein, à écrire tout de suite.
    pub fn queue(&mut self, frame: &[u8]) -> Option<Vec<u8>> {
        self.buf.extend_from_slice(frame);
        self.n += 1;
        if self.buf.len() >= SSE_BATCH_BYTES || self.n >= SSE_BATCH_EVENTS {
            return self.flush();
        }
        None
    }

    /// Tout ce qui attend, en un lot ; `None` si rien.
    pub fn flush(&mut self) -> Option<Vec<u8>> {
        if self.buf.is_empty() {
            return None;
        }
        self.n = 0;
        Some(std::mem::take(&mut self.buf))
    }

    /// Nombre de trames en attente.
    pub fn pending(&self) -> usize {
        self.n
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn d(v: Value) -> Delta {
        match v {
            Value::Object(m) => m,
            _ => panic!("objet attendu"),
        }
    }

    fn ev(seq: i64, v: Value) -> LogEvent {
        LogEvent {
            seq,
            ts: 0,
            delta: d(v),
        }
    }

    fn evts(seq: i64, ts: i64, v: Value) -> LogEvent {
        LogEvent {
            seq,
            ts,
            delta: d(v),
        }
    }

    /// Conversation de test : journal + Seq, comme `newTestConv`.
    #[derive(Default)]
    struct Conv {
        log: Vec<LogEvent>,
        seq: i64,
    }

    impl Conv {
        fn add(&mut self, v: Value) {
            append_delta(&mut self.log, &mut self.seq, 0, d(v));
        }
    }

    fn seqs(evs: &[Delta]) -> Vec<i64> {
        evs.iter()
            .filter_map(|m| m.get("seq").and_then(Value::as_i64))
            .collect()
    }

    // --- chat_conversation_test.go ---

    // TestSubscribeReplayAndLive : rejeu de l'existant PUIS direct.
    #[test]
    fn subscribe_replay_and_live() {
        let mut c = Conv::default();
        c.add(json!({"user": "salut"}));
        c.add(json!({"content": "bon"}));
        let plan = replay_plan(&c.log, 0, -1, "", "", 0);
        assert_eq!(seqs(&plan.events), vec![1, 2]);
        c.add(json!({"content": "jour"}));
        let live = live_batch(&c.log, plan.last);
        assert_eq!(seqs(&live.events), vec![3]);
        assert_eq!(live.last, 3);
    }

    // TestSubscribeFromOffset : from=N → seulement ce qui est plus récent.
    #[test]
    fn subscribe_from_offset() {
        let mut c = Conv::default();
        c.add(json!({"user": "a"}));
        c.add(json!({"user": "b"}));
        let plan = replay_plan(&c.log, 1, -1, "", "", 0);
        assert_eq!(seqs(&plan.events).first(), Some(&2));
    }

    // TestResetNotifiesSubscribers (partie abonné) : un reset pendant
    // l'abonnement pousse {reset:true} ; le journal vidé, on repart de 0.
    #[test]
    fn reset_notifies_subscribers() {
        let plan = live_reset_plan(&[], -1, "", 0);
        assert!(plan.events.iter().any(|m| m.contains_key("reset")));
        assert_eq!(plan.last, 0);
    }

    // TestReplayMarksReplaceWhenClientSawPartOfBlock
    #[test]
    fn replay_marks_replace_when_client_saw_part_of_block() {
        let mut c = Conv::default();
        c.add(json!({"user": "salut"})); // seq 1
        c.add(json!({"content": "Oui, "})); // seq 2
        c.add(json!({"content": "j'ai "})); // seq 3
        c.add(json!({"content": "accès"})); // seq 4
        c.log = compact_log_owned(std::mem::take(&mut c.log));

        let out = coalesce_replay(&c.log, 3);
        assert_eq!(out.len(), 1, "{out:?}");
        assert_eq!(out[0]["content"], json!("Oui, j'ai accès"));
        assert_eq!(out[0].get("replace"), Some(&json!(true)));

        let out = coalesce_replay(&c.log, 1);
        assert_eq!(out.len(), 1);
        assert!(!out[0].contains_key("replace"));
    }

    // TestLiveSelectionSurJournalTronque
    #[test]
    fn live_selection_sur_journal_tronque() {
        let mut c = Conv {
            log: vec![
                ev(500, json!({"user": "a"})),
                ev(501, json!({"user": "b"})),
                ev(502, json!({"user": "c"})),
            ],
            seq: 502,
        };
        for (from, first) in [(0, 500), (500, 501), (501, 502)] {
            let plan = replay_plan(&c.log, from, -1, "", "", 0);
            assert_eq!(seqs(&plan.events).first(), Some(&first), "from={from}");
            // La dichotomie du direct tombe au même endroit.
            assert_eq!(c.log[first_after(&c.log, from)].seq, first);
        }
        // Client déjà à jour : rien à rejouer, mais le direct suit.
        let plan = replay_plan(&c.log, 502, -1, "", "", 0);
        assert!(seqs(&plan.events).is_empty());
        assert!(live_batch(&c.log, plan.last).events.is_empty());
        c.add(json!({"content": "suite"}));
        assert_eq!(seqs(&live_batch(&c.log, plan.last).events), vec![503]);
    }

    // TestCompactLogCoalesceTools
    #[test]
    fn compact_log_coalesce_tools() {
        let log = vec![
            ev(1, json!({"user": "édite"})),
            ev(2, json!({"tool_used": {"name": "edit", "label": "f"}})),
            ev(
                3,
                json!({"tool_used": {"name": "edit", "label": "f", "typing": true, "body": "a"}}),
            ),
            ev(
                4,
                json!({"tool_used": {"name": "edit", "label": "f", "result": "ok", "done": true}}),
            ),
            ev(5, json!({"content": "fini"})),
        ];
        let out = compact_log_owned(log);
        let mut n_tool = 0;
        for e in &out {
            if let Some(Value::Object(tu)) = e.delta.get("tool_used") {
                n_tool += 1;
                assert_eq!(
                    tu.get("done"),
                    Some(&json!(true)),
                    "un tool_used non terminé a survécu à la compaction"
                );
            }
        }
        assert_eq!(n_tool, 1);
    }

    // TestCoalesceReplayNoDoubleTool
    #[test]
    fn coalesce_replay_no_double_tool() {
        let events = vec![
            ev(1, json!({"tool_used": {"name": "bash", "label": "ls"}})),
            ev(2, json!({"stats": {"gen_tokens": 3}})),
            ev(
                3,
                json!({"tool_used": {"name": "bash", "label": "ls", "result": "x", "done": true}}),
            ),
        ];
        let n = coalesce_replay(&events, 0)
            .iter()
            .filter(|m| m.contains_key("tool_used"))
            .count();
        assert_eq!(n, 1);
    }

    // TestSubscribeStaleFromCrossSession
    #[test]
    fn subscribe_stale_from_cross_session() {
        let mut c = Conv::default();
        c.add(json!({"user": "premier message"}));
        c.add(json!({"content": "réponse"}));
        let plan = replay_plan(&c.log, 9999, -1, "", "", 0);
        let seen: String = plan
            .events
            .iter()
            .filter_map(|m| m.get("user").and_then(Value::as_str))
            .collect();
        assert!(seen.contains("premier message"), "reçu : {seen:?}");
    }

    // --- chat_history_tail_test.go ---

    fn five_exchanges() -> Vec<LogEvent> {
        let mut c = Conv::default();
        for _ in 0..5 {
            c.add(json!({"user": "q"}));
            c.add(json!({"content": "r"}));
            c.add(json!({"turn_done": true}));
        }
        c.log
    }

    // TestHistoryCut
    #[test]
    fn history_cut_garde_les_n_derniers() {
        let log = five_exchanges();
        assert_eq!(history_cut(&log, 2), (9, 3));
        assert_eq!(history_cut(&log, 5), (0, 0));
        assert_eq!(history_cut(&log, 0), (0, 0));
        let evs = coalesce_replay(&log, 9);
        assert_eq!(evs[0]["user"], json!("q"));
        assert_eq!(evs[0]["seq"], json!(10));
    }

    // TestHistoryHeadCarriesMode
    #[test]
    fn history_head_carries_mode() {
        let log = vec![
            ev(1, json!({"mode": "code"})),
            ev(2, json!({"user": "q1"})),
            ev(3, json!({"user": "q2"})),
        ];
        let head = history_head(&log, 2, 1);
        assert_eq!(head.len(), 2, "{head:?}");
        assert_eq!(head[0]["history_more"], json!(1));
        assert_eq!(head[1]["mode"], json!("code"));
    }

    // TestSubscribeTailPaginates
    #[test]
    fn subscribe_tail_paginates() {
        let mut c = Conv::default();
        for _ in 0..5 {
            c.add(json!({"user": "q"}));
            c.add(json!({"content": "r"}));
        }
        let collect = |tail: i64| {
            let plan = replay_plan(&c.log, 0, tail, "", "", 0);
            let mut more = None;
            let mut users = 0;
            for m in &plan.events {
                if let Some(v) = m.get("history_more") {
                    more = Some(v.clone());
                }
                if m.contains_key("user") {
                    users += 1;
                }
                if m.contains_key("caught_up") {
                    break;
                }
            }
            (more, users)
        };
        assert_eq!(collect(2), (Some(json!(3)), 2));
        assert_eq!(collect(-1), (None, 5));
    }

    // --- cu_autoshot_log_test.go ---

    // TestCompactLogRetireLAperçuNavigateur
    #[test]
    fn compact_log_retire_l_apercu_navigateur() {
        let log = vec![
            ev(
                1,
                json!({"tool_used": {"name": "browser_click", "done": false}}),
            ),
            ev(
                2,
                json!({"tool_used": {"name": "browser_click", "done": true, "result": "ok", "shot": "abc"}}),
            ),
        ];
        let out = compact_log(&log);
        assert_eq!(out.len(), 1);
        let tu = out[0].delta["tool_used"].as_object().unwrap();
        assert!(
            !tu.contains_key("shot"),
            "l'aperçu aurait dû quitter le journal"
        );
        assert_eq!(tu["result"], json!("ok"));
        assert_eq!(tu["name"], json!("browser_click"));
        assert!(
            log[1].delta["tool_used"]
                .as_object()
                .unwrap()
                .contains_key("shot"),
            "le journal d'origine ne doit pas être modifié en place"
        );
    }

    // --- chat_persist_test.go ---

    // TestCompactLogPurEgalFinDeTour
    #[test]
    fn compact_log_pur_egal_fin_de_tour() {
        let log = vec![
            evts(1, 10, json!({"user": "q"})),
            evts(2, 11, json!({"reasoning_content": "je ", "toks": 1})),
            evts(3, 12, json!({"reasoning_content": "pense", "toks": 1})),
            evts(4, 13, json!({"tool_used": {"name": "bash", "done": false}})),
            evts(5, 14, json!({"tool_used": {"name": "bash", "done": true}})),
            evts(6, 15, json!({"content": "bon", "toks": 1})),
            evts(7, 16, json!({"content": "jour", "toks": 1})),
        ];
        let orig = log.clone();
        let got = compact_log(&log);
        assert_eq!(log, orig, "compact_log a modifié le journal d'origine");
        assert_eq!(got, compact_log_owned(log.clone()));
        assert_eq!(got.len(), 4);
        // Valeurs exactes du bloc de réflexion fusionné (entiers, comme en Go).
        assert_eq!((got[1].seq, got[1].ts), (3, 12));
        assert_eq!(
            Value::Object(got[1].delta.clone()),
            json!({"reasoning_content": "je pense", "toks": 2, "ts0": 11, "seq0": 2})
        );
    }

    // TestPersistEnPleinTourEcritLeJournalCompacte (partie pure : la forme
    // compactée écrite en plein tour, journal d'origine intact).
    #[test]
    fn persist_en_plein_tour_ecrit_le_journal_compacte() {
        let log: Vec<LogEvent> = (0..50)
            .map(|i| ev(i + 1, json!({"content": "x", "toks": 1})))
            .collect();
        let st = compact_log(&log);
        assert_eq!(st.len(), 1);
        assert_eq!(st[0].delta["content"], json!("x".repeat(50)));
        assert_eq!(log.len(), 50);
    }

    // --- stream_hotpath_test.go ---

    // TestSSEStreamBatches : un lot = une écriture, ordre gardé, gros lot découpé.
    #[test]
    fn sse_stream_batches() {
        let frame = |i: usize| format!("data: {i}\n\n").into_bytes();
        let mut writes: Vec<Vec<u8>> = Vec::new();
        let mut s = SseBatcher::new();
        for i in 1..=10 {
            writes.extend(s.queue(&frame(i)));
        }
        assert_eq!(writes.len(), 0, "écritures avant flush");
        writes.extend(s.flush());
        assert_eq!(writes.len(), 1);
        for i in 11..=10 + 2 * SSE_BATCH_EVENTS {
            writes.extend(s.queue(&frame(i)));
        }
        // emit = queue puis flush
        writes.extend(s.queue(&frame(11 + 2 * SSE_BATCH_EVENTS)));
        writes.extend(s.flush());
        assert_eq!(writes.len(), 4);
        let want: Vec<u8> = (1..=11 + 2 * SSE_BATCH_EVENTS).flat_map(frame).collect();
        assert_eq!(writes.concat(), want, "ordre des événements perdu");
        assert_eq!(s.pending(), 0);
        assert!(s.flush().is_none());
    }

    // TestSubscribeSinkLiveBatchOrder (partie pure) : le direct rend les
    // événements dans l'ordre des seq.
    #[test]
    fn subscribe_sink_live_batch_order() {
        let mut c = Conv::default();
        let plan = replay_plan(&c.log, 0, -1, "", "", 0);
        assert!(plan.events.iter().any(|m| m.contains_key("caught_up")));
        const N: i64 = 500;
        for _ in 0..N {
            c.add(json!({"content": "t"}));
        }
        let live = live_batch(&c.log, plan.last);
        assert_eq!(seqs(&live.events), (1..=N).collect::<Vec<_>>());
        assert_eq!(live.last, N);
    }

    // --- Tests propres au port ---

    #[test]
    fn plafond_du_journal() {
        let mut log = Vec::new();
        let mut seq = 0;
        for _ in 0..5 {
            seq += 1;
            log.push(ev(seq, json!({"content": "t"})));
            cap_log(&mut log, 3);
        }
        assert_eq!(log.iter().map(|e| e.seq).collect::<Vec<_>>(), vec![3, 4, 5]);
        let mut big = Vec::new();
        let mut s = 0;
        assert_eq!(
            append_delta(&mut big, &mut s, 42, d(json!({"user": "a"}))),
            1
        );
        assert_eq!((big[0].seq, big[0].ts), (1, 42));
    }

    #[test]
    fn compaction_journal_court_inchange() {
        // Moins de deux événements : rendu tel quel (ni toks ni ts0).
        let log = vec![ev(1, json!({"content": "a"}))];
        assert_eq!(compact_log(&log), log);
        let log = vec![ev(1, json!({"tool_used": {"name": "x"}}))];
        assert_eq!(compact_log(&log), log);
    }

    #[test]
    fn compaction_relit_les_nombres_du_json() {
        // Journal relu depuis la base : toks/ts0/seq0 en flottants.
        let log = vec![
            evts(
                5,
                50,
                json!({"content": "ab", "toks": 2.0, "ts0": 40.0, "seq0": 3.0}),
            ),
            evts(6, 60, json!({"content": "c"})),
        ];
        let out = compact_log(&log);
        assert_eq!(
            Value::Object(out[0].delta.clone()),
            json!({"content": "abc", "toks": 3, "ts0": 40, "seq0": 3})
        );
        assert_eq!((out[0].seq, out[0].ts), (6, 60));
    }

    #[test]
    fn decoration_du_direct() {
        // Bloc fusionné seq0=2..seq=4 que le client suivait (dernier vu : 3).
        let e = evts(
            4,
            9,
            json!({"content": "abc", "toks": 3, "ts0": 7, "seq0": 2}),
        );
        let m = decorate_event(&e, 3);
        assert_eq!(
            Value::Object(m),
            json!({"seq": 4, "ts": 9, "content": "abc", "toks": 3, "ts0": 7, "seq0": 2, "replace": true})
        );
        assert!(!decorate_event(&e, 1).contains_key("replace"));
        // Non-texte : jamais de replace.
        let u = ev(2, json!({"user": "q"}));
        assert!(!decorate_event(&u, 5).contains_key("replace"));
    }

    #[test]
    fn plan_de_rejeu_complet() {
        let log = vec![
            evts(1, 1, json!({"user": "q"})),
            evts(2, 2, json!({"content": "a"})),
            evts(3, 3, json!({"content": "b"})),
        ];
        let p = replay_plan(&log, 0, -1, "c1", "c1", 77);
        let ev = &p.events;
        assert_eq!(ev[0]["pad"].as_str().unwrap().chars().count(), 2048);
        assert_eq!(ev[0]["pad"].as_str().unwrap().len(), 4096);
        assert_eq!(
            Value::Object(ev[1].clone()),
            json!({"seq": 1, "ts": 1, "user": "q"})
        );
        assert_eq!(
            Value::Object(ev[2].clone()),
            json!({"content": "ab", "seq": 3, "ts": 3, "ts0": 2, "toks": 2, "seq0": 2})
        );
        assert_eq!(
            Value::Object(ev[3].clone()),
            json!({"caught_up": true, "id": "c1"})
        );
        assert_eq!(Value::Object(ev[4].clone()), json!({"ctx_used": 77}));
        assert_eq!(ev[5]["pad"].as_str().unwrap().chars().count(), 16384);
        assert_eq!(ev.len(), 6);
        assert_eq!(p.last, 3);
    }

    #[test]
    fn plan_de_rejeu_discussion_perimee() {
        let log = vec![ev(1, json!({"user": "q"})), ev(2, json!({"content": "r"}))];
        // Id différent : reset puis tout le fil.
        let p = replay_plan(&log, 1, -1, "ancienne", "neuve", 0);
        assert_eq!(
            Value::Object(p.events[1].clone()),
            json!({"reset": true, "id": "neuve"})
        );
        assert_eq!(seqs(&p.events), vec![1, 2]);
        // Même id, curseur au-delà : tout le fil ET reset (id connu).
        let p = replay_plan(&log, 50, -1, "neuve", "neuve", 0);
        assert!(p.events[1].contains_key("reset"));
        assert_eq!(seqs(&p.events), vec![1, 2]);
        // Curseur au-delà sans id : tout le fil, sans reset.
        let p = replay_plan(&log, 50, -1, "", "neuve", 0);
        assert!(!p.events.iter().any(|m| m.contains_key("reset")));
        assert_eq!(seqs(&p.events), vec![1, 2]);
        // Journal vide, from > 0 : rien, last = from.
        let p = replay_plan(&[], 7, -1, "", "x", 0);
        assert_eq!(p.last, 7);
        assert_eq!(p.events.len(), 3);
    }

    #[test]
    fn plan_de_reset_pagine() {
        let mut c = Conv::default();
        c.add(json!({"criteria": ["a"]}));
        for _ in 0..(HISTORY_TAIL_DEFAULT + 2) {
            c.add(json!({"user": "q"}));
        }
        let p = live_reset_plan(&c.log, 0, "id", 0);
        assert_eq!(
            Value::Object(p.events[0].clone()),
            json!({"reset": true, "id": "id"})
        );
        assert_eq!(p.events[1]["history_more"], json!(2));
        assert_eq!(p.events[2]["criteria"], json!(["a"]));
        assert_eq!(Value::Object(p.events[3].clone()), json!({"ctx_used": 0}));
        assert_eq!(p.last, 3);
        // Ancien client (tail < 0) : reset seul.
        let p = live_reset_plan(&c.log, -1, "id", 0);
        assert_eq!(p.events.len(), 1);
        assert_eq!(p.last, 0);
    }

    #[test]
    fn coalescence_outil_interrompu_et_bascule_texte() {
        let log = vec![
            ev(1, json!({"reasoning_content": "r"})),
            ev(2, json!({"content": "a"})),
            ev(3, json!({"tool_used": {"name": "bash", "done": false}})),
            ev(4, json!({"content": "b"})),
        ];
        let out = coalesce_replay(&log, 0);
        let keys: Vec<_> = out
            .iter()
            .map(|m| {
                ["reasoning_content", "content", "tool_used"]
                    .into_iter()
                    .find(|k| m.contains_key(*k))
                    .unwrap()
            })
            .collect();
        // L'outil jamais terminé n'est émis qu'une fois, en fin de fil.
        assert_eq!(
            keys,
            vec!["reasoning_content", "content", "content", "tool_used"]
        );
    }

    #[test]
    fn trames_sse_comme_le_go() {
        let m = d(json!({"content": "<a & b>\u{2028}", "seq": 3}));
        assert_eq!(
            sse_data_frame(&m),
            b"data: {\"choices\":[{\"delta\":{\"content\":\"\\u003ca \\u0026 b\\u003e\\u2028\",\"seq\":3}}]}\n\n"
                .to_vec()
        );
        assert_eq!(
            sse_payload(&d(json!({"x": 1}))),
            br#"{"choices":[{"delta":{"x":1}}]}"#.to_vec()
        );
        assert_eq!(SSE_HEARTBEAT, b": ping\n\n");
    }
}
