# Lot 8 — téléchargements de modèles tenus par la façade

## Contexte

Les téléchargements de `.gguf` (Hugging Face ou lien direct) vivaient dans le
Go : `backend_models.go`, `backend_models_shards.go`, `backend_model_dirs.go`,
`backend_hf.go`. Lot 8 : la façade les tient quand `LOKI_DOWNLOADS=rust`
(expérimental ; sans la variable rien ne change, tout part au Go).

## Ce que sert la façade (`rust/loki-rs/src/downloads.rs`)

| Route | Rôle |
|---|---|
| `POST /api/models/download/probe` | taille du modèle (toutes ses tranches), espace libre, « ça tient ? », ou « déjà là » avec la valeur de `MODEL=` |
| `POST /api/models/download` | lance en arrière-plan ; 409 si déjà en cours ; « déjà là » si toutes les tranches sont présentes |
| `GET /api/models/download/status` | état de chaque téléchargement de ce lancement |
| `POST /api/models/download/cancel` | annule ; ce que le téléchargement a écrit est effacé |

Mêmes contrats (corps, codes, messages) et même mécanique que le Go :

- lien de page Hugging Face (`/blob/`) converti en lien direct ;
- modèle en tranches rapatrié en entier (la première le désigne) ;
- tranches déjà là non retéléchargées ;
- place disque vérifiée sur le total, avec 256 Mio de marge ; jamais de refus
  sur FUSE, overlayfs, NFS, SMB, Ceph ou GlusterFS, où l'espace annoncé
  approxime ;
- jusqu'à `LOKI_DL_CONNS` connexions (8 par défaut, 16 au plus) par tranche
  quand la source honore les plages, 16 Mio par connexion au moins ;
- connexion coupée reprise où elle en était (4 essais) ;
- progression sur l'ensemble, vitesse lissée ;
- jeton Hugging Face (réglage, sinon `HF_TOKEN`) envoyé aux seuls hôtes
  Hugging Face ;
- refus de la source traduits en phrase qui dit quoi faire (dépôt verrouillé,
  jeton absent ou insuffisant, fichier absent, quota…) ;
- `.part` orphelins vidés au démarrage — par la façade : le Go ne le fait
  plus, il effacerait un téléchargement qu'elle mène.

## Côté Go (`facade_downloads.go`)

Le tunnel du relais et la redistribution E2E arrivent au Go sans passer par
la façade : `withFacadeChat` lui remet les quatre routes, authentifiées par le
jeton interne, comme celles du chat. Façade injoignable : 502
« téléchargements indisponibles : … ».

La façade transmet `LOKI_DOWNLOADS=rust` et son adresse au Go qu'elle lance.

## Écarts assumés

- Une source qui ignore les plages (200 à la sonde) n'est plus lue en entier
  pendant la sonde.
- Erreur de transport (réseau, TLS) au format de reqwest.
- Hôte d'un lien collé en majuscules converti comme les autres.
- Espace libre inconnu hors Unix : rien n'est refusé (comme une mesure ratée
  côté Go).
- `/status` trié par date de début (le Go rendait l'ordre d'une map).

## Parité

`internal/loki/parity_dl_test.go` tire du Go le journal des fonctions pures
(lien normalisé, famille de tranches, `Content-Range`, messages de refus,
hôtes et dépôts Hugging Face, dossier de destination), rangé dans
`rust/loki-rs/tests/parity_models/download.json` ; `tests/downloads.rs` le
rejoue (`LOKI_PARITY_UPDATE=1` pour régénérer). Une mutation
(`replacen` → `replace` sur `/blob/`) fait échouer le rejeu.

`tests/downloads.rs` mène aussi le téléchargement de bout en bout contre un
faux Hugging Face : trois connexions sur des plages disjointes, source sans
plages, tranches avec une déjà présente, dépôt verrouillé refusé dès la sonde,
panne en cours de deuxième tranche, annulation, routes par la façade (clé de
pilotage, remise du Go), routes laissées au Go sans la variable.

## Reste

- Les autres routes des modèles (`/api/models`, suppression, dossiers, liste
  Hugging Face, vision) restent au Go.
- Essai réel sur une installation avant d'en faire le défaut.
