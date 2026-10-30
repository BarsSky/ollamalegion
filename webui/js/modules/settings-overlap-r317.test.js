// settings-overlap-r317.test.js — R317 (этап 5 рефакторинга настроек WebUI):
// разведение двух механизмов настроек и бейджи «перекрыто горячей настройкой».
//
// Запуск: node webui/js/modules/settings-overlap-r317.test.js
//
// ЧТО БЫЛО НЕ ТАК ДО ЭТОЙ ПРАВКИ. На одной странице «Настройки» жили ДВА разных
// механизма, и оператор не понимал разницы:
//   * горячий реестр балансера (R278/R305) — применяется сразу, без рестарта;
//   * старые поля config.json — пишутся кнопкой «Сохранить» и вступают в силу
//     только после перезапуска контейнеров.
// Хуже: часть старых полей перекрыта строками реестра (окно контекста и
// разрушающие операции с ним), и правка поля не давала эффекта — оператор
// менял значение и «ничего не происходило».
//
// Что фиксирует тест (без браузера и без сети):
//   1. на границе секций стоит подпись «требует перезапуска контейнеров» и она
//      РОВНО между реестром и старыми полями (порядок, а не просто наличие);
//   2. у кнопки «Сохранить» тоже есть напоминание (оператор доходит до неё и не
//      возвращается наверх читать подпись);
//   3. у каждого помеченного поля есть бейдж badge-warning и якорь на КАЖДУЮ
//      строку реестра, которая им управляет;
//   4. имена в якорях — РЕАЛЬНЫЕ ключи реестра балансера: сверяемся с
//      internal/settings/builtin_r278.go, а не с памятью автора правки;
//   5. строка реестра действительно имеет такой id (её рисует
//      settings-registry.js — якорь обязан куда-то вести);
//   6. поля НЕ удалены и НЕ переименованы: удаление — отдельная работа (риск
//      сломать сохранение), поэтому проверяем наличие исходных id;
//   7. все новые ключи есть и в ru.js, и в en.js (сырой ключ в панели = баг).

'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

let passed = 0;
const failures = [];
async function check(name, fn) {
    try {
        await fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' \u2014 ' + (e && e.message));
    }
}

const INDEX_HTML = fs.readFileSync(path.join(__dirname, '..', '..', 'index.html'), 'utf8');
const RU_SRC = fs.readFileSync(path.join(__dirname, '..', 'i18n', 'ru.js'), 'utf8');
const EN_SRC = fs.readFileSync(path.join(__dirname, '..', 'i18n', 'en.js'), 'utf8');
const GO_BUILTINS = fs.readFileSync(
    path.join(__dirname, '..', '..', '..', 'internal', 'settings', 'builtin_r278.go'), 'utf8');

// Таблица соответствия «поле config.json → строки горячего реестра».
//
// ПОЧЕМУ ИМЕННО ЭТИ. Соответствие искалось по смыслу (и это единственный
// источник правды для теста — разметка проверяется против него):
//   * ctxSize (Context Size / llamaCpp.contextLength) — БАЗА окна. Что делать
//     при несовпадении окна (ждать чужие стримы, ставить модель на свободный
//     узел вместо перезагрузки, спрашивать живой кластер) решает группа nctx;
//   * idleUnloadMinutes и modelReplicationIdleUnload — выгрузка по простою,
//     то есть разрушающая операция: сколько ждать чужие стримы перед ней,
//     задаёт LB_DESTRUCTIVE_OP_WAIT_SEC.
const OVERLAPS = [
    {
        field: 'ctxSize',
        hot: ['LB_PLACE_OVER_RELOAD', 'LB_LIVE_COPY_PROBE',
            'LB_RELOAD_DEFER_MAX_SEC', 'LB_RELOAD_DEFER_FAST_FAIL_SEC'],
    },
    { field: 'idleUnloadMinutes', hot: ['LB_DESTRUCTIVE_OP_WAIT_SEC'] },
    { field: 'modelReplicationIdleUnload', hot: ['LB_DESTRUCTIVE_OP_WAIT_SEC'] },
];

