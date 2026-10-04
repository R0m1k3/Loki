package loki

import "testing"

// La sortie de `llama-server --list-devices` : c'est elle qui fait foi pour
// l'UI, car les noms ET l'ordre dépendent du backend compilé dans CE moteur
// (sur la même machine : CUDA0 = la grosse carte, Vulkan0 = la petite).
func TestParseListDevices(t *testing.T) {
	out := `ggml_cuda_init: found 2 CUDA devices:
Available devices:
  CUDA0: NVIDIA GeForce RTX 5060 Ti (15849 MiB, 15579 MiB free)
  CUDA1: NVIDIA GeForce GTX 1650 (3715 MiB, 3659 MiB free)
`
	devs := parseListDevices(out)
	if len(devs) != 2 {
		t.Fatalf("%d device(s) extraits, attendu 2 : %v", len(devs), devs)
	}
	if devs[0]["id"] != "CUDA0" || devs[0]["name"] != "NVIDIA GeForce RTX 5060 Ti" {
		t.Errorf("device 0 mal lu : %v", devs[0])
	}
	if devs[0]["total_mib"] != 15849 || devs[1]["free_mib"] != 3659 {
		t.Errorf("mémoire mal lue : %v / %v", devs[0], devs[1])
	}
	if devs[1]["id"] != "CUDA1" {
		t.Errorf("device 1 mal lu : %v", devs[1])
	}
}

// Backend Vulkan : même format, autres noms — et rien d'autre ne doit passer.
func TestParseListDevicesVulkanEtBruit(t *testing.T) {
	out := `load_backend: loaded Vulkan backend
Available devices:
  Vulkan0: NVIDIA GeForce GTX 1650 (4342 MiB, 3950 MiB free)
  Vulkan1: NVIDIA GeForce RTX 5060 Ti (16311 MiB, 15848 MiB free)
warning: something else entirely
`
	devs := parseListDevices(out)
	if len(devs) != 2 || devs[0]["id"] != "Vulkan0" || devs[1]["id"] != "Vulkan1" {
		t.Fatalf("devices Vulkan mal extraits : %v", devs)
	}
	if n := len(parseListDevices("aucun device ici\n")); n != 0 {
		t.Errorf("%d device(s) extraits d'une sortie sans device", n)
	}
}

// Mémoire complétée : un device dont le moteur n'a pas su lire la taille (0 Mio,
// carte saturée) récupère celle de nvidia-smi par correspondance de nom ; la
// liaison PCIe s'y ajoute, pour les cartes CUDA seulement.
func TestAnnotateDevicesParNom(t *testing.T) {
	devs := []map[string]any{
		{"id": "CUDA0", "name": "NVIDIA GeForce RTX 5060 Ti", "total_mib": 0, "free_mib": 0},
		{"id": "CUDA1", "name": "NVIDIA GeForce GTX 1650", "total_mib": 3715, "free_mib": 3659},
	}
	gpus := []gpuInfo{
		{Name: "NVIDIA GeForce GTX 1650", MemTotal: "4096", LinkGen: 1, LinkGenMax: 3, LinkWidth: 16, LinkWidthMax: 16},
		{Name: "NVIDIA GeForce RTX 5060 Ti", MemTotal: "16311", LinkGen: 5, LinkGenMax: 5, LinkWidth: 8, LinkWidthMax: 8, MemClockMax: 14001},
	}
	annotateDevices(devs, gpus)
	if devs[0]["total_mib"] != 16311 {
		t.Errorf("mémoire non complétée : %v", devs[0])
	}
	if devs[1]["total_mib"] != 3715 {
		t.Errorf("mémoire déjà connue écrasée : %v", devs[1])
	}
	l, _ := devs[0]["link"].(map[string]any)
	if l == nil || l["gen_max"] != 5 || l["width_max"] != 8 || l["mem_clock_mhz"] != 14001 {
		t.Errorf("liaison absente ou fausse : %v", devs[0]["link"])
	}
	if l, _ := devs[1]["link"].(map[string]any); l == nil || l["width"] != 16 {
		t.Errorf("liaison de la seconde carte : %v", devs[1]["link"])
	}
}

// Deux cartes homonymes : correspondance ambiguë, rien n'est ajouté. Une carte
// Vulkan garde sa mémoire complétée mais pas de liaison.
func TestAnnotateDevicesAmbiguEtVulkan(t *testing.T) {
	devs := []map[string]any{
		{"id": "CUDA0", "name": "NVIDIA GeForce RTX 3060", "total_mib": 0},
		{"id": "CUDA1", "name": "NVIDIA GeForce RTX 3060", "total_mib": 12288},
	}
	twins := []gpuInfo{
		{Name: "NVIDIA GeForce RTX 3060", MemTotal: "12288", LinkGenMax: 4, LinkWidthMax: 16},
		{Name: "NVIDIA GeForce RTX 3060", MemTotal: "12288", LinkGenMax: 3, LinkWidthMax: 4},
	}
	annotateDevices(devs, twins)
	if devs[0]["total_mib"] != 0 || devs[0]["link"] != nil || devs[1]["link"] != nil {
		t.Errorf("cartes homonymes annotées quand même : %v", devs)
	}
	vk := []map[string]any{{"id": "Vulkan0", "name": "NVIDIA GeForce RTX 3060", "total_mib": 0}}
	annotateDevices(vk, twins[:1])
	if vk[0]["total_mib"] != 12288 || vk[0]["link"] != nil {
		t.Errorf("carte Vulkan : %v", vk[0])
	}
}

// Le cache ne doit pas figer une lecture dégénérée (0 Mio) pendant 10 minutes.
func TestHasZeroMemory(t *testing.T) {
	ok := []map[string]any{{"total_mib": 16311}, {"total_mib": 3715}}
	ko := []map[string]any{{"total_mib": 16311}, {"total_mib": 0}}
	if hasZeroMemory(ok) {
		t.Error("lecture complète prise pour dégénérée")
	}
	if !hasZeroMemory(ko) {
		t.Error("lecture à 0 Mio non détectée")
	}
}
