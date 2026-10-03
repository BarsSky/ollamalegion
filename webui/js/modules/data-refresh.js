/**
 * data-refresh.js — единая точка обновления данных WebUI (R-Image / R84, 2026-10-03).
 *
 * ЗАЧЕМ ЭТОТ МОДУЛЬ. До него кнопка «Обновить» жила в шапке приложения И в
 * шапках пяти страниц/табов — на «Image-моделях» оператор одновременно видел три
 * кнопки, а одна из них (#imgTestRefreshBtn) вообще не имела обработчика и не
 * делала ничего. Хуже того, разные страницы обновлялись по-разному: шапка тянула
 * /api/v1/cluster и перерисовывала страницу из локальной копии, а собственные
 * источники (список моделей image-бэкенда, метрики монитора, лог прокси, список
 * GGUF) оставались старыми — со стороны это выглядело как «кнопка не работает».
 *
 * ЧТО ДЕЛАЕТ:
 *   1. РЕЕСТР ПРОВАЙДЕРОВ: страница регистрирует свою функцию обновления
 *      (register(page, fn)), и одна кнопка в шапке дёргает ровно те источники,
 *      которые нужны активной странице.
 *   2. ИНДИКАТОР СВЕЖЕСТИ: «Обновлено 2 с назад · авто» рядом с кнопкой. Он честно
 *      отвечает на вопрос «актуально ли то, что я вижу» — включая ошибку
 *      («не удалось обновить: HTTP 502»), которая раньше исчезала молча.
 *   3. АВТО/ПАУЗА: переключатель рядом с индикатором; состояние переживает
 *      перезагрузку (localStorage). На скрытой вкладке опрос останавливается
 *      принудительно (см. app.js → visibilitychange), чтобы 24/7-дашборд не
 *      молотил API в фоне.
 *
 * ГРАНИЦЫ: модуль не знает, ЧТО именно тянет провайдер, и не рисует страницы.
 * Ему передают Promise-функцию, а он только вызывает её, запоминает время
 * успеха/ошибки и рисует индикатор. Поэтому он тестируется без DOM и без сети
 * (webui/js/modules/data-refresh.test.js).
 *
 * Экспорт: window.DataRefresh = { mount, register, registerGlobal, refresh,
 *   markFresh, markError, setAuto, isAuto, toggleAuto, onAutoChange, ageSeconds,
 *   formatAge, _state, _providers }.
 */
