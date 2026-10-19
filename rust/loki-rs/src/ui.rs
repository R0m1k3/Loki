//! UI web statique, servie par la façade (lot 2). Mêmes en-têtes que les
//! routes de `web_server.go`, même compression que `web_gzip.go`.

use crate::proxy::Body;
use bytes::Bytes;
use flate2::Compression;
use flate2::write::GzEncoder;
use http_body_util::{BodyExt, Full};
use hyper::header::{self, HeaderMap, HeaderValue};
use hyper::{Method, Response};
use std::io::Write;
use std::sync::OnceLock;

mod assets {
    include!(concat!(env!("OUT_DIR"), "/ui_assets.rs"));
}

struct Asset {
    body: &'static [u8],
    ctype: &'static str,
    cache: &'static str,
    /// Version gzip, calculée à la première demande qui l'accepte.
    gz: OnceLock<Vec<u8>>,
}

impl Asset {
    const fn new(body: &'static [u8], ctype: &'static str, cache: &'static str) -> Self {
        Asset {
            body,
            ctype,
            cache,
            gz: OnceLock::new(),
        }
    }
}

const NO_STORE: &str = "no-store, max-age=0";

static INDEX: Asset = Asset::new(assets::INDEX_HTML, "text/html; charset=utf-8", NO_STORE);
static MARKED: Asset = Asset::new(
    assets::MARKED_MIN_JS,
    "application/javascript",
    "public, max-age=86400",
);
static SW: Asset = Asset::new(assets::SW_JS, "application/javascript", NO_STORE);
static MANIFEST: Asset = Asset::new(
    assets::MANIFEST_WEBMANIFEST,
    "application/manifest+json",
    "public, max-age=3600",
);
/// KaTeX (formules LaTeX des réponses) : mêmes en-têtes que `web_katex.go`.
const KATEX_CACHE: &str = "public, max-age=86400";
static KATEX_JS: Asset = Asset::new(assets::KATEX_JS, "application/javascript", KATEX_CACHE);
static KATEX_CSS: Asset = Asset::new(assets::KATEX_CSS, "text/css; charset=utf-8", KATEX_CACHE);

/// Réponse statique pour cette requête, ou `None` si la route n'est pas une
/// ressource de l'UI (elle part alors au Go, qui rendra son 404 habituel).
pub fn serve(method: &Method, path: &str, headers: &HeaderMap) -> Option<Response<Body>> {
    let head = method == Method::HEAD;
    if !head && method != Method::GET {
        return None;
    }
    // Comme gzipHandler : HEAD jamais compressé, et Vary posé dès que le
    // client accepte gzip (même pour une réponse qui ne l'est pas).
    let accepts_gzip = !head
        && headers
            .get_all(header::ACCEPT_ENCODING)
            .iter()
            .filter_map(|v| v.to_str().ok())
            .any(|v| v.contains("gzip"));

    let mut resp = match path {
        "/" | "/index.html" => asset(&INDEX, accepts_gzip),
        "/marked.min.js" => asset(&MARKED, accepts_gzip),
        "/sw.js" => {
            let mut r = asset(&SW, accepts_gzip);
            r.headers_mut()
                .insert("service-worker-allowed", HeaderValue::from_static("/"));
            r
        }
        "/manifest.webmanifest" => asset(&MANIFEST, accepts_gzip),
        p if p.starts_with("/fonts/") => font(p)?,
        "/katex/katex.min.js" => asset(&KATEX_JS, accepts_gzip),
        "/katex/katex.min.css" => asset(&KATEX_CSS, accepts_gzip),
        p if p.starts_with("/katex/") => katex_font(p)?,
        _ => return None,
    };
    if accepts_gzip {
        resp.headers_mut()
            .append(header::VARY, HeaderValue::from_static("Accept-Encoding"));
    }
    if head {
        let len = resp.headers().get(header::CONTENT_LENGTH).cloned();
        let mut empty = Response::new(full(Bytes::new()));
        *empty.headers_mut() = std::mem::take(resp.headers_mut());
        if let Some(len) = len {
            empty.headers_mut().insert(header::CONTENT_LENGTH, len);
        }
        return Some(empty);
    }
    Some(resp)
}

