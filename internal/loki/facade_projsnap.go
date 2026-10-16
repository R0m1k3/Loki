package loki

import (
	"net/http"
)

// facade_projsnap.go — PROJ_SNAPSHOT pour le tour tenu par la façade (lot 34
// de la migration). La façade range l'instantané avec la discussion (il est
// persisté avec elle, champ proj_snap) et pose les <context_update> dans son
// historique ; le Go, qui lit le contexte du projet (description, index
// mémoire, trackers, consignes du dépôt, date et dossier de travail), décide :
//
//   - /internal/v1/turn/view, avec la clé et l'agent : la tête du tour porte
//     le bloc figé (ou le vivant s'il faut rafraîchir), et projsnap dit quoi
//     faire de la discussion — rafraîchir (instantané neuf, blocs retirés de
//     l'historique) ou poser une mise à jour en tête du dernier message
//     utilisateur. dry : la même décision pour le préchauffage, sans rien
//     rendre à ranger ;
//   - /internal/v1/turn/projsnap/refresh : une compaction ou une réduction
//     réécrit l'historique en plein tour, le bloc vivant remplace le figé et
//     devient l'instantané du tour suivant.

// projSnapView : les pièces de turnView avec la clé (assembleTurn, chemin
// PROJ_SNAPSHOT). snap : l'instantané de la discussion (nil : aucun).
func projSnapView(caps Caps, snap *projSnapshot, dry bool) map[string]any {
	cfg := ReadConfig()
	// Date et dossier de travail : dans le bloc, plus dans le système.
	caps.envInCtx = true
	asOf := projSnapNow()
	live := projSnapCapture(caps, asOf)
	key := projSnapKey(caps, cfg)
	tools := EnabledTools(caps)
	if tools == nil {
		tools = []Tool{}
	}
	head := []Message{}
	if sp := readSysPrompt(); sp != "" {
		head = append(head, Message{Role: "system", Content: sp})
	}
	sysHash := projSnapSysHash(InjectSkills(append([]Message(nil), head...), caps, tools), tools)
	refresh, delta, tok := projSnapDecide(snap, live, key, sysHash, asOf)
	block := live.msgs
	if !refresh {
		block = snap.Msgs
	}
	out := map[string]any{"refresh": refresh, "key": key, "sys_hash": sysHash}
	if !dry {
		switch {
		case refresh:
			out["snap"] = newProjSnapshot(live, key, sysHash)
		case delta != "":
			// Rangé par la façade seulement si la mise à jour a trouvé un
			// message utilisateur où se poser (projSnapApplyLocked).
			next := *snap
			next.State = live.state
			next.DeltaTok += tok
			out["snap"] = &next
			out["delta"] = delta
		default:
			out["snap"] = snap
		}
	}
	return map[string]any{
		"head":     append(head, block...),
		"prefix":   injectPrefix(caps, tools),
		"tools":    tools,
		"projsnap": out,
	}
}

// handleInternalTurnProjSnapRefresh : POST {"caps","key","sys_hash"} →
// {"block":[…],"snap":{…}} — le bloc vivant et l'instantané qu'il devient
// (projSnapTurn.refresh).
func handleInternalTurnProjSnapRefresh(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Caps    Caps   `json:"caps"`
		Key     string `json:"key"`
		SysHash string `json:"sys_hash"`
	}
	if !readInternalJSON(w, r, &in) {
		return
	}
	caps := in.Caps
	caps.envInCtx = true
	live := projSnapCapture(caps, projSnapNow())
	block := live.msgs
	if block == nil {
		block = []Message{}
	}
	sendJSON(w, http.StatusOK, map[string]any{
		"block": block,
		"snap":  newProjSnapshot(live, in.Key, in.SysHash),
	})
}
