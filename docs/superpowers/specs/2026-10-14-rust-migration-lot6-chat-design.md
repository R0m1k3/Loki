# Migration Rust — lot 6 : le cœur chat

Date : 2026-10-14 · Statut : **livré** (étapes 1 à 6 ; défaut Rust depuis le 2026-10-15, `LOKI_CHAT=go` pour revenir au Go) ·
Prérequis : lots 1-5 (façade, auth, store via API interne, `/v1`).

## Pourquoi c'est un lot à part

Le chat est le cœur de Loki et le seul chemin qu'on ne peut pas casser un
soir de déploiement. ~17 700 lignes Go (`chat_*.go` 11 491, `llm_*.go`
6 245), dont une fonction de 1 469 lignes (`runChatLoop`,
`llm_client.go:1279`) qui mêle protocole du moteur, reprises sur erreur,
compactage en cours de tour et exécution des outils.

## Carte (HEAD b98ee5c)

### Surface HTTP
Toutes derrière `requireWebAuth` (déjà portée).

| Route | Handler | Rôle |
|---|---|---|
| `/api/chat` | `handleChat` web_chat.go:316 | **SSE** : rejoue le journal puis le suit en direct |
| `/api/chat/send` | `handleChatSend` :178 | démarre un tour ou le met en file (409 occupé, 503 pas prêt) |
| `/api/chat/stop`, `/reset`, `/state`, `/compact`, `/tool-result` | web_chat.go | contrôle et lecture |
| `/api/chat/mode`, `/criteria` | code_mode.go | mode Code |
| `/api/chat/export`, `/upload`, `/files*`, `/file*`, `/image`, `/cushot` | divers | pièces jointes, espace de travail, aperçu navigateur |
| `/api/conversations{,/new,/switch,/rename,/delete}` | web_chat.go | index des discussions |
| `/api/e2e/chat` | relay_e2e.go:143 | même flux, chiffré AES-GCM par événement (tunnel) |

Trame SSE : `data: {"choices":[{"delta":…}]}`, battement `: ping` toutes les
4 s, regroupement à 128 Kio ou 256 événements.

### État en mémoire
- `conv *Conversation` (chat_conversation.go:114) : `Messages` (vue du
  modèle), `Log []LogEvent{Seq,TS,Delta}` (journal d'affichage, **seule voie
  vers l'UI**, via `appendDelta`), `Seq`, `CtxUsed`, `Generating`, `cancel`,
  `epoch`, file d'attente, tâche en cours. Un `mu` + `cond`.
- `Generating` est un **verrou global du moteur** partagé avec les tâches
  planifiées (`RunAutonomous`), le bench et l'optimiseur (`measureLease`) et
  quatre sondes `isGenerating`.
- Persistance : `snapshot()` sérialise Messages ET Log dans un seul JSON,
  **chiffré** (`memEncoderNow`, clé en mémoire du Go après déverrouillage),
  écrit par un écrivain unique (`persistQ`) en une transaction bbolt —
  bucket `chat`, clés `conv:<id>`, `index`, `active`.

### Boucle d'un tour
`handleChatSend` → `EnqueueOrStart` → `StartTurn` → `go generate` →
`turnView` (prompt système, outils, MCP, mémoire, instantané projet) →
`runChatTools` → `runChatLoop` (payload, slot, requête au moteur, analyse du
flux, reprises, compactage réactif, **switch des outils inline**
llm_client.go:2363-2549, compactage en cours de tour) → retour dans
`generate` (validation mode Code `codeVerifyLoop`, compactage de fin de
tour, persistance, file suivante, préchauffage).

## Coupures possibles

| | Coupure | Ce qui traverse | Risque |
|---|---|---|---|
| A | Journal + SSE en Rust, boucle au Go | flux de deltas Go→Rust, epoch, reset | Log persisté DANS le même blob chiffré que Messages |
| B | Client LLM en Rust (protocole, flux, reprises) | appel d'outil Go en RPC, porte du moteur, compactage | fonction de 1 469 lignes, appels récursifs (sous-agents, vérificateur) |
| C | Machine d'état de la conversation en Rust | turnView, compactage, outils, persistance en RPC | verrou `Generating` partagé avec tâches/bench/optimiseur |
| D | Sessions + persistance | — | écrivain unique bbolt, chiffrement, ordre des résultats d'outils |

## Stratégie proposée

Pas de bascule partielle de la discussion de l'utilisateur : le cœur chat
est réécrit **en entier** côté Rust, derrière `LOKI_CHAT=rust` (Go par
défaut), puis comparé au Go sur des conversations rejouées avant de devenir
le défaut. Étapes, chacune livrée et testée :

1. **Go, sans changement de comportement** : extraire le switch des outils en
   `execTool(ctx, name, args, caps) → résultat` et l'exposer en API interne
   (`/internal/v1/tool/exec`), ainsi que le chiffrement des blobs
   (`/internal/v1/crypt/seal|open`, la clé reste au Go) et le bail du moteur
   (`Generating` → `/internal/v1/engine/lease`).
2. **Rust, bibliothèque** : protocole du moteur (payload OpenAI, analyse SSE
   llama.cpp/Strata, reasoning, appels d'outils natifs et textuels, effort,
   reprises réseau), testé contre des flux enregistrés.
3. **Rust, conversation** : Messages + Log + epoch + file + SSE `/api/chat`
   (rejeu, pagination `historyCut`, `coalesceReplay`, `compactLog`),
   persistance au même format (`conv:<id>`, chiffré via l'étape 1).
4. **Rust, boucle** : `runChatLoop` réécrit en machine d'état explicite,
   outils exécutés par le Go (étape 1), compactage porté.
5. **Parité** : rejeu de conversations réelles exportées, comparaison des
   journaux produits (Go vs Rust) avec un faux moteur déterministe.
6. **Bascule** `LOKI_CHAT=rust` par défaut ; outils, MCP, mémoire, mode Code
   portés ensuite (lot 7) en retirant les appels au Go un par un.

## Questions ouvertes
- Chiffrement : port du format (`memEncoderNow`) en Rust à l'étape 3, ou
  RPC au Go jusqu'au lot 9 ?
- Tunnel du relais (`/api/e2e/chat`) : reste au Go jusqu'au portage du
  relais ; tant que `LOKI_CHAT=rust`, le flux chiffré doit lire le journal
  Rust (API interne inverse) — à concevoir.
