//! Routes du chat servies par Rust quand la conversation est tenue par la
//! façade (`LOKI_CHAT=rust`) — port des handlers de `web_chat.go`.
//!
//! - `/api/chat` : flux d'abonnement SSE (rejeu puis direct, battement de
//!   cœur toutes les 4 s, trames regroupées) ;
//! - `/api/chat/send|stop|reset|state|compact|tool-result` ;
//! - `/api/conversations{,/new,/switch,/rename,/delete}`.
//!
//! Le reste (mode Code et critères, pièces jointes, fichiers, images,
//! export) reste au Go, qui suit la discussion active et pousse ses propres
//! événements d'affichage dans le journal tenu ici ([`serve_internal`]). Le
//! trafic de chat qui arrive au Go sans passer par la façade (tunnel du
//! relais) lui est remis, authentifié par le jeton interne.

use std::sync::Arc;
use std::time::Duration;

use bytes::Bytes;
use http_body_util::{BodyExt, Full, Limited, StreamBody};
use hyper::body::{Frame, Incoming};
use hyper::header::{self, HeaderValue};
use hyper::{Method, Request, Response, StatusCode};
use serde_json::{Value, json};
use tokio::sync::mpsc;

use crate::api::{Ctx, query_param};
use crate::auth::json as send_json;
use crate::chat::generate::ChatService;
use crate::chat::journal::{SSE_HEARTBEAT, SseBatcher, sse_data_frame};
use crate::chat::sessions::SessionError;
use crate::proxy::Body;

/// Corps de requête lu au plus (un message collé peut être long).
const MAX_BODY: usize = 32 << 20;
/// Battement de cœur du flux (`sseHeartbeat`).
const HEARTBEAT: Duration = Duration::from_secs(4);

/// Cette route est-elle servie par la conversation tenue ici ?
pub fn handles(path: &str) -> bool {
    matches!(
        path,
        "/api/chat"
            | "/api/chat/send"
            | "/api/chat/stop"
            | "/api/chat/reset"
            | "/api/chat/state"
            | "/api/chat/compact"
            | "/api/chat/tool-result"
            | "/api/conversations"
            | "/api/conversations/new"
            | "/api/conversations/switch"
            | "/api/conversations/rename"
            | "/api/conversations/delete"
            | "/api/perf/summary"
            | "/api/chat/cushot"
    )
}

/// API interne de la façade, pour le Go : un événement d'affichage né chez
/// lui (bascule du mode Code, critères) à journaliser ici, le rechargement de
/// la discussion après le déverrouillage de la mémoire, le changement de
/// projet et les discussions d'un projet supprimé.
pub fn is_internal(path: &str) -> bool {
    matches!(
        path,
        "/internal/facade/v1/delta"
            | "/internal/facade/v1/reload"
            | "/internal/facade/v1/project/switch"
            | "/internal/facade/v1/project/drop"
            | "/internal/facade/v1/chat/tpl"
            | "/internal/facade/v1/chat/prewarm"
            | "/internal/facade/v1/chat/prewarm/cancel"
            | "/internal/facade/v1/perf/record"
            | "/internal/facade/v1/perf/snapshot"
            | "/internal/facade/v1/bgjobs"
            | "/internal/facade/v1/browser"
    )
}

