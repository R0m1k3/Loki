# Lot 38 — L'outil criteria du mode Code dans la façade

## Contexte

En mode Code, le modèle tient les critères d'acceptation de sa tâche avec
l'outil `criteria` :

- les poser ;
- les marquer faits ;
- les lister ;
- les effacer.

La passe de vérification les marque ensuite `passed` ou `failed`.

Depuis le lot 19, la façade sert déjà les critères de l'écran
(`/api/chat/criteria`, `chat/crit:<id>` scellés). La passe de vérification
les relit depuis le lot 7. Mais l'outil lui-même partait au Go
(`/internal/v1/tool/exec`), qui le renvoyait à la façade comme delta
(`critNotify` → `/internal/facade/v1/delta`).

## Le lot

`chat/crittool.rs` porte `toolCriteria`, mêmes messages au caractère près :

- **list** : action vide ou d'un autre type comprise ;
- **add** :
  - textes vides ou d'un autre type ignorés ;
  - ids repris après le plus grand ;
  - vingt critères au plus ;
- **set** :
  - id numérique (tronqué) ou en chaîne (« "2" », « "#2" », « +1 ») ;
  - statut libre, note rognée ;
  - `passed` refusé hors de la passe de vérification ;
- **clear**.

Les règles de rangement et d'affichage :

- une liste vide supprime la clé ; une erreur d'écriture est ignorée, comme
  dans le Go ;
- une liste rangée illisible vaut « aucun critère » ;
- la note vide n'est pas écrite (`omitempty`).

`GoTurnHost::exec_tool` l'exécute pour la discussion du tour
(`main_conv`). La passe de vérification (`Role` = verifier) est la seule à
pouvoir marquer `passed`. Chaque changement pousse la liste à l'écran (delta
`criteria` + `ts`) par un nouveau champ `deltas` de l'hôte :

- le journal de la discussion pour un tour ;
- celui de la conversation pour une passe de vérification.

Écart assumé : le Go rangeait les critères sous la discussion **active**
au moment de l'appel (`convEnsureActive`). La façade les range sous celle du
tour. Elles ne diffèrent que si l'utilisateur change de discussion pendant
la génération, et c'est alors la façade qui a raison.

`see_image` reste au Go. Il redresse et réduit l'image (EXIF, taille) avant
de l'encoder, ce qu'un port ne rendrait pas octet pour octet.

## Parité

`internal/loki/parity_criteria_test.go` tire du Go un journal de 22 appels
(`tests/parity_tools/criteria.json`). Chaque étape porte :

- les arguments, builder ou vérification ;
- le résultat ;
- les critères rangés ensuite.

`tests/tools.rs` (`criteria_comme_le_go`) le rejoue contre le port. Il
vérifie aussi :

- le rangement scellé ;
- la liste poussée à l'écran à chaque changement, et seulement alors.

`tests/lot6.rs` (`contrat_verifie_par_la_facade`) fait tourner un vrai
contrat. La passe de vérification marque le critère `passed` dans la
façade : rien ne passe par le Go, le critère est rangé scellé, l'écran
reçoit la liste.
