//! Petites règles de la boucle d'un tour — port de `llm_budget.go`,
//! `appendNudge` (`llm_injected.go`), `steerSystem`, `dedupableTool`,
//! `repeatedCallResult` et `toolResultPreview` (`llm_client.go`).

use serde_json::Value;

use super::types::Message;

pub(crate) use super::compact::{THINK_NUDGE_FIRST, THINK_NUDGE_STUCK};

/// Palier par défaut du budget souple d'appels d'outils (`agentBudgetDefault`).
pub const AGENT_BUDGET_DEFAULT: i64 = 24;
/// Multiplicateur du palier en mode Code (`codeBudgetFactor`).
pub const CODE_BUDGET_FACTOR: i64 = 3;
/// Relances « pensé sans agir » d'un tour (`maxNudges`).
pub const MAX_NUDGES: i64 = 2;
/// Aperçu d'un résultat d'outil envoyé à l'UI, en caractères (`toolPreviewChars`).
pub const TOOL_PREVIEW_CHARS: usize = 1600;

/// `agentBudget` : palier AGENT_BUDGET ; 0 ou off/false/no/non/disable(d)
/// coupe le mécanisme ; illisible → défaut.
pub fn agent_budget(v: &str) -> i64 {
    match v.trim().to_lowercase().as_str() {
        "" => AGENT_BUDGET_DEFAULT,
        "off" | "false" | "no" | "non" | "disable" | "disabled" => 0,
        s => s
            .parse::<i64>()
            .ok()
            .filter(|n| *n >= 0)
            .unwrap_or(AGENT_BUDGET_DEFAULT),
    }
}

/// `budgetNudge` : le rappel à injecter quand le tour franchit un palier
/// (budget, 2×budget…), ou "" : `calls` appels faits, `sent` rappels déjà
/// envoyés. Une pression, pas une barrière.
pub fn budget_nudge(calls: i64, budget: i64, sent: i64) -> String {
    if budget <= 0 || calls < budget * (sent + 1) {
        return String::new();
    }
    let n = format!("{calls} tool calls");
    match sent {
        0 => format!(
            "[system] You have made {n} in this single turn. That is a lot: you almost certainly have enough to answer already. Stop exploring and write your final answer now, unless exactly one specific call is genuinely still missing."
        ),
        1 => format!(
            "[system] {n} now, and you are going in circles. Re-reading a file you have already read, or re-deriving a conclusion you have already written, adds nothing. Answer NOW with what you have. If something remains uncertain, say so in the answer instead of investigating further."
        ),
        _ => format!(
            "[system] {n}. Stop. Write your final answer in this message. Do not call another tool."
        ),
    }
}

/// `logBudget`.
pub fn log_budget(calls: i64, budget: i64, sent: i64) {
    eprintln!(
        "[agent] {calls} appels d'outils sur ce tour (palier={budget}) — rappel n°{sent} envoyé au modèle"
    );
}

/// `appendNudge` : le rappel part en fin de fil ET dans l'historique (le tour
/// suivant le renvoie à l'identique) — sauf juste après un message `user`,
/// où deux `user` d'affilée n'entrent pas dans l'historique.
pub fn append_nudge(messages: &mut Vec<Message>, extra: &mut Vec<Message>, text: &str) {
    let m = Message {
        role: "user".into(),
        content: Some(Value::String(text.into())),
        ..Default::default()
    };
    let after_user = messages.last().is_some_and(|l| l.role == "user");
    messages.push(m.clone());
    if !after_user {
        extra.push(m);
    }
}

/// `steerSystem` : la consigne s'ajoute au système de tête (texte), ou
/// devient un système en tête — jamais un système en fin de fil, que les
/// gabarits stricts refusent.
pub fn steer_system(msgs: &[Message], hint: &str) -> Vec<Message> {
    if let Some(first) = msgs.first()
        && first.role == "system"
        && let Some(Value::String(existing)) = &first.content
    {
        let mut out = msgs.to_vec();
        out[0] = Message {
            role: "system".into(),
            content: Some(Value::String(format!("{existing}\n\n{hint}"))),
            ..Default::default()
        };
        return out;
    }
    let mut out = Vec::with_capacity(msgs.len() + 1);
    out.push(Message {
        role: "system".into(),
        content: Some(Value::String(hint.into())),
        ..Default::default()
    });
    out.extend_from_slice(msgs);
    out
}

/// `dedupableTool` : un appel identique se court-circuite, sauf bash et ses
/// jobs (effets de bord), see_image et le navigateur (état vivant, image à
/// part).
pub fn dedupable_tool(name: &str) -> bool {
    !matches!(name, "bash" | "bash_bg" | "bash_tail" | "see_image") && !name.starts_with("browser_")
}

