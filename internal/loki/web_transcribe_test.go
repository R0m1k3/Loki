package loki

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func dockerfileSrc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatalf("lecture du Dockerfile : %v", err)
	}
	return string(b)
}

// Le binaire de dictée est TÉLÉCHARGÉ, plus compilé. Ce qui remplace les
// garde-fous de compilation d'avant (-march=native, SIGILL en production) est la
// vérification d'empreinte : sans elle, une release remplacée en amont ferait
// tourner un binaire inconnu sur la machine de l'utilisateur, en silence.
func TestDockerfileASRVerifieLEmpreinte(t *testing.T) {
	src := dockerfileSrc(t)
	if !strings.Contains(src, "sha256sum -c -") {
		t.Error("l'archive sherpa-onnx est extraite sans vérification d'empreinte")
	}
	m := regexp.MustCompile(`(?m)^ARG SHERPA_ONNX_SHA256=([0-9a-f]{64})\s*$`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("ARG SHERPA_ONNX_SHA256 absent ou pas un sha256 de 64 caractères")
	}
	if !strings.Contains(src, "${SHERPA_ONNX_SHA256}") {
		t.Error("l'empreinte est déclarée mais jamais utilisée")
	}
}

// La version doit être ÉPINGLÉE. « latest » ferait changer le binaire de dictée
// sous les pieds de l'utilisateur au prochain build, sans qu'une seule ligne du
// dépôt ait bougé — et l'empreinte figée ci-dessus casserait le build.
func TestDockerfileASRVersionEpinglee(t *testing.T) {
	src := dockerfileSrc(t)
	m := regexp.MustCompile(`(?m)^ARG SHERPA_ONNX_VERSION=(\S+)\s*$`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("ARG SHERPA_ONNX_VERSION absent")
	}
	if !regexp.MustCompile(`^v\d+\.\d+\.\d+$`).MatchString(m[1]) {
		t.Errorf("version de sherp-onnx non épinglée : %q", m[1])
	}
}

// Régression (héritée de whisper) : un ARG déclaré APRÈS le premier FROM
// n'appartient qu'à l'étape où il apparaît. Les « ARG SHERPA_* » nus de l'étape
// asrfetch hériteraient alors d'une valeur vide — URL tronquée, build cassé, et
// la cause invisible dans le journal.
func TestDockerfileArgFlagsGlobal(t *testing.T) {
	src := dockerfileSrc(t)
	fromLoc := regexp.MustCompile(`(?m)^FROM `).FindStringIndex(src)
	// Ancré en début de ligne : une occurrence dans un commentaire ne compte
	// pas — c'est précisément ainsi qu'une première version de ce test s'est
	// laissée berner par sa propre contre-épreuve.
	for _, arg := range []string{"SHERPA_ONNX_VERSION", "SHERPA_ONNX_SHA256"} {
		loc := regexp.MustCompile(`(?m)^ARG ` + arg + `=`).FindStringIndex(src)
		if loc == nil {
			t.Errorf("ARG %s= introuvable dans le Dockerfile", arg)
			continue
		}
		if fromLoc != nil && loc[0] > fromLoc[0] {
			t.Errorf("ARG %s= est déclaré APRÈS un FROM : l'étape asrfetch héritera d'une valeur vide", arg)
		}
	}
}

// Le chemin du binaire est lu depuis LOKI_ASR_SERVER (asrServerBin) : l'image
// doit poser la variable ET copier le binaire à cette adresse, sinon la dictée
// échoue au premier clic sur le micro avec « serveur de dictée introuvable ».
func TestDockerfileASRBinaireEtVariableConcordent(t *testing.T) {
	src := dockerfileSrc(t)
	const chemin = "/usr/local/bin/sherpa-onnx-offline-websocket-server"
	if !strings.Contains(src, "COPY --from=asrfetch /out/sherpa-onnx-offline-websocket-server "+chemin) {
		t.Errorf("le binaire de dictée n'est pas copié vers %s", chemin)
	}
	if !strings.Contains(src, "LOKI_ASR_SERVER="+chemin) {
		t.Errorf("LOKI_ASR_SERVER ne pointe pas sur %s", chemin)
	}
}

// whisper.cpp est parti : plus aucune étape ne doit le compiler, et aucune
// variable ne doit prétendre le trouver. Un reste ferait rebâtir 35 minutes de
// CUDA pour un binaire que plus personne n'exécute.
func TestDockerfileSansWhisper(t *testing.T) {
	src := dockerfileSrc(t)
	for _, motif := range []string{"whisperbuild", "whisper-server", "LOKI_WHISPER_SERVER", "WHISPER_CMAKE_FLAGS"} {
		// Les commentaires ont le droit de mentionner l'histoire ; les
		// instructions, non. On ne regarde donc que les lignes actives.
		for _, ligne := range strings.Split(src, "\n") {
			l := strings.TrimSpace(ligne)
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			if strings.Contains(l, motif) {
				t.Errorf("reste de whisper dans une instruction du Dockerfile (%s) : %s", motif, l)
			}
		}
	}
}

func TestLastLine(t *testing.T) {
	cas := []struct{ in, want string }{
		{"", ""},
		{"   \n\n  ", ""},
		{"une seule ligne", "une seule ligne"},
		{"erreur fatale\nderniere ligne utile\n", "derniere ligne utile"},
		{"fin utile\n\n   \n", "fin utile"},
	}
	for _, c := range cas {
		if got := lastLine(c.in); got != c.want {
			t.Errorf("lastLine(%q) = %q, attendu %q", c.in, got, c.want)
		}
	}
}