pub async fn serve_internal(chat: &Arc<ChatService>, req: Request<Incoming>) -> Response<Body> {
    let path = req.uri().path().to_string();
    if req.method() != Method::POST {
        return send_json(
            StatusCode::METHOD_NOT_ALLOWED,
            &json!({ "error": "POST attendu" }),
        );
    }
    match path.as_str() {
        "/internal/facade/v1/delta" => match read_json(req.into_body()).await {
            Ok(Value::Object(d)) if !d.is_empty() => {
                chat.push_delta(d);
                Response::builder()
                    .status(StatusCode::NO_CONTENT)
                    .body(Full::new(Bytes::new()).map_err(|n| match n {}).boxed())
                    .unwrap_or_default()
            }
            _ => send_json(
                StatusCode::BAD_REQUEST,
                &json!({ "error": "événement illisible" }),
            ),
        },
        // Dernier verdict de la sonde de gabarit (`tplCapsCurrent`), pour le
        // résumé des performances que sert encore le Go.
        "/internal/facade/v1/chat/tpl" => {
            let cfg = chat.shared.store.config().await;
            match chat
                .shared
                .tpl
                .caps_current(crate::engstatus::is_external(&cfg))
            {
                Some(r) => send_json(
                    StatusCode::OK,
                    &json!({ "ok": true, "result": r.to_json() }),
                ),
                None => send_json(StatusCode::OK, &json!({ "ok": false })),
            }
        }
        // Télémétrie (lot 39) : une complétion faite par le Go (sous-agent,
        // tâche IA, bench, compaction de ses tours, client externe de /v1),
        // rangée dans l'anneau tenu ici ; rendue telle que rangée (numéro,
        // perte de cache, ce qui s'est intercalé).
        "/internal/facade/v1/perf/record" => match read_json(req.into_body()).await {
            Ok(v) => {
                let rec: crate::perfring::PerfRec =
                    serde_json::from_value(v.get("rec").cloned().unwrap_or_default())
                        .unwrap_or_default();
                let prefix: Vec<u64> = v
                    .get("prefix")
                    .and_then(Value::as_array)
                    .map(|a| a.iter().filter_map(Value::as_u64).collect())
                    .unwrap_or_default();
                let got = chat.shared.perf.record(rec, &prefix);
                send_json(StatusCode::OK, &json!({ "rec": got }))
            }
            Err(_) => send_json(
                StatusCode::BAD_REQUEST,
                &json!({ "error": "télémétrie illisible" }),
            ),
        },
        // L'anneau, de la plus ancienne à la plus récente (poids de
        // l'optimiseur).
        // Navigateur piloté (lot 44) : les appels du Go (sous-agents, tâches
        // IA) passent par la session tenue ici — un seul navigateur.
        "/internal/facade/v1/browser" => {
            let v = read_json(req.into_body()).await.unwrap_or_default();
            let b = &chat.shared.browser;
            match str_of(&v, "op") {
                "tool" => {
                    let cfg = chat.shared.store.config().await;
                    let vision = crate::system::vision_enabled(&cfg);
                    let home = chat.shared.images.home();
                    let ws = tokio::task::spawn_blocking(move || {
                        let env = std::env::var(crate::paths::WORKSPACE_ENV).ok();
                        crate::paths::agent_workspace(&home, env.as_deref())
                    })
                    .await
                    .unwrap_or_default();
                    let args = v
                        .get("args")
                        .and_then(Value::as_object)
                        .cloned()
                        .unwrap_or_default();
                    match b.exec(str_of(&v, "name"), &args, vision, &ws).await {
                        Some(o) => send_json(
                            StatusCode::OK,
                            &json!({ "result": o.result, "image": o.image }),
                        ),
                        None => send_json(
                            StatusCode::BAD_REQUEST,
                            &json!({ "error": "outil du navigateur inconnu" }),
                        ),
                    }
                }
                "shot" => send_json(StatusCode::OK, &json!({ "id": b.auto_shot().await })),
                "shutdown" => {
                    b.shutdown().await;
                    send_json(StatusCode::OK, &json!({}))
                }
                _ => send_json(
                    StatusCode::BAD_REQUEST,
                    &json!({ "error": "opération inconnue" }),
                ),
            }
        }
        // Jobs d'arrière-plan encore en vie (lot 43) : l'optimiseur du Go
        // refuse de mesurer à côté.
        "/internal/facade/v1/bgjobs" => send_json(
            StatusCode::OK,
            &json!({ "running": chat.shared.jobs.running() }),
        ),
        "/internal/facade/v1/perf/snapshot" => send_json(
            StatusCode::OK,
            &json!({ "recs": chat.shared.perf.snapshot() }),
        ),
        // PREWARM (lot 33) : déclencheur venu du Go (fin d'une tâche
        // planifiée), annulation relayée (une autre requête est partie).
        "/internal/facade/v1/chat/prewarm" => {
            let v = read_json(req.into_body()).await.unwrap_or_default();
            let why = match str_of(&v, "why") {
                crate::chat::prewarm::AFTER_TASK => crate::chat::prewarm::AFTER_TASK,
                crate::chat::prewarm::SWITCH => crate::chat::prewarm::SWITCH,
                _ => crate::chat::prewarm::TURN_END,
            };
            chat.prewarm_kick(why).await;
            send_json(StatusCode::OK, &json!({ "ok": true }))
        }
        "/internal/facade/v1/chat/prewarm/cancel" => {
            let v = read_json(req.into_body()).await.unwrap_or_default();
            let id = v.get("id").and_then(Value::as_u64).unwrap_or(0);
            let cancelled = chat.shared.prewarm.cancel_id(id);
            send_json(
                StatusCode::OK,
                &json!({ "ok": true, "cancelled": cancelled }),
            )
        }
        "/internal/facade/v1/project/switch" | "/internal/facade/v1/project/drop" => {
            let v = read_json(req.into_body()).await.unwrap_or_default();
            let slug = str_of(&v, "slug");
            let res = if path.ends_with("switch") {
                chat.project_switch(slug)
                    .await
                    .map(|()| json!({ "ok": true }))
            } else {
                chat.drop_project(slug)
                    .await
                    .map(|n| json!({ "ok": true, "dropped": n }))
            };
            match res {
                Ok(v) => send_json(StatusCode::OK, &v),
                Err(r) => refused(r.status, &r.error),
            }
        }
        _ => match chat.sessions.reload_if_empty().await {
            Ok(reloaded) => send_json(StatusCode::OK, &json!({ "reloaded": reloaded })),
            Err(e) => send_json(StatusCode::SERVICE_UNAVAILABLE, &json!({ "error": e })),
        },
    }
}

