# Moteur de dictée supervisé et configurable — plan d'implémentation

> **Pour les agents :** SOUS-SKILL REQUISE — utiliser `superpowers:subagent-driven-development` (recommandé) ou `superpowers:executing-plans` pour dérouler ce plan tâche par tâche. Les étapes utilisent des cases à cocher (`- [ ]`).

**But :** remplacer les lancements de `whisper-cli` par un `whisper-server` supervisé, choisi et configuré depuis un panneau Dictée — modèle, GPU, langue.

**Architecture :** Loki lance `whisper-server` à la demande sur un port local, garde le modèle chargé, et l'éteint après inactivité. Le choix du GPU passe par `CUDA_VISIBLE_DEVICES`. Deux binaires sont livrés (CPU et CUDA) parce que l'image runtime peut être une image sans CUDA. `POST /api/transcribe` conserve son contrat et devient un relais vers ce serveur.

**Pile :** Go (stdlib + `net/http`), whisper.cpp, JavaScript sans framework, magasin clé/valeur `bkState`.

**Spec :** `docs/superpowers/specs/2026-08-21-dictee-temps-reel-design.md`

**Portée :** ce plan couvre les sections 1 et 3 de la spec. Le temps réel (WebSocket + découpeur, section 2) fait l'objet d'un second plan, et n'est PAS implémenté ici. À la fin de ce plan, la dictée reste en un coup — mais rapide, avec un bon modèle, sur le GPU choisi.

## Contraintes globales

