// js/monitor/state.js — Global state and configuration for monitor

(function() {
  'use strict';

  window.MonitorApp = window.MonitorApp || {};

  // i18n helpers
  MonitorApp.T = function(k, p) {
    return (window.I18N && window.I18N.t ? window.I18N.t(k, p) : k);
  };
  MonitorApp.LANG = function() {
    return (window.I18N && window.I18N.getLang ? window.I18N.getLang() : 'ru');
  };
  MonitorApp.SETLANG = function(l) {
    if (window.I18N && window.I18N.setLang) window.I18N.setLang(l);
  };

  // API configuration
  function detectApiBase() {
    // R60.16.1 (2026-09-08): R60.16.1 fallback chain for API base URL.
    // Pre-R60.16.1: only checked `WEBUI_CONFIG.apiBase` (lowercase, NOT
    // matching entrypoint.sh which injects `API_BASE` uppercase), localStorage,
    // and ?api_base= query. When all empty, fell through to '' (relative),
    // which works inside docker (nginx proxies) but on a host browser at
    // http://localhost:18083 the page would 404 because the browser's
    // resolved URL didn't match a proxy route.
    //
    // R60.16.1 chain (in order):
    //   1. WEBUI_CONFIG.apiBase (legacy lowercase, for backward compat)
    //   2. WEBUI_CONFIG.API_BASE (current uppercase from entrypoint.sh)
    //   3. localStorage.monitorApiBase (operator override)
    //   4. ?api_base= query param
    //   5. window.location.origin (same-origin — works on host browser
    //      AND inside docker via nginx proxy)
    //   6. '' (relative, legacy fallback)
    function readFromCfg() {
      if (typeof window === 'undefined' || !window.WEBUI_CONFIG) return '';
      return window.WEBUI_CONFIG.apiBase || window.WEBUI_CONFIG.API_BASE || '';
    }
    var cfg = readFromCfg();
    if (cfg) return cfg.replace(/\/$/, '');
    var s = localStorage.getItem('monitorApiBase');
    if (s) return s.replace(/\/$/, '');
    var q = new URLSearchParams(location.search).get('api_base');
    if (q) return q.replace(/\/$/, '');
    if (typeof window !== 'undefined' && window.location && window.location.origin) {
      return window.location.origin;
    }
    return '';
  }

  MonitorApp.API_BASE = detectApiBase();
  // Update display if element exists
  var abd = document.getElementById('apiBaseDisplay');
  if (abd) abd.textContent = MonitorApp.API_BASE || location.origin;
  MonitorApp.refreshInterval = 2000;
  MonitorApp.timerId = null;
  MonitorApp.paused = false;
  MonitorApp.demoMode = false;
  MonitorApp.fetchAttempt = 0;
  MonitorApp.lastData = null;
  MonitorApp.lastTime = Date.now();
  MonitorApp.lastTotalRequests = 0;
  MonitorApp.requestRate = 0;
  MonitorApp.fetching = false;
  MonitorApp.corsErrorDetected = false;

  // Stable backend ordering
  MonitorApp._backendOrder = new Map();
  MonitorApp.stableBackendOrder = function(backends) {
    var now = Date.now(), seen = new Set();
    backends.forEach(function(b) {
      var id = b.id || b.ID;
      if (!id) return;
      seen.add(id);
      if (!MonitorApp._backendOrder.has(id)) {
        MonitorApp._backendOrder.set(id, { index: MonitorApp._backendOrder.size, addedAt: now });
      }
    });
    var entries = [];
    MonitorApp._backendOrder.forEach(function(m, id) {
      if (seen.has(id)) entries.push({ id: id, index: m.index, addedAt: m.addedAt });
    });
    entries.sort(function(a, b) { return a.index - b.index; });
    MonitorApp._backendOrder.clear();
    entries.forEach(function(e, i) {
      MonitorApp._backendOrder.set(e.id, { index: i, addedAt: e.addedAt });
    });
    return [].concat(backends).sort(function(a, b) {
      var ia = (MonitorApp._backendOrder.get(a.id || a.ID) || { index: Infinity }).index;
      var ib = (MonitorApp._backendOrder.get(b.id || b.ID) || { index: Infinity }).index;
      return ia - ib;
    });
  };

  // Canvas state
  MonitorApp.topo = { backends: [], sessions: [], queue: {}, w: 0, h: 0, recentClients: [], warmingUpModels: [] };
  MonitorApp.particles = [];
  MonitorApp.convQueue = [];

  // Viewport transform for pan & zoom
  MonitorApp.viewport = {
    offsetX: 0,
    offsetY: 0,
    scale: 1,
    minScale: 0.1,
    maxScale: 3.0
  };
  MonitorApp.toWorld = function(screenX, screenY) {
    return {
      x: (screenX - MonitorApp.viewport.offsetX) / MonitorApp.viewport.scale,
      y: (screenY - MonitorApp.viewport.offsetY) / MonitorApp.viewport.scale
    };
  };
  MonitorApp.toScreen = function(worldX, worldY) {
    return {
      x: worldX * MonitorApp.viewport.scale + MonitorApp.viewport.offsetX,
      y: worldY * MonitorApp.viewport.scale + MonitorApp.viewport.offsetY
    };
  };
  MonitorApp.resetViewport = function() {
    MonitorApp.viewport.offsetX = 0;
    MonitorApp.viewport.offsetY = 0;
    MonitorApp.viewport.scale = 1;
  };

  // Common geometry constants shared between topology and conveyor
  MonitorApp.GEOM = {
    bw: 180,          // balancer width
    bh: 100,          // balancer height
    clientX: 140,     // client connector x
    backendMargin: 140, // backend right margin
    marginY: 40,      // top/bottom margin for node placement
    // R91 (2026-10-08): ВЫСОТЫ ПЛИТОК и зазор — единственный источник правды.
    //
    // Прежний `minSpacing: 30` был МЕНЬШЕ высоты плитки (клиент 44 px, бэкенд
    // 52 px — см. отрисовку ниже), поэтому как только узлов становилось больше,
    // чем влезает, шаг упирался в 30 и плитки наезжали друг на друга: на живой
    // картинке 12 клиентов и 6 бэкендов накрывали друг друга, скрывая и имя
    // клиента, и модель, и статус бэкенда. Теперь шаг не может быть меньше
    // «высота + зазор», а лишние узлы не рисуются вовсе — вместо них счётчик
    // «+N» (см. MonitorApp.nLayout).
    nodeH: { session: 44, backend: 52 },
    nodeGap: 10,
    // Высота плашки «+N» внизу колонки, когда узлы не поместились.
    chipH: 22,
    // R92 (2026-10-09): «поле расширяется, а не ужимается».
    //
    // Было (R91): раскладка вписывала колонку в ТЕКУЩУЮ высоту канваса и лишние
    // узлы прятала за плашкой «+N». На живом стенде это выглядело так: 14
    // бэкендов и 12 клиентов → видно 5 и 6, остальные скрыты, хотя места в окне
    // хватало (жалоба оператора). Замер до правки: canvas 1440x398,
    // section.overflowY=visible, backendLayout={visible:5,hidden:9}.
    //
    // Стало: колонка всегда рисуется с ПОЛНЫМ шагом (плитка + зазор), а канвас
    // растёт под неё (см. MonitorApp.columnNeedH и layoutCanvas в
    // canvas-topology.js); полоса топологии прокручивается.
    //
    // maxNodes / maxCanvasH — только предохранители от патологического кластера
    // (гигантский bitmap и нечитаемая простыня), а не способ «уместить всех».
    maxNodes: 400,
    maxCanvasH: 8000
  };
  MonitorApp.GEOM.backendX = function(w) { return w - MonitorApp.GEOM.backendMargin; };
  MonitorApp.GEOM.balInX = function(w) { return w / 2 - MonitorApp.GEOM.bw / 2; };
  MonitorApp.GEOM.balOutX = function(w) { return w / 2 + MonitorApp.GEOM.bw / 2; };
  MonitorApp.GEOM.balCenterX = function(w) { return w / 2; };
  MonitorApp.GEOM.balCenterY = function(h) { return h / 2; };

  // nodeCount — сколько узлов этого типа сейчас в состоянии (0 = пусто).
  MonitorApp.nodeCount = function(type) {
    return type === 'session'
      ? (MonitorApp.topo.sessions || []).length
      : (MonitorApp.topo.backends || []).length;
  };

  // columnNeedH — сколько вертикали НУЖНО колонке, чтобы показать все её узлы с
  // полным шагом «высота плитки + зазор» (плюс полоса под плашку «+N», если
  // сработал предохранитель). Это высота, под которую владелец канваса
  // (canvas-topology.js: layoutCanvas) обязан вырастить полотно.
  //
  // Считается ЧЕРЕЗ nLayout, а не параллельной формулой: иначе раскладка и высота
  // полотна разъедутся (ровно так «нужная высота» однажды оказалась больше
  // maxCanvasH, потому что не учитывала предохранитель).
  MonitorApp.columnNeedH = function(type) {
    return MonitorApp.nLayout(type).contentH;
  };

  // nLayout — вертикальная раскладка узлов одного типа.
  //
  // Возвращает: сколько узлов РИСУЕМ (visible), сколько не поместилось (hidden),
  // шаг и верхний отступ. Единственное место, где считается геометрия: и
  // отрисовка, и hit-test, и тултип обязаны спрашивать её, иначе клики и
  // подсказки разъедутся с картинкой.
  //
  // Гарантия отсутствия перекрытия: шаг ЖЁСТКО равен «высота плитки + зазор» и
  // никогда не сжимается под высоту окна (R92). hidden > 0 возможен только когда
  // узлов больше предохранителя maxNodes или нужная высота превышает maxCanvasH.
  MonitorApp.nLayout = function(type) {
    var cnt = Math.max(1, MonitorApp.nodeCount(type));
    var h = MonitorApp.topo.h || 0;
    var G = MonitorApp.GEOM;
    var nodeH = G.nodeH[type] || G.nodeH.session;
    var step = nodeH + G.nodeGap;
    // Сколько узлов влезает в МАКСИМАЛЬНОЕ полотно (предохранитель по памяти).
    var maxFit = Math.max(1, Math.floor((G.maxCanvasH - G.marginY * 2 - nodeH) / step) + 1);
    var visible = Math.min(cnt, G.maxNodes, maxFit);
    var hidden = cnt - visible;
    var spacing = step;
    var contentH = G.marginY * 2 + Math.max(0, visible - 1) * spacing + nodeH;
    if (hidden > 0) contentH += G.chipH;
    // Полотно выше контента (мало узлов, большое окно) — колонка по центру полосы.
    var offset = h > contentH ? (h - contentH) / 2 : 0;
    var lastY = G.marginY + offset + (visible - 1) * spacing;
    return {
      count: cnt,
      visible: visible,
      hidden: hidden,
      spacing: spacing,
      offset: offset,
      // Нужная высота полотна для этой колонки (её читает layoutCanvas).
      contentH: contentH,
      // Y плашки «+N»: сразу под последней плиткой.
      chipY: lastY + nodeH / 2 + 11
    };
  };

  // Vertical node placement (sessions on left, backends on right)
  MonitorApp.nY = function(type, i) {
    var L = MonitorApp.nLayout(type);
    return MonitorApp.GEOM.marginY + L.offset + i * L.spacing;
  };

  // nVisible — сколько узлов этого типа реально нарисовано (R91).
  // Нужна всем, кто двигает частицы по линиям к узлам (canvas-conveyor.js):
  // индекс за пределами nVisible не имеет координат на картинке.
  MonitorApp.nVisible = function(type) {
    return MonitorApp.nLayout(type).visible;
  };

  // ───────────────────────── палитра канваса из темы (R95) ────────────────────
  //
  // ЖАЛОБА: «в мониторе при теме mint не видно сетки и подписей в блоках».
  // ПРИЧИНА: канвас выбирал цвета по ИМЕНИ темы — `data-theme === 'light'` —
  // то есть знал ровно две палитры (light и «всё остальное, как dark»). Темы
  // с фоном светлее среднего (mint: --bg-primary #f5f7f6, linear/vercel тоже
  // не dark) получали ТЁМНУЮ палитру: сетка rgba(255,255,255,0.03) на светлом
  // фоне даёт контраст 1.0, текст #e2e8f0 — 1.15, подписи в блоках #fff — 1.15.
  // Замер аудита по всем 7 темам: grid=1.00-1.08, но у mint текст 1.15 → сливается.
  //
  // ТЕПЕРЬ палитра считается из ФАКТИЧЕСКИХ CSS-переменных темы (яркость фона
  // решает, светлая тема или тёмная), поэтому любая новая тема получает
  // контрастные цвета сетки/текста без правки канваса.
  MonitorApp.parseCssColor = function(spec) {
    if (!spec) return null;
    var s = String(spec).trim();
    var m = /^#([0-9a-f]{3}|[0-9a-f]{6})$/i.exec(s);
    if (m) {
      var h = m[1];
      if (h.length === 3) h = h[0] + h[0] + h[1] + h[1] + h[2] + h[2];
      return {
        r: parseInt(h.slice(0, 2), 16),
        g: parseInt(h.slice(2, 4), 16),
        b: parseInt(h.slice(4, 6), 16),
        a: 1
      };
    }
    m = /^rgba?\(([^)]+)\)$/i.exec(s);
    if (m) {
      var p = m[1].split(',').map(function(x) { return parseFloat(x.trim()); });
      if (p.length >= 3 && !isNaN(p[0]) && !isNaN(p[1]) && !isNaN(p[2])) {
        return { r: p[0], g: p[1], b: p[2], a: p.length > 3 ? p[3] : 1 };
      }
    }
    return null;
  };

  // relativeLuminance — WCAG-яркость цвета (0 = чёрный, 1 = белый).
  MonitorApp.relativeLuminance = function(c) {
    if (!c) return 0;
    var f = function(v) {
      v = v / 255;
      return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
    };
    return 0.2126 * f(c.r) + 0.7152 * f(c.g) + 0.0722 * f(c.b);
  };

  // contrastRatio — отношение контрастов двух цветов (WCAG).
  MonitorApp.contrastRatio = function(c1, c2) {
    var l1 = MonitorApp.relativeLuminance(c1);
    var l2 = MonitorApp.relativeLuminance(c2);
    var hi = Math.max(l1, l2), lo = Math.min(l1, l2);
    return (hi + 0.05) / (lo + 0.05);
  };

  // paletteFor — ЧИСТАЯ функция (тестируется без браузера): по цвету фона темы
  // возвращает светлую или тёмную палитру канваса.
  MonitorApp.paletteFor = function(bgColor) {
    var bg = MonitorApp.parseCssColor(bgColor);
    var isLight = bg ? MonitorApp.relativeLuminance(bg) > 0.5 : false;
    return isLight ? MonitorApp._lightPalette() : MonitorApp._darkPalette();
  };

  MonitorApp._lightPalette = function() {
    return {
      isLight: true,
      // Контраст сетки к светлому фону ≈ 1.33 (заметно, но не «шум»).
      grid: 'rgba(15,23,42,0.14)',
      gridStrong: 'rgba(15,23,42,0.26)',
      text: '#0f172a',
      textMuted: '#475569',
      tileText: '#0f172a',
      lane: 'rgba(15,23,42,0.12)',
      laneLabel: 'rgba(15,23,42,0.5)',
      barBg: 'rgba(15,23,42,0.12)',
      boxShadow: 'rgba(15,23,42,0.10)',
      overlayIcon: 'rgba(15,23,42,0.35)'
    };
  };

  MonitorApp._darkPalette = function() {
    return {
      isLight: false,
      // Контраст сетки к тёмному фону ≈ 1.29 на #0f0f23 и ≈ 1.20 на чистом
      // чёрном (linear/vercel). При прежних 0.07 на чёрных темах сетка давала
      // 1.12 и практически не читалась.
      grid: 'rgba(255,255,255,0.10)',
      gridStrong: 'rgba(255,255,255,0.18)',
      text: '#e2e8f0',
      textMuted: '#b0b7c4',
      tileText: '#ffffff',
      lane: 'rgba(255,255,255,0.12)',
      laneLabel: 'rgba(255,255,255,0.40)',
      barBg: 'rgba(255,255,255,0.14)',
      boxShadow: 'rgba(255,255,255,0.10)',
      overlayIcon: 'rgba(255,255,255,0.35)'
    };
  };

  // themePalette — палитра ТЕКУЩЕЙ темы (кэш по значению data-theme, чтобы не
  // читать computed style каждый кадр).
  MonitorApp.themePalette = function() {
    var root = document.documentElement;
    var key = (root && root.getAttribute) ? (root.getAttribute('data-theme') || 'dark') : 'dark';
    if (MonitorApp._paletteCache && MonitorApp._paletteCacheKey === key) {
      return MonitorApp._paletteCache;
    }
    var bgSpec = '';
    try {
      if (window.getComputedStyle) {
        var cs = window.getComputedStyle(root);
        bgSpec = (cs.getPropertyValue('--bg-primary') || '').trim();
      }
    } catch (e) { /* no DOM (тесты) — палитра по умолчанию */ }
    var pal = MonitorApp.paletteFor(bgSpec || (key === 'light' ? '#ffffff' : '#0b1120'));
    MonitorApp._paletteCache = pal;
    MonitorApp._paletteCacheKey = key;
    return pal;
  };

  // Common utilities
  //
  // MonitorApp.esc ОБЯЗАН реально экранировать HTML. До R-Image Phase 8 карта
  // подстановки была сломана: для '&', '<' и '>' она возвращала ТЕ ЖЕ символы
  // (то есть экранировала только кавычки). Панели Monitor печатают через esc()
  // значения, пришедшие ОТ КЛИЕНТА (имя модели из тела запроса, путь, id
  // бэкенда) прямо в innerHTML — значит имя модели вида
  // <img src=x onerror=...> исполнялось в браузере оператора. Это XSS-вектор, а
  // не косметика: страницу открывает оператор с доступом к admin API.
  //
  // Entity собираем из кода символа (& = 38), а не литералом: так строку нельзя
  // случайно «починить» обратно при переписывании файла инструментами, которые
  // разворачивают HTML-сущности в тексте скрипта.
  MonitorApp.ESC_ENTITIES = (function() {
    var amp = String.fromCharCode(38);
    var map = {};
    map['&'] = amp + 'amp;';
    map['<'] = amp + 'lt;';
    map['>'] = amp + 'gt;';
    map['"'] = amp + 'quot;';
    map["'"] = amp + '#39;';
    return map;
  })();

  MonitorApp.esc = function(s) {
    if (s == null) return '';
    return String(s).replace(/[&<>"']/g, function(c) {
      return MonitorApp.ESC_ENTITIES[c];
    });
  };

  MonitorApp.fmtDur = function(ms) {
    if (ms < 0) ms = 0;
    var s = Math.floor(ms / 1000);
    if (s < 60) return s + 's';
    var m = Math.floor(s / 60);
    if (m < 60) return m + 'm ' + (s % 60) + 's';
    var h = Math.floor(m / 60);
    return h + 'h ' + (m % 60) + 'm';
  };

  MonitorApp.bar = function(v) {
    var x = Math.max(0, Math.min(100, v || 0)), c = 'var(--success)';
    if (x > 60) c = 'var(--warning)';
    if (x > 85) c = 'var(--danger)';
    return '<span class="bar-track"><span class="bar-fill" style="width:' + x + '%;background:' + c + '"></span></span>';
  };

  MonitorApp.ipColor = function(ip) {
    if (!ip) return 'var(--text-secondary)';
    var h = 0, i, c;
    for (i = 0; i < ip.length; i++) { c = ip.charCodeAt(i); h = ((h << 5) - h) + c; h |= 0; }
    h = Math.abs(h);
    var hue = (h % 12) * 30;
    return 'hsl(' + hue + ', 60%, 60%)';
  };

  MonitorApp.isCloudModel = function(name) {
    return name && name.indexOf(':cloud') >= 0;
  };

  MonitorApp.isTechnicalClient = function(cn, ua) {
    if (!cn) return false;
    var l = cn.toLowerCase();
    if (l.indexOf('monitor') >= 0) return true;
    if (l.indexOf('healthcheck') >= 0 || l.indexOf('health') >= 0) return true;
    if (l.indexOf('kube-probe') >= 0 || l.indexOf('kube') >= 0) return true;
    if (l.indexOf('prometheus') >= 0) return true;
    if (l.indexOf('ollamalegion') >= 0 && l.indexOf('agent') >= 0) return true;
    return false;
  };

  MonitorApp.getCI = function(n) {
    if (!n) return '👤';
    var l = n.toLowerCase();
    if (l.indexOf('cline') >= 0) return '🦾';
    if (l.indexOf('openwebui') >= 0 || l.indexOf('open-webui') >= 0) return '🌐';
    if (l.indexOf('curl') >= 0) return '📡';
    if (l.indexOf('python') >= 0 || l.indexOf('requests') >= 0) return '🐍';
    if (l.indexOf('node') >= 0 || l.indexOf('axios') >= 0) return '🟢';
    return '👤';
  };

  // Canvas helpers
  MonitorApp.vF = function() { return 'system-ui,-apple-system,sans-serif'; };
  MonitorApp.trunc = function(s, l) { if (!s) return ''; return s.length > l ? s.slice(0, l) + '…' : s; };
  MonitorApp.rr = function(ctx, x, y, w, h, r) {
    ctx.beginPath();
    ctx.moveTo(x + r, y);
    ctx.arcTo(x + w, y, x + w, y + h, r);
    ctx.arcTo(x + w, y + h, x, y + h, r);
    ctx.arcTo(x, y + h, x, y, r);
    ctx.arcTo(x, y, x + w, y, r);
    ctx.closePath();
  };

  /**
   * setPollingPaused — остановить/возобновить циклы метрик монитора.
   *
   * ЗАЧЕМ: монитор живёт в iframe со своими таймерами (основной цикл 2 с и
   * sparkline-поллер 5 с), из родительского документа их не снять. Пауза
   * авто-обновления и скрытая вкладка должны останавливать и их — иначе
   * «пауза» в WebUI ничего не значит для трафика.
   */
  function setPollingPaused(paused) {
    if (paused) {
      if (MonitorApp.timerId) { clearInterval(MonitorApp.timerId); MonitorApp.timerId = null; }
      if (window.Sparkline && typeof window.Sparkline.stopPoller === 'function') {
        window.Sparkline.stopPoller();
      }
      return;
    }
    if (!MonitorApp.timerId && !MonitorApp.demoMode) {
      MonitorApp.timerId = setInterval(window.fetchAllSafe, MonitorApp.refreshInterval);
    }
    if (window.Sparkline && typeof window.Sparkline.startPoller === 'function') {
      window.Sparkline.startPoller();
    }
  }
  MonitorApp.setPollingPaused = setPollingPaused;

  // Скрытая вкладка браузера — та же пауза (родитель может быть не активен).
  document.addEventListener('visibilitychange', function() {
    setPollingPaused(document.visibilityState === 'hidden');
  });

  // Message listener for config updates
  window.addEventListener('message', function(e) {
    if (e.data && e.data.type === 'ollamalegion-pause') {
      setPollingPaused(!!e.data.paused);
      return;
    }
    if (e.data && e.data.type === 'ollamalegion-config') {
      // R60.16.1 (2026-09-08): skip empty values so the parent's
      // postMessage doesn't overwrite our detectApiBase() fallback
      // (window.location.origin) with ''.
      if (e.data.apiBase !== undefined && e.data.apiBase) {
        MonitorApp.API_BASE = e.data.apiBase;
      }
      if (e.data.apiToken !== undefined) localStorage.setItem('apiToken', e.data.apiToken);
      if (e.data.lang) {
        MonitorApp.SETLANG(e.data.lang);
        document.documentElement.setAttribute('lang', e.data.lang);
        if (typeof window.updateMonitorTexts === 'function') window.updateMonitorTexts();
      }
      if (e.data.refreshInterval && !MonitorApp.demoMode) {
        MonitorApp.refreshInterval = e.data.refreshInterval;
        if (MonitorApp.timerId) {
          clearInterval(MonitorApp.timerId);
          MonitorApp.timerId = setInterval(window.fetchAllSafe, MonitorApp.refreshInterval);
        }
      }
    }
  });
})();