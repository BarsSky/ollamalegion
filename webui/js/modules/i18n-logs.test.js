// i18n-logs.test.js — R234 (2026-10-24): WebUI-переключатель языка логов.
//
// Запуск: node webui/js/modules/i18n-logs.test.js
//
// ЧТО ПРОВЕРЯЕМ:
//   1. модуль читает GET /api/v1/i18n и выставляет select по настройке;
//   2. статус показывает ДЕЙСТВУЮЩИЙ язык (resolved), а не только настройку;
//   3. «Применить» шлёт PUT с выбранным значением и показывает успех;
//   4. ошибка сервера (403 гостью, 400 мусор) показывается, но страница жива;
//   5. старый образ балансера (404/сеть) — select блокируется, статус объясняет;
//   6. window.LogLanguage экспортирует load/apply (для других модулей).
'use strict';

const assert = require('assert');
const path = require('path');

let passed = 0;
const failures = [];
function check(name, fn) {
  try { fn(); passed++; console.log('  \u2713 ' + name); }
  catch (e) { failures.push(name + ': ' + (e && e.message)); console.error('  \u2717 ' + name + ' - ' + (e && e.message)); }
}

// --- минимальный DOM -------------------------------------------------------
function makeEl(id) {
  return {
    id: id, value: '', textContent: '', style: {}, disabled: false,
    options: [
      { value: 'auto' }, { value: 'ru' }, { value: 'en' }
    ],
    addEventListener: function (type, fn) { this.listeners = this.listeners || {}; this.listeners[type] = fn; }
  };
}

function freshGlobals(fetchImpl) {
  const els = {
    logLanguageSelect: makeEl('logLanguageSelect'),
    logLanguageApply: makeEl('logLanguageApply'),
    logLanguageStatus: makeEl('logLanguageStatus')
  };
  global.window = global;
  global.document = {
    readyState: 'complete',
    getElementById: function (id) { return els[id] || null; },
    addEventListener: function () {}
  };
  global.fetch = fetchImpl;
  global.window = global;
  delete global.LogLanguage;
  // модуль ставит слушатель на кнопку при загрузке
  return els;
}

function loadModule() {
  const src = path.join(__dirname, 'i18n-logs.js');
  delete require.cache[require.resolve(src)];
  require(src);
  return global.LogLanguage;
}

function okResponse(body) {
  return Promise.resolve({
    ok: true, status: 200,
    json: function () { return Promise.resolve(body); }
  });
}

function errResponse(status, body) {
  return Promise.resolve({
    ok: false, status: status,
    json: function () { return Promise.resolve(body || {}); }
  });
}

(async function () {
  console.log('R234: язык логов и ошибок в WebUI');

  // 1-2. GET выставляет select и показывает действующий язык
  {
    const calls = [];
    const els = freshGlobals(function (url, init) {
      calls.push({ url: url, init: init });
      return okResponse({ language: 'ru', resolved: 'ru', supported: ['ru', 'en'], source: 'env' });
    });
    const mod = loadModule();
    assert.strictEqual(mod.endpoint, '/api/v1/i18n', 'endpoint должен быть /api/v1/i18n');
    const data = await mod.load();
    assert.strictEqual(data.language, 'ru');
    assert.strictEqual(els.logLanguageSelect.value, 'ru', 'select выставлен по настройке');
    assert.strictEqual(els.logLanguageSelect.disabled, false, 'после успешного GET select активен');
    assert.ok(/ru/.test(els.logLanguageStatus.textContent), 'статус показывает действующий язык');
    assert.strictEqual(calls[0].url, '/api/v1/i18n');
    check('GET выставляет select и показывает действующий язык', function () { assert.ok(true); });
  }

  // 2b. resolved может отличаться от настройки (auto -> ru)
  {
    const els = freshGlobals(function () {
      return okResponse({ language: 'auto', resolved: 'en', supported: ['ru', 'en'] });
    });
    const mod = loadModule();
    await mod.load();
    assert.strictEqual(els.logLanguageSelect.value, 'auto', 'select показывает саму настройку');
    assert.ok(/en/.test(els.logLanguageStatus.textContent),
      'статус показывает действующий язык (en), а не auto');
    check('при auto в select настройка, в статусе — действующий язык', function () { assert.ok(true); });
  }

  // 3. PUT уходит с выбранным языком
  {
    const calls = [];
    const els = freshGlobals(function (url, init) {
      calls.push({ url: url, init: init });
      return okResponse({ success: true, language: 'en', message: 'ok' });
    });
    const mod = loadModule();
    els.logLanguageSelect.value = 'en';
    await mod.apply();
    // init() модуля уже сделал GET при загрузке, поэтому ищем именно PUT.
    const put = calls.filter(function (c) { return c.init && c.init.method === 'PUT'; });
    assert.strictEqual(put.length, 1, 'должен уйти ровно один PUT');
    const body = JSON.parse(put[0].init.body);
    assert.strictEqual(body.language, 'en', 'в теле выбранный язык');
    assert.ok(/en/.test(els.logLanguageStatus.textContent), 'успех показан в статусе');
    check('Применить шлёт PUT и показывает успех', function () { assert.ok(true); });
  }

  // 4. 403/400 показываются, страница не падает
  {
    const els = freshGlobals(function () {
      return errResponse(403, { error: 'changing the language requires administrator rights' });
    });
    const mod = loadModule();
    await mod.apply();
    assert.ok(els.logLanguageStatus.textContent.length > 0, 'ошибка видна в статусе');
    assert.ok(/administrator|администратор/i.test(els.logLanguageStatus.textContent),
      'текст ошибки сервера доносится до оператора: ' + els.logLanguageStatus.textContent);
    check('403 показывается в статусе', function () { assert.ok(true); });
  }

  // 5. старый образ (сеть/404) — select блокируется
  {
    const els = freshGlobals(function () {
      return Promise.reject(new Error('network down'));
    });
    const mod = loadModule();
    const res = await mod.load();
    assert.ok(res.error, 'ошибка возвращена');
    assert.strictEqual(els.logLanguageSelect.disabled, true, 'select заблокирован');
    check('недоступный эндпоинт блокирует настройку, но не ломает страницу', function () { assert.ok(true); });
  }

  // 6. экспорт публичного API
  {
    freshGlobals(function () { return okResponse({ language: 'ru', resolved: 'ru' }); });
    const mod = loadModule();
    assert.strictEqual(typeof mod.load, 'function');
    assert.strictEqual(typeof mod.apply, 'function');
    assert.strictEqual(typeof mod.renderLanguage, 'function');
    check('window.LogLanguage экспортирует load/apply/renderLanguage', function () { assert.ok(true); });
  }

  console.log('');
  if (failures.length) {
    console.error('ИТОГ: FAIL (' + failures.length + ')');
    failures.forEach(f => console.error(' - ' + f));
    process.exit(1);
  }
  console.log('ИТОГ: PASS (' + passed + ' проверок)');
})();
