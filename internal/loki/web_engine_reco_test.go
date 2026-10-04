package loki

// Version recommandée du moteur, retour à la version précédente, et garde du
// rendu des gabarits à travers une mise à jour (web_engine_reco.go).
//
// Ce qui se joue : ne JAMAIS proposer une version qu'on n'a pas vue publiée,
// ne rien faire sans réseau d'autre que le dire, garder un chemin de retour
// sans réseau, et ne pas laisser une mise à jour changer en silence le prompt
// que voit le modèle.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Le cas nominal : le dernier build publié dépasse le minimum, et c'est son tag
// FIGÉ qui est proposé (celui qu'on installera), pas le tag mouvant.
func TestVersionRecommandeeChoixDuTag(t *testing.T) {
	fauxRegistre(t, "b11351", [][]byte{fauxCouche(t, map[string]string{"app/llama-server": "x"})}, nil)
	build, tag, err := engineRecommendedTag("server-cuda")
	if err != nil {
		t.Fatal(err)
	}
	if build != 11351 || tag != "server-cuda-b11351" {
		t.Errorf("build=%d tag=%q, attendu 11351 / server-cuda-b11351", build, tag)
	}
}

// Un registre dont le dernier build est encore sous le minimum, ou qui ne dit
// pas son numéro : rien n'est proposé, et l'erreur dit pourquoi.
func TestVersionRecommandeeRefusee(t *testing.T) {
	for _, c := range []struct{ nom, version, attendu string }{
		{"trop ancienne", "b10700", "antérieur"},
		{"sans numéro", "", "n'annonce pas le build"},
	} {
		t.Run(c.nom, func(t *testing.T) {
			fauxRegistre(t, c.version, [][]byte{fauxCouche(t, map[string]string{"app/llama-server": "x"})}, nil)
			_, tag, err := engineRecommendedTag("server-cuda")
			if err == nil || !strings.Contains(err.Error(), c.attendu) {
				t.Fatalf("tag=%q err=%v, attendu une erreur « %s »", tag, err, c.attendu)
			}
		})
	}
}

// L'annotation du tag mouvant annonce un build dont le tag figé n'existe pas
// (encore) : on ne propose pas un tag introuvable.
func TestVersionRecommandeeTagFigeAbsent(t *testing.T) {
	index, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"layers":        []map[string]any{{"digest": "sha256:x", "size": 1}},
		"annotations":   map[string]string{"org.opencontainers.image.version": "b11400"},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/token"):
			_, _ = w.Write([]byte(`{"token":"t"}`))
		case strings.HasSuffix(r.URL.Path, "/manifests/server-cuda"):
			_, _ = w.Write(index)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	old := ociBase
	ociBase = srv.URL
	defer func() { ociBase = old }()

	if _, tag, err := engineRecommendedTag("server-cuda"); err == nil || !strings.Contains(err.Error(), "introuvable") {
		t.Fatalf("tag=%q err=%v, attendu « introuvable »", tag, err)
	}
}

// Sans réseau : une erreur lisible, aucun tag, rien d'installé.
func TestVersionRecommandeeSansReseau(t *testing.T) {
	testHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // port fermé : connexion refusée
	old := ociBase
	ociBase = srv.URL
	defer func() { ociBase = old }()

	_, tag, err := engineRecommendedTag("server-cuda")
	if err == nil || tag != "" || !strings.Contains(err.Error(), "registre injoignable") {
		t.Fatalf("tag=%q err=%v, attendu « registre injoignable »", tag, err)
	}
	if got := engineInstalled(); len(got) != 0 {
		t.Errorf("versions installées sans réseau : %v", got)
	}
}

// L'encart : seulement pour un build de confiance sous le minimum, là où les
// images officielles existent. Les gains cités nomment leurs builds.
func TestEncartVersionRecommandee(t *testing.T) {
	r := engineRecommendation(10678, true)
	if r == nil {
		t.Fatal("pas d'encart pour b10678")
	}
	gains := strings.Join(r["gains"].([]string), "\n")
	for _, want := range []string{"b10864", "b11009", "b11331", "points de reprise"} {
		if !strings.Contains(gains, want) {
			t.Errorf("gains sans « %s » : %s", want, gains)
		}
	}
	for _, c := range []struct {
		build int
		ok    bool
	}{{engineMinRecommended, true}, {11351, true}, {0, true}, {10678, false}} {
		if engineRecommendation(c.build, c.ok) != nil {
			t.Errorf("encart pour build=%d supporté=%v", c.build, c.ok)
		}
	}
}

