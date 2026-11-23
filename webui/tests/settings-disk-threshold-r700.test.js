#!/usr/bin/env node
/*
 * settings-disk-threshold-r700.test.js — R700 (2026-11-24)
 *
 * ЗАЧЕМ. Порог диска (resources.disk.minFreeMB / LB_DISK_MIN_FREE_MB) исключает
 * узел из выбора, и оператор должен менять его из панели ОСОЗНАННО:
 *   - поле валидируется (целое МБ, 0..1048576), мусор не уходит на сервер;
 *   - рядом с полем написано, ЧТО измеряется (агент делает `df -m /` в своём
 *     контейнере, bind-том /app/models в метрику не входит — освобождение тома
 *     число не меняет, живая жалоба второго стенда);
 *   - рядом видно ТЕКУЩЕЕ значение по узлам (summary.cluster.backends[].system.diskFree)
 *     и кто именно исключён порогом;
 *   - есть «сбросить к умолчанию», и оно сохраняется сразу.
 *
 * ДО R700 этого не было: `parseInt(...) || 10240` молча превращал пустое поле в
 * 10240, а отрицательное число сервер записывал как uint64-переполнение —
 * ответ «success», гейт блокирует ВСЕ узлы, причина нигде не названа.
 *
 * НЕ-ВАКУУМНОСТЬ (проверено вручную, хук ниже):
 *   git show 0b5656b6:webui/js/modules/app-settings.js > /tmp/old-settings.js
 *   APP_SETTINGS_TARGET=/tmp/old-settings.js node webui/tests/settings-disk-threshold-r700.test.js
 *   → КРАСНЕЕТ: старый код отправляет -1 на сервер (PUT происходит) и не рисует
 *   подсказку про измеряемую ФС.
 *
 * ЗАПУСК: node webui/tests/settings-disk-threshold-r700.test.js
 */
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const REPO_ROOT = path.resolve(__dirname, '..', '..');
const MODULES = path.join(REPO_ROOT, 'webui', 'js', 'modules');
const TARGET = process.env.APP_SETTINGS_TARGET
  ? path.resolve(process.env.APP_SETTINGS_TARGET)
  : path.join(MODULES, 'app-settings.js');

let failures = 0;
function check(name, fn) {
  return Promise.resolve()
    .then(fn)
    .then(function () { console.log('  ✓ ' + name); })
    .catch(function (e) {
      failures++;
      console.error('  ✗ ' + name + ' — ' + (e && e.message));
    });
}
function assert(cond, msg) {
  if (!cond) throw new Error(msg);
}

// --- минимальный DOM: только те элементы, которые читает/пишет настройка ----
function makeEl(id, value) {
  const el = {
    id: id,
    value: value === undefined ? '' : value,
    checked: false,
    hidden: false,
    textContent: '',
    innerHTML: '',
    className: '',
    style: {},
    _listeners: {},
    classList: {
      _c: {},
      add(c) { this._c[c] = true; },
      remove(c) { delete this._c[c]; },
      contains(c) { return !!this._c[c]; }
    },
    addEventListener(ev, fn) { this._listeners[ev] = fn; },
    setAttribute() {},
    getAttribute() { return null; }
  };
  return el;
}
const byId = {
  minFreeDisk: makeEl('minFreeDisk', '10240'),
  minFreeDiskHint: makeEl('minFreeDiskHint', ''),
  minFreeDiskError: makeEl('minFreeDiskError', ''),
  minFreeDiskDefault: makeEl('minFreeDiskDefault', ''),
};
global.document = {
  getElementById(id) { return byId[id] || null; },
  querySelector() { return null; },
  querySelectorAll() { return []; },
  createElement(tag) { return makeEl(tag, ''); },
  addEventListener() {},
  removeEventListener() {}
};
global.window = global;
// Utils в бою — глобальный (js/modules/utils.js). Настройкам от него нужен
// только необязательный updateGpuLayersBadge; заглушка повторяет контракт.
global.Utils = {
  setText() {},
  setHTML() {},
  updateGpuLayersBadge() {}
};
global.localStorage = {
  _m: {},
  getItem(k) { return this._m[k] === undefined ? null : this._m[k]; },
  setItem(k, v) { this._m[k] = String(v); },
  removeItem(k) { delete this._m[k]; }
};

