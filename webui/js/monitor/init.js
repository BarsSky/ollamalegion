// js/monitor/init.js — Initialization, rebalance, animation loop

(function() {
  'use strict';

  var MA = window.MonitorApp;

  // Save AutoPull config
  window.saveAutoPullConfig = function() {
    if (MA.demoMode) { alert(MA.T('monitor.rebalance.disabled')); return; }
    var enabled = document.getElementById('autoPullEnabled').checked;
    var maxConcurrent = parseInt(document.getElementById('autoPullMaxConcurrent').value) || 2;
    var pullTimeout = document.getElementById('autoPullPullTimeout').value || '120s';
    var retryCount = parseInt(document.getElementById('autoPullRetryCount').value) || 1;
    // Validate
    if (maxConcurrent < 1) maxConcurrent = 1;
    if (maxConcurrent > 10) maxConcurrent = 10;
    if (retryCount < 0) retryCount = 0;
    if (retryCount > 5) retryCount = 5;
    var cfg = { enabled: enabled, maxConcurrent: maxConcurrent, pullTimeout: pullTimeout, retryCount: retryCount };
    var statusEl = document.getElementById('autoPullSaveStatus');
    if (statusEl) statusEl.style.display = 'none';
    if (typeof window.updateAutoPullConfig === 'function') {
      window.updateAutoPullConfig(cfg).then(function() {
        if (statusEl) {
          statusEl.textContent = MA.T('monitor.autoPull.saved');
          statusEl.style.display = 'inline';
          setTimeout(function() { if (statusEl) statusEl.style.display = 'none'; }, 3000);
        }
        if (typeof window.fetchAll === 'function') window.fetchAll();
      }).catch(function(e) {
        console.error('[monitor] save autopull config failed:', e);
        if (statusEl) {
          statusEl.textContent = '✗ ' + MA.T('monitor.common.error') + ': ' + (e.message || 'Unknown');
          statusEl.style.color = 'var(--danger)';
          statusEl.style.display = 'inline';
          setTimeout(function() { if (statusEl) { statusEl.style.display = 'none'; statusEl.style.color = 'var(--success)'; } }, 5000);
        }
      });
    }
  };

  // AutoPull toggle listener
  document.addEventListener('change', function(e) {
    if (e.target && e.target.id === 'autoPullEnabled') {
      var label = document.getElementById('autoPullEnabledLabel');
      if (label) label.textContent = e.target.checked
        ? (MA.T('monitor.autoPull.on') || 'Вкл')
        : (MA.T('monitor.autoPull.off') || 'Выкл');
    }
  });

  // Rebalance
  //
  // R66c (2026-09-22): кнопка дёргала GET /api/v1/queue/rebalance — такого
  // эндпоинта в балансере НЕТ (есть только /api/v1/queue/stats|details|history),
  // поэтому каждое нажатие давало 404 и alert с ошибкой, а сама балансировка
  // не запускалась. Теперь используется реальный механизм R59:
  //   GET  /api/v1/admin/cluster/autosuggest        — предложения по перекладке
  //   POST /api/v1/admin/cluster/autosuggest/apply  — применить их
  // (см. internal/api/handlers_autosuggest.go).
  function _showRebalanceBanner(text, ok) {
    var c = document.getElementById('alertContainer');
    if (!c) return;
    var b = document.createElement('div');
    b.className = 'alert-banner';
    b.style.cssText = ok
      ? 'background:rgba(34,197,94,0.08);border:1px solid rgba(34,197,94,0.25);color:var(--success)'
      : 'background:rgba(234,179,8,0.08);border:1px solid rgba(234,179,8,0.25);color:var(--warning)';
    b.innerHTML = '<span>' + (ok ? '✅' : 'ℹ️') + '</span><span></span>';
    b.lastChild.textContent = text; // textContent — без HTML-инъекций из ответа
    c.insertBefore(b, c.firstChild);
    setTimeout(function() { if (b.parentNode) b.remove(); }, 6000);
  }

  window.forceRebalance = function() {
    if (MA.demoMode) { alert(MA.T('monitor.rebalance.disabled')); return; }
    var token = new URLSearchParams(location.search).get('token') || localStorage.getItem('apiToken') || '';
    var ctrl = new AbortController();
    var tid = setTimeout(function() { ctrl.abort(); }, 15000);
    var headers = token ? { 'X-API-Token': token } : {};

    fetch(MA.API_BASE + '/api/v1/admin/cluster/autosuggest', {
      headers: headers,
      signal: ctrl.signal
    }).then(function(r) {
      if (!r.ok) throw new Error('autosuggest ' + r.status + ' ' + r.statusText);
      return r.json();
    }).then(function(res) {
      var suggestions = (res && res.suggestions) || [];
      var ids = suggestions.map(function(s) { return s && s.id; }).filter(Boolean);
      if (!ids.length) {
        clearTimeout(tid);
        _showRebalanceBanner(MA.T('monitor.rebalance.nothing') || 'Балансировка не требуется: предложений нет', false);
        return null;
      }
      return fetch(MA.API_BASE + '/api/v1/admin/cluster/autosuggest/apply', {
        method: 'POST',
        headers: Object.assign({ 'Content-Type': 'application/json' }, headers),
        body: JSON.stringify({ suggestion_ids: ids }),
        signal: ctrl.signal
      }).then(function(r2) {
        if (!r2.ok) throw new Error('apply ' + r2.status + ' ' + r2.statusText);
        return r2.json().then(function(body) {
          return { applied: ids.length, body: body };
        });
      });
    }).then(function(out) {
      clearTimeout(tid);
      if (!out) return;
      console.log('[monitor] rebalance applied:', out);
      _showRebalanceBanner(
        (MA.T('monitor.rebalance.success') || 'Rebalance started') + ' (' + out.applied + ')',
        true
      );
      if (typeof window.fetchAll === 'function') window.fetchAll();
    }).catch(function(e) {
      clearTimeout(tid);
      console.error('[monitor] rebalance failed:', e);
      alert(MA.T('monitor.rebalance.error') + e.message);
    });
  };

  // Panel toggle
  window.togglePanel = function(id) {
    document.getElementById(id).classList.toggle('open');
  };

  // Help modal
  window.toggleHelp = function() {
    var m = document.getElementById('helpModal');
    if (!m) return;
    m.style.display = (m.style.display === 'flex' || m.style.display === '') ? 'none' : 'flex';
  };

  // Animation loop
  function anim() {
    if (typeof window.drawTopo === 'function') window.drawTopo();
    if (typeof window.drawConveyor === 'function') window.drawConveyor();
    requestAnimationFrame(anim);
  }

  // INIT
  document.documentElement.setAttribute('lang', MA.LANG());
  if (typeof window.updateMonitorTexts === 'function') window.updateMonitorTexts();
  setTimeout(function() { if (typeof window.updateMonitorTexts === 'function') window.updateMonitorTexts(); }, 100);
  setTimeout(function() { if (typeof window.updateMonitorTexts === 'function') window.updateMonitorTexts(); }, 500);

  // Round 18f: render backend-type switcher IMMEDIATELY at init (не ждём первого fetch).
  // Раньше renderBackendTypeSwitcher() вызывался только из updateUI(), который
  // стартует после fetchAll(). Если fetch падает или долго идёт — switcher пустой.
  // Также рендерим All/Ollama/llama.cpp — раньше было только Ollama/llama.cpp.
  if (typeof window.renderBackendTypeSwitcher === 'function') {
    try { window.renderBackendTypeSwitcher(); } catch (e) { console.warn('[init] switcher:', e); }
  }

  MA.timerId = setInterval(window.fetchAllSafe, MA.refreshInterval);
  // A.2: отдельный sparkline-поллер (5s) — не нагружает основной 2s цикл UI.
  if (window.Sparkline && typeof window.Sparkline.startPoller === 'function') {
    window.Sparkline.startPoller();
  }
  anim();
  if (typeof window.fetchAll === 'function') window.fetchAll();
})();