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

# ── Étape 2 : runtime sur l'image serveur CUDA officielle ───────────────
FROM ${LLAMACPP_IMAGE} AS runtime

# Marqueur de build (sha court), injecté par le workflow GitHub.
ARG LOKI_VERSION=dev
LABEL org.opencontainers.image.revision="${LOKI_VERSION}" \
      org.opencontainers.image.source="https://github.com/R0m1k3/Loki" \
      org.opencontainers.image.description="Loki — fork conteneurisé d'AJEAN (github.com/nathaninline/ajean, MIT)"

# git pour les outils de l'agent ; nodejs/npm pour les serveurs MCP lancés
# via npx ; tini en PID 1. curl et libgomp1 sont déjà dans l'image amont.
RUN apt-get update && apt-get install -y --no-install-recommends \
        git nodejs npm tini \
    && rm -rf /var/lib/apt/lists/*

COPY --from=gobuild /out/loki /usr/local/bin/loki
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh && mkdir -p /data /models

# llama-server et ses .so vivent dans /app (convention de l'image amont).
ENV LOKI_CONTAINER=1 \
    LOKI_HOME=/data \
    LOKI_MODEL_DIRS=/models \
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
