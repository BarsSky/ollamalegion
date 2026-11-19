// renderers-params.js — R643 (2026-11-19): рендер параметров загрузки модели —
// image-воркер (`renderImageParams`) и llama.cpp (`renderOllamaParams`), плюс
// форматирование возраста и времени загрузки.
//
// ЗАЧЕМ ВЫНЕСЕНО. `renderers.js` держал 3031 строку: таблица бэкендов, страницы
// моделей и сессий, GPU-карточки, очередь, логи, агенты, GGUF — и отдельный слой
// «как показать параметры загрузки». Слой большой (≈400 строк), узкий по теме и
// меняется по своим причинам (R346/R348 — возраст и время загрузки, R-Image —
// карточка image-воркера), поэтому вынесен первым.
//
// ПОЧЕМУ ФАБРИКА, А НЕ ЕЩЁ ОДИН ГЛОБАЛЬНЫЙ ОБЪЕКТ. Слой берёт из `renderers.js`
// ровно две вещи (`Utils` и `isZeroTimestamp`) — здесь они приходят аргументами.
// Так зависимости видны в одной строке, а не «где-то в window», и порядок
// подключения не может тихо разойтись с реальностью: не передали — упадёт на
// первой же строке, а не покажет пустую карточку.
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: код перенесён как есть (перенос с добавлением отступа).
(function () {
    'use strict';

    window.RenderersParams = function (ctx) {
        var Utils = ctx.Utils;
        var isZeroTimestamp = ctx.isZeroTimestamp;
        // Из деструктуризации Utils в renderers.js слой использует эти четыре
        // функции и локальный переводчик _t. Передаём их явно — тогда пропущенная
        // зависимость падает на первой строке (как escapeHtml упал в этой правке),
        // а не превращается в пустую карточку.
        var escapeHtml = ctx.escapeHtml;
        var formatNumber = ctx.formatNumber;
        var formatMB = ctx.formatMB;
        var percent = ctx.percent;
        var _t = ctx._t;
        // Таблица подписей параметров llama.cpp остаётся в renderers.js (её читают и
        // другие рендереры) — передаём по ссылке.
        var OLLAMA_PARAMS = ctx.OLLAMA_PARAMS;
        function renderImageParams(backend) {
            var img = Utils.getBackendImage(backend) || {};
            var models = Utils.getBackendImageModels(backend);
            var port = Utils.getBackendImagePort(backend);
            var workerState = Utils.getBackendImageState(backend);
            var lastError = Utils.getBackendImageLastError(backend);
            // Локальный t: _t() возвращает сам ключ, если i18n ещё не загружен -
            // в этом случае показываем английский fallback, а не "image.worker_port".
            var t = function (key, fallback) {
                var v = _t(key);
                return (v && v !== key) ? v : fallback;
            };

            var html = '<div class="be-detail-section"><div class="be-detail-title">🎨 ' +
                escapeHtml(t('image.section_worker', 'image.cpp worker')) + '</div><div class="be-params-grid">';
            html += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(t('image.worker_port', 'Worker port')) +
                '</span><span class="be-param-value">' + (port > 0 ? port : escapeHtml(t('image.port_unset', 'not set'))) + '</span></div>';
            if (workerState) {
                html += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(t('image.worker_state', 'Worker state')) +
                    '</span><span class="be-param-value">' + Utils.imageStateBadge(workerState) + '</span></div>';
            }
            if (backend.host) {
                html += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(t('backends.host', 'Host')) +
                    '</span><span class="be-param-value">' + escapeHtml(String(backend.host)) + '</span></div>';
            }
            if (img.vramFreeMb !== undefined || img.vramTotalMb !== undefined) {
                html += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(t('image.worker_vram', 'Worker VRAM')) +
                    '</span><span class="be-param-value">' + escapeHtml(Utils.formatVramPair(img.vramFreeMb, img.vramTotalMb)) + '</span></div>';
            }
            // R90: RAM ХОСТА рядом с VRAM. Для image-моделей с offload в RAM это
            // главный ресурс (веса живут в оперативке), и без него в блоке состояния
            // воркера не понять, почему модель «не влезает».
            var workerSys = backend.system || {};
            if (workerSys.memoryTotal > 0) {
                html += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(t('metrics.ram_usage', 'RAM usage')) +
                    '</span><span class="be-param-value">' + formatMB(workerSys.memoryUsed) + ' / ' + formatMB(workerSys.memoryTotal) +
                    ' (' + percent(workerSys.memoryUsed, workerSys.memoryTotal).toFixed(1) + '%)</span></div>';
            }
            if (img.currentModel) {
                html += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(t('image.current_model', 'Current model')) +
                    '</span><span class="be-param-value">' + escapeHtml(String(img.currentModel)) + '</span></div>';
            }
            if (img.updatedAt) {
                html += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(t('image.updated', 'Updated')) +
                    '</span><span class="be-param-value">' + escapeHtml(Utils.formatDate(img.updatedAt)) + '</span></div>';
            }
            html += '</div>';
            if (lastError) {
                // Данные могли протухнуть: воркер ответил ошибкой, поэтому метрики
                // ниже - последний успешный снимок, а не текущее состояние.
                html += '<div style="margin-top:8px;padding:6px 8px;border-radius:4px;font-size:var(--font-size-xs);' +
                    'background:rgba(240,173,78,0.12);border:1px solid var(--warning);color:var(--warning)">⚠️ ' +
                    escapeHtml(t('image.last_error', 'Worker error')) + ': ' + escapeHtml(lastError) + '. ' +
                    escapeHtml(t('image.last_error_stale', 'The values below may be stale: the worker reported an error instead of fresh data.')) +
                    '</div>';
            }
            html += '</div>';

            // Section: модели воркера (image.models) со статусом, размером и VRAM.
            html += '<div class="be-detail-section"><div class="be-detail-title">' + escapeHtml(t('image.models_title', 'Image models')) + '</div>';
            if (!models.length) {
                html += '<div class="be-params-grid"><div class="be-param-item" style="grid-column:1/-1;">' +
                    '<span class="be-param-value" style="color:var(--text-secondary)">' + escapeHtml(t('image.no_models', 'No image models on this backend.')) +
                    '</span></div></div>';
            } else {
                html += '<div class="be-params-grid">';
                models.forEach(function (m) {
                    var sizeBytes = Utils.imageSizeBytes(m);
                    var vramMb = Utils.imageVramEstimateMb(m);
                    var bits = [];
                    if (sizeBytes > 0) bits.push(Utils.formatMB(sizeBytes / 1024 / 1024));
                    if (vramMb > 0) bits.push('VRAM ' + Utils.formatMB(vramMb));
                    if (m.activeQueries) bits.push(t('image.col_active', 'Active requests') + ': ' + m.activeQueries);
                    var isCurrentModel = img.currentModel && img.currentModel === m.name;
                    html += '<div class="be-param-item" style="grid-column:1/-1;">' +
                        '<span class="be-param-label">' + escapeHtml(m.name) +
                        (isCurrentModel ? ' <span class="badge badge-success">' + escapeHtml(t('image.active_badge', 'active')) + '</span>' : '') +
                        (m.family ? ' <span style="color:var(--text-secondary);font-size:var(--font-size-xs)">' + escapeHtml(String(m.family)) + '</span>' : '') +
                        '</span><span class="be-param-value">' + Utils.imageStateBadge(m.state) +
                        (bits.length ? ' <span style="color:var(--text-secondary);font-size:var(--font-size-xs)">' + escapeHtml(bits.join(' | ')) + '</span>' : '') +
                        '</span></div>';
                });
                html += '</div>';
            }
            html += '</div>';

            // Section: Disk / Network (общий для всех типов бэкендов).
            const sys = backend.system || {};
            if (sys.diskTotal !== undefined || sys.diskUsed !== undefined || sys.diskFree !== undefined || sys.networkRX !== undefined || sys.networkTX !== undefined) {
                html += '<div class="be-detail-section"><div class="be-detail-title">' + escapeHtml(_t('renderers.section_disk_network')) + '</div><div class="be-params-grid">';
                if (sys.diskTotal !== undefined) html += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.disk_total')) + '</span><span class="be-param-value">' + formatMB(sys.diskTotal) + '</span></div>';
                if (sys.diskUsed !== undefined) html += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.disk_used')) + '</span><span class="be-param-value">' + formatMB(sys.diskUsed) + '</span></div>';
                if (sys.diskFree !== undefined) html += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.disk_free')) + '</span><span class="be-param-value">' + formatMB(sys.diskFree) + '</span></div>';
                if (sys.networkRX !== undefined) html += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.network_rx')) + '</span><span class="be-param-value">' + formatMB(sys.networkRX) + '</span></div>';
                if (sys.networkTX !== undefined) html += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.network_tx')) + '</span><span class="be-param-value">' + formatMB(sys.networkTX) + '</span></div>';
                html += '</div></div>';
            }

            // R-Image follow-up (2026-10-03): рядом с image-бэкендом показываем, как к
            // нему подключиться (ключ + эндпоинты клиентов). Блок общий для всех типов
            // бэкендов — см. webui/js/modules/client-access.js.
            if (window.ClientAccess && typeof window.ClientAccess.render === 'function') {
                html += window.ClientAccess.render(backend);
            }

            return html;
        }

        // R346: время загрузки модели в читаемом виде.
        //
        // Почему не просто iso-строка: cppworker отдаёт RFC3339Nano с наносекундами,
        // а оператор сравнивает «модель поднялась в 12:04» с логами и часами. Для
        // сегодняшней загрузки достаточно времени, для старой — дата и время.
        // Пустая/битая строка → '' (вызывающий не рисует блок), а не «Invalid Date».
        // R348: «сколько назад» для последнего замера VRAM узлов.
        //
        // Почему человеческие единицы, а не секунды: строка «последний замер 37 мин
        // назад» читается сразу, а «2220 с» требует пересчёта в голове. Ноль и
        // отрицательные значения — «только что»: снимок сделан прямо сейчас.
        function formatAgeR348(sec) {
            var s = Number(sec || 0);
            if (!isFinite(s) || s <= 0) return _t('renderers.age_just_now') || 'just now';
            if (s < 60) return Math.round(s) + ' ' + (_t('renderers.age_sec') || 'sec');
            if (s < 3600) return Math.round(s / 60) + ' ' + (_t('renderers.age_min') || 'min');
            return Math.round(s / 3600) + ' ' + (_t('renderers.age_hour') || 'h');
        }

        function formatLoadedAtR346(iso) {
            if (!iso) return '';
            var d = new Date(iso);
            if (isNaN(d.getTime())) return '';
            var pad = function (n) { return (n < 10 ? '0' : '') + n; };
            var time = pad(d.getHours()) + ':' + pad(d.getMinutes());
            var now = new Date();
            var sameDay = d.getFullYear() === now.getFullYear() &&
                d.getMonth() === now.getMonth() && d.getDate() === now.getDate();
            return sameDay ? time : (pad(d.getDate()) + '.' + pad(d.getMonth() + 1) + ' ' + time);
        }

        function renderOllamaParams(backend) {
            // Определяем тип бэкенда (используем унифицированную утилиту)
            var bt = Utils.getBackendType(backend);
            var isLlamaCpp = (bt === 'llama_cpp');

            // R-Image (2026-10-02): image-бэкенд (stable-diffusion.cpp). У него нет
            // runtimeFlags/backendCapacity/loadedModels - без отдельной ветки
            // раскрытая строка на странице «Бэкенды» показывала пустоту, хотя
            // /api/v1/metrics отдаёт image.state / image.models / vramFreeMb.
            if (bt === 'image_cpp') {
                return renderImageParams(backend);
            }

            const flags = backend.ollama?.runtimeFlags || {};
            const cap = backend.ollama?.backendCapacity || {};
            const contexts = backend.ollama?.modelContexts || [];
            const runningModels = backend.ollama?.runningModels || [];

            // Для llama_cpp показываем упрощённые параметры вместо Ollama-специфичных флагов
            if (isLlamaCpp) {
                // NB: Go-структура `LlamaCppMetrics` маршалится в JSON как `llamaCpp` (camelCase),
                // а не `llama_cpp` (snake_case). Использование неправильного ключа оставляло все
                // параметры llama.cpp бэкенда пустыми на вкладке "Бэкенды" (issue 2026-06-29).
                var cppParams = backend.llamaCpp || {};
                var cppHtml = '<div class="be-detail-section"><div class="be-detail-title">🦒 ' + escapeHtml(_t('renderers.section_llama_cpp_params') || 'llama.cpp Parameters') + '</div><div class="be-params-grid">';
                if (cppParams.modelPath) cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.model_path') || 'Model Path') + '</span><span class="be-param-value">' + escapeHtml(cppParams.modelPath) + '</span></div>';
                if (cppParams.contextLength !== undefined) cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.context')) + '</span><span class="be-param-value">' + escapeHtml(String(cppParams.contextLength)) + '</span></div>';
                if (cppParams.nGpuLayers !== undefined) cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.gpu_layers') || 'GPU Layers') + '</span><span class="be-param-value">' + escapeHtml(String(cppParams.nGpuLayers)) + '</span></div>';
                if (cppParams.nThreads !== undefined) cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.threads') || 'Threads') + '</span><span class="be-param-value">' + escapeHtml(String(cppParams.nThreads)) + '</span></div>';
                if (cppParams.batchSize !== undefined) cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.batch_size') || 'Batch Size') + '</span><span class="be-param-value">' + escapeHtml(String(cppParams.batchSize)) + '</span></div>';
                if (cppParams.quantization) cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.quantization') || 'Quantization') + '</span><span class="be-param-value">' + escapeHtml(cppParams.quantization) + '</span></div>';
                cppHtml += '</div></div>';

                // R346 (2026-10-30): VRAM из поллера cppworker.
                //
                // ЗАЧЕМ. Вопрос «почему не поднялась вторая копия модели» оператор задаёт
                // раньше всех остальных, а ответ (сколько VRAM свободно на узле) уже лежал
                // в метриках — availableVramMb/totalVramMb писал поллер, и их не читал
                // никто: числа были видны только в сыром JSON /api/v1/cluster.
                //
                // Почему берём ИМЕННО эти поля, а не gpu.memoryTotal: у llama.cpp-бэкенда
                // без агентской NVML (типовой bundled-режим) gpu пустой — см. фолбэк по
                // disponible VRAM в internal/api/handlers_backends.go.
                var vramFreeMb = Number(cppParams.availableVramMb || 0);
                var vramTotalMb = Number(cppParams.totalVramMb || 0);
                // RPC-раскладка: свободная VRAM удалённых узлов осмысленна только при
                // включённом RPC — иначе это нули и лишний шум в карточке.
                var vramRemoteFreeMb = Number(cppParams.vramRemoteFreeMb || 0);
                var vramRemoteNodes = Number(cppParams.vramRemoteNodes || 0);
                // R348 (2026-10-31): снимок узлов живёт 30 с, после чего текущий вклад
                // честно обнуляется (R132: протухшее не выдаём за текущую ёмкость). Но
                // «0 МБ узлов» и «7 ГБ донора, измеренные 10 минут назад» в панели
                // выглядели одинаково, и вопрос «влезет ли модель в две карты» оставался
                // без ответа. Показываем историю ОТДЕЛЬНОЙ строкой и всегда с возрастом:
                // она не участвует в решениях о загрузке.
                var vramRemoteLastFreeMb = Number(cppParams.vramRemoteLastKnownFreeMb || 0);
                var vramRemoteLastNodes = Number(cppParams.vramRemoteLastKnownNodes || 0);
                var vramRemoteLastAgeSec = Number(cppParams.vramRemoteLastKnownAgeSec || 0);
                var showRemoteCurrent = cppParams.rpcEnabled && vramRemoteFreeMb > 0;
                var showRemoteLast = cppParams.rpcEnabled && !showRemoteCurrent && vramRemoteLastFreeMb > 0;
                if (vramFreeMb > 0 || vramTotalMb > 0 || showRemoteCurrent || showRemoteLast) {
                    cppHtml += '<div class="be-detail-section"><div class="be-detail-title">' + escapeHtml(_t('renderers.section_vram') || 'VRAM') + '</div><div class="be-params-grid">';
                    if (vramTotalMb > 0) cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.vram_total') || 'Total VRAM') + '</span><span class="be-param-value">' + formatMB(vramTotalMb) + '</span></div>';
                    if (vramFreeMb > 0) cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.free_vram')) + '</span><span class="be-param-value">' + formatMB(vramFreeMb) + '</span></div>';
                    if (showRemoteCurrent) {
                        cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.vram_remote_free') || 'Free VRAM (RPC nodes)') + '</span><span class="be-param-value">' + formatMB(vramRemoteFreeMb) + (vramRemoteNodes > 0 ? ' / ' + vramRemoteNodes + ' ' + escapeHtml(_t('renderers.vram_remote_nodes') || 'nodes') : '') + '</span></div>';
                    } else if (showRemoteLast) {
                        var ageLabel = formatAgeR348(vramRemoteLastAgeSec);
                        cppHtml += '<div class="be-param-item" title="' + escapeHtml(_t('renderers.vram_remote_last_hint') || 'Снимок узлов раскладки обновляется при загрузке/раскладке; текущий вклад считается устаревшим') + '"><span class="be-param-label">' + escapeHtml(_t('renderers.vram_remote_last_known') || 'Free VRAM (RPC nodes, last measured)') + '</span><span class="be-param-value">' + formatMB(vramRemoteLastFreeMb) + (vramRemoteLastNodes > 0 ? ' / ' + vramRemoteLastNodes + ' ' + escapeHtml(_t('renderers.vram_remote_nodes') || 'nodes') : '') + ' <span style="font-size:var(--font-size-xs);color:var(--text-secondary)">' + escapeHtml(_t('renderers.ago') || 'ago') + ' ' + escapeHtml(ageLabel) + '</span></span></div>';
                    }
                    cppHtml += '</div></div>';
                }

                // Добавляем Disk/Network как обычно
                const sys = backend.system || {};
                if (sys.diskTotal !== undefined || sys.diskUsed !== undefined || sys.networkRX !== undefined) {
                    cppHtml += '<div class="be-detail-section"><div class="be-detail-title">' + escapeHtml(_t('renderers.section_disk_network')) + '</div><div class="be-params-grid">';
                    if (sys.diskTotal !== undefined) cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.disk_total')) + '</span><span class="be-param-value">' + formatMB(sys.diskTotal) + '</span></div>';
                    if (sys.diskUsed !== undefined) cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.disk_used')) + '</span><span class="be-param-value">' + formatMB(sys.diskUsed) + '</span></div>';
                    if (sys.diskFree !== undefined) cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.disk_free')) + '</span><span class="be-param-value">' + formatMB(sys.diskFree) + '</span></div>';
                    if (sys.networkRX !== undefined) cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.network_rx')) + '</span><span class="be-param-value">' + formatMB(sys.networkRX) + '</span></div>';
                    if (sys.networkTX !== undefined) cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.network_tx')) + '</span><span class="be-param-value">' + formatMB(sys.networkTX) + '</span></div>';
                    cppHtml += '</div></div>';
                }

                // Показываем loaded models если есть.
                //
                // R346 (2026-10-30): раньше здесь читался backend.ollama.runningModels,
                // а у llama.cpp-бэкенда ollama == nil (все метрики лежат в llamaCpp,
                // см. types.BackendMetrics). Поэтому секция «Loaded Models» в раскрытой
                // строке cpp-бэкенда не появлялась НИКОГДА, хотя список моделей есть в
                // llamaCpp.loadedModels (cluster state) и в backend.loadedModels (/backends).
                //
                // loadedAt показываем рядом с моделью: «когда это загрузилось» — первое,
                // что нужно при «модель вдруг стала медленной» (перезагрузка/вытеснение).
                var cppLoadedModels = (Array.isArray(cppParams.loadedModels) && cppParams.loadedModels.length > 0)
                    ? cppParams.loadedModels
                    : (Array.isArray(backend.loadedModels) ? backend.loadedModels : []);
                if (cppLoadedModels.length > 0) {
                    cppHtml += '<div class="be-detail-section"><div class="be-detail-title">' + escapeHtml(_t('renderers.section_loaded_models')) + '</div><div class="be-params-grid">';
                    cppLoadedModels.forEach(function(m) {
                        var sizeGB = (m.size || 0) / 1024 / 1024 / 1024;
                        var loadedAt = formatLoadedAtR346(m.loadedAt);
                        cppHtml += '<div class="be-param-item" style="grid-column: 1/-1;">' +
                            '<span class="be-param-label">' + escapeHtml(m.name) + '</span>' +
                            '<span class="be-param-value">' + sizeGB.toFixed(1) + ' GB' +
                            (loadedAt ? ' <span title="' + escapeHtml(_t('renderers.loaded_at') || 'Loaded at') + ': ' + escapeHtml(m.loadedAt) + '" style="font-size:var(--font-size-xs);color:var(--text-secondary)">⏱ ' + escapeHtml(loadedAt) + '</span>' : '') +
                            '</span>' +
                        '</div>';
                    });
                    cppHtml += '</div></div>';
                }
                // Подключение клиентов (ключ + эндпоинты) — см. client-access.js.
                if (window.ClientAccess && typeof window.ClientAccess.render === 'function') {
                    cppHtml += window.ClientAccess.render(backend);
                }
                return cppHtml;
            }

            // Section: Runtime Flags (Ollama only)
            let flagsHtml = `<div class="be-detail-section"><div class="be-detail-title">${_t('renderers.section_runtime_flags')}</div><div class="be-params-grid">`;

            const mainFlags = ['numGpuLayers', 'contextLength', 'numParallel', 'numThreads', 'batchSize'];
            mainFlags.forEach(key => {
                if (flags[key] !== undefined && flags[key] !== 0 && flags[key] !== '') {
                    const param = OLLAMA_PARAMS[key];
                    const value = key === 'numGpuLayers' && flags[key] === -1 ? 'auto' : String(flags[key]);
                    flagsHtml += `<div class="be-param-item" title="${escapeHtml(param.desc)}">
                        <span class="be-param-label">${escapeHtml(param.label)}</span>
                        <span class="be-param-value">${escapeHtml(value)}</span>
                    </div>`;
                }
            });

            const extraFlags = ['gpuSplitMode', 'mainGpu', 'kvCacheQuant', 'source'];
            extraFlags.forEach(key => {
                if (flags[key] !== undefined && flags[key] !== '' && String(flags[key]) !== '0') {
                    const param = OLLAMA_PARAMS[key];
                    flagsHtml += `<div class="be-param-item" title="${escapeHtml(param.desc)}">
                        <span class="be-param-label">${escapeHtml(param.label)}</span>
                        <span class="be-param-value">${escapeHtml(String(flags[key]))}</span>
                    </div>`;
                }
            });

            const boolFlags = [
                { key: 'lowVram', label: 'Low VRAM', desc: OLLAMA_PARAMS.lowVram.desc },
                { key: 'f16kv', label: 'FP16 KV Cache', desc: OLLAMA_PARAMS.f16kv.desc },
                { key: 'flashAttention', label: 'Flash Attention', desc: OLLAMA_PARAMS.flashAttention.desc }
            ];
            boolFlags.forEach(({ key, label, desc }) => {
                if (flags[key] !== undefined) {
                    const val = flags[key] ? '✅ ' + _t('common.yes') : '❌ ' + _t('common.no');
                    const cls = flags[key] ? 'be-param-on' : 'be-param-off';
                    flagsHtml += `<div class="be-param-item ${cls}" title="${escapeHtml(desc)}">
                        <span class="be-param-label">${escapeHtml(label)}</span>
                        <span class="be-param-value">${val}</span>
                    </div>`;
                }
            });

            flagsHtml += '</div></div>';

            // Section: Backend Capacity
            let capHtml = '';
            if (cap.freeVram !== undefined || cap.loadedModelVram !== undefined) {
                capHtml = `<div class="be-detail-section"><div class="be-detail-title">${_t('renderers.section_backend_capacity')}</div><div class="be-params-grid">`;
                if (cap.freeVram !== undefined) capHtml += `<div class="be-param-item"><span class="be-param-label">${_t('renderers.free_vram')}</span><span class="be-param-value">${formatMB(cap.freeVram)}</span></div>`;
                if (cap.guaranteedVram !== undefined) capHtml += `<div class="be-param-item"><span class="be-param-label">${_t('renderers.guaranteed_90')}</span><span class="be-param-value">${formatMB(cap.guaranteedVram)}</span></div>`;
                if (cap.loadedModelVram !== undefined) capHtml += `<div class="be-param-item"><span class="be-param-label">${_t('renderers.models_vram')}</span><span class="be-param-value">${formatMB(cap.loadedModelVram)}</span></div>`;
                if (cap.contextOverheadMB !== undefined) capHtml += `<div class="be-param-item"><span class="be-param-label">${_t('renderers.context_overhead')}</span><span class="be-param-value">${formatMB(cap.contextOverheadMB)}</span></div>`;
                if (cap.loadableModelCount !== undefined) capHtml += `<div class="be-param-item"><span class="be-param-label">${_t('renderers.loadable_models_count')}</span><span class="be-param-value">${cap.loadableModelCount}</span></div>`;
                if (cap.mode) capHtml += `<div class="be-param-item"><span class="be-param-label">${_t('renderers.platform_mode')}</span><span class="be-param-value">${escapeHtml(String(cap.mode))}</span></div>`;
                capHtml += '</div></div>';
            }

            // Section: Model Contexts
            let ctxHtml = '';
            if (contexts.length > 0) {
                ctxHtml = `<div class="be-detail-section"><div class="be-detail-title">${_t('renderers.section_model_contexts')}</div>`;
                contexts.forEach(ctx => {
                    ctxHtml += `<div class="be-model-ctx">
                        <div class="be-model-ctx-name">${escapeHtml(ctx.name)}</div>
                        <div class="be-params-grid" style="margin-bottom:0;">
                            <div class="be-param-item"><span class="be-param-label">${_t('renderers.context')}</span><span class="be-param-value">${formatNumber(ctx.contextLength)}</span></div>
                            <div class="be-param-item"><span class="be-param-label">${_t('renderers.effective')}</span><span class="be-param-value">${formatNumber(ctx.effectiveContext)}</span></div>
                            <div class="be-param-item"><span class="be-param-label">${_t('common.total')} (${_t('renderers.model_memory')})</span><span class="be-param-value">${formatMB(ctx.totalMemoryMB)}</span></div>
                            <div class="be-param-item"><span class="be-param-label">${_t('renderers.kv_cache')}</span><span class="be-param-value">${formatMB(ctx.kvCacheMemoryMB)}</span></div>
                            <div class="be-param-item"><span class="be-param-label">${_t('renderers.layers')}</span><span class="be-param-value">${ctx.numLayers || '-'}</span></div>
                            <div class="be-param-item"><span class="be-param-label">${_t('renderers.kv_precision')}</span><span class="be-param-value">${ctx.precisionBits || 16}-bit</span></div>
                        </div>
                    </div>`;
                });
                ctxHtml += '</div>';
            }

            // Section: Loaded Models
            let modelsHtml = '';
            if (runningModels.length > 0) {
                modelsHtml = `<div class="be-detail-section"><div class="be-detail-title">${_t('renderers.section_loaded_models')}</div><div class="be-params-grid">`;
                runningModels.forEach(m => {
                    const sizeGB = (m.size || 0) / 1024 / 1024 / 1024;
                    const ramGB = (m.ramUsage || 0) / 1024 / 1024 / 1024;
                    var digestShort = (m.digest || m.Digest || '').substring(0, 12);
                    var expDate = isZeroTimestamp(m.expiresAt || m.ExpiresAt) ? '' : (m.expiresAt || m.ExpiresAt || '');
                    modelsHtml += `<div class="be-param-item" style="grid-column: 1/-1;">
                        <span class="be-param-label">${escapeHtml(m.name)}</span>
                        <span class="be-param-value">${sizeGB.toFixed(1)} GB | ${escapeHtml(m.family || '-')} | ${escapeHtml(m.parameterSize || '-')} | ${escapeHtml(m.quantization || '-')}` +
                        (digestShort ? ` <code title="${_t('renderers.digest')}: ${escapeHtml(digestShort)}">${escapeHtml(digestShort)}</code>` : '') +
                        (expDate ? ` <span title="${_t('renderers.expires')}: ${escapeHtml(expDate)}" style="font-size:var(--font-size-xs);color:var(--warning)">⌛${escapeHtml(expDate.substring(0, 10))}</span>` : '') +
                        (ramGB > 0.5 ? ` <span style="font-size:var(--font-size-xs);color:var(--text-secondary)">RAM: ${ramGB.toFixed(2)} GB</span>` : '') +
                    `</div>`;
                });
                modelsHtml += '</div></div>';
            }


            // Section: Disk / Network
            const sys = backend.system || {};
            let diskNetHtml = '';
            if (sys.diskTotal !== undefined || sys.diskUsed !== undefined || sys.diskFree !== undefined || sys.networkRX !== undefined || sys.networkTX !== undefined) {
                diskNetHtml = `<div class="be-detail-section"><div class="be-detail-title">${_t('renderers.section_disk_network')}</div><div class="be-params-grid">`;
                if (sys.diskTotal !== undefined || sys.diskUsed !== undefined || sys.diskFree !== undefined) {
                    if (sys.diskTotal !== undefined) diskNetHtml += `<div class="be-param-item"><span class="be-param-label">${_t('renderers.disk_total')}</span><span class="be-param-value">${formatMB(sys.diskTotal)}</span></div>`;
                    if (sys.diskUsed !== undefined) diskNetHtml += `<div class="be-param-item"><span class="be-param-label">${_t('renderers.disk_used')}</span><span class="be-param-value">${formatMB(sys.diskUsed)}</span></div>`;
                    if (sys.diskFree !== undefined) diskNetHtml += `<div class="be-param-item"><span class="be-param-label">${_t('renderers.disk_free')}</span><span class="be-param-value">${formatMB(sys.diskFree)}</span></div>`;
                }
                if (sys.networkRX !== undefined) diskNetHtml += `<div class="be-param-item"><span class="be-param-label">${_t('renderers.network_rx')}</span><span class="be-param-value">${formatMB(sys.networkRX)}</span></div>`;
                if (sys.networkTX !== undefined) diskNetHtml += `<div class="be-param-item"><span class="be-param-label">${_t('renderers.network_tx')}</span><span class="be-param-value">${formatMB(sys.networkTX)}</span></div>`;
                diskNetHtml += '</div></div>';
            }

            var accessHtml = (window.ClientAccess && typeof window.ClientAccess.render === 'function')
                ? window.ClientAccess.render(backend)
                : '';
            return flagsHtml + capHtml + ctxHtml + modelsHtml + diskNetHtml + accessHtml;
        }
        return {
            renderImageParams: renderImageParams,
            formatAgeR348: formatAgeR348,
            formatLoadedAtR346: formatLoadedAtR346,
            renderOllamaParams: renderOllamaParams
        };
    };
})();