// Ключи, добавленные этапом 5. Обязаны быть в ОБОИХ словарях.
const NEW_KEYS = [
    'settings.old.title',
    'settings.old.desc',
    'settings.old.save_note',
    'settings.old.overlap_badge',
    'settings.old.overlap.ctx_size',
    'settings.old.overlap.idle_unload',
    'settings.hot.managed_in_roles',
    'settings.hot.open_monitor',
    'settings.hot.reset_viewer',
];

function hasKey(src, key) {
    return src.indexOf('"' + key + '"') >= 0;
}

// namesFromGoRegistry — имена настроек, реально зарегистрированные балансером.
function namesFromGoRegistry() {
    const out = [];
    const re = /Name:\s+"(LB_[A-Z0-9_]+)"/g;
    let m;
    while ((m = re.exec(GO_BUILTINS)) !== null) { out.push(m[1]); }
    return out;
}

// --- минимальный DOM + сеть: чтобы загрузить настоящий settings-registry.js ---
const DOM = {};
function makeEl(id) {
    return {
        id: id || '', innerHTML: '', textContent: '', value: '', style: {}, dataset: {},
        classList: { add() {}, remove() {}, contains() { return false; } },
        addEventListener() {}, removeEventListener() {}, appendChild() {},
        getAttribute() { return null; }, setAttribute() {}, removeAttribute() {},
        querySelector() { return null; }, querySelectorAll() { return []; },
    };
}
DOM['hotSettingsPanel'] = makeEl('hotSettingsPanel');
global.document = {
    readyState: 'loading',
    getElementById(id) { return DOM[id] || null; },
    createElement() { return makeEl(''); },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    addEventListener() {},
};
global.window = global;

const BASE = 'http://balancer.test:18081';
global.WEBUI_CONFIG = { API_BASE: BASE, API_TOKEN: 'tok-317' };

function mkRes(status, body) {
    const text = JSON.stringify(body === undefined ? null : body);
    return {
        ok: status >= 200 && status < 300, status, statusText: 'OK',
        headers: { get() { return 'application/json'; } },
        text() { return Promise.resolve(text); },
        json() { return Promise.resolve(JSON.parse(text)); },
    };
}
let registryBody = null;
global.fetch = function (url, opts) {
    const method = ((opts && opts.method) || 'GET').toUpperCase();
    const p = String(url).replace(BASE, '');
    if (method === 'GET' && p === '/api/v1/settings') {
        return Promise.resolve(mkRes(200, registryBody || { settings: [], count: 0 }));
    }
    return Promise.resolve(mkRes(404, { error: 'no route: ' + method + ' ' + p }));
};

require('../i18n/ru.js');
require('../i18n/en.js');
const RU = global.I18N_RU;
const EN = global.I18N_EN;
const missingKeys = [];
global.I18N = {
    getLang() { return 'ru'; },
    t(key, vars) {
        let s = RU[key];
        if (s === undefined) { if (missingKeys.indexOf(key) === -1) { missingKeys.push(key); } s = key; }
        if (vars) {
            Object.keys(vars).forEach(function (p) { s = String(s).split('{' + p + '}').join(String(vars[p])); });
        }
        return s;
    },
};

require('./api.js');
require('./settings-registry.js');
const SR = global.SettingsRegistry;
assert.ok(SR, 'window.SettingsRegistry должен быть экспортирован');

/** @returns {string} фрагмент разметки поля: от его form-group до input-а. */
function fieldBlock(html, fieldId) {
    const inputAt = html.indexOf('id="' + fieldId + '"');
    assert.ok(inputAt >= 0, 'поля #' + fieldId + ' нет в index.html — его НЕЛЬЗЯ удалять на этом этапе');
    // Бейдж и ссылки стоят внутри того же .form-group, до самого input.
    const start = html.lastIndexOf('<div class="form-group"', inputAt);
    return html.slice(start >= 0 ? start : 0, inputAt);
}

