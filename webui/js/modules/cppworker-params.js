/**
 * cppworker-params.js — Per-Model Profile manager для настроек llama.cpp
 * OllamaLegion WebUI (Session 15 — Q3 W3-4 «Models tab gaps» sub-task «Model profiles UI»)
 *
 * Возможности:
 *  - GET    /api/v1/cppworker/model-profiles         — список всех профилей
 *  - GET    /api/v1/cppworker/model-profiles/{name}  — профиль одной модели
 *  - PUT    /api/v1/cppworker/model-profiles/{name}  — создать/обновить
 *  - DELETE /api/v1/cppworker/model-profiles/{name}  — удалить
 *  - POST   /api/v1/cppworker/model-profiles/{name}/apply — save + reload
 *
 * UI:
 *  - список профилей с метаданными (modelName, contextLength, notes, timeouts)
 *  - wizard с пресетами 4K/8K/16K/32K/64K/128K/256K/Custom
 *  - slider 256..262144 (для gemma-4 — 256K)
 *  - advanced секция: flash_attn / numa / use_mmap (флажки с 3 состояниями inherit/on/off)
 *  - per-model timeouts (collapsed по умолчанию)
 *  - reload progress modal с шагами и результатом по бэкендам
 *  - refresh button рядом со списком
 *
 * Использует:
 *   - window.Api.cppworkerModelProfiles — для HTTP (Q3 W3-4 Session 15)
 *   - window.I18N.t — для переводов (fallback на ru-текст)
 *   - window.showToast — для уведомлений (если есть, иначе alert)
 */
