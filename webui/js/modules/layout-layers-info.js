// layout-layers-info.js — R206 (2026-10-21): РАСПРЕДЕЛЕНИЕ СЛОЁВ РАСКЛАДКИ В «БЭКЕНДАХ».
//
// ЖАЛОБА ОПЕРАТОРА: «нигде не видно, если раскладка применяется — на сколько слоёв и у
// каких доноров», просили блок и в «Бэкендах», и в мониторе. В мониторе блок уже есть
// (R204). Здесь — компактная строка над таблицей бэкендов в настройках.
//
// Источник: GET /api/v1/backends → у главного rpcLayout {enabled, servers, degraded};
// затем GET /api/v1/gguf/backends/{id}/proxy/api/models → device_layers_exact
// (CPU/CUDA0/RPC0 c числом слоёв и адресом узла).
(function () {
  'use strict';

  var CFG = window.WEBUI_CONFIG || {};
  function base() { return String(CFG.API_BASE || CFG.apiBase || '').replace(/\/+$/, ''); }
  function tr(key, fallback) {
    var i18n = window.I18N;
    if (i18n && typeof i18n.t === 'function') {
      var s = i18n.t(key);
      if (s && s !== key) { return s; }
    }
    return fallback;
  }

  // text — чистая функция (тестируется в layers-info-r206.test.js).
  function text(deviceLayers, translator) {
    var t = translator || tr;
    var parts = [], local = 0, remote = 0;
    (deviceLayers || []).forEach(function (d) {
      var name = d.device || '?';
      var n = d.count || 0;
      var entry = name + ' ' + n;
      if (name.indexOf('RPC') === 0) { remote += n; if (d.node) { entry += ' (' + d.node + ')'; } }
      else if (name.indexOf('CUDA') === 0) { local += n; }
      parts.push(entry);
    });
    if (!parts.length) { return ''; }
    return t('backends.layers', 'слои раскладки') + ': ' + parts.join(' · ') +
      ' — ' + t('backends.layersLocal', 'локально') + ' ' + local + ' / ' +
      t('backends.layersRemote', 'на узлах') + ' ' + remote;
  }

  function host() {
    return document.getElementById('layoutLayersInfo');
  }

  function render(rows) {
    var el = host();
    if (!el) { return; }
    var line = text(rows);
    if (!line) {
      el.style.display = 'none';
      el.textContent = '';
      return;
    }
    el.style.display = '';
    el.textContent = '🧩 ' + line;
  }

  function load() {
    // R206: до входа не дёргаем API — иначе в консоли 401 у экрана входа
    // (после логина страница перезагружается, и модуль отработает уже с сессией).
    if (window.Auth && typeof window.Auth.state === 'function') {
      var st = window.Auth.state();
      // Пока /auth/me не ответил (state.loading) — молчим: иначе у экрана входа
      // уходит запрос без сессии и даёт 401 в консоли.
      if (st && (st.loading || (st.usersConfigured && !st.authenticated))) { return; }
    }
    fetch(base() + '/api/v1/backends', { credentials: 'same-origin' })
      .then(function (r) { return r.json(); })
      .then(function (data) {
        var list = (data && data.backends) || [];
        var main = null;
        for (var i = 0; i < list.length; i++) {
          var g = list[i] && list[i].rpcLayout;
          if (g && g.enabled && (g.servers || []).length) { main = list[i]; break; }
        }
        if (!main) { render(null); return; }
        return fetch(base() + '/api/v1/gguf/backends/' + encodeURIComponent(main.id) + '/proxy/api/models',
          { credentials: 'same-origin' })
          .then(function (r) { return r.json(); })
          .then(function (models) {
            var arr = (models && models.models) || [];
            for (var j = 0; j < arr.length; j++) {
              var dl = arr[j] && arr[j].device_layers_exact;
              if (dl && dl.length) { render(dl); return; }
            }
            render(null);
          });
      })
      .catch(function () { render(null); });
  }

  function start() {
    if (!host()) { return; }
    var begin = function () { load(); setInterval(load, 10000); };
    if (typeof window.__authReady === 'function') {
      window.__authReady().then(begin).catch(begin);
    } else {
      begin();
    }
  }

  window.LayoutLayersInfo = { start: start, load: load, text: text };

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', start);
  } else {
    start();
  }
})();
