// ─── Réglages de la dictée ───────────────────────────────────────────────────
// Panneau Paramètres → Dictée : matériel, modèle, langue, réactivité, plus un
// test du micro. Ce sont des réglages de SERVEUR (le modèle chargé et le GPU
// occupé appartiennent à la machine), donc rien n'est gardé en localStorage :
// tout passe par /api/dictate/*.
let DIC_CFG = null;

async function loadDictateSettings(){
  const [cfg, modeles, gpus] = await Promise.all([
    jfetch('/api/dictate/config').then(r => r.json()).catch(()=>({})),
    jfetch('/api/dictate/models').then(r => r.json()).catch(()=>[]),
    jfetch('/api/vram').then(r => r.json()).catch(()=>[]),
  ]);
  DIC_CFG = cfg || {};

  // Matériel : « CPU seul » d'abord — c'est le choix qui marche partout, y
  // compris sur une image bâtie sans CUDA.
  const dev = document.getElementById('dic-device');
  dev.innerHTML = '';
  dev.appendChild(new Option('processeur seul', 'cpu'));
  (gpus||[]).forEach((g, i) => {
    const libre = Math.max(0, (g.total|0) - (g.used|0));
    dev.appendChild(new Option(`GPU ${i} — ${g.name} (${libre} Mo libres sur ${g.total|0})`, String(i)));
  });
  dev.value = cfg.device || 'cpu';

  const mod = document.getElementById('dic-model');
  mod.innerHTML = '';
  (modeles||[]).forEach(m => {
    const mo = Math.round(m.octets / 1e6);
    // Dire ce qui est déjà là : sans ça, choisir un modèle absent ressemble à
    // un réglage instantané alors que c'est un téléchargement de 600 Mo.
    const etat = m.present ? 'déjà téléchargé' : mo + ' Mo à télécharger';
    mod.appendChild(new Option(`${m.nom} — ${etat}, ~${m.vram_mo} Mo de mémoire`, m.id));
  });
  mod.value = cfg.model || '';

  document.getElementById('dic-lang').value = cfg.lang || 'fr';
  document.getElementById('dic-react').value = cfg.reactivity || 'moyen';
  refreshDictateState();
}

async function dictateSave(){
  const cfg = {
    model: document.getElementById('dic-model').value,
    lang: document.getElementById('dic-lang').value,
    device: document.getElementById('dic-device').value,
    reactivity: document.getElementById('dic-react').value,
  };
  const el = document.getElementById('dic-save-state');
  el.textContent = 'enregistrement…';
  const r = await jfetch('/api/dictate/config', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify(cfg)});
  const j = await r.json().catch(()=>({}));
  if(!r.ok){ el.textContent = j.error || 'échec'; return; }
  DIC_CFG = cfg;
  // Le serveur de dictée a été arrêté côté Loki : le prochain clic sur le
  // micro le relancera avec ces réglages. On le dit, sinon la première dictée
  // suivante paraît anormalement lente sans raison visible.
  el.textContent = 'enregistré — la prochaine dictée rechargera le modèle';
  refreshDictateState();
}

async function dictateDownload(){
  const id = document.getElementById('dic-model').value;
  const el = document.getElementById('dic-dl-state');
  el.textContent = 'démarrage…';
  const r = await jfetch('/api/dictate/models/download', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({id})});
  const j = await r.json().catch(()=>({}));
  if(!r.ok){ el.textContent = j.error || 'échec'; return; }
  suivreTelechargement();
}

// Sondage tant que le téléchargement tourne. Un modèle de 1 Go prend plusieurs
// minutes : sans progression affichée, l'utilisateur ne sait pas s'il se passe
// quelque chose.
async function suivreTelechargement(){
  const el = document.getElementById('dic-dl-state');
  const r = await jfetch('/api/dictate/models/download');
  const j = await r.json().catch(()=>({}));
  if(j.error){ el.textContent = 'échec : ' + j.error; return; }
  if(j.running){
    el.textContent = `téléchargement de ${j.id} — ${j.pct||0} %`;
    setTimeout(suivreTelechargement, 1000);
    return;
  }
  el.textContent = 'téléchargé';
  loadDictateSettings();
}

