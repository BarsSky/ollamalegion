/**
 * gguf-reexport-jobs.js — слой ЗАДАЧ блока «Ре-экспорт GGUF» (D11, R628).
 *
 * ЗАЧЕМ ОТДЕЛЬНЫЙ ФАЙЛ. Блок ре-экспорта делает две разные вещи: (1) шаги
 * сценария — какие модели есть, что сделает проверка, чем закончится
 * нормализация, как пройдёт пересканирование и загрузка; (2) работа с ЗАДАЧЕЙ как
 * с объектом сервиса — создать, опросить до терминального статуса, отменить,
 * показать в истории. Вторая часть выросла в отдельный слой и упёрлась в страж
 * структуры R628 (tests/structure_ratchet_r628_test.go: боевой файл >= 1500
 * непустых строк запрещён). Здесь — ровно она.
 *
 * ЧТО ЗДЕСЬ: транспорт задач (POST/GET/DELETE /api/reexport/jobs*), ожидание
 * терминального статуса, поллинг истории, таблица задач с прогрессом и итогом,
 * человеческие подписи статусов и стадий, разбор отказа задачи (код + текст
 * сервиса + последняя строка лога) и отрисовка итогов шагов проверки и
 * нормализации (это вид ЗАДАЧИ, а не вид шага).
 *
 * ЧЕГО ЗДЕСЬ НЕТ: пробы модуля, разметки блока, выбора модели и вердикта,
 * запуска шагов (кнопок) и проверки загрузки — это главный файл
 * (js/modules/gguf-reexport.js), он же точка входа блока.
 *
 * ПОЧЕМУ ФАБРИКА. Слой берёт состояние и помощники у главного модуля
 * (state, host, requestJSON, authHeaders, t/esc, formatBytes/formatDuration,
 * currentModel, onServiceGone) — список фиксирован контрактом фабрики и
 * проверяется scripts/webui_ctx_contract_check.js. Своего состояния у слоя нет:
 * таблица задач живёт в общем state блока, иначе два счётчика «что сейчас
 * выполняется» разошлись бы.
 *
 * ЕСЛИ ФАЙЛ НЕ ЗАГРУЖЕН. Главный модуль не падает: шаги модели/вердикта и
 * проверки загрузки работают, а проверка и нормализация честно говорят, чего не
 * хватает (reexport.jobs_module_missing), плюс предупреждение в консоль.
 */
