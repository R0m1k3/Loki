//! Décodage JSON au comportement d'`encoding/json` du Go
//! (`json.NewDecoder(r.Body).Decode(&req)`), pour les routes portées qui
//! renvoient le message d'erreur du Go au client (`{"error": err.Error()}`).
//!
//! Ce qui est reproduit :
//! - seule la PREMIÈRE valeur du corps compte (ce qui suit est ignoré) ;
//! - corps vide → `EOF`, valeur tronquée → `unexpected EOF` ;
//! - erreurs de syntaxe du scanner Go (`invalid character 'p' looking for
//!   beginning of value`, `… after object key`, `… in string literal`…) ;
//! - `null` ne touche à rien ; noms de champs sans égard à la casse ;
//! - erreurs de type (`json: cannot unmarshal number into Go struct field
//!   saveReq.name of type string`) : la première est rendue, les autres champs
//!   sont décodés quand même, comme le Go.

use serde::de::{Deserialize, Deserializer, MapAccess, Visitor};
use serde_json::value::RawValue;
use serde_json::{Map, Value};
use std::marker::PhantomData;

/// Type Go d'un champ de la struct cible.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Kind {
    /// `string`
    Str,
    /// `bool`
    Bool,
    /// `int`
    Int,
    /// `[]string`
    StrList,
    /// `map[string]string`
    StrMap,
    /// `*string` : présent même vide, `null` = absent.
    OptStr,
    /// `*bool`
    OptBool,
    /// `any` / `json.RawMessage` : tout est accepté tel quel.
    Any,
}

impl Kind {
    fn go_type(self) -> &'static str {
        match self {
            Kind::Str => "string",
            Kind::Bool => "bool",
            Kind::Int => "int",
            Kind::StrList => "[]string",
            Kind::StrMap => "map[string]string",
            Kind::OptStr => "*string",
            Kind::OptBool => "*bool",
            Kind::Any => "interface {}",
        }
    }
}

/// Résultat d'un décodage : les champs posés (clé = nom JSON de la struct,
/// valeur déjà au bon type) et la première erreur, comme `Decode` la rend.
#[derive(Debug, Default)]
pub struct Decoded {
    pub fields: Map<String, Value>,
    pub err: Option<String>,
}

impl Decoded {
    /// Champ texte (`""` si absent).
    pub fn str(&self, name: &str) -> String {
        self.fields
            .get(name)
            .and_then(Value::as_str)
            .unwrap_or("")
            .to_string()
    }

    /// Champ `*string` : `None` si absent ou `null`.
    pub fn opt_str(&self, name: &str) -> Option<String> {
        self.fields
            .get(name)
            .and_then(Value::as_str)
            .map(str::to_string)
    }

    /// Champ booléen (`false` si absent).
    pub fn bool(&self, name: &str) -> bool {
        self.fields
            .get(name)
            .and_then(Value::as_bool)
            .unwrap_or(false)
    }

    /// Champ `*bool`.
    pub fn opt_bool(&self, name: &str) -> Option<bool> {
        self.fields.get(name).and_then(Value::as_bool)
    }

    /// Champ entier (`0` si absent).
    pub fn int(&self, name: &str) -> i64 {
        self.fields.get(name).and_then(Value::as_i64).unwrap_or(0)
    }

    /// Champ `[]string` (`None` = nil côté Go).
    pub fn str_list(&self, name: &str) -> Option<Vec<String>> {
        self.fields.get(name).and_then(Value::as_array).map(|a| {
            a.iter()
                .map(|v| v.as_str().unwrap_or("").to_string())
                .collect()
        })
    }

    /// Champ `map[string]string` (`None` = nil côté Go).
    pub fn str_map(&self, name: &str) -> Option<Vec<(String, String)>> {
        self.fields.get(name).and_then(Value::as_object).map(|m| {
            m.iter()
                .map(|(k, v)| (k.clone(), v.as_str().unwrap_or("").to_string()))
                .collect()
        })
    }
}

