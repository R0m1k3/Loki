//! Fuseaux horaires à la Go (`time.LoadLocation`, `time.Local`) : les fichiers
//! TZif du système (`/usr/share/zoneinfo`…), lus comme le fait la bibliothèque
//! standard du Go — transitions 64 bits (v2+), règle POSIX du pied de fichier
//! pour les dates au-delà de la dernière transition — et la conversion d'une
//! heure murale en instant comme `time.Date`.
//!
//! Sert aux tâches planifiées (fréquences cron, « tous les jours à 23:00 »,
//! « @once ») : la même zoneinfo que le Go, donc les mêmes déclenchements.

use std::path::Path;

/// Une zone : décalage UTC (s), heure d'été, abréviation.
#[derive(Clone, Debug, PartialEq)]
pub struct Zone {
    pub name: String,
    pub offset: i64,
    pub is_dst: bool,
}

/// Un fuseau chargé.
#[derive(Clone, Debug)]
pub struct Location {
    pub name: String,
    zones: Vec<Zone>,
    /// (instant, index de zone), triés.
    tx: Vec<(i64, usize)>,
    /// Règle POSIX au-delà de la dernière transition.
    extend: String,
}

const ALPHA: i64 = i64::MIN;
const OMEGA: i64 = i64::MAX;

impl Location {
    /// `time.UTC`.
    pub fn utc() -> Self {
        Location {
            name: "UTC".into(),
            zones: vec![Zone {
                name: "UTC".into(),
                offset: 0,
                is_dst: false,
            }],
            tx: Vec::new(),
            extend: String::new(),
        }
    }

    /// `time.LoadLocation` : "" ou "UTC" = UTC ; sinon le fichier TZif du
    /// système. Erreur si introuvable ou illisible.
    pub fn load(name: &str) -> Result<Self, String> {
        if name.is_empty() || name == "UTC" {
            return Ok(Self::utc());
        }
        if name == "Local" {
            return Ok(Self::local());
        }
        let mut sources: Vec<String> = Vec::new();
        if let Ok(z) = std::env::var("ZONEINFO")
            && !z.is_empty()
        {
            sources.push(z);
        }
        sources.extend(
            [
                "/usr/share/zoneinfo/",
                "/usr/share/lib/zoneinfo/",
                "/usr/lib/locale/TZ/",
                "/etc/zoneinfo/",
            ]
            .iter()
            .map(|s| s.to_string()),
        );
        Self::load_in(name, &sources)
    }

    /// `LoadLocation` sur les bases données (dans l'ordre).
    fn load_in(name: &str, sources: &[String]) -> Result<Self, String> {
        if name.is_empty() || name == "UTC" {
            return Ok(Self::utc());
        }
        if !valid_name(name) {
            return Err("time: invalid location name".to_string());
        }
        for src in sources {
            let p = Path::new(src).join(name);
            if let Ok(b) = std::fs::read(&p) {
                return parse_tzif(name, &b);
            }
        }
        Err(format!("unknown time zone {name}"))
    }

    /// `time.Local` : `$TZ` (vide = UTC, « :chemin » accepté), sinon
    /// /etc/localtime, sinon UTC.
    pub fn local() -> Self {
        Self::local_from(std::env::var("TZ").ok(), Path::new("/etc/localtime"))
    }

