# Lot 12 — notifications Web Push tenues par la façade

## Contexte

Les notifications « réponse prête » vivaient dans le Go : `push.go`,
`web_push.go` et webpush-go. Lot 12 : la façade les tient quand
`LOKI_PUSH=rust` (`push.rs`, expérimental ; sans la variable rien ne change).

| Route | Rôle |
|---|---|
| `GET /api/push/key` | clé publique VAPID, générée au premier appel puis stable |
| `POST /api/push/subscribe` | enregistre un abonnement (idempotent par endpoint, clés mises à jour) |
| `POST /api/push/unsubscribe` | le retire |
| `POST /internal/facade/v1/push/send` | (le Go, jeton interne) pousse une notification à tous les abonnés, en tâche de fond |

Les envois du Go passent tous par `sendPushToAll` : fin de tour du chat Go,
fin de tour tenu par la façade (`/internal/v1/turn/done`), fin de tâche. Ils
sont délégués à la façade. Clés et abonnements restent en base aux mêmes
clés (`push_vapid_priv`, `push_vapid_pub`, `push_subs`).

Chiffrement RFC 8291 (`aes128gcm`), enregistrement complété à 4096 octets
comme webpush-go. Signature VAPID RFC 8292 (JWT ES256, 12 h, sujet
« mailto: »). TTL de 120 s, urgence haute. Un abonnement refusé pour de bon
(404/410) est purgé. La cryptographie passe par ring, déjà présent via
rustls. Une façade construite sans TLS répond que les notifications sont
indisponibles.

## Écarts assumés

- Messages des journaux d'envoi au format des crates Rust.

## Parité

- `internal/loki/parity_push_test.go` tire du Go :
  - les durées du corps de notification ;
  - onze appels de route (clé, abonnements valides et refusés, mise à jour
    des clés, désabonnements), avec la liste en base après chacun.

  `tests/push.rs` rejoue le tout par la façade. La liste en base est
  comparée à l'octet près, et la clé reste la même d'un appel à l'autre.
- Le chiffrement reproduit l'exemple de l'annexe A de la RFC 8291 à l'octet
  près.
- Un envoi de bout en bout vers un faux service de push vérifie :
  - les en-têtes ;
  - la signature VAPID, contrôlée avec la clé rangée ;
  - le corps, déchiffré côté navigateur avec la clé de l'abonnement ;
  - l'abonnement en 410, purgé.
