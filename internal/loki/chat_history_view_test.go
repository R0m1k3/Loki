package loki

import (
	"reflect"
	"strings"
	"testing"
)

// Compaction EN COURS de tour : la vue publiée perd ce que la vue d'envoi avait
// posé en tête (préambule, prompt du preset, contexte du projet), mais garde le
// rappel des pages lues, qui appartient à l'historique.
func TestCompactionEnTourGardeLeRappel(t *testing.T) {
	for _, on := range []bool{false, true} {
		m := projSnapSetup(t, on)
		if err := saveSysPrompt("Réponds en français."); err != nil {
			t.Fatal(err)
		}
		if err := MemAdd("nas.md", "# NAS\n"); err != nil {
			t.Fatal(err)
		}
		reminder, _ := buildReminderMessage([]string{"nas.md"})
		c := newTestConv()
		c.Messages = []Message{reminder}
		for i := 0; i < 12; i++ {
			c.Messages = append(c.Messages, um("continue "+strings.Repeat("x", 600)), am("étape "+strings.Repeat("détail ", 300)))
		}
		n := len(m.all())
		m.mu.Lock()
		m.reply = func(k int, _ string) (int, string) {
			if k == n {
				return 400, `{"error":{"code":400,"message":"request (99999 tokens) exceeds the available context size"}}`
			}
			return 200, sseChunk("ok.") + sseFinal("stop", 100, 2)
		}
		m.mu.Unlock()
		playTurn(t, m, c, Caps{Agent: true, Mem: MemAlways}, "et le NAS ?")
		if len(m.all()) < n+2 {
			t.Fatalf("clé %v : pas de requête rejouée après compaction", on)
		}
		if len(c.Messages) == 0 || !reflect.DeepEqual(c.Messages[0], reminder) {
			t.Fatalf("clé %v : rappel des pages lues perdu : %+v", on, c.Messages[0])
		}
		for _, msg := range c.Messages[1:] {
			if msg.Role == "system" {
				t.Fatalf("clé %v : système injecté rangé dans l'historique : %q", on, msgText(msg))
			}
		}
		if len(c.Messages) >= 25 {
			t.Fatalf("clé %v : compaction non rangée (%d messages)", on, len(c.Messages))
		}
		if err := saveSysPrompt(""); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHistoryFromView(t *testing.T) {
	rem := Message{Role: "system", Content: memReminderPrefix + " Memory pages you read earlier: a.md"}
	sys := Message{Role: "system", Content: "/sys perso"}
	u := um("q")
	cases := []struct {
		name       string
		view, hist []Message
		want       []Message
	}{
		{"injectés seuls", []Message{{Role: "system", Content: "préambule"}, {Role: "system", Content: projectContextPrefix + " x"}, u}, []Message{u}, []Message{u}},
		{"rappel gardé", []Message{{Role: "system", Content: "préambule"}, {Role: "system", Content: projectContextPrefix + " x"}, rem, u}, []Message{rem, u}, []Message{rem, u}},
		{"système fusionné rendu", []Message{{Role: "system", Content: "préambule\n\n/sys perso"}, u}, []Message{sys, u}, []Message{sys, u}},
		{"sans système", []Message{u}, []Message{u}, []Message{u}},
	}
	for _, tc := range cases {
		if got := historyFromView(tc.view, tc.hist); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s : %+v, attendu %+v", tc.name, got, tc.want)
		}
	}
}
