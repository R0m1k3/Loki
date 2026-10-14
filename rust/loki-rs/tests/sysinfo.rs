//! Port de `TestStorageFuseHintOutsideContainer` (sys_storagefuse_test.go),
//! étendu à `dataVolumeWarning` qui suit la même règle. Il modifie
//! l'environnement du processus : il vit seul dans ce binaire de test.

use loki_rs::sysinfo::{data_volume_warning, storage_fuse_hint};

#[tokio::test]
async fn hors_conteneur_aucun_conseil() {
    let jamais = || async { panic!("dossiers de modèles lus hors conteneur") };
    // SAFETY : seul test de ce binaire, aucun autre fil ne lit l'environnement.
    unsafe { std::env::set_var("LOKI_CONTAINER", "") };
    let s = storage_fuse_hint(jamais).await;
    assert_eq!(s, "", "hors conteneur, aucun conseil attendu");
    assert_eq!(
        data_volume_warning(),
        "",
        "hors conteneur, aucune alerte attendue"
    );
    if !cfg!(target_os = "linux") {
        // SAFETY : idem.
        unsafe { std::env::set_var("LOKI_CONTAINER", "1") };
        let s = storage_fuse_hint(jamais).await;
        assert_eq!(s, "", "hors Linux, aucun conseil attendu");
        // Hors Linux, /proc/self/mountinfo n'existe pas : rien à signaler.
        assert_eq!(
            data_volume_warning(),
            "",
            "hors Linux, aucune alerte attendue"
        );
    }
}