(function () {
    'use strict';

    // Поллинг таблицы задач: 2 с. Работа идёт минутами, чаще незачем; реже —
    // прогресс выглядит замершим.
    var POLL_INTERVAL_MS = 2000;

    /**
     * GgufReexportJobs — фабрика слоя задач.
     *
     * @param {object} ctx — {state, host, API, requestJSON, authHeaders, esc,
     *   t, showError, formatBytes, formatDuration, currentModel, clearBlock}
     * @returns {object} транспорт задач, отрисовка их итогов, таблица и поллинг
     */
    window.GgufReexportJobs = function (ctx) {
        var state = ctx.state;
        var host = ctx.host;
        var API = ctx.API;
        var requestJSON = ctx.requestJSON;
        var authHeaders = ctx.authHeaders;
        var esc = ctx.esc;
        var t = ctx.t;
        var showError = ctx.showError;
        var formatBytes = ctx.formatBytes;
        var formatDuration = ctx.formatDuration;
        var currentModel = ctx.currentModel;
        var clearBlock = ctx.clearBlock;
    // =====================================================================
    // Подписи: статусы задач, стадии и причина отказа
    // =====================================================================

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

    /**
     * phaseLabel — стадия работы из API прогресса (`progress.phase`).
     *
     * Стадии приходят от сервиса (py/reexport_job.py: queued/start/plan/copy/
     * verify/done). Показываем их словами, а не сырым словом протокола: «копирование»
     * понятнее, чем «copy», и это же слово оператор ищет в логе задачи.
     */
    function phaseLabel(phase) {
        switch (phase) {
        case 'queued': return t('reexport.phase_queued');
        case 'start': return t('reexport.phase_start');
        case 'plan': return t('reexport.phase_plan');
        case 'copy': return t('reexport.phase_copy');
        case 'verify': return t('reexport.phase_verify');
        case 'done': return t('reexport.phase_done');
        default: return String(phase || '');
        }
    }

    /**
     * jobErrorText — причина отказа задачи: ярлык кода + текст сервиса + лог.
     *
     * ПОЧЕМУ ЛОГ ЗАДАЧИ, А НЕ ТОЛЬКО ПОЛЕ error. runner.go переводит код возврата
     * драйвера в общую формулировку («файл не подходит под патч», «мало места»),
     * а КОНКРЕТНАЯ причина остаётся последней строкой лога задачи: «ожидался
     * general.architecture=inkling2, получено 'qwen3'» или «block_count=24 не равно
     * числу визитов 48». Оператору нужна именно она, иначе отказ неотличим от
     * «сервис сломался». Берём последнюю строку как есть (это факт от сервиса, а не
     * наш разбор), и только если она не повторяет message.
     */
    function jobErrorText(err, job) {
        if (!err) return '';
        var label = '';
        switch (err.code) {
        case 'model_not_patchable': label = t('reexport.err_model_not_patchable'); break;
        case 'insufficient_space': label = t('reexport.err_insufficient_space'); break;
        case 'dst_exists': label = t('reexport.err_dst_exists'); break;
        case 'input_missing': label = t('reexport.err_input_missing'); break;
        case 'canceled': label = t('reexport.err_canceled'); break;
        case 'interrupted': label = t('reexport.err_interrupted'); break;
        case 'target_unavailable': label = t('reexport.err_target_unavailable'); break;
        case 'converter_failed': label = t('reexport.err_converter_failed'); break;
        default: label = '';
        }
        var parts = [];
        if (err.code) parts.push(t('reexport.err_code') + ': ' + err.code);
        if (label) parts.push(label);
        if (err.message) parts.push(err.message);
        var lastLine = '';
        var tail = (job && job.log_tail) || [];
        for (var i = tail.length - 1; i >= 0; i--) {
            if (String(tail[i] || '').trim()) { lastLine = String(tail[i]).trim(); break; }
        }
        if (lastLine && lastLine !== err.message) {
            parts.push(t('reexport.err_log_tail') + ': ' + lastLine);
        }
        return parts.join(' — ');
    }

    // =====================================================================
    // Транспорт задачи: создать, дождаться, отменить
    // =====================================================================

    function isTerminal(status) {
        return status === 'succeeded' || status === 'failed' ||
            status === 'canceled' || status === 'interrupted';
    }

    /** createJob — POST задачи; ошибку сервиса показываем как есть. */
    function createJob(body) {
        return requestJSON(API + '/jobs', {
            method: 'POST',
            headers: authHeaders({ 'Content-Type': 'application/json' }),
            credentials: 'same-origin',
            timeoutMs: 0,
            body: JSON.stringify(body)
        }).then(function (job) {
            state.lastJobId = job && job.id ? job.id : '';
            loadJobs();
            return job;
        });
    }

    /**
     * waitJob — ожидание терминального статуса задачи с обновлением экрана.
     *
     * Опрос идёт по GET /api/reexport/jobs/{id}: задача живёт в сервисе и пишется
     * на диск, поэтому перезагрузка страницы не теряет прогресс — при повторном
     * входе шаг просто спросит статус по сохранённому id.
     */
    function waitJob(id, timerKey, onUpdate) {
        function tick() {
            return requestJSON(API + '/jobs/' + encodeURIComponent(id), {
                method: 'GET', headers: authHeaders(), credentials: 'same-origin', timeoutMs: 15000
            }).then(function (job) {
                if (onUpdate) onUpdate(job);
                if (isTerminal(job.status)) return job;
                return new Promise(function (resolve, reject) {
                    state[timerKey] = setTimeout(function () {
                        state[timerKey] = null;
                        tick().then(resolve, reject);
                    }, POLL_INTERVAL_MS);
                });
            });
        }
        return tick();
    }

    /** cancelById — DELETE задачи: убивает процесс конвертера и убирает недописанный файл. */
    function cancelById(id) {
        return requestJSON(API + '/jobs/' + encodeURIComponent(id), {
            method: 'DELETE',
            headers: authHeaders(),
            credentials: 'same-origin',
            timeoutMs: 15000
        });
    }

    // =====================================================================
    // Итоги шагов проверки и нормализации (это вид ЗАДАЧИ, не вид шага)
    // =====================================================================

    function renderInspect(el) {
        var status = el.querySelector('#rxInspectStatus');
        var result = el.querySelector('#rxInspectResult');
        if (!status || !result) return;
        var st = state.inspect;
        if (!st) {
            status.textContent = '';
            result.innerHTML = '';
            return;
        }
        var bits = [];
        if (st.status === 'running') bits.push(t('reexport.inspect_running'));
        else if (st.status === 'succeeded') bits.push(t('reexport.inspect_done'));
        else bits.push(t('reexport.inspect_failed'));
        if (st.jobId) bits.push(st.jobId);
        if (st.error) bits.push(st.error);
        status.textContent = bits.join(' · ');
        result.innerHTML = inspectResultHTML(st);
    }

    /** inspectResultHTML — факты файла + план изменений (то, что сделает normalize). */
    function inspectResultHTML(st) {
        var job = st && st.job;
        if (!job) return '';
        if (job.error && job.error.message) {
            return '<div class="rx-error-text" style="color:var(--danger, #ef4444);">' +
                esc(jobErrorText(job.error, job)) + '</div>';
        }
        var facts = (job.result && job.result.facts) || {};
        var model = currentModel() || {};
        var rows = [];
        var arch = facts.arch || model.arch || '';
        if (arch) rows.push([t('reexport.fact_arch'), arch]);
        if (facts.block_count || model.block_count) {
            rows.push([t('reexport.fact_block_count'), facts.block_count || model.block_count]);
        }
        if (facts.physical_blocks || model.physical_blocks) {
            rows.push([t('reexport.fact_physical'), facts.physical_blocks || model.physical_blocks]);
        }
        if (facts.visits) rows.push([t('reexport.fact_visits'), facts.visits]);
        if (facts.core && facts.core.length === 2) {
            rows.push([t('reexport.fact_loop'), facts.core[0] + '..' + (facts.core[1] - 1)]);
        }
        if (facts.passes) rows.push([t('reexport.fact_passes'), facts.passes]);
        if (facts.gate_rank) rows.push([t('reexport.fact_gate_rank'), facts.gate_rank]);
        if (facts.tensors) rows.push([t('reexport.fact_tensors'), facts.tensors]);
        // Наличие loop-ключей и контроллеров: сведения из шапки файла (Go-парсер),
        // а не пересказ лога задачи.
        rows.push([t('reexport.fact_loop_keys'), model.has_loop_keys ? t('reexport.yes') : t('reexport.no')]);
        rows.push([t('reexport.fact_query_matrices'), model.query_matrices ? t('reexport.yes') : t('reexport.no')]);
        if (typeof model.msg_query_tensors === 'number') {
            rows.push([t('reexport.fact_msg_query'), model.msg_query_tensors]);
        }
        if (facts.msg_query) rows.push([t('reexport.fact_msg_query_future'), facts.msg_query]);

        var html = '';
        if (rows.length) {
            html += '<div class="rx-facts" style="display:flex;gap:14px;flex-wrap:wrap;margin:6px 0;">' +
                rows.map(function (r) {
                    return '<span><span style="color:var(--text-muted);">' + esc(r[0]) + ':</span> ' +
                        esc(r[1]) + '</span>';
                }).join('') + '</div>';
        }
        var changes = (job.result && job.result.changes) || [];
        if (changes.length) {
            html += '<div style="font-size:var(--font-size-sm);margin-top:6px;">' +
                esc(t('reexport.plan_title')) + '</div><ul class="rx-plan">' +
                changes.map(function (c) { return '<li>' + esc(c) + '</li>'; }).join('') + '</ul>';
        } else {
            html += '<div style="font-size:var(--font-size-sm);color:var(--text-muted);">' +
                esc(t('reexport.plan_empty')) + '</div>';
        }
        if (job.result) {
            var fuseOn = !!(job.options && job.options.fuse_loop_queries);
            if (fuseOn && facts.msg_query) {
                html += '<div style="font-size:var(--font-size-xs);color:var(--text-muted);">' +
                    esc(t('reexport.plan_fuse')) + '</div>';
            } else if (model.msg_query_tensors) {
                html += '<div style="font-size:var(--font-size-xs);color:var(--text-muted);">' +
                    esc(t('reexport.plan_no_fuse')) + '</div>';
            }
            if (job.result.verify && job.result.verify.dry_run) {
                html += '<div style="font-size:var(--font-size-xs);color:var(--text-muted);">' +
                    esc(t('reexport.dry_run_result')) + '</div>';
            }
        }
        return html;
    }

    function renderNormalize(el) {
        var status = el.querySelector('#rxNormalizeStatus');
        var progress = el.querySelector('#rxNormalizeProgress');
        var result = el.querySelector('#rxNormalizeResult');
        var cancelBtn = el.querySelector('#rxCancelJob');
        if (!status || !progress || !result) return;
        var st = state.normalize;
        if (!st) {
            status.textContent = '';
            progress.innerHTML = '';
            result.innerHTML = '';
            if (cancelBtn) cancelBtn.style.display = 'none';
            return;
        }
        var bits = [];
        if (st.status === 'running') bits.push(t('reexport.normalize_running'));
        else if (st.status === 'succeeded') bits.push(t('reexport.normalize_done'));
        else if (st.status === 'skipped') bits.push(t('reexport.normalize_already'));
        else if (st.status === 'canceled') bits.push(t('reexport.normalize_canceled'));
        else bits.push(t('reexport.normalize_failed'));
        if (st.message && st.status !== 'skipped') bits.push(st.message);
        if (st.jobId) bits.push(st.jobId);
        if (st.error) bits.push(st.error);
        status.textContent = bits.join(' · ');

        var job = st.job;
        var active = !!(job && (job.status === 'running' || job.status === 'queued'));
        if (cancelBtn) cancelBtn.style.display = active ? '' : 'none';
        progress.innerHTML = active ? progressHTML(job) : '';
        result.innerHTML = normalizeResultHTML(job);
    }

    /** progressHTML — стадия, процент и объём (данные сервиса, не свои счётчики). */
    function progressHTML(job) {
        var p = job.progress || {};
        var percent = Math.max(0, Math.min(100, Number(p.percent) || 0));
        var bar = '<div style="height:6px;border-radius:3px;background:var(--bg-secondary);overflow:hidden;max-width:420px;">' +
            '<div style="height:100%;width:' + percent.toFixed(1) + '%;background:var(--accent, #3b82f6);"></div></div>';
        var text = phaseLabel(p.phase) + ' · ' + percent.toFixed(1) + '%';
        if (p.bytes_total) {
            text += ' · ' + formatBytes(p.bytes_done || 0) + ' / ' + formatBytes(p.bytes_total);
        }
        return bar + '<div style="font-size:var(--font-size-xs);color:var(--text-muted);margin-top:4px;">' +
            esc(text) + '</div>';
    }

    /** normalizeResultHTML — итог: файл, размер, архитектура, block_count, время. */
    function normalizeResultHTML(job) {
        if (!job) return '';
        if (job.error && job.error.message) {
            return '<div class="rx-error-text" style="color:var(--danger, #ef4444);">' +
                esc(jobErrorText(job.error, job)) + '</div>';
        }
        var r = job.result;
        if (!r) return '';
        var verify = r.verify || {};
        var facts = r.facts || {};
        var rows = [];
        if (job.out_path) rows.push([t('reexport.result_path'), job.out_path]);
        if (r.dst_bytes) rows.push([t('reexport.result_size'), formatBytes(r.dst_bytes)]);
        var arch = verify.arch || facts.arch || '';
        if (arch) rows.push([t('reexport.fact_arch'), arch]);
        var bc = verify.block_count || facts.block_count || '';
        if (bc) rows.push([t('reexport.fact_block_count'), bc]);
        var pb = facts.physical_blocks || '';
        if (pb) rows.push([t('reexport.fact_physical'), pb]);
        if (r.duration_ms) rows.push([t('reexport.result_duration'), formatDuration(r.duration_ms)]);
        if (!rows.length) return '';
        return '<div style="font-size:var(--font-size-sm);margin-top:6px;">' + esc(t('reexport.result_title')) + '</div>' +
            '<div class="rx-facts" style="display:flex;gap:14px;flex-wrap:wrap;">' +
            rows.map(function (row) {
                return '<span><span style="color:var(--text-muted);">' + esc(row[0]) + ':</span> ' + esc(row[1]) + '</span>';
            }).join('') + '</div>';
    }

    // =====================================================================
    // История задач: список, таблица, поллинг
    // =====================================================================

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
            startJobsPolling();
        } else {
            stopJobsPolling();
        }
    }

    function startJobsPolling() {
        if (state.pollTimer) return;
        state.pollTimer = setTimeout(function () {
            state.pollTimer = null;
            if (typeof document !== 'undefined' && document.hidden) {
                // Вкладка не видна — не тратим запросы; вернёмся при следующем
                // действии (кнопка «Обновить» или возврат на страницу).
                startJobsPolling();
                return;
            }
            loadJobs();
        }, POLL_INTERVAL_MS);
    }


    /**
     * stopJobsPolling — останавливает ТОЛЬКО поллинг таблицы задач.
     *
     * ЗАЧЕМ РАЗДЕЛЕНИЕ (живой дефект D11, поймано прогоном в браузере). Раньше
     * «остановить поллинг» гасило ВСЕ таймеры модуля, включая ожидание текущей
     * задачи (шаг «Проверить»/«Нормализовать»). Список задач перестаёт поллиться
     * ровно в тот момент, когда активных задач не осталось, — то есть СРАЗУ после
     * завершения задачи. Гонка: следующий опрос статуса задачи ещё не успел
     * случиться, а его таймер уже погашен списком, поэтому шаг навсегда оставался
     * «проверка выполняется», хотя сервис уже отдал succeeded и итог приходил в
     * одном ответе. Теперь у каждого ожидания свой таймер, и чужое его не трогает.
     */
    function stopJobsPolling() {
        if (state.pollTimer) {
            clearTimeout(state.pollTimer);
            state.pollTimer = null;
        }
    }

    /** progressCell — прогресс-бар + проценты + стадия + объём. */
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
        if (p.phase) text += ' · ' + esc(phaseLabel(p.phase));
        return bar + '<div style="font-size:var(--font-size-xs);color:var(--text-muted);margin-top:4px;">' + text + '</div>';
    }

    /** resultCell — итог: время, размеры, причина отказа. */
    function resultCell(job) {
        if (job.error && job.error.message) {
            return '<span style="color:var(--danger, #ef4444);">' + esc(jobErrorText(job.error, job)) + '</span>';
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

    /** cancelJob — DELETE: процесс конвертера убивается, частичный файл удаляется. */
    function cancelJob(el, id) {
        if (!id) return;
        cancelById(id).then(function () {
            return loadJobs();
        }).catch(function (err) {
            showError(el, t('reexport.cancel_failed') + ': ' + ((err && err.message) ? err.message : String(err)));
        });
    }

    /**
     * bind — обработчик кнопки отмены в таблице задач (делегирование).
     *
     * Живёт здесь, а не в главном модуле: строки таблицы рисует этот слой, и
     * знать про их разметку (data-cancel-job) должен он же. Главный модуль
     * только спрашивает загружен ли слой.
     */
    function bind(el) {
        var tbody = el.querySelector('#rxJobsBody');
        if (tbody) {
            tbody.addEventListener('click', function (ev) {
                var btn = ev && ev.target && ev.target.closest ? ev.target.closest('[data-cancel-job]') : null;
                if (!btn) return;
                cancelJob(el, btn.getAttribute('data-cancel-job'));
            });
        }
    }

    return {
        statusLabel: statusLabel,
        phaseLabel: phaseLabel,
        jobErrorText: jobErrorText,
        createJob: createJob,
        waitJob: waitJob,
        cancelJobById: cancelById,
        renderInspect: renderInspect,
        renderNormalize: renderNormalize,
        loadJobs: loadJobs,
        schedulePolling: schedulePolling,
        stopJobsPolling: stopJobsPolling,
        renderJobs: renderJobs,
        bind: bind
    };
};
})();
