/**
 * gguf-renderer-detail-render.js — Right panel render functions for gguf-renderer.
 *
 * R57.5f (2026-09-03): extracted from webui/js/modules/gguf-renderer.js.
 *
 * Содержит рендеринг ПРАВОЙ ПАНЕЛИ (master-detail layout):
 *   - renderDetailPanel — главный dispatcher
 *   - renderEmptyDetail — empty state
 *   - renderDetailHeader — header с status + URL
 *   - renderDetailTabs — табы (about / models / downloads / hf / settings)
 *   - renderDetailPane — dispatcher по detailPane state
 *   - renderAboutPane — таб About
 *   - renderModelsPane — таб Models
 *   - renderLoadedPane — loaded models cards
 *   - renderDownloadsPane — downloads progress
 *   - renderHuggingFacePane — HF search results
 *   - renderSettingsPane — settings tab
 *   - Backend options load/save (loadAndRenderBackendOptions, saveBackendOptions)
 *   - mountProfilesInSettings — Per-Model Profiles mounting
 *
 * Зависимости (должны быть загружены раньше):
 *   - window.GgufModule.state (from gguf-renderer-state.js)
 *   - window.GgufModule.* (all helpers / actions / refresh / list / detail functions)
 *   - window.Renderers (statusBadges, etc.)
 *   - window.Utils (escapeHtml)
 *   - window.I18N.t
 *
 * Загружается ДО gguf-renderer.js (в defer-цепочке).
 */
