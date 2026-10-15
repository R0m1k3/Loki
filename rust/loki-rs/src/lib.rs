//! loki-rs — façade Rust de Loki (migration Go → Rust par lots).
//!
//! - `proxy` : façade HTTP/SSE/WebSocket devant le serveur Go ;
//! - `child` : supervision du processus `loki web` (Go) ;
//! - `engine` : supervision du moteur (`loki serve` et son arbre, lot 3) ;
//! - `ui` : UI web statique embarquée (lot 2) ;
//! - `auth` : authentification de l'API de pilotage (lot 2) ;
//! - `oai` : surface compatible OpenAI `/v1` relayée au moteur (lot 5) ;
//! - `chat` : cœur chat (lot 6) ; `chatapi` : ses routes, servies quand la
//!   conversation est tenue par la façade (`LOKI_CHAT=rust`) ;
//! - `update` : mise à jour des binaires au démarrage du conteneur.

pub mod api;
pub mod auth;
pub mod chat;
pub mod chatapi;
pub mod child;
pub mod convfiles;
pub mod dictate;
pub mod downloads;
pub mod engine;
pub mod engstatus;
pub mod gguf;
pub mod gojson;
pub mod hf;
pub mod memory;
pub mod models;
pub mod oai;
pub mod paths;
pub mod presets;
pub mod presets_api;
pub mod proxy;
pub mod push;
pub mod serve;
pub mod settings;
pub mod store;
pub mod sysinfo;
pub mod tools;
pub mod ui;
pub mod update;
