package loki

// web_hf.go — routes de recherche de modèles sur Hugging Face.
//
//	GET /api/hf/search?q=…              dépôts GGUF correspondants
//	GET /api/hf/files?repo=auteur/dépôt modèles, projecteurs et drafts du dépôt
//	GET|POST /api/hf/token              jeton Hugging Face (dépôts verrouillés)
//
// Les deux renvoient aussi le matériel local, pour que l'interface n'ait pas à
// le demander séparément et n'affiche jamais une liste de tailles sans le
// budget en face.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

func handleHFSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		sendJSON(w, 400, map[string]any{"ok": false, "error": "précise ce que tu cherches"})
		return
	}
	repos, err := hfSearch(r.Context(), q)
	if err != nil {
		sendJSON(w, 502, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	sendJSON(w, 200, map[string]any{"ok": true, "hardware": detectHardware(), "repos": repos, "hf_token": hfTokenSet()})
}

// handleHFFiles liste les .gguf d'un dépôt, chaque modèle portant son verdict
// « ça tient / ça ne tient pas ».
//
// Le verdict d'un modèle inclut le projecteur QUE si le dépôt en publie un : sur
// un dépôt sans vision, compter un projecteur inexistant ferait basculer à tort
// des modèles en « trop ». Le contexte pris en compte est celui du preset actif
// (CTX), pas une constante : c'est lui qui sera réellement lancé.
func handleHFFiles(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	if repo == "" {
		sendJSON(w, 400, map[string]any{"ok": false, "error": "dépôt manquant"})
		return
	}
	list, err := hfFiles(r.Context(), repo)
	if err != nil {
		sendJSON(w, 502, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	hw := detectHardware()

	// Contexte du preset actif, avec le même défaut que backend_serve.go — sinon
	// le verdict est calculé sur un contexte que personne n'utilisera.
	ctxTokens := 32768
	if v, convErr := strconv.Atoi(strings.TrimSpace(ReadConfig()["CTX"])); convErr == nil && v > 0 {
		ctxTokens = v
	}

	var mmSize int64
	if p, ok := hfPickProjector(list.Projectors); ok {
		mmSize = p.Size
	}
	for i := range list.Models {
		list.Models[i].Verdict, list.Models[i].Why = fitVerdict(hw, list.Models[i].Size, mmSize, ctxTokens)
	}
	// `vision` sort d'ici et de nulle part ailleurs : c'est la présence d'un
	// mmproj dans CE dépôt, pas un tag Hugging Face — lequel manque sur des
	// dépôts qui en publient pourtant un.
	// Un projecteur ne reçoit pas de verdict : il ne se charge jamais seul.
	// `gated` et `hf_token` servent le même but que `vision` : dire AVANT le
	// téléchargement ce qui va se passer. Un dépôt verrouillé se lit sans jeton
	// mais ne se télécharge pas — l'avertir ici évite de choisir un quant, de
	// lancer 15 Go et de récolter un 401.
	sendJSON(w, 200, map[string]any{
		"ok": true, "hardware": hw, "ctx": ctxTokens,
		"repo": list.Repo, "models": list.Models,
		"projectors": list.Projectors, "drafts": list.Drafts,
		"vision":   len(list.Projectors) > 0,
		"gated":    list.Gated,
		"hf_token": hfTokenSet(),
	})
}

// handleHFToken lit et enregistre le jeton Hugging Face.
//
//	GET  → {ok, set, source:"config"|"env"|"", masked, account}
//	POST {token:"hf_…"} → vérifie le jeton PUIS l'enregistre
//	POST {token:""}     → efface le jeton enregistré
//
// Le jeton n'est jamais renvoyé en clair : il ne sert qu'ici, personne n'a
// besoin de le recopier depuis Loki. Et il est vérifié AVANT d'être enregistré —
// un jeton mal collé qu'on accepte silencieusement redonne exactement le 401
// qu'on cherchait à expliquer.
func handleHFToken(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			sendJSON(w, 400, map[string]any{"ok": false, "error": "requête illisible"})
			return
		}
		tok := strings.TrimSpace(req.Token)
		account := ""
		if tok != "" {
			var err error
			if account, err = hfWhoAmI(r.Context(), tok); err != nil {
				sendJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		}
		if err := writeHFToken(tok); err != nil {
			sendJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		sendJSON(w, 200, hfTokenState(account))
		return
	}
	sendJSON(w, 200, hfTokenState(""))
}

// hfTokenState décrit le jeton pour l'interface. account n'est renseigné qu'au
// retour d'un enregistrement : le vérifier à chaque affichage ferait un appel
// sortant à l'ouverture de la fenêtre des réglages.
func hfTokenState(account string) map[string]any {
	src := hfTokenSource()
	return map[string]any{
		"ok": true, "set": src != "", "source": src,
		"masked": maskHFToken(hfToken()), "account": account,
	}
}
