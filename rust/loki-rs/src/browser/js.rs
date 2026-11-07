//! Scripts injectés dans la page (copiés de `computer_cdp.go`, à l'octet près :
//! le journal de parité compare les expressions envoyées).

/// `cdpSameTabJS` : les navigations « nouvel onglet » restent dans l'onglet.
pub const SAME_TAB_JS: &str = r#"(function(){try{
  window.open=function(u){if(u)location.href=u;return window;};
  document.addEventListener('click',function(e){
    var a=e.target&&e.target.closest&&e.target.closest('a[target]');
    if(a)a.removeAttribute('target');
  },true);
}catch(e){}})();"#;

/// `cdpHelpersJS` : rôle, nom, actionnable, visible, shadow DOM.
pub const HELPERS_JS: &str = r#"
function ajRole(el){var r=el.getAttribute('role');if(r)return r;var t=el.tagName.toLowerCase();if(t==='a')return 'link';if(t==='button')return 'button';if(t==='select')return 'combobox';if(t==='textarea')return 'textbox';if(t==='input'){var ty=(el.type||'text').toLowerCase();var m={checkbox:'checkbox',radio:'radio',button:'button',submit:'button',reset:'button',file:'button',range:'slider',password:'password',search:'searchbox'};return m[ty]||'textbox';}return t;}
function ajName(el){var t=el.tagName.toLowerCase();var lbl=el.getAttribute('aria-label')||'';if(!lbl){var lb=el.getAttribute('aria-labelledby');if(lb){var e2=document.getElementById(lb.split(' ')[0]);if(e2)lbl=e2.innerText||'';}}if(!lbl&&el.labels&&el.labels.length)lbl=el.labels[0].innerText||'';if(!lbl)lbl=el.getAttribute('placeholder')||el.getAttribute('title')||el.getAttribute('alt')||'';var val='';if((t==='input'||t==='textarea')&&el.type!=='password'&&el.type!=='hidden')val=el.value||'';var n=val?(lbl?(lbl+' = '+val):val):lbl;if(!n&&t!=='input'&&t!=='select'&&t!=='textarea')n=el.innerText||'';if(!n){var c=el.querySelector('img[alt],[aria-label],[title],svg title');if(c)n=c.getAttribute&&(c.getAttribute('alt')||c.getAttribute('aria-label')||c.getAttribute('title'))||c.textContent||'';}if(!n)n=el.getAttribute('name')||el.getAttribute('value')||'';return (n||'').replace(/\s+/g,' ').trim().slice(0,100);}
function ajActionable(el){var t=el.tagName.toLowerCase();if(t==='a')return !!el.getAttribute('href');if(t==='button'||t==='input'||t==='select'||t==='textarea'||t==='summary')return el.type!=='hidden';if(el.isContentEditable)return true;if(el.getAttribute('onclick'))return true;var r=(el.getAttribute('role')||'').toLowerCase();return {button:1,link:1,checkbox:1,radio:1,tab:1,menuitem:1,menuitemcheckbox:1,menuitemradio:1,option:1,'switch':1,combobox:1,textbox:1,searchbox:1,slider:1,spinbutton:1}[r]===1;}
function ajVis(el){var r=el.getBoundingClientRect();if(r.width<3||r.height<3)return false;var s=getComputedStyle(el);if(s.visibility==='hidden'||s.display==='none'||s.opacity==='0')return false;if(r.bottom<0||r.top>innerHeight||r.right<0||r.left>innerWidth)return false;return true;}
var ajSel='a[href],button,input,select,textarea,[role],[onclick],[tabindex],summary,[contenteditable="true"]';
function ajToggle(el){var t=el.tagName.toLowerCase(),ty=(el.type||'').toLowerCase(),r=(el.getAttribute('role')||'').toLowerCase();return (t==='input'&&(ty==='radio'||ty==='checkbox'))||r==='radio'||r==='checkbox'||r==='switch';}
function ajTarget(el){if(!ajToggle(el)||ajVis(el))return el;var c=[];if(el.labels)for(var i=0;i<el.labels.length;i++)c.push(el.labels[i]);if(el.id){try{var l=document.querySelector('label[for="'+CSS.escape(el.id)+'"]');if(l)c.push(l);}catch(e){}}var p=el.closest&&el.closest('label');if(p)c.push(p);var q=el.parentElement;for(var k=0;k<3&&q;k++,q=q.parentElement){if(q.querySelectorAll('input[type=radio],input[type=checkbox],[role=radio],[role=checkbox]').length===1)c.push(q);}for(var j=0;j<c.length;j++)if(ajVis(c[j]))return c[j];return el;}
function ajOn(el){return !!(el.checked||el.getAttribute('aria-checked')==='true'||el.getAttribute('aria-pressed')==='true');}
function ajState(el){var s='';if(ajOn(el))s+=' [coché]';else if(el.getAttribute('aria-selected')==='true')s+=' [sélectionné]';if(el.disabled||el.getAttribute('aria-disabled')==='true')s+=' [désactivé]';return s;}
// Traversée du SHADOW DOM : les bandeaux cookies et composants web (usercentrics,
// cookiebot…) vivent dans des shadow roots que querySelectorAll ne voit pas → le
// modèle ne pouvait ni les lister ni les cliquer (test Lady Sushi). ajQueryAll /
// ajByRef / ajClear descendent récursivement dans chaque shadowRoot ouvert.
function ajQueryAll(){var out=[];function w(r){var e=r.querySelectorAll(ajSel);for(var i=0;i<e.length;i++)out.push(e[i]);var a=r.querySelectorAll('*');for(var j=0;j<a.length;j++)if(a[j].shadowRoot)w(a[j].shadowRoot);}w(document);return out;}
function ajByRef(ref){function f(r){var e=r.querySelector('[data-loki-ref="'+ref+'"]');if(e)return e;var a=r.querySelectorAll('*');for(var i=0;i<a.length;i++)if(a[i].shadowRoot){var x=f(a[i].shadowRoot);if(x)return x;}return null;}return f(document);}
function ajClear(){function w(r){var o=r.querySelectorAll('[data-loki-ref]');for(var i=0;i<o.length;i++)o[i].removeAttribute('data-loki-ref');var a=r.querySelectorAll('*');for(var j=0;j<a.length;j++)if(a[j].shadowRoot)w(a[j].shadowRoot);}w(document);}
"#;

