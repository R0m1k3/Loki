# ── Loki — image GPU (fork d'AJEAN, https://github.com/nathaninline/ajean)
#
# Deux étapes seulement :
#   1. compilation du binaire Go `loki` (UI embarquée via go:embed) ;
#   2. runtime = l'image serveur CUDA OFFICIELLE de llama.cpp — llama-server
#      y est précompilé et maintenu par l'équipe amont (base nvidia/cuda
#      runtime, backends .so dans /app, architectures GPU courantes).
#      Aucune compilation CUDA ici : le build complet prend quelques minutes.
#
# Épingler une version : --build-arg LLAMACPP_IMAGE=ghcr.io/ggml-org/llama.cpp:server-cuda-b10423
# Variante CPU (test sans GPU) : --build-arg LLAMACPP_IMAGE=ghcr.io/ggml-org/llama.cpp:server
# Pré-requis d'exécution GPU : NVIDIA Container Toolkit sur l'hôte.

# Déclaré avant le premier FROM : requis pour être utilisable dans un FROM.
ARG LLAMACPP_IMAGE=ghcr.io/ggml-org/llama.cpp:server-cuda

# ── Étape 1 : binaire Go loki ───────────────────────────────────────────
FROM golang:1.25 AS gobuild
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY tools/ tools/
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/loki ./cmd/loki
# gopls : serveur LSP Go, consommé par le vérificateur de code du mode code
# (lsp.go). Compilé ici parce que l'image finale n'a pas de toolchain Go.
# GOTOOLCHAIN=auto : gopls@latest exige régulièrement un Go plus récent que
# celui de l'image (v0.23 veut ≥ 1.26 quand l'image est en 1.25) ; en local,
# l'installation échouait net. Auto télécharge le toolchain requis — coût
# limité à cette étape de build.
RUN GOBIN=/out GOTOOLCHAIN=auto go install golang.org/x/tools/gopls@latest

# ── Étape 2 : runtime sur l'image serveur CUDA officielle ───────────────
FROM ${LLAMACPP_IMAGE} AS runtime

# Marqueur de build (sha court), injecté par le workflow GitHub.
ARG LOKI_VERSION=dev
LABEL org.opencontainers.image.revision="${LOKI_VERSION}" \
      org.opencontainers.image.source="https://github.com/R0m1k3/Loki" \
      org.opencontainers.image.description="Loki — fork conteneurisé d'AJEAN (github.com/nathaninline/ajean, MIT)"

# git pour les outils de l'agent ; nodejs/npm pour les serveurs MCP lancés
# via npx ; tini en PID 1. curl et libgomp1 sont déjà dans l'image amont.
# Node 22 depuis NodeSource et non depuis Ubuntu (qui livre Node 18) : Playwright
# et la plupart des serveurs MCP réclament Node ≥ 20.
RUN apt-get update && apt-get install -y --no-install-recommends \
        git tini ca-certificates curl gnupg \
    && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# Serveurs LSP du mode code (lsp.go) : TypeScript/JavaScript et Python via
# npm, Go via gopls compilé à l'étape 1. Un serveur absent désactive juste son
# langage — mais autant livrer l'image complète.
RUN npm install -g --no-audit --no-fund typescript typescript-language-server pyright     && npm cache clean --force

# uv/uvx : lanceur des serveurs MCP écrits en Python (mcp-server-git, -fetch,
# -time, sqlite, docker…). Sans lui, une bonne partie du catalogue embarqué
# s'affiche mais ne peut pas démarrer. Deux binaires statiques, ~35 Mo.
COPY --from=ghcr.io/astral-sh/uv:latest /uv /uvx /usr/local/bin/

# Playwright + Chromium : l'outil web_screenshot capture une page RENDUE
# (JavaScript exécuté), ce que le moteur web intégré ne sait pas faire.
# Coût assumé : ~500 Mo à 1 Go d'image. Mettre PLAYWRIGHT=0 pour bâtir une image
# légère — l'outil se désactive alors tout seul (le binaire absent est détecté).
ARG PLAYWRIGHT=1
ARG PLAYWRIGHT_VERSION=latest
ENV PLAYWRIGHT_BROWSERS_PATH=/opt/pw-browsers
# Les polices sont INDISPENSABLES : sans elles, Chromium rend les pages sans
# AUCUN texte — boutons et titres sortent vides, seuls les images et les aplats
# apparaissent. Noto couvre l'essentiel des écritures, Liberation fournit les
# substituts d'Arial/Times que réclament la plupart des sites.
RUN if [ "$PLAYWRIGHT" = "1" ]; then \
        npm i -g playwright@${PLAYWRIGHT_VERSION} \
        && playwright install --with-deps chromium \
        && apt-get update \
        && apt-get install -y --no-install-recommends \
             fonts-liberation fonts-dejavu-core fonts-noto-core \
             fonts-noto-cjk fonts-noto-color-emoji \
        && fc-cache -f \
        && rm -rf /root/.npm /var/lib/apt/lists/* ; \
    fi

COPY --from=gobuild /out/loki /usr/local/bin/loki
COPY --from=gobuild /out/gopls /usr/local/bin/gopls
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh && mkdir -p /data /models

# llama-server et ses .so vivent dans /app (convention de l'image amont).
ENV LOKI_CONTAINER=1 \
    LOKI_HOME=/data \
    LOKI_MODEL_DIRS=/models \
    LOKI_ENGINE_BIN=/app/llama-server \
    LD_LIBRARY_PATH=/app \
    NVIDIA_VISIBLE_DEVICES=all \
    NVIDIA_DRIVER_CAPABILITIES=compute,utility

# 8090 : interface web (seule à exposer). Le moteur (8080) reste interne au
# conteneur — il n'est pas authentifié par défaut.
EXPOSE 8090

# Remplace le HEALTHCHECK de l'image amont (qui vise le moteur sur :8080).
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD curl -fsS "http://localhost:${LOKI_WEB_PORT:-8090}/" >/dev/null || exit 1

WORKDIR /data
# Remplace l'ENTRYPOINT de l'image amont (/app/llama-server).
ENTRYPOINT ["tini", "--", "docker-entrypoint.sh"]
