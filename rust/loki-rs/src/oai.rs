//! Surface compatible OpenAI (`/v1/*`), relayÃ©e par la faÃ§ade directement au
//! moteur (lot 5) â€” port de `mountOAI`, `requireCompletionKey` et
//! `oaiHandler` (`llm_oai.go`, `web_auth.go`).
//!
//! Les DÃ‰CISIONS restent au Go (`web_internal_oai.go`), qui tient l'Ã©tat de
//! la conversation : il dit oÃ¹ router (`route`), compte la requÃªte Â« en vol Â»
//! et refuse pendant un benchmark ou une optimisation (`begin`), puis apprend
//! sa fin (`end`) â€” Ã  la fin du flux ou quand le client part.
//!
//! Comme le Go : on NE touche PAS Ã  `Authorization` (llama-server revÃ©rifie
//! la clÃ©, `--api-key`), on ne bufferise rien (les jetons arrivent un par
//! un), et les `X-Forwarded-*` entrants ne passent pas au moteur.

use bytes::Bytes;
use http_body_util::{BodyExt, Full, combinators::BoxBody};
use hyper::body::{Frame, Incoming, SizeHint};
use hyper::header::{self, HeaderValue};
use hyper::{Method, Request, Response, StatusCode, Uri};
use hyper_util::client::legacy::{Client, connect::HttpConnector};
use serde_json::{Value, json, value::RawValue};
use std::collections::BTreeMap;
use std::pin::Pin;
use std::task::{Context, Poll};

use crate::api::Ctx;
use crate::auth;
use crate::proxy::{Body, strip_hop_by_hop};
use crate::store::Store;

pub type OutBody = BoxBody<Bytes, hyper::Error>;
pub type EngineClient = Client<HttpConnector, OutBody>;

/// RÃ©ponse d'erreur au format OpenAI (`sendOAIError`) : les SDK lisent
/// `body.error.message`.
pub fn oai_error(status: StatusCode, msg: &str, typ: &str, code: &str) -> Response<Body> {
    auth::json(
        status,
        &json!({ "error": { "message": msg, "type": typ, "param": null, "code": code } }),
    )
}

