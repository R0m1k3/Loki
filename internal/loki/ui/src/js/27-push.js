// 27-push.js — notifications Web Push côté client (voir push.go / sw.js).
//
// Le SERVEUR pousse une notif à la fin d'un tour et à la fin d'une tâche
// planifiée, même app fermée ou téléphone verrouillé. Ici on ne gère que
// l'INSCRIPTION : enregistrer le service worker, demander la permission (sur
// clic — un geste utilisateur est obligatoire), s'abonner avec la clé publique
// VAPID du serveur, et lui transmettre l'abonnement.
//
// ⚠️ Le service worker est un fichier de l'ORIGINE (/sw.js), pas un appel
// /api : il doit venir de la racine pour couvrir toute l'app. La clé VAPID et
// l'enregistrement de l'abonnement, eux, passent par jfetch (donc par la clé
// de pilotage, et par le canal E2E à distance).

// pushSupported : le navigateur sait-il faire du Web Push ? (Safari iOS hors
// PWA installée, vieux navigateurs → non.)
function pushSupported(){
  return ('serviceWorker' in navigator) && ('PushManager' in window) && ('Notification' in window);
}

// La clé VAPID est renvoyée en base64url ; PushManager.subscribe veut un Uint8Array.
function urlB64ToUint8Array(base64){
  const pad = '='.repeat((4 - base64.length % 4) % 4);
  const b64 = (base64 + pad).replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(b64);
  const out = new Uint8Array(raw.length);
  for(let i=0;i<raw.length;i++) out[i]=raw.charCodeAt(i);
  return out;
}

// Enregistre (une seule fois) le service worker et renvoie sa registration.
// Scope racine : le worker doit couvrir toute l'app pour recevoir les push.
let _swReg = null;
async function pushRegisterSW(){
  if(_swReg) return _swReg;
  _swReg = await navigator.serviceWorker.register('/sw.js', {scope:'/'});
  return _swReg;
}

function pushSetStatus(msg){
  const el = document.getElementById('push-status');
  if(el) el.textContent = msg || '';
}

// Reflète l'état RÉEL (abonné ou non, permission refusée, non supporté) dans
// l'interrupteur et la ligne d'état. Appelé au chargement et après bascule.
async function pushRefresh(){
  const cb = document.getElementById('push-toggle');
  if(!cb) return;
  if(!pushSupported()){
    cb.checked = false; cb.disabled = true;
    // iOS ne sait faire du Web Push QUE depuis une PWA ajoutée à l'écran d'accueil.
    if(document.documentElement.getAttribute('data-pwa')!=='1' && /iphone|ipad|ipod/i.test(navigator.userAgent))
      pushSetStatus("sur iPhone : ajoute d'abord Loki à l'écran d'accueil, puis active depuis l'app installée.");
    else
      pushSetStatus('ce navigateur ne sait pas recevoir de notifications push.');
    return;
  }
  // HTTPS (ou localhost) obligatoire : sans contexte sûr, ni service worker ni
  // push. Le dire ici évite un « échec » incompréhensible au clic.
  if(!window.isSecureContext){
    cb.checked = false; cb.disabled = true;
    pushSetStatus('HTTPS requis (ou localhost) : ouvre Loki en https pour activer les notifications.');
    return;
  }
  if(Notification.permission === 'denied'){
    cb.checked = false; cb.disabled = false;
    pushSetStatus('notifications bloquées dans le navigateur — à débloquer dans ses réglages de site.');
    return;
  }
  try{
    const reg = await pushRegisterSW();
    const sub = await reg.pushManager.getSubscription();
    cb.checked = !!sub; cb.disabled = false;
    pushSetStatus(sub ? 'activées sur cet appareil.' : '');
  }catch(e){ cb.checked=false; pushSetStatus(''); }
}

// togglePush : abonne ou désabonne selon l'état de l'interrupteur.
async function togglePush(){
  const cb = document.getElementById('push-toggle');
  const want = cb.checked;
  if(!pushSupported()){ cb.checked=false; await pushRefresh(); return; }
  try{
    if(want){
      // Permission (geste utilisateur = ce clic). Refus → on éteint et on explique.
      const perm = await Notification.requestPermission();
      if(perm !== 'granted'){ cb.checked=false; pushSetStatus('permission refusée.'); return; }
      const reg = await pushRegisterSW();
      let sub = await reg.pushManager.getSubscription();
      if(!sub){
        const r = await jget('/api/push/key');
        if(!r || !r.key){ cb.checked=false; pushSetStatus('clé du serveur indisponible.'); return; }
        sub = await reg.pushManager.subscribe({
          userVisibleOnly: true,               // exigé par Chrome : pas de push silencieux
          applicationServerKey: urlB64ToUint8Array(r.key)
        });
      }
      const res = await jpost('/api/push/subscribe', sub.toJSON());
      if(!res || !res.ok){ pushSetStatus("le serveur n'a pas enregistré l'abonnement."); }
      else pushSetStatus('activées sur cet appareil.');
      toast('notifications activées');
    } else {
      const reg = await pushRegisterSW();
      const sub = await reg.pushManager.getSubscription();
      if(sub){
        const ep = sub.endpoint;
        await sub.unsubscribe().catch(()=>{});
        await jpost('/api/push/unsubscribe', {endpoint:ep}).catch(()=>{});
      }
      pushSetStatus('');
      toast('notifications désactivées');
    }
  }catch(e){
    cb.checked = !want;
    pushSetStatus('erreur : ' + (e && e.message ? e.message : e));
  }
}

// Au chargement : enregistre le SW en avance (pour recevoir les push sans même
// ouvrir les réglages) et cale l'interrupteur. Silencieux si non supporté.
if(pushSupported() && window.isSecureContext){
  navigator.serviceWorker.register('/sw.js', {scope:'/'}).then(r=>{ _swReg=r; }).catch(()=>{});
}
document.addEventListener('DOMContentLoaded', ()=>{ pushRefresh(); });
