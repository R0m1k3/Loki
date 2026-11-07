//! Le shell de l'agent — port de `runShell` (`chat_tools.go`) et de
//! `chat_shell_hygiene.go`.
//!
//! La commande part dans `/bin/bash -c` (`cmd /C` sous Windows), dans le
//! dossier de la discussion, délai borné. Ce qu'elle rend au modèle est
//! nettoyé : « | tail -N » final retiré (le code de sortie serait celui de
//! tail) puis appliqué ici après repli des barres de progression (`\r`),
//! séquences ANSI retirées, sortie bornée à son début et à sa fin.
//!
//! L'arrêt du tour abandonne le futur : le process est tué (`kill_on_drop`).

use std::path::Path;
use std::process::Stdio;
use std::sync::{Arc, LazyLock, Mutex};
use std::time::{Duration, Instant};

use regex::Regex;
use tokio::io::AsyncReadExt;

use super::policy::guard_tool_only_command;

pub const TOOL_DEFAULT_TIMEOUT: i64 = 30;
pub const TOOL_MAX_TIMEOUT: i64 = 300;
/// Caractères de stdout/stderr rendus au modèle.
pub const TOOL_MAX_OUTPUT: usize = 8000;
/// `WaitDelay` : attente bornée des tubes après la fin du process — un
/// « ./serveur & » laisse un petit-enfant accroché aux tubes, et le tour ne
/// rendrait jamais la main.
const WAIT_DELAY: Duration = Duration::from_secs(2);

/// « | tail », « | tail -20 », « | tail -n 20 », « | tail -n20 » en fin.
static TAIL_PIPE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"\s*\|\s*tail(?:\s+-n\s*([0-9]+)|\s+-([0-9]+))?\s*$").expect("motif tail")
});

/// `splitTailPipe` : la commande sans son « | tail -N » final, et N (0 : pas
/// de tail ; 10 : tail sans nombre).
pub fn split_tail_pipe(command: &str) -> (String, usize) {
    let Some(c) = TAIL_PIPE.captures(command) else {
        return (command.to_string(), 0);
    };
    let n = [1, 2]
        .into_iter()
        .filter_map(|g| c.get(g))
        .filter_map(|m| m.as_str().parse::<usize>().ok())
        .find(|v| *v > 0)
        .unwrap_or(10);
    let rest = command[..c.get(0).map_or(0, |m| m.start())].trim();
    if rest.is_empty() {
        return (command.to_string(), 0);
    }
    (rest.to_string(), n)
}

/// Séquences CSI (couleurs, curseur) et OSC (titres de fenêtre).
static ANSI: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)").expect("motif ANSI")
});

pub fn strip_ansi(s: &str) -> String {
    if !s.contains('\x1b') {
        return s.to_string();
    }
    ANSI.replace_all(s, "").into_owned()
}

/// `trailingBackground` : la commande finit par un « & » (pas « && »).
pub fn trailing_background(command: &str) -> bool {
    let c = command.trim();
    c.ends_with('&') && !c.ends_with("&&")
}

/// `tailLines` : les n dernières lignes.
pub fn tail_lines(s: &str, n: usize) -> String {
    let lines: Vec<&str> = s.trim_end_matches('\n').split('\n').collect();
    lines[lines.len().saturating_sub(n)..].join("\n")
}

/// Part du budget gardée au début d'une sortie trop longue (le reste : la fin).
const HEAD_SHARE: usize = TOOL_MAX_OUTPUT / 4;

/// Barres de progression repliées, sur les octets bruts (avant décodage) :
/// une ligne redessinée avec `\r` ne garde que son dernier dessin non vide.
/// Le `\r` d'un « \r\n » reste une fin de ligne ordinaire.
pub fn collapse_cr(b: &[u8]) -> std::borrow::Cow<'_, [u8]> {
    if !b.contains(&b'\r') {
        return b.into();
    }
    let lines: Vec<&[u8]> = b.split(|&c| c == b'\n').collect();
    let last_line = lines.len() - 1;
    let mut out = Vec::with_capacity(b.len());
    for (i, line) in lines.into_iter().enumerate() {
        if i > 0 {
            out.push(b'\n');
        }
        // Un `\r` final sans `\n` derrière (dernier dessin d'une barre) part.
        let (body, crlf) = match line.strip_suffix(b"\r") {
            Some(body) => (body, i < last_line),
            None => (line, false),
        };
        let last = body
            .split(|&c| c == b'\r')
            .rfind(|seg| !seg.is_empty())
            .unwrap_or_default();
        out.extend_from_slice(last);
        if crlf {
            out.push(b'\r');
        }
    }
    out.into()
}

