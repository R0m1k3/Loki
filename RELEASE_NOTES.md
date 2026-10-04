# Loki 0.15.0

Une version entièrement consacrée à la vitesse, avec une règle tenue de bout en
bout : **ne jamais dénaturer le modèle**. Rien ne touche à la quantification
des poids, à l'échantillonnage, à la température, au budget de raisonnement ni
au contenu du contexte. Ce qui est sans perte est actif d'office ; tout ce qui
changerait ce que voit le modèle, le placement des poids ou les slots du moteur
attend une clé. Sans clé, la requête et la ligne de commande restent celles
d'avant.

La section **Performance** du README rassemble toutes les clés, leur défaut et
leur prix.

## Actif d'office, sans perte

**La conversation n'est plus recalculée pour rien.**

- **Cache de prompts dimensionné** : le moteur garde en RAM une copie exacte des
  conversations qu'il quitte. Son défaut ne tenait pas une conversation de 30 à
  65 k jetons à côté d'un vérificateur ou d'un sous-agent : il l'évinçait, et
  tout était recalculé (30 à 80 s sur un 27B). Il est maintenant agrandi pour un
  modèle tout-GPU, et le slot est effacé après chaque travail annexe pour que la
  conversation revienne du cache.
- **Préfixe stable** : les rappels de Loki entrent dans l'historique tels
  qu'envoyés, la relance après un 500 ne change plus les outils, la ligne MCP ne
  manque plus au premier tour après un démarrage, l'ordre des trackers ne bouge
  plus. Chacun de ces écarts faisait recalculer toute la boucle d'outils.
