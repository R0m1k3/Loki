// web_engine_reco.go — « version recommandée » du panneau Moteur, retour à la
// version précédente, et garde du rendu des gabarits à travers une mise à jour.
//
//	POST /api/engine/plan      → choisit la version à installer (sur clic) et
//	                             dit si le prompt rendu va changer
//	POST /api/engine/rollback  → revient au moteur d'avant la dernière bascule
//
// RIEN D'AUTOMATIQUE. Aucune de ces routes n'est appelée au démarrage ni par
// l'état du panneau : le registre n'est interrogé que sur un clic, et rien ne
// s'installe sans la confirmation qui suit. L'encart « version recommandée »
// lui-même se décide sans réseau, sur le build du moteur courant.
package loki

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// enginePreserveDefaultBuild : premier build où llama-server active
	// preserve_reasoning quand ni --reasoning-preserve ni
	// --no-reasoning-preserve n'est passé (PR #28174).
	enginePreserveDefaultBuild = 10763
	// engineQwen4MTPBuild : premier build qui sait la tête MTP de qwen4exp
	// (PR #29761). Cité dans l'encart, jamais activé par Loki : SPEC reste off
	// tant que l'utilisateur ne le pose pas.
	engineQwen4MTPBuild = 11331
)

// ---------------------------------------------------------------------------
// Choix de la version recommandée
// ---------------------------------------------------------------------------

// engineRecommendedTag : le tag figé le plus récent publié pour la variante,
// à condition qu'il atteigne engineMinRecommended et qu'il existe vraiment.
//
// Le registre n'offre pas de « plus récent au-dessus de N » ; lister ses
// milliers de tags pour le trouver serait lent et fragile. Le plus récent
// au-dessus du minimum, c'est le dernier publié — l'annotation du tag mouvant
// le nomme (engineOCILatest) — et on vérifie que son tag figé répond avant de
// le proposer : c'est lui qu'on installera, pas le tag mouvant, pour que la
// version confirmée soit la version installée.
func engineRecommendedTag(variant string) (int, string, error) {
	build, tag, err := engineOCILatest(variant)
	if err != nil {
		return 0, "", fmt.Errorf("registre injoignable, aucune version choisie : %w", err)
	}
	if build == 0 {
		// Sans numéro, impossible de promettre « au moins b10864 » : on ne
		// propose pas un tag mouvant dont on ne sait pas ce qu'il contient.
		return 0, "", fmt.Errorf("le registre n'annonce pas le build du tag « %s » : impossible de vérifier qu'il atteint b%d", variant, engineMinRecommended)
	}
	if build < engineMinRecommended {
		return 0, "", fmt.Errorf("dernier build publié pour %s : b%d, antérieur à la version recommandée b%d", variant, build, engineMinRecommended)
	}
	if _, _, err := ociResolve(ociEngineRepo, tag); err != nil {
		return 0, "", fmt.Errorf("%s annoncé mais introuvable sur le registre : %w", tag, err)
	}
	return build, tag, nil
}

// engineRecommendation : l'encart du panneau, ou nil. Sans réseau : seulement
// le build de confiance du moteur courant (engineBuildTrust — jamais pour un
// compilé ou un fork), comparé au minimum recommandé.
func engineRecommendation(trusted int, supported bool) map[string]any {
	if trusted <= 0 || trusted >= engineMinRecommended || !supported {
		return nil
	}
	return map[string]any{
		"current": trusted,
		"min":     engineMinRecommended,
		"gains": []string{
			fmt.Sprintf("points de reprise des modèles hybrides (Qwen3.5/3.6, Qwen3-Next…) gardés : plus de ~8 k jetons recalculés à chaque reprise (b%d)", engineMinRecommended),
			fmt.Sprintf("MTP rapide (graphes CUDA) à partir de b%d", specAutoMinBuild),
			fmt.Sprintf("MTP de qwen4exp à partir de b%d", engineQwen4MTPBuild),
		},
	}
}

// ---------------------------------------------------------------------------
// Rendu du raisonnement : le défaut qui change à b10763
// ---------------------------------------------------------------------------

