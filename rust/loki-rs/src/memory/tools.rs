//! Les outils mémoire de l'agent (lot 40) — port de `chat_memory.go`
//! (`MemSearch`, `MemRead`, `MemAdd`, `MemEdit`) et de l'outil `tracker`
//! (`tracker.go`), exécutés par la façade quand elle tient la mémoire
//! (`LOKI_MEM=rust`). La mémoire est celle du projet actif ; les pages
//! passent par les mêmes lectures et écritures que les routes (scellées
//! selon le chiffrement, `.bak`, index `MEMORY.md` tenu par le code).

use std::collections::HashMap;

use serde_json::{Map, Value};

use super::{
    Memory, ReadErr, Tracker, TrackerEvent, Tz, active_project, parse_when, slugify_ident,
};
use crate::chat::types::DiffLine;
use crate::store::Store;

/// Les outils portés ici.
pub fn is_tool(name: &str) -> bool {
    matches!(
        name,
        "mem_search" | "mem_read" | "mem_add" | "mem_edit" | "tracker"
    )
}

/// L'issue d'un appel : le texte rendu au modèle et le diff montré.
#[derive(Debug, Default, PartialEq)]
pub struct MemOut {
    pub result: String,
    pub diff: Vec<DiffLine>,
    pub added: i64,
    pub removed: i64,
}

impl MemOut {
    fn text(s: impl Into<String>) -> Self {
        MemOut {
            result: s.into(),
            ..Default::default()
        }
    }
}

/// `strings.TrimSpace`.
fn go_trim(s: &str) -> &str {
    s.trim_matches(char::is_whitespace)
}

/// `unicode.ToLower` rune par rune (`strings.ToLower`).
fn go_lower(s: &str) -> String {
    s.chars()
        .map(|c| c.to_lowercase().next().unwrap_or(c))
        .collect()
}

/// Un entier d'argument (`args[k].(float64)` → `int`), 0 sinon.
fn int_arg(args: &Map<String, Value>, k: &str) -> i64 {
    args.get(k).and_then(Value::as_f64).map_or(0, |v| v as i64)
}

/// `str` de `tracker.go` : une chaîne, un nombre arrondi, sinon la forme `%v`.
fn go_str(v: Option<&Value>) -> String {
    match v {
        None | Some(Value::Null) => String::new(),
        Some(Value::String(s)) => s.clone(),
        Some(Value::Number(n)) => {
            let f = n.as_f64().unwrap_or(0.0);
            format!("{f:.0}")
        }
        Some(v) => go_fmt_v(v),
    }
}

/// `fmt.Sprintf("%v", v)` d'une valeur JSON décodée par le Go.
fn go_fmt_v(v: &Value) -> String {
    match v {
        Value::Null => "<nil>".into(),
        Value::Bool(b) => b.to_string(),
        Value::Number(n) => {
            let f = n.as_f64().unwrap_or(0.0);
            if f.fract() == 0.0 && f.abs() < 1e21 {
                format!("{f:.0}")
            } else {
                f.to_string()
            }
        }
        Value::String(s) => s.clone(),
        Value::Array(a) => format!("[{}]", a.iter().map(go_fmt_v).collect::<Vec<_>>().join(" ")),
        Value::Object(m) => {
            let mut keys: Vec<&String> = m.keys().collect();
            keys.sort();
            format!(
                "map[{}]",
                keys.iter()
                    .map(|k| format!("{k}:{}", go_fmt_v(&m[*k])))
                    .collect::<Vec<_>>()
                    .join(" ")
            )
        }
    }
}

/// Ce qu'il faut pour exécuter un outil : la base, la mémoire, et l'heure
/// (ms Unix) pour les trackers.
pub struct MemEnv<'a> {
    pub store: &'a Store,
    pub mem: &'a Memory,
    pub now_ms: i64,
}

