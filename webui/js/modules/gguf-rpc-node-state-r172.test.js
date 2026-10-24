// gguf-rpc-node-state-r172.test.js — R172 (2026-10-18).
//
// ЗАЧЕМ. Клиент раскладки на любую неудачную операцию узла пишет одно и то же —
// «Remote RPC server crashed or returned malformed response» (ggml-rpc.cpp:35), и
// по этому сообщению нельзя отличить «сервер разбился» от «сервер ответил false».
// R172 добавил в статус узла ФАКТЫ: как завершился прошлый процесс, поднимается
// ли узел сам, и последние строки самого rpc-server.
//
// Здесь проверяется, что карточка показывает эти факты, ничего не выдумывая:
//   1. exit-статус прошлого процесса виден (с pid), а не теряется;
//   2. «узел падал и поднимается заново» — отдельная подпись, не «остановлен»;
//   3. последние строки узла рендерятся ЭКРАНИРОВАННЫМИ внутри <details>;
//   4. старый ответ без новых полей рендерится как раньше (без ложных блоков).
//
// Запуск: node webui/js/modules/gguf-rpc-node-state-r172.test.js
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

let checks = 0;
function check(name, cond) {
    checks++;
    if (!cond) {
        console.error('FAIL: ' + name);
        process.exitCode = 1;
        return;
    }
    console.log('ok: ' + name);
}

const M = global.GgufModule;

// --- 1. exit-статус прошлого процесса -------------------------------------
const crashed = M.renderRpcServerState({
    enabled: true, running: false, listening: false, pid: 0,
    addr: '0.0.0.0:50052',
    lastExit: 'signal: segmentation fault (core dumped)',
    lastExitPid: 4242,
    lastExitAt: new Date(Date.now() - 60000).toISOString(),
    restarts: 2,
    restartEnabled: true,
    restarting: true,
    restartInMs: 1500,
    logLines: 812, logDropped: 0,
    logTail: [
        'Accepted client connection',
        'ggml-rpc: [server] tensor region out of buffer bounds',
    ],
});
check('exit-статус прошлого процесса показан', crashed.indexOf('segmentation fault') !== -1);
check('pid упавшего процесса показан', crashed.indexOf('4242') !== -1);
check('подпись «прошлый процесс завершился» присутствует',
    crashed.indexOf('прошлый процесс завершился') !== -1);

// --- 2. «поднимается заново» — не «остановлен» ----------------------------
check('поднимающийся узел подписан отдельно', crashed.indexOf('поднимается заново') !== -1);
check('поднимающийся узел НЕ подписан как остановленный', crashed.indexOf('остановлен') === -1);
check('пауза до подъёма показана в секундах', crashed.indexOf('через') !== -1 && crashed.indexOf('2 с') !== -1);
check('счётчик перезапусков показан', crashed.indexOf('перезапусков: 2') !== -1);

// --- 3. строки лога узла: <details> + экранирование ------------------------
check('строки узла в свёрнутом блоке', crashed.indexOf('<details class="gguf-rpc-state-log"') !== -1);
check('строки узла в <pre>', crashed.indexOf('<pre>') !== -1);
check('серверная причина отказа видна', crashed.indexOf('tensor region out of buffer bounds') !== -1);

const injected = M.renderRpcServerState({
    enabled: true, running: false, listening: false,
    logTail: ['<script>alert(1)</script>'],
});
check('строки лога экранированы (нет сырого <script>)', injected.indexOf('<script>alert') === -1);
check('строки лога экранированы (есть &lt;script&gt;)', injected.indexOf('&lt;script&gt;') !== -1);

// --- 4. вытеснение лога и внешний узел ------------------------------------
const dropped = M.renderRpcServerState({
    enabled: true, running: true, listening: true, addr: '10.0.0.2:50055',
    external: true, debug: true, logLines: 2000, logDropped: 37,
});
check('вытеснение лога отмечено', dropped.indexOf('в буфере только хвост лога') !== -1);
check('внешний узел подписан', dropped.indexOf('внешний узел') !== -1);
check('отладочный режим подписан', dropped.indexOf('отладка протокола') !== -1);

// --- 5. обратная совместимость: старый ответ без новых полей --------------
const legacy = M.renderRpcServerState({
    enabled: true, running: true, listening: true, pid: 134,
    bind: '0.0.0.0', port: 50052, addr: '0.0.0.0:50052',
});
check('старый ответ: статус слушает', legacy.indexOf('слушает') !== -1);
check('старый ответ: нет блока exit', legacy.indexOf('прошлый процесс завершился') === -1);
check('старый ответ: нет блока лога', legacy.indexOf('gguf-rpc-state-log') === -1);
check('старый ответ: нет выдуманного «поднимается»', legacy.indexOf('поднимается заново') === -1);

// --- 6. отсутствие данных честно ------------------------------------------
const unknown = M.renderRpcServerState(undefined);
check('нет данных — так и написано', unknown.indexOf('нет данных') !== -1);

console.log('\nвсего проверок: ' + checks + (process.exitCode ? ' — ЕСТЬ ПАДЕНИЯ' : ' — все прошли'));
