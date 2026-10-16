//! Lot 12 : notifications Web Push tenues par la façade (`LOKI_PUSH=rust`).
//!
//! - le journal tiré du Go (`internal/loki/parity_push_test.go` →
//!   `tests/parity_push/push.json`) : durées, clé, abonnements — la liste en
//!   base comparée à l'octet près ;
//! - le chiffrement contre l'exemple de la RFC 8291 (annexe A) ;
//! - un envoi de bout en bout vers un faux service de push : en-têtes,
//!   signature VAPID vérifiée, corps déchiffré côté navigateur, abonnement
//!   purgé sur 410.
#![cfg(feature = "tls")]

mod common;

use std::convert::Infallible;
use std::net::SocketAddr;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use base64::Engine;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use bytes::Bytes;
use common::fakego;
use http_body_util::{BodyExt, Full};
use hyper::body::Incoming;
use hyper::{Request, Response, StatusCode, service::service_fn};
use hyper_util::rt::TokioIo;
use loki_rs::push;
use ring::{aead, agreement, hkdf, signature};
use serde_json::Value;

fn fixture() -> Value {
    serde_json::from_str(include_str!("parity_push/push.json")).expect("journal")
}

/// `time.ParseDuration`, réduit à ce que le journal emploie.
fn go_ms(s: &str) -> i64 {
    let (neg, mut s) = match s.strip_prefix('-') {
        Some(r) => (true, r),
        None => (false, s),
    };
    let mut ms = 0f64;
    while !s.is_empty() {
        let n = s.find(|c: char| !c.is_ascii_digit() && c != '.').unwrap();
        let v: f64 = s[..n].parse().unwrap();
        let rest = &s[n..];
        let (mul, len) = if rest.starts_with("ms") {
            (1.0, 2)
        } else if rest.starts_with('h') {
            (3_600_000.0, 1)
        } else if rest.starts_with('m') {
            (60_000.0, 1)
        } else {
            (1000.0, 1)
        };
        ms += v * mul;
        s = &rest[len..];
    }
    if neg { -(ms as i64) } else { ms as i64 }
}

#[test]
fn parite_des_durees() {
    for (d, want) in fixture()["durations"].as_object().unwrap() {
        assert_eq!(push::fmt_dur_fr(go_ms(d)), want.as_str().unwrap(), "{d}");
    }
}

#[test]
fn exemple_de_la_rfc_8291() {
    let b = |s: &str| URL_SAFE_NO_PAD.decode(s).unwrap();
    let body = push::encrypt_with(
        b"When I grow up, I want to be a watermelon",
        &b("BTBZMqHH6r4Tts7J_aSIgg"),
        &b("BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"),
        b("DGv6ra1nlYgDCS1FRnbzlw").as_slice().try_into().unwrap(),
        &b("BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8"),
        &b("kyrL1jIIOHEzg3sM2ZWRHDRB62YACZhhSlknJ672kSs"),
        4096,
        false,
    )
    .unwrap();
    assert_eq!(
        URL_SAFE_NO_PAD.encode(body),
        "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
    );
}

