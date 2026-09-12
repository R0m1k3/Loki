package loki

// backend_external.go — presets « externes » : au lieu de lancer un llama-server
// local, un tel preset route le chat vers une API OpenAI-compatible distante
// (OpenAI, Groq, OpenRouter, un autre Loki, un vLLM sur une autre machine…).
// Un preset externe se reconnaît à sa clé EXTERNAL=1 ; il porte l'URL, le nom du
// modèle et la clé d'accès.
//
// Choix d'archi, repris d'AJEAN : l'externe est un PRESET COMME UN AUTRE, un
// fichier .env dans presetsDir. Toute la mécanique existante s'applique sans
// rien changer — liste, nom d'affichage, empreinte d'activité, bascule, prompt
// système par preset, réglages d'échantillonnage. La différence ne vit qu'à
// DEUX endroits : au moment de l'inférence (resolveChatEndpoint) et au moment
// de la bascule (aucun moteur à redémarrer).
//
// Ce qui ne s'applique pas, et c'est voulu : un preset externe n'a ni MODEL, ni
// NGL, ni moteur. La machine distante décide de tout ça. CTX, en revanche,
// reste utile — c'est lui qui pilote la jauge de contexte et le seuil de
// compaction côté Loki.

import (
	"fmt"
	"strings"
)

const (
	extKeyFlag  = "EXTERNAL"       // marqueur : "1" = preset externe
	extKeyURL   = "EXTERNAL_URL"   // base ou URL complète des complétions
	extKeyModel = "EXTERNAL_MODEL" // nom du modèle envoyé dans le payload
	extKeyToken = "EXTERNAL_KEY"   // clé API (Bearer), peut être vide
)

// chatEndpoint : où partent les appels /v1/chat/completions de ce tour.
type chatEndpoint struct {
	URL      string // URL complète des complétions
	Model    string // nom de modèle envoyé dans le payload
	Key      string // Bearer ("" = aucun)
	External bool   // true = API distante (pas le llama-server local)
}

// isExternalConfig indique si une configuration décrit un endpoint externe.
func isExternalConfig(cfg map[string]string) bool {
	return strings.TrimSpace(cfg[extKeyFlag]) == "1"
}

// externalActive : le preset actif est-il un endpoint externe ?
func externalActive() bool { return isExternalConfig(ReadConfig()) }

// completionsURL normalise l'URL saisie en une URL de complétions complète.
// Accepte les trois formes qu'on colle en pratique :
//
//   - une URL déjà complète (…/chat/completions) — laissée telle quelle ;
//   - une base OpenAI (…/v1, …/openai/v1, …/api/v1) — on ajoute /chat/completions ;
//   - autre chose — on suppose une racine et on ajoute /v1/chat/completions.
//
// Exiger la forme exacte serait une source d'échecs muets : la page d'un
// fournisseur donne tantôt « https://api.groq.com/openai/v1 », tantôt l'URL
// complète, et se tromper ne produit qu'un 404 illisible.
func completionsURL(raw string) string {
	u := strings.TrimRight(strings.TrimSpace(raw), "/")
	if u == "" {
		return ""
	}
	if strings.HasSuffix(u, "/chat/completions") {
		return u
	}
	if strings.HasSuffix(u, "/v1") {
		return u + "/chat/completions"
	}
	return u + "/v1/chat/completions"
}

// resolveChatEndpoint décide où envoyer les complétions : l'API externe si le
// preset actif en est un, sinon le llama-server local.
func resolveChatEndpoint() chatEndpoint {
	cfg := ReadConfig()
	if isExternalConfig(cfg) {
		return chatEndpoint{
			URL:      completionsURL(cfg[extKeyURL]),
			Model:    strings.TrimSpace(cfg[extKeyModel]),
			Key:      strings.TrimSpace(cfg[extKeyToken]),
			External: true,
		}
	}
	return chatEndpoint{
		URL:   fmt.Sprintf("http://localhost:%d/v1/chat/completions", LLMPort()),
		Model: "loki",
		Key:   readAPIKey(),
	}
}

// auth pose l'en-tête d'autorisation de CET endpoint. Indispensable de le tenir
// ici plutôt que d'appeler authHeader : celui-ci envoie la clé du serveur LOCAL,
// et l'expédier à api.openai.com serait fuiter un secret chez un tiers en même
// temps qu'un 401 garanti.
func (e chatEndpoint) auth(set func(k, v string)) {
	if e.Key != "" {
		set("Authorization", "Bearer "+e.Key)
	}
}

// externalPresetContent construit le corps .env d'un preset externe (hors ligne
// « # NAME= », ajoutée par SavePreset). Une clé vide n'est pas écrite. `ctx` va
// dans la clé CTX standard — elle pilote la jauge de contexte et le seuil de
// compaction, exactement comme pour un preset local.
func externalPresetContent(url, model, key, ctx string) string {
	m := map[string]string{
		extKeyFlag:  "1",
		extKeyURL:   strings.TrimSpace(url),
		extKeyModel: strings.TrimSpace(model),
	}
	if k := strings.TrimSpace(key); k != "" {
		m[extKeyToken] = k
	}
	if c := strings.TrimSpace(ctx); c != "" {
		m["CTX"] = c
	}
	return formatEnv(m)
}
