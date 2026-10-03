package loki

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const sseStop = `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"

const sseGlobCall = `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"g1","type":"function","function":{"name":"glob","arguments":"{\"pattern\":\"*.go\"}"}}]}}]}` + "\n\n" +
	`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n"

// scriptedServer répond à la n-ième requête par steps[n] (le dernier se répète)
// et garde le corps JSON de chaque requête. Un step "500" renvoie une erreur
// 500, comme llama.cpp sur un appel d'outil qu'il ne sait pas parser.
func scriptedServer(t *testing.T, steps ...string) func() []map[string]any {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		n := len(bodies) - 1
		mu.Unlock()
		step := steps[min(n, len(steps)-1)]
		switch step {
		case "500":
			http.Error(w, "Failed to parse tool call", http.StatusInternalServerError)
			return
		case "400":
			http.Error(w, "Unsupported param: tool_choice", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(step))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	if err := SetConfigKey("PORT", u.Port()); err != nil {
		t.Fatal(err)
	}
	return func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), bodies...)
	}
}

// reqMessages relit les messages d'une requête enregistrée.
func reqMessages(t *testing.T, body map[string]any) []Message {
	t.Helper()
	raw, _ := json.Marshal(body["messages"])
	var out []Message
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestIsLokiInjected(t *testing.T) {
	cases := []struct {
		name string
		m    Message
		want bool
	}{
		{"budget 1", um(budgetNudge(24, 24, 0)), true},
		{"budget 2", um(budgetNudge(48, 24, 1)), true},
		{"budget 3", um(budgetNudge(72, 24, 2)), true},
		{"budget suivants", um(budgetNudge(150, 24, 5)), true},
		{"consigne du 500 en message à part", um(lokiNotePrefix + toolsOffHint), true},
		{"demande qui commence comme un rappel", um(lokiNotePrefix + "réécris ce prompt système"), false},
		{"pensé sans agir", um(thinkNudgeFirst), true},
		{"pensé sans agir, bis", um(thinkNudgeStuck), true},
		{"appel écrit en texte", um(retryCorrective("<tool_call>x")), true},
		{"vraie demande", um("cherche les horaires"), false},
		{"demande qui cite un rappel", um("pourquoi « " + thinkNudgeFirst + " » ?"), false},
		{"assistant", am(lokiNotePrefix + "x"), false},
		{"user multimodal", Message{Role: "user", Content: []any{map[string]any{"type": "text", "text": lokiNotePrefix}}}, false},
	}
	for _, c := range cases {
		if got := isLokiInjected(c.m); got != c.want {
			t.Errorf("%s : isLokiInjected = %v, attendu %v", c.name, got, c.want)
		}
	}
}

// Le rappel entre dans l'historique tel qu'envoyé, sauf derrière un autre
// message user (gabarits à alternance stricte) où il reste éphémère.
func TestAppendNudge(t *testing.T) {
	cases := []struct {
		name      string
		before    []Message
		persisted bool
	}{
		{"après un résultat d'outil", []Message{um("q"), atc("glob"), tm("r")}, true},
		{"après un assistant", []Message{um("q"), am("a")}, true},
		{"après la demande", []Message{um("q")}, false},
		{"après un autre rappel", []Message{um("q"), atc("glob"), tm("r"), um(thinkNudgeFirst)}, false},
		{"fil vide", nil, true},
	}
	for _, c := range cases {
		msgs, extra := appendNudge(append([]Message(nil), c.before...), nil, thinkNudgeFirst)
		if len(msgs) != len(c.before)+1 || msgText(msgs[len(msgs)-1]) != thinkNudgeFirst {
			t.Errorf("%s : rappel absent de la vue du modèle : %+v", c.name, msgs)
		}
		if got := len(extra) == 1; got != c.persisted {
			t.Errorf("%s : persisté = %v, attendu %v", c.name, got, c.persisted)
		}
		if c.persisted && !reflect.DeepEqual(extra[0], msgs[len(msgs)-1]) {
			t.Errorf("%s : persisté %+v ≠ envoyé %+v", c.name, extra[0], msgs[len(msgs)-1])
		}
	}
}

func TestWithTrailingHint(t *testing.T) {
	note := "\n\n" + lokiNotePrefix + "H"
	cases := []struct {
		name string
		in   []Message
		want Message // dernier message attendu
		grow bool    // un message ajouté plutôt qu'enrichi
	}{
		{"résultat d'outil", []Message{um("q"), atc("glob"), tm("r")}, Message{Role: "tool", ToolCallID: "c1", Content: "r" + note}, false},
		{"message user", []Message{um("q")}, um("q" + note), false},
		{"parties []any", []Message{{Role: "user", Content: []any{map[string]any{"type": "text", "text": "q"}}}},
			Message{Role: "user", Content: []any{map[string]any{"type": "text", "text": "q"}, map[string]any{"type": "text", "text": note}}}, false},
		{"parties []map", []Message{{Role: "user", Content: []map[string]any{{"type": "text", "text": "q"}}}},
			Message{Role: "user", Content: []map[string]any{{"type": "text", "text": "q"}, {"type": "text", "text": note}}}, false},
		{"assistant en dernier", []Message{um("q"), am("a")}, um(lokiNotePrefix + "H"), true},
	}
	for _, c := range cases {
		orig, _ := json.Marshal(c.in)
		out := withTrailingHint(c.in, "H")
		if after, _ := json.Marshal(c.in); string(after) != string(orig) {
			t.Errorf("%s : l'historique d'origine a été modifié", c.name)
		}
		wantLen := len(c.in)
		if c.grow {
			wantLen++
		}
		if len(out) != wantLen {
			t.Fatalf("%s : %d messages, attendu %d", c.name, len(out), wantLen)
		}
		if !reflect.DeepEqual(out[len(out)-1], c.want) {
			t.Errorf("%s : dernier message %+v, attendu %+v", c.name, out[len(out)-1], c.want)
		}
		if !reflect.DeepEqual(out[:len(c.in)-1], c.in[:len(c.in)-1]) {
			t.Errorf("%s : le début du fil a changé", c.name)
		}
	}
}

// Les lecteurs de « la demande de l'utilisateur » sautent les rappels.
func TestRappelsPasPrisPourLaDemande(t *testing.T) {
	msgs := []Message{um("corrige le build"), atc("bash"), tm("ok"), um(budgetNudge(24, 24, 0)), am("fait"),
		um("et les tests"), atc("bash"), tm("ok"), um(thinkNudgeFirst), am("fait")}
	c := &Conversation{Messages: msgs}
	if got := c.lastUserText(); got != "et les tests" {
		t.Errorf("lastUserText = %q", got)
	}
	if got := lastTurnsMessages(msgs, 1); len(got) == 0 || msgText(got[0]) != "et les tests" {
		t.Errorf("dernier échange = %+v", got)
	}
	if got := lastTurnsMessages(msgs, 2); len(got) != len(msgs) {
		t.Errorf("deux échanges : %d messages, attendu %d", len(got), len(msgs))
	}
	if got := convSummary([]Message{um(thinkNudgeFirst), um("bonjour")}); got != "bonjour" {
		t.Errorf("titre = %q", got)
	}
	if tr := renderTranscript([]Message{um(thinkNudgeFirst)}); !strings.HasPrefix(tr, "System: ") {
		t.Errorf("rappel présenté au résumeur comme parole de l'utilisateur : %q", tr)
	}
}

// Compaction après un long tour ponctué de rappels : la VRAIE demande est
// réinjectée, qu'un rappel traîne dans le torse ou dans la queue.
func TestCompactionReinjecteLaVraieDemandePasLeRappel(t *testing.T) {
	testHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 200, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "- pages lues : horaires trouvés pour le matin, reste l'après-midi"}}}})
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	if err := SetConfigKey("PORT", u.Port()); err != nil {
		t.Fatal(err)
	}
	page := func(n int) Message {
		return tm("contenu de page web très long " + string(rune('a'+n)) + strings.Repeat("x", 400))
	}
	torsoNudge, tailNudge := budgetNudge(24, 24, 0), budgetNudge(48, 24, 1)
	msgs := []Message{um("première question"), am("ok"), um("cherche les horaires du train pour Lyon")}
	for i := 0; i < 12; i++ {
		msgs = append(msgs, atc("web_read"), page(i))
		if i == 3 {
			msgs = append(msgs, um(torsoNudge))
		}
	}
	msgs = append(msgs, um(tailNudge), atc("web_read"), page(13))
	_, tailStart := compactBounds(msgs, int(float64(estimateTokens(msgs))*compactTailFrac))
	if msgText(msgs[len(msgs)-3]) != tailNudge || tailStart > len(msgs)-3 {
		t.Fatalf("montage : le rappel de queue n'est pas dans la queue (tailStart=%d)", tailStart)
	}
	out, changed := compactMessages(t.Context(), msgs, Caps{})
	if !changed {
		t.Fatal("pas compacté")
	}
	count := func(s string) int {
		n := 0
		for _, m := range out {
			if m.Role == "user" && msgText(m) == s {
				n++
			}
		}
		return n
	}
	if n := count("cherche les horaires du train pour Lyon"); n != 1 {
		t.Fatalf("demande en cours présente %d fois après compaction, attendu 1", n)
	}
	if count(torsoNudge) != 0 {
		t.Fatal("le rappel du torse a été réinjecté comme demande en cours")
	}
	if count(tailNudge) != 1 {
		t.Fatal("le rappel de la queue a disparu")
	}
	assertAlternance(t, out)
}

