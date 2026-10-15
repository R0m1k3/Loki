//! Serveur HTTP minimal pour les tests : sert des fichiers en mémoire et
//! journalise les chemins demandés.
#![allow(dead_code)]

pub mod fakego;
pub mod toolsfx;

use bytes::Bytes;
use http_body_util::Full;
use hyper::{Request, Response, StatusCode, body::Incoming, service::service_fn};
use hyper_util::rt::TokioIo;
use std::collections::HashMap;
use std::net::SocketAddr;
use std::sync::{Arc, Mutex};

pub struct FileServer {
    pub addr: SocketAddr,
    pub hits: Arc<Mutex<Vec<String>>>,
    _rt: tokio::runtime::Runtime,
}

impl FileServer {
    pub fn url(&self) -> String {
        format!("http://{}/rolling", self.addr)
    }
    pub fn hits(&self) -> Vec<String> {
        self.hits.lock().unwrap().clone()
    }
}

pub fn serve_files(files: HashMap<String, Vec<u8>>) -> FileServer {
    let rt = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .unwrap();
    let files = Arc::new(files);
    let hits = Arc::new(Mutex::new(Vec::new()));
    let listener = rt
        .block_on(tokio::net::TcpListener::bind("127.0.0.1:0"))
        .unwrap();
    let addr = listener.local_addr().unwrap();
    let h = hits.clone();
    rt.spawn(async move {
        loop {
            let Ok((stream, _)) = listener.accept().await else {
                return;
            };
            let files = files.clone();
            let h = h.clone();
            tokio::spawn(async move {
                let svc = service_fn(move |req: Request<Incoming>| {
                    let files = files.clone();
                    let h = h.clone();
                    async move {
                        let path = req.uri().path().trim_start_matches("/rolling/").to_string();
                        h.lock().unwrap().push(path.clone());
                        let resp = match files.get(&path) {
                            Some(b) => Response::new(Full::new(Bytes::from(b.clone()))),
                            None => {
                                let mut r = Response::new(Full::new(Bytes::from_static(b"404")));
                                *r.status_mut() = StatusCode::NOT_FOUND;
                                r
                            }
                        };
                        Ok::<_, std::convert::Infallible>(resp)
                    }
                });
                let _ = hyper::server::conn::http1::Builder::new()
                    .serve_connection(TokioIo::new(stream), svc)
                    .await;
            });
        }
    });
    FileServer {
        addr,
        hits,
        _rt: rt,
    }
}
