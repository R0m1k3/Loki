//! La boucle du contrat du mode Code — port de `code_verify.go` (lot 7).
//!
//! À la fin d'un tour de build en mode Code, tant que les critères
//! d'acceptation ne sont pas tous `passed` : le builder est d'abord relancé
//! sur ses critères encore ouverts (au plus [`CODE_MAX_CONTINUE`] fois), puis
//! une passe de VÉRIFICATION tourne sur un contexte isolé (prompt du rôle +
//! critères + tâche, pas l'historique) — seule autorisée à marquer un critère
//! `passed` —, suivie au besoin d'une correction par le builder sur
//! l'historique normal, au plus [`CODE_MAX_FIX_TURNS`] fois.
//!
//! Les critères restent au Go (l'outil `criteria` s'y exécute) : ils sont lus
//! en base (`chat/crit:<id>`, chiffrés comme le reste) ; le contexte isolé du
//! vérificateur vient de `/internal/v1/turn/verifier`.
//!
//! La passe de vérification est un travail annexe (`engineSideJob`, lot 31) :
//! le Go voit passer ses requêtes (tickets « en vol », moteur qui les accepte)
//! et, à la fin, efface le slot local s'il a servi — la discussion se
//! recharge alors depuis le cache RAM du moteur au lieu de se recalculer.

use std::sync::Arc;

use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tokio::sync::watch;

use super::compact::{CompactCaps, is_loki_injected, without_ctx_update};
use super::generate::{Assembled, ChatService, TurnEnv, obj};
use super::history::{drop_stray_nudges, history_from_view, tool_used_delta};
use super::host::{GoTurnHost, SnapTurn, TurnCompact};
use super::lease::{EngineLease, LeaseSignal};
use super::persist::BK_CHAT;
use super::persist::ProjSnapshot;
use super::turn::{Cancel, TurnConfig, run_turn};
use super::types::{Message, ReasoningEcho, StreamEvent, Tool};
use super::wire::{TurnReasoning, with_echo};
use crate::store::Store;

/// Tours de CORRECTION après un échec de vérification. Au-delà, on s'arrête
/// et on dit ce qui manque : l'utilisateur reprend la main.
pub const CODE_MAX_FIX_TURNS: usize = 2;
/// Relances du builder qui a rendu la main avec des critères encore ouverts,
/// avant de vérifier quand même.
pub const CODE_MAX_CONTINUE: usize = 2;

/// Un critère d'acceptation (`Criterion`).
#[derive(Debug, Clone, Default, PartialEq, Deserialize, Serialize)]
pub struct Criterion {
    #[serde(default)]
    pub id: i64,
    #[serde(default)]
    pub text: String,
    /// pending | completed (le builder dit « fait ») | passed | failed.
    #[serde(default)]
    pub status: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub note: String,
}

/// `engineSideJob` : début d'un travail annexe auprès du Go ; `None` s'il ne
/// répond pas.
async fn side_job_begin(store: &Store) -> Option<i64> {
    store
        .internal_post("/internal/v1/engine/sidejob", &json!({}))
        .await
        .ok()
        .and_then(|(_, v)| v.get("id").and_then(Value::as_i64))
}

/// Fin du travail annexe. Les requêtes servies ont déjà été comptées une à
/// une (`/internal/v1/oai/served`) : rien à ajouter.
async fn side_job_end(store: &Store, id: Option<i64>) {
    if let Some(id) = id {
        let _ = store
            .internal_post(
                "/internal/v1/engine/sidejob",
                &json!({ "end": id, "served": false }),
            )
            .await;
    }
}

/// `critList` : les critères de la discussion (vide si absents ou illisibles,
/// mémoire verrouillée comprise).
pub async fn crit_list(store: &Store, conv_id: &str) -> Vec<Criterion> {
    match store.get_open(BK_CHAT, &format!("crit:{conv_id}")).await {
        Ok(Some(b)) if !b.is_empty() => serde_json::from_slice(&b).unwrap_or_default(),
        _ => Vec::new(),
    }
}

/// `critAllPassed` : contrat rempli. Une liste vide ne compte pas.
pub fn crit_all_passed(list: &[Criterion]) -> bool {
    !list.is_empty() && list.iter().all(|c| c.status == "passed")
}

/// `critPending` : critères pas encore `passed`.
pub fn crit_pending(list: &[Criterion]) -> usize {
    list.iter().filter(|c| c.status != "passed").count()
}

