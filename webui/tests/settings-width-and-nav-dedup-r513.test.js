// settings-width-and-nav-dedup-r513.test.js — R513 (2026-11-19): два замечания
// оператора из docs/plans/2026-11-19-webui-and-balancer-fixes.md.
//
//   1. «Настройки жмутся в узкую полосу». Причина: у `.settings-form` стоял
//      `max-width: 600px` без `margin: auto`. На экране 1920 px карточка 1612 px,
//      а содержимое — 600 px, справа ≈1012 px пустоты; двойной отступ (24 + 24)
//      оставлял рабочую ширину ≈504 px, в которую не влезали `.theme-picker`
//      (7 колонок), `.mode-cards` (`minmax(180px, 1fr)`) и `.form-row`
//      (`1fr 1fr`) — они сваливались в один столбец. Медиа-правил для формы не
//      было, поэтому 600 px действовали и на 4K.
//
//   2. «Две Диагностики в меню». Пункт списка (`index.html:125`) дублировал
//      ярлык в подвале (`R481`, `.sidebar-diag-link`). Удалён пункт СПИСКА:
//      ярлык в подвале вне прокручиваемого `nav` и виден всегда — именно ради
//      этого R481 его и добавлял (на невысоком экране пункт списка уезжал за
//      границу и его не находили).
//
// Тест статический (разметка + CSS), браузер не поднимается — как
// sidebar-nav-r497.test.js и logs-rework-r481.test.js.
const assert = require('assert');
const fs = require('fs');
const path = require('path');

const root = path.join(__dirname, '..');
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

const css = read('css/pages.css');
const html = read('index.html');

// settingsFormBlock — объявление `.settings-form { ... }` (одно объявление).
function settingsFormBlock() {
    const m = /\.settings-form\s*\{([^}]*)\}/.exec(css);
    assert.ok(m, 'в pages.css нет объявления .settings-form — сторож потерял точку отсчёта');
    return m[1];
}

check('R513/1: у формы настроек нет ограничения ширины', () => {
    assert.ok(!/max-width/.test(settingsFormBlock()),
        '.settings-form снова ограничен по ширине: ' + settingsFormBlock().trim() +
        ' — форма обязана занимать всю карточку, иначе на 1920 полоса ≈504 px');
});

check('R513/1: форма настроек по-прежнему с отступами (не «прилипает» к краю)', () => {
    assert.ok(/padding\s*:\s*24px/.test(settingsFormBlock()),
        '.settings-form потерял padding — содержимое прилипнет к границе карточки');
});

check('R513/2: «Диагностика» объявлена в меню РОВНО один раз', () => {
    const list = html.match(/<a[^>]*data-page="diagnostics"[^>]*>/g) || [];
    assert.strictEqual(list.length, 1,
        'пунктов с data-page="diagnostics" в index.html: ' + list.length +
        ' (ожидается 1) — второй снова виден в меню как дубль');
});

check('R513/2: единственный пункт «Диагностики» — ярлык в подвале (виден без прокрутки)', () => {
    const m = /<a[^>]*data-page="diagnostics"[^>]*>/.exec(html);
    assert.ok(m, 'пункта «Диагностика» нет вовсе');
    assert.ok(/sidebar-diag-link/.test(m[0]),
        'пункт «Диагностика» снова в прокручиваемом списке: ' + m[0] +
        ' — на невысоком экране он уедет за границу nav (живой дефект R481)');
});

check('R513/2: остальные страницы меню не потеряны', () => {
    // Список сверяется с pageFromURL/страницами приложения: удалять из меню
    // можно только дубль, а не саму страницу.
    const pages = ['dashboard', 'monitor', 'backends', 'models', 'sessions', 'queue',
        'gguf', 'image', 'virtual-models', 'agents', 'logs', 'health', 'reference', 'settings'];
    const missing = pages.filter((p) => html.indexOf('data-page="' + p + '"') === -1);
    assert.deepStrictEqual(missing, [], 'из меню пропали страницы: ' + missing.join(', '));
});

if (failures.length) {
    console.error('\nИТОГ: ' + failures.length + ' проверок красных');
    process.exit(1);
}
console.log('\nИТОГ: ' + passed + ' проверок зелёные');
