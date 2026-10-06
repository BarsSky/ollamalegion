/**
 * image-resources-policy.js — R84 (2026-10-03): настройка политики
 * сосуществования image-генерации с текстовым инференсом из WebUI.
 *
 * ЗАЧЕМ. До этого политику (`balancing.image`: coexistence, резерв VRAM, таймауты,
 * «выключить гейт») можно было задать ТОЛЬКО в config/config.json, а в стенде этот
 * каталог смонтирован read-only — то есть правка требовала доступа к хосту и
 * перезапуска балансера. Env-переменных для этих полей в проекте нет. WebUI
 * показывал политику только для чтения (колонка «Сосуществование с текстом»).
 * Теперь те же значения правятся здесь: балансер хранит переопределение в
 * записываемом томе (/app/data/image-resources.json) и применяет его мгновенно.
 *
 * КОНТРАКТ (GET/PUT/DELETE /api/v1/image/resources): effective — что действует
 * сейчас, defaults — что лежит в config.json (для «Сбросить»), overridden —
 * сохранено ли переопределение из UI, limits — границы полей, policies — подсказки
 * по каждой политике (тексты приходят с сервера, чтобы UI не расходился с гейтом).
 *
 * Экспорт: window.ImageResourcesPolicy = { mount, render, tabIds, _actions, _state, pure }.
 */
