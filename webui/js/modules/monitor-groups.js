// monitor-groups.js — R104 (S6 плана 2026-10-07): карточка логических бэкендов
// (групп) на странице «Монитор».
//
// ЗАЧЕМ. Последний пункт S6: «Карточка группы на /monitor: узлы, роли, VRAM,
// tok/s, трафик». До неё состояние групп было видно только в JSON
// `/api/v1/placement`: оператор, глядя на монитор, не понимал, что две машины
// работают как одна модель, кто из них мастер и почему раскладка «degraded».
//
// ЧТО ПОКАЗЫВАЕТ (всё — из серверного состояния, ничего не досчитывает само):
//   - группы: модель, стратегия, мастер, состояние (готова/degraded + причина);
//   - узлы с ролями, свободным и общим VRAM, слоты мастера;
//   - загруженные на узлах модели (что выгрузится при разъединении);
//   - измеренную скорость декода (tok/s) — источник ModelLatencyTracker; если
//     измерений не было, честное «н/д», а не ноль;
//   - сетевой бюджет раскладки (процент к времени токена и секунды на префилл
//     512) — из `/api/v1/placement/plan`, сопоставлением по имени модели.
//
// Карточка ТОЛЬКО ЧИТАЕТ: применение раскладки живёт в окне «Объединение
// бэкендов» (страница «Бэкенды»), и дублировать запись на мониторе не нужно.
//
// Тесты: webui/js/modules/monitor-groups-dom.test.js (mock DOM + mock fetch).

