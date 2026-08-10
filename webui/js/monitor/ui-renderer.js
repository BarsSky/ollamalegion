// js/monitor/ui-renderer.js — UI update and table rendering

(function() {
  'use strict';

  var MA = window.MonitorApp;
  var T = MA.T;

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
      if (typeof window.setConnStatus === 'function') window.setConnStatus(T('monitor.status.idle'), 'green');
      return;
    }

    var adapt = function(b) {
      if (!b) return b;
      if (b.activeRequests !== undefined && b.vram !== undefined) return b;
      var o = b.ollama || b.Ollama || {};
      // Round 18f: для llama.cpp бэкендов читаем из b.llamaCpp (cppworker poller).
      // balancer НЕ выставляет b.ollama.activeRequests для llama.cpp, поэтому
      // stats bar (statActive/statRate/statRPSCluster) показывал «—». Теперь
      // fallback на b.llamaCpp для всех полей.
      var lc = b.llamaCpp || b.LlamaCpp || {};
      var g = b.gpu || b.GPU || {}, s = b.system || b.System || {};
      var sr = (b.status || b.Status || '').toString().toLowerCase();
      var sm = { healthy: 'active', active: 'active', ready: 'ready', unhealthy: 'error', offline: 'offline', starting: 'starting', ollama_unavailable: 'ollama_unavailable' };
      // Active requests / RPS — fallback llama.cpp → ollama → 0.
      var activeReq = b.activeRequests;
      if (activeReq === undefined) activeReq = lc.activeRequests !== undefined ? lc.activeRequests : (o.activeRequests || 0);
      // Models list — для llama.cpp берём из loadedModels.
      var models = b.models;
      if (!models) {
        if (Array.isArray(lc.loadedModels) && lc.loadedModels.length) {
          models = lc.loadedModels.map(function (m) { return m.name || m.Name || m; });
        } else {
          models = (o.runningModels || []).map(function (m) { return m.name || m; });
        }
      }
      return Object.assign({}, b, {
        id: b.id || b.ID,
        status: sm[sr] || sr || 'unknown',
        score: b.score !== undefined ? b.score : (b.Score !== undefined ? b.Score : 50),
        activeRequests: activeReq,
        maxConcurrentRequests: b.maxConcurrentRequests !== undefined ? b.maxConcurrentRequests : (lc.maxConcurrentReqs || lc.maxConcurrentRequests || o.maxConcurrentRequests || 10),
        models: models,
        backendType: b.backendType || b.BackendType || b.type || b.Type || '',
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
        gpu: g, system: s, ollama: o, llamaCpp: lc
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
    if (userType && (userType === 'llama_cpp' || userType === 'ollama')) {
      effType = userType;
    }
    if (effType) {
      bk = bk.filter(function(b) {
        var bt = b.backendType || b.BackendType || b.backend_type || b.type || '';
        if (effType === 'llama_cpp') return bt === 'llama_cpp';
        if (effType === 'ollama') return bt === 'ollama' || bt === '' || bt === 'ollama_api';
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
    hideOllamaOnlyPanels(bk);

    renderAutoPullPanel(data.autoPullConfig || null, data.autoPullStatus || null);
    renderModelOps(data.modelOps || null);
    renderDispatchStats(data.queueStats || {});
    renderCandidateBackends(data.candidates || null);
    renderVirtualModels(data.virtualModels || null);
    diagnose(data);
    renderClusterResources(bk);
    renderModelsInMemory(bk, ss);
    renderBackends(bk, data.modelOps || null);
    // Добавляем бейджи типа бэкенда после рендера таблиц
    if (window.BackendTypeBadges) {
        BackendTypeBadges.enhanceBackendsTable();
    }
    // Round 32 #4 (2026-08-10): bind delegated click handler для inline Unload buttons
    // в таблице backends. handler attached ОДИН раз (через _ggufUnloadBound флаг)
    // чтобы не утекали listeners при каждом refresh'е таблицы.
    bindUnloadHandlers();
    renderDiskNetwork(bk);
    // B-12: Render backend type switcher on first load
    renderBackendTypeSwitcher();
    renderLoadFeasibility(bk);
    renderQueue(q);
    renderSessions(ss);
    if (typeof window.updateTopology === 'function') window.updateTopology(bk, ss, q, recentClients, warmingUpModels);
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
      var nameCell = '<strong>' + MA.esc(m.name) + '</strong>' + digHTML + (m.info.cloud ? ' <span style="font-size:10px;color:var(--accent);background:rgba(59,130,246,0.12);padding:1px 5px;border-radius:4px">☁️ ' + T('renderers.cloud') + '</span>' : '');
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
              } else if (btType === 'ollama' || !btType) {
                btBadge = ' <span class="badge" style="background:#1a73e820;border:1px solid #1a73e8;color:#1a73e8;font-size:9px;padding:0 3px;border-radius:2px">🦙</span>';
              }
            }
            return '<span class="badge badge-purple">' + MA.esc(bid) + btBadge + '</span>';
          }).join(' ');
      return '<tr><td>' + nameCell + '</td><td>' + bkCell + '</td><td class="col-right">' + m.info.sc + '</td><td class="col-right">' + (m.info.cloud ? '☁️ N/A' : '~' + vg + ' GB') + '</td><td class="col-right">' + (m.info.sc > 0 ? (m.info.cloud ? '☁️ ' + m.info.sc : '🔥 ' + m.info.sc) : '—') + '</td><td class="col-right" style="font-size:11px">' + expHTML + '</td></tr>';
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
    if (!bk.length) { tb.innerHTML = '<tr><td colspan="14" style="color:var(--text-secondary);text-align:center;padding:16px">' + T('monitor.common.noData') + '</td></tr>'; return; }
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
      // Per-backend type badge (🦙 Ollama / 🦒 llama.cpp)
      var btType = b.backendType || '';
      var typeBadge = '';
      if (btType === 'llama_cpp') {
        typeBadge = ' <span class="badge" style="background:#ff6d0020;border:1px solid #ff6d00;color:#ff6d00;font-size:10px;padding:0 4px;border-radius:3px">🦒 llama.cpp</span>';
      } else if (btType === 'ollama' || !btType) {
        typeBadge = ' <span class="badge" style="background:#1a73e820;border:1px solid #1a73e8;color:#1a73e8;font-size:10px;padding:0 4px;border-radius:3px">🦙 Ollama</span>';
      }
// Sparkline helper: record + render per backend (no-op if Sparkline not loaded).
      function sl(bid, key, color) {
        if (!window.Sparkline) return '';
        try {
          var avgRTraw = (b.ollama && b.ollama.avgResponseTime != null) ? b.ollama.avgResponseTime : 0;
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
      var modelsCell = (b.models || []).slice(0, 3).map(function(m) {
        return '<span class="badge" style="background:rgba(168,85,247,0.12);color:var(--purple-accent);border-color:rgba(168,85,247,0.2)">' +
          MA.esc(m) +
          ' <button class="monitor-unload-btn" data-backend="' + MA.esc(b.id) + '" data-model="' + MA.esc(m) + '" ' +
          'title="Unload ' + MA.esc(m) + '" ' +
          'style="background:transparent;border:none;color:inherit;cursor:pointer;padding:0 2px;font-size:11px;line-height:1;opacity:0.7;">' +
          '<i class="fas fa-times"></i></button>' +
        '</span>';
      }).join(' ');
      return '<tr data-backend-type="' + MA.esc(btType) + '"><td><strong>' + MA.esc(b.id) + '</strong>' + typeBadge + '</td><td><span class="badge ' + scs + '">' + b.status + '</span></td><td>' + MA.bar(gu) + ' ' + gu.toFixed(0) + '%' + gpuHidden + '<div class="sl-cell">' + sl(b.id, 'gpu', 'var(--accent)') + '</div></td><td>' + MA.bar(vu) + ' ' + vu.toFixed(0) + '%<div class="sl-cell">' + sl(b.id, 'vram', 'var(--purple-accent)') + '</div></td><td title="' + cpuHint + '">' + MA.bar(cu) + ' ' + cu.toFixed(0) + '%<div class="sl-cell">' + sl(b.id, 'cpu', 'var(--success)') + '</div></td><td>' + MA.bar(ru) + ' ' + ru.toFixed(0) + '%<div class="sl-cell">' + sl(b.id, 'ram', 'var(--warning)') + '</div></td><td class="col-right">' + a + '/' + mr + '</td><td class="col-right">' + (rps > 0 ? rps.toFixed(1) : '-') + '<div class="sl-cell">' + sl(b.id, 'rps', 'var(--info)') + '</div></td><td class="col-right">' + avgRT + '<div class="sl-cell">' + sl(b.id, 'avgRt', 'var(--text-secondary)') + '</div></td><td class="col-right">' + reqCap + '</td><td class="col-right">' + sc + '</td><td style="font-size:11px">' + loadingCell + '</td><td>' + modelsCell + '</td><td class="col-right">' + up + '</td></tr>';
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