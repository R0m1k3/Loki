//! Parité des outils fichiers et du shell avec le Go (lot 7).
//!
//! `tests/parity_tools/outils.json` : journal produit par `execTool`
//! (`internal/loki/parity_tools_test.go`, régénéré par
//! `LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteOutils`). La
//! même arborescence est recréée ici et les mêmes appels passent, dans le même
//! ordre, par `tools::exec_local` : libellés, résultats, diffs, questions et
//! arborescence finale doivent être ceux du Go. L'état partagé (suivi « lu
//! avant d'écrire ») est tenu par le Go en service ; ici un faux qui en
//! reprend les règles et les messages.

use std::collections::{BTreeMap, HashMap};
use std::path::{Path, PathBuf};
use std::sync::Mutex;
use std::time::{Duration, SystemTime};

use regex::Regex;
use serde::Deserialize;
use serde_json::{Map, Value, json};

use loki_rs::chat::stream::tool_call_label;
use loki_rs::chat::types::{AskEvent, DiffLine};
use loki_rs::tools::{ToolCtx, ToolState, exec_local};

#[derive(Deserialize)]
struct FileFx {
    path: String,
    content: String,
    age: u64,
}

#[derive(Deserialize)]
struct CaseFx {
    name: String,
    tool: String,
    args: Map<String, Value>,
    #[serde(default)]
    code: bool,
    #[serde(default)]
    unix: bool,
    label: String,
    result: String,
    #[serde(default)]
    diff: Vec<DiffLine>,
    #[serde(default)]
    added: i64,
    #[serde(default)]
    removed: i64,
    #[serde(default)]
    ask: Option<AskEvent>,
}

#[derive(Deserialize)]
struct Fixture {
    cwd: String,
    memory_dir: String,
    files: Vec<FileFx>,
    cases: Vec<CaseFx>,
    after: BTreeMap<String, String>,
}

/// Le suivi « lu avant d'écrire » du Go (`code_tracker.go`), en mémoire.
struct FakeState {
    ctx: ToolCtx,
    reads: Mutex<HashMap<PathBuf, SystemTime>>,
}

impl ToolState for FakeState {
    async fn context(&self) -> Result<ToolCtx, String> {
        Ok(self.ctx.clone())
    }

    async fn note_read(&self, path: &Path) {
        self.reads
            .lock()
            .unwrap()
            .insert(path.to_path_buf(), SystemTime::now());
    }

    async fn check(&self, path: &Path, _for_write: bool) -> Option<String> {
        let mtime = std::fs::metadata(path).ok()?.modified().ok()?;
        let p = path.display();
        match self.reads.lock().unwrap().get(path) {
            None => Some(format!(
                "[refusé] {p} existe mais tu ne l'as pas lu dans cette discussion. Lis-le d'abord (outil read), puis modifie-le."
            )),
            Some(at) if mtime > *at => Some(format!(
                "[refusé] {p} a changé sur le disque depuis ta lecture. Relis-le (outil read) avant de le modifier."
            )),
            Some(_) => None,
        }
    }

    async fn wrote(&self, path: &Path) -> String {
        self.note_read(path).await;
        String::new()
    }
}

fn subst(v: &Value, root: &str, mem: &str) -> Value {
    match v {
        Value::String(s) => Value::String(s.replace("{{MEM}}", mem).replace("{{ROOT}}", root)),
        Value::Object(m) => Value::Object(
            m.iter()
                .map(|(k, e)| (k.clone(), subst(e, root, mem)))
                .collect(),
        ),
        Value::Array(a) => Value::Array(a.iter().map(|e| subst(e, root, mem)).collect()),
        _ => v.clone(),
    }
}

fn norm(s: &str, root: &str) -> String {
    static DUR: std::sync::LazyLock<Regex> = std::sync::LazyLock::new(|| {
        Regex::new(r"(exit: -?\d+) · [0-9.]+[a-zµ]+(?:[0-9.]+[a-zµ]+)*").unwrap()
    });
    DUR.replace_all(&s.replace(root, "{{ROOT}}"), "$1 · {{DUR}}")
        .into_owned()
}

