// Backend llama.cpp — la barre latérale sert UNIQUEMENT à installer les moteurs.
//   « llama.cpp précompilé »   = binaires officiels, aucune compilation
//   « llama.cpp compilé »      = compilé ici, pour cette machine
//   « llama.cpp personnalisé » = un fork (modèles à quant spéciale)
// Le CHOIX du moteur utilisé se fait par modèle (preset), pas ici — voir la
// section « Moteur » dans l'éditeur de modèle. Ça évite que la barre latérale
// et les presets se battent pour la ligne BIN.
let lcState = null, lcPoll = null, lcLogNext = 0;

async function loadLlamacpp(){
  let s;
  try{ s = await jget('/api/llamacpp'); }catch(_){ return; }
  lcState = s;
  const pb = s.prebuilt || {};
  const fastInstalled = !!pb.bin;
  const optInstalled  = !!s.bin;

  // Moteur fourni par l'image Docker : les trois modes d'installation n'ont
  // aucun sens (rien à installer, aucun compilateur dans l'image). On affiche
  // l'état réel à la place — les boutons du service, eux, restent utiles.
  lcRenderProvided(s);

  if(!s.provided){
    lcRenderReco(s.reco);
    lcRenderMode('fast', fastInstalled);
    lcRenderMode('opt', optInstalled);
    lcRenderCustomCard();

    // Job en cours (page rechargée pendant une install) → on raccroche l'affichage.
    if(s.job && s.job.exists && s.job.running && !lcPoll){
      openDetails('lc-details');
      lcStartPolling();
    }
    // Job terminé/interrompu qu'on n'a pas encore montré (rechargement APRÈS coup,
    // typiquement quand le service a redémarré) : on l'affiche sans polling.
    else if(s.job && s.job.exists && !s.job.running && !lcPoll && s.job.error && !lcEndShown){
      openDetails('lc-details');
      document.getElementById('lc-job').style.display = '';
      lcLogNext = 0;
      document.getElementById('lc-log').textContent = '';
      lcEndShown = true;
      lcPollJob(true); // quiet : simple rattrapage, pas de toast ni de rechargement
    }
    lcChipSync(s.job);
  }
  // Préchauffe la liste des cartes du moteur actif : interroger le moteur prend
  // 1 à 3 s (init CUDA/Vulkan), et sans ça l'encart « cartes graphiques » de
  // l'éditeur apparaissait après coup, une fois le reste déjà affiché.
  if(typeof prefetchGpuDevices === 'function') prefetchGpuDevices(s.config_bin);
}

// --- Pastille de rappel hors panneau ---------------------------------------
// Le détail de l'installation vit dans le panneau latéral, qui est un tiroir
// FERMÉ sur téléphone : après un rechargement, rien ne disait qu'une
// compilation tournait encore. La pastille le dit, et y ramène en un clic.
let lcSeenEnd = false, lcEndShown = false;
function lcChipLabel(action){
  return {install:'Compilation du moteur', update:'Mise à jour du moteur',
          prebuilt:'Téléchargement du moteur', engine:'Mise à jour du moteur',
          custom:'Installation du backend'}[action] || 'Installation du moteur';
}
function lcChipSync(j){
  const chip = document.getElementById('lc-chip');
  if(!chip) return;
  if(!j || !j.exists || (!j.running && (!j.error || lcSeenEnd))){ chip.hidden = true; return; }
  chip.hidden = false;
  chip.classList.toggle('failed', !j.running && !!j.error);
  chip.textContent = j.running
    ? '⏳ ' + lcChipLabel(j.action) + ' — ' + (j.phase || '…')
    : '✗ ' + lcChipLabel(j.action) + ' interrompue';
}
// Clic sur la pastille : ouvrir le tiroir sur la section Moteur.
function lcChipOpen(){
  const side = document.getElementById('side');
  if(!side.classList.contains('open')) toggleSide();
  const det = document.getElementById('lc-details');
  openDetails(det); // déplie aussi « Réglages », qui contient la section
  det.scrollIntoView({block:'center'});
  if(!document.getElementById('lc-chip').classList.contains('failed')) return;
  lcSeenEnd = true;
  lcChipSync(null);
}

