// ===== Conversation SERVEUR (source de vérité, partagée entre appareils) =====
// L'historique et la génération vivent sur le serveur loki. Le client ouvre un
// flux d'ABONNEMENT permanent (SSE) qui rejoue le journal depuis lastSeq puis
// suit le direct. Fermer l'onglet n'arrête plus la génération (détachée côté
// serveur) ; se reconnecter rejoue tout le fil, détails compris.
let lastSeq=0, streamAbort=null;
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
function newTurn(){ T={ reasonEl:null, contentEl:null, pendingToolEl:null, typingEl:null, fullContent:'', fullReason:'', turnCollapsibles:[], serverStats:null, reasonTok:0, contentTok:0, reasonFirstTs:0, reasonLastTs:0, contentFirstTs:0, contentLastTs:0, startTs:0, doneTs:0, speedText:'' }; }
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
  el.textContent = (T.doneTs ? 'tour terminé en ' : 'en cours — ') + fmtDur(ms);
  el.classList.toggle('done', !!T.doneTs);
}
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
  if(pt) parts.push('prefill '+pt+' tok · '+(s.prompt_per_second||0).toFixed(0)+' tok/s');
  if(s.gen_tokens) parts.push('decode '+s.gen_tokens+' tok · '+(s.gen_per_second||0).toFixed(1)+' tok/s');
  if(!parts.length) return;
  // Réponse de l'assistant : ligne de mesures dédiée sous le texte (son étiquette
  // est masquée dans cette mise en page). Bulle repliable : l'étiquette EST le
  // bouton de repli, on y écrit comme avant.
  if(el.classList.contains('collapsible')) setLabel(el, ['reasoning'].concat(workLabel()||[], parts).join('  ·  '));
  else paintStats(el, parts.join('  ·  '));
}
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
  sb.title = ready ? '' : 'le modèle n\'est pas encore chargé';
  const hint=document.getElementById('sendhint');
  if(hint){
    hint.textContent = ready ? 'Entrée pour envoyer · Maj+Entrée = nouvelle ligne'
                             : 'Le modèle charge — envoi possible dès qu\'il est prêt.';
    hint.classList.toggle('waiting', !ready);
  }
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
    // Fin du replay initial : on saute en bas puis on révèle (une seule fois — pas
    // sur les reconnexions, pour ne pas te ramener en bas si tu lisais plus haut).
    setChatLoading(null);
    if(REPLAYING){ REPLAYING=false; jumpBottom(); syncSendBtn(); const c=chatEl(); c.style.transition='opacity .15s'; c.style.opacity='1'; }
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
  if(d.reset!==undefined){ stopWorkTimer(); const tc=document.getElementById('turn-clock'); if(tc) tc.remove(); PENDING=null; document.getElementById('chat').innerHTML=''; newTurn(); setCtxUsed(0); lastSeq=0; setBusy(false); if(typeof loadConversations==='function') loadConversations(); if(typeof filesOnConvChange==='function') filesOnConvChange(); if(typeof modeOnConvChange==='function') modeOnConvChange(); return; }
  // --- Mode code (20-mode.js) -----------------------------------------------
  if(d.mode!==undefined){ if(typeof applyModeDelta==='function') applyModeDelta(d.mode); return; }
  if(d.code_hint){ if(typeof showCodeHint==='function') showCodeHint(); return; }
  if(d.criteria!==undefined){ if(typeof renderCriteria==='function') renderCriteria(d.criteria); return; }
  if(d.role!==undefined){ ROLE_BADGE = d.role || ''; T.contentEl=null; T.reasonEl=null; return; }
  if(d.ask){ if(typeof renderAskCard==='function') renderAskCard(d.ask); return; }
  if(d.verify_done){ if(typeof onVerifyDone==='function') onVerifyDone(d.verify_done); return; }
  if(d.user!==undefined){
    newTurn();
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
  if(d.turn_done){ removeTyping(); collapseAll(T.turnCollapsibles); stopWorkTimer();
    // La durée se fige ICI : au-delà, plus rien ne bouge, la ligne doit montrer
    // le temps réellement passé et non continuer d'avancer.
    T.doneTs = d.ts || T.doneTs || 0;
    if(T.serverStats) renderStats(T.contentEl||T.reasonEl, T.serverStats);
    else if(T.contentEl) paintStats(T.contentEl);   // sans mesures serveur, la durée reste
    paintTurnClock();                               // fige le total au-dessus de la carte
    setBusy(false); if(!REPLAYING && typeof loadConversations==='function') loadConversations(); return; }
  if(d.error){ removeTyping(); T.contentEl=null; T.reasonEl=null; const eb=addMsg('assistant',''); eb.classList.add('errmsg'); renderBody(eb, d.error); return; }
  if(d.compacting!==undefined){ setCompacting(d.compacting); return; }
  if(d.compacted){ setCompacting(false); addCompactMark(); return; }
  // Pas de toast au REPLAY : le journal est rejoué à chaque chargement de page,
  // donc une notification persistée se re-déclenchait à chaque rafraîchissement
  // (« rien à compacter » qui revient sans raison). C'est un événement ponctuel,
  // il n'a de sens qu'en direct — contrairement à la marque `compacted`, qui est
  // une trace du fil et DOIT être rejouée.
  if(d.compact_noop){ setCompacting(false); if(!REPLAYING) toast('rien à compacter (contexte déjà minimal)'); return; }
  if(d.ctx_used!==undefined){ setCtxUsed(d.ctx_used); return; }
  if(d.stats){ T.serverStats=d.stats;
    if(d.stats.prompt_tokens_total){ setCtxUsed((d.stats.prompt_tokens_total||0)+(d.stats.gen_tokens||0)); }
    if(T.contentEl||T.reasonEl) renderStats(T.contentEl||T.reasonEl, d.stats); return; }
  if(d.tool_used){
    killTyping('tool'); T.contentEl=null; T.reasonEl=null; const tu=d.tool_used;
    if(!T.pendingToolEl){ collapseAll(T.turnCollapsibles); T.pendingToolEl=addMsg('tool',''); if(REPLAYING||viewOn('fold-tools')) collapseInstant(T.pendingToolEl); T.turnCollapsibles.push(T.pendingToolEl); }
    renderToolMsg(T.pendingToolEl, tu);
    // Outils masqués : on garde l'indicateur même quand l'appel est terminé (le
    // tour continue, et rien d'autre n'est visible). Sinon, comportement inchangé.
    if(!tu.done || viewOn('hide-tools')) showTyping('tool');
    if(tu.done){ T.pendingToolEl=null; if(tu.name==='mem_add'||tu.name==='mem_edit') loadMem(); }
    return; }
  if(d.drop_reasoning){
    if(T.reasonEl){ const i=T.turnCollapsibles.indexOf(T.reasonEl); if(i>=0) T.turnCollapsibles.splice(i,1); T.reasonEl.remove(); T.reasonEl=null; T.fullReason=''; }
    return; }
  if(d.reasoning_content){
    killTyping('reasoning');
    if(!T.reasonEl){ collapseAll(T.turnCollapsibles); T.reasonEl=addMsg('reasoning',''); if(REPLAYING||viewOn('fold-tools')) collapseInstant(T.reasonEl); T.fullReason=''; resetReasonStats(); T.turnCollapsibles.push(T.reasonEl); }
    // d.replace : le serveur renvoie le bloc ENTIER alors qu'on en affichait déjà
    // le début (voir decorateEvent/coalesceReplay côté serveur) → on repart de zéro
    // au lieu de concaténer, sinon le texte apparaît en double.
    if(d.replace){ T.fullReason=''; resetReasonStats(); }
    showTyping('reasoning'); T.fullReason+=d.reasoning_content; renderBody(T.reasonEl, T.fullReason);
    // d.toks/d.ts0 présents quand l'événement est coalescé (replay) : plusieurs
    // tokens d'un coup. Sinon (direct), 1 token, ts0=ts.
    if(!T.reasonFirstTs) T.reasonFirstTs=d.ts0||d.ts||0; T.reasonLastTs=d.ts||T.reasonLastTs; T.reasonTok+=(d.toks||1);
    labelTokens(T.reasonEl, 'reasoning', T.reasonTok, T.reasonFirstTs, T.reasonLastTs);
    return; }
  if(d.content){
    removeTyping();
    if(!T.contentEl){ collapseAll(T.turnCollapsibles); T.contentEl=addMsg('assistant',''); T.fullContent=''; resetContentStats(); }
    if(d.replace){ T.fullContent=''; resetContentStats(); }
    T.fullContent+=d.content; renderBody(T.contentEl, T.fullContent);
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
      const r=await jfetch('/api/chat',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({from:lastSeq}),signal:streamAbort.signal});
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
function stopGen(){ jfetch('/api/chat/stop',{method:'POST'}).catch(()=>{}); toast('stop'); }
// Envoi RÉSILIENT : sur le tunnel E2E, un aller-retour peut échouer transitoirement
// alors qu'il a en fait abouti (la génération démarre). On réessaie, et un 409
// (« déjà en cours ») = succès (c'est notre envoi qui est passé). On ne montre une
// erreur qu'après plusieurs échecs ET vérification que rien ne tourne — plus de
// « network error » alarmiste alors que l'IA répond quand même.
async function send(){
  if(busy) return;
  // Garde-fou : le bouton est déjà désactivé, mais l'Entrée passe aussi par ici.
  if(STATUS_SEEN && !MODEL_READY){ toast('le modèle n\'est pas encore prêt'); return; }
  const ta=document.getElementById('input'); const text=ta.value.trim();
  // Un envoi sans texte est légitime s'il porte une pièce jointe (« tiens, regarde »).
  if(!text && !ATTACH.length) return;
  ta.value=''; autoGrow(ta);
  // Le message s'affiche TOUT DE SUITE, en gris : il ne disparaît plus le temps
  // de l'aller-retour. Il s'éclaircit quand le flux le confirme (confirmPending).
  addPending(text);
  const fail=(m)=>{ clearPending(); toast(m); ta.value=text; autoGrow(ta); };
  // C'est ici que les fichiers partent vers le serveur — pas avant. Les pastilles
  // ne sont retirées qu'une fois le message accepté : tant qu'il n'est pas parti,
  // on doit pouvoir en enlever une, et un échec doit rester visible.
  const files=await attachPaths();
  if(!text && !files.length){ fail('aucun fichier n\'a pu être déposé'); return; }
  // Les pastilles passent dans la bulle en attente : le message porte ses
  // fichiers dès l'envoi, sans attendre l'aller-retour.
  if(PENDING) addMsgFiles(PENDING, attachSent());
  for(let attempt=0; attempt<3; attempt++){
    try{
      const r=await jfetch('/api/chat/send',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({message:text,files:files,ctx_used:CTX_USED})});
      if(r.status===409 || r.ok) clearAttach();
      if(r.status===409) return;               // déjà en cours (notre envoi a abouti) → OK
      if(r.ok) return;                          // la bulle + les tokens arrivent par le flux
      if(r.status<500){ let m='erreur'; try{ m=(await r.json()).error||m; }catch(_){} fail(m); return; }
    }catch(e){ /* réseau : on retente */ }
    await new Promise(res=>setTimeout(res, 600));
  }
  // Après plusieurs échecs : le serveur a peut-être quand même reçu le message.
  try{ const s=await (await jfetch('/api/chat/state')).json(); if(s.generating) return; }catch(_){}
  fail('échec de l\'envoi — réessaie');
}
loadAll();
setInterval(loadStatus, 5000);
setInterval(loadVram, 3000);
setInterval(loadRam, 3000);
