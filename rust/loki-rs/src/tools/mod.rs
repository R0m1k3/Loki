//! Outils de l'agent exécutés par la façade (lot 7 de la migration).
//!
//! Quand la façade tient la conversation (`LOKI_CHAT=rust`), les outils
//! fichiers, le shell et git s'exécutent ici : `read`, `grep`, `glob`,
//! `write`, `edit`, `bash`, `ask`, `git_status`, `git_diff`, `git_clone`. Les
//! autres (mémoire, web, tâches, navigateur, MCP,
//! sous-agents, critères…) restent au Go (`/internal/v1/tool/exec`), de même
//! que `write`/`edit`/`bash` quand l'agent vise un poste distant.
//!
//! Ce que ces outils partagent avec le reste de Loki reste au Go, en un seul
//! exemplaire ([`ToolState`]) : le dossier de travail de la discussion, le
//! dossier mémoire, la cible distante, le suivi « lu avant d'écrire » (aussi
//! tenu par les sous-agents et les tâches, qui tournent là-bas) et les
//! diagnostics LSP. Les garde-fous sont ceux du Go, dans le même ordre.

pub mod diff;
pub mod files;
pub mod git;
pub mod policy;
pub mod shell;

use std::future::Future;
use std::path::{Path, PathBuf};

use serde_json::{Map, Value, json};

use crate::chat::turn::ToolOutcome;
use crate::chat::types::AskEvent;
use crate::store::Store;

/// Ce que les outils doivent savoir avant de s'exécuter.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct ToolCtx {
    /// Dossier de travail (`agentCwd`).
    pub cwd: PathBuf,
    /// Dossier interdit au shell et à write/edit (`memoryDir`).
    pub memory_dir: PathBuf,
    /// Poste distant visé ("" : cette machine).
    pub target: String,
    /// Mode Code : chemins bornés au dossier, lu avant d'écrire.
    pub code: bool,
}

/// L'état partagé avec le Go.
pub trait ToolState: Send + Sync {
    /// Le contexte du moment ; `Err` : outils locaux indisponibles.
    fn context(&self) -> impl Future<Output = Result<ToolCtx, String>> + Send;
    /// `trackerNoteRead`.
    fn note_read(&self, path: &Path) -> impl Future<Output = ()> + Send;
    /// `trackerCheck` : le refus, ou `None`.
    fn check(&self, path: &Path, for_write: bool) -> impl Future<Output = Option<String>> + Send;
    /// Après une écriture réussie : `trackerNoteWrite` puis le bloc LSP.
    fn wrote(&self, path: &Path) -> impl Future<Output = String> + Send;
}

/// Outils exécutés ici (hors cible distante pour write/edit/bash).
pub fn is_local(name: &str) -> bool {
    matches!(
        name,
        "read"
            | "grep"
            | "glob"
            | "write"
            | "edit"
            | "bash"
            | "ask"
            | "git_status"
            | "git_diff"
            | "git_clone"
    )
}

fn outcome(result: String) -> ToolOutcome {
    ToolOutcome {
        result,
        ..Default::default()
    }
}

async fn blocking<T: Send + 'static>(f: impl FnOnce() -> T + Send + 'static) -> Result<T, String> {
    tokio::task::spawn_blocking(f)
        .await
        .map_err(|e| format!("[erreur] outil interrompu : {e}"))
}

/// `codeWriteGuard` : en mode Code, borne de chemin puis « lu avant
/// d'écrire ». `None` : l'écriture peut avoir lieu.
async fn code_write_guard<S: ToolState>(
    st: &S,
    ctx: &ToolCtx,
    file: &str,
    for_write: bool,
) -> Option<String> {
    if !ctx.code {
        return None;
    }
    let path = policy::resolve_agent_path(&ctx.cwd, file);
    if !policy::code_path_allowed(&ctx.cwd, &path) {
        return Some(policy::refused_path_result(file));
    }
    st.check(&path, for_write).await
}