// Place la pastille « conseillé » sur la carte que le SERVEUR recommande pour
// cette machine (voir recommendedMode) : le précompilé partout, sauf sur Linux
// avec une carte NVIDIA où il ne donnerait que du Vulkan. La raison est ajoutée
// à la description de la carte, pour expliquer plutôt que d'imposer.
function lcRenderReco(reco){
  const mode = (reco && reco.mode) || 'fast';
  for(const m of ['fast','opt']){
    const badge = document.getElementById('lc-reco-'+m);
    if(badge) badge.hidden = (m !== mode);
  }
  // La raison REMPLACE la description générique de la carte conseillée : elle
  // dit déjà ce que fait l'option et pourquoi c'est le bon choix ici. (loadAll
  // repasse par là, donc pas d'accumulation possible.)
  const desc = document.getElementById('lc-desc-'+mode);
  if(desc && reco && reco.why) desc.textContent = reco.why;
}

// Bascule entre la carte « moteur de l'image » et les trois modes
// d'installation gérés par Loki.
function lcRenderProvided(s){
  const box = document.getElementById('lc-provided');
  const modes = document.querySelector('.lc-modes');
  if(!box || !modes) return;
  box.style.display = s.provided ? '' : 'none';
  modes.style.display = s.provided ? 'none' : '';
  if(!s.provided) return;
  const bin = document.getElementById('lc-provided-bin');
  if(bin) bin.textContent = s.config_bin || '';
  loadEngine();
  // Un job de mise à jour du moteur peut être en cours : le bloc de progression
  // était masqué ici tant que la carte ne servait qu'à dire « rien à faire ».
  if(s.job && s.job.exists && s.job.running && !lcPoll){
    openDetails('lc-details');
    lcStartPolling();
  }
  lcChipSync(s.job);
}

// --- Moteur de l'image : le mettre à jour sans reconstruire l'image ---------
// llama.cpp publie plusieurs versions par jour ; son moteur déjà compilé vit
// dans l'image officielle, dont on n'extrait que /app (~170 Mo). Voir
// web_engine.go pour le détail, et pourquoi ce n'est pas une release GitHub :
// llama.cpp n'en publie aucune pour CUDA/Linux.
let engState = null;

