// health-spa-r690.test.js — R690 (2026-11-22): «Состояние» интегрировано в панель.
//
// ЖИВАЯ ЖАЛОБА ЗАКАЗЧИКА (дословно): «Страницу состояние я бы интегрировал вместе с
// остальными: сейчас она открывается в новом окне, а лучше бы как монитор
// интегрирована. Не забудь учесть темы, стили и обновления».
//
// ЧТО БЫЛО. Пункт меню «Состояние» — внешняя ссылка (`href="health.html"`,
// `target="_blank"`): клик открывал НОВУЮ ВКЛАДКУ, панель не переключалась, а
// отдельный документ жил своей темой (знал ДВЕ темы из семи) и своим языком.
// Цикл обновления (setTimeout 5 с) работал всегда, как только документ загружен, —
// вопрос «видит ли оператор эти данные» не задавался вовсе.
//
// ЧТО ПРОВЕРЯЕТСЯ (каждый пункт — своим прогоном):
//   1. пункт меню — обычный SPA-переход: data-page="health", href="#health",
//      БЕЗ target="_blank" (иначе снова новая вкладка);
//   2. в index.html есть SPA-секция #health-page и iframe #healthFrame с адресом
//      в data-src и БЕЗ src (иначе документ грузится и опрашивает API на дашборде);
//   3. ключи nav.health и header.health есть в ОБОИХ пакетах (требование
//      scripts/check_webui_i18n_keys.py для каждой data-page);
//   4. пауза/возобновление РАБОТАЮТ — проверяется исполнением модуля цикла
//      (webui/js/modules/health-poll.js) на подменённых таймерах: на паузе
//      запросов нет, после возврата данные обновляются СРАЗУ, ручная кнопка
//      работает и на паузе, два таймера не появляются;
//   5. контракт с панелью тот же, что у монитора: postMessage ollamalegion-pause/
//      ollamalegion-config, тема и язык читаются тем же способом (localStorage +
//      data-theme/lang родительского документа + storage-событие), а сама панель
//      считает паузу по тем же четырём поводам (страница, скрытая вкладка,
//      истёкшая сессия, пауза авто-обновления);
//   6. монитор не сломан: его iframe по-прежнему в data-src без src, а функции
//      sendMonitorConfig/syncMonitorIframePause на месте.
//
// Запуск: node webui/tests/health-spa-r690.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const root = path.join(__dirname, '..');            // webui/
const read = (rel) => fs.readFileSync(path.join(root, rel), 'utf8');

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

