//! Surface compatible OpenAI (`/v1/*`), relayée par la façade directement au
//! moteur (lot 5) — port de `mountOAI`, `requireCompletionKey` et
//! `oaiHandler` (`llm_oai.go`, `web_auth.go`).
//!
//! Les DÉCISIONS restent au Go (`web_internal_oai.go`), qui tient l'état de
//! la conversation : il dit où router (`route`), compte la requête « en vol »
//! et refuse pendant un benchmark ou une optimisation (`begin`), puis apprend
//! sa fin (`end`) — à la fin du flux ou quand le client part.
//!
//! Comme le Go : on NE touche PAS à `Authorization` (llama-server revérifie
//! la clé, `--api-key`), on ne bufferise rien (les jetons arrivent un par
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
use std::time::Duration;

use crate::api::Ctx;
use crate::auth;
use crate::proxy::{Body, strip_hop_by_hop};
use crate::store::Store;

pub type OutBody = BoxBody<Bytes, hyper::Error>;
pub type EngineClient = Client<HttpConnector, OutBody>;

/// Réponse d'erreur au format OpenAI (`sendOAIError`) : les SDK lisent
/// `body.error.message`.
pub fn oai_error(status: StatusCode, msg: &str, typ: &str, code: &str) -> Response<Body> {
    auth::json(
        status,
        &json!({ "error": { "message": msg, "type": typ, "param": null, "code": code } }),
    )
}

/// Lit `route` auprès du Go : port du moteur, clé, second slot.
async fn fetch_route(ctx: &Ctx, method: &Method, path: &str) -> Result<Value, Box<Response<Body>>> {
    route_for(&ctx.store, method, path).await.map_err(|e| {
        eprintln!("[loki-rs] /v1 : route illisible : {e}");
        Box::new(unavailable())
    })
}

/// `route` du Go pour `method path` (aussi lu par le mode Code avec Claude
/// Code : clé `/v1` et état de la clé).
pub async fn route_for(store: &Store, method: &Method, path: &str) -> Result<Value, String> {
    store
        .internal_json(
            Method::GET,
            &format!(
                "/internal/v1/oai/route?method={}&path={}",
                crate::store::enc(method.as_str()),
                crate::store::enc(path)
            ),
            None,
        )
        .await
        .map_err(|e| e.to_string())
}

fn unavailable() -> Response<Body> {
    oai_error(
        StatusCode::SERVICE_UNAVAILABLE,
        "configuration illisible — réessaie dans un instant",
        "api_error",
        "service_unavailable",
    )
}

/// Traduit un refus du Go (`{"status","message","type","code","retry_after"}`)
/// en erreur OpenAI ; un `retry_after` vide ne pose pas d'en-tête.
fn refusal_response(refus: &Value) -> Response<Body> {
    let status = StatusCode::from_u16(refus["status"].as_u64().unwrap_or(503) as u16)
        .unwrap_or(StatusCode::SERVICE_UNAVAILABLE);
    let mut r = oai_error(
        status,
        refus["message"].as_str().unwrap_or(""),
        refus["type"].as_str().unwrap_or("server_error"),
        refus["code"].as_str().unwrap_or(""),
    );
    if let Some(ra) = refus["retry_after"].as_str().filter(|s| !s.is_empty())
        && let Ok(v) = HeaderValue::from_str(ra)
    {
        r.headers_mut().insert(header::RETRY_AFTER, v);
    }
    r
}

/// Chemins dont le champ `model` peut demander une bascule de preset.
fn is_completion(path: &str) -> bool {
    matches!(path, "/v1/chat/completions" | "/v1/completions")
}

/// Le champ `model` (chaîne de premier niveau) d'un corps JSON.
fn body_model(raw: &[u8]) -> Option<String> {
    let obj: serde_json::Map<String, Value> = serde_json::from_slice(raw).ok()?;
    obj.get("model")?.as_str().map(str::to_string)
}