async function loadEngine(){
  let s;
  try{ s = await jget('/api/engine'); }catch(_){ return; }
  engState = s;
  const build = document.getElementById('eng-build');
  if(build) build.textContent = s.build ? ('b'+s.build+(s.commit ? ' · '+s.commit : '')) : 'version inconnue';
  const src = document.getElementById('eng-source');
  if(src) src.textContent = {image:'fourni par l\'image', downloaded:'mis à jour par Loki',
                             prebuilt:'précompilé', custom:'personnalisé'}[s.source] || '';
  const repo = document.getElementById('eng-repo');
  if(repo) repo.textContent = s.repo + ':' + (s.variant || 'server');
  // Version précédente (celle qui tournait avant la dernière bascule) : un
  // bouton à elle, sans réseau. Les autres boutons ne la répètent pas.
  const prev = s.previous || null;
  const prevBtn = document.getElementById('eng-prev');
  if(prevBtn){
    prevBtn.style.display = prev ? '' : 'none';
    if(prev) prevBtn.textContent = 'Revenir à la version précédente'
      + (prev.source === 'image' ? ' (moteur de l\'image' + (prev.build ? ', b'+prev.build : '') + ')'
                                 : (prev.build ? ' (b'+prev.build+')' : ''));
  }
  // Le retour arrière n'a de sens que si on n'est pas déjà dessus.
  const revert = document.getElementById('eng-revert');
  if(revert) revert.style.display = (s.image_bin && s.source !== 'image' && !(prev && prev.source === 'image')) ? '' : 'none';
  // Versions téléchargées gardées à côté (la précédente survit à une mise à
  // jour) : revenir dessus ne demande ni réseau ni que le tag soit encore publié.
  const others = document.getElementById('eng-others');
  if(others) others.innerHTML = (s.installed || []).filter(v=>!v.in_use && !(prev && prev.tag === v.tag)).map(v=>{
    const tag = String(v.tag).replace(/[^A-Za-z0-9._-]/g,'');
    return '<button onclick="engUse(\''+tag+'\')">utiliser '+(v.build ? 'b'+v.build : tag)+'</button>';
  }).join(' ');
  // Build trop ancien pour les points de reprise des hybrides (voir
  // engineBuildNotice) : un avis, rien n'est changé d'office ici.
  // L'encart « version recommandée » reprend l'avis : il le remplace quand il
  // est affiché.
  const notice = document.getElementById('eng-notice');
  if(notice){
    notice.textContent = s.notice ? '⚠ '+s.notice : '';
    notice.style.display = (s.notice && !s.recommend) ? '' : 'none';
  }
  // Encart « version recommandée » : décidé côté serveur sans réseau ; le tag
  // n'est choisi qu'au clic (engRecommended).
  const reco = document.getElementById('eng-reco');
  if(reco){
    const r = s.recommend;
    reco.style.display = r ? '' : 'none';
    if(r){
      document.getElementById('eng-reco-min').textContent = r.min;
      document.getElementById('eng-reco-cur').textContent = r.current;
      document.getElementById('eng-reco-gains').innerHTML = (r.gains||[])
        .map(g=>'<li>'+String(g).replace(/[<>&]/g,'')+'</li>').join('');
    }
  }
  engRenderCheck(s.render_check);
  engSay('');
}

// Rendu du gabarit comparé avant/après la dernière bascule (web_engine_reco.go).
// « pending » : le nouveau moteur charge encore le modèle — on repasse plus
// tard tant que le panneau est ouvert.
let engRenderTimer = null;
function engRenderCheck(c){
  const el = document.getElementById('eng-render');
  if(!el) return;
  const esc = x=>String(x||'').replace(/[<>&]/g,'');
  if(!c){ el.style.display = 'none'; return; }
  el.style.display = '';
  el.className = c.status === 'changed' ? '' : 'muted';
  if(c.status === 'pending'){
    el.innerHTML = '⏳ vérification du rendu du gabarit après '+esc(c.tag)+' (une fois le modèle chargé)…';
    if(!engRenderTimer) engRenderTimer = setTimeout(()=>{
      engRenderTimer = null;
      const pane = document.getElementById('lc-details');
      if(pane && pane.offsetParent !== null) loadEngine();
    }, 10000);
  } else if(c.status === 'same'){
    el.innerHTML = '✓ rendu du gabarit identique avant et après '+esc(c.tag);
  } else if(c.status === 'changed'){
    el.innerHTML = '⚠ <b>le prompt rendu a changé avec '+esc(c.tag)+'</b> — '+esc(c.detail)
      + '<br>Si ce sont des blocs de réflexion vides dans l\'historique, <code>REASONING_PRESERVE=off</code> dans le preset rétablit le rendu d\'avant ; « Revenir à la version précédente » annule la mise à jour.';
  } else {
    el.innerHTML = 'rendu du gabarit non vérifié après '+esc(c.tag)+(c.detail ? ' : '+esc(c.detail) : '');
  }
}

// engSay écrit sous la ligne d'état : c'est le résultat d'une vérification, qui
// doit rester lisible après le toast (on ne se souvient pas d'un toast).
function engSay(html, cls){
  const el = document.getElementById('eng-latest');
  if(!el) return;
  el.innerHTML = html;
  el.className = cls || 'muted';
  el.style.display = html ? '' : 'none';
}

