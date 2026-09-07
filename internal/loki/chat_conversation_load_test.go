package loki

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// resetConvForTest vide l'état en RAM pour forcer un VRAI rechargement depuis la
// base, comme au démarrage d'un process qui ne connaît encore rien. `conv` est un
// global du paquet : sans ça, un test verrait l'état laissé par le précédent.
func resetConvForTest() {
	conv.mu.Lock()
	conv.Messages, conv.Log, conv.Seq, conv.CtxUsed = nil, nil, 0, 0
	conv.mu.Unlock()
}

// TestLoadConversationReessayeSousContention : la panne d'origine est un verrou
// bbolt transitoire au redémarrage du conteneur (l'ancien process tient encore
// le fichier pendant que le nouveau démarre), confondu avec « rien à charger ».
//
// Réserve honnête : ce test ne reproduit PAS la panne à coup sûr. withDB ouvre
// déjà la base avec Options.Timeout de 5 s, qui absorbe seul une contention de
// 400 ms — ce test passe donc aussi sur le code d'AVANT le correctif. Il est
// gardé comme filet de non-régression sur le nouveau chemin (getBytesErr + boucle
// de tentatives) : ce chemin ne doit ni perdre la conversation, ni se bloquer.
func TestLoadConversationReessayeSousContention(t *testing.T) {
	t.Setenv("LOKI_HOME", t.TempDir())

	// 1. Écrit une conversation par l'API normale (elle crée aussi la clé active).
	resetConvForTest()
	conv.mu.Lock()
	conv.Messages = []Message{{Role: "user", Content: "bonjour"}}
	conv.mu.Unlock()
	conv.persist()
	activeID := getStr(bkChat, ckActive)
	if activeID == "" {
		t.Fatal("persist n'a pas créé de discussion active — le montage du test est faux")
	}

	// 2. État en RAM vidé : le rechargement doit tout reprendre depuis la base.
	resetConvForTest()

	// 3. Verrou exclusif tenu une fenêtre courte dans une goroutine séparée : la
	// base est momentanément indisponible, exactement comme au chevauchement.
	db, err := bolt.Open(dbPath(), 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("impossible de prendre le verrou pour le test : %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(400 * time.Millisecond)
		db.Close()
	}()

	LoadConversation()
	wg.Wait()

	conv.mu.Lock()
	got := len(conv.Messages)
	conv.mu.Unlock()
	if got != 1 {
		t.Fatalf("conversation perdue malgré une contention temporaire : %d message(s), attendu 1", got)
	}
	if id := getStr(bkChat, ckActive); id != activeID {
		t.Fatalf("la discussion active a changé (%q → %q) : le fil d'origine est orphelin", activeID, id)
	}
}

// TestLoadConversationBaseIllisibleNeForgePasDeDiscussion garde l'invariant qui
// justifie le correctif : convEnsureActive ÉCRIT (elle forge un identifiant et
// l'enregistre sous bkChat/active). Elle ne doit donc jamais être appelée sur la
// foi d'une lecture ratée — sinon un verrou durable suffisait à mettre le fil en
// cours de côté au profit d'une discussion neuve, en silence.
//
// Réserve honnête, là encore : avec une base illisible l'écriture échoue elle
// aussi, donc le disque finit identique dans les deux versions. Ce que ce test
// verrouille, c'est le CONTRAT — sortir sans rien écrire, et sans faire attendre
// indéfiniment — pas la reproduction du dégât.
func TestLoadConversationBaseIllisibleNeForgePasDeDiscussion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LOKI_HOME", home)

	// Fichier de base invalide : bolt.Open échoue tout de suite (pas d'attente de
	// verrou), ce qui exerce le chemin d'erreur sans allonger le test.
	path := filepath.Join(home, "loki.db")
	if err := os.WriteFile(path, []byte("ceci n'est pas une base bbolt"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	resetConvForTest()
	start := time.Now()
	LoadConversation()
	elapsed := time.Since(start)

	// Budget : loadConvAttempts tentatives espacées de loadConvRetryWait, plus une
	// marge. Au-delà, la boucle de reprise retarderait le démarrage du serveur.
	if max := time.Duration(loadConvAttempts)*loadConvRetryWait + 2*time.Second; elapsed > max {
		t.Fatalf("démarrage retardé de %v par une base illisible (budget %v)", elapsed, max)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("la base a été modifiée alors qu'elle n'a même pas pu être lue")
	}
	conv.mu.Lock()
	n := len(conv.Messages)
	conv.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d message(s) chargés depuis une base illisible", n)
	}
}

// TestLoadConversationAbsenceLegitimeResteRapide : le correctif ne doit pas avoir
// transformé le cas NORMAL (base saine, rien d'enregistré encore — premier
// démarrage) en attente inutile. Une seule tentative doit suffire.
func TestLoadConversationAbsenceLegitimeResteRapide(t *testing.T) {
	t.Setenv("LOKI_HOME", t.TempDir())
	resetConvForTest()

	start := time.Now()
	LoadConversation()
	elapsed := time.Since(start)

	if elapsed >= loadConvRetryWait {
		t.Fatalf("absence légitime traitée comme une erreur : %v d'attente, aucune reprise ne devait être déclenchée", elapsed)
	}
	conv.mu.Lock()
	n := len(conv.Messages)
	conv.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d message(s) chargés alors que rien n'avait été enregistré", n)
	}
}
