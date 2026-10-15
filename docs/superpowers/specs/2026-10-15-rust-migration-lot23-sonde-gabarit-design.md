# Lot 23 — sonde de gabarit portée en Rust

## Contexte

La sonde de gabarit (`chat_tplprobe.go`) dit ce que le gabarit de chat du
modèle chargé fait de ce qu'on lui envoie :

- `preserves_history` : le raisonnement d'un tour passé est-il encore rendu ?
- `prefix_stable` : après un tour d'outil, le rendu précédent reste-t-il un
  préfixe exact du suivant ?
- `renders_reasoning` : `reasoning_content` est-il rendu quelque part ?

Le Go la lance après chaque complétion du fil principal, avec les outils et
les `chat_template_kwargs` de la requête. Trois choses lisent son verdict :

- la politique d'écho du raisonnement (`REASONING_ECHO`) ;
- le rappel de budget glissé dans le résultat d'outil (`NUDGE_IN_TOOL`) ;
- le résumé des performances (`/api/perf/summary`).

Le relevé du rendu (`tplRenderPrint`) et sa comparaison (`tplRenderDiff`)
servent le panneau Moteur, autour d'une mise à jour.

Quand la façade tenait la conversation (`LOKI_CHAT=rust`), plus aucune sonde
ne tournait :

- l'écho décidait sans verdict ;
- `NUDGE_IN_TOOL` gardait la conversation au Go (`facadeChatBlocker`).

## Le lot

`rust/loki-rs/src/chat/tplprobe.rs` porte la sonde entière :

- les requêtes au moteur local (`/health`, `/props`, `/apply-template`, avec la
  clé du moteur et les relances sur 503) ;
- les rendus synthétiques et leurs verdicts ;
- le cache par build, modèle, gabarit et forme ;
- le dernier verdict, global et par forme ;
- la ligne de journal ;
- le déclenchement borné (une sonde en vol au plus, une par minute) ;
- le relevé du rendu et sa comparaison.

Avec `LOKI_CHAT=rust` :

- **la boucle du tour** signale chaque complétion lue en entier sur le moteur
  local, outils ni coupés ni neutralisés (`TurnHost::completed`). L'hôte lance
  la sonde pour le fil principal seulement : ni tâche ni sous-agent ;
- **la politique d'écho** lit le dernier verdict ;
- **`NUDGE_IN_TOOL`** est porté (`nudge_in_tool_key`, `nudge_into_tool`,
  `TurnHost::prefix_unstable`). Il ne garde plus la conversation au Go ;
- **le résumé des performances**, que sert encore le Go, relit le verdict de la
  façade (`/internal/facade/v1/chat/tpl`, `facadeTplCaps`).

## Parité

`internal/loki/parity_tplprobe_test.go` enregistre chaque échange de la sonde
du Go avec un faux moteur : méthode, chemin, clé, corps envoyé, réponse.

Les comportements de moteur couverts :

- gabarit Qwen3, gabarit qui garde tout, raisonnement jamais rendu, amorce
  ignorée ;
- 404, exception du gabarit, clé exigée (inconnue ou connue) ;
- 503 relancés ou persistants, 429 ;
- `/props` illisible, moteur pas prêt, réponse sans prompt ;
- rendus incohérents, preset externe.

Le journal contient aussi :

- le verdict, sa ligne de journal, `tplCapsCurrent` et `tplCapsFor` ;
- le cache : même moteur, gabarit changé, build changé ;
- le relevé du rendu sur trois gabarits, et une erreur ;
- neuf comparaisons de relevés ;
- l'empreinte de six formes.

`tests/tplprobe.rs` rejoue chaque échange contre le port. Le faux moteur rend
les réponses enregistrées et exige des requêtes identiques, octet pour octet.
Il vérifie aussi le déclenchement borné.

Autres tests :

- `tests/lot6.rs` vérifie la sonde lancée après un vrai tour de la façade ;
- les tests de la boucle vérifient le rappel dans le résultat d'outil et les
  complétions signalées ;
- `TestFacadeSondeDeGabarit` vérifie, côté Go, que le résumé des performances
  relit le verdict de la façade.
