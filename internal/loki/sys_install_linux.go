//go:build linux

package loki

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// sys_install_linux.go — la part systemd de l'installation. Le parcours commun
// (dossiers, lien du binaire, /etc/default, chown) vit dans sys_install_unix.go.

// sudoersTemplate autorise l'utilisateur à piloter UNE unité sans mot de passe.
// Posé pour les deux : l'interface, qui tourne sans privilèges, doit pouvoir
// redémarrer le moteur (bascule de preset) comme elle-même (nouveau jeton).
const sudoersTemplate = `# Permet à %[1]s de piloter l'unité %[2]s sans mot de passe (posé par loki install).
%[1]s ALL=(root) NOPASSWD: /bin/systemctl start %[2]s, /bin/systemctl stop %[2]s, /bin/systemctl restart %[2]s, /bin/systemctl enable %[2]s, /bin/systemctl disable %[2]s
`

// engineUnitTemplate — le moteur : exec llama-server, supervisé directement par
// systemd. Champs : User, WorkingDirectory, ExecStart.
const engineUnitTemplate = `[Unit]
Description=Loki — moteur llama.cpp
After=network.target

[Service]
Type=simple
User=%s
WorkingDirectory=%s
ExecStart=%s
Restart=on-failure
RestartSec=3
# Lancement refusé d'avance par LOAD_GUARD (le mode de chargement ne tient pas
# en RAM) : relancer n'y changerait rien.
RestartPreventExitStatus=78

# Priorité CPU : on remonte le process pour qu'il ne soit pas dépriorisé face
# aux tâches de fond (sampling/orchestration côté CPU pèsent sur le débit même
# en inference GPU). Nice négatif + scheduling normal réactif.
Nice=-10
CPUSchedulingPolicy=other
CPUAccounting=yes

[Install]
WantedBy=multi-user.target
`

// uiUnitTemplate — l'interface : UI web locale, tunnel du relais et endpoint
// OpenAI, servis par un SEUL process (donc une seule conversation).
// Champs : unité du moteur (dépendance), User, WorkingDirectory, ExecStart.
const uiUnitTemplate = `[Unit]
Description=Loki — interface web + accès distant
After=network-online.target %s.service
Wants=network-online.target

[Service]
Type=simple
User=%s
WorkingDirectory=%s
ExecStart=%s
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`

// engineUnitPath : l'unité du moteur telle que « loki install » l'écrit.
func engineUnitPath() string { return "/etc/systemd/system/" + serviceName() + ".service" }

// supervisorRestartsRefusal : « loki serve » tourne-t-il sous une unité
// systemd qui le relancerait sur loadGuardExitStatus ? C'est le cas d'une unité
// écrite avant que Loki ne pose RestartPreventExitStatus : « loki update » ne
// réécrit pas les unités (il tourne sans droits root), seul « loki install »
// le fait. Reconnu à coup sûr : systemd pour parent, INVOCATION_ID posé par
// systemd, et notre unité lisible sans la directive. Dans le doute, non : le
// code de sortie reste celui d'une erreur.
func supervisorRestartsRefusal() (bool, string) {
	if !systemdAvailable() || os.Getenv("INVOCATION_ID") == "" || os.Getppid() != 1 {
		return false, ""
	}
	b, err := os.ReadFile(engineUnitPath())
	if err != nil {
		return false, ""
	}
	// Les compléments (« systemctl edit ») comptent aussi.
	unit := string(b)
	drops, _ := filepath.Glob(engineUnitPath() + ".d/*.conf")
	for _, p := range drops {
		if d, err := os.ReadFile(p); err == nil {
			unit += "\n" + string(d)
		}
	}
	if unitPreventsRefusalRestart(unit) {
		return false, ""
	}
	return true, fmt.Sprintf("unité %s d'une version précédente : sortie sans erreur pour que systemd ne relance pas "+
		"en boucle — « sudo systemctl edit %s », section [Service], RestartPreventExitStatus=%d (ou « sudo loki install ») "+
		"la met à jour", serviceName(), serviceName(), loadGuardExitStatus)
}

func installServices(targetUser, lokiHome string) error {
	svc, exe := serviceName(), installedExePath()
	units := map[string]string{
		svc:        fmt.Sprintf(engineUnitTemplate, targetUser, lokiHome, exe+" serve"),
		uiUnitName: fmt.Sprintf(uiUnitTemplate, svc, targetUser, lokiHome, exe+" web"),
	}
	for name, body := range units {
		path := "/etc/systemd/system/" + name + ".service"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return err
		}
		fmt.Printf("  %s %s\n", green("✓"), path)

		sudoers := fmt.Sprintf(sudoersTemplate, targetUser, name)
		sudoersPath := "/etc/sudoers.d/" + name
		if err := os.WriteFile(sudoersPath, []byte(sudoers), 0o440); err != nil {
			return err
		}
		fmt.Printf("  %s %s\n", green("✓"), sudoersPath)
	}
	_ = exec.Command("systemctl", "daemon-reload").Run()
	return nil
}

// uninstallServices arrête, désactive et efface les deux unités. L'interface
// passe AVANT le moteur : elle sait le redémarrer, l'ordre inverse pourrait le
// relancer sous nos pieds.
func uninstallServices() {
	svc := serviceName()
	for _, name := range []string{uiUnitName, svc} {
		_ = exec.Command("systemctl", "stop", name).Run()
		_ = exec.Command("systemctl", "disable", name).Run()
	}
	for _, p := range []string{
		"/etc/systemd/system/" + svc + ".service",
		"/etc/systemd/system/" + uiUnitName + ".service",
		"/etc/sudoers.d/" + svc,
		"/etc/sudoers.d/" + uiUnitName,
	} {
		if err := os.Remove(p); err == nil {
			fmt.Printf("  %s %s\n", green("✓"), p)
		}
	}
	_ = exec.Command("systemctl", "daemon-reload").Run()
}
