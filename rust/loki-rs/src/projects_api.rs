//! Hub des projets tenu par la façade (lot 18 de la migration,
//! `LOKI_PROJECTS=rust` ; expérimental, le Go reste le défaut) — port de
//! `handleProjects` / `projectDTOs` (`web_projects.go`) et des mutations de
//! `projects.go` : création (slug translittéré, unique), renommage,
//! description, suppression (discussions, trackers et mémoire du projet, actif
//! réassigné, dernier projet indestructible) et bascule.
//!
//! Le registre reste dans la base (`state/projects`), la mémoire de chaque
//! projet dans `memory/<slug>`. Le projet actif passe toujours par le Go
//! (`/internal/v1/project/activate`), qui en garde une copie en RAM. Les
//! discussions sont à qui tient la conversation : la façade les bascule et
//! les supprime elle-même quand elle la tient, sinon le Go
//! (`/internal/v1/project/switch`, `/internal/v1/project/drop-convs`).

use std::path::Path;

use http_body_util::{BodyExt, Limited};
use hyper::body::Incoming;
use hyper::{Method, Request, Response, StatusCode};
use serde_json::{Value, json};

use crate::api::Ctx;
use crate::auth::json as send_json;
use crate::chat::persist::{BK_CHAT, CK_INDEX, decode_index};
use crate::gojson::{self, Kind};
use crate::memory::{self, BK_TRACKER, Project, ST_PROJECTS, active_project, list_projects};
use crate::paths::DEFAULT_PROJECT_SLUG;
use crate::proxy::Body;
use crate::store::{BK_STATE, Store};

/// Taille maximale d'un nom de projet, en runes.
pub const MAX_NAME: usize = 60;
/// Taille maximale d'une description, en runes.
pub const MAX_DESC: usize = 2000;

/// Une erreur de route : (code, message).
type Fail = (StatusCode, String);

fn bad(e: impl Into<String>) -> Fail {
    (StatusCode::BAD_REQUEST, e.into())
}

/// Les `n` premières runes.
fn runes(s: &str, n: usize) -> String {
    s.chars().take(n).collect()
}

/// `saveProjects` (`json.Marshal`, HTML échappé comme le Go).
async fn save_projects(store: &Store, list: &[Project]) -> Result<(), String> {
    store
        .put(BK_STATE, ST_PROJECTS, gojson::marshal(list).into_bytes())
        .await
}

/// `projectMemoryDir`.
pub fn project_memory_dir(home: &Path, slug: &str) -> std::path::PathBuf {
    let slug = if slug.is_empty() {
        DEFAULT_PROJECT_SLUG
    } else {
        slug
    };
    home.join("memory").join(slug)
}

/// Pages d'un projet (`MemList` sans l'index) : fichiers `.md` visibles.
pub fn count_pages(dir: &Path) -> usize {
    let Ok(rd) = std::fs::read_dir(dir) else {
        return 0;
    };
    rd.filter_map(|e| e.ok())
        .filter(|e| !e.file_type().is_ok_and(|t| t.is_dir()))
        .map(|e| e.file_name().to_string_lossy().into_owned())
        .filter(|n| {
            !n.starts_with('.') && n.to_lowercase().ends_with(".md") && !memory::is_index_file(n)
        })
        .count()
}

/// `slugifyIdent` puis le repli « projet » et le suffixe -2, -3… d'un slug
/// déjà pris.
pub fn unique_slug(name: &str, list: &[Project]) -> String {
    let mut base = memory::slugify_ident(name);
    if base.is_empty() {
        base = "projet".into();
    }
    let taken = |s: &str| list.iter().any(|p| p.slug == s);
    let mut slug = base.clone();
    let mut i = 2;
    while taken(&slug) {
        slug = format!("{base}-{i}");
        i += 1;
    }
    slug
}