// --- i18n: НАСТОЯЩИЙ ru-пакет (проверяем реальные тексты, а не заглушки) ----
vm.runInThisContext(fs.readFileSync(path.join(REPO_ROOT, 'webui', 'js', 'i18n', 'ru.js'), 'utf8'));
const RU = global.window.I18N_RU;
global.window.I18N = {
  t(k, vars) {
    let s = Object.prototype.hasOwnProperty.call(RU, k) ? RU[k] : k;
    if (vars) Object.keys(vars).forEach(function (n) { s = s.split('{' + n + '}').join(String(vars[n])); });
    return s;
  },
  getLang() { return 'ru'; }
};

// --- Api-заглушка: пишет PUT и отдаёт состояние «стенда» --------------------
// Метрики диска — МЕГАБАЙТЫ (types.SystemMetrics.DiskFree: агент делает
// `df -m /`, slot_manager сравнивает значение с minFreeMB напрямую).
const DISK_BACKENDS = [
  { id: 'A10-126', system: { diskTotal: 194403, diskFree: 9480 } },
  { id: 'A10-127', system: { diskTotal: 194403, diskFree: 20000 } },
];
let puts = [];
let serverMinFreeDisk = 10240;
const toasts = [];
global.Api = {
  cluster() { return Promise.resolve({ backends: DISK_BACKENDS }); },
  config() {
    return Promise.resolve({
      operatingMode: 'standard',
      useEnhancedScoring: true,
      modelAffinity: true,
      sessionStickiness: true,
      minFreeDisk: serverMinFreeDisk,
      minFreeDiskDefault: 10240,
      minFreeDiskMax: 1048576,
      // Секции — как в настоящем GET /api/v1/cluster/config: verifySync сверяет
      // именно их, и без них путь сохранения уходил бы в catch (проверка
      // «после сохранения подсказка обновилась» тогда не выполнялась бы).
      modelReplication: { enabled: false, defaultMinInstances: 1, defaultMaxInstances: 3, idleUnloadAfter: '10m' },
      rpcCoordinator: { enabled: false, coordinatorURL: '', workerPort: 18050, protocol: 'http' },
      virtualModels: { enabled: false, coordMode: 'sequential' },
      distInference: { enabled: false, grpcPort: 19000 },
    });
  },
  updateConfig(cfg) {
    puts.push(cfg);
    serverMinFreeDisk = cfg.minFreeDisk;
    return Promise.resolve({ success: true });
  },
  handleError(e) { console.warn('handleError:', e && e.message); }
};

// --- загрузка модуля (с хуком на произвольную ревизию) ----------------------
if (TARGET === path.join(MODULES, 'app-settings.js')) {
  require(TARGET);
} else {
  vm.runInThisContext(fs.readFileSync(TARGET, 'utf8'));
}
assert(typeof global.window.AppSettings === 'function', 'window.AppSettings должен быть фабрикой');

const settings = global.window.AppSettings({
  data: {},
  getCurrentPage() { return 'settings'; },
  refreshPage() {},
  showToast(msg, kind) { toasts.push({ msg: msg, kind: kind }); },
  updateConnectionStatus() {},
  updateBackendEngineBadge() {}
});
assert(settings && typeof settings.saveSettings === 'function', 'saveSettings должен быть экспортирован');

function tick(ms) {
  return new Promise(function (r) { setTimeout(r, ms || 10); });
}
function resetPuts() {
  puts = [];
  toasts.length = 0;
  byId.minFreeDiskError.hidden = true;
  byId.minFreeDiskError.textContent = '';
}