/// `critOpen` : pas encore déclarés faits par le builder.
pub fn crit_open(list: &[Criterion]) -> Vec<Criterion> {
    list.iter()
        .filter(|c| matches!(c.status.as_str(), "pending" | "failed" | ""))
        .cloned()
        .collect()
}

/// `critRender` : la liste formatée pour un prompt.
pub fn crit_render(list: &[Criterion]) -> String {
    list.iter()
        .map(|c| {
            let mark = match c.status.as_str() {
                "passed" => "[✓]",
                "failed" => "[✗]",
                "completed" => "[~]", // fait selon le builder, pas encore vérifié
                _ => "[ ]",
            };
            let mut l = format!("{mark} #{} {}", c.id, c.text);
            if !c.note.is_empty() {
                l.push_str(&format!(" — {}", c.note));
            }
            l
        })
        .collect::<Vec<_>>()
        .join("\n")
}

/// `lastUserText` : la tâche — le dernier message utilisateur, hors rappels
/// de Loki et sans mise à jour du projet en tête.
pub fn last_user_text(msgs: &[Message]) -> String {
    for m in msgs.iter().rev() {
        if m.role != "user" || is_loki_injected(m) {
            continue;
        }
        let m = without_ctx_update(m).unwrap_or_else(|| m.clone());
        if let Some(Value::String(s)) = &m.content {
            return s.clone();
        }
    }
    "(tâche non retrouvée — vérifie d'après le diff)".into()
}

/// Ce que les passes du contrat partagent avec le tour qui les lance.
pub(super) struct PassEnv<'a> {
    pub env: &'a TurnEnv,
    pub turn_caps: &'a Value,
    pub temperature: f64,
    pub reasoning: &'a TurnReasoning,
    pub ccaps: CompactCaps,
    pub conv_id: &'a str,
}

/// L'arrêt du tour et le bail du moteur, tenus pendant les passes.
pub(super) struct PassRun<'a> {
    pub lease: &'a mut EngineLease,
    pub cancel: &'a Cancel,
    pub cancel_tx: &'a Arc<watch::Sender<bool>>,
}

/// Ce que rend une passe.
#[derive(Default)]
struct PassOut {
    extra: Vec<Message>,
    content: String,
    new_base: Option<Vec<Message>>,
    echo: Option<ReasoningEcho>,
    saw_usage: bool,
    asked: bool,
    /// Bloc projet rafraîchi en cours de passe (PROJ_SNAPSHOT).
    snap: Option<ProjSnapshot>,
}

fn cap_bool(caps: &Value, k: &str) -> bool {
    caps.get(k).and_then(Value::as_bool).unwrap_or(false)
}

impl ChatService {
    /// `codeVerifyLoop` : rien hors mode Code, sans contrat, pour le
    /// vérificateur ou le planificateur, ni après une question à
    /// l'utilisateur (sa réponse passe avant).
    pub(super) async fn code_verify_loop(
        self: &Arc<Self>,
        pe: &PassEnv<'_>,
        epoch: i64,
        asked: bool,
        run: &mut PassRun<'_>,
    ) {
        let role = pe
            .turn_caps
            .get("Role")
            .and_then(Value::as_str)
            .unwrap_or("");
        if !cap_bool(pe.turn_caps, "Code")
            || role == "verifier"
            || run.cancel.is_cancelled()
            || asked
        {
            return;
        }
        let store = &self.shared.store;
        let mut list = crit_list(store, pe.conv_id).await;
        if list.is_empty() || crit_all_passed(&list) || role == "planner" {
            return; // planification : rien n'a encore été construit
        }
        for _ in 0..CODE_MAX_CONTINUE {
            let open = crit_open(&list);
            if open.is_empty() {
                break;
            }
            let msg = format!(
                "Not done yet — these acceptance criteria are still open:\n{}\n\n\
                 Continue working on them. Once one is done and checked, mark it completed \
                 (criteria action=set, status=completed). If you need the user's decision, use ask.",
                crit_render(&open)
            );
            if self.builder_pass(pe, epoch, msg, run).await || run.cancel.is_cancelled() {
                return; // question posée à l'utilisateur, ou arrêt
            }
            list = crit_list(store, pe.conv_id).await;
            if crit_all_passed(&list) {
                return;
            }
        }
        let conv = self.sessions.conv().clone();
        let mut fix = 0;
        loop {
            if run.cancel.is_cancelled() {
                return;
            }
            self.verify_pass(pe, epoch, run).await;
            list = crit_list(store, pe.conv_id).await;
            if crit_all_passed(&list) {
                conv.append_delta(epoch, obj("verify_done", "passed"));
                return;
            }
            if fix >= CODE_MAX_FIX_TURNS {
                conv.append_delta(epoch, obj("verify_done", "gave_up"));
                // Le fil doit dire où on en est.
                conv.append_delta(
                    epoch,
                    obj(
                        "content",
                        format!(
                            "\n\n_(vérification : {} critère(s) non satisfaits après {CODE_MAX_FIX_TURNS} corrections — voir le panneau des critères)_",
                            crit_pending(&list)
                        ),
                    ),
                );
                return;
            }
            let failed: Vec<Criterion> = list
                .iter()
                .filter(|c| c.status != "passed")
                .cloned()
                .collect();
            let msg = format!(
                "Verification failed on these criteria:\n{}\n\n\
                 Fix the code so they pass. Address the notes precisely; do not touch what already passed. \
                 Mark each fixed criterion completed again.",
                crit_render(&failed)
            );
            if self.builder_pass(pe, epoch, msg, run).await {
                return; // le builder attend une réponse de l'utilisateur
            }
            fix += 1;
        }
    }

