// nginx-font-trailing-slash-r680.test.js — R680 (2026-11-21): шрифтовой путь с
// завершающим слэшем отдаёт 404, а не 200 + index.html.
//
// ЖИВОЙ ДЕФЕКТ (замер на nginx:alpine, конфиг ДО правки):
//   GET /webfonts/fa-solid-900.woff2   -> 200 font/woff2
//   GET /webfonts/fa-solid-900.woff2/  -> 200 text/html    <-- ДЕФЕКТ
//
// Причина: `location ~* \.(woff|woff2|ttf|eot|otf)$` матчит только путь, у
// которого расширение В КОНЦЕ. Путь со слэшем в него не попадал и уходил в общий
// `location /` с `try_files $uri $uri/ /index.html` — то есть на несуществующий
// путь nginx отдавал SPA-фолбэк. С `X-Content-Type-Options: nosniff` браузер
// обязан выбросить «шрифт» с типом text/html, и Font Awesome молча остаётся без
// иконок — ни ошибки в UI, ни понятной строки в консоли.
//
// ПОЧЕМУ ПРОВЕРКА ТАКАЯ. Конфигурацию nginx нельзя «выполнить» в node, поэтому
// здесь (1) разбирается сам `webui/nginx.conf` и (2) применяется МОДЕЛЬ правил
// выбора location из документации nginx: точное совпадение (`=`) → самый длинный
// префикс → если он `^~`, регулярки не смотрятся → иначе первый совпавший regex
// В ПОРЯДКЕ ОБЪЯВЛЕНИЯ → иначе самый длинный префикс. Для путей этого стенда
// модель воспроизводит то, что показывает живой nginx (значения сверены с
// реальными ответами nginx:alpine: см. блок «СВЕРКА С ЖИВЫМ NGINX» ниже).
//
// Дополнительно проверяется, что правка не сломала:
//   * сам шрифт (`200` и статический файл существует);
//   * SPA-фолбэк на настоящих клиентских маршрутах (`/deep/spa/route` -> 200);
//   * шаблонную подстановку entrypoint.sh (в конфиг не добавлена новая ${VAR}:
//     sed подставляет РОВНО шесть переменных, остальные остались бы литералами).
//
// СВЕРКА С ЖИВЫМ NGINX (выполнена вручную, повторяется одной командой; образы
// проекта не собираются, живые контейнеры не трогаются):
//   docker run --rm -v <conf>:/etc/nginx/conf.d/default.conf:ro nginx:alpine nginx -t
//   -> nginx: configuration file /etc/nginx/nginx.conf test is successful
//   GET /webfonts/fa-solid-900.woff2  -> 200 font/woff2
//   GET /webfonts/fa-solid-900.woff2/ -> 404
//   GET /webfonts/                    -> 403 (каталог без index.html)
//   GET /deep/spa/route               -> 200 text/html (SPA-фолбэк цел)
//
// Запуск: node webui/tests/nginx-font-trailing-slash-r680.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const WEBUI = path.join(__dirname, '..');
const confSrc = fs.readFileSync(path.join(WEBUI, 'nginx.conf'), 'utf8');
const entrypoint = fs.readFileSync(path.join(WEBUI, '..', 'docker', 'webui', 'entrypoint.sh'), 'utf8');

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

// ---------------------------------------------------------------------------
// РАЗБОР КОНФИГА
// ---------------------------------------------------------------------------

/** stripComments — конфиг без `#`-комментариев (они ЦИТИРУЮТ дефектный путь). */
function stripComments(src) {
    return src.split('\n').map(function (line) {
        let out = '';
        let quote = null;
        for (let i = 0; i < line.length; i++) {
            const c = line[i];
            if (quote) {
                out += c;
                if (c === quote) quote = null;
                continue;
            }
            if (c === '"' || c === "'") { quote = c; out += c; continue; }
            if (c === '#') break;
            out += c;
        }
        return out;
    }).join('\n');
}

/**
 * parseLocations — все `location` серверного блока в ПОРЯДКЕ ОБЪЯВЛЕНИЯ.
 *
 * kind: 'exact' (=) | 'noregex' (^~) | 'regex' (~ / ~*) | 'prefix'.
 * Порядок важен: регулярные location nginx проверяет именно в нём.
 */
