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
//! - [`sessions`] : plusieurs discussions (index, bascules, chargement) ;
//! - [`lease`] : bail du moteur (verrou de génération partagé avec le Go) ;
//! - [`nudges`] : budget d'outils, rappels, appels répétés, aperçus ;
//! - [`turn`] : la boucle d'un tour (complétions, outils, filets) ;
//! - [`host`] : le tour branché sur le moteur, les outils du Go et la base ;
//! - [`history`] : début et fin d'un tour côté historique ;
//! - [`generate`] : un tour de bout en bout (envoi, file, fin de tour).

pub mod compact;
pub mod conv;
pub mod engine;
pub mod generate;
pub mod history;
pub mod host;
pub mod journal;
pub mod lease;
pub mod nudges;
pub mod persist;
pub mod sessions;
pub mod stream;
pub mod turn;
pub mod types;
pub mod verify;
pub mod wire;