    /// `runVerifyPass` : la vérification sur un contexte ISOLÉ — ni rangée dans
    /// l'historique, ni compactée avec lui ; seuls ses deltas d'affichage
    /// sortent.
    async fn verify_pass(self: &Arc<Self>, pe: &PassEnv<'_>, epoch: i64, run: &mut PassRun<'_>) {
        let conv = self.sessions.conv().clone();
        conv.append_delta(epoch, obj("role", "verifier"));
        let list = crit_list(&self.shared.store, pe.conv_id).await;
        let task = conv.with_state(|st| last_user_text(&st.messages));
        let user = format!(
            "Task:\n{task}\n\nAcceptance criteria:\n{}\n\n\
             Verify each non-passed criterion now (git_diff first), and record every verdict with the criteria tool.",
            crit_render(&list)
        );
        let view = self
            .shared
            .store
            .internal_post(
                "/internal/v1/turn/verifier",
                &json!({ "caps": pe.turn_caps }),
            )
            .await;
        match view {
            Ok((s, v)) if s.as_u16() == 200 => {
                let tools: Vec<Tool> =
                    serde_json::from_value(v["tools"].clone()).unwrap_or_default();
                let msgs = vec![
                    Message {
                        role: "system".into(),
                        content: Some(v["system"].clone()),
                        ..Default::default()
                    },
                    Message {
                        role: "user".into(),
                        content: Some(Value::String(user)),
                        ..Default::default()
                    },
                ];
                // Travail annexe : le Go efface le slot à la fin s'il a servi
                // (les requêtes de la passe le lui disent en passant).
                let side = side_job_begin(&self.shared.store).await;
                self.run_pass(
                    pe,
                    epoch,
                    msgs,
                    tools,
                    v["caps"].clone(),
                    true,
                    &[],
                    None,
                    run,
                )
                .await;
                side_job_end(&self.shared.store, side).await;
            }
            other => {
                let e = match other {
                    Ok((_, v)) => v["error"]
                        .as_str()
                        .unwrap_or("vue du vérificateur refusée")
                        .to_string(),
                    Err(e) => e,
                };
                conv.append_delta(epoch, obj("error", e));
            }
        }
        conv.append_delta(epoch, obj("role", ""));
    }

    /// `runBuilderTurn` : le builder relancé sur l'historique normal avec une
    /// consigne ; sa trace est rangée comme un tour ordinaire. `true` : il a
    /// posé une question à l'utilisateur.
    async fn builder_pass(
        self: &Arc<Self>,
        pe: &PassEnv<'_>,
        epoch: i64,
        instruction: String,
        run: &mut PassRun<'_>,
    ) -> bool {
        let conv = self.sessions.conv().clone();
        conv.append_delta(epoch, obj("role", "builder"));
        let asked = self.builder_pass_inner(pe, epoch, instruction, run).await;
        conv.append_delta(epoch, obj("role", ""));
        asked
    }

