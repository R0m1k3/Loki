// ===== Loupe d'image =======================================================
// Les images du fil sont PLAFONNÉES (CSS .chatimg / .msg-img) : sans plafond,
// une capture sortait à sa taille native et mangeait tout l'écran, ce qui rendait
// la conversation illisible. Le plafond ne se paie donc pas en information : un
// clic rouvre l'image ici, aussi grande que la fenêtre le permet.
//
// On réutilise la source DÉJÀ chargée par la vignette (blob: hydraté par
// hydrateImages, ou objectURL local d'une pièce jointe en attente) : rien n'est
// re-téléchargé, et ça marche derrière la clé de pilotage — qu'une balise <img>
// ne sait pas porter, d'où le blob: en premier lieu.
function lightboxEl(){
  let el=document.getElementById('lightbox');
  if(el) return el;
  el=document.createElement('div');
  el.id='lightbox';
  el.innerHTML='<button type="button" class="lb-x" title="fermer" aria-label="fermer">×</button>'
    +'<img alt=""><div class="lb-name"></div>';
  // Clic sur le FOND (ou sur le ×) = fermer ; clic sur l'image = ne rien faire,
  // sinon on referme dès qu'on veut la regarder de près.
  el.onclick=e=>{ if(e.target===el || e.target.classList.contains('lb-x')) closeLightbox(); };
  document.body.appendChild(el);
  return el;
}
// Capture (3e argument à true) : sans elle, Échap serait d'abord vu par les
// écouteurs des modales déjà en place et la loupe ne se fermerait pas.
function _lbKey(e){
  if(e.key==='Escape'){ e.preventDefault(); e.stopPropagation(); closeLightbox(); }
}
function openLightbox(src, name){
  if(!src) return;
  const el=lightboxEl(), img=el.querySelector('img');
  img.src=src;
  img.alt=name||'';
  el.querySelector('.lb-name').textContent=name||'';
  showModal(el);
  document.addEventListener('keydown', _lbKey, true);
}
function closeLightbox(){
  const el=document.getElementById('lightbox'); if(!el) return;
  document.removeEventListener('keydown', _lbKey, true);
  hideModal(el);
  // La source n'est lâchée qu'APRÈS la transition : la vider tout de suite
  // laissait un cadre vide clignoter pendant la fermeture.
  setTimeout(()=>{
    if(!el.classList.contains('show')) el.querySelector('img').removeAttribute('src');
  }, 200);
}
// Rend une image cliquable (idempotent : renderBody reconstruit le DOM à chaque
// delta du streaming, et hydrateImages nous rappelle sur les mêmes éléments).
//
// On lit currentSrc/src AU CLIC et non à la liaison : l'hydratation remplace la
// source par un blob: après coup, et figer l'URL /api/chat/image ici rouvrirait
// une image que le navigateur n'a pas le droit de charger seul.
function bindZoom(img, name){
  if(!img || img.dataset.zoom) return;
  img.dataset.zoom='1';
  img.addEventListener('click', ()=>openLightbox(img.currentSrc||img.src, name||img.alt||''));
}