// assertAlternance : jamais deux `user` d'affilée (gabarits stricts).
func assertAlternance(t *testing.T, msgs []Message) {
	t.Helper()
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Role == "user" && msgs[i-1].Role == "user" {
			t.Fatalf("deux user d'affilée en %d : %q puis %q", i, msgText(msgs[i-1]), msgText(msgs[i]))
		}
	}
}

// La queue protégée ne commence jamais sur un rappel : la vraie demande,
// réinjectée juste devant elle, lui serait collée (deux user d'affilée).
func TestCompactBoundsPasSurUnRappel(t *testing.T) {
	nudge := um(budgetNudge(24, 24, 0))
	msgs := []Message{um("cherche"), atc("web_read"), tm(strings.Repeat("x", 800)), nudge, atc("web_read"), tm("court")}
	budget := msgTokens(msgs[3]) + msgTokens(msgs[4]) + msgTokens(msgs[5])
	_, tailStart := compactBounds(msgs, budget)
	if tailStart != 1 {
		t.Fatalf("tailStart = %d, attendu 1 (l'appel d'outil avant le rappel)", tailStart)
	}
	// Sans rappel, la frontière reste celle d'avant.
	plain := []Message{um("cherche"), atc("web_read"), tm(strings.Repeat("x", 800)), um("et ensuite"), atc("web_read"), tm("court")}
	if _, ts := compactBounds(plain, msgTokens(plain[3])+msgTokens(plain[4])+msgTokens(plain[5])); ts != 3 {
		t.Fatalf("frontière sans rappel déplacée : %d", ts)
	}
}