// Retour arrière : la version quittée est notée, proposée tant qu'elle existe,
// et le retour fait de la version quittée à son tour « la précédente ».
func TestRetourVersionPrecedente(t *testing.T) {
	testHome(t)
	mk := func(tag string) string {
		d := filepath.Join(engineDir(), tag)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(d, "llama-server")
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ancien, nouveau := mk("server-cuda-b10678"), mk("server-cuda-b11351")

	if enginePrevious(nouveau) != nil {
		t.Fatal("version précédente annoncée avant toute bascule")
	}
	// Le ménage d'après mise à jour garde bien l'ancienne (lot 1).
	if keep := engineKeepAfterUpdate(nouveau, ancien, nil); !keep["server-cuda-b10678"] {
		t.Fatalf("ménage : l'ancienne version n'est pas gardée (%v)", keep)
	}
	engineRememberPrevious(ancien, nouveau)
	prev := enginePrevious(nouveau)
	if prev == nil || prev["tag"] != "server-cuda-b10678" || prev["build"] != 10678 {
		t.Fatalf("précédente = %v", prev)
	}
	// Ne jamais proposer de « revenir » sur le moteur qui tourne.
	if enginePrevious(ancien) != nil {
		t.Error("la version courante proposée comme précédente")
	}

	// Le retour par la route : BIN revient, et la version quittée devient la
	// précédente (deux clics ramènent où l'on était).
	if err := SetConfigKey("BIN", nouveau); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handleEngineRollback(rec, httptest.NewRequest("POST", "/api/engine/rollback", strings.NewReader("{}")))
	if rec.Code != 200 {
		t.Fatalf("rollback : HTTP %d %s", rec.Code, rec.Body.String())
	}
	if got := ReadConfig()["BIN"]; !samePath(got, ancien) {
		t.Fatalf("BIN = %q après retour, attendu %q", got, ancien)
	}
	if p := enginePrevious(ancien); p == nil || p["tag"] != "server-cuda-b11351" {
		t.Errorf("après retour, précédente = %v", p)
	}

	// Une version supprimée à la main : plus de bouton, et la route refuse.
	if err := os.RemoveAll(filepath.Dir(nouveau)); err != nil {
		t.Fatal(err)
	}
	if enginePrevious(ancien) != nil {
		t.Error("version supprimée encore proposée")
	}
	rec = httptest.NewRecorder()
	handleEngineRollback(rec, httptest.NewRequest("POST", "/api/engine/rollback", strings.NewReader("{}")))
	if rec.Code != 400 {
		t.Errorf("rollback sans précédente : HTTP %d, attendu 400", rec.Code)
	}
}

// Le verdict « le prompt rendu va changer » : seulement quand c'est établi
// (gabarit qui lit preserve_thinking ET retire aujourd'hui la réflexion), et
// jamais de case cochée d'office ailleurs — sur Qwen3.8, off changerait le rendu.
func TestAvertissementRenduRaisonnement(t *testing.T) {
	qwen36 := "{%- if preserve_thinking is defined and preserve_thinking %}…"
	base := preserveIn{Current: 10678, Target: 11351, Template: qwen36, TplKnown: true, Preserves: tplNo}
	for _, c := range []struct {
		nom     string
		mod     func(*preserveIn)
		risk    string
		suggest bool
	}{
		{"Qwen3.6 sur moteur ancien", func(*preserveIn) {}, "yes", true},
		{"Qwen3.8 garde déjà", func(in *preserveIn) { in.Preserves = tplYes }, "no", false},
		{"gabarit sans la variable", func(in *preserveIn) { in.Template = "{{ messages }}" }, "no", false},
		{"clear_thinking (GLM)", func(in *preserveIn) { in.Template = "{% if clear_thinking %}" }, "yes", true},
		{"sonde sans verdict", func(in *preserveIn) { in.Preserves = tplUnknown }, "unknown", false},
		{"moteur arrêté", func(in *preserveIn) { in.TplKnown, in.Template = false, "" }, "unknown", false},
		{"clé posée", func(in *preserveIn) { in.Key = "on" }, "no", false},
		{"EXTRA_ARGS", func(in *preserveIn) { in.Overridden = "EXTRA_ARGS" }, "no", false},
		{"cible avant b10763", func(in *preserveIn) { in.Target = 10700 }, "no", false},
		{"cible inconnue", func(in *preserveIn) { in.Target = 0 }, "yes", true},
		{"moteur courant déjà ≥ b10763", func(in *preserveIn) { in.Current = 10800 }, "no", false},
		{"preset externe", func(in *preserveIn) { in.External = true }, "no", false},
	} {
		t.Run(c.nom, func(t *testing.T) {
			in := base
			c.mod(&in)
			r := reasoningPreserveRisk(in)
			if r.Risk != c.risk || r.Suggest != c.suggest || r.Why == "" {
				t.Errorf("= %+v, attendu risk=%s suggest=%v", r, c.risk, c.suggest)
			}
		})
	}
}

// Les réglages qui fixent déjà preserve_reasoning hors de la clé.
func TestPreserveDejaRegle(t *testing.T) {
	t.Setenv("LLAMA_ARG_REASONING_PRESERVE", "")
	t.Setenv("LLAMA_ARG_CHAT_TEMPLATE_KWARGS", "")
	if got := preserveOverridden(map[string]string{"EXTRA_ARGS": "--no-reasoning-preserve -c 4096"}); got != "EXTRA_ARGS" {
		t.Errorf("EXTRA_ARGS : %q", got)
	}
	if got := preserveOverridden(map[string]string{"EXTRA_ARGS": `--chat-template-kwargs {"preserve_reasoning":false}`}); got != "--chat-template-kwargs" {
		t.Errorf("kwargs : %q", got)
	}
	if got := preserveOverridden(map[string]string{}); got != "" {
		t.Errorf("rien de posé : %q", got)
	}
	t.Setenv("LLAMA_ARG_REASONING_PRESERVE", "0")
	if got := preserveOverridden(map[string]string{}); got != "LLAMA_ARG_REASONING_PRESERVE" {
		t.Errorf("variable : %q", got)
	}
}

// Les lectures du verdict sur un vrai (faux) moteur : gabarit et sonde.
func TestAvertissementRenduSurLeMoteur(t *testing.T) {
	t.Setenv("LLAMA_ARG_REASONING_PRESERVE", "")
	t.Setenv("LLAMA_ARG_CHAT_TEMPLATE_KWARGS", "")
	for _, c := range []struct{ nom, mode, tpl, risk string }{
		{"retire la réflexion", "qwen3", "{% if preserve_thinking %}", "yes"},
		{"la garde déjà", "keep", "{% if preserve_thinking is not defined or preserve_thinking %}", "no"},
		{"ne lit pas la variable", "qwen3", "", "no"},
	} {
		t.Run(c.nom, func(t *testing.T) {
			testHome(t)
			freshTplProbe(t)
			(&fakeTpl{mode: c.mode, tpl: c.tpl}).start(t)
			r := reasoningPreserveRisk(enginePreserveInputs(context.Background(), 11351))
			if r.Risk != c.risk {
				t.Errorf("risk=%s (%s), attendu %s", r.Risk, r.Why, c.risk)
			}
		})
	}
	t.Run("moteur arrêté", func(t *testing.T) {
		testHome(t)
		freshTplProbe(t)
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		u, _ := url.Parse(srv.URL)
		srv.Close()
		if err := SetConfigKey("PORT", u.Port()); err != nil {
			t.Fatal(err)
		}
		if r := reasoningPreserveRisk(enginePreserveInputs(context.Background(), 11351)); r.Risk != "unknown" || r.Suggest {
			t.Errorf("= %+v, attendu unknown sans case cochée", r)
		}
	})
}

// REASONING_PRESERVE=off, choisi par l'utilisateur : dans le preset actif (il
// survit aux bascules), jamais par-dessus une valeur déjà posée, et pas du tout
// sur un moteur qui ne connaît pas le drapeau.
func TestPoserPreserveOff(t *testing.T) {
	testHome(t)
	dir := presetsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "qwen.env"), []byte("# NAME=Qwen\nMODEL=q.gguf\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setConfig(t, "MODEL=q.gguf\n")
	oui := func() bool { return true }

	if _, err := enginePreserveOffIf(func() bool { return false }); err == nil {
		t.Fatal("posée alors que le moteur ne connaît pas --no-reasoning-preserve")
	}
	if ReadConfig()["REASONING_PRESERVE"] != "" {
		t.Fatal("clé écrite malgré le refus")
	}
	if _, err := enginePreserveOffIf(oui); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "qwen.env"))
	if parseEnv(string(b))["REASONING_PRESERVE"] != "off" || ReadConfig()["REASONING_PRESERVE"] != "off" {
		t.Fatalf("preset=%q config=%q", b, ReadConfig()["REASONING_PRESERVE"])
	}

	// Déjà posée (à on) : on ne contredit pas un choix antérieur.
	setConfig(t, "MODEL=autre.gguf\nREASONING_PRESERVE=on\n")
	if msg, err := enginePreserveOffIf(oui); err != nil || ReadConfig()["REASONING_PRESERVE"] != "on" {
		t.Fatalf("msg=%q err=%v valeur=%q", msg, err, ReadConfig()["REASONING_PRESERVE"])
	}

	// Aucun preset ne correspond : la configuration active.
	setConfig(t, "MODEL=sans-preset.gguf\n")
	if _, err := enginePreserveOffIf(oui); err != nil || ReadConfig()["REASONING_PRESERVE"] != "off" {
		t.Fatalf("err=%v valeur=%q", err, ReadConfig()["REASONING_PRESERVE"])
	}
}

