package loki

import (
	"reflect"
	"strings"
	"testing"
)

// Le contexte PROJET quitte le bloc système pour la tête du 1er message user :
// le système commun reste identique d'un projet à l'autre (cache de prompt), et
// l'historique d'origine n'est pas muté.
func TestNormalizeMovesProjectContextToFirstUser(t *testing.T) {
	in := []Message{
		{Role: "system", Content: "Préambule commun"},
		{Role: "system", Content: projectContextPrefix + " — projet A"},
		{Role: "system", Content: memIndexPrefix + " for project A"},
		{Role: "user", Content: "bonjour"},
		{Role: "assistant", Content: "salut"},
		{Role: "user", Content: "suite"},
	}
	ref := append([]Message(nil), in...)
	out := normalizeSystemMessages(in)
	if len(out) != 4 || out[0].Content != "Préambule commun" {
		t.Fatalf("système commun attendu seul en tête : %#v", out)
	}
	first, _ := out[1].Content.(string)
	if !strings.HasPrefix(first, "<project_context>\n"+projectContextPrefix) ||
		!strings.Contains(first, memIndexPrefix) || !strings.HasSuffix(first, "bonjour") {
		t.Fatalf("contexte projet absent du 1er message user : %q", first)
	}
	if out[3].Content != "suite" {
		t.Fatal("seul le PREMIER message user reçoit le contexte")
	}
	if !reflect.DeepEqual(in, ref) {
		t.Fatal("l'entrée a été mutée (interdit)")
	}
}

// Sans prompt de preset, le premier système est un message projet : le préambule
// ne doit pas l'absorber (il perdrait son préfixe et resterait dans le système).
func TestInjectSkillsKeepsProjectMessageSeparate(t *testing.T) {
	in := []Message{
		{Role: "system", Content: projectContextPrefix + " — projet A"},
		{Role: "user", Content: "bonjour"},
	}
	out := normalizeSystemMessages(InjectSkills(in, Caps{Agent: true}, nil))
	if s, _ := out[0].Content.(string); out[0].Role != "system" || strings.Contains(s, projectContextPrefix) {
		t.Fatalf("le contexte projet ne doit pas rester dans le système : %#v", out[0])
	}
	if u, _ := out[1].Content.(string); !strings.Contains(u, projectContextPrefix) {
		t.Fatalf("contexte projet attendu dans le 1er message user : %#v", out[1])
	}
}
