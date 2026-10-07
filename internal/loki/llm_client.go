package loki

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Message is one entry in the chat history sent to llama.cpp.
// `Content` may be nil when an assistant message only contains tool_calls.
type Message struct {
	Role       string     `json:"role"`
	Content    any        `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	// ReasoningContent : raisonnement que le moteur a séparé pour ce message
	// assistant, gardé seulement avec REASONING_ECHO=on (llm_reasoning_echo.go).
	// ReasoningModel : le modèle qui l'a produit. Les deux sont persistés avec la
	// discussion ; à l'envoi, echoMessages retire toujours l'étiquette, et le
	// raisonnement quand il ne doit pas partir. Vides : JSON inchangé.
	ReasoningContent string `json:"reasoning_content,omitempty"`
	ReasoningModel   string `json:"reasoning_model,omitempty"`
	// ImgRelay : message `user` qui relaie l'image d'un outil (capture,
	// see_image) sous KEEP_TURN_IMAGES (chat_keep_images.go) — jamais posé sans
	// la clé. Persisté pour qu'un fil rouvert le reconnaisse encore comme un
	// relais et non comme une demande (isLokiInjected) ; retiré à l'envoi
	// (wireMessages). Faux : JSON inchangé.
	ImgRelay bool `json:"img_relay,omitempty"`
	// CtxUpd : Loki a posé un bloc <context_update> en tête de ce message
	// utilisateur (PROJ_SNAPSHOT, chat_projsnap.go) — jamais sans la clé. Seuls
	// les messages marqués sont nettoyés : un texte TAPÉ qui commencerait par la
	// même balise reste tel quel. Persisté, retiré à l'envoi (wireMessages).
	CtxUpd bool `json:"ctx_upd,omitempty"`
	// imgTokens : coût mesuré par le moteur de l'image d'un relais GARDÉ dans
	// l'historique (KEEP_TURN_IMAGES) ; 0 = image non gardée, ou déjà retirée.
	// En mémoire seulement : un fil rechargé a de toute façon perdu ses images
	// (stripImageParts).
	imgTokens int
}

type ToolCall struct {
	// Index : position de l'appel dans un flux (delta OpenAI). Absent des
	// messages de l'historique (nil → omis).
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolCallFunc `json:"function"`
}
type ToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}
type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

// Tool definitions: OpenAI-shaped function schemas advertised to the model when
// the agent mode is on. The memory tools (mem_*) let the model keep persistent
// Markdown notes across sessions; bash is its real access to the machine.

func memSearchTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "mem_search",
			Description: "Search your memory (Markdown pages under memory/) → ranked {file, title, snippet}. Use FIRST when the user mentions something you might already know, then mem_read the best page.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "Keywords"},
					"limit": map[string]any{"type": "integer", "description": "Default 8, max 30"},
				},
				"required": []string{"query"},
			},
		},
	}
}

func memReadTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "mem_read",
			Description: "Read a memory page. Lines prefixed with their 1-indexed number; offset/limit for long pages.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file":   map[string]any{"type": "string", "description": "Page name (e.g. docker-notes.md)"},
					"offset": map[string]any{"type": "integer", "description": "Start line (default 1)"},
					"limit":  map[string]any{"type": "integer", "description": "Lines (max 500)"},
				},
				"required": []string{"file"},
			},
		},
	}
}

func memAddTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "mem_add",
			Description: "Create a memory page. One topic per page, kebab-case name, first line = title (#). Refuses to overwrite an existing page (use mem_edit).",
			Parameters: map[string]any{
				"type": "object",
				"properties": orderedProps{
					{"file", map[string]any{"type": "string", "description": "Page name"}},
					{"content", map[string]any{"type": "string", "description": "Markdown, first line = title #"}},
				},
				"required": []string{"file", "content"},
			},
		},
	}
}

func memEditTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "mem_edit",
			Description: "Patch a memory page: old → new, old unique in the page. To append, put the current end of the page in old and the extended version in new.",
			Parameters: map[string]any{
				"type": "object",
				"properties": orderedProps{
					{"file", map[string]any{"type": "string", "description": "Page name"}},
					{"old", map[string]any{"type": "string", "description": "Exact text to replace (unique)"}},
					{"new", map[string]any{"type": "string", "description": "Replacement"}},
				},
				"required": []string{"file", "old", "new"},
			},
		},
	}
}

func editTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "edit",
			Description: "Patch a file by exact replacement: old → new. old must appear EXACTLY once (add context to make it unique). Prefer this over rewriting a whole file.",
			Parameters: map[string]any{
				"type": "object",
				// Ordre voulu : le chemin d'abord (voir orderedProps).
				"properties": orderedProps{
					{"file", map[string]any{"type": "string", "description": "Path"}},
					{"old", map[string]any{"type": "string", "description": "Exact text to replace (unique)"}},
					{"new", map[string]any{"type": "string", "description": "Replacement"}},
				},
				"required": []string{"file", "old", "new"},
			},
		},
	}
}

func writeTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "write",
			Description: "Create or replace a file with the exact content given (parent dirs created, content verbatim, no escaping). ALWAYS use this for a script or any text file — NEVER build one through the shell with echo, cat, python -c or Set-Content: quoting breaks.",
			Parameters: map[string]any{
				"type": "object",
				// Ordre voulu : le chemin d'abord (voir orderedProps).
				"properties": orderedProps{
					{"file", map[string]any{"type": "string", "description": "Path"}},
					{"content", map[string]any{"type": "string", "description": "Full content"}},
				},
				"required": []string{"file", "content"},
			},
		},
	}
}

func bashTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "bash",
			Description: "Run a shell command (" + agentTargetShellName() + " syntax) and return stdout, stderr and exit code: inspect the system, read files and logs, run scripts. To CREATE or REWRITE a file use write instead — never echo/cat/python -c. Avoid destructive commands unless asked.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string", "description": "The command"},
					"timeout": map[string]any{"type": "integer", "description": fmt.Sprintf("Timeout s (default %d, max %d)", toolDefaultTimeout, toolMaxTimeout)},
				},
				"required": []string{"command"},
			},
		},
	}
}

// Caps are the per-request capabilities (tool access) for a chat turn. They let
// a caller (e.g. an ajean.link agent with its own tools/skills toggles) scope
// what the model can do for this conversation, instead of always inheriting the
// machine's global config. Use globalCaps() to fall back to the global config.
type Caps struct {
	// Agent = mode agent actif : un seul interrupteur qui débloque TOUS les
	// outils de l'IA (shell + skills). Un skill est un outil comme un autre.
	Agent bool
	// Internet = accès web actif (serveur Crawl4AI configuré + joignable) : ajoute
	// les outils web_search/web_open/web_read/web_grep. Requiert aussi Agent.
	Internet bool
	// Mem = mode d'accès à la mémoire persistante (off / ondemand / always),
	// indépendant du mode agent. Voir MemMode.
	Mem MemMode
	// Code = mode code actif pour cette discussion (sélecteur Chat/Code) :
	// ajoute les outils codeur (read/grep/glob, git, critères, jobs) et le
	// prompt de rôle. Requiert Agent.
	Code bool
	// Role = rôle du tour en mode code : "" ou "builder" (défaut), "planner",
	// "verifier" (passe de vérification, seule autorisée à marquer un critère
	// passed), "explorer", "code-reviewer". Voir code_roles.go.
	Role string
	// ComputerUse = pilotage du navigateur du conteneur (outils browser_*).
	// Requiert aussi Agent (mêmes actions réelles que bash). Voir computer_use.go.
	ComputerUse bool
	// envInCtx : la date et le dossier de travail partent dans le bloc projet
	// figé (PROJ_SNAPSHOT, chat_envctx.go), pas dans le système. Posé par le seul
	// assemblage d'un tour de discussion avec la clé ; jamais pour une tâche, un
	// sous-agent ou la vérification, qui gardent le système d'avant.
	envInCtx bool
}

// globalCaps reads the machine-wide config — the default when a request doesn't
// specify its own capabilities.
func globalCaps() Caps {
	// Internet inclut la joignabilité du serveur Crawl4AI : « actif ET fonctionnel ».
	// Ainsi le prompt système (chat_tools.go) et les outils fournis (EnabledTools) sont
	// gouvernés par la MÊME condition — sinon le prompt promet web_search alors que
	// l'outil n'existe pas, et le modèle le tape en bash (command not found).
	// Mémoire et accès internet sont des sous-réglages du mode agent (cf. UI web) :
	// sans agent, on ne fournit NI les outils mem_*, NI les outils web. Ça garde le
	// prompt système et les outils cohérents avec l'interface (blocs grisés quand
	// l'agent est off) — l'IA en chat pur répond sans mémoire ni web.
	agent := agentEnabled()
	if !agent {
		return Caps{Agent: false, Internet: false, Mem: MemOff}
	}
	return Caps{Agent: true, Internet: internetEnabled() && crawlReachable(), Mem: memMode(), ComputerUse: computerUseEnabled()}
}

// InjectSkills prepends context system messages to msgs: the decisive-agent
// preamble + machine briefing (when tools are enabled) and the lightweight
// skills directory (when skills are enabled). Merges with an existing system
// message if present. tools = the tools sent on this same turn (see prepareTurn).
func InjectSkills(msgs []Message, caps Caps, tools []Tool) []Message {
	var parts []string
	// Decisive-agent preamble (anti-loop) — only when the model actually has
	// tools, otherwise it nudges a plain chat model to "call tools" it doesn't
	// have, which leaks malformed tool-call text into the answer.
	if bp := baseSystemPrompt(caps, tools); bp != "" {
		parts = append(parts, bp)
	}
	if mp := machineSystemPrompt(caps); mp != "" {
		parts = append(parts, mp)
	}
	if len(parts) == 0 {
		return msgs
	}
	prefix := strings.Join(parts, "\n\n")
	// Un message PROJET en tête (pas de prompt de preset) ne doit pas absorber le
	// préambule : il perdrait son préfixe, et normalizeSystemMessages ne saurait
	// plus le sortir du bloc système commun (voir isProjectSystem).
	if len(msgs) > 0 && msgs[0].Role == "system" && !isProjectSystem(msgs[0]) {
		existing, _ := msgs[0].Content.(string)
		merged := append([]Message{{Role: "system", Content: prefix + "\n\n" + existing}}, msgs[1:]...)
		return merged
	}
	return append([]Message{{Role: "system", Content: prefix}}, msgs...)
}

// prepareTurn calcule UNE fois les outils du tour, puis le préambule à partir
// d'eux : la séquence et les outils renvoyés vont ensemble à runChatTools.
//
// Les calculer deux fois (préambule, puis runChat) payait deux fois les
// tentatives de connexion MCP d'un serveur en panne, et laissait le préambule
// décrire un autre état que les schémas envoyés à côté.
func prepareTurn(msgs []Message, caps Caps) ([]Message, []Tool) {
	tools := EnabledTools(caps)
	return InjectSkills(msgs, caps, tools), tools
}

// steerSystem ajoute une consigne système SANS jamais créer un second message
// système ailleurs qu'en tête : elle est fusionnée dans celui d'ouverture, ou
// posée en première position s'il n'y en a pas.
//
// Un message système ajouté À LA FIN faisait échouer le tour sur les modèles
// dont le gabarit l'interdit : gpt-oss (rôle « developer ») lève « System
// message must be at the beginning », et ce 500 du gabarit remplaçait l'erreur
// d'origine que la nouvelle tentative cherchait justement à contourner —
// l'utilisateur voyait une exception Jinja au lieu du vrai problème.
func steerSystem(msgs []Message, hint string) []Message {
	if len(msgs) > 0 && msgs[0].Role == "system" {
		if existing, ok := msgs[0].Content.(string); ok {
			out := append([]Message(nil), msgs...)
			out[0] = Message{Role: "system", Content: existing + "\n\n" + hint}
			return out
		}
	}
	return append([]Message{{Role: "system", Content: hint}}, msgs...)
}

// normalizeSystemMessages garantit un UNIQUE message système, en tête, dans la
// séquence ENVOYÉE au moteur. Repris de l'amont AJEAN (v0.9.8, issue #26).
//
// steerSystem (ci-dessus) empêche déjà loki d'en poser un en cours de route, mais
// il ne couvre que SES propres consignes : un historique venu d'ailleurs (import,
// preset, conversation migrée, agent de code qui compose sa propre séquence) peut
// encore porter deux systèmes, ou un système ailleurs qu'en position 0. Les
// gabarits stricts — Qwen3.x en --jinja — répondent alors « System message must
// be at the beginning », un 500 qui tue le tour sans rien expliquer.
//
// La séquence renvoyée est une COPIE : l'historique d'origine garde sa forme pour
// l'affichage, la persistance et la compaction. Un modèle qui accepte le système
// n'importe où n'est pas gêné de le recevoir en tête — la normalisation est donc
// sans risque pour tous, et sans effet sur une conversation déjà normale (payload
// identique, garanti par test).
//
// Ce qui dépend du PROJET (description, index mémoire, trackers) ne va PAS dans
// ce bloc mais en tête du premier message utilisateur (AJEAN 0.15.8). Sur un
// modèle hybride (Qwen3.5+, couches récurrentes), llama.cpp ne sait reprendre un
// prompt qu'à un point de sauvegarde, et il n'en pose qu'au DÉBUT des messages
// utilisateur. Laissé dans le système, le contexte projet faisait diverger le
// prompt au milieu du bloc : changer de projet recalculait tout. Au début du 1er
// message user, la divergence tombe pile sur un point de sauvegarde, après le
// système commun, qui reste en cache.
func normalizeSystemMessages(msgs []Message) []Message {
	var sys, proj []string
	sawSystem := false
	rest := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "system" {
			if s, ok := m.Content.(string); ok {
				sawSystem = true
				if strings.TrimSpace(s) == "" {
					continue
				}
				if isProjectSystem(m) {
					proj = append(proj, s)
				} else {
					sys = append(sys, s)
				}
				continue
			}
			// Contenu système non textuel (cas théorique) : on le préserve tel quel
			// plutôt que de le perdre.
		}
		rest = append(rest, m)
	}
	// Aucun message système : rien à réordonner, on rend l'entrée telle quelle —
	// pas de copie inutile sur le chemin le plus fréquent.
	if !sawSystem {
		return msgs
	}
	if len(proj) > 0 {
		if !prependToFirstUser(rest, strings.Join(proj, "\n\n")) {
			// Pas encore de message user (cas théorique) : on les garde en système.
			sys = append(sys, proj...)
		}
	}
	// Des systèmes existaient mais tous vides : on les a retirés (un système vide
	// hors position 0 casserait tout autant), sans en réinsérer.
	if len(sys) == 0 {
		return rest
	}
	out := make([]Message, 0, len(rest)+1)
	out = append(out, Message{Role: "system", Content: strings.Join(sys, "\n\n")})
	return append(out, rest...)
}

// isProjectSystem : message système propre au projet actif (description, index
// mémoire, trackers) ou à la conversation (rappel des pages mémoire lues, posé
// au compactage ; date et dossier de travail avec PROJ_SNAPSHOT), à sortir
// du bloc système commun (voir normalizeSystemMessages) : ce bloc doit rester
// identique partout pour rester en cache.
func isProjectSystem(m Message) bool {
	s, ok := m.Content.(string)
	if !ok || m.Role != "system" {
		return false
	}
	return strings.HasPrefix(s, projectContextPrefix) ||
		strings.HasPrefix(s, memIndexPrefix) ||
		strings.HasPrefix(s, trackerIndexPrefix) ||
		strings.HasPrefix(s, memReminderPrefix) ||
		strings.HasPrefix(s, codeInstructionsPrefix) ||
		strings.HasPrefix(s, envContextPrefix)
}

// prependToFirstUser place ctx en tête du premier message user de msgs (modifié
// en place : msgs est déjà une copie, mais ses Content ne sont PAS recopiés —
// on remplace donc la valeur, jamais on ne mute une tranche partagée).
// Renvoie false s'il n'y a aucun message user.
func prependToFirstUser(msgs []Message, ctx string) bool {
	block := "<project_context>\n" + ctx + "\n</project_context>\n\n"
	for i, m := range msgs {
		if m.Role != "user" {
			continue
		}
		switch c := m.Content.(type) {
		case string:
			msgs[i].Content = block + c
		case []any:
			msgs[i].Content = append([]any{map[string]any{"type": "text", "text": block}}, c...)
		case []map[string]any:
			msgs[i].Content = append([]map[string]any{{"type": "text", "text": block}}, c...)
		default:
			return false
		}
		return true
	}
	return false
}

// EnabledTools returns the tools to advertise on the next inference call.
func EnabledTools(caps Caps) []Tool {
	tools := []Tool{}
	// Passe de VÉRIFICATION du mode code : jeu d'outils fermé, en lecture
	// seule + bash (lancer les tests) + critères. Ni write/edit (elle ne
	// corrige pas), ni mémoire ni web (hors sujet, et chaque schéma coûte du
	// contexte).
	if caps.Code && caps.Role == "verifier" {
		return []Tool{bashTool(), readTool(), grepTool(), globTool(), gitStatusTool(), gitDiffTool(), criteriaTool()}
	}
	// Rôles DÉLÉGUÉS (outil subagent) : lecture seule, et rien d'autre. Ni
	// write/edit (ce qui modifie le dépôt reste dans le fil principal, sous les
	// yeux de l'utilisateur), ni critères (seule la passe de vérification les
	// marque), ni subagent (pas de récursion), ni mémoire ni web — chaque schéma
	// coûte du contexte, et un explorateur n'a que du code à lire.
	if caps.Code && isSubagentRole(caps.Role) {
		tools := []Tool{readTool(), grepTool(), globTool(), gitStatusTool(), gitDiffTool()}
		if caps.Role == "planner" {
			// Le planificateur POSE le contrat (criteria action=add) : c'est la
			// moitié de son travail, et son prompt le lui demande. Marquer un
			// critère « passé » lui reste interdit — allowPass ne vaut que pour
			// la passe de vérification (voir toolCriteria).
			tools = append(tools, criteriaTool())
		}
		if caps.Role == "explorer" || caps.Role == "code-reviewer" {
			// bash : lancer un test, compter des occurrences. Les commandes
			// catastrophiques restent refusées (code_policy.go) et les chemins
			// bornés au dossier de la discussion.
			tools = append(tools, bashTool())
		}
		return tools
	}
	if caps.Agent {
		tools = append(tools, bashTool(), writeTool(), editTool())
		// Mémoire longue de la conversation (chat_recall.go) : le compactage
		// n'archive des blocs QUE quand le mode agent est actif — annoncer recall
		// sans lui donnerait un outil qui ne peut rien trouver.
		tools = append(tools, recallTool(), recallSearchTool())
		// Tâches planifiées : l'IA se donne elle-même des rappels et des veilles
		// récurrentes, et gère les siennes (tasks_tools.go). Cloisonnées par
		// projet : dans un projet, elle ne voit et ne pilote QUE ses tâches.
		// Réservé au mode agent — une tâche sans outils pour agir n'aurait rien
		// à livrer.
		tools = append(tools, taskListTool(), taskCreateTool(), taskUpdateTool(), taskDeleteTool())
	}
	if caps.Code {
		tools = append(tools, readTool(), grepTool(), globTool(), askTool(),
			bashBgTool(), bashTailTool(), gitStatusTool(), gitDiffTool(), gitCloneTool(), criteriaTool())
		// Délégation à un rôle en contexte isolé (code_subagent.go). Réservée au
		// fil principal : un sous-agent ne délègue pas à son tour.
		tools = append(tools, subagentTool())
	}
	// Mémoire = axe indépendant du mode agent : les outils mem_* sont fournis dès
	// que le mode mémoire n'est pas « off » (que l'agent soit actif ou non).
	if caps.Mem != MemOff {
		tools = append(tools, memSearchTool(), memReadTool(), memAddTool(), memEditTool())
		// Trackers : 3e type de mémoire (données datées). Même axe que les pages —
		// un seul outil pour les quatre actions, cf. trackerTool.
		tools = append(tools, trackerTool())
	}
	// Outils web : seulement si le mode agent ET l'accès internet sont actifs.
	// caps.Internet intègre déjà la joignabilité (globalCaps / override web_server.go),
	// donc prompt et outils restent cohérents — pas de web_search halluciné.
	if caps.Agent && caps.Internet {
		tools = append(tools, webSearchTool(), webImagesTool(), webOpenTool(), webReadTool(), webGrepTool())
	}
	// Capture d'écran : seulement si Playwright est réellement présent dans
	// l'image (annoncer un outil absent enverrait le modèle en boucle de
	// réessai). Offerte aussi SANS accès internet quand le mode code est actif :
	// photographier son propre serveur de dev est une vérification locale, pas
	// une sortie sur le web — et sans elle, le modèle croyait n'avoir aucun
	// navigateur et perdait des minutes à installer puppeteer. La borne aux
	// adresses locales vit dans toolWebScreenshot.
	// Voir une image du DISQUE : seulement quand la vision est réellement active
	// (projecteur MMPROJ déclaré). Sinon l'outil ne saurait que renvoyer une
	// erreur, et l'annoncer ferait croire au modèle qu'il a des yeux qu'il n'a
	// pas — il promettrait alors de regarder, puis se contredirait.
	if caps.Agent && visionEnabled() {
		tools = append(tools, seeImageTool())
	}
	if caps.Agent && (caps.Internet || caps.Code) && screenshotAvailable() {
		tools = append(tools, webScreenshotTool())
	}
	// Pilotage de navigateur (browser_*) : l'IA ouvre une page, lit ses éléments
	// interactifs NUMÉROTÉS et agit par numéro. Ce sont des actions réelles sur
	// le web (cliquer, taper, valider un formulaire) : même niveau de confiance
	// que bash, donc mode agent requis, plus son propre interrupteur (Réglages).
	// Voir computer_use.go.
	if caps.Agent && caps.ComputerUse {
		tools = append(tools, computerUseTools()...)
	}
	// Outils MCP : serveurs tiers configurés par le propriétaire de la machine.
	// Comme bash, ils exécutent du code arbitraire côté hôte → réservés au mode
	// agent. La découverte est paresseuse et cachée (voir mcp_client.go).
	if caps.Agent {
		tools = append(tools, mcpTools()...)
	}
	// Postes distants : PAS de nouveaux outils. L'IA garde bash/write/edit ; c'est
	// leur CIBLE D'EXÉCUTION qui change quand un poste est sélectionné (voir le
	// routage dans la boucle d'outils et agentTargetSlug). Redonner des outils que
	// le modèle a déjà (node__…__shell alors qu'il a bash) doublonnait le catalogue.
	return tools
}

// StreamEvent is what a ChatCallback receives for each piece of streamed output.
// Exactly one of {Content, Reasoning, ToolUsed, Stats, Err, DropReasoning, Echo}
// is set per call.
type StreamEvent struct {
	Content   string
	Reasoning string
	ToolUsed  *ToolUsedEvent
	Stats     *StatsEvent
	Ask       *AskEvent
	Err       error
	// DropReasoning demande à l'UI de retirer la dernière bulle de raisonnement :
	// le modèle a « pensé sans agir » et on relance le tour, ce raisonnement-là
	// est mort-né et ne doit pas rester à l'écran (sinon double raisonnement).
	DropReasoning bool
	// Compacting signale une compaction déclenchée EN COURS DE TOUR (boucle
	// d'outils) : true à l'entrée du résumé, false à la sortie. Même bannière que
	// la compaction proactive de début de tour, qui elle est émise directement par
	// Conversation.generate. nil = l'événement ne parle pas de compaction.
	Compacting *bool
	// NewHistory publie la vue modèle APRÈS une compaction faite en cours de tour.
	// Sans elle la compaction est perdue : l'appelant reconstruit l'historique en
	// ajoutant les messages du tour à sa copie d'AVANT compaction, donc le fil
	// complet revient et le tour suivant re-déborde aussitôt (43% → 92% en un
	// message). Cette liste contient DÉJÀ tout le tour en cours : l'appelant doit
	// REMPLACER son historique par elle (préfixe système injecté retiré), pas l'y
	// ajouter.
	NewHistory []Message
	// Echo : raisonnement séparé de la complétion qui porte la réponse finale,
	// à ranger avec elle (withEcho). Émis une fois, en fin de tour, et seulement
	// avec REASONING_ECHO=on sur le moteur local : les autres tours n'en voient
	// jamais. Les messages tool_calls du tour portent déjà le leur (extra).
	Echo *ReasoningEcho
}

// AskEvent : l'outil ask — question structurée posée à l'utilisateur, rendue
// par l'UI comme une carte à boutons. Le tour se termine juste après.
type AskEvent struct {
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
}

type ToolUsedEvent struct {
	Name   string
	Label  string // user-visible summary (skill name or the command)
	Result string // tool output (stdout/stderr/exit for run_shell, skill body for read_skill)
	Done   bool   // false = call announced (command only); true = result is ready
	Typing bool   // true = command still being written (partial), no spinner yet
	// Body : contenu en cours d'écriture par un outil d'écriture (write/edit,
	// mem_add/mem_edit), diffusé ligne à ligne pendant que le modèle le tape,
	// pour que la bulle se remplisse en direct au lieu de rester figée puis de
	// s'ouvrir d'un coup. Transitoire : seul Diff (état final) est rejoué.
	// Ce n'est que la FIN du corps (bodyTail) : BodyTail dit que le début a été
	// omis, BodyLines donne le nombre réel de lignes pour le « +N ».
	Body      string
	BodyTail  bool
	BodyLines int
	// Diff : lignes ajoutées/retirées quand l'outil a MODIFIÉ quelque chose
	// (edit, mem_add, mem_edit). L'UI les affiche en vert (+) et rouge (-).
	Diff []DiffLine
	// Added / Removed : nombre RÉEL de lignes ajoutées / retirées. Diff est
	// tronqué pour l'affichage, on ne peut donc pas recompter à partir de lui.
	Added, Removed int
	// Image : chemin (relatif au dossier de travail) d'une image PRODUITE par
	// l'outil — aujourd'hui la capture de web_screenshot. L'UI l'affiche dans la
	// bulle de l'outil. Sans ça, la capture n'apparaissait QUE si le modèle
	// pensait à recopier la ligne markdown rendue par l'outil : un petit modèle
	// l'oublie, et l'utilisateur ne voyait jamais l'image qu'il avait demandée.
	Image string
	// Shot : identifiant de la capture du navigateur prise après une action
	// browser_* (cu_autoshot.go). Aperçu EN DIRECT seulement : gardé en RAM,
	// jamais envoyé au modèle, retiré du journal en fin de tour.
	Shot string
	// ResultChars : taille RÉELLE du résultat (en runes), même quand Result n'en
	// porte qu'un APERÇU. Sert au compteur « ~N tok » de la bulle.
	ResultChars int
	// ResultID : identifiant (voir saveToolResult) permettant à l'UI de CHARGER
	// le résultat complet à la demande via /api/chat/tool-result — le flux ne
	// transporte que l'aperçu. Vide = Result est déjà complet.
	ResultID string
}

// toolPreviewChars borne l'aperçu de résultat d'outil envoyé à l'UI (AJEAN
// 0.15.1). Le flux — et son rejeu au rechargement — ne transporte plus que
// ça ; le reste se charge au clic sur « voir plus » (tool_results.go).
// Avant, chaque résultat partait en entier (jusqu'à 12 000 caractères), et un
// long fil d'agent en rejouait des centaines à chaque ouverture.
const toolPreviewChars = 1600

// toolResultPreview renvoie (aperçu, taille réelle en runes, coupé?).
func toolResultPreview(s string) (string, int, bool) {
	r := []rune(s)
	if len(r) <= toolPreviewChars {
		return s, len(r), false
	}
	return string(r[:toolPreviewChars]), len(r), true
}

// fillToolResult pose sur l'événement l'aperçu envoyé à l'UI, la taille réelle
// et, si le résultat est coupé, l'id qui permet de charger le reste. Si
// l'enregistrement échoue, on envoie le résultat entier (il reste borné par les
// plafonds propres à chaque outil).
func fillToolResult(ev *ToolUsedEvent, result string) *ToolUsedEvent {
	prev, n, cut := toolResultPreview(result)
	ev.ResultChars = n
	ev.Result = result
	if cut {
		if id := saveToolResult(result); id != "" {
			ev.Result = prev
			ev.ResultID = id
		}
	}
	return ev
}

// dedupableTool indique si un appel RIGOUREUSEMENT identique (même outil, mêmes
// arguments) doit être court-circuité au lieu d'être rejoué. Vrai pour les outils
// où un ré-appel identique n'a pas de sens ou produit une fausse erreur (rejouer
// une écriture déjà appliquée → « old introuvable »).
//
// FAUX pour bash : relancer la MÊME commande est un usage courant et légitime —
// l'IA crée un fichier, le lance, le modifie, puis le RELANCE avec la même ligne ;
// le résultat change parce que le fichier a changé. La dédup la bloquait par un
// « [déjà fait] » exaspérant. bash a des effets de bord : on le laisse toujours
// s'exécuter. Même raison pour bash_bg / bash_tail (suivre un job, c'est relire
// la même sortie qui, elle, avance).
//
// FAUX aussi pour see_image et tout le pilotage de navigateur (browser_*) :
//   - leur résultat dépend de l'ÉTAT VIVANT de la page (un browser_snapshot sans
//     argument a forcément la même clé de dédup à chaque appel, alors que la page
//     a changé) — les dédupliquer fige l'IA sur un vieux cliché ;
//   - surtout, browser_screenshot et see_image portent leur IMAGE dans un message
//     à part (visionImg) que le chemin de dédup ne rejoue PAS : l'IA recevrait
//     « [déjà fait] » SANS l'image et tournerait en boucle.
func dedupableTool(name string) bool {
	switch name {
	case "bash", "bash_bg", "bash_tail", "see_image":
		return false
	}
	return !strings.HasPrefix(name, "browser_")
}

// repeatedCallResult construit ce qu'on renvoie quand le modèle redemande un
// appel RIGOUREUSEMENT identique (même outil, mêmes arguments) dans le même tour.
// L'appel n'est jamais rejoué — on répond depuis le résultat mémorisé.
//
// Le plafond d'itérations et l'anti-boucle ont été retirés en v0.6.3 parce qu'ils
// coupaient des recherches légitimes ; il ne restait donc plus RIEN pour arrêter
// un modèle qui redemande dix fois la même page. Et le mécanisme se retournait
// contre lui-même : la note « déjà exécuté » était collée APRÈS le contenu, donc
// noyée en fin d'un résultat de plusieurs milliers de caractères — le modèle
// voyait le contenu, pas l'avertissement, et recommençait.
//
// D'où l'escalade : la note passe EN TÊTE, et à partir de la 2ᵉ redemande on ne
// renvoie plus la charge utile du tout. Le tour n'est pas coupé (le modèle garde
// la main), mais redemander la même chose ne rapporte plus rien — ni contenu, ni
// contexte consommé.
func repeatedCallResult(prev string, repeats int) string {
	if repeats >= 2 {
		return "[déjà fait] Cet appel exact a déjà été exécuté " + strconv.Itoa(repeats) +
			" fois dans ce tour ; son résultat est plus haut dans la conversation. " +
			"Ne le redemande plus : réponds avec ce que tu as, ou change d'approche " +
			"(autre URL, autres arguments, web_grep pour cibler)."
	}
	return "[déjà fait] Appel identique déjà exécuté dans ce tour — non rejoué. " +
		"Voici à nouveau son résultat ; ne le redemande pas une troisième fois.\n\n" + prev
}

// writeBodyKey returns the argument holding the text an écriture tool is about
// to commit — the part worth showing live in the bubble — or "" for tools that
// have no such body (bash, lectures, recherches).
func writeBodyKey(tool string) string {
	switch tool {
	case "write", "mem_add":
		return "content"
	case "edit", "mem_edit":
		return "new"
	}
	return ""
}

// previewArgDone pulls the (possibly incomplete) string value of key out of a
// streaming tool-call arguments JSON (best-effort: it tolerates a truncated tail
// and basic escapes), et dit si la valeur est COMPLÈTE (guillemet fermant reçu)
// — de quoi agir sur un argument avant la fin du flux. Pour l'affichage en
// direct, argPreview fait la même lecture sans tout relire à chaque morceau.
func previewArgDone(args, key string) (string, bool) {
	i := strings.Index(args, "\""+key+"\"")
	if i < 0 {
		return "", false
	}
	rest := args[i+len(key)+2:]
	if j := strings.Index(rest, ":"); j >= 0 {
		rest = rest[j+1:]
	} else {
		return "", false
	}
	q := strings.Index(rest, "\"")
	if q < 0 {
		return "", false
	}
	rest = rest[q+1:]
	var b strings.Builder
	closed := false
	for x := 0; x < len(rest); x++ {
		c := rest[x]
		if c == '\\' && x+1 < len(rest) {
			switch rest[x+1] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			default:
				b.WriteByte(rest[x+1])
			}
			x++
			continue
		}
		if c == '"' {
			closed = true
			break
		}
		b.WriteByte(c)
	}
	return b.String(), closed
}

// argPreview : previewArgDone au fil du flux. Pendant l'écriture d'un fichier,
// previewArg relisait et redécodait tout le JSON des arguments à CHAQUE morceau
// — quadratique, et ce CPU-là manque au décodage quand des experts tournent sur
// le processeur. Ici on retient où on en est et on ne décode que les octets
// neufs. Même règle que previewArgDone : première occurrence de "clé", puis
// premier ':', puis premier '"' ; \r ignoré, échappement inconnu (\uXXXX
// compris) recopié tel quel sans la barre. Seule différence, sur un préfixe :
// une barre oblique finale reste en attente de son second octet au lieu d'être
// écrite — elle n'est pas encore un caractère.
type argPreview struct {
	key   string
	pat   string // `"key"`
	pos   int    // octets de args déjà consommés
	phase int    // 0 clé, 1 ':', 2 '"' ouvrant, 3 valeur, 4 valeur fermée
	val   strings.Builder
	lines int // '\n' de la valeur décodée
}

func newArgPreview(key string) *argPreview {
	return &argPreview{key: key, pat: `"` + key + `"`}
}

// update consomme la suite de args, qui doit prolonger celui des appels
// précédents (les arguments d'un appel ne font que grandir pendant le flux).
func (a *argPreview) update(args string) {
	for a.pos < len(args) && a.phase < 4 {
		switch a.phase {
		case 0:
			// La clé peut être coupée entre deux morceaux : on reprend un peu avant.
			s := a.pos - (len(a.pat) - 1)
			if s < 0 {
				s = 0
			}
			i := strings.Index(args[s:], a.pat)
			if i < 0 {
				a.pos = len(args)
				return
			}
			a.pos, a.phase = s+i+len(a.pat), 1
		case 1, 2:
			sep := byte(':')
			if a.phase == 2 {
				sep = '"'
			}
			j := strings.IndexByte(args[a.pos:], sep)
			if j < 0 {
				a.pos = len(args)
				return
			}
			a.pos += j + 1
			a.phase++
		case 3:
			seg := args[a.pos:]
			k := strings.IndexAny(seg, "\\\"")
			if k < 0 {
				k = len(seg)
			}
			a.val.WriteString(seg[:k])
			a.lines += strings.Count(seg[:k], "\n")
			a.pos += k
			if k == len(seg) {
				return
			}
			if seg[k] == '"' {
				a.pos++
				a.phase = 4
				return
			}
			if k+1 >= len(seg) {
				return // échappement coupé : on attend son second octet
			}
			switch e := seg[k+1]; e {
			case 'n':
				a.val.WriteByte('\n')
				a.lines++
			case 't':
				a.val.WriteByte('\t')
			case 'r':
			default: // '"', '\\', '/', et l'inconnu recopié tel quel
				a.val.WriteByte(e)
				if e == '\n' {
					a.lines++
				}
			}
			a.pos += 2
		}
	}
}

// value : la valeur décodée jusqu'ici (sans copie).
func (a *argPreview) value() string { return a.val.String() }

// done : guillemet fermant reçu.
func (a *argPreview) done() bool { return a.phase == 4 }

// lineCount : bodyLineCount(value()) sans relire la valeur. Recompter les '\n'
// de tout le corps à chaque ligne émise restait quadratique.
func (a *argPreview) lineCount() int {
	n := a.val.Len()
	switch {
	case n == 0:
		return 0
	case a.val.String()[n-1] != '\n':
		return a.lines + 1
	case n == 1:
		return 0 // "\n" seul : bodyLineCount le compte vide
	}
	return a.lines
}

// bodyTailLines / bodyTailBytes : ce qu'un événement de frappe montre du corps.
const (
	bodyTailLines = 40
	bodyTailBytes = 4096
)

// bodyTail : la fin du corps en cours d'écriture, ses 40 dernières lignes et
// 4 Kio au plus, coupée en début de ligne (en début de caractère pour une ligne
// seule plus longue). cut : le début a été omis. La bulle ne fait défiler que
// la fin de toute façon, et le diff final reste complet.
func bodyTail(s string) (tail string, cut bool) {
	lo := len(s) - bodyTailBytes
	if lo < 0 {
		lo = 0
	}
	start, n, end := -1, 0, len(s)
	for {
		i := strings.LastIndexByte(s[:end], '\n')
		if i < 0 {
			if lo == 0 {
				start = 0
			}
			break
		}
		if i+1 < lo {
			break
		}
		n++
		start = i + 1
		if n == bodyTailLines {
			break
		}
		end = i
	}
	if start < 0 {
		start = lo
		for start < len(s) && !utf8.RuneStart(s[start]) {
			start++
		}
	}
	return s[start:], start > 0
}

// bodyLineCount : lignes du corps comme les compte l'UI (bodyLineCount en JS) —
// un saut de ligne final n'ouvre pas de ligne de plus.
func bodyLineCount(s string) int {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// StatsEvent carries llama.cpp's per-completion timing (final chunk).
type StatsEvent struct {
	PromptTokens    int     `json:"prompt_tokens,omitempty"`
	PromptPerSecond float64 `json:"prompt_per_second,omitempty"`
	PromptMs        float64 `json:"prompt_ms,omitempty"`
	GenTokens       int     `json:"gen_tokens,omitempty"`
	GenPerSecond    float64 `json:"gen_per_second,omitempty"`
	GenMs           float64 `json:"gen_ms,omitempty"`
	// Taille TOTALE du prompt traité ce tour (préfixe caché compris), issue de
	// `usage.prompt_tokens`. 0 si le backend ne renvoie pas d'usage.
	PromptTokensTotal int `json:"prompt_tokens_total,omitempty"`
	// Morceaux de raisonnement reçus SÉPARÉS par le moteur (reasoning_content),
	// llama-server local seulement. Sans REASONING_ECHO, Loki ne renvoie jamais
	// ce raisonnement au modèle : ces jetons font partie de GenTokens mais pas de
	// la requête suivante. Un morceau vaut au plus un jeton, donc ce compte ne
	// peut que sous-estimer — le sens sans danger : on compacte au pire comme
	// avant, jamais trop tard. Voir ctxAfter.
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
	// echoed : ce raisonnement-là repart dans la requête suivante
	// (REASONING_ECHO) — ctxAfter ne le retire alors pas. Jamais sérialisé.
	echoed bool
	// Conseil ponctuel quand le cache de prompts n'a pas tenu après un travail
	// annexe (voir noteEnginePrompt). Vide la plupart du temps.
	CacheHint string `json:"cache_hint,omitempty"`
	// Télémétrie d'affichage (perf_log.go), jamais relue pour décider quoi que ce
	// soit. Pointeurs : nil = le moteur ne l'a pas dit, ce qui n'est pas 0 — un
	// cache_n à 0 (tout recalculé) est précisément ce qu'il faut pouvoir montrer.
	CacheTokens   *int   `json:"cache_tokens,omitempty"`
	DraftN        *int   `json:"draft_n,omitempty"`
	DraftAccepted *int   `json:"draft_accepted,omitempty"`
	TTFTms        *int64 `json:"ttft_ms,omitempty"`
	// Posés sur le dernier événement de la complétion seulement, une fois
	// celle-ci rangée : jetons du tour précédent qu'il a fallu recalculer, et
	// s'il faut le signaler (seuil relevé sur un modèle hybride).
	Lost      *int   `json:"lost,omitempty"`
	LostAfter string `json:"lost_after,omitempty"`
	LostAlert bool   `json:"lost_alert,omitempty"`
	Kind      string `json:"kind,omitempty"`
}

// ctxAfter : taille du contexte que la requête SUIVANTE renverra réellement au
// moteur — le prompt de cette complétion plus ce qu'elle a généré, MOINS son
// raisonnement, jamais renvoyé. Compter ce raisonnement gonflait le contexte de
// 1 à 8 k jetons par étape, et la compaction (avec perte) partait trop tôt.
// Borné par GenTokens : le compte de morceaux ne retire jamais plus que généré.
// Raisonnement renvoyé (echoed) : il fait partie de la requête suivante, il
// reste compté.
func (s StatsEvent) ctxAfter() int {
	if s.echoed {
		return s.PromptTokensTotal + s.GenTokens
	}
	return s.PromptTokensTotal + s.GenTokens - min(s.GenTokens, s.ReasoningTokens)
}

// ChatCallback receives stream events. Return false to abort the stream.
type ChatCallback func(StreamEvent) bool

// completionResp / streamChunk model the subset of llama.cpp's
// OpenAI-compatible /v1/chat/completions response that we care about.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string     `json:"content"`
			ReasoningContent string     `json:"reasoning_content"`
			ToolCalls        []ToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	// llama.cpp's "timings" appears on the final chunk and on intermediate
	// /completion endpoint responses. Snake-case mapping per llama.cpp source.
	Timings *struct {
		PromptN         int     `json:"prompt_n"`
		PromptMs        float64 `json:"prompt_ms"`
		PromptPerSecond float64 `json:"prompt_per_second"`
		PredictedN      int     `json:"predicted_n"`
		PredictedMs     float64 `json:"predicted_ms"`
		PredictedPerSec float64 `json:"predicted_per_second"`
	} `json:"timings"`
	// Chunk final (include_usage) : taille totale du prompt, hors choices.
	// ⚠️ Rien de plus ici : un champ typé de travers fait échouer le décodage
	// du chunk ENTIER, qui est sauté — texte final, finish_reason et usage
	// compris. Les compteurs de télémétrie (cache_n, draft_n, cached_tokens) se
	// lisent à part, sans jamais échouer (perfWire, perf_log.go).
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// runChat drives the full inference loop including tool calling.
// On finish_reason="tool_calls" we execute locally, append a "tool" message
// and call /v1/chat/completions again — up to 8 iterations as a safety cap.
const thinkClose = "</think>"

// runChat returns `extra`: the tool-turn messages (assistant-with-tool_calls and
// their tool results) it appended during the agentic loop. The caller persists
// these into the durable history BEFORE the final assistant text, so the model
// keeps the trace of what it already read/ran across user turns — otherwise it
// re-invokes the same skill/command every turn (it has no memory of having done
// it) and can confabulate paths/results it can no longer see.
// friendlyLLMError transforme une erreur de transport vers llama-server en un
// message clair. Le cas le plus fréquent — « connection refused » (Linux) /
// « actively refused » (Windows) — arrive quand le moteur redémarre ou charge
// encore le modèle ; l'erreur brute (« dial tcp 127.0.0.1:8080… ») ne dit rien à
// l'utilisateur. On garde le silence sur une annulation volontaire (/stop).
// On classe d'abord sur les erreurs TYPÉES (errors.Is / net.Error) : la
// reconnaissance par sous-chaîne dépend de la langue et du format des messages
// de l'OS, et « connexion refusée » d'un Windows français ne ressemble à aucun
// des motifs anglais. Les sous-chaînes restent en second rideau, pour les
// erreurs enveloppées par une bibliothèque qui perd le type d'origine.
func friendlyLLMError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return err // /stop : pas d'alarme
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return errEngineDown()
	case errors.Is(err, context.DeadlineExceeded), isNetTimeout(err):
		return fmt.Errorf("⚠️ Le moteur (llama-server) met trop de temps à répondre (port %d) — il est peut-être surchargé ou en plein chargement. Réessaie dans un instant.", LLMPort())
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, syscall.ECONNRESET):
		return errEngineReset()
	}
	low := strings.ToLower(err.Error())
	switch {
	case strings.Contains(low, "context canceled"):
		return err
	case strings.Contains(low, "connection refused"), strings.Contains(low, "actively refused"), strings.Contains(low, "connectex"), strings.Contains(low, "no connection could be made"):
		return errEngineDown()
	case strings.Contains(low, "timeout"), strings.Contains(low, "deadline exceeded"):
		return fmt.Errorf("⚠️ Le moteur (llama-server) met trop de temps à répondre (port %d) — il est peut-être surchargé ou en plein chargement. Réessaie dans un instant.", LLMPort())
	case strings.Contains(low, "eof"), strings.Contains(low, "connection reset"):
		return errEngineReset()
	}
	return err
}

func errEngineDown() error {
	return fmt.Errorf("⚠️ Le moteur (llama-server) ne répond pas sur le port %d. Il est probablement en train de démarrer ou de charger le modèle — réessaie dans quelques secondes.", LLMPort())
}

func errEngineReset() error {
	return fmt.Errorf("⚠️ Connexion au moteur (llama-server, port %d) interrompue — il a peut-être redémarré. Réessaie.", LLMPort())
}

// streamCutError explique un flux de complétion coupé en cours de route. Cas à
// part de friendlyLLMError : ici la requête avait ABOUTI (200 reçu, tokens déjà
// reçus), c'est la lecture qui a lâché. Le dire autrement qu'un « le moteur ne
// répond pas » évite d'envoyer l'utilisateur vérifier un moteur qui va bien.
func streamCutError(err error) error {
	if errors.Is(err, bufio.ErrTooLong) {
		return fmt.Errorf("⚠️ Réponse du moteur illisible : une ligne du flux dépasse la taille maximale (%d Mio). C'est presque toujours un appel d'outil démesuré (écriture d'un très gros fichier). Le tour est abandonné pour ne pas exécuter un appel tronqué.", 8)
	}
	return fmt.Errorf("⚠️ Le flux de réponse du moteur (llama-server, port %d) a été coupé en cours de route : %v. La réponse est incomplète et le tour est abandonné — réessaie.", LLMPort(), err)
}

// isNetTimeout : une erreur réseau qui se déclare elle-même comme un délai
// dépassé (net.Error.Timeout), quel que soit son libellé.
func isNetTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// toolCallLabel dérive le libellé HUMAIN d'un appel d'outil : la commande, le
// chemin, la requête… Il est annoncé à l'interface AVANT l'exécution — sans ça,
// une commande shell lente laisse l'écran figé sans rien dire — et il sert
// ensuite d'argument principal à plusieurs outils.
//
// Cette double fonction est un piège : un outil absent de cette table reçoit un
// libellé VIDE, et si son exécution lit ce libellé (see_image, read, bash…), il
// s'exécute sur une chaîne vide. Vécu à l'ajout de see_image : l'outil répondait
// « chemin de fichier manquant » quoi qu'on lui passe, sans que rien d'autre ne
// bronche. D'où l'extraction : une table pareille se teste.
func toolCallLabel(name string, args map[string]any) string {
	label := ""
	switch name {
	case "mem_search", "web_search", "web_images", "recall_search":
		label, _ = args["query"].(string)
	case "mem_read", "mem_add", "mem_edit", "edit", "write", "read", "git_diff", "see_image":
		label, _ = args["file"].(string)
	case "recall":
		label, _ = args["id"].(string)
	case "tracker":
		// Libellé lisible : « nom » ou « action nom », pas un dump d'arguments.
		label = strings.TrimSpace(str(args["action"]) + " " + str(args["name"]))
	case "bash", "bash_bg":
		label, _ = args["command"].(string)
	case "grep", "glob":
		label, _ = args["pattern"].(string)
	case "ask":
		label, _ = args["question"].(string)
	case "criteria":
		label, _ = args["action"].(string)
	case "bash_tail":
		label, _ = args["id"].(string)
	case "git_clone":
		label, _ = args["url"].(string)
	case "web_open", "web_read":
		label, _ = args["url"].(string)
	case "web_grep":
		u, _ := args["url"].(string)
		p, _ := args["pattern"].(string)
		label = p + " @ " + u
	default:
		// Outils MCP : libellé = un aperçu compact des arguments.
		if isMCPTool(name) {
			label = mcpArgLabel(args)
		}
	}
	return label
}

// turnReasoning : ce que la requête dit du raisonnement, tiré de la
// configuration. Partagé par runChat et le préchauffage (chat_prewarm.go).
func turnReasoning(cfg map[string]string) (effortWanted, effort string, thinkOff bool, kwargs map[string]any) {
	// Intensité du raisonnement, passée telle quelle au gabarit du modèle. Vide
	// = on n'envoie rien. Réglage par preset, donc de fait par modèle.
	effortWanted = reasoningEffortValue(cfg["REASONING_EFFORT"])
	// …sauf si le gabarit de CE modèle a déjà refusé ce niveau (llm_effort.go) :
	// on part alors directement sur la traduction apprise, sans repayer le 500.
	effort = effortResolve(effortWanted)
	// Le raisonnement est-il interdit pour ce tour ? « aucune » compte comme une
	// interdiction, y compris quand le repli ci-dessus a retiré le niveau : c'est
	// `enable_thinking` qui porte alors la consigne, seul.
	thinkOff = reasoningExplicitlyOff(cfg["REASONING"]) || effortWanted == "none"
	// Ce que le gabarit du modèle doit savoir, dans SA langue : `reasoning_effort`
	// ne parle qu'aux gabarits qui le lisent, `enable_thinking` parle aux modèles
	// hybrides (Qwen3 & co). Sans ça, « aucune » n'avait aucun effet sur eux : le
	// modèle réfléchissait pendant que l'interface annonçait le contraire.
	kwargs = reasoningTemplateKwargs(thinkOff, effort)
	return effortWanted, effort, thinkOff, kwargs
}

// turnEchoPolicy : la politique de REASONING_ECHO d'une requête ; off = preset
// externe ou repli en cours, rien ne repart.
func turnEchoPolicy(off bool) echoPolicy {
	if off {
		return echoPolicy{}
	}
	return currentEchoPolicy()
}

// wireMessages : les messages tels qu'ils partent au moteur — un seul système
// en tête, la consigne de fin si les outils sont neutralisés, le raisonnement
// renvoyé selon pol — et le nombre de raisonnements renvoyés.
func wireMessages(messages []Message, toolChoiceNone bool, pol echoPolicy) ([]Message, int) {
	sent := normalizeSystemMessages(messages)
	// Marque de relais d'image (KEEP_TURN_IMAGES) : propre à Loki, jamais au
	// moteur. Sans la clé, aucune : la tranche repart telle quelle.
	sent = stripRelayTags(sent)
	if toolChoiceNone {
		sent = withTrailingHint(sent, toolsOffHint)
	}
	return echoMessages(sent, pol)
}

// chatPayloadOpts : ce qui, en plus des messages, façonne une requête de chat.
type chatPayloadOpts struct {
	tools          []Tool
	disableTools   bool
	toolChoiceNone bool
	effort         string
	kwargs         map[string]any
	// noSampling : sans l'échantillonnage du preset. Pour le résumé en
	// continuation (COMPACT_CONTINUATION, chat_compact_cont.go), qui garde celui
	// du résumeur d'aujourd'hui — température 0.2 seule : une pénalité de
	// répétition jouerait contre la reprise fidèle des noms, chemins et chiffres.
	// L'échantillonnage ne touche pas au prompt : le cache n'y perd rien.
	noSampling bool
}

// buildChatPayload : le corps d'une requête de complétion de chat, à partir des
// messages déjà passés par wireMessages. UN seul endroit le construit : le
// préchauffage (chat_prewarm.go) envoie ce même corps, aux seuls champs de
// réponse près (stream, max_tokens), et ne peut donc pas en dériver.
func buildChatPayload(ep chatEndpoint, sent []Message, temperature float64, o chatPayloadOpts) map[string]any {
	payload := map[string]any{
		"model": ep.Model,
		// Les images de l'historique y sont rangées par référence
		// (chat_images.go) : on remet leurs octets juste avant l'envoi.
		"messages":    expandImageRefs(sent),
		"stream":      true,
		"temperature": temperature,
		// include_usage → chunk final avec `usage.prompt_tokens` = taille TOTALE
		// du prompt (préfixe caché compris), contrairement à timings.prompt_n qui
		// ne compte que les tokens nouvellement traités. Sert au comptage exact du
		// contexte (sinon le system prompt déjà en cache n'est pas recompté).
		"stream_options": map[string]any{"include_usage": true},
	}
	// Échantillonnage du preset (top_p/top_k/min_p/pénalités, et TEMP qui
	// l'emporte sur la température ci-dessus). Posé AVANT le raisonnement : les
	// deux blocs écrivent des clés disjointes, mais l'ordre rend explicite que
	// c'est bien loki qui a le dernier mot sur `chat_template_kwargs`.
	if !o.noSampling {
		applySampling(payload)
	}
	if o.effort != "" {
		payload["reasoning_effort"] = o.effort
	}
	// chat_template_kwargs est propre à llama.cpp (--jinja) : une API
	// distante stricte (OpenAI) refuse un argument inconnu (400).
	if o.kwargs != nil && !ep.External {
		payload["chat_template_kwargs"] = o.kwargs
	}
	if len(o.tools) > 0 && !o.disableTools {
		payload["tools"] = o.tools
		// The model sometimes emits parallel tool calls, which this llama.cpp
		// build serialises as two concatenated JSON objects in one arguments
		// string ("{...}{...}") and then fails to parse (HTTP 500). Forcing a
		// single tool call per turn avoids that.
		payload["parallel_tool_calls"] = false
		if o.toolChoiceNone {
			payload["tool_choice"] = "none"
		}
	}
	return payload
}

// injectQueued (optionnel, variadique pour ne pas toucher aux appels hors chat)
// est consulté à CHAQUE frontière d'étape de la boucle d'outils : il renvoie les
// messages utilisateur mis en file PENDANT la génération, pour que le modèle les
// prenne en compte dans la SUITE de sa réponse (AJEAN 0.14.0) au lieu
// d'obliger à arrêter puis relancer. Ajoutés à `messages` ET à `extra`.
func runChat(ctx context.Context, messages []Message, temperature float64, caps Caps, cb ChatCallback, injectQueued ...func() []Message) ([]Message, error) {
	return runChatTools(ctx, messages, EnabledTools(caps), temperature, caps, cb, injectQueued...)
}

// runChatTools est runChat avec les outils déjà calculés par prepareTurn : ceux
// que décrit le préambule, à l'octet près.
func runChatTools(ctx context.Context, messages []Message, tools []Tool, temperature float64, caps Caps, cb ChatCallback, injectQueued ...func() []Message) ([]Message, error) {
	// KEEP_TURN_IMAGES (chat_keep_images.go) : nil sans la clé, et tout se
	// passe alors comme avant. En fin de tour, une image gardée à l'essai mais
	// jamais mesurée redevient éphémère, quelle que soit la sortie de la boucle.
	kt := newKeepImages(ReadConfig(), perfTagOf(ctx))
	extra, err := runChatLoop(ctx, kt, messages, tools, temperature, caps, cb, injectQueued...)
	return kt.finish(extra), err
}

// runChatLoop : la boucle de runChatTools.
func runChatLoop(ctx context.Context, kt *keepImages, messages []Message, tools []Tool, temperature float64, caps Caps, cb ChatCallback, injectQueued ...func() []Message) ([]Message, error) {
	var extra []Message
	// Bloc projet figé de la discussion (PROJ_SNAPSHOT, chat_projsnap.go) : nil
	// hors tour de discussion avec la clé. Masqué pour la suite du contexte —
	// un sous-agent lancé par un outil a son propre fil, pas ce bloc.
	snapTurn := projSnapTurnFrom(ctx)
	if snapTurn != nil {
		ctx = withProjSnapTurn(ctx, nil)
	}
	// newHistory publie l'historique réécrit en cours de tour (compaction,
	// réduction). Le début du prompt change de toute façon : avec la clé, le
	// bloc figé y est remplacé par le vivant et les mises à jour retirées.
	// publishHistory : la même publication, sans rafraîchir le bloc projet —
	// pour un retrait d'images gardées (KEEP_TURN_IMAGES), qui ne touche pas au
	// début du prompt. KEEP_TURN_IMAGES : une image non gardée n'entre pas dans
	// l'historique par ce chemin non plus. Sans la clé, publication telle quelle.
	publishHistory := func() {
		kt.rewritten()
		cb(StreamEvent{NewHistory: kt.publishable(append([]Message(nil), messages...))})
	}
	newHistory := func() {
		if snapTurn != nil {
			messages = snapTurn.refresh(messages)
		}
		publishHistory()
	}
	// Some backends (vanilla llama.cpp builds) don't populate `reasoning_content`
	// in streaming mode: the model's <think> block (opened by the chat template)
	// arrives inline in `content`, terminated by a literal </think>. When
	// reasoning is enabled we split that out ourselves so the UI's reasoning
	// bubble works regardless of backend. The ik_llama.cpp fork already sends
	// reasoning_content, in which case we leave content untouched.
	chatCfg := ReadConfig()
	// Nature et conversation de ces complétions, pour la télémétrie seulement
	// (perf_log.go) : rien de tout ça ne part dans la requête.
	ptag, perfIter := perfTagOf(ctx), 0
	reasoningOn := reasoningActive(chatCfg["REASONING"])
	effortWanted, reasoningEffort, thinkOff, reasoningKwargs := turnReasoning(chatCfg)
	// When llama.cpp fails to parse a model-generated tool call (HTTP 500), we
	// retry the same turn once with tools removed so the model answers in plain
	// text from the tool results already gathered, instead of dying mid-chat.
	disableTools := false
	// Première relance après ce 500, sur le llama-server local : outils GARDÉS
	// à l'identique dans la requête, mais tool_choice « none » et la consigne en
	// fin de fil (withTrailingHint). Le prompt rendu ne change pas jusqu'au
	// dernier message, donc le cache sert presque tout ; retirer les outils
	// changeait le gabarit dès le système et recalculait toute la conversation.
	// Les appels éventuels sont ignorés (callsOn) ; le moindre écart — nouveau
	// refus, appel émis malgré tout, réponse vide — retombe UNE fois sur le
	// chemin historique (outils retirés, consigne via steerSystem).
	toolChoiceNone := false
	// Filet réactif (façon Hermes) : si llama-server refuse le prompt (souvent un
	// dépassement de la fenêtre de contexte après de gros résultats d'outils), on
	// compacte l'historique en vol et on rejoue le tour — une seule fois.
	compactedRetry := false
	// Réductions forcées (shrinkToFit) quand le compactage n'a pas suffi.
	shrinkRetries := 0
	// Rejeux à l'identique après un cache KV plein en plein calcul (SIDE_SLOT).
	poolRetries := 0
	// Repli d'intensité de raisonnement (llm_effort.go) : une seule tentative par
	// tour, comme les autres filets.
	effortRetried := false
	// Appels d'outil déjà exécutés (clé = nom + arguments bruts) : sert à ne pas
	// rejouer deux fois exactement la même écriture dans un même échange.
	doneCalls := map[string]string{}
	// Nombre de fois où le modèle a REDEMANDÉ un appel déjà exécuté. Sert à durcir
	// la réponse progressivement (voir repeatedCallResult) : rendre le contenu une
	// fois, puis refuser en le renvoyant vers ce qu'il a déjà.
	repeatCount := map[string]int{}
	// Garde-fou « pensé sans agir » : certains modèles à reasoning planifient un
	// appel d'outil dans leur <think> puis émettent le token de fin SANS l'émettre
	// (ni réponse, ni tool_call). On relance alors le tour avec un nudge explicite
	// au lieu d'afficher « pas de réponse ». Le premier suffit la plupart du temps ;
	// un second, plus direct, rattrape le cas où le modèle re-décrit le même plan
	// en boucle sans jamais l'exécuter — une simple reformulation de « agis » ne
	// suffit alors plus. Borné à maxNudges : un modèle vraiment bloqué doit finir
	// par rendre la main plutôt que faire attendre.
	const maxNudges = 2
	nudgeCount := 0
	// Génération la plus longue de ce tour (llama-server local), pour la garde
	// de marge de compactNeeded : le raisonnement n'étant plus compté dans le
	// contexte, il faut s'assurer AUTREMENT qu'une étape aussi longue tient
	// encore dans la fenêtre.
	peakGen := 0
	// Complétion coupée par la fenêtre pleine (finish_reason « length ») :
	// compactée puis rejouée une fois, au lieu du nudge « arrête de raisonner »
	// qui raccourcirait la réflexion du modèle.
	lengthReplays := 0
	// Contexte estimé pour la requête suivante (en-tour), comparé au compte
	// réel du moteur quand il arrive — journalisé s'il s'en écarte.
	lastEst := 0
	// Garde-fou « appel d'outil écrit en texte » (code_retry.go) : relances
	// consécutives bornées, compteur remis à zéro après chaque outil exécuté.
	patternRetries := 0
	// Reprises RÉSEAU consécutives (llm_retry_net.go). Remis à zéro dès qu'une
	// réponse arrive : une boucle d'outils longue retrouve son budget à chaque
	// itération réussie, un moteur durablement mort finit par rendre la main.
	netRetries := 0
	// Reprises après un flux coupé EN COURS de réponse (API externe seulement,
	// voir après la lecture du flux). Remis à zéro à chaque complétion lue en
	// entier.
	const maxStreamRetries = 5
	streamRetries := 0
	// REASONING_ECHO (llm_reasoning_echo.go). echoOffTurn : plus aucun
	// raisonnement ne part de ce tour (prompt trop long, ou gabarit qui l'a
	// refusé) — retour exact à la requête d'avant la clé. echoTplRetry : relance
	// en cours après une erreur de gabarit ; si elle passe, c'était bien le
	// raisonnement, et le refus est retenu pour ce modèle.
	echoOffTurn, echoTplRetry := false, false
	// Destination des complétions : llama-server local, ou une API OpenAI-compatible
	// externe si le preset actif en est un (backend_external.go). Résolu UNE FOIS
	// par tour — une bascule de preset en plein tour est rare, et se rejoue de
	// toute façon au message suivant.
	ep := resolveChatEndpoint()
	// Budget SOUPLE d'appels d'outils (llm_budget.go). Toujours pas de plafond
	// d'itérations : couper un tour cassait des recherches légitimes. Mais au-delà
	// d'un palier on RAPPELLE au modèle combien d'appels il a déjà faits et on lui
	// demande de conclure — de la pression, pas une barrière. Sans ça, un modèle
	// qui tourne en rond n'avait rien en face de lui sauf le bouton stop.
	toolRuns, budgetNudges, budget := 0, 0, agentBudget()
	// Mode Code : lire, éditer, compiler, relancer les tests — un vrai tour de
	// build dépasse vite 24 appels sans tourner en rond, et le troisième rappel
	// (« n'appelle plus d'outil ») coupait le travail au milieu. Palier triplé.
	if caps.Code {
		budget *= codeBudgetFactor
	}
	for iter := 0; ; iter++ {
		// Ajout en cours de réponse : entre deux étapes (après un appel d'outil ou
		// une relance), on injecte les messages mis en file par l'utilisateur. Pas
		// à iter==0 : le message initial du tour est déjà dans `messages`. Ni après
		// un stop : la boucle repasse ici une fois l'outil interrompu, et le
		// message en file serait journalisé alors que l'utilisateur a repris la
		// main (dropQueued l'abandonne à la fin du tour).
		if iter > 0 && ctx.Err() == nil {
			for _, inject := range injectQueued {
				if q := inject(); len(q) > 0 {
					messages = append(messages, q...)
					extra = append(extra, q...)
				}
			}
		}
		// Rappel injecté EN FIN d'historique : le préfixe déjà en cache côté
		// llama-server reste valide, seul le nouveau message est à traiter. Et
		// persisté tel quel (appendNudge) : le tour suivant le renvoie à
		// l'identique au lieu de diverger juste avant lui.
		if msg := budgetNudge(toolRuns, budget, budgetNudges); msg != "" {
			budgetNudges++
			logBudget(toolRuns, budget, budgetNudges)
			// NUDGE_IN_TOOL (opt-in, gabarits dont le rendu bouge quand un message
			// user s'ajoute) : au bout du dernier résultat d'outil. Sinon, ou si
			// la forme ne s'y prête pas, un message à part comme toujours.
			// La sonde ne connaît que la forme normale (outils annoncés, ni coupés
			// ni neutralisés) : une relance d'une autre forme se replie.
			placed := false
			if !disableTools && !toolChoiceNone && nudgeInToolOn(chatCfg, ep, tools, reasoningKwargs, reasoningEffort) {
				messages, extra, placed = nudgeIntoTool(messages, extra, msg)
			}
			if !placed {
				messages, extra = appendNudge(messages, extra, msg)
			}
		}
		// Appels d'outils exécutables pour cette complétion : outils annoncés,
		// et ni coupés ni neutralisés par tool_choice « none ».
		callsOn := len(tools) > 0 && !disableTools && !toolChoiceNone
		// Normalisé juste avant l'envoi : un seul système, en tête. Les gabarits
		// stricts (Qwen3.x) refusent un système ailleurs qu'en position 0.
		// Gardé à part : la télémétrie compare ces messages-là d'une requête à
		// l'autre (perfPrefix).
		// Raisonnement renvoyé : décidé au point de sortie unique, pour chaque
		// requête. Clé absente (ou preset externe, ou repli en cours) :
		// echoMessages rend la tranche telle quelle, requête inchangée.
		// Assemblage partagé avec le préchauffage (chat_prewarm.go) : les deux ne
		// peuvent pas diverger.
		pol := turnEchoPolicy(ep.External || echoOffTurn)
		sent, echoSent := wireMessages(messages, toolChoiceNone, pol)
		payload := buildChatPayload(ep, sent, temperature, chatPayloadOpts{
			tools: tools, disableTools: disableTools, toolChoiceNone: toolChoiceNone,
			effort: reasoningEffort, kwargs: reasoningKwargs,
		})
		// SIDE_SLOT (llm_sideslot.go) : le fil de la discussion sur le slot 0,
		// le reste sur le 1. Sans second slot en service, aucun id_slot.
		setEngineSlot(payload, engineSlotFor(ep, slotIsMain(ptag.kind)))
		body, _ := json.Marshal(payload)
		req, err := http.NewRequestWithContext(ctx, "POST", ep.URL, bytes.NewReader(body))
		if err != nil {
			return extra, err
		}
		kt.sending(len(messages)) // KEEP_TURN_IMAGES : rien sans la clé
		req.Header.Set("Content-Type", "application/json")
		// ⚠️ Surtout pas authHeader : celui-ci pose la clé du serveur LOCAL, et
		// l'envoyer à api.openai.com serait fuiter un secret chez un tiers en même
		// temps qu'un 401 garanti. Chaque endpoint porte la sienne.
		ep.auth(req.Header.Set)
		tReq := time.Now() // secours des stats quand le serveur n'envoie pas de timings
		// Requête en vol vers le moteur local, jusqu'à la lecture complète du corps :
		// l'isolation des travaux annexes n'efface jamais le slot pendant ce temps
		// (llm_slots.go). Rien à compter pour une API externe.
		// Un préchauffage en vol (PREWARM, chat_prewarm.go) est annulé ici, sauf
		// s'il prépare exactement le début de CETTE requête d'un tour de chat.
		endReq := func() {}
		var reqSeq uint64
		// SLOT_PERSIST (llm_slotpersist.go) : l'état gardé à la dernière bascule
		// de preset est rechargé avant la première requête de la discussion,
		// si tout concorde. Sans la clé, rien.
		if !ep.External && ptag.kind == perfMain {
			slotPersistRestore(ep, ptag.conv)
		}
		if !ep.External {
			endReq, reqSeq = engineRequestBeginSide(prewarmKeeper(ptag.kind, sent, payload), payloadOnSideSlot(payload))
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			endReq()
			// Rien n'est encore parti à l'écran : le tour est rejouable tel quel.
			// C'est le cas du moteur qui redémarre (bascule de preset, rechargement
			// de modèle) — quelques secondes de connexion refusée qui faisaient
			// échouer pour de bon une tâche planifiée tombée pile là.
			if netRetries < llmNetRetries && llmRetryableErr(ctx, err) {
				logLLMRetry(netRetries+1, friendlyLLMError(err).Error())
				if werr := llmNetBackoff(ctx, netRetries); werr == nil {
					netRetries++
					continue
				}
			}
			err = friendlyLLMError(err)
			cb(StreamEvent{Err: err})
			return extra, err
		}
		// A non-200 here (e.g. context window exceeded after several large tool
		// outputs) is NOT valid SSE: without this check we'd scan an empty/HTML
		// body, find no data lines, and return silently — the chat just stops
		// with no answer. Surface the body so the cause is visible instead.
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
			resp.Body.Close()
			endReq()
			msg := strings.TrimSpace(string(b))
			if msg == "" {
				msg = resp.Status
			}
			// La relance sans raisonnement est refusée à son tour : il n'y était
			// pour rien, aucun refus à retenir pour ce modèle.
			echoTplRetry = false
			// Refus du niveau de raisonnement par le gabarit du modèle (Qwen3.8 ne
			// connaît pas « high », gpt-oss ne connaît pas « xhigh »…). C'est un
			// refus DÉFINITIF, pas une question de taille de prompt : à traiter
			// avant la compaction, qui sinon taillait l'historique pour rien puis
			// échouait quand même. On rejoue le tour avec le niveau accepté (ou
			// sans le champ), l'historique intact.
			if !effortRetried && effortWanted != "" {
				if fixed, ok := effortFromRejection(msg, effortWanted); ok {
					effortRetried = true
					effortRemember(effortWanted, fixed)
					logEffortFallback(effortWanted, fixed)
					reasoningEffort = fixed
					reasoningKwargs = reasoningTemplateKwargs(thinkOff, fixed)
					continue
				}
			}
			// SIDE_SLOT : un cache KV à court de place en plein calcul n'est pas un
			// prompt trop long (llm_sideslot.go). Rejouée telle quelle, sans
			// compaction ni réduction — deux fois au plus, le temps que l'autre
			// slot finisse, puis l'erreur telle quelle : la conversation tenait
			// dans sa fenêtre, rien ne justifie d'en perdre un morceau. Sans
			// second slot en service, ce filet ne joue pas.
			if !ep.External && sideSlotPoolError(msg) && sideSlotLive(ep) {
				if poolRetries < 2 {
					poolRetries++
					logCtx("cache KV plein pendant le calcul (deux slots) : requête rejouée telle quelle, sans compaction")
					if werr := llmNetBackoff(ctx, poolRetries-1); werr == nil {
						continue
					}
				}
				err := fmt.Errorf("llama-server a renvoyé %d : %s", resp.StatusCode, msg)
				cb(StreamEvent{Err: err})
				return extra, err
			}
			// Gabarit qui refuse le raisonnement renvoyé (gpt-oss : « Cannot pass
			// both content and thinking », message assistant jugé invalide…) :
			// rejoué UNE fois sans lui, AVANT la coupure des outils et la consigne
			// système, qui casseraient le tour et tout le cache de prompt.
			if echoSent > 0 && echoTemplateError(resp.StatusCode, msg) {
				echoOffTurn, echoTplRetry = true, true
				logCtx("erreur de gabarit avec le raisonnement renvoyé (%d) : relance sans lui", resp.StatusCode)
				continue
			}
			// Prompt trop long alors que du raisonnement est parti : on retente
			// d'abord sans lui — exactement la requête d'avant REASONING_ECHO —,
			// avant toute compaction ou réduction, qui perdent de l'information.
			if echoSent > 0 && contextOverflow(msg, messages) {
				echoOffTurn = true
				logCtx("prompt trop long avec le raisonnement renvoyé : relance sans lui avant toute compaction")
				continue
			}
			// Le prompt a peut-être dépassé la fenêtre de contexte : on tente une
			// compaction en vol et on rejoue le tour (une seule fois) avant tout le
			// reste. C'est le filet de secours à la Hermes.
			// ⚠️ Seulement si l'erreur est VRAIMENT un débordement de contexte : avant,
			// n'importe quel refus (appel d'outil mal formé, modèle en chargement,
			// erreur de template…) résumait ~75 % de la conversation, même courte.
			// Raisonnement déjà retiré de ce tour (repli ci-dessus) : la réduction
			// travaille sur ce qui part vraiment. Sinon elle compterait — puis
			// « retirerait » — du raisonnement qui ne partait plus, et gâcherait
			// une relance sur une requête identique.
			if echoOffTurn {
				messages = stripReasoning(messages)
			}
			// KEEP_TURN_IMAGES : les images gardées partent d'abord, et la requête
			// est rejouée sans elles ; le résumé (avec perte) ne vient qu'ensuite,
			// s'il le faut encore. Sans la clé, aucune image gardée.
			if contextOverflow(msg, messages) && hasKeptImages(messages) {
				if s, freed := dropKeptImages(messages); freed > 0 {
					logKeptImagesDropped("réactif", freed)
					messages = s
					extra = nil
					publishHistory()
					continue
				}
			}
			if compactEnabled() && !compactedRetry && contextOverflow(msg, messages) {
				if c, changed := compactMessages(ctx, messages, caps); changed {
					compactedRetry = true
					// ⚠️ Journaliser AVANT d'installer le résultat : l'ancien ordre
					// passait `messages` déjà remplacé comme état « avant », donc la
					// ligne comparait le résultat à lui-même et n'apprenait rien.
					logCompact("réactif", 0, messages, c, changed)
					messages = c
					// Même publication qu'en cours de tour : sans elle, la compaction de
					// secours ne survit pas à la fin du tour et le prompt re-déborde au
					// message suivant.
					extra = nil
					newHistory()
					continue
				}
			}
			// Compactage impuissant (ou déjà tenté et encore trop long) : réduction
			// forcée plutôt que de laisser remonter un 400 qui bloque la
			// conversation (AJEAN 0.17.5).
			if compactEnabled() && shrinkRetries < 2 && contextOverflow(msg, messages) {
				if c, changed := shrinkToFit(messages, overflowTokens(msg)); changed {
					shrinkRetries++
					logCompact("réduction", overflowTokens(msg), messages, c, changed)
					messages = c
					extra = nil
					newHistory()
					continue
				}
			}
			// Most common 500 here: llama.cpp couldn't parse a malformed tool call
			// the model emitted. Retry the turn once without tools so it answers
			// in plain text rather than leaving the chat dead. Seulement sur 500 :
			// un 401, 429 ou 502 (API externe, passerelle) n'a rien à voir avec un
			// appel mal formé, et couper les outils pour ça cassait le tour d'agent.
			//
			// Sur le llama-server local, la première relance garde le prompt
			// intact (voir toolChoiceNone). Une API externe garde le chemin
			// historique : certaines passerelles OpenAI-compatibles traitent
			// tool_choice à leur façon. Si la relance « none » est refusée à
			// son tour (500, ou un 4xx d'un moteur qui ne connaîtrait pas le
			// champ), on retombe sur le chemin historique au lieu de finir le
			// tour.
			if !disableTools && len(tools) > 0 && (resp.StatusCode == http.StatusInternalServerError ||
				(toolChoiceNone && resp.StatusCode >= 400 && resp.StatusCode < 500)) {
				if !ep.External && !toolChoiceNone {
					toolChoiceNone = true
					logCtx("relance sans outil après un 500 : tool_choice=none, prompt conservé")
					continue
				}
				toolChoiceNone = false
				disableTools = true
				// Nudge the model to answer in plain text from what it already
				// gathered, so it doesn't immediately re-emit a tool call that
				// llama.cpp would again fail to parse.
				messages = steerSystem(messages, toolsOffHint)
				continue
			}
			// Dernier recours, APRÈS les filets sémantiques ci-dessus : un statut
			// qui dit « pas maintenant » (passerelle, service indisponible, trop de
			// requêtes) et non « ta requête est fautive ». Le corps n'a pas été
			// diffusé, le tour est rejouable.
			if netRetries < llmNetRetries && llmRetryableStatus(resp.StatusCode) && ctx.Err() == nil {
				logLLMRetry(netRetries+1, fmt.Sprintf("le moteur a renvoyé %d", resp.StatusCode))
				if werr := llmNetBackoff(ctx, netRetries); werr == nil {
					netRetries++
					continue
				}
			}
			// Nommer la BONNE machine : « llama-server a renvoyé 401 » sur un preset
			// externe envoie chercher la panne du mauvais côté.
			who := "llama-server"
			if ep.External {
				who = "l'API externe"
			}
			err := fmt.Errorf("%s a renvoyé %d : %s", who, resp.StatusCode, msg)
			cb(StreamEvent{Err: err})
			return extra, err
		}
		// Une réponse est arrivée : le budget de reprise réseau repart à neuf pour
		// la suite de la boucle d'outils.
		netRetries = 0
		// La relance sans raisonnement passe : c'était bien lui que le gabarit
		// refusait. Retenu pour ce modèle, plus de 500 à repayer à chaque tour.
		if echoTplRetry {
			echoTplRetry = false
			echoRefuse(reasoningEchoModel(ReadConfig()))
		}
		if !ep.External {
			engineServed() // le slot porte désormais cette requête (llm_slots.go)
			// Étape d'un tour de discussion : une compaction en continuation
			// pourra la prolonger (chat_compact_cont.go). Sans la clé, rien.
			// SLOT_PERSIST s'en sert aussi : seul un slot qui porte un tour de
			// discussion est gardé à la bascule de preset.
			if ptag.kind == perfMain && (compactContinuationOn(chatCfg) || slotPersistOn(chatCfg)) {
				engineMarkMain(reqSeq, ptag.conv)
			}
		}
		toolCalls := map[int]*ToolCall{}
		// argBufs : arguments de chaque appel, accumulés sans recopie. `+=` sur la
		// chaîne recopiait tout le JSON à chaque morceau — quadratique sur l'écriture
		// d'un gros fichier. cur.Function.Arguments reste la valeur de référence :
		// on la recale sur le tampon à chaque morceau (String() ne copie rien).
		argBufs := map[int]*strings.Builder{}
		assistantContent := strings.Builder{}
		// retryScanned : longueur de assistantContent déjà balayée sans appel
		// textuel (voir textualToolCallFrom).
		retryScanned := 0
		finishReason := ""
		// Accumulateur de stats : timings (prefill/decode) puis usage (total prompt)
		// arrivent sur des chunks séparés ; on émet une copie complète à chaque MAJ
		// pour que les consommateurs (terminal, web) aient toujours tout.
		var stats StatsEvent
		// Serveur sans `timings` (API tierces) : on mesure nous-mêmes le décodage
		// entre le premier et le dernier token (voir après la boucle).
		sawTimings := false
		var tFirst, tLast time.Time
		usageGen, genChunks := 0, 0
		lastPreview := ""   // last command preview emitted (to stream the typing)
		lastBodyLines := -1 // lignes déjà diffusées du corps en cours d'écriture
		// Lecture au fil de l'eau de l'argument affiché (commande, chemin…) et du
		// corps d'une écriture : seuls les octets neufs sont décodés.
		var labelDec, bodyDec *argPreview
		// sentAnswer : du texte de réponse ou un outil est déjà parti vers l'UI pour
		// cette complétion (une reprise le doublerait). sentReasoning : seul du
		// raisonnement est parti, qu'on sait retirer (DropReasoning). shown : texte
		// de réponse réellement affiché, rendu au modèle pour qu'il reprenne après
		// une coupure. typingTool : appel d'outil en cours d'écriture à l'écran.
		sentAnswer, sentReasoning := false, false
		var shown strings.Builder
		var typingTool *ToolUsedEvent
		scb := func(ev StreamEvent) bool {
			if ev.Content != "" {
				sentAnswer = true
				shown.WriteString(ev.Content)
			}
			if ev.ToolUsed != nil {
				sentAnswer = true
				t := *ev.ToolUsed
				typingTool = &t
			}
			if ev.Reasoning != "" {
				sentReasoning = true
			}
			return cb(ev)
		}
		// Per-completion reasoning-split state (see reasoningOn comment above).
		sawReasoningField := false
		// Raisonnement séparé PAR LE SERVEUR, gardé pour être renvoyé
		// (REASONING_ECHO). Jamais celui découpé chez nous sur « </think> » : il
		// reste dans le contenu, où le gabarit le redécoupe lui-même.
		var echoBuf strings.Builder
		thinkOpen := reasoningOn
		var thinkTail strings.Builder
		// Scanner à gros tampon : un chunk peut porter un gros JSON d'arguments
		// (écriture de fichier). 8 Mio et non 1 : au-delà du tampon, le scanner
		// s'arrête sur « token too long » AU MILIEU du flux, et jusqu'à la 0.8.4
		// personne ne le voyait (voir sc.Err() plus bas).
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
		aborted := false
		// patternHit : flux coupé sur un appel d'outil écrit en texte (voir plus bas).
		patternHit := false
		// noneLeak : sous tool_choice « none », le modèle a quand même tenté un
		// appel (protocole ou texte) — voir toolChoiceNone.
		noneLeak := false
		// preflight : write/edit refusé dès son chemin (voir plus bas) ; non-nil
		// mais vide = chemin déjà vérifié, rien à signaler.
		var preflight *preflightRefusal
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(line[5:])
			if data == "" || data == "[DONE]" {
				continue
			}
			var chunk streamChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}
			// Timings ET usage (include_usage) arrivent sur le CHUNK FINAL qui, sur ce
			// build llama.cpp (MTP/spéculatif), a `choices:[]` — on les traite AVANT le
			// garde de choices, sinon `gen_tokens`/`gen_per_second` (decode) sont jetés
			// et l'UI retombe à « 0 tok/s » à la fin de la génération.
			// Deuxième lecture, tolérante, pour la télémétrie : seulement sur les
			// chunks qui portent timings ou usage, pas sur chaque jeton.
			var pw perfWire
			if chunk.Timings != nil || chunk.Usage != nil {
				pw = decodePerfWire([]byte(data))
				stats.applyPerf(pw)
			}
			if chunk.Timings != nil {
				sawTimings = true
				stats.PromptTokens = chunk.Timings.PromptN
				stats.PromptPerSecond = chunk.Timings.PromptPerSecond
				stats.PromptMs = chunk.Timings.PromptMs
				stats.GenTokens = chunk.Timings.PredictedN
				stats.GenPerSecond = chunk.Timings.PredictedPerSec
				stats.GenMs = chunk.Timings.PredictedMs
				s := stats
				scb(StreamEvent{Stats: &s})
			}
			if chunk.Usage != nil && chunk.Usage.CompletionTokens > 0 {
				usageGen = chunk.Usage.CompletionTokens
			}
			if chunk.Usage != nil && chunk.Usage.PromptTokens > 0 {
				stats.PromptTokensTotal = chunk.Usage.PromptTokens
				s := stats
				if !ep.External {
					cached := pw.cachedTokens()
					s.CacheHint = noteEnginePrompt(chunk.Usage.PromptTokens, cached.int(), cached.ok)
				}
				scb(StreamEvent{Stats: &s})
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			ch := chunk.Choices[0]
			if ch.Delta.Content != "" || ch.Delta.ReasoningContent != "" || len(ch.Delta.ToolCalls) > 0 {
				now := time.Now()
				if tFirst.IsZero() {
					tFirst = now
				}
				tLast = now
				genChunks++
			}
			if ch.FinishReason != "" {
				finishReason = ch.FinishReason
			}
			// Un tool_call n'est retenu que si des outils ont VRAIMENT été annoncés ce
			// tour-ci (même condition que le payload). Sinon — mode agent coupé, ou
			// relance sans outils après un 500 — le moteur peut quand même parser un
			// appel que le modèle a émis de lui-même ; l'exécuter donnait « outil
			// inconnu », réinjecté puis mis en boucle. On l'ignore (AJEAN 0.13.12).
			// Relance tool_choice « none » : un appel émis malgré tout n'est pas
			// exécuté, on coupe et on retombe sur la relance sans outils.
			if toolChoiceNone && len(ch.Delta.ToolCalls) > 0 {
				noneLeak = true
				break
			}
			if callsOn && len(ch.Delta.ToolCalls) > 0 {
				// Un appel d'outil clôt le texte : on vide MAINTENANT le reliquat
				// retenu par la garde « </think> » (voir plus bas). Sinon il n'était
				// émis qu'en fin de flux, donc APRÈS l'événement d'outil, et l'UI
				// (qui coupe la bulle en cours à chaque tool_used) affichait la fin
				// de la phrase — souvent coupée en plein mot — dans une bulle
				// séparée sous l'outil.
				if thinkOpen && thinkTail.Len() > 0 {
					tail := thinkTail.String()
					thinkTail.Reset()
					if !scb(StreamEvent{Content: tail}) {
						aborted = true
						break
					}
				}
				for i, tc := range ch.Delta.ToolCalls {
					// Le champ index dit à quel appel appartient ce morceau. Un serveur
					// qui envoie un morceau par chunk met l'appel n°2 en position 0 du
					// chunk : se fier à i fusionnait les appels parallèles (noms
					// écrasés, JSON collés « {…}{…} »). Sans index (certains builds
					// llama.cpp), on retombe sur la position dans le chunk.
					idx := i
					if tc.Index != nil {
						idx = *tc.Index
					}
					cur, ok := toolCalls[idx]
					if !ok {
						cur = &ToolCall{Type: "function"}
						toolCalls[idx] = cur
					}
					if tc.ID != "" {
						cur.ID = tc.ID
					}
					if tc.Function.Name != "" {
						cur.Function.Name = tc.Function.Name
					}
					ab := argBufs[idx]
					if ab == nil {
						ab = &strings.Builder{}
						ab.WriteString(cur.Function.Arguments)
						argBufs[idx] = ab
					}
					ab.WriteString(tc.Function.Arguments)
					cur.Function.Arguments = ab.String()
				}
				// Stream the command being typed: extract the partial value and
				// emit it whenever it grows, so the UI shows it appear live.
				if cur := toolCalls[0]; cur != nil {
					key := "command"
					switch cur.Function.Name {
					case "mem_search", "web_search", "web_images", "recall_search":
						key = "query"
					case "mem_read", "mem_add", "mem_edit", "edit", "write", "read", "git_diff":
						key = "file"
					case "recall":
						key = "id"
					case "web_open", "web_read", "web_grep":
						key = "url"
					case "grep", "glob":
						key = "pattern"
					case "ask":
						key = "question"
					case "criteria":
						key = "action"
					case "bash_tail":
						key = "id"
					case "git_clone":
						key = "url"
					}
					if labelDec == nil || labelDec.key != key {
						labelDec = newArgPreview(key)
					}
					labelDec.update(cur.Function.Arguments)
					p := labelDec.value()
					// Corps en cours de frappe pour les outils d'écriture : on le diffuse
					// à la LIGNE, pas au token. Un événement par token republierait tout le
					// contenu à chaque fois (coût quadratique, et c'est ce flot qui saturait
					// le rendu mobile) ; à la ligne, le nombre d'événements est celui du
					// fichier et l'animation reste fluide. Et chaque événement ne porte
					// que la FIN du corps (bodyTail) : republier tout le fichier à chaque
					// ligne restait quadratique — 250 Mo de flux pour 120 Ko écrits.
					full := ""
					if bk := writeBodyKey(cur.Function.Name); bk != "" {
						if bodyDec == nil || bodyDec.key != bk {
							bodyDec = newArgPreview(bk)
						}
						bodyDec.update(cur.Function.Arguments)
						full = bodyDec.value()
					}
					grew := full != "" && bodyDec.lines > lastBodyLines
					if (p != "" && p != lastPreview) || grew {
						if p != "" {
							lastPreview = p
						}
						if grew {
							lastBodyLines = bodyDec.lines
						}
						tu := &ToolUsedEvent{Name: cur.Function.Name, Label: lastPreview, Typing: true}
						if full != "" {
							tu.Body, tu.BodyTail = bodyTail(full)
							tu.BodyLines = bodyDec.lineCount()
						}
						if !scb(StreamEvent{ToolUsed: tu}) {
							aborted = true
							break
						}
					}
					// Pré-vol (OpenFox 2.0.154) : dès que le chemin d'un write/edit est
					// complet, on vérifie les gardes du mode Code (fichier lu avant
					// d'être modifié, chemin permis). Refusé : on coupe la génération
					// MAINTENANT au lieu de laisser le modèle écrire tout un fichier
					// qui serait de toute façon rejeté.
					if caps.Code && preflight == nil && (cur.Function.Name == "write" || cur.Function.Name == "edit") {
						if file, done := previewArgDone(cur.Function.Arguments, "file"); done && strings.TrimSpace(file) != "" {
							if msg := codeWriteGuard(caps, file, cur.Function.Name == "write"); msg != "" {
								preflight = &preflightRefusal{name: cur.Function.Name, id: cur.ID, file: file, msg: msg}
								break
							}
							preflight = &preflightRefusal{} // vérifié : plus besoin de regarder
						}
					}
				}
				continue
			}
			if ch.Delta.ReasoningContent != "" {
				// Backend already separates reasoning — trust it, disable our split.
				sawReasoningField = true
				thinkOpen = false
				// Compté ICI seulement : le raisonnement découpé chez nous (<think>
				// en ligne, plus bas) reste dans assistantContent et repart dans
				// l'étape suivante — le retirer sous-estimerait le contexte. Pas
				// pour une API externe : son découpage en morceaux ne dit rien du
				// nombre de jetons, et son preset doit rester tel quel. stats est
				// propre à cette tentative : une relance repart de zéro.
				if !ep.External {
					stats.ReasoningTokens++
				}
				if pol.on {
					echoBuf.WriteString(ch.Delta.ReasoningContent)
				}
				if !scb(StreamEvent{Reasoning: ch.Delta.ReasoningContent}) {
					aborted = true
					break
				}
			}
			if ch.Delta.Content != "" {
				assistantContent.WriteString(ch.Delta.Content)
				if !thinkOpen || sawReasoningField {
					if !scb(StreamEvent{Content: ch.Delta.Content}) {
						aborted = true
						break
					}
					// Appel d'outil écrit en texte, repéré PENDANT le flux (OpenFox) :
					// on coupe tout de suite au lieu de laisser le modèle dérouler un
					// faux appel — parfois un fichier entier — qui ne sera jamais
					// exécuté. La relance corrective part juste après (voir fin de
					// boucle). Testé seulement quand le morceau peut ouvrir un motif.
					// Balayage limité à ce qui suit le précédent (retryScanned, avancé
					// seulement quand un balayage a VRAIMENT eu lieu : un morceau sans
					// caractère déclencheur n'est pas relu, l'écart doit rester couvert).
					// La passe complète de fin de tour, elle, reste entière.
					hit := false
					if (callsOn && patternRetries < maxPatternRetries || toolChoiceNone) &&
						strings.ContainsAny(ch.Delta.Content, "<`{[_.") {
						s := assistantContent.String()
						hit = textualToolCallFrom(s, retryScanned)
						retryScanned = len(s)
					}
					if hit {
						// Sous tool_choice « none », le moteur ne parse plus les
						// appels : le balisage arrive en texte. Ce n'est pas une
						// réponse, on retombe sur la relance sans outils.
						if toolChoiceNone {
							noneLeak = true
						} else {
							patternHit = true
						}
						break
					}
				} else {
					// The prompt opened a <think> block. Stream `content` LIVE as
					// the answer, holding back only a short tail that could be the
					// start of a literal "</think>". A reasoning-aware backend
					// (llama.cpp with --reasoning-format, Nathan's fork) strips the
					// think tags server-side, so </think> never appears in content
					// and the whole answer streams straight through — including
					// when the model answers WITHOUT thinking (no reasoning_content,
					// no </think>), which is exactly what used to get dumped into
					// the reasoning bubble. A vanilla build that leaves the thinking
					// inline still gets carved at the </think> below.
					thinkTail.WriteString(ch.Delta.Content)
					s := thinkTail.String()
					if i := strings.Index(s, thinkClose); i >= 0 {
						// Vanilla inline think: reasoning before </think>, answer
						// after. Route the reasoning to its bubble, drop the tag.
						reason := s[:i]
						after := strings.TrimLeft(s[i+len(thinkClose):], "\r\n")
						thinkOpen = false
						thinkTail.Reset()
						if reason != "" && !scb(StreamEvent{Reasoning: reason}) {
							aborted = true
							break
						}
						if after != "" && !scb(StreamEvent{Content: after}) {
							aborted = true
							break
						}
					} else {
						// No </think> yet: stream as content, holding back a tail
						// that could be a partial "</think>". Back the cut up to a
						// UTF-8 rune boundary so a multi-byte char (é, …) is never
						// split — otherwise the two halves decode as � (mojibake).
						cut := len(s) - (len(thinkClose) - 1)
						for cut > 0 && !utf8.RuneStart(s[cut]) {
							cut--
						}
						if cut > 0 {
							emit := s[:cut]
							thinkTail.Reset()
							thinkTail.WriteString(s[cut:])
							if !scb(StreamEvent{Content: emit}) {
								aborted = true
								break
							}
						}
					}
				}
			}
		}
		// Flush the held-back tail (never part of a </think>): it's answer text.
		if !aborted && thinkOpen && thinkTail.Len() > 0 {
			scb(StreamEvent{Content: strings.TrimLeft(thinkTail.String(), "\r\n")})
		}
		// ⚠️ Le flux a-t-il fini, ou CASSÉ ? sc.Scan() renvoie false dans les deux
		// cas, et l'erreur n'était jamais consultée : une lecture coupée en plein
		// milieu (connexion réinitialisée, ligne plus longue que le tampon) était
		// donc indiscernable d'une fin normale. Vécu par l'utilisateur : l'agent
		// enchaîne quelques commandes puis « s'arrête et rend la main sans avoir
		// terminé, ni même commenté », pendant que le journal de llama-server
		// affiche un « stop processing » parfaitement normal (issue #19). Pire,
		// des appels d'outils accumulés à moitié auraient été EXÉCUTÉS avec des
		// arguments tronqués. On refuse donc le tour, en le disant.
		scanErr := sc.Err()
		resp.Body.Close()
		endReq()
		// Raisonnement de cette complétion à renvoyer, s'il y a lieu. Il repart
		// dans la requête suivante quand elle continue ce tour (appel d'outil),
		// ou au tour suivant si le gabarit garde le passé : ctxAfter le compte —
		// posé avant toute émission de stats de cette complétion.
		echoText := ""
		if pol.on && sawReasoningField {
			echoText = echoBuf.String()
		}
		if echoText != "" && (len(toolCalls) > 0 || pol.keepsPast) {
			stats.echoed = true
		}
		// Premier jeton, de quelque nature qu'il soit (raisonnement, texte ou
		// appel d'outil). tReq est repris à chaque tentative.
		if !tFirst.IsZero() {
			ms := tFirst.Sub(tReq).Milliseconds()
			stats.TTFTms = &ms
		}
		if !sawTimings && !tFirst.IsZero() {
			// Décodage mesuré ici ; le 1er token tombe dans la lecture du prompt.
			// Pas de débit de lecture : le serveur a pu réutiliser une partie du
			// prompt en cache, et total/délai donnerait des chiffres fantaisistes.
			gen := usageGen
			if gen == 0 {
				gen = genChunks // sans usage : un chunk ≈ un token
			}
			stats.PromptMs = float64(tFirst.Sub(tReq).Milliseconds())
			stats.GenTokens = gen
			stats.GenMs = float64(tLast.Sub(tFirst).Milliseconds())
			if sec := tLast.Sub(tFirst).Seconds(); sec > 0 && gen > 1 {
				stats.GenPerSecond = float64(gen-1) / sec
			}
			s := stats
			cb(StreamEvent{Stats: &s})
		}
		// Télémétrie (perf_log.go). Une complétion coupée avant son chunk final
		// (arrêt, appel écrit en texte, refus anticipé, flux cassé) n'a ni cache
		// ni total fiables : rangée comme incomplète, sans calcul de perte.
		complete := !aborted && (scanErr == nil || finishReason != "") && (sawTimings || stats.PromptTokensTotal > 0)
		var prefix []uint64
		if complete {
			prefix = perfPrefix(sent)
		}
		rec := perfRecord(perfRecFromStats(ptag, perfIter, complete, stats), prefix)
		perfIter++
		if complete {
			// Dernier événement de la complétion, copie COMPLÈTE des stats : rejoué
			// depuis le journal, il ne fait que repeindre la même ligne.
			s := stats
			s.Kind, s.Lost, s.LostAfter = rec.Kind, rec.Lost, rec.LostAfter
			// Pas d'alerte pour une API externe : son cache (s'il existe) suit ses
			// propres règles, sans slot ni points de reprise à surveiller ici. Le
			// relevé reste dans l'anneau, l'affichage ne change pas.
			if !ep.External && ((rec.Lost != nil && *rec.Lost > 0) || (rec.Cached != nil && *rec.Cached == 0)) {
				s.LostAlert = perfAlert(rec, perfLostAlertAt(chatCfg))
			}
			cb(StreamEvent{Stats: &s})
			// Sonde de gabarit (chat_tplprobe.go), en tâche de fond et après coup :
			// la complétion est déjà lue, la sonde ne retarde aucun jeton. Fil
			// principal seulement, sur une requête de forme normale (pas une
			// relance outils coupés ou neutralisés), pour reproduire exactement
			// les outils et chat_template_kwargs d'un vrai tour. Rien n'en revient
			// dans la requête suivante.
			if !ep.External && ptag.kind == perfMain && !disableTools && !toolChoiceNone {
				tplProbeKick(tools, reasoningKwargs, reasoningEffort)
			}
			if !ep.External {
				peakGen = max(peakGen, stats.GenTokens)
				if lastEst > 0 && stats.PromptTokensTotal > 0 {
					logCtxEstimate("en-tour", lastEst, stats.PromptTokensTotal)
				}
			}
			lastEst = 0
		}
		// KEEP_TURN_IMAGES : coût des images de l'étape précédente, mesuré sur
		// cette complétion ; gardées ou rendues éphémères. Rien sans la clé.
		extra = kt.observe(complete, stats, messages, extra)
		if finishReason == "length" {
			logCtx("finish=length prompt=%d généré=%d raisonnement=%d fenêtre=%d", stats.PromptTokensTotal, stats.GenTokens, stats.ReasoningTokens, ctxWindow())
		}
		if aborted {
			return extra, nil
		}
		// Coupure APRÈS le dernier chunk (finish_reason reçu) : la réponse est
		// complète, seule la fermeture a raté. On la garde telle quelle. Flux
		// coupé par NOUS sur un appel écrit en texte : pas une panne non plus.
		if scanErr != nil && (finishReason != "" || patternHit || noneLeak || (preflight != nil && preflight.msg != "")) {
			scanErr = nil
		}
		// Flux coupé vers une API DISTANTE (Wi-Fi, VPN, proxy qui décroche) : on
		// relance au lieu d'abandonner le tour (AJEAN 0.17.4). Jamais pour le
		// llama-server local : coupé, il a planté, et rejouer ne ferait que
		// retarder le message d'erreur.
		//   - Rien de la réponse n'est encore affiché : on rejoue la même requête
		//     (le raisonnement déjà montré est retiré, il va être régénéré).
		//   - Du texte est déjà affiché : on le rend au modèle comme début de sa
		//     réponse, suivi d'un « connexion coupée, continue », et il reprend où
		//     il s'était arrêté. Ces deux messages ne vont que dans la vue modèle
		//     de ce tour : la réponse persistée est le texte complet, reconstitué
		//     par l'appelant à partir du flux. Un appel d'outil à moitié écrit est
		//     jeté : le modèle le réémettra.
		if scanErr != nil && ctx.Err() == nil && !errors.Is(scanErr, bufio.ErrTooLong) &&
			ep.External && streamRetries < maxStreamRetries {
			streamRetries++
			if sentReasoning && !sentAnswer {
				cb(StreamEvent{DropReasoning: true})
			}
			// Clôt la bulle de l'outil à moitié écrit : il va être réémis, sinon
			// elle resterait « en cours d'écriture » pour toujours.
			if typingTool != nil {
				cb(StreamEvent{ToolUsed: &ToolUsedEvent{Name: typingTool.Name, Label: typingTool.Label, Done: true, Result: "(appel interrompu par une coupure réseau, relancé)"}})
			}
			logStreamRetry(streamRetries, maxStreamRetries, scanErr)
			if llmNetBackoff(ctx, streamRetries) == nil {
				if partial := shown.String(); strings.TrimSpace(partial) != "" {
					messages = append(messages,
						Message{Role: "assistant", Content: partial},
						Message{Role: "user", Content: "The connection was cut during your answer. Continue exactly where you stopped, without repeating what you already wrote."})
				}
				continue
			}
		}
		if scanErr != nil && ctx.Err() == nil {
			err := streamCutError(scanErr)
			cb(StreamEvent{Err: err})
			return extra, err
		}
		// Complétion lue en entier : le budget de reprises vaut par coupure
		// rapprochée, pas pour tout un long tour d'agent.
		streamRetries = 0

		// Relance tool_choice « none » sans réponse exploitable (appel tenté
		// malgré tout, balisage d'appel en texte, ou rien du tout) : repli UNE
		// fois sur le chemin historique, outils retirés du gabarit. Le prompt est
		// alors recalculé, mais le tour aboutit comme avant. Pas après un stop :
		// la relance partirait sur un contexte annulé et afficherait une erreur.
		if toolChoiceNone && ctx.Err() == nil && (noneLeak || textualToolCallSnippet(assistantContent.String()) != "" ||
			strings.TrimSpace(assistantContent.String()) == "") {
			logCtx("relance tool_choice=none sans réponse exploitable : repli sans outils")
			toolChoiceNone = false
			disableTools = true
			if sentReasoning && !sentAnswer {
				cb(StreamEvent{DropReasoning: true})
			}
			messages = steerSystem(messages, toolsOffHint)
			continue
		}

		// Écriture refusée en plein flux (pré-vol) : l'appel, réduit à son chemin,
		// entre dans l'historique avec le refus pour résultat — le modèle voit
		// pourquoi, et la boucle repart pour qu'il lise le fichier d'abord.
		if preflight != nil && preflight.msg != "" {
			id := preflight.id
			if id == "" {
				id = fmt.Sprintf("call_%d_0", iter)
			}
			fileArg, _ := json.Marshal(map[string]string{"file": preflight.file})
			tc := ToolCall{ID: id, Type: "function", Function: ToolCallFunc{Name: preflight.name, Arguments: string(fileArg)}}
			assistant := Message{Role: "assistant", ToolCalls: []ToolCall{tc}}
			if s := assistantContent.String(); s != "" {
				assistant.Content = s
			}
			if echoText != "" {
				assistant.ReasoningContent, assistant.ReasoningModel = echoText, pol.model
			}
			result := preflight.msg + " (écriture interrompue avant la fin : rien n'a été modifié)"
			cb(StreamEvent{ToolUsed: &ToolUsedEvent{Name: preflight.name, Label: preflight.file, Done: true, Result: result}})
			toolMsg := Message{Role: "tool", ToolCallID: id, Content: result}
			messages = append(messages, assistant, toolMsg)
			extra = append(extra, assistant, toolMsg)
			toolRuns++
			continue
		}

		// Treat any accumulated tool calls as a tool turn even if the backend set
		// finish_reason to "stop" instead of "tool_calls" (some llama.cpp builds
		// do this) — otherwise we'd skip execution AND skip answering.
		if len(toolCalls) > 0 {
			// 1. Append assistant message with tool_calls so the model sees its own decision next turn.
			idxs := make([]int, 0, len(toolCalls))
			for k := range toolCalls {
				idxs = append(idxs, k)
			}
			sort.Ints(idxs)
			tcs := make([]ToolCall, 0, len(idxs))
			// Appels dont les arguments sont du JSON CASSÉ (tronqué, mal formé) : on ne
			// les exécute pas (voir plus bas). Des arguments vides restent valides :
			// c'est la forme normale d'un outil sans paramètre.
			badArgs := map[string]bool{}
			for i, k := range idxs {
				tc := *toolCalls[k]
				if tc.ID == "" {
					tc.ID = fmt.Sprintf("call_%d_%d", iter, i)
				}
				// Les arguments DOIVENT être du JSON valide : ils sont rangés dans
				// l'historique vu par le modèle, et le template de chat de llama.cpp les
				// re-parse à CHAQUE requête suivante. Un modèle très quantifié peut sortir
				// des arguments vides OU tronqués/non-JSON (ex: `{"command":"python3 …`) ;
				// stockés tels quels, ils font échouer le parsing du template → 500 en
				// boucle jusqu'au reset. On neutralise tout ce qui n'est pas du JSON valide
				// en objet vide (l'appel a de toute façon déjà été exécuté). json.Valid("")
				// étant faux, ça couvre aussi le cas vide d'origine.
				if !json.Valid([]byte(tc.Function.Arguments)) {
					if strings.TrimSpace(tc.Function.Arguments) != "" {
						badArgs[tc.ID] = true
					}
					tc.Function.Arguments = "{}"
				} else {
					// Nom ou argument appris d'un autre agent (read_file, old_string…) :
					// traduit vers l'outil réel, AVANT de ranger l'appel dans
					// l'historique — le modèle y relit l'appel tel qu'exécuté.
					repairToolCall(&tc, tools)
				}
				tcs = append(tcs, tc)
			}
			assistant := Message{Role: "assistant", ToolCalls: tcs}
			if s := assistantContent.String(); s != "" {
				assistant.Content = s
			}
			if echoText != "" {
				assistant.ReasoningContent, assistant.ReasoningModel = echoText, pol.model
			}
			messages = append(messages, assistant)
			extra = append(extra, assistant)
			// Début des résultats de CETTE étape, que le moteur n'a pas encore
			// comptés (voir le test de compactage en cours de tour, plus bas).
			stepStart := len(messages)
			// Appel émis par le protocole : le budget de relances « appel écrit
			// en texte » repart à neuf.
			patternRetries = 0
			// 2. Execute each tool locally and append a "tool" reply.
			for _, tc := range tcs {
				// Arrêt demandé : on n'enchaîne pas les outils restants. Sans ce
				// garde, un stop pendant une série d'appels laissait défiler toute
				// la série avant de reprendre la main.
				if ctx.Err() != nil {
					return extra, nil
				}
				toolRuns++ // alimente le budget souple (voir budgetNudge)
				var args map[string]any
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
				// Libellé humain, annoncé AVANT l'exécution (voir toolCallLabel) : sans
				// ça l'interface reste muette pendant une commande lente. Il sert
				// aussi d'argument principal à plusieurs outils.
				label := toolCallLabel(tc.Function.Name, args)
				cb(StreamEvent{ToolUsed: &ToolUsedEvent{Name: tc.Function.Name, Label: label}})

				result := ""
				// diff : rempli par les outils d'écriture (edit / mémoire) pour que
				// l'UI montre les lignes ajoutées et retirées.
				var diff []DiffLine
				var diffAdd, diffDel int // vrais totaux (diff est tronqué pour l'UI)
				// visionImg : partie image_url rendue par see_image, réinjectée après
				// le résultat de l'outil (un message `tool` ne porte que du texte).
				var visionImg map[string]any
				// Appel rigoureusement identique déjà exécuté dans ce tour : on ne le
				// rejoue pas. Les petits modèles réémettent volontiers deux fois la
				// même écriture ; la rejouer produisait une fausse erreur (« old
				// introuvable », puisque le remplacement est déjà fait).
				// Arguments illisibles : l'exécuter avec des arguments vides donnait une
				// erreur trompeuse (« fichier manquant », « commande vide »). On le dit
				// tel quel au modèle, pour qu'il renvoie un appel complet.
				if badArgs[tc.ID] {
					result = "[erreur] arguments de l'appel illisibles (JSON invalide ou tronqué) : l'outil n'a PAS été exécuté. Renvoie l'appel avec des arguments JSON complets et valides."
					cb(StreamEvent{ToolUsed: fillToolResult(&ToolUsedEvent{Name: tc.Function.Name, Label: label, Done: true}, result)})
					toolMsg := Message{Role: "tool", ToolCallID: tc.ID, Content: result}
					messages = append(messages, toolMsg)
					extra = append(extra, toolMsg)
					continue
				}
				callKey := tc.Function.Name + "\x00" + tc.Function.Arguments
				if prev, seen := doneCalls[callKey]; seen && dedupableTool(tc.Function.Name) {
					repeatCount[callKey]++
					result = repeatedCallResult(prev, repeatCount[callKey])
					cb(StreamEvent{ToolUsed: fillToolResult(&ToolUsedEvent{Name: tc.Function.Name, Label: label, Done: true}, result)})
					toolMsg := Message{Role: "tool", ToolCallID: tc.ID, Content: result}
					messages = append(messages, toolMsg)
					extra = append(extra, toolMsg)
					continue
				}
				switch tc.Function.Name {
				case "see_image":
					result, visionImg = toolSeeImage(label)
				case "recall":
					result = toolRecall(args)
				case "recall_search":
					result = toolRecallSearch(args)
				case "tracker":
					result = toolTracker(args)
				case "mem_search":
					lim := 0
					if v, ok := args["limit"].(float64); ok {
						lim = int(v)
					}
					hits := MemSearch(label, lim)
					if len(hits) == 0 {
						result = "[aucun résultat]"
					} else {
						var b strings.Builder
						for _, h := range hits {
							fmt.Fprintf(&b, "- %s — %s\n  %s\n", h.File, h.Title, h.Snippet)
						}
						result = strings.TrimRight(b.String(), "\n")
					}
				case "mem_read":
					off, lim := 0, 0
					if v, ok := args["offset"].(float64); ok {
						off = int(v)
					}
					if v, ok := args["limit"].(float64); ok {
						lim = int(v)
					}
					if c, rerr := MemRead(label, off, lim); rerr != nil {
						result = "[erreur] " + rerr.Error()
					} else {
						result = c
					}
				case "mem_add":
					content, _ := args["content"].(string)
					if werr := MemAdd(label, content); werr != nil {
						result = "[erreur] " + werr.Error()
					} else {
						result = fmt.Sprintf("[ok] page '%s' créée", label)
						diff, diffAdd = addedDiff(content)
					}
				case "mem_edit":
					oldText, _ := args["old"].(string)
					newText, _ := args["new"].(string)
					if werr := MemEdit(label, oldText, newText); errors.Is(werr, errAlreadyApplied) {
						result = fmt.Sprintf("[ok] page '%s' %s", label, werr.Error())
					} else if werr != nil {
						result = "[erreur] " + werr.Error()
					} else {
						result = fmt.Sprintf("[ok] page '%s' modifiée", label)
						diff, diffAdd, diffDel = lineDiff(oldText, newText)
					}
				case "write":
					content, _ := args["content"].(string)
					if tgt := agentTargetSlug(); tgt != "" {
						// Cible = un poste distant : on écrit LÀ-BAS. Pas de diff (on
						// n'a pas l'ancien contenu du fichier distant).
						result = nodeCall(tgt, nodeCapWrite, map[string]any{"path": label, "content": content})
					} else if msg := codeWriteGuard(caps, label, true); msg != "" {
						result = msg
					} else {
						result = fileWrite(label, content)
						if !strings.HasPrefix(result, "[erreur]") {
							diff, diffAdd = addedDiff(content)
							trackerNoteWrite(resolveAgentPath(label))
							result += lspDiagBlock(resolveAgentPath(label), caps)
						}
					}
				case "edit":
					oldText, _ := args["old"].(string)
					newText, _ := args["new"].(string)
					if tgt := agentTargetSlug(); tgt != "" {
						result = nodeEditRemote(tgt, label, oldText, newText)
					} else if msg := codeWriteGuard(caps, label, false); msg != "" {
						result = msg
					} else {
						result = fileEdit(label, oldText, newText)
						// Diff seulement si l'édition a réussi (sinon le fichier n'a pas bougé).
						if !strings.HasPrefix(result, "[erreur]") {
							diff, diffAdd, diffDel = lineDiff(oldText, newText)
							trackerNoteWrite(resolveAgentPath(label))
							result += lspDiagBlock(resolveAgentPath(label), caps)
						}
					}
				case "read":
					result = toolRead(args, caps.Code)
				case "grep":
					result = toolGrep(args, caps.Code)
				case "glob":
					result = toolGlob(args, caps.Code)
				case "ask":
					result = toolAsk(args)
					if !strings.HasPrefix(result, "[erreur]") {
						q, _ := args["question"].(string)
						cb(StreamEvent{Ask: &AskEvent{Question: q, Options: askOptions(args)}})
					}
				case "bash_bg":
					result = toolBashBg(args)
				case "bash_tail":
					result = toolBashTail(args)
				case "git_status":
					result = toolGitStatus(ctx, args)
				case "git_diff":
					result = toolGitDiff(ctx, args)
				case "git_clone":
					result = toolGitClone(ctx, args)
				case "criteria":
					result = toolCriteria(args, caps.Role == "verifier")
				case "subagent":
					result = toolSubagent(ctx, args, caps)
				case "bash":
					to := 0
					switch v := args["timeout"].(type) {
					case float64:
						to = int(v)
					case int:
						to = v
					}
					// Politique de sécurité : les commandes catastrophiques sont
					// refusées AVANT toute exécution, cible distante comprise.
					if reason := dangerousCommand(label); reason != "" {
						result = refusedCommandResult(reason)
						break
					}
					// Mode Code : « cmd & » laisse un process orphelin, sans sortie
					// lisible ni moyen de l'arrêter — bash_bg est fait pour ça.
					if caps.Code && trailingBackground(label) {
						result = "[refusé] commande terminée par « & » : lance-la avec bash_bg (sortie consultable avec bash_tail), pas en arrière-plan dans bash."
						break
					}
					if tgt := agentTargetSlug(); tgt != "" {
						// Cible = un poste distant : la commande s'exécute LÀ-BAS via le
						// canal du poste (fail-closed : nodeCall renvoie une erreur si le
						// poste est déconnecté, on n'exécute JAMAIS sur le serveur à sa place).
						result = nodeCall(tgt, nodeCapShell, map[string]any{"command": label, "timeout": to})
					} else {
						result = runShell(ctx, label, to)
					}
				case "web_search":
					result = capWebOutput(toolWebSearch(args))
				case "web_images":
					result = capWebOutput(toolWebImages(args))
				case "web_open":
					result = capWebOutput(toolWebOpen(args))
				case "web_read":
					result = capWebOutput(toolWebRead(args))
				case "web_grep":
					result = capWebOutput(toolWebGrep(args))
				case "web_screenshot":
					result = toolWebScreenshot(args, caps)
				case "task_list":
					result = toolTaskList(args)
				case "task_create":
					result = toolTaskCreate(args)
				case "task_update":
					result = toolTaskUpdate(args)
				case "task_delete":
					result = toolTaskDelete(args)
				case "browser_open":
					result = capCUOutput(toolCUOpen(args))
				case "browser_snapshot":
					result = capCUOutput(toolCUSnapshot())
				case "browser_find":
					result = capCUOutput(toolCUFind(args))
				case "browser_click":
					result = capCUOutput(toolCUClick(args))
				case "browser_click_xy":
					result = capCUOutput(toolCUClickXY(args))
				case "browser_type":
					result = capCUOutput(toolCUType(args))
				case "browser_key":
					result = capCUOutput(toolCUKey(args))
				case "browser_scroll":
					result = capCUOutput(toolCUScroll(args))
				case "browser_screenshot":
					result, visionImg = toolCUScreenshot()
				default:
					if isMCPTool(tc.Function.Name) {
						result = mcpCall(tc.Function.Name, args)
					} else {
						result = "[erreur] outil inconnu: " + tc.Function.Name
					}
				}
				// Aperçu en direct du navigateur piloté (repris d'AJEAN) : une
				// capture après chaque action qui change la page, pour l'UI seule.
				liveShot := ""
				if !strings.HasPrefix(result, "[erreur]") {
					doneCalls[callKey] = result
					if cuAutoShotTool(tc.Function.Name) {
						liveShot = cuAutoShot()
					}
				}
				// Capture d'écran : l'image part avec l'événement pour être
				// affichée dans la bulle, quoi que le modèle en fasse ensuite.
				shot := ""
				if tc.Function.Name == "web_screenshot" {
					shot = capturedRelPath(result)
				}
				cb(StreamEvent{ToolUsed: fillToolResult(&ToolUsedEvent{Name: tc.Function.Name, Label: label, Done: true, Diff: diff, Added: diffAdd, Removed: diffDel, Image: shot, Shot: liveShot}, result)})
				// Plafond pour le MODÈLE seulement : l'UI vient de recevoir le
				// résultat complet (aperçu + « voir plus »).
				toolMsg := Message{Role: "tool", ToolCallID: tc.ID, Content: capToolResult(result)}
				messages = append(messages, toolMsg)
				extra = append(extra, toolMsg)
				// Capture d'écran + vision active : on fait SUIVRE l'image elle-même
				// dans un message `user`. Un message `tool` ne transporte que du
				// texte, donc sans ce relais le modèle recevait le chemin du fichier
				// et rien d'autre — il annonçait alors à l'utilisateur qu'il ne
				// voyait pas l'image, alors que le projecteur était bien chargé.
				//
				// ÉPHÉMÈRE : l'image va dans `messages` (le tour en cours) mais PAS
				// dans `extra` (l'historique persistant). Un base64 de capture pèse
				// des dizaines de milliers de tokens ; persisté, il était renvoyé à
				// CHAQUE tour suivant et la conversation dépassait définitivement le
				// contexte (vu en production : requêtes de 55 000 tokens pour une
				// fenêtre de 32 768, plus aucun tour ne passait). Le modèle regarde
				// l'image MAINTENANT et sa description textuelle, elle, reste.
				//
				// KEEP_TURN_IMAGES (opt-in, chat_keep_images.go) : gardée dans
				// l'historique, par référence, sous un budget mesuré — kt.relay.
				// Sans la clé, kt est nil et relay fait exactement l'ajout d'avant.
				if tc.Function.Name == "web_screenshot" {
					if rel := capturedRelPath(result); rel != "" {
						if imgMsg, ok := screenshotImageMessage(rel); ok {
							messages, extra = kt.relay(messages, extra, imgMsg, stepStart)
						}
					}
				}
				// see_image a réussi : même relais, et surtout même règle ÉPHÉMÈRE
				// que la capture — l'image va dans `messages` (le tour en cours)
				// mais PAS dans `extra` (l'historique persistant). Un base64
				// persisté repartirait à CHAQUE tour suivant et finirait par
				// dépasser la fenêtre pour de bon. Le modèle regarde l'image
				// maintenant ; ce qu'il en dit, lui, reste.
				if visionImg != nil {
					messages, extra = kt.relay(messages, extra, seeImageMessage(label, visionImg), stepStart)
				}
			}
			// Compaction EN COURS DE TOUR. Le seuil n'était testé qu'AU DÉBUT du tour :
			// une boucle d'outils peut à elle seule remplir la fenêtre (résultats
			// enchaînés), on partait à 60% et on finissait en dépassement — rattrapé au
			// mieux par le filet réactif sur 500, une seule fois. On re-teste donc ici,
			// avec le contexte RÉEL du dernier appel (usage.prompt_tokens + généré).
			// + les résultats d'outils de CETTE étape : le moteur ne les a pas encore
			// comptés. Sans eux, une étape qui lit plusieurs gros fichiers d'un coup
			// passait de 60 % à plus de 130 % de la fenêtre sans jamais compacter.
			// Serveur sans usage (base 0) : on laisse 0, compactWouldTrigger estime
			// alors TOUT l'historique — étape comprise. Le raisonnement de l'étape
			// est retiré (ctxAfter) : il ne repart pas dans la requête suivante.
			used := 0
			if base := stats.ctxAfter(); base > 0 {
				used = base + estimateTokens(messages[stepStart:])
				lastEst = used
			}
			// COMPACT_CONTINUATION (chat_compact_cont.go) : tour de discussion
			// seulement, ni tâche ni sous-agent. Sans la clé, rien ne change.
			mainConv := ""
			if ptag.kind == perfMain {
				mainConv = ptag.conv
			}
			// KEEP_TURN_IMAGES : les images gardées partent AVANT tout résumé ;
			// la compaction ne suit que si elle reste nécessaire. Sans la clé,
			// aucune image gardée : rien ne change.
			if hasKeptImages(messages) && compactNeeded(messages, used, peakGen) {
				if s, freed := dropKeptImages(messages); freed > 0 {
					logKeptImagesDropped("en-tour", freed)
					messages = s
					if used > 0 {
						used = max(used-freed, 1)
						lastEst = used
					}
					extra = nil
					publishHistory()
				}
			}
			if compactNeeded(messages, used, peakGen) && !compactRefusedSkip(mainConv, used, messages) {
				opt := compactOptsMid(chatCfg, ep, mainConv, messages, used, tools, reasoningEffort, reasoningKwargs, pol,
					disableTools || toolChoiceNone)
				yes, no := true, false
				cb(StreamEvent{Compacting: &yes})
				c, changed := compactMessagesNoted(ctx, messages, caps, opt)
				cb(StreamEvent{Compacting: &no})
				logCompact("en-tour", used, messages, c, changed)
				if changed {
					lastEst = 0 // estimation d'avant compaction : plus rien à comparer
					messages = c
					// La nouvelle base contient déjà tout ce tour : on la publie et on
					// repart d'un `extra` vide, sinon l'appelant la ré-empilerait avec
					// les messages du tour et dupliquerait tout.
					extra = nil
					newHistory()
				}
			}
			continue
		}
		// Appel d'outil TEXTUEL (halluciné, jamais exécuté) dans la réponse
		// finale : on relance le tour UNE fois avec la consigne corrective
		// (code_retry.go). Le texte fautif reste affiché — le remplacer serait
		// mentir sur ce qui s'est passé — mais l'historique du modèle garde la
		// trace ET la correction, donc la vraie réponse suit immédiatement.
		if snippet := textualToolCallSnippet(assistantContent.String()); snippet != "" &&
			patternRetries < maxPatternRetries && callsOn {
			patternRetries++
			bad := withEcho(Message{Role: "assistant", Content: assistantContent.String()}, &ReasoningEcho{Text: echoText, Model: pol.model})
			fix := Message{Role: "user", Content: retryCorrective(snippet)}
			messages = append(messages, bad, fix)
			extra = append(extra, bad, fix)
			continue
		}
		// Normal end of turn. If the model produced no visible answer at all
		// (empty content, e.g. it stopped right after a tool result), say so
		// instead of leaving the user staring at a silent, finished chat.
		if strings.TrimSpace(assistantContent.String()) == "" {
			// Filet de sécurité (le vrai fix est le prompt court, voir baseSystemPrompt) :
			// si un modèle « pense sans agir » malgré tout, on le relance avec une
			// consigne impérative au lieu d'afficher « pas de réponse ».
			//
			// Sauf si la complétion a été COUPÉE par une fenêtre pleine
			// (« length » au-delà du seuil de compaction) : le modèle n'a pas
			// « pensé sans agir », il n'a plus eu de place. Lui dire d'arrêter de
			// raisonner raccourcirait sa réflexion et alourdirait encore le
			// contexte ; on compacte et on rejoue l'étape, une fois.
			if finishReason == "length" && !ep.External && lengthReplays < 1 &&
				compactWouldTrigger(messages, stats.PromptTokensTotal+stats.GenTokens) {
				lengthReplays++
				// KEEP_TURN_IMAGES : les images gardées d'abord ; le résumé ne suit
				// que s'il reste nécessaire. Sans la clé, rien à retirer.
				if s, freed := dropKeptImages(messages); freed > 0 {
					logKeptImagesDropped("fenêtre-pleine", freed)
					messages = s
					extra = nil
					publishHistory()
					if !compactWouldTrigger(messages, max(stats.PromptTokensTotal+stats.GenTokens-freed, 1)) {
						cb(StreamEvent{DropReasoning: true})
						continue
					}
				}
				yes, no := true, false
				cb(StreamEvent{Compacting: &yes})
				c, changed := compactMessages(ctx, messages, caps)
				cb(StreamEvent{Compacting: &no})
				logCompact("fenêtre-pleine", stats.PromptTokensTotal+stats.GenTokens, messages, c, changed)
				if changed {
					cb(StreamEvent{DropReasoning: true})
					messages = c
					extra = nil
					newHistory()
					continue
				}
			}
			if callsOn && nudgeCount < maxNudges {
				nudgeCount++
				logCtx("relance « pensé sans agir » n°%d (finish=%s)", nudgeCount, finishReason)
				// Le raisonnement de ce tour avorté ne mène à rien : on demande à
				// l'UI de l'effacer avant de relancer, pour ne pas afficher deux
				// blocs de réflexion successifs.
				cb(StreamEvent{DropReasoning: true})
				nudge := thinkNudgeFirst
				if nudgeCount > 1 {
					// Le premier nudge n'a pas suffi : le modèle re-décrit le même
					// plan sans l'exécuter. Second nudge plus impératif, où on lui
					// interdit explicitement de re-raisonner.
					nudge = thinkNudgeStuck
				}
				// Persisté tel qu'envoyé (appendNudge) : au tour suivant, le modèle
				// relit la consigne qui a produit sa réponse, et le préfixe en
				// cache tient jusqu'à elle.
				messages, extra = appendNudge(messages, extra, nudge)
				continue
			}
			cb(StreamEvent{Content: "_(le modèle n'a pas produit de réponse — finish: " + finishReason + ")_"})
		}
		// Raisonnement de la réponse finale : l'appelant le range avec elle.
		// Jamais sans texte (un message assistant de pur raisonnement serait
		// refusé par llama.cpp).
		if echoText != "" && strings.TrimSpace(assistantContent.String()) != "" {
			cb(StreamEvent{Echo: &ReasoningEcho{Text: echoText, Model: pol.model}})
		}
		return extra, nil
	}
}

// healthClient : /health doit répondre tout de suite ou pas du tout. Sans
// timeout (http.Get et son client par défaut n'en ont aucun), un moteur qui
// accepte la connexion sans jamais répondre — cas classique d'un très gros
// modèle en cours de chargement, ou d'un process figé — bloquait healthCheck
// indéfiniment. Et comme StartTurn commence par là, /api/chat/send restait
// pendu : l'utilisateur voyait un bouton d'envoi qui ne rendait jamais la main.
var healthClient = &http.Client{Timeout: 3 * time.Second}

// healthCheck pings llama.cpp's /health endpoint.
//
// Preset externe : il n'y a pas de llama-server local à sonder. On répond
// « prêt » sans latence — la vraie joignabilité de l'API distante se révèle à
// l'appel de complétion, avec un message d'erreur explicite si elle échoue.
// Sans ce court-circuit, la saisie resterait bloquée sur un moteur éteint que
// personne n'allumera jamais.
func healthCheck() bool {
	if externalActive() {
		return true
	}
	resp, err := healthClient.Get(fmt.Sprintf("http://localhost:%d/health", LLMPort()))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == 200
}
