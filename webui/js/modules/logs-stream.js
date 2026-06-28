// webui/js/modules/logs-stream.js
// F.2 (Session F) — Live tail Logs через WebSocket /ws/logs.
//
// Singleton window.logsStream с методами:
//   - start({ token, onEntry, onSnapshot, onOpen, onClose }) — устанавливает WS-соединение.
//   - stop() — закрывает WS и останавливает reconnect.
//   - subscribe(callback) — добавляет callback для live entries (мультиподписчик).
//
// Протокол (см. internal/api/handlers_logs_ws.go):
//   - На connect: одно сообщение {type:"snapshot", entries:[LogEntry...], count:N}.
//   - Live:        одно сообщение LogEntry = {time, level, message, source}.
//   - Keep-alive:  {type:"ping"} каждые 30 сек (от сервера).
//
// Reconnect: экспоненциальный backoff 1s → 30s, max 60s, сброс на success.
// Token передаётся в query (?token=...) — EventSource API не поддерживает custom headers.

(function () {
    'use strict';

    class LogsStream {
        constructor() {
            this.ws = null;
            this.token = '';
            this.callbacks = {
                onEntry: [],    // callback(entry) — live LogEntry
                onSnapshot: [], // callback({entries, count}) — initial backlog
                onOpen: [],     // callback() — WS open
                onClose: []     // callback() — WS close (нормальный или reconnect)
            };
            this.reconnectAttempts = 0;
            this.reconnectTimer = null;
            this.shouldReconnect = false;
            this.startedAt = null;
            this.lastError = null;
        }

        /**
         * Запустить WebSocket stream.
         * @param {Object} opts
         * @param {string} opts.token — API token для query ?token=
         * @param {Function} [opts.onEntry] — live entry callback
         * @param {Function} [opts.onSnapshot] — initial backlog callback
         * @param {Function} [opts.onOpen] — open callback
         * @param {Function} [opts.onClose] — close callback
         */
        start(opts) {
            opts = opts || {};
            this.token = opts.token || '';
            if (opts.onEntry) this.callbacks.onEntry.push(opts.onEntry);
            if (opts.onSnapshot) this.callbacks.onSnapshot.push(opts.onSnapshot);
            if (opts.onOpen) this.callbacks.onOpen.push(opts.onOpen);
            if (opts.onClose) this.callbacks.onClose.push(opts.onClose);

            this.shouldReconnect = true;
            this.startedAt = new Date();
            this._connect();
        }

        /**
         * Добавить callback для live entries (мультиподписчик).
         */
        subscribe(callback) {
            if (typeof callback === 'function') {
                this.callbacks.onEntry.push(callback);
            }
        }

        /**
         * Остановить stream. После stop() reconnect не сработает.
         */
        stop() {
            this.shouldReconnect = false;
            if (this.reconnectTimer) {
                clearTimeout(this.reconnectTimer);
                this.reconnectTimer = null;
            }
            if (this.ws) {
                try {
                    this.ws.close(1000, 'client stop');
                } catch (e) { /* ignore */ }
                this.ws = null;
            }
            this.startedAt = null;
        }

        /**
         * Подключиться к /ws/logs. WS URL собирается из текущего origin/api_base.
         */
        _connect() {
            if (!this.shouldReconnect) return;

            // Собираем base URL из window.WEBUI_CONFIG (Docker/local nginx inject).
            var cfg = window.WEBUI_CONFIG || {};
            var apiBase = cfg.API_BASE || '';
            // Нормализуем: убираем trailing slash, делаем ws:// из http:// или wss:// из https://.
            var wsBase;
            if (apiBase) {
                if (apiBase.indexOf('https://') === 0) {
                    wsBase = 'wss://' + apiBase.substring(8);
                } else if (apiBase.indexOf('http://') === 0) {
                    wsBase = 'ws://' + apiBase.substring(7);
                } else {
                    wsBase = apiBase.replace(/\/$/, '');
                }
            } else {
                // Fallback: текущий origin.
                var proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
                wsBase = proto + '//' + window.location.host;
            }

            var url = wsBase.replace(/\/$/, '') + '/ws/logs';
            if (this.token) {
                url += '?token=' + encodeURIComponent(this.token);
            }

            var self = this;
            try {
                this.ws = new WebSocket(url);
            } catch (e) {
                this.lastError = e;
                this._scheduleReconnect();
                return;
            }

            this.ws.onopen = function () {
                self.reconnectAttempts = 0; // reset на успешном open
                self.lastError = null;
                self.callbacks.onOpen.forEach(function (cb) {
                    try { cb(); } catch (e) { /* ignore */ }
                });
            };

            this.ws.onmessage = function (ev) {
                self._handleMessage(ev.data);
            };

            this.ws.onerror = function (e) {
                self.lastError = e;
                // onerror всегда сопровождается onclose — reconnect там.
            };

            this.ws.onclose = function (ev) {
                self.callbacks.onClose.forEach(function (cb) {
                    try { cb(ev); } catch (e) { /* ignore */ }
                });
                self.ws = null;
                if (self.shouldReconnect) {
                    self._scheduleReconnect();
                }
            };
        }

        /**
         * Обработка одного WS-сообщения: snapshot или LogEntry.
         */
        _handleMessage(raw) {
            var msg;
            try {
                msg = JSON.parse(raw);
            } catch (e) {
                // Невалидный JSON — игнорируем.
                return;
            }
            if (!msg || typeof msg !== 'object') return;

            // Snapshot: {type:"snapshot", entries:[...], count:N}
            if (msg.type === 'snapshot' && Array.isArray(msg.entries)) {
                this.callbacks.onSnapshot.forEach(function (cb) {
                    try { cb(msg); } catch (e) { /* ignore */ }
                });
                return;
            }

            // Keep-alive ping.
            if (msg.type === 'ping') {
                return;
            }

            // Live LogEntry: {time, level, message, source}
            if (msg.time && (msg.level || msg.message)) {
                this.callbacks.onEntry.forEach(function (cb) {
                    try { cb(msg); } catch (e) { /* ignore */ }
                });
            }
        }

        /**
         * Reconnect с экспоненциальным backoff: 1s, 2s, 4s, 8s, 16s, 30s (max).
         */
        _scheduleReconnect() {
            if (!this.shouldReconnect) return;
            if (this.reconnectTimer) return; // уже запланирован

            this.reconnectAttempts++;
            var delayMs = Math.min(30000, 1000 * Math.pow(2, this.reconnectAttempts - 1));
            // Jitter ±10% чтобы не было thundering herd.
            var jitter = Math.round(delayMs * 0.1 * (Math.random() * 2 - 1));
            delayMs = Math.max(500, delayMs + jitter);

            var self = this;
            this.reconnectTimer = setTimeout(function () {
                self.reconnectTimer = null;
                self._connect();
            }, delayMs);
        }

        /**
         * Диагностический статус для health UI.
         */
        status() {
            return {
                connected: this.ws && this.ws.readyState === WebSocket.OPEN,
                reconnectAttempts: this.reconnectAttempts,
                startedAt: this.startedAt,
                lastError: this.lastError ? String(this.lastError) : null,
                readyState: this.ws ? this.ws.readyState : null
            };
        }
    }

    window.logsStream = new LogsStream();
})();