function parseLocations(src) {
    const text = stripComments(src);
    const out = [];
    let i = 0;
    while (i < text.length) {
        const at = text.indexOf('location', i);
        if (at === -1) break;
        const prev = at === 0 ? '\n' : text[at - 1];
        if (!/[\s};]/.test(prev)) { i = at + 'location'.length; continue; }
        let j = at + 'location'.length;
        while (j < text.length && /\s/.test(text[j])) j++;

        let kind = 'prefix';
        let modifier = '';
        if (text[j] === '=') { kind = 'exact'; modifier = '='; j++; }
        else if (text[j] === '^' && text[j + 1] === '~') { kind = 'noregex'; modifier = '^~'; j += 2; }
        else if (text[j] === '~') {
            modifier = text[j + 1] === '*' ? '~*' : '~';
            kind = 'regex';
            j += modifier.length;
        }
        while (j < text.length && /\s/.test(text[j])) j++;

        let k = j;
        while (k < text.length && !/[\s{]/.test(text[k])) k++;
        const pattern = text.slice(j, k);
        while (k < text.length && /\s/.test(text[k])) k++;
        if (text[k] !== '{') { i = at + 'location'.length; continue; }

        let depth = 0;
        let end = k;
        for (; end < text.length; end++) {
            if (text[end] === '{') depth++;
            else if (text[end] === '}') {
                depth--;
                if (depth === 0) break;
            }
        }
        out.push({
            kind: kind,
            modifier: modifier,
            pattern: pattern,
            body: text.slice(k + 1, end),
            index: at,
            line: text.slice(0, at).split('\n').length
        });
        i = end + 1;
    }
    return out;
}

const LOCATIONS = parseLocations(confSrc);

/** resolveLocation — модель приоритета location из документации nginx. */
function resolveLocation(uri) {
    // 1. точное совпадение
    const exact = LOCATIONS.filter(function (l) { return l.kind === 'exact' && l.pattern === uri; })[0];
    if (exact) return exact;

    // 2. самый длинный префикс (учитываются и `^~`)
    let bestPrefix = null;
    LOCATIONS.forEach(function (l) {
        if (l.kind !== 'prefix' && l.kind !== 'noregex') return;
        if (l.pattern.charAt(0) === '@') return;           // именованный location
        if (uri.indexOf(l.pattern) !== 0) return;
        if (!bestPrefix || l.pattern.length > bestPrefix.pattern.length) bestPrefix = l;
    });
    // 3. `^~` останавливает поиск регулярных
    if (bestPrefix && bestPrefix.kind === 'noregex') return bestPrefix;

    // 4. регулярные — первый совпавший В ПОРЯДКЕ ОБЪЯВЛЕНИЯ
    for (let n = 0; n < LOCATIONS.length; n++) {
        const l = LOCATIONS[n];
        if (l.kind !== 'regex') continue;
        let re = null;
        try {
            re = new RegExp(l.pattern, l.modifier === '~*' ? 'i' : '');
        } catch (e) {
            throw new Error('шаблон location не разбирается как регулярка: ' + l.pattern);
        }
        if (re.test(uri)) return l;
    }
    // 5. иначе — запомненный префикс
    return bestPrefix;
}

/**
 * outcome — что ответил бы nginx на путь (только те директивы, что есть в конфиге).
 *
 * Возможные ответы:
 *   {status: 200, file}    — статический файл существует;
 *   {status: 200, spa}     — try_files ушёл в /index.html (SPA-фолбэк);
 *   {status: 4xx}          — return/try_files =404;
 *   {status: 403, dir}     — каталог без index.html (autoindex off).
 */
function outcome(uri) {
    const pathOnly = String(uri).split('?')[0];
    const loc = resolveLocation(pathOnly);
    if (!loc) return { status: 0, note: 'location не найден', uri: uri };

    const ret = /\breturn\s+(\d{3})\b/.exec(loc.body);
    if (ret) return { status: Number(ret[1]), location: loc, uri: uri };

    const diskPath = path.join(WEBUI, pathOnly);
    const exists = fs.existsSync(diskPath);
    const isFile = exists && fs.statSync(diskPath).isFile();
    const isDir = exists && fs.statSync(diskPath).isDirectory();

    const tf = /\btry_files\s+([^;]+);/.exec(loc.body);
    if (tf) {
        const args = tf[1].trim().split(/\s+/);
        const last = args[args.length - 1];
        if (pathOnly.charAt(pathOnly.length - 1) === '/' && isDir) {
            // каталог без index.html: автоиндексация выключена → 403 (так и отвечает nginx)
            return { status: 403, location: loc, dir: true, uri: uri };
        }
        if (isFile) return { status: 200, location: loc, file: diskPath, uri: uri };
        const fallbackCode = /^=(\d{3})$/.exec(last);
        if (fallbackCode) return { status: Number(fallbackCode[1]), location: loc, uri: uri };
        return { status: 200, location: loc, spa: true, file: path.join(WEBUI, last), uri: uri };
    }

    // location без обработчика/try_files: отдаёт статический модуль
    if (isFile) return { status: 200, location: loc, file: diskPath, uri: uri };
    if (isDir) return { status: 403, location: loc, dir: true, uri: uri };
    return { status: 404, location: loc, uri: uri };
}

