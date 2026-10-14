# Migration Rust — lot 3 : supervision du moteur

Date : 2026-10-14 · Statut : livré · Prérequis : lots 1-2.

## Découpage

Le « moteur » en conteneur, c'est deux couches de taille très différente :

1. **La supervision du processus** (~350 lignes Go : `sys_service_container.go`,
   `sys_proctree_linux.go`) — lancer `loki serve` détaché, fichier PID,
   journal, arrêter tout l'arbre (Strata compris) et attendre que la VRAM
   soit rendue.
2. **La ligne de commande** de llama-server / Strata (~9 000 lignes :
   `backend_serve*.go`, presets, MoE, spéculatif, threads, cache…), là où
   s'applique la règle « ne jamais dénaturer les modèles ».

Le lot 3 porte la couche 1. La couche 2 reste au Go (`loki serve`) et fera
l'objet d'un lot dédié, parce que chaque option y a été réglée contre un
défaut mesuré.

## Ce qui change

- `loki-rs engine start|stop|restart|status` : port à l'identique de la
  supervision Go — chef de session (`setsid`), fichier PID et journal aux
  **mêmes chemins** (`$LOKI_HOME/<service>.pid|.log`), contrôle à 2 s avec
  les 20 dernières lignes du journal en cas d'échec, arrêt de l'arbre
  (session, groupe, filiation ; SIGTERM, 4 s, SIGKILL, 20 s au plus), mêmes
  messages.
- Le Go garde ses garde-fous (optimisation en cours via `serviceAction`,
  preset externe) puis délègue au `loki-rs` voisin de son binaire.
  `loki logs`, `enable`/`disable` restent au Go.
- Porte de sortie : `LOKI_ENGINE_SUPERVISOR=go`, ou absence de `loki-rs`.
- Différence assumée : le contrôle « démarré ? » regarde si l'enfant est
  sorti (`try_wait`), pas le signal 0 — un enfant mort reste un zombie, qui
  répond encore au signal 0.
- L'arrêt exclut de l'arbre `loki-rs` lui-même ET son parent (le Go qui l'a
  appelé), comme le Go excluait son propre PID.

## Tests

- Rust (toutes plateformes) : `/proc/<pid>/stat` avec parenthèses dans le
  nom, arbre par session/groupe/filiation avec exclusion de l'appelant.
- Rust (Linux, CI) : faux `loki serve` qui lance un enfant dans sa session et
  un « moteur » dans SA PROPRE session (comme Strata) — démarrage, « déjà
  démarré », arrêt de tout l'arbre, redémarrage, échec avec journal, PID
  nettoyé.
- Go (Linux, CI) : choix du `loki-rs` voisin (absent, non exécutable,
  `LOKI_ENGINE_SUPERVISOR=go`), appel réel avec l'environnement attendu,
  erreur rendue = dernière ligne d'erreur du superviseur.
