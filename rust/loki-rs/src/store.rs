//! Accès à la base de Loki À TRAVERS le Go (`/internal/v1/store`, voir
//! `web_internal.go`). Le Go reste l'unique propriétaire du fichier bbolt
//! jusqu'au lot qui le migrera ; ce module est l'unique point de passage
//! côté Rust, pour que cette migration ne touche qu'ici.

use bytes::Bytes;
use http_body_util::{BodyExt, Full};
use hyper::{Method, Request, StatusCode};
use hyper_util::client::legacy::{Client, connect::HttpConnector};
use hyper_util::rt::TokioExecutor;
use std::collections::HashMap;
use std::net::SocketAddr;
use std::time::Duration;

use crate::auth::INTERNAL_HEADER;

pub const BK_CONFIG: &str = "config";
pub const BK_PREFS: &str = "prefs";
pub const BK_STATE: &str = "state";

#[derive(Clone)]
pub struct Store {
    client: Client<HttpConnector, Full<Bytes>>,
    backend: SocketAddr,
    token: String,
}

impl Store {
    pub fn new(backend: SocketAddr, token: String) -> Self {
        Store {
            client: Client::builder(TokioExecutor::new()).build_http(),
            backend,
            token,
        }
    }

    async fn call(
        &self,
        method: Method,
        path: &str,
        body: Vec<u8>,
    ) -> Result<(StatusCode, Bytes), String> {
        let req = Request::builder()
            .method(method)
            .uri(format!("http://{}{path}", self.backend))
            .header(INTERNAL_HEADER, &self.token)
            .body(Full::new(Bytes::from(body)))
            .map_err(|e| e.to_string())?;
        let fut = async {
            let resp = self.client.request(req).await.map_err(|e| e.to_string())?;
            let status = resp.status();
            let body = resp
                .into_body()
                .collect()
                .await
                .map_err(|e| e.to_string())?
                .to_bytes();
            Ok((status, body))
        };
        tokio::time::timeout(Duration::from_secs(10), fut)
            .await
            .map_err(|_| "store : délai dépassé".to_string())?
    }

    /// Valeur brute ; `Ok(None)` = absente, `Err` = pas pu lire (à distinguer
    /// pour les secrets : comme `getBytesErr`).
    pub async fn get(&self, bucket: &str, key: &str) -> Result<Option<Vec<u8>>, String> {
        let path = format!("/internal/v1/store?bucket={}&key={}", enc(bucket), enc(key));
        match self.call(Method::GET, &path, Vec::new()).await? {
            (StatusCode::OK, b) => Ok(Some(b.to_vec())),
            (StatusCode::NOT_FOUND, _) => Ok(None),
            (s, b) => Err(format!("store {s} : {}", String::from_utf8_lossy(&b))),
        }
    }

    /// Comme `getStr` du Go : erreur ou absence → "".
    pub async fn get_str(&self, bucket: &str, key: &str) -> String {
        match self.get(bucket, key).await {
            Ok(Some(v)) => String::from_utf8_lossy(&v).into_owned(),
            _ => String::new(),
        }
    }

    pub async fn put(&self, bucket: &str, key: &str, val: Vec<u8>) -> Result<(), String> {
        let path = format!("/internal/v1/store?bucket={}&key={}", enc(bucket), enc(key));
        match self.call(Method::PUT, &path, val).await? {
            (StatusCode::NO_CONTENT, _) => Ok(()),
            (s, b) => Err(format!("store {s} : {}", String::from_utf8_lossy(&b))),
        }
    }

    pub async fn del(&self, bucket: &str, key: &str) -> Result<(), String> {
        let path = format!("/internal/v1/store?bucket={}&key={}", enc(bucket), enc(key));
        match self.call(Method::DELETE, &path, Vec::new()).await? {
            (StatusCode::NO_CONTENT, _) => Ok(()),
            (s, b) => Err(format!("store {s} : {}", String::from_utf8_lossy(&b))),
        }
    }

    pub async fn all(&self, bucket: &str) -> Result<HashMap<String, String>, String> {
        let path = format!("/internal/v1/store/all?bucket={}", enc(bucket));
        match self.call(Method::GET, &path, Vec::new()).await? {
            (StatusCode::OK, b) => serde_json::from_slice(&b).map_err(|e| e.to_string()),
            (s, b) => Err(format!("store {s} : {}", String::from_utf8_lossy(&b))),
        }
    }

    /// `ReadConfig()` du Go : la configuration du moteur ; base illisible →
    /// configuration vide (comme `allKV`).
    pub async fn config(&self) -> HashMap<String, String> {
        self.all(BK_CONFIG).await.unwrap_or_default()
    }
}

/// Encodage d'un paramètre de requête (RFC 3986, caractères non réservés
/// conservés).
pub fn enc(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for b in s.bytes() {
        if b.is_ascii_alphanumeric() || matches!(b, b'-' | b'_' | b'.' | b'~') {
            out.push(b as char);
        } else {
            out.push_str(&format!("%{b:02X}"));
        }
    }
    out
}

#[cfg(test)]
mod tests {
    #[test]
    fn encodage() {
        assert_eq!(super::enc("sysprompt:a b/é"), "sysprompt%3Aa%20b%2F%C3%A9");
        assert_eq!(super::enc("PORT"), "PORT");
    }
}
