// ─── Intensité du raisonnement (barre de saisie) ─────────────────────────────
// Le même réglage existe dans l'éditeur de preset, mais il y est figé avec le
// preset : le changer demande d'ouvrir les réglages, éditer, enregistrer. Ici il
// s'applique au message suivant, sans redémarrer le moteur — `reasoning_effort`
// voyage dans le corps de la requête, pas dans la ligne de commande.
//
// La valeur part dans la config vivante. Appliquer un preset la réécrit : le
// réglage reste donc attaché au modèle, cette liste ne fait que le dévier à la
// volée.

const THINK_EFFORT_LABELS = {
  '':       'auto',
  none:     'aucune',
  low:      'basse',
  medium:   'moyenne',
  high:     'haute',
  xhigh:    'maximale',
};

async function loadThinkEffort(){
  try{ applyThinkEffort(await jget('/api/reasoning-effort')); }
  catch(e){ /* endpoint indisponible : la liste garde sa valeur par défaut */ }
}

function applyThinkEffort(r){
  const sel = document.getElementById('think-effort');
  if(!sel || !r || !r.ok) return;
  sel.value = r.value || '';
  // Raisonnement coupé au niveau du preset : le niveau n'a plus de prise. On
  // grise plutôt que de masquer, pour que le réglage reste trouvable.
  sel.disabled = !r.on;
  sel.title = r.on
    ? 'Intensité du raisonnement — envoyée au gabarit du modèle'
    : 'Raisonnement désactivé pour ce modèle (réglages → Raisonnement)';
}

async function setThinkEffort(value){
  const r = await jpost('/api/reasoning-effort', {value});
  if(!r || !r.ok){ toast((r && r.error) || 'réglage refusé'); return; }
  applyThinkEffort(r);
  toast('raisonnement : '+(THINK_EFFORT_LABELS[r.value||''] || r.value));
}