#[tokio::test]
async fn parite_des_routes() {
    let fx = fixture();
    let tmp = tempfile::tempdir().unwrap();
    let opts = fakego::Opts {
        push: Some(Arc::new(push::Push::new())),
        ..Default::default()
    };
    let (facade, go, _stop, _) = fakego::start_with(tmp.path().to_path_buf(), opts).await;
    let key_re = regex::Regex::new(r#""key":"[A-Za-z0-9_-]{87}""#).unwrap();
    let mut first_key = String::new();
    for (i, c) in fx["calls"].as_array().unwrap().iter().enumerate() {
        let (m, p) = (c["method"].as_str().unwrap(), c["path"].as_str().unwrap());
        let body = c.get("body").and_then(Value::as_str).unwrap_or("");
        let (code, b) = fakego::send(facade, m, p, &[], body).await;
        let txt = String::from_utf8_lossy(&b).into_owned();
        if let Some(k) = key_re.find(&txt) {
            // Clé stable d'un appel à l'autre.
            if first_key.is_empty() {
                first_key = k.as_str().to_string();
            }
            assert_eq!(k.as_str(), first_key, "clé VAPID changée");
        }
        let got: Value =
            serde_json::from_str(&key_re.replace_all(&txt, r#""key":"{{CLE}}""#)).unwrap();
        let ctx = format!("appel {i} : {m} {p} {body}");
        assert_eq!(code.as_u16() as u64, c["status"].as_u64().unwrap(), "{ctx}");
        assert_eq!(got, c["out"], "{ctx}");
        assert_eq!(
            go.get("state", "push_subs").unwrap_or_default(),
            c["subs"].as_str().unwrap(),
            "{ctx} (abonnements en base)"
        );
    }
    assert_eq!(go.get("state", "push_vapid_priv").unwrap().len(), 43);
}

/// Faux service de push : répond 201 au premier endpoint, 410 au second, et
/// garde chaque requête (chemin, en-têtes, corps).
#[derive(Default)]
struct FakePush {
    seen: Mutex<Vec<(String, hyper::HeaderMap, Bytes)>>,
}

async fn fake_push() -> (SocketAddr, Arc<FakePush>) {
    let st = Arc::new(FakePush::default());
    let l = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = l.local_addr().unwrap();
    let s2 = st.clone();
    tokio::spawn(async move {
        while let Ok((s, _)) = l.accept().await {
            let st = s2.clone();
            tokio::spawn(async move {
                let svc = service_fn(move |req: Request<Incoming>| {
                    let st = st.clone();
                    async move {
                        let path = req.uri().path().to_string();
                        let (parts, body) = req.into_parts();
                        let b = body.collect().await.unwrap().to_bytes();
                        st.seen
                            .lock()
                            .unwrap()
                            .push((path.clone(), parts.headers, b));
                        let code = if path.ends_with("/perime") { 410 } else { 201 };
                        let mut r = Response::new(Full::new(Bytes::from_static(b"")));
                        *r.status_mut() = StatusCode::from_u16(code).unwrap();
                        Ok::<_, Infallible>(r)
                    }
                });
                let _ = hyper::server::conn::http1::Builder::new()
                    .serve_connection(TokioIo::new(s), svc)
                    .await;
            });
        }
    });
    (addr, st)
}

struct Len(usize);
impl hkdf::KeyType for Len {
    fn len(&self) -> usize {
        self.0
    }
}

fn expand(salt: &[u8], ikm: &[u8], info: &[u8], n: usize) -> Vec<u8> {
    let prk = hkdf::Salt::new(hkdf::HKDF_SHA256, salt).extract(ikm);
    let parts = [info];
    let okm = prk.expand(&parts, Len(n)).unwrap();
    let mut out = vec![0u8; n];
    okm.fill(&mut out).unwrap();
    out
}

#[tokio::test]
async fn envoi_de_bout_en_bout() {
    let (addr, svc) = fake_push().await;
    let tmp = tempfile::tempdir().unwrap();
    let p = Arc::new(push::Push::new());
    let opts = fakego::Opts {
        push: Some(p.clone()),
        ..Default::default()
    };
    let (facade, go, _stop, _) = fakego::start_with(tmp.path().to_path_buf(), opts).await;
    // Côté navigateur : une clé ECDH (à usage unique ici) et le secret d'auth.
    let rng = ring::rand::SystemRandom::new();
    let ua_priv = agreement::EphemeralPrivateKey::generate(&agreement::ECDH_P256, &rng).unwrap();
    let ua_pub = ua_priv.compute_public_key().unwrap().as_ref().to_vec();
    let auth = b"seize octets !!!".to_vec();
    let subs = format!(
        r#"[{{"endpoint":"http://{addr}/push/ok","keys":{{"auth":"{}","p256dh":"{}"}}}},{{"endpoint":"http://{addr}/push/perime","keys":{{"auth":"{}","p256dh":"{}"}}}}]"#,
        URL_SAFE_NO_PAD.encode(&auth),
        URL_SAFE_NO_PAD.encode(&ua_pub),
        URL_SAFE_NO_PAD.encode(&auth),
        URL_SAFE_NO_PAD.encode(&ua_pub),
    );
    go.set("state", "push_subs", &subs);
    let (code, _) = fakego::send(
        facade,
        "POST",
        push::INTERNAL_SEND,
        &[("x-loki-internal", fakego::JETON)],
        r#"{"title":"Loki","body":"Réponse prête · 42s"}"#,
    )
    .await;
    assert_eq!(code, StatusCode::NO_CONTENT);
    for _ in 0..200 {
        if svc.seen.lock().unwrap().len() >= 2
            && go
                .get("state", "push_subs")
                .is_some_and(|s| !s.contains("perime"))
        {
            break;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    let seen = svc.seen.lock().unwrap().clone();
    assert_eq!(seen.len(), 2);
    // Abonnement périmé (410) purgé, l'autre gardé.
    let left = go.get("state", "push_subs").unwrap();
    assert!(
        left.contains("/push/ok") && !left.contains("perime"),
        "{left}"
    );

    let (path, h, body) = seen.iter().find(|s| s.0 == "/push/ok").unwrap().clone();
    assert_eq!(path, "/push/ok");
    assert_eq!(h["content-encoding"], "aes128gcm");
    assert_eq!(h["content-type"], "application/octet-stream");
    assert_eq!(h["ttl"], "120");
    assert_eq!(h["urgency"], "high");
    // VAPID : JWT ES256 signé par la clé rangée, audience = origine du service.
    let vapid_pub = go.get("state", "push_vapid_pub").unwrap();
    let auth_h = h["authorization"].to_str().unwrap().to_string();
    let rest = auth_h.strip_prefix("vapid t=").unwrap();
    let (jwt, k) = rest.split_once(", k=").unwrap();
    assert_eq!(k, vapid_pub);
    let mut parts = jwt.split('.');
    let (h64, c64, s64) = (
        parts.next().unwrap(),
        parts.next().unwrap(),
        parts.next().unwrap(),
    );
    let claims: Value = serde_json::from_slice(&URL_SAFE_NO_PAD.decode(c64).unwrap()).unwrap();
    assert_eq!(claims["aud"], format!("http://{addr}"));
    assert_eq!(claims["sub"], "mailto:mail@nathaninline.com");
    signature::UnparsedPublicKey::new(
        &signature::ECDSA_P256_SHA256_FIXED,
        URL_SAFE_NO_PAD.decode(&vapid_pub).unwrap(),
    )
    .verify(
        format!("{h64}.{c64}").as_bytes(),
        &URL_SAFE_NO_PAD.decode(s64).unwrap(),
    )
    .expect("signature VAPID invalide");

    // Déchiffrement côté navigateur (RFC 8291).
    assert_eq!(
        body.len(),
        push::RECORD_SIZE,
        "enregistrement complété à 4096 octets"
    );
    let salt = &body[..16];
    assert_eq!(u32::from_be_bytes(body[16..20].try_into().unwrap()), 4096);
    let id_len = body[20] as usize;
    let as_pub = body[21..21 + id_len].to_vec();
    let cipher = body[21 + id_len..].to_vec();
    let peer = agreement::UnparsedPublicKey::new(&agreement::ECDH_P256, as_pub.clone());
    let shared = agreement::agree_ephemeral(ua_priv, &peer, |s| s.to_vec()).unwrap();
    let mut info = b"WebPush: info\0".to_vec();
    info.extend_from_slice(&ua_pub);
    info.extend_from_slice(&as_pub);
    let ikm = expand(&auth, &shared, &info, 32);
    let cek = expand(salt, &ikm, b"Content-Encoding: aes128gcm\0", 16);
    let nonce = expand(salt, &ikm, b"Content-Encoding: nonce\0", 12);
    let key = aead::LessSafeKey::new(aead::UnboundKey::new(&aead::AES_128_GCM, &cek).unwrap());
    let mut buf = cipher;
    let plain = key
        .open_in_place(
            aead::Nonce::try_assume_unique_for_key(&nonce).unwrap(),
            aead::Aad::empty(),
            &mut buf,
        )
        .expect("déchiffrement");
    let end = plain.iter().rposition(|b| *b == 0x02).unwrap();
    assert!(plain[end + 1..].iter().all(|b| *b == 0));
    let msg: Value = serde_json::from_slice(&plain[..end]).unwrap();
    assert_eq!(
        msg,
        serde_json::json!({ "title": "Loki", "body": "Réponse prête · 42s", "tag": "loki-turn" })
    );
}

#[tokio::test]
async fn sans_la_variable_les_notifications_restent_au_go() {
    let tmp = tempfile::tempdir().unwrap();
    let (facade, _go, _stop) = fakego::start(tmp.path().to_path_buf()).await;
    let (_, _, b) = fakego::get(facade, "/api/push/key", &[]).await;
    assert_eq!(&b[..], b"go:/api/push/key");
    let (code, _) = fakego::send(
        facade,
        "POST",
        push::INTERNAL_SEND,
        &[("x-loki-internal", fakego::JETON)],
        "{}",
    )
    .await;
    assert_eq!(code, StatusCode::NOT_FOUND);
}
