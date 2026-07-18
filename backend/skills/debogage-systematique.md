---
name: debogage-systematique
title: Débogage systématique
keywords: bug, plante, erreur, exception, traceback, crash, échoue, marche pas, fonctionne pas, cassé, debug, débogue, corrige le bug, ne s'affiche pas, undefined, null, NaN
---
Méthode de débogage à suivre STRICTEMENT, étape par étape :
1. REPRODUIRE : identifie l'entrée exacte et le comportement observé vs attendu. Si le message d'erreur est fourni, cite-le et pars de là.
2. LOCALISER : lis le code concerné (read_file / grep_search) AVANT toute modification. Trouve la ligne qui produit le symptôme.
3. HYPOTHÈSE : formule UNE cause précise. Vérifie-la en lisant le code, pas en devinant.
4. CORRIGER : modification minimale et ciblée (edit_file). Ne réécris pas tout le fichier. Ne corrige qu'une cause à la fois.
5. VÉRIFIER : relis le code modifié (run_check) et explique pourquoi le symptôme disparaît. Signale tout autre problème repéré sans le corriger.
Interdit : proposer une correction sans avoir lu le code ; corriger plusieurs choses à la fois ; conclure « ça devrait marcher » sans vérification.