/// `json.NewDecoder(body).Decode(&v)` avec `v` une struct Go dont les
/// champs sont `fields` (nom JSON, type). `ty` : nom de la struct sans le
/// paquet (`saveReq`), ou, pour une struct anonyme, son type complet tel que
/// `reflect` l'écrit (`struct { On bool "json:\"on\"" }`).
pub fn decode_struct(body: &[u8], ty: &str, fields: &[(&str, Kind)]) -> Decoded {
    let (prefix, full) = if ty.starts_with("struct {") {
        ("", ty.to_string())
    } else {
        (ty, format!("loki.{ty}"))
    };
    let ty = prefix;
    let mut out = Decoded::default();
    let raw = match first_value(body) {
        Ok(r) => r,
        Err(e) => {
            out.err = Some(e);
            return out;
        }
    };
    let first = raw.first().copied().unwrap_or(b' ');
    if first != b'{' {
        return match serde_json::from_slice::<Value>(raw) {
            Ok(Value::Null) => out,
            Ok(other) => {
                out.err = Some(format!(
                    "json: cannot unmarshal {} into Go value of type {full}",
                    kind_of(&other)
                ));
                out
            }
            Err(e) => {
                out.err = Some(e.to_string());
                out
            }
        };
    }
    // Paires dans l'ordre du document (la dernière l'emporte, comme en Go),
    // valeurs brutes (le texte d'un nombre entre dans le message d'erreur).
    let obj = match serde_json::from_slice::<Pairs>(raw) {
        Ok(p) => p.0,
        Err(e) => {
            out.err = Some(e.to_string());
            return out;
        }
    };
    let fail = |out: &mut Decoded, what: String, field: &str, kind: Kind| {
        if out.err.is_none() {
            out.err = Some(format!(
                "json: cannot unmarshal {what} into Go struct field {ty}.{field} of type {}",
                kind.go_type()
            ));
        }
    };
    for (k, rawv) in obj {
        let Ok(v) = serde_json::from_str::<Value>(rawv.get()) else {
            continue;
        };
        // Correspondance exacte d'abord, puis sans égard à la casse.
        let f = fields
            .iter()
            .find(|(n, _)| *n == k)
            .or_else(|| fields.iter().find(|(n, _)| n.eq_ignore_ascii_case(&k)));
        let Some(&(name, kind)) = f else { continue };
        if v.is_null() {
            if matches!(kind, Kind::OptStr | Kind::OptBool) {
                out.fields.remove(name);
            }
            continue;
        }
        match kind {
            Kind::Str | Kind::OptStr => match v {
                Value::String(_) => {
                    out.fields.insert(name.into(), v);
                }
                other => {
                    fail(&mut out, kind_of(&other).into(), name, Kind::Str);
                    // Un pointeur est alloué avant l'échec : il reste posé.
                    if kind == Kind::OptStr {
                        out.fields
                            .entry(name)
                            .or_insert(Value::String(String::new()));
                    }
                }
            },
            Kind::Bool | Kind::OptBool => match v {
                Value::Bool(_) => {
                    out.fields.insert(name.into(), v);
                }
                other => {
                    fail(&mut out, kind_of(&other).into(), name, Kind::Bool);
                    if kind == Kind::OptBool {
                        out.fields.entry(name).or_insert(Value::Bool(false));
                    }
                }
            },
            Kind::Int => match &v {
                Value::Number(n) => match n.as_i64() {
                    Some(_) => {
                        out.fields.insert(name.into(), v);
                    }
                    None => fail(&mut out, format!("number {}", rawv.get()), name, kind),
                },
                other => fail(&mut out, kind_of(other).into(), name, kind),
            },
            Kind::StrList => match v {
                Value::Array(a) => {
                    let mut list = Vec::with_capacity(a.len());
                    for e in a {
                        match e {
                            Value::String(s) => list.push(Value::String(s)),
                            Value::Null => list.push(Value::String(String::new())),
                            other => {
                                fail(&mut out, kind_of(&other).into(), name, Kind::Str);
                                list.push(Value::String(String::new()));
                            }
                        }
                    }
                    out.fields.insert(name.into(), Value::Array(list));
                }
                other => fail(&mut out, kind_of(&other).into(), name, kind),
            },
            Kind::StrMap => match v {
                Value::Object(m) => {
                    let mut dst = match out.fields.remove(name) {
                        Some(Value::Object(d)) => d,
                        _ => Map::new(),
                    };
                    for (mk, mv) in m {
                        match mv {
                            Value::String(s) => {
                                dst.insert(mk, Value::String(s));
                            }
                            Value::Null => {
                                dst.insert(mk, Value::String(String::new()));
                            }
                            other => {
                                fail(&mut out, kind_of(&other).into(), name, Kind::Str);
                                dst.insert(mk, Value::String(String::new()));
                            }
                        }
                    }
                    out.fields.insert(name.into(), Value::Object(dst));
                }
                other => fail(&mut out, kind_of(&other).into(), name, kind),
            },
            Kind::Any => {
                out.fields.insert(name.into(), v);
            }
        }
    }
    out
}

