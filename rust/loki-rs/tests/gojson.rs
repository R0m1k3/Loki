//! Décodage JSON des requêtes au comportement d'`encoding/json` : le journal
//! tiré du Go (`internal/loki/parity_gojson_test.go` →
//! `tests/parity_gojson/decode.json`) est rejoué contre `gojson`.

use loki_rs::gojson::{Kind, decode_struct};
use serde_json::{Value, json};

const FIELDS: &[(&str, Kind)] = &[
    ("name", Kind::Str),
    ("on", Kind::Bool),
    ("n", Kind::Int),
    ("args", Kind::StrList),
    ("env", Kind::StrMap),
    ("opt", Kind::OptStr),
    ("flag", Kind::OptBool),
];

#[test]
fn parite_du_decodage() {
    let fx: Value =
        serde_json::from_str(include_str!("parity_gojson/decode.json")).expect("journal");
    for c in fx["cases"].as_array().unwrap() {
        let input = c["in"].as_str().unwrap();
        let d = decode_struct(input.as_bytes(), "parityDecodeT", FIELDS);
        let out = json!({
            "name": d.str("name"),
            "on": d.bool("on"),
            "n": d.int("n"),
            "args": d.str_list("args"),
            "env": d.str_map("env").map(|m| m.into_iter().map(|(k, v)| (k, Value::String(v))).collect::<serde_json::Map<_, _>>()),
            "opt": d.opt_str("opt"),
            "flag": d.opt_bool("flag"),
        });
        assert_eq!(out, c["out"], "{input:?} : champs");
        assert_eq!(
            d.err.as_deref().unwrap_or(""),
            c["err"].as_str().unwrap_or(""),
            "{input:?} : erreur"
        );
    }
}
