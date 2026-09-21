#!/usr/bin/env node
/**
 * test_webui_wiring_r65d.js — R65d (2026-09-20): регрессия на проводку WebUI.
 *
 * Проверяет дефекты, найденные аудитом 2026-09-20 и исправленные в R65d:
 *
 *  1. `window.API` (ЗАГЛАВНЫМИ) не существует — api.js экспортирует `window.Api`.
 *     Из-за этого молча не работали: Load/Unload/Delete Selected
 *     (bulk-models.js), сохранение AutoTune (autotune_settings.js),
 *     AutoTune-карточки и список бэкендов (renderers.js).
 *     Раньше падало как gateway-ошибка `TypeError: Cannot read properties of
 *     undefined (reading 'bulk')`, либо молча через `if (!window.API) return`.
 *
 *  2. `window.Api.authHeaders` — метода нет, есть `getAuthHeaders()`
 *     (autotune_history.js → 401 на истории AutoTune).
 *
 *  3. `GgufApi.cancelGeneration` / `GgufApi.activeQueries` — методов не было,
 *     поэтому кнопка Cancel (Round 32) и busy-badge не работали вообще:
 *     gguf-renderer-actions.js:164 и gguf-renderer-refresh.js:247 проверяли
 *     typeof и немедленно выходили.
 *
 *  4. Прокси-путь /api/v1/gguf/backends/ защищён AuthMiddleware (R65d), поэтому
 *     buildBackendProxyUrl ОБЯЗАН добавлять ?token= — иначе SSE-прогресс
 *     загрузки (EventSource не умеет заголовки) получает 401.
 *
 * Запуск: node webui/tests/test_webui_wiring_r65d.js
 */

'use strict';

const fs = require('fs');
const path = require('path');

let testsRun = 0;
let testsPassed = 0;
let testsFailed = 0;

function assert(cond, msg) {
    testsRun++;
    if (cond) {
        testsPassed++;
        console.log('  \u2713 ' + msg);
    } else {
        testsFailed++;
        console.error('  \u2717 ' + msg);
    }
}

function section(name) {
    console.log('\n=== ' + name + ' ===');
}

const WEBUI_DIR = path.join(__dirname, '..', 'js');
const MODULES_DIR = path.join(WEBUI_DIR, 'modules');

function read(rel) {
    return fs.readFileSync(path.join(WEBUI_DIR, rel), 'utf8');
}

function readModule(rel) {
    // rel передаётся без префикса "modules/" — путь строится от WEBUI_DIR/js.
    return fs.readFileSync(path.join(WEBUI_DIR, rel), 'utf8');
}

// ---------------------------------------------------------------------------
section('1. window.API -> window.Api (несуществующее пространство имён)');

const filesThatMustUseLowercaseApi = [
    'modules/bulk-models.js',
    'modules/autotune_settings.js',
    'modules/renderers.js',
    'modules/autotune_history.js',
];

filesThatMustUseLowercaseApi.forEach(function (rel) {
    const src = readModule(rel);
    // Разрешаем упоминание window.API только в комментариях/доках.
    const codeLines = src.split('\n').filter(function (line) {
        const t = line.trim();
        if (t.startsWith('*') || t.startsWith('//') || t.startsWith('/*')) return false;
        return true;
    });
    const code = codeLines.join('\n');
    assert(code.indexOf('window.API') === -1,
        rel + ': нет обращений к несуществующему window.API');
    assert(code.indexOf('window.Api') !== -1,
        rel + ': использует корректный window.Api');
});

section('2. authHeaders -> getAuthHeaders');