    /// `initLocal` : `tz` la valeur de `$TZ` (absente : `None`), `etc` le
    /// fuseau du système.
    fn local_from(tz: Option<String>, etc: &Path) -> Self {
        match tz {
            Some(tz) => {
                let tz = tz.strip_prefix(':').unwrap_or(&tz).to_string();
                if tz.is_empty() || tz == "UTC" {
                    return Self::utc();
                }
                if tz.starts_with('/') {
                    if let Ok(b) = std::fs::read(&tz)
                        && let Ok(mut l) = parse_tzif("Local", &b)
                    {
                        l.name = "Local".into();
                        return l;
                    }
                    return Self::utc();
                }
                match Self::load(&tz) {
                    Ok(mut l) => {
                        l.name = "Local".into();
                        l
                    }
                    Err(_) => Self::utc(),
                }
            }
            None => match std::fs::read(etc)
                .ok()
                .and_then(|b| parse_tzif("Local", &b).ok())
            {
                Some(mut l) => {
                    l.name = "Local".into();
                    l
                }
                None => {
                    let mut u = Self::utc();
                    u.name = "Local".into();
                    u
                }
            },
        }
    }

    /// `(*Location).lookup` : la zone d'un instant et la période où elle
    /// vaut, [start, end[.
    pub fn lookup(&self, sec: i64) -> (Zone, i64, i64) {
        if self.zones.is_empty() {
            return (
                Zone {
                    name: "UTC".into(),
                    offset: 0,
                    is_dst: false,
                },
                ALPHA,
                OMEGA,
            );
        }
        if self.tx.is_empty() || sec < self.tx[0].0 {
            let z = self.zones[self.lookup_first_zone()].clone();
            let end = self.tx.first().map_or(OMEGA, |t| t.0);
            return (z, ALPHA, end);
        }
        // Dichotomie : la dernière transition <= sec.
        let (mut lo, mut hi) = (0usize, self.tx.len());
        let mut end = OMEGA;
        while hi - lo > 1 {
            let m = lo + (hi - lo) / 2;
            let lim = self.tx[m].0;
            if sec < lim {
                end = lim;
                hi = m;
            } else {
                lo = m;
            }
        }
        let zone = self.zones[self.tx[lo].1].clone();
        let start = self.tx[lo].0;
        if lo == self.tx.len() - 1
            && !self.extend.is_empty()
            && let Some((z, s, e)) = tzset(&self.extend, start, sec)
        {
            return (z, s, e);
        }
        (zone, start, end)
    }

    /// `lookupFirstZone`.
    fn lookup_first_zone(&self) -> usize {
        if !self.first_zone_used() {
            return 0;
        }
        if !self.tx.is_empty() && self.zones[self.tx[0].1].is_dst {
            let mut zi = self.tx[0].1 as i64 - 1;
            while zi >= 0 {
                if !self.zones[zi as usize].is_dst {
                    return zi as usize;
                }
                zi -= 1;
            }
        }
        for (i, z) in self.zones.iter().enumerate() {
            if !z.is_dst {
                return i;
            }
        }
        0
    }

    fn first_zone_used(&self) -> bool {
        self.tx.iter().any(|t| t.1 == 0)
    }

    /// Le décalage d'un instant.
    pub fn offset_at(&self, sec: i64) -> i64 {
        self.lookup(sec).0.offset
    }

    /// `time.Date(y, m, d, h, mi, s, 0, loc)` : l'instant (s Unix) d'une heure
    /// murale, normalisée comme le Go (débordements reportés).
    pub fn date(&self, y: i64, m: i64, d: i64, h: i64, mi: i64, s: i64) -> i64 {
        // Normalisation du mois puis des secondes du jour.
        let (mut y, mut m) = (y, m - 1);
        y += m.div_euclid(12);
        m = m.rem_euclid(12);
        let days = days_from_civil(y, m + 1, 1) + (d - 1);
        let unix = days * 86400 + h * 3600 + mi * 60 + s;
        let (z, start, end) = self.lookup(unix);
        let mut offset = z.offset;
        let mut out = unix;
        if offset != 0 {
            let utc = unix - offset;
            if utc < start || utc >= end {
                offset = self.lookup(utc).0.offset;
            }
            out -= offset;
        }
        out
    }

    /// L'heure murale d'un instant : (année, mois, jour, heure, minute,
    /// seconde, jour de la semaine 0 = dimanche).
    pub fn civil(&self, sec: i64) -> Civil {
        civil(sec + self.offset_at(sec))
    }
}

