//! Tests de l'état KEEP_TURN_IMAGES d'un tour (cas de
//! `chat_keep_images_test.go`).

use super::*;
use crate::chat::compact::IMAGE_LOST_MARKER;
use serde_json::{Value, json};

fn um(s: &str) -> Message {
    Message {
        role: "user".into(),
        content: Some(json!(s)),
        ..Default::default()
    }
}

fn tm(s: &str) -> Message {
    Message {
        role: "tool".into(),
        tool_call_id: "v1".into(),
        content: Some(json!(s)),
        ..Default::default()
    }
}

fn img(url: &str) -> Message {
    Message {
        role: "user".into(),
        content: Some(json!([
            {"type": "text", "text": "Voici la capture demandée."},
            {"type": "image_url", "image_url": {"url": url}}
        ])),
        ..Default::default()
    }
}

fn stats(prompt: i64, gen_tokens: i64) -> StatsEvent {
    StatsEvent {
        prompt_tokens_total: prompt,
        gen_tokens,
        ..Default::default()
    }
}

fn kept(tokens: i64) -> Message {
    let mut m = img("loki-img:aa.png");
    m.img_relay = true;
    m.img_tokens = tokens;
    m
}

/// Une étape `see_image` : première complétion (prompt 1000, 20 générés),
/// résultat d'outil, relais rangé ; rend (état, messages, extra, base du
/// texte ajouté).
fn step(k: &mut KeepImages, stored: bool) -> (Vec<Message>, Vec<Message>, i64) {
    let mut messages = vec![um("regarde img.png")];
    let mut extra = Vec::new();
    k.sending(messages.len());
    k.observe(true, &stats(1000, 20), &mut messages, &mut extra);
    let from = messages.len();
    let t = tm("[ok] image chargée : img.png");
    let text = msg_tokens(&t);
    messages.push(t.clone());
    extra.push(t);
    let s = stored.then(|| (img("loki-img:aa.png"), 1000));
    k.relay(
        &mut messages,
        &mut extra,
        img("data:image/png;base64,AA=="),
        from,
        s,
    );
    k.sending(messages.len());
    (messages, extra, text)
}

#[test]
fn cle() {
    for (v, want) in [
        ("on", true),
        (" OUI ", true),
        ("1", true),
        ("true", true),
        ("yes", true),
        ("off", false),
        ("", false),
        ("2", false),
    ] {
        let cfg = HashMap::from([("KEEP_TURN_IMAGES".to_string(), v.to_string())]);
        assert_eq!(keep_turn_images_on(&cfg), want, "{v:?}");
    }
    assert!(!keep_turn_images_on(&HashMap::new()));
}

/// Coût mesuré par le moteur : écart de prompt moins le texte ajouté ; gardée
/// par référence, dans `messages` comme dans `extra`.
#[test]
fn gardee_et_mesuree() {
    let mut k = KeepImages::new(3200);
    let (mut messages, mut extra, text) = step(&mut k, true);
    assert_eq!(extra.len(), 2);
    k.observe(
        true,
        &stats(1020 + text + 300, 5),
        &mut messages,
        &mut extra,
    );
    assert_eq!(extra.len(), 2);
    let r = &extra[1];
    assert!(r.img_relay && r.img_tokens == 300, "{r:?}");
    assert_eq!(
        r.content.as_ref().unwrap()[1]["image_url"]["url"],
        "loki-img:aa.png"
    );
    assert_eq!(messages[2].img_tokens, 300);
    assert_eq!(kept_image_tokens(&messages), 300);
}

/// Au-delà du budget, l'image reste éphémère : retirée d'`extra`, gardée
/// dans `messages` (le modèle l'a vue), et le tour s'arrête là.
#[test]
fn budget() {
    let mut k = KeepImages::new(200);
    let (mut messages, mut extra, text) = step(&mut k, true);
    k.observe(
        true,
        &stats(1020 + text + 300, 5),
        &mut messages,
        &mut extra,
    );
    assert_eq!(extra.len(), 1);
    assert_eq!(messages.len(), 3);
    assert!(messages[2].img_relay && messages[2].img_tokens == 0);
    assert!(k.stopped);
    // Une image suivante n'est même pas rangée.
    assert!(!k.wants(&messages));
}

/// Moteur sans usage, ou complétion coupée : rien à mesurer.
#[test]
fn sans_mesure() {
    for (complete, prompt) in [(true, 0), (false, 2000)] {
        let mut k = KeepImages::new(3200);
        let (mut messages, mut extra, _) = step(&mut k, true);
        k.observe(complete, &stats(prompt, 5), &mut messages, &mut extra);
        assert_eq!(extra.len(), 1, "complète={complete} prompt={prompt}");
        assert_eq!(kept_image_tokens(&messages), 0);
    }
    // Première complétion sans usage : pas de base.
    let mut k = KeepImages::new(3200);
    let mut messages = vec![um("q")];
    let mut extra = Vec::new();
    k.sending(1);
    k.observe(true, &stats(0, 0), &mut messages, &mut extra);
    let t = tm("[ok]");
    messages.push(t.clone());
    extra.push(t);
    k.relay(
        &mut messages,
        &mut extra,
        img("data:x"),
        1,
        Some((img("loki-img:b.png"), 9)),
    );
    k.sending(messages.len());
    k.observe(true, &stats(5000, 5), &mut messages, &mut extra);
    assert_eq!(extra.len(), 1);
}

