package loki

// chat_search_test.go — recherche plein-texte dans les discussions. Repris
// d'AJEAN (chat_search_test.go, commit 558d3b4d, issue amont #98) et adaptés au
// modèle de Loki : pas d'archives à part, toutes les discussions vivent dans
// bkChat/conv:<id> et l'index suit chaque écriture de l'écrivain différé
// (chat_persist.go) — ou de la façade, par /internal/v1/store. S'y ajoutent les
// cas propres à Loki : écritures de la façade, extrait en contexte, mise à jour
// incrémentale, index périmé.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- aides --------------------------------------------------------------------

// histSetup isole $LOKI_HOME et neutralise la discussion ouverte du process :
// chaque test pose explicitement celle qu'il veut (histSetLive).
func histSetup(t *testing.T) {
	t.Helper()
	testHome(t)
	clearMemDEK()
	conv.mu.Lock()
	prevLog := conv.Log
	conv.Log = nil
	conv.mu.Unlock()
	convActiveCache.Store(nil)
	t.Cleanup(func() {
		conv.mu.Lock()
		conv.Log = prevLog
		conv.mu.Unlock()
		convActiveCache.Store(nil)
		clearMemDEK()
	})
}

func histUser(ts int64, s string) LogEvent {
	return LogEvent{TS: ts, Delta: map[string]any{"user": s}}
}

func histContent(ts int64, s string) LogEvent {
	return LogEvent{TS: ts, Delta: map[string]any{"content": s}}
}

func histBody(t *testing.T, log []LogEvent) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"messages": []Message{}, "log": log})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// histConv écrit une discussion PAR LE CHEMIN DE PRODUCTION (l'écrivain
// différé, qui pose contenu, entrée d'index et index plein-texte), puis fixe
// son titre et sa date dans l'index des discussions.
func histConv(t *testing.T, id, project, title string, updated int64, log []LogEvent) {
	t.Helper()
	if project == "" {
		project = defaultProjectSlug
	}
	turns := 0
	for _, ev := range log {
		if _, ok := ev.Delta["user"]; ok {
			turns++
		}
	}
	persistQ.wait(persistQ.enqueueSnap(&convSnap{path: dbPath(), id: id, seq: persistSeq.Add(1),
		body: histBody(t, log), title: title, turns: turns, project: project}))
	histSetMeta(t, convMeta{ID: id, Title: title, Created: updated, Updated: updated, Turns: turns, Project: project})
}

// histSetMeta pose (ou remplace) l'entrée d'une discussion dans l'index des
// discussions, sans toucher à son contenu.
func histSetMeta(t *testing.T, m convMeta) {
	t.Helper()
	convIndexMu.Lock()
	defer convIndexMu.Unlock()
	idx := convIndex()
	for i := range idx {
		if idx[i].ID == m.ID {
			idx[i] = m
			convIndexSave(idx)
			return
		}
	}
	convIndexSave(append(idx, m))
}

// histSetLive fait de id la discussion ouverte, avec ce journal en RAM.
func histSetLive(t *testing.T, id string, log []LogEvent) {
	t.Helper()
	convActiveRemember(id)
	conv.mu.Lock()
	conv.Log = append([]LogEvent(nil), log...)
	conv.mu.Unlock()
}

func histHasHit(res histSearchResult, id string) bool {
	for _, h := range res.Hits {
		if h.ID == id {
			return true
		}
	}
	return false
}

func histHitIDs(res histSearchResult) string {
	var ids []string
	for _, h := range res.Hits {
		ids = append(ids, h.ID)
	}
	return strings.Join(ids, ",")
}

func histDocExists(t *testing.T, id string) bool {
	t.Helper()
	_, ok := getStoreBytes(bkChatSearch, histDocKey(id))
	return ok
}

func histPosting(t *testing.T, tok string) []string {
	t.Helper()
	b, ok := getStoreBytes(bkChatSearch, histPostKey(tok))
	if !ok {
		return nil
	}
	var ids []string
	if err := json.Unmarshal(b, &ids); err != nil {
		t.Fatalf("posting %q illisible : %v", tok, err)
	}
	return ids
}

// --- 1. texte trouvé même si le titre ne contient rien ------------------------

