// image-master-detail-r747.test.js — R747: страница Image получила ту же
// раскладку, что GGUF: слева список бэкендов, справа табы выбранного узла.
//
// ЖИВЫЕ ПРЕТЕНЗИИ, ЗАКРЕПЛЁННЫЕ ЗДЕСЬ:
//   * «нет выбора бэкенда, непонятно, что к чему относится» — бэкенд выбирался
//     селектом в шапке, а «Модели на диске» / «Загрузки» / «Настройки» жили
//     отдельным потоком;
//   * «повторите отображение как в gguf models» — список бэкендов слева,
//     настройки/табы справа;
//   * «нет управления группой» — это следующий этап, но раскладка уже должна
//     оставлять место под инвентарь выбранного узла.
//
// Тест статический: он фиксирует связку index.html ↔ image-page.js ↔ CSS ↔ i18n.
// Поведение (клик по бэкенду меняет выбор и перегружает модели) проверяется
// живым прогоном на стенде.
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

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

const ROOT = path.join(__dirname, '..', '..');
const read = (rel) => fs.readFileSync(path.join(ROOT, rel), 'utf8');
const html = read('webui/index.html');
const page = read('webui/js/modules/image-page.js');
const css = read('webui/css/pages.css');
const ru = read('webui/js/i18n/ru.js');
const en = read('webui/js/i18n/en.js');

check('index.html: мастер-детейл объявлен, табы ВНУТРИ правой панели', function () {
    assert.ok(html.indexOf('class="im-master-layout"') >= 0, 'нет .im-master-layout');
    const iLayout = html.indexOf('class="im-master-layout"');
    const iTabs = html.indexOf('id="imTabs"');
    assert.ok(iTabs > iLayout, 'табы остались выше мастер-раскладки');
    const iDetail = html.indexOf('class="im-detail-panel"', iLayout);
    assert.ok(iDetail > 0 && iDetail < iTabs, 'табы не внутри .im-detail-panel');
    // Панель должна закрываться ДО следующей страницы, иначе вложенность ломается.
    const iMonitor = html.indexOf('id="monitor-page"');
    const tail = html.slice(iTabs, iMonitor);
    assert.ok((tail.match(/<\/div>/g) || []).length >= 4,
        'похоже, мастер-раскладка не закрыта перед следующей страницей');
});

check('index.html: список бэкендов слева и кнопка обновления', function () {
    assert.ok(/id="imBackendList"/.test(html), 'нет контейнера списка бэкендов');
    // R493: отдельной кнопки «Обновить» быть не должно (общая кнопка шапки).
    assert.ok(!/id="imBackendListRefresh"/.test(html), 'появилась вторая кнопка «Обновить»');
    assert.ok(/data-i18n="image\.backend"/.test(html), 'заголовок списка не переводится');
});

check('index.html: селект бэкенда остался в DOM, но скрыт (совместимость модулей/тестов)', function () {
    const m = html.match(/<select[^>]*id="imgBackendSelect"[^>]*>/);
    assert.ok(m, 'селект #imgBackendSelect пропал — его читают image-page.js и тесты');
    assert.ok(/display:none/.test(m[0]), 'селект не скрыт — в шапке два выбора бэкенда одновременно');
});

check('image-page.js: список рисуется и клик меняет выбор', function () {
    assert.ok(/function renderBackendList\(/.test(page), 'нет renderBackendList');
    assert.ok(/renderBackendList\(\);/.test(page), 'список не вызывается при рендере селекта');
    assert.ok(/data-im-backend-id/.test(page), 'элементы списка не несут id бэкенда');
    assert.ok(/state\.selectedBackendId = id;/.test(page), 'клик по списку не обновляет выбранный бэкенд');
    assert.ok(/sel\.value = id;/.test(page), 'клик не синхронизирует скрытый #imgBackendSelect');
});

check('CSS: две колонки и активный элемент списка', function () {
    assert.ok(/\.im-master-layout\s*\{[^}]*grid-template-columns:\s*260px/.test(css),
        'нет сетки «список + панель»');
    assert.ok(/\.im-backend-item\.active/.test(css), 'нет подсветки выбранного бэкенда');
    assert.ok(/\.im-backend-dot\.ok/.test(css) && /\.im-backend-dot\.bad/.test(css),
        'нет индикаторов статуса бэкенда');
});

check('i18n: ключи списка есть в ru и en', function () {
    ['imageModels.no_backends', 'imageModels.backend_models',
        'imageModels.backend_vram_tpl'].forEach(function (k) {
        assert.ok(ru.indexOf('"' + k + '"') >= 0, 'нет ' + k + ' в ru.js');
        assert.ok(en.indexOf('"' + k + '"') >= 0, 'нет ' + k + ' в en.js');
    });
});

if (failures.length) {
    console.error('\nFAIL: ' + failures.length + ' красных, passed=' + passed);
    process.exit(1);
}
console.log('\nOK: ' + passed + ' проверок (R747)');
