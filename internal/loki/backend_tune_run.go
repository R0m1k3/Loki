package loki

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Optimiseur sans perte — le déroulé (la partie pure est dans backend_tune.go).
//
//  1. Refus d'entrée : preset externe, aucun preset actif, génération, tâche,
//     bench ou job en cours, moteur occupé par un client.
//  2. Verrou exclusif (backend_tune_lock.go), vrai moteur arrêté comme le fait
//     « décharger la VRAM ».
//  3. Chaque essai : une COPIE de la configuration avec ses surcharges, posée
//     dans LOKI_HOME/tune/run/<essai>/config.env — jamais config.env — puis
//     « loki serve » à blanc (la ligne de commande, comparée à la référence
//     avant tout chargement), puis pour de vrai sur 127.0.0.1 et un port libre.
//     Mesure : le bench complet du lot 1 (llm_bench.go) à une profondeur fixe.
//     Le groupe de processus de l'essai est tué en sortie, quoi qu'il arrive.
//  4. Fin : le vrai moteur est relancé (comme handleVramReload), le verrou
//     rendu. Le résultat est montré ; RIEN n'est écrit dans un preset sans un
//     geste de l'utilisateur (tuneApply).

// tuneOpts : ce que l'utilisateur a demandé.
type tuneOpts struct {
	Placement bool          // « inclure le placement » (réécrit EXTRA_ARGS pour --fit)
	OptIn     bool          // « inclure les options opt-in » (SPEC, CUDA_GRAPH_OPT, --backend-sampling)
	Budget    time.Duration // 0 = tuneDefaultBudget
	Via       string        // « web » ou « cli »
	// Stages : étapes de base à faire (marges, lots, délestage, threads, files) ;
	// vide = toutes. Placement et opt-in ont leur propre case.
	Stages []string
}

const (
	tuneDefaultBudget = 30 * time.Minute
	tuneMaxBudget     = 6 * time.Hour
)

// tuneLoadTimeout : chargement d'un essai. Un MoE de 80 Go relu depuis un
// disque lent prend plusieurs minutes ; au-delà, l'essai est déclaré en échec.
var tuneLoadTimeout = 20 * time.Minute

// tuneTrial : un essai, tel que montré à l'utilisateur.
type tuneTrial struct {
	Stage     string    `json:"stage"`
	Label     string    `json:"label"`
	Status    string    `json:"status"` // en cours, mesuré, retenu, écarté, échec, doublon
	Why       string    `json:"why,omitempty"`
	Runs      []tuneRun `json:"runs,omitempty"`
	TurnSec   float64   `json:"turn_sec,omitempty"` // moyenne des passages
	Gain      float64   `json:"gain"`               // contre la meilleure configuration du moment
	Headroom  int       `json:"headroom_mib"`       // VRAM libre minimale après le bench ; -1 = non mesurée
	Offloaded string    `json:"offloaded,omitempty"`
	Changes   []string  `json:"changes,omitempty"` // contre la référence
	Sec       float64   `json:"sec"`
}

// tuneResult : l'issue d'une optimisation.
type tuneResult struct {
	PresetID    string            `json:"preset_id"`
	PresetName  string            `json:"preset_name"`
	BaseFP      string            `json:"base_fp"`   // configFingerprint de la configuration mesurée
	PresetFP    string            `json:"preset_fp"` // presetFingerprint du preset au départ
	Engine      string            `json:"engine"`    // build_info du moteur
	EngineBuild int               `json:"engine_build"`
	Weights     tuneWeights       `json:"weights"`
	Depth       int               `json:"depth"`
	NCtx        int               `json:"n_ctx"`
	Slots       int               `json:"slots"`
	Baseline    tuneTrial         `json:"baseline"`
	Trials      []tuneTrial       `json:"trials"`
	Best        string            `json:"best,omitempty"` // libellé de l'essai retenu ; vide = la référence reste la meilleure
	Gain        float64           `json:"gain"`           // contre la référence
	Set         map[string]string `json:"set,omitempty"`  // clés changées et leur nouvelle valeur ("" = retirée)
	Old         map[string]string `json:"old,omitempty"`  // leur valeur mesurée en référence
	Changes     []string          `json:"changes,omitempty"`
	Placement   bool              `json:"placement"` // EXTRA_ARGS réécrit : confirmation à part
	Partial     string            `json:"partial,omitempty"`
	Skipped     []string          `json:"skipped,omitempty"`
	Notes       []string          `json:"notes,omitempty"`
	OptIn       bool              `json:"opt_in"`
	At          int64             `json:"at"`
}

// tuneTracker : la progression, lue par l'interface ou affichée par la CLI.
type tuneTracker struct {
	mu      sync.Mutex
	phase   string
	started time.Time
	ended   time.Time // fin de la mesure ; zéro = en cours
	trials  []tuneTrial
	avgSec  float64 // durée moyenne d'un essai mesuré
	left    int     // essais encore prévus (estimation)
	budget  time.Duration
	print   bool
}

func (t *tuneTracker) setPhase(format string, a ...any) {
	t.mu.Lock()
	t.phase = fmt.Sprintf(format, a...)
	p := t.print
	t.mu.Unlock()
	if p {
		fmt.Printf("  %s %s\n", dim(time.Now().Format("15:04:05")), fmt.Sprintf(format, a...))
	}
}

func (t *tuneTracker) put(i int, tr tuneTrial) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if i < 0 || i >= len(t.trials) {
		t.trials = append(t.trials, tr)
		return len(t.trials) - 1
	}
	t.trials[i] = tr
	return i
}

