//! Authentification de l'API de pilotage, portée de `web_auth.go`
//! (`requireWebAuth`, `crossSiteReject`, `localHostName`, `checkBearer`).
//!
//! La clé de pilotage vit dans la base du Go : elle est demandée à son API
//! interne (`/internal/v1/auth`, voir `web_internal.go`) à CHAQUE requête,
//! comme le Go la relit à chaque requête — un changement de clé prend effet
//! immédiatement. Si l'API interne ne répond pas proprement, on FERME (503) :
//! une panne ne doit jamais désarmer l'authentification.
//!
//! `X-Loki-Via` (trafic du tunnel) n'est jamais consulté ici : la façade
//! l'efface à l'entrée, et le tunnel ne passe pas par elle.

use bytes::Bytes;
use http_body_util::{BodyExt, Empty, Full};
use hyper::header::{self, HeaderMap, HeaderValue};
use hyper::{Request, Response, StatusCode};
use hyper_util::client::legacy::{Client, connect::HttpConnector};
use hyper_util::rt::TokioExecutor;
use serde::Deserialize;
use std::net::{IpAddr, SocketAddr};
use std::time::Duration;

use crate::proxy::Body;

pub const INTERNAL_HEADER: &str = "x-loki-internal";
pub const INTERNAL_ENV: &str = "LOKI_INTERNAL_TOKEN";

#[derive(Debug, Clone, Deserialize, PartialEq, Eq)]
pub struct AuthInfo {
    pub web_key: String,
    pub version: String,
}

/// Client de l'API interne du Go.
pub struct InternalClient {
    client: Client<HttpConnector, Empty<Bytes>>,
    backend: SocketAddr,
    token: String,
}

impl InternalClient {
    pub fn new(backend: SocketAddr, token: String) -> Self {
        InternalClient {
            client: Client::builder(TokioExecutor::new()).build_http(),
            backend,
            token,
        }
    }

    pub async fn auth(&self) -> Result<AuthInfo, String> {
        let req = Request::get(format!("http://{}/internal/v1/auth", self.backend))
            .header(INTERNAL_HEADER, &self.token)
            .body(Empty::new())
            .map_err(|e| e.to_string())?;
        let fut = async {
            let resp = self.client.request(req).await.map_err(|e| e.to_string())?;
            if resp.status() != StatusCode::OK {
                return Err(format!("API interne : {}", resp.status()));
            }
            let body = resp
                .into_body()
                .collect()
                .await
                .map_err(|e| e.to_string())?;
            serde_json::from_slice(&body.to_bytes()).map_err(|e| e.to_string())
        };
        tokio::time::timeout(Duration::from_secs(5), fut)
            .await
            .map_err(|_| "API interne : délai dépassé".to_string())?
    }
}

/// Ce qu'il faut savoir de l'hôte pour juger qu'un nom est « local ».
#[derive(Debug, Clone, Default)]
pub struct HostEnv {
    /// Nom de la machine (du conteneur), en minuscules.
    pub hostname: Option<String>,
    /// `LOKI_TRUSTED_HOSTS`, en minuscules.
    pub trusted: Vec<String>,
}

impl HostEnv {
    pub fn from_env() -> Self {
        let trusted = std::env::var("LOKI_TRUSTED_HOSTS")
            .unwrap_or_default()
            .split(',')
            .map(|t| t.trim().to_lowercase())
            .filter(|t| !t.is_empty())
            .collect();
        HostEnv {
            hostname: hostname().map(|h| h.to_lowercase()),
            trusted,
        }
    }
}

fn hostname() -> Option<String> {
    // Ce que lit os.Hostname du Go sous Linux.
    let h = std::fs::read_to_string("/proc/sys/kernel/hostname")
        .ok()
        .or_else(|| std::env::var("HOSTNAME").ok())
        .or_else(|| std::env::var("COMPUTERNAME").ok())?;
    let h = h.trim().to_string();
    (!h.is_empty()).then_some(h)
}