/// Une heure murale.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Civil {
    pub year: i64,
    pub month: i64,
    pub day: i64,
    pub hour: i64,
    pub minute: i64,
    pub second: i64,
    pub weekday: i64,
}

impl Civil {
    /// `Format("2006-01-02 15:04")`.
    pub fn ymd_hm(&self) -> String {
        format!(
            "{:04}-{:02}-{:02} {:02}:{:02}",
            self.year, self.month, self.day, self.hour, self.minute
        )
    }
}

/// Les secondes « locales » (UTC + décalage) en heure murale.
pub fn civil(local: i64) -> Civil {
    let days = local.div_euclid(86400);
    let rem = local.rem_euclid(86400);
    let (y, m, d) = civil_from_days(days);
    Civil {
        year: y,
        month: m,
        day: d,
        hour: rem / 3600,
        minute: rem % 3600 / 60,
        second: rem % 60,
        weekday: (days + 4).rem_euclid(7),
    }
}

/// Jours depuis 1970-01-01 (calendrier grégorien proleptique).
pub fn days_from_civil(y: i64, m: i64, d: i64) -> i64 {
    let y = if m <= 2 { y - 1 } else { y };
    let era = y.div_euclid(400);
    let yoe = y - era * 400;
    let mp = (m + 9) % 12;
    let doy = (153 * mp + 2) / 5 + d - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    era * 146097 + doe - 719468
}

/// L'inverse de [`days_from_civil`].
pub fn civil_from_days(z: i64) -> (i64, i64, i64) {
    let z = z + 719468;
    let era = z.div_euclid(146097);
    let doe = z - era * 146097;
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    (if m <= 2 { y + 1 } else { y }, m, d)
}

fn is_leap(y: i64) -> bool {
    y % 4 == 0 && (y % 100 != 0 || y % 400 == 0)
}

/// `containsDotDot` et consorts : un nom de zone sans « .. » ni chemin
/// absolu.
/// Ni `..` (n'importe où, comme `containsDotDot`), ni chemin absolu.
fn valid_name(name: &str) -> bool {
    !(name.contains("..") || name.starts_with('/') || name.starts_with('\\'))
}

fn be32(b: &[u8]) -> i64 {
    i32::from_be_bytes([b[0], b[1], b[2], b[3]]) as i64
}

fn be64(b: &[u8]) -> i64 {
    i64::from_be_bytes([b[0], b[1], b[2], b[3], b[4], b[5], b[6], b[7]])
}

