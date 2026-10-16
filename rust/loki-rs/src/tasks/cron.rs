//! Planification des tâches, résolution à la minute — port de
//! `tasks_cron.go`. Quatre formes :
//!
//! - « @every 30m », « @every 2h » : intervalle glissant (`time.ParseDuration`
//!   du Go, borné à ≥ 1 minute) ;
//! - « @every 1d@23:00 » : tous les N jours, ancré à une heure murale ;
//! - « @once 2026-10-01 16:54 » : une seule fois ;
//! - expression cron 5 champs (*, a, a-b, a,b, */n, a-b/n).
//!
//! Les instants sont des nanosecondes Unix sur 128 bits (une date « @once »
//! peut dépasser 2262, que le Go accepte) ; l'heure murale est celle du
//! fuseau de l'utilisateur ([`crate::tz`], la zoneinfo du système comme le
//! Go).

use std::collections::BTreeSet;

use crate::gojson::quote;
use crate::tz::{Civil, Location};

const NS: i64 = 1_000_000_000;
const MINUTE: i64 = 60 * NS;
const HOUR: i64 = 60 * MINUTE;
const NS128: i128 = NS as i128;

/// `loc` : fuseau IANA, vide ou introuvable → heure locale du serveur.
pub fn loc(tz: &str) -> Location {
    loc_or(tz, Location::local)
}

/// `loc`, l'heure locale donnée.
fn loc_or(tz: &str, local: fn() -> Location) -> Location {
    if tz.is_empty() {
        return local();
    }
    Location::load(tz).unwrap_or_else(|_| local())
}

fn civil_of(l: &Location, ns: i128) -> Civil {
    l.civil(ns.div_euclid(NS128) as i64)
}

/// `cutPrefix` : préfixe insensible à la casse.
pub fn cut_prefix<'a>(s: &'a str, prefix: &str) -> Option<&'a str> {
    if s.len() >= prefix.len()
        && s.is_char_boundary(prefix.len())
        && s[..prefix.len()].eq_ignore_ascii_case(prefix)
    {
        Some(&s[prefix.len()..])
    } else {
        None
    }
}

/// `isOnce`.
pub fn is_once(schedule: &str) -> bool {
    cut_prefix(schedule.trim(), "@once").is_some()
}

/// `nextAfter` : le prochain déclenchement (ns), strictement après `from`
/// (ns).
pub fn next_after(schedule: &str, tz: &str, from: i64) -> Result<i128, String> {
    let from = i128::from(from);
    let s = schedule.trim();
    if s.is_empty() {
        return Err("fréquence vide".into());
    }
    if let Some(rest) = cut_prefix(s, "@once") {
        let l = loc(tz);
        let Some(at) = parse_ymd_hm(rest.trim(), &l) else {
            return Err("date invalide (attendu « @once AAAA-MM-JJ HH:MM »)".into());
        };
        if at <= from {
            return Err(format!(
                "{} est déjà passé (il est {})",
                civil_of(&l, at).ymd_hm(),
                civil_of(&l, from).ymd_hm()
            ));
        }
        return Ok(at);
    }
    if let Some(rest) = cut_prefix(s, "@every") {
        return next_every(rest.trim(), tz, from);
    }
    next_cron(s, tz, from)
}

/// `time.ParseInLocation("2006-01-02 15:04", s, l)` : l'instant (ns), ou
/// rien si la chaîne ne suit pas la forme.
pub fn parse_ymd_hm(s: &str, l: &Location) -> Option<i128> {
    let b = s.as_bytes();
    let digits = |r: &[u8]| r.iter().all(u8::is_ascii_digit);
    // 2006 : 4 chiffres ; -01 et -02 : 2 chiffres ; 15 : 1 ou 2 ; :04 : 2.
    if b.len() < 4 + 1 + 2 + 1 + 2 + 1 + 1 + 1 + 2 || !digits(&b[..4]) {
        return None;
    }
    let year: i64 = s[..4].parse().ok()?;
    let rest = s[4..].strip_prefix('-')?;
    if rest.len() < 2 || !digits(&rest.as_bytes()[..2]) {
        return None;
    }
    let month: i64 = rest[..2].parse().ok()?;
    let rest = rest[2..].strip_prefix('-')?;
    if rest.len() < 2 || !digits(&rest.as_bytes()[..2]) {
        return None;
    }
    let day: i64 = rest[..2].parse().ok()?;
    let rest = rest[2..].strip_prefix(' ')?;
    let hl = rest.bytes().take(2).take_while(u8::is_ascii_digit).count();
    if hl == 0 {
        return None;
    }
    let hour: i64 = rest[..hl].parse().ok()?;
    let rest = rest[hl..].strip_prefix(':')?;
    if rest.len() != 2 || !digits(rest.as_bytes()) {
        return None;
    }
    let minute: i64 = rest.parse().ok()?;
    if !(1..=12).contains(&month) || hour > 23 || minute > 59 {
        return None;
    }
    let dim = days_in(month, year);
    if day < 1 || day > dim {
        return None;
    }
    Some(i128::from(l.date(year, month, day, hour, minute, 0)) * NS128)
}

