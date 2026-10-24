// setup-plan.js — R103 (S5 плана 2026-10-07): шаг мастера «состав кластера».
//
// ЗАЧЕМ. План §6 (S5): «Заменить шаг „выбор режима“ на „состав кластера“:
// обнаружение бэкендов → распределение моделей → проверка → сохранение политики».
// До этого модуля мастер на третьем шаге показывал карточки РЕЖИМОВ работы, а
// данных для «свода кластера» не существовало вовсе. Теперь серверная часть есть
// целиком (R96/R100/R102), и мастер показывает то же, что исполнит резолвер:
//   - POST /api/v1/placement/plan     — предлагаемый план по матрице §8 с ЧИСЛАМИ
//                                       (нужно / свободно на узле / сумма по узлам),
//                                       трафик layer и row по §3, замечания §7;
//   - GET  /api/v1/placement          — текущие сырые placement и backendGroups;
//   - PUT  /api/v1/placement          — запись политики.
//
// ПОЧЕМУ ЛОГИКА НЕ В МАСТЕРЕ. Мастер (setup-wizard.js) — 52 КБ про режимы; его
// переписывание целиком нельзя проверить без браузера. Поэтому шаг собран отдельным
// модулем с mock-DOM тестами, а в мастере — ровно две строки: отрисовать контейнер
// и смонтировать в него панель. Так «состав кластера» проверяем, а остальной мастер
// не трогаем.
//
// ЧУЖИЕ ПРАВИЛА НЕ ТЕРЯЮТСЯ. PUT заменяет секции целиком, поэтому перед записью
// модуль читает текущее состояние и переносит правила и группы, которых план не
// касается: повторный прогон мастера на настроенном стенде не должен стирать
// ручные настройки оператора.

