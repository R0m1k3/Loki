package loki

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// backend_gpu.go — sélection du/des GPU utilisés par llama-server.
//
//	loki gpu            liste les GPU et montre la sélection courante
//	loki gpu 1          n'utilise que le GPU d'index 1
//	loki gpu 0 1        utilise les GPU 0 et 1
//	loki gpu all        réinitialise (tous les GPU visibles)
//
// La sélection est stockée dans config.env sous CUDA_VISIBLE_DEVICES ; backend_serve.go
// l'exporte (avec CUDA_DEVICE_ORDER=PCI_BUS_ID pour que les index correspondent
// à ceux affichés par nvidia-smi).

type gpuInfo struct {
	Index    int
	Name     string
	MemTotal string // en MiB
	MemUsed  string
	Cap      string // compute capability

	// Liaison PCIe et horloge mémoire, pour guider le placement entre cartes
	// inégales (voir annotateDevices). Zéro = inconnu : pilote trop ancien,
	// « [N/A] », ou requête retombée sur les seuls champs historiques.
	BusID        string
	LinkGen      int // génération PCIe ACTUELLE — elle descend au repos (économie d'énergie)
	LinkGenMax   int // génération maximale négociable carte + carte mère
	LinkWidth    int // largeur actuelle (×N), elle aussi réduite au repos sur certaines cartes
	LinkWidthMax int
	MemClockMax  int // horloge mémoire maximale, MHz
}

// gpuBaseFields : la requête historique de detectGPUs, que tout pilote connaît.
// gpuLinkFields : les champs de liaison, ajoutés en fin de ligne. nvidia-smi
// refuse TOUTE la requête au moindre champ inconnu (« is not a valid field to
// query ») : la largeur du bus mémoire, qui n'existe pas, n'y figure donc pas,
// et un refus fait retomber sur la requête historique — jamais de GPU perdu
// pour une information de confort.
const (
	gpuBaseFields = "index,name,memory.total,memory.used,compute_cap"
	gpuLinkFields = "pci.bus_id,pcie.link.gen.current,pcie.link.gen.max,pcie.link.width.current,pcie.link.width.max,clocks.max.memory"
)

// nvidiaSmiGPUQuery lance « nvidia-smi --query-gpu=<fields> ». Variable pour
// que les tests substituent un faux nvidia-smi. Borné dans le temps comme la
// lecture des jauges (nvidiaSmiTimeout) : un pilote coincé ne doit pas figer
// « loki gpu » ni l'éditeur de preset.
var nvidiaSmiGPUQuery = func(fields string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), nvidiaSmiTimeout)
	defer cancel()
	cmd := hideCmd(exec.CommandContext(ctx, "nvidia-smi", "--query-gpu="+fields, "--format=csv,noheader,nounits"))
	cmd.WaitDelay = time.Second
	return cmd.Output()
}

func cmdGPU(args []string) error {
	if len(args) == 0 || args[0] == "list" || args[0] == "ls" {
		return gpuList()
	}
	switch args[0] {
	case "all", "reset", "none", "auto":
		return gpuSet("")
	}
	// Sinon : une liste d'index (« 1 », « 0 1 », « 0,1 »).
	gpus, err := detectGPUs()
	if err != nil {
		return err
	}
	raw := strings.Join(args, ",")
	var idx []string
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		n, err := strconv.Atoi(tok)
		if err != nil {
			return fmt.Errorf("index GPU invalide: %q (attendu un nombre)", tok)
		}
		if n < 0 || n >= len(gpus) {
			return fmt.Errorf("index GPU %d hors limites (0..%d) — voir « loki gpu »", n, len(gpus)-1)
		}
		idx = append(idx, strconv.Itoa(n))
	}
	if len(idx) == 0 {
		return fmt.Errorf("aucun index fourni")
	}
	return gpuSet(strings.Join(idx, ","))
}

// gpuList affiche les GPU détectés et marque ceux actuellement sélectionnés.
func gpuList() error {
	gpus, err := detectGPUs()
	if err != nil {
		return err
	}
	sel := ReadConfig()["CUDA_VISIBLE_DEVICES"]
	selected := map[int]bool{}
	if sel != "" {
		for _, t := range strings.Split(sel, ",") {
			if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
				selected[n] = true
			}
		}
	}

	fmt.Println()
	for _, g := range gpus {
		mark := "  "
		line := fmt.Sprintf("[%d] %s  —  %s/%s MiB  (cc %s)", g.Index, g.Name, g.MemUsed, g.MemTotal, g.Cap)
		if l := g.linkSummary(); l != "" {
			line += "  " + l
		}
		active := sel == "" || selected[g.Index]
		if sel != "" && selected[g.Index] {
			mark = green("● ")
			line = green(line)
		} else if sel == "" {
			mark = dim("○ ")
		} else {
			mark = dim("○ ")
			line = dim(line)
		}
		_ = active
		fmt.Printf("  %s%s\n", mark, line)
	}
	fmt.Println()
	if sel == "" {
		fmt.Printf("  Sélection : %s (tous les GPU)\n", bold("auto"))
	} else {
		fmt.Printf("  Sélection : %s (CUDA_VISIBLE_DEVICES=%s)\n", bold(sel), sel)
	}
	fmt.Printf("  %s loki gpu <index…>  pour choisir,  loki gpu all  pour réinitialiser\n", dim("→"))
	return nil
}

