# Loki 0.13.0

Loki rattrape deux versions majeures d'AJEAN (0.14 → 0.15.4) et reprend deux
idées d'OpenFox. L'IA peut piloter un navigateur, la mémoire se chiffre sur le
disque, et le serveur sait te prévenir quand c'est prêt.

## L'IA pilote un navigateur

Un interrupteur dans *Réglages → Contrôle du navigateur* et l'IA ouvre des pages
dans le Chromium déjà embarqué dans l'image. Elle en reçoit les éléments
interactifs **numérotés** (`[12] bouton « Se connecter »`) et agit par numéro :
`browser_open`, `browser_find`, `browser_click`, `browser_type`, `browser_key`,
`browser_scroll`. **Aucune vision requise** — ça marche avec un petit modèle
texte. Avec un projecteur `mmproj` chargé s'ajoutent `browser_screenshot`
(capture quadrillée tous les 100 px) et `browser_click_xy`, pour ce que l'arbre
d'accessibilité ne montre pas : canvas, bandeau cookies en iframe.

Ce sont des actions réelles sur le web : l'interrupteur est distinct de l'accès
internet et n'agit qu'en **mode agent**, au même niveau de confiance que `bash`.
En CLI : `loki computer [on|off|status]`.

## La mémoire se chiffre

*Réglages → Mémoire → chiffrer la mémoire sur le disque* : pages mémoire,
discussions, blocs archivés au compactage et trackers passent en **AES-256-GCM**
sous `/data`. La clé de données est enfermée dans un coffre par une clé dérivée
en **Argon2id** ; ce qui l'ouvre, c'est la **clé de pilotage de cet appareil**
(le serveur n'en garde qu'une empreinte — il ne peut pas ouvrir le coffre seul)
ou une **clé de récupération** affichée une seule fois à l'activation.

La clé ne vit qu'en RAM : après un redémarrage à froid, la mémoire reste
verrouillée jusqu'à ce qu'un navigateur se reconnecte. Verrouillé, Loki n'écrase
jamais du chiffré par du clair — il refuse d'écrire. Un **snapshot** est pris
avant chaque bascule, une migration interrompue reprend au démarrage, et rien
n'est supprimé avant que son remplaçant ait été relu et vérifié.

**Sauvegarde chiffrée** : *exporter* télécharge un paquet scellé (mémoire,
presets, réglages) que *importer* rejoue sur un autre serveur avec la seule clé —
de quoi remonter le conteneur ailleurs. Le fichier reste chez toi : aucun envoi
vers un service tiers.

## Notifications, même app fermée

Le serveur pousse une notification à la fin d'une réponse **et à la fin d'une
tâche planifiée**, succès comme échec — c'est le cas qui compte, personne ne
regarde. Interrupteur dans *Réglages → Mode agent*, à armer sur chaque appareil.
Demande HTTPS (ou localhost) ; sur iPhone, ajoute d'abord Loki à l'écran
d'accueil.

## Tâches : des scripts qui tournent sans modèle

Un nouveau dossier `/data/scripts`, **hors du workspace jetable** : supprimer une
discussion n'y touche pas. Une tâche peut désormais être un **script seul** — le
planificateur le lance sans charger le modèle ni consommer un jeton. Une
sauvegarde, une synchro, un nettoyage n'ont rien à demander à un LLM.

L'IA dispose aussi de `task_create`, `task_list`, `task_update` et `task_delete` :
elle se pose ses propres rappels et veilles, cloisonnés par projet.

## Mode code : des sous-agents

L'outil `subagent` délègue une recherche (`explorer`), une relecture
(`code-reviewer`) ou un découpage (`planner`) à un rôle qui travaille dans **son
propre contexte** et ne rend que sa réponse. Sur un modèle local, c'est la
fenêtre de contexte qu'on sauve : « trouve où est géré le cache » coûte dix
lectures de fichiers, qui restaient sinon dans l'historique alors que seule la
réponse comptait. Tous les rôles délégués sont en lecture seule.

Le builder ne publie plus de lui-même : sans demande explicite, ni commit, ni
push, ni redémarrage de service.

## Interface

- **Anglais** (*Apparence → Langue*) : la coque — navigation, intitulés,
  boutons — passe en anglais. Ce qui n'est pas encore traduit reste en français
  plutôt que d'afficher une clé technique, et le fil de discussion n'est jamais
  touché.
- **Résultats d'outils** : le flux ne transporte plus qu'un aperçu ; « voir
  plus » charge le reste à la demande et déplie vraiment le bloc. Le compteur
  « ~N tok » dit enfin la taille réelle, pas celle de l'aperçu.
- **Longues discussions** : à l'ouverture, seule la fin du fil est rejouée. Un
  bandeau dit combien d'événements sont masqués et charge le début d'un clic.
- **Images** : l'orientation EXIF est cuite dans les pixels (fini les photos de
  téléphone couchées pour le modèle) et le grand côté ramené sous 1568 px.

## Corrections

- Un outil qui porte une image dans un message séparé (`see_image`,
  `browser_screenshot`) renvoyait « [déjà fait] » **sans** l'image et le modèle
  bouclait. Relancer la même commande `bash` est de nouveau permis.
- **MCP** ne tronque plus sa réponse à 12000 caractères avant le modèle.
- Le dossier mémoire n'est plus joignable qu'aux outils `mem_*` : un `cat
  memory/…` contournait l'index `MEMORY.md`.

## Mise à jour

```
docker compose pull && docker compose up -d
```