(function () {
    const src = readModule('modules/autotune_history.js');
    assert(src.indexOf('.authHeaders') === -1,
        'autotune_history.js: нет вызова несуществующего .authHeaders()');
    assert(src.indexOf('.getAuthHeaders') !== -1,
        'autotune_history.js: использует getAuthHeaders()');

    const api = readModule('modules/api.js');
    assert(/getAuthHeaders\s*\(/.test(api),
        'api.js: getAuthHeaders() экспортирован');
    assert(!/^\s*authHeaders\s*\(/m.test(api),
        'api.js: нет метода authHeaders (только getAuthHeaders)');
})();

section('3. GgufApi.cancelGeneration / activeQueries');

(function () {
    const src = readModule('modules/gguf-api.js');
    assert(/async\s+cancelGeneration\s*\(/.test(src),
        'gguf-api.js: cancelGeneration определён');
    assert(/async\s+activeQueries\s*\(/.test(src),
        'gguf-api.js: activeQueries определён');

    // Контракт, который ожидают потребители.
    const actions = readModule('modules/gguf-renderer-actions.js');
    assert(actions.indexOf('api.cancelGeneration(') !== -1,
        'gguf-renderer-actions.js: вызывает api.cancelGeneration(...)');

    const refresh = readModule('modules/gguf-renderer-refresh.js');
    assert(refresh.indexOf('api.activeQueries(') !== -1,
        'gguf-renderer-refresh.js: вызывает api.activeQueries(...)');
    assert(refresh.indexOf('data.queries') !== -1,
        'gguf-renderer-refresh.js: ожидает агрегат {queries: {...}}');

    // activeQueries должен возвращать именно {queries: {...}}, иначе busy-badge
    // снова перестанет обновляться.
    assert(/return\s*\{\s*queries:/.test(src),
        'gguf-api.js: activeQueries возвращает {queries: {...}}');

    // Не должно остаться вызова несуществующего this.getBackends() в КОДЕ
    // (упоминание в комментарии-объяснении допустимо).
    const ggufCode = src.split('\n').filter(function (line) {
        const t = line.trim();
        return !(t.startsWith('*') || t.startsWith('//') || t.startsWith('/*'));
    }).join('\n');
    assert(ggufCode.indexOf('this.getBackends') === -1,
        'gguf-api.js: нет вызова несуществующего this.getBackends()');
})();

section('4. Токен в URL прокси (SSE / EventSource)');

(function () {
    const src = readModule('modules/gguf-api.js');
    // Находим тело buildBackendProxyUrl.
    const m = src.match(/buildBackendProxyUrl\s*\([^)]*\)\s*\{([\s\S]*?)\n\s{8}\}/);
    assert(!!m, 'gguf-api.js: buildBackendProxyUrl найден');
    if (m) {
        const body = m[1];
        assert(body.indexOf('token=') !== -1,
            'buildBackendProxyUrl добавляет ?token= (нужно для EventSource)');
        assert(body.indexOf('apiToken') !== -1,
            'buildBackendProxyUrl использует apiToken');
    }

    const progress = readModule('modules/gguf-load-progress.js');
    assert(progress.indexOf('token=') !== -1,
        'gguf-load-progress.js fallback-URL тоже добавляет ?token=');
})();

section('5. fetchClusterState обогащает бэкенды конфигом');

(function () {
    const api = readModule('modules/api.js');
    assert(/async\s+backends\s*\(/.test(api),
        'api.js: метод backends() (GET /api/v1/backends) добавлен');
    assert(/async\s+backend\s*\(/.test(api),
        'api.js: метод backend(id) добавлен');

    const app = read('app.js');
    assert(app.indexOf('/api/v1/backends') !== -1 || app.indexOf('Api.backends()') !== -1,
        'app.js: fetchClusterState использует Api.backends()');
    assert(app.indexOf('enrichBackendsWithConfig') !== -1,
        'app.js: enrichBackendsWithConfig определён и вызван');
    // Поля, которые рендерят колонки страницы бэкендов.
    ['weight', 'labels', 'maxModels', 'autoTune'].forEach(function (f) {
        assert(app.indexOf('merged.' + f) !== -1,
            'app.js: переносит поле ' + f + ' из конфига бэкенда');
    });
})();

section('6. PUT /api/v1/cluster/config: payload соответствует серверу');

(function () {
    const app = read('app.js');

    // Секция `agent` не должна отправляться: сервер не может её применить
    // (конфигурация отдельного процесса cmd/agent).
    const agentPayload = /agent:\s*\{\s*\n\s*collectInterval:/.test(app);
    assert(!agentPayload,
        'app.js: секция `agent` не отправляется в PUT (сервер её не применяет)');

    // apiToken не отправляется — серверного поля нет.
    assert(!/^\s*apiToken:\s*apiToken,/m.test(app),
        'app.js: apiToken не отправляется в PUT');

    // Секции, которые сервер Теперь принимает, должны отправляться.
    ['modelReplication', 'rpcCoordinator', 'virtualModels', 'distInference'].forEach(function (s) {
        assert(new RegExp(s + ':\\s*\\{').test(app),
            'app.js: секция ' + s + ' отправляется в PUT');
    });

    // Поля llamaCpp, добавленные в types.LlamaCppConfig, должны отправляться.
    ['flashAttnType', 'splitMode', 'idleUnloadMinutes', 'enableMetrics',
     'metricsRetentionSeconds', 'enableReasoning', 'reasoningBudget'].forEach(function (f) {
        assert(new RegExp('^\\s*' + f + ':', 'm').test(app),
            'app.js: llamaCpp.' + f + ' отправляется в PUT');
    });

    // verifySync должен проверять секции (иначе молчаливый откат не заметен).
    assert(/sectionFields/.test(app),
        'app.js: verifySync проверяет секции (ловит молчаливый откат)');
    assert(/llamaCpp\.' \+ field|llamaCpp\.\[field\]/.test(app),
        'app.js: verifySync проверяет поля llamaCpp');
})();

section('7. Форма бэкенда: cppWorkerPort загружается');

(function () {
    const modals = read('app-modals.js');
    assert(/formBackendCppWorkerPort/.test(modals) && /cppWorkerPort\) \|\| 18092/.test(modals),
        'app-modals.js fillForm: заполняет formBackendCppWorkerPort ' +
        '(иначе Save перезаписывал порт дефолтом 18092)');
    const payload = modals.match(/var payload = \{([\s\S]*?)\};/);
    assert(!!payload, 'app-modals.js: payload собран');
    if (payload) {
        assert(!/^\s*id:/m.test(payload[1]),
            'app-modals.js payload: `id` не отправляется (сервер берёт его из URL)');
        assert(!/grpcPort/.test(payload[1]),
            'app-modals.js payload: grpcPort не отправляется (нет в серверной структуре)');
    }
})();

section('8. Синтаксис (node --check)');

(function () {
    const { execFileSync } = require('child_process');
    const targets = [
        'app.js',
        'modules/api.js',
        'modules/gguf-api.js',
        'modules/gguf-load-progress.js',
        'modules/bulk-models.js',
        'modules/autotune_settings.js',
        'modules/autotune_history.js',
        'modules/renderers.js',
    ];
    targets.forEach(function (rel) {
        const full = path.join(WEBUI_DIR, rel);
        let ok = true;
        try {
            execFileSync(process.execPath, ['--check', full], { stdio: 'pipe' });
        } catch (e) {
            ok = false;
            console.error('    ' + rel + ': ' + String(e.stderr || e.message).split('\n')[0]);
        }
        assert(ok, rel + ': синтаксис валиден');
    });
})();

// ---------------------------------------------------------------------------
console.log('\n' + '='.repeat(60));
console.log('Tests: ' + testsRun + '  passed: ' + testsPassed + '  failed: ' + testsFailed);
console.log('='.repeat(60));

process.exit(testsFailed === 0 ? 0 : 1);

