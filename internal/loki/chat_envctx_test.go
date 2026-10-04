package loki

import (
	"encoding/json"
	"os"
	"os/user"
	"runtime"
	"strings"
	"testing"
	"time"
)

// sysPlaceholders remplace dans un prompt système les valeurs propres à la
// machine et au jour (date, année, hôte, utilisateur, dossiers) par des jetons
// fixes : ce qui reste doit être identique, octet pour octet, d'une machine à
// l'autre.
func sysPlaceholders(s string) string {
	now := time.Now()
	year := now.Format("2006")
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	who := ""
	if u, err := user.Current(); err == nil {
		who = u.Username
	}
	pairs := []string{
		now.Format("2006-01-02"), "<DATE>",
		scriptsDir(), "<SCRIPTS>",
		agentCwd(), "<CWD>",
		"host=" + host, "host=<HOST>",
		runtime.GOOS + "/" + runtime.GOARCH, "<OS>/<ARCH>",
		"The shell is " + agentTargetShellName(), "The shell is <SHELL>",
	}
	if who != "" {
		pairs = append(pairs, "user="+who, "user=<USER>")
	}
	pairs = append(pairs, year, "<YEAR>", prevYear(year), "<PREVYEAR>")
	for i := 0; i < len(pairs); i += 2 {
		s = strings.ReplaceAll(s, pairs[i], pairs[i+1])
	}
	return s
}

func sysOf(t *testing.T, msgs []Message) string {
	t.Helper()
	if len(msgs) == 0 || msgs[0].Role != "system" {
		t.Fatalf("pas de système en tête : %+v", msgs)
	}
	s, _ := msgs[0].Content.(string)
	return s
}

