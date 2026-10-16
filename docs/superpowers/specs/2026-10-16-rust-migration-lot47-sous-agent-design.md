# Lot 47 — Outil subagent du mode Code dans la façade

## Contexte

En mode Code, le builder peut déléguer une question bornée à un rôle en
lecture seule :

- `explorer` cartographie le code ;
- `code-reviewer` relit un changement ;
- `planner` découpe un travail et pose les critères d'acceptation.

Le rôle travaille dans son propre contexte : le prompt du rôle, le
briefing machine et la tâche, rien de l'historique. Sa trace n'est ni
rangée ni affichée. Seul son texte final revient au builder, comme
résultat d'outil. Sur un modèle local, à la fenêtre étroite, c'est ce qui
évite de payer dix lectures de fichiers dans le fil principal.

La façade tient le tour du builder et sa passe de vérification, mais
`subagent` passait encore par `/internal/v1/tool/exec` : le Go faisait
tourner un `runChat` à lui.

## Le lot

`src/chat/subagent.rs` porte `toolSubagent` (`code_subagent.go`).

Avant tout appel au moteur, il fait les mêmes refus que le Go, dans le
même ordre, avec les mêmes textes :

- rôle inconnu (`verifier` compris : seule la boucle du contrat le lance) ;
- tâche vide ;
- appel depuis un tour qui n'est ni le fil principal ni le builder
  (garde-fou anti-récursion).

Le contexte du rôle vient du Go
(`POST /internal/v1/turn/subagent {role}`), qui rend :

- le prompt du rôle suivi du briefing machine ;
- les outils du rôle ;
- ses capacités (agent, Code, mémoire coupée).

`subagentView` et `subagentFraming` sont partagés avec `toolSubagent`.

Le tour du rôle tourne ici avec `run_turn`, comme la passe de
vérification :

- l'endpoint, le raisonnement, l'écho, les motifs de relance et l'arrêt
  sont ceux du tour qui délègue (`SubTurn`) ;
- la température est 0,6 (le TEMP du preset l'emporte) ;
- la tâche porte le cadrage du rôle :
  - planner : critères et étapes, sans demander confirmation ;
  - autres rôles : seulement les trouvailles, `fichier:ligne` ;
- c'est un travail annexe (`engineSideJob`) : second slot, puis slot
  effacé à la fin pour que le builder reparte du cache RAM ;
- la télémétrie le compte à part (`subagent`) ;
- les critères posés par le planner s'affichent comme ceux du fil.

Le rapport se construit ainsi :

- seul le texte écrit après le dernier outil compte ;
- il est rogné de ses blancs, puis préfixé du rôle ;
- au-delà de 6000 runes, il est tronqué (`…[tronqué]`) ;
- vide : « n'a rien répondu » ;
- erreur du tour : « sous-agent <rôle> : <erreur> ».

Le tour du rôle est porté par `SubHost`. Ce `TurnHost` délègue tout au
`GoTurnHost` du rôle, sauf un nouvel appel à `subagent`, toujours refusé.
Les outils de la façade sont dans `GoTurnHost::exec_tool_base` ; seul le
`exec_tool` du tour principal y ajoute `subagent`. Ainsi le type du tour
imbriqué ne se contient pas lui-même.

Le tour du builder et ses relances par la boucle du contrat prêtent leur
`SubTurn`. La vérification aussi, mais son rôle la fait refuser.

## Parité

`internal/loki/parity_subagent_test.go` tire du Go un journal
(`tests/parity_subagent/subagent.json`) contre un faux moteur scripté. Les
vues des trois rôles sont rangées avec lui.

Pour chaque tour délégué, le journal garde chaque requête du rôle :

- système identique à la vue ;
- message de la tâche cadrée ;
- température ;
- outils ;
- nombre de messages.

Il couvre :

- les refus :
  - rôle inconnu, `verifier`, rôle absent ;
  - tâche vide ou absente ;
  - récursion depuis un rôle ou depuis la vérification ;
- les tours :
  - réponse directe rognée ;
  - rapport après deux outils, narration écartée ;
  - texte vide relancé puis remplacé, comme le fait le tour ;
  - rien après l'outil ;
  - rapport tronqué ;
  - cadrage du planner ;
  - moteur qui refuse.

`subagent_comme_le_go` (`tests/lot6.rs`) le rejoue :

- les refus par `precheck` ;
- les tours par un tour complet de la façade qui délègue, contre le même
  script.

Il vérifie les requêtes du rôle, le rapport rendu au builder et le travail
annexe ouvert puis fermé.

D'autres tests complètent :

- `TestSubagentVueInterne` : la vue servie par le Go, rôles refusés ;
- tests unitaires : refus et leur ordre, rapports, cadrages.