/// `LoadLocationFromTZData`.
pub fn parse_tzif(name: &str, data: &[u8]) -> Result<Location, String> {
    let bad = || "malformed time zone information".to_string();
    if data.len() < 44 || &data[..4] != b"TZif" {
        return Err(bad());
    }
    let version = data[4];
    let counts = |off: usize| -> [usize; 6] {
        let mut n = [0usize; 6];
        for (i, v) in n.iter_mut().enumerate() {
            *v = u32::from_be_bytes([
                data[off + 20 + i * 4],
                data[off + 21 + i * 4],
                data[off + 22 + i * 4],
                data[off + 23 + i * 4],
            ]) as usize;
        }
        n
    };
    // [isutcnt, isstdcnt, leapcnt, timecnt, typecnt, charcnt]
    let mut n = counts(0);
    let mut off = 44;
    let mut wide = false;
    if version >= b'2' {
        // Saute le bloc v1.
        let skip = n[3] * 4 + n[3] + n[4] * 6 + n[5] + n[2] * 8 + n[1] + n[0];
        let o2 = 44 + skip;
        if data.len() < o2 + 44 || &data[o2..o2 + 4] != b"TZif" {
            return Err(bad());
        }
        n = counts(o2);
        off = o2 + 44;
        wide = true;
    }
    let tsz = if wide { 8 } else { 4 };
    let need = n[3] * tsz + n[3] + n[4] * 6 + n[5] + n[2] * (tsz + 4) + n[1] + n[0];
    if data.len() < off + need {
        return Err(bad());
    }
    let times = &data[off..off + n[3] * tsz];
    let idx = &data[off + n[3] * tsz..off + n[3] * tsz + n[3]];
    let zo = off + n[3] * tsz + n[3];
    let zdata = &data[zo..zo + n[4] * 6];
    let chars = &data[zo + n[4] * 6..zo + n[4] * 6 + n[5]];
    let after = zo + n[4] * 6 + n[5] + n[2] * (tsz + 4) + n[1] + n[0];
    let mut extend = String::new();
    if wide && data.len() > after && data[after] == b'\n' {
        let rest = &data[after + 1..];
        if let Some(e) = rest.iter().position(|c| *c == b'\n') {
            extend = String::from_utf8_lossy(&rest[..e]).into_owned();
        }
    }
    if n[4] == 0 {
        return Err(bad());
    }
    let mut zones = Vec::new();
    for i in 0..n[4] {
        let z = &zdata[i * 6..i * 6 + 6];
        let off = be32(z);
        let is_dst = z[4] != 0;
        let di = z[5] as usize;
        if di >= chars.len() {
            return Err(bad());
        }
        let end = chars[di..]
            .iter()
            .position(|c| *c == 0)
            .unwrap_or(chars.len() - di);
        zones.push(Zone {
            name: String::from_utf8_lossy(&chars[di..di + end]).into_owned(),
            offset: off,
            is_dst,
        });
    }
    let mut tx = Vec::new();
    for i in 0..n[3] {
        let t = if wide {
            be64(&times[i * 8..])
        } else {
            be32(&times[i * 4..])
        };
        let zi = idx[i] as usize;
        if zi >= zones.len() {
            return Err(bad());
        }
        tx.push((t, zi));
    }
    if tx.is_empty() {
        // Comme le Go : une transition fictive au début des temps.
        tx.push((ALPHA, 0));
    }
    Ok(Location {
        name: name.to_string(),
        zones,
        tx,
        extend,
    })
}

// ---------------------------------------------------------------------------
// Règle POSIX (tzset du Go)
// ---------------------------------------------------------------------------

/// `tzset` : la zone d'un instant d'après une règle POSIX (« CET-1CEST,M3.5.0,
/// M10.5.0/3 »), et sa période. `None` : règle illisible.
fn tzset(s: &str, last_tx: i64, sec: i64) -> Option<(Zone, i64, i64)> {
    let (std_name, s) = tzset_name(s)?;
    let (std_off, s) = tzset_offset(s)?;
    let std_offset = -std_off;
    if s.is_empty() || s.starts_with(',') {
        // Pas d'heure d'été.
        return Some((
            Zone {
                name: std_name,
                offset: std_offset,
                is_dst: false,
            },
            last_tx,
            OMEGA,
        ));
    }
    let (dst_name, s) = tzset_name(s)?;
    let (dst_offset, s) = if s.is_empty() || s.starts_with(',') {
        (std_offset + 3600, s)
    } else {
        let (o, r) = tzset_offset(s)?;
        (-o, r)
    };
    let s = if s.is_empty() { ",M3.2.0,M11.1.0" } else { s };
    let s = s.strip_prefix(',')?;
    let (start_rule, s) = tzset_rule(s)?;
    let s = s.strip_prefix(',')?;
    let (end_rule, s) = tzset_rule(s)?;
    if !s.is_empty() {
        return None;
    }
    let (year, yday) = {
        let c = civil(sec);
        let doy = days_from_civil(c.year, c.month, c.day) - days_from_civil(c.year, 1, 1);
        (c.year, doy)
    };
    let _ = yday;
    let ysec = days_from_civil(year, 1, 1) * 86400;
    let mut start = ysec + tzrule_time(year, &start_rule, std_offset);
    let mut end = ysec + tzrule_time(year, &end_rule, dst_offset);
    let (dst_is_dst, std_is_dst) = (true, false);
    let (mut lo, mut hi) = (
        Zone {
            name: std_name.clone(),
            offset: std_offset,
            is_dst: std_is_dst,
        },
        Zone {
            name: dst_name.clone(),
            offset: dst_offset,
            is_dst: dst_is_dst,
        },
    );
    if end < start {
        std::mem::swap(&mut start, &mut end);
        std::mem::swap(&mut lo, &mut hi);
    }
    let yearend = days_from_civil(year + 1, 1, 1) * 86400;
    if sec < start {
        return Some((lo, ysec, start));
    } else if sec >= end {
        return Some((lo, end, yearend));
    }
    Some((hi, start, end))
}