/** codeOf — исходник без комментариев (объяснения дефекта цитируют старый код). */
function codeOf(src) {
    const noBlock = src.replace(/\/\*[\s\S]*?\*\//g, '');
    return noBlock.split('\n').map(function (line) {
        const i = line.indexOf('//');
        return i === -1 ? line : line.slice(0, i);
    }).join('\n');
}

/** keysOf — ключи языкового пакета: `"key": "value"`. */
function keysOf(rel) {
    const src = read(rel);
    const out = {};
    const re = /"((?:[^"\\]|\\.)*)"\s*:\s*"((?:[^"\\]|\\.)*)"/g;
    let m;
    while ((m = re.exec(src)) !== null) out[m[1]] = m[2];
    return out;
}

// ===========================================================================
// 1. Пункт меню — SPA-переход, а не внешняя ссылка
// ===========================================================================
check('R690: пункт меню «Состояние» — SPA-переход без target=_blank', function () {
    const html = read('index.html');
    // Ищем именно ссылку меню этой страницы (а не любые упоминания health).
    const nav = /<a\s[^>]*data-page="health"[^>]*>/.exec(html);
    assert.ok(nav, 'в index.html нет пункта меню с data-page="health"');
    const tag = nav[0];
    assert.ok(/href="#health"/.test(tag),
        'пункт меню ведёт не в SPA: ' + tag + ' (клик снова откроет отдельный документ)');
    assert.strictEqual(/target\s*=\s*"_blank"/.test(tag), false,
        'у пункта меню снова target="_blank": «Состояние» открывается НОВОЙ ВКЛАДКОЙ ' +
        '(ровно жалоба заказчика), панель при этом не переключается');
    assert.strictEqual(/rel\s*=\s*"noopener"/.test(tag), false,
        'у SPA-ссылки остался rel="noopener" от внешней ссылки');
    assert.strictEqual(/href="health\.html"/.test(tag), false,
        'пункт меню снова ссылается на файл health.html напрямую');
});

// ===========================================================================
// 2. SPA-секция и iframe: адрес в data-src, без src
// ===========================================================================
check('R690: в index.html есть #health-page и iframe#healthFrame с data-src (без src)', function () {
    const html = read('index.html');
    assert.ok(/id="health-page"/.test(html),
        'нет SPA-секции #health-page: переключателю нечего показать (data-page без секции = «пункт ведёт в пустоту»)');
    const tag = /<iframe[^>]*id="healthFrame"[^>]*>/.exec(html);
    assert.ok(tag, 'нет iframe#healthFrame');
    assert.ok(/\sdata-src="health\.html\?v=[A-Za-z0-9._-]+"/.test(tag[0]),
        'нет data-src="health.html?v=…": адрес обязан лежать в разметке, иначе сборка ' +
        '(docker/webui/Dockerfile → sed по *.html) не подставит общий cache-токен и браузер ' +
        'будет держать старый health.html');
    assert.strictEqual(/(^|\s)src\s*=/.test(tag[0]), false,
        'у iframe снова есть src: документ загрузится сразу и начнёт опрос /api ' +
        '(у health.html первый запрос идёт прямо из boot)');
    assert.strictEqual(/(?:^|[^-a-z])src="health\.html/.test(html), false,
        'index.html снова ссылается на health.html через src (а не data-src)');
    assert.ok(/class="[^"]*health-iframe/.test(tag[0]),
        'у iframe нет класса оформления (.health-iframe в css/pages.css): страница ' +
        '«Состояние» не займёт место как «Монитор»');
});

// ===========================================================================
// 3. i18n: nav.health и header.health в ОБОИХ пакетах
// ===========================================================================
check('R690: nav.health и header.health есть в ru.js и en.js', function () {
    const packs = { ru: keysOf('js/i18n/ru.js'), en: keysOf('js/i18n/en.js') };
    ['nav.health', 'header.health'].forEach(function (key) {
        Object.keys(packs).forEach(function (lang) {
            const value = packs[lang][key];
            assert.ok(value !== undefined,
                'в ' + lang + '.js нет ключа ' + key + ': заголовок H1 собирается как ' +
                "'header.' + page, поэтому оператор увидит сырой литерал (дефект R675)");
            assert.ok(String(value).trim() !== '' && String(value).trim() !== key,
                'ключ ' + key + ' в ' + lang + '.js пустой или равен самому себе');
        });
    });
    // Английский и русский обязаны отличаться: иначе «перевод» не переводит.
    assert.notStrictEqual(packs.ru['header.health'], packs.en['header.health'],
        'header.health в ru.js и en.js одинаковый — это не перевод');
});

// ===========================================================================
// 4. Цикл обновления: поведение на подменённых таймерах
// ===========================================================================
const POLL_PATH = path.join(root, 'js/modules/health-poll.js');

/** loadHealthPoll — модуль цикла в песочнице (без DOM и без сети). */
function loadHealthPoll() {
    assert.ok(fs.existsSync(POLL_PATH),
        'нет js/modules/health-poll.js: у страницы «Состояние» нет проверяемого цикла ' +
        'обновления (пауза и «обновить сразу после возврата» живут в нём)');
    const sandbox = { window: {}, console: { error: function () {}, warn: function () {} } };
    vm.createContext(sandbox);
    vm.runInContext(fs.readFileSync(POLL_PATH, 'utf8'), sandbox, { filename: 'health-poll.js' });
    assert.ok(sandbox.window.HealthPoll && typeof sandbox.window.HealthPoll.create === 'function',
        'health-poll.js не выставил window.HealthPoll.create');
    return sandbox.window.HealthPoll;
}

/**
 * fakeTimers — таймеры без ожидания: видно, СКОЛЬКО запросов запланировано и
 * когда именно они сработают. Именно этим проверяется «на паузе запросов нет»
 * и «после возврата запрос идёт сразу, а не через полный интервал».
 */
function fakeTimers() {
    let nextId = 1;
    const pending = new Map();
    return {
        setTimer(fn, ms) { const id = nextId++; pending.set(id, { fn: fn, ms: ms }); return id; },
        clearTimer(id) { pending.delete(id); },
        count() { return pending.size; },
        delays() { return [...pending.values()].map(function (t) { return t.ms; }); },
        fireNext() {
            const first = pending.entries().next();
            if (first.done) return false;
            pending.delete(first.value[0]);
            first.value[1].fn();
            return true;
        },
        fireAll(limit) {
            let n = 0;
            while (this.fireNext()) {
                n++;
                if (limit && n >= limit) break;
            }
            return n;
        }
    };
}

/** makePoll — цикл с подменёнными таймерами и счётчиком запросов. */
function makePoll(opts) {
    const HP = loadHealthPoll();
    const timers = fakeTimers();
    const calls = { loads: 0 };
    const intervalMs = (opts && opts.interval) || 5000;
    const poll = HP.create({
        load: function () { calls.loads++; },
        interval: function () { return intervalMs; },
        setTimer: timers.setTimer,
        clearTimer: timers.clearTimer
    });
    return { poll: poll, timers: timers, calls: calls, intervalMs: intervalMs };
}

check('R690: цикл стартует без лишнего запроса и опрашивает по своему интервалу', function () {
    const h = makePoll({ interval: 5000 });
    h.poll.start();
    assert.strictEqual(h.calls.loads, 0,
        'start() сразу сходил в API: первое открытие страницы давало бы два запроса подряд ' +
        '(свой boot + цикл)');
    assert.strictEqual(h.timers.count(), 1, 'таймер не поставлен: страница не обновляется вовсе');
    assert.deepStrictEqual(h.timers.delays(), [5000],
        'интервал взят не из селектора страницы: ' + h.timers.delays());
    h.timers.fireNext();
    assert.strictEqual(h.calls.loads, 1, 'тик не сделал запрос');
    assert.strictEqual(h.timers.count(), 1, 'после тика таймер не переставлен: цикл умирает');
    h.timers.fireNext();
    assert.strictEqual(h.calls.loads, 2, 'второй тик не сделал запрос');
});

check('R690: на паузе запросов НЕТ (страница закрыта/скрыта/сессия истекла)', function () {
    const h = makePoll();
    h.poll.start();
    h.poll.setPaused('remote', true);
    assert.strictEqual(h.timers.count(), 0,
        'пауза не сняла таймер: документ продолжит опрашивать API невидимым (ровно то, ' +
        'из-за чего появился R493 у монитора)');
    h.timers.fireAll();
    assert.strictEqual(h.calls.loads, 0, 'на паузе всё-таки ушёл запрос');
    assert.strictEqual(h.poll.isPaused(), true);
});

check('R690: ВОЗВРАТ на страницу обновляет данные СРАЗУ, а не через интервал', function () {
    const h = makePoll({ interval: 5000 });
    h.poll.start();
    h.poll.setPaused('remote', true);
    const loadsBefore = h.calls.loads;
    h.poll.setPaused('remote', false);
    assert.strictEqual(h.calls.loads, loadsBefore + 1,
        'после снятия паузы запроса не было: оператор вернулся смотреть состояние, а ' +
        'видит данные с момента ухода (это и есть требование «обновить сразу»)');
    assert.strictEqual(h.timers.count(), 1, 'после возобновления цикл не продолжился');
});

check('R690: поводы паузы независимы (пауза оператора не снимается сообщением панели)', function () {
    const h = makePoll();
    h.poll.start();
    h.poll.setPaused('user', true);          // оператор нажал «Пауза»
    h.poll.setPaused('remote', false);       // панель сообщила «страница открыта»
    assert.strictEqual(h.poll.isPaused(), true,
        'сообщение панели сняло паузу, которую выбрал оператор');
    assert.strictEqual(h.timers.count(), 0, 'на паузе оператора поставлен таймер');
    h.poll.setPaused('user', false);
    assert.strictEqual(h.timers.count(), 1, 'после снятия паузы оператором цикл не продолжился');
});

check('R690: скрытая вкладка не снимает паузу «страница закрыта»', function () {
    const h = makePoll();
    h.poll.start();
    h.poll.setPaused('remote', true);        // ушли на другую страницу панели
    h.poll.setPaused('hidden', true);        // вкладка скрыта
    h.poll.setPaused('hidden', false);       // вкладка снова видима
    assert.strictEqual(h.poll.isPaused(), true,
        'возврат вкладки включил цикл на странице, с которой оператор ушёл');
    assert.strictEqual(h.calls.loads, 0, 'на скрытой/закрытой странице ушёл запрос');
});

check('R690: ручная кнопка «Обновить» работает и на паузе', function () {
    const h = makePoll();
    h.poll.start();
    h.poll.setPaused('user', true);
    h.poll.refreshNow();
    assert.strictEqual(h.calls.loads, 1,
        'кнопка «Обновить» на паузе ничего не делает: оператор не может посмотреть ' +
        'состояние, не снимая паузу');
    assert.strictEqual(h.timers.count(), 0,
        'ручное обновление сняло паузу и завело второй таймер');
});

check('R690: stop() снимает таймер, повторный start() не плодит второй', function () {
    const h = makePoll();
    h.poll.start();
    h.poll.start();
    assert.strictEqual(h.timers.count(), 1, 'второй start() завёл второй таймер (двойной опрос)');
    h.poll.stop();
    assert.strictEqual(h.timers.count(), 0, 'stop() не снял таймер');
    h.timers.fireAll();
    assert.strictEqual(h.calls.loads, 0, 'после stop() ушёл запрос');
});

check('R690: интервал мусорный (0/NaN) не превращается в цикл событий', function () {
    const HP = loadHealthPoll();
    const timers = fakeTimers();
    const poll = HP.create({
        load: function () {},
        interval: function () { return 0; },
        setTimer: timers.setTimer,
        clearTimer: timers.clearTimer
    });
    poll.start();
    assert.deepStrictEqual(timers.delays(), [HP.MIN_INTERVAL_MS],
        'нулевой интервал поставлен как есть: браузер начал бы опрашивать API в цикле');
});

// ===========================================================================
// 5. Контракт с панелью (тот же, что у монитора) и вызовы в боевом коде
// ===========================================================================
check('R690: панель управляет health теми же сообщениями, что монитором', function () {
    const appData = codeOf(read('js/modules/app-data.js'));
    const health = codeOf(read('health.html'));
    ['ollamalegion-pause', 'ollamalegion-config'].forEach(function (type) {
        assert.ok(appData.indexOf(type) !== -1,
            'панель не отправляет ' + type + ' (канал монитора обязан быть общим)');
        assert.ok(health.indexOf(type) !== -1,
            'health.html не обрабатывает ' + type + ': тема/язык/пауза из панели не доедут');
    });
    assert.ok(/message['"],\s*function/.test(health),
        'health.html не слушает postMessage вовсе');
});

check('R690: пауза health считается по тем же четырём поводам, что у монитора', function () {
    const src = codeOf(read('js/modules/app-data.js'));
    const start = src.indexOf('function healthIframeMustPause');
    assert.ok(start !== -1, 'нет healthIframeMustPause: пауза страницы нигде не решается');
    const body = src.slice(start, src.indexOf('function syncHealthIframePause', start));
    assert.ok(/getCurrentPage\(\)\s*!==\s*'health'/.test(body),
        'в паузу не входит «страница «Состояние» не открыта»: документ продолжил бы опрос невидимым');
    assert.ok(/visibilityState\s*===\s*'hidden'/.test(body),
        'в паузу не входит скрытая вкладка');
    assert.ok(/SessionExpiry/.test(body) && /isExpired\(\)/.test(body),
        'в паузу не входит истёкшая сессия: после протухания сессии страница продолжила бы ' +
        'получать 401 (это и есть «поток ошибок» R679)');
    assert.ok(/DataRefresh/.test(body) && /isAuto\(\)/.test(body),
        'в паузу не входит пауза авто-обновления панели');
});

check('R690: switchPage грузит документ при первом входе и синхронизирует паузу', function () {
    const app = codeOf(read('js/app.js'));
    const start = app.indexOf('function switchPage(');
    assert.ok(start !== -1, 'в app.js нет switchPage');
    const body = app.slice(start, app.indexOf('function pageFromURL', start));
    assert.ok(/page\s*===\s*'health'\)\s*ensureHealthFrame\(\)/.test(body),
        'switchPage не применяет data-src: iframe «Состояния» останется пустым');
    assert.ok(/syncHealthIframePause\(\)/.test(body),
        'switchPage не синхронизирует паузу health: уход со страницы не остановит опрос');
});

check('R690: init() ловит load фрейма и отправляет в него конфиг+паузу', function () {
    const app = codeOf(read('js/app.js'));
    const start = app.indexOf('function init() {');
    const body = app.slice(start, app.indexOf('function switchPage(', start));
    assert.ok(/getElementById\('healthFrame'\)/.test(body),
        'init() не находит #healthFrame: гонка R679 (сообщение уходит до регистрации ' +
        'обработчика в документе фрейма) остаётся открытой');
    assert.ok(/addEventListener\('load'/.test(body) && /sendHealthConfig\(\)/.test(body),
        'на load фрейма не отправляется конфиг: язык и состояние паузы не доедут');
    assert.ok(/front|health/i.test(body), 'в init() нет упоминания health-фрейма');
});

check('R690: истёкшая сессия и явный вход гасят/будят цикл «Состояния»', function () {
    const app = codeOf(read('js/app.js'));
    const pauseStart = app.indexOf('function pauseForSessionExpiry()');
    assert.ok(pauseStart !== -1, 'в app.js нет pauseForSessionExpiry');
    const pauseBody = app.slice(pauseStart, app.indexOf('function resumeAfterAuth', pauseStart));
    assert.ok(/postHealthPause\(true\)/.test(pauseBody),
        'pauseForSessionExpiry не гасит цикл «Состояния»: после истечения сессии страница ' +
        'продолжит получать 401');
    const resumeStart = app.indexOf('function resumeAfterAuth()');
    const resumeBody = app.slice(resumeStart, app.indexOf('// ---- Public API ----', resumeStart));
    assert.ok(/syncHealthIframePause\(\)/.test(resumeBody),
        'resumeAfterAuth не будит цикл «Состояния»: после входа страница осталась бы на паузе');
});

check('R690: у страницы есть провайдер DataRefresh (как у монитора)', function () {
    const src = codeOf(read('js/modules/app-data.js'));
    assert.ok(/DR\.register\('health'/.test(src),
        "нет провайдера 'health': панель не переотправляет конфиг/паузу на проходе " +
        'страницы, а общий 5-секундный опрос панели дублирует данные страницы');
    assert.ok(/case 'health':/.test(src),
        "в refreshPage нет case 'health': переотрисовка страницы не досылает конфиг");
});

check('R690: тема и язык health берутся тем же способом, что у монитора', function () {
    const health = read('health.html');
    assert.ok(/ollamalegion_theme/.test(health),
        'health.html не читает ollamalegion_theme: тема панели не доедет');
    assert.ok(/data-theme/.test(health),
        'health.html не ставит data-theme: css/themes.css не переключит палитру');
    assert.ok(/parent\.document\.documentElement/.test(health),
        'health.html не читает тему/язык родительского документа — а именно так их ' +
        'получает монитор (js/monitor/i18n-theme.js → syncTheme)');
    assert.ok(/addEventListener\('storage'/.test(health),
        'health.html не слушает storage: смена темы/языка в панели не применится к уже ' +
        'открытой странице (она останется в старой палитре до перезагрузки)');
    assert.ok(/health-embedded/.test(health),
        'нет класса health-embedded: внутри панели дублировались бы селектор языка, ' +
        'чип пользователя и ссылка «← Dashboard»');
    const code = codeOf(health);
    assert.ok(/js\/modules\/health-poll\.js\?v=/.test(health),
        'health.html не подключает модуль цикла с общим ?v= токеном');
    assert.ok(/setPaused\('hidden'/.test(code),
        'health.html не ставит паузу по своей вкладке (visibilitychange)');
    // Плотность: как и тема, это состояние ПАНЕЛИ (body[data-density] + токены
    // --font-size-*/--space-* в css/themes.css). Внутри iframe документ свой,
    // поэтому значение обязано переноситься — иначе страница остаётся «обычной»
    // на фоне плотной панели (тот же разъезд, что был с темой).
    assert.ok(/ollamalegion_density/.test(health),
        'health.html не читает ollamalegion_density: плотность панели не доедет');
    assert.ok(/data-density/.test(health),
        'health.html не ставит body[data-density]: css/themes.css не сожмёт типографику');
});

check('R690: тема из localStorage принимается ЛЮБАЯ (в панели их семь)', function () {
    const head = read('health.html');
    const start = head.indexOf('localStorage.getItem(\'ollamalegion_theme\')');
    assert.ok(start !== -1, 'health.html не читает сохранённую тему');
    const near = head.slice(start, start + 400);
    assert.strictEqual(/stored\s*===\s*'dark'\s*\|\|\s*stored\s*===\s*'light'/.test(near), false,
        'health.html снова принимает только dark/light: при темах linear/nvidia/vercel/' +
        'sentry/mint страница открывается в чужой палитре');
    assert.ok(/setAttribute\('data-theme', stored\)|setAttribute\(THEME_ATTR, stored\)/.test(near),
        'health.html не применяет сохранённую тему как есть');
});

// ===========================================================================
// 6. Совместимость: прямой URL и целость монитора
// ===========================================================================
check('R690: health.html остался самостоятельной страницей (прямой URL работает)', function () {
    assert.ok(fs.existsSync(path.join(root, 'health.html')),
        'файл health.html удалён: прямой URL /health.html и панель балансера (/health) ' +
        'перестали бы работать (на них ссылаются извне)');
    const html = read('health.html');
    assert.ok(/<html/.test(html) && /<!DOCTYPE html>/i.test(html),
        'health.html перестал быть самостоятельным документом');
    assert.ok(/data-i18n="health\.title"/.test(html),
        'в health.html пропал собственный заголовок страницы');
    assert.ok(/inline I18N|var RU=|window\.I18N=/.test(html),
        'health.html остался без своего словаря: по прямому URL (без панели) тексты ' +
        'потеряют перевод');
});

check('R690: монитор не задет (data-src, sendMonitorConfig, пауза)', function () {
    const html = read('index.html');
    const tag = /<iframe[^>]*id="monitorFrame"[^>]*>/.exec(html);
    assert.ok(tag, 'iframe монитора исчез');
    assert.strictEqual(/(^|\s)src\s*=/.test(tag[0]), false,
        'у iframe монитора снова появился src (регресс R686)');
    assert.ok(/\sdata-src="monitor\.html\?v=/.test(tag[0]), 'у монитора потерян data-src с ?v=');
    const appData = codeOf(read('js/modules/app-data.js'));
    ['sendMonitorConfig', 'syncMonitorIframePause', 'postMonitorPause', 'monitorIframeMustPause']
        .forEach(function (name) {
            assert.ok(new RegExp('function ' + name + '\\s*\\(').test(appData),
                'в app-data.js пропала ' + name + ': сломана пауза/конфиг монитора');
        });
});

console.log('\n' + passed + ' проверок зелёные, ' + failures.length + ' красных');
if (failures.length > 0) {
    failures.forEach(function (f) { console.error('  FAIL: ' + f); });
    process.exit(1);
}