// preserveIn : ce qu'il faut savoir pour dire si la bascule change le prompt.
type preserveIn struct {
	External   bool   // preset externe : le gabarit n'est pas rendu par ce moteur
	Current    int    // build de confiance du moteur courant (0 = inconnu)
	Target     int    // build visé (0 = inconnu)
	Key        string // REASONING_PRESERVE de la configuration active
	Overridden string // EXTRA_ARGS ou LLAMA_ARG_* qui règlent déjà la question
	Template   string // gabarit chargé (/props)
	TplKnown   bool
	Preserves  tplTri // la sonde, sur le moteur courant
}

// preserveRisk : le verdict, pour l'avertissement et la case à cocher.
type preserveRisk struct {
	Risk string `json:"risk"` // "yes", "no" ou "unknown"
	Why  string `json:"why"`
	// Suggest : cocher d'office REASONING_PRESERVE=off. Seulement quand le
	// changement est établi : sur un gabarit qui garde de lui-même la réflexion
	// (Qwen3.8), « off » changerait à son tour le rendu.
	Suggest bool `json:"suggest"`
}

// tplPreserveVars : les variables que llama-server pose dans le contexte du
// gabarit avec preserve_reasoning (caps_apply_preserve_reasoning). Un gabarit
// qui n'en lit aucune rend la même chose sur les deux moteurs.
var tplPreserveVars = []string{"preserve_thinking", "clear_thinking", "truncate_history_thinking", "preserve_reasoning"}

// reasoningPreserveRisk : la bascule vers le build visé change-t-elle le prompt
// rendu ? Fonction pure.
func reasoningPreserveRisk(in preserveIn) preserveRisk {
	no := func(why string) preserveRisk { return preserveRisk{Risk: "no", Why: why} }
	switch {
	case in.External:
		return no("preset externe : son gabarit n'est pas rendu par ce moteur")
	case in.Target > 0 && in.Target < enginePreserveDefaultBuild:
		return no(fmt.Sprintf("b%d n'a pas encore preserve_reasoning par défaut (b%d)", in.Target, enginePreserveDefaultBuild))
	case in.Current >= enginePreserveDefaultBuild:
		return no(fmt.Sprintf("le moteur courant (b%d) a déjà preserve_reasoning par défaut", in.Current))
	case in.Key != "":
		return no("REASONING_PRESERVE=" + in.Key + " est posée : le rendu ne dépend pas du défaut du moteur")
	case in.Overridden != "":
		return no(in.Overridden + " règle déjà preserve_reasoning")
	case !in.TplKnown:
		return preserveRisk{Risk: "unknown", Why: "moteur arrêté ou gabarit illisible : impossible de vérifier. " +
			"À partir de b" + fmt.Sprint(enginePreserveDefaultBuild) + ", un gabarit de type Qwen3.6 rend la réflexion des tours passés, " +
			"vide puisque Loki ne la renvoie pas (sans REASONING_ECHO) ; REASONING_PRESERVE=off garde le rendu actuel. " +
			"À l'inverse, sur un gabarit qui la garde déjà (Qwen3.8), off changerait le rendu."}
	}
	tpl := false
	for _, v := range tplPreserveVars {
		if strings.Contains(in.Template, v) {
			tpl = true
			break
		}
	}
	switch {
	case !tpl:
		return no("le gabarit chargé ne lit pas preserve_thinking / clear_thinking : rendu inchangé")
	case in.Preserves == tplYes:
		return no("le gabarit garde déjà la réflexion des tours passés : rendu inchangé (ne pas poser off)")
	case in.Preserves == tplNo:
		return preserveRisk{Risk: "yes", Suggest: true, Why: fmt.Sprintf("le gabarit chargé retire la réflexion des tours passés ; "+
			"à partir de b%d le moteur la lui fait garder, et sans REASONING_ECHO Loki ne la renvoie pas : chaque tour passé "+
			"serait rendu avec un bloc de réflexion vide. REASONING_PRESERVE=off garde le rendu actuel.", enginePreserveDefaultBuild)}
	}
	return preserveRisk{Risk: "unknown", Why: "le gabarit lit preserve_thinking mais la sonde n'a pas conclu : " +
		"REASONING_PRESERVE=off garde le rendu actuel s'il retire la réflexion des tours passés"}
}