func TestHistSearchBodyMatch(t *testing.T) {
	histSetup(t)
	histConv(t, "A", "", "Voyage en Bretagne", 100, []LogEvent{histUser(1, "parlons du phare de Brest")})
	histConv(t, "B", "", "Recette", 200, []LogEvent{histUser(1, "préparons une tarte")})

	res := histSearch("phare", "", 0, 0)
	if res.State != "ok" {
		t.Fatalf("état attendu ok, obtenu %q", res.State)
	}
	if res.Total != 1 || len(res.Hits) != 1 || res.Hits[0].ID != "A" {
		t.Fatalf("A attendu seul, obtenu total=%d %+v", res.Total, res.Hits)
	}
	if res.Hits[0].Snippet == "" {
		t.Fatal("extrait attendu non vide pour A")
	}
	if res.Hits[0].InTitle {
		t.Fatal("A est trouvée par son texte, pas par son titre")
	}
}

// --- 2. un mot du titre pèse plus qu'un mot du texte --------------------------

// Le titre compte double (+2/+1, comme recall_search). B est plus RÉCENTE que
// A : un tri par date seule inverserait l'ordre.
func TestHistSearchTitleBeatsBody(t *testing.T) {
	histSetup(t)
	histConv(t, "A", "", "Le phare de Brest", 100, []LogEvent{histUser(1, "rien de spécial")})
	histConv(t, "B", "", "Discussion diverse", 200, []LogEvent{histUser(1, "on a vu un phare hier")})

	res := histSearch("phare", "", 0, 0)
	if got := histHitIDs(res); got != "A,B" {
		t.Fatalf("ordre attendu A,B, obtenu %s", got)
	}
	if !res.Hits[0].InTitle || res.Hits[1].InTitle {
		t.Fatalf("in_title attendu vrai pour A seulement : %+v", res.Hits)
	}
}

// --- 3. pertinence, puis date, puis identifiant -------------------------------

func TestHistSearchRelevanceThenDate(t *testing.T) {
	histSetup(t)
	histConv(t, "A", "", "Sans rapport", 200, []LogEvent{histUser(1, "un phare dans le texte")})
	histConv(t, "B", "", "Phare ancien", 100, []LogEvent{histUser(1, "texte neutre")})
	histConv(t, "C", "", "Phare récent", 300, []LogEvent{histUser(1, "texte neutre")})

	if got := histHitIDs(histSearch("phare", "", 0, 0)); got != "C,B,A" {
		t.Fatalf("ordre attendu C,B,A, obtenu %s", got)
	}
}

// --- 4. la discussion OUVERTE est cherchée dans sa version en RAM -------------

// En plein tour, le journal en mémoire est plus frais que la dernière écriture :
// le mot vient d'arriver, il n'est pas encore dans l'index.
func TestHistSearchIncludesActiveSession(t *testing.T) {
	histSetup(t)
	histConv(t, "A", "", "Autre sujet", 100, []LogEvent{histUser(1, "on parle boussole")})
	histConv(t, "live1", "", "salut", 200, []LogEvent{histUser(1, "salut")})
	histSetLive(t, "live1", []LogEvent{histUser(1, "salut"), histContent(2, "le phare en direct")})

	res := histSearch("phare", "", 0, 0)
	if res.Total != 1 || len(res.Hits) != 1 || res.Hits[0].ID != "live1" {
		t.Fatalf("la discussion ouverte devait ressortir seule : %+v", res.Hits)
	}
	if !strings.Contains(res.Hits[0].Snippet, "phare") {
		t.Fatalf("extrait tiré du journal en RAM attendu, obtenu %q", res.Hits[0].Snippet)
	}
}

// --- 5. chercher n'écrit RIEN -------------------------------------------------

func TestHistSearchDoesNotWrite(t *testing.T) {
	histSetup(t)
	histConv(t, "live1", "", "salut", 100, []LogEvent{histUser(1, "salut")})
	histSetLive(t, "live1", []LogEvent{histUser(1, "le phare en direct")})
	beforeChat, beforeIdx := allKV(bkChat), allKV(bkChatSearch)

	_ = histSearch("phare", "", 0, 0)

	if after := allKV(bkChat); len(after) != len(beforeChat) || after[convKey("live1")] != beforeChat[convKey("live1")] || after[ckIndex] != beforeChat[ckIndex] {
		t.Fatal("la recherche a réécrit une discussion ou l'index des discussions")
	}
	after := allKV(bkChatSearch)
	if len(after) != len(beforeIdx) {
		t.Fatalf("la recherche a modifié l'index plein-texte (%d → %d clés)", len(beforeIdx), len(after))
	}
	for k, v := range beforeIdx {
		if after[k] != v {
			t.Fatalf("la recherche a réécrit %q", k)
		}
	}
}

