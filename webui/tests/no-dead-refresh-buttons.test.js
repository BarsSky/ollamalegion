// no-dead-refresh-buttons.test.js — защита от «мёртвых» и ЛИШНИХ кнопок обновления.
//
// ИСТОРИЯ (важно, иначе правило непонятно).
// R84 (2026-10-03): оператор сообщил «слишком много кнопок обновить на страницах,
// и часть из них не производят обновление». Тогда проверили, что у каждой кнопки
// есть обработчик, и удалили ЧАСТЬ дублей — но проверку написали как список
// конкретных id, которые «должны были исчезнуть». Список — снимок реализации,
// а не правило: он не мешал появиться НОВЫМ страничным кнопкам, и на 2026-11-17 в
// разметке снова было ПЯТЬ кнопок «Обновить» (шапка + image-модели + загрузки
// изображений + диагностика + AutoTune), а на странице GGUF ещё две иконки.
// Сам набор при этом не запускался ни локально (его не было в списке
// `test:webui`), ни в CI — поэтому «зелёный» ничего не значил.
//
// ПРАВИЛО СЕЙЧАС (решение оператора, R493 2026-11-17): у приложения ОДНА кнопка
// «Обновить» — в шапке (#refreshBtn), и она же дёргает провайдеров активной
// страницы (js/modules/data-refresh.js). Поэтому проверяется не «список
// удалённых id», а инвариант: в статической разметке нет НИ ОДНОЙ другой кнопки
// обновления; а всякое действие, которое раньше висело на страничной кнопке,
// обязано быть зарегистрировано провайдером страницы — иначе «Обновить» в шапке
// его не обновит и данные останутся старыми.
//
// Запуск: node webui/tests/no-dead-refresh-buttons.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const root = path.join(__dirname, '..');

// Элементы, у которых обработчик/роль живут по другому принципу — с объяснением,
// почему это не «кнопка обновления».
//
// ПРАВИЛО ДЛЯ ALLOWED: сюда можно добавлять только ИНДИКАТОРЫ (span/div), а не
// кнопки. Кнопка обновления в разметке одна; если появилась вторая — это дефект,
// а не запись в этом списке.
const ALLOWED = {
    modelOpsAutoRefresh: 'не кнопка, а индикатор авто-обновления статуса операций (span)',
};

// Подписи «Обновить», которыми может быть подписана кнопка обновления.
// Все остальные кнопки с такими подписями в статической разметке — дубли.
const REFRESH_I18N_KEYS = ['common.retry', 'common.refresh', 'gguf.refresh', 'diag.refresh'];

function jsSources() {
    const out = [];
    (function walk(dir) {
        for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
            const p = path.join(dir, e.name);
            if (e.isDirectory()) { if (e.name !== 'tests') walk(p); }
            else if (e.name.endsWith('.js') && !e.name.endsWith('.test.js')) out.push(p);
        }
    })(path.join(root, 'js'));
    return out.map((f) => ({ file: path.relative(root, f), text: fs.readFileSync(f, 'utf8') }));
}

function readSrc(rel) {
    return fs.readFileSync(path.join(root, rel), 'utf8');
}

function boundBy(id, sources) {
    const patterns = [
        new RegExp("getElementById\\(['\"]" + id + "['\"]\\)"),
        new RegExp("byId\\(['\"]" + id + "['\"]\\)"),
        new RegExp("el\\(['\"]" + id + "['\"]\\)"),
        new RegExp("querySelector\\(['\"]#" + id + "['\"]\\)"),
    ];
    return sources.filter((s) => patterns.some((re) => re.test(s.text))).map((s) => s.file);
}

const html = readSrc('index.html');
const sources = jsSources();

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

/** Статические элементы разметки, которые выглядят как «кнопка обновления». */
function refreshControls() {
    const found = [];
    html.split(/\r?\n/).forEach((line, i) => {
        const id = line.match(/id="([^"]*[Rr]efresh[^"]*)"/);
        const action = line.match(/data-(?:imh|imt)-action="([^"]*refresh[^"]*)"/);
        const i18n = line.match(/data-i18n="([^"]*)"/);
        const isRefreshLabel = !!(i18n && REFRESH_I18N_KEYS.indexOf(i18n[1]) !== -1);
        if (id) found.push({ line: i + 1, id: id[1], kind: 'id', snippet: line.trim() });
        else if (action) found.push({ line: i + 1, id: '(delegated) ' + action[1], kind: 'action', snippet: line.trim() });
        else if (isRefreshLabel && /<button/i.test(line)) found.push({ line: i + 1, id: '(label ' + i18n[1] + ')', kind: 'label', snippet: line.trim() });
    });
    return found;
}

const controls = refreshControls();

check('в index.html есть кнопка обновления (иначе тест бессмыслен)', function () {
    assert.ok(controls.length >= 1, 'найдено ' + controls.length + ' элементов обновления');
});

// ГЛАВНЫЙ ИНВАРИАНТ: кнопка обновления ровно одна — #refreshBtn.
check('кнопка обновления в разметке РОВНО одна (#refreshBtn)', function () {
    const extra = controls
        .filter((c) => c.id !== 'refreshBtn')
        .filter((c) => !ALLOWED[c.id])
        .map((c) => 'L' + c.line + ' ' + c.id);
    assert.deepStrictEqual(extra, [],
        'лишние кнопки обновления: ' + extra.join(', ') +
        ' — единственная кнопка — #refreshBtn в шапке, её работу делают провайдеры страниц');
});

