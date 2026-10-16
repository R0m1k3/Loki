package loki

// lsp.go — client LSP minimal pour le VÉRIFICATEUR DE CODE : après chaque
// write/edit en mode code, les diagnostics du serveur de langage (erreurs de
// compilation, types, imports) sont ajoutés AU RÉSULTAT DE L'OUTIL — le modèle
// apprend immédiatement que son code ne compile pas, sans lancer de build.
// Repris de l'intégration LSP d'OpenFox (manager + languages.json — voir
// NOTICE.md), réécrit en Go réduit à l'essentiel : initialize, didOpen/
// didChange, publishDiagnostics. Pas de complétion, pas de hover — Loki n'est
// pas un éditeur.
//
// La table langage → serveur vit dans lsp_languages.json (embarquée). Un
// serveur absent du PATH désactive silencieusement son langage : le mode code
// marche sans LSP, il vérifie juste moins tôt.

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed lsp_languages.json
var lspLanguagesJSON []byte

type lspLanguage struct {
	ID         string   `json:"id"`
	Command    []string `json:"command"`
	Extensions []string `json:"extensions"`
}

var (
	lspLangsOnce sync.Once
	lspLangs     []lspLanguage
)

func lspLanguages() []lspLanguage {
	lspLangsOnce.Do(func() {
		_ = json.Unmarshal(lspLanguagesJSON, &lspLangs)
	})
	return lspLangs
}

// lspLangFor renvoie la config du langage d'un fichier, nil si aucun.
func lspLangFor(path string) *lspLanguage {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return nil
	}
	for i := range lspLanguages() {
		for _, e := range lspLangs[i].Extensions {
			if e == ext {
				return &lspLangs[i]
			}
		}
	}
	return nil
}

// --- Serveur -----------------------------------------------------------------

const (
	lspDiagWait    = 2500 * time.Millisecond // attente max des diagnostics
	lspIdleTimeout = 5 * time.Minute         // arrêt d'un serveur inactif
	lspMaxDiags    = 10                      // diagnostics montrés au modèle
)

// lspDiagQuiet : calme attendu après une publication avant de la croire
// définitive. gopls, rust-analyzer et pyright publient souvent en plusieurs
// fois pour une même version (liste vide d'abord, puis leur analyse, puis
// celle d'un outil externe) : rendre la PREMIÈRE donnait « propre » à une
// édition qui cassait la compilation. OpenFox attend 400 ms ; variable pour
// les tests.
var lspDiagQuiet = 350 * time.Millisecond

type lspServer struct {
	lang    string
	root    string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	mu      sync.Mutex // sérialise les requêtes (un didOpen+attente à la fois)
	nextID  int
	version map[string]int // uri → version de document
	// diags : dernier publishDiagnostics reçu par uri, et signal d'arrivée.
	diagMu sync.Mutex
	diags  map[string][]lspDiagnostic
	// diagVer : version de document annoncée par la dernière publication de
	// l'uri, -1 quand le serveur n'en donne pas (champ facultatif du protocole).
	diagVer  map[string]int
	diagCh   chan string // uri fraîchement publié
	lastUsed time.Time
	// dead : atomique, pas sous mu. writeMsg le pose sur une écriture ratée, et
	// writeMsg tourne DANS check, qui tient déjà mu : le prendre là bloquait le
	// serveur (et le tour) à vie. lspGet le lit aussi sans mu.
	dead atomic.Bool
}

type lspDiagnostic struct {
	Range struct {
		Start struct {
			Line      int `json:"line"`
			Character int `json:"character"`
		} `json:"start"`
	} `json:"range"`
	Severity int    `json:"severity"`
	Message  string `json:"message"`
	Source   string `json:"source"`
}

var (
	lspMu      sync.Mutex
	lspServers = map[string]*lspServer{} // clé : lang + racine
)

// lspDiagBlock : le bloc à AJOUTER au résultat d'un write/edit réussi.
// "" si pas de mode code, pas de serveur pour ce langage, ou aucun diagnostic.
func lspDiagBlock(absPath string, caps Caps) string {
	if !caps.Code {
		return ""
	}
	diags := lspDiagnosticsFor(absPath)
	if diags == "" {
		return ""
	}
	return "\n\n[diagnostics LSP]\n" + diags
}

