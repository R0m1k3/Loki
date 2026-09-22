// 28-i18n.js — interface en anglais, par-dessus une source française.
//
// L'amont (AJEAN) a un vrai système de clés : chaque texte de l'UI porte un
// data-i18n et vit dans une table FR/EN. Traduire Loki de la même façon
// demanderait de réécrire tous ses écrans d'un coup, avec le risque d'en casser
// un pour une clé oubliée. On prend l'autre chemin, additif : la source RESTE le
// français, et passer en anglais applique un DICTIONNAIRE sur le texte affiché.
//
// Conséquences, assumées :
//   - une chaîne absente du dictionnaire reste en français (dégradation douce,
//     jamais de « settings.memory.title » à l'écran) ;
//   - le dictionnaire s'enrichit sans toucher au reste de l'interface ;
//   - la correspondance est EXACTE (texte complet d'un nœud, une fois les
//     espaces retirés) : aucune substitution partielle, donc pas de mot traduit
//     au milieu d'une phrase qui ne l'est pas.
//
// Ce qui n'est JAMAIS traduit : le fil de discussion (#chat) — c'est ton
// contenu et celui du modèle —, le code (pre/code), les zones de saisie, et
// tout ce qui porte data-no-i18n.

const LANG_KEY = 'loki-lang';
function uiLang(){ try{ return localStorage.getItem(LANG_KEY) || 'fr'; }catch(e){ return 'fr'; } }

// Dictionnaire français → anglais. Couvre la « coque » : navigation des
// réglages, intitulés de sections, libellés de lignes, boutons, interrupteurs.
// Les longues bulles d'aide restent en français pour l'instant — elles sont
// nombreuses et leur traduction n'apporte rien tant que la coque ne l'est pas.
const I18N_EN = {
  // Navigation des réglages
  'Paramètres': 'Settings',
  'Intelligence artificielle': 'Artificial intelligence',
  'Moteur': 'Engine',
  'Application': 'Application',
  'Projets': 'Projects',
  'Config active': 'Active config',
  'Presets': 'Presets',
  'Mode agent': 'Agent mode',
  'Tâches planifiées': 'Scheduled tasks',
  'System prompt': 'System prompt',
  'Moteur llama.cpp': 'llama.cpp engine',
  'Journal du moteur': 'Engine log',
  'Dictée': 'Dictation',
  'Identité': 'Identity',
  'Apparence': 'Appearance',
  'Accès OpenAI': 'OpenAI access',
  'Actions': 'Actions',
  // Sections
  'Mémoire': 'Memory',
  'Accès internet': 'Internet access',
  'Contrôle du navigateur': 'Browser control',
  'Serveurs MCP': 'MCP servers',
  'Service du moteur': 'Engine service',
  'Modèle': 'Model',
  'Reconnaissance': 'Recognition',
  'Affichage': 'Display',
  'Endpoint compatible OpenAI': 'OpenAI-compatible endpoint',
  'Clé API': 'API key',
  'Connexion': 'Connection',
  'Format': 'Format',
  'Portée': 'Scope',
  'Contenu': 'Content',
  'Nom': 'Name',
  'Type': 'Type',
  'Consigne': 'Instruction',
  'Script': 'Script',
  'Preset': 'Preset',
  'Fréquence': 'Frequency',
  'Accès': 'Access',
  'Cartes graphiques': 'Graphics cards',
  'Réglages': 'Settings',
  'Échantillonnage': 'Sampling',
  'Sauvegarde chiffrée': 'Encrypted backup',
  'Ton avatar': 'Your avatar',
  'Avatar de Loki': "Loki's avatar",
  'À exécuter': 'To run',
  'Modèle utilisé': 'Model used',
  'Projet visé': 'Target project',
  'Toutes les': 'Every',
  'Accès à la mémoire': 'Memory access',
  'Accès au web': 'Web access',
  'Tâche active': 'Task enabled',
  'Fichier': 'File',
  'Destination': 'Destination',
  'Contexte': 'Context',
  'Vision': 'Vision',
  'Vérifier': 'Check',
  // Interrupteurs
  'activer le mode agent': 'enable agent mode',
  "me prévenir quand c'est prêt": 'notify me when it is ready',
  'chiffrer la mémoire sur le disque': 'encrypt memory on disk',
  "donner internet à l'IA": 'give the AI internet access',
  "laisser l'IA piloter un navigateur": 'let the AI drive a browser',
  'compactage automatique du contexte': 'automatic context compaction',
  'suspendre toutes les tâches': 'suspend all tasks',
  'masquer le raisonnement': 'hide reasoning',
  "masquer les appels d'outils": 'hide tool calls',
  'garder les bulles repliées': 'keep bubbles collapsed',
  'masquer la vitesse de génération': 'hide generation speed',
  'mode': 'mode',
  // Boutons
  'Libérer la VRAM': 'Free VRAM',
  'libérer la VRAM': 'free VRAM',
  'Chat': 'Chat',
  'Code': 'Code',
  'compacter': 'compact',
  'fermer': 'close',
  'relancer': 'restart',
  'Tester la connexion': 'Test connection',
  'supprimer': 'delete',
  'annuler': 'cancel',
  'Annuler': 'Cancel',
  'Confirmer': 'Confirm',
  'enregistrer': 'save',
  'Enregistrer': 'Save',
  '+ ajouter un projet': '+ add a project',
  '+ ajouter une tâche': '+ add a task',
  '+ ajouter un serveur': '+ add a server',
  '+ ajouter un point': '+ add an entry',
  '+ ajouter': '+ add',
  'exporter': 'export',
  'importer': 'import',
  'retirer': 'remove',
  'catalogue': 'catalog',
  'renommer': 'rename',
  'déplacer': 'move',
  'vérifier les mises à jour': 'check for updates',
  '↥ Vérifier les mises à jour': '↥ Check for updates',
  'mettre à jour le moteur': 'update the engine',
  "revenir au moteur de l'image": 'go back to the image engine',
  'démarrer': 'start',
  'redémarrer': 'restart',
  'arrêter': 'stop',
  'Télécharger ce modèle': 'Download this model',
  'Annuler le téléchargement': 'Cancel download',
  'Retirer le jeton': 'Remove token',
  'Tester le micro (3 s)': 'Test the mic (3 s)',
  'tester maintenant': 'test now',
  'afficher': 'show',
  'générer': 'generate',
  'définir…': 'set…',
  '⤓ exporter…': '⤓ export…',
  '⤓ télécharger': '⤓ download',
  'bench': 'bench',
  'Installer & compiler': 'Install & build',
  'voir plus': 'show more',
  'voir moins': 'show less',
  'copier': 'copy',
  'copié': 'copied',
  'réponse': 'response',
  'modifications': 'changes',
  'écriture en cours': 'writing',
  'exécution en cours…': 'running…',
  // Divers visibles
  'Nouvelle discussion': 'New chat',
  'Discussions': 'Chats',
  'Langue': 'Language',
  'Français': 'French',
  'Anglais': 'English',
};

