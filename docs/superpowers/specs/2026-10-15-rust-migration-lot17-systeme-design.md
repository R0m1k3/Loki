# Lot 17 — interrupteurs système tenus par la façade

## Contexte

Quatre réglages de l'interface vivaient dans le Go :
- l'accès internet de l'IA : `handleInternet`, `chat_internet.go`,
  `web_fetch_go.go` (moteur web, serveur Crawl4AI, clé, joignabilité) ;
- le pilotage du navigateur : `handleComputer`, `chromePath`,
  `visionEnabled` ;
- la clé de complétion du moteur : `handleAPIKey`, `backend_config.go`,
  `oaiBaseURL` ;
- l'écoute réseau du moteur : `handleNetwork`, `sys_network.go`.

Lot 17 : la façade tient ces routes quand `LOKI_SYSTEM=rust` (`system.rs`,
expérimental). Sans la variable, rien ne change.

| Route | Rôle |
|---|---|
| `GET\|POST /api/internet` | interrupteur, moteur web (`go` ou `crawl4ai`), URL et clé du serveur Crawl4AI (la clé n'est jamais rendue, seulement ses quatre derniers caractères), joignabilité |
| `GET\|POST /api/computer` | interrupteur du navigateur, binaire trouvé, vision du modèle |
| `GET\|POST /api/apikey` | clé de complétion : génération, saisie, effacement ; adresse `/v1` à coller dans un client tiers |
| `GET\|POST /api/network` | moteur ouvert au réseau ou à cette machine seulement (`HOST`), adresse de l'endpoint |

Les réglages restent des clés de la base, écrites comme le Go : une chaîne
vide efface la clé, et l'URL comme les clés ne laissent rien traîner dans la
configuration.

La joignabilité de Crawl4AI suit `crawlReachable` :
- `/health`, puis `/`, en moins de deux secondes en tout ;
- clé envoyée en `Authorization: Bearer` ;
- moins de 500 = joignable ;
- cache de 30 s par URL, vidé quand l'URL, la clé ou le moteur changent.

## Restent au Go

Le Go expose trois routes internes (`facade_system.go`) :
- `POST /internal/v1/web/reach/reset` : le cache de joignabilité des outils
  web, encore exécutés par le Go, est vidé à chaque changement ;
- `POST /internal/v1/cdp/shutdown` : le Chromium des outils `browser_*` se
  ferme quand l'interrupteur se coupe ;
- `POST /internal/v1/service/action {action, if_active}` : une clé neuve ne
  s'applique qu'au redémarrage du moteur. La façade le relance elle-même si
  elle le pilote (lot 16), sinon le Go, seulement s'il tourne.

`GET /internal/v1/tune/guard` (lot 15) refuse un changement de clé pendant une
optimisation.

Le relais du tunnel vers la façade garde désormais l'hôte et
`X-Forwarded-Proto` du client : l'adresse de la clé de complétion est celle
que le client a tapée, pas celle de la façade.

## Correction en passant

`maskAPIKey` paniquait sur une clé d'un seul octet (`k[len(k)-2:]`).
`/api/apikey` tombait alors à chaque lecture tant que la clé restait en place.
Une clé d'un ou deux octets s'affiche maintenant « … », côté Go comme côté
Rust.

## Écarts assumés

- Hors Windows, aucun pare-feu n'est piloté : la façade ne tourne qu'en
  conteneur Linux, où le Go répondait déjà « inconnu ».
- La clé masquée est coupée sur les octets, comme le Go. Un caractère coupé
  devient U+FFFD, octet par octet, comme son encodeur JSON.

## Parité

`internal/loki/parity_system_test.go` tire du Go 61 appels de route, avec
après chacun les clés touchées en base et les en-têtes reçus par trois faux
serveurs Crawl4AI (200, 500, 404). Ils couvrent :
- moteurs inconnus, URL avec et sans slashs, injoignable, sans schéma ;
- clés longues, courtes, d'un caractère, résiduelle en configuration ;
- erreurs de type qui laissent passer les autres champs, `null` ;
- vision par projecteur, preset externe ou Strata ;
- clé générée (masquée dans le journal), saisie, de treize octets, repli sur
  `config.env`, adresse publique, `X-Forwarded-Proto`, refus pendant une
  optimisation ;
- écoute ouverte, fermée, `localhost`, `::1`, port illisible.

Il tire aussi huit dispositions du dossier de Playwright pour le Chromium
retenu.

`tests/system.rs` rejoue le tout par une seule façade, cache de
joignabilité compris. Il vérifie de bout en bout ce qui reste au Go : cache
vidé, navigateur fermé, moteur relancé par le Go, ou par le superviseur
quand la façade pilote le moteur. Une mutation de chaque règle fait échouer
les tests.

Côté Go :
- `TestFacadeSysteme` vérifie le relais et les trois routes internes ;
- `TestFacadeRelaisGardeHote` vérifie l'hôte gardé ;
- `TestMaskAPIKeyCourte` vérifie le masque des clés courtes.
