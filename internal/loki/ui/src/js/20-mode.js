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
  paintHarness();
}

// --- Harnais du mode Code (Loki | Claude Code) -------------------------------
// Par discussion, côté façade (/api/chat/harness). HARNESS_OK=false : route
// absente (Go seul) ou illisible, le sélecteur reste caché.
let CURRENT_HARNESS = 'loki';
let HARNESS_OK = false;
let HARNESS_INFO = {state:'', version:'', error:''};
let HARNESS_POLL = null;

function paintHarness(){
  const seg = document.getElementById('harness-seg');
  if(!seg) return;
  seg.style.display = (HARNESS_OK && CURRENT_MODE==='code') ? '' : 'none';
  const cl = seg.querySelector('[data-harness="claude"]');
  seg.querySelector('[data-harness="loki"]').classList.toggle('on', CURRENT_HARNESS!=='claude');
  cl.classList.toggle('on', CURRENT_HARNESS==='claude');
  const st = HARNESS_INFO.state;
  const busy = st==='installing' || st==='updating';
  cl.classList.toggle('busy', CURRENT_HARNESS==='claude' && busy);
  cl.textContent = (CURRENT_HARNESS==='claude' && busy)
    ? (st==='installing' ? 'Claude Code…' : 'Claude Code ↻') : 'Claude Code';
  cl.title = st==='ready' ? ('Claude Code '+HARNESS_INFO.version)
    : st==='failed' ? ('installation ratée : '+HARNESS_INFO.error)
    : st==='installing' ? 'installation de Claude Code…'
    : st==='updating' ? 'mise à jour de Claude Code…'
    : 'Claude Code (installé au premier choix)';
}

function harnessInfo(j){
  CURRENT_HARNESS = j.harness==='claude' ? 'claude' : 'loki';
  HARNESS_INFO = {state:j.state||'', version:j.version||'', error:j.error||''};
  paintHarness();
  // Installation en cours : on suit son état toutes les 3 s.
  const busy = HARNESS_INFO.state==='installing' || HARNESS_INFO.state==='updating';
  if(busy && !HARNESS_POLL){
    HARNESS_POLL = setInterval(harnessRefresh, 3000);
  }else if(!busy && HARNESS_POLL){
    clearInterval(HARNESS_POLL); HARNESS_POLL = null;
    if(HARNESS_INFO.state==='failed' && CURRENT_HARNESS==='claude') toast('Claude Code : '+HARNESS_INFO.error);
  }
}

async function harnessRefresh(){
  try{
    const r = await jfetch('/api/chat/harness');
    const j = await r.json();
    HARNESS_OK = !!j.ok;
    if(j.ok) harnessInfo(j); else paintHarness();
  }catch(e){ HARNESS_OK = false; paintHarness(); }
}

async function setHarness(h){
  if(h===CURRENT_HARNESS) return;
  try{
    const r = await jfetch('/api/chat/harness', {method:'POST', body: JSON.stringify({harness:h})});
    const j = await r.json();
    if(j.ok) harnessInfo(j); else toast('harnais : '+(j.error||'refusé'));
  }catch(e){ toast('bascule impossible : '+e); }
}

// Bascule reçue du serveur : l'état d'installation suit par la route.
function applyHarnessDelta(h){
  CURRENT_HARNESS = h==='claude' ? 'claude' : 'loki';
  paintHarness();
  harnessRefresh();
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
  harnessRefresh();
}

// --- Puce « passer en mode Code ? » -----------------------------------------
// Émise par le serveur (delta code_hint) quand un message de mode chat
// ressemble à une tâche de code. Ignorable, jamais bloquante.

// Insère un bandeau DANS le composeur, au-dessus de la barre de saisie.
// #composer est en position:absolute : un élément posé AVANT lui dans le DOM
// reste dans le flux normal de la page et se retrouve donc CACHÉ dessous. Ses
// enfants, eux, suivent sa colonne (--col) et son empilement.
function composerBanner(id){
  const composer = document.getElementById('composer');
  if(!composer) return null;
  let el = document.getElementById(id);
  if(!el){
    el = document.createElement('div');
    el.id = id;
    el.className = 'composer-banner';
    composer.insertBefore(el, composer.firstChild);
  }
  return el;
}

function showCodeHint(){
  if(CURRENT_MODE==='code' || document.getElementById('code-hint')) return;
  const chip = composerBanner('code-hint');
  if(!chip) return;
  chip.innerHTML = 'ça ressemble à une tâche de code — <button type="button" id="code-hint-yes">passer en mode Code</button><button type="button" id="code-hint-no" title="ignorer" aria-label="ignorer">✕</button>';
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
  const fresh = !box;
  box = composerBanner('criteria-box');
  if(!box) return;
  if(fresh){
    box.innerHTML = '<div id="criteria-head"></div><ul id="criteria-list"></ul>';
    box.querySelector('#criteria-head').onclick = ()=> box.classList.toggle('folded');
    // Repliée par défaut : cinq critères dépliés mangeaient la moitié de
    // l'écran au-dessus de la saisie. L'en-tête porte déjà le compteur.
    box.classList.add('folded');
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
    const mark = c.status==='passed' ? '✓' : (c.status==='failed' ? '✗' : (c.status==='completed' ? '◐' : '○'));
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
