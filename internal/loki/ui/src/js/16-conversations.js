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

async function loadConversations(){
  let r;
  try{ r = await jget('/api/conversations'); }catch(_){ return; }
  const cont = document.getElementById('conv-list');
  if(!cont) return;
  cont.innerHTML = '';
  const list = r.conversations || [];
  if(!list.length){ cont.innerHTML = '<span class="muted">(aucune)</span>'; return; }
  for(const c of list){
    const row = document.createElement('div');
    row.className = 'preset' + (c.id === r.active ? ' active' : '');
    row.onclick = () => convSwitch(c.id);

    const info = document.createElement('div'); info.className = 'preset-info';
    const nm = document.createElement('div'); nm.className = 'preset-name';
    if(c.id === r.active){ const d = document.createElement('i'); d.className = 'preset-dot'; nm.appendChild(d); }
    const title = c.title || 'Nouvelle discussion';
    nm.appendChild(document.createTextNode(title));
    nm.title = title;
    const meta = document.createElement('div'); meta.className = 'preset-meta';
    const when = document.createElement('span'); when.className = 'muted';
    when.textContent = convWhen(c.updated) + (c.turns ? ' · ' + c.turns + ' échange' + (c.turns > 1 ? 's' : '') : '');
    meta.appendChild(when);
    info.appendChild(nm); info.appendChild(meta);

    // Renommer / supprimer : stopPropagation, sinon le clic bascule aussi.
    const acts = document.createElement('div'); acts.className = 'conv-acts';
    const ren = document.createElement('button'); ren.className = 'iconbtn'; ren.textContent = '✎';
    ren.title = 'renommer'; ren.onclick = e => { e.stopPropagation(); convRename(c.id, title); };
    const del = document.createElement('button'); del.className = 'iconbtn'; del.textContent = '🗑';
    del.title = 'supprimer'; del.onclick = e => { e.stopPropagation(); convDelete(c.id, title); };
    acts.appendChild(ren); acts.appendChild(del);

    row.appendChild(info); row.appendChild(acts);
    cont.appendChild(row);
  }
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
