# Lot 24 — panneau « moteur de l'image » tenu par la façade

## Contexte

Le panneau Moteur permet de mettre à jour llama.cpp sans reconstruire
l'image. Le moteur est pris dans l'image de conteneur officielle
(`ghcr.io/ggml-org/llama.cpp:server-cuda-b#####`), dont on ne télécharge que
les couches du sommet, celles qui portent `/app`. Trois fichiers du Go le
servent :

- `web_engine.go` : statut, dernier build publié, mise à jour, bascule,
  suppression ;
- `web_engine_reco.go` : version recommandée, risque de rendu du
  raisonnement (défaut de b10763), retour à la version précédente,
  vérification du rendu du gabarit avant/après la bascule ;
- `backend_engine_oci.go` : dialogue avec le registre, extraction des
  couches, essai à blanc.

La mise à jour est un job, le job des moteurs que la façade tient depuis le
lot 22. Le risque de rendu et la vérification du rendu reposent sur la sonde
de gabarit, portée au lot 23.

## Le lot

Avec `LOKI_ENGINE_IMAGE=rust` **et** `LOKI_BACKENDS=rust` (expérimental), la
façade (`rust/loki-rs/src/engineimg`) sert les routes suivantes :

| Route | Rôle |
| --- | --- |
| `/api/engine` | statut : binaire, build, source, variante, versions installées, avis, encart « version recommandée », version précédente, relevé du rendu |
| `/api/engine/check` | dernier build publié pour la variante, comparé au courant |
| `/api/engine/update` | job : recherche, téléchargement des couches du sommet, essai à blanc, REASONING_PRESERVE=off si demandé, bascule, ménage, vérification du rendu |
| `/api/engine/use` | bascule vers une version installée ou vers le moteur de l'image |
| `/api/engine/remove` | suppression d'une version installée (refusée pour celle qui tourne) |
| `/api/engine/plan` | sur clic : version visée (dernière ou recommandée) et risque de rendu |
| `/api/engine/rollback` | retour au moteur d'avant la dernière bascule |

Sans les backends, loki-rs ne passe pas la variable au Go : le panneau
reste au Go et le dit au démarrage. `/api/engine/cmdline` reste au Go.

La sonde de gabarit est celle de la conversation quand la façade la tient
(`LOKI_CHAT=rust`) : le relevé du rendu rejoue alors la forme de ses vraies
requêtes. Sinon, la façade a sa propre sonde, qui sonde une requête nue.

Pour REASONING_PRESERVE=off posée dans le preset actif, la façade réutilise
l'enregistrement du lot 15 (`save_applying`). Le service est arrêté puis
relancé comme au lot 22 : par la façade si elle pilote le moteur, sinon par
le Go (`/internal/v1/service/action`).

Côté Go, `facade_engineimg.go` relaie à la façade les routes reçues par le
tunnel.

## Parité

`internal/loki/parity_engineimg_test.go` (Linux) tire du Go un journal
rejoué par `tests/engineimg.rs`. Il tourne contre :

- un faux registre OCI : jeton, index multi-architecture, manifeste annoté,
  couches .tar.gz ;
- un faux moteur local (le `fakeTpl` du Go, porté) pour la sonde et le rendu.

Le journal couvre :

- **le statut** : moteur de l'image, téléchargé ou compilé, avec la version
  précédente (absente quand c'est le moteur courant), le relevé du rendu,
  sans moteur d'image, sans aucun moteur ;
- **le dernier build** : publié, sans numéro, ou refusé (jeton refusé ou
  illisible, manifeste refusé ou illisible, aucune image pour
  l'architecture) ;
- **le plan** :
  - dernière version (avec ou sans numéro) ou recommandée (build trop
    ancien, tag figé absent, sans numéro), et déjà à jour ;
  - chaque verdict du risque de rendu : moteur arrêté, gabarit vide,
    REASONING_PRESERVE posée, EXTRA_ARGS, gabarit sans les variables, gabarit
    qui garde la réflexion, build antérieur au défaut, preset externe ;
- **la mise à jour** :
  - couches du sommet vers la base ; une couche du dessus masque celle du
    dessous ; liens absolus ramenés dans `/app` ; marqueurs de suppression et
    sous-dossiers ignorés ; couche sans fichier du moteur ; couche du runtime
    ignorée ; phase du téléchargement ;
  - essai à blanc ;
  - risque de rendu rappelé dans le journal, établi ou impossible à vérifier
    (moteur local arrêté) ;
  - REASONING_PRESERVE=off : posée dans le preset actif ou dans la
    configuration, déjà posée, ou inconnue du moteur ;
  - ménage : versions triées par build, dossier sans moteur épargné, version
    précédente gardée ;
  - rendu comparé avant/après : changé, identique, ou impossible sur le
    nouveau moteur ;
  - déjà à jour, corps illisible ;
- **les échecs** :
  - optimisation en cours, version inconnue, image sans serveur ou sans
    version annoncée, manifeste sans couche, runtime au sommet, couche
    introuvable, tag illisible ;
  - moteur qui ne démarre pas (12 dernières lignes de sa sortie) ou qui ne
    voit aucune carte ;
- **les bascules** : vers une version installée, vers l'image, vers une
  version absente, pendant une optimisation ; retour, deux fois, et en GET ;
- **la suppression** : version (`../` neutralisé), version absente, moteur
  utilisé ;
- **après chaque appel** : BIN, REASONING_PRESERVE, `engine/previous-bin` et
  l'arborescence de `engine/`.

Autres tests :

- la façade vérifie le service arrêté puis relancé autour d'une mise à jour
  et redémarré par une bascule (`service_arrete_puis_relance`), le relevé
  « non vérifié à temps », le 503 sans backends, les variantes et les
  vérifications périmées ;
- le Go (`TestFacadeMoteurImage`) vérifie que les sept routes partent à la
  façade, et elles seulement.

## Mutations

74 mutations du port Rust, toutes tuées sauf trois, équivalentes :

- la garde « moteur d'image absent » de la source : `same_path` ne
  rapproche jamais un chemin vide ;
- l'essai à blanc qui relance l'inventaire des cartes quand l'ancien moteur
  est le nouveau : même résultat ;
- une vérification du rendu périmée qui continue d'attendre son moteur : son
  relevé est de toute façon écarté au rangement.

## Écarts assumés

- Les erreurs réseau (registre injoignable) gardent le texte de `reqwest`.
- Un manifeste dont un champ a le mauvais type ne donne pas le message du
  Go : le nom Go du type anonyme n'est pas reproduit.
- La vérification du rendu attend son délai à la cadence choisie (5 s), sans
  le contexte Go qui coupe aussi une requête en cours au bout de 20 min.
