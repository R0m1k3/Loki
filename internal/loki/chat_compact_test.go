package loki

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// helpers pour construire des historiques de test lisibles.
func um(s string) Message { return Message{Role: "user", Content: s} }
func am(s string) Message { return Message{Role: "assistant", Content: s} }
func atc(name string) Message {
	return Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Function: ToolCallFunc{Name: name, Arguments: "{}"}}}}
}
func tm(s string) Message { return Message{Role: "tool", ToolCallID: "c1", Content: s} }

func TestCompactBoundsProtectsHead(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "sys"},
		um("premier"), am("r1"),
		um("q2"), am("r2"),
		um("q3"), am("r3"),
	}
	// budget minuscule → queue = juste le dernier tour, tête = les seuls systèmes.
	// Le 1er message user N'EST PLUS épinglé : il l'était pour « ancrer
	// l'objectif », mais après compaction le modèle voyait deux demandes (celle du
	// début, épinglée, et la courante réinjectée par `pending`) et répondait
	// volontiers à l'ancienne. Voir compactBounds et TestCompactKeepsPendingRequest.
	head, tail := compactBounds(msgs, 1)
	if head != 1 { // le message système, et lui seul
		t.Fatalf("head = %d, attendu 1 (systèmes uniquement)", head)
	}
	if msgs[head].Role != "user" {
		t.Fatalf("le 1er message user doit être compactable, il commence le torse ; rôle obtenu %q", msgs[head].Role)
	}
	// Frontière sûre = user ou assistant (jamais un `tool`, qui serait orphelin).
	if r := msgs[tail].Role; r != "user" && r != "assistant" {
		t.Fatalf("la queue doit démarrer sur un user ou un assistant, obtenu %q", r)
	}
	if tail <= head {
		t.Fatalf("torse vide (tail=%d head=%d) alors qu'il y a du milieu à compacter", tail, head)
	}
}

// La queue ne doit jamais démarrer entre un assistant+tool_calls et ses
// résultats `tool` : elle recule jusqu'à la frontière sûre précédente (user ou
// assistant), donc l'assistant part dans la queue AVEC ses résultats.
func TestCompactBoundsKeepsToolPairs(t *testing.T) {
	msgs := []Message{
		um("q1"), am("r1"),
		um("q2"), atc("bash"), tm("sortie longue"), am("r2"),
		um("q3"), am("r3"),
	}
	// budget moyen qui, sans le recul, couperait au milieu du tour outillé.
	_, tail := compactBounds(msgs, msgTokens(msgs[6])+msgTokens(msgs[7])+msgTokens(msgs[5])+1)
	if r := msgs[tail].Role; r != "user" && r != "assistant" {
		t.Fatalf("la queue démarre sur %q : frontière non sûre (orphelin tool possible)", r)
	}
	// Vérifie qu'aucun message `tool` de la queue n'a perdu son assistant parent.
	for i := tail; i < len(msgs); i++ {
		if msgs[i].Role == "tool" {
			if i == tail || (msgs[i-1].Role != "assistant" && msgs[i-1].Role != "tool") {
				t.Fatalf("message tool orphelin à l'index %d de la queue", i)
			}
		}
	}
}

// Régression : DEUXIÈME compaction à l'intérieur d'un même tour. Une longue
// boucle d'outils (recherche web : dix pages lues d'affilée) ne contient AUCUN
// message `user` — reculer jusqu'à un `user` faisait donc avaler toute la
// séquence par la queue, torse vide, compaction sans effet. La queue doit
// pouvoir démarrer sur un `assistant`, et les gros résultats d'outils du début
// doivent se retrouver dans le torse (donc résumés).
func TestCompactBoundsSplitsLongToolLoop(t *testing.T) {
	page := func(n int) Message {
		return tm("contenu de page web très long " + string(rune('a'+n)) + strings.Repeat("x", 400))
	}
	msgs := []Message{
		um("premier"), am("ok"), // head : 1er user protégé
		um("cherche des trucs"), // la dernière demande utilisateur du tour
	}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, atc("web_read"), page(i))
	}
	head, tail := compactBounds(msgs, estimateTokens(msgs)/4)
	if tail <= head {
		t.Fatalf("torse vide (tail=%d head=%d) : la boucle d'outils n'est pas compactable", tail, head)
	}
	if r := msgs[tail].Role; r != "user" && r != "assistant" {
		t.Fatalf("la queue démarre sur %q : frontière non sûre", r)
	}
	// Aucun `tool` orphelin en queue.
	for i := tail; i < len(msgs); i++ {
		if msgs[i].Role == "tool" && (i == tail || (msgs[i-1].Role != "assistant" && msgs[i-1].Role != "tool")) {
			t.Fatalf("message tool orphelin à l'index %d de la queue", i)
		}
	}
	// Le torse doit bien contenir des résultats d'outils (c'est ce qui remplit
	// la fenêtre) — sinon la compaction ne libérerait rien.
	tools := 0
	for _, m := range msgs[head:tail] {
		if m.Role == "tool" {
			tools++
		}
	}
	if tools == 0 {
		t.Fatal("aucun résultat d'outil dans le torse : rien à gagner à compacter")
	}
}

