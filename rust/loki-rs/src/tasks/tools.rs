//! Les outils `task_*` de l'agent (lot 37) — port de `tasks_tools.go` :
//! le modèle gère lui-même ses tâches planifiées (rappel, veille, ménage
//! périodique), les liste, les ajuste, les supprime. Mêmes règles que le
//! panneau Tâches : fréquence validée, prochaine exécution calculée dans le
//! fuseau de l'utilisateur, tâche rattachée au projet actif — l'IA ne voit
//! et ne pilote que celles de son projet.
//!
//! La façade les exécute quand elle tient les tâches (`LOKI_TASKS=rust`) ;
//! sinon l'appel part au Go.

use std::path::Path;

use serde_json::{Map, Value};

use super::cron::{compute_next_run, loc, next_after, parse_hhmm, parse_ymd_hm};
use super::{BK_TASKS, Task, get_task, list_tasks, new_task_id, save_task, script_exists};
use crate::paths::DEFAULT_PROJECT_SLUG;
use crate::store::{BK_STATE, Store};
use crate::tz::Location;

const NS: i64 = 1_000_000_000;
const MINUTE: i64 = 60 * NS;

/// `stUserTZKey` : fuseau IANA du navigateur de l'utilisateur.
pub const ST_USER_TZ: &str = "user_tz";

/// Les outils portés ici.
pub fn is_tool(name: &str) -> bool {
    matches!(
        name,
        "task_list" | "task_create" | "task_update" | "task_delete"
    )
}

/// Ce qu'il faut pour exécuter un outil : la base, le dossier protégé des
/// scripts et l'heure des tâches (ns Unix).
pub struct ToolEnv<'a> {
    pub store: &'a Store,
    pub scripts_dir: &'a Path,
    pub now_ns: i64,
}

/// Un appel d'outil `task_*` ; `None` pour un autre outil.
pub async fn exec(env: &ToolEnv<'_>, name: &str, args: &Map<String, Value>) -> Option<String> {
    Some(match name {
        "task_list" => task_list(env, args).await,
        "task_create" => task_create(env, args).await,
        "task_update" => task_update(env, args).await,
        "task_delete" => task_delete(env, args).await,
        _ => return None,
    })
}

/// `strings.TrimSpace`.
fn go_trim(s: &str) -> &str {
    s.trim_matches(char::is_whitespace)
}

/// `taskStrArg` : une chaîne, `""` si absente ou d'un autre type.
fn str_arg<'a>(args: &'a Map<String, Value>, k: &str) -> &'a str {
    args.get(k).and_then(Value::as_str).unwrap_or("")
}

/// `Format("2006-01-02 15:04")` d'un instant en ms Unix, dans `l`.
fn fmt_ms(ms: i64, l: &Location) -> String {
    l.civil(ms.div_euclid(1000)).ymd_hm()
}

/// `activeProjectSlug` et `taskProjectOf`, lus une fois par appel.
struct Projects {
    active: String,
    known: Vec<String>,
}

impl Projects {
    async fn load(store: &Store) -> Self {
        Projects {
            active: crate::memory::active_project(store).await,
            known: crate::memory::list_projects(store)
                .await
                .into_iter()
                .map(|p| p.slug)
                .collect(),
        }
    }

    /// `taskProjectOf` : le projet d'une tâche, absent ou inconnu → le
    /// projet par défaut.
    fn of(&self, t: &Task) -> String {
        let s = go_trim(&t.project);
        if !s.is_empty() && self.known.iter().any(|k| k == s) {
            return s.to_string();
        }
        DEFAULT_PROJECT_SLUG.to_string()
    }
}

/// `defaultTaskTZ` : le fuseau du navigateur s'il est connu, sinon celui
/// d'une tâche existante, sinon vide (heure locale du serveur).
async fn default_task_tz(store: &Store) -> String {
    let tz = store.get_str(BK_STATE, ST_USER_TZ).await;
    if !tz.is_empty() {
        return tz;
    }
    list_tasks(store)
        .await
        .into_iter()
        .find(|t| !go_trim(&t.tz).is_empty())
        .map(|t| t.tz)
        .unwrap_or_default()
}

/// `tzLabel`.
fn tz_label(tz: &str) -> &str {
    if tz.is_empty() {
        "heure du serveur"
    } else {
        tz
    }
}

