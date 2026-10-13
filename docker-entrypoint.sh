#!/bin/sh
# Entrypoint du conteneur Loki : met à jour les binaires Loki, sème la
# configuration au premier démarrage, lance le moteur (supervision PID, sans
# systemd), puis l'UI au premier plan derrière la façade Rust (loki-rs).
set -eu

: "${LOKI_HOME:=/data}"
export LOKI_HOME   # les sous-commandes loki doivent voir le MEME dossier
mkdir -p "$LOKI_HOME"

# Garde-fou persistance : si /data n'est pas un point de montage, tout ce que
# l'utilisateur fera (modèles, discussions, fichiers) mourra avec le conteneur.
# On le dit au premier écran du journal — l'UI l'affiche aussi en bandeau.
if ! grep -qs " $LOKI_HOME " /proc/self/mountinfo; then
    echo "[entrypoint] ⚠️  $LOKI_HOME n'est pas un volume monté : les données seront PERDUES à la recréation du conteneur."
    echo "[entrypoint]     Ajoute « -v /chemin/hote/data:/data » (docker run) ou le mapping volumes du docker-compose."
fi

# ── Mise à jour au démarrage ───────────────────────────────────────────────
# Le loki-rs DE L'IMAGE télécharge la dernière release « rolling » (binaires
# vérifiés par SHA-256) dans $LOKI_HOME/bin/<build>/ : un simple redémarrage
# du conteneur suffit à passer à la dernière version. C'est toujours celui de
# l'image qui s'en charge — un binaire publié défectueux ne peut donc pas
# empêcher le correctif suivant d'arriver. Jamais bloquant (réseau coupé,
# empreinte fausse : l'état précédent reste). LOKI_AUTO_UPDATE=0 le coupe.
/usr/local/bin/loki-rs self-update || true

# loki et loki-rs viennent TOUJOURS du même dossier : /data/bin/<build> s'il
# est plus récent que l'image et de même ABI, sinon l'image.
BIN_DIR=$(/usr/local/bin/loki-rs which-bin 2>/dev/null || true)
if [ -z "$BIN_DIR" ] || [ ! -x "$BIN_DIR/loki" ] || [ ! -x "$BIN_DIR/loki-rs" ]; then
    BIN_DIR=/usr/local/bin
fi
echo "[entrypoint] binaires Loki : $BIN_DIR"
# Tous les appels « loki » ci-dessous visent le binaire choisi.
loki() { "$BIN_DIR/loki" "$@"; }
# Et `docker exec loki loki …` aussi : /usr/local/sbin passe AVANT
# /usr/local/bin dans le PATH, un lien y suffit sans toucher aux binaires de
# l'image (celui de loki-rs doit rester intact, c'est lui qui met à jour).
if [ "$BIN_DIR" != /usr/local/bin ]; then
    ln -sf "$BIN_DIR/loki" /usr/local/sbin/loki 2>/dev/null || true
else
    rm -f /usr/local/sbin/loki 2>/dev/null || true
fi

# Le chemin du moteur est imposé par l'image (llama-server précompilé de
# l'image officielle llama.cpp, dans /app) : on le (re)pose à chaque boot,
# une mise à jour de l'image ne doit pas laisser un BIN obsolète en base.
#
# SAUF si l'utilisateur a mis le moteur à jour depuis l'interface : celui-là
# vit sous $LOKI_HOME/engine/<version>/ (volume de données, il survit au
# conteneur) et c'est lui qu'il a choisi. Le reposer sur /app à chaque
# redémarrage ramenait silencieusement l'ancien moteur — un modèle qui
# chargeait la veille ne chargeait plus le lendemain, « unknown model
# architecture », sans que rien n'ait été touché. On ne le garde que s'il
# existe encore : un dossier supprimé à la main ne doit pas laisser BIN dans
# le vide.
cur=$(loki config get BIN)
case "$cur" in
    "$LOKI_HOME"/engine/*/llama-server) [ -x "$cur" ] || cur="" ;;
    *) cur="" ;;
esac
if [ -n "$cur" ]; then
    echo "[entrypoint] moteur mis à jour conservé : $cur"
else
    loki config set "BIN=${LOKI_ENGINE_BIN:-/app/llama-server}"
fi

# Les autres clés ne sont semées QUE si absentes : ce que l'utilisateur règle
# ensuite dans l'UI (modèle, contexte…) est conservé d'un redémarrage à l'autre.
seed() { # seed CLÉ VALEUR
    [ -n "$2" ] || return 0
    cur=$(loki config get "$1")
    [ -n "$cur" ] || loki config set "$1=$2"
}
seed HOST "${LOKI_ENGINE_HOST:-127.0.0.1}"
seed PORT "${LOKI_ENGINE_PORT:-8080}"
seed MODEL "${LOKI_MODEL:-}"
seed CTX "${LOKI_CTX:-}"
seed NGL "${LOKI_NGL:-}"

# Moteur : démarrage best-effort. Sans MODEL configuré, on laisse l'UI monter
# quand même — le moteur partira au premier modèle choisi/téléchargé dans l'UI.
if [ -n "$(loki config get MODEL)" ]; then
    loki start || echo "[entrypoint] moteur non démarré (voir loki logs) — l'UI reste accessible"
else
    echo "[entrypoint] aucun MODEL configuré — télécharge un modèle depuis l'UI pour démarrer le moteur"
fi

# Façade Rust (loki-rs) sur le port public ; elle lance `loki web` sur
# 127.0.0.1:${LOKI_GO_PORT:-8091} et lui relaie tout. LOKI_FACADE=0 revient à
# l'ancien schéma (Go seul, directement sur le port public) — porte de sortie
# si la façade pose problème.
export LOKI_WEB_PORT="${LOKI_WEB_PORT:-8090}"
if [ "${LOKI_FACADE:-1}" = "0" ]; then
    exec "$BIN_DIR/loki" web "$LOKI_WEB_PORT"
fi
exec "$BIN_DIR/loki-rs" serve
