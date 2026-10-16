package loki

import (
	"strings"
	"testing"
)

// Lot 44 : quand la façade tient la conversation, le navigateur est le sien —
// les outils browser_* du Go, l'aperçu et la fermeture passent par elle ;
// façade en panne, le navigateur du Go prend le relais.
func TestFacadeNavigateur(t *testing.T) {
	testHome(t)
	t.Setenv("LOKI_CHROME", "/nulle/part/chrome")
	f := demarrerFacade(t)
	f.internalStatus = 200
	f.internalBody = `{"result":"# Exemple\nhttps://exemple.test/","image":null}`
	if got := toolCUOpen(map[string]any{"url": "exemple.test"}); got != "# Exemple\nhttps://exemple.test/" {
		t.Fatalf("ouverture : %q", got)
	}
	vu := f.recu()
	if len(vu) != 1 || !strings.HasPrefix(vu[0], "POST /internal/facade/v1/browser jeton ") ||
		!strings.Contains(vu[0], `"op":"tool"`) || !strings.Contains(vu[0], `"name":"browser_open"`) ||
		!strings.Contains(vu[0], `"url":"exemple.test"`) {
		t.Fatalf("appel : %v", vu)
	}
	f.internalBody = `{"result":"[ok] capture","image":{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}}`
	res, img := toolCUScreenshot()
	if res != "[ok] capture" || img["type"] != "image_url" {
		t.Fatalf("capture : %q %v", res, img)
	}
	f.internalBody = `{"result":"[erreur] la vision n'est pas active","image":null}`
	if res, img := toolCUScreenshot(); img != nil || !strings.HasPrefix(res, "[erreur]") {
		t.Fatalf("capture refusée : %q %v", res, img)
	}
	f.internalBody = `{"id":"abc"}`
	if id := cuAutoShot(); id != "abc" {
		t.Fatalf("aperçu : %q", id)
	}
	f.internalBody = `{}`
	cdpShutdown()
	vu = f.recu()
	if last := vu[len(vu)-1]; !strings.Contains(last, `"op":"shutdown"`) {
		t.Fatalf("fermeture : %v", vu)
	}
	if !facadeChatRoutes["/api/chat/cushot"] {
		t.Fatal("/api/chat/cushot n'est pas remis à la façade")
	}
	// Façade en panne : le Go pilote lui-même.
	f.internalStatus = 500
	if got := toolCUSnapshot(); got != "[erreur] fork/exec /nulle/part/chrome: no such file or directory" {
		t.Fatalf("repli : %q", got)
	}
}
