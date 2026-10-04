// Optimiseur sans perte (backend_tune*.go). Il tourne côté serveur, en tâche de
// fond : la modale lance, suit, montre les mesures — et n'écrit RIEN sans un
// clic (copie du preset, ou ce preset après confirmation du diff).
let tunePoll = null, tuneState = 'form';
// Étapes de base (tuneStages côté serveur) ; placement et opt-in ont leur case.
const TUNE_STAGES = [['lots','micro-lot et lot'], ['threads','threads'], ['delestage','délestage'], ['marges','marges --fit'], ['files','files CUDA']];
function closeTuneModal(){ clearTimeout(tunePoll); hideModal('tune-modal'); }
function tuneBtns(state){
  tuneState = state;
  const show = (id, on) => { const b = document.getElementById(id); if(b) b.style.display = on ? '' : 'none'; };
  show('tune-start', state !== 'running');
  const sb = document.getElementById('tune-start');
  if(sb) sb.textContent = state === 'form' ? 'lancer' : 'nouvelle mesure…';
  show('tune-cancel', state === 'running');
  show('tune-copy', state === 'result');
  show('tune-apply', state === 'result');
}
function tuneSec(s){
  s = Math.max(0, Math.round(s||0));
  return s >= 60 ? Math.floor(s/60) + ' min ' + String(s%60).padStart(2,'0') + ' s' : s + ' s';
}
async function openTuneModal(){
  showModal('tune-modal');
  const body = document.getElementById('tune-body');
  body.innerHTML = '<div class="muted">…</div>';
  let st = {};
  try{ st = await jget('/api/tune/status'); }catch(_){}
  // Une mesure finie (modale fermée pendant qu'elle tournait) : son résultat
  // d'abord, avec ses boutons — « nouvelle mesure… » ramène au formulaire.
  if(st.running || st.applying || st.apply || (st.result && st.result.preset_id === editingKey)){ tuneWatch(); return; }
  let last = null;
  if(editingKey){ try{ last = await jget('/api/tune/last?id=' + encodeURIComponent(editingKey)); }catch(_){} }
  tuneForm(last && last.ok ? last : null, st.elsewhere);
}
function tuneForm(last, elsewhere){
  const body = document.getElementById('tune-body');
  let h = '<div class="pe-note">Des essais sur un moteur <b>privé</b> (127.0.0.1), chacun mesuré au bench complet : '
    + 'micro-lot et lot, threads (poids sur CPU), seuil de délestage, marges <code>--fit</code> et files CUDA (2 cartes ou plus). '
    + 'Jamais touchés : modèle, contexte, cache KV, raisonnement, échantillonnage, slots. Le moteur principal est arrêté pendant la mesure '
    + '(chat et tâches en pause), puis relancé. Rien n\'est écrit sans ton accord.</div>'
    + '<div class="pe-note muted">Mesure la version <b>enregistrée</b> du preset en service.</div>'
    + '<div style="display:flex;gap:10px;flex-wrap:wrap;font-size:12px"><span class="muted">Étapes :</span>'
    + TUNE_STAGES.map(s => '<label style="display:flex;gap:4px;align-items:center"><input type="checkbox" class="tune-stage" value="' + s[0] + '" checked> ' + s[1] + '</label>').join('')
    + '</div>'
    + '<label style="display:flex;gap:8px;align-items:flex-start"><input type="checkbox" id="tune-placement"> <span>Inclure le placement'
    + '<span class="muted" style="display:block;font-size:11px">--fit au lieu des experts MoE ou du --tensor-split placés à la main : réécrit EXTRA_ARGS, confirmé à part</span></span></label>'
    + '<label style="display:flex;gap:8px;align-items:flex-start"><input type="checkbox" id="tune-optin"> <span>Inclure les options opt-in'
    + '<span class="muted" style="display:block;font-size:11px">SPEC (sortie inchangée, jugée sur prose et code), SPEC_N_MAX, CUDA_GRAPH_OPT (expérimental), --backend-sampling</span></span></label>'
    + '<label style="display:flex;gap:8px;align-items:center">Budget <input type="number" id="tune-budget" class="pe-val" min="5" max="360" step="5" value="30" style="width:80px"> minutes'
    + '<span class="muted" style="font-size:11px">— au-delà, résultat partiel</span></label>';
  if(elsewhere) h += '<div class="pe-note" style="color:var(--warn)">Une optimisation tourne déjà (' + escHtml(elsewhere) + ').</div>';
  if(last && last.result){
    const r = last.result;
    const when = new Date(r.at*1000).toLocaleString();
    h += '<div class="pe-note">Dernière optimisation : ' + escHtml(when) + (r.best ? ' — ' + escHtml(r.best) + ', −' + (r.gain*100).toFixed(1) + ' % par tour' : ' — rien de mieux trouvé') + '</div>';
    if(last.engine_changed) h += '<div class="pe-note" style="color:var(--warn)">Le moteur a changé depuis (' + escHtml(last.engine_changed) + ') : les meilleurs réglages ont pu bouger — relance l\'optimisation.</div>';
    if(last.preset_changed) h += '<div class="pe-note muted">Le preset a été modifié depuis cette mesure.</div>';
  }
  body.innerHTML = h;
  tuneBtns('form');
}
async function startTuneUI(){
  if(tuneState !== 'form'){
    let last = null;
    if(editingKey){ try{ last = await jget('/api/tune/last?id=' + encodeURIComponent(editingKey)); }catch(_){} }
    tuneForm(last && last.ok ? last : null);
    return;
  }
  const placement = !!(document.getElementById('tune-placement')||{}).checked;
  const optin = !!(document.getElementById('tune-optin')||{}).checked;
  const budget = parseInt((document.getElementById('tune-budget')||{}).value, 10) || 30;
  // Toutes cochées = toutes (liste vide) ; aucune = seulement placement / opt-in.
  const boxes = [...document.querySelectorAll('.tune-stage')];
  const picked = boxes.filter(b => b.checked).map(b => b.value);
  const stages = picked.length === boxes.length ? [] : (picked.length ? picked : ['aucune']);
  const msg = 'Le moteur principal va être ARRÊTÉ pendant jusqu\'à ' + budget + ' minutes : chat, tâches et clients /v1 attendent. '
    + 'Chaque essai recharge le modèle. Lancer l\'optimisation ?';
  if(!await askConfirm(msg, {title:'Optimiser', okText:'Lancer'})) return;
  let r;
  try{ r = await jpost('/api/tune', {placement, optin, budget_min: budget, stages}); }catch(e){ r = {ok:false, error:e.message}; }
  if(!r.ok){ document.getElementById('tune-body').insertAdjacentHTML('beforeend', '<div style="color:var(--err)">' + escHtml(r.error||'?') + '</div>'); return; }
  tuneWatch();
}
async function cancelTuneUI(){ try{ await jpost('/api/tune/cancel', {}); }catch(_){} }
function tuneTrialsHTML(trials){
  if(!trials || !trials.length) return '';
  let h = '<table style="width:100%;font-size:12px;border-collapse:collapse"><tr class="muted"><td>étape</td><td>essai</td>'
    + '<td style="text-align:right">tour</td><td style="text-align:right">VRAM libre</td><td>statut</td></tr>';
  for(const t of trials){
    const color = t.status === 'retenu' ? 'var(--ok)' : (t.status === 'échec' || t.status === 'écarté') ? 'var(--warn)' : 'inherit';
    const runs = (t.runs||[]);
    let tip = '';
    if(runs.length){
      const r0 = runs[0];
      tip = 'prose ' + (r0.prose_pp||0).toFixed(0) + ' / ' + (r0.prose_tg||0).toFixed(1) + ' t/s';
      if(r0.depth) tip += ' · à ' + r0.depth + ' jetons : froid ' + (r0.cold_pp||0).toFixed(0) + ', tours ' + (r0.cached_pp||0).toFixed(0) + ', decode ' + (r0.tg||0).toFixed(1) + ' t/s';
      tip += ' · ' + runs.length + ' passage(s)';
    }
    if(t.offloaded) tip += ' · couches ' + t.offloaded;
    h += '<tr title="' + escHtml(tip) + '"><td class="muted" style="padding:3px 6px 3px 0">' + escHtml(t.stage) + '</td>'
      + '<td>' + escHtml(t.label) + '</td>'
      + '<td style="text-align:right">' + (t.turn_sec ? t.turn_sec.toFixed(1) + ' s' : '—') + '</td>'
      + '<td style="text-align:right">' + (t.headroom_mib >= 0 ? t.headroom_mib + ' Mio' : '—') + '</td>'
      + '<td style="color:' + color + ';padding-left:6px">' + escHtml(t.status) + (t.why ? ' <span class="muted">· ' + escHtml(t.why) + '</span>' : '') + '</td></tr>';
  }
  return h + '</table>';
}
function tuneResultHTML(r){
  let h = '<div class="pe-note muted">Score : durée d\'un tour type — ' + (r.weights.k).toFixed(0) + ' jetons lus (cache repris), '
    + (r.weights.g).toFixed(0) + ' écrits, ' + (r.weights.f_cold*100).toFixed(0) + ' % des tours relus à froid à ' + r.depth + ' jetons ('
    + escHtml(r.weights.from) + '). Un gain compte au-delà de 3 % et de l\'écart entre passages. Moteur ' + escHtml(r.engine||'?') + '.</div>';
  h += tuneTrialsHTML([r.baseline].concat(r.trials||[]));
  for(const s of (r.skipped||[])) h += '<div class="muted" style="font-size:11px">sauté — ' + escHtml(s) + '</div>';
  for(const n of (r.notes||[])) h += '<div style="font-size:11px;color:var(--warn)">' + escHtml(n) + '</div>';
  if(r.partial) h += '<div style="font-size:11px;color:var(--warn)">résultat partiel : ' + escHtml(r.partial) + '</div>';
  if(r.best){
    h += '<div style="margin-top:6px"><b style="color:var(--ok)">Meilleur : ' + escHtml(r.best) + '</b> — −' + (r.gain*100).toFixed(1) + ' % par tour</div>'
      + '<div class="pe-note" style="white-space:pre-wrap;font-family:var(--mono,monospace);font-size:11px">' + escHtml((r.changes||[]).join('\n')) + '</div>';
    if(r.placement) h += '<div class="pe-note" style="color:var(--warn)">Ce résultat réécrit EXTRA_ARGS (placement) : à confirmer à part.</div>';
  } else {
    h += '<div style="margin-top:6px">La configuration actuelle reste la meilleure : rien à changer.</div>';
  }
  return h;
}
function tuneWatch(){
  clearTimeout(tunePoll);
  const body = document.getElementById('tune-body');
  const tick = async () => {
    let st;
    try{ st = await jget('/api/tune/status'); }
    catch(_){ tunePoll = setTimeout(tick, 3000); return; }
    if(st.running){
      tuneBtns('running');
      const eta = st.eta_sec >= 0 ? ' · reste ~' + tuneSec(st.eta_sec) : '';
      body.innerHTML = '<div class="muted" style="text-align:center">⏳ ' + escHtml(st.phase||'…') + '<br>' + tuneSec(st.elapsed_sec) + eta
        + '<br><span style="font-size:11px">moteur principal arrêté · chat et tâches en pause</span></div>' + tuneTrialsHTML(st.trials);
      tunePoll = setTimeout(tick, 2000);
      return;
    }
    let h = '';
    if(st.error) h += '<div style="color:var(--err)">' + escHtml(st.error) + '</div>';
    if(st.result) h += tuneResultHTML(st.result);
    const a = st.apply;
    if(a){
      h += '<div class="pe-note">' + (a.log||[]).map(escHtml).join('<br>') + '</div>';
      if(a.message) h += '<div style="color:var(--ok)">' + escHtml(a.message) + '</div>';
      if(a.error) h += '<div style="color:var(--err)">' + escHtml(a.error) + '</div>';
    }
    body.innerHTML = h || '<div class="muted">rien à montrer</div>';
    const canApply = !!(st.result && st.result.best) && !st.applying && !(a && a.message);
    tuneBtns(canApply ? 'result' : 'done');
    if(st.applying){ tunePoll = setTimeout(tick, 2000); }
  };
  tick();
}
async function applyTuneUI(target){
  let st = {};
  try{ st = await jget('/api/tune/status'); }catch(_){}
  const r = st.result;
  if(!r || !r.best) return;
  const diff = (r.changes||[]).map(c => '• ' + c).join('\n');
  if(target === 'copy'){
    if(!await askConfirm('Créer une COPIE de « ' + r.preset_name + ' » avec :\n\n' + diff + '\n\nLe preset d\'origine ne bouge pas.',
                         {title:'Enregistrer dans une copie', okText:'Créer la copie'})) return;
  } else {
    if(!await askConfirm('Réécrire « ' + r.preset_name + ' » (sauvegardé avant) avec :\n\n' + diff
                         + '\n\nLe moteur redémarre, puis une sonde vérifie un prompt à ' + r.depth + ' jetons ; en cas d\'échec, l\'ancienne version est rétablie.',
                         {title:'Appliquer à ce preset', okText:'Appliquer'})) return;
    if(r.placement && !await askConfirm('Ce résultat RÉÉCRIT EXTRA_ARGS (placement des experts ou --tensor-split retirés pour --fit). Confirmer ?',
                                        {title:'Placement', okText:'Confirmer'})) return;
  }
  let res;
  try{ res = await jpost('/api/tune/apply', {target}); }catch(e){ res = {ok:false, error:e.message}; }
  if(!res.ok){ toast('erreur : ' + (res.error||'')); return; }
  if(target === 'copy'){ toast(res.message || 'copie créée'); KINDS.preset.reload(); return; }
  tuneWatch();
}
