//go:build linux

package loki

// sys_disk_fs_linux.go — l'espace libre annoncé par le système est-il DIGNE DE
// FOI pour décider d'un téléchargement ?
//
// Sur Unraid, les partages vivent sous /mnt/user, servi par shfs — un système
// de fichiers FUSE qui agrège plusieurs disques et le cache. statfs(2) n'y
// renvoie pas l'espace du partage : selon la configuration, c'est celui d'UN
// disque, ou du pool de cache. Vécu : « il ne reste que 11,9 Go » sur une
// machine qui en a des centaines de libres, et le téléchargement du modèle
// REFUSÉ sur cette base.
//
// Même problème sur les montages réseau (NFS, CIFS) et sur la couche
// d'écriture d'un conteneur (overlayfs), qui rapporte le disque virtuel Docker
// et non le volume monté dessous.
//
// On ne cherche pas à corriger le chiffre — c'est impossible depuis l'intérieur
// du conteneur. On sait juste dire « ce chiffre est indicatif », et on cesse
// alors de bloquer sur lui.

import "syscall"

// Magies de systèmes de fichiers dont statfs ment (ou approxime) sur l'espace
// libre. Valeurs de <linux/magic.h>.
const (
	fuseSuperMagic    = 0x65735546 // FUSE — shfs d'Unraid, sshfs, rclone…
	overlayfsMagic    = 0x794c7630 // couche d'écriture d'un conteneur
	nfsSuperMagic     = 0x6969
	smbSuperMagic     = 0x517b     // CIFS/SMB1
	smb2MagicNumber   = 0xfe534d42 // CIFS/SMB2+
	cephSuperMagic    = 0x00c36400
	glusterfsSuperMag = 0x58465342
)

// diskFreeReliable dit si l'espace libre rapporté pour dir peut servir à
// REFUSER une écriture. false = on affiche le chiffre, mais on ne bloque plus.
func diskFreeReliable(dir string) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return false
	}
	switch uint32(st.Type) {
	case fuseSuperMagic, overlayfsMagic, nfsSuperMagic,
		smbSuperMagic, smb2MagicNumber, cephSuperMagic, glusterfsSuperMag:
		return false
	}
	return true
}