check('кнопка обновления стоит в шапке рядом с «Добавить бэкенд»', function () {
    const header = html.slice(0, html.indexOf('</header>'));
    assert.ok(header.indexOf('id="refreshBtn"') !== -1, 'кнопка обновления не в шапке');
    assert.ok(header.indexOf('id="addBackendBtn"') !== -1, 'рядом нет кнопки подключения бэкенда');
    assert.ok(header.indexOf('id="dataFreshness"') !== -1, 'нет индикатора свежести');
    assert.ok(header.indexOf('id="dataAutoToggle"') !== -1, 'нет переключателя авто-обновления');
});

// Регресс-список: эти id/действия уже удалялись и НЕ должны вернуться. Список
// оставлен как страховка для конкретных исторических дублей (правило выше ловит
// и любые новые).
check('исторические страничные дубли не вернулись', function () {
    const removed = [
        'refreshModelsBtn', 'refreshAgentsBtn', 'refreshProxyLogs',
        'imgRefreshBtn', 'imageBackendsRefresh', 'imageProfilesRefreshBtn', 'imgTestRefreshBtn',
        // R493 (2026-11-17): удалены последние четыре + действие загрузок.
        'imgModelsRefreshBtn', 'diagRefresh', 'autotune-refresh-btn',
    ];
    const back = removed.filter((id) => html.indexOf('id="' + id + '"') !== -1);
    assert.deepStrictEqual(back, [], 'вернулись дублирующие кнопки: ' + back.join(', '));
    assert.strictEqual(html.indexOf('data-imh-action="refresh-downloads"'), -1,
        'вернулось действие refresh-downloads: у таба «Загрузки» больше нет своей кнопки');
});

// Действия, которые раньше висели на удалённых кнопках, обязаны быть провайдерами:
// иначе общая кнопка их не обновит, и данные страницы останутся старыми
// (ровно тот дефект, на который жаловался оператор).
check('действия удалённых кнопок стали провайдерами страниц', function () {
    const expectations = [
        ['js/modules/diagnostics.js', "DataRefresh.register('diagnostics'",
            'диагностика: load() раньше висел на #diagRefresh'],
        ['js/modules/image-models-hf.js', 'DataRefresh.register(\'image\'',
            'загрузки изображений: refreshDownloads раньше висел на data-imh-action="refresh-downloads"'],
        ['js/modules/autotune_settings.js', "DataRefresh.register('settings'",
            'AutoTune: render() раньше висел на #autotune-refresh-btn'],
        ['js/modules/image-page.js', "DataRefresh.register('image'",
            'image-модели: loadModels() раньше висел на #imgModelsRefreshBtn'],
        ['js/modules/gguf-page-provider.js', "register('gguf'",
            'GGUF: список/детали обновляются через провайдера, а не страничной кнопкой'],
    ];
    const missing = [];
    for (const [file, needle, why] of expectations) {
        const text = readSrc(file);
        if (text.indexOf(needle) === -1) missing.push(file + ' (' + why + ')');
    }
    assert.deepStrictEqual(missing, [], 'нет провайдера: ' + missing.join('; '));
});

check('каждая страница с собственным источником имеет провайдера', function () {
    // R652–R655: слои вынесены из app.js — читаем и модули.
const appJs = readSrc('js/app.js') + '\n' + readSrc('js/modules/app-data.js') +
    '\n' + readSrc('js/modules/app-models.js') + '\n' + readSrc('js/modules/app-settings.js');
    const required = ['models', 'agents', 'logs', 'sessions', 'queue', 'monitor', 'settings'];
    const missing = required.filter((p) => appJs.indexOf("DR.register('" + p + "'") === -1);
    assert.deepStrictEqual(missing, [], 'нет провайдеров для страниц: ' + missing.join(', '));
    // image/gguf/diagnostics регистрируются в своих модулях.
    assert.ok(readSrc('js/modules/image-page.js').indexOf("DataRefresh.register('image'") !== -1,
        'image-page не регистрирует провайдера');
    assert.ok(readSrc('js/modules/diagnostics.js').indexOf("DataRefresh.register('diagnostics'") !== -1,
        'diagnostics не регистрирует провайдера');
    assert.ok(readSrc('js/modules/gguf-page-provider.js').indexOf("register('gguf'") !== -1,
        'gguf-page-provider не регистрирует провайдера');
});

check('data-refresh.js и его провайдеры подключены в index.html', function () {
    assert.ok(html.indexOf('js/modules/data-refresh.js') !== -1, 'data-refresh.js не подключён');
    assert.ok(html.indexOf('js/modules/gguf-page-provider.js') !== -1, 'gguf-page-provider.js не подключён');
    const dr = html.indexOf('js/modules/data-refresh.js');
    const app = html.indexOf('js/app.js');
    assert.ok(dr !== -1 && app !== -1 && dr < app, 'data-refresh.js должен грузиться ДО app.js');
});

// Обработчики у оставшихся «refresh»-элементов (индикатор) не обязательны, но
// мёртвых КНОПОК быть не должно: у каждой кнопки обновления вне шапки её нет по
// построению, поэтому проверяем только ALLOWED-индикаторы на наличие в разметке.
check('ALLOWED-элементы существуют в разметке (список не протух)', function () {
    const missing = Object.keys(ALLOWED).filter((id) => html.indexOf('id="' + id + '"') === -1);
    assert.deepStrictEqual(missing, [], 'в ALLOWED есть несуществующие id: ' + missing.join(', '));
});

console.log('\n' + (failures.length ? 'FAILED: ' + failures.length : 'ИТОГ: все проверки пройдены (' + passed + ')'));
if (failures.length) {
    failures.forEach(function (f) { console.error(' - ' + f); });
    process.exit(1);
}
