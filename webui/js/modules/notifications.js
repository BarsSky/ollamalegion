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

    // R83 (2026-09-25): экранирование для innerHTML.
    //
    // ЗАЧЕМ. Раньше ev.message / ev.source / ev.model подставлялись в innerHTML
    // КАК ЕСТЬ. А приходит туда внешний текст: сообщения cppworker/llama.cpp
    // (включая пути и сырые ошибки) и имя модели ИЗ ЗАПРОСА КЛИЕНТА. То есть
    // клиент мог прислать имя вида `<img src=x onerror=...>` и получить
    // исполнение скрипта в браузере оператора — хранимый XSS в панели
    // мониторинга. С появлением уведомлений о провале загрузки (туда попадает
    // raw_error от llama.cpp) это стало ещё актуальнее.
    function escapeHtml(value) {
        return String(value == null ? '' : value)
            .replace(/&/g, '&amp;')
            .replace(/</g, '&lt;')
            .replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;')
            .replace(/'/g, '&#39;');
    }

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
                // severity попадает в ИМЯ CSS-класса, поэтому пропускаем только
                // известные значения — иначе значение из события ломает разметку.
                const severity = SEVERITY_ICONS[ev.severity] ? ev.severity : 'info';
                li.className = `notifications-item severity-${severity}`;

                const icon = SEVERITY_ICONS[severity] || SEVERITY_ICONS.info;
                const ts = ev.timestamp ? new Date(ev.timestamp).toLocaleString() : '';
                // R83: всё внешнее — через escapeHtml (см. комментарий выше).
                const source = ev.source ? `[${escapeHtml(ev.source)}]` : '';
                const model = ev.model ? ` (${escapeHtml(ev.model)})` : '';

                li.innerHTML = `
                    <span class="notifications-icon">${icon}</span>
                    <div class="notifications-content">
                        <div class="notifications-header">
                            <strong>${source}${model}</strong>
                            <span class="notifications-time">${escapeHtml(ts)}</span>
                        </div>
                        <div class="notifications-message">${escapeHtml(ev.message || '')}</div>
                        ${this._renderDetails(ev)}
                    </div>
                `;
                container.appendChild(li);
            });
        }

        // _renderDetails — «подробности» уведомления: числа из data.diagnostics.
        //
        // R83 (2026-09-25): уведомление о провале загрузки должно быть
        // actionable без похода в логи. Для отказа по n_ctx это запрошенное
        // значение против границ VRAM/RAM и обучающего контекста; для «модель не
        // найдена» — список доступных. Всё экранируется.
        _renderDetails(ev) {
            const data = ev && ev.data;
            const d = data && data.diagnostics;
            if (!d || typeof d !== 'object') return '';

            const rows = [];
            const add = (label, value) => {
                if (value === undefined || value === null || value === '') return;
                rows.push(
                    '<div style="display:flex;gap:6px;font-size:11px;color:var(--text-secondary)">' +
                    (label ? `<span style="min-width:96px">${escapeHtml(label)}</span>` : '<span></span>') +
                    `<span>${escapeHtml(String(value))}</span></div>`
                );
            };

            add('n_ctx запрошен', d.requested_n_ctx);
            // R83 (2026-09-30): «max_vram_n_ctx» — это НЕ «максимум, что модель
            // может обслужить», а «сколько влезает ЦЕЛИКОМ в VRAM»; для второго
            // есть отдельное поле (hard_max_n_ctx — с частичным оффлоадом).
            // Раньше подпись «влезает в VRAM» читалась как общий потолок, и
            // оператор не понимал, почему модель работает на большем n_ctx.
            add('целиком в VRAM', d.max_vram_n_ctx);
            add('с частичным оффлоадом', d.hard_max_n_ctx);
            add('gguf max', d.gguf_max_context);
            if (Array.isArray(d.available_models) && d.available_models.length) {
                add('доступны', d.available_models.slice(0, 8).join(', '));
            }
            add('', d.suggestion);

            if (!rows.length) return '';
            return '<div style="margin-top:4px;display:flex;flex-direction:column;gap:2px">' +
                rows.join('') + '</div>';
        }
    }

    // Singleton export.
    window.notifications = new NotificationsManager();
})();