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
//
// R-Image Phase 8 (2026-10-03): сюда же добавлены счётчики запросов
// (backend.image.requests + backend.image.recent) и агрегат пула
// (cluster.image) — панель «запросы к image-бэкендам» и колонки
// RPS/Avg RT/Active в таблице бэкендов читают именно их.
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
        // Ровно то, что отдаёт живой балансер: у image-бэкенда нет агента, и Go
        // сериализует нулевое время строкой. До фикса в колонке «Последняя
        // активность» рисовалось «01.01.1, 02:30:17» (см. проверку ниже).
        lastAgentContact: '0001-01-01T00:00:00Z',
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
          // Счётчики ЭТОГО бэкенда. inFlight=2 при activeRequests=0 в топе
          // бэкенда специально: проверка ниже доказывает, что колонка Active
          // берёт inFlight из image.requests, а не пустое активное поле.
          requests: {
            inFlight: 2, total: 3, ok: 2, failed: 1, rejected: 0, accepted: 0, finished: 0,
            rps: 0.4, avgDurationMs: 9100, p50DurationMs: 8800, p95DurationMs: 21000, lastDurationMs: 9000,
            lastRequestAt: '2026-10-03T10:00:00Z',
            failuresByCode: { engine_oom: 1 }, gateDeniedByCode: { insufficient_vram: 2 },
          },
          recent: [
            { id: 'img-1', at: '2026-10-03T10:00:00Z', backendId: 'image-real', surface: 'openai',
              path: '/v1/images/generations', model: 'sd-cpp-local', prompt: 'a cat', width: 512, height: 512,
              steps: 8, batch: 1, durationMs: 9000, status: 'ok', httpStatus: 200, code: '', error: '', images: 1 },
          ],
        },
      }],
      backendMetrics: [],
      recentClients: [],
      totalRequests: 10,
      rps: 0,
      effectiveBackendType: 'image_cpp',
      // Агрегат по пулу (types.ImagePoolMetrics). Значения намеренно
      // отличаются от per-backend: так видно, что панель читает пул, а
      // таблица бэкендов — свою строку.
      image: {
        backends: 1,
        requests: {
          inFlight: 2, total: 7, ok: 6, failed: 1, rejected: 2, accepted: 0, finished: 0,
          rps: 0.1, avgDurationMs: 9100, p50DurationMs: 8800, p95DurationMs: 21000, lastDurationMs: 9000,
          lastRequestAt: '2026-10-03T10:00:00Z',
          failuresByCode: { engine_oom: 1 }, gateDeniedByCode: { insufficient_vram: 2 },
        },
        recent: [
          { id: 'img-1', at: '2026-10-03T10:00:00Z', backendId: 'image-real', surface: 'openai',
            path: '/v1/images/generations', model: 'sd-cpp-local', prompt: 'a cat', width: 512, height: 512,
            steps: 8, batch: 1, durationMs: 9000, status: 'ok', httpStatus: 200, code: '', error: '', images: 1 },
          { id: 'img-2', at: '2026-10-03T10:00:05Z', backendId: 'image-real', surface: 'openai',
            path: '/v1/images/generations', model: 'sd15-q4', prompt: 'a dog', width: 768, height: 768,
            steps: 20, batch: 1, durationMs: 21000, status: 'failed', httpStatus: 500,
            code: 'engine_oom', error: 'out of memory', images: 0 },
        ],
      },
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