(function () {
    'use strict';

    var TAB_IDS = ['overview'];
    var HOST_ID = 'imPolicyHost';
    var BADGE_ID = 'imPolicyBadge';

    var state = {
        ctx: null,
        loading: false,
        saving: false,
        error: '',
        data: null,
        mounted: false,
        bound: false
    };

    // =====================================================================
    // Pure helpers (покрыты node-тестом)
    // =====================================================================

    function escapeHtml(text) {
        if (text === null || text === undefined) return '';
        return String(text)
            .replace(/&/g, '&amp;')
            .replace(/</g, '&lt;')
            .replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;')
            .replace(/'/g, '&#39;');
    }

    /** policyHint — подсказка по политике из ответа сервера (или пустая строка). */
    function policyHint(data, value) {
        var list = (data && data.policies) || [];
        for (var i = 0; i < list.length; i++) {
            if (list[i] && list[i].value === value) return list[i].hint || '';
        }
        return '';
    }

    /**
     * formFromEffective — значения формы из ответа API.
     *
     * Defaults важны: сервер отдаёт effective с уже подставленными дефолтами
     * (пустая политика = exclusive), поэтому в форме показываем именно effective,
     * а «Сбросить» отправляет DELETE (возврат к config.json).
     */
    function formFromEffective(data) {
        var eff = (data && data.effective) || {};
        return {
            coexistence: String(eff.coexistence || 'exclusive'),
            vramHeadroomMb: Number(eff.vramHeadroomMb || 0),
            queueWaitTimeoutSec: Number(eff.queueWaitTimeoutSec || 0),
            exclusiveLockTimeoutSec: Number(eff.exclusiveLockTimeoutSec || 0),
            blockOnUnknownVramEstimate: !!eff.blockOnUnknownVramEstimate,
            gateDisabled: !!eff.gateDisabled
        };
    }

    /** validate — клиентская проверка ДО отправки (сервер проверит ещё раз). */
    function validate(form, limits) {
        limits = limits || {};
        if (form.coexistence !== 'exclusive' && form.coexistence !== 'offload' && form.coexistence !== 'dedicated') {
            return t('imagePolicy.err_policy', 'Неизвестная политика: {value}', { value: form.coexistence });
        }
        var maxHeadroom = Number(limits.maxVramHeadroomMb || 65536);
        if (!(form.vramHeadroomMb >= 0) || form.vramHeadroomMb > maxHeadroom) {
            return t('imagePolicy.err_headroom', 'Резерв VRAM: 0…{max} МБ', { max: maxHeadroom });
        }
        var maxWait = Number(limits.maxQueueWaitTimeoutSec || 3600);
        if (!(form.queueWaitTimeoutSec >= 0) || form.queueWaitTimeoutSec > maxWait) {
            return t('imagePolicy.err_wait', 'Ожидание GPU: 0…{max} с', { max: maxWait });
        }
        var maxFuse = Number(limits.maxExclusiveLockFuseSec || 86400);
        if (!(form.exclusiveLockTimeoutSec >= 0) || form.exclusiveLockTimeoutSec > maxFuse) {
            return t('imagePolicy.err_fuse', 'Предохранитель лока: 0…{max} с', { max: maxFuse });
        }
        return '';
    }

    /** warningFor — предупреждение к выбранной политике (пусто = нет). */
    function warningFor(form) {
        if (form.gateDisabled) {
            return t('imagePolicy.warn_gate_off',
                'Гейт VRAM выключен: генерация пойдёт даже при нехватке памяти на карте — это может вытеснить текстовую модель или привести к OOM.');
        }
        if (form.coexistence === 'offload') {
            return t('imagePolicy.warn_offload',
                'Политика offload разрешает совместную работу с текстом. Она безопасна, только если image-воркер действительно запущен с offload в RAM (--offload-to-cpu / --backend te=cpu): иначе возможен OOM.');
        }
        if (form.coexistence === 'dedicated') {
            return t('imagePolicy.warn_dedicated',
                'Политика dedicated снимает ограничения сосуществования. Выбирайте её, только если под image-бэкенд выделена отдельная GPU.');
        }
        return '';
    }

    /** summaryText — короткая строка «что действует» (для шапки формы). */
    function summaryText(data) {
        var f = formFromEffective(data);
        var parts = [f.coexistence];
        if (f.vramHeadroomMb > 0) parts.push('headroom ' + f.vramHeadroomMb + ' MB');
        if (f.queueWaitTimeoutSec > 0) parts.push('wait ' + f.queueWaitTimeoutSec + 's');
        if (f.exclusiveLockTimeoutSec > 0) parts.push('fuse ' + f.exclusiveLockTimeoutSec + 's');
        if (f.blockOnUnknownVramEstimate) parts.push('block on unknown');
        if (f.gateDisabled) parts.push('gate off');
        return parts.join(', ');
    }

    // =====================================================================
    // Окружение
    // =====================================================================

    function t(key, fallback, vars) {
        if (window.I18N && typeof window.I18N.t === 'function') {
            var v = vars ? window.I18N.t(key, vars) : window.I18N.t(key);
            if (v && v !== key) return v;
        }
        if (fallback && vars) {
            return String(fallback).replace(/\{(\w+)\}/g, function (m, k) {
                return Object.prototype.hasOwnProperty.call(vars, k) ? String(vars[k]) : m;
            });
        }
        return fallback || key;
    }

    function byId(id) {
        return (typeof document !== 'undefined' && document.getElementById) ? document.getElementById(id) : null;
    }

    function apiBase() {
        if (state.ctx && state.ctx.apiBase) return String(state.ctx.apiBase).replace(/\/$/, '');
        if (window.WEBUI_CONFIG && window.WEBUI_CONFIG.API_BASE) return String(window.WEBUI_CONFIG.API_BASE).replace(/\/$/, '');
        return '';
    }

    function authHeaders() {
        if (window.Api && typeof window.Api.getAuthHeaders === 'function') return window.Api.getAuthHeaders();
        var token = (window.WEBUI_CONFIG && window.WEBUI_CONFIG.API_TOKEN) || '';
        return { 'Content-Type': 'application/json', 'X-API-Token': token };
    }

    function request(path, opts) {
        opts = opts || {};
        var init = { method: opts.method || 'GET', headers: authHeaders() };
        if (opts.body !== undefined) init.body = JSON.stringify(opts.body);
        return fetch(apiBase() + path, init).then(function (resp) {
            return resp.text().then(function (text) {
                var parsed = null;
                if (text) { try { parsed = JSON.parse(text); } catch (e) { parsed = text; } }
                if (!resp.ok) {
                    var msg = (parsed && (parsed.error || parsed.message)) || ('HTTP ' + resp.status);
                    var err = new Error(msg);
                    err.status = resp.status;
                    throw err;
                }
                return parsed;
            });
        });
    }

    function toast(message, type) {
        if (window.Toast && typeof window.Toast.show === 'function') {
            window.Toast.show({ message: message, type: type || 'info' });
            return;
        }
        if (window.console) console.log('[image-policy] ' + message);
    }

    // =====================================================================
    // Рендер
    // =====================================================================

    function selectOptions(data, current) {
        var list = (data && data.policies) || [];
        if (!list.length) {
            list = [{ value: 'exclusive' }, { value: 'offload' }, { value: 'dedicated' }];
        }
        return list.map(function (p) {
            return '<option value="' + escapeHtml(p.value) + '"' + (p.value === current ? ' selected' : '') + '>' +
                escapeHtml(p.value) + '</option>';
        }).join('');
    }

    function render() {
        var host = byId(HOST_ID);
        if (!host) return false;
        var badge = byId(BADGE_ID);
        if (badge) badge.style.display = (state.data && state.data.overridden) ? '' : 'none';

        if (state.loading && !state.data) {
            host.innerHTML = '<div style="color:var(--text-muted);font-size:13px;">' +
                escapeHtml(t('common.loading', 'Loading...')) + '</div>';
            return true;
        }
        if (state.error && !state.data) {
            host.innerHTML = '<div style="color:var(--danger);font-size:13px;">' + escapeHtml(state.error) + '</div>';
            return true;
        }

        var f = formFromEffective(state.data);
        var limits = (state.data && state.data.limits) || {};
        var warn = warningFor(f);
        var hint = policyHint(state.data, f.coexistence);

        host.innerHTML =
            '<div style="font-size:12px;color:var(--text-muted);margin-bottom:10px;">' +
                escapeHtml(t('imagePolicy.current', 'Действует сейчас')) + ': <b>' + escapeHtml(summaryText(state.data)) + '</b>' +
                (state.data && state.data.path ? ' · ' + escapeHtml(t('imagePolicy.file', 'файл')) + ': <span style="font-family:monospace;">' + escapeHtml(state.data.path) + '</span>' : '') +
            '</div>' +
            '<div style="display:flex;gap:14px;flex-wrap:wrap;align-items:flex-end;">' +
                '<label style="display:flex;flex-direction:column;gap:4px;font-size:12px;color:var(--text-muted);">' +
                    escapeHtml(t('imagePolicy.coexistence', 'Сосуществование с текстом')) +
                    '<select class="sort-select" id="imPolicyCoexistence" style="min-width:200px;">' + selectOptions(state.data, f.coexistence) + '</select>' +
                '</label>' +
                '<label style="display:flex;flex-direction:column;gap:4px;font-size:12px;color:var(--text-muted);">' +
                    escapeHtml(t('imagePolicy.headroom', 'Резерв VRAM, МБ')) +
                    '<input type="number" class="form-control" id="imPolicyHeadroom" min="0" step="64" style="width:130px;" value="' + f.vramHeadroomMb + '">' +
                '</label>' +
                '<label style="display:flex;flex-direction:column;gap:4px;font-size:12px;color:var(--text-muted);">' +
                    escapeHtml(t('imagePolicy.wait', 'Ожидание GPU, с')) +
                    '<input type="number" class="form-control" id="imPolicyWait" min="0" step="1" style="width:130px;" value="' + f.queueWaitTimeoutSec + '">' +
                '</label>' +
                '<label style="display:flex;flex-direction:column;gap:4px;font-size:12px;color:var(--text-muted);">' +
                    escapeHtml(t('imagePolicy.fuse', 'Предохранитель лока, с')) +
                    '<input type="number" class="form-control" id="imPolicyFuse" min="0" step="1" style="width:150px;" value="' + f.exclusiveLockTimeoutSec + '">' +
                '</label>' +
            '</div>' +
            '<div style="display:flex;gap:18px;flex-wrap:wrap;margin-top:12px;font-size:12px;color:var(--text-muted);">' +
                '<label style="display:flex;gap:6px;align-items:center;cursor:pointer;">' +
                    '<input type="checkbox" id="imPolicyBlockUnknown"' + (f.blockOnUnknownVramEstimate ? ' checked' : '') + '>' +
                    escapeHtml(t('imagePolicy.block_unknown', 'Блокировать, если оценка VRAM неизвестна')) +
                '</label>' +
                '<label style="display:flex;gap:6px;align-items:center;cursor:pointer;">' +
                    '<input type="checkbox" id="imPolicyGateOff"' + (f.gateDisabled ? ' checked' : '') + '>' +
                    escapeHtml(t('imagePolicy.gate_off', 'Выключить гейт VRAM и лок')) +
                '</label>' +
            '</div>' +
            '<div id="imPolicyHint" style="margin-top:10px;font-size:12px;color:var(--text-muted);"' + (hint ? '' : ' hidden') + '>' +
                '<i class="fas fa-circle-info"></i> <span>' + escapeHtml(hint) + '</span></div>' +
            '<div id="imPolicyWarn" style="margin-top:10px;padding:8px 10px;border-left:3px solid var(--warning, #e0a030);background:var(--bg-secondary);border-radius:6px;font-size:12px;"' + (warn ? '' : ' hidden') + '>' +
                '<i class="fas fa-triangle-exclamation"></i> <span>' + escapeHtml(warn) + '</span></div>' +
            (state.error ? '<div style="margin-top:10px;color:var(--danger);font-size:12px;">' + escapeHtml(state.error) + '</div>' : '');
        return true;
    }

    /**
     * updateHints — обновить только подсказку и предупреждение по текущему выбору.
     *
     * ПОЧЕМУ НЕ render(): полная перерисовка формы на событие change сбрасывала
     * выбор оператора (форма собиралась из state.data, где ещё старое значение) —
     * на живом стенде «выбрал offload, нажал Сохранить» сохраняло exclusive.
     */
    function updateHints() {
        var host = byId(HOST_ID);
        if (!host || !state.data) return;
        var f = readForm();
        var hintNode = byId('imPolicyHint');
        if (hintNode) {
            var hint = policyHint(state.data, f.coexistence);
            hintNode.hidden = !hint;
            var hs = hintNode.querySelector ? hintNode.querySelector('span') : null;
            if (hs) hs.textContent = hint;
        }
        var warnNode = byId('imPolicyWarn');
        if (warnNode) {
            var warn = warningFor(f);
            warnNode.hidden = !warn;
            var ws = warnNode.querySelector ? warnNode.querySelector('span') : null;
            if (ws) ws.textContent = warn;
        }
    }

    function readForm() {
        var co = byId('imPolicyCoexistence');
        return {
            coexistence: co ? String(co.value || 'exclusive') : 'exclusive',
            vramHeadroomMb: Number((byId('imPolicyHeadroom') || {}).value || 0),
            queueWaitTimeoutSec: Number((byId('imPolicyWait') || {}).value || 0),
            exclusiveLockTimeoutSec: Number((byId('imPolicyFuse') || {}).value || 0),
            blockOnUnknownVramEstimate: !!(byId('imPolicyBlockUnknown') || {}).checked,
            gateDisabled: !!(byId('imPolicyGateOff') || {}).checked
        };
    }

    // =====================================================================
    // Действия
    // =====================================================================

    function load(force) {
        if (state.loading) return Promise.resolve(state.data);
        state.loading = true;
        if (force) state.data = null;
        render();
        return request('/api/v1/image/resources').then(function (data) {
            state.loading = false;
            state.error = '';
            state.data = data;
            render();
            return data;
        }).catch(function (e) {
            state.loading = false;
            state.error = (e && e.message) || String(e);
            render();
            return null;
        });
    }

    function save() {
        if (state.saving) return Promise.resolve(false);
        var form = readForm();
        var limits = (state.data && state.data.limits) || {};
        var err = validate(form, limits);
        if (err) {
            state.error = err;
            render();
            return Promise.resolve(false);
        }
        state.saving = true;
        state.error = '';
        return request('/api/v1/image/resources', { method: 'PUT', body: form }).then(function (data) {
            state.saving = false;
            state.data = data;
            render();
            toast(t('imagePolicy.saved', 'Политика сохранена и применена'), 'success');
            refreshPolicyColumn();
            return true;
        }).catch(function (e) {
            state.saving = false;
            state.error = (e && e.message) || String(e);
            render();
            toast(t('imagePolicy.save_failed', 'Не удалось сохранить политику') + ': ' + state.error, 'error');
            return false;
        });
    }

    function reset() {
        if (state.saving) return Promise.resolve(false);
        state.saving = true;
        state.error = '';
        return request('/api/v1/image/resources', { method: 'DELETE' }).then(function (data) {
            state.saving = false;
            state.data = data;
            render();
            toast(t('imagePolicy.reset_done', 'Возвращены встроенные значения'), 'success');
            refreshPolicyColumn();
            return true;
        }).catch(function (e) {
            state.saving = false;
            state.error = (e && e.message) || String(e);
            render();
            toast(t('imagePolicy.reset_failed', 'Не удалось сбросить политику') + ': ' + state.error, 'error');
            return false;
        });
    }

    /**
     * refreshPolicyColumn — обновить колонку «Сосуществование с текстом» в таблице
     * бэкендов: она читает /api/v1/metrics, и после сохранения там уже новые
     * значения (иначе оператор видел бы старое и решил, что не применилось).
     */
    function refreshPolicyColumn() {
        var page = window.ImageBackendsPage;
        if (!page || !page._actions) return;
        // Сначала именно loadPolicy(true): у колонки TTL кэш 15 с, и обычный
        // refresh() показал бы старое значение сразу после сохранения.
        if (typeof page._actions.loadPolicy === 'function') {
            try { page._actions.loadPolicy(true); return; } catch (e) { /* фолбэк ниже */ }
        }
        if (typeof page._actions.refresh === 'function') {
            try { page._actions.refresh(); } catch (e) { /* страница может быть не смонтирована */ }
        }
    }

    // =====================================================================
    // Монтирование
    // =====================================================================

    function mount() {
        if (state.bound) return true;
        state.bound = true;
        var saveBtn = byId('imPolicySaveBtn');
        if (saveBtn && saveBtn.addEventListener) saveBtn.addEventListener('click', function () { save(); });
        var resetBtn = byId('imPolicyResetBtn');
        if (resetBtn && resetBtn.addEventListener) resetBtn.addEventListener('click', function () { reset(); });
        var host = byId(HOST_ID);
        if (host && host.addEventListener) {
            // Подсказка и предупреждение зависят от выбранной политики — обновляем
            // их сразу, не дожидаясь «Сохранить».
            // Меняем только подсказку/предупреждение: полная перерисовка стёрла
            // бы выбор оператора.
            host.addEventListener('change', function () { updateHints(); });
        }
        return true;
    }

    function renderTab(ctx) {
        if (ctx) state.ctx = ctx;
        mount();
        var tab = (state.ctx && state.ctx.tab) || 'overview';
        if (TAB_IDS.indexOf(tab) === -1) return false;
        if (!state.data) load(true);
        else render();
        return true;
    }

    window.ImageResourcesPolicy = {
        mount: mount,
        render: renderTab,
        tabIds: TAB_IDS,
        _actions: {
            updateHints: updateHints,
            load: load,
            save: save,
            reset: reset,
            readForm: readForm
        },
        _state: state,
        pure: {
            escapeHtml: escapeHtml,
            policyHint: policyHint,
            formFromEffective: formFromEffective,
            validate: validate,
            warningFor: warningFor,
            summaryText: summaryText
        }
    };

    if (typeof document !== 'undefined' && document.addEventListener) {
        document.addEventListener('DOMContentLoaded', function () { mount(); });
    }
})();
