package loki

import (
	"time"
)

// Délais MCP réglables depuis l'UI (repris d'AJEAN c05a0a9f) : connexion d'une
// session et appel d'outil, en secondes, communs à tous les serveurs. Un
// serveur peut remplacer le délai d'appel par le sien (CallTimeoutSec), et un
// lancement npx/uvx garde au moins mcpConnectTimeoutCold pour se connecter.

// MCPTimeouts : les délais globaux, en secondes. Stockés dans bkState sous
// la clé mcpTimeoutsKey ; zéro (ou clé absente) = valeur par défaut.
type MCPTimeouts struct {
	// Connect borne l'établissement d'une session (handshake initialize +
	// tools/list). Un serveur qui rame ne doit pas figer le tour de chat.
	Connect int `json:"connect"`
	// Call borne un appel d'outil MCP.
	Call int `json:"call"`
}

const (
	// Valeurs par défaut, en secondes (les anciennes constantes).
	mcpConnectTimeoutDefault = 20
	mcpCallTimeoutDefault    = 120
	// mcpTimeoutMaxSec : borne haute des réglages (1 h), globaux comme par
	// serveur.
	mcpTimeoutMaxSec = 3600
	mcpTimeoutsKey   = "mcp_timeouts"
)

// LoadMCPTimeouts lit les délais globaux. Une valeur absente, nulle ou
// illisible retombe sur son défaut ; une valeur hors bornes (base modifiée à
// la main) est ramenée à mcpTimeoutMaxSec.
func LoadMCPTimeouts() MCPTimeouts {
	var t MCPTimeouts
	if !getJSON(bkState, mcpTimeoutsKey, &t) {
		t = MCPTimeouts{}
	}
	return t.normalized()
}

func (t MCPTimeouts) normalized() MCPTimeouts {
	fix := func(v, def int) int {
		switch {
		case v <= 0:
			return def
		case v > mcpTimeoutMaxSec:
			return mcpTimeoutMaxSec
		}
		return v
	}
	return MCPTimeouts{
		Connect: fix(t.Connect, mcpConnectTimeoutDefault),
		Call:    fix(t.Call, mcpCallTimeoutDefault),
	}
}

// SaveMCPTimeouts enregistre les délais globaux (valeurs déjà validées par
// l'appelant ; normalisées par sûreté).
func SaveMCPTimeouts(t MCPTimeouts) error {
	mcpConfigMu.Lock()
	defer mcpConfigMu.Unlock()
	return putJSON(bkState, mcpTimeoutsKey, t.normalized())
}

// connect et call : les délais en durées.
func (t MCPTimeouts) connect() time.Duration { return time.Duration(t.Connect) * time.Second }
func (t MCPTimeouts) call() time.Duration    { return time.Duration(t.Call) * time.Second }

// mcpTimeoutsError : le refus d'un réglage hors bornes, nil sinon. Même texte
// pour la façade (mcp/mod.rs).
func mcpTimeoutsError(t MCPTimeouts) string {
	if t.Connect < 1 || t.Connect > mcpTimeoutMaxSec {
		return "connexion : 1 à 3600 s"
	}
	if t.Call < 1 || t.Call > mcpTimeoutMaxSec {
		return "appel d'outil : 1 à 3600 s"
	}
	return ""
}

// mcpCallTimeoutSecError : le refus d'un délai par serveur hors bornes.
func mcpCallTimeoutSecError(sec int) string {
	if sec < 0 || sec > mcpTimeoutMaxSec {
		return "délai d'un appel : 0 à 3600 s (vide = réglage global)"
	}
	return ""
}
