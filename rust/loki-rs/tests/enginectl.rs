//! Lot 16 : pilotage du moteur tenu par la façade (`LOKI_ENGINE=rust`).
//!
//! Le journal tiré du Go (`internal/loki/parity_engine_test.go` →
//! `tests/parity_engine/engine.json`) est rejoué : bilan de VRAM
//! (`gpuSettle`), contrôle d'avant démarrage (`preflightEngine`) sur chaque
//! installation, dernière ligne d'erreur du superviseur, puis les réponses des
//! routes qui ne lancent aucun processus. Le reste — démarrage, arrêt,
//! redémarrage, déchargement avec bilan chiffré, rechargement — est vérifié de
//! bout en bout avec un faux superviseur (`loki-rs engine <action>`) et un
//! faux nvidia-smi.

mod common;

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use bytes::Bytes;
use common::fakego;
use http_body_util::{BodyExt, Full};
use hyper::header::HeaderMap;
use hyper::{Request, StatusCode};
use loki_rs::enginectl::{self as ec, EngineCtl};
use serde_json::{Value, json};

fn fixture() -> Value {
    serde_json::from_str(include_str!("parity_engine/engine.json")).expect("journal")
}

fn s(v: &Value) -> &str {
    v.as_str().unwrap()
}

/// Écrit un script exécutable sans le garder ouvert (ETXTBSY).
fn script(dir: &Path, name: &str, body: &str) -> PathBuf {
    let p = dir.join(name);
    let ok = std::process::Command::new("sh")
        .args(["-c", "printf %s \"$1\" > \"$2\" && chmod 755 \"$2\""])
        .arg("sh")
        .arg(body)
        .arg(&p)
        .status()
        .unwrap()
        .success();
    assert!(ok);
    p
}

async fn call(
    facade: std::net::SocketAddr,
    method: &str,
    path: &str,
    body: &str,
) -> (StatusCode, HeaderMap, Value) {
    use hyper_util::client::legacy::Client;
    use hyper_util::rt::TokioExecutor;
    let req = Request::builder()
        .method(method)
        .uri(format!("http://{facade}{path}"))
        .body(Full::new(Bytes::from(body.to_string())))
        .unwrap();
    let client: Client<_, Full<Bytes>> = Client::builder(TokioExecutor::new()).build_http();
    let r = client.request(req).await.unwrap();
    let (parts, b) = r.into_parts();
    let b = b.collect().await.unwrap().to_bytes();
    let v = serde_json::from_slice(&b)
        .unwrap_or_else(|_| Value::String(String::from_utf8_lossy(&b).into_owned()));
    (parts.status, parts.headers, v)
}

#[tokio::test]
async fn parite_du_bilan_de_vram() {
    for c in fixture()["settle"].as_array().unwrap() {
        let before = c["before"].as_i64().unwrap();
        let seq: Vec<i64> = c["seq"]
            .as_array()
            .unwrap()
            .iter()
            .map(|x| x.as_i64().unwrap())
            .collect();
        let i = std::sync::Mutex::new(0usize);
        let got = ec::gpu_settle(
            before,
            || {
                let mut n = i.lock().unwrap();
                let v = if *n >= seq.len() {
                    (seq[seq.len() - 1], true)
                } else {
                    let v = seq[*n];
                    *n += 1;
                    if v < 0 { (0, false) } else { (v, true) }
                };
                async move { v }
            },
            Duration::ZERO,
        )
        .await;
        assert_eq!(got, c["out"].as_i64().unwrap(), "{c}");
    }
}

#[test]
fn parite_des_messages() {
    let fx = fixture();
    for (k, v) in fx["last_error"].as_object().unwrap() {
        assert_eq!(ec::last_error_line(k), s(v), "{k:?}");
    }
    for (k, v) in fx["plain"].as_object().unwrap() {
        assert_eq!(ec::plain_err(k), s(v), "{k:?}");
    }
}

#[test]
fn parite_du_preflight() {
    for c in fixture()["preflight"].as_array().unwrap() {
        let tmp = tempfile::tempdir().unwrap();
        let home = tmp.path();
        let h = home.to_string_lossy().into_owned();
        for f in c["files"].as_array().into_iter().flatten() {
            let f = s(f);
            let p = home.join(f.trim_end_matches('/'));
            if f.ends_with('/') {
                std::fs::create_dir_all(&p).unwrap();
            } else {
                std::fs::create_dir_all(p.parent().unwrap()).unwrap();
                std::fs::write(&p, "x").unwrap();
            }
        }
        let cfg: HashMap<String, String> = c["config"]
            .as_object()
            .unwrap()
            .iter()
            .map(|(k, v)| (k.clone(), s(v).replace("{{HOME}}", &h)))
            .collect();
        let dirs: Vec<String> = c["dirs"]
            .as_array()
            .into_iter()
            .flatten()
            .map(|d| home.join(s(d)).to_string_lossy().into_owned())
            .collect();
        let got = match ec::preflight_engine(home, &cfg, &dirs, None) {
            Ok(()) => String::new(),
            Err(e) => e.replace(&h, "{{HOME}}"),
        };
        assert_eq!(got, s(&c["out"]), "{}", c["name"]);
    }
}