    async fn builder_pass_inner(
        self: &Arc<Self>,
        pe: &PassEnv<'_>,
        epoch: i64,
        instruction: String,
        run: &mut PassRun<'_>,
    ) -> bool {
        let conv = self.sessions.conv().clone();
        let fix = Message {
            role: "user".into(),
            content: Some(Value::String(instruction)),
            ..Default::default()
        };
        let Some(msgs) = conv.with_state_if(epoch, |st| {
            st.messages.push(fix);
            st.messages.clone()
        }) else {
            return false;
        };
        // Même contexte que le tour de build : sinon le début du prompt change
        // et tout le cache est à refaire (PROJ_SNAPSHOT compris).
        let Assembled {
            sent,
            tools,
            hist,
            snap_turn,
        } = match self.assemble_turn(epoch, pe.turn_caps, msgs).await {
            Ok(a) => a,
            Err(e) => {
                conv.append_delta(epoch, obj("error", e));
                return false;
            }
        };
        let out = self
            .run_pass(
                pe,
                epoch,
                sent,
                tools,
                pe.turn_caps.clone(),
                false,
                &hist,
                snap_turn,
                run,
            )
            .await;
        conv.with_state_if(epoch, |st| {
            if let Some(base) = out.new_base {
                st.messages = base;
            }
            // Même règle que le tour : le bloc rafraîchi en cours de passe.
            if let Some(s) = out.snap {
                st.proj_snap = Some(s);
            }
            st.messages.extend(out.extra);
            if !out.content.trim().is_empty() {
                let a = Message {
                    role: "assistant".into(),
                    content: Some(Value::String(out.content.clone())),
                    ..Default::default()
                };
                st.messages.push(with_echo(a, out.echo.as_ref()));
            }
            st.messages = drop_stray_nudges(std::mem::take(&mut st.messages));
            if out.saw_usage {
                st.ctx_used_len = st.messages.len();
            }
        });
        // En plein tour : l'étape suivante part juste après, l'écriture
        // ne doit pas l'attendre.
        self.sessions.persist_async().await;
        out.asked
    }

