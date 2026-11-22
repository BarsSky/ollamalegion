// webui/js/modules/session-expiry.js — R679 (2026-11-21): ИСТЁКШАЯ СЕССИЯ —
// ЭТО ГОСТЕВОЙ РЕЖИМ, А НЕ ПОТОК ОШИБОК.
//
// ЖИВАЯ ЖАЛОБА (дословно): «Сейчас сыпятся ошибки что пользователь неавторизован,
// и только перезагрузка открывает меню входа, но при этом режим просмотра не
// активен — то есть не переходит на пользователя гостя, что может только смотреть
// некоторую информацию».
//
// ЧТО ПРОИСХОДИЛО. Сессии WebUI живут В ПАМЯТИ балансера (R201): перезапустили
// балансер (или сессия протухла) — cookie в браузере осталась, а на сервере её
// больше нет. Дальше:
//   * каждый запрос отвечает 401, каждый модуль ловит ошибку и показывает свой
//     toast → десятки одинаковых «HTTP 401: Unauthorized» в правом нижнем углу;
//   * `Auth.state()` продолжает говорить `authenticated: true`, поэтому
//     `[data-admin-only]` кнопки видимы и активны, а действия падают на 401/403;
//   * экран входа не открывается (он показывается только на загрузке страницы),
//     поэтому помочь может только F5 — но и после F5 гость видел перекрывающий
//     слой без объяснения (это лечил R665/R667).
//
// ЧТО ДЕЛАЕТ ЭТОТ МОДУЛЬ. Держит ОДНО правило на все источники 401 — HTTP и
// WebSocket: первый же 401 ПЕРЕВОДИТ панель в гостя НА МЕСТЕ (без перезагрузки),
// показывает РОВНО ОДНО объяснение и ГАСИТ ШУМ от повторных ошибок.
//
// ПОЧЕМУ ОТДЕЛЬНЫЙ МОДУЛЬ, А НЕ СТРОЧКА В auth.js/api.js. Сигнал 401 приходит из
// трёх разных мест (wrapper fetch в auth.js, request() в api.js, рукопожатие
// WebSocket в websocket.js), а «переключить панель в гостя» обязан уметь ровно
// один код. Без единой точки каждый источник начинает «немного чинить» сам:
// именно так появились три разных описания «пользователь неавторизован» в UI.
//
// КОНТРАКТ (его читают api.js, websocket.js, notifications.js, logs-stream.js,
// app-core.js, app-listeners.js, data-refresh.js и тест
// webui/tests/session-expiry-r679.test.js):
//   SessionExpiry.note(source, detail) → true, если ЭТОТ вызов перевёл панель в гостя
//   SessionExpiry.probe(reason)        → проверка «а сессия ещё жива?» через /auth/me
//   SessionExpiry.shouldSilence(msg)   → true = сообщение является шумом, не показывать
//   SessionExpiry.isExpired()          → сейчас гость «из-за истёкшей сессии»
//   SessionExpiry.reset()              → сессия восстановлена (явный вход)
//   SessionExpiry.stats()              → счётчики для диагностики и теста
//
// ГРАНИЦЫ (осознанные):
//   * в демо-режиме модуль молчит: там fetch перехвачен demo-mode.js и 401 не
//     бывает, а «гостевой режим» уже включён самим демо;
//   * 403 — НЕ истечение сессии (это роль или CSRF), панель в гостя не переводит;
//     он только глушится как шум, пока сессия уже признана истёкшей;
//   * 401 самого /api/v1/auth/login — это неверный пароль, а не истёкшая сессия
//     (иначе опечатка в пароле разлогинивала бы и прятала панель).
(function () {
    'use strict';

    var CFG = window.WEBUI_CONFIG || {};

    // Окно «тишины»: сколько живёт один всплеск повторов. За это окно подавленные
    // сообщения собираются в ОДНУ строку консоли (см. flushSuppressed), а не в
    // сотню тостов.
    var QUIET_MS = (Number(CFG.SESSION_EXPIRY_QUIET_MS) > 0)
        ? Number(CFG.SESSION_EXPIRY_QUIET_MS)
        : 30000;

    // Как часто можно проверять сессию через /auth/me. Проверка нужна для
    // WebSocket: браузерный API рукопожатия НЕ отдаёт HTTP-статус (onerror/onclose
    // code=1006), поэтому «401 на рукопожатии» распознаётся повторным запросом к
    // публичному /auth/me: сервер отвечает 200 и честно говорит authenticated:false.
    var PROBE_MIN_MS = 3000;

    var AUTH_NOISE = /(^|\D)(401|403)(\D|$)|unauthori[sz]ed|not\s+authori[sz]ed|требуется\s+вход|не\s*авториз|login\s+required|sign[-\s]?in\s+required|session\s+(has\s+)?(expired|ended)|сесси[яи]\s+(заверш|истек)/i;

    var state = {
        expired: false,
        at: 0,             // когда признали сессию истёкшей (мс)
        source: '',        // 'http' | 'ws' | 'ws-close' | ...
        suppressed: 0,     // подавлено сообщений в ТЕКУЩЕМ окне тишины
        totalSuppressed: 0,// подавлено за всё время (для отчёта/диагностики)
        reports: 0,        // сколько раз всего пришёл сигнал 401
        transitions: 0,    // сколько раз реально переключались в гостя
        probes: 0,
        lastProbeAt: 0
    };
    var flushTimer = null;

    function nowMs() {
        return Date.now();
    }

    function demoMode() {
        return !!(CFG.DEMO_MODE || (window.WEBUI_CONFIG && window.WEBUI_CONFIG.DEMO_MODE));
    }

    function authState() {
        return (window.Auth && typeof window.Auth.state === 'function') ? window.Auth.state() : null;
    }

    /**
     * wasInSession — были ли мы «внутри» до сигнала 401.
     *
     * Это ключевое отличие «сессия истекла» от «страница открыта гостем». Если
     * доступа не было и раньше, 401 — ожидаемое состояние гостя: панель уже
     * показывает полосу/слой входа (R665/R667), и повторно «объяснять» нечего.
     */
    function wasInSession() {
        var st = authState();
        return !!(st && (st.authenticated === true || st.tokenAuth === true));
    }

    /** isExpired — сейчас гость именно из-за истёкшей сессии (а не с загрузки). */
    function isExpired() {
        return state.expired === true;
    }

    function stats() {
        return {
            expired: state.expired,
            at: state.at,
            source: state.source,
            suppressed: state.suppressed,
            totalSuppressed: state.totalSuppressed,
            reports: state.reports,
            transitions: state.transitions,
            probes: state.probes,
            quietMs: QUIET_MS
        };
    }

    /**
     * flushSuppressed — один отчёт в консоль на окно тишины.
     *
     * ЗАЧЕМ СЧЁТЧИК В КОНСОЛИ, А НЕ В UI. Оператору нужен один понятный текст
     * («сессия завершилась, вы в просмотре»), а не лента. При этом «сколько
     * именно ошибок прилетело» полезно при разборе — поэтому одна строка
     * console.info за окно, а не тост на каждую.
     */
    function flushSuppressed() {
        flushTimer = null;
        if (state.suppressed > 0 && typeof console !== 'undefined' && console.info) {
            console.info('[session] подавлено повторных сообщений об авторизации: ' +
                state.suppressed + ' за ' + Math.round(QUIET_MS / 1000) + ' с (сессия завершилась, панель в режиме просмотра)');
        }
        state.suppressed = 0;
        scheduleFlush();
    }

    function scheduleFlush() {
        if (flushTimer || typeof setTimeout !== 'function') { return; }
        flushTimer = setTimeout(flushSuppressed, QUIET_MS);
        // В Node (тесты) таймер не должен держать процесс живым.
        if (flushTimer && typeof flushTimer.unref === 'function') { flushTimer.unref(); }
    }

    /**
     * quietNoisyChannels — остановить то, что само по себе генерирует шум.
     *
     * Сессия истекла ⇒ ЛЮБОЕ переподключение будет снова получать 401. Раньше
     * WS долбился 10 раз (и писал по строке в «Системные логи» на каждую
     * попытку), SSE-уведомления — бесконечно, с console.warn. Это и был «поток
     * ошибок». После явного входа страница перезагружается (R202-контракт), так
     * что возобновлять эти каналы на месте не нужно.
     */
    function quietNoisyChannels() {
        // R679: ВСЕ самостоятельные циклы останавливаются одним вызовом app.js.
        // Иначе авто-обновление (5 с), статус операций и монитор в iframe (2 с)
        // продолжают ходить в API и получать 401 — оператор видит это как «поток
        // ошибок». Монитор — отдельный документ, поэтому app.js гасит его
        // postMessage-ом, а не локальным clearInterval.
        try {
            if (window.App && typeof window.App.pauseForSessionExpiry === 'function') {
                window.App.pauseForSessionExpiry();
            }
        } catch (e) { /* приложение не инициализировано — нечего останавливать */ }
        try {
            if (window.WebSocketManager && typeof window.WebSocketManager.disconnect === 'function') {
                window.WebSocketManager.disconnect();
            }
        } catch (e) { /* канал не поднят — нечего останавливать */ }
        try {
            if (window.notifications && typeof window.notifications.stop === 'function') {
                window.notifications.stop();
            }
        } catch (e) { /* нет SSE — не критично */ }
        try {
            if (window.logsStream && typeof window.logsStream.stop === 'function') {
                window.logsStream.stop();
            }
        } catch (e) { /* нет live-tail — не критично */ }
        // R679: индикатор свежести переходит в спокойное «требуется вход» вместо
        // красной «ошибки» на каждом тике (data-refresh.js).
        try {
            if (window.DataRefresh && typeof window.DataRefresh.setAuthExpired === 'function') {
                window.DataRefresh.setAuthExpired(true);
            }
        } catch (e) { /* индикатора нет — не критично */ }
    }

    /**
     * note — ЕДИНСТВЕННАЯ точка реакции на 401.
     *
     * @param {string} source — откуда пришёл сигнал ('http', 'ws', 'ws-close', …)
     * @param {Object} [detail] — {status, url} для логов/диагностики
     * @returns {boolean} true, если именно этот вызов перевёл панель в гостя
     */
    function note(source, detail) {
        state.reports++;
        if (demoMode()) { return false; }

        if (state.expired) {
            // Уже гость из-за истёкшей сессии: новые сообщения не показываем,
            // только считаем (одна строка в консоль за окно — см. flushSuppressed).
            state.suppressed++;
            state.totalSuppressed++;
            scheduleFlush();
            return false;
        }
        if (!wasInSession()) {
            // Гость с самого начала (R665/R667 уже объяснили состояние): 401 —
            // ожидаемый ответ сервера, переключать нечего, но и тост не нужен.
            state.suppressed++;
            state.totalSuppressed++;
            scheduleFlush();
            return false;
        }

        state.expired = true;
        state.at = nowMs();
        state.source = String(source || 'http');
        state.suppressed = 0;
        state.transitions++;

        // 1) Состояние и UI — одной операцией (auth.js знает про роли и чип).
        try {
            if (window.Auth && typeof window.Auth.expireSession === 'function') {
                window.Auth.expireSession({ source: state.source, status: detail && detail.status });
            }
        } catch (e) {
            if (typeof console !== 'undefined' && console.warn) {
                console.warn('[session] не удалось перевести панель в гостя:', e && e.message);
            }
        }

        // 2) Всё, что само будет стучаться и шуметь, — остановить.
        quietNoisyChannels();

        // 3) Одно сообщение в консоль (оператору — полоса/слой из auth.js).
        if (typeof console !== 'undefined' && console.info) {
            console.info('[session] сессия завершилась (' + state.source + '): панель переведена в режим просмотра, ' +
                'повторные сообщения об авторизации подавляются ' + Math.round(QUIET_MS / 1000) + ' с');
        }
        scheduleFlush();
        return true;
    }

    /**
     * probe — проверить, жива ли сессия, запросом к /auth/me.
     *
     * Нужен WebSocket: браузер не отдаёт статус рукопожатия. /auth/me доступен
     * без сессии и отвечает 200 даже гостю — по его телу и решаем.
     */
    function probe(reason) {
        if (demoMode() || state.expired) { return Promise.resolve(false); }
        if (!wasInSession()) { return Promise.resolve(false); }
        var t = nowMs();
        if (t - state.lastProbeAt < PROBE_MIN_MS) { return Promise.resolve(false); }
        state.lastProbeAt = t;
        state.probes++;

        var doFetch = (typeof window.fetch === 'function') ? window.fetch.bind(window) : null;
        if (!doFetch) { return Promise.resolve(false); }

        return doFetch('/api/v1/auth/me', {
            method: 'GET',
            credentials: 'same-origin',
            headers: { 'Accept': 'application/json' }
        }).then(function (res) {
            if (!res || typeof res.json !== 'function') { return null; }
            return res.json();
        }).then(function (body) {
            if (!body || typeof body !== 'object') { return false; }
            // Сессия жива (или оператор работает по break-glass токену) — молчим.
            if (body.authenticated === true || body.tokenAuth === true) { return false; }
            // Вход на сервере не настроен — это не «истёкшая сессия».
            if (body.usersConfigured !== true) { return false; }
            note(String(reason || 'probe'), { status: 401 });
            return true;
        }).catch(function () {
            // Балансер недоступен/сеть упала: это НЕ истёкшая сессия, а обрыв.
            return false;
        });
    }

    /**
     * shouldSilence — гасить ли это сообщение как шум.
     *
     * Гасим ТОЛЬКО пока панель уже в гостя из-за истёкшей сессии и ТОЛЬКО
     * авторизационные сообщения (401/403/«требуется вход»/…). Прочие ошибки
     * (диск, таймаут, 500) остаются видимыми: «шум» не должен прятать реальные
     * проблемы.
     */
    function shouldSilence(message) {
        if (demoMode() || !state.expired) { return false; }
        var text = String(message == null ? '' : message);
        if (text && !AUTH_NOISE.test(text)) { return false; }
        state.suppressed++;
        state.totalSuppressed++;
        scheduleFlush();
        return true;
    }

    /** reset — сессия восстановлена (явный вход): снять «гостя» и счётчики. */
    function reset() {
        state.expired = false;
        state.at = 0;
        state.source = '';
        state.suppressed = 0;
        if (flushTimer) {
            clearTimeout(flushTimer);
            flushTimer = null;
        }
        return stats();
    }

    window.SessionExpiry = {
        note: note,
        probe: probe,
        shouldSilence: shouldSilence,
        isExpired: isExpired,
        reset: reset,
        stats: stats,
        quietMs: QUIET_MS
    };
})();
