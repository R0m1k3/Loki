//! Début et fin d'un tour, côté historique — port de `historyFromView`
//! (`chat_conversation.go`), `dropStrayNudges` (`llm_injected.go`), du rappel
//! des pages mémoire lues (`chat_mem_pinned.go`), de l'assemblage
//! `InjectSkills` (`llm_client.go`) et de la forme des événements `tool_used`
//! du journal (`generate`).

use std::collections::HashMap;

use serde_json::{Map, Value, json};

use super::compact::{is_loki_injected, msg_text};
use super::stream::call_args;
use super::types::{Message, ToolUsedEvent};
use super::wire::{MEM_REMINDER_PREFIX, is_project_system};

/// Noms de pages listés au plus par le rappel (`memReminderMaxNames`).
pub const MEM_REMINDER_MAX_NAMES: usize = 24;

fn system(text: String) -> Message {
    Message {
        role: "system".into(),
        content: Some(Value::String(text)),
        ..Default::default()
    }
}

/// `InjectSkills` avec le préfixe déjà calculé (préambule d'agent + briefing
/// machine, `/internal/v1/turn/view`) : fusionné dans le premier système s'il
/// n'est pas un message projet, sinon posé en tête. Préfixe vide : rien.
pub fn inject_prefix(msgs: Vec<Message>, prefix: &str) -> Vec<Message> {
    if prefix.is_empty() {
        return msgs;
    }
    if let Some(first) = msgs.first()
        && first.role == "system"
        && !is_project_system(first)
    {
        let existing = match &first.content {
            Some(Value::String(s)) => s.as_str(),
            _ => "",
        };
        let mut out = msgs.clone();
        out[0] = system(format!("{prefix}\n\n{existing}"));
        return out;
    }
    let mut out = Vec::with_capacity(msgs.len() + 1);
    out.push(system(prefix.to_string()));
    out.extend(msgs);
    out
}

/// `historyFromView` : l'historique à ranger après une compaction en cours de
/// tour — la vue publiée sans les systèmes que la vue d'envoi avait posés en
/// tête. Un système de tête n'est gardé que s'il figurait en tête de `hist`
/// (le premier, éventuellement fusionné avec le préambule, est rendu tel qu'il
/// était dans l'historique).
pub fn history_from_view(view: &[Message], hist: &[Message]) -> Vec<Message> {
    let mut own: HashMap<String, usize> = HashMap::new();
    for m in hist.iter().take_while(|m| m.role == "system") {
        *own.entry(msg_text(m)).or_default() += 1;
    }
    let mut out = Vec::new();
    let mut i = 0;
    while i < view.len() && view[i].role == "system" {
        let k = msg_text(&view[i]);
        if let Some(n) = own.get_mut(&k)
            && *n > 0
        {
            *n -= 1;
            out.push(view[i].clone());
            i += 1;
            continue;
        }
        if i == 0
            && let Some(h0) = hist.first()
            && h0.role == "system"
        {
            let h = msg_text(h0);
            if !h.is_empty()
                && own.get(&h).is_some_and(|n| *n > 0)
                && k.ends_with(&format!("\n\n{h}"))
            {
                if let Some(n) = own.get_mut(&h) {
                    *n -= 1;
                }
                out.push(h0.clone());
            }
        }
        i += 1;
    }
    out.extend_from_slice(&view[i..]);
    out
}

/// `dropStrayNudges` : retire les rappels de loki en porte-à-faux (en toute
/// fin, ou collés à un autre `user`) — sinon deux `user` d'affilée au tour
/// suivant.
pub fn drop_stray_nudges(mut msgs: Vec<Message>) -> Vec<Message> {
    loop {
        let n = msgs.len();
        let drop = (0..n).find(|&i| {
            is_loki_injected(&msgs[i])
                && (i == n - 1
                    || msgs[i + 1].role == "user"
                    || (i > 0 && msgs[i - 1].role == "user"))
        });
        match drop {
            Some(i) => {
                msgs.remove(i);
            }
            None => return msgs,
        }
    }
}

/// `parseReminderNames` : noms listés par un rappel, vide si ce n'en est pas.
pub fn parse_reminder_names(text: &str) -> Vec<String> {
    if !text.starts_with(MEM_REMINDER_PREFIX) {
        return Vec::new();
    }
    let line = text.split('\n').next().unwrap_or("");
    let Some(i) = line.rfind(": ") else {
        return Vec::new();
    };
    line[i + 2..]
        .split(',')
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(str::to_string)
        .collect()
}