// Régression : après compaction pendant une recherche web, la DEMANDE EN COURS
// doit encore figurer telle quelle dans l'historique. Sans ça, le seul message
// `user` restant était le tout premier de la conversation (épinglé en tête) et
// le modèle répondait à celui-là au lieu de continuer la recherche.
func TestCompactKeepsPendingRequest(t *testing.T) {
	page := func(n int) Message {
		return tm("contenu de page web très long " + string(rune('a'+n)) + strings.Repeat("x", 400))
	}
	msgs := []Message{
		um("première question de la conversation"), am("ok"),
		um("cherche les horaires du train pour Lyon"),
	}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, atc("web_read"), page(i))
	}
	// summarizeTranscript échoue (pas de llama-server en test) → torse dégraissé,
	// ce qui n'enlève rien à ce qu'on vérifie ici : la demande doit survivre.
	out, _ := compactMessages(t.Context(), msgs, Caps{})
	found := false
	for _, m := range out {
		if m.Role == "user" && strings.Contains(msgText(m), "horaires du train") {
			found = true
		}
	}
	if !found {
		t.Fatal("la demande en cours a disparu de l'historique compacté")
	}
}

// Régression : ce qu'on envoie au RÉSUMEUR doit encore contenir les résultats
// d'outils. On effaçait les résultats (dégraissage) AVANT de résumer : le
// résumeur ne voyait que des marqueurs, le résumé ne pouvait donc porter aucune
// information trouvée, et l'IA relançait la même recherche après chaque
// compactage — sans jamais s'arrêter.
func TestSummaryInputKeepsToolFindings(t *testing.T) {
	fait := "le train de 14h12 part quai 3"
	torso := []Message{
		atc("web_read"),
		tm(fait + strings.Repeat(" blabla de remplissage", 300)),
	}
	// Même transformation que compactMessages avant l'appel au résumeur.
	forSummary := make([]Message, len(torso))
	for i, m := range torso {
		forSummary[i] = m
		if m.Role == "tool" {
			if r := []rune(msgText(m)); len(r) > compactToolSummaryLen {
				forSummary[i].Content = string(r[:compactToolSummaryLen]) + "\n[…suite coupée]"
			}
		}
	}
	tr := renderTranscript(forSummary)
	if !strings.Contains(tr, fait) {
		t.Fatal("le fait trouvé n'atteint pas le résumeur : le résumé sera vide d'information")
	}
	if strings.Contains(tr, compactPrunedMarker) {
		t.Fatal("le résumeur reçoit des marqueurs d'effacement au lieu du contenu")
	}
	if len([]rune(tr)) > 4000 {
		t.Fatalf("transcription non bornée (%d runes) : le résumeur va déborder", len([]rune(tr)))
	}
}

func TestEstimateTokensGrows(t *testing.T) {
	small := estimateTokens([]Message{um("court")})
	big := estimateTokens([]Message{um("un message nettement plus long que le précédent pour dépasser")})
	if big <= small {
		t.Fatalf("estimateTokens ne croît pas avec la taille: small=%d big=%d", small, big)
	}
}

// Repli sans résumé (moteur absent) : la demande en cours est déjà dans le
// torse dégraissé, elle ne doit pas être réinjectée une seconde fois — elle
// apparaissait APRÈS ses propres résultats d'outils (AJEAN 0.17.4).
func TestCompactSansResumeNeDoublePasLaDemande(t *testing.T) {
	testHome(t)
	page := func(n int) Message {
		return tm("contenu de page web très long " + string(rune('a'+n)) + strings.Repeat("x", 400))
	}
	msgs := []Message{
		um("première question de la conversation"), am("ok"),
		um("cherche les horaires du train pour Lyon"),
	}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, atc("web_read"), page(i))
	}
	out, _ := compactMessages(t.Context(), msgs, Caps{})
	n := 0
	for _, m := range out {
		if m.Role == "user" && strings.Contains(msgText(m), "horaires du train") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("demande en cours présente %d fois après compactage, attendu 1", n)
	}
}

