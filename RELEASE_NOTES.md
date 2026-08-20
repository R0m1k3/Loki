# Loki 0.12.0

Le sélecteur de modèle devient un vrai sélecteur, la dictée vocale arrive, et
les cartes de raisonnement suivent l'écriture au lieu d'envahir la page.

## Le sélecteur de l'en-tête charge vraiment un modèle

Deux groupes : « Presets » et « Modèles (.gguf) » — tous les fichiers de
`/models` et `/data/models`. Choisir un modèle le charge (seul `MODEL` change,
contexte/NGL/échantillonnage conservés) et redémarre le moteur. Sans preset
créé, le sélecteur n'offrait aucun choix.

## Dictée vocale

Un bouton micro dans la carte de saisie : tu parles, le texte s'écrit. La
transcription est 100 % locale — whisper.cpp compilé dans l'image, modèle
multilingue téléchargé au premier usage (~190 Mo, dans `/data/whisper/`).
Le navigateur exige HTTPS (ou localhost) pour donner accès au micro.

## Cartes raisonnement/outils : taille fixe, défilement qui suit

Hauteur bornée (280 px) avec défilement interne collé au texte pendant la
génération — remonter lire le début est respecté. Les cartes d'outil suivent
la ligne en cours d'écriture puis remontent en tête une fois l'appel terminé.
Sur un raisonnement géant, seul le bas du bloc est re-rendu en direct (le
texte complet est posé à la fin) : l'affichage ne se fige plus.

## Mesures

- La carte du temps total affiche la **vitesse moyenne de la conversation**
  (« en cours — 12 s · moy 21.3 tok/s »), rejouée au rechargement.
- Le **pourcentage de chargement** du modèle bouge aussi sur un redémarrage à
  chaud (modèle déjà dans le cache disque : on suit la mémoire résidente, plus
  seulement les octets lus).

## Interface

- Discussions triées par date de **création** (récentes en tête) : une
  discussion garde sa place, écrire dans un vieux fil ne le fait plus remonter.
- Bouton **Réglages** ancré en pied de barre latérale, au-dessus du moniteur
  Performance.

## Rappel 0.11.0

Réglages en modale à deux volets ; nom du modèle sur chaque réponse
(journalisé, il survit au rechargement) ; jeton Hugging Face réglable dans
l'interface ; tâches planifiées reprises de l'amont.

## Mise à jour

```
docker compose pull && docker compose up -d
```
