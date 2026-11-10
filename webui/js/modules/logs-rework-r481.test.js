// logs-rework-r481.test.js — R481: страница логов и доступ к «Диагностике».
//
// Живые жалобы, которые держат эти проверки:
//   1. «не видно, как перейти на страницу Диагностика, чтобы получить архив
//      логов» — пункт меню уезжал за границу прокручиваемого nav;
//   2. «отображение не информативно, лишний текст, поля не корректно
//      заполняются» — поля события склеивались JSON-суффиксом в сообщение,
//      поиска и фильтра по уровню на странице не было;
//   3. «таблицы не дают рассмотреть ошибку полностью» — ячейка ошибки в
//      прокси-логах обрезалась, обещанный разворот отсутствовал;
//   4. «форматы по скачиванию» — у прокси-логов не было выгрузки файлом;
//   5. «локализация» — подписи были захардкожены (RU в EN-интерфейсе и English
//      в RU).
//
// Тест статический (как diagnostics-r476.test.js): проверяет разметку, код и
// словари, не поднимая браузер.
const assert = require('assert');
const fs = require('fs');
const path = require('path');

const root = path.resolve(__dirname, '..', '..', '..');
function read(rel) {
    return fs.readFileSync(path.join(root, rel), 'utf8');
}

const index = read('webui/index.html');
const renderers = read('webui/js/modules/renderers.js');
const appjs = read('webui/js/app.js');
const usersAdmin = read('webui/js/modules/users-admin.js');
const ru = read('webui/js/i18n/ru.js');
const en = read('webui/js/i18n/en.js');
const css = read('webui/css/pages.css');

let passed = 0;
function check(name, fn) {
    fn();
    passed++;
    console.log('  ✓ ' + name);
}

check('R481: ярлык «Диагностика» есть в подвале sidebar (виден без прокрутки)', () => {
    assert.ok(/sidebar-diag-link[\s\S]{0,400}data-page="diagnostics"/.test(index),
        'в sidebar-footer нет ярлыка на #diagnostics');
    assert.ok(/\.sidebar-diag-link/.test(css), 'нет стиля для ярлыка в подвале');
});

check('R481: из логов можно скачать архив диагностики и открыть страницу', () => {
    assert.ok(index.includes('id="logsDownloadBundle"'), 'нет кнопки «Архив диагностики»');
    assert.ok(index.includes('id="logsOpenDiagnostics"'), 'нет кнопки перехода на «Диагностику»');
    assert.ok(/logsDownloadBundle[\s\S]{0,400}Diagnostics\.downloadBundle/.test(appjs),
        'кнопка архива не вызывает Diagnostics.downloadBundle');
});

check('R481: системные логи фильтруются по уровню и поиску', () => {
    assert.ok(index.includes('id="logsSearch"'), 'нет поля поиска системных логов');
    assert.ok(/logsLevelFilter[\s\S]{0,400}window\.logFilters/.test(appjs),
        'селект уровня не пишет в window.logFilters');
    assert.ok(/logFieldsSearchTextR481/.test(renderers), 'поиск не учитывает поля события');
});

check('R481: поля события рисуются отдельным блоком, а не JSON-суффиксом', () => {
    assert.ok(/function logFieldPairsR481/.test(renderers), 'нет разбора полей события');
    assert.ok(/<details class="log-fields">/.test(renderers), 'нет раскрываемого блока полей');
    assert.ok(/\.log-fields-grid/.test(css), 'нет стилей блока полей');
});

check('R481: длинное сообщение разворачивается кликом, состояние видно', () => {
    assert.ok(/log-message-expanded/.test(renderers), 'нет переключения развёрнутого сообщения');
    assert.ok(/\.log-message-expanded/.test(css), 'нет CSS для развёрнутого сообщения');
});

check('R481: прокси-логи показывают request_id и стадию', () => {
    assert.ok(/logs\.proxy_request_id/.test(renderers), 'нет колонки/поля request_id');
    assert.ok(/logs\.proxy_stage/.test(renderers), 'нет колонки стадии');
    assert.ok(/e\.requestID/.test(renderers) && /e\.stage/.test(renderers),
        'рендер не читает requestID/stage из записи');
});