/// Un appel d'outil mémoire ; `None` pour un autre outil. `label` : le
/// libellé du tour (`toolCallLabel`), argument principal des outils de page.
pub async fn exec(
    env: &MemEnv<'_>,
    name: &str,
    args: &Map<String, Value>,
    label: &str,
) -> Option<MemOut> {
    let project = active_project(env.store).await;
    let dir = env.mem.project_dir(&project);
    Some(match name {
        "mem_search" => {
            let hits = mem_search(env.store, &dir, label, int_arg(args, "limit")).await;
            if hits.is_empty() {
                MemOut::text("[aucun résultat]")
            } else {
                let mut b = String::new();
                for (file, title, snippet) in hits {
                    b.push_str(&format!("- {file} — {title}\n  {snippet}\n"));
                }
                MemOut::text(b.trim_end_matches('\n'))
            }
        }
        "mem_read" => match mem_read(
            env.store,
            &dir,
            label,
            int_arg(args, "offset"),
            int_arg(args, "limit"),
        )
        .await
        {
            Ok(c) => MemOut::text(c),
            Err(e) => MemOut::text(format!("[erreur] {e}")),
        },
        "mem_add" => {
            let content = args.get("content").and_then(Value::as_str).unwrap_or("");
            let pname = super::project_name(&super::list_projects(env.store).await, &project);
            match mem_add(env.store, &dir, &pname, label, content).await {
                Ok(()) => {
                    let (diff, added) = crate::tools::diff::added_diff(content);
                    MemOut {
                        result: format!("[ok] page '{label}' créée"),
                        diff,
                        added,
                        removed: 0,
                    }
                }
                Err(e) => MemOut::text(format!("[erreur] {e}")),
            }
        }
        "mem_edit" => {
            let s = |k: &str| args.get(k).and_then(Value::as_str).unwrap_or("");
            let (old, new) = (s("old"), s("new"));
            match mem_edit(env.store, &dir, label, old, new).await {
                Ok(()) => {
                    let (diff, added, removed) = crate::tools::diff::line_diff(old, new);
                    MemOut {
                        result: format!("[ok] page '{label}' modifiée"),
                        diff,
                        added,
                        removed,
                    }
                }
                Err(EditErr::Applied) => {
                    MemOut::text(format!("[ok] page '{label}' {ERR_ALREADY_APPLIED}"))
                }
                Err(EditErr::Other(e)) => MemOut::text(format!("[erreur] {e}")),
            }
        }
        "tracker" => MemOut::text(tracker(env, &project, args).await),
        _ => return None,
    })
}

// ---------------------------------------------------------------------------
// Pages
// ---------------------------------------------------------------------------

/// Une page illisible, dite comme le Go.
fn read_err(name: &str, e: &ReadErr) -> String {
    match e {
        ReadErr::Locked => format!("page '{name}' chiffrée — mémoire verrouillée"),
        _ => format!("page '{name}' introuvable"),
    }
}

/// `MemSearch` : les pages classées par couverture des termes, puis par
/// pertinence (rareté du terme, fréquence amortie, bonus du nom et du
/// titre) ; (fichier, titre, extrait).
pub async fn mem_search(
    store: &Store,
    dir: &std::path::Path,
    query: &str,
    limit: i64,
) -> Vec<(String, String, String)> {
    let limit = if limit <= 0 || limit > 30 {
        8
    } else {
        limit as usize
    };
    let terms = unique_terms(&go_lower(query));
    struct Doc {
        file: String,
        title: String,
        name: String,
        title_low: String,
        content: String,
        hay: String,
    }
    let mut docs = Vec::new();
    let mut df: HashMap<&str, i64> = HashMap::new();
    for (file, title) in Memory::list(store, dir).await {
        let content = match Memory::read_page(store, dir, &file).await {
            Ok(b) => String::from_utf8_lossy(&b).into_owned(),
            Err(_) => String::new(),
        };
        let hay = go_lower(&format!("{file}\n{content}"));
        for t in &terms {
            if hay.contains(t.as_str()) {
                *df.entry(t.as_str()).or_default() += 1;
            }
        }
        docs.push(Doc {
            name: go_lower(&file),
            title_low: go_lower(&title),
            file,
            title,
            content,
            hay,
        });
    }
    let n = docs.len() as f64;
    let mut ranked: Vec<(i64, f64, (String, String, String))> = Vec::new();
    for d in &docs {
        let (mut matched, mut score) = (0i64, 0.0f64);
        for t in &terms {
            let cnt = d.hay.matches(t.as_str()).count();
            if cnt == 0 {
                continue;
            }
            matched += 1;
            let dft = df.get(t.as_str()).copied().unwrap_or(0) as f64;
            let idf = ((n + 1.0) / (dft + 1.0)).ln() + 1.0;
            let tf = 1.0 + (cnt as f64).ln();
            let mut field = 1.0;
            if d.name.contains(t.as_str()) {
                field += 4.0;
            }
            if d.title_low.contains(t.as_str()) {
                field += 2.0;
            }
            score += idf * tf * field;
        }
        if matched == 0 && !terms.is_empty() {
            continue;
        }
        ranked.push((
            matched,
            score,
            (
                d.file.clone(),
                d.title.clone(),
                snippet_around(&d.content, &terms),
            ),
        ));
    }
    // Tri stable : couverture d'abord, puis pertinence.
    ranked.sort_by(|a, b| b.0.cmp(&a.0).then(b.1.total_cmp(&a.1)));
    ranked.into_iter().take(limit).map(|r| r.2).collect()
}

