package loki

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// gpuSettle : le bilan « VRAM libérée » est calculé sur ces lectures. Chaque cas
// est un faux bilan qu'on a vu — ou qu'on aurait vu — dans l'interface.
func TestGpuSettle(t *testing.T) {
	seq := func(vals ...int) func() (int, bool) {
		i := 0
		return func() (int, bool) {
			if i >= len(vals) {
				return vals[len(vals)-1], true
			}
			v := vals[i]
			i++
			if v < 0 { // lecture ratée (nvidia-smi absent ou en réinitialisation)
				return 0, false
			}
			return v, true
		}
	}
	for _, c := range []struct {
		name string
		seq  []int
		want int
	}{
		// Le pilote met un temps à réagir : palier AVANT la baisse, puis la
		// mémoire part. L'ancienne version rendait 20000 dès le premier palier.
		{"palier avant la baisse", []int{20000, 20000, 12000, 3000, 500, 500}, 500},
		// Baisse immédiate et régulière, puis stable.
		{"baisse puis stable", []int{15000, 8000, 400, 400}, 400},
		// Une lecture ratée au milieu n'est pas « 0 Mo » : on la saute.
		{"lecture ratée ignorée", []int{14000, -1, 600, 600}, 600},
		// Toutes les lectures ratées : on rend « avant », donc 0 Mo libérés,
		// plutôt que d'annoncer 14 Go rendus.
		{"aucune lecture", []int{-1, -1, -1, -1, -1, -1, -1, -1}, 14000},
		// Rien ne baisse (moteur déjà arrêté) : dernière lecture valide.
		{"rien ne bouge", []int{14000, 14000, 14000, 14000, 14000, 14000, 14000, 14000}, 14000},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := gpuSettle(14000, seq(c.seq...), 0); got != c.want {
				t.Errorf("gpuSettle = %d, attendu %d", got, c.want)
			}
		})
	}
}

// Les deux routes arrêtent ou relancent des processus : un GET — une balise
// <img> sur une page tierce, sans clé de pilotage — ne doit rien déclencher.
func TestVramRoutesRefusentGET(t *testing.T) {
	for path, h := range map[string]http.HandlerFunc{
		"/api/vram/unload": handleVramUnload,
		"/api/vram/reload": handleVramReload,
	} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != 405 {
			t.Errorf("%s GET : code %d, attendu 405", path, rec.Code)
		}
		if rec.Header().Get("Allow") != "POST" {
			t.Errorf("%s GET : en-tête Allow %q, attendu POST", path, rec.Header().Get("Allow"))
		}
		if !strings.Contains(rec.Body.String(), "POST attendu") {
			t.Errorf("%s GET : corps %q sans explication", path, rec.Body.String())
		}
	}
}
