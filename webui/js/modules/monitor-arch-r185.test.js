// monitor-arch-r185.test.js — R185 (2026-10-20).
//
// Запуск: node webui/js/modules/monitor-arch-r185.test.js
//
// ЖИВОЙ РАЗБОР: образ cppworker собран под CUDA_ARCH=86;120, карта — Quadro
// RTX 4000 (sm_75). Воркер падал по SIGABRT на первом GPU-ядре, балансер
// повторял загрузку, а оператор видел только «не грузится».
//
// Что фиксируем:
//   1. при cuda_arch_ok=false над таблицей появляется явное уведомление: КАКОЙ
//      бэкенд, что образ собран под такую-то архитектуру, что загрузка отклонена
//      и что пересобирать;
//   2. при совместимом образе уведомления нет (нет ложной тревоги);
//   3. старый воркер без поля cuda_arch_ok не считается несовместимым;
//   4. опрос не дублируется: для несовместимого — раз в TTL, для совместимого —
//      больше не спрашиваем; ollama/image-бэкенды не опрашиваются вовсе.

'use strict';

const assert = require('assert');

global.window = global;
global.MonitorApp = {
    esc: function (s) { return String(s === null || s === undefined ? '' : s); },
    fmtDur: function () { return '-'; }
};
global.I18N = { t: function (k) { return k; } };

// DOM-заглушка: модуль пишет баннер в #backendsArchWarning.
const bannerEl = { innerHTML: '', style: {} };
const stubTbody = { innerHTML: '', querySelectorAll: function () { return []; } };
global.document = {
    querySelector: function (sel) {
        if (sel === '#backendsTable tbody') return stubTbody;
        return null;
    },
    getElementById: function (id) {
        if (id === 'backendsArchWarning') return bannerEl;
        return null;
    },
    querySelectorAll: function () { return []; },
    createElement: function () { return { style: {}, dataset: {}, addEventListener: function () {} }; },
    addEventListener: function () {}
};

const fs = require('fs');
const path = require('path');
const NodeModule = require('module');
const SRC = path.join(__dirname, 'monitor-backends.js');
const m = new NodeModule(SRC, module);
m._compile(fs.readFileSync(SRC, 'utf8') + '\nmodule.exports = MonitorBackends;', SRC);
const MB = m.exports;

let checks = 0;
function check(name, fn) { fn(); checks++; console.log('  ✓ ' + name); }

const BACKEND = { id: 'cppworker-cyborg-bundled-agent', type: 'llama_cpp' };

check('R185: несовместимая архитектура — уведомление над таблицей с причиной', function () {
    MB.rememberArchInfo(BACKEND.id, {
        cuda_arch_ok: false,
        cuda_arch_warning: 'образ собран под sm=120;86, карта sm=75',
        cuda_arch_built: ['75', '86', '120']
    });
    const html = MB.renderArchBanner([BACKEND]);
    assert.ok(html.length > 0, 'баннер пуст');
    assert.ok(/arch-warning/.test(html), 'нет класса arch-warning');
    assert.ok(html.indexOf(BACKEND.id) !== -1, 'не назван бэкенд');
    assert.ok(/sm=75/.test(html), 'не названа причина (карта sm_75)');
    assert.ok(/75;86;120/.test(html), 'не сказано, под что собран образ');
    assert.ok(/ОТКЛОНЕНА/.test(html), 'не сказано, что загрузка отклонена');
    assert.ok(/CUDA_ARCH/.test(html), 'не сказано, как исправить');
    assert.ok(bannerEl.innerHTML === html, 'баннер не попал в DOM');
    assert.ok(bannerEl.style.display === '', 'баннер остался скрытым');
});

check('R185: совместимый образ — уведомления нет', function () {
    MB.rememberArchInfo('ok-backend', {
        cuda_arch_ok: true, cuda_arch_warning: '', cuda_arch_built: ['75', '86', '120']
    });
    const html = MB.renderArchBanner([{ id: 'ok-backend', type: 'llama_cpp' }]);
    assert.strictEqual(html, '', 'ложная тревога на совместимом образе: ' + html);
    assert.strictEqual(bannerEl.style.display, 'none', 'баннер не скрыт');
});

check('R185: старый воркер без полей не считается несовместимым', function () {
    MB.rememberArchInfo('old-backend', { status: 'ok', version: '0.7.150' });
    const html = MB.renderArchBanner([{ id: 'old-backend', type: 'llama_cpp' }]);
    assert.strictEqual(html, '', 'старый образ ошибочно помечен несовместимым: ' + html);
});

check('R185: совместимый бэкенд больше не опрашивается, несовместимый — по TTL', function () {
    MB.rememberArchInfo('ok-backend', { cuda_arch_ok: true });
    // 'cppworker-cyborg...' был опрошен только что (rememberArchInfo не ставит
    // отметку времени — её ставит archProbeList), поэтому просим оба.
    const need = MB.archProbeList([
        { id: 'ok-backend', type: 'llama_cpp' },
        { id: BACKEND.id, type: 'llama_cpp' }
    ]);
    assert.ok(need.indexOf('ok-backend') === -1, 'совместимый бэкенд опрашивается зря');
    assert.ok(need.indexOf(BACKEND.id) !== -1, 'несовместимый бэкенд не опрошен ни разу');
    // Второй проход подряд: TTL ещё не истёк — повторов быть не должно.
    const again = MB.archProbeList([{ id: BACKEND.id, type: 'llama_cpp' }]);
    assert.strictEqual(again.length, 0, 'повторный опрос в пределах TTL: ' + again.join(','));
});

check('R185: ollama/image-бэкенды не опрашиваются (у них нет CUDA-архитектур)', function () {
    const need = MB.archProbeList([
        { id: 'ollama-1', type: 'ollama' },
        { id: 'image-1', type: 'image_cpp' }
    ]);
    assert.strictEqual(need.length, 0, 'опрашиваются лишние бэкенды: ' + need.join(','));
});

check('R185: два несовместимых бэкенда — два уведомления', function () {
    MB.rememberArchInfo('b1', { cuda_arch_ok: false, cuda_arch_warning: 'карта sm=75', cuda_arch_built: ['86'] });
    MB.rememberArchInfo('b2', { cuda_arch_ok: false, cuda_arch_warning: 'карта sm=61', cuda_arch_built: ['89'] });
    const html = MB.renderArchBanner([
        { id: 'b1', type: 'llama_cpp' },
        { id: 'b2', type: 'llama_cpp' }
    ]);
    assert.strictEqual((html.match(/arch-warning/g) || []).length, 2, 'не два уведомления: ' + html);
});

console.log('\nR185: ' + checks + ' проверок пройдено');
