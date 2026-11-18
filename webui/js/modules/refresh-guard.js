// js/modules/refresh-guard.js — R512 (2026-11-18): авто-обновление не имеет права
// перерисовывать страницу, пока оператор РЕДАКТИРУЕТ форму.
//
// ЖИВАЯ ЖАЛОБА. «Постоянное обновление WebUI держит новую информацию, но не даёт
// нормально взаимодействовать с формами — постоянно их скрывая». Механизм:
// app.js → startPeriodicRefresh раз в 5 секунд вызывает refreshPage(currentPage), а
// для страницы настроек это loadSettings() + SettingsRegistry.load() +
// loadBackendLimits() + AutoTuneSettings.init(). Каждый такой вызов заново
// заполняет поля из ответа сервера: набранный символ затирался, курсор уезжал,
// раскрытые секции схлопывались — форма выглядела «пропавшей».
//
// ЧТО ДЕЛАЕТ ЭТОТ МОДУЛЬ. Отвечает на один вопрос: «оператор сейчас в поле ввода
// активной страницы?». Таймер при «да» пропускает тик (данные не перечитываются и
// DOM не перерисовывается), при «нет» работает как раньше.
//
// ПОЧЕМУ ТОЛЬКО ФОКУС, А НЕ «ЕСТЬ НЕСОХРАНЁННЫЕ ИЗМЕНЕНИЯ». Флаг «dirty» без
// надёжного сброса замораживает страницу навсегда (оператор ушёл, а авто-обновление
// стоит), а без сохранения набранное всё равно теряется при следующей перерисовке.
// Фокус — единственное состояние, которое точно означает «прямо сейчас идёт ввод».
(function () {
    'use strict';

    // Типы input, в которых идёт НАБОР. checkbox/radio/button/submit/reset —
    // это клики; их перерисовка не мешает (значение ставится кликом и сразу уходит
    // в форму), поэтому фокус на них авто-обновление не блокирует.
    var EDITABLE_INPUT_TYPES = {
        text: true, password: true, email: true, number: true, search: true,
        tel: true, url: true, date: true, 'datetime-local': true, month: true,
        time: true, week: true, range: true, color: true, file: true
    };

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
     * isEditingIn — идёт ли ввод внутри pageEl прямо сейчас.
     * @param {Element|null} pageEl контейнер активной страницы (может быть null)
     * @param {Element|null} activeEl активный элемент (по умолчанию document.activeElement)
     */
    function isEditingIn(pageEl, activeEl) {
        if (!pageEl) { return false; }
        var el = activeEl;
        if (el === undefined) {
            el = (typeof document !== 'undefined') ? document.activeElement : null;
        }
        if (!isEditable(el)) { return false; }
        if (typeof pageEl.contains === 'function') { return pageEl.contains(el); }
        return false;
    }

    /**
     * editingInId — идёт ли ввод внутри элемента с указанным id (R513).
     *
     * Нужен там, где перерисовывается ОДНА панель, а не вся страница: панель
     * реестра горячих настроек, таблица лимитов, список моделей AutoTune.
     * Если элемента нет в DOM — false (нечего охранять).
     */
    function editingInId(id) {
        if (typeof document === 'undefined' || typeof document.getElementById !== 'function') {
            return false;
        }
        return isEditingIn(document.getElementById(id));
    }

    var api = { isEditable: isEditable, isEditingIn: isEditingIn, editingInId: editingInId };
    if (typeof window !== 'undefined') {
        window.RefreshGuard = api;
        // Для Node-тестов модуля (webui/tests): window в них — объект-заглушка.
    }
})();