/// `uniqueTerms` : les mots distincts, dans l'ordre.
fn unique_terms(q: &str) -> Vec<String> {
    let mut out: Vec<String> = Vec::new();
    for t in q.split_whitespace() {
        if !out.iter().any(|o| o == t) {
            out.push(t.to_string());
        }
    }
    out
}

/// `snippetAround` : un court extrait autour de la première occurrence d'un
/// terme. Coupé à l'octet comme le Go : un caractère coupé devient U+FFFD
/// par octet, comme à son encodage JSON.
fn snippet_around(content: &str, terms: &[String]) -> String {
    let flat = content.split_whitespace().collect::<Vec<_>>().join(" ");
    let low = go_lower(&flat);
    let idx = terms.iter().filter_map(|t| low.find(t.as_str())).min();
    let fb = flat.as_bytes();
    let Some(idx) = idx else {
        if fb.len() > 160 {
            return format!("{}…", crate::system::go_lossy(&fb[..160]));
        }
        return flat;
    };
    let start = idx.saturating_sub(60).min(fb.len());
    let end = (idx + 100).min(fb.len());
    let mut s = crate::system::go_lossy(&fb[start..end.max(start)]);
    if start > 0 {
        s = format!("…{s}");
    }
    if end < fb.len() {
        s.push('…');
    }
    s
}

/// `MemRead` : une plage de lignes numérotées (1-indexé ; 500 au plus).
async fn mem_read(
    store: &Store,
    dir: &std::path::Path,
    name: &str,
    offset: i64,
    limit: i64,
) -> Result<String, String> {
    let b = Memory::read_page(store, dir, name)
        .await
        .map_err(|e| read_err(name, &e))?;
    let text = String::from_utf8_lossy(&b);
    let lines: Vec<&str> = text.split('\n').collect();
    let offset = if offset <= 0 { 1 } else { offset };
    let limit = if limit <= 0 || limit > 500 {
        500
    } else {
        limit
    };
    let mut out = String::new();
    let mut i = offset - 1;
    while (i as usize) < lines.len() && i < offset - 1 + limit {
        out.push_str(&format!("{}\t{}\n", i + 1, lines[i as usize]));
        i += 1;
    }
    Ok(out.trim_end_matches('\n').to_string())
}

/// `MemAdd` : une nouvelle page, jamais par-dessus une existante ; l'index
/// la reçoit toujours.
async fn mem_add(
    store: &Store,
    dir: &std::path::Path,
    project_name: &str,
    name: &str,
    content: &str,
) -> Result<(), String> {
    let p = Memory::safe_path(dir, name)?;
    if std::fs::metadata(&p).is_ok() {
        return Err("la page existe déjà — utilise mem_edit pour la modifier".into());
    }
    let body = format!("{}\n", content.trim_end_matches('\n'));
    Memory::write_page(store, dir, name, body.as_bytes()).await?;
    Memory::index_add(store, dir, name, project_name).await;
    Ok(())
}

