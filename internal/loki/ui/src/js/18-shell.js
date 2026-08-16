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

// Le sélecteur affiche LE MODÈLE CHARGÉ, pas seulement le preset actif.
//
// Les deux ne coïncident pas toujours : un preset n'est « actif » que si son
// empreinte correspond exactement à la configuration courante. Une configuration
// écrite à la main, héritée d'une image, ou modifiée d'un cran depuis le dernier
// preset ne correspond à AUCUN d'entre eux — et l'en-tête n'affichait alors rien
// alors qu'un modèle était bel et bien chargé. On retombe donc sur le nom du
// fichier de la configuration active.
//
// Deux sources, deux moments : loadPresets() donne la liste, loadCfg() donne le
// modèle. Chacune dépose ce qu'elle sait et redemande le dessin — elles partent
// en parallèle (loadAll), aucun ordre n'est garanti.
let MS_PRESETS = [], MS_ACTIVE = null, MS_MODEL = '';

function renderModelSwitch(presets, active){
  MS_PRESETS = presets || []; MS_ACTIVE = active || null;
  paintModelSwitch();
}
// Appelé par loadCfg avec la valeur MODEL de la configuration active.
function setLoadedModel(name){
  MS_MODEL = (name || '').trim();
  paintModelSwitch();
}
// Nom lisible d'un fichier de modèle : sans dossier ni extension. Un .gguf
// s'appelle « Qwen3-14B-Instruct-Q4_K_M.gguf » — le chemin et le suffixe
// n'apprennent rien et mangent toute la place.
function modelLabel(path){
  const base = path.split(/[\\/]/).pop();
  return base.replace(/\.gguf$/i, '');
}
function paintModelSwitch(){
  const sel = document.getElementById('model-switch');
  if(!sel) return;
  const prev = sel.value;
  sel.textContent = '';
  // Ligne de tête : ce qui est RÉELLEMENT chargé, quand aucun preset ne le
  // revendique. Value vide → onModelSwitch l'ignore, elle ne bascule rien.
  if(!MS_ACTIVE){
    const o = document.createElement('option');
    o.value = '';
    o.textContent = MS_MODEL ? modelLabel(MS_MODEL) : (MS_PRESETS.length ? 'aucun modèle actif' : 'aucun preset');
    o.selected = true;
    sel.appendChild(o);
  }
  MS_PRESETS.forEach((p, i) => {
    const o = document.createElement('option');
    o.value = String(i + 1);
    o.textContent = p.name;
    if(MS_ACTIVE && p.id === MS_ACTIVE.id) o.selected = true;
    sel.appendChild(o);
  });
  sel.disabled = !MS_PRESETS.length;
  sel.title = MS_MODEL
    ? 'modèle chargé : ' + MS_MODEL + ' — changer de preset relance le moteur'
    : 'aucun modèle configuré';
  // Une bascule en cours ne doit pas voir sa sélection sauter sous la souris.
  if(prev && [...sel.options].some(o => o.value === prev)) sel.value = prev;
}
