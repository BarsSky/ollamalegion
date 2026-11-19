// reference-page-static.test.js — R493 (2026-11-17): статические инварианты
// страницы «Справка и шаблоны».
//
// ПОЧЕМУ СТАТИЧЕСКИЙ ТЕСТ, А НЕ БРАУЗЕРНЫЙ. Три правила ниже проверяются по
// тексту файлов, потому что нарушение видно сразу и не требует браузера:
//   1. ОДНА КНОПКА ОБНОВЛЕНИЯ НА ПРИЛОЖЕНИЕ. Кнопка — #refreshBtn в шапке, а
//      страница обязана зарегистрировать провайдера 'reference' в реестре
//      data-refresh.js. Своя кнопка «Обновить» здесь = возврат дефекта Д1
//      (docs/plans/2026-11-17-webui-refresh-diagnosis.md).
//   2. МУТАЦИИ — ТОЛЬКО ADMIN. `data-admin-only` на всех кнопках, которые шлют
//      PUT/DELETE/POST. Сервер всё равно проверяет права (AuthMiddleware), это
//      только UX; но без атрибута viewer видит кнопки, которые ему запрещены.
//   3. I18N. Каждый ключ, который показывает страница (в разметке и в модуле),
//      обязан существовать в ОБОИХ пакетах — иначе оператор увидит литерал
//      `reference.error.backend_required` вместо текста.
//
// Запуск: node webui/tests/reference-page-static.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const root = path.join(__dirname, '..');

function readSrc(rel) {
    return fs.readFileSync(path.join(root, rel), 'utf8');
}

const html = readSrc('index.html');
const appJs = readSrc('js/app.js');
const pageJs = readSrc('js/modules/reference-page.js');
// R642: состояние и помощники страницы вынесены в reference-page-helpers.js.
// Разметка кнопок и ключи t('...') теперь живут в ДВУХ файлах — читаем оба, иначе
// проверки «мутации только admin» и «ключи есть в ru/en» врали бы (первый прогон
// после выноса дал ровно это: «кнопки не найдены в модуле: edit-rec, delete-rec,
// to-template» — кнопки были на месте, в файле-помощнике).
const helpersJs = readSrc('js/modules/reference-page-helpers.js');
const pageJsAll = pageJs + '\n' + helpersJs;
const ru = readSrc('js/i18n/ru.js');
const en = readSrc('js/i18n/en.js');

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

/** packKeys — ключи пакета переводов без исполнения JS (как check_webui_i18n_keys.py). */
function packKeys(src) {
    const keys = new Set();
    const re = /"((?:[^"\\]|\\.)*)"\s*:/g;
    let m;
    while ((m = re.exec(src)) !== null) keys.add(m[1]);
    return keys;
}
const RU_KEYS = packKeys(ru);
const EN_KEYS = packKeys(en);

/** referenceBlock — разметка страницы «Справка и шаблоны» из index.html. */
function referenceBlock() {
    const start = html.indexOf('id="reference-page"');
    assert.ok(start !== -1, 'в index.html нет контейнера #reference-page');
    const end = html.indexOf('<!-- Settings Page -->', start);
    assert.ok(end !== -1, 'не найден конец страницы reference (маркер следующей страницы)');
    return html.slice(start, end);
}
const block = referenceBlock();

/** moduleKeys — ключи reference.*, которые модуль запрашивает через t('...'). */
function moduleKeys() {
    const keys = new Set();
    // Левый контекст `t(` — только одиночный идентификатор t, чтобы `list('x')`
    // и `gateText('...')` не считались вызовами перевода.
    const re = /(?<![A-Za-z0-9_$.])t\('((?:[a-z][a-z0-9._]*))'/g;
    let m;
    while ((m = re.exec(pageJsAll)) !== null) keys.add(m[1]);
    return keys;
}

/** htmlI18nKeys — ключи data-i18n* внутри блока страницы. */
function htmlI18nKeys(text) {
    const keys = new Set();
    const re = /data-i18n(?:-[a-z-]+)?="([^"]*)"/g;
    let m;
    while ((m = re.exec(text)) !== null) {
        let raw = m[1].trim();
        if (!raw) continue;
        const bracket = raw.match(/^\[[a-z-]+\](.+)$/);
        keys.add(bracket ? bracket[1].trim() : raw);
    }
    return keys;
}

