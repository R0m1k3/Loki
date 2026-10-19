package loki

import "testing"

// Un motif vide dans la surcharge correspondait à tout : chaque tour était
// relancé une fois. Il est écarté ; une surcharge sans motif utilisable retombe
// sur les motifs embarqués.
func TestCompileRetryPatternsSkipsEmpty(t *testing.T) {
	res, custom := compileRetryPatterns(`["", "  ", "<oups>"]`)
	if !custom || len(res) != 1 {
		t.Fatalf("custom=%v, %d motifs ; attendu la surcharge avec un seul motif", custom, len(res))
	}
	if res[0].MatchString("une réponse tout à fait normale") {
		t.Fatal("un texte ordinaire déclenche une relance")
	}
	res, custom = compileRetryPatterns(`["", "([invalide"]`)
	if custom || len(res) != len(defaultRetryPatterns) {
		t.Fatalf("custom=%v, %d motifs ; attendu les motifs embarqués", custom, len(res))
	}
	if _, custom = compileRetryPatterns(`pas du json`); custom {
		t.Fatal("surcharge illisible prise en compte")
	}
}

// Un changement de retry_patterns agit sans redémarrer le process.
func TestRetryPatternsReloadOnChange(t *testing.T) {
	testHome(t)
	retryStateBust()
	if _, custom := retryState(); custom {
		t.Fatal("surcharge active sans clé")
	}
	if err := putStr(bkState, "retry_patterns", `["OUPS-OUTIL"]`); err != nil {
		t.Fatal(err)
	}
	if !textualToolCall("bon, OUPS-OUTIL, voilà") {
		t.Fatal("nouveau motif ignoré après l'écriture")
	}
	if err := putStr(bkState, "retry_patterns", ""); err != nil {
		t.Fatal(err)
	}
	if _, custom := retryState(); custom {
		t.Fatal("surcharge retirée mais toujours active")
	}
	if textualToolCall("bon, OUPS-OUTIL, voilà") {
		t.Fatal("ancien motif encore appliqué")
	}
}
