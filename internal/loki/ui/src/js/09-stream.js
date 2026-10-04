// ===== Conversation SERVEUR (source de vérité, partagée entre appareils) =====
// L'historique et la génération vivent sur le serveur loki. Le client ouvre un
// flux d'ABONNEMENT permanent (SSE) qui rejoue le journal depuis lastSeq puis
// suit le direct. Fermer l'onglet n'arrête plus la génération (détachée côté
// serveur) ; se reconnecter rejoue tout le fil, détails compris.
let lastSeq=0, streamAbort=null;
// Historique paginé (AJEAN 0.15.7) : au chargement, le serveur ne rejoue que les
// HIST_TAIL derniers échanges et annonce le reste ({history_more: N}). Le bouton
// en haut du fil redemande alors un rejeu complet (HIST_FULL) en gardant la
// position de lecture (HIST_RESTORE = distance au bas du fil, réappliquée au
// caught_up).
const HIST_TAIL=20;
let HIST_FULL=false, HIST_RESTORE=null;
// Discussion AFFICHÉE (id reçu au caught_up/reset). Renvoyée à chaque
// (ré)abonnement : si un autre appareil a changé de discussion pendant une
// coupure, le serveur ordonne un reset au lieu de greffer le nouveau fil sur
// l'ancien (AJEAN 0.14.0).
let CONV_ID='';
// File d'attente (AJEAN 0.14.0) : messages envoyés PENDANT une réponse, montrés
// en gris au-dessus de la carte jusqu'à ce que le flux les confirme (delta
// `user` du même texte) — injectés en cours de réponse ou au tour suivant.
function queueAdd(text){
  const box=document.getElementById('queue-list'); if(!box) return null;
  const el=document.createElement('div'); el.className='queued-msg'; el.textContent=text||'(pièce jointe)';
  el.dataset.text=text;
  box.appendChild(el); box.classList.add('show');
  return el;
}
function queueSync(){ const box=document.getElementById('queue-list'); if(box) box.classList.toggle('show', !!box.children.length); }
function queueTake(text){
  const box=document.getElementById('queue-list'); if(!box) return;
  const el=[...box.children].find(e=>e.dataset.text===text);
  if(el) el.remove();
  queueSync();
}
function queueClear(){ const box=document.getElementById('queue-list'); if(box) box.textContent=''; queueSync(); }
// Identifiant d'envoi, stable d'un réessai à l'autre : le serveur ne met pas
// deux fois le même message en file si la réponse s'est perdue en route.
function newCID(){ try{ return crypto.randomUUID(); }catch(_){ return Date.now().toString(36)+Math.random().toString(36).slice(2); } }
// Bulle « en attente » : affichée EN GRIS dès l'appui sur envoyer, avant tout
// aller-retour réseau. Le message ne disparaît donc plus de l'écran entre la
// frappe et la réponse du serveur. Elle s'éclaircit (classe retirée) quand
// l'événement `user` revient par le flux — preuve que le serveur l'a bien
// enregistré. En cas d'échec d'envoi, elle est retirée et le texte est rendu.
let PENDING=null;
// Retire aussi la rangée de pièces jointes, qui vit JUSTE AVANT la bulle : sans
// ça, un envoi échoué laissait les fichiers seuls dans le fil, sans message.
function clearPending(){
  if(!PENDING) return;
  const f=PENDING.previousElementSibling;
  if(f&&f.classList.contains('msg-files')) f.remove();
  PENDING.remove(); PENDING=null;
}
function addPending(text){
  clearPending();
  PENDING=addMsg('user', text);
  PENDING.classList.add('pending');
  // L'étiquette garde l'avatar et le prénom (paintLabel, posé par addMsg) : une
  // bulle ne doit JAMAIS s'appeler autrement d'un tour à l'autre. L'envoi en
  // cours se lit à la bulle grisée (.pending) et à l'infobulle.
  const l=PENDING.querySelector('.label'); if(l) l.title='envoi en cours…';
  jumpBottom();
  return PENDING;
}
// Le serveur confirme le message : on réutilise la bulle grise au lieu d'en
// ajouter une seconde (sinon le message clignoterait en double).
function confirmPending(text){
  if(!PENDING) return false;
  const b=PENDING.querySelector('.body');
  if(!b || b.textContent!==text) return false;
  PENDING.classList.remove('pending');
  // ⚠️ NE PAS réécrire l'étiquette ici : elle porte l'avatar et le prénom
  // (17-identity.js). Elle y était remise à « user », si bien que le message
  // qu'on venait d'envoyer s'affichait « USER » alors que ceux rejoués au
  // chargement portaient le prénom — deux bulles identiques, deux libellés.
  const l=PENDING.querySelector('.label'); if(l) l.removeAttribute('title');
  PENDING=null;
  return true;
}
// REPLAYING = on est dans le replay initial (rejeu du journal au chargement).
// Pendant ce temps, les bulles raisonnement/outil sont créées DÉJÀ repliées →
// pas d'animation d'ouverture/fermeture au refresh. Le serveur envoie {caught_up}
// quand le replay est fini, on repasse alors en direct.
let REPLAYING=true;
// État de rendu du tour courant, délimité par les événements user / turn_done.
let T=null;
function newTurn(){ T={ reasonEl:null, contentEl:null, pendingToolEl:null, typingEl:null, fullContent:'', fullReason:'', turnCollapsibles:[], serverStats:null, reasonTok:0, contentTok:0, reasonFirstTs:0, reasonLastTs:0, contentFirstTs:0, contentLastTs:0, startTs:0, doneTs:0, speedText:'', model:'' }; }
newTurn();

// --- Temps de travail du tour ------------------------------------------------
// « Combien de temps l'IA a-t-elle travaillé ? » se lit sous la réponse, de la
// question envoyée à la fin du tour — raisonnement, appels d'outils et attentes
// compris. La mesure est prise sur les HORODATAGES SERVEUR (l'événement `user`
// puis `turn_done`) : elle est donc exacte en direct comme au rejeu du journal,
// où tout arrive d'un bloc côté client.
//
// Pendant le tour il n'y a pas encore de `turn_done` : le compteur avance à la
// seconde. Il ne peut pas se baser sur l'horloge du navigateur telle quelle (un
// téléphone n'est pas à la même heure que la machine), on garde donc l'ÉCART
// entre les deux, réévalué à chaque événement reçu en direct.
let TS_SKEW = 0;                       // horloge client − horloge serveur (ms)
function nowServer(){ return Date.now() - TS_SKEW; }
let WORK_TIMER = null;
function startWorkTimer(){ if(WORK_TIMER || REPLAYING) return; WORK_TIMER=setInterval(tickWork, 1000); }
function stopWorkTimer(){ if(WORK_TIMER){ clearInterval(WORK_TIMER); WORK_TIMER=null; } }
function tickWork(){ if(T.contentEl) paintStats(T.contentEl); paintTypingClock(); paintTurnClock(); }