/// La fin de `cdpSnapshotJS` (préfixée de [`HELPERS_JS`]).
pub const SNAPSHOT_TAIL_JS: &str = r#"(function(){
  ajClear();
  // Case radio/à cocher cachée (site moderne : l'étiquette visible porte le clic) :
  // gardée via sa cible visible, sinon les choix d'un configurateur n'existaient pas.
  var els=ajQueryAll().filter(function(el){return ajActionable(el)&&ajVis(ajTarget(el));});
  var map={},refs=[],i=0;
  for(var k=0;k<els.length&&i<200;k++){var el=els[k];var nm=ajName(el),rl=ajRole(el);
    i++;var r=ajTarget(el).getBoundingClientRect();map[i]={x:r.left+r.width/2,y:r.top+r.height/2};
    el.setAttribute('data-loki-ref',String(i));
    refs.push('['+i+'] '+rl+(nm?' "'+nm+'"':'')+ajState(el));
  }
  window.__lokiRefs=map;
  var de=document.documentElement;
  var sh=Math.max(de.scrollHeight,document.body?document.body.scrollHeight:0);
  return {title:document.title||'',url:location.href,refs:refs,
    above:Math.round(scrollY),below:Math.round(Math.max(0,sh-scrollY-innerHeight))};
})()"#;

/// `cdpFindJS` : recherche non destructive sur toute la page.
pub const FIND_JS: &str = r#"function(q){
  q=(q||'').toLowerCase();
  var pool=ajQueryAll();
  var maxRef=0;for(var m=0;m<pool.length;m++){var v=parseInt(pool[m].getAttribute('data-loki-ref'),10)||0;if(v>maxRef)maxRef=v;}
  var els=pool.filter(ajActionable);
  var refs=[],count=0,first=null;
  for(var k=0;k<els.length&&count<60;k++){var el=els[k];var nm=ajName(el),rl=ajRole(el);
    if((rl+' '+nm).toLowerCase().indexOf(q)<0)continue;
    var st=getComputedStyle(el);if(st.display==='none'||st.visibility==='hidden')continue;
    var r=el.getBoundingClientRect();if(r.width<1&&r.height<1)continue;
    var ref=el.getAttribute('data-loki-ref');
    if(!ref){maxRef++;ref=String(maxRef);el.setAttribute('data-loki-ref',ref);}
    if(!first)first=el;count++;
    refs.push('['+ref+'] '+rl+(nm?' "'+nm+'"':'')+ajState(el));
  }
  // Texte simple (non cliquable) : une date de livraison, un message d'erreur,
  // un prix. Sans ça, le modèle ne pouvait lire ces informations qu'à la capture.
  var texts=[],seen={};
  if(q){var tw=document.createTreeWalker(document.body,NodeFilter.SHOW_TEXT);var n;
    while((n=tw.nextNode())&&texts.length<8){var v=(n.nodeValue||'');if(v.toLowerCase().indexOf(q)<0)continue;
      var pe=n.parentElement;if(!pe||pe.closest('script,style,noscript'))continue;
      var st2=getComputedStyle(pe);if(st2.display==='none'||st2.visibility==='hidden')continue;
      var rr=pe.getBoundingClientRect();if(rr.width<1&&rr.height<1)continue;
      var blk=pe;for(var u=0;u<3&&blk.parentElement&&(blk.innerText||'').length<60;u++)blk=blk.parentElement;
      var tx=(blk.innerText||v).replace(/\s+/g,' ').trim();if(tx.length>240)tx=tx.slice(0,240)+'…';
      if(seen[tx])continue;seen[tx]=1;texts.push(tx);if(!first)first=pe;}}
  if(first)ajTarget(first).scrollIntoView({block:'center'});
  return {refs:refs,texts:texts};
}"#;

/// `cdpSnapshotJS`.
pub fn snapshot_js() -> String {
    format!("{HELPERS_JS}{SNAPSHOT_TAIL_JS}")
}
