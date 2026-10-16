package loki

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// projSnapFacade : /internal/v1/turn/view avec la clé, décodé.
type projSnapFacadeView struct {
	Head     []Message `json:"head"`
	Prefix   string    `json:"prefix"`
	Tools    []Tool    `json:"tools"`
	ProjSnap struct {
		Refresh bool          `json:"refresh"`
		Delta   string        `json:"delta"`
		Snap    *projSnapshot `json:"snap"`
		Key     string        `json:"key"`
		SysHash string        `json:"sys_hash"`
	} `json:"projsnap"`
}

func projSnapFacade(t *testing.T, caps Caps, snap *projSnapshot, dry bool) projSnapFacadeView {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"caps": caps, "snap": snap, "dry": dry})
	w := appelOAIInterne(t, "POST", "/internal/v1/turn/view", string(body))
	if w.Code != 200 {
		t.Fatalf("vue : %d %s", w.Code, w.Body.String())
	}
	var v projSnapFacadeView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// msgsJSON : la séquence telle qu'elle part (forme JSON, sans les champs
// propres au Go).
func msgsJSON(t *testing.T, msgs []Message) string {
	t.Helper()
	b, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// La façade assemble ce que turnView aurait envoyé : instantané pris au
// premier tour, mise à jour posée en tête du message suivant après un
// changement, l'instantané de la façade rangé et relu.
func TestFacadeProjSnapCommeTurnView(t *testing.T) {
	projSnapSetup(t, true)
	t.Setenv(internalTokenEnv, jetonTest)
	caps := Caps{Agent: true, Mem: MemAlways}

	// Premier tour : rien de figé, instantané neuf.
	v := projSnapFacade(t, caps, nil, false)
	if !v.ProjSnap.Refresh || v.ProjSnap.Snap == nil || v.ProjSnap.Delta != "" {
		t.Fatalf("premier tour : %+v", v.ProjSnap)
	}
	c := newTestConv()
	msgs := []Message{um("bonjour")}
	c.Messages = append([]Message(nil), msgs...)
	sent, tools, _ := c.turnView(caps, c.epoch, msgs, true)
	got := withPrefix(append(append([]Message(nil), v.Head...), msgs...), v.Prefix)
	if msgsJSON(t, got) != msgsJSON(t, sent) {
		t.Fatalf("séquence différente de turnView :\n%s\n%s", msgsJSON(t, got), msgsJSON(t, sent))
	}
	if !reflect.DeepEqual(jsonValue(t, v.Tools), jsonValue(t, tools)) {
		t.Fatal("outils différents de turnView")
	}
	if !reflect.DeepEqual(v.ProjSnap.Snap.State, c.ProjSnap.State) || v.ProjSnap.Snap.Key != c.ProjSnap.Key || v.ProjSnap.Snap.SysHash != c.ProjSnap.SysHash {
		t.Fatalf("instantané différent :\n%+v\n%+v", v.ProjSnap.Snap, c.ProjSnap)
	}
	if v.ProjSnap.Key != c.ProjSnap.Key || v.ProjSnap.SysHash != c.ProjSnap.SysHash {
		t.Fatal("clé du tour différente")
	}

	// Une page ajoutée : bloc figé gardé, mise à jour annoncée.
	if err := MemAdd("nas.md", "# NAS\n"); err != nil {
		t.Fatal(err)
	}
	snap := v.ProjSnap.Snap
	v2 := projSnapFacade(t, caps, snap, false)
	if v2.ProjSnap.Refresh || !strings.Contains(v2.ProjSnap.Delta, "](nas.md)") {
		t.Fatalf("mise à jour attendue : %+v", v2.ProjSnap)
	}
	if msgsJSON(t, v2.Head) != msgsJSON(t, v.Head) {
		t.Fatal("bloc figé non gardé")
	}
	c.Messages = append(c.Messages, am("ok."), um("et le NAS ?"))
	msgs2 := append([]Message(nil), c.Messages...)
	sent2, _, _ := c.turnView(caps, c.epoch, msgs2, true)
	last := msgText(sent2[len(sent2)-1])
	if last != v2.ProjSnap.Delta+"et le NAS ?" {
		t.Fatalf("mise à jour différente de turnView :\n%q\n%q", last, v2.ProjSnap.Delta+"et le NAS ?")
	}
	next := v2.ProjSnap.Snap
	if next == nil || next.State.Mem["nas.md"] == "" || next.DeltaTok <= snap.DeltaTok || !reflect.DeepEqual(next.State, c.ProjSnap.State) {
		t.Fatalf("état annoncé non suivi : %+v", next)
	}
	// Sans nouveau changement : rien à poser, instantané inchangé.
	v3 := projSnapFacade(t, caps, next, false)
	if v3.ProjSnap.Refresh || v3.ProjSnap.Delta != "" || !reflect.DeepEqual(v3.ProjSnap.Snap.State, next.State) {
		t.Fatalf("tour sans changement : %+v", v3.ProjSnap)
	}
	// dry (préchauffage) : la même décision, rien à ranger.
	if err := MemAdd("docker.md", "# Docker\n"); err != nil {
		t.Fatal(err)
	}
	d := projSnapFacade(t, caps, next, true)
	if d.ProjSnap.Refresh || d.ProjSnap.Snap != nil || d.ProjSnap.Delta != "" || msgsJSON(t, d.Head) != msgsJSON(t, v.Head) {
		t.Fatalf("préchauffage : %+v", d.ProjSnap)
	}
	// Instantané d'un autre lancement (clé) : rafraîchi.
	stale := *next
	stale.Key = "autre"
	if r := projSnapFacade(t, caps, &stale, false); !r.ProjSnap.Refresh {
		t.Fatal("clé changée : rafraîchissement attendu")
	}
	if r := projSnapFacade(t, caps, &stale, true); !r.ProjSnap.Refresh || msgsJSON(t, r.Head) == msgsJSON(t, v.Head) {
		t.Fatal("préchauffage d'un instantané périmé : bloc vivant attendu")
	}
}

// Compaction en plein tour : le bloc vivant, et l'instantané qu'il devient
// avec la clé et le système du tour.
func TestFacadeProjSnapRafraichi(t *testing.T) {
	projSnapSetup(t, true)
	t.Setenv(internalTokenEnv, jetonTest)
	if err := MemAdd("nas.md", "# NAS\n"); err != nil {
		t.Fatal(err)
	}
	w := appelOAIInterne(t, "POST", "/internal/v1/turn/projsnap/refresh", `{"caps":{"Agent":true,"Mem":"always"},"key":"k1","sys_hash":"s1"}`)
	if w.Code != 200 {
		t.Fatalf("rafraîchissement : %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Block []Message     `json:"block"`
		Snap  *projSnapshot `json:"snap"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	live := projSnapCapture(Caps{Agent: true, Mem: MemAlways, envInCtx: true}, projSnapNow())
	if msgsJSON(t, out.Block) != msgsJSON(t, live.msgs) || len(out.Block) == 0 {
		t.Fatalf("bloc vivant différent : %s", msgsJSON(t, out.Block))
	}
	if out.Snap == nil || out.Snap.Key != "k1" || out.Snap.SysHash != "s1" || out.Snap.State.Mem["nas.md"] == "" {
		t.Fatalf("instantané : %+v", out.Snap)
	}
	if w := appelOAIInterne(t, "POST", "/internal/v1/turn/projsnap/refresh", `{`); w.Code != 400 {
		t.Fatalf("corps illisible : %d", w.Code)
	}
}

// withPrefix : ce que la façade fait du préambule rendu (inject_prefix,
// comme InjectSkills avec ce texte).
func withPrefix(msgs []Message, prefix string) []Message {
	if prefix == "" {
		return msgs
	}
	if len(msgs) > 0 && msgs[0].Role == "system" && !isProjectSystem(msgs[0]) {
		existing, _ := msgs[0].Content.(string)
		return append([]Message{{Role: "system", Content: prefix + "\n\n" + existing}}, msgs[1:]...)
	}
	return append([]Message{{Role: "system", Content: prefix}}, msgs...)
}