/// Vérifie la requête comme `requireWebAuth` : `None` = autorisée, sinon la
/// réponse de refus.
pub fn authorize(
    headers: &HeaderMap,
    info: &Result<AuthInfo, String>,
    env: &HostEnv,
) -> Option<Response<Body>> {
    let info = match info {
        Ok(i) => i,
        Err(e) => {
            eprintln!("[loki-rs] clé de pilotage illisible : {e}");
            return Some(json_error(
                StatusCode::SERVICE_UNAVAILABLE,
                "configuration illisible — réessaie dans un instant",
            ));
        }
    };
    let keyed = !info.web_key.is_empty();
    if let Some(msg) = cross_site_reject(headers, keyed, env) {
        return Some(json_error(StatusCode::FORBIDDEN, &msg));
    }
    if !keyed || check_bearer(headers, &info.web_key) {
        return None;
    }
    let mut r = json_error(StatusCode::UNAUTHORIZED, "non autorisé");
    r.headers_mut().insert(
        header::WWW_AUTHENTICATE,
        HeaderValue::from_static(r#"Bearer realm="loki""#),
    );
    Some(r)
}

/// Raison du refus d'une requête venue d'un site tiers, ou `None`.
pub fn cross_site_reject(headers: &HeaderMap, keyed: bool, env: &HostEnv) -> Option<String> {
    let get = |k: header::HeaderName| headers.get(k).and_then(|v| v.to_str().ok());
    let host = get(header::HOST).unwrap_or("");
    if get(header::HeaderName::from_static("sec-fetch-site"))
        .is_some_and(|v| v.eq_ignore_ascii_case("cross-site"))
    {
        return Some("requête d'un site tiers refusée".into());
    }
    match get(header::ORIGIN) {
        Some("null") => return Some("origine non autorisée".into()),
        Some(o) if !o.is_empty() && !origin_host(o).eq_ignore_ascii_case(host) => {
            return Some(format!("origine non autorisée : {o}"));
        }
        _ => {}
    }
    if !keyed && !local_host_name(host, env) {
        return Some(format!(
            "hôte « {host} » non autorisé sans clé de pilotage — définis-en une \
             (loki set-web-key) ou ajoute ce nom à LOKI_TRUSTED_HOSTS"
        ));
    }
    None
}

/// Partie « hôte[:port] » d'une origine, comme `url.Parse(o).Host` en Go.
fn origin_host(o: &str) -> &str {
    let rest = match o.find("://") {
        Some(i) => &o[i + 3..],
        None => match o.strip_prefix("//") {
            Some(r) => r,
            None => return "",
        },
    };
    let end = rest.find(['/', '?', '#']).unwrap_or(rest.len());
    let auth = &rest[..end];
    auth.rsplit_once('@').map(|(_, h)| h).unwrap_or(auth)
}

/// L'hôte désigne-t-il la machine ou le réseau local (et non un domaine
/// public, seul utilisable pour un DNS rebinding) ?
pub fn local_host_name(hostport: &str, env: &HostEnv) -> bool {
    let h = split_host(hostport).unwrap_or(hostport);
    let h = h.trim_matches(['[', ']', '.']).to_lowercase();
    if h.is_empty() || h.parse::<IpAddr>().is_ok() || !h.contains('.') {
        return true;
    }
    const SUFFIXES: [&str; 7] = [
        ".localhost",
        ".local",
        ".lan",
        ".home",
        ".internal",
        ".home.arpa",
        ".localdomain",
    ];
    if SUFFIXES.iter().any(|s| h.ends_with(s)) {
        return true;
    }
    if let Some(me) = env.hostname.as_deref().filter(|m| !m.is_empty())
        && (h == me || h.starts_with(&format!("{me}.")))
    {
        return true;
    }
    env.trusted.contains(&h)
}

/// `net.SplitHostPort` réduit à ce qui sert ici : l'hôte, si `s` porte un port.
fn split_host(s: &str) -> Option<&str> {
    if let Some(rest) = s.strip_prefix('[') {
        let (host, after) = rest.split_once(']')?;
        return after.starts_with(':').then_some(host);
    }
    let (host, port) = s.split_once(':')?;
    (!port.contains(':')).then_some(host)
}

/// `Authorization: Bearer <clé>`, comparaison en temps constant.
pub fn check_bearer(headers: &HeaderMap, key: &str) -> bool {
    let Some(got) = headers
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "))
    else {
        return false;
    };
    constant_time_eq(got.trim().as_bytes(), key.as_bytes())
}

fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    a.iter().zip(b).fold(0u8, |acc, (x, y)| acc | (x ^ y)) == 0
}

/// JSON comme `sendJSON` du Go (json.Encoder : saut de ligne final).
pub fn json<T: serde::Serialize + ?Sized>(status: StatusCode, v: &T) -> Response<Body> {
    let mut s = serde_json::to_string(v).unwrap_or_else(|_| "{}".into());
    s.push('\n');
    let mut r = Response::new(Full::new(Bytes::from(s)).map_err(|n| match n {}).boxed());
    *r.status_mut() = status;
    r.headers_mut().insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("application/json"),
    );
    r
}

