import type { Config } from "tailwindcss";

/**
 * Système de design « Loki » — thème néo-brutaliste clair.
 * Bordures noires épaisses, ombres dures décalées, accent orange,
 * police pixel (Press Start 2P) pour les libellés, DM Mono pour le corps.
 */
export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        // Fonds
        base: "#ececef", // contenu principal
        bar: "#18181b", // chrome sombre (barres)
        panel: "#fafafa", // panneaux latéraux
        sunken: "#18181b", // encarts sombres (code, invite système)
        // Surfaces / cartes
        card: "#ffffff",
        "card-soft": "#fafafa",
        "card-deep": "#18181b",
        // Bordures
        line: "#18181b", // bordure noire signature
        "line-soft": "#e4e4e7", // séparateur clair
        "line-strong": "#18181b",
        // Accent orange
        accent: "#ff5436",
        "accent-1": "#ff6b50",
        "accent-2": "#e23e22",
        // Statuts
        ok: "#22c55e",
        info: "#3b82f6",
        warn: "#dc2626",
        // Textes (sombres sur fond clair)
        ink: "#18181b",
        "ink-2": "#27272a",
        "ink-3": "#3f3f46",
        muted: "#52525b",
        "muted-2": "#71717a",
        "muted-3": "#a1a1aa",
        "muted-4": "#a1a1aa",
        label: "#52525b",
        // Teintes claires utiles sur le chrome sombre
        "chrome-2": "#27272a",
        "chrome-3": "#3f3f46",
        "on-dark": "#e4e4e7",
        "on-dark-2": "#a1a1aa",
        "on-dark-3": "#71717a",
      },
      fontFamily: {
        sans: ['"DM Mono"', "ui-monospace", "monospace"],
        mono: ['"DM Mono"', "ui-monospace", "monospace"],
        pixel: ['"Press Start 2P"', "ui-monospace", "monospace"],
      },
      borderRadius: {
        card: "9px",
      },
      boxShadow: {
        "hard-sm": "3px 3px 0 #18181b",
        hard: "4px 4px 0 #18181b",
        "hard-lg": "7px 7px 0 #18181b",
        "hard-accent": "3px 3px 0 #ff5436",
        "accent-soft": "3px 3px 0 rgba(255,84,54,.4)",
        frame: "4px 4px 0 #18181b",
      },
      backgroundImage: {
        "accent-grad": "linear-gradient(140deg, #ff6b50, #e23e22)",
      },
    },
  },
  plugins: [],
} satisfies Config;
