// gguf-detail-models.js — R656 (2026-11-20): таб «Models» правой панели
// (GGUF) вынесен из gguf-renderer-detail-render.js (цель §5 — < 1500 строк).
//
// ЗАЧЕМ ВЫНЕСЕНО. Один связный слой: карточки моделей на диске, точный и
// эвристический план раскладки по слоям (R118), флаги загрузки, источники
// параметров (R119). Он СЧИТАЕТ, а не только рисует — причина меняться своя.
//
// ПОЧЕМУ ФАБРИКА. Слой берёт из основного модуля состояние (`M`, `state`),
// Utils и шесть хелперов/рендереров; список получен scripts/webui_extract_deps.js.
// `PARAM_SOURCE_LABELS/HINTS` объявлены ВНУТРИ слоя и возвращаются наружу: их
// читает редактор параметров (renderModelParamsEditor) в другом файле.
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: тела перенесены как есть, отступ сохранён.
(function () {
    'use strict';

    window.GgufDetailModels = function (ctx) {
        var M = ctx.M;
        var _ = ctx._;
        var state = ctx.state;
        var Utils = ctx.Utils;
        var loadedHandleFor = ctx.loadedHandleFor;
        var renderInventoryNote = ctx.renderInventoryNote;
        var formatElapsedShort = ctx.formatElapsedShort;
        var isUnloadingHandle = ctx.isUnloadingHandle;
        var unloadingElapsedMs = ctx.unloadingElapsedMs;
        var unloadingItemFor = ctx.unloadingItemFor;
        var renderModelParamsEditor = ctx.renderModelParamsEditor;
        // R674 (2026-11-21): ВТОРАЯ ПОЛОВИНА фикса. R672 добавил эти два ключа в
        // литерал вызывающего (gguf-renderer-detail-render.js:485-486), но фабрика
        // их не читала — тело продолжало видеть bare-имена, и таб «Модели» падал на
        // `ReferenceError: formatFileSize is not defined` (gguf-detail-models.js:56),
        // затем на `stripGGUF` (:68,74,79). Панель не перерисовывалась, а
        // `state.detailPane` уже был "models" — таб «залипал» на старой вкладке.
        // Без stripGGUF деградируют молча и обе ветки поиска значений профиля
        // (:532-534, :629-631): там стоит `typeof stripGGUF === 'function'`.
        var stripGGUF = ctx.stripGGUF;
        var formatFileSize = ctx.formatFileSize;

    function renderModelsPane() {
        const models = state.localModels || [];
        const inv = (state.diskInventory && state.selectedBackendId)
            ? state.diskInventory[state.selectedBackendId] : null;
        const note = renderInventoryNote(inv);
        if (models.length === 0) {
            // R93: пустой список обязан объяснять причину. «Каталог недоступен»
            // (удалённый bind-mount, отвалившийся том) — это не «моделей нет».
            const hint = (inv && inv.dirError)
                ? '<p style="font-size:var(--font-size-xs);margin-top:8px;color:var(--danger);">' +
                    Utils.escapeHtml(_('gguf.inventory_dir_error', { error: inv.dirError })) + '</p>'
                : '<p style="font-size:var(--font-size-xs);margin-top:8px;">' + _('gguf.no_models_on_disk_hint') + '</p>';
            return '<div class="gguf-empty-state">' +
                '<div class="empty-icon"><i class="fas fa-folder-open"></i></div>' +
                '<div>' + _('gguf.no_local_models') + '</div>' +
                hint +
            '</div>' + note;
        }
        // R304: донор не поднимает свои копии — состояние берём ОДИН раз на
        // отрисовку списка (не внутри map и без нового запроса: см.
        // M.donorMarkFor, источник — уже загруженный список бэкендов).
        const donorBlocked = (typeof M.loadBlockedByDonor === 'function')
            ? M.loadBlockedByDonor(state.selectedBackendId)
            : false;
        return note + models.map(function (m, idx) {
            const name = m.name || m.filename || m.path || '-';
            const size = m.size ? formatFileSize(m.size) : '-';
            const quant = m.quantization || m.quant || '-';
            // Round 24 (2026-08-04) Bug #2 fix: isLoaded check was unreliable.
            // Old check `(lm.model || lm.name) === name || lm.path === m.path` failed
            // when user loaded via API (e.g., balancer auto-warmup) with name without
            // .gguf extension: lm.name="Qwen3-Instruct-2507-q4km" vs
            // m.name="Qwen3-Instruct-2507-q4km.gguf" → no match → state shows
            // "Load" button for already-loaded model. Or reverse: stale loadedModels
            // after switch backends → "Unload" button for unloaded model.
            //
            // Robust check: compare normalized names (strip .gguf) AND check if
            // lm.path basename matches m.name (when lm.path is available).
            const nameNoExt = stripGGUF(name);
            const isLoaded = state.loadedModels.some(function (lm) {
                const lmName = lm.model || lm.name || '';
                // 1) Exact match (handles case where both have .gguf or both don't).
                if (lmName && lmName === name) return true;
                // 2) Normalized match (strip .gguf from both sides).
                if (lmName && stripGGUF(lmName) === nameNoExt) return true;
                // 3) Path basename match (m.name is a basename from /api/models/files;
                //    lm.path is full path from /api/models — compare basenames).
                if (lm.path) {
                    const lmPathBase = lm.path.split(/[\\/]/).pop();
                    if (lmPathBase === name || stripGGUF(lmPathBase) === nameNoExt) {
                        return true;
                    }
                }
                return false;
            });
            return '<div class="gguf-local-card">' +
                '<div class="gguf-local-icon"><i class="fas fa-cube"></i></div>' +
                '<div class="gguf-local-info">' +
                    '<div class="gguf-local-name">' + Utils.escapeHtml(name) + '</div>' +
                    '<div class="gguf-local-meta">' +
                        '<span>' + _('gguf.model_size') + ': ' + size + '</span>' +
                        '<span>' + _('gguf.quantization') + ': ' + quant + '</span>' +
                        (isLoaded ? '<span class="badge badge-success">' + _('gguf.tab_loaded') + '</span>' : '') +
                    '</div>' +
                '</div>' +
                '<div class="gguf-local-actions" style="display:flex;gap:6px;flex-wrap:wrap;">' +
                    (isLoaded
                        ? (isUnloadingHandle(name)
                            // Идёт выгрузка: вместо кнопки — состояние с таймером,
                            // иначе клик по «Выгрузить» снова уходил бы в API и
                            // возвращал «model not found».
                            ? '<span class="gguf-local-unloading" style="display:flex;align-items:center;gap:6px;font-size:var(--font-size-xs);color:var(--warning, #e0a030);">' +
                                '<i class="fas fa-spinner fa-spin"></i>' +
                                Utils.escapeHtml(_('gguf.unloading', 'Выгружается…')) + ' ' +
                                formatElapsedShort(unloadingElapsedMs(unloadingItemFor(name))) +
                              '</span>'
                            : '<button class="btn btn-sm btn-danger gguf-unload-btn" data-admin-only data-handle="' + Utils.escapeHtml(loadedHandleFor(name)) + '">' +
                                '<i class="fas fa-stop"></i> ' + _('gguf.unload_model') +
                              '</button>')
                        : (donorBlocked
                            // R304: у донора вместо кнопки — причина. Кнопку не
                            // гасим «молча»: оператор должен прочитать, ПОЧЕМУ
                            // здесь нельзя грузить (VRAM отдана мастеру), и что
                            // делать (снять роль донора).
                            ? '<span class="gguf-donor-note" style="display:inline-flex;align-items:center;gap:6px;font-size:var(--font-size-xs);color:var(--warning, #e0a030);max-width:320px;">' +
                                '<i class="fas fa-shield-halved"></i>' +
                                Utils.escapeHtml(_('models.donor_load_blocked', 'Донор не поднимает свои копии моделей: его VRAM отдана мастеру через rpc-server.')) +
                              '</span>'
                            : '<button class="btn btn-sm btn-primary gguf-load-btn" data-admin-only data-idx="' + idx + '">' +
                                '<i class="fas fa-play"></i> ' + _('gguf.load_model') +
                              '</button>')) +
                    '<button class="btn btn-sm btn-secondary gguf-delete-btn" data-admin-only data-idx="' + idx + '" title="' + _('gguf.delete_model') + '">' +
                        '<i class="fas fa-trash"></i>' +
                    '</button>' +
                    // R89: перенос модели на другой бэкенд (второй машине не нужно
                    // качать тот же файл из HuggingFace заново).
                    '<button class="btn btn-sm btn-secondary gguf-share-btn" data-admin-only data-share-model="' +
                        Utils.escapeHtml(name) + '" title="' + _('share.title', 'Поделиться моделью') + '">' +
                        '<i class="fas fa-share-nodes"></i>' +
                    '</button>' +
                '</div>' +
                // R126 (пункт 6): клик по имени модели раскрывает выбор параметров
                // запуска. Раньше параметры задавались только в отдельном блоке
                // настроек — «приходилось изгаляться в настройках отдельным полем».
                '<details class="gguf-mp-acc" data-model-acc="' + Utils.escapeHtml(name) + '">' +
                    '<summary class="gguf-mp-summary"><i class="fas fa-sliders"></i> ' +
                        Utils.escapeHtml(_('gguf.mp_open_for_model', 'Параметры запуска этой модели')) + '</summary>' +
                    renderModelParamsEditor(name, {}, { source: 'disk' }) +
                '</details>' +
            '</div>';
        }).join('');
    }

    /**
     * exactLayerPlanJS — R118.16 (2026-10-14): план слоёв из ФАКТА.
     *
     * Источник — device_layers_exact от cppworker: массив диапазонов
     * [{device,from,to,count,node}], собранный мостом из строк лога llama.cpp
     * («load_tensors: layer N assigned to device X»). Это не расчёт: оператор
     * сверяет числа с `docker logs` воркера.
     *
     * @param {Array} ranges device_layers_exact из
     *   /api/v1/cppworker/config/runtime (или /api/models)
     * @param {number} nLayers n_layers модели (для проверки полноты)
     * @returns {Object|null} план формы deriveLayerPlanJS с estimated=false;
     *   null = факта нет или он неполный (тогда вызывающий показывает расчёт).
     */
    function exactLayerPlanJS(ranges, nLayers) {
        nLayers = Number(nLayers) || 0;
        if (!Array.isArray(ranges) || ranges.length === 0 || nLayers <= 0) return null;
        let total = 0;
        for (let i = 0; i < ranges.length; i++) {
            const r = ranges[i];
            if (!r || !r.device || !(Number(r.count) > 0)) return null;
            total += Number(r.count);
        }
        // llama.cpp печатает ровно n_layer + 1 назначение (n_layer блоков плюс
        // выходной слой). Не сходится — это не наша раскладка (или воркер сменил
        // формат): честнее показать расчёт, чем чужой факт.
        if (total !== nLayers + 1) return null;

        const plan = {
            total_layers: total,
            gpu_layers: 0,
            cpu_layers: 0,
            estimated: false,
            note: 'точный список из лога llama.cpp (load_tensors: layer N assigned to device X); ' +
                'нумерация слоёв как в логе — 0…n_layer, последний номер это выходной слой',
            devices: []
        };
        ranges.forEach(function (r) {
            if (String(r.device).toUpperCase() === 'CPU') {
                plan.cpu_layers += Number(r.count);
                return;
            }
            plan.devices.push({
                device: r.device,
                kind: String(r.device).toUpperCase().indexOf('RPC') === 0 ? 'rpc' : 'local',
                node: r.node || '',
                count: Number(r.count),
                from: Number(r.from) || 0,
                to: Number(r.to) || 0
            });
        });
        plan.gpu_layers = plan.total_layers - plan.cpu_layers;
        return plan;
    }

    /**
     * deriveLayerPlanJS — R118 (2026-10-14): какие слои на каком устройстве.
     *
     * Зеркало балансерного deriveLayerPlanR118: GPU получает ПОСЛЕДНИЕ
     * gpu_layers слоёв, они делятся между устройствами подряд по долям
     * tensor_split с накопительным округлением ВВЕРХ, на CPU остаётся
     * n_layers − gpu_layers ПЛЮС выходной слой. Сверено с логом llama.cpp на трёх
     * живых прогонах:
     *   42 слоя, split [0.537, 0.463], все слои → CUDA0 23, RPC0 19, CPU 1
     *   42 слоя, split [0.537, 0.463], gpu=10  → CUDA0 6 (33–38), RPC0 4 (39–42), CPU 33
     *   42 слоя, split [0.605, 0.395], gpu=10  → CUDA0 7 (33–39), RPC0 3 (40–42), CPU 33
     *
     * @returns {Object|null} план вида {total_layers, gpu_layers, cpu_layers,
     *   estimated, note, devices:[{device,kind,node,count,from,to}]}
     */
    function deriveLayerPlanJS(nLayers, gpuLayers, split, localGPUCount, nodes) {
        nLayers = Number(nLayers) || 0;
        gpuLayers = Number(gpuLayers);
        if (!isFinite(gpuLayers)) gpuLayers = 0;
        if (nLayers <= 0) return null;
        if (gpuLayers < 0) gpuLayers = nLayers;
        if (gpuLayers > nLayers) gpuLayers = nLayers;

        const plan = {
            total_layers: nLayers,
            gpu_layers: gpuLayers,
            cpu_layers: gpuLayers > 0 ? (nLayers - gpuLayers + 1) : nLayers,
            estimated: true,
            note: 'расчёт по n_layers/gpu_layers/tensor_split; точный список — в логе cppworker ' +
                '(load_tensors: layer N assigned to device X)',
            devices: []
        };
        if (gpuLayers === 0) return plan;

        const rpcNodes = Array.isArray(nodes) ? nodes : [];
        const nLocal = Number(localGPUCount) > 0 ? Number(localGPUCount) : 1;
        const devs = [];
        for (let i = 0; i < nLocal; i++) devs.push({ name: 'CUDA' + i, kind: 'local', node: '' });
        rpcNodes.forEach(function (node, i) {
            devs.push({ name: 'RPC' + i, kind: 'rpc', node: String(node || '') });
        });
        if (devs.length === 0) devs.push({ name: 'CUDA0', kind: 'local', node: '' });

        const props = devs.map(function (_, i) {
            const v = (Array.isArray(split) && i < split.length) ? Number(split[i]) : 0;
            return (isFinite(v) && v > 0) ? v : 0;
        });
        let sum = props.reduce(function (a, b) { return a + b; }, 0);
        if (sum <= 0) {
            props.fill(1);
            sum = props.length;
        }

        const firstGPU = nLayers - gpuLayers + 1;
        let assigned = 0;
        let prevCum = 0;
        for (let i = 0; i < devs.length; i++) {
            let cum;
            if (i === devs.length - 1) {
                cum = gpuLayers;
            } else {
                let acc = 0;
                for (let k = 0; k <= i; k++) acc += props[k];
                cum = Math.ceil(gpuLayers * acc / sum);
            }
            if (cum < prevCum) cum = prevCum;
            const count = cum - prevCum;
            prevCum = cum;
            if (count <= 0 && i !== devs.length - 1) continue;
            assigned += count;
            plan.devices.push({
                device: devs[i].name,
                kind: devs[i].kind,
                node: devs[i].node,
                count: count,
                from: firstLayerFrom(firstGPU, assigned, count),
                to: firstGPU + assigned - 1
            });
        }
        return plan;
    }

    function firstLayerFrom(firstGPU, assigned, count) {
        return firstGPU + assigned - count;
    }

    /**
     * buildLayoutFlags — R118 (2026-10-14): пометки раскладки слоёв.
     *
     * ЖАЛОБА: «не хватает пометок в webui, что осуществлена раскладка на донора и
     * по какой модели и по каким слоям, а также замер пропускной способности
     * канала». Здесь это и появляется: чип «раскладка=...» со списком устройств и
     * диапазонов слоёв и отдельный чип «канал=...» с замером.
     *
     * Источник — GET /api/v1/cppworker/config/runtime (балансер): layout (узлы,
     * VRAM, link_mbps) + device_layers у каждой модели. R118.16: раскладка
     * помечена как ФАКТ (device_layers.estimated=false), если воркер отдал
     * device_layers_exact — перехваченные строки лога llama.cpp; иначе это
     * расчёт и так и подписано.
     *
     * @param {Object} m элемент state.loadedModels
     * @param {Object|null} layout state.rpcLayout
     * @returns {Array<{label:string,value:string,title:string,tone:string}>}
     */
    function buildLayoutFlags(m, layout) {
        m = m || {};
        const out = [];
        const plan = m.device_layers || null;
        const active = !!(layout && layout.active);
        const nodes = (layout && Array.isArray(layout.nodes)) ? layout.nodes : [];
        if (!active && !plan) return out;

        const nodeText = nodes.length ? nodes.join(', ') : '';
        const parts = [];
        if (plan && Array.isArray(plan.devices)) {
            plan.devices.forEach(function (d) {
                if (!d) return;
                let txt = String(d.device || '?');
                if (d.from && d.to) txt += ' ' + d.from + '–' + d.to;
                txt += ' (' + (d.count || 0) + ')';
                if (d.kind === 'rpc' && d.node) txt += ' → ' + d.node;
                parts.push(txt);
            });
        }
        if (plan && typeof plan.cpu_layers === 'number') {
            parts.push('CPU (' + plan.cpu_layers + ')');
        }

        const titleBits = [];
        titleBits.push(active
            ? 'Раскладка слоёв включена: GPU модели делится между локальной картой и узлом(ами) раскладки.'
            : 'Раскладка выключена — все слои считает локальная карта/CPU.');
        if (plan && plan.note) titleBits.push(plan.note);
        if (plan && plan.total_layers) {
            titleBits.push('Слоёв в модели: ' + plan.total_layers + ', на GPU: ' + (plan.gpu_layers || 0) +
                ', на CPU: ' + (plan.cpu_layers || 0) + '.');
        }

        if (active || parts.length) {
            out.push({
                label: 'раскладка',
                value: (nodeText ? 'донор ' + nodeText + ' · ' : 'только локально · ') + parts.join(' · '),
                title: titleBits.join(' '),
                tone: active ? 'ok' : 'off'
            });
        }

        // R118.2 (2026-10-14): балансер сам следит за доступностью узлов и
        // выключает раскладку, если узел пропал (иначе загрузка модели падала с
        // «Remote RPC server crashed»). Оператор должен видеть ЭТО, а не гадать,
        // почему модель поднялась локально.
        if (layout && layout.degraded) {
            out.push({
                label: 'раскладка',
                value: 'внимание: ' + (layout.degrade_reason || 'узлы раскладки недоступны'),
                title: 'Балансер проверяет доступность узлов раскладки и работает только на живых. ' +
                    (layout.degrade_reason || '') +
                    ' Локальная загрузка вместо отказа: узел вернётся — раскладка включится сама.',
                tone: 'warn'
            });
        }

        // R118.15 (2026-10-14): ДВА ЧИСЛА О КАНАЛЕ, и они означают разное.
        //
        //   link_mbps      — замер ОПЕРАТОРА (LB_RPC_LINK_MBPS): хост↔хост,
        //                    scripts/measure-network.ps1. На стенде 889 Мбит/с.
        //   link_auto_mbps — АВТОзамер балансера: он качает известный объём с
        //                    cppworker'а узла. Идёт через контейнерную сеть Docker
        //                    на обеих машинах и на стенде даёт 99..143 Мбит/с при
        //                    748 Мбит/с по прямому curl с хоста. Слои раскладки
        //                    передаёт cppworker (тоже контейнер), поэтому именно
        //                    автозамер ближе к тому, что реально получат слои.
        // Показываем оба и подписываем источники: одно «правильное» число тут
        // было бы обманом.
        const linkParts = [];
        if (layout && layout.link_mbps) {
            const mbps = (layout.link_mbps / 8).toFixed(1);
            const when = layout.link_measured_at ? ', замер ' + layout.link_measured_at : '';
            const src = layout.link_source ? ' (' + layout.link_source + when + ')' : '';
            linkParts.push({
                label: 'канал',
                value: layout.link_mbps + ' Мбит/с (' + mbps + ' МБ/с)',
                title: 'Пропускная способность канала до узла раскладки (замер оператора, хост↔хост): ' +
                    layout.link_mbps + ' Мбит/с = ' + mbps + ' МБ/с' + src +
                    '. Для раскладки это потолок: веса и префилл идут на узел, токены — обратно.',
                tone: 'normal'
            });
        }
        if (layout && layout.link_auto_mbps) {
            const mbpsAuto = (layout.link_auto_mbps / 8).toFixed(1);
            const node = layout.link_auto_node ? ' до ' + layout.link_auto_node : '';
            const whenAuto = layout.link_auto_at ? ', замер ' + layout.link_auto_at : '';
            linkParts.push({
                label: 'канал',
                value: layout.link_auto_mbps + ' Мбит/с (авто)',
                title: 'Автозамер балансера' + node + ': ' + layout.link_auto_mbps +
                    ' Мбит/с = ' + mbpsAuto + ' МБ/с' + whenAuto +
                    '. Балансер сам качает известный объём с cppworker\'а узла, поэтому ' +
                    'число идёт через контейнерную сеть Docker (NAT) и обычно НИЖЕ замера ' +
                    'оператора хост↔хост. Слои раскладки передаёт cppworker (тоже контейнер), ' +
                    'так что для бюджета раскладки ориентируйтесь на это число.',
                tone: 'normal'
            });
        }
        for (const part of linkParts) out.push(part);
        return out;
    }

    /**
     * buildLoadedFlags — R83/v67 (2026-10-02): строка состояния ЗАГРУЖЕННОЙ
     * модели со ВСЕМИ флагами, с которыми она реально работает.
     *
     * ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «не выводится информация о всех флагах в строке
     * состояния Загруженные у модели на странице GGUF Models». Раньше в строке
     * были только ctx_size и VRAM — по ней нельзя было понять, применились ли к
     * модели параллельность, kv-cache, flash attention и галочка «размышления»
     * (именно поэтому «флаг включён, а размышлений нет» выглядело необъяснимо).
     *
     * Источники — оба, с приоритетом runtime (cppworker отдаёт фактические
     * значения загрузки), а балансерный loadedModels дополняет тем, что знает
     * только он (parallel/maxSlots/kvCacheType/reasoningEnabled/context_per_seq).
     *
     * @param {Object} m  элемент state.loadedModels (ответ балансера)
     * @param {Object|null} rt runtime-параметры модели (cppworker config/runtime)
     * @returns {Array<{label:string,value:string,title:string,tone:string}>}
     */
    // R119 (2026-10-15, пункт 4): источники параметров загрузки.
    //
    // ЗАЧЕМ. Оператор видел только значения и не мог понять, какой слой их поставил
    // (явный запрос клиента / per-model профиль / «настройки по умолчанию» /
    // env cppworker), поэтому вопрос «настройки вообще применяются?» оставался
    // догадкой. Балансер считает источник при загрузке и отдаёт его в
    // loadedModels[].parameters: {"contextLength":{"value":8192,"source":"profile"}}.
    //
    // Функция чистая (без DOM) — её проверяет node-тест
    // gguf-param-sources-r119.test.js.
    var PARAM_SOURCE_LABELS = {
        request: 'запрос',
        profile: 'профиль',
        default_profile: 'по умолчанию',
        backend: 'cppworker'
    };

    // PARAM_SOURCE_HINTS — что означает источник (title для оператора).
    var PARAM_SOURCE_HINTS = {
        request: 'Значение пришло в самом запросе на загрузку (клиент/WebUI-форма)',
        profile: 'Значение взято из профиля этой модели',
        default_profile: 'Значение взято из «настроек по умолчанию для новых моделей»',
        backend: 'Никто не задавал — cppworker использует свой env/config-дефолт'
    };

    function buildParamSources(parameters) {
        if (!parameters || typeof parameters !== 'object') return [];
        const order = ['contextLength', 'contextPerSeq', 'parallel', 'batchSize',
                       'gpuLayers', 'flashAttn', 'useMmap', 'kvCacheType', 'enableReasoning'];
        const keys = Object.keys(parameters).sort(function (a, b) {
            const ia = order.indexOf(a), ib = order.indexOf(b);
            return (ia < 0 ? 99 : ia) - (ib < 0 ? 99 : ib);
        });
        const out = [];
        keys.forEach(function (key) {
            const entry = parameters[key];
            if (!entry || typeof entry !== 'object') return;
            const source = String(entry.source || '');
            let value = entry.value;
            if (value === undefined || value === null || value === '') {
                // nil = «никто не задал»: показываем прочерк, а не пустоту, иначе
                // строка выглядит как потерянное значение.
                value = '—';
            }
            out.push({
                key: key,
                value: String(value),
                source: source,
                sourceLabel: PARAM_SOURCE_LABELS[source] || source || '?',
                title: PARAM_SOURCE_HINTS[source] || ''
            });
        });
        return out;
    }

    function renderParamSources(parameters) {
        const items = buildParamSources(parameters);
        if (items.length === 0) return '';
        const parts = items.map(function (it) {
            return '<span title="' + Utils.escapeHtml(it.key + ': ' + it.title) + '" ' +
                'style="white-space:nowrap;">' +
                Utils.escapeHtml(it.key) + '=' + Utils.escapeHtml(it.value) +
                '<span style="color:var(--text-muted);">←' + Utils.escapeHtml(it.sourceLabel) + '</span>' +
            '</span>';
        }).join('');
        return '<div class="gguf-loaded-model-info" style="margin-top:4px;display:flex;gap:10px;' +
            'flex-wrap:wrap;font-family:monospace;font-size:var(--font-size-xs);color:var(--text-secondary);">' +
            '<span style="color:var(--text-muted);">' +
            Utils.escapeHtml(_('gguf.param_sources') || 'источники') + ':</span>' + parts +
        '</div>';
    }

    function buildLoadedFlags(m, rt) {
        m = m || {};
        rt = rt || {};
        const pick = function () {
            for (let i = 0; i < arguments.length; i++) {
                const v = arguments[i];
                if (v !== undefined && v !== null && v !== '') return v;
            }
            return null;
        };
        // factOf — первое непустое поле ИЗ СПИСКА имён (факт воркера приходит
        // snake_case, балансер отдаёт и camelCase).
        const factOf = function (obj, keys) {
            if (!obj) return null;
            for (let i = 0; i < keys.length; i++) {
                const v = obj[keys[i]];
                if (v !== undefined && v !== null && v !== '') return v;
            }
            return null;
        };
        // intentOf — ЧТО ПРОСИЛ ОПЕРАТОР (источники параметров R119):
        // m.parameters — параметры этой загрузки, rt.parameters — runtime-документ,
        // state.modelProfiles — профиль модели (R162: единственный доступный
        // источник намерения, когда загрузка шла по профилю).
        const intentOf = function (key) {
            const fromM = m.parameters && m.parameters[key] && typeof m.parameters[key].value === 'number'
                ? m.parameters[key].value : null;
            if (fromM !== null) return fromM;
            const fromRt = rt.parameters && rt.parameters[key] && typeof rt.parameters[key].value === 'number'
                ? rt.parameters[key].value : null;
            if (fromRt !== null) return fromRt;
            const profs = (M.state && M.state.modelProfiles) || null;
            if (!profs) return null;
            const name = String(m.name || m.model || '');
            const base = m.path ? String(m.path).split(/[\\/]/).pop() : '';
            const keys = [name, base];
            if (typeof stripGGUF === 'function') {
                keys.push(stripGGUF(name));
                if (base) keys.push(stripGGUF(base));
            }
            for (let i = 0; i < keys.length; i++) {
                const entry = keys[i] ? profs[keys[i]] : null;
                if (entry && typeof entry[key] === 'number') return entry[key];
            }
            return null;
        };
        const flags = [];

        // ── ctx ─────────────────────────────────────────────────────────────
        // ФАКТ И НАМЕРЕНИЕ — РАЗНЫЕ ИСТОЧНИКИ, И ФАКТ ГЛАВНЕЕ (R162, 2026-10-17).
        //
        // ЖИВОЙ ДЕФЕКТ (нашёл инструмент, дошедший до чипов). Воркер сообщал
        // инстанс с окном 8192 и 38 слоями на GPU, а чип показывал 32768 и 19/42:
        // предпочтение отдавалось runtime-документу (state.runtimeModels), который
        // к тому моменту был устаревшим. Оператор видел в карточке числа, которых
        // в инстансе нет, — ровно та жалоба, ради которой карточка и делалась.
        //
        // Теперь: значение = ФАКТ инстанса; если runtime-документ расходится,
        // это НАЗЫВАЕТСЯ в чипе и подсказке, а не подменяет факт.
        const factCtx = factOf(m, ['context_size', 'contextSize', 'ctxSize', 'contextLength']);
        const rtCtx = factOf(rt, ['context_size']);
        const ctx = pick(factCtx, rtCtx);
        const perSeq = pick(factOf(m, ['context_per_seq', 'contextPerSeq']), factOf(rt, ['context_per_seq']));
        const ctxOverrideDiffers = factCtx !== null && rtCtx !== null && Number(rtCtx) !== Number(factCtx);
        if (ctx !== null) {
            // R155 (2026-10-16): окно могло быть ПОДНЯТО по запросу клиента.
            //
            // ЖИВОЙ СЛУЧАЙ: профиль оператора задаёт 8192 (4096 на слот), а инстанс
            // работает с 65536 (32768 на слот) — балансер увеличивает окно под
            // требовательный запрос клиента. Оператор узнавал об этом только по логу
            // воркера: карточка показывала факт без объяснения, а профиль рядом
            // показывал другое число. Теперь видно прямо в чипе: стрелка и
            // предупреждающий тон, а в подсказке — оба числа и причина.
            const intended = intentOf('contextLength');
            const actual = Number(factCtx);
            const grew = intended !== null && Number.isFinite(actual) && actual > intended;
            let title;
            let tone = 'normal';
            if (grew) {
                title = 'Окно БОЛЬШЕ заданного оператором: ' + actual +
                    ' вместо ' + intended +
                    '. Так бывает, когда балансер поднимает окно под требовательный запрос клиента,' +
                    ' либо модель поднимали с другими параметрами. Профиль не менялся;' +
                    ' вернуть своё значение можно повторным применением параметров.';
                tone = 'warn';
            } else if (ctxOverrideDiffers) {
                title = 'Фактическое окно инстанса: ' + factCtx +
                    '. В runtime-документе записано ' + rtCtx + ' — расхождение: документ не ' +
                    'описывает текущий инстанс (после перезагрузки с другими параметрами он ' +
                    'обновляется отдельно). Показываем ФАКТ.';
                tone = 'warn';
            } else {
                title = 'Суммарное окно контекста' + (perSeq !== null ? '; на один слот (клиента): ' + perSeq : '');
            }
            flags.push({
                label: 'ctx',
                value: String(ctx) +
                    (perSeq !== null && String(perSeq) !== String(ctx) ? ' (' + perSeq + '/slot)' : '') +
                    (grew ? ' ↑' : '') +
                    (ctxOverrideDiffers ? ' (runtime ' + rtCtx + ')' : ''),
                title: title,
                tone: tone
            });
        }

        const parallel = pick(m.parallel, m.maxSlots, rt.parallel);
        if (parallel !== null) {
            flags.push({
                label: 'parallel',
                value: String(parallel) + (m.maxSlots ? ' (max ' + m.maxSlots + ')' : ''),
                title: 'Число параллельных слотов (llama.cpp n_parallel)',
                tone: 'normal'
            });
        }

        // ── kv ──────────────────────────────────────────────────────────────
        // R213 (2026-10-21). Раньше здесь стояло `value: kv !== null ? kv : 'f16'`
        // — и это была ДОГАДКА, напечатанная как факт. Живая жалоба оператора:
        // «в настройках стоит q4_0, а в отображении загруженных параметров f16».
        // Причина была не в модели, а в этом рендере: /api/v1/gguf/backends
        // отдавал только name/status/contextSize, kv_cache_type в карточку не
        // приезжал вовсе, и карточка молча подставляла f16.
        //
        // Теперь: (1) балансер присылает факт kv_cache_type (R213), (2) если
        // факта всё же нет — показываем значение ПРОФИЛЯ с явной пометкой
        // «профиль», (3) если и профиля нет — честный прочерк, а не f16.
        const kvFact = pick(factOf(m, ['kv_cache_type', 'kvCacheType']), factOf(rt, ['kv_cache_type']));
        const kvProfile = (function () {
            const profs = (M.state && M.state.modelProfiles) || null;
            if (!profs) return null;
            const name = String(m.name || m.model || '');
            const base = m.path ? String(m.path).split(/[\\/]/).pop() : '';
            const keys = [name, base];
            if (typeof stripGGUF === 'function') {
                keys.push(stripGGUF(name));
                if (base) keys.push(stripGGUF(base));
            }
            for (let i = 0; i < keys.length; i++) {
                const entry = keys[i] ? profs[keys[i]] : null;
                if (!entry) continue;
                const v = entry.kvCacheType || entry.kv_cache_type;
                if (v) return String(v);
            }
            return null;
        })();
        let kvValue = '—';
        let kvTone = 'warn';
        let kvTitle = 'Воркер не сообщил тип KV-cache этого инстанса, и в профиле он не задан: ' +
            'действует умолчание llama.cpp (f16). Задайте тип в блоке «можно задать» ниже — ' +
            'он применится при следующей загрузке/перезагрузке.';
        if (kvFact !== null) {
            kvValue = String(kvFact);
            kvTone = 'normal';
            kvTitle = 'Тип KV-cache загруженного инстанса (факт из воркера)';
        } else if (kvProfile) {
            kvValue = kvProfile + ' (профиль)';
            kvTone = 'warn';
            kvTitle = 'Воркер не сообщил тип KV-cache инстанса. Показано значение ПРОФИЛЯ ' +
                kvProfile + ': оно применится при следующей загрузке/перезагрузке модели. ' +
                'Если инстанс поднимался до появления профиля, фактически может быть f16.';
        }
        flags.push({
            label: 'kv',
            value: kvValue,
            title: kvTitle,
            tone: kvTone
        });

        // R162 (2026-10-17): слои и n_layers берём у ИНСТАНСА, runtime-документ —
        // только запасной источник (см. разбор у ctx).
        //
        // ПРО device_layers_exact. Воркер отдаёт ТОЧНУЮ раскладку по устройствам
        // (device_layers_exact: [{device:'CUDA0',count:42},{device:'CPU',count:1}]),
        // и она ГЛАВНЕЕ полей gpu_layers/numGpuLayers: наблюдалось, что чип показывал
        // «gpu_layers=0/42 — модель целиком на CPU» при фактических 42/42 на GPU
        // (живой стенд 2026-10-17). Такое предупреждение — ложь, которая отправляет
        // оператора искать несуществующую проблему.
        const gpuFromDevices = (function () {
            const arr = factOf(m, ['device_layers_exact', 'deviceLayersExact']);
            if (!arr || !arr.length) return null;
            let sum = 0;
            for (let i = 0; i < arr.length; i++) {
                const d = arr[i] || {};
                const dev = String(d.device || '');
                if (!dev || dev.toUpperCase() === 'CPU') continue;
                sum += Number(d.count) || 0;
            }
            return sum > 0 ? sum : null;
        })();
        const gpu = pick(gpuFromDevices, factOf(m, ['gpu_layers', 'numGpuLayers']), factOf(rt, ['gpu_layers']));
        const layers = pick(factOf(m, ['n_layers', 'nLayers']), factOf(rt, ['n_layers']));
        if (gpu !== null) {
            const gpuNum = Number(gpu);
            const layersNum = layers !== null ? Number(layers) : 0;
            let tone = 'normal';
            if (gpuNum === 0) tone = 'warn';
            else if (layersNum > 0 && gpuNum > 0 && gpuNum < layersNum) tone = 'warn';
            // R158 (2026-10-16): ЯВНАЯ подпись частичной выгрузки.
            //
            // Живой факт: карточка просит numGpuLayers=-1 («все слои»), а memfit
            // поднимает модель частично — наблюдалось 19 из 42 и 10 из 42. Чип показывал
            // только «19/42» с предупреждающим тоном, и оператор не видел, ЧТО осталось
            // в RAM и почему. Теперь это сказано словами.
            const partial = layersNum > 0 && gpuNum > 0 && gpuNum < layersNum;
            const inRam = partial ? (layersNum - gpuNum) : 0;
            flags.push({
                label: 'gpu_layers',
                value: layersNum > 0
                    ? gpuNum + '/' + layersNum + (partial ? ' (в RAM ' + inRam + ')' : '')
                    : String(gpuNum),
                title: gpuNum === 0
                    ? 'Модель целиком на CPU — генерация будет очень медленной'
                    : (partial
                        ? 'Частичная выгрузка: на GPU ' + gpuNum + ' слоёв из ' + layersNum +
                          ', остальные ' + inRam + ' — в RAM. Так memfit подгоняет модель под ' +
                          'свободную память: numGpuLayers задаёт НАМЕРЕНИЕ, а фактическое число ' +
                          'слоёв зависит от памяти.'
                        : 'Сколько слоёв модели на GPU (из общего числа слоёв)'),
                tone: tone
            });
        }

        const fa = pick(factOf(m, ['flash_attn_type', 'flashAttnType']), factOf(rt, ['flash_attn_type']));
        if (fa !== null) {
            const faNum = Number(fa);
            flags.push({
                label: 'flash_attn',
                value: faNum === 1 ? 'on' : (faNum === 0 ? 'off' : 'auto'),
                title: 'Flash attention: auto = решает llama.cpp по архитектуре GPU',
                tone: 'normal'
            });
        }

        const mmap = pick(factOf(m, ['use_mmap', 'useMmap']), factOf(rt, ['use_mmap']));
        if (mmap !== null) {
            flags.push({
                label: 'mmap',
                value: mmap ? 'on' : 'off',
                title: 'use_mmap: отображение весов файла в память',
                tone: 'normal'
            });
        }

        // reasoning_enabled — то самое поле, которое показывает, «применилась ли
        // галочка размышлений» из профиля/WebUI (см. /api/models на cppworker).
        const reasoning = pick(m.reasoningEnabled, rt.reasoning_enabled,
            (m.capabilities ? m.capabilities.reasoning : null));
        if (reasoning !== null && reasoning !== undefined) {
            const on = reasoning === true || reasoning === 'true';
            flags.push({
                label: 'reasoning',
                value: on ? 'on' : 'off',
                title: on
                    ? 'Размышления включены: промпт получает thinking-инструкцию, рассуждение отделяется от ответа'
                    : 'Размышления выключены (галочка «Enable reasoning» в настройках модели снята)',
                tone: on ? 'ok' : 'off'
            });
        }

        const batched = pick(factOf(m, ['batched_parallel', 'batchedParallel']), factOf(rt, ['batched_parallel']));
        if (batched !== null) {
            flags.push({
                label: 'batched',
                value: batched ? 'on' : 'off',
                title: 'Один llama_decode на несколько сессий (true parallel inference)',
                tone: batched ? 'ok' : 'off'
            });
        }

        // R463-R465 (2026-11-06): сколько токенов ответа реально разрешено ЭТОЙ
        // модели, если клиент не прислал num_predict, и до какого потолка воркер
        // может поднять бюджет сам.
        //
        // ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «num_predict в env выставлен, а в WebUI его
        // значения не увидеть». До этой правки число не доезжало: воркер отдаёт
        // default_num_predict/output_budget_ceiling в runtime, балансер декодирует
        // ответ в свою структуру и пересобирает его для WebUI — без этих полей
        // они терялись (ровно класс R134).
        const npDefault = pick(factOf(rt, ['default_num_predict', 'defaultNumPredict']),
            factOf(m, ['default_num_predict', 'defaultNumPredict']));
        const npCeiling = pick(factOf(rt, ['output_budget_ceiling', 'outputBudgetCeiling']),
            factOf(m, ['output_budget_ceiling', 'outputBudgetCeiling']));
        if (npDefault !== null || npCeiling !== null) {
            const npValue = (npDefault !== null ? String(npDefault) : '?') +
                (npCeiling !== null ? ' (потолок ' + String(npCeiling) + ')' : '');
            flags.push({
                label: 'num_predict',
                value: npValue,
                title: 'Бюджет ответа без клиентского num_predict/max_tokens: ' +
                    (npDefault !== null ? String(npDefault) + ' токенов' : 'неизвестен') +
                    (npCeiling !== null
                        ? '; выше ' + String(npCeiling) + ' воркер не поднимает (CPPWORKER_MAX_N_PREDICT)'
                        : '') +
                    '. Клиентский num_predict важнее и не понижается; явный max_output_tokens может поднять бюджет до потолка.',
                tone: 'normal'
            });
        }

        const quant = pick(m.quantization);
        if (quant !== null) {
            flags.push({label: 'quant', value: String(quant), title: 'Квантование весов', tone: 'normal'});
        }

        // R118: пометки раскладки слоёв (узел-донор, слои по устройствам, канал).
        buildLayoutFlags(m, state.rpcLayout).forEach(function (f) { flags.push(f); });

        // R182 (2026-10-19): РАСКЛАДКА НЕ ПРИМЕНИЛАСЬ - отдельный чип.
        //
        // ЗАЧЕМ. Воркер отдаёт факт сверки «переданные серверы vs фактическая
        // раскладка слоёв» (layout_degraded + причина, R176). До этой правки факт
        // жил только в API и логах: оператор видел «gpu_layers=42/43» на одной карте,
        // не понимал, почему вторая машина простаивает, и узнавал о деградации по
        // скорости. Причина приходит с сервера, поэтому и подпись берём оттуда.
        const degraded = factOf(m, ['layout_degraded', 'layoutDegraded']) === true
            || factOf(rt, ['layout_degraded', 'layoutDegraded']) === true;
        if (degraded) {
            const why = pick(factOf(m, ['layout_degraded_reason', 'layoutDegradedReason']),
                factOf(rt, ['layout_degraded_reason', 'layoutDegradedReason']));
            flags.push({
                label: _('gguf.layout_degraded', 'раскладка'),
                value: _('gguf.layout_degraded_value', 'НЕ применена'),
                title: why || _('gguf.layout_degraded_hint',
                    'Узлы раскладки переданы в загрузку, но ни один не стал устройством ' +
                    'модели: слои остались на локальных устройствах. Проверьте доступность ' +
                    'узла и повторите применение профиля.'),
                tone: 'warn'
            });
        }

        return flags;
    }

        return {
            renderModelsPane: renderModelsPane,
            exactLayerPlanJS: exactLayerPlanJS,
            deriveLayerPlanJS: deriveLayerPlanJS,
            firstLayerFrom: firstLayerFrom,
            buildLayoutFlags: buildLayoutFlags,
            buildParamSources: buildParamSources,
            renderParamSources: renderParamSources,
            buildLoadedFlags: buildLoadedFlags,
            PARAM_SOURCE_LABELS: PARAM_SOURCE_LABELS,
            PARAM_SOURCE_HINTS: PARAM_SOURCE_HINTS
        };
    };
})();