// Chrono du TOUR COMPLET, affiché au-dessus de la carte de saisie : le temps
// entre l'envoi du message et le moment où l'on peut reparler — outils, passes
// de vérification et compaction compris. Les durées portées par les cartes ne
// disent que la génération de texte ; celle-ci répond à « ça a pris combien de
// temps, en tout ? ».
function paintTurnClock(){
  const composer = document.getElementById('composer');
  if(!composer || !T.startTs) return;
  let el = document.getElementById('turn-clock');
  if(!el){
    el = document.createElement('div');
    el.id = 'turn-clock';
    el.className = 'composer-banner';
    composer.insertBefore(el, composer.firstChild);
  }
  const ms = (T.doneTs || nowServer()) - T.startTs;
  if(!(ms > 0)){ el.remove(); return; }
  // Vitesse moyenne de la CONVERSATION à côté du temps du tour : le débit
  // typique de ce modèle sur ce fil, toutes réponses confondues.
  const avg = (CONV_MS > 1000 && CONV_TOK > 20)
    ? '  ·  moy ' + (CONV_TOK / (CONV_MS/1000)).toFixed(1) + ' tok/s' : '';
  el.textContent = (T.doneTs ? 'tour terminé en ' : 'en cours — ') + fmtDur(ms) + avg;
  el.classList.toggle('done', !!T.doneTs);
}
// --- Vitesse moyenne de la conversation --------------------------------------
// Alimentée par les événements `stats` (journalisés, donc rejoués au
// chargement : la moyenne survit au refresh). gen_tokens / gen_ms sont
// CUMULATIFS au sein d'une complétion et retombent à la suivante : on n'ajoute
// que le delta, et une valeur qui recule signale une nouvelle complétion.
let CONV_TOK = 0, CONV_MS = 0, _cgTok = 0, _cgMs = 0;
function feedConvSpeed(s){
  const gt = s.gen_tokens || 0, gms = s.gen_ms || 0;
  if(!gt) return;
  if(gt < _cgTok){ _cgTok = 0; _cgMs = 0; }
  CONV_TOK += gt - _cgTok;
  CONV_MS  += Math.max(0, gms - _cgMs);
  _cgTok = gt; _cgMs = gms;
}
function resetConvSpeed(){ CONV_TOK = 0; CONV_MS = 0; _cgTok = 0; _cgMs = 0; }
// Le compteur se montre AUSSI sur l'indicateur « … » : pendant un appel d'outil
// ou un long prefill il n'y a encore aucune réponse sous laquelle écrire, et
// c'est précisément le moment où l'on se demande si ça avance.
function paintTypingClock(){
  const el = T.typingEl;
  if(!el || el.classList.contains('compacting')) return; // le compactage a son propre libellé
  if(!T.startTs) return;
  const ms = nowServer() - T.startTs;
  if(!(ms > 1500)) return;                               // pas de compteur pour une réponse immédiate
  let c = el.querySelector('.wclock');
  if(!c){ c=document.createElement('span'); c.className='wclock'; el.appendChild(c); }
  c.textContent = fmtDur(ms);
}
// Durée lisible : dixièmes sous 10 s (un tour court se juge à la fraction),
// secondes rondes ensuite, puis minutes et heures. Le séparateur décimal reste
// le POINT — la durée voisine des « 21.5 tok/s » sur la même ligne, une virgule
// y ferait deux conventions à trois mots d'écart.
function fmtDur(ms){
  if(!(ms > 0)) return '';
  if(ms < 10000) return (ms/1000).toFixed(1) + ' s';
  const t = Math.round(ms/1000);
  if(t < 60) return t + ' s';
  const m = Math.floor(t/60), sec = t % 60;
  if(m < 60) return m + ' min ' + String(sec).padStart(2, '0') + ' s';
  return Math.floor(m/60) + ' h ' + String(m % 60).padStart(2, '0') + ' min';
}
// Libellé de durée du tour courant, vide tant qu'il n'y a rien à montrer (tour
// non commencé, ou horloges trop désaccordées pour que l'écart ait un sens).
function workLabel(){
  if(!T.startTs) return '';
  const ms = (T.doneTs || nowServer()) - T.startTs;
  if(!(ms > 300)) return '';
  return 'travail ' + fmtDur(ms);
}
// Repeint la ligne de mesures de la réponse : durée d'abord, vitesse ensuite.
// `speed` non fourni = on garde le dernier libellé de vitesse connu (c'est le
// cas du tic de seconde, qui ne fait avancer que la durée).
function paintStats(el, speed){
  if(speed !== undefined) T.speedText = speed;
  setStats(el, workLabel(), T.speedText);
}
const simpleMode=()=>document.documentElement.getAttribute('data-display')==='simple';
function removeTyping(){ if(T.typingEl){ T.typingEl.remove(); T.typingEl=null; } }
// Compactage : on ÉTIQUETTE l'indicateur de frappe déjà à l'écran au lieu
// d'ouvrir une bannière à part (on avait les deux en même temps pour un seul
// état d'attente). En fin de tour l'indicateur a déjà été retiré : on le
// recrée, en le marquant pour le reprendre quand le compactage est fini.
function setCompacting(on){
  if(on){
    if(!T.typingEl){ T.typingEl=addTyping(); T.typingEl.dataset.forCompact='1'; }
    T.typingEl.classList.add('compacting');
    if(!T.typingEl.querySelector('.tlabel')){
      const s=document.createElement('span');
      s.className='tlabel';
      s.textContent='compactage du contexte…';
      T.typingEl.appendChild(s);
    }
    scrollMaybe();
    return;
  }
  if(!T.typingEl) return;
  if(T.typingEl.dataset.forCompact){ removeTyping(); return; }
  T.typingEl.classList.remove('compacting');
  const s=T.typingEl.querySelector('.tlabel'); if(s) s.remove();
}
// Séparateur laissé dans le fil à l'endroit exact de la coupure.
function addCompactMark(){
  const el=document.createElement('div');
  el.className='compact-mark';
  el.textContent='contexte compacté — anciens tours résumés';
  // Devant l'indicateur de frappe s'il est encore là (compactage en cours de
  // tour) : la suite de la réponse doit rester APRÈS la marque de coupure.
  if(T.typingEl && T.typingEl.parentNode===chatEl()) chatEl().insertBefore(el, T.typingEl);
  else chatEl().appendChild(el);
  scrollMaybe();
}
// L'indicateur « … » est retiré dès que quelque chose de VISIBLE le remplace.
// Il doit donc survivre quand la bulle qui arrive ne sera pas affichée : mode
// simplifié, mais aussi raisonnement/outils masqués par les préférences — sinon
// le fil reste totalement vide pendant que le modèle travaille (rien à voir, et
// aucun signe que ça tourne).
const typingKept=(kind)=>simpleMode()
  || (kind==='reasoning' && viewOn('hide-reasoning'))
  || (kind==='tool' && viewOn('hide-tools'));
