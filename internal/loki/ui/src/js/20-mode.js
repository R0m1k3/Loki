// 20-mode.js — le mode Chat/Code : sélecteur du composeur, puce de suggestion,
// panneau des critères d'acceptation, carte de question (outil ask).
// La bascule est PAR DISCUSSION et vit côté serveur (/api/chat/mode) : tous
// les appareils voient le même mode, comme le fil lui-même.

let CURRENT_MODE = 'chat';

function modeSeg(){ return document.getElementById('mode-seg'); }

function paintMode(){
  const seg = modeSeg();
  if(!seg) return;
  seg.querySelector('[data-mode="chat"]').classList.toggle('on', CURRENT_MODE!=='code');
  seg.querySelector('[data-mode="code"]').classList.toggle('on', CURRENT_MODE==='code');
  // Le panneau des critères n'a de sens qu'en mode code.
  const box = document.getElementById('criteria-box');
  if(box && CURRENT_MODE!=='code') box.style.display='none';
}

// Bascule demandée depuis CE navigateur.
async function setMode(mode){
  if(mode===CURRENT_MODE) return;
  try{
    const r = await jfetch('/api/chat/mode', {method:'POST', body: JSON.stringify({mode})});
    const j = await r.json();
    if(j.ok){ CURRENT_MODE = j.mode; paintMode(); hideCodeHint(); }
  }catch(e){ toast('bascule impossible : '+e); }
}

// Bascule reçue du serveur (autre appareil, ou écho de la nôtre).
function applyModeDelta(mode){
  CURRENT_MODE = mode==='code' ? 'code' : 'chat';
  paintMode();
  if(CURRENT_MODE==='code') hideCodeHint();
}

// Recharge le mode + critères au chargement et à chaque bascule de discussion.
async function modeOnConvChange(){
  try{
    const r = await jfetch('/api/chat/mode');
    const j = await r.json();
    if(!j.ok) return;
    CURRENT_MODE = j.mode==='code' ? 'code' : 'chat';
    paintMode();
    renderCriteria(j.criteria||[]);
    hideCodeHint();
  }catch(e){}
}

// --- Puce « passer en mode Code ? » -----------------------------------------
// Émise par le serveur (delta code_hint) quand un message de mode chat
// ressemble à une tâche de code. Ignorable, jamais bloquante.

function showCodeHint(){
  if(CURRENT_MODE==='code' || document.getElementById('code-hint')) return;
  const composer = document.getElementById('composer');
  if(!composer) return;
  const chip = document.createElement('div');
  chip.id = 'code-hint';
  chip.innerHTML = 'ça ressemble à une tâche de code — <button type="button" id="code-hint-yes">passer en mode Code</button><button type="button" id="code-hint-no" title="ignorer" aria-label="ignorer">✕</button>';
  composer.parentNode.insertBefore(chip, composer);
  chip.querySelector('#code-hint-yes').onclick = ()=>{ setMode('code'); };
  chip.querySelector('#code-hint-no').onclick = hideCodeHint;
}

function hideCodeHint(){
  const chip = document.getElementById('code-hint');
  if(chip) chip.remove();
}

// --- Panneau des critères ----------------------------------------------------
// Rendu par les deltas {criteria:[...]} (outil criteria, passe de vérif, ou
// édition manuelle). Repliable ; en-tête = compteur passés / total.

function renderCriteria(list){
  let box = document.getElementById('criteria-box');
  if(!list || !list.length){ if(box) box.remove(); return; }
  if(CURRENT_MODE!=='code'){ if(box) box.remove(); return; }
  if(!box){
    const composer = document.getElementById('composer');
    if(!composer) return;
    box = document.createElement('div');
    box.id = 'criteria-box';
    box.innerHTML = '<div id="criteria-head"></div><ul id="criteria-list"></ul>';
    composer.parentNode.insertBefore(box, composer);
    box.querySelector('#criteria-head').onclick = ()=> box.classList.toggle('folded');
  }
  box.style.display='';
  const passed = list.filter(c=>c.status==='passed').length;
  box.querySelector('#criteria-head').textContent =
    'critères ' + passed + '/' + list.length + (passed===list.length ? ' ✓' : '');
  box.classList.toggle('allpass', passed===list.length);
  const ul = box.querySelector('#criteria-list');
  ul.innerHTML='';
  for(const c of list){
    const li = document.createElement('li');
    li.className = 'crit-'+(c.status||'pending');
    const mark = c.status==='passed' ? '✓' : (c.status==='failed' ? '✗' : '○');
    li.textContent = mark+' '+c.text + (c.note ? ' — '+c.note : '');
    ul.appendChild(li);
  }
}

function onVerifyDone(status){
  if(status==='passed') toast('vérification : tous les critères passent ✓');
}

// --- Carte de question (outil ask) ------------------------------------------
// Le modèle a posé une question structurée et clos son tour : la carte propose
// ses options en boutons ; cliquer envoie la réponse comme un message normal.

function renderAskCard(ask){
  if(!ask || !ask.question) return;
  removeTyping();
  const el = addMsg('assistant', '');
  el.classList.add('askcard');
  const body = el.querySelector('.body');
  body.textContent = ask.question;
  if(ask.options && ask.options.length){
    const row = document.createElement('div');
    row.className = 'askopts';
    for(const opt of ask.options){
      const b = document.createElement('button');
      b.type = 'button';
      b.textContent = opt;
      b.onclick = ()=>{
        // La réponse part comme un message utilisateur ordinaire.
        const input = document.getElementById('input');
        input.value = opt;
        send();
        row.querySelectorAll('button').forEach(x=>x.disabled=true);
      };
      row.appendChild(b);
    }
    body.appendChild(row);
  }
  scrollMaybe();
}

// Init : état du mode au chargement de la page.
window.addEventListener('DOMContentLoaded', modeOnConvChange);