// preserveOverridden : EXTRA_ARGS ou variable d'environnement qui fixent déjà
// preserve_reasoning (mêmes gardes que reasoningPreserveArgs). Vide = aucune.
func preserveOverridden(cfg map[string]string) string {
	extra := splitArgs(cfg["EXTRA_ARGS"])
	switch {
	case hasAnyFlag(extra, "--reasoning-preserve", "--no-reasoning-preserve"):
		return "EXTRA_ARGS"
	case strings.Contains(strings.Join(flagValues(extra, "--chat-template-kwargs"), " "), "preserve_reasoning"):
		return "--chat-template-kwargs"
	case strings.TrimSpace(os.Getenv("LLAMA_ARG_REASONING_PRESERVE")) != "":
		return "LLAMA_ARG_REASONING_PRESERVE"
	case strings.Contains(os.Getenv("LLAMA_ARG_CHAT_TEMPLATE_KWARGS"), "preserve_reasoning"):
		return "LLAMA_ARG_CHAT_TEMPLATE_KWARGS"
	}
	return ""
}

// enginePreserveInputs : les lectures de reasoningPreserveRisk. Le moteur
// courant n'est interrogé qu'en lecture (/health, /props, /apply-template) :
// rien n'y touche au cache de la conversation.
func enginePreserveInputs(ctx context.Context, target int) preserveIn {
	cfg := ReadConfig()
	in := preserveIn{
		External:   isExternalConfig(cfg),
		Current:    engineBuildTrusted(currentEngineBin()),
		Target:     target,
		Key:        strings.TrimSpace(cfg["REASONING_PRESERVE"]),
		Overridden: preserveOverridden(cfg),
		Preserves:  tplUnknown,
	}
	if in.External || in.Key != "" || in.Overridden != "" || (target > 0 && target < enginePreserveDefaultBuild) {
		return in // verdict déjà acquis : pas la peine d'interroger le moteur
	}
	p := tplLocalProber()
	if !p.healthy(ctx) {
		return in
	}
	props, err := p.props(ctx)
	if err != nil {
		return in
	}
	in.Template, in.TplKnown = props.ChatTemplate, props.ChatTemplate != ""
	in.Preserves = tplProbeEnsure(ctx, tplLastShape()).PreservesHistory
	return in
}

// engineHelpHas : l'aide du binaire mentionne-t-elle ce drapeau ? Lancé avec
// ses bibliothèques à côté, comme le test à blanc : un moteur téléchargé n'a
// pas encore servi, binHelp ne le connaît pas.
func engineHelpHas(bin, flag string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := hideCmd(exec.CommandContext(ctx, bin, "--help"))
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = libraryPathEnv(filepath.Dir(bin))
	out, _ := cmd.CombinedOutput()
	return strings.Contains(string(out), flag)
}

// enginePreserveOff pose REASONING_PRESERVE=off, choisi par l'utilisateur
// avant la bascule. Dans le preset actif s'il y en a un — sinon la prochaine
// bascule de preset l'effacerait sans un mot —, dans la configuration sinon.
// Rien si la clé est déjà posée : c'est un choix antérieur, on ne le contredit
// pas.
func enginePreserveOff(newBin string) (string, error) {
	return enginePreserveOffIf(func() bool { return engineHelpHas(newBin, "--no-reasoning-preserve") })
}

// enginePreserveOffIf : le corps d'enginePreserveOff, l'aide du nouveau moteur
// passée en fonction (les tests n'ont pas de llama-server à lancer).
func enginePreserveOffIf(knowsFlag func() bool) (string, error) {
	if v := strings.TrimSpace(ReadConfig()["REASONING_PRESERVE"]); v != "" {
		return "REASONING_PRESERVE=" + v + " déjà posée : inchangée", nil
	}
	if !knowsFlag() {
		return "", fmt.Errorf("le nouveau moteur ne connaît pas --no-reasoning-preserve")
	}
	if err := tuneGuard(); err != nil {
		return "", err
	}
	if p, content, err := tuneActivePreset(); err == nil {
		if _, _, err := SavePresetApplying(p.ID, p.Name, presetSetKey(content, "REASONING_PRESERVE", "off")); err != nil {
			return "", err
		}
		return "REASONING_PRESERVE=off posée dans le preset « " + p.Name + " »", nil
	}
	if err := SetConfigKey("REASONING_PRESERVE", "off"); err != nil {
		return "", err
	}
	return "REASONING_PRESERVE=off posée dans la configuration active (aucun preset ne lui correspond)", nil
}