function killTyping(kind){ if(!T.typingEl) return; if(typingKept(kind)) return; removeTyping(); }
function showTyping(kind){ if(!typingKept(kind)) return; const c=document.getElementById('chat');
  if(!T.typingEl){ T.typingEl=addTyping(); } else if(c.lastElementChild!==T.typingEl){ c.appendChild(T.typingEl); } }
// Label de vitesse rendu depuis les valeurs (fonctionne aussi bien en direct
// qu'au replay — pas de timer performance.now, qui n'a pas de sens hors-ligne).
function renderStats(el, s){
  if(!el||!s) return;
  const parts=[];
  const pt=s.prompt_tokens||s.prompt_tokens_total;
  const pps=s.prompt_per_second ? ' · '+s.prompt_per_second.toFixed(0)+' tok/s' : '';
  // Cache connu (cache_tokens présent, même à 0) : ce qui a été recalculé et ce
  // qui venait du cache. Inconnu (moteur ancien, API tierce) : la taille seule,
  // comme avant — un « 0 en cache » inventé serait pire que rien.
  const cached=s.cache_tokens;
  if(cached!=null && (s.prompt_tokens!=null || s.prompt_tokens_total)){
    const fresh=s.prompt_tokens!=null ? s.prompt_tokens : Math.max(0,s.prompt_tokens_total-cached);
    parts.push('prefill '+nfmt(fresh)+' nouveaux / '+nfmt(cached)+' en cache'+pps);
  } else if(pt) parts.push('prefill '+pt+' tok'+pps); // débit inconnu (API tierce sans timings) : la taille seule
  if(s.gen_tokens) parts.push('decode '+s.gen_tokens+' tok · '+(s.gen_per_second||0).toFixed(1)+' tok/s');
  // Perte de cache signalée par le serveur (seuil relevé sur un modèle hybride,
  // dont les points de reprise font perdre un peu à chaque étape).
  if(s.lost_alert && s.lost) parts.push(nfmt(s.lost)+' recalculés'+(s.lost_after ? ' après '+s.lost_after.split(',').map(k=>PERF_KIND[k]||k).join(', ') : ''));
  if(!parts.length) return;
  // Réponse de l'assistant : ligne de mesures dédiée sous le texte (son étiquette
  // est masquée dans cette mise en page). Bulle repliable : l'étiquette EST le
  // bouton de repli, on y écrit comme avant.
  if(el.classList.contains('collapsible')) setLabel(el, ['reasoning'].concat(workLabel()||[], parts).join('  ·  '));
  else{
    paintStats(el, parts.join('  ·  '));
    const sl=el.querySelector(':scope > .statline');
    if(sl) sl.classList.toggle('cache-miss', !!s.lost_alert);
  }
}
// Ce qui s'est intercalé avant une perte de cache (lost_after, perf_log.go),
// dit en clair : les identifiants internes n'ont rien à faire dans l'interface.
const PERF_KIND={main:'tour', subagent:'sous-agent', verify:'vérification', task:'tâche', compact:'compaction', bench:'bench', foreign:'client /v1', prewarm:'préchauffage'};
// Milliers séparés par une espace fine insécable : « 41 230 », pas « 41230 ».
function nfmt(n){ return String(Math.round(n||0)).replace(/\B(?=(\d{3})+(?!\d))/g,'\u202f'); }
// --- Compteurs de bulle -----------------------------------------------------
// Ils sont PAR BULLE, jamais par tour. Un tour d'agent en ouvre une nouvelle
// après CHAQUE appel d'outil (le flux repasse T.contentEl à null) ; les
// compteurs, eux, n'étaient jamais remis à zéro. Chaque bulle affichait donc le
// CUMUL de tout le tour, divisé par le temps écoulé depuis le tout premier
// token — exécution des outils, pages web et prefill compris.
//
// Sur un tour agentique d'une heure, ça donnait une vitesse qui décroissait
// mécaniquement de 17 tok/s à 0,9 : le moteur n'avait pas ralenti, le compteur
// mesurait « tokens générés ÷ durée totale du tour ». La remise à zéro est faite
// à la CRÉATION de la bulle : tout chemin qui en ouvre une neuve est couvert,
// aujourd'hui comme demain.
function resetContentStats(){ T.contentTok=0; T.contentFirstTs=0; T.contentLastTs=0; T.speedText=''; }
function resetReasonStats(){ T.reasonTok=0; T.reasonFirstTs=0; T.reasonLastTs=0; }
// Label d'une bulle : nombre de tokens + vitesse. La vitesse est calculée à
// partir des HORODATAGES SERVEUR (firstTs→lastTs) : le temps réel de génération,
// donc correct aussi bien en direct qu'au replay (où les deltas arrivent d'un
// bloc côté client, mais leurs ts serveur restent espacés du vrai temps écoulé).
function labelTokens(el, role, n, firstTs, lastTs){
  if(!el) return;
  const secs=(lastTs-firstTs)/1000;
  const m = (secs>0.05 && n>1) ? n+' tok  ·  '+(n/secs).toFixed(1)+' tok/s' : n+' tok';
  // Bulle de raisonnement : sa propre durée de génération, comme le « réfléchi
  // pendant… » d'un fil de discussion — elle reste lisible une fois repliée.
  const d = (secs>0.05 && n>1) ? fmtDur(lastTs-firstTs) : '';
  // Bulle technique : son étiquette EST le bouton de repli, les mesures y vont.
  // Réponse de l'assistant : son étiquette porte l'avatar et le nom, les mesures
  // vont dans la ligne dédiée — comme le font déjà les stats de fin de tour
  // (applyStats). Sans ce partage, le libellé « Loki » était remplacé en direct
  // par « assistant · 75 tok · 21.5 tok/s », et ne revenait qu'au rechargement.
  if(el.classList.contains('collapsible')) setLabel(el, [role].concat(d||[], m).join('  ·  '));
  else paintStats(el, m);
}
// Badge du modèle sur une bulle de réponse : le nom voyage avec la borne de
// tour (T.model), on le dépose dans dataset.model pour qu'applyIdentity — qui
// repeint les libellés de zéro — puisse le reposer (voir paintLabel).
function tagModel(el){
  if(!el || !T.model) return;
  el.dataset.model = T.model;
  const l = el.querySelector('.label');
  if(l && !l.querySelector('.modelbadge') && typeof modelBadge==='function') l.appendChild(modelBadge(T.model));
}
// Pendant le replay on met à jour l'état `busy` mais on NE touche PAS aux boutons
// (sinon user→stop puis turn_done→send à chaque tour rejoué = flottement visible).
// L'état final est appliqué une seule fois au caught_up via syncSendBtn().
function setBusy(on){ busy=on; if(!REPLAYING) syncSendBtn();
  // La liste des discussions montre laquelle travaille : elle doit suivre l'état
  // MÊME pendant le rejeu (une page rechargée en cours de génération doit voir
  // l'anneau tourner sans attendre le premier événement en direct).
  if(typeof convSyncBusy==='function') convSyncBusy(); }
