// ─── Fichiers de la discussion ───────────────────────────────────────────────
// L'agent écrit des rapports, des scripts, des exports, des captures. Jusqu'ici
// on ne pouvait récupérer un fichier que si le modèle avait pensé à en mettre le
// lien dans sa réponse — tout le reste restait invisible, et faire le ménage
// demandait un shell.
//
// Le panneau montre les fichiers de la discussion OUVERTE, et rien d'autre :
// chaque discussion a son dossier côté serveur (chat_convfiles.go), changer de
// discussion change donc la liste, et supprimer une discussion emporte ses
// fichiers. `FILES_SCOPE = 'legacy'` ouvre le détour « hors discussion » : les
// fichiers d'avant ce rangement, que la migration n'a pas su rattacher.
//
// Le panneau est construit EN DOM, jamais en innerHTML : les noms de fichiers
// viennent du modèle et du système de fichiers, donc d'ailleurs. Même règle que
// pour les titres de discussions (16-conversations.js).
//
// Le téléchargement réutilise downloadWorkspaceFile (14-attach.js), qui sait
// déjà gérer le blob et les cas particuliers du navigateur.

let FILES_DIR = '';   // dossier affiché, relatif à la racine du panneau
let FILES_SCOPE = ''; // '' = discussion ouverte, 'legacy' = fichiers hors discussion
let FILES_OPEN = false;

// Ouvre/ferme le panneau. L'état est persisté : sur un grand écran on garde
// souvent le panneau ouvert, et le rouvrir à chaque rechargement est pénible.
function toggleFiles(force){
  FILES_OPEN = (force === undefined) ? !FILES_OPEN : !!force;
  document.documentElement.setAttribute('data-files', FILES_OPEN ? '1' : '0');
  const btn = document.getElementById('files-btn');
  if(btn) btn.setAttribute('aria-expanded', FILES_OPEN ? 'true' : 'false');
  try{ localStorage.setItem('loki-files-open', FILES_OPEN ? '1' : '0'); }catch(_){}
  // Ouvrir le panneau rétrécit la conversation SANS déclencher de `resize` :
  // la gouttière de barre de défilement mesurée dans --sbw resterait celle de
  // l'ancienne largeur, et la carte de saisie serait décalée du fil. La hauteur,
  // elle, est suivie par un ResizeObserver et n'a besoin de rien.
  if(typeof syncGutter === 'function') requestAnimationFrame(syncGutter);
  if(FILES_OPEN) loadFiles();
}

// Restaure l'état à l'ouverture de la page. Sur petit écran le panneau est un
// tiroir qui masque toute la conversation : on ne le rouvre jamais tout seul.
function initFiles(){
  let want = false;
  try{ want = localStorage.getItem('loki-files-open') === '1'; }catch(_){}
  toggleFiles(want && window.innerWidth > 720);
}

async function loadFiles(dir){
  if(dir !== undefined) FILES_DIR = dir;
  const list = document.getElementById('files-list');
  const foot = document.getElementById('files-foot');
  if(!list) return;
  let r;
  try{ r = await jget('/api/chat/files?dir='+encodeURIComponent(FILES_DIR)+'&scope='+encodeURIComponent(FILES_SCOPE)); }
  catch(_){ filesMsg('lecture impossible'); return; }
  if(!r.ok){
    // Un dossier supprimé entre deux affichages (par l'agent, ou depuis un autre
    // onglet) ne doit pas laisser le panneau bloqué : on remonte à la racine.
    if(FILES_DIR){ FILES_DIR = ''; return loadFiles(); }
    filesMsg(r.error || 'lecture impossible'); return;
  }

  renderCrumb(r);
  list.textContent = '';
  if(!r.entries.length){
    const d = document.createElement('div');
    d.className = 'files-empty muted';
    d.textContent = FILES_DIR ? '(dossier vide)'
      : (FILES_SCOPE === 'legacy' ? '(plus rien hors discussion)'
                                  : "Rien dans cette discussion — les fichiers déposés et ceux que l'agent écrit apparaissent ici.");
    list.appendChild(d);
  }
  for(const e of r.entries) list.appendChild(fileRow(e));

  renderFoot(foot, r);
}

// Pied : occupation disque de ce qui est affiché — la discussion ouverte, ou le
// hors-discussion — et le va-et-vient entre les deux. Le bouton n'apparaît que
// s'il reste vraiment quelque chose hors discussion (compté par le serveur), et
// disparaît de lui-même une fois le ménage fait.
function renderFoot(foot, r){
  if(!foot) return;
  foot.textContent = '';
  const n = document.createElement('span');
  n.textContent = r.count + (r.count > 1 ? ' fichiers · ' : ' fichier · ') + fmtSize(r.total);
  foot.appendChild(n);
  foot.title = r.root || '';
  if(FILES_SCOPE !== 'legacy' && !r.legacy) return;
  const b = document.createElement('button');
  b.className = 'files-scope';
  if(FILES_SCOPE === 'legacy'){
    b.textContent = '← cette discussion';
    b.title = 'revenir aux fichiers de la discussion ouverte';
    b.onclick = ()=>{ FILES_SCOPE = ''; loadFiles(''); };
  } else {
    b.textContent = 'hors discussion (' + r.legacy + ')';
    b.title = "fichiers d'avant le rangement par discussion";
    b.onclick = ()=>{ FILES_SCOPE = 'legacy'; loadFiles(''); };
  }
  foot.appendChild(b);
}

