package loki

import (
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Une écriture de ce process est vue tout de suite : le cache ne retarde pas un
// changement de clé.
func TestWebKeyCacheInvalidatedByLocalWrite(t *testing.T) {
	testHome(t)
	if err := putStr(bkState, "web_key", "ancienne"); err != nil {
		t.Fatal(err)
	}
	if k, err := readWebKeyErr(); err != nil || k != "ancienne" {
		t.Fatalf("clé = %q, %v", k, err)
	}
	if err := putStr(bkState, "web_key", "nouvelle"); err != nil {
		t.Fatal(err)
	}
	if k, err := readWebKeyErr(); err != nil || k != "nouvelle" {
		t.Fatalf("clé après écriture = %q, %v ; attendu la nouvelle", k, err)
	}
}

// Une écriture d'un AUTRE process (sans cacheBust ici) finit vue : au plus
// cacheMaxAge plus tard.
func TestWebKeyCacheSeesExternalWrite(t *testing.T) {
	testHome(t)
	if err := putStr(bkState, "web_key", "a"); err != nil {
		t.Fatal(err)
	}
	if k, _ := readWebKeyErr(); k != "a" {
		t.Fatalf("clé = %q", k)
	}
	// Écriture directe, comme le ferait la CLI : pas de cacheBust.
	err := withDB(func(d *bolt.DB) error {
		return d.Update(func(tx *bolt.Tx) error {
			return tx.Bucket([]byte(bkState)).Put([]byte("web_key"), []byte("b"))
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(cacheMaxAge + 500*time.Millisecond)
	for {
		if k, _ := readWebKeyErr(); k == "b" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("écriture externe jamais vue")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Sans changement, les lectures suivantes n'ouvrent plus la base.
func TestWebKeyCacheServesRepeatedReads(t *testing.T) {
	testHome(t)
	if err := putStr(bkState, "web_key", "k"); err != nil {
		t.Fatal(err)
	}
	if k, _ := readWebKeyErr(); k != "k" {
		t.Fatalf("clé = %q", k)
	}
	// Base verrouillée par « un autre process » : une lecture qui l'ouvrirait
	// attendrait la borne de 5 s puis échouerait.
	dbMu.Lock()
	type res struct {
		k   string
		err error
	}
	ch := make(chan res, 1)
	go func() { k, err := readWebKeyErr(); ch <- res{k, err} }()
	select {
	case r := <-ch:
		dbMu.Unlock()
		if r.err != nil || r.k != "k" {
			t.Fatalf("clé = %q, %v", r.k, r.err)
		}
	case <-time.After(300 * time.Millisecond):
		dbMu.Unlock()
		<-ch
		t.Fatal("lecture servie par la base, pas par le cache")
	}
}