// Gabarits du système SANS PROJ_SNAPSHOT, relevés sur le code d'avant le
// déplacement de la date et du dossier de travail : ils ne doivent pas bouger
// d'un octet (valeurs du jour et de la machine remplacées par des jetons).
var sysGolden = map[string]string{
	"agent":    "You are Loki, an expert assistant operating directly on this machine with real tools. You evolve with every conversation: you actively maintain a persistent memory so nothing useful is lost between sessions.\n\nThe shell is <SHELL>: use its syntax.\n\nManaging your memory is part of the job, not optional:\n- Save anything worth keeping (a preference, fact, decision, how-to) with mem_add, or mem_edit to update a page — on your own, without being asked.\n- The memory index (page list) is in your context: when a page looks relevant, mem_read it directly. mem_search is only for finding something by content.\nFor anything about the system or files, use bash instead of guessing. Act immediately — call the right tool, then answer. Never end your turn after only thinking. Be concise.\nTo give the user a file, link it in Markdown with its path relative to your working directory — [le rapport](rapport.pdf) — which downloads it. A raw server path is useless: they read you in a browser.\nYou can schedule work for yourself with task_create — only for what must recur or happen later, never a one-off you can do now. A script from your scripts folder runs as a task with no model at all.\n\nDate: <DATE>\n\nMachine: host=<HOST>, <OS>/<ARCH>, user=<USER>, cwd=<CWD>. This is your working folder: relative paths in write/edit/bash resolve here, and it is the DEFAULT place for scratch work — notes, outputs, a clone, a test. But it is DISPOSABLE: deleting the discussion wipes it. Any script you want to KEEP (or schedule), write it into your scripts folder <SCRIPTS> instead — a separate folder a workspace wipe won't touch; you write and run scripts there normally. Do NOT install or write files into system directories such as /usr/local/bin, /usr, /bin or /etc: those need root and are not yours. Only use an absolute path outside this folder (except your scripts folder) when the user explicitly named that location.",
	"internet": "You are Loki, an expert assistant operating directly on this machine with real tools. You evolve with every conversation: you actively maintain a persistent memory so nothing useful is lost between sessions.\n\nThe shell is <SHELL>: use its syntax.\n\nManaging your memory is part of the job, not optional:\n- Save anything worth keeping (a preference, fact, decision, how-to) with mem_add, or mem_edit to update a page — on your own, without being asked.\n- The memory index (page list) is in your context: when a page looks relevant, mem_read it directly. mem_search is only for finding something by content.\nFor anything about the system or files, use bash instead of guessing. Act immediately — call the right tool, then answer. Never end your turn after only thinking. Be concise.\nTo give the user a file, link it in Markdown with its path relative to your working directory — [le rapport](rapport.pdf) — which downloads it. A raw server path is useless: they read you in a browser.\nYou can schedule work for yourself with task_create — only for what must recur or happen later, never a one-off you can do now. A script from your scripts folder runs as a task with no model at all.\n\nWeb: web_open first, then web_read/web_grep on it.\nYour training data is stale. For ANY question about recent/latest/current things (releases, versions, news, prices, scores, 'since when') call web_search BEFORE writing any date or version, and match what you actually read.\nToday is in <YEAR>. If a query needs a year use ONLY <YEAR>, never a remembered past year like <PREVYEAR> — it biases results toward stale pages; better still, omit the year. Don't hedge ('probably') about a fact a tool can verify — search instead.\n\nDate: <DATE>\n\nMachine: host=<HOST>, <OS>/<ARCH>, user=<USER>, cwd=<CWD>. This is your working folder: relative paths in write/edit/bash resolve here, and it is the DEFAULT place for scratch work — notes, outputs, a clone, a test. But it is DISPOSABLE: deleting the discussion wipes it. Any script you want to KEEP (or schedule), write it into your scripts folder <SCRIPTS> instead — a separate folder a workspace wipe won't touch; you write and run scripts there normally. Do NOT install or write files into system directories such as /usr/local/bin, /usr, /bin or /etc: those need root and are not yours. Only use an absolute path outside this folder (except your scripts folder) when the user explicitly named that location.",
	"ondemand": "You are Loki, an expert assistant operating directly on this machine with real tools.\n\nThe shell is <SHELL>: use its syntax.\n\nMemory is ON-DEMAND: you have the mem_* tools but do NOT read or write memory on your own. Call mem_search/mem_read only when the user explicitly asks you to recall or look something up, and mem_add/mem_edit only when the user explicitly asks you to remember something. Otherwise leave memory untouched and answer directly.\nFor anything about the system or files, use bash instead of guessing. Act immediately — call the right tool, then answer. Never end your turn after only thinking. Be concise.\nTo give the user a file, link it in Markdown with its path relative to your working directory — [le rapport](rapport.pdf) — which downloads it. A raw server path is useless: they read you in a browser.\nYou can schedule work for yourself with task_create — only for what must recur or happen later, never a one-off you can do now. A script from your scripts folder runs as a task with no model at all.\n\nDate: <DATE>\n\nMachine: host=<HOST>, <OS>/<ARCH>, user=<USER>, cwd=<CWD>. This is your working folder: relative paths in write/edit/bash resolve here, and it is the DEFAULT place for scratch work — notes, outputs, a clone, a test. But it is DISPOSABLE: deleting the discussion wipes it. Any script you want to KEEP (or schedule), write it into your scripts folder <SCRIPTS> instead — a separate folder a workspace wipe won't touch; you write and run scripts there normally. Do NOT install or write files into system directories such as /usr/local/bin, /usr, /bin or /etc: those need root and are not yours. Only use an absolute path outside this folder (except your scripts folder) when the user explicitly named that location.",
	"memseule": "You are Loki, an expert assistant operating directly on this machine with real tools. You evolve with every conversation: you actively maintain a persistent memory so nothing useful is lost between sessions.\n\n\nManaging your memory is part of the job, not optional:\n- Save anything worth keeping (a preference, fact, decision, how-to) with mem_add, or mem_edit to update a page — on your own, without being asked.\n- The memory index (page list) is in your context: when a page looks relevant, mem_read it directly. mem_search is only for finding something by content.\n\nDate: <DATE>",
}

func TestSystemeSansCleInchange(t *testing.T) {
	projSnapSetup(t, false)
	cases := map[string]Caps{
		"agent":    {Agent: true, Mem: MemAlways},
		"internet": {Agent: true, Internet: true, Mem: MemAlways},
		"ondemand": {Agent: true, Mem: MemOnDemand},
		"memseule": {Mem: MemAlways},
	}
	for name, caps := range cases {
		got, _ := prepareTurn([]Message{um("salut")}, caps)
		s := sysPlaceholders(sysOf(t, got))
		if want, ok := sysGolden[name]; !ok || s != want {
			t.Errorf("%s : système changé sans la clé\nreçu   %q\nattendu %q", name, s, want)
		}
	}
}

