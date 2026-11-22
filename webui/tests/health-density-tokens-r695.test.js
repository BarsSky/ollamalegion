// health-density-tokens-r695.test.js — R695: типографика health.html подчиняется
// плотности панели (data-density=dense), то есть берётся из токенов themes.css.
//
// ЖИВОЙ ДЕФЕКТ. Плотность в панели — это body[data-density] + переопределение
// --font-size-* в css/themes.css (-15 % у dense). Сама страница «Состояние» живёт
// в iframe отдельным документом, но атрибут плотности ей уже переносится (R690,
// applyDensity). Толку от переноса не было: размеры были ВПИСАНЫ числом —
// «32px» у оценки здоровья, 11/12/13/16/24px у подписей карточек, шапок секций,
// таблиц и строк ошибок. Токены при этом менялись, а страница оставалась
// «обычной»: на фоне плотной панели это видно как чужое пятно (тот же класс, что
// был у темы до R690 и у рамок до R514).
//
// ЧТО ПРОВЕРЯЕТСЯ:
//   1. СТАТИЧЕСКИ: в health.html не осталось ни одного `font-size: <число>px` —
//      ни в <style>, ни в inline-атрибутах (проверка по всему файлу, без вырезки
//      комментариев: если кто-то вернёт px, набор покраснеет даже в комментарии —
//      это намеренно строго для этой страницы).
//   2. КАЖДЫЙ использованный --font-size-* определён в css/themes.css И имеет
//      значение в блоке [data-density="dense"] — иначе токен есть, а сжатия нет.
//   3. ЧИСЛА: мини-каскад (значения токенов из themes.css + правила страницы)
//      даёт реальные px для normal и dense. Ключевые элементы (оценка здоровья,
//      подписи карточек, шапки секций, таблица) обязаны УМЕНЬШАТЬСЯ, а при
//      normal возвращаться к значениям шкалы.
//
// ПОЧЕМУ МИНИ-КАСКАД, А НЕ БРАУЗЕР. Тест обязан быть быстрым и без сети/DOM, но
// при этом мерить НЕ текст файла, а значения: «font-size: var(--font-size-3xl)» —
// это 36px при normal и 28px при dense. Живые getComputedStyle-числа снимаются
// отдельным живым замером на одноразовых контейнерах (см. отчёт R695).
//
// НЕ-ВАКУУМНОСТЬ. `WEBUI_ROOT=<копия git show 4613df08:webui>` даёт КРАСНЫЙ
// результат: там в health.html 22 объявления с px и ни одного токена.
//
// Запуск: node webui/tests/health-density-tokens-r695.test.js
//         WEBUI_ROOT=<dir> node webui/tests/health-density-tokens-r695.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const WEBUI = process.env.WEBUI_ROOT
    ? path.resolve(process.env.WEBUI_ROOT)
    : path.join(__dirname, '..');
const read = (rel) => fs.readFileSync(path.join(WEBUI, rel), 'utf8');

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

const html = read('health.html');
const themes = read('css/themes.css');

// --- разбор шкалы из themes.css --------------------------------------------

/** tokensIn — { '--font-size-xs': 11, ... } из фрагмента css (первое значение). */
function tokensIn(css) {
    const out = {};
    const re = /(--font-size-[a-z0-9]+)\s*:\s*([0-9.]+)px/gi;
    let m;
    while ((m = re.exec(css)) !== null) {
        if (!Object.prototype.hasOwnProperty.call(out, m[1])) out[m[1]] = Number(m[2]);
    }
    return out;
}

