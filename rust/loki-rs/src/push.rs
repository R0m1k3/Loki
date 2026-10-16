//! Notifications Web Push — port de `push.go` et `web_push.go` (lot 12 de la
//! migration), tenues par la façade quand `LOKI_PUSH=rust` (expérimental ; le
//! Go par défaut) :
//!
//! - `GET /api/push/key` : clé publique VAPID (générée une fois, stable) ;
//! - `POST /api/push/subscribe` : enregistre un abonnement du navigateur ;
//! - `POST /api/push/unsubscribe` : le retire ;
//! - `POST /internal/facade/v1/push/send` (le Go, jeton interne) : pousse
//!   « réponse prête » à tous les abonnés — le Go y délègue ses envois de fin
//!   de tour et de fin de tâche.
//!
//! Le chiffrement est celui de RFC 8291 (`aes128gcm`, enregistrement complété
//! à 4096 octets comme webpush-go), signé VAPID (JWT ES256, 12 h). Mêmes clés
//! en base que le Go (`push_vapid_priv`, `push_vapid_pub`, `push_subs`) : un
//! abonnement pris d'un côté vaut de l'autre.
//!
//! Écart assumé : messages des journaux d'envoi au format des crates Rust.

use std::time::{Duration, SystemTime, UNIX_EPOCH};

use hyper::body::Incoming;
use hyper::{Request, Response, StatusCode};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use crate::api::Ctx;
use crate::auth::json as send_json;
use crate::proxy::Body;
use crate::store::{BK_STATE, Store};

const ST_PRIV: &str = "push_vapid_priv";
const ST_PUB: &str = "push_vapid_pub";
const ST_SUBS: &str = "push_subs";
/// Taille d'enregistrement (`MaxRecordSize` de webpush-go).
pub const RECORD_SIZE: usize = 4096;
/// Sujet VAPID : l'adresse nue, « mailto: » ajouté à la signature.
#[cfg_attr(not(feature = "tls"), allow(dead_code))]
const SUBSCRIBER: &str = "mail@nathaninline.com";
/// Périmé après 2 min : une notification tardive n'a pas de sens.
const TTL: u32 = 120;

/// La route interne d'envoi.
pub const INTERNAL_SEND: &str = "/internal/facade/v1/push/send";

/// Routes servies ici.
pub fn handles(path: &str) -> bool {
    matches!(
        path,
        "/api/push/key" | "/api/push/subscribe" | "/api/push/unsubscribe"
    )
}

/// Clés d'un abonnement (`webpush.Keys`).
#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
pub struct Keys {
    #[serde(default)]
    pub auth: String,
    #[serde(default)]
    pub p256dh: String,
}

/// Un abonnement du navigateur (`pushSub`).
#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
pub struct PushSub {
    #[serde(default)]
    pub endpoint: String,
    #[serde(default)]
    pub keys: Keys,
}

/// `fmtDurFR` : « 42s », « 3 mn 05s », « 1 h 12 mn ».
pub fn fmt_dur_fr(ms: i64) -> String {
    // d.Round(time.Second) : à la seconde la plus proche, moitié vers le haut.
    let secs = ((ms.max(0) + 500) / 1000).max(0);
    let (h, m, s) = (secs / 3600, (secs % 3600) / 60, secs % 60);
    if h > 0 {
        format!("{h} h {m:02} mn")
    } else if m > 0 {
        format!("{m} mn {s:02}s")
    } else {
        format!("{s}s")
    }
}

// ---------------------------------------------------------------------------
// Chiffrement (RFC 8291) et signature (RFC 8292)
// ---------------------------------------------------------------------------

#[cfg(feature = "tls")]
mod crypto {
    use super::{PushSub, RECORD_SIZE, SUBSCRIBER};
    use base64::Engine;
    use base64::engine::general_purpose::{STANDARD, URL_SAFE, URL_SAFE_NO_PAD};
    use ring::rand::SystemRandom;
    use ring::signature::{ECDSA_P256_SHA256_FIXED_SIGNING, EcdsaKeyPair, KeyPair};
    use ring::{aead, agreement, hkdf};
    use serde_json::json;

    /// `decodeSubscriptionKey` : base64 standard, sinon URL, remplissage ajouté.
    pub fn decode_sub_key(key: &str) -> Result<Vec<u8>, String> {
        let mut k = key.to_string();
        let rem = k.len() % 4;
        if rem != 0 {
            k.push_str(&"=".repeat(4 - rem));
        }
        STANDARD
            .decode(&k)
            .or_else(|_| URL_SAFE.decode(&k))
            .map_err(|e| e.to_string())
    }

