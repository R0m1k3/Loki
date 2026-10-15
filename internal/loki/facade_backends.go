package loki

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// facade_backends.go — backends llama.cpp tenus par la façade Rust
// (LOKI_BACKENDS=rust, lot 22 de la migration ; expérimental, le Go reste le
// défaut) : statut, vérification git, binaires précompilés, bascule fast/opt,
// suivi du job, backends custom et GPU vus par un moteur
// (rust/loki-rs/src/backends).
//
// La façade détient alors LE job des moteurs. Ceux que le Go fait encore
// tourner — compilation depuis les sources (install, update, install-custom,
// que la façade lui relaie), moteur de l'image, Strata — lui rapportent chaque
// étape (/internal/facade/v1/build-job/*) : lcAppend, lcPhase, lcFail, lcDone,
// startLcJob et lcJobSnapshot passent par elle, et le Go ne restaure plus de
// job au démarrage (la façade s'en charge).

const facadeBackendsEnv = "LOKI_BACKENDS"

// facadeBackends : la façade tient-elle les backends et le job ?
func facadeBackends() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(facadeBackendsEnv)), "rust") &&
		facadeAddr() != "" && internalToken() != ""
}

// facadeBackendsRoutes : les routes que la façade sert alors (backends/mod.rs).
var facadeBackendsRoutes = map[string]bool{
	"/api/llamacpp":                  true,
	"/api/llamacpp/check":            true,
	"/api/llamacpp/prebuilt":         true,
	"/api/llamacpp/prebuilt/check":   true,
	"/api/llamacpp/use":              true,
	"/api/llamacpp/job":              true,
	"/api/llamacpp/uninstall-custom": true,
	"/api/backends":                  true,
	"/api/backends/custom":           true,
	"/api/backends/devices":          true,
}

// facadeJobClient : appels courts (une ligne de journal, une phase).
var facadeJobClient = &http.Client{Timeout: 30 * time.Second}

// facadeJobCall envoie une étape du job à la façade. out peut être nil.
// Une réponse 409 rend l'erreur que la façade explique (job déjà en cours).
func facadeJobCall(op string, body map[string]any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+facadeAddr()+"/internal/facade/v1/build-job/"+op, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(internalTokenHeader, internalToken())
	resp, err := facadeJobClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("façade : %s", resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// facadeJobNote : une étape dont l'échec ne doit pas arrêter le job (le
// journal perd une ligne, rien de plus) — tracée sur la sortie d'erreur.
func facadeJobNote(op string, body map[string]any) {
	if err := facadeJobCall(op, body, nil); err != nil {
		fmt.Fprintf(os.Stderr, "[job] étape %s non transmise à la façade : %v\n", op, err)
	}
}

// facadeJobStart démarre le job côté façade, puis son corps ici.
func facadeJobStart(action string, run func()) error {
	if err := facadeJobCall("start", map[string]any{"action": action}, nil); err != nil {
		return err
	}
	setBuildSink(lcAppend)
	go func() {
		defer func() {
			setBuildSink(nil)
			facadeJobNote("end", map[string]any{})
		}()
		run()
	}()
	return nil
}

// facadeJobSnapshot : la vue du job tenue par la façade.
func facadeJobSnapshot(from int, withLines bool) map[string]any {
	lines := "0"
	if withLines {
		lines = "1"
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+facadeAddr()+
		"/internal/facade/v1/build-job/snapshot?from="+strconv.Itoa(from)+"&lines="+lines, nil)
	if err != nil {
		return map[string]any{"exists": false}
	}
	req.Header.Set(internalTokenHeader, internalToken())
	resp, err := facadeJobClient.Do(req)
	if err != nil {
		return map[string]any{"exists": false}
	}
	defer resp.Body.Close()
	var out map[string]any
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&out) != nil {
		return map[string]any{"exists": false}
	}
	return out
}

// lcSetCommit note le commit d'avant ("old") ou d'après ("new") une mise à
// jour.
func lcSetCommit(which, commit string) {
	if facadeBackends() {
		facadeJobNote("commits", map[string]any{which: commit})
		return
	}
	lcMu.Lock()
	if lcCur != nil {
		if which == "old" {
			lcCur.OldCommit = commit
		} else {
			lcCur.NewCommit = commit
		}
	}
	lcMu.Unlock()
}