(function() {
    'use strict';

    const M = (window.GgufModule = window.GgufModule || {});

    // Local aliases для коротких ссылок внутри IIFE
    const state = M.state;

    // R118 (2026-10-14): ПРОВЕРЕННЫЕ варианты настроек загрузки.
    //
    // Требование оператора: «не отображать в настройках те режимы, что не
    // проверены». Проверено живьём на стенде (мастер RTX 3070 + донор RTX 5060
    // через RPC): backend раскладки — CUDA; split mode — LAYER (1), потому что
    // раскладка слоёв между машинами работает именно в нём. Vulkan/Kompute и
    // ROW/NONE/TENSOR на стенде не проверялись, поэтому в форме их нет —
    // выбирать непроверенный режим «вслепую» оператор не должен.
    const GGUF_VERIFIED_RPC_BACKENDS = [
        { value: '', label: '— default —' },
        { value: 'cuda', label: 'CUDA' }
    ];
    const GGUF_VERIFIED_SPLIT_MODES = [
        { value: -1, label: '-1 — default' },
        { value: 1, label: '1 — LAYER (раскладка слоёв)' }
    ];
    const _ = M._;
    const stripGGUF = M.stripGGUF;
    const formatFileSize = M.formatFileSize;
    // formatVramMB — метрики GPU приходят в МБ, formatFileSize ждёт байты
    // (см. gguf-renderer-helpers.js: M.formatVramMB).
    const formatVramMB = M.formatVramMB;
    const showToast = M.showToast;

    /**
     * R66d (2026-09-23): найти runtime-параметры модели в карте
     * state.runtimeModels.
     *
     * Карта приходит из cppworker (/api/v1/cppworker/config/runtime) с ключом
     * m.name — обычно БЕЗ расширения .gguf. А список загруженных моделей в
     * балансере может содержать и имя файла, и полный путь. Перебираем варианты:
     * точное имя, basename пути, имя без .gguf, basename без .gguf.
     *
     * @param {Object} map — state.runtimeModels
     * @param {string} name — имя из loadedModels
     * @param {string} [path] — путь модели, если есть
     * @returns {Object|null}
     */
    function lookupRuntimeModel(map, name, path) {
        if (!map || typeof map !== 'object') return null;
        var base = path ? String(path).split(/[\\/]/).pop() : '';
        var candidates = [name, base];
        if (typeof stripGGUF === 'function') {
            candidates.push(stripGGUF(name || ''));
            if (base) candidates.push(stripGGUF(base));
        }
        for (var i = 0; i < candidates.length; i++) {
            var key = candidates[i];
            if (key && map[key]) return map[key];
        }
        // Последний шанс: сравнение без учёта регистра и расширения.
        var wanted = (typeof stripGGUF === 'function' ? stripGGUF(name || '') : String(name || '')).toLowerCase();
        if (!wanted) return null;
        var keys = Object.keys(map);
        for (var j = 0; j < keys.length; j++) {
            var norm = (typeof stripGGUF === 'function' ? stripGGUF(keys[j]) : keys[j]).toLowerCase();
            if (norm === wanted) return map[keys[j]];
        }
        return null;
    }

(function() {
    'use strict';
    // ---- Right panel: detail ----

    function renderDetailPanel() {
        if (!state.selectedBackendId) {
            return renderEmptyDetail();
        }
        const backend = M.currentBackend();
        if (!backend) {
            return renderEmptyDetail();
        }
        return '' +
            renderDetailHeader(backend) +
            renderDetailTabs() +
            // R59.9 (2026-09-03): added id="ggufDetailContent" so
            // M.refreshDetailPane() can find the target via
            // getElementById('ggufDetailContent'). Before this, the
            // class-only div was unreachable by id, so tab clicks
            // updated state.detailPane but the panel content never
            // re-rendered. Symptom: clicking tabs did nothing visually.
            '<div id="ggufDetailContent" class="gguf-detail-content">' +
                renderDetailPane() +
            '</div>';
    }

    function renderEmptyDetail() {
        return '' +
            '<div class="gguf-detail-header">' +
                '<h2 class="gguf-detail-title"><i class="fas fa-cube"></i> ' + _('gguf.master_title') + '</h2>' +
            '</div>' +
            '<div class="gguf-detail-content">' +
                '<div class="gguf-empty-detail">' +
                    '<div class="empty-icon"><i class="fas fa-arrow-left"></i></div>' +
                    '<h3>' + _('gguf.no_backend_selected') + '</h3>' +
                    '<p>' + _('gguf.select_backend_prompt') + '</p>' +
                '</div>' +
            '</div>';
    }

    function renderDetailHeader(backend) {
        const statusClass = backend.status === 'healthy' ? 'connected' :
            (backend.status === 'offline' ? 'disconnected' : 'warn');
        const statusLabel = backend.status === 'healthy' ? _('gguf.backend_healthy') :
            (backend.status === 'offline' ? _('gguf.backend_unreachable') : (backend.status || '-'));
        const url = GgufApi.buildBackendWorkerUrl(backend);
        return '' +
            '<div class="gguf-detail-header">' +
                '<div class="gguf-detail-header-info">' +
                    '<h2 class="gguf-detail-title">' +
                        '<span class="gguf-status-dot ' + statusClass + '"></span>' +
                        Utils.escapeHtml(backend.id) +
                    '</h2>' +
                    '<div class="gguf-url-block" title="' + Utils.escapeHtml(url) + '">' +
                        '<i class="fas fa-link"></i>' +
                        '<span class="url-text">' + Utils.escapeHtml(url) + '</span>' +
                        '<button class="gguf-link-button" id="ggufCopyUrl" title="' + _('gguf.copy_url') + '"><i class="fas fa-copy"></i></button>' +
                    '</div>' +
                    renderDuplicateHostWarning(backend) +
                '</div>' +
                '<div class="gguf-detail-actions">' +
                    '<button class="btn btn-sm btn-secondary" id="ggufRefreshDetail"><i class="fas fa-sync"></i> ' + _('gguf.refresh_backend') + '</button>' +
                '</div>' +
            '</div>';
    }

    /**
     * renderDuplicateHostWarning — R-MultiHost (2026-10-07).
     *
     * Показывает предупреждение, если ДРУГОЙ бэкенд зарегистрирован с тем же
     * host. Имя хоста — не идентичность: на двух машинах с одинаковым compose
     * контейнеры называются одинаково, и тогда URL этого бэкенда указывает на
     * чужую машину. Симптом — «модели на диске есть, а на самом деле их нет»:
     * список читается у соседа.
     *
     * Ничего не блокируем — только говорим оператору, что видит балансер.
     */
    function renderDuplicateHostWarning(backend) {
        var list = (state && state.registeredBackends) || [];
        var twins = list.filter(function (b) {
            return b && b.id !== backend.id && b.host && b.host === backend.host;
        });
        if (!twins.length) return '';
        var names = twins.map(function (b) { return b.id; }).join(', ');
        var text = (typeof _ === 'function' && _('gguf.duplicate_host_warning')) ||
            'Another backend is registered with the SAME host — requests for one of them will go to the wrong machine. Set BACKEND_HOST on the other machine and recreate its worker.';
        return '<div class="gguf-url-warning" title="' + Utils.escapeHtml(text + ' [' + names + ']') + '">' +
            '<i class="fas fa-triangle-exclamation"></i>' +
            '<span>' + Utils.escapeHtml(text) + '</span>' +
            '<span class="gguf-url-warning-list">' + Utils.escapeHtml(names) + '</span>' +
        '</div>';
    }

    function renderDetailTabs() {
        const tabs = [
            { id: 'about',     icon: 'fas fa-info-circle',    label: _('gguf.tab_about') },
            { id: 'models',    icon: 'fas fa-cube',           label: _('gguf.tab_models') },
            { id: 'loaded',    icon: 'fas fa-brain',          label: _('gguf.tab_loaded') },
            { id: 'downloads', icon: 'fas fa-download',       label: _('gguf.tab_downloads') },
            { id: 'hf',        icon: 'fab fa-huggingface',    label: _('gguf.tab_hf') },
            { id: 'settings',  icon: 'fas fa-cog',            label: _('gguf.tab_settings') }
        ];
        return '<div class="gguf-detail-tabs">' +
            tabs.map(function (t) {
                return '<button class="gguf-detail-tab ' + (state.detailPane === t.id ? 'active' : '') + '" data-detail-tab="' + t.id + '">' +
                    '<i class="' + t.icon + '"></i> ' + t.label +
                '</button>';
            }).join('') +
        '</div>';
    }

    function renderDetailPane() {
        if (state.detailLoading) {
            return '<div class="gguf-empty-state"><i class="fas fa-spinner fa-spin"></i> ' + _('common.loading') + '</div>';
        }
        if (state.detailError) {
            // R91 (2026-10-08): ошибка должна быть ЧИТАЕМОЙ и с выходом.
            // Раньше текст ошибки вставлялся как HTML (сообщение сервера с
            // угловыми скобками ломало разметку), а кнопки «повторить» не было —
            // оператор видел пустые вкладки и не знал, что произошло.
            return '<div class="gguf-empty-state" style="padding:12px;">' +
                '<div style="display:flex;align-items:center;gap:8px;margin-bottom:8px;">' +
                    '<i class="fas fa-exclamation-triangle" style="color:var(--danger);"></i>' +
                    '<strong>' + Utils.escapeHtml(_('gguf.detail_load_failed')) + '</strong>' +
                '</div>' +
                '<div style="font-family:monospace;font-size:11px;background:rgba(0,0,0,0.2);' +
                    'padding:8px;border-radius:4px;margin-bottom:8px;word-break:break-all;">' +
                    Utils.escapeHtml(state.detailError) +
                '</div>' +
                '<button class="btn btn-secondary btn-sm" id="ggufDetailRetry">' +
                    '<i class="fas fa-sync"></i> ' + Utils.escapeHtml(_('gguf.detail_load_retry')) +
                '</button>' +
            '</div>';
        }
        switch (state.detailPane) {
            case 'about':     return renderAboutPane();
            case 'models':    return renderModelsPane();
            case 'loaded':    return renderLoadedPane();
            case 'downloads': return renderDownloadsPane();
            case 'hf':        return renderHuggingFacePane();
            case 'settings':  return renderSettingsPane();
            default:          return renderAboutPane();
        }
    }

    function renderAboutPane() {
        const backend = M.currentBackend();
        if (!backend) return '';
        const gpus = renderGpuInfoCards();
        const host = renderHostSystemCard();
        const worker = renderWorkerInfoCard();
        // 2026-10-03: блок «Подключение клиентов» (ключ + эндпоинты) прямо в
        // «О бэкенде» — оператор настраивает клиента, глядя на этот бэкенд, и
        // раньше уходил искать токен в deployments/.env на хосте. Модуль общий с
        // страницей «Бэкенды» (webui/js/modules/client-access.js).
        const accessHtml = (window.ClientAccess && typeof window.ClientAccess.render === 'function')
            ? window.ClientAccess.render(backend)
            : '';
        return '<div class="gguf-about-grid">' +
            '<div>' +
                '<div class="gguf-about-section">' +
                    '<h4><i class="fas fa-microchip"></i> ' + _('gguf.gpu_info') + '</h4>' +
                    gpus +
                '</div>' +
                // R91: хост-метрики (RAM/CPU/диск). Раньше на странице
                // llama.cpp-бэкенда были только GPU и версия воркера, хотя
                // /api/v1/backends/{id} отдаёт system.* и refreshDetail уже
                // складывает его в state.backendRuntime — данные приезжали, но
                // нигде не рисовались.
                '<div class="gguf-about-section" style="margin-top:12px;">' +
                    '<h4><i class="fas fa-memory"></i> ' + _('gguf.host_info') + '</h4>' +
                    host +
                '</div>' +
            '</div>' +
            '<div>' +
                '<div class="gguf-about-section">' +
                    '<h4><i class="fas fa-server"></i> ' + _('gguf.worker_info') + '</h4>' +
                    worker +
                '</div>' +
                '<div class="gguf-about-section" style="margin-top:12px;">' +
                    '<h4><i class="fas fa-cube"></i> ' + _('gguf.models_on_disk') + '</h4>' +
                    '<div class="gguf-stat-value">' +
                        (state.localModels ? state.localModels.length : 0) +
                        ' <span class="gguf-stat-value muted">' + _('gguf.about_models_on_disk') + '</span>' +
                    '</div>' +
                '</div>' +
            '</div>' +
        '</div>' + accessHtml;
    }

    function renderGpuInfoCards() {
        if (state.gpuInfo) {
            const devices = state.gpuInfo.devices || (Array.isArray(state.gpuInfo) ? state.gpuInfo : [state.gpuInfo]);
            if (devices.length > 0) {
                return '<div class="gguf-stats-grid">' +
                    devices.map(function (gpu, idx) {
                        // В BackendMetrics.GPU нет имени карты (GPUMetrics:
                        // UsagePercent/Memory*/Temperature/Power/Clock/UUIDs), поэтому
                        // раньше в подписи всегда стоял прочерк. Показываем индекс —
                        // он же используется в других местах UI.
                        const name = gpu.name || gpu.brand || gpu.model || ('GPU ' + idx);
                        const memTotal = gpu.memoryTotal || gpu.totalMemory || 0;
                        const memFree = gpu.memoryFree || gpu.freeMemory || 0;
                        const util = gpu.utilization || gpu.usagePercent || 0;
                        // formatVramMB, а НЕ formatFileSize: метрики приходят в МБ
                        // (см. gguf-renderer-helpers.js). Раньше 8192 MB рисовались
                        // как «8.0 KB».
                        return '<div class="gguf-stat-card">' +
                            '<div class="gguf-stat-label">' + Utils.escapeHtml(name) + '</div>' +
                            // Подпись «Всего VRAM» обязательна: без неё первое число
                            // в карточке висело без пояснения (оператор видел
                            // «8.0 KB» и не понимал, что это вообще).
                            '<div class="gguf-stat-label" style="margin-top:6px;">' + _('gguf.about_vram') + '</div>' +
                            '<div class="gguf-stat-value">' + formatVramMB(memTotal) + '</div>' +
                            '<div class="gguf-stat-label" style="margin-top:6px;">' + _('common.free') + '</div>' +
                            '<div class="gguf-stat-value muted">' + formatVramMB(memFree) + '</div>' +
                            '<div class="gguf-stat-label" style="margin-top:6px;">' + _('metrics.gpu_util') + '</div>' +
                            '<div class="gguf-stat-value muted">' + Math.round(util) + '%</div>' +
                        '</div>';
                    }).join('') +
                '</div>';
            }
        }
        return '<div class="gguf-stat-value muted">' + _('gguf.no_gpu') + '</div>';
    }

    /**
     * renderHostSystemCard — сводка по ХОСТУ бэкенда: RAM, CPU, диск (R91).
     *
     * Данные берём из state.backendRuntime.system — это JSON-поле `system`
     * ответа GET /api/v1/backends/{id} (types.SystemMetrics, значения в МБ),
     * которое refreshDetail уже складывает в state. Отдельных запросов нет.
     * Разбор чисел — в hostSystemStats (gguf-renderer-helpers.js), чтобы это
     * можно было проверить тестом без DOM. Форматирование — formatVramMB:
     * память в МБ, а formatFileSize ждёт байты.
     */
    function renderHostSystemCard() {
        const sys = M.hostSystemStats(hostSystemPayload());
        if (!sys.known) {
            // Прочерк с объяснением причины, а не пустая карточка: «нет данных»
            // и «агент не отдаёт system.memory*» — разные вещи для оператора.
            return '<div class="gguf-stat-value muted">' + _('renderers.ram_unknown') + '</div>';
        }
        const ramText = formatVramMB(sys.ramUsedMb) + ' / ' + formatVramMB(sys.ramTotalMb) +
            (sys.ramPercent !== null ? ' (' + sys.ramPercent.toFixed(1) + '%)' : '');
        let html = '<div class="gguf-stats-grid">' +
            '<div class="gguf-stat-card">' +
                '<div class="gguf-stat-label">' + _('metrics.ram_usage') + '</div>' +
                '<div class="gguf-stat-value">' + ramText + '</div>' +
                '<div class="gguf-stat-label" style="margin-top:6px;">' + _('common.free') + '</div>' +
                '<div class="gguf-stat-value muted">' + formatVramMB(sys.ramFreeMb) + '</div>' +
            '</div>';
        if (sys.cpuPercent !== null || sys.cpuTempC !== null) {
            html += '<div class="gguf-stat-card">' +
                '<div class="gguf-stat-label">' + _('metrics.cpu_usage') + '</div>' +
                '<div class="gguf-stat-value">' +
                    (sys.cpuPercent !== null ? sys.cpuPercent.toFixed(1) + '%' : '-') +
                '</div>' +
                '<div class="gguf-stat-label" style="margin-top:6px;">' + _('metrics.cpu_temp') + '</div>' +
                '<div class="gguf-stat-value muted">' +
                    (sys.cpuTempC !== null ? sys.cpuTempC + '°C' : '-') +
                '</div>' +
            '</div>';
        }
        if (sys.diskTotalMb !== null || sys.diskFreeMb !== null) {
            html += '<div class="gguf-stat-card">' +
                '<div class="gguf-stat-label">' + _('gguf.disk_usage') + '</div>' +
                '<div class="gguf-stat-value">' + formatVramMB(sys.diskTotalMb) + '</div>' +
                '<div class="gguf-stat-label" style="margin-top:6px;">' + _('common.free') + '</div>' +
                '<div class="gguf-stat-value muted">' + formatVramMB(sys.diskFreeMb) + '</div>' +
            '</div>';
        }
        return html + '</div>';
    }

    /**
     * hostSystemPayload — где искать system-метрики выбранного бэкенда.
     * Основной источник — state.backendRuntime.system (полный BackendMetrics);
     * запасной — сам объект бэкенда из /api/v1/gguf/backends (там тоже бывает
     * system, если запись пришла из /api/v1/backends).
     */
    function hostSystemPayload() {
        const rt = state.backendRuntime || {};
        if (rt.system) return rt.system;
        const backend = M.currentBackend();
        return (backend && backend.system) || null;
    }

    function renderWorkerInfoCard() {
        if (state.workerInfo) {
            const version = state.workerInfo.version || state.workerInfo.llamaVersion || '-';
            const gpuCount = state.gpuInfo ? (state.gpuInfo.count || (Array.isArray(state.gpuInfo) ? state.gpuInfo.length : 1)) : 0;
            const loadedCount = state.loadedModels ? state.loadedModels.length : 0;
            return '<div class="gguf-stats-grid">' +
                '<div class="gguf-stat-card">' +
                    '<div class="gguf-stat-label">' + _('gguf.version') + '</div>' +
                    '<div class="gguf-stat-value">' + Utils.escapeHtml(version) + '</div>' +
                '</div>' +
                '<div class="gguf-stat-card">' +
                    '<div class="gguf-stat-label">' + _('gguf.gpu_count') + '</div>' +
                    '<div class="gguf-stat-value">' + gpuCount + '</div>' +
                '</div>' +
                '<div class="gguf-stat-card">' +
                    '<div class="gguf-stat-label">' + _('gguf.loaded_models_header') + '</div>' +
                    '<div class="gguf-stat-value">' + loadedCount + '</div>' +
                '</div>' +
            '</div>';
        }
        return '<div class="gguf-stat-value muted">' + _('common.disconnected') + '</div>';
    }

    // renderInventoryNote — R93 (2026-10-09): свежесть проверки каталога моделей и
    // причина пустоты. Оператор должен видеть не только «моделей нет», но и КОГДА
    // проверяли и не отвалился ли каталог: без этого пустой список выглядит как
    // «страница сломалась», а исчезнувшая модель — как «ничего не произошло».
    function renderInventoryNote(inv) {
        if (!inv) return '';
        const parts = [];
        if (inv.dirError) {
            parts.push('<span style="color:var(--danger);font-weight:600;">' +
                Utils.escapeHtml(_('gguf.inventory_dir_error', { error: inv.dirError })) + '</span>');
        } else if (inv.error) {
            parts.push('<span style="color:var(--warning);">' +
                Utils.escapeHtml(_('gguf.inventory_check_failed', { error: inv.error })) + '</span>');
        } else {
            const ageSec = inv.fetchedAt ? Math.round((Date.now() - inv.fetchedAt) / 1000) : 0;
            parts.push(Utils.escapeHtml(_('gguf.inventory_checked', { sec: ageSec })) +
                ' · <span style="color:var(--text-secondary)">' + inv.count + '</span>');
            if ((inv.added && inv.added.length) || (inv.removed && inv.removed.length)) {
                parts.push('<span style="color:var(--warning);">' + Utils.escapeHtml(_('gguf.inventory_changed', {
                    added: (inv.added || []).join(', ') || '—',
                    removed: (inv.removed || []).join(', ') || '—'
                })) + '</span>');
            }
        }
        return '<div class="gguf-inventory-note" style="display:flex;gap:10px;align-items:center;flex-wrap:wrap;font-size:12px;margin:0 0 10px 0;">' +
            parts.join(' <span style="color:var(--border)">|</span> ') +
            '<button class="btn btn-secondary btn-sm" id="ggufRescanModels" title="' +
                Utils.escapeHtml(_('gguf.inventory_rescan_hint')) + '">' +
                '<i class="fas fa-sync"></i> ' + Utils.escapeHtml(_('gguf.inventory_rescan')) + '</button>' +
            '</div>';
    }

    // mergeModelRows — ОДИН источник: gguf-renderer-refresh.js (R165, 2026-10-17).
    // Здесь была ВТОРАЯ копия; она перекрывала первую при загрузке, и R156
    // приходилось править в двух местах. Экспорт не нужен: M.mergeModelRows
    // выставляет модуль refresh (загружается раньше).

    // loadedHandleFor — имя, по которому ЗАГРУЖЕННЫЙ инстанс этой модели известен
    // воркеру (R137, 2026-10-16).
    //
    // ЗАЧЕМ. Кнопка «Выгрузить» в списке моделей на диске отправляла в API имя
    // ФАЙЛА (`Qwen3.8-27B-UD-Q4_K_M.gguf`), а реестр воркера знает инстанс без
    // расширения — выгрузка отвечала 404 «model not currently loaded». Тот же
    // класс, что и профиль модели (R136).
    function loadedHandleFor(diskName) {
        // stripGGUF приходит из соседнего модуля (gguf-renderer-helpers.js);
        // если его нет (тест/ранняя загрузка) — снимаем расширение сами.
        const norm = function (v) {
            return (typeof stripGGUF === 'function')
                ? stripGGUF(String(v || ''))
                : String(v || '').replace(/\.gguf$/i, '');
        };
        const want = norm(diskName);
        const hit = (state.loadedModels || []).find(function (lm) {
            const nm = lm && (lm.model || lm.name || lm.handle);
            return nm && norm(nm) === want;
        });
        return (hit && (hit.handle || hit.model || hit.name)) || diskName;
    }


    function renderModelsPane() {
        const models = state.localModels || [];
        const inv = (state.diskInventory && state.selectedBackendId)
            ? state.diskInventory[state.selectedBackendId] : null;
        const note = renderInventoryNote(inv);
        if (models.length === 0) {
            // R93: пустой список обязан объяснять причину. «Каталог недоступен»
            // (удалённый bind-mount, отвалившийся том) — это не «моделей нет».
            const hint = (inv && inv.dirError)
                ? '<p style="font-size:12px;margin-top:8px;color:var(--danger);">' +
                    Utils.escapeHtml(_('gguf.inventory_dir_error', { error: inv.dirError })) + '</p>'
                : '<p style="font-size:12px;margin-top:8px;">' + _('gguf.no_models_on_disk_hint') + '</p>';
            return '<div class="gguf-empty-state">' +
                '<div class="empty-icon"><i class="fas fa-folder-open"></i></div>' +
                '<div>' + _('gguf.no_local_models') + '</div>' +
                hint +
            '</div>' + note;
        }
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
                            ? '<span class="gguf-local-unloading" style="display:flex;align-items:center;gap:6px;font-size:12px;color:var(--warning, #e0a030);">' +
                                '<i class="fas fa-spinner fa-spin"></i>' +
                                Utils.escapeHtml(_('gguf.unloading', 'Выгружается…')) + ' ' +
                                formatElapsedShort(unloadingElapsedMs(unloadingItemFor(name))) +
                              '</span>'
                            : '<button class="btn btn-sm btn-danger gguf-unload-btn" data-admin-only data-handle="' + Utils.escapeHtml(loadedHandleFor(name)) + '">' +
                                '<i class="fas fa-stop"></i> ' + _('gguf.unload_model') +
                              '</button>')
                        : '<button class="btn btn-sm btn-primary gguf-load-btn" data-admin-only data-idx="' + idx + '">' +
                            '<i class="fas fa-play"></i> ' + _('gguf.load_model') +
                          '</button>') +
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
            'flex-wrap:wrap;font-family:monospace;font-size:10px;color:var(--text-secondary);">' +
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

    /**
     * formatElapsedShort — «12s» / «1m 05s» для карточек загрузки/выгрузки.
     *
     * Общий хелпер вместо копии в каждой карточке: формат обязан совпадать,
     * иначе таймер выгрузки выглядел бы иначе, чем таймер загрузки рядом.
     */
    function formatElapsedShort(ms) {
        const elapsedSec = Math.max(0, Math.floor((Number(ms) || 0) / 1000));
        if (elapsedSec < 60) return elapsedSec + 's';
        return Math.floor(elapsedSec / 60) + 'm ' + (elapsedSec % 60) + 's';
    }

    /** Прошло миллисекунд с момента начала выгрузки (для таймера). */
    function unloadingElapsedMs(item) {
        const startedAt = item && item.startedAt ? Number(item.startedAt) : Date.now();
        return Math.max(0, Date.now() - startedAt);
    }

    /** Запись о выгрузке для модели (по handle или имени), либо null. */
    function unloadingItemFor(handle) {
        const want = String(handle || '');
        if (!want || !state.selectedBackendId) return null;
        const list = (state.unloadingModels && state.unloadingModels[state.selectedBackendId]) || [];
        for (var i = 0; i < list.length; i++) {
            const item = list[i] || {};
            if (String(item.handle || '') === want || String(item.name || '') === want) return item;
        }
        return null;
    }

    /** Идёт ли выгрузка этой модели (для панели «Модели на диске»). */
    function isUnloadingHandle(handle) {
        return !!unloadingItemFor(handle);
    }

    /**
     * renderUnloadingCard — карточка «модель выгружается».
     *
     * Рисуется из state.unloadingModels (UI-локальное состояние, см.
     * gguf-renderer-actions.js): cppworker не отдаёт признак выгрузки, а на
     * больших моделях освобождение VRAM/mmap занимает десятки секунд — без
     * этой карточки оператор видел либо «модели нет», либо «модель есть» без
     * единого признака, что что-то происходит.
     */
    function renderUnloadingCard(item, opts) {
        opts = opts || {};
        const name = (item && (item.name || item.handle)) || '-';
        const elapsed = formatElapsedShort(unloadingElapsedMs(item));
        // Для модели, которая ещё числится загруженной (opts.inline), карточку
        // не дублируем: там состояние выгрузки показывается вместо кнопки.
        return '<div class="gguf-loaded-model-item gguf-loaded-model-unloading" style="border-left:3px solid var(--warning, #e0a030);">' +
            '<div class="gguf-loaded-model-name">' +
                '<i class="fas fa-spinner fa-spin" style="color:var(--warning, #e0a030);margin-right:6px;"></i>' +
                Utils.escapeHtml(name) +
            '</div>' +
            '<div class="gguf-loaded-model-info">' +
                '<span style="color:var(--warning, #e0a030);">' + Utils.escapeHtml(_('gguf.unloading', 'Выгружается…')) + '</span>' +
                '<span style="margin-left:12px;">' + Utils.escapeHtml(_('gguf.elapsed', 'Прошло')) + ': ' + elapsed + '</span>' +
            '</div>' +
            '<div class="gguf-loaded-model-info" style="font-size:11px;color:var(--text-muted);">' +
                Utils.escapeHtml(_('gguf.unloading_hint', 'Освобождаю память (VRAM). На больших моделях это занимает десятки секунд.')) +
            '</div>' +
        '</div>';
    }

    // donorContributionHtml — R229 (2026-10-22): КАРТОЧКА ВКЛАДА ДОНОРОВ.
    //
    // ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «видно, что модель разложена на донора, но не видно,
    // ЧТО именно он даёт» — сколько слоёв держит, какая это модель и сколько памяти
    // занимает. Источник — ТОЧНАЯ раскладка device_layers_exact от воркера (строки
    // `load_tensors: layer N assigned to device RPC0`): у RPC-диапазонов есть node
    // (адрес узла). Память — ОЦЕНКА по размеру модели и доле слоёв: фактическую
    // память узла воркер по-узлово не отдаёт, поэтому подписываем «оценка», а не
    // выдаём догадку за факт.
    function donorContributionHtml(m) {
        m = m || {};
        var ranges = m.device_layers_exact || m.deviceLayersExact || null;
        if (!ranges || !ranges.length) return '';
        var totalLayers = Number(m.n_layers != null ? m.n_layers : m.nLayers) || 0;
        var sizeBytes = Number(m.size_bytes != null ? m.size_bytes : (m.sizeBytes != null ? m.sizeBytes : m.size)) || 0;
        var modelName = String(m.name || m.model || '');
        var rows = [];
        for (var i = 0; i < ranges.length; i++) {
            var r = ranges[i] || {};
            var dev = String(r.device || '');
            var node = String(r.node || '');
            // Донор — это RPC-устройство; локальные CUDA0/CPU сюда не относятся.
            if (!/^RPC/i.test(dev) && !node) continue;
            var count = Number(r.count) || 0;
            if (count <= 0) continue;
            var mb = (sizeBytes > 0 && totalLayers > 0)
                ? Math.round(sizeBytes * (count / totalLayers) / (1024 * 1024))
                : null;
            rows.push('<tr><td>' + Utils.escapeHtml(node || dev) + '</td>' +
                '<td>' + Utils.escapeHtml(String(count)) +
                    (totalLayers > 0 ? ' ' + Utils.escapeHtml(_('gguf.donor_of', 'из')) + ' ' + totalLayers : '') +
                '</td>' +
                '<td>' + (mb === null ? '—' : '~' + mb + ' ' + Utils.escapeHtml(_('gguf.mb', 'МБ'))) + '</td></tr>');
        }
        if (!rows.length) return '';
        return '<div class="gguf-donor-card" style="margin-top:8px;padding:8px;border:1px solid var(--border);border-radius:6px;">' +
            '<div style="font-size:12px;font-weight:600;">' + Utils.escapeHtml(_('gguf.donor_title', 'Вклад доноров')) +
                (modelName ? '<span style="font-weight:400;color:var(--text-secondary);"> — ' +
                    Utils.escapeHtml(modelName) + '</span>' : '') + '</div>' +
            '<table class="gguf-mp-table" style="margin-top:4px;"><thead><tr>' +
                '<th>' + Utils.escapeHtml(_('gguf.donor_node', 'Узел')) + '</th>' +
                '<th>' + Utils.escapeHtml(_('gguf.donor_layers', 'Слои')) + '</th>' +
                '<th>' + Utils.escapeHtml(_('gguf.donor_mem', 'Память (оценка)')) + '</th>' +
            '</tr></thead><tbody>' + rows.join('') + '</tbody></table>' +
            '<div style="font-size:11px;color:var(--text-secondary);margin-top:4px;">' +
                Utils.escapeHtml(_('gguf.donor_hint',
                    'Оценка памяти — по доле слоёв модели; точную память узла воркер по-узлово не отдаёт.')) +
            '</div></div>';
    }

    function renderLoadedPane() {
        const models = state.loadedModels || [];
        const loadingArr = (state.selectedBackendId && state.loadingModels && state.loadingModels[state.selectedBackendId]) || [];
        const unloadingArr = (state.selectedBackendId && state.unloadingModels && state.unloadingModels[state.selectedBackendId]) || [];
        // Выгружаемые, которых УЖЕ нет в списке загруженных: cppworker удаляет
        // запись из реестра в начале выгрузки, поэтому карточка нужна отдельно —
        // иначе в момент самой долгой операции (FreeModel) на экране не было бы
        // вообще ничего.
        const unloadingStandalone = unloadingArr.filter(function(item) {
            const want = String(item.handle || item.name || '');
            return !models.some(function(m) {
                return String(m.handle || m.model || m.name || '') === want;
            });
        });
        if (models.length === 0 && loadingArr.length === 0 && unloadingArr.length === 0) {
            return '<div class="gguf-empty-state">' +
                '<div class="empty-icon"><i class="fas fa-brain"></i></div>' +
                '<div>' + _('gguf.no_loaded_models') + '</div>' +
            '</div>';
        }
        // ==== Loading models block (Issue: «отображение состояния загрузки») ====
        // Рисуется ПЕРЕД списком загруженных моделей, чтобы пользователь видел,
        // что идёт загрузка. Каждая модель — карточка с индикатором прогресса и elapsed.
        const loadingHtml = loadingArr.map(function (lm) {
            const startedAt = lm.loadingStartedAt ? new Date(lm.loadingStartedAt).getTime() : Date.now();
            const elapsedMs = (lm.elapsedMs && lm.elapsedMs > 0) ? lm.elapsedMs : (Date.now() - startedAt);
            const elapsedSec = Math.max(0, Math.floor(elapsedMs / 1000));
            const elapsedLabel = elapsedSec < 60
                ? elapsedSec + 's'
                : Math.floor(elapsedSec / 60) + 'm ' + (elapsedSec % 60) + 's';
            let stateIcon, stateColor, stateText;
            if (lm.state === 'error') {
                stateIcon = 'fa-times-circle';
                stateColor = 'var(--danger, #d9534f)';
                stateText = (_('gguf.load_error_short') || 'Load error');
            } else {
                stateIcon = 'fa-spinner fa-spin';
                stateColor = 'var(--accent, #4a9eff)';
                stateText = (_('gguf.loading_indicator') || 'Loading');
            }
            return '<div class="gguf-loaded-model-item gguf-loaded-model-loading" style="border-left:3px solid ' + stateColor + ';">' +
                '<div class="gguf-loaded-model-name">' +
                    '<i class="fas ' + stateIcon + '" style="color:' + stateColor + ';margin-right:6px;"></i>' +
                    Utils.escapeHtml(lm.name) +
                '</div>' +
                '<div class="gguf-loaded-model-info">' +
                    '<span style="color:' + stateColor + ';">' + stateText + '</span>' +
                    '<span style="margin-left:12px;">' + (_('gguf.elapsed') || 'Elapsed') + ': ' + elapsedLabel + '</span>' +
                '</div>' +
                (lm.error ? '<div class="gguf-loaded-model-info" style="color:var(--danger,#d9534f);font-size:11px;">' +
                    '<i class="fas fa-exclamation-triangle"></i> ' + Utils.escapeHtml(lm.error) +
                '</div>' : '') +
            '</div>';
        }).join('');
        const loadedHtml = models.map(function (m) {
            const name = m.model || m.name || m.path || '-';
            // Ищем runtime-параметры (n_ctx, gpu_layers) этой модели.
            // Ключ ищем по имени или по path, потому что cppworker может вернуть
            // «gemma-4.gguf» а loadedModels — «gemma-4» (без расширения).
            //
            // R66d (2026-09-23): добавлен поиск без расширения .gguf и по basename,
            // потому что карта runtimeModels приходит из
            // /api/v1/cppworker/config/runtime с ключом m.name (без .gguf), а
            // список загруженных моделей балансера может содержать имя файла.
            const rt = lookupRuntimeModel(state.runtimeModels, name, m.path);
            // R83/v67: contextLength — поле, которое реально отдаёт балансер в
            // loadedModels (раньше здесь были только ctxSize/contextSize, поэтому
            // в строке состояния показывался прочерк «ctx_size: -»).
            const ctx = m.ctxSize || m.contextSize || m.contextLength || (rt && rt.context_size) || '-';
            const vram = m.vramBytes ? formatFileSize(m.vramBytes) : (m.vramUsage ? m.vramUsage + ' MB' : '-');
            // Runtime-блок (показываем рядом с default если rt есть).
            // Поля из cppworker: context_size, gpu_layers, batch_size, flash_attn_type,
            // gguf_context_length, n_layers, n_embd, state.
            let rtHtml = '';
            if (rt) {
                const rtCtx = rt.context_size || 0;
                const rtGpu = (rt.gpu_layers !== undefined) ? rt.gpu_layers : null;
                const rtBatch = rt.batch_size || 0;
                const rtFa = rt.flash_attn_type;
                const rtLayers = rt.n_layers || 0;
                const rtState = rt.state || 'loaded';
                const rtCtxBadge = (rtCtx && rtCtx !== ctx) ? '<span class="badge" style="background:var(--accent);margin-left:6px;">runtime: ' + Utils.escapeHtml(String(rtCtx)) + '</span>' : '';
                const rtGpuBadge = (rtGpu !== null && rtGpu !== undefined && rtGpu !== m.gpuLayers) ?
                    '<span class="badge" style="background:var(--bg-tertiary);margin-left:6px;padding:2px 6px;font-size:10px;">gpu: ' + Utils.escapeHtml(String(rtGpu)) + '</span>' : '';
                const meta = [];
                // R83/v67 (2026-10-02): ctx/gpu_layers/batch/flash_attn/layers/mmap
                // переехали в строку флагов (buildLoadedFlags) — раньше они были
                // здесь, но без kv-cache, параллельности и reasoning, и по строке
                // нельзя было понять, применились ли настройки модели.
                if (rt.gguf_context_length && rt.gguf_context_length !== rtCtx) {
                    meta.push('<span title="Max n_ctx per GGUF metadata">gguf_max=' + Utils.escapeHtml(String(rt.gguf_context_length)) + '</span>');
                }
                // R66d: CPU-offload — прямая причина «модель работает медленно,
                // хотя VRAM свободна». cppworker в auto-режиме (gpu_layers=-2)
                // сам уменьшает число слоёв на GPU, если не хватает VRAM под
                // KV-cache. В UI это было не видно: показывались только
                // запрошенные/default-значения, поэтому пользователь не понимал,
                // почему генерация медленная.
                let offloadBadge = '';
                if (rtGpu !== null && rtGpu !== undefined && rtLayers > 0) {
                    if (rtGpu === 0) {
                        offloadBadge = '<span class="badge" style="background:rgba(217,83,79,0.2);color:#ff7b76;margin-left:6px;" ' +
                            'title="' + Utils.escapeHtml(_('gguf.cpu_only_title') || 'Модель загружена целиком на CPU (gpu_layers=0) — генерация будет очень медленной') + '">' +
                            (Utils.escapeHtml(_('gguf.cpu_only') || 'CPU only')) + '</span>';
                    } else if (rtGpu > 0 && rtGpu < rtLayers) {
                        const missed = rtLayers - rtGpu;
                        offloadBadge = '<span class="badge" style="background:rgba(240,173,78,0.2);color:#f0ad4e;margin-left:6px;" ' +
                            'title="' + Utils.escapeHtml(_('gguf.partial_offload_title') || 'Часть слоёв на CPU: слотов на GPU не хватило под выбранный n_ctx/VRAM. Уменьшите n_ctx или kv-cache, чтобы ускорить генерацию') + '">' +
                            (Utils.escapeHtml(_('gguf.partial_offload') || 'CPU offload')) + ': ' + rtGpu + '/' + rtLayers + '</span>';
                    }
                }
                if (meta.length > 0 || offloadBadge) {
                    rtHtml = '<div class="gguf-loaded-model-info" style="margin-top:4px;display:flex;gap:12px;flex-wrap:wrap;font-family:monospace;font-size:11px;">' +
                        meta.join('') + offloadBadge +
                        '</div>';
                }
            }
            // Round 26 v0.5.13: busy badge. Если activeQueries > 0, показываем
            // "🔴 Generating (N active)" inline рядом с именем модели. Это
            // разъясняет пользователю, почему apply может ждать.
            //
            // Round 32 #2 (2026-08-10): busy badge теперь имеет встроенную кнопку
            // "Cancel" — клик вызывает cppworkerCancelGeneration.post(backend.id, name),
            // что отправляет POST /api/cancel в cppworker → AbortWatcher →
            // bridge.RequestAbort → C-bridge прерывает текущий llama_decode при
            // следующей проверке abort флага (каждые ~100ms после Round 32 n_batch=64).
            // Без этой кнопки пользователь не мог отменить генерацию из WebUI —
            // приходилось закрывать чат-клиент (Cline/OpenWebUI) чтобы balancer
            // увидел TCP close.
            const activeCount = (state.activeQueries && state.activeQueries[name]) || 0;
            const busyBadge = activeCount > 0
                ? '<span class="gguf-loaded-busy-badge" style="display:inline-flex;align-items:center;gap:6px;margin-left:8px;padding:2px 8px;background:rgba(217,83,79,0.15);color:#ff7b76;border-radius:10px;font-size:11px;font-weight:600;" title="' + (_('gguf.busy_badge_title') || 'Active generations') + '">' +
                    '<span style="display:inline-block;width:6px;height:6px;background:#ff7b76;border-radius:50%;animation:gguf-pulse 1.2s infinite;"></span>' +
                    (_('gguf.busy_badge') || 'Generating') + ' (' + activeCount + ' ' + (_('gguf.active_short') || 'active') + ')' +
                    // Round 32 #2: cancel button внутри busy badge.
                    // data-model-name используется в event handler для определения
                    // какую модель отменять.
                    '<button class="btn btn-xs gguf-cancel-gen-btn" data-model-name="' + Utils.escapeHtml(name) + '" ' +
                            'style="margin-left:4px;padding:1px 8px;font-size:10px;line-height:1.4;background:#d9534f;color:#fff;border:none;border-radius:8px;cursor:pointer;" ' +
                            'title="' + (_('gguf.cancel_generation') || 'Cancel active generation') + '">' +
                        '<i class="fas fa-times"></i> ' + (_('common.cancel') || 'Cancel') +
                    '</button>' +
                  '</span>'
                : '';

            // R83/v67 (2026-10-02): строка состояния со ВСЕМИ флагами модели.
            // Жалоба оператора: «не выводится информация о всех флагах в строке
            // состояния Загруженные у модели на странице GGUF Models» — без
            // kv/parallel/reasoning нельзя было убедиться, что настройки
            // применились к загруженной модели.
            const flags = buildLoadedFlags(m, rt);
            const flagsHtml = flags.length > 0
                ? '<div class="gguf-loaded-model-flags" style="margin-top:4px;display:flex;gap:8px;flex-wrap:wrap;font-family:monospace;font-size:11px;">' +
                    flags.map(function (f) {
                        const color = f.tone === 'ok' ? '#5cb85c'
                            : (f.tone === 'warn' ? '#f0ad4e'
                            : (f.tone === 'off' ? 'var(--text-muted)' : 'var(--text-secondary)'));
                        return '<span class="gguf-flag" style="color:' + color + ';padding:1px 6px;background:var(--bg-tertiary);border-radius:6px;" ' +
                            'title="' + Utils.escapeHtml(f.title) + '">' +
                            Utils.escapeHtml(f.label) + '=' + Utils.escapeHtml(f.value) + '</span>';
                    }).join('') +
                  '</div>'
                : '';

            const unloading = unloadingArr.some(function(item) {
                const want = String(item.handle || item.name || '');
                return want && (want === String(m.handle || '') || want === name);
            });
            // R119 (пункт 4): рядом со значениями показываем ИСТОЧНИК каждого
            // параметра загрузки — иначе «настройки применились?» остаётся догадкой.
            const sourcesHtml = renderParamSources(m.parameters || (rt && rt.parameters));
            return '<div class="gguf-loaded-model-item">' +
                '<div class="gguf-loaded-model-name">' + Utils.escapeHtml(name) + busyBadge + '</div>' +
                '<div class="gguf-loaded-model-info">' +
                    '<span>' + _('gguf.ctx_size') + ': ' + ctx + '</span>' +
                    '<span style="margin-left:12px;">VRAM: ' + vram + '</span>' +
                '</div>' +
                flagsHtml +
                sourcesHtml +
                rtHtml +
                // Выгружается: кнопка «Выгрузить» заменяется на состояние с
                // таймером. Так оператор видит, что команда принята и работа
                // идёт, а не нажимает кнопку повторно (второй unload вернул бы
                // «model not found»).
                (unloading
                    ? '<div class="gguf-loaded-model-unloading-inline" style="display:flex;align-items:center;gap:8px;margin-top:6px;font-size:12px;color:var(--warning, #e0a030);">' +
                        '<i class="fas fa-spinner fa-spin"></i>' +
                        '<span>' + Utils.escapeHtml(_('gguf.unloading', 'Выгружается…')) + '</span>' +
                        '<span style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.elapsed', 'Прошло')) + ': ' +
                            formatElapsedShort(unloadingElapsedMs(unloadingArr.filter(function(item) {
                                const want = String(item.handle || item.name || '');
                                return want === String(m.handle || '') || want === name;
                            })[0])) + '</span>' +
                      '</div>'
                    : '<button class="btn btn-sm btn-danger gguf-unload-btn" data-admin-only data-handle="' + Utils.escapeHtml(m.handle || name) + '">' +
                        '<i class="fas fa-stop"></i> ' + _('gguf.unload_model') +
                      '</button>') +
                donorContributionHtml(m) +
                // R126 (пункт 5): параметры загруженной модели можно раскрыть,
                // изменить и применить — модель уйдёт в перезагрузку. Значения
                // предзаполняем тем, что известно о загруженном инстансе.
                '<details class="gguf-mp-acc" data-model-acc="' + Utils.escapeHtml(name) + '">' +
                    '<summary class="gguf-mp-summary"><i class="fas fa-sliders"></i> ' +
                        Utils.escapeHtml(_('gguf.mp_open_loaded', 'Параметры модели — изменить и применить')) + '</summary>' +
                    renderModelParamsEditor(name, loadedParamsFor(m, rt), { source: 'loaded', model: m, runtime: rt }) +
                '</details>' +
            '</div>';
        }).join('');
        const unloadingHtml = unloadingStandalone.map(function(item) { return renderUnloadingCard(item); }).join('');
        return (unloadingHtml ? '<div class="gguf-loaded-unloading-block">' + unloadingHtml + '</div>' : '') +
            (loadingHtml ? '<div class="gguf-loaded-loading-block">' + loadingHtml + '</div>' : '') + loadedHtml;
    }

    function renderDownloadsPane() {
        const dls = state.activeDownloads || [];
        const history = state.downloadHistory || [];
        const orphans = state.orphanDownloads || [];
        const activeHtml = dls.length === 0
            ? ('<div class="gguf-empty-state" style="margin-bottom:16px;">' +
                '<div class="empty-icon"><i class="fas fa-download"></i></div>' +
                '<div>' + _('gguf.no_active_downloads') + '</div>' +
               '</div>')
            : ('<div class="gguf-downloads-section">' +
                '<h5 style="margin:0 0 8px 0;font-size:13px;color:var(--text-secondary);text-transform:uppercase;letter-spacing:0.5px;">' +
                    _('gguf.active_downloads_for_backend') + '</h5>' +
                dls.map(renderDownloadItem).join('') +
               '</div>');

        const historyItems = history.filter(function (h) {
            // Не дублируем активные в истории
            return !dls.some(function (dl) { return dl.modelId === h.modelId && dl.filename === h.filename; });
        });
        const historyHtml = historyItems.length === 0
            ? ''
            : ('<div class="gguf-downloads-section" style="margin-top:16px;padding-top:16px;border-top:1px solid var(--border-color);">' +
                '<h5 style="margin:0 0 8px 0;font-size:13px;color:var(--text-secondary);text-transform:uppercase;letter-spacing:0.5px;">' +
                    (_('gguf.download_history') || 'Recent downloads') + '</h5>' +
                historyItems.map(renderDownloadItem).join('') +
               '</div>');

        // R66.4 (2026-09-16): orphan .download файлы — то, что осталось после сбоев.
        // cppworker возвращает filename+size, но НЕ modelId (нельзя восстановить
        // из имени файла). UI показывает "Residual files" отдельным блоком.
        const orphanHtml = orphans.length === 0
            ? ''
            : ('<div class="gguf-downloads-section gguf-orphans-section" style="margin-top:16px;padding-top:16px;border-top:1px solid var(--warning);">' +
                '<h5 style="margin:0 0 8px 0;font-size:13px;color:var(--warning);text-transform:uppercase;letter-spacing:0.5px;">' +
                    '<i class="fas fa-exclamation-triangle"></i> ' +
                    (_('gguf.residual_downloads') || 'Residual files (orphans)') +
                '</h5>' +
                '<div style="font-size:11px;color:var(--text-muted);margin-bottom:8px;">' +
                    Utils.escapeHtml(_('gguf.orphans_hint') || 'Partial .download files left from interrupted downloads. They take disk space but are not associated with any active download.') +
                '</div>' +
                orphans.map(renderOrphanItem).join('') +
                '<div style="margin-top:8px;text-align:right;">' +
                    '<button class="btn btn-sm btn-warning gguf-cleanup-all-orphans-btn" id="ggufCleanupAllOrphans">' +
                        '<i class="fas fa-broom"></i> ' +
                        Utils.escapeHtml(_('gguf.cleanup_all_orphans') || 'Clear all residual files') +
                    '</button>' +
                '</div>' +
               '</div>');

        return activeHtml + historyHtml + orphanHtml;
    }

    function renderOrphanItem(o) {
        const sizeLabel = o.size > 0 ? formatBytesShort(o.size) : '?';
        const ageLabel = o.modified
            ? new Date(o.modified * 1000).toLocaleString()
            : '-';
        // modelId неизвестен (filename не кодирует репо), поэтому для cleanup
        // передаём пустую строку. Cppworker cleanup ищет файл ТОЛЬКО по filename,
        // modelId игнорируется при поиске temp/final файлов.
        return '<div class="gguf-orphan-item" style="display:flex;align-items:center;gap:8px;padding:6px 8px;background:var(--bg-secondary);border-radius:6px;margin-bottom:4px;">' +
            '<i class="fas fa-file-archive" style="color:var(--warning);"></i>' +
            '<div style="flex:1;min-width:0;">' +
                '<div style="font-family:monospace;font-size:12px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;" title="' + Utils.escapeHtml(o.path || o.filename) + '">' +
                    Utils.escapeHtml(o.filename) +
                '</div>' +
                '<div style="font-size:10px;color:var(--text-muted);">' +
                    Utils.escapeHtml(sizeLabel) + ' · ' + Utils.escapeHtml(ageLabel) +
                '</div>' +
            '</div>' +
            '<button class="btn btn-sm btn-danger gguf-delete-orphan-btn" data-filename="' + Utils.escapeHtml(o.filename) + '" title="' + Utils.escapeHtml(_('gguf.delete_from_disk') || 'Delete downloaded file from disk') + '">' +
                '<i class="fas fa-trash"></i>' +
            '</button>' +
        '</div>';
    }

    function renderDownloadItem(dl) {
        const progress = state.downloadProgress[dl.modelId + '/' + dl.filename];
        const pct = progress ? (progress.progressPct || progress.percent || progress.completed || 0) : (dl.progressPct || dl.ProgressPct || 0);
        const speed = progress ? (progress.speedBps || progress.speed || '') : (dl.speedBps || dl.speed || '');
        const status = progress ? (progress.status || '') : (dl.status || dl.Status || '');
        const isCompleted = status === 'completed';
        const isFailed = status === 'failed' || status === 'error';
        const isCancelled = status === 'cancelled';
        const isInterrupted = status === 'interrupted'; // Round 17.3
        const fillColor = isCompleted ? 'var(--success)' : isFailed ? 'var(--danger)' : isCancelled ? 'var(--text-muted)' : (isInterrupted ? 'var(--warning)' : 'var(--accent)');
        const totalBytes = dl.totalBytes || dl.TotalBytes || 0;
        const downloaded = dl.downloaded || dl.Downloaded || 0;
        const sizeLabel = totalBytes > 0
            ? (formatFileSize(downloaded) + ' / ' + formatFileSize(totalBytes))
            : (downloaded > 0 ? formatFileSize(downloaded) : '');
        const speedLabel = speed ? formatFileSize(speed) + '/s' : '';
        // Round 17.3 (2026-08-03): пути + resume state.
        const tempPath = progress ? (progress.tempPath || '') : (dl.tempPath || dl.TempPath || '');
        const finalPath = progress ? (progress.finalPath || '') : (dl.finalPath || dl.FinalPath || '');
        const resumable = progress ? !!progress.resumable : !!dl.resumable;
        const resumedFrom = progress ? (progress.resumedFrom || 0) : (dl.resumedFrom || 0);
        const isActive = (status === 'downloading' || status === 'queued' || status === 'starting' || status === '');

        return '<div class="gguf-download-item">' +
            '<div class="gguf-download-info">' +
                '<div class="gguf-download-model">' + Utils.escapeHtml(dl.modelId) + '</div>' +
                '<div class="gguf-download-file">' + Utils.escapeHtml(dl.filename) + '</div>' +
                // Round 17.3: показываем пути маленьким серым текстом.
                (finalPath ? '<div class="gguf-download-paths" style="font-size:10px;color:var(--text-muted);margin-top:2px;line-height:1.4;">' +
                    '<i class="fas fa-folder-open" style="margin-right:3px;"></i>' + Utils.escapeHtml(finalPath) +
                    (tempPath ? '<br><i class="fas fa-file-download" style="margin-right:3px;"></i><span style="opacity:0.8;">temp: </span>' + Utils.escapeHtml(tempPath) : '') +
                  '</div>' : '') +
                (resumable ? '<div style="font-size:10px;color:var(--warning);margin-top:2px;"><i class="fas fa-pause-circle"></i> ' +
                    (_('gguf.partial_download_resumable') || 'Partial download — can be resumed') +
                    (resumedFrom > 0 ? ' (' + formatFileSize(resumedFrom) + ' ' + (_('gguf.already_downloaded') || 'already downloaded') + ')' : '') +
                  '</div>' : '') +
            '</div>' +
            '<div class="gguf-download-progress-bar">' +
                '<div class="gguf-progress-fill" style="width:' + pct + '%;background:' + fillColor + '"></div>' +
            '</div>' +
            '<div class="gguf-download-stats">' +
                '<span>' + Math.round(pct) + '%</span>' +
                (sizeLabel ? '<span>' + sizeLabel + '</span>' : '') +
                (speedLabel ? '<span>' + speedLabel + '</span>' : '') +
                (status ? '<span style="margin-left:8px;text-transform:capitalize;">' + status + '</span>' : '') +
            '</div>' +
            '<div class="gguf-download-actions">' +
                // Cancel для активных
                (isActive
                    ? '<button class="btn btn-sm btn-danger gguf-cancel-dl-btn" data-model-id="' + Utils.escapeHtml(dl.modelId) + '" data-filename="' + Utils.escapeHtml(dl.filename) + '">' +
                        '<i class="fas fa-ban"></i> ' + _('gguf.cancel_download') +
                      '</button>'
                    : '') +
                // Round 17.3: Resume для прерванных
                (isInterrupted && resumable
                    ? '<button class="btn btn-sm btn-warning gguf-resume-dl-btn" data-model-id="' + Utils.escapeHtml(dl.modelId) + '" data-filename="' + Utils.escapeHtml(dl.filename) + '" data-quantization="' + Utils.escapeHtml(dl.quantization || '') + '">' +
                        '<i class="fas fa-play"></i> ' + (_('gguf.resume_download') || 'Resume') +
                      '</button>'
                    : '') +
                // Round 17.3: Delete для completed/failed/interrupted
                ((isCompleted || isFailed || isCancelled || isInterrupted)
                    ? '<button class="btn btn-sm btn-secondary gguf-delete-dl-btn" data-model-id="' + Utils.escapeHtml(dl.modelId) + '" data-filename="' + Utils.escapeHtml(dl.filename) + '" title="' + (_('gguf.delete_from_disk') || 'Delete downloaded file from disk') + '">' +
                        '<i class="fas fa-trash"></i> ' + (_('gguf.delete') || 'Delete') +
                      '</button>'
                    : '') +
            '</div>' +
        '</div>';
    }

    function renderHuggingFacePane() {
        return '' +
            '<div class="gguf-search-form">' +
                '<div class="gguf-search-row">' +
                    '<input type="text" id="ggufHfDetailSearch" data-focus-key="hf-search" class="form-control gguf-search-input" value="' + Utils.escapeHtml(state.hfSearchQuery) + '" placeholder="' + _('gguf.search_placeholder') + '">' +
                    '<button class="btn btn-primary" id="ggufHfDetailSearchBtn">' + _('gguf.search_btn') + '</button>' +
                '</div>' +
            '</div>' +
            '<div id="ggufHfSearchResults">' +
                renderHfSearchResults() +
            '</div>' +
            '<div id="ggufHfModelFiles" style="' + (state.hfSearchSelected ? '' : 'display:none;') + ';margin-top:16px;">' +
                renderHfModelFiles() +
            '</div>';
    }

    /**
     * Format bytes as human-readable size (KB, MB, GB, TB).
     */
    function formatBytesShort(bytes) {
        if (!bytes || bytes === 0) return '-';
        var units = ['B', 'KB', 'MB', 'GB', 'TB'];
        var i = Math.floor(Math.log(bytes) / Math.log(1024));
        return (bytes / Math.pow(1024, i)).toFixed(i > 0 ? 1 : 0) + ' ' + units[i];
    }

    /**
     * Empty-state hints shown before the user types anything.
     * Suggestion chips are clickable — handler in gguf-renderer-detail.js sets
     * the input value and triggers a search.
     */
    function renderHfEmptyHints() {
        var suggestions = [
            { label: 'Qwen3', q: 'Qwen3' },
            { label: 'Llama-3', q: 'Llama-3' },
            { label: 'gemma-2', q: 'gemma-2' },
            { label: 'Mistral', q: 'Mistral' },
            { label: 'deepseek', q: 'deepseek' }
        ];
        var direct = [
            { label: 'Qwen/Qwen3-8B-Instruct', q: 'Qwen/Qwen3-8B-Instruct' },
            { label: 'google/gemma-2-9b-it-GGUF', q: 'google/gemma-2-9b-it-GGUF' }
        ];
        var suggHtml = suggestions.map(function (s) {
            return '<button class="gguf-suggestion-chip" data-suggestion="' + Utils.escapeHtml(s.q) + '" ' +
                'style="margin:4px 4px 0 0;padding:4px 10px;border-radius:14px;border:1px solid var(--border);background:var(--bg-secondary);cursor:pointer;font-size:12px;">' +
                Utils.escapeHtml(s.label) + '</button>';
        }).join('');
        var directHtml = direct.map(function (d) {
            return '<button class="gguf-direct-chip" data-direct="' + Utils.escapeHtml(d.q) + '" ' +
                'style="display:block;width:100%;text-align:left;margin:4px 0;padding:6px 10px;border-radius:6px;border:1px solid var(--border);background:var(--bg-secondary);cursor:pointer;font-family:monospace;font-size:12px;">' +
                Utils.escapeHtml(d.label) + '</button>';
        }).join('');
        return '<div class="gguf-empty-state gguf-search-empty" style="padding:12px 4px;">' +
            '<div style="display:flex;align-items:center;gap:8px;margin-bottom:6px;">' +
                '<i class="fab fa-huggingface" style="font-size:18px;color:#ff9a00;"></i>' +
                '<span style="font-weight:500;">' + Utils.escapeHtml(_('gguf.search_title') || 'HuggingFace Hub') + '</span>' +
            '</div>' +
            '<div style="color:var(--text-muted);font-size:12px;margin-bottom:8px;">' +
                Utils.escapeHtml(_('gguf.search_hint') || 'Type 2+ chars to search, or paste an org/model id (with "/") to jump straight to its files.') +
            '</div>' +
            '<div style="margin-top:10px;">' +
                '<div style="font-size:11px;color:var(--text-muted);text-transform:uppercase;letter-spacing:0.5px;margin-bottom:4px;">' +
                    Utils.escapeHtml(_('gguf.popular') || 'Popular') +
                '</div>' +
                '<div>' + suggHtml + '</div>' +
            '</div>' +
            '<div style="margin-top:12px;">' +
                '<div style="font-size:11px;color:var(--text-muted);text-transform:uppercase;letter-spacing:0.5px;margin-bottom:4px;">' +
                    Utils.escapeHtml(_('gguf.direct_link') || 'Or paste org/model id') +
                '</div>' +
                '<div>' + directHtml + '</div>' +
            '</div>' +
        '</div>';
    }

    /**
     * Universal field extractors for HF file records.
     * CppWorker API returns {path, sizeBytes, isGGUF, quantization};
     * raw HF API returns {rfilename, size, ...}.
     */
    function hfFileName(f) { return f.path || f.rfilename || f.filename || f.name || ''; }
    function hfFileSize(f) { return f.sizeBytes || f.size || 0; }
    function hfFileQuant(f) {
        if (f.quantization && f.quantization !== 'unknown') return f.quantization;
        var m = hfFileName(f).toUpperCase().match(/[._-](Q\d[_A-Z]*|F16|F32|FP16|FP32|BF16)[._-]/);
        return m ? m[1] : '';
    }
    function hfIsGgufFile(f) {
        if (f.isGGUF === true) return true;
        if (f.isGGUF === false) return false;
        return /\.gguf$/i.test(hfFileName(f));
    }
    /**
     * Filter out non-model auxiliary files (mmproj, etc.) and limit to top-N
     * most popular quants. Возвращает top файлов по приоритету Q4_K_M > Q5_K_M > ...
     */
    function pickTopGgufFiles(files, maxCount) {
        if (!Array.isArray(files) || files.length === 0) return [];
        var popQ = ['Q4_K_M','Q5_K_M','Q6_K','Q8_0','Q4_0','Q4_K_S','Q3_K_M','Q2_K'];
        var gguf = files.filter(function (f) {
            return hfIsGgufFile(f) && !/mmproj/i.test(hfFileName(f));
        });
        gguf = gguf.map(function (f) { f.__quant = hfFileQuant(f); return f; });
        gguf.sort(function (a, b) {
            var qa = (a.__quant || '').toUpperCase();
            var qb = (b.__quant || '').toUpperCase();
            var pa = popQ.indexOf(qa);
            var pb = popQ.indexOf(qb);
            if (pa === -1) pa = 999;
            if (pb === -1) pb = 999;
            if (pa !== pb) return pa - pb;
            return (hfFileSize(a) || 0) - (hfFileSize(b) || 0);
        });
        return gguf.slice(0, maxCount || 6);
    }

    function renderHfSearchResults() {
        if (state.hfSearching) {
            return '<div class="gguf-empty-state"><i class="fas fa-spinner fa-spin"></i> ' +
                (_('gguf.searching_for', { q: state.hfSearchQuery }) || ('Searching for "' + Utils.escapeHtml(state.hfSearchQuery) + '"...')) +
                '</div>';
        }
        // Show structured error if the last search failed
        if (state.hfLastError) {
            return '<div class="gguf-empty-state gguf-search-error">' +
                '<i class="fas fa-exclamation-triangle" style="color:var(--warning,#e0a030);font-size:18px;"></i>' +
                '<div style="margin-top:6px;font-weight:500;">' + Utils.escapeHtml(state.hfLastError.title) + '</div>' +
                '<div style="margin-top:4px;color:var(--text-muted);font-size:12px;">' + Utils.escapeHtml(state.hfLastError.hint) + '</div>' +
            '</div>';
        }
        // R66.2 (2026-09-16): на случай если state.hfSearchResults стал не-массивом
        // (старый код приводил к TypeError "filter is not a function" — infinite loading).
        // Защищаемся: если пришло что-то не-array, считаем пустым.
        var resultsArr = Array.isArray(state.hfSearchResults) ? state.hfSearchResults : [];
        if (!resultsArr.length) {
            // No results yet — different message depending on whether the user typed something
            if (!state.hfSearchQuery || state.hfSearchQuery.trim().length < 2) {
                return renderHfEmptyHints();
            }
            return '<div class="gguf-empty-state">' +
                '<i class="fas fa-search" style="opacity:0.4;"></i> ' +
                '<div style="margin-top:6px;">' + Utils.escapeHtml(_('gguf.no_results_for', { q: state.hfSearchQuery }) || ('No results for "' + state.hfSearchQuery + '"')) + '</div>' +
                '<div style="margin-top:4px;color:var(--text-muted);font-size:12px;">' + Utils.escapeHtml(_('gguf.try_short_query') || 'Try a shorter query, e.g. "Qwen", "Llama-3", "gemma-2-9b"') + '</div>' +
            '</div>';
        }
        var filtered = resultsArr.filter(function (m) {
            return m && m.files && Array.isArray(m.files) && m.files.length > 0;
        });
        if (filtered.length === 0) {
            return '<div class="gguf-empty-state">' +
                '<i class="fas fa-box-open" style="opacity:0.4;"></i> ' +
                '<div style="margin-top:6px;">' + Utils.escapeHtml(_('gguf.no_gguf_files_found')) + '</div>' +
                '<div style="margin-top:4px;color:var(--text-muted);font-size:12px;">' +
                Utils.escapeHtml(_('gguf.no_gguf_files_hint') || 'Repository exists but contains no .gguf files (maybe GGUF is in a different repo or branch).') +
                '</div>' +
            '</div>';
        }
        var toolbar = filtered.length > 1
            ? '<div class="gguf-results-toolbar" style="display:flex;justify-content:space-between;align-items:center;gap:8px;margin-bottom:8px;">' +
                '<span class="gguf-results-count" style="font-size:12px;color:var(--text-muted);">' +
                    Utils.escapeHtml(_('gguf.results_count', { n: filtered.length }) || (filtered.length + ' models')) +
                '</span>' +
                '<button class="btn btn-sm btn-secondary gguf-collapse-all-btn" title="' + (_('gguf.collapse_all') || 'Collapse all') + '">' +
                    '<i class="fas fa-compress-arrows-alt"></i> ' + (_('gguf.collapse_all') || 'Collapse all') +
                '</button>' +
              '</div>'
            : '<div class="gguf-results-toolbar" style="display:flex;justify-content:flex-end;margin-bottom:8px;">' +
                '<span class="gguf-results-count" style="font-size:12px;color:var(--text-muted);margin-right:auto;">' +
                    Utils.escapeHtml(_('gguf.results_count', { n: filtered.length }) || (filtered.length + ' model')) +
                '</span>' +
              '</div>';
        return toolbar + '<div class="gguf-results-list">' +
            filtered.map(function (model, idx) {
                const downloads = model.downloads || 0;
                const likes = model.likes || 0;
                const updated = model.lastModified ? new Date(model.lastModified).toLocaleDateString() : '-';
                const modelId = model.id || model.modelId || '';
                const isLikelyGGUF = /gguf/i.test(modelId) || (model.hasGguf === true);
                const totalSize = model.totalSize || 0;
                const recommended = model.recommended || '';
                // COLLAPSED_VISIBLE — сколько файлов видно по умолчанию. Остальные
                // НЕ выбрасываются: их отдаёт кнопка «Показать все» (см. обработчик
                // .gguf-files-more-btn в gguf-renderer-detail.js). Раньше строка
                // «+N ещё файлов» была просто текстом без обработчика, и добраться
                // до остальных файлов репозитория было нельзя вообще.
                const COLLAPSED_VISIBLE = 5;
                const topFiles = pickTopGgufFiles(model.files, COLLAPSED_VISIBLE);
                const allFiles = model.files || [];
                
                // Хвост = файлы, которых нет в topFiles (сравнение по имени, а не по
                // индексу: pickTopGgufFiles сортирует по приоритету кванта).
                const topNames = {};
                topFiles.forEach(function (f) { topNames[hfFileName(f)] = true; });
                const restFiles = allFiles.filter(function (f) { return !topNames[hfFileName(f)]; });
                const restCount = restFiles.length;

                // fileRowHtml — одна строка файла. Вынесено из прежнего инлайнового
                // forEach, чтобы одинаково рисовать и видимые, и раскрытые файлы
                // (разметка обязана совпадать: на .gguf-download-file-btn и
                // .gguf-file-progress завязаны делегированные обработчики).
                const fileRowHtml = function (f) {
                    var fname = hfFileName(f);
                    var fsize = formatBytesShort(hfFileSize(f));
                    var fquant = hfFileQuant(f) || '-';
                    var isRecommended = recommended && fname === recommended;
                    return '<div class="gguf-file-row" data-model-idx="' + idx + '" data-filename="' + Utils.escapeHtml(fname) + '" style="display:flex;align-items:center;gap:8px;padding:6px 4px;border-bottom:1px solid var(--border-subtle,#eee);">' +
                        '<i class="fas fa-cube" style="color:var(--text-muted);"></i>' +
                        '<span style="flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-family:monospace;font-size:12px;" title="' + Utils.escapeHtml(fname) + '">' + Utils.escapeHtml(fname) + '</span>' +
                        '<span class="badge" style="background:var(--accent);font-size:10px;">' + Utils.escapeHtml(fquant) + '</span>' +
                        '<span style="font-size:11px;color:var(--text-muted);min-width:60px;text-align:right;">' + fsize + '</span>' +
                        (isRecommended ? '<span class="badge" style="background:var(--success);font-size:9px;" title="' + (_('gguf.recommended') || 'Recommended') + '">' + (_('gguf.recommended') || '★') + '</span>' : '') +
                        '<button class="btn btn-sm btn-primary gguf-download-file-btn" data-model-idx="' + idx + '" data-filename="' + Utils.escapeHtml(fname) + '" title="' + (_('gguf.download') || 'Download') + ': ' + Utils.escapeHtml(fname) + '" style="padding:2px 8px;font-size:12px;">' +
                            '<i class="fas fa-download"></i>' +
                        '</button>' +
                        '<div class="gguf-file-progress" data-model-idx="' + idx + '" data-filename="' + Utils.escapeHtml(fname) + '" style="display:none;flex-basis:100%;margin-top:4px;">' +
                            '<div class="gguf-progress-bar" style="height:4px;background:var(--bg-tertiary,#333);border-radius:2px;overflow:hidden;">' +
                                '<div class="gguf-progress-fill" style="height:100%;width:0%;background:var(--success);transition:width 0.2s;"></div>' +
                            '</div>' +
                            '<div class="gguf-progress-text" style="font-size:10px;color:var(--text-muted);margin-top:2px;">0%</div>' +
                        '</div>' +
                    '</div>';
                };

                const filesHtml = (function () {
                    if (topFiles.length === 0) return '';
                    var html = '<div class="gguf-files-list" style="margin-top:10px;border-top:1px solid var(--border);padding-top:8px;">';
                    html += topFiles.map(fileRowHtml).join('');
                    if (restCount > 0) {
                        // Свёрнутый хвост. Рисуется сразу (без второго запроса), но
                        // скрыт — раскрытие мгновенное и не зависит от сети.
                        html += '<div class="gguf-files-extra" style="display:none;">' +
                            restFiles.map(fileRowHtml).join('') +
                        '</div>';
                        html += '<button type="button" class="gguf-files-more-btn" ' +
                            'style="display:block;width:100%;padding:6px;margin-top:4px;background:transparent;border:1px dashed var(--border);border-radius:4px;text-align:center;font-size:11px;color:var(--accent);cursor:pointer;">' +
                            '<span class="gguf-files-more-label">+' + restCount + ' ' + (_('gguf.more_files') || 'more files') + ' — ' + (_('gguf.show_more_files') || 'Show all') + '</span>' +
                        '</button>';
                    }
                    html += '</div>';
                    return html;
                })();

                return '<div class="gguf-result-card" data-model-idx="' + idx + '">' +
                    '<div class="gguf-result-header" style="cursor:pointer;">' +
                        '<div class="gguf-result-title">' +
                            '<i class="fas fa-cube"></i> ' + Utils.escapeHtml(modelId) +
                        '</div>' +
                        '<div class="gguf-result-meta">' +
                            '<span title="' + _('gguf.downloads') + '"><i class="fas fa-download"></i> ' + downloads.toLocaleString() + '</span>' +
                            '<span title="' + _('gguf.likes') + '"><i class="fas fa-heart"></i> ' + likes.toLocaleString() + '</span>' +
                            '<span title="' + _('gguf.last_updated') + '"><i class="fas fa-calendar"></i> ' + updated + '</span>' +
                            (isLikelyGGUF ? '<span class="badge badge-info" title="GGUF"><i class="fas fa-cube"></i> GGUF</span>' : '') +
                            (totalSize > 0 ? '<span title="' + (_('gguf.total_size') || 'Total size') + '"><i class="fas fa-hdd"></i> ' + formatBytesShort(totalSize) + '</span>' : '') +
                        '</div>' +
                    '</div>' +
                    filesHtml +
                    '<div class="gguf-result-actions" style="display:flex;gap:6px;flex-wrap:wrap;margin-top:8px;">' +
                        '<button class="btn btn-sm btn-success gguf-quick-download-btn" data-model-idx="' + idx + '" title="' + (_('gguf.quick_download_tooltip') || 'Auto-pick best .gguf and start download') + '">' +
                            '<i class="fas fa-bolt"></i> ' + (_('gguf.download') || 'Download') + ' (' + (_('gguf.recommended') || 'Recommended') + ')' +
                        '</button>' +
                    '</div>' +
                '</div>';
            }).join('') +
        '</div>';
    }

    function renderHfModelFiles() {
        if (!state.hfSearchSelected) return '';
        const modelId = state.hfSearchSelected.id || state.hfSearchSelected.modelId || '';
        // R66.2 (2026-09-16): cppworker оборачивает files в {count, modelId, files: [...]} —
        // || [] ловит только falsy, объект пройдёт как truthy → .map() упадёт. Защищаемся.
        const files = Array.isArray(state.hfModelFiles) ? state.hfModelFiles : [];
        function fileName(f) { return f.path || f.rfilename || f.filename || f.name || ''; }
        function fileSize(f) { return f.sizeBytes || f.size || 0; }
        function isGgufFile(f) {
            if (f.isGGUF === true) return true;
            if (f.isGGUF === false) return false;
            return /\.gguf$/i.test(fileName(f));
        }
        return '' +
            '<div class="gguf-files-header" style="display:flex;justify-content:space-between;align-items:center;">' +
                '<h4 style="margin:0;font-size:14px;"><i class="fas fa-file"></i> ' + Utils.escapeHtml(modelId) + '</h4>' +
                '<button class="btn btn-sm btn-secondary" id="ggufCloseHfFiles"><i class="fas fa-times"></i> ' + _('common.close') + '</button>' +
            '</div>' +
            '<div class="gguf-files-grid">' +
                (files.length === 0
                    ? '<div class="gguf-empty-state">' + _('gguf.no_results') + '</div>'
                    : files.map(function (file) {
                        const fname = fileName(file);
                        const size = fileSize(file) ? formatFileSize(fileSize(file)) : '-';
                        const gguf = isGgufFile(file);
                        return '<div class="gguf-file-card">' +
                            '<div class="gguf-file-icon"><i class="fas fa-' + (gguf ? 'file' : 'file-lines') + '"></i></div>' +
                            '<div class="gguf-file-info">' +
                                '<div class="gguf-file-name">' + Utils.escapeHtml(fname) + '</div>' +
                                '<div class="gguf-file-size">' + size + '</div>' +
                            '</div>' +
                            '<div class="gguf-file-actions">' +
                                (gguf
                                    ? '<button class="btn btn-sm btn-primary gguf-hf-download-btn" data-filename="' + Utils.escapeHtml(fname) + '" data-model-id="' + Utils.escapeHtml(modelId) + '">' +
                                        '<i class="fas fa-download"></i> ' + _('gguf.download') +
                                      '</button>'
                                    : '') +
                            '</div>' +
                        '</div>';
                    }).join('')) +
            '</div>';
    }

    /**
     * Settings tab в gguf-рендерере.
     *
     * Раньше здесь была «мёртвая» форма gguf-settings-form, которая меняла только
     * локальный state.loadOptions и не отправляла ничего на бэкенд. Теперь таб
     * показывает два осмысленных блока, привязанных к выбранному бэкенду:
     *
     *   1) Backend load options — реальные llama.cpp дефолты cppworker'а.
     *      Загружаются и сохраняются через /api/v1/cppworker/config[+/update].
     *
     *   2) Per-Model Profiles — список профилей n_ctx, общий для всех
     *      зарегистрированных cppworker'ов (эндпоинт /api/v1/cppworker/model-profiles
     *      на балансировщике). Использует уже существующий модуль window.CppWorkerParams.
     */
    function renderSettingsPane() {
        const backend = M.currentBackend();
        const backendId = backend ? Utils.escapeHtml(backend.id) : '';
        const isRegistered = !!(backend && backend.url && backend.url.indexOf('http') === 0 && backend.id && /^(?:[a-z0-9_-]+)$/i.test(backend.id));
        // Round 32 #6 (2026-08-10): используем кэшированный HTML формы вместо
        // spinner-плейсхолдера, если форма уже отрендерена для этого бэкенда.
        // Без этого refreshDetailPane() (вызывается из active-queries polling
        // каждые 3s) затирал форму на «Loading…», и пользователь видел её
        // на краткий миг, потом она исчезала.
        const cached = backend && state._settingsFormHtmlByBackend[backend.id];
        const backendOptionsInner = cached
            ? cached.html
            : '<div class="loading"><i class="fas fa-spinner fa-spin"></i> ' + (window.I18N ? I18N.t('common.loading', 'Loading…') : 'Loading…') + '</div>';
        return '' +
            '<div class="gguf-settings-form">' +

            // ---------- (1) Backend load options ----------
            '<div class="settings-subsection" id="gguf-backend-options-section">' +
                '<h4 style="margin:0 0 6px 0;font-size:14px;display:flex;align-items:center;gap:8px;">' +
                    '<i class="fas fa-sliders-h"></i> ' +
                    '<span>' + _('gguf.backend_options_title') + '</span>' +
                '</h4>' +
                '<p style="color:var(--text-muted);margin:0 0 6px 0;font-size:12px;">' +
                    _('gguf.backend_options_desc') +
                '</p>' +
                // Round 32 #6 (2026-08-10): disclaimer что это ГЛОБАЛЬНЫЕ дефолты,
                // которые применяются при загрузке ЛЮБОЙ модели на этом бэкенде.
                // Per-model overrides — в Per-Model Profiles ниже. Без этого
                // disclaimer'а пользователи думали что settings «привязаны к
                // модели» или «не работают без загруженной модели».
                '<p style="color:var(--text-muted);margin:0 0 12px 0;font-size:11px;font-style:italic;">' +
                    '<i class="fas fa-info-circle"></i> ' +
                    Utils.escapeHtml(_('gguf.backend_options_global_hint', 'Эти параметры — глобальные дефолты для всех моделей на бэкенде. Per-model overrides — в секции «Per-Model Profiles» ниже. Доступны даже без загруженной модели.')) +
                '</p>' +
                '<div id="ggufBackendOptionsContainer" data-backend-id="' + backendId + '">' +
                    backendOptionsInner +
                '</div>' +
            '</div>' +

            // ---------- (2) Per-Model Profiles ----------
            '<div class="settings-subsection" id="gguf-profiles-section" style="margin-top:24px;padding-top:16px;border-top:1px solid var(--border-color, #2a3441);">' +
                '<h4 style="margin:0 0 6px 0;font-size:14px;display:flex;align-items:center;gap:8px;">' +
                    '<i class="fas fa-layer-group"></i> ' +
                    '<span>' + _('gguf.profiles_section_title') + '</span>' +
                '</h4>' +
                '<p style="color:var(--text-muted);margin:0 0 12px 0;font-size:12px;">' +
                    _('gguf.profiles_section_desc') +
                '</p>' +
                (isRegistered
                    ? ('<div id="cppProfilesList">' +
                        '<div class="cpp-profiles-empty">' + (window.I18N ? I18N.t('settings.profiles.empty', '') : '') + '</div>' +
                       '</div>' +
                       '<button class="btn btn-primary" id="cppProfileAddBtn" style="margin-top:8px;">' +
                           '<i class="fas fa-plus"></i> ' +
                           '<span>' + (window.I18N ? I18N.t('settings.profiles.add', 'Add profile…') : 'Add profile…') + '</span>' +
                       '</button>')
                    : ('<div class="gguf-empty-detail" style="padding:12px;text-align:left;">' +
                        '<i class="fas fa-info-circle"></i> ' + _('gguf.profiles_only_registered') +
                       '</div>')) +
            '</div>' +

            // ---------- (3) R106b: что получат НОВЫЕ модели и что применится к
            // каждой модели в папке. Раньше этот слой был доступен только на
            // странице «Настройки», поэтому на странице GGUF оператор правил
            // настройки контейнера и не видел, что при загрузке выигрывают
            // профиль модели и «настройки по умолчанию» — отсюда жалоба «модель
            // грузится не с теми параметрами, что указаны в настройках».
            '<div class="settings-subsection" id="gguf-load-defaults-section" style="margin-top:24px;padding-top:16px;border-top:1px solid var(--border-color, #2a3441);">' +
                '<h4 style="margin:0 0 6px 0;font-size:14px;display:flex;align-items:center;gap:8px;">' +
                    '<i class="fas fa-crown"></i> ' +
                    '<span>' + Utils.escapeHtml(_('gguf.load_defaults_title', 'Что получат НОВЫЕ модели (приоритетнее настроек контейнера)')) + '</span>' +
                '</h4>' +
                '<p style="color:var(--text-muted);margin:0 0 10px 0;font-size:12px;">' +
                    Utils.escapeHtml(_('gguf.load_defaults_desc',
                        'Порядок применения при загрузке: настройки этого слоя (и профиль конкретной модели) важнее значений из формы выше. Кнопка «Сделать приоритетом для новых моделей» переносит значения формы сюда.')) +
                '</p>' +
                '<div data-role="cpp-load-defaults">' +
                    '<div class="loading">' + Utils.escapeHtml(_('common.loading', 'Загрузка…')) + '</div>' +
                '</div>' +
                '<h4 style="margin:20px 0 6px 0;font-size:14px;display:flex;align-items:center;gap:8px;">' +
                    '<i class="fas fa-hard-drive"></i> ' +
                    '<span>' + Utils.escapeHtml(_('gguf.model_catalog_title', 'Модели в папке — что применится к каждой')) + '</span>' +
                '</h4>' +
                '<p style="color:var(--text-muted);margin:0 0 10px 0;font-size:12px;">' +
                    Utils.escapeHtml(_('gguf.model_catalog_desc',
                        'Включая ещё не загруженные. «Свой профиль» = у модели есть собственные настройки — они важнее и настроек по умолчанию, и настроек контейнера. Кнопка с ползунками открывает профиль этой модели (там же «Применить (reload)» для уже загруженной).')) +
                '</p>' +
                '<div data-role="cpp-model-catalog">' +
                    '<div class="loading">' + Utils.escapeHtml(_('common.loading', 'Загрузка…')) + '</div>' +
                '</div>' +
            '</div>' +

            '</div>';
    }

    // ---- Backend options load/save ----

    /**
     * Загрузить текущие llama.cpp-дефолты выбранного cppworker'а и отрендерить
     * редактируемую форму внутри #ggufBackendOptionsContainer.
     *
     * Round 32 #3 fix (2026-08-10): при успешной загрузке конфига — также
     * синхронизировать `state.loadOptions` с бэкенд-дефолтами. Без этого фикса
     * `loadOnSelectedBackend` использовал хардкод `state.loadOptions = {ctxSize:2048,
     * gpuLayers:-1, batchSize:512}` (см. state-initialization вверху файла),
     * НЕЗАВИСИМО от того, что показано в форме Settings. После Save пользователь
     * видел форму с 4096, но клик Load применял 2048 → путаница + silent override.
     *
     * Round 32 #5 fix (2026-08-10): добавлен elapsed-time в loading spinner
     * и более информативный error message. Без этого пользователь видел
     * "infinite loading" когда cppworker был недоступен (10s+ balancer retry
     * с backoff 1+2+4=7s + сам connect timeout 90s) — общий fetch timeout
     * webui (10s) не успевал abort'ить запрос, и спиннер крутился пока
     * balancer не возвращал 502 (10-12s). Теперь показывается "(прошло Xs)"
     * каждые 2 секунды + понятное сообщение об ошибке с подсказкой проверить
     * статус cppworker контейнера.
     */
    async function loadAndRenderBackendOptions() {
        const container = document.getElementById('ggufBackendOptionsContainer');
        if (!container) return;
        const backend = M.currentBackend();
        if (!backend) {
            container.innerHTML = '<div class="gguf-empty-state">' + (window.I18N ? I18N.t('gguf.no_backend_selected') : '') + '</div>';
            return;
        }
        const t0 = Date.now();
        // Spinner с elapsed-time обновлением каждые 2s. Если load >5s — пользователь
        // видит "Loading... (5s)" вместо "вечно крутящегося спиннера".
        let elapsedTimer = null;
        function updateSpinner() {
            const elapsed = ((Date.now() - t0) / 1000).toFixed(1);
            container.innerHTML = '<div class="loading"><i class="fas fa-spinner fa-spin"></i> ' +
                Utils.escapeHtml(_('common.loading', 'Loading…')) + ' (' + elapsed + 's)' +
                '<div style="margin-top:4px;font-size:11px;color:var(--text-muted);">' +
                Utils.escapeHtml(backend.id) +
                '</div></div>';
        }
        updateSpinner();
        elapsedTimer = setInterval(updateSpinner, 2000);
        // R60.5 (2026-09-07): Hard timeout 15s. requestViaBackend имеет 10s
        // AbortController, но если fetch зависает (например, не отвечает balancer
        // proxy), спиннер может крутиться вечно. Дополнительный внешний таймаут
        // гарантирует что пользователь увидит ошибку.
        const hardTimeout = setTimeout(() => {
            if (elapsedTimer) clearInterval(elapsedTimer);
            const elapsedSec = ((Date.now() - t0) / 1000).toFixed(1);
            container.innerHTML =
                '<div class="gguf-empty-state" style="color:var(--danger);padding:12px;">' +
                    '<div style="display:flex;align-items:center;gap:8px;margin-bottom:8px;">' +
                        '<i class="fas fa-exclamation-triangle"></i>' +
                        '<strong>' + Utils.escapeHtml(_('gguf.config_load_failed', 'Не удалось загрузить параметры бэкенда')) + '</strong>' +
                    '</div>' +
                    '<div style="font-family:monospace;font-size:11px;background:rgba(0,0,0,0.2);padding:8px;border-radius:4px;margin-bottom:8px;word-break:break-all;">' +
                        'R60.5 hard timeout 15s — fetch завис, проверьте balancer proxy' +
                    '</div>' +
                    '<div style="display:flex;gap:6px;">' +
                        '<button class="btn btn-secondary btn-sm" id="ggufBackendOptionsReload">' +
                            '<i class="fas fa-sync"></i> ' + Utils.escapeHtml(_('gguf.reload_backend_options')) +
                        '</button>' +
                        '<span style="font-size:11px;color:var(--text-muted);align-self:center;">' +
                            '(' + elapsedSec + 's)' +
                        '</span>' +
                    '</div>' +
                '</div>';
            const retry = document.getElementById('ggufBackendOptionsReload');
            if (retry) retry.addEventListener('click', loadAndRenderBackendOptions);
        }, 15000);
        try {
            const data = await GgufApi.requestViaBackend(backend.id, '/api/v1/cppworker/config');
            if (elapsedTimer) clearInterval(elapsedTimer);
            clearTimeout(hardTimeout);
            const cfg = (data && data.config) || {};
            // R125 (2026-10-16, пункт 3): какие узлы раскладки РЕАЛЬНО отвечают.
            // Отдельный запрос (проверка узлов — сетевой ввод-вывод с таймаутом) и
            // best-effort: без него секция честно скажет «не проверялось», а не
            // «донор не активен» из-за отсутствия данных.
            let rpcPeers = null;
            try {
                rpcPeers = await GgufApi.requestViaBackend(backend.id, '/api/v1/cppworker/rpc-peers');
            } catch (peersErr) {
                console.warn('[gguf-renderer] rpc-peers недоступны:', peersErr);
            }
            // === Round 32 #3: sync state.loadOptions с бэкенд-дефолтами ===
            if (!state._loadOptionsCustom) state._loadOptionsCustom = {};
            if (!state._loadOptionsCustom[backend.id]) {
                syncLoadOptionsFromConfig(cfg);
            }
            // Round 32 #6 (2026-08-10): кэшируем отрендеренный HTML формы, чтобы
            // он не затирался на spinner-плейсхолдер при последующих
            // refreshDetailPane() (active-queries polling каждые 3s).
            // R60.5 (2026-09-07): try/catch вокруг render — если Utils.escapeHtml
            // или другое выражение throws, показываем ошибку вместо вечного спиннера.
            let formHtml;
            try {
                formHtml = renderBackendOptionsForm(cfg, backend, data, rpcPeers);
            } catch (renderErr) {
                if (elapsedTimer) clearInterval(elapsedTimer);
                const elapsedSec = ((Date.now() - t0) / 1000).toFixed(1);
                const renderMsg = renderErr && renderErr.message ? renderErr.message : String(renderErr);
                console.error('[gguf-renderer] renderBackendOptionsForm failed:', renderErr);
                container.innerHTML =
                    '<div class="gguf-empty-state" style="color:var(--danger);padding:12px;">' +
                        '<div style="display:flex;align-items:center;gap:8px;margin-bottom:8px;">' +
                            '<i class="fas fa-exclamation-triangle"></i>' +
                            '<strong>Ошибка рендера формы (R60.5)</strong>' +
                        '</div>' +
                        '<div style="font-family:monospace;font-size:11px;background:rgba(0,0,0,0.2);padding:8px;border-radius:4px;margin-bottom:8px;word-break:break-all;">' +
                            Utils.escapeHtml(renderMsg) +
                        '</div>' +
                        '<div style="font-size:12px;color:var(--text-muted);margin-bottom:8px;">' +
                            'Config получен: ' + Object.keys(cfg).length + ' полей. См. console.error для stack trace.' +
                        '</div>' +
                        '<button class="btn btn-secondary btn-sm" id="ggufBackendOptionsReload">' +
                            '<i class="fas fa-sync"></i> Retry' +
                        '</button>' +
                    '</div>';
                const retry = document.getElementById('ggufBackendOptionsReload');
                if (retry) retry.addEventListener('click', loadAndRenderBackendOptions);
                return;
            }
            if (!state._settingsFormHtmlByBackend) state._settingsFormHtmlByBackend = {};
            state._settingsFormHtmlByBackend[backend.id] = { html: formHtml, cfg: cfg, ts: Date.now() };
            container.innerHTML = formHtml;
            // Round 35c+ (2026-08-13): init gpu_layers live badge (после render, чтобы
            // badge отражал текущее значение gpu, а не дефолт "AUTO" в HTML).
            if (Utils.updateGpuLayersBadge) {
                Utils.updateGpuLayersBadge('ggufOptGpuLayers', 'ggufOptGpuLayersBadge');
            }
            // Привязываем handlers
            const reloadBtn = container.querySelector('#ggufBackendOptionsReload');
            if (reloadBtn) reloadBtn.addEventListener('click', loadAndRenderBackendOptions);
            const saveBtn = container.querySelector('#ggufBackendOptionsSave');
            if (saveBtn) saveBtn.addEventListener('click', saveBackendOptions);
            // R106b (2026-10-12): перенос значений формы в приоритетный слой.
            const promoteBtn = container.querySelector('#ggufBackendOptionsPromote');
            if (promoteBtn) promoteBtn.addEventListener('click', promoteBackendOptionsToDefaults);
            const elapsedSec = ((Date.now() - t0) / 1000).toFixed(1);
            showToast(_('gguf.backend_options_loaded') + ' (' + elapsedSec + 's)', 'success');
        } catch (e) {
            if (elapsedTimer) clearInterval(elapsedTimer);
            clearTimeout(hardTimeout);
            const elapsedSec = ((Date.now() - t0) / 1000).toFixed(1);
            const errMsg = e && e.message ? e.message : String(e);
            // Расширенный error message: показываем что случилось + подсказку.
            // До этого фикса пользователь видел "aborted/timeout" без объяснений
            // и думал что это "infinite loading" (спиннер висел до timeout).
            container.innerHTML =
                '<div class="gguf-empty-state" style="color:var(--danger);padding:12px;">' +
                    '<div style="display:flex;align-items:center;gap:8px;margin-bottom:8px;">' +
                        '<i class="fas fa-exclamation-triangle"></i>' +
                        '<strong>' + Utils.escapeHtml(_('gguf.config_load_failed', 'Не удалось загрузить параметры бэкенда')) + '</strong>' +
                    '</div>' +
                    '<div style="font-family:monospace;font-size:11px;background:rgba(0,0,0,0.2);padding:8px;border-radius:4px;margin-bottom:8px;word-break:break-all;">' +
                        Utils.escapeHtml(errMsg) +
                    '</div>' +
                    '<div style="font-size:12px;color:var(--text-muted);margin-bottom:8px;">' +
                        Utils.escapeHtml(_('gguf.config_unavailable')) + '<br>' +
                        Utils.escapeHtml(_('gguf.config_check_cppworker', 'Проверьте: docker ps (cppworker запущен?), CppWorker endpoint доступен через balancer proxy.')) +
                    '</div>' +
                    '<div style="display:flex;gap:6px;">' +
                        '<button class="btn btn-secondary btn-sm" id="ggufBackendOptionsReload">' +
                            '<i class="fas fa-sync"></i> ' + Utils.escapeHtml(_('gguf.reload_backend_options')) +
                        '</button>' +
                        '<span style="font-size:11px;color:var(--text-muted);align-self:center;">' +
                            '(' + elapsedSec + 's)' +
                        '</span>' +
                    '</div>' +
                '</div>';
            const retry = document.getElementById('ggufBackendOptionsReload');
            if (retry) retry.addEventListener('click', loadAndRenderBackendOptions);
        }
    }

    /**
     * Round 32 #3 (2026-08-10): скопировать значения из бэкенд-конфига в
     * `state.loadOptions`, чтобы клик "Загрузить модель" использовал актуальные
     * дефолты, а не хардкод 2048/-1/512.
     *
     * Маппинг: cfg.defaultXxx → state.loadOptions.xxx (для loadOnSelectedBackend).
     * Только те поля, которые реально используются в loadOnSelectedBackend:
     *   gpuLayers, ctxSize, batchSize, flashAttn, numa, useMmap, autoGpuDistribution, tensorSplit.
     * Поля вроде defaultNuma/defaultUseMmap (bool) маппятся отдельно от flashAttn (int).
     */
    function syncLoadOptionsFromConfig(cfg) {
        if (!cfg) return;
        if (cfg.defaultGpuLayers !== undefined && cfg.defaultGpuLayers !== null) {
            state.loadOptions.gpuLayers = cfg.defaultGpuLayers;
        }
        if (cfg.defaultCtxSize !== undefined && cfg.defaultCtxSize !== null) {
            state.loadOptions.ctxSize = cfg.defaultCtxSize;
        }
        if (cfg.defaultBatchSize !== undefined && cfg.defaultBatchSize !== null) {
            state.loadOptions.batchSize = cfg.defaultBatchSize;
        }
        if (cfg.defaultFlashAttnType !== undefined && cfg.defaultFlashAttnType !== null) {
            // cppworker flash_attn_type: -1=auto, 0=disabled, 1=enabled.
            // webui loadOptions.flashAttn: bool. Конвертим: 0=false, ≠0=true.
            state.loadOptions.flashAttn = (cfg.defaultFlashAttnType !== 0);
        }
        if (cfg.defaultNuma !== undefined && cfg.defaultNuma !== null) {
            state.loadOptions.numa = !!cfg.defaultNuma;
        }
        if (cfg.defaultUseMmap !== undefined && cfg.defaultUseMmap !== null) {
            state.loadOptions.useMmap = !!cfg.defaultUseMmap;
        }
        if (cfg.autoGpuDistribution !== undefined && cfg.autoGpuDistribution !== null) {
            state.loadOptions.autoGpuDistribution = !!cfg.autoGpuDistribution;
        }
        if (Array.isArray(cfg.defaultTensorSplit) && cfg.defaultTensorSplit.length > 0) {
            state.loadOptions.tensorSplit = cfg.defaultTensorSplit.join(',');
        }
    }

    /**
     * Round 32 #3 (2026-08-10): пометить, что пользователь явно настроил
     * loadOptions через Settings.Save. После этого syncLoadOptionsFromConfig
     * не будет перезатирать пользовательские значения при следующем заходе.
     */
    function markLoadOptionsCustom(backendId) {
        if (!state._loadOptionsCustom) state._loadOptionsCustom = {};
        state._loadOptionsCustom[backendId] = true;
    }

    // renderRpcServerState — ЖИВОЕ состояние узла раскладки (R124, пункт 2).
    //
    // Показываем ФАКТ, а не галочку: процеcc запущен, порт слушается, какой pid,
    // сколько живёт, сколько было перезапусков и ПОЧЕМУ не поднялся. Нет данных
    // (старая сборка cppworker поля не отдаёт) — честно говорим «нет данных»:
    // рисовать «выключено» или «работает» вместо неизвестного нельзя, оператор
    // примет это за состояние.
    function renderRpcServerState(st) {
        if (!st || typeof st !== 'object') {
            return '<span class="gguf-rpc-state-unknown">' +
                Utils.escapeHtml(_('gguf.rpc_server_state_unknown',
                    'нет данных (воркер старой сборки не отдаёт состояние узла)')) + '</span>';
        }
        var bits = [];
        var dot = st.listening ? 'ok' : ((st.running || st.restarting) ? 'warn' : 'off');
        var label = st.listening
            ? _('gguf.rpc_server_running', 'слушает')
            : (st.running
                ? _('gguf.rpc_server_starting', 'порт открывается')
                : (st.restarting
                    ? _('gguf.rpc_server_restarting', 'узел падал, поднимается заново')
                    : _('gguf.rpc_server_stopped', 'остановлен')));
        bits.push('<span class="gguf-rpc-state-dot is-' + dot + '"></span>' +
            '<strong>' + Utils.escapeHtml(label) + '</strong>');
        if (st.addr) bits.push('<code>' + Utils.escapeHtml(st.addr) + '</code>');
        if (st.pid) bits.push('pid ' + Utils.escapeHtml(String(st.pid)));
        if (st.uptimeSec) {
            bits.push(Utils.escapeHtml(_('gguf.rpc_server_uptime', 'работает')) + ' ' +
                Utils.escapeHtml(formatElapsedShort(st.uptimeSec * 1000)));
        }
        if (st.restarts) bits.push(Utils.escapeHtml(_('gguf.rpc_server_restarts', 'перезапусков')) + ': ' + st.restarts);
        if (typeof st.restartInMs === 'number' && st.restartInMs >= 0) {
            bits.push(Utils.escapeHtml(_('gguf.rpc_server_restart_in', 'подъём через')) + ' ' +
                Math.max(1, Math.round(st.restartInMs / 1000)) + ' ' +
                Utils.escapeHtml(_('gguf.rpc_server_seconds', 'с')));
        }
        // R172: ЧУЖОЙ узел (отдельный контейнер) — это не «наш процесс»: пусть
        // оператор видит, что гасить его отсюда нельзя.
        if (st.external) bits.push(Utils.escapeHtml(_('gguf.rpc_server_external', 'внешний узел (управляет оператор)')));
        if (st.debug) bits.push(Utils.escapeHtml(_('gguf.rpc_server_debug_on', 'отладка протокола')));
        var html = '<div class="gguf-rpc-state-row">' + bits.join(' · ') + '</div>';
        // R172: ПРИЧИНА прошлой смерти. Клиент раскладки на любую неудачную
        // операцию пишет одно и то же «Remote RPC server crashed or returned
        // malformed response», поэтому exit-статус процесса — единственное, что
        // отличает «сервер разбился» от «сервер ответил false».
        if (st.lastExit) {
            var exitBits = [Utils.escapeHtml(String(st.lastExit))];
            if (st.lastExitPid) exitBits.push('pid ' + Utils.escapeHtml(String(st.lastExitPid)));
            if (st.lastExitAt) exitBits.push(Utils.escapeHtml(formatElapsedShort(Date.now() - Date.parse(st.lastExitAt))) + ' ' +
                Utils.escapeHtml(_('gguf.rpc_server_ago', 'назад')));
            html += '<div class="gguf-rpc-state-exit">' +
                Utils.escapeHtml(_('gguf.rpc_server_last_exit', 'прошлый процесс завершился')) + ': <code>' +
                exitBits.join(' · ') + '</code></div>';
        }
        if (st.lastError) {
            html += '<div class="gguf-rpc-state-error">' + Utils.escapeHtml(st.lastError) + '</div>';
        }
        if (Array.isArray(st.notes) && st.notes.length) {
            html += '<div class="gguf-rpc-state-note">' + Utils.escapeHtml(st.notes.join('; ')) + '</div>';
        }
        // R172: последние строки САМОГО rpc-server (GGML_LOG_ERROR сервера).
        // Свёрнуто: в обычном состоянии оператору они не нужны, а при аварии —
        // это единственное место, где причина названа прямо.
        if (Array.isArray(st.logTail) && st.logTail.length) {
            html += '<details class="gguf-rpc-state-log"><summary>' +
                Utils.escapeHtml(_('gguf.rpc_server_log_tail', 'последние строки узла')) +
                ' (' + st.logTail.length + ')</summary><pre>' +
                Utils.escapeHtml(st.logTail.join('\n')) + '</pre></details>';
        }
        if (st.logDropped) {
            html += '<div class="gguf-rpc-state-note">' +
                Utils.escapeHtml(_('gguf.rpc_server_log_dropped', 'в буфере только хвост лога')) +
                ': ' + st.logLines + ' / +' + st.logDropped + '</div>';
        }
        return html;
    }

    // MODEL_PARAM_SPECS — ЕДИНЫЙ реестр параметров модели (R134, 2026-10-16).
    //
    // ЗАЧЕМ РЕЕСТР. До R134 каждый параметр был описан своей разметкой, карточка
    // показывала 7 полей из 11, которые реально уезжают в загрузку, а имена полей
    // расходились (форма читала gpuLayers, значения приходили как numGpuLayers —
    // поле оставалось пустым). Список ниже — ЕДИНЫЙ источник для ДВУХ таблиц
    // (действующие значения и то, что можно задать) и для ОБОИХ мест: карточка
    // модели на диске и карточка загруженной модели. Иначе они снова разойдутся.
    //
    // ГРАНИЦА ДОСТОВЕРНОСТИ. Набор параметров — не выдумка UI, а контракт бэкенда:
    //   * internal/balancer/model_management.go: llamaCppLoadBodyKeys — что едет в
    //     теле загрузки;
    //   * internal/balancer/load_provenance_r119.go: trackedLoadParams — по каким из
    //     них бэкенд отдаёт ПРОИСХОЖДЕНИЕ значения;
    //   * pkg/types/balancing.go: LlamaCppModelProfile — что принимает профиль.
    //
    // field    — имя поля в профиле модели (PUT /api/v1/cppworker/model-profiles/{name});
    // loadKey  — как то же значение называется в теле загрузки;
    // factKeys — как это значение называет ВОРКЕР в /api/models. Читаем ФАКТ, а не
    //            догадку: именно на этом расхождении карточка показывала
    //            «размышления выключены» у модели, где они включены.
    const MODEL_PARAM_SPECS = [
        { field: 'contextLength', loadKey: 'contextSize', type: 'int', min: 256, max: 1048576, step: 256,
          label: 'gguf.ctx_size', hint: 'gguf.mp_ctx_hint', defaultKey: 'contextLength',
          factKeys: ['context_size', 'contextSize', 'contextLength'] },
        { field: 'contextPerSeq', loadKey: 'contextPerSeq', type: 'int', min: 0, max: 262144, step: 256,
          label: 'gguf.context_per_seq', hint: 'gguf.mp_ctx_seq_hint', defaultKey: 'contextPerSlot',
          factKeys: ['context_per_seq', 'contextPerSeq'] },
        { field: 'numGpuLayers', loadKey: 'gpuLayers', type: 'int', min: -2, max: 200, step: 1,
          label: 'gguf.gpu_layers', hint: 'gguf.mp_gpu_hint', defaultKey: 'numGpuLayers',
          factKeys: ['gpu_layers', 'gpuLayers', 'numGpuLayers'] },
        { field: 'batchSize', loadKey: 'batchSize', type: 'int', min: 1, max: 4096, step: 1,
          label: 'gguf.batch_size', hint: 'gguf.mp_batch_hint', defaultKey: 'batchSize',
          factKeys: ['batch_size', 'batchSize'] },
        { field: 'parallel', loadKey: 'parallel', type: 'int', min: 0, max: 32, step: 1,
          label: 'gguf.parallel', hint: 'gguf.mp_parallel_hint', defaultKey: 'parallel',
          factKeys: ['parallel', 'max_slots', 'maxSlots'] },
        { field: 'kvCacheType', loadKey: 'kvCacheType', type: 'enum', options: ['f16', 'q8_0', 'q4_0'],
          label: 'gguf.kv_cache_type', hint: 'gguf.mp_kv_hint', defaultKey: 'kvCacheType',
          factKeys: ['kv_cache_type', 'kvCacheType'] },
        { field: 'flashAttn', loadKey: 'flashAttn', type: 'bool',
          label: 'gguf.flash_attn', hint: 'gguf.mp_flash_hint', defaultKey: 'flashAttn',
          factKeys: ['flash_attn', 'flashAttn', 'flashAttnType'] },
        { field: 'useMmap', loadKey: 'useMmap', type: 'bool',
          label: 'gguf.use_mmap', hint: 'gguf.mp_mmap_hint', defaultKey: 'useMmap',
          factKeys: ['use_mmap', 'useMmap'] },
        { field: 'enableReasoning', loadKey: 'enableReasoning', type: 'bool',
          label: 'gguf.reasoning', hint: 'gguf.mp_reasoning_hint', defaultKey: 'enableReasoning',
          factKeys: ['reasoning_enabled', 'reasoningEnabled', 'reasoning', 'enableReasoning'] },
        // Дополнительно: per-tensor override для MoE — контракт загрузки их знает
        // (llamaCppLoadBodyKeys), происхождение по ним бэкенд не считает.
        { field: 'overrideTensors', loadKey: 'overrideTensors', type: 'list', advanced: true,
          label: 'gguf.override_tensors', hint: 'gguf.mp_override_hint', factKeys: [] },
        { field: 'overrideTensorBufts', loadKey: 'overrideTensorBufts', type: 'list', advanced: true,
          label: 'gguf.override_bufts', hint: 'gguf.mp_override_bufts_hint', factKeys: [] }
    ];


    // paramFact — что о параметре сообщает ВОРКЕР (факт загруженного инстанса).
    function paramFact(spec, m, rt) {
        m = m || {};
        rt = rt || {};
        var keys = spec.factKeys || [];
        for (var i = 0; i < keys.length; i++) {
            var k = keys[i];
            if (m[k] !== undefined && m[k] !== null && m[k] !== '') return m[k];
            if (rt[k] !== undefined && rt[k] !== null && rt[k] !== '') return rt[k];
        }
        return null;
    }

    // paramOrigin — значение и ИСТОЧНИК из происхождения параметров (R119/R134).
    function paramOrigin(spec, m, rt) {
        var prov = (m && m.parameters) || (rt && rt.parameters) || {};
        var keys = [spec.loadKey, spec.field].concat(spec.factKeys || []);
        for (var i = 0; i < keys.length; i++) {
            var entry = prov[keys[i]];
            if (entry && typeof entry === 'object') {
                return { value: entry.value, source: entry.source || '' };
            }
        }
        return null;
    }

    // loadedParamsFor — предварительные значения формы для ЗАГРУЖЕННОЙ модели (R126/R134).
    //
    // Приоритет: происхождение (кто поставил значение) → факт воркера. Профиль
    // подгружается отдельно (hydrateModelParamEditors) и, если он есть, заменяет
    // предзаполнение: форма редактирует ПРОФИЛЬ, а не текущий инстанс.
    function loadedParamsFor(m, rt) {
        m = m || {};
        rt = rt || {};
        var out = {};
        MODEL_PARAM_SPECS.forEach(function (spec) {
            var origin = paramOrigin(spec, m, rt);
            var value = (origin && origin.value !== undefined && origin.value !== null)
                ? origin.value
                : paramFact(spec, m, rt);
            if (value !== undefined && value !== null && value !== '') out[spec.field] = value;
        });
        // Совместимость с прежним именем поля (R126 отдавал gpuLayers): форма
        // теперь читает numGpuLayers, но старые вызовы не должны терять значение.
        if (out.gpuLayers !== undefined && out.numGpuLayers === undefined) {
            out.numGpuLayers = out.gpuLayers;
        }
        return out;
    }

    // formatParamValue — человекочитаемое значение для таблицы «действующие».
    function formatParamValue(spec, value) {
        if (value === null || value === undefined || value === '') return null;
        if (spec.type === 'bool') {
            // Flash Attention у воркера ТРЁХЗНАЧНЫЙ: -1 = авто, 0 = выкл, 1 = вкл.
            // Печатать -1 как «выключено» нельзя: оператор видит выключенным то,
            // что работает в автоматическом режиме (находка R136).
            if (spec.field === 'flashAttn' && typeof value === 'number') {
                if (value < 0) return _('gguf.mp_auto', 'авто');
                return value === 0 ? _('gguf.mp_off', 'выключено') : _('gguf.mp_on', 'включено');
            }
            var on = value === true || value === 'true' || value === 1 || value === '1';
            return on ? _('gguf.mp_on', 'включено') : _('gguf.mp_off', 'выключено');
        }
        if (spec.type === 'list') {
            return Array.isArray(value) ? value.join(', ') : String(value);
        }
        return String(value);
    }

    // paramInput — элемент управления для таблицы «можно задать».
    //
    // Пустое значение = «наследуется»: в профиль пустое поле не попадёт
    // (collectModelParams), поэтому форма не может молча отключить наследование.
    //
    // R134 (доп., 2026-10-16): БУЛЕВЫ ПАРАМЕТРЫ — ТРЁХЗНАЧНЫЕ. Раньше это была
    // галочка, а collectModelParams записывал `!!checked` ВСЕГДА — то есть
    // «Применить», не тронув ни одной галочки, клал в профиль flashAttn=false,
    // useMmap=false и enableReasoning=false. Следствие ровно то, на что жаловался
    // оператор: у модели с включёнными размышлениями после открытия карточки и
    // применения профиль начинал говорить «выключено», и строка модели расходилась
    // с карточкой. Теперь у bool три состояния: наследуется / включено / выключено.
    // ===== R229 (2026-10-22): «наследуется» показывает РЕАЛЬНОЕ значение =====
    //
    // ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: поле «наследуется» не говорило, ЧТО именно
    // унаследуется, и приходилось угадывать («в профиле пусто — наверное,
    // выключено»). Ответ у балансера уже есть: GET /api/v1/cppworker/load-defaults
    // отдаёт effective (что применится) и source (default_profile / env воркера).
    // Теперь метка наследования несёт фактическое значение.
    //
    // Правило преобразования — ОДНА функция на все три места показа (bool-селект,
    // enum-селект, placeholder числового поля): иначе «включено/1/on» разъедутся,
    // как это уже было с трёхзначными флажками в R134.
    function inheritedValueOf(spec, effective) {
        if (!spec || !effective) return null;
        var key = spec.defaultKey;
        if (!key || !Object.prototype.hasOwnProperty.call(effective, key)) return null;
        var raw = effective[key];
        if (raw === null || raw === undefined || raw === '') return null;
        if (spec.type === 'bool') {
            if (raw === true || raw === 1 || raw === '1' || raw === 'true') return true;
            if (raw === false || raw === -1 || raw === '0' || raw === 'false') return false;
            return null;
        }
        if (spec.type === 'int') {
            var n = Number(raw);
            return isFinite(n) ? n : null;
        }
        return String(raw);
    }

    // inheritedLabel — текст метки наследования. Если фактическое значение
    // неизвестно (воркер недоступен), остаётся честное «наследуется»: пустая
    // скобка вводила бы в заблуждение сильнее.
    function inheritedLabel(spec, effective) {
        var v = inheritedValueOf(spec, effective);
        if (v === null) return _('gguf.mp_inherit', 'наследуется');
        var shown = spec.type === 'bool'
            ? (v ? _('gguf.mp_on', 'включено') : _('gguf.mp_off', 'выключено'))
            : String(v);
        return _('gguf.mp_inherit', 'наследуется') + ' (' + shown + ')';
    }

    // applyInheritedLabels — обновить метки в уже отрисованной форме (значения по
    // умолчанию приходят отдельным запросом, после первой отрисовки).
    function applyInheritedLabels(editorEl, effective) {
        if (!editorEl || !effective || typeof editorEl.querySelector !== 'function') return;
        MODEL_PARAM_SPECS.forEach(function (spec) {
            var input = editorEl.querySelector('.gguf-mp-field[data-field="' + spec.field + '"]');
            if (!input) return;
            var text = inheritedLabel(spec, effective);
            if (input.options || input.tagName === 'SELECT') {
                var opt = (typeof input.querySelector === 'function')
                    ? input.querySelector('option[value=""]') : null;
                if (opt) opt.textContent = text;
            } else if ('placeholder' in input) {
                input.placeholder = text;
            }
        });
    }

    function paramInput(spec, idBase, value, effective) {
        var id = idBase + '_' + spec.field;
        var common = 'id="' + id + '" class="form-control form-control-sm gguf-mp-field" data-field="' + spec.field + '"';
        if (spec.type === 'bool') {
            var cur = (value === undefined || value === null || value === '') ? ''
                : ((value === true || value === 'true') ? 'true' : 'false');
            var boolOpts = [
                { v: '', t: inheritedLabel(spec, effective) },
                { v: 'true', t: _('gguf.mp_on', 'включено') },
                { v: 'false', t: _('gguf.mp_off', 'выключено') }
            ].map(function (o) {
                return '<option value="' + o.v + '"' + (o.v === cur ? ' selected' : '') + '>' +
                    Utils.escapeHtml(o.t) + '</option>';
            }).join('');
            return '<select ' + common + ' data-type="bool">' + boolOpts + '</select>';
        }
        if (spec.type === 'enum') {
            var cur = (value === undefined || value === null) ? '' : String(value);
            var opts = ['<option value=""' + (cur === '' ? ' selected' : '') + '>' +
                Utils.escapeHtml(inheritedLabel(spec, effective)) + '</option>'];
            (spec.options || []).forEach(function (o) {
                opts.push('<option value="' + Utils.escapeHtml(o) + '"' + (cur === o ? ' selected' : '') + '>' +
                    Utils.escapeHtml(o) + '</option>');
            });
            return '<select ' + common + '>' + opts.join('') + '</select>';
        }
        var v = (value === undefined || value === null) ? '' : String(value);
        if (spec.type === 'list') {
            return '<input type="text" ' + common + ' value="' + Utils.escapeHtml(v) +
                '" placeholder="' + Utils.escapeHtml(_('gguf.mp_list_placeholder', 'через запятую')) + '">';
        }
        return '<input type="number" ' + common + ' value="' + Utils.escapeHtml(v) +
            '" min="' + spec.min + '" max="' + spec.max + '" step="' + (spec.step || 1) +
            '" placeholder="' + Utils.escapeHtml(inheritedLabel(spec, effective)) + '">';
    }

    // renderModelParamsEditor — ДВЕ таблицы параметров модели (R134).
    //
    // ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «параметры разбросаны по области — неудобно, и если
    // их будет больше, воспринимать неудобно; лучше два списка-таблицы: активные и
    // отдельно те, что можно задать для загрузки/перезагрузки; и параметры должны
    // быть одинаковы и в едином стиле для моделей на диске и для загруженных».
    //
    //   Таблица 1 «Действующие значения» — ФАКТ: что реально у модели сейчас (от
    //     воркера) и кто это поставил (request / profile / default_profile /
    //     backend). Только чтение: правка идёт в профиль, а не в живой инстанс.
    //   Таблица 2 «Задать для загрузки / перезагрузки» — ПРОФИЛЬ: значения, которые
    //     применятся при следующей загрузке. Пусто = «наследуется».
    //
    // Один и тот же компонент рисуется и для модели на диске, и для загруженной:
    // разный только набор известных фактов (у модели на диске их нет).
    function renderModelParamsEditor(modelName, values, opts) {
        values = values || {};
        opts = opts || {};
        var activeSource = isLoaded;
        var safeName = String(modelName == null ? '' : modelName);
        var idBase = 'ggufMp_' + safeName.replace(/[^A-Za-z0-9_.-]/g, '_');
        var isLoaded = opts.source === 'loaded';
        var model = opts.model || {};
        var runtime = opts.runtime || {};
        // R229: значения по умолчанию («что унаследует пустое поле») — карта effective
        // из GET /api/v1/cppworker/load-defaults (обёртка defaults.effective).
        var defaultsEffective = (opts.defaults && opts.defaults.effective) || null;

        var activeSource = isLoaded;
        var rowsHtml = function (specs, active) {
            return specs.map(function (spec) {
                var label = Utils.escapeHtml(_(spec.label, spec.field));
                var hint = spec.hint
                    ? '<div class="gguf-mp-hint">' + Utils.escapeHtml(_(spec.hint, '')) + '</div>'
                    : '';
                if (active) {
                    var fact = paramFact(spec, model, runtime);
                    var origin = paramOrigin(spec, model, runtime);
                    var shown = formatParamValue(spec, fact !== null ? fact : (origin ? origin.value : null));
                    // Подписи источников — те же, что в блоке происхождения R119
                    // (PARAM_SOURCE_LABELS): один словарь на страницу, без второй копии.
                    var src = (origin && PARAM_SOURCE_LABELS[origin.source]) || _('gguf.mp_src_unknown', '—');
                    return '<tr>' +
                        '<td>' + label + hint + '</td>' +
                        '<td class="gguf-mp-val">' + (shown === null
                            ? '<span class="gguf-mp-none">' + Utils.escapeHtml(_('gguf.mp_unknown', '—')) + '</span>'
                            : Utils.escapeHtml(shown)) + '</td>' +
                        '<td class="gguf-mp-src">' + Utils.escapeHtml(src) + '</td>' +
                    '</tr>';
                }
                // ФАКТ РЯДОМ С ФОРМОЙ. Форма редактирует ПРОФИЛЬ, и до R136 оператор
                // видел только его: «в профиле не задано» читалось как «выключено»,
                // хотя у загруженной модели размышления работали. Теперь под контролом
                // видно, что действует СЕЙЧАС (источник — факт воркера).
                var nowHint = '';
                if (activeSource && spec.type !== 'list') {
                    var nowVal = formatParamValue(spec, paramFact(spec, model, runtime));
                    if (nowVal !== null) {
                        nowHint = '<div class="gguf-mp-fact">' +
                            Utils.escapeHtml(_('gguf.mp_now', 'сейчас')) + ': ' + Utils.escapeHtml(nowVal) + '</div>';
                    }
                }
                return '<tr' + (spec.advanced ? ' class="gguf-mp-adv-row"' : '') + '>' +
                    '<td>' + label + hint + '</td>' +
                    '<td class="gguf-mp-profile" data-profile-field="' + spec.field + '">' +
                        '<span class="gguf-mp-none">' + Utils.escapeHtml(_('gguf.mp_not_in_profile', '— не задано —')) + '</span>' +
                    '</td>' +
                    '<td class="gguf-mp-input">' + paramInput(spec, idBase, values[spec.field], defaultsEffective) + nowHint + '</td>' +
                '</tr>';
            }).join('');
        };

        var mainSpecs = MODEL_PARAM_SPECS.filter(function (s) { return !s.advanced; });
        var advSpecs = MODEL_PARAM_SPECS.filter(function (s) { return s.advanced; });
        var activeHint = isLoaded
            ? _('gguf.mp_active_hint_loaded', 'Что реально действует у загруженной модели и кто это поставил.')
            : _('gguf.mp_active_hint_disk', 'Модель не загружена — действующих значений нет. Ниже задайте параметры для следующей загрузки.');

        return '<div class="gguf-mp-editor" data-model="' + Utils.escapeHtml(safeName) + '" data-source="' + (isLoaded ? 'loaded' : 'disk') + '">' +
            '<div class="gguf-mp-block">' +
                '<div class="gguf-mp-title">' + Utils.escapeHtml(_('gguf.mp_table_active', 'Действующие значения')) +
                    '<span class="gguf-mp-sub">' + Utils.escapeHtml(activeHint) + '</span></div>' +
                '<table class="gguf-mp-table"><thead><tr>' +
                    '<th>' + Utils.escapeHtml(_('gguf.mp_col_param', 'Параметр')) + '</th>' +
                    '<th>' + Utils.escapeHtml(_('gguf.mp_col_value', 'Значение')) + '</th>' +
                    '<th>' + Utils.escapeHtml(_('gguf.mp_col_source', 'Источник')) + '</th>' +
                '</tr></thead><tbody>' + rowsHtml(mainSpecs, true) + '</tbody></table>' +
            '</div>' +
            '<div class="gguf-mp-block">' +
                '<div class="gguf-mp-title">' + Utils.escapeHtml(_('gguf.mp_table_set', 'Задать для загрузки / перезагрузки')) +
                    '<span class="gguf-mp-sub">' + Utils.escapeHtml(_('gguf.mp_set_hint',
                        'Пустое поле = наследуется (профиль модели → настройки по умолчанию → окружение контейнера). Применение перезагрузит модель.')) + '</span></div>' +
                '<table class="gguf-mp-table"><thead><tr>' +
                    '<th>' + Utils.escapeHtml(_('gguf.mp_col_param', 'Параметр')) + '</th>' +
                    '<th>' + Utils.escapeHtml(_('gguf.mp_col_profile', 'В профиле')) + '</th>' +
                    '<th>' + Utils.escapeHtml(_('gguf.mp_col_set', 'Задать')) + '</th>' +
                '</tr></thead><tbody>' + rowsHtml(mainSpecs, false) +
                '<tr class="gguf-mp-adv-head"><td colspan="3">' +
                    Utils.escapeHtml(_('gguf.mp_advanced', 'Дополнительно: per-tensor override для MoE-моделей')) + '</td></tr>' +
                rowsHtml(advSpecs, false) + '</tbody></table>' +
            '</div>' +
            '<div class="gguf-mp-actions">' +
                '<button class="btn btn-primary btn-sm gguf-mp-apply" data-admin-only data-model-params-apply="' + Utils.escapeHtml(safeName) + '">' +
                    '<i class="fas fa-rotate"></i> ' + Utils.escapeHtml(_('gguf.mp_apply', 'Применить и перезагрузить')) +
                '</button>' +
                '<small class="form-text gguf-mp-warn">' + Utils.escapeHtml(_('gguf.mp_apply_hint',
                    'Применение сохранит профиль модели и перезагрузит её: текущая сессия прервётся, загрузка займёт время.')) + '</small>' +
            '</div>' +
        '</div>';
    }

    // hydrateModelParamEditors — подставить в формы значения ПРОФИЛЯ (R134).
    //
    // ЗАЧЕМ ОТДЕЛЬНЫМ ШАГОМ. Карточка рисуется синхронно, а профиль приходит
    // запросом. Из-за этого модель на диске открывалась с пустыми полями
    // («не задано»), хотя значения в профиле были, — и оператор не мог понять, что
    // же применится при загрузке. Профили запрашиваются ОДНИМ списком и раздаются
    // всем формам на странице.
    function hydrateModelParamEditors() {
        if (typeof document === 'undefined' || !document.querySelectorAll) return;
        var editors = document.querySelectorAll('.gguf-mp-editor[data-model]:not([data-hydrated="1"])');
        if (!editors || !editors.length) return;
        var api = (typeof window !== 'undefined') && (window.Api || window.BalancerApi);
        if (!api || !api.cppworkerModelProfiles || typeof api.cppworkerModelProfiles.list !== 'function') return;
        Array.prototype.forEach.call(editors, function (el) { el.setAttribute('data-hydrated', '1'); });
        api.cppworkerModelProfiles.list().then(function (res) {
            var profiles = (res && (res.models || res.profiles)) || {};
            // R162 (2026-10-17): кладём профили в state — это ЕДИНСТВЕННЫЙ
            // источник НАМЕРЕНИЯ ОПЕРАТОРА (contextLength и т.п.) для чипов
            // карточки загруженной модели. Без него R155 («окно ПОДНЯТО по
            // запросу клиента») не срабатывал вообще: сравнивать фактическое окно
            // инстанса было не с чем, и стрелка не появлялась никогда.
            if (M.state && typeof M.state === 'object') M.state.modelProfiles = profiles;
            Array.prototype.forEach.call(editors, function (el) {
                fillModelParamEditor(el, profiles[el.getAttribute('data-model')] || null);
            });
            // R229 (2026-10-22): подписать «наследуется» РЕАЛЬНЫМ значением.
            applyInheritedDefaults(editors);
        }).catch(function () {
            // Профили не получены: формы остаются с тем, что уже известно.
        });
    }

    // applyInheritedDefaults — R229: значения по умолчанию (что именно унаследует
    // пустое поле) приходят отдельным запросом GET /api/v1/cppworker/load-defaults:
    // effective — фактические значения, source — кто их поставил (default_profile
    // или env воркера). Кэшируем в состоянии страницы: один запрос на все открытые
    // формы. Недоступность не критична — остаётся честное «наследуется» без скобки.
    function applyInheritedDefaults(editors) {
        if (!editors || !editors.length) return;
        var api = (typeof window !== 'undefined') && (window.Api || window.BalancerApi);
        var cached = (M.state && M.state.loadDefaults) || null;
        var paint = function (d) {
            var effective = d && d.effective ? d.effective : null;
            if (!effective) return;
            Array.prototype.forEach.call(editors, function (el) {
                applyInheritedLabels(el, effective);
            });
        };
        if (cached && cached.effective) {
            paint(cached);
            return;
        }
        if (!api || !api.cppworkerLoadDefaults || typeof api.cppworkerLoadDefaults.get !== 'function') return;
        api.cppworkerLoadDefaults.get().then(function (d) {
            if (M.state && typeof M.state === 'object') M.state.loadDefaults = d || null;
            paint(d);
        }).catch(function () {
            // Значения по умолчанию не получены — метки остаются без значений.
        });
    }

    // hydrateOnOpen — подтянуть профиль при раскрытии карточки параметров.
    //
    // Значения профиля могли измениться (их правит и эта же форма), поэтому при
    // каждом раскрытии снимаем отметку «уже подставлено» и запрашиваем список
    // заново: список профилей — один недорогой запрос на все открытые формы.
    function hydrateOnOpen(target) {
        var box = (target && target.closest) ? target.closest('.gguf-mp-acc') : null;
        var scope = box || (typeof document !== 'undefined' ? document : null);
        if (scope && scope.querySelectorAll) {
            var editors = scope.querySelectorAll('.gguf-mp-editor[data-model]');
            Array.prototype.forEach.call(editors, function (el) { el.removeAttribute('data-hydrated'); });
        }
        hydrateModelParamEditors();
    }

    // fillModelParamEditor — разложить значения профиля по форме и по колонке
    // «В профиле». Пустое значение профиля остаётся «не задано» (наследуется).
    function fillModelParamEditor(editorEl, profile) {
        if (!editorEl) return;
        profile = profile || {};
        MODEL_PARAM_SPECS.forEach(function (spec) {
            var raw = profile[spec.field];
            var cell = editorEl.querySelector('[data-profile-field="' + spec.field + '"]');
            var input = editorEl.querySelector('.gguf-mp-field[data-field="' + spec.field + '"]');
            var known = !(raw === undefined || raw === null || raw === '');
            if (cell) {
                cell.innerHTML = known
                    ? Utils.escapeHtml(formatParamValue(spec, raw) || String(raw))
                    : '<span class="gguf-mp-none">' + Utils.escapeHtml(_('gguf.mp_not_in_profile', '— не задано —')) + '</span>';
            }
            if (!input) return;
            if (input.type === 'checkbox') {
                input.checked = known && (raw === true || raw === 'true');
            } else if (known) {
                input.value = Array.isArray(raw) ? raw.join(', ') : String(raw);
            } else {
                input.value = '';
            }
        });
    }

    // collectModelParams — собрать значения ИЗ формы редактора (R126).
    //
    // Пустое поле = «не задано» и в профиль НЕ попадает: иначе мы записали бы в
    // профиль модели пустоту и отключили наследование (профиль → настройки по
    // умолчанию → env), то есть «применить» ухудшило бы конфигурацию.
    function collectModelParams(modelName) {
        var root = document.querySelector('.gguf-mp-editor[data-model="' + String(modelName).replace(/"/g, '\\"') + '"]');
        if (!root) return null;
        var out = {};
        var fields = root.querySelectorAll('.gguf-mp-field');
        for (var i = 0; i < fields.length; i++) {
            var el = fields[i];
            var field = el.getAttribute('data-field');
            if (!field) continue;
            // R134 (доп.): bool — ТРЁХЗНАЧНЫЙ выбор ("" / "true" / "false").
            // Пустое значение означает «наследуется» и в профиль НЕ попадает:
            // иначе применение формы молча выключало бы параметр, которого
            // оператор не касался (живой класс жалобы «в карточке выключено,
            // хотя модель работает с включённым»).
            var dtype = el.getAttribute('data-type') || '';
            if (dtype === 'bool') {
                var braw = String(el.value || '').trim();
                if (braw === '') continue;
                out[field] = (braw === 'true');
                continue;
            }
            if (el.type === 'checkbox') {
                // Совместимость: старые чекбоксы (если где-то остались) собираем
                // только отмеченными — «не отмечено» больше не значит «выключить».
                if (el.checked) out[field] = true;
                continue;
            }
            var raw = String(el.value || '').trim();
            if (raw === '') continue; // не задано → не фиксируем
            // Строковые значения (enum) определяем по data-type, а НЕ по tagName:
            // иначе любой select сохранялся бы строкой, в том числе числовой.
            if (dtype === 'enum' || field === 'kvCacheType') {
                out[field] = raw;
            } else {
                var num = Number(raw);
                if (Number.isFinite(num)) out[field] = num;
            }
        }
        return out;
    }

    // applyModelParams — сохранить профиль модели и перезагрузить её (R126).
    //
    // Порядок ровно как у существующего пути применения профилей:
    //   PUT  .../model-profiles/{name}       — сохранить значения
    //   POST .../model-profiles/{name}/apply — save+reload (с прогрессом)
    // Второй шаг делегируем window.CppWorkerParams.applyProfileWithProgress, если
    // он доступен: там уже есть модалка прогресса и SSE-обновления. Если модуль
    // не загружен — говорим об этом честно, а не делаем вид, что применили.
    async function applyModelParams(modelName) {
        var backend = M.currentBackend();
        if (!backend) {
            showToast(_('gguf.mp_no_backend', 'Бэкенд не выбран'), 'error');
            return false;
        }
        var body = collectModelParams(modelName);
        if (body === null) {
            showToast(_('gguf.mp_no_editor', 'Форма параметров модели не найдена'), 'error');
            return false;
        }
        try {
            // ПРОФИЛИ МОДЕЛЕЙ ЖИВУТ НА БАЛАНСЕРЕ, а не в воркере: живая проверка
            // показала 404 на прокси-путь через бэкенд (cppworker такой ручки не
            // имеет вовсе), а на балансере PUT /api/v1/cppworker/model-profiles/{name}
            // отвечает 200. Поэтому используем документированный модуль
            // Api.cppworkerModelProfiles (webui/js/modules/api.js).
            if (!(window.Api && window.Api.cppworkerModelProfiles &&
                  typeof window.Api.cppworkerModelProfiles.upsert === 'function')) {
                showToast(_('gguf.mp_api_missing',
                    'Модуль профилей моделей не загружен — сохранить параметры нельзя'), 'error');
                return false;
            }
            await window.Api.cppworkerModelProfiles.upsert(modelName, body);
        } catch (e) {
            showToast(_('gguf.mp_save_failed', 'Не удалось сохранить профиль') + ': ' +
                (e && e.message ? e.message : e), 'error');
            return false;
        }
        if (window.CppWorkerParams && typeof window.CppWorkerParams.applyProfileWithProgress === 'function') {
            // applyProfileWithProgress сам показывает прогресс и результат перезагрузки.
            // R228: применяем ТОЛЬКО к выбранному бэкенду, а не ко всем, где модель загружена.
            await window.CppWorkerParams.applyProfileWithProgress(modelName, backend.id);
            return true;
        }
        showToast(_('gguf.mp_apply_manual',
            'Профиль сохранён. Применение с перезагрузкой недоступно: модуль прогресса не загружен — перезагрузите модель вручную.'),
            'warning');
        return false;
    }

    // collapseSections — превратить плоскую форму в КОМПАКТНЫЕ раскрываемые секции
    // (R124, пункт 4 разбора стенда).
    //
    // ЗАЧЕМ ПОСТОБРАБОТКА, А НЕ ПЕРЕПИСЫВАНИЕ ФОРМЫ. Форма — это ~500 строк
    // конкатенации; переписывание в массив секций трогает каждый input и рискует
    // сломать обработчики. Здесь же HTML уже собран: мы находим заголовки секций
    // (`h6.gguf-section-header`), которые и так размечают группы, и заворачиваем
    // каждую группу в <details>. Значения, id полей и обработчики не меняются —
    // меняется только представление.
    //
    // Первая секция раскрыта (оператор чаще всего правит именно её), остальные
    // свёрнуты: страница перестаёт быть простынёй, которую надо пролистывать.
    function collapseSections(html) {
        if (typeof html !== 'string' || html === '') return html || '';
        var marker = '<h6 class="gguf-section-header">';
        var parts = html.split(marker);
        if (parts.length < 2) return html; // заголовков нет — не трогаем
        var head = parts[0];
        var out = head;
        for (var i = 1; i < parts.length; i++) {
            var chunk = parts[i];
            // Заголовок отделяем от тела секции: он заканчивается первым </h6>.
            var close = chunk.indexOf('</h6>');
            if (close < 0) {
                // Битый заголовок — возвращаем как есть, не теряя содержимое.
                out += marker + chunk;
                continue;
            }
            var headerInner = chunk.slice(0, close);
            var body = chunk.slice(close + '</h6>'.length);
            // data-acc нужен тестам и стилям; первый блок открыт.
            var isFirst = i === 1;
            out += '<details class="gguf-acc"' + (isFirst ? ' open' : '') + ' data-acc="' + i + '">' +
                '<summary class="gguf-acc-summary">' +
                    '<i class="fas fa-chevron-right gguf-acc-caret"></i>' +
                    headerInner +
                '</summary>' +
                '<div class="gguf-acc-body">' + body + '</div>' +
                '</details>';
        }
        return out;
    }

    // renderRpcPeers — какие узлы раскладки РЕАЛЬНО отвечают (R125, пункт 3).
    //
    // Живая жалоба оператора: «нехватает метрик для того чтобы понимать какой
    // донор активен». Список узлов как текст этого не показывает: строка в
    // textarea остаётся и когда узел лежит.
    //
    // ЧЕСТНО О ТОМ, ЧЕГО ЗДЕСЬ НЕТ. Показываются доступность (TCP) и задержка
    // соединения. Пропускная способность не измеряется: у rpc-server нет ни
    // ручки здоровья, ни эхо-протокола, и выдавать задержку за «скорость обмена»
    // нельзя. Нет ответа пробника — «не проверялось», а не «не активен».
    function renderRpcPeers(peers) {
        if (!peers || typeof peers !== 'object') {
            return '<small class="form-text gguf-rpc-peers-unknown">' +
                Utils.escapeHtml(_('gguf.rpc_peers_not_checked', 'узлы не проверялись')) + '</small>';
        }
        var list = Array.isArray(peers.servers) ? peers.servers : [];
        if (list.length === 0) {
            return '<small class="form-text">' +
                Utils.escapeHtml(_('gguf.rpc_peers_none',
                    'раскладка не настроена — узлов нет')) + '</small>';
        }
        var rows = list.map(function (p) {
            var ok = !!p.reachable;
            var bits = '<span class="gguf-rpc-state-dot is-' + (ok ? 'ok' : 'off') + '"></span>' +
                '<code>' + Utils.escapeHtml(p.addr || '') + '</code>';
            if (ok) {
                bits += '<span>' + Utils.escapeHtml(_('gguf.rpc_peer_active', 'активен')) + '</span>';
                if (p.latencyMs !== undefined && p.latencyMs !== null) {
                    bits += '<span class="gguf-rpc-peer-latency">' + Utils.escapeHtml(String(p.latencyMs)) + ' ms</span>';
                }
            } else {
                bits += '<span>' + Utils.escapeHtml(_('gguf.rpc_peer_inactive', 'не отвечает')) + '</span>';
                if (p.error) {
                    bits += '<span class="gguf-rpc-peer-error">' + Utils.escapeHtml(p.error) + '</span>';
                }
            }
            return '<div class="gguf-rpc-peer-row">' + bits + '</div>';
        }).join('');
        var summary = '<div class="gguf-rpc-peers-summary">' +
            Utils.escapeHtml(_('gguf.rpc_peers_active', 'активных узлов')) + ': ' +
            Utils.escapeHtml(String(peers.reachable || 0)) + ' / ' + Utils.escapeHtml(String(peers.total || list.length)) +
            '</div>';
        // R129 (пункт 3): измеренная пропускная способность до узлов — только если
        // замер РЕАЛЬНО выполнялся (?bench). Отказ замера показываем причиной:
        // «0 МБ/с» означало бы измеренную нулевую скорость, а это не то же самое.
        var speedRows = '';
        var speed = Array.isArray(peers.linkSpeed) ? peers.linkSpeed : [];
        speed.forEach(function (s) {
            if (s.error) {
                speedRows += '<div class="gguf-rpc-peer-row gguf-rpc-peer-speed">' +
                    '<code>' + Utils.escapeHtml(s.endpoint || '') + '</code>' +
                    '<span class="gguf-rpc-peer-error">' +
                    Utils.escapeHtml(_('gguf.link_speed_failed', 'замер не удался')) + ': ' +
                    Utils.escapeHtml(s.error) + '</span></div>';
                return;
            }
            speedRows += '<div class="gguf-rpc-peer-row gguf-rpc-peer-speed">' +
                '<i class="fas fa-right-left"></i>' +
                '<strong>' + Utils.escapeHtml(String(s.mbps)) + ' ' +
                Utils.escapeHtml(_('gguf.link_speed_unit', 'МБ/с')) + '</strong>' +
                '<span class="gguf-rpc-peer-latency">' +
                Utils.escapeHtml(s.endpoint || '') + ' · ' +
                Utils.escapeHtml(String(s.mb)) + ' ' + Utils.escapeHtml(_('gguf.link_speed_mb', 'МБ за')) + ' ' +
                Utils.escapeHtml(String(s.durationMs)) + ' ms</span></div>';
        });
        if (speedRows) {
            speedRows = '<div class="gguf-rpc-peers-summary">' +
                Utils.escapeHtml(_('gguf.link_speed_title', 'Скорость обмена между машинами (TCP)')) +
                '</div>' + speedRows;
        }
        // R129: замер дорогой (десятки МБ), поэтому только по кнопке.
        var benchBtn = speedRows ? '' :
            '<button class="btn btn-secondary btn-sm gguf-rpc-bench" data-rpc-bench="1" style="margin-top:6px;">' +
                '<i class="fas fa-gauge-high"></i> ' +
                Utils.escapeHtml(_('gguf.link_speed_measure', 'Измерить скорость обмена')) +
            '</button>';
        return summary + rows + speedRows + benchBtn;
    }

    // measureLinkSpeed — R129 (пункт 3): запросить замер до узлов и обновить блок.
    //
    // Замер выполняет ВОРКЕР того бэкенда, чью форму открыли: он измеряет путь до
    // узлов раскладки своими средствами (HTTP-приёмник на узле). UI лишь просит и
    // показывает измеренное — считать «на глазок» здесь нечего.
    async function measureLinkSpeed() {
        var backend = M.currentBackend();
        var host = document.getElementById('ggufOptRpcPeers');
        if (!backend || !host) return false;
        host.innerHTML = '<small class="form-text"><i class="fas fa-spinner fa-spin"></i> ' +
            Utils.escapeHtml(_('gguf.link_speed_measuring', 'Измеряю скорость обмена…')) + '</small>';
        try {
            var res = await GgufApi.requestViaBackend(backend.id, '/api/v1/cppworker/rpc-peers?bench=1');
            host.innerHTML = renderRpcPeers(res);
            return true;
        } catch (e) {
            host.innerHTML = '<small class="form-text gguf-rpc-peer-error">' +
                Utils.escapeHtml(_('gguf.link_speed_failed', 'замер не удался')) + ': ' +
                Utils.escapeHtml(e && e.message ? e.message : String(e)) + '</small>';
            return false;
        }
    }

    function renderBackendOptionsForm(cfg, backend, resp, peers) {
        // Маппинг полей cppbackend.Config ↔ UI. Полное покрытие (~25 полей),
        // сгруппировано по секциям: General / Multi-GPU / KV-cache / RoPE-YaRN / Perf.
        //
        // Все inputs имеют префикс "ggufOpt" для удобства findElementById.
        const v = function (key, fallback) {
            return (cfg[key] !== undefined && cfg[key] !== null) ? cfg[key] : fallback;
        };
        const chk = function (key, fallback) {
            if (cfg[key] === undefined || cfg[key] === null) return !!fallback;
            return !!cfg[key];
        };
        const ctx = v('defaultCtxSize', 4096);
        const batch = v('defaultBatchSize', 512);
        const gpu = v('defaultGpuLayers', -1);
        const fa = v('defaultFlashAttnType', -1);
        const numa = chk('defaultNuma', false);
        const mmap = chk('defaultUseMmap', true);
        const mlock = chk('defaultUseMlock', false);
        const threads = v('defaultNThreads', 0);
        const rmsNormEps = v('defaultRmsNormEps', 0.00001);
        // Multi-GPU
        const autoGpu = chk('autoGpuDistribution', true);
        const splitMode = v('defaultSplitMode', -1);
        const mainGpu = v('defaultMainGpu', 0);
        const rpcBackend = v('defaultRpcBackend', '') || '';
        const noMemoryMap = chk('defaultNoMemoryMap', false);
        // R115: раскладка слоёв между машинами (llama.cpp RPC). До R115 эти
        // значения жили только в переменных окружения контейнера, и WebUI их не
        // показывал вовсе — оператор не видел ни режима, ни списка узлов.
        const rpcEnabled = chk('rpcEnabled', false);
        const rpcServersVal = Array.isArray(cfg.rpcServers) ? cfg.rpcServers.join('\n') : '';
        // R124 (2026-10-16, пункт 2): узел раскладки внутри ЭТОГО cppworker.
        // rpcServerState приходит в ответе GET /config отдельным полем (не в config):
        // это ФАКТ о процессе (running/listening/pid/причина), а не настройка.
        const rpcServerEnabled = chk('rpcServerEnabled', false);
        const rpcServerPort = v('rpcServerPort', 50052);
        const rpcServerStateHtml = renderRpcServerState(resp && resp.rpcServer);
        // Tensor split: из бэка приходит []float32.
        let tensorSplitVal = '';
        if (cfg.defaultTensorSplit && Array.isArray(cfg.defaultTensorSplit)) {
            tensorSplitVal = cfg.defaultTensorSplit.join(',');
        }
        // KV cache
        const kvCacheType = v('defaultKvCacheType', '') || '';
        const noKvOffload = chk('defaultNoKvOffload', false);
        // RoPE/YaRN
        const ropeFreqBase = v('defaultRopeFreqBase', 10000.0);
        const ropeFreqScale = v('defaultRopeFreqScale', 1.0);
        const ropeScalingType = v('defaultRopeScalingType', 'none') || 'none';
        const ropeScalingFactor = v('defaultRopeScalingFactor', 1.0);
        const yarnExtFactor = v('defaultYarnExtFactor', 1.0);
        const yarnAttnFactor = v('defaultYarnAttnFactor', 1.0);
        const yarnBetaFast = v('defaultYarnBetaFast', 32.0);
        const yarnBetaSlow = v('defaultYarnBetaSlow', 1.0);
        // Performance / lifecycle
        const idleUnload = v('idleUnloadMinutes', 0);
        const enableMetrics = chk('enableMetrics', true);
        const metricsRetention = v('metricsRetentionSeconds', 3600);
        // Session 18 Round 15 (2026-07-29): Inference defaults — параллелизм и reasoning.
        // defaultNParallel: 0 = inherit cppworker default (1), 1..8 = parallel slots per model.
        // enableReasoning: включает soft+native thinking path (Round 11/14).
        //   cppworker handler принимает ключ "enableReasoning" (без "default" префикса),
        //   см. handlers_config.go:387 applyBool("enableReasoning", ...).
        // reasoningBudget: max thinking tokens before answer (0 = unlimited).
        const nParallel = v('defaultNParallel', 0);
        const enableReasoning = chk('enableReasoning', false);
        const reasoningBudget = v('reasoningBudget', 0);
        // Diagnostics
        const nodeName = (cfg.nodeName != null) ? cfg.nodeName : (backend ? backend.id : '');
        const balancerUrl = (cfg.balancerUrl != null) ? cfg.balancerUrl : '';
        const uptime = (cfg.uptime != null) ? cfg.uptime : '';
        var formHtml = '' +
            // ====== General ======
            '<h6 class="gguf-section-header"><i class="fas fa-sliders-h"></i> ' + Utils.escapeHtml(_('gguf.tab_general') || 'General') + '</h6>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.ctx_size')) + '</label>' +
                    '<input type="number" id="ggufOptCtxSize" class="form-control" value="' + ctx + '" min="256" max="262144" step="64">' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.ctx_size_desc')) + '</small>' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.batch_size')) + '</label>' +
                    '<input type="number" id="ggufOptBatchSize" class="form-control" value="' + batch + '" min="1" max="4096">' +
                '</div>' +
            '</div>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.gpu_layers')) +
                        ' <span id="ggufOptGpuLayersBadge" class="gpu-layers-badge is-auto">AUTO</span>' +
                    '</label>' +
                    '<input type="number" id="ggufOptGpuLayers" class="form-control" value="' + gpu + '" min="-2" max="200" ' +
                        'oninput="Utils.updateGpuLayersBadge(\'ggufOptGpuLayers\',\'ggufOptGpuLayersBadge\')">' +
                    '<div class="gpu-layers-quick-row">' +
                        '<button type="button" class="btn btn-secondary" data-gpu-value="-2" ' +
                            'onclick="Utils.setGpuLayers(-2,\'ggufOptGpuLayers\',\'ggufOptGpuLayersBadge\')">' +
                            Utils.escapeHtml(_('gguf.gpu_layers_btn_auto', 'AUTO (-2)')) + '</button>' +
                        '<button type="button" class="btn btn-secondary" data-gpu-value="-1" ' +
                            'onclick="Utils.setGpuLayers(-1,\'ggufOptGpuLayers\',\'ggufOptGpuLayersBadge\')">' +
                            Utils.escapeHtml(_('gguf.gpu_layers_btn_all', 'All layers (-1)')) + '</button>' +
                        '<button type="button" class="btn btn-secondary" data-gpu-value="0" ' +
                            'onclick="Utils.setGpuLayers(0,\'ggufOptGpuLayers\',\'ggufOptGpuLayersBadge\')">' +
                            Utils.escapeHtml(_('gguf.gpu_layers_btn_cpu', 'CPU only (0)')) + '</button>' +
                    '</div>' +
                    '<small style="color:var(--text-muted);display:block;margin-top:6px;">' +
                        Utils.escapeHtml(_('gguf.gpu_layers_desc')) + ' ' +
                        '<strong style="color:var(--success);">' +
                        Utils.escapeHtml(_('gguf.gpu_layers_recommended', 'AUTO is recommended.')) + '</strong>' +
                    '</small>' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.flash_attn_type')) + '</label>' +
                    '<input type="number" id="ggufOptFlashAttn" class="form-control" value="' + fa + '" min="-1" max="2">' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.flash_attn_type_desc')) + '</small>' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.n_threads')) + '</label>' +
                    '<input type="number" id="ggufOptNThreads" class="form-control" value="' + threads + '" min="0" max="256">' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.n_threads_desc')) + '</small>' +
                '</div>' +
            '</div>' +
            '<div class="form-row" style="display:grid;grid-template-columns:repeat(4,1fr);gap:12px;">' +
                '<div class="form-group"><label><input type="checkbox" id="ggufOptNuma" ' + (numa ? 'checked' : '') + '> ' + Utils.escapeHtml(_('gguf.numa')) + '</label></div>' +
                '<div class="form-group"><label><input type="checkbox" id="ggufOptUseMmap" ' + (mmap ? 'checked' : '') + '> ' + Utils.escapeHtml(_('gguf.use_mmap')) + '</label></div>' +
                '<div class="form-group"><label><input type="checkbox" id="ggufOptUseMlock" ' + (mlock ? 'checked' : '') + '> ' + Utils.escapeHtml(_('gguf.use_mlock')) + '</label></div>' +
                '<div class="form-group"><label><input type="checkbox" id="ggufOptAutoGpu" ' + (autoGpu ? 'checked' : '') + '> ' + Utils.escapeHtml(_('gguf.auto_gpu_distribution')) + '</label></div>' +
            '</div>' +
            // ====== Multi-GPU ======
            '<h6 class="gguf-section-header"><i class="fas fa-layer-group"></i> ' + Utils.escapeHtml(_('gguf.tab_multi_gpu') || 'Multi-GPU') + '</h6>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.tensor_split')) + '</label>' +
                    '<input type="text" id="ggufOptTensorSplit" class="form-control" value="' + Utils.escapeHtml(tensorSplitVal) + '" placeholder="0.5,0.5">' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.tensor_split_desc')) + '</small>' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.split_mode')) + '</label>' +
                    // R118 (2026-10-14): только ПРОВЕРЕННЫЕ значения. Раньше здесь
                    // было числовое поле -1..3, то есть оператор мог выставить
                    // непроверенные режимы разбиения. Раскладка слоёв между
                    // машинами проверена живьём именно в LAYER, поэтому в форме
                    // остались только default и LAYER.
                    '<select id="ggufOptSplitMode" class="form-control">' +
                        GGUF_VERIFIED_SPLIT_MODES.map(function (o) {
                            return '<option value="' + o.value + '"' +
                                (String(splitMode) === String(o.value) ? ' selected' : '') + '>' +
                                Utils.escapeHtml(o.label) + '</option>';
                        }).join('') +
                    '</select>' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.split_mode_desc') || '-1 = default, 1 = LAYER (раскладка слоёв)') + '</small>' +
                '</div>' +
            '</div>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.main_gpu')) + '</label>' +
                    '<input type="number" id="ggufOptMainGpu" class="form-control" value="' + mainGpu + '" min="0" max="16">' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.rpc_backend')) + '</label>' +
                    // R118 (2026-10-14): только тот backend, на котором раскладка
                    // проверена живьём (CUDA — мастер RTX 3070 + донор RTX 5060).
                    // Vulkan/Kompute из списка убраны: эти ветки на стенде не
                    // проверялись, а выбор «вслепую» ломал загрузку.
                    '<select id="ggufOptRpcBackend" class="form-control">' +
                        GGUF_VERIFIED_RPC_BACKENDS.map(function (o) {
                            return '<option value="' + o.value + '"' +
                                (rpcBackend === o.value ? ' selected' : '') + '>' +
                                Utils.escapeHtml(o.label) + '</option>';
                        }).join('') +
                    '</select>' +
                '</div>' +
            '</div>' +
            '<div class="form-row">' +
                '<div class="form-group"><label><input type="checkbox" id="ggufOptNoMemoryMap" ' + (noMemoryMap ? 'checked' : '') + '> ' + Utils.escapeHtml(_('gguf.no_memory_map')) + '</label></div>' +
            '</div>' +
            // ====== Донор / приёмник (раскладка слоёв) ======
            //
            // R124 (2026-10-16, пункты 2-4 разбора стенда). Управление раскладкой
            // раньше лежало ВНУТРИ блока Multi-GPU, в одной строке с «без mmap»:
            // галочка режима, список узлов — и ничего о том, участвует ли в
            // раскладке САМА эта машина. Оператор не мог ответить на вопросы «кто
            // донор», «кто приёмник», «работает ли узел» — и не видел, что донором
            // теперь может быть сам cppworker, а не отдельный контейнер.
            //
            // Теперь это ОТДЕЛЬНАЯ секция: сначала роль этой машины (отдаёт карту
            // соседу — узел rpc-server), затем узлы, которые подключает мастер, и
            // ЖИВОЕ состояние процесса.
            '<h6 class="gguf-section-header"><i class="fas fa-network-wired"></i> ' +
                Utils.escapeHtml(_('gguf.tab_rpc_role', 'Донор / приёмник (раскладка слоёв)')) + '</h6>' +
            '<div class="form-row">' +
                '<div class="form-group gguf-rpc-node-block">' +
                    '<label><input type="checkbox" id="ggufOptRpcServerEnabled" ' + (rpcServerEnabled ? 'checked' : '') + '> ' +
                        Utils.escapeHtml(_('gguf.rpc_server_enabled', 'Эта машина отдаёт свою видеокарту соседу (узел раскладки)')) + '</label>' +
                    '<small class="form-text">' + Utils.escapeHtml(_('gguf.rpc_server_hint') ||
                        'cppworker сам поднимает llama.cpp rpc-server — отдельный контейнер не нужен. ' +
                        'Порт должен совпадать с публикацией в compose, иначе узел будет недоступен снаружи.') + '</small>' +
                '</div>' +
            '</div>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 2fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.rpc_server_port', 'Порт узла')) + '</label>' +
                    '<input type="number" id="ggufOptRpcServerPort" class="form-control" value="' + rpcServerPort + '" min="1" max="65535">' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.rpc_server_state', 'Состояние узла')) + '</label>' +
                    // Показываем ФАКТ, а не галочку: процесс/порт/причина отказа.
                    // Нет данных — честно говорим «нет данных» (старая сборка
                    // cppworker поля не отдаёт), а не рисуем «работает».
                    '<div id="ggufOptRpcServerState" class="gguf-rpc-state">' + rpcServerStateHtml + '</div>' +
                '</div>' +
            '</div>' +
            '<div class="form-row">' +
                '<div class="form-group">' +
                    '<label><input type="checkbox" id="ggufOptRpcEnabled" ' + (rpcEnabled ? 'checked' : '') + '> ' +
                        Utils.escapeHtml(_('gguf.rpc_enabled')) + '</label>' +
                    '<textarea id="ggufOptRpcServers" class="form-control" rows="3" spellcheck="false" ' +
                        'placeholder="192.0.2.10:50052">' + Utils.escapeHtml(rpcServersVal) + '</textarea>' +
                    '<small class="form-text">' + Utils.escapeHtml(_('gguf.rpc_servers_hint')) + '</small>' +
                    // R125 (пункт 3): какие узлы РЕАЛЬНО отвечают. Оператор просил
                    // метрику «какой донор активен» — показываем факт проверки, а не
                    // пересказ списка. Нет данных — «не проверялось».
                    '<div id="ggufOptRpcPeers" class="gguf-rpc-peers">' + renderRpcPeers(peers) + '</div>' +
                '</div>' +
            '</div>' +
            // ====== KV Cache ======
            '<h6 class="gguf-section-header"><i class="fas fa-memory"></i> ' + Utils.escapeHtml(_('gguf.tab_kv_cache') || 'KV-cache') + '</h6>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.kv_cache_type')) + '</label>' +
                    '<select id="ggufOptKvCacheType" class="form-control">' +
                        '<option value=""' + (kvCacheType === '' ? ' selected' : '') + '>— inherit (F16) —</option>' +
                        '<option value="f16"' + (kvCacheType === 'f16' ? ' selected' : '') + '>f16 (default)</option>' +
                        '<option value="f32"' + (kvCacheType === 'f32' ? ' selected' : '') + '>f32 (full precision)</option>' +
                        '<option value="q8_0"' + (kvCacheType === 'q8_0' ? ' selected' : '') + '>q8_0 (-50% VRAM)</option>' +
                        '<option value="q4_0"' + (kvCacheType === 'q4_0' ? ' selected' : '') + '>q4_0 (-75% VRAM)</option>' +
                    '</select>' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.kv_cache_type_desc')) + '</small>' +
                '</div>' +
                '<div class="form-group">' +
                    '<label><input type="checkbox" id="ggufOptNoKvOffload" ' + (noKvOffload ? 'checked' : '') + '> ' + Utils.escapeHtml(_('gguf.no_kv_offload')) + '</label>' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.no_kv_offload_desc') || 'Держать KV-cache в RAM (не выгружать на GPU)') + '</small>' +
                '</div>' +
            '</div>' +
            // ====== RoPE & YaRN ======
            '<h6 class="gguf-section-header"><i class="fas fa-wave-square"></i> ' + Utils.escapeHtml(_('gguf.tab_rope_yarn') || 'RoPE / YaRN') + '</h6>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.rope_freq_base')) + '</label>' +
                    '<input type="number" id="ggufOptRopeFreqBase" class="form-control" value="' + ropeFreqBase + '" min="1" step="0.0001">' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.rope_freq_scale')) + '</label>' +
                    '<input type="number" id="ggufOptRopeFreqScale" class="form-control" value="' + ropeFreqScale + '" min="0" step="0.0001">' +
                '</div>' +
            '</div>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.rope_scaling_type')) + '</label>' +
                    '<select id="ggufOptRopeScalingType" class="form-control">' +
                        '<option value="none"' + (ropeScalingType === 'none' ? ' selected' : '') + '>none</option>' +
                        '<option value="linear"' + (ropeScalingType === 'linear' ? ' selected' : '') + '>linear</option>' +
                        '<option value="yarn"' + (ropeScalingType === 'yarn' ? ' selected' : '') + '>yarn</option>' +
                    '</select>' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.rope_scaling_factor')) + '</label>' +
                    '<input type="number" id="ggufOptRopeScalingFactor" class="form-control" value="' + ropeScalingFactor + '" min="0" step="0.01">' +
                '</div>' +
            '</div>' +
            '<div class="form-row" style="display:grid;grid-template-columns:repeat(4,1fr);gap:12px;">' +
                '<div class="form-group"><label>' + Utils.escapeHtml(_('gguf.yarn_ext_factor')) + '<input type="number" id="ggufOptYarnExtFactor" class="form-control" value="' + yarnExtFactor + '" min="0" step="0.01"></label></div>' +
                '<div class="form-group"><label>' + Utils.escapeHtml(_('gguf.yarn_attn_factor')) + '<input type="number" id="ggufOptYarnAttnFactor" class="form-control" value="' + yarnAttnFactor + '" min="0" step="0.01"></label></div>' +
                '<div class="form-group"><label>' + Utils.escapeHtml(_('gguf.yarn_beta_fast')) + '<input type="number" id="ggufOptYarnBetaFast" class="form-control" value="' + yarnBetaFast + '" min="0" step="0.01"></label></div>' +
                '<div class="form-group"><label>' + Utils.escapeHtml(_('gguf.yarn_beta_slow')) + '<input type="number" id="ggufOptYarnBetaSlow" class="form-control" value="' + yarnBetaSlow + '" min="0" step="0.01"></label></div>' +
            '</div>' +
            // ====== Performance / Metrics / Lifecycle ======
            '<h6 class="gguf-section-header"><i class="fas fa-tachometer-alt"></i> ' + Utils.escapeHtml(_('gguf.tab_perf') || 'Performance') + '</h6>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.rms_norm_eps')) + '</label>' +
                    '<input type="number" id="ggufOptRmsNormEps" class="form-control" value="' + rmsNormEps + '" min="0" step="0.000001">' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.idle_unload_minutes')) + '</label>' +
                    '<input type="number" id="ggufOptIdleUnloadMinutes" class="form-control" value="' + idleUnload + '" min="0" max="10080">' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.idle_unload_desc') || '0 = off, >0 = unload after N minutes idle') + '</small>' +
                '</div>' +
            '</div>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">' +
                '<div class="form-group"><label><input type="checkbox" id="ggufOptEnableMetrics" ' + (enableMetrics ? 'checked' : '') + '> ' + Utils.escapeHtml(_('gguf.enable_metrics')) + '</label></div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.metrics_retention')) + '</label>' +
                    '<input type="number" id="ggufOptMetricsRetention" class="form-control" value="' + metricsRetention + '" min="0">' +
                '</div>' +
            '</div>' +
            // ====== Inference (Round 15, 2026-07-29) ======
            // n_parallel + enableReasoning + reasoningBudget — default значения
            // для ВСЕХ загружаемых моделей на этом backend. Per-Model Profile
            // (cppworker-params.js) может override'нуть для конкретной модели.
            '<h6 class="gguf-section-header"><i class="fas fa-brain"></i> ' + Utils.escapeHtml(_('gguf.tab_inference') || 'Inference') + '</h6>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.n_parallel') || 'Parallel sequences (n_parallel)') + '</label>' +
                    '<input type="number" id="ggufOptNParallel" class="form-control" value="' + nParallel + '" min="0" max="8">' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.n_parallel_desc') || 'Default parallel slots per model. 0 = 1 (sequential), 1..8 = multi-slot KV-cache isolation. > 1 = more VRAM.') + '</small>' +
                '</div>' +
                '<div class="form-group">' +
                    '<label><input type="checkbox" id="ggufOptEnableReasoning" ' + (enableReasoning ? 'checked' : '') + '> ' + Utils.escapeHtml(_('gguf.enable_reasoning') || 'Enable reasoning/thinking') + '</label>' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.enable_reasoning_desc') || 'Soft+native thinking path for gemma-4, deepseek-r1, qwen3-thinking.') + '</small>' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.reasoning_budget') || 'Reasoning budget (tokens)') + '</label>' +
                    '<input type="number" id="ggufOptReasoningBudget" class="form-control" value="' + reasoningBudget + '" min="0" max="100000" step="256">' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.reasoning_budget_desc') || 'Max thinking tokens before answer. 0 = unlimited.') + '</small>' +
                '</div>' +
            '</div>' +
            // ====== Diagnostics ======
            '<div class="form-group" style="color:var(--text-muted);font-size:12px;margin-top:8px;">' +
                (nodeName ? ('<div><b>node:</b> ' + Utils.escapeHtml(nodeName) + '</div>') : '') +
                (balancerUrl ? ('<div><b>balancer:</b> ' + Utils.escapeHtml(balancerUrl) + '</div>') : '') +
                (uptime ? ('<div><b>uptime:</b> ' + Utils.escapeHtml(uptime) + '</div>') : '') +
            '</div>' +
            '<div class="form-actions" style="margin-top:12px;display:flex;gap:8px;flex-wrap:wrap;">' +
                '<button class="btn btn-primary" id="ggufBackendOptionsSave">' +
                    '<i class="fas fa-save"></i> ' + Utils.escapeHtml(_('gguf.save_backend_options')) +
                '</button>' +
                '<button class="btn btn-secondary" id="ggufBackendOptionsReload">' +
                    '<i class="fas fa-sync"></i> ' + Utils.escapeHtml(_('gguf.reload_backend_options')) +
                '</button>' +
                // R106b (2026-10-12): «сделать эти значения приоритетом для новых
                // моделей». Слой «настройки контейнера» слабее двух других (профиль
                // модели и «настройки по умолчанию»), поэтому значения этой формы
                // сами по себе не выигрывают при загрузке. Кнопка переносит
                // load-affecting поля в тот слой, который реально применяется —
                // без ручного дублирования значений в двух местах.
                '<button class="btn btn-secondary" id="ggufBackendOptionsPromote" title="' +
                    Utils.escapeHtml(_('gguf.backend_options_promote_title', 'Записать n_ctx, слои, KV-cache, параллельность, flash и размышления в «настройки по умолчанию для новых моделей» — слой, который применяется при загрузке раньше настроек контейнера.')) + '">' +
                    '<i class="fas fa-arrow-up"></i> ' + Utils.escapeHtml(_('gguf.backend_options_promote', 'Сделать приоритетом для новых моделей')) +
                '</button>' +
            '</div>';
        // R124: плоская форма → компактные раскрываемые секции (см. collapseSections).
        return collapseSections(formHtml);
    }

    async function saveBackendOptions() {
        const backend = M.currentBackend();
        if (!backend) return;
        // Парсим значения из формы. Хелперы parseIntOr/parseFloatOr/strVal
        // (определены ниже) устойчивы к NaN/пустым полям.
        const splitStr = parseStr('ggufOptTensorSplit', '');
        let tensorSplit = null;
        if (splitStr.trim() !== '') {
            // Парсим "0.5,0.5" → [0.5, 0.5]
            const parts = splitStr.split(',').map(s => parseFloat(s.trim()));
            if (parts.every(p => Number.isFinite(p))) {
                tensorSplit = parts;
            }
        }
        const payload = {
            // Базовые
            defaultCtxSize: parseIntOr(document.getElementById('ggufOptCtxSize').value, 4096),
            defaultBatchSize: parseIntOr(document.getElementById('ggufOptBatchSize').value, 512),
            defaultGpuLayers: parseIntOr(document.getElementById('ggufOptGpuLayers').value, -1),
            defaultFlashAttnType: parseIntOr(document.getElementById('ggufOptFlashAttn').value, -1),
            defaultNuma: !!document.getElementById('ggufOptNuma').checked,
            defaultUseMmap: !!document.getElementById('ggufOptUseMmap').checked,
            defaultUseMlock: !!document.getElementById('ggufOptUseMlock').checked,
            defaultNThreads: parseIntOr(document.getElementById('ggufOptNThreads').value, 0),
            defaultRmsNormEps: parseFloatOr(document.getElementById('ggufOptRmsNormEps').value, 0.00001),
            // Multi-GPU
            autoGpuDistribution: !!document.getElementById('ggufOptAutoGpu').checked,
            defaultSplitMode: parseIntOr(document.getElementById('ggufOptSplitMode').value, -1),
            defaultMainGpu: parseIntOr(document.getElementById('ggufOptMainGpu').value, 0),
            defaultRpcBackend: parseStr('ggufOptRpcBackend', ''),
            defaultNoMemoryMap: !!document.getElementById('ggufOptNoMemoryMap').checked,
            defaultTensorSplit: tensorSplit,
            // KV cache
            defaultKvCacheType: parseStr('ggufOptKvCacheType', ''),
            defaultNoKvOffload: !!document.getElementById('ggufOptNoKvOffload').checked,
            // RoPE/YaRN
            defaultRopeFreqBase: parseFloatOr(document.getElementById('ggufOptRopeFreqBase').value, 10000.0),
            defaultRopeFreqScale: parseFloatOr(document.getElementById('ggufOptRopeFreqScale').value, 1.0),
            defaultRopeScalingType: parseStr('ggufOptRopeScalingType', 'none'),
            defaultRopeScalingFactor: parseFloatOr(document.getElementById('ggufOptRopeScalingFactor').value, 1.0),
            defaultYarnExtFactor: parseFloatOr(document.getElementById('ggufOptYarnExtFactor').value, 1.0),
            defaultYarnAttnFactor: parseFloatOr(document.getElementById('ggufOptYarnAttnFactor').value, 1.0),
            defaultYarnBetaFast: parseFloatOr(document.getElementById('ggufOptYarnBetaFast').value, 32.0),
            defaultYarnBetaSlow: parseFloatOr(document.getElementById('ggufOptYarnBetaSlow').value, 1.0),
            // Performance
            idleUnloadMinutes: parseIntOr(document.getElementById('ggufOptIdleUnloadMinutes').value, 0),
            enableMetrics: !!document.getElementById('ggufOptEnableMetrics').checked,
            metricsRetentionSeconds: parseIntOr(document.getElementById('ggufOptMetricsRetention').value, 3600),
            // Inference (Round 15, 2026-07-29): n_parallel + reasoning.
            // enableReasoning / reasoningBudget — без "default" префикса, как в app.js
            // (handler applyBool("enableReasoning") в handlers_config.go:387).
            defaultNParallel: parseIntOr(document.getElementById('ggufOptNParallel').value, 0),
            enableReasoning: !!document.getElementById('ggufOptEnableReasoning').checked,
            reasoningBudget: parseIntOr(document.getElementById('ggufOptReasoningBudget').value, 0),
            // R106b (2026-10-12): те же значения — ещё и в слой «настройки по
            // умолчанию ДЛЯ ЗАГРУЗКИ».
            //
            // ЗАЧЕМ. `enableReasoning` (runtime) включает thinking-инструкцию в
            // промпте, но модель при загрузке наследует ДРУГОЕ поле —
            // `defaultEnableReasoning`. Без него галочка на этой странице не
            // доезжала до новой загрузки: оператор включал размышления, а
            // cppworker поднимал модель с reasoning_enabled=false — «галочка не
            // применяется, модель грузится не с теми параметрами» (живая жалоба).
            defaultEnableReasoning: !!document.getElementById('ggufOptEnableReasoning').checked,
            defaultReasoningBudget: parseIntOr(document.getElementById('ggufOptReasoningBudget').value, 0)
        };
        // R115: раскладка слоёв. Отправляем ТОЛЬКО если блок отрисован (иначе
        // сохранение настроек молча выключило бы раскладку на бэкенде, где поля
        // нет). Пустой список — законное значение: узлы убираются, режим остаётся
        // таким, каким его выставила галочка.
        const rpcEnabledEl = document.getElementById('ggufOptRpcEnabled');
        const rpcServersEl = document.getElementById('ggufOptRpcServers');
        if (rpcEnabledEl) {
            payload.rpcEnabled = !!rpcEnabledEl.checked;
        }
        if (rpcServersEl) {
            payload.rpcServers = rpcServersEl.value
                .split(/[\n,]/)
                .map(function (s) { return s.trim(); })
                .filter(function (s) { return s.length > 0; });
        }
        // R124 (2026-10-16, пункт 2): узел раскладки внутри ЭТОГО cppworker.
        // Как и rpcEnabled/rpcServers — отправляем только если поля отрисованы:
        // иначе сохранение настроек на бэкенде со старой сборкой молча выключило
        // бы узел, которого там и не было.
        const rpcServerEnabledEl = document.getElementById('ggufOptRpcServerEnabled');
        if (rpcServerEnabledEl) {
            payload.rpcServerEnabled = !!rpcServerEnabledEl.checked;
        }
        const rpcServerPortEl = document.getElementById('ggufOptRpcServerPort');
        if (rpcServerPortEl) {
            payload.rpcServerPort = parseIntOr(rpcServerPortEl.value, 0);
        }
        // === Round 32 #3 (2026-08-10): полное обновление state.loadOptions ===
        // Раньше (до фикса) обновлялся только autoGpuDistribution — остальные поля
        // (gpuLayers/ctxSize/batchSize/flashAttn/numa/useMmap) оставались в state
        // хардкодными (2048/-1/512), независимо от того, что ввёл пользователь.
        // Теперь: читаем все поля из формы + помечаем backend как "custom"
        // (syncLoadOptionsFromConfig не будет их перезатирать при следующем заходе).
        state.loadOptions.gpuLayers = payload.defaultGpuLayers;
        state.loadOptions.ctxSize = payload.defaultCtxSize;
        state.loadOptions.batchSize = payload.defaultBatchSize;
        // cppworker flash_attn_type: -1=auto, 0=disabled, 1=enabled.
        // webui loadOptions.flashAttn: bool. Конвертим: 0=false, ≠0=true.
        state.loadOptions.flashAttn = (payload.defaultFlashAttnType !== 0);
        state.loadOptions.numa = payload.defaultNuma;
        state.loadOptions.useMmap = payload.defaultUseMmap;
        state.loadOptions.autoGpuDistribution = payload.autoGpuDistribution;
        if (Array.isArray(payload.defaultTensorSplit) && payload.defaultTensorSplit.length > 0) {
            state.loadOptions.tensorSplit = payload.defaultTensorSplit.join(',');
        }
        markLoadOptionsCustom(backend.id);
        const saveBtn = document.getElementById('ggufBackendOptionsSave');
        if (saveBtn) saveBtn.disabled = true;
        try {
            const resp = await GgufApi.requestViaBackend(backend.id, '/api/v1/cppworker/config/update', {
                method: 'PUT',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(payload)
            });
            // Показываем пользователю, что применилось и сколько ошибок валидации.
            let msg = _('gguf.backend_options_saved');
            if (resp && resp.applied) {
                msg += ' (' + resp.applied.length + ' полей)';
            }
            if (resp && resp.validation_errors && resp.validation_errors.length > 0) {
                showToast('⚠️ ' + resp.validation_errors.length + ' validation errors: ' +
                    resp.validation_errors.slice(0, 3).join('; '), 'error');
            }
            showToast(msg, 'success');
            // R115: смена раскладки слоёв применяется при СЛЕДУЮЩЕЙ загрузке
            // модели. Без этого сообщения «сохранено» читалось бы как «уже
            // работает», хотя загруженная модель осталась на прежней схеме.
            if (resp && resp.rpc_reload_required) {
                showToast(_('gguf.rpc_reload_required'), 'info');
            }
            // Re-fetch config чтобы пользователь сразу видел, что изменения применились.
            // cppworker возвращает applied[] и reload_started[] — показываем их в toast.
            if (resp && resp.reload_started && resp.reload_started.length > 0) {
                showToast('Reloading ' + resp.reload_started.length + ' model(s) with new defaults...', 'info');
            }
            // Перезагружаем форму с актуальными значениями из cppworker.
            await loadAndRenderBackendOptions();
            // R106b (2026-10-12): обновляем и блоки «что получат новые модели» /
            // «модели в папке» — иначе после сохранения оператор видит старое
            // значение и делает вывод «галочка не применилась».
            if (window.CppWorkerParams && typeof window.CppWorkerParams.loadAndRender === 'function') {
                window.CppWorkerParams.loadAndRender();
            }
            // Также обновляем runtime-параметры загруженных моделей (context_size и т.д.).
            try {
                const rtData = await GgufApi.getRuntimeConfigViaBackend(backend.id);
                if (rtData && rtData.loaded_models) {
                    state.runtimeModels = {};
                    (rtData.loaded_models || []).forEach(function (m) {
                        state.runtimeModels[m.name] = m;
                    });
                }
            } catch (rtErr) {
                // runtime config может быть недоступен на старых cppworker — игнорируем.
            }
        } catch (e) {
            showToast(_('gguf.backend_options_save_error') + ': ' + (e.message || e), 'error');
        } finally {
            if (saveBtn) saveBtn.disabled = false;
        }
    }

    /**
     * R106b (2026-10-12): перенести load-affecting значения формы в «настройки по
     * умолчанию для новых моделей» (config.defaultModelProfile) — слой, который
     * применяется к новой модели РАНЬШЕ настроек контейнера.
     *
     * ПОЧЕМУ ОТДЕЛЬНАЯ КНОПКА, а не автоматическая запись при Save. Молчаливая
     * запись в чужой слой перезаписала бы настройки, выставленные оператором
     * ранее (на стенде: kv=q4_0, parallel=2, flash=on), значениями формы
     * контейнера (kv=f16, flash=auto) — то есть «сделать приоритетом» тихо
     * УХУДШИЛО бы загрузку. Поэтому перенос — явное действие оператора, а
     * результат виден в блоке «Что получат новые модели» ниже.
     */
    async function promoteBackendOptionsToDefaults() {
        const api = window.Api;
        if (!api || !api.cppworkerLoadDefaults || typeof api.cppworkerLoadDefaults.save !== 'function') {
            showToast(_('gguf.backend_options_promote_unavailable',
                'API «настройки по умолчанию» недоступно — обновите страницу'), 'error');
            return;
        }
        const btn = document.getElementById('ggufBackendOptionsPromote');
        if (btn) btn.disabled = true;
        const intOrSkip = function (id) {
            const el = document.getElementById(id);
            if (!el) return undefined;
            const n = parseInt(el.value, 10);
            return Number.isFinite(n) ? n : undefined;
        };
        const body = {};
        // contextLength обязателен для слоя: без него PUT отказывает, если
        // дефолтного профиля ещё нет.
        const ctx = intOrSkip('ggufOptCtxSize');
        if (ctx !== undefined && ctx > 0) body.contextLength = ctx;
        const batch = intOrSkip('ggufOptBatchSize');
        if (batch !== undefined && batch > 0) body.batchSize = batch;
        const layers = intOrSkip('ggufOptGpuLayers');
        if (layers !== undefined) body.numGpuLayers = layers;
        const parallel = intOrSkip('ggufOptNParallel');
        if (parallel !== undefined && parallel > 0) body.parallel = parallel;
        const kvEl = document.getElementById('ggufOptKvCacheType');
        if (kvEl && kvEl.value) body.kvCacheType = kvEl.value;
        // flashAttn в профиле — *bool, в контейнере — -1/0/1. -1 (auto) =
        // «не задавать», иначе явное true/false.
        const fa = intOrSkip('ggufOptFlashAttn');
        if (fa !== undefined && fa !== -1) body.flashAttn = (fa === 1);
        const mmapEl = document.getElementById('ggufOptUseMmap');
        if (mmapEl) body.useMmap = !!mmapEl.checked;
        const reasoningEl = document.getElementById('ggufOptEnableReasoning');
        if (reasoningEl) body.enableReasoning = !!reasoningEl.checked;

        try {
            await api.cppworkerLoadDefaults.save(body);
            showToast(_('gguf.backend_options_promoted',
                'Записано в «настройки по умолчанию для новых моделей» — теперь эти значения применяются при загрузке раньше настроек контейнера'), 'success');
            if (window.CppWorkerParams && typeof window.CppWorkerParams.loadAndRender === 'function') {
                window.CppWorkerParams.loadAndRender();
            }
        } catch (e) {
            showToast(_('gguf.backend_options_promote_error',
                'Не удалось записать настройки по умолчанию') + ': ' + (e && e.message ? e.message : e), 'error');
        } finally {
            if (btn) btn.disabled = false;
        }
    }

    // Хелперы для парсинга значений из формы (parseIntOr объявлена ниже,
    // parseFloatOr/parseStr определяем тут же — близко к месту использования).
    function parseFloatOr(v, def) {
        const n = parseFloat(v);
        return Number.isFinite(n) ? n : def;
    }
    function parseStr(id, def) {
        const el = document.getElementById(id);
        if (!el) return def;
        return el.value;
    }

    function parseIntOr(v, def) {
        const n = parseInt(v, 10);
        return Number.isFinite(n) ? n : def;
    }

    /**
     * Инициализировать Per-Model Profiles внутри gguf-настроек.
     * Использует window.CppWorkerParams (cppworker-params.js).
     */
    function mountProfilesInSettings() {
        const btn = document.getElementById('cppProfileAddBtn');
        if (btn && !btn._ggufBound) {
            btn._ggufBound = true;
            btn.addEventListener('click', function () {
                if (window.CppWorkerParams && typeof window.CppWorkerParams.openWizard === 'function') {
                    window.CppWorkerParams.openWizard('', null);
                }
            });
        }
        if (window.CppWorkerParams && typeof window.CppWorkerParams.loadAndRender === 'function') {
            window.CppWorkerParams.loadAndRender();
        }
    }

    function renderConnectModal() {
        return '' +
            '<div class="gguf-modal-backdrop" id="ggufConnectModal">' +
                '<div class="gguf-modal">' +
                    '<h3><i class="fas fa-plug"></i> ' + _('gguf.modal_title') + '</h3>' +
                    '<p>' + _('gguf.modal_desc') + '</p>' +
                    '<div class="form-group">' +
                        '<label>' + (_('gguf.cppworker_url_placeholder') || 'cppworker URL') + '</label>' +
                        '<input type="text" id="ggufConnectUrl" class="form-control" placeholder="http://host:18091" value="' + Utils.escapeHtml(GgufApi.getUrl()) + '">' +
                    '</div>' +
                    '<div class="gguf-modal-actions">' +
                        '<button class="btn btn-secondary" id="ggufConnectCancel"><i class="fas fa-times"></i> ' + (_('common.cancel') || 'Cancel') + '</button>' +
                        '<button class="btn btn-primary" id="ggufConnectSubmit"><i class="fas fa-plug"></i> ' + (_('gguf.connect') || 'Connect') + '</button>' +
                    '</div>' +
                '</div>' +
            '</div>';
    }

    // R59.5 (2026-09-03): export to GgufModule namespace so gguf-renderer.js
    // can capture them as `var renderDetailPanel = M.renderDetailPanel;`.
    // The whole file is wrapped in TWO IIFEs (outer at line 30 sets up `M`,
    // inner at line 42 defines functions), and the inner IIFE was never
    // exporting them — so M.renderDetailPanel etc. were all `undefined` at
    // the moment gguf-renderer.js tried to grab them. Symptom: TypeError
    // "renderDetailPanel is not a function" on first switch to GGUF page.
    //
    // We export the 14 names that gguf-renderer.js (lines 68-81) expects,
    // plus a couple of internals (loadAndRenderBackendOptions) that are
    // also referenced from elsewhere. Internal-only helpers (formatBytesShort,
    // parseIntOr, hfFileName, etc.) stay private to this file.
    M.renderDetailPanel = renderDetailPanel;
    M.renderEmptyDetail = renderEmptyDetail;
    M.renderDetailHeader = renderDetailHeader;
    M.renderDetailTabs = renderDetailTabs;
    M.renderDetailPane = renderDetailPane;
    M.renderAboutPane = renderAboutPane;
    // R91: хост-метрики (RAM/CPU/диск) — экспортируем для юнит-тестов:
    // webui/js/modules/gguf-host-ram.test.js проверяет, что RAM хоста реально
    // попадает в разметку панели «Инфо», а не только парсится.
    M.renderHostSystemCard = renderHostSystemCard;
    M.hostSystemPayload = hostSystemPayload;
    M.renderModelsPane = renderModelsPane;
    // R165 (2026-10-17): НЕ переопределяем mergeModelRows. Реализация одна — в
    // gguf-renderer-refresh.js (загружается раньше), и именно она попадает в
    // M.mergeModelRows. Вторая копия здесь удалена: она перекрывала первую, и
    // R156 приходилось править в двух местах.

    M.loadedHandleFor = loadedHandleFor;
    M.renderLoadedPane = renderLoadedPane;
    M.renderDownloadsPane = renderDownloadsPane;
    M.renderHuggingFacePane = renderHuggingFacePane;
    M.renderSettingsPane = renderSettingsPane;
    M.loadAndRenderBackendOptions = loadAndRenderBackendOptions;
    M.mountProfilesInSettings = mountProfilesInSettings;
    M.renderConnectModal = renderConnectModal;
    // R118: пометки раскладки слоёв — публикуем как чистые функции, чтобы их
    // проверял тест (gguf-layout-marks-r118.test.js), а не DOM.
    M.buildLoadedFlags = buildLoadedFlags;
    // R119 (пункт 4): источники параметров загрузки (чистые функции — их
    // проверяет node-тест gguf-param-sources-r119.test.js).
    M.buildParamSources = buildParamSources;
    M.renderParamSources = renderParamSources;
    // R124 (пункты 3-4): раскрываемые секции формы и состояние узла раскладки —
    // экспортируем для тестов (проверяются как чистые функции на реальном HTML).
    M.collapseSections = collapseSections;
    M.renderRpcServerState = renderRpcServerState;
    // R125 (пункт 3): какие узлы раскладки реально отвечают.
    M.renderRpcPeers = renderRpcPeers;
    M.measureLinkSpeed = measureLinkSpeed;
    // R126 (пункты 5-6): редактор параметров модели прямо в карточке.
    M.renderModelParamsEditor = renderModelParamsEditor;
    M.collectModelParams = collectModelParams;
    M.applyModelParams = applyModelParams;
    M.loadedParamsFor = loadedParamsFor;
    // R134 (2026-10-16): единый реестр параметров и две таблицы (действующие /
    // задать) — экспортируем для тестов; hydrateOnOpen зовётся при раскрытии
    // карточки, чтобы подтянуть значения профиля.
    M.MODEL_PARAM_SPECS = MODEL_PARAM_SPECS;
    M.donorContributionHtml = donorContributionHtml;
    M.paramFact = paramFact;
    M.paramOrigin = paramOrigin;
    M.paramInput = paramInput;
    M.inheritedValueOf = inheritedValueOf;
    M.inheritedLabel = inheritedLabel;
    M.applyInheritedLabels = applyInheritedLabels;
    M.formatParamValue = formatParamValue;
    M.fillModelParamEditor = fillModelParamEditor;
    M.hydrateModelParamEditors = hydrateModelParamEditors;
    M.hydrateOnOpen = hydrateOnOpen;
    M.PARAM_SOURCE_LABELS = PARAM_SOURCE_LABELS;
    M.buildLayoutFlags = buildLayoutFlags;
    M.deriveLayerPlanJS = deriveLayerPlanJS;
    M.exactLayerPlanJS = exactLayerPlanJS;
    // R118: списки проверенных режимов — для теста, что непроверенные не
    // возвращаются в форму (unverified-modes-hidden-r118.test.js).
    M.GGUF_VERIFIED_RPC_BACKENDS = GGUF_VERIFIED_RPC_BACKENDS;
    M.GGUF_VERIFIED_SPLIT_MODES = GGUF_VERIFIED_SPLIT_MODES;

})();

})();