- **La CI compile pour Windows** (`GOOS=windows go build ./cmd/loki`). Aucun appel spécifique à Unix dans du code non suffixé `_linux.go`/`_unix.go`. En particulier : pas de `syscall.SysProcAttr{Setsid: true}`.
- **`staticcheck` doit rester à zéro avertissement.** Pas de variable inutilisée, pas d'erreur ignorée sans `_ =`.
- **`gofmt -l .` doit être vide.**
- **`internal/loki/ui/index.html` est GÉNÉRÉ et COMMITÉ.** Après toute modification de `ui/src/`, lancer `go run ./tools/assemble-ui` et commiter le résultat.
- **Les drapeaux de portabilité de whisper.cpp sont obligatoires sur toutes les cibles :** `-DGGML_NATIVE=OFF -DGGML_AVX512=OFF -DGGML_AMX_TILE=OFF -DGGML_AMX_INT8=OFF -DGGML_AMX_BF16=OFF`. Leur absence a causé un SIGILL en production (PR #18).
- **Les modèles ne vont jamais dans l'image Docker.** Ils vivent dans `LokiHome()/whisper/`.
- **Langue par défaut : `fr`.** Pas `auto`.
- **Commentaires et messages d'erreur en français**, comme le reste du dépôt.

## Fichiers

| Fichier | Responsabilité |
|---|---|
| `internal/loki/dictate_config.go` (créer) | Réglages de dictée : structure, lecture/écriture dans `bkState`, valeurs par défaut |
| `internal/loki/dictate_models.go` (créer) | Catalogue des modèles whisper, chemins, téléchargement |
| `internal/loki/dictate_server.go` (créer) | Supervision de `whisper-server` : lancement, attente, extinction sur inactivité, appel `/inference` |
| `internal/loki/web_transcribe.go` (modifier) | `/api/transcribe` relaie vers le serveur ; nouveaux endpoints de config et de modèles |
| `internal/loki/web_server.go` (modifier) | Enregistrement des nouvelles routes |
| `internal/loki/ui/src/index.tmpl.html` (modifier) | Panneau `data-pane="dictee"` + entrée de navigation |
| `internal/loki/ui/src/js/23-dictate.js` (modifier) | Test du micro, messages d'erreur |
| `internal/loki/ui/src/js/24-dictate-settings.js` (créer) | Chargement/enregistrement du panneau Dictée |
| `Dockerfile` (modifier) | Deux cibles `whisper-server` : CPU et CUDA |
| `internal/loki/web_transcribe_test.go` (modifier) | Garde-fou Dockerfile étendu |

---

### Tâche 1 : Les réglages de dictée

**Fichiers :**
- Créer : `internal/loki/dictate_config.go`
- Test : `internal/loki/dictate_config_test.go`

**Interfaces :**
- Consomme : `getStr(bucket, key string) string` et `putStr(bucket, key, val string) error` de `store.go` ; la constante `bkState = "state"`.
- Produit :
  - `type DictateCfg struct { Model, Lang, Device, Reactivity string }`
  - `func dictateCfgLoad() DictateCfg`
  - `func dictateCfgSave(c DictateCfg) error`
  - `func (c DictateCfg) cudaVisibleDevices() string`
  - `func (c DictateCfg) chunkBounds() (min, max time.Duration)`

`Device` vaut `"cpu"` ou l'indice GPU en décimal (`"0"`, `"1"`). `Reactivity` vaut `"court"`, `"moyen"` ou `"long"`. Les bornes de `chunkBounds` ne servent pas dans ce plan — elles sont posées ici parce que le réglage est stocké ici, et consommées par le plan « temps réel ».

- [ ] **Étape 1 : Écrire le test qui échoue**

Créer `internal/loki/dictate_config_test.go` :

```go
package loki

import (
	"testing"
	"time"
)

func TestDictateCfgDefauts(t *testing.T) {
	c := DictateCfg{}.withDefauts()
	if c.Model != "large-v3-turbo-q5_0" {
		t.Errorf("modèle par défaut = %q, attendu large-v3-turbo-q5_0", c.Model)
	}
	if c.Lang != "fr" {
		t.Errorf("langue par défaut = %q, attendu fr — la détection auto se trompe sur les tranches courtes", c.Lang)
	}
	if c.Device != "cpu" {
		t.Errorf("matériel par défaut = %q, attendu cpu (aucun GPU supposé)", c.Device)
	}
	if c.Reactivity != "moyen" {
		t.Errorf("réactivité par défaut = %q, attendu moyen", c.Reactivity)
	}
}

func TestCudaVisibleDevices(t *testing.T) {
	cas := []struct{ device, want string }{
		{"cpu", ""},
		{"0", "0"},
		{"1", "1"},
		{"", ""},        // vide = pas de GPU
		{"bidon", ""},   // valeur illisible : CPU plutôt qu'un GPU au hasard
		{"-1", ""},      // indice négatif : refusé
	}
	for _, c := range cas {
		got := DictateCfg{Device: c.device}.cudaVisibleDevices()
		if got != c.want {
			t.Errorf("cudaVisibleDevices(%q) = %q, attendu %q", c.device, got, c.want)
		}
	}
}

func TestChunkBounds(t *testing.T) {
	cas := []struct {
		react   string
		min, max time.Duration
	}{
		{"court", 1000 * time.Millisecond, 5 * time.Second},
		{"moyen", 1500 * time.Millisecond, 8 * time.Second},
		{"long", 3 * time.Second, 15 * time.Second},
		{"inconnu", 1500 * time.Millisecond, 8 * time.Second},
	}
	for _, c := range cas {
		min, max := DictateCfg{Reactivity: c.react}.chunkBounds()
		if min != c.min || max != c.max {
			t.Errorf("chunkBounds(%q) = %v/%v, attendu %v/%v", c.react, min, max, c.min, c.max)
		}
	}
}

func TestDictateCfgAllerRetour(t *testing.T) {
	withTestStore(t)
	in := DictateCfg{Model: "medium-q5_0", Lang: "en", Device: "1", Reactivity: "long"}
	if err := dictateCfgSave(in); err != nil {
		t.Fatalf("enregistrement : %v", err)
	}
	out := dictateCfgLoad()
	if out != in {
		t.Errorf("relu %+v, attendu %+v", out, in)
	}
}

func TestDictateCfgSaveRefuseValeursInvalides(t *testing.T) {
	withTestStore(t)
	err := dictateCfgSave(DictateCfg{Model: "modele-inexistant", Lang: "fr", Device: "cpu", Reactivity: "moyen"})
	if err == nil {
		t.Error("un modèle hors catalogue doit être refusé : sinon whisper-server ne démarrera jamais et l'erreur n'apparaîtra qu'au premier clic sur le micro")
	}
}
```

`withTestStore(t)` est le helper existant du paquet qui ouvre un magasin temporaire. **Avant d'écrire ce test, vérifier son nom réel** :

```bash
grep -rn "func withTestStore\|func testStore\|func openTestDB" internal/loki/*_test.go
```

Si le helper porte un autre nom, utiliser celui-là. S'il n'en existe aucun, en écrire un dans ce fichier de test :

```go
func withTestStore(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LOKI_HOME", dir)
	if err := openStore(); err != nil {
		t.Fatalf("ouverture du magasin de test : %v", err)
	}
	t.Cleanup(func() { _ = closeStore() })
}
```

en adaptant `openStore`/`closeStore` aux noms réels trouvés dans `store.go`.

- [ ] **Étape 2 : Lancer le test et vérifier qu'il échoue**

```bash
go test ./internal/loki/ -run 'TestDictateCfg|TestCudaVisibleDevices|TestChunkBounds' -v
```

Attendu : ÉCHEC de compilation, `undefined: DictateCfg`.

- [ ] **Étape 3 : Écrire l'implémentation**

Créer `internal/loki/dictate_config.go` :

```go
// Réglages de la dictée vocale — modèle, langue, matériel, réactivité.
//
// Ils vivent dans le magasin clé/valeur (bkState), comme le jeton Hugging
// Face : ce sont des réglages de SERVEUR, pas de navigateur. Le modèle chargé
// et le GPU occupé sont des propriétés de la machine, identiques pour tous les
// onglets ouverts.
package loki

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

const dictateCfgKey = "dictate_cfg"

// DictateCfg : Device vaut "cpu" ou un indice de GPU en décimal ("0", "1"),
// dans l'ordre de nvidia-smi — le même que celui affiché par /api/vram.
type DictateCfg struct {
	Model      string `json:"model"`
	Lang       string `json:"lang"`
	Device     string `json:"device"`
	Reactivity string `json:"reactivity"`
}

// withDefauts comble les champs vides. Appliqué à la LECTURE plutôt qu'à
// l'écriture : une version future qui ajoute un champ trouvera les réglages
// déjà enregistrés sans champ correspondant, et doit pouvoir le combler.
func (c DictateCfg) withDefauts() DictateCfg {
	if c.Model == "" {
		// large-v3-turbo : meilleur rapport qualité/vitesse en français à ce
		// jour. Le défaut penche vers la qualité, comme demandé.
		c.Model = "large-v3-turbo-q5_0"
	}
	if c.Lang == "" {
		// Pas "auto" : sur des tranches de quelques secondes la détection se
		// trompe, et une langue mal détectée produit du charabia — des suites
		// de caractères géorgiens observées en production.
		c.Lang = "fr"
	}
	if c.Device == "" {
		c.Device = "cpu"
	}
	if c.Reactivity == "" {
		c.Reactivity = "moyen"
	}
	return c
}

func dictateCfgLoad() DictateCfg {
	var c DictateCfg
	if raw := getStr(bkState, dictateCfgKey); raw != "" {
		_ = json.Unmarshal([]byte(raw), &c)
	}
	return c.withDefauts()
}

func dictateCfgSave(c DictateCfg) error {
	c = c.withDefauts()
	// Valider ICI plutôt qu'au démarrage du serveur : un modèle hors catalogue
	// accepté en silence ne se manifesterait qu'au premier clic sur le micro,
	// loin du geste qui l'a causé.
	if _, ok := whisperCatalogue[c.Model]; !ok {
		return fmt.Errorf("modèle de dictée inconnu : %s", c.Model)
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return putStr(bkState, dictateCfgKey, string(b))
}

// cudaVisibleDevices traduit le réglage en valeur de CUDA_VISIBLE_DEVICES.
// Vide = aucun GPU visible = whisper tourne sur le CPU. Toute valeur qui n'est
// pas un indice positif retombe sur le CPU : mieux vaut une dictée lente qu'un
// GPU choisi au hasard sur une machine dont on ne sait rien.
func (c DictateCfg) cudaVisibleDevices() string {
	n, err := strconv.Atoi(c.Device)
	if err != nil || n < 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// chunkBounds : réserve minimale avant de couper, et coupure forcée quand
// aucun silence n'arrive. Consommé par le découpeur du plan « temps réel ».
func (c DictateCfg) chunkBounds() (time.Duration, time.Duration) {
	switch c.Reactivity {
	case "court":
		return 1000 * time.Millisecond, 5 * time.Second
	case "long":
		return 3 * time.Second, 15 * time.Second
	default:
		return 1500 * time.Millisecond, 8 * time.Second
	}
}
```

- [ ] **Étape 4 : Lancer le test et vérifier qu'il passe**

```bash
go test ./internal/loki/ -run 'TestDictateCfg|TestCudaVisibleDevices|TestChunkBounds' -v
```

Attendu : PASS sur les cinq tests. `TestDictateCfgSaveRefuseValeursInvalides` dépend de `whisperCatalogue`, défini à la tâche 2 — **si la compilation échoue sur `undefined: whisperCatalogue`, faire la tâche 2 d'abord puis revenir valider cette étape.**

- [ ] **Étape 5 : Commiter**

```bash
go run honnef.co/go/tools/cmd/staticcheck@latest ./internal/loki/
gofmt -l internal/loki/
git add internal/loki/dictate_config.go internal/loki/dictate_config_test.go
git commit -m "Dictée : réglages serveur (modèle, langue, matériel, réactivité)"
```

---

### Tâche 2 : Le catalogue des modèles et leur téléchargement

**Fichiers :**
- Créer : `internal/loki/dictate_models.go`
- Test : `internal/loki/dictate_models_test.go`
- Modifier : `internal/loki/web_transcribe.go` (retirer l'ancien mono-modèle)

**Interfaces :**
- Consomme : `LokiHome() string` ; `DictateCfg` de la tâche 1.
- Produit :
  - `var whisperCatalogue map[string]whisperModel`
  - `type whisperModel struct { Nom, Fichier string; Octets int64; VRAMMo int }`
  - `func whisperModelPathFor(id string) string`
  - `func whisperModelURLFor(id string) string`
  - `func whisperModelPresent(id string) bool`
  - `func whisperCatalogueTrie() []map[string]any`

L'ancien `whisperModelPath()` sans argument et l'ancienne constante `whisperModelURL` disparaissent : ils codaient `small-q5_1` en dur.

- [ ] **Étape 1 : Écrire le test qui échoue**

Créer `internal/loki/dictate_models_test.go` :

```go
package loki

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCatalogueContientLeDefautEtLAncien(t *testing.T) {
	// Le défaut doit exister, sinon dictateCfgSave refuse sa propre valeur
	// par défaut et plus aucun réglage n'est enregistrable.
	if _, ok := whisperCatalogue["large-v3-turbo-q5_0"]; !ok {
		t.Error("le modèle par défaut large-v3-turbo-q5_0 est absent du catalogue")
	}
	// small-q5_1 est le modèle déjà téléchargé sur les installations
	// existantes : le retirer du catalogue le rendrait insélectionnable alors
	// que son fichier est là.
	if _, ok := whisperCatalogue["small-q5_1"]; !ok {
		t.Error("small-q5_1 est absent : les installations existantes l'ont déjà sur disque")
	}
}

func TestModelPathEtURL(t *testing.T) {
	t.Setenv("LOKI_HOME", t.TempDir())
	p := whisperModelPathFor("small-q5_1")
	if filepath.Base(p) != "ggml-small-q5_1.bin" {
		t.Errorf("chemin = %q, le fichier doit s'appeler ggml-small-q5_1.bin", p)
	}
	// Le chemin DOIT rester celui d'avant : sinon les installations existantes
	// re-téléchargent 190 Mo pour rien.
	if filepath.Base(filepath.Dir(p)) != "whisper" {
		t.Errorf("chemin = %q, le dossier doit rester <LOKI_HOME>/whisper", p)
	}
	u := whisperModelURLFor("small-q5_1")
	if !strings.HasSuffix(u, "/ggml-small-q5_1.bin") {
		t.Errorf("URL = %q, doit finir par /ggml-small-q5_1.bin", u)
	}
	if !strings.HasPrefix(u, "https://") {
		t.Errorf("URL = %q, doit être en HTTPS", u)
	}
}

func TestModelPathInconnuVide(t *testing.T) {
	t.Setenv("LOKI_HOME", t.TempDir())
	if p := whisperModelPathFor("nawak"); p != "" {
		t.Errorf("un modèle hors catalogue doit donner un chemin vide, pas %q", p)
	}
}

func TestCatalogueTrieParTaille(t *testing.T) {
	l := whisperCatalogueTrie()
	if len(l) < 4 {
		t.Fatalf("catalogue de %d entrées, au moins 4 attendues", len(l))
	}
	var prec int64
	for i, m := range l {
		o, _ := m["octets"].(int64)
		if i > 0 && o < prec {
			t.Errorf("entrée %d : catalogue non trié par taille croissante", i)
		}
		prec = o
		for _, clef := range []string{"id", "nom", "octets", "vram_mo", "present"} {
			if _, ok := m[clef]; !ok {
				t.Errorf("entrée %d : clé %q manquante — l'UI en a besoin", i, clef)
			}
		}
	}
}
```

- [ ] **Étape 2 : Lancer le test et vérifier qu'il échoue**

```bash
go test ./internal/loki/ -run 'TestCatalogue|TestModelPath' -v
```

Attendu : ÉCHEC de compilation, `undefined: whisperCatalogue`.

- [ ] **Étape 3 : Écrire l'implémentation**

Créer `internal/loki/dictate_models.go` :

```go
// Catalogue des modèles de dictée. Aucun n'est embarqué dans l'image : ils
// pèsent de 190 Mo à 1,1 Go et vivent dans <LOKI_HOME>/whisper/, donc dans le
// volume /data — ils survivent aux recréations du conteneur.
package loki

import (
	"os"
	"path/filepath"
	"sort"
)

// whisperModelBase : les modèles ggml officiels de whisper.cpp.
const whisperModelBase = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/"

type whisperModel struct {
	Nom     string // libellé affiché
	Fichier string // nom du .bin, identique en local et chez Hugging Face
	Octets  int64  // taille du téléchargement, pour prévenir avant de le lancer
	VRAMMo  int    // ordre de grandeur occupé sur le GPU, pour choisir en connaissance de cause
}

// Les tailles sont celles publiées par le dépôt amont. Elles servent à
// informer l'utilisateur avant un téléchargement, pas à vérifier le fichier :
// une taille qui dérive d'une release à l'autre ne doit pas casser la dictée.
var whisperCatalogue = map[string]whisperModel{
	"small-q5_1":          {Nom: "small (rapide, correct)", Fichier: "ggml-small-q5_1.bin", Octets: 190_000_000, VRAMMo: 600},
	"medium-q5_0":         {Nom: "medium (bon compromis)", Fichier: "ggml-medium-q5_0.bin", Octets: 539_000_000, VRAMMo: 1400},
	"large-v3-turbo-q5_0": {Nom: "large-v3-turbo (recommandé)", Fichier: "ggml-large-v3-turbo-q5_0.bin", Octets: 574_000_000, VRAMMo: 1600},
	"large-v3-q5_0":       {Nom: "large-v3 (le plus précis, le plus lent)", Fichier: "ggml-large-v3-q5_0.bin", Octets: 1_080_000_000, VRAMMo: 3600},
}

// whisperModelPathFor : chemin local. Vide si l'identifiant est inconnu —
// l'appelant doit traiter ce cas, plutôt que de manipuler un chemin bâti sur
// une chaîne arbitraire venue d'une requête HTTP.
func whisperModelPathFor(id string) string {
	m, ok := whisperCatalogue[id]
	if !ok {
		return ""
	}
	return filepath.Join(LokiHome(), "whisper", m.Fichier)
}

func whisperModelURLFor(id string) string {
	m, ok := whisperCatalogue[id]
	if !ok {
		return ""
	}
	return whisperModelBase + m.Fichier
}

func whisperModelPresent(id string) bool {
	p := whisperModelPathFor(id)
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && st.Size() > 0
}

// whisperCatalogueTrie : le catalogue pour l'UI, du plus léger au plus lourd —
// l'ordre dans lequel se pose la question du compromis.
func whisperCatalogueTrie() []map[string]any {
	out := make([]map[string]any, 0, len(whisperCatalogue))
	for id, m := range whisperCatalogue {
		out = append(out, map[string]any{
			"id": id, "nom": m.Nom, "octets": m.Octets,
			"vram_mo": m.VRAMMo, "present": whisperModelPresent(id),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["octets"].(int64) < out[j]["octets"].(int64)
	})
	return out
}
```

- [ ] **Étape 4 : Adapter le téléchargeur existant au multi-modèles**

Dans `internal/loki/web_transcribe.go`, supprimer la constante `whisperModelURL` et la fonction `whisperModelPath()`, puis remplacer `whisperStartDownload()` et `whisperDownload()` par des versions prenant l'identifiant du modèle. Lire le fichier en entier avant d'éditer :

```bash
cat internal/loki/web_transcribe.go
```

L'état de téléchargement `wspDl` gagne un champ `id string` pour que l'UI sache quel modèle est en cours. `whisperStartDownload(id string)` refuse de lancer un second téléchargement tant qu'un autre tourne. `whisperDownload(id string)` utilise `whisperModelURLFor(id)` et `whisperModelPathFor(id)`, et écrit dans un fichier temporaire renommé à la fin — un téléchargement interrompu ne doit pas laisser un `.bin` tronqué que `whisperModelPresent` croirait valide.

- [ ] **Étape 5 : Lancer les tests**

```bash
go build ./... && go test ./internal/loki/ -run 'TestCatalogue|TestModelPath|TestDictateCfg' -v
```

Attendu : PASS sur tous, y compris `TestDictateCfgSaveRefuseValeursInvalides` de la tâche 1.

- [ ] **Étape 6 : Commiter**

```bash
go run honnef.co/go/tools/cmd/staticcheck@latest ./internal/loki/
gofmt -l internal/loki/
git add internal/loki/dictate_models.go internal/loki/dictate_models_test.go internal/loki/web_transcribe.go
git commit -m "Dictée : catalogue de modèles et téléchargement par identifiant"
```

---

### Tâche 3 : L'environnement de lancement de whisper-server

**Fichiers :**
- Créer : `internal/loki/dictate_server.go`
- Test : `internal/loki/dictate_server_test.go`

**Interfaces :**
- Consomme : `DictateCfg` (tâche 1), `whisperModelPathFor` (tâche 2).
- Produit :
  - `func whisperServerBin(gpu bool) string`
  - `func whisperServerArgs(c DictateCfg, port int) ([]string, error)`
  - `func whisperServerEnv(c DictateCfg, base []string) []string`

Cette tâche ne lance aucun processus : elle construit et teste la ligne de commande. C'est la partie la plus facile à casser en silence et la plus facile à tester.

- [ ] **Étape 1 : Écrire le test qui échoue**

Créer `internal/loki/dictate_server_test.go` :

```go
package loki

import (
	"strings"
	"testing"
)

func argVal(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func TestServerArgs(t *testing.T) {
	t.Setenv("LOKI_HOME", t.TempDir())
	c := DictateCfg{Model: "small-q5_1", Lang: "fr", Device: "cpu"}.withDefauts()
	args, err := whisperServerArgs(c, 8091)
	if err != nil {
		t.Fatalf("construction des arguments : %v", err)
	}
	if v, ok := argVal(args, "-m"); !ok || !strings.HasSuffix(v, "ggml-small-q5_1.bin") {
		t.Errorf("-m = %q, doit pointer sur le .bin du modèle choisi", v)
	}
	if v, ok := argVal(args, "-l"); !ok || v != "fr" {
		t.Errorf("-l = %q, attendu fr", v)
	}
	if v, ok := argVal(args, "--port"); !ok || v != "8091" {
		t.Errorf("--port = %q, attendu 8091", v)
	}
	// Écoute locale UNIQUEMENT : whisper-server n'a aucune authentification,
	// l'exposer sur 0.0.0.0 ouvrirait une transcription libre à tout le réseau.
	if v, ok := argVal(args, "--host"); !ok || v != "127.0.0.1" {
		t.Errorf("--host = %q, attendu 127.0.0.1 — whisper-server n'a pas d'authentification", v)
	}
}

func TestServerArgsModeleInconnu(t *testing.T) {
	t.Setenv("LOKI_HOME", t.TempDir())
	_, err := whisperServerArgs(DictateCfg{Model: "nawak", Lang: "fr", Device: "cpu"}, 8091)
	if err == nil {
		t.Error("un modèle hors catalogue doit produire une erreur, pas une commande avec -m vide")
	}
}

func TestServerEnvGPU(t *testing.T) {
	base := []string{"PATH=/usr/bin", "CUDA_VISIBLE_DEVICES=7", "HOME=/root"}

	env := whisperServerEnv(DictateCfg{Device: "1"}, base)
	if !contientExact(env, "CUDA_VISIBLE_DEVICES=1") {
		t.Errorf("env = %v, doit contenir CUDA_VISIBLE_DEVICES=1", env)
	}
	// L'ancienne valeur doit être REMPLACÉE, pas doublée : avec deux
	// occurrences, c'est la dernière qui gagne selon l'implémentation — le
	// choix de l'utilisateur ne doit pas dépendre de ça.
	if n := compte(env, "CUDA_VISIBLE_DEVICES="); n != 1 {
		t.Errorf("%d occurrences de CUDA_VISIBLE_DEVICES, attendu exactement 1", n)
	}
	if !contientExact(env, "PATH=/usr/bin") {
		t.Error("le reste de l'environnement doit être conservé")
	}

	env = whisperServerEnv(DictateCfg{Device: "cpu"}, base)
	if !contientExact(env, "CUDA_VISIBLE_DEVICES=") {
		t.Errorf("env = %v, en mode CPU la variable doit être posée VIDE (et non retirée) pour masquer tous les GPU", env)
	}
}

func contientExact(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

func compte(l []string, prefixe string) int {
	n := 0
	for _, v := range l {
		if strings.HasPrefix(v, prefixe) {
			n++
		}
	}
	return n
}
```

- [ ] **Étape 2 : Lancer le test et vérifier qu'il échoue**

```bash
go test ./internal/loki/ -run TestServer -v
```

Attendu : ÉCHEC de compilation, `undefined: whisperServerArgs`.

- [ ] **Étape 3 : Écrire l'implémentation**

Créer `internal/loki/dictate_server.go` avec, pour l'instant, uniquement ces trois fonctions :

```go
// Supervision de whisper-server : le modèle est chargé UNE fois et reste en
// mémoire, au lieu d'être relu depuis le disque à chaque phrase dictée.
package loki

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// whisperServerBin : deux binaires sont livrés, pas un.
//
// L'image runtime peut être bâtie sur la variante CPU de llama.cpp
// (LLAMACPP_IMAGE=ghcr.io/ggml-org/llama.cpp:server). Sur cette base, les
// bibliothèques CUDA sont absentes et un binaire lié à CUDA ne démarre pas du
// tout : l'éditeur de liens échoue avant la première instruction, il n'y a
// donc aucun repli possible depuis l'intérieur du programme. D'où deux
// binaires et un choix fait ici.
func whisperServerBin(gpu bool) string {
	if gpu {
		for _, p := range []string{os.Getenv("LOKI_WHISPER_SERVER_CUDA"), "/usr/local/bin/whisper-server-cuda"} {
			if p != "" {
				if _, err := os.Stat(p); err == nil {
					return p
				}
			}
		}
		// Pas de binaire CUDA : l'appelant retombera sur le CPU en le disant.
		return ""
	}
	for _, p := range []string{os.Getenv("LOKI_WHISPER_SERVER_CPU"), "/usr/local/bin/whisper-server-cpu"} {
		if p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	if p, err := exec.LookPath("whisper-server"); err == nil {
		return p
	}
	return ""
}

func whisperServerArgs(c DictateCfg, port int) ([]string, error) {
	model := whisperModelPathFor(c.Model)
	if model == "" {
		return nil, fmt.Errorf("modèle de dictée inconnu : %s", c.Model)
	}
	return []string{
		"-m", model,
		"-l", c.Lang,
		// 127.0.0.1 et pas 0.0.0.0 : whisper-server n'a aucune
		// authentification. Sur un conteneur au réseau partagé, l'exposer
		// offrirait la transcription à tout le réseau.
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
	}, nil
}

// whisperServerEnv pose CUDA_VISIBLE_DEVICES en REMPLAÇANT toute valeur déjà
// présente. Une variable dupliquée laisse le gagnant dépendre de la libc, et
// le choix de GPU de l'utilisateur deviendrait un coup de dé.
func whisperServerEnv(c DictateCfg, base []string) []string {
	const clef = "CUDA_VISIBLE_DEVICES="
	out := make([]string, 0, len(base)+1)
	for _, v := range base {
		if !strings.HasPrefix(v, clef) {
			out = append(out, v)
		}
	}
	// Valeur vide en mode CPU : masque TOUS les GPU. Retirer la variable
	// ferait l'inverse — ggml les verrait tous.
	return append(out, clef+c.cudaVisibleDevices())
}
```

- [ ] **Étape 4 : Lancer le test et vérifier qu'il passe**

```bash
go test ./internal/loki/ -run TestServer -v
```

Attendu : PASS sur les trois tests.

- [ ] **Étape 5 : Commiter**

```bash
go run honnef.co/go/tools/cmd/staticcheck@latest ./internal/loki/
gofmt -l internal/loki/
git add internal/loki/dictate_server.go internal/loki/dictate_server_test.go
git commit -m "Dictée : ligne de commande et environnement de whisper-server"
```

---

### Tâche 4 : La supervision — démarrage paresseux, extinction sur inactivité

**Fichiers :**
- Modifier : `internal/loki/dictate_server.go`
- Test : `internal/loki/dictate_server_test.go`

**Interfaces :**
- Consomme : `whisperServerBin`, `whisperServerArgs`, `whisperServerEnv` (tâche 3) ; `dictateCfgLoad` (tâche 1).
- Produit :
  - `func whisperEnsure() (int, error)` — garantit un serveur vivant, renvoie son port
  - `func whisperInfer(ctx context.Context, wav []byte) (string, error)`
  - `func whisperShutdown()` — arrêt immédiat (changement de réglages)
  - `func whisperEtat() map[string]any`

**Décisions de conception à respecter :**

- **Pas de `Setsid`.** La CI compile pour Windows. Le processus est un enfant ordinaire, arrêté par `Process.Kill()`.
- **Port choisi par le système.** Écouter sur `:0` pour connaître un port libre, le refermer, puis le passer à whisper-server. Coder un port en dur casserait sur une machine qui l'utilise déjà.
- **Extinction après 10 min sans appel.** Une minuterie remise à zéro à chaque `whisperInfer`.
- **Un changement de réglages appelle `whisperShutdown()`** : le serveur suivant repart avec le nouveau modèle et le nouveau GPU.
- **Mort du processus rapportée comme en PR #18** : code de sortie ou signal, jamais la seule dernière ligne du journal.

- [ ] **Étape 1 : Écrire les tests qui échouent**

Ajouter à `internal/loki/dictate_server_test.go` :

```go
func TestPortLibre(t *testing.T) {
	p, err := portLibre()
	if err != nil {
		t.Fatalf("portLibre : %v", err)
	}
	if p < 1024 || p > 65535 {
		t.Errorf("port = %d, hors de la plage utilisable", p)
	}
	// Le port doit être RÉELLEMENT libre : on doit pouvoir s'y lier.
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
	if err != nil {
		t.Fatalf("port %d annoncé libre mais inutilisable : %v", p, err)
	}
	_ = l.Close()
}

func TestInferParseLaReponse(t *testing.T) {
	// whisper-server répond {"text":"..."} sur /inference. On vérifie le
	// décodage et le nettoyage sans lancer de vrai serveur.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/inference" {
			t.Errorf("chemin appelé = %q, attendu /inference", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/form-data") {
			t.Errorf("Content-Type = %q, whisper-server attend du multipart", ct)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"  bonjour ceci est un test  "}`))
	}))
	defer srv.Close()

	txt, err := whisperInferSur(context.Background(), srv.URL, []byte("RIFFfaux"))
	if err != nil {
		t.Fatalf("whisperInferSur : %v", err)
	}
	if txt != "bonjour ceci est un test" {
		t.Errorf("texte = %q, les espaces de bord doivent être retirés", txt)
	}
}