async function refreshDictateState(){
  const el = document.getElementById('dic-state');
  if(!el) return;
  const j = await jfetch('/api/dictate/state').then(r => r.json()).catch(()=>null);
  if(!j){ el.textContent = ''; return; }
  const bouts = [];
  bouts.push(j.actif ? `moteur allumé depuis ${j.depuis_s||0} s` : 'moteur éteint (il démarre au premier clic sur le micro)');
  bouts.push(j.present ? `modèle ${j.modele} présent` : `modèle ${j.modele} ABSENT — à télécharger`);
  // note : le serveur nous dit quand il n'a pas pu respecter le matériel
  // demandé (image sans CUDA, par exemple). Le taire ferait croire que le GPU
  // choisi est utilisé.
  if(j.note) bouts.push('⚠ ' + j.note);
  if(j.dl && j.dl.running) bouts.push(`téléchargement ${j.dl.id} ${j.dl.pct||0} %`);
  el.textContent = bouts.join(' · ');
}

// Test du micro : capte 3 s, mesure le niveau réel et transcrit, SANS toucher
// au champ de saisie. C'est le diagnostic qu'il fallait bricoler à la main
// quand la dictée ne rendait rien — niveau nul, micro muet, serveur en panne
// se ressemblent tous de l'extérieur.
async function dictateTestMic(){
  const out = document.getElementById('dic-test-out');
  const btn = document.getElementById('dic-test');
  if(!(navigator.mediaDevices && navigator.mediaDevices.getUserMedia)){
    out.textContent = 'micro bloqué par le navigateur : il faut HTTPS (ou localhost)';
    return;
  }
  btn.disabled = true;
  out.textContent = 'écoute… parle maintenant (3 s)';
  let stream;
  try{
    stream = await navigator.mediaDevices.getUserMedia({audio:{channelCount:1, echoCancellation:true, noiseSuppression:true}});
  }catch(e){ out.textContent = 'accès au micro refusé'; btn.disabled = false; return; }
  const ctx = new (window.AudioContext||window.webkitAudioContext)();
  const src = ctx.createMediaStreamSource(stream);
  const node = ctx.createScriptProcessor(4096, 1, 1);
  const chunks = [];
  node.onaudioprocess = e => chunks.push(new Float32Array(e.inputBuffer.getChannelData(0)));
  src.connect(node); node.connect(ctx.destination);
  await new Promise(r => setTimeout(r, 3000));
  try{ src.disconnect(); node.disconnect(); }catch(_){}
  stream.getTracks().forEach(t => t.stop());
  const rate = ctx.sampleRate;
  try{ ctx.close(); }catch(_){}

  let n = 0; for(const c of chunks) n += c.length;
  let somme = 0, crete = 0;
  for(const c of chunks) for(let i = 0; i < c.length; i++){
    const v = c[i]; somme += v*v;
    const a = Math.abs(v); if(a > crete) crete = a;
  }
  const rms = Math.sqrt(somme / (n||1));
  const niveau = rms < 0.002 ? 'QUASI MUET' : rms < 0.01 ? 'faible' : 'correct';
  out.textContent = `niveau ${niveau} (rms ${rms.toFixed(4)}, crête ${crete.toFixed(3)}) — transcription…`;

  const wav = encodeWav16k(chunks, rate);
  const r = await jfetch('/api/transcribe', {method:'POST', headers:{'Content-Type':'audio/wav'}, body: wav});
  const j = await r.json().catch(()=>({}));
  const tete = `niveau ${niveau} (rms ${rms.toFixed(4)}) — `;
  if(!r.ok) out.textContent = tete + (j.error || 'échec de la transcription');
  else if(!(j.text||'').trim()) out.textContent = tete + 'rien compris' + (rms < 0.002 ? ' (le micro n’a rien capté : vérifie l’entrée choisie par le système)' : '');
  else out.textContent = tete + '« ' + j.text.trim() + ' »';
  btn.disabled = false;
  refreshDictateState();
}
