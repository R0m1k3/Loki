// ─── Jeu d'icônes ────────────────────────────────────────────────────────────
// Un seul trait pour toute l'interface : 24 unités de côté, 1,8 d'épaisseur,
// extrémités et jonctions arrondies — celui des deux icônes de la maquette
// (dépôt et envoi), étendu au reste de l'application.
//
// Elles étaient jusqu'ici des GLYPHES DE TEXTE (↻ × ☰ 🗑 ✎ ↓ ▸). C'est le
// système d'exploitation qui les dessinait : le même bouton sortait fin sur
// macOS, gras et bleuté sur Windows (émojis en couleur), et carrément absent sur
// un serveur sans police d'émojis. Aucune cohérence possible, et rien à régler
// depuis la feuille de style. En SVG, elles suivent currentColor et la taille
// qu'on leur donne.
//
// Ajouter une icône : une entrée ici, et rien d'autre — surtout pas un second
// dessin ailleurs, c'est ainsi que le trait finit par diverger.
const ICONS = {
  menu:      '<path d="M4 7h16"/><path d="M4 12h16"/><path d="M4 17h16"/>',
  close:     '<path d="M6.5 6.5l11 11"/><path d="M17.5 6.5l-11 11"/>',
  refresh:   '<path d="M20.5 12a8.5 8.5 0 1 1-2.5-6"/><path d="M20.5 3.5V9H15"/>',
  pencil:    '<path d="M4 20h4L19 9a2.1 2.1 0 0 0-3-3L5 17z"/><path d="M14.5 7.5l3 3"/>',
  trash:     '<path d="M4 7h16"/><path d="M9.5 7V4.5h5V7"/><path d="M6.5 7l1 12.5h9l1-12.5"/>',
  download:  '<path d="M12 4v11"/><path d="M7.5 10.5 12 15l4.5-4.5"/><path d="M4.5 19.5h15"/>',
  arrowDown: '<path d="M12 5v14"/><path d="M6 13l6 6 6-6"/>',
  folder:    '<path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/>',
  file:      '<path d="M13.5 3.5H7A1.5 1.5 0 0 0 5.5 5v14A1.5 1.5 0 0 0 7 20.5h10a1.5 1.5 0 0 0 1.5-1.5V8.5z"/><path d="M13.5 3.5V8.5h5"/>',
  image:     '<rect x="3.5" y="5" width="17" height="14" rx="2"/><circle cx="8.5" cy="10" r="1.4"/><path d="M4.5 17.5 10 12l3.5 3.5L16 13l3.5 3.5"/>',
  plus:      '<path d="M12 5.5v13"/><path d="M5.5 12h13"/>',
  chevron:   '<path d="M9.5 5.5 16 12l-6.5 6.5"/>',
  search:    '<circle cx="11" cy="11" r="6.5"/><path d="M20 20l-4.4-4.4"/>',
  // Anneau ouvert : c'est la ROTATION (CSS) qui en fait un indicateur d'activité,
  // le dessin reste au même trait que le reste du jeu.
  spinner:   '<path d="M12 3.5a8.5 8.5 0 1 1-8.5 8.5"/>',
  sun:       '<circle cx="12" cy="12" r="4.2"/><path d="M12 2.5v2.2"/><path d="M12 19.3v2.2"/><path d="M4.2 4.2l1.6 1.6"/><path d="M18.2 18.2l1.6 1.6"/><path d="M2.5 12h2.2"/><path d="M19.3 12h2.2"/><path d="M4.2 19.8l1.6-1.6"/><path d="M18.2 5.8l1.6-1.6"/>',
};

// iconHTML rend l'icône en chaîne, pour les endroits qui composent déjà du
// balisage. `aria-hidden` : le sens est porté par le title/aria-label du bouton,
// jamais par le dessin.
function iconHTML(name, size){
  const d = ICONS[name];
  if(!d) return '';
  const n = size || 18;
  return '<svg viewBox="0 0 24 24" width="' + n + '" height="' + n + '" fill="none" '
    + 'stroke="currentColor" stroke-width="1.8" stroke-linecap="round" '
    + 'stroke-linejoin="round" aria-hidden="true">' + d + '</svg>';
}

// icon rend l'icône en ÉLÉMENT, pour les listes construites en DOM (fichiers,
// discussions) où l'on ne veut pas d'innerHTML.
function icon(name, size){
  const span = document.createElement('span');
  span.className = 'icn';
  span.innerHTML = iconHTML(name, size); // contenu constant, jamais de donnée
  return span.firstElementChild || span;
}
