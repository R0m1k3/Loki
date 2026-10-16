# Lot 45 — Outils d'image de l'agent dans la façade

## Contexte

Deux outils de l'agent montrent une image au modèle :

- `see_image` charge un fichier image du disque dans la vision du
  modèle ;
- `web_screenshot` photographie une page rendue par Playwright, la range
  dans le dossier de la discussion et donne à l'agent la ligne markdown
  qui la montre à l'utilisateur.

La façade tient la conversation par défaut, mais ces deux outils passaient
encore par le Go (`/internal/v1/tool/exec`), qui renvoyait l'image à
relayer dans `vision_img`.

## Le lot

`src/imagetools.rs` porte trois fichiers Go :

- `chat_vision_tool.go` (`see_image`) ;
- `chat_screenshot.go` (`web_screenshot`) ;
- `prepareImageForModel` (`web_upload_orient.go`).

Ce qui est porté :

- **la préparation de l'image**, la même que pour les pièces jointes :
  - orientation EXIF d'une JPEG redressée dans les pixels ;
  - grand côté ramené sous 1568 px (mêmes pixels que le Go) ;
  - PNG réencodée en PNG, le reste en JPEG qualité 88 ;
  - rien à faire, ou image que la bibliothèque standard du Go ne décode
    pas (WebP, BMP, PNG cassée) : les octets d'origine ;
- **la sonde du moteur** (`engineSeesImages`) :
  - preset externe : sa case « vision » ;
  - sinon `GET /props` du moteur local, authentifié par la clé de
    complétion, gardé 10 s ;
  - appelée en dernier, après toutes les vérifications du fichier ;
- **`see_image`**, mêmes textes :
  - vision coupée, poste distant, chemin manquant ;
  - format non reconnu, fichier introuvable, dossier ;
  - trop lourde (12 Mo au plus) ;
  - moteur aveugle, fichier vide ;
  - chemin relatif au dossier de travail de l'agent, ou absolu ;
- **`web_screenshot`**, mêmes textes :
  - Playwright absent, url manquante ou invalide ;
  - internet coupé : seule une adresse locale passe (`localhost`,
    `127.0.0.1`, `::1`, `0.0.0.0`, `*.localhost`, casse comprise comme
    le Go) ;
  - mêmes arguments de Playwright (viewport, page entière, attente,
    90 s au plus) ;
  - erreur résumée par ses trois dernières lignes non vides ;
  - aucune image ou image vide refusée ;
  - capture rangée dans `captures/` du dossier de la discussion, nommée
    par l'hôte et l'heure locale ;
  - ménage : 20 captures et 40 Mo au plus, la nouvelle reste ;
  - relayée au modèle (« Voici la capture demandée. ») seulement si la
    vision est active et que le moteur voit les images.

`ChatShared.vision` porte la sonde du processus ; `GoTurnHost::exec_tool`
exécute les deux outils :

- `see_image` prend le dossier de travail et le poste visé du contexte
  des outils (lot 7) ; contexte indisponible : le Go prend le relais ;
- `web_screenshot` range ses captures dans le dossier de la discussion
  principale, sous le dossier de travail de l'agent, et les montre par
  `ToolOutcome.image`.

Rien ne change côté Go : ces outils n'ont pas d'état partagé, et le Go
garde les siens pour ses sous-agents et ses tâches IA.

## Écarts connus

- Encodeurs PNG et JPEG : mêmes pixels, octets différents.

## Parité

`internal/loki/parity_image_tools_test.go` (Linux) tire du Go un journal
(`tests/parity_images/images.json`) avec ses fichiers (en base64), un faux
moteur (`/props` authentifié, vision réglée par étape, sondes comptées) et
un faux Playwright (script qui note ses arguments et produit l'image, rien,
un fichier vide ou une erreur).

Chaque étape porte :

- le résultat, avec le texte relayé au modèle ;
- l'image relayée :
  - identique à un fichier : son nom ;
  - réencodée : son type et ses dimensions ;
  - en PNG : cinq pixels ;
- la capture montrée, les arguments de Playwright, le nombre de sondes ;
- le dossier des captures.

Le journal couvre :

- `see_image` :
  - vision coupée ;
  - chemin vide, pas une image, introuvable, dossier, trop lourde ;
  - moteur aveugle, fichier vide ;
  - onze images : PNG, JPEG, GIF, WebP, BMP, PNG cassée, JPEG tournée,
    grandes images réduites en largeur et en hauteur, sous-dossier,
    chemin absolu ;
- `web_screenshot` :
  - url manquante ou invalide, internet coupé ;
  - capture locale sans vision, capture vue par le modèle ;
  - page entière réduite ;
  - vision mais moteur aveugle ;
  - Playwright en échec, image vide, aucune image ;
  - ménage des captures.

`tests/imagetools.rs` le rejoue contre le port. D'autres tests
complètent :

- `tests/lot6.rs` (`outils_image_par_la_facade`) : un tour de la façade
  appelle les deux outils sans passer par le Go ;
- tests unitaires : types d'image, noms de capture, adresses locales,
  dernières lignes, préparation, ménage, refus avant Playwright.
