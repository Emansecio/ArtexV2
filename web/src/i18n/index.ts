// UI translation layer.
//
// The English source text is the key: t("System logs") looks the string up in the active
// locale's dictionary and falls back to the key itself, so a missing entry shows English
// instead of breaking. Placeholders use {name}: t("{count} lines", { count: 3 }).
//
// The locale is read once when this module loads and never changes while a page is open;
// setLocale() stores the choice and reloads. That keeps t() usable anywhere (components,
// module constants, toasts, non-React helpers) without hooks or re-render plumbing.
//
// To translate the UI into another language, add locales/<code>.json and register it below.
// Dictionaries are served on demand from /locales/<code>.json by I18nBootScript so
// English sessions pay 0 bytes for unused translations.

export type Locale = "pt-BR" | "en" | "tr" | "zh-CN";

// Used when nothing is saved and the browser language is not one of LOCALES,
// and for the static prerender, which has no browser.
export const DEFAULT_LOCALE: Locale = "en";

export const LOCALES: { code: Locale; label: string }[] = [
  { code: "pt-BR", label: "Português (BR)" },
  { code: "en", label: "English" },
  { code: "tr", label: "Türkçe" },
  { code: "zh-CN", label: "简体中文" },
];

const STORAGE_KEY = "artex.locale";

const INTL_LOCALES: Record<Locale, string> = {
  "pt-BR": "pt-BR",
  en: "en-US",
  tr: "tr-TR",
  "zh-CN": "zh-CN",
};

function readLocaleCookie(): string | null {
  if (typeof document === "undefined") return null;
  try {
    const match = document.cookie.split("; ").find((row) => row.startsWith(`${STORAGE_KEY}=`));
    return match ? decodeURIComponent(match.slice(STORAGE_KEY.length + 1)) : null;
  } catch {
    return null;
  }
}

function writeLocaleCookie(value: Locale) {
  if (typeof document === "undefined") return;
  try {
    // biome-ignore lint/suspicious/noDocumentCookie: dual persistence backup alongside localStorage
    document.cookie = `${STORAGE_KEY}=${encodeURIComponent(value)}; path=/; max-age=31536000; SameSite=Lax`;
  } catch {
    /* ignore */
  }
}

function loadRuntimeDict(active: Locale): Record<string, string> {
  if (active === "en" || typeof window === "undefined") return {};
  const win = window as unknown as { __ARTEX_I18N__?: Record<string, Record<string, string>> };
  const existing = win.__ARTEX_I18N__?.[active];
  if (existing && typeof existing === "object") return existing;
  // Self-healing fallback: if an async JS chunk evaluated before I18nBootScript
  // (or after an SPA redirect), load the dictionary synchronously right now so
  // locale === "pt-BR" never renders with an empty dictionary.
  try {
    const xhr = new XMLHttpRequest();
    xhr.open("GET", `/locales/${encodeURIComponent(active)}.json`, false);
    xhr.send(null);
    if (xhr.status >= 200 && xhr.status < 300 && xhr.responseText) {
      const parsed = JSON.parse(xhr.responseText) as Record<string, string>;
      if (parsed && typeof parsed === "object") {
        win.__ARTEX_I18N__ = win.__ARTEX_I18N__ || {};
        win.__ARTEX_I18N__[active] = parsed;
        return parsed;
      }
    }
  } catch {
    /* fall through to English keys */
  }
  return {};
}

function detectLocale(): Locale {
  // Prerendering (static export) has no window: it renders the default locale.
  if (typeof window === "undefined") return DEFAULT_LOCALE;
  try {
    const saved = window.localStorage.getItem(STORAGE_KEY) ?? readLocaleCookie();
    const match = LOCALES.find((l) => l.code === saved);
    if (match) return match.code;
  } catch {
    const saved = readLocaleCookie();
    const match = LOCALES.find((l) => l.code === saved);
    if (match) return match.code;
  }
  for (const lang of window.navigator.languages ?? [window.navigator.language]) {
    const tag = lang?.toLowerCase();
    const base = tag?.split("-")[0];
    // Prefer a full BCP 47 match, then the available locale for that language.
    const match =
      LOCALES.find((l) => l.code.toLowerCase() === tag) ??
      LOCALES.find((l) => l.code.toLowerCase().split("-")[0] === base);
    if (match) return match.code;
  }
  return DEFAULT_LOCALE;
}

export const locale: Locale = detectLocale();

if (typeof document !== "undefined") document.documentElement.lang = locale;

/** BCP 47 tag for Intl / toLocaleString calls, e.g. date.toLocaleString(intlLocale). */
export const intlLocale = INTL_LOCALES[locale];

const dict = loadRuntimeDict(locale);

export type TVars = Record<string, string | number>;

export function t(key: string, vars?: TVars): string {
  const s = Object.prototype.hasOwnProperty.call(dict, key) ? dict[key] : key;
  if (!vars) return s;
  return s.replace(/\{(\w+)\}/g, (m, name: string) =>
    Object.prototype.hasOwnProperty.call(vars, name) ? String(vars[name]) : m,
  );
}

export function setLocale(next: Locale) {
  if (next === locale) return;
  try {
    window.localStorage.setItem(STORAGE_KEY, next);
  } catch {
    /* storage blocked: fall back to cookie / browser-language detection on reload */
  }
  writeLocaleCookie(next);
  window.location.reload();
}
