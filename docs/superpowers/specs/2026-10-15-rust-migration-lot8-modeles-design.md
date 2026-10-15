# Lot 8 — routes des modèles tenues par la façade

## Contexte

Les modèles `.gguf` vivaient dans le Go :

- la liste du sélecteur (`handleModels`, `backend_models_refs.go`) ;
- les dossiers de modèles (`backend_model_dirs.go`) ;
- la suppression (`deleteModelFile`) ;
- les téléchargements (`backend_models.go`, `backend_models_shards.go`) ;
- le catalogue Hugging Face (`backend_hf.go`, `web_hf.go`, `backend_catalog.go`).

Lot 8 : la façade tient tout cela quand `LOKI_MODELS=rust` (expérimental ;
sans la variable rien ne change, tout part au Go).

## Ce que sert la façade

| Route | Module | Rôle |
|---|---|---|
| `GET /api/models` | `models.rs` | `.gguf` des dossiers déclarés (tranches 2..N exclues), taille de la famille, tranches manquantes, qui les référence (presets, configuration en service) |
| `POST /api/models/delete` | `models.rs` | refus si en service ; 409 avec la liste des presets sans `force` ; le modèle part avec toutes ses tranches, octets libérés rendus |
| `GET\|POST /api/models/dirs` | `models.rs` | dossiers de modèles, nombre de modèles, espace libre ; ajout (dossier existant) et retrait |
| `POST /api/models/download/probe` | `downloads.rs` | taille du modèle (toutes ses tranches), espace libre, « ça tient ? », ou « déjà là » avec la valeur de `MODEL=` |
| `POST /api/models/download` | `downloads.rs` | lance en arrière-plan ; 409 si déjà en cours ; « déjà là » si toutes les tranches sont présentes |
| `GET /api/models/download/status` | `downloads.rs` | état de chaque téléchargement de ce lancement |
| `POST /api/models/download/cancel` | `downloads.rs` | annule ; ce que le téléchargement a écrit est effacé |
| `GET /api/hf/search` | `hf.rs` | dépôts GGUF, matériel local joint |
| `GET /api/hf/files` | `hf.rs` | modèles, projecteurs et poids MTP d'un dépôt, verdict mémoire de chaque modèle (contexte du preset actif, projecteur du dépôt) |
| `GET\|POST /api/hf/token` | `hf.rs` | jeton vérifié auprès de Hugging Face avant d'être enregistré, jamais rendu en clair |

Mêmes contrats (corps, codes, messages) et même mécanique que le Go, entre
autres :

- téléchargement :
  - lien de page Hugging Face (`/blob/`) converti en lien direct ;
  - modèle en tranches rapatrié en entier ;
  - place disque vérifiée sur le total, avec 256 Mio de marge ; jamais de
    refus sur FUSE, overlayfs, NFS, SMB, Ceph ou GlusterFS ;
  - plages d'octets en parallèle (`LOKI_DL_CONNS`) et reprise d'une
    connexion coupée ;
  - jeton envoyé aux seuls hôtes Hugging Face ;
- catalogue : réponses mises en cache (60 s pour une recherche, 10 min pour un
  dépôt), cache vidé à l'enregistrement d'un jeton ;
- `.part` orphelins vidés au démarrage — par la façade : le Go ne le fait
  plus, il effacerait un téléchargement qu'elle mène.

## Côté Go (`facade_models.go`)

Le tunnel du relais et la redistribution E2E arrivent au Go sans passer par
la façade : `withFacadeChat` lui remet ces routes, authentifiées par le
jeton interne, comme celles du chat. Façade injoignable : 502 « modèles
indisponibles : … ».

La façade transmet `LOKI_MODELS=rust` et son adresse au Go qu'elle lance.

## Écarts assumés

- Une source qui ignore les plages n'est plus lue en entier pendant la sonde.
- Erreurs de transport (réseau, TLS) au format de reqwest.
- Message d'un corps JSON illisible : celui de serde (l'interface n'en envoie
  jamais).
- Hôte d'un lien collé en majuscules converti comme les autres.
- Espace libre et mémoire vive inconnus hors Unix/Linux (la façade n'est
  livrée que pour Linux).
- `/api/models/download/status` trié par date de début.
- Nombres flottants du matériel écrits `24.0` (le Go écrit `24`) : même
  valeur JSON.

## Parité

Journaux tirés du Go et rejoués en Rust (`LOKI_PARITY_UPDATE=1` pour
régénérer) :

- `internal/loki/parity_dl_test.go` → `tests/parity_models/download.json` →
  `tests/downloads.rs` : lien normalisé, famille de tranches,
  `Content-Range`, messages de refus, hôtes et dépôts Hugging Face, dossier
  de destination ;
- `internal/loki/parity_models_test.go` → `tests/parity_models/models.json` →
  `tests/models.rs` : quantification déduite du nom, découpage
  d'`EXTRA_ARGS`, jeton masqué, `gated`, classement d'un dépôt, choix du
  projecteur, verdicts mémoire, puis un scénario sur disque de 20 appels
  (liste, dossiers, suppressions avec et sans `force`, modèle en service,
  tranches, chemins refusés) rejoué à l'identique, fichiers restants et
  dossiers enregistrés compris.

Deux mutations (`replacen` → `replace` sur `/blob/`, `--mmproj` ignoré)
font échouer les rejeux.

De bout en bout :

- téléchargements contre un faux Hugging Face : trois connexions sur des
  plages disjointes, source sans plages, tranches dont une présente, dépôt
  verrouillé, panne pendant la deuxième tranche, annulation et 409 ;
- catalogue contre un faux Hugging Face : recherche, cache, taille LFS,
  dépôt verrouillé ou absent, jeton refusé ou accepté ;
- toutes les routes par la façade (clé de pilotage, remise du Go, base du Go
  écrite), et laissées au Go sans la variable.

## Reste

- Essai réel sur une installation avant d'en faire le défaut.