(function () {
    'use strict';

    const PROFILES = window.Api && window.Api.cppworkerModelProfiles;
    const ACTIVE_QUERIES = window.Api && window.Api.cppworkerActiveQueries;
    const APPLY_PROGRESS = window.Api && window.Api.cppworkerApplyProgress;
    const I18N = window.I18N || { t: (k, def) => def || k };

    // Пресеты contextLength (в токенах)
    const CTX_PRESETS = [
        { label: '4K',   value: 4096 },
        { label: '8K',   value: 8192 },
        { label: '16K',  value: 16384 },
        { label: '32K',  value: 32768 },
        { label: '64K',  value: 65536 },
        { label: '128K', value: 131072 },
        { label: '256K', value: 262144 },
        { label: 'Custom', value: 0 }
    ];
    const CTX_MIN = 256;
    const CTX_MAX = 262144; // 256K — для gemma-4

    // Allowed timeout range (сек). 0 = использовать глобальное значение.
    const TIMEOUT_MIN = 0;
    const TIMEOUT_MAX = 24 * 3600; // 24ч максимум

    // Round 32 #11 (2026-08-10): JS port of balancer's EstimateStreamTimeoutFromModelSize
    // / EstimateIdleTimeoutFromModelSize (internal/balancer/model_latency_tracker.go,
    // Round 32 #9 + #11 bumps). Used for placeholder hints in the wizard.
    // ДЕРЖИТЕ В СИНХРОНЕ с Go-кодом — при изменении tier'ов в Go менять и здесь.
    function estimateStreamTimeoutGB(gb) {
        if (gb <= 0) return 0;
        if (gb < 2.0) return 120;
        if (gb < 5.0) return 1800;  // reasoning-capable 4-8B (gemma-4, Qwen3-8B), 30 min
        if (gb < 12.0) return 2400; // reasoning 8-20B, 40 min
        if (gb < 24.0) return 3600; // 20-40B, 60 min
        return 5400;                 // >40B, 90 min
    }
    function estimateIdleTimeoutGB(gb) {
        if (gb <= 0) return 0;
        if (gb < 2.0) return 120;
        if (gb < 5.0) return 600;   // reasoning 4-8B, 10 min pause
        if (gb < 12.0) return 1200; // 8-20B, 20 min
        if (gb < 24.0) return 1800; // 20-40B, 30 min
        return 2400;                 // >40B, 40 min
    }

    // Достаём размер модели из window.CPPWORKER_LOADED_MODELS (если уже загружена)
    // или возвращаем 0 — fallback на "typical 2-5GB" в openWizard.
    function getModelSizeBytesForName(modelName) {
        if (!modelName) return 0;
        try {
            const models = (window.CPPWORKER_LOADED_MODELS && window.CPPWORKER_LOADED_MODELS.models) || [];
            for (const m of models) {
                if (m.name === modelName || (m.path && m.path.indexOf(modelName) >= 0)) {
                    return m.sizeBytes || m.size || 0;
                }
            }
        } catch (e) {
            // ignore — fallback к typical
        }
        return 0;
    }

    // Max gpu layers (верхний предел для slider/number).
    // -2 = AUTO (Round 23, 2026-08-04): cppworker auto-рассчитывает на основе
    //      modelSize, availableVRAM, requestedNCtx. Подробнее cmd/cppworker/inference.go.
    // -1 = все слои на GPU (требует больше VRAM)
    //  0 = CPU-only (модель целиком в RAM через mmap, медленно)
    //  N = явное число слоёв на GPU (1..200)
    const GPU_LAYERS_MIN = -2;
    const GPU_LAYERS_MAX = 200;
    const BATCH_SIZE_MIN = 0;
    const BATCH_SIZE_MAX = 4096;

    /**
     * Загрузить список профилей и отрендерить в #cppProfilesList.
     */
    async function loadAndRender() {
        const listEl = document.getElementById('cppProfilesList');
        if (!listEl) return;

        listEl.innerHTML = '<div class="loading">' + (I18N.t('common.loading', 'Загрузка...')) + '</div>';

        try {
            const data = await PROFILES.list();
            renderProfiles(data.models || {}, listEl);
        } catch (e) {
            listEl.innerHTML = '<div class="cpp-profiles-empty">' + escapeHtml(e.message || String(e)) + '</div>';
        }
    }

    function renderProfiles(models, container) {
        const keys = Object.keys(models).sort();
        if (keys.length === 0) {
            container.innerHTML = '<div class="cpp-profiles-empty" data-i18n="settings.profiles.empty">' +
                escapeHtml(I18N.t('settings.profiles.empty', 'Нет настроенных профилей. Создайте первый, чтобы переопределить n_ctx для конкретной модели.')) +
                '</div>';
            return;
        }
        const html = keys.map(name => renderProfileItem(name, models[name])).join('');
        container.innerHTML = html;

        // Привязываем handlers
        container.querySelectorAll('[data-action]').forEach(btn => {
            btn.addEventListener('click', onProfileAction);
        });
    }

    function renderProfileItem(name, profile) {
        const ctxFormatted = formatCtx(profile.contextLength);
        const meta = [];
        meta.push('n_ctx=' + ctxFormatted);
        if (profile.batchSize && profile.batchSize > 0) meta.push('batch=' + profile.batchSize);
        if (profile.numGpuLayers !== 0 && profile.numGpuLayers !== undefined) {
            // Round 23 (2026-08-04): -2 = AUTO (auto-рассчёт gpu_layers по modelSize+VRAM+ctx)
            let gpuDisplay;
            if (profile.numGpuLayers === -1) gpuDisplay = 'all';
            else if (profile.numGpuLayers === -2) gpuDisplay = 'AUTO';
            else gpuDisplay = profile.numGpuLayers;
            meta.push('gpuLayers=' + gpuDisplay);
        }
        // Boolean overrides (3-state)
        const flags = [];
        if (profile.flashAttn !== null && profile.flashAttn !== undefined) {
            flags.push('flash=' + (profile.flashAttn ? 'on' : 'off'));
        }
        if (profile.numa !== null && profile.numa !== undefined) {
            flags.push('numa=' + (profile.numa ? 'on' : 'off'));
        }
        if (profile.useMmap !== null && profile.useMmap !== undefined) {
            flags.push('mmap=' + (profile.useMmap ? 'on' : 'off'));
        }
        if (flags.length > 0) meta.push('[' + flags.join(' · ') + ']');
        // Session 16 (2026-06-27): Parallel + KVCacheType (advanced fields).
        if (profile.parallel && profile.parallel > 0) meta.push('parallel=' + profile.parallel);
        if (profile.kvCacheType && profile.kvCacheType !== '') meta.push('kv=' + profile.kvCacheType);
        // Per-model timeouts (только non-zero)
        const timeouts = [];
        if (profile.streamingTimeoutSec > 0) timeouts.push('stream=' + profile.streamingTimeoutSec + 's');
        if (profile.streamingIdleTimeoutSec > 0) timeouts.push('idle=' + profile.streamingIdleTimeoutSec + 's');
        if (profile.requestTimeoutSec > 0) timeouts.push('req=' + profile.requestTimeoutSec + 's');
        if (profile.firstByteTimeoutSec > 0) timeouts.push('fb=' + profile.firstByteTimeoutSec + 's');
        if (timeouts.length > 0) meta.push('⏱ ' + timeouts.join(' · '));
        const notes = profile.notes ? ' • ' + escapeHtml(profile.notes) : '';
        return `
            <div class="cpp-profile-item" data-name="${escapeHtml(name)}">
                <div class="cpp-profile-info">
                    <div class="cpp-profile-name">
                        ${escapeHtml(name)}
                        <span class="badge">${ctxFormatted}</span>
                    </div>
                    <div class="cpp-profile-meta">${meta.join(' · ')}${notes}</div>
                </div>
                <div class="cpp-profile-actions">
                    <button class="btn btn-secondary" data-action="edit" data-name="${escapeHtml(name)}" title="${escapeHtml(I18N.t('settings.profiles.edit', 'Редактировать профиль'))}">
                        <i class="fas fa-pen"></i>
                    </button>
                    <button class="btn btn-secondary" data-action="apply" data-name="${escapeHtml(name)}" title="${escapeHtml(I18N.t('settings.profiles.applying', 'Применить (reload)'))}">
                        <i class="fas fa-rotate"></i>
                    </button>
                    <button class="btn btn-danger" data-action="delete" data-name="${escapeHtml(name)}" title="${escapeHtml(I18N.t('common.delete', 'Удалить'))}">
                        <i class="fas fa-trash"></i>
                    </button>
                </div>
            </div>
        `;
    }

    function formatCtx(n) {
        if (!n || n < 0) return '0';
        if (n >= 1024) {
            const k = n / 1024;
            return (k % 1 === 0) ? (k + 'K') : (k.toFixed(1) + 'K');
        }
        return String(n);
    }

    async function onProfileAction(e) {
        const btn = e.currentTarget;
        const action = btn.dataset.action;
        const name = btn.dataset.name;
        if (!action || !name) return;

        try {
            if (action === 'edit') {
                const data = await PROFILES.get(name);
                openWizard(data.model, data.profile);
            } else if (action === 'delete') {
                if (!confirm(I18N.t('settings.profiles.confirm_delete', 'Удалить профиль для') + ' "' + name + '"?')) {
                    return;
                }
                await PROFILES.remove(name);
                showToast('success', I18N.t('settings.profiles.deleted', 'Профиль удалён'));
                await loadAndRender();
            } else if (action === 'apply') {
                await applyProfileWithProgress(name);
            }
        } catch (err) {
            showToast('error', err.message || String(err));
        }
    }

    // ----- Wizard -----

    /**
     * Преобразование профиля из API в payload для UI и обратно.
     * API контракт: {contextLength, batchSize, numGpuLayers, flashAttn?, numa?, useMmap?, notes?,
     *                streamingTimeoutSec?, streamingIdleTimeoutSec?, requestTimeoutSec?, firstByteTimeoutSec?}.
     * flashAttn/numa/useMmap могут быть null (наследовать) или boolean.
     */
    function profileToWizardState(profile) {
        return {
            contextLength: profile.contextLength || 16384,
            batchSize: profile.batchSize || 0,
            numGpuLayers: (profile.numGpuLayers === undefined || profile.numGpuLayers === null) ? 0 : profile.numGpuLayers,
            flashAttn: (profile.flashAttn === null || profile.flashAttn === undefined) ? '' : (profile.flashAttn ? 'on' : 'off'),
            numa: (profile.numa === null || profile.numa === undefined) ? '' : (profile.numa ? 'on' : 'off'),
            useMmap: (profile.useMmap === null || profile.useMmap === undefined) ? '' : (profile.useMmap ? 'on' : 'off'),
            // Session 16 (2026-06-27): Parallel + KVCacheType.
            // parallel — int [0..8], 0 = наследовать cppworker default (1).
            // kvCacheType — "f16"/"q8_0"/"q4_0", "" = наследовать default (F16).
            parallel: (profile.parallel === undefined || profile.parallel === null) ? 0 : profile.parallel,
            kvCacheType: profile.kvCacheType || '',
            notes: profile.notes || '',
            streamingTimeoutSec: profile.streamingTimeoutSec || 0,
            streamingIdleTimeoutSec: profile.streamingIdleTimeoutSec || 0,
            requestTimeoutSec: profile.requestTimeoutSec || 0,
            firstByteTimeoutSec: profile.firstByteTimeoutSec || 0,
            // Round 32 #10 (2026-08-10): reasoning mode toggle. UI-only — не
            // отправляется в API. На save конвертируется в ×3 таймауты.
            reasoningMode: false
        };
    }

    function wizardStateToProfileBody(state) {
        const body = {
            contextLength: state.contextLength,
            batchSize: state.batchSize,
            numGpuLayers: state.numGpuLayers,
            notes: state.notes || ''
        };
        // Bool 3-state: '' = inherit (omit key), 'on' = true, 'off' = false
        if (state.flashAttn === 'on') body.flashAttn = true;
        else if (state.flashAttn === 'off') body.flashAttn = false;
        if (state.numa === 'on') body.numa = true;
        else if (state.numa === 'off') body.numa = false;
        if (state.useMmap === 'on') body.useMmap = true;
        else if (state.useMmap === 'off') body.useMmap = false;
        // Session 16 (2026-06-27): Parallel + KVCacheType.
        // Оба поля опциональные (omit = inherit). Если >0 / != "" — пробрасываем в API.
        if (state.parallel > 0) body.parallel = state.parallel;
        if (state.kvCacheType && state.kvCacheType !== '') body.kvCacheType = state.kvCacheType;
        // Timeouts (только non-zero)
        if (state.streamingTimeoutSec > 0) body.streamingTimeoutSec = state.streamingTimeoutSec;
        if (state.streamingIdleTimeoutSec > 0) body.streamingIdleTimeoutSec = state.streamingIdleTimeoutSec;
        if (state.requestTimeoutSec > 0) body.requestTimeoutSec = state.requestTimeoutSec;
        if (state.firstByteTimeoutSec > 0) body.firstByteTimeoutSec = state.firstByteTimeoutSec;
        return body;
    }

    function openWizard(modelName, profile) {
        const isNew = !profile;
        const state = profileToWizardState(profile || {});

        // Round 32 #10 (2026-08-10): heuristic-based placeholder text для таймаутов.
        // JS port of EstimateStreamTimeoutFromModelSize / EstimateIdleTimeoutFromModelSize
        // (см. internal/balancer/model_latency_tracker.go — Round 32 #9 bumps).
        // Если modelName есть — пробуем достать реальный size из ранее загруженных
        // моделей. Иначе используем "typical 2-5GB" (наиболее частый случай).
        const ggufSizeBytes = getModelSizeBytesForName(modelName);
        const ggufSizeGB = ggufSizeBytes > 0 ? ggufSizeBytes / (1024 * 1024 * 1024) : 3.5;  // default 3.5GB ≈ 2-5GB tier
        const reasoningMult = state.reasoningMode ? 3 : 1;
        const streamAuto = estimateStreamTimeoutGB(ggufSizeGB) * reasoningMult;
        const idleAuto = estimateIdleTimeoutGB(ggufSizeGB) * reasoningMult;
        const streamingTimeoutPlaceholder = state.reasoningMode
            ? '0 = auto (5400s × 3 reasoning) или задайте явно'
            : '0 = auto (heuristic 1800s для 2-5GB, stats)';
        const streamingIdleTimeoutPlaceholder = state.reasoningMode
            ? '0 = auto (1800s × 3 reasoning) или задайте явно'
            : '0 = auto (heuristic 600s для 2-5GB, stats)';
        const requestTimeoutPlaceholder = '0 = global (default 600s)';
        const firstByteTimeoutPlaceholder = '0 = global (default 120s)';

        const overlay = document.createElement('div');
        overlay.className = 'mode-wizard-overlay cpp-profile-wizard';
        overlay.innerHTML = `
            <div class="mode-wizard-modal">
                <div class="wizard-header">
                    <h3>${isNew ? I18N.t('settings.profiles.add', 'Добавить профиль…') : I18N.t('settings.profiles.edit', 'Редактировать профиль')}</h3>
                </div>
                <div class="wizard-desc">
                    ${escapeHtml(I18N.t('settings.profiles.wizard_desc', 'Per-model profile переопределяет n_ctx для конкретной GGUF-модели. Применяется в 3-tier резолвере между body и per-backend default. После сохранения можно применить (reload) на бэкенды через кнопку «Применить» в списке.'))}
                </div>
                <div class="wizard-fields">
                    <div class="wizard-field">
                        <label>${escapeHtml(I18N.t('settings.profiles.model_name', 'Имя модели'))} *</label>
                        <input type="text" id="wizModelName" value="${escapeHtml(modelName || '')}" ${isNew ? '' : 'readonly'} placeholder="gemma-4-E4B-it-Q4_K_M">
                    </div>
                    <div class="wizard-field">
                        <label>${escapeHtml(I18N.t('settings.profiles.n_ctx', 'Context Length (n_ctx)'))} *</label>
                        <div class="ctx-presets" id="wizPresets">
                            ${CTX_PRESETS.map(p => `<button type="button" class="ctx-preset" data-value="${p.value}">${p.label}</button>`).join('')}
                        </div>
                        <div class="ctx-slider-row">
                            <input type="range" id="wizCtxSlider" min="${CTX_MIN}" max="${CTX_MAX}" step="256" value="${state.contextLength}">
                            <span class="ctx-value" id="wizCtxValue">${formatCtx(state.contextLength)}</span>
                        </div>
                        <div class="ctx-help">${escapeHtml(I18N.t('settings.profiles.ctx_help', 'От 256 до 262144 (256K). 256K = gemma-4 max context.'))}</div>
                    </div>
                    <div class="wizard-field">
                        <label>${escapeHtml(I18N.t('settings.profiles.batch_size', 'Batch Size (опционально)'))}</label>
                        <input type="number" id="wizBatchSize" value="${state.batchSize}" min="${BATCH_SIZE_MIN}" max="${BATCH_SIZE_MAX}" placeholder="0 = не задано">
                    </div>
                    <div class="wizard-field">
                        <label>${escapeHtml(I18N.t('settings.profiles.num_gpu_layers', 'Num GPU Layers (опционально)'))}
                            <span id="wizNumGpuLayersBadge" class="gpu-layers-badge is-auto">AUTO</span>
                        </label>
                        <input type="number" id="wizNumGpuLayers" value="${state.numGpuLayers}" min="${GPU_LAYERS_MIN}" max="${GPU_LAYERS_MAX}"
                               placeholder="-2=AUTO, -1=все, 0=CPU only"
                               oninput="Utils.updateGpuLayersBadge('wizNumGpuLayers','wizNumGpuLayersBadge')">
                        <div class="gpu-layers-quick-row">
                            <button type="button" class="btn btn-secondary" data-gpu-value="-2"
                                    onclick="Utils.setGpuLayers(-2,'wizNumGpuLayers','wizNumGpuLayersBadge')">AUTO (-2)</button>
                            <button type="button" class="btn btn-secondary" data-gpu-value="-1"
                                    onclick="Utils.setGpuLayers(-1,'wizNumGpuLayers','wizNumGpuLayersBadge')">All (-1)</button>
                            <button type="button" class="btn btn-secondary" data-gpu-value="0"
                                    onclick="Utils.setGpuLayers(0,'wizNumGpuLayers','wizNumGpuLayersBadge')">CPU (0)</button>
                        </div>
                    </div>
                    <div class="wizard-field">
                        <label>${escapeHtml(I18N.t('settings.profiles.notes', 'Заметки'))}</label>
                        <textarea id="wizNotes" rows="2" placeholder="${escapeHtml(I18N.t('settings.profiles.notes', 'Например: 256K для gemma-4'))}">${escapeHtml(state.notes)}</textarea>
                        <div class="ctx-help">${escapeHtml(I18N.t('settings.profiles.notes_help', 'Свободное описание (назначение, особенности производительности).'))}</div>
                    </div>
                    <div class="wizard-field">
                        <button type="button" class="btn btn-secondary advanced-toggle" id="wizAdvancedToggle">
                            <i class="fas fa-cog"></i> ${escapeHtml(I18N.t('settings.profiles.advanced_toggle_show', 'Показать расширенные'))}
                        </button>
                    </div>
                    <div class="wizard-advanced" id="wizAdvancedSection" style="display:none;">
                        <div class="wizard-field">
                            <label>${escapeHtml(I18N.t('settings.profiles.advanced_section', 'Дополнительно (опциональные override)'))}</label>
                            <div class="ctx-help">${escapeHtml(I18N.t('settings.profiles.advanced_section', 'Дополнительно (опциональные override)'))}</div>
                        </div>
                        <div class="wizard-field">
                            <label>${escapeHtml(I18N.t('settings.profiles.flash_attn', 'Flash Attention'))}</label>
                            <select id="wizFlashAttn">
                                <option value="" ${state.flashAttn === '' ? 'selected' : ''}>— inherit —</option>
                                <option value="on" ${state.flashAttn === 'on' ? 'selected' : ''}>on</option>
                                <option value="off" ${state.flashAttn === 'off' ? 'selected' : ''}>off</option>
                            </select>
                            <div class="ctx-help">${escapeHtml(I18N.t('settings.profiles.flash_attn_help', 'Включить Flash Attention для KV-cache. nil = наследовать дефолт cppworker\'а.'))}</div>
                        </div>
                        <div class="wizard-field">
                            <label>${escapeHtml(I18N.t('settings.profiles.numa', 'NUMA'))}</label>
                            <select id="wizNUMA">
                                <option value="" ${state.numa === '' ? 'selected' : ''}>— inherit —</option>
                                <option value="on" ${state.numa === 'on' ? 'selected' : ''}>on</option>
                                <option value="off" ${state.numa === 'off' ? 'selected' : ''}>off</option>
                            </select>
                            <div class="ctx-help">${escapeHtml(I18N.t('settings.profiles.numa_help', 'NUMA-aware аллокации. Полезно на multi-socket серверах с partial offload.'))}</div>
                        </div>
                        <div class="wizard-field">
                            <label>${escapeHtml(I18N.t('settings.profiles.use_mmap', 'Использовать mmap'))}</label>
                            <select id="wizUseMmap">
                                <option value="" ${state.useMmap === '' ? 'selected' : ''}>— inherit —</option>
                                <option value="on" ${state.useMmap === 'on' ? 'selected' : ''}>on</option>
                                <option value="off" ${state.useMmap === 'off' ? 'selected' : ''}>off</option>
                            </select>
                            <div class="ctx-help">${escapeHtml(I18N.t('settings.profiles.use_mmap_help', 'Memory-map файла модели. false = полностью читать в RAM. nil = наследовать.'))}</div>
                        </div>
                        <div class="wizard-field">
                            <label>${escapeHtml(I18N.t('settings.profiles.parallel', 'Параллельные sequences'))}</label>
                            <input type="number" id="wizParallel" value="${state.parallel}" min="0" max="8" placeholder="0 = default (1)">
                            <div class="ctx-help">${escapeHtml(I18N.t('settings.profiles.parallel_help', 'Число параллельных sequences (batched generation). > 1 требует больше VRAM (KV-cache × parallel). 0 = оставить дефолт cppworker (1).'))}</div>
                        </div>
                        <div class="wizard-field">
                            <label>${escapeHtml(I18N.t('settings.profiles.kv_cache_type', 'KV-cache quantization'))}</label>
                            <select id="wizKVCacheType">
                                <option value="" ${state.kvCacheType === '' ? 'selected' : ''}>— inherit (F16) —</option>
                                <option value="f16" ${state.kvCacheType === 'f16' ? 'selected' : ''}>f16 (default)</option>
                                <option value="q8_0" ${state.kvCacheType === 'q8_0' ? 'selected' : ''}>q8_0 (-50% VRAM)</option>
                                <option value="q4_0" ${state.kvCacheType === 'q4_0' ? 'selected' : ''}>q4_0 (-75% VRAM)</option>
                            </select>
                            <div class="ctx-help">${escapeHtml(I18N.t('settings.profiles.kv_cache_type_help', "Тип квантизации KV-cache. q8_0 экономит ~50% VRAM с минимальной потерей качества. q4_0 экономит ~75%, но заметная потеря на длинных контекстах. '' = default (F16)."))}</div>
                        </div>
                        <div class="wizard-field">
                            <label>${escapeHtml(I18N.t('settings.profiles.timeouts_section', 'Per-model таймауты (сек, 0 = глобальные)'))}</label>
                            <div class="ctx-help">${escapeHtml(I18N.t('settings.profiles.timeout_help', '0 = auto (3-tier resolver: per-model profile → stats из истории → heuristic по размеру GGUF → global config). После 1-й успешной генерации автоподстройка включится и заменит heuristic. > 0 переопределяет только для этой модели.'))}</div>
                        </div>
                        <div class="wizard-field">
                            <label class="checkbox-label">
                                <input type="checkbox" id="wizReasoningMode" ${state.reasoningMode ? 'checked' : ''}>
                                <span>${escapeHtml(I18N.t('settings.profiles.reasoning_mode', 'Reasoning mode (×3 таймаут для thinking моделей)'))}</span>
                            </label>
                            <div class="ctx-help">${escapeHtml(I18N.t('settings.profiles.reasoning_mode_help', 'Включите для моделей с reasoning (gemma-4, Qwen3-Instruct с thinking): эвристический таймаут автоматически умножается на 3 (например, 900s → 2700s). Не трогайте поля ниже — они заполнятся автоматически. Можно потом переопределить вручную.'))}</div>
                        </div>
                        <div class="wizard-field wizard-field-row">
                            <div class="wizard-field-col">
                                <label>${escapeHtml(I18N.t('settings.profiles.streaming_timeout', 'Общий таймаут streaming'))}</label>
                                <input type="number" id="wizStreamingTimeout" value="${state.streamingTimeoutSec}" min="${TIMEOUT_MIN}" max="${TIMEOUT_MAX}" placeholder="${streamingTimeoutPlaceholder}">
                            </div>
                            <div class="wizard-field-col">
                                <label>${escapeHtml(I18N.t('settings.profiles.streaming_idle_timeout', 'Таймаут простоя streaming'))}</label>
                                <input type="number" id="wizStreamingIdleTimeout" value="${state.streamingIdleTimeoutSec}" min="${TIMEOUT_MIN}" max="${TIMEOUT_MAX}" placeholder="${streamingIdleTimeoutPlaceholder}">
                            </div>
                        </div>
                        <div class="wizard-field wizard-field-row">
                            <div class="wizard-field-col">
                                <label>${escapeHtml(I18N.t('settings.profiles.request_timeout', 'Таймаут non-streaming запроса'))}</label>
                                <input type="number" id="wizRequestTimeout" value="${state.requestTimeoutSec}" min="${TIMEOUT_MIN}" max="${TIMEOUT_MAX}" placeholder="${requestTimeoutPlaceholder}">
                            </div>
                            <div class="wizard-field-col">
                                <label>${escapeHtml(I18N.t('settings.profiles.first_byte_timeout', 'Таймаут первого байта'))}</label>
                                <input type="number" id="wizFirstByteTimeout" value="${state.firstByteTimeoutSec}" min="${TIMEOUT_MIN}" max="${TIMEOUT_MAX}" placeholder="${firstByteTimeoutPlaceholder}">
                            </div>
                        </div>
                    </div>
                </div>
                <div class="wizard-footer">
                    <button class="btn btn-secondary" id="wizCancel">${escapeHtml(I18N.t('common.cancel', 'Отмена'))}</button>
                    <button class="btn btn-primary" id="wizSave">${escapeHtml(I18N.t('common.save', 'Сохранить'))}</button>
                </div>
            </div>
        `;
        document.body.appendChild(overlay);
        // Round 35c+ (2026-08-13): init gpu_layers live badge в wizard.
        // Без этого badge показывает "AUTO" (HTML default) даже если state.numGpuLayers = 0.
        if (typeof Utils !== 'undefined' && Utils.updateGpuLayersBadge) {
            Utils.updateGpuLayersBadge('wizNumGpuLayers', 'wizNumGpuLayersBadge');
        }
        // Закрытие по клику на оверлей
        overlay.addEventListener('click', e => {
            if (e.target === overlay) closeWizard();
        });

        // Элементы
        const modelNameInput = overlay.querySelector('#wizModelName');
        const slider = overlay.querySelector('#wizCtxSlider');
        const valueEl = overlay.querySelector('#wizCtxValue');
        const presets = overlay.querySelectorAll('.ctx-preset');
        const advancedToggle = overlay.querySelector('#wizAdvancedToggle');
        const advancedSection = overlay.querySelector('#wizAdvancedSection');

        function updatePresets() {
            const v = parseInt(slider.value, 10);
            presets.forEach(btn => {
                const pv = parseInt(btn.dataset.value, 10);
                btn.classList.toggle('active', pv === v);
            });
            valueEl.textContent = formatCtx(v);
        }
        slider.addEventListener('input', updatePresets);
        presets.forEach(btn => {
            btn.addEventListener('click', () => {
                const v = parseInt(btn.dataset.value, 10);
                if (v === 0) {
                    // Custom — фокус на slider, ничего не делаем
                    slider.focus();
                } else {
                    slider.value = v;
                    updatePresets();
                }
            });
        });
        updatePresets();

        // Advanced toggle (запоминаем состояние)
        advancedToggle.addEventListener('click', () => {
            const isShown = advancedSection.style.display !== 'none';
            advancedSection.style.display = isShown ? 'none' : '';
            advancedToggle.innerHTML = '<i class="fas fa-cog"></i> ' + escapeHtml(
                isShown
                    ? I18N.t('settings.profiles.advanced_toggle_show', 'Показать расширенные')
                    : I18N.t('settings.profiles.advanced_toggle_hide', 'Скрыть расширенные')
            );
        });

        // Handlers кнопок
        overlay.querySelector('#wizCancel').addEventListener('click', closeWizard);
        overlay.querySelector('#wizSave').addEventListener('click', () => onWizardSave(overlay, isNew));

        // Round 32 #10 (2026-08-10): Reasoning mode toggle handler.
        // При включении — обновляем placeholder'ы и автоматически заполняем поля
        // значениями ×3 от heuristic (если они пустые). Пользователь может потом
        // переопределить вручную. Это UX-улучшение для reasoning моделей, которые
        // генерят в 3-5x дольше (gemma-4, Qwen3-Instruct с thinking).
        const reasoningToggle = overlay.querySelector('#wizReasoningMode');
        const streamInput = overlay.querySelector('#wizStreamingTimeout');
        const idleInput = overlay.querySelector('#wizStreamingIdleTimeout');
        function updateReasoningPlaceholders() {
            const isReasoning = reasoningToggle.checked;
            const mult = isReasoning ? 3 : 1;
            const curGb = getModelSizeBytesForName(modelNameInput.value) / (1024*1024*1024) || 3.5;
            const sAuto = estimateStreamTimeoutGB(curGb) * mult;
            const iAuto = estimateIdleTimeoutGB(curGb) * mult;
            streamInput.placeholder = isReasoning
                ? '0 = auto (' + sAuto + 's = heuristic × 3 reasoning) или задайте явно'
                : '0 = auto (heuristic 1800s для 2-5GB, stats)';
            idleInput.placeholder = isReasoning
                ? '0 = auto (' + iAuto + 's = heuristic × 3 reasoning) или задайте явно'
                : '0 = auto (heuristic 600s для 2-5GB, stats)';
        }
        reasoningToggle.addEventListener('change', () => {
            const wasChecked = !reasoningToggle.checked;
            const isNowChecked = reasoningToggle.checked;
            // Если тогглим ON — и поля пустые — заполняем ×3 значениями.
            if (isNowChecked) {
                const curGb = getModelSizeBytesForName(modelNameInput.value) / (1024*1024*1024) || 3.5;
                if (!streamInput.value || parseInt(streamInput.value, 10) === 0) {
                    streamInput.value = String(estimateStreamTimeoutGB(curGb) * 3);
                }
                if (!idleInput.value || parseInt(idleInput.value, 10) === 0) {
                    idleInput.value = String(estimateIdleTimeoutGB(curGb) * 3);
                }
            } else if (wasChecked) {
                // Тогглим OFF — очищаем поля (пользователь может заново задать).
                streamInput.value = '';
                idleInput.value = '';
            }
            updateReasoningPlaceholders();
        });
        // При смене имени модели в isNew-режиме — обновляем placeholder с новым размером.
        if (isNew) {
            modelNameInput.addEventListener('change', updateReasoningPlaceholders);
        }
        updateReasoningPlaceholders();  // initial render

        // Сохранение по Enter
        overlay.addEventListener('keydown', e => {
            if (e.key === 'Enter' && e.target.tagName !== 'BUTTON' && e.target.tagName !== 'SELECT' && e.target.tagName !== 'TEXTAREA') {
                e.preventDefault();
                onWizardSave(overlay, isNew);
            }
        });

        // Фокус на первое поле
        if (isNew) {
            modelNameInput.focus();
        } else {
            slider.focus();
        }
    }

    function closeWizard() {
        const overlay = document.querySelector('.cpp-profile-wizard');
        if (overlay) overlay.remove();
    }

    function readWizardState(overlay) {
        return {
            contextLength: parseInt(overlay.querySelector('#wizCtxSlider').value, 10),
            batchSize: parseInt(overlay.querySelector('#wizBatchSize').value, 10) || 0,
            numGpuLayers: parseInt(overlay.querySelector('#wizNumGpuLayers').value, 10) || 0,
            flashAttn: overlay.querySelector('#wizFlashAttn').value,
            numa: overlay.querySelector('#wizNUMA').value,
            useMmap: overlay.querySelector('#wizUseMmap').value,
            // Session 16 (2026-06-27): Parallel + KVCacheType (advanced fields).
            parallel: parseInt(overlay.querySelector('#wizParallel').value, 10) || 0,
            kvCacheType: overlay.querySelector('#wizKVCacheType').value,
            notes: (overlay.querySelector('#wizNotes').value || '').trim(),
            streamingTimeoutSec: parseInt(overlay.querySelector('#wizStreamingTimeout').value, 10) || 0,
            streamingIdleTimeoutSec: parseInt(overlay.querySelector('#wizStreamingIdleTimeout').value, 10) || 0,
            requestTimeoutSec: parseInt(overlay.querySelector('#wizRequestTimeout').value, 10) || 0,
            firstByteTimeoutSec: parseInt(overlay.querySelector('#wizFirstByteTimeout').value, 10) || 0
        };
    }

    function validateWizardState(state, name) {
        if (!name) {
            showToast('error', I18N.t('settings.profiles.model_name_required', 'Имя модели обязательно'));
            return false;
        }
        if (!state.contextLength || state.contextLength < CTX_MIN || state.contextLength > CTX_MAX) {
            showToast('error', I18N.t('settings.profiles.invalid_n_ctx', 'n_ctx должен быть в [256, 262144]'));
            return false;
        }
        if (state.batchSize < BATCH_SIZE_MIN || state.batchSize > BATCH_SIZE_MAX) {
            showToast('error', 'batchSize должен быть в [0, ' + BATCH_SIZE_MAX + ']');
            return false;
        }
        if (state.numGpuLayers < GPU_LAYERS_MIN || state.numGpuLayers > GPU_LAYERS_MAX) {
            showToast('error', 'numGpuLayers должен быть в [' + GPU_LAYERS_MIN + ', ' + GPU_LAYERS_MAX + ']');
            return false;
        }
        // Session 16 (2026-06-27): Parallel [0..8] + KVCacheType валидация.
        if (state.parallel < 0 || state.parallel > 8) {
            showToast('error', 'parallel должен быть в [0, 8]');
            return false;
        }
        const allowedKV = ['', 'f16', 'q8_0', 'q4_0'];
        if (allowedKV.indexOf(state.kvCacheType) === -1) {
            showToast('error', "kvCacheType должен быть одним из ['', 'f16', 'q8_0', 'q4_0']");
            return false;
        }
        // Timeout валидация
        const timeoutFields = [
            ['streamingTimeoutSec', state.streamingTimeoutSec],
            ['streamingIdleTimeoutSec', state.streamingIdleTimeoutSec],
            ['requestTimeoutSec', state.requestTimeoutSec],
            ['firstByteTimeoutSec', state.firstByteTimeoutSec]
        ];
        for (const [name_, v] of timeoutFields) {
            if (v < TIMEOUT_MIN || v > TIMEOUT_MAX) {
                showToast('error', name_ + ' должен быть в [0, ' + TIMEOUT_MAX + ']');
                return false;
            }
        }
        return true;
    }

    async function onWizardSave(overlay, isNew) {
        const name = (overlay.querySelector('#wizModelName').value || '').trim();
        const state = readWizardState(overlay);

        if (!validateWizardState(state, name)) {
            return;
        }

        const body = wizardStateToProfileBody(state);
        try {
            await PROFILES.upsert(name, body);
            showToast('success', isNew
                ? I18N.t('settings.profiles.created', 'Профиль создан')
                : I18N.t('settings.profiles.updated', 'Профиль обновлён'));
            closeWizard();
            await loadAndRender();
        } catch (err) {
            showToast('error', err.message || String(err));
        }
    }

    // ----- Apply with progress modal (Round 26 v0.5.13) -----
    // Async path: detect busy → 202 + SSE progress → terminal state.
    // Sync path: 200 + final backends list → close modal.

    async function applyProfileWithProgress(modelName) {
        // Создаём modal
        const overlay = document.createElement('div');
        overlay.className = 'mode-wizard-overlay';
        overlay.innerHTML = `
            <div class="mode-wizard-modal" style="max-width:520px;">
                <div class="wizard-header">
                    <h3>${escapeHtml(I18N.t('settings.profiles.applying', 'Применение профиля…'))}</h3>
                </div>
                <div class="wizard-desc cpp-reload-progress" id="reloadProgressBody">
                    <div class="reload-step pending" data-step="save">
                        <span class="reload-step-icon">⏵</span>
                        <span class="reload-step-text">${escapeHtml(I18N.t('settings.profiles.step_save', 'Сохранение профиля…'))}</span>
                    </div>
                    <div class="reload-step pending" data-step="reload">
                        <span class="reload-step-icon">⏵</span>
                        <span class="reload-step-text">${escapeHtml(I18N.t('settings.profiles.step_reload', 'Reload модели на бэкендах…'))}</span>
                    </div>
                    <div class="reload-step pending" data-step="busy" id="busyStep" style="display:none;">
                        <span class="reload-step-icon">⏵</span>
                        <span class="reload-step-text">${escapeHtml(I18N.t('settings.profiles.step_busy', 'Ожидание завершения активных генераций…'))}</span>
                    </div>
                    <div class="reload-progress-bar"><div class="reload-progress-fill" id="reloadProgressFill"></div></div>
                </div>
                <div class="wizard-footer">
                    <button class="btn btn-secondary" id="reloadClose" disabled>${escapeHtml(I18N.t('common.close', 'Закрыть'))}</button>
                </div>
            </div>
        `;
        document.body.appendChild(overlay);

        const closeBtn = overlay.querySelector('#reloadClose');
        const fillEl = overlay.querySelector('#reloadProgressFill');
        const steps = {
            save: overlay.querySelector('[data-step="save"]'),
            reload: overlay.querySelector('[data-step="reload"]'),
            busy: overlay.querySelector('[data-step="busy"]')
        };

        function setStep(name, state) {
            const el = steps[name];
            if (!el) return;
            el.style.display = '';
            el.classList.remove('pending', 'running', 'done', 'error');
            el.classList.add(state);
            const icon = el.querySelector('.reload-step-icon');
            if (state === 'running') icon.textContent = '⟳';
            else if (state === 'done') icon.textContent = '✓';
            else if (state === 'error') icon.textContent = '✗';
            else icon.textContent = '⏵';
        }

        function renderBackendResults(backends) {
            const body = overlay.querySelector('#reloadProgressBody');
            const details = (backends || []).map(b => {
                let icon = '·';
                let cls = 'pending';
                if (b.status === 'reloaded') { icon = '✓'; cls = 'done'; }
                else if (b.status === 'skipped') { icon = '⤳'; cls = 'pending'; }
                else if (b.status === 'error') { icon = '✗'; cls = 'error'; }
                const msg = b.message ? ' — ' + escapeHtml(b.message) : '';
                return `<div class="reload-step ${cls}"><span class="reload-step-icon">${icon}</span><span class="reload-step-text">${escapeHtml(b.backendId)}${msg}</span></div>`;
            }).join('');
            if (details) {
                body.insertAdjacentHTML('beforeend', details);
            } else {
                body.insertAdjacentHTML('beforeend', '<div class="reload-step pending"><span class="reload-step-icon">·</span><span class="reload-step-text">No llama.cpp backends registered</span></div>');
            }
        }

        try {
            // Шаг 1: save+reload в одном вызове
            setStep('save', 'running');
            fillEl.style.width = '20%';

            const resp = await PROFILES.apply(modelName);

            setStep('save', 'done');

            // Если 202 — async path (buggy бэкенд был занят).
            // Используем SSE для live прогресса.
            if (resp._status === 202 && resp.applyId && APPLY_PROGRESS) {
                fillEl.style.width = '30%';
                setStep('busy', 'running');
                steps.busy.querySelector('.reload-step-text').textContent =
                    I18N.t('settings.profiles.step_busy_with_count',
                        'Ожидание завершения активных генераций (initial: ' + (resp.initialActiveQueries || 0) + ')…');

                // Попробуем EventSource (SSE), fallback на polling
                const useSSE = typeof EventSource !== 'undefined';
                if (useSSE) {
                    const es = APPLY_PROGRESS.streamProgress(modelName, resp.applyId, {
                        onProgress: (data) => {
                            if (data.state === 'reloading') {
                                setStep('busy', 'done');
                                setStep('reload', 'running');
                                fillEl.style.width = '70%';
                            }
                            // Update per-backend status
                            if (data.backends) {
                                Object.keys(data.backends).forEach(bid => {
                                    const b = data.backends[bid];
                                    // Find or add a step for this backend
                                    let stepEl = overlay.querySelector('[data-backend-id="' + bid + '"]');
                                    if (!stepEl) {
                                        const html = `<div class="reload-step pending" data-backend-id="${escapeHtml(bid)}">
                                            <span class="reload-step-icon">·</span>
                                            <span class="reload-step-text">${escapeHtml(bid)}</span>
                                        </div>`;
                                        overlay.querySelector('#reloadProgressBody').insertAdjacentHTML('beforeend', html);
                                        stepEl = overlay.querySelector('[data-backend-id="' + bid + '"]');
                                    }
                                    stepEl.classList.remove('pending', 'running', 'done', 'error');
                                    let cls = 'pending';
                                    if (b.status === 'reloaded') cls = 'done';
                                    else if (b.status === 'error') cls = 'error';
                                    else if (b.status === 'pending') cls = 'running';
                                    stepEl.classList.add(cls);
                                    const icon = stepEl.querySelector('.reload-step-icon');
                                    if (b.status === 'reloaded') icon.textContent = '✓';
                                    else if (b.status === 'error') icon.textContent = '✗';
                                    else if (b.status === 'pending') icon.textContent = '⟳';
                                    else icon.textContent = '⏵';
                                    const txt = stepEl.querySelector('.reload-step-text');
                                    txt.textContent = bid + (b.message ? ' — ' + b.message : '');
                                });
                            }
                        },
                        onComplete: (data) => {
                            // Финализация
                            setStep('reload', 'done');
                            fillEl.style.width = '100%';
                            // Replace per-backend steps with final results
                            const body = overlay.querySelector('#reloadProgressBody');
                            body.querySelectorAll('[data-backend-id]').forEach(el => el.remove());
                            renderBackendResults(data.result && data.result.backends);

                            const errors = (data.result && data.result.backends || []).filter(b => b.status === 'error');
                            if (errors.length > 0) {
                                showToast('warning', I18N.t('settings.profiles.applied_with_errors', 'Профиль применён с ошибками на нескольких бэкендах'));
                            } else {
                                showToast('success', I18N.t('settings.profiles.applied', 'Профиль применён'));
                            }
                            closeBtn.disabled = false;
                        },
                        onError: () => {
                            // SSE упал — fallback на polling
                            pollUntilDone(modelName, resp.applyId, overlay, setStep, fillEl, renderBackendResults, closeBtn);
                        }
                    });
                    // Cleanup: close EventSource when overlay closed
                    closeBtn.addEventListener('click', () => { try { es.close(); } catch (_) {} overlay.remove(); });
                } else {
                    // Polling fallback
                    pollUntilDone(modelName, resp.applyId, overlay, setStep, fillEl, renderBackendResults, closeBtn);
                    closeBtn.addEventListener('click', () => overlay.remove());
                }
                return; // async path, close button enabled by callbacks
            }

            // Sync path (200) — все бэкенды были idle, reload сделался в момент запроса
            setStep('reload', 'running');
            fillEl.style.width = '70%';

            renderBackendResults(resp.backends);

            setStep('reload', 'done');
            fillEl.style.width = '100%';

            const errors = (resp.backends || []).filter(b => b.status === 'error');
            if (errors.length > 0) {
                showToast('warning', I18N.t('settings.profiles.applied_with_errors', 'Профиль применён с ошибками на нескольких бэкендах'));
            } else {
                showToast('success', I18N.t('settings.profiles.applied', 'Профиль применён'));
            }
        } catch (err) {
            setStep('save', 'error');
            setStep('reload', 'error');
            showToast('error', err.message || String(err));
        } finally {
            closeBtn.disabled = false;
        }

        closeBtn.addEventListener('click', () => overlay.remove());
    }

    /**
     * Polling fallback если EventSource не работает.
     * Опрос /status каждые 1s пока terminal.
     */
    async function pollUntilDone(modelName, applyId, overlay, setStep, fillEl, renderBackendResults, closeBtn) {
        try {
            const final = await APPLY_PROGRESS.pollStatus(modelName, applyId, 300000);
            setStep('reload', 'done');
            fillEl.style.width = '100%';
            const body = overlay.querySelector('#reloadProgressBody');
            body.querySelectorAll('[data-backend-id]').forEach(el => el.remove());
            renderBackendResults(final.result && final.result.backends);

            const errors = (final.result && final.result.backends || []).filter(b => b.status === 'error');
            if (errors.length > 0) {
                showToast('warning', I18N.t('settings.profiles.applied_with_errors', 'Профиль применён с ошибками'));
            } else {
                showToast('success', I18N.t('settings.profiles.applied', 'Профиль применён'));
            }
        } catch (err) {
            showToast('error', 'Polling failed: ' + (err.message || err));
        } finally {
            closeBtn.disabled = false;
        }
    }

    // ----- Helpers -----

    function escapeHtml(s) {
        if (s == null) return '';
        return String(s)
            .replace(/&/g, '&')
            .replace(/</g, '<')
            .replace(/>/g, '>')
            .replace(/"/g, '"')
            .replace(/'/g, '&#39;');
    }

    function showToast(kind, message) {
        if (typeof window.showToast === 'function') {
            window.showToast(kind, message);
            return;
        }
        if (kind === 'error') console.error(message);
        else console.log(message);
        // Fallback
        const container = document.getElementById('toastContainer');
        if (!container) return;
        const t = document.createElement('div');
        t.className = 'toast ' + kind;
        t.textContent = message;
        container.appendChild(t);
        setTimeout(() => t.remove(), 5000);
    }

    // ----- Init -----

    function init() {
        const addBtn = document.getElementById('cppProfileAddBtn');
        if (addBtn && !addBtn._ggufBound) {
            addBtn.addEventListener('click', () => openWizard('', null));
        }
        // Загружаем при заходе на секцию (когда settings открывается)
        const section = document.getElementById('section-llama-cpp');
        if (section) {
            // Mutation observer на display — перезагружаем список при показе
            const observer = new MutationObserver(() => {
                if (section.style.display !== 'none') {
                    loadAndRender();
                }
            });
            observer.observe(section, { attributes: true, attributeFilter: ['style'] });
            // Первичная загрузка, если секция уже видима
            if (section.style.display !== 'none') {
                loadAndRender();
            }
        }
    }

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', init);
    } else {
        init();
    }

    // Экспорт для тестов и debugging
    window.CppWorkerParams = {
        loadAndRender,
        openWizard,
        applyProfileWithProgress,
        profileToWizardState,
        wizardStateToProfileBody,
        CTX_PRESETS,
        CTX_MIN,
        CTX_MAX
    };
})();