/// `createProject`.
async fn create_project(store: &Store, home: &Path, name: &str) -> Result<Project, String> {
    let name = name.trim();
    if name.is_empty() {
        return Err("nom vide".into());
    }
    let name = runes(name, MAX_NAME);
    let mut list = list_projects(store).await;
    let slug = unique_slug(&name, &list);
    let dir = project_memory_dir(home, &slug);
    std::fs::create_dir_all(&dir).map_err(|e| e.to_string())?;
    let idx = dir.join(memory::MEM_INDEX_FILE);
    if !idx.exists() {
        let _ = write_private(&idx, memory::memory_seed(&name).as_bytes());
    }
    let p = Project {
        slug,
        name,
        created_at: memory::now_ms(),
        desc: String::new(),
        mem_mode: String::new(),
    };
    list.push(p.clone());
    save_projects(store, &list).await?;
    Ok(p)
}

/// Fichier en 0600, comme `os.WriteFile(…, 0o600)`.
fn write_private(p: &Path, b: &[u8]) -> std::io::Result<()> {
    use std::io::Write;
    let mut o = std::fs::OpenOptions::new();
    o.write(true).create(true).truncate(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        o.mode(0o600);
    }
    o.open(p)?.write_all(b)
}

/// `renameProject`.
async fn rename_project(store: &Store, slug: &str, name: &str) -> Result<(), String> {
    let name = name.trim();
    if name.is_empty() {
        return Err("nom vide".into());
    }
    let name = runes(name, MAX_NAME);
    let mut list = list_projects(store).await;
    let Some(p) = list.iter_mut().find(|p| p.slug == slug) else {
        return Err("projet introuvable".into());
    };
    p.name = name;
    save_projects(store, &list).await
}

/// `setProjectDesc` : tronquée à 2 000 runes (contexte injecté à chaque
/// conversation, pas un cahier des charges) ; vide = effacée.
async fn set_project_desc(store: &Store, slug: &str, desc: &str) -> Result<(), String> {
    let mut desc = desc.trim().to_string();
    if desc.chars().count() > MAX_DESC {
        desc = runes(&desc, MAX_DESC).trim().to_string();
    }
    let mut list = list_projects(store).await;
    let Some(p) = list.iter_mut().find(|p| p.slug == slug) else {
        return Err("projet introuvable".into());
    };
    p.desc = desc;
    save_projects(store, &list).await
}

/// Le projet actif, par le Go (qui en garde une copie en RAM).
async fn activate(store: &Store, slug: &str) -> Result<(), Fail> {
    let (code, v) = store
        .internal_post("/internal/v1/project/activate", &json!({ "slug": slug }))
        .await
        .map_err(bad)?;
    if code.is_success() {
        return Ok(());
    }
    Err(go_fail(code, &v))
}

/// Une réponse d'erreur du Go : 409 gardé, le reste en 400.
fn go_fail(code: StatusCode, v: &Value) -> Fail {
    let msg = v
        .get("error")
        .and_then(Value::as_str)
        .map(str::to_string)
        .unwrap_or_else(|| code.to_string());
    let code = if code == StatusCode::CONFLICT {
        StatusCode::CONFLICT
    } else {
        StatusCode::BAD_REQUEST
    };
    (code, msg)
}

fn refused(r: crate::chat::generate::Refused) -> Fail {
    let code = if r.status == 409 {
        StatusCode::CONFLICT
    } else {
        StatusCode::BAD_REQUEST
    };
    (code, r.error)
}

/// Bascule de la conversation sur un projet : la façade si elle la tient,
/// sinon le Go.
async fn switch_conversation(ctx: &Ctx, slug: &str) -> Result<(), Fail> {
    if let Some(chat) = &ctx.chat {
        return chat.project_switch(slug).await.map_err(refused);
    }
    let (code, v) = ctx
        .store
        .internal_post("/internal/v1/project/switch", &json!({ "slug": slug }))
        .await
        .map_err(bad)?;
    if code.is_success() {
        Ok(())
    } else {
        Err(go_fail(code, &v))
    }
}

