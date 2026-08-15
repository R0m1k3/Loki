// Identité — prénom et avatars affichés en tête des bulles du chat.
//
// Les valeurs vivent dans les préférences SERVEUR (comme le thème) : elles
// suivent donc d'un appareil à l'autre, au lieu de rester dans le localStorage
// d'un seul navigateur. La liste d'emojis est celle que sert le serveur — pas
// une seconde copie côté client, qui divergerait à la première modification.

let IDENT = {name:'', user:'🙂', ai:'🦊'};
let AVATARS = [];

// applyIdentity redessine les libellés des bulles DÉJÀ affichées : changer son
// prénom doit se voir tout de suite, sans recharger la page.
function applyIdentity(){
  document.querySelectorAll('#chat .msg.user > .label, #chat .msg.assistant > .label')
    .forEach(l => paintLabel(l, l.parentElement.classList.contains('user') ? 'user' : 'assistant'));
}

// paintLabel remplit un libellé : avatar + nom. Le prénom vient de
// l'utilisateur, donc textContent — jamais innerHTML.
function paintLabel(label, role){
  if(!label) return;
  label.textContent = '';
  const av = document.createElement('i'); av.className = 'av';
  av.textContent = role === 'user' ? (IDENT.user || '🙂') : (IDENT.ai || '🦊');
  const who = document.createElement('span'); who.className = 'who';
  who.textContent = role === 'user' ? (IDENT.name || 'toi') : 'Loki';
  label.appendChild(av); label.appendChild(who);
}

async function loadIdentity(){
  let r;
  try{ r = await jget('/api/prefs'); }catch(_){ return; }
  const p = r.prefs || {};
  AVATARS = r.avatars || [];
  IDENT = {
    name: p.user_name || '',
    user: p.avatar_user || '🙂',
    ai:   p.avatar_ai   || '🦊',
  };
  const inp = document.getElementById('p-username');
  if(inp && document.activeElement !== inp) inp.value = IDENT.name;
  renderAvatarPicker('p-avatar-user', 'user');
  renderAvatarPicker('p-avatar-ai', 'ai');
  applyIdentity();
}

function renderAvatarPicker(elID, which){
  const box = document.getElementById(elID);
  if(!box) return;
  box.innerHTML = '';
  for(const e of AVATARS){
    const b = document.createElement('button');
    b.type = 'button'; b.textContent = e;
    if(e === IDENT[which]) b.classList.add('sel');
    b.onclick = () => pickAvatar(which, e);
    box.appendChild(b);
  }
}

async function pickAvatar(which, emoji){
  IDENT[which] = emoji;
  renderAvatarPicker(which === 'user' ? 'p-avatar-user' : 'p-avatar-ai', which);
  applyIdentity();
  try{ await jpost('/api/prefs', which === 'user' ? {avatar_user:emoji} : {avatar_ai:emoji}); }
  catch(_){ toast('erreur réseau'); }
}

// Saisie du prénom : on n'écrit pas au serveur à chaque frappe.
let identTimer = null;
function saveIdentity(){
  const inp = document.getElementById('p-username');
  if(!inp) return;
  IDENT.name = inp.value.trim();
  applyIdentity();
  clearTimeout(identTimer);
  identTimer = setTimeout(async () => {
    try{ await jpost('/api/prefs', {user_name: IDENT.name}); }catch(_){ toast('erreur réseau'); }
  }, 500);
}