/// `toolTaskList` : avec un id, cette tâche seule et son compte-rendu
/// complet ; sinon la liste du projet, un aperçu court par tâche.
async fn task_list(env: &ToolEnv<'_>, args: &Map<String, Value>) -> String {
    let projects = Projects::load(env.store).await;
    let id = go_trim(str_arg(args, "id"));
    if !id.is_empty() {
        return match get_task(env.store, id).await {
            Some(t) if projects.of(&t) == projects.active => format_task(&t, true),
            _ => format!("[erreur] tâche introuvable : {id}"),
        };
    }
    let tasks: Vec<Task> = list_tasks(env.store)
        .await
        .into_iter()
        .filter(|t| projects.of(t) == projects.active)
        .collect();
    if tasks.is_empty() {
        return "[aucune tâche planifiée dans ce projet]".into();
    }
    let mut b = String::new();
    for t in &tasks {
        b.push_str(&format_task(t, false));
        b.push('\n');
    }
    b.trim_end_matches('\n').to_string()
}

/// `formatTask` : `full`, le compte-rendu entier ; sinon un aperçu d'une
/// ligne.
pub fn format_task(t: &Task, full: bool) -> String {
    let state = if t.enabled { "on" } else { "off" };
    let mut b = format!("- {} [{state}] « {} » — {}", t.id, t.name, t.schedule);
    let l = loc(&t.tz);
    if t.next_run > 0 && t.enabled {
        b.push_str(&format!(" (prochaine : {})", fmt_ms(t.next_run, &l)));
    }
    if t.last_run > 0 {
        let when = fmt_ms(t.last_run, &l);
        if t.last_ok {
            b.push_str(&format!(" ✓ dernier run {when}"));
        } else {
            b.push_str(&format!(" ✗ dernier run {when} : {}", t.last_error));
        }
        let r = go_trim(&t.last_report);
        if !r.is_empty() {
            if full {
                b.push_str("\n  compte-rendu complet :\n");
                b.push_str(r);
            } else {
                b.push_str("\n    compte-rendu : ");
                b.push_str(&preview_report(r));
            }
        }
    }
    b
}

/// `previewReport` : aplati, coupé à 300 octets (un caractère coupé devient
/// U+FFFD par octet, comme à l'encodage JSON du Go).
pub fn preview_report(s: &str) -> String {
    let s = s.split_whitespace().collect::<Vec<_>>().join(" ");
    const MAX: usize = 300;
    if s.len() > MAX {
        return format!(
            "{} […] (id pour le compte-rendu complet)",
            crate::system::go_lossy(&s.as_bytes()[..MAX])
        );
    }
    s
}