// Ce qui est persisté en fin de tour ne garde aucun rappel en porte-à-faux.
func TestDropStrayNudges(t *testing.T) {
	n := um(thinkNudgeFirst)
	cases := []struct {
		name string
		in   []Message
		want []Message
	}{
		{"rappel répondu", []Message{um("q"), atc("glob"), tm("r"), n, am("fini")}, nil},
		{"rappel resté sans réponse (stop)", []Message{um("q"), atc("glob"), tm("r"), n},
			[]Message{um("q"), atc("glob"), tm("r")}},
		{"ajout en cours de réponse juste derrière", []Message{um("q"), atc("glob"), tm("r"), n, um("et aussi"), am("fini")},
			[]Message{um("q"), atc("glob"), tm("r"), um("et aussi"), am("fini")}},
		{"rappel collé derrière la demande", []Message{um("q"), n, am("fini")}, []Message{um("q"), am("fini")}},
		{"correctif sans réponse", []Message{um("q"), am("<tool_call>"), um(retryCorrective("<tool_call>"))},
			[]Message{um("q"), am("<tool_call>")}},
		{"deux rappels de suite en fin", []Message{um("q"), atc("glob"), tm("r"), n, um(thinkNudgeStuck)},
			[]Message{um("q"), atc("glob"), tm("r")}},
		{"vraie demande en fin", []Message{um("q"), am("a"), um("suite")}, nil},
	}
	for _, c := range cases {
		orig, _ := json.Marshal(c.in)
		got := dropStrayNudges(c.in)
		if after, _ := json.Marshal(c.in); string(after) != string(orig) {
			t.Errorf("%s : l'entrée a été modifiée", c.name)
		}
		want := c.want
		if want == nil {
			want = c.in
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s :\n got  %+v\n want %+v", c.name, got, want)
		}
		assertAlternance(t, got)
	}
}