fn tzset_name(s: &str) -> Option<(String, &str)> {
    if s.is_empty() {
        return None;
    }
    if let Some(r) = s.strip_prefix('<') {
        let e = r.find('>')?;
        return Some((r[..e].to_string(), &r[e + 1..]));
    }
    let e = s
        .find(|c: char| c.is_ascii_digit() || c == ',' || c == '-' || c == '+')
        .unwrap_or(s.len());
    if e < 3 {
        return None;
    }
    Some((s[..e].to_string(), &s[e..]))
}

fn tzset_offset(s: &str) -> Option<(i64, &str)> {
    let (neg, s) = match s.as_bytes().first()? {
        b'+' => (false, &s[1..]),
        b'-' => (true, &s[1..]),
        _ => (false, s),
    };
    let (h, mut s) = tzset_num(s, 0, 24 * 7)?;
    let mut off = h * 3600;
    if let Some(r) = s.strip_prefix(':') {
        let (m, r) = tzset_num(r, 0, 59)?;
        off += m * 60;
        s = r;
        if let Some(r) = s.strip_prefix(':') {
            let (sec, r) = tzset_num(r, 0, 59)?;
            off += sec;
            s = r;
        }
    }
    Some((if neg { -off } else { off }, s))
}

fn tzset_num(s: &str, min: i64, max: i64) -> Option<(i64, &str)> {
    let e = s.find(|c: char| !c.is_ascii_digit()).unwrap_or(s.len());
    if e == 0 {
        return None;
    }
    let n: i64 = s[..e].parse().ok()?;
    if n < min || n > max {
        return None;
    }
    Some((n, &s[e..]))
}

enum RuleKind {
    Julian(i64),
    Doy(i64),
    Mwd(i64, i64, i64),
}

struct Rule {
    kind: RuleKind,
    time: i64,
}

fn tzset_rule(s: &str) -> Option<(Rule, &str)> {
    let (kind, s) = if let Some(r) = s.strip_prefix('J') {
        let (d, r) = tzset_num(r, 1, 365)?;
        (RuleKind::Julian(d), r)
    } else if let Some(r) = s.strip_prefix('M') {
        let (m, r) = tzset_num(r, 1, 12)?;
        let r = r.strip_prefix('.')?;
        let (w, r) = tzset_num(r, 1, 5)?;
        let r = r.strip_prefix('.')?;
        let (d, r) = tzset_num(r, 0, 6)?;
        (RuleKind::Mwd(m, w, d), r)
    } else {
        let (d, r) = tzset_num(s, 0, 365)?;
        (RuleKind::Doy(d), r)
    };
    let (time, s) = if let Some(r) = s.strip_prefix('/') {
        tzset_offset(r)?
    } else {
        (2 * 3600, s)
    };
    Some((Rule { kind, time }, s))
}