/// `toolTaskCreate`.
async fn task_create(env: &ToolEnv<'_>, args: &Map<String, Value>) -> String {
    let name = go_trim(str_arg(args, "name"));
    let prompt = go_trim(str_arg(args, "prompt"));
    let script = go_trim(str_arg(args, "script"));
    let mut schedule = go_trim(str_arg(args, "schedule")).to_string();
    let tz = default_task_tz(env.store).await;
    // Tâche unique : « dans N minutes » ou « à HH:MM », calculés ici dans le
    // fuseau de l'utilisateur.
    if schedule.is_empty() {
        let l = loc(&tz);
        let at = go_trim(str_arg(args, "at"));
        if let Some(v) = args
            .get("in_minutes")
            .and_then(Value::as_f64)
            .filter(|v| *v >= 1.0)
        {
            // time.Duration(v) * time.Minute : tronqué vers zéro.
            let t = env
                .now_ns
                .wrapping_add((v as i64).wrapping_mul(MINUTE))
                .div_euclid(NS);
            schedule = format!("@once {}", l.civil(t).ymd_hm());
        } else if !at.is_empty() {
            if let Some(t) = parse_ymd_hm(at, &l) {
                let sec = t.div_euclid(i128::from(NS)) as i64;
                schedule = format!("@once {}", l.civil(sec).ymd_hm());
            } else if let Ok((hh, mm)) = parse_hhmm(at) {
                let now = l.civil(env.now_ns.div_euclid(NS));
                let mut t = l.date(now.year, now.month, now.day, hh, mm, 0);
                if i128::from(t) * i128::from(NS) <= i128::from(env.now_ns) {
                    // AddDate(0, 0, 1) : le lendemain, à la même heure murale.
                    let c = l.civil(t);
                    t = l.date(c.year, c.month, c.day + 1, c.hour, c.minute, c.second);
                }
                schedule = format!("@once {}", l.civil(t).ymd_hm());
            } else {
                return "[erreur] 'at' invalide : « HH:MM » ou « AAAA-MM-JJ HH:MM »".into();
            }
        }
    }
    if name.is_empty() || schedule.is_empty() {
        return "[erreur] name obligatoire, et schedule (tâche répétée) ou in_minutes / at (tâche unique)".into();
    }
    let mut kind = "";
    if !script.is_empty() {
        // Tâche script : le script doit exister dans le dossier protégé.
        kind = "script";
        if let Err(e) = script_exists(env.scripts_dir, script) {
            return format!("[erreur] {e}");
        }
    } else if prompt.is_empty() {
        return "[erreur] fournis soit 'prompt' (tâche IA) soit 'script' (tâche script)".into();
    }
    if let Err(e) = next_after(&schedule, &tz, env.now_ns) {
        return format!("[erreur] fréquence invalide : {e}");
    }
    let enabled = args.get("enabled").and_then(Value::as_bool).unwrap_or(true);
    let mut t = Task {
        id: new_task_id(),
        name: name.to_string(),
        prompt: prompt.to_string(),
        schedule: schedule.clone(),
        tz: tz.clone(),
        enabled,
        kind: kind.to_string(),
        script: script.to_string(),
        // L'IA ne crée que DANS son projet.
        project: crate::memory::active_project(env.store).await,
        ..Default::default()
    };
    t.next_run = compute_next_run(&schedule, &tz, env.now_ns);
    if let Err(e) = save_task(env.store, &t).await {
        return format!("[erreur] {e}");
    }
    let when = if t.next_run > 0 && enabled {
        format!(
            ", prochaine exécution {} ({})",
            fmt_ms(t.next_run, &loc(&tz)),
            tz_label(&tz)
        )
    } else {
        String::new()
    };
    format!("[ok] tâche '{name}' créée (id {}){when}", t.id)
}

/// `toolTaskUpdate` : modification partielle.
async fn task_update(env: &ToolEnv<'_>, args: &Map<String, Value>) -> String {
    let id = go_trim(str_arg(args, "id"));
    if id.is_empty() {
        return "[erreur] id manquant".into();
    }
    let projects = Projects::load(env.store).await;
    let mut t = match get_task(env.store, id).await {
        Some(t) if projects.of(&t) == projects.active => t,
        _ => return format!("[erreur] tâche introuvable : {id}"),
    };
    let text = |k: &str| {
        args.get(k)
            .and_then(Value::as_str)
            .map(go_trim)
            .filter(|v| !v.is_empty())
    };
    if let Some(v) = text("name") {
        t.name = v.to_string();
    }
    if let Some(v) = text("prompt") {
        t.prompt = v.to_string();
    }
    if let Some(sched) = text("schedule") {
        if let Err(e) = next_after(sched, &t.tz, env.now_ns) {
            return format!("[erreur] fréquence invalide : {e}");
        }
        t.schedule = sched.to_string();
        t.next_run = compute_next_run(sched, &t.tz, env.now_ns);
    }
    if let Some(v) = args.get("enabled").and_then(Value::as_bool) {
        t.enabled = v;
        if v && t.next_run == 0 {
            t.next_run = compute_next_run(&t.schedule, &t.tz, env.now_ns);
        }
    }
    if let Err(e) = save_task(env.store, &t).await {
        return format!("[erreur] {e}");
    }
    format!("[ok] tâche '{}' (id {}) mise à jour", t.name, t.id)
}

/// `toolTaskDelete`.
async fn task_delete(env: &ToolEnv<'_>, args: &Map<String, Value>) -> String {
    let id = go_trim(str_arg(args, "id"));
    if id.is_empty() {
        return "[erreur] id manquant".into();
    }
    let projects = Projects::load(env.store).await;
    let t = match get_task(env.store, id).await {
        Some(t) if projects.of(&t) == projects.active => t,
        _ => return format!("[erreur] tâche introuvable : {id}"),
    };
    if let Err(e) = env.store.del(BK_TASKS, id).await {
        return format!("[erreur] {e}");
    }
    format!("[ok] tâche '{}' (id {id}) supprimée", t.name)
}