async function run() {
  // Открытие страницы «Настройки»: loadSettings навешивает обработчики (кнопка
  // «по умолчанию», живая подсказка) и заливает серверный конфиг. Именно этот
  // путь проходит оператор — без него проверка ниже тестировала бы не панель,
  // а выдуманный порядок вызовов.
  settings.loadSettings();
  await tick();

  // -------------------------------------------------------------------------
  await check('недопустимое значение не уходит на сервер и объясняется рядом с полем', async function () {
    const bad = ['', '12.5', '-1', '2000000', 'abc'];
    for (const raw of bad) {
      resetPuts();
      byId.minFreeDisk.value = raw;
      await settings.saveSettings(false);
      await tick();
      assert(puts.length === 0, 'значение ' + JSON.stringify(raw) + ' ушло в PUT (' +
        JSON.stringify(puts.map(function (p) { return p.minFreeDisk; })) + ') — сервер запишет мусор, а гейт диска заблокирует все узлы');
      assert(byId.minFreeDiskError.hidden === false, 'нет строки ошибки для ' + JSON.stringify(raw));
      assert(byId.minFreeDiskError.textContent.indexOf('1048576') !== -1,
        'в ошибке нет верхней границы: ' + byId.minFreeDiskError.textContent);
      assert(toasts.length > 0 && toasts[toasts.length - 1].kind === 'error',
        'оператор не получил сообщения об ошибке (toast)');
    }
  });

  await check('допустимое значение сохраняется через существующий PUT конфига', async function () {
    resetPuts();
    byId.minFreeDisk.value = '12000';
    await settings.saveSettings(false);
    await tick();
    assert(puts.length === 1, 'PUT конфига не вызван (было ' + puts.length + ')');
    assert(puts[0].minFreeDisk === 12000, 'в PUT ушло ' + puts[0].minFreeDisk + ', want 12000');
    assert(byId.minFreeDiskError.hidden === true, 'строка ошибки осталась после сохранения');
  });

  await check('подсказка у порога: измеряемая ФС, текущее значение и кто исключён', async function () {
    byId.minFreeDisk.value = '12000';
    await settings.refreshDiskGuardHint();
    await tick();
    const hint = byId.minFreeDiskHint.textContent;
    assert(hint.indexOf('df -m /') !== -1,
      'в подсказке не сказано, ЧТО измеряется (df -m / в контейнере агента): ' + hint);
    assert(hint.indexOf('/app/models') !== -1,
      'в подсказке нет предупреждения про bind-том моделей (освобождение тома метрику не меняет): ' + hint);
    assert(hint.indexOf('9480') !== -1 && hint.indexOf('20000') !== -1,
      'в подсказке нет текущих значений узлов: ' + hint);
    assert(hint.indexOf('A10-126') !== -1 && hint.indexOf('исключён') !== -1,
      'в подсказке не назван узел, исключённый порогом: ' + hint);
    assert(hint.indexOf('12000') !== -1, 'в подсказке нет действующего порога: ' + hint);
  });

  await check('сброс к умолчанию: поле 10240 и сохранение сразу', async function () {
    resetPuts();
    byId.minFreeDisk.value = '9000';
    const click = byId.minFreeDiskDefault._listeners.click;
    assert(typeof click === 'function', 'у кнопки «Сбросить к 10240 МБ» нет обработчика');
    click({ preventDefault() {} });
    await tick();
    assert(String(byId.minFreeDisk.value) === '10240',
      'после сброса в поле ' + JSON.stringify(byId.minFreeDisk.value) + ', want "10240"');
    assert(puts.length === 1 && puts[0].minFreeDisk === 10240,
      'сброс не сохранён (PUT: ' + JSON.stringify(puts.map(function (p) { return p.minFreeDisk; })) + ') — ' +
      'гейт продолжал бы работать по старому порогу');
  });

  await check('ключи подписей есть в ru.js и en.js (симметрия)', async function () {
    const ru = fs.readFileSync(path.join(REPO_ROOT, 'webui', 'js', 'i18n', 'ru.js'), 'utf8');
    const en = fs.readFileSync(path.join(REPO_ROOT, 'webui', 'js', 'i18n', 'en.js'), 'utf8');
    ['settings.min_free_disk', 'settings.min_free_disk.measured', 'settings.min_free_disk.current',
      'settings.min_free_disk.no_data', 'settings.min_free_disk.blocked', 'settings.min_free_disk.invalid',
      'settings.min_free_disk.invalid_short', 'settings.min_free_disk.default_btn'].forEach(function (key) {
      const re = new RegExp('"' + key.replace(/\./g, '\\.') + '"\\s*:');
      assert(re.test(ru), 'ключа ' + key + ' нет в ru.js');
      assert(re.test(en), 'ключа ' + key + ' нет в en.js');
    });
  });

  if (failures > 0) {
    console.error('\nFAIL: ' + failures + ' проверок красные (файл: ' + TARGET + ')');
    process.exit(1);
  }
  console.log('\nOK: порог диска сохраняется, валидируется и объясняет измеряемую ФС');
}

run().catch(function (e) {
  console.error('FAIL: ' + (e && e.stack || e));
  process.exit(1);
});
