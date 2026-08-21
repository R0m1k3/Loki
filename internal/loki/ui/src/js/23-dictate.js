// ─── Dictée vocale ───────────────────────────────────────────────────────────
// Bouton micro de la carte de saisie : un clic enregistre, un second arrête et
// transcrit. La capture est encodée en WAV 16 kHz mono ICI, côté navigateur —
// whisper.cpp ne lit que du PCM, et encoder au bon format évite d'embarquer
// ffmpeg dans l'image. La transcription est locale (POST /api/transcribe →
// whisper-cli). Le texte s'ajoute au champ sans écraser ce qui s'y trouve.
//
// ⚠️ Le micro n'est autorisé par le NAVIGATEUR qu'en contexte sécurisé (HTTPS
// ou localhost) : servi en http://IP:8090, getUserMedia n'existe pas — on
// l'explique plutôt que d'échouer en silence.
let MIC = null; // {ctx, stream, src, node, chunks, rate, timer}

async function dictateToggle(){
  if(MIC){ dictateStop(); return; }
  if(!(navigator.mediaDevices && navigator.mediaDevices.getUserMedia)){
    toast('micro bloqué par le navigateur : il faut HTTPS (ou localhost)');
    return;
  }
  let stream;
  try{
    stream = await navigator.mediaDevices.getUserMedia({audio:{channelCount:1, echoCancellation:true, noiseSuppression:true}});
  }catch(e){ toast('accès au micro refusé'); return; }
  const ctx = new (window.AudioContext||window.webkitAudioContext)();
  const src = ctx.createMediaStreamSource(stream);
  // ScriptProcessor : déprécié mais universel, et amplement suffisant pour
  // copier des blocs de PCM (AudioWorklet demanderait un module séparé,
  // impossible dans un fichier unique servi inline).
  const node = ctx.createScriptProcessor(4096, 1, 1);
  const chunks = [];
  node.onaudioprocess = e => { chunks.push(new Float32Array(e.inputBuffer.getChannelData(0))); };
  src.connect(node); node.connect(ctx.destination);
  // Garde-fou : une dictée oubliée ne tourne pas des heures.
  MIC = {ctx, stream, src, node, chunks, rate:ctx.sampleRate, timer:setTimeout(dictateStop, 90000)};
  document.getElementById('mic').classList.add('rec');
}

async function dictateStop(){
  const m = MIC; if(!m) return; MIC = null;
  clearTimeout(m.timer);
  try{ m.src.disconnect(); m.node.disconnect(); }catch(_){}
  m.stream.getTracks().forEach(t => t.stop());
  try{ m.ctx.close(); }catch(_){}
  const btn = document.getElementById('mic');
  btn.classList.remove('rec'); btn.classList.add('busy'); btn.disabled = true;
  try{
    const wav = encodeWav16k(m.chunks, m.rate);
    if(wav.byteLength < 16000){ toast('rien entendu'); return; } // < 0,5 s
    const r = await jfetch('/api/transcribe', {method:'POST', headers:{'Content-Type':'audio/wav'}, body:wav});
    const j = await r.json().catch(()=>({}));
    if(r.status === 503 && j.downloading){
      // Le serveur relance le téléchargement à chaque tentative ; s'il a déjà
      // échoué (conteneur sans accès réseau, Hugging Face injoignable), il nous
      // renvoie la raison. La cacher derrière « 0 % » laissait l'utilisateur
      // cliquer indéfiniment sur un micro qui n'écrira jamais rien.
      toast(j.error ? 'modèle de dictée : échec du téléchargement — ' + j.error
                    : 'modèle de dictée en téléchargement (' + (j.pct||0) + ' %) — suivi dans Paramètres → Dictée');
      return;
    }
    if(!r.ok){ toast(j.error || 'échec de la transcription'); return; }
    const text = (j.text||'').trim();
    if(!text){ toast('rien compris — réessaie'); return; }
    const ta = document.getElementById('input');
    ta.value = ta.value.trim() ? ta.value.replace(/\s+$/,'') + ' ' + text : text;
    autoGrow(ta); ta.focus();
  }catch(_){ toast('erreur réseau'); }
  finally{ btn.classList.remove('busy'); btn.disabled = false; }
}

// Ré-échantillonne le PCM capturé (44,1/48 kHz selon la machine) vers du WAV
// 16 kHz mono 16 bits — le format d'entrée de whisper.cpp. Décimation simple :
// pour de la voix ré-échantillonnée VERS LE BAS, largement suffisant.
function encodeWav16k(chunks, fromRate){
  const toRate = 16000;
  let n = 0; for(const c of chunks) n += c.length;
  const all = new Float32Array(n); let o = 0;
  for(const c of chunks){ all.set(c, o); o += c.length; }
  const ratio = fromRate / toRate, outN = Math.floor(n / ratio);
  const pcm = new Int16Array(outN);
  for(let i = 0; i < outN; i++){
    const s = Math.max(-1, Math.min(1, all[Math.floor(i*ratio)] || 0));
    pcm[i] = s < 0 ? s*0x8000 : s*0x7fff;
  }
  const buf = new ArrayBuffer(44 + pcm.length*2), v = new DataView(buf);
  const wr = (off, s) => { for(let i = 0; i < s.length; i++) v.setUint8(off+i, s.charCodeAt(i)); };
  wr(0,'RIFF'); v.setUint32(4, 36 + pcm.length*2, true); wr(8,'WAVEfmt ');
  v.setUint32(16, 16, true); v.setUint16(20, 1, true); v.setUint16(22, 1, true);
  v.setUint32(24, toRate, true); v.setUint32(28, toRate*2, true);
  v.setUint16(32, 2, true); v.setUint16(34, 16, true);
  wr(36,'data'); v.setUint32(40, pcm.length*2, true);
  new Int16Array(buf, 44).set(pcm);
  return buf;
}
