// 11-external.js — presets « externes » : le chat part vers une API
// OpenAI-compatible distante (OpenAI, Groq, OpenRouter, un vLLM sur une autre
// machine…) au lieu du llama-server local.
//
// Un preset externe vit dans la même liste que les autres et se bascule pareil.
// Mais ses réglages n'ont rien de commun avec ceux d'un modèle local — ni
// quantification, ni couches GPU, ni moteur, ni projecteur vision : la machine
// distante décide de tout ça. D'où cette fenêtre à part, plutôt qu'un éditeur de
// preset aux trois quarts grisé.

// La clé n'est JAMAIS renvoyée en clair par le serveur : le champ s'affiche
// vide même quand une clé est enregistrée. Un champ vide ne vaut donc pas
// « efface la clé » — il faut y avoir touché. Sans ce drapeau, rouvrir la
// fenêtre pour corriger une faute de frappe dans l'URL effaçait la clé au
// passage, et le preset tombait en 401 au message suivant.
let extKeyTouched = false;
let extEditing = '';

function closeExternal(){ hideModal('ext-modal'); }

async function openExternal(id){
  extEditing = id || '';
  extKeyTouched = false;
  const set = (el, v) => { const e = document.getElementById(el); if(e) e.value = v; };
  document.getElementById('ext-title').textContent = id ? 'API externe' : 'Nouvelle API externe';
  document.getElementById('ext-del').style.display = id ? '' : 'none';
  document.getElementById('ext-test-out').textContent = '';
  document.getElementById('ext-key-sub').textContent = 'vide = aucune authentification';
  set('ext-name',''); set('ext-url',''); set('ext-model',''); set('ext-key',''); set('ext-ctx','');
  showModal('ext-modal');
  if(!id) return;
  let d = {};
  try{ d = await jget('/api/preset/external?id=' + encodeURIComponent(id)); }catch(_){ return; }
  if(extEditing !== id) return;           // une autre ouverture a pris la main
  set('ext-name', d.name || ''); set('ext-url', d.url || '');
  set('ext-model', d.model || ''); set('ext-ctx', d.ctx || '');
  if(d.hasKey){
    document.getElementById('ext-key-sub').textContent =
      'une clé est enregistrée — laisse vide pour la conserver';
  }
}

// Le corps commun aux deux routes (enregistrement et test).
function extPayload(){
  return {
    id: extEditing,
    name: (document.getElementById('ext-name').value || '').trim(),
    url: (document.getElementById('ext-url').value || '').trim(),
    model: (document.getElementById('ext-model').value || '').trim(),
    key: document.getElementById('ext-key').value || '',
    ctx: (document.getElementById('ext-ctx').value || '').trim(),
    keyTouched: extKeyTouched,
  };
}

// Tester AVANT d'enregistrer n'est pas un confort : une URL mal collée ou une
// clé expirée ne se voit sinon qu'au premier message, sous la forme d'un code
// HTTP nu au milieu d'une conversation.
async function testExternal(){
  const out = document.getElementById('ext-test-out');
  const btn = document.getElementById('ext-test');
  const p = extPayload();
  if(!p.url || !p.model){ out.innerHTML = '<span style="color:var(--err)">URL et modèle requis</span>'; return; }
  btn.disabled = true;
  out.textContent = 'appel de test en cours…';
  let r = {};
  try{ r = await jpost('/api/preset/external/test', p); }
  catch(_){ r = {ok:false, error:'réseau'}; }
  btn.disabled = false;
  out.innerHTML = r.ok
    ? '<span style="color:var(--ok)">✓ l’API répond</span>'
    : '<span style="color:var(--err)">' + escHtml((r.status ? r.status + ' — ' : '') + (r.error || 'échec')) + '</span>';
}

async function saveExternal(){
  const p = extPayload();
  if(!p.name){ toast('nom requis'); return; }
  if(!p.url){ toast('URL requise'); return; }
  if(!p.model){ toast('modèle requis'); return; }
  const r = await jpost('/api/preset/external/save', p);
  if(!r.ok){ toast('erreur : ' + (r.error||'')); return; }
  toast('enregistré');
  closeExternal();
  loadPresets();
}

// Même route de suppression que n'importe quel preset — et même refus si c'est
// celui en service. Pas de case « supprimer aussi le .gguf » : il n'y a pas de
// fichier, le modèle est chez quelqu'un d'autre.
async function deleteExternal(){
  if(!extEditing) return;
  const name = (document.getElementById('ext-name').value || '').trim() || extEditing;
  if(!await askConfirm('Supprimer le preset « ' + name + ' » ?',
      {title:'Suppression', okText:'Supprimer', danger:true})) return;
  const r = await jpost('/api/preset/delete', {id: extEditing});
  if(!r.ok){ toast('erreur : ' + (r.error||'')); return; }
  toast('supprimé');
  closeExternal();
  loadPresets();
}