/// Objet JSON en paires ordonnées, valeurs brutes.
struct Pairs<'a>(Vec<(String, &'a RawValue)>);

impl<'de: 'a, 'a> Deserialize<'de> for Pairs<'a> {
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        struct V<'a>(PhantomData<&'a ()>);
        impl<'de: 'a, 'a> Visitor<'de> for V<'a> {
            type Value = Pairs<'a>;
            fn expecting(&self, f: &mut std::fmt::Formatter) -> std::fmt::Result {
                f.write_str("un objet JSON")
            }
            fn visit_map<A: MapAccess<'de>>(self, mut m: A) -> Result<Pairs<'a>, A::Error> {
                let mut v = Vec::new();
                while let Some((k, val)) = m.next_entry::<String, &'a RawValue>()? {
                    v.push((k, val));
                }
                Ok(Pairs(v))
            }
        }
        d.deserialize_map(V(PhantomData))
    }
}

fn kind_of(v: &Value) -> &'static str {
    match v {
        Value::Null => "null",
        Value::Bool(_) => "bool",
        Value::Number(_) => "number",
        Value::String(_) => "string",
        Value::Array(_) => "array",
        Value::Object(_) => "object",
    }
}

/// `quoteChar` du scanner Go.
fn quote_char(c: u8) -> String {
    match c {
        b'\'' => r"'\''".into(),
        b'"' => "'\"'".into(),
        _ => {
            let ch = c as char; // octet → U+00XX, comme string(byte) en Go
            let inner = match ch {
                '\x07' => r"\a".to_string(),
                '\x08' => r"\b".to_string(),
                '\x0c' => r"\f".to_string(),
                '\n' => r"\n".to_string(),
                '\r' => r"\r".to_string(),
                '\t' => r"\t".to_string(),
                '\x0b' => r"\v".to_string(),
                '\\' => r"\\".to_string(),
                c if (c as u32) < 0x20 || c as u32 == 0x7f => format!(r"\x{:02x}", c as u32),
                c if (0x80..0xa1).contains(&(c as u32)) || c as u32 == 0xad => {
                    format!(r"\u{:04x}", c as u32)
                }
                c => c.to_string(),
            };
            format!("'{inner}'")
        }
    }
}

fn syntax(c: u8, ctx: &str) -> String {
    format!("invalid character {} {ctx}", quote_char(c))
}

fn is_space(c: u8) -> bool {
    matches!(c, b' ' | b'\t' | b'\r' | b'\n')
}

/// Première valeur JSON du corps, validée par le scanner du Go
/// (`Decoder.readValue`) : la tranche de la valeur, ou l'erreur du Go.
pub fn first_value(body: &[u8]) -> Result<&[u8], String> {
    let mut i = 0;
    while i < body.len() && is_space(body[i]) {
        i += 1;
    }
    if i == body.len() {
        return Err("EOF".into());
    }
    let start = i;
    let mut p = Scan {
        b: body,
        i,
        depth: 0,
    };
    p.value()?;
    Ok(&body[start..p.i])
}

struct Scan<'a> {
    b: &'a [u8],
    i: usize,
    depth: usize,
}

const MAX_DEPTH: usize = 10000;

