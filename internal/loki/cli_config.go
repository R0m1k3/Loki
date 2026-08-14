package loki

// cli_config.go — lecture/écriture non interactive de la configuration du
// moteur (l'équivalent scriptable de `loki edit`). Ajout du fork Loki : le
// docker-entrypoint s'en sert pour semer BIN/MODEL/CTX/NGL au premier
// démarrage du conteneur, où aucun $EDITOR n'est disponible.

import (
	"fmt"
	"sort"
	"strings"
)

// cmdConfigKV :
//
//	loki config                  affiche toute la configuration (KEY=VALUE)
//	loki config get KEY          affiche la valeur (vide si absente)
//	loki config set K=V [K=V…]   définit des clés ; une valeur vide supprime
func cmdConfigKV(args []string) error {
	if len(args) == 0 {
		cfg := ReadConfig()
		keys := make([]string, 0, len(cfg))
		for k := range cfg {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("%s=%s\n", k, cfg[k])
		}
		return nil
	}
	switch args[0] {
	case "get":
		if len(args) != 2 {
			return fmt.Errorf("usage : loki config get KEY")
		}
		fmt.Println(ReadConfig()[args[1]])
		return nil
	case "set":
		if len(args) < 2 {
			return fmt.Errorf("usage : loki config set KEY=VALUE [KEY=VALUE…]")
		}
		for _, kv := range args[1:] {
			k, v, ok := strings.Cut(kv, "=")
			k = strings.TrimSpace(k)
			if !ok || k == "" {
				return fmt.Errorf("argument invalide %q — attendu KEY=VALUE", kv)
			}
			if err := SetConfigKey(k, strings.TrimSpace(v)); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("usage : loki config [get KEY | set KEY=VALUE…]")
}
