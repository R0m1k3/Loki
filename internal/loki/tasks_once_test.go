package loki

import (
	"strings"
	"testing"
	"time"
)

// « @once » : une date future est acceptée, une date passée refusée.
func TestScheduleOnce(t *testing.T) {
	from := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	at, err := nextAfter("@once 2026-10-02 12:30", "UTC", from)
	if err != nil || !at.Equal(from.Add(30*time.Minute)) {
		t.Fatalf("@once futur : %v, %v", at, err)
	}
	if _, err := nextAfter("@once 2026-10-02 11:00", "UTC", from); err == nil {
		t.Fatal("@once passé accepté")
	}
	if !isOnce(" @ONCE 2026-10-02 12:30") || isOnce("@every 2h") {
		t.Fatal("isOnce se trompe")
	}
}

// L'IA crée un rappel « dans 20 minutes » ou « à HH:MM » sans écrire de cron,
// dans le fuseau du navigateur ; une fois passée, la tâche se désactive.
func TestTacheUniqueParLIA(t *testing.T) {
	testHome(t)
	rememberUserTZ("Europe/Paris")
	if got := defaultTaskTZ(); got != "Europe/Paris" {
		t.Fatalf("fuseau retenu = %q", got)
	}
	rememberUserTZ("Pas/Un_Fuseau") // invalide : ignoré
	if got := defaultTaskTZ(); got != "Europe/Paris" {
		t.Fatalf("fuseau invalide retenu : %q", got)
	}

	out := toolTaskCreate(map[string]any{"name": "rappel", "prompt": "dis bonjour", "in_minutes": float64(20)})
	if !strings.HasPrefix(out, "[ok]") || !strings.Contains(out, "Europe/Paris") {
		t.Fatalf("création « dans 20 min » : %s", out)
	}
	out = toolTaskCreate(map[string]any{"name": "apéro", "prompt": "rappelle l'apéro", "at": "18:30"})
	if !strings.HasPrefix(out, "[ok]") {
		t.Fatalf("création « à 18:30 » : %s", out)
	}
	if out := toolTaskCreate(map[string]any{"name": "flou", "prompt": "x", "at": "demain"}); !strings.HasPrefix(out, "[erreur]") {
		t.Fatalf("'at' invalide accepté : %s", out)
	}

	var once Task
	for _, tk := range listTasks() {
		if tk.Name == "rappel" {
			once = tk
		}
	}
	if !isOnce(once.Schedule) || once.TZ != "Europe/Paris" || once.NextRun == 0 {
		t.Fatalf("tâche unique mal posée : %+v", once)
	}
	want := time.Now().Add(20 * time.Minute)
	if d := time.UnixMilli(once.NextRun).Sub(want); d > time.Minute || d < -time.Minute {
		t.Fatalf("prochaine exécution à %v, attendu ≈ %v", time.UnixMilli(once.NextRun), want)
	}

	recordTaskEnd(once.ID, time.Now(), "bonjour", nil)
	after, _ := getTask(once.ID)
	if after.Enabled || after.NextRun != 0 {
		t.Fatalf("tâche unique encore active après son passage : %+v", after)
	}
}