impl Scan<'_> {
    fn peek(&self) -> Option<u8> {
        self.b.get(self.i).copied()
    }

    fn eof() -> String {
        "unexpected EOF".into()
    }

    fn skip_ws(&mut self) {
        while let Some(c) = self.peek() {
            if !is_space(c) {
                break;
            }
            self.i += 1;
        }
    }

    fn value(&mut self) -> Result<(), String> {
        self.skip_ws();
        let c = self.peek().ok_or_else(Self::eof)?;
        match c {
            b'{' => self.object(),
            b'[' => self.array(),
            b'"' => self.string(),
            b'-' | b'0'..=b'9' => self.number(),
            b't' => self.literal(b"true", "true"),
            b'f' => self.literal(b"false", "false"),
            b'n' => self.literal(b"null", "null"),
            _ => Err(syntax(c, "looking for beginning of value")),
        }
    }

    fn push(&mut self) -> Result<(), String> {
        self.depth += 1;
        if self.depth > MAX_DEPTH {
            return Err("exceeded max depth".into());
        }
        Ok(())
    }

    fn object(&mut self) -> Result<(), String> {
        self.push()?;
        self.i += 1; // {
        self.skip_ws();
        match self.peek() {
            None => return Err(Self::eof()),
            Some(b'}') => {
                self.i += 1;
                self.depth -= 1;
                return Ok(());
            }
            _ => {}
        }
        loop {
            self.skip_ws();
            match self.peek() {
                None => return Err(Self::eof()),
                Some(b'"') => self.string()?,
                Some(c) => return Err(syntax(c, "looking for beginning of object key string")),
            }
            self.skip_ws();
            match self.peek() {
                None => return Err(Self::eof()),
                Some(b':') => self.i += 1,
                Some(c) => return Err(syntax(c, "after object key")),
            }
            self.value()?;
            self.skip_ws();
            match self.peek() {
                None => return Err(Self::eof()),
                Some(b',') => self.i += 1,
                Some(b'}') => {
                    self.i += 1;
                    self.depth -= 1;
                    return Ok(());
                }
                Some(c) => return Err(syntax(c, "after object key:value pair")),
            }
        }
    }

    fn array(&mut self) -> Result<(), String> {
        self.push()?;
        self.i += 1; // [
        self.skip_ws();
        match self.peek() {
            None => return Err(Self::eof()),
            Some(b']') => {
                self.i += 1;
                self.depth -= 1;
                return Ok(());
            }
            _ => {}
        }
        loop {
            self.value()?;
            self.skip_ws();
            match self.peek() {
                None => return Err(Self::eof()),
                Some(b',') => self.i += 1,
                Some(b']') => {
                    self.i += 1;
                    self.depth -= 1;
                    return Ok(());
                }
                Some(c) => return Err(syntax(c, "after array element")),
            }
        }
    }

    fn string(&mut self) -> Result<(), String> {
        self.i += 1; // "
        loop {
            let c = self.peek().ok_or_else(Self::eof)?;
            self.i += 1;
            match c {
                b'"' => return Ok(()),
                b'\\' => {
                    let e = self.peek().ok_or_else(Self::eof)?;
                    self.i += 1;
                    match e {
                        b'b' | b'f' | b'n' | b'r' | b't' | b'\\' | b'/' | b'"' => {}
                        b'u' => {
                            for _ in 0..4 {
                                let h = self.peek().ok_or_else(Self::eof)?;
                                if !h.is_ascii_hexdigit() {
                                    return Err(syntax(h, "in \\u hexadecimal character escape"));
                                }
                                self.i += 1;
                            }
                        }
                        _ => return Err(syntax(e, "in string escape code")),
                    }
                }
                c if c < 0x20 => return Err(syntax(c, "in string literal")),
                _ => {}
            }
        }
    }

    fn digits(&mut self) {
        while matches!(self.peek(), Some(b'0'..=b'9')) {
            self.i += 1;
        }
    }

    fn number(&mut self) -> Result<(), String> {
        if self.peek() == Some(b'-') {
            self.i += 1;
            match self.peek() {
                None => return Err(Self::eof()),
                Some(b'0'..=b'9') => {}
                Some(c) => return Err(syntax(c, "in numeric literal")),
            }
        }
        if self.peek() == Some(b'0') {
            self.i += 1;
        } else {
            self.digits();
        }
        if self.peek() == Some(b'.') {
            self.i += 1;
            match self.peek() {
                // Valeur de tête tronquée : le Go la complète d'un espace,
                // qui échoue ici.
                None => return Err(Self::eof()),
                Some(b'0'..=b'9') => self.digits(),
                Some(c) => return Err(syntax(c, "after decimal point in numeric literal")),
            }
        }
        if matches!(self.peek(), Some(b'e' | b'E')) {
            self.i += 1;
            if matches!(self.peek(), Some(b'+' | b'-')) {
                self.i += 1;
            }
            match self.peek() {
                None => return Err(Self::eof()),
                Some(b'0'..=b'9') => self.digits(),
                Some(c) => return Err(syntax(c, "in exponent of numeric literal")),
            }
        }
        Ok(())
    }

    fn literal(&mut self, word: &[u8], name: &str) -> Result<(), String> {
        self.i += 1;
        for &want in &word[1..] {
            match self.peek() {
                None => return Err(Self::eof()),
                Some(c) if c == want => self.i += 1,
                Some(c) => {
                    return Err(syntax(
                        c,
                        &format!("in literal {name} (expecting {})", quote_char(want)),
                    ));
                }
            }
        }
        Ok(())
    }
}