fn days_in(m: i64, y: i64) -> i64 {
    match m {
        2 => {
            if y % 4 == 0 && (y % 100 != 0 || y % 400 == 0) {
                29
            } else {
                28
            }
        }
        4 | 6 | 9 | 11 => 30,
        _ => 31,
    }
}

/// `nextEvery`.
fn next_every(spec: &str, tz: &str, from: i128) -> Result<i128, String> {
    let (dur_part, time_part) = match spec.split_once('@') {
        Some((d, t)) => (d.trim(), Some(t)),
        None => (spec.trim(), None),
    };
    if let Some(n) = dur_part.strip_suffix('d') {
        let days = n
            .parse::<i64>()
            .ok()
            .filter(|d| *d >= 1)
            .ok_or_else(|| format!("nombre de jours invalide : {}", quote(dur_part)))?;
        let Some(tp) = time_part else {
            // time.Duration(days) * 24 * time.Hour : déborde comme le Go.
            return Ok(from + i128::from(days.wrapping_mul(24).wrapping_mul(HOUR)));
        };
        let (hh, mm) = parse_hhmm(tp)?;
        let l = loc(tz);
        let now = civil_of(&l, from);
        let mut cand = l.date(now.year, now.month, now.day, hh, mm, 0);
        // cand.AddDate(0, 0, days) : l'heure murale DU CANDIDAT, jour après
        // jour (une heure qui saute au printemps reste décalée ensuite).
        while i128::from(cand) * NS128 <= from {
            let c = l.civil(cand);
            cand = l.date(c.year, c.month, c.day + days, c.hour, c.minute, c.second);
        }
        return Ok(i128::from(cand) * NS128);
    }
    let d = parse_duration(dur_part)
        .map_err(|e| format!("intervalle invalide (ex. « 2h », « 30m ») : {e}"))?;
    if d < MINUTE {
        return Err("intervalle minimum : 1 minute".into());
    }
    Ok(from + i128::from(d))
}

/// `parseHHMM`.
pub fn parse_hhmm(s: &str) -> Result<(i64, i64), String> {
    let Some((hs, ms)) = s.trim().split_once(':') else {
        return Err(format!("heure invalide (attendu HH:MM) : {}", quote(s)));
    };
    match (go_atoi(hs.trim()), go_atoi(ms.trim())) {
        (Some(h), Some(m)) if (0..=23).contains(&h) && (0..=59).contains(&m) => Ok((h, m)),
        _ => Err(format!("heure hors bornes : {}", quote(s))),
    }
}

/// `strconv.Atoi` (signe accepté).
fn go_atoi(s: &str) -> Option<i64> {
    let digits = s.strip_prefix(['+', '-']).unwrap_or(s);
    if digits.is_empty() || !digits.bytes().all(|c| c.is_ascii_digit()) {
        return None;
    }
    s.parse().ok()
}

/// `quote` du paquet time (messages de `ParseDuration`).
fn time_quote(s: &str) -> String {
    let mut out = String::from("\"");
    for c in s.chars() {
        if (c as u32) >= 0x80 || c < ' ' {
            let mut buf = [0u8; 4];
            for byte in c.encode_utf8(&mut buf).bytes() {
                out.push_str(&format!("\\x{byte:02x}"));
            }
        } else {
            if c == '"' || c == '\\' {
                out.push('\\');
            }
            out.push(c);
        }
    }
    out.push('"');
    out
}

