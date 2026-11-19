// settings-inert-fields-r544.test.js — R544 (F10 §1.9 плана
// 2026-11-19-webui-and-balancer-fixes; W6 и §4.8 плана
// 2026-11-19-webui-pages-and-setup): в «Настройках» не осталось полей,
// которые ничего не делают.
//
// ЖИВОЙ ДЕФЕКТ, который здесь закрыт. Оператор видел в «Настройках» пять
// полей, которых не существует как настройки:
//   - #apiToken «API Токен»: saveSettings его не читает (R65d), сервер не
//     принимает поле в PUT и не отдаёт в обычном GET → поле ВСЕГДА пустое;
//   - #agentCollectInterval / #agentHeartbeatInterval / #agentMaxConcurrent /
//     #agentMaxModels: блок `agent` убран из payload, значения не сохраняются
//     и не возвращаются → любая правка исчезала при обновлении страницы.
// Плюс то же самое поле было в мастере инициализации (#wiz-apiToken), и оно
// даже попадало в файл экспорта пустой строкой.
//
// ЧТО СТАЛО: поля удалены, на их месте — строка факта (токен задаётся на
// сервере; параметры агента — в его конфиге на узле). Экспорт файла больше не
// пишет пустых значений.
//
// Проверка статическая. Запуск: node webui/tests/settings-inert-fields-r544.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const root = path.join(__dirname, '..');
const read = (rel) => fs.readFileSync(path.join(root, rel), 'utf8');

const indexHtml = read('index.html');
const appJs = read('js/app.js');
const configIo = read('js/modules/config-io.js');
const wizard = read('js/modules/setup-wizard.js');

let passed = 0;
const failures = [];
function check(name, fn) {
    try {
        fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' — ' + (e && e.message));
    }
}

// --- словари ---------------------------------------------------------------
// Загружаем настоящие ru.js/en.js (они пишут window.I18N_RU/EN), как это делают
// остальные наборы: проверять текст по константе в тесте бессмысленно — она
// разойдётся с словарём на первой же правке формулировки.
const sandbox = { window: {} };
function loadDict(rel) {
    const src = read(rel);
    const fn = new Function('window', src);
    fn(sandbox.window);
}
loadDict('js/i18n/ru.js');
loadDict('js/i18n/en.js');
const RU = sandbox.window.I18N_RU;
const EN = sandbox.window.I18N_EN;

const DEAD_KEYS = [
    'settings.api_token',
    'settings.agent.collect_interval',
    'settings.agent.heartbeat_interval',
    'settings.agent.max_concurrent',
    'settings.agent.max_models',
];
const NEW_KEYS = [
    'settings.security.token_source_note',
    'settings.agent.managed_note',
];

const FIELD_IDS = [
    'apiToken',
    'agentCollectInterval',
    'agentHeartbeatInterval',
    'agentMaxConcurrent',
    'agentMaxModels',
    'wiz-apiToken',
];

check('в разметке нет инертных полей (id=…)', function () {
    FIELD_IDS.forEach(function (id) {
        assert.strictEqual(indexHtml.indexOf('id="' + id + '"'), -1,
            'поле #' + id + ' вернулось в index.html: его значение никуда не уходит (saveSettings его не читает, сервер не отдаёт) — оператор снова правит пустоту');
    });
});

check('в мастере нет поля токена', function () {
    assert.strictEqual(wizard.indexOf('wiz-apiToken'), -1,
        '#wiz-apiToken вернулся в мастер: buildWizardPayload это поле не читает, токен задаётся сервером');
});

check('вместо полей — честный источник факта', function () {
    NEW_KEYS.forEach(function (k) {
        assert.ok(indexHtml.indexOf('data-i18n="' + k + '"') !== -1,
            'в разметке нет строки факта data-i18n="' + k + '" — раздел снова пуст или врёт');
    });
});

check('новые ключи есть в обоих словарях', function () {
    NEW_KEYS.forEach(function (k) {
        assert.ok(RU && RU[k], 'нет ru-перевода для ' + k);
        assert.ok(EN && EN[k], 'нет en-перевода для ' + k);
    });
});

check('удалённых ключей нет ни в одном словаре', function () {
    DEAD_KEYS.forEach(function (k) {
        assert.strictEqual(RU && Object.prototype.hasOwnProperty.call(RU, k), false,
            'ru.js всё ещё содержит мёртвый ключ ' + k);
        assert.strictEqual(EN && Object.prototype.hasOwnProperty.call(EN, k), false,
            'en.js всё ещё содержит мёртвый ключ ' + k);
    });
});

check('app.js не ищет удалённые поля', function () {
    FIELD_IDS.forEach(function (id) {
        assert.strictEqual(appJs.indexOf("getElementById('" + id + "')"), -1,
            "app.js всё ещё делает getElementById('" + id + "') — код молча работает вхолостую (element = null)");
    });
    // Список автосохранения не должен навешивать обработчики на несуществующие поля.
    FIELD_IDS.forEach(function (id) {
        assert.strictEqual(appJs.indexOf("'" + id + "'") === -1, true,
            "в app.js осталось упоминание '" + id + "' (например, в списке settingsFields)");
    });
});

check('экспорт/импорт конфигурации не пишет инертные значения', function () {
    assert.strictEqual(/apiToken:/.test(configIo), false,
        'config-io.js снова пишет apiToken в файл экспорта (там всегда пустая строка)');
    assert.strictEqual(/agent:\s*\{/.test(configIo), false,
        'config-io.js снова пишет блок agent в файл экспорта (значения берутся из несуществующих полей)');
    assert.strictEqual(/setFieldValue\('apiToken'/.test(configIo), false,
        "config-io.js при импорте снова применяет apiToken к полю, которого нет");
    assert.strictEqual(/setFieldValue\('agentCollectInterval'/.test(configIo), false,
        'config-io.js при импорте снова применяет поля агента');
});

console.log('\n' + passed + ' проверок зелёные, ' + failures.length + ' красных');
if (failures.length > 0) {
    failures.forEach(function (f) { console.error('  FAIL: ' + f); });
    process.exit(1);
}
