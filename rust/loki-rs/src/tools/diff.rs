//! Petit diff ligne à ligne pour l'interface — port de `chat_diff.go`.
//!
//! Quand l'agent modifie un fichier (`edit`) ou en crée un (`write`), l'UI
//! affiche les lignes ajoutées (vert) et retirées (rouge). LCS classique sur
//! les lignes, avec des garde-fous pour ne jamais transformer un gros
//! remplacement en pavé. Les compteurs +N / -N sont calculés AVANT la coupe
//! des lignes affichées.

use crate::chat::types::DiffLine;

/// Au-delà (partie modifiée), comparaison sans détail (LCS trop lente).
const DIFF_MAX_LINES: usize = 400;
/// Lignes envoyées à l'UI (le reste est résumé).
const DIFF_MAX_SHOWN: usize = 120;
/// Lignes identiques gardées autour d'un changement.
const DIFF_CONTEXT: usize = 3;
const DIFF_OMIT_PREFIX: &str = "…";

fn line(op: &str, text: &str) -> DiffLine {
    DiffLine {
        op: op.into(),
        text: text.into(),
    }
}

/// `lineDiff` : lignes à afficher (bornées), lignes ajoutées, retirées.
pub fn line_diff(old_text: &str, new_text: &str) -> (Vec<DiffLine>, i64, i64) {
    let a = split_lines(old_text);
    let b = split_lines(new_text);
    let mut pre = 0;
    while pre < a.len() && pre < b.len() && a[pre] == b[pre] {
        pre += 1;
    }
    let mut suf = 0;
    while suf < a.len() - pre && suf < b.len() - pre && a[a.len() - 1 - suf] == b[b.len() - 1 - suf]
    {
        suf += 1;
    }
    let (ma, mb) = (&a[pre..a.len() - suf], &b[pre..b.len() - suf]);
    let mid: Vec<DiffLine> = if ma.len() > DIFF_MAX_LINES || mb.len() > DIFF_MAX_LINES {
        ma.iter()
            .map(|l| line("-", l))
            .chain(mb.iter().map(|l| line("+", l)))
            .collect()
    } else {
        lcs_diff(ma, mb)
    };
    let added = mid.iter().filter(|d| d.op == "+").count() as i64;
    let removed = mid.iter().filter(|d| d.op == "-").count() as i64;

    let mut out = Vec::new();
    if pre > DIFF_CONTEXT {
        out.push(line(" ", &omitted(pre - DIFF_CONTEXT)));
    }
    for l in &a[pre.saturating_sub(DIFF_CONTEXT)..pre] {
        out.push(line(" ", l));
    }
    out.extend(mid);
    for l in &a[a.len() - suf..a.len() - suf + suf.min(DIFF_CONTEXT)] {
        out.push(line(" ", l));
    }
    if suf > DIFF_CONTEXT {
        out.push(line(" ", &omitted(suf - DIFF_CONTEXT)));
    }
    (cap_lines(out), added, removed)
}

/// Diff exact par plus longue sous-séquence commune.
fn lcs_diff(a: &[String], b: &[String]) -> Vec<DiffLine> {
    let (n, m) = (a.len(), b.len());
    let mut lcs = vec![vec![0usize; m + 1]; n + 1];
    for i in (0..n).rev() {
        for j in (0..m).rev() {
            lcs[i][j] = if a[i] == b[j] {
                lcs[i + 1][j + 1] + 1
            } else {
                lcs[i + 1][j].max(lcs[i][j + 1])
            };
        }
    }
    let mut out = Vec::new();
    let (mut i, mut j) = (0, 0);
    while i < n && j < m {
        if a[i] == b[j] {
            out.push(line(" ", &a[i]));
            i += 1;
            j += 1;
        } else if lcs[i + 1][j] >= lcs[i][j + 1] {
            out.push(line("-", &a[i]));
            i += 1;
        } else {
            out.push(line("+", &b[j]));
            j += 1;
        }
    }
    out.extend(a[i..].iter().map(|l| line("-", l)));
    out.extend(b[j..].iter().map(|l| line("+", l)));
    out
}

/// `addedDiff` : contenu entièrement nouveau, toutes ses lignes en ajout.
pub fn added_diff(text: &str) -> (Vec<DiffLine>, i64) {
    let ls = split_lines(text);
    let n = ls.len() as i64;
    (cap_lines(ls.iter().map(|l| line("+", l)).collect()), n)
}

/// Un saut de ligne final termine la dernière ligne, il n'en ouvre pas une
/// vide. Les fins CRLF comptent comme LF.
fn split_lines(s: &str) -> Vec<String> {
    let s = s.replace("\r\n", "\n");
    let s = s.strip_suffix('\n').unwrap_or(&s);
    if s.is_empty() {
        return Vec::new();
    }
    s.split('\n').map(str::to_string).collect()
}

fn omitted(n: usize) -> String {
    if n == 1 {
        format!("{DIFF_OMIT_PREFIX}(1 ligne identique)")
    } else {
        format!("{DIFF_OMIT_PREFIX}({n} lignes identiques)")
    }
}

fn cap_lines(mut d: Vec<DiffLine>) -> Vec<DiffLine> {
    if d.len() <= DIFF_MAX_SHOWN {
        return d;
    }
    let cut = d.len() - DIFF_MAX_SHOWN;
    d.truncate(DIFF_MAX_SHOWN);
    d.push(line(
        " ",
        &format!("{DIFF_OMIT_PREFIX}({cut} lignes de plus)"),
    ));
    d
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ops(d: &[DiffLine]) -> Vec<String> {
        d.iter().map(|l| format!("{}{}", l.op, l.text)).collect()
    }

    #[test]
    fn une_ligne_changee_dans_un_gros_bloc() {
        let old: String = (1..=20).map(|i| format!("l{i}\n")).collect();
        let new = old.replace("l10\n", "dix\n");
        let (d, a, r) = line_diff(&old, &new);
        assert_eq!((a, r), (1, 1));
        assert_eq!(
            ops(&d),
            [
                " …(6 lignes identiques)",
                " l7",
                " l8",
                " l9",
                "-l10",
                "+dix",
                " l11",
                " l12",
                " l13",
                " …(7 lignes identiques)"
            ]
        );
    }

    #[test]
    fn ajout_et_coupe() {
        let text: String = (0..130).map(|i| format!("{i}\n")).collect();
        let (d, n) = added_diff(&text);
        assert_eq!(n, 130);
        assert_eq!(d.len(), 121);
        assert_eq!(d[120].text, "…(10 lignes de plus)");
        assert_eq!(added_diff("").1, 0);
        assert_eq!(added_diff("a\r\nb\r\n").1, 2);
    }

    #[test]
    fn gros_remplacement_sans_lcs() {
        let old: String = (0..401).map(|i| format!("a{i}\n")).collect();
        let (_, a, r) = line_diff(&old, "b\n");
        assert_eq!((a, r), (1, 401));
    }
}
