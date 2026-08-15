package loki

import "testing"

// Plusieurs gabarits de conversation exigent que le message système soit le
// PREMIER — gpt-oss lève « System message must be at the beginning ». La
// consigne posée en cours de tour (repli après un 500) ne doit donc jamais
// arriver en fin de liste, sinon la tentative de récupération échoue à son tour
// et masque l'erreur d'origine.
func TestSteerSystemResteEnTete(t *testing.T) {
	hint := "N'appelle plus d'outil."

	t.Run("fusionne dans le système existant", func(t *testing.T) {
		msgs := []Message{
			{Role: "system", Content: "Tu es Loki."},
			{Role: "user", Content: "bonjour"},
			{Role: "assistant", Content: "salut"},
		}
		out := steerSystem(msgs, hint)
		if len(out) != len(msgs) {
			t.Fatalf("%d messages, attendu %d : aucun message ne doit être ajouté", len(out), len(msgs))
		}
		if got := out[0].Content.(string); got != "Tu es Loki.\n\n"+hint {
			t.Fatalf("consigne non fusionnée en tête : %q", got)
		}
		for i, m := range out[1:] {
			if m.Role == "system" {
				t.Fatalf("message système en position %d — il doit rester unique et en tête", i+1)
			}
		}
	})

	t.Run("sans système existant, il est posé en tête", func(t *testing.T) {
		msgs := []Message{{Role: "user", Content: "bonjour"}}
		out := steerSystem(msgs, hint)
		if out[0].Role != "system" || out[0].Content.(string) != hint {
			t.Fatalf("premier message = %+v, attendu le système", out[0])
		}
		if out[1].Role != "user" {
			t.Fatalf("la conversation d'origine doit suivre, obtenu %+v", out[1])
		}
	})

	t.Run("ne modifie pas la liste d'origine", func(t *testing.T) {
		msgs := []Message{{Role: "system", Content: "Tu es Loki."}, {Role: "user", Content: "bonjour"}}
		_ = steerSystem(msgs, hint)
		if got := msgs[0].Content.(string); got != "Tu es Loki." {
			t.Fatalf("liste d'appelant modifiée : %q", got)
		}
	})
}