// La réduction forcée coupe aux vraies demandes ; un rappel ne sert de coupe
// qu'à défaut.
func TestShrinkCoupeAuxVraiesDemandes(t *testing.T) {
	testHome(t)
	if err := SetConfigKey("CTX", "4096"); err != nil {
		t.Fatal(err)
	}
	big := am(strings.Repeat("raisonnement long ", 400))
	sys := Message{Role: "system", Content: "SYS"}
	out, changed := shrinkToFit([]Message{sys, um("demande A"), big, um(thinkNudgeFirst), big, um("demande B"), am("ok")}, 0)
	if !changed || len(out) < 2 || msgText(out[1]) != "demande B" {
		t.Fatalf("coupe attendue à « demande B » : %+v", out)
	}
	out, changed = shrinkToFit([]Message{sys, um("demande A"), big, um(thinkNudgeFirst), big}, 0)
	if !changed || len(out) < 2 || msgText(out[1]) != thinkNudgeFirst {
		t.Fatalf("sans autre demande, la coupe doit retomber sur le rappel : %+v", out)
	}
}

// Le rappel de budget part au moteur ET dans l'historique renvoyé : le tour
// suivant rejoue les mêmes octets.
func TestRappelDeBudgetPersisteTelQuEnvoye(t *testing.T) {
	withWorkspace(t)
	if err := SetConfigKey("AGENT_BUDGET", "1"); err != nil {
		t.Fatal(err)
	}
	reqs := scriptedServer(t, sseGlobCall, sseChunk("fini")+sseStop)
	extra, err := runChat(t.Context(), []Message{um("cherche")}, 0.7, Caps{Agent: true}, func(StreamEvent) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	bodies := reqs()
	if len(bodies) != 2 {
		t.Fatalf("%d requêtes, attendu 2", len(bodies))
	}
	sent := reqMessages(t, bodies[1])
	last := sent[len(sent)-1]
	if !strings.HasPrefix(msgText(last), lokiNotePrefix) {
		t.Fatalf("rappel absent de la requête : %+v", last)
	}
	if len(extra) != 3 || !reflect.DeepEqual(extra[2], last) {
		t.Fatalf("historique renvoyé %+v : le rappel doit y être tel qu'envoyé", extra)
	}
}

// « Pensé sans agir » : persisté après un résultat d'outil, éphémère juste
// après le message de l'utilisateur (deux user d'affilée).
func TestRelancePenseSansAgirPersistee(t *testing.T) {
	withWorkspace(t)
	reqs := scriptedServer(t, sseGlobCall, sseStop, sseChunk("fini")+sseStop)
	extra, err := runChat(t.Context(), []Message{um("cherche")}, 0.7, Caps{Agent: true}, func(StreamEvent) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if n := len(reqs()); n != 3 {
		t.Fatalf("%d requêtes, attendu 3", n)
	}
	if len(extra) != 3 || extra[2].Role != "user" || msgText(extra[2]) != thinkNudgeFirst {
		t.Fatalf("relance non persistée après l'outil : %+v", extra)
	}

	reqs = scriptedServer(t, sseStop, sseChunk("fini")+sseStop)
	extra, err = runChat(t.Context(), []Message{um("cherche")}, 0.7, Caps{Agent: true}, func(StreamEvent) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if bodies := reqs(); len(bodies) != 2 || msgText(reqMessages(t, bodies[1])[1]) != thinkNudgeFirst {
		t.Fatal("la relance doit quand même partir au moteur")
	}
	if len(extra) != 0 {
		t.Fatalf("relance persistée derrière la demande (deux user d'affilée) : %+v", extra)
	}
}

// Relance après un 500 (llama-server local) : mêmes outils, même message 0,
// tool_choice none et la consigne au bout du dernier message — qui n'entre
// pas dans l'historique.
func TestRelance500GardeLePrompt(t *testing.T) {
	testHome(t)
	reqs := scriptedServer(t, "500", sseChunk("voici")+sseStop)
	in := []Message{{Role: "system", Content: "SYS"}, um("question")}
	extra, err := runChat(t.Context(), in, 0.7, Caps{Agent: true}, func(StreamEvent) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	bodies := reqs()
	if len(bodies) != 2 {
		t.Fatalf("%d requêtes, attendu 2", len(bodies))
	}
	first, retry := bodies[0], bodies[1]
	if first["tools"] == nil || !reflect.DeepEqual(first["tools"], retry["tools"]) {
		t.Fatal("les outils doivent rester identiques dans la relance")
	}
	if _, ok := first["tool_choice"]; ok {
		t.Fatal("tool_choice posé hors relance")
	}
	if retry["tool_choice"] != "none" {
		t.Fatalf("tool_choice = %v, attendu none", retry["tool_choice"])
	}
	m0, r0 := reqMessages(t, first), reqMessages(t, retry)
	if !reflect.DeepEqual(m0[0], r0[0]) {
		t.Fatalf("message 0 modifié : %+v → %+v", m0[0], r0[0])
	}
	if got := msgText(r0[len(r0)-1]); got != "question\n\n"+lokiNotePrefix+toolsOffHint {
		t.Fatalf("consigne absente du bout du fil : %q", got)
	}
	if len(extra) != 0 {
		t.Fatalf("la consigne ne doit pas être persistée : %+v", extra)
	}
}

// Repli : la relance « none » échoue à son tour (500, ou appel tenté malgré
// tout) → chemin historique, outils retirés et consigne dans le système.
func TestRelance500ReplieSurLeCheminHistorique(t *testing.T) {
	leak := `data: {"choices":[{"delta":{"content":"<tool_call>"}}]}` + "\n\n" + sseStop
	for name, second := range map[string]string{"second 500": "500", "refus 4xx": "400", "appel en texte": leak,
		"appel par le protocole": sseGlobCall, "réponse vide": sseStop} {
		t.Run(name, func(t *testing.T) {
			testHome(t)
			reqs := scriptedServer(t, "500", second, sseChunk("voici")+sseStop)
			in := []Message{{Role: "system", Content: "SYS"}, um("question")}
			ran := false
			if _, err := runChat(t.Context(), in, 0.7, Caps{Agent: true}, func(ev StreamEvent) bool {
				ran = ran || ev.ToolUsed != nil
				return true
			}); err != nil {
				t.Fatal(err)
			}
			if ran {
				t.Fatal("un appel émis sous tool_choice none a été exécuté")
			}
			bodies := reqs()
			if len(bodies) != 3 {
				t.Fatalf("%d requêtes, attendu 3", len(bodies))
			}
			last := bodies[2]
			if _, ok := last["tools"]; ok {
				t.Fatal("le repli doit retirer les outils")
			}
			if _, ok := last["tool_choice"]; ok {
				t.Fatal("tool_choice ne doit pas survivre au repli")
			}
			msgs := reqMessages(t, last)
			if !strings.Contains(msgText(msgs[0]), toolsOffHint) || strings.Contains(msgText(msgs[len(msgs)-1]), toolsOffHint) {
				t.Fatalf("consigne du repli mal placée : %+v", msgs)
			}
		})
	}
}

// Preset externe : la relance garde le chemin historique dès le premier 500.
func TestRelance500ExterneInchangee(t *testing.T) {
	testHome(t)
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		n := len(bodies)
		mu.Unlock()
		if n == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseChunk("voici") + sseStop))
	}))
	t.Cleanup(srv.Close)
	if err := WriteConfig(parseEnv(externalPresetContent(srv.URL, "gpt-4o-mini", "", "", false))); err != nil {
		t.Fatal(err)
	}
	if _, err := runChat(t.Context(), []Message{um("question")}, 0.7, Caps{Agent: true}, func(StreamEvent) bool { return true }); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("%d requêtes, attendu 2", len(bodies))
	}
	if _, ok := bodies[1]["tools"]; ok {
		t.Fatal("preset externe : la relance doit retirer les outils, comme avant")
	}
	if _, ok := bodies[1]["tool_choice"]; ok {
		t.Fatal("preset externe : pas de tool_choice")
	}
}
