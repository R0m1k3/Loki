package loki

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// videoEngine simule un moteur qui sait (ou non) lire la vidéo.
func videoEngine(t *testing.T, ok bool) {
	t.Helper()
	prev := videoInputSupported
	videoInputSupported = func() bool { return ok }
	t.Cleanup(func() { videoInputSupported = prev })
}

// engineSees amorce le cache de la sonde /props : aucun llama-server ne tourne
// pendant les tests.
func engineSees(t *testing.T, seen bool) {
	t.Helper()
	visionProbeMu.Lock()
	visionProbeAt, visionProbeSeen = time.Now(), seen
	visionProbeMu.Unlock()
	t.Cleanup(func() { visionProbeMu.Lock(); visionProbeAt = time.Time{}; visionProbeMu.Unlock() })
}

// Une vidéo en base64 part sur disque et revient identique à l'envoi au modèle ;
// l'historique ne garde qu'une référence de quelques octets.
func TestVideoRefsRoundTrip(t *testing.T) {
	testHome(t)
	raw := []byte(strings.Repeat("\x00\x00\x00\x18ftypmp42-fake-video-bytes", 5000))
	url := "data:video/mp4;base64," + base64.StdEncoding.EncodeToString(raw)
	msgs := []Message{
		{Role: "user", Content: []map[string]any{
			{"type": "text", "text": "regarde la vidéo"},
			{"type": "input_video", "input_video": map[string]any{"url": url}},
		}},
		{Role: "assistant", Content: "vu"},
	}
	// Relu depuis le JSON (forme []any), comme après un redémarrage.
	var reread []Message
	b, _ := json.Marshal(msgs)
	_ = json.Unmarshal(b, &reread)

	for _, set := range [][]Message{msgs, reread} {
		orig := contentParts(set[0].Content)[1]
		if !hasInlineVideos(set) || !refVideosInMessages(set) || hasInlineVideos(set) {
			t.Fatal("la vidéo aurait dû être rangée par référence")
		}
		// Recopiée, jamais mutée : un runChat en vol peut lire ces mêmes maps.
		if partVideoURL(orig) != url {
			t.Fatal("refVideosInMessages a muté la partie d'origine")
		}
		js, _ := json.Marshal(set)
		if len(js) > 500 || !strings.Contains(string(js), vidRefScheme) {
			t.Fatalf("historique encore lourd : %d octets", len(js))
		}
		out := expandVideoRefs(set)
		if got := partVideoURL(contentParts(out[0].Content)[1]); got != url {
			t.Fatal("la vidéo renvoyée au modèle diffère de l'originale")
		}
		// L'historique lui-même n'est pas modifié par l'expansion.
		if u := partVideoURL(contentParts(set[0].Content)[1]); !strings.HasPrefix(u, vidRefScheme) {
			t.Fatal("expandVideoRefs ne doit pas toucher l'historique")
		}
	}
	// Fichier effacé (ménage) : une mention, pas une requête qui échoue.
	_ = os.RemoveAll(chatVidDir())
	vidCacheMu.Lock()
	vidCache, vidCacheOrder, vidCacheBytes = map[string]string{}, nil, 0
	vidCacheMu.Unlock()
	out := expandVideoRefs(msgs)
	if p := contentParts(out[0].Content)[1]; p["type"] != "text" || !strings.Contains(p["text"].(string), "indisponible") {
		t.Fatalf("vidéo disparue : %v", p)
	}
}

// userMessageContent : une vidéo jointe devient une partie input_video (et une
// image jointe reste une partie image_url), quand la vision est active et que
// le moteur lit la vidéo.
func TestUserMessageContentVideo(t *testing.T) {
	videoEngine(t, true)
	visionOn(t)
	engineSees(t, true)
	dir, err := uploadsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "clip-test.mp4"), []byte("fake-mp4-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "photo.png"), pngPixel, 0o600); err != nil {
		t.Fatal(err)
	}
	in := []attachInfo{
		{Name: "clip-test.mp4", Path: "uploads/clip-test.mp4", Size: 14},
		{Name: "photo.png", Path: "uploads/photo.png", Size: int64(len(pngPixel))},
	}
	parts, ok := userMessageContent(in, "décris ce clip").([]map[string]any)
	if !ok || len(parts) != 3 {
		t.Fatalf("attendu texte + image + vidéo, obtenu %#v", parts)
	}
	if parts[0]["type"] != "text" || parts[1]["type"] != "image_url" || parts[2]["type"] != "input_video" {
		t.Fatalf("ordre des parties : %v %v %v", parts[0]["type"], parts[1]["type"], parts[2]["type"])
	}
	if u := partVideoURL(parts[2]); !strings.HasPrefix(u, "data:video/mp4;base64,") {
		t.Fatalf("data URI vidéo inattendue : %.40q", u)
	}
	// Montrée, elle n'est plus annoncée comme simple fichier.
	if txt := parts[0]["text"].(string); strings.Contains(txt, "clip-test.mp4") || strings.Contains(txt, "ffmpeg") {
		t.Fatalf("vidéo vue annoncée comme fichier : %q", txt)
	}
}

