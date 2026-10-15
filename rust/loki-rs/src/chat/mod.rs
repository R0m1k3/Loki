//! Cœur chat en Rust (lot 6) — bibliothèque, pas encore branchée.
//!
//! Port de `llm_client.go`, `chat_conversation.go`, `chat_compact*.go` :
//! - [`engine`] : transport HTTP vers le moteur (envoi, flux, reprises) ;
//! - [`types`] : types partagés, JSON identique au Go ;
//! - [`wire`] : ce qui part vers le moteur (messages, charge utile) ;
//! - [`stream`] : analyse du flux SSE du moteur (texte, raisonnement, appels
//!   d'outils, statistiques) ;
//! - [`compact`] : estimation du contexte et compactage de l'historique ;
//! - [`journal`] : journal d'affichage (compactage, rejeu, pagination) ;
//! - [`persist`] : format en base, index des discussions, écrivain unique.

pub mod compact;
pub mod engine;
pub mod journal;
pub mod persist;
pub mod stream;
pub mod types;
pub mod wire;
