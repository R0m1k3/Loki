package loki

import (
	"bytes"
	"encoding/json"
)

// orderedProps : propriétés d'un schéma d'outil sérialisées DANS L'ORDRE donné.
//
// Une map Go sort ses clés par ordre alphabétique : write annonçait content
// AVANT file, edit new avant old. Le modèle écrit ses arguments dans l'ordre
// du schéma — il déroulait donc tout le contenu d'un fichier avant d'en donner
// le chemin. Le chemin d'abord permet de refuser en plein flux une écriture
// interdite (fichier non lu) au lieu d'attendre la fin, et à l'interface de
// nommer le fichier dès le début de la frappe (repris d'OpenFox, 2.0.154).
type orderedProps []propEntry

type propEntry struct {
	Key    string
	Schema map[string]any
}

func (o orderedProps) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, p := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := json.Marshal(p.Key)
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(p.Schema)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// preflightRefusal : write/edit refusé pendant le flux, dès que son chemin est
// connu (voir runChat). msg vide = chemin vérifié et accepté.
type preflightRefusal struct {
	name, id, file, msg string
}