// toolsOf : le champ tools d'une requête, tel qu'envoyé.
func toolsOf(t *testing.T, body string) string {
	t.Helper()
	var p struct {
		Tools json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	return string(p.Tools)
}

// Avec la clé : deux discussions dans deux dossiers ont le MÊME système et les
// mêmes outils ; la date et le dossier sont dans le bloc projet de chacune.
func TestEnvCtxSystemeCommunAuxDiscussions(t *testing.T) {
	m := projSnapSetup(t, true)
	t.Cleanup(func() { setTaskWorkspace("") })
	caps := Caps{Agent: true, Internet: true, Mem: MemAlways}
	var bodies []string
	dirs := []string{t.TempDir(), t.TempDir()}
	for _, d := range dirs {
		setTaskWorkspace(d)
		bodies = append(bodies, playTurn(t, m, newTestConv(), caps, "salut"))
	}
	s0, s1 := systemContent(t, bodies[0]), systemContent(t, bodies[1])
	if s0 != s1 || toolsOf(t, bodies[0]) != toolsOf(t, bodies[1]) {
		t.Fatalf("système ou outils différents d'une discussion à l'autre :\n%q\n%q", s0, s1)
	}
	today := time.Now().Format("2006-01-02")
	year := today[:4]
	for _, v := range []string{today, year, dirs[0], dirs[1], "cwd="} {
		if strings.Contains(s0, v) {
			t.Fatalf("%q encore dans le système : %q", v, s0)
		}
	}
	if !strings.Contains(s0, "Your working folder (cwd) is given in your context") || !strings.Contains(s0, scriptsDir()) {
		t.Fatalf("consignes du dossier de travail perdues : %q", s0)
	}
	for i, b := range bodies {
		u := userContents(t, b)[0]
		if !strings.Contains(u, envContextPrefix+":\n"+envDateLine(today)) || !strings.Contains(u, "Working folder (cwd): "+dirs[i]+".") {
			t.Fatalf("discussion %d : date ou dossier absents du bloc : %q", i, u)
		}
	}
}

// Changement de jour : une ligne de mise à jour, une seule fois ; système et
// bloc figé inchangés, donc tout le début du prompt reste en cache.
func TestEnvCtxChangementDeJour(t *testing.T) {
	m := projSnapSetup(t, true)
	day := time.Date(2026, 12, 31, 23, 50, 0, 0, time.Local)
	envNow = func() time.Time { return day }
	t.Cleanup(func() { envNow = time.Now })
	c := newTestConv()
	caps := Caps{Agent: true, Mem: MemAlways}
	b1 := playTurn(t, m, c, caps, "salut")
	day = day.Add(20 * time.Minute)
	b2 := playTurn(t, m, c, caps, "et maintenant ?")
	b3 := playTurn(t, m, c, caps, "merci")
	if systemContent(t, b1) != systemContent(t, b2) || systemContent(t, b2) != systemContent(t, b3) {
		t.Fatal("le système a changé avec le jour")
	}
	u1, u2, u3 := userContents(t, b1), userContents(t, b2), userContents(t, b3)
	if u1[0] != u2[0] || u2[0] != u3[0] || !strings.Contains(u1[0], "Date: 2026-12-31 (the current year is 2026, not 2025).") {
		t.Fatalf("bloc figé modifié ou sans date : %q / %q", u1[0], u2[0])
	}
	want := ctxUpdOpen
	last := u2[len(u2)-1]
	if !strings.HasPrefix(last, want) || !strings.Contains(last, "New day — Date: 2027-01-01 (the current year is 2027, not 2026).") {
		t.Fatalf("mise à jour du jour attendue : %q", last)
	}
	if u3[1] != last || strings.Contains(u3[len(u3)-1], ctxUpdOpen) {
		t.Fatalf("la mise à jour doit partir une fois, puis rester telle quelle : %q", u3)
	}
}

// Tâches, sous-agents, vérification, terminal : la clé ne change rien à leur
// système, qui garde la date et le dossier.
func TestEnvCtxHorsDiscussionInchange(t *testing.T) {
	projSnapSetup(t, true)
	caps := Caps{Agent: true, Mem: MemAlways}
	got, _ := prepareTurn([]Message{um("tâche")}, caps)
	s := sysOf(t, got)
	if !strings.Contains(s, "\nDate: "+time.Now().Format("2006-01-02")) || !strings.Contains(s, ", cwd="+agentCwd()+".") {
		t.Fatalf("système d'une tâche modifié par la clé : %q", s)
	}
	if mp := machineSystemPrompt(caps); !strings.Contains(mp, "cwd="+agentCwd()) {
		t.Fatalf("briefing d'un sous-agent sans dossier : %q", mp)
	}
}

// Poste distant passé hors ligne, autre dossier : le bloc entier repart, rien
// n'est retenu d'avant.
func TestEnvDelta(t *testing.T) {
	if d := envDelta("2026-10-04", "x", "2026-10-04", "x"); d != "" {
		t.Fatalf("rien de changé : %q", d)
	}
	if d := envDelta("2026-10-04", "Working folder (cwd): /a.", "2026-10-04", "Machine: REMOTE node pc. "+nodeOfflineLine); !strings.Contains(d, "Current version") || !strings.Contains(d, nodeOfflineLine) || !strings.Contains(d, "Date: 2026-10-04") {
		t.Fatalf("bloc entier attendu : %q", d)
	}
	if d := envDelta("2026-10-04", "x", "2026-10-05", "x"); d != "New day — "+envDateLine("2026-10-05")+"\n" {
		t.Fatalf("une ligne attendue : %q", d)
	}
}