func (t *tuneTracker) snapshot() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	end := t.ended
	if end.IsZero() {
		end = time.Now()
	}
	el := end.Sub(t.started)
	eta := -1.0
	if t.avgSec > 0 {
		eta = min(t.avgSec*float64(t.left), (t.budget - el).Seconds())
		eta = max(eta, 0)
	}
	return map[string]any{"phase": t.phase, "elapsed_sec": el.Seconds(), "eta_sec": eta,
		"trials": append([]tuneTrial(nil), t.trials...)}
}

// tuneActivePreset : le preset dont l'empreinte est celle de la configuration
// active — c'est lui que l'optimisation mesure, et lui seul qu'elle peut
// réécrire. Aucun : la mesure n'aurait nulle part où aller.
func tuneActivePreset() (Preset, string, error) {
	list, err := ListPresets()
	if err != nil {
		return Preset{}, "", err
	}
	for _, p := range list {
		if p.Active {
			content, err := ReadPreset(p.ID)
			return p, content, err
		}
	}
	return Preset{}, "", errors.New("aucun preset ne correspond à la configuration active : enregistre-la comme preset " +
		"(ou bascule sur l'un d'eux) avant d'optimiser")
}

// tuneAuth : l'en-tête du moteur protégé par clé (essai comme vrai moteur).
func tuneAuth() func(set func(k, v string)) {
	key, _ := effectiveAPIKeyErr()
	return func(set func(k, v string)) {
		if key != "" {
			set("Authorization", "Bearer "+key)
		}
	}
}

// tunePreflight : tout ce qui interdit de commencer, dans CE processus. Le
// processus web voit la discussion, les tâches et les jobs ; la CLI ne voit que
// le moteur (/slots).
func tunePreflight(ctx context.Context, inWeb bool) error {
	if externalActive() {
		return errors.New("optimisation indisponible : le preset actif est une API externe")
	}
	if err := tuneGuard(); err != nil {
		return err
	}
	if _, _, err := tuneActivePreset(); err != nil {
		return err
	}
	if inWeb {
		switch {
		case benchRunning():
			return errBenchBusy
		case conv.isGenerating():
			return conv.busyReason()
		case bgJobsRunning() > 0:
			return fmt.Errorf("%d job(s) d'arrière-plan du mode code tournent encore (bash_bg) : ils fausseraient les mesures — arrête-les d'abord",
				bgJobsRunning())
		}
	}
	// En ligne de commande : le moteur qui répond sur le port de CE LOKI_HOME
	// est-il bien le sien ? (Le processus web, lui, fait foi pour son moteur.)
	if !inWeb {
		cfg := ReadConfig()
		ours, _ := resolveServeModelPath(strings.TrimSpace(cfg["MODEL"]))
		auth := tuneAuth()
		if err := tuneForeignEngine(ctx, fmt.Sprintf("http://localhost:%d", LLMPort()), serviceIsActive(),
			func(r *http.Request) { auth(r.Header.Set) }, ours); err != nil {
			return err
		}
	}
	if serviceIsActive() {
		e := benchEngine{base: fmt.Sprintf("http://localhost:%d", LLMPort()), auth: tuneAuth(), client: http.DefaultClient}
		if err := e.idle(ctx); err != nil {
			return errors.New("le moteur traite une requête (discussion, tâche ou client /v1) : réessaie une fois libre")
		}
	}
	return nil
}

// tuneForeignEngine : « loki tune » et le processus web avec des LOKI_HOME
// différents (sudo qui retire la variable, LOKI_HOME exporté dans un seul
// shell) ne se voient pas : le verrou de l'optimisation tombe dans un dossier
// que l'interface ne lit pas, et l'arrêt du « vrai » moteur vise celui d'une
// autre configuration. L'essai chargeait alors à côté d'un moteur bien vivant
// — VRAM saturée, mesures fausses — et l'interface pouvait relancer le sien
// en pleine mesure. On regarde donc ce qui répond sur le port de CE
// LOKI_HOME :
//
//   - un serveur répond alors que ce LOKI_HOME dit son moteur arrêté : un autre
//     Loki, ou un llama-server orphelin ;
//   - le moteur refuse la clé d'API de ce LOKI_HOME (401 sur /props), ou sert
//     un autre fichier que son MODEL : le moteur d'une autre configuration.
//
// Tout ce qui ne conclut pas (pas de réponse, /props absent d'un moteur
// ancien, chemin illisible d'ici) laisse passer : seul un désaccord constaté
// refuse.
func tuneForeignEngine(ctx context.Context, base string, active bool, auth func(*http.Request), ourModel string) error {
	p := tplProber{base: base, auth: auth, client: &http.Client{Timeout: 2 * time.Second}}
	// /health d'un llama-server : 200, 503 en chargement, 401 derrière une clé
	// sur certaines versions. Rien n'écoute, ou un autre service (404…) : aucun
	// moteur à craindre.
	var he *tplHTTPError
	if _, err := p.do(ctx, http.MethodGet, "/health", nil); err != nil &&
		(!errors.As(err, &he) || (he.status != http.StatusServiceUnavailable && he.status != http.StatusUnauthorized)) {
		return nil
	}
	home := LokiHome()
	foreign := func(why string) error {
		return fmt.Errorf("%s, alors que LOKI_HOME=%s — un autre Loki tourne sans doute avec un autre LOKI_HOME "+
			"(sudo retire la variable) : relance avec le sien (LOKI_HOME=… loki tune) ou utilise le bouton "+
			"« Optimiser… » de l'interface ; un llama-server orphelin, lui, s'arrête à la main", why, home)
	}
	if !active {
		return foreign("un moteur répond sur " + base + " sans que ce dossier le suive")
	}
	props, err := p.props(ctx)
	switch {
	case errors.As(err, &he) && he.status == http.StatusUnauthorized:
		return foreign("le moteur de " + base + " refuse la clé d'API de ce dossier")
	case err != nil || props.ModelPath == "" || ourModel == "":
		return nil
	}
	a, errA := os.Stat(props.ModelPath)
	b, errB := os.Stat(ourModel)
	if errA == nil && errB == nil && !os.SameFile(a, b) {
		return foreign(fmt.Sprintf("le moteur de %s sert %s, pas le MODEL de ce dossier (%s ; config.env modifiée "+
			"sans redémarrage ? « loki restart »)", base, filepath.Base(props.ModelPath), filepath.Base(ourModel)))
	}
	return nil
}

