package loki

// facade_core.go — les routes de base que la façade sert toujours (api.rs,
// lot 35 de la migration) : santé, état, configuration, ligne de commande du
// moteur, mémoire vive et vidéo, journal du service, chemins, couches du
// modèle. Quand elles arrivent par le tunnel du relais (accès distant, E2E),
// le Go les lui remet aussi, comme les routes des lots : ses propres
// handlers ne servent plus que sans façade (LOKI_FACADE=0).

var facadeCoreRoutes = map[string]bool{
	"/api/ping":           true,
	"/api/status":         true,
	"/api/config":         true,
	"/api/engine/cmdline": true,
	"/api/model/layers":   true,
	"/api/paths":          true,
	"/api/ram":            true,
	"/api/vram":           true,
	"/api/service/log":    true,
}

// facadeCore : une façade tourne devant ce processus (son adresse et le
// jeton interne sont là).
func facadeCore() bool {
	return facadeAddr() != "" && internalToken() != ""
}
