//! KEEP_TURN_IMAGES pour le tour tenu par la façade (lot 32) — port de
//! l'état du tour de `chat_keep_images.go` (`keepImages`).
//!
//! Sans la clé, une image montrée par un outil (capture, `see_image`) est
//! ÉPHÉMÈRE : vue pendant le tour, absente de l'historique. Avec la clé, elle
//! est gardée telle qu'envoyée, sous conditions :
//! - fil principal d'une discussion, vision réellement active (demandé au Go
//!   à chaque tour) ;
//! - rangée par référence par le Go et relue à l'octet près ;
//! - coût MESURÉ par le moteur (écart de `usage.prompt_tokens` entre deux
//!   requêtes, moins l'estimation du texte ajouté), jamais estimé ;
//! - toutes les images gardées tiennent dans 10 % de la fenêtre ; la
//!   première qui ne tient pas reste éphémère, et toutes celles qui la
//!   suivent dans le tour aussi (le préfixe divergera de toute façon à elle).
//!
//! Le retrait prioritaire par une compaction ou une réduction est dans
//! [`super::compact`] ; celui du début de tour, de la boucle, ici et dans
//! [`super::turn`].

use std::collections::{HashMap, HashSet};

use super::compact::msg_tokens;
use super::types::{Message, StatsEvent};
use super::wire::content_parts;

/// En dessous, la mesure ne vaut rien (écart noyé dans l'erreur
/// d'estimation du texte) : l'image reste éphémère.
pub const KEEP_IMG_MIN_TOKENS: i64 = 16;

/// `keepTurnImagesOn`.
pub fn keep_turn_images_on(cfg: &HashMap<String, String>) -> bool {
    matches!(
        cfg.get("KEEP_TURN_IMAGES")
            .map(|v| v.trim().to_lowercase())
            .as_deref(),
        Some("on" | "1" | "true" | "yes" | "oui")
    )
}

/// `keptImageTokens` : coût total des images gardées.
pub fn kept_image_tokens(msgs: &[Message]) -> i64 {
    msgs.iter().map(|m| m.img_tokens).sum()
}

/// `hasKeptImages`.
pub fn has_kept_images(msgs: &[Message]) -> bool {
    msgs.iter().any(|m| m.img_tokens > 0)
}

fn has_image_part(m: &Message) -> bool {
    content_parts(m.content.as_ref())
        .iter()
        .any(|p| p.get("type").and_then(|t| t.as_str()) == Some("image_url"))
}

#[derive(Debug, Clone, Copy, PartialEq)]
struct Pend {
    msg_idx: usize,
    extra_idx: usize,
    weight: i64,
}

/// L'état de KEEP_TURN_IMAGES pendant UN tour ; absent (`None` dans la
/// boucle) sans la clé, hors fil principal ou sans vision.
#[derive(Debug, Default)]
pub struct KeepImages {
    /// `keepImgBudget` : part de la fenêtre que les images gardées occupent.
    budget: i64,
    /// Une image du tour n'a pas été gardée : les suivantes non plus.
    stopped: bool,
    /// Images de l'étape courante, à l'essai jusqu'à la mesure de la requête
    /// qui les montre la première.
    pend: Vec<Pend>,
    /// Numéro de cette requête, contexte que la complétion précédente laisse
    /// (`ctxAfter`), premier message ajouté depuis.
    pend_req: i64,
    pend_base: i64,
    pend_from: usize,
    /// Requêtes envoyées ; messages de la dernière.
    req_n: i64,
    sent_len: usize,
    /// `ctxAfter` de la dernière complétion complète (0 : inconnu).
    last_after: i64,
}

impl KeepImages {
    pub fn new(budget: i64) -> Self {
        KeepImages {
            budget,
            ..Default::default()
        }
    }

    /// Faut-il ranger l'image (ni tour arrêté, ni budget déjà plein) ?
    pub fn wants(&self, messages: &[Message]) -> bool {
        !self.stopped && kept_image_tokens(messages) < self.budget
    }

    /// `relay` : ajoute le message qui relaie une image d'outil. `stored` :
    /// l'image rangée par référence et son poids (octets), `None` si elle ne
    /// l'a pas été — elle reste alors éphémère, et le tour s'arrête là pour
    /// les suivantes. `from` : début des messages de l'étape.
    pub fn relay(
        &mut self,
        messages: &mut Vec<Message>,
        extra: &mut Vec<Message>,
        mut m: Message,
        from: usize,
        stored: Option<(Message, i64)>,
    ) {
        m.img_relay = true;
        if self.stopped {
            messages.push(m);
            return;
        }
        let Some((mut r, weight)) = stored.filter(|(_, w)| *w > 0) else {
            self.stopped = true;
            messages.push(m);
            return;
        };
        r.img_relay = true;
        if self.pend.is_empty() {
            self.pend_req = self.req_n + 1;
            self.pend_base = self.last_after;
            self.pend_from = from;
        }
        messages.push(r.clone());
        extra.push(r);
        self.pend.push(Pend {
            msg_idx: messages.len() - 1,
            extra_idx: extra.len() - 1,
            weight,
        });
    }

