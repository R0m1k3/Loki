package loki

// chat_video.go — vidéos vues par le modèle (repris d'AJEAN, PR #100 et son
// correctif « vidéo envoyée seulement si le moteur sait la lire »).
//
// Trois morceaux :
//   - videoInputSupported : le moteur actif sait-il lire une partie
//     `input_video` ? Seul un llama.cpp local récent le fait (option
//     --video-fps, décodage par ffmpeg). Une API externe ou Strata ignorent la
//     partie SANS erreur : le modèle ne recevait que le texte et cherchait à voir
//     la vidéo avec see_image. Ailleurs, la vidéo reste un simple fichier, avec
//     la marche à suivre (videoFallbackNote) ;
//   - une vidéo JOINTE au message est rangée PAR RÉFÉRENCE, comme les images
//     (chat_images.go) : une fois sur disque sous LOKI_HOME/chatvid/, chiffrée
//     comme le fil, et l'historique n'en garde que « loki-vid:<nom> ».
//     expandVideoRefs remet les octets juste avant l'envoi au moteur ;
//   - la vidéo chargée par l'outil see_video (chat_video_tool.go) est, elle,
//     TOUJOURS éphémère : elle part dans le tour en cours et jamais dans
//     l'historique, KEEP_TURN_IMAGES compris (des milliers de jetons visuels).
//
// Écart avec l'amont : comme les images, un fil rechargé perd ses vidéos
// (stripImageParts, avec videoLostMarker) et pruneChatImages vide aussi
// chatvid/ — Loki ne rejoue jamais un média d'un fil rouvert.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const vidRefScheme = "loki-vid:"

func chatVidDir() string { return filepath.Join(LokiHome(), "chatvid") }

// maxVideoBytes borne la taille d'une vidéo montrée au modèle : le moteur en
// tire des images à --video-fps (4 par seconde par défaut), donc une vidéo
// longue = des dizaines de milliers de jetons visuels. Au-delà, le contexte
// sature sans que le modèle puisse raisonner dessus.
const maxVideoBytes = 200 << 20 // 200 Mio

// videoMimes : les extensions qu'on peut ENVOYER AU MODÈLE comme vidéo (partie
// input_video). Le moteur décode lui-même avec ffmpeg ; un format hors de cette
// liste reste un simple fichier du dossier de travail.
var videoMimes = map[string]string{
	".mp4":  "video/mp4",
	".webm": "video/webm",
	".mov":  "video/quicktime",
	".mkv":  "video/x-matroska",
	".avi":  "video/x-msvideo",
}

// videoMime renvoie le type MIME vidéo d'un nom de fichier, ou "" si ce n'est
// pas une vidéo qu'on sait montrer au modèle.
func videoMime(name string) string {
	return videoMimes[strings.ToLower(filepath.Ext(name))]
}

// videoFallbackNote : consigne quand le moteur ne lit pas la vidéo. Le fichier
// reste utilisable : quelques images extraites avec ffmpeg, puis see_image.
// ffmpeg est dans l'image Docker de Loki (l'image serveur officielle de
// llama.cpp l'installe pour son décodage vidéo) ; une installation hors
// conteneur peut ne pas l'avoir, d'où la dernière phrase.
const videoFallbackNote = "Le moteur actif ne lit pas la vidéo directement. Pour en voir le contenu, extrais quelques images avec ffmpeg (par exemple `ffmpeg -i <vidéo> -vf fps=1 frame_%03d.jpg`) puis regarde-les avec see_image ; si ffmpeg est absent, dis-le plutôt que de deviner le contenu.\n"

// videoLostMarker : équivalent d'imageLostMarker pour une partie input_video
// (résumé de compaction, fil rechargé).
const videoLostMarker = " [video — not kept in the conversation past this point; call see_video on the file again if you still need to watch it]"

// videoPart construit la partie multimodale {type:input_video, input_video:{url}}
// — le format que llama-server lit une fois --mmproj chargé.
func videoPart(b []byte, mime string) map[string]any {
	return map[string]any{
		"type": "input_video",
		"input_video": map[string]any{
			"url": "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b),
		},
	}
}