/// `errAlreadyApplied`.
const ERR_ALREADY_APPLIED: &str = "déjà à jour — la page contient déjà cette modification";

enum EditErr {
    /// L'édition est déjà en place : pas un échec.
    Applied,
    Other(String),
}

/// `MemEdit` : `old` doit apparaître exactement une fois.
async fn mem_edit(
    store: &Store,
    dir: &std::path::Path,
    name: &str,
    old: &str,
    new: &str,
) -> Result<(), EditErr> {
    let b = Memory::read_page(store, dir, name)
        .await
        .map_err(|e| EditErr::Other(read_err(name, &e)))?;
    let content = String::from_utf8_lossy(&b).into_owned();
    if old.is_empty() {
        return Err(EditErr::Other("old vide".into()));
    }
    let n = content.matches(old).count();
    if n == 0 {
        if !new.is_empty() && content.contains(new) {
            return Err(EditErr::Applied);
        }
        return Err(EditErr::Other("old introuvable dans la page".into()));
    }
    if n > 1 {
        return Err(EditErr::Other(format!(
            "old apparaît {n} fois — ajoute du contexte pour le rendre unique"
        )));
    }
    let updated = content.replacen(old, new, 1);
    Memory::write_page(store, dir, name, updated.as_bytes())
        .await
        .map_err(EditErr::Other)
}

// ---------------------------------------------------------------------------
// Trackers
// ---------------------------------------------------------------------------

/// L'heure murale d'un instant (ms) dans le fuseau des trackers.
fn civil_at(tz: Tz, ms: i64) -> crate::tz::Civil {
    let sec = ms.div_euclid(1000);
    match tz {
        Tz::Fixed(off) => crate::tz::civil(sec + off),
        Tz::Local => crate::tz::Location::local().civil(sec),
    }
}

/// `fmtTS` : « AAAA-MM-JJ HH:MM ».
fn fmt_ts(tz: Tz, ms: i64) -> String {
    civil_at(tz, ms).ymd_hm()
}

/// `fmtDay` : « AAAA-MM-JJ ».
fn fmt_day(tz: Tz, ms: i64) -> String {
    let c = civil_at(tz, ms);
    format!("{:04}-{:02}-{:02}", c.year, c.month, c.day)
}

fn fmt_month(tz: Tz, ms: i64) -> String {
    let c = civil_at(tz, ms);
    format!("{:04}-{:02}", c.year, c.month)
}

fn fmt_year(tz: Tz, ms: i64) -> String {
    format!("{:04}", civil_at(tz, ms).year)
}

/// `fmtEvent` : sans heure si le point n'en a pas.
fn fmt_event(tz: Tz, e: &TrackerEvent) -> String {
    if e.date_only {
        fmt_day(tz, e.ts)
    } else {
        fmt_ts(tz, e.ts)
    }
}

/// `whenParts` : 0 rien, 1 année, 2 mois, 3 jour, 4 heure.
fn when_parts(when: &str) -> usize {
    let w = go_trim(when);
    if w.is_empty() {
        return 0;
    }
    let w = w.replacen('T', " ", 1);
    let mut date = w.as_str();
    if let Some(sp) = w.find(' ') {
        date = &w[..sp];
        if !go_trim(&w[sp..]).is_empty() {
            return 4;
        }
    }
    date.split('-').count()
}