// Sélecteurs dont le CONTENU n'est jamais traduit : ton texte et celui du
// modèle, le code, les champs de saisie.
const I18N_SKIP = '#chat, pre, code, textarea, input, [data-no-i18n]';

function i18nText(fr){
  const key = (fr || '').trim();
  if (!key) return null;
  const en = I18N_EN[key];
  if (en === undefined) return null;
  // On rend la traduction en conservant l'espacement d'origine autour du texte
  // (un « ' ' + label » dans le HTML garde son espace).
  const lead = fr.slice(0, fr.length - fr.trimStart().length);
  const tail = fr.slice(fr.trimEnd().length);
  return lead + en + tail;
}

// Traduit les nœuds texte et quelques attributs sous `root`.
function i18nApply(root){
  if (uiLang() !== 'en' || !root) return;
  const el = root.nodeType === 1 ? root : root.parentElement;
  if (el && el.closest && el.closest(I18N_SKIP)) return;
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, {
    acceptNode(n){
      if (!n.nodeValue || !n.nodeValue.trim()) return NodeFilter.FILTER_REJECT;
      if (n.parentElement && n.parentElement.closest(I18N_SKIP)) return NodeFilter.FILTER_REJECT;
      return NodeFilter.FILTER_ACCEPT;
    }
  });
  const todo = [];
  for (let n = walker.nextNode(); n; n = walker.nextNode()) todo.push(n);
  for (const n of todo){
    const tr = i18nText(n.nodeValue);
    if (tr !== null) n.nodeValue = tr;
  }
  // Attributs visibles (infobulles, champs vides, accessibilité).
  const scope = root.nodeType === 1 ? root : document.body;
  const all = scope.querySelectorAll ? [scope, ...scope.querySelectorAll('[placeholder],[title],[aria-label]')] : [];
  for (const e of all){
    if (!e.getAttribute) continue;
    for (const attr of ['placeholder', 'title', 'aria-label']){
      const v = e.getAttribute(attr);
      if (!v) continue;
      const tr = i18nText(v);
      if (tr !== null) e.setAttribute(attr, tr);
    }
  }
}

// Les panneaux sont rendus en JS bien après le chargement : un observateur
// traduit ce qui apparaît. Il ignore #chat, donc le fil ne passe jamais dessous.
function i18nWatch(){
  if (uiLang() !== 'en') return;
  const obs = new MutationObserver(muts => {
    for (const m of muts){
      for (const n of m.addedNodes){
        if (n.nodeType === 1 || n.nodeType === 3) i18nApply(n);
      }
    }
  });
  obs.observe(document.body, {childList: true, subtree: true});
}

// setUiLang : bascule la langue. Vers l'anglais, on applique tout de suite ;
// vers le français, on RECHARGE — le texte français d'origine a été remplacé
// dans le DOM, le recharger est le moyen le plus sûr de le retrouver intact.
function setUiLang(lang){
  const next = lang === 'en' ? 'en' : 'fr';
  const prev = uiLang();
  try{ localStorage.setItem(LANG_KEY, next); }catch(e){}
  if (next === prev) return;
  if (next === 'en'){ i18nApply(document.body); i18nWatch(); }
  else location.reload();
}

document.addEventListener('DOMContentLoaded', () => {
  const sel = document.getElementById('ui-lang');
  if (sel) sel.value = uiLang();
  i18nApply(document.body);
  i18nWatch();
});