/** denseBlock — содержимое [data-density="dense"] { ... } (фигурные скобки считаем). */
function denseBlock(css) {
    const start = css.search(/\[data-density\s*=\s*"dense"\]\s*\{/);
    assert.ok(start !== -1, 'в css/themes.css нет блока [data-density="dense"]: плотность не работает вовсе');
    let i = css.indexOf('{', start);
    let depth = 0;
    for (let j = i; j < css.length; j++) {
        if (css[j] === '{') depth++;
        else if (css[j] === '}') {
            depth--;
            if (depth === 0) return css.slice(i, j + 1);
        }
    }
    throw new Error('незакрытый блок [data-density="dense"]');
}

const normalTokens = tokensIn(themes);
const denseTokens = Object.assign({}, normalTokens, tokensIn(denseBlock(themes)));

// --- разбор правил страницы -------------------------------------------------

/** ruleOf — тело css-правила по селектору внутри health.html. */
function ruleOf(selector) {
    const idx = html.indexOf(selector + ' {');
    assert.ok(idx !== -1, 'в health.html нет правила ' + selector);
    return html.slice(idx, html.indexOf('}', idx));
}

/** fontSizeToken — какой --font-size-* ставит правило. */
function fontSizeToken(selector) {
    const rule = ruleOf(selector);
    const m = /font-size\s*:\s*var\(\s*(--font-size-[a-z0-9]+)\s*\)/.exec(rule);
    assert.ok(m, 'у ' + selector + ' размер не из токена: ' + rule.trim());
    return m[1];
}

/** pxFor — значение токена в плотности. */
function pxFor(token, density) {
    const map = density === 'dense' ? denseTokens : normalTokens;
    const px = map[token];
    assert.strictEqual(typeof px, 'number',
        'токен ' + token + ' не определён в css/themes.css для плотности ' + density);
    return px;
}

// ===========================================================================
// 1. Статика: ни одного px-размера
// ===========================================================================
check('1a. в health.html нет ни одного font-size: <число>px (ни в стилях, ни в разметке)', function () {
    const bad = [];
    const re = /font-size\s*:\s*[0-9.]+px/gi;
    let m;
    while ((m = re.exec(html)) !== null) {
        const line = html.slice(0, m.index).split('\n').length;
        bad.push('строка ' + line + ': ' + m[0]);
    }
    assert.deepStrictEqual(bad, [],
        'эти размеры не подчиняются data-density=dense (страница останется «обычной» ' +
        'на фоне плотной панели): ' + bad.join('; '));
});

check('1b. размеры берутся из --font-size-* (положительный якорь)', function () {
    const used = new Set();
    const re = /font-size\s*:\s*var\(\s*(--font-size-[a-z0-9]+)\s*\)/gi;
    let m;
    while ((m = re.exec(html)) !== null) used.add(m[1]);
    assert.ok(used.size >= 5, 'переведено на токены всего ' + used.size + ' уникальных размеров: ' +
        [...used].join(', '));
});

// ===========================================================================
// 2. Каждый токен страницы сжимается плотностью
// ===========================================================================
check('2a. каждый --font-size-* страницы определён и имеет dense-значение', function () {
    const used = new Set();
    const re = /var\(\s*(--font-size-[a-z0-9]+)\s*\)/gi;
    let m;
    while ((m = re.exec(html)) !== null) used.add(m[1]);
    assert.ok(used.size > 0, 'страница не использует ни одного размера из шкалы');
    used.forEach(function (token) {
        assert.strictEqual(typeof normalTokens[token], 'number',
            'токен ' + token + ' используется, но не определён в css/themes.css');
        assert.strictEqual(typeof denseTokens[token], 'number',
            'у токена ' + token + ' нет значения в [data-density="dense"]: сжатия не будет');
        assert.ok(denseTokens[token] < normalTokens[token],
            'плотная версия ' + token + ' не меньше обычной: ' +
            normalTokens[token] + ' → ' + denseTokens[token]);
    });
});

check('2b. ключевые элементы: score и подпись карточки — токены и сжимаются', function () {
    // Оценка здоровья — метрика того же смысла, что цифры дашборда
    // (css/metrics.css:64), поэтому 3xl; было вписано 32px.
    assert.strictEqual(fontSizeToken('.health-card .value'), '--font-size-3xl');
    assert.strictEqual(pxFor('--font-size-3xl', 'normal'), 36);
    assert.strictEqual(pxFor('--font-size-3xl', 'dense'), 28);

    // Подпись карточки (и «sub») — мелкая подпись интерфейса, xs; было 12px.
    assert.strictEqual(fontSizeToken('.health-card .label'), '--font-size-xs');
    assert.strictEqual(fontSizeToken('.health-card .sub'), '--font-size-xs');
    assert.strictEqual(pxFor('--font-size-xs', 'normal'), 11);
    assert.strictEqual(pxFor('--font-size-xs', 'dense'), 10);

    // Заголовок страницы — как .header h1 панели (css/layout.css:274), 2xl; было 24px.
    assert.strictEqual(fontSizeToken('.health-header h1'), '--font-size-2xl');
    // Шапка секции — md; было 16px.
    assert.strictEqual(fontSizeToken('.health-section h2'), '--font-size-md');
    // Таблица: ячейки — sm, шапка — xs.
    const tableRule = ruleOf('.health-table th, .health-table td');
    assert.ok(/font-size\s*:\s*var\(\s*--font-size-sm\s*\)/.test(tableRule),
        'ячейки таблицы не на токене sm: ' + tableRule.trim());
    assert.strictEqual(fontSizeToken('.health-table th'), '--font-size-xs');
});

check('2c. мини-каскад: числа до/после для ключевых элементов (normal → dense)', function () {
    const targets = [
        ['.health-card .value', 'score'],
        ['.health-card .label', 'подпись карточки'],
        ['.health-card .sub', 'подпись под значением'],
        ['.health-header h1', 'заголовок страницы'],
        ['.health-section h2', 'шапка секции'],
        ['.health-table th', 'шапка таблицы'],
        ['.empty-state', 'пустое состояние']
    ];
    const report = [];
    targets.forEach(function (pair) {
        const selector = pair[0];
        const token = fontSizeToken(selector);
        const normal = pxFor(token, 'normal');
        const dense = pxFor(token, 'dense');
        report.push(pair[1] + ': ' + normal + 'px → ' + dense + 'px (' + token + ')');
        assert.ok(dense < normal, pair[1] + ' не сжимается: ' + normal + ' → ' + dense);
        // Уменьшение должно быть заметным, а не 1px: у dense шкала -15 %.
        assert.ok(dense <= Math.round(normal * 0.85) + 1,
            pair[1] + ': плотность почти не уменьшает размер (' + normal + ' → ' + dense + ')');
    });
    console.log('      ' + report.join('\n      '));
});

check('2d. плотность ставится на body документом страницы (иначе токены не применятся)', function () {
    assert.ok(/data-density/.test(html), 'health.html не ставит data-density');
    assert.ok(/document\.body\.setAttribute\('data-density'/.test(html),
        'плотность ставится не на body: [data-density] из themes.css матчится на любом ' +
        'элементе, но токены --font-size-* наследуются от предка');
    assert.ok(/ollamalegion_density/.test(html), 'плотность не читается из localStorage панели');
});

console.log('\n' + (failures.length ? 'FAILED: ' + failures.length : 'OK: ' + passed + ' checks passed'));
if (failures.length) {
    failures.forEach(function (f) { console.error('  FAIL: ' + f); });
    process.exit(1);
}
