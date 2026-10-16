//! Lot 28 : l'optimiseur tenu par la façade (`LOKI_TUNE=rust`).
//!
//! Les essais passent par de vrais processus : ce binaire de test joue
//! « loki serve » (la seule fonction dont le nom contient « serve »,
//! sélectionnée par l'argument que la façade passe). À blanc, il écrit la
//! ligne de commande de l'essai ; pour de vrai, il sert un faux llama-server
//! dont le prefill est d'autant plus rapide que le micro-lot est petit.

mod common;

use std::collections::HashMap;
use std::io::{BufRead, BufReader, Read, Write};
use std::net::TcpListener;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use common::fakego;
use loki_rs::tune::job::Tune;
use serde_json::{Value, json};

const FAUX: &str = "LOKI_TUNE_FAUX";
const VRAM_NOTE: &str = "VRAM libre non mesurable (nvidia-smi absent) : ni micro-lot plus grand ni marges --fit essayés";
const BUSY: &str = "optimisation en cours (loki tune) : le moteur est réservé aux essais — attends la fin ou annule-la";

// ---------------------------------------------------------------------------
// Le faux moteur
// ---------------------------------------------------------------------------

/// Ce que le faux moteur annonce : son contexte, s'il travaille déjà, et ses
/// débits (0 : prefill selon le micro-lot, decode à 50 jetons/s) ; `down` :
/// /health en 503 ; `flat_props` : /props sans `default_generation_settings`
/// (le contexte au premier niveau seulement).
#[derive(Default)]
struct Knobs {
    n_ctx: Mutex<i64>,
    busy: Mutex<bool>,
    pps: Mutex<f64>,
    tg: Mutex<f64>,
    down: Mutex<bool>,
    flat_props: Mutex<bool>,
}

/// Un faux llama-server (fil à part, jusqu'à la fin du processus).
fn fake_llama(listener: TcpListener, ubatch: i64, knobs: Arc<Knobs>) {
    let last = Arc::new(Mutex::new(0i64));
    std::thread::spawn(move || {
        for st in listener.incoming().flatten() {
            let (last, knobs) = (last.clone(), knobs.clone());
            std::thread::spawn(move || {
                let _ = answer(st, ubatch, &knobs, &last);
            });
        }
    });
}

fn answer(
    st: std::net::TcpStream,
    ubatch: i64,
    knobs: &Knobs,
    last: &Mutex<i64>,
) -> std::io::Result<()> {
    let n_ctx = *knobs.n_ctx.lock().unwrap();
    let mut r = BufReader::new(st.try_clone()?);
    let mut line = String::new();
    r.read_line(&mut line)?;
    let path = line.split(' ').nth(1).unwrap_or("").to_string();
    let mut len = 0usize;
    loop {
        let mut h = String::new();
        r.read_line(&mut h)?;
        if h.trim().is_empty() {
            break;
        }
        if let Some((k, v)) = h.split_once(':')
            && k.eq_ignore_ascii_case("content-length")
        {
            len = v.trim().parse().unwrap_or(0);
        }
    }
    let mut body = vec![0u8; len];
    r.read_exact(&mut body)?;
    let req: Value = serde_json::from_slice(&body).unwrap_or(Value::Null);
    // Un seul slot : comme llama-server, un id_slot qu'il n'a pas est refusé.
    if path == "/v1/chat/completions" && req["id_slot"].as_i64().is_some_and(|i| i >= 1) {
        let b = r#"{"error":{"code":400,"message":"slot id invalide"}}"#;
        let mut w = st;
        return write!(
            w,
            "HTTP/1.1 400 Bad Request\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{b}",
            b.len()
        );
    }
    if path == "/health" && *knobs.down.lock().unwrap() {
        let mut w = st;
        return w.write_all(
            b"HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
        );
    }
    let out = match path.as_str() {
        "/health" => json!({ "status": "ok" }),
        "/props" if *knobs.flat_props.lock().unwrap() => json!({
            "n_ctx": n_ctx, "total_slots": 1, "build_info": "b7000-faux",
        }),
        "/props" => json!({
            "n_ctx": n_ctx, "total_slots": 1, "build_info": "b7000-faux",
            "default_generation_settings": { "n_ctx": n_ctx },
        }),
        "/slots" => json!([{ "id": 0, "is_processing": *knobs.busy.lock().unwrap() }]),
        "/tokenize" => {
            let n = req["content"].as_str().unwrap_or("").len() / 4;
            json!({ "tokens": vec![0; n.max(1)] })
        }
        "/v1/chat/completions" => {
            let chars: usize = req["messages"]
                .as_array()
                .map(|m| {
                    m.iter()
                        .map(|x| x["content"].as_str().unwrap_or("").len())
                        .sum()
                })
                .unwrap_or(0);
            let total = (chars / 4).max(1) as i64;
            let mut l = last.lock().unwrap();
            let cached = if req["cache_prompt"] == json!(true) && *l <= total {
                *l
            } else {
                0
            };
            *l = total;
            let new = total - cached;
            // Le prefill ralentit avec le micro-lot ; le decode, non.
            let pps = match *knobs.pps.lock().unwrap() {
                0.0 => 4_000_000.0 / ubatch as f64,
                v => v,
            };
            let tg = match *knobs.tg.lock().unwrap() {
                0.0 => 50.0,
                v => v,
            };
            let n_gen = req["max_tokens"].as_i64().unwrap_or(16);
            json!({
                "choices": [{ "message": { "content": "ok" } }],
                "usage": { "prompt_tokens": total },
                "timings": {
                    "prompt_n": new, "prompt_ms": (new as f64 / pps * 1000.0).max(1.0),
                    "predicted_n": n_gen, "predicted_ms": n_gen as f64 / tg * 1000.0,
                    "cache_n": cached,
                },
            })
        }
        _ => {
            let mut w = st;
            return w.write_all(
                b"HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
            );
        }
    };
    let b = out.to_string();
    let mut w = st;
    write!(
        w,
        "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{b}",
        b.len()
    )
}