function describe(result) {
    const loc = result.location ? (result.location.modifier + ' ' + result.location.pattern) : '—';
    return 'status=' + result.status + ' location=' + loc + ' uri=' + result.uri;
}

// ---------------------------------------------------------------------------
// ПРОВЕРКИ
// ---------------------------------------------------------------------------

check('R680/3: конфиг разобран, location-блоки найдены (проверка не «по пустоте»)', function () {
    assert.ok(LOCATIONS.length >= 15,
        'в nginx.conf найдено всего ' + LOCATIONS.length + ' location — разбор сломан, проверки ниже ничего не значат');
    const fonts = LOCATIONS.filter(function (l) { return /woff/.test(l.pattern); });
    assert.strictEqual(fonts.length, 2,
        'шрифтовых location ожидалось 2 (файл и путь со слэшем), найдено ' + fonts.length);
});

check('R680/3: `/webfonts/fa-solid-900.woff2` -> 200, отдаёт шрифтовой location', function () {
    const r = outcome('/webfonts/fa-solid-900.woff2');
    assert.strictEqual(r.status, 200, 'сам шрифт перестал отдаваться: ' + describe(r));
    assert.ok(r.file && /fa-solid-900\.woff2$/.test(r.file), 'отдаётся не файл шрифта: ' + describe(r));
    assert.ok(/expires\s+1h/.test(r.location.body),
        'шрифт отдаёт НЕ шрифтовой location (потерян часовой кэш/политика R675): ' + describe(r));
});

check('R680/3: `/webfonts/fa-solid-900.woff2/` -> 404, а не 200 + index.html', function () {
    const r = outcome('/webfonts/fa-solid-900.woff2/');
    assert.strictEqual(r.status, 404,
        'путь шрифта с завершающим слэшем снова уходит в SPA-фолбэк (200 + HTML при nosniff): ' + describe(r));
    assert.ok(!r.spa, 'ответ пришёл из try_files … /index.html — это ровно дефект R680: ' + describe(r));
    assert.ok(r.location && r.location.kind === 'regex',
        'путь со слэшем обслуживает не регулярный location шрифтов: ' + describe(r));
});

check('R680/3: `/webfonts/` -> не 200 и не SPA-фолбэк (осознанный 403)', function () {
    const r = outcome('/webfonts/');
    // ОСОЗНАННОЕ РЕШЕНИЕ. `/webfonts/` — СУЩЕСТВУЮЩИЙ каталог без index.html:
    // nginx отвечает на него 403 (autoindex off), и это честный ответ «нечего
    // отдавать», в отличие от 200 + HTML. Приводить его к 404 пришлось бы
    // хардкодом пути каталога (`location = /webfonts/`), который обязан
    // существовать в образе, — лишняя связка без выгоды для оператора.
    assert.notStrictEqual(r.status, 200, 'на каталог шрифтов снова отдаётся 200: ' + describe(r));
    assert.ok(!r.spa, 'каталог шрифтов ушёл в SPA-фолбэк: ' + describe(r));
    assert.strictEqual(r.status, 403,
        'ожидался 403 (каталог без index.html), получено: ' + describe(r));
});

check('R680/3: SPA-фолбэк на настоящих клиентских маршрутах ЦЕЛ', function () {    const r = outcome('/deep/spa/route');
    assert.strictEqual(r.status, 200, 'клиентский маршрут перестал открывать панель: ' + describe(r));
    assert.ok(r.spa && /index\.html$/.test(r.file), 'фолбэк ведёт не на index.html: ' + describe(r));
});

check('R680/3: обычная статика и страницы не задеты', function () {
    const css = outcome('/css/font-awesome.min.css');
    assert.strictEqual(css.status, 200, 'CSS не отдаётся: ' + describe(css));
    const state = outcome('/js/app.js');
    assert.strictEqual(state.status, 200, 'JS не отдаётся: ' + describe(state));
    const index = outcome('/index.html');
    assert.strictEqual(index.status, 200, 'index.html не отдаётся: ' + describe(index));
});

