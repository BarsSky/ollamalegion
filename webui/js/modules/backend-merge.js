// backend-merge.js — R102 (S6 плана 2026-10-07): окно «Объединение бэкендов».
//
// ЗАЧЕМ. План §6 (S6): выбрать 2+ бэкенда → роли (мастер/воркер) → стратегия →
// предпросмотр бюджета VRAM и трафика → dry-run → применить; плюс «Разъединить».
// Серверная часть к этому моменту готова целиком:
//   - POST /api/v1/placement/plan      — предлагаемый план с числами (матрица §8,
//                                        трафик layer/row по §3);
//   - POST /api/v1/placement/validate  — сухой прогон §7 (узлы, RPC-порты,
//                                        версии сборок, сумма VRAM, KV, трафик);
//   - GET  /api/v1/placement           — состояние + СЫРЫЕ placement и backendGroups
//                                        (нужны, чтобы не затереть чужие правила);
//   - PUT  /api/v1/placement           — применить (полная замена секций).
//
// ЧТО ДЕЛАЕТ МОДУЛЬ. Рисует панель в контейнер `#backendMergePanel` и ходит по этим
// эндпоинтам. Сам ничего не решает: стратегия, роли и вердикты приходят с сервера,
// иначе окно и резолвер разошлись бы (окно считало бы «влезает», а резолвер отдавал
// 503).
//
// ЧЕСТНОСТЬ. `persisted: false` показывается как есть: в bundled-стеке config.json
// смонтирован только для чтения, и делать вид, что настройка сохранена, нельзя.
// «Разъединить» снимает и группу, И правило модели, которое на неё ссылалось, —
// иначе после разъединения запросы к этой модели начали бы получать 503
// (стратегия есть, исполнителя нет). Об этом UI говорит прямо.
//
// Тесты: webui/js/modules/backend-merge-dom.test.js (mock DOM + mock fetch).

