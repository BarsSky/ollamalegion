// renderers-logs.js — R650 (2026-11-20): очередь, журналы событий и прокси-логи
// вынесены из `renderers.js` (цель §5 — < 1500 строк).
//
// ЗАЧЕМ ВЫНЕСЕНО. Три экрана читают ОДИН поток событий: admission-очередь (R91),
// журнал логов с раскрываемыми полями (R481) и прокси-логи с request_id/stage
// (R389/R481). Они меняются вместе и по своей причине — «что показать оператору
// про запрос», а не «как нарисовать карточку бэкенда».
//
// ПОЧЕМУ ФАБРИКА. Слой берёт из `renderers.js` семь имён (var ctx.* ниже); список
// посчитан `scripts/webui_extract_deps.js`, а не «на глаз» (R643/R644: зависимости
// из деструктуризации Utils и const-таблиц терялись при выносе).
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: тела перенесены как есть, отступ сохранён.
(function () {
    'use strict';

    window.RenderersLogs = function (ctx) {
        var Utils = ctx.Utils;
        var _t = ctx._t;
        var percent = ctx.percent;
        var emptyRow = ctx.emptyRow;
        var escapeAttr = ctx.escapeAttr;
        var escapeHtml = ctx.escapeHtml;
        var protocolBadge = ctx.protocolBadge;

    // ---- Queue (R91, 2026-10-08) ----
    //
    // ЧТО БЫЛО НЕ ТАК. Страница «Очередь» рисовала ЛЕГАСИ-очередь балансера
    // (/api/v1/queue/stats: current_size/processed_total/avg_wait_time_ms и
    // /api/v1/queue/details: all/pending/processing). В этом развёртывании она
    // ВСЕГДА пуста: реальное ожидание свободного слота живёт в admission-очереди
    // (LB_ADMISSION_WAIT_SEC). Живой замер со стенда: queue/details → {"all":[],
    // "total":0}, а queue/stats → admission{enabled:true, served_total:42,
    // waited_total:42, avg_wait_ms:13926}. То есть 42 запроса прождали в среднем
    // 13.9 с, а оператор видел «Очередь пуста», «Выполнено 0» и «Среднее
    // ожидание —» — и справедливо спрашивал, работает ли очередь вообще.
    //
    // queueView — ЧИСТАЯ функция: превращает payload /api/v1/queue/stats в модель
    // для отрисовки. Вынесена отдельно, чтобы её проверял юнит-тест
    // (renderers-queue.test.js), а не DOM.
    function queueView(queue) {
        queue = (queue && typeof queue === 'object') ? queue : {};
        const adm = (queue.admission && typeof queue.admission === 'object') ? queue.admission : {};
        // Включённая admission-очередь = реальная очередь. Легаси-счётчики в этом
        // режиме нули не потому, что «нет очереди», а потому что через них уже
        // никто не ходит.
        const admissionMode = !!adm.enabled;
        const legacy = {
            waiting: firstNumOr(queue.current_size, 0),
            max: firstNumOr(queue.max_size, 100),
            processed: firstNumOr(queue.processed_total, 0),
            workers: firstNumOr(queue.workers, 0),
            avgWaitMs: firstNumOr(queue.avg_wait_time_ms, 0)
        };
        const detail = Array.isArray(adm.waiting_detail) ? adm.waiting_detail.map(function(w, index) {
            const session = String((w && w.session) || '');
            // R118 (2026-10-14): позиция приходит с балансера (position). Индекс
            // строки — только запасной вариант для старого балансера без поля.
            const position = firstNumOr(w && w.position, index + 1);
            // R118: различает ли балансер этого клиента. `fp:` — отпечаток
            // IP+User-Agent, `tkn:` — общий Authorization-токен: у двух экземпляров
            // клиента с одной машины они совпадают, и очередь физически не может их
            // разделить. Оператор должен видеть это прямо, а не две одинаковые
            // строки, выглядящие как разные клиенты.
            const sessionKnown = (w && typeof w.session_known === 'boolean')
                ? w.session_known
                : !/^(fp:|tkn:)/.test(session);
            return {
                session: session,
                backend: String((w && w.backend) || ''),
                waitedMs: firstNumOr(w && w.waited_ms, 0),
                position: position,
                sessionKnown: sessionKnown,
                sessionSource: String((w && w.session_source) || ''),
                // R229: протокол ожидающего (openai/ollama), если сессия известна.
                protocol: String((w && w.protocol) || '')
            };
        }) : [];
        // Карта «по бэкендам» — из waiting_by_backend, но при её отсутствии
        // считаем по поимённому списку: два источника одного факта не должны
        // расходиться (иначе «счётчик 2, список пуст»).
        let byBackend = [];
        const rawBy = (adm.waiting_by_backend && typeof adm.waiting_by_backend === 'object') ? adm.waiting_by_backend : null;
        if (rawBy) {
            byBackend = Object.keys(rawBy).map(function(id) {
                return { backend: id, count: firstNumOr(rawBy[id], 0) };
            });
        } else if (detail.length) {
            const acc = {};
            detail.forEach(function(w) { acc[w.backend] = (acc[w.backend] || 0) + 1; });
            byBackend = Object.keys(acc).map(function(id) { return { backend: id, count: acc[id] }; });
        }
        byBackend.sort(function(a, b) { return b.count - a.count || (a.backend < b.backend ? -1 : 1); });

        return {
            mode: admissionMode ? 'admission' : 'legacy',
            waiting: admissionMode ? firstNumOr(adm.waiting, 0) : legacy.waiting,
            processed: admissionMode ? firstNumOr(adm.served_total, 0) : legacy.processed,
            avgWaitMs: admissionMode ? firstNumOr(adm.avg_wait_ms, 0) : legacy.avgWaitMs,
            // «Воркеры» у admission-режима — это не число воркеров, а число
            // сессий с активным запросом; подменять смысл молча нельзя, поэтому
            // значение кладём отдельным полем и подписываем по-другому.
            activeSessions: firstNumOr(adm.active_sessions, 0),
            workers: legacy.workers,
            // Предел ожидания слота (секунды) — в admission-режиме он и есть
            // «максимум»: длина очереди не ограничена, ограничено ожидание.
            waitMaxSec: firstNumOr(adm.wait_max_sec, 0),
            timeouts: firstNumOr(adm.timeout_total, 0),
            waitedTotal: firstNumOr(adm.waited_total, 0),
            // R118: сколько ожидающих балансер не может различить между собой
            // (ключ выведен из IP+User-Agent или общего токена).
            unidentified: firstNumOr(adm.unidentified_waiting, 0),
            legacyMax: legacy.max,
            byBackend: byBackend,
            detail: detail
        };
    }

    function firstNumOr(v, fallback) {
        const n = Number(v);
        return isFinite(n) ? n : fallback;
    }

    function queuePage(queue, queueTasks, queueHistory) {
        const v = queueView(queue);

        Utils.setText('queueCurrentSize', v.waiting);
        Utils.setText('queueProcessed', v.processed);
        Utils.setText('queueWorkers', v.mode === 'admission' ? v.activeSessions : v.workers);
        Utils.setText('queueTimeouts', v.timeouts);
        Utils.setText('queueAvgWait', v.avgWaitMs > 0
            ? _t('renderers.queue_avg_wait', { seconds: (v.avgWaitMs / 1000).toFixed(1) })
            : '-');

        // Подписи карточек зависят от режима: в admission-режиме те же плитки
        // означают другое, и оставлять «Максимум 100» рядом с очередью, у которой
        // нет предела по длине, — вводить в заблуждение.
        if (v.mode === 'admission') {
            Utils.setText('queueMaxSize', v.waitMaxSec);
            setLabel('queueLabelPosition', 'queue.admission_waiting');
            setLabel('queueLabelMax', 'queue.admission_wait_max');
            setLabel('queueLabelProcessed', 'queue.admission_served');
            setLabel('queueLabelWorkers', 'queue.admission_active');
            setLabel('queueLabelAvgWait', 'queue.estimated_wait');
        } else {
            Utils.setText('queueMaxSize', v.legacyMax);
            setLabel('queueLabelPosition', 'queue.position');
            setLabel('queueLabelMax', 'queue.max_size');
            setLabel('queueLabelProcessed', 'queue.completed');
            setLabel('queueLabelWorkers', 'queue.workers');
            setLabel('queueLabelAvgWait', 'queue.estimated_wait');
        }

        // Визуализация: в admission-режиме — СКОЛЬКО ЖДЁТ НА КАКОМ БЭКЕНДЕ.
        // Легаси-полоса «заполнение 0..100» показывала 0 при очереди из 5
        // запросов и не говорила ничего.
        Utils.setHTML('queueVisualization', queueVisualization(v));

        const badgeEl = document.getElementById('queueTasksCount');
        if (badgeEl) badgeEl.textContent = v.mode === 'admission' ? v.detail.length : v.waiting;

        Utils.setHTML('queueTasksBody', v.mode === 'admission'
            ? queueAdmissionRows(v)
            : queueTasksBody(queueTasks));
        // Заголовок «Модель» в admission-режиме не подходит: ожидающий запрос
        // идентифицируется сессией клиента (модель в очередь не передаётся).
        setLabel('queueColModel', v.mode === 'admission' ? 'queue.col_client' : 'sessions.model');
        Utils.setHTML('queueHistoryBody', queueHistoryBody(queueHistory));
    }

    function setLabel(id, key) {
        const el = document.getElementById(id);
        if (el && window.I18N) el.textContent = _t(key);
    }

    // queueVisualization — состояние очереди «по-человечески».
    function queueVisualization(v) {
        if (v.mode !== 'admission') {
            const pct = percent(v.waiting, v.legacyMax);
            return '<div class="queue-bar"><div class="queue-fill" id="queueFill" style="width: ' + pct + '%"></div></div>' +
                '<div class="queue-labels"><span>0</span><span>' + Math.round(v.legacyMax / 2) + '</span><span>' + v.legacyMax + '</span></div>';
        }
        if (!v.byBackend.length) {
            return '<div class="queue-adm-empty">' + escapeHtml(_t('queue.admission_idle')) + '</div>' +
                queueAdmissionFootnote(v);
        }
        const maxCount = v.byBackend.reduce(function(m, b) { return Math.max(m, b.count); }, 1);
        const rows = v.byBackend.map(function(b) {
            const w = Math.round(percent(b.count, maxCount));
            return '<div class="queue-adm-row">' +
                '<span class="queue-adm-name">' + escapeHtml(b.backend) + '</span>' +
                '<span class="queue-adm-bar"><span class="queue-fill" style="width: ' + w + '%"></span></span>' +
                '<span class="queue-adm-count">' + b.count + '</span>' +
            '</div>';
        }).join('');
        return '<div class="queue-adm-list">' + rows + '</div>' + queueAdmissionFootnote(v);
    }

    function queueAdmissionFootnote(v) {
        // Явно называем режим: оператор должен понимать, что это ожидание СЛОТА,
        // а не легаси-очередь с её нулями.
        return '<div class="queue-adm-note">' + escapeHtml(_t('queue.admission_note', {
            served: v.processed,
            timeouts: v.timeouts,
            seconds: v.waitMaxSec
        })) + '</div>';
    }

    // queueAdmissionRows — таблица ожидающих слотов.
    function queueAdmissionRows(v) {
        if (!v.detail.length) return emptyRow(5, _t('queue.no_tasks'));
        return v.detail.map(function(w, index) {
            const waited = (w.waitedMs / 1000).toFixed(1) + _t('renderers.seconds');
            // R118: позиция — из очереди (не индекс отрисовки), а для клиента,
            // которого балансер не различает, показываем честную подпись вместо
            // синтетического ключа (сам ключ остаётся в title для разбора).
            const pos = (w.position && w.position > 0) ? w.position : (index + 1);
            const sess = w.sessionKnown
                ? '<code>' + escapeHtml(w.session || '-') + '</code>'
                : '<code title="' + escapeHtml(w.session || '') + '">' +
                    escapeHtml(_t('queue.session_unidentified')) + '</code>';
            return '<tr>' +
                '<td>' + pos + '</td>' +
                '<td>' + sess + protocolBadge(w.protocol) + '</td>' +
                '<td>' + escapeHtml(w.backend || '-') + '</td>' +
                '<td>' + waited + '</td>' +
                '<td><span class="queue-task-status pending">' + escapeHtml(_t('queue.admission_status')) + '</span></td>' +
            '</tr>';
        }).join('');
    }

    function queueTasksBody(tasks) {
        if (!tasks.length) return emptyRow(5, _t('queue.no_tasks'));
        return tasks.map((task, index) => {
            const status = task.status || 'pending';
            const statusClass = status === 'processing' ? 'processing' : (status === 'completed' ? 'completed' : 'pending');
            const statusText = status === 'processing' ? _t('queue.processing') : (status === 'completed' ? _t('queue.completed') : _t('queue.pending'));
            const waitTime = task.waitTimeMs ? (task.waitTimeMs / 1000).toFixed(1) + _t('renderers.seconds') : '-';
            return `
                <tr>
                    <td>${index + 1}</td>
                    <td><code>${escapeHtml(task.model || '-')}</code></td>
                    <td>${escapeHtml(task.backend || 'Auto')}</td>
                    <td>${waitTime}</td>
                    <td><span class="queue-task-status ${statusClass}">${statusText}</span></td>
                </tr>
            `;
        }).join('');
    }

    function queueHistoryBody(history) {
        const badge = document.getElementById('queueHistoryCount');
        if (badge) badge.textContent = history.length;
        if (!history.length) return emptyRow(6, _t('renderers.no_completed_tasks'));

        return history.map((task, index) => {
            const enqueued = task.enqueued ? new Date(task.enqueued).toLocaleString(Utils._locale(), { hour: '2-digit', minute: '2-digit', second: '2-digit' }) : '-';
            const completed = task.completed_at ? new Date(task.completed_at).toLocaleString(Utils._locale(), { hour: '2-digit', minute: '2-digit', second: '2-digit' }) : '-';
            const waitMs = task.wait_time_ms || 0;
            const waitStr = waitMs > 0 ? (waitMs / 1000).toFixed(1) + _t('renderers.seconds') : '-';
            return `
                <tr>
                    <td>${index + 1}</td>
                    <td><code>${escapeHtml(task.model || '-')}</code></td>
                    <td>${escapeHtml(task.target || 'Auto')}</td>
                    <td>${enqueued}</td>
                    <td>${completed}</td>
                    <td>${waitStr}</td>
                </tr>
            `;
        }).join('');
    }

    // ---- Logs ----

    // logFieldPairsR481 — поля события детерминированным списком [key, value].
    //
    // R481. Раньше поля склеивались JSON-суффиксом прямо в сообщение, и строка
    // лога выглядела как «текст + нечитаемый JSON»: прочитать ошибку и найти в
    // ней backend/request_id было нельзя. Теперь сообщение — только текст, а
    // поля рисуются отдельным раскрываемым блоком.
    function logFieldPairsR481(fields) {
        if (!fields || typeof fields !== 'object') return [];
        return Object.keys(fields).sort().map(function (k) {
            var v = fields[k];
            var text = (v === null || v === undefined) ? '' : (typeof v === 'object' ? JSON.stringify(v) : String(v));
            return [k, text];
        });
    }

    function logFieldsSearchTextR481(fields) {
        var pairs = logFieldPairsR481(fields);
        return pairs.map(function (p) { return p[0] + '=' + p[1]; }).join(' ');
    }

    function logs(logEntries) {
        const container = document.getElementById('logsContainer');
        if (!container) return;
        const all = Array.isArray(logEntries) ? logEntries : [];
        const levelFilter = (window.logFilters && window.logFilters.level) || 'all';
        const search = String((window.logFilters && window.logFilters.search) || '').trim().toLowerCase();
        const filtered = all.filter(function (log) {
            if (!log) return false;
            if (levelFilter !== 'all' && String(log.level || '').toLowerCase() !== levelFilter) return false;
            if (!search) return true;
            const haystack = [
                log.message || '',
                log.source || '',
                logFieldsSearchTextR481(log.fields)
            ].join(' ').toLowerCase();
            return haystack.indexOf(search) !== -1;
        });

        const countEl = document.getElementById('logsCount');
        if (countEl) {
            countEl.textContent = all.length
                ? _t('logs.count', { shown: filtered.length, total: all.length })
                : '';
        }
        if (!filtered.length) {
            container.innerHTML = `<div class="logs-empty">${escapeHtml(_t('logs.no_logs'))}</div>`;
            return;
        }
        container.innerHTML = filtered.slice(0, 1000).map(function (log) {
            const level = String(log.level || 'info').toLowerCase();
            const fields = logFieldPairsR481(log.fields);
            const msg = String(log.message || '');
            const long = msg.length > 220 || msg.indexOf('\n') !== -1 || fields.length > 0;
            const fieldsHtml = fields.length
                ? `<details class="log-fields"><summary>${escapeHtml(_t('logs.fields'))} (${fields.length})</summary>` +
                  `<div class="log-fields-grid">` +
                  fields.map(function (p) {
                      return `<div class="log-field-key">${escapeHtml(p[0])}</div><div class="log-field-val">${escapeHtml(p[1])}</div>`;
                  }).join('') +
                  `</div></details>`
                : '';
            return `<div class="log-entry log-${escapeAttr(level)}${long ? ' log-entry-long' : ''}">` +
                `<span class="log-time">${escapeHtml(log.time || '')}</span>` +
                `<span class="log-level ${escapeAttr(level)}">${escapeHtml(String(log.level || 'INFO'))}</span>` +
                (log.source ? `<span class="log-source">${escapeHtml(String(log.source))}</span>` : '') +
                `<div class="log-body">` +
                    `<div class="log-message" title="${escapeAttr(msg)}">${escapeHtml(msg)}</div>` +
                    (long ? `<button type="button" class="btn-link log-expand-btn">${escapeHtml(_t('logs.expand'))}</button>` : '') +
                    fieldsHtml +
                `</div>` +
            `</div>`;
        }).join('');

        // Клик по кнопке «Развернуть» раскрывает сообщение целиком (CSS-клэмп
        // снимается). Делегирование — один обработчик на контейнер.
        if (!container.dataset.logExpandBound && container.addEventListener) {
            container.dataset.logExpandBound = '1';
            container.addEventListener('click', function (ev) {
                const btn = ev.target && ev.target.closest ? ev.target.closest('.log-expand-btn') : null;
                if (!btn) return;
                const body = btn.closest('.log-body');
                const msgEl = body ? body.querySelector('.log-message') : null;
                if (!msgEl) return;
                const expanded = msgEl.classList.toggle('log-message-expanded');
                btn.textContent = expanded ? _t('logs.collapse') : _t('logs.expand');
            });
        }
        container.scrollTop = 0;
    }

    // ---- Prediction Alerts ----

    function predictionAlerts(backends) {
        const alerts = [];
        backends.forEach(b => {
            const pred = b.prediction || {};
            const seconds = pred.secondsToCritical;
            if (seconds > 0 && seconds < 300) {
                alerts.push({
                    backend: b.id,
                    reason: pred.criticalReason || 'unknown',
                    seconds: Math.round(seconds),
                    level: seconds < 120 ? 'danger' : 'warning'
                });
            }
        });

        const container = document.getElementById('alertsList');
        if (!container) return;
        if (!alerts.length) {
            container.innerHTML = `<div class="alert alert-info">${_t('renderers.no_active_alerts')}</div>`;
            return;
        }
        container.innerHTML = alerts.map(a => `
            <div class="alert alert-${a.level}">
                <svg class="alert-icon-svg" viewBox="0 0 24 24"><circle cx="12" cy="12" r="10" fill="currentColor"/></svg>
                <span><strong>${escapeHtml(a.backend)}</strong>: ${escapeHtml(a.reason)} ${_t('common.in')} ${a.seconds}${_t('renderers.seconds')}</span>
            </div>
        `).join('');
    }

    // ---- Proxy Logs ----

    function copyProxyLogs() {
        const container = document.getElementById('proxyLogsContainer');
        const table = container && container.querySelector('table.data-table');
        if (!table) return;
        const rows = table.querySelectorAll('tr');
        let csv = '';
        rows.forEach(function(row) {
            const cells = row.querySelectorAll('th, td');
            const rowData = Array.from(cells).map(function(cell) {
                return '"' + cell.textContent.trim().replace(/"/g, '""') + '"';
            }).join('\t');
            csv += rowData + '\n';
        });
        navigator.clipboard.writeText(csv).then(function() {
            const btn = document.getElementById('copyProxyLogs');
            if (btn) {
                const orig = btn.textContent;
                btn.textContent = '✓ ' + (window.I18N ? I18N.t('common.success') : 'Copied');
                setTimeout(function() { btn.textContent = orig; }, 2000);
            }
        }).catch(function() {});
    }

    // proxyLogs — R481: читаемая таблица с локалью, раскрываемыми деталями и
    // request_id/stage.
    //
    // Живые жалобы, которые здесь закрыты:
    //   1. «таблицы не дают рассмотреть ошибку полностью»: ячейка ошибки была
    //      обрезана до 120 символов, а обещанный клик-разворот (R389) не был
    //      реализован. Теперь клик по строке открывает блок деталей с ПОЛНЫМ
    //      текстом ошибки, путём, request_id и стадией.
    //   2. «локализация»: «All / 2xx OK / 4xx Client / 5xx Server / Error /
    //      No entries match filter» были захардкожены по-английски.
    //   3. ошибка и путь были доступны только через title (наведение мышью) —
    //      их нельзя скопировать; в деталях есть кнопки «Копировать».
    function proxyLogs(entries) {
        const container = document.getElementById('proxyLogsContainer');
        if (!container) return;
        if (!entries.length) {
            container.innerHTML = `<div class="proxy-log-placeholder">${escapeHtml(_t('logs.proxy_waiting'))}</div>`;
            return;
        }
        var levelFilter = (window.proxyLogFilters && window.proxyLogFilters.level) || 'all';
        var searchQuery = ((window.proxyLogFilters && window.proxyLogFilters.search) || '').toLowerCase();
        var filtered = entries.filter(function(e) {
            if (levelFilter !== 'all') {
                var sc = e.statusCode || 0;
                if (levelFilter === '2xx' && (sc < 200 || sc >= 300)) return false;
                if (levelFilter === '4xx' && (sc < 400 || sc >= 500)) return false;
                if (levelFilter === '5xx' && sc < 500) return false;
            }
            if (searchQuery) {
                var haystack = ((e.path || '') + ' ' + (e.model || '') + ' ' +
                    (e.backendID || '') + ' ' + (e.error || '') + ' ' + (e.clientName || '') + ' ' +
                    (e.requestID || '') + ' ' + (e.stage || '')).toLowerCase();
                if (haystack.indexOf(searchQuery) === -1) return false;
            }
            return true;
        });
        if (!filtered.length) {
            container.innerHTML = `<div class="proxy-log-placeholder">${escapeHtml(_t('logs.proxy_no_match'))}</div>`;
            return;
        }
        const locale = (window.I18N && I18N.getLang() === 'ru') ? 'ru' : 'en';
        var html = '<div class="proxy-logs-toolbar">' +
            '<select class="proxy-logs-level-filter">' +
            '<option value="all"' + (levelFilter === 'all' ? ' selected' : '') + '>' + escapeHtml(_t('logs.proxy_all')) + '</option>' +
            '<option value="2xx"' + (levelFilter === '2xx' ? ' selected' : '') + '>' + escapeHtml(_t('logs.proxy_status_2xx')) + '</option>' +
            '<option value="4xx"' + (levelFilter === '4xx' ? ' selected' : '') + '>' + escapeHtml(_t('logs.proxy_status_4xx')) + '</option>' +
            '<option value="5xx"' + (levelFilter === '5xx' ? ' selected' : '') + '>' + escapeHtml(_t('logs.proxy_status_5xx')) + '</option>' +
            '</select>' +
            '<input type="text" class="proxy-logs-search" placeholder="' + escapeAttr(_t('common.search')) + '" value="' + escapeAttr(searchQuery) + '">' +
            '<span class="proxy-logs-count">' + filtered.length + '/' + entries.length + '</span>' +
            '</div>' +
            '<table class="data-table proxy-logs-table"><thead><tr>' +
            '<th>' + _t('logs.time') + '</th>' +
            '<th>' + _t('logs.method') + '</th>' +
            '<th>' + _t('logs.path') + '</th>' +
            '<th>' + _t('logs.model') + '</th>' +
            '<th>' + _t('logs.client') + '</th>' +
            '<th>' + _t('logs.status') + '</th>' +
            '<th>' + _t('logs.duration') + '</th>' +
            '<th>' + _t('logs.backend') + '</th>' +
            '<th>' + _t('logs.proxy_stage') + '</th>' +
            '<th>' + _t('logs.proxy_error') + '</th>' +
            '</tr></thead><tbody>';
        filtered.slice(0, 500).forEach(function(e, idx) {
            var time = e._time || (e.timestamp ? new Date(e.timestamp).toLocaleTimeString(locale) : '-');
            var method = e.method || 'GET';
            var path = e.path || '-';
            var model = e.model || '-';
            var client = e.clientName || e.clientIP || '-';
            var status = e.statusCode ? String(e.statusCode) : '-';
            var duration = e.durationMs ? e.durationMs + 'ms' : '-';
            var backend = e.backendID || '-';
            var stage = e.stage || '-';
            var requestID = e.requestID || '';
            var errorFull = e.error ? String(e.error) : '';
            var errorText = errorFull.length > 90 ? errorFull.substring(0, 90) + '…' : errorFull;
            var statusClass = 'log-status-ok';
            if (status !== '-' && parseInt(status) >= 400) statusClass = 'log-status-err';
            var hasDetails = !!(errorFull || requestID || stage !== '-' || path.length > 60);
            html += '<tr class="proxy-log-row' + (hasDetails ? ' has-details' : '') + '" data-idx="' + idx + '">' +
                '<td class="log-time-cell">' + escapeHtml(time) + '</td>' +
                '<td><span class="log-method-badge log-method-' + escapeAttr(method.toLowerCase()) + '">' + escapeHtml(method) + '</span></td>' +
                '<td class="log-path-cell">' + escapeHtml(path.length > 60 ? path.substring(0, 60) + '…' : path) + '</td>' +
                '<td>' + escapeHtml(model) + '</td>' +
                '<td>' + escapeHtml(client) + '</td>' +
                '<td><span class="' + statusClass + '">' + escapeHtml(status) + '</span></td>' +
                '<td>' + escapeHtml(duration) + '</td>' +
                '<td>' + escapeHtml(backend) + '</td>' +
                '<td class="log-stage-cell">' + escapeHtml(stage) + '</td>' +
                '<td class="log-error-cell">' + (errorText ? escapeHtml(errorText) : '-') + '</td>' +
                '</tr>';
            if (hasDetails) {
                html += '<tr class="proxy-log-detail" hidden><td colspan="10">' +
                    '<div class="proxy-log-detail-body">' +
                    '<div class="proxy-log-detail-row"><span class="proxy-log-detail-key">' + escapeHtml(_t('logs.proxy_request_id')) + '</span>' +
                        '<code>' + escapeHtml(requestID || '-') + '</code>' +
                        (requestID ? '<button type="button" class="btn-link proxy-copy-btn" data-copy="' + escapeAttr(requestID) + '">' + escapeHtml(_t('logs.copy_message')) + '</button>' : '') + '</div>' +
                    '<div class="proxy-log-detail-row"><span class="proxy-log-detail-key">' + escapeHtml(_t('logs.path')) + '</span>' +
                        '<code>' + escapeHtml(path) + '</code>' +
                        '<button type="button" class="btn-link proxy-copy-btn" data-copy="' + escapeAttr(path) + '">' + escapeHtml(_t('logs.proxy_copy_path')) + '</button></div>' +
                    '<div class="proxy-log-detail-row"><span class="proxy-log-detail-key">' + escapeHtml(_t('logs.proxy_stage')) + '</span>' +
                        '<code>' + escapeHtml(stage) + '</code></div>' +
                    '<div class="proxy-log-detail-row proxy-log-detail-error"><span class="proxy-log-detail-key">' + escapeHtml(_t('logs.proxy_error')) + '</span>' +
                        '<pre class="proxy-log-error-full">' + escapeHtml(errorFull || '-') + '</pre>' +
                        (errorFull ? '<button type="button" class="btn-link proxy-copy-btn" data-copy="' + escapeAttr(errorFull) + '">' + escapeHtml(_t('logs.proxy_copy_error')) + '</button>' : '') + '</div>' +
                    '</div>' +
                '</td></tr>';
            }
        });
        html += '</tbody></table>';
        container.innerHTML = html;

        var sel = container.querySelector('.proxy-logs-level-filter');
        var inp = container.querySelector('.proxy-logs-search');
        if (sel) sel.onchange = function() {
            window.proxyLogFilters = window.proxyLogFilters || {};
            window.proxyLogFilters.level = sel.value;
            proxyLogs(entries);
        };
        if (inp) inp.oninput = function() {
            window.proxyLogFilters = window.proxyLogFilters || {};
            window.proxyLogFilters.search = inp.value;
            proxyLogs(entries);
        };
        // Раскрытие деталей и копирование — делегированием, один раз на контейнер.
        if (!container.dataset.proxyDetailsBound && container.addEventListener) {
            container.dataset.proxyDetailsBound = '1';
            container.addEventListener('click', function(ev) {
                var copyBtn = ev.target && ev.target.closest ? ev.target.closest('.proxy-copy-btn') : null;
                if (copyBtn) {
                    var text = copyBtn.getAttribute('data-copy') || '';
                    var done = function() {
                        var prev = copyBtn.textContent;
                        copyBtn.textContent = _t('logs.copied');
                        setTimeout(function() { copyBtn.textContent = prev; }, 1500);
                    };
                    if (navigator.clipboard && navigator.clipboard.writeText) {
                        navigator.clipboard.writeText(text).then(done).catch(function() {});
                    }
                    return;
                }
                var row = ev.target && ev.target.closest ? ev.target.closest('tr.proxy-log-row.has-details') : null;
                if (!row) return;
                var detail = row.nextElementSibling;
                if (detail && detail.classList && detail.classList.contains('proxy-log-detail')) {
                    detail.hidden = !detail.hidden;
                }
            });
        }
    }

        return {
            queueView: queueView,
            queuePage: queuePage,
            logs: logs,
            predictionAlerts: predictionAlerts,
            proxyLogs: proxyLogs,
            copyProxyLogs: copyProxyLogs
        };
    };
})();