/// `deleteProject`.
async fn delete_project(ctx: &Ctx, slug: &str) -> Result<(), Fail> {
    let store = &ctx.store;
    let list = list_projects(store).await;
    if list.len() <= 1 {
        return Err(bad("impossible de supprimer le dernier projet"));
    }
    let kept: Vec<Project> = list.iter().filter(|p| p.slug != slug).cloned().collect();
    if kept.len() == list.len() {
        return Err(bad("projet introuvable"));
    }
    // Discussions du projet. Quand la façade tient la conversation, elle
    // quitte d'abord le projet s'il est actif (sa discussion enregistrée, une
    // du projet restant ouverte), puis supprime les autres ; sinon le Go.
    if let Some(chat) = &ctx.chat {
        if active_project(store).await == slug {
            chat.project_switch(&kept[0].slug).await.map_err(refused)?;
        }
        chat.drop_project(slug).await.map_err(refused)?;
    } else {
        let (code, v) = store
            .internal_post("/internal/v1/project/drop-convs", &json!({ "slug": slug }))
            .await
            .map_err(bad)?;
        if !code.is_success() {
            return Err(go_fail(code, &v));
        }
    }
    // Trackers du projet.
    let prefix = format!("{slug}/");
    if let Ok(all) = store.all(BK_TRACKER).await {
        for k in all.keys().filter(|k| k.starts_with(&prefix)) {
            let _ = store.del(BK_TRACKER, k).await;
        }
    }
    // Dossier disque du projet.
    let _ = std::fs::remove_dir_all(project_memory_dir(&ctx.engine.home, slug));
    save_projects(store, &kept).await.map_err(bad)?;
    if active_project(store).await == slug {
        let _ = activate(store, &kept[0].slug).await;
    }
    Ok(())
}

/// `projectDTOs` : le registre et ses comptes, plus le projet actif.
async fn listing(ctx: &Ctx) -> Value {
    let active = active_project(&ctx.store).await;
    let projects = project_dtos(ctx).await;
    json!({ "ok": true, "projects": projects, "active": active })
}

/// `projectDTOs` : les projets avec leurs compteurs (discussions, pages,
/// trackers) et le drapeau du projet actif.
pub(crate) async fn project_dtos(ctx: &Ctx) -> Vec<Value> {
    let store = &ctx.store;
    let active = active_project(store).await;
    let mut convs: std::collections::HashMap<String, usize> = std::collections::HashMap::new();
    if let Ok(Some(b)) = store.get_open(BK_CHAT, CK_INDEX).await {
        for m in decode_index(&b) {
            let p = match m.project.trim() {
                "" => DEFAULT_PROJECT_SLUG.to_string(),
                s => s.to_string(),
            };
            *convs.entry(p).or_default() += 1;
        }
    }
    let trackers: Vec<String> = store
        .all(BK_TRACKER)
        .await
        .map(|m| m.into_keys().collect())
        .unwrap_or_default();
    let home = ctx.engine.home.clone();
    let list = list_projects(store).await;
    let pages: Vec<usize> = {
        let dirs: Vec<_> = list
            .iter()
            .map(|p| project_memory_dir(&home, &p.slug))
            .collect();
        tokio::task::spawn_blocking(move || dirs.iter().map(|d| count_pages(d)).collect())
            .await
            .unwrap_or_default()
    };
    let projects: Vec<Value> = list
        .iter()
        .enumerate()
        .map(|(i, p)| {
            let prefix = format!("{}/", p.slug);
            let mut d = json!({
                "slug": p.slug,
                "name": p.name,
                "created": p.created_at,
                "convs": convs.get(&p.slug).copied().unwrap_or(0),
                "pages": pages.get(i).copied().unwrap_or(0),
                "trackers": trackers.iter().filter(|k| k.starts_with(&prefix)).count(),
                "active": p.slug == active,
            });
            if !p.desc.is_empty() {
                d["desc"] = json!(p.desc);
            }
            d
        })
        .collect();
    projects
}

