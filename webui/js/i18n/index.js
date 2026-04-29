// OllamaLegion i18n Manager
// Supports multiple languages, extensible by adding new I18N_XX.js files
(function () {
  'use strict';

  const STORAGE_KEY = 'ollamalegion_lang';
  const DEFAULT_LANG = 'ru';

  // All loaded language packs (populated by external I18N_XX.js files)
  const translations = {};

  let currentLang = localStorage.getItem(STORAGE_KEY) || DEFAULT_LANG;

  /**
   * Register a language pack. Called automatically when I18N_XX.js files set window.I18N_XX.
   * @param {string} lang - Language code (ru, en, etc.)
   * @param {object} pack - Translation key-value pairs
   */
  function register(lang, pack) {
    translations[lang] = pack;
  }

  /**
   * Get a translated string by key.
   * Supports {placeholder} substitution.
   * @param {string} key - Translation key (e.g., "nav.dashboard")
   * @param {object} [params] - Optional placeholder values
   * @returns {string} Translated string or key if not found
   */
  function t(key, params) {
    const pack = translations[currentLang] || translations[DEFAULT_LANG] || {};
    let str = pack[key];
    if (str === undefined || str === null) {
      // Fallback to default language
      const defaultPack = translations[DEFAULT_LANG] || {};
      str = defaultPack[key];
      if (str === undefined || str === null) {
        console.warn('[i18n] Missing translation key:', key);
        return key;
      }
    }
    if (params) {
      Object.keys(params).forEach(function (p) {
        str = str.replace('{' + p + '}', params[p]);
      });
    }
    return str;
  }

  /**
   * Get current language code.
   * @returns {string}
   */
  function getLang() {
    return currentLang;
  }

  /**
   * Set language and persist.
   * @param {string} lang - Language code to switch to
   */
  function setLang(lang) {
    if (!translations[lang]) {
      console.warn('[i18n] Language pack not found:', lang);
      return;
    }
    currentLang = lang;
    localStorage.setItem(STORAGE_KEY, lang);
    document.documentElement.setAttribute('lang', lang);
    // Dispatch event so other modules can react
    window.dispatchEvent(new CustomEvent('i18n:changed', { detail: { lang: lang } }));
  }

  /**
   * Get list of available languages.
   * @returns {Array<{code: string, name: string}>}
   */
  function getAvailableLanguages() {
    const names = {
      ru: 'Русский',
      en: 'English',
    };
    return Object.keys(translations).map(function (code) {
      return { code: code, name: names[code] || code };
    });
  }

  // Auto-register language packs from window globals
  function autoRegister() {
    if (window.I18N_RU) { register('ru', window.I18N_RU); }
    if (window.I18N_EN) { register('en', window.I18N_EN); }
  }

  // Wait for all I18N scripts to load, then auto-register
  if (document.readyState === 'loading') {
    window.addEventListener('DOMContentLoaded', autoRegister);
  } else {
    autoRegister();
  }

  // Also register on i18n:register event for dynamically loaded packs
  window.addEventListener('i18n:register', function (e) {
    if (e.detail && e.detail.lang && e.detail.pack) {
      register(e.detail.lang, e.detail.pack);
    }
  });

  // Expose API
  window.I18N = {
    register: register,
    t: t,
    getLang: getLang,
    setLang: setLang,
    getAvailableLanguages: getAvailableLanguages,
  };

  // Set initial lang attribute
  document.documentElement.setAttribute('lang', currentLang);

  console.log('[i18n] Initialized with language:', currentLang);
})();