func TestInferFiltreLesMarqueurs(t *testing.T) {
	// Sur du silence, whisper renvoie ses marqueurs internes. Les laisser
	// passer les collerait tels quels dans le champ de saisie — c'est ce qui
	// est arrivé avec [BLANK_AUDIO].
	for _, marqueur := range []string{"[BLANK_AUDIO]", "(silence)", "[SOUND]", " [ Silence ] "} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"text":"` + marqueur + `"}`))
		}))
		txt, err := whisperInferSur(context.Background(), srv.URL, []byte("RIFFfaux"))
		srv.Close()
		if err != nil {
			t.Fatalf("whisperInferSur(%q) : %v", marqueur, err)
		}
		if txt != "" {
			t.Errorf("marqueur %q rendu comme texte %q, attendu vide", marqueur, txt)
		}
	}
}

func TestInferErreurHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()
	if _, err := whisperInferSur(context.Background(), srv.URL, []byte("RIFFfaux")); err == nil {
		t.Error("un 500 de whisper-server doit remonter une erreur")
	}
}
```

Ajouter les imports `context`, `net`, `net/http`, `net/http/httptest`, `strconv` au fichier de test.

- [ ] **Étape 2 : Lancer et vérifier l'échec**

```bash
go test ./internal/loki/ -run 'TestPortLibre|TestInfer' -v
```