/// Les routes servies par ce module.
pub fn handles(path: &str) -> bool {
    path == "/api/projects"
}

const PROJECT_REQ_TYPE: &str = r#"struct { Action string "json:\"action\""; Slug string "json:\"slug\""; Name string "json:\"name\""; Desc string "json:\"desc\"" }"#;

/// `from_go` : remise par le Go (tunnel), déjà authentifiée par lui.
pub async fn serve(ctx: &Ctx, req: Request<Incoming>, from_go: bool) -> Response<Body> {
    let (parts, body) = req.into_parts();
    if !from_go && let Err(refus) = ctx.guard(&parts.headers).await {
        return *refus;
    }
    match parts.method {
        Method::GET => send_json(StatusCode::OK, &listing(ctx).await),
        Method::POST => {
            let raw = Limited::new(body, 1 << 20)
                .collect()
                .await
                .map(|b| b.to_bytes().to_vec())
                .unwrap_or_default();
            let d = gojson::decode_struct(
                &raw,
                PROJECT_REQ_TYPE,
                &[
                    ("action", Kind::Str),
                    ("slug", Kind::Str),
                    ("name", Kind::Str),
                    ("desc", Kind::Str),
                ],
            );
            if let Some(e) = d.err.clone() {
                return send_json(StatusCode::BAD_REQUEST, &json!({ "ok": false, "error": e }));
            }
            let slug = d.str("slug").trim().to_string();
            let store = &ctx.store;
            let home = ctx.engine.home.clone();
            let action = d.str("action");
            let res: Result<(), Fail> = match action.trim().to_lowercase().as_str() {
                "create" => create_project(store, &home, &d.str("name"))
                    .await
                    .map(|_| ())
                    .map_err(bad),
                "rename" => rename_project(store, &slug, &d.str("name"))
                    .await
                    .map_err(bad),
                "desc" => set_project_desc(store, &slug, &d.str("desc"))
                    .await
                    .map_err(bad),
                "delete" => {
                    // Une génération travaille dans la mémoire du projet actif :
                    // supprimer sous ses pieds la laisserait écrire dans le vide.
                    if crate::enginectl::generating(ctx).await {
                        Err((StatusCode::CONFLICT, "une génération est en cours".into()))
                    } else {
                        delete_project(ctx, &slug).await
                    }
                }
                "switch" => {
                    if crate::enginectl::generating(ctx).await {
                        Err((StatusCode::CONFLICT, "une génération est en cours".into()))
                    } else if ctx.chat.is_some()
                        && !list_projects(store).await.iter().any(|p| p.slug == slug)
                    {
                        Err(bad("projet introuvable"))
                    } else {
                        switch_conversation(ctx, &slug).await
                    }
                }
                _ => Err(bad(format!("action inconnue : {}", gojson::quote(&action)))),
            };
            match res {
                Ok(()) => send_json(StatusCode::OK, &listing(ctx).await),
                Err((code, e)) => send_json(code, &json!({ "ok": false, "error": e })),
            }
        }
        _ => send_json(
            StatusCode::METHOD_NOT_ALLOWED,
            &json!({ "ok": false, "error": "méthode non autorisée" }),
        ),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn p(slug: &str) -> Project {
        Project {
            slug: slug.into(),
            name: slug.into(),
            created_at: 0,
            desc: String::new(),
            mem_mode: String::new(),
        }
    }

    #[test]
    fn slugs_uniques() {
        let list = vec![p("alpha"), p("alpha-2"), p("projet")];
        assert_eq!(unique_slug("Alpha", &list), "alpha-3");
        assert_eq!(unique_slug("!!!", &list), "projet-2");
        assert_eq!(unique_slug("Bêta", &list), "beta");
    }
}
