// setup-wizard-presets.test.js — verifies HardwarePresets registry and apply logic.
//
// R58.3 (2026-09-03): TDD for setup-wizard preset selector.
//
// Run: node webui/js/modules/setup-wizard-presets.test.js
//
// Tests:
//  1. window.HardwarePresets is registered (IIFE runs)
//  2. All 4 presets are in `presets` map
//  3. `order` array has 4 keys in expected order
//  4. `get(key)` returns the right preset or null
//  5. `list()` returns array of {key, label, description} in order
//  6. Each preset has required fields: vramMaxUsage, gpuMaxUsage, etc.

const assert = require('assert');

// Mock window + I18N environment (since IIFE references window.HardwarePresets)
global.window = global;
require('./setup-wizard-presets.js');

const P = global.HardwarePresets;

assert.ok(P, 'window.HardwarePresets should be registered');
assert.ok(P.presets, 'presets map should exist');
assert.ok(Array.isArray(P.order), 'order array should exist');

// Test 1: All 4 presets exist
['rtx30-8gb', 'rtx40-24gb', 'a10-24gb', 'rtx50-32gb'].forEach(function(key) {
    assert.ok(P.presets[key], `preset ${key} should exist`);
    const p = P.presets[key];
    assert.ok(p.label, `${key} should have label`);
    assert.ok(p.description, `${key} should have description`);
    assert.ok(typeof p.vramMaxUsage === 'number', `${key} should have numeric vramMaxUsage`);
    assert.ok(typeof p.gpuMaxUsage === 'number', `${key} should have numeric gpuMaxUsage`);
    assert.ok(typeof p.cpuMaxUsage === 'number', `${key} should have numeric cpuMaxUsage`);
    assert.ok(typeof p.ramMaxUsage === 'number', `${key} should have numeric ramMaxUsage`);
    assert.ok(p.cudaArch, `${key} should have cudaArch`);
    assert.ok(p.recommendedModels, `${key} should have recommendedModels`);
    assert.ok(p.cppworkerHint && typeof p.cppworkerHint === 'object', `${key} should have cppworkerHint object`);
});

// Test 2: order has all 4 keys
assert.strictEqual(P.order.length, 4, 'order should have 4 entries');
['rtx50-32gb', 'rtx40-24gb', 'a10-24gb', 'rtx30-8gb'].forEach(function(key) {
    assert.ok(P.order.indexOf(key) >= 0, `order should contain ${key}`);
});

// Test 3: get() returns the right preset
const rtx = P.get('rtx30-8gb');
assert.ok(rtx, 'get(rtx30-8gb) should return preset');
assert.strictEqual(rtx.label, 'RTX 30xx 8GB');
assert.strictEqual(rtx.cudaArch, '86');

// Test 4: get() returns null for unknown
assert.strictEqual(P.get('unknown'), null);
assert.strictEqual(P.get(''), null);

// Test 5: list() returns array in order
const list = P.list();
assert.strictEqual(list.length, 4);
assert.strictEqual(list[0].key, 'rtx50-32gb');  // newest first
assert.strictEqual(list[3].key, 'rtx30-8gb');
assert.ok(list[0].label);
assert.ok(list[0].description);

// Test 6: cppworkerHint has expected keys
['CPPWORKER_CTX_SIZE', 'CPPWORKER_BATCH_SIZE', 'CPPWORKER_GPU_LAYERS', 'CPPWORKER_KV_CACHE_TYPE'].forEach(function(k) {
    assert.ok(P.presets['rtx40-24gb'].cppworkerHint[k], `rtx40-24gb should have cppworkerHint.${k}`);
});

// Test 7: Reasonable vramMaxUsage (higher VRAM → more aggressive usage)
const rtx30vram = P.presets['rtx30-8gb'].vramMaxUsage;
const rtx50vram = P.presets['rtx50-32gb'].vramMaxUsage;
assert.ok(rtx50vram >= rtx30vram, 'rtx50-32gb should have higher vramMaxUsage than rtx30-8gb');

console.log('OK: all', Object.keys(P.presets).length, 'presets pass validation');

// ============================================================
// R106 (S5 плана 2026-10-07): пресеты РАСКЛАДКИ
// ============================================================
//
// Уточнение к плану: этот файл — про ЖЕЛЕЗО, пресетов режимов в нём не было.
// Набор раскладки добавлен как самостоятельный, и проверяем мы его отдельно:
// стратегии обязаны совпадать с теми, что исполняет балансер, а ключи i18n —
// существовать в ОБОИХ языках (иначе мастер покажет сырой ключ).

require('../i18n/ru.js');
require('../i18n/en.js');
const RU = global.I18N_RU;
const EN = global.I18N_EN;

assert.ok(P.placementPresets, 'placementPresets map should exist');
assert.ok(Array.isArray(P.placementOrder), 'placementOrder should exist');
assert.strictEqual(P.placementOrder.length, 5, 'раскладок должно быть пять');

const VALID_STRATEGIES = ['single', 'replicated', 'sharded', 'pool'];
const VALID_GROUP_STRATEGIES = ['', 'layer', 'row'];

P.placementOrder.forEach(function (key) {
    const p = P.placementPresets[key];
    assert.ok(p, `раскладка ${key} должна существовать`);
    assert.ok(p.label, `${key}: нужен label`);
    assert.ok(VALID_STRATEGIES.indexOf(p.strategy) >= 0,
        `${key}: стратегия ${p.strategy} не исполняется балансером`);
    const gs = p.groupStrategy || '';
    assert.ok(VALID_GROUP_STRATEGIES.indexOf(gs) >= 0, `${key}: неизвестная стратегия группы ${gs}`);
    assert.ok(p.labelKey && RU[p.labelKey] && EN[p.labelKey],
        `${key}: labelKey ${p.labelKey} должен быть переведён на оба языка`);
    assert.ok(p.hintKey && RU[p.hintKey] && EN[p.hintKey],
        `${key}: hintKey ${p.hintKey} должен быть переведён на оба языка`);
});

// listPlacements: match-ключи различают layer и row у sharded (иначе две разные
// раскладки выглядели бы одной).
const placements = P.listPlacements();
assert.strictEqual(placements.length, 5);
const matches = placements.map(function (p) { return p.match; }).sort();
assert.deepStrictEqual(matches, ['pool', 'replicated', 'sharded:layer', 'sharded:row', 'single']);

assert.strictEqual(P.matchKey('sharded', 'row'), 'sharded:row');
assert.strictEqual(P.matchKey('sharded'), 'sharded');
assert.strictEqual(P.matchKey('single', ''), 'single');

assert.ok(P.getPlacement('sharded-layer'), 'getPlacement должен находить набор');
assert.strictEqual(P.getPlacement('unknown'), null);
assert.strictEqual(P.getPlacement(''), null);

console.log('OK:', P.listPlacements().length, 'пресетов раскладки проверено');
