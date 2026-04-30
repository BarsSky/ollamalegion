// OllamaLegion i18n Manager
// Supports multiple languages, extensible by adding new I18N_XX.js files
(function () {
  'use strict';

  const STORAGE_KEY = 'ollamalegion_lang';
  const DEFAULT_LANG = 'en';

  // All loaded language packs (populated by external I18N_XX.js files)
  const translations = {};

  /**
   * Auto-detect language from multiple sources:
   * 1. localStorage (user preference)
   * 2. navigator.language / navigator.languages
   * 3. Geo heuristic based on timezone (Intl.DateTimeFormat)
   * 4. Fallback: 'en'
   */
  function detectLanguage() {
    // 1. Stored preference
    var stored = localStorage.getItem(STORAGE_KEY);
    if (stored && (stored === 'ru' || stored === 'en')) return stored;

    // 2. Browser language (primary)
    var browserLang = (navigator.language || navigator.userLanguage || '').toLowerCase();
    if (browserLang.startsWith('ru')) return 'ru';
    if (browserLang.startsWith('en')) return 'en';

    // 3. navigator.languages array (fallback order)
    if (navigator.languages && navigator.languages.length) {
      for (var i = 0; i < navigator.languages.length; i++) {
        var lang = navigator.languages[i].toLowerCase();
        if (lang.startsWith('ru')) return 'ru';
        if (lang.startsWith('en')) return 'en';
      }
    }

    // 4. Geo heuristic based on timezone
    try {
      var tz = Intl.DateTimeFormat().resolvedOptions().timeZone || '';
      // European Russian timezones
      if (tz.startsWith('Europe/Moscow') || tz.startsWith('Europe/Kaliningrad') ||
          tz.startsWith('Europe/Samara') || tz === 'Europe/Volgograd' ||
          tz === 'Europe/Kirov' || tz === 'Europe/Saratov' ||
          tz === 'Europe/Ulyanovsk' || tz === 'Europe/Astrakhan') {
        return 'ru';
      }
      // Asian Russian timezones
      if (tz.startsWith('Asia/Yekaterinburg') || tz.startsWith('Asia/Omsk') ||
          tz.startsWith('Asia/Krasnoyarsk') || tz.startsWith('Asia/Novosibirsk') ||
          tz.startsWith('Asia/Irkutsk') || tz.startsWith('Asia/Yakutsk') ||
          tz.startsWith('Asia/Vladivostok') || tz.startsWith('Asia/Magadan') ||
          tz.startsWith('Asia/Kamchatka') || tz.startsWith('Asia/Anadyr') ||
          tz.startsWith('Asia/Chita') || tz.startsWith('Asia/Khandyga') ||
          tz.startsWith('Asia/Ust-Nera') || tz.startsWith('Asia/Sakhalin') ||
          tz.startsWith('Asia/Srednekolymsk')) {
        return 'ru';
      }
    } catch (e) { /* ignore */ }

    // 5. Fallback
    return DEFAULT_LANG;
  }

  var currentLang = detectLanguage();

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
    var pack = translations[currentLang];
    if (!pack) {
      pack = translations['en'] || {};
    }
    var str = pack[key];
    if (str === undefined || str === null) {
      // Fallback to English
      var enPack = translations['en'] || {};
      str = enPack[key];
      if (str === undefined || str === null) {
        console.warn('[i18n] Missing translation key:', key, 'lang:', currentLang);
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
    var names = {
      ru: 'Русский',
      en: 'English',
    };
    return Object.keys(translations).map(function (code) {
      return { code: code, name: names[code] || code };
    });
  }

  // Auto-register language packs from window globals (synchronous, called immediately)
  function autoRegister() {
    if (window.I18N_RU) { register('ru', window.I18N_RU); }
    if (window.I18N_EN) { register('en', window.I18N_EN); }
  }

  // Since language packs are loaded as <script> tags before this file (see index.html),
  // window.I18N_RU and window.I18N_EN are already available. Register immediately.
  autoRegister();

  // Also listen for dynamic registrations
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
    detectLanguage: detectLanguage,
  };

  // Set initial lang attribute
  document.documentElement.setAttribute('lang', currentLang);

  console.log('[i18n] Initialized with language:', currentLang, '(detected)');
})();