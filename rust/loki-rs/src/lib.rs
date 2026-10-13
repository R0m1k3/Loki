//! loki-rs — façade Rust de Loki (lot 1 de la migration Go → Rust).
//!
//! - `proxy` : façade HTTP/SSE/WebSocket devant le serveur Go ;
//! - `child` : supervision du processus `loki web` (Go) ;
//! - `update` : mise à jour des binaires au démarrage du conteneur.

pub mod child;
pub mod proxy;
pub mod update;
