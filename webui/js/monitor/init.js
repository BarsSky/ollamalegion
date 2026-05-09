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
          statusEl.textContent = '✓ Saved';
          statusEl.style.display = 'inline';
          setTimeout(function() { if (statusEl) statusEl.style.display = 'none'; }, 3000);
        }
        if (typeof window.fetchAll === 'function') window.fetchAll();
      }).catch(function(e) {
        console.error('[monitor] save autopull config failed:', e);
        if (statusEl) {
          statusEl.textContent = '✗ Error: ' + (e.message || 'Unknown');
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
  window.forceRebalance = function() {
    if (MA.demoMode) { alert(MA.T('monitor.rebalance.disabled')); return; }
    var token = new URLSearchParams(location.search).get('token') || localStorage.getItem('apiToken') || '';
    var ctrl = new AbortController();
    var tid = setTimeout(function() { ctrl.abort(); }, 15000);
    fetch(MA.API_BASE + '/api/v1/queue/rebalance', {
      headers: token ? { 'X-API-Token': token } : {},
      signal: ctrl.signal
    }).then(function(r) {
      clearTimeout(tid);
      if (!r.ok) throw new Error(r.status + ' ' + r.statusText);
      return r.json();
    }).then(function(res) {
      console.log('[monitor] rebalance result:', res);
      var c = document.getElementById('alertContainer'), b = document.createElement('div');
      b.className = 'alert-banner';
      b.style.cssText = 'background:rgba(34,197,94,0.08);border:1px solid rgba(34,197,94,0.25);color:var(--success)';
      b.innerHTML = '<span>✅</span><span>' + MA.T('monitor.rebalance.success') + '</span>';
      c.insertBefore(b, c.firstChild);
      setTimeout(function() { if (b.parentNode) b.remove(); }, 6000);
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

  MA.timerId = setInterval(window.fetchAllSafe, MA.refreshInterval);
  anim();
  if (typeof window.fetchAll === 'function') window.fetchAll();
})();