# Lot 30 — Motifs de relance personnalisés tenus par la façade

## Contexte

Quand le modèle écrit un appel d'outil en toutes lettres au lieu de
l'émettre par le protocole, Loki le reconnaît et relance le tour avec une
consigne corrective. Les motifs embarqués couvrent les formats connus. Un
expert peut les remplacer par la clé d'état `retry_patterns` :

- c'est un tableau JSON de regex du Go, chacune compilée en `(?mi)` ;
- une regex illisible est sautée ;
- une surcharge illisible ou vide laisse les motifs embarqués.

Jusqu'ici, une surcharge posée gardait la conversation au Go
(`facadeChatBlocker`), même avec `LOKI_CHAT=rust`.

## Le lot

La façade lit la surcharge au démarrage de sa conversation
(`chat::generate::retry_patterns`), comme le Go la compile une fois par
processus. Le Go ne garde plus la conversation pour cette clé.

Les regex sont celles du Go (paquet `regexp`), pas celles de `regex`.
`chat::stream::go_regex` les réécrit pour garder le sens du Go :

- les classes Perl et les limites de mot du Go sont ASCII ; celles de
  `regex` sont Unicode :
  - hors d'une classe : `\w`, `\W`, `\d`, `\D`, `\s`, `\S` deviennent des
    classes ASCII ;
  - dans une classe : leur équivalent dans la classe ;
  - `\b`, `\B` deviennent `(?-u:\b)`, `(?-u:\B)` ;
- `\Q…\E` (texte littéral) devient le texte échappé ; le Go le refuse dans
  une classe, la traduction aussi ;
- dans une classe, `[`, `&&`, `~~` sont littéraux pour le Go, et `--` est un
  intervalle : ils sont échappés ;
- l'octal du Go (`\101`) est permis (`octal(true)`). Une référence arrière
  (`\1`), que le Go refuse, est refusée.

## Parité

`internal/loki/parity_retry_test.go` compile côté Go une soixantaine de
regex (dont celles qu'il refuse). Il note leur première correspondance sur
onze textes : accents, chiffres non ASCII, espaces insécables, crochets,
majuscules accentuées. Le journal (`tests/parity_retry/patterns.json`) est
rejoué par `tests/retry.rs` : même compilation, même correspondance, même
position.

Autres tests :

- la lecture de la surcharge : absente, `null`, vide, mal typée, suivie de
  texte ;
- les regex sautées, les correspondances ;
- la surcharge lue au démarrage de la conversation de la façade ;
- côté Go, la clé ne garde plus la conversation.
