# Lot 42 — Outils web de l'agent dans la façade

## Contexte

La façade tient la conversation par défaut. Les outils web de ses tours
partaient pourtant au Go (`/internal/v1/tool/exec`) :

- `web_search` ;
- `web_images` ;
- `web_open`, `web_read`, `web_grep`.

Le cache des pages (10 min) était donc au Go : `web_open` puis `web_read`
faisaient chacun un aller-retour.

## Le lot

`src/webtools.rs` porte les outils. Sources Go :

- `chat_internet_tools.go` : les quatre outils de lecture et de recherche ;
- `chat_internet.go` : client Crawl4AI, cache, lecture des résultats
  DuckDuckGo ;
- `chat_web_images.go` : recherche d'images.

Ce qui est porté :

- **le cache** :
  - clé : l'URL, suivie de l'empreinte MD5 des options qui changent la page
    (actions, attente, bandeaux), à l'échappement HTML près du Go ;
  - `web_read` et `web_grep` lisent l'ouverture la plus récente d'une URL,
    toutes options confondues ;
  - 10 minutes ;
- **Crawl4AI** :
  - corps envoyé au Go près (clés triées, échappement HTML) ;
  - clé d'accès en `Bearer` ;
  - réponse `{results:[…]}` ou résultat seul, lue comme `json.Unmarshal`
    (types faux, `null`, casse des clés) ;
  - markdown en chaîne ou `{raw_markdown, fit_markdown}` ;
  - erreurs : HTTP (fin du corps), échec du crawl, réponse vide, serveur
    injoignable (texte de l'erreur du client Go) ;
- **les lignes** : `\r\n`, blancs de fin, vides et doublons consécutifs
  réduits ; plan des titres ;
- **la recherche par Crawl4AI** : titres, liens `uddg` décodés deux fois
  comme le Go, entités HTML, extrait tiré des liens qui suivent, doublons et
  liens DuckDuckGo écartés, défi anti-bot ;
- **les images** :
  - jeton `vqd` de la page, puis `i.js` ;
  - mêmes en-têtes ;
  - cookies gardés par hôte, comme le jar du client Go ;
  - réponse lue comme `json.Unmarshal` (erreurs de syntaxe et de type au
    texte du Go) ;
- **`web_grep`** :
  - regex du Go (`(?i)`, classes ASCII) ;
  - erreurs au texte de `regexp` pour les fautes courantes ;
- **le plafond** de 8000 caractères ;
- **les arguments** : nombres tronqués comme `int(float64)` du Go.

Le moteur intégré lit le HTML avec go-readability, html-to-markdown et le
parseur HTML5 de x/net : rien en Rust ne les reproduit au caractère près.
Ces deux étapes restent au Go, derrière deux appels sans état :

- `POST /internal/v1/web/fetch` `{url}` → `{markdown}` ou `{error}`
  (`goFetchMarkdown`) ;
- `POST /internal/v1/web/search` `{query, limit}` → `{results}` ou `{error}`
  (`goSearch`).

`ChatShared.web` porte les outils ; `GoTurnHost::exec_tool` les exécute
avant tout le reste. `web_screenshot` reste au Go (navigateur piloté).

Le traducteur des regex du Go (`go_regex`) refuse désormais :

- la répétition d'une répétition (`a**`, `a*??`, `a{2}*`) ;
- un compte au-delà de 1000 ou inversé.

Un `{` qui n'ouvre pas de répétition y devient littéral, comme au Go. Les
motifs `retry_patterns` en profitent.

Côté Go, pour le journal :

- `webNow` : l'horloge du cache ;
- `ddgImagesBase` et `ddgHTMLBase` : les adresses de DuckDuckGo.

## Écart connu

reqwest pose `Accept: */*` sur une requête qui n'en a pas ; le client Go
n'en pose aucun. Seule la page de recherche d'images est concernée, sans
effet sur DuckDuckGo.

## Parité

`internal/loki/parity_web_tools_test.go` tire du Go un journal de 74
appels (`tests/parity_web/outils.json`). Chaque appel porte :

- le résultat ;
- les requêtes reçues par le faux Crawl4AI (méthode, clé, type, corps) ;
- les requêtes reçues par le faux DuckDuckGo (chemin, en-têtes, cookies) ;
- les pages et recherches demandées au moteur intégré.

Il couvre :

- recherche : limites, anti-bot, aucun résultat, panne ;
- lecture : plages, bornes, au-delà, page jamais ouverte ;
- grep : contexte fusionné, plafond, sans résultat, classes ASCII, sept
  regex invalides ;
- cache : réouverture, forcée, options, la plus récente, péremption ;
- GitHub ;
- les réponses de Crawl4AI ;
- serveur injoignable, sans serveur, sans clé ;
- moteur intégré : ouverture, texte brut, page absente, recherche ;
- images : doublons, cookie gardé, jeton absent, pannes, réponses
  illisibles ou mal typées, `null`.

`tests/webtools.rs` le rejoue contre le port. Le faux Go sert ce que le
moteur intégré a tiré des pages.

D'autres tests complètent le journal :

- `tests/lot6.rs` (`outil_web_par_la_facade`) : un tour de la façade ouvre
  une page sans passer par `/tool/exec` ;
- `TestInternalMoteurWebIntegre` : les deux appels internes du Go ;
- tests unitaires : MD5, clés du cache, URL, entités, nombres, erreurs de
  regex, réponses de Crawl4AI.
