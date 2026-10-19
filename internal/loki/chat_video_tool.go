package loki

// chat_video_tool.go — l'outil see_video (repris d'AJEAN) : le modèle charge
// lui-même un fichier vidéo du disque dans sa VISION, sans que l'utilisateur ait
// à le joindre. Même mécanique que see_image (chat_vision_tool.go) : le résultat
// de l'outil reste un accusé texte, la vidéo part juste après dans un message
// utilisateur multimodal (partie input_video), que llama-server décode en
// images avec ffmpeg. Toujours ÉPHÉMÈRE : jamais dans l'historique.
//
// Annoncé seulement quand le moteur sait lire la vidéo (videoInputSupported) ;
// appelé quand même, il le dit avec la marche à suivre (ffmpeg puis see_image).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// toolSeeVideo lit un fichier vidéo et renvoie (accusé texte, partie input_video).
// La partie vaut nil en cas d'erreur : l'appelant n'injecte alors rien. Même
// ordre de refus que toolSeeImage, la sonde du moteur en dernier.
func toolSeeVideo(path string) (string, map[string]any) {
	if !visionEnabled() {
		return "[erreur] la vision n'est pas active sur ce preset (aucun projecteur MMPROJ configuré) — impossible de voir une vidéo", nil
	}
	if agentTargetSlug() != "" {
		return "[erreur] voir une vidéo n'est pas possible sur un poste distant (le fichier est sur l'autre machine)", nil
	}
	if path == "" {
		return "[erreur] chemin de fichier manquant", nil
	}
	if !videoInputSupported() {
		return "[erreur] " + strings.TrimSuffix(videoFallbackNote, "\n"), nil
	}
	abs := resolveAgentPath(path)
	mime := videoMime(abs)
	if mime == "" {
		return "[erreur] format non reconnu comme vidéo (attendu : mp4, webm, mov, mkv, avi) : " + path, nil
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "[erreur] fichier introuvable : " + path, nil
	}
	if st.IsDir() {
		return "[erreur] c'est un dossier, pas une vidéo : " + path, nil
	}
	if st.Size() > maxVideoBytes {
		return fmt.Sprintf("[erreur] vidéo trop lourde (%s, max %s) — coupe-la en un clip plus court : %s",
			humanBytes(st.Size()), humanBytes(maxVideoBytes), path), nil
	}
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
	return "[ok] vidéo chargée : " + filepath.Base(abs), videoPart(b, mime)
}

// seeVideoMessage emballe la partie vidéo dans le message utilisateur qui la
// porte jusqu'au modèle ; la légende nomme le fichier (voir seeImageMessage).
func seeVideoMessage(label string, vid map[string]any) Message {
	text := "Vidéo :"
	if label != "" {
		text = "Vidéo demandée (" + label + ") :"
	}
	return Message{Role: "user", Content: []map[string]any{
		{"type": "text", "text": text},
		vid,
	}}
}

// seeVideoTool — schéma envoyé au modèle, tenu court comme seeImageTool.
func seeVideoTool() Tool {
	return Tool{Type: "function", Function: ToolFunction{
		Name:        "see_video",
		Description: "Ouvre un fichier vidéo du disque pour la REGARDER (mp4, webm, mov, mkv, avi). Clips courts.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file": map[string]any{"type": "string", "description": "Chemin de la vidéo (relatif au dossier de travail, ou absolu)"},
			},
			"required": []string{"file"},
		},
	}}
}
