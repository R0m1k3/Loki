# Lot 25 — Strata tenu par la façade

## Contexte

Strata est le deuxième moteur de Loki. Il répartit les experts d'un MoE de
125 B (famille Qwen3.8-Flash-Next) entre VRAM, RAM et disque. Le moteur est
le paquet **figé** publié par AJEAN : les sources et le binaire Linux CUDA 12,
vérifiés par SHA-256. Un preset le choisit (`ENGINE=strata`).

`backend_strata.go` sert trois routes :

- `/api/strata` : la machine (cartes, RAM, disque, Python), ce qui y tient,
  la recommandation par version et les modèles installés, avec leur
  configuration de lancement en clair ;
- `/api/strata/settings` : les réglages d'un modèle installé. S'il est en
  service, la configuration est réappliquée et le moteur relancé ;
- `/api/strata/install` : un job des moteurs. Il télécharge et vérifie le
  paquet, lance l'installeur de Strata, écrit le preset et bascule dessus.

Le lancement (`loki serve` d'un preset Strata) vit aussi dans ce fichier.

## Le lot

Avec `LOKI_STRATA=rust` **et** `LOKI_BACKENDS=rust` (expérimental), la façade
(`rust/loki-rs/src/strata`) sert les trois routes :

- **la décision pure** (`strata/mod.rs`) :
  - placement des experts : drop, ram, arena ou disk, selon la RAM
    disponible, la carte d'appoint et `experts.bin` ;
  - configuration de lancement : réglages validés, carte d'aide, mode des
    experts, réglages du preset ;
  - ce qui tient sur la machine, la recommandation, les réglages, les noms ;
- **la machine** (`strata/api.rs`) :
  - cartes via `nvidia-smi` (la sonde des backends) ;
  - Python 3.10+ et son module venv ;
  - RAM totale et disponible (`/proc/meminfo`) ;
  - RAM du moteur Strata en marche (arbre du service dans `/proc`) ;
  - place libre ;
- **l'installation** : le job des moteurs. La sortie de l'installeur est
  découpée en lignes comme le `sinkWriter` du Go : une barre `\r` n'est
  montrée que toutes les 10 s. La bascule finale passe par le Go
  (`/internal/v1/preset/apply`, mode `switch`), qui applique le preset et
  relance le service.

Sans les backends, loki-rs ne passe pas la variable au Go et Strata reste au
Go. Le lancement reste au Go (`loki serve`, que `loki-rs engine serve` lui
délègue pour un preset Strata).

Côté Go, `facade_strata.go` relaie à la façade les routes reçues par le
tunnel.

## Correction du Go

`/api/strata/settings` décidait si le preset était en service **après** y
avoir écrit les nouveaux réglages. L'empreinte du nouveau contenu ne
correspondait alors plus à la configuration active : changer un réglage du
modèle en service ne l'appliquait jamais, et le moteur n'était pas relancé.
« En service » se décide désormais avant l'écriture, comme dans
`SavePresetApplying`. Le port Rust suit.

## Parité

`internal/loki/parity_strata_test.go` (Linux x86_64) tire du Go un journal
rejoué par `tests/strata.rs`. Pour le tirer, le Go reçoit des crochets de
test : `strataReleaseBase` (variable), `meminfoPath`, `strataDiskFree` et
`serviceActionOSFn`. Le journal couvre :

- **la machine** :
  - sans carte, nvidia-smi en panne ou vide, carte trop ancienne ou trop
    petite, carte d'aide trop ancienne ;
  - trois cartes de même VRAM : la plus récente devant (capacité de calcul
    arrondie au dixième) ;
  - Python absent, en panne, illisible, trop ancien, sans venv ;
  - 16, 47 ou 64 Go de RAM, meminfo sans MemAvailable ;
  - disque presque plein ou juste trop petit (marge de 5 Go), données déjà
    présentes ;
- **les réglages** :
  - GET, corps illisible, preset vide ou avec chemin, preset absent ou
    llama.cpp ;
  - hors limites, experts inconnus ;
  - preset inactif ; preset actif (appliqué et relancé), et pendant une
    optimisation ;
- **l'installation** :
  - GET, corps illisible, quant inconnu, optimisation en cours ;
  - machine sans carte, modèle qui ne tient pas ;
  - release introuvable, moteur altéré (empreinte) ;
  - installation complète, puis réinstallation (sources déjà là et
    décompressées, moteur téléchargé périmé retéléchargé) ;
  - installeur en échec, installeur sans configuration ;
- **les modèles installés** :
  - détails de lancement du modèle actif et de modèles inactifs (experts
    sur disque ou en RAM, vision coupée, sans carte d'aide) ;
  - mode du dernier lancement ;
  - `experts.bin` avec le moteur du paquet ou d'un autre paquet ;
  - RAM juste sous ou au-dessus du seuil du mode hors RAM, mode hors RAM
    coupé dans le preset ;
  - configuration de lancement disparue ou illisible ;
- **après chaque appel** : les actions du service, les clés de la
  configuration et l'arborescence de `presets/` et `strata/` (sans le
  contenu des archives).

Autres tests :

- les tests du Go (`backend_strata_test.go`), portés (`strata/tests.rs`) :
  configuration de lancement, placement des experts, recommandation,
  réglages, noms ;
- la RAM du moteur en marche, le découpage en lignes et les nombres écrits
  à la Go ;
- le Go (`TestFacadeStrata`) : les trois routes partent à la façade, et
  elles seules.

## Mutations

71 mutations du port Rust, toutes tuées (les dix survivantes du premier
passage l'ont été par les étapes et les tests ajoutés ci-dessus).

## Écarts assumés

- Les erreurs réseau (release injoignable) gardent le texte de `reqwest`.
- Une copie du moteur qui échoue (`prebuilt/`) ne donne pas exactement le
  message du Go, qui ouvre la source puis la destination.