// tunePhysCores : cœurs physiques (Linux ; ailleurs inconnu, l'axe threads est
// sauté). Un conteneur à l'étroit (cpuset, quota) donne sa propre limite.
func tunePhysCores(probe *tuneProbe) int {
	if probe != nil && probe.CPUThreads > 0 {
		return probe.CPUThreads
	}
	fsys := os.DirFS("/")
	online, err := readCPUList(fsys, "sys/devices/system/cpu/online")
	if err != nil || len(online) == 0 {
		return 0
	}
	atom, _ := readCPUList(fsys, "sys/devices/cpu_atom/cpus")
	n, err := physicalCores(fsys, online, atom)
	if err != nil {
		return 0
	}
	return n
}

func tuneFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// tuneTrialer lance les essais d'une optimisation.
type tuneTrialer struct {
	dir   string
	self  string
	lock  *tuneLock
	seq   int
	optIn bool
}

// prepare pose la configuration d'un essai (copie + adresse privée) dans un
// dossier neuf.
func (t *tuneTrialer) prepare(cfg map[string]string) (dir string, port int, err error) {
	t.seq++
	dir = filepath.Join(t.dir, fmt.Sprintf("e%02d", t.seq))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, err
	}
	if port, err = tuneFreePort(); err != nil {
		return "", 0, err
	}
	c := copyCfg(cfg)
	c["HOST"], c["PORT"] = "127.0.0.1", strconv.Itoa(port)
	return dir, port, writeTuneTrialConfig(dir, c)
}

// dry : « loki serve » à blanc — la ligne de commande, rien de chargé.
func (t *tuneTrialer) dry(ctx context.Context, dir string) (tuneLaunch, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := hideCmd(exec.CommandContext(ctx, t.self, "serve"))
	cmd.Dir = LokiHome()
	cmd.Env = append(os.Environ(), tuneTrialEnv+"="+dir, tuneDryRunEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(ansiRe.ReplaceAllString(string(out), ""))
		if i := strings.LastIndex(msg, "\n"); i >= 0 {
			msg = msg[i+1:]
		}
		if msg == "" {
			msg = err.Error()
		}
		return tuneLaunch{}, errors.New(msg)
	}
	return readTuneLaunch(dir)
}

// tuneLoaded : un essai chargé et prêt.
type tuneLoaded struct {
	eng       benchEngine
	nCtx      int
	slots     int
	build     string
	offloaded int
	layers    int
	kv        string
	log       string
}

// tuneProps lit n_ctx, total_slots et build_info du moteur.
func tuneProps(ctx context.Context, e benchEngine) (nCtx, slots int, build string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	data, err := e.do(ctx, http.MethodGet, "/props", nil)
	if err != nil {
		return 0, 0, "", err
	}
	var p struct {
		BuildInfo  string `json:"build_info"`
		TotalSlots int    `json:"total_slots"`
		NCtx       int    `json:"n_ctx"`
		Default    struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return 0, 0, "", err
	}
	nCtx = p.Default.NCtx
	if nCtx <= 0 {
		nCtx = p.NCtx
	}
	return nCtx, p.TotalSlots, p.BuildInfo, nil
}

// load lance l'essai pour de vrai et attend qu'il réponde. La fonction rendue
// l'arrête (groupe entier), efface sa trace du verrou et attend que la VRAM
// soit rendue : à appeler en defer, quoi qu'il arrive.
func (t *tuneTrialer) load(ctx context.Context, dir string, port int, launch tuneLaunch) (*tuneLoaded, func(), error) {
	logPath := filepath.Join(dir, "engine.log")
	logf, err := os.Create(logPath)
	if err != nil {
		return nil, func() {}, err
	}
	cmd := exec.Command(t.self, "serve")
	cmd.Dir = LokiHome()
	cmd.Env = append(os.Environ(), tuneTrialEnv+"="+dir)
	cmd.Stdout, cmd.Stderr = logf, logf
	tuneTrialAttr(cmd)
	if err := cmd.Start(); err != nil {
		logf.Close()
		return nil, func() {}, err
	}
	pid := cmd.Process.Pid
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		logf.Close()
		close(done)
	}()
	proc := tuneProc{PID: pid, Exes: []string{t.self, launch.Bin}}
	if start, _, ok := procIdentity(pid); ok {
		proc.Start = start
	}
	t.lock.setTrial(&proc)
	stop := func() {
		before := gpuUsedMB(gpuStats())
		tuneKillTree(pid, done)
		// Noté jusqu'à sa fin constatée : un essai qui survit à l'arrêt reste
		// désigné dans le verrou, qu'un Loki redémarré pourra encore arrêter.
		select {
		case <-done:
			t.lock.setTrial(nil)
		default:
			fmt.Fprintf(os.Stderr, "[loki tune] l'essai (PID %d) ne s'est pas arrêté dans les temps\n", pid)
		}
		if before > 0 {
			gpuUsedSettled(before)
		} else {
			time.Sleep(2 * time.Second)
		}
	}
	readLog := func() string {
		b, _ := os.ReadFile(logPath)
		return string(b)
	}
	e := benchEngine{base: fmt.Sprintf("http://127.0.0.1:%d", port), auth: tuneAuth(), client: http.DefaultClient}
	deadline := time.Now().Add(tuneLoadTimeout)
	hc := &http.Client{Timeout: 3 * time.Second}
	for {
		select {
		case <-ctx.Done():
			return nil, stop, ctx.Err()
		case <-done:
			return nil, stop, errors.New(tuneLoadFailure(readLog()))
		case <-time.After(time.Second):
		}
		if resp, err := hc.Get(e.base + "/health"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			return nil, stop, fmt.Errorf("pas prêt après %s", tuneLoadTimeout)
		}
	}
	nCtx, slots, build, err := tuneProps(ctx, e)
	if err != nil {
		return nil, stop, fmt.Errorf("/props illisible : %w", err)
	}
	log := readLog()
	off, layers, kv := tuneLogFacts(log)
	return &tuneLoaded{eng: e, nCtx: nCtx, slots: slots, build: build, offloaded: off, layers: layers, kv: kv, log: log}, stop, nil
}