/// `toolTracker` : le texte vu par le modèle.
async fn tracker(env: &MemEnv<'_>, proj: &str, args: &Map<String, Value>) -> String {
    let action = go_trim(&go_str(args.get("action"))).to_lowercase();
    let name = go_trim(&go_str(args.get("name"))).to_string();
    let when = go_trim(&go_str(args.get("when"))).to_string();
    let text = go_trim(&go_str(args.get("text"))).to_string();
    let id = go_trim(&go_str(args.get("id"))).to_string();
    let store = env.store;
    match action.as_str() {
        "" | "view" | "read" | "list" => tracker_view(env, proj, &name, &when).await,
        "add" => match env
            .mem
            .tracker_add_at(store, proj, &name, &when, &text, env.now_ms)
            .await
        {
            Ok(id) => format!("[ok] point ajouté à « {name} » (id {id})."),
            Err(e) => format!("[erreur] {e}"),
        },
        "edit" => {
            if name.is_empty() || id.is_empty() {
                return "[erreur] name et id sont requis pour modifier un point".into();
            }
            let slug = slugify_ident(&name);
            let res = async {
                let mut t = Memory::tracker_load(store, proj, &slug)
                    .await
                    .ok_or("tracker introuvable")?;
                let ev = t.events.get_or_insert_with(Vec::new);
                let Some(e) = ev.iter_mut().find(|e| e.id == id) else {
                    return Err("événement introuvable".to_string());
                };
                if !text.is_empty() {
                    e.text = text.clone();
                }
                if !when.is_empty() {
                    let (ts, date_only) = parse_when(&when, env.now_ms, env.mem.tz)?;
                    e.ts = ts;
                    e.date_only = date_only;
                }
                Memory::tracker_save(store, proj, &slug, &mut t).await
            }
            .await;
            match res {
                Ok(()) => format!("[ok] point {id} de « {name} » modifié."),
                Err(e) => format!("[erreur] {e}"),
            }
        }
        "delete" => {
            if name.is_empty() {
                return "[erreur] name requis".into();
            }
            // Sans id, c'est le tracker entier : réservé à l'interface.
            if id.is_empty() {
                return "[erreur] id requis — supprimer un tracker entier se fait depuis l'interface, pas par l'outil".into();
            }
            let slug = slugify_ident(&name);
            let res = async {
                let mut t = Memory::tracker_load(store, proj, &slug)
                    .await
                    .ok_or("tracker introuvable")?;
                let ev = t.events.get_or_insert_with(Vec::new);
                let n = ev.len();
                ev.retain(|e| e.id != id);
                if ev.len() == n {
                    return Err("événement introuvable".to_string());
                }
                Memory::tracker_save(store, proj, &slug, &mut t).await
            }
            .await;
            match res {
                Ok(()) => format!("[ok] point {id} de « {name} » supprimé."),
                Err(e) => format!("[erreur] {e}"),
            }
        }
        a => format!(
            "[erreur] action inconnue {} (view, add, edit, delete)",
            crate::gojson::quote(a)
        ),
    }
}

/// `trackerView` : vue d'ensemble, ou descente année → mois → points.
async fn tracker_view(env: &MemEnv<'_>, proj: &str, name: &str, when: &str) -> String {
    let tz = env.mem.tz;
    if go_trim(name).is_empty() {
        return tracker_overview(env, proj).await;
    }
    let slug = slugify_ident(name);
    let Some(t) = Memory::tracker_load(env.store, proj, &slug).await else {
        return format!(
            "[aucun tracker « {name} »] Appelle tracker() sans argument pour la liste, ou ajoute un point avec action:\"add\"."
        );
    };
    let ev = t.events.clone().unwrap_or_default();
    if ev.is_empty() {
        return format!("Tracker « {} » : vide pour l'instant.", t.name);
    }
    let prefix = go_trim(when).replacen('T', " ", 1);
    let matched: Vec<TrackerEvent> = ev
        .iter()
        .filter(|e| prefix.is_empty() || fmt_ts(tz, e.ts).starts_with(&prefix))
        .cloned()
        .collect();
    match when_parts(when) {
        0 => tracker_skeleton(tz, &t, &ev, env.now_ms),
        1 => {
            if matched.is_empty() {
                return format!("Tracker « {} » : rien en {when}.", t.name);
            }
            let example = format!("when:\"{when}-07\"");
            format!(
                "Tracker « {} » en {when} — {} points :\n{}\nZoom : {example} pour un mois.",
                t.name,
                matched.len(),
                group_line(&matched, |e| fmt_month(tz, e.ts))
            )
        }
        _ => tracker_events(tz, &t.name, when, &matched),
    }
}