fn tree(root: &Path) -> BTreeMap<String, String> {
    fn walk(root: &Path, dir: &Path, out: &mut BTreeMap<String, String>) {
        for e in std::fs::read_dir(dir).unwrap().flatten() {
            let p = e.path();
            if p.is_dir() {
                walk(root, &p, out);
                continue;
            }
            let rel = p
                .strip_prefix(root)
                .unwrap()
                .to_string_lossy()
                .replace('\\', "/");
            if rel.starts_with("ws/") || rel == "dehors.txt" || rel == "evade.txt" {
                out.insert(
                    rel,
                    String::from_utf8_lossy(&std::fs::read(&p).unwrap()).into_owned(),
                );
            }
        }
    }
    let mut out = BTreeMap::new();
    walk(root, root, &mut out);
    out
}

#[tokio::test]
async fn outils_comme_le_go() {
    let path = Path::new(env!("CARGO_MANIFEST_DIR")).join("tests/parity_tools/outils.json");
    let fx: Fixture = serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
    let tmp = tempfile::tempdir().unwrap();
    let root = std::fs::canonicalize(tmp.path()).unwrap();
    let root_s = root.display().to_string();
    let now = SystemTime::now();
    for f in &fx.files {
        let p = root.join(&f.path);
        std::fs::create_dir_all(p.parent().unwrap()).unwrap();
        std::fs::write(&p, &f.content).unwrap();
        std::fs::File::options()
            .write(true)
            .open(&p)
            .unwrap()
            .set_modified(now - Duration::from_secs(f.age))
            .unwrap();
    }
    let mem = root.join(&fx.memory_dir);
    let st = FakeState {
        ctx: ToolCtx {
            cwd: root.join(&fx.cwd),
            memory_dir: mem.clone(),
            target: String::new(),
            code: false,
        },
        reads: Mutex::new(HashMap::new()),
    };
    let mem_s = mem.display().to_string();
    let mut diffs = Vec::new();
    for c in &fx.cases {
        if c.unix && cfg!(windows) {
            continue;
        }
        let args = match subst(&Value::Object(c.args.clone()), &root_s, &mem_s) {
            Value::Object(m) => m,
            _ => unreachable!(),
        };
        let label = tool_call_label(&c.tool, &args);
        let out = exec_local(&st, c.code, &c.tool, &args, &label)
            .await
            .unwrap_or_else(|| panic!("{} : non exécuté ici", c.name));
        let got = json!({
            "label": norm(&label, &root_s),
            "result": norm(&out.result, &root_s),
            "diff": out.diff,
            "added": out.added,
            "removed": out.removed,
            "ask": out.ask,
        });
        let want = json!({
            "label": c.label,
            "result": c.result,
            "diff": c.diff,
            "added": c.added,
            "removed": c.removed,
            "ask": c.ask,
        });
        if got != want {
            diffs.push(format!("{} :\n  obtenu  {got}\n  attendu {want}", c.name));
        }
    }
    let after = tree(&root);
    if after != fx.after {
        diffs.push(format!(
            "arborescence finale :\n  obtenu  {after:?}\n  attendu {:?}",
            fx.after
        ));
    }
    assert!(
        diffs.is_empty(),
        "{} écart(s) avec le Go :\n\n{}",
        diffs.len(),
        diffs.join("\n\n")
    );
}

/// Outils non portés, ou cible distante : c'est au Go de les exécuter.
#[tokio::test]
async fn le_reste_va_au_go() {
    let st = FakeState {
        ctx: ToolCtx {
            cwd: PathBuf::from("/nulle-part"),
            memory_dir: PathBuf::from("/nulle-part/memory"),
            target: "poste".into(),
            code: false,
        },
        reads: Mutex::new(HashMap::new()),
    };
    let a = |v: Value| v.as_object().unwrap().clone();
    for (name, args) in [
        ("mem_read", json!({"file":"x"})),
        ("web_search", json!({"query":"x"})),
        ("write", json!({"file":"a","content":"x"})),
        ("edit", json!({"file":"a","old":"x","new":"y"})),
        ("bash", json!({"command":"ls"})),
    ] {
        let args = a(args);
        let label = tool_call_label(name, &args);
        assert!(
            exec_local(&st, false, name, &args, &label).await.is_none(),
            "{name}"
        );
    }
    // Une commande catastrophique est refusée ici, cible distante comprise.
    let args = a(json!({"command":"rm -rf /"}));
    let out = exec_local(&st, false, "bash", &args, "rm -rf /")
        .await
        .unwrap();
    assert!(out.result.starts_with("[refusé] Commande bloquée"));
    // read/grep/glob lisent toujours cette machine.
    let args = a(json!({"pattern":"*"}));
    assert!(exec_local(&st, false, "glob", &args, "*").await.is_some());
}
