package loki

import (
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

// Threads CPU de llama-server.
//
// Loki passait toujours « -t <THREADS|0> -tb <THREADS_BATCH|0> ». Or pour
// llama.cpp, 0 (ou moins) veut dire std::thread::hardware_concurrency() : TOUS
// les threads logiques, frères SMT compris. Avec l'attente active par défaut
// (--poll 50), deux threads qui se disputent le même cœur physique se gênent
// plus qu'ils ne s'aident — c'est le décodage des experts MoE sur CPU
// (--n-cpu-moe) qui paie. Le « 0 = auto » de l'interface mentait donc : ce
// n'était pas l'auto de llama.cpp.
//
// Le vrai auto, c'est l'ABSENCE du drapeau : n_threads reste à -1 et le moteur
// prend common_cpu_get_num_math() — les cœurs physiques (sans les cœurs E sur
// Intel hybride, sous Linux x86). Un -tb absent recopie -t, et le brouillon
// spéculatif hérite des deux. Le nombre de threads ne change que la répartition
// du travail, pas le calcul du modèle.
//
// Effet de bord assumé : sans -t de Loki, une variable LLAMA_ARG_THREADS posée
// dans l'environnement du moteur s'applique enfin (le -t 0 l'écrasait).
//
// Gain non mesuré sur la machine de référence : avant d'en revendiquer un,
// comparer tg du preset MoE à physiques, physiques-1 et logiques (MTP actif et
// coupé, la vérification spéculative passant par n_threads_batch). Si les
// logiques gagnent, poser THREADS dans CE preset plutôt que changer le défaut.
// Pour voir le nombre retenu : -lv 4 dans EXTRA_ARGS (system_info est en TRACE).

// cpuBudget est ce que la sonde de conteneur (cpusetThreads) a conclu. N = 0 :
// rien à dire, llama.cpp choisit seul.
type cpuBudget struct {
	N      int    // threads à imposer via -t
	Engine int    // ce que llama.cpp aurait pris seul, pour la note
	Why    string // raison, en clair, pour stderr
}

// threadCount lit THREADS / THREADS_BATCH. Vide ou 0 : auto, sans un mot. Une
// valeur illisible (« auto ») ou négative faisait jusqu'ici tomber le moteur en
// boucle sur son analyse d'arguments ; elle est désormais ignorée — mais on le
// DIT, plutôt que de la jeter en douce.
func threadCount(key, v string) (int, string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, ""
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Sprintf("%s=%s ignoré (pas un nombre de threads ≥ 0) → automatique", key, v)
	}
	return n, ""
}

// threadArgs compose -t / -tb. Fonction pure, comme nglArgs : la sonde du
// conteneur arrive déjà tranchée dans auto.
//
//	THREADS=N>0                → -t N
//	THREADS vide/0, sonde N>0  → -t N  (conteneur à l'étroit, voir cpusetThreads)
//	THREADS vide/0             → rien  (cœurs physiques, choisis par llama.cpp)
//	THREADS_BATCH=N>0          → -tb N
//	THREADS_BATCH vide/0       → rien  (le moteur recopie -t)
//
// Un -t / -tb déjà écrit dans EXTRA_ARGS gagne : on ne double pas le drapeau.
// Un masque d'affinité posé à la main (-C, -Cr, --cpu-strict) désarme la sonde :
// l'utilisateur a pris la main sur le placement, on ne devine rien par-dessus.
func threadArgs(threads, threadsBatch string, extra []string, auto cpuBudget) (args, notes []string) {
	t, note := threadCount("THREADS", threads)
	if note != "" {
		notes = append(notes, note)
	}
	switch {
	case hasAnyFlag(extra, "-t", "--threads"):
	case t > 0:
		args = append(args, "-t", strconv.Itoa(t))
	case auto.N > 0 && !hasAnyFlag(extra, "-C", "--cpu-mask", "-Cr", "--cpu-range", "--cpu-strict"):
		args = append(args, "-t", strconv.Itoa(auto.N))
		notes = append(notes, fmt.Sprintf("threads auto → -t %d : %s (llama.cpp seul en lancerait %d)",
			auto.N, auto.Why, auto.Engine))
	}
	tb, note := threadCount("THREADS_BATCH", threadsBatch)
	if note != "" {
		notes = append(notes, note)
	}
	if tb > 0 && !hasAnyFlag(extra, "-tb", "--threads-batch") {
		args = append(args, "-tb", strconv.Itoa(tb))
	}
	return args, notes
}