async function engCheck(){
  toast('vérification…');
  let r;
  try{ r = await jpost('/api/engine/check', {}); }catch(_){ toast('erreur réseau'); return; }
  if(!r.ok){ engSay('✗ '+String(r.error||'').replace(/[<>&]/g,'')); toast('erreur'); return; }
  if(r.update){
    engSay('Version disponible : <b>b'+r.latest+'</b>'+(r.current ? ' (installée : b'+r.current+')' : '')
           + ' — « mettre à jour le moteur » pour l\'installer.');
    toast('mise à jour disponible : b'+r.latest);
  } else {
    engSay('Moteur à jour ✓ (b'+r.latest+')');
    toast('moteur à jour ✓');
  }
}

async function engUpdate(){ return engPlanAndUpdate(false); }
async function engRecommended(){ return engPlanAndUpdate(true); }

// engPlanAndUpdate : sur clic seulement. Le serveur choisit la version (la
// dernière publiée, ou la plus récente ≥ minimum recommandé) et dit si le
// prompt rendu va changer (preserve_reasoning par défaut depuis b10763) ; la
// confirmation montre les deux, et la case REASONING_PRESERVE=off laisse le
// choix à l'utilisateur — cochée d'office seulement quand le changement est
// établi, puisque sur un gabarit qui garde déjà la réflexion, off changerait
// à son tour le rendu.
async function engPlanAndUpdate(recommended){
  toast(recommended ? 'recherche de la version recommandée…' : 'recherche de la dernière version…');
  let p;
  try{ p = await jpost('/api/engine/plan', {recommended}); }catch(_){ toast('erreur réseau'); return; }
  if(!p.ok){ engSay('✗ '+String(p.error||'').replace(/[<>&]/g,'')); toast('erreur'); return; }
  if(p.uptodate){ engSay('Moteur à jour ✓ (b'+p.build+')'); toast('moteur à jour ✓'); return; }
  const what = p.build ? 'b'+p.build+' ('+p.tag+')' : p.tag;
  let msg = 'Télécharger llama.cpp '+what+' (~170 Mo) et l\'utiliser à la place du moteur actuel'
          + (p.current ? ' (b'+p.current+')' : '') + '.\n\n'
          + 'Le moteur est essayé avant d\'être adopté ; '
          + (p.keeps ? 'la version actuelle ('+p.keeps+') est gardée' : 'celui de l\'image reste intact')
          + ' pour revenir en arrière. Le moteur redémarre à la fin — la génération en cours sera coupée.';
  const pr = p.preserve || {risk:'no'};
  const opts = {title: recommended ? 'Version recommandée' : 'Mettre à jour le moteur', okText:'Mettre à jour'};
  if(pr.risk !== 'no'){
    msg += '\n\n⚠ Rendu du raisonnement : '+(pr.why||'');
    opts.check = 'poser REASONING_PRESERVE=off (garde le rendu actuel de l\'historique)';
    opts.checked = !!pr.suggest;
  }
  if(!await askConfirm(msg, opts)) return;
  const preserve_off = pr.risk !== 'no' && askChecked();
  let r;
  try{ r = await jpost('/api/engine/update', {tag: p.tag, preserve_off}); }catch(_){ toast('erreur réseau'); return; }
  if(!r.ok){ toast('erreur : '+(r.error||'')); return; }
  openDetails('lc-details');
  lcStartPolling();
}

// Retour au moteur d'avant la dernière bascule — sans réseau.
async function engRollback(){
  const prev = (engState && engState.previous) || null;
  if(!prev) return;
  const what = prev.source === 'image' ? 'le moteur de l\'image' : (prev.build ? 'b'+prev.build : (prev.tag || prev.bin));
  if(!await askConfirm('Revenir à la version précédente ('+what+'). Le moteur redémarre.',
      {title:'Revenir à la version précédente', okText:'Revenir'})) return;
  let r;
  try{ r = await jpost('/api/engine/rollback', {}); }catch(_){ toast('erreur réseau'); return; }
  if(!r.ok){ toast('erreur : '+(r.error||'')); return; }
  toast('version précédente rétablie — redémarrage en cours');
  loadAll();
}

