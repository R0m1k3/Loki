package loki

import (
	"context"
	"testing"
)

// Pendant une génération de chat, un envoi est mis en file ; il ressort à la
// frontière d'étape suivante, journalisé comme une bulle utilisateur.
func TestQueueDuringGeneration(t *testing.T) {
	c := newTestConv()
	c.Generating = true
	queued, err := c.EnqueueOrStart("cid-1", "précision", nil, Caps{}, 0.7)
	if err != nil || !queued {
		t.Fatalf("mise en file attendue, got queued=%v err=%v", queued, err)
	}
	// Réessai du même envoi (réponse perdue) : pas de doublon.
	if _, err := c.EnqueueOrStart("cid-1", "précision", nil, Caps{}, 0.7); err != ErrDupSend {
		t.Fatalf("doublon attendu, got %v", err)
	}
	msgs := c.drainQueued(c.epoch)
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("un message user attendu, got %#v", msgs)
	}
	if len(c.Log) != 1 || c.Log[0].Delta["user"] != "précision" {
		t.Fatalf("le message injecté doit apparaître dans le fil, journal : %#v", c.Log)
	}
	if again := c.drainQueued(c.epoch); len(again) != 0 {
		t.Fatal("la file doit être vidée par drainQueued")
	}
}

// Une tâche planifiée qui occupe le modèle garde le refus : sa fin ne dépile rien.
func TestQueueRefusedDuringTask(t *testing.T) {
	c := newTestConv()
	c.Generating = true
	c.runningTaskName = "veille"
	if queued, _ := c.EnqueueOrStart("", "x", nil, Caps{}, 0.7); queued {
		t.Fatal("pas de file pendant une tâche planifiée")
	}
}

// Stop : la file est abandonnée et le client en est prévenu.
func TestDropQueuedAnnounces(t *testing.T) {
	c := newTestConv()
	c.Generating = true
	_, _ = c.EnqueueOrStart("", "a", nil, Caps{}, 0.7)
	c.dropQueued(c.epoch)
	if len(c.queued) != 0 || len(c.Log) != 1 || c.Log[0].Delta["queue_dropped"] != 1 {
		t.Fatalf("abandon non signalé : file=%d journal=%#v", len(c.queued), c.Log)
	}
}

// Un client dont la discussion affichée n'est plus l'active reçoit d'abord un
// reset, pour ne pas greffer le nouveau fil sur l'ancien.
func TestSubscribeStaleConvResets(t *testing.T) {
	testHome(t)
	_ = putStr(bkChat, ckActive, "c-nouvelle")
	c := newTestConv()
	for i := 0; i < 3; i++ {
		c.appendDelta(c.epoch, map[string]any{"user": "q"})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var first map[string]any
	c.SubscribeTail(ctx, 2, -1, "c-ancienne", func(m map[string]any) bool {
		if _, pad := m["pad"]; pad {
			return true
		}
		if first == nil {
			first = m
		}
		if _, ok := m["caught_up"]; ok {
			cancel()
			return false
		}
		return true
	})
	if first["reset"] != true || first["id"] != "c-nouvelle" {
		t.Fatalf("reset attendu en premier, got %v", first)
	}
}
