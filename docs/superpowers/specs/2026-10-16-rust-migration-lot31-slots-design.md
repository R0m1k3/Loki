# Lot 31 — Slots du moteur pour le tour tenu par la façade

## Contexte

Trois clés opt-in du moteur local gardaient la conversation au Go
(`facadeChatBlocker`), faute d'être honorées par la façade :

- `SIDE_SLOT` (`llm_sideslot.go`) : un second slot pour les travaux annexes.
  Chaque requête de Loki porte son `id_slot` : 0 pour le fil de la
  discussion (étape d'un tour, résumé en continuation), 1 pour le reste
  (vérification, résumé sur transcription). Un cache KV à court de place en
  plein calcul est rejoué tel quel, sans compaction : c'est la faute du
  voisin, pas un prompt trop long.
- `SLOT_PERSIST` (`llm_slotpersist.go`) : l'état du slot de la discussion est
  gardé sur disque à la bascule de preset, et rechargé avant la première
  requête de la discussion au retour.

Et l'isolation des travaux annexes (`engineSideJob`, `CACHE_ISOLATE`) : après
la passe de vérification du mode Code, le Go efface le slot local s'il a
servi. La façade la sautait (écart du lot 7), le Go ne voyant pas passer ses
requêtes.

Depuis la fusion de `main`, le Go voit passer chaque requête de la façade au
moteur local (tickets « en vol », `/internal/v1/oai/begin`, `served`, `end`).
La machinerie peut donc rester au Go ; il suffit que la façade la consulte.

## Le lot

### Go

- `/internal/v1/turn/slot` (`facade_slots.go`) :
  - entrée : `{"conv":"…","main":true}` ;
  - sortie : `{"slot":0|1|-1}`, la valeur d'`engineSlotFor` ;
  - avec `main` et `conv`, recharge d'abord l'état gardé de la discussion
    (`slotPersistRestore`), comme la boucle Go avant une requête d'un tour.
- La sauvegarde à la bascule ne change pas. Le slot porte la discussion dès
  que le moteur accepte une étape de son tour : `/internal/v1/oai/served`
  appelle `engineMarkMain`.
- `SIDE_SLOT` et `SLOT_PERSIST` ne gardent plus la conversation.

### Rust

- `TurnHost::engine_slot`, appelé avant chaque requête. La boucle pose
  l'`id_slot` rendu dans le corps.
- Le rejeu « cache KV plein » (`sideSlotPoolError`) n'a lieu qu'avec un slot
  en service et un moteur local :
  - deux fois au plus, après l'attente des reprises réseau ;
  - puis l'erreur telle quelle ;
  - ni compaction ni réduction.
- `GoTurnHost` :
  - moteur externe : aucun slot ;
  - étape d'un tour : `main` avec la discussion (donc le rechargement
    `SLOT_PERSIST`) ;
  - vérification : le second slot, et le ticket « en vol » ouvert sur lui
    (`side`).
- `EngineSummarizer` : un résumé est désormais compté en vol, comme
  `postSummary`. Il part sur le second slot s'il est fait sur la
  transcription, sur le slot 0 s'il est fait en continuation.
- La passe de vérification est encadrée par `/internal/v1/engine/sidejob`.
  À la fin, le Go efface le slot si le moteur a servi.

## Tests

- Go (`facade_slots_test.go`) :
  - sans la clé, aucun slot et `/props` jamais lu ;
  - fil de la discussion sur le 0, le reste sur le 1 ;
  - second slot hors service ;
  - rechargement une seule fois, pour la bonne discussion et une étape de tour
    seulement ;
  - une étape de la façade acceptée par le moteur suffit à la sauvegarde ;
  - corps illisible, GET refusé.
- La boucle du tour :
  - `id_slot` dans chaque requête, slot demandé avant chaque envoi ;
  - cache KV plein : rejoué deux fois sans compaction, puis l'erreur ;
  - sans slot, ou moteur externe : un débordement comme un autre ;
  - motif `sideSlotPoolError` repris des cas du Go.
- Le vrai hôte (`tests/lot6.rs`) :
  - contrat du mode Code : le tour sur le 0, la vérification sur le 1 ;
  - tickets ouverts sur le second slot, travail annexe ouvert puis rendu ;
  - résumés : transcription sur le 1, continuation sur le 0, tickets rendus.

## Écarts assumés

- Le rejeu « cache KV plein » se fie au slot demandé pour cette requête. Le Go
  relit `sideSlotLive` au moment de l'erreur ; les deux lectures partagent
  le même cache de dix secondes.
- Le slot est demandé avant de composer la requête, et non juste avant de
  l'envoyer : rien ne se passe entre les deux.