/// « loki serve » pour l'optimiseur : ce binaire relancé par la façade avec
/// `LOKI_TUNE_FAUX`. Ailleurs : rien.
#[test]
fn serve() {
    if std::env::var(FAUX).is_err() {
        return;
    }
    let dir = PathBuf::from(std::env::var("LOKI_TUNE_TRIAL").unwrap());
    let cfg =
        loki_rs::presets::parse_env(&std::fs::read_to_string(dir.join("config.env")).unwrap());
    let get = |k: &str| cfg.get(k).cloned().unwrap_or_default();
    let ub = get("UBATCH");
    let die = |msg: &str| -> ! {
        let _ = std::io::stderr().write_all(format!("{msg}\n").as_bytes());
        std::process::exit(1);
    };
    if std::env::var("LOKI_TUNE_DRYRUN").as_deref() == Ok("1") {
        if get("REFUS") == ub {
            die(&format!("\x1b[31m[err]\x1b[0m UBATCH={ub} refusé"));
        }
        let mut args = vec!["-m".to_string(), get("MODEL"), "-c".into(), get("CTX")];
        for (k, f) in [("UBATCH", "-ub"), ("BATCH", "-b")] {
            if !get(k).is_empty() && !(k == "BATCH" && !get("SANS_BATCH").is_empty()) {
                args.extend([f.to_string(), get(k)]);
            }
        }
        // Une ligne qui change ce que le modèle calcule.
        if get("DENATURE") == ub {
            args.push("--no-mmap".into());
        }
        args.extend(["--host".into(), get("HOST"), "--port".into(), get("PORT")]);
        let launch = json!({
            "bin": "/opt/faux/llama-server", "args": args,
            "probe": { "help": "", "gpus": 0, "vram_mib": 0,
                "model_bytes": get("MODEL_BYTES").parse::<i64>().unwrap_or(1000), "moe": false,
                "cpu_threads": get("CPU_THREADS").parse::<i64>().unwrap_or(8) },
        });
        std::fs::write(dir.join("launch.json"), launch.to_string()).unwrap();
        return;
    }
    if get("ECHEC") == ub {
        die("llama_model_load: error loading model: out of memory");
    }
    let port: u16 = get("PORT").parse().unwrap();
    let l = TcpListener::bind(("127.0.0.1", port)).unwrap();
    // Un essai qui ne répond jamais : il tient son port (sinon un autre test
    // lancé en parallèle peut le prendre et répondre à sa place) sans jamais
    // accepter.
    if get("MUET") == ub {
        std::thread::sleep(Duration::from_secs(600));
        drop(l);
        return;
    }
    let _ = std::io::stdout().write_all(b"load_tensors: offloaded 33/33 layers to GPU\n");
    let knobs = Knobs::default();
    // Un essai qui ne garde pas le contexte de la référence.
    let ctx: i64 = get("CTX").parse().unwrap();
    *knobs.n_ctx.lock().unwrap() = if get("CTX_FAUX") == ub { ctx / 2 } else { ctx };
    *knobs.flat_props.lock().unwrap() = get("PROPS_PLAT") == "1";
    // Débits imposés par micro-lot : « 1024:2000,512:1953 ».
    let speed = |k: &str| {
        get(k)
            .split(',')
            .filter_map(|p| p.split_once(':'))
            .find(|(u, _)| *u == ub)
            .and_then(|(_, v)| v.parse::<f64>().ok())
            .unwrap_or(0.0)
    };
    *knobs.pps.lock().unwrap() = speed("VITESSE");
    *knobs.tg.lock().unwrap() = speed("DECODE");
    fake_llama(l, ub.parse().unwrap_or(2048), Arc::new(knobs));
    // Arrêté par la façade ; un essai oublié (test interrompu) s'en va seul.
    std::thread::sleep(Duration::from_secs(600));
}

// ---------------------------------------------------------------------------
// Le banc
// ---------------------------------------------------------------------------

struct Tn {
    facade: std::net::SocketAddr,
    go: Arc<fakego::FakeGo>,
    _stop: tokio::sync::oneshot::Sender<()>,
    home: PathBuf,
    port: u16,
    main: Arc<Knobs>,
    _tmp: tempfile::TempDir,
}

const PRESET: &str = "# NAME=Dense\nCTX=32768\nUBATCH=2048\nREASONING=off\nCACHE_ISOLATE=off\n";

async fn start() -> Tn {
    start_with(|_, _| {}, |_, _| {}).await
}

/// [`start`], l'optimiseur réglé par `ft` et la façade par `fo` (le
/// répertoire en second).
async fn start_with(
    ft: impl FnOnce(&mut Tune, &std::path::Path),
    fo: impl FnOnce(&mut fakego::Opts, &std::path::Path),
) -> Tn {
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().to_path_buf();
    let mut t = Tune::new(home.clone(), std::env::current_exe().unwrap());
    t.trial_env = vec![(FAUX.into(), "1".into())];
    t.poll = Duration::from_millis(20);
    t.probe_poll = Duration::from_millis(20);
    t.settle = Duration::ZERO;
    t.load_timeout = Duration::from_secs(20);
    t.lease_ttl = Duration::from_millis(300);
    t.gpu_pause = Duration::ZERO;
    ft(&mut t, &home);
    let tune = Arc::new(t);
    // Moteur et modèle présents : la relance du vrai moteur passe ses
    // vérifications.
    std::fs::write(home.join("llama-server"), "x").unwrap();
    std::fs::write(home.join("m.gguf"), "x").unwrap();
    let mut opts = fakego::Opts {
        tune: Some(tune),
        ..Default::default()
    };
    fo(&mut opts, &home);
    let (facade, go, stop, _) = fakego::start_with(home.clone(), opts).await;
    // Le vrai moteur, en service.
    let l = TcpListener::bind("127.0.0.1:0").unwrap();
    let port = l.local_addr().unwrap().port();
    let main: Arc<Knobs> = Arc::default();
    *main.n_ctx.lock().unwrap() = 32768;
    fake_llama(l, 2048, main.clone());
    let tn = Tn {
        facade,
        go,
        _stop: stop,
        home,
        port,
        main,
        _tmp: tmp,
    };
    tn.preset(PRESET);
    *tn.go.service_active.lock().unwrap() = true;
    *tn.go.apply_presets.lock().unwrap() = true;
    tn
}

impl Tn {
    /// Le preset « dense », et la configuration qui lui correspond (elle
    /// seule).
    fn preset(&self, content: &str) {
        let dir = self.home.join("presets");
        std::fs::create_dir_all(&dir).unwrap();
        let h = self.home.display();
        let content = format!(
            "{content}MODEL={h}/m.gguf\nBIN={h}/llama-server\nPORT={}\n",
            self.port
        );
        std::fs::write(dir.join("dense.env"), &content).unwrap();
        for k in self.go.config_map().keys() {
            self.go.del("config", k);
        }
        for (k, v) in loki_rs::presets::parse_env(&content) {
            self.go.set("config", &k, &v);
        }
    }

    async fn call(&self, method: &str, path: &str, body: &str) -> (u16, Value) {
        let (code, b) = fakego::send(self.facade, method, path, &[], body).await;
        let v = if b.is_empty() {
            Value::Null
        } else {
            serde_json::from_slice(&b)
                .unwrap_or_else(|_| panic!("illisible : {}", String::from_utf8_lossy(&b)))
        };
        (code.as_u16(), v)
    }

    async fn status(&self) -> Value {
        self.call("GET", "/api/tune/status", "").await.1
    }

