// i18n-node20-env-r734.test.js — CI-гейт: i18n обязан загружаться там, где нет
// `localStorage` и `navigator` (Node 20 на раннере GitHub Actions).
//
// ЖИВОЙ ДЕФЕКТ (2026-11-24). CI-джоба «WebUI JS tests (all *.test.js)» краснела
// на двух тестах, ЛОКАЛЬНО зелёных:
//
//   at detectLanguage (.../webui/js/i18n/index.js:31:23)
//   TypeError: Cannot read properties of undefined (reading 'getItem')
//   --- webui/tests/image-hf-i18n-r694.test.js (код 1)
//   --- webui/tests/image-role-label-r695.test.js (код 1)
//
// Причина: `detectLanguage` читал `localStorage.getItem(...)` и
// `navigator.language` как ГЛОБАЛЫ. В Node 22 (локально) они есть, в Node 20 (CI)
// нет. Тесты этих файлов стабилизировали `localStorage`, но не `navigator` — и
// падали только в CI.
//
// ЧТО ПРОВЕРЯЕТ ЭТОТ ТЕСТ. Он НЕ полагается на глобалы текущего Node: модуль
// исполняется в отдельном vm-контексте, где `localStorage`/`navigator` вообще не
// объявлены. Так воспроизводится окружение CI независимо от версии раннера:
// тест, написанный «на своём Node 22», снова пропустил бы дефект.
'use strict';

const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const WEBUI_DIR = path.join(__dirname, '..');

function readSource(rel) {
    return fs.readFileSync(path.join(WEBUI_DIR, rel), 'utf8');
}

// runI18nInSandbox — исполнить пакеты и index.js в изолированном контексте.
// extra: необязательные "браузерные" глобалы (navigator/localStorage).
function runI18nInSandbox(extra) {
    const sandbox = {
        console: { log: function () {}, warn: function () {}, error: function () {} },
        // Фиксированная зона: иначе geo-heuristic возьмёт таймзону ХОСТА и тест
        // «по умолчанию en» станет зависеть от машины разработчика.
        Intl: { DateTimeFormat: function () { return { resolvedOptions: function () { return { timeZone: 'UTC' }; } }; } },
        document: {
            documentElement: {
                attrs: {},
                setAttribute: function (k, v) { this.attrs[k] = v; },
                getAttribute: function (k) { return this.attrs[k]; },
            },
            querySelector: function () { return null; },
            querySelectorAll: function () { return []; },
            addEventListener: function () {},
        },
        addEventListener: function () {},
        removeEventListener: function () {},
        dispatchEvent: function () { return true; },
        CustomEvent: function (type, init) { this.type = type; this.detail = init && init.detail; },
    };
    sandbox.window = sandbox;
    if (extra) Object.keys(extra).forEach(function (k) { sandbox[k] = extra[k]; });

    vm.createContext(sandbox);
    // Порядок как в странице: пакеты объявляют window.I18N_EN/RU, index.js их
    // регистрирует и выбирает язык. Ни localStorage, ни navigator здесь НЕТ.
    ['js/i18n/en.js', 'js/i18n/ru.js', 'js/i18n/index.js'].forEach(function (rel) {
        vm.runInContext(readSource(rel), sandbox, { filename: rel });
    });
    return sandbox;
}

test('R734: i18n грузится без localStorage и navigator (как в CI на Node 20)', function () {
    const sandbox = runI18nInSandbox(null);
    const i18n = sandbox.window.I18N;
    assert.ok(i18n, 'window.I18N не создан — модуль упал на отсутствии глобалов');
    assert.strictEqual(typeof i18n.getLang, 'function', 'нет getLang');
    // Без подсказок окружения — дефолт en; главное здесь то, что загрузка не падает.
    assert.strictEqual(i18n.getLang(), 'en', 'без navigator/localStorage ожидался дефолт en');
    assert.strictEqual(i18n.t('common.loading'), sandbox.window.I18N_EN['common.loading'],
        'перевод из пакета не резолвится после загрузки без глобалов');
});

test('R734: navigator.language=ru по-прежнему выбирает ru', function () {
    const sandbox = runI18nInSandbox({ navigator: { language: 'ru-RU', languages: ['ru-RU', 'en'] } });
    assert.strictEqual(sandbox.window.I18N.getLang(), 'ru',
        'язык из navigator.language перестал учитываться');
});

test('R734: сохранённый выбор (localStorage) важнее navigator', function () {
    const store = { ollamalegion_lang: 'en' };
    const sandbox = runI18nInSandbox({
        navigator: { language: 'ru-RU', languages: ['ru-RU'] },
        localStorage: {
            getItem: function (k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
            setItem: function (k, v) { store[k] = String(v); },
        },
    });
    assert.strictEqual(sandbox.window.I18N.getLang(), 'en',
        'сохранённый выбор перестал перебивать язык браузера');
});

test('R734: setLang без localStorage не падает', function () {
    const sandbox = runI18nInSandbox(null);
    assert.doesNotThrow(function () {
        sandbox.window.I18N.setLang('ru');
    }, 'setLang упал на отсутствии localStorage — в CI это отдельный красный тест');
    assert.strictEqual(sandbox.window.I18N.getLang(), 'ru');
});
