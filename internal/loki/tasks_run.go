package loki

// tasks_run.go — exécution d'une tâche, ISOLÉE de la discussion ouverte.
// Repris de l'amont AJEAN (v0.9.9 → v0.10.2), adapté au fonctionnement de loki
// (dossier de travail par discussion, modes de la machine).
//
// Une tâche ne doit ni apparaître dans le chat, ni casser le rejeu SSE, ni entrer
// en collision avec un tour utilisateur. On ne partage donc qu'UN SEUL point avec
// la conversation : son verrou de génération (conv.Generating), qui garantit
// qu'une seule inférence tourne à la fois — llama-server est lancé en
// --parallel 1. Tout le reste — messages, journal d'affichage — n'est jamais
// touché : la tâche construit son propre fil éphémère et le jette à la fin, ne
// gardant que le texte final comme compte-rendu.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RunAutonomous exécute un prompt en arrière-plan, sans laisser de trace dans la
// discussion ouverte, et renvoie le texte final produit par l'IA (le compte-rendu).
// Prend le MÊME verrou que StartTurn : renvoie ErrBusy si un tour (utilisateur ou
// une autre tâche) est déjà en cours, ou errModelLoading si le modèle n'est pas
// prêt. caps gouverne l'accès aux outils (agent, mémoire, internet).
// lastReport est le compte-rendu du passage précédent (vide au premier) : il est
// réinjecté pour donner à l'IA une continuité d'une exécution à l'autre.
func (c *Conversation) RunAutonomous(ctx context.Context, taskID, taskName, prompt, lastReport string, caps Caps, temperature float64) (string, error) {
	if !healthCheck() {
		return "", errModelLoading
	}
	// Contexte annulable propre à la tâche, exposé via c.cancel : le bouton stop
	// du chat (/api/chat/stop → conv.Stop) interrompt donc VRAIMENT une tâche de
	// fond, au lieu de l'afficher « en cours » sans pouvoir l'arrêter.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.mu.Lock()
	if c.Generating {
		c.mu.Unlock()
		return "", ErrBusy
	}
	c.Generating = true
	c.cancel = cancel
	c.runningTaskID = taskID
	c.runningTaskName = taskName
	c.mu.Unlock()
	// Libère le verrou quoi qu'il arrive. On ne touche NI c.Messages NI c.Log NI
	// c.epoch : la discussion ouverte reste totalement à l'écart.
	defer func() {
		c.mu.Lock()
		c.Generating = false
		c.cancel = nil
		c.runningTaskID = ""
		c.runningTaskName = ""
		c.mu.Unlock()
	}()

	// Dossier de travail de la tâche : le sien, pas celui de la discussion
	// ouverte (chat_workspace.go). Stable d'un passage à l'autre — une tâche qui
	// tient un fichier de suivi le retrouve la fois suivante.
	setTaskWorkspace(taskWorkspaceDir(taskID))
	defer setTaskWorkspace("")

	if temperature == 0 {
		temperature = 0.7
	}
	msgs := []Message{{Role: "user", Content: prompt}}
	// Même préambule que la vraie génération : consigne personnelle + préambule
	// agent + briefing machine (via InjectSkills), pour que l'IA ait le même
	// contexte et les mêmes outils qu'en chat. On préfixe le tout d'une note de
	// contexte (conscience du mode autonome + mémoire du passage précédent),
	// fusionnée dans UN SEUL message système en tête — comme l'exigent les
	// gabarits stricts (normalizeSystemMessages le garantit de toute façon).
	final := msgs
	sys := readSysPrompt()
	note := taskContextNote(taskName, lastReport)
	switch {
	case sys != "" && note != "":
		final = append([]Message{{Role: "system", Content: sys + "\n\n" + note}}, msgs...)
	case sys != "":
		final = append([]Message{{Role: "system", Content: sys}}, msgs...)
	case note != "":
		final = append([]Message{{Role: "system", Content: note}}, msgs...)
	}

	var content strings.Builder
	_, err := runChat(ctx, InjectSkills(final, caps), temperature, caps, func(ev StreamEvent) bool {
		if ev.Content != "" {
			content.WriteString(ev.Content)
		}
		return true
	})
	// Les messages d'outils ne sont pas conservés : la tâche est éphémère, seul
	// son compte-rendu survit.
	return strings.TrimSpace(content.String()), err
}

// taskWorkspaceDir donne (et crée) le dossier de travail d'une tâche, sous la
// racine du dossier de travail — jamais dans le dossier d'une discussion, dont
// il partagerait le sort à la suppression.
func taskWorkspaceDir(taskID string) string {
	root := agentWorkspace()
	if taskID == "" {
		return root
	}
	dir := filepath.Join(root, "tasks", taskID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return root
	}
	return dir
}

// taskContextNote construit la note de contexte injectée en tête du fil d'une
// tâche : d'abord la conscience du mode (l'IA tourne seule, sans utilisateur en
// face), puis — s'il existe — le compte-rendu du passage précédent, pour la
// continuité.
func taskContextNote(taskName, lastReport string) string {
	var b strings.Builder
	b.WriteString("[Tâche planifiée")
	if taskName != "" {
		b.WriteString(" « " + taskName + " »")
	}
	b.WriteString("] Tu es exécuté automatiquement en arrière-plan, sans utilisateur " +
		"présent pour te répondre. Mène la tâche à son terme de façon autonome, puis " +
		"termine par un compte-rendu clair et court de ce que tu as fait ou trouvé.")
	if r := strings.TrimSpace(lastReport); r != "" {
		if len(r) > reportMax {
			r = r[:reportMax] + "…"
		}
		b.WriteString("\n\nCompte-rendu de ta dernière exécution (pour la continuité — " +
			"appuie-toi dessus, ne le répète pas tel quel) :\n" + r)
	}
	return b.String()
}

