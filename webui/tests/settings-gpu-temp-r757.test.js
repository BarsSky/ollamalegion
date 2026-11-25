#!/usr/bin/env node
/*
 * settings-gpu-temp-r757.test.js — R757 (2026-11-25)
 *
 * ЗАЧЕМ. Живая жалоба оператора: «в балансере постоянно фигурировала информация
 * о возможном троттлинге из-за температуры выше пороговой, но явно об этом не
 * сообщалось в WebUI и не понятно, какая на самом деле температура критична для
 * видеокарты».
 *
 * Так и было: порог `resources.gpu.maxTemperature` (LB_GPU_MAX_TEMP, дефолт 85)
 * использовался балансером как soft-WARN — в архиве второго стенда строка
 * `backend GPU горячее заданного предела (soft, не блокирует) temperature=88
 * threshold=85` повторилась 14 раз. В панели температура не показывалась вообще,
 * а значение порога нельзя было ни увидеть, ни изменить.
 *
 * ЧТО ПРОВЕРЯЕТ:
 *   1. недопустимое значение НЕ уходит в PUT и объясняется рядом с полем;
 *   2. допустимое сохраняется полем gpuMaxTemp существующего PUT конфига;
 *   3. подсказка объясняет: порог мягкий; аппаратный троттлинг карта включает
 *      сама (≈90-95 °C); вот текущая температура по узлам; вот кто уже выше;
 *   4. серверное значение и границы заливаются в поле при открытии страницы;
 *   5. ключи подписей есть в ru.js и en.js (симметрия).
 *
 * НЕ-ВАКУУМНОСТЬ (проверено вручную):
 *   git show HEAD~1:webui/js/modules/app-settings.js > /tmp/old-settings.js
 *   APP_SETTINGS_TARGET=/tmp/old-settings.js node webui/tests/settings-gpu-temp-r757.test.js
 *   → КРАСНЕЕТ: старый код не отправляет gpuMaxTemp вовсе и не рисует подсказку.
 *
 * ЗАПУСК: node webui/tests/settings-gpu-temp-r757.test.js
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
  return {
    id: id,
    value: value === undefined ? '' : value,
    checked: false,
    hidden: false,
    textContent: '',
    innerHTML: '',
    className: '',
    style: {},
    _attrs: {},
    _listeners: {},
    classList: {
      _c: {},
      add(c) { this._c[c] = true; },
      remove(c) { delete this._c[c]; },
      contains(c) { return !!this._c[c]; }
    },
    addEventListener(ev, fn) { this._listeners[ev] = fn; },
    setAttribute(k, v) { this._attrs[k] = String(v); },
    getAttribute(k) { return this._attrs[k] === undefined ? null : this._attrs[k]; }
  };
}
const byId = {
  gpuMaxTemp: makeEl('gpuMaxTemp', '85'),
  gpuMaxTempHint: makeEl('gpuMaxTempHint', ''),
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
// Температуры — как в архиве второго стенда: A10 держит 88 °C при пороге 85.
const GPU_BACKENDS = [
  { id: 'A10-126', gpu: { usagePercent: 92, temperature: 88 } },
  { id: 'cppworker-gpu-bundled-agent', gpu: { usagePercent: 40, temperature: 71 } },
];
let puts = [];
let serverGpuMaxTemp = 85;
const toasts = [];
global.Api = {
  cluster() { return Promise.resolve({ backends: GPU_BACKENDS, gpuMaxTemp: serverGpuMaxTemp }); },
  config() {
    return Promise.resolve({
      operatingMode: 'standard',
      useEnhancedScoring: true,
      modelAffinity: true,
      sessionStickiness: true,
      gpuMaxTemp: serverGpuMaxTemp,
      gpuMaxTempDefault: 85,
      gpuMaxTempMin: 50,
      gpuMaxTempMax: 120,
      // Секции — как в настоящем GET /api/v1/cluster/config: verifySync сверяет
      // именно их, и без них путь сохранения уходил бы в catch.
      modelReplication: { enabled: false, defaultMinInstances: 1, defaultMaxInstances: 3, idleUnloadAfter: '10m' },
      rpcCoordinator: { enabled: false, coordinatorURL: '', workerPort: 18050, protocol: 'http' },
      virtualModels: { enabled: false, coordMode: 'sequential' },
      distInference: { enabled: false, grpcPort: 19000 },
    });
  },
  updateConfig(cfg) {
    puts.push(cfg);
    if (cfg.gpuMaxTemp) serverGpuMaxTemp = cfg.gpuMaxTemp;
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
}

async function run() {
  settings.loadSettings();
  await tick();

  await check('серверное значение и границы заливаются в поле при открытии страницы', async function () {
    assert(String(byId.gpuMaxTemp.value) === '85',
      'в поле ' + JSON.stringify(String(byId.gpuMaxTemp.value)) + ', want "85" — оператор не видит действующий порог');
    assert(byId.gpuMaxTemp.getAttribute('min') === '50', 'min не выставлен: ' + byId.gpuMaxTemp.getAttribute('min'));
    assert(byId.gpuMaxTemp.getAttribute('max') === '120', 'max не выставлен: ' + byId.gpuMaxTemp.getAttribute('max'));
  });

  await check('недопустимое значение не уходит на сервер и объясняется тостом', async function () {
    const bad = ['150', '-5', '12.5', 'abc'];
    for (const raw of bad) {
      resetPuts();
      byId.gpuMaxTemp.value = raw;
      await settings.saveSettings(false);
      await tick();
      assert(puts.length === 0, 'значение ' + JSON.stringify(raw) + ' ушло в PUT (gpuMaxTemp=' +
        JSON.stringify(puts.map(function (p) { return p.gpuMaxTemp; })) + ') — сервер отвергнет его 400-й, а порог остался бы прежним');
      assert(toasts.length > 0 && toasts[toasts.length - 1].kind === 'error',
        'оператор не получил сообщения об ошибке для ' + JSON.stringify(raw));
      assert(toasts[toasts.length - 1].msg.indexOf('120') !== -1,
        'в сообщении нет границ порога: ' + toasts[toasts.length - 1].msg);
    }
  });

  await check('допустимое значение сохраняется через существующий PUT конфига', async function () {
    resetPuts();
    byId.gpuMaxTemp.value = '92';
    await settings.saveSettings(false);
    await tick();
    assert(puts.length === 1, 'PUT конфига не вызван (было ' + puts.length + ')');
    assert(puts[0].gpuMaxTemp === 92, 'в PUT ушло ' + puts[0].gpuMaxTemp + ', want 92');
    assert(serverGpuMaxTemp === 92, 'сервер не получил новое значение (стоит ' + serverGpuMaxTemp + ')');
    assert(toasts.length === 1 && toasts[0].kind === 'success',
      'нет тоста об успехе: ' + JSON.stringify(toasts.map(function (t) { return t.kind + ':' + t.msg; })));
  });

  await check('подсказка объясняет: порог мягкий, троттлинг карты, текущие температуры и кто выше', async function () {
    byId.gpuMaxTemp.value = '85';
    await settings.refreshGpuTempHint();
    await tick();
    const hint = byId.gpuMaxTempHint.textContent;
    assert(hint.indexOf('мягкий') !== -1,
      'в подсказке не сказано, что порог мягкий (не блокирует запросы): ' + hint);
    assert(hint.indexOf('90-95') !== -1,
      'в подсказке нет аппаратного порога троттлинга карты — оператор не понимает, какая температура критична: ' + hint);
    assert(hint.indexOf('88 °C') !== -1 && hint.indexOf('71 °C') !== -1,
      'в подсказке нет текущих температур узлов: ' + hint);
    assert(hint.indexOf('A10-126') !== -1 && hint.indexOf('выше порога') !== -1,
      'в подсказке не назван узел, который уже выше порога: ' + hint);
    assert(hint.indexOf('85 °C') !== -1, 'в подсказке нет действующего порога: ' + hint);
  });

  await check('порог из подсказки пересчитывается по введённому значению', async function () {
    byId.gpuMaxTemp.value = '95';
    await settings.refreshGpuTempHint();
    await tick();
    const hint = byId.gpuMaxTempHint.textContent;
    assert(hint.indexOf('95 °C') !== -1, 'в подсказке нет нового порога 95: ' + hint);
    assert(hint.indexOf('выше порога') === -1,
      'при пороге 95 узел с 88 °C всё ещё назван «выше порога»: ' + hint);
  });

  await check('ключи подписей есть в ru.js и en.js (симметрия)', async function () {
    const ru = fs.readFileSync(path.join(REPO_ROOT, 'webui', 'js', 'i18n', 'ru.js'), 'utf8');
    const en = fs.readFileSync(path.join(REPO_ROOT, 'webui', 'js', 'i18n', 'en.js'), 'utf8');
    ['settings.gpu_max_temp', 'settings.gpu_max_temp.soft', 'settings.gpu_max_temp.current',
      'settings.gpu_max_temp.no_data', 'settings.gpu_max_temp.hot', 'settings.gpu_max_temp.invalid'].forEach(function (key) {
      const re = new RegExp('"' + key.replace(/\./g, '\\.') + '"\\s*:');
      assert(re.test(ru), 'ключа ' + key + ' нет в ru.js');
      assert(re.test(en), 'ключа ' + key + ' нет в en.js');
    });
  });

  if (failures > 0) {
    console.error('\nFAIL: ' + failures + ' проверок красные (файл: ' + TARGET + ')');
    process.exit(1);
  }
  console.log('\nOK: порог температуры GPU виден, валидируется и объясняет, что критично');
}

run().catch(function (e) {
  console.error('FAIL: ' + (e && e.stack || e));
  process.exit(1);
});
