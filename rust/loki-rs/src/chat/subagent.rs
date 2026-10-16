//! L'outil `subagent` du mode Code (lot 47) — port de `toolSubagent`
//! (`code_subagent.go`).
//!
//! Le builder délègue une question bornée à un rôle en lecture seule
//! (explorer, code-reviewer, planner) qui travaille dans SON PROPRE contexte :
//! le prompt du rôle, le briefing machine et la tâche, rien de l'historique.
//! Sa trace n'est ni rangée ni affichée : seul son texte final revient, comme
//! résultat d'outil. Le contexte du rôle (prompt système, outils, capacités)
//! vient du Go (`/internal/v1/turn/subagent`) ; le tour imbriqué tourne ici,
//! comme la passe de vérification : un travail annexe (`engineSideJob`).
//!
//! Le tour du rôle est porté par [`SubHost`], qui délègue tout à son
//! [`GoTurnHost`] sauf un nouvel appel à `subagent` : un sous-agent ne
//! délègue pas à son tour (et le type du tour imbriqué ne se contient pas
//! lui-même).

use std::sync::Arc;

use serde_json::{Map, Value, json};

use super::compact::CompactConfig;
use super::host::{GoTurnHost, TurnCompact};
use super::stream::{CacheHintFn, PreflightFn, RetryPatterns};
use super::turn::{
    Cancel, CompactPhase, PerfMarks, PerfRec, Reply, SendError, ToolOutcome, TurnConfig, TurnHost,
    run_turn,
};
use super::types::{Message, StreamEvent, Tool};
use super::wire::{ChatEndpoint, EchoPolicy, TurnReasoning};

/// `subagentRoles` : les rôles délégables (`verifier` n'en est pas : seule
/// la boucle du contrat le lance).
pub const ROLES: [&str; 3] = ["explorer", "code-reviewer", "planner"];
/// `subagentTemperature` (le TEMP du preset, s'il est posé, l'emporte).
pub const TEMPERATURE: f64 = 0.6;
/// `subagentMaxOutput` : un rapport, pas un vidage de fichiers.
pub const MAX_OUTPUT: usize = 6000;

/// Ce que le tour du builder prête à ses sous-agents : l'endpoint, le
/// raisonnement, l'écho, les motifs de relance et l'arrêt du tour.
#[derive(Clone)]
pub struct SubTurn {
    pub ep: ChatEndpoint,
    pub reasoning: TurnReasoning,
    pub echo: EchoPolicy,
    pub patterns: Arc<RetryPatterns>,
    pub cancel: Cancel,
}

/// `subagentFraming` : le cadrage ajouté à la tâche — l'interlocuteur du rôle
/// est le builder, pas un humain.
pub fn framing(role: &str) -> &'static str {
    if role == "planner" {
        "\n\nYou are a delegated pass: nobody will answer you. Record the acceptance criteria with the criteria tool, then give the ordered steps. Do not ask for confirmation."
    } else {
        "\n\nAnswer with your findings only: what you found and where (file:line). No preamble."
    }
}

/// Les vérifications de `toolSubagent` avant tout appel au moteur : rôle,
/// tâche, puis le garde-fou anti-récursion (`parent_role` : le rôle du tour
/// qui délègue, vide ou `builder` pour le fil principal).
pub fn precheck(args: &Map<String, Value>, parent_role: &str) -> Result<(String, String), String> {
    let s = |k: &str| {
        args.get(k)
            .and_then(Value::as_str)
            .unwrap_or("")
            .trim()
            .to_string()
    };
    let (role, task) = (s("role"), s("task"));
    if !ROLES.contains(&role.as_str()) {
        return Err(format!(
            "[erreur] rôle inconnu : {role} (attendu : {})",
            ROLES.join(", ")
        ));
    }
    if task.is_empty() {
        return Err("[erreur] tâche vide — décris ce que le rôle doit chercher ou relire".into());
    }
    if !parent_role.is_empty() && parent_role != "builder" {
        return Err(
            "[erreur] un sous-agent ne peut pas en appeler un autre — réponds avec ce que tu as"
                .into(),
        );
    }
    Ok((role, task))
}

