//! Les décisions pures d'un tour du mode Code confié à Claude Code
//! (`ChatService::generate_claude`) : refus, prompt envoyé, relance quand la
//! session à reprendre a disparu, rangement de la réponse.

use std::future::Future;

use serde_json::Value;

use super::compact::{estimate_tokens, strip_context_updates};
use super::history::drop_stray_nudges;
use super::persist::ConvState;
use super::types::Message;
use super::wire::EchoPolicy;
use crate::harness::claude::{ClaudeTurn, Outcome, content_text};
use crate::tools::ToolCtx;

/// Fenêtre minimale : le prompt de Claude Code pèse ~20 000 jetons.
pub const MIN_CTX: i64 = 32_000;
/// Contexte joint au premier tour (ou à une reprise ratée).
const RECENT_MSGS: usize = 10;
const RECENT_CHARS: usize = 12_000;

/// Pourquoi Claude Code ne peut pas prendre ce tour (`None` : il peut).
pub fn claude_refusal(
    external: bool,
    ctx_window: i64,
    tool_ctx: &Result<ToolCtx, String>,
    bin_present: bool,
    route: &Result<Value, String>,
) -> Option<String> {
    if external {
        return Some(
            "Claude Code ne marche qu'avec le moteur local de Loki (moteur externe actif).".into(),
        );
    }
    if ctx_window < MIN_CTX {
        return Some(format!(
            "Claude Code demande une fenêtre d'au moins {MIN_CTX} jetons (preset actif : {ctx_window}). Choisis un preset plus grand."
        ));
    }
    match tool_ctx {
        Err(e) => return Some(format!("Claude Code : dossier de travail illisible ({e}).")),
        Ok(c) if !c.target.is_empty() => {
            return Some(
                "Claude Code ne travaille que sur cette machine (poste distant choisi).".into(),
            );
        }
        Ok(c) if c.cwd.as_os_str().is_empty() => {
            return Some("Claude Code : aucun dossier de travail.".into());
        }
        _ => {}
    }
    if !bin_present {
        return Some("Claude Code s'installe : réessaie dans une minute.".into());
    }
    match route {
        Err(e) => Some(format!("Claude Code : relais /v1 illisible ({e}).")),
        Ok(r) if r["key_ok"] != Value::Bool(true) => {
            Some("Claude Code : la clé du relais /v1 n'est pas lisible.".into())
        }
        _ => None,
    }
}

/// Le texte du message utilisateur, images signalées.
pub fn prompt_text(content: &Value) -> String {
    let mut t = content_text(content);
    let images = content.as_array().is_some_and(|a| {
        a.iter()
            .any(|p| p["type"].as_str().is_some_and(|k| k != "text"))
    });
    if images {
        if !t.is_empty() {
            t.push_str("\n\n");
        }
        t.push_str("[images jointes ignorées par Claude Code]");
    }
    t
}

/// Les derniers échanges de la discussion (sans le message en cours, déjà le
/// dernier de `msgs`), pour un Claude Code qui ne les a pas vus.
pub fn recent_context(msgs: &[Message]) -> String {
    let prior = &msgs[..msgs.len().saturating_sub(1)];
    let clean = drop_stray_nudges(strip_context_updates(prior.to_vec()));
    let mut lines: Vec<String> = clean
        .iter()
        .filter(|m| m.role == "user" || m.role == "assistant")
        .filter_map(|m| {
            let t = content_text(m.content.as_ref()?);
            let t = t.trim();
            if t.is_empty() {
                return None;
            }
            let who = if m.role == "user" {
                "Utilisateur"
            } else {
                "Assistant"
            };
            Some(format!("{who} : {t}"))
        })
        .collect();
    let keep = lines.len().saturating_sub(RECENT_MSGS);
    lines.drain(..keep);
    // Les plus récents d'abord dans le budget.
    let mut out: Vec<String> = Vec::new();
    let mut used = 0;
    for l in lines.into_iter().rev() {
        let n = l.chars().count();
        if used + n > RECENT_CHARS {
            break;
        }
        used += n;
        out.push(l);
    }
    if out.is_empty() {
        return String::new();
    }
    out.reverse();
    format!(
        "Contexte de la discussion jusqu'ici :\n\n{}",
        out.join("\n\n")
    )
}

