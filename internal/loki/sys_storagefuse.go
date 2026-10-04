package loki

// sys_storagefuse.go — conseil de PERFORMANCE pour Unraid : /data et /models
// servis par la couche FUSE des partages (fuse.shfs).
//
// Sur Unraid, /mnt/user/… n'est pas un disque mais un démon en espace
// utilisateur (shfs) qui agrège array et pools. Tout y passe : chaque fsync de
// loki.db, et surtout chaque page du .gguf que le noyau doit relire quand un
// gros MoE mmappé ne tient pas en RAM — pendant le décodage, à chaque token.
// Rien de perdu, rien d'altéré (les octets du modèle sont les mêmes), mais tout
// est plus lent.
//
// Loki ne déplace RIEN : changer le chemin hôte d'une installation existante
// donnerait un /data vide et ferait croire à une perte de données (voir le
// README). Il se contente de le VOIR et de le DIRE, sur un ton d'information —
// pas le bandeau rouge de sys_datavolume.go, réservé à la perte de données. Le
// remède sans changement de chemin : des partages en « Exclusive access »
// (Unraid 6.12+). Ce n'est pas une case à cocher mais un état : « Permit
// exclusive shares » activé, partage tout entier sur un pool, sans stockage
// secondaire — /mnt/user/<partage> devient alors un lien symbolique vers le
// pool, que Docker résout au démarrage du conteneur.

import (
	"bufio"
	"io"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// mountEntry : ce qu'on retient d'une ligne de /proc/self/mountinfo.
type mountEntry struct {
	point  string // point de montage (5ᵉ champ), échappements décodés
	fsType string // type de système de fichiers (1ᵉʳ champ après « - »)
}

// parseMountinfo lit le format de /proc/<pid>/mountinfo. Le nombre de champs
// optionnels (shared:N, master:N…) varie d'une ligne à l'autre : le type de
// système de fichiers se lit donc APRÈS le séparateur « - », jamais à un index
// fixe. Les lignes malformées sont ignorées.
func parseMountinfo(r io.Reader) []mountEntry {
	var out []mountEntry
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		sep := -1
		for i := 5; i < len(fields); i++ {
			if fields[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+1 >= len(fields) {
			continue
		}
		out = append(out, mountEntry{
			point:  path.Clean(unescapeMountinfo(fields[4])),
			fsType: fields[sep+1],
		})
	}
	return out
}

// unescapeMountinfo décode les séquences octales du noyau (\040 espace,
// \011 tabulation, \012 saut de ligne, \134 antislash).
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// mountFSType renvoie le type de système de fichiers qui porte path : celui du
// point de montage le plus LONG qui le contient (/data/models peut être un
// montage imbriqué distinct de /data). À longueur égale, la dernière ligne
// gagne — c'est le montage empilé par-dessus, celui qu'on voit vraiment.
// "" quand aucun montage ne couvre le chemin. Chemins Linux : package path, et
// non filepath, pour que les tests lisent la même chose sous Windows.
func mountFSType(mounts []mountEntry, target string) string {
	m, _ := mountOf(mounts, target)
	return m.fsType
}

// mountOf renvoie le montage qui porte target (règle de mountFSType) ; false
// quand aucun ne le couvre.
func mountOf(mounts []mountEntry, target string) (mountEntry, bool) {
	p := path.Clean(target)
	best, found := -1, mountEntry{}
	for _, m := range mounts {
		mp := m.point
		if mp != "/" && p != mp && !strings.HasPrefix(p, mp+"/") {
			continue
		}
		if len(mp) >= best {
			best, found = len(mp), m
		}
	}
	return found, best >= 0
}

// shfsPaths renvoie, parmi paths, ceux qui passent par fuse.shfs, dans l'ordre
// reçu et un seul par montage : /data/models, qui vit sur le montage de /data,
// n'apprendrait rien de plus à l'utilisateur et allongerait le conseil.
func shfsPaths(mounts []mountEntry, paths []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		m, ok := mountOf(mounts, p)
		if !ok || m.fsType != "fuse.shfs" || seen[m.point] {
			continue
		}
		seen[m.point] = true
		out = append(out, path.Clean(p))
	}
	return out
}

// storageHintText formule le conseil pour les chemins concernés ("" si aucun).
// Conseil uniquement : les fichiers sont intacts, seule la vitesse en pâtit.
func storageHintText(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	return "ℹ️ Stockage via la couche FUSE des partages Unraid (fuse.shfs) : " + strings.Join(paths, ", ") + ". " +
		"Les écritures de la base et les pages du modèle relues depuis le disque (gros modèle qui ne tient pas en RAM) " +
		"traversent un démon en espace utilisateur, ce qui ralentit le décodage. Rien n'est perdu. " +
		"Remède sans changer de chemin (Unraid 6.12+) : Global Share Settings → « Permit exclusive shares » = Yes, " +
		"partages appdata/modèles entièrement sur un pool sans stockage secondaire, jusqu'à lire « Exclusive access : Yes », " +
		"puis redémarrer le conteneur. Voir docker-compose.unraid.yml."
}

// storageHint mis en cache : /api/status est interrogé en boucle, et la liste
// des dossiers de modèles passe par la base. Les montages ne bougent pas pendant
// la vie du conteneur ; une minute suffit à refléter un dossier ajouté dans l'UI.
var storageHintCache struct {
	sync.Mutex
	at  time.Time
	val string
}

// storageFuseHint renvoie le conseil « /data ou /models sur fuse.shfs » ("" si
// sans objet). Linux en conteneur seulement, comme dataVolumeWarning : hors
// conteneur, l'utilisateur gère ses chemins.
func storageFuseHint() string {
	if runtime.GOOS != "linux" || os.Getenv("LOKI_CONTAINER") == "" {
		return ""
	}
	c := &storageHintCache
	c.Lock()
	defer c.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < time.Minute {
		return c.val
	}
	c.at = time.Now()
	c.val = ""
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "" // illisible : on ne conseille rien plutôt que de deviner
	}
	defer f.Close()
	mounts := parseMountinfo(f)
	paths := append([]string{LokiHome()}, modelDirs()...)
	c.val = storageHintText(shfsPaths(mounts, paths))
	return c.val
}