function filesMsg(txt){
  const list = document.getElementById('files-list');
  if(!list) return;
  list.textContent = '';
  const d = document.createElement('div');
  d.className = 'files-empty'; d.style.color = 'var(--err)'; d.textContent = txt;
  list.appendChild(d);
}

// Fil d'Ariane : « discussion / captures », chaque segment cliquable. La racine
// est le dossier de la discussion ouverte — on n'en sort pas, sauf par le
// détour « hors discussion », qui a sa propre racine.
function renderCrumb(r){
  const c = document.getElementById('files-crumb');
  c.textContent = '';
  const seg = (label, dir, last)=>{
    const b = document.createElement('button');
    b.className = 'files-crumb-b' + (last ? ' on' : '');
    b.textContent = label;
    if(!last) b.onclick = ()=>loadFiles(dir);
    else b.disabled = true;
    c.appendChild(b);
  };
  const parts = r.dir ? r.dir.split('/') : [];
  seg(FILES_SCOPE === 'legacy' ? 'hors discussion' : 'discussion', '', parts.length === 0);
  let acc = '';
  parts.forEach((p, i)=>{
    acc = acc ? acc + '/' + p : p;
    const sep = document.createElement('span');
    sep.className = 'files-crumb-s'; sep.textContent = '/';
    c.appendChild(sep);
    seg(p, acc, i === parts.length - 1);
  });
}

function fileRow(e){
  const row = document.createElement('div');
  row.className = 'files-row' + (e.dir ? ' isdir' : '');

  const ic = document.createElement('span');
  ic.className = 'files-ic';
  ic.appendChild(icon(e.dir ? 'folder' : (e.image ? 'image' : 'file'), 16));
  row.appendChild(ic);

  const mid = document.createElement('span');
  mid.className = 'files-mid';
  const nm = document.createElement('span');
  nm.className = 'files-name';
  nm.textContent = e.name;
  nm.title = e.name;
  const meta = document.createElement('span');
  meta.className = 'files-meta';
  meta.textContent = fmtSize(e.size) + (e.dir && e.items ? ' · ' + e.items + ' fich.' : '') + ' · ' + fmtWhen(e.mod);
  mid.append(nm, meta);
  row.appendChild(mid);

  // Un dossier s'ouvre ; un fichier se télécharge. Le clic sur la ligne fait
  // l'action évidente, les boutons ne servent qu'au reste.
  if(e.dir){
    mid.onclick = ()=>loadFiles(e.path);
    mid.style.cursor = 'pointer';
  } else {
    mid.onclick = ()=>downloadWorkspaceFile(e.path, e.name, row);
    mid.style.cursor = 'pointer';
  }

  const acts = document.createElement('span');
  acts.className = 'files-acts';
  if(!e.dir){
    const dl = document.createElement('button');
    dl.className = 'iconbtn'; dl.appendChild(icon('download', 16));
    dl.title = 'télécharger'; dl.setAttribute('aria-label', 'télécharger');
    dl.onclick = ev=>{ ev.stopPropagation(); downloadWorkspaceFile(e.path, e.name, row); };
    acts.appendChild(dl);
  }
  const rm = document.createElement('button');
  rm.className = 'iconbtn'; rm.appendChild(icon('trash', 16));
  rm.title = 'supprimer'; rm.setAttribute('aria-label', 'supprimer');
  rm.onclick = ev=>{ ev.stopPropagation(); removeFile(e); };
  acts.appendChild(rm);
  row.appendChild(acts);
  return row;
}

// Suppression. Toujours confirmée, et l'invite dit ce qui part : un dossier
// emporte tout son contenu, ce qu'un nom seul ne laisse pas deviner.
async function removeFile(e){
  const quoi = e.dir
    ? 'Le dossier « '+e.name+' » et tout son contenu ('+(e.items||0)+' fichiers, '+fmtSize(e.size)+') seront supprimés.'
    : '« '+e.name+' » ('+fmtSize(e.size)+') sera supprimé.';
  if(!await askConfirm(quoi + '\n\nC\'est définitif — le fichier ne part pas à la corbeille.',
      {title:'Supprimer ?', okText:'Supprimer', danger:true})) return;
  const r = await jpost('/api/chat/file/delete', {path: e.path, scope: FILES_SCOPE});
  if(!r.ok){ toast('suppression impossible : '+(r.error||'')); return; }
  toast('supprimé — '+fmtSize(r.freed||0)+' libérés');
  loadFiles();
}

// Changement de discussion (bascule, « vider »), signalé par le flux SSE :
// le panneau montre les fichiers de la discussion OUVERTE, il doit donc être
// redessiné. On revient aussi à sa racine — le sous-dossier affiché était celui
// de l'autre discussion, il n'existe pas forcément ici.
function filesOnConvChange(){
  FILES_DIR = ''; FILES_SCOPE = '';
  if(FILES_OPEN) loadFiles('');
}

// Date courte : l'heure pour aujourd'hui, la date sinon. Ce qu'on veut savoir,
// c'est « est-ce que ça vient du tour que je viens de lancer ? ».
document.addEventListener('DOMContentLoaded', initFiles);

function fmtWhen(sec){
  if(!sec) return '';
  const d = new Date(sec*1000), now = new Date();
  const memeJour = d.toDateString() === now.toDateString();
  return memeJour
    ? d.toLocaleTimeString(undefined, {hour:'2-digit', minute:'2-digit'})
    : d.toLocaleDateString(undefined, {day:'2-digit', month:'2-digit'});
}
