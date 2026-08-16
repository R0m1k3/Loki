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
| `HF_TOKEN` | jeton Hugging Face, pour les dépôts à accès restreint | — |

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
- **Serveurs MCP** : Node.js est inclus dans l'image pour les serveurs `npx`.
- **Presets** de configuration par modèle, bench, auto-détection GPU.
- **API OpenAI-compatible** exposable (`network on`, protégée par clé).

Ajoutées par ce fork :

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
- **Identité** : ton prénom et un avatar emoji pour toi et pour Loki, affichés
  dans le fil.
- **Paramètres** : les réglages d'application (identité, apparence, accès
  OpenAI, actions) sont regroupés à part des réglages d'IA.

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
| Panneau « Moteur » | propose d'installer/compiler | annonce le moteur fourni par l'image, sans proposer d'install |
| Supervision moteur | systemd / launchd / PID (Windows) | fichier PID (`LOKI_CONTAINER=1`) |
| Configuration initiale | `ajean edit` ($EDITOR) | entrypoint + `loki config set` |
| Choix du modèle | lien Hugging Face collé à la main | recherche intégrée + verdict VRAM + projecteur lié |
| Historique de tchat | conversation unique | discussions multiples, titrées et persistées |
| Accès distant | relais chiffré ajean.link | retiré de l'interface |
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

## Licence

MIT — © les contributeurs d'AJEAN (« Jean contributors ») pour le code amont,
voir [`LICENSE`](LICENSE) et [`NOTICE.md`](NOTICE.md).
