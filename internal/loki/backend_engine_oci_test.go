package loki

// Mise à jour du moteur depuis l'image officielle llama.cpp.
//
// Ce qui se joue ici n'est pas « est-ce qu'on télécharge » mais « est-ce qu'on
// télécharge le MINIMUM et est-ce que ce qu'on pose est LANÇABLE ». Deux pièges
// concrets, tous deux invisibles sans test :
//
//   - l'image officielle range llama-server dans une couche et ses .so dans la
//     précédente ; s'arrêter au binaire donne un moteur qui meurt sur
//     « libllama.so: cannot open shared object file » ;
//   - la couche du dessous est le runtime CUDA (2 Go). La descendre par erreur,
//     c'est transformer une mise à jour de 170 Mo en un téléchargement de 2 Go.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- Faux registre OCI ------------------------------------------------------

// fauxCouche fabrique une couche tar.gz. Les entrées sont décrites par leur nom
// complet dans l'archive ; une valeur préfixée par « -> » crée un lien.
func fauxCouche(t *testing.T, entrees map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for nom, contenu := range entrees {
		if cible, ok := strings.CutPrefix(contenu, "-> "); ok {
			if err := tw.WriteHeader(&tar.Header{Name: nom, Typeflag: tar.TypeSymlink, Linkname: cible, Mode: 0o777}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Name: nom, Typeflag: tar.TypeReg, Size: int64(len(contenu)), Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(contenu)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fauxRegistre sert un index multi-architecture, le manifeste amd64 et les
// blobs. `tailles` permet d'annoncer une taille différente de la taille réelle
// (c'est ce que lit le garde-fou des grosses couches). Renvoie le compteur des
// blobs réellement téléchargés.
func fauxRegistre(t *testing.T, version string, couches [][]byte, tailles []int64) (servis map[string]int) {
	t.Helper()
	servis = map[string]int{}
	digests := make([]string, len(couches))
	blobs := map[string][]byte{}
	for i, c := range couches {
		sum := sha256.Sum256(c)
		digests[i] = "sha256:" + hex.EncodeToString(sum[:])
		blobs[digests[i]] = c
	}
	manifeste := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"layers": func() []map[string]any {
			var out []map[string]any
			for i := range couches {
				taille := int64(len(couches[i]))
				if tailles != nil && tailles[i] > 0 {
					taille = tailles[i]
				}
				out = append(out, map[string]any{
					"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
					"digest":    digests[i], "size": taille,
				})
			}
			return out
		}(),
	}
	manBytes, _ := json.Marshal(manifeste)
	manSum := sha256.Sum256(manBytes)
	manDigest := "sha256:" + hex.EncodeToString(manSum[:])

	index := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []map[string]any{
			// L'architecture qui ne nous concerne pas vient EN PREMIER : choisir
			// « le premier manifeste » au lieu de filtrer sur la plateforme
			// installerait un moteur d'une autre architecture.
			{"digest": "sha256:mauvaise", "platform": map[string]string{"os": "linux", "architecture": "ppc64le"}},
			{"digest": manDigest, "platform": map[string]string{"os": "linux", "architecture": ociArch()}},
		},
		"annotations": map[string]string{"org.opencontainers.image.version": version},
	}
	indexBytes, _ := json.Marshal(index)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/token"):
			_, _ = w.Write([]byte(`{"token":"jeton-de-test"}`))
		case strings.Contains(r.URL.Path, "/manifests/"):
			if r.Header.Get("Authorization") != "Bearer jeton-de-test" {
				w.WriteHeader(401)
				return
			}
			ref := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			if ref == manDigest {
				_, _ = w.Write(manBytes)
				return
			}
			_, _ = w.Write(indexBytes)
		case strings.Contains(r.URL.Path, "/blobs/"):
			ref := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			b, ok := blobs[ref]
			if !ok {
				w.WriteHeader(404)
				return
			}
			servis[ref]++
			_, _ = w.Write(b)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)

	ancien := ociBase
	ociBase = srv.URL
	t.Cleanup(func() { ociBase = ancien })
	return servis
}

// --- Tests ------------------------------------------------------------------

// Le cas réel : trois couches utiles au-dessus d'un runtime de 2 Go. On doit
// prendre le binaire ET les bibliothèques, résoudre les liens de version, et
// ne JAMAIS descendre jusqu'au runtime.
func TestInstallationMoteurDepuisImage(t *testing.T) {
	testHome(t)
	couches := [][]byte{
		fauxCouche(t, map[string]string{"usr/local/cuda/lib64/libcudart.so": "runtime CUDA"}), // 2 Go annoncés
		fauxCouche(t, map[string]string{
			"app/libggml-base.so":        "-> libggml-base.so.0",
			"app/libggml-base.so.0":      "-> libggml-base.so.0.20.0",
			"app/libggml-base.so.0.20.0": "binaire ELF",
			"app/libllama.so":            "-> libllama.so.0",
			"app/libllama.so.0":          "-> libllama.so.0.1.0",
			"app/libllama.so.0.1.0":      "binaire ELF",
			"app/libggml-cuda.so":        "binaire ELF",
		}),
		fauxCouche(t, map[string]string{"app/llama-server": "#!/bin/true\n", "app/llama": "#!/bin/true\n"}),
		fauxCouche(t, map[string]string{"etc/passwd": "sans rapport"}),
	}
	servis := fauxRegistre(t, "b10450", couches, []int64{2 << 30, 0, 0, 0})

	bin, err := engineOCIInstall("server-cuda-b10450", func(string) {}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if veut := filepath.Join(engineDir(), "server-cuda-b10450", "llama-server"); bin != veut {
		t.Errorf("binaire installé en %s, attendu %s", bin, veut)
	}

	// La couche du runtime CUDA ne doit pas avoir été demandée : c'est tout
	// l'intérêt (170 Mo au lieu de 2,6 Go).
	sum := sha256.Sum256(couches[0])
	if n := servis["sha256:"+hex.EncodeToString(sum[:])]; n != 0 {
		t.Errorf("la couche du runtime CUDA a été téléchargée %d fois", n)
	}

	dir := filepath.Dir(bin)
	for _, nom := range []string{"llama-server", "llama", "libggml-cuda.so", "libggml-base.so.0.20.0", "libllama.so.0.1.0"} {
		if !isFile(filepath.Join(dir, nom)) {
			t.Errorf("%s manquant", nom)
		}
	}
	// Les noms courts sont ceux que l'éditeur de liens cherche au lancement :
	// sans eux le moteur ne démarre pas, alors que tout « a l'air » installé.
	for _, lien := range []string{"libggml-base.so", "libggml-base.so.0", "libllama.so", "libllama.so.0"} {
		p := filepath.Join(dir, lien)
		if _, err := os.Stat(p); err != nil { // Stat suit le lien : vérifie aussi qu'il ne pend pas
			t.Errorf("%s : %v", lien, err)
		}
	}
	// /app à plat, et rien d'autre que /app.
	if isDir(filepath.Join(dir, "etc")) || isDir(filepath.Join(dir, "usr")) {
		t.Error("des fichiers hors /app ont été extraits")
	}
	if st, err := os.Stat(bin); err != nil || st.Mode()&0o100 == 0 {
		t.Errorf("llama-server non exécutable : %v", err)
	}

	// Réinstaller la même version ne redemande RIEN au registre : le job de mise
	// à jour repasse par là à chaque vérification.
	total := func() int {
		n := 0
		for _, v := range servis {
			n += v
		}
		return n
	}
	avant := total()
	if _, err := engineOCIInstall("server-cuda-b10450", func(string) {}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if total() != avant {
		t.Errorf("une version déjà installée a été re-téléchargée (%d → %d couches)", avant, total())
	}

	// Et la version se retrouve dans la liste des moteurs installés.
	if got := engineInstalled(); len(got) != 1 || got[0] != "server-cuda-b10450" {
		t.Errorf("versions installées = %v", got)
	}
	if engineTagBuild("server-cuda-b10450") != 10450 {
		t.Error("numéro de build mal extrait du tag")
	}
	if !engineOwns(bin) {
		t.Error("le binaire installé n'est pas reconnu comme géré par Loki")
	}
}

// Une variante dont l'image ne contient pas de llama-server (mauvais tag, image
// « full » sans serveur) doit échouer CLAIREMENT, et ne rien laisser derrière
// elle qui ressemblerait à un moteur installé.
func TestInstallationRefuseUneImageSansMoteur(t *testing.T) {
	testHome(t)
	fauxRegistre(t, "b10450", [][]byte{
		fauxCouche(t, map[string]string{"app/README": "pas de moteur ici"}),
	}, nil)

	if _, err := engineOCIInstall("server-b1", func(string) {}, func(string) {}); err == nil {
		t.Fatal("installation acceptée sans llama-server")
	}
	if got := engineInstalled(); len(got) != 0 {
		t.Errorf("versions installées après échec = %v", got)
	}
}

// engineOCILatest lit le numéro de build dans l'annotation de l'image, et en
// déduit le tag figé — c'est lui qu'on installe, pour que « mis à jour vers »
// désigne une version précise et reproductible plutôt qu'un tag mouvant.
func TestDerniereVersionPubliee(t *testing.T) {
	fauxRegistre(t, "b10450", [][]byte{fauxCouche(t, map[string]string{"app/llama-server": "x"})}, nil)
	build, tag, err := engineOCILatest("server-cuda")
	if err != nil {
		t.Fatal(err)
	}
	if build != 10450 || tag != "server-cuda-b10450" {
		t.Errorf("build=%d tag=%q", build, tag)
	}
}

// La bannière du moteur est la seule source de vérité sur la version qui tourne
// vraiment — le tag de l'image, lui, ment dès qu'on a basculé de moteur.
func TestLectureVersionMoteur(t *testing.T) {
	for _, c := range []struct {
		nom, sortie string
		build       int
		commit      string
	}{
		// Relevée sur le binaire réel de l'image server-cuda-b10450.
		{"forme actuelle", "version: 0.1.0-dev (build 10450, commit ece963f41)\nbuilt with GNU 14.2.0 for Linux x86_64\n", 10450, "ece963f41"},
		{"forme précédente", "version: 10450 (ece963f)\nbuilt with cc\n", 10450, "ece963f"},
		{"forme ancienne", "build: 4589 (a1b2c3d)\n", 4589, "a1b2c3d"},
		{"bannière de démarrage", "ggml_cuda_init: found 1 device\nversion: 9001 (deadbee)\n", 9001, "deadbee"},
		{"rien d'exploitable", "usage: llama-server [options]\n", 0, ""},
	} {
		t.Run(c.nom, func(t *testing.T) {
			build, commit := parseEngineBuild(c.sortie)
			if build != c.build || commit != c.commit {
				t.Errorf("= (%d, %q), attendu (%d, %q)", build, commit, c.build, c.commit)
			}
		})
	}
}

// Le filtre des entrées d'archive : on ne veut QUE les fichiers directs de /app.
// Un sous-dossier ou un marqueur de suppression OCI recopié à plat produirait
// des fichiers parasites à côté du binaire.
func TestFiltreDesEntreesApp(t *testing.T) {
	for _, c := range []struct {
		entree, veut string
		ok           bool
	}{
		{"app/llama-server", "llama-server", true},
		{"./app/libllama.so", "libllama.so", true},
		{"app/", "", false},
		{"app/tools/quantize", "", false},
		{"app/.wh.libggml-cuda.so", "", false},
		{"usr/local/cuda/lib64/libcudart.so", "", false},
		{"apparence/piege", "", false},
	} {
		got, ok := appEntryName(c.entree)
		if ok != c.ok || got != c.veut {
			t.Errorf("%q → (%q, %v), attendu (%q, %v)", c.entree, got, ok, c.veut, c.ok)
		}
	}
}

// Le binaire seul ne fait pas un moteur : c'est la règle d'arrêt de la descente
// des couches.
func TestMoteurCompletExigeLesBibliotheques(t *testing.T) {
	vu := func(noms ...string) map[string]bool {
		m := map[string]bool{}
		for _, n := range noms {
			m[n] = true
		}
		return m
	}
	if engineSetComplete(vu("llama-server")) {
		t.Error("binaire seul accepté comme moteur complet")
	}
	if engineSetComplete(vu("llama-server", "libggml-base.so.0.20.0")) {
		t.Error("moteur sans libllama accepté")
	}
	if !engineSetComplete(vu("llama-server", "libggml-base.so.0.20.0", "libllama.so.0.1.0")) {
		t.Error("moteur complet refusé")
	}
}

// La variante se déduit des backends posés à côté du binaire : c'est ce qui
// évite de proposer une image CUDA à une installation Vulkan (et l'inverse).
func TestVarianteDeduiteDesBackends(t *testing.T) {
	for _, c := range []struct{ backend, veut string }{
		{"libggml-cuda.so", "server-cuda"},
		{"libggml-vulkan.so", "server-vulkan"},
		{"libggml-sycl.so", "server-intel"},
		{"libggml-musa.so", "server-musa"},
		{"", "server"},
	} {
		t.Run(c.veut, func(t *testing.T) {
			home := testHome(t)
			dir := filepath.Join(home, "faux-moteur")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(dir, "llama-server")
			if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
				t.Fatal(err)
			}
			if c.backend != "" {
				if err := os.WriteFile(filepath.Join(dir, c.backend), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := SetConfigKey("BIN", bin); err != nil {
				t.Fatal(err)
			}
			if got := engineVariant(); got != c.veut {
				t.Errorf("variante = %q, attendu %q", got, c.veut)
			}
		})
	}
}

// Le ménage garde la version demandée et efface les autres : sans lui, chaque
// mise à jour laisse ~450 Mo sur le volume de données.
func TestMenageDesAnciennesVersions(t *testing.T) {
	testHome(t)
	for _, tag := range []string{"server-cuda-b1", "server-cuda-b2", "server-cuda-b3"} {
		d := filepath.Join(engineDir(), tag)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "llama-server"), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	enginePrune(map[string]bool{"server-cuda-b3": true}, nil)
	got := engineInstalled()
	if fmt.Sprint(got) != "[server-cuda-b3]" {
		t.Errorf("après ménage : %v", got)
	}
}