// videoInputSupported : le moteur actif sait-il lire une partie input_video ?
// Preset local avec projecteur (MMPROJ) ET binaire dont l'aide annonce
// --video-fps. Variable : remplaçable dans les tests.
var videoInputSupported = func() bool {
	cfg := ReadConfig()
	if isExternalConfig(cfg) || isStrataConfig(cfg) || strings.TrimSpace(cfg["MMPROJ"]) == "" {
		return false
	}
	bin := strings.TrimSpace(cfg["BIN"])
	if bin == "" {
		return false
	}
	if !filepath.IsAbs(bin) {
		bin = filepath.Join(LokiHome(), bin)
	}
	return binHelpHasVideo(prebuiltResolveBin(bin))
}

var videoCapCache sync.Map // "binaire\x00mtime" → bool

// binHelpHasVideo lit l'aide du binaire, une fois par binaire ET par date de
// modification (un moteur mis à jour en place est relu). Volontairement à part
// de binHelp : celle-ci garde aussi une aide VIDE pour toujours, et l'appeler
// avant le lancement du moteur (chemin des bibliothèques pas encore posé)
// figerait « aucune capacité » pour le vrai lancement. Ici, le chemin des
// bibliothèques est passé à la seule commande (libraryPathEnv).
func binHelpHasVideo(bin string) bool {
	st, err := os.Stat(bin)
	if err != nil || st.IsDir() {
		return false
	}
	key := bin + "\x00" + st.ModTime().String()
	if v, ok := videoCapCache.Load(key); ok {
		return v.(bool)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--help")
	cmd.Env = libraryPathEnv(filepath.Dir(bin))
	out, _ := cmd.CombinedOutput()
	ok := strings.Contains(string(out), "--video-fps")
	videoCapCache.Store(key, ok)
	return ok
}

// --- Vidéos jointes, par référence -------------------------------------------

var vidExtByMime = map[string]string{
	"video/mp4":        ".mp4",
	"video/webm":       ".webm",
	"video/quicktime":  ".mov",
	"video/x-matroska": ".mkv",
	"video/x-msvideo":  ".avi",
}

// storeChatVideo range les octets d'une vidéo et renvoie sa référence. Chiffrée
// comme le fil quand la mémoire l'est ; verrouillée : refus, la vidéo reste en
// base64 dans le message (rien ne se perd).
func storeChatVideo(b []byte, mime string) (string, error) {
	ext := vidExtByMime[mime]
	if ext == "" {
		ext = ".vid"
	}
	sum := sha256.Sum256(b)
	name := hex.EncodeToString(sum[:16]) + ext
	dir := chatVidDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err != nil {
		enc, err := encodeMemContent(b)
		if err != nil {
			return "", err
		}
		tmp := p + ".tmp"
		if err := os.WriteFile(tmp, enc, 0o600); err != nil {
			return "", err
		}
		if err := os.Rename(tmp, p); err != nil {
			return "", err
		}
	}
	return vidRefScheme + name, nil
}

// Cache des data-URL reconstituées (même raison que imgCache : un tour d'agent
// renvoie tout l'historique à chaque étape). Borné en taille.
var (
	vidCacheMu    sync.Mutex
	vidCache      = map[string]string{}
	vidCacheOrder []string
	vidCacheBytes int
)

const vidCacheMax = 64 << 20

func chatVideoDataURL(ref string) (string, bool) {
	name := strings.TrimPrefix(ref, vidRefScheme)
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", false
	}
	vidCacheMu.Lock()
	if u, ok := vidCache[name]; ok {
		vidCacheMu.Unlock()
		return u, true
	}
	vidCacheMu.Unlock()
	raw, err := os.ReadFile(filepath.Join(chatVidDir(), name))
	if err != nil {
		return "", false
	}
	b, err := decodeMemContent(raw)
	if err != nil {
		return "", false
	}
	mime := "video/mp4"
	for m, e := range vidExtByMime {
		if strings.HasSuffix(name, e) {
			mime = m
		}
	}
	u := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
	vidCacheMu.Lock()
	if _, ok := vidCache[name]; !ok {
		vidCache[name] = u
		vidCacheOrder = append(vidCacheOrder, name)
		vidCacheBytes += len(u)
		for vidCacheBytes > vidCacheMax && len(vidCacheOrder) > 1 {
			old := vidCacheOrder[0]
			vidCacheOrder = vidCacheOrder[1:]
			vidCacheBytes -= len(vidCache[old])
			delete(vidCache, old)
		}
	}
	vidCacheMu.Unlock()
	return u, true
}

