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
                '<div style="font-family:monospace;font-size:var(--font-size-xs);background:rgba(0,0,0,0.2);' +
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
        return '<div class="gguf-inventory-note" style="display:flex;gap:10px;align-items:center;flex-wrap:wrap;font-size:var(--font-size-xs);margin:0 0 10px 0;">' +
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


    // R656 (2026-11-20): таб Models (карточки, план раскладки, флаги, источники
    // параметров) вынесен в gguf-detail-models.js (фабрика).
    var ggufDetailModels = window.GgufDetailModels({
        M: M,
        _: _,
        state: state,
        Utils: Utils,
        // R672 (2026-11-21): ctx фабрики не содержал двух имён, которые её тело
        // использует bare-именами — `formatFileSize` (gguf-detail-models.js:56) и
        // `stripGGUF` (:68,74,79). Живой дефект оператора: таб «Модели» не
        // прожимается — обработчик срабатывает, но renderModelsPane падает на
        // ReferenceError, панель не перерисовывается, а `state.detailPane` уже
        // равен "models": таб «залипает» на старой вкладке. Оба имени объявлены
        // выше в этом файле (строки 55-56).
        stripGGUF: stripGGUF,
        formatFileSize: formatFileSize,
        loadedHandleFor: loadedHandleFor,
        renderInventoryNote: renderInventoryNote,
        // R675 (2026-11-21): ключ showToast УДАЛЁН — он был инертен с самого
        // выноса (R656): фабрика GgufDetailModels ни разу не обращается к
        // ctx.showToast (grep по файлу пуст), то есть зависимость «передавали»,
        // но ею никто не пользовался. Такой ключ не просто мусор: он создаёт
        // ложное впечатление, что тост в этом слое доступен, и при следующей
        // правке его «уже передали» — а читать нечего. Проверка:
        // scripts/webui_ctx_contract_check.js (правило «ключ инертен»).
        formatElapsedShort: formatElapsedShort,
        isUnloadingHandle: isUnloadingHandle,
        unloadingElapsedMs: unloadingElapsedMs,
        unloadingItemFor: unloadingItemFor,
        renderModelParamsEditor: function () { return renderModelParamsEditor.apply(null, arguments); }
    });
    var renderModelsPane = ggufDetailModels.renderModelsPane;
    var exactLayerPlanJS = ggufDetailModels.exactLayerPlanJS;
    var deriveLayerPlanJS = ggufDetailModels.deriveLayerPlanJS;
    var firstLayerFrom = ggufDetailModels.firstLayerFrom;
    var buildLayoutFlags = ggufDetailModels.buildLayoutFlags;
    var buildParamSources = ggufDetailModels.buildParamSources;
    var renderParamSources = ggufDetailModels.renderParamSources;
    var buildLoadedFlags = ggufDetailModels.buildLoadedFlags;
    // R708: раскладка KV в карточке модели на диске. Чистые функции — их
    // проверяет node-тест webui/tests/gguf-kv-plan-r708.test.js; экспорт нужен,
    // чтобы тест звал ИХ, а не копию правил.
    var kvPlanFacts = ggufDetailModels.kvPlanFacts;
    var kvPlanLineText = ggufDetailModels.kvPlanLineText;
    var kvPlanLineTitle = ggufDetailModels.kvPlanLineTitle;
    var renderKVPlanLine = ggufDetailModels.renderKVPlanLine;
    var PARAM_SOURCE_LABELS = ggufDetailModels.PARAM_SOURCE_LABELS;
    var PARAM_SOURCE_HINTS = ggufDetailModels.PARAM_SOURCE_HINTS;

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
            '<div class="gguf-loaded-model-info" style="font-size:var(--font-size-xs);color:var(--text-muted);">' +
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
            '<div style="font-size:var(--font-size-xs);font-weight:600;">' + Utils.escapeHtml(_('gguf.donor_title', 'Вклад доноров')) +
                (modelName ? '<span style="font-weight:400;color:var(--text-secondary);"> — ' +
                    Utils.escapeHtml(modelName) + '</span>' : '') + '</div>' +
            '<table class="gguf-mp-table" style="margin-top:4px;"><thead><tr>' +
                '<th>' + Utils.escapeHtml(_('gguf.donor_node', 'Узел')) + '</th>' +
                '<th>' + Utils.escapeHtml(_('gguf.donor_layers', 'Слои')) + '</th>' +
                '<th>' + Utils.escapeHtml(_('gguf.donor_mem', 'Память (оценка)')) + '</th>' +
            '</tr></thead><tbody>' + rows.join('') + '</tbody></table>' +
            '<div style="font-size:var(--font-size-xs);color:var(--text-secondary);margin-top:4px;">' +
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
            // R395: у ошибки время уже не идёт — растущий счётчик выглядел как
            // бесконечная загрузка (жалоба: счётчик висит после загрузки модели).
            const isError = lm.state === 'error';
            const startedAt = lm.loadingStartedAt ? new Date(lm.loadingStartedAt).getTime() : Date.now();
            const elapsedMs = (lm.elapsedMs && lm.elapsedMs > 0) ? lm.elapsedMs : (Date.now() - startedAt);
            const elapsedSec = Math.max(0, Math.floor(elapsedMs / 1000));
            const elapsedLabel = elapsedSec < 60
                ? elapsedSec + 's'
                : Math.floor(elapsedSec / 60) + 'm ' + (elapsedSec % 60) + 's';
            let stateIcon, stateColor, stateText;
            if (isError) {
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
                    (isError ? '' : '<span style="margin-left:12px;">' + (_('gguf.elapsed') || 'Elapsed') + ': ' + elapsedLabel + '</span>') +
                '</div>' +
                (lm.error ? '<div class="gguf-loaded-model-info" style="color:var(--danger,#d9534f);font-size:var(--font-size-xs);">' +
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
                    '<span class="badge" style="background:var(--bg-tertiary);margin-left:6px;padding:2px 6px;font-size:var(--font-size-xs);">gpu: ' + Utils.escapeHtml(String(rtGpu)) + '</span>' : '';
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
                    rtHtml = '<div class="gguf-loaded-model-info" style="margin-top:4px;display:flex;gap:12px;flex-wrap:wrap;font-family:monospace;font-size:var(--font-size-xs);">' +
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
                ? '<span class="gguf-loaded-busy-badge" style="display:inline-flex;align-items:center;gap:6px;margin-left:8px;padding:2px 8px;background:rgba(217,83,79,0.15);color:#ff7b76;border-radius:10px;font-size:var(--font-size-xs);font-weight:600;" title="' + (_('gguf.busy_badge_title') || 'Active generations') + '">' +
                    '<span style="display:inline-block;width:6px;height:6px;background:#ff7b76;border-radius:50%;animation:gguf-pulse 1.2s infinite;"></span>' +
                    (_('gguf.busy_badge') || 'Generating') + ' (' + activeCount + ' ' + (_('gguf.active_short') || 'active') + ')' +
                    // Round 32 #2: cancel button внутри busy badge.
                    // data-model-name используется в event handler для определения
                    // какую модель отменять.
                    '<button class="btn btn-xs gguf-cancel-gen-btn" data-model-name="' + Utils.escapeHtml(name) + '" ' +
                            'style="margin-left:4px;padding:1px 8px;font-size:var(--font-size-xs);line-height:1.4;background:#d9534f;color:#fff;border:none;border-radius:8px;cursor:pointer;" ' +
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
                ? '<div class="gguf-loaded-model-flags" style="margin-top:4px;display:flex;gap:8px;flex-wrap:wrap;font-family:monospace;font-size:var(--font-size-xs);">' +
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
                    ? '<div class="gguf-loaded-model-unloading-inline" style="display:flex;align-items:center;gap:8px;margin-top:6px;font-size:var(--font-size-xs);color:var(--warning, #e0a030);">' +
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
                '<h5 style="margin:0 0 8px 0;font-size:var(--font-size-sm);color:var(--text-secondary);text-transform:uppercase;letter-spacing:0.5px;">' +
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
                '<h5 style="margin:0 0 8px 0;font-size:var(--font-size-sm);color:var(--text-secondary);text-transform:uppercase;letter-spacing:0.5px;">' +
                    (_('gguf.download_history') || 'Recent downloads') + '</h5>' +
                historyItems.map(renderDownloadItem).join('') +
               '</div>');

        // R66.4 (2026-09-16): orphan .download файлы — то, что осталось после сбоев.
        // cppworker возвращает filename+size, но НЕ modelId (нельзя восстановить
        // из имени файла). UI показывает "Residual files" отдельным блоком.
        const orphanHtml = orphans.length === 0
            ? ''
            : ('<div class="gguf-downloads-section gguf-orphans-section" style="margin-top:16px;padding-top:16px;border-top:1px solid var(--warning);">' +
                '<h5 style="margin:0 0 8px 0;font-size:var(--font-size-sm);color:var(--warning);text-transform:uppercase;letter-spacing:0.5px;">' +
                    '<i class="fas fa-exclamation-triangle"></i> ' +
                    (_('gguf.residual_downloads') || 'Residual files (orphans)') +
                '</h5>' +
                '<div style="font-size:var(--font-size-xs);color:var(--text-muted);margin-bottom:8px;">' +
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
                '<div style="font-family:monospace;font-size:var(--font-size-xs);overflow:hidden;text-overflow:ellipsis;white-space:nowrap;" title="' + Utils.escapeHtml(o.path || o.filename) + '">' +
                    Utils.escapeHtml(o.filename) +
                '</div>' +
                '<div style="font-size:var(--font-size-xs);color:var(--text-muted);">' +
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
                (finalPath ? '<div class="gguf-download-paths" style="font-size:var(--font-size-xs);color:var(--text-muted);margin-top:2px;line-height:1.4;">' +
                    '<i class="fas fa-folder-open" style="margin-right:3px;"></i>' + Utils.escapeHtml(finalPath) +
                    (tempPath ? '<br><i class="fas fa-file-download" style="margin-right:3px;"></i><span style="opacity:0.8;">temp: </span>' + Utils.escapeHtml(tempPath) : '') +
                  '</div>' : '') +
                (resumable ? '<div style="font-size:var(--font-size-xs);color:var(--warning);margin-top:2px;"><i class="fas fa-pause-circle"></i> ' +
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
                'style="margin:4px 4px 0 0;padding:4px 10px;border-radius:14px;border:1px solid var(--border);background:var(--bg-secondary);cursor:pointer;font-size:var(--font-size-xs);">' +
                Utils.escapeHtml(s.label) + '</button>';
        }).join('');
        var directHtml = direct.map(function (d) {
            return '<button class="gguf-direct-chip" data-direct="' + Utils.escapeHtml(d.q) + '" ' +
                'style="display:block;width:100%;text-align:left;margin:4px 0;padding:6px 10px;border-radius:6px;border:1px solid var(--border);background:var(--bg-secondary);cursor:pointer;font-family:monospace;font-size:var(--font-size-xs);">' +
                Utils.escapeHtml(d.label) + '</button>';
        }).join('');
        return '<div class="gguf-empty-state gguf-search-empty" style="padding:12px 4px;">' +
            '<div style="display:flex;align-items:center;gap:8px;margin-bottom:6px;">' +
                '<i class="fab fa-huggingface" style="font-size:var(--font-size-lg);color:#ff9a00;"></i>' +
                '<span style="font-weight:500;">' + Utils.escapeHtml(_('gguf.search_title') || 'HuggingFace Hub') + '</span>' +
            '</div>' +
            '<div style="color:var(--text-muted);font-size:var(--font-size-xs);margin-bottom:8px;">' +
                Utils.escapeHtml(_('gguf.search_hint') || 'Type 2+ chars to search, or paste an org/model id (with "/") to jump straight to its files.') +
            '</div>' +
            '<div style="margin-top:10px;">' +
                '<div style="font-size:var(--font-size-xs);color:var(--text-muted);text-transform:uppercase;letter-spacing:0.5px;margin-bottom:4px;">' +
                    Utils.escapeHtml(_('gguf.popular') || 'Popular') +
                '</div>' +
                '<div>' + suggHtml + '</div>' +
            '</div>' +
            '<div style="margin-top:12px;">' +
                '<div style="font-size:var(--font-size-xs);color:var(--text-muted);text-transform:uppercase;letter-spacing:0.5px;margin-bottom:4px;">' +
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
                '<i class="fas fa-exclamation-triangle" style="color:var(--warning,#e0a030);font-size:var(--font-size-lg);"></i>' +
                '<div style="margin-top:6px;font-weight:500;">' + Utils.escapeHtml(state.hfLastError.title) + '</div>' +
                '<div style="margin-top:4px;color:var(--text-muted);font-size:var(--font-size-xs);">' + Utils.escapeHtml(state.hfLastError.hint) + '</div>' +
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
                '<div style="margin-top:4px;color:var(--text-muted);font-size:var(--font-size-xs);">' + Utils.escapeHtml(_('gguf.try_short_query') || 'Try a shorter query, e.g. "Qwen", "Llama-3", "gemma-2-9b"') + '</div>' +
            '</div>';
        }
        var filtered = resultsArr.filter(function (m) {
            return m && m.files && Array.isArray(m.files) && m.files.length > 0;
        });
        if (filtered.length === 0) {
            return '<div class="gguf-empty-state">' +
                '<i class="fas fa-box-open" style="opacity:0.4;"></i> ' +
                '<div style="margin-top:6px;">' + Utils.escapeHtml(_('gguf.no_gguf_files_found')) + '</div>' +
                '<div style="margin-top:4px;color:var(--text-muted);font-size:var(--font-size-xs);">' +
                Utils.escapeHtml(_('gguf.no_gguf_files_hint') || 'Repository exists but contains no .gguf files (maybe GGUF is in a different repo or branch).') +
                '</div>' +
            '</div>';
        }
        var toolbar = filtered.length > 1
            ? '<div class="gguf-results-toolbar" style="display:flex;justify-content:space-between;align-items:center;gap:8px;margin-bottom:8px;">' +
                '<span class="gguf-results-count" style="font-size:var(--font-size-xs);color:var(--text-muted);">' +
                    Utils.escapeHtml(_('gguf.results_count', { n: filtered.length }) || (filtered.length + ' models')) +
                '</span>' +
                '<button class="btn btn-sm btn-secondary gguf-collapse-all-btn" title="' + (_('gguf.collapse_all') || 'Collapse all') + '">' +
                    '<i class="fas fa-compress-arrows-alt"></i> ' + (_('gguf.collapse_all') || 'Collapse all') +
                '</button>' +
              '</div>'
            : '<div class="gguf-results-toolbar" style="display:flex;justify-content:flex-end;margin-bottom:8px;">' +
                '<span class="gguf-results-count" style="font-size:var(--font-size-xs);color:var(--text-muted);margin-right:auto;">' +
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
                        '<span style="flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-family:monospace;font-size:var(--font-size-xs);" title="' + Utils.escapeHtml(fname) + '">' + Utils.escapeHtml(fname) + '</span>' +
                        '<span class="badge" style="background:var(--accent);font-size:var(--font-size-xs);">' + Utils.escapeHtml(fquant) + '</span>' +
                        '<span style="font-size:var(--font-size-xs);color:var(--text-muted);min-width:60px;text-align:right;">' + fsize + '</span>' +
                        (isRecommended ? '<span class="badge" style="background:var(--success);font-size:var(--font-size-xs);" title="' + (_('gguf.recommended') || 'Recommended') + '">' + (_('gguf.recommended') || '★') + '</span>' : '') +
                        '<button class="btn btn-sm btn-primary gguf-download-file-btn" data-model-idx="' + idx + '" data-filename="' + Utils.escapeHtml(fname) + '" title="' + (_('gguf.download') || 'Download') + ': ' + Utils.escapeHtml(fname) + '" style="padding:2px 8px;font-size:var(--font-size-xs);">' +
                            '<i class="fas fa-download"></i>' +
                        '</button>' +
                        '<div class="gguf-file-progress" data-model-idx="' + idx + '" data-filename="' + Utils.escapeHtml(fname) + '" style="display:none;flex-basis:100%;margin-top:4px;">' +
                            '<div class="gguf-progress-bar" style="height:4px;background:var(--bg-tertiary,#333);border-radius:2px;overflow:hidden;">' +
                                '<div class="gguf-progress-fill" style="height:100%;width:0%;background:var(--success);transition:width 0.2s;"></div>' +
                            '</div>' +
                            '<div class="gguf-progress-text" style="font-size:var(--font-size-xs);color:var(--text-muted);margin-top:2px;">0%</div>' +
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
                            'style="display:block;width:100%;padding:6px;margin-top:4px;background:transparent;border:1px dashed var(--border);border-radius:4px;text-align:center;font-size:var(--font-size-xs);color:var(--accent);cursor:pointer;">' +
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
                '<h4 style="margin:0;font-size:var(--font-size-base);"><i class="fas fa-file"></i> ' + Utils.escapeHtml(modelId) + '</h4>' +
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
    // R657 (2026-11-20): таб Settings, RPC-состояние/пиры и редакторы параметров
    // модели вынесены в gguf-detail-settings.js (фабрика).
    var ggufDetailSettings = window.GgufDetailSettings({
        M: M,
        _: _,
        state: state,
        GGUF_VERIFIED_SPLIT_MODES: GGUF_VERIFIED_SPLIT_MODES,
        GGUF_VERIFIED_RPC_BACKENDS: GGUF_VERIFIED_RPC_BACKENDS,
        Utils: Utils,
        I18N: (typeof window !== 'undefined' ? window.I18N : undefined),
        PARAM_SOURCE_LABELS: PARAM_SOURCE_LABELS,
        formatElapsedShort: formatElapsedShort
    });
    var loadAndRenderBackendOptions = ggufDetailSettings.loadAndRenderBackendOptions;
    var applyModelParams = ggufDetailSettings.applyModelParams;
    var measureLinkSpeed = ggufDetailSettings.measureLinkSpeed;
    var saveBackendOptions = ggufDetailSettings.saveBackendOptions;
    var promoteBackendOptionsToDefaults = ggufDetailSettings.promoteBackendOptionsToDefaults;
    var renderSettingsPane = ggufDetailSettings.renderSettingsPane;
    var syncLoadOptionsFromConfig = ggufDetailSettings.syncLoadOptionsFromConfig;
    var markLoadOptionsCustom = ggufDetailSettings.markLoadOptionsCustom;
    var renderRpcServerState = ggufDetailSettings.renderRpcServerState;
    var paramFact = ggufDetailSettings.paramFact;
    var paramOrigin = ggufDetailSettings.paramOrigin;
    var loadedParamsFor = ggufDetailSettings.loadedParamsFor;
    var formatParamValue = ggufDetailSettings.formatParamValue;
    var inheritedValueOf = ggufDetailSettings.inheritedValueOf;
    var inheritedLabel = ggufDetailSettings.inheritedLabel;
    var applyInheritedLabels = ggufDetailSettings.applyInheritedLabels;
    var paramInput = ggufDetailSettings.paramInput;
    var renderModelParamsEditor = ggufDetailSettings.renderModelParamsEditor;
    var hydrateModelParamEditors = ggufDetailSettings.hydrateModelParamEditors;
    var windowLimitNote = ggufDetailSettings.windowLimitNote;
    var modelEffectiveEntry = ggufDetailSettings.modelEffectiveEntry;
    var paintWindowLimit = ggufDetailSettings.paintWindowLimit;
    var applyInheritedDefaults = ggufDetailSettings.applyInheritedDefaults;
    var hydrateOnOpen = ggufDetailSettings.hydrateOnOpen;
    var fillModelParamEditor = ggufDetailSettings.fillModelParamEditor;
    var collectModelParams = ggufDetailSettings.collectModelParams;
    var collapseSections = ggufDetailSettings.collapseSections;
    var renderRpcPeers = ggufDetailSettings.renderRpcPeers;
    var renderBackendOptionsForm = ggufDetailSettings.renderBackendOptionsForm;
    var parseFloatOr = ggufDetailSettings.parseFloatOr;
    var parseStr = ggufDetailSettings.parseStr;
    var parseIntOr = ggufDetailSettings.parseIntOr;
    var mountProfilesInSettings = ggufDetailSettings.mountProfilesInSettings;
    var renderConnectModal = ggufDetailSettings.renderConnectModal;
    var MODEL_PARAM_SPECS = ggufDetailSettings.MODEL_PARAM_SPECS;

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
    // R708: раскладка KV в карточке модели на диске (чистые функции — их
    // проверяет node-тест gguf-kv-plan-r708.test.js).
    M.kvPlanFacts = kvPlanFacts;
    M.kvPlanLineText = kvPlanLineText;
    M.kvPlanLineTitle = kvPlanLineTitle;
    M.renderKVPlanLine = renderKVPlanLine;
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
    // R319 (2026-10-30): «чем ограничено окно» — чистая функция + отрисовка.
    M.windowLimitNote = windowLimitNote;
    M.modelEffectiveEntry = modelEffectiveEntry;
    M.paintWindowLimit = paintWindowLimit;
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
