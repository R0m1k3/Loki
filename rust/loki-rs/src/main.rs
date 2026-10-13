//! loki-rs — façade Rust de Loki.
//!
//! ```text
//! loki-rs serve         façade sur LOKI_WEB_PORT (8090), Go sur 127.0.0.1:LOKI_GO_PORT (8091)
//! loki-rs self-update   met à jour les binaires dans $LOKI_HOME/bin (jamais bloquant)
//! loki-rs which-bin     dossier d'où prendre loki et loki-rs
//! loki-rs version
//! ```

use loki_rs::{auth, child, proxy, update};
use std::net::SocketAddr;
use std::path::PathBuf;
use std::process::ExitCode;
use std::time::{Duration, Instant};

fn main() -> ExitCode {
    let cmd = std::env::args().nth(1).unwrap_or_default();
    match cmd.as_str() {
        "serve" => serve(),
        "self-update" => {
            let outcome = update::run(&update::UpdateConfig::from_env());
            eprintln!("[loki-rs update] {}", outcome.message());
            ExitCode::SUCCESS
        }
        "which-bin" => {
            let cfg = update::UpdateConfig::from_env();
            let dir = update::state::select_dir(
                &cfg.data_bin,
                &image_dir(),
                cfg.image_build,
                cfg.image_abi,
            );
            println!("{}", dir.display());
            ExitCode::SUCCESS
        }
        "version" | "--version" | "-V" => {
            let cfg = update::UpdateConfig::from_env();
            println!(
                "loki-rs {} (build image {}, ABI {})",
                env!("CARGO_PKG_VERSION"),
                cfg.image_build,
                cfg.image_abi
            );
            ExitCode::SUCCESS
        }
        _ => {
            eprintln!("usage : loki-rs serve | self-update | which-bin | version");
            ExitCode::from(2)
        }
    }
}

fn exe() -> PathBuf {
    std::env::current_exe().unwrap_or_else(|_| PathBuf::from("loki-rs"))
}

fn image_dir() -> PathBuf {
    exe()
        .parent()
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from("."))
}

fn env_port(key: &str, default: u16) -> u16 {
    std::env::var(key)
        .ok()
        .and_then(|v| v.trim().parse().ok())
        .unwrap_or(default)
}

fn serve() -> ExitCode {
    let rt = match tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
    {
        Ok(rt) => rt,
        Err(e) => {
            eprintln!("[loki-rs] runtime : {e}");
            return ExitCode::FAILURE;
        }
    };
    rt.block_on(async {
        let web_port = env_port("LOKI_WEB_PORT", 8090);
        let go_port = env_port("LOKI_GO_PORT", 8091);
        let go_bin = child::go_bin(std::env::var("LOKI_GO_BIN").ok(), &exe());
        let backend = SocketAddr::from(([127, 0, 0, 1], go_port));

        eprintln!(
            "[loki-rs] serveur Go : {} web {go_port} (local)",
            go_bin.display()
        );
        // Jeton de l'API interne du Go : tiré au hasard à chaque démarrage,
        // connu seulement de la façade et du Go qu'elle lance.
        let token = match internal_token() {
            Ok(t) => t,
            Err(e) => {
                eprintln!("[loki-rs] aléa système indisponible : {e}");
                return ExitCode::FAILURE;
            }
        };
        let sup = child::Supervisor::spawn(
            go_bin,
            go_port,
            vec![(auth::INTERNAL_ENV.to_string(), token.clone())],
        );
        wait_for(backend, Duration::from_secs(30)).await;

        let addr = SocketAddr::from(([0, 0, 0, 0], web_port));
        let listener = match tokio::net::TcpListener::bind(addr).await {
            Ok(l) => l,
            Err(e) => {
                eprintln!("[loki-rs] écoute sur {addr} impossible : {e}");
                sup.shutdown(Duration::from_secs(10)).await;
                return ExitCode::FAILURE;
            }
        };
        eprintln!("[loki-rs] façade : http://{addr} → {backend}");
        let cfg = proxy::Config {
            backend,
            internal_token: token,
            host_env: auth::HostEnv::from_env(),
        };
        let res = proxy::serve(listener, cfg, shutdown_signal()).await;
        eprintln!("[loki-rs] arrêt demandé — arrêt du serveur Go");
        sup.shutdown(Duration::from_secs(10)).await;
        match res {
            Ok(()) => ExitCode::SUCCESS,
            Err(e) => {
                eprintln!("[loki-rs] {e}");
                ExitCode::FAILURE
            }
        }
    })
}

fn internal_token() -> Result<String, getrandom::Error> {
    let mut b = [0u8; 32];
    getrandom::fill(&mut b)?;
    Ok(hex::encode(b))
}

/// Attend que le Go accepte les connexions, sans bloquer au-delà de `max` :
/// la façade ouvre quand même et répond 502 en attendant.
async fn wait_for(addr: SocketAddr, max: Duration) {
    let t0 = Instant::now();
    while t0.elapsed() < max {
        if tokio::net::TcpStream::connect(addr).await.is_ok() {
            return;
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
    eprintln!("[loki-rs] le serveur Go ne répond pas encore — façade ouverte quand même");
}

async fn shutdown_signal() {
    #[cfg(unix)]
    {
        use tokio::signal::unix::{SignalKind, signal};
        match signal(SignalKind::terminate()) {
            Ok(mut term) => {
                tokio::select! {
                    _ = term.recv() => {}
                    _ = tokio::signal::ctrl_c() => {}
                }
            }
            Err(_) => {
                let _ = tokio::signal::ctrl_c().await;
            }
        }
    }
    #[cfg(not(unix))]
    {
        let _ = tokio::signal::ctrl_c().await;
    }
}
