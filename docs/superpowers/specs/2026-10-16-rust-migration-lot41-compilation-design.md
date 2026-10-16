# Lot 41 — Compilation de llama.cpp depuis les sources dans la façade

## Contexte

Depuis le lot 22, la façade tient les backends llama.cpp quand
`LOKI_BACKENDS=rust` :

- statut, vérification git ;
- binaires précompilés ;
- bascule fast/opt ;
- backends custom ;
- le job des moteurs.

La compilation depuis les sources restait au Go. Il en rapportait chaque
étape au job de la façade par l'API interne
(`/internal/facade/v1/build-job/*`). Elle couvre trois routes :

- `POST /api/llamacpp/install` `{force}` ;
- `POST /api/llamacpp/update` `{clean}` ;
- `POST /api/llamacpp/install-custom` `{repo, name, ref}`.

## Le lot

`src/backends/build.rs` porte la compilation sous Linux et macOS. Sources Go :

- `backend_build.go` : plan, CMake, étapes suivies ;
- `lcRunInstall`, `lcRunUpdate`, `lcBuildAndSwitch`, `lcAppendLogTail`
  (`web_llamacpp.go`) ;
- `installCustomBackend`, `deriveBackendName`, `looksLikeGitURL`
  (`backend_llamacpp.go`) ;
- `autoInstallTool`, `ensureCompiler` (`sys_platform_unix.go`).

Le port couvre :

- **le plan complet** (`detectBuildPlan`) :
  - drapeaux Release, natif, UI de llama-server coupée ;
  - Metal sous macOS ;
  - CUDA quand nvcc et un GPU NVIDIA sont là : F16, racine du toolkit et
    compilateur explicites hors `/usr`, architectures lues par
    `nvidia-smi` ;
  - HIP, puis Vulkan ;
- **CMake** :
  - `build/` configuré ailleurs (dépôt déplacé) ou `clean` : mis de côté en
    `build.old` ;
  - nvcc dans le PATH et `CUDACXX` ;
  - `configure.log` et `build.log` dans le dépôt ;
  - erreurs « configuration CMake échouée » et « compilation échouée » ;
- **les étapes suivies** :
  - sorties standard et d'erreur dans un même tube, ordre gardé ;
  - git : découpage du `sinkWriter` (barre qui se redessine montrée toutes
    les 10 s, reste sans fin de ligne perdu) ;
  - CMake : découpage du `bufio.Scanner` (dernier morceau rendu, ligne de
    plus de 1 Mio qui arrête la lecture) ;
  - octets invalides en U+FFFD un à un ;
  - erreurs au format d'`exec` (`exit status N`, `signal: killed`,
    programme introuvable) ;
- **les outils** :
  - git et cmake, puis cc, c++, make ;
  - un manquant est installé au mieux par apt-get, dnf, pacman ou brew
    (sudo hors root) ;
  - sinon le message du Go ;
- **les jobs** :
  - installation : clone, compilation propre, BIN ; dépôt présent : bascule
    en mise à jour ; `force` : supprimé puis recloné ;
  - mise à jour : fetch, pull au besoin, commits avant et après ;
    « déjà à jour » sans recompiler ; service arrêté le temps du build et
    relancé même en échec ;
  - échec du build : fin des deux journaux (30 lignes) remontée dans le job ;
  - backend custom : URL vérifiée, nom tiré du dépôt (propriétaire ajouté à
    un fork nommé llama.cpp) ou assaini, clone ou mise à jour, ref, sans
    toucher à BIN.

Les routes répondent comme le Go :

- moteur fourni par l'image : 409 et le message du Go ;
- custom sans URL : 400 « URL du dépôt requise » ;
- optimisation en cours : 409 ;
- job déjà lancé : 409 ;
- sinon 200 `{"ok":true}` et le job tourne.

Le service moteur est celui de la façade si elle le pilote, sinon celui du
Go (`/internal/v1/service/action`), comme pour les binaires précompilés.

Côté Go, `facadeBackendsRoutes` gagne les trois routes hors Windows. Sous
Windows, la compilation reste au Go, qui rapporte toujours ses étapes à la
façade :

- générateur Visual Studio ;
- intégration CUDA de MSBuild ;
- `CUDA_PATH`.

Les messages du terminal du Go (outils manquants, dépendance connue absente
après un configure en échec) vont à la sortie d'erreur de la façade.

## Parité

`internal/loki/parity_build_test.go` (Linux) tire du Go un journal de 15
étapes (`tests/parity_backends/build.json`). De faux git, cmake et
compilateur, rangés dans le journal, sont seuls sur le PATH. Chaque étape
porte :

- la vue du job (lignes, phase, erreur, commits, fichiers compilés) ;
- BIN ;
- les dépôts qui ont un `build.old`.

Il couvre :

- mise à jour sans dépôt ;
- installation, puis installation sur un dépôt présent et installation
  forcée ;
- déjà à jour, nouveau commit, recompilation propre ;
- build en échec (fin des journaux) ;
- cmake absent, compilateur absent ;
- clone en échec ;
- custom : URL invalide, nouveau fork, nom assaini sur un ref, dossier
  existant sur un ref (URL `git@`).

`tests/backends.rs` (`compilation_comme_le_go`) rejoue le journal par la
façade : mêmes faux outils dans le PATH des sondes, POST de la route, job
suivi jusqu'au bout. `compilation_refusee` vérifie les refus des routes.

Les tests unitaires de `build.rs` couvrent :

- la barre de progression (`TestSinkWriterProgression`) ;
- le scanner (fin `\r\n`, dernier morceau, octets invalides, ligne
  démesurée) ;
- la racine du toolkit CUDA (`TestCudaToolkitRoot`) ;
- le cache CMake déplacé ;
- noms et URL des backends custom ;
- le plan par machine (CPU, nvcc du PATH, toolkit versionné,
  architectures, HIP, Vulkan) ;
- les erreurs des commandes (code de sortie, signal, programme absent,
  journal écrit).
