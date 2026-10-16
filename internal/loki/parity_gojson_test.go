package loki

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parity_gojson_test.go — journal de référence du décodage JSON des requêtes
// (json.NewDecoder(r.Body).Decode(&req)) : champs décodés et message d'erreur,
// que plusieurs routes renvoient tels quels au client. Rangé dans
// rust/loki-rs/tests/parity_gojson/decode.json ; tests/gojson.rs le rejoue
// contre src/gojson.rs.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteDecodageJSON

const parityGoJSONFile = "../../rust/loki-rs/tests/parity_gojson/decode.json"

type parityDecodeT struct {
	Name string            `json:"name"`
	On   bool              `json:"on"`
	N    int               `json:"n"`
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
	Opt  *string           `json:"opt"`
	Flag *bool             `json:"flag"`
}

func TestPariteDecodageJSON(t *testing.T) {
	type cas struct {
		In  string        `json:"in"`
		Out parityDecodeT `json:"out"`
		Err string        `json:"err,omitempty"`
	}
	var fx struct {
		Cases []cas `json:"cases"`
	}
	bodies := []string{
		``, `   `, "\n\t", `pas du json`, `{`, `{"name"`, `{"name":`, `{"name":"a"`, `{"name":"a",}`,
		`{"name" "a"}`, `{"name":"a" "on":true}`, `{name:"a"}`, `{'name':"a"}`, `[`, `[1,]`, `[1 2]`,
		`{"name":"a"} et la suite`, `{"name":"a"}{"name":"b"}`, `null`, `true`, `"texte"`, `12`, `[1,2]`,
		`{"name":5}`, `{"name":true}`, `{"name":{}}`, `{"name":[]}`, `{"name":null}`, `{"NAME":"maj"}`,
		`{"Name":"a","name":"b"}`, `{"on":"oui"}`, `{"on":1}`, `{"on":true,"name":3,"n":4}`,
		`{"n":1.5}`, `{"n":"3"}`, `{"n":-7}`, `{"n":1e2}`, `{"args":["a","b"]}`, `{"args":["a",1,"c"]}`,
		`{"args":"x"}`, `{"args":[]}`, `{"args":[null]}`, `{"env":{"A":"1","B":2}}`, `{"env":[]}`,
		`{"opt":""}`, `{"opt":null}`, `{"opt":"x","flag":false}`, `{"flag":"non"}`,
		`{"name":"été"}`, `{"name":"a\nb"}`, "{\"name\":\"a\x01b\"}", `{"name":"\x"}`,
		`{"name":"\u12G4"}`, `tru`, `trux`, `nul`, `nulx`, `fals`, `falsy`, `-`, `-x`, `1.`, `1.x`, `1e`,
		`1ex`, `{"a":1.}`, `{"a":01}`, `{"a":-}`, `[1,2,3}`, `{"a":1]`, "\x01", "\x7f", "\xc3\xa9",
		`'x'`, `"x`, `{"a":"b"}}`, `}`, `]`, `,`, `:`, `{"a" : [ { "b" : null } ] , "name" : "ok" }`,
	}
	for _, b := range bodies {
		var v parityDecodeT
		c := cas{In: b}
		if err := json.NewDecoder(strings.NewReader(b)).Decode(&v); err != nil {
			c.Err = err.Error()
		}
		c.Out = v
		fx.Cases = append(fx.Cases, c)
	}
	got, err := parityJSON(fx)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityGoJSONFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityGoJSONFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityGoJSONFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("le décodage JSON ne produit plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteDecodageJSON,\n"+
			"puis cargo test --test gojson dit si le port Rust suit.\n--- obtenu ---\n%s", parityGoJSONFile, got)
	}
}