// gpuSet writes (or clears) CUDA_VISIBLE_DEVICES in config.env then offers a
// restart so the change takes effect.
func gpuSet(value string) error {
	if err := SetConfigKey("CUDA_VISIBLE_DEVICES", value); err != nil {
		return err
	}
	if value == "" {
		fmt.Printf("%s sélection GPU réinitialisée — tous les GPU seront visibles\n", green("[ok]"))
	} else {
		fmt.Printf("%s GPU sélectionné(s) : %s\n", green("[ok]"), bold(value))
	}
	fmt.Print(dim("[info] redémarrer le service pour appliquer ? [Y/n] "))
	sc := bufio.NewScanner(os.Stdin)
	if sc.Scan() && strings.HasPrefix(strings.ToLower(strings.TrimSpace(sc.Text())), "n") {
		fmt.Println(dim("[info] pense à lancer 'loki restart'"))
		return nil
	}
	return serviceAction("restart")
}

// detectGPUs queries nvidia-smi for the list of NVIDIA GPUs.
func detectGPUs() ([]gpuInfo, error) {
	if !hasTool("nvidia-smi") {
		return nil, fmt.Errorf("nvidia-smi introuvable — sélection GPU disponible uniquement sur NVIDIA")
	}
	return queryGPUs(nvidiaSmiGPUQuery)
}

// queryGPUs : le cœur de detectGPUs, la requête passée en paramètre pour être
// testée sans nvidia-smi. UNE invocation d'ordinaire : champs historiques et
// liaison ensemble. La seconde n'a lieu que si nvidia-smi refuse un champ de
// liaison (pilote ancien) — pas sur un délai dépassé, qu'une relance ne ferait
// que doubler.
func queryGPUs(query func(fields string) ([]byte, error)) ([]gpuInfo, error) {
	out, err := query(gpuBaseFields + "," + gpuLinkFields)
	if err != nil && smiFieldRefused(out, err) {
		out, err = query(gpuBaseFields)
	}
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi a échoué: %w", err)
	}
	gpus := parseGPUQuery(string(out))
	if len(gpus) == 0 {
		return nil, fmt.Errorf("aucun GPU NVIDIA détecté")
	}
	return gpus, nil
}

// smiFieldRefused : nvidia-smi a-t-il rejeté un champ de la requête ? Il
// l'écrit sur sa sortie standard et sort en code 2 (argument invalide).
func smiFieldRefused(out []byte, err error) bool {
	if strings.Contains(string(out), "valid field") {
		return true
	}
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == 2
}

// parseGPUQuery lit la sortie CSV de detectGPUs, avec ou sans les champs de
// liaison. Fonction pure. Une ligne de moins de cinq colonnes est ignorée,
// comme avant ; une valeur de liaison illisible (« [N/A] », « [Not
// Supported] ») vaut zéro, sans faire tomber la carte.
func parseGPUQuery(out string) []gpuInfo {
	var gpus []gpuInfo
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.Split(line, ",")
		if len(parts) < 5 {
			continue
		}
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		idx, _ := strconv.Atoi(parts[0])
		g := gpuInfo{Index: idx, Name: parts[1], MemTotal: parts[2], MemUsed: parts[3], Cap: parts[4]}
		if len(parts) >= 11 {
			if b := parts[5]; b != "" && b != "N/A" && !strings.HasPrefix(b, "[") {
				g.BusID = b
			}
			g.LinkGen, g.LinkGenMax = smiNum(parts[6]), smiNum(parts[7])
			g.LinkWidth, g.LinkWidthMax = smiNum(parts[8]), smiNum(parts[9])
			g.MemClockMax = smiNum(parts[10])
		}
		gpus = append(gpus, g)
	}
	return gpus
}

// smiNum lit un entier de nvidia-smi ; « [N/A] », vide ou autre chose = 0.
func smiNum(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// linkSummary : la liaison PCIe en clair, « PCIe 4 ×16 », maximum d'abord —
// l'actuel descend au repos et ne dit rien de la carte sous charge. Vide =
// inconnue.
func (g gpuInfo) linkSummary() string {
	if g.LinkGenMax == 0 && g.LinkWidthMax == 0 {
		return ""
	}
	s := "PCIe"
	if g.LinkGenMax > 0 {
		s += " " + strconv.Itoa(g.LinkGenMax)
	}
	if g.LinkWidthMax > 0 {
		s += " ×" + strconv.Itoa(g.LinkWidthMax)
	}
	if (g.LinkGen > 0 && g.LinkGen < g.LinkGenMax) || (g.LinkWidth > 0 && g.LinkWidth < g.LinkWidthMax) {
		known := func(n int) string {
			if n <= 0 {
				return "?"
			}
			return strconv.Itoa(n)
		}
		s += " (actuellement " + known(g.LinkGen) + " ×" + known(g.LinkWidth) + ", réduit au repos)"
	}
	return s
}
