package loki

import (
	"reflect"
	"testing"
	"time"
)

// Une barre de progression (\r sans \n) doit apparaître dans le journal, sans
// le noyer ; \r\n reste une fin de ligne ordinaire, même coupé entre deux écritures.
func TestSinkWriterProgression(t *testing.T) {
	var got []string
	setBuildSink(func(l string) { got = append(got, l) })
	defer setBuildSink(nil)

	s := &sinkWriter{}
	s.Write([]byte("Step 5\r"))
	s.Write([]byte("\n  modèle : 1 / 76 GB\r  modèle : 2 / 76 GB\r"))
	s.Write([]byte("  modèle : 3 / 76 GB\r"))
	s.lastProg = time.Now().Add(-time.Minute) // 10 s écoulées
	s.Write([]byte("  modèle : 40 / 76 GB\r[ok] fini\n"))

	// « 3 » finissait sa propre écriture : il attendait de savoir si un saut de
	// ligne suivait, et part donc avec l'écriture suivante.
	want := []string{"Step 5", "modèle : 1 / 76 GB", "modèle : 3 / 76 GB", "[ok] fini"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lignes %q, attendu %q", got, want)
	}
}
