// router-r513.test.js — R513 (2026-11-19): навигация переживает F5 и «назад».
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА. «F5 не возвращает на ту же страницу». Причина: адрес
// не менялся НИКОГДА — в webui/js не было ни одного pushState/replaceState
// (проверено grep: 0 совпадений), а pageFromURL() читал ?page=/hash только при
// загрузке. Если в адресе однажды оказался ?page=settings, F5 открывал
// «Настройки» с любой страницы; если не оказалось — всегда дашборд.
//
// Тест двойной:
//   1. ПОВЕДЕНЧЕСКИЙ: тела knownPages/pageFromURL/syncPageURL вытаскиваются из
//      app.js и исполняются с подставным window — проверяется именно логика
//      (запись адреса, приоритет ?page= над #hash, отсев мусора), а не текст.
//   2. СТАТИЧЕСКИЙ: switchPage обязан звать syncPageURL, init — вешать popstate
//      и hashchange. Без этого «поведенческая» половина мертва в браузере.
const assert = require('assert');
const fs = require('fs');
const path = require('path');

const root = path.join(__dirname, '..');
const appJs = fs.readFileSync(path.join(root, 'js/app.js'), 'utf8');

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

// extractFn — тело функции по имени, по балансу фигурных скобок.
// Нужно, потому что app.js — один IIFE без экспортов: иначе логику роутера
// пришлось бы проверять по регуляркам, то есть «на глаз».
function extractFn(src, name) {
    const marker = 'function ' + name + '(';
    const start = src.indexOf(marker);
    assert.ok(start >= 0, 'в app.js нет функции ' + name);
    let i = src.indexOf('{', start);
    assert.ok(i > 0, 'у функции ' + name + ' нет тела');
    let depth = 0;
    for (; i < src.length; i++) {
        const ch = src[i];
        if (ch === '{') depth++;
        else if (ch === '}') {
            depth--;
            if (depth === 0) return src.slice(start, i + 1);
        }
    }
    throw new Error('не закрыто тело функции ' + name);
}

function makeRouter() {
    const code = [extractFn(appJs, 'pageFromURL'), extractFn(appJs, 'syncPageURL')].join('\n') +
        '\nreturn { pageFromURL: pageFromURL, syncPageURL: syncPageURL };';
    const factory = new Function('window', 'URLSearchParams', code);
    return function build(href) {
        const u = new URL(href);
        const win = {
            location: { href: u.href, search: u.search, hash: u.hash },
            history: {
                replaceState: function (state, title, next) {
                    const nu = new URL(next, u.origin);
                    win.location.href = nu.href;
                    win.location.search = nu.search;
                    win.location.hash = nu.hash;
                }
            }
        };
        const api = factory(win, URLSearchParams);
        api.win = win;
        return api;
    };
}

check('R513/3: выбранная страница попадает в адрес (иначе F5 не вернёт её)', () => {
    const r = makeRouter()('http://lb.local/index.html?page=dashboard');
    r.syncPageURL('logs');
    assert.strictEqual(new URL(r.win.location.href).searchParams.get('page'), 'logs',
        'адрес после перехода на «Логи»: ' + r.win.location.href);
});

check('R513/3: pageFromURL читает страницу из адреса (сценарий F5)', () => {
    const r = makeRouter()('http://lb.local/index.html?page=queue');
    assert.strictEqual(r.pageFromURL(), 'queue',
        'F5 на адресе ?page=queue обязан открыть «Очередь»');
});

check('R513/3: ?page= приоритетнее устаревшего #hash', () => {
    // Ссылку могли сохранить в старом формате #hash; после первого клика в
    // адресе появляются оба, и побеждать должен ?page=.
    const r = makeRouter()('http://lb.local/index.html?page=settings#queue');
    assert.strictEqual(r.pageFromURL(), 'settings',
        'при ?page=settings и #queue открылась не та страница');
});

check('R513/3: hash-ссылка без ?page= продолжает работать (обратная совместимость)', () => {
    const r = makeRouter()('http://lb.local/index.html#diagnostics');
    assert.strictEqual(r.pageFromURL(), 'diagnostics', 'старая ссылка #diagnostics сломалась');
});

check('R513/3: мусор в адресе не открывает пустой экран', () => {
    const r = makeRouter()('http://lb.local/index.html?page=<script>');
    assert.strictEqual(r.pageFromURL(), '', 'неизвестная страница не должна проходить фильтр');
});

check('R513/3: в списке страниц есть agents и нет несуществующей help', () => {
    assert.strictEqual(makeRouter()('http://lb.local/index.html?page=agents').pageFromURL(), 'agents',
        '?page=agents молча открывает дашборд — страницы нет в списке известных');
    assert.strictEqual(makeRouter()('http://lb.local/index.html?page=help').pageFromURL(), '',
        'help остался в списке, хотя страницы #help-page в index.html нет');
    // Список читаем прямо из тела pageFromURL: другой копии у роутера нет.
    const fn = extractFn(appJs, 'pageFromURL');
    const listMatch = /var known = \[([\s\S]*?)\];/.exec(fn);
    assert.ok(listMatch, 'в pageFromURL нет списка known');
    const pages = (listMatch[1].match(/'[a-z0-9-]+'/g) || []).map((s) => s.replace(/'/g, ''));
    const html = fs.readFileSync(path.join(root, 'index.html'), 'utf8');
    const missing = pages.filter((p) => html.indexOf('id="' + p + '-page"') === -1);
    assert.deepStrictEqual(missing, [],
        'в списке роутера есть страницы без секции в разметке: ' + missing.join(', '));
});

check('R513/3: switchPage пишет адрес (поведение выше не осталось мёртвым кодом)', () => {
    assert.ok(/syncPageURL\(page\)/.test(extractFn(appJs, 'switchPage')),
        'switchPage не вызывает syncPageURL — адрес снова заморожен');
});

check('R513/3: F5-сценарий подписан на историю и хэш', () => {
    assert.ok(/addEventListener\('popstate'/.test(appJs),
        'нет обработчика popstate — кнопка «назад» не переключит страницу');
    assert.ok(/addEventListener\('hashchange'/.test(appJs),
        'нет обработчика hashchange — ссылки #page снова молча игнорируются');
});

if (failures.length) {
    console.error('\nИТОГ: ' + failures.length + ' проверок красных');
    process.exit(1);
}
console.log('\nИТОГ: ' + passed + ' проверок зелёные');
