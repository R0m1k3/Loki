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
//! - [`conv`] : la discussion en mémoire (état, epoch, verrou du tour,
//!   abonnés) ;
//! - [`persist`] : format en base, index des discussions, écrivain unique ;
//! - [`lease`] : bail du moteur (verrou de génération partagé avec le Go).

pub mod compact;
pub mod conv;
pub mod engine;
pub mod journal;
pub mod lease;
pub mod persist;
pub mod stream;
pub mod types;
pub mod wire;
