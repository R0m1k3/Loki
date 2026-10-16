//! Transport vers le moteur (llama-server local, Strata, ou API externe
//! compatible OpenAI) — port de la partie HTTP de `runChatLoop`
//! (`llm_client.go`) et de `llm_retry_net.go`.
//!
//! Ce module ENVOIE et LIT ; il ne décide rien. Les refus du moteur (niveau
//! de raisonnement, slot annexe, écho, compactage…) sont traités par la
//! boucle, qui s'appuie sur [`classify`], [`retryable`] et [`net_delay`].

use bytes::{Buf, BytesMut};
use futures_util::StreamExt;
use std::time::Duration;

/// Nouvelles tentatives sur erreur réseau AVANT la première réponse.
pub const NET_RETRIES: u32 = 3;
const NET_WAIT: Duration = Duration::from_millis(800);
const NET_MAX_WAIT: Duration = Duration::from_secs(5);
/// Plus longue ligne de flux acceptée (`sc.Buffer(…, 8<<20)`).
pub const MAX_LINE: usize = 8 << 20;

/// `llmNetDelay` : 800 ms, doublé à chaque essai, plafonné à 5 s.
pub fn net_delay(n: i64) -> Duration {
    let n = n.clamp(0, 16) as u32;
    let d = NET_WAIT * 2u32.pow(n);
    d.min(NET_MAX_WAIT)
}

/// `llmRetryableStatus` : refus passagers du moteur ou d'un proxy.
pub fn retryable_status(code: u16) -> bool {
    matches!(code, 429 | 502 | 503 | 504)
}

/// Nature d'une erreur réseau, pour la reprise et le message.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum NetKind {
    /// Connexion refusée : moteur en cours de démarrage.
    Refused,
    /// Délai dépassé : moteur surchargé ou en plein chargement.
    Timeout,
    /// Connexion coupée (EOF, reset) : moteur redémarré.
    Reset,
    /// Arrêt demandé.
    Canceled,
    Other,
}

/// `llmRetryableErr` : seules les pannes passagères se rejouent.
pub fn retryable(kind: NetKind) -> bool {
    matches!(kind, NetKind::Refused | NetKind::Timeout | NetKind::Reset)
}

