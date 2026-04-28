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
        if (ws) {
            try { ws.close(); } catch {}
            ws = null;
        }

        try {
            ws = new WebSocket(buildUrl());

            ws.onopen = () => {
                reconnectAttempts = 0;
                setStatus(true);
                startHeartbeat();
                window.dispatchEvent(new CustomEvent('ws-open'));
            };

            ws.onmessage = onMessage;

            ws.onclose = () => {
                setStatus(false);
                stopHeartbeat();
                attemptReconnect();
            };

            ws.onerror = (err) => {
                setStatus(false);
                window.dispatchEvent(new CustomEvent('ws-error', { detail: err }));
            };
        } catch (e) {
            setStatus(false);
            attemptReconnect();
        }
    }

    // Reconnect with exponential backoff
    function attemptReconnect() {
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
        }
    };
})();