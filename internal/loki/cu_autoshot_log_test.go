package loki

import "testing"

// L'aperçu du navigateur (shot) n'est que du direct : la compaction de fin de
// tour le retire du journal, le reste de l'événement d'outil est gardé.
func TestCompactLogRetireLAperçuNavigateur(t *testing.T) {
	log := []LogEvent{
		{Seq: 1, Delta: map[string]any{"tool_used": map[string]any{"name": "browser_click", "done": false}}},
		{Seq: 2, Delta: map[string]any{"tool_used": map[string]any{"name": "browser_click", "done": true, "result": "ok", "shot": "abc"}}},
	}
	out := compactLog(log)
	if len(out) != 1 {
		t.Fatalf("un seul événement d'outil attendu, %d", len(out))
	}
	tu := out[0].Delta["tool_used"].(map[string]any)
	if _, has := tu["shot"]; has {
		t.Fatal("l'aperçu aurait dû quitter le journal")
	}
	if tu["result"] != "ok" || tu["name"] != "browser_click" {
		t.Fatalf("événement abîmé : %v", tu)
	}
	if _, has := log[1].Delta["tool_used"].(map[string]any)["shot"]; !has {
		t.Fatal("le journal d'origine ne doit pas être modifié en place")
	}
}

func TestCUAutoShotTool(t *testing.T) {
	for _, n := range []string{"browser_open", "browser_click", "browser_type", "browser_scroll"} {
		if !cuAutoShotTool(n) {
			t.Errorf("%s devrait déclencher l'aperçu", n)
		}
	}
	for _, n := range []string{"browser_screenshot", "browser_snapshot", "bash", "web_images"} {
		if cuAutoShotTool(n) {
			t.Errorf("%s ne devrait pas déclencher l'aperçu", n)
		}
	}
}