    /// `decodeVapidKey` : base64 URL avec ou sans remplissage.
    fn decode_vapid_key(key: &str) -> Result<Vec<u8>, String> {
        URL_SAFE
            .decode(key)
            .or_else(|_| URL_SAFE_NO_PAD.decode(key))
            .map_err(|e| e.to_string())
    }

    struct Len(usize);

    impl hkdf::KeyType for Len {
        fn len(&self) -> usize {
            self.0
        }
    }

    fn hkdf_expand(salt: &[u8], ikm: &[u8], info: &[u8], len: usize) -> Result<Vec<u8>, String> {
        let prk = hkdf::Salt::new(hkdf::HKDF_SHA256, salt).extract(ikm);
        let parts = [info];
        let okm = prk
            .expand(&parts, Len(len))
            .map_err(|_| "hkdf".to_string())?;
        let mut out = vec![0u8; len];
        okm.fill(&mut out).map_err(|_| "hkdf".to_string())?;
        Ok(out)
    }

    /// Le corps chiffré d'une notification : en-tête `aes128gcm` (sel, taille
    /// d'enregistrement, clé publique éphémère) puis le message complété de
    /// `0x02` et de zéros jusqu'à remplir l'enregistrement, comme webpush-go.
    ///
    /// `local_public` et `shared` : clé publique éphémère et secret ECDH —
    /// tirés au hasard en production, imposés par les tests. `pad` : complété
    /// jusqu'à remplir l'enregistrement (webpush-go), ou pas (exemple de la
    /// RFC 8291).
    #[allow(clippy::too_many_arguments)]
    pub fn encrypt_with(
        message: &[u8],
        auth: &[u8],
        ua_public: &[u8],
        salt: &[u8; 16],
        local_public: &[u8],
        shared: &[u8],
        record_size: usize,
        pad: bool,
    ) -> Result<Vec<u8>, String> {
        let mut info = b"WebPush: info\0".to_vec();
        info.extend_from_slice(ua_public);
        info.extend_from_slice(local_public);
        let ikm = hkdf_expand(auth, shared, &info, 32)?;
        let cek = hkdf_expand(salt, &ikm, b"Content-Encoding: aes128gcm\0", 16)?;
        let nonce = hkdf_expand(salt, &ikm, b"Content-Encoding: nonce\0", 12)?;
        let mut header = salt.to_vec();
        header.extend_from_slice(&(record_size as u32).to_be_bytes());
        header.push(local_public.len() as u8);
        header.extend_from_slice(local_public);
        let record_len = record_size - 16;
        let mut data = message.to_vec();
        data.push(0x02);
        let target = record_len.saturating_sub(header.len());
        if data.len() > target {
            return Err("payload has exceeded the maximum length".into());
        }
        if pad {
            data.resize(target, 0);
        }
        let key = aead::LessSafeKey::new(
            aead::UnboundKey::new(&aead::AES_128_GCM, &cek).map_err(|_| "aes".to_string())?,
        );
        let nonce =
            aead::Nonce::try_assume_unique_for_key(&nonce).map_err(|_| "nonce".to_string())?;
        key.seal_in_place_append_tag(nonce, aead::Aad::empty(), &mut data)
            .map_err(|_| "chiffrement".to_string())?;
        header.extend_from_slice(&data);
        Ok(header)
    }

    /// [`encrypt_with`] avec un sel et une clé éphémère tirés au hasard.
    pub fn encrypt(message: &[u8], sub: &PushSub) -> Result<Vec<u8>, String> {
        let auth = decode_sub_key(&sub.keys.auth)?;
        let ua = decode_sub_key(&sub.keys.p256dh)?;
        let rng = SystemRandom::new();
        let mut salt = [0u8; 16];
        ring::rand::SecureRandom::fill(&rng, &mut salt).map_err(|_| "aléa".to_string())?;
        let local = agreement::EphemeralPrivateKey::generate(&agreement::ECDH_P256, &rng)
            .map_err(|_| "clé éphémère".to_string())?;
        let local_pub = local
            .compute_public_key()
            .map_err(|_| "clé éphémère".to_string())?
            .as_ref()
            .to_vec();
        let peer = agreement::UnparsedPublicKey::new(&agreement::ECDH_P256, ua.clone());
        let shared = agreement::agree_ephemeral(local, &peer, |s| s.to_vec()).map_err(|_| {
            "Unmarshal Error: Public key is not a valid point on the curve".to_string()
        })?;
        encrypt_with(
            message,
            &auth,
            &ua,
            &salt,
            &local_pub,
            &shared,
            RECORD_SIZE,
            true,
        )
    }

