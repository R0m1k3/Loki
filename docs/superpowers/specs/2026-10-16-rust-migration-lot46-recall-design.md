# Lot 46 — Outils recall et recall_search dans la façade

## Contexte

La compaction résume les vieux tours d'une discussion. Avant de résumer un
gros bloc (un code, une page, un diff), elle l'archive verbatim sous un id
court (`r7`) que le résumé cite. L'agent a deux outils pour le retrouver :

- `recall(id)` rend le bloc exact ;
- `recall_search(query)` retrouve un bloc et son id par mots-clés, même
  sorti du résumé courant.

La façade compacte elle-même et archive par le Go
(`/internal/v1/recall/archive`, lot 6) : l'id vient de la séquence de la
base, que seul le Go tient. Les deux outils passaient encore par
`/internal/v1/tool/exec`.

## Le lot

`src/recall.rs` porte `toolRecall` et `toolRecallSearch`
(`chat_recall.go`) :

- **l'id tolérant** (`recallIDToSeq`) :
  - espaces, un préfixe `r` puis un préfixe `R`, espaces encore ;
  - des chiffres seulement : ni signe, ni dépassement de 64 bits ;
  - un id mal formé ne lit pas l'archive ;
- **les mots-clés** (`recallTokens`) :
  - mise en minuscules rune par rune, comme `strings.ToLower` (`İ`
    donne `i`, `Σ` donne `σ`) ;
  - coupés sur tout ce qui n'est ni `a`–`z`, ni `0`–`9`, ni `à`–`ÿ` ;
  - deux runes au moins ;
- **le score** :
  - un point par terme présent dans le contenu, deux dans le libellé ;
  - à égalité, le plus récent d'abord ;
  - 5 résultats par défaut, 20 au plus, limite tronquée comme
    `int(float64)` ;
- **les textes**, mêmes que le Go :
  - id ou requête manquants ;
  - bloc introuvable (id cité par `%q`) ;
  - bloc rappelé verbatim ;
  - liste des résultats avec un extrait d'une ligne de 160 runes ;
  - aucun résultat.

L'archive reste au Go, qui la sert à la façade :

- `GET /internal/v1/recall/block?seq=N` : le bloc (200), ou 404 s'il est
  absent ou chiffré sous une mémoire verrouillée ;
- `GET /internal/v1/recall/blocks` : tous les blocs lisibles, par séquence
  croissante.

`recallGet` et `recallSearch` du Go passent par les mêmes lectures
(`recallGetSeq`, `recallAll`).

`GoTurnHost::exec_tool` exécute les deux outils avant le repli sur le Go.
Si le Go ne répond pas, l'appel passe par lui.

## Parité

`internal/loki/parity_recall_test.go` archive 35 blocs variés :

- accents et casse Unicode ;
- balises et entités ;
- bloc vide ;
- premières lignes longues ;
- vingt-cinq blocs semblables.

Il tire ensuite du Go un journal de 41 appels
(`tests/parity_recall/recall.json`), avec les blocs tels que l'archive les
sert :

- `recall` :
  - id absent, non texte, blanc ;
  - variantes de préfixe et d'espaces ;
  - zéros de tête ;
  - mal formé, négatif, signe plus, zéro, absent, démesuré ;
  - guillemets dans l'id ;
- `recall_search` :
  - requête absente, non texte, blanche ;
  - mots trop courts ;
  - casse, accents, `İ`, grec hors des mots ;
  - souligné coupé ;
  - extraits bornés ;
  - limites : une, tronquée, négative, par défaut, au plus, en texte
    ignorée ;
  - aucun résultat ;
  - ponctuation.

`tests/recall.rs` le rejoue : le faux Go sert les mêmes blocs. Il vérifie
aussi le repli quand le Go ne répond pas.

D'autres tests complètent :

- `tests/lot6.rs` (`recall_par_la_facade`) : un tour de la façade rappelle
  et cherche sans passer par `/tool/exec` ;
- `TestRecallInterneLecture` : les deux lectures du Go (absent, seq
  illisible, méthode refusée, ordre) ;
- tests unitaires : ids, mots-clés, score, extraits, textes.
