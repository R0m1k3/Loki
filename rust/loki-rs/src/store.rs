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

    /// Appel JSON à une autre route de l'API interne du Go (`/internal/v1/…`).
    /// Code HTTP autre que 2xx → `Err`.
    pub async fn internal_json(
        &self,
        method: Method,
        path_and_query: &str,
        body: Option<&serde_json::Value>,
    ) -> Result<serde_json::Value, String> {
        let body = body
            .map(|b| serde_json::to_vec(b).unwrap_or_default())
            .unwrap_or_default();
        let (status, bytes) = self.call(method, path_and_query, body).await?;
        if !status.is_success() {
            return Err(format!(
                "{path_and_query} : {status} {}",
                String::from_utf8_lossy(&bytes)
            ));
        }
        if bytes.is_empty() {
            return Ok(serde_json::Value::Null);
        }
        serde_json::from_slice(&bytes).map_err(|e| e.to_string())
    }

    /// POST JSON à l'API interne, statut compris (refus métier à distinguer :
    /// 409, 410…). Corps vide ou illisible → `Value::Null`.
    pub async fn internal_post(
        &self,
        path: &str,
        body: &serde_json::Value,
    ) -> Result<(StatusCode, serde_json::Value), String> {
        let (status, bytes) = self
            .call(
                Method::POST,
                path,
                serde_json::to_vec(body).unwrap_or_default(),
            )
            .await?;
        let v = serde_json::from_slice(&bytes).unwrap_or(serde_json::Value::Null);
        Ok((status, v))
    }

    /// Scelle un blob avec la clé de la mémoire (gardée par le Go). Clair
    /// rendu tel quel si le chiffrement est coupé.
    pub async fn seal(&self, plain: Vec<u8>) -> Result<Vec<u8>, CryptError> {
        self.crypt("/internal/v1/crypt/seal", plain).await
    }

    /// Ouvre un blob (un contenu non chiffré passe tel quel).
    pub async fn open(&self, raw: Vec<u8>) -> Result<Vec<u8>, CryptError> {
        self.crypt("/internal/v1/crypt/open", raw).await
    }

    async fn crypt(&self, path: &str, body: Vec<u8>) -> Result<Vec<u8>, CryptError> {
        match self.call(Method::POST, path, body).await {
            Ok((StatusCode::OK, b)) => Ok(b.to_vec()),
            Ok((StatusCode::LOCKED, _)) => Err(CryptError::Locked),
            Ok((s, b)) => Err(CryptError::Other(format!(
                "{path} : {s} {}",
                String::from_utf8_lossy(&b)
            ))),
            Err(e) => Err(CryptError::Other(e)),
        }
    }

    /// `getStoreBytesErr` : lit et ouvre une valeur scellée. Absente, ou
    /// illisible (mémoire verrouillée, autre clé) → `Ok(None)` ; `Err`
    /// seulement si la base n'a pas pu être lue (« rien » et « pas pu lire »
    /// ne veulent pas dire la même chose).
    pub async fn get_open(&self, bucket: &str, key: &str) -> Result<Option<Vec<u8>>, String> {
        match self.get(bucket, key).await? {
            Some(raw) if !raw.is_empty() => Ok(self.open(raw).await.ok()),
            _ => Ok(None),
        }
    }

    /// `putStoreBytes` : scelle puis écrit. Mémoire verrouillée : rien n'est
    /// écrit (jamais en clair).
    pub async fn put_seal(
        &self,
        bucket: &str,
        key: &str,
        plain: Vec<u8>,
    ) -> Result<(), CryptError> {
        let sealed = self.seal(plain).await?;
        self.put(bucket, key, sealed)
            .await
            .map_err(CryptError::Other)
    }

    /// Exécute un appel d'outil par le Go (`/internal/v1/tool/exec`) : même
    /// fonction que sa boucle de chat. `caps` : objet JSON des capacités du
    /// tour (champs Go : Agent, Internet, Mem, Code, Role, ComputerUse).
    pub async fn tool_exec(
        &self,
        name: &str,
        args: &serde_json::Value,
        caps: &serde_json::Value,
    ) -> Result<serde_json::Value, String> {
        self.internal_json(
            Method::POST,
            "/internal/v1/tool/exec",
            Some(&serde_json::json!({ "name": name, "args": args, "caps": caps })),
        )
        .await
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

/// Échec de chiffrement / déchiffrement.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum CryptError {
    /// Mémoire verrouillée : attendre le déverrouillage, ne rien écrire.
    Locked,
    Other(String),
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
