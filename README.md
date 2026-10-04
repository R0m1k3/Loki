# Loki — assistant IA local en conteneur (fork d'AJEAN)

> **Loki est un fork de [AJEAN](https://github.com/nathaninline/ajean)** de
> [nathaninline](https://github.com/nathaninline), sous licence MIT — voir
> [`NOTICE.md`](NOTICE.md) et [`LICENSE`](LICENSE). L'essentiel du code et des
> fonctionnalités vient d'AJEAN ; ce fork le rebaptise et le fait tourner dans
> **un conteneur Docker GPU autonome**, là où l'amont s'installe en binaire +
> systemd sur la machine hôte.

Loki fait tourner un modèle de langage **100 % en local** : discussions
multiples, mémoire persistante, accès internet, captures de pages web, outils
MCP, agent (shell, fichiers) — serveur d'inférence llama.cpp compris, dans une
seule image.

## Architecture

```
┌────────────────── conteneur loki ──────────────────┐
│  loki web  (UI + API, port 8090, premier plan)     │
│     │ pilote (fichier PID — pas de systemd)        │
│  loki serve ──exec──► llama-server (CUDA, :8080)   │
│                        ▲ modèles .gguf             │
│  chromium (Playwright) — captures de pages web     │
│  /data (config, bbolt, presets, mémoire,           │
│         workspace par discussion, modèles)         │
│  /models (GGUF déposés à la main)                  │
└────────────────────────────────────────────────────┘
```

- **Un seul conteneur** : l'UI joint le moteur sur `localhost` (contrainte
  héritée de l'amont), les deux partagent donc le même conteneur.
- **Sans systemd** : l'amont pilote le moteur via systemctl ; en conteneur,
  Loki bascule automatiquement sur une supervision par fichier PID
  (`internal/loki/sys_service_container.go`). Changer de modèle depuis l'UI
  redémarre le moteur normalement.
- Le moteur (port 8080, non authentifié par défaut) **n'est pas exposé** ;
  seule l'UI (8090) l'est.

## Démarrage rapide (Docker, GPU NVIDIA)

Pré-requis : pilote NVIDIA + [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html).

```bash
cp .env.example .env
docker compose up --build   # quelques minutes : llama-server vient précompilé
                            # de l'image officielle llama.cpp (server-cuda)
```

Interface : http://localhost:8090 — installe un modèle depuis la recherche
Hugging Face intégrée (voir ci-dessous), il démarre tout seul.

## Installer un modèle

Dans l'éditeur de preset, **Chercher un modèle** interroge Hugging Face et ne
remonte que les dépôts GGUF. Choisir un dépôt déplie ses quantifications avec
leur taille et un verdict mémoire (`ok` / `juste` / `trop`) calculé sur la VRAM
réellement détectée — ou sur la RAM système s'il n'y a pas de GPU. C'est une
estimation : le coût exact du cache KV dépend de l'architecture du modèle, que
la seule liste des fichiers ne révèle pas.

Si le dépôt publie un projecteur vision (`mmproj-*.gguf`), Loki propose de
l'installer avec le modèle et remplit le champ **Vision** du preset. C'est le
seul moyen fiable d'avoir la vision : un projecteur encode dans l'espace latent
de **son** modèle, donc un `mmproj` pris dans un autre dépôt donne un moteur qui
démarre et ne voit rien. Quand le dépôt n'en publie pas, Loki le dit plutôt que
d'aller en chercher un ailleurs.

Deux repères pour choisir un dépôt :

- `unsloth/*` publie des quantifications **Dynamic** (`UD-Q4_K_XL`,
  `UD-IQ3_XXS`…) qui gardent en plus haute précision les tenseurs sensibles :
  à taille égale, elles se tiennent mieux qu'un `Q4_K_M` classique.
- `ggml-org/*` est le dépôt de référence de l'équipe llama.cpp — c'est en
  général là que le projecteur vision est publié en premier.

Le champ **Télécharger un modèle** reste disponible pour coller un lien direct
(dépôt privé, fichier hors des conventions). Un dépôt à accès restreint demande
la variable d'environnement `HF_TOKEN`.

Certains dépôts sont **à accès restreint** (« gated ») : leur arborescence se lit
sans rien, mais chaque `.gguf` répond `401` tant que les conditions du dépôt
n'ont pas été acceptées sur huggingface.co **et** qu'un jeton n'est pas fourni.
Loki les marque « accès restreint » dès la liste des résultats et rappelle le
geste à faire, plutôt que de laisser choisir une quantification pour échouer au
lancement du transfert.

Le jeton se règle **dans l'interface** : éditeur de preset → *Modèle* → **Jeton
Hugging Face**. Il est vérifié auprès de Hugging Face avant d'être enregistré
(le compte associé s'affiche), rangé avec les autres secrets sous `/data` — donc
il survit aux redémarrages et aux changements de preset — et il sert aussi bien à
la recherche qu'au téléchargement. À défaut, la variable d'environnement
`HF_TOKEN` reste lue comme avant ; le jeton enregistré dans l'interface a la
priorité. Un jeton en **lecture** suffit (huggingface.co/settings/tokens), et il
n'est envoyé qu'aux adresses Hugging Face.

## Installation sur Unraid

L'image est construite et publiée par GitHub Actions sur GHCR
(`ghcr.io/r0m1k3/loki:latest`) à chaque push sur `main` — aucun build sur
Unraid. Compose prêt à l'emploi : [`docker-compose.unraid.yml`](docker-compose.unraid.yml).

1. Installe le plugin **Nvidia Driver** (Apps) et vérifie `nvidia-smi`.
2. Crée les dossiers :
   ```bash
   mkdir -p /mnt/user/appdata/loki/data /mnt/user/appdata/loki/models
   ```
3. Plugin **Compose Manager** → nouvelle stack → colle
   `docker-compose.unraid.yml` → **Compose Up**.
4. Interface : `http://<ip-unraid>:8090`.

## Configuration

Tout se règle **dans l'UI** (modèle, contexte, presets…) et survit aux
redémarrages (volume `/data`). Variables d'environnement du conteneur :

| Variable | Rôle | Défaut |
|---|---|---|
| `LOKI_WEB_PORT` | port de l'UI | `8090` |
| `LOKI_MODEL` | modèle initial (semé au 1er boot seulement) | — |
| `LOKI_CTX` | taille de contexte initiale | `32768` |
| `LOKI_NGL` | couches GPU initiales | `999` (tout) |
| `LOKI_HOME` | données (volume) | `/data` |
| `LOKI_MODEL_DIRS` | dossiers .gguf additionnels | `/models` |
| `HF_TOKEN` | jeton Hugging Face, pour les dépôts à accès restreint (repli : le jeton réglé dans l'UI prime) | — |
| `LOKI_CHROME` | binaire du navigateur piloté (contrôle du navigateur) ; par défaut le Chromium de Playwright de l'image | — |
| `LOKI_CU_HEADFUL` | `1` : ouvre une vraie fenêtre au lieu du mode headless (machine avec écran) | — |
| `LOKI_TRUSTED_HOSTS` | noms de domaine autorisés à servir l'UI **sans** clé de pilotage (reverse proxy), séparés par des virgules | — |

**Accès par nom de domaine.** Sans clé de pilotage, l'API n'accepte que les
hôtes locaux (IP, `localhost`, nom sans point, `.local`/`.lan`…) et refuse
toute requête venue d'un autre site : une page web ouverte dans un navigateur
du réseau ne peut plus piloter Loki en douce (ni par DNS rebinding). Derrière
un reverse proxy (`loki.mondomaine.fr`), définir une clé
(`docker exec -it loki loki set-web-key`) ou lister le nom dans
`LOKI_TRUSTED_HOSTS`.

En CLI dans le conteneur : `docker exec -it loki loki status` (aussi :
`logs`, `restart`, `config`, `bench`, `tune`, `test`…).

### Données et persistance

**Tout** l'état vit sous `/data` (= `LOKI_HOME`). Si ce chemin n'est pas un
volume monté sur l'hôte, il disparaît au premier `docker compose down` — presets
et modèles compris.

| Chemin | Contenu |
|---|---|
| `/data/loki.db` | base bbolt : préférences, discussions, mémoire des réglages |
| `/data/presets/` | un `.env` par preset (modèle, contexte, NGL, vision…) |
| `/data/models/` | modèles téléchargés depuis l'interface |
| `/data/memory/` | pages de mémoire persistante (`.md`) — joignables **uniquement** par les outils `mem_*`, pas au shell ; chiffrées si le chiffrement est actif |
| `/data/backups/memory/` | snapshots de la mémoire pris avant chaque opération qui touche à tout |
| `/data/scripts/` | scripts durables de l'agent, hors du workspace jetable (planifiables sans modèle) |
| `/data/workspace/` | racine du dossier de travail de l'agent |
| `/data/workspace/discussions/<id>/` | fichiers d'UNE discussion : dépôts, captures, ce que l'agent y écrit |
| `/data/loki-engine.log` | journal de `llama-server` (aussi via `loki logs`) |
| `/models` | GGUF déposés à la main depuis l'hôte (volume séparé, `LOKI_MODEL_DIRS`) |

Vérifier que le volume est bien là :

```bash
docker inspect loki --format '{{range .Mounts}}{{.Source}} → {{.Destination}}{{println}}{{end}}'
```

Sur Unraid, garde le **même** chemin hôte d'un lancement à l'autre : `/mnt/user/…`
(partage, via FUSE) et `/mnt/cache/…` (disque de cache) désignent des
emplacements différents dès que le partage n'est pas en cache-only ou que le
*mover* est passé. Le compose fourni utilise `/mnt/user/appdata/loki/…`.

**Performance sur Unraid.** `/mnt/user/…` passe par `shfs`, la couche FUSE des
partages : chaque écriture de `loki.db` et chaque page d'un gros modèle relue
depuis le disque (MoE plus gros que la RAM, mmappé) traverse un démon en espace
utilisateur, ce qui ralentit le décodage. Loki le détecte au démarrage
(`fuse.shfs` dans `/proc/self/mountinfo`) et l'affiche en encart d'information —
rien n'est perdu ni altéré. Pour l'éviter :

- **recommandé** — partages en *Exclusive access* (Unraid 6.12+). Ce n'est
  pas une case mais un état : *Global Share Settings* → *Permit exclusive
  shares* = Yes, puis le partage `appdata` (et celui des modèles) entièrement
  sur **un** pool — mover d'abord vers le pool, ensuite *Secondary storage* =
  None — jusqu'à lire *Exclusive access : Yes* sur la page du partage.
  Redémarre le conteneur : le chemin `/mnt/user/…` ne change pas, FUSE est
  court-circuité ;
- **avancé** — mappe `/mnt/<ton-pool>/appdata/loki/…` (ex. `/mnt/cache` si ton
  pool s'appelle `cache`), avec une migration manuelle : `Compose Down`,
  `rsync -a` de l'ancien dossier vers le nouveau, puis modification des deux
  lignes `volumes`. Un `/mnt/<pool>` inexistant atterrit dans la RAM d'Unraid
  (données perdues au redémarrage, serveur saturé par un modèle de 80 Go), et
  changer le chemin sans migrer donne un `/data` vide.

**Tu perds modèles, discussions et fichiers à chaque redémarrage ?** C'est le
signe que `/data` n'est **pas monté** : le conteneur écrit alors dans sa couche
éphémère, détruite à chaque recréation (mise à jour d'image, `compose down`,
redémarrage de l'array). Vérifie avec la commande `docker inspect` ci-dessus :
il doit y avoir une ligne `… → /data` **et** une `… → /models`. S'il n'y en a
pas, ton conteneur a été lancé sans mapping (template Docker Unraid incomplet,
`docker run` sans `-v`) — recrée-le avec les volumes du compose fourni. Depuis
cette version, Loki le détecte au démarrage : bandeau rouge dans l'UI et
avertissement en tête du journal du conteneur.

Autre piège au redémarrage du serveur : Docker relance les conteneurs
`restart: unless-stopped` **avant** que le plugin Nvidia Driver ait chargé ses
modules. Le journal montre alors des `ERROR: init … result=11` et le moteur
démarre sans GPU. Un `docker restart loki` une fois le pilote prêt suffit.

## Fonctionnalités

Héritées d'AJEAN :

- **Tchat** avec streaming, raisonnement visible, pièces jointes, export de
  conversations. La **vision** demande un modèle multimodal *et* son projecteur
  `mmproj` — voir [Installer un modèle](#installer-un-modèle). Toute image
  envoyée au modèle (pièce jointe, `see_image`, capture) est d'abord
  **redressée** — l'orientation EXIF d'une photo de téléphone est cuite dans
  les pixels, sinon le projecteur, qui ignore l'EXIF, la voyait couchée — et
  **ramenée sous 1568 px** de grand côté, taille au-delà de laquelle le base64
  grossit sans rien apprendre de plus au modèle.
- **Mémoire persistante** (`memory off|ondemand|always`).
- **Accès internet** : recherche + lecture de pages, moteur Go intégré ou
  [Crawl4AI](https://github.com/unclecode/crawl4ai) pour les pages JS.
- **Agent** : shell, fichiers, workspace (`agent on`).
- **Contrôle du navigateur** (`computer on`, ou *Réglages → Contrôle du
  navigateur*) : l'IA **pilote** un Chromium — celui de Playwright, déjà dans
  l'image — par le protocole DevTools. Elle ouvre une page, en reçoit les
  éléments interactifs **numérotés** (`[12] bouton « Se connecter »`) et agit
  par numéro : `browser_open`, `browser_snapshot`, `browser_find`,
  `browser_click`, `browser_type`, `browser_key`, `browser_scroll`. Aucune
  vision requise — ça marche avec de petits modèles texte. Avec un projecteur
  `mmproj` chargé s'ajoutent `browser_screenshot` (image quadrillée tous les
  100 px) et `browser_click_xy`, pour ce que l'arbre d'accessibilité ne montre
  pas (canvas, bandeau en iframe). Ce sont des **actions réelles** sur le web :
  l'interrupteur est distinct de l'accès internet et n'agit qu'en **mode
  agent**, au même niveau de confiance que `bash`. La session navigateur est
  unique et réutilisée entre les appels ; couper l'interrupteur la ferme.
- **Serveurs MCP** : Node.js (`npx`) et uv (`uvx`) sont inclus dans l'image, pour
  les serveurs écrits en JavaScript comme en Python. Au **premier** lancement,
  `npx`/`uvx` téléchargent le paquet du serveur — Loki attend jusqu'à 3 minutes
  ce coup-là (au lieu d'échouer sur « context deadline exceeded ») ; les
  lancements suivants partent du cache en quelques secondes.
- **Tâches planifiées** : une consigne que l'IA exécute **toute seule**, sur une
  fréquence réglable (« toutes les 2 h », « tous les jours à 9 h », ou une
  expression cron). Chaque tâche tourne **isolée des discussions** — elle n'écrit
  pas dans le fil, travaille dans son propre dossier (`workspace/tasks/<id>/`) et
  livre son résultat par les outils de l'IA (mail via MCP, shell, fichiers…) ; le
  compte-rendu du dernier passage est visible dans sa fiche, et **réinjecté** au
  passage suivant pour la continuité. Réglages par tâche : preset (donc modèle) à
  activer avant l'exécution, accès mémoire et web. Un **interrupteur maître**
  suspend tout d'un coup. Sans **mode agent**, une tâche s'exécute mais n'a aucun
  outil pour agir — l'interface le dit. Une seule inférence tourne à la fois : une
  tâche attend son tour, et le bouton stop du chat l'interrompt. Deux types de
  tâche : une **consigne IA**, ou un **script seul** — un fichier du dossier
  `/data/scripts` lancé directement, **sans charger le modèle ni consommer un
  token** (sauvegarde, synchro, nettoyage n'ont rien à demander à un LLM). Ce
  dossier vit **hors du workspace** : vider une discussion, ou la supprimer,
  n'y touche pas. L'IA elle-même dispose des outils `task_create`, `task_list`,
  `task_update` et `task_delete` — elle peut donc se poser ses propres rappels
  et veilles — cloisonnés par projet : dans un projet, elle ne voit et ne
  pilote que les tâches de ce projet.
- **Notifications** (Web Push) : le serveur prévient le navigateur **à la fin
  d'une réponse et à la fin d'une tâche planifiée**, même l'app fermée ou le
  téléphone verrouillé — c'est le serveur qui pousse, pas la page (un onglet
  caché relâche son flux). L'interrupteur est dans *Réglages → Mode agent*, à
  armer **sur chaque appareil** (l'abonnement appartient au navigateur). Exige
  **HTTPS** ou localhost ; sur iPhone, il faut d'abord ajouter Loki à l'écran
  d'accueil. Les clés VAPID sont générées à la première demande et rangées avec
  le reste sous `/data` ; le corps de la notification reste générique (aucun
  extrait de réponse), puisqu'elle transite par Apple ou Google.
- **Chiffrement de la mémoire** (*Réglages → Mémoire*) : pages mémoire,
  discussions, blocs archivés au compactage et trackers chiffrés en
  **AES-256-GCM** sur le disque. Chiffrement à enveloppe : une clé de données
  (DEK) tirée une fois, enfermée dans un coffre par une clé dérivée en
  **Argon2id**. Ce qui ouvre le coffre : la **clé de pilotage de l'appareil**
  (le serveur n'en garde qu'une empreinte — il ne peut pas ouvrir le coffre
  seul) ou une **clé de récupération** affichée une seule fois à l'activation.
  La DEK ne vit qu'en RAM : après un redémarrage à froid, la mémoire est
  verrouillée jusqu'à ce qu'un navigateur se reconnecte — verrouillée, Loki
  n'écrit jamais de clair par-dessus du chiffré, il refuse d'écrire. Règle
  tenue partout : **rien n'est supprimé avant que son remplaçant ait été relu
  et vérifié**, un **snapshot** est pris avant chaque bascule, et une migration
  interrompue reprend au démarrage.
- **Sauvegarde chiffrée en fichier** : *exporter* télécharge un paquet scellé
  (mémoire, presets, réglages) que *importer* rejoue sur un autre serveur avec
  la seule clé — de quoi remonter le conteneur ailleurs. C'est la sauvegarde de
  l'amont sans son relais : ici rien ne part sur un service tiers, le fichier
  reste chez toi.
- **Interface en anglais** (*Réglages → Apparence → Langue*) : la source reste
  française et « English » applique un dictionnaire sur la coque — navigation,
  intitulés, boutons, interrupteurs. Ce qui n'y figure pas **reste en
  français** plutôt que d'afficher une clé technique, et le fil de discussion
  n'est jamais touché : c'est ton contenu. Le dictionnaire s'enrichit sans
  toucher au reste de l'interface (`ui/src/js/28-i18n.js`).
- **Presets** de configuration par modèle, bench, auto-détection GPU.
- **Échantillonnage réglable par preset** : température, `top_p`, `top_k`,
  `min_p`, pénalités de présence et de répétition, dans l'éditeur de preset. Ces
  valeurs partent dans **chaque requête** au moteur, pas sur sa ligne de commande :
  les changer ne demande donc aucun redémarrage. Un champ laissé vide n'envoie
  rien et llama-server garde son défaut — utile parce que le défaut de llama.cpp
  (top_k 40, min_p 0.05) est rarement celui que recommande le modèle (Qwen3.8 en
  réflexion veut top_k 20, min_p 0, temp 1.0, top_p 0.95).
- **API OpenAI-compatible** exposable, protégée par clé (voir ci-dessous — ce
  fork la sert autrement que l'amont).

Ajoutées par ce fork :

- **Mode Code** : chaque discussion a un sélecteur **Chat | Code** dans le pied
  de la carte de saisie. En mode Code, Loki devient un agent de code : outils
  `read`/`grep`/`glob` (lecture bornée, numéros de ligne — et un fichier doit
  avoir été LU avant d'être modifié), outils git (`git_status`, `git_diff`,
  `git_clone` — le clone atterrit dans le dossier de la discussion), jobs
  d'arrière-plan (`bash_bg`/`bash_tail` pour un serveur de dev ou un build
  long), et **critères d'acceptation** : le modèle pose le contrat (2-6
  critères testables, éditables dans le panneau au-dessus de la saisie), puis
  une **passe de vérification indépendante** — même modèle, contexte isolé,
  seule habilitée à marquer un critère « passé » — contrôle le diff et relance
  la correction jusqu'à ce que tout passe (2 corrections max). Les fichiers
  modifiés passent au **LSP** (gopls, typescript-language-server, pyright —
  inclus dans l'image) : les erreurs de compilation reviennent dans le résultat
  de l'outil, sans lancer de build. Sécurité : commandes catastrophiques
  refusées (rm -rf /, mkfs, reboot…), chemins bornés au dossier de la
  discussion en mode Code. En mode Chat, un message qui ressemble à une tâche
  de code fait apparaître une puce « passer en mode Code ? » — suggestion,
  jamais bascule automatique. Conception reprise
  d'[OpenFox](https://github.com/co-l/openfox) (MIT), réécrite en Go — voir
  `NOTICE.md`. **Sous-agents** : l'outil `subagent` délègue une recherche
  (`explorer`), une relecture (`code-reviewer`) ou un découpage (`planner`) à
  un rôle qui travaille dans **son propre contexte** et ne rend que sa réponse.
  Sur un modèle local, c'est ce qui sauve la fenêtre : « trouve où est géré le
  cache » coûte dix lectures de fichiers, qui resteraient sinon dans
  l'historique jusqu'à la compaction alors que seule la réponse comptait. Tous
  les rôles délégués sont en **lecture seule** — ce qui modifie le dépôt reste
  dans le fil principal, sous tes yeux — et un sous-agent ne peut pas en
  appeler un autre.
- **Catalogue MCP** : le panneau *Serveurs MCP* offre un bouton **catalogue** —
  une vingtaine de serveurs connus (filesystem, git, fetch, memory, sqlite,
  playwright, context7, github…) avec leur commande déjà renseignée, classés par
  catégorie. Choisir une entrée **n'installe rien** : ça remplit le formulaire
  d'ajout, tu relis la commande — qui s'exécutera sur cette machine — puis tu
  enregistres. Le catalogue est un JSON **embarqué dans le binaire**
  (`internal/loki/mcp_catalog.json`), donc aucun appel à un annuaire distant :
  pour en proposer d'autres, édite ce fichier et recompile. Une entrée dont le
  runtime manque (`npx`/`uvx` absent) le signale au lieu d'échouer plus tard, et
  celles qui réclament une clé d'API la rappellent avant l'enregistrement.
- **Intensité du raisonnement** : un niveau — auto / aucune / basse / moyenne /
  haute / maximale — envoyé à `llama-server` comme `reasoning_effort`. Réglable
  **dans la barre de saisie**, parce que ça se décide en écrivant le message : le
  changement s'applique au message suivant, sans redémarrer le moteur. L'éditeur
  de preset garde le même réglage comme **défaut du modèle** ; appliquer un
  preset reprend donc la main sur le choix fait à la volée. `none` coupe le
  raisonnement ; les autres valeurs sont passées au gabarit jinja du modèle, ce
  qui ne change le comportement que des modèles qui les lisent (gpt-oss et
  apparentés) — ailleurs c'est ignoré sans erreur, et l'interface le dit plutôt
  que de promettre un effet. Aucun gabarit ne les connaît toutes (gpt-oss :
  basse/moyenne/haute ; Qwen3.8 : basse/moyenne/maximale) et certains **refusent**
  celles qu'ils ne connaissent pas, avec une erreur 500 qui tuait le tour : loki
  lit alors les niveaux annoncés par le refus, **repli sur le plus proche** (haute
  → maximale) et rejoue le message sans rien perdre de l'historique — une fois,
  puis la traduction est retenue pour ce modèle. La liste est grisée quand le
  raisonnement est coupé pour ce modèle.
- **Raisonnement renvoyé au modèle** (clé `REASONING_ECHO`, **off** par défaut,
  `loki config set REASONING_ECHO on`) : le raisonnement que le moteur local a
  séparé (`reasoning_content`) est gardé avec chaque message et renvoyé au même
  modèle. Les gabarits Qwen3.5/3.6 relisent alors les étapes d'une boucle
  d'outils avec leur réflexion — le format entraîné — au lieu de blocs vides, et
  le moteur ne recalcule plus le dernier message. Avec `REASONING_PRESERVE=on`
  (gabarits qui ont ce réglage, Qwen3.6), le préfixe reste stable d'un message
  utilisateur à l'autre ; Qwen3.5 n'en a pas, le gain y reste interne au tour.
  Contrepartie : plus de contexte par tour, donc compaction plus tôt. Jamais
  vers une API externe ni vers un autre modèle ; un prompt trop long est d'abord
  rejoué sans raisonnement, et un gabarit qui le refuse le suspend pour ce
  modèle jusqu'au redémarrage.
- **Bloc projet figé** (clé `PROJ_SNAPSHOT`, **off** par défaut,
  `loki config set PROJ_SNAPSHOT on`) : le contexte du projet (description,
  index mémoire, trackers, `AGENTS.md`) part en tête du premier message ; sans
  la clé il est reconstruit à chaque tour, et une page créée ou une valeur de
  tracker notée fait recalculer toute la conversation derrière lui. Avec la clé,
  chaque discussion garde une copie datée du bloc, envoyée à l'identique, et les
  changements arrivent en tête du message suivant dans un bloc
  `<context_update>` : pages ajoutées, modifiées, retirées, nouvelle ligne d'un
  tracker, et le texte **complet** d'une description ou d'un `AGENTS.md` modifié
  — rien n'est perdu, seul ce petit bloc est à calculer. Une ligne du prompt
  système (présente seulement avec la clé) dit au modèle que ces blocs viennent
  de Loki et que le plus récent l'emporte. Le bloc est repris tout neuf, et les
  anciens `<context_update>` retirés, quand le début du prompt change de toute
  façon : compaction, système ou outils modifiés (date, réglages), redémarrage de
  Loki, changement de modèle, de preset, de projet, de mode mémoire ou de mode
  Code — et dès que les mises à jour accumulées deviennent trop longues. Les
  tâches planifiées gardent le bloc à jour à chaque tour ; sans agent, rien du
  projet n'est envoyé ; un preset externe (API) n'est jamais concerné. Le titre, l'export JSON, le résumé de compaction et la
  passe de vérification ne voient pas ces blocs.
- **Préchauffage du prochain tour** (clé `PREWARM`, **off** par défaut,
  `loki config set PREWARM on`) : certains recalculs sont inévitables — prompt
  réécrit par une compaction, dernier message rendu autrement au tour suivant,
  discussion reprise après une tâche planifiée qui a pris le slot — et sans la
  clé ils retardent le premier mot du message suivant. Avec `on`, dès que le
  moteur local est libre après un tour ou une tâche, Loki lui envoie la requête
  du prochain tour, assemblée par les mêmes fonctions que la vraie (contenu
  vivant, rien de figé), suivie d'un message `.` et limitée à 1 jeton : le
  moteur calcule le préfixe pendant que tu lis, et le vrai message ne calcule
  plus que lui-même. Le jeton et le `.` sont jetés, rien n'est enregistré. Toute
  autre requête de Loki l'annule aussitôt, sauf un message dont la requête
  prolonge exactement le préfixe préparé (même historique, mêmes outils, mêmes
  réglages du gabarit). Jamais avec plus d'un slot (`PARALLEL` ou `-np` dans
  `EXTRA_ARGS`) — sauf les deux de `SIDE_SLOT`, où il prépare le slot de la
  discussion —, pendant un tour, une tâche, un bench, ni vers un preset externe.
  `full` prépare aussi la discussion qu'on ouvre, si on y reste 3 s. Visible dans
  la télémétrie sous `prewarm`. Ce que voit le modèle ne change pas : au pire, le
  préchauffage ne sert à rien (moteur sans points de reprise aux messages
  utilisateur, image juste avant, agent ou web changé avant d'envoyer).
- **Compaction en continuation** (clé `COMPACT_CONTINUATION`, **off** par
  défaut, `loki config set COMPACT_CONTINUATION on`) : sans la clé, le résumé
  d'une compaction part dans une requête à part — un prompt de résumeur et une
  transcription des anciens tours — que le moteur local calcule à froid, des
  dizaines de secondes sur un 27B, des minutes sur un MoE. Avec la clé, quand le
  slot porte encore le prompt de la discussion, Loki renvoie la requête du tour
  telle qu'elle est partie (mêmes messages, outils, arguments du gabarit et
  intensité de raisonnement) suivie d'une seule demande de résumé : le moteur ne
  calcule que celle-ci. La demande désigne le premier message gardé tel quel,
  demande de tout résumer avant lui et de dater l'état d'avancement à cet
  endroit ; mêmes règles de résumé qu'avant (mode Code compris), même budget,
  température 0.2 sans l'échantillonnage du preset, réflexion coupée,
  `tool_choice` à `none`. Ce qui est compacté, archivé et rangé ne change pas :
  la requête sert seulement à obtenir le résumé, et rien du prompt système ou du
  projet n'entre dans l'historique. Repli sur la transcription au moindre
  écart : autre requête passée par le slot depuis le tour (vérification du mode Code,
  sous-agent, tâche, préchauffage, bench, client `/v1`, autre discussion),
  moteur relancé sur un autre modèle ou une autre fenêtre, marge insuffisante
  dans la fenêtre, refus du moteur, appel d'outil émis, résumé vide ou fait de
  seul raisonnement ; un refus du gabarit ou un appel d'outil la suspend pour ce
  modèle jusqu'au redémarrage (gpt-oss à intensité haute peut épuiser le budget
  en réflexion : repli). La clé évite aussi un résumé voué au refus (même vide,
  il ne réduirait pas le contexte de 20 %) et, après un refus faute de réduction,
  n'en redemande pas avant que le contexte ait grossi de 10 % sur le même fil —
  sauf à 90 % de la fenêtre ou après une édition, une régénération ou un fil
  vidé. Le filet réactif (prompt refusé), le bouton « compacter », les tâches,
  les sous-agents et un preset externe gardent le chemin d'avant. Visible dans la
  télémétrie sous `compact`.
- **Images d'outils gardées** (clé `KEEP_TURN_IMAGES`, **off** par défaut,
  **zone grise**, `loki config set KEEP_TURN_IMAGES on`) : sans la clé, une
  image montrée par un outil (`web_screenshot`, `browser_screenshot`,
  `see_image`) n'est vue que pendant le tour ; au tour suivant elle manque à
  l'historique, et toute la boucle d'outils qui la suivait — souvent des
  dizaines d'étapes de navigation — est recalculée. Avec `on`, elle reste dans
  l'historique telle qu'envoyée, rangée par référence (`chatimg/`, chiffrée si
  la mémoire l'est), sous conditions : vision réellement active (projecteur
  chargé, revérifié à chaque tour — un preset sans vision n'en reçoit jamais) ;
  coût mesuré par le moteur (écart du nombre de jetons du prompt entre deux
  requêtes), et une image qu'on ne sait pas mesurer reste éphémère ; toutes les
  images gardées tiennent dans 10 % de la fenêtre, la première qui dépasse et
  celles qui la suivent dans le tour restent éphémères. Plus d'images gardées,
  c'est plus de contexte, donc une compaction (avec perte) plus tôt : toute
  compaction ou réduction forcée retire ces images **d'abord** (leur légende et
  une mention « image non gardée » restent) et ne résume ensuite que si c'est
  encore nécessaire ; rouvrir la discussion les retire aussi, comme les pièces
  jointes. Discussion seulement (ni tâche, ni sous-agent, ni vérification). Un
  preset externe n'est concerné que si sa vision est déclarée, et l'API
  refacture alors ces images à chaque tour. Le gain suppose que le moteur
  reprenne son cache au-delà d'une image ; sur un modèle hybride (Qwen3.5/3.6),
  dont les points de reprise ne suivent pas toujours une image, à vérifier dans
  la télémétrie (`lost` du tour suivant) avant de l'adopter. Sans la clé, la
  requête est identique à l'octet près.
- **Rappel de budget dans le résultat d'outil** (clé `NUDGE_IN_TOOL`, **off**
  par défaut, **zone grise**, `loki config set NUDGE_IN_TOOL on`) : après un
  grand nombre d'appels d'outils dans un même tour, Loki rappelle au modèle de
  conclure, dans un message à part. Sur un gabarit dont le rendu change dès
  qu'un message s'ajoute (Qwen3.5 : ce message devient la « dernière question »
  et tout le tour est rendu autrement), ce rappel fait recalculer toute la
  boucle d'outils du tour. Avec `on`, le rappel part au bout du dernier résultat
  d'outil, et seul ce bout est à calculer — sur le moteur local, et seulement
  si la sonde de gabarit a conclu que le rendu de ce modèle bouge (« inconnu » :
  message à part, comme sans la clé). Le texte du rappel ne change pas, sa
  place si : un modèle entraîné à se méfier des consignes lues dans une sortie
  d'outil peut moins bien le suivre. À comparer (tours qui concluent après le
  rappel) avant de l'adopter.
- **Second slot pour les travaux annexes** (clé `SIDE_SLOT`, **off** par
  défaut, `loki config set SIDE_SLOT on`, moteur relancé) : avec un seul slot,
  une vérification du mode Code, un sous-agent, une tâche planifiée ou un bench
  prennent la place de la discussion ; son état part dans le cache RAM et en
  revient — ou est recalculé s'il n'y tient plus. Avec `on`, le moteur ouvre
  deux slots de `CTX` jetons **chacun** (`-c` 2×`CTX`, `--parallel 2`,
  `--no-kv-unified`) : deux flux de cache KV séparés, si bien qu'un slot ne
  peut jamais manquer de place à cause de l'autre, et que les travaux annexes
  gardent toute la fenêtre. Chaque requête de Loki porte alors son `id_slot` :
  le tour, ses étapes, le préchauffage et le résumé en continuation sur le
  slot 0 ; vérification, sous-agents, tâches, bench, résumé sur transcription
  et clients `/v1` (forcés, quel que soit l'`id_slot` demandé) sur le slot 1.
  L'état de la discussion ne bouge plus ; l'effacement de slot après un
  travail annexe (`CACHE_ISOLATE`) n'a plus lieu d'être et s'abstient. Le prix :
  le cache KV et l'état récurrent d'un hybride en double en VRAM (le
  brouillon MTP et les points de reprise en RAM hôte aussi) — l'éditeur de
  preset affiche ce surcoût sous le cache de prompts, et le lancement refuse la
  clé (un slot, comme sans elle, avec une note dans le journal) si modèle et
  deux états dépassent 90 % de la VRAM, si la VRAM est inconnue (macOS, AMD),
  si des poids sont sur CPU (`--n-cpu-moe`, `-ot …=CPU`, `NGL` partiel), si le
  moteur ne connaît pas `--no-kv-unified`, ou si `PARALLEL`≠2, `-c`, `-np`,
  `-kvu`, `--kv-unified-per-slot`, `--cache-idle-slots` (ou leurs
  `LLAMA_ARG_*`) sont réglés à la main. Le routage ne s'active que si le moteur
  annonce bien deux slots d'au moins `CTX` jetons (`/props`) ; sinon les
  requêtes partent sans `id_slot`, comme avant. Ce que voit le modèle ne
  change pas : deux slots qui calculent en même temps donnent des logits égaux
  au bruit de virgule flottante près, comme un découpage de lots différent.
  Un « cache KV plein » sans nombre de jetons (le pool, pas le prompt) est
  rejoué tel quel et ne déclenche jamais de compaction.
- **État du slot gardé à la bascule de preset** (clé `SLOT_PERSIST`, **off**
  par défaut, `loki config set SLOT_PERSIST on`) : passer d'un preset A à B
  puis revenir à A relance le moteur deux fois, et la conversation de A est
  recalculée en entier au retour. Avec `on`, juste avant une bascule faite
  depuis l'interface ou par une tâche, Loki demande au moteur d'écrire l'état
  du slot de la discussion (`/slots/0?action=save`, sous `LOKI_HOME/slots`),
  et le recharge au retour, avant le premier message de cette discussion.
  Conditions strictes, sinon rien n'est écrit ou rechargé et le calcul est
  normal : la dernière requête passée par le slot était bien un tour de
  cette discussion (pas une vérification, une tâche, un préchauffage ou un
  client `/v1`), au moins 4096 jetons, au plus 8 Gio estimés et le double
  de place libre ; au retour, le moteur doit avoir **exactement** la même
  empreinte — ligne de commande complète (modèle, `CTX`, types KV, `NGL`,
  lots, gabarit, `EXTRA_ARGS`…), variables `LLAMA_ARG_*`/`GGML_*`/`CUDA_*`,
  taille et date du modèle, du projecteur, du binaire et de ses bibliothèques
  — et son slot 0 ne doit encore avoir servi aucune tâche. Une mise à jour du
  moteur change l'empreinte : jamais de reprise d'un build à l'autre. Une
  seule tentative par fichier, retiré ensuite ; deux fichiers au plus,
  supprimés dès que la clé est retirée. Réglage de la machine : il survit aux
  bascules comme `HOST` (un preset qui le pose l'emporte). Refusé au lancement (note au journal)
  avec le décodage spéculatif (`SPEC`, brouillon dans `EXTRA_ARGS` : la
  sauvegarde du moteur ne garde pas l'état du brouillon, et un brouillon MTP
  détecté sur le slot bloque aussi), des poids sur CPU (MoE déporté), un
  `--slot-save-path` déjà réglé, ou un moteur joignable par d'autres sans clé
  d'API (`HOST` autre que `127.0.0.1`). `loki switch` en ligne de commande ne
  garde rien (seul le process web sait ce que porte le slot). Le gain est
  réel surtout sur un modèle dense ; sur un hybride (Qwen3.5/3.6), le fichier
  n'a pas de points de reprise et ne sert que si le message suivant prolonge
  exactement les jetons gardés (gabarit qui rend le dernier tour à
  l'identique), sinon recalcul comme avant. Ce que voit le modèle ne change
  pas : llama.cpp ne reprend un état que sur un préfixe de jetons identique.
  Avec `PREWARM`, le préchauffage passe par le slot après chaque tour : une
  bascule faite ensuite ne garde rien (les deux clés se recouvrent peu). Une
  simple lecture d'un client (`/v1/models`, `/health`) ne compte pas.
- **Spéculation par n-grammes** (clé `SPEC`, valeurs `ngram` et `mtp+ngram`,
  **off** par défaut, `loki config set SPEC ngram`, moteur relancé) : en mode
  Code, le modèle recopie sans cesse ce qui est déjà dans le contexte (chemins,
  diffs, arguments JSON, fichiers réécrits). `ngram` y cherche la suite des
  24 derniers jetons et propose d'un coup les 48 à 64 suivants
  (`--spec-type ngram-mod --spec-ngram-mod-n-match 24 --spec-ngram-mod-n-min 48
  --spec-ngram-mod-n-max 64`, toujours en clair, jamais `--spec-default` dont
  le contenu peut changer d'une version à l'autre) ; `mtp+ngram` ajoute la tête
  MTP (`--spec-type draft-mtp,ngram-mod`), les n-grammes passant d'abord quand
  ils trouvent. Chaque jeton reste tiré par le modèle et un jeton proposé n'est
  gardé que s'il coïncide : même distribution, mais pas le même texte au bit
  près (la vérification par lots n'emprunte pas les noyaux du décodage jeton
  par jeton) — on compare des sessions rejouées, pas des diffs. Rien n'est posé
  si le moteur ne connaît pas `--spec-ngram-mod-n-match`, si `EXTRA_ARGS` ou
  `LLAMA_ARG_SPEC_TYPE` règlent déjà la spéculation (`--spec-type`, qui
  s'additionne au lieu de remplacer, `--spec-default`, `--spec-ngram-*`, `-md`…),
  ou, pour `ngram`, si un `--spec-draft-*` y figure. `mtp+ngram` sans tête MTP
  (ni dans le fichier ni en `MODEL_DRAFT`) ou sans `draft-mtp` dans le moteur
  retombe sur les n-grammes seuls, avec une note. Avec `ngram`, une tête MTP
  présente reste inutilisée (le type explicite coupe le choix automatique) : la
  note le dit. Poids sur CPU (`--n-cpu-moe`, `-ot …=CPU`, `NGL` partiel) :
  refusé tant que `GGML_OP_OFFLOAD_MIN_BATCH` (clé `OP_OFFLOAD_MIN_BATCH`) ne
  dépasse pas le lot de vérification de 65 jetons — sinon chaque brouillon
  recopierait les experts vers le GPU ; `OP_OFFLOAD_MIN_BATCH=128` pour
  l'essayer, en mesurant aussi les lectures disque si le modèle dépasse la RAM.
  Sur un hybride, chaque brouillon copie l'état récurrent (point de reprise)
  et le recharge s'il est rejeté. `/api/perf/summary` donne l'acceptation par
  nature de requête (`kinds.<nature>.draft_rate`) : à garder là où elle paie.
  Les drapeaux d'acceptation synthétique (`--spec-synth-*`, « benchmarking
  only »), que Loki ne pose jamais, déclenchent un avertissement au lancement.
- **Parallélisme de tenseurs** (clé `SPLIT_MODE=tensor`, **off** par défaut,
  expérimental, moteur relancé) : au lieu de donner à chaque carte des couches
  entières (`-sm layer`, défaut), chaque couche est coupée entre les cartes,
  qui additionnent ensuite leurs morceaux. L'amont mesure un décodage plus
  rapide mais un prefill nettement plus lent : sur deux GeForce en PCIe et une
  boucle d'agent qui relit beaucoup, à mesurer par tour avant de l'adopter.
  Loki n'active le mode que si l'aide du moteur liste `{none,layer,row,tensor}`
  (le mot « tensor » seul est partout), et ajoute alors `-ngl all`, `-ts` au
  prorata de la VRAM totale des cartes dans l'ordre du moteur (`--device`
  compris), `-fa on` — chacun seulement s'il n'est pas déjà dans `EXTRA_ARGS`
  ou une `LLAMA_ARG_*` — et `GGML_CUDA_ALLREDUCE=internal` si l'environnement
  ne le fixe pas : le chemin NCCL compresse en BF16 les grosses réductions du
  prefill, une perte de précision ; la réduction interne garde le type natif.
  « NCCL not compiled in » au journal est alors sans effet ; `GGML_CUDA_P2P=1`
  reste à poser soi-même si le pilote le permet. Pas de `--fit` en mode
  tensor : Loki estime poids + cache de `CTX` jetons et refuse au-delà de 90 %
  de la VRAM — jamais en réduisant `CTX`. Refusé aussi (découpe par couches,
  note au journal) avec un cache KV autre que f16/bf16/f32 (jamais réécrit
  d'office), des poids ou le cache KV sur CPU (`--n-cpu-moe`, `-ot`, `NGL`
  partiel, `-nkvo`), `--backend-sampling`, `-fa off`, un `-sm` déjà réglé,
  `SPEC`, `SIDE_SLOT`, `CTX` non chiffré, une seule carte ou une carte non
  CUDA, une architecture que llama.cpp exclut (et `qwen4exp`), un preset
  externe ; `SLOT_PERSIST` est refusé avec la clé.
- **Guide de placement entre cartes inégales** (éditeur de preset, groupe
  « Cartes graphiques », dès deux cartes ; un conseil, rien n'est appliqué
  d'office) : chaque carte CUDA affiche sa liaison PCIe maximale (génération,
  largeur), l'actuelle et l'horloge mémoire en info-bulle — lues par
  `nvidia-smi` en une requête, jointes par nom de carte (rien pour deux
  cartes homonymes ni pour un moteur Vulkan), avec repli sur la requête
  historique si un vieux pilote refuse un champ. `loki gpu` les affiche aussi.
  Le guide rappelle les deux règles du moteur, dans l'ordre qu'il voit
  (`--device` compris) : en placement auto, `--fit` remplit d'abord la
  **dernière** carte, qui porte la couche de sortie (modèle dense : la plus
  rapide en dernier, ou une marge `FIT_TARGET` plus large sur la lente) ; les
  experts MoE en RAM sont recopiés au prefill vers la **première** (à elle la
  liaison la plus large). Les marges `FIT_TARGET` du preset sont montrées carte
  par carte ; un `--tensor-split` (qui désactive `--fit`) ou un
  `CUDA_VISIBLE_DEVICES` propre au preset (ordre réel différent) est signalé.
  Un lien « inverser l'ordre » écrit `--device` (et retourne `--tensor-split`)
  dans le preset édité. Pas de bouton de mesure automatique : comparer deux
  ordres demande deux rechargements du moteur et un ordre inversé peut manquer
  de VRAM sur la petite carte — dupliquer le preset, inverser l'ordre dans la
  copie, « bench complet » sur chacun, et vérifier `offloaded N/N layers`.
- **MoE aux experts sur CPU : avis et copie en placement auto** (jamais de
  réécriture du preset). Au lancement (journal) et dans l'éditeur, sous
  « Experts MoE sur CPU », pour un modèle dont le GGUF annonce des experts :
  experts placés à la main (`-ot` visant `…exps…` vers le CPU, `--n-cpu-moe`,
  `--cpu-moe` — un `-ot` de la seule couche d'entrée `per_layer_token_embd` ne
  compte pas) → `--fit` ne les place pas, et monter `UBATCH` seul grossirait
  le tampon de calcul de chaque carte sans personne pour rééquilibrer (monter
  `--n-cpu-moe` d'abord) ; placement auto avec un modèle plus gros que la VRAM
  et `UBATCH` ≤ 512 → « UBATCH 2048+ recommandé » (les experts restés en RAM
  sont recopiés vers le GPU à chaque micro-lot du prefill) ; moteur sans
  `--fit` → « garde `--n-cpu-moe` » ; `--fit` coupé par un autre `-ot` → rien
  ne sort les experts du GPU. Le bouton **« Dupliquer en placement auto… »**
  crée une **copie** du preset affiché, sous « (placement auto) », sans la
  bascule : `-ot` des experts (et celui de `per_layer_token_embd`, redondant :
  llama.cpp garde toujours la couche d'entrée au CPU), `--n-cpu-moe`,
  `--cpu-moe`, `-ngl`, `--tensor-split`, `--fit off`, `-b`/`-ub` et drapeaux
  de chargement retirés ; `NGL` retiré (`-ngl auto`), `UBATCH=2048`,
  `BATCH=4096`, `--load-mode mmap` ; `CTX`, cache KV (quantifié ou non : à
  garder identique pour comparer), échantillonnage intacts. Refusé si la copie
  ne tournerait pas en `--fit` (autre `-ot`, `-sm row/tensor`, `SPLIT_MODE`,
  `--n-cpu-ffn`) ou si `CTX` vaut 0. Après bascule : `successfully fit params`
  au journal (`failed to fit` = tous les experts sur GPU, revenir à
  l'original), puis bench complet des deux.
- **Garde du mode de chargement** (clé d'échappement `LOAD_GUARD=off`, dans
  le preset) : `--load-mode none`, `mlock`, `dio`, `mmap+mlock` (ou
  `--no-mmap`, `--mlock` sur un moteur ancien, ou leurs `LLAMA_ARG_*`) gardent
  en RAM tous les poids laissés au CPU. Loki en estime la part — modèle
  (toutes tranches) moins VRAM totale, tout le modèle sur un Mac Apple Silicon — face à la
  RAM effective (limite du conteneur cgroup comprise) : avertissement au-delà
  de 80 % (llama.cpp #26110 : swap, décodage de 25 à 7,5 t/s ; mlock : processus
  tué) ; **refus** au lancement seulement quand la borne basse dépasse 90 %
  (échec certain), avec la raison en clair, aussi affichée par l'interface.
  VRAM inconnue (Mac Intel, AMD, Vulkan, `--device`) ou serveurs `--rpc` :
  avertissement, jamais de refus. `mmap` et `auto` ne sont jamais concernés ;
  `LLAMA_ARG_NO_MMAP`/`LLAMA_ARG_MLOCK` ne comptent que sur un moteur ancien
  (un moteur à `--load-mode` les ignore). Rien ne change dans la ligne de
  commande.
- **Optimiseur sans perte** (`loki tune`, bouton **« Optimiser… »** dans
  l'éditeur du preset en service ; rien ne tourne de soi-même, aucune clé de
  config). Cherche, pour cette machine et ce build du moteur, les réglages
  d'ordonnancement qui raccourcissent un tour de conversation, sans rien changer
  à ce que calcule le modèle (sortie équivalente en distribution : les sommes
  flottantes ne sont pas identiques au bit près d'un lot ou d'un placement à
  l'autre ; la spéculation vérifie chaque jeton). **Isolation** : le vrai moteur
  est arrêté (comme « décharger la VRAM »), chaque essai est une **copie** de la
  configuration avec ses surcharges dans `LOKI_HOME/tune/run/` — `config.env`
  n'est jamais écrit — lancée par `loki serve` sur `127.0.0.1` et un port libre,
  mêmes `CUDA_VISIBLE_DEVICES` ; son groupe de processus est tué en sortie
  quoi qu'il arrive, puis le vrai moteur est relancé. **Verrou** exclusif entre
  processus (`LOKI_HOME/tune.lock`, PID + instant de démarrage + binaire) :
  pendant la mesure, tout démarrage du moteur (`serviceAction`, toutes
  plateformes), bascule ou enregistrement de preset, choix des GPU, clé d'API,
  mise à jour ou recompilation du moteur, rechargement de la VRAM, bench, chat,
  compaction, `bash_bg` et clients `/v1` (503) sont refusés avec une phrase
  claire ; les tâches planifiées attendent. Un verrou dont le propriétaire est
  mort (Loki tué en plein essai) est écarté avant tout démarrage du moteur :
  l'essai orphelin n'est arrêté que si PID, instant de démarrage ET binaire
  concordent (`taskkill /T` sous Windows), et le moteur est relancé au
  démarrage de l'interface s'il tournait avant. **Refus d'entrée** : preset
  externe, aucun preset actif, génération, tâche, bench ou job `bash_bg` en
  cours, moteur occupé (`/slots`) — en ligne de commande, seul ce dernier
  contrôle voit le processus web : préférer le bouton. **Essais** (descente étape par étape depuis
  la meilleure configuration du moment, chaque axe seulement si l'aide du
  moteur et la machine le permettent) : placement — seulement avec « inclure
  le placement » / `--placement` : `--fit` à la place des experts placés à la
  main (`-ot …exps…`, `--n-cpu-moe`, `--cpu-moe`) ou d'un `--tensor-split` ;
  marges `FIT_TARGET` (2 cartes ou plus, `--fit-target` dans l'aide) ;
  `UBATCH` ∈ {512, 1024, 2048, + 4096 pour un MoE} × `BATCH` ∈ {max(2048, ub),
  2 × ub ≤ 8192}, lot > micro-lot sur 2 cartes tout GPU, jamais de micro-lot
  plus grand sans `nvidia-smi` ; `OP_OFFLOAD_MIN_BATCH` ∈ {32, 128, 512} et
  threads (omis = cœurs physiques, physiques − 1, moitié ; Linux) seulement
  avec des poids sur CPU ; `CUDA_LAUNCH_QUEUES` off/4x sur 2 cartes ou plus ;
  avec « inclure les options opt-in » / `--opt-in` : `SPEC=auto`,
  `SPEC_N_MAX` ∈ {2, 3, 4}, `CUDA_GRAPH_OPT=on`, `--backend-sampling`. Un
  réglage qu'`EXTRA_ARGS` écraserait (`-ub`, `-t`, `-fitt`…) est réglé **dans
  EXTRA_ARGS, sur place** ; EXTRA_ARGS est traité jeton par jeton et seuls les
  drapeaux de la liste blanche (`-ot` des experts, `-ncmoe`, `-cmoe`, `-sm`,
  `-ts`, `-mg`, `-fit`, `-fitt`, `-b`, `-ub`, `-t`, `-tb`, + `--spec-draft-n-max`
  et `-bs` en opt-in) bougent ; tous les autres jetons (`--cache-type-k`,
  `--flash-attn`, `--chat-template-file`…) ressortent à l'octet près. **Jamais
  touchés** : `MODEL`, `CTX`, `KV_TYPE*` (un cache déjà quantifié est signalé
  zone grise, jamais modifié), `MMPROJ`, `REASONING*`, `TEMP` et autres clés
  d'échantillonnage, `PARALLEL`, `NGL`, `SPLIT_MODE`, `CUDA_VISIBLE_DEVICES`.
  **Garde-fous** : chaque essai est d'abord composé à blanc (`loki serve` sans
  rien charger) et sa ligne de commande comparée à celle de référence hors
  réglages permis — toute autre différence (contexte, cache KV, flash-attn,
  slots, variable du moteur…) l'écarte comme « dénature » ; une fois chargé,
  `n_ctx`, `total_slots` (`/props`) et les types du cache KV (journal) sont
  recomparés ; moins de couches déportées que la référence = « fit en recul »
  (performance), écarté ; moins de 768 Mio de VRAM libre sur une carte après la
  mesure (+ 512 avec `MMPROJ`) = « trop juste », jamais retenu ; échec de
  chargement ou OOM = essai ignoré ; deux essais à la même ligne de commande ne
  sont mesurés qu'une fois. **Mesure** : le bench complet (prefill à froid à une
  profondeur D fixe pour tous les essais, trois tours qui reprennent le cache —
  points de reprise réels d'un hybride compris —, decode à D, ligne courte en
  prose), avec l'échantillonnage et le raisonnement du preset. **Score** : durée
  d'un tour type = K / prefill des tours + G / decode à D + f × D / prefill à
  froid, K, G et f tirés des médianes de la télémétrie (`/api/perf/summary`)
  quand elle en a vu assez, sinon 2000, 600 et 5 %. La référence est mesurée
  deux fois ; un essai ne gagne que s'il bat la meilleure configuration du
  moment de plus de max(3 %, écart entre passages) sur **deux** passages, sans
  ralentir de plus de 5 % le decode en prose. Budget par défaut 30 min
  (`--budget`), ETA et annulation ; au-delà, résultat partiel dit tel ; étapes de
  base à la carte (`--stages lots,threads…`, cases de la fenêtre). **Résultat**
  : les mesures de chaque essai et le diff des clés, enregistrés par preset
  avec l'empreinte et le build du moteur (l'éditeur propose de relancer après
  une mise à jour du moteur). **Rien n'est écrit sans un clic** : « enregistrer
  dans une copie » (preset « (optimisé) », non activé) ou « appliquer à ce
  preset » après confirmation du diff — et une confirmation de plus si
  EXTRA_ARGS est réécrit par le placement. Le preset est sauvegardé
  (`LOKI_HOME/tune/backup/`), réécrit clé par clé (commentaires et ordre
  intacts), réappliqué ; la configuration active doit alors être exactement la
  référence plus les clés réglées, le moteur redémarre et doit répondre avec le
  même contexte et les mêmes slots puis tenir une sonde (prompt de D jetons et
  decode) — sinon l'ancienne version est rétablie et le moteur relancé. Pendant
  la mesure, le chat est indisponible : prévoir 10 à 40 minutes selon la taille
  du modèle (un modèle relu depuis un disque lent recharge à chaque essai).
- **Discussions multiples** : historique complet dans la barre latérale, titre
  repris du premier message (renommable), suppression. **Chaque discussion a son
  dossier de fichiers** (`workspace/discussions/<id>/`) : les pièces jointes
  déposées, les captures et ce que l'agent écrit y atterrissent, le shell et les
  chemins relatifs du modèle y sont résolus. Changer de discussion change donc
  les fichiers ; supprimer (ou vider) une discussion emporte les siens, pour que
  le disque ne se remplisse pas en silence.
- **Recherche Hugging Face** intégrée avec verdict mémoire et installation liée
  du projecteur vision (voir [Installer un modèle](#installer-un-modèle)).
- **Captures de pages web** : l'agent dispose de l'outil `web_screenshot`
  (Chromium via Playwright, inclus dans l'image). Les captures partent en JPEG
  et sont plafonnées à 20 fichiers / 40 Mo par discussion. La description de
  l'outil suit la capacité **réelle** du moteur, sondée sur `/props` : sans
  vision effective, elle dit au modèle « tu ne vois pas l'image » plutôt que de
  lui promettre des yeux qu'il n'a pas — il peut toujours prendre la capture et
  la montrer, sans prétendre la décrire. L'image relayée au moteur reste
  éphémère : la persister gonflait le contexte jusqu'à le faire déborder.
- **Panneau Fichiers** (bouton dossier de la barre de saisie) : les fichiers de la
  discussion ouverte — dépôts, captures, ce que l'agent y a écrit — avec
  navigation dans les sous-dossiers, téléchargement et suppression. Un dossier
  affiche la taille de **tout** son contenu, c'est ce qu'on libère en le
  supprimant, et le pied donne l'occupation disque de la discussion. Les chemins
  sont bornés à son dossier, liens symboliques résolus des deux côtés : ni le
  reste du disque ni les autres discussions ne sont atteignables. Les fichiers
  d'avant ce rangement que la migration n'a pas su rattacher restent joignables
  par le bouton **hors discussion**, qui disparaît une fois le ménage fait.
- **Interface « Sober Tech »** : ardoise et sauge, typographie Inter (interface)
  et JetBrains Mono (code, chiffres, chemins) — embarquées dans le binaire, donc
  aucune requête vers un service de polices. Deux variantes : claire par défaut,
  **Deep Dark** (fond `#0F172A`, cartes `#1E293B`) d'un clic depuis l'en-tête.
  L'en-tête porte le titre de la discussion et le **sélecteur de modèle** (le
  changement de preset ne demande plus d'ouvrir les réglages) ; la barre
  latérale s'escamote pour rendre toute la largeur au fil ; les discussions s'y
  cherchent au clavier et les jauges **GPU / VRAM / mémoire vive** restent
  visibles en pied de colonne.
- **Libérer la VRAM d'un clic** : sur les jauges du moniteur, un bouton décharge
  le modèle et arrête le moteur (ainsi que le serveur de dictée, qui occupe la
  carte lui aussi) pour rendre la mémoire vidéo à une autre application — jeu,
  encodage, autre serveur d'inférence. Le bilan est annoncé en Gio réellement
  rendus, et le même bouton devient **Recharger le modèle** pour reprendre la
  main. Routes : `POST /api/vram/unload` et `POST /api/vram/reload`.
- **API OpenAI servie par Loki** : `/v1/*` est exposé **sur le port de
  l'interface** (8090) et relayé vers llama-server, au lieu d'annoncer l'adresse
  du moteur. Conséquence directe : l'API est joignable partout où l'interface
  l'est — par l'IP du réseau local comme par un nom de domaine — sans publier de
  second port ni ouvrir le moteur. L'amont annonçait `http://<ip>:8080/v1`, une
  adresse injoignable en conteneur (le port 8080 n'y est pas publié, et l'IP
  détectée est celle du bridge Docker).
  - Authentification par la **clé API** du panneau (`Authorization: Bearer …`),
    vérifiée par Loki **et** par le moteur. Sans clé, l'endpoint est ouvert et
    l'interface le dit en rouge.
  - **Adresse publique** : un champ où saisir son domaine, pour le cas du
    reverse proxy où Loki ne voit qu'un appel interne. Laissé vide, l'adresse
    affichée suit celle du navigateur.
  - **TLS** : mettre un reverse proxy devant (Caddy, Nginx, Traefik). Loki
    honore `X-Forwarded-Proto` pour annoncer une adresse en `https`.
  - L'ancienne exposition publique via le relais de l'amont
    (`<machine>.oai.ajean.link`) est retirée de l'interface : elle exigeait un
    jeton de relais que ce fork ne permet plus d'obtenir, l'interrupteur ne
    pouvait donc qu'échouer.
- **Budget d'appels d'outils** : un tour d'agent n'a aucun plafond — couper une
  recherche légitime est pire que la laisser durer — mais au-delà de 24 appels
  sur un même tour, Loki rappelle au modèle combien il en a déjà faits et lui
  demande de conclure. Le rappel revient tous les 24 appels, en durcissant le
  ton ; il ne coupe jamais le tour, c'est de la pression, pas une barrière.
  Sans lui, un petit modèle qui tourne en rond n'avait rien en face de lui sauf
  le bouton stop (vu en production : 50 appels, 55 minutes, à relire cinq fois
  les mêmes fichiers). Réglable par `AGENT_BUDGET` dans `config.env` —
  `AGENT_BUDGET=off` le désactive complètement.
- **Identité** : ton prénom et un avatar emoji pour toi et pour Loki, affichés
  dans le fil.
- **Réglages en modale** : tous les réglages vivent dans une fenêtre à deux
  volets — la nav des sections à gauche (IA, moteur, application), le panneau
  choisi à droite. La barre latérale ne garde que les discussions (les plus
  récentes en tête) et le moniteur machine.
- **Nom du modèle sur chaque réponse** : une pastille à côté de « Loki » dit
  quel modèle a produit la réponse. Elle est journalisée avec le tour : elle
  survit au rechargement, et un vieux tour garde le modèle de l'époque.
- **Dictée vocale** : un bouton micro dans la carte de saisie enregistre,
  transcrit **en local** (whisper.cpp, compilé dans l'image ; modèle
  `small-q5_1` multilingue ~190 Mo téléchargé au premier usage dans
  `/data/whisper/`) et pose le texte dans le champ. ⚠️ le navigateur n'autorise
  le micro qu'en **HTTPS** (ou sur `localhost`) — derrière un reverse proxy
  TLS, rien à faire ; en `http://IP:8090`, le bouton l'explique.
- **Cartes raisonnement/outils à hauteur bornée** : un long raisonnement ne
  fait plus grandir la page de plusieurs écrans — la carte reste à taille fixe
  et défile toute seule pendant la génération. Sur un raisonnement géant, seul
  le bas du bloc est re-rendu en direct (le texte complet est posé à la fin) :
  l'affichage ne se fige plus.

Retirées par ce fork :

- **Accès distant via [ajean.link](https://ajean.link)** : la section de
  l'interface et son module JS sont supprimés — un conteneur derrière son
  propre réseau n'en a pas l'usage. Le code serveur du relais reste en place
  mais **inerte** (aucun jeton, aucune section pour en fournir un) : le retirer
  créerait un conflit à chaque reprise de l'amont.
- **Postes distants** (faire agir l'agent sur un autre PC appairé) : bouton du
  composeur, modales d'appairage et module JS supprimés. Même traitement que
  ci-dessus — les routes `/api/node/*` subsistent mais plus rien ne peut
  générer de code d'appairage, donc aucun poste ne peut se connecter.
- **Catalogue de modèles distant** : il interrogeait `ajean.link/models.json`,
  sa route n'avait aucun consommateur et son repli embarqué datait de 2024. La
  recherche Hugging Face le remplace.

## Différences avec l'amont

| | AJEAN (amont) | Loki (ce fork) |
|---|---|---|
| Installation | binaire + `sudo ajean install` (systemd) | `docker compose up` |
| Moteur llama.cpp | compilé sur la machine (`ajean llamacpp install`) | image officielle llama.cpp (`server-cuda`), précompilée |
| Panneau « Moteur » | propose d'installer/compiler | affiche la version qui tourne et la met à jour en un clic (sans rebuild) |
| Supervision moteur | systemd / launchd / PID (Windows) | fichier PID (`LOKI_CONTAINER=1`) |
| Configuration initiale | `ajean edit` ($EDITOR) | entrypoint + `loki config set` |
| Choix du modèle | lien Hugging Face collé à la main | recherche intégrée + verdict VRAM + projecteur lié |
| Historique de tchat | conversation unique | discussions multiples, titrées et persistées |
| Agent de code | — | mode Code : critères d'acceptation, passe de vérification, LSP, outils git |
| Accès distant | relais chiffré ajean.link | retiré de l'interface |
| Endpoint OpenAI | `:8080/v1` du moteur, ouvert par `network on` | `/v1` servi par Loki sur le port de l'interface, protégé par la clé API |
| Mise à jour | `ajean update` (binaire GitHub) | `docker compose pull` |

Le reste — mémoire, outils, protocole, moteur d'inférence — est celui d'AJEAN.
Pour récupérer les évolutions de l'amont :

```bash
git fetch upstream && git merge upstream/main   # conflits de renommage à arbitrer
```

## Build sans GPU / autres accélérateurs / version épinglée

L'image Loki se construit **au-dessus de l'image serveur officielle de
llama.cpp**, choisie par le build-arg `LLAMACPP_IMAGE` :

```bash
# CPU seul (test sans GPU)
docker build --build-arg LLAMACPP_IMAGE=ghcr.io/ggml-org/llama.cpp:server .
# Vulkan (GPU AMD/Intel/NVIDIA sans CUDA)
docker build --build-arg LLAMACPP_IMAGE=ghcr.io/ggml-org/llama.cpp:server-vulkan .
# Version de llama.cpp épinglée (reproductible)
docker build --build-arg LLAMACPP_IMAGE=ghcr.io/ggml-org/llama.cpp:server-cuda-b10423 .
```

Aucune compilation de llama.cpp n'a lieu : le moteur est maintenu et
précompilé par l'équipe amont (toutes architectures GPU courantes).

## Mettre à jour llama.cpp sans reconstruire l'image

llama.cpp publie plusieurs versions par jour ; l'image de Loki, elle, ne se
reconstruit qu'à une mise à jour de Loki. Le moteur y était donc figé à la date
du dernier build, et le rattraper imposait un rebuild complet (2,6 Go) pour un
composant de 170 Mo.

**Réglages → Moteur** affiche désormais la version de llama.cpp qui tourne
(`b10450`, avec son commit) et deux boutons :

- **vérifier la version** — interroge le registre et dit s'il existe plus récent ;
- **mettre à jour le moteur** — télécharge `llama-server` et ses bibliothèques
  depuis l'image officielle, `ghcr.io/ggml-org/llama.cpp:server-cuda`.

Ce qui est téléchargé n'est **pas l'image** : un manifeste OCI liste ses couches,
et seules celles qui portent `/app` sont récupérées (~170 Mo). Le runtime CUDA
(2 Go) et la base système sont déjà dans l'image de Loki. Le moteur atterrit dans
`/data/engine/<version>/`, donc sur le volume de données : il survit à un
`docker compose pull`.

La variante est déduite du moteur en place (CUDA, Vulkan, SYCL, MUSA ou CPU) :
une installation Vulkan ne se verra jamais proposer une image CUDA.

**Le garde-fou.** La mise à jour apporte llama.cpp, pas le runtime CUDA, qui
reste celui de l'image. Un llama.cpp compilé pour un CUDA plus récent ne
chargerait pas son backend GPU ici — et le symptôme serait silencieux : tout
fonctionne, mais sur le processeur. Le nouveau moteur est donc lancé à blanc
avant toute bascule ; s'il ne démarre pas, ou s'il ne voit plus aucune carte
alors que le moteur courant en voyait, la mise à jour est refusée et le moteur
courant n'est pas touché. Le moteur livré par l'image reste par ailleurs intact :
**revenir au moteur de l'image** y ramène en un clic, sans réseau.

Quand cette limite est atteinte pour de bon (CUDA majeur trop ancien), la
solution reste le rebuild avec un `LLAMACPP_IMAGE` récent, ci-dessus.

Derrière un miroir de registre ou un réseau qui n'atteint pas ghcr.io :
`LOKI_OCI_REGISTRY=https://mon-miroir.interne` dans l'environnement du conteneur.

## Licence

MIT — © les contributeurs d'AJEAN (« Jean contributors ») pour le code amont,
voir [`LICENSE`](LICENSE) et [`NOTICE.md`](NOTICE.md).
