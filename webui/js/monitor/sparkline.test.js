// sparkline.test.js — R91 (2026-10-08).
//
// Запуск: node webui/js/monitor/sparkline.test.js
//
// ЖАЛОБА ОПЕРАТОРА: «графики по загруженности на странице монитор выглядят не
// информативно» (скриншот плиток: тёмные «горбы» и по два процента на плитку).
//
// ЧТО БЫЛО НЕ ТАК (проверено дампом DOM Monitor):
//   1. шкала подгонялась под окно min..max, поэтому изменение с 1 % на 2 %
//      рисовалось во всю высоту, а 13 % было не отличить от 90 %;
//   2. подпись всегда добавляла '%': RPS показывался как «12.0%», среднее время
//      ответа — как «3450.0%»;
//   3. плоский ноль рисовал заливку — простаивающий бэкенд выглядел нагруженным;
//   4. min/avg/max были только в title (нужно навести мышь);
//   5. рисовались лишь 20 последних точек (100 с), хотя буфер хранит 200.
//
// Тест фиксирует свойства, из-за которых график стал читаемым: абсолютная шкала
// 0..100 для процентов, правильные единицы, сетка, строка сводки и честный ноль.

'use strict';

const assert = require('assert');

global.window = global;
// Стаб i18n с подстановкой {vars} — как в реальном I18N.t.
global.I18N = {
    t: function (key, vars) {
        const dict = {
            'monitor.sparkline.empty': 'Метрики ещё собираются…',
            'monitor.sparkline.caption': 'макс {max}',
            'metrics.gpuUtil': 'GPU Утилизация',
            'metrics.vramUsed': 'VRAM занято',
            'metrics.cpuUsed': 'CPU занято',
            'metrics.ramUsed': 'RAM занято',
            'metrics.rps': 'RPS',
            'metrics.avgRt': 'Среднее время ответа'
        };
        let s = Object.prototype.hasOwnProperty.call(dict, key) ? dict[key] : key;
        if (vars) {
            Object.keys(vars).forEach(function (k) {
                s = s.split('{' + k + '}').join(String(vars[k]));
            });
        }
        return s;
    }
};

require('./sparkline.js');
const S = global.window.Sparkline;
assert.ok(S, 'sparkline.js должен экспортировать window.Sparkline');
const pure = S.pure;

let checks = 0;
function check(name, fn) {
    fn();
    checks++;
    console.log('  ✓ ' + name);
}

// Достаём y-координаты точек из атрибута d (формат "Mx,y Lx,y …").
function pathYs(d) {
    return (d.match(/,([0-9.]+)/g) || []).map(function (s) { return parseFloat(s.slice(1)); });
}

check('единицы: проценты только у процентных метрик', function () {
    assert.strictEqual(pure.unitFor('gpu'), '%');
    assert.strictEqual(pure.unitFor('ram'), '%');
    // Регрессия: раньше подпись всегда добавляла '%'.
    assert.strictEqual(pure.unitFor('rps'), '', 'RPS не измеряется в процентах');
    assert.strictEqual(pure.unitFor('avgRt'), 'ms', 'среднее время ответа — миллисекунды');
});

check('формат значений: целые мс, десятые у RPS и процентов', function () {
    assert.strictEqual(pure.fmtVal(12.345, 'gpu'), '12.3%');
    assert.strictEqual(pure.fmtVal(3450.7, 'avgRt'), '3451ms');
    assert.strictEqual(pure.fmtVal(12.345, 'rps'), '12.3');
});

check('шкала процентов ВСЕГДА 0..100, а не по окну', function () {
    assert.strictEqual(pure.domain('gpu', 13), 100);
    assert.strictEqual(pure.domain('ram', 0.5), 100);
    // Неограниченные сверху метрики — 0..max с запасом, и не ноль.
    assert.ok(pure.domain('rps', 10) >= 10, 'верх шкалы RPS не может быть меньше максимума');
    assert.strictEqual(pure.domain('rps', 0), 1, 'нулевая серия не должна давать деление на ноль');
});

check('окно подписано честно и по РЕАЛЬНЫМ отметкам времени', function () {
    // Буфер наполняют два источника (поллер 5 с и рендер таблицы 2 с), поэтому
    // «точек × 5 с» — неправда. Считаем по меткам времени.
    assert.strictEqual(pure.windowLabel(1000, 3000, 3), '2 s');
    assert.strictEqual(pure.windowLabel(1000, 121000, 60), '2 min');
    // Отметки времени есть, но все точки в одну миллисекунду — «меньше секунды»,
    // а не повод принять метку времени за количество (живой баг теста:
    // получалось «149291437987 min»).
    assert.strictEqual(pure.windowLabel(5000, 5000, 3), '<1 s');
    // Отметок нет вовсе — только тогда оценка по числу точек.
    assert.strictEqual(pure.windowLabel(undefined, undefined, 2), '5 s');
});

check('мелкие, но ненулевые проценты не показываются как ноль', function () {
    assert.strictEqual(pure.fmtShort(0, 'gpu'), '0%');
    assert.strictEqual(pure.fmtShort(0.4, 'gpu'), '<1%', '0.4% — это не 0%');
    assert.strictEqual(pure.fmtShort(13.4, 'gpu'), '13%');
    assert.strictEqual(pure.fmtShort(3450.7, 'avgRt'), '3451ms');
});

// ─── Разметка ───────────────────────────────────────────────────────────────

function renderSeries(backendId, key, values) {
    S.reset();
    for (let i = 0; i < values.length; i++) {
        const m = {};
        m[key] = values[i];
        S.recordMetricsHistory(backendId, m);
    }
    const { I18N } = global; // eslint-disable-line no-unused-vars
    return S.render(backendId, key, '#2dd4bf');
}