// reportMax borne la taille du compte-rendu conservé : le texte final peut être
// long, on n'en garde qu'un aperçu pour l'interface et pour la relance suivante.
const reportMax = 4000

// runTask exécute une tâche et met à jour son état persisté (dernier passage,
// prochain passage, succès/échec, compte-rendu).
func runTask(t Task) {
	start := time.Now()

	// Preset épinglé : on bascule le moteur dessus AVANT d'exécuter (rechargement
	// du modèle). Vide = on garde le preset actif. Un échec de bascule est un vrai
	// échec de la tâche : la faire tourner sur le mauvais modèle serait pire.
	if err := ensureTaskPreset(t.Preset); err != nil {
		recordTaskEnd(t.ID, start, "", fmt.Errorf("changement de preset : %w", err))
		return
	}

	report, err := conv.RunAutonomous(context.Background(), t.ID, t.Name, t.Prompt, t.LastReport, taskCaps(t), 0)

	// Occupé, ou modèle pas encore prêt : ce n'est pas un échec de la tâche, juste
	// un mauvais moment. On ne touche pas à son état (NextRun reste dans le passé)
	// pour qu'elle soit réessayée au tick suivant.
	if err == ErrBusy || err == errModelLoading {
		return
	}
	recordTaskEnd(t.ID, start, report, err)
}

// taskCaps dérive les capacités d'une tâche : on part du mode agent de la machine
// (qui débloque les outils), puis on applique les réglages propres à la tâche pour
// la mémoire et le web. Le web reste borné à ce que la machine offre réellement
// (serveur Crawl4AI configuré et joignable) : une tâche ne peut pas l'inventer.
//
// Le mode code reste OFF : c'est un mode de discussion (rôles, critères,
// vérification, puces dans le fil) qui n'a pas de sens sans personne en face.
func taskCaps(t Task) Caps {
	// Mem posé EXPLICITEMENT à MemOff : le zéro de MemMode est la chaîne vide,
	// que EnabledTools ne reconnaît pas comme « coupée » (il compare à MemOff) —
	// une tâche sans agent se serait donc vu offrir les outils mem_*.
	c := Caps{Agent: agentEnabled(), Mem: MemOff}
	if !c.Agent {
		return c // agent coupé : aucun outil, mémoire et web n'ont plus de sens
	}
	if t.NoWeb {
		c.Internet = false
	} else {
		c.Internet = internetEnabled() && crawlReachable()
	}
	if t.NoMem {
		c.Mem = MemOff
	} else if m := memMode(); m == MemOff {
		// La tâche veut la mémoire mais la machine l'a coupée globalement : on donne
		// au moins les outils à la demande, sans l'injection proactive.
		c.Mem = MemOnDemand
	} else {
		c.Mem = m
	}
	return c
}

// ensureTaskPreset bascule le moteur sur le preset d'id `id` s'il n'est pas déjà
// actif, puis attend que le modèle ait rechargé. Vide = rien à faire.
func ensureTaskPreset(id string) error {
	if id == "" {
		return nil
	}
	list, err := ListPresets()
	if err != nil {
		return err
	}
	var target *Preset
	for i := range list {
		if list[i].ID == id {
			target = &list[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("preset introuvable : %s", id)
	}
	if target.Active {
		return nil
	}
	if err := SwitchToPreset(target.Path); err != nil {
		return err
	}
	// Le moteur redémarre : on attend qu'il réponde à nouveau (charger un gros
	// modèle prend un moment).
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		if healthCheck() {
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("le modèle n'a pas fini de charger après le changement de preset")
}

// recordTaskEnd met à jour l'état persisté d'une tâche après une exécution :
// durée, dernier passage, prochain passage, succès/échec, compte-rendu. Relit la
// tâche juste avant d'écrire pour ne pas écraser une modification concurrente
// (bascule ou édition depuis l'interface pendant l'exécution).
func recordTaskEnd(id string, start time.Time, report string, err error) {
	now := time.Now()
	cur, ok := getTask(id)
	if !ok {
		return // supprimée entre-temps : rien à écrire
	}
	cur.LastDurMs = now.Sub(start).Milliseconds()
	cur.LastRun = now.UnixMilli()
	cur.NextRun = computeNextRun(cur.Schedule, cur.TZ, now)
	if errors.Is(err, context.Canceled) {
		// Arrêt volontaire (bouton stop) : ce n'est pas un échec. On garde la trace
		// « interrompue » sans allumer l'indicateur rouge d'erreur.
		cur.LastOK = true
		cur.LastError = ""
		cur.LastReport = "(interrompue manuellement)"
	} else if err != nil {
		cur.LastOK = false
		cur.LastError = err.Error()
	} else {
		cur.LastOK = true
		cur.LastError = ""
		if len(report) > reportMax {
			report = report[:reportMax] + "…"
		}
		cur.LastReport = report
	}
	_ = saveTask(cur)
}
