/**
 * gguf-reexport.js — блок «Ре-экспорт GGUF» на странице моделей (R709).
 *
 * ЗАЧЕМ БЛОК ЖИВЁТ ОТДЕЛЬНО ОТ РЕНДЕРЕРА GGUF. Ре-экспорт — ОПЦИОНАЛЬНЫЙ модуль
 * (compose-профиль `reexport`): на большинстве стендов сервиса нет вовсе, и
 * страница моделей обязана выглядеть ровно как раньше. Поэтому:
 *
 *   1. рендерер GGUF создаёт только ПУСТОЙ хост (#ggufReexportHost) — ни одного
 *      элемента, ни одной подписи в разметке нет;
 *   2. этот модуль сам спрашивает GET /api/reexport/capabilities (через nginx
 *      WebUI, тот же origin — CORS и отдельных портов не нужно);
 *   3. ответ 200 → в хост вставляется блок; 502/таймаут/сеть → хост остаётся
 *      пустым, и вкладки/блока НЕТ ВООБЩЕ (требование оператора: «нет модуля —
 *      нет вкладки», а не «вкладка есть, но пустая»).
 *
 * ПОЧЕМУ ПРОБА, А НЕ ФЛАГ В КОНФИГЕ. Флаг в .env пришлось бы синхронизировать с
 * фактом «сервис поднят» — и он врёт в обе стороны: включили профиль без флага
 * (блок не появился) или выключили сервис с флагом (мёртвая вкладка с ошибками).
 * Проба отвечает на настоящий вопрос «модуль доступен прямо сейчас».
 *
 * ЧТО ПОКАЗЫВАЕТ: поддерживаемые архитектуры и версию конвертера llama.cpp (из
 * образа), список моделей тома моделей с вердиктом схемы, форму создания задачи,
 * список задач с прогрессом и причиной отказа, отмену, и кнопку
 * «Пересканировать модели» (POST /api/models/refresh на воркере) — после ре-экспорта
 * воркер обязан перечитать каталог, иначе новый файл для него не существует.
 *
 * ЭКСПОРТ: window.GgufReexport = { mount, _state, _buildJobBody, _formatBytes,
 * _statusLabel, _probeOnce }. Тесты используют подчёркнутые члены; публичный
 * контракт — только mount().
 */
