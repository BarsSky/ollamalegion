// js/modules/form-dirty.js — R662 (2026-11-20): незавершённый ввод защищает
// страницу от авто-обновления, а не только фокус в поле.
//
// ЖИВАЯ ЖАЛОБА (продолжение R512). «Постоянное обновление WebUI не даёт
// взаимодействовать с формами». R512 закрыл только случай «оператор ПЕЧАТАЕТ
// прямо сейчас» (документ.activeElement — редактируемое поле активной страницы).
// Дырка осталась ровно в обычном сценарии заполнения: набрал значение → кликнул
// чекбокс / вкладку / кнопку / просто в пустое место → фокус уже НЕ в текстовом
// поле → следующий тик (раз в 5 с) перерисовывает страницу и затирает набранное.
//
// ЧТО ДЕЛАЕТ ЭТОТ МОДУЛЬ. Помнит, когда оператор последний раз ЧТО-ТО менял на
// странице (input/change по редактируемому полю): `isDirtyPage('settings')` →
// true, пока с последней правки не прошло TTL. Тик пропускается (как и при
// фокусе) — то есть DOM не перерисовывается и набранное не теряется.
//
// ПОЧЕМУ TTL, А НЕ «флаг dirty до сохранения». Это прямой ответ на возражение
// R512 («флаг dirty без надёжного сброса замораживает страницу навсегда»):
//   - TTL (2 минуты) гарантирует, что авто-обновление вернётся само, даже если
//     оператор ушёл, а страница осталась открытой;
//   - явные сбросы — submit формы и кнопки сохранения/применения
//     (`noteSave`) — снимают защиту сразу, не дожидаясь TTL;
//   - навигация по страницам сбрасывает состояние (`clearAll` из app.js).
// Компромисс осознанный: до 2 минут после последней правки данные на странице
// могут отставать — это лучше, чем стереть незавершённое заполнение формы.
(function () {
    'use strict';

    // TTL защиты: 2 минуты после последней правки.
    var DEFAULT_TTL_MS = 120000;

    // Типы input, в которых идёт НАБОР (совпадает с refresh-guard.js).
    var EDITABLE_INPUT_TYPES = {
        text: true, password: true, email: true, number: true, search: true,
        tel: true, url: true, date: true, 'datetime-local': true, month: true,
        time: true, week: true, range: true, color: true, file: true
    };

    var pageEdits = Object.create(null); // pageId -> время последней правки (ms)
    var ttlMs = DEFAULT_TTL_MS;
    var nowFn = function () { return Date.now(); };

    function isEditable(el) {
        if (!el || typeof el !== 'object') { return false; }
        if (el.isContentEditable === true) { return true; }
        var tag = (el.tagName || '').toUpperCase();
        if (tag === 'TEXTAREA' || tag === 'SELECT') { return true; }
        if (tag === 'INPUT') {
            var type = String(el.type || 'text').toLowerCase();
            return EDITABLE_INPUT_TYPES[type] === true;
        }
        return false;
    }

    /**
     * pageIdOf — id страницы-контейнера для узла (`settings-page` и т.п.).
     *
     * Контракт совпадает с тиком автообновления: он ищет `currentPage + '-page'`
     * через document.getElementById. Поэтому ключом служит сам id контейнера:
     * одно и то же имя не может означать разные страницы.
     */
    function pageIdOf(node) {
        var el = node;
        var guard = 0;
        while (el && guard < 100) {
            guard++;
            if (el.id && /-page$/.test(String(el.id))) { return String(el.id); }
            el = el.parentNode || null;
        }
        return '';
    }

    /** noteEdit — запомнить правку внутри страницы узла. */
    function noteEdit(node) {
        var id = pageIdOf(node);
        if (!id) { return ''; }
        pageEdits[id] = nowFn();
        return id;
    }

    /** noteSave — сохранение/применение: защита снимается сразу. */
    function noteSave(pageElOrId) {
        var id = '';
        if (typeof pageElOrId === 'string') {
            id = pageElOrId;
        } else if (pageElOrId && typeof pageElOrId === 'object') {
            id = pageIdOf(pageElOrId);
        }
        if (!id) { return; }
        delete pageEdits[id];
    }

    /** isDirtyPage — есть ли свежая несохранённая правка на странице. */
    function isDirtyPage(pageIdOrName, atMs) {
        if (!pageIdOrName) { return false; }
        var id = String(pageIdOrName);
        if (id.slice(-5) !== '-page') { id = id + '-page'; }
        var at = pageEdits[id];
        if (!at) { return false; }
        var now = (typeof atMs === 'number') ? atMs : nowFn();
        if (ttlMs > 0 && now - at > ttlMs) {
            delete pageEdits[id];
            return false;
        }
        return true;
    }

    /** clear — снять защиту со страницы (id или имя). */
    function clear(pageIdOrName) {
        if (!pageIdOrName) { return; }
        var id = String(pageIdOrName);
        if (id.slice(-5) !== '-page') { id = id + '-page'; }
        delete pageEdits[id];
    }

    /** clearAll — сброс при навигации/выходе из раздела. */
    function clearAll() {
        pageEdits = Object.create(null);
    }

    /** setTTLForTest — только для node-тестов (в браузере не используется). */
    function setTTLForTest(ms) {
        ttlMs = (typeof ms === 'number' && ms >= 0) ? ms : DEFAULT_TTL_MS;
    }

    /** setNowForTest — подмена часов в node-тестах. */
    function setNowForTest(fn) {
        nowFn = (typeof fn === 'function') ? fn : function () { return Date.now(); };
    }

    function onEdit(event) {
        if (!event || !isEditable(event.target)) { return; }
        noteEdit(event.target);
    }

    if (typeof document !== 'undefined' && document && typeof document.addEventListener === 'function') {
        // capture: правки ловятся и там, где обработчик остановил всплытие.
        document.addEventListener('input', onEdit, true);
        document.addEventListener('change', onEdit, true);
        // submit — штатное сохранение формы: защита больше не нужна.
        document.addEventListener('submit', function (event) {
            noteSave(event ? event.target : '');
        }, true);
    }

    var api = {
        DEFAULT_TTL_MS: DEFAULT_TTL_MS,
        isEditable: isEditable,
        pageIdOf: pageIdOf,
        noteEdit: noteEdit,
        noteSave: noteSave,
        isDirtyPage: isDirtyPage,
        clear: clear,
        clearAll: clearAll,
        setTTLForTest: setTTLForTest,
        setNowForTest: setNowForTest
    };
    if (typeof window !== 'undefined') {
        window.FormDirty = api;
    }
    if (typeof module !== 'undefined' && module.exports) {
        module.exports = api; // node-тесты (webui/tests) без DOM
    }
})();