/// `tzruleTime` : secondes depuis le début de l'année (UTC) où la règle
/// s'applique, pour le décalage en vigueur avant elle.
fn tzrule_time(year: i64, r: &Rule, off: i64) -> i64 {
    let s = match r.kind {
        RuleKind::Julian(d) => {
            let mut s = (d - 1) * 86400;
            if is_leap(year) && d >= 60 {
                s += 86400;
            }
            s
        }
        RuleKind::Doy(d) => d * 86400,
        RuleKind::Mwd(mon, week, day) => {
            // Zeller : jour de la semaine du 1er du mois.
            let m1 = (mon + 9) % 12 + 1;
            let mut yy0 = year;
            if mon <= 2 {
                yy0 -= 1;
            }
            let yy1 = yy0 / 100;
            let yy2 = yy0 % 100;
            let mut dow = ((26 * m1 - 2) / 10 + 1 + yy2 + yy2 / 4 + yy1 / 4 - 2 * yy1) % 7;
            if dow < 0 {
                dow += 7;
            }
            let mut d = day - dow;
            if d < 0 {
                d += 7;
            }
            for _ in 1..week {
                if d + 7 >= days_in(mon, year) {
                    break;
                }
                d += 7;
            }
            let mut s = d * 86400;
            for m in 1..mon {
                s += days_in(m, year) * 86400;
            }
            s
        }
    };
    s + r.time - off
}