/// Indice d'octet du `n`-ième caractère de `s` (`s.len()` au-delà).
fn char_byte(s: &str, n: usize) -> usize {
    s.char_indices().nth(n).map_or(s.len(), |(i, _)| i)
}

/// `tailOutput` : une sortie bornée à `TOOL_MAX_OUTPUT` caractères — son
/// DÉBUT (un quart) et sa FIN (le reste), coupés entre deux lignes quand la
/// coupe garde au moins la moitié de sa part, avec le nombre de lignes
/// omises dit en clair entre les deux.
pub fn tail_output(s: &str) -> String {
    let n = s.chars().count();
    if n <= TOOL_MAX_OUTPUT {
        return s.to_string();
    }
    let head_raw = char_byte(s, HEAD_SHARE);
    let head_end = match s[..head_raw].rfind('\n') {
        Some(i) if s[..=i].chars().count() >= HEAD_SHARE / 2 => i + 1,
        _ => head_raw,
    };
    let tail_share = TOOL_MAX_OUTPUT - HEAD_SHARE;
    let tail_raw = char_byte(s, n - tail_share);
    let tail_start = match s[tail_raw..].find('\n') {
        Some(i) if s[tail_raw + i + 1..].chars().count() >= tail_share / 2 => tail_raw + i + 1,
        _ => tail_raw,
    };
    let (head, tail) = (&s[..head_end], &s[tail_start..]);
    // Lignes touchées par la coupe, entières ou en partie.
    let omitted = s[head_end..tail_start].split_inclusive('\n').count();
    let what = if omitted == 1 {
        "ligne omise"
    } else {
        "lignes omises"
    };
    let sep = if head.ends_with('\n') { "" } else { "\n" };
    format!("{head}{sep}… {omitted} {what} …\n{tail}")
}

/// `time.Duration.String()` d'une durée arrondie à 100 ms (« 0s », « 300ms »,
/// « 1.2s », « 1m5s », « 1h0m0s »).
pub fn go_duration(d: Duration) -> String {
    let ms = ((d.as_millis() + 50) / 100 * 100) as u64;
    if ms == 0 {
        return "0s".into();
    }
    if ms < 1000 {
        return format!("{ms}ms");
    }
    let (h, rem) = (ms / 3_600_000, ms % 3_600_000);
    let (m, rem) = (rem / 60_000, rem % 60_000);
    let (s, tenth) = (rem / 1000, rem % 1000 / 100);
    let secs = if tenth > 0 {
        format!("{s}.{tenth}s")
    } else {
        format!("{s}s")
    };
    match (h, m) {
        (0, 0) => secs,
        (0, m) => format!("{m}m{secs}"),
        (h, m) => format!("{h}h{m}m{secs}"),
    }
}

/// Lit un tube jusqu'au bout dans un tampon partagé (lisible même si la
/// lecture est abandonnée en route).
fn drain<R: tokio::io::AsyncRead + Unpin + Send + 'static>(
    mut r: R,
) -> (Arc<Mutex<Vec<u8>>>, tokio::task::JoinHandle<()>) {
    let buf = Arc::new(Mutex::new(Vec::new()));
    let b = buf.clone();
    let h = tokio::spawn(async move {
        let mut chunk = [0u8; 8192];
        while let Ok(n) = r.read(&mut chunk).await {
            if n == 0 {
                break;
            }
            b.lock()
                .unwrap_or_else(|e| e.into_inner())
                .extend_from_slice(&chunk[..n]);
        }
    });
    (buf, h)
}

/// La commande du shell de la plateforme. Le jeton de l'API interne n'a rien
/// à faire dans le shell de l'agent.
pub(crate) fn shell_cmd(command: &str) -> tokio::process::Command {
    #[cfg(windows)]
    let mut c = {
        let mut c = tokio::process::Command::new("cmd");
        c.arg("/C").arg(command);
        c.creation_flags(0x0800_0000); // CREATE_NO_WINDOW : pas de flash de console
        c
    };
    #[cfg(not(windows))]
    let mut c = {
        let mut c = tokio::process::Command::new("/bin/bash");
        c.arg("-c").arg(command);
        c
    };
    c.env_remove(crate::auth::INTERNAL_ENV);
    c
}

