package loki

// chat_vision_tool.go — l'outil see_image : le modèle charge lui-même une image
// du disque dans sa VISION, sans que l'utilisateur ait à la joindre au message.
//
// Loki savait déjà voir deux choses : une pièce jointe (web_upload.go) et une
// capture d'écran qu'il venait de prendre (chat_screenshot.go). Pas un fichier
// qui dort sur le disque. « Regarde la capture dans ~/photos/bug.png » n'avait
// donc aucune réponse : `read` rend des octets binaires, et le modèle finissait
// par décrire ce qu'il croyait deviner du nom de fichier.
//
// Mécanique : le résultat de l'outil reste un simple texte (un accusé). L'image,
// elle, est réinjectée juste après dans un message utilisateur multimodal
// (image_url) — le SEUL format que llama-server comprenne une fois --mmproj
// chargé, et exactement le chemin déjà emprunté par les pièces jointes et les
// captures.

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
)

// maxVisionBytes borne la taille d'une image chargée dans la vision. Au-delà,
// le base64 gonfle le contexte pour rien : le moteur redimensionne de toute
// façon avant l'encodeur d'images.
const maxVisionBytes = 12 << 20 // 12 Mio

// toolSeeImage lit un fichier image et renvoie (accusé texte, partie image_url).
// La partie image vaut nil en cas d'erreur : l'appelant n'injecte alors rien.
//
// Chaque refus dit CE QUI manque plutôt que « impossible » : sans projecteur, la
// réponse n'est pas la même que sur un poste distant ou un fichier trop lourd,
// et un modèle qui reçoit un motif clair peut corriger son geste lui-même.
func toolSeeImage(path string) (string, map[string]any) {
	if !visionEnabled() {
		return "[erreur] la vision n'est pas active sur ce preset (aucun projecteur MMPROJ configuré) — impossible de voir une image", nil
	}
	// Cible = un poste distant : le fichier est LÀ-BAS, pas lisible d'ici.
	if agentTargetSlug() != "" {
		return "[erreur] voir une image n'est pas possible sur un poste distant (le fichier est sur l'autre machine)", nil
	}
	if path == "" {
		return "[erreur] chemin de fichier manquant", nil
	}
	abs := resolveAgentPath(path)
	mime := imageMime(abs)
	if mime == "" {
		return "[erreur] format non reconnu comme image (attendu : png, jpg, gif, webp, bmp) : " + path, nil
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "[erreur] fichier introuvable : " + path, nil
	}
	if st.IsDir() {
		return "[erreur] c'est un dossier, pas une image : " + path, nil
	}
	if st.Size() > maxVisionBytes {
		return fmt.Sprintf("[erreur] image trop lourde (%s, max %s) : %s",
			humanBytes(st.Size()), humanBytes(maxVisionBytes), path), nil
	}
	// Sonde du moteur EN DERNIER, juste avant le travail coûteux. Elle fait un
	// appel réseau (/props, mis en cache 10 s) : la poser d'entrée, c'était payer
	// un aller-retour pour répondre « chemin manquant », et surtout masquer les
	// motifs précis ci-dessus derrière un « le moteur ne voit pas » qui n'apprend
	// rien quand le vrai problème est une faute de frappe dans le chemin.
	//
	// Elle reste indispensable : un projecteur déclaré ne garantit pas que le
	// moteur en service sache traiter une image (mauvais couple modèle/mmproj,
	// moteur trop ancien). Autant le dire que d'envoyer 12 Mio de base64 que
	// personne ne regardera.
	if !engineSeesImages() {
		return "[erreur] le moteur en service ne traite pas les images (projecteur non chargé ?) — impossible de voir " + path, nil
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return "[erreur] lecture impossible : " + err.Error(), nil
	}
	if len(b) == 0 {
		return "[erreur] fichier vide : " + path, nil
	}
	// Même préparation que les pièces jointes : orientation EXIF redressée et
	// grand côté ramené sous maxImageDim (voir web_upload_orient.go).
	b, mime = prepareImageForModel(b, mime)
	return "[ok] image chargée : " + filepath.Base(abs), imageURLPart(b, mime)
}

// imageURLPart construit la partie multimodale {type:image_url, image_url:{url}}
// à partir d'octets image déjà préparés — le SEUL format qu'un llama-server
// --mmproj comprend. Partagé par see_image, web_screenshot et browser_screenshot.
func imageURLPart(b []byte, mime string) map[string]any {
	return map[string]any{
		"type": "image_url",
		"image_url": map[string]any{
			"url": "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b),
		},
	}
}

// seeImageMessage emballe la partie image dans le message utilisateur qui la
// porte jusqu'au modèle. La légende nomme le fichier : après compaction il ne
// restera qu'elle et imageLostMarker, et « Image demandée : » tout court ne
// dirait pas LAQUELLE rouvrir.
func seeImageMessage(label string, img map[string]any) Message {
	// Sans parenthèses vides : browser_screenshot n'a pas d'argument (label=""),
	// là où see_image porte le chemin du fichier.
	text := "Image :"
	if label != "" {
		text = "Image demandée (" + label + ") :"
	}
	return Message{Role: "user", Content: []map[string]any{
		{"type": "text", "text": text},
		img,
	}}
}

// seeImageTool — schéma envoyé au modèle. Description tenue au plus court : les
// schémas partent dans CHAQUE requête et le préambule a un budget
// (TestSystemPromptStaysLean).
func seeImageTool() Tool {
	return Tool{Type: "function", Function: ToolFunction{
		Name:        "see_image",
		Description: "Ouvre un fichier image du disque pour le VOIR (png, jpg, gif, webp, bmp).",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file": map[string]any{"type": "string", "description": "Chemin de l'image (relatif au dossier de travail, ou absolu)"},
			},
			"required": []string{"file"},
		},
	}}
}
