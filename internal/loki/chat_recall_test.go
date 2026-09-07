package loki

import (
	"strings"
	"testing"
)

// bigBlock fabrique un contenu franchement au-dessus de recallArchiveMinLen,
// pour qu'il soit archivé plutôt que simplement porté par le résumé.
func bigBlock(marker string) string {
	return marker + "\n" + strings.Repeat("ligne de contenu volumineux à conserver verbatim\n", 40)
}

// Un bloc archivé doit revenir MOT POUR MOT : c'est toute la promesse de recall.
// Un résumé approximatif suffisait déjà avant ; ce qu'on ajoute ici, c'est
// l'exactitude — un gros code ou un diff ne survit pas à une reformulation.
func TestRecallRendVerbatim(t *testing.T) {
	t.Setenv("LOKI_HOME", t.TempDir())

	content := bigBlock("MARQUEUR-UNIQUE-42")
	id, err := archiveRecallBlock("web_read: page", "tool", content)
	if err != nil {
		t.Fatalf("archivage impossible : %v", err)
	}
	if !strings.HasPrefix(id, "r") {
		t.Fatalf("id mal formé : %q", id)
	}
	blk, ok := recallGet(id)
	if !ok {
		t.Fatalf("bloc %q introuvable juste après son archivage", id)
	}
	if blk.Content != content {
		t.Fatal("le contenu rappelé diffère de l'original : l'archive n'est pas verbatim")
	}
	// Le passage par l'outil (ce que voit le modèle) doit aussi porter le contenu.
	if got := toolRecall(map[string]any{"id": id}); !strings.Contains(got, "MARQUEUR-UNIQUE-42") {
		t.Fatalf("l'outil recall ne rend pas le bloc : %q", got)
	}
}

// Les ids doivent être uniques et monotones : deux compactages qui se croisent ne
// doivent jamais écraser le bloc de l'autre. NextSequence le garantit côté bbolt ;
// ce test verrouille qu'on s'en sert bien.
func TestRecallIDsUniquesEtMonotones(t *testing.T) {
	t.Setenv("LOKI_HOME", t.TempDir())

	vus := map[string]bool{}
	var last uint64
	for i := 0; i < 5; i++ {
		id, err := archiveRecallBlock("bloc", "tool", bigBlock("contenu"))
		if err != nil {
			t.Fatalf("archivage %d : %v", i, err)
		}
		if vus[id] {
			t.Fatalf("id %q réutilisé : un bloc en écrase un autre", id)
		}
		vus[id] = true
		seq, ok := recallIDToSeq(id)
		if !ok {
			t.Fatalf("id illisible : %q", id)
		}
		if i > 0 && seq <= last {
			t.Fatalf("séquence non monotone : %d après %d", seq, last)
		}
		last = seq
	}
}

// recallIDToSeq doit tolérer ce que le modèle écrit réellement : avec ou sans
// préfixe, majuscule, espaces. Un id refusé pour un espace en trop enverrait le
// modèle croire que son bloc a disparu.
func TestRecallIDToSeqTolerant(t *testing.T) {
	for _, in := range []string{"r7", "R7", " r7 ", "7"} {
		if seq, ok := recallIDToSeq(in); !ok || seq != 7 {
			t.Fatalf("%q → (%d, %v), attendu (7, true)", in, seq, ok)
		}
	}
	for _, in := range []string{"", "rr", "abc", "r-1"} {
		if _, ok := recallIDToSeq(in); ok {
			t.Fatalf("%q accepté alors qu'il est invalide", in)
		}
	}
}

// recall_search est l'issue de secours quand l'id n'est plus dans le résumé
// courant : le bloc doit rester trouvable par mots-clés, et l'outil doit rendre
// son id pour que le modèle puisse enchaîner sur recall(id).
func TestRecallSearchRetrouveParMotsCles(t *testing.T) {
	t.Setenv("LOKI_HOME", t.TempDir())

	if _, err := archiveRecallBlock("web_read: recette", "tool", bigBlock("gratin dauphinois")); err != nil {
		t.Fatal(err)
	}
	wanted, err := archiveRecallBlock("web_read: horaires", "tool", bigBlock("le train de 14h12 part quai 3"))
	if err != nil {
		t.Fatal(err)
	}
	hits := recallSearch("train horaires", 0)
	if len(hits) == 0 {
		t.Fatal("aucun résultat pour des mots-clés pourtant présents")
	}
	if hits[0].ID != wanted {
		t.Fatalf("meilleur résultat %q, attendu %q (le libellé doit compter double)", hits[0].ID, wanted)
	}
	out := toolRecallSearch(map[string]any{"query": "train horaires"})
	if !strings.Contains(out, wanted) {
		t.Fatalf("l'outil recall_search ne rend pas l'id à rappeler : %q", out)
	}
	if n := len(recallSearch("motabsolumentabsent", 0)); n != 0 {
		t.Fatalf("%d bloc(s) remontent pour un terme absent", n)
	}
}

