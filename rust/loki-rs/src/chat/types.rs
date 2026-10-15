//! Types partagés du chat — sérialisation JSON identique aux types Go
//! (`llm_client.go`, `chat_conversation.go`, `chat_diff.go`).
//!
//! Règles de fidélité : `omitempty` Go → `skip_serializing_if` ; un champ Go
//! `any` (`Content`) → `Option<Value>` (nil omis, "" gardé : en Go, omitempty
//! n'omet une interface que si elle est nil).

use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};

fn is_false(b: &bool) -> bool {
    !*b
}

/// Un message de l'historique (vue du modèle).
#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize)]
pub struct Message {
    pub role: String,
    /// Texte, ou tableau de parties (texte + image_url) pour un message multimodal.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub content: Option<Value>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tool_calls: Vec<ToolCall>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub tool_call_id: String,
    /// Raisonnement séparé gardé avec REASONING_ECHO=on.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub reasoning_content: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub reasoning_model: String,
    /// Relais d'image d'un outil (KEEP_TURN_IMAGES) ; retiré à l'envoi.
    #[serde(default, skip_serializing_if = "is_false")]
    pub img_relay: bool,
    /// Bloc <context_update> posé par Loki (PROJ_SNAPSHOT) ; retiré à l'envoi.
    #[serde(default, skip_serializing_if = "is_false")]
    pub ctx_upd: bool,
    /// Coût mesuré d'une image gardée (KEEP_TURN_IMAGES) — en mémoire seulement.
    #[serde(skip)]
    pub img_tokens: i64,
}

#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize)]
pub struct ToolCall {
    /// Position dans un flux (delta OpenAI) ; absente de l'historique.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub index: Option<i64>,
    #[serde(default)]
    pub id: String,
    #[serde(default, rename = "type")]
    pub kind: String,
    #[serde(default)]
    pub function: ToolCallFunc,
}

#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize)]
pub struct ToolCallFunc {
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub arguments: String,
}

/// Définition d'un outil annoncée au modèle (schéma OpenAI).
#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize)]
pub struct Tool {
    #[serde(rename = "type")]
    pub kind: String,
    pub function: ToolFunction,
}

#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize)]
pub struct ToolFunction {
    pub name: String,
    pub description: String,
    /// `any` en Go : `null` si absent.
    pub parameters: Value,
}

/// Question structurée posée à l'utilisateur (outil `ask`).
#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize)]
pub struct AskEvent {
    pub question: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub options: Vec<String>,
}

#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
pub struct DiffLine {
    pub op: String,
    pub text: String,
}

/// Appel d'outil annoncé, en cours d'écriture, ou terminé (`ToolUsedEvent`).
/// Pas de JSON propre en Go : le journal en construit un objet à la main.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct ToolUsedEvent {
    pub name: String,
    pub label: String,
    pub result: String,
    pub done: bool,
    pub typing: bool,
    pub body: String,
    pub body_tail: bool,
    pub body_lines: i64,
    pub diff: Vec<DiffLine>,
    pub added: i64,
    pub removed: i64,
    pub image: String,
    pub shot: String,
    pub result_chars: i64,
    pub result_id: String,
}

/// Mesures llama.cpp d'une complétion (`StatsEvent`).
#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize)]
pub struct StatsEvent {
    #[serde(default, skip_serializing_if = "is_zero_i64")]
    pub prompt_tokens: i64,
    #[serde(default, skip_serializing_if = "is_zero_f64")]
    pub prompt_per_second: f64,
    #[serde(default, skip_serializing_if = "is_zero_f64")]
    pub prompt_ms: f64,
    #[serde(default, skip_serializing_if = "is_zero_i64")]
    pub gen_tokens: i64,
    #[serde(default, skip_serializing_if = "is_zero_f64")]
    pub gen_per_second: f64,
    #[serde(default, skip_serializing_if = "is_zero_f64")]
    pub gen_ms: f64,
    #[serde(default, skip_serializing_if = "is_zero_i64")]
    pub prompt_tokens_total: i64,
    #[serde(default, skip_serializing_if = "is_zero_i64")]
    pub reasoning_tokens: i64,
    /// Ce raisonnement repart dans la requête suivante (REASONING_ECHO).
    #[serde(skip)]
    pub echoed: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub cache_hint: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cache_tokens: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub draft_n: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub draft_accepted: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub ttft_ms: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub lost: Option<i64>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub lost_after: String,
    #[serde(default, skip_serializing_if = "is_false")]
    pub lost_alert: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub kind: String,
}

fn is_zero_i64(v: &i64) -> bool {
    *v == 0
}
fn is_zero_f64(v: &f64) -> bool {
    *v == 0.0
}

impl StatsEvent {
    /// `ctxAfter` : contexte que la requête SUIVANTE renverra au moteur — le
    /// prompt plus ce qui a été généré, moins le raisonnement jamais renvoyé.
    pub fn ctx_after(&self) -> i64 {
        if self.echoed {
            return self.prompt_tokens_total + self.gen_tokens;
        }
        self.prompt_tokens_total + self.gen_tokens - self.gen_tokens.min(self.reasoning_tokens)
    }
}

/// Raisonnement de la complétion finale, à ranger avec la réponse.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct ReasoningEcho {
    pub text: String,
    pub model: String,
}

/// Ce que reçoit le rappel d'une boucle de chat (`StreamEvent` Go, dont un
/// seul champ est posé par appel).
#[derive(Debug, Clone, PartialEq)]
pub enum StreamEvent {
    Content(String),
    Reasoning(String),
    ToolUsed(ToolUsedEvent),
    Stats(StatsEvent),
    Ask(AskEvent),
    Err(String),
    /// Retirer la dernière bulle de raisonnement (tour relancé).
    DropReasoning,
    /// Compactage en cours de tour : début (true) / fin (false).
    Compacting(bool),
    /// Vue modèle APRÈS un compactage en cours de tour — à substituer.
    NewHistory(Vec<Message>),
    Echo(ReasoningEcho),
}

/// Événement du journal d'affichage (`LogEvent`).
#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize)]
pub struct LogEvent {
    pub seq: i64,
    pub ts: i64,
    /// Objet JSON (ordre des clés libre : l'UI les lit par nom).
    pub delta: Map<String, Value>,
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn message_json_comme_le_go() {
        let m = Message {
            role: "assistant".into(),
            tool_calls: vec![ToolCall {
                id: "c1".into(),
                kind: "function".into(),
                function: ToolCallFunc {
                    name: "bash".into(),
                    arguments: "{}".into(),
                },
                ..Default::default()
            }],
            ..Default::default()
        };
        assert_eq!(
            serde_json::to_value(&m).unwrap(),
            json!({"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"bash","arguments":"{}"}}]})
        );
        // Contenu vide mais présent : gardé (interface non nil en Go).
        let u = Message {
            role: "user".into(),
            content: Some(json!("")),
            ..Default::default()
        };
        assert_eq!(
            serde_json::to_string(&u).unwrap(),
            r#"{"role":"user","content":""}"#
        );
    }

    #[test]
    fn contexte_apres() {
        let s = StatsEvent {
            prompt_tokens_total: 1000,
            gen_tokens: 300,
            reasoning_tokens: 500,
            ..Default::default()
        };
        assert_eq!(s.ctx_after(), 1000);
        let e = StatsEvent { echoed: true, ..s };
        assert_eq!(e.ctx_after(), 1300);
    }
}