// --- 6. cloisonnement par projet ----------------------------------------------

func TestHistSearchProjectScope(t *testing.T) {
	histSetup(t)
	histConv(t, "A", "alpha", "Projet alpha", 100, []LogEvent{histUser(1, "le phare")})
	histConv(t, "B", "beta", "Projet beta", 100, []LogEvent{histUser(1, "le phare")})

	if got := histHitIDs(histSearch("phare", "alpha", 0, 0)); got != "A" {
		t.Fatalf("projet alpha : A attendu seul, obtenu %s", got)
	}
	if got := histHitIDs(histSearch("phare", "beta", 0, 0)); got != "B" {
		t.Fatalf("projet beta : B attendu seul, obtenu %s", got)
	}
	// Sans projet donné : celui de la barre latérale, lu en base.
	if err := setActiveProject("beta"); err != nil {
		t.Fatal(err)
	}
	if got := histHitIDs(histSearch("phare", "", 0, 0)); got != "B" {
		t.Fatalf("projet actif beta : B attendu seul, obtenu %s", got)
	}
}

// --- 7. mémoire verrouillée = état explicite, pas « rien trouvé » -------------

func TestHistSearchLockedState(t *testing.T) {
	histSetup(t)
	histConv(t, "A", "", "Secret", 100, []LogEvent{histUser(1, "le phare secret")})
	if _, err := EnableMemEncryption("motdepasse-fort"); err != nil {
		t.Fatal(err)
	}
	clearMemDEK()

	res := histSearch("phare", "", 0, 0)
	if res.State != "locked" {
		t.Fatalf("état attendu locked, obtenu %q", res.State)
	}
	if len(res.Hits) != 0 || res.Total != 0 {
		t.Fatalf("aucun résultat attendu verrouillé, obtenu %+v", res.Hits)
	}
}

// --- 8. requête sans terme exploitable ----------------------------------------

func TestHistSearchNoUsableTerms(t *testing.T) {
	histSetup(t)
	histConv(t, "A", "", "Phare", 100, []LogEvent{histUser(1, "le phare")})

	res := histSearch("a!", "", 0, 0)
	if res.State != "no_terms" {
		t.Fatalf("état attendu no_terms, obtenu %q", res.State)
	}
	if res.Terms == nil || len(res.Terms) != 0 || res.Total != 0 {
		t.Fatalf("aucun terme (tableau vide, jamais null) ni résultat attendu : terms=%#v total=%d", res.Terms, res.Total)
	}
}

// --- 9. cycle de vie : écriture puis suppression de la discussion -------------

func TestHistSearchIndexLifecycle(t *testing.T) {
	histSetup(t)
	histConv(t, "lifeA", "", "A", 100, []LogEvent{histUser(1, "le phare")})
	histConv(t, "lifeB", "", "B", 200, []LogEvent{histUser(1, "le phare")})

	if !histDocExists(t, "lifeA") || !histDocExists(t, "lifeB") {
		t.Fatal("les deux fiches d| devaient exister après l'écriture")
	}
	if ids := histPosting(t, "phare"); len(ids) != 2 {
		t.Fatalf("posting phare attendu de 2, obtenu %v", ids)
	}
	if err := convDelete("lifeA"); err != nil {
		t.Fatal(err)
	}
	if histDocExists(t, "lifeA") {
		t.Fatal("d|lifeA devait disparaître avec la discussion")
	}
	if _, ok := getStoreBytes(bkChatSearch, histToksKey("lifeA")); ok {
		t.Fatal("t|lifeA devait disparaître avec la discussion")
	}
	if ids := histPosting(t, "phare"); len(ids) != 1 || ids[0] != "lifeB" {
		t.Fatalf("posting phare attendu [lifeB], obtenu %v", ids)
	}
}

// --- 9 bis. mise à jour incrémentale ------------------------------------------

