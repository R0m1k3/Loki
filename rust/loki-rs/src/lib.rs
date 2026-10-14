//! loki-rs — façade Rust de Loki (migration Go → Rust par lots).
//!
//! - `proxy` : façade HTTP/SSE/WebSocket devant le serveur Go ;
//! - `child` : supervision du processus `loki web` (Go) ;
//! - `engine` : supervision du moteur (`loki serve` et son arbre, lot 3) ;
//! - `ui` : UI web statique embarquée (lot 2) ;
//! - `auth` : authentification de l'API de pilotage (lot 2) ;
//! - `update` : mise à jour des binaires au démarrage du conteneur.

pub mod auth;
pub mod child;
pub mod engine;
pub mod proxy;
pub mod ui;
pub mod update;
