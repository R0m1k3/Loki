#!/bin/sh
# Entrypoint du conteneur Loki : sème la configuration au premier démarrage,
# lance le moteur (supervision PID, sans systemd), puis l'UI au premier plan.
set -eu

: "${LOKI_HOME:=/data}"
mkdir -p "$LOKI_HOME"

# Le chemin du moteur est imposé par l'image (llama-server précompilé de
# l'image officielle llama.cpp, dans /app) : on le (re)pose à chaque boot,
# une mise à jour de l'image ne doit pas laisser un BIN obsolète en base.
loki config set "BIN=${LOKI_ENGINE_BIN:-/app/llama-server}"

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

exec loki web "${LOKI_WEB_PORT:-8090}"