// partVideoURL : l'adresse d'une partie input_video (url, sinon data — les deux
// formes que lit llama-server), "" pour toute autre partie.
func partVideoURL(p map[string]any) string {
	if p["type"] != "input_video" {
		return ""
	}
	iv, _ := p["input_video"].(map[string]any)
	if iv == nil {
		return ""
	}
	u, _ := iv["url"].(string)
	if u == "" {
		u, _ = iv["data"].(string)
	}
	return u
}

// refVideosInMessages remplace les vidéos en base64 des messages par des
// références. Comme refImagesInMessages, les parties modifiées sont RECOPIÉES,
// jamais mutées (persist peut tourner pendant qu'un runChat sérialise ces mêmes
// maps). Renvoie true si quelque chose a changé.
func refVideosInMessages(msgs []Message) bool {
	changed := false
	for mi, m := range msgs {
		parts := contentParts(m.Content)
		var np []map[string]any
		for pi, p := range parts {
			u := partVideoURL(p)
			if !strings.HasPrefix(u, "data:video/") {
				continue
			}
			i := strings.Index(u, ";base64,")
			if i < 0 {
				continue
			}
			b, err := base64.StdEncoding.DecodeString(u[i+8:])
			if err != nil {
				continue
			}
			ref, err := storeChatVideo(b, u[5:i])
			if err != nil {
				continue
			}
			if np == nil {
				np = append([]map[string]any(nil), parts...)
			}
			np[pi] = map[string]any{"type": "input_video", "input_video": map[string]any{"url": ref}}
		}
		if np != nil {
			msgs[mi].Content = np
			changed = true
		}
	}
	return changed
}

// hasInlineVideos : au moins une vidéo encore en base64 dans ces messages ?
func hasInlineVideos(msgs []Message) bool {
	for _, m := range msgs {
		for _, p := range contentParts(m.Content) {
			if strings.HasPrefix(partVideoURL(p), "data:video/") {
				return true
			}
		}
	}
	return false
}

// expandVideoRefs renvoie une COPIE des messages prête pour le modèle : chaque
// référence de vidéo redevient sa data-URL. Les messages sans référence sont
// partagés tels quels. Une vidéo introuvable devient une mention textuelle.
func expandVideoRefs(msgs []Message) []Message {
	out := msgs
	copied := false
	for i, m := range msgs {
		parts := contentParts(m.Content)
		has := false
		for _, p := range parts {
			if strings.HasPrefix(partVideoURL(p), vidRefScheme) {
				has = true
				break
			}
		}
		if !has {
			continue
		}
		if !copied {
			out = append([]Message(nil), msgs...)
			copied = true
		}
		np := make([]map[string]any, 0, len(parts))
		for _, p := range parts {
			u := partVideoURL(p)
			if !strings.HasPrefix(u, vidRefScheme) {
				np = append(np, p)
				continue
			}
			if d, ok := chatVideoDataURL(u); ok {
				np = append(np, map[string]any{"type": "input_video", "input_video": map[string]any{"url": d}})
			} else {
				np = append(np, map[string]any{"type": "text", "text": "[vidéo indisponible : fichier supprimé]"})
			}
		}
		m.Content = np
		out[i] = m
	}
	return out
}
