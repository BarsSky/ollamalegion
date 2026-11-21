// app-models.js — R654 (2026-11-20): страницы моделей, агентов и лимитов
// бэкендов плюс карточка деталей модели вынесены из `app.js` (цель §5 — < 1500).
//
// ЗАЧЕМ ВЫНЕСЕНО. Одна тема — действия с моделями и узлами: Load/Unload/Delete,
// прогресс операций с авто-обновлением (R346), лимиты бэкенда, включение/
// выключение узла (R250), рестарт агента и логи агента, донорские ограничения
// (R304) и модалка деталей модели. Оркестратор (init, роутинг, слушатели) — другое.
//
// СОСТОЯНИЕ ОПЕРАЦИЙ ЖИВЁТ ЗДЕСЬ. `_cancelledOps`, `_opsAutoRefreshTimer`,
// `OPS_AUTO_REFRESH_MS`, `OP_HEURISTIC_DURATION_MS` принадлежали только этой
// группе (проверено: вне её обращений нет) — переехали вместе с ней.
//
// ПОЧЕМУ ФАБРИКА. Слой берёт из `app.js` состояние (`data`), Api/Utils/I18N,
// addLog/showToast, filterBackendsForUI, backendsPage/renderLogs/renderProxyLogs
// и ДОСТУП К ТЕКУЩЕЙ СТРАНИЦЕ (геттером: currentPage — это let).
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: тела перенесены как есть, отступ сохранён.
(function () {
    'use strict';

    window.AppModels = function (ctx) {
        var Api = ctx.Api;
        var I18N = ctx.I18N;
        var Utils = ctx.Utils;
        var addLog = ctx.addLog;
        var backendsPage = ctx.backendsPage;
        var currentPage = ctx.currentPage;
        var data = ctx.data;
        var filterBackendsForUI = ctx.filterBackendsForUI;
        var renderLogs = ctx.renderLogs;
        var renderProxyLogs = ctx.renderProxyLogs;
        var showToast = ctx.showToast;
        // R675 (2026-11-21), найдено scripts/webui_ctx_contract_check.js.
        //
        // ЖИВОЙ ДЕФЕКТ ТОГО ЖЕ КЛАССА, ЧТО R671/R672. R673 прокинул имя в ctx со
        // стороны вызывающего (app.js:955 — ленивая обёртка), но в САМОЙ фабрике
        // строку привязки забыли. Тело зовёт `fetchClusterState()` в двух местах —
        // после переключения «не использовать бэкенд» (:77) и после сохранения
        // лимитов бэкенда (:175) — то есть браузер получал ReferenceError ровно
        // после успешного ответа API: тост «saved» уже показан, а панель не
        // обновлена, и оператор видит устаревшие данные.
        var fetchClusterState = ctx.fetchClusterState;

    function restartAgent(backendId) {
        if (!confirm((window.I18N ? I18N.t('agents.confirm_restart') : 'Restart agent on backend') + ' ' + backendId + '?')) return;
        showToast((window.I18N ? I18N.t('agents.restarting') : 'Restarting agent...'), 'info');
        Api.restartAgent(backendId).then(function(result) {
            showToast((window.I18N ? I18N.t('agents.restarted') : 'Agent restarted'), 'success');
            addLog('Agent restarted on ' + backendId, 'info');
        }).catch(function(err) {
            showToast((window.I18N ? I18N.t('agents.restart_error') : 'Restart error') + ': ' + (err.message || err), 'error');
        });
    }

    // R250 (2026-10-28): «не использовать бэкенд» из списка бэкендов.
    //
    // ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: чтобы исключить узел из работы, приходилось
    // останавливать контейнер — узел пропадал из панели (а его надо настраивать), а
    // возврат требовал `docker start` и нового цикла регистрации. Здесь флаг
    // ставится на ЖИВОМ узле: контейнер работает, текущие запросы доигрываются,
    // новые не выбираются балансером.
    //
    // Панель обновляем ПОСЛЕ ответа сервера (перечитываем состав кластера), а не
    // оптимистично: иначе при отказе бейдж «исключён» показывал бы неправду.
    function toggleBackendEnabled(backendId, disabledNow) {
        var willEnable = !!disabledNow; // кнопка в строке выключенного узла — «Включить»
        Api.setBackendEnabled(backendId, willEnable).then(function(resp) {
            var isOff = !!(resp && resp.enabled === false);
            var loaded = (resp && resp.loadedModels) || [];
            var text = backendId + ': ' + (window.I18N
                ? I18N.t(isOff ? 'backends.excluded' : 'backends.use_on')
                : (isOff ? 'excluded' : 'enabled'));
            // Модель на исключённом узле остаётся загруженной: выгрузка — отдельное
            // действие (страница моделей), и молчать об этом нельзя — «выключил, а
            // GPU занята» выглядит поломкой.
            if (isOff && loaded.length) {
                text += ' · ' + (window.I18N ? I18N.t('merge.useStillLoaded') : 'model stays loaded') +
                    ': ' + loaded.join(', ');
            }
            showToast(text, isOff ? 'info' : 'success');
            addLog('backend ' + backendId + (isOff
                ? ' excluded from balancer selection'
                : ' returned to balancer selection'), 'info');
            if (resp && resp.persisted === false && resp.warning) {
                showToast(String(resp.warning), 'warning');
            }
            return fetchClusterState();
        }).then(function() {
            if (currentPage === 'backends') {
                backendsPage(filterBackendsForUI(data.backends));
            }
        }).catch(function(err) {
            showToast((window.I18N ? I18N.t('merge.useFailed') : 'Failed to toggle usage') +
                ': ' + ((err && err.message) || err), 'error');
        });
    }

    function viewAgentLogs(backendId) {
        showToast((window.I18N ? I18N.t('agents.loading_logs') : 'Loading logs...'), 'info');
        Api.agentLogs(backendId, 200).then(function(data) {
            var logs = data.logs || data.entries || [];
            var content = logs.length ? logs.map(function(l) {
                var time = l.time || l.timestamp || '';
                var level = l.level || 'INFO';
                var msg = l.message || l.msg || '';
                var levelClass = level.toLowerCase();
                return '<div class="log-entry ' + levelClass + '"><span class="log-time">' + Utils.escapeHtml(time) + '</span> <span class="log-level log-level-' + levelClass + '">' + Utils.escapeHtml(level) + '</span> ' + Utils.escapeHtml(msg) + '</div>';
            }).join('') : '<div style="text-align:center;color:var(--text-muted);padding:20px;">' + (window.I18N ? I18N.t('agents.no_logs') : 'No logs available') + '</div>';

            var modalHtml = '<div id="agentLogsModal" class="modal active" style="z-index:2000;"><div class="modal-content" style="max-width:800px;">' +
                '<div class="modal-header"><h3>' + (window.I18N ? I18N.t('agents.logs_title') : 'Agent Logs') + ' — ' + Utils.escapeHtml(backendId) + '</h3><button class="modal-close" onclick="document.getElementById(\'agentLogsModal\').remove()">×</button></div>' +
                '<div class="modal-body"><div class="agent-logs-content">' + content + '</div></div>' +
                '<div class="modal-footer"><button class="btn btn-secondary" onclick="document.getElementById(\'agentLogsModal\').remove()">' + (window.I18N ? I18N.t('common.close') : 'Закрыть') + '</button></div>' +
                '</div></div>';

            var existing = document.getElementById('agentLogsModal');
            if (existing) existing.remove();
            document.body.insertAdjacentHTML('beforeend', modalHtml);
        }).catch(function(err) {
            showToast((window.I18N ? I18N.t('agents.logs_error') : 'Failed to load logs') + ': ' + (err.message || err), 'error');
        });
    }

    function fetchProxyLogs() {
        Api.proxyLogs(100).then(function(res) {
            data.proxyLogs = res.entries || [];
            if (currentPage === 'logs') {
                renderLogs(data.logs);
                var proxyTab = document.getElementById('logsTabProxy');
                if (proxyTab && proxyTab.classList.contains('active')) {
                    renderProxyLogs(data.proxyLogs);
                }
            }
        }).catch(function(err) {
            console.error('Failed to fetch proxy logs:', err);
        });
    }

    function loadBackendLimits() {
        var tbody = document.getElementById('backendLimitsBody');
        if (!tbody) return;
        // R513: лимиты — это `<input type="number">` в таблице; пересборка
        // innerHTML уносит набранное значение вместе со строкой. Пока оператор
        // правит число в этой таблице, её не перерисовываем.
        if (window.RefreshGuard && typeof window.RefreshGuard.isEditingIn === 'function' &&
            window.RefreshGuard.isEditingIn(tbody)) {
            return;
        }
        var backends = data.backends || [];
        if (backends.length === 0) {
            tbody.innerHTML = '<tr><td colspan="6" class="loading-cell">' + (window.I18N ? I18N.t('common.loading') : 'Loading...') + '</td></tr>';
            return;
        }
        tbody.innerHTML = backends.map(function(b) {
            var maxConcurrent = b.maxConcurrentRequests || b.activeRequests || 10;
            var maxModels = b.maxModels || 0;
            var statusClass = b.status === 'healthy' ? 'badge-success' : (b.status === 'warning' ? 'badge-warning' : 'badge-error');
            var statusText = b.status || (window.I18N ? I18N.t('common.unknown') : 'Unknown');
            return '<tr>' +
                '<td><strong>' + Utils.escapeHtml(b.id || '-') + '</strong></td>' +
                '<td>' + Utils.escapeHtml(b.host || '-') + '</td>' +
                '<td>' + (b.agentPort || '-') + '</td>' +
                '<td><input type="number" class="form-control backend-limit-input" data-backend-id="' + Utils.escapeHtml(b.id) + '" data-limit-type="maxConcurrent" value="' + maxConcurrent + '" min="1" style="width:100px;"></td>' +
                '<td><input type="number" class="form-control backend-limit-input" data-backend-id="' + Utils.escapeHtml(b.id) + '" data-limit-type="maxModels" value="' + maxModels + '" min="0" style="width:100px;"></td>' +
                '<td><span class="badge ' + statusClass + '">' + statusText + '</span></td>' +
            '</tr>';
        }).join('');
    }

    function saveBackendLimits() {
        var inputs = document.querySelectorAll('.backend-limit-input');
        var limits = {};
        inputs.forEach(function(input) {
            var backendId = input.dataset.backendId;
            var limitType = input.dataset.limitType;
            if (!limits[backendId]) limits[backendId] = {};
            limits[backendId][limitType] = parseInt(input.value) || 0;
        });
        var promises = Object.keys(limits).map(function(backendId) {
            var l = limits[backendId];
            return Api.updateBackendLimitsFull(backendId, l.maxConcurrent || 10, l.maxModels || 0);
        });
        Promise.all(promises).then(function() {
            showToast(window.I18N ? I18N.t('settings.saved') : 'Backend limits saved', 'success');
            fetchClusterState();
        }).catch(function(err) {
            showToast((window.I18N ? I18N.t('common.error') : 'Error') + ': ' + (err.message || err), 'error');
        });
    }


    // ---- Model Management ----

    // donorMarkForBackend — R304 (2026-10-30): пометка донора для проверок в
    // модалке и в отказе операций.
    //
    // ЗАЧЕМ. Доктрина оператора: «если бэкенд становится донором, он уже не
    // может подымать свои копии моделей — он становится именно донором мастера
    // и может только делить память с мастером». UI обязан отказывать ДО
    // запроса: иначе кнопка «Загрузить» у донора уходила в балансер, тот
    // отказывал (или, что хуже, грузил копию в VRAM, которую донор отдал
    // мастеру), а оператор видел ошибку без объяснения роли.
    //
    // Источник — уже загруженный список бэкендов (App.context.data — тот же
    // объект, что data ниже), новый запрос для проверки не нужен. Разбор полей
    // (включая fallback для старого балансера) — Utils.donorMark, один на всю
    // панель.
    function donorMarkForBackend(backendId) {
        var own = Array.isArray(data.backends) ? data.backends : [];
        var ctx = (typeof App !== 'undefined' && App && App.context &&
            App.context.data && Array.isArray(App.context.data.backends)) ? App.context.data.backends : null;
        var list = (ctx && ctx.length) ? ctx : own;
        var backend = null;
        for (var i = 0; i < list.length; i++) {
            if (list[i] && list[i].id === backendId) { backend = list[i]; break; }
        }
        if (!backend && list !== own) {
            for (var j = 0; j < own.length; j++) {
                if (own[j] && own[j].id === backendId) { backend = own[j]; break; }
            }
        }
        if (!backend) return null;
        return (typeof Utils.donorMark === 'function') ? Utils.donorMark(backend, list) : null;
    }

    // donorLoadBlocked — донору нельзя поднимать свои копии (canLoadOwnModels).
    function donorLoadBlocked(backendId) {
        var dm = donorMarkForBackend(backendId);
        return !!(dm && !dm.canLoad);
    }

    // donorBlockedText — одна и та же причина на всех уровнях блокировки
    // (карточка модели, модалка, GGUF, отказ в действии).
    function donorBlockedText() {
        return window.I18N ? I18N.t('models.donor_load_blocked')
            : 'A donor does not raise its own model copies: its VRAM is lent to the master over rpc-server.';
    }

    function openModelManageModal(backendId) {
        const backend = data.backends.find(b => b.id === backendId);
        if (!backend) {
            showToast('Backend not found', 'error');
            return;
        }

        const modal = document.getElementById('modelManageModal');
        if (!modal) return;

        // Store backendId in dataset for pull button
        modal.dataset.backendId = backendId;

        // Set title
        const title = modal.querySelector('.modal-header h3');
        if (title) title.textContent = (window.I18N ? I18N.t('models.manage_title') : 'Model Management') + ' — ' + backendId;

        // B-08: Скрываем Pull Model секцию для llama_cpp бэкендов
        var bt = backend.backend_type || backend.BackendType || 'ollama';
        var pullSection = modal.querySelector('.model-manage-section');
        if (pullSection) {
            pullSection.style.display = (bt === 'llama_cpp') ? 'none' : '';
        }

        // R304 (2026-10-30): у донора Pull недоступен — он не поднимает свои
        // копии, его VRAM отдана мастеру через rpc-server. Строку ввода ПРЯЧЕМ
        // (не оставляем поле, которое гарантированно откажет), а на её месте
        // пишем причину: «почему нельзя» оператор должен прочитать здесь, а не
        // догадываться по тосту после нажатия.
        var pullRow = modal.querySelector('.model-pull-row');
        var pullDonorNote = document.getElementById('modelPullDonorNote');
        if (donorLoadBlocked(backendId)) {
            if (pullSection) pullSection.style.display = '';
            if (pullRow) pullRow.style.display = 'none';
            if (pullDonorNote) {
                pullDonorNote.style.display = '';
                pullDonorNote.textContent = donorBlockedText();
            }
        } else {
            if (pullRow) pullRow.style.display = '';
            if (pullDonorNote) pullDonorNote.style.display = 'none';
        }

        // Show operations list with loading
        const modelsBody = document.getElementById('modelManageModelsBody');
        if (modelsBody) {
            modelsBody.innerHTML = '<tr><td colspan="7" class="loading-cell">' + (window.I18N ? I18N.t('common.loading') : 'Loading...') + '</td></tr>';
        }

        // Show active operations (запускает auto-refresh при наличии активных op)
        refreshModelOpsStatus();

        modal.classList.add('active');

        // Load models from backend
        loadBackendModels(backendId);
    }

    function loadBackendModels(backendId) {
        const modelsBody = document.getElementById('modelManageModelsBody');
        if (!modelsBody) return;

        // R304 (2026-10-30): у донора «Загрузить» гаснет и объясняет причину, а
        // «Выгрузить» остаётся рабочей — снять нарушающую копию это законное
        // действие (сервер её не выгружает сам: решение за оператором).
        const _donorBlocked = donorLoadBlocked(backendId);
        const loadBtnAttrs = _donorBlocked
            ? ' disabled title="' + Utils.escapeHtml(donorBlockedText()) + '"'
            : ' title="' + (window.I18N ? I18N.t('models.load_hint') : 'Load') + '"';

        Api.backendModels(backendId).then(function(data) {
            const models = data.models || [];
            if (models.length === 0) {
                modelsBody.innerHTML = '<tr><td colspan="7" class="loading-cell">' + (window.I18N ? I18N.t('models.no_models_backend') : 'No models') + '</td></tr>';
                return;
            }
            modelsBody.innerHTML = models.map(function(m) {
                const sizeStr = m.size ? (m.size / 1024 / 1024 / 1024).toFixed(2) + ' GB' : '-';
                const modified = m.modifiedAt ? new Date(m.modifiedAt).toLocaleString(Utils._locale()) : '-';
                const loadedBadge = m.loaded
                    ? '<span class="badge badge-success">' + (window.I18N ? I18N.t('models.loaded_status') : 'Loaded') + '</span>'
                    : '<span class="badge badge-secondary">' + (window.I18N ? I18N.t('models.unloaded_status') : 'Not loaded') + '</span>';
                return '<tr>' +
                    '<td><strong>' + Utils.escapeHtml(m.name) + '</strong></td>' +
                    '<td>' + (m.digest ? m.digest.substring(0, 16) + '...' : '-') + '</td>' +
                    '<td>' + sizeStr + '</td>' +
                    '<td>' + modified + '</td>' +
                    '<td>' + loadedBadge + '</td>' +
                    '<td>' +
                        '<button class="action-btn edit" onclick="ui.executeModelOperation(\'' + Utils.escapeHtml(backendId) + '\', \'load\', \'' + Utils.escapeHtml(m.name) + '\')"' + loadBtnAttrs + '>' + (window.I18N ? I18N.t('models.load') : 'Load') + '</button>' +
                        '<button class="action-btn delete" onclick="ui.executeModelOperation(\'' + Utils.escapeHtml(backendId) + '\', \'unload\', \'' + Utils.escapeHtml(m.name) + '\')" title="' + (window.I18N ? I18N.t('models.unload_hint') : 'Unload') + '">' + (window.I18N ? I18N.t('models.unload') : 'Unload') + '</button>' +
                    '</td>' +
                '</tr>';
            }).join('');
        }).catch(function(err) {
            modelsBody.innerHTML = '<tr><td colspan="7" class="loading-cell">' + (window.I18N ? I18N.t('models.operation_error') : 'Error') + ': ' + (err.message || err) + '</td></tr>';
        });
    }

    function executeModelOperation(backendId, operation, modelName, options = {}) {
        // R304 (2026-10-30): донор своих копий не поднимает. Отказ ДО запроса:
        // load и pull у донора запрещены доктриной («донор только делит память с
        // мастером»), и отправлять их в балансер бессмысленно — сервер обязан
        // отказать, а оператор получил бы техническую ошибку вместо объяснения.
        // UI-блокировка (карточка/модалка/GGUF) — удобство; ОТКАЗ ЖИВЁТ ЗДЕСЬ,
        // потому что inline-onclick в разметке легко обойти, а это последний
        // уровень перед API.
        if (operation === 'load' || operation === 'pull') {
            if (donorLoadBlocked(backendId)) {
                showToast(donorBlockedText(), 'error');
                addLog('Model op refused (R304: donor holds only the master memory): ' + operation +
                    ' ' + modelName + ' on ' + backendId, 'warn');
                return;
            }
        }

        showToast((window.I18N ? I18N.t('models.operation_running') : 'Operation in progress...') + ' ' + operation + ' ' + modelName, 'info');

        Api.backendModelOperation(backendId, operation, modelName, options).then(function(result) {
            // R83/v60 (2026-10-01): модель ЗАНЯТА активной генерацией.
            //
            // ДЕФЕКТ (живая проверка на стенде). cppworker отказывает в выгрузке
            // занятой модели: 409 + {success:false, busy:true, retryWithForce:true}
            // (cmd/cppworker/handlers_model.go:942-965, балансер —
            // internal/balancer/model_management.go:1423-1433). GGUF-страница этот
            // ответ обрабатывает и повторяет операцию с force (gguf-renderer-actions.js:249-257),
            // а страница Models — нет: пользователь получал тупиковую ошибку
            // «Cannot unload: model is busy with active inference requests» и мог
            // выгрузить модель ТОЛЬКО после того, как генерация закончится сама.
            // Замер: unload во время генерации → 409 за 6 мс, генерация продолжается.
            //
            // Теперь предлагаем подтверждение и повторяем с force=true: cppworker
            // обрывает активные генерации этой модели (AbortWatcher → C atomic flag)
            // и выгружает её. Проверено: тот же запрос с force → 200, модель исчезает
            // из /api/models, хотя генерация шла.
            if (result && result.success === false && operation === 'unload' && !options.force &&
                (result.retryWithForce || result.busy || result.status === 409)) {
                var busyMsg = (window.I18N ? I18N.t('gguf.confirm_unload_force')
                    : 'Модель занята активными запросами. Прервать их и выгрузить?');
                if (!confirm(busyMsg)) {
                    addLog('Model op cancelled by user (model busy): unload ' + modelName + ' on ' + backendId, 'warn');
                    return;
                }
                executeModelOperation(backendId, operation, modelName, Object.assign({}, options, { force: true }));
                return;
            }
            if (result && result.success) {
                var successMsg = (window.I18N ? I18N.t('models.operation_success') : 'Success') + ': ' + operation + ' ' + modelName;
                if (options.force) {
                    // Явно сообщаем, что операция потребовала обрыва генераций.
                    successMsg += ' (' + (window.I18N ? I18N.t('gguf.generation_cancelled') : 'generations cancelled') + ')';
                }
                showToast(successMsg, 'success');
                addLog('Model op success: ' + operation + ' ' + modelName + ' on ' + backendId + (options.force ? ' (force)' : ''), 'info');
                // Reload models after a short delay
                setTimeout(function() {
                    loadBackendModels(backendId);
                    refreshModelOpsStatus();
                }, 1500);
            } else {
                var errMsg = (result && result.error) || (window.I18N ? I18N.t('models.operation_error') : 'Operation failed');
                showToast(errMsg, 'error');
                addLog('Model op error: ' + operation + ' ' + modelName + ' on ' + backendId + ': ' + errMsg, 'error');
            }
        }).catch(function(err) {
            showToast((window.I18N ? I18N.t('models.operation_error') : 'Error') + ': ' + (err.message || err), 'error');
            addLog('Model op error: ' + operation + ' ' + modelName + ' on ' + backendId + ': ' + (err.message || err), 'error');
        });
    }

    // ===== Pull / Op progress (Q3 W3-4 sub-task) =====
    // Локальный кэш отмен: ключ = operation + ':' + modelName + ':' + backendId.
    // Используется для UI-state «cancel requested» — пока op живёт на сервере,
    // показываем progress как «cancelling» и блокируем кнопку. Когда op уходит
    // из active list, считаем её завершённой (cancel success).
    const _cancelledOps = new Set();
    let _opsAutoRefreshTimer = null;
    const OPS_AUTO_REFRESH_MS = 2000;
    // Heuristic-константы для расчёта прогресса (нет данных от backend'а):
    // — pull операции обычно идут 1–30 мин (HF модели);
    // — load операции — 5–60 сек (модель уже на диске, нужно прочитать header + mmap).
    const OP_HEURISTIC_DURATION_MS = {
        pull:   180000, // 3 мин — default для pull (HF model)
        load:    30000, // 30 сек — default для load
        unload:   5000, // 5 сек — default для unload
        delete:   5000,
        copy:    10000,
        create:   5000
    };

    function _opKey(op) {
        return (op.operation || '?') + ':' + (op.modelName || '?') + ':' + (op.backendId || '?');
    }

    /**
     * Heuristic-расчёт прогресса для op без реального progress-индикатора от backend.
     * Используется startedAt + heuristic duration (per operation type).
     * @returns {{pct: number, etaSec: number, isIndeterminate: boolean}}
     */
    function computeOpProgress(op) {
        const key = _opKey(op);
        const opType = (op.operation || 'load').toLowerCase();
        const startedMs = op.startedAt ? new Date(op.startedAt).getTime() : Date.now();
        const elapsedMs = Math.max(0, Date.now() - startedMs);
        const expectedMs = OP_HEURISTIC_DURATION_MS[opType] || 60000;

        if (_cancelledOps.has(key)) {
            // Замораживаем прогресс на текущем значении при отмене.
            const pctFrozen = Math.min(99, Math.max(5, Math.round((elapsedMs / expectedMs) * 100)));
            return { pct: pctFrozen, etaSec: 0, isIndeterminate: false, cancelled: true };
        }

        // Линейный прогресс от 5% (сразу) до 95% (по истечении expectedMs).
        // Не показываем 100% до того, как op реально исчезнет из active list —
        // иначе пользователь видит «готово», хотя backend ещё работает.
        const pct = Math.min(95, Math.max(5, Math.round((elapsedMs / expectedMs) * 100)));
        const remainingMs = Math.max(0, expectedMs - elapsedMs);
        const etaSec = Math.round(remainingMs / 1000);
        return { pct: pct, etaSec: etaSec, isIndeterminate: false, cancelled: false };
    }

    function formatOpsEta(etaSec) {
        if (!etaSec || etaSec < 0) return '';
        if (etaSec < 60) return '~' + etaSec + 's';
        const m = Math.floor(etaSec / 60);
        const s = etaSec % 60;
        if (m < 60) return '~' + m + 'm ' + (s ? s + 's' : '');
        const h = Math.floor(m / 60);
        const rm = m % 60;
        return '~' + h + 'h ' + rm + 'm';
    }

    /**
     * Помечает op как cancelled в локальном кэше. Backend не получает cancel-сигнал
     * (DELETE endpoint отсутствует), но UI сразу показывает «cancelling» state.
     * Когда op уйдёт из active list — toast «cancelled».
     */
    function cancelOperation(op) {
        const key = _opKey(op);
        const opLabel = (op.operation || 'op') + ' ' + (op.modelName || '');
        _cancelledOps.add(key);
        showToast((window.I18N ? I18N.t('models.cancel_requested') : 'Cancel requested for') + ': ' + opLabel, 'warning');
        addLog('Cancel requested for op: ' + key, 'warning');
        // Принудительно обновить отображение (без ожидания следующего тика).
        refreshModelOpsStatus();
    }

    function startOpsAutoRefresh() {
        if (_opsAutoRefreshTimer) return; // уже запущен
        // Защита от race: если модалка закрыта, не запускаем polling.
        // (race возникает, когда refreshModelOpsStatus resolve'ится ПОСЛЕ closeModelManageModal.)
        var modalEl = document.getElementById('modelManageModal');
        if (modalEl && !modalEl.classList.contains('active')) return;
        const indicator = document.getElementById('modelOpsAutoRefresh');
        if (indicator) indicator.hidden = false;
        refreshModelOpsStatus();
        _opsAutoRefreshTimer = setInterval(refreshModelOpsStatus, OPS_AUTO_REFRESH_MS);
    }

    function stopOpsAutoRefresh() {
        if (_opsAutoRefreshTimer) {
            clearInterval(_opsAutoRefreshTimer);
            _opsAutoRefreshTimer = null;
        }
        const indicator = document.getElementById('modelOpsAutoRefresh');
        if (indicator) indicator.hidden = true;
    }

    function refreshModelOpsStatus() {
        const opsBody = document.getElementById('modelOpsBody');
        if (!opsBody) return;
        // R84: единое правило опроса — пауза авто-обновления и скрытая вкладка.
        if (window.DataRefresh && typeof window.DataRefresh.shouldPoll === 'function' &&
            !window.DataRefresh.shouldPoll()) {
            return;
        }
        Api.modelOperationsStatus().then(function(data) {
            const ops = (data && data.operations) || [];

            // Отслеживаем op, которые ушли из active list — это «завершённые».
            // Если op была помечена как cancelled — показываем toast «cancelled».
            // Если op не была cancelled — показываем toast «completed» (только для
            // pull/load/unload/delete, чтобы не спамить на каждое обновление).
            const seenKeys = new Set();
            ops.forEach(function(op) { seenKeys.add(_opKey(op)); });
            // Копия — иначе модифицируем Set во время итерации.
            Array.from(_cancelledOps).forEach(function(key) {
                if (!seenKeys.has(key)) {
                    _cancelledOps.delete(key);
                    showToast((window.I18N ? I18N.t('models.op_cancelled') : 'Operation cancelled'), 'warning');
                }
            });

            if (ops.length === 0) {
                opsBody.innerHTML = '<tr><td colspan="7" class="loading-cell">' +
                    (window.I18N ? I18N.t('models.no_active_ops') : 'No active operations') + '</td></tr>';
                // Если была хоть одна op и сейчас ноль — останавливаем auto-refresh.
                if (_opsAutoRefreshTimer) stopOpsAutoRefresh();
                return;
            }

            // Есть активные op — убеждаемся, что auto-refresh запущен.
            if (!_opsAutoRefreshTimer) startOpsAutoRefresh();

            opsBody.innerHTML = ops.map(function(op) {
                const opKey = _opKey(op);
                const opType = (op.operation || '-');
                const progress = computeOpProgress(op);
                const isCancelling = progress.cancelled;
                const fillClass = 'ops-progress-fill' + (isCancelling ? ' cancelling' : '');
                const opTypeLabel = window.I18N
                    ? I18N.t('models.operation_' + opType, opType)
                    : opType;
                const etaText = progress.cancelled
                    ? (window.I18N ? I18N.t('models.cancel_requested') : 'Cancelling…')
                    : formatOpsEta(progress.etaSec);
                const cancelLabel = window.I18N ? I18N.t('models.cancel_op') : 'Cancel';
                const statusLabel = isCancelling
                    ? (window.I18N ? I18N.t('models.cancel_requested') : 'Cancelling…')
                    : (window.I18N ? I18N.t('models.op_running') : 'Running');

                // Escape opKey для inline onclick (replace ':' не нужен — это просто строка).
                const safeOpKey = Utils.escapeHtml(opKey).replace(/'/g, "\\'");

                return '<tr data-op-key="' + Utils.escapeHtml(opKey) + '">' +
                    '<td><span class="badge badge-info">' + Utils.escapeHtml(opTypeLabel) + '</span></td>' +
                    '<td><strong>' + Utils.escapeHtml(op.modelName || '-') + '</strong></td>' +
                    '<td>' + Utils.escapeHtml(op.backendId || '-') + '</td>' +
                    '<td>' + (op.startedAt ? new Date(op.startedAt).toLocaleString(Utils._locale()) : '-') + '</td>' +
                    '<td>' +
                        '<div class="ops-progress-wrap">' +
                            '<div class="ops-progress-bar">' +
                                '<div class="' + fillClass + '" style="width: ' + progress.pct + '%;"></div>' +
                            '</div>' +
                            '<div class="ops-progress-text">' +
                                '<span class="ops-progress-percent">' + progress.pct + '%</span>' +
                                '<span class="ops-progress-eta">' + etaText + '</span>' +
                            '</div>' +
                        '</div>' +
                    '</td>' +
                    '<td><span class="badge ' + (isCancelling ? 'badge-warning' : 'badge-info') + '">' +
                        Utils.escapeHtml(statusLabel) + '</span></td>' +
                    '<td>' +
                        '<button type="button" class="ops-action-cancel' + (isCancelling ? ' cancelling' : '') + '"' +
                            (isCancelling ? ' disabled' : '') +
                            ' onclick="ui.cancelOperationByKey(\'' + safeOpKey + '\')"' +
                            ' title="' + Utils.escapeHtml(cancelLabel) + '">' +
                            '<i class="fas ' + (isCancelling ? 'fa-circle-notch' : 'fa-stop') + '"></i> ' +
                            '<span>' + Utils.escapeHtml(cancelLabel) + '</span>' +
                        '</button>' +
                    '</td>' +
                '</tr>';
            }).join('');
        }).catch(function(err) {
            // Silently ignore — это background refresh.
            console.error('Failed to refresh model ops status:', err);
        });
    }

    /**
     * Cancel-by-key (вызывается из inline onclick в HTML рендере).
     * Ищет op в кэше по ключу и помечает cancelled.
     */
    function cancelOperationByKey(opKey) {
        // Нам нужен полный op для cancelOperation, но у нас только ключ.
        // Запоминаем ключ напрямую — следующий refresh подхватит cancelled state.
        _cancelledOps.add(opKey);
        showToast((window.I18N ? I18N.t('models.cancel_requested') : 'Cancel requested'), 'warning');
        refreshModelOpsStatus();
    }

    function closeModelManageModal() {
        // Останавливаем auto-refresh активных op (экономим трафик, если модалка закрыта).
        stopOpsAutoRefresh();
        const modal = document.getElementById('modelManageModal');
        if (modal) modal.classList.remove('active');
    }

// ---- Global helpers for model card actions (used by renderers.js modelsGrid) ----
window.modelCardAction = function(operation, backendId, modelName) {
    if (!backendId || !modelName) return;
    if (operation === 'delete') {
        if (!confirm((window.I18N ? I18N.t('models.confirm_delete') : 'Delete model') + ' ' + modelName + '?')) return;
    }
    ui.executeModelOperation(backendId, operation, modelName);
};

// ---- Q3 W4 — Session 19: Model details modal ----
// Открывает модальное окно с подробностями модели (architecture, family, params,
// quantization, n_ctx, gpu_layers, state) — через cluster-level endpoint
// GET /api/v1/cluster/models/{name}/info (см. internal/api/handlers_cluster_model_info.go).
// Один запрос агрегирует данные со всех бэкендов кластера (cppworker + ollama).
//
// Round 18 (2026-07-10): для llama.cpp бэкенда перенаправляем на GGUF Models tab
// с pre-selected backend вместо модала. Причина: cluster-models-info возвращает
// «parameter_size: 0.0B / quantization: unknown» для cppworker (cppworker их не
// отдаёт), а на GGUF tab оператор видит реальные load options, file management,
// batch/parallel/kv_cache controls и per-model profiles — всё что нужно для
// работы с cppworker моделью.
window.openModelDetailsModal = function(modelName, backendId, backendType) {
    if (!modelName) return;
    // Round 18: если backend llama.cpp → редирект на GGUF tab с pre-selected backend.
    if (backendType === 'llama_cpp' && backendId) {
        // Переключаемся на GGUF tab через nav link.
        var navLink = document.querySelector('[data-page="gguf"]');
        if (navLink) navLink.click();
        // Pre-select backend после того как GgufRenderer отрендерит.
        setTimeout(function() {
            if (window.GgufRenderer && typeof window.GgufRenderer.selectBackend === 'function') {
                window.GgufRenderer.selectBackend(backendId);
            } else {
                console.warn('[openModelDetailsModal] GgufRenderer.selectBackend unavailable');
            }
        }, 100);
        return;
    }
    // Ollama (или backend type неизвестен) → показываем модал как раньше.
    // Modal DOM может быть ещё не создан — создаём лениво.
    ensureModelDetailsModalDom();
    const modal = document.getElementById('modelDetailsModal');
    const body = document.getElementById('modelDetailsBody');
    const title = document.getElementById('modelDetailsTitle');
    if (!modal || !body || !title) {
        console.warn('model details modal DOM not found');
        return;
    }
    title.textContent = (window.I18N ? I18N.t('models.details.title') : 'Model details') + ' — ' + modelName;
    body.innerHTML = '<div class="loading">' +
        (window.I18N ? I18N.t('models.details.loading') : 'Loading details...') + '</div>';
    modal.style.display = 'flex';

    Api.clusterModels.info(modelName).then(function(resp) {
        renderModelDetailsBody(body, modelName, resp);
    }).catch(function(err) {
        body.innerHTML = '<div class="error-message">' +
            (window.I18N ? I18N.t('models.details.error') : 'Failed to load details') + ': ' +
            Utils.escapeHtml(err && err.message ? err.message : err) + '</div>';
    });
};

// ensureModelDetailsModalDom — создаёт модальное окно в <body> если ещё не существует.
//
// Используем ленивую инициализацию, чтобы не раздувать webui/index.html.
function ensureModelDetailsModalDom() {
    let modal = document.getElementById('modelDetailsModal');
    if (modal) return;
    modal = document.createElement('div');
    modal.id = 'modelDetailsModal';
    modal.className = 'modal-overlay';
    modal.style.display = 'none';
    modal.innerHTML = `
        <div class="modal-window model-details-window" role="dialog" aria-modal="true" aria-labelledby="modelDetailsTitle">
            <div class="modal-header">
                <h2 id="modelDetailsTitle">${Utils.escapeHtml(window.I18N ? I18N.t('models.details.title') : 'Model details')}</h2>
                <button class="modal-close" id="modelDetailsClose" aria-label="Close">×</button>
            </div>
            <div class="modal-body" id="modelDetailsBody">
                <div class="loading">...</div>
            </div>
        </div>
    `;
    document.body.appendChild(modal);
    // Закрытие по клику на крестик или backdrop.
    document.getElementById('modelDetailsClose').addEventListener('click', closeModelDetailsModal);
    modal.addEventListener('click', function(ev) {
        if (ev.target === modal) closeModelDetailsModal();
    });
    // Закрытие по Escape.
    document.addEventListener('keydown', function(ev) {
        if (ev.key === 'Escape') closeModelDetailsModal();
    });
}

function closeModelDetailsModal() {
    const modal = document.getElementById('modelDetailsModal');
    if (modal) modal.style.display = 'none';
}

// renderModelDetailsBody — рисует содержимое модального окна по ответу
// /api/v1/cluster/models/{name}/info.
//
// Структура ответа:
//   {
//     model: string,
//     count: number,
//     okCount: number,
//     backends: [
//       { backendId, backendType, status, info: { details, model_info, ... }, error?, httpStatus? }
//     ]
//   }
//
// Семантика:
//   - Если есть хотя бы один backend со status=ok, рендерим секцию «Primary» с данными info.
//   - Если есть несколько ok — рендерим несколько секций, чтобы показать расхождения
//     (например, разные n_ctx / gpu_layers на разных бэкендах).
//   - Если okCount == 0 — рендерим список ошибок per-backend.
function renderModelDetailsBody(body, modelName, resp) {
    if (!resp || !resp.backends || !resp.backends.length) {
        body.innerHTML = '<div class="error-message">' +
            (window.I18N ? I18N.t('models.details.no_backends') : 'No backends in cluster') + '</div>';
        return;
    }
    const okBackends = resp.backends.filter(function(b) { return b.status === 'ok'; });
    const otherBackends = resp.backends.filter(function(b) { return b.status !== 'ok'; });
    let html = '';
    if (okBackends.length > 0) {
        html += renderModelDetailsOkSection(modelName, okBackends, resp);
    } else {
        html += '<div class="warning-message">' +
            (window.I18N ? I18N.t('models.details.no_ok_backends') :
                'Model is not loaded on any backend — details unavailable.') + '</div>';
    }
    if (otherBackends.length > 0) {
        html += renderModelDetailsBackendsList(otherBackends, /* onlyIssues */ true);
    }
    body.innerHTML = html;
}

// renderModelDetailsOkSection — рендерит секции «General / Capabilities / Model Info» для ok-бэкендов.
//
// Если ok-бэкендов несколько, рендерим каждый отдельно (с заголовком backendId),
// чтобы пользователь видел расхождения в параметрах.
function renderModelDetailsOkSection(modelName, okBackends, resp) {
    const _t = function(k, fb) { return (window.I18N ? I18N.t(k) : fb); };
    let html = '<div class="model-details-summary">';
    html += '<div class="model-details-row"><span class="label">' + _t('models.details.summary_model', 'Model') + ':</span>' +
        '<span class="value"><code>' + Utils.escapeHtml(modelName) + '</code></span></div>';
    html += '<div class="model-details-row"><span class="label">' + _t('models.details.summary_count', 'Reported by') + ':</span>' +
        '<span class="value">' + resp.okCount + ' / ' + resp.count + ' ' + _t('models.details.summary_backends', 'backends') + '</span></div>';
    html += '</div>';
    okBackends.forEach(function(b) {
        const info = b.info || {};
        const details = info.details || {};
        const modelInfo = info.model_info || {};
        const backendLabel = Utils.escapeHtml(b.backendId) +
            ' <span class="badge badge-info">' + Utils.escapeHtml(b.backendType) + '</span>';
        html += '<h3 class="model-details-backend-title">' + backendLabel + '</h3>';
        html += '<div class="model-details-section">';
        html += '<h4>' + _t('models.details.section_general', 'General') + '</h4>';
        html += modelDetailsRow('Family', details.family || modelInfo.architecture);
        html += modelDetailsRow('Format', details.format);
        html += modelDetailsRow('Parameter Size', details.parameter_size || info.parameters);
        html += modelDetailsRow('Quantization', details.quantization_level);
        html += '</div>';
        html += '<div class="model-details-section">';
        html += '<h4>' + _t('models.details.section_capabilities', 'Capabilities') + '</h4>';
        html += modelDetailsRow('Architecture', modelInfo.architecture);
        html += modelDetailsRow('Layers (n_layers)', modelInfo.n_layers);
        html += modelDetailsRow('Heads (n_heads)', modelInfo.n_heads);
        html += modelDetailsRow('Embedding size (n_embd)', modelInfo.n_embd);
        html += modelDetailsRow('Vocab size (n_vocab)', modelInfo.n_vocab);
        html += '</div>';
        html += '<div class="model-details-section">';
        html += '<h4>' + _t('models.details.section_runtime', 'Runtime / Load') + '</h4>';
        html += modelDetailsRow('Context size (n_ctx)', modelInfo.context_size);
        html += modelDetailsRow('GPU layers', modelInfo.gpu_layers);
        html += modelDetailsRow('Batch size', modelInfo.batch_size);
        html += modelDetailsRow('Parallel (n_parallel)', modelInfo.parallel);
        html += modelDetailsRow('KV cache type', modelInfo.kv_cache_type);
        html += modelDetailsRow('Flash attention', modelInfo.flash_attn);
        html += modelDetailsRow('Use mmap', modelInfo.use_mmap);
        html += modelDetailsRow('NUMA', modelInfo.numa);
        html += modelDetailsRow('State', modelInfo.state);
        html += modelDetailsRow('Size (bytes)', modelInfo.size_bytes);
        html += modelDetailsRow('Modified at', modelInfo.modified_at);
        html += '</div>';
    });
    return html;
}

function modelDetailsRow(label, value) {
    if (value === undefined || value === null || value === '') return '';
    const _t = function(k, fb) { return (window.I18N ? I18N.t(k) : fb); };
    let display = value;
    if (typeof value === 'object') {
        display = JSON.stringify(value);
    }
    return '<div class="model-details-row"><span class="label">' + Utils.escapeHtml(label) + ':</span>' +
        '<span class="value">' + Utils.escapeHtml(String(display)) + '</span></div>';
}

// renderModelDetailsBackendsList — список проблемных бэкендов (error/not_found/unavailable).
function renderModelDetailsBackendsList(backends, onlyIssues) {
    const _t = function(k, fb) { return (window.I18N ? I18N.t(k) : fb); };
    let html = '<h3>' + _t('models.details.section_other_backends', 'Other backends') + '</h3>';
    html += '<ul class="model-details-backends-list">';
    backends.forEach(function(b) {
        const cls = b.status === 'ok' ? 'success' : (b.status === 'not_found' ? 'info' : 'danger');
        html += '<li class="model-details-backend-item">';
        html += '<span class="badge badge-' + cls + '">' + Utils.escapeHtml(b.status) + '</span> ';
        html += '<strong>' + Utils.escapeHtml(b.backendId) + '</strong> ' +
                '<span class="model-details-backend-type">(' + Utils.escapeHtml(b.backendType) + ')</span>';
        if (b.error) html += ' — ' + Utils.escapeHtml(b.error);
        html += '</li>';
    });
    html += '</ul>';
    return html;
}



        return {
            restartAgent: restartAgent,
            viewAgentLogs: viewAgentLogs,
            fetchProxyLogs: fetchProxyLogs,
            toggleBackendEnabled: toggleBackendEnabled,
            loadBackendLimits: loadBackendLimits,
            saveBackendLimits: saveBackendLimits,
            openModelManageModal: openModelManageModal,
            loadBackendModels: loadBackendModels,
            executeModelOperation: executeModelOperation,
            refreshModelOpsStatus: refreshModelOpsStatus,
            closeModelManageModal: closeModelManageModal,
            startOpsAutoRefresh: startOpsAutoRefresh,
            stopOpsAutoRefresh: stopOpsAutoRefresh,
            cancelOperationByKey: cancelOperationByKey,
            donorMarkForBackend: donorMarkForBackend,
            donorLoadBlocked: donorLoadBlocked,
            donorBlockedText: donorBlockedText
        };
    };
})();
