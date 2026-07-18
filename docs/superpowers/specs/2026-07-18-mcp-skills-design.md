# Loki — MCP intégré + Skills automatiques + Boucle code vérifiée

**Date** : 2026-07-18
**Objectif** : augmenter l'intelligence effective des modèles locaux (~17 Go,
répartis sur 5060 Ti + 3060 12 Go) sans changer de modèle, en améliorant le
harnais : outils professionnels via MCP, méthodes expertes injectées (skills),
et vérification réelle du code produit.

**Choix utilisateur validés** :
- Cas d'usage : tout (code + raisonnement + multi-étapes).
- Compromis : équilibré — passes supplémentaires uniquement pour le code
  (1 passe de correction max).
- Un seul gros modèle ; pas d'architecture deux-modèles.
- MCP : catalogue préconfiguré des meilleurs serveurs, toggles on/off,
  désactivé = zéro coût (aucun outil dans le prompt, aucun process lancé).
- Skills : déclenchement automatique (routeur lexical), invisible, désactivable.

---

## 1. Client MCP intégré

### Backend — `backend/app/mcp_client.py`

- SDK officiel Python `mcp` (ajout à `requirements.txt`).
- Transports : stdio (commande locale, ex. `npx …`) et streamable-http (URL).
- **Gestionnaire** (singleton, même pattern que `ollama_client`) :
  - Catalogue embarqué (dict Python) + état activé/désactivé et paramètres
    (clé API, URL) persistés en DB (`db.get/set_config_value`, clé `mcp`).
  - Connexion **lazy** : un serveur activé n'est démarré qu'au premier message
    qui suit ; la session MCP (process + handshake) est réutilisée ensuite.
  - Serveur désactivé : process jamais lancé, outils jamais exposés.
  - `list_tools()` par serveur → conversion schéma MCP → schéma
    function-calling Ollama ; préfixe `mcp_<serveur>_<outil>`.
  - `call_tool(name, args)` avec timeout 30 s par appel.
  - Panne (crash process, handshake KO, timeout répété) : le serveur est
    marqué en erreur, ses outils retirés, notice SSE dans le fil ; nouvelle
    tentative au message suivant. Un MCP cassé ne bloque jamais le chat.
  - `aclose()` au shutdown (lifespan `main.py`).

### Catalogue préconfiguré (désactivés par défaut)

| ID | Serveur | Commande / transport | Apport |
|---|---|---|---|
| `playwright` | Playwright MCP | `npx @playwright/mcp@latest --headless` | navigateur réel : naviguer, cliquer, lire la console, screenshot |
| `context7` | Context7 | `npx -y @upstash/context7-mcp` | doc à jour de toute librairie/framework |
| `fetch` | Fetch | `python -m mcp_server_fetch` | lecture propre d'URL (markdown) |
| `searxng` | SearxNG | `npx -y mcp-searxng` + `SEARXNG_URL` | vraie recherche web (nécessite instance SearxNG) |
| `custom` | Personnalisé | commande ou URL saisie par l'utilisateur | n'importe quel serveur MCP |

- Filtre d'outils par serveur (liste `expose` optionnelle dans le catalogue)
  pour limiter le nombre d'outils injectés (ex. Playwright : navigate, click,
  type, snapshot, console, screenshot — pas les 25+ outils complets).

### Intégration boucle agent — `backend/app/agent.py` + `tools.py`

- Les outils MCP actifs sont ajoutés à la liste d'outils envoyée au modèle
  (après les outils natifs) et dispatchés vers `mcp_client.call_tool`.
- Résultats tronqués à une taille max (~8 000 caractères) avant retour au
  modèle (protège le contexte).
- Rendu frontend : ToolCards existantes (aucun composant nouveau requis).

### UI — Configuration → onglet « MCP » (`SettingsView.tsx`)

- Une carte par serveur du catalogue : nom, description, toggle, statut
  (● connecté / ○ inactif / ⚠ erreur + message), nombre d'outils exposés,
  champs paramètres (URL/clé) si requis.
- Carte « Personnalisé » : champ commande ou URL + toggle.
- Routes : `GET /api/mcp` (état), `PUT /api/mcp/{id}` (toggle + params),
  `POST /api/mcp/{id}/test` (connexion d'essai, renvoie outils découverts).

### Dockerfile

- Ajout Node.js LTS (requis npx) + `pip install mcp mcp-server-fetch`.
- Image : les serveurs npx sont téléchargés au premier lancement (cache
  volume npm optionnel).

## 2. Skills automatiques

### Bibliothèque — `backend/skills/*.md`

Fichiers markdown en français, livrés avec l'app :
- `debogage-systematique.md` — reproduire → isoler → hypothèse → corriger → vérifier.
- `creation-web.md` — structure sémantique → style → interactivité → vérification (liens, console, responsive).
- `refactor-sur.md` — comprendre → tests/garde-fous → petits pas → vérifier à chaque pas.
- `analyse-donnees.md` — examiner le format → valider → transformer → présenter.
- `redaction-structuree.md` — plan → rédaction → relecture ciblée.

Format : frontmatter (`name`, `description`, `keywords`) + corps injecté tel quel.

### Routeur de skills — `backend/app/skills.py`

- Sélection **lexicale instantanée** (regex/mots-clés, même style que
  `router.py`) : au plus UNE skill par message ; aucun appel LLM.
- Injection : message système supplémentaire
  (`"Méthode à suivre pour cette tâche :\n<corps>"`) inséré après l'invite
  système, pour ce tour uniquement (non persisté dans l'historique).
- Événement SSE `skill` `{name, title}` → badge dans le fil
  (« 📘 Skill : Débogage systématique »).
- Toggle global `skills_enabled` (défaut : activé) dans la config agent
  (Configuration → Intelligence).

## 3. Boucle code vérifiée

- Nouvel outil natif `run_check` (`tools.py`) : détection par extension —
  `.py` → `python -m py_compile` (analyse seule, JAMAIS d'exécution — toute
  exécution passe par `run_shell` et sa validation utilisateur) ;
  `.js/.ts` → `node --check` ; `.html` → `check_html` existant ; JSON → parse.
  Sortie = erreurs compactées.
- Boucle agent : après un `write_file`/`edit_file` sur du code, le harnais
  exécute `run_check` automatiquement et renvoie les erreurs au modèle dans le
  même tour — **1 passe de correction maximum** (choix « équilibré »).
- Si le serveur MCP Playwright est actif et que la tâche a produit du HTML :
  la skill `creation-web` oriente le modèle vers une vérification réelle dans
  le navigateur (console + rendu) au lieu du check statique seul.

## 4. Hors périmètre

- Architecture deux-modèles (architecte/codeur) — rejetée par l'utilisateur.
- Best-of-N / juge — trop coûteux.
- Marketplace/édition de skills dans l'UI (v2 possible ; v1 = fichiers livrés).

## 5. Vérification

1. **MCP** : activer Fetch → demander « résume cette page <url> » → ToolCard
   `mcp_fetch_fetch` + résumé correct. Désactiver → l'outil disparaît du
   prompt (vérifiable via logs). Serveur cassé (commande invalide) → notice,
   chat fonctionnel.
2. **Playwright** : « crée une page X puis vérifie-la dans le navigateur » →
   navigation + console lue + correction éventuelle.
3. **Skills** : message « mon script plante avec TypeError » → badge skill
   débogage ; message banal → aucun badge.
4. **run_check** : demander un script Python avec bug volontaire induit →
   l'erreur est détectée et corrigée dans le même tour (1 passe).
5. **Perf** : aucun serveur MCP actif → latence premier token inchangée
   (mesure avant/après).