// ---------------------------------------------------------------------------
// A. Навигация, контейнер страницы, порядок скриптов, роутер
// ---------------------------------------------------------------------------

check('в меню есть пункт reference с ключом nav.reference', function () {
    assert.ok(/data-page="reference"/.test(html), 'нет пункта меню data-page="reference"');
    assert.ok(/data-page="reference"[^>]*>[\s\S]{0,200}data-i18n="nav\.reference"/.test(html) ||
        /data-i18n="nav\.reference"/.test(html),
        'пункт меню не подписан ключом nav.reference');
});

check('есть контейнер страницы <div class="page" id="reference-page">', function () {
    assert.ok(/<div class="page" id="reference-page">/.test(html),
        'нет контейнера страницы #reference-page (switchPage ищет page + "-page")');
});

check('R642: reference-page-helpers.js подключён ДО reference-page.js', function () {
    const helpersAt = html.indexOf('src="js/modules/reference-page-helpers.js');
    const pageAt = html.indexOf('src="js/modules/reference-page.js');
    assert.ok(helpersAt !== -1, 'reference-page-helpers.js не подключён в index.html');
    assert.ok(pageAt !== -1, 'reference-page.js не подключён');
    assert.ok(helpersAt < pageAt,
        'помощники обязаны грузиться ПЕРЕД страницей: она берёт window.ReferencePageHelpers');
});

check('скрипт reference-page.js подключён ДО js/app.js', function () {
    // Ищем именно тег подключения (src="..."), а не упоминание имени в
    // комментариях: имя файла встречается и в пояснении к пункту меню.
    const page = html.indexOf('src="js/modules/reference-page.js');
    const app = html.indexOf('src="js/app.js');
    assert.ok(page !== -1, 'reference-page.js не подключён в index.html');
    assert.ok(app !== -1, 'app.js не подключён');
    assert.ok(page < app, 'reference-page.js должен грузиться ДО app.js (refreshPage монтирует страницу)');
});

check('в index.html страница грузится после data-refresh/api/gguf/cppworker-params', function () {
    const page = html.indexOf('src="js/modules/reference-page.js');
    const deps = [
        'src="js/modules/data-refresh.js',
        'src="js/modules/api.js',
        'src="js/modules/gguf-renderer-detail-render.js',
        'src="js/modules/cppworker-params.js'
    ];
    const missing = deps.filter((dep) => {
        const at = html.indexOf(dep);
        return at === -1 || at > page;
    });
    assert.deepStrictEqual(missing, [], 'зависимости подключены позже страницы: ' + missing.join(', '));
});

check('роутер: pageFromURL знает reference (прямая ссылка ?page=reference)', function () {
    const fromUrl = appJs.slice(appJs.indexOf('function pageFromURL'), appJs.indexOf('function getPageTitle'));
    assert.ok(/'reference'/.test(fromUrl), 'pageFromURL не знает страницу reference — прямая ссылка уведёт на дашборд');
});

check("refreshPage: case 'reference' монтирует страницу, а не пересобирает DOM", function () {
    const start = appJs.indexOf("case 'reference':");
    assert.ok(start !== -1, "в refreshPage нет case 'reference'");
    const body = appJs.slice(start, start + 1200);
    assert.ok(/ReferencePage\.mount\(\)/.test(body), 'страница не монтируется модулем');
    assert.ok(!/innerHTML/.test(body),
        'refreshPage пересобирает разметку: тик авто-обновления будет стирать фильтр и выбор бэкенда (дефект GGUF R493)');
});

// ---------------------------------------------------------------------------
// B. Одна кнопка обновления на приложение + провайдер
// ---------------------------------------------------------------------------

check('на странице reference НЕТ своей кнопки «Обновить»', function () {
    const badId = block.match(/id="[^"]*refresh[^"]*"/i);
    assert.strictEqual(badId, null,
        'в разметке страницы есть элемент обновления: ' + (badId && badId[0]) +
        ' — кнопка обновления одна, #refreshBtn в шапке');
    assert.ok(!/data-i18n="common\.(retry|refresh)"/.test(block),
        'в разметке страницы есть кнопка с подписью «Обновить»');
    assert.ok(!/data-ref-action="refresh"/.test(pageJs),
        'модуль рисует кнопку обновления (data-ref-action="refresh")');
});

