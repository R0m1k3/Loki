// Moteur Strata : installation « en un clic » du moteur spécialisé Qwen3.8 Flash
// Next (repris d'AJEAN, « AJEAN MoE »). L'utilisateur choisit la version (Swift
// ou Classique) et la qualité (quant) ; tout le reste (cartes, RAM, mmap,
// réglages) est déduit côté serveur (backend_strata.go). L'installation est une
// tâche comme celles de llama.cpp : même suivi (#lc-job), même pastille.
let strataState = null, strataFam = '', strataQuant = '', strataTarget = '';

async function loadStrata(){
  let s;
  try{ s = await jget('/api/strata'); }catch(_){ return; }
  strataState = s;
  const row = document.getElementById('lc-strata');
  if(!row) return;
  // Hors Linux + NVIDIA, la ligne reste visible mais dit pourquoi : choisir son
  // moteur, c'est aussi savoir pourquoi l'un des deux n'est pas proposé.
  const env = s.env || {};
  row.classList.toggle('moe-off', !env.supported);
  const st = document.getElementById('lc-strata-state');
  if(st){
    const n = (s.installed||[]).length;
    st.textContent = !env.supported ? (env.reason || 'indisponible sur cette machine')
      : s.active ? 'moteur actif' + (n > 1 ? ' · '+n+' modèles installés' : '')
      : n ? n+' modèle(s) installé(s)' : '';
  }
  const eng = document.getElementById('lc-active-engine');
  if(eng) eng.textContent = s.active ? 'Strata' : 'llama.cpp';
}

// presetId (optionnel) : ouverte depuis l'engrenage d'un modèle installé, la
// fenêtre se place directement sur lui.
async function openStrata(presetId){
  // état relu à chaque ouverture : juste après une installation, l'ancien
  // proposait encore « Installer »
  await loadStrata();
  if(!strataState) return;
  const s = strataState, env = s.env || {};
  if(!env.supported){ toast('Strata : '+(env.reason||'indisponible sur cette machine')); return; }
  strataTarget = typeof presetId === 'string' ? presetId : '';
  const cards = (env.gpus||[]).filter(g=>g.index===env.main || g.index===env.helper)
    .sort((a,b)=>(a.index===env.main?-1:1))
    .map(g=>g.name.replace(/^NVIDIA GeForce /,'')+' '+Math.round(g.vram_gb)+' Go');
  document.getElementById('strata-machine').textContent =
    'Machine : '+cards.join(' + ')+', '+Math.round(env.ram_gb)+' Go de RAM dont '
    +Math.round(env.avail_gb||env.ram_gb)+' disponibles, '
    +Math.round(env.disk_free_gb)+' Go de disque libres. Réglages automatiques.';
  if(!strataFam) strataFam = (s.families[0]||{}).id || '';
  if(strataTarget){
    for(const f of s.families){
      const hit = f.fits.find(x=>x.preset===strataTarget);
      if(hit){ strataFam = f.id; strataQuant = hit.quant.id; }
    }
  }
  strataRenderFams();
  strataRenderQuants(!strataTarget);
  showModal('strata-modal');
}
function closeStrata(){ hideModal('strata-modal'); }

function strataRenderFams(){
  const box = document.getElementById('strata-fams');
  box.innerHTML = strataState.families.map(f=>
    '<label><input type="radio" name="strata-fam" value="'+f.id+'"'+(f.id===strataFam?' checked':'')
    +' onchange="strataFam=this.value;strataRenderQuants(true)">'
    +'<span>'+escHtml(f.label)+'</span><span class="be-note">'+escHtml(f.about)+'</span></label>').join('');
}

