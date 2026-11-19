// first-run-card.js — R596 (2026-11-19): карточка первого запуска вместо мастера.
//
// ЗАЧЕМ (решение владельца 2026-11-19). Мастер `setup-wizard.js` (739 строк, 4 шага)
// спрашивал то, чего в продукте больше нет: выбор режима удалён (R107/R560–R567),
// тип бэкенда выводится из состава (R112/R195), движок не выбирается. Оператор
// проходил четыре шага, сводку и импорт файла ради ТРЁХ чисел и флага. Здесь —
// один экран: три реально влияющих порога + «Готово» + ссылка «настроить подробно».
//
// ЧТО ИМЕННО ЗАПИСЫВАЕТСЯ И ПОЧЕМУ ИМЕННО ЭТО (проверено по коду):
//   gpuMaxUsage  → `slot_manager.go` (порог занятости слота)
//   cpuMaxUsage  → `slot_manager.go`
//   ramMaxUsage  → `slot_manager.go`
//   vramMaxUsage → НЕ пишется: единственное упоминание на сервере — pass-through в
//                  `handlers_cluster.go`, ни одно решение его не читает. Мастер
//                  писал его годами, и оно создавало ложную картину «настройка
//                  применена».
//   initialized  → флаг «первый запуск пройден» (config.json), по нему карточка и
//                  скрывается. Ставится ОДНИМ запросом вместе с порогами.
//
// Экспорт: window.FirstRunCard = { check, show, hide, mount, submit,
//   buildPayload, render, _isVisible }.
(function () {
    'use strict';

    var DEFAULTS = { gpuMaxUsage: 90, cpuMaxUsage: 80, ramMaxUsage: 85 };
    var CARD_ID = 'firstRunCard';
    var LS_KEY = 'ollamalegion_wizard_done';

    function t(key, fallback, vars) {
        if (window.I18N && typeof window.I18N.t === 'function') return window.I18N.t(key, vars);
        return String(fallback).replace(/\{(\w+)\}/g, function (m, k) {
            return (vars && vars[k] !== undefined) ? String(vars[k]) : m;
        });
    }

    function esc(s) {
        if (s === null || s === undefined) return '';
        return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;')
            .replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }

    // ---- чистые функции (тестируются без DOM) ----------------------------

    // clampPercent — порог — это доля, а не что угодно: вне 1..100 значение
    // отвергается и берётся дефолт. Молча записать 0 или 1000 нельзя: 0 отключил бы
    // учёт занятости, 1000 — разрешил бы всё.
    function clampPercent(value, fallback) {
        var n = parseInt(value, 10);
        if (!isFinite(n) || n < 1 || n > 100) return fallback;
        return n;
    }

    // buildPayload — единственный запрос первого запуска.
    //
    // Контракт (проверяется тестом): ровно ТРИ порога + initialized; ни
    // `vramMaxUsage` (нет потребителя), ни `operatingMode`, ни `backendEngine`
    // (сняты раньше — R107/R543) здесь быть не должно.
    function buildPayload(fields) {
        var f = fields || {};
        return {
            gpuMaxUsage: clampPercent(f.gpuMaxUsage, DEFAULTS.gpuMaxUsage),
            cpuMaxUsage: clampPercent(f.cpuMaxUsage, DEFAULTS.cpuMaxUsage),
            ramMaxUsage: clampPercent(f.ramMaxUsage, DEFAULTS.ramMaxUsage),
            initialized: true
        };
    }

    // render — разметка карточки. Три поля, кнопка и ссылка на подробные настройки.
    function render(values) {
        var v = values || DEFAULTS;
        return '' +
            '<div class="card first-run-card" id="' + CARD_ID + '" role="dialog" aria-labelledby="firstRunTitle">' +
            '<div class="card-header">' +
            '<h3 id="firstRunTitle"><i class="fas fa-rocket section-icon-i"></i> ' +
            esc(t('firstrun.title', 'Первый запуск')) + '</h3>' +
            '</div>' +
            '<div class="card-body">' +
            '<p class="first-run-hint">' +
            esc(t('firstrun.hint', 'Три порога, по которым балансер решает, можно ли ещё нагружать бэкенд. Остальное — по умолчанию; подробности в «Настройках».')) +
            '</p>' +
            '<div class="first-run-fields" style="display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:0.6rem;">' +
            field('gpuMaxUsage', t('firstrun.gpu', 'Занятость GPU, %'), v.gpuMaxUsage) +
            field('cpuMaxUsage', t('firstrun.cpu', 'Занятость CPU, %'), v.cpuMaxUsage) +
            field('ramMaxUsage', t('firstrun.ram', 'Занятость RAM, %'), v.ramMaxUsage) +
            '</div>' +
            '<div class="first-run-actions" style="margin-top:0.9rem;display:flex;gap:0.6rem;align-items:center;flex-wrap:wrap;">' +
            '<button type="button" class="btn btn-primary" id="firstRunDone">' +
            esc(t('firstrun.done', 'Готово')) + '</button>' +
            '<a href="#settings" class="first-run-more" id="firstRunMore">' +
            esc(t('firstrun.more', 'Настроить подробно')) + '</a>' +
            '<span class="first-run-error" id="firstRunError" style="display:none;color:var(--danger,#f85149);"></span>' +
            '</div>' +
            '</div></div>';
    }

    function field(id, label, value) {
        return '<label class="form-group"><span class="form-label">' + esc(label) + '</span>' +
            '<input type="number" class="form-control" id="firstRun-' + id + '" data-field="' + id + '"' +
            ' min="1" max="100" step="1" value="' + esc(value) + '"></label>';
    }

    // ---- состояние -------------------------------------------------------

    var shown = false;

    function byId(id) { return document.getElementById(id); }

    function isVisible() { return !!byId(CARD_ID); }

    function hide() {
        var el = byId(CARD_ID);
        if (el && el.parentNode) el.parentNode.removeChild(el);
        shown = false;
    }

    function show() {
        if (isVisible()) return false;
        var host = byId('firstRunHost');
        if (!host) return false;
        host.innerHTML = render(DEFAULTS);
        shown = true;
        return true;
    }

    function setError(msg) {
        var el = byId('firstRunError');
        if (!el) return;
        if (!msg) { el.style.display = 'none'; el.textContent = ''; return; }
        el.textContent = msg;
        el.style.display = 'inline';
    }

    function readFields() {
        var out = {};
        ['gpuMaxUsage', 'cpuMaxUsage', 'ramMaxUsage'].forEach(function (name) {
            var el = byId('firstRun-' + name);
            out[name] = el ? el.value : undefined;
        });
        return out;
    }

    // submit — один PUT. `initialized: true` ставится ВМЕСТЕ с порогами, иначе
    // карточка исчезнет, а значения не применятся (или наоборот).
    function submit() {
        var payload = buildPayload(readFields());
        if (!window.Api || typeof window.Api.updateConfig !== 'function') {
            setError(t('firstrun.error_api', 'Слой HTTP не загружен — обновите страницу'));
            return Promise.reject(new Error('Api.updateConfig unavailable'));
        }
        return window.Api.updateConfig(payload).then(function () {
            try { localStorage.setItem(LS_KEY, '1'); } catch (e) { /* приватный режим */ }
            hide();
            if (window.App && typeof window.App.refresh === 'function') {
                try { window.App.refresh(); } catch (e) { /* обновление не обязательно */ }
            }
            return payload;
        }).catch(function (err) {
            setError(t('firstrun.error_save', 'Не удалось сохранить') + ': ' + (err && err.message ? err.message : String(err)));
            throw err;
        });
    }

    // check — показывать ли карточку. Источник истины — СЕРВЕР (config.json);
    // localStorage — только запасной путь, когда конфиг недоступен.
    function check() {
        mount();
        if (!window.Api || typeof window.Api.config !== 'function') return Promise.resolve(false);
        return window.Api.config().then(function (cfg) {
            var done = !!cfg && cfg.initialized === true;
            if (done) {
                try { localStorage.setItem(LS_KEY, '1'); } catch (e) { /* ignore */ }
                hide();
                return false;
            }
            show();
            return true;
        }).catch(function () {
            var done = false;
            try { done = localStorage.getItem(LS_KEY) === '1'; } catch (e) { /* ignore */ }
            if (!done) show();
            return !done;
        });
    }

    var mounted = false;

    function mount() {
        if (mounted) return false;
        var host = byId('firstRunHost');
        if (!host) return false;
        // Делегирование: разметка перерисовывается, а слушатель — один.
        host.addEventListener('click', function (ev) {
            var target = ev.target;
            if (!target || !target.id) return;
            if (target.id === 'firstRunDone') { submit(); }
        });
        mounted = true;
        return true;
    }

    window.FirstRunCard = {
        check: check,
        show: show,
        hide: hide,
        mount: mount,
        submit: submit,
        buildPayload: buildPayload,
        render: render,
        clampPercent: clampPercent,
        _isVisible: isVisible,
        _defaults: DEFAULTS
    };
})();