// tuneBenchSetup : le bench du preset de l'essai — son échantillonnage et son
// raisonnement réels (une spéculation se juge sur le tirage du preset, pas en
// glouton), profondeur plafonnée comme celle de la référence.
func tuneBenchSetup(cfg map[string]string, argEnv map[string]string, cpuPlaced bool) benchSetup {
	s := benchSetupFrom(cfg, argEnv)
	s.learn = func(want, got string) { effortRemember(want, got) }
	s.cpuPlaced = cpuPlaced
	return s
}

// runTune déroule une optimisation. L'appelant fournit le suivi ; le verrou,
// l'arrêt et la relance du vrai moteur sont faits ici.
func runTune(ctx context.Context, opts tuneOpts, tr *tuneTracker) (out *tuneResult, retErr error) {
	if opts.Budget <= 0 {
		opts.Budget = tuneDefaultBudget
	}
	opts.Budget = min(opts.Budget, tuneMaxBudget)
	tr.mu.Lock()
	tr.started, tr.budget = time.Now(), opts.Budget
	tr.mu.Unlock()
	cfg := ReadConfig()
	preset, content, err := tuneActivePreset()
	if err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	mainActive := engineNeedsStop()
	lock, err := tuneLockAcquire(opts.Via, mainActive)
	if err != nil {
		return nil, err
	}
	defer lock.release()
	// Le vrai moteur revient AVANT que le verrou ne soit rendu (les defer
	// passent dans l'ordre inverse) : personne ne le relance par-dessus un essai.
	defer func() {
		if !mainActive {
			return
		}
		lock.setPhase("relance")
		tr.setPhase("relance du moteur principal")
		// Un échec de relance doit se voir là où l'on regarde (résultat de
		// l'interface, sortie de « loki tune »), pas seulement au journal.
		failed := func(msg string) {
			fmt.Fprintf(os.Stderr, "[loki tune] %s\n", msg)
			switch {
			case out != nil:
				out.Notes = append(out.Notes, "⚠️ "+msg)
			case retErr != nil:
				retErr = fmt.Errorf("%w — %s", retErr, msg)
			}
		}
		if err := preflightEngine(); err != nil {
			failed("relance du moteur principal impossible : " + plainErr(err))
			return
		}
		if err := serviceActionOS("start"); err != nil {
			failed("relance du moteur principal : " + plainErr(err))
		}
		gpuMonitor.invalidate()
	}()

	runDir := filepath.Join(LokiHome(), "tune", "run")
	_ = os.RemoveAll(runDir)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return nil, err
	}
	t := &tuneTrialer{dir: runDir, self: self, lock: lock, optIn: opts.OptIn}

	if mainActive {
		tr.setPhase("arrêt du moteur principal")
		gpus := gpuStats()
		before := gpuUsedMB(gpus)
		if err := serviceActionOS("stop"); err != nil {
			return nil, fmt.Errorf("arrêt du moteur : %s", plainErr(err))
		}
		asrShutdownVite()
		if gpus != nil {
			gpuUsedSettled(before)
		}
		gpuMonitor.invalidate()
	}

	res := &tuneResult{PresetID: preset.ID, PresetName: preset.Name, BaseFP: configFingerprint(cfg),
		PresetFP: presetFingerprint([]byte(content)), OptIn: opts.OptIn, At: time.Now().Unix(),
		Weights: tuneWeightsFrom(perfLog.snapshot())}

	// Référence : la ligne composée telle quelle, la sonde de la machine.
	tr.setPhase("référence : composition de la ligne de commande")
	baseDir, basePort, err := t.prepare(cfg)
	if err != nil {
		return nil, err
	}
	baseLaunch, err := t.dry(ctx, baseDir)
	if err != nil {
		return nil, fmt.Errorf("la configuration active ne se lance pas : %s", err)
	}
	probe := baseLaunch.Probe
	if probe == nil {
		probe = &tuneProbe{}
	}
	smi := gpuStats()
	env := tuneEnv{Help: probe.Help, GPUs: probe.GPUs, VRAMMiB: probe.VRAMMiB, ModelBytes: probe.ModelBytes,
		MoE: probe.MoE, PhysCores: tunePhysCores(probe), SMI: smi != nil, ArgEnv: probe.ArgEnv,
		UserEnv: probe.UserEnv, QueuesEnv: probe.QueuesEnv, Placement: opts.Placement, OptIn: opts.OptIn}
	visible := tuneVisibleGPUs(cfg)
	if env.VRAMMiB == 0 && smi != nil {
		for i, g := range smi {
			if len(visible) == 0 || containsInt(visible, i) {
				env.VRAMMiB += int64(g.Total)
			}
		}
	}
	if env.GPUs == 0 && smi != nil {
		env.GPUs = len(smi)
		if len(visible) > 0 {
			env.GPUs = len(visible)
		}
	}
	extra := splitArgs(cfg["EXTRA_ARGS"])
	kt, vt, _ := effectiveKVTypes(cfg, extra, env.ArgEnv)
	if label, quant := benchKVLabel(kt, vt); quant {
		res.Notes = append(res.Notes, "cache KV "+label+" quantifié : zone grise, conservé tel quel (jamais modifié par l'optimiseur)")
	}
	if !env.SMI {
		res.Notes = append(res.Notes, "VRAM libre non mesurable (nvidia-smi absent) : ni micro-lot plus grand ni marges --fit essayés")
	}
	if _, total := ramUsageMB(); total > 0 && env.ModelBytes > int64(total)<<20 {
		res.Notes = append(res.Notes, "modèle plus gros que la RAM : chaque essai le relit depuis le disque — prévoir un budget "+
			"plus long, ou ne garder que quelques étapes")
	}
	reserve := tuneTightMiB
	if strings.TrimSpace(cfg["MMPROJ"]) != "" {
		reserve += tuneVisionReserveMiB
	}
	cpuPlaced := tensorOverride(extra, env.ArgEnv) != ""

	// La référence se mesure deux fois : son écart entre passages est le bruit
	// que tout gain doit dépasser.
	tr.setPhase("référence : chargement")
	bi := tr.put(-1, tuneTrial{Stage: "référence", Label: "configuration actuelle", Status: "en cours", Headroom: -1})
	t0 := time.Now()
	base, stop, err := t.load(ctx, baseDir, basePort, baseLaunch)
	if err != nil {
		stop()
		return nil, fmt.Errorf("la référence ne charge pas : %s", err)
	}
	res.Engine, res.NCtx, res.Slots = base.build, base.nCtx, base.slots
	if m := reBuildInfo.FindStringSubmatch(base.build); m != nil {
		res.EngineBuild, _ = strconv.Atoi(m[1])
	}
	// Une profondeur pour tous : celle que la référence peut tenir avec le plus
	// gros préchauffage essayé (2 × 4096 jetons, au plus un quart du contexte).
	warmMax := max(min(2*4096, base.nCtx/4), 64) + benchWarmGen
	res.Depth, _ = benchDepthFor(base.nCtx, cpuPlaced, warmMax)
	bopts := benchOpts{Mode: benchModeFull, Prompt: 2000, Predict: 300, Depth: res.Depth}
	corp := defaultBenchCorpora()
	measure := func(cfg map[string]string, l *tuneLoaded, n int, phase string) ([]tuneRun, error) {
		var runs []tuneRun
		for i := 0; i < n; i++ {
			tr.setPhase("%s : mesure %d/%d", phase, i+1, n)
			// SIDE_SLOT : deux slots ; les tours en profondeur restent sur le même
			// (le second, comme le bench ordinaire), sans quoi la reprise du cache
			// dépendrait du slot choisi par le moteur.
			s := tuneBenchSetup(cfg, env.ArgEnv, cpuPlaced)
			s.sideSlot = sideSlotOn(cfg) && l.slots >= 2
			r, err := benchRun(ctx, l.eng, bopts, s, corp, nil)
			if err != nil {
				return runs, err
			}
			run, err := tuneRunFrom(r, res.Weights)
			if err != nil {
				return runs, err
			}
			runs = append(runs, run)
		}
		return runs, nil
	}
	headroom := func() (int, bool) {
		if !env.SMI {
			return -1, false
		}
		free, ok := tuneHeadroom(gpuStats(), visible)
		if !ok {
			return -1, false
		}
		return free, free < reserve
	}
	baseRuns, err := measure(cfg, base, 2, "référence")
	hr, _ := headroom()
	stop()
	if err != nil {
		return nil, fmt.Errorf("mesure de la référence : %s", err)
	}
	bt := tuneTrial{Stage: "référence", Label: "configuration actuelle", Status: "mesuré", Runs: baseRuns,
		TurnSec: turnMean(baseRuns), Headroom: hr, Sec: time.Since(t0).Seconds()}
	if base.offloaded >= 0 {
		bt.Offloaded = fmt.Sprintf("%d/%d", base.offloaded, base.layers)
	}
	res.Baseline = bt
	tr.put(bi, bt)
	if hr >= 0 && hr < reserve {
		res.Notes = append(res.Notes, fmt.Sprintf("la configuration actuelle laisse déjà moins de %d Mio libres sur une carte", reserve))
	}

	best, bestRuns, bestLabel := cfg, baseRuns, ""
	durations := []float64{bt.Sec}
	measured := map[string]string{tuneLaunchKey(baseLaunch): bt.Label}
	deadline := tr.started.Add(opts.Budget)