// Bascule vers une version déjà installée — « image » = celle d'origine.
async function engUse(tag){
  const image = tag === 'image';
  if(!await askConfirm(image ? 'Repasser sur le moteur livré avec l\'image Docker. Le moteur redémarre.'
                             : 'Utiliser la version « '+tag+' ». Le moteur redémarre.',
      {title:'Changer de moteur', okText:'Basculer'})) return;
  let r;
  try{ r = await jpost('/api/engine/use', {tag}); }catch(_){ toast('erreur réseau'); return; }
  if(!r.ok){ toast('erreur : '+(r.error||'')); return; }
  toast('moteur basculé — redémarrage en cours');
  loadAll();
}

function lcRenderMode(mode, installed){
  const card  = document.getElementById(mode==='fast' ? 'lc-mode-fast' : 'lc-mode-opt');
  const state = document.getElementById(mode==='fast' ? 'lc-fast-state' : 'lc-opt-state');
  card.classList.toggle('installed', installed);
  // Lien de vérification : interroge la dernière version SANS rien installer.
  // event.stopPropagation empêche le clic de la carte (qui lance l'install).
  const check = '<span class="lc-update-link" onclick="event.stopPropagation();lcCheck(\''+mode+'\')">vérifier la version</span>';
  if(installed){
    state.innerHTML = '<span class="lc-mode-active-tag">✓ installée</span>'
      + '<span class="lc-update-link" onclick="event.stopPropagation();lcUpdate(\''+mode+'\')">↻ mettre à jour</span>'
      + check;
  } else {
    state.innerHTML = '<span class="lc-mode-go">→ cliquer pour installer</span>' + check;
  }
}

// lcCheck vérifie s'il existe une version plus récente AVANT toute installation
// (endpoints de check dédiés, sans effet de bord). Résultat affiché en toast.
async function lcCheck(mode){
  toast('vérification…');
  try{
    if(mode === 'fast'){
      const r = await jpost('/api/llamacpp/prebuilt/check', {});
      if(!r.ok){ toast('erreur : '+(r.error||'')); return; }
      if(r.update) toast('nouvelle version disponible : '+r.latest+(r.current ? ' (installée : '+r.current+')' : ''));
      else toast('llama.cpp précompilé à jour ✓'+(r.latest ? ' ('+r.latest+')' : ''));
    } else {
      const r = await jpost('/api/llamacpp/check', {});
      if(!r.ok){ toast('erreur : '+(r.error||'')); return; }
      if(r.behind > 0) toast(r.behind+' nouveau(x) commit(s) disponible(s) — utilisez « mettre à jour »');
      else toast('llama.cpp compilé à jour ✓');
    }
  }catch(_){ toast('erreur réseau'); }
}

// Clic sur une carte : installer le moteur (s'il ne l'est pas déjà).
async function lcPick(mode){
  const s = lcState || {}, pb = s.prebuilt || {};
  const installed = mode==='fast' ? !!pb.bin : !!s.bin;
  if(installed){
    toast('déjà installée — choisissez-la dans l\'édition d\'un modèle (⚙)');
    return;
  }
  if(mode === 'fast'){
    if(!await askConfirm('Télécharger le binaire officiel de llama.cpp, prêt à l\'emploi (~2 min, aucune compilation).', {title:'llama.cpp précompilé', okText:'Installer'})) return;
    const r = await jpost('/api/llamacpp/prebuilt', {});
    if(!r.ok){ toast('erreur : '+(r.error||'')); return; }
  } else {
    if(!await askConfirm('Compiler llama.cpp pour votre machine. Ça peut prendre de longues minutes (surtout avec une carte NVIDIA).', {title:'llama.cpp compilé', okText:'Compiler'})) return;
    const r = await jpost('/api/llamacpp/install', {});
    if(!r.ok){ toast('erreur : '+(r.error||'')); return; }
  }
  lcStartPolling();
}