// cpusetThreads : sonde de conteneur, Linux seulement (fsys = la racine « / »).
//
// L'auto de llama.cpp se trompe quand le conteneur n'a droit qu'à une partie
// de la machine :
//   - cpuset restreint (docker --cpuset-cpus) : common_cpu_get_num_math essaie
//     d'épingler un thread sur chaque CPU de l'hôte, échoue hors du cpuset et
//     se rabat sur TOUS les cœurs physiques de l'hôte (sysfs n'est pas cloisonné),
//     cœurs E compris ;
//   - quota CFS (docker --cpus) : invisible dans l'affinité, il étrangle des
//     threads qui attendent en boucle active — le pire cas.
//
// On compte alors comme llama.cpp (chaînes thread_siblings distinctes, cœurs E
// de /sys/devices/cpu_atom/cpus écartés), mais sur les seuls CPU permis, plafonné
// par le quota. On ne renvoie un nombre que s'il est PLUS PETIT que ce que le
// moteur aurait pris seul ; à la moindre lecture ratée, rien (le moteur décide).
func cpusetThreads(fsys fs.FS) cpuBudget {
	online, err := readCPUList(fsys, "sys/devices/system/cpu/online")
	if err != nil || len(online) == 0 {
		return cpuBudget{}
	}
	allowed, err := procAllowedCPUs(fsys)
	if err != nil {
		return cpuBudget{}
	}
	allowed = intersectCPUs(allowed, online)
	if len(allowed) == 0 {
		return cpuBudget{}
	}
	restricted := len(allowed) < len(online)
	quota := cgroupCPUQuota(fsys)
	if !restricted && quota == 0 {
		return cpuBudget{}
	}
	// Absent = CPU non hybride : aucun cœur E à écarter.
	atom, _ := readCPUList(fsys, "sys/devices/cpu_atom/cpus")

	hostAll, err := physicalCores(fsys, online, nil)
	if err != nil {
		return cpuBudget{}
	}
	hostMath, err := physicalCores(fsys, online, atom)
	if err != nil {
		return cpuBudget{}
	}
	if hostMath == 0 {
		hostMath = hostAll
	}

	var n, engine int
	var why string
	if restricted {
		engine = hostAll // épinglage refusé hors cpuset → repli sur tous les cœurs de l'hôte
		if n, err = physicalCores(fsys, allowed, atom); err != nil {
			return cpuBudget{}
		}
		if n == 0 { // conteneur cantonné aux cœurs E : on les prend, faute de mieux
			if n, err = physicalCores(fsys, allowed, nil); err != nil {
				return cpuBudget{}
			}
		}
		why = fmt.Sprintf("cpuset du conteneur, %d cœurs physiques permis sur %d", n, hostAll)
		if len(atom) > 0 {
			why += " (cœurs E écartés)"
		}
	} else {
		engine, n = hostMath, hostMath
	}
	if quota > 0 && quota < n {
		n = quota
		why = fmt.Sprintf("quota cgroup de %d CPU", quota)
	}
	if n <= 0 || n >= engine {
		return cpuBudget{}
	}
	return cpuBudget{N: n, Engine: engine, Why: why}
}