stages:
	for si, st := range tuneStages {
		if !tuneStageWanted(st.Name, opts.Stages) {
			res.Skipped = append(res.Skipped, st.Name+" : non demandé")
			continue
		}
		cands, why := st.Gen(best, env)
		if why != "" {
			res.Skipped = append(res.Skipped, st.Name+" : "+why)
			continue
		}
		for ci, c := range cands {
			// Ce qui reste à faire, pour l'estimation : les essais restants de
			// l'étape, plus ceux des étapes suivantes vus depuis la meilleure
			// configuration du moment.
			left := len(cands) - ci
			for _, nx := range tuneStages[si+1:] {
				if !tuneStageWanted(nx.Name, opts.Stages) {
					continue
				}
				more, _ := nx.Gen(best, env)
				left += len(more)
			}
			avg := tuneMean(durations)
			tr.mu.Lock()
			tr.avgSec, tr.left = avg, left
			tr.mu.Unlock()
			if ctx.Err() != nil {
				res.Partial = "annulé"
				break stages
			}
			if time.Until(deadline).Seconds() < avg {
				res.Partial = fmt.Sprintf("budget de %s atteint : %d essai(s) non faits", opts.Budget.Round(time.Minute), left)
				break stages
			}
			trial := tuneTrial{Stage: c.Stage, Label: c.Label, Status: "en cours", Headroom: -1,
				Changes: tuneDiff(cfg, c.Cfg)}
			idx := tr.put(-1, trial)
			finish := func(status, why string) {
				trial.Status, trial.Why = status, why
				tr.put(idx, trial)
				res.Trials = append(res.Trials, trial)
			}
			if err := tuneCfgCheck(cfg, c.Cfg, opts.OptIn); err != nil {
				finish("écarté", "dénature : "+err.Error())
				continue
			}
			tr.setPhase("%s — %s : composition", st.Name, c.Label)
			dir, port, err := t.prepare(c.Cfg)
			if err != nil {
				finish("échec", err.Error())
				continue
			}
			launch, err := t.dry(ctx, dir)
			if err != nil {
				if ctx.Err() != nil {
					finish("échec", "annulé")
					res.Partial = "annulé"
					break stages
				}
				finish("écarté", "refusé au lancement : "+err.Error())
				continue
			}
			if why := tuneDenature(baseLaunch, launch, opts.OptIn); why != "" {
				finish("écarté", "dénature : "+why)
				continue
			}
			key := tuneLaunchKey(launch)
			if prev, ok := measured[key]; ok {
				finish("doublon", "même ligne de commande que « "+prev+" »")
				continue
			}
			measured[key] = c.Label
			t1 := time.Now()
			tr.setPhase("%s — %s : chargement", st.Name, c.Label)
			l, stop, err := t.load(ctx, dir, port, launch)
			if err != nil {
				stop()
				if ctx.Err() != nil {
					finish("échec", "annulé")
					res.Partial = "annulé"
					break stages
				}
				finish("échec", err.Error())
				durations = append(durations, time.Since(t1).Seconds())
				continue
			}
			trial.Sec = time.Since(t1).Seconds()
			if l.offloaded >= 0 {
				trial.Offloaded = fmt.Sprintf("%d/%d", l.offloaded, l.layers)
			}
			// Contre-vérification à l'exécution : même contexte, mêmes slots,
			// mêmes types de cache que la référence.
			switch {
			case l.nCtx != base.nCtx || l.slots != base.slots:
				stop()
				finish("écarté", fmt.Sprintf("dénature : contexte %d × %d slot(s) au lieu de %d × %d", l.nCtx, l.slots, base.nCtx, base.slots))
				continue
			case base.kv != "" && l.kv != "" && l.kv != base.kv:
				stop()
				finish("écarté", "dénature : cache KV "+l.kv+" au lieu de "+base.kv)
				continue
			}
			fitRegressed := base.offloaded >= 0 && l.offloaded >= 0 && l.offloaded < base.offloaded
			runs, err := measure(c.Cfg, l, 1, st.Name+" — "+c.Label)
			if err == nil && len(runs) == 1 && tuneWorthRepeat(turnSecs(bestRuns), runs[0].TurnSec) && !fitRegressed {
				var more []tuneRun
				more, err = measure(c.Cfg, l, 1, st.Name+" — "+c.Label+" (confirmation)")
				runs = append(runs, more...)
			}
			hr, tight := headroom()
			stop()
			trial.Sec = time.Since(t1).Seconds()
			durations = append(durations, trial.Sec)
			trial.Runs, trial.Headroom = runs, hr
			if err != nil {
				if ctx.Err() != nil {
					finish("échec", "annulé")
					res.Partial = "annulé"
					break stages
				}
				finish("échec", "mesure : "+err.Error())
				continue
			}
			trial.TurnSec = turnMean(runs)
			gain, need, wins := tuneWins(turnSecs(bestRuns), turnSecs(runs))
			trial.Gain = gain
			proseBest, proseCand := proseMean(bestRuns), proseMean(runs)
			switch {
			case fitRegressed:
				finish("écarté", fmt.Sprintf("fit en recul : %d couches déportées au lieu de %d (performance)", l.offloaded, base.offloaded))
			case tight:
				finish("écarté", fmt.Sprintf("trop juste : %d Mio libres sur une carte (< %d)", hr, reserve))
			case proseBest > 0 && proseCand < 0.95*proseBest:
				finish("mesuré", fmt.Sprintf("decode en prose plus lent de %.0f %%", (1-proseCand/proseBest)*100))
			case wins:
				best, bestRuns, bestLabel = c.Cfg, runs, c.Label
				if c.Stage == "placement" {
					res.Placement = true
				}
				finish("retenu", fmt.Sprintf("−%.1f %% par tour (seuil %.1f %%)", gain*100, need*100))
			case len(runs) < 2 && gain > 0:
				finish("mesuré", fmt.Sprintf("−%.1f %%, sous le seuil de %.0f %%", gain*100, tuneMinGain*100))
			default:
				delta := fmt.Sprintf("%+.1f %%", -gain*100)
				if gain*1000 > -0.5 && gain*1000 < 0.5 {
					delta = "≈ 0 %"
				}
				finish("mesuré", delta+fmt.Sprintf(" (seuil %.1f %%)", need*100))
			}
		}
	}
	tr.mu.Lock()
	tr.left = 0
	tr.mu.Unlock()

	res.Best = bestLabel
	if bestLabel != "" {
		res.Gain = (turnMean(baseRuns) - turnMean(bestRuns)) / turnMean(baseRuns)
		res.Changes = tuneDiff(cfg, best)
		res.Set, res.Old = map[string]string{}, map[string]string{}
		for k := range keysOf(cfg, best) {
			if cfg[k] != best[k] {
				res.Set[k], res.Old[k] = best[k], cfg[k]
			}
		}
	}
	if ctx.Err() != nil && res.Partial == "" {
		res.Partial = "annulé"
	}
	saveTuneResult(res)
	return res, nil
}

