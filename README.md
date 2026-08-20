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

En CLI dans le conteneur : `docker exec -it loki loki status` (aussi :
`logs`, `restart`, `config`, `bench`, `test`…).

### Données et persistance

**Tout** l'état vit sous `/data` (= `LOKI_HOME`). Si ce chemin n'est pas un
volume monté sur l'hôte, il disparaît au premier `docker compose down` — presets
et modèles compris.

| Chemin | Contenu |
|---|---|
| `/data/loki.db` | base bbolt : préférences, discussions, mémoire des réglages |
| `/data/presets/` | un `.env` par preset (modèle, contexte, NGL, vision…) |
| `/data/models/` | modèles téléchargés depuis l'interface |
| `/data/memory/` | pages de mémoire persistante (`.md`) |
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
  `mmproj` — voir [Installer un modèle](#installer-un-modèle).
- **Mémoire persistante** (`memory off|ondemand|always`).
- **Accès internet** : recherche + lecture de pages, moteur Go intégré ou
  [Crawl4AI](https://github.com/unclecode/crawl4ai) pour les pages JS.
- **Agent** : shell, fichiers, workspace (`agent on`).
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
  tâche attend son tour, et le bouton stop du chat l'interrompt.
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
  `NOTICE.md`.
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