// Le cœur du lot : un gros bloc du torse doit être ARCHIVÉ avant compaction, et
// l'historique compacté doit porter son id. Sans llama-server en test, le résumé
// échoue et on tombe sur le repli dégraissé — c'est justement le chemin où
// l'ancien code effaçait le contenu sans laisser d'adresse.
func TestCompactArchiveLesGrosBlocs(t *testing.T) {
	t.Setenv("LOKI_HOME", t.TempDir())

	page := func(n int) Message {
		return tm(bigBlock("page numéro " + string(rune('a'+n))))
	}
	msgs := []Message{um("cherche les horaires du train pour Lyon")}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, atc("web_read"), page(i))
	}
	// caps.Agent : sans le mode agent, le modèle n'aurait pas l'outil recall et on
	// n'archive délibérément rien.
	out, changed := compactMessages(t.Context(), msgs, Caps{Agent: true})
	if !changed {
		t.Fatal("aucune compaction alors que le torse est plein de grosses pages")
	}
	joined := ""
	for _, m := range out {
		joined += msgText(m) + "\n"
	}
	if !strings.Contains(joined, "recall(") {
		t.Fatalf("l'historique compacté ne cite aucun bloc rappelable :\n%s", joined)
	}
	if strings.Contains(joined, compactPrunedMarker) {
		t.Fatal("un gros bloc a été effacé sans adresse de rappel alors qu'il était archivable")
	}
	// Et le contenu doit être réellement récupérable, pas seulement cité.
	hits := recallSearch("page numéro", 0)
	if len(hits) == 0 {
		t.Fatal("les blocs cités dans l'historique ne sont pas dans l'archive")
	}
	if !strings.Contains(hits[0].Content, "ligne de contenu volumineux") {
		t.Fatal("le bloc archivé ne contient pas le texte d'origine")
	}
}

// Sans mode agent, le modèle n'a pas l'outil recall : archiver produirait des ids
// qu'il ne peut pas utiliser, et un résumé pollué de références mortes.
func TestCompactNArchivePasSansAgent(t *testing.T) {
	t.Setenv("LOKI_HOME", t.TempDir())

	msgs := []Message{um("question")}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, atc("web_read"), tm(bigBlock("page")))
	}
	out, _ := compactMessages(t.Context(), msgs, Caps{})
	for _, m := range out {
		if strings.Contains(msgText(m), "recall(") {
			t.Fatal("des références recall sont proposées alors que l'outil n'est pas fourni")
		}
	}
	if len(recallSearch("page", 0)) != 0 {
		t.Fatal("des blocs ont été archivés hors mode agent")
	}
}

// recallEligible décide ce qui mérite un id. Deux pièges : les petits blocs (le
// résumé les porte très bien) et le résumé d'une compaction PRÉCÉDENTE, qu'on
// ré-archiverait à chaque passe en faisant enfler l'archive pour rien.
func TestRecallEligible(t *testing.T) {
	gros := strings.Repeat("x", recallArchiveMinLen+1)
	if !recallEligible(tm(gros)) {
		t.Fatal("un gros résultat d'outil doit être archivé")
	}
	if recallEligible(tm("court")) {
		t.Fatal("un petit bloc ne mérite pas un id : le résumé le porte")
	}
	if recallEligible(Message{Role: "system", Content: gros}) {
		t.Fatal("un message système n'a rien à faire dans l'archive")
	}
	if recallEligible(um(compactSummaryPrefix + " " + gros)) {
		t.Fatal("le résumé d'une compaction précédente serait ré-archivé à chaque passe")
	}
}

// Garde-fou anti-résumé-dégénéré : un petit modèle mal aiguillé répond parfois
// par un simple « recall:r7 » au lieu d'écrire de la prose. Accepter ça, c'est
// remplacer tout le passé de la conversation par une référence vide.
func TestSummaryLooksEmpty(t *testing.T) {
	vrai := "L'utilisateur cherche les horaires du train pour Lyon ; le train de 14h12 part quai 3, reste à confirmer le tarif."
	if summaryLooksEmpty(vrai) {
		t.Fatalf("un vrai résumé est rejeté : %q", vrai)
	}
	for _, degenere := range []string{"", "   ", "recall:r7", "recall:r7 recall:r8 recall:r9", "voir recall(r7)."} {
		if !summaryLooksEmpty(degenere) {
			t.Fatalf("résumé dégénéré accepté : %q", degenere)
		}
	}
}

// compactSummaryUserMsg porte l'index des ids rappelables. Il doit rester borné
// à la passe courante : sinon une conversation infinie ferait enfler cette
// section jusqu'à annuler le bénéfice de la compaction.
func TestCompactSummaryUserMsg(t *testing.T) {
	sans := compactSummaryUserMsg("résumé quelconque", nil)
	if !strings.HasPrefix(sans, compactSummaryPrefix) {
		t.Fatal("le message de compaction doit être reconnaissable à son préfixe")
	}
	if strings.Contains(sans, "recall(") {
		t.Fatal("recall est proposé alors qu'aucun bloc n'a été archivé")
	}
	avec := compactSummaryUserMsg("résumé quelconque", []recallEntry{{id: "r7", label: "web_read: horaires"}})
	if !strings.Contains(avec, "r7") || !strings.Contains(avec, "web_read: horaires") {
		t.Fatalf("l'index des blocs rappelables est absent :\n%s", avec)
	}
}
