/**
 * GgufLoadProgress — модуль поллинга состояния загрузки моделей
 *
 * Проблема: модель llama.cpp грузится в blocking CGo-вызове 5-30+ секунд.
 * Во время этой загрузки:
 *   - бэкенд отдаёт 503 + {loading:true, elapsedMs, retryAfterMs}
 *   - WebUI должен показывать «Загружается model-name 25s» в sidebar/detail
 *   - polling /api/models/load/progress раз в 1.5 сек показывает live-прогресс
 *
 * API:
 *   GgufLoadProgress.startPolling(backend, isActiveCallback, [opts])
 *     — запускает polling для конкретного бэкенда.
 *     — isActiveCallback() — функция, возвращающая true, пока polling должен
 *       продолжаться (например, return currentBackend() === backend).
 *     — opts: { intervalMs?: number, onUpdate?: (models) => void, onLoaded?: (name)=>void, onError?: (name,err)=>void }
 *
 *   GgufLoadProgress.stopPolling(backendId)
 *     — останавливает polling для бэкенда (если был запущен).
 *
 *   GgufLoadProgress.stopAll()
 *     — останавливает все активные поллеры.
 *
 *   GgufLoadProgress.isActive(backendId)
 *     — проверяет, активен ли polling для бэкенда.
 *
 *   GgufLoadProgress.getState(backendId)
 *     — возвращает последнее известное состояние моделей: [{name,path,state,elapsedMs,loadingStartedAt,loadingSizeBytes,error}]
 */
