package loki

// chat_envctx.go — PROJ_SNAPSHOT : la date et le dossier de travail sortent du
// prompt système pour entrer dans le bloc projet figé de la discussion.
//
// Pourquoi. Le système (préambule + briefing machine) et les outils forment le
// début de chaque requête ; tant qu'ils ne bougent pas, le moteur reprend son
// cache. Deux valeurs y bougeaient pourtant : la date — à minuit, tout le prompt
// d'une discussion en cours est recalculé — et le dossier de travail, propre à
// chaque discussion — deux discussions ne partageaient donc rien, pas même le
// système. Avec la clé, le système ne garde que des consignes (« ton dossier de
// travail est dans ton contexte ») et les valeurs partent dans le bloc projet,
// en tête du premier message utilisateur, avec le reste du contexte figé.
//
// Rien n'est mémorisé au-delà du bloc lui-même : chaque tour relit la date, le
// dossier et la cible, et tout écart part en <context_update> dans le message
// suivant — une ligne pour un changement de jour, le bloc entier sinon (poste
// distant passé hors ligne, autre cible, autre dossier). Les mêmes faits, à une
// autre place ; aucun n'est retenu par Loki plus longtemps qu'avant.
//
// Seul un tour de discussion avec la clé y passe (assembleTurn pose
// caps.envInCtx). Une tâche planifiée, un sous-agent, la vérification du mode
// Code, le terminal gardent leur système d'avant, date et dossier compris.

import (
	"strings"
	"time"
)

// envContextPrefix ouvre le message (isProjectSystem le range avec le contexte
// du projet). Assez particulier pour qu'aucun prompt de preset ne commence ainsi.
const envContextPrefix = "Environment (from Loki)"

// nodeOfflineLine : l'avertissement d'un poste distant hors ligne — dans le
// système sans la clé, dans le bloc projet avec.
const nodeOfflineLine = "⚠ It is currently OFFLINE: those tools will fail until it reconnects. Tell the user instead of trying repeatedly."

// envNow : l'horloge du bloc (remplaçable en test).
var envNow = time.Now

// envDateLine : la date du jour, avec l'année et l'année périmée que la
// consigne web du système désignait jusque-là par leur valeur.
func envDateLine(day string) string {
	year := day
	if len(day) >= 4 {
		year = day[:4]
	}
	return "Date: " + day + " (the current year is " + year + ", not " + prevYear(year) + ")."
}

// envMachineFacts : les valeurs du briefing machine qui varient — dossier de la
// discussion, ou poste distant (nom, système, dossier, état). "" sans agent.
// Les consignes qui les accompagnent restent dans machineSystemPrompt.
func envMachineFacts(caps Caps) string {
	if !caps.Agent {
		return ""
	}
	if tgt, ok := nodeTargetMetaGet(); ok {
		s := "Machine: REMOTE node " + tgt.name
		if tgt.os != "" {
			s += " (" + tgt.os + ")"
		}
		if tgt.root != "" {
			s += ", working folder " + tgt.root
		}
		s += "."
		if !tgt.connected {
			s += " " + nodeOfflineLine
		}
		return s
	}
	if cwd := agentCwd(); cwd != "" {
		return "Working folder (cwd): " + cwd + "."
	}
	return ""
}

// envCapture : la date (si le préambule en portait une) et les valeurs machine
// de ce tour.
func envCapture(caps Caps) (day, facts string) {
	if caps.Agent || caps.Mem != MemOff {
		day = envNow().Format("2006-01-02")
	}
	return day, envMachineFacts(caps)
}

// envContextMessage : le message du bloc, ok=false s'il n'a rien à dire.
func envContextMessage(day, facts string) (Message, bool) {
	var lines []string
	if day != "" {
		lines = append(lines, envDateLine(day))
	}
	if facts != "" {
		lines = append(lines, facts)
	}
	if len(lines) == 0 {
		return Message{}, false
	}
	return Message{Role: "system", Content: envContextPrefix + ":\n" + strings.Join(lines, "\n")}, true
}

// envDelta : la mise à jour entre l'environnement annoncé et le vivant. Un
// changement de jour seul tient en une ligne ; tout autre écart renvoie le
// message entier.
func envDelta(oldDay, oldFacts, day, facts string) string {
	switch {
	case oldFacts == facts && oldDay == day:
		return ""
	case oldFacts == facts && day != "" && oldDay != "":
		return "New day — " + envDateLine(day) + "\n"
	}
	if m, ok := envContextMessage(day, facts); ok {
		return "Current version, replaces any earlier one — " + msgText(m) + "\n"
	}
	return "Environment: removed.\n"
}