/// Requête relancée entre-temps (reprise réseau) : la mesure ne vaut plus.
#[test]
fn requete_relancee() {
    let mut k = KeepImages::new(3200);
    let (mut messages, mut extra, text) = step(&mut k, true);
    k.sending(messages.len());
    k.observe(
        true,
        &stats(1020 + text + 300, 5),
        &mut messages,
        &mut extra,
    );
    assert_eq!(extra.len(), 1);
}

/// Rangement refusé : éphémère, marqué relais, et les suivantes aussi.
#[test]
fn rangement_refuse() {
    let mut k = KeepImages::new(3200);
    let (mut messages, extra, _) = step(&mut k, false);
    assert_eq!(extra.len(), 1);
    assert!(messages[2].img_relay);
    assert!(k.stopped);
    let mut extra = extra;
    k.relay(
        &mut messages,
        &mut extra,
        img("data:y"),
        1,
        Some((img("loki-img:c.png"), 5)),
    );
    assert_eq!(extra.len(), 1, "tour arrêté : plus rien de gardé");
    assert!(messages[3].img_relay);
}

/// Deux images dans l'étape : le coût réparti au prorata des octets, la
/// dernière prend le reste ; une part sous le seuil, tout reste éphémère.
#[test]
fn partage_entre_images() {
    let mut k = KeepImages::new(3200);
    let mut messages = vec![um("q")];
    let mut extra = Vec::new();
    k.sending(1);
    k.observe(true, &stats(1000, 20), &mut messages, &mut extra);
    k.relay(
        &mut messages,
        &mut extra,
        img("data:a"),
        1,
        Some((img("loki-img:a.png"), 3000)),
    );
    k.relay(
        &mut messages,
        &mut extra,
        img("data:b"),
        1,
        Some((img("loki-img:b.png"), 1000)),
    );
    k.sending(messages.len());
    k.observe(true, &stats(1020 + 401, 5), &mut messages, &mut extra);
    assert_eq!(
        (extra[0].img_tokens, extra[1].img_tokens),
        (300, 101),
        "{extra:?}"
    );
    let mut k = KeepImages::new(3200);
    let mut messages = vec![um("q")];
    let mut extra = Vec::new();
    k.sending(1);
    k.observe(true, &stats(1000, 20), &mut messages, &mut extra);
    k.relay(
        &mut messages,
        &mut extra,
        img("data:a"),
        1,
        Some((img("loki-img:a.png"), 3000)),
    );
    k.relay(
        &mut messages,
        &mut extra,
        img("data:b"),
        1,
        Some((img("loki-img:b.png"), 10)),
    );
    k.sending(messages.len());
    k.observe(true, &stats(1020 + 400, 5), &mut messages, &mut extra);
    assert!(extra.is_empty(), "part de 1 jeton : {extra:?}");
}

/// Réécriture en cours de tour : une image non gardée n'entre pas dans
/// l'historique publié ; gardée ou déjà retirée, si. Les images à l'essai ne
/// sont plus suivies.
#[test]
fn publication() {
    let mut eph = img("data:image/png;base64,AA==");
    eph.img_relay = true;
    let mut stub = um(&format!("Image :{IMAGE_LOST_MARKER}"));
    stub.img_relay = true;
    let msgs = vec![um("q"), tm("a"), eph, tm("b"), kept(50), stub];
    let k = KeepImages::new(3200);
    let got = publishable(Some(&k), &msgs);
    assert_eq!(got.len(), 5);
    assert_eq!(got[2].content, Some(Value::from("b")));
    assert_eq!(publishable(None, &msgs).len(), 6, "sans la clé : tel quel");
    let mut k = KeepImages::new(3200);
    let (_, _, _) = step(&mut k, true);
    k.rewritten();
    assert!(k.stopped && k.pend.is_empty());
    let mut k = KeepImages::new(3200);
    k.rewritten();
    assert!(!k.stopped, "rien à l'essai : le tour continue");
}

/// Fin du tour avec une image à l'essai : éphémère.
#[test]
fn fin_de_tour() {
    let mut k = KeepImages::new(3200);
    let (_, mut extra, _) = step(&mut k, true);
    k.finish(&mut extra);
    assert_eq!(extra.len(), 1);
    assert_eq!(extra[0].role, "tool");
}

/// Budget déjà plein : rien à ranger.
#[test]
fn budget_plein_avant_rangement() {
    let k = KeepImages::new(200);
    assert!(!k.wants(&[um("q"), kept(200)]));
    assert!(k.wants(&[um("q"), kept(199)]));
}
