// js/monitor/api.js — API fetching, error handling, demo mode

(function() {
  'use strict';

  var MA = window.MonitorApp;
  var T = MA.T;

  function isAbortError(e) {
    return e && (e.name === 'AbortError' || (e.message && e.message.indexOf('aborted') >= 0));
  }

  function api(path) {
    var token = new URLSearchParams(location.search).get('token') || localStorage.getItem('apiToken') || '';
    var ctrl = new AbortController();
    var tid = setTimeout(function() { ctrl.abort(); }, 15000);
    return fetch(MA.API_BASE + path, {
      headers: token ? { 'X-API-Token': token } : {},
      signal: ctrl.signal
    }).then(function(r) {
      clearTimeout(tid);
      if (!r.ok) throw new Error(r.status + ' ' + r.statusText);
      return r.json();
    }).catch(function(e) {
      clearTimeout(tid);
      throw e;
    });
  }

  function setConnStatus(text, cls) {
    document.getElementById('connBadge').textContent = text;
    document.getElementById('connBadge').className = 'badge badge-' + cls;
    var dot = document.getElementById('statusDot');
    dot.className = 'dot ' + (cls === 'green' ? '' : cls === 'red' ? 'offline' : cls === 'yellow' ? 'error' : 'demo');
  }

  function hideOverlays() {
    document.getElementById('loadingOverlay').classList.add('hidden');
    document.getElementById('errorOverlay').classList.add('hidden');
  }

  function showError(msg) {
    document.getElementById('loadingOverlay').classList.add('hidden');
    document.getElementById('errorOverlay').classList.remove('hidden');
    document.getElementById('errorText').textContent = msg;
    setConnStatus(T('monitor.status.offline'), 'red');
    document.getElementById('demoBtn').style.display = '';
  }

  function renderErrorDetails(err) {
    var el = document.getElementById('errorDetails'), d = [];
    d.push('API Base: ' + (MA.API_BASE || location.origin));
    d.push('Protocol: ' + location.protocol);
    d.push('Error: ' + (err.message || err.name || 'Unknown'));
    if (MA.corsErrorDetected) {
      d.push(T('monitor.error.corsDetails1'));
      d.push(T('monitor.error.corsDetails2'));
      d.push(T('monitor.error.corsDetails3'));
      d.push(T('monitor.error.corsDetails4'));
    }
    el.innerHTML = d.map(function(s) { return '<div>' + MA.esc(s) + '</div>'; }).join('');
  }



  function demoData() {
    var n = new Date().toISOString();
    return {
      cluster: {
        backends: [
          { id: 'ollama-1', type: 'ollama', backendType: 'ollama', status: 'active', activeRequests: 2, maxConcurrentRequests: 8, gpu: { usagePercent: 45 }, vram: { usagePercent: 62, totalGB: 24, usedGB: 14.88 }, system: { cpuUsagePercent: 30, memoryUsagePercent: 55, diskTotal: 500472979456, diskUsed: 209089773568, diskFree: 291383205888, networkRX: 1842176, networkTX: 1024512 }, score: 0.95, models: ['llama3.1', 'gemma2'], lastSeen: n, ollama: { requestsPerSecond: 1.2, backendCapacity: { freeVram: 8192, loadableModelCount: 3, guaranteedVram: 2048, mode: 'gpu', availableModels: [{ name: 'mistral:7b', canLoad: true, estimatedVram: 4096 }, { name: 'phi3:mini', canLoad: true, estimatedVram: 2048 }, { name: 'qwen2:7b', canLoad: true, estimatedVram: 4096 }, { name: 'codellama:13b', canLoad: false, estimatedVram: 10240 }, { name: 'mixtral:8x7b', canLoad: false, estimatedVram: 28672 }] } } },
          { id: 'ollama-2', type: 'ollama', backendType: 'ollama', status: 'active', activeRequests: 1, maxConcurrentRequests: 8, gpu: { usagePercent: 12 }, vram: { usagePercent: 28, totalGB: 24, usedGB: 6.72 }, system: { cpuUsagePercent: 18, memoryUsagePercent: 40, diskTotal: 1000965890048, diskUsed: 322122547200, diskFree: 678843342848, networkRX: 5242880, networkTX: 3145728 }, score: 0.88, models: ['llama3.1'], lastSeen: n, ollama: { requestsPerSecond: 0.5, backendCapacity: { freeVram: 16384, loadableModelCount: 5, guaranteedVram: 2048, mode: 'gpu', availableModels: [{ name: 'gemma2:9b', canLoad: true, estimatedVram: 5120 }, { name: 'llama3.1:70b', canLoad: true, estimatedVram: 40960 }, { name: 'deepseek-r1:7b', canLoad: true, estimatedVram: 4096 }, { name: 'phi3:mini', canLoad: true, estimatedVram: 2048 }, { name: 'mistral:7b', canLoad: true, estimatedVram: 4096 }, { name: 'codellama:34b', canLoad: false, estimatedVram: 22528 }] } } },
          { id: 'ollama-3', type: 'ollama', backendType: 'ollama', status: 'error', activeRequests: 0, maxConcurrentRequests: 8, gpu: { usagePercent: 0 }, vram: { usagePercent: 0, totalGB: 24, usedGB: 0 }, system: { cpuUsagePercent: 5, memoryUsagePercent: 20, diskTotal: 250225098752, diskUsed: 131941395333, diskFree: 118283703419 }, score: 0.0, models: [], lastSeen: n, ollama: { requestsPerSecond: 0, backendCapacity: { freeVram: 24576, loadableModelCount: 0, guaranteedVram: 0, mode: 'gpu', availableModels: [] } } },
          { id: 'llamacpp-1', type: 'llama_cpp', backendType: 'llama_cpp', status: 'active', activeRequests: 1, maxConcurrentRequests: 4, gpu: { usagePercent: 72 }, vram: { usagePercent: 78, totalGB: 8, usedGB: 6.24 }, system: { cpuUsagePercent: 45, memoryUsagePercent: 60, diskTotal: 500472979456, diskUsed: 209089773568, diskFree: 291383205888, networkRX: 1048576, networkTX: 524288 }, score: 0.82, models: ['qwen2.5:7b-q4_k_m', 'gemma-2:9b-q4_k_m'], lastSeen: n, ollama: { requestsPerSecond: 0.8, backendCapacity: { freeVram: 1536, loadableModelCount: 1, guaranteedVram: 1024, mode: 'gpu', availableModels: [{ name: 'qwen2.5:7b-q4_k_m', canLoad: true, estimatedVram: 5120 }, { name: 'gemma-2:9b-q4_k_m', canLoad: true, estimatedVram: 6144 }, { name: 'llama-3.2:3b-q4_k_m', canLoad: true, estimatedVram: 2048 }, { name: 'deepseek-r1:7b-q4_k_m', canLoad: false, estimatedVram: 11264 }] } } },
          { id: 'llamacpp-2', type: 'llama_cpp', backendType: 'llama_cpp', status: 'healthy', activeRequests: 0, maxConcurrentRequests: 4, gpu: { usagePercent: 0 }, vram: { usagePercent: 15, totalGB: 8, usedGB: 1.2 }, system: { cpuUsagePercent: 12, memoryUsagePercent: 35, diskTotal: 1000965890048, diskUsed: 322122547200, diskFree: 678843342848, networkRX: 2097152, networkTX: 1048576 }, score: 0.75, models: ['phi-4:14b-q4_k_m'], lastSeen: n, ollama: { requestsPerSecond: 0, backendCapacity: { freeVram: 6912, loadableModelCount: 2, guaranteedVram: 2048, mode: 'gpu', availableModels: [{ name: 'phi-4:14b-q4_k_m', canLoad: true, estimatedVram: 9216 }, { name: 'qwen2.5:7b-q4_k_m', canLoad: true, estimatedVram: 5120 }] } } }
        ],
        rps: 1.7,
        backendEngine: 'llama_cpp',
        effectiveBackendType: '',
        queue: { max_size: 100 }
      },

      queueDetails: {
        pending_count: 3,
        processing_count: 2,
        current_size: 5,
        processed_total: 128,
        all: [
          { model: 'llama3.1', target: null, status: 'pending', enqueued: new Date(Date.now() - 4200).toISOString(), sessionId: 'sess-demo-1' },
          { model: 'gemma2', target: null, status: 'pending', enqueued: new Date(Date.now() - 1800).toISOString(), sessionId: 'sess-demo-2' },
          { model: 'llama3.1', target: 'ollama-1', status: 'processing', enqueued: new Date(Date.now() - 800).toISOString(), sessionId: 'sess-demo-3' },
          { model: 'llama3.1', target: 'ollama-2', status: 'processing', enqueued: new Date(Date.now() - 600).toISOString(), sessionId: 'sess-demo-4' },
          { model: 'llama3.1', target: null, status: 'pending', enqueued: new Date(Date.now() - 200).toISOString(), sessionId: 'sess-demo-5' }
        ]
      },
      virtualModels: {
        enabled: true,
        count: 2,
        models: [
          {
            name: 'deepseek-r1-vm',
            description: 'Двухсрезная виртуальная модель DeepSeek-R1',
            mode: 'pipeline',
            slices: [
              { id: 'slice-1', model: 'deepseek-r1:1.5b', ordinal: 1, targetBackends: ['ollama-1','ollama-2'], fallbackBackends: ['ollama-3'] },
              { id: 'slice-2', model: 'deepseek-r1:7b', ordinal: 2, targetBackends: ['ollama-2'], fallbackBackends: ['ollama-1'] }
            ],
            activeJobs: 1,
            timeoutMs: 60000,
            jobInfos: [
              { requestId: 'vm-demo-001', currentSlice: 1, hasError: false }
            ]
          },
          {
            name: 'llama3.1-vm',
            description: 'Трёхсрезная виртуальная модель LLaMA 3.1',
            mode: 'pipeline',
            slices: [
              { id: 'slice-1', model: 'llama3.1:8b', ordinal: 1, targetBackends: ['ollama-1','ollama-2'] },
              { id: 'slice-2', model: 'llama3.1:8b', ordinal: 2, targetBackends: ['ollama-2'] },
              { id: 'slice-3', model: 'llama3.1:70b', ordinal: 3, targetBackends: ['ollama-1'], fallbackBackends: ['ollama-2'] }
            ],
            activeJobs: 0,
            timeoutMs: 120000,
            jobInfos: []
          }
        ]
      },
      candidates: [
        {
          model: "llama3.1:8b",
          groups: [
            { priority: 1, label: "LOADED", backend_ids: ["ollama-1", "ollama-2"] },
            { priority: 3, label: "FREE", backend_ids: ["ollama-3"] },
            { priority: 4, label: "FALLBACK", backend_ids: ["ollama-1", "ollama-2", "ollama-3"] }
          ],
          total: 6,
          has_ready: true
        },
        {
          model: "gemma2:9b",
          groups: [
            { priority: 1, label: "LOADED", backend_ids: ["ollama-1"] },
            { priority: 3, label: "FREE", backend_ids: ["ollama-3"] },
            { priority: 4, label: "FALLBACK", backend_ids: ["ollama-1", "ollama-2", "ollama-3"] }
          ],
          total: 5,
          has_ready: true
        },
        {
          model: "deepseek-r1:7b",
          groups: [
            { priority: 2, label: "WARMING", backend_ids: ["ollama-2"] },
            { priority: 3, label: "FREE", backend_ids: ["ollama-3"] },
            { priority: 4, label: "FALLBACK", backend_ids: ["ollama-1", "ollama-2", "ollama-3"] }
          ],
          total: 5,
          has_ready: false
        }
      ],
      queueStats: {
        current_size: 5,
        max_size: 100,
        processed_total: 15420,
        avg_wait_time_ms: 12,
        workers: 4,
        timeout_sec: 30,
        dispatch_by_affinity: 8750,
        dispatch_by_load: 4520,
        dispatch_by_config: 2150
      },
      sessions: {
        sessions: [
          { id: '192.168.1.100::Cline::llama3.1:cloud', backendId: '', model: 'llama3.1:cloud', clientIP: '192.168.1.100', clientName: 'Cline', requestCount: 12, lastRequestAt: new Date(Date.now() - 3000).toISOString() },
          { id: '10.0.0.55::Cline::llama3.1:cloud', backendId: '', model: 'llama3.1:cloud', clientIP: '10.0.0.55', clientName: 'Cline', requestCount: 7, lastRequestAt: new Date(Date.now() - 1500).toISOString() },
          { id: '192.168.1.100::OpenWebUI::gemma2', backendId: 'ollama-2', model: 'gemma2', clientIP: '192.168.1.100', clientName: 'OpenWebUI', requestCount: 5, lastRequestAt: new Date(Date.now() - 8000).toISOString() },
          { id: '192.168.1.100::python-requests::llama3.1', backendId: 'ollama-1', model: 'llama3.1', clientIP: '192.168.1.100', clientName: 'python-requests/2.31.0', requestCount: 3, lastRequestAt: new Date(Date.now() - 1200).toISOString() },
          { id: '10.0.0.50::Cline::deepseek-r1', backendId: 'ollama-1', model: 'deepseek-r1', clientIP: '10.0.0.50', clientName: 'Cline', requestCount: 8, lastRequestAt: new Date(Date.now() - 4000).toISOString() }
        ]
      },
      modelOps: {
        operations: [
          { operation: 'load', modelName: 'llama3.1:8b', backendId: 'ollama-1', startedAt: new Date(Date.now() - 5000).toISOString(), duration: '5s', status: 'running' },
          { operation: 'pull', modelName: 'deepseek-r1:7b', backendId: 'ollama-2', startedAt: new Date(Date.now() - 12000).toISOString(), duration: '12s', status: 'running' }
        ]
      }
    };
  }

  function fetchAutoPullConfig() {
    return api('/api/v1/autopull').catch(function(e) { if (!isAbortError(e)) console.warn('[monitor] autopull config:', e.message); return null; });
  }

  function fetchAutoPullStatus() {
    return api('/api/v1/autopull/status').catch(function(e) { if (!isAbortError(e)) console.warn('[monitor] autopull status:', e.message); return null; });
  }

  function updateAutoPullConfig(cfg) {
    var token = new URLSearchParams(location.search).get('token') || localStorage.getItem('apiToken') || '';
    var ctrl = new AbortController();
    var tid = setTimeout(function() { ctrl.abort(); }, 15000);
    return fetch(MA.API_BASE + '/api/v1/autopull', {
      method: 'PUT',
      headers: token ? { 'Content-Type': 'application/json', 'X-API-Token': token } : { 'Content-Type': 'application/json' },
      body: JSON.stringify(cfg),
      signal: ctrl.signal
    }).then(function(r) {
      clearTimeout(tid);
      if (!r.ok) throw new Error(r.status + ' ' + r.statusText);
      return r.json();
    }).catch(function(e) {
      clearTimeout(tid);
      throw e;
    });
  }

  function fetchAll() {
    if (MA.paused || MA.fetching) return Promise.resolve();
    if (MA.demoMode) {
      if (typeof window.updateUI === 'function') window.updateUI(demoData());
      return Promise.resolve();
    }
    MA.fetching = true;
    return Promise.all([
      api('/api/v1/cluster').catch(function(e) { if (!isAbortError(e)) console.warn('[monitor] cluster:', e.message); return null; }),
      api('/api/v1/queue/details').catch(function(e) { if (!isAbortError(e)) console.warn('[monitor] queue:', e.message); return null; }),
      api('/api/v1/queue/stats').catch(function(e) { if (!isAbortError(e)) console.warn('[monitor] queue stats:', e.message); return null; }),
      api('/api/v1/sessions').catch(function(e) { if (!isAbortError(e)) console.warn('[monitor] sessions:', e.message); return null; }),
      api('/api/v1/autopull').catch(function(e) { if (!isAbortError(e)) console.warn('[monitor] autopull config:', e.message); return null; }),
      api('/api/v1/autopull/status').catch(function(e) { if (!isAbortError(e)) console.warn('[monitor] autopull status:', e.message); return null; }),
      api('/api/v1/virtualmodels').catch(function(e) { if (!isAbortError(e)) console.debug('[monitor] virtualmodels:', e.message); return null; }),
      api('/api/v1/candidates').catch(function(e) { if (!isAbortError(e)) console.warn('[monitor] candidates:', e.message); return null; }),
      api('/api/v1/models/operations').catch(function(e) { if (!isAbortError(e)) console.warn('[monitor] model ops:', e.message); return null; })
    ]).then(function(r) {
      var cluster = r[0], qd = r[1], qs = r[2], sess = r[3], apCfg = r[4], apStatus = r[5], vm = r[6], cand = r[7], modelOps = r[8];
      var data;
      if (cluster && (!cluster.backends || cluster.backends.length === 0)) {
        MA.lastData = null;
        data = null;
      } else {
        data = { cluster: cluster, queueDetails: qd, queueStats: qs, sessions: sess, autoPullConfig: apCfg, autoPullStatus: apStatus, virtualModels: vm, candidates: cand, modelOps: modelOps };
        MA.lastData = data;
      }

      MA.fetchAttempt = 0;
      MA.corsErrorDetected = false;
      hideOverlays();
      document.getElementById('demoIndicator').style.display = 'none';
      setConnStatus(T('monitor.status.live'), 'green');
      document.getElementById('demoBtn').style.display = 'none';
      if (typeof window.updateUI === 'function') window.updateUI(data);
    }).finally(function() { MA.fetching = false; });
  }

  function fetchAllSafe() {
    fetchAll().catch(function(e) {
      if (!isAbortError(e)) console.error('[monitor] fetchAll failed:', e);
      MA.fetchAttempt++;
      MA.fetching = false;
      var msgText = e.message || '';
      var isNetwork = msgText === 'Failed to fetch' || e.name === 'TypeError';
      var isCors = isNetwork && MA.API_BASE === '';
      var isAuth = msgText.indexOf('401') >= 0 || msgText.indexOf('403') >= 0;
      if (isCors) MA.corsErrorDetected = true;
      if (isAuth && MA.fetchAttempt < 3) {
        setConnStatus('🔒 Auth Required', 'red');
        document.getElementById('errorOverlay').classList.add('hidden');
        document.getElementById('loadingOverlay').classList.add('hidden');
        return;
      }
      if (MA.fetchAttempt >= 3) {
        var msg = T('monitor.error.balancerUnreachable', { count: MA.fetchAttempt });
        if (isAuth) msg = '🔒 ' + T('monitor.error.authRequired') + ' (' + msgText + ')';
        else if (MA.corsErrorDetected) msg += T('monitor.error.corsHint');
        else if (isNetwork) msg += T('monitor.error.networkHint', { addr: MA.API_BASE || location.origin });
        showError(msg);
        renderErrorDetails(e);
      } else {
        setConnStatus(T('monitor.status.offline'), 'red');
      }
    });
  }

  function retryNow() {
    MA.fetchAttempt = 0;
    document.getElementById('errorOverlay').classList.add('hidden');
    document.getElementById('loadingOverlay').classList.remove('hidden');
    setConnStatus(T('monitor.header.connecting'), 'yellow');
    fetchAll();
  }

  function enterDemoMode() {
    MA.demoMode = true;
    hideOverlays();
    setConnStatus(T('monitor.status.demo'), 'blue');
    document.getElementById('demoBtn').style.display = 'none';
    document.getElementById('demoIndicator').style.display = 'inline';
    // P-5: Добавляем DEMO MODE баннер для визуальной индикации
    var demoBanner = document.getElementById('demoModeBanner');
    if (!demoBanner) {
      demoBanner = document.createElement('div');
      demoBanner.id = 'demoModeBanner';
      demoBanner.style.cssText = 'position:fixed;top:0;left:0;right:0;z-index:9999;background:rgba(255,165,0,0.15);border-bottom:2px solid #ff8c00;text-align:center;padding:6px 12px;font-size:13px;font-weight:600;color:#ff8c00;backdrop-filter:blur(4px);pointer-events:none';
      demoBanner.textContent = '🔶 DEMO MODE — отображаются тестовые данные. Подключитесь к балансировщику для реальных метрик.';
      document.body.prepend(demoBanner);
    }
    demoBanner.style.display = '';
    if (typeof window.updateUI === 'function') window.updateUI(demoData());
    if (typeof window.updateMonitorTexts === 'function') window.updateMonitorTexts();
  }

  function togglePause() {
    MA.paused = !MA.paused;
    // Round 18e: иконка и текст — отдельные span'ы чтобы не дублировались.
    var icon = document.getElementById('pauseBtnIcon');
    var text = document.getElementById('pauseBtnText');
    if (icon) icon.textContent = MA.paused ? '▶' : '⏸';
    if (text) text.textContent = T(MA.paused ? 'monitor.header.continue' : 'monitor.header.pause');
    setConnStatus(
      MA.paused ? T('monitor.header.pause') : (MA.demoMode ? T('monitor.status.demo') : T('monitor.status.live')),
      MA.paused ? 'yellow' : MA.demoMode ? 'blue' : 'green'
    );
  }

  // Expose to global for inline onclick handlers
  window.retryNow = retryNow;
  window.enterDemoMode = enterDemoMode;
  window.togglePause = togglePause;
  window.fetchAllSafe = fetchAllSafe;
  window.fetchAll = fetchAll;
  window.setConnStatus = setConnStatus;
  window.fetchAutoPullConfig = fetchAutoPullConfig;
  window.fetchAutoPullStatus = fetchAutoPullStatus;
  window.updateAutoPullConfig = updateAutoPullConfig;

  // Interval select listener
  document.getElementById('intervalSelect').addEventListener('change', function(e) {
    MA.refreshInterval = parseInt(e.target.value);
    if (MA.timerId) {
      clearInterval(MA.timerId);
      MA.timerId = setInterval(fetchAllSafe, MA.refreshInterval);
    }
  });
})();