fn asset(a: &'static Asset, gzip: bool) -> Response<Body> {
    let (body, encoded) = if gzip {
        (a.gz.get_or_init(|| compress(a.body)).as_slice(), true)
    } else {
        (a.body, false)
    };
    let mut r = Response::new(full(Bytes::from_static(body)));
    let h = r.headers_mut();
    h.insert(header::CONTENT_TYPE, HeaderValue::from_static(a.ctype));
    h.insert(header::CACHE_CONTROL, HeaderValue::from_static(a.cache));
    h.insert(header::CONTENT_LENGTH, HeaderValue::from(body.len()));
    if encoded {
        h.insert(header::CONTENT_ENCODING, HeaderValue::from_static("gzip"));
    }
    r
}

/// `/fonts/<nom>.woff2` — comme le Go, seul le dernier segment compte.
/// Jamais compressées : woff2 l'est déjà.
fn font(path: &str) -> Option<Response<Body>> {
    let name = path.rsplit('/').next()?;
    if !name.ends_with(".woff2") {
        return None;
    }
    let (_, body) = assets::FONTS.iter().find(|(n, _)| *n == name)?;
    let mut r = Response::new(full(Bytes::from_static(body)));
    let h = r.headers_mut();
    h.insert(header::CONTENT_TYPE, HeaderValue::from_static("font/woff2"));
    h.insert(
        header::CACHE_CONTROL,
        HeaderValue::from_static("public, max-age=31536000, immutable"),
    );
    h.insert(header::CONTENT_LENGTH, HeaderValue::from(body.len()));
    Some(r)
}

/// `/katex/fonts/<nom>.woff2` — chemin exact (pas le dernier segment seul,
/// contrairement à `/fonts/`), comme `katexFile` dans `web_katex.go`. Le
/// reste de `/katex/` part au Go, qui rend son 404.
fn katex_font(path: &str) -> Option<Response<Body>> {
    let rel = path.strip_prefix("/katex/")?;
    let (_, body) = assets::KATEX_FONTS.iter().find(|(n, _)| *n == rel)?;
    let mut r = Response::new(full(Bytes::from_static(body)));
    let h = r.headers_mut();
    h.insert(header::CONTENT_TYPE, HeaderValue::from_static("font/woff2"));
    h.insert(header::CACHE_CONTROL, HeaderValue::from_static(KATEX_CACHE));
    h.insert(header::CONTENT_LENGTH, HeaderValue::from(body.len()));
    Some(r)
}

fn compress(b: &[u8]) -> Vec<u8> {
    let mut enc = GzEncoder::new(Vec::with_capacity(b.len() / 3), Compression::default());
    enc.write_all(b).expect("gzip en mémoire");
    enc.finish().expect("gzip en mémoire")
}

fn full(b: Bytes) -> Body {
    Full::new(b).map_err(|n| match n {}).boxed()
}

/// Taille de l'index embarqué (tests).
pub fn index_bytes() -> &'static [u8] {
    INDEX.body
}

#[cfg(test)]
mod tests {
    use super::*;

    fn get(path: &str, gzip: bool) -> Option<Response<Body>> {
        let mut h = HeaderMap::new();
        if gzip {
            h.insert(
                header::ACCEPT_ENCODING,
                HeaderValue::from_static("gzip, br"),
            );
        }
        serve(&Method::GET, path, &h)
    }