(function () {
    'use strict';

    var API = '/api/reexport';
    // Таймаут пробы короткий осознанно: это вопрос «ты здесь», а не работа. 4 с
    // хватает локальной сети docker; при отсутствии сервиса nginx отвечает 502
    // сразу, так что ожидание видит только человек с медленным прокси.
    var PROBE_TIMEOUT_MS = 4000;
    // Поллинг задач: 2 с. Работа идёт минутами, чаще незачем; реже — прогресс
    // выглядит замершим.
    var POLL_INTERVAL_MS = 2000;

    var state = {
        probed: false,
        available: false,
        caps: null,
        models: [],
        selectedModel: '',
        jobs: [],
        error: '',
        lastJobId: '',
        pollTimer: null,
        modelsError: ''
    };

    function t(key, vars) {
        if (typeof window !== 'undefined' && window.I18N && typeof window.I18N.t === 'function') {
            return vars ? window.I18N.t(key, vars) : window.I18N.t(key);
        }
        return key;
    }

    function esc(value) {
        var s = (value === undefined || value === null) ? '' : String(value);
        if (typeof window !== 'undefined' && window.Utils && typeof window.Utils.escapeHtml === 'function') {
            return window.Utils.escapeHtml(s);
        }
        return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }

    /** formatBytes — размер в человеческом виде (МиБ/ГиБ, а не миллионы цифр). */
    function formatBytes(bytes) {
        var n = Number(bytes) || 0;
        if (n <= 0) return '0';
        var gb = n / (1024 * 1024 * 1024);
        if (gb >= 1) return gb.toFixed(gb >= 10 ? 1 : 2) + ' GiB';
        var mb = n / (1024 * 1024);
        return mb.toFixed(0) + ' MiB';
    }

    /** formatDuration — мм:сс для итога задачи. */
    function formatDuration(ms) {
        var total = Math.max(0, Math.round((Number(ms) || 0) / 1000));
        var m = Math.floor(total / 60);
        var s = total % 60;
        return (m > 0 ? m + 'm ' : '') + s + 's';
    }

    /** statusLabel — человеческий статус задачи (ключ i18n на каждый статус). */
    function statusLabel(status) {
        switch (status) {
        case 'queued': return t('reexport.status_queued');
        case 'running': return t('reexport.status_running');
        case 'succeeded': return t('reexport.status_succeeded');
        case 'failed': return t('reexport.status_failed');
        case 'canceled': return t('reexport.status_canceled');
        case 'interrupted': return t('reexport.status_interrupted');
        default: return String(status || '');
        }
    }

    /** authHeaders — CSRF/токен как у остальных модулей (R202: сессия, R59.5). */
    function authHeaders(extra) {
        var out = { 'Accept': 'application/json' };
        if (extra) {
            Object.keys(extra).forEach(function (k) { out[k] = extra[k]; });
        }
        if (typeof window !== 'undefined' && window.Api && typeof window.Api.getAuthHeaders === 'function') {
            var h = window.Api.getAuthHeaders() || {};
            Object.keys(h).forEach(function (k) {
                if (out[k] === undefined && h[k]) out[k] = h[k];
            });
        }
        return out;
    }

    /** requestJSON — fetch с таймаутом на чтение и разбором ошибки сервиса. */
    function requestJSON(url, options) {
        var opts = options || {};
        var timeout = opts.timeoutMs === undefined ? PROBE_TIMEOUT_MS : opts.timeoutMs;
        var ctrl = null;
        var timer = null;
        if (timeout > 0 && typeof AbortController !== 'undefined') {
            ctrl = new AbortController();
            timer = setTimeout(function () { ctrl.abort(); }, timeout);
            opts.signal = ctrl.signal;
        }
        return fetch(url, opts).then(function (resp) {
            return resp.text().then(function (text) {
                var body = null;
                if (text) {
                    try { body = JSON.parse(text); } catch (e) { body = null; }
                }
                if (!resp.ok) {
                    var err = new Error((body && body.error && body.error.message) || ('HTTP ' + resp.status));
                    err.status = resp.status;
                    err.code = body && body.error ? body.error.code : '';
                    err.body = body;
                    throw err;
                }
                return body || {};
            });
        }).finally(function () {
            if (timer) clearTimeout(timer);
        });
    }

    /**
     * probeOnce — одна проба доступности модуля.
     *
     * Возвращает Promise<boolean>. Никогда не rejects: отсутствие модуля — это
     * НОРМАЛЬНОЕ состояние стенда, а не ошибка страницы, и не повод писать в
     * консоль красным.
     */
    function probeOnce(timeoutMs) {
        return requestJSON(API + '/capabilities', {
            method: 'GET',
            headers: authHeaders(),
            credentials: 'same-origin',
            timeoutMs: timeoutMs === undefined ? PROBE_TIMEOUT_MS : timeoutMs
        }).then(function (caps) {
            state.caps = caps || null;
            state.available = !!(caps && caps.service);
            state.probed = true;
            state.error = '';
            return state.available;
        }).catch(function () {
            state.caps = null;
            state.available = false;
            state.probed = true;
            return false;
        });
    }

    /** host — место блока. Его создаёт рендерер GGUF (всегда, пустым). */
    function host() {
        if (typeof document === 'undefined' || !document.getElementById) return null;
        return document.getElementById('ggufReexportHost');
    }

    /** mount — точка входа: проба + (при успехе) отрисовка блока. */
    function mount() {
        var el = host();
        if (!el) return Promise.resolve(false);
        // Повторный вход на страницу: если модуль уже известен — рисуем сразу из
        // кэша и обновляем возможности в фоне (без мигания «блока нет»).
        if (state.available && state.caps) {
            renderBlock(el);
            probeOnce().then(function (ok) {
                if (!ok) {
                    clearBlock(el);
                } else {
                    updateConverterLine(el);
                }
            });
            return Promise.resolve(true);
        }
        clearBlock(el);
        return probeOnce().then(function (ok) {
            if (!ok) {
                // Модуля нет: хост пуст, вкладки нет вообще. Никаких «ошибок» и
                // повторных попыток — стенд без модуля это штатный стенд.
                state.models = [];
                state.jobs = [];
                return false;
            }
            renderBlock(el);
            return true;
        });
    }

    function clearBlock(el) {
        if (el) el.innerHTML = '';
        stopPolling();
    }

    /** buildHTML — разметка блока. Данные подставляются отдельными функциями. */
    function buildHTML() {
        return '' +
            '<section class="card" id="ggufReexportCard" style="margin-bottom:12px;">' +
                '<div class="card-header">' +
                    '<h3><i class="fas fa-recycle section-icon-i" aria-hidden="true"></i> ' +
                        '<span data-i18n="reexport.title">' + esc(t('reexport.title')) + '</span></h3>' +
                    '<div class="card-actions" style="display:flex;gap:8px;align-items:center;flex-wrap:wrap;">' +
                        '<span id="rxConverter" class="rx-converter" style="font-size:var(--font-size-xs);color:var(--text-muted);"></span>' +
                        '<button type="button" class="btn btn-sm btn-secondary" id="rxRefresh">' +
                            '<i class="fas fa-rotate" aria-hidden="true"></i> ' + esc(t('reexport.refresh')) + '</button>' +
                        '<button type="button" class="btn btn-sm btn-secondary" id="rxRescan">' +
                            '<i class="fas fa-arrows-rotate" aria-hidden="true"></i> ' + esc(t('reexport.rescan')) + '</button>' +
                    '</div>' +
                '</div>' +
                '<div class="card-body">' +
                    '<div id="rxArch" style="display:flex;gap:6px;flex-wrap:wrap;margin-bottom:10px;"></div>' +
                    '<form id="rxForm" style="display:flex;gap:10px;flex-wrap:wrap;align-items:flex-end;">' +
                        '<div class="form-group" style="margin:0;min-width:280px;flex:1;">' +
                            '<label for="rxModel">' + esc(t('reexport.model')) + '</label>' +
                            '<select class="form-control" id="rxModel"></select>' +
                        '</div>' +
                        '<div class="form-group" style="margin:0;min-width:200px;">' +
                            '<label for="rxTarget">' + esc(t('reexport.target')) + '</label>' +
                            '<select class="form-control" id="rxTarget"></select>' +
                        '</div>' +
                        '<div class="form-group" style="margin:0;min-width:260px;flex:1;">' +
                            '<label for="rxOut">' + esc(t('reexport.out_path')) + '</label>' +
                            '<input class="form-control" id="rxOut" type="text" placeholder="' + esc(t('reexport.out_path_hint')) + '">' +
                        '</div>' +
                        '<div class="form-group" style="margin:0;">' +
                            '<label style="display:flex;gap:6px;align-items:center;">' +
                                '<input type="checkbox" id="rxFuse" checked> ' + esc(t('reexport.fuse')) +
                            '</label>' +
                        '</div>' +
                        '<div class="form-group" style="margin:0;">' +
                            '<label style="display:flex;gap:6px;align-items:center;">' +
                                '<input type="checkbox" id="rxDryRun"> ' + esc(t('reexport.dry_run')) +
                            '</label>' +
                        '</div>' +
                        '<button type="submit" class="btn btn-primary" id="rxCreate">' +
                            '<i class="fas fa-file-export" aria-hidden="true"></i> ' + esc(t('reexport.create')) + '</button>' +
                    '</form>' +
                    '<div id="rxModelHint" style="margin-top:8px;font-size:var(--font-size-xs);color:var(--text-muted);"></div>' +
                    '<div id="rxError" role="alert" style="display:none;margin-top:10px;padding:8px 10px;border-radius:6px;background:var(--bg-secondary);border:1px solid var(--border-color);font-size:var(--font-size-sm);"></div>' +
                    '<div style="margin-top:14px;">' +
                        '<h4 style="margin:0 0 8px;font-size:var(--font-size-sm);">' + esc(t('reexport.jobs')) + '</h4>' +
                        '<div class="table-responsive">' +
                            '<table class="data-table"><thead><tr>' +
                                '<th>' + esc(t('reexport.col_job')) + '</th>' +
                                '<th>' + esc(t('reexport.col_model')) + '</th>' +
                                '<th>' + esc(t('reexport.col_status')) + '</th>' +
                                '<th>' + esc(t('reexport.col_progress')) + '</th>' +
                                '<th>' + esc(t('reexport.col_result')) + '</th>' +
                                '<th></th>' +
                            '</tr></thead><tbody id="rxJobsBody"></tbody></table>' +
                        '</div>' +
                    '</div>' +
                '</div>' +
            '</section>';
    }

    /** renderBlock — вставка разметки и привязка обработчиков. */
    function renderBlock(el) {
        el.innerHTML = buildHTML();
        updateConverterLine(el);
        renderArchitectures(el);
        renderTargets(el);
        bind(el);
        state.error = '';
        loadModels();
        loadJobs();
    }

    function updateConverterLine(el) {
        var node = el.querySelector('#rxConverter');
        if (!node || !state.caps) return;
        var conv = state.caps.converter || {};
        var parts = [];
        if (conv.name) parts.push(conv.name + (conv.revision ? ' ' + conv.revision : ''));
        if (conv.python) parts.push('python ' + conv.python);
        parts.push('torch: ' + (conv.torch_state === 'present' ? (conv.torch || 'present') : t('reexport.torch_absent')));
        node.textContent = parts.join(' · ');
    }

    /**
     * renderArchitectures — какие архитектуры модуль умеет приводить в порядок.
     *
     * Показываем данные сервиса, а не свою таблицу: если сервис обновится, страница
     * обновится вместе с ним (иначе UI и возможности расходятся).
     */
    function renderArchitectures(el) {
        var node = el.querySelector('#rxArch');
        if (!node || !state.caps) return;
        var archs = state.caps.architectures || [];
        node.innerHTML = archs.map(function (a) {
            var label = a.label || a.id;
            var note = a.note ? (' title="' + esc(a.note) + '"') : '';
            return '<span class="filter-btn"' + note + ' style="cursor:default;">' +
                '<i class="fas fa-microchip" aria-hidden="true"></i> ' + esc(label) + '</span>';
        }).join(' ');
    }

    /** renderTargets — что вообще можно сделать (доступность приходит от сервиса). */
    function renderTargets(el) {
        var sel = el.querySelector('#rxTarget');
        if (!sel || !state.caps) return;
        var targets = state.caps.targets || [];
        sel.innerHTML = targets.map(function (tg) {
            var disabled = tg.available ? '' : ' disabled';
            var label = tg.label || tg.id;
            if (!tg.available && tg.reason) {
                label += ' — ' + t('reexport.target_unavailable');
            }
            return '<option value="' + esc(tg.id) + '"' + disabled + '>' + esc(label) + '</option>';
        }).join('');
        // Автовыбор первой доступной цели, чтобы «Создать задачу» не отправляло
        // недоступную по умолчанию.
        for (var i = 0; i < targets.length; i++) {
            if (targets[i].available) { sel.value = targets[i].id; break; }
        }
    }

    /** bind — обработчики блока. Пересоздаются вместе с разметкой. */
    function bind(el) {
        var form = el.querySelector('#rxForm');
        if (form) {
            form.addEventListener('submit', function (ev) {
                ev.preventDefault();
                createJob(el);
            });
        }
        var refresh = el.querySelector('#rxRefresh');
        if (refresh) {
            refresh.addEventListener('click', function () {
                probeOnce().then(function (ok) {
                    if (!ok) { clearBlock(el); return; }
                    renderBlock(el);
                    loadModels(true);
                });
            });
        }
        var rescan = el.querySelector('#rxRescan');
        if (rescan) {
            rescan.addEventListener('click', function () { rescanModels(el); });
        }
        var sel = el.querySelector('#rxModel');
        if (sel) {
            sel.addEventListener('change', function () {
                state.selectedModel = sel.value;
                renderModelHint(el);
            });
        }
        var tbody = el.querySelector('#rxJobsBody');
        if (tbody) {
            tbody.addEventListener('click', function (ev) {
                var btn = ev.target && ev.target.closest ? ev.target.closest('[data-cancel-job]') : null;
                if (!btn) return;
                cancelJob(el, btn.getAttribute('data-cancel-job'));
            });
        }
    }

    /** showError — причина отказа одной строкой (код + текст сервиса). */
    function showError(el, message) {
        var node = el.querySelector('#rxError');
        if (!node) return;
        if (!message) {
            node.style.display = 'none';
            node.textContent = '';
            return;
        }
        node.style.display = '';
        node.textContent = message;
    }

    /** loadModels — список GGUF тома моделей (с вердиктом схемы). */
    function loadModels(refresh) {
        var el = host();
        if (!el) return Promise.resolve();
        return requestJSON(API + '/models' + (refresh ? '?refresh=1' : ''), {
            method: 'GET', headers: authHeaders(), credentials: 'same-origin', timeoutMs: 120000
        }).then(function (data) {
            state.models = (data && data.models) || [];
            state.modelsError = '';
            fillModelSelect(el);
            renderModelHint(el);
        }).catch(function (err) {
            state.models = [];
            state.modelsError = err && err.message ? err.message : String(err);
            fillModelSelect(el);
            showError(el, t('reexport.models_failed') + ': ' + state.modelsError);
        });
    }

    function fillModelSelect(el) {
        var sel = el.querySelector('#rxModel');
        if (!sel) return;
        var prev = state.selectedModel || sel.value;
        if (!state.models.length) {
            sel.innerHTML = '<option value="">' + esc(t('reexport.no_models')) + '</option>';
            state.selectedModel = '';
            return;
        }
        sel.innerHTML = state.models.map(function (m) {
            var label = (m.name || m.path) + ' · ' + formatBytes(m.size_bytes);
            if (m.arch) label += ' · ' + m.arch;
            return '<option value="' + esc(m.path) + '">' + esc(label) + '</option>';
        }).join('');
        if (prev && state.models.some(function (m) { return m.path === prev; })) {
            sel.value = prev;
        }
        state.selectedModel = sel.value;
    }

    /**
     * renderModelHint — что сервис думает о выбранной модели.
     *
     * Здесь видно ДО создания задачи, нужен ли патч: «block_count=48 при 24
     * физических блоках — loop-экспорт, нужен ре-экспорт» либо «файл загружается
     * как есть». Это ответ на вопрос оператора «а почему эта модель не грузится».
     */
    function renderModelHint(el) {
        var node = el.querySelector('#rxModelHint');
        if (!node) return;
        var model = null;
        for (var i = 0; i < state.models.length; i++) {
            if (state.models[i].path === state.selectedModel) model = state.models[i];
        }
        if (!model) {
            node.textContent = state.modelsError
                ? (t('reexport.models_failed') + ': ' + state.modelsError)
                : t('reexport.no_models');
            return;
        }
        var bits = [];
        if (model.read_error) {
            bits.push(t('reexport.model_unreadable') + ': ' + model.read_error);
        } else {
            bits.push(t('reexport.hint_scheme') + ': ' + (model.scheme || '—'));
            if (model.block_count) bits.push('block_count=' + model.block_count);
            if (model.physical_blocks) bits.push(t('reexport.hint_physical') + '=' + model.physical_blocks);
            if (model.patchable) {
                bits.push(t('reexport.hint_patch_needed'));
            } else if (model.scheme === 'plain' || model.scheme === 'inkling2_loop') {
                bits.push(t('reexport.hint_plain'));
            }
            if (model.msg_query_tensors) bits.push('loop_msg_query=' + model.msg_query_tensors);
        }
        node.textContent = bits.join(' · ');
    }

    /** buildJobBody — тело POST из формы (чистая функция: её проверяет тест). */
    function buildJobBody(el) {
        var opts = {};
        var fuse = el.querySelector('#rxFuse');
        var dry = el.querySelector('#rxDryRun');
        var out = el.querySelector('#rxOut');
        if (fuse && fuse.checked) opts.fuse_loop_queries = true;
        if (dry && dry.checked) opts.dry_run = true;
        if (out && out.value && out.value.trim()) opts.out_path = out.value.trim();
        return {
            model_dir: state.selectedModel,
            target: (el.querySelector('#rxTarget') || {}).value || '',
            options: opts
        };
    }

    /** createJob — создание задачи. Ошибку сервиса показываем как есть. */
    function createJob(el) {
        if (!state.selectedModel) {
            showError(el, t('reexport.need_model'));
            return;
        }
        var body = buildJobBody(el);
        showError(el, '');
        var btn = el.querySelector('#rxCreate');
        if (btn) { btn.disabled = true; }
        requestJSON(API + '/jobs', {
            method: 'POST',
            headers: authHeaders({ 'Content-Type': 'application/json' }),
            credentials: 'same-origin',
            timeoutMs: 0,
            body: JSON.stringify(body)
        }).then(function (job) {
            state.lastJobId = job && job.id ? job.id : '';
            showError(el, '');
            return loadJobs();
        }).catch(function (err) {
            showError(el, t('reexport.create_failed') + ': ' + (err && err.message ? err.message : String(err)));
        }).finally(function () {
            if (btn) { btn.disabled = false; }
        });
    }

    /** cancelJob — DELETE: процесс конвертера убивается, частичный файл удаляется. */
    function cancelJob(el, id) {
        if (!id) return;
        requestJSON(API + '/jobs/' + encodeURIComponent(id), {
            method: 'DELETE',
            headers: authHeaders(),
            credentials: 'same-origin',
            timeoutMs: 15000
        }).then(function () {
            return loadJobs();
        }).catch(function (err) {
            showError(el, t('reexport.cancel_failed') + ': ' + (err && err.message ? err.message : String(err)));
        });
    }

    /** loadJobs — список задач; запускает поллинг, пока есть активные. */
    function loadJobs() {
        var el = host();
        if (!el) return Promise.resolve();
        return requestJSON(API + '/jobs', {
            method: 'GET', headers: authHeaders(), credentials: 'same-origin', timeoutMs: 15000
        }).then(function (data) {
            state.jobs = (data && data.jobs) || [];
            renderJobs(el);
            schedulePolling();
        }).catch(function () {
            // Сервис мог быть выключен между пробой и запросом: блок убираем,
            // чтобы не показывать мёртвую вкладку.
            state.jobs = [];
            clearBlock(el);
        });
    }

    function activeJobs() {
        return (state.jobs || []).filter(function (j) {
            return j.status === 'running' || j.status === 'queued';
        });
    }

    function schedulePolling() {
        if (activeJobs().length > 0) {
            startPolling();
        } else {
            stopPolling();
        }
    }

    function startPolling() {
        if (state.pollTimer) return;
        state.pollTimer = setTimeout(function () {
            state.pollTimer = null;
            if (typeof document !== 'undefined' && document.hidden) {
                // Вкладка не видна — не тратим запросы; вернёмся при следующем
                // действии (кнопка «Обновить» или возврат на страницу).
                startPolling();
                return;
            }
            loadJobs();
        }, POLL_INTERVAL_MS);
    }

    function stopPolling() {
        if (state.pollTimer) {
            clearTimeout(state.pollTimer);
            state.pollTimer = null;
        }
    }

    /** progressCell — прогресс-бар + проценты + объём. */
    function progressCell(job) {
        var p = job.progress || {};
        var percent = Number(p.percent) || 0;
        if (job.status === 'succeeded') percent = 100;
        var width = Math.max(0, Math.min(100, percent));
        var bar = '<div style="height:6px;border-radius:3px;background:var(--bg-secondary);overflow:hidden;">' +
            '<div style="height:100%;width:' + width.toFixed(1) + '%;background:var(--accent, #3b82f6);"></div></div>';
        var text = percent.toFixed(1) + '%';
        if (p.bytes_total) {
            text += ' · ' + formatBytes(p.bytes_done || 0) + ' / ' + formatBytes(p.bytes_total);
        }
        if (p.phase) text += ' · ' + esc(p.phase);
        return bar + '<div style="font-size:var(--font-size-xs);color:var(--text-muted);margin-top:4px;">' + text + '</div>';
    }

    /** resultCell — итог: время, размеры, причина отказа. */
    function resultCell(job) {
        if (job.error && job.error.message) {
            return '<span style="color:var(--danger, #ef4444);">' + esc(job.error.message) + '</span>';
        }
        var r = job.result;
        if (!r) return '';
        var parts = [];
        if (r.duration_ms) parts.push(formatDuration(r.duration_ms));
        if (r.dst_bytes) parts.push(formatBytes(r.dst_bytes));
        if (r.verify && r.verify.dry_run) parts.push(t('reexport.dry_run_result'));
        return esc(parts.join(' · '));
    }

    function renderJobs(el) {
        var tbody = el.querySelector('#rxJobsBody');
        if (!tbody) return;
        if (!state.jobs.length) {
            tbody.innerHTML = '<tr><td colspan="6" style="color:var(--text-muted);">' +
                esc(t('reexport.no_jobs')) + '</td></tr>';
            return;
        }
        tbody.innerHTML = state.jobs.map(function (job) {
            var id = esc(job.id);
            var canCancel = job.status === 'running' || job.status === 'queued';
            var action = canCancel
                ? '<button type="button" class="btn btn-sm btn-secondary" data-cancel-job="' + id + '">' +
                    esc(t('reexport.cancel')) + '</button>'
                : '';
            var name = esc(job.model_dir || '');
            if (job.out_path) {
                name += '<div style="font-size:var(--font-size-xs);color:var(--text-muted);">' +
                    esc(job.out_path) + '</div>';
            }
            return '<tr>' +
                '<td><code>' + id + '</code><div style="font-size:var(--font-size-xs);color:var(--text-muted);">' +
                    esc(job.target || '') + '</div></td>' +
                '<td>' + name + '</td>' +
                '<td>' + esc(statusLabel(job.status)) + '</td>' +
                '<td style="min-width:160px;">' + progressCell(job) + '</td>' +
                '<td>' + resultCell(job) + '</td>' +
                '<td>' + action + '</td>' +
                '</tr>';
        }).join('');
    }

    /**
     * rescanModels — «Пересканировать модели» после успешного ре-экспорта.
     *
     * ПОЧЕМУ КНОПКА ВООБЩЕ НУЖНА. Ре-экспорт кладёт НОВЫЙ файл в том моделей, но
     * воркер держит свой каталог моделей в памяти (инвентарь с TTL) и без
     * POST /api/models/refresh просто не знает о файле — «задача успешна, а модели
     * нет». Кнопка зовёт существующий механизм страницы GGUF
     * (GgufModule.rescanAllBackendModels → /api/models/refresh на каждом воркере),
     * а не свой запрос: правило пересканирования должно быть одно.
     *
     * Плюс сбрасываем кэш фактов сервиса (?refresh=1), чтобы форма увидела новый
     * размер и схему файла.
     */
    function rescanModels(el) {
        var mod = (typeof window !== 'undefined') ? window.GgufModule : null;
        var done = function () {
            loadModels(true);
            showError(el, '');
        };
        if (mod && typeof mod.rescanAllBackendModels === 'function') {
            try {
                var res = mod.rescanAllBackendModels();
                if (res && typeof res.then === 'function') {
                    res.then(done, done);
                    return;
                }
            } catch (e) { /* падение пересканирования не должно ломать блок */ }
        }
        done();
    }

    window.GgufReexport = {
        mount: mount,
        _state: state,
        _buildJobBody: buildJobBody,
        _formatBytes: formatBytes,
        _formatDuration: formatDuration,
        _statusLabel: statusLabel,
        _probeOnce: probeOnce,
        _buildHTML: buildHTML,
        _renderJobs: renderJobs,
        _stopPolling: stopPolling
    };
})();