function strataRenderQuants(pickReco){
  const f = strataState.families.find(x=>x.id===strataFam);
  if(!f) return;
  // à l'ouverture : le modèle actif s'il est de cette version, sinon un installé, sinon le conseillé
  if(pickReco || !f.fits.some(x=>x.quant.id===strataQuant && (x.ok || x.preset))){
    const cur = f.fits.find(x=>x.active) || f.fits.find(x=>x.preset);
    strataQuant = cur ? cur.quant.id : (f.recommend || '');
  }
  const box = document.getElementById('strata-quants');
  box.innerHTML = f.fits.map(x=>{
    const q = x.quant, sel = q.id===strataQuant;
    const tag = x.active ? 'actif' : x.preset ? 'installé' : q.id===f.recommend ? 'conseillé' : '';
    const reco = tag ? ' <span class="lc-reco">'+tag+'</span>' : '';
    const usable = x.ok || !!x.preset;   // installé : activable même si l'espace disque manque
    const desc = x.preset ? escHtml(q.about)
      : x.ok ? escHtml(q.about)+' · ~'+Math.round(x.disk_gb)+' Go sur le disque'
      : escHtml(x.why);
    return '<div class="lc-mode'+(sel?' moe-sel':'')+(usable?'':' moe-off')+'"'
      +(usable?' onclick="strataQuant=\''+q.id+'\';strataRenderQuants(false)"':'')+'>'
      +'<div class="lc-mode-t">'+q.id+reco+'</div><div class="lc-mode-d">'+desc+'</div></div>';
  }).join('');
  const fit = f.fits.find(x=>x.quant.id===strataQuant);
  const go = document.getElementById('strata-go');
  go.disabled = !fit || !!fit.active;
  go.textContent = fit && fit.active ? 'Déjà actif' : fit && fit.preset ? 'Activer' : 'Installer';
  strataRenderDisk(fit);
  strataRenderSettings(fit);
  strataRenderDetails(fit);
  document.getElementById('strata-note').textContent = !fit ? 'Aucune version ne tient sur cette machine.'
    : fit.active ? 'Modèle actif.' : fit.preset ? 'Modèle installé, prêt à être activé.'
    : fit.mmap ? 'RAM disponible insuffisante pour tout charger : les experts seront lus depuis le disque (mmap, un peu plus lent). Installation et activation automatiques.'
    : 'Installation et activation automatiques. Premier chargement : quelques minutes.';
}

// Place disque : libre, ce que ce choix occupe, ce qui restera.
function strataRenderDisk(fit){
  const el = document.getElementById('strata-disk');
  const free = Math.round((strataState.env||{}).disk_free_gb||0);
  if(!fit){ el.textContent = ''; return; }
  if(fit.preset){ el.textContent = 'Disque : '+free+' Go libres. Modèle déjà présent.'; return; }
  const need = Math.round(fit.disk_gb), left = free - need;
  el.textContent = left >= 5
    ? 'Disque : '+free+' Go libres, ~'+need+' Go requis, ~'+left+' Go restants.'
    : 'Disque : '+free+' Go libres, ~'+need+' Go requis. Espace insuffisant.';
}

// Réglages modifiables d'un modèle installé.
function strataRenderSettings(fit){
  const fld = document.getElementById('strata-settings-fld'), save = document.getElementById('strata-save');
  const d = fit && fit.details;
  fld.hidden = save.hidden = !d;
  if(!d) return;
  const sel = (id, opts, cur) => '<select class="ctl" id="'+id+'" style="width:auto">'
    + opts.map(([v,l])=>'<option value="'+v+'"'+(String(v)===String(cur)?' selected':'')+'>'+escHtml(l)+'</option>').join('')+'</select>';
  const sw = (id, on) => '<label class="switch"><input type="checkbox" id="'+id+'"'+(on?' checked':'')+'><span class="slider"></span></label>';
  const rows = [
    ['Contexte', '<span class="moe-ctx"><input type="range" id="strata-s-ctx" min="32768" max="262144" step="4096" value="'+(+d.ctx||131072)+'" oninput="strataCtxLbl()"><span id="strata-s-ctx-v"></span></span>'],
    ['Cache KV', sel('strata-s-kv', [['fp16', 'fp16 (meilleure qualité)'],['int8', '8 bits (plus léger)']], d.kv)],
    ['Prédiction (MTP)', sel('strata-s-spec', [[2,'2'],[3,'3'],[4,'4']], d.spec)],
    // Placement des experts : auto décide AU LANCEMENT sur la RAM disponible ;
    // « disque » ne charge rien en RAM au démarrage (jamais tué faute de RAM).
    ['Experts', sel('strata-s-experts', [['auto','auto (selon la RAM libre)'],['ram','en RAM (le plus rapide)'],['disk','depuis le disque (mmap, économe)']], d.experts||'auto')],
    ['Lire les images', sw('strata-s-vision', d.vision)],
  ];
  if(d.has_helper) rows.push(["Carte d'appoint", sw('strata-s-helper', d.helper_on)]);
  document.getElementById('strata-settings').innerHTML = rows.map(([a,b])=>
    '<div class="moe-kv"><span>'+escHtml(a)+'</span><span>'+b+'</span></div>').join('');
  strataCtxLbl();
}
// valeur exacte en jetons (131 072 et non « 128K », source de confusion)
function strataCtxLbl(){
  const r = document.getElementById('strata-s-ctx'), o = document.getElementById('strata-s-ctx-v');
  if(!r || !o) return;
  o.textContent = (+r.value).toLocaleString('fr-FR');
  // partie remplie à gauche de la pastille (la piste est dessinée en CSS)
  r.style.setProperty('--p', ((r.value - r.min) / (r.max - r.min) * 100) + '%');
}

