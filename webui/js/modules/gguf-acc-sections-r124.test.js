// gguf-acc-sections-r124.test.js — R124 (2026-10-16), пункты 3-4 разбора стенда.
//
// ЗАЧЕМ. Оператор жаловался на форму параметров бэкенда: «всё вперемешку, сложно
// воспринимать, неудобно пролистывать вниз, чтобы от дефолтных перейти к профилям
// и параметрам загруженной модели». Плюс управление раскладкой лежало внутри
// блока Multi-GPU в одной строке с «без mmap», а состояние узла (отдаёт ли ЭТА
// машина свою карту) не показывалось вовсе.
//
// Здесь проверяется:
//   1. плоская форма превращается в КОМПАКТНЫЕ раскрываемые секции (<details>),
//      первая раскрыта, содержимое и id полей при этом не теряются;
//   2. раскладка (донор/приёмник) — ОТДЕЛЬНАЯ секция, а не строка внутри Multi-GPU;
//   3. состояние узла показывается ФАКТОМ (слушает / порт открывается /
//      остановлен + причина), а при отсутствии данных честно пишется «нет данных»,
//      а не «работает»/«выключено».
//
// Запуск: node webui/js/modules/gguf-acc-sections-r124.test.js
'use strict';

const assert = require('assert');

global.window = global;
global.Utils = {
    escapeHtml: function (s) {
        return String(s === null || s === undefined ? '' : s)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    },
    updateGpuLayersBadge: function () {},
    setGpuLayers: function () {},
};
global.I18N = { t: function (key) { return key; } };

global.GgufModule = {
    state: {
        selectedBackendId: 'bk-1',
        registeredBackends: [{ id: 'bk-1', backendType: 'llama_cpp' }],
        detailPane: 'settings',
        localModels: [],
        loadedModels: [],
        runtimeModels: {},
        rpcLayout: null,
        workerInfo: null,
        gpuInfo: null,
        diskInventory: {},
    },
    _: function (key, fallback) { return fallback || key; },
    stripGGUF: function (n) { return n; },
    formatFileSize: function () { return '-'; },
    formatVramMB: function (n) { return String(n); },
    showToast: function () {},
    refreshDetailPane: function () {},
    refreshDetailPanel: function () {},
};
global.GgufApi = {
    getRuntimeConfigViaBackend: function () { return Promise.resolve({ loaded_models: [] }); },
    listLocalModelsViaBackend: function () { return Promise.resolve({ files: [] }); },
    requestViaBackend: function () { return Promise.resolve({ files: [] }); },
};

require('./gguf-renderer-detail-render.js');
const M = global.GgufModule;

let failures = 0;
function check(name, cond, detail) {
    if (cond) { console.log('  PASS ' + name); }
    else { failures++; console.log('  FAIL ' + name + (detail ? ' — ' + detail : '')); }
}

console.log('=== R124: компактные секции формы и блок «донор/приёмник» ===\n');

check('модуль экспортирует collapseSections', typeof M.collapseSections === 'function');
check('модуль экспортирует renderRpcServerState', typeof M.renderRpcServerState === 'function');

// ── 1. Свёртка плоского HTML в секции ────────────────────────────────────────
const flat =
    '<div class="form-actions">save</div>' +
    '<h6 class="gguf-section-header"><i class="fas fa-sliders-h"></i> General</h6>' +
    '<input id="ggufOptCtxSize" value="8192">' +
    '<h6 class="gguf-section-header"><i class="fas fa-layer-group"></i> Multi-GPU</h6>' +
    '<input id="ggufOptMainGpu" value="0">' +
    '<h6 class="gguf-section-header"><i class="fas fa-network-wired"></i> Донор / приёмник</h6>' +
    '<input id="ggufOptRpcServerPort" value="50052">';

const folded = M.collapseSections(flat);
const detailsCount = (folded.match(/<details class="gguf-acc"/g) || []).length;
check('каждая секция стала <details>', detailsCount === 3, 'найдено ' + detailsCount);
check('у секций есть <summary> с заголовком', (folded.match(/<summary class="gguf-acc-summary">/g) || []).length === 3);
check('первая секция раскрыта (open)', /<details class="gguf-acc" open data-acc="1">/.test(folded));
check('остальные секции свёрнуты', !/<details class="gguf-acc" open data-acc="2">/.test(folded));
check('содержимое секций не потеряно', folded.indexOf('id="ggufOptCtxSize"') !== -1 &&
    folded.indexOf('id="ggufOptMainGpu"') !== -1 && folded.indexOf('id="ggufOptRpcServerPort"') !== -1);