Attendu : ÉCHEC de compilation, `undefined: portLibre`, `undefined: whisperInferSur`.

- [ ] **Étape 3 : Implémenter**

Ajouter à `internal/loki/dictate_server.go`. `whisperInferSur` prend l'URL de base en paramètre — c'est ce qui rend l'appel testable sans lancer de processus ; `whisperInfer` n'est qu'une enveloppe qui garantit le serveur puis délègue.

```go
// portLibre demande au système un port disponible, puis le relâche. Coder un
// port en dur casserait sur une machine qui l'occupe déjà ; la fenêtre entre
// la fermeture et la reprise par whisper-server est négligeable devant le
// risque d'un conflit permanent.
func portLibre() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// whisperMarqueur reconnaît les annotations que whisper produit à la place
// d'un texte : [BLANK_AUDIO], (silence), [SOUND]… Un contenu entièrement
// entouré de crochets ou de parenthèses n'est jamais de la parole dictée.
func whisperMarqueur(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	return (strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]")) ||
		(strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")"))
}

func whisperInferSur(ctx context.Context, base string, wav []byte) (string, error) {
	var corps bytes.Buffer
	mw := multipart.NewWriter(&corps)
	part, err := mw.CreateFormFile("file", "dictee.wav")
	if err != nil {
		return "", err
	}
	if _, err := part.Write(wav); err != nil {
		return "", err
	}
	if err := mw.WriteField("response_format", "json"); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/inference", &corps)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	brut, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("whisper-server a répondu %d : %s", resp.StatusCode, lastLine(string(brut)))
	}
	var j struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(brut, &j); err != nil {
		return "", fmt.Errorf("réponse illisible de whisper-server : %w", err)
	}
	if whisperMarqueur(j.Text) {
		return "", nil
	}
	return strings.TrimSpace(j.Text), nil
}
```

