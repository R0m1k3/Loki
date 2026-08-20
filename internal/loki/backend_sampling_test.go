package loki

import "testing"

// Le sampling du preset doit se retrouver dans le corps de la requête, avec les
// bons types (top_k entier, le reste flottant), et TEMP écrase la température
// déjà posée. Une clé vide n'ajoute rien : le serveur garde son défaut.
func TestApplySamplingFrom(t *testing.T) {
	payload := map[string]any{"temperature": 0.7}
	applySamplingFrom(payload, map[string]string{
		"TEMP":             "1.0",
		"TOP_P":            "0.95",
		"TOP_K":            "20",
		"MIN_P":            "0",
		"REPEAT_PENALTY":   "1.05",
		"PRESENCE_PENALTY": "", // vide → ignoré
	})
	if payload["temperature"] != 1.0 {
		t.Errorf("TEMP doit écraser la température : %v", payload["temperature"])
	}
	if payload["top_p"] != 0.95 || payload["min_p"] != 0.0 || payload["repeat_penalty"] != 1.05 {
		t.Errorf("flottants attendus : %v %v %v", payload["top_p"], payload["min_p"], payload["repeat_penalty"])
	}
	if payload["top_k"] != 20 {
		t.Errorf("top_k doit être un entier 20, pas %#v", payload["top_k"])
	}
	if _, ok := payload["presence_penalty"]; ok {
		t.Error("une clé vide ne doit rien envoyer")
	}
}

// L'échantillonnage ne touche PAS au raisonnement : ce chemin appartient à
// llm_client.go/llm_effort.go, qui y met aussi `enable_thinking`. L'amont écrit
// `chat_template_kwargs` ici ; le faire chez nous effacerait la consigne
// « aucune » — donc le modèle réfléchirait alors que l'interface dit l'inverse.
func TestApplySamplingNeTouchePasAuRaisonnement(t *testing.T) {
	payload := map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}}
	applySamplingFrom(payload, map[string]string{"REASONING_EFFORT": "medium", "TOP_K": "20"})
	kw, ok := payload["chat_template_kwargs"].(map[string]any)
	if !ok || kw["enable_thinking"] != false || len(kw) != 1 {
		t.Errorf("chat_template_kwargs modifié : %v", payload["chat_template_kwargs"])
	}
	if _, ok := payload["reasoning_effort"]; ok {
		t.Error("reasoning_effort ne doit pas être posé par l'échantillonnage")
	}
}

// Sans aucune clé de sampling, le payload n'est pas modifié : on ne force pas de
// valeurs qui contrediraient le défaut du modèle.
func TestApplySamplingEmpty(t *testing.T) {
	payload := map[string]any{"temperature": 0.7}
	applySamplingFrom(payload, map[string]string{})
	if len(payload) != 1 || payload["temperature"] != 0.7 {
		t.Errorf("payload modifié à tort : %v", payload)
	}
}
