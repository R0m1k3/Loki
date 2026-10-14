//! Arbre de processus du moteur — port de `sys_proctree_linux.go`.
//!
//! Un processus est repéré par son PID ET son instant de démarrage : un PID
//! recyclé pendant l'attente ne passe pas pour le même processus.

use std::collections::{HashMap, HashSet};

/// Les champs de `/proc/<pid>/stat` utiles ici.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ProcStat {
    pub state: String,
    pub ppid: i32,
    pub pgrp: i32,
    pub sid: i32,
    pub start: String,
}

#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub struct ProcRef {
    pub pid: i32,
    pub start: String,
}

/// Lit une ligne de `/proc/<pid>/stat`. Le nom (champ 2, entre parenthèses)
/// peut contenir des blancs et des parenthèses : on repart de la DERNIÈRE
/// fermante.
pub fn parse_proc_stat(s: &str) -> Option<ProcStat> {
    let i = s.rfind(')')?;
    // f[0] = champ 3 (état), f[1] ppid, f[2] groupe, f[3] session,
    // f[19] instant de démarrage (champ 22).
    let f: Vec<&str> = s[i + 1..].split_whitespace().collect();
    if f.len() < 20 {
        return None;
    }
    Some(ProcStat {
        state: f[0].to_string(),
        ppid: f[1].parse().unwrap_or(0),
        pgrp: f[2].parse().unwrap_or(0),
        sid: f[3].parse().unwrap_or(0),
        start: f[19].to_string(),
    })
}

/// Le chef `root` et tout ce qui en descend : sa session et son groupe (il est
/// lancé chef de session), plus les descendants par la filiation, pour un
/// enfant qui aurait pris sa propre session (le serveur Python de Strata lance
/// son moteur dans un processus à part). À relever AVANT tout signal : une
/// fois le chef mort, ses orphelins sont rattachés à PID 1 et la filiation est
/// perdue. Les PID de `exclude` (l'appelant) n'en font jamais partie.
pub fn proc_tree_of(table: &HashMap<i32, ProcStat>, root: i32, exclude: &[i32]) -> Vec<ProcRef> {
    let mut inside: HashSet<i32> = table
        .iter()
        .filter(|(pid, st)| {
            !exclude.contains(pid) && (**pid == root || st.sid == root || st.pgrp == root)
        })
        .map(|(pid, _)| *pid)
        .collect();
    // Fermeture par la filiation, jusqu'à ce que plus rien ne s'ajoute.
    loop {
        let more: Vec<i32> = table
            .iter()
            .filter(|(pid, st)| {
                !inside.contains(pid) && !exclude.contains(pid) && inside.contains(&st.ppid)
            })
            .map(|(pid, _)| *pid)
            .collect();
        if more.is_empty() {
            break;
        }
        inside.extend(more);
    }
    let mut refs: Vec<ProcRef> = inside
        .into_iter()
        .map(|pid| ProcRef {
            pid,
            start: table[&pid].start.clone(),
        })
        .collect();
    refs.sort_by_key(|r| r.pid);
    refs
}

#[cfg(target_os = "linux")]
pub mod linux {
    use super::*;
    use std::time::{Duration, Instant};

    pub fn read_proc_stat(pid: i32) -> Option<ProcStat> {
        let s = std::fs::read_to_string(format!("/proc/{pid}/stat")).ok()?;
        parse_proc_stat(&s)
    }

    /// Tous les processus vivants (un zombie a fini de tourner : sa mémoire,
    /// VRAM comprise, est déjà rendue).
    pub fn proc_table() -> HashMap<i32, ProcStat> {
        let Ok(rd) = std::fs::read_dir("/proc") else {
            return HashMap::new();
        };
        rd.flatten()
            .filter_map(|e| e.file_name().to_str()?.parse::<i32>().ok())
            .filter_map(|pid| read_proc_stat(pid).map(|st| (pid, st)))
            .filter(|(_, st)| st.state != "Z")
            .collect()
    }

    /// Ni disparu, ni zombie, ni remplacé par un autre sous le même PID.
    pub fn ref_alive(r: &ProcRef) -> bool {
        read_proc_stat(r.pid).is_some_and(|st| st.state != "Z" && st.start == r.start)
    }