check('шапка до первой секции осталась снаружи', folded.indexOf('class="form-actions"') < folded.indexOf('<details'));
check('id полей не изменились', /id="ggufOptCtxSize"/.test(folded));
check('пустой вход не ломает свёртку', M.collapseSections('') === '');
check('HTML без заголовков возвращается как есть', M.collapseSections('<div>x</div>') === '<div>x</div>');

// ── 2. Раскладка — отдельная секция формы ────────────────────────────────────
const cfg = {
    defaultCtxSize: 8192, defaultBatchSize: 512, defaultGpuLayers: -1,
    defaultFlashAttnType: -1, defaultNuma: false, defaultUseMmap: true,
    defaultUseMlock: false, defaultNThreads: 0, defaultRmsNormEps: 0.00001,
    autoGpuDistribution: true, defaultSplitMode: -1, defaultMainGpu: 0,
    defaultRpcBackend: 'cuda', defaultNoMemoryMap: false,
    rpcEnabled: true, rpcServers: ['192.0.2.10:50052'],
    rpcServerEnabled: true, rpcServerPort: 50052,
    defaultKvCacheType: '', defaultNoKvOffload: false,
    idleUnloadMinutes: 10, enableMetrics: true, metricsRetentionSeconds: 3600,
    defaultNParallel: 2, enableReasoning: false, reasoningBudget: 0,
    nodeName: 'cppworker-gpu', balancerUrl: 'http://loadbalancer:18081', uptime: '1h',
};
// Полный ответ API: config + ЖИВОЕ состояние узла (пункт 2).
const apiResp = {
    config: cfg,
    rpcServer: { enabled: true, running: true, pid: 134, bind: '0.0.0.0', port: 50052, listening: true, addr: '0.0.0.0:50052' },
};
const html = (function () {
    // renderBackendOptionsForm не экспортирован — рендерим через публичный путь:
    // вызываем ту же свёртку на реальном HTML формы, собранном модулем.
    // Для теста достаточно вызвать внутренний рендер через экспортированный
    // collapseSections и проверку структуры ниже; чтобы не дублировать рендер,
    // используем renderRpcServerState и проверку HTML формы напрямую.
    return null;
})();

check('секция раскладки вынесена из Multi-GPU (проверка по исходнику)', (function () {
    const fs = require('fs');
    const src = fs.readFileSync(require('path').join(__dirname, 'gguf-renderer-detail-render.js'), 'utf8');
    const multi = src.indexOf("gguf.tab_multi_gpu");
    const role = src.indexOf("gguf.tab_rpc_role");
    if (multi < 0 || role < 0) return false;
    // Между заголовком Multi-GPU и заголовком раскладки НЕ должно быть узловых
    // полей: они переехали в свою секцию.
    const between = src.slice(multi, role);
    return between.indexOf('ggufOptRpcServers') === -1 && between.indexOf('ggufOptRpcEnabled') === -1;
})(), 'поля раскладки всё ещё внутри Multi-GPU');

check('порт узла и галочка узла живут в секции раскладки', (function () {
    const fs = require('fs');
    const src = fs.readFileSync(require('path').join(__dirname, 'gguf-renderer-detail-render.js'), 'utf8');
    const role = src.indexOf("gguf.tab_rpc_role");
    const kv = src.indexOf("gguf.tab_kv_cache");
    const section = src.slice(role, kv);
    return section.indexOf('ggufOptRpcServerEnabled') !== -1 &&
        section.indexOf('ggufOptRpcServerPort') !== -1 &&
        section.indexOf('ggufOptRpcServers') !== -1 &&
        section.indexOf('ggufOptRpcEnabled') !== -1;
})(), 'в секции раскладки нет узловых полей');