    async fn wait(&self, what: &str, key: &str) -> Value {
        for _ in 0..6000 {
            let st = self.status().await;
            // Notre propre verrou n'est jamais « une optimisation ailleurs ».
            assert_eq!(st.get("elsewhere"), None, "{st}");
            if st[key] != json!(true) {
                return st;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        panic!("{what} : ne finit pas");
    }

    /// Application dans le preset ; son issue (`apply` de l'état).
    async fn apply_preset(&self) -> Value {
        let (code, out) = self
            .call("POST", "/api/tune/apply", r#"{"target":"preset"}"#)
            .await;
        assert_eq!((code, out), (202, json!({ "ok": true })));
        self.wait("application", "applying").await["apply"].clone()
    }

    fn config(&self) -> HashMap<String, String> {
        ["MODEL", "CTX", "UBATCH", "BATCH", "PORT"]
            .iter()
            .filter_map(|k| self.go.get("config", k).map(|v| (k.to_string(), v)))
            .collect()
    }
}

fn trials(res: &Value) -> Vec<(String, String, String)> {
    res["trials"]
        .as_array()
        .map(|a| {
            a.iter()
                .map(|t| {
                    (
                        t["label"].as_str().unwrap_or("").to_string(),
                        t["status"].as_str().unwrap_or("").to_string(),
                        t["why"].as_str().unwrap_or("").to_string(),
                    )
                })
                .collect()
        })
        .unwrap_or_default()
}

// ---------------------------------------------------------------------------
// Les tests
// ---------------------------------------------------------------------------

/// Une optimisation entière : vrai moteur arrêté puis relancé, essais
/// composés à blanc puis lancés, mesurés, retenus ; puis l'application dans
/// une copie, dans le preset, et le refus d'un preset changé depuis.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn optimisation_complete() {
    let tn = start().await;
    let (code, out) = tn
        .call("POST", "/api/tune", r#"{"stages":["lots"],"budget_min":5}"#)
        .await;
    assert_eq!((code, out), (202, json!({ "ok": true })));
    assert_eq!(*tn.go.lease_whys.lock().unwrap(), vec!["tune".to_string()]);
    // Pendant la mesure : le verrou de LOKI_HOME, au nom de la façade.
    let mut seen_lock = false;
    for _ in 0..2000 {
        if let Some(o) = loki_rs::tune::lock::read_owner(&tn.home.join("tune.lock")) {
            assert_eq!(o.owner.pid, i64::from(std::process::id()));
            assert_eq!(o.via, "web");
            assert!(o.main_was_active);
            seen_lock = true;
            break;
        }
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    assert!(seen_lock, "verrou jamais posé");
    // Chaque essai lancé est noté dans le verrou : un Loki tué pendant la
    // mesure l'arrête à son redémarrage.
    let mut trial = None;
    for _ in 0..4000 {
        if let Some(t) =
            loki_rs::tune::lock::read_owner(&tn.home.join("tune.lock")).and_then(|o| o.trial)
        {
            trial = Some(t);
            break;
        }
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    let trial = trial.expect("essai jamais noté dans le verrou");
    assert!(trial.pid > 0 && trial.pid != i64::from(std::process::id()));
    assert_eq!(
        trial.exes[0],
        std::env::current_exe()
            .unwrap()
            .to_string_lossy()
            .into_owned()
    );
    let (code, out) = tn.call("POST", "/api/tune", "{}").await;
    assert_eq!(code, 409);
    assert_eq!(out["error"], json!(BUSY));
    // Rien ne s'applique pendant la mesure.
    assert_eq!(
        tn.call("POST", "/api/tune/apply", r#"{"target":"preset"}"#)
            .await,
        (409, json!({ "ok": false, "error": BUSY }))
    );
    let st = tn.wait("optimisation", "running").await;
    assert_eq!(st["error"], Value::Null, "{st}");
    // La progression : la référence puis chaque essai, une fois chacun.
    let shown: Vec<(&str, &str)> = st["trials"]
        .as_array()
        .unwrap()
        .iter()
        .map(|t| (t["label"].as_str().unwrap(), t["status"].as_str().unwrap()))
        .collect();
    assert_eq!(shown.len(), 5, "{shown:?}");
    assert_eq!(shown[0], ("configuration actuelle", "mesuré"));
    assert!(shown.iter().all(|t| t.1 != "en cours"), "{shown:?}");
    assert_eq!(st["eta_sec"], json!(0));
    let res = &st["result"];
    assert_eq!(res["preset_id"], json!("dense"));
    assert_eq!(res["preset_name"], json!("Dense"));
    assert_eq!(res["engine"], json!("b7000-faux"));
    assert_eq!(res["engine_build"], json!(7000));
    assert_eq!(
        (res["n_ctx"].clone(), res["slots"].clone()),
        (json!(32768), json!(1))
    );
    assert_eq!(res["depth"], json!(16384));
    assert_eq!(res["baseline"]["status"], json!("mesuré"));
    assert_eq!(res["baseline"]["runs"].as_array().unwrap().len(), 2);
    assert_eq!(res["baseline"]["offloaded"], json!("33/33"));
    let tr = trials(res);
    assert_eq!(
        tr.iter().map(|t| t.0.as_str()).collect::<Vec<_>>(),
        [
            "UBATCH=512 BATCH=2048",
            "UBATCH=512 BATCH=1024",
            "UBATCH=1024 BATCH=2048",
            "UBATCH=2048 BATCH=4096"
        ]
    );
    assert_eq!(tr[0].1, "retenu", "{tr:?}");
    // Confirmé seulement s'il promet : le premier, pas les suivants.
    let runs = |i: usize| res["trials"][i]["runs"].as_array().unwrap().len();
    assert_eq!((runs(0), runs(1), runs(2), runs(3)), (2, 1, 1, 1));
    assert!(
        tr[0].2.starts_with('−') && tr[0].2.contains("par tour (seuil"),
        "{tr:?}"
    );
    assert_eq!(
        res["best"],
        json!(tr.iter().rfind(|t| t.1 == "retenu").unwrap().0)
    );
    assert_eq!(res["set"], json!({ "BATCH": "2048", "UBATCH": "512" }));
    assert_eq!(res["old"], json!({ "BATCH": "", "UBATCH": "2048" }));
    let (base_turn, best_turn) = (
        res["baseline"]["turn_sec"].as_f64().unwrap(),
        res["trials"][0]["turn_sec"].as_f64().unwrap(),
    );
    let gain = res["gain"].as_f64().unwrap();
    assert!(
        (gain - (base_turn - best_turn) / base_turn).abs() < 1e-12,
        "{gain} {base_turn} {best_turn}"
    );
    // Aussi rapide que le meilleur : « ≈ 0 % ».
    assert_eq!(tr[1].2, "≈ 0 % (seuil 3.0 %)");
    let skipped: Vec<&str> = res["skipped"]
        .as_array()
        .unwrap()
        .iter()
        .map(|s| s.as_str().unwrap())
        .collect();
    assert!(skipped.contains(&"marges : non demandé"), "{skipped:?}");
    assert_eq!(res["notes"], json!([VRAM_NOTE]));
    // Le vrai moteur : arrêté, puis relancé, sans le refus de l'optimiseur ;
    // la dictée coupée ; le verrou rendu ; le bail aussi.
    assert_eq!(*tn.go.service_os.lock().unwrap(), ["stop", "start"]);
    assert_eq!(*tn.go.dictate_stops.lock().unwrap(), 1);
    assert!(!tn.home.join("tune.lock").exists(), "verrou non rendu");
    assert!(tn.go.lease.lock().unwrap().is_none(), "bail non rendu");
    // Chaque essai a sa copie de configuration, sur une adresse privée ;
    // config.env n'est jamais touché.
    let e1 = loki_rs::presets::parse_env(
        &std::fs::read_to_string(tn.home.join("tune/run/e01/config.env")).unwrap(),
    );
    assert_eq!(e1.get("HOST").map(String::as_str), Some("127.0.0.1"));
    assert_ne!(e1.get("PORT"), Some(&tn.port.to_string()));
    assert_eq!(tn.config()["UBATCH"], "2048");
    // Enregistré par preset.
    let saved: Value = serde_json::from_str(&tn.go.get("state", "tune_results").unwrap()).unwrap();
    assert_eq!(saved["dense"]["best"], res["best"]);
    let (_, last) = tn.call("GET", "/api/tune/last?id=dense", "").await;
    assert_eq!(last["ok"], json!(true));
    assert_eq!(last["preset_changed"], json!(false));
    assert_eq!(last["result"], *res);
    // Le build du moteur actuel est inconnu (binaire illisible) : rien à
    // signaler.
    assert_eq!(last.get("engine_changed"), None, "{last}");
    assert_eq!(
        tn.call("POST", "/api/tune/apply", r#"{"target":"ailleurs"}"#)
            .await,
        (400, json!({ "ok": false, "error": "cible inconnue" }))
    );

    // Copie : rien d'autre ne bouge.
    let (code, out) = tn
        .call("POST", "/api/tune/apply", r#"{"target":"copy"}"#)
        .await;
    assert_eq!(code, 200, "{out}");
    let msg = out["message"].as_str().unwrap();
    assert!(
        msg.starts_with("copie « Dense (optimisé) » créée ("),
        "{msg}"
    );
    assert!(msg.ends_with(") — bascule dessus pour l'utiliser"), "{msg}");
    assert!(
        std::fs::read_to_string(tn.home.join("presets/dense.env"))
            .unwrap()
            .contains("UBATCH=2048")
    );
    assert_eq!(*tn.go.service_os.lock().unwrap(), ["stop", "start"]);

    // Le preset lui-même : réécrit, appliqué, moteur redémarré et sondé.
    let (code, out) = tn
        .call("POST", "/api/tune/apply", r#"{"target":"preset"}"#)
        .await;
    assert_eq!((code, out), (202, json!({ "ok": true })));
    let st = tn.wait("application", "applying").await;
    assert_eq!(st["apply"]["error"], json!(""), "{st}");
    assert_eq!(
        st["apply"]["message"],
        json!("preset « Dense » optimisé et vérifié")
    );
    let log: Vec<&str> = st["apply"]["log"]
        .as_array()
        .unwrap()
        .iter()
        .map(|s| s.as_str().unwrap())
        .collect();
    assert!(
        log[0].starts_with("écriture du preset (sauvegarde : "),
        "{log:?}"
    );
    assert_eq!(
        &log[1..],
        [
            "redémarrage du moteur",
            "attente du moteur",
            "sonde : prompt de 16384 jetons puis decode"
        ]
    );
    let preset = std::fs::read_to_string(tn.home.join("presets/dense.env")).unwrap();
    assert!(
        preset.contains("UBATCH=512") && preset.contains("# NAME=Dense"),
        "{preset}"
    );
    assert_eq!(tn.config()["UBATCH"], "512");
    assert_eq!(
        *tn.go.service_os.lock().unwrap(),
        ["stop", "start", "restart"]
    );
    assert!(!tn.home.join("tune.lock").exists(), "verrou non rendu");
    // Le preset a changé depuis la mesure : plus rien ne s'applique.
    let (code, out) = tn
        .call("POST", "/api/tune/apply", r#"{"target":"preset"}"#)
        .await;
    assert_eq!(
        (code, out),
        (
            200,
            json!({ "ok": false, "error": "le preset a changé depuis la mesure : relance l'optimisation" })
        )
    );
    let (_, last) = tn.call("GET", "/api/tune/last?id=dense", "").await;
    assert_eq!(last["preset_changed"], json!(true));
}

/// Une sonde qui échoue après application : l'ancienne version revient —
/// preset ET configuration — et le moteur est redémarré dessus.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn application_defaite() {
    let tn = start().await;
    tn.call("POST", "/api/tune", r#"{"stages":["lots"]}"#).await;
    let st = tn.wait("optimisation", "running").await;
    assert_eq!(st["result"]["set"]["UBATCH"], json!("512"), "{st}");
    // Le moteur relancé ne garde pas son contexte.
    *tn.main.n_ctx.lock().unwrap() = 4096;
    let (code, _) = tn
        .call("POST", "/api/tune/apply", r#"{"target":"preset"}"#)
        .await;
    assert_eq!(code, 202);
    let st = tn.wait("application", "applying").await;
    assert_eq!(
        st["apply"]["error"],
        json!("contexte 4096 × 1 slot(s) au lieu de 32768 × 1 : ancienne version rétablie")
    );
    assert_eq!(st["apply"]["message"], json!(""));
    assert_eq!(
        st["apply"]["log"].as_array().unwrap().last().unwrap(),
        "échec (contexte 4096 × 1 slot(s) au lieu de 32768 × 1) : retour à l'ancienne version"
    );
    let preset = std::fs::read_to_string(tn.home.join("presets/dense.env")).unwrap();
    assert!(
        preset.contains("UBATCH=2048"),
        "preset non rétabli : {preset}"
    );
    assert_eq!(tn.config()["UBATCH"], "2048", "configuration non rétablie");
    assert_eq!(
        *tn.go.service_os.lock().unwrap(),
        ["stop", "start", "restart", "restart"]
    );
    assert!(!tn.home.join("tune.lock").exists());
    // Une sauvegarde a été faite avant d'écrire.
    let backups: Vec<_> = std::fs::read_dir(tn.home.join("tune/backup"))
        .unwrap()
        .flatten()
        .collect();
    assert_eq!(backups.len(), 1);
}

/// Essais refusés à la composition, essais qui ne chargent pas, budget.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn essais_refuses() {
    let tn = start().await;
    tn.preset(&format!("{PRESET}REFUS=1024\nECHEC=512\n"));
    tn.call("POST", "/api/tune", r#"{"stages":["lots"]}"#).await;
    let st = tn.wait("optimisation", "running").await;
    let tr = trials(&st["result"]);
    assert_eq!(
        tr,
        [
            (
                "UBATCH=512 BATCH=2048".to_string(),
                "échec".to_string(),
                "mémoire insuffisante (OOM)".to_string()
            ),
            (
                "UBATCH=512 BATCH=1024".into(),
                "échec".into(),
                "mémoire insuffisante (OOM)".into()
            ),
            (
                "UBATCH=1024 BATCH=2048".into(),
                "écarté".into(),
                "refusé au lancement : [err] UBATCH=1024 refusé".into()
            ),
            (
                "UBATCH=2048 BATCH=4096".into(),
                "mesuré".into(),
                tr[3].2.clone()
            ),
        ],
        "{tr:?}"
    );
    assert!(tr[3].2.ends_with(" (seuil 3.0 %)"), "{tr:?}");
    assert_eq!(st["result"]["best"], Value::Null);
    assert_eq!(st["result"]["set"], Value::Null);
    // Rien à appliquer.
    let (code, out) = tn
        .call("POST", "/api/tune/apply", r#"{"target":"preset"}"#)
        .await;
    assert_eq!(
        (code, out),
        (
            200,
            json!({ "ok": false, "error": "rien à appliquer : la configuration actuelle reste la meilleure" })
        )
    );

    // La référence ne se compose pas : l'optimisation échoue, le moteur est
    // relancé, le verrou rendu.
    tn.preset(&format!("{PRESET}REFUS=2048\n"));
    tn.go.service_os.lock().unwrap().clear();
    tn.call("POST", "/api/tune", "{}").await;
    let st = tn.wait("référence", "running").await;
    assert_eq!(
        st["error"],
        json!("la configuration active ne se lance pas : [err] UBATCH=2048 refusé")
    );
    assert_eq!(st["result"], Value::Null);
    assert_eq!(*tn.go.service_os.lock().unwrap(), ["stop", "start"]);
    // La relance échoue elle aussi : dit dans l'erreur.
    *tn.go.service_os_fail.lock().unwrap() = Some(("start".into(), "boum".into()));
    tn.call("POST", "/api/tune", "{}").await;
    let st = tn.wait("relance", "running").await;
    assert_eq!(
        st["error"],
        json!(
            "la configuration active ne se lance pas : [err] UBATCH=2048 refusé — relance du moteur principal : boum"
        )
    );
    *tn.go.service_os_fail.lock().unwrap() = None;
    assert!(!tn.home.join("tune.lock").exists());
}

/// Annulation pendant les essais : l'essai arrêté, le résultat partiel
/// gardé.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn annulation() {
    let tn = start().await;
    tn.call("POST", "/api/tune", r#"{"stages":["lots"]}"#).await;
    // Attendre le premier essai, puis annuler.
    for _ in 0..4000 {
        let st = tn.status().await;
        if st["phase"]
            .as_str()
            .is_some_and(|p| p.starts_with("lots — "))
        {
            break;
        }
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    let (code, out) = tn.call("POST", "/api/tune/cancel", "").await;
    assert_eq!((code, out), (200, json!({ "ok": true })));
    let st = tn.wait("annulation", "running").await;
    assert_eq!(st["result"]["partial"], json!("annulé"), "{st}");
    assert!(!tn.home.join("tune.lock").exists());
    let (_, out) = tn.call("POST", "/api/tune/cancel", "").await;
    assert_eq!(out, json!({ "ok": false }));
    // Le stop du chat (signalé au renouvellement du bail) annule aussi.
    *tn.go.lease_stop.lock().unwrap() = true;
    tn.call("POST", "/api/tune", r#"{"stages":["lots"]}"#).await;
    let st = tn.wait("stop", "running").await;
    *tn.go.lease_stop.lock().unwrap() = false;
    let annule =
        st["result"]["partial"] == json!("annulé") || st["error"] == json!("optimisation annulée");
    assert!(annule, "{st}");
}

/// Les refus d'entrée et les routes au repos, avec les messages du Go.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn refus_d_entree() {
    let tn = start().await;
    assert_eq!(
        tn.status().await,
        json!({ "ok": true, "running": false, "applying": false })
    );
    assert_eq!(
        tn.call("GET", "/api/tune", "").await,
        (
            405,
            json!({ "ok": false, "error": "méthode GET refusée : POST attendu" })
        )
    );
    assert_eq!(tn.call("GET", "/api/tune/cancel", "").await.0, 405);
    assert_eq!(
        tn.call("POST", "/api/tune/cancel", "").await,
        (200, json!({ "ok": false }))
    );
    assert_eq!(
        tn.call("POST", "/api/tune/apply", r#"{"target":"copy"}"#)
            .await,
        (
            400,
            json!({ "ok": false, "error": "aucun résultat à appliquer" })
        )
    );
    assert_eq!(
        tn.call("GET", "/api/tune/last?id=dense", "").await,
        (200, json!({ "ok": false }))
    );
    let refus = |code: u16, e: &str| (code, json!({ "ok": false, "error": e }));
    tn.go.set("config", "EXTERNAL", "1");
    assert_eq!(
        tn.call("POST", "/api/tune", "{}").await,
        refus(
            400,
            "optimisation indisponible : le preset actif est une API externe"
        )
    );
    tn.go.del("config", "EXTERNAL");
    tn.go.set("config", "ENGINE", "strata");
    assert_eq!(
        tn.call("POST", "/api/tune", "{}").await,
        refus(
            409,
            "optimisation indisponible : le preset actif tourne sur Strata, réglé par sa propre fenêtre"
        )
    );
    tn.go.del("config", "ENGINE");
    tn.go.set("config", "CTX", "4096");
    assert_eq!(
        tn.call("POST", "/api/tune", "{}").await,
        refus(
            409,
            "aucun preset ne correspond à la configuration active : enregistre-la comme preset (ou bascule sur l'un d'eux) avant d'optimiser"
        )
    );
    tn.go.set("config", "CTX", "32768");
    *tn.go.tune_ctx_busy.lock().unwrap() =
        "2 job(s) d'arrière-plan du mode code tournent encore".into();
    assert_eq!(
        tn.call("POST", "/api/tune", "{}").await,
        refus(409, "2 job(s) d'arrière-plan du mode code tournent encore")
    );
    tn.go.tune_ctx_busy.lock().unwrap().clear();
    *tn.main.busy.lock().unwrap() = true;
    assert_eq!(
        tn.call("POST", "/api/tune", "{}").await,
        refus(
            409,
            "le moteur traite une requête (discussion, tâche ou client /v1) : réessaie une fois libre"
        )
    );
    *tn.main.busy.lock().unwrap() = false;
    *tn.go.go_busy.lock().unwrap() = Some("génération en cours".into());
    assert_eq!(
        tn.call("POST", "/api/tune", "{}").await,
        refus(409, "génération en cours")
    );
    *tn.go.go_busy.lock().unwrap() = None;
    // Une optimisation tenue par un autre processus vivant (« loki tune ») :
    // refus, et l'interface le voit.
    let other = json!({ "owner": { "pid": 1 }, "via": "cli", "phase": "essais", "since": 1700000000, "main_was_active": false });
    std::fs::write(tn.home.join("tune.lock"), other.to_string()).unwrap();
    assert_eq!(tn.call("POST", "/api/tune", "{}").await, refus(409, BUSY));
    assert_eq!(tn.status().await["elsewhere"], json!("cli"));
    std::fs::remove_file(tn.home.join("tune.lock")).unwrap();
    assert_eq!(tn.status().await.get("elsewhere"), None);
    assert!(tn.go.lease_whys.lock().unwrap().iter().all(|w| w == "tune"));
}

/// Un verrou périmé (Loki tué en plein essai) : l'essai orphelin est arrêté
/// — identité complète vérifiée — avant que l'optimisation ne reprenne la
/// place ; une application interrompue est défaite.
#[cfg(target_os = "linux")]
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn orphelin_et_application_interrompue() {
    use std::os::unix::process::CommandExt;
    let tn = start().await;
    let mut child = std::process::Command::new("sleep")
        .arg("60")
        .process_group(0)
        .spawn()
        .unwrap();
    let pid = i64::from(child.id());
    // L'identité une fois l'exec visible dans /proc : lue trop tôt, elle
    // porterait l'exécutable du test, et l'essai ne serait plus reconnu.
    let mut ident = None;
    for _ in 0..200 {
        if let Some((start, exe)) = loki_rs::tune::lock::proc_identity(pid)
            && exe.ends_with("sleep")
        {
            ident = Some((start, exe));
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    let (start, exe) = ident.expect("exec de sleep jamais visible");
    let dead = json!({ "pid": 999999, "start": "x" });
    let write = |trial: Value, extra: Value| {
        let mut o = json!({ "owner": dead, "via": "cli", "phase": "essais", "since": 1, "trial": trial, "main_was_active": false });
        for (k, v) in extra.as_object().unwrap() {
            o[k] = v.clone();
        }
        std::fs::write(tn.home.join("tune.lock"), o.to_string()).unwrap();
    };
    // Identité qui ne concorde pas (PID recyclé) : on ne tue rien.
    write(
        json!({ "pid": pid, "start": format!("{start}x"), "exes": [exe] }),
        json!({}),
    );
    tn.call("POST", "/api/tune", r#"{"stages":["aucune"]}"#)
        .await;
    tn.wait("essais", "running").await;
    assert!(
        child.try_wait().unwrap().is_none(),
        "processus tué sur la foi du seul PID"
    );
    // Identité complète : arrêté.
    write(
        json!({ "pid": pid, "start": start, "exes": [exe] }),
        json!({}),
    );
    tn.call("POST", "/api/tune", r#"{"stages":["aucune"]}"#)
        .await;
    tn.wait("orphelin", "running").await;
    let mut gone = false;
    for _ in 0..300 {
        if child.try_wait().unwrap().is_some() {
            gone = true;
            break;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    assert!(gone, "essai orphelin toujours en vie");

    // Application interrompue : la sauvegarde revient, preset et
    // configuration.
    let old = std::fs::read_to_string(tn.home.join("presets/dense.env")).unwrap();
    let backup = tn.home.join("vieux.env");
    std::fs::write(&backup, &old).unwrap();
    tn.preset(&PRESET.replace("UBATCH=2048", "UBATCH=999"));
    write(
        Value::Null,
        json!({ "phase": "application", "backup": backup, "preset_id": "dense", "preset_name": "Dense" }),
    );
    tn.call("POST", "/api/tune", r#"{"stages":["aucune"]}"#)
        .await;
    tn.wait("application interrompue", "running").await;
    assert_eq!(
        std::fs::read_to_string(tn.home.join("presets/dense.env")).unwrap(),
        old
    );
    assert_eq!(tn.config()["UBATCH"], "2048");
    // Hors phase d'application, rien n'est défait.
    tn.preset(&PRESET.replace("UBATCH=2048", "UBATCH=999"));
    write(
        Value::Null,
        json!({ "phase": "essais", "backup": backup, "preset_id": "dense", "preset_name": "Dense" }),
    );
    tn.call("POST", "/api/tune", r#"{"stages":["aucune"]}"#)
        .await;
    tn.wait("essais interrompus", "running").await;
    assert_eq!(tn.config()["UBATCH"], "999");
}

/// Sans la façade pour l'optimiseur, ses routes partent au Go.
#[tokio::test]
async fn sans_la_facade() {
    let tmp = tempfile::tempdir().unwrap();
    let (facade, _go, _stop) = fakego::start(tmp.path().to_path_buf()).await;
    for p in [
        "/api/tune",
        "/api/tune/status",
        "/api/tune/cancel",
        "/api/tune/apply",
        "/api/tune/last",
    ] {
        let (c, b) = fakego::send(facade, "GET", p, &[], "").await;
        assert_eq!(c.as_u16(), 200);
        assert_eq!(String::from_utf8_lossy(&b), format!("go:{p}"));
    }
}

/// Pendant la prise du bail, un second lancement est refusé.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn second_lancement_pendant_le_bail() {
    let tn = Arc::new(start().await);
    *tn.go.lease_delay.lock().unwrap() = Some(Duration::from_millis(400));
    let first = {
        let tn = tn.clone();
        tokio::spawn(async move {
            tn.call("POST", "/api/tune", r#"{"stages":["aucune"]}"#)
                .await
        })
    };
    tokio::time::sleep(Duration::from_millis(150)).await;
    // Avant la prise du bail : aucune estimation encore.
    assert_eq!(tn.status().await["running"], json!(false));
    assert_eq!(
        tn.call("POST", "/api/tune", "{}").await,
        (409, json!({ "ok": false, "error": BUSY }))
    );
    assert_eq!(first.await.unwrap().0, 202);
    let st = tn.status().await;
    if st["running"] == json!(true) {
        assert_eq!(
            st["eta_sec"],
            json!(-1),
            "aucun essai mesuré : ETA inconnue"
        );
    }
    tn.wait("optimisation", "running").await;
    assert_eq!(*tn.go.lease_whys.lock().unwrap(), vec!["tune".to_string()]);
}

/// Une application qui échoue avant d'écrire quoi que ce soit (une autre
/// optimisation tient le moteur) : l'erreur, sans journal.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn application_refusee_sans_journal() {
    let tn = start().await;
    tn.call("POST", "/api/tune", r#"{"stages":["lots"]}"#).await;
    let st = tn.wait("optimisation", "running").await;
    assert_eq!(st["result"]["set"]["UBATCH"], json!("512"), "{st}");
    let other = json!({ "owner": { "pid": 1 }, "via": "cli", "phase": "essais", "since": 1700000000, "main_was_active": false });
    std::fs::write(tn.home.join("tune.lock"), other.to_string()).unwrap();
    let (code, _) = tn
        .call("POST", "/api/tune/apply", r#"{"target":"preset"}"#)
        .await;
    assert_eq!(code, 202);
    let mut st = tn.status().await;
    for _ in 0..500 {
        if st["applying"] == json!(false) {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
        st = tn.status().await;
    }
    let apply = &st["apply"];
    assert_eq!(apply["log"], Value::Null, "{st}");
    assert_eq!(apply["message"], json!(""));
    assert!(
        apply["error"]
            .as_str()
            .unwrap()
            .starts_with("une optimisation tourne déjà (cli, PID 1, depuis "),
        "{st}"
    );
    assert_eq!(st["elsewhere"], json!("cli"));
    assert!(
        std::fs::read_to_string(tn.home.join("presets/dense.env"))
            .unwrap()
            .contains("UBATCH=2048"),
        "rien n'est écrit"
    );
    std::fs::remove_file(tn.home.join("tune.lock")).unwrap();
}

/// La façade pilote elle-même le moteur (LOKI_ENGINE=rust) : arrêt et
/// relance par son superviseur, pas par le Go.
#[cfg(unix)]
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn moteur_pilote_par_la_facade() {
    use std::os::unix::fs::PermissionsExt;
    let tn = start_with(
        |_, _| {},
        |o, home| {
            let script = home.join("superviseur.sh");
            std::fs::write(
                &script,
                "#!/bin/sh\necho \"$1 $2\" >> \"$SUPERVISEUR_LOG\"\n",
            )
            .unwrap();
            std::fs::set_permissions(&script, std::fs::Permissions::from_mode(0o755)).unwrap();
            let ctl = loki_rs::enginectl::EngineCtl::new(
                script,
                vec![(
                    "SUPERVISEUR_LOG".into(),
                    home.join("superviseur.log").to_string_lossy().into_owned(),
                )],
            );
            o.engine_ctl = Some(Arc::new(ctl));
            // Le moteur tourne : son PID, ce processus.
            let pid = loki_rs::engine::Paths {
                home: home.to_path_buf(),
                service: "loki-engine".into(),
            }
            .pid();
            std::fs::create_dir_all(pid.parent().unwrap()).unwrap();
            std::fs::write(pid, format!("{}\n", std::process::id())).unwrap();
        },
    )
    .await;
    *tn.go.service_active.lock().unwrap() = false;
    tn.call("POST", "/api/tune", r#"{"stages":["aucune"]}"#)
        .await;
    let st = tn.wait("optimisation", "running").await;
    assert_eq!(st["error"], Value::Null, "{st}");
    assert_eq!(
        std::fs::read_to_string(tn.home.join("superviseur.log")).unwrap(),
        "engine stop\nengine start\n"
    );
    assert!(
        tn.go.service_os.lock().unwrap().is_empty(),
        "rien par le Go"
    );
}

/// Les essais écartés après composition : ligne dénaturée, ligne déjà
/// mesurée (doublon), contexte perdu au chargement.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn essais_ecartes() {
    let tn = start().await;
    tn.preset(&format!(
        "{PRESET}DENATURE=1024\nCTX_FAUX=512\nSANS_BATCH=1\n"
    ));
    tn.call("POST", "/api/tune", r#"{"stages":["lots"]}"#).await;
    let st = tn.wait("optimisation", "running").await;
    let tr = trials(&st["result"]);
    assert_eq!(tr.len(), 4, "{tr:?}");
    assert_eq!(
        tr[0],
        (
            "UBATCH=512 BATCH=2048".into(),
            "écarté".into(),
            "dénature : contexte 16384 × 1 slot(s) au lieu de 32768 × 1".into()
        )
    );
    assert_eq!(
        tr[1],
        (
            "UBATCH=512 BATCH=1024".into(),
            "doublon".into(),
            "même ligne de commande que « UBATCH=512 BATCH=2048 »".into()
        )
    );
    assert_eq!(tr[2].1, "écarté");
    assert!(
        tr[2].2.starts_with("dénature : ") && tr[2].2.contains("--no-mmap"),
        "{tr:?}"
    );
    assert_eq!(
        tr[3],
        (
            "UBATCH=2048 BATCH=4096".into(),
            "doublon".into(),
            "même ligne de commande que « configuration actuelle »".into()
        )
    );
    assert_eq!(st["result"]["best"], Value::Null);
}

/// La VRAM mesurée (faux nvidia-smi) : marge libre après chaque mesure, et
/// les essais trop justes écartés.
#[cfg(unix)]
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn vram_mesuree() {
    use std::os::unix::fs::PermissionsExt;
    let tn = start_with(
        |t, home| {
            let smi = home.join("nvidia-smi");
            std::fs::write(
                &smi,
                format!("#!/bin/sh\ncat '{}'\n", home.join("smi.txt").display()),
            )
            .unwrap();
            std::fs::set_permissions(&smi, std::fs::Permissions::from_mode(0o755)).unwrap();
            std::fs::write(home.join("smi.txt"), "Fausse, 1000, 24000, 0, 40\n").unwrap();
            t.nvidia_smi = smi.to_string_lossy().into_owned();
        },
        |_, _| {},
    )
    .await;
    tn.call("POST", "/api/tune", r#"{"stages":["lots"]}"#).await;
    let st = tn.wait("optimisation", "running").await;
    let res = &st["result"];
    assert_eq!(res["baseline"]["headroom_mib"], json!(23000), "{res}");
    assert_eq!(res["trials"][0]["headroom_mib"], json!(23000));
    assert_eq!(res["trials"][0]["status"], json!("retenu"));
    assert_eq!(res["notes"], Value::Null, "VRAM mesurable : aucune note");
    // Une carte presque pleine : tout essai est trop juste.
    std::fs::write(tn.home.join("smi.txt"), "Fausse, 23500, 24000, 0, 40\n").unwrap();
    tn.preset(&format!("{PRESET}MMPROJ=/models/vision.gguf\n"));
    tn.call("POST", "/api/tune", r#"{"stages":["lots"]}"#).await;
    let st = tn.wait("trop juste", "running").await;
    let res = &st["result"];
    assert_eq!(
        res["notes"],
        json!(["la configuration actuelle laisse déjà moins de 1280 Mio libres sur une carte"])
    );
    let tr = trials(res);
    assert!(
        tr.iter()
            .all(|t| t.1 == "écarté" && t.2 == "trop juste : 500 Mio libres sur une carte (< 1280)"),
        "{tr:?}"
    );
    assert_eq!(res["best"], Value::Null);
}

/// Le budget atteint : les essais restants ne sont pas faits.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn budget_atteint() {
    let tn = start_with(|t, _| t.budget_unit = Duration::from_millis(100), |_, _| {}).await;
    tn.call("POST", "/api/tune", r#"{"stages":["lots"],"budget_min":1}"#)
        .await;
    let st = tn.wait("budget", "running").await;
    assert_eq!(
        st["result"]["partial"],
        json!("budget de 0s atteint : 4 essai(s) non faits")
    );
    assert_eq!(st["result"]["trials"], Value::Null);
}

/// La profondeur : celle que la référence tient avec le plus gros
/// préchauffage essayé.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn profondeur_commune() {
    let tn = start().await;
    // Un moteur qui ne donne le contexte qu'au premier niveau de /props.
    tn.preset(&format!(
        "{}PROPS_PLAT=1\n",
        PRESET.replace("CTX=32768", "CTX=20000")
    ));
    tn.call("POST", "/api/tune", r#"{"stages":["aucune"]}"#)
        .await;
    let st = tn.wait("optimisation", "running").await;
    assert_eq!(st["result"]["depth"], json!(7288), "{st}");
    assert_eq!(st["result"]["n_ctx"], json!(20000));
}

/// Les notes : modèle plus gros que la RAM ; relance du vrai moteur qui
/// échoue après une optimisation réussie ; poids tirés de la télémétrie ;
/// essais d'une optimisation précédente effacés.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn notes_et_poids() {
    let tn = start().await;
    let (_, total_mb) = loki_rs::sysinfo::ram_usage_mb();
    assert!(total_mb > 0);
    // La moitié de la RAM : pas de note (des octets, comparés à des Mio).
    tn.preset(&format!("{PRESET}MODEL_BYTES={}\n", total_mb << 19));
    let stale = tn.home.join("tune/run/e09");
    std::fs::create_dir_all(&stale).unwrap();
    let recs: Vec<Value> = (0..20)
        .map(|_| json!({ "kind": "main", "complete": true, "total": 4000, "cached": 0, "new": 1000, "gen": 100 }))
        .collect();
    *tn.go.tune_perf.lock().unwrap() = Value::Array(recs);
    *tn.go.service_os_fail.lock().unwrap() = Some(("start".into(), "boum".into()));
    tn.call("POST", "/api/tune", r#"{"stages":["aucune"]}"#)
        .await;
    let st = tn.wait("optimisation", "running").await;
    let res = &st["result"];
    assert_eq!(
        res["notes"],
        json!([VRAM_NOTE, "⚠️ relance du moteur principal : boum"]),
        "{res}"
    );
    assert_eq!(
        res["weights"],
        json!({ "k": 1000, "g": 100, "f_cold": 1, "from": "télémétrie" })
    );
    assert!(!stale.exists(), "essais précédents effacés");
    // Enregistré AVANT la relance : sans sa note.
    let saved: Value = serde_json::from_str(&tn.go.get("state", "tune_results").unwrap()).unwrap();
    assert_eq!(saved["dense"]["notes"], json!([VRAM_NOTE]));
    *tn.go.service_os_fail.lock().unwrap() = None;
    // Deux fois la RAM : la note.
    tn.preset(&format!("{PRESET}MODEL_BYTES={}\n", total_mb << 21));
    tn.call("POST", "/api/tune", r#"{"stages":["aucune"]}"#)
        .await;
    let st = tn.wait("optimisation", "running").await;
    assert_eq!(
        st["result"]["notes"],
        json!([
            VRAM_NOTE,
            "modèle plus gros que la RAM : chaque essai le relit depuis le disque — prévoir un budget plus long, ou ne garder que quelques étapes"
        ])
    );
}

/// Ce que la mesure dit d'un essai : un gain sous le seuil, un écart nul,
/// un decode en prose plus lent.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn ecarts_de_mesure() {
    let tn = start().await;
    // 512 refusé : la référence reste la meilleure ; 1024 à peine plus
    // rapide ; 2048/4096 identique.
    tn.preset(&format!("{PRESET}REFUS=512\nVITESSE=1024:1990\n"));
    tn.call("POST", "/api/tune", r#"{"stages":["lots"]}"#).await;
    let st = tn.wait("optimisation", "running").await;
    let tr = trials(&st["result"]);
    assert_eq!(tr.len(), 4, "{tr:?}");
    assert_eq!(tr[2].0, "UBATCH=1024 BATCH=2048");
    assert_eq!(tr[2].1, "mesuré");
    assert!(
        tr[2].2.starts_with('−') && tr[2].2.ends_with(" %, sous le seuil de 3 %"),
        "{tr:?}"
    );
    assert_eq!(
        st["result"]["trials"][2]["runs"].as_array().unwrap().len(),
        1
    );
    assert_eq!(
        tr[3],
        (
            "UBATCH=2048 BATCH=4096".into(),
            "mesuré".into(),
            "≈ 0 % (seuil 3.0 %)".into()
        )
    );
    // Un prefill bien plus rapide, mais un decode en prose plus lent de 20 %.
    tn.preset(&format!("{PRESET}REFUS=1024\nDECODE=512:40\n"));
    tn.call("POST", "/api/tune", r#"{"stages":["lots"]}"#).await;
    let st = tn.wait("decode", "running").await;
    let tr = trials(&st["result"]);
    assert_eq!(
        tr[0],
        (
            "UBATCH=512 BATCH=2048".into(),
            "mesuré".into(),
            "decode en prose plus lent de 20 %".into()
        ),
        "{tr:?}"
    );
    assert_eq!(st["result"]["best"], Value::Null);
}

/// L'application dans le preset, gardée : il doit être celui en service, et
/// pas une API externe ; une configuration changée ailleurs pendant
/// l'application la défait ; la sauvegarde est notée dans le verrou avant
/// toute écriture.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn application_gardee() {
    let tn = start().await;
    tn.call("POST", "/api/tune", r#"{"stages":["lots"]}"#).await;
    let st = tn.wait("optimisation", "running").await;
    assert_eq!(st["result"]["set"]["UBATCH"], json!("512"), "{st}");
    let old = std::fs::read_to_string(tn.home.join("presets/dense.env")).unwrap();

    // La configuration active n'est plus ce preset.
    tn.go.set("config", "UBATCH", "1024");
    let apply = tn.apply_preset().await;
    assert_eq!(
        apply["error"],
        json!(
            "ce preset n'est plus celui en service : bascule dessus, ou enregistre le résultat dans une copie"
        ),
        "{apply}"
    );
    assert_eq!(apply["log"], Value::Null);
    tn.go.set("config", "UBATCH", "2048");

    // Une API externe.
    tn.go.set("config", "EXTERNAL", "1");
    let apply = tn.apply_preset().await;
    assert_eq!(
        apply["error"],
        json!("le preset actif est une API externe"),
        "{apply}"
    );
    tn.go.del("config", "EXTERNAL");

    // Une clé changée ailleurs pendant l'application : l'ancienne version
    // revient, sans relance sur la nouvelle.
    tn.go
        .apply_extra
        .lock()
        .unwrap()
        .push(("THREADS".into(), "7".into()));
    let apply = tn.apply_preset().await;
    assert_eq!(
        apply["error"],
        json!(
            "la configuration a changé ailleurs que sur les clés réglées depuis la mesure : ancienne version rétablie"
        ),
        "{apply}"
    );
    assert_eq!(
        std::fs::read_to_string(tn.home.join("presets/dense.env")).unwrap(),
        old
    );
    assert_eq!(
        *tn.go.service_os.lock().unwrap(),
        ["stop", "start", "restart"]
    );
    tn.go.apply_extra.lock().unwrap().clear();
    tn.preset(PRESET);

    // Le moteur relancé tarde : pendant l'attente, le verrou désigne la
    // sauvegarde (un Loki tué la remet au redémarrage).
    *tn.main.down.lock().unwrap() = true;
    let (code, _) = tn
        .call("POST", "/api/tune/apply", r#"{"target":"preset"}"#)
        .await;
    assert_eq!(code, 202);
    let mut noted = None;
    for _ in 0..1000 {
        if let Some(o) = loki_rs::tune::lock::read_owner(&tn.home.join("tune.lock"))
            && !o.backup.is_empty()
        {
            noted = Some(o);
            break;
        }
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    let o = noted.expect("sauvegarde jamais notée dans le verrou");
    assert_eq!(
        (
            o.phase.as_str(),
            o.preset_id.as_str(),
            o.preset_name.as_str()
        ),
        ("application", "dense", "Dense")
    );
    assert_eq!(std::fs::read_to_string(&o.backup).unwrap(), old);
    *tn.main.down.lock().unwrap() = false;
    let apply = tn.wait("application", "applying").await["apply"].clone();
    assert_eq!(
        (apply["error"].clone(), apply["message"].clone()),
        (json!(""), json!("preset « Dense » optimisé et vérifié")),
        "{apply}"
    );
    assert!(!tn.home.join("tune.lock").exists(), "verrou non rendu");
}

/// Un essai qui ne répond jamais : abandonné au bout du délai de chargement.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn essai_muet() {
    let tn = start_with(|t, _| t.load_timeout = Duration::from_secs(2), |_, _| {}).await;
    tn.preset(&format!("{PRESET}MUET=512\n"));
    tn.call("POST", "/api/tune", r#"{"stages":["lots"]}"#).await;
    let st = tn.wait("optimisation", "running").await;
    let tr = trials(&st["result"]);
    assert_eq!(
        tr[0],
        (
            "UBATCH=512 BATCH=2048".into(),
            "échec".into(),
            "pas prêt après 2s".into()
        ),
        "{tr:?}"
    );
}

/// SIDE_SLOT sur un moteur à un seul slot : les mesures restent sur le
/// slot 0.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn second_slot_absent() {
    let tn = start().await;
    tn.preset(&format!("{PRESET}SIDE_SLOT=on\n"));
    tn.call("POST", "/api/tune", r#"{"stages":["aucune"]}"#)
        .await;
    let st = tn.wait("optimisation", "running").await;
    assert_eq!(st["error"], Value::Null, "{st}");
    assert_eq!(st["result"]["baseline"]["status"], json!("mesuré"), "{st}");
}

/// Les threads essayés : comptés sur les cœurs physiques que la sonde du
/// moteur annonce (des poids sur CPU).
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn threads_de_la_sonde() {
    let tn = start().await;
    tn.preset(&format!("{PRESET}EXTRA_ARGS=--cpu-moe\nCPU_THREADS=13\n"));
    tn.call("POST", "/api/tune", r#"{"stages":["threads"]}"#)
        .await;
    let st = tn.wait("optimisation", "running").await;
    let labels: Vec<String> = trials(&st["result"]).into_iter().map(|t| t.0).collect();
    assert_eq!(labels, ["THREADS=12", "THREADS=6"], "{st}");
}
