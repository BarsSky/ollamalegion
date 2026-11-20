// app-logs.js — R653 (2026-11-20): журналы WebUI, прокси-логи, выгрузки и
// уведомления вынесены из `app.js` (оркестратор, цель §5 — < 1500 строк).
//
// ЗАЧЕМ ВЫНЕСЕНО. Одна тема — «поток событий в UI»: добавить запись в журнал
// (F.2: две сигнатуры, addLog/addLogEntry), подключить /ws/logs, показать поля
// события (R481), выгрузить журналы и бэкенды файлом, нарисовать прокси-логи
// (R389/R481) и уведомления (F.α, колокольчик). Оркестратор (init, роутинг,
// обновление страниц) — другое.
//
// ПОЧЕМУ ФАБРИКА. Слой берёт из `app.js` состояние (`data`), рендереры,
// showToast, init и ДОСТУП К ТЕКУЩЕЙ СТРАНИЦЕ. Страница — геттером: `currentPage`
// это `let`, который меняет switchPage (передача значением заморозила бы его).
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: тела перенесены как есть, отступ сохранён.
(function () {
    'use strict';

    window.AppLogs = function (ctx) {
        var data = ctx.data;
        var getCurrentPage = ctx.getCurrentPage;
        var init = ctx.init;
        var renderAgentDetails = ctx.renderAgentDetails;
        var renderAgentsPage = ctx.renderAgentsPage;
        var renderLogs = ctx.renderLogs;
        var renderProxyLogs = ctx.renderProxyLogs;
        var showToast = ctx.showToast;

    function addLogEntry(entry) {
        if (!entry || typeof entry !== 'object') return;
        var locale = (window.I18N && I18N.getLang() === 'ru') ? 'ru' : 'en';
        var timeStr;
        if (entry.time) {
            try {
                var d = entry.time instanceof Date ? entry.time : new Date(entry.time);
                if (!isNaN(d.getTime())) {
                    timeStr = d.toLocaleTimeString(locale);
                }
            } catch (e) { /* ignore */ }
        }
        if (!timeStr) timeStr = new Date().toLocaleTimeString(locale);

        var normalized = {
            time: timeStr,
            level: (entry.level || 'info').toUpperCase(),
            message: entry.message || '',
            source: entry.source || '',
            // R481: поля события — отдельной картой (раньше JSON-суффикс жил
            // внутри message, и лог выглядел как «текст + нечитаемый JSON»).
            fields: (entry.fields && typeof entry.fields === 'object') ? entry.fields : null
        };
        data.logs.unshift(normalized);
        if (data.logs.length > 500) data.logs = data.logs.slice(0, 500);
        if (getCurrentPage() === 'logs') renderLogs(data.logs);
    }

    function addLog(message, level = 'info') {
        // Обратная совместимость: локальные события WebUI (инициализация, ошибки).
        addLogEntry({ level: level, message: message });
    }

    /**
     * F.2 (Session F): инициализация live tail через WebSocket /ws/logs.
     *
     * WS присылает snapshot ring buffer на connect + live LogEntry. Backend-side broker
     * живёт в process balancer (см. internal/api/handlers_logs_ws.go), наполняется
     * через logger.Publish(...) из любого места кода.
     *
     * Token: тот же что и для остальных API endpoints (через WEBUI_CONFIG.API_TOKEN).
     * Если broker не инициализирован на сервере — WS всё равно откроется, пришлёт
     * пустой snapshot и закроется (graceful degradation).
     */
    function initLogsStream() {
        if (!window.logsStream) return;
        var token = (window.WEBUI_CONFIG && window.WEBUI_CONFIG.API_TOKEN) || '';
        window.logsStream.start({
            token: token,
            onSnapshot: function (msg) {
                if (!msg || !Array.isArray(msg.entries)) return;
                // Заменяем data.logs на snapshot (server is source of truth).
                var locale = (window.I18N && I18N.getLang() === 'ru') ? 'ru' : 'en';
                data.logs = msg.entries.map(function (e) {
                    var timeStr;
                    if (e.time) {
                        try {
                            var d = e.time instanceof Date ? e.time : new Date(e.time);
                            if (!isNaN(d.getTime())) timeStr = d.toLocaleTimeString(locale);
                        } catch (err) { /* ignore */ }
                    }
                    if (!timeStr) timeStr = new Date().toLocaleTimeString(locale);
                    return {
                        time: timeStr,
                        level: (e.level || 'info').toUpperCase(),
                        message: e.message || '',
                        source: e.source || '',
                        fields: (e.fields && typeof e.fields === 'object') ? e.fields : null
                    };
                });
                if (getCurrentPage() === 'logs') renderLogs(data.logs);
            },
            onEntry: function (entry) {
                addLogEntry(entry);
            }
        });
    }

    // ---- Export ----

    /**
     * Export системных логов в файл (Session E — Export logs to CSV).
     *
     * Поддерживает 3 формата: CSV (RFC 4180), TXT (legacy), JSON.
     * Применяет level filter из #logsLevelFilter (по умолчанию "all" — без фильтра).
     *
     * CSV-формат: "timestamp,level,message\r\n<row>..." с правильным escape для
     * запятых/кавычек/переносов строк (RFC 4180 §2.6/§2.7).
     *
     * Использует Utils.downloadFile для Blob-based download (без сервера).
     */
    // logFieldsPairsR481 — те же пары key/value, что рисует renderers.js.
    function logFieldsPairsR481(fields) {
        if (!fields || typeof fields !== 'object') return [];
        return Object.keys(fields).sort().map(function (k) {
            var v = fields[k];
            return [k, (v === null || v === undefined) ? '' : (typeof v === 'object' ? JSON.stringify(v) : String(v))];
        });
    }

    function logFieldsTextR481(fields) {
        return logFieldsPairsR481(fields).map(function (p) { return p[0] + '=' + p[1]; }).join(' ');
    }

    function exportLogs() {
        // R481: экспорт учитывает ОБА фильтра панели — уровень и поиск, — и
        // включает поля события. Раньше «Экспорт» игнорировал поиск и терял
        // поля (они были склеены в message).
        var levelSelect = document.getElementById('logsLevelFilter');
        var levelFilter = levelSelect ? levelSelect.value : 'all';
        var searchEl = document.getElementById('logsSearch');
        var searchQuery = String(searchEl ? searchEl.value : '').trim().toLowerCase();

        var filteredLogs = (data.logs || []).filter(function (l) {
            if (!l) return false;
            if (levelFilter !== 'all' && String(l.level || '').toLowerCase() !== levelFilter) return false;
            if (!searchQuery) return true;
            var haystack = [l.message || '', l.source || '', logFieldsTextR481(l.fields)].join(' ').toLowerCase();
            return haystack.indexOf(searchQuery) !== -1;
        });
        if (!filteredLogs.length) {
            showToast(window.I18N ? I18N.t('logs.export_empty') : 'Nothing to export: no entries left after filtering', 'warning');
            return;
        }

        // Определяем формат из #logsExportFormat (default csv).
        var formatSelect = document.getElementById('logsExportFormat');
        var format = formatSelect ? formatSelect.value : 'csv';

        var content = '';
        var mime = 'text/plain';
        var ext = 'txt';
        var stamp = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);

        if (format === 'csv') {
            // RFC 4180: поля с запятой/кавычкой/переносом оборачиваются в кавычки,
            // кавычки внутри удваиваются.
            var escapeCsv = function (val) {
                var s = val === null || val === undefined ? '' : String(val);
                if (/[",\r\n]/.test(s)) {
                    return '"' + s.replace(/"/g, '""') + '"';
                }
                return s;
            };
            content = 'timestamp,level,source,message,fields\r\n' + filteredLogs.map(function (l) {
                return escapeCsv(l.time) + ',' + escapeCsv(l.level) + ',' + escapeCsv(l.source) +
                    ',' + escapeCsv(l.message) + ',' + escapeCsv(logFieldsTextR481(l.fields));
            }).join('\r\n');
            mime = 'text/csv';
            ext = 'csv';
        } else if (format === 'json') {
            content = JSON.stringify({
                exportedAt: new Date().toISOString(),
                count: filteredLogs.length,
                levelFilter: levelFilter,
                logs: filteredLogs
            }, null, 2);
            mime = 'application/json';
            ext = 'json';
        } else {
            // TXT (legacy): [time] LEVEL: message
            content = filteredLogs.map(function (l) {
                var line = '[' + (l.time || '') + '] ' + (l.level || '') + ': ' + (l.message || '');
                var fields = logFieldsTextR481(l.fields);
                if (fields) line += ' | ' + fields;
                return line;
            }).join('\n');
        }

        var filename = 'ollamalegion-logs-' + stamp + (levelFilter !== 'all' ? '-' + levelFilter : '') + '.' + ext;
        Utils.downloadFile(content, filename, mime);
        if (window.I18N) {
            showToast(I18N.t('logs.export_done', { filename: filename, count: filteredLogs.length }), 'success');
        }
    }

    // exportProxyLogsR481 — скачивание отфильтрованных прокси-логов файлом.
    //
    // Раньше у прокси-логов была только кнопка «Копировать» (TSV в буфер) — ни
    // файла, ни формата. Теперь CSV/JSON с учётом фильтров уровня и поиска;
    // в детали попадают request_id, stage, полный текст ошибки.
    function exportProxyLogs() {
        var entries = Array.isArray(data.proxyLogs) ? data.proxyLogs : [];
        var levelFilter = (window.proxyLogFilters && window.proxyLogFilters.level) || 'all';
        var searchQuery = String((window.proxyLogFilters && window.proxyLogFilters.search) || '').toLowerCase();
        var filtered = entries.filter(function (e) {
            if (!e) return false;
            if (levelFilter !== 'all') {
                var sc = e.statusCode || 0;
                if (levelFilter === '2xx' && (sc < 200 || sc >= 300)) return false;
                if (levelFilter === '4xx' && (sc < 400 || sc >= 500)) return false;
                if (levelFilter === '5xx' && sc < 500) return false;
            }
            if (!searchQuery) return true;
            var haystack = [e.path, e.model, e.backendID, e.error, e.clientName, e.requestID, e.stage]
                .join(' ').toLowerCase();
            return haystack.indexOf(searchQuery) !== -1;
        });
        if (!filtered.length) {
            showToast(window.I18N ? I18N.t('logs.export_empty') : 'Nothing to export', 'warning');
            return;
        }
        var formatEl = document.getElementById('proxyExportFormat');
        var format = formatEl ? formatEl.value : 'csv';
        var stamp = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
        var content = '';
        var mime = 'text/csv';
        var ext = 'csv';
        var escapeCsv = function (val) {
            var s = val === null || val === undefined ? '' : String(val);
            return /[",\r\n]/.test(s) ? '"' + s.replace(/"/g, '""') + '"' : s;
        };
        if (format === 'json') {
            content = JSON.stringify({ exportedAt: new Date().toISOString(), count: filtered.length, entries: filtered }, null, 2);
            mime = 'application/json';
            ext = 'json';
        } else {
            content = 'timestamp,method,path,model,client,status,durationMs,backend,stage,requestID,error\r\n' +
                filtered.map(function (e) {
                    return [
                        e.timestamp || '', e.method || '', e.path || '', e.model || '',
                        e.clientName || e.clientIP || '', e.statusCode || '', e.durationMs || '',
                        e.backendID || '', e.stage || '', e.requestID || '', e.error || ''
                    ].map(escapeCsv).join(',');
                }).join('\r\n');
        }
        var filename = 'ollamalegion-proxy-logs-' + stamp + '.' + ext;
        Utils.downloadFile(content, filename, mime);
        if (window.I18N) {
            showToast(I18N.t('logs.export_done', { filename: filename, count: filtered.length }), 'success');
        }
    }

    function exportBackends() {
        const content = JSON.stringify(data.backends, null, 2);
        Utils.downloadFile(content, 'ollamalegion-backends.json', 'application/json');
    }

    // ---- Proxy Logs ----

    function addProxyLog(entry) {
        if (!entry) return;
        var locale = (window.I18N && I18N.getLang() === 'ru') ? 'ru' : 'en';
        if (entry.timestamp) {
            entry._time = new Date(entry.timestamp).toLocaleTimeString(locale);
        } else {
            entry._time = new Date().toLocaleTimeString(locale);
        }
        data.proxyLogs.unshift(entry);
        if (data.proxyLogs.length > 500) data.proxyLogs = data.proxyLogs.slice(0, 500);
        // Round 16 (2026-07-10): для 4xx/5xx дубль в общий Logs page — пользователь
        // увидит и в Logs (system) и в Proxy Logs. Бэкенд вернул клиенту ошибку →
        // оператор должен видеть это в обеих секциях.
        if (entry.statusCode && entry.statusCode >= 400) {
            var logEntry = {
                time: entry._time,
                level: entry.statusCode >= 500 ? 'error' : 'warn',
                message: 'Backend "' + (entry.backendID || '?') + '" returned ' + entry.statusCode +
                    ' on ' + (entry.method || '?') + ' ' + (entry.path || '?') +
                    (entry.error ? ' — ' + String(entry.error).substring(0, 200) : ''),
                source: 'proxy'
            };
            data.logs.unshift(logEntry);
            if (data.logs.length > 1000) data.logs = data.logs.slice(0, 1000);
            // Если активна вкладка Logs (system, не proxy) — тоже обновить
            if (getCurrentPage() === 'logs') {
                var systemTab = document.getElementById('logsTabSystem') || document.getElementById('logsTab');
                var proxyTabActive = document.getElementById('logsTabProxy');
                if (systemTab && systemTab.classList.contains('active') &&
                    !(proxyTabActive && proxyTabActive.classList.contains('active'))) {
                    renderLogs(data.logs);
                }
            }
        }
        if (getCurrentPage() === 'logs') {
            var proxyTab = document.getElementById('logsTabProxy');
            if (proxyTab && proxyTab.classList.contains('active')) {
                renderProxyLogs(data.proxyLogs);
            }
        }
    }

    // ---- Agents ----

    async function fetchAgents() {
        try {
            data.agents = await Api.agentsStats();
            if (getCurrentPage() === 'agents') renderAgentsPage(data.agents);
        } catch (e) {
            Api.handleError(e, window.I18N ? I18N.t('app.error_loading_agents') : 'Error loading agents');
        }
    }

    async function showAgentDetails(agentId) {
        var card = document.getElementById('agentDetailsCard');
        var content = document.getElementById('agentDetailsContent');
        if (!card || !content) return;
        content.innerHTML = '<div class="loading">' + (window.I18N ? I18N.t('common.loading') : 'Loading...') + '</div>';
        card.style.display = 'block';
        try {
            var info = await Api.agentInfo(agentId);
            renderAgentDetails(info);
        } catch (e) {
            content.innerHTML = '<div class="error">' + (window.I18N ? I18N.t('common.error') : 'Error') + ': ' + (e.message || e) + '</div>';
        }
    }

    // ---- Notifications (F.α) ----
    // helpers: рендер отдельного уведомления, отрисовка списка, UI-wiring (bell click, mark-read, clear).

    /**
     * Добавляет новое уведомление в DOM список + обновляет badge.
     * Вызывается из window.notifications.callbacks.onNew.
     */
    function renderNotification(ev) {
        if (!ev) return;
        var list = document.getElementById('notificationsList');
        var badge = document.getElementById('notificationsBadge');
        if (!list) return;

        var severity = (ev.severity || 'info').toLowerCase();
        var time = ev.time || new Date().toISOString();
        var timeStr = new Date(time).toLocaleTimeString(window.I18N && I18N.getLang() === 'ru' ? 'ru' : 'en');
        var source = ev.source ? '<span class="notification-source">' + Utils.escapeHtml(ev.source) + '</span>' : '';
        var severityLabel = window.I18N ? I18N.t('notifications.severity.' + severity) : severity;

        var li = document.createElement('li');
        li.className = 'notifications-item notifications-item-' + severity;
        li.dataset.eventTime = time;
        li.innerHTML =
            '<div class="notifications-item-header">' +
                '<span class="notifications-severity notifications-severity-' + severity + '">' + Utils.escapeHtml(severityLabel) + '</span>' +
                source +
                '<span class="notifications-time">' + Utils.escapeHtml(timeStr) + '</span>' +
            '</div>' +
            '<div class="notifications-message">' + Utils.escapeHtml(ev.message || '') + '</div>';

        list.insertBefore(li, list.firstChild);

        // Лимит 50 видимых (сервер держит 100 в ring buffer; UI ограничиваем сильнее).
        while (list.children.length > 50) {
            list.removeChild(list.lastChild);
        }

        // Badge — непрочитанные = общее число видимых (read state мы не храним, по требованию F.α — простая индикация).
        if (badge) {
            var unread = list.querySelectorAll('.notifications-item').length;
            if (unread > 0) {
                badge.textContent = unread > 99 ? '99+' : String(unread);
                badge.hidden = false;
            } else {
                badge.hidden = true;
            }
        }
    }

    /**
     * Перерисовка всего списка (после onClear из notifications.js).
     * Источник истины — DOM внутри #notificationsList; здесь мы только чистим + badge.
     */
    function renderNotificationsList() {
        var list = document.getElementById('notificationsList');
        var badge = document.getElementById('notificationsBadge');
        if (!list) return;
        list.innerHTML = '';
        if (badge) {
            badge.hidden = true;
            badge.textContent = '0';
        }
        // Empty-state placeholder
        var empty = document.createElement('li');
        empty.className = 'notifications-empty';
        empty.textContent = window.I18N ? I18N.t('notifications.empty') : 'No notifications';
        list.appendChild(empty);
    }

    /**
     * Подключает обработчики к bell-кнопке, dropdown, mark-read, clear.
     * Вызывается один раз в init() после window.notifications.init().
     */
    function setupNotificationsUI() {
        var btn = document.getElementById('notificationsBtn');
        var dropdown = document.getElementById('notificationsDropdown');
        var markReadBtn = document.getElementById('notificationsMarkRead');
        var clearBtn = document.getElementById('notificationsClear');
        var list = document.getElementById('notificationsList');

        if (btn && dropdown) {
            // 2026-06-29 v3: Portal-паттерн. Переносим .notifications-dropdown
            // в document.body, чтобы обойти containing block от родителей с
            // backdrop-filter / transform / filter / will-change / contain
            // (в частности .header использует backdrop-filter, а иконки —
            // transform: scale при hover; оба ломают "position: fixed").
            // После переноса position: fixed гарантированно привязан к
            // viewport, а z-index 2147483000 попадает в корневой stacking
            // context и не "проваливается" под вкладки.
            if (dropdown.parentNode !== document.body) {
                document.body.appendChild(dropdown);
            }

            function positionDropdown() {
                // Не пересчитываем на мобильных — там CSS bottom-sheet
                if (window.matchMedia && window.matchMedia('(max-width: 768px)').matches) {
                    return;
                }
                var rect = btn.getBoundingClientRect();
                var dropdownW = dropdown.offsetWidth || 380;
                var dropdownH = dropdown.offsetHeight || 480;
                var margin = 8;
                // top: сразу под кнопкой
                var top = rect.bottom + margin;
                // right: правый край выравнивается с правым краем кнопки
                var right = Math.max(margin, window.innerWidth - rect.right);
                // Если dropdown не помещается снизу — открыть вверх
                if (top + dropdownH > window.innerHeight - margin) {
                    top = Math.max(margin, rect.top - dropdownH - margin);
                }
                // Если dropdown не помещается слева — сдвинуть к левому краю
                if (right + dropdownW > window.innerWidth - margin) {
                    right = margin;
                }
                dropdown.style.top = top + 'px';
                dropdown.style.right = right + 'px';
                dropdown.style.left = 'auto';
                dropdown.style.bottom = 'auto';
            }
            btn.addEventListener('click', function (e) {
                e.stopPropagation();
                var isOpen = !dropdown.hidden;
                if (!isOpen) {
                    // Открытие — сначала показываем для измерения, потом позиционируем
                    dropdown.hidden = false;
                    positionDropdown();
                } else {
                    dropdown.hidden = true;
                    // Очищаем inline-position для корректной работы mobile @media
                    dropdown.style.top = '';
                    dropdown.style.right = '';
                    dropdown.style.left = '';
                    dropdown.style.bottom = '';
                }
                btn.setAttribute('aria-expanded', String(!isOpen));
            });
            // При ресайзе пересчитываем позицию, если dropdown открыт
            window.addEventListener('resize', function () {
                if (!dropdown.hidden) positionDropdown();
            });
            // Закрываем dropdown при клике снаружи
            document.addEventListener('click', function (e) {
                if (dropdown.hidden) return;
                if (e.target === btn || (btn.contains && btn.contains(e.target))) return;
                if (dropdown.contains && dropdown.contains(e.target)) return;
                dropdown.hidden = true;
                dropdown.style.top = '';
                dropdown.style.right = '';
                dropdown.style.left = '';
                dropdown.style.bottom = '';
                btn.setAttribute('aria-expanded', 'false');
            });
            // Esc закрывает dropdown
            document.addEventListener('keydown', function (e) {
                if (e.key === 'Escape' && !dropdown.hidden) {
                    dropdown.hidden = true;
                    dropdown.style.top = '';
                    dropdown.style.right = '';
                    dropdown.style.left = '';
                    dropdown.style.bottom = '';
                    btn.setAttribute('aria-expanded', 'false');
                }
            });
        }

        if (markReadBtn) {
            markReadBtn.addEventListener('click', function (e) {
                e.stopPropagation();
                var badge = document.getElementById('notificationsBadge');
                if (badge) {
                    badge.hidden = true;
                    badge.textContent = '0';
                }
                if (list) {
                    var items = list.querySelectorAll('.notifications-item');
                    items.forEach(function (el) { el.classList.add('notifications-item-read'); });
                }
            });
        }

        if (clearBtn) {
            clearBtn.addEventListener('click', function (e) {
                e.stopPropagation();
                if (window.notifications && typeof window.notifications.clear === 'function') {
                    window.notifications.clear();
                } else {
                    renderNotificationsList();
                }
            });
        }

        // Initial render — empty state
        if (list && list.children.length === 0) {
            renderNotificationsList();
        }
    }

        return {
            addLog: addLog,
            addLogEntry: addLogEntry,
            addProxyLog: addProxyLog,
            initLogsStream: initLogsStream,
            exportLogs: exportLogs,
            exportProxyLogs: exportProxyLogs,
            exportBackends: exportBackends,
            renderNotification: renderNotification,
            renderNotificationsList: renderNotificationsList,
            setupNotificationsUI: setupNotificationsUI,
            fetchAgents: fetchAgents,
            showAgentDetails: showAgentDetails
        };
    };
})();