    /// Une passe : le tour tourne, ses événements deviennent des deltas
    /// (`forwardStream`). `isolated` : trace à part (vérificateur) — ses
    /// comptes ne disent rien du contexte de la discussion, et sa compaction
    /// éventuelle n'est pas celle de la discussion.
    #[allow(clippy::too_many_arguments)]
    async fn run_pass(
        self: &Arc<Self>,
        pe: &PassEnv<'_>,
        epoch: i64,
        sent: Vec<Message>,
        tools: Vec<Tool>,
        caps: Value,
        isolated: bool,
        base: &[Message],
        snap_turn: Option<SnapTurn>,
        run: &mut PassRun<'_>,
    ) -> PassOut {
        let conv = self.sessions.conv().clone();
        let env = pe.env;
        let local = !env.ep.external;
        let tc = TurnConfig {
            cfg: env.cfg.clone(),
            ep: env.ep.clone(),
            tools: tools.clone(),
            temperature: pe.temperature,
            code: cap_bool(&caps, "Code"),
            reasoning: pe.reasoning.clone(),
            echo: env.echo.clone(),
            compact: env.ccfg.clone(),
            patterns: self.shared.patterns.clone(),
        };
        let host = GoTurnHost {
            shared: self.shared.clone(),
            conv_id: pe.conv_id.to_string(),
            caps,
            cfg: env.cfg.clone(),
            port: env.port,
            summarizer: env.summarizer.clone(),
            compact: TurnCompact {
                cfg: env.ccfg.clone(),
                caps: pe.ccaps,
                criteria: String::new(),
                main_conv: pe.conv_id.to_string(),
                tools,
                effort: pe.reasoning.effort.clone(),
                kwargs: pe.reasoning.kwargs.clone().unwrap_or_default(),
            },
            // Les messages en file attendent la fin du tour (runChatTools sans
            // injection, comme le Go).
            queued: Box::new(Vec::new),
            deltas: {
                let conv = conv.clone();
                Box::new(move |d| {
                    conv.append_delta(conv.epoch(), d);
                })
            },
            // Passe de vérification : comptée à part, comme perfVerify.
            perf_kind: if isolated {
                super::perf::KIND_VERIFY
            } else {
                super::perf::KIND_MAIN
            },
            side_slot: Default::default(),
            projsnap: snap_turn,
            snap_refreshed: Default::default(),
        };
        let mut out = PassOut::default();
        let mut cb = |ev: StreamEvent| -> bool {
            match ev {
                StreamEvent::NewHistory(h) => {
                    if !isolated {
                        out.new_base = Some(history_from_view(&h, base));
                    }
                }
                StreamEvent::Echo(e) => {
                    if !isolated {
                        out.echo = Some(e);
                    }
                }
                StreamEvent::Compacting(b) => {
                    if !isolated {
                        conv.append_delta(epoch, obj("compacting", b));
                    }
                }
                StreamEvent::Err(e) => {
                    conv.append_delta(epoch, obj("error", e));
                }
                StreamEvent::ToolUsed(t) => {
                    out.content.clear(); // déjà dans le message tool_calls
                    conv.append_delta(epoch, tool_used_delta(&t));
                }
                StreamEvent::Ask(a) => {
                    out.asked = true;
                    conv.append_delta(
                        epoch,
                        obj("ask", serde_json::to_value(&a).unwrap_or_default()),
                    );
                }
                StreamEvent::Stats(s) => {
                    let stats = serde_json::to_value(&s).unwrap_or_default();
                    if isolated {
                        let mut d = obj("stats", stats);
                        d.insert("ctx_isolated".into(), Value::Bool(true));
                        conv.append_delta(epoch, d);
                        return true;
                    }
                    if s.prompt_tokens_total > 0 {
                        out.saw_usage = true;
                    }
                    conv.with_state_if(epoch, |st| {
                        if s.prompt_tokens_total > 0 {
                            st.ctx_used = s.ctx_after();
                        }
                        if local {
                            st.gen_peak = st.gen_peak.max(s.gen_tokens);
                        }
                    });
                    conv.append_delta(epoch, obj("stats", stats));
                }
                StreamEvent::DropReasoning => {
                    conv.append_delta(epoch, obj("drop_reasoning", true));
                }
                StreamEvent::Reasoning(r) if !r.is_empty() => {
                    conv.append_delta(epoch, obj("reasoning_content", r));
                }
                StreamEvent::Content(c) if !c.is_empty() => {
                    out.content.push_str(&c);
                    conv.append_delta(epoch, obj("content", c));
                }
                _ => {}
            }
            true
        };
        let res = {
            let turn = run_turn(&host, &tc, sent, run.cancel, &mut cb);
            tokio::pin!(turn);
            tokio::select! {
                r = &mut turn => r,
                sig = run.lease.interrupted() => {
                    if sig == LeaseSignal::Lost {
                        eprintln!("[chat] bail du moteur perdu pendant la vérification : arrêt");
                    }
                    let _ = run.cancel_tx.send(true);
                    turn.await
                }
            }
        };
        out.extra = res.extra;
        out.snap = host.take_refreshed_snap();
        out
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn c(id: i64, status: &str) -> Criterion {
        Criterion {
            id,
            text: format!("critère {id}"),
            status: status.into(),
            note: String::new(),
        }
    }

    #[test]
    fn contrat() {
        let list = vec![c(1, "passed"), c(2, "completed"), c(3, "failed"), c(4, "")];
        assert!(!crit_all_passed(&list) && !crit_all_passed(&[]));
        assert!(crit_all_passed(&[c(1, "passed")]));
        assert_eq!(crit_pending(&list), 3);
        assert_eq!(
            crit_open(&list).iter().map(|c| c.id).collect::<Vec<_>>(),
            [3, 4]
        );
        let mut noted = c(5, "failed");
        noted.note = "le test échoue".into();
        assert_eq!(
            crit_render(&[c(1, "passed"), c(2, "completed"), noted, c(4, "pending")]),
            "[✓] #1 critère 1\n[~] #2 critère 2\n[✗] #5 critère 5 — le test échoue\n[ ] #4 critère 4"
        );
    }

    #[test]
    fn tache_retrouvee() {
        let u = |s: &str| Message {
            role: "user".into(),
            content: Some(Value::String(s.into())),
            ..Default::default()
        };
        let a = Message {
            role: "assistant".into(),
            content: Some(Value::String("ok".into())),
            ..Default::default()
        };
        assert_eq!(
            last_user_text(&[u("fais le jeu"), a.clone()]),
            "fais le jeu"
        );
        assert_eq!(
            last_user_text(&[a]),
            "(tâche non retrouvée — vérifie d'après le diff)"
        );
    }
}
