# Migration Rust lot 1 — façade + mise à jour au démarrage — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Mettre un binaire Rust `loki-rs` en frontal du conteneur (proxy vers le Go) et faire que le conteneur prenne les derniers binaires publiés à chaque démarrage.

**Architecture:** `loki-rs serve` écoute sur `LOKI_WEB_PORT` (8090), lance `loki web 8091` (Go, `LOKI_WEB_BIND=127.0.0.1`) en enfant supervisé et lui proxifie tout (HTTP, SSE, WebSocket). `loki-rs self-update` télécharge la release GitHub `rolling` dans `/data/bin/<build>/`, `loki-rs which-bin` dit à l'entrypoint quel dossier de binaires utiliser.

**Tech Stack:** Rust 2021 (tokio, hyper 1, hyper-util, http-body-util, reqwest rustls, sha2, serde_json), cible `x86_64-unknown-linux-musl` ; Go 1.25 existant ; GitHub Actions.

Spec : `docs/superpowers/specs/2026-10-13-rust-migration-lot1-design.md`.

## Global Constraints

- Port public inchangé : 8090 (`LOKI_WEB_PORT`). Go interne : `127.0.0.1:8091` (`LOKI_GO_PORT`).
- Release : tag `rolling`, pré-release, assets `loki-linux`, `loki-rs-linux`, `SHA256SUMS`, `manifest.json` (uploadé en dernier).
- `manifest.json` = `{"build": <github.run_number>, "abi": <IMAGE_ABI>, "commit": "<sha>"}`.
- Source unique de l'ABI : fichier `IMAGE_ABI` à la racine (contenu `1`), copié dans l'image en `/etc/loki/IMAGE_ABI`. Build de l'image : `ENV LOKI_BUILD=<run_number>` (0 en build local).
- Les binaires de `/data/bin/<build>/` ne sont utilisés que si `abi == IMAGE_ABI` **et** `build > LOKI_BUILD`.
- `LOKI_AUTO_UPDATE=0` : aucune requête réseau. Le boot n'est jamais bloqué (code 0, timeouts bornés).
- La façade efface `X-Loki-Via` entrant ; transmet `Host`, `Authorization`, `Cookie`, `Origin`, `Sec-Fetch-*`, `X-Forwarded-Proto` intacts ; retire les hop-by-hop ; ne bufferise rien.
- Go : `gofmt -l .` vide + `staticcheck ./...` propre. Rust : `cargo fmt --check`, `cargo clippy --all-targets -- -D warnings`, `cargo test`.
- Git : jamais `git add -A` ni `git stash` ; commits en français, style narratif.

## File Structure

```
IMAGE_ABI                          ABI d'image (entier)
rust/Cargo.toml                    workspace
rust/loki-rs/Cargo.toml
rust/loki-rs/src/main.rs           CLI : serve | self-update | which-bin | version
rust/loki-rs/src/proxy.rs          proxy HTTP/SSE/WS vers le Go
rust/loki-rs/src/child.rs          supervision de `loki web`
rust/loki-rs/src/update/mod.rs     orchestration self-update + which-bin
rust/loki-rs/src/update/sums.rs    parse SHA256SUMS
rust/loki-rs/src/update/state.rs   release.json, décision, sélection du dossier
rust/loki-rs/tests/proxy.rs        intégration façade
rust/loki-rs/tests/update.rs       intégration self-update (faux serveur)
internal/loki/web_server.go        LOKI_WEB_BIND
Dockerfile                         étape rustbuild, IMAGE_ABI, LOKI_BUILD
docker-entrypoint.sh               self-update, which-bin, exec loki-rs serve
.github/workflows/ci.yml           job rust
.github/workflows/docker-publish.yml  LOKI_BUILD + job rolling
README.md                          section mise à jour au démarrage
```

---

### Task 1: Go — adresse d'écoute configurable

**Files:** Modify `internal/loki/web_server.go` (cmdWeb), Test `internal/loki/web_bind_test.go`

**Interfaces:** Produces `webBindAddr(port int) string` — `LOKI_WEB_BIND` (défaut `0.0.0.0`) + `:port`.