    fn hv<'a>(r: &'a Response<Body>, k: &str) -> &'a str {
        r.headers()
            .get(k)
            .map(|v| v.to_str().unwrap())
            .unwrap_or("")
    }

    #[test]
    fn en_tetes_comme_le_go() {
        let r = get("/", false).unwrap();
        assert_eq!(hv(&r, "content-type"), "text/html; charset=utf-8");
        assert_eq!(hv(&r, "cache-control"), "no-store, max-age=0");
        assert_eq!(hv(&r, "vary"), "");
        let r = get("/sw.js", false).unwrap();
        assert_eq!(hv(&r, "service-worker-allowed"), "/");
        assert_eq!(hv(&r, "cache-control"), "no-store, max-age=0");
        let r = get("/marked.min.js", false).unwrap();
        assert_eq!(hv(&r, "cache-control"), "public, max-age=86400");
        let r = get("/manifest.webmanifest", false).unwrap();
        assert_eq!(hv(&r, "content-type"), "application/manifest+json");
    }

    #[test]
    fn polices() {
        let (name, _) = assets::FONTS[0];
        let r = get(&format!("/fonts/{name}"), true).unwrap();
        assert_eq!(hv(&r, "content-type"), "font/woff2");
        assert_eq!(hv(&r, "content-encoding"), "");
        assert_eq!(hv(&r, "vary"), "Accept-Encoding");
        assert!(get("/fonts/absente.woff2", false).is_none());
        assert!(get("/fonts/inter.ttf", false).is_none());
    }

    #[test]
    fn katex() {
        let r = get("/katex/katex.min.js", true).unwrap();
        assert_eq!(hv(&r, "content-type"), "application/javascript");
        assert_eq!(hv(&r, "cache-control"), "public, max-age=86400");
        assert_eq!(hv(&r, "content-encoding"), "gzip");
        assert!(
            String::from_utf8_lossy(assets::KATEX_JS).contains("version:\"0.16.11\""),
            "KaTeX 0.16.11 attendu"
        );
        let r = get("/katex/katex.min.css", false).unwrap();
        assert_eq!(hv(&r, "content-type"), "text/css; charset=utf-8");
        // Chaque police woff2 demandée par la feuille est servie, non compressée.
        let css = String::from_utf8_lossy(assets::KATEX_CSS);
        let polices: Vec<&str> = css
            .split("url(")
            .skip(1)
            .filter_map(|s| s.split(')').next())
            .filter(|u| u.ends_with(".woff2"))
            .collect();
        assert!(polices.len() >= 10, "{} polices", polices.len());
        for p in polices {
            let r = get(&format!("/katex/{p}"), true).unwrap_or_else(|| panic!("{p}"));
            assert_eq!(hv(&r, "content-type"), "font/woff2");
            assert_eq!(hv(&r, "content-encoding"), "");
            assert_eq!(hv(&r, "cache-control"), "public, max-age=86400");
        }
        // Le reste part au Go (404) : ni index, ni licence, ni dernier segment seul.
        for p in [
            "/katex/",
            "/katex/LICENSE",
            "/katex/fonts/",
            "/katex/fonts/absente.woff2",
            "/katex/KaTeX_Main-Regular.woff2",
            "/katex/x/fonts/KaTeX_Main-Regular.woff2",
        ] {
            assert!(get(p, false).is_none(), "{p}");
        }
    }

    #[test]
    fn gzip_si_accepte() {
        let r = get("/", true).unwrap();
        assert_eq!(hv(&r, "content-encoding"), "gzip");
        assert_eq!(hv(&r, "vary"), "Accept-Encoding");
        let len: usize = hv(&r, "content-length").parse().unwrap();
        assert!(len < index_bytes().len() / 2, "gzip inefficace : {len}");
    }

    #[test]
    fn head_et_methodes() {
        let h = HeaderMap::new();
        let r = serve(&Method::HEAD, "/", &h).unwrap();
        assert_eq!(hv(&r, "content-length"), index_bytes().len().to_string());
        assert!(serve(&Method::POST, "/", &h).is_none());
        assert!(get("/api/status", false).is_none());
        assert!(get("/index.htm", false).is_none());
    }
}
