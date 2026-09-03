// webui/js/modules/notifications.js — singleton для SSE notifications (F.α).
//
// Архитектура:
//   - EventSource подключается к GET /api/v1/events?token=<X-API-Token>.
//   - При получении события добавляется в ring buffer (max 50 в памяти).
//   - bell icon показывает badge с количеством непрочитанных событий.
//   - dropdown отображает список событий с severity icons + timestamps.
//   - При reconnect — exponential backoff (1s → 30s).
//   - При mount — persist unread count в localStorage.
//
// F.α (2026-06-28): session F.

(function () {
    'use strict';

    const STORAGE_KEY_READ_IDS = 'notifications.readIds';
    const STORAGE_KEY_LAST_SEEN_TS = 'notifications.lastSeenTs';
    const MAX_BUFFER_SIZE = 50;
    const RECONNECT_BASE_MS = 1000;
    const RECONNECT_MAX_MS = 30000;

    // Severity icons (Unicode symbols — кросс-платформенные).
    const SEVERITY_ICONS = {
        info: 'ℹ️',
        warning: '⚠️',
        error: '❌',
        critical: '🔥',
    };

    class NotificationsManager {
        constructor() {
            this.eventSource = null;
            this.buffer = [];          // ring buffer событий
            this.unreadCount = 0;
            this.readIds = new Set();
            this.lastSeenTs = null;
            this.reconnectAttempt = 0;
            this.callbacks = {
                onNew: [],             // новые события
                onClear: [],           // очистка
            };
            this._closed = false;
            this._loadFromStorage();
        }

        // init() запускает EventSource с правильным токеном.
        init({ token, onNew, onClear } = {}) {
            if (onNew) this.callbacks.onNew.push(onNew);
            if (onClear) this.callbacks.onClear.push(onClear);
            this._connect(token);
        }

        stop() {
            this._closed = true;
            if (this.eventSource) {
                this.eventSource.close();
                this.eventSource = null;
            }
        }

        markAllAsRead() {
            this.buffer.forEach(ev => this.readIds.add(ev.id || `${ev.timestamp}-${ev.message}`));
            this.unreadCount = 0;
            this._saveToStorage();
            this._renderBadge();
        }

        clear() {
            this.buffer = [];
            this.unreadCount = 0;
            this.readIds.clear();
            this._saveToStorage();
            this.callbacks.onClear.forEach(cb => { try { cb(); } catch (e) { console.error(e); } });
        }

        getUnreadCount() {
            return this.unreadCount;
        }

        getBuffer() {
            return this.buffer.slice();
        }

        _connect(token) {
            if (this._closed) return;

            const url = `/api/v1/events?token=${encodeURIComponent(token || '')}`;
            try {
                this.eventSource = new EventSource(url);
            } catch (err) {
                console.error('NotificationsManager: EventSource failed to construct', err);
                this._scheduleReconnect(token);
                return;
            }

            this.eventSource.onopen = () => {
                console.info('NotificationsManager: SSE connection opened');
                this.reconnectAttempt = 0;
            };

            this.eventSource.onerror = (err) => {
                console.warn('NotificationsManager: SSE connection error', err);
                if (this.eventSource) {
                    this.eventSource.close();
                    this.eventSource = null;
                }
                this._scheduleReconnect(token);
            };

            this.eventSource.onmessage = (e) => {
                if (!e.data || e.data === ': ping') return; // heartbeat
                try {
                    const ev = JSON.parse(e.data);
                    this._handleEvent(ev);
                } catch (err) {
                    console.warn('NotificationsManager: failed to parse SSE event', err, e.data);
                }
            };
        }

        _scheduleReconnect(token) {
            if (this._closed) return;
            const delay = Math.min(
                RECONNECT_BASE_MS * Math.pow(2, this.reconnectAttempt),
                RECONNECT_MAX_MS
            );
            this.reconnectAttempt++;
            console.info(`NotificationsManager: reconnect in ${delay}ms (attempt ${this.reconnectAttempt})`);
            setTimeout(() => this._connect(token), delay);
        }

        _handleEvent(ev) {
            if (!ev || ev.type !== 'notification') return;

            // Назначаем id если нет.
            if (!ev.id) {
                ev.id = `${ev.timestamp || Date.now()}-${ev.message || ''}`;
            }

            // Добавляем в ring buffer.
            this.buffer.unshift(ev);
            if (this.buffer.length > MAX_BUFFER_SIZE) {
                this.buffer = this.buffer.slice(0, MAX_BUFFER_SIZE);
            }

            // Increment unread только если событие новее lastSeenTs.
            const evTs = ev.timestamp ? new Date(ev.timestamp).getTime() : Date.now();
            if (!this.lastSeenTs || evTs > this.lastSeenTs) {
                this.unreadCount++;
                this._renderBadge();
            }

            // Notify listeners.
            this.callbacks.onNew.forEach(cb => {
                try { cb(ev); } catch (e) { console.error(e); }
            });
        }

        _renderBadge() {
            const badge = document.getElementById('notificationsBadge');
            if (!badge) return;
            if (this.unreadCount > 0) {
                badge.textContent = this.unreadCount > 99 ? '99+' : String(this.unreadCount);
                badge.hidden = false;
            } else {
                badge.hidden = true;
            }
        }

        _loadFromStorage() {
            try {
                const ids = localStorage.getItem(STORAGE_KEY_READ_IDS);
                if (ids) this.readIds = new Set(JSON.parse(ids));
                const ts = localStorage.getItem(STORAGE_KEY_LAST_SEEN_TS);
                if (ts) this.lastSeenTs = parseInt(ts, 10);
            } catch (e) {
                console.warn('NotificationsManager: localStorage load failed', e);
            }
        }

        _saveToStorage() {
            try {
                localStorage.setItem(STORAGE_KEY_READ_IDS, JSON.stringify([...this.readIds]));
                if (this.lastSeenTs) {
                    localStorage.setItem(STORAGE_KEY_LAST_SEEN_TS, String(this.lastSeenTs));
                }
            } catch (e) {
                console.warn('NotificationsManager: localStorage save failed', e);
            }
        }

        // render() — рендерит dropdown с событиями.
        render(containerId) {
            const container = document.getElementById(containerId);
            if (!container) return;
            container.innerHTML = '';

            if (this.buffer.length === 0) {
                const empty = document.createElement('li');
                empty.className = 'notifications-empty';
                empty.dataset = { i18n: 'notifications.empty' };
                empty.textContent = window.I18N ? window.I18N.t('notifications.empty') : 'No notifications';
                container.appendChild(empty);
                return;
            }

            this.buffer.forEach(ev => {
                const li = document.createElement('li');
                li.className = `notifications-item severity-${ev.severity || 'info'}`;

                const icon = SEVERITY_ICONS[ev.severity] || SEVERITY_ICONS.info;
                const ts = ev.timestamp ? new Date(ev.timestamp).toLocaleString() : '';
                const source = ev.source ? `[${ev.source}]` : '';
                const model = ev.model ? ` (${ev.model})` : '';

                li.innerHTML = `
                    <span class="notifications-icon">${icon}</span>
                    <div class="notifications-content">
                        <div class="notifications-header">
                            <strong>${source}${model}</strong>
                            <span class="notifications-time">${ts}</span>
                        </div>
                        <div class="notifications-message">${ev.message || ''}</div>
                    </div>
                `;
                container.appendChild(li);
            });
        }
    }

    // Singleton export.
    window.notifications = new NotificationsManager();
})();