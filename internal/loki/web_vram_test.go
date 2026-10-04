package loki

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// gpuCache : les jauges de tous les onglets se partagent une lecture par
// fenêtre de deux secondes, même quand les requêtes arrivent en même temps.
func TestGpuCache(t *testing.T) {
	type step struct {
		at    time.Duration // horloge depuis le départ
		err   error         // erreur rendue si la requête part
		calls int32         // requêtes cumulées attendues après l'appel
	}
	absent := &exec.Error{Name: "nvidia-smi", Err: exec.ErrNotFound}
	for _, c := range []struct {
		name  string
		steps []step
	}{
		{"lecture réutilisée puis rafraîchie", []step{
			{0, nil, 1}, {time.Second, nil, 1}, {gpuCacheTTL, nil, 2},
		}},
		// Carte en réinitialisation : on retente au délai normal, pas plus tard.
		{"erreur retentée au délai normal", []step{
			{0, errors.New("exit status 9"), 1}, {time.Second, nil, 1}, {gpuCacheTTL, nil, 2},
		}},
		// Pas de nvidia-smi du tout : une recherche par minute suffit.
		{"binaire absent mémorisé", []step{
			{0, absent, 1}, {30 * time.Second, absent, 1}, {gpuCacheAbsent, absent, 2},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var cache gpuCache
			var calls int32
			t0 := time.Unix(1000, 0)
			for i, st := range c.steps {
				now := func() time.Time { return t0.Add(st.at) }
				cache.get(now, func() ([]gpuStat, error) {
					atomic.AddInt32(&calls, 1)
					if st.err != nil {
						return nil, st.err
					}
					return []gpuStat{{Name: "fake", Used: 1000}}, nil
				})
				if got := atomic.LoadInt32(&calls); got != st.calls {
					t.Fatalf("étape %d : %d requêtes, attendu %d", i, got, st.calls)
				}
			}
		})
	}

	t.Run("appels simultanés", func(t *testing.T) {
		var cache gpuCache
		var calls int32
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				g := cache.get(time.Now, func() ([]gpuStat, error) {
					atomic.AddInt32(&calls, 1)
					time.Sleep(20 * time.Millisecond) // nvidia-smi n'est pas instantané
					return []gpuStat{{Name: "fake", Used: 1000}}, nil
				})
				if len(g) != 1 || g[0].Used != 1000 {
					t.Errorf("lecture partagée %+v", g)
				}
			}()
		}
		wg.Wait()
		if calls != 1 {
			t.Errorf("%d nvidia-smi lancés, attendu 1", calls)
		}
	})
}

// Le bilan du déchargement doit voir la mémoire PARTIR : son échantillonneur
// ne passe jamais par le cache des jauges, même tout juste rempli.
func TestGpuSettleSampleFrais(t *testing.T) {
	orig := nvidiaSmiQuery
	t.Cleanup(func() { nvidiaSmiQuery = orig; gpuMonitor = gpuCache{} })
	var calls int32
	nvidiaSmiQuery = func() ([]byte, error) {
		n := atomic.AddInt32(&calls, 1)
		return []byte(fmt.Sprintf("Fake GPU, %d, 24000, 0, 40\n", 20000-int(n)*1000)), nil
	}
	gpuMonitor = gpuCache{}
	if g := gpuStatsCached(); len(g) != 1 || g[0].Used != 19000 {
		t.Fatalf("jauges : %+v", g)
	}
	for want := 18000; want >= 17000; want -= 1000 {
		if used, ok := gpuSettleSample(); !ok || used != want {
			t.Errorf("échantillon = %d (%v), attendu %d frais", used, ok, want)
		}
	}
	if g := gpuStatsCached(); g[0].Used != 19000 {
		t.Errorf("jauges : %d, attendu la lecture en cache 19000", g[0].Used)
	}
}

// Le cache ne mémorise une minute que l'ABSENCE de nvidia-smi : il faut donc que
// la vraie requête (bornée dans le temps) rende toujours exec.ErrNotFound quand
// le binaire n'est pas dans le PATH — sinon Mac et CPU seul relanceraient une
// recherche toutes les deux secondes.
func TestNvidiaSmiQueryAbsent(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := nvidiaSmiQuery(); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("erreur %v, attendu exec.ErrNotFound", err)
	}
}