// « Mettre à jour » sur une carte installée.
async function lcUpdate(mode){
  if(mode === 'fast'){
    if(!await askConfirm('Vérifier et installer le dernier binaire précompilé.', {title:'Mettre à jour', okText:'Mettre à jour'})) return;
    const r = await jpost('/api/llamacpp/prebuilt', {});
    if(!r.ok){ toast('erreur : '+(r.error||'')); return; }
  } else {
    if(!await askConfirm('Vérifier et installer la dernière version compilée (recompilation si besoin).', {title:'Mettre à jour', okText:'Mettre à jour'})) return;
    const r = await jpost('/api/llamacpp/update', {clean:false});
    if(!r.ok){ toast('erreur : '+(r.error||'')); return; }
  }
  lcStartPolling();
}

// --- Backends personnalisés (3e carte + modal de gestion) ------------------
let lcCustomBackends = [];

// État affiché sur la carte « Backend personnalisé » : nombre d'installés.
async function lcRenderCustomCard(){
  const state = document.getElementById('lc-custom-state');
  if(!state) return;
  try{ lcCustomBackends = await jget('/api/backends/custom') || []; }catch(_){ lcCustomBackends = []; }
  const n = lcCustomBackends.length;
  document.getElementById('lc-mode-custom').classList.toggle('installed', n>0);
  state.innerHTML = n>0
    ? '<span class="lc-mode-active-tag">✓ '+n+' installé'+(n>1?'s':'')+'</span><span class="lc-mode-go">→ gérer</span>'
    : '<span class="lc-mode-go">→ voir / installer</span>';
}

function openCustomBackends(){
  showModal('lc-custom-modal');
  document.getElementById('lc-custom-url').value = '';
  document.getElementById('lc-custom-name').value = '';
  loadCustomBackends();
}
function closeCustomBackends(){ hideModal('lc-custom-modal'); }

