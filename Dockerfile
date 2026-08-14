# ── Loki — image GPU autonome (fork d'AJEAN, https://github.com/nathaninline/ajean)
#
# Trois étapes :
#   1. compilation de llama.cpp avec CUDA (mêmes flags que backend_build.go,
#      sauf GGML_NATIVE=OFF : l'image est bâtie sur un runner GitHub, pas sur
#      la machine qui l'exécutera — des instructions CPU « natives » du runner
#      provoqueraient un Illegal instruction ailleurs) ;
#   2. compilation du binaire Go `loki` (UI embarquée via go:embed) ;
#   3. runtime CUDA léger : llama-server + loki + entrypoint.
#
# Pré-requis d'exécution : NVIDIA Container Toolkit sur l'hôte.
#   docker build -t loki --build-arg CUDA_ARCHS=86 .
# CUDA_ARCHS : 75=Turing, 86=Ampere, 89=Ada — restreindre à sa carte divise
# le temps de build et la taille de l'image.

# ── Étape 1 : llama.cpp CUDA ────────────────────────────────────────────
FROM nvidia/cuda:12.6.3-devel-ubuntu24.04 AS llamacpp

RUN apt-get update && apt-get install -y --no-install-recommends \
        git cmake build-essential ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Épingler LLAMACPP_REF sur un tag (ex. b4991) pour des builds reproductibles.
ARG LLAMACPP_REF=master
ARG CUDA_ARCHS=75;86;89

RUN git clone --depth 1 --branch "${LLAMACPP_REF}" \
        https://github.com/ggml-org/llama.cpp /src/llama.cpp

RUN cmake -S /src/llama.cpp -B /src/llama.cpp/build \
        -DCMAKE_BUILD_TYPE=Release \
        -DGGML_CUDA=ON \
        -DGGML_CUDA_F16=ON \
        -DGGML_NATIVE=OFF \
        -DCMAKE_CUDA_ARCHITECTURES="${CUDA_ARCHS}" \
        -DLLAMA_CURL=OFF \
        -DLLAMA_BUILD_TESTS=OFF \
        -DLLAMA_BUILD_EXAMPLES=OFF \
        -DLLAMA_BUILD_UI=OFF \
        -DLLAMA_USE_PREBUILT_UI=OFF \
    && cmake --build /src/llama.cpp/build --target llama-server -j"$(nproc)" \
    && mkdir -p /opt/llama.cpp \
    && find /src/llama.cpp/build -name 'llama-server' -type f -exec cp {} /opt/llama.cpp/ \; \
    && find /src/llama.cpp/build -name '*.so*' -exec cp -P {} /opt/llama.cpp/ \;

# ── Étape 2 : binaire Go loki ───────────────────────────────────────────
FROM golang:1.25 AS gobuild
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY tools/ tools/
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/loki ./cmd/loki

# ── Étape 3 : runtime ───────────────────────────────────────────────────
FROM nvidia/cuda:12.6.3-runtime-ubuntu24.04 AS runtime

# Marqueur de build (sha court), injecté par le workflow GitHub.
ARG LOKI_VERSION=dev
LABEL org.opencontainers.image.revision="${LOKI_VERSION}" \
      org.opencontainers.image.source="https://github.com/R0m1k3/Loki" \
      org.opencontainers.image.description="Loki — fork conteneurisé d'AJEAN (github.com/nathaninline/ajean, MIT)"

# curl pour le HEALTHCHECK ; git pour les outils de l'agent ; nodejs/npm pour
# les serveurs MCP lancés via npx ; libgomp1 pour llama-server ; tini en PID 1.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl git libgomp1 tini nodejs npm \
    && rm -rf /var/lib/apt/lists/*

COPY --from=llamacpp /opt/llama.cpp /opt/llama.cpp
COPY --from=gobuild /out/loki /usr/local/bin/loki
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh && mkdir -p /data /models

ENV LOKI_CONTAINER=1 \
    LOKI_HOME=/data \
    LOKI_MODEL_DIRS=/models \
    LD_LIBRARY_PATH=/opt/llama.cpp \
    NVIDIA_VISIBLE_DEVICES=all \
    NVIDIA_DRIVER_CAPABILITIES=compute,utility

# 8090 : interface web (seule à exposer). Le moteur (8080) reste interne au
# conteneur — il n'est pas authentifié par défaut.
EXPOSE 8090

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD curl -fsS "http://localhost:${LOKI_WEB_PORT:-8090}/" >/dev/null || exit 1

WORKDIR /data
ENTRYPOINT ["tini", "--", "docker-entrypoint.sh"]