/// Exécute un outil ici ; `None` : c'est au Go de le faire (outil non porté,
/// cible distante, contexte indisponible).
pub async fn exec_local<S: ToolState>(
    st: &S,
    code: bool,
    name: &str,
    args: &Map<String, Value>,
    label: &str,
) -> Option<ToolOutcome> {
    if !is_local(name) {
        return None;
    }
    let mut ctx = match st.context().await {
        Ok(c) => c,
        Err(e) => {
            eprintln!("[outils] contexte indisponible, exécution par le Go : {e}");
            return None;
        }
    };
    ctx.code = code;
    let remote = !ctx.target.is_empty();
    let out = match name {
        "read" => {
            let (cwd, a) = (ctx.cwd.clone(), args.clone());
            let r = match blocking(move || files::tool_read(&cwd, &a, code)).await {
                Ok(r) => r,
                Err(e) => return Some(outcome(e)),
            };
            if let Some(p) = &r.noted {
                st.note_read(p).await;
            }
            outcome(r.result)
        }
        "grep" => {
            let (cwd, a) = (ctx.cwd.clone(), args.clone());
            outcome(
                blocking(move || files::tool_grep(&cwd, &a, code))
                    .await
                    .unwrap_or_else(|e| e),
            )
        }
        "glob" => {
            let (cwd, a) = (ctx.cwd.clone(), args.clone());
            outcome(
                blocking(move || files::tool_glob(&cwd, &a))
                    .await
                    .unwrap_or_else(|e| e),
            )
        }
        // git tourne toujours sur cette machine, cible distante ou non (comme
        // le Go).
        "git_status" => outcome(git::tool_git_status(&ctx.cwd, args).await),
        "git_diff" => outcome(git::tool_git_diff(&ctx.cwd, args).await),
        "git_clone" => outcome(git::tool_git_clone(&ctx.cwd, args).await),
        "ask" => {
            let result = files::tool_ask(args);
            let ask = (!result.starts_with("[erreur]")).then(|| AskEvent {
                question: files::arg_str(args, "question").to_string(),
                options: files::ask_options(args),
            });
            ToolOutcome {
                result,
                ask,
                ..Default::default()
            }
        }
        "write" if !remote => {
            let content = files::arg_str(args, "content").to_string();
            if let Some(msg) = code_write_guard(st, &ctx, label, true).await {
                return Some(outcome(msg));
            }
            let (cwd, mem, l, c) = (
                ctx.cwd.clone(),
                ctx.memory_dir.clone(),
                label.to_string(),
                content.clone(),
            );
            let mut result = match blocking(move || files::file_write(&cwd, &mem, &l, &c)).await {
                Ok(r) => r,
                Err(e) => return Some(outcome(e)),
            };
            let mut o = ToolOutcome::default();
            if !result.starts_with("[erreur]") {
                (o.diff, o.added) = diff::added_diff(&content);
                result += &st.wrote(&policy::resolve_agent_path(&ctx.cwd, label)).await;
            }
            o.result = result;
            o
        }
        "edit" if !remote => {
            let (old, new) = (
                files::arg_str(args, "old").to_string(),
                files::arg_str(args, "new").to_string(),
            );
            if let Some(msg) = code_write_guard(st, &ctx, label, false).await {
                return Some(outcome(msg));
            }
            let (cwd, mem, l, o2, n2) = (
                ctx.cwd.clone(),
                ctx.memory_dir.clone(),
                label.to_string(),
                old.clone(),
                new.clone(),
            );
            let mut result =
                match blocking(move || files::file_edit(&cwd, &mem, &l, &o2, &n2)).await {
                    Ok(r) => r,
                    Err(e) => return Some(outcome(e)),
                };
            let mut o = ToolOutcome::default();
            if !result.starts_with("[erreur]") {
                (o.diff, o.added, o.removed) = diff::line_diff(&old, &new);
                result += &st.wrote(&policy::resolve_agent_path(&ctx.cwd, label)).await;
            }
            o.result = result;
            o
        }
        "bash" if !remote || policy::dangerous_command(label).is_some() => {
            let timeout = files::arg_int(args, "timeout").unwrap_or(0);
            // Refusées AVANT toute exécution, cible distante comprise.
            if let Some(reason) = policy::dangerous_command(label) {
                return Some(outcome(policy::refused_command_result(reason)));
            }
            // Mode Code : « cmd & » laisse un orphelin sans sortie ni arrêt.
            if code && shell::trailing_background(label) {
                return Some(outcome("[refusé] commande terminée par « & » : lance-la avec bash_bg (sortie consultable avec bash_tail), pas en arrière-plan dans bash.".into()));
            }
            outcome(shell::run_shell(&ctx.cwd, &ctx.memory_dir, label, timeout).await)
        }
        _ => return None, // cible distante : le Go passe par le poste
    };
    Some(out)
}

/// [`ToolState`] servi par l'API interne du Go.
#[derive(Clone)]
pub struct GoToolState {
    pub store: Store,
    /// Capacités du tour (pour les diagnostics LSP).
    pub caps: Value,
}

impl ToolState for GoToolState {
    async fn context(&self) -> Result<ToolCtx, String> {
        let (status, v) = self
            .store
            .internal_post("/internal/v1/tool/context", &json!({}))
            .await?;
        if !status.is_success() {
            return Err(format!("contexte des outils : {status}"));
        }
        let s = |k: &str| v.get(k).and_then(Value::as_str).unwrap_or("").to_string();
        Ok(ToolCtx {
            cwd: PathBuf::from(s("cwd")),
            memory_dir: PathBuf::from(s("memory_dir")),
            target: s("target"),
            code: false,
        })
    }

    async fn note_read(&self, path: &Path) {
        let body = json!({ "op": "read", "path": path });
        if let Err(e) = self
            .store
            .internal_post("/internal/v1/tool/tracker", &body)
            .await
        {
            eprintln!("[outils] lecture non notée : {e}");
        }
    }

    async fn check(&self, path: &Path, for_write: bool) -> Option<String> {
        let body = json!({ "op": "check", "path": path, "for_write": for_write });
        match self
            .store
            .internal_post("/internal/v1/tool/tracker", &body)
            .await
        {
            Ok((st, v)) if st.is_success() => v
                .get("refusal")
                .and_then(Value::as_str)
                .filter(|r| !r.is_empty())
                .map(str::to_string),
            // Sans le suivi, écrire serait écrire à l'aveugle : refus.
            Ok((st, _)) => Some(format!("[erreur] suivi des lectures indisponible : {st}")),
            Err(e) => Some(format!("[erreur] suivi des lectures indisponible : {e}")),
        }
    }

    async fn wrote(&self, path: &Path) -> String {
        let body = json!({ "path": path, "caps": self.caps });
        match self
            .store
            .internal_post("/internal/v1/tool/wrote", &body)
            .await
        {
            Ok((st, v)) if st.is_success() => v
                .get("lsp")
                .and_then(Value::as_str)
                .unwrap_or("")
                .to_string(),
            Ok(_) | Err(_) => String::new(),
        }
    }
}
