#!/usr/bin/env node
/**
 * test_gguf_api_headers.js — регрессионный тест для Session 17 P.4 (2026-07-27).
 *
 * Баг: `requestViaBackend()` в `webui/js/modules/gguf-api.js` строил fetch options так:
 *   { headers: headersWithToken, ...options }
 * В JS object spread перезаписывает свойства слева направо. Если caller передал
 * `options.headers = { 'Content-Type': 'application/json' }`, наш X-API-Token
 * стирался → HTTP 401 "invalid or missing API token" на per-backend save.
 *
 * Этот тест имитирует логику `requestViaBackend` через mock-fetch и проверяет,
 * что X-API-Token доходит до fetch().
 *
 * Запуск: node webui/tests/test_gguf_api_headers.js
 *   или: cd webui/tests && node test_gguf_api_headers.js
 */

'use strict';

let testsRun = 0;
let testsPassed = 0;
let testsFailed = 0;

function assert(cond, msg) {
    testsRun++;
    if (cond) {
        testsPassed++;
        console.log('  ✓ ' + msg);
    } else {
        testsFailed++;
        console.error('  ✗ ' + msg);
    }
}

function section(name) {
    console.log('\n=== ' + name + ' ===');
}

/**
 * Копия исправленной логики из gguf-api.js::requestViaBackend
 * (с явным мержем, БЕЗ `...options` поверх headers).
 * Держим в синхронизации с webui/js/modules/gguf-api.js!
 */
function buildFetchOptions(apiToken, hfToken, path, options) {
    const headers = { 'Content-Type': 'application/json', ...(options.headers || {}) };
    if (apiToken) {
        headers['X-API-Token'] = apiToken;
    }
    if (hfToken && (path === '/api/hf/search' || path.indexOf('/api/hf/files') === 0 || path === '/api/hf/download')) {
        headers['X-HF-Token'] = hfToken;
    }
    // ВАЖНО: не spread'им options после headers (Session 17 P.4)
    const fetchOptions = {
        method: options.method,
        headers: headers,
        signal: options.signal || null,
    };
    if (options.body !== undefined) {
        fetchOptions.body = options.body;
    }
    return fetchOptions;
}

/**
 * Копия СТАРОЙ (баговой) логики — для контраста.
 * Должна провалить тест.
 */
function buildFetchOptions_BUGGY(apiToken, hfToken, path, options) {
    const headers = { 'Content-Type': 'application/json', ...(options.headers || {}) };
    if (apiToken) {
        headers['X-API-Token'] = apiToken;
    }
    if (hfToken && (path === '/api/hf/search' || path.indexOf('/api/hf/files') === 0 || path === '/api/hf/download')) {
        headers['X-HF-Token'] = hfToken;
    }
    // СТАРАЯ ЛОГИКА: ...options перезаписывает headers
    return {
        headers: headers,
        ...options,
    };
}

// =============================================================================
// Tests
// =============================================================================

section('1. FIXED buildFetchOptions: X-API-Token preserved with options.headers');

{
    const opts = buildFetchOptions('test-token-abc', null, '/api/v1/cppworker/config/update', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ defaultGpuLayers: -2 }),
    });
    assert(opts.headers['X-API-Token'] === 'test-token-abc', 'X-API-Token присутствует в headers');
    assert(opts.headers['Content-Type'] === 'application/json', 'Content-Type от caller сохранён');
    assert(opts.method === 'PUT', 'method сохранён');
    assert(typeof opts.body === 'string', 'body сохранён (string JSON)');
}

section('2. FIXED buildFetchOptions: X-HF-Token preserved for HF endpoints');

{
    const opts = buildFetchOptions(null, 'hf-secret-xyz', '/api/hf/search', {
        method: 'GET',
        headers: { 'Accept': 'application/json' },
    });
    assert(opts.headers['X-HF-Token'] === 'hf-secret-xyz', 'X-HF-Token присутствует для /api/hf/search');
    assert(opts.headers['Accept'] === 'application/json', 'caller Accept header сохранён');
}

section('3. FIXED buildFetchOptions: token НЕ передаётся если apiToken пустой');

{
    const opts = buildFetchOptions('', null, '/api/v1/cppworker/config', {
        method: 'GET',
    });
    assert(!('X-API-Token' in opts.headers), 'X-API-Token отсутствует когда apiToken пустой');
}

section('4. BUGGY buildFetchOptions: показываем что баг воспроизводился');