- **Compaction moins tôt** : le raisonnement que Loki ne renvoie jamais ne
  compte plus dans le contexte. La compaction (qui perd de l'information)
  partait vers 44-47 k au lieu de 49 k sur une fenêtre de 65 k. Son résumé n'est
  plus amputé de l'état d'avancement.
- **Hybrides (Qwen3.5/3.6) sur un moteur ancien** : `--checkpoint-min-step 2048`
  d'office quand la RAM le permet, pour ne plus retomber jusqu'à ~8 k jetons en
  arrière à chaque reprise.

**Le matériel est mieux employé.**

- **Threads CPU** : « auto » veut dire les cœurs physiques, plus tous les
  threads logiques — c'est le décodage des experts MoE sur CPU qui payait.
- **Deux GPU** : file de lancements CUDA élargie quand le pipeline entre cartes
  est possible (même calcul ; seul le prompt peut gagner).

**Loki lui-même pèse moins.**

- Écrire un gros fichier ne coûte plus un temps quadratique côté Go (jusqu'à
  8 s de CPU volées au décodage sur un MoE).
- L'enregistrement de la discussion ne retarde plus le premier jeton.
- Un seul `nvidia-smi` pour tous les onglets, aucun pour un onglet caché, un
  seul à l'ouverture de l'éditeur de preset.
- Unraid : un `/data` ou `/models` servi par la couche FUSE est signalé, avec
  le remède.

**On peut enfin mesurer.**

- Chaque réponse dit ce qu'elle a repris du cache, recalculé et accepté du
  brouillon ; `GET /api/perf/summary` en fait la synthèse par nature de requête
  (`lost_events`, `lost_tokens` : le cache perdu, et après quoi).
- **Bench** honnête, en tâche de fond : mode complet à profondeur réelle, tours
  qui reprennent le cache, rien d'enregistré sans timings réels.
- Une sonde du gabarit de chat dit, sans rien envoyer au modèle, si le rendu
  reste stable d'un message à l'autre.

**Garde-fous.** Un cache KV quantifié, `--context-shift` ou `--cache-reuse`
posés à la main sont signalés. Un mode de chargement résident voué à l'échec
(poids restés en RAM au-delà de 90 %) est refusé avec la raison en clair —
`LOAD_GUARD=off` pour passer outre ; le refus n'est plus relancé en boucle par
systemd, et un moteur Vulkan sur cartes mixtes n'est plus refusé à tort.

## En opt-in : à essayer, à mesurer

| Clé | Ce qu'elle fait |
|---|---|
| `PREWARM=on` | prépare le prochain tour pendant que tu lis (`full` : aussi la discussion qu'on ouvre) |
| `COMPACT_CONTINUATION=on` | le résumé de compaction prolonge le prompt en cache au lieu d'être calculé à froid |
| `PROJ_SNAPSHOT=on` | bloc projet figé par discussion, changements livrés à part ; date et dossier sortent du système |
| `REASONING_ECHO=on` | renvoie au moteur local la réflexion du modèle, au format entraîné |
| `SPEC=auto` / `mtp` | décodage spéculatif MTP (tête détectée dans le fichier, garde-fous VRAM) |
| `SPEC=ngram` / `mtp+ngram` | n-grammes du contexte, utiles en mode Code |
| `SIDE_SLOT=on` | second slot pour les travaux annexes (VRAM en double) |
| `SLOT_PERSIST=on` | état du slot gardé à la bascule de preset |
| `SPLIT_MODE=tensor` | parallélisme de tenseurs entre cartes (expérimental) |
| `FIT_TARGET`, `OP_OFFLOAD_MIN_BATCH`, `CUDA_GRAPH_OPT` | réglages d'expert pour cartes inégales et MoE |
| `KEEP_TURN_IMAGES=on` | **zone grise** : garde les images des outils dans l'historique |
| `NUDGE_IN_TOOL=on` | **zone grise** : rappel de budget dans le résultat d'outil |

Et deux outils qui ne font rien d'eux-mêmes :

- **Optimiseur** (`loki tune`, bouton « Optimiser… ») : cherche lots, threads,
  marges et placement sur un moteur d'essai isolé ; rien n'est écrit sans un
  clic, et l'application est vérifiée puis défaite si le moteur ne tient pas.
- **« Dupliquer en placement auto… »** pour un MoE aux experts placés à la
  main : une copie du preset où `--fit` les répartit.

## Ordre de test conseillé

Une étape à la fois, mesurée avant la suivante :

1. **Bench complet** du preset tel quel : la référence.
2. **`PREWARM` + `COMPACT_CONTINUATION`**, une vraie session de travail, puis
   `/api/perf/summary` : le cache perdu (`lost`) doit baisser.
3. **Optimiseur** sur le preset en service.
4. **MoE** : « Dupliquer en placement auto… », bench complet des deux.
5. **Mise à jour du moteur** vers la version recommandée (encart du panneau
   Moteur).
6. **`SPEC=auto`** (ou `mtp`) si le modèle a une tête MTP.

## Moteur : version recommandée et retour arrière

Quand le moteur officiel est antérieur à `b10864`, le panneau Moteur dit ce
qu'une mise à jour apporte (points de reprise des hybrides, MTP rapide) et la
propose — sur clic seulement, version vérifiée sur le registre. La version
quittée est gardée : **Revenir à la version précédente** y ramène sans réseau.

Depuis `b10763`, llama-server garde par défaut la réflexion des tours passés :
un gabarit comme Qwen3.6 la rendrait vide. Avant de basculer, Loki le détecte et
propose `REASONING_PRESERVE=off` ; après, il compare le rendu de l'ancien et du
nouveau moteur et signale le premier écart.

## Ce qui a été écarté

- **`--cache-reuse`** : réutilise du cache calculé sous un autre contexte, donc
  change les sorties.
- **`--context-shift`** : retire des jetons du contexte, et ne marche pas sur
  les hybrides.
- **Cache KV quantifié par défaut** : `q8_0` n'est pas exact, `q4_0` perd
  mesurablement, pour ~2 Gio gagnés sur un 27B hybride. Reste un choix
  explicite (`KV_TYPE`).

## Autres corrections

- Une compaction en plein tour ne fait plus disparaître le rappel des pages
  mémoire lues.
- Un `loki tune` tué net ne laisse plus le moteur arrêté ; lancé avec un autre
  `LOKI_HOME` que l'interface (`sudo`), il refuse au lieu de charger un essai à
  côté d'un moteur vivant.
- L'ancienne option « n-grammes (mod) » de l'éditeur rejoint `SPEC=ngram`, avec
  une migration proposée (jamais faite d'office).
- Accès refusé : l'interface dit pourquoi et comment le lever.

## Mise à jour

```
docker compose pull && docker compose up -d
```

Installation systemd (`loki install`) : l'unité du moteur gagne
`RestartPreventExitStatus=78`. `loki update` ne réécrit pas les unités ;
`sudo loki install`, ou `sudo systemctl edit loki-engine` avec cette ligne dans
`[Service]`, la met à jour.