/// Corps JSON de la requête ; vide → `{}` ; illisible → `Err(message)`.
async fn read_json(body: Incoming) -> Result<Value, String> {
    let bytes = Limited::new(body, MAX_BODY)
        .collect()
        .await
        .map_err(|e| e.to_string())?
        .to_bytes();
    if bytes.iter().all(u8::is_ascii_whitespace) {
        return Err("EOF".into());
    }
    serde_json::from_slice(&bytes).map_err(|e| e.to_string())
}

fn ok() -> Response<Body> {
    send_json(StatusCode::OK, &json!({ "ok": true }))
}

fn refused(status: u16, error: &str) -> Response<Body> {
    send_json(
        StatusCode::from_u16(status).unwrap_or(StatusCode::SERVICE_UNAVAILABLE),
        &json!({ "ok": false, "error": error }),
    )
}

fn session_error(e: SessionError) -> Response<Body> {
    send_json(
        StatusCode::BAD_REQUEST,
        &json!({ "ok": false, "error": e.to_string() }),
    )
}

fn str_of<'a>(v: &'a Value, k: &str) -> &'a str {
    v.get(k).and_then(Value::as_str).unwrap_or("")
}

/// `from_go` : relayée par le Go, qui l'a déjà authentifiée.
pub async fn serve(
    ctx: &Ctx,
    chat: &Arc<ChatService>,
    req: Request<Incoming>,
    from_go: bool,
) -> Response<Body> {
    let (parts, body) = req.into_parts();
    if !from_go && let Err(refus) = ctx.guard(&parts.headers).await {
        return *refus;
    }
    let path = parts.uri.path().to_string();
    match path.as_str() {
        // `handlePerfSummary` : des agrégats seulement, aucun texte.
        "/api/perf/summary" => {
            if parts.method != Method::GET {
                return send_json(
                    StatusCode::METHOD_NOT_ALLOWED,
                    &json!({ "error": "GET seulement" }),
                );
            }
            let mut s = crate::perfring::summarize(&chat.shared.perf.snapshot());
            let cfg = chat.shared.store.config().await;
            s.template = chat
                .shared
                .tpl
                .caps_current(crate::engstatus::is_external(&cfg))
                .map(|r| r.to_json());
            send_json(StatusCode::OK, &json!(s))
        }
        "/api/chat" => match read_json(body).await {
            Ok(v) => subscribe(chat, &v),
            Err(e) => plain(StatusCode::BAD_REQUEST, &e),
        },
        "/api/chat/send" => {
            let v = match read_json(body).await {
                Ok(v) => v,
                Err(e) => return refused(400, &e),
            };
            match chat.send(v).await {
                Ok(s) if s.dup => send_json(StatusCode::OK, &json!({ "ok": true, "dup": true })),
                Ok(s) => send_json(StatusCode::OK, &json!({ "ok": true, "queued": s.queued })),
                Err(r) => refused(r.status, &r.error),
            }
        }
        "/api/chat/stop" => {
            chat.stop().await;
            ok()
        }
        // `handleCUShot` (lot 44) : un aperçu du navigateur piloté, en base64
        // (JSON, pour traverser aussi le tunnel du relais).
        "/api/chat/cushot" => {
            let id = query_param(&parts.uri, "id").unwrap_or_default();
            match chat.shared.browser.shot(&id) {
                Some(b) => send_json(
                    StatusCode::OK,
                    &json!({ "ok": true, "mime": "image/jpeg",
                        "data": base64::Engine::encode(&base64::engine::general_purpose::STANDARD, b) }),
                ),
                None => send_json(
                    StatusCode::NOT_FOUND,
                    &json!({ "ok": false, "error": "capture expirée" }),
                ),
            }
        }
        "/api/chat/reset" => {
            chat.reset().await;
            ok()
        }
        "/api/chat/state" => send_json(
            StatusCode::OK,
            &Value::Object(chat.sessions.conv().state_json()),
        ),
        "/api/chat/compact" => match chat.compact_now().await {
            Ok(()) => ok(),
            Err(r) => refused(r.status, &r.error),
        },
        "/api/chat/tool-result" => {
            let id = query_param(&parts.uri, "id").unwrap_or_default();
            let id = id.trim();
            if id.is_empty() {
                return send_json(StatusCode::BAD_REQUEST, &json!({ "error": "id manquant" }));
            }
            match chat.tool_result(id).await {
                Some(s) => send_json(StatusCode::OK, &json!({ "result": s })),
                None => send_json(
                    StatusCode::NOT_FOUND,
                    &json!({ "error": "résultat non disponible" }),
                ),
            }
        }
        "/api/conversations" => match chat.sessions.list().await {
            Ok((list, active)) => send_json(
                StatusCode::OK,
                &json!({ "conversations": list, "active": active, "busy": chat.sessions.conv().is_generating() }),
            ),
            Err(e) => send_json(StatusCode::SERVICE_UNAVAILABLE, &json!({ "error": e })),
        },
        "/api/conversations/new" => match chat.sessions.new_conv().await {
            Ok(id) => send_json(StatusCode::OK, &json!({ "ok": true, "id": id })),
            Err(e) => session_error(e),
        },
        // Corps illisible : champs vides, comme le `_ = Decode` du Go.
        "/api/conversations/switch" => {
            let v = read_json(body).await.unwrap_or_default();
            let before = chat.sessions.conv().id();
            match chat.sessions.switch(str_of(&v, "id")).await {
                Ok(()) => {
                    // PREWARM : le préchauffage de l'ancienne est annulé ; avec
                    // « full », la nouvelle est préparée si on y reste.
                    if chat.sessions.conv().id() != before {
                        chat.prewarm_after_switch().await;
                    }
                    ok()
                }
                Err(e) => session_error(e),
            }
        }
        "/api/conversations/rename" => {
            let v = read_json(body).await.unwrap_or_default();
            match chat
                .sessions
                .rename(str_of(&v, "id"), str_of(&v, "title"))
                .await
            {
                Ok(()) => ok(),
                Err(e) => session_error(e),
            }
        }
        "/api/conversations/delete" => {
            let v = read_json(body).await.unwrap_or_default();
            match chat.sessions.delete(str_of(&v, "id")).await {
                Ok(()) => ok(),
                Err(e) => session_error(e),
            }
        }
        _ => plain(StatusCode::NOT_FOUND, "404 page not found"),
    }
}