{
    const opts = buildFetchOptions_BUGGY('test-token-abc', null, '/api/v1/cppworker/config/update', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
    });
    assert(opts.headers['X-API-Token'] === undefined, 'BUGGY версия теряет X-API-Token (это и был наш 401)');
    assert(opts.headers['Content-Type'] === 'application/json', 'BUGGY версия сохраняет Content-Type');
}

section('5. FIXED buildFetchOptions: caller без headers работает');

{
    const opts = buildFetchOptions('test-token', null, '/api/v1/cppworker/config/runtime', {
        method: 'GET',
    });
    assert(opts.headers['X-API-Token'] === 'test-token', 'X-API-Token есть когда caller не передавал headers');
    assert(opts.headers['Content-Type'] === 'application/json', 'default Content-Type применён');
}

section('6. FIXED buildFetchOptions: body undefined не попадает в fetch options');

{
    const opts = buildFetchOptions('test-token', null, '/api/v1/cppworker/config', {
        method: 'GET',
    });
    assert(!('body' in opts), 'body отсутствует в GET запросе (не undefined)');
}

// =============================================================================
// i18n completeness test
// =============================================================================

section('7. i18n completeness: все gguf.* ключи из всех WebUI JS есть в en.js и ru.js');

{
    const fs = require('fs');
    const path = require('path');

    const enPath = path.join(__dirname, '..', 'js', 'i18n', 'en.js');
    const ruPath = path.join(__dirname, '..', 'js', 'i18n', 'ru.js');

    if (!fs.existsSync(enPath) || !fs.existsSync(ruPath)) {
        assert(false, 'не найдены en.js / ru.js — тест не может проверить');
    } else {
        const en = fs.readFileSync(enPath, 'utf-8');
        const ru = fs.readFileSync(ruPath, 'utf-8');

        // Сканируем все .js файлы в webui/js/ на наличие _("gguf.XXX")
        const jsFiles = [];
        function walk(dir) {
            for (const f of fs.readdirSync(dir, { withFileTypes: true })) {
                const p = path.join(dir, f.name);
                if (f.isDirectory()) walk(p);
                else if (f.name.endsWith('.js') && !f.name.endsWith('.min.js')) jsFiles.push(p);
            }
        }
        walk(path.join(__dirname, '..', 'js'));

        // Извлекаем все используемые gguf.* ключи
        const usedKeys = new Set();
        const re = /_\(\s*['"](gguf\.[a-z0-9_]+)['"]/g;
        let m;
        for (const f of jsFiles) {
            const content = fs.readFileSync(f, 'utf-8');
            while ((m = re.exec(content)) !== null) {
                usedKeys.add(m[1]);
            }
        }

        // Извлекаем все ключи из en.js/ru.js
        function collectI18nKeys(content) {
            const out = new Set();
            const re2 = /['"](gguf\.[a-z0-9_]+)['"]\s*:/g;
            let m2;
            while ((m2 = re2.exec(content)) !== null) {
                out.add(m2[1]);
            }
            return out;
        }

        const enKeys = collectI18nKeys(en);
        const ruKeys = collectI18nKeys(ru);

        const missingInEn = [...usedKeys].filter(k => !enKeys.has(k));
        const missingInRu = [...usedKeys].filter(k => !ruKeys.has(k));

        assert(missingInEn.length === 0,
            `в en.js нет ключей: ${missingInEn.length === 0 ? '(none)' : missingInEn.join(', ')}`);
        assert(missingInRu.length === 0,
            `в ru.js нет ключей: ${missingInRu.length === 0 ? '(none)' : missingInRu.join(', ')}`);

        // Session 17 P.4 specific: strategy_auto и rope_scaling_factor_desc
        // должны быть в обоих файлах (регрессия для конкретного бага)
        assert(enKeys.has('gguf.strategy_auto'),
            'regression: gguf.strategy_auto присутствует в en.js (Session 17 P.4 fix)');
        assert(ruKeys.has('gguf.strategy_auto'),
            'regression: gguf.strategy_auto присутствует в ru.js (Session 17 P.4 fix)');
        assert(enKeys.has('gguf.rope_scaling_factor_desc'),
            'regression: gguf.rope_scaling_factor_desc присутствует в en.js (Session 17 P.4 fix)');
        assert(ruKeys.has('gguf.rope_scaling_factor_desc'),
            'regression: gguf.rope_scaling_factor_desc присутствует в ru.js (Session 17 P.4 fix)');
    }
}

// =============================================================================
// Summary
// =============================================================================

console.log('\n=== SUMMARY ===');
console.log(`Tests run:    ${testsRun}`);
console.log(`Tests passed: ${testsPassed}`);
console.log(`Tests failed: ${testsFailed}`);

if (testsFailed > 0) {
    process.exit(1);
}
process.exit(0);
