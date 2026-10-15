package loki

import (
	"encoding/json"
	"strings"
	"testing"
)

func turnJSON(t *testing.T, path, body string, want int) map[string]any {
	t.Helper()
	w := appelOAIInterne(t, "POST", path, body)
	if w.Code != want {
		t.Fatalf("%s : %d %s", path, w.Code, w.Body.String())
	}
	var m map[string]any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// Début d'un tour : message vu par le modèle, modèle, capacités du tour,
// puce « mode Code » une seule fois par discussion.
func TestTourInterneDebut(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	turnJSON(t, "/internal/v1/turn/start", `{"message":"  "}`, 400)
	if err := SetConfigKey("MODEL", "/data/models/Qwen3-8B.gguf"); err != nil {
		t.Fatal(err)
	}
	got := turnJSON(t, "/internal/v1/turn/start", `{"message":"salut"}`, 200)
	if got["content"] != "salut" || got["model"] != "Qwen3-8B" || got["code_hint"] != false {
		t.Fatalf("début : %v", got)
	}
	if files, _ := got["files"].([]any); files == nil || len(files) != 0 {
		t.Fatalf("files : %v", got["files"])
	}
	if c, _ := got["caps"].(map[string]any); c["Agent"] != false {
		t.Fatalf("caps sans agent : %v", c)
	}

	// Agent actif : un message qui ressemble à du code fait apparaître la puce,
	// une seule fois.
	if err := setAgentEnabled(true); err != nil {
		t.Fatal(err)
	}
	got = turnJSON(t, "/internal/v1/turn/start", "{\"message\":\"corrige main.go\"}", 200)
	if got["code_hint"] != true {
		t.Fatalf("puce attendue : %v", got)
	}
	if got = turnJSON(t, "/internal/v1/turn/start", "{\"message\":\"corrige main.go\"}", 200); got["code_hint"] != false {
		t.Fatalf("puce montrée deux fois : %v", got)
	}

	// Mode Code : rôle du tour figé (plan demandé → planner).
	if err := setConvMode(convEnsureActive(), "code"); err != nil {
		t.Fatal(err)
	}
	got = turnJSON(t, "/internal/v1/turn/start", `{"message":"fais un plan"}`, 200)
	tc, _ := got["turn_caps"].(map[string]any)
	c, _ := got["caps"].(map[string]any)
	if tc["Code"] != true || tc["Role"] == "" || c["Role"] != "" {
		t.Fatalf("rôle du tour : caps=%v turn_caps=%v", c, tc)
	}
}

// Vue d'un tour : rien du projet sans agent ; tête et préambule avec ; refus
// sous PROJ_SNAPSHOT.
func TestTourInterneVue(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	got := turnJSON(t, "/internal/v1/turn/view", `{"caps":{"Agent":false,"Mem":"off"}}`, 200)
	if h, _ := got["head"].([]any); len(h) != 0 {
		t.Fatalf("sans agent, rien du projet : %v", got["head"])
	}
	off := Caps{Mem: MemOff}
	if got["prefix"] != injectPrefix(off, EnabledTools(off)) {
		t.Fatalf("préambule sans agent : %v", got["prefix"])
	}
	if err := saveSysPrompt("Réponds en vers."); err != nil {
		t.Fatal(err)
	}
	got = turnJSON(t, "/internal/v1/turn/view", `{"caps":{"Agent":true,"Mem":"off"}}`, 200)
	tools, _ := got["tools"].([]any)
	prefix, _ := got["prefix"].(string)
	head, _ := got["head"].([]any)
	if len(tools) == 0 || prefix == "" || len(head) == 0 {
		t.Fatalf("agent : %d outils, préambule %q, tête %v", len(tools), prefix, head)
	}
	if h0, _ := head[0].(map[string]any); h0["content"] != "Réponds en vers." {
		t.Fatalf("prompt du preset en tête : %v", head[0])
	}
	// Même préambule que celui que pose InjectSkills.
	var caps Caps
	_ = json.Unmarshal([]byte(`{"Agent":true,"Mem":"off"}`), &caps)
	if want := injectPrefix(caps, EnabledTools(caps)); prefix != want {
		t.Fatal("préambule différent de celui d'InjectSkills")
	}
	if err := SetConfigKey("PROJ_SNAPSHOT", "on"); err != nil {
		t.Fatal(err)
	}
	turnJSON(t, "/internal/v1/turn/view", `{"caps":{"Agent":true}}`, 409)
	turnJSON(t, "/internal/v1/turn/view", `{"caps":{"Agent":false,"Mem":"off"}}`, 200)
}

// État par discussion : l'active suit la façade ; une discussion supprimée
// emporte son mode ; fin de tour sans abonnés push.
func TestTourInterneDiscussions(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	turnJSON(t, "/internal/v1/conv/active", `{"id":"c99"}`, 204)
	if got := convActiveCached(); got != "c99" {
		t.Fatalf("active en cache : %q", got)
	}
	turnJSON(t, "/internal/v1/conv/active", `{"id":" "}`, 400)
	if err := setConvMode("c99", "code"); err != nil {
		t.Fatal(err)
	}
	turnJSON(t, "/internal/v1/conv/dropped", `{"id":"c99"}`, 204)
	if getStr(bkChat, modeKey("c99")) != "" {
		t.Fatal("mode de la discussion supprimée resté en base")
	}
	turnJSON(t, "/internal/v1/turn/done", `{"ms":1200}`, 204)
	if w := appelOAIInterne(t, "POST", "/internal/v1/conv/dropped", "{"); w.Code != 400 || strings.Count(w.Body.String(), "error") != 1 {
		t.Fatalf("corps illisible : %d %q", w.Code, w.Body.String())
	}
}

// La passe de vérification de la façade reçoit exactement le contexte isolé
// de runVerifyPass : prompt du rôle et faits de la machine, outils fermés,
// rôle verifier (seul autorisé à marquer un critère « passed »).
func TestTourInterneVerificateur(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	got := turnJSON(t, "/internal/v1/turn/verifier", `{"caps":{"Agent":true,"Code":true,"Mem":"off","Role":"builder"}}`, 200)
	vcaps := Caps{Agent: true, Code: true, Mem: MemOff, Role: "verifier"}
	if got["system"] != rolePrompt("verifier")+"\n\n"+machineSystemPrompt(vcaps) {
		t.Fatalf("système : %v", got["system"])
	}
	caps, _ := got["caps"].(map[string]any)
	if caps["Role"] != "verifier" || caps["Code"] != true {
		t.Fatalf("caps : %v", caps)
	}
	tools, _ := got["tools"].([]any)
	var names []string
	for _, tl := range tools {
		f, _ := tl.(map[string]any)["function"].(map[string]any)
		names = append(names, f["name"].(string))
	}
	var want []string
	for _, tl := range EnabledTools(vcaps) {
		want = append(want, tl.Function.Name)
	}
	if strings.Join(names, ",") != strings.Join(want, ",") || !strings.Contains(strings.Join(names, ","), "criteria") {
		t.Fatalf("outils : %v, attendu %v", names, want)
	}
	turnJSON(t, "/internal/v1/turn/verifier", `{`, 400)
}