    /// `getVAPIDAuthorizationHeader` : « vapid t=<JWT ES256>, k=<clé publique> ».
    pub fn vapid_header(
        endpoint: &str,
        public: &str,
        private: &str,
        now: u64,
    ) -> Result<String, String> {
        let u = reqwest::Url::parse(endpoint).map_err(|e| e.to_string())?;
        let host = match u.port() {
            Some(p) => format!("{}:{p}", u.host_str().unwrap_or("")),
            None => u.host_str().unwrap_or("").to_string(),
        };
        let sub = if SUBSCRIBER.starts_with("https:") {
            SUBSCRIBER.to_string()
        } else {
            format!("mailto:{SUBSCRIBER}")
        };
        // json.Marshal d'une MapClaims : clés triées.
        let claims = json!({ "aud": format!("{}://{host}", u.scheme()), "exp": now + 12 * 3600, "sub": sub });
        let signing_input = format!(
            "{}.{}",
            URL_SAFE_NO_PAD.encode(br#"{"alg":"ES256","typ":"JWT"}"#),
            URL_SAFE_NO_PAD.encode(claims.to_string())
        );
        let priv_raw = decode_vapid_key(private)?;
        let pub_raw = decode_vapid_key(public)?;
        let rng = SystemRandom::new();
        let kp = EcdsaKeyPair::from_private_key_and_public_key(
            &ECDSA_P256_SHA256_FIXED_SIGNING,
            &priv_raw,
            &pub_raw,
            &rng,
        )
        .map_err(|e| format!("clé VAPID illisible : {e}"))?;
        let sig = kp
            .sign(&rng, signing_input.as_bytes())
            .map_err(|_| "signature VAPID".to_string())?;
        Ok(format!(
            "vapid t={signing_input}.{}, k={}",
            URL_SAFE_NO_PAD.encode(sig.as_ref()),
            URL_SAFE_NO_PAD.encode(&pub_raw)
        ))
    }

    /// `GenerateVAPIDKeys` : (privée 32 octets, publique 65 octets), base64 URL
    /// sans remplissage.
    pub fn generate_vapid_keys() -> Result<(String, String), String> {
        let rng = SystemRandom::new();
        let doc = EcdsaKeyPair::generate_pkcs8(&ECDSA_P256_SHA256_FIXED_SIGNING, &rng)
            .map_err(|_| "génération VAPID".to_string())?;
        let kp = EcdsaKeyPair::from_pkcs8(&ECDSA_P256_SHA256_FIXED_SIGNING, doc.as_ref(), &rng)
            .map_err(|_| "génération VAPID".to_string())?;
        let public = kp.public_key().as_ref().to_vec();
        // ECPrivateKey dans le PKCS#8 : « 04 20 » puis les 32 octets du scalaire.
        let d = doc.as_ref();
        let i = d
            .windows(2)
            .position(|w| w == [0x04, 0x20])
            .ok_or("génération VAPID")?;
        let private = d.get(i + 2..i + 34).ok_or("génération VAPID")?.to_vec();
        Ok((
            URL_SAFE_NO_PAD.encode(private),
            URL_SAFE_NO_PAD.encode(public),
        ))
    }
}

#[cfg(not(feature = "tls"))]
mod crypto {
    //! Sans `tls` (pas de ring) : pas de notifications.
    use super::PushSub;
    const NO: &str = "notifications indisponibles (façade construite sans TLS)";
    pub fn decode_sub_key(_: &str) -> Result<Vec<u8>, String> {
        Err(NO.into())
    }
    pub fn encrypt(_: &[u8], _: &PushSub) -> Result<Vec<u8>, String> {
        Err(NO.into())
    }
    pub fn vapid_header(_: &str, _: &str, _: &str, _: u64) -> Result<String, String> {
        Err(NO.into())
    }
    pub fn generate_vapid_keys() -> Result<(String, String), String> {
        Err(NO.into())
    }
}

pub use crypto::*;

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

/// Les notifications tenues par la façade.
pub struct Push {
    client: reqwest::Client,
    /// Génération paresseuse des clés : une seule paire, même sous la course.
    vapid: tokio::sync::Mutex<()>,
    /// Lecture-modification-écriture de la liste des abonnements.
    subs: tokio::sync::Mutex<()>,
}

impl Default for Push {
    fn default() -> Self {
        Self::new()
    }
}

/// Hôte d'un endpoint pour les journaux (jamais le jeton entier).
fn endpoint_host(endpoint: &str) -> String {
    reqwest::Url::parse(endpoint)
        .ok()
        .and_then(|u| {
            u.host_str().map(|h| match u.port() {
                Some(p) => format!("{h}:{p}"),
                None => h.to_string(),
            })
        })
        .filter(|h| !h.is_empty())
        .unwrap_or_else(|| "?".into())
}

impl Push {
    pub fn new() -> Self {
        crate::chat::engine::install_crypto();
        Push {
            client: reqwest::Client::builder()
                .timeout(Duration::from_secs(30))
                .build()
                .unwrap_or_default(),
            vapid: tokio::sync::Mutex::new(()),
            subs: tokio::sync::Mutex::new(()),
        }
    }