// lspDiagnosticsFor ouvre (ou met à jour) le document auprès du serveur de son
// langage et attend ses diagnostics. Best-effort borné : au pire lspDiagWait.
func lspDiagnosticsFor(absPath string) string {
	lang := lspLangFor(absPath)
	if lang == nil {
		return ""
	}
	if _, err := exec.LookPath(lang.Command[0]); err != nil {
		return "" // serveur non installé : le mode code marche sans
	}
	root := agentCwd()
	// Le dépôt cloné est la vraie racine projet quand le fichier est dedans :
	// gopls a besoin du go.mod, tsserver du tsconfig.
	if r := projectRootFor(absPath, root); r != "" {
		root = r
	}
	srv, err := lspGet(lang, root)
	if err != nil {
		return ""
	}
	diags, ok := srv.check(absPath)
	if !ok {
		return ""
	}
	if len(diags) == 0 {
		return ""
	}
	// Erreurs d'abord, puis avertissements, puis le reste (OpenFox) : au-delà
	// de lspMaxDiags on tronque, et des indices de style passaient devant
	// l'erreur de compilation qui comptait. Le compte annonce le total.
	rank := func(s int) int {
		if s <= 0 {
			return 1 // sévérité absente : traitée comme une erreur
		}
		return s
	}
	sort.SliceStable(diags, func(i, j int) bool { return rank(diags[i].Severity) < rank(diags[j].Severity) })
	nErr, nWarn := 0, 0
	for _, d := range diags {
		switch rank(d.Severity) {
		case 1:
			nErr++
		case 2:
			nWarn++
		}
	}
	total := len(diags)
	if len(diags) > lspMaxDiags {
		diags = diags[:lspMaxDiags]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d erreur(s), %d avertissement(s)", nErr, nWarn)
	if total > len(diags) {
		fmt.Fprintf(&b, " — %d premiers sur %d", len(diags), total)
	}
	b.WriteString("\n")
	for _, d := range diags {
		sev := "info"
		switch d.Severity {
		case 1:
			sev = "error"
		case 2:
			sev = "warning"
		}
		fmt.Fprintf(&b, "%d:%d [%s] %s\n", d.Range.Start.Line+1, d.Range.Start.Character+1, sev, strings.TrimSpace(d.Message))
	}
	return strings.TrimRight(b.String(), "\n")
}

// projectRootFor remonte du fichier vers ws en cherchant un marqueur de racine
// de projet. Renvoie "" si rien trouvé au-dessus du fichier (dans la borne ws).
func projectRootFor(absPath, ws string) string {
	dir := filepath.Dir(absPath)
	for {
		for _, marker := range []string{"go.mod", "package.json", "pyproject.toml", ".git"} {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir
			}
		}
		if rel, ok := relWithin(ws, dir); !ok || rel == "" || rel == "." {
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// lspGet renvoie le serveur (lang, racine), démarré au besoin.
func lspGet(lang *lspLanguage, root string) (*lspServer, error) {
	key := lang.ID + "\x00" + root
	lspMu.Lock()
	defer lspMu.Unlock()
	if s := lspServers[key]; s != nil && !s.dead.Load() {
		return s, nil
	}
	s, err := lspStart(lang, root)
	if err != nil {
		return nil, err
	}
	lspServers[key] = s
	return s, nil
}

func lspStart(lang *lspLanguage, root string) (*lspServer, error) {
	cmd := exec.Command(lang.Command[0], lang.Command[1:]...)
	cmd.Dir = root
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := &lspServer{
		lang: lang.ID, root: root, cmd: cmd, stdin: stdin,
		version: map[string]int{}, diags: map[string][]lspDiagnostic{}, diagVer: map[string]int{},
		diagCh: make(chan string, 16), lastUsed: time.Now(),
	}
	go s.readLoop(stdout)
	go s.idleWatch()
	// initialize / initialized. La réponse est consommée par readLoop ; on
	// n'attend pas les capacités — on n'utilise que les diagnostics.
	s.send("initialize", map[string]any{
		"processId": nil,
		"rootUri":   fileURI(root),
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"publishDiagnostics": map[string]any{},
				"synchronization":    map[string]any{"didSave": false},
			},
		},
		"workspaceFolders": []map[string]any{{"uri": fileURI(root), "name": filepath.Base(root)}},
	}, true)
	s.notify("initialized", map[string]any{})
	return s, nil
}

// check ouvre (ou met à jour) le document et attend ses diagnostics.
func (s *lspServer) check(absPath string) ([]lspDiagnostic, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead.Load() {
		return nil, false
	}
	s.lastUsed = time.Now()
	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil, false
	}
	uri := fileURI(absPath)
	// Signaux en attente : tous PÉRIMÉS. Un serveur qui republie après le
	// retour du check précédent laisse son uri dans le tampon ; lu ici tout de
	// suite après didChange, il faisait rendre « aucun diagnostic » avant même
	// que le serveur ait vu la nouvelle version.
	for drained := false; !drained; {
		select {
		case <-s.diagCh:
		default:
			drained = true
		}
	}
	// Vide les publications en attente pour cet uri : on veut la PROCHAINE.
	s.diagMu.Lock()
	delete(s.diags, uri)
	delete(s.diagVer, uri)
	s.diagMu.Unlock()
	v, open := s.version[uri]
	if !open {
		v = 1
		s.version[uri] = v
		s.notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri": uri, "languageId": s.lang, "version": v, "text": string(content),
			},
		})
	} else {
		v++
		s.version[uri] = v
		s.notify("textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": uri, "version": v},
			"contentChanges": []map[string]any{{"text": string(content)}},
		})
	}
	if s.dead.Load() {
		return nil, false // écriture ratée : le serveur ne verra jamais cette version
	}
	// current : les diagnostics publiés pour CETTE version (ou sans version),
	// pas ceux d'une version antérieure arrivés en retard.
	current := func() ([]lspDiagnostic, bool) {
		s.diagMu.Lock()
		defer s.diagMu.Unlock()
		d, ok := s.diags[uri]
		if !ok {
			return nil, false
		}
		if pv, has := s.diagVer[uri]; has && pv >= 0 && pv < v {
			return nil, false
		}
		return d, true
	}
	// Attend la publication pour CET uri, puis un calme de lspDiagQuiet sans
	// nouvelle publication ; le tout borné par lspDiagWait.
	deadline := time.NewTimer(lspDiagWait)
	defer deadline.Stop()
	var quiet <-chan time.Time
	var quietT *time.Timer
	defer func() {
		if quietT != nil {
			quietT.Stop()
		}
	}()
	for {
		select {
		case got := <-s.diagCh:
			if got != uri {
				continue
			}
			if _, ok := current(); !ok {
				continue // publication d'une version antérieure
			}
			if quietT == nil {
				quietT = time.NewTimer(lspDiagQuiet)
			} else {
				quietT.Reset(lspDiagQuiet)
			}
			quiet = quietT.C
		case <-quiet:
			d, _ := current()
			return d, true
		case <-deadline.C:
			// Pas de publication à temps : certains serveurs ne publient rien
			// quand il n'y a AUCUN diagnostic. On rend « rien », pas un échec.
			d, _ := current()
			return d, true
		}
	}
}

