package loki

import (
	"encoding/json"
	"strings"
	"testing"
)

// Budget du préambule envoyé À CHAQUE TOUR : prompt système + schémas des outils.
// Il est payé sur toute la conversation et sort du contexte utile.
//
// Ce test existe pour empêcher le regonflement. Le prompt a déjà été raccourci
// une fois pour une raison de comportement (un préambule verbeux fait
// sur-raisonner les modèles à reasoning, qui finissent leur tour sans appeler
// d'outil), puis une seconde fois pour le coût. Les deux fois, il avait
// regrossi ligne par ligne, chacune paraissant anodine.
//
// Approximation 1 tok ≈ 4 caractères : suffisante pour une alerte, et sans
// dépendance à un tokenizer.
//
// Le budget n'a été relevé que pour de VRAIS outils nouveaux, jamais pour des
// lignes de prompt — et chaque palier dit lequel :
//
//	7500 → 7900 : recall + recall_search (chat_recall.go), ~720 car. Le compactage
//	              cesse d'être destructif : le modèle ne relance plus les lectures
//	              et recherches déjà faites. Il récupère bien plus qu'il ne coûte.
//	7900 → 8600 : tracker (tracker.go), ~620 car. Un seul schéma pour les quatre
//	              actions — quatre outils auraient coûté quatre fois ça.
//	8600 → 11000 : les quatre outils task_* (tasks_tools.go), ~1290 car. L'IA se
//	              donne elle-même ses rappels et ses veilles ; sans eux il fallait
//	              passer par l'interface. Descriptions déjà au plus court, et un
//	              seul schéma par action (create/update/delete ne partagent pas
//	              assez d'arguments pour fusionner sans rendre chacun ambigu).
//	              Les ~500 car restants couvrent web_screenshot, déclaré dès que
//	              Playwright est présent — c'est le cas dans l'image, pas sur le
//	              runner de CI : le budget doit tenir pour l'image, la vraie.
//	              (Le pilotage de navigateur, lui, est HORS de ce décompte : ses
//	              schémas n'existent que si on arme son interrupteur.)
//
//	              Les ~200 car de prompt qui les accompagnent sont le seul écart
//	              assumé à la règle « pas de relèvement pour du prompt » : un outil
//	              qu'aucune ligne ne mentionne n'est jamais appelé de lui-même, et
//	              la ligne dit ce que les schémas ne disent pas (quand se planifier,
//	              et qu'un script planifié ne charge aucun modèle). Elle a été
//	              ramenée de trois lignes à une.
//
// Descriptions déjà réduites au strict nécessaire. La consigne d'origine tient
// pour la suite : ce budget ne se relève pas pour du prompt.
const promptCharBudget = 11000 // ~2750 tokens, tout allumé

func TestSystemPromptStaysLean(t *testing.T) {
	caps := Caps{Agent: true, Internet: true, Mem: MemAlways}
	sp := baseSystemPrompt(caps)
	tb, err := json.Marshal(EnabledTools(caps))
	if err != nil {
		t.Fatal(err)
	}
	total := len(sp) + len(tb)
	t.Logf("prompt %d car (~%d tok) + outils %d car (~%d tok) = ~%d tok",
		len(sp), len(sp)/4, len(tb), len(tb)/4, total/4)
	if total > promptCharBudget {
		t.Fatalf("préambule à %d car (~%d tok), budget %d car (~%d tok).\n"+
			"Avant de relever le budget : les schémas d'outils pèsent le double du prompt,\n"+
			"et une consigne écrite dans un schéma n'a pas à être répétée dans le prompt.",
			total, total/4, promptCharBudget, promptCharBudget/4)
	}
}

// Le prompt ne doit pas redevenir un catalogue d'outils : leurs schémas partent
// dans la MÊME requête et les décrivent déjà un par un.
func TestSystemPromptDoesNotRelistTools(t *testing.T) {
	sp := baseSystemPrompt(Caps{Agent: true, Internet: true, Mem: MemAlways})
	for _, line := range strings.Split(sp, "\n") {
		l := strings.TrimSpace(line)
		if !strings.HasPrefix(l, "- ") {
			continue
		}
		// Une puce qui commence par un nom d'outil = un catalogue qui revient.
		for _, tool := range []string{"bash ", "write ", "edit ", "mem_", "web_"} {
			if strings.HasPrefix(l[2:], tool) {
				t.Fatalf("le prompt réénumère un outil : %q", l)
			}
		}
	}
}

// Sans agent ni mémoire, aucun préambule : un modèle de chat simple à qui on
// ordonne d'appeler des outils invente des appels textuels qui fuient dans la
// réponse.
func TestSystemPromptEmptyWithoutTools(t *testing.T) {
	if sp := baseSystemPrompt(Caps{Mem: MemOff}); sp != "" {
		t.Fatalf("préambule non vide sans outils : %q", sp)
	}
}
