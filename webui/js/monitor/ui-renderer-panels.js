// js/monitor/ui-renderer-panels.js — R641 (2026-11-19): панели монитора, вынесенные
// из ui-renderer.js — изображения (запросы, модели, размеры), авто-пулл и
// виртуальные модели.
//
// ЗАЧЕМ. `ui-renderer.js` держал 1958 строк: весь updateUI, диагностика, кластерные
// ресурсы, таблицы бэкендов, очередь, сессии, топология — и отдельная группа
// «панели изображений + авто-пулл». Разрез сделан по теме: сюда уехало то, что
// рисует панели, которые включаются только на image-бэкендах и при включённом
// авто-пулле.
//
// КАК СВЯЗАНО. Этот файл обязан грузиться ПЕРЕД `ui-renderer.js`
// (`webui/monitor.html`): тот берёт `window.MonitorUiPanels` и раздаёт имена
// локальными ссылками. Публичная точка входа страницы не менялась.
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: код перенесён как есть, включая тексты подсказок.
(function() {
  'use strict';

  var MA = window.MonitorApp;
  var T = MA.T;
  /**
   * R-Image Phase 5 (2026-10-02): helpers для image-бэкенда
   * (BackendType=image_cpp, stable-diffusion.cpp).
   *
   * ЗАЧЕМ локальные функции, а не modules/utils.js: monitor.html исторически
   * грузит только monitor-модули и MonitorApp-хелперы. Если Utils на странице
   * есть (мы его подключаем), берём его - единая трактовка с WebUI; если нет
   * (страницу открыли со старым набором скриптов/кэшем), работает локальный
   * fallback, и Monitor всё равно показывает порт воркера и модели.
   *
   * image-бэкенд отличается тем, что воркер живёт на своём порту (imagePort),
   * а метрики лежат в backend.image, а не в ollama/llamaCpp.
   */
  function imageBlockOf(b) {
    return (b && (b.image || b.Image)) || {};
  }
  function imageModelsOf(b) {
    var img = imageBlockOf(b);
    return Array.isArray(img.models) ? img.models : [];
  }
  function imagePortOf(b) {
    if (!b) return 0;
    var raw = (b.imagePort !== undefined) ? b.imagePort : b.image_port;
    var p = parseInt(raw, 10);
    return (isFinite(p) && p > 0) ? p : 0;
  }
  function isImageBackendType(bt) {
    var s = String(bt || '').toLowerCase();
    return s === 'image_cpp' || s === 'imagecpp' || s === 'image.cpp' || s === 'sd_cpp' || s === 'sdcpp';
  }
  function isImageBackend(b) {
    if (window.Utils && typeof window.Utils.getBackendType === 'function') {
      return window.Utils.getBackendType(b) === 'image_cpp';
    }
    return isImageBackendType(b && (b.backendType || b.BackendType || b.backend_type || b.type));
  }
  /** Состояние image-модели/воркера -> локализованный текст (gguf.model_state_*). */
  function imageStateText(state) {
    var s = String(state || '').toLowerCase();
    if (window.Utils && typeof window.Utils.imageStateLabel === 'function') {
      return window.Utils.imageStateLabel(s);
    }
    var key = (s === 'loaded') ? 'gguf.model_state_loaded'
      : (s === 'loading') ? 'gguf.model_state_loading'
      : (s === 'error' || s === 'failed') ? 'gguf.model_state_error'
      : 'gguf.model_state_unloaded';
    var v = T(key);
    return (v && v !== key) ? v : (s || 'not_loaded');
  }
  /** Размер image-модели в байтах (см. Utils.imageSizeBytes). */
  function imageSizeBytesOf(m) {
    if (window.Utils && typeof window.Utils.imageSizeBytes === 'function') {
      return window.Utils.imageSizeBytes(m);
    }
    var v = Number((m && (m.sizeBytes !== undefined ? m.sizeBytes : m.size_bytes)) || 0);
    if (!isFinite(v) || v <= 0) return 0;
    return v < 1024 * 1024 ? Math.round(v * 1024 * 1024) : Math.round(v);
  }
  /** Формат байт -> "1.5 GB" / "512 MB". */
  function fmtBytesShort(bytes) {
    if (!bytes || bytes <= 0) return '';
    var mb = bytes / 1024 / 1024;
    if (mb >= 1024) return (mb / 1024).toFixed(1) + ' GB';
    return Math.round(mb) + ' MB';
  }
  /**
   * R-Image Phase 8 (2026-10-03): экранирование текста ленты image-запросов.
   *
   * ЗАЧЕМ СВОЙ ХЕЛПЕР, А НЕ MA.esc. В webui/js/monitor/state.js MonitorApp.esc
   * объявлен как `{ '&': '&', '<': '<', '>': '>' ... }` - то есть для &, < и >
   * возвращает ТЕ ЖЕ символы и фактически не экранирует ничего (баг жил
   * незамеченным, потому что панели монитора печатают имена бэкендов/моделей,
   * где этих символов обычно нет). В ленте image-запросов лежат поля из тела
   * запроса КЛИЕНТА (model, path, prompt), поэтому подставлять их в innerHTML
   * без экранирования нельзя - это XSS в мониторе. Правим локально, чтобы не
   * менять поведение уже существующих панелей.
   */
  function escHtml(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function(c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  /**
   * Длительность image-запроса -> человекочитаемо: "900 ms" / "9.1 s" /
   * "21 s" / "1m 20s". До секунды показываем миллисекунды (важно для мелких
   * генераций), дальше - секунды: полное число миллисекунд в ленте нечитаемо.
   */
  function fmtImageDuration(ms) {
    var v = Number(ms) || 0;
    if (v <= 0) return '—';
    if (v < 1000) return Math.round(v) + ' ms';
    if (v < 10000) return (v / 1000).toFixed(1) + ' s';
    var sec = Math.round(v / 1000);
    if (sec < 60) return sec + ' s';
    var min = Math.floor(sec / 60);
    if (min < 60) return min + 'm ' + (sec % 60) + 's';
    return Math.floor(min / 60) + 'h ' + (min % 60) + 'm';
  }

  /** Размер генерации: "512x512"; прочерк, если воркер его не сообщил. */
  function fmtImageSize(w, h) {
    var W = Number(w) || 0, H = Number(h) || 0;
    if (W <= 0 || H <= 0) return '—';
    return W + 'x' + H;
  }

  /**
   * Время запроса -> локаль. Нулевое/битое время даёт прочерк, а не
   * "01.01.1, 00:00:00": у Go нулевое time.Time сериализуется как
   * "0001-01-01T00:00:00Z" (та же ловушка, что у lastAgentContact).
   */
  function fmtImageTime(at) {
    if (!at) return '—';
    var d = new Date(at);
    var t = d.getTime();
    if (!isFinite(t) || d.getFullYear() <= 1) return '—';
    return d.toLocaleTimeString();
  }

  /** Статус image-запроса -> локализованный текст (monitor.imageRequests.status.*). */
  function imageRequestStatusText(st) {
    var s = String(st || '').toLowerCase();
    if (!s) return '—';
    var key = 'monitor.imageRequests.status.' + s;
    var v = T(key);
    return (v && v !== key) ? v : s;
  }

  /** Бейдж статуса image-запроса (ok/failed/rejected/accepted/finished). */
  function imageRequestStatusBadge(st) {
    var s = String(st || '').toLowerCase();
    if (s === 'ok') return 'badge-green';
    if (s === 'failed') return 'badge-red';
    if (s === 'rejected') return 'badge-orange';
    if (s === 'accepted') return 'badge-yellow';
    if (s === 'finished') return 'badge-blue';
    return 'badge-yellow';
  }

  /**
   * R83 (2026-09-25): локальный хелпер записи текста в элемент.
   *
   * ЗАЧЕМ. renderAdmissionStats (R70) и renderPlacementPolicy (R77) звали
   * голый updateText(...), которого в этом файле НИКОГДА не было — он объявлен
   * приватно внутри IIFE MonitorMetrics (js/modules/monitor-metrics.js:83) и
   * наружу торчит только как MonitorMetrics.updateText. Итог: на КАЖДОМ refresh'е
   * падало `ReferenceError: updateText is not defined`, updateUI обрывался на
   * первой такой панели, и /monitor оставался пустым («Нет данных для отображения
   * топологии»), а в консоли росло `[monitor] fetchAll failed`.
   *
   * Предпочитаем собственный хелпер, а не MonitorMetrics.updateText: он не
   * зависит от порядка загрузки скриптов (оба идут с defer) и не ломается, если
   * monitor-metrics.js не подключён на странице.
   */
  function updateText(id, text) {
    var el = document.getElementById(id);
    if (el) el.textContent = text;
  }

  /**
   * applyImageRequestsPanelI18n — перевести СТАТИЧЕСКИЕ подписи панели запросов.
   *
   * ЗАЧЕМ ОТДЕЛЬНО. monitor.html переводит статику ([data-i18n]) только по
   * событию смены языка (#langSelect 'change', monitor.html:663) и один раз при
   * инициализации i18n. Если начальная детекция языка разошлась с языком, который
   * в итоге показывает переключатель (свежий профиль: localStorage пуст,
   * navigator.language=en, а переключатель встаёт на ru), часть панелей остаётся
   * с англоязычными дефолтами из разметки. Свою панель переводим сами — это
   * дешёво (десяток элементов) и не зависит от порядка инициализации; чужие
   * панели сознательно не трогаем (иначе это правка поведения всего Monitor).
   */
  function applyImageRequestsPanelI18n(panel) {
    if (!panel || typeof panel.querySelectorAll !== 'function') return;
    var nodes = panel.querySelectorAll('[data-i18n]');
    if (!nodes || !nodes.length) return;
    Array.prototype.forEach.call(nodes, function(el) {
      var key = el.getAttribute && el.getAttribute('data-i18n');
      if (key) el.textContent = T(key);
    });
  }

  /**
   * renderImageRequests — R-Image Phase 8 (2026-10-03): панель «запросы к
   * image-бэкендам» (тип image_cpp).
   *
   * Данные — data.cluster.image (тип types.ImagePoolMetrics, уже отдаётся
   * GET /api/v1/cluster): requests (агрегаты по всему пулу) + recent (общая
   * лента, свежие в начале). Per-backend счётчики лежат в
   * backend.image.requests и используются в таблице бэкендов (renderBackends).
   *
   * РЕШЕНИЕ О ВИДИМОСТИ (почему так): панель без image-бэкендов бесполезна и
   * только засоряет экран, поэтому при их отсутствии (cluster.image нет вообще
   * либо в списке нет ни одного image_cpp) она прячется через
   * style.display='none' - в чисто текстовом стенде лишней панели быть не
   * должно. Если image-бэкенды ЕСТЬ, панель показывается даже при нулевых
   * счётчиках: «воркер поднят, но генераций ещё не было» - это тоже полезная
   * информация, и она снимает вопрос «а счётчики вообще работают?».
   *
   * @param {Object} data — ответ GET /api/v1/cluster (+ остальные секции)
   * @param {Array}  [bk] — уже отфильтрованный список бэкендов (updateUI
   *                        передаёт его, чтобы панель уважала фильтр по типу)
   */
  function renderImageRequests(data, bk) {
    var panel = document.getElementById('panelImageRequests');
    if (!panel) return;

    var backends = bk || (data && data.cluster && data.cluster.backends) || [];
    var pool = (data && data.cluster && data.cluster.image) || null;
    var hasImageBackend = backends.some(function(b) {
      if (isImageBackend(b)) return true;
      return isImageBackendType(b && (b.backendType || b.BackendType || b.backend_type || b.type));
    });
    // pool.backends — счётчик image-бэкендов самого балансера: страховка на
    // случай, когда клиентский фильтр по типу выкинул строки из bk, а данные
    // пула при этом есть.
    if (!hasImageBackend && !(pool && (pool.backends || 0) > 0)) {
      panel.style.display = 'none';
      return;
    }
    panel.style.display = '';
    applyImageRequestsPanelI18n(panel);

    var req = (pool && pool.requests) || {};
    var recent = (pool && Array.isArray(pool.recent)) ? pool.recent : [];

    updateText('imgReqTotal', req.total || 0);
    updateText('imgReqOk', req.ok || 0);
    updateText('imgReqFailed', req.failed || 0);
    updateText('imgReqRejected', req.rejected || 0);
    updateText('imgReqInFlight', req.inFlight || 0);
    updateText('imgReqRps', (Number(req.rps) || 0).toFixed(1));
    updateText('imgReqAvg', req.avgDurationMs > 0 ? fmtImageDuration(req.avgDurationMs) : '—');
    updateText('imgReqP95', req.p95DurationMs > 0 ? fmtImageDuration(req.p95DurationMs) : '—');
    updateText('imageRequestsCount', recent.length);

    var tb = document.querySelector('#imageRequestsTable tbody');
    if (!tb) return;
    if (!recent.length) {
      tb.innerHTML = '<tr><td colspan="8" style="color:var(--text-secondary);text-align:center;padding:16px">' +
        T('monitor.imageRequests.empty') + '</td></tr>';
      return;
    }

    tb.innerHTML = recent.map(function(r) {
      r = r || {};
      var st = String(r.status || '').toLowerCase();
      // title строки: HTTP-код, код ошибки и сообщение движка/гейта. В
      // колонках они не помещаются, но при разборе инцидента нужны.
      var titleBits = [];
      if (r.httpStatus) titleBits.push('HTTP ' + r.httpStatus);
      if (r.code) titleBits.push(String(r.code));
      if (r.error) titleBits.push(String(r.error));
      var titleAttr = titleBits.length ? ' title="' + escHtml(titleBits.join(' | ')) + '"' : '';
      var sizeTxt = fmtImageSize(r.width, r.height);
      var batch = Number(r.batch) || 0;
      if (batch > 1) sizeTxt += ' x' + batch;
      var images = Number(r.images) || 0;
      var steps = Number(r.steps) || 0;
      return '<tr' + titleAttr + '>' +
        '<td style="font-size:var(--font-size-xs);white-space:nowrap">' + escHtml(fmtImageTime(r.at)) + '</td>' +
        '<td>' + escHtml(r.backendId || '—') + '</td>' +
        '<td><code style="font-size:var(--font-size-xs)">' + escHtml(r.path || '—') + '</code></td>' +
        '<td>' + escHtml(r.model || '—') +
          (images > 0 ? ' <span style="font-size:var(--font-size-xs);color:var(--text-secondary)">×' + images + '</span>' : '') + '</td>' +
        '<td class="col-right">' + escHtml(sizeTxt) + '</td>' +
        '<td class="col-right">' + (steps > 0 ? steps : '—') + '</td>' +
        '<td class="col-right">' + escHtml(fmtImageDuration(r.durationMs)) + '</td>' +
        '<td><span class="badge ' + imageRequestStatusBadge(st) + '">' + escHtml(imageRequestStatusText(st)) + '</span></td>' +
        '</tr>';
    }).join('');
  }

  function renderAutoPullPanel(cfg, status) {
    var enabledEl = document.getElementById('autoPullEnabled');
    var enabledLabel = document.getElementById('autoPullEnabledLabel');
    var maxConcurrentEl = document.getElementById('autoPullMaxConcurrent');
    // R106c (2026-10-12): поле Pull Timeout убрано из панели — элемент может не
    // существовать, поэтому обращения к нему ниже защищены (иначе TypeError и
    // вся панель auto-pull перестаёт отрисовываться).
    var retryCountEl = document.getElementById('autoPullRetryCount');
    var activeCountEl = document.getElementById('autoPullActiveCount');
    var tableBody = document.querySelector('#autoPullTable tbody');

    if (!enabledEl) return; // Panel not rendered yet

    // If no config data (API not available, demo mode, or error), show "no data" state
    // but DON'T reset the checkbox — preserve user interaction
    if (!cfg) {
      maxConcurrentEl.value = 2;
      retryCountEl.value = 1;
      if (activeCountEl) activeCountEl.textContent = '0';
      if (tableBody) tableBody.innerHTML = '<tr><td colspan="6" style="color:var(--text-secondary);text-align:center;padding:12px">' + (T('monitor.autoPull.noData') || 'No data') + '</td></tr>';
      return;
    }

    // Update config fields from server data
    enabledEl.checked = !!cfg.enabled;
    if (enabledLabel) enabledLabel.textContent = cfg.enabled
      ? (T('monitor.autoPull.on') || 'Вкл')
      : (T('monitor.autoPull.off') || 'Выкл');
    maxConcurrentEl.value = cfg.maxConcurrent || 2;
    retryCountEl.value = cfg.retryCount || 1;

    // Update active pulls count from status
    if (status) {
      var activePulls = (status.activePulls && status.activePulls.length) || 0;
      if (activeCountEl) activeCountEl.textContent = activePulls;
    } else {
      if (activeCountEl) activeCountEl.textContent = '0';
    }

    // Render active pulls table
    var pulls = (status && status.activePulls) || [];
    if (!tableBody) return;
    if (pulls.length === 0) {
      tableBody.innerHTML = '<tr><td colspan="6" style="color:var(--text-secondary);text-align:center;padding:12px">' + (T('monitor.autoPull.noActive') || 'No active pulls') + '</td></tr>';
      return;
    }

    tableBody.innerHTML = pulls.map(function(p) {
      var model = MA.esc(p.model || '-');
      var backend = MA.esc(p.backendId || p.backendID || '-');
      var started = p.startedAt ? new Date(p.startedAt).toLocaleTimeString() : '-';
      var duration = p.startedAt ? MA.fmtDur(Date.now() - new Date(p.startedAt).getTime()) : '-';
      var statusText = p.done ? '✅ ' + T('monitor.autoPull.statusDone') : (p.error ? '❌ ' + T('monitor.autoPull.statusError') : '⏳ ' + T('monitor.autoPull.statusPulling'));
      var statusClass = p.done ? 'badge-green' : (p.error ? 'badge-red' : 'badge-yellow');
      var errorText = p.error ? MA.esc(p.error) : (p.httpStatus ? 'HTTP ' + p.httpStatus : '-');
      return '<tr><td><strong>' + model + '</strong></td><td>' + backend + '</td><td>' + started + '</td><td>' + duration + '</td><td><span class="badge ' + statusClass + '">' + statusText + '</span></td><td style="font-size:var(--font-size-xs);color:var(--text-secondary);max-width:200px;overflow:hidden;text-overflow:ellipsis">' + errorText + '</td></tr>';
    }).join('');
  }

  function renderVirtualModels(vmData) {
    var tb = document.querySelector('#vmTable tbody');
    var countEl = document.getElementById('vmCount');
    var jobDetailsEl = document.getElementById('vmJobDetails');
    var jobsTb = document.querySelector('#vmJobsTable tbody');

    if (!tb) return; // Panel not rendered

    if (!vmData || !vmData.models || vmData.models.length === 0) {
      if (countEl) countEl.textContent = '0';
      if (tb) tb.innerHTML = '<tr><td colspan="6" style="color:var(--text-secondary);text-align:center;padding:16px">' + T('vm.noModels') + '</td></tr>';
      if (jobDetailsEl) jobDetailsEl.style.display = 'none';
      return;
    }

    if (countEl) countEl.textContent = vmData.models.length;

    // Render models table
    tb.innerHTML = vmData.models.map(function(m) {
      var slices = m.slices || [];
      var slicesCount = Array.isArray(slices) ? slices.length : (slices || 0);
      var activeJobs = m.activeJobs || 0;
      var timeout = m.timeoutMs || '-';
      var enabled = vmData.enabled !== false;
      var status = enabled
        ? '<span class="badge badge-green">' + T('vm.enabled') + '</span>'
        : '<span class="badge badge-yellow">' + T('vm.disabled') + '</span>';
      return '<tr>' +
        '<td><strong>' + MA.esc(m.name || '-') + '</strong>' + (m.description ? '<br><span style="font-size:var(--font-size-xs);color:var(--text-secondary)">' + MA.esc(m.description) + '</span>' : '') + '</td>' +
        '<td>' + slicesCount + '</td>' +
        '<td><code>' + MA.esc(m.mode || '-') + '</code></td>' +
        '<td class="col-right">' + activeJobs + '</td>' +
        '<td class="col-right">' + timeout + 'ms</td>' +
        '<td>' + status + '</td>' +
        '</tr>';
    }).join('');

    // Aggregate active jobs across all models
    var allJobs = [];
    vmData.models.forEach(function(m) {
      if (m.activeJobs && m.activeJobs > 0) {
        (m.jobInfos || []).forEach(function(j) {
          allJobs.push({ model: m.name, job: j });
        });
      }
    });

    if (allJobs.length > 0 && jobDetailsEl) {
      jobDetailsEl.style.display = 'block';
      jobsTb.innerHTML = allJobs.map(function(j) {
        return '<tr>' +
          '<td>' + MA.esc(j.model) + '</td>' +
          '<td><code>' + MA.esc(j.job.requestId || '-') + '</code></td>' +
          '<td>' + (j.job.currentSlice !== undefined ? T('vm.sliceOrdinal') + ' #' + j.job.currentSlice : '-') + '</td>' +
          '<td class="col-right">' + (j.job.hasError ? '❌ ' + T('vm.jobError') : '⏳ ' + T('vm.jobRunning')) + '</td>' +
          '</tr>';
      }).join('');
    } else if (jobDetailsEl) {
      jobDetailsEl.style.display = 'none';
    }
  }
  // R641: экспорт для ui-renderer.js (там — локальные ссылки на эти имена).
  window.MonitorUiPanels = {
    imageBlockOf: imageBlockOf,
    imageModelsOf: imageModelsOf,
    imagePortOf: imagePortOf,
    isImageBackendType: isImageBackendType,
    isImageBackend: isImageBackend,
    imageStateText: imageStateText,
    imageSizeBytesOf: imageSizeBytesOf,
    fmtBytesShort: fmtBytesShort,
    escHtml: escHtml,
    fmtImageDuration: fmtImageDuration,
    fmtImageSize: fmtImageSize,
    fmtImageTime: fmtImageTime,
    imageRequestStatusText: imageRequestStatusText,
    imageRequestStatusBadge: imageRequestStatusBadge,
    updateText: updateText,
    applyImageRequestsPanelI18n: applyImageRequestsPanelI18n,
    renderImageRequests: renderImageRequests,
    renderAutoPullPanel: renderAutoPullPanel,
    renderVirtualModels: renderVirtualModels
  };
})();