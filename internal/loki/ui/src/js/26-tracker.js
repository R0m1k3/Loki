// Trackers — le 3ᵉ type de mémoire : les données DATÉES qui s'accumulent
// (compteurs, relevés, journaux d'événements).
//
// Une page mémoire répond à « qu'est-ce que je sais » ; un tracker répond à
// « comment ça évolue ». D'où deux affichages distincts : la liste montre la
// DERNIÈRE valeur de chaque tracker (c'est presque toujours la question posée),
// le détail montre les points, du plus récent au plus ancien.
//
// La liste ne charge jamais les points : un tracker peut compter des milliers
// d'entrées, et le panneau doit s'ouvrir instantanément.

let TRACKERS = [], TRACK_OPEN = null;

async function loadTrackers(){
  let r;
  try{ r = await jget('/api/tracker'); }catch(_){ return; }
  if(!r || !r.ok) return;
  TRACKERS = r.trackers || [];
  renderTrackers();
}

function toggleTrackers(){
  const body = document.getElementById('track-body');
  if(!body) return;
  body.hidden = !body.hidden;
  if(!body.hidden) loadTrackers();
}

// Date lisible : on n'affiche l'heure que si le point en a une côté serveur —
// un relevé saisi « le 12 mars » ne doit pas s'afficher « 00:00 ».
function trackWhen(ev){
  const d = new Date(ev.ts);
  const day = d.toLocaleDateString();
  return ev.date_only ? day : day + ' ' + d.toLocaleTimeString([], {hour:'2-digit', minute:'2-digit'});
}

function renderTrackers(){
  const list = document.getElementById('track-list');
  const count = document.getElementById('track-count');
  if(!list) return;
  if(count) count.textContent = TRACKERS.length ? '(' + TRACKERS.length + ')' : '';
  list.textContent = '';
  if(!TRACKERS.length){
    const e = document.createElement('div');
    e.className = 'muted';
    e.style.fontSize = '12px';
    e.textContent = '(aucun tracker — ex. « poids », « abonnés », « relevés compteur »)';
    list.appendChild(e);
    return;
  }
  TRACKERS.forEach(t => {
    const row = document.createElement('div');
    row.className = 'preset';
    row.style.fontSize = '12px';
    row.onclick = () => openTracker(t.slug);

    const span = document.createElement('span');
    const b = document.createElement('b');
    b.style.color = 'var(--text)';
    b.textContent = t.name;
    span.appendChild(b);
    const d = document.createElement('span');
    d.className = 'muted';
    // La dernière valeur EST l'information : elle passe avant le compte.
    d.textContent = t.last_text ? ' — ' + t.last_text : ' — (vide)';
    span.appendChild(d);
    const sub = document.createElement('div');
    sub.className = 'muted';
    sub.style.cssText = 'font-size:11px;margin-top:2px';
    sub.textContent = t.count + ' point' + (t.count > 1 ? 's' : '')
                    + (t.last_ts ? ' · dernier le ' + new Date(t.last_ts).toLocaleDateString() : '');
    span.appendChild(sub);

    row.appendChild(span);
    list.appendChild(row);
  });
}

// --- détail d'un tracker -----------------------------------------------------

async function openTracker(slug){
  const r = await jget('/api/tracker?slug=' + encodeURIComponent(slug));
  if(!r || !r.ok){ toast('erreur : ' + ((r&&r.error)||'')); return; }
  TRACK_OPEN = r.tracker;
  document.getElementById('track-modal-title').textContent = TRACK_OPEN.name;
  renderTrackerEvents();
  showModal('track-modal');
}

function closeTracker(){ hideModal('track-modal'); TRACK_OPEN = null; }

function renderTrackerEvents(){
  const body = document.getElementById('track-events');
  if(!body || !TRACK_OPEN) return;
  body.textContent = '';
  const evs = (TRACK_OPEN.events || []).slice().reverse(); // plus récent d'abord
  if(!evs.length){
    const e = document.createElement('div');
    e.className = 'muted';
    e.style.fontSize = '12px';
    e.textContent = '(aucun point)';
    body.appendChild(e);
    return;
  }
  evs.forEach(ev => {
    const row = document.createElement('div');
    row.className = 'preset';
    row.style.fontSize = '12px';
    const span = document.createElement('span');
    const w = document.createElement('b');
    w.style.color = 'var(--text)';
    w.textContent = trackWhen(ev);
    span.appendChild(w);
    const t = document.createElement('span');
    t.className = 'muted';
    t.textContent = ' — ' + ev.text;
    span.appendChild(t);

    const acts = document.createElement('span');
    acts.style.cssText = 'display:flex;gap:4px;flex:none';
    acts.appendChild(trackBtn('modifier', () => editTrackerPoint(ev)));
    acts.appendChild(trackBtn('supprimer', () => deleteTrackerPoint(ev)));

    row.appendChild(span);
    row.appendChild(acts);
    body.appendChild(row);
  });
}

function trackBtn(label, fn){
  const b = document.createElement('button');
  b.textContent = label;
  b.style.cssText = 'margin:0;padding:2px 8px;font-size:11px';
  b.onclick = e => { e.stopPropagation(); fn(); };
  return b;
}

