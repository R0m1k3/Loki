//! Optimiseur sans perte tenu par la façade (lot 28 de la migration,
//! `LOKI_TUNE=rust` ; expérimental, le Go reste le défaut) — port de
//! `backend_tune*.go`.
//!
//! - [`core`] : la partie pure (essais, garde-fous, score, réécriture du
//!   preset) ;
//! - [`lock`] : le verrou de LOKI_HOME, au format du Go ;
//! - [`job`] : les routes, le job, les essais et l'application.

pub mod core;
pub mod job;
pub mod lock;

#[cfg(test)]
mod tests;