/// `runShell` : « exit: N · durée », puis stdout et stderr nettoyés.
pub async fn run_shell(cwd: &Path, memory_dir: &Path, command: &str, timeout_sec: i64) -> String {
    if let Some(msg) = guard_tool_only_command(command, memory_dir) {
        return msg;
    }
    let timeout_sec = match timeout_sec {
        t if t <= 0 => TOOL_DEFAULT_TIMEOUT,
        t => t.min(TOOL_MAX_TIMEOUT),
    };
    let (command, keep_lines) = split_tail_pipe(command);
    let start = Instant::now();
    let mut cmd = shell_cmd(&command);
    cmd.stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(true);
    // Dans le dossier de la discussion, recréé au besoin (l'utilisateur ou le
    // modèle a pu le supprimer) ; à défaut, là où on peut.
    if !cwd.as_os_str().is_empty() && std::fs::create_dir_all(cwd).is_ok() {
        cmd.current_dir(cwd);
    }
    let mut child = match cmd.spawn() {
        Ok(c) => c,
        Err(e) => return format!("[erreur: {e}]"),
    };
    let (out_buf, out_h) = drain(child.stdout.take().expect("stdout"));
    let (err_buf, err_h) = drain(child.stderr.take().expect("stderr"));
    let deadline = Duration::from_secs(timeout_sec as u64);
    let (status, timed_out) = match tokio::time::timeout(deadline, child.wait()).await {
        Ok(st) => (st.ok(), false),
        Err(_) => {
            let _ = child.start_kill();
            (child.wait().await.ok(), true)
        }
    };
    // Tubes encore tenus par un petit-enfant : WAIT_DELAY, puis on lâche.
    let pipes_closed = tokio::time::timeout(WAIT_DELAY, async {
        let _ = out_h.await;
        let _ = err_h.await;
    })
    .await
    .is_ok();
    let take = |b: &Arc<Mutex<Vec<u8>>>| {
        String::from_utf8_lossy(&collapse_cr(&b.lock().unwrap_or_else(|e| e.into_inner())))
            .into_owned()
    };
    let clean = |s: &str| tail_output(&strip_ansi(s));
    let mut out = take(&out_buf);
    if keep_lines > 0 {
        out = tail_lines(&out, keep_lines);
    }
    let out = clean(&out);
    let err_out = clean(&take(&err_buf));
    if timed_out {
        let mut msg = format!(
            "[timeout après {timeout_sec}s — commande arrêtée ; pour un serveur ou une tâche longue, utilise bash_bg]"
        );
        if !out.is_empty() {
            msg.push_str(&format!("\n\nstdout (partiel):\n{out}"));
        }
        if !err_out.is_empty() {
            msg.push_str(&format!("\n\nstderr (partiel):\n{err_out}"));
        }
        return msg;
    }
    let exit = match status {
        Some(st) => st.code().unwrap_or(-1),
        None => return "[erreur: attente du process impossible]".into(),
    };
    // Go : sortie 0 mais tubes jamais fermés → ErrWaitDelay.
    if exit == 0 && !pipes_closed {
        return "[erreur: exec: WaitDelay expired before I/O complete]".into();
    }
    let mut parts = vec![format!("exit: {exit} · {}", go_duration(start.elapsed()))];
    if !out.is_empty() {
        parts.push(format!("stdout:\n{out}"));
    }
    if !err_out.is_empty() {
        parts.push(format!("stderr:\n{err_out}"));
    }
    parts.join("\n\n")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn tail_final_retire() {
        assert_eq!(split_tail_pipe("make | tail -20"), ("make".into(), 20));
        assert_eq!(split_tail_pipe("make | tail -n 5 "), ("make".into(), 5));
        assert_eq!(split_tail_pipe("make|tail"), ("make".into(), 10));
        assert_eq!(split_tail_pipe("| tail -3"), ("| tail -3".into(), 0));
        assert_eq!(split_tail_pipe("tail -f x"), ("tail -f x".into(), 0));
    }

    #[test]
    fn ansi_et_arriere_plan() {
        assert_eq!(
            strip_ansi("\x1b[31mrouge\x1b[0m \x1b]0;titre\x07ok"),
            "rouge ok"
        );
        assert!(trailing_background("serveur &") && !trailing_background("a && b"));
    }

    #[test]
    fn durees_comme_go() {
        for (ms, want) in [
            (0, "0s"),
            (49, "0s"),
            (50, "100ms"),
            (340, "300ms"),
            (1_250, "1.3s"),
            (12_000, "12s"),
            (65_400, "1m5.4s"),
            (3_600_000, "1h0m0s"),
        ] {
            assert_eq!(go_duration(Duration::from_millis(ms)), want, "{ms}");
        }
    }

    #[test]
    fn sortie_bornee() {
        assert_eq!(tail_lines("a\nb\nc\n", 2), "b\nc");
        // Courte : inchangée.
        let court = "un\ndeux\n".repeat(10);
        assert_eq!(tail_output(&court), court);
        let pile = "y".repeat(TOOL_MAX_OUTPUT);
        assert_eq!(tail_output(&pile), pile);
        // Longue : début et fin, lignes entières, compte exact des omises.
        let s: String = (1..=3000).map(|i| format!("{i}\n")).collect();
        let t = tail_output(&s);
        let (head, rest) = t.split_once("… ").unwrap();
        let (marker, tail) = rest.split_once(" …\n").unwrap();
        assert!(
            head.starts_with("1\n2\n") && head.ends_with('\n'),
            "{head:?}"
        );
        assert!(tail.ends_with("2999\n3000\n"), "{tail:?}");
        let first_kept: usize = tail.lines().next().unwrap().parse().unwrap();
        let last_head: usize = head.lines().last().unwrap().parse().unwrap();
        assert_eq!(
            marker,
            format!("{} lignes omises", first_kept - last_head - 1)
        );
        assert!(t.chars().count() <= TOOL_MAX_OUTPUT + 40);
        assert!(head.chars().count() <= HEAD_SHARE && head.chars().count() > HEAD_SHARE / 2);
    }

    #[test]
    fn sortie_bornee_sans_lignes_ni_coupe_de_caractere() {
        // Une seule ligne géante en caractères multi-octets : coupe sur un
        // bord de caractère, au quart et aux trois quarts du budget.
        let s = "é€".repeat(TOOL_MAX_OUTPUT);
        let t = tail_output(&s);
        let (head, tail) = t.split_once("\n… 1 ligne omise …\n").unwrap();
        assert_eq!(head.chars().count(), HEAD_SHARE);
        assert_eq!(tail.chars().count(), TOOL_MAX_OUTPUT - HEAD_SHARE);
        assert!(head.starts_with('é') && tail.ends_with('€'));
        // Multi-octets aux bords des lignes coupées.
        let s: String = (0..2000).map(|i| format!("ligne {i} — ç\n")).collect();
        let t = tail_output(&s);
        assert!(t.contains(" lignes omises …\nligne "), "{}", &t[..200]);
        assert!(t.ends_with("ligne 1999 — ç\n"));
    }

    #[test]
    fn barres_de_progression_repliees() {
        let b = b"debut\n 10%\r 50%\r100%\nfin\r\nok\r\n";
        assert_eq!(&*collapse_cr(b), b"debut\n100%\nfin\r\nok\r\n");
        // Dernier dessin non vide, \r final sans saut de ligne retiré.
        assert_eq!(&*collapse_cr(b"a\rb\r\r\nc\rd\r"), b"b\r\nd");
        assert_eq!(&*collapse_cr(b"rien\n"), b"rien\n");
        // Sur les octets : un \r ne coupe jamais un caractère multi-octets.
        let b = "étape 1\rétape 2 — fini\n".as_bytes();
        assert_eq!(&*collapse_cr(b), "étape 2 — fini\n".as_bytes());
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn commande_et_delai() {
        let d = tempfile::tempdir().unwrap();
        let mem = d.path().join("memory");
        let r = run_shell(d.path(), &mem, "echo un; echo deux >&2; exit 3", 5).await;
        assert!(r.starts_with("exit: 3 · "), "{r}");
        assert!(r.ends_with("stdout:\nun\n\n\nstderr:\ndeux\n"), "{r:?}");
        let r = run_shell(d.path(), &mem, "printf 'a\\nb\\nc\\n' | tail -2", 5).await;
        assert!(r.ends_with("stdout:\nb\nc"), "{r:?}");
        let r = run_shell(d.path(), &mem, "printf '1%%\\r50%%\\r100%%\\n'", 5).await;
        assert!(r.ends_with("stdout:\n100%\n"), "{r:?}");
        let r = run_shell(d.path(), &mem, "pwd", 5).await;
        assert!(r.contains(&d.path().display().to_string()), "{r}");
        let r = run_shell(d.path(), &mem, "echo avant; sleep 5", 1).await;
        assert!(r.starts_with("[timeout après 1s"), "{r}");
        assert!(r.contains("stdout (partiel):\navant"), "{r}");
        let r = run_shell(d.path(), &mem, &format!("ls {}", mem.display()), 5).await;
        assert!(r.starts_with("[refusé] pas d'accès direct"), "{r}");
    }

    #[test]
    fn jeton_interne_absent_du_shell() {
        let c = shell_cmd("env");
        assert!(
            c.as_std()
                .get_envs()
                .any(|(k, v)| k == crate::auth::INTERNAL_ENV && v.is_none())
        );
    }
}