// ---------------------------------------------------------------------------
// Vérification du rendu après la bascule
// ---------------------------------------------------------------------------

// engineRenderCheck : le dernier relevé avant/après une bascule de moteur.
type engineRenderCheck struct {
	Status string    `json:"status"` // pending, same, changed, unknown
	Tag    string    `json:"tag"`
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
}

var (
	engineRenderMu   sync.Mutex
	engineRenderLast *engineRenderCheck
	// engineRenderWait : délai laissé au nouveau moteur pour charger le modèle
	// (un gros MoE en mmap peut demander plusieurs minutes). Variable pour les
	// tests.
	engineRenderWait = 20 * time.Minute
	engineRenderPoll = 5 * time.Second
)

func engineRenderSet(c engineRenderCheck) {
	c.At = time.Now()
	engineRenderMu.Lock()
	engineRenderLast = &c
	engineRenderMu.Unlock()
	if c.Status != "pending" {
		line := "[moteur] rendu du gabarit après " + c.Tag + " : " + c.Status
		if c.Detail != "" {
			line += " — " + c.Detail
		}
		fmt.Fprintln(os.Stderr, line)
	}
}

func engineRenderSnapshot() *engineRenderCheck {
	engineRenderMu.Lock()
	defer engineRenderMu.Unlock()
	if engineRenderLast == nil {
		return nil
	}
	c := *engineRenderLast
	return &c
}

// engineRenderRef : le rendu relevé sur le moteur qui tourne encore, la forme
// rejouée et le build qu'il annonce (/props).
type engineRenderRef struct {
	text  string
	shape tplShape
	build string
}

// engineRenderBefore relève le rendu sur le moteur qui tourne encore. ok=false
// : moteur arrêté, preset externe ou rendu impossible — pas de comparaison.
func engineRenderBefore(ctx context.Context) (engineRenderRef, bool) {
	ref := engineRenderRef{shape: tplLastShape()}
	if externalActive() {
		return ref, false
	}
	p := tplLocalProber()
	if !p.healthy(ctx) {
		return ref, false
	}
	if props, err := p.props(ctx); err == nil {
		ref.build = props.BuildInfo
	}
	fp, err := tplRenderPrint(ctx, p, ref.shape)
	ref.text = fp
	return ref, err == nil
}

// engineRenderAfter attend le nouveau moteur (modèle chargé) et compare. En
// tâche de fond : le job de mise à jour est fini, la bascule faite ; ceci ne
// fait que signaler.
//
// « Prêt » veut dire /health à 200 ET un build différent de celui relevé
// avant : un ancien moteur qui aurait survécu à l'arrêt répondrait aussi, et
// comparer l'ancien à lui-même conclurait à tort « identique ».
func engineRenderAfter(ref engineRenderRef, tag string) {
	engineRenderSet(engineRenderCheck{Status: "pending", Tag: tag})
	ctx, cancel := context.WithTimeout(context.Background(), engineRenderWait)
	defer cancel()
	p := tplLocalProber()
	ready := func() bool {
		if !p.healthy(ctx) {
			return false
		}
		if ref.build == "" {
			return true
		}
		props, err := p.props(ctx)
		return err == nil && props.BuildInfo != ref.build
	}
	for !ready() {
		select {
		case <-ctx.Done():
			engineRenderSet(engineRenderCheck{Status: "unknown", Tag: tag, Detail: "le nouveau moteur n'a pas répondu à temps : rendu non vérifié"})
			return
		case <-time.After(engineRenderPoll):
		}
	}
	after, err := tplRenderPrint(ctx, p, ref.shape)
	if err != nil {
		engineRenderSet(engineRenderCheck{Status: "unknown", Tag: tag, Detail: "rendu impossible sur le nouveau moteur : " + err.Error()})
		return
	}
	if same, d := tplRenderDiff(ref.text, after); same {
		engineRenderSet(engineRenderCheck{Status: "same", Tag: tag})
	} else {
		engineRenderSet(engineRenderCheck{Status: "changed", Tag: tag, Detail: d})
	}
}