// Applique une action sur le tracker ouvert, puis recharge son détail ET la
// liste : les deux affichent la même donnée, ils ne doivent pas diverger.
async function trackerAction(payload, okMsg){
  const r = await jpost('/api/tracker', payload);
  if(!r.ok){ toast('erreur : ' + (r.error||'')); return false; }
  TRACKERS = r.trackers || TRACKERS;
  renderTrackers();
  if(okMsg) toast(okMsg);
  return true;
}

// La date est saisie en texte libre à précision variable (« 2026-07-15 »,
// « 2026-07-15 14:30 », vide = maintenant) : c'est le serveur qui la comprend
// (parseWhen), pour que l'outil du modèle et l'interface acceptent exactement
// la même chose.
const TRACK_WHEN_HINT = 'Date (vide = maintenant) — AAAA-MM-JJ ou AAAA-MM-JJ HH:MM';

async function addTrackerPoint(){
  if(!TRACK_OPEN) return;
  const text = await askPrompt('Valeur ou note du point :', {title:'Ajouter un point', placeholder:'ex. 78,2 kg', okText:'Suivant'});
  if(!text || !text.trim()) return;
  const when = await askPrompt(TRACK_WHEN_HINT, {title:'Quand ?', placeholder:'vide = maintenant', okText:'Ajouter'});
  if(when === null) return;
  if(!await trackerAction({action:'add', name:TRACK_OPEN.name, when, text}, 'point ajouté')) return;
  openTracker(TRACK_OPEN.slug);
}

async function newTracker(){
  const name = await askPrompt('Nom du tracker (ex. « poids », « abonnés ») :', {title:'Nouveau tracker', okText:'Suivant'});
  if(!name || !name.trim()) return;
  const text = await askPrompt('Premier point — valeur ou note :', {title:'Premier point', placeholder:'ex. 78,2 kg', okText:'Suivant'});
  if(!text || !text.trim()) return;
  const when = await askPrompt(TRACK_WHEN_HINT, {title:'Quand ?', placeholder:'vide = maintenant', okText:'Créer'});
  if(when === null) return;
  // Un tracker naît de son premier point : pas de coquille vide à remplir plus tard.
  await trackerAction({action:'add', name:name.trim(), when, text}, 'tracker créé');
}

async function editTrackerPoint(ev){
  if(!TRACK_OPEN) return;
  const text = await askPrompt('Valeur ou note :', {title:'Modifier le point', default:ev.text, okText:'Suivant'});
  if(text === null) return;
  const when = await askPrompt(TRACK_WHEN_HINT, {title:'Quand ?', default:'', placeholder:'vide = date inchangée', okText:'Enregistrer'});
  if(when === null) return;
  if(!await trackerAction({action:'edit', slug:TRACK_OPEN.slug, id:ev.id, text, when}, 'point modifié')) return;
  openTracker(TRACK_OPEN.slug);
}

async function deleteTrackerPoint(ev){
  if(!TRACK_OPEN) return;
  if(!await askConfirm('Supprimer le point du ' + trackWhen(ev) + ' ?', {title:'Supprimer', okText:'Supprimer', danger:true})) return;
  if(!await trackerAction({action:'delete', slug:TRACK_OPEN.slug, id:ev.id}, 'point supprimé')) return;
  openTracker(TRACK_OPEN.slug);
}

async function renameTracker(){
  if(!TRACK_OPEN) return;
  const name = await askPrompt('Nouveau nom :', {title:'Renommer le tracker', default:TRACK_OPEN.name, okText:'Renommer'});
  if(!name || !name.trim() || name.trim() === TRACK_OPEN.name) return;
  // Le slug dérive du nom : le serveur re-clé le tracker si nécessaire, sinon un
  // point ajouté ensuite créerait un tracker parallèle sous l'ancien nom.
  if(!await trackerAction({action:'rename', slug:TRACK_OPEN.slug, name:name.trim()}, 'renommé')) return;
  closeTracker();
}

async function moveTracker(){
  if(!TRACK_OPEN) return;
  const others = (typeof PROJECTS !== 'undefined' ? PROJECTS : []).filter(p => p.slug !== PROJ_ACTIVE);
  if(!others.length){ toast('aucun autre projet où déplacer'); return; }
  const choice = await askPrompt(
    'Déplacer « ' + TRACK_OPEN.name + ' » vers quel projet ?\n' + others.map(p => '· ' + p.name).join('\n'),
    {title:'Déplacer le tracker', default:others[0].name, okText:'Déplacer'});
  if(!choice) return;
  const target = others.find(p => p.name.toLowerCase() === choice.trim().toLowerCase());
  if(!target){ toast('projet inconnu : ' + choice); return; }
  if(!await trackerAction({action:'move', slug:TRACK_OPEN.slug, to:target.slug}, 'déplacé vers ' + target.name)) return;
  closeTracker();
  if(typeof loadProjects === 'function') loadProjects();
}

async function deleteTracker(){
  if(!TRACK_OPEN) return;
  const ok = await askConfirm(
    'Supprimer le tracker « ' + TRACK_OPEN.name + ' » et ses ' + TRACK_OPEN.count + ' point(s) ? Irréversible.',
    {title:'Supprimer le tracker', okText:'Supprimer', danger:true});
  if(!ok) return;
  if(!await trackerAction({action:'delete_tracker', slug:TRACK_OPEN.slug}, 'tracker supprimé')) return;
  closeTracker();
}