/// `GET /v1/models` : les presets locaux d'au moins `OAI_MIN_CTX` jetons, au
/// format OpenAI, avec leur fenêtre (`context_length`, lue par Hermes Agent).
async fn models_list(ctx: &Ctx, head: bool) -> Response<Body> {
    let lv = crate::presets_api::live(&ctx.store).await;
    let data: Vec<Value> = crate::presets::oai_models(&ctx.engine.home, &lv.cfg, &lv.order)
        .into_iter()
        .map(|m| {
            json!({
                "id": m.id, "object": "model", "created": 0, "owned_by": "loki",
                "name": m.name, "context_length": m.ctx, "active": m.active,
            })
        })
        .collect();
    let mut r = auth::json(StatusCode::OK, &json!({ "object": "list", "data": data }));
    if head {
        *r.body_mut() = Full::new(Bytes::new()).map_err(|n| match n {}).boxed();
    }
    r
}

/// Bascule sur le preset `m` par le Go (`/internal/v1/oai/switch`), qui prend
/// le verrou de génération, refuse si quoi que ce soit tourne, relance le
/// moteur et attend qu'il réponde.
async fn switch_to(ctx: &Ctx, m: &crate::presets::OaiModel) -> Result<(), Box<Response<Body>>> {
    let res = ctx
        .store
        .internal_post_within(
            "/internal/v1/oai/switch",
            &json!({ "path": m.path.to_string_lossy() }),
            Duration::from_secs(240),
        )
        .await;
    match res {
        Ok((code, v)) if code == StatusCode::OK => {
            if let Some(refus) = v.get("refuse").filter(|r| r.is_object()) {
                return Err(Box::new(refusal_response(refus)));
            }
            eprintln!("[loki-rs] /v1 : bascule vers le preset {}", m.id);
            Ok(())
        }
        Ok((code, v)) => {
            eprintln!("[loki-rs] /v1 : bascule refusée ({code}) : {v}");
            Err(Box::new(unavailable()))
        }
        Err(e) => {
            eprintln!("[loki-rs] /v1 : bascule : {e}");
            Err(Box::new(unavailable()))
        }
    }
}

