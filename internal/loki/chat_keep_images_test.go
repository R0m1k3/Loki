package loki

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// sseSeeImage : le modèle demande see_image sur img.png ; usage du prompt
// prompt (complétion de 20 jetons). prompt = 0 : aucun usage, comme un moteur
// qui ne le renvoie pas.
func sseSeeImage(prompt int) string {
	s := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"v1","type":"function","function":{"name":"see_image","arguments":"{\"file\":\"img.png\"}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"
	if prompt > 0 {
		s += fmt.Sprintf(`data: {"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":20}}`, prompt) + "\n\n"
	}
	return s + "data: [DONE]\n\n"
}

func sseAnswer(text string, prompt int) string {
	s := sseChunk(text) + `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	if prompt > 0 {
		s += fmt.Sprintf(`data: {"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":5}}`, prompt) + "\n\n"
	}
	return s + "data: [DONE]\n\n"
}

// visionServer : un llama-server de test qui déclare la vision (/props) et
// répond aux complétions dans l'ordre des steps, en gardant chaque corps.
func visionServer(t *testing.T, steps ...string) func() []map[string]any {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/props" {
			_, _ = w.Write([]byte(`{"modalities":{"vision":true}}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		n := len(bodies) - 1
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(steps[min(n, len(steps)-1)]))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	for k, v := range map[string]string{"PORT": u.Port(), "MMPROJ": "/models/mmproj.gguf"} {
		if err := SetConfigKey(k, v); err != nil {
			t.Fatal(err)
		}
	}
	// Sonde de vision en cache 10 s : un autre test a pu conclure « non ».
	visionProbeMu.Lock()
	visionProbeAt = time.Time{}
	visionProbeMu.Unlock()
	return func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), bodies...)
	}
}

