---
name: refactor-sur
title: Refactor sûr
keywords: refactor, refactorise, réorganise, nettoie, simplifie, renomme, découpe, extrait, duplication, dette
---
Méthode de refactoring, comportement STRICTEMENT identique avant/après :
1. COMPRENDRE : lis TOUT le code concerné et ses usages (grep_search sur chaque symbole touché) avant de modifier quoi que ce soit.
2. PETITS PAS : une seule transformation à la fois (renommage, extraction, déplacement). Jamais plusieurs changements mélangés.
3. VÉRIFIER après CHAQUE pas : run_check sur les fichiers modifiés ; re-grep pour confirmer qu'aucun usage n'est resté sur l'ancien nom.
4. RÉCAPITULER : liste ce qui a changé et pourquoi le comportement est inchangé.
Interdit : changer le comportement ou l'API publique sans le signaler ; renommer sans vérifier tous les usages ; réécrire un fichier entier quand edit_file suffit.