// --- Тест 4: панель image-запросов (R-Image Phase 8) -------------------------
// Симптом: «Monitor не показывает запросы, которые идут к image-бэкендам» -
// в таблице бэкендов у image-воркера RPS=«-», Avg RT=«-», Active=«0/10», а
// сами генерации нигде не видны.
// Проверяем: (а) агрегаты пула в элементах панели, (б) строки ленты,
// (в) RPS/Avg RT/Active в таблице бэкендов берутся из backend.image.requests.
{
  // Spy на T: рендерер захватывает MA.T на этапе загрузки скрипта, поэтому
  // подменяем его ДО loadRenderer. Нужен, чтобы доказать: статус запроса
  // переводится через ключи monitor.imageRequests.status.*, а не печатается
  // сырым кодом. Обычный T в этом sandbox'е возвращает сам ключ, и отличить
  // «перевели» от «не перевели» было бы нельзя.
  const tKeys = [];
  const { sandbox, calls, consoleErrors } = makeSandbox(function (sb) {
    const origT = sb.MonitorApp.T;
    sb.MonitorApp.T = function (k, p) {
      tKeys.push(String(k));
      if (String(k).indexOf('monitor.imageRequests.status.') === 0) return '«' + k + '»';
      return origT(k, p);
    };
  });
  loadRenderer(sandbox, { withUtils: true });

  let threw = null;
  try {
    sandbox.updateUI(sampleImageData());
  } catch (e) {
    threw = e;
  }
  const txt = function (id) { return String(sandbox.document.getElementById(id).textContent); };

  check('imageReq: updateUI() не бросает исключение', threw === null, threw && threw.message);
  check('imageReq: цепочка панелей дошла до конца', calls.topology === 1, 'вызовов: ' + calls.topology);

  // (а) агрегаты пула (cluster.image.requests) отрисованы в панели
  check('imageReq: панель показана (image-бэкенды есть)',
    sandbox.document.getElementById('panelImageRequests').style.display === '',
    String(sandbox.document.getElementById('panelImageRequests').style.display));
  check('imageReq: total = 7', txt('imgReqTotal') === '7', txt('imgReqTotal'));
  check('imageReq: ok = 6', txt('imgReqOk') === '6', txt('imgReqOk'));
  check('imageReq: failed = 1', txt('imgReqFailed') === '1', txt('imgReqFailed'));
  check('imageReq: rejected = 2', txt('imgReqRejected') === '2', txt('imgReqRejected'));
  check('imageReq: in-flight = 2', txt('imgReqInFlight') === '2', txt('imgReqInFlight'));
  check('imageReq: RPS = 0.1', txt('imgReqRps') === '0.1', txt('imgReqRps'));
  check('imageReq: avg = 9.1 s (9100 ms)', txt('imgReqAvg') === '9.1 s', txt('imgReqAvg'));
  check('imageReq: p95 = 21 s (21000 ms)', txt('imgReqP95') === '21 s', txt('imgReqP95'));
  check('imageReq: счётчик в заголовке = 2 записи ленты',
    txt('imageRequestsCount') === '2', txt('imageRequestsCount'));

  // (б) лента: модель/размер/длительность/статус/бэкенд/путь
  const feedHtml = sandbox.document.querySelector('#imageRequestsTable tbody').innerHTML;
  check('imageReq: модель запроса в ленте', feedHtml.indexOf('sd-cpp-local') !== -1, feedHtml.slice(0, 200));
  check('imageReq: размер WxH в ленте (512x512)', feedHtml.indexOf('512x512') !== -1);
  check('imageReq: размер второй строки (768x768)', feedHtml.indexOf('768x768') !== -1);
  check('imageReq: длительность в ленте (9000 ms -> 9.0 s)', feedHtml.indexOf('9.0 s') !== -1);
  check('imageReq: длительность в ленте (21000 ms -> 21 s)', feedHtml.indexOf('21 s') !== -1);
  check('imageReq: статус ok переведён через i18n-ключ',
    feedHtml.indexOf('monitor.imageRequests.status.ok') !== -1);
  check('imageReq: статус failed переведён через i18n-ключ',
    feedHtml.indexOf('monitor.imageRequests.status.failed') !== -1);
  check('imageReq: оба статуса реально запрошены у i18n',
    tKeys.indexOf('monitor.imageRequests.status.ok') !== -1 &&
    tKeys.indexOf('monitor.imageRequests.status.failed') !== -1,
    tKeys.join(','));
  check('imageReq: шаги запроса в ленте (8 и 20)',
    feedHtml.indexOf('>8</td>') !== -1 && feedHtml.indexOf('>20</td>') !== -1);
  check('imageReq: бэкенд и путь запроса в ленте',
    feedHtml.indexOf('image-real') !== -1 && feedHtml.indexOf('/v1/images/generations') !== -1);
  check('imageReq: код ошибки попал в title (разбор инцидента)',
    feedHtml.indexOf('engine_oom') !== -1 && feedHtml.indexOf('out of memory') !== -1);

  // (в) таблица бэкендов: RPS / Avg RT / Active из image.requests
  const backendsHtml = sandbox.document.querySelector('#backendsTable tbody').innerHTML;
  check('imageReq: Active в таблице бэкендов = 2/4 (inFlight/max)',
    backendsHtml.indexOf('>2/4</td>') !== -1, backendsHtml.slice(0, 300));
  check('imageReq: RPS в таблице бэкендов = 0.4 (image.requests.rps)',
    backendsHtml.indexOf('>0.4<div class="sl-cell">') !== -1);
  check('imageReq: Avg RT в таблице бэкендов = 9.1 s (avgDurationMs)',
    backendsHtml.indexOf('>9.1 s<div class="sl-cell">') !== -1);
  check('imageReq: колонки Image req/OK/err заполнены из image.requests',
    backendsHtml.indexOf('<td class="col-right">3</td>' +
      '<td class="col-right" style="color:var(--success)">2</td>' +
      '<td class="col-right" style="color:var(--danger)">1</td>') !== -1,
    backendsHtml.slice(-260));

  const renderErrors = consoleErrors.filter(function (m) { return m.indexOf('failed') !== -1; });
  check('imageReq: ни один рендер не упал', renderErrors.length === 0, renderErrors.join(' | '));
}