// writeTestPNG pose img.png (petit, sous maxImageDim : octets inchangés) dans
// le dossier de travail et renvoie la data-URL que le modèle doit recevoir.
func writeTestPNG(t *testing.T, dir string) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := 0; i < 8; i++ {
		img.Set(i, i, color.RGBA{R: 200, A: 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "img.png"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	part := imageURLPart(buf.Bytes(), "image/png")
	return part["image_url"].(map[string]any)["url"].(string)
}

func mainCtx(t *testing.T) context.Context {
	return withPerf(t.Context(), perfMain, "conv-test")
}

// relayOf : le message de relais d'une requête (celui qui porte l'image).
func relayOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	for _, m := range body["messages"].([]any) {
		mm := m.(map[string]any)
		if parts, ok := mm["content"].([]any); ok && len(parts) == 2 {
			return mm
		}
	}
	t.Fatal("aucun relais d'image dans la requête")
	return nil
}

// Sans la clé : image éphémère, relais en base64, aucune marque — et, avec la
// clé, la requête qui montre l'image est la MÊME à l'octet près.
func TestKeepImagesSansCleRequeteIdentique(t *testing.T) {
	dir := withWorkspace(t)
	want := writeTestPNG(t, dir)

	reqs := visionServer(t, sseSeeImage(1000), sseAnswer("vu", 1331))
	extra, err := runChat(mainCtx(t), []Message{um("regarde img.png")}, 0.7, Caps{Agent: true}, func(StreamEvent) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	off := reqs()
	if len(off) != 2 {
		t.Fatalf("%d requêtes, attendu 2", len(off))
	}
	relay := relayOf(t, off[1])
	if _, tagged := relay["img_relay"]; tagged {
		t.Fatal("marque img_relay envoyée sans la clé")
	}
	part := relay["content"].([]any)[1].(map[string]any)
	if got := part["image_url"].(map[string]any)["url"]; got != want {
		t.Fatal("image du relais différente de celle du fichier")
	}
	if len(extra) != 2 {
		t.Fatalf("sans la clé, l'image doit rester éphémère : %+v", extra)
	}
	for _, m := range extra {
		if m.ImgRelay || m.imgTokens != 0 {
			t.Fatalf("message marqué sans la clé : %+v", m)
		}
	}

	if err := SetConfigKey("KEEP_TURN_IMAGES", "on"); err != nil {
		t.Fatal(err)
	}
	reqs = visionServer(t, sseSeeImage(1000), sseAnswer("vu", 1331))
	if _, err := runChat(mainCtx(t), []Message{um("regarde img.png")}, 0.7, Caps{Agent: true}, func(StreamEvent) bool { return true }); err != nil {
		t.Fatal(err)
	}
	on := reqs()
	if len(on) != 2 {
		t.Fatalf("%d requêtes avec la clé, attendu 2", len(on))
	}
	for i := range on {
		if !reflect.DeepEqual(on[i], off[i]) {
			a, _ := json.Marshal(off[i])
			b, _ := json.Marshal(on[i])
			t.Fatalf("requête %d différente avec la clé :\n%s\n%s", i, a, b)
		}
	}
}

// Avec la clé : l'image est gardée par référence, son coût est celui que le
// moteur a mesuré (écart de prompt moins le texte ajouté), et le tour suivant
// renvoie exactement les mêmes octets.
func TestKeepImagesGardeeEtMesuree(t *testing.T) {
	dir := withWorkspace(t)
	want := writeTestPNG(t, dir)
	if err := SetConfigKey("KEEP_TURN_IMAGES", "on"); err != nil {
		t.Fatal(err)
	}
	toolMsg := Message{Role: "tool", ToolCallID: "v1", Content: "[ok] image chargée : img.png"}
	// ctxAfter de la 1re complétion = 1000 + 20 ; + résultat d'outil ; + 300
	// pour l'image.
	prompt2 := 1020 + msgTokens(toolMsg) + 300
	visionServer(t, sseSeeImage(1000), sseAnswer("vu", prompt2))
	extra, err := runChat(mainCtx(t), []Message{um("regarde img.png")}, 0.7, Caps{Agent: true}, func(StreamEvent) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if len(extra) != 3 {
		t.Fatalf("image non gardée : %+v", extra)
	}
	r := extra[2]
	if !r.ImgRelay || r.imgTokens != 300 {
		t.Fatalf("relais gardé = %+v, attendu marqué et 300 jetons", r)
	}
	_, u := partImageURL(contentParts(r.Content)[1])
	if !strings.HasPrefix(u, imgRefScheme) {
		t.Fatalf("image gardée en base64 et non par référence : %.60s", u)
	}
	if !isLokiInjected(r) {
		t.Fatal("relais pris pour une demande de l'utilisateur")
	}

	// Tour suivant : l'historique renvoyé repart avec la même image, sans la
	// marque, à l'octet près de ce que le moteur a déjà vu.
	hist := append([]Message{um("regarde img.png")}, extra...)
	hist = append(hist, am("vu"), um("et maintenant ?"))
	reqs := visionServer(t, sseAnswer("ok", 0))
	if _, err := runChat(mainCtx(t), hist, 0.7, Caps{Agent: true}, func(StreamEvent) bool { return true }); err != nil {
		t.Fatal(err)
	}
	next := reqs()[0]
	relay := relayOf(t, next)
	if _, tagged := relay["img_relay"]; tagged {
		t.Fatal("marque img_relay envoyée au moteur")
	}
	if got := relay["content"].([]any)[1].(map[string]any)["image_url"].(map[string]any)["url"]; got != want {
		t.Fatal("le tour suivant ne renvoie pas les mêmes octets d'image")
	}
}

// Garde : au-delà de 10 % de la fenêtre, l'image reste éphémère.
func TestKeepImagesBudget(t *testing.T) {
	dir := withWorkspace(t)
	writeTestPNG(t, dir)
	for k, v := range map[string]string{"KEEP_TURN_IMAGES": "on", "CTX": "2000"} {
		if err := SetConfigKey(k, v); err != nil {
			t.Fatal(err)
		}
	}
	toolMsg := Message{Role: "tool", ToolCallID: "v1", Content: "[ok] image chargée : img.png"}
	visionServer(t, sseSeeImage(1000), sseAnswer("vu", 1020+msgTokens(toolMsg)+300))
	extra, err := runChat(mainCtx(t), []Message{um("regarde img.png")}, 0.7, Caps{Agent: true}, func(StreamEvent) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if len(extra) != 2 {
		t.Fatalf("image de 300 jetons gardée malgré un budget de 200 : %+v", extra)
	}
}

// Moteur sans usage : rien à mesurer, l'image reste éphémère.
func TestKeepImagesSansMesure(t *testing.T) {
	dir := withWorkspace(t)
	writeTestPNG(t, dir)
	if err := SetConfigKey("KEEP_TURN_IMAGES", "on"); err != nil {
		t.Fatal(err)
	}
	visionServer(t, sseSeeImage(0), sseAnswer("vu", 0))
	extra, err := runChat(mainCtx(t), []Message{um("regarde img.png")}, 0.7, Caps{Agent: true}, func(StreamEvent) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if len(extra) != 2 {
		t.Fatalf("image gardée sans mesure : %+v", extra)
	}
}

// Ni tâche ni sous-agent : seul le fil d'une discussion garde ses images.
func TestKeepImagesFilPrincipalSeulement(t *testing.T) {
	testHome(t)
	if err := SetConfigKey("KEEP_TURN_IMAGES", "on"); err != nil {
		t.Fatal(err)
	}
	if newKeepImages(ReadConfig(), perfTag{kind: perfTask, conv: "task:x"}) != nil ||
		newKeepImages(ReadConfig(), perfTag{kind: perfMain}) != nil {
		t.Fatal("état de KEEP_TURN_IMAGES hors d'une discussion")
	}
	var k *keepImages // clé absente : chaque méthode rend le comportement d'avant
	m := []Message{um("a")}
	got, e := k.relay(m, nil, um("img"), 1)
	if len(got) != 2 || e != nil || got[1].ImgRelay {
		t.Fatal("relais sans état modifié")
	}
	k.sending(2)
	k.rewritten()
	if k.observe(true, StatsEvent{}, got, nil) != nil || k.finish(nil) != nil ||
		len(k.publishable(got)) != 2 {
		t.Fatal("état nil qui agit")
	}
}

func keptRelay(tokens int) Message {
	return Message{Role: "user", ImgRelay: true, imgTokens: tokens, Content: []map[string]any{
		{"type": "text", "text": "Voici la capture demandée."},
		{"type": "image_url", "image_url": map[string]any{"url": imgRefScheme + "abc.png"}},
	}}
}

// Retrait : légende + imageLostMarker, toujours un relais, coût libéré ; rien
// à faire sans image gardée.
func TestDropKeptImages(t *testing.T) {
	plain := []Message{um("q"), tm("r")}
	if out, freed := dropKeptImages(plain); freed != 0 || &out[0] != &plain[0] {
		t.Fatal("copie ou retrait sans image gardée")
	}
	msgs := []Message{um("q"), atc("see_image"), tm("[ok]"), keptRelay(500), am("vu")}
	out, freed := dropKeptImages(msgs)
	if freed != 500 {
		t.Fatalf("libéré %d, attendu 500", freed)
	}
	r := out[3]
	if r.Content != "Voici la capture demandée."+imageLostMarker || !r.ImgRelay || r.imgTokens != 0 {
		t.Fatalf("relais retiré = %+v", r)
	}
	if msgs[3].imgTokens != 500 {
		t.Fatal("l'historique d'origine a été modifié en place")
	}
	if estimateTokens(msgs)-estimateTokens(out) < 400 {
		t.Fatal("le coût mesuré de l'image n'est pas compté dans l'estimation")
	}
}

// Toute compaction retire les images gardées d'abord, et ce retrait compte
// comme un changement même quand rien n'est à résumer.
func TestCompactionRetireLesImagesDAbord(t *testing.T) {
	testHome(t)
	msgs := []Message{{Role: "system", Content: "sys"}, um("q"), atc("see_image"), tm("[ok]"), keptRelay(5000), am("vu")}
	out, changed, _ := compactMessagesOpt(t.Context(), msgs, Caps{}, compactOpts{})
	if !changed || hasKeptImages(out) {
		t.Fatalf("images gardées non retirées par la compaction : changé=%v", changed)
	}
	out, changed = shrinkToFit(msgs, 0)
	if !changed || hasKeptImages(out) {
		t.Fatal("images gardées non retirées par la réduction forcée")
	}
	// Sans image gardée : chemin d'avant, rien de changé sur un torse vide.
	plain := []Message{{Role: "system", Content: "sys"}, um("q"), am("r")}
	if _, changed, _ := compactMessagesOpt(t.Context(), plain, Caps{}, compactOpts{}); changed {
		t.Fatal("compaction d'un fil sans torse")
	}
}

// Un relais n'est ni une demande à réinjecter, ni la preuve qu'une demande a
// été servie ; rouvert, il le reste (marque persistée), et la marque ne part
// jamais au moteur.
func TestRelaisPasPrisPourLaDemande(t *testing.T) {
	r := keptRelay(10)
	msgs := []Message{um("ancienne"), am("ok"), um("vraie demande"), atc("see_image"), tm("[ok]"), r}
	if p := compactPending(msgs, msgs[:3], 3); len(p) != 1 || msgText(p[0]) != "vraie demande" {
		t.Fatalf("demande réinjectée : %+v", p)
	}
	raw, _ := json.Marshal(r)
	var back Message
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	back = stripImageParts([]Message{back})[0]
	if !isLokiInjected(back) {
		t.Fatal("relais rouvert pris pour une demande")
	}
	sent, _ := wireMessages(msgs, false, echoPolicy{})
	for _, m := range sent {
		if m.ImgRelay {
			t.Fatal("marque de relais dans la requête")
		}
	}
	if !msgs[5].ImgRelay {
		t.Fatal("l'historique a perdu sa marque à l'envoi")
	}
}

// Réécriture en cours de tour : une image non gardée n'entre pas dans
// l'historique publié ; une image gardée, ou déjà retirée, si.
func TestKeepImagesPublication(t *testing.T) {
	k := &keepImages{}
	eph := Message{Role: "user", ImgRelay: true, Content: []map[string]any{
		{"type": "text", "text": "Image :"}, {"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA=="}}}}
	stub := Message{Role: "user", ImgRelay: true, Content: "Image :" + imageLostMarker}
	msgs := []Message{um("q"), tm("a"), eph, tm("b"), keptRelay(50), stub}
	got := k.publishable(msgs)
	if len(got) != 5 || got[2].Content != "b" {
		t.Fatalf("publié : %+v", got)
	}
	k.pend = []keepPend{{msgIdx: 2, extraIdx: 0, weight: 1}}
	k.rewritten()
	if !k.stopped || k.pend != nil {
		t.Fatal("images à l'essai encore suivies après une réécriture")
	}
}

// Début de tour : clé retirée → images gardées retirées ; clé présente et
// vision active sans besoin de compacter → gardées.
func TestKeptImagesDebutDeTour(t *testing.T) {
	withWorkspace(t)
	visionServer(t, sseStop)
	c := newTestConv()
	msgs := []Message{um("q"), atc("see_image"), tm("[ok]"), keptRelay(300), am("vu"), um("suite")}
	c.Messages = append([]Message(nil), msgs...)
	c.CtxUsed = 2000
	out, used := c.keptImagesTurnStart(c.epoch, msgs, 2000, 0)
	if hasKeptImages(out) || used != 1700 || hasKeptImages(c.Messages) || c.CtxUsed != 1700 {
		t.Fatalf("clé absente : images gardées encore là (used=%d)", used)
	}
	if err := SetConfigKey("KEEP_TURN_IMAGES", "on"); err != nil {
		t.Fatal(err)
	}
	c.Messages = append([]Message(nil), msgs...)
	out, used = c.keptImagesTurnStart(c.epoch, msgs, 2000, 0)
	if !hasKeptImages(out) || used != 2000 {
		t.Fatal("images retirées alors que rien ne le demandait")
	}
	// Contexte au seuil : retirées d'abord.
	out, used = c.keptImagesTurnStart(c.epoch, msgs, ctxWindow(), 0)
	if hasKeptImages(out) || used != ctxWindow()-300 {
		t.Fatal("images gardées malgré un contexte au seuil")
	}
}

// NUDGE_IN_TOOL : seulement avec la clé, sur le moteur local, si la sonde a dit
// « instable » pour CE modèle.
func TestNudgeInToolOn(t *testing.T) {
	freshTplProbe(t)
	cfg := map[string]string{"MODEL": "/models/qwen35.gguf"}
	set := func(r tplProbeResult) {
		tplProbeMu.Lock()
		tplProbeLast = &r
		tplProbeMu.Unlock()
	}
	set(tplProbeResult{Model: "qwen35.gguf", PrefixStable: tplNo})
	if nudgeInToolOn(cfg, chatEndpoint{}) {
		t.Fatal("actif sans la clé")
	}
	cfg["NUDGE_IN_TOOL"] = "on"
	if !nudgeInToolOn(cfg, chatEndpoint{}) {
		t.Fatal("inactif avec la clé sur un gabarit instable")
	}
	if nudgeInToolOn(cfg, chatEndpoint{External: true}) {
		t.Fatal("actif sur un preset externe")
	}
	for _, r := range []tplProbeResult{
		{Model: "qwen35.gguf", PrefixStable: tplUnknown},
		{Model: "qwen35.gguf", PrefixStable: tplYes},
		{Model: "autre.gguf", PrefixStable: tplNo},
	} {
		set(r)
		if nudgeInToolOn(cfg, chatEndpoint{}) {
			t.Fatalf("actif pour %+v", r)
		}
	}
}

func TestNudgeIntoTool(t *testing.T) {
	msgs := []Message{um("q"), atc("glob"), tm("res")}
	extra := []Message{atc("glob"), tm("res")}
	m2, e2, ok := nudgeIntoTool(msgs, extra, "[system] stop")
	if !ok || m2[2].Content != "res\n\n[system] stop" || !reflect.DeepEqual(m2[2], e2[1]) {
		t.Fatalf("rappel mal posé : %+v / %+v", m2, e2)
	}
	if msgs[2].Content != "res" || extra[1].Content != "res" {
		t.Fatal("messages modifiés en place")
	}
	// Dernier message qui n'est pas un résultat d'outil, ou historique réécrit.
	if _, _, ok := nudgeIntoTool(append(msgs, um("img")), extra, "x"); ok {
		t.Fatal("rappel posé derrière un message user")
	}
	if _, _, ok := nudgeIntoTool(msgs, nil, "x"); ok {
		t.Fatal("rappel posé sans le résultat dans l'historique")
	}
}

// Bout à bout : avec la clé et un gabarit instable, le rappel part au bout du
// résultat d'outil, persisté tel qu'envoyé, sans message user à part.
func TestRappelDeBudgetDansLeResultat(t *testing.T) {
	withWorkspace(t)
	freshTplProbe(t)
	for k, v := range map[string]string{"AGENT_BUDGET": "1", "NUDGE_IN_TOOL": "on", "MODEL": "/models/qwen35.gguf"} {
		if err := SetConfigKey(k, v); err != nil {
			t.Fatal(err)
		}
	}
	tplProbeMu.Lock()
	tplProbeLast = &tplProbeResult{Model: "qwen35.gguf", PrefixStable: tplNo}
	tplProbeMu.Unlock()
	reqs := scriptedServer(t, sseGlobCall, sseChunk("fini")+sseStop)
	extra, err := runChat(t.Context(), []Message{um("cherche")}, 0.7, Caps{Agent: true}, func(StreamEvent) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	sent := reqMessages(t, reqs()[1])
	last := sent[len(sent)-1]
	if last.Role != "tool" || !strings.HasSuffix(msgText(last), "unless exactly one specific call is genuinely still missing.") {
		t.Fatalf("rappel absent du résultat d'outil : %+v", last)
	}
	if len(extra) != 2 || !reflect.DeepEqual(extra[1], last) {
		t.Fatalf("historique %+v : le résultat doit y être tel qu'envoyé", extra)
	}
}

// En cours de tour, au seuil : les images gardées partent d'abord, et le
// résumé n'est pas demandé si leur retrait suffit.
func TestKeepImagesRetireesEnCoursDeTour(t *testing.T) {
	withWorkspace(t)
	for k, v := range map[string]string{"KEEP_TURN_IMAGES": "on", "CTX": "2000"} {
		if err := SetConfigKey(k, v); err != nil {
			t.Fatal(err)
		}
	}
	glob := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"g1","type":"function","function":{"name":"glob","arguments":"{\"pattern\":\"*.go\"}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":1600,"completion_tokens":10}}` + "\n\ndata: [DONE]\n\n"
	reqs := visionServer(t, glob, sseAnswer("fini", 0))
	hist := []Message{um("q"), atc("see_image"), tm("[ok]"), keptRelay(1000), am("vu"), um("suite")}
	var published []Message
	extra, err := runChat(mainCtx(t), hist, 0.7, Caps{Agent: true}, func(ev StreamEvent) bool {
		if ev.NewHistory != nil {
			published = ev.NewHistory
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(reqs()); n != 2 {
		t.Fatalf("%d requêtes : un résumé est parti alors que retirer l'image suffisait", n)
	}
	if published == nil || hasKeptImages(published) || len(published) != len(hist)+2 {
		t.Fatalf("historique publié : %+v", published)
	}
	if published[3].Content != "Voici la capture demandée."+imageLostMarker {
		t.Fatalf("relais publié : %+v", published[3])
	}
	if relay := reqMessages(t, reqs()[1])[3]; msgText(relay) != "Voici la capture demandée."+imageLostMarker {
		t.Fatalf("image encore envoyée après son retrait : %+v", relay)
	}
	if len(extra) != 0 {
		t.Fatalf("extra après publication : %+v", extra)
	}
}

// Budget déjà plein : l'image suivante reste éphémère sans même être rangée.
func TestKeepImagesBudgetPleinAvantRangement(t *testing.T) {
	testHome(t)
	k := &keepImages{}
	msgs := []Message{um("q"), keptRelay(keepImgBudget())}
	img := seeImageMessage("x.png", imageURLPart([]byte("png"), "image/png"))
	got, extra := k.relay(msgs, nil, img, 1)
	if len(got) != 3 || len(extra) != 0 || !k.stopped || len(k.pend) != 0 || !got[2].ImgRelay {
		t.Fatalf("relais au budget plein : %+v / %+v", got, k)
	}
	if ents, _ := os.ReadDir(chatImgDir()); len(ents) != 0 {
		t.Fatal("image rangée alors que le budget était plein")
	}
}

// Corrections de relecture du lot 2 : prompt refusé pour débordement alors que
// des images sont gardées — elles partent d'abord et la requête est rejouée
// sans elles, sans résumé (avec perte) tant que ça suffit.
func TestKeepImagesRetireesAvantLeFiletReactif(t *testing.T) {
	withWorkspace(t)
	for k, v := range map[string]string{"KEEP_TURN_IMAGES": "on", "CTX": "2000"} {
		if err := SetConfigKey(k, v); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/props" {
			_, _ = w.Write([]byte(`{"modalities":{"vision":true}}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		n := len(bodies)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":400,"message":"request (2500 tokens) exceeds the available context size (2000 tokens), try increasing it"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseAnswer("fini", 0)))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	for k, v := range map[string]string{"PORT": u.Port(), "MMPROJ": "/models/mmproj.gguf"} {
		if err := SetConfigKey(k, v); err != nil {
			t.Fatal(err)
		}
	}
	visionProbeMu.Lock()
	visionProbeAt = time.Time{}
	visionProbeMu.Unlock()
	hist := []Message{um("q"), atc("see_image"), tm("[ok]"), keptRelay(1000), am("vu"), um("suite")}
	var published []Message
	if _, err := runChat(mainCtx(t), hist, 0.7, Caps{Agent: true}, func(ev StreamEvent) bool {
		if ev.NewHistory != nil {
			published = ev.NewHistory
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("%d requêtes : un résumé est parti alors que retirer l'image suffisait", len(bodies))
	}
	if published == nil || hasKeptImages(published) || len(published) != len(hist) {
		t.Fatalf("historique publié : %+v", published)
	}
	if relay := reqMessages(t, bodies[1])[3]; msgText(relay) != "Voici la capture demandée."+imageLostMarker {
		t.Fatalf("image encore envoyée après son retrait : %+v", relay)
	}
}