/// `collectReadPageNames` : pages lues (mem_read réussis, puis rappels
/// hérités), dans l'ordre, sans doublon.
pub fn collect_read_page_names(msgs: &[Message]) -> Vec<String> {
    let mut read_call: HashMap<&str, String> = HashMap::new();
    for m in msgs {
        for tc in &m.tool_calls {
            if tc.function.name != "mem_read" || tc.id.is_empty() {
                continue;
            }
            if let Some(Value::String(f)) = call_args(&tc.function.arguments).get("file")
                && !f.is_empty()
            {
                read_call.insert(tc.id.as_str(), f.clone());
            }
        }
    }
    let mut order: Vec<String> = Vec::new();
    let mut add = |name: &str| {
        let name = name.trim();
        if !name.is_empty() && !order.iter().any(|n| n == name) {
            order.push(name.to_string());
        }
    };
    for m in msgs {
        match m.role.as_str() {
            "system" => {
                for n in parse_reminder_names(&msg_text(m)) {
                    add(&n);
                }
            }
            "tool" => {
                let Some(name) = read_call.get(m.tool_call_id.as_str()) else {
                    continue;
                };
                if msg_text(m).trim().starts_with("[erreur]") {
                    continue;
                }
                add(name);
            }
            _ => {}
        }
    }
    order
}

/// `buildReminderMessage` : le rappel, les plus récentes en dernier ; `None`
/// sans page lue.
pub fn build_reminder_message(names: &[String]) -> Option<Message> {
    if names.is_empty() {
        return None;
    }
    let names = &names[names.len().saturating_sub(MEM_REMINDER_MAX_NAMES)..];
    Some(system(format!(
        "{MEM_REMINDER_PREFIX} Memory pages you read earlier: {}\nTheir full content is no longer inline after context compaction — if any of them is relevant to what you're doing, mem_read it again so you have all the rules and info you need before answering.",
        names.join(", ")
    )))
}

/// `stripReminder`.
pub fn strip_reminder(msgs: Vec<Message>) -> Vec<Message> {
    msgs.into_iter()
        .filter(|m| {
            !(m.role == "system"
                && matches!(&m.content, Some(Value::String(s)) if s.starts_with(MEM_REMINDER_PREFIX)))
        })
        .collect()
}

/// `remindReadMemPages` : rappel reconstruit d'après l'historique d'avant la
/// compaction, en tête de l'historique compacté. Mémoire coupée : rien.
pub fn remind_read_mem_pages(
    compacted: Vec<Message>,
    source: &[Message],
    mem_off: bool,
) -> Vec<Message> {
    if mem_off {
        return compacted;
    }
    let names = collect_read_page_names(source);
    let mut out = strip_reminder(compacted);
    if let Some(m) = build_reminder_message(&names) {
        out.insert(0, m);
    }
    out
}