// physicalCores compte les cœurs physiques parmi cpus comme le fait
// common_cpu_get_num_physical_cores : une chaîne thread_siblings distincte par
// cœur — juste aussi sur les machines multi-puces, où (package, core_id) peut
// se répéter. Les CPU de exclude (cœurs E) ne comptent pas.
func physicalCores(fsys fs.FS, cpus, exclude []int) (int, error) {
	skip := map[int]bool{}
	for _, c := range exclude {
		skip[c] = true
	}
	seen := map[string]bool{}
	for _, c := range cpus {
		if skip[c] {
			continue
		}
		b, err := fs.ReadFile(fsys, fmt.Sprintf("sys/devices/system/cpu/cpu%d/topology/thread_siblings", c))
		if err != nil {
			return 0, err
		}
		seen[strings.TrimSpace(string(b))] = true
	}
	return len(seen), nil
}

// procAllowedCPUs lit Cpus_allowed_list dans /proc/self/status : l'affinité
// réelle du processus, cpuset du conteneur compris.
func procAllowedCPUs(fsys fs.FS) ([]int, error) {
	b, err := fs.ReadFile(fsys, "proc/self/status")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "Cpus_allowed_list:"); ok {
			return parseCPUList(v)
		}
	}
	return nil, fmt.Errorf("pas de Cpus_allowed_list")
}

// cgroupCPUQuota renvoie le quota CPU du cgroup arrondi au CPU supérieur
// (au moins 1), 0 s'il n'y en a pas ou s'il est illisible. cgroup v2
// (cpu.max : « max 100000 » ou « 250000 100000 »), sinon v1 (cfs_quota_us,
// -1 = illimité). Seul un conteneur voit son propre cgroup sous /sys/fs/cgroup.
func cgroupCPUQuota(fsys fs.FS) int {
	if b, err := fs.ReadFile(fsys, "sys/fs/cgroup/cpu.max"); err == nil {
		f := strings.Fields(string(b))
		if len(f) == 0 || f[0] == "max" {
			return 0
		}
		period := "100000"
		if len(f) > 1 {
			period = f[1]
		}
		return ceilQuota(f[0], period)
	}
	for _, dir := range []string{"sys/fs/cgroup/cpu", "sys/fs/cgroup/cpu,cpuacct"} {
		q, err := fs.ReadFile(fsys, dir+"/cpu.cfs_quota_us")
		if err != nil {
			continue
		}
		p, err := fs.ReadFile(fsys, dir+"/cpu.cfs_period_us")
		if err != nil {
			return 0
		}
		return ceilQuota(strings.TrimSpace(string(q)), strings.TrimSpace(string(p)))
	}
	return 0
}

func ceilQuota(quota, period string) int {
	q, err1 := strconv.ParseInt(quota, 10, 64)
	p, err2 := strconv.ParseInt(period, 10, 64)
	if err1 != nil || err2 != nil || q <= 0 || p <= 0 {
		return 0
	}
	return int(max(1, (q+p-1)/p))
}

// readCPUList lit un fichier au format liste du noyau (« 0-3,8-11 »).
func readCPUList(fsys fs.FS, name string) ([]int, error) {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, err
	}
	return parseCPUList(string(b))
}

// parseCPUList décode « 0-3,8-11 » en liste triée, sans doublon. Une liste
// vide est valide (aucun CPU) ; une syntaxe inconnue est une erreur.
func parseCPUList(s string) ([]int, error) {
	set := map[int]bool{}
	for _, part := range strings.Split(strings.TrimSpace(s), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return nil, err
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil {
				return nil, err
			}
		}
		if a < 0 || b < a || b-a > 1<<16 {
			return nil, fmt.Errorf("liste de CPU invalide : %q", part)
		}
		for c := a; c <= b; c++ {
			set[c] = true
		}
	}
	out := make([]int, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Ints(out)
	return out, nil
}

func intersectCPUs(a, b []int) []int {
	in := map[int]bool{}
	for _, c := range b {
		in[c] = true
	}
	var out []int
	for _, c := range a {
		if in[c] {
			out = append(out, c)
		}
	}
	return out
}