    /// `sending` : une requête part avec `n` messages.
    pub fn sending(&mut self, n: usize) {
        self.req_n += 1;
        self.sent_len = n;
    }

    /// `observe` : une complétion est lue. Mesure le coût des images à
    /// l'essai si c'est la requête qui les a montrées la première, et les
    /// garde si le budget le permet ; sinon elles redeviennent éphémères.
    pub fn observe(
        &mut self,
        complete: bool,
        st: &StatsEvent,
        messages: &mut [Message],
        extra: &mut Vec<Message>,
    ) {
        let after = if complete && st.prompt_tokens_total > 0 {
            st.ctx_after()
        } else {
            0
        };
        if !self.pend.is_empty() {
            if !self.measure(after, st.prompt_tokens_total, messages, extra) {
                self.reject(extra);
            }
            self.pend.clear();
        }
        self.last_after = after;
    }

    /// `measure` : répartit le coût mesuré entre les images à l'essai, au
    /// prorata de leur poids, et les garde si tout tient dans le budget.
    fn measure(
        &self,
        after: i64,
        prompt: i64,
        messages: &mut [Message],
        extra: &mut [Message],
    ) -> bool {
        if after <= 0
            || self.pend_base <= 0
            || self.pend_req != self.req_n
            || self.pend_from > self.sent_len
            || self.sent_len > messages.len()
        {
            return false;
        }
        // Ce que la requête a ajouté depuis la précédente, hors images.
        let text: i64 = messages[self.pend_from..self.sent_len]
            .iter()
            .filter(|m| !m.img_relay)
            .map(msg_tokens)
            .sum();
        let img = prompt - self.pend_base - text;
        let weight: i64 = self.pend.iter().map(|p| p.weight).sum();
        let n = self.pend.len() as i64;
        if weight <= 0 || img < KEEP_IMG_MIN_TOKENS * n {
            return false;
        }
        if kept_image_tokens(messages) + img > self.budget {
            return false;
        }
        let mut shares = Vec::with_capacity(self.pend.len());
        let mut left = img;
        for (i, p) in self.pend.iter().enumerate() {
            let share = if i == self.pend.len() - 1 {
                left
            } else {
                img * p.weight / weight
            };
            if share < KEEP_IMG_MIN_TOKENS {
                return false;
            }
            left -= share;
            shares.push(share);
        }
        if self.pend.iter().any(|p| {
            !messages.get(p.msg_idx).is_some_and(|m| m.img_relay)
                || !extra.get(p.extra_idx).is_some_and(|m| m.img_relay)
        }) {
            return false;
        }
        for (p, share) in self.pend.iter().zip(shares) {
            messages[p.msg_idx].img_tokens = share;
            extra[p.extra_idx].img_tokens = share;
        }
        true
    }

    /// `reject` : retire d'`extra` les images à l'essai — éphémères, comme
    /// sans la clé — et arrête le tour pour les suivantes.
    fn reject(&mut self, extra: &mut Vec<Message>) {
        if self.pend.is_empty() {
            return;
        }
        self.stopped = true;
        let drop: HashSet<usize> = self.pend.iter().map(|p| p.extra_idx).collect();
        let mut i = 0;
        extra.retain(|_| {
            let keep = !drop.contains(&i);
            i += 1;
            keep
        });
        self.pend.clear();
    }

    /// `rewritten` : l'historique vient d'être réécrit en cours de tour et
    /// `extra` repart de zéro. Les images à l'essai ne se mesurent plus :
    /// éphémères, et le tour s'arrête là pour les suivantes.
    pub fn rewritten(&mut self) {
        if !self.pend.is_empty() {
            self.stopped = true;
        }
        self.pend.clear();
    }

    /// `finish` : fin du tour. Une image encore à l'essai n'a pas été
    /// mesurée : éphémère.
    pub fn finish(&mut self, extra: &mut Vec<Message>) {
        self.reject(extra);
    }
}

/// `publishable` : l'historique publié par une réécriture en cours de tour,
/// sans les relais dont l'image n'est pas gardée. Sans l'état : tel quel.
pub fn publishable(kt: Option<&KeepImages>, msgs: &[Message]) -> Vec<Message> {
    if kt.is_none() {
        return msgs.to_vec();
    }
    msgs.iter()
        .filter(|m| !(m.img_relay && m.img_tokens == 0 && has_image_part(m)))
        .cloned()
        .collect()
}

#[cfg(test)]
mod tests;