check("модуль регистрирует провайдера DataRefresh.register('reference', ...)", function () {
    // Ищем именно СТРОКУ КОДА (в начале строки), а не упоминание в шапке файла:
    // пояснение «страница регистрирует провайдера» — это текст, а не регистрация.
    const re = /^[ \t]*window\.DataRefresh\.register\('reference'/m;
    assert.ok(re.test(pageJs),
        'нет регистрации провайдера reference — кнопка и авто-тик в шапке страницу не обновят');
    const call = pageJs.search(re);
    const tail = pageJs.slice(call, call + 220);
    assert.ok(/load\(\)/.test(tail), 'провайдер обязан вызывать load() (обновление списка и деталей)');
});

check('модуль отдаёт публичный API в window (check_iife_exports.py)', function () {
    assert.ok(/window\.ReferencePage\s*=/.test(pageJs), 'window.ReferencePage не выставлен');
    ['mount', 'load', 'selectTab'].forEach((name) => {
        assert.ok(new RegExp('\\b' + name + '\\s*:').test(pageJs), 'в API нет метода ' + name);
    });
});

// ---------------------------------------------------------------------------
// C. Мутации — только admin
// ---------------------------------------------------------------------------

check('все мутирующие кнопки модуля помечены data-admin-only', function () {
    const MUTATING = ['edit-rec', 'delete-rec', 'to-template', 'apply', 'preview', 'editor-save', 'editor-delete'];
    const buttons = pageJsAll.match(/<button[^>]*>/g) || [];
    const found = {};
    buttons.forEach((btn) => {
        const m = btn.match(/data-ref-action="([a-z-]+)"/);
        if (!m) return;
        found[m[1]] = btn;
    });
    const missing = MUTATING.filter((action) => !found[action]);
    assert.deepStrictEqual(missing, [], 'кнопки не найдены в модуле: ' + missing.join(', '));
    const without = MUTATING.filter((action) => found[action].indexOf('data-admin-only') === -1);
    assert.deepStrictEqual(without, [],
        'мутирующие кнопки без data-admin-only: ' + without.join(', ') +
        ' — viewer увидит то, что ему запрещено');
});

check('кнопка «Новая запись» помечена data-admin-only', function () {
    const newBtn = block.match(/<button[^>]*data-ref-action="new-rec"[^>]*>/);
    assert.ok(newBtn, 'в разметке нет кнопки создания записи');
    assert.ok(newBtn[0].indexOf('data-admin-only') !== -1, 'кнопка создания записи без data-admin-only');
});

// ---------------------------------------------------------------------------
// D. i18n: ключи разметки и модуля есть в ОБОИХ пакетах
// ---------------------------------------------------------------------------

check('ключи разметки страницы reference есть в ru.js и en.js', function () {
    const keys = Array.from(htmlI18nKeys(block));
    assert.ok(keys.length >= 6, 'в разметке страницы подозрительно мало ключей: ' + keys.join(','));
    const missing = [];
    keys.forEach((key) => {
        if (!RU_KEYS.has(key)) missing.push(key + ' (ru)');
        if (!EN_KEYS.has(key)) missing.push(key + ' (en)');
    });
    assert.deepStrictEqual(missing, [], 'нет переводов: ' + missing.join(', '));
});

check('все ключи reference.*, которые использует модуль, есть в ru.js и en.js', function () {
    const keys = Array.from(moduleKeys()).filter((k) => k.indexOf('reference.') === 0);
    assert.ok(keys.length > 40, 'модуль почти не использует i18n: ' + keys.length + ' ключей');
    const missing = [];
    keys.forEach((key) => {
        if (!RU_KEYS.has(key)) missing.push(key + ' (ru)');
        if (!EN_KEYS.has(key)) missing.push(key + ' (en)');
    });
    assert.deepStrictEqual(missing, [], 'нет переводов: ' + missing.join(', '));
});

check('ни один ключ reference.* в пакетах не переведён сам в себя', function () {
    const self = [];
    const re = /"(reference\.[^"]+)"\s*:\s*"([^"]*)"/g;
    [ru, en].forEach((src) => {
        let m;
        while ((m = re.exec(src)) !== null) {
            if (m[2].trim() === m[1] || m[2].trim() === '') self.push(m[1]);
        }
    });
    assert.deepStrictEqual(Array.from(new Set(self)), [], 'переводы-пустышки: ' + self.join(', '));
});

// ---------------------------------------------------------------------------
// E. Не дублируем знания: шаблонные источники данных
// ---------------------------------------------------------------------------

check('список «где загружена модель» берётся из Api.clusterModels.loaded()', function () {
    assert.ok(/clusterModels/.test(pageJs), 'модуль не использует Api.clusterModels');
    assert.ok(/\.loaded\(\)/.test(pageJs), 'модуль не вызывает clusterModels.loaded()');
    assert.ok(!/api\/v1\/models['"]/.test(pageJs),
        'модуль ходит в /api/v1/models: для llama.cpp он пуст (models:null), нужен cluster/models/loaded');
});

check('метаданные параметров берутся из MODEL_PARAM_SPECS, своей таблицы полей нет', function () {
    assert.ok(/MODEL_PARAM_SPECS/.test(pageJs), 'модуль не читает MODEL_PARAM_SPECS');
    const ownMin = /min\s*:\s*256\b/.test(pageJs);
    const ownMax = /max\s*:\s*1048576\b/.test(pageJs);
    assert.ok(!ownMin && !ownMax, 'модуль завёл свою таблицу границ полей (дубль MODEL_PARAM_SPECS)');
});

check('применение шаблона переиспользует существующую модалку CppWorkerParams, а не свой reload', function () {
    assert.ok(/CppWorkerParams/.test(pageJs), 'модуль не переиспользует CppWorkerParams');
    assert.ok(/applyProfileWithProgress/.test(pageJs), 'модуль не использует существующий прогресс применения');
    assert.ok(!/\/apply\/progress/.test(pageJs), 'модуль завёл свой SSE-прогресс (второй механизм запрещён)');
    assert.ok(!/clusterModels\.reload/.test(pageJs) && !/models\/reload/.test(pageJs),
        'модуль перезагружает модель сам — это второй механизм reload');
});

check('CSRF/токен не собираются в модуле руками (только через Api)', function () {
    // Проверяем ПРИСВАИВАНИЕ заголовков, а не упоминание имён: имена встречаются
    // в пояснении «заголовки уже решены в api.js», и это не дубль обёртки.
    assert.ok(!/['"]X-API-Token['"]\s*:/.test(pageJs), 'модуль собирает заголовок токена руками — это делает api.js');
    assert.ok(!/['"]X-CSRF-Token['"]\s*:/.test(pageJs), 'модуль собирает CSRF-заголовок руками — это делает auth.js');
    assert.ok(!/\bfetch\s*\(/.test(pageJs), 'модуль вызывает fetch напрямую вместо Api.reference');
});

// ---------------------------------------------------------------------------
// F. Обёртки HTTP присутствуют там, где им место
// ---------------------------------------------------------------------------

check('Api.reference предоставляет list/get/upsert/remove/toTemplate/preview/apply', function () {
    const apiJs = readSrc('js/modules/api.js');
    const start = apiJs.indexOf('reference: {');
    assert.ok(start !== -1, 'в api.js нет объекта reference');
    const body = apiJs.slice(start, start + 6000);
    ['list', 'get', 'upsert', 'remove', 'toTemplate', 'preview', 'apply'].forEach((name) => {
        assert.ok(new RegExp('async\\s+' + name + '\\s*\\(').test(body), 'нет метода Api.reference.' + name + '()');
    });
    assert.ok(/api\/v1\/reference\//.test(apiJs), 'пути справочника не собираются в api.js');
});

console.log('\n' + (failures.length ? 'FAILED: ' + failures.length : 'ИТОГ: все проверки пройдены (' + passed + ')'));
if (failures.length) {
    failures.forEach((f) => console.error(' - ' + f));
    process.exit(1);
}