check('13 % на шкале 0..100 — низко, а не «во всю высоту»', function () {
    const html = renderSeries('bk', 'gpu', [0, 0, 13, 13, 13]);
    const m = html.match(/class="sparkline-path" d="([^"]+)"/);
    assert.ok(m, 'нет линии графика: ' + html);
    const ys = pathYs(m[1]);
    const maxY = Math.min.apply(null, ys); // меньший y = выше на картинке
    assert.ok(maxY > 12,
        'точка 13 % нарисована слишком высоко (y=' + maxY + '): шкала снова подогнана под окно');
    assert.ok(maxY < 24, 'точка 13 % ушла за нижнюю границу (y=' + maxY + ')');
});

check('100 % заполняет высоту — шкала читается как доля от максимума', function () {
    const html = renderSeries('bk', 'gpu', [0, 100]);
    const m = html.match(/class="sparkline-path" d="([^"]+)"/);
    const ys = pathYs(m[1]);
    assert.ok(Math.min.apply(null, ys) <= 4, '100 % должно доходить до верха: ' + JSON.stringify(ys));
});

check('опорная линия 50 % есть у процентов и не нужна у RPS', function () {
    const pct = renderSeries('bk', 'gpu', [0, 10, 20]);
    assert.strictEqual((pct.match(/sparkline-grid/g) || []).length, 1,
        'ожидалась одна опорная линия на половине шкалы');
    const rps = renderSeries('bk', 'rps', [0, 5, 9]);
    assert.strictEqual((rps.match(/sparkline-grid/g) || []).length, 0,
        'у неограниченной метрики опорная линия 50 % бессмысленна');
});

check('пик за окно отмечен риской максимума', function () {
    const html = renderSeries('bk', 'gpu', [5, 40, 5]);
    assert.ok(/sparkline-max/.test(html), 'риска максимума не нарисована — пик не видно');
});

check('строка сводки короткая: только максимум', function () {
    const html = renderSeries('bk', 'ram', [10, 20, 30]);
    const m = html.match(/class="sparkline-caption">([^<]+)</);
    assert.ok(m, 'нет строки сводки: ' + html);
    assert.ok(/макс 30%/.test(m[1]), 'в сводке нет максимума: ' + m[1]);
    // Строка живёт в узкой ячейке: длинный текст вылезал в соседнюю колонку.
    assert.ok(m[1].length <= 12, 'строка сводки слишком длинная для ячейки: «' + m[1] + '»');
});

check('простаивающий бэкенд не получает строку сводки', function () {
    const html = renderSeries('bk', 'gpu', [0, 0, 0]);
    assert.strictEqual(html.indexOf('sparkline-caption'), -1,
        '«макс 0%» про простой — шум, строки быть не должно');
});

check('окно наблюдения остаётся в подсказке', function () {
    const html = renderSeries('bk', 'gpu', [0, 10, 20]);
    const m = html.match(/title="([^"]+)"/);
    assert.ok(m && /\d+ (s|min)/.test(m[1]), 'в подсказке нет окна: ' + (m && m[1]));
    assert.ok(m && /avg 10\.0%/.test(m[1]), 'в подсказке нет среднего: ' + m[1]);
});

check('плоский ноль рисуется БЕЗ заливки (иначе выглядит как нагрузка)', function () {
    const html = renderSeries('bk', 'gpu', [0, 0, 0, 0]);
    assert.strictEqual(html.indexOf('sparkline-area'), -1,
        'под нулевой линией снова появилась заливка — простаивающий бэкенд будет выглядеть нагруженным');
    assert.ok(/sparkline-path/.test(html), 'линия нуля должна остаться');
    assert.ok(/0\.0%/.test(html), 'подпись текущего значения потерялась');
});

check('подпись содержит правильную единицу для каждой метрики', function () {
    assert.ok(/sparkline-label">13\.0%</.test(renderSeries('bk', 'vram', [1, 13])));
    assert.ok(/sparkline-label">42ms</.test(renderSeries('bk', 'avgRt', [1, 42])),
        'время ответа подписано не миллисекундами');
    assert.ok(/sparkline-label">3\.5</.test(renderSeries('bk', 'rps', [1, 3.5])),
        'RPS подписан процентом');
});

check('меньше двух точек — честный empty-state, а не пустой график', function () {
    S.reset();
    S.recordMetricsHistory('bk', { gpu: 5 });
    const html = S.render('bk', 'gpu', '#2dd4bf');
    assert.ok(/sparkline-empty/.test(html), 'нет empty-state');
    assert.ok(/Метрики ещё собираются/.test(html), 'текст empty-state потерялся: ' + html);
});

check('в окно попадают последние 60 точек, а не 20', function () {
    S.reset();
    for (let i = 0; i < 120; i++) S.recordMetricsHistory('bk', { gpu: i % 100 });
    const html = S.render('bk', 'gpu', '#2dd4bf');
    const m = html.match(/class="sparkline-path" d="([^"]+)"/);
    const points = (m[1].match(/L/g) || []).length + 1;
    assert.strictEqual(points, S._WINDOW_POINTS, 'нарисовано ' + points + ' точек, ожидалось ' + S._WINDOW_POINTS);
    assert.strictEqual(S._WINDOW_POINTS, 60);
});

check('кластерный спарклайн показывает и среднее, и максимум', function () {
    const html = S.renderCluster('gpu', 10, 40, '#2dd4bf');
    assert.ok(/sparkline-max/.test(html), 'риска максимума не нарисована — avg и max не различить');
    const noMax = S.renderCluster('gpu', 40, 40, '#2dd4bf');
    assert.strictEqual(noMax.indexOf('sparkline-max'), -1,
        'при avg == max риска не нужна');
});

console.log('\nOK: ' + checks + ' checks passed');