- [ ] Test : `t.Setenv("LOKI_WEB_BIND","")` → `"0.0.0.0:8090"` ; `"127.0.0.1"` → `"127.0.0.1:8091"`.
- [ ] `go test ./internal/loki -run TestWebBindAddr` → FAIL (indéfini).
- [ ] Implémenter, l'utiliser dans `cmdWeb` à la place de `fmt.Sprintf("0.0.0.0:%d", port)`.
- [ ] Test PASS, `gofmt -l .` vide, staticcheck propre. Commit.

### Task 2: Workspace Rust + parse SHA256SUMS + état d'installation

**Files:** Create `rust/Cargo.toml`, `rust/loki-rs/Cargo.toml`, `src/main.rs` (squelette CLI), `src/update/{mod,sums,state}.rs`, `.gitignore` (`rust/target/`).

**Interfaces (produits) :**
- `sums::parse(text:&str) -> HashMap<String,String>` (nom → hex minuscule ; accepte `*nom`, ignore lignes vides/invalides).
- `state::Manifest { build:u64, abi:u32, commit:String }` (serde).
- `state::Installed` = Manifest écrit dans `<dir>/release.json`.
- `state::read_installed(dir:&Path) -> Option<Manifest>`.
- `state::select_dir(data_bin:&Path, image_dir:&Path, image_build:u64, image_abi:u32) -> PathBuf` : `<data_bin>/<build>` si release.json valide, abi égal, build > image_build, et `loki` + `loki-rs` présents dedans ; sinon `image_dir`.
- `state::needs_update(m:&Manifest, image_build:u64, image_abi:u32, installed:Option<&Manifest>) -> Decision` avec `enum Decision { Install, UpToDate, AbiMismatch }`.

- [ ] Tests unitaires : parse (2 espaces, `*`, majuscules → minuscules, ligne cassée ignorée) ; `needs_update` (abi ≠ → AbiMismatch ; build ≤ image → UpToDate ; build ≤ installé → UpToDate ; sinon Install) ; `select_dir` (aucun fichier → image ; abi ≠ → image ; build ≤ image → image ; binaire manquant → image ; ok → data dir).
- [ ] `cargo test` FAIL puis implémentation, PASS, fmt + clippy. Commit.

### Task 3: self-update (téléchargement vérifié, installation atomique)

**Files:** `src/update/mod.rs`, `tests/update.rs`