    pub fn refs_alive(refs: &[ProcRef]) -> Vec<ProcRef> {
        refs.iter().filter(|r| ref_alive(r)).cloned().collect()
    }

    fn kill(pid: i32, sig: i32) -> bool {
        // SAFETY: kill(2) n'a pas d'effet mémoire ; un PID absent rend -1.
        unsafe { libc::kill(pid, sig) == 0 }
    }

    /// Le processus existe et n'est pas un zombie.
    pub fn pid_alive(pid: i32) -> bool {
        pid > 0 && kill(pid, 0) && read_proc_stat(pid).is_some_and(|st| st.state != "Z")
    }

    /// Arrête le service lancé chef de session (`pid`) et N'EN REVIENT QU'UNE
    /// FOIS TOUT L'ARBRE PARTI. Attendre le seul chef ne suffit pas : avec
    /// Strata, le moteur qui tient la VRAM finit de s'arrêter après lui, et la
    /// relance qui suivait démarrait sur une carte encore pleine.
    ///
    /// SIGTERM à tous, `grace` pour finir proprement, SIGKILL aux survivants,
    /// puis au plus `settle` pour que le noyau les ait retirés. Renvoie ceux
    /// qui tournent encore au-delà.
    pub fn stop_proc_tree(
        pid: i32,
        exclude: &[i32],
        grace: Duration,
        settle: Duration,
    ) -> Vec<ProcRef> {
        let refs = proc_tree_of(&proc_table(), pid, exclude);
        if refs.is_empty() {
            return Vec::new();
        }
        if !kill(-pid, libc::SIGTERM) {
            kill(pid, libc::SIGTERM);
        }
        for r in &refs {
            kill(r.pid, libc::SIGTERM);
        }
        if wait_refs(&refs, grace).is_empty() {
            return Vec::new();
        }
        kill(-pid, libc::SIGKILL);
        for r in refs_alive(&refs) {
            kill(r.pid, libc::SIGKILL);
        }
        wait_refs(&refs, settle)
    }

    /// Attend que tous aient disparu, au plus `wait`. Renvoie les survivants.
    pub fn wait_refs(refs: &[ProcRef], wait: Duration) -> Vec<ProcRef> {
        let deadline = Instant::now() + wait;
        loop {
            let left = refs_alive(refs);
            if left.is_empty() || Instant::now() >= deadline {
                return left;
            }
            std::thread::sleep(Duration::from_millis(100));
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn st(ppid: i32, pgrp: i32, sid: i32) -> ProcStat {
        ProcStat {
            state: "S".into(),
            ppid,
            pgrp,
            sid,
            start: "42".into(),
        }
    }

    #[test]
    fn stat_avec_parentheses_dans_le_nom() {
        let line = "1234 (python3 (serve) x) S 1 1234 1234 0 -1 4194560 0 0 0 0 \
                    0 0 0 0 20 0 3 0 987654 0 0";
        let s = parse_proc_stat(line).unwrap();
        assert_eq!(s.state, "S");
        assert_eq!((s.ppid, s.pgrp, s.sid), (1, 1234, 1234));
        assert_eq!(s.start, "987654");
        assert!(parse_proc_stat("12 (court) S 1 2").is_none());
        assert!(parse_proc_stat("sans parenthèse").is_none());
    }

    #[test]
    fn arbre_session_groupe_et_filiation() {
        let mut t = HashMap::new();
        t.insert(100, st(1, 100, 100)); // chef (loki serve, chef de session)
        t.insert(101, st(100, 100, 100)); // llama-server / serveur Strata
        t.insert(200, st(101, 200, 200)); // moteur Strata : sa propre session
        t.insert(201, st(200, 200, 200)); // et son enfant
        t.insert(300, st(1, 300, 300)); // étranger
        t.insert(102, st(100, 100, 100)); // l'appelant, dans l'arbre : exclu
        let pids: Vec<i32> = proc_tree_of(&t, 100, &[102])
            .iter()
            .map(|r| r.pid)
            .collect();
        assert_eq!(pids, vec![100, 101, 200, 201]);
        assert!(proc_tree_of(&t, 999, &[]).is_empty());
    }
}
