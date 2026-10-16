package loki

import (
	"fmt"
	"os"
	"strings"
)

// facade_push.go — notifications Web Push tenues par la façade Rust
// (LOKI_PUSH=rust, lot 12 de la migration ; expérimental, le Go reste le
// défaut).
//
// La façade sert la clé VAPID et les (dés)abonnements, et pousse elle-même
// les notifications : les envois de ce process (fin de tour, fin de tâche) lui
// sont délégués par /internal/facade/v1/push/send. Clés et abonnements restent
// en base aux mêmes clés : hasPushSubs les lit toujours ici.

const facadePushEnv = "LOKI_PUSH"

// facadePush : la façade tient-elle les notifications ?
func facadePush() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(facadePushEnv)), "rust") &&
		facadeAddr() != "" && internalToken() != ""
}

// facadePushRoutes : les routes que la façade sert alors (push.rs).
var facadePushRoutes = map[string]bool{
	"/api/push/key":         true,
	"/api/push/subscribe":   true,
	"/api/push/unsubscribe": true,
}

// facadePushSend délègue un envoi à la façade ; false quand elle ne tient pas
// les notifications (l'appelant envoie alors lui-même).
func facadePushSend(title, body string) bool {
	if !facadePush() {
		return false
	}
	if err := facadePost("/internal/facade/v1/push/send", map[string]any{"title": title, "body": body}); err != nil {
		fmt.Printf("[push] envoi délégué à la façade : %v\n", err)
	}
	return true
}
