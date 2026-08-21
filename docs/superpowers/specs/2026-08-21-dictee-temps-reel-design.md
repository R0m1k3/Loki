# Dictée en temps réel, configurable selon le matériel

*Conception validée le 21 août 2026.*

## Le problème

La dictée fonctionne depuis la correction du binaire non portable (PR #18), mais deux
défauts la rendent pénible :

1. **Elle comprend mal.** Le modèle `small-q5_1` est le plus petit modèle multilingue
   utilisable, et `-l auto` lui fait redeviner la langue à chaque appel.
2. **Elle est lente et muette pendant l'enregistrement.** Le texte n'arrive qu'à
   l'arrêt du micro, après ~3 s de calcul pour 3,4 s d'audio.

La lenteur a une cause structurelle : chaque `POST /api/transcribe` lance un processus
`whisper-cli` qui **recharge le modèle depuis le disque**, transcrit, puis meurt. Le
chargement domine le temps de réponse. Aucun réglage ne rattrape ça, et le découpage en
tranches — indispensable pour écrire pendant qu'on parle — le rendrait catastrophique :
un rechargement complet du modèle toutes les quelques secondes.

## Ce qu'on construit

Trois choses, dans cet ordre de dépendance :

1. `whisper-server` supervisé par Loki, remplaçant les lancements de `whisper-cli`
2. Un découpage en tranches piloté par le serveur, alimenté par une WebSocket
3. Un panneau **Dictée** dans les réglages : GPU, modèle, langue, réactivité, test du micro

## 1. Le moteur

### whisper-server plutôt que whisper-cli

`whisper.cpp` fournit `whisper-server` : modèle chargé **une fois**, requêtes en HTTP.
Loki le supervise comme il supervise déjà `llama-server`.

**Démarrage paresseux, extinction sur inactivité.** Le serveur démarre au premier clic
sur le micro et s'arrête après ~10 min sans dictée. La machine cible a deux GPU dont on
veut préserver la VRAM : whisper n'a aucune raison d'occuper plusieurs centaines de Mo
pendant qu'on ne dicte pas. Le premier clic après extinction coûte le chargement du
modèle ; les suivants sont immédiats.

### Choix du GPU

`CUDA_VISIBLE_DEVICES` posé sur le processus `whisper-server`. Pas de drapeau à inventer,
pas de code à maintenir dans ggml :

- `CUDA_VISIBLE_DEVICES=1` → whisper ne voit que le second GPU
- valeur vide → CPU seul

Changer le GPU, le modèle ou la langue redémarre le processus. Rien d'autre ne bouge.

### Compilation : deux binaires, pas un

L'étape `whisperbuild` produit **deux** binaires :

- `whisper-server-cpu`, bâti comme aujourd'hui (`ubuntu:22.04`)
- `whisper-server-cuda`, bâti sur une image `nvidia/cuda:*-devel` avec `-DGGML_CUDA=ON`

La raison est dans le Dockerfile lui-même : `LLAMACPP_IMAGE` accepte la variante CPU
(`ghcr.io/ggml-org/llama.cpp:server`, ligne 11). Sur cette base, les bibliothèques CUDA
sont absentes et un binaire lié à CUDA **ne démarre pas du tout** — pas de repli
possible, l'éditeur de liens échoue avant le premier octet de code. Un unique binaire
CUDA condamnerait donc la variante CPU de l'image.

Loki choisit à l'exécution : binaire CUDA si un GPU est demandé *et* que le binaire
démarre, binaire CPU sinon. Un échec de démarrage du binaire CUDA bascule sur le CPU en
le disant, plutôt que de laisser la dictée morte.

**Les drapeaux de portabilité restent sur les deux cibles** : `GGML_NATIVE=OFF`,
`GGML_AVX512=OFF`, `GGML_AMX_*=OFF`. Le bug corrigé en PR #18 — binaire compilé pour le
processeur du runner, SIGILL ailleurs — ne doit pas revenir par la porte du build CUDA.

## 2. Le découpage

### On coupe dans les silences, jamais à heure fixe

Une tranche toutes les N secondes tombe statistiquement au milieu d'un mot une fois sur
deux. Whisper reçoit alors une demi-syllabe sans contexte, l'entrée même qui lui fait
produire du charabia. Couper au mauvais endroit dégraderait la reconnaissance au lieu de
l'améliorer.

Le **seuil de bruit est adaptatif**, pas une constante. Un seuil fixe se trompe des deux
côtés : trop haut dans une pièce silencieuse avec un micro discret, il ne coupe jamais ;
trop bas près d'un ventilateur, il coupe en permanence. Le découpeur mesure donc le
niveau des premières centaines de millisecondes comme plancher de bruit ambiant, le
réévalue en continu sur les passages calmes, et considère « silence » tout ce qui reste
proche de ce plancher. Le cas dégénéré — un plancher qui monte jusqu'à avaler la parole —
est borné par la coupure forcée.

Règle : on ferme la tranche dès qu'on observe **~400 ms de silence** *et*
qu'au moins **1,5 s de parole** est en réserve. Filet de sécurité : au-delà de **~8 s**
sans silence exploitable, on coupe quand même — quelqu'un qui parle sans respirer ne doit
pas voir le texte se figer.

Le réglage « réactivité » pilote ces bornes **ensemble** plutôt que d'exposer des curseurs
dont personne ne connaît l'interaction :

| Réglage | Réserve minimale | Coupure forcée |
|---|---|---|
| Court | 1 s | 5 s |
| Moyen (défaut) | 1,5 s | 8 s |
| Long | 3 s | 15 s |

### La logique vit en Go

Le navigateur ouvre une WebSocket et pousse le PCM au fil de l'eau **sans rien décider**.
Go accumule, détecte les silences, appelle `whisper-server`, renvoie chaque tranche.

Deux raisons. Il n'existe aucun harnais de test JavaScript dans le dépôt, alors que la CI
fait tourner `go test` — et le découpeur est précisément la pièce qui mérite des tests.
Et le navigateur redevient bête : capter, envoyer, afficher.

### Contrat de la socket

- **Client → serveur** : trames binaires, PCM 16 kHz mono 16 bits
- **Serveur → client** : JSON, `{"texte": "..."}` par tranche définitive, `{"erreur": "..."}` sinon

Le champ de saisie **ajoute à la suite, sans jamais réécrire**. Aucune révision d'un texte
déjà affiché : c'est le comportement retenu, contre l'affichage mot à mot qui se corrige
sous les yeux.

### Langue forcée par défaut

Le réglage part sur **français** plutôt que `-l auto`. Sur des tranches de quelques
secondes, la détection automatique se trompe régulièrement — c'est le mécanisme le plus
probable derrière les `ლლლ` géorgiens observés. Le mode auto reste disponible.

## 3. Le panneau Dictée

Nouveau `data-pane="dictee"` dans la modale des réglages, entre « Moteur llama.cpp » et
« Identité ». Suit le gabarit des panneaux existants.

| Réglage | Contenu |
|---|---|
| **Matériel** | GPU détectés avec VRAM libre (`/api/vram`, `/api/backends/devices`), plus « CPU seul » |
| **Modèle** | `large-v3-turbo` q5_0 (574 Mo) par défaut, `medium` q5_0 (539 Mo), `small` q5_1 (190 Mo), `large-v3` q5_0 (1,1 Go) |
| **Langue** | Français par défaut, auto et autres langues disponibles |
| **Réactivité** | Court / Moyen / Long — les bornes ci-dessus |
| **État** | whisper-server allumé ou éteint, modèle chargé, dernière erreur |
| **Test du micro** | Capte 3 s, affiche le niveau réel et la transcription, sans toucher au champ de saisie |

Les modèles restent **hors de l'image**, dans `/data/whisper/` : ils survivent aux
recréations du conteneur et ne gonflent pas le tirage. Téléchargement à la demande avec
progression et suppression, en généralisant le mécanisme mono-modèle existant.

Persistance des réglages : `putStr(bkState, …)`, le magasin clé/valeur déjà utilisé pour
le jeton Hugging Face.

## Les pannes, nommées

Chaque échec dit ce qui a échoué et ce qu'on peut y faire :

- **VRAM insuffisante au démarrage** → on le dit et on propose le repli CPU, plutôt que
  d'échouer sec
- **Modèle absent** → téléchargement avec progression, pas un micro qui ne répond pas
- **Socket coupée en pleine dictée** → le texte déjà obtenu reste dans le champ,
  l'utilisateur est prévenu
- **whisper-server qui meurt** → on dit **comment** il est mort (code de sortie ou signal),
  pas la dernière ligne de son journal. La leçon de la PR #18 : sur une mort par signal,
  cette dernière ligne ressemble à une explication sans en être une, et envoie chercher
  dans la mauvaise direction.

`POST /api/transcribe` **survit** et passe désormais par `whisper-server`. C'est le chemin
de repli si la WebSocket ne s'établit pas, et ce qui rend le test du micro trivial.

## Tests

En Go, sur ce qui peut réellement casser :

- **le découpeur** — silence franc, parole ininterrompue jusqu'à la coupure forcée,
  tranche trop courte rejetée, silence total, bruit de fond continu au-dessus du seuil
- **l'environnement de whisper-server** — indice GPU → `CUDA_VISIBLE_DEVICES`, chemin du
  modèle, langue
- **l'aller-retour des réglages** dans `bkState`
- **l'extinction sur inactivité**
- **le garde-fou du Dockerfile**, étendu : `GGML_NATIVE=OFF` conservé sur les deux cibles,
  présence des cibles `whisper-server` CPU et CUDA, `GGML_CUDA=ON` sur la seconde

## Ce que les tests ne prouveront pas

**La qualité de reconnaissance ne peut pas être vérifiée automatiquement ici** : aucun
échantillon de voix n'est disponible côté développement. Les tests établissent que le
découpage et la supervision sont corrects ; que `large-v3-turbo` comprenne mieux que
`small` reste une constatation d'usage.

C'est la raison d'être du sélecteur de modèle et du test du micro : rendre le compromis
ajustable par celui qui l'entend, au lieu de le figer dans le code sur une intuition.
