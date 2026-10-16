# Lot 44 — Navigateur piloté dans la façade

## Contexte

Le contrôle du navigateur donne à l'agent un Chromium local, piloté par le
protocole DevTools (CDP). L'agent l'utilise ainsi :

- il ouvre une page et lit les éléments interactifs numérotés ;
- il clique, tape, défile par numéro ;
- avec la vision, il voit une capture quadrillée et clique en
  coordonnées.

La façade tient la conversation par défaut, mais la session Chrome, les
outils `browser_*` et l'anneau des aperçus montrés à l'utilisateur étaient
au Go (`/internal/v1/tool/exec`).

## Le lot

`src/browser/` porte quatre fichiers Go :

- `computer_cdp.go` : client CDP, lancement, scripts ;
- `computer_use.go` : les neuf outils ;
- `browser_grid.go` : la grille de la capture ;
- `cu_autoshot.go` : les aperçus.

Ce qui est porté :

- **le lancement** :
  - binaire imposé, celui de Playwright ou du système ;
  - profil temporaire `loki-cu-*`, et ceux de plus d'une heure balayés ;
  - mêmes arguments (headless sauf `LOKI_CU_HEADFUL`) ;
  - tué avec la façade (`PR_SET_PDEATHSIG`) ;
  - port lu dans `DevToolsActivePort`, cible « page » par `/json` ;
  - viewport fixe 1280×800 ;
  - script qui garde les nouveaux onglets dans l'onglet courant ;
- **la session** :
  - une seule, lancée à la demande ;
  - vérifiée par un ping avant chaque outil et relancée si elle ne répond
    plus ;
  - commandes multiplexées par numéro, 30 s au plus ;
  - requêtes réseau suivies pour attendre le calme (600 ms, 6 s au plus)
    après un chargement ;
- **les scripts** injectés, copiés du Go à l'octet près (le journal
  compare leurs empreintes) :
  - éléments actionnables et visibles ;
  - shadow DOM ;
  - nom d'un champ par son libellé, sans révéler un mot de passe ;
  - recherche non destructive ;
  - liste déroulante native réglée par son option ;
- **les outils**, mêmes textes :
  - snapshot dédoublonné ;
  - indices de défilement ;
  - erreurs de navigation (`errorText`, page `chrome-error://`,
    `ERR_ABORTED` bénin) ;
  - `back` et `forward` ;
  - touches nommées ;
  - plafond de 8000 caractères ;
- **la capture** (`browser_screenshot`, vision active) :
  - PNG propre rangé dans le dossier de travail
    (`capture-AAAAMMJJ-HHMMSS.png`, heure locale) ;
  - image du modèle quadrillée tous les 100 px, mêmes pixels que le Go ;
    à 1280×800 elle est sous la taille maximale et part telle quelle ;
  - relayée au modèle comme `see_image` ;
- **les aperçus** :
  - après une action qui change la page, jamais après une erreur ;
  - JPEG en mémoire, anneau de huit ;
  - servis par `/api/chat/cushot`, que le Go remet désormais à la façade.

`ChatShared.browser` porte le navigateur du processus ;
`GoTurnHost::exec_tool` exécute les outils et prend l'aperçu.

Un seul navigateur : quand la façade tient la conversation, le Go lui passe
ses propres appels (`facade_browser.go`) :

- outils des sous-agents et des tâches IA ;
- aperçus ;
- fermeture (interrupteur coupé, ligne de commande).

Ils passent par `POST /internal/facade/v1/browser`, avec trois opérations :
`tool`, `shot`, `shutdown`. Si la façade ne répond pas, le navigateur du Go
prend le relais. Couper l'interrupteur côté façade (lot 17) ferme les deux.

## Écarts connus

- Encodeurs PNG et JPEG : mêmes pixels, octets différents.
- Une capture au-delà de 1568 px ne serait pas réduite (le viewport fixe
  rend le cas impossible).

## Parité

`internal/loki/parity_browser_test.go` (Linux) tire du Go un journal de 42
étapes (`tests/parity_browser/browser.json`) contre un faux Chrome :

- un script annonce le port d'un faux serveur CDP ;
- le faux CDP répond selon le scénario de chaque étape et note chaque
  commande.

Chaque étape porte :

- le résultat ;
- les commandes CDP (méthode, paramètres, scripts résumés par leur
  empreinte) ;
- pour une capture, ses dimensions et dix pixels de la grille ;
- les captures rangées.

Les arguments de lancement sont relevés aussi. Le journal couvre :

- ouverture, snapshot inchangé, réouverture complète ;
- snapshots vide, `null`, sans valeur, illisible, exception JS ;
- défilements ;
- clics par numéro, introuvable, refusé, en coordonnées ;
- saisies libre, dans un champ, liste déroulante, option absente, champ
  introuvable ;
- touches ;
- recherches ;
- navigations en échec, page d'erreur, interruption bénigne, `back`,
  `forward`, commande refusée ;
- captures sans vision, quadrillée, refusée ;
- sortie plafonnée.

`tests/browser.rs` le rejoue contre le port, puis vérifie l'aperçu (JPEG
de l'anneau) et la fermeture (profil retiré).

`vrai_navigateur` (avec `LOKI_CU_SMOKE=1`, comme `TestCDPSmoke`) pilote un
vrai Chromium sur une page locale : ouverture, recherche, saisie, liste
déroulante, clic qui navigue, `back`, capture 1280×800, aperçu, navigation
en échec.

D'autres tests complètent :

- `tests/lot6.rs` (`navigateur_par_la_facade`) : un tour de la façade ouvre
  une page sans passer par le Go, avec aperçu ; l'API du Go et
  `/api/chat/cushot` par une façade complète ;
- `TestFacadeNavigateur` : le Go passe ses outils, captures, aperçus et
  fermeture à la façade, et se replie sur son navigateur si elle est en
  panne ;
- tests unitaires : outils, nombres au format du Go, arguments, grille,
  profils, lancement impossible.
