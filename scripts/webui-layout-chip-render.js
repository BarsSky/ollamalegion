// scripts/webui-layout-chip-render.js — R118.17 (2026-10-15).
//
// ЖИВАЯ ПРОВЕРКА БЕЗ БРАУЗЕРА: берём РЕАЛЬНЫЕ ответы стенда, скармливаем их
// НАСТОЯЩИМ модулям WebUI (webui/js/modules/gguf-renderer-*.js) и печатаем текст
// чипа «раскладка» — тот, который браузер нарисовал бы на строке модели.
//
// ЗАЧЕМ. Браузер в этом окружении недоступен: Playwright не может создать
// страницу, а CDP-команды по TCP не отвечают (браузер стартует, соединение
// открывается, но Page.enable висит — см. tests/visual/layout-chip-live.spec.js и
// scripts/webui-layout-chip-check.js, они для обычного хоста). Здесь проверяется
// всё, кроме самого рендера в DOM: живые данные + живой код WebUI + реальные
// функции, из которых складывается чип.
//
// Запуск:
//   node scripts/webui-layout-chip-render.js
//   LB_URL=http://127.0.0.1:18081 CPPWORKER_URL=http://127.0.0.1:18092 \
//     WEBUI_API_TOKEN=<токен> node scripts/webui-layout-chip-render.js

'use strict';

const LB = process.env.LB_URL || 'http://127.0.0.1:18081';
const WORKER = process.env.CPPWORKER_URL || 'http://127.0.0.1:18092';
const TOKEN = process.env.WEBUI_API_TOKEN || 'changeme-bundled-with-agent-token';

// Минимальные глобалы, которые нужны модулям WebUI при загрузке.
global.window = global;
global.Utils = { escapeHtml: (s) => String(s === null || s === undefined ? '' : s) };
global.I18N = { t: (k) => k };
global.GGUF_DEBUG = false;

const state = {
    selectedBackendId: null,
    registeredBackends: [],
    detailPane: 'models',
    localModels: [],
    loadedModels: [],
    runtimeModels: {},
    rpcLayout: null,
    workerInfo: null,
    gpuInfo: null,
    diskInventory: {},
};
global.GgufModule = {
    state,
    _: (k) => k,
    stripGGUF: (n) => n,
    formatFileSize: () => '-',
    formatVramMB: (n) => String(n),
    showToast: () => {},
    refreshDetailPane: () => {},
    refreshDetailPanel: () => {},
};
global.GgufApi = {
    getRuntimeConfigViaBackend: () => Promise.resolve({ loaded_models: [] }),
    listLocalModelsViaBackend: () => Promise.resolve({ files: [] }),
    requestViaBackend: () => Promise.resolve({ files: [] }),
};

require('../webui/js/modules/gguf-renderer-refresh.js');
require('../webui/js/modules/gguf-renderer-detail-render.js');
const M = global.GgufModule;

async function getJSON(url) {
    const resp = await fetch(url, { headers: { 'X-API-Token': TOKEN } });
    if (!resp.ok) throw new Error(`${url} → HTTP ${resp.status}`);
    return resp.json();
}

(async () => {
    // 1. Живые данные: список бэкендов (там rpcLink/rpcLinkAuto/guard и загруженные модели).
    const backends = await getJSON(`${LB}/api/v1/backends`);
    const main = (backends.backends || []).find((b) => b.role === 'main' && (b.loadedModels || []).length);
    if (!main) {
        console.log('нет бэкенда с загруженными моделями — проверять нечего (загрузите модель)');
        process.exitCode = 2;
        return;
    }
    const backendId = main.id;

    // 2. Живые данные воркера: rpc_enabled/rpc_servers_active — из них WebUI
    //    собирает state.rpcLayout (см. applyLayoutState).
    const workerModels = await getJSON(`${WORKER}/api/models`);
    const link = main.rpcLink || null;
    const linkAuto = main.rpcLinkAuto || null;
    const guard = main.rpcLayoutGuard || null;
    state.rpcLayout = {
        active: !!workerModels.rpc_enabled,
        degraded: !!(guard && guard.degraded),
        degrade_reason: (guard && guard.reason) || '',
        nodes: Array.isArray(workerModels.rpc_servers_active) ? workerModels.rpc_servers_active : [],
        remote_nodes: workerModels.vram_remote_nodes || 0,
        vram_local_free_mb: workerModels.vram_local_free_mb || 0,
        vram_remote_free_mb: workerModels.vram_remote_free_mb || 0,
        link_mbps: (link && link.mbps) || 0,
        link_measured_at: (link && link.measured_at) || '',
        link_source: (link && link.source) || '',
        link_auto_mbps: (linkAuto && linkAuto.mbps) || 0,
        link_auto_at: (linkAuto && linkAuto.measured_at) || '',
        link_auto_node: (linkAuto && linkAuto.node) || '',
    };
    state.selectedBackendId = backendId;

    // 3. Живой runtime-конфиг воркера — так его видит WebUI (через прокси балансера).
    const runtime = await getJSON(`${LB}/api/v1/gguf/backends/${backendId}/proxy/api/v1/cppworker/config/runtime`);
    const map = {};
    for (const m of runtime.loaded_models || []) map[m.name] = m;
    state.runtimeModels = map;

    // 4. Настоящая функция WebUI: факт важнее расчёта.
    M.resolveDeviceLayers(state);

    console.log('=== чип «раскладка» из живых данных (без браузера) ===');
    console.log('бэкенд          :', backendId);
    console.log('rpc активна     :', state.rpcLayout.active, '| узлы:', state.rpcLayout.nodes.join(',') || '—');

    let failures = 0;
    for (const [name, rt] of Object.entries(map)) {
        const model = Object.assign({}, rt, { device_layers: rt.device_layers });
        const flags = M.buildLayoutFlags(model, state.rpcLayout);
        const chip = flags.filter((f) => f.label === 'раскладка')[0];
        if (!chip) {
            console.log(`  ${name}: чипа нет (нет данных раскладки)`);
            failures++;
            continue;
        }
        console.log(`  ${name}:`);
        console.log(`    значение : ${chip.value}`);
        console.log(`    подсказка: ${chip.title.slice(0, 160)}...`);
        const exact = /точный список из лога llama\.cpp/.test(chip.title);
        const devices = /CUDA\d+|RPC\d+/.test(chip.value);
        console.log(`    факт (не расчёт): ${exact ? 'PASS' : 'FAIL'} | устройства: ${devices ? 'PASS' : 'FAIL'}`);
        if (!exact || !devices) failures++;
    }
    process.exitCode = failures ? 1 : 0;
})().catch((e) => {
    console.error('ERR:', e.message);
    process.exitCode = 1;
});