/// L'événement `tool_used` du journal (forme de `generate`) : le corps en
/// cours de frappe, le diff, la capture, l'aperçu et l'id du résultat complet
/// seulement quand ils sont là.
pub fn tool_used_delta(t: &ToolUsedEvent) -> Map<String, Value> {
    let mut tu = Map::new();
    tu.insert("name".into(), t.name.clone().into());
    tu.insert("label".into(), t.label.clone().into());
    tu.insert("result".into(), t.result.clone().into());
    tu.insert("done".into(), t.done.into());
    tu.insert("typing".into(), t.typing.into());
    if !t.body.is_empty() {
        tu.insert("body".into(), t.body.clone().into());
        tu.insert("body_lines".into(), t.body_lines.into());
        if t.body_tail {
            tu.insert("body_tail".into(), true.into());
        }
    }
    if !t.diff.is_empty() {
        tu.insert("diff".into(), json!(t.diff));
        tu.insert("added".into(), t.added.into());
        tu.insert("removed".into(), t.removed.into());
    }
    if !t.image.is_empty() {
        tu.insert("image".into(), t.image.clone().into());
    }
    if !t.shot.is_empty() {
        tu.insert("shot".into(), t.shot.clone().into());
    }
    if t.result_chars > 0 {
        tu.insert("result_chars".into(), t.result_chars.into());
    }
    if !t.result_id.is_empty() {
        tu.insert("result_id".into(), t.result_id.clone().into());
    }
    let mut d = Map::new();
    d.insert("tool_used".into(), Value::Object(tu));
    d
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::chat::compact::{THINK_NUDGE_FIRST, THINK_NUDGE_STUCK};
    use crate::chat::stream::retry_corrective;
    use crate::chat::types::{DiffLine, ToolCall, ToolCallFunc};
    use crate::chat::wire::PROJECT_CONTEXT_PREFIX;

    fn msg(role: &str, s: &str) -> Message {
        Message {
            role: role.into(),
            content: Some(json!(s)),
            ..Default::default()
        }
    }
    fn um(s: &str) -> Message {
        msg("user", s)
    }
    fn am(s: &str) -> Message {
        msg("assistant", s)
    }
    fn tm(s: &str) -> Message {
        msg("tool", s)
    }
    fn atc(name: &str) -> Message {
        Message {
            role: "assistant".into(),
            tool_calls: vec![ToolCall {
                id: "c1".into(),
                kind: "function".into(),
                function: ToolCallFunc {
                    name: name.into(),
                    arguments: "{}".into(),
                },
                ..Default::default()
            }],
            ..Default::default()
        }
    }
    fn read(id: &str, file: &str) -> Message {
        Message {
            role: "assistant".into(),
            tool_calls: vec![ToolCall {
                id: id.into(),
                function: ToolCallFunc {
                    name: "mem_read".into(),
                    arguments: format!("{{\"file\":\"{file}\"}}"),
                },
                ..Default::default()
            }],
            ..Default::default()
        }
    }
    fn res(id: &str, s: &str) -> Message {
        Message {
            role: "tool".into(),
            tool_call_id: id.into(),
            content: Some(json!(s)),
            ..Default::default()
        }
    }

    #[test]
    fn prefixe_fusionne_ou_pose_en_tete() {
        let sys = msg("system", "/sys perso");
        let out = inject_prefix(vec![sys, um("q")], "préambule");
        assert_eq!(out[0], msg("system", "préambule\n\n/sys perso"));
        assert_eq!(out.len(), 2);
        let proj = msg("system", &format!("{PROJECT_CONTEXT_PREFIX} x"));
        let out = inject_prefix(vec![proj.clone(), um("q")], "préambule");
        assert_eq!(
            (out.len(), &out[1]),
            (3, &proj),
            "un message projet n'absorbe pas le préambule"
        );
        assert_eq!(inject_prefix(vec![um("q")], ""), vec![um("q")]);
    }

    #[test]
    fn historique_depuis_la_vue() {
        let rem = msg(
            "system",
            &format!("{MEM_REMINDER_PREFIX} Memory pages you read earlier: a.md"),
        );
        let sys = msg("system", "/sys perso");
        let proj = msg("system", &format!("{PROJECT_CONTEXT_PREFIX} x"));
        let u = um("q");
        let pre = msg("system", "préambule");
        let cases = [
            (
                "injectés seuls",
                vec![pre.clone(), proj.clone(), u.clone()],
                vec![u.clone()],
                vec![u.clone()],
            ),
            (
                "rappel gardé",
                vec![pre.clone(), proj.clone(), rem.clone(), u.clone()],
                vec![rem.clone(), u.clone()],
                vec![rem.clone(), u.clone()],
            ),
            (
                "système fusionné rendu",
                vec![msg("system", "préambule\n\n/sys perso"), u.clone()],
                vec![sys.clone(), u.clone()],
                vec![sys.clone(), u.clone()],
            ),
            (
                "sans système",
                vec![u.clone()],
                vec![u.clone()],
                vec![u.clone()],
            ),
        ];
        for (name, view, hist, want) in cases {
            assert_eq!(history_from_view(&view, &hist), want, "{name}");
        }
    }

    #[test]
    fn rappels_en_porte_a_faux() {
        let n = um(THINK_NUDGE_FIRST);
        type Case = (&'static str, Vec<Message>, Option<Vec<Message>>);
        let cases: Vec<Case> = vec![
            (
                "rappel répondu",
                vec![um("q"), atc("glob"), tm("r"), n.clone(), am("fini")],
                None,
            ),
            (
                "resté sans réponse",
                vec![um("q"), atc("glob"), tm("r"), n.clone()],
                Some(vec![um("q"), atc("glob"), tm("r")]),
            ),
            (
                "ajout juste derrière",
                vec![
                    um("q"),
                    atc("glob"),
                    tm("r"),
                    n.clone(),
                    um("et aussi"),
                    am("fini"),
                ],
                Some(vec![
                    um("q"),
                    atc("glob"),
                    tm("r"),
                    um("et aussi"),
                    am("fini"),
                ]),
            ),
            (
                "collé derrière la demande",
                vec![um("q"), n.clone(), am("fini")],
                Some(vec![um("q"), am("fini")]),
            ),
            (
                "correctif sans réponse",
                vec![
                    um("q"),
                    am("<tool_call>"),
                    um(&retry_corrective("<tool_call>")),
                ],
                Some(vec![um("q"), am("<tool_call>")]),
            ),
            (
                "deux rappels en fin",
                vec![
                    um("q"),
                    atc("glob"),
                    tm("r"),
                    n.clone(),
                    um(THINK_NUDGE_STUCK),
                ],
                Some(vec![um("q"), atc("glob"), tm("r")]),
            ),
            (
                "vraie demande en fin",
                vec![um("q"), am("a"), um("suite")],
                None,
            ),
        ];
        for (name, input, want) in cases {
            let want = want.unwrap_or_else(|| input.clone());
            assert_eq!(drop_stray_nudges(input), want, "{name}");
        }
    }

    #[test]
    fn rappel_des_pages_lues() {
        let msgs = vec![
            um("applique"),
            read("c1", "regles-tache.md"),
            res("c1", "RÈGLE 1"),
        ];
        let names = collect_read_page_names(&msgs);
        assert_eq!(names, ["regles-tache.md"]);
        let m = build_reminder_message(&names).unwrap();
        let txt = msg_text(&m);
        assert!(txt.contains("regles-tache.md") && txt.contains("mem_read"));
        assert_eq!(parse_reminder_names(&txt), names);
        // Accumulé à travers les compactions, dédoublonné, sans lecture ratée.
        let msgs = vec![
            m.clone(),
            um("[CONTEXT COMPACTED] résumé…"),
            read("c2", "procedure.md"),
            res("c2", "procédure"),
            read("c3", "procedure.md"),
            res("c3", "v2"),
            read("e", "absente.md"),
            res("e", "[erreur] page introuvable"),
        ];
        assert_eq!(
            collect_read_page_names(&msgs),
            ["regles-tache.md", "procedure.md"]
        );
        // Borné aux plus récentes.
        let many: Vec<String> = (0..MEM_REMINDER_MAX_NAMES + 10)
            .map(|i| format!("p{i}.md"))
            .collect();
        let got = parse_reminder_names(&msg_text(&build_reminder_message(&many).unwrap()));
        assert_eq!(got.len(), MEM_REMINDER_MAX_NAMES);
        assert_eq!(got.last(), many.last());
        // Reconstruit en tête de l'historique compacté ; rien si mémoire coupée.
        let compacted = vec![m.clone(), um("résumé")];
        let out = remind_read_mem_pages(compacted.clone(), &msgs, false);
        assert_eq!(out.len(), 2);
        assert_eq!(
            parse_reminder_names(&msg_text(&out[0])),
            ["regles-tache.md", "procedure.md"]
        );
        assert_eq!(
            remind_read_mem_pages(compacted.clone(), &msgs, true),
            compacted
        );
        assert_eq!(strip_reminder(vec![m, um("x")]), vec![um("x")]);
    }

    #[test]
    fn evenement_d_outil_du_journal() {
        let d = tool_used_delta(&ToolUsedEvent {
            name: "bash".into(),
            label: "ls".into(),
            ..Default::default()
        });
        assert_eq!(
            Value::Object(d),
            json!({"tool_used": {"name": "bash", "label": "ls", "result": "", "done": false, "typing": false}})
        );
        let d = tool_used_delta(&ToolUsedEvent {
            name: "edit".into(),
            done: true,
            body: "x".into(),
            body_lines: 1,
            body_tail: true,
            diff: vec![DiffLine {
                op: "+".into(),
                text: "x".into(),
            }],
            added: 1,
            result_chars: 3000,
            result_id: "c1.ab".into(),
            ..Default::default()
        });
        let tu = &d["tool_used"];
        assert_eq!(tu["body_tail"], true);
        assert_eq!(tu["diff"], json!([{"op": "+", "text": "x"}]));
        assert_eq!(
            (tu["added"].clone(), tu["removed"].clone()),
            (json!(1), json!(0))
        );
        assert_eq!(
            (tu["result_chars"].clone(), tu["result_id"].clone()),
            (json!(3000), json!("c1.ab"))
        );
        assert!(tu.get("image").is_none() && tu.get("shot").is_none());
    }
}