pub async fn serve(ctx: &Ctx, engine: &EngineClient, req: Request<Incoming>) -> Response<Body> {
    let method = req.method().clone();
    let path = req.uri().path().to_string();
    let route = match ctx
        .store
        .internal_json(
            Method::GET,
            &format!(
                "/internal/v1/oai/route?method={}&path={}",
                crate::store::enc(method.as_str()),
                crate::store::enc(&path)
            ),
            None,
        )
        .await
    {
        Ok(v) => v,
        Err(e) => {
            eprintln!("[loki-rs] /v1 : route illisible : {e}");
            return oai_error(
                StatusCode::SERVICE_UNAVAILABLE,
                "configuration illisible â€” rÃ©essaie dans un instant",
                "api_error",
                "service_unavailable",
            );
        }
    };

    // requireCompletionKey : un prÃ©flight CORS ne porte jamais d'Authorization.
    if method != Method::OPTIONS {
        if route["key_ok"] != Value::Bool(true) {
            return oai_error(
                StatusCode::SERVICE_UNAVAILABLE,
                "configuration illisible â€” rÃ©essaie dans un instant",
                "api_error",
                "service_unavailable",
            );
        }
        let key = route["key"].as_str().unwrap_or("");
        if !key.is_empty() && !auth::check_bearer(req.headers(), key) {
            let mut r = oai_error(
                StatusCode::UNAUTHORIZED,
                "ClÃ© API invalide ou absente â€” envoie l'en-tÃªte Authorization: Bearer <clÃ©>.",
                "invalid_request_error",
                "invalid_api_key",
            );
            r.headers_mut().insert(
                header::WWW_AUTHENTICATE,
                HeaderValue::from_static(r#"Bearer realm="loki""#),
            );
            return r;
        }
    }

    let port = route["port"].as_u64().unwrap_or(8080);
    let strata = route["strata"] == Value::Bool(true);
    let side_live = route["side_live"] == Value::Bool(true);
    let max_body = route["max_body"].as_u64().unwrap_or(0) as usize;

    // SIDE_SLOT : la complÃ©tion d'un client passe par le second slot.
    let (mut parts, body) = req.into_parts();
    let post_v1 = method == Method::POST && path.starts_with("/v1/");
    let (body, side): (OutBody, bool) = if post_v1 && side_live {
        match side_slot_body(body, max_body).await {
            Ok((b, side)) => {
                parts
                    .headers
                    .insert(header::CONTENT_LENGTH, HeaderValue::from(b.len()));
                parts.headers.remove(header::TRANSFER_ENCODING);
                (Full::new(b).map_err(|n| match n {}).boxed(), side)
            }
            Err(r) => return *r,
        }
    } else {
        (body.boxed(), false)
    };

    let read = method == Method::GET || method == Method::HEAD;
    let begin = ctx
        .store
        .internal_json(
            Method::POST,
            "/internal/v1/oai/begin",
            Some(&json!({ "read": read, "side": side, "post_v1": post_v1 })),
        )
        .await;
    let ticket = match begin {
        Ok(v) => {
            if let Some(refus) = v.get("refuse").filter(|r| r.is_object()) {
                let status = StatusCode::from_u16(refus["status"].as_u64().unwrap_or(503) as u16)
                    .unwrap_or(StatusCode::SERVICE_UNAVAILABLE);
                let mut r = oai_error(
                    status,
                    refus["message"].as_str().unwrap_or(""),
                    refus["type"].as_str().unwrap_or("server_error"),
                    refus["code"].as_str().unwrap_or(""),
                );
                if let Some(ra) = refus["retry_after"].as_str()
                    && let Ok(v) = HeaderValue::from_str(ra)
                {
                    r.headers_mut().insert(header::RETRY_AFTER, v);
                }
                return r;
            }
            v["ticket"].as_u64()
        }
        Err(e) => {
            // Sans ticket, le Go ne saurait pas qu'une requÃªte occupe le moteur :
            // il pourrait effacer le slot en plein calcul. On refuse.
            eprintln!("[loki-rs] /v1 : begin : {e}");
            return oai_error(
                StatusCode::SERVICE_UNAVAILABLE,
                "configuration illisible â€” rÃ©essaie dans un instant",
                "api_error",
                "service_unavailable",
            );
        }
    };
    let guard = TicketGuard {
        store: ctx.store.clone(),
        ticket,
    };

    // Vers le moteur : Host d'origine, sauf Strata qui refuse un Host inconnu.
    let engine_host = format!("127.0.0.1:{port}");
    let pq = parts
        .uri
        .path_and_query()
        .map(|p| p.as_str())
        .unwrap_or("/");
    parts.uri = match format!("http://{engine_host}{pq}").parse::<Uri>() {
        Ok(u) => u,
        Err(_) => {
            return oai_error(
                StatusCode::BAD_REQUEST,
                "URI invalide",
                "invalid_request_error",
                "bad_request",
            );
        }
    };
    strip_hop_by_hop(&mut parts.headers);
    for h in [
        "x-forwarded-for",
        "x-forwarded-host",
        "x-forwarded-proto",
        "forwarded",
    ] {
        parts.headers.remove(h);
    }
    if strata && let Ok(v) = HeaderValue::from_str(&engine_host) {
        parts.headers.insert(header::HOST, v);
    }
    let out = Request::from_parts(parts, body);
    match engine.request(out).await {
        Ok(resp) => {
            let (mut rp, rb) = resp.into_parts();
            strip_hop_by_hop(&mut rp.headers);
            Response::from_parts(
                rp,
                Guarded {
                    inner: rb,
                    _guard: guard,
                }
                .boxed(),
            )
        }
        Err(e) => {
            drop(guard);
            oai_error(
                StatusCode::BAD_GATEWAY,
                &format!("moteur injoignable : {e}"),
                "api_error",
                "upstream_unavailable",
            )
        }
    }
}

/// `sideSlotProxyRewrite` : lit le corps (bornÃ©), pose `id_slot: 1` si c'est
/// un objet JSON. Les valeurs sont gardÃ©es telles quelles (RawValue) ; les
/// clÃ©s ressortent triÃ©es, comme `json.Marshal` d'une map Go.
async fn side_slot_body(body: Incoming, max: usize) -> Result<(Bytes, bool), Box<Response<Body>>> {
    let raw = match http_body_util::Limited::new(body, max + 1).collect().await {
        Ok(c) => c.to_bytes(),
        Err(e) => {
            let too_big = e
                .downcast_ref::<http_body_util::LengthLimitError>()
                .is_some();
            return Err(Box::new(if too_big {
                oai_error(
                    StatusCode::PAYLOAD_TOO_LARGE,
                    "corps trop gros pour Ãªtre routÃ© vers le slot des clients",
                    "invalid_request_error",
                    "side_slot",
                )
            } else {
                oai_error(
                    StatusCode::BAD_REQUEST,
                    &format!("corps illisible : {e}"),
                    "invalid_request_error",
                    "side_slot",
                )
            }));
        }
    };
    Ok(match rewrite_id_slot(&raw) {
        Some(b) => (Bytes::from(b), true),
        None => (raw, false),
    })
}

/// Corps avec `id_slot: 1`, ou `None` si ce n'est pas un objet JSON.
pub fn rewrite_id_slot(raw: &[u8]) -> Option<Vec<u8>> {
    let mut obj: BTreeMap<String, Box<RawValue>> = serde_json::from_slice(raw).ok()?;
    obj.insert("id_slot".into(), RawValue::from_string("1".into()).ok()?);
    serde_json::to_vec(&obj).ok()
}

/// Rend le ticket au Go quand la rÃ©ponse est finie ou abandonnÃ©e.
struct TicketGuard {
    store: Store,
    ticket: Option<u64>,
}

impl Drop for TicketGuard {
    fn drop(&mut self) {
        let Some(ticket) = self.ticket.take() else {
            return;
        };
        let store = self.store.clone();
        if let Ok(rt) = tokio::runtime::Handle::try_current() {
            rt.spawn(async move {
                if let Err(e) = store
                    .internal_json(
                        Method::POST,
                        "/internal/v1/oai/end",
                        Some(&json!({ "ticket": ticket })),
                    )
                    .await
                {
                    eprintln!("[loki-rs] /v1 : end : {e}");
                }
            });
        }
    }
}

/// Corps de rÃ©ponse du moteur, relayÃ© trame par trame ; le ticket est rendu
/// quand il est lÃ¢chÃ© (flux terminÃ© ou client parti).
struct Guarded {
    inner: Incoming,
    _guard: TicketGuard,
}

impl hyper::body::Body for Guarded {
    type Data = Bytes;
    type Error = hyper::Error;

    fn poll_frame(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<Frame<Bytes>, hyper::Error>>> {
        Pin::new(&mut self.inner).poll_frame(cx)
    }
    fn is_end_stream(&self) -> bool {
        self.inner.is_end_stream()
    }
    fn size_hint(&self) -> SizeHint {
        self.inner.size_hint()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn id_slot_pose_cles_triees_valeurs_intactes() {
        let raw = br#"{"stream":true,"model":"x","temperature":0.10000000000000001,"id_slot":0}"#;
        let out = String::from_utf8(rewrite_id_slot(raw).unwrap()).unwrap();
        assert_eq!(
            out,
            r#"{"id_slot":1,"model":"x","stream":true,"temperature":0.10000000000000001}"#
        );
        assert!(rewrite_id_slot(b"[1,2]").is_none());
        assert!(rewrite_id_slot(b"pas du json").is_none());
    }
}