func turnSecs(r []tuneRun) []float64 {
	out := make([]float64, len(r))
	for i, x := range r {
		out[i] = x.TurnSec
	}
	return out
}

func turnMean(r []tuneRun) float64 { return tuneMean(turnSecs(r)) }

func proseMean(r []tuneRun) float64 {
	v := make([]float64, len(r))
	for i, x := range r {
		v[i] = x.ProseTG
	}
	return tuneMean(v)
}

func keysOf(ms ...map[string]string) map[string]bool {
	out := map[string]bool{}
	for _, m := range ms {
		for k := range m {
			out[k] = true
		}
	}
	return out
}

// --- résultats enregistrés -------------------------------------------------------

// saveTuneResult garde le dernier résultat par preset, avec l'empreinte et le
// build du moteur : après une mise à jour du moteur, l'interface propose de
// relancer l'optimisation.
func saveTuneResult(res *tuneResult) {
	m := map[string]tuneResult{}
	getJSON(bkState, "tune_results", &m)
	if m == nil {
		m = map[string]tuneResult{}
	}
	m[res.PresetID] = *res
	_ = putJSON(bkState, "tune_results", m)
}

func loadTuneResult(id string) (*tuneResult, bool) {
	m := map[string]tuneResult{}
	if !getJSON(bkState, "tune_results", &m) {
		return nil, false
	}
	r, ok := m[id]
	if !ok {
		return nil, false
	}
	return &r, true
}