(function () {
    'use strict';

    var MOUNT_ID = 'setupPlanPanel';

    var state = {
        lang: 'ru',
        plan: null,
        result: null,
        errorKey: '',
        busy: false
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

    function authHeaders() {
        if (globalThis.Api && globalThis.Api.getAuthHeaders) { return globalThis.Api.getAuthHeaders(); }
        return { 'Content-Type': 'application/json' };
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
                    err.body = parsed;
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

    function humanGB(gb) {
        if (typeof gb !== 'number' || gb <= 0) { return '?'; }
        return gb.toFixed(1) + ' GB';
    }

    function strategyLabel(strategy) {
        var key = 'plan.strategy.' + strategy;
        var text = T(key);
        return text === key ? strategy : text;
    }

    // planRows — по одной строке на модель: стратегия, вес, решение, обоснование.
    //
    // Числа берём из ответа сервера как есть (sizeGB / needGB / maxNodeGB /
    // sumFreeGB): считает их балансер, и подменять их своей арифметикой значило бы
    // показывать оператору не то, по чему потом принимается решение.
    function planRows(plan) {
        if (!plan || !plan.plans || !plan.plans.length) { return []; }
        return plan.plans.map(function (item) {
            var traffic = item.traffic || null;
            return {
                model: item.model,
                strategy: item.strategy,
                reason: item.reason || '',
                sizeText: item.knownSize ? humanGB(item.sizeGB) : T('plan.sizeUnknown'),
                needText: item.knownSize ? humanGB(item.needGB) : '?',
                maxNodeText: humanGB(item.maxNodeGB),
                sumText: humanGB(item.sumFreeGB),
                degraded: !!item.degraded,
                trafficLayer: trafficPercent(traffic, 'layer'),
                trafficRow: trafficPercent(traffic, 'row'),
                issues: item.issues || []
            };
        });
    }

    function trafficPercent(traffic, kind) {
        if (!traffic || !traffic[kind] || !traffic[kind].known) { return ''; }
        var estimate = traffic[kind];
        var percent = (typeof estimate.tokenOverheadPercent === 'number' && estimate.tokenOverheadPercent >= 0)
            ? estimate.tokenOverheadPercent.toFixed(1) + ' %' : '?';
        var prefill = (typeof estimate.prefillMs === 'number')
            ? (estimate.prefillMs / 1000).toFixed(1) + ' s' : '?';
        return percent + ' / ' + prefill;
    }

    // buildApplyPayload — что записываем: план, но чужие настройки переносим.
    //
    // Ключи плана (модели) заменяются целиком; всё остальное в placement.models,
    // placement.classes и backendGroups остаётся как было. Иначе повторный прогон
    // мастера стирал бы ручные правила оператора.
    function buildApplyPayload(plan, current) {
        if (!plan || !plan.placement) { return null; }
        var plannedModels = (plan.plans || []).map(function (p) { return p.model; });
        var plannedGroups = (plan.backendGroups || []).map(function (g) { return g.id; });

        var placement = { enabled: true, fallback: plan.placement.fallback || 'error' };
        if (current && current.placement) {
            Object.keys(current.placement).forEach(function (k) { placement[k] = current.placement[k]; });
        }
        placement.enabled = plan.placement.enabled !== false;
        placement.models = (placement.models || []).filter(function (r) {
            return plannedModels.indexOf(r.model) < 0;
        }).concat(plan.placement.models || []);
        placement.classes = (placement.classes || []).concat(
            (plan.placement.classes || []).filter(function (c) {
                return !(placement.classes || []).some(function (existing) {
                    return JSON.stringify(existing) === JSON.stringify(c);
                });
            })
        );

        var groups = (current && current.backendGroups ? current.backendGroups : []).filter(function (g) {
            return plannedGroups.indexOf(g.id) < 0;
        }).concat(plan.backendGroups || []);

        return { placement: placement, backendGroups: groups };
    }

    // --- отрисовка --------------------------------------------------------

    function rowsHtml(plan) {
        var rows = planRows(plan);
        if (!rows.length) {
            return '<div class="sp-empty">' + escapeHtml(T('plan.noModels')) + '</div>';
        }
        return '<table class="sp-table"><thead><tr>' +
            '<th>' + escapeHtml(T('plan.model')) + '</th>' +
            '<th>' + escapeHtml(T('plan.strategy')) + '</th>' +
            '<th>' + escapeHtml(T('plan.weights')) + '</th>' +
            '<th>' + escapeHtml(T('plan.free')) + '</th>' +
            '<th>' + escapeHtml(T('plan.traffic')) + '</th>' +
            '</tr></thead><tbody>' + rows.map(function (row) {
                var traffic = '';
                if (row.trafficLayer) { traffic += escapeHtml(T('plan.strategy.layer')) + ': ' + escapeHtml(row.trafficLayer); }
                if (row.trafficRow) { traffic += (traffic ? '<br>' : '') + escapeHtml(T('plan.strategy.row')) + ': ' + escapeHtml(row.trafficRow); }
                return '<tr class="' + (row.degraded ? 'sp-degraded' : '') + '">' +
                    '<td><strong>' + escapeHtml(row.model) + '</strong>' +
                    (row.reason ? '<div class="sp-reason">' + escapeHtml(row.reason) + '</div>' : '') +
                    issuesHtml(row.issues) + '</td>' +
                    '<td>' + escapeHtml(strategyLabel(row.strategy)) + '</td>' +
                    '<td>' + escapeHtml(T('plan.need')) + ' ' + escapeHtml(row.needText) +
                    '<div class="sp-dim">' + escapeHtml(T('plan.weights')) + ' ' + escapeHtml(row.sizeText) + '</div></td>' +
                    '<td>' + escapeHtml(T('plan.maxNode')) + ' ' + escapeHtml(row.maxNodeText) +
                    '<div class="sp-dim">' + escapeHtml(T('plan.sum')) + ' ' + escapeHtml(row.sumText) + '</div></td>' +
                    '<td>' + traffic + '</td></tr>';
            }).join('') + '</tbody></table>';
    }

    function issuesHtml(issues) {
        if (!issues || !issues.length) { return ''; }
        return '<ul class="sp-issues">' + issues.map(function (issue) {
            return '<li class="sp-' + escapeHtml(issue.level || 'hint') + '">' +
                escapeHtml(issue.text || '') + '</li>';
        }).join('') + '</ul>';
    }

    // matchesOf — какие пресеты раскладки соответствуют предложенному плану.
    //
    // Нужно, чтобы оператор видел не только «sharded (слоями)», но и что это
    // значит: какой случай стенда так раскладывается и чем он отличается от
    // соседнего. Сопоставление — по стратегии (+ layer/row у sharded).
    function matchesOf(plan) {
        if (!plan || !plan.plans || !plan.plans.length) { return []; }
        if (!globalThis.HardwarePresets || !globalThis.HardwarePresets.listPlacements) { return []; }
        var wanted = {};
        plan.plans.forEach(function (item) {
            var groupStrategy = '';
            if (item.backendGroup && item.backendGroup.strategy) { groupStrategy = item.backendGroup.strategy; }
            else if (item.strategy === 'sharded') { groupStrategy = 'layer'; }
            wanted[globalThis.HardwarePresets.matchKey(item.strategy, groupStrategy)] = true;
        });
        return globalThis.HardwarePresets.listPlacements().filter(function (p) {
            return wanted[p.match];
        }).map(function (p) { return p.key; });
    }

    // presetsHtml — справка «какие раскладки бывают», с подсветкой совпавших.
    function presetsHtml(plan) {
        if (!globalThis.HardwarePresets || !globalThis.HardwarePresets.listPlacements) { return ''; }
        var matched = matchesOf(plan);
        var rows = globalThis.HardwarePresets.listPlacements().map(function (p) {
            var isMatched = matched.indexOf(p.key) >= 0;
            var label = T(p.labelKey);
            if (label === p.labelKey) { label = p.label; }
            var hint = T(p.hintKey);
            if (hint === p.hintKey) { hint = ''; }
            return '<li class="sp-preset' + (isMatched ? ' sp-preset-active' : '') + '">' +
                '<strong>' + escapeHtml(label) + '</strong>' +
                (hint ? ': ' + escapeHtml(hint) : '') +
                (isMatched ? ' <em>(' + escapeHtml(T('plan.presetApplies')) + ')</em>' : '') +
                '</li>';
        }).join('');
        return '<div class="sp-presets"><strong>' + escapeHtml(T('plan.presetsTitle')) + '</strong><ul>' +
            rows + '</ul></div>';
    }

    function html(st) {
        var out = '<div class="sp-header"><strong>' + escapeHtml(T('plan.title')) + '</strong>' +
            '<div class="sp-hint">' + escapeHtml(T('plan.hint')) + '</div></div>';

        if (st.errorKey) {
            out += '<div class="sp-error">' + escapeHtml(T(st.errorKey)) + '</div>';
        }
        if (!st.plan) {
            out += '<div class="sp-loading">' + escapeHtml(T('plan.loading')) + '</div>';
            return out;
        }

        out += rowsHtml(st.plan);
        if (st.plan.discoveredModels !== undefined) {
            out += '<div class="sp-dim">' + escapeHtml(T('plan.discovered')) + ': ' + escapeHtml(st.plan.discoveredModels) + '</div>';
        }
        if (st.plan.warnings && st.plan.warnings.length) {
            out += '<ul class="sp-issues">' + st.plan.warnings.map(function (w) {
                return '<li class="sp-warning">' + escapeHtml(w) + '</li>';
            }).join('') + '</ul>';
        }

        out += '<div class="sp-actions">' +
            '<button type="button" id="spRefresh"' + (st.busy ? ' disabled' : '') + '>' + escapeHtml(T('plan.refresh')) + '</button>' +
            '<button type="button" id="spApply"' + (st.busy ? ' disabled' : '') + '>' + escapeHtml(T('plan.apply')) + '</button>' +
            '</div>';

        out += presetsHtml(st.plan);

        if (st.result && st.result.applied) {
            out += '<div class="sp-applied">' + escapeHtml(T('plan.applied')) + '</div>';
            out += st.result.persisted
                ? '<div class="sp-ok">' + escapeHtml(T('plan.persisted')) + '</div>'
                : '<div class="sp-warn">' + escapeHtml(T('plan.notPersisted')) +
                  (st.result.persistError ? ': ' + escapeHtml(st.result.persistError) : '') + '</div>';
        }
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
        state.busy = true;
        state.errorKey = '';
        render();
        return request('POST', '/api/v1/placement/plan', { lang: state.lang }).then(function (plan) {
            state.plan = plan;
            state.busy = false;
            render();
            return plan;
        }).catch(function (err) {
            state.busy = false;
            state.errorKey = 'plan.loadFailed';
            render();
            throw err;
        });
    }

    function mount(rootId) {
        state.lang = (globalThis.I18N && globalThis.I18N.getLang) ? globalThis.I18N.getLang() : state.lang;
        state.result = null;
        render(rootId);
        return refresh();
    }

    function apply() {
        if (!state.plan) { return Promise.resolve(null); }
        state.busy = true;
        state.errorKey = '';
        render();
        return request('GET', '/api/v1/placement').then(function (current) {
            var payload = buildApplyPayload(state.plan, current);
            if (!payload) { throw new Error('nothing to apply'); }
            return request('PUT', '/api/v1/placement', payload);
        }).then(function (res) {
            state.result = res;
            state.busy = false;
            render();
            return res;
        }).catch(function (err) {
            state.busy = false;
            state.errorKey = 'plan.applyFailed';
            render();
            throw err;
        });
    }

    function handleClick(ev) {
        var t = ev && ev.target;
        if (!t || !t.id) { return null; }
        if (t.id === 'spRefresh') { return refresh(); }
        if (t.id === 'spApply') { return apply(); }
        return null;
    }

    function reset() {
        state.plan = null;
        state.result = null;
        state.errorKey = '';
        state.busy = false;
    }

    var api = {
        MOUNT_ID: MOUNT_ID,
        state: state,
        mount: mount,
        refresh: refresh,
        apply: apply,
        render: render,
        handleClick: handleClick,
        reset: reset,
        panelHtml: html,
        // чистые функции
        planRows: planRows,
        matchesOf: matchesOf,
        presetsHtml: presetsHtml,
        buildApplyPayload: buildApplyPayload,
        trafficPercent: trafficPercent,
        strategyLabel: strategyLabel
    };

    if (typeof window !== 'undefined') { window.SetupPlan = api; }
    if (typeof module !== 'undefined' && module.exports) { module.exports = api; }
})();