(function () {
    'use strict';

    var MOUNT_ID = 'monitorGroupsPanel';

    var state = {
        lang: 'ru',
        groups: [],
        trafficByModel: {},
        errorKey: '',
        loading: false
    };

    function T(key) {
        if (globalThis.T) { return globalThis.T(key); }
        if (globalThis.I18N && globalThis.I18N.t) { return globalThis.I18N.t(key); }
        return key;
    }

    function apiBase() {
        if (globalThis.WEBUI_CONFIG && globalThis.WEBUI_CONFIG.API_BASE) { return globalThis.WEBUI_CONFIG.API_BASE; }
        return '';
    }

    // authHeaders — заголовки для запросов карточки групп.
    //
    // ЖИВОЙ ДЕФЕКТ (R163, 2026-10-17). В консоли дашборда стабильно висел
    //   POST /api/v1/placement/plan → 401 Unauthorized (тело {"lang":"ru"})
    // Причина: monitor-groups.js подключён в monitor.html ДО js/monitor/api.js, а
    // общий window.Api (из js/modules/api.js) на странице монитора отсутствует
    // вовсе — своя реализация живёт в js/monitor/api.js. Старый код в этом случае
    // возвращал только Content-Type, без X-API-Token.
    //
    // Теперь источники токена те же, что у монитора: ?token=… → localStorage.apiToken
    // → WEBUI_CONFIG.API_TOKEN. Если ничего нет (стенд без авторизации) — идём без
    // токена, как раньше.
    function authHeaders() {
        var headers = { 'Content-Type': 'application/json' };
        if (globalThis.Api && typeof globalThis.Api.getAuthHeaders === 'function') {
            var fromApi = globalThis.Api.getAuthHeaders();
            if (fromApi && typeof fromApi === 'object') { headers = fromApi; }
            if (headers['X-API-Token'] || headers['x-api-token']) { return headers; }
        }
        var token = '';
        try {
            token = new URLSearchParams(globalThis.location.search).get('token') || '';
        } catch (e) { token = ''; }
        if (!token) {
            try { token = globalThis.localStorage.getItem('apiToken') || ''; } catch (e) { token = ''; }
        }
        if (!token) {
            var cfg = globalThis.WEBUI_CONFIG || {};
            token = cfg.API_TOKEN || '';
        }
        if (token) { headers['X-API-Token'] = token; }
        return headers;
    }

    function request(method, path, body) {
        var opts = { method: method, headers: authHeaders() };
        if (body !== undefined) { opts.body = JSON.stringify(body); }
        return fetch(apiBase() + path, opts).then(function (resp) {
            return resp.text().then(function (text) {
                var parsed = null;
                try { parsed = text ? JSON.parse(text) : null; } catch (e) { parsed = null; }
                if (!resp.ok) {
                    var err = new Error((parsed && (parsed.error || parsed.message)) || ('HTTP ' + resp.status));
                    err.status = resp.status;
                    throw err;
                }
                return parsed;
            });
        });
    }

    function escapeHtml(s) {
        return String(s === undefined || s === null ? '' : s)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }

    // --- чистые функции ---------------------------------------------------

    function humanGB(mb) {
        if (typeof mb !== 'number' || mb <= 0) { return '?'; }
        return (mb / 1024).toFixed(1) + ' GB';
    }

    // trafficByModel — индекс трафика из ответа /plan.
    //
    // Сопоставление ТОЧНОЕ по имени модели: у группы может быть маска, и
    // «приклеить» к ней числа чужой модели значило бы показать оператору
    // недостоверный бюджет. Не нашли — не показываем.
    function indexTraffic(plan) {
        var out = {};
        if (!plan || !plan.plans) { return out; }
        plan.plans.forEach(function (item) {
            if (item && item.model && item.traffic) { out[item.model] = item.traffic; }
        });
        return out;
    }

    function tokensText(group) {
        if (!group || !group.tokensKnown) { return T('monitorGroups.notMeasured'); }
        return (Math.round(group.tokensPerSecond * 10) / 10) + ' tok/s';
    }

    function trafficText(traffic) {
        if (!traffic) { return ''; }
        var parts = [];
        ['layer', 'row'].forEach(function (kind) {
            var estimate = traffic[kind];
            if (!estimate || !estimate.known) { return; }
            var percent = (typeof estimate.tokenOverheadPercent === 'number' && estimate.tokenOverheadPercent >= 0)
                ? estimate.tokenOverheadPercent.toFixed(1) + ' %' : '?';
            var prefill = (typeof estimate.prefillMs === 'number')
                ? (estimate.prefillMs / 1000).toFixed(1) + ' s' : '?';
            parts.push(T('plan.strategy.' + kind) + ': ' + percent + ' / ' + prefill);
        });
        return parts.join('<br>');
    }

    function membersHtml(group) {
        var members = group.members || [];
        if (!members.length) { return ''; }
        return '<table class="mg-members"><thead><tr>' +
            '<th>' + escapeHtml(T('monitorGroups.node')) + '</th>' +
            '<th>' + escapeHtml(T('monitorGroups.role')) + '</th>' +
            '<th>' + escapeHtml(T('monitorGroups.vram')) + '</th>' +
            '<th>' + escapeHtml(T('monitorGroups.loaded')) + '</th>' +
            '</tr></thead><tbody>' + members.map(function (m) {
                var isMaster = m.role === 'master';
                var role = isMaster ? T('merge.role.master') : T('merge.role.worker');
                var vram = m.vramKnown
                    ? humanGB(m.vramFreeMB) + ' / ' + humanGB(m.vramTotalMB)
                    : T('monitorGroups.vramUnknown');
                var loaded = (m.loadedModels && m.loadedModels.length)
                    ? m.loadedModels.join(', ')
                    : '—';
                var problem = m.problem ? '<div class="mg-problem">' + escapeHtml(m.problem) + '</div>' : '';
                return '<tr class="' + (m.healthy ? '' : 'mg-bad') + '">' +
                    '<td>' + escapeHtml(m.backendId) + problem + '</td>' +
                    '<td>' + escapeHtml(role) + (m.rpcEndpoint ? '<div class="mg-dim">' + escapeHtml(m.rpcEndpoint) + '</div>' : '') + '</td>' +
                    '<td>' + escapeHtml(vram) + '</td>' +
                    '<td>' + escapeHtml(loaded) + '</td></tr>';
            }).join('') + '</tbody></table>';
    }

    function groupHtml(group, traffic) {
        var stateText = group.usable
            ? T('monitorGroups.ready')
            : (group.healthy ? T('monitorGroups.degraded') : T('monitorGroups.unavailable'));
        var reason = group.degradeReason
            ? '<div class="mg-reason">' + escapeHtml(group.degradeReason) + '</div>' : '';
        return '<div class="mg-group ' + (group.usable ? 'mg-ok' : 'mg-warn') + '">' +
            '<div class="mg-head"><strong>' + escapeHtml(group.id) + '</strong> ' +
            '<span class="mg-model">' + escapeHtml(group.model || '') + '</span> ' +
            '<span class="mg-strategy">' + escapeHtml(T('plan.strategy.' + group.strategy) !== 'plan.strategy.' + group.strategy
                ? T('plan.strategy.' + group.strategy) : (group.strategy || '')) + '</span> ' +
            '<span class="mg-state">' + escapeHtml(stateText) + '</span></div>' +
            '<div class="mg-metrics">' +
            escapeHtml(T('monitorGroups.slots')) + ': ' + escapeHtml(group.slots || 0) + ' · ' +
            escapeHtml(T('monitorGroups.tokens')) + ': ' + escapeHtml(tokensText(group)) + ' · ' +
            escapeHtml(T('monitorGroups.vramTotal')) + ': ' + escapeHtml(humanGB(group.vramFreeMB)) + ' / ' + escapeHtml(humanGB(group.vramTotalMB)) +
            (traffic ? '<div class="mg-traffic">' + trafficText(traffic) + '</div>' : '') +
            '</div>' + reason + membersHtml(group) + '</div>';
    }

    function html(st) {
        var out = '<div class="mg-header"><strong>' + escapeHtml(T('monitorGroups.title')) + '</strong></div>';
        if (st.errorKey) {
            out += '<div class="mg-error">' + escapeHtml(T(st.errorKey)) + '</div>';
        }
        if (!st.groups.length) {
            out += '<div class="mg-empty">' + escapeHtml(T('monitorGroups.none')) + '</div>';
            return out;
        }
        out += st.groups.map(function (group) {
            return groupHtml(group, st.trafficByModel[group.model] || null);
        }).join('');
        return out;
    }

    function render(rootId) {
        if (!globalThis.document || !globalThis.document.getElementById) { return false; }
        var root = globalThis.document.getElementById(rootId || MOUNT_ID);
        if (!root) { return false; }
        root.innerHTML = html(state);
        return true;
    }

    // --- действия ---------------------------------------------------------

    function refresh() {
        state.loading = true;
        state.errorKey = '';
        render();
        return Promise.all([
            request('GET', '/api/v1/placement'),
            // План нужен только ради чисел трафика: у /placement их нет.
            request('POST', '/api/v1/placement/plan', { lang: state.lang }).catch(function () { return null; })
        ]).then(function (out) {
            state.groups = (out[0] && out[0].groups) || [];
            state.trafficByModel = indexTraffic(out[1]);
            state.loading = false;
            render();
            return state.groups;
        }).catch(function (err) {
            state.loading = false;
            state.errorKey = 'monitorGroups.loadFailed';
            render();
            throw err;
        });
    }

    function mount(rootId) {
        state.lang = (globalThis.I18N && globalThis.I18N.getLang) ? globalThis.I18N.getLang() : state.lang;
        render(rootId);
        return refresh();
    }

    // renderFromPlacement — отрисовать из данных, которые монитор УЖЕ получил.
    //
    // Монитор опрашивает `/api/v1/placement` каждые ~2 с и передаёт ответ в
    // renderPlacementPolicy. Делать здесь свой запрос значило бы удваивать трафик
    // на каждом тике ради тех же данных, поэтому карточка рисуется из того же
    // объекта. Трафик (он есть только в /plan) подтягивается один раз при
    // монтировании и кэшируется.
    function renderFromPlacement(pl) {
        state.groups = (pl && pl.groups) || [];
        state.errorKey = '';
        return render();
    }

    // autoMount — подключение без правок логики монитора: рисуем сразу, а числа
    // трафика добираем одним запросом (они меняются редко).
    function autoMount() {
        if (!globalThis.document || !globalThis.document.getElementById) { return; }
        if (!globalThis.document.getElementById(MOUNT_ID)) { return; }
        state.lang = (globalThis.I18N && globalThis.I18N.getLang) ? globalThis.I18N.getLang() : state.lang;
        render();
        request('POST', '/api/v1/placement/plan', { lang: state.lang })
            .then(function (plan) {
                state.trafficByModel = indexTraffic(plan);
                render();
            })
            .catch(function () {
                // Трафик — дополнение к карточке, а не её содержание: без него
                // показываем группы без чисел, а не ошибку.
            });
    }

    function reset() {
        state.groups = [];
        state.trafficByModel = {};
        state.errorKey = '';
        state.loading = false;
    }

    var api = {
        MOUNT_ID: MOUNT_ID,
        state: state,
        mount: mount,
        refresh: refresh,
        render: render,
        reset: reset,
        panelHtml: html,
        renderFromPlacement: renderFromPlacement,
        autoMount: autoMount,
        // чистые функции
        indexTraffic: indexTraffic,
        tokensText: tokensText,
        trafficText: trafficText,
        humanGB: humanGB,
        groupHtml: groupHtml
    };

    if (typeof window !== 'undefined') { window.MonitorGroups = api; }
    if (typeof module !== 'undefined' && module.exports) { module.exports = api; }

    if (globalThis.document) {
        if (globalThis.document.readyState === 'loading' && globalThis.document.addEventListener) {
            globalThis.document.addEventListener('DOMContentLoaded', autoMount);
        } else if (typeof setTimeout === 'function') {
            setTimeout(autoMount, 0);
        }
    }
})();
