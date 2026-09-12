package loki

// llm_retry_net.go — réessayer un tour que RIEN n'a encore diffusé.
//
// Jusqu'ici, la requête de complétion partait une fois : un `Do()` qui échoue
// ou un statut d'erreur tuait le tour. Les messages d'erreur eux-mêmes le
// disaient — « réessaie dans quelques secondes », « réessaie dans un
// instant » — c'est-à-dire qu'on demandait à l'utilisateur de refaire à la main
// ce que le code pouvait faire seul.
//
// Le cas courant chez Loki n'a rien d'exotique : le moteur redémarre (bascule de
// preset, rechargement de modèle, redémarrage du conteneur) et la connexion est
// refusée pendant quelques secondes. Une tâche planifiée qui tombe pile là
// échouait pour de bon, sans personne pour cliquer.
//
// LA règle de sûreté, et elle ne souffre pas d'exception : on ne rejoue que
// TANT QU'AUCUN OCTET N'A ÉTÉ DIFFUSÉ. Une fois le flux commencé, la moitié de
// la réponse est déjà chez l'utilisateur ; la rejouer la dupliquerait. Les deux
// points de reprise (échec de `Do`, statut d'erreur avant lecture du corps)
// sont tous deux situés avant la première ligne SSE.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"syscall"
	"time"
)

const (
	// llmNetRetries : tentatives CONSÉCUTIVES par tour. Le compteur repart à zéro
	// dès qu'une réponse arrive, donc une boucle d'outils longue retrouve son
	// budget à chaque itération réussie — mais un moteur durablement mort rend la
	// main au lieu de tourner sans fin.
	llmNetRetries = 3
	// Attente de la 1re reprise, doublée ensuite (0,8 s → 1,6 s → 3,2 s). Un
	// llama-server qui redémarre met quelques secondes à rouvrir son port : plus
	// court ne sert à rien, plus long fait passer une panne franche pour un gel.
	llmNetWait    = 800 * time.Millisecond
	llmNetMaxWait = 5 * time.Second
)

// llmRetryableErr : cette erreur de transport vaut-elle une nouvelle tentative ?
//
// Un ctx déjà fini n'en vaut JAMAIS une : c'est le /stop de l'utilisateur ou la
// limite de l'appelant, et réessayer serait passer outre. On le vérifie donc
// avant de regarder l'erreur elle-même — une échéance dépassée remonte sinon
// comme un simple timeout réseau, indiscernable d'une surcharge du moteur.
func llmRetryableErr(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED): // moteur en cours de démarrage
		return true
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return true
	case isNetTimeout(err): // moteur surchargé ou en plein chargement du modèle
		return true
	}
	return false
}

// llmRetryableStatus : ce code HTTP vaut-il une nouvelle tentative ?
//
// Volontairement étroit. 500 en est ABSENT : c'est le code que llama.cpp rend
// pour un appel d'outil malformé ou un prompt qui dépasse le contexte, deux
// échecs déterministes que les filets au-dessus traitent déjà (retrait des
// outils, compaction en vol). Le rejouer trois fois ne ferait que retarder
// l'erreur de plusieurs secondes. Restent les codes qui disent « pas
// maintenant » : passerelle, service indisponible, trop de requêtes.
func llmRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// llmNetDelay : combien attendre avant la tentative n (0 = la première reprise).
// Doublement puis plafond. Séparé de l'attente pour être vérifiable sans dormir
// cinq secondes dans la suite de tests — et parce qu'un décalage de bits sur un
// n non borné déborderait en silence (0,8 s << 64 revient à zéro).
func llmNetDelay(n int) time.Duration {
	if n < 0 {
		n = 0
	}
	if n > 16 { // au-delà, le plafond s'applique de toute façon
		n = 16
	}
	if d := llmNetWait << n; d < llmNetMaxWait {
		return d
	}
	return llmNetMaxWait
}

// llmNetBackoff attend avant la tentative n (0 = la première reprise).
// Interruptible : un /stop pendant l'attente rend la main tout de suite au lieu
// de faire patienter l'utilisateur pour une requête qu'il vient d'annuler.
func llmNetBackoff(ctx context.Context, n int) error {
	t := time.NewTimer(llmNetDelay(n))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// logLLMRetry trace la reprise. Elle n'est PAS envoyée à l'interface : le tour
// n'a encore rien produit, et faire clignoter une erreur qu'on est en train de
// rattraper inquiéterait pour rien. Le journal du conteneur, lui, doit la voir —
// c'est là qu'on cherche quand « ça a mis huit secondes ».
func logLLMRetry(attempt int, cause string) {
	fmt.Fprintf(os.Stderr, "[llm] %s — nouvelle tentative %d/%d\n", cause, attempt, llmNetRetries)
}
