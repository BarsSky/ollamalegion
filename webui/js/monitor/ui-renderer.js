// js/monitor/ui-renderer.js — UI update and table rendering

(function() {
  'use strict';

  var MA = window.MonitorApp;
  var T = MA.T;

  /**
   * aggVRAM — суммарная VRAM кластера БЕЗ двойного счёта по одной физкарте.
   *
   * Берём общий хелпер из monitor-metrics.js (window.MonitorAggregates), потому
   * что сумм по бэкендам в проекте несколько, и они обязаны считать одинаково;
   * локальный fallback нужен на случай старого набора скриптов/кеша, когда
   * monitor-metrics.js ещё не загрузился (порядок defer-скриптов).
   *
   * Почему это вообще нужно: cppworker и imageworker на ОДНОЙ машине читают
   * nvidia-smi одной карты и присылают одинаковые memoryTotal/Used. Наивная
   * сумма давала 16 GB на карте в 8 GB в шапке Monitor и в баннере
   * «VRAM заполнена».
   */
  function aggVRAM(backends) {
    if (window.MonitorAggregates && typeof window.MonitorAggregates.aggregateVRAM === 'function') {
      return window.MonitorAggregates.aggregateVRAM(backends);
    }
    // Fallback: группируем по UUID карт, иначе по имени хоста, и берём максимум
    // в группе (две записи об одной карте не складываем).
    var byGPU = {};
    (Array.isArray(backends) ? backends : []).forEach(function(b) {
      var v = (b && b.vram) || null;
      if (!v || !v.totalGB) return;
      var g = (b && b.gpu) || {};
      var rawUUIDs = g.uuids || g.UUIDs;
      var uuids = Array.isArray(rawUUIDs) ? rawUUIDs.filter(Boolean).sort() : [];
      var key = uuids.length ? 'gpu:' + uuids.join(',') : 'host:' + ((b && (b.host || b.id)) || 'unknown');
      var prev = byGPU[key];
      if (prev) {
        prev.totalGB = Math.max(prev.totalGB, v.totalGB || 0);
        prev.usedGB = Math.max(prev.usedGB, v.usedGB || 0);
      } else {
        byGPU[key] = { totalGB: v.totalGB || 0, usedGB: v.usedGB || 0 };
      }
    });
    var totalGB = 0, usedGB = 0;
    Object.keys(byGPU).forEach(function(k) { totalGB += byGPU[k].totalGB; usedGB += byGPU[k].usedGB; });
    return { totalGB: totalGB, usedGB: usedGB, groups: Object.keys(byGPU).length, deduped: false };
  }

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

  /** Бейдж типа бэкенда: Utils.getBackendTypeBadge, иначе локальные стили. */
  function backendTypeBadgeHtml(b, bt) {
    if (window.Utils && typeof window.Utils.getBackendTypeBadge === 'function') {
      return window.Utils.getBackendTypeBadge(b);
    }
    if (isImageBackendType(bt)) {
      return ' <span class="badge" style="background:#4ade8020;border:1px solid #4ade80;color:#4ade80;font-size:10px;padding:0 4px;border-radius:3px">🎨 image.cpp</span>';
    }
    if (bt === 'llama_cpp') {
      return ' <span class="badge" style="background:#ff6d0020;border:1px solid #ff6d00;color:#ff6d00;font-size:10px;padding:0 4px;border-radius:3px">🦒 llama.cpp</span>';
    }
    if (bt === 'ollama' || !bt) {
      return ' <span class="badge" style="background:#1a73e820;border:1px solid #1a73e8;color:#1a73e8;font-size:10px;padding:0 4px;border-radius:3px">🦙 Ollama</span>';
    }
    return '';
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
   * R83: один упавший рендер не должен убивать всю страницу.
   *
   * До этой правки любое исключение в середине updateUI (как раз ReferenceError
   * выше) выбрасывало управление из функции целиком: терялись ВСЕ панели после
   * упавшей — placement, candidates, virtualModels, диагностика, кластерные
   * ресурсы, таблица бэкендов, диск/сеть, очередь, сессии и топология.
   * Теперь каждая панель изолирована: падает — пишем в консоль и продолжаем.
   */
  function safeRender(name, fn) {
    try {
      fn();
    } catch (e) {
      console.error('[monitor] render ' + name + ' failed:', e);
    }
  }

  /**
   * B-12: Добавляет переключатель backend-типа в заголовок Monitor.
   * Ollama ↔ llama.cpp — синхронизируется с BackendTypeFilter и localStorage.
   */
  function renderBackendTypeSwitcher() {
    var container = document.getElementById('monitorBackendTypeSwitcher');
    if (!container) return;

    var currentType = 'all';
    if (window.BackendTypeFilter) {
      currentType = window.BackendTypeFilter.getCurrentType() || 'all';
    }

    // Round 18f: 3-state switcher (All / Ollama / llama.cpp) вместо 2-state.
    // Кнопки вместо <select> — нагляднее и не зависит от native dropdown.
    container.innerHTML =
      '<div class="monitor-type-switcher" style="display:flex;align-items:center;gap:4px">' +
        '<span style="font-size:11px;color:var(--text-secondary);font-weight:600;margin-right:4px">' + T('monitor.common.backendType') + '</span>' +
        '<button type="button" data-type="all" class="mtype-btn' + (currentType === 'all' || currentType === '' ? ' active' : '') + '" onclick="window.switchBackendType(\'all\')" style="font-size:11px;padding:3px 8px;border-radius:4px;border:1px solid var(--border-color);background:' + (currentType === 'all' || currentType === '' ? 'var(--accent)' : 'var(--bg-secondary)') + ';color:' + (currentType === 'all' || currentType === '' ? '#fff' : 'var(--text-primary)') + ';cursor:pointer;font-weight:600" title="Все бэкенды">' + T('monitor.common.allBackends') + '</button>' +
        '<button type="button" data-type="ollama" class="mtype-btn' + (currentType === 'ollama' ? ' active' : '') + '" onclick="window.switchBackendType(\'ollama\')" style="font-size:11px;padding:3px 8px;border-radius:4px;border:1px solid var(--border-color);background:' + (currentType === 'ollama' ? 'var(--accent)' : 'var(--bg-secondary)') + ';color:' + (currentType === 'ollama' ? '#fff' : 'var(--text-primary)') + ';cursor:pointer;font-weight:600" title="Ollama API">🦙 Ollama</button>' +
        '<button type="button" data-type="llama_cpp" class="mtype-btn' + (currentType === 'llama_cpp' ? ' active' : '') + '" onclick="window.switchBackendType(\'llama_cpp\')" style="font-size:11px;padding:3px 8px;border-radius:4px;border:1px solid var(--border-color);background:' + (currentType === 'llama_cpp' ? 'var(--accent)' : 'var(--bg-secondary)') + ';color:' + (currentType === 'llama_cpp' ? '#fff' : 'var(--text-primary)') + ';cursor:pointer;font-weight:600" title="llama.cpp / GGUF">🦒 llama.cpp</button>' +
        // R-Image Phase 5: четвёртое состояние переключателя — image-бэкенды.
        '<button type="button" data-type="image_cpp" class="mtype-btn' + (currentType === 'image_cpp' ? ' active' : '') + '" onclick="window.switchBackendType(\'image_cpp\')" style="font-size:11px;padding:3px 8px;border-radius:4px;border:1px solid var(--border-color);background:' + (currentType === 'image_cpp' ? 'var(--accent)' : 'var(--bg-secondary)') + ';color:' + (currentType === 'image_cpp' ? '#fff' : 'var(--text-primary)') + ';cursor:pointer;font-weight:600" title="image.cpp / stable-diffusion.cpp">🎨 image.cpp</button>' +
      '</div>';
  }

  function switchBackendType(type) {
    // Round 18f: нормализуем 'all' → '' для BackendTypeFilter.
    if (type === 'all') type = '';
    if (window.BackendTypeFilter) {
      window.BackendTypeFilter.setCurrentType(type);
    }
    localStorage.setItem('ollamalegion_backend_type', type || '');
    // Trigger data refresh with new filter
    if (window.MonitorApp && window.MonitorApp.fetchData) {
      window.MonitorApp.fetchData();
    }
    // Re-render switcher buttons (active state changed)
    if (typeof renderBackendTypeSwitcher === 'function') {
      try { renderBackendTypeSwitcher(); } catch (e) { /* ignore */ }
    }
  }

  function renderLoadFeasibility(bk) {
    var body = document.getElementById('feasibilityBody');
    if (!body) return;

    if (!bk || bk.length === 0) {
      body.innerHTML = '<div style="color:var(--text-secondary);text-align:center;padding:16px">' + T('monitor.common.noData') + '</div>';
      return;
    }

    // Collect per-backend feasibility info from ollama.backendCapacity (Ollama)
    // или llamaCpp.loadedModels + freeSlots (llama.cpp). Round 18f: для llama.cpp
    // бэкенда b.ollama == null, читаем из b.llamaCpp.
    var cards = bk.map(function(b) {
      var isLlamaCpp = (b.backendType || b.BackendType || '') === 'llama_cpp';

      if (isLlamaCpp) {
        // === llama.cpp backend ===
        var lc = b.llamaCpp || b.LlamaCpp || {};
        var loadedModels = Array.isArray(lc.loadedModels) ? lc.loadedModels : [];
        var freeSlots = (lc.freeSlots !== undefined) ? lc.freeSlots
                       : (lc.availableSlots !== undefined) ? lc.availableSlots
                       : 0;
        var maxConcurrent = (lc.maxConcurrentReqs !== undefined) ? lc.maxConcurrentReqs
                           : (b.maxConcurrentRequests || 4);
        // Для llama.cpp «loadable» = сколько ещё моделей может загрузить.
        // Грубая оценка: maxConcurrent - loadedModels.length (cppworker load = slot).
        var loadableCount = Math.max(0, maxConcurrent - loadedModels.length);

        var totalVRAM = b.vram ? b.vram.totalGB : 0;
        var usedVRAM = b.vram ? b.vram.usedGB : 0;
        var freeVRAM = Math.max(0, totalVRAM - usedVRAM) * 1024; // GB → MB

        var fillColor = loadableCount >= 1 ? 'var(--success)' : 'var(--warning)';
        var fillPct = totalVRAM > 0 ? Math.min(100, (usedVRAM / totalVRAM) * 100) : 0;

        // Loaded models: каждый показываем как «✅ loaded»
        var loadableModelsHtml = '';
        if (loadedModels.length > 0) {
          loadableModelsHtml += '<div style="margin-top:6px;font-size:10px;color:var(--text-secondary)">✅ ' + T('monitor.feasibility.canLoad') + ' (' + loadedModels.length + ')</div>' +
            loadedModels.slice(0, 8).map(function(m) {
              var nm = m.name || m.Name || m;
              var ctx = m.contextLength ? ' C:' + m.contextLength : '';
              return '<span class="badge badge-green" style="font-size:10px;margin:1px 2px" title="loaded">' + MA.esc(nm) + ctx + '</span>';
            }).join('') + (loadedModels.length > 8 ? ' <span style="font-size:10px;color:var(--text-secondary)">+' + (loadedModels.length - 8) + '</span>' : '');
        }

        return '<div class="capacity-card" style="min-width:280px;flex:1">' +
          '<h4>' + MA.esc(b.id) + ' 🦒 ' + T('monitor.feasibility.modeLlamaCpp') + '</h4>' +
          // VRAM bar
          '<div style="margin-bottom:6px">' +
            '<div style="display:flex;justify-content:space-between;font-size:10px;color:var(--text-secondary);margin-bottom:2px">' +
              '<span>' + T('monitor.feasibility.vram') + ' (' + (totalVRAM > 0 ? usedVRAM.toFixed(1) + '/' + totalVRAM.toFixed(0) + 'GB' : '—') + ')</span>' +
              '<span>' + fillPct.toFixed(0) + '%</span>' +
            '</div>' +
            '<div class="bar-track-wide"><div class="bar-fill-wide" style="width:' + fillPct + '%;background:' + fillColor + '"></div></div>' +
          '</div>' +
          // Free VRAM + Loadable count
          '<div style="display:flex;gap:12px;margin-top:6px">' +
            '<div style="flex:1;background:var(--bg-secondary);border-radius:6px;padding:6px 8px;text-align:center">' +
              '<div style="font-size:9px;color:var(--text-secondary);text-transform:uppercase;letter-spacing:0.5px">' + T('monitor.feasibility.freeVram') + '</div>' +
              '<div style="font-size:16px;font-weight:700;color:' + (freeVRAM > 4096 ? 'var(--success)' : freeVRAM > 1024 ? 'var(--warning)' : 'var(--danger)') + '">' + (freeVRAM > 1024 ? (freeVRAM / 1024).toFixed(1) + 'G' : freeVRAM.toFixed(0) + 'M') + '</div>' +
            '</div>' +
            '<div style="flex:1;background:var(--bg-secondary);border-radius:6px;padding:6px 8px;text-align:center">' +
              '<div style="font-size:9px;color:var(--text-secondary);text-transform:uppercase;letter-spacing:0.5px">' + T('monitor.feasibility.loadable') + '</div>' +
              '<div style="font-size:16px;font-weight:700;color:' + fillColor + '">' + loadableCount + '</div>' +
            '</div>' +
            '<div style="flex:1;background:var(--bg-secondary);border-radius:6px;padding:6px 8px;text-align:center">' +
              '<div style="font-size:9px;color:var(--text-secondary);text-transform:uppercase;letter-spacing:0.5px">' + T('monitor.feasibility.loaded') + '</div>' +
              '<div style="font-size:16px;font-weight:700;color:var(--accent)">' + loadedModels.length + '</div>' +
            '</div>' +
          '</div>' +
          (loadableModelsHtml ? '<div style="margin-top:4px">' + loadableModelsHtml + '</div>' : '') +
        '</div>';
      }

      // === Ollama backend ===
      var cap = b.ollama && b.ollama.backendCapacity;
      if (!cap) return null;

      var freeVRAM = cap.freeVram || 0; // MB
      var loadableCount = cap.loadableModelCount || 0;
      var guaranteedVRAM = cap.guaranteedVram || 0;
      var mode = cap.mode || 'gpu';
      var availableModels = cap.availableModels || [];

      // Total VRAM in GB for display
      var totalVRAM = b.vram ? b.vram.totalGB : 0;
      var usedVRAM = b.vram ? b.vram.usedGB : 0;

      // Color based on free VRAM / loadable models
      var fillColor = loadableCount >= 3 ? 'var(--success)' : loadableCount >= 1 ? 'var(--warning)' : 'var(--danger)';
      var fillPct = totalVRAM > 0 ? Math.min(100, (usedVRAM / totalVRAM) * 100) : 0;

      // Render available models that can be loaded
      var loadableModelsHtml = '';
      if (availableModels.length > 0) {
        var canLoad = availableModels.filter(function(m) { return m.canLoad; });
        var cannotLoad = availableModels.filter(function(m) { return !m.canLoad; });

        if (canLoad.length > 0) {
          loadableModelsHtml += '<div style="margin-top:6px;font-size:10px;color:var(--text-secondary)">✅ ' + T('monitor.feasibility.canLoad') + '</div>' +
            canLoad.slice(0, 8).map(function(m) {
              var vramMB = m.estimatedVram || m.vramUsage || 0;
              return '<span class="badge badge-green" style="font-size:10px;margin:1px 2px" title="~' + vramMB + 'MB VRAM">' + MA.esc(m.name) + '</span>';
            }).join('') + (canLoad.length > 8 ? ' <span style="font-size:10px;color:var(--text-secondary)">+' + (canLoad.length - 8) + '</span>' : '');
        }
        if (cannotLoad.length > 0) {
          loadableModelsHtml += '<div style="margin-top:4px;font-size:10px;color:var(--text-secondary)">❌ ' + T('monitor.feasibility.cannotLoad') + '</div>' +
            cannotLoad.slice(0, 5).map(function(m) {
              var vramMB = m.estimatedVram || m.vramUsage || 0;
              return '<span class="badge badge-red" style="font-size:10px;margin:1px 2px" title="~' + vramMB + 'MB VRAM">' + MA.esc(m.name) + '</span>';
            }).join('') + (cannotLoad.length > 5 ? ' <span style="font-size:10px;color:var(--text-secondary)">+' + (cannotLoad.length - 5) + '</span>' : '');
        }
      }

      return '<div class="capacity-card" style="min-width:280px;flex:1">' +
        '<h4>' + MA.esc(b.id) + (mode === 'cpu' ? ' 🖥️ ' + T('monitor.feasibility.modeCpu') : ' 🎮 ' + T('monitor.feasibility.modeGpu')) + '</h4>' +
        // VRAM bar
        '<div style="margin-bottom:6px">' +
          '<div style="display:flex;justify-content:space-between;font-size:10px;color:var(--text-secondary);margin-bottom:2px">' +
            '<span>' + T('monitor.feasibility.vram') + ' (' + (totalVRAM > 0 ? usedVRAM.toFixed(1) + '/' + totalVRAM.toFixed(0) + 'GB' : '—') + ')</span>' +
            '<span>' + fillPct.toFixed(0) + '%</span>' +
          '</div>' +
          '<div class="bar-track-wide"><div class="bar-fill-wide" style="width:' + fillPct + '%;background:' + fillColor + '"></div></div>' +
        '</div>' +
        // Free VRAM + Loadable count
        '<div style="display:flex;gap:12px;margin-top:6px">' +
          '<div style="flex:1;background:var(--bg-secondary);border-radius:6px;padding:6px 8px;text-align:center">' +
            '<div style="font-size:9px;color:var(--text-secondary);text-transform:uppercase;letter-spacing:0.5px">' + T('monitor.feasibility.freeVram') + '</div>' +
            '<div style="font-size:16px;font-weight:700;color:' + (freeVRAM > 4096 ? 'var(--success)' : freeVRAM > 1024 ? 'var(--warning)' : 'var(--danger)') + '">' + (freeVRAM > 1024 ? (freeVRAM / 1024).toFixed(1) + 'G' : freeVRAM + 'M') + '</div>' +
          '</div>' +
          '<div style="flex:1;background:var(--bg-secondary);border-radius:6px;padding:6px 8px;text-align:center">' +
            '<div style="font-size:9px;color:var(--text-secondary);text-transform:uppercase;letter-spacing:0.5px">' + T('monitor.feasibility.loadable') + '</div>' +
            '<div style="font-size:16px;font-weight:700;color:' + fillColor + '">' + loadableCount + '</div>' +
          '</div>' +
          '<div style="flex:1;background:var(--bg-secondary);border-radius:6px;padding:6px 8px;text-align:center">' +
            '<div style="font-size:9px;color:var(--text-secondary);text-transform:uppercase;letter-spacing:0.5px">' + T('monitor.feasibility.available') + '</div>' +
            '<div style="font-size:16px;font-weight:700;color:var(--accent)">' + availableModels.length + '</div>' +
          '</div>' +
        '</div>' +
        // Loadable / Non-loadable models
        (loadableModelsHtml ? '<div style="margin-top:4px">' + loadableModelsHtml + '</div>' : '') +
      '</div>';
    }).filter(function(c) { return c !== null; });

    if (cards.length === 0) {
      body.innerHTML = '<div style="color:var(--text-secondary);text-align:center;padding:16px">' + T('monitor.feasibility.noCapacity') + '</div>';
      return;
    }

    body.innerHTML = '<div style="display:flex;flex-wrap:wrap;gap:10px">' + cards.join('') + '</div>';
  }

  function updateUI(data) {
    if (!data || !data.cluster) {
      ['statRate','statRPSCluster','statActive','statPending','statProcessing','statSessions','statBackends','statVRAM','statModelsLoaded'].forEach(function(id) {
        document.getElementById(id).textContent = id === 'statVRAM' ? '—' : '0';
      });
      MA.lastTotalRequests = 0;
      MA.requestRate = 0;
      // R-Image Phase 8: кластера нет -> прячем панель image-запросов, иначе на
      // экране остались бы счётчики прошлого успешного опроса.
      var ip = document.getElementById('panelImageRequests');
      if (ip) ip.style.display = 'none';
      if (typeof window.setConnStatus === 'function') window.setConnStatus(T('monitor.status.idle'), 'green');
      return;
    }

    var adapt = function(b) {
      if (!b) return b;
      if (b.activeRequests !== undefined && b.vram !== undefined) {
        // R-Image Phase 5: у "полного" бэкенда (cluster уже отдал
        // activeRequests/vram) модели image-воркера всё равно лежат только в
        // backend.image.models, а тип может прийти в другой форме (image.cpp).
        // Без этой ветки колонка Models оставалась пустой, а фильтр по типу
        // "image.cpp" не находил бэкенд.
        var fullImg = b.image || b.Image || {};
        var patch = {};
        if (!b.models && Array.isArray(fullImg.models) && fullImg.models.length) {
          patch.models = fullImg.models.map(function (m) { return m.name || m.Name || m; });
        }
        if (window.Utils && typeof window.Utils.normalizeBackendType === 'function') {
          var normBt = window.Utils.normalizeBackendType(b.backendType || b.BackendType || b.type || b.Type || '');
          if (normBt && normBt !== b.backendType) patch.backendType = normBt;
        }
        return Object.keys(patch).length ? Object.assign({}, b, patch) : b;
      }
      var o = b.ollama || b.Ollama || {};
      // Round 18f: для llama.cpp бэкендов читаем из b.llamaCpp (cppworker poller).
      // balancer НЕ выставляет b.ollama.activeRequests для llama.cpp, поэтому
      // stats bar (statActive/statRate/statRPSCluster) показывал «—». Теперь
      // fallback на b.llamaCpp для всех полей.
      var lc = b.llamaCpp || b.LlamaCpp || {};
      // R-Image Phase 5: image-воркер отдаёт свой блок метрик (state,
      // currentModel, vramFreeMb/vramTotalMb, models[]) в backend.image.
      var img = b.image || b.Image || {};
      var g = b.gpu || b.GPU || {}, s = b.system || b.System || {};
      var sr = (b.status || b.Status || '').toString().toLowerCase();
      var sm = { healthy: 'active', active: 'active', ready: 'ready', unhealthy: 'error', offline: 'offline', starting: 'starting', ollama_unavailable: 'ollama_unavailable' };
      // Active requests / RPS — fallback llama.cpp → ollama → 0.
      var activeReq = b.activeRequests;
      if (activeReq === undefined) activeReq = lc.activeRequests !== undefined ? lc.activeRequests : (o.activeRequests || 0);
      // Models list — для llama.cpp берём из loadedModels.
      // R-Image Phase 5: у image-бэкенда ни runningModels, ни loadedModels -
      // модели лежат в backend.image.models. Без этой ветки Monitor показывал
      // 0 моделей у image-бэкенда с загруженной SD-моделью.
      var models = b.models;
      if (!models) {
        if (Array.isArray(img.models) && img.models.length) {
          models = img.models.map(function (m) { return m.name || m.Name || m; });
        } else if (Array.isArray(lc.loadedModels) && lc.loadedModels.length) {
          models = lc.loadedModels.map(function (m) { return m.name || m.Name || m; });
        } else {
          models = (o.runningModels || []).map(function (m) { return m.name || m; });
        }
      }
      // UUID физических карт — по ним Monitor понимает, что несколько бэкендов
      // видят ОДНУ видеокарту, и не удваивает её память. Нормализуем к
      // lowerCamelCase независимо от регистра исходного JSON.
      g = Object.assign({}, g);
      if (g.uuids === undefined && g.UUIDs !== undefined) g.uuids = g.UUIDs;
      if (g.uuids === undefined && g.Uuids !== undefined) g.uuids = g.Uuids;

      return Object.assign({}, b, {
        id: b.id || b.ID,
        status: sm[sr] || sr || 'unknown',
        score: b.score !== undefined ? b.score : (b.Score !== undefined ? b.Score : 50),
        activeRequests: activeReq,
        maxConcurrentRequests: b.maxConcurrentRequests !== undefined ? b.maxConcurrentRequests : (lc.maxConcurrentReqs || lc.maxConcurrentRequests || o.maxConcurrentRequests || 10),
        models: models,
        // R-Image Phase 5: нормализуем тип (image.cpp/imagecpp -> image_cpp),
        // иначе бейдж типа и фильтр по типу не узнают image-бэкенд.
        backendType: (window.Utils && typeof window.Utils.normalizeBackendType === 'function')
          ? window.Utils.normalizeBackendType(b.backendType || b.BackendType || b.type || b.Type || '')
          : (b.backendType || b.BackendType || b.type || b.Type || ''),
        // Round 18f: rps / avgResponseTime — fallback llama.cpp → ollama.
        rps: b.rps !== undefined ? b.rps : (lc.requestsPerSecond !== undefined ? lc.requestsPerSecond : (o.requestsPerSecond || 0)),
        avgResponseTime: b.avgResponseTime !== undefined ? b.avgResponseTime : (lc.avgResponseTime !== undefined ? lc.avgResponseTime : (o.avgResponseTime || 0)),
        freeSlots: b.freeSlots !== undefined ? b.freeSlots : (lc.freeSlots !== undefined ? lc.freeSlots : null),
        vram: b.vram || {
          totalGB: (g.memoryTotal || g.MemoryTotal || 0) / 1024,
          usedGB: (g.memoryUsed || g.MemoryUsed || 0) / 1024,
          usagePercent: (g.memoryTotal || g.MemoryTotal || 0) > 0 ? (g.memoryUsed || g.MemoryUsed || 0) / (g.memoryTotal || g.MemoryTotal || 1) * 100 : 0
        },
        memoryUsagePercent: b.memoryUsagePercent || (function() {
          var t = s.memoryTotal || s.MemoryTotal || 0, u = s.memoryUsed || s.MemoryUsed || 0;
          return t > 0 ? u / t * 100 : 0;
        })(),
        gpu: g, system: s, ollama: o, llamaCpp: lc, image: img
      });
    };

    if (data.cluster && data.cluster.backends) data.cluster.backends = data.cluster.backends.map(adapt);
    if (data.cluster && data.cluster.backendMetrics) data.cluster.backendMetrics = data.cluster.backendMetrics.map(adapt);

    var bk = MA.stableBackendOrder(data.cluster.backends || []);

    // Фильтрация бэкендов по effectiveBackendType (клиентский fallback)
    // Если effectiveBackendType задан — оставляем только бэкенды этого типа
    var effType = data.cluster.effectiveBackendType || '';
    // Также учитываем выбор пользователя из localStorage (синхронизация с BackendTypeFilter)
    var userType = localStorage.getItem('ollamalegion_backend_type') || '';
    if (userType && (userType === 'llama_cpp' || userType === 'ollama' || userType === 'image_cpp')) {
      effType = userType;
    }
    if (effType) {
      bk = bk.filter(function(b) {
        var bt = b.backendType || b.BackendType || b.backend_type || b.type || '';
        if (effType === 'llama_cpp') return bt === 'llama_cpp' || bt === 'image_cpp';
        if (effType === 'ollama') return bt === 'ollama' || bt === '' || bt === 'ollama_api' || bt === 'image_cpp';
        if (effType === 'image_cpp') return bt === 'image_cpp';
        return true;
      });
    }

    // Сохраняем backendEngine глобально для использования в бейджах и топологии
    MA.backendEngine = data.cluster.backendEngine || 'ollama_api';
    MA.effectiveBackendType = data.cluster.effectiveBackendType || '';
    var q = data.queueDetails || {};
    var rawSs = (data.sessions && data.sessions.sessions) || [];
    var recentClients = data.cluster.recentClients || [];
    var warmingUpModels = data.cluster.warmingUpModels || [];
    var act = bk.reduce(function(s, b) { return s + (b.activeRequests || 0); }, 0);
    // Защита от пустого блока клиентов: если есть активные запросы, но сессии пусты,
    // создаём синтетические сессии для отображения на топологии
    var ss = [].concat(rawSs);
    if (rawSs.length === 0 && act > 0) {
      ss = ss.concat(bk.filter(function(b) { return b.activeRequests > 0; }).map(function(b) {
        return {
          id: 'active-' + b.id,
          clientName: 'Active Request',
          clientIP: '—',
          model: (b.models && b.models[0]) || '?',
          backendId: b.id,
          requestCount: b.activeRequests,
          lastRequestAt: new Date().toISOString()
        };
      }));
    }
    // Synthetic sessions для RecentClients без сессии (например, при /api/tags)
    var existingNames = {};
    ss.forEach(function(s) { existingNames[s.clientName || ''] = true; });
    recentClients.forEach(function(rc) {
      if (!existingNames[rc.clientName]) {
        existingNames[rc.clientName] = true;
        ss.push({
          id: 'rc-' + rc.clientName + '-' + rc.clientIP,
          clientName: rc.clientName,
          clientIP: rc.clientIP,
          model: '-',
          backendId: null,
          requestCount: rc.requestCount || 1,
          lastRequestAt: rc.lastRequestAt || new Date().toISOString()
        });
      }
    });
    var pend = q.pending_count || 0, proc = q.processing_count || 0;
    // Дедуп по физической GPU (см. MonitorAggregates.aggregateVRAM): несколько
    // бэкендов одной машины читают nvidia-smi ОДНОЙ карты, и наивная сумма
    // показывала удвоенную память (16 GB на карте 8 GB).
    var vramAgg = aggVRAM(bk);
    var totV = vramAgg.totalGB;
    var usdV = vramAgg.usedGB;
    var tml = bk.reduce(function(s, b) { return s + ((b.models || []).length); }, 0);

    document.getElementById('statBackends').textContent = bk.length;
    document.getElementById('statActive').textContent = act;
    document.getElementById('statPending').textContent = pend;
    document.getElementById('statProcessing').textContent = proc;
    document.getElementById('statSessions').textContent = ss.length;

    var now = Date.now(), dt = (now - MA.lastTime) / 1000;
    var tr = data.cluster.totalRequests || 0;
    // Защита от first-frame spike: первый вызов только сохраняет значения, не вычисляя RPS
    if (!MA._rpsInitialized) {
      MA.requestRate = 0;
      MA._rpsInitialized = true;
    } else {
      MA.requestRate = Math.max(0, dt > 0.1 ? (tr - MA.lastTotalRequests) / dt : 0);
    }
    MA.lastTotalRequests = tr;
    MA.lastTime = now;

    document.getElementById('statRate').textContent = MA.requestRate.toFixed(1);
    document.getElementById('statRPSCluster').textContent = (data.cluster.rps || 0) > 0 ? data.cluster.rps.toFixed(1) : '0';
    document.getElementById('statVRAM').textContent = totV > 0 ? usdV.toFixed(1) + '/' + totV.toFixed(0) + 'G' : '—';
    document.getElementById('statModelsLoaded').textContent = tml;

    if (ss.length === 0 && act === 0 && pend === 0 && proc === 0) {
      document.getElementById('statusDot').className = 'dot';
      if (typeof window.setConnStatus === 'function') window.setConnStatus(T('monitor.status.idle'), 'green');
    }

    // Round 18f: hide Ollama-specific panels if no Ollama backends present.
    // Auto-Pull / Virtual Models — специфичны для Ollama workflow.
    // Если в кластере только llama.cpp — панели бесполезны, скрываем.
    //
    // R83: каждый рендер изолирован через safeRender — см. её комментарий.
    safeRender('hideOllamaOnlyPanels', function() { hideOllamaOnlyPanels(bk); });

    safeRender('autoPullPanel', function() { renderAutoPullPanel(data.autoPullConfig || null, data.autoPullStatus || null); });
    safeRender('modelOps', function() { renderModelOps(data.modelOps || null); });
    safeRender('dispatchStats', function() { renderDispatchStats(data.queueStats || {}); });
    safeRender('admissionStats', function() { renderAdmissionStats(data.queueStats || {}); });
    safeRender('placementPolicy', function() { renderPlacementPolicy(data.placement || {}); });
    safeRender('candidateBackends', function() { renderCandidateBackends(data.candidates || null); });
    safeRender('virtualModels', function() { renderVirtualModels(data.virtualModels || null); });
    safeRender('diagnose', function() { diagnose(data); });
    safeRender('clusterResources', function() { renderClusterResources(bk); });
    safeRender('modelsInMemory', function() { renderModelsInMemory(bk, ss); });
    safeRender('backends', function() { renderBackends(bk, data.modelOps || null); });
    // R-Image Phase 8: панель запросов к image-бэкендам. Передаём уже
    // отфильтрованный bk — если пользователь отфильтровал страницу по типу без
    // image-бэкендов, панель скроется вместе с их строками в таблице.
    safeRender('imageRequests', function() { renderImageRequests(data, bk); });
    // Добавляем бейджи типа бэкенда после рендера таблиц
    safeRender('backendTypeBadges', function() {
      if (window.BackendTypeBadges) {
          BackendTypeBadges.enhanceBackendsTable();
      }
    });
    // Round 32 #4 (2026-08-10): bind delegated click handler для inline Unload buttons
    // в таблице backends. handler attached ОДИН раз (через _ggufUnloadBound флаг)
    // чтобы не утекали listeners при каждом refresh'е таблицы.
    safeRender('bindUnloadHandlers', function() { bindUnloadHandlers(); });
    safeRender('diskNetwork', function() { renderDiskNetwork(bk); });
    // B-12: Render backend type switcher on first load
    safeRender('backendTypeSwitcher', function() { renderBackendTypeSwitcher(); });
    safeRender('loadFeasibility', function() { renderLoadFeasibility(bk); });
    safeRender('queue', function() { renderQueue(q); });
    safeRender('sessions', function() { renderSessions(ss); });
    safeRender('topology', function() {
      if (typeof window.updateTopology === 'function') window.updateTopology(bk, ss, q, recentClients, warmingUpModels);
    });
  }

  function diagnose(data) {
    var alerts = [], bks = data.cluster.backends || [], q = data.queueDetails || {}, all = q.all || [];
    var activeBks = bks.filter(function(b) { return b.status === 'active' || b.status === 'ready'; });
    var freeBks = activeBks.filter(function(b) {
      var m = b.maxConcurrentRequests || 10;
      return (b.activeRequests || 0) / m < 0.5;
    });
    if (q.pending_count > 0 && freeBks.length > 0) {
      alerts.push({
        level: 'red',
        action: 'rebalance',
        text: T('monitor.alert.pendingWithFree', { pending: q.pending_count, backends: freeBks.map(function(b) { return b.id; }).join(', ') })
      });
    }
    all.forEach(function(r) {
      if (r.status === 'pending' && r.enqueued) {
        var w = (Date.now() - new Date(r.enqueued).getTime()) / 1000;
        if (w > 10) alerts.push({ level: 'yellow', text: T('monitor.alert.stuckInQueue', { model: r.model, sec: w.toFixed(0) }) });
      }
    });
    (data.sessions && data.sessions.sessions || []).forEach(function(s) {
      if (!s.backendId) return;
      var b = bks.find(function(x) { return x.id === s.backendId; });
      if (!b) return;
      var m = b.maxConcurrentRequests || 10, l = (b.activeRequests || 0) / m;
      if (l > 0.8) {
        var alt = activeBks.filter(function(x) { return x.id !== b.id && (x.activeRequests || 0) / (x.maxConcurrentRequests || 10) < 0.5; });
        if (alt.length > 0) {
          alerts.push({
            level: 'yellow',
            text: T('monitor.alert.sessionOverloaded', { id: s.id.slice(0, 8), backend: b.id, load: (l * 100).toFixed(0), alt: alt.map(function(a) { return a.id; }).join(', ') })
          });
        }
      }
    });
    bks.forEach(function(b) {
      var m = b.maxConcurrentRequests || 10, l = (b.activeRequests || 0) / m;
      if (l >= 1.0) alerts.push({ level: 'red', text: T('monitor.alert.backendFull', { backend: b.id, active: b.activeRequests || 0, max: m }) });
    });
    // R83-fix (2026-10-09): «потенциал параллельности не раскрыт».
    //
    // Воркер держит модель с N слотами (cppworker max_slots -> runtimeModelSlots),
    // а балансер пропускает меньше (effectiveMaxConcurrentRequests). Это НЕ ошибка
    // и НЕ повод что-то менять автоматически: авто-привязка вместимости к слотам
    // меняет поведение admission-очереди, поэтому она opt-in
    // (LB_CAPACITY_FROM_MODEL_SLOTS=true). Задача баннера — сказать оператору, что
    // потенциал не раскрыт, и назвать конкретные настройки; решение остаётся за ним.
    //
    // Оба числа приходят из /api/v1/cluster (types.BackendMetrics): считать
    // «эффективную вместимость» в JS нельзя — при включённом флаге она равна
    // слотам, и предупреждение было бы ложным.
    bks.forEach(function(b) {
      var slots = b.runtimeModelSlots || 0;
      var eff = b.effectiveMaxConcurrentRequests || 0;
      if (slots > 1 && eff > 0 && eff < slots) {
        alerts.push({
          level: 'yellow',
          text: T('monitor.alert.parallelismUnused', {
            backend: b.id,
            slots: slots,
            effective: eff
          })
        });
      }
    });
    var ms = data.cluster.queue ? data.cluster.queue.max_size : 100, cs = q.current_size || 0;
    if (cs > ms * 0.8) alerts.push({ level: 'red', text: T('monitor.alert.queueFull', { pct: (cs / ms * 100).toFixed(0), size: cs, max: ms }) });
    var vramAlerts = aggVRAM(bks);
    var tv = vramAlerts.totalGB;
    var uv = vramAlerts.usedGB;
    if (tv > 0 && uv / tv > 0.85) alerts.push({ level: 'red', text: T('monitor.alert.vramFull', { pct: (uv / tv * 100).toFixed(0), used: uv.toFixed(1), total: tv.toFixed(0) }) });

    document.getElementById('alertContainer').innerHTML = alerts.map(function(a) {
      return '<div class="alert-banner alert-' + a.level + '"><span>' + (a.level === 'red' ? '❌' : '⚠️') + '</span><span>' + MA.esc(a.text) + '</span>' +
        (a.action === 'rebalance' ? '<button onclick="forceRebalance()" style="margin-left:8px;padding:2px 8px;font-size:11px;background:var(--danger);color:#fff;border:none;border-radius:4px;cursor:pointer">' + T('monitor.alert.rebalance') + '</button>' : '') +
        '</div>';
    }).join('');
  }

  function renderClusterResources(bk) {
    var g = document.getElementById('capacityGrid');
    if (!bk.length) { g.innerHTML = '<div style="color:var(--text-secondary);text-align:center;padding:8px">' + T('monitor.common.noData') + '</div>'; return; }
    var metrics = [
      { label: T('metrics.gpuUtil'), key: 'gpu', unit: '%', color: 'var(--accent)' },
      { label: T('metrics.vramUsed'), key: 'vram', unit: '%', color: 'var(--purple-accent)' },
      { label: T('metrics.cpuUsed'), key: 'system', subkey: 'cpuUsagePercent', unit: '%', color: 'var(--success)' },
      { label: T('metrics.ramUsed'), key: 'system', subkey: 'memoryUsagePercent', unit: '%', color: 'var(--warning)', useFlatKey: 'memoryUsagePercent' }
    ];
    g.innerHTML = metrics.map(function(m) {
      var vals = bk.map(function(b) {
        if (m.useFlatKey) return b[m.useFlatKey] || 0;
        if (m.subkey) return (b[m.key] && b[m.key][m.subkey]) || 0;
        return (b[m.key] && b[m.key].usagePercent) || 0;
      }).filter(function(v) { return v > 0; });
      var avg = vals.length ? vals.reduce(function(a, b) { return a + b; }, 0) / vals.length : 0;
      var max = vals.length ? Math.max.apply(null, vals) : 0;
      var slKey = m.key === 'system' ? (m.useFlatKey ? 'ram' : 'cpu') : m.key;
      var clusterSl = '';
      if (window.Sparkline) {
        try {
          var avgVal = avg, maxVal = max;
          clusterSl = '<div class="sl-cluster-cell" style="margin-top:4px">' + window.Sparkline.renderCluster(slKey, avgVal, maxVal, m.color) + '</div>';
        } catch (e) { clusterSl = ''; }
      }
      return '<div class="capacity-card"><h4>' + MA.esc(m.label) + '</h4><div class="capacity-bar"><div class="bar-track-wide"><div class="bar-fill-wide" style="width:' + avg + '%;background:' + m.color + '"></div></div><span class="capacity-val">avg ' + avg.toFixed(0) + m.unit + '</span></div><div class="capacity-bar"><div class="bar-track-wide"><div class="bar-fill-wide" style="width:' + max + '%;background:' + m.color + ';opacity:0.5"></div></div><span class="capacity-val">max ' + max.toFixed(0) + m.unit + '</span></div>' + clusterSl + '</div>';
    }).join('') +
      '<div class="capacity-card"><h4>' + T('monitor.capacity.freeSlots') + '</h4>' +
      bk.map(function(b) {
        var m = b.maxConcurrentRequests || 10, a = b.activeRequests || 0, f = m - a, p = a / m * 100;
        var c = p >= 80 ? 'var(--danger)' : p >= 50 ? 'var(--warning)' : 'var(--success)';
        return '<div class="capacity-bar"><span style="font-size:11px;min-width:80px">' + MA.esc(b.id) + '</span><div class="bar-track-wide"><div class="bar-fill-wide" style="width:' + p + '%;background:' + c + '"></div></div><span class="capacity-val">' + f + '/' + m + '</span></div>';
      }).join('') + '</div>' +
      '<div class="capacity-card"><h4>' + T('monitor.capacity.vramPerBackend') + '</h4>' +
      bk.map(function(b) {
        var t = b.vram ? b.vram.totalGB : 0, u = b.vram ? b.vram.usedGB : 0, p = t > 0 ? u / t * 100 : 0;
        var c = p >= 85 ? 'var(--danger)' : p >= 60 ? 'var(--warning)' : 'var(--success)';
        return '<div class="capacity-bar"><span style="font-size:11px;min-width:80px">' + MA.esc(b.id) + '</span><div class="bar-track-wide"><div class="bar-fill-wide" style="width:' + p + '%;background:' + c + '"></div></div><span class="capacity-val">' + u.toFixed(1) + '/' + t.toFixed(0) + 'G</span></div>';
      }).join('') + '</div>';
  }

  function renderModelsInMemory(bk, ss) {
    var mm = {};
    // Local models from backends
    bk.forEach(function(b) {
      // R-Image Phase 5: модели image-воркера лежат в backend.image.models.
      // b.models у image-бэкенда появляется только после adapt(); страницу может
      // отрисовать и код без adapt (демо-режим, старый кэш скриптов), поэтому
      // читаем оба источника и не дублируем имена.
      var imgModels = imageModelsOf(b);
      var list = (b.models || []).slice();
      imgModels.forEach(function(im) {
        var nm = im && (im.name || im.Name);
        if (nm && list.indexOf(nm) < 0) list.push(nm);
      });
      list.forEach(function(m) {
        if (!mm[m]) mm[m] = { bks: [], sc: 0, cloud: false, image: null };
        mm[m].bks.push(b.id);
        // Метаданные image-модели: состояние, размер, оценка VRAM, семейство.
        var im2 = null;
        for (var i = 0; i < imgModels.length; i++) {
          if ((imgModels[i].name || imgModels[i].Name) === m) { im2 = imgModels[i]; break; }
        }
        if (im2) {
          mm[m].image = {
            state: im2.state || '',
            family: im2.family || '',
            sizeBytes: imageSizeBytesOf(im2),
            vramMb: Number(im2.vramEstimateMb || im2.vram_estimate_mb || 0) || 0,
            activeQueries: im2.activeQueries || 0
          };
        }
      });
    });
    // Add models from sessions (including cloud models not loaded locally)
    ss.forEach(function(s) {
      if (!s.model) return;
      if (!mm[s.model]) mm[s.model] = { bks: [], sc: 0, cloud: MA.isCloudModel(s.model), image: null };
      if (MA.isCloudModel(s.model)) mm[s.model].cloud = true;
      mm[s.model].sc++;
    });
    var models = Object.keys(mm).map(function(k) { return { name: k, info: mm[k] }; }).sort(function(a, b) { return b.info.sc - a.info.sc; });
    document.getElementById('modelsCount').textContent = models.length;
    var tb = document.querySelector('#modelsTable tbody');
    if (!models.length) { tb.innerHTML = '<tr><td colspan="5" style="color:var(--text-secondary);text-align:center;padding:16px">' + T('monitor.common.noData') + '</td></tr>'; return; }
    tb.innerHTML = models.map(function(m) {
      var fb = bk.find(function(b) { return (b.models || []).indexOf(m.name) >= 0; });
      var vg = fb && fb.vram && fb.vram.usedGB ? (fb.vram.usedGB / (fb.models || []).length).toFixed(1) : (m.info.cloud ? '☁️' : '—');
      // R-Image Phase 5: у image-модели есть собственная оценка VRAM
      // (vramEstimateMb) - она честнее, чем деление VRAM бэкенда на число моделей.
      var imgMeta = m.info.image;
      var vramCell;
      if (m.info.cloud) vramCell = '☁️ N/A';
      else if (imgMeta && imgMeta.vramMb > 0) {
        vramCell = '~' + (imgMeta.vramMb >= 1024
          ? (imgMeta.vramMb / 1024).toFixed(1) + ' GB'
          : Math.round(imgMeta.vramMb) + ' MB');
      } else vramCell = '~' + vg + ' GB';
      // Find model info from backends for expiresAt and digest
      var expDate = '';
      var digestShort = '';
      if (fb && fb.ollama && fb.ollama.runningModels) {
        var mm = fb.ollama.runningModels.find(function(r) { return r.name === m.name || r.model === m.name; });
        if (mm) {
          expDate = mm.expiresAt || mm.ExpiresAt || mm.expires_at || '';
          var dig = mm.digest || mm.Digest || mm.digest || '';
          if (dig) digestShort = dig.substring(0, 12);
        }
      }
      var expHTML = '';
      if (expDate) {
        var expObj = new Date(expDate);
        var diff = expObj.getTime() - Date.now();
        var label = diff < 0 ? '⌛' + T('monitor.models.expired') : (diff < 60000 ? '⌛' + Math.round(diff/1000) + 's' : (diff < 3600000 ? '⌛' + Math.round(diff/60000) + T('renderers.minutes') : '⌛' + expDate.substring(0, 10)));
        expHTML = '<span title="' + MA.esc(expDate) + '" style="font-size:10px;color:var(--warning);margin-left:4px">' + label + '</span>';
      }
      var digHTML = digestShort ? ' <code style="font-size:9px;background:var(--bg-secondary);padding:1px 3px;border-radius:3px" title="' + MA.esc('Digest: ' + digestShort) + '">' + MA.esc(digestShort) + '</code>' : '';
      // R-Image Phase 5: состояние image-модели видно в колонке Model - иначе
      // загруженная и незагруженная SD-модель выглядели одинаково.
      var imgStateHTML = (imgMeta && imgMeta.state)
        ? ' <span class="badge" style="background:rgba(74,222,128,0.12);color:#4ade80;font-size:9px;padding:0 4px;border-radius:2px" title="' +
          MA.esc(T('image.worker_state')) + '">🎨 ' + MA.esc(imageStateText(imgMeta.state)) + '</span>'
        : '';
      var nameCell = '<strong>' + MA.esc(m.name) + '</strong>' + digHTML + imgStateHTML + (m.info.cloud ? ' <span style="font-size:10px;color:var(--accent);background:rgba(59,130,246,0.12);padding:1px 5px;border-radius:4px">☁️ ' + T('renderers.cloud') + '</span>' : '');
      // B-11: Добавляем бейдж backend-типа для каждого бэкенда
      var bkCell = m.info.cloud && m.info.bks.length === 0
        ? '<span class="badge badge-blue">☁️ cloud</span>'
        : m.info.bks.map(function(bid) {
            var fbBk = bk.find(function(x) { return x.id === bid; });
            var btBadge = '';
            if (fbBk && window.Utils && window.Utils.getBackendTypeBadge) {
              btBadge = ' ' + window.Utils.getBackendTypeBadge(fbBk);
            } else if (fbBk) {
              var btType = fbBk.backendType || fbBk.type || '';
              if (btType === 'llama_cpp') {
                btBadge = ' <span class="badge" style="background:#ff6d0020;border:1px solid #ff6d00;color:#ff6d00;font-size:9px;padding:0 3px;border-radius:2px">🦒</span>';
              } else if (isImageBackendType(btType)) {
                // R-Image Phase 5: image_cpp раньше не имел ветки - у image-модели
                // в колонке Backends не было бейджа типа вообще.
                btBadge = ' <span class="badge" style="background:#4ade8020;border:1px solid #4ade80;color:#4ade80;font-size:9px;padding:0 3px;border-radius:2px">🎨</span>';
              } else if (btType === 'ollama' || !btType) {
                btBadge = ' <span class="badge" style="background:#1a73e820;border:1px solid #1a73e8;color:#1a73e8;font-size:9px;padding:0 3px;border-radius:2px">🦙</span>';
              }
            }
            return '<span class="badge badge-purple">' + MA.esc(bid) + btBadge + '</span>';
          }).join(' ');
      return '<tr><td>' + nameCell + '</td><td>' + bkCell + '</td><td class="col-right">' + m.info.sc + '</td><td class="col-right">' + vramCell + '</td><td class="col-right">' + (m.info.sc > 0 ? (m.info.cloud ? '☁️ ' + m.info.sc : '🔥 ' + m.info.sc) : '—') + '</td><td class="col-right" style="font-size:11px">' + expHTML + '</td></tr>';
    }).join('');
    // Update colspan for no-data row (was 5, now 6 with expiresAt column)
    if (!models.length) { 
      tb.innerHTML = '<tr><td colspan="6" style="color:var(--text-secondary);text-align:center;padding:16px">' + T('monitor.common.noData') + '</td></tr>'; 
    }

  }

  // ===== Unload handler (Round 32 #4, 2026-08-10) =====
  // Delegated click handler для inline Unload buttons в таблице backends.
  // При клике — вызывает GgufApi.manageModel(backendId, 'unload', modelName)
  // (тот же path что GGUF page использует) и показывает toast о результате.
  // При успехе — force refresh монитора (30s poll слишком медленный).
  function bindUnloadHandlers() {
    var tbody = document.querySelector('#backendsTable tbody');
    if (!tbody || tbody._ggufUnloadBound) return;
    tbody._ggufUnloadBound = true;
    tbody.addEventListener('click', function(e) {
      var btn = e.target.closest('.monitor-unload-btn');
      if (!btn) return;
      e.preventDefault();
      e.stopPropagation();
      var backendId = btn.getAttribute('data-backend');
      var modelName = btn.getAttribute('data-model');
      if (!backendId || !modelName) return;
      if (!window.GgufApi || !window.GgufApi.manageModel) {
        if (window.showToast) window.showToast('GgufApi not available', 'error');
        return;
      }
      btn.disabled = true;
      var origHtml = btn.innerHTML;
      btn.innerHTML = '<i class="fas fa-spinner fa-spin"></i>';
      if (window.showToast) window.showToast('Unloading ' + modelName + '...', 'info');
      window.GgufApi.manageModel(backendId, 'unload', modelName).then(function(result) {
        if (result && result.success === false) {
          if (window.showToast) window.showToast('Unload failed: ' + (result.error || 'unknown'), 'error');
        } else {
          if (window.showToast) window.showToast('Unloaded ' + modelName, 'success');
          // Force refresh — 30s poll слишком медленный.
          if (MA && typeof MA.refresh === 'function') MA.refresh();
        }
      }).catch(function(err) {
        if (window.showToast) window.showToast('Unload error: ' + (err && err.message || err), 'error');
      }).finally(function() {
        btn.disabled = false;
        btn.innerHTML = origHtml;
      });
    });
  }

  function renderBackends(bk, modelOps) {
    document.getElementById('backendCount').textContent = bk.length;
    var tb = document.querySelector('#backendsTable tbody');
    if (!bk.length) { tb.innerHTML = '<tr><td colspan="17" style="color:var(--text-secondary);text-align:center;padding:16px">' + T('monitor.common.noData') + '</td></tr>'; return; }
    // Build a lookup: backendId -> array of operations running on it
    var opsByBackend = {};
    if (modelOps) {
      var ops = modelOps.operations || [];
      if (!Array.isArray(ops)) ops = [];
      ops.forEach(function(op) {
        var bid = op.backendId || op.backendID;
        if (bid) {
          if (!opsByBackend[bid]) opsByBackend[bid] = [];
          opsByBackend[bid].push(op);
        }
      });
    }
    tb.innerHTML = bk.map(function(b) {
      // R-Image Phase 8 (2026-10-03): у image-бэкенда счётчики запросов лежат в
      // backend.image.requests (их пишет ImageRouter балансера), а не в
      // ollama/llamaCpp. Без этого блока строка image-воркера показывала
      // RPS = «-», Avg RT = «-» и Active = «0/10» даже когда генерации шли:
      // по таблице нельзя было понять, работает ли воркер. Для текстовых
      // бэкендов ветка не меняет ничего (imgReq там всегда null).
      var isImgRow = isImageBackendType(b.backendType || b.BackendType || b.backend_type || b.type || '') || isImageBackend(b);
      var imgReq = (isImgRow && b.image && b.image.requests) ? b.image.requests : null;
      var mr = b.maxConcurrentRequests || 10, a = b.activeRequests || 0;
      if (imgReq && imgReq.inFlight != null) a = imgReq.inFlight;
      var gu = (b.gpu && b.gpu.usagePercent != null) ? b.gpu.usagePercent : 0;
      var vu = (b.vram && b.vram.usagePercent != null) ? b.vram.usagePercent : (b.vramUsagePercent || 0);
      var cu = (b.system && b.system.cpuUsagePercent != null) ? b.system.cpuUsagePercent : 0;
      var ru = (b.memoryUsagePercent != null) ? b.memoryUsagePercent : ((b.system && b.system.memoryUsagePercent != null) ? b.system.memoryUsagePercent : 0);
      var sc = (b.score || b.weight || 0).toFixed(2);
      var scs = b.status === 'healthy' || b.status === 'active' || b.status === 'ready' ? 'badge-green' : (b.status === 'error' || b.status === 'unhealthy' ? 'badge-red' : (b.status === 'ollama_unavailable' ? 'badge-orange' : 'badge-yellow'));
      var up = b.lastSeen ? MA.fmtDur(Date.now() - new Date(b.lastSeen).getTime()) : '-';
      var rps = (b.ollama && b.ollama.requestsPerSecond != null) ? b.ollama.requestsPerSecond : 0;
      if (imgReq && imgReq.rps != null) rps = imgReq.rps;
      var avgRT = (b.ollama && b.ollama.avgResponseTime != null && b.ollama.avgResponseTime > 0) ? b.ollama.avgResponseTime.toFixed(0) + 'ms' : '-';
      if (imgReq && imgReq.avgDurationMs > 0) avgRT = fmtImageDuration(imgReq.avgDurationMs);
      var reqCap = (b.prediction && b.prediction.requestCapacity != null) ? b.prediction.requestCapacity.toFixed(0) + '%' : '-';
      // GPU hidden metrics tooltip
      var gpu = b.gpu || {};
      var gpuHidden = (gpu.powerLimit > 0 || gpu.gpuClock > 0 || gpu.memClock > 0)
        ? ' <span title="Limit: ' + (gpu.powerLimit || '-') + 'W | GPU: ' + (gpu.gpuClock || '-') + ' MHz | Mem: ' + (gpu.memClock || '-') + ' MHz">⚡</span>' : '';
      // CPU details
      var sys = b.system || {};
      var cpu = sys.cpu || {};
      var loadAvg = cpu.loadAverage1 != null ? cpu.loadAverage1.toFixed(2) : (sys.loadAverage1 != null ? sys.loadAverage1.toFixed(2) : '-');
      var cores = cpu.coreCount || '-';
      var cpuModel = cpu.model ? cpu.model.split(' ').slice(0, 2).join(' ') : '';
      var throttled = cpu.throttled != null ? (cpu.throttled ? '⚠️' : T('common.ok')) : '-';
      var cpuHint = 'Load: ' + loadAvg + ' | Cores: ' + cores + (cpuModel ? ' (' + cpuModel + ')' : '') + ' | Throttle: ' + throttled;
      // Loading indicator — show operations running on this backend
      var loadingCell = '';
      var backendOps = opsByBackend[b.id];
      if (backendOps && backendOps.length > 0) {
        var loadingIcons = backendOps.map(function(op) {
          var opType = op.operation || 'op';
          var modelName = op.modelName || '';
          return '<span class="badge badge-yellow" style="font-size:10px;animation:pulse 1.5s infinite" title="' + MA.esc(opType) + ': ' + MA.esc(modelName) + '">⏳ ' + MA.esc(opType) + '</span>';
        }).join(' ');
        loadingCell = loadingIcons;
      } else {
        loadingCell = '<span style="font-size:11px;color:var(--text-secondary)">—</span>';
      }
      // Per-backend type badge (🦙 Ollama / 🦒 llama.cpp / 🎨 image.cpp).
      // R-Image Phase 5: image_cpp раньше не попадал ни в одну ветку, поэтому
      // бейджа типа у image-бэкенда не было вообще (и по строке нельзя было
      // понять, что это stable-diffusion.cpp, а не Ollama).
      var bt = b.backendType || '';
      var isImg = isImageBackendType(bt) || isImageBackend(b);
      var btType = isImg ? 'image_cpp' : (bt || 'ollama');
      var typeBadge = backendTypeBadgeHtml(b, btType);
      // R-Image Phase 5: порт воркера и его состояние - прямо в строке.
      // У image-бэкенда нет валидных RPS/avgRT (воркер их не отдаёт), поэтому
      // без этих бейджей строка выглядела «мёртвой», хотя воркер работает.
      var imageBadges = '';
      if (isImg) {
        var imgPort = imagePortOf(b);
        var imgBlock = imageBlockOf(b);
        var imgState = String(imgBlock.state || '').toLowerCase();
        var imgStyle = 'background:rgba(74,222,128,0.12);color:#4ade80;border-color:rgba(74,222,128,0.3)';
        imageBadges += ' <span class="badge" style="' + imgStyle + '" title="' + MA.esc(T('image.worker_port')) + '">🔌 ' +
          (imgPort > 0 ? imgPort : MA.esc(T('image.port_unset'))) + '</span>';
        if (imgState) {
          imageBadges += ' <span class="badge" style="' + imgStyle + '" title="' + MA.esc(T('image.worker_state')) + '">' +
            MA.esc(imageStateText(imgState)) + '</span>';
        }
        if (imgBlock.lastError) {
          imageBadges += ' <span class="badge badge-orange" title="' + MA.esc(T('image.last_error') + ': ' + imgBlock.lastError) + '">⚠️</span>';
        }
      }
// Sparkline helper: record + render per backend (no-op if Sparkline not loaded).
      function sl(bid, key, color) {
        if (!window.Sparkline) return '';
        try {
          // R-Image Phase 8: у image-бэкенда истории avg RT в ollama нет -
          // берём avgDurationMs из image.requests, иначе график был бы нулевым.
          var avgRTraw = (b.ollama && b.ollama.avgResponseTime != null) ? b.ollama.avgResponseTime
            : ((imgReq && imgReq.avgDurationMs) ? imgReq.avgDurationMs : 0);
          window.Sparkline.recordMetricsHistory(bid, { gpu: gu, vram: vu, cpu: cu, ram: ru, rps: rps, avgRt: avgRTraw });
          return window.Sparkline.render(bid, key, color);
        } catch (e) { return ''; }
      }
      // === Round 32 #4 (2026-08-10): inline Unload button в Models cell ===
      // Раньше (до фикса) Monitor показывал бэйджи с именами моделей но без action
      // buttons — пользователь видел "загружено N моделей" но не мог ничего с этим
      // сделать прямо из Monitor'а. Приходилось идти на GGUF page → выбирать backend →
      // кликать Unload там. Теперь inline кнопка ✕ рядом с каждым именем.
      //
      // Использует GgufApi.manageModel(backendId, 'unload', modelName) — тот же
      // path что GGUF page (per-operation timeout, 30s для unload).
      //
      // R-Image Phase 5: для image-бэкенда кнопку unload НЕ рисуем - 
      // GgufApi.manageModel шлёт ollama/cppworker-путь (/api/v1/backends/{id}/models),
      // которого image-воркер не понимает; управление image-моделями живёт на
      // странице «Изображения» (/api/v1/image/models/...). Вместо кнопки
      // показываем состояние модели и её оценку VRAM в title.
      var modelsCell;
      if (isImg) {
        var imgModelsList = imageModelsOf(b);
        if (!imgModelsList.length) {
          modelsCell = '<span style="font-size:11px;color:var(--text-secondary)">—</span>';
        } else {
          modelsCell = imgModelsList.slice(0, 3).map(function (m) {
            var st = String(m.state || '').toLowerCase();
            var col = (st === 'loaded') ? '#4ade80'
              : (st === 'loading') ? '#f0ad4e'
              : (st === 'error' || st === 'failed') ? '#ef4444'
              : 'var(--text-secondary)';
            var sub = [];
            var szTxt = fmtBytesShort(imageSizeBytesOf(m));
            if (szTxt) sub.push(szTxt);
            var vramMb = Number(m.vramEstimateMb || m.vram_estimate_mb || 0);
            if (vramMb > 0) sub.push('VRAM ' + (vramMb >= 1024 ? (vramMb / 1024).toFixed(1) + ' GB' : Math.round(vramMb) + ' MB'));
            sub.push(imageStateText(st));
            return '<span class="badge" style="background:rgba(74,222,128,0.10);color:' + col + ';border-color:' + col + '" title="' +
              MA.esc(String(m.name || '') + ' — ' + sub.join(' | ')) + '">🎨 ' + MA.esc(m.name) +
              ' <span style="opacity:0.8">(' + MA.esc(imageStateText(st)) + ')</span></span>';
          }).join(' ');
          if (imgModelsList.length > 3) modelsCell += ' +' + (imgModelsList.length - 3);
        }
      } else {
        modelsCell = (b.models || []).slice(0, 3).map(function(m) {
          return '<span class="badge" style="background:rgba(168,85,247,0.12);color:var(--purple-accent);border-color:rgba(168,85,247,0.2)">' +
            MA.esc(m) +
            ' <button class="monitor-unload-btn" data-backend="' + MA.esc(b.id) + '" data-model="' + MA.esc(m) + '" ' +
            'title="Unload ' + MA.esc(m) + '" ' +
            'style="background:transparent;border:none;color:inherit;cursor:pointer;padding:0 2px;font-size:11px;line-height:1;opacity:0.7;">' +
            '<i class="fas fa-times"></i></button>' +
          '</span>';
        }).join(' ');
      }
      // R-Image Phase 8: три колонки счётчиков image-запросов (total/ok/failed).
      // У не-image бэкендов таких данных нет вовсе, поэтому там прочерк - так
      // сразу видно, что колонки относятся только к image_cpp. Считаем
      // завершённые исходы (total включает и ещё не завершённые accepted).
      var imgReqCells = imgReq
        ? '<td class="col-right">' + (imgReq.total || 0) + '</td>' +
          '<td class="col-right" style="color:var(--success)">' + (imgReq.ok || 0) + '</td>' +
          '<td class="col-right" style="color:' + ((imgReq.failed || 0) > 0 ? 'var(--danger)' : 'var(--text-secondary)') + '">' + (imgReq.failed || 0) + '</td>'
        : '<td class="col-right">-</td><td class="col-right">-</td><td class="col-right">-</td>';
      return '<tr data-backend-type="' + MA.esc(btType) + '"><td><strong>' + MA.esc(b.id) + '</strong>' + imageBadges + typeBadge + '</td><td><span class="badge ' + scs + '">' + b.status + '</span></td><td>' + MA.bar(gu) + ' ' + gu.toFixed(0) + '%' + gpuHidden + '<div class="sl-cell">' + sl(b.id, 'gpu', 'var(--accent)') + '</div></td><td>' + MA.bar(vu) + ' ' + vu.toFixed(0) + '%<div class="sl-cell">' + sl(b.id, 'vram', 'var(--purple-accent)') + '</div></td><td title="' + cpuHint + '">' + MA.bar(cu) + ' ' + cu.toFixed(0) + '%<div class="sl-cell">' + sl(b.id, 'cpu', 'var(--success)') + '</div></td><td>' + MA.bar(ru) + ' ' + ru.toFixed(0) + '%<div class="sl-cell">' + sl(b.id, 'ram', 'var(--warning)') + '</div></td><td class="col-right">' + a + '/' + mr + '</td><td class="col-right">' + (rps > 0 ? rps.toFixed(1) : '-') + '<div class="sl-cell">' + sl(b.id, 'rps', 'var(--info)') + '</div></td><td class="col-right">' + avgRT + '<div class="sl-cell">' + sl(b.id, 'avgRt', 'var(--text-secondary)') + '</div></td><td class="col-right">' + reqCap + '</td><td class="col-right">' + sc + '</td><td style="font-size:11px">' + loadingCell + '</td><td>' + modelsCell + '</td>' + imgReqCells + '<td class="col-right">' + up + '</td></tr>';
    }).join('');
  }

  // renderAdmissionStats — R70: admission-очередь (кто ждёт свободный слот).
  // Данные — GET /api/v1/queue/stats → поле "admission" (LB_ADMISSION_WAIT_SEC,
  // per-session справедливость, keepalive для streaming-ожидающих).
  function renderAdmissionStats(qs) {
    var a = (qs && qs.admission) || {};
    var enabled = a.enabled !== false;
    updateText('admWaiting', a.waiting || 0);
    updateText('admActive', a.active_sessions || 0);
    updateText('admServed', a.served_total || 0);
    updateText('admTimeouts', a.timeout_total || 0);
    var avg = a.avg_wait_ms || 0;
    updateText('admAvgWait', avg > 0 ? MA.fmtDur(avg) : '—');
    var waitMax = a.wait_max_sec || 0;
    updateText('admWaitMax', enabled ? (waitMax > 0 ? waitMax + T('monitor.admission.secSuffix') : '∞') : T('monitor.admission.off'));

    var sessions = a.sessions || [];
    var el = document.getElementById('admSessions');
    if (el) {
      el.textContent = sessions.length
        ? T('monitor.admission.sessions') + ': ' + sessions.join(', ')
        : '';
    }
  }

  // renderPlacementPolicy — R77: placement policy (стратегия размещения по
  // моделям). Данные — GET /api/v1/placement: enabled/fallback/operatingMode,
  // decisions[] (strategy/source/degraded/reason/matchedRule), replication
  // (managerReady/groups/candidates) и warnings валидации конфига.
  function renderPlacementPolicy(pl) {
    if (!document.getElementById('placementPanel') && !document.getElementById('placementStats')) return;
    var enabled = !!(pl && pl.enabled === true);
    updateText('plEnabled', enabled ? T('monitor.placement.on') : T('monitor.placement.off'));
    updateText('plFallback', (pl && pl.fallback) || 'error');
    updateText('plMode', (pl && pl.operatingMode) || 'standard');
    var decisions = (pl && pl.decisions) || [];
    // Первая строка отчёта — глобальный дефолт («*»), отдельно её не считаем.
    var modelCount = decisions.filter(function(d) { return d && d.model !== '*'; }).length;
    updateText('plModels', modelCount);

    var warns = (pl && pl.warnings) || [];
    updateText('plWarnings', warns.length);

    var repl = (pl && pl.replication) || {};
    updateText('plManager', repl.managerReady ? T('monitor.placement.ready') : T('monitor.placement.absent'));

    var groups = repl.groups || {};
    var groupLines = Object.keys(groups).map(function(name) {
      var g = groups[name] || {};
      var cand = (g.candidates || []).join(', ') || '—';
      return name + ': ' + (g.hasGroup ? T('monitor.placement.groupYes') : T('monitor.placement.groupNo')) + ' [' + cand + ']';
    });
    var gl = document.getElementById('plGroups');
    if (gl) {
      gl.textContent = groupLines.length ? T('monitor.placement.groups') + ': ' + groupLines.join(' · ') : '';
    }
    var wl = document.getElementById('plWarnList');
    if (wl) {
      wl.textContent = warns.length ? T('monitor.placement.warnings') + ': ' + warns.join(' · ') : '';
    }

    var tb = document.querySelector('#placementTable tbody');
    if (!tb) return;
    if (!decisions.length) {
      tb.innerHTML = '<tr><td colspan="5" style="color:var(--text-secondary);text-align:center;padding:16px">' + T('monitor.common.noData') + '</td></tr>';
      return;
    }
    tb.innerHTML = decisions.map(function(d) {
      var strat = (d && d.strategy) || '-';
      var badge = 'badge-yellow';
      if (d && d.degraded) badge = 'badge-red';
      else if (d && d.executable === false) badge = 'badge-red';
      else if (strat === 'single') badge = 'badge-green';
      else if (strat === 'pool' || strat === 'replicated' || strat === 'auto') badge = 'badge-blue';
      var flags = [];
      if (d && d.degraded) flags.push(T('monitor.placement.degraded'));
      if (d && d.executable === false) flags.push(T('monitor.placement.notExecutable'));
      if (d && d.refined) flags.push(T('monitor.placement.autoResolved'));
      var reason = MA.esc((d && d.reason) || '');
      if (d && d.matchedRule) {
        reason += '<br><span style="color:var(--text-secondary)">' + MA.esc(d.matchedRule) + '</span>';
      }
      return '<tr><td><strong>' + MA.esc((d && d.model) || '*') + '</strong></td>' +
        '<td><span class="badge ' + badge + '">' + MA.esc(strat) + '</span>' +
        (flags.length ? '<div style="font-size:10px;color:var(--text-secondary);margin-top:2px">' + flags.join(', ') + '</div>' : '') + '</td>' +
        '<td>' + MA.esc((d && d.source) || '-') + '</td>' +
        '<td>' + MA.esc((d && d.fallback) || '-') + '</td>' +
        '<td style="font-size:11px">' + reason + '</td></tr>';
    }).join('');
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
        '<td style="font-size:11px;white-space:nowrap">' + escHtml(fmtImageTime(r.at)) + '</td>' +
        '<td>' + escHtml(r.backendId || '—') + '</td>' +
        '<td><code style="font-size:11px">' + escHtml(r.path || '—') + '</code></td>' +
        '<td>' + escHtml(r.model || '—') +
          (images > 0 ? ' <span style="font-size:10px;color:var(--text-secondary)">×' + images + '</span>' : '') + '</td>' +
        '<td class="col-right">' + escHtml(sizeTxt) + '</td>' +
        '<td class="col-right">' + (steps > 0 ? steps : '—') + '</td>' +
        '<td class="col-right">' + escHtml(fmtImageDuration(r.durationMs)) + '</td>' +
        '<td><span class="badge ' + imageRequestStatusBadge(st) + '">' + escHtml(imageRequestStatusText(st)) + '</span></td>' +
        '</tr>';
    }).join('');
  }

  function renderQueue(q) {    var all = q.all || [];
    document.getElementById('queueCount').textContent = all.length;
    var tb = document.querySelector('#queueTable tbody');
    if (!all.length) { tb.innerHTML = '<tr><td colspan="6" style="color:var(--text-secondary);text-align:center;padding:16px">' + T('monitor.common.noData') + '</td></tr>'; return; }
    tb.innerHTML = all.map(function(r, i) {
      var w = r.enqueued ? Date.now() - new Date(r.enqueued).getTime() : 0;
      var st = r.status || (r.target ? 'processing' : 'pending');
      return '<tr><td>' + (i + 1) + '</td><td>' + MA.esc(r.model || '-') + '</td><td>' + MA.esc(r.target || 'Auto') + '</td><td><span class="badge ' + (st === 'processing' ? 'badge-green' : 'badge-yellow') + '">' + st + '</span></td><td class="col-right">' + MA.fmtDur(w) + '</td><td>' + MA.esc((r.sessionId || '-').slice(0, 12)) + '</td></tr>';
    }).join('');
  }

  function renderSessions(ss) {
    var visibleSessions = ss.filter(function(s) {
      if (MA.isTechnicalClient(s.clientName, s.userAgent)) return false;
      var idleMs = s.lastRequestAt ? (Date.now() - new Date(s.lastRequestAt).getTime()) : 999999;
      // Не скрываем сессии с активным streaming даже при idle > 5 минут
      if (s.hasActiveStream) return true;
      if ((s.requestCount || 0) === 0 && idleMs > 300000) return false;
      return true;
    });

    document.getElementById('sessionCount').textContent = visibleSessions.length;
    var tb = document.querySelector('#sessionsTable tbody');
    if (!visibleSessions.length) { tb.innerHTML = '<tr><td colspan="7" style="color:var(--text-secondary);text-align:center;padding:16px">' + T('monitor.common.noData') + '</td></tr>'; return; }
    tb.innerHTML = visibleSessions.map(function(s) {
      var idl = s.lastRequestAt ? MA.fmtDur(Date.now() - new Date(s.lastRequestAt).getTime()) : '-';
      var modelCell = MA.esc(s.model || '-');
      if (MA.isCloudModel(s.model)) modelCell += ' ☁️';
      var ipCol = MA.ipColor(s.clientIP);
      var ci = MA.getCI(s.clientName);
      // Индикатор активного стриминга: зелёная точка для активного, серая для idle
      var streamIndicator = '';
      if (s.hasActiveStream) {
        streamIndicator = ' <span title="Active streaming" style="display:inline-block;width:8px;height:8px;border-radius:50%;background:#22c55e;margin-left:4px;box-shadow:0 0 6px #22c55e"></span>';
      } else if (s.requestCount > 0) {
        streamIndicator = ' <span title="Idle (last: ' + idl + ')" style="display:inline-block;width:8px;height:8px;border-radius:50%;background:#94a3b8;margin-left:4px"></span>';
      }
      return '<tr><td>' + MA.esc(s.id.slice(0, 16)) + '…</td><td>' + MA.esc(s.backendId || '—') + streamIndicator + '</td><td>' + modelCell + '</td><td class="col-right">' + (s.requestCount || 0) + '</td><td class="col-right">' + idl + '</td><td><span style="color:' + ipCol + ';font-weight:600">' + MA.esc(s.clientIP || '-') + '</span></td><td>' + ci + ' <span style="color:' + ipCol + '">' + MA.esc(s.clientName || '-') + '</span></td></tr>';
    }).join('');
  }

  /**
   * Round 18f: hideOllamaOnlyPanels — скрывает Ollama-специфичные панели когда
   * в кластере нет Ollama-бэкендов. Это «Auto-Pull» (Ollama workflow) и
   * «Virtual Models» (Ollama slicer). Для llama.cpp-only setup эти панели
   * бесполезны и засоряют экран.
   *
   * Также скрываем Dispatch panel если routing mode = simple (нет scoring).
   * И AutoPull — если нет активных pulls и нет ollama-бэкендов.
   */
  function hideOllamaOnlyPanels(bk) {
    var hasOllama = bk.some(function (b) {
      var bt = b.backendType || b.BackendType || b.backend_type || b.type || '';
      return bt === 'ollama' || bt === '' || bt === 'ollama_api';
    });
    var hasLlamaCpp = bk.some(function (b) {
      var bt = b.backendType || b.BackendType || b.backend_type || b.type || '';
      return bt === 'llama_cpp';
    });

    // Auto-Pull — только Ollama. Скрываем если нет ollama-бэкендов.
    var autoPullPanel = document.getElementById('panelAutoPull');
    if (autoPullPanel) {
      autoPullPanel.style.display = hasOllama ? '' : 'none';
    }

    // Virtual Models — Ollama-специфичны (slicer). Скрываем если нет ollama-бэкендов.
    var vmPanel = document.getElementById('panelVirtualModels');
    if (vmPanel) {
      vmPanel.style.display = hasOllama ? '' : 'none';
    }

    // Dispatch Stats — generic, но описание modes (P1-P4) специфично для Ollama.
    // Не скрываем — может быть полезно для llama.cpp тоже.
  }

  function renderAutoPullPanel(cfg, status) {
    var enabledEl = document.getElementById('autoPullEnabled');
    var enabledLabel = document.getElementById('autoPullEnabledLabel');
    var maxConcurrentEl = document.getElementById('autoPullMaxConcurrent');
    var pullTimeoutEl = document.getElementById('autoPullPullTimeout');
    var retryCountEl = document.getElementById('autoPullRetryCount');
    var activeCountEl = document.getElementById('autoPullActiveCount');
    var tableBody = document.querySelector('#autoPullTable tbody');

    if (!enabledEl) return; // Panel not rendered yet

    // If no config data (API not available, demo mode, or error), show "no data" state
    // but DON'T reset the checkbox — preserve user interaction
    if (!cfg) {
      maxConcurrentEl.value = 2;
      pullTimeoutEl.value = '120s';
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
    pullTimeoutEl.value = cfg.pullTimeout || '120s';
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
      return '<tr><td><strong>' + model + '</strong></td><td>' + backend + '</td><td>' + started + '</td><td>' + duration + '</td><td><span class="badge ' + statusClass + '">' + statusText + '</span></td><td style="font-size:11px;color:var(--text-secondary);max-width:200px;overflow:hidden;text-overflow:ellipsis">' + errorText + '</td></tr>';
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
        '<td><strong>' + MA.esc(m.name || '-') + '</strong>' + (m.description ? '<br><span style="font-size:10px;color:var(--text-secondary)">' + MA.esc(m.description) + '</span>' : '') + '</td>' +
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

  function renderDiskNetwork(bk) {
    var body = document.getElementById('diskNetworkBody');
    if (!body) return;
    if (!bk.length) { body.innerHTML = '<div style="color:var(--text-secondary);text-align:center;padding:16px">' + T('monitor.common.noData') + '</div>'; return; }

    // Filter backends that actually have agent system metrics
    var hasSystemMetrics = function(b) {
      var s = b.system || {};
      return (s.diskTotal || s.diskUsed || s.diskFree || s.networkRX || s.networkTX) > 0;
    };
    var validBk = bk.filter(hasSystemMetrics);
    if (!validBk.length) {
      body.innerHTML = '<div style="color:var(--text-secondary);text-align:center;padding:16px">' +
        (T('monitor.diskNetwork.noAgentData') || 'Нет данных агента (disk/network)') +
        '</div>';
      return;
    }

    // Disk and Network cards per backend
    body.innerHTML = '<div style="display:flex;flex-wrap:wrap;gap:10px">' +
      validBk.map(function(b) {
        var s = b.system || {};
        var dTotal = s.diskTotal || 0;
        var dUsed = s.diskUsed || 0;
        var dFree = s.diskFree || 0;
        var nRX = s.networkRX || 0;
        var nTX = s.networkTX || 0;

        // Format bytes to human-readable
        function fmtBytes(bytes) {
          if (!bytes) return '—';
          if (bytes < 1024) return bytes + ' B';
          if (bytes < 1024*1024) return (bytes/1024).toFixed(1) + ' KB';
          if (bytes < 1024*1024*1024) return (bytes/1024/1024).toFixed(1) + ' MB';
          if (bytes < 1024*1024*1024*1024) return (bytes/1024/1024/1024).toFixed(1) + ' GB';
          return (bytes/1024/1024/1024/1024).toFixed(1) + ' TB';
        }
        function fmtNet(bytes) {
          if (!bytes) return '—';
          if (bytes < 1024) return bytes + ' B/s';
          if (bytes < 1024*1024) return (bytes/1024).toFixed(1) + ' KB/s';
          return (bytes/1024/1024).toFixed(1) + ' MB/s';
        }

        // Disk usage percent
        var dPct = dTotal > 0 ? (dUsed / dTotal * 100) : 0;
        var dColor = dPct >= 90 ? 'var(--danger)' : dPct >= 70 ? 'var(--warning)' : 'var(--success)';

        return '<div class="capacity-card" style="min-width:260px;flex:1">' +
          '<h4>' + MA.esc(b.id) + '</h4>' +
          // Disk
          '<div style="margin-bottom:8px">' +
            '<div style="font-size:11px;color:var(--text-secondary);margin-bottom:2px">💾 ' + T('monitor.diskNetwork.disk') + '</div>' +
            '<div class="capacity-bar"><div class="bar-track-wide"><div class="bar-fill-wide" style="width:' + dPct + '%;background:' + dColor + '"></div></div>' +
            '<span class="capacity-val" title="' + T('monitor.diskNetwork.used') + ': ' + fmtBytes(dUsed) + ' | ' + T('monitor.diskNetwork.free') + ': ' + fmtBytes(dFree) + '">' +
            fmtBytes(dUsed) + ' / ' + fmtBytes(dTotal) + '</span></div>' +
          '</div>' +
          // Network
          '<div style="font-size:11px;color:var(--text-secondary);margin-bottom:2px">🌐 ' + T('monitor.diskNetwork.network') + '</div>' +
          '<div style="display:flex;gap:16px;font-size:12px">' +
            '<div><span style="color:var(--accent)">▼ ' + T('monitor.diskNetwork.rx') + ':</span> <strong>' + fmtNet(nRX) + '</strong></div>' +
            '<div><span style="color:var(--warning)">▲ ' + T('monitor.diskNetwork.tx') + ':</span> <strong>' + fmtNet(nTX) + '</strong></div>' +
          '</div>' +
        '</div>';
      }).join('') + '</div>';
  }

  function renderCandidateBackends(candidates) {
    var countEl = document.getElementById('candidatesCount');
    var tb = document.querySelector('#candidatesTable tbody');
    var priorityLabels = { 1: T('monitor.candidates.p1Loaded'), 2: T('monitor.candidates.p2Warming'), 3: T('monitor.candidates.p3Free'), 4: T('monitor.candidates.p4Fallback') };
    var priorityColors = { 1: 'var(--success)', 2: 'var(--warning)', 3: 'var(--accent)', 4: 'var(--text-secondary)' };

    if (!tb) return; // Panel not rendered

    if (!candidates || candidates.length === 0) {
      if (countEl) countEl.textContent = '0';
      if (tb) tb.innerHTML = '<tr><td colspan="6" style="color:var(--text-secondary);text-align:center;padding:16px">' + T('renderers.no_candidate_data') + '</td></tr>';
      return;
    }

    if (countEl) countEl.textContent = candidates.length;

    tb.innerHTML = candidates.map(function(c) {
      var readyBadge = c.has_ready
        ? '<span class="badge badge-green">✅ ' + T('monitor.candidates.ready') + '</span>'
        : '<span class="badge badge-yellow">⏳ ' + T('monitor.candidates.warming') + '</span>';

      // Build groups HTML
      var groupsHtml = (c.groups || []).map(function(g) {
        var label = g.label || priorityLabels[g.priority] || 'P' + g.priority;
        var color = priorityColors[g.priority] || 'var(--text-secondary)';
        var bks = (g.backend_ids || []).map(function(bid) {
          return '<span class="badge badge-purple">' + MA.esc(bid) + '</span>';
        }).join(' ');
        return '<div style="display:flex;align-items:center;gap:6px;margin:2px 0">' +
          '<span class="badge" style="background:' + color + '20;color:' + color + ';border:1px solid ' + color + '40;font-size:10px;font-weight:700;min-width:60px;text-align:center">P' + g.priority + ' ' + MA.esc(label) + '</span>' +
          '<span style="font-size:11px">' + (bks || '<span style="color:var(--text-secondary)">—</span>') + '</span>' +
          '</div>';
      }).join('');

      return '<tr>' +
        '<td><strong>' + MA.esc(c.model) + '</strong></td>' +
        '<td>' + readyBadge + '</td>' +
        '<td class="col-right">' + c.total + '</td>' +
        '<td style="font-size:11px">' + groupsHtml + '</td>' +
        '</tr>';
    }).join('');
  }

  function renderDispatchStats(qs) {
    var da = qs.dispatch_by_affinity || 0;
    var dl = qs.dispatch_by_load || 0;
    var dc = qs.dispatch_by_config || 0;
    var pt = qs.processed_total || (da + dl + dc);

    document.getElementById('dispatchAffinity').textContent = da.toLocaleString();
    document.getElementById('dispatchLoad').textContent = dl.toLocaleString();
    document.getElementById('dispatchConfig').textContent = dc.toLocaleString();
    document.getElementById('dispatchTotal').textContent = pt.toLocaleString();

    var total = da + dl + dc;
    if (total > 0) {
      document.getElementById('dispatchAffinityPct').textContent = (da / total * 100).toFixed(1) + '%';
      document.getElementById('dispatchLoadPct').textContent = (dl / total * 100).toFixed(1) + '%';
      document.getElementById('dispatchConfigPct').textContent = (dc / total * 100).toFixed(1) + '%';
    } else {
      document.getElementById('dispatchAffinityPct').textContent = '—';
      document.getElementById('dispatchLoadPct').textContent = '—';
      document.getElementById('dispatchConfigPct').textContent = '—';
    }
  }

  function renderModelOps(modelOps) {
    var tb = document.querySelector('#modelOpsTable tbody');
    var countEl = document.getElementById('modelOpsCount');
    if (!tb && !countEl) return; // Panel not rendered

    var ops = (modelOps && modelOps.operations) || [];
    if (!Array.isArray(ops)) ops = [];

    if (countEl) countEl.textContent = ops.length;

    if (!tb) return;
    if (ops.length === 0) {
      tb.innerHTML = '<tr><td colspan="6" style="color:var(--text-secondary);text-align:center;padding:16px">' + T('monitor.common.noData') + '</td></tr>';
      return;
    }

    tb.innerHTML = ops.map(function(op) {
      var opType = op.operation || '-';
      var modelName = op.modelName || '-';
      var backendId = op.backendId || '-';
      var startedAt = op.startedAt ? new Date(op.startedAt).toLocaleString() : '-';
      var duration = op.duration || (op.startedAt ? MA.fmtDur(Date.now() - new Date(op.startedAt).getTime()) : '-');
      var status = op.status || 'running';

      var badgeClass = 'badge-info';
      if (status === 'running') badgeClass = 'badge-yellow';
      else if (status === 'complete' || status === 'completed') badgeClass = 'badge-green';
      else if (status === 'error' || status === 'failed') badgeClass = 'badge-red';

      return '<tr>' +
        '<td><span class="badge badge-purple">' + MA.esc(opType) + '</span></td>' +
        '<td><strong>' + MA.esc(modelName) + '</strong></td>' +
        '<td>' + MA.esc(backendId) + '</td>' +
        '<td class="col-right" style="font-size:11px">' + startedAt + '</td>' +
        '<td class="col-right" style="font-size:11px">' + duration + '</td>' +
        '<td><span class="badge ' + badgeClass + '">' + MA.esc(status) + '</span></td>' +
        '</tr>';
    }).join('');
  }

  window.updateUI = updateUI;
  window.switchBackendType = switchBackendType;
  window.renderBackendTypeSwitcher = renderBackendTypeSwitcher;
})();