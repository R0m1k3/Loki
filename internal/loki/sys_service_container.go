//go:build linux

package loki

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// sys_service_container.go — supervision du moteur SANS systemd (conteneur
// Docker, distribution sans systemd, WSL…). Même approche que le mode
// utilisateur de macOS et que Windows : `loki serve` est lancé détaché, son PID
// va dans LOKI_HOME/<svc>.pid et sa sortie dans LOKI_HOME/<svc>.log. Sans ce
// repli, chaque changement de modèle depuis l'UI (serviceAction("restart"))
// échouerait sur un systemctl absent — le conteneur serait inutilisable.
//
// La bascule est automatique : sys_service_linux.go route ici quand
// /run/systemd/system n'existe pas, ou quand LOKI_CONTAINER=1 force le mode.

// systemdAvailable dit si systemd pilote cette machine. /run/systemd/system
// n'existe que lorsque systemd est PID 1 (convention documentée par sd_booted).
func systemdAvailable() bool {
	if os.Getenv("LOKI_CONTAINER") == "1" {
		return false
	}
	st, err := os.Stat("/run/systemd/system")
	return err == nil && st.IsDir()
}

func pidFilePath() string { return filepath.Join(LokiHome(), serviceName()+".pid") }
func logFilePath() string { return filepath.Join(LokiHome(), serviceName()+".log") }

func userSvcAction(action string) error {
	switch action {
	case "start":
		return userSvcStart()
	case "stop":
		return userSvcStop(true)
	case "restart":
		_ = userSvcStop(false)
		time.Sleep(500 * time.Millisecond)
		return userSvcStart()
	case "status":
		pid := readServicePID()
		if pid > 0 && processAlive(pid) {
			fmt.Printf("%s %s: actif (PID %d)\n", green("[ok]"), serviceName(), pid)
		} else {
			fmt.Printf("%s %s: arrêté\n", yellow("[info]"), serviceName())
		}
		fmt.Printf("  logs   : %s\n", logFilePath())
		return nil
	case "enable", "disable":
		fmt.Printf("%s '%s' est sans objet ici (pas de systemd) : en conteneur, le démarrage\n", yellow("[info]"), action)
		fmt.Printf("       est géré par la politique de redémarrage Docker (restart: unless-stopped).\n")
		return nil
	}
	return fmt.Errorf("action inconnue: %s", action)
}

func userSvcStart() error {
	if pid := readServicePID(); pid > 0 && processAlive(pid) {
		fmt.Printf("%s déjà démarré (PID %d)\n", yellow("[info]"), pid)
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(LokiHome(), 0o755); err != nil {
		return err
	}
	logf, err := os.OpenFile(logFilePath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("ouverture du log %s: %w", logFilePath(), err)
	}
	defer logf.Close()

	cmd := exec.Command(self, "serve")
	cmd.Stdout, cmd.Stderr = logf, logf
	// Même répertoire de travail que sous systemd : les chemins relatifs de
	// config.env (MODEL=…gguf) se résolvent depuis LOKI_HOME.
	cmd.Dir = LokiHome()
	// Setsid : l'enfant devient chef de groupe — il survit à la fin de ce
	// process, et un signal au groupe (-pid) arrête aussi llama-server.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("démarrage de 'loki serve': %w", err)
	}
	pid := cmd.Process.Pid
	if err := os.WriteFile(pidFilePath(), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return fmt.Errorf("écriture du PID: %w", err)
	}
	_ = cmd.Process.Release()
	return userCheckStarted(pid)
}

func userCheckStarted(pid int) error {
	time.Sleep(2 * time.Second)
	if processAlive(pid) {
		fmt.Printf("%s %s: démarré (PID %d)\n", green("[ok]"), serviceName(), pid)
		fmt.Printf("       logs: %s  (loki logs pour suivre)\n", dim(logFilePath()))
		return nil
	}
	fmt.Printf("%s %s: le processus s'est arrêté — derniers logs :\n", red("[ERREUR]"), serviceName())
	fmt.Println("------------------------------------------------")
	fmt.Print(tailFile(logFilePath(), 20))
	fmt.Println("------------------------------------------------")
	fmt.Printf("→ loki logs   pour plus de détails\n→ loki edit   pour corriger config.env\n")
	_ = os.Remove(pidFilePath())
	return fmt.Errorf("service %s non démarré", serviceName())
}

func userSvcStop(verbose bool) error {
	pid := readServicePID()
	if pid <= 0 || !processAlive(pid) {
		_ = os.Remove(pidFilePath())
		if verbose {
			fmt.Println(yellow("[info]") + " aucun service en cours d'exécution")
		}
		return nil
	}
	// Setsid a fait de l'enfant un chef de groupe : le PID négatif vise le
	// groupe entier, donc llama-server s'arrête avec lui.
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	for i := 0; i < 40 && processAlive(pid); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if processAlive(pid) {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	_ = os.Remove(pidFilePath())
	if verbose {
		fmt.Println(green("[ok]") + " arrêté")
	}
	return nil
}

// userServiceLogs affiche la fin du log puis suit les ajouts (tail -f minimal,
// sans dépendre d'un binaire `tail` présent dans l'image).
func userServiceLogs() error {
	path := logFilePath()
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("aucun log à %s (le service a-t-il déjà démarré ?): %w", path, err)
	}
	defer f.Close()
	fmt.Print(tailFile(path, 80))
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			os.Stdout.Write(buf[:n])
		}
		if err == io.EOF {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if err != nil {
			return err
		}
	}
}

func readServicePID() int {
	b, err := os.ReadFile(pidFilePath())
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

// processAlive : le signal 0 ne tue rien, il teste juste l'existence du process.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// tailFile renvoie les n dernières lignes d'un fichier (best-effort).
func tailFile(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n") + "\n"
}