(function () {
    'use strict';

    var MOUNT_ID = 'backendMergePanel';
    var DEFAULT_TPS = 30;

    var state = {
        lang: 'ru',
        backends: [],
        selected: {},      // backendId -> true
        masterId: '',
        strategy: 'layer',
        model: '',
        sizeBytes: 0,
        hiddenSize: 0,
        layers: 0,
        nCtx: 0,
        plan: null,
        report: null,
        result: null,
        errorKey: '',
        busy: false
    };

    // --- i18n и сеть ------------------------------------------------------

    function T(key, vars) {
        if (globalThis.T) { return globalThis.T(key, vars); }
        if (globalThis.I18N && globalThis.I18N.t) { return globalThis.I18N.t(key, vars); }
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

    function getJSON(path) { return request('GET', path); }
    function postJSON(path, body) { return request('POST', path, body); }
    function putJSON(path, body) { return request('PUT', path, body); }

    // --- чистые функции (тестируются напрямую) ----------------------------

    function escapeHtml(s) {
        return String(s === undefined || s === null ? '' : s)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }

    // groupIdFor — id группы из имени модели (тот же вид, что предлагает сервер).
    function groupIdFor(model) {
        var id = String(model || '').toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '');
        if (!id) { id = 'model'; }
        return 'group-' + id.slice(0, 48);
    }

    function selectedIDs(st) {
        return Object.keys(st.selected).filter(function (id) { return st.selected[id]; });
    }

    // buildGroup — композиция из состояния формы; null, если собрать нечего.
    //
    // Правила те же, что у серверного валидатора: минимум два узла, ровно один
    // мастер, у воркеров обязателен RPC-эндпоинт. Локальная проверка нужна,
    // чтобы не гонять заведомо неполный запрос и сразу объяснить, чего не хватает.
    function buildGroup(st) {
        var ids = selectedIDs(st);
        if (ids.length < 2) { return null; }
        if (!st.model) { return null; }
        var master = st.masterId && st.selected[st.masterId] ? st.masterId : ids[0];
        var members = ids.map(function (id) {
            var backend = backendByID(st, id);
            if (id === master) { return { backendId: id, role: 'master' }; }
            return {
                backendId: id,
                role: 'worker',
                rpcEndpoint: rpcEndpointFor(backend)
            };
        });
        return {
            id: groupIdFor(st.model),
            model: st.model,
            strategy: st.strategy,
            members: members
        };
    }

    function backendByID(st, id) {
        for (var i = 0; i < st.backends.length; i++) {
            if (st.backends[i].id === id) { return st.backends[i]; }
        }
        return null;
    }

    // rpcEndpointFor — адрес RPC-сервера воркера.
    //
    // Порт по умолчанию 50052 (тот же, что предлагает серверный план): реальный
    // знает только оператор, и валидатор скажет, если порт закрыт. Угадать и
    // промолчать нельзя — поэтому значение видно в форме и его можно поправить.
    function rpcEndpointFor(backend) {
        var host = backend && backend.host ? backend.host : '';
        if (!host) { return ''; }
        return host + ':50052';
    }

    function ruleForModel(model) {
        return {
            model: model,
            strategy: 'auto',
            reason: T('merge.appliedReason'),
            auto: { prefer: ['sharded', 'single'] }
        };
    }

    // buildApplyPayload — тело PUT: текущее состояние + наша группа и правило.
    //
    // PUT заменяет секции целиком, поэтому чужие правила и группы переносятся как
    // есть: окно объединения не должно стирать настройки, которых не касается.
    function buildApplyPayload(st, current) {
        var group = buildGroup(st);
        if (!group) { return null; }
        var placement = { enabled: true, fallback: 'error' };
        if (current && current.placement) {
            Object.keys(current.placement).forEach(function (k) { placement[k] = current.placement[k]; });
        }
        placement.models = (placement.models || []).filter(function (r) { return r.model !== st.model; });
        placement.models.push(ruleForModel(st.model));

        var groups = [];
        if (current && current.backendGroups) {
            groups = current.backendGroups.filter(function (g) { return g.id !== group.id; });
        }
        groups.push(group);
        return { placement: placement, backendGroups: groups, splitRules: [] };
    }

    // buildSplitPayload — тело PUT для «Разъединить».
    //
    // Снимаем и группу, И правила моделей этой группы: оставить правило значит
    // после разъединения получить 503 на каждый запрос к модели (стратегия есть,
    // исполнителя нет). Что именно снято — возвращаем списком, чтобы UI сказал это
    // оператору, а не сделал молча.
    function buildSplitPayload(current, groupId) {
        var group = null;
        var groups = [];
        (current && current.backendGroups ? current.backendGroups : []).forEach(function (g) {
            if (g.id === groupId) { group = g; } else { groups.push(g); }
        });
        if (!group) { return null; }

        var placement = { enabled: true, fallback: 'error' };
        if (current && current.placement) {
            Object.keys(current.placement).forEach(function (k) { placement[k] = current.placement[k]; });
        }
        var removed = [];
        placement.models = (placement.models || []).filter(function (r) {
            if (r.model && group.model && r.model === group.model) { removed.push(r.model); return false; }
            return true;
        });
        return { placement: placement, backendGroups: groups, removedRules: removed };
    }

    function levelLabel(level) {
        if (level === 'error') { return T('merge.level.error'); }
        if (level === 'warning') { return T('merge.level.warning'); }
        return T('merge.level.hint');
    }

    function checkLabel(check) {
        var key = 'merge.check.' + check;
        var text = T(key);
        return text === key ? check : text;
    }

    // issuesHtml — замечания §7 с числами (сервер уже подставил их в текст).
    function issuesHtml(issues) {
        if (!issues || !issues.length) {
            return '<div class="bm-ok">' + escapeHtml(T('merge.noIssues')) + '</div>';
        }
        return '<ul class="bm-issues">' + issues.map(function (issue) {
            return '<li class="bm-issue bm-' + escapeHtml(issue.level || 'hint') + '">' +
                '<strong>' + escapeHtml(levelLabel(issue.level)) + '</strong> ' +
                '<em>' + escapeHtml(checkLabel(issue.check)) + '</em>: ' +
                escapeHtml(issue.text || '') + '</li>';
        }).join('') + '</ul>';
    }

    // trafficOf — где лежат оценки трафика.
    //
    // СЕРВЕР ОТДАЁТ ИХ ПО-РАЗНОМУ, и это ловушка: /api/v1/placement/plan кладёт
    // traffic внутрь элемента plans[] (у каждой модели он свой), а элемент плана
    // отдельно — на верхнем уровне. Раньше модуль читал только верхний уровень, и
    // числа просто не появлялись в окне (поймано тестом).
    function trafficOf(plan) {
        if (!plan) { return null; }
        if (plan.traffic) { return plan.traffic; }
        if (plan.plans && plan.plans.length && plan.plans[0].traffic) { return plan.plans[0].traffic; }
        return null;
    }

    // trafficHtml — числа §3 для обеих стратегий: между layer и row оператор
    // выбирает по процентам и секундам, а не «на глаз».
    function trafficHtml(plan) {
        var traffic = trafficOf(plan);
        if (!traffic) { return ''; }
        var layer = traffic.layer || {};
        var row = traffic.row || {};
        function line(name, est) {
            if (!est || !est.known) { return ''; }
            var percent = typeof est.tokenOverheadPercent === 'number' && est.tokenOverheadPercent >= 0
                ? est.tokenOverheadPercent.toFixed(1) + ' %' : '?';
            var prefill = typeof est.prefillMs === 'number' ? (est.prefillMs / 1000).toFixed(1) + ' s' : '?';
            return '<li>' + escapeHtml(name) + ': ' +
                escapeHtml(T('merge.tokenOverhead')) + ' ' + escapeHtml(percent) + ', ' +
                escapeHtml(T('merge.prefill')) + ' ' + escapeHtml(prefill) + '</li>';
        }
        var rows = line(T('merge.traffic.layer'), layer) + line(T('merge.traffic.row'), row);
        if (!rows) { return ''; }
        return '<div class="bm-traffic"><strong>' + escapeHtml(T('merge.traffic')) + '</strong><ul>' + rows + '</ul></div>';
    }

    function backendsHtml(st) {
        if (!st.backends.length) {
            return '<div class="bm-empty">' + escapeHtml(T('merge.noBackends')) + '</div>';
        }
        // R175: таблица с ЗАГОЛОВКАМИ и штатными классами. Без шапки блок читался
        // как набор подписей вперемешку с контролами (жалоба: «стили не применились»).
        return '<table class="gguf-mp-table bm-backends"><thead><tr>' +
            '<th class="bm-col-pick"></th>' +
            '<th>' + escapeHtml(T('merge.col.node')) + '</th>' +
            '<th>' + escapeHtml(T('merge.col.host')) + '</th>' +
            '<th>' + escapeHtml(T('merge.col.type')) + '</th>' +
            '<th class="bm-col-master">' + escapeHtml(T('merge.col.master')) + '</th>' +
            '</tr></thead><tbody>' + st.backends.map(function (b) {
            var checked = st.selected[b.id] ? ' checked' : '';
            var isMaster = st.masterId === b.id || (!st.masterId && selectedIDs(st)[0] === b.id);
            return '<tr><td><input type="checkbox" id="bmBackend" data-backend="' + escapeHtml(b.id) + '"' + checked + '></td>' +
                '<td>' + escapeHtml(b.id) + '</td>' +
                '<td>' + escapeHtml(b.host || '') + '</td>' +
                '<td>' + escapeHtml(b.type || '') + '</td>' +
                '<td><input type="radio" name="bmMaster" id="bmMaster" data-backend="' + escapeHtml(b.id) + '"' +
                (isMaster ? ' checked' : '') + '></td></tr>';
        }).join('') + '</tbody></table>';
    }

    function resultHtml(st) {
        var html = '';
        if (st.errorKey) {
            html += '<div class="bm-error">' + escapeHtml(T(st.errorKey)) + '</div>';
        }
        if (st.result && st.result.applied) {
            var persisted = st.result.persisted
                ? '<div class="bm-ok">' + escapeHtml(T('merge.persisted')) + '</div>'
                : '<div class="bm-warn">' + escapeHtml(T('merge.notPersisted')) +
                  (st.result.persistError ? ': ' + escapeHtml(st.result.persistError) : '') + '</div>';
            html += '<div class="bm-applied">' + escapeHtml(T('merge.applied')) + '</div>' + persisted;
            // R107: что именно сняли при разъединении. Раньше removedRules считались,
            // но нигде не показывались — оператор не видел, что вместе с группой ушло
            // и правило модели (иначе после разъединения были бы 503).
            if (st.splitInfo && st.splitInfo.removedRules && st.splitInfo.removedRules.length) {
                html += '<div class="bm-warn">' + escapeHtml(T('merge.removedRules')) + ': ' +
                    escapeHtml(st.splitInfo.removedRules.join(', ')) + '</div>';
            }
        }
        if (st.plan) {
            html += '<div class="bm-plan"><strong>' + escapeHtml(T('merge.planFor')) + '</strong> ' +
                escapeHtml(st.plan.model || '') + ': ' + escapeHtml(describeFirstPlan(st.plan)) + '</div>';
        }
        html += trafficHtml(st.plan);
        if (st.report) {
            html += '<div class="bm-report"><strong>' + escapeHtml(T('merge.issues')) + '</strong>' +
                issuesHtml(st.report.issues) + '</div>';
        }
        return html;
    }

    function describeFirstPlan(plan) {
        if (!plan || !plan.plans || !plan.plans.length) { return ''; }
        var item = plan.plans[0];
        var text = item.strategy || '';
        if (item.reason) { text += ' — ' + item.reason; }
        return text;
    }

    function panelHtml(st) {
        return '' +
            // R181: своего заголовка НЕ рисуем - он уже в шапке карточки;
            // дубль «Объединение бэкендов» дважды подряд выглядел ошибкой вёрстки.
            '<div class="bm-header">' +
            '<div class="bm-hint">' + escapeHtml(T('merge.hint')) + '</div></div>' +
            '<label class="bm-field form-group">' + escapeHtml(T('merge.model')) +
            '<input type="text" class="form-control" id="bmModel" value="' + escapeHtml(st.model) + '"></label>' +
            '<div class="bm-field bm-link"><label for="bmLinkMbps">' + escapeHtml(T('merge.link')) +
            '<span class="bm-hint-inline">' + escapeHtml(T('merge.linkHint')) + '</span></label>' +
            '<div class="bm-link-row" style="display:flex;gap:8px;align-items:center;flex-wrap:wrap;margin-top:4px;">' +
            '<input type="number" class="form-control" id="bmLinkMbps" min="0" step="100" value="' +
            (st.bandwidthMbps > 0 ? st.bandwidthMbps : '') + '" placeholder="' + escapeHtml(T('merge.linkAuto')) + '">' +
            '<span class="bm-link-unit">' + escapeHtml(T('merge.mbps')) + '</span>' +
            '<button type="button" class="btn btn-secondary" id="bmMeasure"' + (st.busy ? ' disabled' : '') + '>' + escapeHtml(T('merge.measure')) + '</button>' +
            '</div>' +
            (st.linkHint ? '<div class="bm-hint">' + escapeHtml(st.linkHint) + '</div>' : '') +
            '</div>' +
            '<label class="bm-field form-group">' + escapeHtml(T('merge.strategy')) +
            '<select class="form-control" id="bmStrategy">' +
            '<option value="layer"' + (st.strategy === 'layer' ? ' selected' : '') + '>' + escapeHtml(T('merge.strategy.layer')) + '</option>' +
            '<option value="row"' + (st.strategy === 'row' ? ' selected' : '') + '>' + escapeHtml(T('merge.strategy.row')) + '</option>' +
            '</select></label>' +
            '<div class="bm-list">' + backendsHtml(st) + '</div>' +
            '<div class="bm-actions form-actions">' +
            '<button type="button" class="btn btn-secondary" id="bmCheck"' + (st.busy ? ' disabled' : '') + '>' + escapeHtml(T('merge.check')) + '</button>' +
            '<button type="button" class="btn btn-primary" id="bmApply"' + (st.busy ? ' disabled' : '') + '>' + escapeHtml(T('merge.apply')) + '</button>' +
            '</div>' +
            '<div class="bm-groups">' + groupsHtml(st) + '</div>' +
            resultHtml(st);
    }

    function groupsHtml(st) {
        var groups = (st.result && st.result.groups) ? st.result.groups : [];
        if (!groups.length) { return ''; }
        return '<strong>' + escapeHtml(T('merge.groups')) + '</strong><ul>' + groups.map(function (g) {
            return '<li>' + escapeHtml(g.id) + ' — ' + escapeHtml(g.strategy || '') +
                ' (' + escapeHtml(T('merge.ready')) + ': ' + (g.usable ? 'yes' : 'no') + ')' +
                ' <button type="button" id="bmSplit" data-group="' + escapeHtml(g.id) + '">' +
                escapeHtml(T('merge.split')) + '</button></li>';
        }).join('') + '</ul>';
    }

    // --- действия ---------------------------------------------------------

    function render(rootId) {
        if (!globalThis.document || !globalThis.document.getElementById) { return false; }
        var root = globalThis.document.getElementById(rootId || MOUNT_ID);
        if (!root) { return false; }
        root.innerHTML = panelHtml(state);
        return true;
    }

    function mount(rootId) {
        loadLinkSetting(state);
        state.lang = (globalThis.I18N && globalThis.I18N.getLang) ? globalThis.I18N.getLang() : state.lang;
        return loadBackends().then(function () {
            return render(rootId);
        }).catch(function (err) {
            state.errorKey = 'merge.backendsFailed';
            render(rootId);
            throw err;
        });
    }

    function loadBackends() {
        return getJSON('/api/v1/backends').then(function (resp) {
            var list = (resp && resp.backends) || [];
            state.backends = list.filter(function (b) {
                return b && b.type === 'image_cpp' ? false : (b.type === 'llama_cpp' || b.type === '');
            }).map(function (b) {
                return { id: b.id, host: b.host, type: b.type, status: b.status };
            });
            return state.backends;
        });
    }

    // --- канал между узлами (R166, 2026-10-17) ---------------------------
    //
    // ЗАЧЕМ ЯВНОЕ ПОЛЕ. Пропускная способность канала — ключевой параметр раскладки:
    // по ней оценивается трафик (composition.EstimateTraffic) и решается, тянет ли
    // layer-split по сети. До R166 число бралось только из .env (LB_RPC_LINK_MBPS) и
    // молча устаревало; теперь балансер САМ замеряет канал до узлов при старте
    // (и далее раз в период), а здесь оператор может перезамерить и/или задать
    // значение руками — с явным указанием, чей это замер.
    var LINK_STORAGE_KEY = 'ollamalegion_link_mbps';

    function loadLinkSetting(st) {
        try {
            var v = globalThis.localStorage.getItem(LINK_STORAGE_KEY);
            var n = v ? parseInt(v, 10) : 0;
            if (n > 0) { st.bandwidthMbps = n; }
        } catch (e) { /* localStorage недоступен — работаем как «авто» */ }
    }

    function saveLinkSetting(mbps) {
        try {
            if (mbps > 0) { globalThis.localStorage.setItem(LINK_STORAGE_KEY, String(mbps)); }
            else { globalThis.localStorage.removeItem(LINK_STORAGE_KEY); }
        } catch (e) { /* noop */ }
    }

    // linkHint — человекочитаемое «что пойдёт в оценку» (из ответа балансера).
    function linkHintFrom(bw) {
        if (!bw) { return ''; }
        var src = bw.source || '';
        var mbps = bw.mbps_bits || 0;
        var mBps = bw.mbps || 0;
        if (!mbps && !mBps) { return ''; }
        return T('merge.linkApplied') + ': ' + mbps + ' ' + T('merge.mbps') +
            ' (' + Math.round(mBps) + ' ' + T('merge.mBps') + '), ' + T('merge.source') + ': ' +
            (src === 'auto' ? T('merge.source.auto') : src === 'request' ? T('merge.source.request')
                : src === 'env' ? T('merge.source.env') : T('merge.source.default'));
    }

    function planBody(st) {
        return {
            models: [{ name: st.model, sizeBytes: st.sizeBytes || 0, hiddenSize: st.hiddenSize || 0, layers: st.layers || 0, nCtx: st.nCtx || 0 }],
            lang: st.lang,
            tokensPerSecond: st.tokensPerSecond || DEFAULT_TPS,
            // R166: 0 = «пусть балансер решит сам» (автозамер/env/дефолт).
            bandwidthMBps: st.bandwidthMbps > 0 ? Math.round(st.bandwidthMbps / 8) : 0
        };
    }

    function validateBody(st, group) {
        return {
            group: group,
            model: {
                name: st.model,
                backendType: 'llama_cpp',
                sizeBytes: st.sizeBytes || 0,
                hiddenSize: st.hiddenSize || 0,
                layers: st.layers || 0,
                nCtx: st.nCtx || 0
            },
            lang: st.lang,
            bandwidthMBps: st.bandwidthMbps > 0 ? Math.round(st.bandwidthMbps / 8) : 0
        };
    }

    // measureLink — «Замерить»: просим балансер замерить канал до узлов сейчас.
    //
    // Почему это делает балансер, а не браузер: слои передаёт cppworker, то есть
    // путь «контейнер ↔ контейнер». Замер из браузера показал бы путь до WebUI
    // (nginx) и к делу не относится.
    function measureLink() {
        state.busy = true;
        state.linkHint = T('merge.measuring');
        render();
        return postJSON('/api/v1/rpc/link/measure', {}).then(function (res) {
            state.busy = false;
            var rows = (res && res.results) || [];
            var eff = (res && res.effective) || null;
            var parts = rows.map(function (r) {
                if (r.error) { return (r.backendId || '?') + ': ' + T('merge.linkFailed'); }
                return (r.backendId || '?') + ': ' + r.mbps + ' ' + T('merge.mbps');
            });
            state.linkHint = (parts.length ? parts.join(' · ') + ' | ' : '') + linkHintFrom(eff);
            // Поле оператора НЕ заполняем автоматически: пустое поле означает «авто»,
            // и затирать этот выбор замером — значит прятать, откуда число.
            render();
            return res;
        }).catch(function (err) {
            state.busy = false;
            state.linkHint = T('merge.linkMeasureFailed');
            render();
            throw err;
        });
    }

    // check — сухой прогон: числа плана + замечания §7. Ничего не сохраняет.
    function check() {
        var group = buildGroup(state);
        state.result = null;
        if (!group) {
            state.errorKey = selectedIDs(state).length < 2 ? 'merge.selectTwo' : 'merge.needModel';
            render();
            return Promise.resolve(null);
        }
        state.errorKey = '';
        state.busy = true;
        render();
        return Promise.all([
            postJSON('/api/v1/placement/plan', planBody(state)),
            postJSON('/api/v1/placement/validate', validateBody(state, group))
        ]).then(function (out) {
            state.plan = out[0];
            state.report = out[1];
            // R166: показываем, какая скорость канала реально ушла в оценку.
            var bw = (out[0] && out[0].bandwidth) || (out[1] && out[1].bandwidth) || null;
            if (bw) { state.linkHint = linkHintFrom(bw); }
            state.busy = false;
            render();
            return { plan: state.plan, report: state.report };
        }).catch(function (err) {
            state.busy = false;
            state.errorKey = 'merge.checkFailed';
            render();
            throw err;
        });
    }

    // apply — запись политики (PUT): полная замена секций, но чужие правила и
    // группы переносятся из текущего состояния как есть.
    function apply() {
        var group = buildGroup(state);
        if (!group) {
            state.errorKey = selectedIDs(state).length < 2 ? 'merge.selectTwo' : 'merge.needModel';
            render();
            return Promise.resolve(null);
        }
        state.errorKey = '';
        state.busy = true;
        render();
        return getJSON('/api/v1/placement').then(function (current) {
            var payload = buildApplyPayload(state, current);
            if (!payload) { throw new Error('payload'); }
            return putJSON('/api/v1/placement', {
                placement: payload.placement,
                backendGroups: payload.backendGroups
            });
        }).then(function (res) {
            state.result = res;
            state.busy = false;
            render();
            return res;
        }).catch(function (err) {
            state.busy = false;
            state.errorKey = 'merge.applyFailed';
            if (err && err.body && err.body.warnings) { state.report = { issues: warningsAsIssues(err.body.warnings) }; }
            render();
            throw err;
        });
    }

    function warningsAsIssues(warnings) {
        return (warnings || []).map(function (text) {
            return { check: 'config', level: 'error', text: text };
        });
    }

    // loadedModelsFor — что БУДЕТ ВЫГРУЖЕНО при разъединении этой группы.
    //
    // Источник — состояние групп (`groups[]` из GET /api/v1/placement), а не
    // конфиг: конфиг описывает раскладку, а загруженное знает только балансер.
    // Если объединённого списка нет, собираем из узлов — так список не потеряется
    // на сборке, которая отдаёт только per-member данные.
    function loadedModelsFor(current, groupId) {
        var groups = (current && current.groups) ? current.groups : [];
        for (var i = 0; i < groups.length; i++) {
            if (groups[i] && groups[i].id === groupId) {
                if (groups[i].loadedModels && groups[i].loadedModels.length) {
                    return groups[i].loadedModels.slice();
                }
                var fromMembers = [];
                (groups[i].members || []).forEach(function (m) {
                    (m.loadedModels || []).forEach(function (model) {
                        if (fromMembers.indexOf(model) < 0) { fromMembers.push(model); }
                    });
                });
                return fromMembers.sort();
            }
        }
        return [];
    }

    // splitConfirmMessage — текст подтверждения с тем, ЧТО потеряет оператор.
    function splitConfirmMessage(current, groupId) {
        var loaded = loadedModelsFor(current, groupId);
        var message = T('merge.splitConfirm');
        if (loaded.length) {
            message += '\n\n' + T('merge.splitWillUnload') + ': ' + loaded.join(', ');
        } else {
            message += '\n\n' + T('merge.splitNothingLoaded');
        }
        return message;
    }

    // split — «Разъединить»: снять группу и правила её модели.
    //
    // Порядок важен: сначала ЧИТАЕМ состояние (нужно и для payload, и для списка
    // выгрузки), показываем оператору, что именно потеряется, и только потом
    // записываем. Раньше подтверждение было вслепую: «снять группу?» без ответа на
    // вопрос «а что с загруженными моделями».
    function split(groupId) {
        state.busy = true;
        render();
        return getJSON('/api/v1/placement').then(function (current) {
            var payload = buildSplitPayload(current, groupId);
            if (!payload) { throw new Error('group not found: ' + groupId); }
            if (globalThis.confirm && !globalThis.confirm(splitConfirmMessage(current, groupId))) {
                state.busy = false;
                render();
                return null;
            }
            state.splitInfo = { removedRules: payload.removedRules };
            return putJSON('/api/v1/placement', {
                placement: payload.placement,
                backendGroups: payload.backendGroups
            });
        }).then(function (res) {
            state.busy = false;
            if (res === null) { return null; }
            state.result = res;
            render();
            return res;
        }).catch(function (err) {
            state.busy = false;
            state.errorKey = 'merge.splitFailed';
            render();
            throw err;
        });
    }

    // --- обработчики (проверяются тестом с поддельным событием) ------------

    function handleInput(ev) {
        var t = ev && ev.target;
        if (!t) { return; }
        if (t.id === 'bmModel') { state.model = String(t.value || '').trim(); return; }
        if (t.id === 'bmStrategy') { state.strategy = String(t.value || 'layer'); return; }
        if (t.id === 'bmLinkMbps') {
            // R166: явный override канала. Пустое = «авто» (автозамер балансера).
            var mbps = parseInt(t.value, 10);
            if (isNaN(mbps) || mbps < 0) { mbps = 0; }
            state.bandwidthMbps = mbps;
            saveLinkSetting(mbps);
            state.linkHint = mbps > 0 ? T('merge.linkManualHint') : T('merge.linkAutoHint');
            return;
        }
        var backendId = t.getAttribute ? t.getAttribute('data-backend') : t.dataset && t.dataset.backend;
        if (t.id === 'bmBackend' && backendId) {
            state.selected[backendId] = !!t.checked;
            if (!state.selected[backendId] && state.masterId === backendId) { state.masterId = ''; }
            return;
        }
        if (t.id === 'bmMaster' && backendId) {
            state.masterId = backendId;
            state.selected[backendId] = true;
        }
    }

    function handleClick(ev) {
        var t = ev && ev.target;
        if (!t || !t.id) { return null; }
        if (t.id === 'bmMeasure') { return measureLink(); }
        if (t.id === 'bmCheck') { return check(); }
        if (t.id === 'bmApply') { return apply(); }
        if (t.id === 'bmSplit') {
            var groupId = t.getAttribute ? t.getAttribute('data-group') : t.dataset && t.dataset.group;
            return split(groupId);
        }
        return null;
    }

    function reset() {
        state.selected = {};
        state.masterId = '';
        state.plan = null;
        state.report = null;
        state.result = null;
        state.errorKey = '';
        state.splitInfo = null;
    }

    var api = {
        MOUNT_ID: MOUNT_ID,
        state: state,
        mount: mount,
        bindEvents: bindEvents,
        render: render,
        check: check,
        apply: apply,
        split: split,
        reset: reset,
        handleInput: handleInput,
        handleClick: handleClick,
        panelHtml: panelHtml,
        issuesHtml: issuesHtml,
        trafficHtml: trafficHtml,
        autoMount: autoMount,
        // чистые функции
        buildGroup: buildGroup,
        buildApplyPayload: buildApplyPayload,
        buildSplitPayload: buildSplitPayload,
        loadedModelsFor: loadedModelsFor,
        splitConfirmMessage: splitConfirmMessage,
        groupIdFor: groupIdFor,
        rpcEndpointFor: rpcEndpointFor,
        ruleForModel: ruleForModel,
        selectedIDs: selectedIDs,
        loadBackends: loadBackends
    };

    // autoMount — подключение без правок app.js: если контейнер есть, панель
    // рисуется сама. Ошибка загрузки бэкендов не летит в консоль необработанной:
    // она уже нарисована в панели текстом.
    // bindEvents — привязать обработчики к панели ОДИН раз.
    //
    // ЖИВОЙ ДЕФЕКТ (R166, 2026-10-17). Слушателей у панели не было НИКОГДА: в
    // репозитории handleClick/handleInput вызывали только тесты, а autoMount лишь
    // рисовал разметку. То есть кнопки «Проверить», «Применить», «Разъединить»
    // (и новое поле канала) в живом WebUI не срабатывали — панель выглядела
    // рабочей, но клики уходили в пустоту.
    //
    // Почему на КОРНЕ: render() заменяет root.innerHTML, а слушатели на контейнере
    // живут дальше — привязка не нужна на каждой перерисовке.
    function bindEvents() {
        if (!globalThis.document || !globalThis.document.getElementById) { return; }
        var root = globalThis.document.getElementById(MOUNT_ID);
        if (!root || !root.addEventListener) { return; }
        if (root.getAttribute && root.getAttribute('data-bm-bound') === '1') { return; }
        if (root.setAttribute) { root.setAttribute('data-bm-bound', '1'); }
        root.addEventListener('click', function (e) {
            var res = handleClick(e);
            if (res && typeof res.catch === 'function') {
                // Отказ показывается в панели (state.errorKey), поэтому здесь только
                // гасим unhandled rejection.
                res.catch(function () {});
            }
        });
        root.addEventListener('input', handleInput);
        root.addEventListener('change', handleInput);
    }

    function autoMount() {
        if (!globalThis.document || !globalThis.document.getElementById) { return; }
        if (!globalThis.document.getElementById(MOUNT_ID)) { return; }
        mount(MOUNT_ID).then(bindEvents, bindEvents);
    }

    if (globalThis.document) {
        if (globalThis.document.readyState === 'loading' && globalThis.document.addEventListener) {
            globalThis.document.addEventListener('DOMContentLoaded', autoMount);
        } else if (typeof setTimeout === 'function') {
            setTimeout(autoMount, 0);
        }
    }

    if (typeof window !== 'undefined') { window.BackendMerge = api; }
    if (typeof module !== 'undefined' && module.exports) { module.exports = api; }
})();
