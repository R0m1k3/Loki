package loki

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func argVal(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func TestServerArgs(t *testing.T) {
	testHome(t)
	c := DictateCfg{Model: "small-q5_1", Lang: "fr", Device: "cpu"}.withDefauts()
	args, err := whisperServerArgs(c, 8091)
	if err != nil {
		t.Fatalf("construction des arguments : %v", err)
	}
	if v, ok := argVal(args, "-m"); !ok || !strings.HasSuffix(v, "ggml-small-q5_1.bin") {
		t.Errorf("-m = %q, doit pointer sur le .bin du modèle choisi", v)
	}
	if v, ok := argVal(args, "-l"); !ok || v != "fr" {
		t.Errorf("-l = %q, attendu fr", v)
	}
	if v, ok := argVal(args, "--port"); !ok || v != "8091" {
		t.Errorf("--port = %q, attendu 8091", v)
	}
	// whisper-server n'a aucune authentification : l'exposer sur 0.0.0.0
	// ouvrirait la transcription à tout le réseau.
	if v, ok := argVal(args, "--host"); !ok || v != "127.0.0.1" {
		t.Errorf("--host = %q, attendu 127.0.0.1", v)
	}
}

func TestServerArgsModeleInconnu(t *testing.T) {
	testHome(t)
	if _, err := whisperServerArgs(DictateCfg{Model: "nawak", Lang: "fr", Device: "cpu"}, 8091); err == nil {
		t.Error("un modèle hors catalogue doit produire une erreur, pas une commande avec -m vide")
	}
}

func TestServerEnvGPU(t *testing.T) {
	base := []string{"PATH=/usr/bin", "CUDA_VISIBLE_DEVICES=7", "HOME=/root"}

	env := whisperServerEnv(DictateCfg{Device: "1"}, base)
	if !contientExact(env, "CUDA_VISIBLE_DEVICES=1") {
		t.Errorf("env = %v, doit contenir CUDA_VISIBLE_DEVICES=1", env)
	}
	// L'ancienne valeur doit être REMPLACÉE, pas doublée : avec deux
	// occurrences, le gagnant dépend de l'implémentation — le choix de
	// l'utilisateur ne doit pas dépendre de ça.
	if n := compte(env, "CUDA_VISIBLE_DEVICES="); n != 1 {
		t.Errorf("%d occurrences de CUDA_VISIBLE_DEVICES, attendu exactement 1", n)
	}
	if !contientExact(env, "PATH=/usr/bin") {
		t.Error("le reste de l'environnement doit être conservé")
	}

	env = whisperServerEnv(DictateCfg{Device: "cpu"}, base)
	if !contientExact(env, "CUDA_VISIBLE_DEVICES=") {
		t.Errorf("env = %v, en mode CPU la variable doit être posée VIDE (et non retirée) pour masquer tous les GPU", env)
	}
}

func contientExact(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

func compte(l []string, prefixe string) int {
	n := 0
	for _, v := range l {
		if strings.HasPrefix(v, prefixe) {
			n++
		}
	}
	return n
}

func TestPortLibre(t *testing.T) {
	p, err := portLibre()
	if err != nil {
		t.Fatalf("portLibre : %v", err)
	}
	if p < 1024 || p > 65535 {
		t.Errorf("port = %d, hors de la plage utilisable", p)
	}
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
	if err != nil {
		t.Fatalf("port %d annoncé libre mais inutilisable : %v", p, err)
	}
	_ = l.Close()
}

func TestInferParseLaReponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/inference" {
			t.Errorf("chemin appelé = %q, attendu /inference", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/form-data") {
			t.Errorf("Content-Type = %q, whisper-server attend du multipart", ct)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"  bonjour ceci est un test  "}`))
	}))
	defer srv.Close()

	txt, err := whisperInferSur(context.Background(), srv.URL, []byte("RIFFfaux"))
	if err != nil {
		t.Fatalf("whisperInferSur : %v", err)
	}
	if txt != "bonjour ceci est un test" {
		t.Errorf("texte = %q, les espaces de bord doivent être retirés", txt)
	}
}

// Sur du silence, whisper renvoie ses marqueurs internes. Les laisser passer
// les collerait tels quels dans le champ de saisie — c'est ce qui est arrivé
// avec [BLANK_AUDIO].
func TestInferFiltreLesMarqueurs(t *testing.T) {
	for _, marqueur := range []string{"[BLANK_AUDIO]", "(silence)", "[SOUND]", " [ Silence ] ", "  "} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"text":"` + marqueur + `"}`))
		}))
		txt, err := whisperInferSur(context.Background(), srv.URL, []byte("RIFFfaux"))
		srv.Close()
		if err != nil {
			t.Fatalf("whisperInferSur(%q) : %v", marqueur, err)
		}
		if txt != "" {
			t.Errorf("marqueur %q rendu comme texte %q, attendu vide", marqueur, txt)
		}
	}
}

func TestInferErreurHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()
	if _, err := whisperInferSur(context.Background(), srv.URL, []byte("RIFFfaux")); err == nil {
		t.Error("un 500 de whisper-server doit remonter une erreur")
	}
}

// Un modèle absent doit être annoncé comme tel, pas produire un lancement de
// whisper-server voué à mourir sur un fichier introuvable.
func TestEnsureModeleAbsent(t *testing.T) {
	testHome(t)
	if err := dictateCfgSave(DictateCfg{Model: "small-q5_1", Lang: "fr", Device: "cpu", Reactivity: "moyen"}); err != nil {
		t.Fatal(err)
	}
	_, err := whisperEnsure()
	if err == nil {
		t.Fatal("un modèle absent doit produire une erreur")
	}
	if !strings.Contains(err.Error(), "absent") {
		t.Errorf("erreur = %q, elle doit dire que le modèle est absent", err)
	}
}

func TestEtatSansServeur(t *testing.T) {
	testHome(t)
	e := whisperEtat()
	if e["actif"] != false {
		t.Errorf("actif = %v, attendu false sans serveur lancé", e["actif"])
	}
	for _, clef := range []string{"modele", "present", "device", "note"} {
		if _, ok := e[clef]; !ok {
			t.Errorf("clé %q manquante — l'UI en a besoin", clef)
		}
	}
}
