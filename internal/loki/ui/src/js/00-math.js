// --- Formules LaTeX (KaTeX) ---------------------------------------------------
// Repris d'AJEAN (#123). md() (00-md.js) met les formules de côté AVANT marked,
// qui abîmerait \\, _ et * : un repère (caractères à usage privé U+E000/U+E001
// autour du numéro) prend leur place, puis reçoit le rendu. Le code (blocs et
// `en ligne`) n'est jamais touché. Formes reconnues : $$…$$ et \[…\] (bloc),
// \(…\) et $…$ (en ligne). $…$ n'est une formule que s'il ne commence ni ne finit
// par une espace, n'est pas collé à un mot ni échappé (\$) avant, et n'est pas
// suivi d'un chiffre : « 5 $ et 10 $ » ou « $5 et $10 » restent du texte.
// Ajouts de Loki : un $ ouvrant précédé d'un chiffre et d'une espace (« 5 $, »)
// est un montant, et $…$ ne contient pas de ` — sans quoi « 5 $, code `$x$` »
// s'apparierait par-dessus le code en ligne.
// KaTeX (embarqué, servi sous /katex/ par web_katex.go et la façade) n'est chargé
// qu'à la première formule ; d'ici là la source s'affiche, puis est rendue sur
// place. (Pas de lookbehind dans l'expression : les iOS d'avant 16.4 refusent de
// charger le script entier. Ces contrôles-là sont faits dans le remplacement.)
//
// MATH_RE et mathExtract sont purs (ni DOM ni réseau) : web_katex_math_test.go les
// exécute sous node sur les cas limites (montants, code, échappements).
const MATH_RE = /(```[\s\S]*?(?:```|$)|~~~[\s\S]*?(?:~~~|$)|`[^`\n]*`)|\$\$([\s\S]+?)\$\$|\\\[([\s\S]+?)\\\]|\\\(([\s\S]+?)\\\)|\$(?![\s$])((?:\\.|[^$\\\n`])+?)\$(?![\d$])/g;
const MATH_MARK_RE = /\uE000(\d+)\uE001/g;
function mathExtract(s){
  const list = [];
  const text = s.replace(MATH_RE, (all, code, dd, br, pa, d, at)=>{
    if(code !== undefined) return all;
    // $…$ : pas collé à un mot ni échappé (\$) avant, pas un montant (« 5 $ »),
    // pas d'espace avant le $ final.
    if(d !== undefined && (/[\\$\w]/.test(s[at-1] || '') || /\d\s$/.test(s.slice(Math.max(0, at-2), at)) || /\s$/.test(d))) return all;
    const display = dd !== undefined || br !== undefined;
    list.push({tex: (dd ?? br ?? pa ?? d).trim(), display});
    return '\uE000' + (list.length-1) + '\uE001';
  });
  return {text, list};
}
// mathRestore remplace chaque repère du HTML produit par marked par le rendu de
// sa formule (render : formule → HTML).
function mathRestore(html, list, render){
  return list.length ? html.replace(MATH_MARK_RE, (all, i)=>list[+i] ? render(list[+i]) : all) : html;
}
const mathEsc = (s)=>s.replace(/[&<>"]/g, c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]));
const MATH_OPTS = {throwOnError: false, output: 'html'};
function mathHtml(f){
  if(window.katex){
    try{ return katex.renderToString(f.tex, {...MATH_OPTS, displayMode: f.display}); }
    catch(_){ return '<code>'+mathEsc(f.tex)+'</code>'; }
  }
  mathLoad();
  return '<span class="math-pending" data-d="'+(f.display?1:0)+'">'+mathEsc(f.tex)+'</span>';
}
let mathLoading = null;
function mathLoad(){
  if(mathLoading) return mathLoading;
  if(!document.querySelector('link[data-katex]')){
    const css = document.createElement('link');
    css.rel = 'stylesheet'; css.href = 'katex/katex.min.css'; css.dataset.katex = '1';
    document.head.appendChild(css);
  }
  mathLoading = new Promise((ok, ko)=>{
    const s = document.createElement('script');
    s.src = 'katex/katex.min.js'; s.onload = ok;
    // Échec (hors ligne, 404) : script retiré, nouvel essai à la prochaine formule.
    s.onerror = ()=>{ s.remove(); ko(); };
    document.head.appendChild(s);
  }).then(()=>{
    document.querySelectorAll('.math-pending').forEach(el=>{
      try{ katex.render(el.textContent, el, {...MATH_OPTS, displayMode: el.dataset.d === '1'}); }catch(_){}
      el.classList.remove('math-pending');
    });
  }).catch(()=>{ mathLoading = null; });
  return mathLoading;
}
