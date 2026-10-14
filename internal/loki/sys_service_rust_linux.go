//go:build linux

package loki

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Supervision du moteur déléguée à la façade Rust (lot 3 de la migration,
// rust/loki-rs/src/engine). En conteneur, start/stop/restart/status passent
// par `loki-rs engine <action>` : mêmes fichiers PID et journal, même arrêt de
// tout l'arbre, mêmes délais. Les garde-fous restent ici, en amont
// (serviceAction : optimisation en cours ; userSvcAction : preset externe) ;
// `loki serve` construit toujours la ligne de commande du moteur.
//
// Repli sur la supervision Go si loki-rs est absent à côté de ce binaire, ou
// si LOKI_ENGINE_SUPERVISOR=go (porte de sortie).

// rustEngineSupervisorFor : le loki-rs voisin de exe, s'il doit superviser.
func rustEngineSupervisorFor(exe string) string {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("LOKI_ENGINE_SUPERVISOR")), "go") || exe == "" {
		return ""
	}
	p := filepath.Join(filepath.Dir(exe), "loki-rs")
	st, err := os.Stat(p)
	if err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
		return ""
	}
	return p
}

func rustEngineSupervisor() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return rustEngineSupervisorFor(exe)
}

// rustSvcAction lance `loki-rs engine <action>`. Sa sortie s'affiche telle
// quelle ; en cas d'échec, sa dernière ligne d'erreur devient l'erreur rendue
// (c'est elle que l'UI montre), sans le préfixe du binaire.
func rustSvcAction(rs, action string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd := exec.Command(rs, "engine", action)
	cmd.Stdout = os.Stdout
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	cmd.Env = append(os.Environ(),
		"LOKI_GO_BIN="+self,
		"LOKI_HOME="+LokiHome(),
		"LOKI_SERVICE="+serviceName(),
	)
	if err := cmd.Run(); err != nil {
		if msg := lastErrorLine(stderr.String()); msg != "" {
			return errors.New(msg)
		}
		return fmt.Errorf("loki-rs engine %s : %w", action, err)
	}
	return nil
}

func lastErrorLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	return strings.TrimSpace(strings.TrimPrefix(last, "[loki-rs engine]"))
}

// rustUserSvcAction : version déléguée de userSvcAction. handled=false laisse
// l'action au Go (logs, enable/disable).
func rustUserSvcAction(rs, action string) (handled bool, err error) {
	switch action {
	case "start", "restart":
		// Preset externe : `loki serve` sortirait aussitôt (rien à lancer),
		// et le superviseur le prendrait pour un plantage.
		if isExternalConfig(ReadConfig()) {
			if action == "restart" {
				_ = rustSvcAction(rs, "stop")
			}
			fmt.Printf("%s preset externe actif : pas de moteur local à démarrer\n", dim("[info]"))
			return true, nil
		}
		return true, rustSvcAction(rs, action)
	case "stop", "status":
		return true, rustSvcAction(rs, action)
	}
	return false, nil
}
