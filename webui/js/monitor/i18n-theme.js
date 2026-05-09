// js/monitor/i18n-theme.js — i18n text updates and theme synchronization

(function() {
  'use strict';

  var MA = window.MonitorApp;
  var T = MA.T;

  function syncTheme() {
    try {
      var st = localStorage.getItem('ollamalegion_theme');
      if (st) { document.documentElement.setAttribute('data-theme', st); applyMT(st); }
      if (window.parent && window.parent !== window) {
        var pt = window.parent.document.documentElement.getAttribute('data-theme');
        if (pt) { document.documentElement.setAttribute('data-theme', pt); applyMT(pt); }
        var pl = window.parent.document.documentElement.getAttribute('lang');
        if (pl && MA.LANG() !== pl) { MA.SETLANG(pl); document.documentElement.setAttribute('lang', pl); }
      }
    } catch(e) {}
  }

  function applyMT(t) {
    document.documentElement.setAttribute('data-theme', t);
    var ov = document.querySelectorAll('.overlay'), sb = document.querySelectorAll('.stats-bar');
    if (t === 'light') {
      ov.forEach(function(e) { e.style.background = 'rgba(241,245,249,0.92)'; });
      sb.forEach(function(e) { e.style.background = 'rgba(255,255,255,0.92)'; });
    } else {
      ov.forEach(function(e) { e.style.background = 'rgba(11,17,32,0.92)'; });
      sb.forEach(function(e) { e.style.background = 'rgba(17,24,39,0.92)'; });
    }
    if (typeof window.drawTopo === 'function') window.drawTopo();
  }

  function updateMonitorTexts() {
    var t = T;
    document.querySelectorAll('.stat-label').forEach(function(el) {
      var mp = {
        Backends: t('monitor.stats.backends'), Active: t('monitor.stats.active'),
        Pending: t('monitor.stats.pending'), Processing: t('monitor.stats.processing'),
        Sessions: t('monitor.stats.sessions'), 'Req/s': t('monitor.stats.rate'),
        'Cluster RPS': t('monitor.stats.clusterRps'), VRAM: t('monitor.stats.vram'),
        Models: t('monitor.stats.models')
      };
      var tx = el.textContent.trim();
      if (mp[tx]) el.textContent = mp[tx];
    });

    var hm = {
      '📊 Ресурсы кластера': t('monitor.panel.clusterResources'),
      '🧠 Модели в памяти': t('monitor.panel.modelsInMemory'),
      '🖥️ Backends': t('monitor.panel.backends'),
      '📋 Queue': t('monitor.panel.queue'),
      '👤 Sessions': t('monitor.panel.sessions')
    };
    document.querySelectorAll('.panel-header span:first-child').forEach(function(el) {
      var tx = el.textContent.replace(/\\s*\\(\\d+\\)\\s*$/, '').trim();
      var b = hm[tx];
      if (b) {
        if (el.childNodes[0] && el.childNodes[0].nodeType === 3) el.childNodes[0].textContent = b + ' ';
        else el.textContent = b;
      }
    });

    var thm = {
      'Модель': t('monitor.table.model'), 'Бэкенды': t('monitor.table.backends'),
      'Сессий': t('monitor.table.sessionsCount'), VRAM: t('monitor.table.vram'),
      'Загрузка': t('monitor.table.load'), ID: t('monitor.table.id'),
      'Статус': t('monitor.table.status'), GPU: t('monitor.table.gpu'),
      CPU: t('monitor.table.cpu'), RAM: t('monitor.table.ram'),
      Active: t('monitor.table.active'), RPS: t('monitor.table.rps'),
      Score: t('monitor.table.score'), 'Модели': t('monitor.table.models'),
      Uptime: t('monitor.table.uptime'), Target: t('monitor.table.target'),
      'Ожидание': t('monitor.table.wait'), 'Сессия': t('monitor.table.session'),
      Backend: t('monitor.table.backend'), 'Запросов': t('monitor.table.requests'),
      Idle: t('monitor.table.idle'), IP: t('monitor.table.ip'),
      'Клиент': t('monitor.table.client')
    };
    document.querySelectorAll('thead th').forEach(function(th) {
      var tx = th.textContent.trim();
      if (thm[tx]) th.textContent = thm[tx];
    });

    var pb = document.getElementById('pauseBtn');
    if (pb) pb.textContent = MA.paused ? '▶ ' + t('monitor.header.continue') : '⏸ ' + t('monitor.header.pause');
    var db = document.getElementById('demoBtn');
    if (db) db.textContent = '▶ ' + t('monitor.header.demo');
    var cb = document.getElementById('connBadge');
    if (cb && cb.textContent === 'Подключение…') cb.textContent = t('monitor.header.connecting');
    var lo = document.querySelector('#loadingOverlay h2');
    if (lo) lo.textContent = t('monitor.overlay.connecting');
    var eo = document.querySelector('#errorOverlay h2');
    if (eo) eo.textContent = t('monitor.overlay.noConnection');
    var et = document.getElementById('errorText');
    if (et && et.textContent.indexOf('Не удалось') >= 0) et.textContent = t('monitor.overlay.checkConnection');
    var rb = document.querySelector('#errorOverlay button:first-of-type');
    if (rb) rb.textContent = '🔄 ' + t('monitor.overlay.retry');
    var dob = document.querySelector('#errorOverlay button:last-of-type');
    if (dob) dob.textContent = '▶ ' + t('monitor.overlay.demoMode');
    var di = document.getElementById('demoIndicator');
    if (di && MA.demoMode) di.textContent = t('monitor.header.demoIndicator');
    var cm = {
      GPU: t('metrics.gpuUtil'), VRAM: t('metrics.vramUsed'), CPU: t('metrics.cpuUsed'), RAM: t('metrics.ramUsed'),
      'Свободные слоты': t('monitor.capacity.freeSlots'), 'VRAM по бэкендам': t('monitor.capacity.vramPerBackend'),
      'Free Slots': t('monitor.capacity.freeSlots'), 'VRAM per Backend': t('monitor.capacity.vramPerBackend')
    };
    document.querySelectorAll('.capacity-card h4').forEach(function(el) {
      var tx = el.textContent.trim();
      if (cm[tx]) el.textContent = cm[tx];
    });
    var lop = document.querySelector('#loadingOverlay p');
    if (lop) lop.innerHTML = t('monitor.overlay.waitingDesc').replace('loadbalancer', '<code>loadbalancer</code>');
  }

  window.updateMonitorTexts = updateMonitorTexts;
  window.applyMT = applyMT;
  window.addEventListener('i18n:changed', function(e) {
    document.documentElement.setAttribute('lang', e.detail.lang);
    updateMonitorTexts();
  });
  window.addEventListener('storage', function(e) {
    if (e.key === 'ollamalegion_theme') applyMT(e.newValue || 'dark');
    if (e.key === 'ollamalegion_lang' && MA.LANG() !== e.newValue) MA.SETLANG(e.newValue);
  });

  syncTheme();
})();