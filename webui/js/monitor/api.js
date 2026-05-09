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
          { id: 'ollama-1', status: 'active', activeRequests: 2, maxConcurrentRequests: 8, gpu: { usagePercent: 45 }, vram: { usagePercent: 62, totalGB: 24, usedGB: 14.88 }, system: { cpuUsagePercent: 30, memoryUsagePercent: 55 }, score: 0.95, models: ['llama3.1', 'gemma2'], lastSeen: n, ollama: { requestsPerSecond: 1.2 } },
          { id: 'ollama-2', status: 'active', activeRequests: 1, maxConcurrentRequests: 8, gpu: { usagePercent: 12 }, vram: { usagePercent: 28, totalGB: 24, usedGB: 6.72 }, system: { cpuUsagePercent: 18, memoryUsagePercent: 40 }, score: 0.88, models: ['llama3.1'], lastSeen: n, ollama: { requestsPerSecond: 0.5 } },
          { id: 'ollama-3', status: 'error', activeRequests: 0, maxConcurrentRequests: 8, gpu: { usagePercent: 0 }, vram: { usagePercent: 0, totalGB: 24, usedGB: 0 }, system: { cpuUsagePercent: 5, memoryUsagePercent: 20 }, score: 0.0, models: [], lastSeen: n, ollama: { requestsPerSecond: 0 } }
        ],
        rps: 1.7,
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
      api('/api/v1/autopull/status').catch(function(e) { if (!isAbortError(e)) console.warn('[monitor] autopull status:', e.message); return null; })
    ]).then(function(r) {
      var cluster = r[0], qd = r[1], qs = r[2], sess = r[3], apCfg = r[4], apStatus = r[5];
      var data;
      if (cluster && (!cluster.backends || cluster.backends.length === 0)) {
        MA.lastData = null;
        data = null;
      } else {
        data = { cluster: cluster, queueDetails: qd, queueStats: qs, sessions: sess, autoPullConfig: apCfg, autoPullStatus: apStatus };
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
    if (typeof window.updateUI === 'function') window.updateUI(demoData());
    if (typeof window.updateMonitorTexts === 'function') window.updateMonitorTexts();
  }

  function togglePause() {
    MA.paused = !MA.paused;
    document.getElementById('pauseBtn').textContent = MA.paused
      ? '▶ ' + T('monitor.header.continue')
      : '⏸ ' + T('monitor.header.pause');
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