// demo-mode.js — R232 (2026-10-24): ДЕМО-РЕЖИМ WebUI.
//
// ЗАЧЕМ. Оператору нужно показать панель «как она работает» человеку без доступа
// к стенду: подключение клиентов, распределение запросов балансером, очередь,
// назначение донора (RPC-раскладка) и работу AutoTune. Показывать реальные
// данные нельзя (их нет / они приватные), а статичный скриншот не объясняет
// процессы.
//
// КАК ВКЛЮЧАЕТСЯ. ТОЛЬКО через env: `WEBUI_DEMO_MODE=1` у контейнера webui
// (entrypoint.sh пишет `DEMO_MODE: true` в config.js). В самом интерфейсе
// переключателя нет: это режим развёртывания, а не пользовательская настройка.
//
// ЧТО ДЕЛАЕТ. При включённом режиме:
//   1. перехватывает ВСЕ same-origin `/api/`-запросы и отвечает синтетикой
//      (реальный балансер не опрашивается, мутации запрещены — 403 read-only);
//   2. подставляет гостевую сессию (viewer): вход не запрашивается, админские
//      действия скрыты штатной ролевой логикой auth.js;
//   3. гоняет детерминированный сценарий (цикл 150 с): старт кластера и загрузка
//      qwen3.8 → клиенты Cline/Open WebUI/curl → очередь → назначение донора по
//      RPC → AutoTune (рекомендация, reload, проверка) → стабильная работа;
//   4. показывает баннер «ДЕМО» и панель «Демо-сценарий» с текущей фазой,
//      счётчиками и лентой событий;
//   5. шлёт синтетические `ws-message` (autotune_reload_triggered/succeeded),
//      чтобы тосты AutoTune тоже были видны.
//
// ВАЖНО: числа сценария согласованы с живым замером R231 (qwen3.8 27B Q4,
// ctx 16384, мастер 8 ГБ + донор 8 ГБ): 20/66 слоёв на GPU до донора, 48/66
// после (26 CUDA0 + 22 RPC0), скорость 0.71 → 2.08 tok/s. Время в демо
// УСКОРЕНО (иначе один ответ шёл бы 2 минуты) — это честно подписано в баннере.
(function () {
  'use strict';

  function flag(v) {
    if (v === true || v === 1) { return true; }
    return /^(1|true|yes|on)$/i.test(String(v === undefined || v === null ? '' : v));
  }

  var CFG = (typeof window !== 'undefined' && window.WEBUI_CONFIG) || {};
  var ENABLED = flag(CFG.DEMO_MODE);
  var CYCLE_SEC = 150;
  var MODEL = 'Qwen3.8-27B-UD-Q4_K_M';
  var GEMMA = 'gemma-4-E4B-it-Q4_K_M';
  var DONOR_NODE = 'donor.local:50052';
  var startedAt = Date.now();

  // --- сценарий -----------------------------------------------------------

  var CLIENTS = [
    { key: 'cline-1', name: 'Cline', ip: '192.0.2.55', model: MODEL, join: 3, leave: 138 },
    { key: 'owui-1', name: 'Open WebUI', ip: '192.0.2.100', model: GEMMA, join: 9, leave: 143 },
    { key: 'cline-2', name: 'Cline', ip: '192.0.2.100', model: MODEL, join: 16, leave: 146 },
    { key: 'curl-1', name: 'curl/8.5', ip: '192.0.2.42', model: MODEL, join: 24, leave: 120 }
  ];

  var DONOR_AT = 45;       // донор входит в раскладку
  var TUNE_REC_AT = 80;    // AutoTune увидел sub-optimal
  var TUNE_RELOAD_AT = 86; // reload (unload+load)
  var TUNE_DONE_AT = 94;   // конфигурация применена и подтверждена
  var TUNE_OK_AT = 96;     // анализ: optimal

  function simNow() {
    var ms = (Date.now() - startedAt) % (CYCLE_SEC * 1000);
    if (ms < 0) { ms = 0; }
    return { t: ms / 1000, cycle: Math.floor((Date.now() - startedAt) / (CYCLE_SEC * 1000)) };
  }

  function iso(t, base) {
    return new Date((base || startedAt) + t * 1000).toISOString();
  }

  function round(n) { return Math.round(n); }

  function phaseAt(t) {
    if (t < 8) {
      return { id: 'boot', title: 'Загрузка модели на мастер', note: 'qwen3.8 · 15.3 GB · ctx 16384 · KV q8_0' };
    }
    if (t < 14) {
      return { id: 'clients', title: 'Клиенты подключаются', note: 'один адрес балансера — клиенты не знают, где модель' };
    }
    if (t < DONOR_AT) {
      return { id: 'queue', title: 'Очередь и распределение', note: 'слот один: запросы ждут, балансер ведёт очередь' };
    }
    if (t < TUNE_REC_AT) {
      return { id: 'donor', title: 'Назначение донора (RPC)', note: '22 слоя уходят на донора; CPU-слоёв 46 → 18' };
    }
    if (t < TUNE_DONE_AT) {
      return { id: 'autotune', title: 'AutoTune: анализ и применение', note: 'KV q4_0 → f16; проверка бюджета 8.0 GB' };
    }
    if (t < 128) {
      return { id: 'steady', title: 'Стабильная работа', note: 'AutoTune: optimal · очередь пуста · донор в раскладке' };
    }
    return { id: 'cooldown', title: 'Завершение демо-цикла', note: 'через несколько секунд сценарий повторится' };
  }

  // arrivalTimes — детерминированные моменты прихода запросов (ускоренно).
  function arrivalTimes(tEnd) {
    var list = [];
    var t = 14;
    while (t <= 132 && list.length < 200) {
      list.push(t);
      var interval = t < 44 ? 2.5 : (t < TUNE_DONE_AT ? 4 : 6);
      t += interval;
    }
    return list.filter(function (a) { return a <= tEnd + 0.0001; });
  }

  function activeClients(t) {
    return CLIENTS.filter(function (c) { return t >= c.join && t < c.leave; });
  }

  // computeState — вся картина «на момент t секунд сценария».
  function computeState(tSec) {
    var t = Math.max(0, Math.min(CYCLE_SEC, Number(tSec) || 0));
    var donorOn = t >= DONOR_AT;
    var tuned = t >= TUNE_DONE_AT;

    var slots = donorOn ? [{ id: 'master', name: 'cppworker-gpu', free: 0 }, { id: 'donor', name: 'rpc-donor', free: 0 }]
                        : [{ id: 'master', name: 'cppworker-gpu', free: 0 }];
    var duration = tuned ? 5 : (donorOn ? 6 : 9);
    var arrivals = arrivalTimes(t);
    var requests = [];
    var cycleClients = activeClients(t);
    for (var i = 0; i < arrivals.length; i++) {
      var arrival = arrivals[i];
      var slot = slots[0];
      for (var s = 1; s < slots.length; s++) {
        if (slots[s].free < slot.free) { slot = slots[s]; }
      }
      var start = Math.max(arrival, slot.free);
      var end = start + duration;
      slot.free = end;
      var client = CLIENTS[i % CLIENTS.length];
      requests.push({
        id: 'req-' + String(i + 1).padStart(3, '0'),
        arrival: arrival,
        start: start,
        end: end,
        backend: slot.id,
        backendName: slot.name,
        model: client.model,
        client: client,
        sessionId: 'sess-' + client.key,
        waitMs: round((start - arrival) * 1000)
      });
    }

    var pending = requests.filter(function (r) { return r.start > t + 0.0001; });
    var processing = requests.filter(function (r) { return r.start <= t + 0.0001 && r.end > t; });
    var done = requests.filter(function (r) { return r.end <= t + 0.0001; });

    var loaded = t >= 8 && !(t >= TUNE_RELOAD_AT && t < TUNE_DONE_AT);
    var loading = (t >= 0 && t < 8) || (t >= TUNE_RELOAD_AT && t < TUNE_DONE_AT);
    var loadProgress = t < 8 ? round((t / 8) * 100) : (t >= TUNE_RELOAD_AT && t < TUNE_DONE_AT ? round(((t - TUNE_RELOAD_AT) / (TUNE_DONE_AT - TUNE_RELOAD_AT)) * 100) : 100);

    var layers = donorOn
      ? [{ device: 'CPU', from: 0, to: 17, count: 18 },
         { device: 'CUDA0', from: 18, to: 43, count: 26 },
         { device: 'RPC0', from: 44, to: 65, count: 22, node: DONOR_NODE }]
      : [{ device: 'CPU', from: 0, to: 45, count: 46 },
         { device: 'CUDA0', from: 46, to: 65, count: 20 }];
    var gpuLayers = donorOn ? 48 : 20;
    var rpcLayers = donorOn ? 22 : 0;

    var modelInfo = {
      name: MODEL,
      path: '/demo/models/' + MODEL + '.gguf',
      size: 16464440224,
      vramUsage: donorOn ? 5571 : 5172,
      ramUsage: donorOn ? 8750 : 10184,
      contextLength: 16384,
      batchSize: 512,
      numGpuLayers: gpuLayers,
      quantization: 'Q4_K_M',
      state: loaded ? 'loaded' : (loading ? 'loading' : 'loaded'),
      architecture: 'qwen35moe',
      nLayers: 66,
      nEmbd: 5120,
      nKvHeads: 8,
      headDimK: 128,
      maxContext: 131072,
      kvCacheType: tuned ? 'f16' : 'q4_0',
      feasibleMaxContext: 131072,
      loadedAt: iso(loaded ? 8 : 0),
      deviceLayersExact: layers,
      deviceLayers: layers
    };

    var activeMaster = processing.filter(function (r) { return r.backend === 'master'; }).length;
    var activeDonor = processing.filter(function (r) { return r.backend === 'donor'; }).length;
    var gpuUsageMaster = round(Math.min(99, 34 + activeMaster * 42 + (pending.length > 0 ? 8 : 0)));
    var gpuUsageDonor = donorOn ? round(Math.min(99, 22 + activeDonor * 46)) : 0;
    var vramUsedMaster = donorOn ? 7420 : 7560;
    var vramFreeMaster = 8191 - vramUsedMaster;
    var tokS = donorOn ? (tuned ? 2.15 : 2.08) : 0.71;
    var rps = (processing.length + pending.length * 0.25 + 0.4).toFixed(2);

    var modelOps = [];
    if (t < 8) {
      modelOps.push({ operation: 'load', modelName: MODEL, backendId: 'cppworker-gpu-bundled-agent', startedAt: iso(0), duration: round(t) + 's', status: 'running', progress: loadProgress });
    }
    if (t >= TUNE_RELOAD_AT && t < TUNE_DONE_AT) {
      modelOps.push({ operation: 'reload', modelName: MODEL, backendId: 'cppworker-gpu-bundled-agent', startedAt: iso(TUNE_RELOAD_AT), duration: round(t - TUNE_RELOAD_AT) + 's', status: 'running', progress: loadProgress });
    }

    function backendBase(id, name, host, type, status, role) {
      return {
        id: id, name: name, host: host, type: type, backendType: type, status: status,
        role: role, weight: 1, labels: role === 'donor' ? ['rpc:donor', 'gpu:sm_120'] : ['gpu:sm_86'],
        maxConcurrentReqs: 1, maxModels: 1, apiStyle: 'openai', gpuMode: 'auto',
        score: role === 'donor' ? 0.78 : 0.91,
        ollama: { requestsPerSecond: Number(rps), backendCapacity: { freeVram: 1024, loadableModelCount: 1, guaranteedVram: 512, mode: 'gpu', availableModels: [] } }
      };
    }

    var master = backendBase('cppworker-gpu-bundled-agent', 'cppworker GPU (мастер)', '127.0.0.1', 'llama_cpp', 'healthy', 'main');
    master.gpu = { usagePercent: gpuUsageMaster, memoryTotal: 8191, memoryUsed: vramUsedMaster, memoryFree: vramFreeMaster, temperature: 64, powerUsage: 180, powerLimit: 350, gpuClock: 1800, memClock: 7000 };
    master.vram = { usagePercent: round((vramUsedMaster / 8191) * 100), totalGB: 8, usedGB: Number((vramUsedMaster / 1024).toFixed(2)) };
    master.system = { cpuUsagePercent: donorOn ? 34 : 62, memoryUsagePercent: donorOn ? 52 : 74, diskTotal: 500472979456, diskUsed: 209089773568, diskFree: 291383205888, networkRX: 1842176 + processing.length * 1048576, networkTX: 1024512 + processing.length * 524288 };
    master.models = loaded ? [MODEL] : [];
    master.loadedModels = (loaded || loading) ? [Object.assign({}, modelInfo, { state: loaded ? 'loaded' : 'loading' })] : [];
    master.rpcEnabled = donorOn;
    master.rpcServersActive = donorOn ? [DONOR_NODE] : [];
    master.rpcServers = donorOn ? [DONOR_NODE] : [];
    master.rpcLayout = { enabled: donorOn, mode: 'auto', servers: donorOn ? [DONOR_NODE] : [], degraded: false, reason: '' };
    master.rpcLink = { mbps: 8947, node: 'cppworker-gpu:18092', measuredAt: iso(2), source: 'lb-rpc-link-probe' };
    master.rpcNode = { enabled: false, running: false, listening: false };
    master.loadingModels = loading ? [Object.assign({}, modelInfo, { state: 'loading', progress: loadProgress })] : [];
    master.loadingModelCount = loading ? 1 : 0;
    master.loadedModelCount = loaded ? 1 : 0;
    master.autoTune = autoTuneAt(t);

    var donor = backendBase('rpc-donor', 'donor (узел RPC)', 'donor.local', 'llama_cpp', donorOn ? 'healthy' : 'offline', 'donor');
    donor.gpu = { usagePercent: gpuUsageDonor, memoryTotal: 8123, memoryUsed: donorOn ? 6200 : 1090, memoryFree: donorOn ? 1923 : 7033, temperature: 58, powerUsage: donorOn ? 95 : 18, powerLimit: 140, gpuClock: 2100, memClock: 7000 };
    donor.vram = { usagePercent: donorOn ? 76 : 13, totalGB: 8, usedGB: donorOn ? 6.05 : 1.06 };
    donor.system = { cpuUsagePercent: donorOn ? 22 : 6, memoryUsagePercent: donorOn ? 38 : 12, diskTotal: 1000965890048, diskUsed: 322122547200, diskFree: 678843342848, networkRX: donorOn ? 8947 * 1048576 / 8 : 0, networkTX: donorOn ? 8947 * 1048576 / 8 : 0 };
    donor.models = [];
    donor.loadedModels = [];
    donor.rpcEnabled = false;
    donor.rpcServersActive = [];
    donor.autoTune = { autoTuneEnabled: true, overallSeverity: 'ok', overallSummary: 'no models loaded', models: [] };

    var image = backendBase('imageworker', 'imageworker', '127.0.0.1', 'image_cpp', 'healthy', 'standalone');
    image.gpu = { usagePercent: 0, memoryTotal: 8191, memoryUsed: 1200, memoryFree: 6991 };
    image.vram = { usagePercent: 15, totalGB: 8, usedGB: 1.17 };
    image.system = { cpuUsagePercent: 4, memoryUsagePercent: 18, diskTotal: 500472979456, diskUsed: 409089773568, diskFree: 91383205888, networkRX: 0, networkTX: 0 };
    image.models = [];
    image.loadedModels = [];

    var backends = [master, donor, image];

    var sessions = CLIENTS.filter(function (c) { return t >= c.join; }).map(function (c) {
      var count = requests.filter(function (r) { return r.client.key === c.key; }).length;
      var last = requests.filter(function (r) { return r.client.key === c.key; }).slice(-1)[0];
      return {
        id: c.ip + '::' + c.name + '::' + c.model,
        backendId: last ? (last.backend === 'donor' ? 'rpc-donor' : 'cppworker-gpu-bundled-agent') : '',
        model: c.model,
        clientIP: c.ip,
        clientName: c.name,
        requestCount: count,
        lastRequestAt: iso(last ? last.arrival : c.join),
        protocol: c.name === 'Open WebUI' ? 'ollama' : 'openai',
        active: t < c.leave
      };
    });

    var queueAll = pending.map(function (r, idx) {
      return {
        model: r.model, target: null, status: 'pending',
        enqueued: iso(r.arrival), sessionId: r.sessionId, requestId: r.id, position: idx + 1,
        waitMs: round((t - r.arrival) * 1000)
      };
    }).concat(processing.map(function (r) {
      return {
        model: r.model, target: r.backendName, status: 'processing',
        enqueued: iso(r.arrival), sessionId: r.sessionId, requestId: r.id, waitMs: r.waitMs
      };
    }));

    var history = done.slice(-10).reverse().map(function (r) {
      return {
        model: r.model, backend: r.backendName, status: 'done',
        waitMs: r.waitMs, processedAt: iso(r.end), requestId: r.id, sessionId: r.sessionId,
        durationMs: round((r.end - r.start) * 1000)
      };
    });

    var avgWait = pending.length
      ? round(pending.reduce(function (sum, r) { return sum + (t - r.arrival) * 1000; }, 0) / pending.length)
      : 0;

    var queueStats = {
      current_size: pending.length,
      pending_count: pending.length,
      processing_count: processing.length,
      processed_total: 1240 + simNow().cycle * 40 + done.length,
      max_size: 100,
      avg_wait_time_ms: avgWait,
      workers: 4,
      timeout_sec: 30,
      dispatch_by_affinity: 8750 + done.length,
      dispatch_by_load: 4520 + (donorOn ? 12 : 0),
      dispatch_by_config: 2150
    };

    var events = buildEvents(t, requests, pending, processing, done);

    return {
      t: t, cycle: simNow().cycle, phase: phaseAt(t),
      donorOn: donorOn, tuned: tuned, loaded: loaded, loading: loading, loadProgress: loadProgress,
      model: modelInfo, layers: layers, gpuLayers: gpuLayers, rpcLayers: rpcLayers,
      tokS: tokS, rps: rps, requests: requests, pending: pending, processing: processing, done: done,
      backends: backends, sessions: sessions, queueAll: queueAll, history: history,
      queueStats: queueStats, avgWait: avgWait,
      autoTune: autoTuneAt(t), events: events, modelOps: modelOps,
      clients: activeClients(t), clientCount: activeClients(t).length
    };
  }

  function buildEvents(t, requests, pending, processing, done) {
    var list = [];
    if (t >= CLIENTS[0].join) {
      // «Сводка по клиентам» держит категорию client в ленте даже тогда, когда
      // подключения остались далеко в прошлом (иначе панель в steady-фазе
      // показывает только последние queue/assign-события).
      list.push({ at: t, icon: '👥', kind: 'client', text: 'клиентов в работе: ' + activeClients(t).length });
    }
    CLIENTS.forEach(function (c) {
      if (t >= c.join) { list.push({ at: c.join, icon: '👤', kind: 'client', text: 'клиент ' + c.name + ' подключился (' + c.ip + ')' }); }
      if (t >= c.leave) { list.push({ at: c.leave, icon: '👋', kind: 'client', text: 'клиент ' + c.name + ' отключился' }); }
    });
    requests.forEach(function (r) {
      if (r.arrival > t) { return; }
      if (r.start > r.arrival + 0.01) {
        list.push({ at: r.arrival, icon: '⏳', kind: 'queue', text: 'запрос ' + r.id + ' встал в очередь (' + r.model + ')' });
      }
      if (r.start <= t) {
        list.push({
          at: r.start, icon: r.backend === 'donor' ? '🔗' : '⚖️', kind: 'assign',
          text: 'балансер → ' + r.backendName + ' · ' + r.model + ' · ' + (r.backend === 'donor' ? '22 слоя на доноре' : (t >= DONOR_AT ? '26 слоёв локально' : '20 слоёв локально'))
        });
      }
    });
    if (t >= DONOR_AT) { list.push({ at: DONOR_AT, icon: '🔗', kind: 'donor', text: 'донор подключён: RPC0 · 22 слоя (донабор VRAM ~5.6 GB)' }); }
    if (t >= TUNE_REC_AT) {
      list.push({ at: TUNE_REC_AT, icon: '🛠', kind: 'autotune', text: 'AutoTune: рекомендация — KV q4_0 → f16 + все слои (бюджет 8.0 GB)' });
    }
    if (t >= TUNE_RELOAD_AT) {
      list.push({ at: TUNE_RELOAD_AT, icon: '🔄', kind: 'autotune', text: 'AutoTune: reload модели (' + MODEL + ')' });
    }
    if (t >= TUNE_DONE_AT) {
      list.push({ at: TUNE_DONE_AT, icon: '✅', kind: 'autotune', text: 'AutoTune: конфигурация применена и подтверждена (f16, 48/66 слоёв)' });
      list.push({ at: TUNE_OK_AT, icon: '🟢', kind: 'autotune', text: 'AutoTune: анализ — optimal, рекомендаций нет' });
    }
    list.sort(function (a, b) { return b.at - a.at; });
    return list.slice(0, 12);
  }

  function autoTuneAt(t) {
    if (t >= TUNE_OK_AT) {
      return {
        autoTuneEnabled: true, overallSeverity: 'ok', overallSummary: '1 models, all optimal',
        models: [{ modelName: MODEL, isSubOptimal: false, autoTuneEnabled: true, recommendations: [] }]
      };
    }
    if (t >= TUNE_REC_AT) {
      return {
        autoTuneEnabled: true, overallSeverity: 'warning', overallSummary: '1 models, 1 sub-optimal',
        models: [{
          modelName: MODEL, isSubOptimal: true, autoTuneEnabled: true,
          analysisReasoning: 'Найдено 2 проблемы для ' + MODEL,
          recommendations: [
            {
              severity: 'warning', category: 'kv_cache',
              message: "KV cache 'q4_0' — sub-optimal, рекомендуется 'f16'",
              suggestion: 'Перезагрузить с kvCacheType=f16: f16 KV (1.2GB) для n_ctx=16384 влезает в бюджет 8.0GB (free+usage модели, model=3.9GB, резерв=0.4GB) → f16 (лучшее качество)',
              currentKvCache: 'q4_0', recommendedKvCache: 'f16'
            },
            {
              severity: 'info', category: 'layers',
              message: 'num_gpu_layers=48 — sub-optimal, рекомендуется -1',
              suggestion: 'model=3.9GB + KV_f16(1.2GB) + резерв(0.4GB) влезают в бюджет 8.0GB — рекомендуем -1 (all layers)',
              currentNumGpuLayers: 48, recommendedNumGpuLayers: -1
            }
          ],
          optimalKvCacheType: 'f16', optimalNumGpuLayers: -1
        }]
      };
    }
    return {
      autoTuneEnabled: true, overallSeverity: 'ok', overallSummary: '1 models, all optimal',
      models: [{ modelName: MODEL, isSubOptimal: false, autoTuneEnabled: true, recommendations: [] }]
    };
  }

  // --- API-роутер ---------------------------------------------------------

  // ORIGIN — свой origin страницы. Нужен, чтобы отличать СВОЙ абсолютный URL
  // (monitor/api.js склеивает `location.origin + '/api/...'`) от чужого: чужие
  // запросы демо не трогает. Пустая строка в Node-тестах/без window.
  function pageOrigin() {
    try {
      if (typeof window !== 'undefined' && window.location && window.location.origin) { return String(window.location.origin); }
    } catch (e) { /* ignore */ }
    return '';
  }
  var ORIGIN = pageOrigin();

  function stripBase(url) {
    var u = String(url || '');
    if (u.indexOf('http') === 0) {
      try {
        var parsed = new URL(u);
        if (ORIGIN && parsed.origin !== ORIGIN) { return null; }
        u = parsed.pathname + parsed.search;
      } catch (e) { return null; }
    }
    var base = String(CFG.API_BASE || '');
    if (base && u.indexOf(base) === 0) { u = u.slice(base.length); }
    u = u.split('#')[0];
    var q = u.indexOf('?');
    if (q >= 0) { u = u.slice(0, q); }
    return u;
  }

  function responseBody(path, method, st) {
    if (method !== 'GET' && method !== 'HEAD') {
      return { status: 403, body: { error: 'demo mode: read-only (изменения запрещены)', demo: true } };
    }
    var p = path;
    if (p === '/api/v1/auth/me') {
      return { status: 200, body: { authenticated: true, usersConfigured: true, tokenAuth: false, csrf: 'demo-csrf', user: { name: 'demo-guest', role: 'viewer' }, demo: true } };
    }
    if (p === '/api/v1/auth/logout') { return { status: 200, body: { success: true, demo: true } }; }
    if (p === '/api/v1/cluster') {
      return { status: 200, body: { backends: st.backends, rps: Number(st.rps), backendEngine: 'llama_cpp', effectiveBackendType: 'llama_cpp', queue: { max_size: 100 }, timestamp: iso(st.t), demo: true } };
    }
    if (p === '/api/v1/backends') {
      return { status: 200, body: { backends: st.backends, totalBackends: st.backends.length, healthyBackends: st.backends.filter(function (b) { return b.status === 'healthy'; }).length, timestamp: iso(st.t), demo: true } };
    }
    if (p === '/api/v1/backends/roles' || p === '/api/v1/backends/metrics') {
      return { status: 200, body: { backends: st.backends, demo: true } };
    }
    if (p === '/api/v1/queue/stats') { return { status: 200, body: Object.assign({}, st.queueStats, { demo: true }) }; }
    if (p === '/api/v1/queue/details') {
      return { status: 200, body: { pending_count: st.pending.length, processing_count: st.processing.length, current_size: st.pending.length, processed_total: st.queueStats.processed_total, all: st.queueAll, demo: true } };
    }
    if (p === '/api/v1/queue/history') { return { status: 200, body: { history: st.history, demo: true } }; }
    if (p === '/api/v1/sessions') { return { status: 200, body: { sessions: st.sessions, demo: true } }; }
    if (p === '/api/v1/placement') {
      return { status: 200, body: { enabled: true, models: [{ model: MODEL, strategy: st.donorOn ? 'sharded' : 'single', reason: st.donorOn ? '48/66 слоёв на GPU (26 локально + 22 на доноре)' : '20/66 слоёв на GPU, остальные в RAM', degraded: false }], demo: true } };
    }
    if (p === '/api/v1/candidates') {
      return { status: 200, body: [{
        model: MODEL, groups: [{ priority: 1, label: 'LOADED', backend_ids: ['cppworker-gpu-bundled-agent'] }],
        total: 1, has_ready: st.loaded
      }, {
        model: GEMMA, groups: [{ priority: 1, label: 'LOADED', backend_ids: ['cppworker-gpu-bundled-agent'] }],
        total: 1, has_ready: true
      }] };
    }
    if (p === '/api/v1/virtualmodels') { return { status: 200, body: { enabled: false, count: 0, models: [], demo: true } }; }
    if (p === '/api/v1/models/operations') { return { status: 200, body: { operations: st.modelOps, demo: true } }; }
    if (p === '/api/v1/admin/autotune') { return { status: 200, body: { backends: { 'cppworker-gpu-bundled-agent': st.autoTune }, demo: true } }; }
    if (p === '/api/v1/admin/autotune/history') {
      var hist = [];
      if (st.t >= TUNE_REC_AT) { hist.push({ timestamp: iso(TUNE_REC_AT), type: 'recommendation', backendId: 'cppworker-gpu-bundled-agent', model: MODEL, message: 'KV q4_0 → f16 + все слои (бюджет 8.0 GB)' }); }
      if (st.t >= TUNE_RELOAD_AT) { hist.push({ timestamp: iso(TUNE_RELOAD_AT), type: 'reload_triggered', backendId: 'cppworker-gpu-bundled-agent', model: MODEL, message: 'AutoTune reload' }); }
      if (st.t >= TUNE_DONE_AT) { hist.push({ timestamp: iso(TUNE_DONE_AT), type: 'reload_succeeded', backendId: 'cppworker-gpu-bundled-agent', model: MODEL, message: 'конфигурация применена и подтверждена' }); }
      return { status: 200, body: { events: hist, demo: true } };
    }
    if (p === '/api/v1/autopull') { return { status: 200, body: { enabled: false, demo: true } }; }
    if (p === '/api/v1/autopull/status') { return { status: 200, body: { running: false, demo: true } }; }
    if (p === '/api/v1/agents/stats') { return { status: 200, body: { agents: [], total: 0, demo: true } }; }
    if (p === '/api/v1/config' || p === '/api/v1/cluster/config') {
      // initialized: true — иначе в демо вылезает «Мастер настройки» первого
      // запуска и закрывает собой сценарий (app.js спрашивает
      // SetupWizard.isInitialized → Api.config → GET /api/v1/cluster/config).
      return { status: 200, body: {
        initialized: true, backendType: 'llama_cpp',
        algorithm: 'resource-aware', modelAffinity: true, sessionStickiness: true,
        useEnhancedScoring: true, predictionFiltering: true,
        gpuMaxUsage: 90, vramMaxUsage: 85, cpuMaxUsage: 80, ramMaxUsage: 85, minFreeDisk: 10,
        backendEngine: 'llama_cpp', demo: true
      } };
    }
    if (p === '/api/v1/cluster/models/loaded') {
      var loaded = st.loaded ? [{
        name: MODEL, model: MODEL, backendId: 'cppworker-gpu-bundled-agent', backend_id: 'cppworker-gpu-bundled-agent',
        size: 16450000000, sizeVram: 9200000000, contextLength: 16384, kvCacheType: st.model.kvCacheType
      }] : [];
      return { status: 200, body: { count: loaded.length, models: loaded, modelsPerBackend: { 'cppworker-gpu-bundled-agent': loaded.length }, demo: true } };
    }
    if (p === '/api/v1/cluster/models/loading') {
      return { status: 200, body: { count: st.loading ? 1 : 0, models: st.loading ? [{ name: MODEL, progress: st.loadProgress }] : [], demo: true } };
    }
    if (p === '/api/v1/proxy/logs' || p === '/api/v1/logs') {
      var logs = st.events.slice(0, 10).map(function (e) {
        return { timestamp: iso(e.at), level: e.kind === 'autotune' ? 'warn' : 'info', message: e.text, demo: true };
      });
      return { status: 200, body: { logs: logs, demo: true } };
    }
    if (p === '/api/v1/metrics' || p === '/api/v1/cluster/state') {
      return { status: 200, body: { backends: st.backends, demo: true } };
    }
    return { status: 200, body: { demo: true } };
  }

  function route(method, url, stOpt) {
    var path = stripBase(url);
    if (!path || path.indexOf('/api/') !== 0) { return null; }
    var st = stOpt || computeState(simNow().t);
    return responseBody(path, String(method || 'GET').toUpperCase(), st);
  }

  // --- перехват fetch (только same-origin /api/) --------------------------

  function installFetch() {
    if (typeof window === 'undefined' || typeof window.fetch !== 'function') { return; }
    if (window.__demoFetchInstalled) { return; }
    window.__demoFetchInstalled = true;
    var orig = window.fetch.bind(window);
    window.fetch = function (input, init) {
      var url = typeof input === 'string' ? input : ((input && input.url) || '');
      var method = String((init && init.method) || (input && input.method) || 'GET').toUpperCase();
      var path = stripBase(url);
      if (path === null || path.indexOf('/api/') !== 0) { return orig(input, init); }
      var res = responseBody(path, method, computeState(simNow().t));
      var body = JSON.stringify(res.body);
      var ResponseCtor = window.Response || (typeof Response !== 'undefined' ? Response : null);
      if (ResponseCtor) {
        return Promise.resolve(new ResponseCtor(body, {
          status: res.status,
          headers: { 'Content-Type': 'application/json', 'X-OllamaLegion-Demo': '1' }
        }));
      }
      return Promise.resolve({
        ok: res.status >= 200 && res.status < 300,
        status: res.status,
        headers: { get: function () { return 'application/json'; } },
        json: function () { return Promise.resolve(res.body); },
        text: function () { return Promise.resolve(body); }
      });
    };
  }

  // --- WebSocket-заглушка (никаких реальных соединений) -------------------

  function installWebSocket() {
    if (typeof window === 'undefined' || typeof window.WebSocket !== 'function') { return; }
    if (window.__demoWsInstalled) { return; }
    window.__demoWsInstalled = true;

    function DemoWebSocket(url) {
      var self = this;
      this.url = url;
      this.readyState = 0;
      this.onopen = null; this.onmessage = null; this.onclose = null; this.onerror = null;
      this._listeners = {};
      this._closed = false;
      setTimeout(function () {
        if (self._closed) { return; }
        self.readyState = 1;
        self._fire('open', {});
      }, 120);
    }
    DemoWebSocket.prototype._fire = function (type, ev) {
      var handler = this['on' + type];
      var listeners = this._listeners[type] || [];
      try { if (typeof handler === 'function') { handler(ev); } } catch (e) { /* ignore */ }
      for (var i = 0; i < listeners.length; i++) {
        try { listeners[i].call(this, ev); } catch (e2) { /* ignore */ }
      }
    };
    DemoWebSocket.prototype.addEventListener = function (type, fn) {
      (this._listeners[type] = this._listeners[type] || []).push(fn);
    };
    DemoWebSocket.prototype.removeEventListener = function (type, fn) {
      var arr = this._listeners[type] || [];
      var i = arr.indexOf(fn);
      if (i >= 0) { arr.splice(i, 1); }
    };
    DemoWebSocket.prototype.send = function () { /* read-only demo */ };
    DemoWebSocket.prototype.close = function () {
      if (this._closed) { return; }
      this._closed = true;
      this.readyState = 3;
      this._fire('close', { code: 1000, reason: 'demo' });
    };
    DemoWebSocket.CONNECTING = 0; DemoWebSocket.OPEN = 1; DemoWebSocket.CLOSING = 2; DemoWebSocket.CLOSED = 3;
    DemoWebSocket.prototype.CONNECTING = 0; DemoWebSocket.prototype.OPEN = 1; DemoWebSocket.prototype.CLOSING = 2; DemoWebSocket.prototype.CLOSED = 3;
    window.WebSocket = DemoWebSocket;
  }

  function announceWs() {
    if (typeof window === 'undefined' || typeof window.dispatchEvent !== 'function') { return; }
    try {
      window.dispatchEvent(new CustomEvent('ws-status', { detail: { connected: true } }));
    } catch (e) { /* CustomEvent недоступен (тесты) */ }
  }

  // Синтетические тосты AutoTune в момент фаз перезагрузки.
  var wsPhase = { reloadSent: false, doneSent: false, lastCycle: -1 };
  function pumpWsEvents(st) {
    if (typeof window === 'undefined' || typeof window.dispatchEvent !== 'function') { return; }
    if (wsPhase.lastCycle !== st.cycle) { wsPhase = { reloadSent: false, doneSent: false, lastCycle: st.cycle }; }
    function send(detail) {
      try { window.dispatchEvent(new CustomEvent('ws-message', { detail: detail })); } catch (e) { /* ignore */ }
    }
    if (st.t >= TUNE_RELOAD_AT && !wsPhase.reloadSent) {
      wsPhase.reloadSent = true;
      send({ eventType: 'autotune_reload_triggered', backendId: 'cppworker-gpu-bundled-agent', model: MODEL, severity: 'info', message: 'AutoTune: reload (KV q4_0 → f16)' });
    }
    if (st.t >= TUNE_DONE_AT && !wsPhase.doneSent) {
      wsPhase.doneSent = true;
      send({ eventType: 'autotune_reload_succeeded', backendId: 'cppworker-gpu-bundled-agent', model: MODEL, severity: 'info', message: 'AutoTune: конфигурация применена и подтверждена' });
    }
  }

  // --- EventSource-заглушка (SSE /api/v1/events) --------------------------
  //
  // notifications.js открывает `new EventSource('/api/v1/events?token=...')`.
  // EventSource не проходит через перехват fetch, поэтому без заглушки демо
  // страница всё равно стучится в балансер (гость получает 401, а стенд —
  // лишние запросы). Подменяем на локальный поток с теми же событиями, что
  // видит панель демо-сценария.

  var esInstances = [];
  var lastNotified = null;

  function installEventSource() {
    if (typeof window === 'undefined' || typeof window.EventSource !== 'function') { return; }
    if (window.__demoEventSourceInstalled) { return; }
    window.__demoEventSourceInstalled = true;

    function DemoEventSource(url) {
      var self = this;
      this.url = url;
      this.readyState = 0;
      this.onopen = null; this.onmessage = null; this.onerror = null;
      this._listeners = {};
      this._closed = false;
      esInstances.push(this);
      setTimeout(function () {
        if (self._closed) { return; }
        self.readyState = 1;
        self._fire('open', {});
      }, 100);
    }
    DemoEventSource.prototype._fire = function (type, ev) {
      var handler = this['on' + type];
      var listeners = (this._listeners[type] || []).slice();
      try { if (typeof handler === 'function') { handler(ev); } } catch (e) { /* ignore */ }
      for (var i = 0; i < listeners.length; i++) {
        try { listeners[i].call(this, ev); } catch (e2) { /* ignore */ }
      }
    };
    DemoEventSource.prototype.addEventListener = function (type, fn) {
      (this._listeners[type] = this._listeners[type] || []).push(fn);
    };
    DemoEventSource.prototype.removeEventListener = function (type, fn) {
      var arr = this._listeners[type] || [];
      var i = arr.indexOf(fn);
      if (i >= 0) { arr.splice(i, 1); }
    };
    DemoEventSource.prototype.close = function () {
      this._closed = true;
      this.readyState = 2;
      var i = esInstances.indexOf(this);
      if (i >= 0) { esInstances.splice(i, 1); }
    };
    DemoEventSource.CONNECTING = 0; DemoEventSource.OPEN = 1; DemoEventSource.CLOSED = 2;
    DemoEventSource.prototype.CONNECTING = 0; DemoEventSource.prototype.OPEN = 1; DemoEventSource.prototype.CLOSED = 2;
    window.EventSource = DemoEventSource;
  }

  // Синтетические уведомления: только значимые события (клиент, донор, AutoTune),
  // чтобы колокольчик не забивался очередью.
  function pumpNotifications(st) {
    if (!esInstances.length) { return; }
    var ev = st.events && st.events[0];
    if (!ev) { return; }
    var key = ev.kind + '@' + ev.at + ':' + ev.text;
    if (key === lastNotified) { return; }
    lastNotified = key;
    if (ev.kind !== 'client' && ev.kind !== 'donor' && ev.kind !== 'autotune') { return; }
    var payload = JSON.stringify({
      type: 'notification',
      id: 'demo-' + ev.kind + '-' + Math.round(ev.at),
      timestamp: iso(ev.at),
      severity: ev.kind === 'autotune' ? 'warning' : 'info',
      category: ev.kind,
      message: ev.text
    });
    var list = esInstances.slice();
    for (var i = 0; i < list.length; i++) {
      if (list[i]._closed) { continue; }
      try { list[i]._fire('message', { data: payload, type: 'message' }); } catch (e) { /* ignore */ }
    }
  }

  // --- UI: баннер + панель «Демо-сценарий» -------------------------------

  function esc(s) {
    return String(s === null || s === undefined ? '' : s).replace(/[&<>"]/g, function (c) {
      return ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c];
    });
  }

  function ensureUI() {
    if (typeof document === 'undefined' || !document.body) { return; }
    if (document.body.classList) { document.body.classList.add('demo-mode'); }
    if (!document.getElementById('demoModeBanner')) {
      var banner = document.createElement('div');
      banner.id = 'demoModeBanner';
      banner.className = 'demo-banner';
      banner.innerHTML = '<b>🔶 ДЕМО-РЕЖИМ</b> · имитация кластера для гостевого просмотра · ' +
        '<span>данные синтетические, только чтение, время ускорено</span>';
      if (document.body.insertBefore) { document.body.insertBefore(banner, document.body.firstChild); }
      else { document.body.appendChild(banner); }
    }
    if (!document.getElementById('demoScenario')) {
      var panel = document.createElement('div');
      panel.id = 'demoScenario';
      panel.className = 'demo-scenario';
      panel.innerHTML =
        '<div class="demo-scenario-head">' +
          '<span class="demo-scenario-title">Демо-сценарий</span>' +
          '<button type="button" id="demoScenarioToggle" class="demo-scenario-toggle" title="Свернуть">–</button>' +
        '</div>' +
        '<div class="demo-scenario-phase"><b id="demoPhase">…</b><span id="demoPhaseNote"></span></div>' +
        '<div class="demo-scenario-counters">' +
          '<span>клиенты <b id="demoClients">0</b></span>' +
          '<span>очередь <b id="demoQueue">0</b></span>' +
          '<span>GPU <b id="demoGpu">0/0</b></span>' +
          '<span>донор <b id="demoRpc">0</b></span>' +
          '<span>tok/s <b id="demoTps">0</b></span>' +
          '<span>AutoTune <b id="demoTune">ok</b></span>' +
        '</div>' +
        '<ol class="demo-scenario-feed" id="demoFeed"></ol>' +
        '<div class="demo-scenario-foot">Демо включается только через env <code>WEBUI_DEMO_MODE=1</code>.</div>';
      document.body.appendChild(panel);
      var toggle = document.getElementById('demoScenarioToggle');
      if (toggle && toggle.addEventListener) {
        toggle.addEventListener('click', function () {
          if (panel.classList.toggle) { panel.classList.toggle('collapsed'); }
          toggle.textContent = (panel.classList && panel.classList.contains('collapsed')) ? '+' : '–';
        });
      }
    }
  }

  // hideNonDemoPages — служебные страницы (health/rpc-status/tp-pipeline/
  // virtual-models) не участвуют в демо (у них свои запросы и своя вёрстка),
  // поэтому в демо-режиме не показываем ссылки на них: гость не должен
  // попадать в страницы, где нет синтетики.
  function hideNonDemoPages() {
    if (typeof document === 'undefined' || !document.querySelectorAll) { return; }
    var links = document.querySelectorAll('a[href]');
    for (var i = 0; i < links.length; i++) {
      var href = String(links[i].getAttribute('href') || '');
      if (/^(health|rpc-status|tp-pipeline|virtual-models)\.html/i.test(href)) {
        links[i].style.display = 'none';
        links[i].setAttribute('data-demo-hidden', '1');
      }
    }
  }

  function setText(id, value) {
    var el = typeof document !== 'undefined' ? document.getElementById(id) : null;
    if (el) { el.textContent = String(value); }
  }

  function renderUI(st) {
    if (typeof document === 'undefined') { return; }
    ensureUI();
    setText('demoPhase', st.phase.title);
    setText('demoPhaseNote', st.phase.note);
    setText('demoClients', st.clientCount);
    setText('demoQueue', st.queueStats.current_size);
    setText('demoGpu', st.gpuLayers + '/' + st.model.nLayers);
    setText('demoRpc', st.rpcLayers);
    setText('demoTps', st.tokS.toFixed(2));
    setText('demoTune', st.autoTune.overallSeverity === 'ok' ? 'optimal' : (st.autoTune.overallSeverity === 'warning' ? 'рекомендация' : 'анализ'));
    var feed = document.getElementById('demoFeed');
    if (feed) {
      feed.innerHTML = st.events.slice(0, 8).map(function (e) {
        var mm = Math.floor(e.at / 60); var ss = Math.floor(e.at % 60);
        var ts = (mm < 10 ? '0' : '') + mm + ':' + (ss < 10 ? '0' : '') + ss;
        return '<li class="demo-ev demo-ev-' + esc(e.kind) + '"><span class="demo-ev-ts">' + ts + '</span>' +
          '<span class="demo-ev-icon">' + esc(e.icon) + '</span>' +
          '<span class="demo-ev-text">' + esc(e.text) + '</span></li>';
      }).join('');
    }
  }

  // refreshAppOnPhaseChange — демо идёт быстрее штатного опроса, поэтому на
  // смене фазы просим приложение перерисовать дашборд из уже полученных данных:
  // счётчики (модели, очередь) не отстают от ускоренного сценария на такт.
  var lastPhase = null;
  function refreshAppOnPhaseChange(st) {
    var key = st.phase.id + '#' + st.cycle;
    if (key === lastPhase) { return; }
    lastPhase = key;
    try {
      var app = (typeof window !== 'undefined') ? window.App : null;
      if (app && app.context && typeof app.context.refreshPage === 'function') {
        app.context.refreshPage('dashboard');
      }
    } catch (e) { /* демо не должно ломать страницу */ }
  }

  function startLoop() {
    if (typeof window === 'undefined') { return; }
    var st = computeState(simNow().t);
    ensureUI();
    hideNonDemoPages();
    renderUI(st);
    announceWs();
    setTimeout(announceWs, 400);
    setInterval(function () {
      var s = computeState(simNow().t);
      refreshAppOnPhaseChange(s);
      renderUI(s);
      pumpWsEvents(s);
      pumpNotifications(s);
    }, 1000);
  }

  // --- публичный объект ---------------------------------------------------

  var DemoMode = {
    enabled: ENABLED,
    cycleSec: CYCLE_SEC,
    state: function (tSec) { return computeState(tSec === undefined ? simNow().t : tSec); },
    route: route,
    responseBody: responseBody,
    phaseAt: phaseAt,
    autoTuneAt: autoTuneAt,
    _clients: CLIENTS,
    _timeline: { donorAt: DONOR_AT, recommendAt: TUNE_REC_AT, reloadAt: TUNE_RELOAD_AT, doneAt: TUNE_DONE_AT, okAt: TUNE_OK_AT }
  };
  try { window.DemoMode = DemoMode; } catch (e) { /* no window (tests) */ }

  if (!ENABLED) { return; }

  installFetch();
  installWebSocket();
  installEventSource();
  if (typeof document !== 'undefined') {
    if (document.readyState === 'loading' && typeof document.addEventListener === 'function') {
      document.addEventListener('DOMContentLoaded', startLoop);
    } else {
      startLoop();
    }
  }
})();
