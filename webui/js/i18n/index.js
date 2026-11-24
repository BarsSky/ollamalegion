// OllamaLegion i18n Manager
// Supports multiple languages, extensible by adding new I18N_XX.js files
(function () {
  'use strict';

  // Guard against duplicate initialization (e.g. when monitor.html is loaded inside an iframe)
  if (window.__i18nInitialized) {
    return;
  }
  window.__i18nInitialized = true;

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
  // Safe access to browser storage: в Node (тесты, SSR) `localStorage` нет.
  //
  // ЖИВОЙ ДЕФЕКТ CI. Модуль читал `localStorage.getItem(...)` прямо в
  // `detectLanguage`, и на раннере с Node 20 (`localStorage` появился в Node 22)
  // падал при загрузке: `ReferenceError: localStorage is not defined` —
  // `webui/tests/image-hf-i18n-r694.test.js` и `image-role-label-r695.test.js`
  // краснели ТОЛЬКО в CI (локально Node 22, поэтому «не воспроизводится»).
  // Хранилище — оптимизация «запомнить выбор», а не условие работы, поэтому его
  // отсутствие означает «взять язык из окружения или дефолт», а не падение.
  function safeStorageGet(key) {
    try {
      if (typeof localStorage === 'undefined' || !localStorage) return null;
      return localStorage.getItem(key);
    } catch (e) {
      return null; // приватный режим/запрет доступа — не повод падать
    }
  }

  function safeStorageSet(key, value) {
    try {
      if (typeof localStorage === 'undefined' || !localStorage) return;
      localStorage.setItem(key, value);
    } catch (e) {
      // тот же случай: язык просто не запомнится между визитами
    }
  }

  function detectLanguage() {
    // 1. Stored preference
    var stored = safeStorageGet(STORAGE_KEY);
    if (stored && (stored === 'ru' || stored === 'en')) return stored;

    // 2. Browser language (primary). `navigator` в Node 20 тоже нет — читаем
    // через проверку, а не полагаемся на глобал.
    var nav = (typeof navigator !== 'undefined' && navigator) ? navigator : null;
    var browserLang = ((nav && (nav.language || nav.userLanguage)) || '').toLowerCase();
    if (browserLang.startsWith('ru')) return 'ru';
    if (browserLang.startsWith('en')) return 'en';

    // 3. navigator.languages array (fallback order)
    var navLangs = (nav && nav.languages) ? nav.languages : null;
    if (navLangs && navLangs.length) {
      for (var i = 0; i < navLangs.length; i++) {
        var lang = String(navLangs[i] || '').toLowerCase();
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
        // R83-fix (2026-10-09): заменяем ВСЕ вхождения плейсхолдера, а не только
        // первое. String.replace со строковым шаблоном меняет лишь первое
        // совпадение, поэтому строка, где {slots} встречается дважды (совет
        // «воркер держит {slots} слотов … выставьте maxConcurrentRequests={slots}»),
        // показывала оператору сырой "{slots}" во втором месте — поймано живой
        // проверкой баннера «потенциал параллельности не раскрыт».
        str = str.split('{' + p + '}').join(params[p]);
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
    // Запись — тоже через безопасную обёртку: в Node 20 `localStorage` нет, а
    // setLang зовётся из тестов и из кода, который может исполняться вне браузера.
    safeStorageSet(STORAGE_KEY, lang);
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

  /**
   * Apply translations to DOM attributes.
   *
   * R238d: monitor.html исторически вообще не применял data-i18n при загрузке —
   * он переводил лишь небольшую карту текстов (updateMonitorTexts), поэтому
   * «переведёнными» выглядели не все поля: заголовки панели раскладки, кнопки,
   * подсказка и т.п. оставались на языке разметки. Общий apply() переводит:
   *   [data-i18n]            -> textContent (со спец-синтаксисом [title]key)
   *   [data-i18n-html]       -> innerHTML (тексты со <strong>/<br>/<code>)
   *   [data-i18n-title]      -> title
   *   [data-i18n-placeholder]-> placeholder
   * @param {Document|Element} [root] - корень (по умолчанию document)
   * @returns {number} сколько элементов обновлено
   */
  function apply(root) {
    var scope = root || (typeof document !== 'undefined' ? document : null);
    if (!scope || !scope.querySelectorAll) { return 0; }
    var n = 0;
    scope.querySelectorAll('[data-i18n]').forEach(function (el) {
      var key = el.getAttribute('data-i18n');
      if (!key) { return; }
      if (key.indexOf('[title]') === 0) {
        el.title = t(key.slice('[title]'.length));
      } else if (el.tagName === 'INPUT' && el.hasAttribute('data-i18n-placeholder')) {
        el.placeholder = t(el.getAttribute('data-i18n-placeholder'));
      } else {
        el.textContent = t(key);
      }
      n++;
    });
    scope.querySelectorAll('[data-i18n-html]').forEach(function (el) {
      var key = el.getAttribute('data-i18n-html');
      if (!key) { return; }
      el.innerHTML = t(key);
      n++;
    });
    scope.querySelectorAll('[data-i18n-title]').forEach(function (el) {
      var key = el.getAttribute('data-i18n-title');
      if (!key) { return; }
      el.title = t(key);
      n++;
    });
    scope.querySelectorAll('[data-i18n-placeholder]').forEach(function (el) {
      var key = el.getAttribute('data-i18n-placeholder');
      if (!key) { return; }
      el.placeholder = t(key);
      n++;
    });
    return n;
  }

  // Expose API
  window.I18N = {
    register: register,
    t: t,
    getLang: getLang,
    setLang: setLang,
    apply: apply,
    getAvailableLanguages: getAvailableLanguages,
    detectLanguage: detectLanguage,
  };

  // Set initial lang attribute
  document.documentElement.setAttribute('lang', currentLang);

  console.log('[i18n] Initialized with language:', currentLang, '(detected)');
})();