check('R481: полный текст ошибки раскрывается в блоке деталей', () => {
    assert.ok(/proxy-log-detail/.test(renderers), 'нет строки деталей');
    assert.ok(/proxy-log-error-full/.test(renderers), 'нет полного текста ошибки');
    assert.ok(/data-copy=/.test(renderers), 'нет кнопок копирования в деталях');
    assert.ok(/\.proxy-log-error-full/.test(css), 'нет стиля полного текста ошибки');
});

check('R481: у прокси-логов есть выгрузка файлом (CSV/JSON)', () => {
    assert.ok(index.includes('id="exportProxyLogs"'), 'нет кнопки скачивания прокси-логов');
    assert.ok(index.includes('id="proxyExportFormat"'), 'нет выбора формата');
    assert.ok(/function exportProxyLogs/.test(appjs), 'нет функции выгрузки');
    assert.ok(/ollamalegion-proxy-logs-/.test(appjs), 'нет имени файла выгрузки');
});

check('R481: подписи фильтров прокси-логов больше не английские в RU', () => {
    assert.ok(!/'2xx OK'|>2xx OK</.test(renderers), 'жёстко зашитая подпись 2xx OK в рендере');
    assert.ok(/"logs\.proxy_status_2xx"/.test(ru) && /"logs\.proxy_status_2xx"/.test(en),
        'нет ключа i18n для фильтра 2xx');
    assert.ok(/"logs\.proxy_status_4xx"/.test(ru) && /"logs\.proxy_status_5xx"/.test(en),
        'нет ключей i18n для фильтров 4xx/5xx');
});

check('R481: экспорт системных логов учитывает поиск и поля', () => {
    assert.ok(/logFieldsTextR481\(l\.fields\)/.test(appjs), 'экспорт не пишет поля');
    assert.ok(/searchEl[\s\S]{0,200}searchQuery/.test(appjs), 'экспорт не учитывает поиск');
});

check('R481: раздел «Пользователи» локализован', () => {
    assert.ok(/"settings\.users\.role_admin"/.test(ru) && /"settings\.users\.role_admin"/.test(en),
        'нет ключа роли администратора');
    assert.ok(/"settings\.users\.add"/.test(ru) && /"settings\.users\.add"/.test(en),
        'нет ключа кнопки добавления');
    assert.ok(!/textContent = 'Сменить пароль'/.test(usersAdmin),
        'строка «Сменить пароль» осталась захардкоженной');
    assert.ok(/window\.I18N\.t/.test(usersAdmin), 'модуль не использует i18n');
});

check('R481: ошибка сервера о users.json показывает подсказку (hint)', () => {
    assert.ok(/body\.hint/.test(usersAdmin), 'модуль не показывает подсказку сервера о пути/правах');
});

check('R481: compose-варианты передают LB_USERS_FILE на записываемый том', () => {
    ['deployments/docker-compose.cppworker-bundled-with-agent.yml',
     'deployments/docker-compose.bundled-full.yml',
     'deployments/docker-compose.cppworker-bundled.yml'].forEach((rel) => {
        const compose = read(rel);
        assert.ok(/LB_USERS_FILE=\$\{LB_USERS_FILE:-\/app\/data\/users\.json\}/.test(compose),
            rel + ': нет LB_USERS_FILE с дефолтом /app/data/users.json');
    });
});

check('R481: новые ключи i18n есть в обоих словарях', () => {
    ['logs.search_placeholder', 'logs.count', 'logs.expand', 'logs.fields',
     'logs.download_bundle', 'logs.open_diagnostics', 'logs.proxy_export',
     'logs.proxy_error', 'logs.proxy_no_match', 'logs.export_done',
     'settings.users.empty', 'settings.users.save_failed', 'settings.users.add',
     'nav.diagnostics_hint'].forEach((key) => {
        assert.ok(ru.includes('"' + key + '"'), 'RU: нет ключа ' + key);
        assert.ok(en.includes('"' + key + '"'), 'EN: нет ключа ' + key);
    });
});

console.log('\nOK: ' + passed + ' проверок');
