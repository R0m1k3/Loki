//! Bail du moteur — le verrou de génération du Go (`conv.Generating`), pris
//! par une conversation tenue par Rust (`web_internal_lease.go`).
//!
//! Le verrou est partagé avec les tâches planifiées, le bench et l'optimiseur,
//! qui restent au Go : un tour Rust doit le tenir du début à la fin, sinon une
//! tâche partirait vers le même moteur en plein tour.
//!
//! [`EngineLease::acquire`] prend le bail et le renouvelle en tâche de fond
//! (au tiers de sa durée). Le Go le rompt (Reset, bascule) ou y signale un
//! stop : [`EngineLease::interrupted`] le dit au tour. Une façade tuée ne
//! renouvelle plus : le Go rend le verrou à l'expiration. [`EngineLease::release`]
//! le rend tout de suite ; un bail lâché sans `release` est rendu en tâche de
//! fond.

use std::time::Duration;

use hyper::StatusCode;
use serde_json::{Value, json};
use tokio::sync::watch;
use tokio::task::JoinHandle;

use crate::store::Store;

/// Durée d'un bail par défaut (`extLeaseDefaultTTL`).
pub const LEASE_TTL: Duration = Duration::from_secs(30);

const PATH_LEASE: &str = "/internal/v1/engine/lease";
const PATH_RENEW: &str = "/internal/v1/engine/lease/renew";
const PATH_RELEASE: &str = "/internal/v1/engine/lease/release";
const PATH_STOP: &str = "/internal/v1/engine/stop";

/// Refus de prendre le bail.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum LeaseError {
    /// Le moteur est tenu (tour, tâche planifiée, bench, optimiseur). `error`
    /// est le message du Go (`busyReason`) ; la tâche est nommée s'il s'agit
    /// d'une tâche planifiée.
    Busy {
        error: String,
        task_id: Option<String>,
        task_name: Option<String>,
    },
    /// Go injoignable ou réponse inattendue.
    Other(String),
}

impl std::fmt::Display for LeaseError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            LeaseError::Busy { error, .. } => f.write_str(error),
            LeaseError::Other(e) => write!(f, "bail du moteur : {e}"),
        }
    }
}

/// État du bail vu du tour.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum LeaseSignal {
    Held,
    /// Stop demandé côté Go : le tour s'arrête, le bail tient jusqu'à `release`.
    Stop,
    /// Bail rompu (Reset ou bascule côté Go, ou expiré) : le verrou n'est plus
    /// à nous.
    Lost,
}

/// Le bail tenu ; rendu par [`Self::release`] ou à la destruction.
pub struct EngineLease {
    id: String,
    store: Store,
    signal: watch::Receiver<LeaseSignal>,
    renew: JoinHandle<()>,
    released: bool,
}

impl EngineLease {
    /// Prend le verrou de génération pour `ttl` (borné par le Go à 5 s – 5 min).
    pub async fn acquire(store: Store, ttl: Duration) -> Result<Self, LeaseError> {
        let body = json!({ "ttl_ms": ttl.as_millis() as u64 });
        let (status, v) = store
            .internal_post(PATH_LEASE, &body)
            .await
            .map_err(LeaseError::Other)?;
        match status {
            StatusCode::OK => {}
            StatusCode::CONFLICT => {
                let s = |k: &str| v.get(k).and_then(Value::as_str).map(str::to_string);
                return Err(LeaseError::Busy {
                    error: s("error").unwrap_or_else(|| "génération en cours".into()),
                    task_id: s("task_id"),
                    task_name: s("task_name"),
                });
            }
            s => return Err(LeaseError::Other(format!("{PATH_LEASE} : {s} {v}"))),
        }
        let id = v
            .get("lease")
            .and_then(Value::as_str)
            .filter(|s| !s.is_empty())
            .ok_or_else(|| LeaseError::Other(format!("{PATH_LEASE} : réponse sans bail {v}")))?
            .to_string();
        let granted = v
            .get("ttl_ms")
            .and_then(Value::as_u64)
            .map(Duration::from_millis)
            .unwrap_or(ttl);
        let (tx, rx) = watch::channel(LeaseSignal::Held);
        let renew = tokio::spawn(renew_loop(store.clone(), id.clone(), granted / 3, tx));
        Ok(EngineLease {
            id,
            store,
            signal: rx,
            renew,
            released: false,
        })
    }

    pub fn id(&self) -> &str {
        &self.id
    }

    /// État courant (dernier renouvellement).
    pub fn signal(&self) -> LeaseSignal {
        *self.signal.borrow()
    }

    /// Se résout au premier stop ou à la perte du bail, et rend lequel.
    pub async fn interrupted(&mut self) -> LeaseSignal {
        match self.signal.wait_for(|s| *s != LeaseSignal::Held).await {
            Ok(s) => *s,
            // Tâche de renouvellement terminée sans signal : rien à attendre.
            Err(_) => std::future::pending().await,
        }
    }

    /// Rend le verrou maintenant.
    pub async fn release(mut self) {
        self.released = true;
        self.renew.abort();
        release(&self.store, &self.id).await;
    }
}

impl Drop for EngineLease {
    fn drop(&mut self) {
        self.renew.abort();
        if self.released {
            return;
        }
        // Lâché sans release (tour annulé, panique) : rendu en tâche de fond.
        // Hors runtime, l'expiration côté Go s'en charge.
        if let Ok(rt) = tokio::runtime::Handle::try_current() {
            let (store, id) = (self.store.clone(), std::mem::take(&mut self.id));
            rt.spawn(async move { release(&store, &id).await });
        }
    }
}

async fn release(store: &Store, id: &str) {
    if let Err(e) = store
        .internal_post(PATH_RELEASE, &json!({ "lease": id }))
        .await
    {
        eprintln!("[lease] libération du bail échouée (le Go le rendra à l'expiration) : {e}");
    }
}

/// Renouvelle jusqu'à la perte du bail. Une erreur réseau n'arrête pas la
/// boucle : le prochain essai peut passer avant l'expiration.
async fn renew_loop(store: Store, id: String, every: Duration, tx: watch::Sender<LeaseSignal>) {
    let mut tick = tokio::time::interval(every.max(Duration::from_millis(100)));
    tick.tick().await; // le premier top est immédiat : le bail vient d'être pris
    loop {
        tick.tick().await;
        match store
            .internal_post(PATH_RENEW, &json!({ "lease": id }))
            .await
        {
            Ok((StatusCode::OK, v)) => {
                if v.get("stop").and_then(Value::as_bool) == Some(true) {
                    tx.send_if_modified(|s| {
                        let was = *s;
                        if was == LeaseSignal::Held {
                            *s = LeaseSignal::Stop;
                        }
                        was == LeaseSignal::Held
                    });
                }
            }
            Ok((StatusCode::GONE, _)) => {
                tx.send_replace(LeaseSignal::Lost);
                return;
            }
            Ok((s, v)) => eprintln!("[lease] renouvellement : {s} {v}"),
            Err(e) => eprintln!("[lease] renouvellement : {e}"),
        }
    }
}

/// Arrête le détenteur Go du moteur (tâche planifiée, bench, optimiseur) :
/// le bouton stop du chat, quand ce n'est pas un tour Rust qui tourne.
pub async fn engine_stop(store: &Store) -> Result<(), String> {
    match store.internal_post(PATH_STOP, &json!({})).await? {
        (StatusCode::NO_CONTENT, _) => Ok(()),
        (s, v) => Err(format!("{PATH_STOP} : {s} {v}")),
    }
}
