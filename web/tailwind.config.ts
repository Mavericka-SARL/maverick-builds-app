import type { Config } from "tailwindcss";

const config: Config = {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      // ── Brand colours ──────────────────────────────────────────────────────────
      colors: {
        brand: {
          50:  "#eef2ff",
          100: "#e0e7ff",
          200: "#c7d2fe",
          300: "#a5b4fc",
          400: "#818cf8",
          500: "#6366f1",   // primary
          600: "#4f46e5",
          700: "#4338ca",
          800: "#3730a3",
          900: "#312e81",
        },
        success: {
          50:  "#f0fdf4",
          500: "#22c55e",
          700: "#15803d",
        },
        warning: {
          50:  "#fffbeb",
          500: "#f59e0b",
          700: "#b45309",
        },
        danger: {
          50:  "#fef2f2",
          500: "#ef4444",
          700: "#b91c1c",
        },
      },

      // ── Typography ─────────────────────────────────────────────────────────────
      fontFamily: {
        sans: [
          "Inter",
          "ui-sans-serif",
          "system-ui",
          "-apple-system",
          "BlinkMacSystemFont",
          "sans-serif",
        ],
        mono: [
          "JetBrains Mono",
          "ui-monospace",
          "SFMono-Regular",
          "Menlo",
          "monospace",
        ],
      },

      // ── Spacing / layout tokens ────────────────────────────────────────────────
      spacing: {
        // Sidebar widths
        "sidebar-sm": "220px",
        "sidebar":    "260px",
      },

      // ── Border radii ───────────────────────────────────────────────────────────
      borderRadius: {
        card:   "12px",
        button: "6px",
        badge:  "4px",
        input:  "6px",
      },

      // ── Shadows ────────────────────────────────────────────────────────────────
      boxShadow: {
        card: "0 1px 3px rgba(0,0,0,0.08), 0 1px 2px rgba(0,0,0,0.04)",
        "card-hover": "0 4px 12px rgba(0,0,0,0.12)",
        modal: "0 20px 40px rgba(0,0,0,0.15)",
      },
    },
  },
  plugins: [],
};

export default config;