    /// `vapidKeys` : la paire, générée et rangée au premier appel.
    pub async fn vapid_keys(&self, store: &Store) -> Result<(String, String), String> {
        let _g = self.vapid.lock().await;
        let private = store.get_str(BK_STATE, ST_PRIV).await;
        let public = store.get_str(BK_STATE, ST_PUB).await;
        if !private.is_empty() && !public.is_empty() {
            return Ok((private, public));
        }
        let (private, public) = generate_vapid_keys()?;
        store
            .put(BK_STATE, ST_PRIV, private.clone().into_bytes())
            .await?;
        store
            .put(BK_STATE, ST_PUB, public.clone().into_bytes())
            .await?;
        Ok((private, public))
    }

    async fn load_subs(store: &Store) -> Vec<PushSub> {
        store
            .get(BK_STATE, ST_SUBS)
            .await
            .ok()
            .flatten()
            .and_then(|b| {
                serde_json::from_slice::<Option<Vec<PushSub>>>(&b)
                    .ok()
                    .flatten()
            })
            .unwrap_or_default()
    }

    async fn save_subs(store: &Store, subs: &[PushSub]) -> Result<(), String> {
        let raw = serde_json::to_vec(subs).map_err(|e| e.to_string())?;
        store.put(BK_STATE, ST_SUBS, raw).await
    }

    /// `addSub` : idempotent par endpoint (les clés sont mises à jour).
    pub async fn add_sub(&self, store: &Store, s: PushSub) -> Result<usize, String> {
        let _g = self.subs.lock().await;
        let mut subs = Self::load_subs(store).await;
        match subs.iter_mut().find(|x| x.endpoint == s.endpoint) {
            Some(x) => *x = s,
            None => subs.push(s),
        }
        Self::save_subs(store, &subs).await?;
        Ok(subs.len())
    }

    /// `removeSub`.
    pub async fn remove_sub(&self, store: &Store, endpoint: &str) {
        if endpoint.is_empty() {
            return;
        }
        let _g = self.subs.lock().await;
        let mut subs = Self::load_subs(store).await;
        subs.retain(|x| x.endpoint != endpoint);
        let _ = Self::save_subs(store, &subs).await;
    }

