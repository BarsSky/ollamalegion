#!/usr/bin/env node
/*
 * ui-renderer-smoke.test.js — R83 (2026-09-25)
 *
 * ЗАЧЕМ. R70 добавил в webui/js/monitor/ui-renderer.js вызовы `updateText(...)`,
 * которых в этом файле никогда не было (функция приватная внутри IIFE
 * MonitorMetrics). На каждом refresh'е /monitor падало:
 *
 *     ReferenceError: updateText is not defined
 *         at renderAdmissionStats (ui-renderer.js:695)
 *         at updateUI (ui-renderer.js:367)
 *
 * Исключение выбрасывало управление из updateUI, поэтому терялись ВСЕ панели
 * после упавшей — включая updateTopology. Оператор видел пустой монитор
 * («Нет данных для отображения топологии») и `[monitor] fetchAll failed`
 * в консоли каждые ~2 секунды. Баг прожил R70 → R82 и не был пойман ничем:
 * Go-тесты WebUI не покрывают, а Playwright-спеки требуют поднятого стенда.
 *
 * ЧТО ПРОВЕРЯЕТ:
 *   1. updateUI() доходит до КОНЦА — вызывается updateTopology (до фикса он не
 *      вызывался никогда, потому что управление улетало раньше).
 *   2. Базовые счётчики реально заполняются (statBackends / statModelsLoaded).
 *   3. Один упавший рендер НЕ обрывает остальные: ломаем
 *      BackendTypeBadges.enhanceBackendsTable (середина цепочки) и убеждаемся,
 *      что очереди/сессии/топология всё равно отрисовались.
 *
 * ЗАПУСК: node webui/tests/ui-renderer-smoke.test.js   (без зависимостей)
 */

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const REPO_ROOT = path.resolve(__dirname, '..', '..');
// UI_RENDERER_TARGET — хук для проверки теста на произвольной ревизии файла
// (например, на версии из git, где баг ещё жив):
//   git show HEAD:webui/js/monitor/ui-renderer.js > /tmp/old.js
//   UI_RENDERER_TARGET=/tmp/old.js node webui/tests/ui-renderer-smoke.test.js   # должен УПАСТЬ
const TARGET = process.env.UI_RENDERER_TARGET
  ? path.resolve(process.env.UI_RENDERER_TARGET)
  : path.join(REPO_ROOT, 'webui', 'js', 'monitor', 'ui-renderer.js');

let failures = 0;

function check(name, ok, detail) {
  if (ok) {
    console.log('  PASS  ' + name);
  } else {
    failures++;
    console.log('  FAIL  ' + name + (detail ? ' — ' + detail : ''));
  }
}

// --- минимальный DOM ---------------------------------------------------------
function fakeEl(id) {
  const el = {
    id: id || '',
    textContent: '',
    innerHTML: '',
    className: '',
    hidden: false,
    value: '',
    checked: false,
    children: [],
    dataset: {},
    style: {},
    classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
    setAttribute() {},
    removeAttribute() {},
    getAttribute() { return null; },
    appendChild() {},
    removeChild() {},
    insertAdjacentHTML() {},
    addEventListener() {},
    removeEventListener() {},
    querySelector() { return null; },
    querySelectorAll() { return []; },
    closest() { return null; },
  };
  el.parentNode = { removeChild() {}, appendChild() {} };
  return el;
}

function makeSandbox(overrides) {
  const calls = { topology: 0, sessions: 0, queue: 0, clusterResources: 0 };
  const consoleErrors = [];

  const sandbox = {
    console: {
      log: function () {},
      info: function () {},
      warn: function () {},
      error: function () { consoleErrors.push(Array.prototype.slice.call(arguments).join(' ')); },
      debug: function () {},
    },
    localStorage: { getItem() { return null; }, setItem() {}, removeItem() {} },
    setTimeout, clearTimeout, setInterval, clearInterval,
    Date, Math, JSON, Object, Array, String, Number, Boolean, RegExp, Error,
    parseInt, parseFloat, isNaN, encodeURIComponent, decodeURIComponent,
  };

  // В браузере window === globalThis; воспроизводим это, иначе голые
  // идентификаторы вида `BackendTypeBadges` не разрешатся.
  sandbox.window = sandbox;
  sandbox.globalThis = sandbox;

  const elById = new Map();
  sandbox.document = {
    // Кэшируем элементы по id — иначе запись в .textContent одного экземпляра
    // не видна следующему getElementById, и проверки состояния DOM бесполезны.
    getElementById(id) {
      if (!elById.has(id)) elById.set(id, fakeEl(id));
      return elById.get(id);
    },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    createElement(tag) { return fakeEl(tag); },
    addEventListener() {},
    removeEventListener() {},
  };
  sandbox.__elById = elById;
  sandbox.document.body = fakeEl('body');
  sandbox.document.documentElement = fakeEl('html');

  // MonitorApp (MA) — helpers, которые ui-renderer ожидает от state.js.
  sandbox.MonitorApp = {
    T: function (k) { return k; },
    esc: function (s) { return String(s == null ? '' : s); },
    bar: function () { return '<div></div>'; },
    fmtDur: function (ms) { return (ms || 0) + 'ms'; },
    stableBackendOrder: function (arr) { return (arr || []).slice(); },
    requestRate: 0,
    lastTime: 0,
    lastTotalRequests: 0,
    _rpsInitialized: false,
  };

  // Считаем вызовы «хвостовых» панелей, чтобы доказать, что цепочка дошла.
  sandbox.updateTopology = function () { calls.topology++; };

  if (overrides) overrides(sandbox, calls);

  return { sandbox, calls, consoleErrors };
}

