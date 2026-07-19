# Loki — Projets (répertoires de travail) + aperçu réductible

**Date** : 2026-07-19
**Objectif** : permettre à l'agent de travailler dans des projets entiers —
un projet = un répertoire choisi depuis la carte du chat — et pouvoir replier
le panneau d'aperçu.

**Choix validés** : projet = sous-dossier de premier niveau du workspace
(pas de chemin arbitraire) ; portée par session ; édition de la mémoire
écartée du périmètre.

---

## 1. Projets — backend

### Modèle
- Un projet = un sous-dossier de premier niveau de `WORKSPACE_DIR`
  (ex. `workspace/jeu-snake/`). Nom validé : `^[a-z0-9][a-z0-9_-]{0,40}$`.
- Session sans projet (`NULL`) = racine du workspace — compatibilité totale
  avec les sessions existantes.
- Colonne `project TEXT` ajoutée à `sessions` (migration douce
  `ALTER TABLE`, comme `summary`/`meta`).

### Racine active par requête (contextvar)
- `tools.py` : `_ACTIVE_ROOT: ContextVar[str | None]` + fonctions
  `set_project(name | None)` / lecture dans `_workspace_root()`. `_safe_path`
  inchangé dans sa logique de confinement — simplement re-raciné sur
  `workspace/<projet>` quand un projet est actif.
- `routes/chat.py` : au début de `chat()`, `tools.set_project(session["project"])`
  (après validation : dossier existant, sinon retombe sur la racine + notice
  SSE « projet introuvable, retour au workspace »).
- Les aides de contexte suivent la même racine : `_workspace_listing`,
  `_mentioned_files`, `_session_code_context` (fichiers vérifiés sous la
  racine active).
- `run_shell` : cwd = racine active (déjà `_workspace_root()`).

### Moteur code (Aider) par projet
- `coder.run_code_task` reçoit la racine active (cwd du process Aider).
- `coder.ensure_git(dir)` appelé à la création d'un projet → un dépôt git
  par projet, historique/revert propres. L'onglet Git du panneau droit opère
  sur le projet de la session courante (routes git prennent `project`).

### Routes
- `GET /api/projects` → `{projects: [{name, files: int}], root_files: int}`
  (sous-dossiers de premier niveau, dotfiles exclus).
- `POST /api/projects {name}` → mkdir + `ensure_git` ; 400 si nom invalide
  ou existant.
- `PATCH /api/sessions/{sid}` accepte `project: str | null` (en plus de
  `title`).
- `GET /api/files`, `/api/files/content`, `/api/files/download`,
  `DELETE /api/files` : paramètre optionnel `project` — même re-racinage,
  même confinement.
- Routes git (`/api/git/*`) : paramètre optionnel `project`.

## 2. Sélecteur projet — frontend (carte du chat)

- **Chip « 📁 <projet> »** dans le composer, à côté du sélecteur de mode
  Plan/Build/Yolo. Affiche `workspace` si aucun projet.
- Clic → menu (même style que les menus existants) : liste des projets,
  entrée active cochée, **« + Nouveau projet »** avec input inline
  (Enter crée + sélectionne, Escape annule).
- Sélection → `PATCH /api/sessions/{sid} {project}` → store met à jour la
  session courante → `refreshFiles()` re-racine LeftPanel / FilesView /
  PreviewPanel (le store passe `project` de la session courante aux appels
  fichiers).
- Nouvelle session : hérite du projet de la session courante (envoyé au
  `POST /api/sessions`).
- Session sans projet : comportement actuel inchangé.

## 3. Aperçu réductible — frontend

- Bouton de repli dans l'en-tête du `PreviewPanel` (à côté des onglets) :
  replie le panneau en **barre verticale fine (36 px)** ne contenant qu'un
  bouton de réouverture (icône ⇤ pivotée) et l'indicateur d'onglet actif.
- État replié persisté en `localStorage` (`loki.preview.collapsed`), la
  largeur l'est déjà (`loki.preview.width`).
- Replié, le panneau ne rend pas son contenu (pas d'iframe HTML vivante).

## 4. Hors périmètre
- Édition de la mémoire (RAG/résumés) — écartée par l'utilisateur.
- Chemins hors workspace, suppression/renommage de projets depuis l'UI
  (suppression possible via la corbeille de l'arborescence).
- Projet imbriqué (sous-sous-dossier comme projet).

## 5. Vérification
1. `POST /api/projects {"name":"demo"}` → dossier + `.git` créés ;
   nom invalide → 400.
2. Session A projet `demo`, session B sans projet : fichiers créés par
   l'agent de A atterrissent dans `workspace/demo/`, ceux de B à la racine ;
   l'arborescence gauche suit la session ouverte.
3. `curl "…/api/files?project=demo"` → arbre du projet seul ;
   `?project=../x` → 400.
4. Reprise : « corrige les bugs » dans une session projet → Aider travaille
   dans `workspace/demo/`, commit dans le git du projet, onglet Git montre
   l'historique du projet.
5. Aperçu : bouton replie → barre fine, contenu démonté ; réouverture
   restaure l'onglet et la largeur ; état conservé après rechargement.
6. pytest : contextvar (re-racinage + confinement), routes projects,
   migration colonne, héritage projet à la création de session.