(function () {
    'use strict';

    // Map backendId -> { timer, state, backend, isActive, opts, lastSeen, lastActivity }
    const _pollers = Object.create(null);

    /**
     * Создаёт URL для /api/models/load/progress конкретного бэкенда через прокси балансировщика.
     */
    function _buildProgressUrl(backendId) {
        if (window.GgufApi && typeof window.GgufApi.buildBackendProxyUrl === 'function') {
            return window.GgufApi.buildBackendProxyUrl(backendId, '/api/models/load/progress');
        }
        // Fallback: balancer API base
        const balancerUrl = (window.WEBUI_CONFIG && window.WEBUI_CONFIG.API_BASE_URL) ||
            (typeof window !== 'undefined' && window.location && window.location.origin) ||
            '/';
        return balancerUrl.replace(/\/+$/, '') + '/api/v1/gguf/backends/' + encodeURIComponent(backendId) + '/proxy/api/models/load/progress';
    }

    function _now() {
        return new Date();
    }

    function _formatElapsed(ms) {
        if (!ms || ms < 0) return '0s';
        const sec = Math.floor(ms / 1000);
        if (sec < 60) return sec + 's';
        const min = Math.floor(sec / 60);
        const s = sec % 60;
        return min + 'm ' + s + 's';
    }

    /**
     * Парсит ответ /api/models/load/progress (массив моделей с состоянием loading/error/loaded).
     * Формат cppworker'а: { models: [{name, path, state, loadingStartedAt, loadingSizeBytes, elapsedMs, error}], backend: "..." }
     */
    function _parseProgressResponse(data) {
        let arr = [];
        if (Array.isArray(data)) arr = data;
        else if (data && Array.isArray(data.models)) arr = data.models;
        else if (data && data.model) arr = [data.model];  // single-model запрос
        else if (data && data.name) arr = [data];         // альтернативный формат
        return arr.map(function (m) {
            return {
                name: m.name || m.model || '',
                path: m.path || '',
                state: m.state || (m.error ? 'error' : 'loading'),
                loadingStartedAt: m.loadingStartedAt || m.startedAt || null,
                loadingSizeBytes: m.loadingSizeBytes || m.sizeBytes || 0,
                elapsedMs: m.elapsedMs || 0,
                error: m.error || ''
            };
        });
    }

    /**
     * Нормализует state.loadingModels[backendId] под актуальный ответ сервера.
     * Сохраняет loadingStartedAt, если на сервере оно отсутствует (например, в cached-ответе).
     */
    function _applyToState(backendId, models) {
        const renderer = window.GgufRenderer;
        if (!renderer || typeof renderer.getState !== 'function') return;
        const s = renderer.getState();
        if (!s.loadingModels) s.loadingModels = {};
        const old = s.loadingModels[backendId] || [];
        const oldByName = Object.create(null);
        for (let i = 0; i < old.length; i++) {
            const o = old[i];
            if (o && o.name) oldByName[o.name] = o;
        }
        const next = models.map(function (m) {
            const o = oldByName[m.name] || {};
            return {
                name: m.name,
                path: m.path || o.path || '',
                state: m.state,
                loadingStartedAt: m.loadingStartedAt || o.loadingStartedAt || null,
                loadingSizeBytes: m.loadingSizeBytes || o.loadingSizeBytes || 0,
                elapsedMs: m.elapsedMs || 0,
                error: m.error || ''
            };
        });
        s.loadingModels[backendId] = next;

        // Дёрнем сайдбар renderBackendsList через глобальный updateBackendsList
        if (typeof s._backendsRefreshFn === 'function') {
            try { s._backendsRefreshFn(); } catch (e) { /* noop */ }
        } else if (renderer && typeof renderer.refreshBackends === 'function' && renderer.getState()._lastPolledBackendsRefreshAt) {
            // Лёгкий фоллбек: если есть renderer, можно вызвать refreshBackends (но он может рекурсивно
            // перезагрузить данные; поэтому предпочитаем _backendsRefreshFn, если он зарегистрирован).
        }
    }

    /**
     * Запускает polling для бэкенда.
     * @param {object} backend — backend объект ({id, url, ...})
     * @param {function} isActiveCallback — () => bool
     * @param {object} [opts]
     * @param {number} [opts.intervalMs=1500] — интервал поллинга
     * @param {function} [opts.onUpdate] — (models) => void
     * @param {function} [opts.onLoaded] — (name) => void
     * @param {function} [opts.onError] — (name, errMsg) => void
     * @param {function} [opts.onAllLoaded] — () => void  // вызывается, когда loadingModels[] пустеет
     */
    function startPolling(backend, isActiveCallback, opts) {
        if (!backend || !backend.id) {
            console.warn('[gguf-load-progress] startPolling: backend.id missing');
            return;
        }
        const backendId = backend.id;
        opts = opts || {};
        const intervalMs = opts.intervalMs || 1500;
        const isActive = (typeof isActiveCallback === 'function')
            ? isActiveCallback
            : function () { return true; };

        // Если уже запущен — не дублируем
        if (_pollers[backendId]) {
            // Обновим callback активности (например, при selectBackend сменился)
            _pollers[backendId].isActive = isActive;
            _pollers[backendId].opts = opts;
            return;
        }

        const state = {
            timer: null,
            inFlight: false,
            lastError: null,
            lastActivity: _now(),
            models: [],
            backend: backend,
            isActive: isActive,
            opts: opts
        };
        _pollers[backendId] = state;

        const tick = function () {
            if (!_pollers[backendId]) return;          // был stopPolling между тиками
            if (state.inFlight) return;                // запрос ещё в полёте
            if (typeof state.isActive === 'function' && !state.isActive()) {
                // Пользователь ушёл с бэкенда — стоп.
                stopPolling(backendId);
                return;
            }
            state.inFlight = true;
            const url = _buildProgressUrl(backendId);
            const controller = (typeof AbortController === 'function') ? new AbortController() : null;
            const timer = controller ? setTimeout(function () { try { controller.abort(); } catch (e) {} }, 5000) : null;
            fetch(url, {
                method: 'GET',
                headers: { 'Accept': 'application/json' },
                cache: 'no-store',
                ...(controller ? { signal: controller.signal } : {})
            }).then(function (resp) {
                if (timer) clearTimeout(timer);
                if (!resp.ok) {
                    throw new Error('HTTP ' + resp.status);
                }
                return resp.json();
            }).then(function (data) {
                state.inFlight = false;
                state.lastError = null;
                state.lastActivity = _now();
                const models = _parseProgressResponse(data);
                state.models = models;
                _applyToState(backendId, models);
                if (typeof state.opts.onUpdate === 'function') {
                    try { state.opts.onUpdate(models); } catch (e) { console.warn('[gguf-load-progress] onUpdate error', e); }
                }
                // Детектим переход loading → loaded / loading → error
                const prev = state._prevModels || [];
                const prevByName = Object.create(null);
                for (let i = 0; i < prev.length; i++) {
                    if (prev[i] && prev[i].name) prevByName[prev[i].name] = prev[i];
                }
                for (let i = 0; i < models.length; i++) {
                    const m = models[i];
                    const p = prevByName[m.name];
                    if (p && p.state === 'loading' && m.state === 'loaded') {
                        if (typeof state.opts.onLoaded === 'function') {
                            try { state.opts.onLoaded(m.name); } catch (e) { /* noop */ }
                        }
                    } else if (p && p.state === 'loading' && m.state === 'error') {
                        if (typeof state.opts.onError === 'function') {
                            try { state.opts.onError(m.name, m.error || 'unknown'); } catch (e) { /* noop */ }
                        }
                    }
                }
                state._prevModels = models;
                // Если loadingModels[] пуст и раньше были — все загрузились
                if (models.length === 0 && prev.length > 0) {
                    if (typeof state.opts.onAllLoaded === 'function') {
                        try { state.opts.onAllLoaded(); } catch (e) { /* noop */ }
                    }
                    // Polling можно остановить, если нет isActive-callback требования
                    if (typeof state.isActive !== 'function') {
                        stopPolling(backendId);
                    }
                }
            }).catch(function (err) {
                if (timer) clearTimeout(timer);
                state.inFlight = false;
                state.lastError = err && err.message || String(err);
                // На 5xx/network error — не паникуем, polling продолжится на следующем тике.
                if (window.console && console.debug) {
                    console.debug('[gguf-load-progress] poll error for', backendId, state.lastError);
                }
            });
        };

        state.timer = setInterval(tick, intervalMs);
        // Первый тик сразу (не ждём intervalMs)
        setTimeout(tick, 50);
    }

    /**
     * Останавливает polling для бэкенда.
     */
    function stopPolling(backendId) {
        const p = _pollers[backendId];
        if (!p) return;
        if (p.timer) clearInterval(p.timer);
        delete _pollers[backendId];
    }

    function stopAll() {
        Object.keys(_pollers).forEach(function (id) { stopPolling(id); });
    }

    function isActive(backendId) {
        return !!_pollers[backendId];
    }

    function getState(backendId) {
        const p = _pollers[backendId];
        return p ? p.models.slice() : [];
    }

    function getLastError(backendId) {
        const p = _pollers[backendId];
        return p ? p.lastError : null;
    }

    /**
     * Удобный хелпер: зарегистрировать callback, который renderBackendsList вызывает
     * для триггера ререндера sidebar (используется из gguf-renderer.js).
     */
    function registerBackendsRefreshFn(fn) {
        const renderer = window.GgufRenderer;
        if (!renderer || typeof renderer.getState !== 'function') return;
        renderer.getState()._backendsRefreshFn = fn;
    }

    window.GgufLoadProgress = {
        startPolling: startPolling,
        stopPolling: stopPolling,
        stopAll: stopAll,
        isActive: isActive,
        getState: getState,
        getLastError: getLastError,
        registerBackendsRefreshFn: registerBackendsRefreshFn,
        // Экспортируем для тестов
        _formatElapsed: _formatElapsed,
        _buildProgressUrl: _buildProgressUrl
    };
})();