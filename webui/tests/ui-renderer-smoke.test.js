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
  // R-Image Phase 5 (2026-10-02): селекторы тоже кэшируем. До этого
  // document.querySelector всегда возвращал null, поэтому renderBackends и
  // renderModelsInMemory падали на `tb.innerHTML = ...` (ошибку глотал
  // safeRender), и таблицы монитора в этом smoke-тесте не проверялись вообще.
  const elBySelector = new Map();
  sandbox.document = {
    // Кэшируем элементы по id — иначе запись в .textContent одного экземпляра
    // не видна следующему getElementById, и проверки состояния DOM бесполезны.
    getElementById(id) {
      if (!elById.has(id)) elById.set(id, fakeEl(id));
      return elById.get(id);
    },
    querySelector(sel) {
      if (!elBySelector.has(sel)) elBySelector.set(sel, fakeEl(sel));
      return elBySelector.get(sel);
    },
    querySelectorAll() { return []; },
    createElement(tag) { return fakeEl(tag); },
    addEventListener() {},
    removeEventListener() {},
  };
  sandbox.__elById = elById;
  sandbox.__elBySelector = elBySelector;
  sandbox.document.body = fakeEl('body');
  sandbox.document.documentElement = fakeEl('html');

  // MonitorApp (MA) — helpers, которые ui-renderer ожидает от state.js.
  sandbox.MonitorApp = {
    T: function (k) { return k; },
    esc: function (s) { return String(s == null ? '' : s); },
    bar: function () { return '<div></div>'; },
    fmtDur: function (ms) { return (ms || 0) + 'ms'; },
    stableBackendOrder: function (arr) { return (arr || []).slice(); },
    isCloudModel: function () { return false; },
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

function loadRenderer(sandbox, opts) {
  const src = fs.readFileSync(TARGET, 'utf8');
  vm.createContext(sandbox);
  // R-Image Phase 5: monitor.html теперь грузит modules/utils.js (общие хелперы
  // бейджа типа и разбора backend.image). В sandbox он подключается по флагу,
  // чтобы проверить реальный бейдж 🎨 image.cpp, а не локальный fallback.
  if (opts && opts.withUtils) {
    const utilsPath = path.join(REPO_ROOT, 'webui', 'js', 'modules', 'utils.js');
    const utilsSrc = fs.readFileSync(utilsPath, 'utf8');
    vm.runInContext(utilsSrc, sandbox, { filename: utilsPath });
  }
  vm.runInContext(src, sandbox, { filename: TARGET });
}

// R-Image Phase 5 (2026-10-02): image-бэкенд, как его отдаёт GET /api/v1/metrics.
// Отдельный пример нужен потому, что у него нет ни ollama, ни llamaCpp: порт
// воркера лежит в imagePort, модели — в backend.image.models.
function sampleImageData() {
  return {
    cluster: {
      backends: [{
        id: 'image-real',
        status: 'healthy',
        backendType: 'image_cpp',
        host: '127.0.0.1',
        ollamaPort: 11434,
        cppWorkerPort: 0,
        imagePort: 18093,
        maxConcurrentRequests: 4,
        image: {
          state: 'loaded',
          currentModel: 'sd15-q4',
          vramFreeMb: 894,
          vramTotalMb: 8192,
          updatedAt: '2026-10-02T10:00:00Z',
          lastError: '',
          models: [
            { name: 'sd15-q4', state: 'loaded', family: 'sd15', sizeBytes: 1526, vramEstimateMb: 2600, activeQueries: 0 },
            { name: 'sdxl-base', state: 'not_loaded', family: 'sdxl', sizeBytes: 6800, vramEstimateMb: 7800, activeQueries: 0 },
          ],
        },
      }],
      backendMetrics: [],
      recentClients: [],
      totalRequests: 10,
      rps: 0,
      effectiveBackendType: 'image_cpp',
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

// --- Тест 3: image-бэкенд (R-Image Phase 5) ---------------------------------
// Симптом: «Monitor не показывает image-бэкенд с его моделью и отдельным портом».
// Проверяем, что строка таблицы бэкендов и список моделей в памяти содержат
// реальные данные воркера (imagePort / image.models), а не ollama-поля.
{
  const { sandbox, calls, consoleErrors } = makeSandbox();
  loadRenderer(sandbox, { withUtils: true });

  let threw = null;
  try {
    sandbox.updateUI(sampleImageData());
  } catch (e) {
    threw = e;
  }
  check('image: updateUI() не бросает исключение', threw === null, threw && threw.message);
  check('image: цепочка панелей дошла до конца', calls.topology === 1, 'вызовов: ' + calls.topology);

  const backendsHtml = sandbox.document.querySelector('#backendsTable tbody').innerHTML;
  check('image: в таблице бэкендов виден порт воркера 18093',
    backendsHtml.indexOf('18093') !== -1);
  check('image: в таблице бэкендов видна модель воркера',
    backendsHtml.indexOf('sd15-q4') !== -1);
  check('image: бейдж типа 🎨 image.cpp отрисован (utils.js подключён)',
    backendsHtml.indexOf('🎨 image.cpp') !== -1, backendsHtml.slice(0, 200));
  check('image: состояние воркера отрисовано', backendsHtml.indexOf('loaded') !== -1);
  check('image: кнопки unload (чужой API) у image-моделей нет',
    backendsHtml.indexOf('monitor-unload-btn') === -1);

  const modelsHtml = sandbox.document.querySelector('#modelsTable tbody').innerHTML;
  check('image: модель попала в «Models in Memory»',
    modelsHtml.indexOf('sd15-q4') !== -1 && modelsHtml.indexOf('sdxl-base') !== -1);
  check('image: оценка VRAM модели отрисована (vramEstimateMb, а не 0)',
    modelsHtml.indexOf('~2.5 GB') !== -1, modelsHtml.slice(0, 300));
  check('image: счётчик загруженных моделей = 2',
    sandbox.document.getElementById('statModelsLoaded').textContent === 2,
    String(sandbox.document.getElementById('statModelsLoaded').textContent));

  const realErrors = consoleErrors.filter(function (m) {
    return m.indexOf('panel failed') !== -1;
  });
  check('image: ни одна панель не упала', realErrors.length === 0, realErrors.join(' | '));
}

// --- Тест 4: фильтр по типу «image.cpp» не выкидывает image-бэкенд ----------
{
  const data = sampleImageData();
  // Добавляем Ollama-бэкенд: фильтр effType=image_cpp должен оставить один.
  data.cluster.backends.push({
    id: 'ollama-1', status: 'healthy', backendType: 'ollama',
    activeRequests: 0, maxConcurrentRequests: 8, models: ['llama3.1'],
    vram: { totalGB: 24, usedGB: 0, usagePercent: 0 }, gpu: {}, system: {}, ollama: {},
  });
  const { sandbox } = makeSandbox();
  loadRenderer(sandbox, { withUtils: true });
  sandbox.updateUI(data);
  check('filter: при типе image.cpp остаётся только image-бэкенд',
    sandbox.document.getElementById('statBackends').textContent === 1,
    String(sandbox.document.getElementById('statBackends').textContent));
  const backendsHtml = sandbox.document.querySelector('#backendsTable tbody').innerHTML;
  check('filter: image-бэкенд в отфильтрованной таблице',
    backendsHtml.indexOf('image-real') !== -1 && backendsHtml.indexOf('ollama-1') === -1);
}

console.log('');
if (failures > 0) {
  console.log('ИТОГ: провалено проверок — ' + failures);
  process.exit(1);
}
console.log('ИТОГ: все проверки пройдены');