/// Le rapport rendu au builder : texte écrit après le dernier outil, borné.
pub fn report(role: &str, content: &str, err: Option<&str>) -> String {
    if let Some(e) = err {
        return format!("[erreur] sous-agent {role} : {e}");
    }
    let res = content.trim();
    if res.is_empty() {
        return format!("[erreur] le sous-agent {role} n'a rien répondu");
    }
    let res = if res.chars().count() > MAX_OUTPUT {
        format!(
            "{}\n…[tronqué]",
            res.chars().take(MAX_OUTPUT).collect::<String>()
        )
    } else {
        res.to_string()
    };
    format!("[{role}]\n{res}")
}

fn caps_role(caps: &Value) -> &str {
    caps.get("Role").and_then(Value::as_str).unwrap_or("")
}

/// `toolSubagent` : délègue au rôle et rend son rapport.
pub async fn run(parent: &GoTurnHost, sub: &SubTurn, args: &Map<String, Value>) -> String {
    let (role, task) = match precheck(args, caps_role(&parent.caps)) {
        Ok(rt) => rt,
        Err(e) => return e,
    };
    let store = &parent.shared.store;
    let view = match store
        .internal_post("/internal/v1/turn/subagent", &json!({ "role": role }))
        .await
    {
        Ok((s, v)) if s.as_u16() == 200 => v,
        Ok((_, v)) => {
            let e = v["error"].as_str().unwrap_or("vue du sous-agent refusée");
            return report(&role, "", Some(e));
        }
        Err(e) => return report(&role, "", Some(&e)),
    };
    let tools: Vec<Tool> = serde_json::from_value(view["tools"].clone()).unwrap_or_default();
    let caps = view["caps"].clone();
    let msgs = vec![
        Message {
            role: "system".into(),
            content: Some(view["system"].clone()),
            ..Default::default()
        },
        Message {
            role: "user".into(),
            content: Some(Value::String(format!("{task}{}", framing(&role)))),
            ..Default::default()
        },
    ];
    let compact: CompactConfig = parent.compact.cfg.clone();
    let tc = TurnConfig {
        cfg: parent.cfg.clone(),
        ep: sub.ep.clone(),
        tools: tools.clone(),
        temperature: TEMPERATURE,
        code: caps.get("Code").and_then(Value::as_bool) == Some(true),
        reasoning: sub.reasoning.clone(),
        echo: sub.echo.clone(),
        compact: compact.clone(),
        patterns: sub.patterns.clone(),
    };
    let host = SubHost(GoTurnHost {
        shared: parent.shared.clone(),
        conv_id: parent.conv_id.clone(),
        caps,
        cfg: parent.cfg.clone(),
        port: parent.port,
        summarizer: parent.summarizer.clone(),
        compact: TurnCompact {
            cfg: compact,
            caps: parent.compact.caps,
            criteria: String::new(),
            main_conv: parent.compact.main_conv.clone(),
            tools,
            effort: sub.reasoning.effort.clone(),
            kwargs: sub.reasoning.kwargs.clone().unwrap_or_default(),
        },
        queued: Box::new(Vec::new),
        // Les critères posés par le planner s'affichent comme ceux du fil.
        deltas: parent.deltas.clone(),
        perf_kind: super::perf::KIND_SUBAGENT,
        side_slot: Default::default(),
        projsnap: None,
        snap_refreshed: Default::default(),
        sub: None,
    });
    // Le rôle occupe le slot de la discussion du builder : effacé à la fin
    // pour que le builder reparte du cache RAM (pas entre ses lectures).
    let side = super::verify::side_job_begin(store).await;
    let mut out = String::new();
    let mut cb = |ev: StreamEvent| -> bool {
        match ev {
            // Seul le texte écrit APRÈS le dernier outil est le rapport.
            StreamEvent::ToolUsed(_) => out.clear(),
            StreamEvent::Content(c) => out.push_str(&c),
            _ => {}
        }
        true
    };
    let r = run_turn(&host, &tc, msgs, &sub.cancel, &mut cb).await;
    super::verify::side_job_end(store, side).await;
    report(&role, &out, r.err.as_deref())
}

