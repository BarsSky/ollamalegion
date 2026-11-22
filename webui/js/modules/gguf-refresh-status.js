// js/modules/gguf-refresh-status.js — R689 (2026-11-23): видимая обратная связь
// для правой панели GGUF, когда авто-перерисовка отложена из-за ввода.
//
// ЖИВАЯ ЖАЛОБА (дословно): «Пока не вводится значение, интерфейс продолжает
// скакать при обновлении, но форму не закрывает. Когда же есть фокус на поле для
// ввода, то обновления скачками пропадают, и не понятно, обновляется ли статус».
//
// ЧТО ЗДЕСЬ ЛЕЧИТСЯ. R688 научил панель НЕ пересобираться, пока оператор правит
// форму (js/modules/gguf-detail-form-state.js). Правильно по значениям — и слепо
// для оператора: пропадали и скрывались лишние перерисовки, но вместе с ними и
// любой признак того, что данные вообще идут. Оставался один индикатор в шапке,
// а он на странице GGUF показывал рост «Обновлено 12 с назад · авто», потому что
// тик авто-обновления целиком пропускался, пока фокус в поле (app-data.js).
//
// ЧТО ДЕЛАЕТ МОДУЛЬ:
//   1. СЛУШАЕТ два события страницы и держит по ним состояние:
//      * noteSuccess()  — проход авто-обновления УСПЕШНО обновил данные
//        (его зовёт провайдер страницы, gguf-page-provider.js);
//      * noteDeferred() — авто-пересборка панели ОТЛОЖЕНА, потому что оператор
//        правит форму (его зовёт gguf-detail-form-state.js → rebuild);
//      * noteResumed()  — защита больше не нужна (значения сохранены/поле
//        отпущено), строка скрывается.
//   2. РИСУЕТ компактную строку #ggufAutoStatus над правой панелью:
//      «данные обновлены 3 с назад · перерисовка приостановлена, пока вы вводите ·
//      обновлений: 7». Возраст обновляется раз в секунду — поэтому видно, что
//      данные продолжают идти, а не «замерли».
//   3. СЧИТАЕТ успешные обновления, пока панель не перерисовывается: счётчик
//      сбрасывается в момент начала паузы и растёт на каждом успешном проходе.
//      Это ответ на «не понятно, обновляется ли статус» — число, а не слово.
//
// ГРАНИЦЫ: модуль ничего не запрашивает и не решает, когда обновлять. Он только
// отображает состояние, которое сообщают провайдер и защита формы. Строка живёт
// ВНЕ #ggufDetailPanel: панель пересобирается, строка — нет (иначе она мигала бы
// вместе с ней и снова превращалась в «скачки»).
//
// Тесты: webui/tests/gguf-auto-refresh-stability-r689.test.js (RED до R689 —
// модуля и строки не было вовсе, оператор не видел ни свежести, ни причины).
(function () {
    'use strict';

    // id в разметке страницы (webui/js/modules/gguf-renderer.js → buildPageHtml).
    var HOST_ID = 'ggufAutoStatus';
    var TEXT_ID = 'ggufAutoStatusText';
    var TICK_MS = 1000;

    var state = {
        // lastOkAt — время последнего успешного обновления данных (мс).
        lastOkAt: 0,
        // okCount — сколько успешных обновлений прошло с начала паузы перерисовки.
        okCount: 0,
        // deferred — авто-перерисовка сейчас отложена (оператор правит форму).
        deferred: false,
        // deferredAt — когда пауза началась (диагностика/тесты).
        deferredAt: 0
    };

    var timer = null;

    function t(key, fallback, vars) {
        if (typeof window !== 'undefined' && window.I18N && typeof window.I18N.t === 'function') {
            var v = vars ? window.I18N.t(key, vars) : window.I18N.t(key);
            if (v && v !== key) { return v; }
        }
        if (fallback && vars) {
            return String(fallback).replace(/\{(\w+)\}/g, function (m, k) {
                return Object.prototype.hasOwnProperty.call(vars, k) ? String(vars[k]) : m;
            });
        }
        return fallback || key;
    }

    function byId(id) {
        return (typeof document !== 'undefined' && document.getElementById) ? document.getElementById(id) : null;
    }

    /** ageSeconds — сколько секунд назад данные обновились (-1 = ещё ни разу). */
    function ageSeconds(now) {
        if (!state.lastOkAt) { return -1; }
        var ms = (typeof now === 'number' ? now : Date.now()) - state.lastOkAt;
        return ms < 0 ? 0 : Math.floor(ms / 1000);
    }

    /** formatAge — «только что» / «12 с» / «3 мин» (те же правила, что в шапке). */
    function formatAge(sec) {
        if (sec < 0) { return t('refresh.never', 'нет данных'); }
        if (sec < 5) { return t('refresh.just_now', 'только что'); }
        if (sec < 60) { return t('refresh.sec_ago', '{n} с', { n: sec }); }
        if (sec < 3600) { return t('refresh.min_ago', '{n} мин', { n: Math.floor(sec / 60) }); }
        return t('refresh.hour_ago', '{n} ч', { n: Math.floor(sec / 3600) });
    }

    /**
     * label — текст строки (чистая функция: её проверяет тест без DOM).
     *
     * Собирается из трёх честных фактов: когда данные обновились в последний раз,
     * почему панель не перерисована, сколько обновлений прошло за паузу.
     */
    function label(now) {
        var age = ageSeconds(now);
        var parts = [];
        if (age < 0) {
            parts.push(t('gguf.auto.data_waiting', 'данные ещё не обновлялись'));
        } else if (age < 5) {
            parts.push(t('gguf.auto.data_just_now', 'данные обновлены только что'));
        } else {
            parts.push(t('gguf.auto.data_ago', 'данные обновлены {age} назад', { age: formatAge(age) }));
        }
        parts.push(t('gguf.auto.redraw_paused', 'перерисовка приостановлена, пока вы вводите'));
        parts.push(t('gguf.auto.updates_count', 'обновлений: {n}', { n: state.okCount }));
        return parts.join(' · ');
    }

    function startTimer() {
        if (timer || typeof setInterval !== 'function') { return; }
        // Раз в секунду обновляем ТОЛЬКО подпись возраста: сети это не касается.
        timer = setInterval(render, TICK_MS);
    }

    function stopTimer() {
        if (!timer) { return; }
        if (typeof clearInterval === 'function') { clearInterval(timer); }
        timer = null;
    }

    /**
     * render — показать/скрыть строку по текущему состоянию.
     *
     * Строка нужна только в паузе: когда перерисовка не отложена, свежесть уже
     * показывает индикатор в шапке (#dataFreshness), дублировать его незачем.
     */
    function render() {
        var node = byId(HOST_ID);
        if (!node) { stopTimer(); return false; }
        var text = byId(TEXT_ID) || node;
        if (!state.deferred) {
            if (text.textContent) { text.textContent = ''; }
            if (node.hidden !== true) { node.hidden = true; }
            if (node.classList && node.classList.remove) { node.classList.remove('is-paused'); }
            stopTimer();
            return true;
        }
        text.textContent = label();
        if (node.hidden !== false) { node.hidden = false; }
        if (node.classList && node.classList.add) { node.classList.add('is-paused'); }
        startTimer();
        return true;
    }

    /** noteSuccess — проход авто-обновления успешно обновил данные. */
    function noteSuccess(at) {
        state.lastOkAt = (typeof at === 'number' && at > 0) ? at : Date.now();
        if (state.deferred) { state.okCount += 1; }
        render();
        return state.lastOkAt;
    }

    /**
     * noteDeferred — авто-перерисовка отложена защитой формы.
     *
     * Начало паузы обнуляет счётчик: оператору важно видеть, сколько обновлений
     * прошло ИМЕННО пока панель не перерисовывалась, а не «сколько за сутки».
     */
    function noteDeferred() {
        if (!state.deferred) {
            state.deferred = true;
            state.deferredAt = Date.now();
            state.okCount = 0;
        }
        render();
        return true;
    }

    /** noteResumed — защита снята: перерисовка снова разрешена, строка скрывается. */
    function noteResumed() {
        if (!state.deferred) { return false; }
        state.deferred = false;
        state.deferredAt = 0;
        render();
        return true;
    }

    var api = {
        HOST_ID: HOST_ID,
        TEXT_ID: TEXT_ID,
        noteSuccess: noteSuccess,
        noteDeferred: noteDeferred,
        noteResumed: noteResumed,
        render: render,
        label: label,
        ageSeconds: ageSeconds,
        isDeferred: function () { return state.deferred === true; },
        _state: state
    };
    if (typeof window !== 'undefined') {
        window.GgufRefreshStatus = api;
    }
    if (typeof module !== 'undefined' && module.exports) {
        module.exports = api; // node-тесты без DOM
    }
})();
