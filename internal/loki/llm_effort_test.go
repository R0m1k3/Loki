package loki

import "testing"

// Le message qui a motivé tout ce fichier : Qwen3.8-27B refuse « high », le
// niveau que l'interface enregistre par défaut, et llama-server répond 500.
const qwenRefus = `{"error":{"code":500,"message":"\n------------\nWhile executing CallExpression at line 49, column 28 in source:\n...', 'low') %}\n {{- raise_exception('Unexpected reasoning effort ' ~ reason...\n ^\nError: Jinja Exception: Unexpected reasoning effort high. Supported types are xhigh (default), medium, and low.","type":"server_error"}}`

func TestEffortRejection(t *testing.T) {
	sup, ok := effortRejection(qwenRefus)
	if !ok {
		t.Fatal("refus de reasoning_effort non reconnu")
	}
	// « high » est cité AVANT la liste (c'est la valeur refusée) : le lire comme
	// une valeur acceptée renverrait loki sur la même erreur en boucle.
	want := map[string]bool{"xhigh": true, "medium": true, "low": true}
	if len(sup) != len(want) {
		t.Fatalf("niveaux acceptés = %v, attendu %v", sup, want)
	}
	for _, s := range sup {
		if !want[s] {
			t.Errorf("niveau %q lu à tort comme accepté (%v)", s, sup)
		}
	}
	// Les autres 500 (prompt trop long, appel d'outil illisible) ne doivent pas
	// passer par ce filet : ils ont le leur.
	for _, body := range []string{
		"",
		`{"error":{"code":500,"message":"the request exceeds the available context size"}}`,
		`{"error":{"code":500,"message":"Failed to parse tool call"}}`,
		// Parle bien de reasoning_effort mais ne le refuse pas.
		`{"error":{"code":500,"message":"reasoning_effort applied"}}`,
	} {
		if _, ok := effortRejection(body); ok {
			t.Errorf("corps %q pris à tort pour un refus d'intensité", body)
		}
	}
}

// Sans liste de niveaux acceptés, il n'y a rien à deviner : on retire le champ.
func TestEffortRejectionSansListe(t *testing.T) {
	sup, ok := effortRejection("Error: Unknown reasoning effort 'xhigh'")
	if !ok {
		t.Fatal("refus non reconnu")
	}
	if len(sup) != 0 {
		t.Fatalf("niveaux acceptés = %v, attendu aucun", sup)
	}
	if got := nearestEffort("xhigh", sup); got != "" {
		t.Errorf("nearestEffort sans liste = %q, attendu \"\"", got)
	}
}

func TestNearestEffort(t *testing.T) {
	qwen := []string{"xhigh", "medium", "low"}
	oss := []string{"low", "medium", "high"}
	cases := []struct {
		want      string
		supported []string
		expect    string
	}{
		{"high", qwen, "xhigh"}, // voisin immédiat vers le haut
		{"xhigh", oss, "high"},  // voisin immédiat vers le bas
		{"medium", qwen, "low"}, // « medium » refusé quand même : on ne le renvoie pas
		{"low", []string{"medium", "high"}, "medium"},
		{"high", nil, ""},     // rien d'annoncé : on retire le champ
		{"", qwen, ""},        // auto : il n'y avait rien à envoyer
		{"none", qwen, ""},    // consigne de coupure, pas une intensité
		{"maximum", qwen, ""}, // hors échelle : on n'invente pas
	}
	for _, c := range cases {
		if got := nearestEffort(c.want, c.supported); got != c.expect {
			t.Errorf("nearestEffort(%q, %v) = %q, attendu %q", c.want, c.supported, got, c.expect)
		}
	}
	// À égalité de distance, le niveau le plus FORT : dégrader en silence est
	// pire que générer un peu plus longtemps.
	if got := nearestEffort("medium", []string{"low", "high"}); got != "high" {
		t.Errorf("égalité de distance = %q, attendu high", got)
	}
}

func TestEffortFromRejection(t *testing.T) {
	got, ok := effortFromRejection(qwenRefus, "high")
	if !ok || got != "xhigh" {
		t.Fatalf("effortFromRejection = %q, %v ; attendu xhigh, true", got, ok)
	}
	if _, ok := effortFromRejection("boom", "high"); ok {
		t.Error("un 500 quelconque ne doit pas déclencher le repli d'intensité")
	}
}

// La traduction apprise doit valoir pour les messages SUIVANTS (sinon chaque
// message repaie un aller-retour 500) et rester attachée au modèle : un autre
// modèle a un autre gabarit, donc d'autres niveaux.
func TestEffortMemoire(t *testing.T) {
	testHome(t)
	if err := WriteConfig(map[string]string{"MODEL": "/data/models/qwen.gguf"}); err != nil {
		t.Fatal(err)
	}
	if got := effortResolve("high"); got != "high" {
		t.Fatalf("sans refus connu : %q, attendu high", got)
	}
	effortRemember("high", "xhigh")
	if got := effortResolve("high"); got != "xhigh" {
		t.Errorf("après apprentissage : %q, attendu xhigh", got)
	}
	if err := WriteConfig(map[string]string{"MODEL": "/data/models/gpt-oss.gguf"}); err != nil {
		t.Fatal(err)
	}
	if got := effortResolve("high"); got != "high" {
		t.Errorf("autre modèle : %q, attendu high (gabarit inconnu)", got)
	}
	// Champ retiré ("") : c'est une réponse apprise, pas une absence de réponse.
	effortRemember("high", "")
	if got := effortResolve("high"); got != "" {
		t.Errorf("repli « champ retiré » : %q, attendu \"\"", got)
	}
}