/// Le prompt : avec la session, le message seul ; sans, précédé du contexte.
pub fn build_prompt(text: &str, context: &str, resumed: bool) -> String {
    if resumed || context.is_empty() {
        return text.to_string();
    }
    format!("{context}\n\n---\n\n{text}")
}

/// Lance le tour ; si la session à reprendre a disparu, relance une fois
/// sans elle, contexte joint. `forget` efface la session gardée.
pub async fn run_with_resume<L, LF, D, DF>(
    mut turn: ClaudeTurn,
    text: &str,
    context: &str,
    mut launch: L,
    forget: D,
) -> Outcome
where
    L: FnMut(ClaudeTurn) -> LF,
    LF: Future<Output = Outcome>,
    D: FnOnce() -> DF,
    DF: Future<Output = ()>,
{
    let resumed = turn.session.is_some();
    turn.prompt = build_prompt(text, context, resumed);
    let out = launch(turn.clone()).await;
    if !(resumed && out.resume_missing) {
        return out;
    }
    eprintln!("[harness] session Claude Code introuvable : nouveau départ avec le contexte");
    forget().await;
    turn.session = None;
    turn.prompt = build_prompt(text, context, false);
    launch(turn).await
}

/// Range la réponse finale dans la vue modèle et estime la jauge.
pub fn claude_finish(st: &mut ConvState, final_text: &str, echo: &EchoPolicy) -> i64 {
    if !final_text.trim().is_empty() {
        st.messages.push(Message {
            role: "assistant".into(),
            content: Some(Value::String(final_text.to_string())),
            ..Default::default()
        });
    }
    let est = estimate_tokens(&st.messages, echo);
    st.ctx_used = est;
    st.ctx_used_len = st.messages.len();
    est
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use std::sync::{Arc, Mutex};

    fn m(role: &str, t: &str) -> Message {
        Message {
            role: role.into(),
            content: Some(json!(t)),
            ..Default::default()
        }
    }

    fn ctx(cwd: &str, target: &str) -> Result<ToolCtx, String> {
        Ok(ToolCtx {
            cwd: cwd.into(),
            memory_dir: "/m".into(),
            target: target.into(),
            code: true,
        })
    }

    #[test]
    fn refus() {
        let ok_route: Result<Value, String> = Ok(json!({"key_ok": true, "key": ""}));
        let c = ctx("/w", "");
        assert_eq!(claude_refusal(false, 65536, &c, true, &ok_route), None);
        assert!(
            claude_refusal(true, 65536, &c, true, &ok_route)
                .unwrap()
                .contains("externe")
        );
        assert!(
            claude_refusal(false, 16384, &c, true, &ok_route)
                .unwrap()
                .contains("32000")
        );
        assert!(claude_refusal(false, 65536, &Err("x".into()), true, &ok_route).is_some());
        assert!(
            claude_refusal(false, 65536, &ctx("/w", "pc"), true, &ok_route)
                .unwrap()
                .contains("distant")
        );
        assert!(claude_refusal(false, 65536, &ctx("", ""), true, &ok_route).is_some());
        assert!(
            claude_refusal(false, 65536, &c, false, &ok_route)
                .unwrap()
                .contains("installe")
        );
        assert!(claude_refusal(false, 65536, &c, true, &Err("down".into())).is_some());
        assert!(claude_refusal(false, 65536, &c, true, &Ok(json!({"key_ok": false}))).is_some());
    }

    #[test]
    fn texte_du_message() {
        assert_eq!(prompt_text(&json!("salut")), "salut");
        let v =
            json!([{"type":"text","text":"vois"},{"type":"image_url","image_url":{"url":"data:"}}]);
        assert_eq!(
            prompt_text(&v),
            "vois\n\n[images jointes ignorées par Claude Code]"
        );
    }

    #[test]
    fn contexte_recent_sans_message_en_cours() {
        let msgs = vec![
            m("system", "consignes"),
            m("user", "un"),
            Message {
                role: "assistant".into(),
                content: None,
                ..Default::default()
            },
            m("tool", "sortie"),
            m("assistant", "deux"),
            m("user", "EN COURS"),
        ];
        let c = recent_context(&msgs);
        assert_eq!(
            c,
            "Contexte de la discussion jusqu'ici :\n\nUtilisateur : un\n\nAssistant : deux"
        );
        assert_eq!(recent_context(&[m("user", "seul")]), "");
        // Bornes : 10 messages, 12 000 caractères, les plus récents gardés.
        let mut many: Vec<Message> = (0..30).map(|i| m("user", &format!("m{i}"))).collect();
        many.push(m("user", "EN COURS"));
        let c = recent_context(&many);
        assert!(
            c.contains("m29") && c.contains("m20") && !c.contains("m19"),
            "{c}"
        );
        let big = vec![
            m("user", &"a".repeat(7000)),
            m("user", &"b".repeat(7000)),
            m("user", "x"),
        ];
        let c = recent_context(&big);
        assert!(c.contains('b') && !c.contains('a'.to_string().repeat(10).as_str()));
    }

    #[test]
    fn prompt_avec_ou_sans_session() {
        assert_eq!(build_prompt("q", "C", true), "q");
        assert_eq!(build_prompt("q", "", false), "q");
        assert_eq!(build_prompt("q", "C", false), "C\n\n---\n\nq");
    }

    #[tokio::test]
    async fn reprise_ratee_relancee_avec_contexte() {
        type Seen = Arc<Mutex<Vec<(Option<String>, String)>>>;
        let seen: Seen = Default::default();
        let forgot = Arc::new(Mutex::new(false));
        let t = ClaudeTurn {
            session: Some("vieille".into()),
            ..Default::default()
        };
        let s2 = seen.clone();
        let f2 = forgot.clone();
        let out = run_with_resume(
            t,
            "q",
            "C",
            move |t: ClaudeTurn| {
                let s2 = s2.clone();
                async move {
                    let first = s2.lock().unwrap().is_empty();
                    s2.lock()
                        .unwrap()
                        .push((t.session.clone(), t.prompt.clone()));
                    Outcome {
                        resume_missing: first,
                        session_id: if first { String::new() } else { "neuve".into() },
                        ..Default::default()
                    }
                }
            },
            move || async move { *f2.lock().unwrap() = true },
        )
        .await;
        assert_eq!(out.session_id, "neuve");
        assert!(*forgot.lock().unwrap());
        assert_eq!(
            *seen.lock().unwrap(),
            vec![
                (Some("vieille".into()), "q".into()),
                (None, "C\n\n---\n\nq".into())
            ]
        );
        // Sans session, une seule tentative même si « introuvable ».
        let n = Arc::new(Mutex::new(0));
        let n2 = n.clone();
        run_with_resume(
            ClaudeTurn::default(),
            "q",
            "",
            move |_| {
                *n2.lock().unwrap() += 1;
                async {
                    Outcome {
                        resume_missing: true,
                        ..Default::default()
                    }
                }
            },
            || async {},
        )
        .await;
        assert_eq!(*n.lock().unwrap(), 1);
    }

    #[test]
    fn fin_de_tour() {
        let mut st = ConvState {
            messages: vec![m("user", "q")],
            ..Default::default()
        };
        let echo = EchoPolicy::default();
        let est = claude_finish(&mut st, "réponse", &echo);
        assert_eq!(st.messages.len(), 2);
        assert_eq!(st.messages[1].content, Some(json!("réponse")));
        assert_eq!((st.ctx_used, st.ctx_used_len), (est, 2));
        assert!(est > 0);
        claude_finish(&mut st, "  ", &echo);
        assert_eq!(st.messages.len(), 2);
    }
}