pub async fn serve(ctx: &Ctx, engine: &EngineClient, req: Request<Incoming>) -> Response<Body> {
    let method = req.method().clone();
    let path = req.uri().path().to_string();
    let mut route = match fetch_route(ctx, &method, &path).await {
        Ok(v) => v,
        Err(r) => return *r,
    };

    // requireCompletionKey : un préflight CORS ne porte jamais d'Authorization.
    if method != Method::OPTIONS {
        if route["key_ok"] != Value::Bool(true) {
            return unavailable();
        }
        let key = route["key"].as_str().unwrap_or("");
        if !key.is_empty() && !auth::check_bearer(req.headers(), key) {
            let mut r = oai_error(
                StatusCode::UNAUTHORIZED,
                "Clé API invalide ou absente — envoie l'en-tête Authorization: Bearer <clé>.",
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

    // La liste des modèles est celle des presets, pas celle du moteur.
    if path == "/v1/models" && (method == Method::GET || method == Method::HEAD) {
        return models_list(ctx, method == Method::HEAD).await;
    }

    let post_v1 = method == Method::POST && path.starts_with("/v1/");
    // Claude Code (mode Code) se fait reconnaître : sa discussion garde le
    // slot 0 même avec SIDE_SLOT. L'en-tête n'ouvre aucun droit (la clé est
    // déjà contrôlée) et ne va pas au moteur.
    let from_harness = req
        .headers()
        .get(crate::harness::HEADER)
        .and_then(|v| v.to_str().ok())
        .is_some_and(crate::harness::matches);
    let (mut parts, body) = req.into_parts();
    parts.headers.remove(crate::harness::HEADER);
    // Tout POST /v1 est lu (borné) : le champ `model` peut demander une
    // bascule, et le second slot réécrit le corps.
    let (raw, raw_body): (Option<Bytes>, Option<Incoming>) = if post_v1 {
        let max = route["max_body"].as_u64().unwrap_or(0) as usize;
        match read_body_limited(body, max).await {
            Ok(b) => (Some(b), None),
            Err(r) => return *r,
        }
    } else {
        (None, Some(body))
    };

    // Bascule de modèle : seulement avec une clé (sinon n'importe qui
    // rechargerait les modèles), pour l'id exact d'un preset annoncé. Le
    // verrou tient jusqu'au `begin` : le ticket de cette requête existe alors,
    // et une autre bascule sera refusée tant qu'il vit.
    let key_set = !route["key"].as_str().unwrap_or("").is_empty();
    let mut _switch_guard = None;
    if let Some(b) = raw.as_ref()
        && key_set
        && is_completion(&path)
        && let Some(model) = body_model(b)
    {
        let listed = |lv: &crate::presets_api::Live| {
            crate::presets::oai_models(&ctx.engine.home, &lv.cfg, &lv.order)
                .into_iter()
                .find(|m| m.id == model)
        };
        if listed(&crate::presets_api::live(&ctx.store).await).is_some() {
            let guard = match tokio::time::timeout(
                Duration::from_secs(2),
                ctx.oai_switch.clone().lock_owned(),
            )
            .await
            {
                Ok(g) => g,
                Err(_) => {
                    let mut r = oai_error(
                        StatusCode::SERVICE_UNAVAILABLE,
                        "changement de modèle en cours — réessaie dans un moment",
                        "server_error",
                        "model_switching",
                    );
                    r.headers_mut()
                        .insert(header::RETRY_AFTER, HeaderValue::from_static("30"));
                    return r;
                }
            };
            if let Some(m) = listed(&crate::presets_api::live(&ctx.store).await)
                && !m.active
                && let Err(r) = switch_to(ctx, &m).await
            {
                return *r;
            }
            // Une autre requête a pu basculer pendant l'attente : port et
            // second slot suivent le preset en service.
            route = match fetch_route(ctx, &method, &path).await {
                Ok(v) => v,
                Err(r) => return *r,
            };
            _switch_guard = Some(guard);
        }
    }

    // Claude Code impose son niveau de réflexion (`output_config.effort`,
    // « high » par défaut) que le gabarit du modèle peut refuser : on le
    // remplace par le réglage du preset, comme pour la boucle de Loki.
    let raw = match raw {
        Some(b) if from_harness => {
            let kwargs = match ctx.chat.as_ref() {
                Some(c) => c.harness_reasoning().await,
                None => None,
            };
            Some(
                rewrite_harness_body(&b, kwargs.as_ref())
                    .map(Bytes::from)
                    .unwrap_or(b),
            )
        }
        r => r,
    };

    let port = route["port"].as_u64().unwrap_or(8080);
    let side_live = route["side_live"] == Value::Bool(true);

    // SIDE_SLOT : la complétion d'un client passe par le second slot ; celle
    // de Claude Code, par le slot de la discussion.
    let (body, side): (OutBody, bool) = match (raw, raw_body) {
        (Some(b), _) => {
            let slot = if from_harness { 0 } else { 1 };
            let (b, side) = if side_live {
                match rewrite_id_slot(&b, slot) {
                    Some(nb) => (Bytes::from(nb), slot == 1),
                    None => (b, false),
                }
            } else {
                (b, false)
            };
            parts
                .headers
                .insert(header::CONTENT_LENGTH, HeaderValue::from(b.len()));
            parts.headers.remove(header::TRANSFER_ENCODING);
            (Full::new(b).map_err(|n| match n {}).boxed(), side)
        }
        (None, Some(body)) => (body.boxed(), false),
        (None, None) => unreachable!("corps lu ou gardé tel quel"),
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
    drop(_switch_guard);
    let ticket = match begin {
        Ok(v) => {
            if let Some(refus) = v.get("refuse").filter(|r| r.is_object()) {
                return refusal_response(refus);
            }
            v["ticket"].as_u64()
        }
        Err(e) => {
            // Sans ticket, le Go ne saurait pas qu'une requête occupe le moteur :
            // il pourrait effacer le slot en plein calcul. On refuse.
            eprintln!("[loki-rs] /v1 : begin : {e}");
            return unavailable();
        }
    };
    let guard = TicketGuard {
        store: ctx.store.clone(),
        ticket,
    };

    // Vers le moteur : Host d'origine.
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

/// Lit le corps d'une requête, borné à `max` octets.
async fn read_body_limited(body: Incoming, max: usize) -> Result<Bytes, Box<Response<Body>>> {
    match http_body_util::Limited::new(body, max + 1).collect().await {
        Ok(c) => Ok(c.to_bytes()),
        Err(e) => {
            let too_big = e
                .downcast_ref::<http_body_util::LengthLimitError>()
                .is_some();
            Err(Box::new(if too_big {
                oai_error(
                    StatusCode::PAYLOAD_TOO_LARGE,
                    &format!("corps trop gros (plus de {max} octets)"),
                    "invalid_request_error",
                    "body_too_large",
                )
            } else {
                oai_error(
                    StatusCode::BAD_REQUEST,
                    &format!("corps illisible : {e}"),
                    "invalid_request_error",
                    "bad_request",
                )
            }))
        }
    }
}

/// Corps avec `id_slot: <slot>`, ou `None` si ce n'est pas un objet JSON.
pub fn rewrite_id_slot(raw: &[u8], slot: u8) -> Option<Vec<u8>> {
    let mut obj: BTreeMap<String, Box<RawValue>> = serde_json::from_slice(raw).ok()?;
    obj.insert(
        "id_slot".into(),
        RawValue::from_string(slot.to_string()).ok()?,
    );
    serde_json::to_vec(&obj).ok()
}

/// Corps d'une requête de Claude Code : `output_config.effort` retiré, le
/// raisonnement du preset (`kwargs`) posé dans `chat_template_kwargs`.
/// `None` si ce n'est pas un objet JSON.
pub fn rewrite_harness_body(
    raw: &[u8],
    kwargs: Option<&serde_json::Map<String, Value>>,
) -> Option<Vec<u8>> {
    let mut obj: serde_json::Map<String, Value> = serde_json::from_slice(raw).ok()?;
    if let Some(Value::Object(oc)) = obj.get_mut("output_config") {
        oc.remove("effort");
        if oc.is_empty() {
            obj.remove("output_config");
        }
    }
    if let Some(k) = kwargs {
        let ctk = obj
            .entry("chat_template_kwargs")
            .or_insert_with(|| Value::Object(Default::default()));
        if !ctk.is_object() {
            *ctk = Value::Object(Default::default());
        }
        if let Value::Object(m) = ctk {
            for (key, v) in k {
                m.insert(key.clone(), v.clone());
            }
        }
    }
    serde_json::to_vec(&obj).ok()
}

/// Rend le ticket au Go quand la réponse est finie ou abandonnée.
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

/// Corps de réponse du moteur, relayé trame par trame ; le ticket est rendu
/// quand il est lâché (flux terminé ou client parti).
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
        let out = String::from_utf8(rewrite_id_slot(raw, 1).unwrap()).unwrap();
        assert_eq!(
            out,
            r#"{"id_slot":1,"model":"x","stream":true,"temperature":0.10000000000000001}"#
        );
        assert!(rewrite_id_slot(b"[1,2]", 1).is_none());
        // Claude Code : effort retiré, raisonnement du preset posé.
        let raw =
            br#"{"model":"m","output_config":{"effort":"high"},"chat_template_kwargs":{"a":1}}"#;
        let k: serde_json::Map<String, Value> =
            serde_json::from_str(r#"{"reasoning_effort":"medium"}"#).unwrap();
        let v: Value =
            serde_json::from_slice(&rewrite_harness_body(raw, Some(&k)).unwrap()).unwrap();
        assert_eq!(
            v,
            json!({"model":"m","chat_template_kwargs":{"a":1,"reasoning_effort":"medium"}})
        );
        let raw = br#"{"output_config":{"effort":"high","format":{"type":"json"}}}"#;
        let v: Value = serde_json::from_slice(&rewrite_harness_body(raw, None).unwrap()).unwrap();
        assert_eq!(v, json!({"output_config":{"format":{"type":"json"}}}));
        assert!(rewrite_harness_body(b"[]", None).is_none());
        assert!(rewrite_id_slot(b"pas du json", 1).is_none());
        let out =
            String::from_utf8(rewrite_id_slot(br#"{"id_slot":1,"m":2}"#, 0).unwrap()).unwrap();
        assert_eq!(out, r#"{"id_slot":0,"m":2}"#);
    }
}
