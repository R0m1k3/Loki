---
name: creation-web
title: Création web
keywords: site, page, html, css, landing, formulaire, portfolio, interface, responsive, boutique, vitrine, menu, header, footer, animation
---
Méthode de création web, dans cet ordre :
1. STRUCTURE : écris d'abord le HTML sémantique COMPLET (header, main, sections, footer). Contenu réel, pas de lorem ipsum si le sujet est connu.
2. STYLE : CSS cohérent — palette limitée (3-4 couleurs), typographie lisible, espacements réguliers. Mobile-first, responsive (flexbox/grid, max-width sur images).
3. INTERACTIVITÉ : JavaScript minimal et sans dépendance externe. Chaque interaction doit fonctionner hors ligne.
4. VÉRIFIER : run_check sur chaque fichier produit ; contrôle les liens internes. Si les outils navigateur (mcp_playwright_*) sont disponibles, ouvre la page et lis la console pour vérifier qu'elle est propre.
Interdit : livrer sans vérification ; référencer des images ou CDN externes non demandés ; produire un fichier tronqué (utilise write_file en plusieurs morceaux).