function syncSendBtn(){
  const sb=document.getElementById('send');
  // ⚠️ L'état passe par un ATTRIBUT, jamais par un style inline. Ces deux
  // boutons étaient montrés/cachés avec `style.display='inline-block'`, qui
  // l'emportait sur le `display:flex` de la feuille : le bouton gardait sa
  // boîte de 34 px mais son icône de 18 px se collait à gauche, décalée de 8 px.
  // C'est invisible tant que l'icône est un glyphe de texte, flagrant en SVG.
  document.documentElement.setAttribute('data-busy', busy ? '1' : '0');
  // Tant que le moteur n'a pas fini de charger le modèle, envoyer ne mène à rien :
  // on bloque le bouton et l'Entrée, et on le DIT sous le champ. `STATUS_SEEN`
  // évite de verrouiller le chat quand /api/status n'a pas encore répondu (ou ne
  // répond pas du tout) — dans le doute on laisse la main.
  const ready = !STATUS_SEEN || MODEL_READY;
  sb.disabled = !ready;
  // Moteur ARRÊTÉ (VRAM libérée à la demande, ou service coupé) : dire « le
  // modèle charge » serait faux, et on attendrait un chargement qui ne viendra
  // jamais. On nomme alors le geste qui remet le modèle en mémoire.
  // Trois raisons de ne pas être prêt, trois messages : planté au chargement
  // (recharger ne ferait que replanter — la cause est affichée dans le
  // moniteur), déchargé à la demande, ou simplement en train de charger.
  const failed = !ready && !!LOAD_ERROR;
  const unloaded = !ready && !failed && !ENGINE_ACTIVE;
  sb.title = ready ? '' : failed ? 'le modèle n\'a pas pu charger — voir l\'erreur dans le moniteur'
                        : (unloaded ? 'le modèle est déchargé — recharge-le pour écrire'
                                    : 'le modèle n\'est pas encore chargé');
  const hint=document.getElementById('sendhint');
  if(hint){
    hint.textContent = ready ? 'Entrée pour envoyer · Maj+Entrée = nouvelle ligne'
                     : failed ? 'Le modèle n\'a pas pu charger — l\'erreur est affichée dans le moniteur, en bas de la barre latérale.'
                     : unloaded ? 'Modèle déchargé — « Recharger le modèle » dans le moniteur, en bas de la barre latérale.'
                                : 'Le modèle charge — envoi possible dès qu\'il est prêt.';
    hint.classList.toggle('waiting', !ready);
  }
}
// ─── Rendu du bloc en cours : cadencé, puis lissé ────────────────────────────
// Repris de l'amont AJEAN (v0.9.5 issue #24, puis v0.10.5), adapté.
//
// Re-parser le Markdown du bloc ENTIER à chaque token est en O(n²) : sur un long
// raisonnement (des milliers de tokens) l'interface se met à ramer, les tokens
// semblent arriver au ralenti — alors que le moteur, lui, débite toujours autant
// — et un simple rafraîchissement « répare » tout, puisqu'il rend le bloc une
// seule fois. On coalesce donc : le texte s'accumule (concaténation, quasi
// gratuite) et on ne re-rend qu'à intervalle borné. Le rendu final exact est
// garanti par flushRender(), appelé à chaque frontière de bloc (outil, fin de
// tour, erreur, caught_up).
let renderTimer=null, renderPending=null, lastRenderMs=0; // {el, text}
function scheduleRender(el, text, final){
  // Changement de bloc en cours de route : on rend d'abord l'ancien à sa dernière
  // valeur, sinon son ultime bout de texte serait perdu.
  if(renderPending && renderPending.el!==el) flushRender();
  // `final` voyage AVEC le rendu en attente : le timer déjà armé (qui passe
  // final=false) peut être celui qui consommera ce rendu — sans le drapeau sur
  // le pending, le rendu de FIN de bloc repassait par le chemin « queue seule »
  // et le début du raisonnement n'était jamais posé.
  renderPending={el, text, final:!!final};
  if(renderTimer) return;
  // Cadence ADAPTATIVE : on vise à ne pas passer plus d'~1/6 du temps à re-parser
  // le Markdown. Tant que le bloc est petit, un rendu coûte 1-2 ms → plancher
  // 16 ms ≈ 60 img/s, l'apparition reste fluide token par token. Quand le bloc
  // devient énorme, le rendu coûte cher et on espace tout seul jusqu'à 500 ms :
  // le O(n²) est cassé sans jamais figer l'interface.
  const delay=Math.min(500, Math.max(16, lastRenderMs*6));
  renderTimer=setTimeout(()=>flushRender(false), delay);
}
// Au-delà de cette taille, un raisonnement EN COURS n'est re-parsé que sur sa
// fin : re-parser le bloc entier à chaque tick est en O(n²) et finissait par
// figer l'affichage (le texte semblait s'arrêter alors que le moteur débitait).
// La carte étant bornée en hauteur et collée en bas, on ne perd rien à l'écran ;
// le texte COMPLET est posé au rendu final (frontière de bloc → final=true).
const REASON_TAIL=6000;
function flushRender(final){
  if(renderTimer){ clearTimeout(renderTimer); renderTimer=null; }
  const p=renderPending; renderPending=null;
  if(!p) return;
  let text=p.text;
  if(final===false && !p.final && text.length>REASON_TAIL+800 && p.el.closest('.msg.reasoning')){
    let cut=text.length-REASON_TAIL;
    const nl=text.indexOf('\n', cut); if(nl>-1 && nl<cut+400) cut=nl+1;
    text='*… (début replié pendant la génération — le texte complet est posé à la fin)*\n\n'+text.slice(cut);
  }
  // Carte bornée (raisonnement/outil) : si on était collé en bas, on y reste —
  // la carte suit la génération. Un défilement manuel vers le haut est respecté.
  // ⚠️ p.el est la BULLE (.msg), pas .body : le bodywrap est dedans, pas au-dessus.
  const bw=p.el.querySelector ? p.el.querySelector(':scope > .bodywrap') : null;
  const scrollable=!!bw;
  const stick=scrollable && bw.scrollTop+bw.clientHeight>=bw.scrollHeight-60;
  const t0=performance.now();
  renderBody(p.el, text);
  lastRenderMs=performance.now()-t0;
  if(scrollable && stick) bw.scrollTop=bw.scrollHeight;
}
// Annule le rendu en attente SANS le poser : le bloc visé disparaît (fil vidé,
// raisonnement jeté), le rendre ensuite écrirait dans un élément détaché.
function cancelRender(){ if(renderTimer){ clearTimeout(renderTimer); renderTimer=null; } renderPending=null; }
// Lissage d'apparition (« machine à écrire »). Le décodage spéculatif (MTP) rend
// les tokens PAR RAFALES — plusieurs acceptés d'un coup, puis une pause : le
// texte grandit par paquets et saute à l'écran. On découple donc l'ARRIVÉE (les
// rafales du moteur) de l'AFFICHAGE : `target` = tout ce qui est reçu, `shown`
// avance à cadence régulière (requestAnimationFrame), et on n'affiche que le
// préfixe révélé. Le rendu passe TOUJOURS par scheduleRender pour conserver le
// garde-fou anti-O(n²) ci-dessus.
// UNIQUEMENT EN DIRECT : au rejeu tout est posé d'un bloc, sinon relire un fil
// deviendrait une lente réécriture caractère par caractère.
let smooth=null, smoothLast=0; // {el, target, shown, raf}
function smoothReset(){ if(smooth&&smooth.raf) cancelAnimationFrame(smooth.raf); smooth=null; smoothLast=0; }
// Solde le bloc courant : on affiche TOUT immédiatement. Appelé à chaque
// frontière (fin de tour, outil, changement de bloc) — sinon l'ultime bout de
// texte resterait en retard derrière le curseur de révélation.
function smoothSnap(){ if(!smooth) return; const el=smooth.el, target=smooth.target; smoothReset(); scheduleRender(el, target, true); }
// Débit BASÉ SUR LE TEMPS (indépendant du taux de rafraîchissement). La vitesse
// de révélation est proportionnelle au RETARD accumulé, avec une constante de
// temps TAU : le curseur traîne volontairement ~TAU derrière l'arrivée, ce qui
// donne un écoulement CONTINU malgré les rafales, et se vide en douceur quand la
// génération s'arrête. Un plancher garantit un progrès même sur un retard
// minuscule, sans jamais figer.
const SMOOTH_TAU=260; // ms — plus grand = plus lisse mais traîne davantage
function smoothStep(ts){
  if(!smooth) return;
  if(!smoothLast) smoothLast=ts;
  const dt=Math.min(120, ts-smoothLast); smoothLast=ts;
  const remaining=smooth.target.length - smooth.shown;
  if(remaining<=0){ smooth.raf=null; smoothLast=0; return; }
  let adv=remaining*dt/SMOOTH_TAU;      // vitesse ∝ retard
  if(adv<0.4) adv=0.4;                  // progrès minimal
  smooth.shown=Math.min(smooth.target.length, smooth.shown+Math.ceil(adv));
  scheduleRender(smooth.el, smooth.target.slice(0, smooth.shown));
  smooth.raf=requestAnimationFrame(smoothStep);
}
function smoothFeed(el, target){
  if(smooth && smooth.el!==el) smoothSnap();   // changement de bloc : solder l'ancien
  if(!smooth) smooth={el, target, shown:0, raf:null};
  smooth.target=target;
  if(!smooth.raf) smooth.raf=requestAnimationFrame(smoothStep);
}
// Rend un bloc en streaming : lissé en direct, instantané au rejeu.
function feedBlock(el, full){ if(REPLAYING) scheduleRender(el, full); else smoothFeed(el, full); }
// Solde tout ce qui est en vol : le bloc est terminé, la suite (stats, repli,
// bulle d'outil) doit voir le texte complet.
function settleBlocks(){ smoothSnap(); flushRender(); }