// --- Тест 5: без image-бэкендов панель скрыта, с нулями - показана -----------
// Требование контракта: cluster.image ОТСУТСТВУЕТ, если в кластере нет ни
// одного image-бэкенда, и панель обязана быть скрыта (в чисто текстовом
// стенде лишней панели быть не должно). Обратный случай - image-бэкенды есть,
// запросов ноль: панель показывается с нулями и строкой «запросов пока не
// было» (осознанное решение: «воркер поднят, генераций ещё не было» - это тоже
// информация, а не отсутствие данных).
{
  const { sandbox, calls } = makeSandbox();
  loadRenderer(sandbox, { withUtils: true });

  let threw = null;
  try {
    sandbox.updateUI(sampleData());
  } catch (e) {
    threw = e;
  }
  check('imageReq-off: updateUI() на текстовом кластере не падает', threw === null, threw && threw.message);
  check('imageReq-off: цепочка панелей дошла до конца', calls.topology === 1, 'вызовов: ' + calls.topology);
  check('imageReq-off: панель скрыта (cluster.image нет)',
    sandbox.document.getElementById('panelImageRequests').style.display === 'none',
    String(sandbox.document.getElementById('panelImageRequests').style.display));

  const backendsHtml = sandbox.document.querySelector('#backendsTable tbody').innerHTML;
  check('imageReq-off: у не-image бэкенда три колонки image-запросов = «-»',
    backendsHtml.indexOf('<td class="col-right">-</td><td class="col-right">-</td><td class="col-right">-</td>') !== -1,
    backendsHtml.slice(-260));
  check('imageReq-off: RPS не-image бэкенда остался прочерком',
    backendsHtml.indexOf('>-<div class="sl-cell">') !== -1 || backendsHtml.indexOf('>0.0<div class="sl-cell">') !== -1);
}

// --- Тест 6: image-бэкенды есть, запросов ноль -> панель с нулями -----------
{
  const data = sampleImageData();
  data.cluster.image = {
    backends: 1,
    requests: { inFlight: 0, total: 0, ok: 0, failed: 0, rejected: 0, accepted: 0, finished: 0, rps: 0 },
    recent: [],
  };
  const { sandbox } = makeSandbox();
  loadRenderer(sandbox, { withUtils: true });
  sandbox.updateUI(data);

  const txt = function (id) { return String(sandbox.document.getElementById(id).textContent); };
  check('imageReq-zero: панель показана (image-бэкенд есть, запросов нет)',
    sandbox.document.getElementById('panelImageRequests').style.display === '');
  check('imageReq-zero: total = 0', txt('imgReqTotal') === '0', txt('imgReqTotal'));
  check('imageReq-zero: avg без данных = «—»', txt('imgReqAvg') === '—', txt('imgReqAvg'));
  check('imageReq-zero: счётчик ленты = 0', txt('imageRequestsCount') === '0', txt('imageRequestsCount'));
  const feedHtml = sandbox.document.querySelector('#imageRequestsTable tbody').innerHTML;
  check('imageReq-zero: строка «запросов пока не было»',
    feedHtml.indexOf('monitor.imageRequests.empty') !== -1, feedHtml.slice(0, 200));
}

// --- Тест 7: фильтр по типу «image.cpp» не выкидывает image-бэкенд ----------
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

// --- Тест 8: лента экранирует текст из тела запроса клиента (XSS) ------------
// model/path/backendId в ленте приходят из ТЕЛА запроса клиента, то есть
// подконтрольны тому, кто вызывает генерацию. MonitorApp.esc в
// webui/js/monitor/state.js для &, <, > сейчас возвращает те же символы (карта
// замены потеряла HTML-сущности), поэтому рендерер ленты использует
// собственный escHtml - проверяем, что разметка не инжектится.
{
  const evil = '"><img src=x onerror=alert(1)>';
  const data = sampleImageData();
  data.cluster.image.recent = [{
    id: 'img-x', at: '2026-10-03T10:00:00Z', backendId: evil, surface: 'openai',
    path: evil, model: evil, width: 512, height: 512, steps: 8, batch: 1,
    durationMs: 1000, status: 'ok', httpStatus: 200, code: evil, error: evil, images: 1,
  }];
  const { sandbox } = makeSandbox();
  loadRenderer(sandbox, { withUtils: true });
  sandbox.updateUI(data);

  const feedHtml = sandbox.document.querySelector('#imageRequestsTable tbody').innerHTML;
  check('imageReq-xss: сырой <img> из ленты не попал в разметку',
    feedHtml.indexOf('<img') === -1, feedHtml.slice(0, 300));
  check('imageReq-xss: инъекция экранирована',
    feedHtml.indexOf('&lt;img') !== -1 && feedHtml.indexOf('&quot;&gt;') !== -1);
}

console.log('');
if (failures > 0) {
  console.log('ИТОГ: провалено проверок — ' + failures);
  process.exit(1);
}
console.log('ИТОГ: все проверки пройдены');
