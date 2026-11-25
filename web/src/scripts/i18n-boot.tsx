/**
 * Boot script that detects the active UI locale early in <head> and loads
 * non-English translation dictionaries on demand before app bundles evaluate.
 *
 * English ("en") is the source key itself and needs zero bytes transferred.
 * For "tr" and "zh-CN", the pre-compressed JSON dictionary in /locales/<lang>.json
 * is fetched once into window.__ARTEX_I18N__ so synchronous module-level t()
 * calls work without bundling 442 KB of unused JSON into every JS chunk.
 */
export function I18nBootScript() {
  const code = `
    (function () {
      try {
        var SUPPORTED = ["tr", "en", "zh-CN"];
        var locale = "en";
        try {
          var saved = window.localStorage.getItem("artex.locale");
          if (saved && SUPPORTED.indexOf(saved) !== -1) {
            locale = saved;
          } else {
            var langs = window.navigator.languages || (window.navigator.language ? [window.navigator.language] : []);
            for (var i = 0; i < langs.length; i++) {
              var tag = (langs[i] || "").toLowerCase();
              var base = tag.split("-")[0];
              var hit = null;
              for (var j = 0; j < SUPPORTED.length; j++) {
                if (SUPPORTED[j].toLowerCase() === tag) { hit = SUPPORTED[j]; break; }
              }
              if (!hit) {
                for (var k = 0; k < SUPPORTED.length; k++) {
                  if (SUPPORTED[k].toLowerCase().split("-")[0] === base) { hit = SUPPORTED[k]; break; }
                }
              }
              if (hit) { locale = hit; break; }
            }
          }
        } catch (e) {}
        document.documentElement.lang = locale;
        if (locale !== "en") {
          var xhr = new XMLHttpRequest();
          xhr.open("GET", "/locales/" + encodeURIComponent(locale) + ".json", false);
          xhr.send(null);
          if (xhr.status >= 200 && xhr.status < 300 && xhr.responseText) {
            window.__ARTEX_I18N__ = window.__ARTEX_I18N__ || {};
            window.__ARTEX_I18N__[locale] = JSON.parse(xhr.responseText);
          }
        }
      } catch (e) {}
    })();
  `;

  // biome-ignore lint/security/noDangerouslySetInnerHtml: static boot script mirroring ThemeBootScript
  return <script dangerouslySetInnerHTML={{ __html: code }} />;
}