// Rafraîchissement groupé de la liste des discussions (plusieurs événements
// rapprochés = un seul appel).
let histRefreshTimer=null;
function histRefreshSoon(){
  if(typeof loadConversations!=='function') return;
  clearTimeout(histRefreshTimer);
  histRefreshTimer=setTimeout(()=>{ histRefreshTimer=null; loadConversations(); }, 400);
}

// Traite UN événement du flux — même sémantique que l'ancien switch inline, mais
// piloté par le serveur et rejouable à l'identique.
function handleDelta(d){
  if(typeof d.seq==='number' && d.seq>lastSeq) lastSeq=d.seq;
  // Recalage de l'horloge : un événement reçu en DIRECT vient d'être émis, son
  // `ts` serveur et l'heure locale désignent donc le même instant. Surtout pas
  // pendant le rejeu, où les `ts` sont vieux de plusieurs heures.
  if(!REPLAYING && typeof d.ts==='number' && d.ts>0) TS_SKEW = Date.now() - d.ts;
  if(d.caught_up){
    if(typeof d.id==='string') CONV_ID=d.id;
    settleBlocks(); // rendre le dernier bloc rejoué à sa valeur exacte
    // Fin du replay initial : on saute en bas puis on révèle (une seule fois — pas
    // sur les reconnexions, pour ne pas te ramener en bas si tu lisais plus haut).
    setChatLoading(null);
    if(REPLAYING && HIST_RESTORE!==null){
      // Historique complet demandé : on garde la position de lecture (distance au
      // bas du fil) au lieu de sauter en bas, et on la recale une fois la mise en
      // page stabilisée (le rejeu pose ses blocs de façon différée).
      REPLAYING=false; syncSendBtn(); const c=chatEl(); c.style.transition='opacity .15s'; c.style.opacity='1';
      const keep=HIST_RESTORE; HIST_RESTORE=null; stickyBottom=false;
      const put=()=>{ c.scrollTop=Math.max(0, c.scrollHeight-keep); };
      put(); requestAnimationFrame(()=>requestAnimationFrame(put)); setTimeout(put, 80); setTimeout(put, 250);
    } else if(REPLAYING){ REPLAYING=false; jumpBottom(); syncSendBtn(); const c=chatEl(); c.style.transition='opacity .15s'; c.style.opacity='1'; }
    // Le chrono ne démarre pas pendant le rejeu (aucun tic n'aurait de sens sur
    // des tours déjà finis) : si le dernier tour rejoué est ENCORE en cours, il
    // faut le lancer maintenant, sinon la durée resterait figée à l'écran.
    if(busy && T.startTs && !T.doneTs) startWorkTimer();
    // Fil vide : aucune bulle n'a été rejouée, donc aucune mutation ne viendra
    // déclencher la synchro — c'est ici qu'on décide d'afficher l'accueil.
    syncChatEmpty();
    return; }
  // `reset` = fil vidé OU bascule de discussion (même mécanisme d'epoch côté
  // serveur) : on nettoie l'écran et on resynchronise la liste, car la bascule
  // a pu être déclenchée depuis un autre appareil. Les fichiers suivent : ils
  // appartiennent à la discussion, le panneau doit changer avec elle.
  if(d.history_more!==undefined){ showHistoryMore(d.history_more); return; }
  if(d.queue_dropped!==undefined){ if(!REPLAYING){ queueClear(); toast('messages en attente abandonnés'); } return; }
  if(d.reset!==undefined){ if(typeof d.id==='string') CONV_ID=d.id; queueClear(); HIST_FULL=false; smoothReset(); cancelRender(); stopWorkTimer(); resetConvSpeed(); const tc=document.getElementById('turn-clock'); if(tc) tc.remove(); PENDING=null; document.getElementById('chat').innerHTML=''; newTurn(); setCtxUsed(0); lastSeq=0; setBusy(false); if(typeof loadConversations==='function') loadConversations(); if(typeof filesOnConvChange==='function') filesOnConvChange(); if(typeof modeOnConvChange==='function') modeOnConvChange(); return; }
  // --- Mode code (20-mode.js) -----------------------------------------------
  if(d.mode!==undefined){ if(typeof applyModeDelta==='function') applyModeDelta(d.mode); return; }
  if(d.code_hint){ if(typeof showCodeHint==='function') showCodeHint(); return; }
  if(d.criteria!==undefined){ if(typeof renderCriteria==='function') renderCriteria(d.criteria); return; }
  // Bascule de rôle (mode code) : les blocs en cours appartiennent au rôle
  // précédent — on les solde avant de les oublier, sinon leur dernier fragment
  // resterait en vol et ne s'afficherait jamais.
  if(d.role!==undefined){ settleBlocks(); ROLE_BADGE = d.role || ''; T.contentEl=null; T.reasonEl=null; return; }
  if(d.ask){ if(typeof renderAskCard==='function') renderAskCard(d.ask); return; }
  if(d.verify_done){ if(typeof onVerifyDone==='function') onVerifyDone(d.verify_done); return; }
  if(d.user!==undefined){
    newTurn();
    // Modèle du tour : posé par le serveur sur la borne de tour (journalisée),
    // donc présent au rejeu comme en direct. Repris par tagModel sur chaque
    // bulle de réponse du tour.
    T.model = d.model || '';
    if(!REPLAYING) queueTake(d.user); // message en file désormais pris en compte
    // Le serveur a déjà enregistré la discussion (StartTurn persiste) : la liste
    // la montre dès le premier message, sans attendre la fin de la réponse.
    if(!REPLAYING) histRefreshSoon();
    let el=PENDING;
    if(!confirmPending(d.user)) el=addMsg('user', d.user);
    // Pièces jointes du tour : rendues DANS la bulle. La bulle en attente en
    // porte déjà (posées à l'envoi), on ne les ajoute donc qu'au replay/à une
    // bulle neuve — sinon elles apparaîtraient en double.
    if(d.files && !hasMsgFiles(el)) addMsgFiles(el, d.files);
    // Départ du chronomètre : l'horodatage SERVEUR du message, pas l'heure
    // locale — c'est ce qui rend la durée juste au rejeu comme en direct.
    T.startTs = d.ts || 0; T.doneTs = 0;
    setBusy(true); startWorkTimer(); paintTurnClock(); T.typingEl=addTyping(); return; }
  // Fin de tour : la discussion vient d'être enregistrée côté serveur — son
  // titre (déduit du 1er message) et son compteur d'échanges ont changé. Pas au
  // replay, qui rejoue tous les tours passés d'un bloc.
  if(d.turn_done){ settleBlocks(); removeTyping(); collapseAll(T.turnCollapsibles); stopWorkTimer();
    // La durée se fige ICI : au-delà, plus rien ne bouge, la ligne doit montrer
    // le temps réellement passé et non continuer d'avancer.
    T.doneTs = d.ts || T.doneTs || 0;
    if(T.serverStats) renderStats(T.contentEl||T.reasonEl, T.serverStats);
    else if(T.contentEl) paintStats(T.contentEl);   // sans mesures serveur, la durée reste
    paintTurnClock();                               // fige le total au-dessus de la carte
    setBusy(false); if(!REPLAYING && typeof loadConversations==='function') loadConversations();
    if(typeof filesOnActivity==='function') filesOnActivity(); return; }
  if(d.error){ settleBlocks(); removeTyping(); T.contentEl=null; T.reasonEl=null; const eb=addMsg('assistant',''); eb.classList.add('errmsg'); renderBody(eb, d.error); return; }
  if(d.compacting!==undefined){ setCompacting(d.compacting); return; }
  if(d.compacted){ setCompacting(false); addCompactMark(); return; }
  // Pas de toast au REPLAY : le journal est rejoué à chaque chargement de page,
  // donc une notification persistée se re-déclenchait à chaque rafraîchissement
  // (« rien à compacter » qui revient sans raison). C'est un événement ponctuel,
  // il n'a de sens qu'en direct — contrairement à la marque `compacted`, qui est
  // une trace du fil et DOIT être rejouée.
  if(d.compact_noop){ setCompacting(false); if(!REPLAYING) toast('rien à compacter (contexte déjà minimal)'); return; }
  if(d.ctx_used!==undefined){ setCtxUsed(d.ctx_used); return; }
  if(d.stats){ T.serverStats=d.stats; feedConvSpeed(d.stats);
    // Conseil ponctuel (cache de prompts trop petit) : en direct seulement.
    if(d.stats.cache_hint && !REPLAYING) toast(d.stats.cache_hint);
    // prompt_tokens_total = comptage exact du moteur (include_usage). Absent sur
    // certains llama-server récents : le serveur publie alors un `ctx_used`
    // estimé en fin de tour, traité plus bas — on ne remet donc PAS la jauge à
    // zéro ici, on la laisse simplement inchangée. Même calcul que le serveur
    // (StatsEvent.ctxAfter) : le raisonnement, jamais renvoyé au modèle, ne
    // compte pas. ctx_isolated = passe du vérificateur, sur sa propre trace :
    // ses chiffres ne disent rien de la discussion, la jauge ne bouge pas.
    if(d.stats.prompt_tokens_total && !d.ctx_isolated){ const g=d.stats.gen_tokens||0; setCtxUsed(d.stats.prompt_tokens_total+g-Math.min(g, d.stats.reasoning_tokens||0)); }
    if(T.contentEl||T.reasonEl) renderStats(T.contentEl||T.reasonEl, d.stats); return; }
  if(d.tool_used){
    settleBlocks(); // le bloc texte précédent (raisonnement/contenu) est terminé
    killTyping('tool'); T.contentEl=null; T.reasonEl=null; const tu=d.tool_used;
    // Une carte reste OUVERTE tant que SA section travaille (hauteur bornée à
    // 280 px, défilement qui suit l'écriture), et se replie DÈS qu'elle est
    // finie : l'outil à son résultat (tu.done, plus bas), le raisonnement quand
    // le bloc suivant démarre (collapseAll à la création de la carte suivante).
    if(!T.pendingToolEl){ collapseAll(T.turnCollapsibles); T.pendingToolEl=addMsg('tool',''); if(REPLAYING||viewOn('fold-tools')) collapseInstant(T.pendingToolEl); T.turnCollapsibles.push(T.pendingToolEl); }
    renderToolMsg(T.pendingToolEl, tu);
    // Outils masqués : on garde l'indicateur même quand l'appel est terminé (le
    // tour continue, et rien d'autre n'est visible). Sinon, comportement inchangé.
    if(!tu.done || viewOn('hide-tools')) showTyping('tool');
    // Résultat arrivé : la carte est finie, elle se replie tout de suite (en
    // direct seulement — au rejeu elle est déjà née repliée). SAUF une capture
    // d'écran : l'image EST le résultat, la replier la cacherait sitôt prise.
    if(tu.done){
      if(!REPLAYING && T.pendingToolEl && !tu.image) collapseBody(T.pendingToolEl);
      // Carte à image : retirée de la liste du tour, sinon le repli de fin de
      // tour (collapseAll) la fermerait quand même.
      if(tu.image && T.pendingToolEl){ const i=T.turnCollapsibles.indexOf(T.pendingToolEl); if(i>=0) T.turnCollapsibles.splice(i,1); }
      T.pendingToolEl=null; if(tu.name==='mem_add'||tu.name==='mem_edit') loadMem();
      if(typeof filesOnActivity==='function') filesOnActivity(); }
    return; }
  if(d.drop_reasoning){
    smoothReset(); cancelRender(); // le bloc raisonnement disparaît : rien à rendre
    if(T.reasonEl){ const i=T.turnCollapsibles.indexOf(T.reasonEl); if(i>=0) T.turnCollapsibles.splice(i,1); T.reasonEl.remove(); T.reasonEl=null; T.fullReason=''; }
    return; }
  if(d.reasoning_content){
    killTyping('reasoning');
    // Raisonnement qui reprend APRÈS du texte de réponse (modèle qui repense en
    // cours de réponse, reprise après coupure) : il allait dans l'ANCIENNE bulle,
    // déjà repliée au-dessus de la réponse. On en ouvre une nouvelle sous la
    // réponse : l'ordre affiché suit l'ordre reçu (AJEAN 0.17.4).
    // Réponse encore vide (simple saut de ligne avant la réflexion) : on retire
    // la bulle vide et le raisonnement continue dans la sienne.
    if(T.contentEl){
      if((T.fullContent||'').trim()){ settleBlocks(); T.reasonEl=null; }
      else { smoothReset(); cancelRender(); T.contentEl.remove(); }
      T.contentEl=null;
    }
    if(!T.reasonEl){ collapseAll(T.turnCollapsibles); T.reasonEl=addMsg('reasoning',''); if(REPLAYING||viewOn('fold-tools')) collapseInstant(T.reasonEl); T.fullReason=''; resetReasonStats(); T.turnCollapsibles.push(T.reasonEl); }
    // d.replace : le serveur renvoie le bloc ENTIER alors qu'on en affichait déjà
    // le début (voir decorateEvent/coalesceReplay côté serveur) → on repart de zéro
    // au lieu de concaténer, sinon le texte apparaît en double.
    if(d.replace){ smoothSnap(); T.fullReason=''; resetReasonStats(); }
    showTyping('reasoning'); T.fullReason+=d.reasoning_content; feedBlock(T.reasonEl, T.fullReason);
    // d.toks/d.ts0 présents quand l'événement est coalescé (replay) : plusieurs
    // tokens d'un coup. Sinon (direct), 1 token, ts0=ts.
    if(!T.reasonFirstTs) T.reasonFirstTs=d.ts0||d.ts||0; T.reasonLastTs=d.ts||T.reasonLastTs; T.reasonTok+=(d.toks||1);
    labelTokens(T.reasonEl, 'reasoning', T.reasonTok, T.reasonFirstTs, T.reasonLastTs);
    return; }
  if(d.content){
    removeTyping();
    if(!T.contentEl){ collapseAll(T.turnCollapsibles); T.contentEl=addMsg('assistant',''); tagModel(T.contentEl); T.fullContent=''; resetContentStats(); }
    if(d.replace){ smoothSnap(); T.fullContent=''; resetContentStats(); }
    T.fullContent+=d.content; feedBlock(T.contentEl, T.fullContent);
    if(!T.contentFirstTs) T.contentFirstTs=d.ts0||d.ts||0; T.contentLastTs=d.ts||T.contentLastTs; T.contentTok+=(d.toks||1);
    labelTokens(T.contentEl, 'assistant', T.contentTok, T.contentFirstTs, T.contentLastTs);
    return; }
}
// Flux d'abonnement permanent + reconnexion auto (from=lastSeq → pas de
// re-téléchargement complet après une coupure / bascule d'appareil).
// Un onglet caché RELÂCHE son flux SSE. Sans ça, chaque onglet Loki laissé
// ouvert monopolise une des ~6 connexions simultanées autorisées par domaine :
// au-delà, toute requête (journal, installation du moteur…) reste en file
// d'attente sans jamais partir ni échouer — un blocage silencieux très
// déroutant. Au retour de l'onglet on se reconnecte, et le replay depuis
// lastSeq rattrape tout ce qui s'est passé entre-temps.
let streamPaused=false;
document.addEventListener('visibilitychange', ()=>{
  if(document.hidden){
    streamPaused=true;
    if(streamAbort) try{ streamAbort.abort(); }catch(e){}
  } else if(streamPaused){
    streamPaused=false;
  }
});
async function connectStream(){
  while(true){
    // Onglet en arrière-plan : on n'ouvre aucune connexion, on attend le retour.
    while(document.hidden){ await new Promise(res=>setTimeout(res, 500)); }
    streamAbort=new AbortController();
    try{
      const r=await jfetch('/api/chat',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({from:lastSeq,tail:HIST_FULL?0:HIST_TAIL,conv_id:CONV_ID}),signal:streamAbort.signal});
      if(REPLAYING) setChatLoading('chargement de la conversation…');
      const reader=r.body.getReader(); const dec=new TextDecoder(); let buf='';
      while(true){
        const {done,value}=await reader.read(); if(done) break;
        buf+=dec.decode(value,{stream:true}); let i;
        while((i=buf.indexOf('\n\n'))>=0){
          const chunk=buf.slice(0,i); buf=buf.slice(i+2);
          for(const line of chunk.split('\n')){
            if(!line.startsWith('data:')) continue;
            const data=line.slice(5).trim(); if(data===''||data==='[DONE]') continue;
            try{ const o=JSON.parse(data); const d=(o.choices&&o.choices[0]&&o.choices[0].delta)||{}; handleDelta(d); }catch(e){}
          }
        }
      }
    }catch(e){ /* coupure : on reconnecte silencieusement */ }
    // Le flux s'est arrêté (coupure ou fin prématurée). Si le fil n'a JAMAIS fini
    // de charger, le silence est trompeur — un chat vide sans explication. On le
    // dit dans le voile ; il disparaîtra au {caught_up} de la reconnexion.
    if(REPLAYING) setChatLoading('connexion au serveur…');
    await new Promise(res=>setTimeout(res, 600));
  }
}
// Interrompt la génération en cours côté serveur (la goroutine détachée est
// annulée). Le serveur émet alors turn_done → le bouton repasse en « send ».
// _stopAt : anti-doublon, le bouton réagit à l'APPUI sur écran tactile (voir plus
// bas) puis le clic qui suit éventuellement ne doit pas relancer.
let _stopAt = 0;
function stopGen(){
  const now = Date.now(); if(now - _stopAt < 800) return; _stopAt = now;
  jfetch('/api/chat/stop',{method:'POST'}).catch(()=>{}); toast('stop');
}
// Sur écran tactile, stop agit dès l'appui (pointerdown) : le « clic » complet
// peut être annulé par iOS si le fil défile au même moment — or c'est justement
// quand l'IA écrit qu'on veut l'arrêter vite. Repris d'AJEAN 0.15.7.
document.addEventListener('DOMContentLoaded', ()=>{
  const st=document.getElementById('stop'); if(!st) return;
  st.addEventListener('pointerdown', (e)=>{ if(e.pointerType!=='mouse'){ e.preventDefault(); stopGen(); } });
});
// Bouton « échanges précédents » posé en tête du fil quand le serveur n'a rejoué
// que la fin de la conversation.
function showHistoryMore(n){
  const chat=chatEl(); if(!chat || !n) return;
  let box=chat.querySelector('.history-more');
  if(!box){ box=document.createElement('div'); box.className='history-more'; chat.insertBefore(box, chat.firstChild); }
  box.innerHTML='';
  const b=document.createElement('button'); b.type='button';
  b.textContent = n===1 ? 'afficher l\'échange précédent' : 'afficher les '+n+' échanges précédents';
  b.onclick=loadFullHistory;
  box.appendChild(b);
}
// Rejeu COMPLET de la conversation vive, en gardant ce qu'on lisait à l'écran :
// on vide le fil, on coupe le flux, et la boucle connectStream se reconnecte
// depuis le début sans pagination (tail=0).
function loadFullHistory(){
  const chat=chatEl(); if(!chat) return;
  const box=chat.querySelector('.history-more'); if(box){ const b=box.querySelector('button'); if(b){ b.disabled=true; b.textContent='chargement…'; } }
  HIST_RESTORE=chat.scrollHeight-chat.scrollTop;
  HIST_FULL=true;
  chat.innerHTML=''; newTurn();
  smoothReset(); cancelRender();
  lastSeq=0; REPLAYING=true;
  if(streamAbort){ try{ streamAbort.abort(); }catch(e){} }
}
// Envoi RÉSILIENT : sur le tunnel E2E, un aller-retour peut échouer transitoirement
// alors qu'il a en fait abouti (la génération démarre). On réessaie, et un 409
// (« déjà en cours ») = succès (c'est notre envoi qui est passé). On ne montre une
// erreur qu'après plusieurs échecs ET vérification que rien ne tourne — plus de
// « network error » alarmiste alors que l'IA répond quand même.
// Fuseau du navigateur, envoyé avec chaque message : le conteneur tourne en UTC,
// et « rappelle-moi à 17h » doit tomber à 17h ici (voir rememberUserTZ).
const USER_TZ=(()=>{ try{ return Intl.DateTimeFormat().resolvedOptions().timeZone||''; }catch(_){ return ''; } })();
async function send(){
  // Garde-fou : le bouton est déjà désactivé, mais l'Entrée passe aussi par ici.
  if(STATUS_SEEN && !MODEL_READY){ toast('le modèle n\'est pas encore prêt'); return; }
  const ta=document.getElementById('input'); const text=ta.value.trim();
  // Un envoi sans texte est légitime s'il porte une pièce jointe (« tiens, regarde »).
  if(!text && !ATTACH.length) return;
  ta.value=''; autoGrow(ta);
  // Réponse en cours : le message part EN FILE (AJEAN 0.14.0) — il sera pris en
  // compte à la prochaine étape de la réponse, ou au tour suivant. Il s'affiche
  // en attente au-dessus de la carte, pas dans le fil qui s'écrit encore.
  const queuedAtSend = busy;
  let qel=null;
  // Sinon le message s'affiche TOUT DE SUITE dans le fil, en gris : il ne
  // disparaît plus le temps de l'aller-retour. Il s'éclaircit quand le flux le
  // confirme (confirmPending).
  if(queuedAtSend) qel=queueAdd(text); else addPending(text);
  const fail=(m)=>{ if(qel){ qel.remove(); queueSync(); } else clearPending(); toast(m); ta.value=text; autoGrow(ta); };
  const cid=newCID();
  // C'est ici que les fichiers partent vers le serveur — pas avant. Les pastilles
  // ne sont retirées qu'une fois le message accepté : tant qu'il n'est pas parti,
  // on doit pouvoir en enlever une, et un échec doit rester visible.
  const files=await attachPaths();
  if(!text && !files.length){ fail('aucun fichier n\'a pu être déposé'); return; }
  // Les pastilles passent dans la bulle en attente : le message porte ses
  // fichiers dès l'envoi, sans attendre l'aller-retour.
  if(PENDING && !queuedAtSend) addMsgFiles(PENDING, attachSent());
  for(let attempt=0; attempt<3; attempt++){
    try{
      const r=await jfetch('/api/chat/send',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({message:text,files:files,ctx_used:CTX_USED,cid,tz:USER_TZ})});
      if(r.ok) clearAttach();
      // 409 = une tâche planifiée occupe le modèle : pas de file dans ce cas.
      if(r.status===409){ let m='modèle occupé'; try{ m=(await r.json()).error||m; }catch(_){} fail(m); return; }
      if(r.ok){
        // Parti tout de suite alors qu'on le croyait en file (le tour venait de
        // finir) : la pastille disparaît, la bulle arrive par le flux.
        let j={}; try{ j=await r.json(); }catch(_){}
        if(qel && j.queued===false){ qel.remove(); queueSync(); }
        return;                                 // la bulle + les tokens arrivent par le flux
      }
      if(r.status<500){ let m='erreur'; try{ m=(await r.json()).error||m; }catch(_){} fail(m); return; }
    }catch(e){ /* réseau : on retente */ }
    await new Promise(res=>setTimeout(res, 600));
  }
  // Après plusieurs échecs : le serveur a peut-être quand même reçu le message.
  try{ const s=await (await jfetch('/api/chat/state')).json(); if(s.generating) return; }catch(_){}
  fail('échec de l\'envoi — réessaie');
}
loadAll();
// Jauges et pastille d'état : rien à rafraîchir dans un onglet que personne ne
// regarde. Chaque tic lançait sinon un nvidia-smi côté serveur, pour chaque
// onglet ou téléphone laissé ouvert, pendant que le modèle génère. Au retour de
// l'onglet, une lecture immédiate remet tout à jour.
const pollVisible=(fn)=>()=>{ if(!document.hidden) fn(); };
setInterval(pollVisible(loadStatus), 5000);
setInterval(pollVisible(loadVram), 3000);
setInterval(pollVisible(loadRam), 3000);
document.addEventListener('visibilitychange', ()=>{
  if(!document.hidden){ loadStatus(); loadVram(); loadRam(); }
});
