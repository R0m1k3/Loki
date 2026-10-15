//! Façade HTTP : reçoit tout le trafic public. Les routes déjà réécrites en
//! Rust sont servies ici (UI statique, routes de `api.rs`) ; tout le reste
//! est relayé au serveur Go (`loki web` sur 127.0.0.1).
//!
//! Règles, héritées de `web_server.go` :
//! - aucun tampon : un flux SSE (chat, `/v1` en streaming) part trame par trame ;
//! - aucun délai d'écriture : un flux reste ouvert tant que la page l'écoute ;
//! - en-têtes de bout en bout transmis intacts (`Host`, `Authorization`,
//!   `Origin`, `Sec-Fetch-*`, `X-Forwarded-Proto`…) : le Go s'en sert pour ses
//!   contrôles d'accès ; seuls les en-têtes hop-by-hop sont retirés ;
//! - `X-Loki-Via` entrant effacé : il marque le trafic du tunnel du relais et
//!   dispense du contrôle d'hôte anti-DNS-rebinding — un client ne doit jamais
//!   pouvoir le poser lui-même.

use bytes::Bytes;
use http_body_util::{BodyExt, Full, combinators::BoxBody};
use hyper::body::Incoming;
use hyper::header::{self, HeaderMap, HeaderName, HeaderValue};
use hyper::service::service_fn;
use hyper::{Request, Response, StatusCode, Uri};
use hyper_util::client::legacy::{Client, connect::HttpConnector};
use hyper_util::rt::{TokioExecutor, TokioIo};
use std::convert::Infallible;
use std::future::Future;
use std::net::SocketAddr;
use std::sync::Arc;
use std::time::Duration;
use tokio::net::TcpListener;

use crate::api;
use crate::auth::{HostEnv, INTERNAL_HEADER, InternalClient};
use crate::chatapi;
use crate::engine;
use crate::oai;
use crate::store::Store;
use crate::ui;

pub type Body = BoxBody<Bytes, hyper::Error>;

/// En-tête posé par le tunnel du relais côté Go (`viaTunnelHeader`).
pub const VIA_TUNNEL: &str = "x-loki-via";

const HOP_BY_HOP: [&str; 8] = [
    "connection",
    "keep-alive",
    "proxy-connection",
    "proxy-authenticate",
    "te",
    "trailer",
    "transfer-encoding",
    "upgrade",
];

/// Réglages de la façade.
pub struct Config {
    /// Serveur Go (`loki web`), en écoute locale.
    pub backend: SocketAddr,
    /// Jeton de l'API interne du Go (`LOKI_INTERNAL_TOKEN`).
    pub internal_token: String,
    pub host_env: HostEnv,
    /// `$LOKI_HOME` et service moteur, pour les routes portées.
    pub engine: engine::Paths,
    /// Conversation tenue par la façade (`LOKI_CHAT=rust`) : ses routes sont
    /// servies ici ; `None` : relayées au Go.
    pub chat: Option<Arc<crate::chat::generate::ChatService>>,
    /// Routes des modèles tenues par la façade (`LOKI_MODELS=rust`) : servies
    /// ici ; `None` : relayées au Go.
    pub models: Option<Arc<crate::models::Models>>,
    /// Dictée vocale tenue par la façade (`LOKI_DICTATE=rust`) : servie ici ;
    /// `None` : relayée au Go.
    pub dictate: Option<Arc<crate::dictate::Dictation>>,
    /// Réglages partagés tenus par la façade (`LOKI_SETTINGS=rust`).
    pub settings: bool,
    /// Fichiers de la discussion tenus par la façade (`LOKI_FILES=rust`).
    pub files: Option<Arc<crate::convfiles::ConvFiles>>,
    /// Notifications Web Push tenues par la façade (`LOKI_PUSH=rust`).
    pub push: Option<Arc<crate::push::Push>>,
}

struct Proxy {
    client: Client<HttpConnector, Incoming>,
    /// Vers le moteur (llama-server / Strata), pour `/v1`.
    engine: oai::EngineClient,
    backend: SocketAddr,
    ctx: api::Ctx,
}

