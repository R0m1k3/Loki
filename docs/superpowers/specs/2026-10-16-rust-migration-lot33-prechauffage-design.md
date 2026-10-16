# Lot 33 — Préchauffage (PREWARM) tenu par la façade

## Contexte

Certains recalculs sont inévitables :

- le prompt réécrit par une compaction ;
- le dernier message assistant rendu autrement au tour suivant ;
- la discussion reprise après une tâche qui a pris le slot.

Sans la clé, ils tombent sur le premier jeton du message suivant.

`PREWARM` (`chat_prewarm.go`) envoie au moteur local, dès qu'il est libre, la
requête du prochain tour telle qu'elle partira, suivie d'une sentinelle « . »
et avec `max_tokens` 1. Le moteur calcule tout le préfixe ; le vrai tour ne
calcule plus que son propre message.

Le préchauffage ne passe jamais devant un vrai travail :

- toute requête de Loki l'annule, sauf un tour qui le prolonge exactement ;
- un seul est en vol à la fois ;
- aucune erreur ne remonte.

La clé gardait la conversation au Go.

## Le lot

La façade tient l'historique : c'est elle qui assemble et envoie. Le Go
garde le compteur des requêtes en vol (`engineGate`) et ce qui annule.

### Go (`facade_prewarm.go`)

- `/internal/v1/prewarm/check` dit si le préchauffage peut partir :
  - clé posée, moteur local ;
  - ni travail annexe, ni bench, ni optimiseur en cours ;
  - loin de minuit ;
  - moteur en bonne santé ;
  - slot visé : un seul slot, ou celui de la discussion avec SIDE_SLOT.
- `/internal/v1/prewarm/begin` :
  - recharge l'état SLOT_PERSIST de la discussion ;
  - inscrit le préchauffage comme LA requête en vol (`prewarmBegin`) ;
  - son annulation (une autre requête part) est relayée à la façade
    (`/internal/facade/v1/chat/prewarm/cancel`).
- `/internal/v1/prewarm/end` :
  - rend la place ;
  - note ce que le moteur a repris (`noteEnginePrompt`).
- `/internal/v1/oai/begin` accepte `prewarm_keep` : l'étape d'un tour de la
  façade qui prolonge le préchauffage ne l'annule pas.
- Après une tâche planifiée, le déclencheur part à la façade
  (`/internal/facade/v1/chat/prewarm`).
- PREWARM ne garde plus la conversation.

### Rust (`chat/prewarm.rs`)

- L'état partagé :
  - le préchauffage en vol, son inscription, son annulation ;
  - les capacités du dernier tour de chaque discussion (64 au plus).
- L'assemblage : les mêmes fonctions que le tour.
  - Vue du Go, historique sans blocs `<context_update>`, préambule.
  - Écho, messages d'envoi, corps.
  - Sentinelle « . » (rôle `builder` en mode Code), non streamé, un seul jeton.
- On s'abstient dans ces cas :
  - capacités inconnues ;
  - tour en cours ;
  - discussion vide ;
  - compaction attendue ;
  - assemblage inattendu.
- Déclencheurs :
  - fin de tour quand rien n'attend dans la file ;
  - fin de tâche (relayée par le Go) ;
  - changement de discussion ou de projet : le préchauffage en cours est
    annulé ; avec `full`, la nouvelle discussion est préparée après 3 s.
- Le tour :
  - `TurnHost::prewarm_keeps` compare les empreintes des messages envoyés
    et des champs qui façonnent le rendu (`prewarmOptsHash`) ;
  - une requête qui ne prolonge pas le préchauffage le coupe tout de suite,
    sans attendre l'aller-retour par le Go.
- Télémétrie : le préchauffage est rangé dans l'anneau (`kind=prewarm`).

## Tests

- `chat/prewarm` :
  - mode (jamais pour un preset externe) ;
  - empreinte des options (l'échantillonnage n'y entre pas, un champ absent
    n'est pas un champ vide) ;
  - rôle de la sentinelle ;
  - prolongement ;
  - état partagé (un seul en vol, garde, annulation, fin) ;
  - capacités bornées.
- La boucle du tour : le drapeau transmis à l'envoi, jamais vers une API
  externe.
- Le vrai hôte (`tests/lot6.rs`) :
  - fin de tour : requête préchauffée (sentinelle, un seul jeton, même
    préfixe et mêmes outils que le tour), inscrite au Go, pas de ticket de
    la façade ;
  - le tour suivant la prolonge et la garde (`prewarm_keep`) ;
  - fin rendue avec total et cache, télémétrie ;
  - refus du Go : rien ne part ;
  - annulation relayée : connexion coupée, place rendue.
- Le Go (`facade_prewarm_test.go`) :
  - contrôle : clé, slots, minuit, travail annexe, moteur muet ;
  - inscription et deuxième préchauffage refusé ;
  - gardé par le tour qui le prolonge, annulé et relayé sinon ;
  - fin unique ;
  - requête déjà en vol ;
  - déclencheur après tâche relayé.

## Écarts assumés

- L'annulation d'un préchauffage pas encore inscrit au Go (aller-retour en
  cours) annule celui qui part, quel que soit son numéro : il n'y en a
  qu'un.