// Liste les backends custom dans le modal, avec un bouton supprimer par ligne.
async function loadCustomBackends(){
  const box = document.getElementById('lc-custom-list');
  box.innerHTML = '<span class="muted" style="font-size:12px">chargement…</span>';
  let list = [];
  try{ list = await jget('/api/backends/custom') || []; }catch(_){}
  lcCustomBackends = list;
  if(!list.length){ box.innerHTML = '<span class="muted" style="font-size:12px">aucun backend personnalisé pour l\'instant.</span>'; return; }
  box.innerHTML = list.map(b=>{
    const nm = String(b.name).replace(/[<>&]/g,'');
    const used = b.in_use ? '<span class="mcp-tag" style="border-color:var(--accent);color:var(--accent)">utilisé</span>' : '';
    return '<div class="mcp-row" style="cursor:default">'
      + '<span class="mcp-dot '+(b.in_use?'mcp-dot-ok':'mcp-dot-off')+'"></span>'
      + '<div class="mcp-info"><div class="mcp-name">'+nm+'</div>'
      + '<div class="mcp-meta">'+used+'<span class="mcp-tag" title="'+String(b.path).replace(/"/g,'&quot;')+'">'+String(b.path).split('/').slice(-3).join('/').replace(/[<>&]/g,'')+'</span></div></div>'
      + '<button class="btn-danger" style="padding:3px 8px;font-size:11px" onclick="lcUninstallCustom(\''+nm.replace(/'/g,"\\'")+'\')">supprimer</button>'
      + '</div>';
  }).join('');
}

// Installer un backend CUSTOM depuis une URL de dépôt Git (fork llama.cpp).
// Cloné + compilé dans backends/<nom>, SANS toucher au moteur global : il
// apparaît ensuite dans le menu « backend détecté » de l'éditeur de modèle.
async function lcInstallCustom(){
  const url = (document.getElementById('lc-custom-url').value||'').trim();
  if(!url){ toast('collez l\'URL d\'un dépôt Git'); return; }
  const name = (document.getElementById('lc-custom-name').value||'').trim();
  if(!await askConfirm('Cloner et compiler ce backend depuis :\n'+url+'\n\nCela peut prendre de longues minutes (surtout avec une carte NVIDIA). Il ne remplace pas le moteur global — vous le choisirez par modèle.', {title:'Backend personnalisé', okText:'Installer'})) return;
  const r = await jpost('/api/llamacpp/install-custom', {repo:url, name});
  if(!r.ok){ toast('erreur : '+(r.error||'')); return; }
  closeCustomBackends();
  openDetails('lc-details');
  lcStartPolling();
}

// Désinstaller (supprime le dossier backends/<name>). Le serveur refuse si le
// backend sert de moteur au modèle actif.
async function lcUninstallCustom(name){
  if(!await askConfirm('Supprimer le backend « '+name+' » ? Son dossier compilé sera effacé. Les modèles qui l\'utilisent devront être repointés sur un autre moteur.', {title:'Supprimer le backend', okText:'Supprimer', danger:true})) return;
  const r = await jpost('/api/llamacpp/uninstall-custom', {name});
  if(!r.ok){ toast('erreur : '+(r.error||'')); return; }
  toast('backend supprimé');
  loadCustomBackends();
  lcRenderCustomCard();
}

// --- Progression de l'installation (téléchargement / compilation) ----------
function lcBusy(on){ document.querySelector('.lc-modes').classList.toggle('busy', on); }

function lcStartPolling(){
  document.getElementById('lc-job').style.display = '';
  document.getElementById('lc-log').textContent = '';
  lcLogNext = 0;
  lcSeenEnd = false; lcEndShown = false;
  lcBusy(true);
  if(lcPoll) clearInterval(lcPoll);
  lcPoll = setInterval(lcPollJob, 1000);
  lcPollJob();
}

async function lcPollJob(quiet){
  let j;
  try{ j = await jget('/api/llamacpp/job?from='+lcLogNext); }catch(_){ return; }
  if(!j.exists) return;
  const phaseEl = document.getElementById('lc-job-phase');
  if(j.lines && j.lines.length){
    const pre = document.getElementById('lc-log');
    const stick = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 20;
    pre.textContent += j.lines.join('\n') + '\n';
    if(stick) pre.scrollTop = pre.scrollHeight;
  }
  if(typeof j.next === 'number') lcLogNext = j.next;
  lcChipSync(j);
  if(j.running){
    phaseEl.innerHTML = '<span class="lc-spin">⏳</span> <span>'+String(j.phase||'…').replace(/[<>&]/g,'')+'</span>';
    return;
  }
  if(lcPoll){ clearInterval(lcPoll); lcPoll = null; }
  lcBusy(false);
  if(j.error){
    phaseEl.innerHTML = '<span style="color:var(--err)">✗ '+String(j.error).replace(/[<>&]/g,'')+'</span>';
    const pre = document.getElementById('lc-log');
    if(pre.hasAttribute('hidden')) lcToggleLog();
    pre.scrollTop = pre.scrollHeight;
    toast('échec — voir les détails');
  } else {
    phaseEl.innerHTML = '<span style="color:var(--ok)">✓ '+String(j.phase||'terminé').replace(/[<>&]/g,'')+'</span>';
    toast('c\'est prêt ✓');
  }
  loadAll();
}

function lcToggleLog(){
  const pre = document.getElementById('lc-log');
  const bar = document.querySelector('.lc-logbar');
  if(pre.hasAttribute('hidden')){ pre.removeAttribute('hidden'); bar.classList.add('open'); pre.scrollTop = pre.scrollHeight; }
  else { pre.setAttribute('hidden',''); bar.classList.remove('open'); }
}