/// `time.ParseDuration` du Go (nanosecondes).
pub fn parse_duration(orig: &str) -> Result<i64, String> {
    let invalid = || format!("time: invalid duration {}", time_quote(orig));
    let mut s = orig.as_bytes();
    let mut neg = false;
    if let Some(&c) = s.first()
        && (c == b'-' || c == b'+')
    {
        neg = c == b'-';
        s = &s[1..];
    }
    if s == b"0" {
        return Ok(0);
    }
    if s.is_empty() {
        return Err(invalid());
    }
    let mut d: u64 = 0;
    while !s.is_empty() {
        if !(s[0] == b'.' || s[0].is_ascii_digit()) {
            return Err(invalid());
        }
        // [0-9]*
        let pl = s.len();
        let mut v: u64 = 0;
        let mut i = 0;
        while i < s.len() && s[i].is_ascii_digit() {
            if v > (1u64 << 63) / 10 {
                return Err(invalid());
            }
            v = v * 10 + (s[i] - b'0') as u64;
            if v > 1u64 << 63 {
                return Err(invalid());
            }
            i += 1;
        }
        s = &s[i..];
        let pre = pl != s.len();
        // (\.[0-9]*)?
        let mut post = false;
        let (mut f, mut scale) = (0u64, 1f64);
        if !s.is_empty() && s[0] == b'.' {
            s = &s[1..];
            let pl = s.len();
            let mut overflow = false;
            let mut i = 0;
            while i < s.len() && s[i].is_ascii_digit() {
                if !overflow {
                    if f > ((1u64 << 63) - 1) / 10 {
                        overflow = true;
                    } else {
                        let y = f * 10 + (s[i] - b'0') as u64;
                        if y > 1u64 << 63 {
                            overflow = true;
                        } else {
                            f = y;
                            scale *= 10.0;
                        }
                    }
                }
                i += 1;
            }
            s = &s[i..];
            post = pl != s.len();
        }
        if !pre && !post {
            return Err(invalid());
        }
        // L'unité.
        let mut i = 0;
        while i < s.len() && !(s[i] == b'.' || s[i].is_ascii_digit()) {
            i += 1;
        }
        if i == 0 {
            return Err(format!(
                "time: missing unit in duration {}",
                time_quote(orig)
            ));
        }
        let u = String::from_utf8_lossy(&s[..i]).into_owned();
        s = &s[i..];
        let unit: u64 = match u.as_str() {
            "ns" => 1,
            "us" | "\u{b5}s" | "\u{3bc}s" => 1_000,
            "ms" => 1_000_000,
            "s" => NS as u64,
            "m" => MINUTE as u64,
            "h" => HOUR as u64,
            _ => {
                return Err(format!(
                    "time: unknown unit {} in duration {}",
                    time_quote(&u),
                    time_quote(orig)
                ));
            }
        };
        if v > (1u64 << 63) / unit {
            return Err(invalid());
        }
        v *= unit;
        if f > 0 {
            v += (f as f64 * (unit as f64 / scale)) as u64;
            if v > 1u64 << 63 {
                return Err(invalid());
            }
        }
        d += v;
        if d > 1u64 << 63 {
            return Err(invalid());
        }
    }
    if neg {
        return Ok((d as i128).wrapping_neg() as i64);
    }
    if d > (1u64 << 63) - 1 {
        return Err(invalid());
    }
    Ok(d as i64)
}

/// `cronSpec`.
pub struct CronSpec {
    min: BTreeSet<i64>,
    hour: BTreeSet<i64>,
    dom: BTreeSet<i64>,
    month: BTreeSet<i64>,
    dow: BTreeSet<i64>,
}

impl CronSpec {
    fn matches(&self, c: &Civil) -> bool {
        self.min.contains(&c.minute)
            && self.hour.contains(&c.hour)
            && self.dom.contains(&c.day)
            && self.month.contains(&c.month)
            && self.dow.contains(&c.weekday)
    }
}

/// `nextCron` : le prochain match à la minute, sur ~366 jours au plus.
fn next_cron(expr: &str, tz: &str, from: i128) -> Result<i128, String> {
    let spec = parse_cron(expr)?;
    let l = loc(tz);
    let minute = i128::from(MINUTE);
    // Truncate(time.Minute) se fait sur l'instant absolu.
    let mut t = from.div_euclid(minute) * minute + minute;
    let limit = t + 366 * 24 * i128::from(HOUR);
    while t < limit {
        if spec.matches(&civil_of(&l, t)) {
            return Ok(t);
        }
        t += minute;
    }
    Err(format!("fréquence jamais satisfaite : {}", quote(expr)))
}

