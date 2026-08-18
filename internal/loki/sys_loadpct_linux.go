//go:build linux

package loki

// sys_loadpct_linux.go — pourcentage de CHARGEMENT du modèle, pour la pastille
// « chargement… » de l'UI qui restait muette de longues minutes sur un gros
// GGUF (un 27B Q4 met facilement 1 à 3 minutes à monter en VRAM).
//
// llama-server n'expose aucun progrès pendant le chargement (/health renvoie
// juste 503). Mais le chargement, c'est LIRE le fichier .gguf : on mesure donc
// les octets lus par le process moteur (/proc/<pid>/io, read_bytes — les pages
// mmap rentrées comptent dedans) et on les rapporte à la taille du fichier.
// Approximation honnête : monotone, ~0→100, et surtout elle BOUGE — c'est ce
// qui distingue « ça charge » de « c'est planté ».

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// engineLoadPct renvoie le pourcentage estimé de chargement du modèle, ou -1
// quand il n'est pas mesurable (pas de PID, /proc illisible, taille inconnue).
func engineLoadPct() int {
	pid := readServicePID()
	if pid <= 0 {
		return -1
	}
	cfg := ReadConfig()
	model := cfg["MODEL"]
	if model == "" {
		return -1
	}
	path, err := resolveServeModelPath(model)
	if err != nil {
		return -1
	}
	size := modelTotalSize(path)
	if size <= 0 {
		return -1
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/io")
	if err != nil {
		return -1
	}
	var read int64 = -1
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "read_bytes:"); ok {
			read, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			break
		}
	}
	if read < 0 {
		return -1
	}
	pct := int(read * 100 / size)
	if pct > 99 {
		// 100 % n'existe pas ici : c'est /health qui dit « prêt ». Au-delà de 99
		// (relectures, autres IO du process), on plafonne.
		pct = 99
	}
	return pct
}

// modelTotalSize : taille du modèle, shards compris (model-00001-of-00004.gguf
// → somme des tranches présentes — c'est tout ça qu'il faut charger).
func modelTotalSize(path string) int64 {
	if total := shardFamilySize(filepath.Dir(path), filepath.Base(path)); total > 0 {
		return total
	}
	if st, err := os.Stat(path); err == nil {
		return st.Size()
	}
	return 0
}