/// `handleChat` : le flux d'abonnement. `from` : dernier Seq vu ; `tail` :
/// échanges à rejouer (absent : ancien client, tout) ; `conv_id` : discussion
/// affichée.
fn subscribe(chat: &Arc<ChatService>, v: &Value) -> Response<Body> {
    let from = v.get("from").and_then(Value::as_i64).unwrap_or(0);
    let tail = v.get("tail").and_then(Value::as_i64).unwrap_or(-1);
    let mut sub = chat
        .sessions
        .conv()
        .subscribe(from, tail, str_of(v, "conv_id"));
    let (tx, rx) = mpsc::channel::<Bytes>(8);
    tokio::spawn(async move {
        let mut beat = tokio::time::interval(HEARTBEAT);
        beat.tick().await;
        loop {
            tokio::select! {
                evs = sub.next() => {
                    let mut batch = SseBatcher::new();
                    for e in &evs {
                        if let Some(chunk) = batch.queue(&sse_data_frame(e))
                            && tx.send(Bytes::from(chunk)).await.is_err()
                        {
                            return;
                        }
                    }
                    if let Some(chunk) = batch.flush()
                        && tx.send(Bytes::from(chunk)).await.is_err()
                    {
                        return;
                    }
                }
                _ = beat.tick() => {
                    if tx.send(Bytes::from_static(SSE_HEARTBEAT)).await.is_err() {
                        return;
                    }
                }
                _ = tx.closed() => return,
            }
        }
    });
    let stream = futures_util::stream::unfold(rx, |mut rx| async move {
        rx.recv()
            .await
            .map(|b| (Ok::<_, hyper::Error>(Frame::data(b)), rx))
    });
    let mut r = Response::new(BodyExt::boxed(StreamBody::new(stream)));
    let h = r.headers_mut();
    h.insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("text/event-stream"),
    );
    h.insert(
        header::CACHE_CONTROL,
        HeaderValue::from_static("no-cache, no-transform"),
    );
    h.insert("x-accel-buffering", HeaderValue::from_static("no"));
    r
}

fn plain(status: StatusCode, msg: &str) -> Response<Body> {
    let mut r = Response::new(
        Full::new(Bytes::from(format!("{msg}\n")))
            .map_err(|n| match n {})
            .boxed(),
    );
    *r.status_mut() = status;
    r.headers_mut().insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("text/plain; charset=utf-8"),
    );
    r
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn routes() {
        assert!(handles("/api/chat") && handles("/api/conversations/delete"));
        assert!(
            !handles("/api/chat/upload")
                && !handles("/api/chat/export")
                && !handles("/api/chat/mode")
        );
        assert!(is_internal("/internal/facade/v1/delta") && !is_internal("/internal/v1/auth"));
    }

    /// La recherche plein-texte reste au Go, qui tient l'index (chat_search.go) :
    /// la façade la relaie, elle ne doit pas la servir.
    #[test]
    fn recherche_relayee_au_go() {
        assert!(!handles("/api/conversations/search"));
    }
}