check('R680/3: шрифтовой location со слэшем стоит ДО общего `location /`', function () {
    const slashBlock = LOCATIONS.filter(function (l) {
        return l.kind === 'regex' && /woff/.test(l.pattern) && /\/\$/.test(l.pattern);
    })[0];
    assert.ok(slashBlock, 'нет регулярного location для шрифтового пути с завершающим слэшем ' +
        '(`location ~* \\.(woff|woff2|ttf|eot|otf)/$`) — дефект R680 вернулся');
    assert.ok(/\breturn\s+404\b/.test(slashBlock.body), 'этот location не возвращает 404');
    const fallback = LOCATIONS.filter(function (l) {
        return l.kind === 'prefix' && l.pattern === '/';
    })[0];
    assert.ok(fallback, 'в конфиге нет общего `location /` — проверка потеряла точку отсчёта');
    assert.ok(slashBlock.index < fallback.index,
        'location шрифтов со слэшем объявлен ПОСЛЕ общего `location /` (строка ' +
        slashBlock.line + ' против ' + fallback.line + ') — держите его рядом со шрифтовым блоком');
});

check('R680/3: файловый шрифтовой location не переписан (200-путь цел)', function () {
    const fileBlock = LOCATIONS.filter(function (l) {
        return l.kind === 'regex' && /woff/.test(l.pattern) && !/\/\$/.test(l.pattern);
    })[0];
    assert.ok(fileBlock, 'пропал `location ~* \\.(woff|woff2|ttf|eot|otf)$` — шрифт вернётся в годовой кэш');
    assert.ok(/\btry_files\s+\$uri\s+=404\s*;/.test(fileBlock.body),
        'в шрифтовом location нет `try_files $uri =404` — отсутствующий шрифт снова станет 200 + HTML');
    assert.ok(/expires\s+1h/.test(fileBlock.body), 'потерян часовой кэш шрифтов (R675)');
});

check('R680/3: правка не добавила в шаблон новых ${VAR}', function () {
    // entrypoint.sh подставляет sed'ом РОВНО перечисленные переменные; любая
    // другая ${VAR} осталась бы литералом, и nginx -t упал бы на неизвестной
    // переменной. Список обязан совпадать с sed-строками entrypoint.sh —
    // расхождение ловится ещё и scripts/…/reexport-nginx-proxy-r709.test.js.
    //
    // R709: добавлены REEXPORT_HOST/REEXPORT_PORT (прокси опционального модуля
    // ре-экспорта GGUF). Их дефолты задаются в entrypoint, поэтому в шаблоне они
    // появляются только вместе с sed-подстановкой и никогда — литералами.
    //
    // R743: добавлен API_TOKEN — им location /api/reexport/ закрывает изменяющие
    // запросы к модулю (X-API-Token). Это токен СТЕКОВОГО контейнера, а не
    // значение из отданной страницы: R222 (токен не вшивается в config.js)
    // остаётся в силе.
    const expected = ['NGINX_PORT', 'API_HOST', 'API_PORT', 'LB_PORT', 'CPPWORKER_HOST', 'CPPWORKER_PORT',
        'REEXPORT_HOST', 'REEXPORT_PORT', 'API_TOKEN'];
    const found = (stripComments(confSrc).match(/\$\{[A-Z0-9_]+\}/g) || [])
        .map(function (m) { return m.slice(2, -1); })
        .filter(function (v, i, arr) { return arr.indexOf(v) === i; })
        .sort();
    assert.deepStrictEqual(found, expected.slice().sort(),
        'набор шаблонных переменных изменился: ' + found.join(', '));
    expected.forEach(function (name) {
        assert.ok(entrypoint.indexOf('${' + name + '}') !== -1,
            'entrypoint.sh не подставляет ' + name + ' — в конфиг попал бы литерал');
    });
});

check('R680/3: cache-bust `?v=…` не меняет решение (query отбрасывается до location)', function () {
    // Шрифт в CSS запрашивается как /webfonts/fa-solid-900.woff2?v=<хеш статики>
    // (R675), и nginx выбирает location по URI БЕЗ аргументов. Если бы это было
    // не так, cache-bust ломал бы и 200-путь, и новый 404-путь.
    const file = outcome('/webfonts/fa-solid-900.woff2?v=R680');
    assert.strictEqual(file.status, 200, 'шрифт с ?v= перестал отдаваться: ' + describe(file));
    const slash = outcome('/webfonts/fa-solid-900.woff2/?v=R680');
    assert.strictEqual(slash.status, 404, 'путь со слэшем и ?v= снова уходит в HTML: ' + describe(slash));
});

if (failures.length) {
    console.error('\n' + failures.length + ' проверок провалено, ' + passed + ' пройдено');
    process.exit(1);
}
console.log('\nвсе ' + passed + ' проверок пройдены');