// Réécrire une discussion ne touche que les mots qui ont changé — et un mot
// disparu (journal compacté, discussion vidée) quitte bien l'index.
func TestHistSearchIncrementalUpdate(t *testing.T) {
	histSetup(t)
	histConv(t, "A", "", "Neutre", 100, []LogEvent{histUser(1, "phare boussole")})
	if ids := histPosting(t, "boussole"); len(ids) != 1 {
		t.Fatalf("boussole attendu indexé, obtenu %v", ids)
	}
	histConv(t, "A", "", "Neutre", 100, []LogEvent{histUser(1, "phare compas")})
	if ids := histPosting(t, "boussole"); len(ids) != 0 {
		t.Fatalf("boussole devait quitter l'index, obtenu %v", ids)
	}
	if ids := histPosting(t, "compas"); len(ids) != 1 || ids[0] != "A" {
		t.Fatalf("compas attendu [A], obtenu %v", ids)
	}
	if ids := histPosting(t, "phare"); len(ids) != 1 {
		t.Fatalf("phare attendu une fois, obtenu %v", ids)
	}
}

// --- 10. l'index est chiffré ----------------------------------------------------

func TestHistSearchBucketEncrypted(t *testing.T) {
	histSetup(t)
	histConv(t, "A", "", "Titre secret", 100, []LogEvent{histUser(1, "le phare")})
	if raw := getBytes(bkChatSearch, histDocKey("A")); looksEncrypted(raw) {
		t.Fatal("avant activation, la valeur ne devait pas être chiffrée")
	}
	if _, err := EnableMemEncryption("motdepasse-fort"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{histDocKey("A"), histToksKey("A"), histPostKey("phare")} {
		if !looksEncrypted(getBytes(bkChatSearch, k)) {
			t.Fatalf("%s devait être chiffré (bucket absent d'encryptedBuckets ?)", k)
		}
	}
	// Écrite après l'activation : chiffrée d'emblée, et toujours trouvable.
	histConv(t, "B", "", "Autre", 200, []LogEvent{histUser(1, "le phare encore")})
	if !looksEncrypted(getBytes(bkChatSearch, histPostKey("encore"))) {
		t.Fatal("posting écrit mémoire chiffrée resté en clair")
	}
	if got := histHitIDs(histSearch("phare", "", 0, 0)); got != "B,A" {
		t.Fatalf("B,A attendus mémoire déverrouillée, obtenu %s", got)
	}
	clearMemDEK()
	if _, ok := getStoreBytes(bkChatSearch, histDocKey("A")); ok {
		t.Fatal("index relisible alors que la mémoire est verrouillée")
	}
}

// --- 11. couverture : jamais présenter un index partiel comme complet ---------

func TestHistSearchCoverageContract(t *testing.T) {
	histSetup(t)
	log := []LogEvent{histUser(1, "le phare")}
	// Écrites À LA MAIN, sans passer par l'index : des discussions d'avant la
	// fonctionnalité.
	for _, id := range []string{"a", "b", "c"} {
		if err := putStoreBytes(bkChat, convKey(id), histBody(t, log)); err != nil {
			t.Fatal(err)
		}
		histSetMeta(t, convMeta{ID: id, Title: id, Updated: 100, Project: defaultProjectSlug})
	}
	if err := histIndexAt(dbPath(), "a", log); err != nil {
		t.Fatal(err)
	}

	res := histSearch("phare", "", 0, 0)
	if res.IndexedTotal != 3 || res.Indexed != 1 {
		t.Fatalf("couverture attendue 1/3, obtenue %d/%d", res.Indexed, res.IndexedTotal)
	}
	if res.Total != 1 {
		t.Fatalf("1 seule trouvable avant rattrapage, obtenu %d", res.Total)
	}

	histSearchIndexBackfill()

	res = histSearch("phare", "", 0, 0)
	if res.Indexed != 3 || res.IndexedTotal != 3 || res.Total != 3 {
		t.Fatalf("après rattrapage : %d/%d, %d trouvées (3 attendues)", res.Indexed, res.IndexedTotal, res.Total)
	}
}

// --- 11 bis. une fiche plus ancienne que la discussion est à refaire ----------

// L'écriture a réussi, l'indexation qui suit a raté (base occupée, process
// coupé) : la fiche existe mais décrit un état antérieur. Elle ne doit pas
// compter comme à jour, et le rattrapage doit la refaire.
func TestHistSearchStaleDocReindexed(t *testing.T) {
	histSetup(t)
	histConv(t, "A", "", "Neutre", 100, []LogEvent{histUser(1, "le phare")})
	future := histReadDocs()["A"].At + 10
	if err := putStoreBytes(bkChat, convKey("A"), histBody(t, []LogEvent{histUser(1, "le phare"), histContent(2, "une boussole")})); err != nil {
		t.Fatal(err)
	}
	histSetMeta(t, convMeta{ID: "A", Title: "Neutre", Updated: future, Project: defaultProjectSlug})

	res := histSearch("boussole", "", 0, 0)
	if res.Total != 0 || res.Indexed != 0 || res.IndexedTotal != 1 {
		t.Fatalf("fiche périmée : 0 trouvée et couverture 0/1 attendues, obtenu %d trouvée(s), %d/%d", res.Total, res.Indexed, res.IndexedTotal)
	}
	histSearchIndexBackfill()
	// Le rattrapage date la fiche de MAINTENANT : on recale la discussion dessus.
	histSetMeta(t, convMeta{ID: "A", Title: "Neutre", Updated: histReadDocs()["A"].At, Project: defaultProjectSlug})
	res = histSearch("boussole", "", 0, 0)
	if res.Total != 1 || res.Indexed != 1 {
		t.Fatalf("après rattrapage : boussole attendue trouvée et index à jour, obtenu %d trouvée(s), %d/%d", res.Total, res.Indexed, res.IndexedTotal)
	}
}

// --- 12. le classement vient de l'index, pas des corps ------------------------

// Corps disparu, fiche et postings restés : la discussion reste trouvable, et
// l'extrait se rabat sur l'aperçu de la fiche.
func TestHistSearchRanksFromIndexNotBodies(t *testing.T) {
	histSetup(t)
	histConv(t, "A", "", "Titre", 100, []LogEvent{histUser(1, "le phare")})
	if err := putBytes(bkChat, convKey("A"), nil); err != nil {
		t.Fatal(err)
	}
	res := histSearch("phare", "", 0, 0)
	if res.Total != 1 || res.Hits[0].ID != "A" {
		t.Fatalf("le résultat devait venir de l'index, obtenu %+v", res.Hits)
	}
	if res.Hits[0].Snippet != "le phare" {
		t.Fatalf("aperçu de la fiche attendu, obtenu %q", res.Hits[0].Snippet)
	}
}

// --- 13. la discussion ouverte n'apparaît qu'une fois, son score n'est pas SOMMÉ

// Ouverte ET indexée : une seule entrée, et le journal en RAM REMPLACE la copie
// indexée au lieu de s'y ajouter. Requête « alpha beta », A ouverte :
//
//	A : titre « alpha » (1) ; texte « alpha beta » (2) → 2*1+2 = 4 ; sommé → 6.
//	B : titre « alpha beta » (2) ; texte « alpha » (1) → 2*2+1 = 5.
//
// Remplacement : [B A]. Somme : [A B] — l'ordre s'inverse.
func TestHistSearchDeduplicatesActiveAgainstIndex(t *testing.T) {
	histSetup(t)
	histConv(t, "A", "", "alpha", 100, []LogEvent{histUser(1, "alpha beta")})
	histConv(t, "B", "", "alpha beta", 200, []LogEvent{histUser(1, "alpha")})
	histSetLive(t, "A", []LogEvent{histUser(1, "alpha beta")})

	res := histSearch("alpha beta", "", 0, 0)
	if got := histHitIDs(res); got != "B,A" {
		t.Fatalf("une entrée par discussion, score remplacé : B,A attendus, obtenu %s (%+v)", got, res.Hits)
	}
}

// --- 14. pagination : total AVANT découpage -----------------------------------

func TestHistSearchPagination(t *testing.T) {
	histSetup(t)
	histConv(t, "A", "", "Phare A", 100, []LogEvent{histUser(1, "le phare")})
	histConv(t, "B", "", "Phare B", 200, []LogEvent{histUser(1, "le phare")})
	histConv(t, "C", "", "Phare C", 300, []LogEvent{histUser(1, "le phare")})

	page := histSearch("phare", "", 1, 1)
	if len(page.Hits) != 1 || page.Total != 3 || page.Hits[0].ID != "B" {
		t.Fatalf("page 2 de 1 : B et total 3 attendus, obtenu %+v total=%d", page.Hits, page.Total)
	}
	beyond := histSearch("phare", "", 10, 5)
	if len(beyond.Hits) != 0 || beyond.Total != 3 {
		t.Fatalf("au-delà de la fin : 0 résultat et total 3 attendus, obtenu %d / %d", len(beyond.Hits), beyond.Total)
	}
}

// --- 15. repli des accents, dans les DEUX sens --------------------------------

func TestHistSearchAccentFolding(t *testing.T) {
	histSetup(t)
	histConv(t, "A", "", "Notes", 100, []LogEvent{histUser(1, "voici le résumé du rapport")})
	histConv(t, "B", "", "Notes", 200, []LogEvent{histUser(1, "voici le resume du dossier")})

	if n := len(histSearch("resume", "", 0, 0).Hits); n != 2 {
		t.Fatalf("resume devait trouver les 2, obtenu %d", n)
	}
	if n := len(histSearch("RÉSUMÉ", "", 0, 0).Hits); n != 2 {
		t.Fatalf("RÉSUMÉ devait trouver les 2, obtenu %d", n)
	}
}

// --- 16. seul le texte VISIBLE est indexé -------------------------------------

func TestHistSearchIndexesVisibleTextOnly(t *testing.T) {
	histSetup(t)
	histConv(t, "A", "", "Neutre", 100, []LogEvent{histContent(1, "voici un zèbre dans la réponse")})
	histConv(t, "B", "", "Neutre", 200, []LogEvent{{TS: 1, Delta: map[string]any{"tool_used": map[string]any{
		"name": "bash", "label": "rg zèbre", "result": "peu importe", "done": true}}}})
	histConv(t, "C", "", "Neutre", 300, []LogEvent{{TS: 1, Delta: map[string]any{"reasoning_content": "je pense au zèbre"}}})
	histConv(t, "D", "", "Neutre", 400, []LogEvent{{TS: 1, Delta: map[string]any{"tool_used": map[string]any{
		"name": "bash", "label": "ls", "result": "le zèbre est dans la sortie", "done": true}}}})

	res := histSearch("zèbre", "", 0, 0)
	if !histHasHit(res, "A") || !histHasHit(res, "B") {
		t.Fatalf("réponse et libellé d'outil devaient être indexés : %+v", res.Hits)
	}
	if histHasHit(res, "C") || histHasHit(res, "D") {
		t.Fatalf("raisonnement et sortie d'outil ne devaient PAS être indexés : %+v", res.Hits)
	}
}

// --- 17. correspondance par PRÉFIXE -------------------------------------------

func TestHistSearchPrefixMatchesBody(t *testing.T) {
	histSetup(t)
	histConv(t, "A", "", "Sujet neutre", 100, []LogEvent{histUser(1, "on passe du temps à compiling le binaire")})
	histConv(t, "B", "", "Autre", 200, []LogEvent{histUser(1, "rien à voir ici")})

	if got := histHitIDs(histSearch("compil", "", 0, 0)); got != "A" {
		t.Fatalf("compil devait trouver A (« compiling »), obtenu %s", got)
	}
}

// Garde-fous inverses : un préfixe qui ne commence aucun mot ne rend rien, et le
// MILIEU d'un mot ne correspond pas. « deepseek » est dans un TITRE à dessein :
// le scan des postings, borné au préfixe « p|eepseek », ne peut pas atteindre
// « deepseek » ; seul le titre verrait une bascule HasPrefix → Contains.
func TestHistSearchPrefixRejectsNonPrefix(t *testing.T) {
	histSetup(t)
	histConv(t, "A", "", "compiling", 100, []LogEvent{histUser(1, "compiling le binaire")})
	histConv(t, "B", "", "deepseek", 200, []LogEvent{histUser(1, "on parle deepseek ici")})

	for _, q := range []string{"compilo", "eepseek"} {
		if res := histSearch(q, "", 0, 0); res.Total != 0 {
			t.Fatalf("%q ne devait rien trouver, obtenu %s", q, histHitIDs(res))
		}
	}
}

func TestHistSearchPrefixMatchesTitle(t *testing.T) {
	histSetup(t)
	histConv(t, "A", "", "Compilation annuelle", 100, []LogEvent{histUser(1, "texte neutre")})
	histConv(t, "B", "", "Sujet divers", 200, []LogEvent{histUser(1, "on finit le compiling")})

	if got := histHitIDs(histSearch("compil", "", 0, 0)); got != "A,B" {
		t.Fatalf("le titre (+2) doit primer le texte (+1) : A,B attendus, obtenu %s", got)
	}
}

// Un préfixe qui touche plusieurs mots d'une même discussion ne compte qu'une
// fois : à un incrément par mot, A (3 variantes, plus ancienne) passerait devant.
func TestHistSearchPrefixCountsTermOnce(t *testing.T) {
	histSetup(t)
	histConv(t, "A", "", "Neutre", 100, []LogEvent{histUser(1, "compiling compilation compiled")})
	histConv(t, "B", "", "Neutre", 200, []LogEvent{histUser(1, "on a fini de compiling")})

	if got := histHitIDs(histSearch("compil", "", 0, 0)); got != "B,A" {
		t.Fatalf("score par TERME (égalité, B plus récente) : B,A attendus, obtenu %s", got)
	}
}

// La discussion ouverte suit la même règle de préfixe que l'index.
func TestHistSearchPrefixMatchesLiveSession(t *testing.T) {
	histSetup(t)
	histConv(t, "A", "", "Autre", 100, []LogEvent{histUser(1, "rien de spécial")})
	histConv(t, "live1", "", "salut", 200, []LogEvent{histUser(1, "salut")})
	histSetLive(t, "live1", []LogEvent{histUser(1, "salut"), histContent(2, "on parle de compilation ici")})

	if got := histHitIDs(histSearch("compil", "", 0, 0)); got != "live1" {
		t.Fatalf("la discussion ouverte devait ressortir sur « compil », obtenu %s", got)
	}
}

// --- 18. couverture : une entrée d'index SANS corps n'est pas « à indexer » ---

// Une discussion toute neuve est dans l'index des discussions avant d'avoir
// jamais été écrite : elle ne recevra pas de fiche tant qu'elle est vide. La
// compter au dénominateur laissait l'UI annoncer « indexation en cours » pour
// toujours (constat amont, 13/14 stable).
func TestHistSearchCoverageIgnoresMetaWithoutBody(t *testing.T) {
	histSetup(t)
	histConv(t, "normale", "", "Sujet", 100, []LogEvent{histUser(1, "le phare")})
	histSetMeta(t, convMeta{ID: "neuve", Title: "", Updated: 200, Project: defaultProjectSlug})

	res := histSearch("phare", "", 0, 0)
	if res.IndexedTotal != 1 || res.Indexed != 1 {
		t.Fatalf("couverture 1/1 attendue, obtenue %d/%d", res.Indexed, res.IndexedTotal)
	}
	if !histHasHit(res, "normale") || histHasHit(res, "neuve") {
		t.Fatalf("la discussion écrite attendue, la neuve exclue : %+v", res.Hits)
	}
}

// --- 19. l'extrait montre le passage trouvé, pas le début -----------------------

// Le début d'une discussion est son titre (convSummary) : l'aperçu seul
// n'apprendrait rien. L'extrait est pris autour du mot trouvé, loin dans le fil.
func TestHistSearchSnippetInContext(t *testing.T) {
	histSetup(t)
	long := strings.Repeat("du texte sans rapport ", 40)
	histConv(t, "A", "", "Question de départ", 100, []LogEvent{
		histUser(1, "Question de départ"),
		histContent(2, long+"et voici enfin le Phare du Créac'h, au bout de l'île. "+long),
	})
	res := histSearch("phare", "", 0, 0)
	if len(res.Hits) != 1 {
		t.Fatalf("A attendue, obtenu %+v", res.Hits)
	}
	sn := res.Hits[0].Snippet
	if !strings.Contains(sn, "Phare du Créac'h") {
		t.Fatalf("extrait autour du mot attendu, obtenu %q", sn)
	}
	if !strings.HasPrefix(sn, "…") || !strings.HasSuffix(sn, "…") {
		t.Fatalf("extrait coupé des deux côtés attendu (…), obtenu %q", sn)
	}
	if n := len([]rune(sn)); n > 260 {
		t.Fatalf("extrait trop long (%d runes) : %q", n, sn)
	}
}

// --- 20. écritures de la FAÇADE (LOKI_CHAT=rust) ------------------------------

// La façade écrit ses discussions par /internal/v1/store, scellées par ce
// process : l'index doit les suivre — écriture, réécriture, suppression — sans
// que la façade ait rien à en savoir.
func TestHistSearchFacadeStoreWrites(t *testing.T) {
	histSetup(t)
	if _, err := EnableMemEncryption("motdepasse-fort"); err != nil {
		t.Fatal(err)
	}
	histSetMeta(t, convMeta{ID: "r1", Title: "Écrite par la façade", Updated: 100, Project: defaultProjectSlug})
	put := func(log []LogEvent) {
		t.Helper()
		sealed, err := encodeMemContent(histBody(t, log))
		if err != nil {
			t.Fatal(err)
		}
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/internal/v1/store?bucket=chat&key=conv:r1", strings.NewReader(string(sealed)))
		handleInternalStore(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("PUT : %d %s", rr.Code, rr.Body.String())
		}
	}
	put([]LogEvent{histUser(1, "le phare vu par la façade")})
	if got := histHitIDs(histSearch("phare", "", 0, 0)); got != "r1" {
		t.Fatalf("discussion de la façade attendue, obtenu %q", got)
	}
	if !looksEncrypted(getBytes(bkChatSearch, histPostKey("phare"))) {
		t.Fatal("index tenu pour la façade resté en clair")
	}
	put([]LogEvent{histUser(1, "maintenant une boussole")})
	if res := histSearch("phare", "", 0, 0); res.Total != 0 {
		t.Fatalf("réécrite sans « phare » : plus rien attendu, obtenu %s", histHitIDs(res))
	}
	rr := httptest.NewRecorder()
	handleInternalStore(rr, httptest.NewRequest(http.MethodDelete, "/internal/v1/store?bucket=chat&key=conv:r1", nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE : %d", rr.Code)
	}
	if histDocExists(t, "r1") || len(histPosting(t, "boussole")) != 0 {
		t.Fatal("discussion supprimée par la façade restée dans l'index")
	}
	// Une autre clé du même bucket (l'index des discussions) n'est pas une discussion.
	histIndexStoreWrite(bkChat, ckIndex, []byte("[]"), false)
	if histDocExists(t, "index") {
		t.Fatal("l'index des discussions a été pris pour une discussion")
	}
}

// La route reste à ce process : la façade ne la sert pas (elle la relaie), et
// le tunnel du relais ne la lui remet pas.
func TestHistSearchRouteNotDelegated(t *testing.T) {
	if facadeChatRoutes["/api/conversations/search"] {
		t.Fatal("/api/conversations/search ne doit pas être remise à la façade : l'index est tenu ici")
	}
}

// --- 21. la route HTTP --------------------------------------------------------

func TestHandleConvSearch(t *testing.T) {
	histSetup(t)
	histConv(t, "A", "", "Le phare", 100, []LogEvent{histUser(1, "Le phare")})

	rr := httptest.NewRecorder()
	handleConvSearch(rr, httptest.NewRequest(http.MethodGet, "/api/conversations/search?q=phare", nil))
	var got struct {
		OK            bool            `json:"ok"`
		State         string          `json:"state"`
		Conversations []histSearchHit `json:"conversations"`
		Total         int             `json:"total"`
		Indexed       int             `json:"indexed"`
		IndexedTotal  int             `json:"indexed_total"`
		Terms         []string        `json:"terms"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("réponse illisible : %v (%s)", err, rr.Body.String())
	}
	if !got.OK || got.State != "ok" || got.Total != 1 || len(got.Conversations) != 1 || !got.Conversations[0].InTitle {
		t.Fatalf("réponse inattendue : %+v", got)
	}
	if got.Indexed != 1 || got.IndexedTotal != 1 || strings.Join(got.Terms, ",") != "phare" {
		t.Fatalf("couverture ou termes inattendus : %+v", got)
	}

	rr = httptest.NewRecorder()
	handleConvSearch(rr, httptest.NewRequest(http.MethodGet, "/api/conversations/search?q=", nil))
	if !strings.Contains(rr.Body.String(), `"conversations":[]`) || !strings.Contains(rr.Body.String(), `"no_terms"`) {
		t.Fatalf("requête vide : tableau vide et no_terms attendus, obtenu %s", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	handleConvSearch(rr, httptest.NewRequest(http.MethodPost, "/api/conversations/search", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST : 405 attendu, obtenu %d", rr.Code)
	}
}
