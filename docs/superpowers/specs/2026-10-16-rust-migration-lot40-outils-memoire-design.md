# Lot 40 — Outils mémoire de l'agent dans la façade

## Contexte

Depuis le lot 13, la façade tient la mémoire quand `LOKI_MEM=rust` :

- pages ;
- index `MEMORY.md` ;
- trackers ;
- mode mémoire.

Les outils par lesquels le modèle s'en sert partaient pourtant au Go
(`/internal/v1/tool/exec`) :

- `mem_search`, `mem_read`, `mem_add`, `mem_edit` ;
- `tracker`.

Deux processus écrivaient donc les mêmes pages et le même index.

## Le lot

`src/memory/tools.rs` porte les outils (`chat_memory.go`, `tracker.go`),
mêmes résultats au caractère près. Il réutilise les lectures et écritures des
routes de la mémoire :

- pages scellées selon le chiffrement ;
- `.bak` de la version précédente ;
- écriture vérifiée ;
- index tenu par le code.

Chaque outil :

- **`mem_search`** : classement par couverture des termes puis par
  pertinence (rareté IDF, fréquence amortie, bonus du nom et du titre) ;
  extrait coupé à l'octet comme le Go (U+FFFD par octet d'un caractère
  coupé) ; limite 8 par défaut, 30 au plus ;
- **`mem_read`** : lignes numérotées, plage, 500 au plus ;
- **`mem_add`** : jamais par-dessus une page existante, index tenu (seed au
  nom du projet), diff d'ajout ;
- **`mem_edit`** : `old` unique, édition déjà en place reconnue, diff ;
- **`tracker`** :
  - vue d'ensemble ;
  - squelette (années, année en cours par mois) ;
  - une année par mois ;
  - points (60 plus récents au plus) ;
  - ajout, modification, suppression d'un point (jamais d'un tracker
    entier) ;
  - arguments d'un autre type rendus comme `str` du Go.

`tracker_add` prend désormais l'heure en paramètre (`tracker_add_at`).

`ChatShared.memory` porte la même mémoire que les routes. `GoTurnHost::exec_tool`
y exécute les outils quand la façade la tient ; sinon l'appel part au Go
comme avant.

Côté Go, l'heure des trackers (« maintenant », année en cours) passe par
`trackerNow`, que le journal fixe ; rien ne change en service.

## Parité

`internal/loki/parity_mem_tools_test.go` tire du Go un journal de 57 appels
(`tests/parity_mem/outils.json`), en fuseau UTC et à heure fixée. Chaque
étape porte :

- les arguments ;
- le libellé du tour ;
- le résultat, le diff ;
- les fichiers de la mémoire du projet (`.bak` et index compris) ;
- les trackers en base.

Il couvre :

- recherche : couverture, rareté, requête vide, limites, extrait coupé ;
- lecture : plage, au-delà, page absente, nom invalide ;
- ajout : existant, nom invalide, majuscules ;
- modification : rejouée, déjà en place, absente, en double, vide ;
- trackers : chaque vue, dates illisibles, nom en nombre, identifiants
  inconnus.

`tests/memory.rs` (`outils_memoire_comme_le_go`) le rejoue contre le port.

`tests/lot6.rs` (`outil_memoire_par_la_facade`) fait tourner un tour de la
façade qui crée une page. Il vérifie :

- rien ne passe par le Go ;
- la page est rangée dans le projet actif, l'index tenu ;
- le résultat est celui que le modèle reçoit.