// ---------------------------------------------------------------------------
// Version précédente
// ---------------------------------------------------------------------------

// enginePrevFile : le moteur d'avant la dernière bascule faite par le panneau.
// Un fichier à part, pas une clé de configuration : un preset n'a rien à en
// dire, et il ne doit pas changer l'empreinte de la configuration active.
func enginePrevFile() string { return filepath.Join(engineDir(), "previous-bin") }

// engineRememberPrevious note le moteur qu'on quitte. Best-effort : sans ce
// fichier, le bouton « revenir » n'apparaît simplement pas.
func engineRememberPrevious(left, next string) {
	if left == "" || samePath(left, next) {
		return
	}
	_ = os.MkdirAll(engineDir(), 0o755)
	_ = os.WriteFile(enginePrevFile(), []byte(left+"\n"), 0o644)
}

// enginePrevious : le moteur précédent s'il est encore là et n'est pas celui
// qui tourne. Le ménage d'après mise à jour le garde (engineKeepAfterUpdate) ;
// une suppression à la main le fait disparaître, et le bouton avec.
func enginePrevious(cur string) map[string]any {
	b, err := os.ReadFile(enginePrevFile())
	if err != nil {
		return nil
	}
	bin := strings.TrimSpace(string(b))
	if bin == "" || !isFile(bin) || samePath(bin, cur) {
		return nil
	}
	out := map[string]any{"bin": bin, "source": engineSource(bin)}
	if tag := engineTagOf(bin); tag != "" {
		out["tag"] = tag
		out["build"] = engineTagBuild(tag)
	} else {
		out["build"] = engineBuildCached(bin)
	}
	return out
}

// handleEnginePlan : sur clic, la version qu'on installerait et ce qu'elle
// changerait au prompt. {recommended:true} : la plus récente ≥ b10864
// réellement publiée ; sinon la dernière publiée. Rien n'est téléchargé.
func handleEnginePlan(w http.ResponseWriter, r *http.Request) {
	if !engineOCISupported() {
		sendJSON(w, 200, map[string]any{"ok": false, "error": engineUnsupportedWhy()})
		return
	}
	var req struct {
		Recommended bool `json:"recommended"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	variant := engineVariant()
	var (
		build int
		tag   string
		err   error
	)
	if req.Recommended {
		build, tag, err = engineRecommendedTag(variant)
	} else {
		build, tag, err = engineOCILatest(variant)
	}
	if err != nil {
		sendJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	cur := currentEngineBin()
	curBuild, _ := engineBuildOf(cur)
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	sendJSON(w, 200, map[string]any{
		"ok":       true,
		"tag":      tag,
		"build":    build,
		"current":  curBuild,
		"uptodate": build > 0 && curBuild == build,
		"preserve": reasoningPreserveRisk(enginePreserveInputs(ctx, build)),
		// Ce qui restera pour revenir en arrière : la version téléchargée qui
		// tourne (gardée par le ménage), ou le moteur de l'image.
		"keeps": engineTagOf(cur),
	})
}

// handleEngineRollback revient au moteur d'avant la dernière bascule. Aucun
// réseau. Le moteur quitté devient à son tour « le précédent » : deux clics
// ramènent où l'on était.
func handleEngineRollback(w http.ResponseWriter, r *http.Request) {
	if tuneDenyHTTP(w) {
		return
	}
	cur := currentEngineBin()
	prev := enginePrevious(cur)
	if prev == nil {
		sendJSON(w, 400, map[string]any{"ok": false, "error": "aucune version précédente conservée"})
		return
	}
	bin := prev["bin"].(string)
	if err := SetConfigKey("BIN", bin); err != nil {
		sendJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	engineRememberPrevious(cur, bin)
	if serviceIsActive() {
		_ = serviceAction("restart")
	}
	sendJSON(w, 200, map[string]any{"ok": true, "bin": bin})
}