/// `parseCron`.
pub fn parse_cron(expr: &str) -> Result<CronSpec, String> {
    let f: Vec<&str> = expr.split_whitespace().collect();
    if f.len() != 5 {
        return Err(format!(
            "cron : 5 champs attendus « min heure jour mois jour-semaine », reçu {}",
            f.len()
        ));
    }
    let field = |i: usize, lo, hi, name: &str| {
        parse_field(f[i], lo, hi).map_err(|e| format!("champ {name} : {e}"))
    };
    let min = field(0, 0, 59, "minute")?;
    let hour = field(1, 0, 23, "heure")?;
    let dom = field(2, 1, 31, "jour-du-mois")?;
    let month = field(3, 1, 12, "mois")?;
    let mut dow = field(4, 0, 7, "jour-semaine")?;
    if dow.contains(&7) {
        dow.insert(0);
    }
    Ok(CronSpec {
        min,
        hour,
        dom,
        month,
        dow,
    })
}

/// `parseField`.
fn parse_field(field: &str, lo: i64, hi: i64) -> Result<BTreeSet<i64>, String> {
    let mut out = BTreeSet::new();
    for part in field.split(',') {
        let mut rng = part;
        let mut step = 1;
        if let Some((base, st)) = part.split_once('/') {
            step = go_atoi(st)
                .filter(|n| *n >= 1)
                .ok_or_else(|| format!("pas invalide : {}", quote(part)))?;
            rng = base;
        }
        let (mut start, mut end) = (lo, hi);
        if rng != "*" {
            if let Some((a, b)) = rng.split_once('-') {
                start = atoi_range(a, lo, hi)?;
                end = atoi_range(b, lo, hi)?;
            } else {
                let v = atoi_range(rng, lo, hi)?;
                start = v;
                end = v;
            }
        }
        if start > end {
            return Err(format!("plage inversée : {}", quote(part)));
        }
        let mut v = start;
        while v <= end {
            out.insert(v);
            v += step;
        }
    }
    if out.is_empty() {
        return Err("champ vide".into());
    }
    Ok(out)
}

/// `atoiRange`.
fn atoi_range(s: &str, lo: i64, hi: i64) -> Result<i64, String> {
    let v = go_atoi(s.trim()).ok_or_else(|| format!("valeur invalide : {}", quote(s)))?;
    if v < lo || v > hi {
        return Err(format!("valeur {v} hors bornes [{lo}-{hi}]"));
    }
    Ok(v)
}