/// `trackerOverview` : la liste des trackers du projet.
async fn tracker_overview(env: &MemEnv<'_>, proj: &str) -> String {
    let tz = env.mem.tz;
    let list = Memory::tracker_list(env.store, proj).await;
    if list.is_empty() {
        return "Aucun tracker dans ce projet. Crée-en un en ajoutant un point : action:\"add\", name:\"…\", text:\"…\" (when facultatif = maintenant).".into();
    }
    let mut b = format!("Trackers du projet ({}) :\n", list.len());
    for m in &list {
        let count = m["count"].as_i64().unwrap_or(0);
        let span = if count > 0 {
            format!(
                " — {count} points, du {} au {}",
                fmt_day(tz, m["first_ts"].as_i64().unwrap_or(0)),
                fmt_day(tz, m["last_ts"].as_i64().unwrap_or(0))
            )
        } else {
            String::new()
        };
        b.push_str(&format!("- {}{span}\n", m["name"].as_str().unwrap_or("")));
    }
    b.push_str("Zoom : tracker(name:\"<nom>\") pour le détail d'un tracker.");
    b
}

/// Des points regroupés par une clé de date, triés, avec leurs comptes :
/// « 2026-07 (3) · … » (`monthsLine`, `trackerGroup`).
fn group_line(ev: &[TrackerEvent], key: impl Fn(&TrackerEvent) -> String) -> String {
    let mut groups: std::collections::BTreeMap<String, usize> = Default::default();
    for e in ev {
        *groups.entry(key(e)).or_default() += 1;
    }
    groups
        .into_iter()
        .map(|(g, n)| format!("{g} ({n})"))
        .collect::<Vec<_>>()
        .join(" · ")
}

/// `trackerSkeleton` : les années (avec compte), et l'année en cours par
/// mois.
fn tracker_skeleton(tz: Tz, t: &Tracker, ev: &[TrackerEvent], now_ms: i64) -> String {
    let mut b = format!(
        "Tracker « {} » — {} points, du {} au {}\n",
        t.name,
        ev.len(),
        fmt_day(tz, ev[0].ts),
        fmt_day(tz, ev[ev.len() - 1].ts)
    );
    b.push_str(&format!(
        "Années : {}\n",
        group_line(ev, |e| fmt_year(tz, e.ts))
    ));
    let cur = fmt_year(tz, now_ms);
    let cur_ev: Vec<TrackerEvent> = ev
        .iter()
        .filter(|e| fmt_ts(tz, e.ts).starts_with(&cur))
        .cloned()
        .collect();
    if !cur_ev.is_empty() {
        b.push_str(&format!(
            "{cur} (année en cours) : {}\n",
            group_line(&cur_ev, |e| fmt_month(tz, e.ts))
        ));
    }
    b.push_str(&format!(
        "Zoom : tracker(name:\"{}\", when:\"2025\") pour une année · when:\"{cur}-07\" pour un mois.",
        t.name
    ));
    b
}

/// `trackerEventsCap`.
const EVENTS_CAP: usize = 60;

/// `trackerEvents` : les points (id, date, texte), les plus récents d'abord
/// gardés au-delà du plafond.
fn tracker_events(tz: Tz, name: &str, when: &str, ev: &[TrackerEvent]) -> String {
    let trimmed = ev.len() > EVENTS_CAP;
    let shown = if trimmed {
        &ev[ev.len() - EVENTS_CAP..]
    } else {
        ev
    };
    let mut b = format!("Tracker « {name} »");
    if !go_trim(when).is_empty() {
        b.push_str(&format!(" en {when}"));
    }
    if trimmed {
        b.push_str(&format!(
            " — {} points (les {EVENTS_CAP} plus récents ; affine le when pour les plus anciens) :\n",
            ev.len()
        ));
    } else {
        b.push_str(&format!(" — {} points :\n", ev.len()));
    }
    for e in shown {
        b.push_str(&format!("- [{}] {} — {}\n", e.id, fmt_event(tz, e), e.text));
    }
    b.push_str(
        "Modifier/supprimer : action:\"edit\"|\"delete\", name, id (l'identifiant entre crochets).",
    );
    b
}
