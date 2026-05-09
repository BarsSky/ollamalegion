// js/monitor/ui-renderer.js — UI update and table rendering

(function() {
  'use strict';

  var MA = window.MonitorApp;
  var T = MA.T;

  function updateUI(data) {
    if (!data || !data.cluster) {
      ['statRate','statRPSCluster','statActive','statPending','statProcessing','statSessions','statBackends','statVRAM','statModelsLoaded'].forEach(function(id) {
        document.getElementById(id).textContent = id === 'statVRAM' ? '—' : '0';
      });
      MA.lastTotalRequests = 0;
      MA.requestRate = 0;
      if (typeof window.setConnStatus === 'function') window.setConnStatus(T('monitor.status.idle'), 'green');
      return;
    }

    var adapt = function(b) {
      if (!b) return b;
      if (b.activeRequests !== undefined && b.vram !== undefined) return b;
      var o = b.ollama || b.Ollama || {}, g = b.gpu || b.GPU || {}, s = b.system || b.System || {};
      var sr = (b.status || b.Status || '').toString().toLowerCase();
      var sm = { healthy: 'active', active: 'active', ready: 'ready', unhealthy: 'error', offline: 'offline', starting: 'starting', ollama_unavailable: 'ollama_unavailable' };
      return Object.assign({}, b, {
        id: b.id || b.ID,
        status: sm[sr] || sr || 'unknown',
        score: b.score !== undefined ? b.score : (b.Score !== undefined ? b.Score : 50),
        activeRequests: b.activeRequests !== undefined ? b.activeRequests : (o.activeRequests || o.ActiveRequests || 0),
        maxConcurrentRequests: b.maxConcurrentRequests !== undefined ? b.maxConcurrentRequests : (o.maxConcurrentRequests || o.MaxConcurrentRequests || 10),
        models: b.models || ((o.runningModels || o.RunningModels || []).map(function(m) { return m.name || m.Name; })),
        vram: b.vram || {
          totalGB: (g.memoryTotal || g.MemoryTotal || 0) / 1024,
          usedGB: (g.memoryUsed || g.MemoryUsed || 0) / 1024,
          usagePercent: (g.memoryTotal || g.MemoryTotal || 0) > 0 ? (g.memoryUsed || g.MemoryUsed || 0) / (g.memoryTotal || g.MemoryTotal || 1) * 100 : 0
        },
        memoryUsagePercent: b.memoryUsagePercent || (function() {
          var t = s.memoryTotal || s.MemoryTotal || 0, u = s.memoryUsed || s.MemoryUsed || 0;
          return t > 0 ? u / t * 100 : 0;
        })(),
        gpu: g, system: s, ollama: o
      });
    };

    if (data.cluster && data.cluster.backends) data.cluster.backends = data.cluster.backends.map(adapt);
    if (data.cluster && data.cluster.backendMetrics) data.cluster.backendMetrics = data.cluster.backendMetrics.map(adapt);

    var bk = MA.stableBackendOrder(data.cluster.backends || []);
    var q = data.queueDetails || {}, ss = (data.sessions && data.sessions.sessions) || [];
    var act = bk.reduce(function(s, b) { return s + (b.activeRequests || 0); }, 0);
    var pend = q.pending_count || 0, proc = q.processing_count || 0;
    var totV = bk.reduce(function(s, b) { return s + ((b.vram && b.vram.totalGB) || 0); }, 0);
    var usdV = bk.reduce(function(s, b) { return s + ((b.vram && b.vram.usedGB) || 0); }, 0);
    var tml = bk.reduce(function(s, b) { return s + ((b.models || []).length); }, 0);

    document.getElementById('statBackends').textContent = bk.length;
    document.getElementById('statActive').textContent = act;
    document.getElementById('statPending').textContent = pend;
    document.getElementById('statProcessing').textContent = proc;
    document.getElementById('statSessions').textContent = ss.length;

    var now = Date.now(), dt = (now - MA.lastTime) / 1000;
    var tr = data.cluster.totalRequests || 0;
    MA.requestRate = Math.max(0, dt > 0 ? (tr - MA.lastTotalRequests) / dt : 0);
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

    renderAutoPullPanel(data.autoPullConfig || null, data.autoPullStatus || null);
    renderDispatchStats(data.queueStats || {});
    diagnose(data);
    renderClusterResources(bk);
    renderModelsInMemory(bk, ss);
    renderBackends(bk);
    renderQueue(q);
    renderSessions(ss);
    if (typeof window.updateTopology === 'function') window.updateTopology(bk, ss, q);
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
    var ms = data.cluster.queue ? data.cluster.queue.max_size : 100, cs = q.current_size || 0;
    if (cs > ms * 0.8) alerts.push({ level: 'red', text: T('monitor.alert.queueFull', { pct: (cs / ms * 100).toFixed(0), size: cs, max: ms }) });
    var tv = bks.reduce(function(s, b) { return s + ((b.vram && b.vram.totalGB) || 0); }, 0);
    var uv = bks.reduce(function(s, b) { return s + ((b.vram && b.vram.usedGB) || 0); }, 0);
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
      return '<div class="capacity-card"><h4>' + MA.esc(m.label) + '</h4><div class="capacity-bar"><div class="bar-track-wide"><div class="bar-fill-wide" style="width:' + avg + '%;background:' + m.color + '"></div></div><span class="capacity-val">avg ' + avg.toFixed(0) + m.unit + '</span></div><div class="capacity-bar"><div class="bar-track-wide"><div class="bar-fill-wide" style="width:' + max + '%;background:' + m.color + ';opacity:0.5"></div></div><span class="capacity-val">max ' + max.toFixed(0) + m.unit + '</span></div></div>';
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
      (b.models || []).forEach(function(m) {
        if (!mm[m]) mm[m] = { bks: [], sc: 0, cloud: false };
        mm[m].bks.push(b.id);
      });
    });
    // Add models from sessions (including cloud models not loaded locally)
    ss.forEach(function(s) {
      if (!s.model) return;
      if (!mm[s.model]) mm[s.model] = { bks: [], sc: 0, cloud: MA.isCloudModel(s.model) };
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
      var nameCell = '<strong>' + MA.esc(m.name) + '</strong>' + (m.info.cloud ? ' <span style="font-size:10px;color:var(--accent);background:rgba(59,130,246,0.12);padding:1px 5px;border-radius:4px">☁️ cloud</span>' : '');
      var bkCell = m.info.cloud && m.info.bks.length === 0
        ? '<span class="badge badge-blue">☁️ cloud</span>'
        : m.info.bks.map(function(bid) { return '<span class="badge badge-purple">' + MA.esc(bid) + '</span>'; }).join(' ');
      return '<tr><td>' + nameCell + '</td><td>' + bkCell + '</td><td class="col-right">' + m.info.sc + '</td><td class="col-right">' + (m.info.cloud ? '☁️ N/A' : '~' + vg + ' GB') + '</td><td class="col-right">' + (m.info.sc > 0 ? (m.info.cloud ? '☁️ ' + m.info.sc : '🔥 ' + m.info.sc) : '—') + '</td></tr>';
    }).join('');
  }

  function renderBackends(bk) {
    document.getElementById('backendCount').textContent = bk.length;
    var tb = document.querySelector('#backendsTable tbody');
    if (!bk.length) { tb.innerHTML = '<tr><td colspan="13" style="color:var(--text-secondary);text-align:center;padding:16px">' + T('monitor.common.noData') + '</td></tr>'; return; }
    tb.innerHTML = bk.map(function(b) {
      var mr = b.maxConcurrentRequests || 10, a = b.activeRequests || 0;
      var gu = (b.gpu && b.gpu.usagePercent != null) ? b.gpu.usagePercent : 0;
      var vu = (b.vram && b.vram.usagePercent != null) ? b.vram.usagePercent : (b.vramUsagePercent || 0);
      var cu = (b.system && b.system.cpuUsagePercent != null) ? b.system.cpuUsagePercent : 0;
      var ru = (b.memoryUsagePercent != null) ? b.memoryUsagePercent : ((b.system && b.system.memoryUsagePercent != null) ? b.system.memoryUsagePercent : 0);
      var sc = (b.score || b.weight || 0).toFixed(2);
      var scs = b.status === 'healthy' || b.status === 'active' || b.status === 'ready' ? 'badge-green' : (b.status === 'error' || b.status === 'unhealthy' ? 'badge-red' : (b.status === 'ollama_unavailable' ? 'badge-orange' : 'badge-yellow'));
      var up = b.lastSeen ? MA.fmtDur(Date.now() - new Date(b.lastSeen).getTime()) : '-';
      var rps = (b.ollama && b.ollama.requestsPerSecond != null) ? b.ollama.requestsPerSecond : 0;
      var avgRT = (b.ollama && b.ollama.avgResponseTime != null && b.ollama.avgResponseTime > 0) ? b.ollama.avgResponseTime.toFixed(0) + 'ms' : '-';
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
      var throttled = cpu.throttled != null ? (cpu.throttled ? '⚠️' : 'OK') : '-';
      var cpuHint = 'Load: ' + loadAvg + ' | Cores: ' + cores + (cpuModel ? ' (' + cpuModel + ')' : '') + ' | Throttle: ' + throttled;
      return '<tr><td><strong>' + MA.esc(b.id) + '</strong></td><td><span class="badge ' + scs + '">' + b.status + '</span></td><td>' + MA.bar(gu) + ' ' + gu.toFixed(0) + '%' + gpuHidden + '</td><td>' + MA.bar(vu) + ' ' + vu.toFixed(0) + '%</td><td title="' + cpuHint + '">' + MA.bar(cu) + ' ' + cu.toFixed(0) + '%</td><td>' + MA.bar(ru) + ' ' + ru.toFixed(0) + '%</td><td class="col-right">' + a + '/' + mr + '</td><td class="col-right">' + (rps > 0 ? rps.toFixed(1) : '-') + '</td><td class="col-right">' + avgRT + '</td><td class="col-right">' + reqCap + '</td><td class="col-right">' + sc + '</td><td>' + (b.models || []).slice(0, 3).map(function(m) { return '<span class="badge" style="background:rgba(168,85,247,0.12);color:var(--purple-accent);border-color:rgba(168,85,247,0.2)">' + MA.esc(m) + '</span>'; }).join(' ') + '</td><td class="col-right">' + up + '</td></tr>';
    }).join('');
  }

  function renderQueue(q) {
    var all = q.all || [];
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
      return '<tr><td>' + MA.esc(s.id.slice(0, 16)) + '…</td><td>' + MA.esc(s.backendId || '—') + '</td><td>' + modelCell + '</td><td class="col-right">' + (s.requestCount || 0) + '</td><td class="col-right">' + idl + '</td><td><span style="color:' + ipCol + ';font-weight:600">' + MA.esc(s.clientIP || '-') + '</span></td><td>' + ci + ' <span style="color:' + ipCol + '">' + MA.esc(s.clientName || '-') + '</span></td></tr>';
    }).join('');
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
      var statusText = p.done ? '✅ Done' : (p.error ? '❌ Error' : '⏳ Pulling...');
      var statusClass = p.done ? 'badge-green' : (p.error ? 'badge-red' : 'badge-yellow');
      var errorText = p.error ? MA.esc(p.error) : (p.httpStatus ? 'HTTP ' + p.httpStatus : '-');
      return '<tr><td><strong>' + model + '</strong></td><td>' + backend + '</td><td>' + started + '</td><td>' + duration + '</td><td><span class="badge ' + statusClass + '">' + statusText + '</span></td><td style="font-size:11px;color:var(--text-secondary);max-width:200px;overflow:hidden;text-overflow:ellipsis">' + errorText + '</td></tr>';
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

  window.updateUI = updateUI;
})();