/// Sert la façade sur `listener` jusqu'à ce que `shutdown` se termine.
pub async fn serve(
    listener: TcpListener,
    cfg: Config,
    shutdown: impl Future<Output = ()>,
) -> std::io::Result<()> {
    let client = Client::builder(TokioExecutor::new()).build_http();
    let proxy = Arc::new(Proxy {
        client,
        engine: Client::builder(TokioExecutor::new()).build_http(),
        backend: cfg.backend,
        ctx: api::Ctx {
            store: Store::new(cfg.backend, cfg.internal_token.clone()),
            internal: InternalClient::new(cfg.backend, cfg.internal_token.clone()),
            host_env: cfg.host_env,
            engine: cfg.engine,
            chat: cfg.chat,
            models: cfg.models,
            dictate: cfg.dictate,
            settings: cfg.settings,
            files: cfg.files,
            push: cfg.push,
            internal_token: cfg.internal_token.clone(),
        },
    });
    tokio::pin!(shutdown);
    loop {
        tokio::select! {
            _ = &mut shutdown => return Ok(()),
            res = listener.accept() => {
                let stream = match res {
                    Ok((s, _)) => s,
                    Err(e) => {
                        // EMFILE & co : on ne lâche pas l'écoute pour autant.
                        eprintln!("[loki-rs] accept : {e}");
                        tokio::time::sleep(Duration::from_millis(50)).await;
                        continue;
                    }
                };
                let _ = stream.set_nodelay(true);
                let p = proxy.clone();
                tokio::spawn(async move {
                    let svc = service_fn(move |req| {
                        let p = p.clone();
                        async move { Ok::<_, Infallible>(p.handle(req).await) }
                    });
                    let _ = hyper::server::conn::http1::Builder::new()
                        .timer(hyper_util::rt::TokioTimer::new())
                        .header_read_timeout(Duration::from_secs(10))
                        .serve_connection(TokioIo::new(stream), svc)
                        .with_upgrades()
                        .await;
                });
            }
        }
    }
}

impl Proxy {
    async fn handle(&self, mut req: Request<Incoming>) -> Response<Body> {
        // Le Go (et lui seul : jeton tiré à chaque démarrage) remet à la
        // façade le trafic des lots qu'elle tient et qui lui arrive sans passer
        // par elle.
        let from_go = self.ctx.holds_any() && self.ctx.from_go(req.headers());
        // Jamais de confiance accordée sur la foi d'un en-tête du client :
        // ni « je viens du tunnel », ni « je suis la façade ».
        req.headers_mut().remove(VIA_TUNNEL);
        req.headers_mut().remove(INTERNAL_HEADER);

        let path = req.uri().path();
        // L'API interne de la façade : pour le Go seulement.
        if from_go
            && let Some(chat) = &self.ctx.chat
            && chatapi::is_internal(path)
        {
            return chatapi::serve_internal(chat, req).await;
        }
        if from_go
            && let Some(p) = &self.ctx.push
            && path == crate::push::INTERNAL_SEND
        {
            return crate::push::serve_internal(self.ctx.store.clone(), p.clone(), req).await;
        }
        if from_go
            && let Some(d) = &self.ctx.dictate
            && path == crate::dictate::INTERNAL_STOP
        {
            d.shutdown_fast().await;
            return plain(StatusCode::NO_CONTENT, "");
        }
        // L'API interne du Go n'est jamais joignable de l'extérieur.
        if path.starts_with("/internal/") {
            return plain(StatusCode::NOT_FOUND, "404 page not found");
        }
        if let Some(resp) = ui::serve(req.method(), path, req.headers()) {
            return resp;
        }
        // Surface OpenAI : relayée au moteur, décisions demandées au Go.
        if path.starts_with("/v1/") {
            return oai::serve(&self.ctx, &self.engine, req).await;
        }
        if api::handles(req.method(), path) {
            return api::serve(&self.ctx, req).await;
        }
        if let Some(chat) = &self.ctx.chat
            && chatapi::handles(path)
        {
            return chatapi::serve(&self.ctx, chat, req, from_go).await;
        }
        if let Some(p) = &self.ctx.push
            && crate::push::handles(path)
        {
            return crate::push::serve(&self.ctx, p, req, from_go).await;
        }
        if let Some(f) = &self.ctx.files
            && crate::convfiles::handles(path)
        {
            return crate::convfiles::serve(&self.ctx, f, req, from_go).await;
        }
        if self.ctx.settings && crate::settings::handles(path) {
            return crate::settings::serve(&self.ctx, req, from_go).await;
        }
        if let Some(d) = &self.ctx.dictate
            && crate::dictate::handles(path)
        {
            return crate::dictate::serve(&self.ctx, d, req, from_go).await;
        }
        if let Some(m) = &self.ctx.models
            && crate::models::handles(path)
        {
            return crate::models::serve(&self.ctx, m, req, from_go).await;
        }
        self.forward(req).await
    }

