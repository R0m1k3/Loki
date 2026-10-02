package loki

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Une image en base64 part sur disque et revient identique à l'envoi au modèle ;
// l'historique ne garde qu'une référence de quelques octets.
func TestImageRefsRoundTrip(t *testing.T) {
	testHome(t)
	raw := []byte(strings.Repeat("\x89PNG-fake-image-bytes", 5000))
	url := "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
	msgs := []Message{
		{Role: "user", Content: []map[string]any{
			{"type": "text", "text": "regarde"},
			{"type": "image_url", "image_url": map[string]any{"url": url}},
		}},
		{Role: "assistant", Content: "vu"},
	}
	// Relu depuis le JSON (forme []any), comme après un redémarrage.
	var reread []Message
	b, _ := json.Marshal(msgs)
	_ = json.Unmarshal(b, &reread)

	for _, set := range [][]Message{msgs, reread} {
		if !hasInlineImages(set) || !refImagesInMessages(set) || hasInlineImages(set) {
			t.Fatal("l'image aurait dû être rangée par référence")
		}
		js, _ := json.Marshal(set)
		if len(js) > 500 || !strings.Contains(string(js), imgRefScheme) {
			t.Fatalf("historique encore lourd : %d octets", len(js))
		}
		out := expandImageRefs(set)
		_, got := partImageURL(contentParts(out[0].Content)[1])
		if got != url {
			t.Fatal("l'image renvoyée au modèle diffère de l'originale")
		}
		// L'historique lui-même n'est pas modifié par l'expansion.
		if _, u := partImageURL(contentParts(set[0].Content)[1]); !strings.HasPrefix(u, imgRefScheme) {
			t.Fatal("expandImageRefs ne doit pas toucher l'historique")
		}
	}
}

// Mémoire chiffrée : l'image rangée est chiffrée sur disque et revient intacte ;
// verrouillée, elle n'est pas écrite (elle reste en base64 dans le message).
func TestImageRefsChiffrees(t *testing.T) {
	testHome(t)
	clearMemDEK()
	if _, err := EnableMemEncryption("motdepasse-fort"); err != nil {
		t.Fatalf("EnableMemEncryption: %v", err)
	}
	raw := []byte(strings.Repeat("\xff\xd8JPEG-fake", 3000))
	ref, err := storeChatImage(raw, "image/jpeg")
	if err != nil {
		t.Fatalf("storeChatImage: %v", err)
	}
	disk, _ := os.ReadFile(filepath.Join(chatImgDir(), strings.TrimPrefix(ref, imgRefScheme)))
	if !looksEncrypted(disk) {
		t.Fatal("image en clair sur disque alors que la mémoire est chiffrée")
	}
	u, ok := chatImageDataURL(ref)
	if !ok || u != "data:image/jpeg;base64,"+base64.StdEncoding.EncodeToString(raw) {
		t.Fatal("image chiffrée non restituée à l'identique")
	}
	clearMemDEK()
	if _, err := storeChatImage([]byte("autre image"), "image/png"); err == nil {
		t.Fatal("mémoire verrouillée : l'image n'aurait pas dû être écrite")
	}
}

// Les images de plus de chatImgKeep sont effacées, les récentes restent.
func TestPruneChatImages(t *testing.T) {
	testHome(t)
	vieille, _ := storeChatImage([]byte("vieille"), "image/png")
	recente, _ := storeChatImage([]byte("récente"), "image/png")
	pv := filepath.Join(chatImgDir(), strings.TrimPrefix(vieille, imgRefScheme))
	old := time.Now().Add(-chatImgKeep - time.Hour)
	if err := os.Chtimes(pv, old, old); err != nil {
		t.Fatal(err)
	}
	pruneChatImages()
	if _, err := os.Stat(pv); !os.IsNotExist(err) {
		t.Fatal("la vieille image aurait dû être effacée")
	}
	if _, err := os.Stat(filepath.Join(chatImgDir(), strings.TrimPrefix(recente, imgRefScheme))); err != nil {
		t.Fatal("la récente image a disparu")
	}
}
