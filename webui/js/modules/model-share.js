// model-share.js — R89 (2026-10-08): «Поделиться моделью» — перенос модели с
// одного бэкенда на другие.
//
// ЗАЧЕМ. В кластере несколько машин, и модель обычно лежит только на одной.
// Раньше её можно было получить на второй машине лишь повторной загрузкой из
// HuggingFace (интернет, токен, время, риск другой ревизии). Балансер умеет
// стримить файл из источника в приёмник (POST /api/v1/models/share), а этот
// модуль — его UI: выбор целей, запуск, прогресс по каждой цели и отмена.
//
// Контракт балансера:
//   POST /api/v1/models/share {source, model, targets[], overwrite} → 202 {id, targets[]}
//   GET  /api/v1/models/share/{id}                                 → состояние и прогресс
//   POST /api/v1/models/share/{id}/cancel
(function () {
    'use strict';

    var POLL_MS = 1000;
    var MODAL_ID = 'modelShareModal';

    // ============================================================
    // pure-слой: всё, что можно проверить без DOM (см. model-share.test.js)
    // ============================================================

    /** kindOf — группа типа бэкенда: 'image' | 'text' | ''. */
    function kindOf(backend) {
        var ty = String((backend && (backend.type || backend.backendType || backend.backend_type)) || '').toLowerCase();
        if (ty === 'image_cpp' || ty === 'image') return 'image';
        if (ty === 'llama_cpp' || ty === 'ollama' || ty === 'llamacpp') return 'text';
        return '';
    }

    /**
     * candidateBackends — куда можно перенести модель: тот же тип, что у
     * источника, и это не сам источник.
     *
     * ПОЧЕМУ ПО ТИПУ: у image-воркера bundle-каталог, у текстового — файл .gguf;
     * перенос между разными типами невозможен технически, и предлагать такой
     * выбор значило бы обещать невыполнимое.
     */
    function candidateBackends(all, sourceId, sourceKind) {
        var list = all || [];
        var kind = sourceKind || '';
        if (!kind) {
            // Тип источника известен не всегда (вызывающий может передать только
            // id) — берём его из самого списка бэкендов, иначе фильтр по типу
            // молча не применился бы и в цели попали бы несовместимые узлы.
            for (var i = 0; i < list.length; i++) {
                if (list[i] && list[i].id === sourceId) { kind = kindOf(list[i]); break; }
            }
        }
        return list.filter(function (b) {
            if (!b || !b.id || b.id === sourceId) return false;
            return kind ? kindOf(b) === kind : true;
        });
    }

    /** buildShareRequest — тело POST /api/v1/models/share. */
    function buildShareRequest(sourceId, model, targetIds, overwrite) {
        return {
            source: String(sourceId || ''),
            model: String(model || ''),
            targets: (targetIds || []).slice(),
            overwrite: !!overwrite
        };
    }

    /** formatBytes — размер по-человечески (для строки прогресса). */
    function formatBytes(n) {
        n = Number(n || 0);
        if (!isFinite(n) || n <= 0) return '0 B';
        var units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
        var i = 0;
        while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
        return (i === 0 ? n.toFixed(0) : n.toFixed(n >= 100 ? 0 : 1)) + ' ' + units[i];
    }

    /** formatSpeed — скорость в МиБ/с. */
    function formatSpeed(bps) {
        var v = Number(bps || 0);
        if (!isFinite(v) || v <= 0) return '';
        return (v / (1024 * 1024)).toFixed(1) + ' MiB/s';
    }

    /**
     * targetView — что показывать в строке цели.
     *
     * ВАЖНО: 100% ставим ТОЛЬКО когда цель действительно завершена. Байты при
     * переносе могут слегка обгонять «полезную нагрузку» (tar-заголовки), и
     * прогресс-бар, показывающий 100% на ещё идущем переносе, читается как
     * «зависло».
     */
    function targetView(target) {
        var state = String((target && target.state) || 'pending');
        var pct = Number((target && target.percent) || 0);
        if (state === 'done' || state === 'skipped') pct = 100;
        else if (pct > 99) pct = 99;
        if (!isFinite(pct) || pct < 0) pct = 0;
        var out = {
            percent: pct,
            state: state,
            label: stateLabel(state),
            detail: ''
        };
        if (state === 'running' || state === 'done') {
            var bytes = Number((target && target.bytes) || 0);
            var total = Number((target && target.total) || 0);
            out.detail = total > 0 ? (formatBytes(bytes) + ' / ' + formatBytes(total)) : formatBytes(bytes);
            var speed = formatSpeed(target && target.speedBps);
            if (speed && state === 'running') out.detail += ' · ' + speed;
        }
        if (target && target.note) out.detail = target.note;
        if (target && target.error) out.detail = target.error;
        return out;
    }

    /** stateLabel — человеческое имя состояния цели. */
    function stateLabel(state) {
        switch (String(state || '')) {
            case 'pending': return t('share.pending', 'в очереди');
            case 'running': return t('share.running', 'копируется');
            case 'done': return t('share.done', 'готово');
            case 'skipped': return t('share.skipped', 'пропущено');
            case 'failed': return t('share.failed', 'ошибка');
            default: return String(state || '');
        }
    }

    /** jobFinished — задание перестало быть активным. */
    function jobFinished(job) {
        var s = String((job && job.state) || '');
        return s === 'done' || s === 'partial' || s === 'failed' || s === 'canceled';
    }

    // ============================================================
    // окружение
    // ============================================================

    function t(key, fallback, vars) {
        if (window.I18N && typeof window.I18N.t === 'function') {
            var v = vars ? window.I18N.t(key, vars) : window.I18N.t(key);
            if (v && v !== key) return v;
        }
        var s = fallback || key;
        if (vars) {
            Object.keys(vars).forEach(function (k) { s = String(s).replace('{' + k + '}', vars[k]); });
        }
        return s;
    }

    function esc(s) {
        if (window.Utils && typeof window.Utils.escapeHtml === 'function') return window.Utils.escapeHtml(String(s));
        return String(s).replace(/[&<>"']/g, function (c) {
            return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
        });
    }

    function apiBase() {
        var cfg = window.WEBUI_CONFIG || {};
        var base = cfg.API_BASE_URL || cfg.API_BASE || '';
        return String(base).replace(/\/+$/, '');
    }

    function apiToken() {
        var cfg = window.WEBUI_CONFIG || {};
        var key = cfg.API_TOKEN || '';
        if (!key) { try { key = localStorage.getItem('apiToken') || ''; } catch (e) { key = ''; } }
        return String(key);
    }

    function request(url, opts) {
        opts = opts || {};
        var init = { method: opts.method || 'GET', headers: { 'Content-Type': 'application/json' } };
        var tok = apiToken();
        if (tok) init.headers['X-API-Token'] = tok;
        if (opts.body !== undefined) init.body = JSON.stringify(opts.body);
        return fetch(url, init).then(function (resp) {
            return resp.text().then(function (text) {
                var parsed = null;
                if (text) { try { parsed = JSON.parse(text); } catch (e) { parsed = null; } }
                if (!resp.ok) {
                    var msg = (parsed && (parsed.error || parsed.message)) || ('HTTP ' + resp.status);
                    var err = new Error(String(msg));
                    err.status = resp.status;
                    err.body = parsed;
                    if (parsed && parsed.hint) err.hint = parsed.hint;
                    throw err;
                }
                return parsed;
            });
        });
    }

    // ============================================================
    // модальное окно
    // ============================================================

    var state = { job: null, timer: null, targets: [], opts: null };

    function byId(id) {
        return (typeof document !== 'undefined' && document.getElementById) ? document.getElementById(id) : null;
    }

    function close() {
        if (state.timer) { clearInterval(state.timer); state.timer = null; }
        var modal = byId(MODAL_ID);
        if (modal && modal.parentNode) modal.parentNode.removeChild(modal);
        state.job = null;
        state.opts = null;
    }

    function ensureModal() {
        var existing = byId(MODAL_ID);
        if (existing && existing.parentNode) existing.parentNode.removeChild(existing);
        var modal = document.createElement('div');
        modal.id = MODAL_ID;
        modal.className = 'modal active';
        modal.style.cssText = 'position:fixed;inset:0;z-index:9999;display:flex;align-items:center;justify-content:center;' +
            'background:rgba(0,0,0,0.45);padding:16px;';
        var box = document.createElement('div');
        box.className = 'modal-content';
        box.style.cssText = 'background:var(--bg-card,#fff);color:var(--text,#111);border-radius:10px;' +
            'max-width:640px;width:100%;max-height:85vh;overflow:auto;padding:18px;box-shadow:0 10px 40px rgba(0,0,0,0.35);';
        box.id = MODAL_ID + 'Body';
        modal.appendChild(box);
        modal.addEventListener('click', function (ev) { if (ev.target === modal) close(); });
        document.body.appendChild(modal);
        return box;
    }

    function renderShell(box, opts) {
        box.innerHTML =
            '<div style="display:flex;justify-content:space-between;align-items:center;gap:12px;">' +
                '<h3 style="margin:0;font-size:16px;">' + esc(t('share.title', 'Поделиться моделью')) + '</h3>' +
                '<button class="btn btn-sm btn-secondary" id="shareClose">' + esc(t('common.close', 'Закрыть')) + '</button>' +
            '</div>' +
            '<div style="margin-top:10px;font-size:13px;color:var(--text-muted,#666);">' +
                esc(t('share.source_hint', 'Источник')) + ': <strong>' + esc(opts.backendId) + '</strong> · ' +
                '<strong>' + esc(opts.model) + '</strong>' +
            '</div>' +
            '<div id="shareBody" style="margin-top:14px;"></div>';
        var closeBtn = byId('shareClose');
        if (closeBtn) closeBtn.addEventListener('click', close);
    }

    function renderTargetPicker(box, candidates) {
        var body = byId('shareBody');
        if (!body) return;
        if (!candidates.length) {
            body.innerHTML = '<div style="font-size:13px;">' +
                esc(t('share.no_targets', 'Других бэкендов того же типа в кластере нет — переносить некуда.')) +
                '</div>';
            return;
        }
        var rows = candidates.map(function (b) {
            var checked = candidates.length === 1 ? ' checked' : '';
            return '<label style="display:flex;gap:8px;align-items:center;padding:6px 0;font-size:13px;">' +
                '<input type="checkbox" class="share-target-cb" value="' + esc(b.id) + '"' + checked + '>' +
                '<span><strong>' + esc(b.id) + '</strong>' +
                (b.host ? ' <span style="color:var(--text-muted,#666);">(' + esc(b.host) + ')</span>' : '') +
                '</span></label>';
        }).join('');
        body.innerHTML =
            '<div style="font-size:13px;margin-bottom:6px;">' + esc(t('share.pick_targets', 'Куда перенести:')) + '</div>' +
            rows +
            '<label style="display:flex;gap:8px;align-items:center;margin-top:10px;font-size:13px;">' +
                '<input type="checkbox" id="shareOverwrite">' +
                '<span>' + esc(t('share.overwrite', 'Перезаписать, если модель уже есть на приёмнике')) + '</span>' +
            '</label>' +
            '<div id="shareError" style="color:var(--danger,#c33);font-size:13px;margin-top:8px;"></div>' +
            '<div style="display:flex;gap:8px;margin-top:14px;">' +
                '<button class="btn btn-primary" id="shareStart">' + esc(t('share.start', 'Начать перенос')) + '</button>' +
            '</div>';
        var start = byId('shareStart');
        if (start) start.addEventListener('click', function () { submit(box); });
    }

    function selectedTargets() {
        var out = [];
        var nodes = document.querySelectorAll ? document.querySelectorAll('.share-target-cb') : [];
        for (var i = 0; i < nodes.length; i++) {
            if (nodes[i].checked) out.push(nodes[i].value);
        }
        return out;
    }

    function renderError(msg) {
        var box = byId('shareError');
        if (box) box.textContent = String(msg || '');
    }

    function submit(box) {
        var opts = state.opts || {};
        var targets = selectedTargets();
        if (!targets.length) {
            renderError(t('share.need_target', 'Выберите хотя бы один бэкенд-приёмник'));
            return;
        }
        var overwrite = !!(byId('shareOverwrite') && byId('shareOverwrite').checked);
        renderError('');
        var body = byId('shareBody');
        if (body) body.innerHTML = '<div style="font-size:13px;"><i class="fas fa-circle-notch fa-spin"></i> ' +
            esc(t('share.starting', 'Запускаю перенос…')) + '</div>';
        request(apiBase() + '/api/v1/models/share', {
            method: 'POST',
            body: buildShareRequest(opts.backendId, opts.model, targets, overwrite)
        }).then(function (job) {
            state.job = job;
            renderJob(box, job);
            startPolling(box);
        }).catch(function (err) {
            renderTargetPicker(box, state.targets);
            renderError((err && err.message) || String(err));
        });
    }

    function renderJob(box, job) {
        var body = byId('shareBody');
        if (!body) return;
        var targets = (job && job.targets) || [];
        var rows = targets.map(function (tg) {
            var v = targetView(tg);
            return '<div style="padding:8px 0;border-top:1px solid var(--border,#eee);">' +
                '<div style="display:flex;justify-content:space-between;font-size:13px;gap:8px;">' +
                    '<span><strong>' + esc(tg.backendId) + '</strong> — ' + esc(v.label) + '</span>' +
                    '<span style="color:var(--text-muted,#666);">' + esc(v.detail || '') + '</span>' +
                '</div>' +
                '<div style="height:6px;background:var(--bg-secondary,#eee);border-radius:3px;margin-top:6px;overflow:hidden;">' +
                    '<div style="height:100%;width:' + v.percent.toFixed(1) + '%;background:var(--accent,#3a7);"></div>' +
                '</div>' +
            '</div>';
        }).join('');
        var finished = jobFinished(job);
        var head = '<div style="font-size:13px;">' +
            esc(t('share.job_state', 'Состояние задания')) + ': <strong>' + esc(stateLabel(job.state)) + '</strong>' +
            (job.error ? '<div style="color:var(--danger,#c33);margin-top:6px;">' + esc(job.error) + '</div>' : '') +
            '</div>';
        var footer = finished
            ? '<div style="margin-top:12px;font-size:13px;color:var(--text-muted,#666);">' +
                esc(t('share.finished_hint', 'Готово. Обновите страницу бэкендов, чтобы увидеть модель на приёмнике.')) +
              '</div>'
            : '<div style="margin-top:12px;"><button class="btn btn-sm btn-secondary" id="shareCancel">' +
                esc(t('common.cancel', 'Отменить')) + '</button></div>';
        body.innerHTML = head + rows + footer;
        var cancel = byId('shareCancel');
        if (cancel) cancel.addEventListener('click', cancelJob);
    }

    function cancelJob() {
        var job = state.job;
        if (!job || !job.id) return;
        request(apiBase() + '/api/v1/models/share/' + encodeURIComponent(job.id) + '/cancel', { method: 'POST' })
            .then(function (updated) { state.job = updated; renderJob(byId(MODAL_ID + 'Body'), updated); })
            .catch(function () { /* отмена — best effort */ });
    }

    function startPolling(box) {
        if (state.timer) clearInterval(state.timer);
        state.timer = setInterval(function () {
            var job = state.job;
            if (!job || !job.id) return;
            request(apiBase() + '/api/v1/models/share/' + encodeURIComponent(job.id))
                .then(function (updated) {
                    state.job = updated;
                    renderJob(box, updated);
                    if (jobFinished(updated)) {
                        clearInterval(state.timer);
                        state.timer = null;
                        // Список бэкендов/моделей изменился на приёмнике —
                        // подтягиваем данные, если страница это умеет.
                        try { if (window.App && window.App.fetchClusterState) window.App.fetchClusterState(); } catch (e) {}
                    }
                })
                .catch(function () { /* сеть моргнула — следующий тик догонит */ });
        }, POLL_MS);
    }

    /**
     * open — открыть окно переноса для конкретной модели.
     * @param {{backendId:string, model:string, kind?:string}} opts
     */
    function open(opts) {
        opts = opts || {};
        if (!opts.backendId || !opts.model) return Promise.resolve(false);
        state.opts = opts;
        var box = ensureModal();
        renderShell(box, opts);
        var body = byId('shareBody');
        if (body) body.innerHTML = '<div style="font-size:13px;"><i class="fas fa-circle-notch fa-spin"></i> ' +
            esc(t('share.loading_backends', 'Смотрю бэкенды кластера…')) + '</div>';
        return request(apiBase() + '/api/v1/backends')
            .then(function (data) {
                var all = (data && data.backends) || [];
                var source = null;
                for (var i = 0; i < all.length; i++) { if (all[i] && all[i].id === opts.backendId) { source = all[i]; break; } }
                var kind = opts.kind || kindOf(source);
                state.targets = candidateBackends(all, opts.backendId, kind);
                renderTargetPicker(box, state.targets);
                return true;
            })
            .catch(function (err) {
                var b = byId('shareBody');
                if (b) b.innerHTML = '<div style="font-size:13px;color:var(--danger,#c33);">' +
                    esc((err && err.message) || String(err)) + '</div>';
                return false;
            });
    }

    window.ModelShare = {
        open: open,
        close: close,
        pure: {
            kindOf: kindOf,
            candidateBackends: candidateBackends,
            buildShareRequest: buildShareRequest,
            formatBytes: formatBytes,
            formatSpeed: formatSpeed,
            targetView: targetView,
            stateLabel: stateLabel,
            jobFinished: jobFinished
        }
    };
})();