// La comparaison des rendus : identique, ou le premier écart, avec la
// conversation où il se trouve.
func TestComparaisonDesRendus(t *testing.T) {
	a := tplRenderSep + "historique sans raisonnement ---\n<|im_start|>assistant\nloki-sonde-reponse-c1<|im_end|>\n"
	if same, d := tplRenderDiff(a, a); !same || d != "" {
		t.Fatalf("identiques : same=%v d=%q", same, d)
	}
	b := strings.Replace(a, "assistant\n", "assistant\n<think>\n\n</think>\n\n", 1)
	same, d := tplRenderDiff(a, b)
	if same || !strings.Contains(d, "historique sans raisonnement") || !strings.Contains(d, "<think>") {
		t.Fatalf("same=%v d=%q", same, d)
	}
	// Un extrait ne coupe jamais un caractère.
	_, d = tplRenderDiff(a+"é", a+"è")
	if !strings.Contains(d, "é") || !strings.Contains(d, "è") {
		t.Errorf("extrait coupé : %q", d)
	}
}

// Après la bascule : le rendu relevé sur le nouveau moteur est comparé à celui
// d'avant, et l'écart est signalé dans l'état du panneau. Tant que le moteur
// qui répond annonce l'ancien build, rien n'est comparé.
func TestVerificationDuRenduApresBascule(t *testing.T) {
	testHome(t)
	freshTplProbe(t)
	oldPoll, oldWait := engineRenderPoll, engineRenderWait
	engineRenderPoll = time.Millisecond
	t.Cleanup(func() {
		engineRenderPoll, engineRenderWait = oldPoll, oldWait
		engineRenderMu.Lock()
		engineRenderLast = nil
		engineRenderMu.Unlock()
	})
	f := &fakeTpl{mode: "qwen3", build: "b10678-old"}
	f.start(t)
	ref, ok := engineRenderBefore(context.Background())
	if !ok || ref.build != "b10678-old" {
		t.Fatalf("rendu de référence : ok=%v build=%q", ok, ref.build)
	}
	set := func(mode, build string) {
		f.mu.Lock()
		f.mode, f.build = mode, build
		f.mu.Unlock()
	}

	// L'ancien moteur répond encore : pas de comparaison, « non vérifié ».
	engineRenderWait = 50 * time.Millisecond
	engineRenderAfter(ref, "server-cuda-b11351")
	if c := engineRenderSnapshot(); c == nil || c.Status != "unknown" {
		t.Fatalf("ancien moteur toujours là : %+v", c)
	}
	engineRenderWait = 10 * time.Second

	// Nouveau build, même gabarit : identique.
	set("qwen3", "b11351-new")
	engineRenderAfter(ref, "server-cuda-b11351")
	if c := engineRenderSnapshot(); c == nil || c.Status != "same" {
		t.Fatalf("même gabarit : %+v", c)
	}
	// Le moteur garde désormais la réflexion des tours passés : changé.
	set("keep", "b11351-new")
	engineRenderAfter(ref, "server-cuda-b11351")
	c := engineRenderSnapshot()
	if c == nil || c.Status != "changed" || !strings.Contains(c.Detail, "historique avec raisonnement") {
		t.Fatalf("gabarit changé : %+v", c)
	}
	f.assertNoCompletion(t)
}