/// `repeatedCallResult` : réponse à un appel RIGOUREUSEMENT identique déjà
/// exécuté dans le tour — la note en tête, puis plus de charge utile du tout.
pub fn repeated_call_result(prev: &str, repeats: i64) -> String {
    if repeats >= 2 {
        return format!(
            "[déjà fait] Cet appel exact a déjà été exécuté {repeats} fois dans ce tour ; son résultat est plus haut dans la conversation. Ne le redemande plus : réponds avec ce que tu as, ou change d'approche (autre URL, autres arguments, web_grep pour cibler)."
        );
    }
    format!(
        "[déjà fait] Appel identique déjà exécuté dans ce tour — non rejoué. Voici à nouveau son résultat ; ne le redemande pas une troisième fois.\n\n{prev}"
    )
}

/// `toolResultPreview` : (aperçu, taille réelle en caractères, coupé ?).
pub fn tool_result_preview(s: &str) -> (&str, usize, bool) {
    let n = s.chars().count();
    if n <= TOOL_PREVIEW_CHARS {
        return (s, n, false);
    }
    let end = s
        .char_indices()
        .nth(TOOL_PREVIEW_CHARS)
        .map_or(s.len(), |(i, _)| i);
    (&s[..end], n, true)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::chat::compact::is_loki_injected;
    use serde_json::json;

    fn um(s: &str) -> Message {
        Message {
            role: "user".into(),
            content: Some(json!(s)),
            ..Default::default()
        }
    }

    #[test]
    fn budget() {
        assert_eq!(agent_budget(""), 24);
        assert_eq!(agent_budget(" Off "), 0);
        assert_eq!(agent_budget("10"), 10);
        assert_eq!(agent_budget("-3"), 24);
        assert_eq!(agent_budget("x"), 24);
        assert_eq!(budget_nudge(23, 24, 0), "");
        assert_eq!(budget_nudge(100, 0, 0), "");
        let first = budget_nudge(24, 24, 0);
        assert!(first.contains("24 tool calls") && first.starts_with("[system] "));
        assert_eq!(budget_nudge(47, 24, 1), "");
        assert!(budget_nudge(48, 24, 1).contains("going in circles"));
        assert!(budget_nudge(72, 24, 2).ends_with("Do not call another tool."));
        // Les rappels sont reconnus comme venant de loki (titre, compaction…).
        for sent in 0..3 {
            assert!(is_loki_injected(&um(&budget_nudge(
                24 * (sent + 1),
                24,
                sent
            ))));
        }
    }

    #[test]
    fn rappel_en_fin_de_fil() {
        let (mut msgs, mut extra) = (vec![um("q")], vec![]);
        append_nudge(&mut msgs, &mut extra, "a");
        assert_eq!(
            (msgs.len(), extra.len()),
            (2, 0),
            "après un user : hors historique"
        );
        let tool = Message {
            role: "tool".into(),
            content: Some(json!("r")),
            ..Default::default()
        };
        msgs.push(tool);
        append_nudge(&mut msgs, &mut extra, "b");
        assert_eq!((msgs.len(), extra.len()), (4, 1));
    }

    #[test]
    fn consigne_systeme_en_tete() {
        let sys = Message {
            role: "system".into(),
            content: Some(json!("base")),
            ..Default::default()
        };
        let out = steer_system(&[sys, um("q")], "h");
        assert_eq!(out[0].content, Some(json!("base\n\nh")));
        assert_eq!(out.len(), 2);
        let out = steer_system(&[um("q")], "h");
        assert_eq!((out[0].role.as_str(), out.len()), ("system", 2));
    }

    #[test]
    fn appels_repetes() {
        assert!(dedupable_tool("edit") && dedupable_tool("web_fetch"));
        assert!(!dedupable_tool("bash") && !dedupable_tool("browser_click"));
        assert!(repeated_call_result("R", 1).ends_with("\n\nR"));
        let r = repeated_call_result("R", 2);
        assert!(r.contains("2 fois") && !r.ends_with("\n\nR"));
    }

    #[test]
    fn apercu() {
        assert_eq!(tool_result_preview("abc"), ("abc", 3, false));
        let long = "é".repeat(TOOL_PREVIEW_CHARS + 5);
        let (p, n, cut) = tool_result_preview(&long);
        assert_eq!(
            (p.chars().count(), n, cut),
            (TOOL_PREVIEW_CHARS, TOOL_PREVIEW_CHARS + 5, true)
        );
    }
}