**Interfaces :** `update::run(cfg:&UpdateConfig) -> Outcome` (jamais d'erreur remontée au CLI : journalise et rend `Outcome::{Installed(u64), UpToDate, AbiMismatch, Failed(String), Disabled}`) ; `UpdateConfig { base_url, data_bin, image_build, image_abi, enabled, timeout }` construit par `UpdateConfig::from_env()` (`LOKI_UPDATE_URL` défaut `https://github.com/R0m1k3/Loki/releases/download/rolling`, `LOKI_HOME/bin`, `LOKI_BUILD`, `/etc/loki/IMAGE_ABI` puis `LOKI_IMAGE_ABI`, `LOKI_AUTO_UPDATE`).

Algorithme : manifeste → décision → `SHA256SUMS` → télécharge `loki-linux`, `loki-rs-linux` dans `<data_bin>/.tmp-<build>/` → vérifie chaque hash → chmod 755 (unix) → renomme en `loki`, `loki-rs` → écrit `release.json` → renomme le dossier en `<data_bin>/<build>` → écrit `<data_bin>/release.json` (tmp + rename) → supprime les autres `<n>/` et `.tmp-*`. Toute erreur : supprime `.tmp-<build>`, `Failed`.

- [ ] Tests d'intégration (faux serveur hyper sur 127.0.0.1:0 servant des fichiers en mémoire) : installation OK (fichiers présents, release.json, select_dir pointe dessus) ; hash faux → rien installé, ancien état intact ; abi ≠ → rien téléchargé hormis le manifeste ; serveur absent → Failed, pas de panic ; `enabled=false` → Disabled sans requête ; seconde exécution → UpToDate et nettoyage des anciens builds.
- [ ] FAIL → implémentation → PASS, fmt + clippy. Commit.

### Task 4: Façade proxy

**Files:** `src/proxy.rs`, `tests/proxy.rs`

**Interfaces :** `proxy::serve(listener: tokio::net::TcpListener, backend: SocketAddr, shutdown: impl Future) -> io::Result<()>`.

Comportement : hyper http1 + `with_upgrades` ; chaque requête → client hyper-util vers `backend` (URI `http://backend{path_and_query}`), Host d'origine conservé ; retire hop-by-hop (`connection`, `keep-alive`, `proxy-connection`, `te`, `trailer`, `transfer-encoding`, `upgrade`, + noms listés dans `Connection`) et `x-loki-via` ; si requête d'upgrade (`Connection: upgrade` + `Upgrade`), garde `Connection: upgrade` + `Upgrade`, attend 101, puis `copy_bidirectional` entre les deux côtés upgradés ; corps en streaming dans les deux sens ; erreur de connexion → 502 `text/plain` « Loki (Go) redémarre — réessaie dans un instant ».

- [ ] Tests (faux backend hyper) : GET relayé avec Host/Authorization/Origin/X-Forwarded-Proto intacts et `X-Loki-Via` absent côté backend ; POST corps relayé ; SSE : backend envoie 1er événement puis attend 500 ms — le client reçoit le 1er avant 300 ms ; WebSocket écho via tokio-tungstenite ; backend arrêté → 502.
- [ ] FAIL → implémentation → PASS, fmt + clippy. Commit.

### Task 5: Supervision du Go + commande `serve`

**Files:** `src/child.rs`, `src/main.rs`

**Interfaces :** `child::Supervisor::spawn(go_bin:PathBuf, port:u16) -> Supervisor` (relance avec backoff 1 s → 30 s, remis à 1 s après 60 s de vie ; stdin null, stdout/stderr hérités ; env `LOKI_WEB_BIND=127.0.0.1`) ; `Supervisor::shutdown(self, grace:Duration)` (SIGTERM unix / kill windows, puis kill après `grace`).
`main serve` : go_bin = `LOKI_GO_BIN` sinon `loki` à côté de l'exe courant ; attend jusqu'à 30 s que `127.0.0.1:LOKI_GO_PORT` accepte (sinon ouvre quand même, 502 en attendant) ; `proxy::serve` sur `0.0.0.0:LOKI_WEB_PORT` ; SIGTERM/SIGINT → arrêt du proxy + `shutdown(10 s)`.
`main which-bin` imprime `select_dir(...)` ; `main version` imprime version crate + `LOKI_BUILD`.

- [ ] Test unitaire `go_bin` par défaut ; test du backoff (fonction pure `next_backoff(prev, lived) -> Duration`).
- [ ] Test manuel local : `loki-rs serve` avec un `loki.exe` construit → `curl localhost:8090/` renvoie l'UI.
- [ ] Commit.

### Task 6: Image, entrypoint, CI, release rolling, doc

**Files:** `IMAGE_ABI`, `Dockerfile`, `docker-entrypoint.sh`, `.github/workflows/ci.yml`, `.github/workflows/docker-publish.yml`, `README.md`.

- Dockerfile : étape `FROM rust:1-alpine AS rustbuild` (`apk add musl-dev`, `cargo build --release --locked`), copie `/usr/local/bin/loki-rs` ; `ARG LOKI_BUILD=0` → `ENV LOKI_BUILD` ; `COPY IMAGE_ABI /etc/loki/IMAGE_ABI`.
- Entrypoint : `loki-rs self-update` (best-effort) ; `BIN_DIR=$(loki-rs which-bin)` ; `LOKI="$BIN_DIR/loki"` pour tous les appels config/start ; `exec "$BIN_DIR/loki-rs" serve`.
- CI : job `rust` (fmt, clippy, test sur ubuntu).
- docker-publish : `LOKI_BUILD=${{ github.run_number }}` en build-arg ; job `rolling` (contents: write, concurrency `rolling`) : build Go linux CGO=0, build Rust musl (`rustup target add x86_64-unknown-linux-musl`, `musl-tools`), SHA256SUMS, manifest, `gh release create rolling --prerelease` si absent, upload `--clobber` binaires+sums puis manifest.
- README : section « Mise à jour au démarrage » (comportement, `LOKI_AUTO_UPDATE=0`, ABI → pull).
- [ ] `sh -n docker-entrypoint.sh`, YAML relu, commit.

## Self-review

Spec couvert : façade (T4/T5), X-Loki-Via (T4), bind Go (T1), rolling + manifeste + ABI (T2/T3/T6), repli et boot non bloquant (T3/T6), désactivation (T3), tests (T2–T4), CI (T6). Écart assumé vs spec : ordre des versions par `build` (run_number) et non par commit, et installation dans `/data/bin/<build>/` (bascule atomique par `release.json`).