/// `json.Marshal` du Go : comme serde_json, avec l'échappement HTML du Go
/// (`<`, `>`, `&`, U+2028, U+2029), pour écrire en base les mêmes octets.
pub fn marshal<T: serde::Serialize + ?Sized>(v: &T) -> String {
    let s = serde_json::to_string(v).unwrap_or_default();
    if !s.contains(['<', '>', '&', '\u{2028}', '\u{2029}']) {
        return s;
    }
    let mut out = String::with_capacity(s.len() + 16);
    for c in s.chars() {
        match c {
            '<' => out.push_str("\\u003c"),
            '>' => out.push_str("\\u003e"),
            '&' => out.push_str("\\u0026"),
            '\u{2028}' => out.push_str("\\u2028"),
            '\u{2029}' => out.push_str("\\u2029"),
            c => out.push(c),
        }
    }
    out
}

/// `unicode.IsPrint` (approché : catégories de contrôle, de format, privées,
/// séparateurs autres que l'espace ASCII → non imprimables).
pub fn is_print(c: char) -> bool {
    let u = c as u32;
    if c == ' ' {
        return true;
    }
    if c.is_control() || c.is_whitespace() {
        return false;
    }
    !matches!(u,
        0xAD | 0x600..=0x605 | 0x61C | 0x6DD | 0x70F | 0x890..=0x891 | 0x8E2 | 0x180E
        | 0x200B..=0x200F | 0x202A..=0x202E | 0x2060..=0x2064 | 0x2066..=0x206F
        | 0xFEFF | 0xFFF9..=0xFFFB | 0xE000..=0xF8FF | 0xFFFE | 0xFFFF
        | 0x110BD | 0x110CD | 0x13430..=0x1343F | 0x1BCA0..=0x1BCA3
        | 0x1D173..=0x1D17A | 0xE0001 | 0xE0020..=0xE007F
        | 0xF0000..=0x10FFFF)
}

/// `strconv.Quote` du Go (`%q` des messages d'erreur).
pub fn quote(s: &str) -> String {
    let mut out = String::with_capacity(s.len() + 2);
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\x07' => out.push_str("\\a"),
            '\x08' => out.push_str("\\b"),
            '\x0c' => out.push_str("\\f"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '\x0b' => out.push_str("\\v"),
            c if is_print(c) => out.push(c),
            c if (c as u32) < 0x80 => out.push_str(&format!("\\x{:02x}", c as u32)),
            c if (c as u32) < 0x10000 => out.push_str(&format!("\\u{:04x}", c as u32)),
            c => out.push_str(&format!("\\U{:08x}", c as u32)),
        }
    }
    out.push('"');
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    const T: &[(&str, Kind)] = &[("name", Kind::Str), ("on", Kind::Bool)];

    #[test]
    fn erreurs_du_scanner() {
        let e = |b: &str| decode_struct(b.as_bytes(), "t", T).err.unwrap_or_default();
        assert_eq!(e(""), "EOF");
        assert_eq!(e("  \n"), "EOF");
        assert_eq!(
            e("pas du json"),
            "invalid character 'p' looking for beginning of value"
        );
        assert_eq!(e("{\"name\""), "unexpected EOF");
        assert_eq!(e("{\"name\" 1}"), "invalid character '1' after object key");
        assert_eq!(
            e("{'a':1}"),
            r"invalid character '\'' looking for beginning of object key string"
        );
        assert_eq!(e("tru"), "unexpected EOF");
        assert_eq!(
            e("trux"),
            "invalid character 'x' in literal true (expecting 'e')"
        );
    }

    #[test]
    fn premiere_valeur_seulement() {
        let d = decode_struct(br#"{"name":"a"} et la suite"#, "t", T);
        assert_eq!(d.err, None);
        assert_eq!(d.str("name"), "a");
    }

    #[test]
    fn erreurs_de_type_et_casse() {
        let d = decode_struct(br#"{"NAME":"x","on":"oui"}"#, "saveReq", T);
        assert_eq!(d.str("name"), "x");
        assert_eq!(
            d.err.as_deref(),
            Some("json: cannot unmarshal string into Go struct field saveReq.on of type bool")
        );
        let d = decode_struct(b"[1]", "saveReq", T);
        assert_eq!(
            d.err.as_deref(),
            Some("json: cannot unmarshal array into Go value of type loki.saveReq")
        );
        assert!(decode_struct(b"null", "t", T).err.is_none());
    }
}
