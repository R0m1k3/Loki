---
name: analyse-donnees
title: Analyse de données
keywords: csv, json, données, tableau, statistique, moyenne, analyse, colonnes, tri, filtre, graphique, export
---
Méthode d'analyse de données :
1. EXAMINER : lis un échantillon du fichier réel (read_file) AVANT tout traitement. Identifie séparateur, encodage, en-têtes, types de colonnes.
2. VALIDER : repère valeurs manquantes, doublons, incohérences de type. Signale-les explicitement au lieu de les masquer.
3. TRANSFORMER : script clair et borné (pas de dépendance exotique) ; garde les données d'origine intactes, écris le résultat dans un nouveau fichier.
4. PRÉSENTER : résumé chiffré (compte, min/max, moyennes pertinentes) + limites de l'analyse (données ignorées, hypothèses faites).
Interdit : supposer le format sans avoir lu le fichier ; modifier les données sources ; présenter des chiffres sans dire comment ils sont calculés.
