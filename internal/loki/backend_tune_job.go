package loki

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// L'optimiseur côté web et en ligne de commande.
//
//	POST /api/tune          lance (202), 409 si le moteur est occupé
//	GET  /api/tune/status   phase, essais, ETA ; puis le résultat
//	POST /api/tune/cancel   annule (l'essai en cours est arrêté, le moteur relancé)
//	POST /api/tune/apply    {target:"copy"|"preset"} — SEULEMENT sur un clic
//	GET  /api/tune/last     dernier résultat d'un preset, et si le moteur a changé depuis
//
// Rien ne tourne de soi-même : ni au démarrage, ni après une mise à jour du
// moteur (l'interface PROPOSE alors de relancer).

// tuneJob : l'optimisation en cours ou la dernière (une seule à la fois).
var tuneJob struct {
	mu       sync.Mutex
	running  bool
	tracker  *tuneTracker
	cancel   context.CancelFunc
	result   *tuneResult
	err      string
	finished time.Time
	// application en cours (preset réécrit, redémarrage, sonde)
	applying bool
	applyLog []string
	applyMsg string
	applyErr string
}

// tuneRunner est runTune, remplaçable dans les tests.
var tuneRunner = runTune

func tuneStart(opts tuneOpts) (int, error) {
	if err := tunePreflight(context.Background(), true); err != nil {
		code := http.StatusConflict
		if externalActive() {
			code = http.StatusBadRequest
		}
		return code, err
	}
	tuneJob.mu.Lock()
	defer tuneJob.mu.Unlock()
	if tuneJob.running || tuneJob.applying {
		return http.StatusConflict, errTuneBusy
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Le verrou de la discussion : un message, une tâche, une compaction
	// reçoivent « optimisation en cours » au lieu d'attendre un moteur absent.
	// Le bouton stop du chat annule l'optimisation, comme un bench.
	release, err := conv.measureLease(cancel, errTuneBusy)
	if err != nil {
		cancel()
		return http.StatusConflict, conv.busyReason()
	}
	tr := &tuneTracker{}
	tuneJob.running, tuneJob.tracker, tuneJob.cancel = true, tr, cancel
	tuneJob.result, tuneJob.err = nil, ""
	tuneJob.applyLog, tuneJob.applyMsg, tuneJob.applyErr = nil, "", ""
	opts.Via = "web"
	go func() {
		defer cancel()
		res, err := tuneRunner(ctx, opts, tr)
		tr.mu.Lock()
		tr.ended = time.Now()
		tr.mu.Unlock()
		release()
		tuneJob.mu.Lock()
		defer tuneJob.mu.Unlock()
		tuneJob.running, tuneJob.cancel, tuneJob.finished = false, nil, time.Now()
		tuneJob.result = res
		if err != nil {
			tuneJob.err = err.Error()
			if ctx.Err() != nil {
				tuneJob.err = "optimisation annulée"
			}
		}
	}()
	return http.StatusAccepted, nil
}

func tuneStatus() map[string]any {
	tuneJob.mu.Lock()
	defer tuneJob.mu.Unlock()
	st := map[string]any{"ok": true, "running": tuneJob.running, "applying": tuneJob.applying}
	if tuneJob.tracker != nil {
		for k, v := range tuneJob.tracker.snapshot() {
			st[k] = v
		}
	}
	if !tuneJob.running {
		if tuneJob.result != nil {
			st["result"] = tuneJob.result
		}
		if tuneJob.err != "" {
			st["error"] = tuneJob.err
		}
	}
	if len(tuneJob.applyLog) > 0 || tuneJob.applyMsg != "" || tuneJob.applyErr != "" {
		st["apply"] = map[string]any{"log": tuneJob.applyLog, "message": tuneJob.applyMsg, "error": tuneJob.applyErr}
	}
	// Une optimisation lancée ailleurs (« loki tune » en ligne de commande).
	if o, busy := tuneActive(); busy && !tuneJob.running && !tuneJob.applying {
		st["elsewhere"] = o.Via
	}
	return st
}

func handleTune(w http.ResponseWriter, r *http.Request) {
	if !postOnly(w, r) {
		return
	}
	var body struct {
		Placement bool     `json:"placement"`
		OptIn     bool     `json:"optin"`
		BudgetMin int      `json:"budget_min"`
		Stages    []string `json:"stages"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body)
	opts := tuneOpts{Placement: body.Placement, OptIn: body.OptIn, Stages: body.Stages}
	if body.BudgetMin > 0 {
		opts.Budget = time.Duration(body.BudgetMin) * time.Minute
	}
	code, err := tuneStart(opts)
	if err != nil {
		sendJSON(w, code, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	sendJSON(w, code, map[string]any{"ok": true})
}

func handleTuneStatus(w http.ResponseWriter, r *http.Request) { sendJSON(w, 200, tuneStatus()) }

func handleTuneCancel(w http.ResponseWriter, r *http.Request) {
	if !postOnly(w, r) {
		return
	}
	tuneJob.mu.Lock()
	ok := tuneJob.running && tuneJob.cancel != nil
	if ok {
		tuneJob.cancel()
	}
	tuneJob.mu.Unlock()
	sendJSON(w, 200, map[string]any{"ok": ok})
}

// handleTuneApply écrit le dernier résultat, sur le clic de l'utilisateur
// (l'interface a montré le diff et demandé confirmation). Copie : immédiat.
// Preset : en arrière-plan (redémarrage, sonde, retour arrière si besoin).
func handleTuneApply(w http.ResponseWriter, r *http.Request) {
	if !postOnly(w, r) {
		return
	}
	var body struct {
		Target string `json:"target"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body)
	tuneJob.mu.Lock()
	res := tuneJob.result
	busy := tuneJob.running || tuneJob.applying
	tuneJob.mu.Unlock()
	if busy {
		sendJSON(w, 409, map[string]any{"ok": false, "error": errTuneBusy.Error()})
		return
	}
	if res == nil {
		sendJSON(w, 400, map[string]any{"ok": false, "error": "aucun résultat à appliquer"})
		return
	}
	if body.Target == tuneApplyCopy {
		msg, err := tuneApply(context.Background(), res, tuneApplyCopy, func(string) {})
		if err != nil {
			sendJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		sendJSON(w, 200, map[string]any{"ok": true, "message": msg})
		return
	}
	if body.Target != tuneApplyPreset {
		sendJSON(w, 400, map[string]any{"ok": false, "error": "cible inconnue"})
		return
	}
	if _, _, err := tunePlanApply(res); err != nil {
		sendJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if conv.isGenerating() {
		sendJSON(w, 409, map[string]any{"ok": false, "error": conv.busyReason().Error()})
		return
	}
	// Vérifié et posé d'un seul tenant : deux clics rapprochés passaient tous
	// deux le test du début, et le perdant remettait applying à faux pendant
	// que le premier écrivait encore.
	tuneJob.mu.Lock()
	if tuneJob.running || tuneJob.applying {
		tuneJob.mu.Unlock()
		sendJSON(w, 409, map[string]any{"ok": false, "error": errTuneBusy.Error()})
		return
	}
	tuneJob.applying, tuneJob.applyLog, tuneJob.applyMsg, tuneJob.applyErr = true, nil, "", ""
	tuneJob.mu.Unlock()
	go func() {
		say := func(s string) {
			tuneJob.mu.Lock()
			tuneJob.applyLog = append(tuneJob.applyLog, s)
			tuneJob.mu.Unlock()
		}
		msg, err := tuneApply(context.Background(), res, tuneApplyPreset, say)
		tuneJob.mu.Lock()
		defer tuneJob.mu.Unlock()
		tuneJob.applying = false
		tuneJob.applyMsg = msg
		if err != nil {
			tuneJob.applyErr = err.Error()
		}
	}()
	sendJSON(w, 202, map[string]any{"ok": true})
}

// handleTuneLast : le dernier résultat enregistré d'un preset, et si le moteur
// a changé depuis (build différent) — l'interface propose alors de relancer.
func handleTuneLast(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	res, ok := loadTuneResult(id)
	if !ok {
		sendJSON(w, 200, map[string]any{"ok": false})
		return
	}
	out := map[string]any{"ok": true, "result": res}
	if content, err := ReadPreset(id); err == nil {
		out["preset_changed"] = presetFingerprint([]byte(content)) != res.PresetFP
	}
	if bin := tuneEngineBin(ReadConfig()); bin != "" && res.EngineBuild > 0 {
		if b := engineBuildCached(bin); b > 0 && b != res.EngineBuild {
			out["engine_changed"] = fmt.Sprintf("b%d → b%d", res.EngineBuild, b)
		}
	}
	sendJSON(w, 200, out)
}

// tuneEngineBin : le binaire du moteur, résolu comme cmdServe le fait.
func tuneEngineBin(cfg map[string]string) string {
	bin := strings.TrimSpace(cfg["BIN"])
	if bin == "" {
		return ""
	}
	if !filepath.IsAbs(bin) {
		bin = filepath.Join(LokiHome(), bin)
	}
	return prebuiltResolveBin(bin)
}

// --- ligne de commande -------------------------------------------------------------

// cmdTune : « loki tune [--placement] [--opt-in] [--budget MIN] [--stages …] ». Mesure, montre,
// puis DEMANDE où écrire le résultat — rien n'est écrit sans réponse. Comme
// « loki bench », ce processus ne voit pas la discussion du processus web :
// seule la vérification /slots (moteur occupé) protège un tour en cours ; une
// fois le verrou pris, le processus web refuse tout nouveau tour. Le bouton de
// l'interface, lui, voit tout.
func cmdTune(args []string) error {
	fs := flag.NewFlagSet("tune", flag.ContinueOnError)
	placement := fs.Bool("placement", false, "essayer --fit à la place du placement manuel (réécrit EXTRA_ARGS)")
	optIn := fs.Bool("opt-in", false, "essayer aussi SPEC, CUDA_GRAPH_OPT et --backend-sampling")
	budget := fs.Int("budget", int(tuneDefaultBudget/time.Minute), "budget en minutes")
	stages := fs.String("stages", "", "étapes de base, séparées par des virgules : marges,lots,delestage,threads,files (vide = toutes)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := tunePreflight(ctx, false); err != nil {
		return err
	}
	fmt.Printf("\n  %s — essais sur un moteur privé (127.0.0.1), config.env jamais modifié.\n", cyan("Optimiseur sans perte"))
	fmt.Println(dim("  Le chat et les tâches attendent pendant la mesure ; Ctrl-C annule (le moteur est relancé)."))
	fmt.Println()
	// Ctrl-C annule proprement : l'essai est arrêté, le vrai moteur relancé.
	// Le terminal fermé (SIGHUP ; CTRL_CLOSE_EVENT, livré en SIGTERM sous
	// Windows) ou un kill aussi : sans ça, le processus mourait sans ses defers,
	// le vrai moteur restait arrêté et l'essai gardait la VRAM.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	go func() {
		if _, ok := <-sig; ok {
			cancel()
		}
	}()
	tr := &tuneTracker{print: true}
	var only []string
	for _, s := range strings.Split(*stages, ",") {
		if s = strings.TrimSpace(s); s != "" {
			only = append(only, s)
		}
	}
	res, err := runTune(ctx, tuneOpts{Placement: *placement, OptIn: *optIn,
		Budget: time.Duration(*budget) * time.Minute, Via: "cli", Stages: only}, tr)
	if err != nil {
		return err
	}
	// Les essais sont finis : Ctrl-C reprend son sens ordinaire aux questions.
	signal.Stop(sig)
	printTuneResult(res)
	if len(res.Set) == 0 {
		return nil
	}
	in := bufio.NewScanner(os.Stdin)
	ask := func(q string) string {
		fmt.Print(q)
		if !in.Scan() {
			return ""
		}
		return strings.ToLower(strings.TrimSpace(in.Text()))
	}
	switch ask("  Enregistrer ? [c] dans une copie du preset · [p] dans « " + res.PresetName + " » (redémarre le moteur) · [N] non : ") {
	case "c":
		msg, err := tuneApply(context.Background(), res, tuneApplyCopy, func(string) {})
		if err != nil {
			return err
		}
		fmt.Println(green("[ok] ") + msg)
	case "p":
		if res.Placement && ask("  Le placement réécrit EXTRA_ARGS (voir ci-dessus). Confirmer ? [o/N] ") != "o" {
			fmt.Println(dim("  rien n'a été écrit"))
			return nil
		}
		msg, err := tuneApply(context.Background(), res, tuneApplyPreset, func(s string) { fmt.Println("  " + s) })
		if err != nil {
			return err
		}
		fmt.Println(green("[ok] ") + msg)
	default:
		fmt.Println(dim("  rien n'a été écrit (le résultat reste visible dans l'éditeur du preset)"))
	}
	return nil
}

func printTuneResult(res *tuneResult) {
	fmt.Println()
	row := func(t tuneTrial) {
		turn := "—"
		if t.TurnSec > 0 {
			turn = fmt.Sprintf("%.1f s", t.TurnSec)
		}
		why := t.Why
		if t.Headroom >= 0 {
			why += fmt.Sprintf(" · %d Mio libres", t.Headroom)
		}
		fmt.Printf("  %-10s %-44s %8s  %-8s %s\n", t.Stage, t.Label, turn, t.Status, strings.TrimPrefix(why, " · "))
	}
	fmt.Printf("  %s (K=%.0f jetons lus, G=%.0f écrits, %.0f %% à froid à %d jetons — %s)\n",
		cyan("Durée d'un tour type"), res.Weights.K, res.Weights.G, res.Weights.FCold*100, res.Depth, res.Weights.From)
	row(res.Baseline)
	for _, t := range res.Trials {
		row(t)
	}
	for _, s := range res.Skipped {
		fmt.Println(dim("  sauté — " + s))
	}
	for _, n := range res.Notes {
		fmt.Println(yellow("  ! ") + n)
	}
	if res.Partial != "" {
		fmt.Println(yellow("  ! résultat partiel : ") + res.Partial)
	}
	fmt.Println()
	if len(res.Set) == 0 {
		fmt.Println("  La configuration actuelle reste la meilleure : rien à changer.")
		return
	}
	fmt.Printf("  %s %s : −%.1f %% par tour\n", green("Meilleur :"), res.Best, res.Gain*100)
	for _, c := range res.Changes {
		fmt.Println("    " + c)
	}
	fmt.Println()
}