fn json_error(status: StatusCode, msg: &str) -> Response<Body> {
    json(status, &serde_json::json!({ "error": msg }))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn env() -> HostEnv {
        HostEnv {
            hostname: Some("tour-unraid".into()),
            trusted: vec!["loki.exemple.fr".into()],
        }
    }

    fn headers(host: &str, origin: &str, fetch_site: &str) -> HeaderMap {
        let mut h = HeaderMap::new();
        h.insert(header::HOST, host.parse().unwrap());
        if !origin.is_empty() {
            h.insert(header::ORIGIN, origin.parse().unwrap());
        }
        if !fetch_site.is_empty() {
            h.insert("sec-fetch-site", fetch_site.parse().unwrap());
        }
        h
    }

    /// Repris un pour un de TestCrossSiteReject (web_auth_crosssite_test.go),
    /// hors cas « tunnel » (le tunnel ne traverse pas la façade).
    #[test]
    fn cross_site_comme_le_go() {
        #[rustfmt::skip]
        let cas: &[(&str, &str, &str, &str, bool, bool)] = &[
            // nom, host, origin, sec-fetch-site, keyed, refusé
            ("curl sur IP", "192.168.1.10:8090", "", "", false, false),
            ("localhost", "localhost:8090", "http://localhost:8090", "", false, false),
            ("nom sans point", "tower:8090", "http://tower:8090", "", false, false),
            ("nom .local", "tower.local:8090", "", "", false, false),
            ("origine tierce", "192.168.1.10:8090", "http://evil.example", "", false, true),
            ("origine null", "localhost:8090", "null", "", false, true),
            ("sec-fetch cross-site", "localhost:8090", "", "cross-site", false, true),
            ("rebinding sans clé", "evil.example:8090", "http://evil.example:8090", "", false, true),
            ("domaine avec clé", "evil.example:8090", "", "", true, false),
            ("domaine de confiance", "loki.exemple.fr", "https://loki.exemple.fr", "", false, false),
        ];
        for (nom, host, origin, fs, keyed, refuse) in cas {
            let got = cross_site_reject(&headers(host, origin, fs), *keyed, &env()).is_some();
            assert_eq!(got, *refuse, "{nom}");
        }
    }

    #[test]
    fn messages_identiques_au_go() {
        let m = cross_site_reject(&headers("evil.example", "", ""), false, &env()).unwrap();
        assert_eq!(
            m,
            "hôte « evil.example » non autorisé sans clé de pilotage — définis-en une \
             (loki set-web-key) ou ajoute ce nom à LOKI_TRUSTED_HOSTS"
        );
        let m = cross_site_reject(&headers("x", "http://evil", ""), true, &env()).unwrap();
        assert_eq!(m, "origine non autorisée : http://evil");
    }

    #[test]
    fn noms_locaux() {
        let e = env();
        for h in [
            "",
            "10.0.0.2",
            "[::1]:8090",
            "::1",
            "tower",
            "nas.lan",
            "x.home.arpa:443",
            "TOUR-UNRAID",
            "tour-unraid.maison.fr",
            "loki.exemple.fr:443",
            "localhost.",
        ] {
            assert!(local_host_name(h, &e), "{h} devrait être local");
        }
        for h in [
            "evil.example",
            "evil.example:8090",
            "loki.exemple.fr.evil.com",
        ] {
            assert!(!local_host_name(h, &e), "{h} ne devrait pas être local");
        }
    }

    #[test]
    fn hote_d_origine() {
        assert_eq!(origin_host("https://a.b:8090"), "a.b:8090");
        assert_eq!(origin_host("http://u:p@a.b/x?y"), "a.b");
        assert_eq!(origin_host("//a.b"), "a.b");
        assert_eq!(origin_host("a.b"), "");
    }

    #[test]
    fn bearer() {
        let mut h = HeaderMap::new();
        assert!(!check_bearer(&h, "k"));
        h.insert(header::AUTHORIZATION, "Bearer k ".parse().unwrap());
        assert!(check_bearer(&h, "k"));
        assert!(!check_bearer(&h, "kk"));
        h.insert(header::AUTHORIZATION, "bearer k".parse().unwrap());
        assert!(!check_bearer(&h, "k"));
    }

    #[test]
    fn autorisation() {
        let e = env();
        let ok = |k: &str| {
            Ok(AuthInfo {
                web_key: k.into(),
                version: "0.15.0".into(),
            })
        };
        let h = headers("localhost:8090", "", "");
        assert!(authorize(&h, &ok(""), &e).is_none());
        let r = authorize(&h, &ok("s"), &e).unwrap();
        assert_eq!(r.status(), StatusCode::UNAUTHORIZED);
        assert_eq!(
            r.headers()[header::WWW_AUTHENTICATE],
            r#"Bearer realm="loki""#
        );
        let mut hb = h.clone();
        hb.insert(header::AUTHORIZATION, "Bearer s".parse().unwrap());
        assert!(authorize(&hb, &ok("s"), &e).is_none());
        let r = authorize(&hb, &Err("panne".into()), &e).unwrap();
        assert_eq!(r.status(), StatusCode::SERVICE_UNAVAILABLE);
        let r = authorize(&headers("evil.example", "", ""), &ok(""), &e).unwrap();
        assert_eq!(r.status(), StatusCode::FORBIDDEN);
    }
}
