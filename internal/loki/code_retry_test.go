package loki

import "testing"

// Les appels d'outils écrits en texte doivent être détectés…
func TestAppelOutilTextuelDetecte(t *testing.T) {
	for _, s := range []string{
		"<tool_call>{\"name\":\"bash\"}</tool_call>",
		"Je vais lancer default_api:bash pour vérifier.",
		"default_api.run_shell(command='ls')",
		"functions.bash({\"command\": \"ls\"})",
		"```tool_code\nprint('x')\n```",
		"{\"name\": \"bash\", \"arguments\": {\"command\": \"ls\"}}",
	} {
		if !textualToolCall(s) {
			t.Errorf("non détecté : %q", s)
		}
	}
}

// … sans jamais relancer un tour normal (code cité, JSON quelconque).
func TestReponseNormaleNonRelancee(t *testing.T) {
	for _, s := range []string{
		"Voici la réponse : le fichier contient trois fonctions.",
		"```go\nfunc main() {}\n```",
		"Le JSON de config : {\"name\": \"loki\", \"port\": 8090}",
		"La fonction bash() de ce script accepte un paramètre.",
		"",
	} {
		if textualToolCall(s) {
			t.Errorf("relance à tort : %q", s)
		}
	}
}
