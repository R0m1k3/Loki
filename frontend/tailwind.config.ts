import type { Config } from "tailwindcss";

/**
 * Système de design « Loki » — extrait fidèlement du visuel Atelier_Agent.
 * Thème sombre chaleureux (café / ambre).
 */
export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        // Fonds
        base: "#1a1714",
        bar: "#1d1a15",
        panel: "#1c1814",
        sunken: "#161310",
        // Surfaces / cartes
        card: "#211d18",
        "card-soft": "#221e18",
        "card-deep": "#1f1b16",
        // Bordures
        line: "#2c2720",
        "line-soft": "#262219",
        "line-strong": "#322d26",
        // Accent ambre
        accent: "#f0a15c",
        "accent-1": "#f4ad6c",
        "accent-2": "#df7c4c",
        // Statuts
        ok: "#86c79a",
        info: "#5b8fb0",
        warn: "#c98b5b",
        // Textes
        ink: "#efe9df",
        "ink-2": "#cfc5b6",
        "ink-3": "#bdb3a4",
        muted: "#9a9082",
        "muted-2": "#8a8175",
        "muted-3": "#766d60",
        "muted-4": "#6a6256",
        label: "#7a7164",
      },
      fontFamily: {
        sans: ['"Hanken Grotesk"', "system-ui", "sans-serif"],
        mono: ['"JetBrains Mono"', "ui-monospace", "monospace"],
      },
      borderRadius: {
        card: "14px",
      },
      boxShadow: {
        frame: "0 24px 60px rgba(0,0,0,.35)",
      },
      backgroundImage: {
        "accent-grad": "linear-gradient(140deg, #f4ad6c, #df7c4c)",
      },
    },
  },
  plugins: [],
} satisfies Config;