fn days_in(m: i64, year: i64) -> i64 {
    match m {
        2 => {
            if is_leap(year) {
                29
            } else {
                28
            }
        }
        4 | 6 | 9 | 11 => 30,
        _ => 31,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn heure_locale() {
        let hiver = 1_767_268_800; // 2026-01-01 12:00 UTC
        let etc = Path::new("/usr/share/zoneinfo/Europe/Paris");
        let off = |tz: Option<&str>, etc: &Path| {
            Location::local_from(tz.map(str::to_string), etc).offset_at(hiver)
        };
        assert_eq!(off(None, etc), 3600);
        assert_eq!(off(None, Path::new("/nulle/part")), 0);
        assert_eq!(off(Some(""), etc), 0);
        assert_eq!(off(Some(":"), etc), 0);
        assert_eq!(off(Some("UTC"), etc), 0);
        assert_eq!(off(Some(":Asia/Kolkata"), etc), 19_800);
        assert_eq!(off(Some("America/New_York"), etc), -18_000);
        assert_eq!(off(Some(":/usr/share/zoneinfo/Asia/Kolkata"), etc), 19_800);
        assert_eq!(off(Some("/nulle/part"), etc), 0);
        assert_eq!(off(Some("Nulle/Part"), etc), 0);
    }

    #[test]
    fn noms_de_fuseau() {
        let sys = vec!["/usr/share/zoneinfo/".to_string()];
        // « UTC » sans base : comme le Go, sans fichier.
        assert_eq!(Location::load_in("UTC", &[]).unwrap().name, "UTC");
        assert_eq!(Location::load_in("", &[]).unwrap().offset_at(0), 0);
        for bad in ["../etc/passwd", "a..b", "/etc/localtime", "\\x"] {
            assert_eq!(
                Location::load_in(bad, &sys).err().as_deref(),
                Some("time: invalid location name"),
                "{bad}"
            );
        }
        assert_eq!(
            Location::load_in("Europe/Paris", &[]).err().as_deref(),
            Some("unknown time zone Europe/Paris")
        );
        assert!(Location::load_in("Europe/Paris", &sys).is_ok());
    }

    /// Valeurs tirées du Go.
    #[test]
    fn transitions_et_normalisation() {
        let ny = Location::load("America/New_York").unwrap();
        let paris = Location::load("Europe/Paris").unwrap();
        // Avant la première transition : l'heure solaire de New York.
        assert_eq!(ny.offset_at(-3_000_000_000), -17_762);
        // Pas d'heure d'été en France en 1970 : la table, pas la règle.
        assert_eq!(paris.offset_at(paris.date(1970, 7, 1, 14, 0, 0)), 3600);
        assert_eq!(paris.offset_at(1_782_907_200), 7200);
        // Pile à la transition : la nouvelle heure.
        assert_eq!(paris.offset_at(828_234_000), 7200);
        assert_eq!(paris.offset_at(828_233_999), 3600);
        assert_eq!(paris.offset_at(846_378_000), 3600);
        assert_eq!(paris.offset_at(846_377_999), 7200);
        // L'heure qui saute, à l'ouest de Greenwich.
        assert_eq!(ny.date(2026, 3, 8, 3, 30, 0), 1_772_955_000);
        assert_eq!(ny.date(2026, 3, 8, 2, 30, 0), 1_772_951_400);
        assert_eq!(ny.offset_at(1_782_907_200), -14_400);
        // Mois hors bornes, reportés sur l'année.
        let u = Location::utc();
        assert_eq!(u.date(2026, 0, 15, 10, 0, 0), 1_765_792_800);
        assert_eq!(u.date(2026, -13, 15, 10, 0, 0), 1_731_664_800);
    }

    #[test]
    fn calendrier() {
        assert_eq!(days_from_civil(1970, 1, 1), 0);
        assert_eq!(civil_from_days(0), (1970, 1, 1));
        assert_eq!(days_from_civil(2026, 10, 15), 20741);
        assert_eq!(civil_from_days(20741), (2026, 10, 15));
        assert_eq!(civil(0).weekday, 4, "jeudi");
        // Mois et jours hors bornes, normalisés comme time.Date.
        let u = Location::utc();
        assert_eq!(u.date(2026, 13, 1, 0, 0, 0), u.date(2027, 1, 1, 0, 0, 0));
        assert_eq!(u.date(2026, 2, 30, 0, 0, 0), u.date(2026, 3, 2, 0, 0, 0));
        assert_eq!(u.date(2026, 1, 0, 0, 0, 0), u.date(2025, 12, 31, 0, 0, 0));
    }

    #[test]
    fn paris() {
        let Ok(p) = Location::load("Europe/Paris") else {
            eprintln!("zoneinfo absente : test sauté");
            return;
        };
        // Hiver : +1 h ; été : +2 h.
        let jan = Location::utc().date(2026, 1, 15, 12, 0, 0);
        assert_eq!(p.offset_at(jan), 3600);
        let jul = Location::utc().date(2026, 7, 15, 12, 0, 0);
        assert_eq!(p.offset_at(jul), 7200);
        // Bien au-delà de la dernière transition du fichier : la règle POSIX.
        let far = Location::utc().date(2090, 7, 15, 12, 0, 0);
        assert_eq!(p.offset_at(far), 7200);
        let far_w = Location::utc().date(2090, 12, 15, 12, 0, 0);
        assert_eq!(p.offset_at(far_w), 3600);
        // Heure murale → instant.
        let t = p.date(2026, 7, 15, 23, 0, 0);
        assert_eq!(p.civil(t).ymd_hm(), "2026-07-15 23:00");
        assert_eq!(Location::utc().civil(t).ymd_hm(), "2026-07-15 21:00");
        // Heure qui n'existe pas (passage à l'été) : comme le Go, 02:30 → 03:30.
        let gap = p.date(2026, 3, 29, 2, 30, 0);
        assert_eq!(p.civil(gap).ymd_hm(), "2026-03-29 03:30");
        assert_eq!(gap, 1774747800, "instant donné par le Go");
        // Heure double (passage à l'hiver) : la seconde, comme l'algorithme de
        // time.Date (vérifié contre le Go).
        let dbl = p.date(2026, 10, 25, 2, 30, 0);
        assert_eq!((dbl, p.offset_at(dbl)), (1792891800, 3600));
        assert_eq!(t, 1784149200);
        assert!(Location::load("Nulle/Part").is_err());
        assert!(Location::load("../etc/passwd").is_err());
    }
}