/// Un faux superviseur : note chaque appel, démarre un `sleep` dont le PID
/// va dans le fichier du service, l'arrête sur « stop » ; « start » échoue si
/// `$LOKI_HOME/echec` existe, comme le vrai : dernière ligne d'erreur.
fn fake_supervisor(dir: &Path) -> PathBuf {
    script(
        dir,
        "loki-rs",
        r#"#!/bin/sh
echo "$1 $2 $LOKI_HOME $LOKI_SERVICE $LOKI_GO_ADDR $LOKI_INTERNAL_TOKEN" >> "$LOKI_HOME/trace"
pid="$LOKI_HOME/$LOKI_SERVICE.pid"
case "$2" in
start)
  if [ -e "$LOKI_HOME/echec" ]; then
    echo "[ERREUR] le processus s'est arrêté"
    echo "[loki-rs engine] service $LOKI_SERVICE non démarré" >&2
    exit 1
  fi
  sleep 60 >/dev/null 2>&1 < /dev/null &
  echo $! > "$pid"
  ;;
stop)
  [ -s "$pid" ] && kill "$(cat "$pid")" 2>/dev/null
  rm -f "$pid"
  ;;
esac
exit 0
"#,
    )
}

struct Banc {
    facade: std::net::SocketAddr,
    go: Arc<fakego::FakeGo>,
    home: PathBuf,
    _stop: tokio::sync::oneshot::Sender<()>,
    _tmp: tempfile::TempDir,
}

impl Banc {
    fn trace(&self) -> Vec<String> {
        std::fs::read_to_string(self.home.join("trace"))
            .unwrap_or_default()
            .lines()
            .map(|l| l.split(' ').take(2).collect::<Vec<_>>().join(" "))
            .collect()
    }
    fn pid(&self) -> i32 {
        loki_rs::engine::read_pid(&self.home.join("loki-engine.pid"))
    }
}

async fn banc(nvidia_smi: Option<&str>) -> Banc {
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().to_path_buf();
    let bin = fake_supervisor(&home);
    let mut ctl = EngineCtl::new(
        bin,
        vec![
            ("LOKI_HOME".into(), home.to_string_lossy().into_owned()),
            ("LOKI_SERVICE".into(), "loki-engine".into()),
            ("LOKI_GO_ADDR".into(), "127.0.0.1:1".into()),
            ("LOKI_INTERNAL_TOKEN".into(), "jeton".into()),
        ],
    );
    ctl.nvidia_smi = match nvidia_smi {
        Some(body) => script(&home, "nvidia-smi", body)
            .to_string_lossy()
            .into_owned(),
        None => home
            .join("absent-nvidia-smi")
            .to_string_lossy()
            .into_owned(),
    };
    ctl.settle_pause = Duration::ZERO;
    let opts = fakego::Opts {
        engine_ctl: Some(Arc::new(ctl)),
        ..Default::default()
    };
    let (facade, go, stop, _) = fakego::start_with(home.clone(), opts).await;
    Banc {
        facade,
        go,
        home,
        _stop: stop,
        _tmp: tmp,
    }
}

#[tokio::test]
async fn parite_des_routes() {
    for st in fixture()["steps"].as_array().unwrap() {
        let b = banc(None).await;
        let h = b.home.to_string_lossy().into_owned();
        if st["tune"].as_bool() == Some(true) {
            *b.go.tune_busy.lock().unwrap() = Some(
                "optimisation en cours (loki tune) : le moteur est réservé aux essais — attends la fin ou annule-la"
                    .into(),
            );
        }
        *b.go.generating.lock().unwrap() = st["generating"].as_bool() == Some(true);
        for (k, v) in st["config"].as_object().into_iter().flatten() {
            b.go.set("config", k, s(v));
        }
        let (code, headers, out) = call(
            b.facade,
            s(&st["method"]),
            s(&st["path"]),
            st["body"].as_str().unwrap_or(""),
        )
        .await;
        let name = &st["name"];
        assert_eq!(
            code.as_u16() as i64,
            st["status"].as_i64().unwrap(),
            "{name}"
        );
        assert_eq!(
            headers.get("allow").map_or("", |v| v.to_str().unwrap()),
            st["allow"].as_str().unwrap_or(""),
            "{name}"
        );
        let out: Value = serde_json::from_str(&out.to_string().replace(&h, "{{HOME}}")).unwrap();
        assert_eq!(out, st["out"], "{name}");
        // Aucune de ces réponses ne lance quoi que ce soit.
        assert!(b.trace().is_empty(), "{name} : {:?}", b.trace());
    }
}

