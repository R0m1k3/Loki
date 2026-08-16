// ─── Coque de l'interface ────────────────────────────────────────────────────
// Les commandes de la refonte : escamoter la barre latérale, replier le
// moniteur machine, basculer le thème depuis l'en-tête, changer de modèle sans
// ouvrir les réglages.
//
// Chaque état vit sur <html> (data-side, data-perf, data-theme) et dans
// localStorage, et il est REPOSÉ AVANT LE RENDU par le script du <head> : sans
// ça, la barre latérale s'afficherait puis se refermerait sous les yeux à chaque
// chargement de page.

// Escamotage de la barre latérale (grand écran). Sur téléphone elle est déjà un
// tiroir — c'est toggleSide() qui s'en charge, et le bouton ☰ qui l'ouvre.
function toggleSideCollapse(){
  if(window.innerWidth <= 720){ toggleSide(); return; }
  const on = document.documentElement.getAttribute('data-side') !== '0';
  document.documentElement.setAttribute('data-side', on ? '0' : '1');
  try{ localStorage.setItem('loki-side', on ? '0' : '1'); }catch(_){}
  // La largeur du fil change sans qu'aucun `resize` ne soit émis : la gouttière
  // de barre de défilement mesurée dans --sbw resterait celle d'avant, et la
  // carte de saisie serait décalée du fil.
  if(typeof syncGutter === 'function') requestAnimationFrame(syncGutter);
}

// Moniteur machine replié : le pied de barre latérale ne garde que son titre.
function togglePerf(){
  const on = document.documentElement.getAttribute('data-perf') !== '0';
  document.documentElement.setAttribute('data-perf', on ? '0' : '1');
  try{ localStorage.setItem('loki-perf', on ? '0' : '1'); }catch(_){}
  const b = document.querySelector('#side-perf .perf-toggle');
  if(b){
    b.textContent = on ? '+' : '–';
    b.title = on ? 'déplier le moniteur' : 'replier le moniteur';
  }
}

// Bascule clair/sombre depuis l'en-tête. Elle passe par applyTheme + savePrefs
// (01-theme.js) pour rester alignée avec l'interrupteur des réglages et avec le
// serveur — deux chemins vers le même réglage, une seule source de vérité.
function toggleThemeQuick(){
  const dark = document.documentElement.getAttribute('data-theme') === 'dark';
  applyTheme(dark ? 'light' : 'dark');
  if(typeof savePrefs === 'function') savePrefs();
}

// Déplie un <details> ET tous ceux qui le contiennent. Depuis que les réglages
// vivent sous un repli unique, ouvrir une section par programme (journal du
// moteur, installation llama.cpp) ne suffit plus : elle s'ouvrait à l'intérieur
// d'un parent fermé, donc invisible.
function openDetails(el){
  if(typeof el === 'string') el = document.getElementById(el);
  while(el){
    if(el.tagName === 'DETAILS') el.open = true;
    el = el.parentElement;
  }
}

// Sélecteur de modèle de l'en-tête. Il liste les presets ; en choisir un revient
// à cliquer la ligne correspondante dans les réglages (switchTo demande
// confirmation puis relance le moteur).
function onModelSwitch(sel){
  const n = parseInt(sel.value, 10);
  if(!n) return;
  const name = sel.options[sel.selectedIndex].textContent;
  // La liste est repeinte par loadPresets() une fois la bascule faite (ou
  // annulée) : on ne touche pas à la sélection ici, sinon l'affichage mentirait
  // pendant les quelques secondes du redémarrage.
  switchTo(n, name);
}

// Remplit le sélecteur à partir de la liste de presets déjà chargée par
// loadPresets() — aucun appel réseau supplémentaire.
function renderModelSwitch(presets, active){
  const sel = document.getElementById('model-switch');
  if(!sel) return;
  sel.textContent = '';
  if(!presets || !presets.length){
    const o = document.createElement('option');
    o.textContent = 'aucun preset'; o.value = '';
    sel.appendChild(o); sel.disabled = true;
    return;
  }
  sel.disabled = false;
  presets.forEach((p, i) => {
    const o = document.createElement('option');
    o.value = String(i + 1);
    o.textContent = p.name;
    if(active && p.id === active.id) o.selected = true;
    sel.appendChild(o);
  });
  // Aucun preset actif (configuration modifiée à la main) : on le dit, plutôt
  // que de laisser le premier de la liste passer pour le modèle chargé.
  if(!active){
    const o = document.createElement('option');
    o.value = ''; o.textContent = 'configuration hors preset'; o.selected = true;
    sel.insertBefore(o, sel.firstChild);
  }
}

// État initial des libellés (le CSS, lui, est déjà appliqué par le <head>).
document.addEventListener('DOMContentLoaded', () => {
  if(document.documentElement.getAttribute('data-perf') === '0'){
    const b = document.querySelector('#side-perf .perf-toggle');
    if(b){ b.textContent = '+'; b.title = 'déplier le moniteur'; }
  }
});
