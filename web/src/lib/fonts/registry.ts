// TEMPORARY: offline build fallback — uses local Geist fonts instead of Google Fonts.
// Restore registry.ts.bak after build.
import localFont from "next/font/local";
import { GeistPixelSquare } from "geist/font/pixel";

// Reuse 2 localFont definitions instead of 17 duplicate font-face rules/preloads.
const geist = localFont({ src: "../../../node_modules/geist/dist/fonts/geist-sans/Geist-Variable.woff2", variable: "--font-geist" });
const geistMono = localFont({ src: "../../../node_modules/geist/dist/fonts/geist-mono/GeistMono-Variable.woff2", variable: "--font-geist-mono" });

export const fontRegistry = {
  geist: { label: "Geist", font: geist },
  inter: { label: "Inter", font: geist },
  notoSans: { label: "Noto Sans", font: geist },
  nunitoSans: { label: "Nunito Sans", font: geist },
  figtree: { label: "Figtree", font: geist },
  roboto: { label: "Roboto", font: geist },
  raleway: { label: "Raleway", font: geist },
  dmSans: { label: "DM Sans", font: geist },
  publicSans: { label: "Public Sans", font: geist },
  outfit: { label: "Outfit", font: geist },
  geistMono: { label: "Geist Mono", font: geistMono },
  geistPixelSquare: { label: "Geist Pixel Square", font: GeistPixelSquare },
  jetBrainsMono: { label: "JetBrains Mono", font: geistMono },
  notoSerif: { label: "Noto Serif", font: geist },
  robotoSlab: { label: "Roboto Slab", font: geist },
  merriweather: { label: "Merriweather", font: geist },
  lora: { label: "Lora", font: geist },
  playfairDisplay: { label: "Playfair Display", font: geist },
} as const;

export type FontKey = keyof typeof fontRegistry;

export const fontVars = Array.from(
  new Set((Object.values(fontRegistry) as Array<(typeof fontRegistry)[FontKey]>).map((f) => f.font.variable)),
).join(" ");

export const fontOptions = (Object.entries(fontRegistry) as [FontKey, { label: string; font: { variable: string } }][]).map(
  ([key, f]) => ({
    key,
    label: f.label,
    variable: f.font.variable,
  }),
);