#[tokio::test]
async fn demarrage_arret_redemarrage() {
    let b = banc(None).await;
    let (c, _, v) = call(b.facade, "POST", "/api/start", "").await;
    assert_eq!(
        (c.as_u16(), v.clone()),
        (200, json!({"ok": true, "out": "ok"}))
    );
    assert_eq!(*b.go.prestarts.lock().unwrap(), 1);
    assert!(b.pid() > 0);
    // Environnement passé au superviseur.
    let line = std::fs::read_to_string(b.home.join("trace")).unwrap();
    assert_eq!(
        line.trim(),
        format!(
            "engine start {} loki-engine 127.0.0.1:1 jeton",
            b.home.display()
        )
    );

    // Optimisation en cours : refus, superviseur jamais appelé.
    *b.go.tune_busy.lock().unwrap() = Some("optimisation en cours".into());
    let (_, _, v) = call(b.facade, "POST", "/api/restart", "").await;
    assert_eq!(v, json!({"ok": false, "out": "optimisation en cours"}));
    assert_eq!(b.trace(), ["engine start"]);
    // L'arrêt, lui, passe toujours (et sans consulter le verrou).
    let (_, _, v) = call(b.facade, "GET", "/api/stop", "").await;
    assert_eq!(v, json!({"ok": true, "out": "ok"}));
    assert_eq!(*b.go.prestarts.lock().unwrap(), 2);
    assert_eq!(b.pid(), 0);
    *b.go.tune_busy.lock().unwrap() = None;

    // Échec du démarrage : la dernière ligne d'erreur, sans préfixe.
    std::fs::write(b.home.join("echec"), "").unwrap();
    let (_, _, v) = call(b.facade, "POST", "/api/start", "").await;
    assert_eq!(
        v,
        json!({"ok": false, "out": "service loki-engine non démarré"})
    );
    std::fs::remove_file(b.home.join("echec")).unwrap();

    let (_, _, v) = call(b.facade, "POST", "/api/restart", "").await;
    assert_eq!(v, json!({"ok": true, "out": "ok"}));
    assert_eq!(
        b.trace(),
        [
            "engine start",
            "engine stop",
            "engine start",
            "engine restart"
        ]
    );

    // Preset externe : rien à lancer ; un redémarrage arrête le moteur local.
    b.go.set("config", "EXTERNAL", "1");
    let (_, _, v) = call(b.facade, "POST", "/api/restart", "").await;
    assert_eq!(v, json!({"ok": true, "out": "ok"}));
    let (_, _, v) = call(b.facade, "POST", "/api/start", "").await;
    assert_eq!(v, json!({"ok": true, "out": "ok"}));
    assert_eq!(b.trace().last().unwrap(), "engine stop");
    assert_eq!(b.trace().len(), 5);
    assert_eq!(b.pid(), 0);
}

