// Projets — un chantier = une mémoire + ses discussions + ses trackers.
//
// Deux surfaces, volontairement asymétriques :
//   - le SÉLECTEUR de la barre latérale, qu'on utilise dix fois par jour : il ne
//     fait qu'une chose, changer de projet ;
//   - le PANNEAU de réglages, qu'on ouvre rarement : créer, renommer, décrire,
//     supprimer.
//
// Changer de projet change ce que voit l'IA (mémoire, trackers) ET la liste des
// discussions : le serveur ouvre une discussion du projet d'arrivée, et le flux
// SSE redessine le chat tout seul — comme une bascule de discussion.

let PROJECTS = [], PROJ_ACTIVE = '';

async function loadProjects(){
  let r;
  try{ r = await jget('/api/projects'); }catch(_){ return; }
  if(!r || !r.ok) return;
  PROJECTS = r.projects || [];
  PROJ_ACTIVE = r.active || '';
  renderProjectPicker();
  renderProjectList();
}

// Le sélecteur de la barre latérale. Reconstruit à chaque chargement : la liste
// est courte, et un rendu complet évite de synchroniser des options une par une.
function renderProjectPicker(){
  const sel = document.getElementById('proj-switch');
  if(!sel) return;
  sel.textContent = '';
  PROJECTS.forEach(p => {
    const o = document.createElement('option');
    o.value = p.slug;
    o.textContent = p.name;
    sel.appendChild(o);
  });
  sel.value = PROJ_ACTIVE;
}

async function onProjectSwitch(sel){
  const slug = sel.value;
  if(!slug || slug === PROJ_ACTIVE) return;
  const r = await jpost('/api/projects', {action:'switch', slug});
  if(!r.ok){
    // Refusé (génération en cours) : on remet le sélecteur sur le projet réel,
    // sinon il affiche un projet où l'on n'est pas.
    sel.value = PROJ_ACTIVE;
    toast('erreur : ' + (r.error||''));
    return;
  }
  PROJECTS = r.projects || PROJECTS;
  PROJ_ACTIVE = r.active || PROJ_ACTIVE;
  renderProjectPicker();
  renderProjectList();
  // La mémoire, les trackers et les discussions appartiennent au projet : tout
  // ce qui est à l'écran parle du précédent.
  if(typeof loadConversations === 'function') loadConversations();
  if(typeof loadAgent === 'function') loadAgent();
  if(typeof loadTrackers === 'function') loadTrackers();
}

// Le panneau de gestion. Une ligne par projet, avec ce qu'il contient — c'est ce
// qui permet de décider quoi supprimer sans ouvrir chaque projet.
function renderProjectList(){
  const list = document.getElementById('proj-list');
  if(!list) return;
  list.textContent = '';
  PROJECTS.forEach(p => {
    const row = document.createElement('div');
    row.className = 'preset';
    row.style.fontSize = '12px';

    const span = document.createElement('span');
    const b = document.createElement('b');
    b.style.color = 'var(--text)';
    b.textContent = p.name;
    span.appendChild(b);
    if(p.active){
      const a = document.createElement('span');
      a.className = 'sbadge on';
      a.textContent = 'actif';
      a.style.marginLeft = '6px';
      span.appendChild(a);
    }
    const d = document.createElement('span');
    d.className = 'muted';
    d.textContent = ' — ' + p.convs + ' discussion' + (p.convs > 1 ? 's' : '')
                  + ' · ' + p.pages + ' page' + (p.pages > 1 ? 's' : '')
                  + ' · ' + p.trackers + ' tracker' + (p.trackers > 1 ? 's' : '');
    span.appendChild(d);
    if(p.desc){
      const desc = document.createElement('div');
      desc.className = 'muted';
      desc.style.cssText = 'font-size:11px;margin-top:2px';
      desc.textContent = p.desc;
      span.appendChild(desc);
    }

    const acts = document.createElement('span');
    acts.style.cssText = 'display:flex;gap:4px;flex:none';
    acts.appendChild(projBtn('décrire', () => editProjectDesc(p)));
    acts.appendChild(projBtn('renommer', () => renameProjectUI(p)));
    // Le dernier projet n'est pas supprimable côté serveur : ne pas proposer le
    // bouton évite une erreur qu'on ne peut pas résoudre.
    if(PROJECTS.length > 1) acts.appendChild(projBtn('supprimer', () => deleteProjectUI(p)));

    row.appendChild(span);
    row.appendChild(acts);
    list.appendChild(row);
  });
}

