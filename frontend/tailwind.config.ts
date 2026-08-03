import type { Config } from "tailwindcss";

/**
 * Système de design « Loki » — thème Nocturne clair (Claude Design).
 *
 * Reprend les jetons de `Loki App.dc.html` : fond gris froid, surfaces
 * blanches, filets 1 px, accent bleu ardoise et ombres douces diffuses.
 * Remplace le thème néo-brutaliste (bordures noires 3 px, ombres dures
 * décalées, accent orange, police pixel).
 *
 * Les NOMS de jetons sont inchangés : seule leur valeur bascule, ce qui fait
 * suivre toute l'interface sans réécrire les composants. Les jetons
 * historiquement « sombres » (card-deep, on-dark…) deviennent la surface
 * sélectionnée claire et son encre, conformément à la maquette.
 */
export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        // Fonds
        base: "#f7f8fa", // --color-bg
        bar: "#ffffff", // barres : surface + filet (plus de chrome sombre)
        panel: "#ffffff", // --color-surface
        sunken: "#f1f3f6", // encarts (code, invite système) : neutral-900
        // Surfaces / cartes
        card: "#ffffff",
        "card-soft": "#f7f8fa",
        "card-deep": "#eef2f9", // sélection : accent-900
        // Filets
        line: "#e3e6eb", // --color-divider
        "line-soft": "#e6e9ee", // neutral-800
        "line-strong": "#c3c9d2", // neutral-700
        // Accent bleu ardoise
        accent: "#2f4a7a", // --color-accent
        "accent-1": "#4a6797", // accent-600
        "accent-2": "#26406b", // accent-300
        "accent-ghost": "#eef2f9", // accent-900 (fonds teintés)
        "accent-line": "#b9c6dd", // accent-700 (bordure de sélection)
        // Statuts, accordés à la palette
        ok: "#2f7a5a",
        info: "#2f4a7a",
        warn: "#b3403a",
        // Encres
        ink: "#1c2536", // --color-text / neutral-100
        "ink-2": "#29334a", // neutral-200
        "ink-3": "#3d4a5e", // neutral-300
        muted: "#55627a", // neutral-400
        "muted-2": "#6b7686", // neutral-500
        "muted-3": "#8b95a4", // neutral-600
        "muted-4": "#8b95a4",
        label: "#8b95a4", // couleur des « kickers »
        // Anciennes teintes « sur fond sombre » -> encres sur surface claire
        "chrome-2": "#e3e6eb",
        "chrome-3": "#c3c9d2",
        "on-dark": "#1c2536",
        "on-dark-2": "#3d4a5e",
        "on-dark-3": "#6b7686",
      },
      fontFamily: {
        sans: ['"Inter"', "ui-sans-serif", "system-ui", "sans-serif"],
        mono: ["ui-monospace", '"SF Mono"', "Menlo", "monospace"],
        // Ex-police pixel : devient le « kicker » de la maquette — petites
        // capitales très espacées. Les libellés existants suivent sans edit.
        pixel: [
          '"Inter"',
          { letterSpacing: "0.14em", fontWeight: "500" },
        ],
      },
      borderRadius: {
        card: "10px", // --radius-md
      },
      boxShadow: {
        // Ombres douces diffuses (--shadow-sm/md/lg) au lieu des ombres dures.
        "hard-sm": "0 1px 2px rgba(28, 37, 54, 0.06)",
        hard: "0 2px 8px rgba(28, 37, 54, 0.08)",
        "hard-lg": "0 8px 28px rgba(28, 37, 54, 0.10)",
        "hard-accent": "0 2px 8px rgba(47, 74, 122, 0.18)",
        "accent-soft": "0 1px 2px rgba(47, 74, 122, 0.14)",
        frame: "0 2px 8px rgba(28, 37, 54, 0.08)",
      },
      backgroundImage: {
        "accent-grad": "linear-gradient(140deg, #4a6797, #2f4a7a)",
      },
    },
  },
  plugins: [],
} satisfies Config;