(function () {
    'use strict';

    var AUTO_KEY = 'ollamalegion_auto_refresh';
    var TICK_MS = 1000;

    var state = {
        // lastOkAt — время последнего УСПЕШНОГО обновления (мс).
        lastOkAt: 0,
        // lastError — текст последней ошибки обновления ('' = ошибок нет).
        lastError: '',
        // running — обновление идёт прямо сейчас (индикатор покажет спиннер).
        running: false,
        // auto — авто-обновление включено (переключатель в шапке).
        auto: true
    };

    // page → [fn, ...]; ключ '*' — провайдеры, нужные на любой странице.
    var providers = {};
    var autoListeners = [];
    var ticker = null;
    var mounted = false;

    function t(key, fallback, vars) {
        if (window.I18N && typeof window.I18N.t === 'function') {
            var v = vars ? window.I18N.t(key, vars) : window.I18N.t(key);
            if (v && v !== key) return v;
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

    // =====================================================================
    // Реестр провайдеров
    // =====================================================================

    /**
     * register — провайдер обновления для страницы.
     *
     * Провайдер обязан вернуть Promise (или значение — оно будет обёрнуто) и
     * обновить видимый DOM. Исключение провайдера НЕ ломает остальные: ошибка
     * попадает в индикатор свежести, а не в консоль.
     */
    function register(page, fn) {
        if (!page || typeof fn !== 'function') return false;
        if (!providers[page]) providers[page] = [];
        providers[page].push(fn);
        return true;
    }

    /** registerGlobal — провайдер для любой страницы (например, cluster state). */
    function registerGlobal(fn) {
        return register('*', fn);
    }

    function providersFor(page) {
        return (providers['*'] || []).concat(providers[page] || []);
    }

    // =====================================================================
    // Свежесть
    // =====================================================================

    function markFresh() {
        state.lastOkAt = Date.now();
        state.lastError = '';
        render();
    }

    function markError(err) {
        var msg = (err && err.message) ? err.message : String(err || '');
        state.lastError = msg || t('refresh.error_unknown', 'неизвестная ошибка');
        render();
    }

    /** ageSeconds — сколько секунд назад данные обновились (-1 = ни разу). */
    function ageSeconds(now) {
        if (!state.lastOkAt) return -1;
        var ms = (typeof now === 'number' ? now : Date.now()) - state.lastOkAt;
        return ms < 0 ? 0 : Math.floor(ms / 1000);
    }

    /** formatAge — «только что» / «12 с» / «3 мин» / «2 ч». */
    function formatAge(sec) {
        if (sec < 0) return t('refresh.never', 'нет данных');
        if (sec < 5) return t('refresh.just_now', 'только что');
        if (sec < 60) return t('refresh.sec_ago', '{n} с', { n: sec });
        if (sec < 3600) return t('refresh.min_ago', '{n} мин', { n: Math.floor(sec / 60) });
        return t('refresh.hour_ago', '{n} ч', { n: Math.floor(sec / 3600) });
    }

    /** freshnessLabel — полная подпись индикатора (используется и в тестах). */
    function freshnessLabel(sec, isAuto, err) {
        if (err) return t('refresh.error', 'не удалось обновить: {error}', { error: err });
        var mode = isAuto ? t('refresh.auto', 'авто') : t('refresh.paused', 'пауза');
        return t('refresh.updated_ago', 'Обновлено {age} назад', { age: formatAge(sec) }) + ' · ' + mode;
    }

    function render() {
        var node = byId('dataFreshness');
        if (!node) return;
        var text = byId('dataFreshnessText');
        var icon = byId('dataFreshnessIcon');
        var label = state.running
            ? t('refresh.running', 'обновляю…')
            : freshnessLabel(ageSeconds(), state.auto, state.lastError);
        if (text) text.textContent = label;
        else node.textContent = label;
        node.classList.toggle('is-error', !!state.lastError);
        node.classList.toggle('is-stale', !state.lastError && !state.auto);
        if (icon) {
            icon.className = state.running ? 'fas fa-circle-notch fa-spin'
                : (state.lastError ? 'fas fa-triangle-exclamation'
                    : (state.auto ? 'fas fa-circle-check' : 'fas fa-pause'));
        }
        var toggle = byId('dataAutoToggle');
        if (toggle) {
            toggle.setAttribute('title', state.auto
                ? t('refresh.toggle_auto_pause', 'Приостановить авто-обновление')
                : t('refresh.toggle_auto_resume', 'Возобновить авто-обновление'));
            var ti = byId('dataAutoToggleIcon');
            if (ti) ti.className = state.auto ? 'fas fa-pause' : 'fas fa-play';
            toggle.classList.toggle('is-paused', !state.auto);
        }
    }

    // =====================================================================
    // Авто-обновление
    // =====================================================================

    function setAuto(on, opts) {
        state.auto = !!on;
        try {
            window.localStorage.setItem(AUTO_KEY, state.auto ? '1' : '0');
        } catch (e) { /* приватный режим */ }
        render();
        if (!(opts && opts.silent)) {
            autoListeners.forEach(function (fn) {
                try { fn(state.auto); } catch (e) { /* подписчик не должен ломать UI */ }
            });
        }
        return state.auto;
    }

    function isAuto() { return state.auto; }

    /**
     * shouldPoll — можно ли сейчас опрашивать API (общее правило для ВСЕХ циклов).
     *
     * ЗАЧЕМ ОДНО ПРАВИЛО: у приложения несколько независимых поллеров (cluster/
     * очередь/сессии, метрики в iframe монитора, прогресс загрузок GGUF/HF,
     * статус операций над моделями). Если каждый решает сам, пауза
     * авто-обновления и скрытая вкладка перестают что-либо значить — проверено
     * живьём: при «паузе» в фоне уходило 40+ запросов за 7 секунд.
     */
    function shouldPoll() {
        if (!state.auto) return false;
        if (typeof document !== 'undefined' && document.visibilityState === 'hidden') return false;
        return true;
    }

    function toggleAuto() { return setAuto(!state.auto); }

    /** onAutoChange — подписка для владельца таймера (app.js → startPeriodicRefresh). */
    function onAutoChange(fn) {
        if (typeof fn === 'function') autoListeners.push(fn);
    }

    function loadAuto() {
        try {
            var saved = window.localStorage.getItem(AUTO_KEY);
            if (saved === '0') state.auto = false;
        } catch (e) { /* приватный режим: остаёмся на дефолте «авто» */ }
    }

    // =====================================================================
    // Обновление
    // =====================================================================

    /**
     * refresh — выполнить всех провайдеров страницы и обновить индикатор.
     * Возвращает true, если все провайдеры отработали без ошибок.
     */
    function refresh(page) {
        var fns = providersFor(page);
        if (!fns.length) {
            // Провайдеров нет — это не ошибка: страница живёт на общем опросе,
            // но индикатор всё равно должен показать, что мы «дёрнули» данные.
            render();
            return Promise.resolve(true);
        }
        state.running = true;
        render();
        var results = fns.map(function (fn) {
            return Promise.resolve().then(fn).then(function () { return true; }, function (e) { return e; });
        });
        return Promise.all(results).then(function (list) {
            var errs = list.filter(function (r) { return r !== true; });
            state.running = false;
            if (errs.length) {
                markError(errs[0]);
                return false;
            }
            markFresh();
            return true;
        });
    }

    // =====================================================================
    // Монтирование
    // =====================================================================

    function mount() {
        if (mounted) return true;
        mounted = true;
        loadAuto();

        var node = byId('dataFreshness');
        if (node && node.addEventListener) {
            // Клик по индикатору = «обновить сейчас»: одна очевидная точка
            // вместо россыпи кнопок в шапках карточек.
            node.addEventListener('click', function () {
                if (window.App && typeof window.App.refreshCurrentPage === 'function') {
                    window.App.refreshCurrentPage();
                }
            });
        }
        var toggle = byId('dataAutoToggle');
        if (toggle && toggle.addEventListener) {
            toggle.addEventListener('click', function (ev) {
                if (ev && ev.preventDefault) ev.preventDefault();
                toggleAuto();
            });
        }
        if (typeof setInterval === 'function' && !ticker) {
            // Раз в секунду перерисовываем ТОЛЬКО подпись: возраст данных
            // меняется сам, без сетевых запросов.
            ticker = setInterval(render, TICK_MS);
        }
        render();
        return true;
    }

    window.DataRefresh = {
        mount: mount,
        register: register,
        registerGlobal: registerGlobal,
        refresh: refresh,
        markFresh: markFresh,
        markError: markError,
        setAuto: setAuto,
        isAuto: isAuto,
        toggleAuto: toggleAuto,
        onAutoChange: onAutoChange,
        shouldPoll: shouldPoll,
        ageSeconds: ageSeconds,
        formatAge: formatAge,
        freshnessLabel: freshnessLabel,
        render: render,
        _state: state,
        _providers: providers
    };
})();
