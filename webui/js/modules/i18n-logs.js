// i18n-logs.js — R234 (2026-10-24): язык логов и ошибок в WebUI.
//
// ЗАЧЕМ. Язык интерфейса (langSelect) и язык, на котором пишут Go-процессы
// (балансер, cppworker, agent), — РАЗНЫЕ вещи: первый живёт в localStorage
// браузера, второй задаётся на стенде и применяется к логам и API-ошибкам.
// До R234 переключить второй из панели было нельзя, только правкой env и
// перезапуском контейнеров.
//
// КОНТРАКТ БЭКЕНДА.
//   GET  /api/v1/i18n -> {language:"ru|en", resolved:"ru|en", supported:[...], source}
//   PUT  /api/v1/i18n <- {language:"ru|en|auto"}   (только администратор)
//
// МОДУЛЬ НЕ ЛОМАЕТ СТРАНИЦУ. Если эндпоинта нет (старый образ балансера) или
// у гостя нет прав, select/кнопка просто не активируются, а статус объясняет
// причину — панель продолжает работать.
(function () {
  'use strict';

  var ENDPOINT = '/api/v1/i18n';

  function $(id) { return typeof document !== 'undefined' ? document.getElementById(id) : null; }

  function t(key, fallback) {
    try {
      if (typeof window !== 'undefined' && window.I18N && typeof window.I18N.t === 'function') {
        var v = window.I18N.t(key);
        if (v && v !== key) { return v; }
      }
    } catch (e) { /* i18n недоступен — отдаём английский фолбэк */ }
    return fallback;
  }

  function setStatus(text, kind) {
    var el = $('logLanguageStatus');
    if (!el) { return; }
    el.textContent = text || '';
    el.style.color = kind === 'error' ? 'var(--danger, #ef4444)'
      : kind === 'ok' ? 'var(--success, #22c55e)' : 'var(--text-muted)';
  }

  function setEnabled(on) {
    var sel = $('logLanguageSelect');
    var btn = $('logLanguageApply');
    if (sel) { sel.disabled = !on; }
    if (btn) { btn.disabled = !on; }
  }

  // renderLanguage — выставляет select по действующей настройке.
  // В select подставляем отправленное значение (может быть auto), а в статусе
  // показываем ДЕЙСТВУЮЩИЙ язык: оператору важно, на каком языке пишутся логи.
  function renderLanguage(data) {
    var sel = $('logLanguageSelect');
    var setting = (data && data.language) || '';
    var resolved = (data && data.resolved) || setting;
    if (sel && setting) {
      var has = false;
      for (var i = 0; i < sel.options.length; i++) {
        if (sel.options[i].value === setting) { has = true; break; }
      }
      if (has) { sel.value = setting; }
    }
    setStatus(t('settings.log_language.current', 'Current: %s').replace('%s', resolved));
  }

  function load() {
    if (typeof fetch !== 'function') { return Promise.resolve(null); }
    return fetch(ENDPOINT, { headers: { Accept: 'application/json' } })
      .then(function (r) {
        if (!r.ok) { throw new Error('HTTP ' + r.status); }
        return r.json();
      })
      .then(function (data) {
        renderLanguage(data);
        setEnabled(true);
        return data;
      })
      .catch(function (err) {
        setEnabled(false);
        setStatus(t('settings.log_language.readonly', 'Only an administrator can change the log language'),
          'error');
        return { error: err && err.message ? err.message : String(err) };
      });
  }

  function apply() {
    var sel = $('logLanguageSelect');
    if (!sel) { return Promise.resolve(null); }
    if (typeof fetch !== 'function') { return Promise.resolve(null); }
    setStatus('');
    return fetch(ENDPOINT, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
      body: JSON.stringify({ language: sel.value })
    })
      .then(function (r) {
        return r.json().catch(function () { return {}; })
          .then(function (body) { return { ok: r.ok, status: r.status, body: body }; });
      })
      .then(function (res) {
        if (!res.ok) {
          var msg = (res.body && (res.body.error || res.body.message)) || ('HTTP ' + res.status);
          setStatus(t('settings.log_language.failed', 'Failed to save the log language: %s')
            .replace('%s', msg), 'error');
          return res;
        }
        if (res.body && res.body.warning) {
          setStatus(res.body.warning, 'error');
        } else {
          var lang = (res.body && res.body.language) || sel.value;
          setStatus(t('settings.log_language.saved', 'Log language saved: %s').replace('%s', lang), 'ok');
        }
        return res;
      })
      .catch(function (err) {
        setStatus(t('settings.log_language.failed', 'Failed to save the log language: %s')
          .replace('%s', err && err.message ? err.message : String(err)), 'error');
        return { error: err };
      });
  }

  function init() {
    var btn = $('logLanguageApply');
    if (btn && btn.addEventListener) { btn.addEventListener('click', apply); }
    // Select показываем сразу как «недоступен»: включается после успешного GET,
    // чтобы гость не думал, что настройка ему доступна.
    setEnabled(false);
    load();
  }

  if (typeof document !== 'undefined') {
    if (document.readyState === 'loading') {
      document.addEventListener('DOMContentLoaded', init);
    } else {
      init();
    }
  }

  var LogLanguage = { load: load, apply: apply, renderLanguage: renderLanguage, endpoint: ENDPOINT };
  try { window.LogLanguage = LogLanguage; } catch (e) { /* нет window (тесты) */ }
})();
