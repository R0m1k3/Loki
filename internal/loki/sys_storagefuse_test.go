package loki

import (
	"runtime"
	"strings"
	"testing"
)

// Extrait réaliste d'un conteneur Loki sur Unraid : racine overlay, /data et
// /models en bind depuis /mnt/user (fuse.shfs), un /data/models imbriqué sur
// un pool btrfs, champs optionnels de longueur variable, un chemin à espace.
const mountinfoUnraid = `612 553 0:156 / / rw,relatime master:221 - overlay overlay rw,lowerdir=/var/lib/docker/l
613 612 0:159 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
620 612 0:52 /appdata/loki/data /data rw,noatime - fuse.shfs shfs rw,user_id=0,group_id=0,allow_other
621 612 0:52 /appdata/loki/models /models rw,noatime shared:5 master:7 - fuse.shfs shfs rw,user_id=0
622 620 0:61 /appdata/loki/fast /data/models rw,noatime - btrfs /dev/nvme0n1p1 rw,ssd
623 612 0:52 /media /mnt/mes\040modeles rw,noatime - fuse.shfs shfs rw
garbage line
624 612 8:1 / /broken rw
`

func TestParseMountinfo(t *testing.T) {
	m := parseMountinfo(strings.NewReader(mountinfoUnraid))
	if len(m) != 6 {
		t.Fatalf("attendu 6 montages valides, obtenu %d : %+v", len(m), m)
	}
	// Champs optionnels (shared:5 master:7) : le type se lit après « - ».
	if m[3].point != "/models" || m[3].fsType != "fuse.shfs" {
		t.Fatalf("ligne à champs optionnels mal lue : %+v", m[3])
	}
	if m[5].point != "/mnt/mes modeles" {
		t.Fatalf("échappement \\040 non décodé : %q", m[5].point)
	}
}

func TestMountFSType(t *testing.T) {
	m := parseMountinfo(strings.NewReader(mountinfoUnraid))
	cases := []struct {
		path, want string
	}{
		{"/data", "fuse.shfs"},
		{"/data/loki.db", "fuse.shfs"},
		{"/data/models", "btrfs"}, // montage imbriqué : le plus long gagne
		{"/data/models/x.gguf", "btrfs"},
		{"/data/modelsX", "fuse.shfs"}, // préfixe de chaîne ≠ préfixe de chemin
		{"/models", "fuse.shfs"},
		{"/mnt/mes modeles/a.gguf", "fuse.shfs"},
		{"/opt/loki", "overlay"},
	}
	for _, c := range cases {
		if got := mountFSType(m, c.path); got != c.want {
			t.Errorf("mountFSType(%q) = %q, attendu %q", c.path, got, c.want)
		}
	}
	if got := mountFSType(nil, "/data"); got != "" {
		t.Errorf("sans montage connu : %q, attendu \"\"", got)
	}
}

func TestShfsPaths(t *testing.T) {
	m := parseMountinfo(strings.NewReader(mountinfoUnraid))
	got := shfsPaths(m, []string{"/data", "/data/models", "/models", "/models/", "/opt"})
	want := []string{"/data", "/models"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("shfsPaths = %v, attendu %v", got, want)
	}
	// Partages en « Exclusive access » : plus de fuse.shfs, plus de conseil.
	excl := `1 0 0:1 / / rw - overlay overlay rw
2 1 0:2 /appdata/loki/data /data rw - btrfs /dev/nvme0n1p1 rw
3 1 0:3 /loki/models /models rw - xfs /dev/md1p1 rw
`
	if p := shfsPaths(parseMountinfo(strings.NewReader(excl)), []string{"/data", "/models"}); len(p) != 0 {
		t.Fatalf("aucun chemin ne devrait être signalé, obtenu %v", p)
	}
}

func TestStorageHintText(t *testing.T) {
	if s := storageHintText(nil); s != "" {
		t.Fatalf("rien à signaler : %q", s)
	}
	s := storageHintText([]string{"/data", "/models"})
	for _, w := range []string{"/data, /models", "fuse.shfs", "Exclusive access"} {
		if !strings.Contains(s, w) {
			t.Errorf("conseil sans %q : %s", w, s)
		}
	}
}

func TestStorageFuseHintOutsideContainer(t *testing.T) {
	t.Setenv("LOKI_CONTAINER", "")
	if s := storageFuseHint(); s != "" {
		t.Fatalf("hors conteneur, aucun conseil attendu : %q", s)
	}
	if runtime.GOOS != "linux" {
		t.Setenv("LOKI_CONTAINER", "1")
		if s := storageFuseHint(); s != "" {
			t.Fatalf("hors Linux, aucun conseil attendu : %q", s)
		}
	}
}