// Moteur sans vidéo (API externe, Strata, llama.cpp ancien) : il ignorait la
// partie input_video sans erreur et le modèle ne recevait que le texte. La
// vidéo doit alors être annoncée comme fichier, avec la marche à suivre
// (ffmpeg puis see_image), et see_video doit le dire au lieu de charger.
func TestVideoUnsupportedEngine(t *testing.T) {
	videoEngine(t, false)
	visionOn(t)
	engineSees(t, true)
	dir, err := uploadsDir()
	if err != nil {
		t.Fatal(err)
	}
	vname := "clip-test.mp4"
	if err := os.WriteFile(filepath.Join(dir, vname), []byte("fake-mp4-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	content := userMessageContent([]attachInfo{{Name: vname, Path: "uploads/" + vname, Size: 14}}, "décris ce clip")
	s, ok := content.(string)
	if !ok {
		t.Fatalf("attendu du texte seul (pas de partie input_video), obtenu %T", content)
	}
	if !strings.Contains(s, "uploads/"+vname) || !strings.Contains(s, "ffmpeg") || !strings.HasSuffix(s, "décris ce clip") {
		t.Fatalf("chemin et consigne ffmpeg attendus : %q", s)
	}
	if msg, part := toolSeeVideo(filepath.Join(dir, vname)); part != nil || !strings.Contains(msg, "ffmpeg") {
		t.Fatalf("see_video doit refuser avec la consigne ffmpeg : %q", msg)
	}
	// Ni outil annoncé, ni consigne quand ce n'est pas une vidéo.
	for _, tl := range EnabledTools(Caps{Agent: true}) {
		if tl.Function.Name == "see_video" {
			t.Fatal("see_video annoncé alors que le moteur ne lit pas la vidéo")
		}
	}
	if s := attachNote([]attachInfo{{Name: "a.txt", Path: "uploads/a.txt", Size: 1}}); strings.Contains(s, "ffmpeg") {
		t.Fatalf("consigne vidéo sur un fichier texte : %q", s)
	}
}

// see_video : annoncé avec see_image quand le moteur lit la vidéo.
func TestSeeVideoAnnonce(t *testing.T) {
	videoEngine(t, true)
	visionOn(t)
	has := func(caps Caps) bool {
		for _, tl := range EnabledTools(caps) {
			if tl.Function.Name == "see_video" {
				return true
			}
		}
		return false
	}
	if !has(Caps{Agent: true}) {
		t.Fatal("see_video absent alors que le moteur lit la vidéo")
	}
	if has(Caps{}) {
		t.Fatal("see_video hors du mode agent")
	}
	if toolCallLabel("see_video", map[string]any{"file": "a.mp4"}) != "a.mp4" {
		t.Fatal("libellé de see_video vide : l'outil s'exécuterait sur une chaîne vide")
	}
	if dedupableTool("see_video") {
		t.Fatal("see_video dédupliqué : le rappel rendrait « déjà fait » sans la vidéo")
	}
}

// toolSeeVideo : chaque refus nomme son motif ; un mp4 présent est chargé et
// relayé dans un message utilisateur qui nomme le fichier.
func TestToolSeeVideo(t *testing.T) {
	videoEngine(t, true)
	home := visionOn(t)
	engineSees(t, true)
	if err := os.MkdirAll(filepath.Join(home, "dossier.mp4"), 0o755); err != nil {
		t.Fatal(err)
	}
	gros := filepath.Join(home, "gros.mp4")
	f, _ := os.Create(gros)
	_ = f.Truncate(maxVideoBytes + 1)
	f.Close()
	_ = os.WriteFile(filepath.Join(home, "vide.webm"), nil, 0o600)
	cas := []struct{ path, attendu string }{
		{"", "chemin de fichier manquant"},
		{filepath.Join(home, "notes.txt"), "format non reconnu comme vidéo"},
		{filepath.Join(home, "nulle-part.mp4"), "introuvable"},
		{filepath.Join(home, "dossier.mp4"), "dossier"},
		{gros, "trop lourde"},
		{filepath.Join(home, "vide.webm"), "fichier vide"},
	}
	for _, c := range cas {
		msg, part := toolSeeVideo(c.path)
		if part != nil || !strings.HasPrefix(msg, "[erreur]") || !strings.Contains(msg, c.attendu) {
			t.Errorf("%q : %q, attendu une erreur contenant %q", c.path, msg, c.attendu)
		}
	}
	p := filepath.Join(home, "clip.MOV")
	if err := os.WriteFile(p, []byte("fake-mov"), 0o600); err != nil {
		t.Fatal(err)
	}
	msg, part := toolSeeVideo(p)
	if part == nil || msg != "[ok] vidéo chargée : clip.MOV" {
		t.Fatalf("attendu un [ok] : %q / %v", msg, part)
	}
	if u := partVideoURL(part); u != "data:video/quicktime;base64,"+base64.StdEncoding.EncodeToString([]byte("fake-mov")) {
		t.Fatalf("partie inattendue : %v", part)
	}
	// Le moteur en service ne voit pas : on le dit au lieu d'envoyer 200 Mio.
	engineSees(t, false)
	if msg, part := toolSeeVideo(p); part != nil || !strings.Contains(msg, "ne traite pas les images") {
		t.Fatalf("moteur aveugle : %q", msg)
	}
	// Relais : passé par l'API interne comme see_image (toolStepExtras).
	_, _, relay := toolStepExtras("see_video", "clip.MOV", toolExecResult{Result: "[ok]", VisionVideo: videoPart([]byte("x"), "video/mp4")})
	if relay == nil {
		t.Fatal("aucun relais pour see_video")
	}
	rp := contentParts(relay.Content)
	if relay.Role != "user" || rp[0]["text"] != "Vidéo demandée (clip.MOV) :" || rp[1]["type"] != "input_video" {
		t.Fatalf("relais inattendu : %#v", relay)
	}
}

// Sans projecteur ou sur un poste distant : refus explicite, avant toute
// question au moteur.
func TestSeeVideoSansVision(t *testing.T) {
	videoEngine(t, true)
	testHome(t)
	if msg, part := toolSeeVideo("a.mp4"); part != nil || !strings.Contains(msg, "MMPROJ") {
		t.Fatalf("sans vision : %q", msg)
	}
}

// videoInputSupported : jamais pour une API externe, Strata, un preset sans
// projecteur ou sans moteur ; sinon, l'aide du binaire fait foi.
func TestVideoInputSupportedPresets(t *testing.T) {
	testHome(t)
	cas := []map[string]string{
		{"BIN": "llama-server", "MODEL": "m.gguf"},                                    // pas de projecteur
		{"MMPROJ": "mm.gguf", "MODEL": "m.gguf"},                                      // pas de moteur
		{"MMPROJ": "mm.gguf", "BIN": "introuvable/llama-server"},                      // moteur absent
		{"MMPROJ": "mm.gguf", extKeyFlag: "1", extKeyVision: "1"},                     // API externe
		{"MMPROJ": "mm.gguf", "ENGINE": engineStrata, "BIN": "/bin/sh", "MODEL": "m"}, // Strata
	}
	for _, cfg := range cas {
		if err := WriteConfig(cfg); err != nil {
			t.Fatal(err)
		}
		if videoInputSupported() {
			t.Errorf("%v : vidéo annoncée", cfg)
		}
	}
}

// L'aide du binaire décide, relue quand le binaire change (mise à jour en place).
func TestBinHelpHasVideo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("faux moteur en sh")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "llama-server")
	write := func(help string, mt time.Time) {
		if err := os.WriteFile(bin, []byte("#!/bin/sh\necho '"+help+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(bin, mt, mt)
	}
	write("--mmproj FILE", time.Now().Add(-time.Hour))
	if binHelpHasVideo(bin) {
		t.Fatal("moteur sans --video-fps annoncé comme lisant la vidéo")
	}
	write("--video-fps N   target fps", time.Now())
	if !binHelpHasVideo(bin) {
		t.Fatal("moteur mis à jour : --video-fps non relu")
	}
}

// Compaction, fil rechargé, export : la vidéo n'est jamais rejouée ni
// recopiée, mais sa perte reste visible.
func TestVideoPerdueVisible(t *testing.T) {
	part := videoPart([]byte(strings.Repeat("v", 3000)), "video/mp4")
	m := Message{Role: "user", Content: []map[string]any{{"type": "text", "text": "regarde"}, part}}
	if got := msgText(m); got != "regarde"+videoLostMarker {
		t.Fatalf("msgText : %q", got)
	}
	var reread []Message
	b, _ := json.Marshal([]Message{m})
	_ = json.Unmarshal(b, &reread)
	out := stripImageParts(reread)
	if s, ok := out[0].Content.(string); !ok || s != "regarde"+videoLostMarker {
		t.Fatalf("fil rechargé : %#v", out[0].Content)
	}
	exp := stripInlineImages([]Message{m})
	js, _ := json.Marshal(exp)
	if len(js) > 400 || !strings.Contains(string(js), "élidé") {
		t.Fatalf("export : %s", js)
	}
	if partVideoURL(contentParts(m.Content)[1]) == "" || !strings.HasPrefix(partVideoURL(part), "data:video/mp4;base64,") {
		t.Fatal("l'export a touché le message vivant")
	}
}

// Le ménage des médias vide aussi les vidéos jointes trop vieilles.
func TestPruneChatVideos(t *testing.T) {
	testHome(t)
	ref, err := storeChatVideo([]byte("vieille"), "video/webm")
	if err != nil || !strings.HasSuffix(ref, ".webm") {
		t.Fatalf("rangement : %q %v", ref, err)
	}
	p := filepath.Join(chatVidDir(), strings.TrimPrefix(ref, vidRefScheme))
	old := time.Now().Add(-2 * chatImgKeep)
	_ = os.Chtimes(p, old, old)
	pruneChatImages()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("vieille vidéo gardée")
	}
}