// --- application ------------------------------------------------------------------

// tuneApplyTarget : où écrire le résultat.
const (
	tuneApplyCopy   = "copy"   // une copie du preset, non activée
	tuneApplyPreset = "preset" // le preset mesuré lui-même, réappliqué et vérifié
)

// tunePlanApply prépare la réécriture sans rien écrire : le contenu du preset
// réglé et le diff des clés. Refusé si le preset a changé depuis la mesure.
func tunePlanApply(res *tuneResult) (content, patched string, err error) {
	if res == nil || len(res.Set) == 0 {
		return "", "", errors.New("rien à appliquer : la configuration actuelle reste la meilleure")
	}
	content, err = ReadPreset(res.PresetID)
	if err != nil {
		return "", "", fmt.Errorf("preset « %s » introuvable : %w", res.PresetName, err)
	}
	if presetFingerprint([]byte(content)) != res.PresetFP {
		return "", "", errors.New("le preset a changé depuis la mesure : relance l'optimisation")
	}
	base := parseEnv(content)
	win := copyCfg(base)
	for k, v := range res.Set {
		if v == "" {
			delete(win, k)
		} else {
			win[k] = v
		}
	}
	if err := tuneCfgCheck(base, win, res.OptIn); err != nil {
		return "", "", err
	}
	patched, err = tunePatchPreset(content, base, win)
	return content, patched, err
}