(async function main() {
    console.log('settings-overlap-r317.test.js — R317 (граница механизмов и бейджи)\n');

    await check('подпись «требует перезапуска контейнеров» стоит МЕЖДУ реестром и старыми полями', function () {
        const notice = INDEX_HTML.indexOf('id="settingsRestartNotice"');
        assert.ok(notice >= 0, 'нет блока #settingsRestartNotice — два механизма снова выглядят одинаково');
        const hotSection = INDEX_HTML.indexOf('id="section-hot-settings"');
        const modeSection = INDEX_HTML.indexOf('id="section-mode"');
        assert.ok(hotSection >= 0 && modeSection >= 0, 'не найдены секции реестра/режима');
        assert.ok(notice > hotSection, 'подпись стоит ВЫШЕ секции реестра — она говорит не о тех полях');
        assert.ok(notice < modeSection, 'подпись стоит НИЖЕ первого старого поля — граница проведена неверно');
    });

    await check('подпись называет последствие (перезапуск контейнеров) и приоритет реестра', function () {
        const i = INDEX_HTML.indexOf('id="settingsRestartNotice"');
        const block = INDEX_HTML.slice(i, INDEX_HTML.indexOf('</div>', INDEX_HTML.indexOf('</p>', i)));
        assert.ok(block.indexOf('data-i18n="settings.old.title"') >= 0, 'нет подписи settings.old.title');
        assert.ok(block.indexOf('data-i18n="settings.old.desc"') >= 0, 'нет пояснения settings.old.desc');
        assert.ok(/перезапуск[а-яА-Я]*\s+контейнер/i.test(block), 'в подписи нет слов про перезапуск контейнеров: ' + block.slice(0, 200));
        assert.ok(/перекрыва[а-я]*\s+горяч/i.test(block), 'в подписи не сказано, что часть значений перекрыта горячим реестром');
    });

    await check('у кнопки «Сохранить» есть напоминание о перезапуске', function () {
        const save = INDEX_HTML.indexOf('id="saveSettings"');
        const note = INDEX_HTML.indexOf('id="settingsSaveNote"');
        assert.ok(note >= 0, 'нет #settingsSaveNote рядом с «Сохранить»');
        assert.ok(note < save, 'напоминание стоит ПОСЛЕ кнопки — оператор его не увидит');
        const block = INDEX_HTML.slice(note, save);
        assert.ok(block.indexOf('data-i18n="settings.old.save_note"') >= 0, 'нет текста settings.old.save_note');
        assert.ok(/перезапуск/i.test(block), 'напоминание не говорит про перезапуск');
    });

    await check('у каждого помеченного поля есть бейдж badge-warning и подпись', function () {
        OVERLAPS.forEach(function (row) {
            const block = fieldBlock(INDEX_HTML, row.field);
            assert.ok(/class="badge badge-warning settings-overlap-badge"/.test(block),
                'у поля #' + row.field + ' нет бейджа badge-warning: ' + block.slice(-260));
            assert.ok(block.indexOf('data-i18n="settings.old.overlap_badge"') >= 0,
                'бейдж у #' + row.field + ' без перевода (сырой текст не локализуется)');
            assert.ok(block.indexOf('data-hot-overlap="' + row.hot.join(' ') + '"') >= 0,
                'data-hot-overlap у #' + row.field + ' не перечисляет все строки реестра');
        });
    });

    await check('анкор ведёт на КАЖДУЮ строку реестра, которая перекрывает поле', function () {
        OVERLAPS.forEach(function (row) {
            const block = fieldBlock(INDEX_HTML, row.field);
            row.hot.forEach(function (name) {
                assert.ok(block.indexOf('href="#hot-setting-' + name + '"') >= 0,
                    'у #' + row.field + ' нет якоря на ' + name);
                assert.ok(block.indexOf('data-hot-goto="' + name + '"') >= 0,
                    'якорь на ' + name + ' без data-hot-goto: свёрнутая секция не раскроется');
            });
        });
    });

    await check('имена в якорях — реальные ключи реестра балансера (сверка с Go)', function () {
        const names = namesFromGoRegistry();
        assert.ok(names.length >= 15, 'в builtin_r278.go не нашлось ключей реестра: ' + names.length);
        OVERLAPS.forEach(function (row) {
            row.hot.forEach(function (name) {
                assert.ok(names.indexOf(name) >= 0,
                    'якорь ссылается на «' + name + '», которого нет в реестре балансера');
            });
        });
    });

    await check('строка реестра с таким id действительно рисуется (якорь ведёт в неё)', async function () {
        const entries = namesFromGoRegistry().map(function (name, idx) {
            // Тип/группа не важны: проверяем только id строки, который строится из имени.
            return { name: name, type: 'int', group: 'nctx', description: 'd', hotReload: true,
                default: 0, effective: idx, source: 'default' };
        });
        registryBody = { count: entries.length, overrides: 0, sourcePriority: ['override', 'env', 'default'], settings: entries };
        await SR.load();
        const html = DOM['hotSettingsPanel'].innerHTML;
        OVERLAPS.forEach(function (row) {
            row.hot.forEach(function (name) {
                assert.ok(html.indexOf('id="hot-setting-' + name + '"') >= 0,
                    'в панели нет строки #hot-setting-' + name + ' — якорь из старого поля ведёт в пустоту');
            });
        });
    });

    await check('старые поля НЕ удалены и НЕ переименованы (удаление — отдельная работа)', function () {
        OVERLAPS.forEach(function (row) {
            assert.ok(INDEX_HTML.indexOf('id="' + row.field + '"') >= 0,
                'поле #' + row.field + ' пропало из разметки: на этом этапе его нельзя удалять');
        });
    });

    await check('новые ключи переведены в ru.js и en.js (сырой ключ не увидит оператор)', function () {
        const missingRu = NEW_KEYS.filter(function (k) { return !hasKey(RU_SRC, k); });
        const missingEn = NEW_KEYS.filter(function (k) { return !hasKey(EN_SRC, k); });
        assert.deepStrictEqual(missingRu, [], 'нет в ru.js: ' + missingRu.join(', '));
        assert.deepStrictEqual(missingEn, [], 'нет в en.js: ' + missingEn.join(', '));
    });

    await check('в разметке нет непереведённых новых ключей (панель не покажет settings.old.*)', function () {
        // Тест страховочный: data-i18n с ключом, которого нет в словарях, оставил бы
        // в UI английский текст или сам ключ.
        const used = [];
        const re = /data-i18n(?:-title)?="(settings\.old\.[A-Za-z0-9_.]+)"/g;
        let m;
        while ((m = re.exec(INDEX_HTML)) !== null) { used.push(m[1]); }
        assert.ok(used.length >= 5, 'в разметке не найдено новых data-i18n-ключей: ' + used.join(','));
        used.forEach(function (k) {
            assert.ok(hasKey(RU_SRC, k), 'ключ ' + k + ' из разметки отсутствует в ru.js');
            assert.ok(hasKey(EN_SRC, k), 'ключ ' + k + ' из разметки отсутствует в en.js');
        });
    });

    console.log('\nsettings-overlap: ' + passed + ' проверок пройдено' +
        (failures.length ? ', ПРОВАЛОВ: ' + failures.length : ''));
    if (failures.length) { process.exit(1); }
})().catch(function (e) {
    console.error('settings-overlap-r317.test.js упал вне проверки:', e);
    process.exit(1);
});