function loadRenderer(sandbox) {
  const src = fs.readFileSync(TARGET, 'utf8');
  vm.createContext(sandbox);
  vm.runInContext(src, sandbox, { filename: TARGET });
}

// Реалистичный payload: один llama.cpp-бэкенд, как в живом стенде (A10 + Qwen3.8).
function sampleData() {
  return {
    cluster: {
      backends: [{
        id: 'cppworker-gpu-bundled',
        status: 'healthy',
        backendType: 'llama_cpp',
        activeRequests: 0,
        maxConcurrentRequests: 10,
        models: ['Qwen3.8-27B-UD-Q4_K_M'],
        vram: { totalGB: 22.5, usedGB: 17.0, usagePercent: 75.8 },
        gpu: { usagePercent: 0, memoryTotal: 23040, memoryUsed: 17408, temperature: 60 },
        system: { memoryTotal: 54181, memoryUsed: 3174, memoryUsagePercent: 5.8 },
        llamaCpp: {
          loadedModels: [{ name: 'Qwen3.8-27B-UD-Q4_K_M', contextLength: 65536 }],
          requestsPerSecond: 0,
        },
        ollama: {},
        lastSeen: new Date().toISOString(),
      }],
      backendMetrics: [],
      recentClients: [],
      totalRequests: 10,
      rps: 0,
      effectiveBackendType: 'llama_cpp',
    },
    sessions: { sessions: [] },
    queueDetails: { pending_count: 0, processing_count: 0, all: [] },
    queueStats: { current_size: 0, processing_count: 0, admission: { enabled: true, waiting: 0 } },
    placement: { enabled: false, fallback: 'error', operatingMode: 'standard', decisions: [], warnings: [] },
    candidates: null,
    virtualModels: null,
    modelOps: null,
    autoPullConfig: null,
    autoPullStatus: null,
  };
}

console.log('ui-renderer smoke (' + path.relative(REPO_ROOT, TARGET) + ')');

// --- Тест 1: базовый прогон — updateUI доходит до конца ----------------------
{
  const { sandbox, calls, consoleErrors } = makeSandbox();
  loadRenderer(sandbox);

  check('файл экспортирует window.updateUI', typeof sandbox.updateUI === 'function');

  let threw = null;
  try {
    sandbox.updateUI(sampleData());
  } catch (e) {
    threw = e;
  }

  check('updateUI() не бросает исключение', threw === null, threw && threw.message);

  // Главный регресс-гард: до фикса управление улетало на renderAdmissionStats,
  // и updateTopology не вызывался НИКОГДА → пустая топология на /monitor.
  check('updateTopology вызван (цепочка дошла до конца)',
    calls.topology === 1, 'вызовов: ' + calls.topology);

  const noUpdateTextError = !consoleErrors.some(function (m) {
    return m.indexOf('updateText is not defined') !== -1 ||
      m.indexOf('render admissionStats failed') !== -1;
  });
  check('нет ошибки updateText в renderAdmissionStats', noUpdateTextError,
    consoleErrors.join(' | '));

  check('statBackends заполнен',
    sandbox.document.getElementById('statBackends').textContent !== '' &&
    sandbox.document.getElementById('statBackends').textContent !== undefined);
}

// --- Тест 2: упавшая панель не обрывает остальные (safeRender) --------------
{
  const { sandbox, calls, consoleErrors } = makeSandbox(function (sb) {
    // Ломаем панель ровно в СЕРЕДИНЕ цепочки updateUI (после admission/placement,
    // до queue/sessions/topology). До safeRender исключение здесь убило бы всё,
    // что идёт ниже.
    sb.BackendTypeBadges = {
      enhanceBackendsTable: function () { throw new Error('synthetic panel failure'); },
    };
  });
  loadRenderer(sandbox);

  let threw = null;
  try {
    sandbox.updateUI(sampleData());
  } catch (e) {
    threw = e;
  }

  check('падение панели не пробрасывается наружу', threw === null, threw && threw.message);
  check('падение панели залогировано',
    consoleErrors.some(function (m) { return m.indexOf('synthetic panel failure') !== -1; }),
    consoleErrors.join(' | '));
  check('панели ПОСЛЕ упавшей всё равно отрисованы (topology)',
    calls.topology === 1, 'вызовов: ' + calls.topology);
}

console.log('');
if (failures > 0) {
  console.log('ИТОГ: провалено проверок — ' + failures);
  process.exit(1);
}
console.log('ИТОГ: все проверки пройдены');