function projBtn(label, fn){
  const b = document.createElement('button');
  b.textContent = label;
  b.style.cssText = 'margin:0;padding:2px 8px;font-size:11px';
  b.onclick = e => { e.stopPropagation(); fn(); };
  return b;
}

// Applique une action et rafraîchit tout ce qui dépend des projets.
async function projectAction(payload, okMsg){
  const r = await jpost('/api/projects', payload);
  if(!r.ok){ toast('erreur : ' + (r.error||'')); return false; }
  PROJECTS = r.projects || PROJECTS;
  PROJ_ACTIVE = r.active || PROJ_ACTIVE;
  renderProjectPicker();
  renderProjectList();
  if(okMsg) toast(okMsg);
  return true;
}

async function newProject(){
  const name = await askPrompt('Nom du projet (ex. « Serveur NAS », « Recettes ») :', {title:'Nouveau projet', okText:'Créer'});
  if(!name || !name.trim()) return;
  await projectAction({action:'create', name:name.trim()}, 'projet créé');
}

async function renameProjectUI(p){
  const name = await askPrompt('Nouveau nom :', {title:'Renommer le projet', default:p.name, okText:'Renommer'});
  if(!name || !name.trim() || name.trim() === p.name) return;
  // Seul le libellé change : les chemins disque, les discussions et les trackers
  // restent accrochés au slug d'origine.
  await projectAction({action:'rename', slug:p.slug, name:name.trim()}, 'renommé');
}

async function editProjectDesc(p){
  const desc = await askPrompt(
    'À quoi sert ce projet ? Ce texte est donné à l\'IA au début de chaque discussion du projet.',
    {title:'Description du projet', default:p.desc||'', placeholder:'ex. administration du NAS Unraid, docker, sauvegardes', okText:'Enregistrer'});
  if(desc === null) return;
  await projectAction({action:'desc', slug:p.slug, desc}, 'description enregistrée');
}

async function deleteProjectUI(p){
  const ok = await askConfirm(
    'Supprimer « ' + p.name + ' » ?\n\nSes ' + p.pages + ' page(s) mémoire, ses '
    + p.convs + ' discussion(s) et ses ' + p.trackers + ' tracker(s) seront effacés. Irréversible.',
    {title:'Supprimer le projet', okText:'Supprimer', danger:true});
  if(!ok) return;
  await projectAction({action:'delete', slug:p.slug}, 'projet supprimé');
  if(typeof loadConversations === 'function') loadConversations();
  if(typeof loadAgent === 'function') loadAgent();
  if(typeof loadTrackers === 'function') loadTrackers();
}

// Déplacer une page mémoire vers un autre projet — appelé depuis la liste des
// pages (06-settings.js). Une page rangée dans le mauvais chantier n'a pas à
// être recopiée à la main pour en changer.
async function moveMemPageUI(name){
  const others = PROJECTS.filter(p => p.slug !== PROJ_ACTIVE);
  if(!others.length){ toast('aucun autre projet où déplacer'); return; }
  const choice = await askPrompt(
    'Déplacer « ' + name + ' » vers quel projet ?\n' + others.map(p => '· ' + p.name).join('\n'),
    {title:'Déplacer la page', default:others[0].name, okText:'Déplacer'});
  if(!choice) return;
  const target = others.find(p => p.name.toLowerCase() === choice.trim().toLowerCase());
  if(!target){ toast('projet inconnu : ' + choice); return; }
  const r = await jpost('/api/mem/move', {name, to:target.slug});
  if(!r.ok){ toast('erreur : ' + (r.error||'')); return; }
  toast('déplacée vers ' + target.name);
  if(typeof loadAgent === 'function') loadAgent();
  loadProjects();
}