// tuneApply écrit le résultat : dans une COPIE du preset (rien d'autre ne
// bouge), ou dans le preset lui-même — il doit être encore actif. Dans ce cas
// le moteur redémarre sur la nouvelle version, qui doit répondre, garder son
// contexte et ses slots, et passer une sonde (un prompt à la profondeur mesurée
// puis un decode) ; sinon l'ancienne version est rétablie. L'appelant a la
// confirmation de l'utilisateur.
func tuneApply(ctx context.Context, res *tuneResult, target string, say func(string)) (string, error) {
	content, patched, err := tunePlanApply(res)
	if err != nil {
		return "", err
	}
	switch target {
	case tuneApplyCopy:
		name := res.PresetName + " (optimisé)"
		id, err := SavePreset("", name, patched)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("copie « %s » créée (%s) — bascule dessus pour l'utiliser", name, id), nil
	case tuneApplyPreset:
	default:
		return "", fmt.Errorf("cible inconnue : %s", target)
	}
	if externalActive() {
		return "", errors.New("le preset actif est une API externe")
	}
	cur := ReadConfig()
	if presetFingerprint([]byte(content)) != configFingerprint(cur) {
		return "", errors.New("ce preset n'est plus celui en service : bascule dessus, ou enregistre le résultat dans une copie")
	}
	if configFingerprint(cur) != res.BaseFP {
		return "", errors.New("la configuration active a changé depuis la mesure : relance l'optimisation")
	}
	lock, err := tuneLockAcquire("apply", serviceIsActive())
	if err != nil {
		return "", err
	}
	defer lock.release()
	lock.setPhase("application")
	backup := filepath.Join(LokiHome(), "tune", "backup", fmt.Sprintf("%s-%d.env", res.PresetID, time.Now().Unix()))
	_ = os.MkdirAll(filepath.Dir(backup), 0o700)
	if err := os.WriteFile(backup, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("sauvegarde du preset : %w", err)
	}
	// Noté dans le verrou AVANT d'écrire : un Loki tué pendant l'application
	// la défait au redémarrage (tuneUndoApply).
	lock.setBackup(backup, res.PresetID, res.PresetName)
	revert := func(why string) (string, error) {
		say("échec (" + why + ") : retour à l'ancienne version")
		if _, _, err := SavePresetApplying(res.PresetID, res.PresetName, content); err != nil {
			return "", fmt.Errorf("%s ; RETOUR IMPOSSIBLE (%v) — l'ancienne version est dans %s", why, err, backup)
		}
		if err := tuneSvc("restart"); err != nil {
			return "", fmt.Errorf("%s ; ancienne version remise, redémarrage : %s", why, plainErr(err))
		}
		return "", fmt.Errorf("%s : ancienne version rétablie", why)
	}
	say("écriture du preset (sauvegarde : " + backup + ")")
	_, applied, err := SavePresetApplying(res.PresetID, res.PresetName, patched)
	if err != nil {
		return revert("écriture : " + err.Error())
	}
	if !applied {
		return revert("le preset n'a pas été appliqué à la configuration active")
	}
	// Même configuration que l'essai retenu, à l'empreinte près : même
	// composition de la ligne de commande (buildServeArgs) que mesurée.
	if why := tuneAppliedMismatch(ReadConfig(), res); why != "" {
		return revert(why)
	}
	say("redémarrage du moteur")
	if err := tuneSvc("restart"); err != nil {
		return revert("redémarrage : " + plainErr(err))
	}
	if err := tuneProbeFn(ctx, res, say); err != nil {
		return revert(err.Error())
	}
	return "preset « " + res.PresetName + " » optimisé et vérifié", nil
}

// tuneSvc et tuneProbeFn : serviceActionOS et tunePostApplyProbe, remplaçables
// dans les tests (application puis retour arrière sans vrai moteur).
var (
	tuneSvc      = serviceActionOS
	tuneProbeFn  = tunePostApplyProbe
	tuneHealthFn = healthCheck
)

// tuneAppliedMismatch : la configuration active après application doit être
// EXACTEMENT celle de l'essai retenu — la référence mesurée plus les clés
// réglées. Remises à leur valeur de référence, on doit retrouver l'empreinte
// mesurée. Même configuration, même composition (buildServeArgs) : le moteur
// relancé est bien celui qui a gagné. "" = conforme.
func tuneAppliedMismatch(now map[string]string, res *tuneResult) string {
	back := copyCfg(now)
	for k, v := range res.Set {
		if now[k] != v {
			return "la configuration appliquée diffère de l'essai retenu sur " + k
		}
		if old := res.Old[k]; old == "" {
			delete(back, k)
		} else {
			back[k] = old
		}
	}
	if configFingerprint(back) != res.BaseFP {
		return "la configuration a changé ailleurs que sur les clés réglées depuis la mesure"
	}
	return ""
}

// tunePostApplyProbe : le moteur redémarré répond, garde le contexte et les
// slots mesurés, et tient un prompt à la profondeur de la mesure suivi d'un
// decode — ce qu'un /health seul ne montre pas (un OOM au premier long prompt).
func tunePostApplyProbe(ctx context.Context, res *tuneResult, say func(string)) error {
	say("attente du moteur")
	e := benchEngine{base: fmt.Sprintf("http://localhost:%d", LLMPort()), auth: tuneAuth(), client: http.DefaultClient}
	deadline := time.Now().Add(tuneLoadTimeout)
	// Un moteur mort au chargement (OOM) se voit vite : plus de service du tout,
	// trois fois de suite après les premières secondes. Inutile d'attendre le
	// délai entier pour revenir à l'ancienne version.
	gone, t0 := 0, time.Now()
	for !tuneHealthFn() {
		if time.Now().After(deadline) {
			return fmt.Errorf("le moteur ne répond pas après %s", tuneLoadTimeout)
		}
		if time.Since(t0) > 10*time.Second && !engineNeedsStop() {
			if gone++; gone >= 3 {
				return errors.New("le moteur s'est arrêté au chargement")
			}
		} else {
			gone = 0
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	nCtx, slots, _, err := tuneProps(ctx, e)
	if err != nil {
		return fmt.Errorf("/props illisible : %w", err)
	}
	if nCtx != res.NCtx || slots != res.Slots {
		return fmt.Errorf("contexte %d × %d slot(s) au lieu de %d × %d", nCtx, slots, res.NCtx, res.Slots)
	}
	if res.Depth <= 0 {
		return nil
	}
	say(fmt.Sprintf("sonde : prompt de %d jetons puis decode", res.Depth))
	cfg := ReadConfig()
	s := tuneBenchSetup(cfg, nil, false)
	code := defaultBenchCorpora().code
	sample, _ := benchSlice(code, 0, benchSampleBytes)
	text, _ := benchSlice(code, 0, int(float64(res.Depth)*e.bytesPerToken(ctx, sample)))
	defer engineSideJob()()
	if _, err := s.ask(ctx, e, []Message{{Role: "user", Content: text + "\n\n" + benchAskFirst}}, 64, false); err != nil {
		return fmt.Errorf("sonde : %w", err)
	}
	return nil
}
