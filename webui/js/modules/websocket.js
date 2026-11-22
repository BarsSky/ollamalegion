/**
 * WebSocket manager with exponential backoff reconnection and heartbeat
 */
const WebSocketManager = (function () {
    const CFG = window.WEBUI_CONFIG || {};

    let ws = null;
    let reconnectTimer = null;
    let heartbeatTimer = null;
    let reconnectAttempts = 0;
    let connected = false;
    // R679: открылось ли ТЕКУЩЕЕ соединение хотя бы раз. Отличие важно: закрытие
    // уже работавшего канала — это обрыв/рестарт балансера, а закрытие без
    // открытия — отказ рукопожатия, в том числе 401 от AuthorizeWS.
    let openedThisAttempt = false;

    const MAX_RECONNECT_ATTEMPTS = CFG.MAX_RECONNECT_ATTEMPTS || 10;
    const BASE_INTERVAL = CFG.RECONNECT_INTERVAL_BASE || 3000;
    const HEARTBEAT_INTERVAL = 15000; // send ping every 15s

    // Calculate exponential backoff delay, capped at 30s
    function reconnectDelay() {
        const delay = Math.min(BASE_INTERVAL * Math.pow(1.5, reconnectAttempts), 30000);
        return delay;
    }

    // Dispatch connection status to UI
    function setStatus(isConnected) {
        connected = isConnected;
        window.dispatchEvent(new CustomEvent('ws-status', { detail: { connected } }));
    }

    /**
     * sessionExpired — R679: сессия уже признана истёкшей?
     *
     * Нужен здесь потому, что переподключение к /ws/metrics после истечения
     * сессии гарантированно снова получит 401: сервер проверяет доступ ДО
     * Upgrade (AuthorizeWS). Раньше это давало 10 попыток и 10 строк «Ошибка
     * WebSocket → Переподключение…» в панели.
     */
    function sessionExpired() {
        return !!(typeof window !== 'undefined' && window.SessionExpiry &&
            typeof window.SessionExpiry.isExpired === 'function' && window.SessionExpiry.isExpired());
    }

    /**
     * noteHandshakeFailure — R679: рукопожатие не удалось, проверить сессию.
     *
     * ПОЧЕМУ ПРОВЕРКА ОТДЕЛЬНЫМ ЗАПРОСОМ. Браузерный WebSocket API НЕ отдаёт
     * HTTP-статус отказа рукопожатия: onerror получает Event без полей, onclose —
     * code 1006. То есть «401 от AuthorizeWS» снаружи неотличим от обрыва сети.
     * Проверяем публичный /api/v1/auth/me: если сервер отвечает 200 и
     * authenticated:false при настроенных пользователях — сессия действительно
     * истекла, и панель надо перевести в гостя ТЕМ ЖЕ путём, что и HTTP-401.
     * Если балансер недоступен (сеть), probe молчит: это не истёкшая сессия.
     */
    function noteHandshakeFailure(reason) {
        try {
            if (typeof window !== 'undefined' && window.SessionExpiry &&
                typeof window.SessionExpiry.probe === 'function') {
                window.SessionExpiry.probe(reason || 'ws');
            }
        } catch (e) { /* диагностика не должна ломать WS */ }
    }

    // Build WS URL with token if present
    function buildUrl() {
        let url = CFG.WS_URL || `${(window.location.protocol === 'https:' ? 'wss:' : 'ws:')}//${window.location.host}/ws/metrics`;
        if (CFG.API_TOKEN) {
            url += (url.includes('?') ? '&' : '?') + 'token=' + encodeURIComponent(CFG.API_TOKEN);
        }
        return url;
    }

    // Handle incoming messages
    function onMessage(event) {
        try {
            const data = JSON.parse(event.data);
            window.dispatchEvent(new CustomEvent('ws-message', { detail: data }));
        } catch (e) {
            console.error('WS parse error:', e);
        }
    }

    // Connect WebSocket
    function connect() {
        // R679: после истечения сессии новое рукопожатие снова получит 401 —
        // не поднимаем канал, пока оператор не войдёт (явный вход перезагружает
        // страницу либо восстанавливает сессию и зовёт connect() сам).
        if (sessionExpired()) { return; }
        if (ws) {
            try { ws.close(); } catch {}
            ws = null;
        }

        try {
            ws = new WebSocket(buildUrl());

            ws.onopen = () => {
                openedThisAttempt = true;
                reconnectAttempts = 0;
                setStatus(true);
                startHeartbeat();
                window.dispatchEvent(new CustomEvent('ws-open'));
            };

            ws.onmessage = onMessage;

            ws.onclose = () => {
                setStatus(false);
                stopHeartbeat();
                // R679: закрытие БЕЗ единого открытия = отказ рукопожатия.
                // Спрашиваем /auth/me, жива ли сессия (см. noteHandshakeFailure).
                if (!openedThisAttempt) { noteHandshakeFailure('ws-close'); }
                attemptReconnect();
            };

            ws.onerror = (err) => {
                setStatus(false);
                // R679: та же причина, что и в onclose, но приходит раньше.
                if (!openedThisAttempt) { noteHandshakeFailure('ws-error'); }
                window.dispatchEvent(new CustomEvent('ws-error', { detail: err }));
            };
            openedThisAttempt = false;
        } catch (e) {
            setStatus(false);
            attemptReconnect();
        }
    }

    // Reconnect with exponential backoff
    function attemptReconnect() {
        // R679: сессия истекла — переподключение бессмысленно (каждая попытка
        // снова 401). Канал поднимет явный вход.
        if (sessionExpired()) { return; }
        if (reconnectTimer) return; // already scheduled
        if (reconnectAttempts >= MAX_RECONNECT_ATTEMPTS) {
            window.dispatchEvent(new CustomEvent('ws-max-reconnect', {
                detail: { attempts: reconnectAttempts }
            }));
            return;
        }

        reconnectAttempts++;
        const delay = reconnectDelay();

        window.dispatchEvent(new CustomEvent('ws-reconnecting', {
            detail: { attempt: reconnectAttempts, max: MAX_RECONNECT_ATTEMPTS, delay }
        }));

        reconnectTimer = setTimeout(() => {
            reconnectTimer = null;
            connect();
        }, delay);
    }

    // Heartbeat: send ping to keep connection alive
    function startHeartbeat() {
        stopHeartbeat();
        heartbeatTimer = setInterval(() => {
            if (ws && ws.readyState === WebSocket.OPEN) {
                ws.send(JSON.stringify({ type: 'ping' }));
            }
        }, HEARTBEAT_INTERVAL);
    }

    function stopHeartbeat() {
        if (heartbeatTimer) {
            clearInterval(heartbeatTimer);
            heartbeatTimer = null;
        }
    }

    // Public API
    return {
        connect,
        disconnect() {
            if (reconnectTimer) {
                clearTimeout(reconnectTimer);
                reconnectTimer = null;
            }
            stopHeartbeat();
            if (ws) {
                ws.onclose = null;
                try { ws.close(); } catch {}
                ws = null;
            }
            openedThisAttempt = false;
            setStatus(false);
        },
        send(data) {
            if (ws && ws.readyState === WebSocket.OPEN) {
                ws.send(typeof data === 'string' ? data : JSON.stringify(data));
            }
        },
        isConnected() {
            return connected;
        },
        getAttempts() {
            return reconnectAttempts;
        },
        /** R679: канал не поднят из-за истёкшей сессии (для тестов/диагностики). */
        isSuspended() {
            return sessionExpired();
        },
        /** R679: сбросить счётчик попыток — нужен после успешного входа. */
        resetAttempts() {
            reconnectAttempts = 0;
        }
    };
})();

// R59.3 (2026-09-03): `const WebSocketManager = ...` был локальной
// переменной IIFE — app-listeners.js setupWebSocketEvents() вызывает
// `window.WebSocketManager.connect()`, и без этого экспорта получается
// `Cannot read properties of undefined (reading 'connect')` в app.js
// init(). Симптом: connection status badge застревает на "Connecting..."
// (висело с момента 9d12d6a "recretae api / refactor webui", 28 апреля;
// до R57.x split никто не замечал, потому что ошибка падала молча и
// инициализация WebUI всё равно доходила до конца).
if (typeof window !== 'undefined') {
    window.WebSocketManager = WebSocketManager;
}