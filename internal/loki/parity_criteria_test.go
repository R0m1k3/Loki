//go:build linux

package loki

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// parity_criteria_test.go — journal de référence de l'outil criteria du mode
// Code (lot 38 de la migration Rust) : chaque appel (arguments tels que le
// modèle les envoie, builder ou vérification), son résultat, et les critères
// rangés ensuite pour la discussion. Rangé dans
// rust/loki-rs/tests/parity_tools/criteria.json ; tests/tools.rs le rejoue
// par le port Rust.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteCriteres

const parityCriteriaFile = "../../rust/loki-rs/tests/parity_tools/criteria.json"

type parityCriteriaStep struct {
	Name     string          `json:"name"`
	Verifier bool            `json:"verifier,omitempty"`
	Put      string          `json:"put,omitempty"` // critères écrits tels quels avant l'appel
	Args     json.RawMessage `json:"args"`
	Out      string          `json:"out"`
	Stored   any             `json:"stored"` // la liste rangée (null : aucune)
}

func TestPariteCriteres(t *testing.T) {
	testHome(t)
	conv := convEnsureActive()
	var steps []parityCriteriaStep
	run := func(st parityCriteriaStep) {
		if st.Put != "" {
			if err := putStoreBytes(bkChat, critKey(conv), []byte(st.Put)); err != nil {
				t.Fatal(err)
			}
		}
		var args map[string]any
		if err := json.Unmarshal(st.Args, &args); err != nil {
			t.Fatalf("%s : %v", st.Name, err)
		}
		if args == nil {
			args = map[string]any{}
		}
		st.Out = toolCriteria(args, st.Verifier)
		st.Stored = nil
		if b, ok := getStoreBytes(bkChat, critKey(conv)); ok && len(b) > 0 {
			var v any
			if err := json.Unmarshal(b, &v); err != nil {
				t.Fatal(err)
			}
			st.Stored = v
		}
		steps = append(steps, st)
	}
	a := func(s string) json.RawMessage { return json.RawMessage(s) }

	run(parityCriteriaStep{Name: "liste vide", Args: a(`{"action":"list"}`)})
	run(parityCriteriaStep{Name: "action absente : liste", Args: a(`{}`)})
	run(parityCriteriaStep{Name: "action d'un autre type", Args: a(`{"action":3}`)})
	run(parityCriteriaStep{Name: "ajout sans textes", Args: a(`{"action":"add"}`)})
	run(parityCriteriaStep{Name: "ajout de textes vides ou d'un autre type", Args: a(`{"action":"add","texts":["  ",4,null]}`)})
	run(parityCriteriaStep{Name: "ajout", Args: a(`{"action":"add","texts":["  les tests passent ","la doc <à jour> & relue",""]}`)})
	run(parityCriteriaStep{Name: "ajout à la suite", Args: a(`{"action":"add","texts":["pas de régression"]}`)})
	run(parityCriteriaStep{Name: "le builder dit fait", Args: a(`{"action":"set","id":1,"status":"completed","note":"  vérifié à la main  "}`)})
	run(parityCriteriaStep{Name: "le builder ne valide pas", Args: a(`{"action":"set","id":1,"status":"passed"}`)})
	run(parityCriteriaStep{Name: "id en chaîne avec #", Verifier: true, Args: a(`{"action":"set","id":" #2 ","status":"failed","note":"lien mort"}`)})
	run(parityCriteriaStep{Name: "id en chaîne illisible", Verifier: true, Args: a(`{"action":"set","id":"deux","status":"passed"}`)})
	run(parityCriteriaStep{Name: "id décimal tronqué", Verifier: true, Args: a(`{"action":"set","id":3.9,"status":"passed"}`)})
	run(parityCriteriaStep{Name: "note seule", Args: a(`{"action":"set","id":"+1","note":"revu"}`)})
	run(parityCriteriaStep{Name: "statut libre", Args: a(`{"action":"set","id":2,"status":"bloqué"}`)})
	run(parityCriteriaStep{Name: "id inconnu", Verifier: true, Args: a(`{"action":"set","id":9,"status":"passed"}`)})
	run(parityCriteriaStep{Name: "liste", Args: a(`{"action":"list"}`)})
	run(parityCriteriaStep{Name: "action inconnue", Args: a(`{"action":"reset"}`)})
	run(parityCriteriaStep{Name: "effacer", Args: a(`{"action":"clear"}`)})
	run(parityCriteriaStep{Name: "effacer à vide", Args: a(`{"action":"clear"}`)})
	// Vingt au plus ; les ids reprennent après le plus grand.
	run(parityCriteriaStep{Name: "liste pleine",
		Put:  `[{"id":7,"text":"a","status":"passed"},{"id":3,"text":"b","status":"pending"}]`,
		Args: a(`{"action":"add","texts":["c","d","e","f","g","h","i","j","k","l","m","n","o","p","q","r","s","t","u","v","w"]}`)})
	run(parityCriteriaStep{Name: "liste déjà pleine", Args: a(`{"action":"add","texts":["x"]}`)})
	run(parityCriteriaStep{Name: "liste rangée illisible", Put: `{"pas":"une liste"}`, Args: a(`{"action":"list"}`)})

	got, err := json.MarshalIndent(steps, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityCriteriaFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityCriteriaFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityCriteriaFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("l'outil criteria ne produit plus le journal rangé dans %s.\n"+
			"LOKI_PARITY_UPDATE=1 pour le régénérer si le changement est voulu.", parityCriteriaFile)
	}
}
