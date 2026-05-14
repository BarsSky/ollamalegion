// js/monitor/state.js — Global state and configuration for monitor

(function() {
  'use strict';

  window.MonitorApp = window.MonitorApp || {};

  // i18n helpers
  MonitorApp.T = function(k, p) {
    return (window.I18N && window.I18N.t ? window.I18N.t(k, p) : k);
  };
  MonitorApp.LANG = function() {
    return (window.I18N && window.I18N.getLang ? window.I18N.getLang() : 'ru');
  };
  MonitorApp.SETLANG = function(l) {
    if (window.I18N && window.I18N.setLang) window.I18N.setLang(l);
  };

  // API configuration
  function detectApiBase() {
    if (typeof window !== 'undefined' && window.WEBUI_CONFIG && window.WEBUI_CONFIG.apiBase) {
      return window.WEBUI_CONFIG.apiBase.replace(/\/$/, '');
    }
    var s = localStorage.getItem('monitorApiBase');
    if (s) return s.replace(/\/$/, '');
    var q = new URLSearchParams(location.search).get('api_base');
    if (q) return q.replace(/\/$/, '');
    return '';
  }

  MonitorApp.API_BASE = detectApiBase();
  // Update display if element exists
  var abd = document.getElementById('apiBaseDisplay');
  if (abd) abd.textContent = MonitorApp.API_BASE || location.origin;
  MonitorApp.refreshInterval = 2000;
  MonitorApp.timerId = null;
  MonitorApp.paused = false;
  MonitorApp.demoMode = false;
  MonitorApp.fetchAttempt = 0;
  MonitorApp.lastData = null;
  MonitorApp.lastTime = Date.now();
  MonitorApp.lastTotalRequests = 0;
  MonitorApp.requestRate = 0;
  MonitorApp.fetching = false;
  MonitorApp.corsErrorDetected = false;

  // Stable backend ordering
  MonitorApp._backendOrder = new Map();
  MonitorApp.stableBackendOrder = function(backends) {
    var now = Date.now(), seen = new Set();
    backends.forEach(function(b) {
      var id = b.id || b.ID;
      if (!id) return;
      seen.add(id);
      if (!MonitorApp._backendOrder.has(id)) {
        MonitorApp._backendOrder.set(id, { index: MonitorApp._backendOrder.size, addedAt: now });
      }
    });
    var entries = [];
    MonitorApp._backendOrder.forEach(function(m, id) {
      if (seen.has(id)) entries.push({ id: id, index: m.index, addedAt: m.addedAt });
    });
    entries.sort(function(a, b) { return a.index - b.index; });
    MonitorApp._backendOrder.clear();
    entries.forEach(function(e, i) {
      MonitorApp._backendOrder.set(e.id, { index: i, addedAt: e.addedAt });
    });
    return [].concat(backends).sort(function(a, b) {
      var ia = (MonitorApp._backendOrder.get(a.id || a.ID) || { index: Infinity }).index;
      var ib = (MonitorApp._backendOrder.get(b.id || b.ID) || { index: Infinity }).index;
      return ia - ib;
    });
  };

  // Canvas state
  MonitorApp.topo = { backends: [], sessions: [], queue: {}, w: 0, h: 0, recentClients: [], warmingUpModels: [] };
  MonitorApp.particles = [];
  MonitorApp.convQueue = [];

  // Viewport transform for pan & zoom
  MonitorApp.viewport = {
    offsetX: 0,
    offsetY: 0,
    scale: 1,
    minScale: 0.1,
    maxScale: 3.0
  };
  MonitorApp.toWorld = function(screenX, screenY) {
    return {
      x: (screenX - MonitorApp.viewport.offsetX) / MonitorApp.viewport.scale,
      y: (screenY - MonitorApp.viewport.offsetY) / MonitorApp.viewport.scale
    };
  };
  MonitorApp.toScreen = function(worldX, worldY) {
    return {
      x: worldX * MonitorApp.viewport.scale + MonitorApp.viewport.offsetX,
      y: worldY * MonitorApp.viewport.scale + MonitorApp.viewport.offsetY
    };
  };
  MonitorApp.resetViewport = function() {
    MonitorApp.viewport.offsetX = 0;
    MonitorApp.viewport.offsetY = 0;
    MonitorApp.viewport.scale = 1;
  };

  // Common geometry constants shared between topology and conveyor
  MonitorApp.GEOM = {
    bw: 180,          // balancer width
    bh: 100,          // balancer height
    clientX: 140,     // client connector x
    backendMargin: 140, // backend right margin
    minSpacing: 30,   // min vertical spacing between nodes
    marginY: 80       // top/bottom margin for node placement
  };
  MonitorApp.GEOM.backendX = function(w) { return w - MonitorApp.GEOM.backendMargin; };
  MonitorApp.GEOM.balInX = function(w) { return w / 2 - MonitorApp.GEOM.bw / 2; };
  MonitorApp.GEOM.balOutX = function(w) { return w / 2 + MonitorApp.GEOM.bw / 2; };
  MonitorApp.GEOM.balCenterX = function(w) { return w / 2; };
  MonitorApp.GEOM.balCenterY = function(h) { return h / 2; };

  // Vertical node placement (sessions on left, backends on right)
  MonitorApp.nY = function(type, i) {
    var cnt = type === 'session'
      ? Math.max(1, MonitorApp.topo.sessions.length)
      : Math.max(1, MonitorApp.topo.backends.length);
    var av = MonitorApp.topo.h - MonitorApp.GEOM.marginY * 2;
    var st = Math.max(MonitorApp.GEOM.minSpacing, av / Math.max(1, cnt - 1));
    var totalHeight = (cnt - 1) * st;
    var offset = Math.max(0, (av - totalHeight) / 2);
    return MonitorApp.GEOM.marginY + offset + i * st;
  };

  // Common utilities
  MonitorApp.esc = function(s) {
    if (s == null) return '';
    return String(s).replace(/[&<>"']/g, function(c) {
      return { '&': '&', '<': '<', '>': '>', '"': '"', "'": '&#39;' }[c];
    });
  };

  MonitorApp.fmtDur = function(ms) {
    if (ms < 0) ms = 0;
    var s = Math.floor(ms / 1000);
    if (s < 60) return s + 's';
    var m = Math.floor(s / 60);
    if (m < 60) return m + 'm ' + (s % 60) + 's';
    var h = Math.floor(m / 60);
    return h + 'h ' + (m % 60) + 'm';
  };

  MonitorApp.bar = function(v) {
    var x = Math.max(0, Math.min(100, v || 0)), c = 'var(--success)';
    if (x > 60) c = 'var(--warning)';
    if (x > 85) c = 'var(--danger)';
    return '<span class="bar-track"><span class="bar-fill" style="width:' + x + '%;background:' + c + '"></span></span>';
  };

  MonitorApp.ipColor = function(ip) {
    if (!ip) return 'var(--text-secondary)';
    var h = 0, i, c;
    for (i = 0; i < ip.length; i++) { c = ip.charCodeAt(i); h = ((h << 5) - h) + c; h |= 0; }
    h = Math.abs(h);
    var hue = (h % 12) * 30;
    return 'hsl(' + hue + ', 60%, 60%)';
  };

  MonitorApp.isCloudModel = function(name) {
    return name && name.indexOf(':cloud') >= 0;
  };

  MonitorApp.isTechnicalClient = function(cn, ua) {
    if (!cn) return false;
    var l = cn.toLowerCase();
    if (l.indexOf('monitor') >= 0) return true;
    if (l.indexOf('healthcheck') >= 0 || l.indexOf('health') >= 0) return true;
    if (l.indexOf('kube-probe') >= 0 || l.indexOf('kube') >= 0) return true;
    if (l.indexOf('prometheus') >= 0) return true;
    if (l.indexOf('ollamalegion') >= 0 && l.indexOf('agent') >= 0) return true;
    return false;
  };

  MonitorApp.getCI = function(n) {
    if (!n) return '👤';
    var l = n.toLowerCase();
    if (l.indexOf('cline') >= 0) return '🦾';
    if (l.indexOf('openwebui') >= 0 || l.indexOf('open-webui') >= 0) return '🌐';
    if (l.indexOf('curl') >= 0) return '📡';
    if (l.indexOf('python') >= 0 || l.indexOf('requests') >= 0) return '🐍';
    if (l.indexOf('node') >= 0 || l.indexOf('axios') >= 0) return '🟢';
    return '👤';
  };

  // Canvas helpers
  MonitorApp.vF = function() { return 'system-ui,-apple-system,sans-serif'; };
  MonitorApp.trunc = function(s, l) { if (!s) return ''; return s.length > l ? s.slice(0, l) + '…' : s; };
  MonitorApp.rr = function(ctx, x, y, w, h, r) {
    ctx.beginPath();
    ctx.moveTo(x + r, y);
    ctx.arcTo(x + w, y, x + w, y + h, r);
    ctx.arcTo(x + w, y + h, x, y + h, r);
    ctx.arcTo(x, y + h, x, y, r);
    ctx.arcTo(x, y, x + w, y, r);
    ctx.closePath();
  };

  // Message listener for config updates
  window.addEventListener('message', function(e) {
    if (e.data && e.data.type === 'ollamalegion-config') {
      if (e.data.apiBase !== undefined) MonitorApp.API_BASE = e.data.apiBase;
      if (e.data.apiToken !== undefined) localStorage.setItem('apiToken', e.data.apiToken);
      if (e.data.lang) {
        MonitorApp.SETLANG(e.data.lang);
        document.documentElement.setAttribute('lang', e.data.lang);
        if (typeof window.updateMonitorTexts === 'function') window.updateMonitorTexts();
      }
      if (e.data.refreshInterval && !MonitorApp.demoMode) {
        MonitorApp.refreshInterval = e.data.refreshInterval;
        if (MonitorApp.timerId) {
          clearInterval(MonitorApp.timerId);
          MonitorApp.timerId = setInterval(window.fetchAllSafe, MonitorApp.refreshInterval);
        }
      }
    }
  });
})();