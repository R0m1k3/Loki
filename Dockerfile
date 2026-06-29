# ── Étape 1 : build du frontend ─────────────────────────────────────────
FROM node:20-alpine AS frontend
WORKDIR /app/frontend
COPY frontend/package.json frontend/package-lock.json* ./
RUN npm install
COPY frontend/ ./
RUN npm run build

# ── Étape 2 : runtime backend (sert aussi le front statique) ────────────
FROM python:3.12-slim AS runtime
WORKDIR /app

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    WORKSPACE_DIR=/workspace \
    DATA_DIR=/data \
    PORT=8080

# curl pour le HEALTHCHECK
RUN apt-get update && apt-get install -y --no-install-recommends curl \
    && rm -rf /var/lib/apt/lists/*

COPY backend/requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

COPY backend/ ./backend/
# Frontend compilé servi en statique par FastAPI
COPY --from=frontend /app/frontend/dist ./backend/static

# Utilisateur non-root + dossiers de runtime lui appartenant
RUN useradd --create-home --uid 10001 loki \
    && mkdir -p /workspace /data \
    && chown -R loki:loki /workspace /data /app
USER loki

EXPOSE 8080
WORKDIR /app/backend

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -fsS "http://localhost:${PORT}/api/health" || exit 1

CMD ["sh", "-c", "uvicorn app.main:app --host 0.0.0.0 --port ${PORT}"]
