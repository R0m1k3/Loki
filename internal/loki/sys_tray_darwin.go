//go:build darwin

package loki

// sys_tray_darwin.go — icône de Loki dans la barre de menus macOS (en haut à
// droite, à côté de l'horloge), pendant exact du system tray Windows : quand on
// ouvre Loki.app, l'UI web s'ouvre dans le navigateur ET l'icône apparaît, avec
// le menu « Ouvrir Loki » / « Quitter ».
//
// Contrairement à Windows, systray passe ici par Cocoa → CGO obligatoire : les
// binaires macOS sont donc compilés sur un runner macOS (voir release.yml), et
// le bundle est marqué LSUIElement (pas d'icône dans le Dock, uniquement la
// barre de menus).

import (
	"os"

	"github.com/getlantern/systray"
)

// runTray fait tourner l'icône de la barre de menus. Bloque jusqu'à « Quitter ».
// À appeler depuis la goroutine principale : Cocoa exige le thread main.
func runTray(url string) {
	systray.Run(func() {
		// Icône « template » : macOS n'en garde que la forme (couche alpha) et la
		// colore selon le thème. Indispensable depuis que la marque est noire —
		// une icône noire « en dur » est illisible sur une barre de menus sombre.
		systray.SetTemplateIcon(brandTemplatePNG(trayIconSize), BrandIconPNG(trayIconSize))
		systray.SetTooltip("Loki — votre IA locale")
		mOpen := systray.AddMenuItem("Ouvrir Loki", "Ouvrir l'interface")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Quitter", "Arrêter Loki")

		go func() {
			for {
				select {
				case <-mOpen.ClickedCh:
					_ = openBrowser(url)
				case <-mQuit.ClickedCh:
					systray.Quit()
					return
				}
			}
		}()
	}, func() {
		// « Quitter » arrête l'application : l'interface et, avec elle, le
		// tunnel d'accès distant qui tourne dans ce process.
		//
		// Le MOTEUR, lui, n'est pas arrêté : sous macOS c'est un daemon launchd,
		// que seul root peut piloter. Fermer une interface n'a pas à arrêter un
		// service système. Sous Windows, où l'app est propriétaire du moteur,
		// « Quitter » l'arrête bel et bien (voir sys_tray_windows.go).
		os.Exit(0)
	})
}
