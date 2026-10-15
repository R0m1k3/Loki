//! loki-rs — façade Rust de Loki.
//!
//! ```text
//! loki-rs serve         façade sur LOKI_WEB_PORT (8090), Go sur 127.0.0.1:LOKI_GO_PORT (8091)
//! loki-rs self-update   met à jour les binaires dans $LOKI_HOME/bin (jamais bloquant)
//! loki-rs which-bin     dossier d'où prendre loki et loki-rs
//! loki-rs engine <start|stop|restart|status>   supervision du moteur (conteneur)
//! loki-rs version
//! ```

use loki_rs::{auth, child, engine, proxy, update};
use std::net::SocketAddr;
use std::path::PathBuf;
use std::process::ExitCode;
use std::time::{Duration, Instant};

fn main() -> ExitCode {
    let cmd = std::env::args().nth(1).unwrap_or_default();
    match cmd.as_str() {
        "serve" => serve(),
        "engine" => {
            let action = std::env::args().nth(2).unwrap_or_default();
            let go_bin = child::go_bin(std::env::var("LOKI_GO_BIN").ok(), &exe());
            let paths = engine::Paths::from_env();
            // La ligne de commande du moteur composée ici (lot 14) : lancée
            // par le superviseur quand LOKI_SERVE=rust.
            if action == "serve" {
                return loki_rs::serve::run::run(&go_bin, &paths.home);
            }
            let mut opts = engine::Opts::new(go_bin);
            if held_by_facade("LOKI_SERVE") {
                opts.serve = (exe(), vec!["engine".into(), "serve".into()]);
            }
            match engine::run(&action, &paths, &opts) {
                Ok(()) => ExitCode::SUCCESS,
                Err(e) => {
                    // Dernière ligne de stderr : le Go en fait son message d'erreur.
                    eprintln!("[loki-rs engine] {e}");
                    ExitCode::FAILURE
                }
            }
        }
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
            eprintln!(
                "usage : loki-rs serve | engine <action> | self-update | which-bin | version"
            );
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
        // LOKI_CHAT=rust : la conversation est tenue par la façade. Le Go le
        // sait (il remet alors à la façade le trafic de chat du tunnel) et
        // connaît son adresse locale.
        let chat_rust = chat_held_by_facade();
        // LOKI_MODELS=rust : les routes des modèles aussi (le Go remet alors
        // celles du tunnel et ne vide plus les .part).
        let models_rust = models_held_by_facade();
        // LOKI_DICTATE=rust : la dictée vocale aussi (le Go lui demande alors
        // d'arrêter son serveur quand il libère la VRAM).
        let dictate_rust = held_by_facade("LOKI_DICTATE");
        // LOKI_SETTINGS=rust : les réglages partagés (préférences, prompt
        // système, intensité du raisonnement, adresse publique).
        let settings_rust = held_by_facade("LOKI_SETTINGS");
        // LOKI_FILES=rust : les fichiers de la discussion (dépôt, panneau
        // Fichiers, archive, téléchargement, images).
        let files_rust = held_by_facade("LOKI_FILES");
        // LOKI_PUSH=rust : les notifications Web Push (le Go lui délègue ses
        // envois de fin de tour et de fin de tâche).
        let push_rust = held_by_facade("LOKI_PUSH");
        // LOKI_MEM=rust : la mémoire (pages, index, trackers, mode mémoire,
        // interrupteurs du mode agent).
        let mem_rust = held_by_facade("LOKI_MEM");
        // LOKI_PRESETS=rust : les presets (liste, éditeur, aperçus, externes,
        // bascule ; l'application au moteur reste au Go).
        let presets_rust = held_by_facade("LOKI_PRESETS");
        // LOKI_ENGINE=rust : le pilotage du moteur (démarrage, arrêt,
        // déchargement de la VRAM), en conteneur avec le superviseur Rust
        // seulement — sous systemd, le Go pilote son unité.
        // LOKI_SYSTEM=rust : les interrupteurs système (accès internet,
        // navigateur, clé de complétion, écoute réseau du moteur).
        let system_rust = held_by_facade("LOKI_SYSTEM");
        // LOKI_PROJECTS=rust : le hub des projets.
        let projects_rust = held_by_facade("LOKI_PROJECTS");
        // LOKI_MCP=rust : les serveurs MCP (le Go demande alors à la façade
        // les outils MCP et leurs appels).
        let mcp_rust = held_by_facade("LOKI_MCP");
        let engine_rust = held_by_facade("LOKI_ENGINE")
            && match loki_rs::enginectl::usable(
                std::env::var("LOKI_CONTAINER").ok().as_deref(),
                std::env::var("LOKI_ENGINE_SUPERVISOR").ok().as_deref(),
            ) {
                Ok(()) => true,
                Err(why) => {
                    eprintln!("[loki-rs] pilotage du moteur tenu par le Go ({why})");
                    false
                }
            };
        let mut env = vec![
            (auth::INTERNAL_ENV.to_string(), token.clone()),
            // Adresse de l'API interne du Go, héritée par ses enfants :
            // « loki-rs engine serve » (LOKI_SERVE=rust) y lit la configuration.
            ("LOKI_GO_ADDR".to_string(), backend.to_string()),
        ];
        let held = [
            ("LOKI_CHAT", chat_rust),
            ("LOKI_MODELS", models_rust),
            ("LOKI_DICTATE", dictate_rust),
            ("LOKI_SETTINGS", settings_rust),
            ("LOKI_FILES", files_rust),
            ("LOKI_PUSH", push_rust),
            ("LOKI_MEM", mem_rust),
            ("LOKI_PRESETS", presets_rust),
            ("LOKI_ENGINE", engine_rust),
            ("LOKI_SYSTEM", system_rust),
            ("LOKI_PROJECTS", projects_rust),
            ("LOKI_MCP", mcp_rust),
        ];
        for (var, on) in held {
            if on {
                env.push((var.into(), "rust".into()));
            }
        }
        if held.iter().any(|(_, on)| *on) {
            env.push(("LOKI_FACADE_ADDR".into(), format!("127.0.0.1:{web_port}")));
        }
        let go_bin_path = go_bin.clone();
        let sup = child::Supervisor::spawn(go_bin, go_port, env);
        wait_for(backend, Duration::from_secs(30)).await;
        let engine_paths = engine::Paths::from_env();
        // Le Go peut garder la conversation malgré LOKI_CHAT=rust (réglage que
        // la façade ne sait pas encore honorer) : sa décision fait foi.
        let chat_rust = chat_rust
            && match go_keeps_chat(backend, &token).await {
                Ok(None) => true,
                Ok(Some(why)) => {
                    eprintln!("[loki-rs] conversation tenue par le Go ({why})");
                    false
                }
                Err(e) => {
                    eprintln!("[loki-rs] conversation : {e}");
                    sup.shutdown(Duration::from_secs(10)).await;
                    return ExitCode::FAILURE;
                }
            };
        let chat = if chat_rust {
            match start_chat(backend, &token, &engine_paths.home).await {
                Ok(c) => Some(c),
                Err(e) => {
                    eprintln!("[loki-rs] conversation : {e}");
                    sup.shutdown(Duration::from_secs(10)).await;
                    return ExitCode::FAILURE;
                }
            }
        } else {
            None
        };
        let models = if models_rust {
            Some(start_models(backend, &token, &engine_paths.home).await)
        } else {
            None
        };

        let dictate = dictate_rust.then(|| {
            eprintln!("[loki-rs] dictée tenue par la façade (LOKI_DICTATE=rust)");
            std::sync::Arc::new(loki_rs::dictate::Dictation::new(&engine_paths.home))
        });

        let files = if files_rust {
            Some(start_files(backend, &token).await)
        } else {
            None
        };

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
        let mem_home = engine_paths.home.clone();
        // Le superviseur reçoit ce que le Go lui passait (rustSvcAction), et de
        // quoi joindre l'API interne du Go pour « loki-rs engine serve ».
        let engine_ctl = engine_rust.then(|| {
            std::sync::Arc::new(loki_rs::enginectl::EngineCtl::new(
                exe(),
                vec![
                    (
                        "LOKI_GO_BIN".into(),
                        go_bin_path.to_string_lossy().into_owned(),
                    ),
                    (
                        "LOKI_HOME".into(),
                        engine_paths.home.to_string_lossy().into_owned(),
                    ),
                    ("LOKI_SERVICE".into(), engine_paths.service.clone()),
                    ("LOKI_GO_ADDR".into(), backend.to_string()),
                    (auth::INTERNAL_ENV.into(), token.clone()),
                ],
            ))
        });
        let cfg = proxy::Config {
            backend,
            internal_token: token,
            host_env: auth::HostEnv::from_env(),
            engine: engine_paths,
            chat,
            models,
            dictate,
            settings: settings_rust,
            files,
            push: push_rust.then(|| std::sync::Arc::new(loki_rs::push::Push::new())),
            mem: mem_rust
                .then(|| std::sync::Arc::new(loki_rs::memory::Memory::new(mem_home.clone()))),
            presets: presets_rust,
            engine_ctl,
            system: system_rust.then(|| std::sync::Arc::new(loki_rs::system::System::new())),
            projects: projects_rust,
            mcp: mcp_rust.then(|| std::sync::Arc::new(loki_rs::mcp::Mcp::new(mem_home))),
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

/// `LOKI_CHAT=rust` : conversation tenue par la façade (Go par défaut).
fn chat_held_by_facade() -> bool {
    std::env::var("LOKI_CHAT").is_ok_and(|v| v.trim().eq_ignore_ascii_case("rust"))
}

/// `LOKI_MODELS=rust` : routes des modèles tenues par la façade.
fn models_held_by_facade() -> bool {
    held_by_facade("LOKI_MODELS")
}

/// `<VAR>=rust` : ce lot est tenu par la façade (le Go par défaut).
fn held_by_facade(var: &str) -> bool {
    std::env::var(var).is_ok_and(|v| v.trim().eq_ignore_ascii_case("rust"))
}

/// Les `.part` d'un téléchargement tué ne reprendront jamais : vidés au
/// démarrage (le Go ne le fait plus quand la façade les tient).
async fn start_models(
    backend: SocketAddr,
    token: &str,
    home: &std::path::Path,
) -> std::sync::Arc<loki_rs::models::Models> {
    use loki_rs::{downloads, gguf, store};
    let st = store::Store::new(backend, token.to_string());
    let stored = gguf::parse_stored_model_dirs(
        st.get(store::BK_STATE, gguf::STATE_MODEL_DIRS_KEY)
            .await
            .ok()
            .flatten()
            .as_deref(),
    );
    let dirs = gguf::model_dirs(
        home,
        &stored,
        std::env::var(gguf::MODEL_DIRS_ENV).ok().as_deref(),
    );
    let _ = tokio::task::spawn_blocking(move || downloads::clean_stale_part_files(&dirs)).await;
    eprintln!("[loki-rs] modèles tenus par la façade (LOKI_MODELS=rust)");
    std::sync::Arc::new(loki_rs::models::Models::new())
}

/// Les envois interrompus (process arrêté en plein dépôt) ne reprendront
/// jamais : leurs `.part` sont vidés au démarrage (le Go ne le fait plus quand
/// la façade tient les fichiers).
async fn start_files(
    backend: SocketAddr,
    token: &str,
) -> std::sync::Arc<loki_rs::convfiles::ConvFiles> {
    let st = loki_rs::store::Store::new(backend, token.to_string());
    let root = st
        .internal_json(
            hyper::Method::POST,
            "/internal/v1/conv/workspace",
            Some(&serde_json::json!({})),
        )
        .await
        .ok()
        .and_then(|v| v.get("root").and_then(|r| r.as_str()).map(str::to_string));
    if let Some(root) = root {
        let _ = tokio::task::spawn_blocking(move || {
            loki_rs::convfiles::clean_stale_upload_parts(&root)
        })
        .await;
    }
    eprintln!("[loki-rs] fichiers de la discussion tenus par la façade (LOKI_FILES=rust)");
    std::sync::Arc::new(loki_rs::convfiles::ConvFiles::default())
}

/// Le Go garde-t-il la conversation ? (quelques essais : il vient de démarrer)
async fn go_keeps_chat(backend: SocketAddr, token: &str) -> Result<Option<String>, String> {
    let store = loki_rs::store::Store::new(backend, token.to_string());
    let mut last = String::new();
    for _ in 0..5 {
        match loki_rs::chat::generate::go_keeps_chat(&store).await {
            Ok(v) => return Ok(v),
            Err(e) => last = e,
        }
        tokio::time::sleep(Duration::from_millis(500)).await;
    }
    Err(last)
}

/// Charge la discussion active et prépare le service des tours.
async fn start_chat(
    backend: SocketAddr,
    token: &str,
    home: &std::path::Path,
) -> Result<std::sync::Arc<loki_rs::chat::generate::ChatService>, String> {
    use loki_rs::chat::{conv::Conversation, generate::ChatService, host::ChatShared, sessions};
    let store = loki_rs::store::Store::new(backend, token.to_string());
    let s = sessions::Sessions::new(
        store.clone(),
        Conversation::new(),
        sessions::GoSessionHooks(store.clone()),
    );
    s.load().await?;
    let shared = ChatShared::for_sessions(&s, store, home.join("chatimg"));
    eprintln!("[loki-rs] conversation tenue par la façade (LOKI_CHAT=rust)");
    Ok(ChatService::new(
        std::sync::Arc::new(s),
        std::sync::Arc::new(shared),
    ))
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