// ── 3. Состояние узла показывается фактом ────────────────────────────────────
const listening = M.renderRpcServerState(apiResp.rpcServer);
check('слушающий узел: есть отметка и адрес', /is-ok/.test(listening) && listening.indexOf('0.0.0.0:50052') !== -1);
check('слушающий узел: показан pid', listening.indexOf('134') !== -1);

const starting = M.renderRpcServerState({ running: true, listening: false, port: 50052, addr: '0.0.0.0:50052', notes: ['порт открывается'] });
check('поднимающийся узел отличается от рабочего', /is-warn/.test(starting) && !/is-ok/.test(starting));

const stopped = M.renderRpcServerState({ enabled: false, running: false, listening: false, port: 50052 });
check('остановленный узел: is-off', /is-off/.test(stopped));

const failed = M.renderRpcServerState({ enabled: true, running: false, listening: false, lastError: 'exec: no such file' });
check('отказ узла показывает причину', failed.indexOf('no such file') !== -1);

const unknown = M.renderRpcServerState(undefined);
check('без данных — честное «нет данных», а не «работает»', /нет данных/.test(unknown) &&
    !/is-ok/.test(unknown) && !/is-off/.test(unknown));

const withRestarts = M.renderRpcServerState({ running: true, listening: true, restarts: 3, uptimeSec: 90 });
check('перезапуски и время работы видны', /3/.test(withRestarts) && /1m|90/.test(withRestarts));

// ── 4. Какие узлы РЕАЛЬНО активны (R125, пункт 3) ────────────────────────────
check('модуль экспортирует renderRpcPeers', typeof M.renderRpcPeers === 'function');

const peersHtml = M.renderRpcPeers({
    reachable: 1,
    total: 2,
    servers: [
        { addr: '192.0.2.10:50052', reachable: true, latencyMs: 3 },
        { addr: '192.0.2.11:50052', reachable: false, error: 'dial tcp: i/o timeout' },
    ],
});
check('активный узел показан как активный с задержкой', /is-ok/.test(peersHtml) &&
    peersHtml.indexOf('192.0.2.10:50052') !== -1 && peersHtml.indexOf('3') !== -1);
check('недоступный узел показан с причиной', /is-off/.test(peersHtml) &&
    peersHtml.indexOf('i/o timeout') !== -1);
check('сводка «активных узлов N из M»', /активных узлов: 1 \/ 2/.test(peersHtml));
check('без данных пробы — «не проверялись», а не «нет узлов»',
    /не проверялись/.test(M.renderRpcPeers(null)) && !/is-off/.test(M.renderRpcPeers(null)));
check('пустая раскладка — «узлов нет»', /узлов нет/.test(M.renderRpcPeers({ servers: [], total: 0 })));
// Честность: скорость обмена не выдумывается — в разметке нет «МБ/с».
check('пропускная способность не выдумывается', peersHtml.indexOf('МБ/с') === -1 &&
    peersHtml.indexOf('MB/s') === -1);

// R129: измеренная скорость обмена и честный отказ замера.
const speedHtml = M.renderRpcPeers({
    reachable: 1, total: 1,
    servers: [{ addr: '192.0.2.10:50052', reachable: true, latencyMs: 2 }],
    linkSpeed: [{ endpoint: 'http://192.0.2.10:18092/api/v1/cppworker/link-bench', mb: 8, durationMs: 700, mbps: 11.4 }],
});
check('измеренная скорость показана числом', /11\.4/.test(speedHtml) && /МБ\/с/.test(speedHtml));
const speedFailHtml = M.renderRpcPeers({
    reachable: 1, total: 1,
    servers: [{ addr: '10.0.0.5:50052', reachable: true }],
    linkSpeed: [{ endpoint: 'http://10.0.0.5:18092/x', mb: 8, mbps: 0, error: 'dial tcp: i/o timeout' }],
});
check('отказ замера показан причиной, а не «0 МБ/с»',
    /i\/o timeout/.test(speedFailHtml) && !/0 МБ\/с/.test(speedFailHtml));
check('без замера блок скорости не рисуется', M.renderRpcPeers({ reachable: 0, total: 0, servers: [] }).indexOf('МБ/с') === -1);

console.log('\nRESULT: ' + (failures === 0 ? 'PASS' : 'FAIL (' + failures + ')'));
process.exit(failures === 0 ? 0 : 1);
