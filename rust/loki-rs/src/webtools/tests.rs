use super::*;

#[test]
fn md5_rfc1321() {
    for (input, want) in [
        ("", "d41d8cd98f00b204e9800998ecf8427e"),
        ("a", "0cc175b9c0f1b6a831c399e269772661"),
        ("abc", "900150983cd24fb0d6963f7d28e17f72"),
        ("message digest", "f96b697d7cb7938d525a2f31aaf161d0"),
        (
            "12345678901234567890123456789012345678901234567890123456789012345678901234567890",
            "57edf4a22be3c955ac49da2e2107b67a",
        ),
    ] {
        assert_eq!(hex::encode(md5(input.as_bytes())), want, "{input}");
    }
}

#[test]
fn plafond_de_sortie() {
    let s = "é".repeat(MAX_OUTPUT);
    assert_eq!(cap_output(&s), s);
    let long = format!("{s}x");
    let out = cap_output(&long);
    assert!(out.starts_with(&s));
    assert!(out.ends_with("ou cible avec web_grep.]"));
    assert!(!out.contains('x'));
}

#[test]
fn cles_du_cache() {
    let base = FetchOpts {
        dismiss_popups: true,
        ..Default::default()
    };
    assert_eq!(cache_key_for("u", &base), "u");
    let o = FetchOpts {
        dismiss_popups: false,
        ..Default::default()
    };
    // Valeurs tirées de cacheKeyFor du Go.
    assert_eq!(cache_key_for("u", &o), "u#77fc370a");
    let o = FetchOpts {
        dismiss_popups: true,
        actions: vec!["a < b && c".into()],
        ..Default::default()
    };
    assert_eq!(cache_key_for("u", &o), "u#d1533b44");
}

#[test]
fn urls() {
    for (raw, want) in [
        (
            "https://github.com/a/b",
            "https://raw.githubusercontent.com/a/b/HEAD/README.md",
        ),
        (
            "//github.com/a/b/",
            "https://raw.githubusercontent.com/a/b/HEAD/README.md",
        ),
        (
            "https://github.com/a%20x/b?tab=1#f",
            "https://raw.githubusercontent.com/a x/b/HEAD/README.md",
        ),
        ("https://GitHub.com/a/b", "https://GitHub.com/a/b"),
        ("https://github.com:443/a/b", "https://github.com:443/a/b"),
        ("https://github.com/a", "https://github.com/a"),
        ("https://github.com/a/b/c", "https://github.com/a/b/c"),
        ("https://github.com/%zz/b", "https://github.com/%zz/b"),
        ("github.com/a/b", "github.com/a/b"),
        (":github.com/a/b", ":github.com/a/b"),
    ] {
        assert_eq!(normalize_crawl_url(raw), want, "{raw}");
    }
    for (raw, want) in [
        (
            "//duckduckgo.com/l/?uddg=https%3A%2F%2Fa.example%2F%3Fx%3D1%26y%3D2&rut=z",
            "https://a.example/?x=1&y=2",
        ),
        ("https://b.example/", "https://b.example/"),
        (
            "//duckduckgo.com/l/?uddg=%ZZ",
            "https://duckduckgo.com/l/?uddg=%ZZ",
        ),
        ("//duckduckgo.com/l/?x;y=1&uddg=a%2Bb+c", "a b c"),
        ("/l/?uddg=", "/l/?uddg="),
    ] {
        assert_eq!(decode_uddg(raw), want, "{raw}");
    }
}

#[test]
fn entites() {
    assert_eq!(
        decode_html_entities(
            "a &amp;lt; &#233;t&#233; &#0; &#4294967361; &#99999999999999999999; &quot;"
        ),
        "a &lt; été &#0; A &#99999999999999999999; \""
    );
}

#[test]
fn nombres_du_go() {
    assert_eq!(go_int(3.9), 3);
    assert_eq!(go_int(-3.9), -3);
    if cfg!(target_arch = "x86_64") {
        assert_eq!(go_int(1e300), i64::MIN);
        assert_eq!(go_int(f64::NAN), i64::MIN);
    }
    assert_eq!(format_bytes(1023), "1023 B");
    assert_eq!(format_bytes(1536), "1.5 KB");
    assert_eq!(format_bytes(3 * 1024 * 1024 + 1), "3.00 MB");
    assert_eq!(tail_runes("abcdé", 2), "dé");
    assert_eq!(tail_runes("ab", 5), "ab");
}

#[test]
fn erreurs_de_regex() {
    for (expr, want) in [
        ("(?i)(", "missing closing ): `(?i)(`"),
        ("(?i)a)", "unexpected ): `(?i)a)`"),
        ("(?i)[abc", "missing closing ]: `[abc`"),
        ("(?i)[[:alpha:]", "missing closing ]: `[[:alpha:]`"),
        ("(?i)*x", "missing argument to repetition operator: `*`"),
        ("(?i)a|+", "missing argument to repetition operator: `+`"),
        ("(?i)a**", "invalid nested repetition operator: `**`"),
        ("(?i)a*??", "invalid nested repetition operator: `*??`"),
        ("(?i)\\q", "invalid escape sequence: `\\q`"),
        ("(?i)\\1", "invalid escape sequence: `\\1`"),
        ("(?i)a\\", "trailing backslash at end of expression: ``"),
    ] {
        assert_eq!(
            go_regexp_error(expr),
            format!("error parsing regexp: {want}"),
            "{expr}"
        );
    }
}

#[test]
fn reponses_de_crawl4ai() {
    let md = |raw: &str, pr: bool| crawl_markdown(raw.as_bytes(), pr);
    assert_eq!(
        md(
            r#"{"results":[{"success":true,"markdown":{"raw_markdown":"r","fit_markdown":"f"}}]}"#,
            true
        ),
        Ok("r".into())
    );
    assert_eq!(
        md(
            r#"{"results":[{"success":true,"markdown":{"raw_markdown":"","fit_markdown":"f"}}]}"#,
            true
        ),
        Ok("f".into())
    );
    assert_eq!(
        md(
            r#"{"results":[{"success":true,"markdown":{"raw_markdown":"r"}}]}"#,
            false
        ),
        Ok("r".into())
    );
    assert_eq!(
        md(r#"{"Results":[{"Success":true,"Markdown":"x"}]}"#, false),
        Ok("x".into())
    );
    assert_eq!(md("null", false), Err("Crawl4AI : échec du crawl".into()));
    assert_eq!(md("5", false), Err("Crawl4AI : réponse vide".into()));
    assert_eq!(
        md(r#"{"results":[{"success":"x"}],"success":"y"}"#, false),
        Err("Crawl4AI : échec du crawl".into())
    );
    assert_eq!(
        md(r#"{"results":[null,{"success":true}]}"#, false),
        Err("Crawl4AI : échec du crawl".into())
    );
    assert_eq!(
        md(r#"{"results":[{"success":true,"markdown":null}]}"#, false),
        Ok(String::new())
    );
}

#[test]
fn lignes_normalisees() {
    assert_eq!(
        normalize_lines("\n\n a \r\nb  \n\n\n\nb\nb\nc\t\n\n"),
        [" a", "b", "", "b", "c"]
    );
    assert!(normalize_lines(" \n\t\n").is_empty());
}