// Mode Code : le résumé demande les fichiers touchés et les erreurs, et l'état
// des critères est rendu au builder après compactage.
func TestCompactageModeCodeGardeLEtat(t *testing.T) {
	withWorkspace(t)
	toolCriteria(map[string]any{"action": "add", "texts": []any{"les tests passent"}}, false)
	var sys string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		sys = msgText(body.Messages[0])
		sendJSON(w, 200, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "- main.go modifié : la fonction parse corrigée, go build et go test passent"}}}})
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	if err := SetConfigKey("PORT", u.Port()); err != nil {
		t.Fatal(err)
	}
	page := func(n int) Message {
		return tm("sortie de commande " + string(rune('a'+n)) + strings.Repeat("x", 400))
	}
	msgs := []Message{um("corrige le build"), am("ok"), um("continue")}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, atc("bash"), page(i))
	}
	out, changed := compactMessages(t.Context(), msgs, Caps{Agent: true, Code: true})
	if !changed {
		t.Fatal("pas compacté")
	}
	if !strings.Contains(sys, "CODING session") {
		t.Fatal("consigne de résumé du mode Code absente")
	}
	found := false
	for _, m := range out {
		if strings.Contains(msgText(m), "Acceptance criteria (current state)") && strings.Contains(msgText(m), "les tests passent") {
			found = true
		}
	}
	if !found {
		t.Fatal("critères non rendus après compactage")
	}
}

// Le résumé n'est plus coupé à 2200 caractères : max_tokens borne déjà la
// sortie, et l'ancienne coupe faisait tomber l'ÉTAT D'AVANCEMENT, écrit en
// dernier. Seul le raisonnement sans résumé est refusé (repli sur le torse).
func TestCleanSummary(t *testing.T) {
	testHome(t)
	long := strings.Repeat("é", 7000) + " ÉTAT D'AVANCEMENT"
	huge := strings.Repeat("x", summaryRuneCap()+500)
	cases := []struct {
		name, content, reasoning, finish string
		want                             string
		err                              bool
	}{
		{"simple", "  - fait A\n- fait B ", "", "stop", "- fait A\n- fait B", false},
		{"long gardé entier", long, "", "stop", long, false},
		{"think retiré", "<think>je réfléchis</think>\n- résumé", "", "stop", "- résumé", false},
		{"length gardé et marqué", "- début du résumé", "", "length", "- début du résumé […]", false},
		{"au-delà du filet", huge, "", "stop", strings.Repeat("x", summaryRuneCap()) + " […]", false},
		{"balise citée en milieu", "- le parseur retire <think> en tête", "", "stop", "- le parseur retire <think> en tête", false},
		{"think jamais refermé", "<think>je réfléchis encore", "", "length", "", true},
		{"que du reasoning_content", "", "je réfléchis", "length", "", true},
		{"think vide de réponse", "<think>x</think>  ", "", "stop", "", true},
		{"vide", "", "", "stop", "", true},
	}
	for _, c := range cases {
		got, err := cleanSummary(c.content, c.reasoning, c.finish)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("%s : %q, %v", c.name, got, err)
		}
	}
}

// Bout à bout : un résumé de 7000 caractères revient intact du moteur, et une
// réponse qui n'a que du raisonnement est une erreur (repli de l'appelant).
func TestSummarizeLongNotTruncated(t *testing.T) {
	testHome(t)
	long := strings.Repeat("ü", 7000)
	var reply map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	if err := SetConfigKey("PORT", u.Port()); err != nil {
		t.Fatal(err)
	}
	reply = map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": long}, "finish_reason": "stop"}}}
	got, err := summarizeTranscriptFor(context.Background(), "transcript", false)
	if err != nil || got != long {
		t.Fatalf("résumé tronqué : %d caractères, %v", len([]rune(got)), err)
	}
	reply = map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "", "reasoning_content": "hmm"}, "finish_reason": "length"}}}
	if got, err := summarizeTranscriptFor(context.Background(), "transcript", false); err == nil {
		t.Fatalf("raisonnement seul accepté comme résumé : %q", got)
	}
}
