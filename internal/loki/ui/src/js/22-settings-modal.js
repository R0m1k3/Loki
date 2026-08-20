// ─── Réglages en modale ──────────────────────────────────────────────────────
// Toutes les sections de réglages vivent dans une modale à deux volets : la nav
// des sections à gauche, LE panneau choisi à droite. La barre latérale ne garde
// que les discussions et le moniteur machine — visuellement plus simple, et
// chaque section dispose enfin de toute la largeur.
//
// Les ids historiques des sections (svc-log-box, lc-details, tasks-details…)
// sont posés sur les .set-pane : openDetails() (18-shell.js) les résout vers le
// bon panneau, si bien que tous les chemins existants — pastille d'état du
// moteur, pastille d'installation, jobs llama.cpp — rouvrent la modale au bon
// endroit sans changer une ligne de leurs appelants.
let SET_LAST = 'config';
// Panneaux à contenu paresseux : chargés à l'affichage — l'équivalent de
// l'ancien ontoggle des <details>.
const SET_HOOKS = {
  'engine-log': () => { loadSvcLog(); showPaths(); },
};
function openSettings(pane){
  showModal('settings-modal');
  settingsShow(pane || SET_LAST);
}
function closeSettings(){ hideModal('settings-modal'); }
function settingsShow(pane){
  SET_LAST = pane;
  document.querySelectorAll('#set-nav [data-pane]').forEach(b => b.classList.toggle('active', b.dataset.pane === pane));
  document.querySelectorAll('#set-body .set-pane').forEach(s => s.classList.toggle('active', s.dataset.pane === pane));
  const body = document.getElementById('set-body');
  if(body) body.scrollTop = 0;
  const h = SET_HOOKS[pane]; if(h) h();
}
// Échap ferme la modale — capture:false, donc les modales à confirmation
// (ask-modal, qui écoute en capture) gardent la main quand elles sont ouvertes.
document.addEventListener('keydown', e => {
  if(e.key !== 'Escape') return;
  const m = document.getElementById('settings-modal');
  if(m && m.classList.contains('show')) closeSettings();
});