/// Le [`TurnHost`] du tour d'un rôle délégué : celui de la façade, sans
/// `subagent`.
pub struct SubHost(pub GoTurnHost);

impl TurnHost for SubHost {
    async fn send(
        &self,
        ep: &ChatEndpoint,
        body: Vec<u8>,
        keep_prewarm: bool,
    ) -> Result<Reply, SendError> {
        self.0.send(ep, body, keep_prewarm).await
    }

    async fn project_refresh(&self, msgs: Vec<Message>) -> Vec<Message> {
        self.0.project_refresh(msgs).await
    }

    fn prewarm_keeps(&self, prefix: &[u64], payload: &Value) -> bool {
        self.0.prewarm_keeps(prefix, payload)
    }

    async fn exec_tool(&self, name: &str, args: &Map<String, Value>, label: &str) -> ToolOutcome {
        if name == "subagent" {
            let result = match precheck(args, caps_role(&self.0.caps)) {
                Err(e) => e,
                // Un rôle sans `Role` : ne peut pas arriver (le Go le pose).
                Ok(_) => "[erreur] un sous-agent ne peut pas en appeler un autre — réponds avec ce que tu as".into(),
            };
            return ToolOutcome {
                result,
                ..Default::default()
            };
        }
        self.0.exec_tool_base(name, args, label).await
    }

    fn save_tool_result(&self, result: &str) -> String {
        self.0.save_tool_result(result)
    }

    async fn compact(&self, phase: CompactPhase, msgs: &[Message]) -> Option<Vec<Message>> {
        self.0.compact(phase, msgs).await
    }

    fn compact_refused(&self, used: i64, msgs: &[Message]) -> bool {
        self.0.compact_refused(used, msgs)
    }

    fn drain_queued(&self) -> Vec<Message> {
        self.0.drain_queued()
    }

    fn completed(&self, tools: &[Tool], kwargs: Option<&Map<String, Value>>, effort: &str) {
        self.0.completed(tools, kwargs, effort)
    }

    fn prefix_unstable(
        &self,
        tools: &[Tool],
        kwargs: Option<&Map<String, Value>>,
        effort: &str,
    ) -> bool {
        self.0.prefix_unstable(tools, kwargs, effort)
    }

    async fn prepare_images(&self, msgs: &[Message]) {
        self.0.prepare_images(msgs).await
    }

    fn image_data_url(&self, r: &str) -> Option<String> {
        self.0.image_data_url(r)
    }

    fn effort_remember(&self, want: &str, got: &str) {
        self.0.effort_remember(want, got)
    }

    fn echo_refuse(&self, model: &str) {
        self.0.echo_refuse(model)
    }

    fn cache_hint(&self) -> Option<CacheHintFn> {
        self.0.cache_hint()
    }

    fn preflight(&self) -> Option<PreflightFn> {
        self.0.preflight()
    }

    async fn keep_images(&self) -> Option<i64> {
        self.0.keep_images().await
    }

    async fn keep_image(&self, m: &Message) -> Option<(Message, i64)> {
        self.0.keep_image(m).await
    }

    async fn engine_slot(&self, ep: &ChatEndpoint) -> Option<u8> {
        self.0.engine_slot(ep).await
    }

    async fn perf_record(&self, rec: PerfRec) -> PerfMarks {
        self.0.perf_record(rec).await
    }
}

#[cfg(test)]
mod tests;