async function strataSaveSettings(){
  const f = strataState && strataState.families.find(x=>x.id===strataFam);
  const fit = f && f.fits.find(x=>x.quant.id===strataQuant);
  if(!fit || !fit.preset) return;
  const v = id => document.getElementById(id);
  const body = {preset: fit.preset, ctx: +v('strata-s-ctx').value, kv: v('strata-s-kv').value, spec: +v('strata-s-spec').value,
    vision: v('strata-s-vision').checked, helper: v('strata-s-helper') ? v('strata-s-helper').checked : false,
    experts: v('strata-s-experts') ? v('strata-s-experts').value : ''};
  if(fit.active && !await askConfirm('Le modèle va redémarrer avec ces réglages (quelques minutes).', {title:'Strata — Qwen3.8 Flash Next', okText:'Enregistrer'})) return;
  const r = await jpost('/api/strata/settings', body);
  if(!r.ok){ toast('Erreur : '+(r.error||'')); return; }
  toast(r.restarted ? 'Réglages enregistrés, redémarrage en cours' : 'Réglages enregistrés');
  strataTarget = fit.preset;
  await openStrata(fit.preset);
  if(typeof loadPresets === 'function') loadPresets();
}

// La config qui tourne (ou tournera) pour un modèle installé.
function strataRenderDetails(fit){
  const fld = document.getElementById('strata-details-fld');
  const d = fit && fit.details;
  fld.hidden = !d;
  if(!d) return;
  const k = n => (+n >= 1024 ? Math.round(+n/1024)+'K' : n);
  const rows = [
    ['Carte principale', d.main_gpu],
    ["Carte d'appoint", d.helper_gpu || 'aucune'],
    ['Lecture des images', d.vision_gpu || 'aucune'],
    ['Contexte', (+d.ctx).toLocaleString('fr-FR')+' jetons'],
    ['Cache KV', d.kv + (d.kv_resident ? ' · '+k(d.kv_resident)+' en VRAM, le reste en RAM' : '')],
    ['Prédiction (MTP)', d.spec+' jetons'],
    ['Experts hors GPU', (d.mode_label || (d.mmap ? 'lus depuis le disque (mmap)' : 'chargés en RAM'))
      + (fit.active ? ' — dernier lancement' : ' — selon la RAM libre maintenant')],
    ['Lecture des prompts', 'blocs de '+k(d.prefill)+', lecture directe sous '+d.short_read+' jetons'],
    ["Démarrage d'une conversation", 'reprise après le prompt système (dès '+d.cache_root+' jetons)'],
    ['Version du moteur', 'paquet AJEAN MoE '+d.version],
  ];
  document.getElementById('strata-details').innerHTML = rows.map(([a,b])=>
    '<div class="moe-kv"><span>'+escHtml(a)+'</span><span>'+escHtml(b)+'</span></div>').join('');
}

async function strataInstall(){
  const f = strataState && strataState.families.find(x=>x.id===strataFam);
  const fit = f && f.fits.find(x=>x.quant.id===strataQuant);
  if(!fit || fit.active) return;
  // déjà installé : simple bascule, la même que dans la liste des modèles
  if(fit.preset){
    closeStrata();
    await switchTo(-1, f.label+' '+strataQuant, fit.preset);
    loadStrata();
    return;
  }
  const msg = 'Installer Qwen3.8 Flash Next '+f.label+' '+strataQuant+' sur Strata ? Téléchargement : ~'
    +Math.round(fit.disk_gb)+' Go. Le modèle sera activé à la fin.';
  if(!await askConfirm(msg, {title:'Strata — Qwen3.8 Flash Next', okText:'Installer'})) return;
  const r = await jpost('/api/strata/install', {family:strataFam, quant:strataQuant});
  if(!r.ok){ toast('Erreur : '+(r.error||'')); return; }
  closeStrata();
  const det = document.getElementById('lc-details');
  if(det && 'open' in det) det.open = true;
  lcStartPolling();
}
