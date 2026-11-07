// Clé de pilotage : envoyée en Authorization: Bearer sur chaque appel /api/*.
// Le navigateur l'obtient avec le MOT DE PASSE (web_password.go) et la garde en
// localStorage ; sur un 401 de la garde, on redemande le mot de passe.
let TOKEN = localStorage.getItem('loki.key') || '';
function authHeaders(h){ h = Object.assign({}, h||{}); if(TOKEN) h['Authorization']='Bearer '+TOKEN; return h; }
// Base d'URL : "" en local (page à "/"), "/u/<id>" servie via le relais ajean.link.
// Garde les chemins absolus (/api/…) corrects derrière le tunnel.
const API_BASE = location.pathname.replace(/\/(index\.html)?$/, '');
// Délai maximal d'un appel /api/* ordinaire. Sans lui, une requête que le
// navigateur met en file d'attente (plafond de ~6 connexions par domaine, atteint
// dès qu'on laisse traîner plusieurs onglets Loki) reste suspendue POUR TOUJOURS :
// ni réponse, ni erreur, et une UI figée sur « chargement… » sans rien à afficher.
// Mieux vaut une erreur franche. Les flux longs (SSE) passent leur propre signal
// et ne sont donc jamais concernés.
const API_TIMEOUT_MS = 30000;
async function jfetch(u, opts){
  opts = opts || {};
  // Tant que la connexion n'est pas faite, les appels attendent : au chargement,
  // une dizaine partent en même temps et ne doivent pas tous échouer.
  await authReady();
  opts.headers = authHeaders(opts.headers);
  if(u.charAt(0) === '/') u = API_BASE + u;
  let timer=null;
  if(!opts.signal){
    const ac=new AbortController();
    opts.signal=ac.signal;
    timer=setTimeout(()=>ac.abort(), API_TIMEOUT_MS);
  }
  let r;
  try{ r = await fetch(u, opts); }
  catch(e){
    if(e && e.name==='AbortError'){ throw new Error('requête sans réponse après 30 s (trop d\'onglets Loki ouverts ?)'); }
    throw e;
  }
  finally{ if(timer) clearTimeout(timer); }
  // 401 de la GARDE (porte WWW-Authenticate) : clé changée ou absente, on
  // redemande le mot de passe. Un 401 sans cet en-tête vient d'une route qui
  // refuse un secret (mémoire verrouillée…) : rien à voir avec la connexion.
  if(r.status === 401 && r.headers.get('WWW-Authenticate')){
    if(await authOnce()){ opts.headers = authHeaders(opts.headers); r = await fetch(u, opts); }
  }
  // 403 de la garde anti-site-tiers (web_auth.go, crossSiteReject) : sans ce
  // bandeau, l'interface restait vide sans dire pourquoi — chaque appel échouait
  // en silence dans la console.
  if(r.status === 403 && !accessBlockedShown){
    r.clone().json().then(j=>{ if(j && j.error) showAccessBlocked(j.error); }).catch(()=>{});
  }
  return r;
}

// showAccessBlocked : bandeau fixe expliquant le refus d'accès et comment le
// lever (nom de domaine derrière un reverse proxy, sans clé de pilotage).
let accessBlockedShown = false;
function showAccessBlocked(msg){
  if(accessBlockedShown) return;
  accessBlockedShown = true;
  const d = document.createElement('div');
  d.id = 'access-blocked';
  d.setAttribute('role', 'alert');
  d.style.cssText = 'position:fixed;top:0;left:0;right:0;z-index:9999;padding:12px 16px;background:var(--err,#b3564d);color:#fff;font-size:13px;line-height:1.5';
  const host = location.hostname;
  d.innerHTML = '<b>Accès refusé par Loki</b> — ' + escHtml(msg) +
    '<br>Choisis un mot de passe (<code>docker exec -it loki loki password</code>), ou, derrière un reverse proxy, ajoute <code>LOKI_TRUSTED_HOSTS=' + escHtml(host) +
    '</code> aux variables du conteneur puis recrée-le.';
  document.body.appendChild(d);
}
// authOnce : UNE seule demande de mot de passe à la fois, partagée par tous les
// appels (sinon chaque 401 rouvrait la modale et effaçait la saisie en cours).
// Renvoie true quand le navigateur a une clé valable.
let _authAsk = null, _authReady = null;
function authReady(){ if(!_authReady) _authReady = authOnce(); return _authReady; }
function authOnce(){
  if(!_authAsk) _authAsk = runAuth().finally(()=>{ _authAsk = null; });
  return _authAsk;
}
async function authPost(path, password){
  const r = await fetch(API_BASE + path, {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({password})});
  let j = {}; try{ j = await r.json(); }catch(e){}
  if(r.ok && j.token){ TOKEN = j.token; localStorage.setItem('loki.key', TOKEN); }
  return {status:r.status, error:j.error||('erreur '+r.status)};
}
async function runAuth(){
  // Réponse inattendue (serveur injoignable, accès par le relais ajean.link qui
  // ne laisse passer que /api/e2e/*…) : on ne demande rien, les appels suivent
  // leur cours comme avant. Seuls « setup » et « login » ouvrent la modale.
  let st;
  try{
    const r = await fetch(API_BASE + '/api/auth/state', {headers:authHeaders()});
    if(r.ok) st = (await r.json()).state;
  }catch(e){}
  if(st !== 'setup' && st !== 'login') return true;
  // Premier démarrage : choisir le mot de passe (deux saisies identiques).
  while(st === 'setup'){
    const a = await askPrompt('Choisis le mot de passe de cette interface (8 caractères minimum). Il te sera demandé sur chaque appareil.', {title:'Bienvenue dans Loki', password:'new', placeholder:'mot de passe', okText:'Continuer'});
    if(a === null) return false;
    if(a.length < 8){ await askAlert('8 caractères minimum.', {title:'Trop court'}); continue; }
    const b = await askPrompt('Retape-le pour confirmer.', {title:'Bienvenue dans Loki', password:'new', placeholder:'mot de passe', okText:'Enregistrer'});
    if(b === null) return false;
    if(a !== b){ await askAlert('Les deux saisies diffèrent, recommence.', {title:'Mot de passe'}); continue; }
    const r = await authPost('/api/auth/setup', a);
    if(r.status === 200) return true;
    if(r.status === 409){ st = 'login'; break; } // choisi entre-temps (autre onglet)
    await askAlert(r.error, {title:'Mot de passe'});
  }
  while(true){
    const p = await askPrompt('Mot de passe :', {title:'Connexion à Loki', password:'current', placeholder:'mot de passe', okText:'Se connecter'});
    if(p === null) return false;
    const r = await authPost('/api/auth/login', p);
    if(r.status === 200) return true;
    await askAlert(r.status === 401 ? 'Mot de passe incorrect. Oublié ? docker exec -it loki loki password' : r.error, {title:'Connexion refusée'});
  }
}
async function jget(u){ const r=await jfetch(u); return r.json(); }
async function jpost(u,b){ const r=await jfetch(u,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(b||{})}); return r.json(); }