// readLoop parse les messages LSP (en-têtes Content-Length) et garde les
// publishDiagnostics ; tout le reste est ignoré.
func (s *lspServer) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		length := 0
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				s.markDead()
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break
			}
			if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
				fmt.Sscanf(strings.TrimSpace(v), "%d", &length)
			}
		}
		if length <= 0 || length > 32<<20 {
			s.markDead()
			return
		}
		buf := make([]byte, length)
		if _, err := io.ReadFull(br, buf); err != nil {
			s.markDead()
			return
		}
		var msg struct {
			Method string `json:"method"`
			Params struct {
				URI         string          `json:"uri"`
				Version     *int            `json:"version"`
				Diagnostics []lspDiagnostic `json:"diagnostics"`
			} `json:"params"`
		}
		if json.Unmarshal(buf, &msg) != nil {
			continue
		}
		if msg.Method == "textDocument/publishDiagnostics" {
			s.diagMu.Lock()
			s.diags[msg.Params.URI] = msg.Params.Diagnostics
			ver := -1
			if msg.Params.Version != nil {
				ver = *msg.Params.Version
			}
			s.diagVer[msg.Params.URI] = ver
			s.diagMu.Unlock()
			select {
			case s.diagCh <- msg.Params.URI:
			default: // personne n'attend : la publication reste dans s.diags
			}
		}
	}
}

func (s *lspServer) idleWatch() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		s.mu.Lock()
		idle := time.Since(s.lastUsed) > lspIdleTimeout
		dead := s.dead.Load()
		s.mu.Unlock()
		if dead {
			return
		}
		if idle {
			s.shutdown()
			return
		}
	}
}

func (s *lspServer) shutdown() {
	s.markDead()
	if s.cmd.Process != nil {
		// shutdown/exit protocolaires seraient plus polis, mais le process peut
		// être déjà figé — kill borné est fiable et le serveur n'a pas d'état.
		_ = s.cmd.Process.Kill()
	}
	lspMu.Lock()
	delete(lspServers, s.lang+"\x00"+s.root)
	lspMu.Unlock()
}

func (s *lspServer) markDead() { s.dead.Store(true) }

func (s *lspServer) send(method string, params any, isRequest bool) {
	s.nextID++
	msg := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	if isRequest {
		msg["id"] = s.nextID
	}
	s.writeMsg(msg)
}

func (s *lspServer) notify(method string, params any) { s.send(method, params, false) }

func (s *lspServer) writeMsg(msg map[string]any) {
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	_, err = fmt.Fprintf(s.stdin, "Content-Length: %d\r\n\r\n%s", len(b), b)
	if err != nil {
		s.markDead()
	}
}

// fileURI convertit un chemin absolu en URI file://.
func fileURI(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // Windows : C:\x → /C:/x
	}
	return "file://" + p
}

// Pas d'arrêt global : les serveurs LSP sont des enfants du process loki et
// meurent avec lui (et l'inactivité de 5 min couvre le reste — idleWatch).