/// Classe une erreur d'envoi : chaîne des causes (io::ErrorKind), puis
/// libellé — mêmes motifs que `friendlyLLMError`.
pub fn classify(err: &(dyn std::error::Error + 'static)) -> NetKind {
    let mut cur: Option<&(dyn std::error::Error + 'static)> = Some(err);
    while let Some(e) = cur {
        if let Some(io) = e.downcast_ref::<std::io::Error>() {
            use std::io::ErrorKind as K;
            match io.kind() {
                K::ConnectionRefused => return NetKind::Refused,
                K::TimedOut => return NetKind::Timeout,
                K::ConnectionReset | K::ConnectionAborted | K::UnexpectedEof | K::BrokenPipe => {
                    return NetKind::Reset;
                }
                _ => {}
            }
        }
        if let Some(r) = e.downcast_ref::<reqwest::Error>()
            && r.is_timeout()
        {
            return NetKind::Timeout;
        }
        cur = e.source();
    }
    classify_text(&full_text(err))
}

fn full_text(err: &(dyn std::error::Error + 'static)) -> String {
    let mut s = err.to_string();
    let mut cur = err.source();
    while let Some(e) = cur {
        s.push_str(": ");
        s.push_str(&e.to_string());
        cur = e.source();
    }
    s
}

/// Repli sur le libellé, comme le second `switch` de `friendlyLLMError`.
pub fn classify_text(msg: &str) -> NetKind {
    let low = msg.to_lowercase();
    if low.contains("context canceled") {
        NetKind::Canceled
    } else if [
        "connection refused",
        "actively refused",
        "connectex",
        "no connection could be made",
    ]
    .iter()
    .any(|m| low.contains(m))
    {
        NetKind::Refused
    } else if low.contains("timeout")
        || low.contains("deadline exceeded")
        || low.contains("timed out")
    {
        NetKind::Timeout
    } else if low.contains("eof") || low.contains("connection reset") {
        NetKind::Reset
    } else {
        NetKind::Other
    }
}

/// `friendlyLLMError` : message affiché pour une panne d'envoi. `load_error`
/// = `modelLoadError()` (journal du moteur), qui l'emporte sur « il démarre ».
pub fn friendly_error(kind: NetKind, raw: &str, port: u16, load_error: &str) -> String {
    match kind {
        NetKind::Refused => {
            if !load_error.is_empty() {
                format!("⚠️ {load_error}")
            } else {
                format!(
                    "⚠️ Le moteur (llama-server) ne répond pas sur le port {port}. Il est probablement en train de démarrer ou de charger le modèle — réessaie dans quelques secondes."
                )
            }
        }
        NetKind::Timeout => format!(
            "⚠️ Le moteur (llama-server) met trop de temps à répondre (port {port}) — il est peut-être surchargé ou en plein chargement. Réessaie dans un instant."
        ),
        NetKind::Reset => format!(
            "⚠️ Connexion au moteur (llama-server, port {port}) interrompue — il a peut-être redémarré. Réessaie."
        ),
        NetKind::Canceled | NetKind::Other => raw.to_string(),
    }
}

/// `streamCutError` : le flux a été coupé APRÈS une réponse 200.
pub fn stream_cut_error(err: &str, too_long: bool, port: u16) -> String {
    if too_long {
        return format!(
            "⚠️ Réponse du moteur illisible : une ligne du flux dépasse la taille maximale ({} Mio). C'est presque toujours un appel d'outil démesuré (écriture d'un très gros fichier). Le tour est abandonné pour ne pas exécuter un appel tronqué.",
            MAX_LINE >> 20
        );
    }
    format!(
        "⚠️ Le flux de réponse du moteur (llama-server, port {port}) a été coupé en cours de route : {err}. La réponse est incomplète et le tour est abandonné — réessaie."
    )
}

/// Client HTTP vers le moteur. Comme `http.DefaultClient` : aucun délai
/// global (une génération dure ce qu'elle dure) ; c'est l'arrêt du tour qui
/// coupe.
#[derive(Clone)]
pub struct EngineHttp {
    client: reqwest::Client,
}

impl Default for EngineHttp {
    fn default() -> Self {
        Self::new()
    }
}

impl EngineHttp {
    pub fn new() -> Self {
        install_crypto();
        EngineHttp {
            client: reqwest::Client::builder()
                .build()
                .expect("client HTTP du moteur"),
        }
    }

    /// POST du JSON `body` vers `url`. `key` : la clé de CET endpoint
    /// (`chatEndpoint.auth`) — jamais celle du serveur local envoyée à une API
    /// externe.
    pub async fn post(
        &self,
        url: &str,
        key: &str,
        body: Vec<u8>,
    ) -> Result<reqwest::Response, reqwest::Error> {
        let mut req = self
            .client
            .post(url)
            .header("Content-Type", "application/json")
            .body(body);
        if !key.is_empty() {
            req = req.header("Authorization", format!("Bearer {key}"));
        }
        req.send().await
    }
}

/// `healthCheck` : `/health` doit répondre tout de suite ou pas du tout
/// (`healthClient`, 3 s) — un moteur qui accepte la connexion sans répondre
/// (très gros modèle en chargement) ne doit pas bloquer l'envoi.
pub const HEALTH_TIMEOUT: Duration = Duration::from_secs(3);

impl EngineHttp {
    /// Le moteur répond 200 sur `url` (`/health`) dans le délai.
    pub async fn health(&self, url: &str) -> bool {
        match tokio::time::timeout(HEALTH_TIMEOUT, self.client.get(url).send()).await {
            Ok(Ok(r)) => r.status().as_u16() == 200,
            _ => false,
        }
    }
}

/// Fournisseur cryptographique de rustls (ring), posé une fois (tout client
/// reqwest de la façade le demande avant d'être construit).
pub fn install_crypto() {
    #[cfg(feature = "tls")]
    let _ = rustls::crypto::ring::default_provider().install_default();
}

/// Erreur de lecture du flux.
#[derive(Debug)]
pub enum LineError {
    /// Ligne plus longue que [`MAX_LINE`] (`bufio.ErrTooLong`).
    TooLong,
    Io(String),
}

/// Découpe un corps de réponse en lignes, comme `bufio.Scanner` (`\n`, `\r`
/// final retiré, dernière ligne sans saut rendue).
pub struct Lines<S> {
    body: S,
    buf: BytesMut,
    done: bool,
}

impl<S, B, E> Lines<S>
where
    S: futures_util::Stream<Item = Result<B, E>> + Unpin,
    B: Buf,
    E: std::fmt::Display,
{
    pub fn new(body: S) -> Self {
        Lines {
            body,
            buf: BytesMut::new(),
            done: false,
        }
    }

    pub async fn next_line(&mut self) -> Option<Result<String, LineError>> {
        loop {
            if let Some(i) = self.buf.iter().position(|&b| b == b'\n') {
                let mut line = self.buf.split_to(i + 1);
                line.truncate(i);
                if line.last() == Some(&b'\r') {
                    line.truncate(line.len() - 1);
                }
                return Some(Ok(String::from_utf8_lossy(&line).into_owned()));
            }
            if self.buf.len() > MAX_LINE {
                // Comme bufio.Scanner : l'erreur arrête la lecture pour de bon.
                self.done = true;
                self.buf.clear();
                return Some(Err(LineError::TooLong));
            }
            if self.done {
                if self.buf.is_empty() {
                    return None;
                }
                let line = self.buf.split();
                let mut s = String::from_utf8_lossy(&line).into_owned();
                if s.ends_with('\r') {
                    s.pop();
                }
                return Some(Ok(s));
            }
            match self.body.next().await {
                Some(Ok(mut chunk)) => {
                    while chunk.has_remaining() {
                        let c = chunk.chunk();
                        let n = c.len();
                        self.buf.extend_from_slice(c);
                        chunk.advance(n);
                    }
                }
                Some(Err(e)) => {
                    self.done = true;
                    return Some(Err(LineError::Io(e.to_string())));
                }
                None => self.done = true,
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use bytes::Bytes;

    #[test]
    fn delais_de_reprise() {
        assert_eq!(net_delay(-1), Duration::from_millis(800));
        assert_eq!(net_delay(0), Duration::from_millis(800));
        assert_eq!(net_delay(1), Duration::from_millis(1600));
        assert_eq!(net_delay(2), Duration::from_millis(3200));
        assert_eq!(net_delay(3), Duration::from_secs(5));
        assert_eq!(net_delay(99), Duration::from_secs(5));
    }

    #[test]
    fn statuts_rejouables() {
        for c in [429, 502, 503, 504] {
            assert!(retryable_status(c));
        }
        for c in [200, 400, 401, 404, 500] {
            assert!(!retryable_status(c));
        }
    }

    #[test]
    fn classement_et_messages() {
        let e = std::io::Error::from(std::io::ErrorKind::ConnectionRefused);
        assert_eq!(classify(&e), NetKind::Refused);
        assert_eq!(
            classify_text("dial tcp: connectex: No connection could be made"),
            NetKind::Refused
        );
        assert_eq!(
            classify_text("read: connection reset by peer"),
            NetKind::Reset
        );
        assert_eq!(classify_text("unexpected EOF"), NetKind::Reset);
        assert_eq!(classify_text("operation timed out"), NetKind::Timeout);
        assert_eq!(classify_text("tls: bad certificate"), NetKind::Other);
        assert!(
            retryable(NetKind::Refused)
                && !retryable(NetKind::Other)
                && !retryable(NetKind::Canceled)
        );
        assert_eq!(
            friendly_error(NetKind::Refused, "x", 8080, ""),
            "⚠️ Le moteur (llama-server) ne répond pas sur le port 8080. Il est probablement en train de démarrer ou de charger le modèle — réessaie dans quelques secondes."
        );
        assert_eq!(
            friendly_error(NetKind::Refused, "x", 8080, "Modèle incompatible"),
            "⚠️ Modèle incompatible"
        );
        assert_eq!(friendly_error(NetKind::Other, "brut", 8080, ""), "brut");
    }

    #[tokio::test]
    async fn lignes_comme_bufio_scanner() {
        let parts: Vec<Result<Bytes, std::io::Error>> = vec![
            Ok(Bytes::from_static(b"data: a\r\n\nda")),
            Ok(Bytes::from_static(b"ta: b\n")),
            Ok(Bytes::from_static(b"fin")),
        ];
        let mut l = Lines::new(futures_util::stream::iter(parts));
        let mut got = Vec::new();
        while let Some(r) = l.next_line().await {
            got.push(r.unwrap());
        }
        assert_eq!(got, ["data: a", "", "data: b", "fin"]);
    }

    #[tokio::test]
    async fn ligne_trop_longue() {
        let big = Bytes::from(vec![b'x'; MAX_LINE + 10]);
        let parts: Vec<Result<Bytes, std::io::Error>> = vec![Ok(big)];
        let mut l = Lines::new(futures_util::stream::iter(parts));
        assert!(matches!(l.next_line().await, Some(Err(LineError::TooLong))));
        assert!(l.next_line().await.is_none());
    }

    #[tokio::test]
    async fn envoi_et_flux_de_bout_en_bout() {
        use hyper::service::service_fn;
        use hyper_util::rt::TokioIo;
        let l = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = l.local_addr().unwrap();
        tokio::spawn(async move {
            let (s, _) = l.accept().await.unwrap();
            let svc = service_fn(|req: hyper::Request<hyper::body::Incoming>| async move {
                let auth = req
                    .headers()
                    .get("authorization")
                    .map(|v| v.to_str().unwrap().to_string())
                    .unwrap_or_default();
                let body = format!("data: {auth}\n\ndata: [DONE]\n\n");
                Ok::<_, std::convert::Infallible>(hyper::Response::new(http_body_util::Full::new(
                    Bytes::from(body),
                )))
            });
            let _ = hyper::server::conn::http1::Builder::new()
                .serve_connection(TokioIo::new(s), svc)
                .await;
        });
        let http = EngineHttp::new();
        let resp = http
            .post(
                &format!("http://{addr}/v1/chat/completions"),
                "cle",
                b"{}".to_vec(),
            )
            .await
            .unwrap();
        assert_eq!(resp.status(), 200);
        let mut lines = Lines::new(resp.bytes_stream());
        let mut got = Vec::new();
        while let Some(r) = lines.next_line().await {
            got.push(r.unwrap());
        }
        assert_eq!(got, ["data: Bearer cle", "", "data: [DONE]", ""]);

        // Moteur absent : connexion refusée, rejouable.
        let mort = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .unwrap()
            .local_addr()
            .unwrap();
        let err = http
            .post(&format!("http://{mort}/"), "", vec![])
            .await
            .unwrap_err();
        assert_eq!(classify(&err), NetKind::Refused);
    }
}