Puis la supervision proprement dite, dans le même fichier :

```go
var wsrvMu sync.Mutex
var wsrv struct {
	cmd    *exec.Cmd
	port   int
	cfg    DictateCfg
	sortie strings.Builder // journal du processus, pour dire comment il est mort
	timer  *time.Timer
	err    string
}

const whisperIdle = 10 * time.Minute

// whisperEnsure garantit un serveur vivant pour la configuration COURANTE.
// Si les réglages ont changé depuis le lancement, l'ancien est arrêté : c'est
// ce qui rend le changement de modèle ou de GPU effectif sans redémarrer Loki.
func whisperEnsure() (int, error) {
	cfg := dictateCfgLoad()
	wsrvMu.Lock()
	defer wsrvMu.Unlock()
	if wsrv.cmd != nil && wsrv.cfg == cfg && wsrv.cmd.ProcessState == nil {
		whisperTouchLocked()
		return wsrv.port, nil
	}
	whisperStopLocked()
	return whisperStartLocked(cfg)
}
```

Écrire `whisperStartLocked`, `whisperStopLocked` et `whisperTouchLocked` en respectant :

- `whisperStartLocked` refuse si le modèle est absent (`whisperModelPresent`) en renvoyant une erreur qui le dit, et si `whisperServerBin` renvoie `""` pour le GPU demandé, retombe sur le binaire CPU **en consignant pourquoi** dans `wsrv.err`.
- Après `cmd.Start()`, attendre que `GET http://127.0.0.1:<port>/` réponde, avec un délai maximal de 120 s (le chargement d'un modèle de 1 Go depuis un disque lent est long). Si le processus meurt pendant l'attente, renvoyer `whisperExitReason(cmd.Wait())` — la fonction écrite en PR #18 — plus la dernière ligne de son journal.
- `whisperTouchLocked` remet la minuterie d'inactivité à `whisperIdle`, dont l'expiration appelle `whisperShutdown`.

- [ ] **Étape 4 : Lancer les tests**

```bash
go build ./... && go test ./internal/loki/ -run 'TestPortLibre|TestInfer|TestServer' -v
```

Attendu : PASS.

- [ ] **Étape 5 : Vérifier la compilation Windows**

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./cmd/loki
```

Attendu : succès. Un échec ici signale un appel spécifique à Unix — le corriger avant de commiter, la CI le refuserait.

- [ ] **Étape 6 : Commiter**

```bash
go run honnef.co/go/tools/cmd/staticcheck@latest ./internal/loki/
gofmt -l internal/loki/
git add internal/loki/dictate_server.go internal/loki/dictate_server_test.go
git commit -m "Dictée : whisper-server supervisé, modèle chargé une seule fois"
```

---

### Tâche 5 : Les routes HTTP

**Fichiers :**
- Modifier : `internal/loki/web_transcribe.go`, `internal/loki/web_server.go`
- Test : `internal/loki/web_transcribe_test.go`

**Interfaces :**
- Consomme : tout ce qui précède.
- Produit : `GET/POST /api/dictate/config`, `GET /api/dictate/models`, `POST /api/dictate/models/download`, `GET /api/dictate/state`.

`POST /api/transcribe` **garde son contrat** : entrée `audio/wav`, sortie `{"text":…}` ou `{"error":…}`, 503 `{"downloading":true,"pct":N}` pendant un téléchargement. Seul son intérieur change — il appelle `whisperEnsure()` puis `whisperInfer()` au lieu de lancer un processus.

- [ ] **Étape 1 : Écrire le test qui échoue**

Ajouter à `internal/loki/web_transcribe_test.go` :

```go
func TestConfigRouteAllerRetour(t *testing.T) {
	withTestStore(t)
	corps := strings.NewReader(`{"model":"small-q5_1","lang":"en","device":"1","reactivity":"court"}`)
	rec := httptest.NewRecorder()
	handleDictateConfig(rec, httptest.NewRequest("POST", "/api/dictate/config", corps))
	if rec.Code != 200 {
		t.Fatalf("POST = %d, corps %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handleDictateConfig(rec, httptest.NewRequest("GET", "/api/dictate/config", nil))
	var got DictateCfg
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("réponse illisible : %v", err)
	}
	if got.Device != "1" || got.Lang != "en" || got.Model != "small-q5_1" {
		t.Errorf("relu %+v, attendu le réglage enregistré", got)
	}
}

func TestConfigRouteRefuseModeleInconnu(t *testing.T) {
	withTestStore(t)
	corps := strings.NewReader(`{"model":"nawak","lang":"fr","device":"cpu","reactivity":"moyen"}`)
	rec := httptest.NewRecorder()
	handleDictateConfig(rec, httptest.NewRequest("POST", "/api/dictate/config", corps))
	if rec.Code == 200 {
		t.Error("un modèle hors catalogue doit être refusé par la route, pas seulement par le magasin")
	}
}

func TestModelsRoute(t *testing.T) {
	t.Setenv("LOKI_HOME", t.TempDir())
	rec := httptest.NewRecorder()
	handleDictateModels(rec, httptest.NewRequest("GET", "/api/dictate/models", nil))
	if rec.Code != 200 {
		t.Fatalf("GET = %d", rec.Code)
	}
	var l []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		t.Fatalf("réponse illisible : %v", err)
	}
	if len(l) < 4 {
		t.Errorf("%d modèles renvoyés, au moins 4 attendus", len(l))
	}
}
```

- [ ] **Étape 2 : Lancer et vérifier l'échec**

```bash
go test ./internal/loki/ -run 'TestConfigRoute|TestModelsRoute' -v
```

Attendu : ÉCHEC, `undefined: handleDictateConfig`.

- [ ] **Étape 3 : Implémenter les handlers**

Dans `internal/loki/web_transcribe.go`, ajouter `handleDictateConfig`, `handleDictateModels`, `handleDictateDownload`, `handleDictateState`. `handleDictateConfig` en POST appelle `dictateCfgSave` puis **`whisperShutdown()`** — sans ça, le serveur continuerait de tourner avec l'ancien modèle et le réglage semblerait sans effet.

Réécrire le corps de `handleTranscribe` : validation de l'entrée inchangée, puis

```go
port, err := whisperEnsure()
if err != nil {
	sendJSON(w, 503, map[string]any{"error": err.Error()})
	return
}
txt, err := whisperInferSur(ctx, "http://127.0.0.1:"+strconv.Itoa(port), audio)
```

Le fichier temporaire disparaît : whisper-server reçoit le WAV en mémoire.

- [ ] **Étape 4 : Enregistrer les routes**

Dans `internal/loki/web_server.go`, après la ligne `api("/api/transcribe", handleTranscribe)` :

```go
api("/api/dictate/config", handleDictateConfig)          // réglages de la dictée
api("/api/dictate/models", handleDictateModels)          // catalogue + présence sur disque
api("/api/dictate/models/download", handleDictateDownload) // télécharge un modèle
api("/api/dictate/state", handleDictateState)            // serveur allumé, modèle chargé, dernière erreur
```

- [ ] **Étape 5 : Lancer toute la suite**

```bash
go build ./... && go test ./internal/loki/ -v 2>&1 | tail -20
```

Attendu : PASS.

- [ ] **Étape 6 : Commiter**

```bash
go run honnef.co/go/tools/cmd/staticcheck@latest ./internal/loki/
gofmt -l internal/loki/
git add internal/loki/web_transcribe.go internal/loki/web_server.go internal/loki/web_transcribe_test.go
git commit -m "Dictée : routes de configuration, catalogue et état"
```

---

### Tâche 6 : Le Dockerfile — deux binaires whisper-server

**Fichiers :**
- Modifier : `Dockerfile`
- Test : `internal/loki/web_transcribe_test.go` (étendre `TestDockerfileWhisperNonNatif`)

- [ ] **Étape 1 : Étendre le garde-fou**

Remplacer `TestDockerfileWhisperNonNatif` dans `internal/loki/web_transcribe_test.go` par une version qui vérifie, dans **chaque** étape dont le nom commence par `whisperbuild` :

- la présence de `-DGGML_NATIVE=OFF` et `-DGGML_AMX_TILE=OFF`
- la construction de la cible `whisper-server`

et, globalement :

- qu'une étape porte `-DGGML_CUDA=ON`
- que le runtime copie `whisper-server-cpu` **et** `whisper-server-cuda`

- [ ] **Étape 2 : Lancer et vérifier l'échec**

```bash
go test ./internal/loki/ -run TestDockerfileWhisper -v
```

Attendu : ÉCHEC — le Dockerfile ne construit encore que `whisper-cli`.

- [ ] **Étape 3 : Modifier le Dockerfile**

Dupliquer l'étape `whisperbuild` en `whisperbuild-cpu` (base `ubuntu:22.04`) et `whisperbuild-cuda` (base `nvidia/cuda:12.4.1-devel-ubuntu22.04`, avec `-DGGML_CUDA=ON`). Les deux construisent la cible `whisper-server` et gardent **tous** les drapeaux de portabilité.

**Vérifier la concordance CUDA avant de figer la version de l'image de build :**

```bash
docker run --rm --entrypoint sh ghcr.io/ggml-org/llama.cpp:server-cuda -c 'ls /usr/local/cuda*/lib64/libcudart.so* 2>/dev/null; ldconfig -p | grep -i cudart'
```

La version majeure de `libcudart` du runtime doit correspondre à celle de l'image `nvidia/cuda:*-devel` choisie. En cas d'écart, aligner l'image de build sur le runtime.

Dans l'étape runtime, remplacer la copie de `whisper-cli` par :

```dockerfile
COPY --from=whisperbuild-cpu  /w/build/bin/whisper-server /usr/local/bin/whisper-server-cpu
COPY --from=whisperbuild-cuda /w/build/bin/whisper-server /usr/local/bin/whisper-server-cuda
```

et retirer `LOKI_WHISPER_BIN` du bloc `ENV` s'il n'est plus lu par le code.

- [ ] **Étape 4 : Vérifier**

```bash
go test ./internal/loki/ -run TestDockerfileWhisper -v
```

Attendu : PASS.

- [ ] **Étape 5 : Bâtir l'image pour de vrai**

```bash
docker build -t loki-test .
```

Attendu : succès. C'est la seule vérification qui prouve que les deux étapes compilent — les tests Go ne lisent que le texte du Dockerfile. **Si Docker n'est pas disponible localement, le noter explicitement et laisser la CI trancher.**

- [ ] **Étape 6 : Commiter**

```bash
git add Dockerfile internal/loki/web_transcribe_test.go
git commit -m "Image : whisper-server en deux binaires, CPU et CUDA"
```

---

### Tâche 7 : Le panneau Dictée

**Fichiers :**
- Créer : `internal/loki/ui/src/js/24-dictate-settings.js`
- Modifier : `internal/loki/ui/src/index.tmpl.html`, `internal/loki/ui/src/js/23-dictate.js`
- Régénérer : `internal/loki/ui/index.html`

- [ ] **Étape 1 : Ajouter l'entrée de navigation**

Dans `internal/loki/ui/src/index.tmpl.html`, après la ligne `<button data-pane="engine-log" …>` (ligne 255) :

```html
        <button data-pane="dictee" onclick="settingsShow('dictee')">Dictée</button>
```

- [ ] **Étape 2 : Ajouter le panneau**

Après la section `data-pane="engine"` (qui se termine avant la ligne 471), insérer une `<section class="set-pane" data-pane="dictee">` contenant : un `<select id="dic-device">`, un `<select id="dic-model">`, un `<select id="dic-lang">`, un `<select id="dic-react">`, un bouton d'enregistrement, une ligne d'état `<div id="dic-state">` et un bouton « Tester le micro » avec `<div id="dic-test-out">`. Suivre le balisage des panneaux voisins pour les classes.

- [ ] **Étape 3 : Écrire le module**

Créer `internal/loki/ui/src/js/24-dictate-settings.js` avec `loadDictateSettings()` (remplit les listes depuis `/api/dictate/models`, `/api/vram` et `/api/dictate/config`), `saveDictateSettings()` et `testerMicro()`. Brancher le chargement paresseux dans `22-settings-modal.js` :

```js
const SET_HOOKS = {
  'engine-log': () => { loadSvcLog(); showPaths(); },
  'dictee': () => { loadDictateSettings(); },
};
```

`testerMicro()` réutilise la capture de `23-dictate.js`, envoie à `/api/transcribe`, et affiche niveau + texte dans `#dic-test-out` **sans toucher à `#input`**.

- [ ] **Étape 4 : Régénérer l'UI**

```bash
go run ./tools/assemble-ui
git diff --stat internal/loki/ui/index.html
```

Attendu : `index.html` modifié. **Ne jamais l'éditer à la main** — il est généré.

- [ ] **Étape 5 : Vérifier dans le navigateur**

```bash
go run ./cmd/loki serve
```

Ouvrir l'interface, Paramètres → Dictée. Vérifier : les GPU apparaissent avec leur VRAM, le catalogue liste quatre modèles avec leur taille, l'enregistrement persiste après rechargement de la page, le test du micro affiche un niveau.

- [ ] **Étape 6 : Commiter**

```bash
go build ./... && go test ./internal/loki/
git add internal/loki/ui/
git commit -m "Dictée : panneau de réglages — GPU, modèle, langue, réactivité, test du micro"
```

---

## Vérification finale

- [ ] `gofmt -l .` — vide
- [ ] `go vet ./...` — silencieux
- [ ] `go test ./...` — vert
- [ ] `go run honnef.co/go/tools/cmd/staticcheck@latest ./...` — zéro avertissement
- [ ] `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./cmd/loki` — succès
- [ ] `docker build -t loki-test .` — succès (ou constat explicite que Docker est indisponible localement)
- [ ] `internal/loki/ui/index.html` régénéré et commité

**Ce que la vérification automatique ne couvre pas :** la qualité de reconnaissance. Aucun échantillon de voix n'est disponible côté développement. À livrer, il faudra dicter une phrase et comparer avec l'ancien modèle — c'est une constatation d'usage, pas un test.
