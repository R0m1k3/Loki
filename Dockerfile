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

# ⚠️ AVANT le premier FROM, obligatoirement. Un ARG posé entre deux étapes
# appartient à l'étape où il apparaît ; les « ARG WHISPER_CMAKE_FLAGS » nus des
# étapes whisperbuild-* héritent du scope GLOBAL — c'est-à-dire d'ici. Placé
# plus bas, ils héritaient d'une valeur VIDE : cmake tournait sans aucun
# drapeau, ggml se construisait en bibliothèques partagées (échec de lien sur
# libcuda.so.1) et surtout en -march=native — le SIGILL de la PR #18 revenu en
# silence. Un test (TestDockerfileArgFlagsGlobal) verrouille cette position.
ARG WHISPER_CMAKE_FLAGS="-DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF \
    -DWHISPER_BUILD_TESTS=OFF -DWHISPER_BUILD_EXAMPLES=ON \
    -DGGML_NATIVE=OFF -DGGML_AVX512=OFF -DGGML_AMX_TILE=OFF \
    -DGGML_AMX_INT8=OFF -DGGML_AMX_BF16=OFF"

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

# ── Étape 1 bis : whisper-server (dictée vocale) ────────────────────────
# DEUX binaires sont construits, pas un. LLAMACPP_IMAGE accepte la variante
# CPU de l'image amont (voir ligne 11) : sur cette base, les bibliothèques
# CUDA sont absentes et un binaire lié à CUDA ne démarre pas du tout —
# l'éditeur de liens échoue avant la première instruction, donc aucun repli
# n'est possible depuis l'intérieur du programme. Loki choisit à l'exécution
# (whisperServerBin, dictate_server.go).
#
# whisper-server et non whisper-cli : le modèle reste chargé entre deux
# dictées. L'ancien chemin le relisait depuis le disque à chaque phrase, ce
# qui dominait le temps de réponse.
#
# ⚠️ GGML_NATIVE=OFF est OBLIGATOIRE sur LES DEUX cibles. Par défaut ggml
# compile en -march=native, c'est-à-dire pour le processeur DU RUNNER DE
# BUILD — un Xeon récent chez GitHub, avec AVX-512 et AMX. Le binaire partait
# alors sur une machine qui n'a pas ces instructions et mourait d'un SIGILL en
# pleine transcription : « AMX is not ready to be used! », puis plus rien,
# l'interface affichant un échec sans raison. OFF retombe sur la ligne de base
# AVX2/FMA/F16C de ggml, présente sur tout x86-64 depuis 2013.
# Ubuntu 22.04 : glibc plus ancienne que l'image runtime, donc compatible quoi
# qu'elle embarque.
FROM ubuntu:22.04 AS whisperbuild-cpu
ARG WHISPER_CMAKE_FLAGS
RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential cmake git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
# Le grep final vérifie que ggml est bien lié EN STATIQUE : l'image finale ne
# reçoit que le binaire, une .so ggml manquante ne se verrait qu'au premier
# clic sur le micro. Des libs partagées ici = drapeaux non transmis.
RUN git clone --depth 1 https://github.com/ggml-org/whisper.cpp /w \
    && cmake -S /w -B /w/build ${WHISPER_CMAKE_FLAGS} \
    && cmake --build /w/build -j"$(nproc)" --target whisper-server \
    && ( ! ldd /w/build/bin/whisper-server | grep -E "libggml|libwhisper" )

# Version CUDA : la MÊME que celle de l'image amont (llama.cpp bâtit avec
# CUDA 12.8.1 sur Ubuntu 24.04). Le binaire est lié dynamiquement à libcudart,
# fournie par LLAMACPP_IMAGE et non par cette étape.
#
# ⚠️ 12.8 est un PLANCHER, pas un détail de version. Les GPU Blackwell
# (RTX 50xx, sm_120) n'existent pas pour un nvcc 12.4 : il refuse l'archi, et
# à défaut le binaire ne tourne que par recompilation PTX au chargement, quand
# elle est possible.
#
# ⚠️ -j"$(nproc)" et JAMAIS -j nu. Avec Make, « -j » sans nombre autorise un
# parallélisme ILLIMITÉ. ggml-cuda compte ~200 fichiers d'instanciation de
# gabarits, et chaque nvcc réclame 1 à 2 Go : le runner GitHub (16 Go) était
# tué par l'OOM après avoir lancé 92 % des compilations en treize secondes,
# sans écrire la moindre ligne d'erreur. L'étape CPU y survivait — peu de
# fichiers, compilation légère — ce qui rendait le piège invisible jusqu'ici.
#
# CUDA_ARCHS borne le travail : ggml compile sinon pour TOUTES les
# architectures qu'il connaît, de Maxwell à Blackwell, et chacune multiplie le
# temps de compilation. La liste par défaut couvre Turing à Blackwell
# (RTX 20xx → 50xx) ; l'élargir pour un GPU plus ancien se fait sans toucher
# au fichier : --build-arg CUDA_ARCHS="61;75;86".
FROM nvidia/cuda:12.8.1-devel-ubuntu24.04 AS whisperbuild-cuda
ARG WHISPER_CMAKE_FLAGS
ARG CUDA_ARCHS="75;86;89;120"
RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential cmake git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
# Même vérification statique que l'étape CPU. libcuda.so.1 (le PILOTE) reste
# elle une dépendance dynamique normale : absente du runner de build, injectée
# à l'exécution par le NVIDIA Container Toolkit — l'édition de liens la
# résout via les stubs du toolkit.
RUN git clone --depth 1 https://github.com/ggml-org/whisper.cpp /w \
    && cmake -S /w -B /w/build ${WHISPER_CMAKE_FLAGS} -DGGML_CUDA=ON \
         -DCMAKE_CUDA_ARCHITECTURES="${CUDA_ARCHS}" \
    && cmake --build /w/build -j"$(nproc)" --target whisper-server \
    && strip /w/build/bin/whisper-server \
    && ( ! ldd /w/build/bin/whisper-server | grep -E "libggml|libwhisper" )

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
# Dictée vocale (POST /api/transcribe). Les modèles (190 Mo à 1,1 Go) ne sont
# PAS dans l'image : téléchargés à la demande dans /data/whisper/.
# Deux binaires : voir l'étape whisperbuild-cpu pour la raison.
COPY --from=whisperbuild-cpu  /w/build/bin/whisper-server /usr/local/bin/whisper-server-cpu
COPY --from=whisperbuild-cuda /w/build/bin/whisper-server /usr/local/bin/whisper-server-cuda
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh && mkdir -p /data /models

# llama-server et ses .so vivent dans /app (convention de l'image amont).
ENV LOKI_CONTAINER=1 \
    LOKI_HOME=/data \
    LOKI_MODEL_DIRS=/models \
    LOKI_ENGINE_BIN=/app/llama-server \
    LOKI_WHISPER_SERVER_CPU=/usr/local/bin/whisper-server-cpu \
    LOKI_WHISPER_SERVER_CUDA=/usr/local/bin/whisper-server-cuda \
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