    /// `sendPushToAll` : best-effort ; un abonnement rejeté pour de bon
    /// (404/410) est purgé.
    pub async fn send_all(&self, store: &Store, title: &str, body: &str) {
        let subs = Self::load_subs(store).await;
        if subs.is_empty() {
            println!("[push] fin de tour : aucun abonné enregistré (rien à envoyer)");
            return;
        }
        let (private, public) = match self.vapid_keys(store).await {
            Ok(k) => k,
            Err(e) => {
                println!("[push] clés VAPID indisponibles : {e}");
                return;
            }
        };
        #[derive(Serialize)]
        struct Payload<'a> {
            title: &'a str,
            body: &'a str,
            tag: &'a str,
        }
        let msg = serde_json::to_vec(&Payload {
            title,
            body,
            tag: "loki-turn",
        })
        .unwrap_or_default();
        println!("[push] envoi à {} abonné(s)…", subs.len());
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map_or(0, |d| d.as_secs());
        for s in subs {
            let host = endpoint_host(&s.endpoint);
            let sent = async {
                let body = encrypt(&msg, &s)?;
                let auth = vapid_header(&s.endpoint, &public, &private, now)?;
                self.client
                    .post(&s.endpoint)
                    .header("Content-Encoding", "aes128gcm")
                    .header("Content-Type", "application/octet-stream")
                    .header("TTL", TTL.to_string())
                    .header("Urgency", "high")
                    .header("Authorization", auth)
                    .body(body)
                    .send()
                    .await
                    .map_err(|e| e.to_string())
            }
            .await;
            let resp = match sent {
                Ok(r) => r,
                Err(e) => {
                    println!("[push] échec envoi ({host}) : {e}");
                    continue;
                }
            };
            let code = resp.status().as_u16();
            if code >= 300 {
                let text = resp.text().await.unwrap_or_default();
                let text: String = text.chars().take(512).collect();
                println!("[push] refus {code} ({host}) : {}", text.trim());
            } else {
                println!("[push] accepté {code} ({host})");
            }
            if code == 404 || code == 410 {
                self.remove_sub(store, &s.endpoint).await;
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Routes
// ---------------------------------------------------------------------------

async fn body_json(body: Incoming) -> Option<Value> {
    use http_body_util::BodyExt;
    let b = http_body_util::Limited::new(body, 1 << 20)
        .collect()
        .await
        .ok()?
        .to_bytes();
    serde_json::from_slice(&b).ok()
}

/// `from_go` : remise par le Go (tunnel), déjà authentifiée par lui.
pub async fn serve(
    ctx: &Ctx,
    push: &std::sync::Arc<Push>,
    req: Request<Incoming>,
    from_go: bool,
) -> Response<Body> {
    let (parts, body) = req.into_parts();
    if !from_go && let Err(refus) = ctx.guard(&parts.headers).await {
        return *refus;
    }
    let fail = |code: StatusCode, e: String| send_json(code, &json!({ "ok": false, "error": e }));
    match parts.uri.path() {
        "/api/push/key" => match push.vapid_keys(&ctx.store).await {
            Ok((_, public)) => send_json(StatusCode::OK, &json!({ "ok": true, "key": public })),
            Err(e) => fail(StatusCode::INTERNAL_SERVER_ERROR, e),
        },
        "/api/push/subscribe" => {
            let s: PushSub = match body_json(body)
                .await
                .and_then(|v| serde_json::from_value::<PushSub>(v).ok())
            {
                Some(s) if !s.endpoint.is_empty() => s,
                _ => return fail(StatusCode::BAD_REQUEST, "abonnement invalide".into()),
            };
            let host = endpoint_host(&s.endpoint);
            match push.add_sub(&ctx.store, s).await {
                Ok(n) => {
                    println!("[push] abonnement enregistré ({host}) — {n} au total");
                    send_json(StatusCode::OK, &json!({ "ok": true }))
                }
                Err(e) => {
                    println!("[push] abonnement REFUSÉ (stockage) : {e}");
                    fail(StatusCode::INTERNAL_SERVER_ERROR, e)
                }
            }
        }
        "/api/push/unsubscribe" => {
            let ep = body_json(body)
                .await
                .and_then(|v| {
                    v.get("endpoint")
                        .and_then(Value::as_str)
                        .map(str::to_string)
                })
                .unwrap_or_default();
            push.remove_sub(&ctx.store, &ep).await;
            send_json(StatusCode::OK, &json!({ "ok": true }))
        }
        _ => send_json(
            StatusCode::NOT_FOUND,
            &json!({ "error": "404 page not found" }),
        ),
    }
}

/// `POST /internal/facade/v1/push/send {title, body}` : envoi en tâche de
/// fond (le Go n'attend pas les services de push).
pub async fn serve_internal(
    store: Store,
    push: std::sync::Arc<Push>,
    req: Request<Incoming>,
) -> Response<Body> {
    let v = body_json(req.into_body()).await.unwrap_or(Value::Null);
    let title = v
        .get("title")
        .and_then(Value::as_str)
        .unwrap_or("Loki")
        .to_string();
    let body = v
        .get("body")
        .and_then(Value::as_str)
        .unwrap_or("")
        .to_string();
    tokio::spawn(async move { push.send_all(&store, &title, &body).await });
    use http_body_util::BodyExt;
    let mut r = Response::new(
        http_body_util::Full::new(bytes::Bytes::new())
            .map_err(|n| match n {})
            .boxed(),
    );
    *r.status_mut() = StatusCode::NO_CONTENT;
    r
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn durees() {
        assert_eq!(fmt_dur_fr(42_000), "42s");
        assert_eq!(fmt_dur_fr(185_000), "3 mn 05s");
        assert_eq!(fmt_dur_fr(4_330_000), "1 h 12 mn");
        assert_eq!(fmt_dur_fr(600), "1s");
        assert_eq!(fmt_dur_fr(-5), "0s");
    }

    #[cfg(feature = "tls")]
    #[test]
    fn cles_vapid() {
        let (p, q) = generate_vapid_keys().unwrap();
        assert_eq!((p.len(), q.len()), (43, 87));
        let h = vapid_header("https://push.exemple.fr:8443/x/y", &q, &p, 1_700_000_000).unwrap();
        assert!(
            h.starts_with("vapid t=eyJhbGciOiJFUzI1NiIsInR5cCI6IkpXVCJ9."),
            "{h}"
        );
        assert!(h.ends_with(&format!(", k={q}")));
    }
}