#[tokio::test]
async fn dechargement_avec_bilan() {
    // nvidia-smi : 20 Go occupés avant l'arrêt, puis la mémoire part.
    let smi = r#"#!/bin/sh
n=$(cat "$0.n" 2>/dev/null || echo 0)
echo $((n+1)) > "$0.n"
case $n in
0) echo "RTX, 20000, 24000, 50, 60" ;;
1) echo "RTX, 20000, 24000, 0, 60" ;;
2) echo "RTX, 9000, 24000, 0, 55" ;;
*) echo "RTX, 700, 24000, 0, 50" ;;
esac
"#;
    let b = banc(Some(smi)).await;
    let (_, _, v) = call(b.facade, "POST", "/api/start", "").await;
    assert_eq!(v["ok"], true);
    let pid = b.pid();
    assert!(pid > 0);

    // Une réponse en cours (conversation au Go) : refus, sauf forcé.
    *b.go.generating.lock().unwrap() = true;
    let (c, _, v) = call(b.facade, "POST", "/api/vram/unload", "{}").await;
    assert_eq!(c, 409);
    assert_eq!(v["generating"], true);
    let (c, _, v) = call(b.facade, "POST", "/api/vram/unload", r#"{"force":true}"#).await;
    assert_eq!(c, 200);
    assert_eq!(
        v,
        json!({
            "ok": true, "error": "", "was_active": true, "active": false, "gpu": true,
            "before_mb": 20000, "used_mb": 700, "freed_mb": 19300,
        })
    );
    assert_eq!(b.trace(), ["engine start", "engine stop"]);
    assert_eq!(*b.go.dictate_stops.lock().unwrap(), 1);
    assert_eq!(b.pid(), 0);

    // Rechargement : préflight, puis démarrage ; déjà actif ensuite.
    let bin = b.home.join("llama-server");
    std::fs::write(&bin, "").unwrap();
    std::fs::create_dir_all(b.home.join("models")).unwrap();
    std::fs::write(b.home.join("models/m.gguf"), "x").unwrap();
    b.go.set("config", "BIN", &bin.to_string_lossy());
    b.go.set("config", "MODEL", "m.gguf");
    let (_, _, v) = call(b.facade, "POST", "/api/vram/reload", "").await;
    assert_eq!(v, json!({"ok": true}));
    let (_, _, v) = call(b.facade, "POST", "/api/vram/reload", "").await;
    assert_eq!(v, json!({"ok": true, "already": true}));
    assert_eq!(b.trace(), ["engine start", "engine stop", "engine start"]);
    // Démarrage refusé par le verrou après le préflight.
    let (_, _, _) = call(b.facade, "POST", "/api/stop", "").await;
    *b.go.tune_busy.lock().unwrap() = Some("optimisation en cours".into());
    let (c, _, v) = call(b.facade, "POST", "/api/vram/reload", "").await;
    assert_eq!(c, 409);
    assert_eq!(v["tuning"], true);
    let _ = std::fs::remove_file(b.home.join("loki-engine.pid"));
}

#[tokio::test]
async fn dechargement_arret_en_echec() {
    // Un moteur actif que le superviseur ne sait pas arrêter.
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().to_path_buf();
    let bin = script(
        &home,
        "loki-rs",
        "#!/bin/sh\necho \"[loki-rs engine] \x1b[1marrêt impossible\x1b[0m\" >&2\nexit 1\n",
    );
    let mut sleeper = std::process::Command::new("sleep")
        .arg("30")
        .spawn()
        .unwrap();
    std::fs::write(home.join("loki-engine.pid"), sleeper.id().to_string()).unwrap();
    let mut ctl = EngineCtl::new(bin, vec![]);
    ctl.nvidia_smi = home.join("absent").to_string_lossy().into_owned();
    let opts = fakego::Opts {
        engine_ctl: Some(Arc::new(ctl)),
        ..Default::default()
    };
    let (facade, go, _stop, _) = fakego::start_with(home.clone(), opts).await;
    let (_, _, v) = call(facade, "POST", "/api/vram/unload", "").await;
    assert_eq!(
        v,
        json!({
            "ok": false, "error": "arrêt du moteur : arrêt impossible", "was_active": true,
            "active": true, "gpu": false, "before_mb": 0, "used_mb": 0, "freed_mb": 0,
        })
    );
    assert_eq!(*go.dictate_stops.lock().unwrap(), 1);
    let _ = sleeper.kill();
    let _ = sleeper.wait();
}

#[tokio::test]
async fn sans_la_variable_le_moteur_reste_au_go() {
    let tmp = tempfile::tempdir().unwrap();
    let (facade, _go, _stop) = fakego::start(tmp.path().to_path_buf()).await;
    for p in [
        "/api/start",
        "/api/stop",
        "/api/restart",
        "/api/vram/unload",
        "/api/vram/reload",
    ] {
        let (_, _, b) = fakego::get(facade, p, &[]).await;
        assert_eq!(&b[..], format!("go:{p}").as_bytes());
    }
}

#[tokio::test]
async fn dechargement_vram_en_hausse() {
    // Une autre application prend la carte pendant le déchargement : jamais
    // de « Mo libérés » négatifs.
    let smi = r#"#!/bin/sh
n=$(cat "$0.n" 2>/dev/null || echo 0)
echo $((n+1)) > "$0.n"
if [ "$n" = 0 ]; then echo "A, 5000, 24000, 0, 40"; else echo "A, 6000, 24000, 0, 40"; fi
"#;
    let b = banc(Some(smi)).await;
    let (_, _, v) = call(b.facade, "POST", "/api/vram/unload", "").await;
    assert_eq!(
        v,
        json!({
            "ok": true, "error": "", "was_active": false, "active": false, "gpu": true,
            "before_mb": 5000, "used_mb": 6000, "freed_mb": 0,
        })
    );
    assert!(b.trace().is_empty());
}
