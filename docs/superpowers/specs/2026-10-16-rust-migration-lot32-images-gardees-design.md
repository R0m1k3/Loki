# Lot 32 — Images gardées (KEEP_TURN_IMAGES) dans le tour de la façade

## Contexte

Sans la clé, une image montrée par un outil (capture, `see_image`) est
éphémère. Le modèle la voit pendant le tour, puis elle disparaît de
l'historique. Le tour suivant renvoie donc un fil où elle manque : le
préfixe en cache diverge à elle, et toute la boucle d'outils qui suivait est
recalculée.

`KEEP_TURN_IMAGES` (`chat_keep_images.go`) garde l'image telle qu'envoyée,
sous conditions :

- vision réellement active ;
- image rangée par référence et relue à l'octet près ;
- coût **mesuré** par le moteur, jamais estimé ;
- toutes les images gardées tiennent dans 10 % de la fenêtre.

Les images gardées partent avant tout résumé. La clé gardait la conversation
au Go.

## Le lot

### Go (`facade_keepimg.go`)

- `/internal/v1/turn/keepimg` rend deux choses :
  - si la vision est réellement active (`visionEnabled`, `engineSeesImages`) ;
  - le budget des images gardées (`keepImgBudget`).
- `/internal/v1/turn/keepimg/store` range une image relayée par référence
  (`refRelayImage`) :
  - fichier chiffré si la mémoire l'est ;
  - date rafraîchie ;
  - relu à l'octet près.

  Il rend le message qui la porte ainsi et son poids en octets, ou un refus.
- `KEEP_TURN_IMAGES` ne garde plus la conversation.

### Rust

- `chat/keepimg.rs` porte l'état du tour (`keepImages`) :
  - `relay` : image gardée à l'essai, dans `messages` et `extra` ;
  - `sending` ;
  - `observe` : mesure de la requête qui l'a montrée la première, partage au
    prorata des octets, seuil de 16 jetons, budget ;
  - `rewritten` ;
  - `publishable` ;
  - `finish` : une image à l'essai jamais mesurée redevient éphémère.

  Une image refusée arrête le tour pour les suivantes.
- La boucle du tour :
  - `TurnHost::keep_images` donne le budget, mais seulement pour le fil
    principal d'une discussion, avec la clé et la vision active ;
  - `TurnHost::keep_image` range l'image par le Go ;
  - l'historique réécrit en cours de tour est publié sans les relais non
    gardés.
- Les images gardées partent d'abord, avant tout résumé :
  - au filet réactif (prompt trop long) : requête rejouée sans elles ;
  - en cours de tour, au seuil : la compaction ne suit que si elle reste
    nécessaire ;
  - sur une complétion coupée par une fenêtre pleine : rejouée sans résumé
    si leur retrait suffit.
- Début et fin de tour (`keptImagesTurnStart`) : les images gardées sont
  retirées si le contexte le demande, ou si elles ne doivent plus partir
  (clé retirée, vision absente).

La compaction et la réduction forcée retiraient déjà les images gardées
d'abord (`chat/compact.rs`).

## Tests

- `chat/keepimg` :
  - clé ;
  - gardée au coût mesuré ;
  - budget ;
  - sans mesure (pas d'usage, complétion coupée, pas de base) ;
  - requête relancée ;
  - rangement refusé ;
  - partage entre deux images ;
  - publication, réécriture, fin de tour, budget plein avant rangement.
- La boucle du tour :
  - requête identique avec et sans la clé, image gardée par référence au
    coût mesuré ;
  - refus : budget, pas d'usage, rangement refusé ;
  - image à l'essai en fin de tour ;
  - retrait en cours de tour, au filet réactif et sur une fenêtre pleine, à
    chaque fois sans résumé.
- Le vrai hôte (`tests/lot6.rs`) :
  - image rangée par le Go, vue en data-URL par le moteur, gardée avec son
    coût ;
  - clé retirée : l'image part au début du tour suivant, légende et marque
    gardées.
- Le Go (`facade_keepimg_test.go`) :
  - état sans projecteur puis vu par le moteur, budget ;
  - rangement relu à l'octet près ;
  - refus : texte seul, URL distante, format inconnu, base64 illisible, deux
    images ;
  - corps illisible, GET refusé.