    /// Relais vers le Go (HTTP, SSE, WebSocket).
    async fn forward(&self, mut req: Request<Incoming>) -> Response<Body> {
        let upgrade = upgrade_protocol(req.headers());
        strip_hop_by_hop(req.headers_mut());
        let client_upgrade = upgrade.as_ref().map(|proto| {
            let h = req.headers_mut();
            h.insert(header::CONNECTION, HeaderValue::from_static("upgrade"));
            h.insert(header::UPGRADE, proto.clone());
            hyper::upgrade::on(&mut req)
        });

        let pq = req
            .uri()
            .path_and_query()
            .map(|p| p.as_str())
            .unwrap_or("/");
        let uri: Uri = match format!("http://{}{}", self.backend, pq).parse() {
            Ok(u) => u,
            Err(_) => return plain(StatusCode::BAD_REQUEST, "URI invalide"),
        };
        *req.uri_mut() = uri;

        let mut resp = match self.client.request(req).await {
            Ok(r) => r,
            Err(e) => {
                eprintln!("[loki-rs] backend Go injoignable : {e}");
                return plain(
                    StatusCode::BAD_GATEWAY,
                    "Loki (serveur Go) redémarre — réessaie dans un instant.",
                );
            }
        };

        if let Some(client_upgrade) = client_upgrade
            && resp.status() == StatusCode::SWITCHING_PROTOCOLS
        {
            let backend_upgrade = hyper::upgrade::on(&mut resp);
            tokio::spawn(async move {
                match (client_upgrade.await, backend_upgrade.await) {
                    (Ok(a), Ok(b)) => {
                        let (mut a, mut b) = (TokioIo::new(a), TokioIo::new(b));
                        let _ = tokio::io::copy_bidirectional(&mut a, &mut b).await;
                    }
                    (Err(e), _) | (_, Err(e)) => eprintln!("[loki-rs] upgrade : {e}"),
                }
            });
            // 101 : Connection/Upgrade font partie de la poignée de main.
            return resp.map(BodyExt::boxed);
        }
        strip_hop_by_hop(resp.headers_mut());
        resp.map(BodyExt::boxed)
    }
}

/// Protocole demandé si la requête est une demande d'upgrade (WebSocket).
fn upgrade_protocol(h: &HeaderMap) -> Option<HeaderValue> {
    let wants = h
        .get_all(header::CONNECTION)
        .iter()
        .filter_map(|v| v.to_str().ok())
        .flat_map(|s| s.split(','))
        .any(|t| t.trim().eq_ignore_ascii_case("upgrade"));
    if wants {
        h.get(header::UPGRADE).cloned()
    } else {
        None
    }
}

/// Retire les en-têtes propres à une connexion (RFC 9110 §7.6.1), y compris
/// ceux que `Connection` désigne.
pub fn strip_hop_by_hop(h: &mut HeaderMap) {
    let listed: Vec<HeaderName> = h
        .get_all(header::CONNECTION)
        .iter()
        .filter_map(|v| v.to_str().ok())
        .flat_map(|s| s.split(','))
        .filter_map(|t| HeaderName::from_bytes(t.trim().as_bytes()).ok())
        .collect();
    for n in listed {
        h.remove(n);
    }
    for n in HOP_BY_HOP {
        h.remove(n);
    }
}

fn plain(status: StatusCode, msg: &'static str) -> Response<Body> {
    let mut r = Response::new(
        Full::new(Bytes::from_static(msg.as_bytes()))
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
    fn retire_les_hop_by_hop_et_ceux_listes() {
        let mut h = HeaderMap::new();
        h.insert(
            "connection",
            HeaderValue::from_static("keep-alive, x-perso"),
        );
        h.insert("keep-alive", HeaderValue::from_static("timeout=5"));
        h.insert("x-perso", HeaderValue::from_static("1"));
        h.insert("transfer-encoding", HeaderValue::from_static("chunked"));
        h.insert("authorization", HeaderValue::from_static("Bearer k"));
        strip_hop_by_hop(&mut h);
        assert_eq!(h.len(), 1);
        assert!(h.contains_key("authorization"));
    }

    #[test]
    fn detecte_l_upgrade() {
        let mut h = HeaderMap::new();
        h.insert(
            "connection",
            HeaderValue::from_static("keep-alive, Upgrade"),
        );
        h.insert("upgrade", HeaderValue::from_static("websocket"));
        assert_eq!(
            upgrade_protocol(&h),
            Some(HeaderValue::from_static("websocket"))
        );
        h.insert("connection", HeaderValue::from_static("keep-alive"));
        assert_eq!(upgrade_protocol(&h), None);
    }
}