/// `computeNextRun` : le prochain déclenchement en ms Unix, 0 si la fréquence
/// est invalide.
pub fn compute_next_run(schedule: &str, tz: &str, from_ns: i64) -> i64 {
    next_after(schedule, tz, from_ns).map_or(0, |t| t.div_euclid(1_000_000) as i64)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn durees_du_go() {
        assert_eq!(parse_duration("2h"), Ok(2 * HOUR));
        assert_eq!(parse_duration("1h30m"), Ok(90 * MINUTE));
        assert_eq!(parse_duration("1.5h"), Ok(90 * MINUTE));
        assert_eq!(parse_duration(".5m"), Ok(30 * NS));
        assert_eq!(parse_duration("0"), Ok(0));
        assert_eq!(parse_duration("-2m"), Ok(-2 * MINUTE));
        assert_eq!(parse_duration("300ms"), Ok(300_000_000));
        assert_eq!(parse_duration("1µs"), Ok(1_000));
        assert_eq!(
            parse_duration(""),
            Err("time: invalid duration \"\"".into())
        );
        assert_eq!(
            parse_duration("2"),
            Err("time: missing unit in duration \"2\"".into())
        );
        assert_eq!(
            parse_duration("2d"),
            Err("time: unknown unit \"d\" in duration \"2d\"".into())
        );
        assert_eq!(
            parse_duration("abc"),
            Err("time: invalid duration \"abc\"".into())
        );
        assert_eq!(
            parse_duration("é"),
            Err("time: invalid duration \"\\xc3\\xa9\"".into())
        );
        assert_eq!(
            parse_duration("9999999999h"),
            Err("time: invalid duration \"9999999999h\"".into())
        );
        for big in ["18446744073709551619h", "9223372036854775808ns"] {
            assert_eq!(
                parse_duration(big),
                Err(format!("time: invalid duration \"{big}\"")),
                "{big}"
            );
        }
        assert_eq!(parse_duration("9223372036854775807ns"), Ok(i64::MAX));
    }

    #[test]
    fn fuseau_de_la_tache() {
        fn paris() -> Location {
            Location::load("Europe/Paris").unwrap()
        }
        // Vide ou introuvable : l'heure locale ; connu : le sien.
        assert_eq!(loc_or("", paris).offset_at(1_792_134_000), 7200);
        assert_eq!(loc_or("Nulle/Part", paris).offset_at(1_792_134_000), 7200);
        assert_eq!(loc_or("UTC", paris).offset_at(1_792_134_000), 0);
        assert_eq!(
            loc_or("America/New_York", paris).offset_at(1_792_134_000),
            -4 * 3600
        );
    }

    /// Valeurs tirées du Go (`nextAfter`).
    #[test]
    fn bornes_du_calendrier_et_heure_murale() {
        let u = Location::utc();
        let from = u.date(2026, 3, 28, 10, 0, 0) * NS;
        let at = |s: &str, tz: &str, from: i64| next_after(s, tz, from).map(|n| (n / NS128) as i64);
        let invalid = Err("date invalide (attendu « @once AAAA-MM-JJ HH:MM »)".to_string());
        assert_eq!(at("@once 2100-02-29 10:00", "UTC", from), invalid);
        assert_eq!(
            at("@once 2400-02-29 10:00", "UTC", from),
            Ok(13_574_599_200)
        );
        assert_eq!(at("@once 2028-02-29 10:00", "UTC", from), Ok(1_835_431_200));
        assert_eq!(at("@once 2026-09-31 10:00", "UTC", from), invalid);
        assert_eq!(at("@once 2026-11-31 10:00", "UTC", from), invalid);
        // À l'heure pile : le lendemain.
        let pile = u.date(2026, 3, 28, 23, 0, 0) * NS;
        assert_eq!(at("@every 1d@23:00", "UTC", pile), Ok(1_774_825_200));
        // Le changement d'heure de la nuit : 9 h murales, pas 24 h plus tard.
        let paris = Location::load("Europe/Paris").unwrap();
        let matin = paris.date(2026, 3, 28, 10, 0, 0) * NS;
        assert_eq!(
            at("@every 1d@09:00", "Europe/Paris", matin),
            Ok(1_774_767_600)
        );
        // L'heure qui saute (2 h 30 → 3 h 30) : AddDate repart de 3 h 30.
        let apres = paris.date(2026, 3, 29, 4, 0, 0) * NS;
        assert_eq!(
            at("@every 1d@02:30", "Europe/Paris", apres),
            Ok(1_774_834_200)
        );
        let avant = paris.date(2026, 3, 27, 4, 0, 0) * NS;
        assert_eq!(
            at("@every 2d@02:30", "Europe/Paris", avant),
            Ok(1_774_747_800)
        );
        // Débordements : la durée du Go, et des dates très lointaines.
        assert_eq!(
            compute_next_run("@every 200000d", "UTC", from),
            607_947_926_290
        );
        assert_eq!(
            compute_next_run("@every 106751d", "UTC", from),
            10_997_978_400_000
        );
        assert_eq!(
            compute_next_run("@once 9999-12-31 23:59", "UTC", from),
            253_402_300_740_000
        );
        assert_eq!(
            compute_next_run("@every 99999999d@10:00", "UTC", from),
            8_641_774_605_600_000
        );
        // strconv.Atoi accepte un signe.
        assert_eq!(at("@every 1d@+9:30", "UTC", from), Ok(1_774_776_600));
        assert_eq!(at("@every 1d@-0:30", "UTC", from), Ok(1_774_744_200));
    }

    #[test]
    fn cron_et_erreurs() {
        let err = |e: &str| parse_cron(e).err().unwrap_or_default();
        assert_eq!(
            err("* * *"),
            "cron : 5 champs attendus « min heure jour mois jour-semaine », reçu 3"
        );
        assert_eq!(
            err("60 * * * *"),
            "champ minute : valeur 60 hors bornes [0-59]"
        );
        assert_eq!(err("* x * * *"), "champ heure : valeur invalide : \"x\"");
        assert_eq!(
            err("* * 5-1 * *"),
            "champ jour-du-mois : plage inversée : \"5-1\""
        );
        assert_eq!(err("* * * */0 *"), "champ mois : pas invalide : \"*/0\"");
        assert_eq!(
            err("* * 5-4 * *"),
            "champ jour-du-mois : plage inversée : \"5-4\""
        );
        assert_eq!(
            err("0 9 0 * *"),
            "champ jour-du-mois : valeur 0 hors bornes [1-31]"
        );
        assert_eq!(err("-1 9 * * *"), "champ minute : valeur invalide : \"\"");
        assert!(parse_cron("0 9 * * 1-5").is_ok());
        let s = parse_cron("0 0 * * 7").unwrap();
        assert!(s.dow.contains(&0));
    }

    #[test]
    fn prochains_declenchements() {
        let u = Location::utc();
        let from = u.date(2026, 10, 15, 21, 10, 30) * NS;
        let ns = |t: i64| i128::from(t) * NS128;
        let from128 = i128::from(from);
        assert_eq!(
            next_after("@every 2h", "UTC", from),
            Ok(from128 + i128::from(2 * HOUR))
        );
        assert_eq!(
            next_after("@every 30s", "UTC", from),
            Err("intervalle minimum : 1 minute".into())
        );
        assert_eq!(
            next_after("@EVERY 1d", "UTC", from),
            Ok(from128 + i128::from(24 * HOUR))
        );
        assert_eq!(
            next_after("@every 1d@23:00", "UTC", from),
            Ok(ns(u.date(2026, 10, 15, 23, 0, 0)))
        );
        assert_eq!(
            next_after("@every 2d@09:00", "UTC", from),
            Ok(ns(u.date(2026, 10, 17, 9, 0, 0)))
        );
        assert_eq!(
            next_after("@every 0d", "UTC", from),
            Err("nombre de jours invalide : \"0d\"".into())
        );
        assert_eq!(
            next_after("@every 1d@25:00", "UTC", from),
            Err("heure hors bornes : \"25:00\"".into())
        );
        assert_eq!(
            next_after("@every 1d@9", "UTC", from),
            Err("heure invalide (attendu HH:MM) : \"9\"".into())
        );
        assert_eq!(
            next_after("0 9 * * 1-5", "UTC", from),
            Ok(ns(u.date(2026, 10, 16, 9, 0, 0)))
        );
        assert_eq!(
            next_after("@once 2026-10-16 9:05", "UTC", from),
            Ok(ns(u.date(2026, 10, 16, 9, 5, 0)))
        );
        assert_eq!(
            next_after("@once 2026-10-01 16:54", "UTC", from),
            Err("2026-10-01 16:54 est déjà passé (il est 2026-10-15 21:10)".into())
        );
        for bad in [
            "@once 2026-02-30 10:00",
            "@once 2026-10-16 24:00",
            "@once 2026-10-16 23:60",
            "@once 2026-13-01 10:00",
            "@once 26-10-16 10:00",
            "@once 2026-10-16 10:5",
        ] {
            assert_eq!(
                next_after(bad, "UTC", from),
                Err("date invalide (attendu « @once AAAA-MM-JJ HH:MM »)".into()),
                "{bad}"
            );
        }
        assert_eq!(next_after("  ", "UTC", from), Err("fréquence vide".into()));
        // Le prochain 1er janvier, à 78 jours ; le 14 octobre, à 364.
        assert_eq!(
            next_after("0 0 1 1 *", "UTC", from),
            Ok(ns(u.date(2027, 1, 1, 0, 0, 0)))
        );
        assert_eq!(
            next_after("0 0 14 10 *", "UTC", from),
            Ok(ns(u.date(2027, 10, 14, 0, 0, 0)))
        );
        assert_eq!(
            next_after("0 0 31 2 *", "UTC", from),
            Err("fréquence jamais satisfaite : \"0 0 31 2 *\"".into())
        );
        assert!(is_once(" @Once 2026"));
        assert_eq!(
            next_after("@once 2026-10-16 7:00", "UTC", from),
            Ok(ns(1_792_134_000))
        );
        assert_eq!(compute_next_run("bad", "UTC", from), 0);
    }
}
