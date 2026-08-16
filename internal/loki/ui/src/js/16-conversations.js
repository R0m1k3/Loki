// Discussions — plusieurs fils au lieu de la conversation unique d'origine.
//
// Le fil actif est possédé par le SERVEUR et partagé par tous les appareils :
// basculer ici bascule partout. Le changement arrive donc par le flux SSE (un
// événement `reset`, comme pour « vider »), et c'est lui qui redessine le chat —
// on ne fait que rafraîchir la liste.
//
// Les titres viennent du premier message de l'utilisateur : ils sont construits
// en DOM (jamais en innerHTML) pour qu'un titre contenant < > & " ne casse ni le
// balisage ni les gestionnaires d'événement.
//
// La liste chargée est GARDÉE en mémoire (CONVS) pour que la recherche filtre
// sans aller-retour serveur : elle ne porte que des métadonnées — identifiant,
// titre, dates, nombre d'échanges — jamais les messages.

let CONVS = [], CONV_ACTIVE = '';

async function loadConversations(){
  let r;
  try{ r = await jget('/api/conversations'); }catch(_){ return; }
  CONVS = r.conversations || [];
  CONV_ACTIVE = r.active || '';
  renderConversations();
}

function renderConversations(){
  const cont = document.getElementById('conv-list');
  if(!cont) return;
  const q = (document.getElementById('conv-search')?.value || '').trim().toLowerCase();
  cont.textContent = '';
  const list = q
    ? CONVS.filter(c => (c.title || 'Nouvelle discussion').toLowerCase().includes(q))
    : CONVS;
  if(!list.length){
    const e = document.createElement('div');
    e.className = 'conv-empty';
    e.textContent = q ? 'aucune discussion ne correspond' : '(aucune)';
    cont.appendChild(e);
    syncTopbarTitle();
    return;
  }
  for(const c of list){
    const row = document.createElement('div');
    row.className = 'preset' + (c.id === CONV_ACTIVE ? ' active' : '');
    row.onclick = () => convSwitch(c.id);

    const info = document.createElement('div'); info.className = 'preset-info';
    const nm = document.createElement('div'); nm.className = 'preset-name';
    const title = c.title || 'Nouvelle discussion';
    nm.appendChild(document.createTextNode(title));
    nm.title = title + (c.turns ? ' · ' + c.turns + ' échange' + (c.turns > 1 ? 's' : '') : '');
    info.appendChild(nm);

    // Heure à droite du titre, sur la même ligne (maquette). Elle s'efface au
    // survol : renommer et supprimer prennent sa place, la ligne ne s'allonge
    // donc jamais et rien ne se chevauche.
    const when = document.createElement('span');
    when.className = 'conv-when';
    when.textContent = convWhen(c.updated);

    // Renommer / supprimer : stopPropagation, sinon le clic bascule aussi.
    const acts = document.createElement('div'); acts.className = 'conv-acts';
    const ren = document.createElement('button'); ren.className = 'iconbtn';
    ren.appendChild(icon('pencil', 15)); ren.title = 'renommer';
    ren.setAttribute('aria-label', 'renommer la discussion');
    ren.onclick = e => { e.stopPropagation(); convRename(c.id, title); };
    const del = document.createElement('button'); del.className = 'iconbtn';
    del.appendChild(icon('trash', 15)); del.title = 'supprimer';
    del.setAttribute('aria-label', 'supprimer la discussion');
    del.onclick = e => { e.stopPropagation(); convDelete(c.id, title); };
    acts.appendChild(ren); acts.appendChild(del);

    row.append(info, when, acts);
    cont.appendChild(row);
  }
  syncTopbarTitle();
}

// Titre de l'en-tête : celui de la discussion ouverte. Il dit de quoi on parle
// quand la barre latérale est escamotée — c'est-à-dire quand on en a besoin.
function syncTopbarTitle(){
  const el = document.getElementById('topbar-title');
  if(!el) return;
  const cur = CONVS.find(c => c.id === CONV_ACTIVE);
  const t = (cur && cur.title) || 'Nouvelle discussion';
  el.textContent = t;
  el.title = t;
}

// Date courte : l'heure pour aujourd'hui, la date sinon — une liste de
// discussions se lit d'un coup d'œil, pas au timestamp complet.
function convWhen(unix){
  if(!unix) return '';
  const d = new Date(unix * 1000), now = new Date();
  const sameDay = d.toDateString() === now.toDateString();
  return sameDay
    ? d.toLocaleTimeString([], {hour:'2-digit', minute:'2-digit'})
    : d.toLocaleDateString([], {day:'2-digit', month:'2-digit'});
}

async function convNew(){
  try{
    await jpost('/api/conversations/new', {});
    toast('nouvelle discussion');
    const s = document.getElementById('conv-search');
    if(s) s.value = ''; // sinon la discussion toute neuve est masquée par le filtre
    loadConversations();
  }catch(_){ toast('erreur réseau'); }
}

async function convSwitch(id){
  try{
    const r = await jpost('/api/conversations/switch', {id});
    if(r && r.ok === false){ toast('erreur : ' + (r.error || '')); return; }
    loadConversations();
  }catch(_){ toast('erreur réseau'); }
}

async function convRename(id, current){
  const t = await askPrompt('Nouveau titre de la discussion', {title:'Renommer', default:current, okText:'Renommer'});
  if(!t) return;
  try{
    const r = await jpost('/api/conversations/rename', {id, title:t});
    if(r && r.ok === false){ toast('erreur : ' + (r.error || '')); return; }
    loadConversations();
  }catch(_){ toast('erreur réseau'); }
}

async function convDelete(id, title){
  // Le geste emporte aussi les fichiers de la discussion : ils lui appartiennent
  // (un dossier par discussion côté serveur), autant le dire avant.
  if(!await askConfirm('Supprimer « ' + title + ' » ? Son historique ET ses fichiers (pièces jointes, captures, fichiers écrits par l\'agent) seront perdus.',
                       {title:'Supprimer la discussion', okText:'Supprimer', danger:true})) return;
  try{
    const r = await jpost('/api/conversations/delete', {id});
    if(r && r.ok === false){ toast('erreur : ' + (r.error || '')); return; }
    loadConversations();
  }catch(_){ toast('erreur réseau'); }
}
