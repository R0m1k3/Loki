//! Parité des outils git avec le Go (lot 7) : `tests/parity_tools/git.json`
//! (`TestPariteOutilsGit`), rejoué dans `tools::exec_local`. Binaire à part :
//! la configuration de git de la machine est écartée par des variables
//! d'environnement, posées avant tout autre fil.

mod common;

#[tokio::test]
async fn outils_git_comme_le_go() {
    if std::process::Command::new("git")
        .arg("--version")
        .output()
        .is_err()
    {
        eprintln!("git absent : test sauté");
        return;
    }
    // SAFETY : seul test de ce binaire, rien d'autre ne lit l'environnement.
    unsafe {
        std::env::set_var(
            "GIT_CONFIG_GLOBAL",
            if cfg!(windows) { "NUL" } else { "/dev/null" },
        );
        std::env::set_var("GIT_CONFIG_NOSYSTEM", "1");
    }
    let diffs = common::toolsfx::replay("git").await;
    assert!(
        diffs.is_empty(),
        "{} écart(s) avec le Go :\n\n{}",
        diffs.len(),
        diffs.join("\n\n")
    );
}
