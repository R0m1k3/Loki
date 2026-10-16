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
const BUSY: &str = "optimisation en cours (loki tune) : le moteur est réservé aux essais — attends la fin ou annule-la";

// ---------------------------------------------------------------------------
// Le faux moteur
// ---------------------------------------------------------------------------

/// Ce que le faux moteur annonce : son contexte, et s'il travaille déjà.
#[derive(Default)]
struct Knobs {
    n_ctx: Mutex<i64>,
    busy: Mutex<bool>,
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
    let out = match path.as_str() {
        "/health" => json!({ "status": "ok" }),
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
            let pps = 4_000_000.0 / ubatch as f64;
            let n_gen = req["max_tokens"].as_i64().unwrap_or(16);
            json!({
                "choices": [{ "message": { "content": "ok" } }],
                "usage": { "prompt_tokens": total },
                "timings": {
                    "prompt_n": new, "prompt_ms": (new as f64 / pps * 1000.0).max(1.0),
                    "predicted_n": n_gen, "predicted_ms": n_gen as f64 / 50.0 * 1000.0,
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
            if !get(k).is_empty() {
                args.extend([f.to_string(), get(k)]);
            }
        }
        args.extend(["--host".into(), get("HOST"), "--port".into(), get("PORT")]);
        let launch = json!({
            "bin": "/opt/faux/llama-server", "args": args,
            "probe": { "help": "", "gpus": 0, "vram_mib": 0, "model_bytes": 1000, "moe": false, "cpu_threads": 8 },
        });
        std::fs::write(dir.join("launch.json"), launch.to_string()).unwrap();
        return;
    }
    if get("ECHEC") == ub {
        die("llama_model_load: error loading model: out of memory");
    }
    let port: u16 = get("PORT").parse().unwrap();
    let l = TcpListener::bind(("127.0.0.1", port)).unwrap();
    let _ = std::io::stdout().write_all(b"load_tensors: offloaded 33/33 layers to GPU\n");
    let knobs = Knobs::default();
    *knobs.n_ctx.lock().unwrap() = get("CTX").parse().unwrap();
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
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().to_path_buf();
    let mut t = Tune::new(home.clone(), std::env::current_exe().unwrap());
    t.trial_env = vec![(FAUX.into(), "1".into())];
    t.poll = Duration::from_millis(20);
    t.probe_poll = Duration::from_millis(20);
    t.settle = Duration::ZERO;
    t.load_timeout = Duration::from_secs(20);
    t.lease_ttl = Duration::from_millis(300);
    let tune = Arc::new(t);
    // Moteur et modèle présents : la relance du vrai moteur passe ses
    // vérifications.
    std::fs::write(home.join("llama-server"), "x").unwrap();
    std::fs::write(home.join("m.gguf"), "x").unwrap();
    let (facade, go, stop, _) = fakego::start_with(
        home.clone(),
        fakego::Opts {
            tune: Some(tune),
            ..Default::default()
        },
    )
    .await;
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
            if st[key] != json!(true) {
                return st;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        panic!("{what} : ne finit pas");
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
    let (code, out) = tn.call("POST", "/api/tune", "{}").await;
    assert_eq!(code, 409);
    assert_eq!(out["error"], json!(BUSY));
    let st = tn.wait("optimisation", "running").await;
    assert_eq!(st["error"], Value::Null, "{st}");
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
    assert!(
        tr[0].2.starts_with('−') && tr[0].2.contains("par tour (seuil"),
        "{tr:?}"
    );
    assert_eq!(
        res["best"],
        json!(tr.iter().rfind(|t| t.1 == "retenu").unwrap().0)
    );
    assert_eq!(res["set"]["UBATCH"], json!("512"));
    assert!(res["gain"].as_f64().unwrap() > 0.03, "{res}");
    let skipped: Vec<&str> = res["skipped"]
        .as_array()
        .unwrap()
        .iter()
        .map(|s| s.as_str().unwrap())
        .collect();
    assert!(skipped.contains(&"marges : non demandé"), "{skipped:?}");
    assert!(
        res["notes"]
            .as_array()
            .unwrap()
            .contains(&json!("VRAM libre non mesurable (nvidia-smi absent) : ni micro-lot plus grand ni marges --fit essayés"))
    );
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
    let (start, exe) = loki_rs::tune::lock::proc_identity(pid).unwrap();
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
