# Loki 0.11.0

Première version numérotée du fork depuis la synchronisation avec l'amont
AJEAN (v0.10.7). L'essentiel de cette version : une interface qui respire.

## Réglages en modale

Tous les réglages quittent la barre latérale pour une fenêtre à deux volets :
la nav des sections à gauche (IA, moteur, application), le panneau choisi à
droite — chaque section a enfin toute la largeur. La barre latérale ne garde
que les discussions et le moniteur machine. Les chemins historiques (pastille
d'état du moteur, pastille d'installation llama.cpp) rouvrent la modale au bon
endroit.

## Discussions : la plus récente en tête

La liste se lit en ordre d'activité, la discussion du moment en haut — y
compris une discussion toute neuve, qui apparaissait sous celle qu'on venait de
quitter.

## Nom du modèle sur chaque réponse

Une pastille à côté de « Loki » dit quel modèle a produit la réponse. Le nom
est journalisé avec le tour : il survit au rechargement de page, et un vieux
tour garde le modèle de l'époque, pas celui chargé aujourd'hui.

## Cartes de raisonnement à hauteur bornée

Un long raisonnement (plusieurs milliers de tokens) faisait grandir la page de
plusieurs écrans et finissait par figer l'affichage (re-parse Markdown du bloc
entier à chaque token, en O(n²)). La carte reste maintenant à taille fixe et
suit la génération en défilant toute seule ; en direct, seul le bas d'un bloc
géant est re-rendu, et le texte complet est posé en fin de bloc.

## Divers

- Jeton Hugging Face réglable dans l'interface (dépôts verrouillés : le
  message dit lesquels, et pourquoi le 401).
- Tâches planifiées reprises de l'amont : l'IA travaille toute seule sur une
  fréquence réglable, isolée des discussions.

## Mise à jour

```
docker compose pull && docker compose up -d
```
