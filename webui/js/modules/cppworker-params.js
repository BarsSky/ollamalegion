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
 *  - список профилей с метаданными (modelName, contextLength, notes, reasoning)
 *  - wizard с пресетами 4K/8K/16K/32K/64K/128K/256K/Custom
 *  - slider 256..262144 (для gemma-4 — 256K)
 *  - advanced секция: flash_attn / numa / use_mmap (флажки с 3 состояниями inherit/on/off)
 *  - per-model timeouts УБРАНЫ из формы (R106c): работа идёт по состоянию
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
    // R83 (2026-09-30): «доступные настройки загрузки» + список моделей в папке.
    const LOAD_DEFAULTS = window.Api && window.Api.cppworkerLoadDefaults;
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

    // R106c (2026-10-12): ЗДЕСЬ БЫЛИ функции estimateStreamTimeoutGB /
    // estimateIdleTimeoutGB / getModelSizeBytesForName и константы TIMEOUT_MIN/MAX.
    // Они существовали только для подсказок и автозаполнения per-model ТАЙМАУТОВ,
    // а сами поля таймаутов убраны из формы по договорённости с оператором:
    // работа идёт ПО СОСТОЯНИЮ (загрузка → готово → ошибка), а не по «эфимерному
    // времени». Таймаут ничего не гарантирует, зато молча обрывает длинную
    // генерацию; при ошибке UI реагирует сразу, а не ждёт N секунд.

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
     *
     * R83 (2026-09-30): вместе с профилями рисуем два новых блока —
     *   * #cppLoadDefaults  — «настройки по умолчанию для новых моделей»
     *     (редактируемый конфиг инициализации + источник каждого значения);
     *   * #cppModelCatalog  — модели, лежащие в папке (в том числе ещё не
     *     загруженные) с эффективными настройками и кнопкой «Настроить».
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

        // Блоки ниже независимы: их падение не должно ломать список профилей.
        loadAndRenderDefaults();
        loadAndRenderCatalog();
    }

    // ----- Настройки по умолчанию для новых моделей -----

    // R106b (2026-10-12): блок ищется и по id (страница «Настройки»), и по
    // data-role. ЗАЧЕМ: те же блоки нужны на странице GGUF во вкладке настроек
    // бэкенда (там оператор и настраивает загрузку). Дублировать id нельзя —
    // document.getElementById вернул бы первый (скрытый) и блок молча не
    // отрисовался бы; поэтому второе размещение помечается data-role.
    function findTargets(id, role) {
        const out = [];
        const byId = document.getElementById(id);
        if (byId) out.push(byId);
        if (document.querySelectorAll) {
            const list = document.querySelectorAll('[data-role="' + role + '"]') || [];
            for (let i = 0; i < list.length; i++) {
                if (list[i] && out.indexOf(list[i]) === -1) out.push(list[i]);
            }
        }
        return out;
    }

    async function loadAndRenderDefaults() {
        const targets = findTargets('cppLoadDefaults', 'cpp-load-defaults');
        if (targets.length === 0) return;
        if (!LOAD_DEFAULTS) {
            targets.forEach(function (t) { t.innerHTML = ''; });
            return;
        }
        const loading = '<div class="loading">' + escapeHtml(I18N.t('common.loading', 'Загрузка...')) + '</div>';
        targets.forEach(function (t) { t.innerHTML = loading; });
        try {
            const data = await LOAD_DEFAULTS.get();
            targets.forEach(function (t) { renderLoadDefaults(data || {}, t); });
        } catch (e) {
            const msg = '<div class="cpp-profiles-empty">' + escapeHtml(e.message || String(e)) + '</div>';
            targets.forEach(function (t) { t.innerHTML = msg; });
        }
    }

    const SOURCE_LABEL = {
        defaultProfile: 'настройки по умолчанию',
        'cppworker-env': 'env cppworker',
        none: 'не задано'
    };

    function sourceBadge(src) {
        const map = {
            defaultProfile: ['is-default', I18N.t('settings.load_defaults.source_default', 'настройки по умолчанию')],
            'cppworker-env': ['is-env', I18N.t('settings.load_defaults.source_env', 'env контейнера')],
            none: ['is-none', I18N.t('settings.load_defaults.source_none', 'не задано')]
        };
        const [cls, label] = map[src] || map.none;
        return '<span class="cpp-source-badge ' + cls + '">' + escapeHtml(label) + '</span>';
    }

    function fmtValue(key, v) {
        if (v === null || v === undefined || v === '') return '—';
        if (key === 'numGpuLayers') {
            if (v === -2) return 'AUTO';
            if (v === -1) return I18N.t('settings.load_defaults.all_layers', 'все слои');
            if (v === 0) return 'CPU';
        }
        if (key === 'flashAttn') {
            if (v === -1) return 'auto';
            if (v === 0) return 'off';
            if (v === 1) return 'on';
        }
        // R106b (2026-10-12): для reasoning — слова, а не «on/off»: это то
        // значение, которое оператор искал в списке и не находил («галочка не
        // применяется»). Для mmap оставляем прежний компактный on/off.
        if (key === 'enableReasoning') {
            return v ? I18N.t('settings.load_defaults.reasoning_on', 'включено')
                     : I18N.t('settings.load_defaults.reasoning_off', 'выключено');
        }
        if (key === 'useMmap') return v ? 'on' : 'off';
        return String(v);
    }

    const DEFAULTS_FIELDS = [
        ['contextLength', 'n_ctx (окно контекста, СУММАРНОЕ)'],
        ['contextPerSlot', 'n_ctx на клиента (llama.cpp: n_ctx / слоты)'],
        ['batchSize', 'batch size'],
        ['numGpuLayers', 'слоёв на GPU (-1 = все, -2 = AUTO)'],
        ['kvCacheType', 'KV-cache (f16/q8_0/q4_0)'],
        ['parallel', 'параллельных слотов'],
        ['flashAttn', 'flash attention (-1 = auto)'],
        ['useMmap', 'mmap'],
        ['enableReasoning', 'reasoning по умолчанию']
    ];

    // lowPerSlotWarning — R83 (2026-09-30): окно на клиента < 1024 — почти всегда
    // ошибка оператора (он думал, что contextLength и есть окно на клиента).
    function lowPerSlotWarning(eff) {
        const per = eff && eff.contextPerSlot;
        if (!per || per >= 1024) return '';
        return '<div class="cpp-profiles-hint" style="color:var(--warning,#d29922);">' +
            escapeHtml(I18N.t('settings.load_defaults.low_per_slot',
                'Внимание: на каждого клиента приходится меньше 1024 токенов — это окно делится между слотами (n_ctx / параллельные слоты). Чтобы дать 16K каждому при 2 слотах, задайте суммарное окно 32768 или укажите «окно на клиента».')) +
            '</div>';
    }

    function renderLoadDefaults(data, el) {
        let eff = data.effective || {};
        let src = data.source || {};
        const cw = data.cppworker || {};
        const prof = data.defaultModelProfile || null;

        // R106b (2026-10-12): страховка по reasoning. Сервер (до обновления
        // 0.7.67) строил effective.enableReasoning ТОЛЬКО из env контейнера и не
        // читал одноимённое поле профиля. Тогда сразу после сохранения «включено»
        // список показывал «off (env контейнера)» — оператор справедливо считал,
        // что галочка не применяется (живая жалоба). Значение самого профиля
        // приходит в ответе всегда, поэтому показываем его как источник
        // «настройки по умолчанию», а не врём про env.
        if (prof && typeof prof.enableReasoning === 'boolean' && src.enableReasoning !== 'defaultProfile') {
            eff = Object.assign({}, eff, { enableReasoning: prof.enableReasoning });
            src = Object.assign({}, src, { enableReasoning: 'defaultProfile' });
        }

        const rows = DEFAULTS_FIELDS.map(([key, label]) => {
            return '<tr>' +
                '<td>' + escapeHtml(label) + '</td>' +
                '<td class="cpp-defaults-value">' + escapeHtml(fmtValue(key, eff[key])) + '</td>' +
                '<td>' + sourceBadge(src[key]) + '</td>' +
                '</tr>';
        }).join('');

        const cwState = cw.available
            ? '<span class="cpp-source-badge is-env">' + escapeHtml(cw.backendId || '') + '</span>'
            : '<span class="cpp-source-badge is-none">' + escapeHtml(cw.error || I18N.t('settings.load_defaults.cppworker_unavailable', 'недоступен')) + '</span>';

        // ignoreDefaults на дефолтном профиле = «ничего не подставлять, пусть
        // решает env контейнера» — тот же принцип, что у per-model профилей.
        const ignored = !!(prof && prof.ignoreDefaults);
        const ignoredNote = ignored
            ? '<div class="cpp-profiles-hint"><span class="cpp-source-badge is-env">' +
              escapeHtml(I18N.t('settings.load_defaults.ignored', 'настройки по умолчанию отключены (ignoreDefaults) — значения берутся из env контейнера')) +
              '</span></div>'
            : '';

        el.innerHTML = `
            <div class="cpp-defaults-head">
                <div>
                    <strong>${escapeHtml(I18N.t('settings.load_defaults.title', 'Настройки по умолчанию для новых моделей'))}</strong>
                    <div class="cpp-profiles-hint">${escapeHtml(I18N.t('settings.load_defaults.hint',
                        'Применяются к модели, у которой нет своего профиля. Профиль модели всегда важнее; если ничего не задано — берётся env контейнера cppworker.'))}</div>
                    <div class="cpp-profiles-hint">${escapeHtml(I18N.t('settings.load_defaults.cppworker_label', 'env cppworker:'))} ${cwState}</div>
                    ${ignoredNote}
                    ${lowPerSlotWarning(eff)}
                </div>
                <div>
                    <button class="btn btn-primary" id="cppDefaultsEditBtn">${escapeHtml(I18N.t('common.edit', 'Изменить'))}</button>
                </div>
            </div>
            <table class="cpp-defaults-table"><tbody>${rows}</tbody></table>
            <div class="cpp-defaults-form-host" data-role="cpp-defaults-form"></div>
        `;

        const btn = el.querySelector('#cppDefaultsEditBtn');
        if (btn) btn.addEventListener('click', () => openDefaultsForm(data, el));
    }

    // openDefaultsForm — компактная форма правки конфига инициализации.
    // Пустое поле = «не задавать» (тогда работает env cppworker).
    //
    // R106b: hostEl — контейнер блока (у блока может быть несколько размещений:
    // страница «Настройки» и вкладка настроек бэкенда на странице GGUF).
    // Форма ищется ВНУТРИ своего контейнера, поэтому id не конфликтуют.
    function openDefaultsForm(data, hostEl) {
        const box = hostEl
            ? hostEl.querySelector('[data-role="cpp-defaults-form"]')
            : document.getElementById('cppDefaultsForm');
        if (!box) return;
        const prof = data.defaultModelProfile || {};
        const eff = data.effective || {};
        const num = (v) => (v === null || v === undefined) ? '' : String(v);
        const field = (id, label, value, attrs) =>
            '<label class="cpp-defaults-field"><span>' + escapeHtml(label) + '</span>' +
            '<input class="form-control" id="' + id + '" type="number" value="' + escapeHtml(num(value)) + '" ' + (attrs || '') + '></label>';
        // R106b (2026-10-12): reasoning — единственное поле настроек, которое
        // имеет три состояния, а не число: «не задавать» / «включено» /
        // «выключено». Раньше его в форме не было ВООБЩЕ, поэтому оператор не мог
        // включить размышления для новых моделей (жалоба «не дало поменять
        // параметры размышления с выключенного на включенный»). Значение читаем из
        // профиля (там *bool), а не из effective — иначе «не задавать» нельзя
        // было бы отличить от «выключено».
        const reasoningState = (prof.enableReasoning === true) ? 'on'
            : (prof.enableReasoning === false ? 'off' : '');
        const reasoningSelect =
            '<label class="cpp-defaults-field"><span>' +
                escapeHtml(I18N.t('settings.load_defaults.reasoning', 'размышления (reasoning/thinking)')) +
            '</span>' +
            '<select class="form-control" id="cppDefReasoning">' +
                '<option value=""' + (reasoningState === '' ? ' selected' : '') + '>' +
                    escapeHtml(I18N.t('settings.load_defaults.reasoning_inherit', '— не задавать (решит env контейнера) —')) + '</option>' +
                '<option value="on"' + (reasoningState === 'on' ? ' selected' : '') + '>' +
                    escapeHtml(I18N.t('settings.load_defaults.reasoning_on', 'включено')) + '</option>' +
                '<option value="off"' + (reasoningState === 'off' ? ' selected' : '') + '>' +
                    escapeHtml(I18N.t('settings.load_defaults.reasoning_off', 'выключено')) + '</option>' +
            '</select>' +
            '<span class="cpp-defaults-hint">' +
                escapeHtml(I18N.t('settings.load_defaults.reasoning_help',
                    'Применяется к новым моделям БЕЗ своего профиля. У модели со своим профилем значение берётся из него (секция «Профили моделей»), поэтому для конкретной модели правьте её профиль.')) +
            '</span></label>';

        box.innerHTML = `
            <div class="cpp-defaults-form">
                <div class="cpp-defaults-grid">
                    ${field('cppDefCtx', 'n_ctx суммарное (слоты делят его)', prof.contextLength !== undefined ? prof.contextLength : eff.contextLength, 'min="256" max="262144"')}
                    ${field('cppDefPerSeq', 'окно НА КЛИЕНТА (0 = не задавать)', prof.contextPerSeq, 'min="0" max="262144"')}
                    ${field('cppDefBatch', 'batch size', prof.batchSize !== undefined ? prof.batchSize : eff.batchSize, 'min="0" max="4096"')}
                    ${field('cppDefGpuLayers', 'слоёв на GPU', prof.numGpuLayers, 'min="-2" max="200"')}
                    ${field('cppDefParallel', 'параллельных слотов', prof.parallel !== undefined ? prof.parallel : eff.parallel, 'min="0" max="8"')}
                    <label class="cpp-defaults-field"><span>KV-cache</span>
                        <select class="form-control" id="cppDefKv">
                            <option value=""${!prof.kvCacheType ? ' selected' : ''}>— не задавать (env) —</option>
                            <option value="f16"${prof.kvCacheType === 'f16' ? ' selected' : ''}>f16</option>
                            <option value="q8_0"${prof.kvCacheType === 'q8_0' ? ' selected' : ''}>q8_0</option>
                            <option value="q4_0"${prof.kvCacheType === 'q4_0' ? ' selected' : ''}>q4_0</option>
                        </select>
                    </label>
                    ${field('cppDefFlash', 'flash attention (-1 auto, 0 off, 1 on)', prof.flashAttn === true ? 1 : (prof.flashAttn === false ? 0 : ''), 'min="-1" max="1"')}
                    ${reasoningSelect}
                </div>
                <div class="cpp-profiles-hint">${escapeHtml(I18N.t('settings.load_defaults.form_hint',
                    'Пустое поле = не задавать (тогда значение берётся из env контейнера cppworker). Сохранение пишет значения в конфиг балансера.'))}</div>
                <div class="cpp-profiles-hint">${escapeHtml(I18N.t('settings.load_defaults.perslot_hint',
                    'n_ctx — СУММАРНОЕ окно: llama.cpp делит его между слотами (n_ctx_seq = n_ctx / слоты). Чтобы каждый клиент получил, например, 16K при 2 слотах, задайте суммарное 32768 или впишите 16384 в «окно НА КЛИЕНТА» — тогда суммарное посчитается само (16384 × 2 = 32768).'))}</div>
                <label class="cpp-defaults-field" style="flex-direction:row; align-items:center; gap:8px; margin-top:8px;">
                    <input type="checkbox" id="cppDefIgnore"${prof.ignoreDefaults ? ' checked' : ''}>
                    <span>${escapeHtml(I18N.t('settings.load_defaults.ignore_defaults',
                        'Не подставлять эти значения (ignoreDefaults) — всё берётся из env контейнера cppworker'))}</span>
                </label>
                <div class="cpp-defaults-actions">
                    <button class="btn btn-primary" id="cppDefaultsSave">${escapeHtml(I18N.t('common.save', 'Сохранить'))}</button>
                    <button class="btn btn-secondary" id="cppDefaultsCancel">${escapeHtml(I18N.t('common.cancel', 'Отмена'))}</button>
                </div>
            </div>
        `;

        box.querySelector('#cppDefaultsCancel').addEventListener('click', () => { box.innerHTML = ''; });
        box.querySelector('#cppDefaultsSave').addEventListener('click', async () => {
            const body = {};
            const intOrSkip = (id, key) => {
                const raw = (box.querySelector('#' + id).value || '').trim();
                if (raw === '') return;
                const n = parseInt(raw, 10);
                if (!isNaN(n)) body[key] = n;
            };
            intOrSkip('cppDefCtx', 'contextLength');
            // R83 (2026-09-30): окно НА КЛИЕНТА — 0/пусто = не задавать
            // (тогда contextLength трактуется как суммарное).
            const perRaw = (box.querySelector('#cppDefPerSeq').value || '').trim();
            const perVal = perRaw === '' ? 0 : parseInt(perRaw, 10);
            if (!isNaN(perVal) && perVal > 0) body.contextPerSeq = perVal;
            intOrSkip('cppDefBatch', 'batchSize');
            intOrSkip('cppDefGpuLayers', 'numGpuLayers');
            intOrSkip('cppDefParallel', 'parallel');
            const kv = box.querySelector('#cppDefKv').value;
            if (kv) body.kvCacheType = kv;
            const fa = (box.querySelector('#cppDefFlash').value || '').trim();
            if (fa !== '') body.flashAttn = (parseInt(fa, 10) === 1);
            // R106b (2026-10-12): reasoning — три состояния. Пустая строка =
            // «не задавать» (ключ не отправляем, сервер оставит прежнее значение).
            // «выключено» отправляем ЯВНЫМ false: сервер различает «не прислали» и
            // «прислали false» (profilePresence), иначе выключить было бы нельзя.
            const rsn = (box.querySelector('#cppDefReasoning') || {}).value || '';
            if (rsn === 'on') body.enableReasoning = true;
            else if (rsn === 'off') body.enableReasoning = false;
            const ignoreEl = box.querySelector('#cppDefIgnore');
            if (ignoreEl) body.ignoreDefaults = !!ignoreEl.checked;

            if (body.contextLength === undefined && !data.defaultModelProfile && !body.contextPerSeq) {
                showToast('error', I18N.t('settings.load_defaults.ctx_required',
                    'Укажите n_ctx (суммарное) или «окно на клиента»: иначе настройки по умолчанию ничего не задают'));
                return;
            }
            try {
                await LOAD_DEFAULTS.save(body);
                showToast('success', I18N.t('settings.load_defaults.saved',
                    'Настройки по умолчанию сохранены'));
                box.innerHTML = '';
                await loadAndRenderDefaults();
            } catch (err) {
                showToast('error', err.message || String(err));
            }
        });
    }

    // ----- Модели в папке -----

    async function loadAndRenderCatalog() {
        const targets = findTargets('cppModelCatalog', 'cpp-model-catalog');
        if (targets.length === 0) return;
        if (!LOAD_DEFAULTS || !LOAD_DEFAULTS.catalog) {
            targets.forEach(function (t) { t.innerHTML = ''; });
            return;
        }
        const loading = '<div class="loading">' + escapeHtml(I18N.t('common.loading', 'Загрузка...')) + '</div>';
        targets.forEach(function (t) { t.innerHTML = loading; });
        try {
            const data = await LOAD_DEFAULTS.catalog();
            targets.forEach(function (t) { renderModelCatalog(data || {}, t); });
        } catch (e) {
            const msg = '<div class="cpp-profiles-empty">' + escapeHtml(e.message || String(e)) + '</div>';
            targets.forEach(function (t) { t.innerHTML = msg; });
        }
    }

    function fmtBytes(b) {
        if (!b || b <= 0) return '—';
        const gb = b / (1024 * 1024 * 1024);
        return gb >= 1 ? gb.toFixed(2) + ' GB' : (b / (1024 * 1024)).toFixed(0) + ' MB';
    }

    function renderModelCatalog(data, el) {
        const files = data.files || [];
        if (files.length === 0) {
            el.innerHTML = '<div class="cpp-profiles-empty">' +
                escapeHtml(I18N.t('settings.catalog.empty', 'В папке моделей нет GGUF-файлов (или cppworker недоступен).')) +
                '</div>';
            return;
        }
        const rows = files.map((f) => {
            const eff = f.effective || {};
            const src = f.source || {};
            const loadedBadge = f.loaded
                ? '<span class="cpp-source-badge is-default">' + escapeHtml(I18N.t('settings.catalog.loaded', 'загружена')) + (f.loadedContextSize ? ' ' + escapeHtml(formatCtx(f.loadedContextSize)) : '') + '</span>'
                : '<span class="cpp-source-badge is-none">' + escapeHtml(I18N.t('settings.catalog.not_loaded', 'не загружена')) + '</span>';
            const profBadge = f.hasProfile
                ? (f.profileIgnored
                    // R106b (2026-10-12): смысл флага уточнён. Профиль модели
                    // применяется ВСЕГДА (это настройки оператора); ignoreDefaults
                    // означает лишь «для этой модели не подставлять общие
                    // настройки по умолчанию». Прежняя подпись «профиль отключён»
                    // вводила в заблуждение — оператор думал, что его настройки не
                    // действуют, хотя грузится именно профиль.
                    ? '<span class="cpp-source-badge is-default">' + escapeHtml(I18N.t('settings.catalog.profile_ignored', 'свой профиль (общие настройки для неё отключены)')) + '</span>'
                    : '<span class="cpp-source-badge is-default">' + escapeHtml(I18N.t('settings.catalog.has_profile', 'свой профиль')) + '</span>')
                : '<span class="cpp-source-badge is-env">' + escapeHtml(I18N.t('settings.catalog.no_profile', 'по умолчанию')) + '</span>';
            const perSlot = eff.contextPerSlot || 0;
            const settings = [
                'n_ctx=' + fmtValue('contextLength', eff.contextLength) + ' суммарно (' + (src.contextLength || '—') + ')',
                perSlot ? 'на клиента=' + fmtValue('contextLength', perSlot) : null,
                eff.parallel ? 'parallel=' + eff.parallel + ' (' + (src.parallel || '—') + ')' : 'parallel=1',
                // R213 (2026-10-21): «kv=f16» при отсутствии значения было
                // догадкой, напечатанной как факт (та же жалоба оператора, что и
                // в карточке модели: «в настройках q4_0, а тут f16»). Если
                // эффективное значение не посчитано — показываем прочерк.
                eff.kvCacheType ? 'kv=' + eff.kvCacheType + ' (' + (src.kvCacheType || '—') + ')' : 'kv=—',
                // R106b: reasoning — то значение, которое оператор искал и не
                // находил в карточке («модель грузится не с теми параметрами»).
                'reasoning=' + fmtValue('enableReasoning', eff.enableReasoning === true) + ' (' + (src.enableReasoning || '—') + ')'
            ].filter(Boolean).join(' · ');
            return `
                <div class="cpp-profile-item" data-file="${escapeHtml(f.name)}">
                    <div class="cpp-profile-info">
                        <div class="cpp-profile-name">
                            ${escapeHtml(f.name)}
                            ${loadedBadge}
                            ${profBadge}
                        </div>
                        <div class="cpp-profile-meta">${escapeHtml(fmtBytes(f.sizeBytes))}${f.quantization ? ' · ' + escapeHtml(f.quantization) : ''} · ${escapeHtml(settings)}</div>
                    </div>
                    <div class="cpp-profile-actions">
                        <button class="btn btn-secondary" data-action="catalog-edit" data-name="${escapeHtml(f.name)}" title="${escapeHtml(I18N.t('settings.catalog.assign', 'Назначить настройки этой модели'))}">
                            <i class="fas fa-sliders"></i>
                        </button>
                    </div>
                </div>
            `;
        }).join('');

        el.innerHTML = `
            <div class="cpp-defaults-head">
                <div>
                    <strong>${escapeHtml(I18N.t('settings.catalog.title', 'Модели в папке'))}</strong>
                    <div class="cpp-profiles-hint">${escapeHtml(I18N.t('settings.catalog.hint',
                        'Файлы, доступные для загрузки. «Настроить» создаёт профиль модели — он применяется при следующей загрузке и по кнопке «Применить (reload)».'))}</div>
                </div>
            </div>
            ${rows}
        `;

        el.querySelectorAll('[data-action="catalog-edit"]').forEach(btn => {
            btn.addEventListener('click', async () => {
                const name = btn.dataset.name;
                // Имя файла: профили хранятся и без .gguf — попробуем оба варианта.
                const base = name.replace(/\.gguf$/i, '');
                let profile = null;
                try {
                    const r = await PROFILES.get(base);
                    profile = r.profile;
                } catch (e) {
                    try {
                        const r2 = await PROFILES.get(name);
                        profile = r2.profile;
                    } catch (e2) {
                        // профиля ещё нет — предзаполним текущими эффективными
                        const file = files.find(x => x.name === name);
                        if (file) {
                            profile = {
                                contextLength: file.effective.contextLength || 0,
                                batchSize: file.effective.batchSize || 0,
                                numGpuLayers: file.effective.numGpuLayers || 0,
                                parallel: file.effective.parallel || 0,
                                kvCacheType: file.effective.kvCacheType || ''
                            };
                        }
                    }
                }
                openWizard(base, profile);
            });
        });
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
        // R106c (2026-10-12): per-model timeouts больше не показываем — из формы
        // они убраны (работа идёт по состоянию). Значения, оставшиеся в старых
        // профилях, сервер продолжает применять, но в UI они не выставляются.
        if (profile.enableReasoning !== null && profile.enableReasoning !== undefined) {
            meta.push('reasoning=' + (profile.enableReasoning ? 'on' : 'off'));
        }
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
     *                parallel?, kvCacheType?, enableReasoning?}.
     * flashAttn/numa/useMmap/enableReasoning могут быть null (наследовать) или boolean.
     * R106c: per-model таймауты из UI не отправляются (поля убраны).
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
            // R106b (2026-10-12): reasoning/thinking ДЛЯ ЭТОЙ МОДЕЛИ.
            // Три состояния: '' = наследовать (настройки по умолчанию → env
            // контейнера), 'on' = включено, 'off' = выключено.
            //
            // ПОЧЕМУ ПОЯВИЛОСЬ: поля не было ни в одной форме WebUI — оператор
            // не мог ни включить размышления конкретной модели, ни выключить их
            // (жалоба «не дало поменять параметры размышления с выключенного на
            // включенный»). Поле профиля (types.LlamaCppModelProfile.EnableReasoning)
            // и presence-мерж на сервере поддерживают его с R83/v62 — не хватало
            // только органа управления.
            enableReasoning: (profile.enableReasoning === null || profile.enableReasoning === undefined)
                ? '' : (profile.enableReasoning ? 'on' : 'off'),
            notes: profile.notes || '',
            // R106c (2026-10-12): per-model ТАЙМАУТЫ УБРАНЫ из UI.
            //
            // Договорённость оператора: работа идёт ПО СОСТОЯНИЮ (загрузка/ошибка/
            // готовность), а не по «эфимерному времени»: таймаут ничего не
            // гарантирует, зато молча обрывает длинную генерацию. Поля больше не
            // показываются и не отправляются; серверные значения (0 = авто/
            // глобальные) остаются как были.
            // R67a: потолок n_ctx. Раньше эти поля не переносились и не
            // отправлялись — сохранение профиля из UI теряло авто-режим, и
            // профиль превращался в жёсткий потолок contextLength (32768).
            contextLengthAuto: !!profile.contextLengthAuto,
            contextLengthMax: profile.contextLengthMax || 0,
            // R83 (2026-09-29): primary — «настройки модели зафиксированы
            // администратором, клиентский запрос их не меняет». См.
            // internal/balancer/primary_model_policy.go.
            primary: !!profile.primary
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
        // R106b (2026-10-12): reasoning — 3 состояния. '' = ключ НЕ отправляем
        // (тогда сервер оставляет прежнее значение: мерж-семантика профиля).
        // Сервер различает «не прислали» и «прислали false» через profilePresence,
        // поэтому явное «выключено» доезжает и не подменяется наследованием.
        if (state.enableReasoning === 'on') body.enableReasoning = true;
        else if (state.enableReasoning === 'off') body.enableReasoning = false;
        // R106c: таймауты больше не отправляются из UI (см. комментарий выше).
        // R67a: auto/max отправляем ВСЕГДА (в т.ч. false/0) — сервер применяет их
        // по факту передачи, поэтому явное «выключить авто» и «снять потолок»
        // работают, а частичные обновления больше не обнуляют остальные поля.
        body.contextLengthAuto = !!state.contextLengthAuto;
        body.contextLengthMax = parseInt(state.contextLengthMax, 10) || 0;
        // R83 (2026-09-29): primary отправляем ВСЕГДА (true/false) — иначе снять
        // фиксацию из UI было бы нельзя (мерж-семантика: отсутствие поля = «не менять»).
        body.primary = !!state.primary;
        return body;
    }

    function openWizard(modelName, profile) {
        const isNew = !profile;
        const state = profileToWizardState(profile || {});

        // R106c (2026-10-12): расчёты «эвристических таймаутов» удалены вместе с
        // полями: UI больше не предлагает выставлять время на работу модели.
        // Резолвер таймаутов на сервере остался (0 = авто), но оператор им не
        // управляет из формы: состояние важнее часов.

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
                    <!-- R67a (2026-09-23): управление потолком n_ctx.
                         Без contextLengthAuto профиль — ЖЁСТКИЙ потолок: клиент с
                         num_ctx больше этого значения получает ровно contextLength
                         независимо от свободной VRAM (жалоба «жёстко 32768 на A10»). -->
                    <div class="wizard-field">
                        <label>
                            <input type="checkbox" id="wizCtxAuto" ${state.contextLengthAuto ? 'checked' : ''}>
                            ${escapeHtml(I18N.t('settings.profiles.ctx_auto', 'Авто-адаптация контекста (не ограничивать профилем)'))}
                        </label>
                        <div class="ctx-help">${escapeHtml(I18N.t('settings.profiles.ctx_auto_help', 'Включено: n_ctx подбирается по свободной VRAM/GGUF (профиль — подсказка). Выключено: contextLength становится ЖЁСТКИМ потолком (например 32768), и больший контекст клиент не получит даже при свободной VRAM.'))}</div>
                    </div>
                    <div class="wizard-field">
                        <label>${escapeHtml(I18N.t('settings.profiles.ctx_max', 'Максимальный контекст, токенов (0 = до предела GGUF)'))}</label>
                        <input type="number" id="wizCtxMax" value="${state.contextLengthMax || 0}" min="0" max="${CTX_MAX}" step="1024" placeholder="0 = unlimited (до GGUF max)">
                        <div class="ctx-help">${escapeHtml(I18N.t('settings.profiles.ctx_max_help', 'Работает вместе с авто-адаптацией: верхняя граница (политика оператора). 0 = без ограничения, вплоть до максимума GGUF.'))}</div>
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
                            <label class="checkbox-label">
                                <input type="checkbox" id="wizPrimary" ${state.primary ? 'checked' : ''}>
                                <span>${escapeHtml(I18N.t('settings.profiles.primary', 'Зафиксировать настройки (клиентские запросы их не меняют)'))}</span>
                            </label>
                            <div class="ctx-help">${escapeHtml(I18N.t('settings.profiles.primary_help', 'Включено: клиентский запрос НЕ может изменить параметры модели. Если запросу хватает зафиксированного контекста — он обслуживается (num_ctx подменяется на загруженный); если нужно больше — клиент получает отказ с указанием, что настройки зафиксированы администратором. Также отключает авто-оптимизацию (AutoTune) для этой модели. Параметры по-прежнему меняются здесь, через администратора.'))}</div>
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
                            <label>${escapeHtml(I18N.t('settings.profiles.enable_reasoning', 'Размышления (reasoning/thinking)'))}</label>
                            <select id="wizEnableReasoning">
                                <option value="" ${state.enableReasoning === '' ? 'selected' : ''}>— наследовать (настройки по умолчанию → env) —</option>
                                <option value="on" ${state.enableReasoning === 'on' ? 'selected' : ''}>включено</option>
                                <option value="off" ${state.enableReasoning === 'off' ? 'selected' : ''}>выключено</option>
                            </select>
                            <div class="ctx-help">${escapeHtml(I18N.t('settings.profiles.enable_reasoning_help', 'Включает soft+native thinking для этой модели (gemma-4, deepseek-r1, qwen3-thinking). «Наследовать» = взять из «настроек по умолчанию для новых моделей», а если и там не задано — из env контейнера cppworker. Значение применяется при следующей загрузке и по кнопке «Применить (reload)».'))}</div>
                        </div>
                        <!-- R106c (2026-10-12): per-model ТАЙМАУТЫ УБРАНЫ.
                             Договорённость оператора: работа идёт ПО СОСТОЯНИЮ
                             (загрузка/ошибка/готовность), а не по «эфимерному
                             времени» — таймаут ничего не гарантирует, зато молча
                             обрывает длинную генерацию. Вместе с полями убран и
                             переключатель «Reasoning mode (×3 таймаут)»: он
                             существовал только ради этих множителей.
                             Серверные значения (0 = авто) остаются как были. -->
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

        // R106c (2026-10-12): обработчики «Reasoning mode (×3 таймаут)» и
        // автозаполнения таймаутов удалены вместе с полями. Работа идёт по
        // состоянию: пока модель грузится/генерирует — UI показывает состояние,
        // при ошибке — сразу ошибку (см. gguf-load-progress/preflight), а не
        // «подождём N секунд».

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
            // R106b (2026-10-12): reasoning для конкретной модели (3 состояния).
            enableReasoning: ((overlay.querySelector('#wizEnableReasoning') || {}).value || ''),
            notes: (overlay.querySelector('#wizNotes').value || '').trim(),
            // R67a: потолок n_ctx (авто-режим + максимум).
            contextLengthAuto: !!(overlay.querySelector('#wizCtxAuto') && overlay.querySelector('#wizCtxAuto').checked),
            contextLengthMax: parseInt((overlay.querySelector('#wizCtxMax') || {}).value, 10) || 0,
            // R83 (2026-09-29): primary — фиксация настроек администратором.
            primary: !!(overlay.querySelector('#wizPrimary') && overlay.querySelector('#wizPrimary').checked)
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
        // R106b: reasoning — 3 состояния (наследовать / вкл / выкл).
        const allowedReasoning = ['', 'on', 'off'];
        if (allowedReasoning.indexOf(state.enableReasoning) === -1) {
            showToast('error', "enableReasoning должен быть одним из ['', 'on', 'off']");
            return false;
        }
        // R106c (2026-10-12): валидация таймаутов удалена вместе с полями
        // (в форме их больше нет — работа идёт по состоянию, а не по времени).
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
            closeWizard();
            await loadAndRender();

            // R83 (2026-09-29): «нажал Сохранить — ничего не изменилось».
            //
            // PUT профиля ТОЛЬКО сохраняет настройки. Уже загруженная модель
            // продолжает работать со СТАРЫМИ параметрами, пока профиль не
            // применён (reload): оператор сохранял n_ctx/parallel/kvCacheType,
            // видел «Профиль сохранён» и ждал эффекта, которого не было.
            // Теперь говорим прямо, что дальше, и предлагаем применить сразу.
            const isLoaded = isModelLoadedNow(name);
            if (isLoaded) {
                const applyNow = confirm(I18N.t('settings.profiles.saved_needs_apply_confirm',
                    'Профиль сохранён. Загруженная модель пока работает со СТАРЫМИ настройками.\n\n' +
                    'Применить сейчас? Модель будет перезагружена (несколько секунд/минут, ' +
                    'активные запросы к ней прервутся).\n\n' +
                    '«Отмена» — настройки применятся при следующей загрузке модели.'));
                if (applyNow) {
                    await applyProfileWithProgress(name);
                } else {
                    showToast('warning', I18N.t('settings.profiles.saved_not_applied',
                        'Профиль сохранён. Модель работает со старыми настройками — ' +
                        'изменения применятся при следующей загрузке или по кнопке «Применить (reload)».'));
                }
            } else {
                showToast('success', isNew
                    ? I18N.t('settings.profiles.created', 'Профиль создан')
                    : I18N.t('settings.profiles.updated', 'Профиль обновлён'));
            }
        } catch (err) {
            showToast('error', err.message || String(err));
        }
    }

    // isModelLoadedNow — загружена ли модель прямо сейчас (по данным последнего
    // опроса /api/v1/cluster/models/loaded, который кладёт app.js в
    // window.CPPWORKER_LOADED_MODELS). Имя может быть как именем модели, так и
    // именем файла — сравниваем в обе стороны, как это делает профиль-резолвер
    // балансера.
    function isModelLoadedNow(name) {
        try {
            const models = (window.CPPWORKER_LOADED_MODELS && window.CPPWORKER_LOADED_MODELS.models) || [];
            if (!models.length) return false;
            const target = String(name || '').toLowerCase();
            return models.some(function (m) {
                const n = String(m.name || '').toLowerCase();
                const p = String(m.path || '').toLowerCase();
                return n === target || p.indexOf(target) >= 0 || (target && target.indexOf(n) >= 0);
            });
        } catch (e) {
            return false;
        }
    }

    // ----- Apply with progress modal (Round 26 v0.5.13) -----
    // Async path: detect busy → 202 + SSE progress → terminal state.
    // Sync path: 200 + final backends list → close modal.

    async function applyProfileWithProgress(modelName, backendID) {
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

            const resp = await PROFILES.apply(modelName, undefined, backendID);

            setStep('save', 'done');

            // Если 202 — async path (buggy бэкенд был занят).
            // Используем SSE для live прогресса.
            if (resp._status === 202 && resp.applyId && APPLY_PROGRESS) {
                fillEl.style.width = '30%';
                setStep('busy', 'running');
                steps.busy.querySelector('.reload-step-text').textContent =
                    // Параметры передаём ОБЪЕКТОМ: i18n/index.js.t(key, params) —
                    // второй аргумент это подстановки, а не текст fallback. Раньше
                    // сюда шла готовая строка, поэтому при наличии перевода с
                    // {count} число не подставлялось бы вовсе.
                    I18N.t('settings.profiles.step_busy_with_count',
                        { count: resp.initialActiveQueries || 0 });

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