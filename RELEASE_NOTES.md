# Loki 0.14.0

Loki rattrape AJEAN jusqu'à la 0.17.6 et reprend une série d'idées d'OpenFox
pour le mode Code. Le gros du travail est invisible : des discussions qui ne se
bloquent plus sur un contexte plein, des réponses qui survivent à une coupure
réseau, un agent de code qui gaspille moins de tours.

## Le contexte ne bloque plus la discussion

Sur une fenêtre de 65k, un tour qui lisait plusieurs gros fichiers d'un coup
passait de 60 % à plus de 130 % sans jamais compacter, et le moteur répondait
par un 400 qui figeait la discussion.

- Le compactage en cours de tour compte aussi les résultats d'outils que le
  moteur n'a pas encore vus.
- Tout résultat d'outil est borné à 30 000 caractères **pour le modèle** ;
  l'interface garde le résultat complet (« voir plus »).
- En dernier recours, les gros résultats sont tronqués puis les plus vieux
  échanges retirés, au lieu de laisser remonter l'erreur.
- Le compactage ne fait plus répondre deux fois à une vieille question, et le
  texte écrit avant un appel d'outil n'est plus rangé en double.

## Presets : API compatible OpenAI

Les presets d'API externe gagnent ce qui leur manquait :

- une case **« le modèle accepte les images »** pour un modèle distant
  multimodal ;
- plus de `chat_template_kwargs` (propre à llama.cpp) envoyé à l'API : une API
  stricte répondait 400 et **chaque compactage échouait** ;
- **reprise automatique** quand le flux se coupe en pleine réponse (Wi-Fi, VPN,
  proxy) : jusqu'à 5 fois, le modèle reprend là où il s'était arrêté ;
- un 401, 429 ou 502 n'est plus pris pour un appel d'outil mal formé ;
- le débit est mesuré même quand l'API ne le donne pas ; le benchmark refuse de
  mesurer un moteur local qui n'est pas celui du preset ; le badge des réponses
  porte le nom du modèle distant.

## Mode Code

- **La vérification attend que le builder ait fini.** Il marque chaque critère
  `completed` une fois fait ; tant qu'il en reste d'ouverts, il est relancé au
  lieu de payer une passe de vérification sur un travail inachevé. Plus aucune
  vérification ne part quand il vient de poser une question.
- **Écriture refusée dès son chemin** : un `write` sur un fichier jamais lu
  était refusé après tout le fichier généré. Le chemin passe maintenant en
  premier et le refus coupe la génération aussitôt.
- **Appel d'outil écrit en texte** (`<tool_call>`, format XML de Qwen3-Coder) :
  repéré pendant le flux, coupé, et la relance cite l'extrait fautif.
- `read_file`, `str_replace`, `old_string`… appris d'autres agents sont
  traduits vers les vrais outils.
- Les consignes du dépôt (**AGENTS.md**, CLAUDE.md) font partie du contexte.
- Terminal : `cmd | tail -N` rend le **vrai** code de sortie, la sortie déjà
  produite est gardée au délai dépassé, les couleurs ANSI sont retirées.
- `git_status` / `git_diff` trouvent le dépôt cloné dans un sous-dossier.
- Le compactage garde les fichiers touchés, les erreurs résolues et l'état des
  critères.

## Tâches et chat

- **« Rappelle-moi dans 20 minutes »** : `task_create` accepte `in_minutes` ou
  `at`, à l'heure du navigateur (le conteneur tourne en UTC).
- **Écrire pendant que l'IA répond** : le message part en file et le modèle en
  tient compte dans la suite de sa réponse ; deux appareils qui envoient en
  même temps ne se refusent plus.
- Longues discussions : au chargement, les 20 derniers échanges, le début d'un
  clic ; la liste des discussions se dessine par pages et montre la nouvelle
  dès le premier message.
- Mémoire : un mode par projet, et un quatrième, « recherche ».
- Appels d'outils parallèles séparés selon leur index ; bascule de preset par
  identifiant ; un raisonnement qui reprend après la réponse a sa propre bulle.

## Sécurité et données

- **Un site tiers ne peut plus piloter Loki** depuis le navigateur (Origin /
  Sec-Fetch-Site, DNS rebinding). ⚠️ Derrière un reverse proxy par nom de
  domaine, définis une clé de pilotage ou `LOKI_TRUSTED_HOSTS` **avant** la mise
  à jour, et vérifie que le proxy transmet l'en-tête `Host` d'origine.
- **Chiffrement de la mémoire** : l'activer rendait illisibles la discussion
  active et le mode Code. Corrigé ; une base déjà touchée est réparée au
  déverrouillage. Les résultats d'outils (« voir plus ») et les images du fil
  sont désormais chiffrés eux aussi, et les images effacées après 24 h.

## Moteur

- `--mlock` seul ne coupe plus le mmap (OOM au chargement).
- Port déjà occupé refusé avec un message clair ; cache KV lent signalé.
- Réponses web compressées en gzip.

## Mise à jour

```
docker compose pull && docker compose up -d
```
