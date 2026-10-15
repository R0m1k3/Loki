package loki

import (
	"errors"
	"io"
	"net/http"
)

// API interne de chiffrement (lot 6 de la migration Rust).
//
// Les discussions sont chiffrées en base quand la mémoire l'est
// (memEncoderNow / decodeMemContent). La clé de données (DEK) ne vit qu'en
// mémoire de ce process, après déverrouillage : une conversation tenue par la
// façade Rust fait donc sceller et ouvrir ses blobs ICI, la clé ne sort jamais.
//
//	POST /internal/v1/crypt/seal   corps clair   → 200 corps scellé (ou clair si
//	                                                le chiffrement est coupé)
//	POST /internal/v1/crypt/open   corps brut    → 200 corps clair (un contenu
//	                                                non chiffré passe tel quel)
//
// Mémoire verrouillée : 423 (Locked) — l'appelant ne doit ni écrire en clair
// ni perdre la donnée, il attend le déverrouillage comme le Go le fait.

const internalCryptMaxBody = 256 << 20

func handleInternalCrypt(w http.ResponseWriter, r *http.Request, seal bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		sendJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST attendu"})
		return
	}
	in, err := io.ReadAll(io.LimitReader(r.Body, internalCryptMaxBody+1))
	if err != nil || len(in) > internalCryptMaxBody {
		sendJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "corps trop grand ou illisible"})
		return
	}
	var out []byte
	if seal {
		var enc func([]byte) ([]byte, error)
		if enc, err = memEncoderNow(); err == nil {
			out, err = enc(in)
		}
	} else {
		out, err = decodeMemContent(in)
	}
	if errors.Is(err, errMemLocked) {
		sendJSON(w, http.StatusLocked, map[string]any{"error": err.Error()})
		return
	}
	if err != nil {
		sendJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(out)
}
