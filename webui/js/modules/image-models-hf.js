/**
 * image-models-hf.js — R-Image Phase 9: табы «HuggingFace» и «Загрузки»
 * страницы «Image-модели» (window.ImageModelsHf).
 *
 * ЧТО ЭТО. Модуль табов HF/Загрузки: поиск репозиториев на HuggingFace, выбор
 * файлов модели (bundle) с ПРЕДЛОЖЕННЫМИ сервером ролями, запуск bundle-загрузки
 * и страница состояния загрузок (активные bundle, история, остатки).
 *
 * ПОЧЕМУ В СТИЛЕ GGUF. Страница «GGUF модели» уже решает ту же задачу для
 * llama.cpp (поиск → файлы → скачивание → прогресс → история → остатки), и
 * оператору не нужно учить второй интерфейс. Поэтому здесь ТОТ ЖЕ визуальный
 * язык (классы .gguf-search-form/.gguf-result-card/.gguf-file-item/
 * .gguf-download-item/.gguf-empty-state из webui/css/pages.css) и те же готовые
 * ключи i18n там, где смысл совпадает (gguf.search_btn, gguf.delete_from_disk,
 * gguf.active_downloads, gguf.download_history, gguf.orphans_hint, ...).
 *
 * ГРАНИЦЫ (Phase 9): модуль владеет ТОЛЬКО табами hf/downloads. Список «Модели на
 * диске», «Загруженные» и «Настройки» рисует image-page.js (у него уже есть
 * рабочий код и тесты), шелл image-models-page.js даёт контекст.
 *
 * ЧЕГО ЗДЕСЬ НЕТ: генерации и показа картинок. WebUI — для настройки и состояния.
 *
 * РОЛИ ФАЙЛОВ приходят с СЕРВЕРА (поле suggestedRole в GET /api/hf/files,
 * internal/sdbackend/models.go → SuggestRole). В JS эвристика НЕ дублируется:
 * иначе правила «какой файл есть VAE» разъехались бы между воркером и UI, и
 * bundle собирался бы неправильно при том же имени файла.
 *
 * Экспорт: window.ImageModelsHf = { mount, unmount, render, tabIds, _actions, _state, pure }.
 * R694: модуль подписан на i18n:changed (в mount, снимается unmount'ом) и
 * перерисовывает динамику активного таба из state — без сети и без потери
 * раскрытой карточки R693.
 */
(function () {
    'use strict';

    var TAB_IDS = ['hf', 'downloads'];
    var HF_TOKEN_KEY = 'ollamalegion_hf_token';

    // Таймауты: управляющие запросы к воркеру быстрые, но поиск HF ходит в
    // интернет, поэтому у него свой, более щедрый лимит.
    var TIMEOUT_CONTROL_MS = 15000;
    var TIMEOUT_SEARCH_MS = 30000;
    var DEFAULT_POLL_MS = 2000;
    var SEARCH_LIMIT = 20;

    // Роли файлов — ЗАМОРОЖЕННЫЙ контракт pkg/types/image_model.go:32-45.
    var IMAGE_ROLES = ['diffusion', 'vae', 'clip_l', 'clip_g', 't5xxl', 'llm', 'clip_vision', 'taesd', 'lora', 'upscaler', 'controlnet', 'ip_adapter'];

    // R694: ключи подписей ролей для селекта файла. Перечислены ЯВНО, а не
    // собираются как 'imageModels.role_' + role, и это осознанное решение:
    //
    //   1) scripts/i18n_dead_keys.js (храповик R575: «мёртвых» ключей 0) видит
    //      потребителя либо как строку, РАВНУЮ ключу, либо как динамический
    //      префикс. Явный список делает каждый ключ видимым поимённо, а не
    //      «возможно динамическим»: удалить живой перевод или добавить ключ без
    //      потребителя чекер поймает, не ослабляя allowlist;
    //   2) карта — единственный источник всех 12 подписей: тест сверяет её с
    //      IMAGE_ROLES и с обоими пакетами, поэтому новая роль без перевода
    //      краснеет, а не показывает сырой id (clip_l, t5xxl, ...) в селекте.
    var ROLE_LABEL_KEYS = {
        diffusion: 'imageModels.role_diffusion',
        vae: 'imageModels.role_vae',
        clip_l: 'imageModels.role_clip_l',
        clip_g: 'imageModels.role_clip_g',
        t5xxl: 'imageModels.role_t5xxl',
        llm: 'imageModels.role_llm',
        clip_vision: 'imageModels.role_clip_vision',
        taesd: 'imageModels.role_taesd',
        lora: 'imageModels.role_lora',
        upscaler: 'imageModels.role_upscaler',
        controlnet: 'imageModels.role_controlnet',
        ip_adapter: 'imageModels.role_ip_adapter'
    };
    // Семейства — pkg/types/image_model.go:49-52.
    var IMAGE_FAMILIES = ['sd15', 'sd21', 'sd_turbo', 'sdxl', 'sdxl_turbo', 'sd3', 'flux', 'flux2', 'chroma', 'qwen_image', 'z_image', 'other'];

    /**
     * Роли, которые отмечаются галочкой автоматически: почти любой bundle для
     * sd.cpp — это diffusion + VAE + text encoder'ы.
     *
     * t5xxl/llm СОЗНАТЕЛЬНО не отмечаются: это отдельные файлы на 3-9 ГБ, и
     * скачать их «за компанию» по ошибке дороже, чем поставить галочку руками.
     * Роль при этом всё равно подставлена из suggestedRole — оператор видит,
     * что это text encoder, и решает сам.
     */
    var AUTO_ROLES = ['diffusion', 'vae', 'clip_l', 'clip_g'];

    /**
     * COMPONENT_ROLES — роли, которые можно ДОБАВИТЬ к готовому набору.
     *
     * Зеркало серверного списка sdbackend.IsComponentRole (componentKindRoles):
     * diffusion исключён сознательно — его файл и есть модель, «добавление»
     * второго означало бы подмену диффузии. Список статический (а не собранный
     * из IMAGE_ROLES) по той же причине, что и ROLE_LABEL_KEYS: измеритель i18n
     * и тесты должны видеть каждую роль поимённо.
     */
    var COMPONENT_ROLES = ['vae', 'llm', 't5xxl', 'clip_l', 'clip_g', 'clip_vision',
        'taesd', 'upscaler', 'controlnet', 'ip_adapter'];

    /** Порядок ролей: сначала то, без чего движок не поднимется (как в каталоге). */
    var COMPONENT_ROLE_RANK = { vae: 0, llm: 1, t5xxl: 2, clip_l: 3, clip_g: 4, clip_vision: 5 };

    var mounted = false;
    // R694: подписка на i18n:changed. Храним функцию, а не навешиваем анонимную:
    // unmount() обязан снять РОВНО её, иначе повторный mount() навесит вторую и
    // смена языка перерисует выдачу дважды (двойные запросы на табе «Загрузки»).
    var i18nHandler = null;
    // keydownHandler — обработчик Enter в поле поиска: тоже хранится, чтобы
    // unmount() снял его, а повторный mount не навесил второй.
    var keydownHandler = null;
    // R749: inputHandler — ввод имени bundle в корзине обновляет строку «Куда»
    // сразу (change срабатывает только на blur). Хранится по той же причине.
    var inputHandler = null;
    // dataRefreshBound — провайдер «Загрузок» регистрируется один раз за жизнь
    // страницы: у DataRefresh нет unregister (см. unmount ниже).
    var dataRefreshBound = false;
    var state = {
        ctx: null,
        query: '',
        task: 'text-to-image',
        searching: false,
        searchError: '',
        results: [],
        selectedRepo: '',
        // expandedRepo — репозиторий, у которого файлы РАСКРЫТЫ в его же карточке
        // результата (R693). Пустая строка = всё свёрнуто.
        //
        // ЗАЧЕМ ОТДЕЛЬНОЕ ПОЛЕ, А НЕ selectedRepo. Раньше клик по «Показать файлы»
        // просто выбирал репозиторий и заполнял ОДНУ общую карточку файлов
        // (#imHfFilesCard), которая стоит в разметке ПОСЛЕ всего списка найденных
        // моделей. При 20 результатах это ~2 экрана ниже нажатой кнопки, страница
        // туда не прокручивалась — оператор видел «клик ничего не делает», хотя
        // строки уже были отрисованы. Теперь карточка файлов переезжает внутрь
        // карточки нажатой модели (см. attachFilesCard), а expandedRepo говорит,
        // в какой именно: раскрыт всегда максимум один репозиторий.
        expandedRepo: '',
        // repoCache — repo → {files, selection, probes, plan, familyNotice}:
        // повторное раскрытие того же репозитория не дёргает воркер заново
        // (файлы/паспорт/пред-проверки уже приходили), галочки оператора
        // сохраняются. Хранятся ССЫЛКИ на объекты состояния, поэтому пред-проверка,
        // доехавшая после сворачивания, тоже попадает в кэш.
        repoCache: {},
        // pickSeq — номер попытки раскрытия: ответ /hf/files, пришедший после
        // клика по ДРУГОЙ карточке, не имеет права перезаписать её состояние.
        pickSeq: 0,
        files: [],
        filesLoading: false,
        filesError: '',
        // path → { checked: bool, role: string }
        selection: {},
        // path → результат пред-проверки заголовка (/api/hf/probe): вердикт
        // «прочитает ли движок». Ключ — путь файла в репозитории.
        probes: {},
        // plan — «паспорт репозитория» от воркера (GET /api/hf/plan): комплектность
        // набора, режим движка, чего не хватает и шаги. Нужен, потому что
        // пригодность ОДНОГО файла и пригодность НАБОРА — разные вещи (живой
        // случай: diffusion скачан, VAE и text encoder — нет).
        plan: null,
        planLoading: false,
        // path → true, пока идёт пред-проверка (для спиннера в строке файла).
        probing: {},
        bundleName: '',
        bundleFamily: 'sd15',
        // familyManual — оператор сам выбирал семейство в селекте: автоподстановка
        // по заголовку файла (applyProbedFamily) после этого молчит.
        familyManual: false,
        // familyNotice — сообщение «семейство подставлено по файлу» для шапки.
        familyNotice: '',
        tokenSaved: false,
        // R696: вердикт воркера по ВЫБРАННОМУ набору (POST /api/hf/verify).
        // verifyKey — отпечаток выбора (репозиторий + семейство + пути/роли),
        // чтобы одинаковый набор не проверялся повторно при перерисовках.
        verify: null,
        verifyKey: '',
        verifyBusy: false,
        verifyError: '',
        // verifySeq — ответ устаревшей проверки (выбор успел измениться) не
        // должен перезаписать вердикт актуального набора.
        verifySeq: 0,
        verifyTimer: null,
        // ==== R710 (2026-11-24): недостающие компоненты набора ====
        //
        // extraComponents — файлы, добавленные к ТЕКУЩЕМУ ВЫБОРУ из ДРУГОГО
        // репозитория (VAE/text encoder, которых нет в репозитории модели). На
        // вкладке HF набор ещё не скачан, поэтому «добавить компонент» здесь =
        // добавить файл в выбор: он попадёт в один POST /api/hf/bundle и
        // скачается вместе с остальным. Для уже скачанного bundle (страница
        // «Модели на диске») работает другой путь — installBundleComponent()
        // ниже: он пишет файл в каталог модели и роль в profile.json.
        extraComponents: [],
        // componentCatalog — ответ GET /api/v1/image/components/catalog (кэш по
        // family|roles). null = ещё не загружен.
        componentCatalog: null,
        componentCatalogKey: '',
        componentCatalogBusy: false,
        componentCatalogError: '',
        // componentManual — открытая форма «Выбрать из другого репозитория…»:
        // {repo, loading, error, files, selection:{path:role}}. null = закрыта.
        componentManual: null,
        // componentNote — предупреждение после добавления («добавлено по
        // умолчанию — проверьте лицензию»), показывается в карточке вердикта.
        componentNote: '',
        // componentBusy — идёт установка в bundle (страница «Модели на диске»):
        // {model, role, trackingId, status, pct, error}.
        componentBusy: null,
        // прогресс bundle: снимок HFBundleProgress из /api/hf/progress?bundleId=
        bundle: null,
        poll: null,
        pollIdleTicks: 0,
        downloads: null,
        downloadsError: '',
        // ==== R749 (2026-11-25): корзина загрузки и состояние узла ====
        //
        // backendFiles — индекс файлов ВЫБРАННОГО image-бэкенда (GET .../models):
        // {paths, models, at} либо null, пока не прочитан. Нужен, чтобы корзина
        // отвечала «есть на узле / нет на узле», а не заставляла оператора
        // скачивать вслепую то, что уже лежит. backendFilesBackendId — для какой
        // машины построен индекс: при смене бэкенда индекс сбрасывается.
        backendFiles: null,
        backendFilesBackendId: '',
        backendFilesBusy: null,
        backendFilesError: '',
        // onlyMissing — одноразовый режим startBundle: отправить ТОЛЬКО те
        // отмеченные файлы, которых нет на узле (кнопка «Скачать недостающее»).
        onlyMissing: false,
        // ==== R755 (2026-11-25): матрица форматов (что движок читает) ====
        // formatMatrices — family → ответ GET .../formats. Кэш на страницу:
        // правила статические, а нужны на каждую строку файла. formatLoading —
        // family → true, чтобы не дёргать воркер на каждый рендер.
        formatMatrices: {},
        formatLoading: {},
        busy: false
    };

    // =====================================================================
    // Pure helpers — без DOM, покрыты node-тестом
    // (webui/js/modules/image-models-hf.test.js).
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

    function formatBytes(n) {
        if (n === null || n === undefined || n === '' || isNaN(Number(n))) return '-';
        var v = Number(n);
        if (v <= 0) return '0 B';
        var units = ['B', 'KB', 'MB', 'GB', 'TB'];
        var i = 0;
        while (v >= 1024 && i < units.length - 1) { v = v / 1024; i++; }
        return (i === 0 ? String(Math.round(v)) : v.toFixed(v >= 100 ? 0 : 1)) + ' ' + units[i];
    }

    /** Имя файла: воркер отдаёт path, raw HF API — rfilename. */
    function filePath(f) {
        f = f || {};
        return String(f.path || f.rfilename || f.filename || f.name || '');
    }

    function fileSize(f) {
        f = f || {};
        return Number(f.sizeBytes || f.size_bytes || f.size || 0);
    }

    /**
     * Роль файла: ТОЛЬКО серверная подсказка (suggestedRole). Пустая строка =
     * «сервер не знает» (тогда UI честно предлагает first-role из списка и
     * оператор выбирает сам).
     */
    function suggestedRole(f) {
        f = f || {};
        return String(f.suggestedRole || f.suggested_role || '');
    }

    function isValidRole(role) {
        return IMAGE_ROLES.indexOf(String(role || '')) !== -1;
    }

    /**
     * Отметки по умолчанию для списка файлов репозитория.
     *
     * ПРАВИЛА (объяснены в комментарии к AUTO_ROLES):
     *   - файл с ролью из AUTO_ROLES отмечается, если для этой роли ещё нет
     *     отмеченного файла (первый по порядку в репозитории — как правило,
     *     основной вариант, а не fp16-дубль);
     *   - t5xxl/llm/lora/controlnet/... — не отмечаются;
     *   - preserve (предыдущие выборы пользователя) имеет приоритет: повторный
     *     рендер (смена языка, refresh) не должен сбрасывать галочки.
     */
    function defaultSelection(files, preserve) {
        var prev = preserve || {};
        var sel = {};
        var autoUsed = {};
        (files || []).forEach(function (f) {
            var p = filePath(f);
            if (!p) return;
            if (Object.prototype.hasOwnProperty.call(prev, p)) {
                sel[p] = {
                    checked: !!prev[p].checked,
                    role: prev[p].role || suggestedRole(f) || 'diffusion',
                    // sizeBytes тащим в выбор: он нужен buildFileSpecs, чтобы
                    // воркер не перекачивал уже лежащие файлы (hf_bundle.go:128).
                    sizeBytes: fileSize(f)
                };
                autoUsed[sel[p].role] = true;
                return;
            }
            var role = suggestedRole(f) || 'diffusion';
            var auto = AUTO_ROLES.indexOf(role) !== -1 && !autoUsed[role];
            if (auto) autoUsed[role] = true;
            sel[p] = { checked: auto, role: role, sizeBytes: fileSize(f) };
        });
        return sel;
    }

    /**
     * Отметки → спецификация bundle для POST /api/hf/bundle.
     *
     * Тело: {name, family, files:[{role, repo, filename, revision}]}
     * (cmd/sdworker/handlers_hf.go:45-48, hfBundleRequest).
     *
     * Ошибки — КОДАМИ (не текстом): pure-слой не зависит от языка.
     *   need_name      — имя bundle не задано;
     *   empty          — ни один файл не отмечен;
     *   need_diffusion — нет роли diffusion (без него sd-server не стартует);
     *   dup_role       — роль занята дважды (кроме lora: их может быть много).
     *
     * R710: opts.components — файлы, добавленные к набору из ДРУГОГО
     * репозитория (VAE/text encoder). У них СВОЙ repo, поэтому они не выводятся
     * из repo модели и добавляются в files как есть. Роль компонента тоже
     * участвует в проверке дублей: два VAE в одном наборе — это ошибка.
     */
    function buildFileSpecs(repo, selection, opts) {
        opts = opts || {};
        var errors = [];
        var name = String(opts.name || '').trim();
        if (!name) errors.push({ code: 'need_name' });
        var files = [];
        var seen = {};
        Object.keys(selection || {}).forEach(function (path) {
            var item = selection[path];
            if (!item || !item.checked) return;
            var role = String(item.role || '');
            if (!isValidRole(role)) {
                errors.push({ code: 'bad_role', path: path, role: role });
                return;
            }
            // Дубликат роли запрещён, кроме lora (pkg/types/image_model.go:
            // "duplicate role" только для остальных).
            if (seen[role] && role !== 'lora') {
                errors.push({ code: 'dup_role', role: role, path: path });
                return;
            }
            seen[role] = true;
            files.push({
                role: role,
                repo: String(repo || ''),
                filename: path,
                revision: 'main',
                sizeBytes: Number((item && item.sizeBytes) || 0)
            });
        });
        (opts.components || []).forEach(function (c) {
            c = c || {};
            var role = String(c.role || '');
            if (!isValidRole(role)) {
                errors.push({ code: 'bad_role', path: String(c.filename || ''), role: role });
                return;
            }
            if (seen[role] && role !== 'lora') {
                errors.push({ code: 'dup_role', role: role, path: String(c.filename || '') });
                return;
            }
            seen[role] = true;
            files.push({
                role: role,
                repo: String(c.repo || ''),
                filename: String(c.filename || ''),
                revision: String(c.revision || 'main'),
                sizeBytes: Number(c.sizeBytes || 0)
            });
        });
        if (!files.length) errors.push({ code: 'empty' });
        if (files.length && !seen.diffusion) errors.push({ code: 'need_diffusion' });
        return { name: name, files: files, errors: errors };
    }

    /** Первая ошибка сборки → ключ i18n + параметры (для тоста). */
    function bundleErrorToI18n(err) {
        err = err || {};
        if (err.code === 'need_name') return { key: 'image.bundle_need_name', fallback: 'Bundle name is required', vars: {} };
        if (err.code === 'empty') return { key: 'imageModels.hf_none_selected', fallback: 'Select at least one file', vars: {} };
        if (err.code === 'need_diffusion') return { key: 'image.bundle_need_diffusion', fallback: 'A diffusion file is required in the bundle', vars: {} };
        if (err.code === 'dup_role') return { key: 'image.bundle_dup_role', fallback: 'Duplicate role {role}', vars: { role: err.role } };
        if (err.code === 'bad_role') return { key: 'imageModels.hf_bad_role', fallback: 'Unknown role: {role}', vars: { role: err.role } };
        return { key: 'common.error', fallback: 'Error', vars: {} };
    }

    /**
     * Процент по снимку прогресса: сервер отдаёт progressPct, но для снимков
     * bundle-истории он может быть 0 при completed — тогда считаем 100.
     */
    function progressPercent(p) {
        p = p || {};
        var status = String(p.status || '');
        if (status === 'completed') return 100;
        var pct = Number(p.progressPct || p.progress_pct || 0);
        if (!pct && p.totalBytes > 0) pct = Math.floor((Number(p.downloaded || 0) / Number(p.totalBytes)) * 100);
        if (pct > 100) pct = 100;
        if (pct < 0 || isNaN(pct)) pct = 0;
        return Math.round(pct);
    }

    /** Ключ i18n состояния загрузки bundle (переиспользуем gguf.* там, где есть). */
    function bundleStatusKey(status) {
        var s = String(status || '').toLowerCase();
        if (s === 'downloading') return 'gguf.downloading';
        if (s === 'completed') return 'gguf.download_completed';
        if (s === 'failed') return 'gguf.download_failed';
        if (s === 'cancelled') return 'gguf.download_cancelled';
        if (s === 'interrupted') return 'imageModels.status_interrupted';
        return 'common.unknown';
    }

    /**
     * Сводка состава download-снимка: нужна «Загрузкам», чтобы не рисовать
     * пустые секции (в стиле GGUF: история есть — секция есть).
     */
    function downloadsSummary(snap) {
        snap = snap || {};
        var bundles = Array.isArray(snap.bundles) ? snap.bundles : [];
        var bundleHistory = Array.isArray(snap.bundleHistory) ? snap.bundleHistory : [];
        var active = Array.isArray(snap.active) ? snap.active : [];
        var history = Array.isArray(snap.history) ? snap.history : [];
        var orphans = Array.isArray(snap.orphans) ? snap.orphans : [];
        return {
            bundles: bundles,
            bundleHistory: bundleHistory,
            active: active,
            history: history,
            orphans: orphans,
            hasAny: !!(bundles.length || bundleHistory.length || active.length || history.length || orphans.length),
            activeCount: bundles.length + active.length
        };
    }

    // =====================================================================
    // Контекст и запросы
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

    function el(id) {
        return (typeof document !== 'undefined' && document.getElementById) ? document.getElementById(id) : null;
    }

    function apiBase() {
        if (state.ctx && state.ctx.apiBase) return String(state.ctx.apiBase).replace(/\/+$/, '');
        var cfg = window.WEBUI_CONFIG || {};
        var base = cfg.API_BASE_URL || cfg.API_BASE ||
            (typeof window !== 'undefined' && window.location && window.location.origin) || '';
        return String(base).replace(/\/+$/, '');
    }

    function authHeaders(extra) {
        var headers = (window.Api && typeof window.Api.getAuthHeaders === 'function')
            ? window.Api.getAuthHeaders()
            : { 'Content-Type': 'application/json' };
        if (!headers['X-API-Token']) {
            var tk = (window.WEBUI_CONFIG && window.WEBUI_CONFIG.API_TOKEN) || '';
            if (!tk) {
                try { tk = localStorage.getItem('apiToken') || ''; } catch (e) { tk = ''; }
            }
            if (tk) headers['X-API-Token'] = tk;
        }
        if (extra) Object.keys(extra).forEach(function (k) { headers[k] = extra[k]; });
        return headers;
    }

    function hfToken() {
        try { return localStorage.getItem(HF_TOKEN_KEY) || ''; } catch (e) { return ''; }
    }

    function saveHfToken(value) {
        try {
            if (value) localStorage.setItem(HF_TOKEN_KEY, value);
            else localStorage.removeItem(HF_TOKEN_KEY);
        } catch (e) { /* приватный режим */ }
        if (window.GgufApi && typeof window.GgufApi.setHFToken === 'function') {
            try { window.GgufApi.setHFToken(value || ''); } catch (e2) { /* необязательный мост */ }
        }
    }

    function backendId() {
        return state.ctx && state.ctx.backendId ? String(state.ctx.backendId) : '';
    }

    /** Путь к воркеру через балансер: /api/v1/image/backends/{id}/<rest>. */
    function backendUrl(rest) {
        return apiBase() + '/api/v1/image/backends/' + encodeURIComponent(backendId()) + '/' + rest;
    }

    /** fetch + JSON с разбором конверта ошибки воркера ({error: "..."}). */
    function request(url, opts) {
        opts = opts || {};
        // R88: timeoutMs=0 = без капа (доктрина: работу не рвём по будильнику),
        // поэтому проверяем undefined/null, а не truthiness.
        var timeoutMs = (opts.timeoutMs === undefined || opts.timeoutMs === null)
            ? TIMEOUT_CONTROL_MS
            : opts.timeoutMs;
        var ctrl = (typeof AbortController !== 'undefined') ? new AbortController() : null;
        var timer = null;
        if (ctrl && timeoutMs > 0) timer = setTimeout(function () { ctrl.abort(); }, timeoutMs);
        var headers = authHeaders(opts.headers);
        // HF-токен шлём только HF-путям (как gguf-api.js:1015 и image-page.js:539):
        // воркер читает его из X-HF-Token (cmd/sdworker/handlers_hf.go:107).
        if (url.indexOf('/hf/') !== -1) {
            var tk = hfToken();
            if (tk) headers['X-HF-Token'] = tk;
        }
        var init = { method: opts.method || 'GET', headers: headers };
        if (ctrl) init.signal = ctrl.signal;
        if (opts.body !== undefined) init.body = opts.body;
        return fetch(url, init).then(function (resp) {
            if (timer) clearTimeout(timer);
            return resp.text().then(function (text) {
                var parsed = null;
                if (text) { try { parsed = JSON.parse(text); } catch (e) { parsed = text; } }
                if (!resp.ok) {
                    var msg = (parsed && (parsed.error || parsed.message)) || ('HTTP ' + resp.status);
                    var err = new Error(msg);
                    err.status = resp.status;
                    err.body = parsed;
                    throw err;
                }
                return parsed;
            });
        }).catch(function (err) {
            if (timer) clearTimeout(timer);
            if (err && err.name === 'AbortError') {
                var e2 = new Error('timeout after ' + Math.round(timeoutMs / 1000) + 's');
                e2.code = 'timeout';
                throw e2;
            }
            throw err;
        });
    }

    function toast(msg, type) {
        if (window.Toast && typeof window.Toast.show === 'function') {
            window.Toast.show({ message: msg, type: type || 'info' });
            return;
        }
        if (window.Notifications && typeof window.Notifications.show === 'function') {
            window.Notifications.show(msg, type);
            return;
        }
        if (window.console) console.log('[image-models-hf]', type || 'info', msg);
    }

    // =====================================================================
    // Рендер: поиск на HuggingFace
    // =====================================================================

    function setHtml(id, html) {
        var node = el(id);
        if (node) node.innerHTML = html;
    }

    function renderNotice(id, text, isError) {
        var node = el(id);
        if (!node) return;
        if (!text) { node.style.display = 'none'; node.innerHTML = ''; return; }
        node.style.display = '';
        node.style.color = isError ? 'var(--danger)' : 'var(--text-muted)';
        node.innerHTML = isError
            ? '<i class="fas fa-exclamation-triangle"></i> ' + escapeHtml(text)
            : '<i class="fas fa-circle-info"></i> ' + escapeHtml(text);
    }

    // =====================================================================
    // Карточка файлов: переезд внутрь раскрытой карточки результата (R693)
    // =====================================================================
    //
    // ЖИВОЙ ДЕФЕКТ (числа замера на стенде r692, образ webui 18084):
    // «Показать файлы» в списке из 20 моделей отрисовывал 16 строк файлов в
    // карточку #imHfFilesCard, которая стоит в разметке ПОСЛЕ всего списка:
    // кнопка нажата на y≈577, карточка файлов появляется на y≈2995 при окне
    // 768 px, scrollY не меняется. Запрос уходит (GET /hf/files → 200), DOM
    // меняется, а оператор в своём окне не видит НИЧЕГО — «кнопка не работает».
    //
    // ПОЧЕМУ ПЕРЕЕЗД УЗЛА, А НЕ ВТОРОЙ РЕНДЕР. Список файлов — это ещё и рабочий
    // инструмент сборки bundle (галочки + роль у файла). Две копии одной модели
    // (в карточке и внизу) означали бы два комплекта одинаковых чекбоксов
    // data-imh-check и два источника правды. Поэтому узел один: его либо
    // показывают в слоте раскрытой карточки, либо паркуют на прежнее место в
    // разметке (и прячут). renderFiles() пишет всё так же в #imHfFilesBody —
    // точка рендера не меняется, меняется только место, где её видно.
    var filesCardNode = null;
    var filesBodyNode = null;
    // Где карточка жила в разметке: чтобы свернуть — вернуть на место.
    var filesCardHome = null;

    function filesCard() {
        if (!filesCardNode) filesCardNode = el('imHfFilesCard');
        return filesCardNode;
    }

    function filesBody() {
        if (!filesBodyNode) filesBodyNode = el('imHfFilesBody');
        return filesBodyNode;
    }

    /** Слот раскрытой карточки: [data-imh-files-slot="<repo>"]. */
    function filesSlot(repo) {
        if (!repo || typeof document === 'undefined' || !document.querySelector) return null;
        try {
            // Имя репозитория попадает в значение атрибута в кавычках: экранируем
            // только то, что реально ломает CSS-селектор.
            return document.querySelector('[data-imh-files-slot="' +
                String(repo).replace(/\\/g, '\\\\').replace(/"/g, '\\"') + '"]');
        } catch (e) { return null; }
    }

    /** attachFilesCard — показать карточку файлов ВНУТРИ карточки результата. */
    function attachFilesCard(repo) {
        var card = filesCard();
        if (!card) return false;
        // Место в разметке запоминаем до первого переезда (parentNode у карточки
        // ещё исходный) — по нему парковка вернёт её ровно туда же.
        if (!filesCardHome && card.parentNode) {
            filesCardHome = { parent: card.parentNode, before: card.nextSibling || null };
        }
        var slot = filesSlot(repo);
        if (!slot || typeof slot.appendChild !== 'function') {
            // Разметки раскрытой карточки нет (старый index.html, тест-заглушка):
            // не выдумываем вторую точку рендера — карточка просто скрыта.
            card.style.display = 'none';
            return false;
        }
        if (slot.appendChild) slot.appendChild(card);
        card.style.display = '';
        return true;
    }

    /** parkFilesCard — свернуть: карточка возвращается в разметку и скрывается. */
    function parkFilesCard() {
        var card = filesCard();
        if (!card) return;
        card.style.display = 'none';
        if (filesCardHome && filesCardHome.parent && typeof filesCardHome.parent.insertBefore === 'function' &&
            card.parentNode !== filesCardHome.parent) {
            filesCardHome.parent.insertBefore(card, filesCardHome.before);
        }
    }

    /**
     * revealFilesCard — гарантировать, что раскрытая карточка ВИДНА.
     *
     * Панель растёт под кнопкой, поэтому обычно и так в окне. Но если кнопка была
     * у нижней кромки экрана (обычное дело при 20 результатах), виден был бы один
     * пиксель панели — и это снова выглядело бы как «ничего не раскрылось».
     * `block:'nearest'` прокручивает ровно столько, сколько нужно, и никогда не
     * дёргает страницу, если панель уже видна.
     */
    function revealFilesCard() {
        var card = filesCard();
        if (!card || typeof card.scrollIntoView !== 'function') return;
        try {
            card.scrollIntoView({ block: 'nearest', behavior: 'auto' });
        } catch (e) {
            try { card.scrollIntoView(); } catch (e2) { /* старый браузер: не критично */ }
        }
    }

    function renderSearchResults() {
        var host = el('imHfSearchResults');
        if (!host) return;
        if (state.searching) {
            host.innerHTML = '<div class="gguf-empty-state"><i class="fas fa-spinner fa-spin"></i> ' +
                escapeHtml(t('gguf.searching_for', 'Searching for "{q}"...', { q: state.query })) + '</div>';
            return;
        }
        if (state.searchError) {
            host.innerHTML = '<div class="gguf-empty-state gguf-search-error">' +
                '<i class="fas fa-exclamation-triangle" style="color:var(--warning,#e0a030);font-size:var(--font-size-lg);"></i>' +
                '<div style="margin-top:6px;font-weight:500;">' + escapeHtml(t('gguf.search_error', 'Search failed')) + '</div>' +
                '<div style="margin-top:4px;color:var(--text-muted);font-size:var(--font-size-xs);">' + escapeHtml(state.searchError) + '</div>' +
                '</div>';
            return;
        }
        var list = Array.isArray(state.results) ? state.results : [];
        if (!list.length) {
            if (!state.query || state.query.trim().length < 2) {
                host.innerHTML = '<div class="gguf-empty-state gguf-search-empty" style="padding:12px 4px;">' +
                    '<i class="fab fa-huggingface" style="font-size:var(--font-size-lg);color:#ff9a00;"></i> ' +
                    '<span style="font-weight:500;">' + escapeHtml(t('gguf.search_title', 'HuggingFace Hub')) + '</span>' +
                    '<div style="color:var(--text-muted);font-size:var(--font-size-xs);margin-top:6px;">' +
                    escapeHtml(t('imageModels.hf_search_hint', 'Введите 2+ символа или org/model (со слешем) — сразу попадёте к файлам.')) +
                    '</div></div>';
                return;
            }
            host.innerHTML = '<div class="gguf-empty-state"><i class="fas fa-search" style="opacity:0.4;"></i>' +
                '<div style="margin-top:6px;">' + escapeHtml(t('gguf.no_results_for', 'No results for "{q}"', { q: state.query })) + '</div></div>';
            return;
        }
        var rows = list.map(function (m) {
            m = m || {};
            var id = String(m.id || m.modelId || m.name || '');
            var updates = m.lastModified || m.lastUpdated || '';
            var when = updates ? String(updates).slice(0, 10) : '-';
            var selected = id === state.selectedRepo;
            // R693: раскрытая карточка помечена классом, кнопка — состояние
            // (aria-expanded + другая подпись), а под кнопкой стоит слот, КУДА
            // переезжает карточка файлов (#imHfFilesCard, см. attachFilesCard).
            // Слот есть только у раскрытой: список файлов живёт в одном экземпляре,
            // поэтому «две копии одной модели» невозможны by design.
            var expanded = !!state.expandedRepo && id === state.expandedRepo;
            var pickLabel = expanded
                ? t('imageModels.hf_hide_files', 'Свернуть файлы')
                : t('imageModels.hf_pick_files', 'Показать файлы');
            return '<div class="gguf-result-card' + (selected ? ' active' : '') + (expanded ? ' imh-expanded' : '') +
                    '" data-imh-repo="' + escapeHtml(id) + '">' +
                '<div class="gguf-result-header">' +
                    '<div class="gguf-result-title"><i class="fas fa-cube"></i> ' + escapeHtml(id) + '</div>' +
                    '<div class="gguf-result-meta">' +
                        '<span title="' + escapeHtml(t('gguf.downloads', 'Downloads')) + '"><i class="fas fa-download"></i> ' + Number(m.downloads || 0) + '</span>' +
                        '<span title="' + escapeHtml(t('gguf.likes', 'Likes')) + '"><i class="fas fa-heart"></i> ' + Number(m.likes || 0) + '</span>' +
                        '<span title="' + escapeHtml(t('gguf.last_updated', 'Updated')) + '"><i class="fas fa-calendar"></i> ' + escapeHtml(when) + '</span>' +
                        (m.pipelineTag ? '<span class="badge badge-info">' + escapeHtml(String(m.pipelineTag)) + '</span>' : '') +
                    '</div>' +
                '</div>' +
                '<div class="gguf-result-actions" style="display:flex;gap:6px;margin-top:8px;">' +
                    '<button class="btn btn-sm ' + (expanded ? 'btn-secondary' : 'btn-primary') + '"' +
                        ' data-imh-pick="' + escapeHtml(id) + '"' +
                        ' aria-expanded="' + (expanded ? 'true' : 'false') + '">' +
                        '<i class="fas ' + (expanded ? 'fa-chevron-up' : 'fa-list') + '"></i> ' + escapeHtml(pickLabel) +
                    '</button>' +
                '</div>' +
                (expanded ? '<div class="imh-files-slot" data-imh-files-slot="' + escapeHtml(id) + '"></div>' : '') +
            '</div>';
        }).join('');
        host.innerHTML = '<div class="gguf-results-toolbar" style="display:flex;justify-content:space-between;align-items:center;margin-bottom:8px;">' +
            '<span class="gguf-results-count" style="font-size:var(--font-size-xs);color:var(--text-muted);">' +
                escapeHtml(t('gguf.results_count', 'Found: {n}', { n: list.length })) + '</span>' +
            '</div><div class="gguf-results-list">' + rows + '</div>';
        // Разметка списка только что пересобрана — карточка файлов, если она была
        // внутри старой карточки, вместе с ней и уничтожилась из DOM (сам узел
        // жив: ссылка в filesCard()). Возвращаем её в слот раскрытой модели или
        // паркуем обратно (свёрнуто).
        if (state.expandedRepo) attachFilesCard(state.expandedRepo);
        else parkFilesCard();
        // R696: липкая панель и вердикт набора живут в карточке файлов, а она
        // переезжает вместе с этим рендером — обновляем их здесь же.
        renderSelectionSummary();
        renderVerifyVerdict();
    }

    // =====================================================================
    // Рендер: файлы репозитория + сборка bundle
    // =====================================================================

    function roleOptions(selected) {
        return IMAGE_ROLES.map(function (role) {
            // Подпись берём из статического ROLE_LABEL_KEYS (см. комментарий у
            // карты): для оператора «CLIP-L (текстовый энкодер)», а не «clip_l».
            return '<option value="' + role + '"' + (role === selected ? ' selected' : '') + '>' +
                escapeHtml(roleLabel(role)) + '</option>';
        }).join('');
    }

    /**
     * roleLabel — человеческая подпись роли для оператора.
     *
     * ЕДИНСТВЕННЫЙ источник подписи — статическая карта ROLE_LABEL_KEYS (см.
     * комментарий у неё). Никакой склейки вида 'imageModels.role_' + role здесь
     * нет сознательно: ровно такая склейка в подсказке «(предложено: …)» и была
     * дефектом R695 — подсказка собиралась из СЕРВЕРНОГО id (suggestedRole),
     * поэтому рядом с переведённым селектом стояло сырое «clip_l»/«vae».
     *
     * ЧЕГО ЗДЕСЬ НЕТ: падения на роли вне контракта. Сервер вправе прислать
     * что-то новое (или пустое), и подсказка обязана остаться читаемой: для
     * такой роли возвращается сам id как есть (что было нормой для всех 12 до
     * R694), а не «undefined» и не исключение.
     */
    function roleLabel(role) {
        var key = ROLE_LABEL_KEYS[role];
        if (!key) return String(role == null ? '' : role);
        return t(key, role);
    }

    /**
     * formatFamily — для какого семейства спрашивать матрицу форматов.
     *
     * План репозитория знает семейство точнее селектора (он читает заголовок
     * главного файла), поэтому приоритет у plan.family; селектор — запасной путь,
     * пока паспорт не приехал.
     */
    function formatFamily() {
        if (state.plan && state.plan.family) return String(state.plan.family);
        return String(state.bundleFamily || '');
    }

    /** formatMatrix — матрица форматов текущего семейства (или null). */
    function formatMatrix() {
        var m = state.formatMatrices[formatFamily()];
        return (m && !m.error) ? m : null;
    }

    /**
     * ensureFormatMatrix — один запрос на семейство.
     *
     * Сеть не блокирует рендер: сначала рисуем без бейджей (probe-бейджи R696
     * остаются), после ответа перерисовываем список файлов.
     */
    function ensureFormatMatrix(family) {
        var key = String(family || '');
        if (Object.prototype.hasOwnProperty.call(state.formatMatrices, key)) return;
        if (state.formatLoading[key]) return;
        if (!backendId()) return;
        state.formatLoading[key] = true;
        request(backendUrl('formats?family=' + encodeURIComponent(key)), { timeoutMs: TIMEOUT_CONTROL_MS })
            .then(function (data) {
                state.formatMatrices[key] = data || { error: true };
            })
            .catch(function (e) {
                // Ошибка сети: бейджи не рисуем, probe-проверка остаётся источником
                // правды. Повторять на каждый рендер нельзя (шторм запросов).
                state.formatMatrices[key] = { error: true, message: (e && e.message) || String(e) };
            })
            .then(function () {
                state.formatLoading[key] = false;
                if (state.selectedRepo) renderFiles();
            });
    }

    /** formatBadgeHtml — бейдж формата для файла (пусто, пока матрицы нет). */
    function formatBadgeHtml(path, role) {
        var api = window.ImageFormatBadges;
        var m = formatMatrix();
        if (!api || !m) return '';
        return ' ' + api.badgeHtml(m, path, role);
    }

    /** formatSummaryHtml — строка «Форматы: …» под паспортом репозитория. */
    function formatSummaryHtml() {
        var api = window.ImageFormatBadges;
        var m = formatMatrix();
        if (!api || !m) return '';
        var text = api.summaryText(m);
        if (!text) return '';
        return '<div class="ifb-summary">' + escapeHtml(text) + '</div>';
    }

    /** formatMlxWarningHtml — предупреждение о MLX-файлах в репозитории. */
    function formatMlxWarningHtml(files) {
        var api = window.ImageFormatBadges;
        var m = formatMatrix();
        if (!api || !m) return '';
        var names = (files || []).map(filePath);
        return api.mlxWarningHtml(m, names);
    }

    /**
     * DIT_FAMILIES — семейства, которые движок грузит ТОЛЬКО через
     * --diffusion-model (pkg/types/image_model.go: diTFamilies). Держим список
     * здесь ещё и для UI-подсказки: если пред-проверка говорит «DiT», а в
     * профиле выбрано семейство all-in-one, движок ответит
     * «get sd version from file failed» — ровно та ошибка, которую мы ловим.
     */
    var DIT_FAMILIES = ['sd3', 'flux', 'flux2', 'chroma', 'qwen_image', 'z_image'];

    /**
     * repoCompatibility — подсказка по автору репозитория (без сети).
     *
     * ЧЕГО ЗДЕСЬ СОЗНАТЕЛЬНО НЕТ: приговора «ComfyUI-сборку sd.cpp не читает».
     * Проверено на движке pinned master-929-3f8527a: экспорт для ComfyUI
     * (раздельные img_mlp.gate_layer + img_mlp.proj) читается наравне со сборкой
     * leejet (fused img_mlp.gate_up) — qwen_image_2_1.hpp:38-43 поддерживает обе
     * раскладки. Поэтому автор — только слабая подсказка, а вердикт даёт
     * пред-проверка заголовка (кнопка «Проверить»).
     */
    function repoCompatibility(repo) {
        var id = String((repo && (repo.id || repo.modelId || repo.name)) || '').toLowerCase();
        if (!id) {
            return {
                level: 'unknown',
                label: t('imageModels.compat_unknown', 'формат не проверен'),
                hint: ''
            };
        }
        var sdCppAuthors = ['leejet/', 'quantstack/', 'silveroxides/'];
        for (var i = 0; i < sdCppAuthors.length; i++) {
            if (id.indexOf(sdCppAuthors[i]) === 0) {
                return {
                    level: 'ok',
                    label: t('imageModels.compat_ok', 'автор публикует сборки под sd.cpp'),
                    hint: t('imageModels.compat_ok_hint', 'Этот автор публикует сборки для stable-diffusion.cpp. Точный ответ всё равно даёт кнопка «Проверить» у файла: она читает заголовок.')
                };
            }
        }
        return {
            level: 'unknown',
            label: t('imageModels.compat_unknown', 'формат не проверен'),
            hint: t('imageModels.compat_unknown_hint', 'Нажмите «Проверить» у файла: воркер прочитает заголовок (без скачивания) и скажет, какое семейство в нём узнаёт движок.')
        };
    }

    /**
     * probeBadge — вердикт пред-проверки заголовка.
     *
     * supported → движок узнаёт семейство (подпись: какое именно) + предупреждение
     * про --diffusion-model для DiT; unknown → по заголовку не определить
     * (VAE/text encoder/LoRA или незнакомое семейство). Приговоров нет.
     */
    function probeBadge(probe) {
        probe = probe || {};
        // UNSUPPORTED — единственный случай, когда вердикт выносится по факту из
        // шапки: веса в U32 (MLX-квантование), движок такое не читает. Живой случай
        // 2026-10-06: 4 ГБ скачались, загрузка упала с unsupported dtype "U32".
        if (probe.verdict === 'unsupported') {
            return {
                level: 'error',
                label: t('imageModels.probe_unsupported', 'формат не поддерживается'),
                hint: probe.reason || t('imageModels.probe_unsupported_hint',
                    'Это MLX-квантование (веса U32 + scales/biases): stable-diffusion.cpp читает GGUF. Возьмите GGUF-квант этого репозитория.')
            };
        }
        if (probe.verdict === 'supported' && probe.versionLabel) {
            var hint = probe.reason || '';
            if (probe.dit) {
                hint += ' ' + t('imageModels.probe_dit_hint',
                    'Семейство DiT: в профиле bundle должно быть выбрано это же семейство, тогда воркер запустит движок с --diffusion-model.');
            }
            return {
                level: 'ok',
                label: t('imageModels.probe_ok_family', 'движок узнаёт: {version}', { version: probe.versionLabel }),
                hint: hint
            };
        }
        return {
            level: 'unknown',
            label: t('imageModels.probe_unknown', 'версия по заголовку не определяется'),
            hint: probe.reason || ''
        };
    }

    /** diTFamilyWarning — DiT-файл против выбранного семейства профиля. */
    function diTFamilyWarning(probe, selectedFamily) {
        if (!probe || probe.verdict !== 'supported' || !probe.dit) return '';
        var fam = String(selectedFamily || '');
        // Семейство уже DiT-шное (значит движок получит --diffusion-model) или
        // совпадает с определённым по файлу (Wan/PixArt мапятся в «other») —
        // предупреждать не о чем.
        if (!fam || DIT_FAMILIES.indexOf(fam) !== -1 || fam === probe.family) return '';
        return t('imageModels.probe_family_mismatch',
            'Файл — DiT-семейства ({version}), а в профиле выбрано «{family}»: воркер подключит его как all-in-one (--model) и движок ответит «get sd version from file failed». Выберите семейство {suggested}.',
            { version: probe.versionLabel || '', family: fam, suggested: probe.family || 'DiT' });
    }

    /**
     * ditHint — предупреждение о неполном наборе файлов для DiT-семейств.
     *
     * FLUX/SD3/Qwen-Image/Z-Image/Chroma в sd.cpp требуют отдельные VAE и
     * text encoder; если в репозитории только diffusion-файл, bundle не поедет.
     * Это видно по именам файлов ещё до скачивания.
     */
    function ditHint(files, repoId) {
        var id = String(repoId || '').toLowerCase();
        var fam = '';
        ['flux', 'sd3', 'qwen', 'z-image', 'z_image', 'chroma'].forEach(function (f) {
            if (!fam && id.indexOf(f) !== -1) fam = f;
        });
        if (!fam) return '';
        var roles = {};
        (files || []).forEach(function (f) { roles[suggestedRole(f) || 'diffusion'] = true; });
        if (roles.vae && (roles.clip_l || roles.clip_g || roles.t5xxl || roles.llm)) return '';
        var missing = [];
        if (!roles.vae) missing.push('VAE');
        if (!roles.clip_l && !roles.clip_g && !roles.t5xxl && !roles.llm) missing.push('text encoder');
        return t('imageModels.dit_missing',
            'Похоже на семейство {family}: в sd.cpp для него нужны отдельные {missing} — в этом репозитории их нет. Добавьте их файлами из другого репозитория (например, VAE ae.safetensors).',
            { family: fam, missing: missing.join(' + ') });
    }

    function compatBadgeHtml(badge, probePath) {
        badge = badge || {};
        // error — «движок это не прочитает» (MLX-кванты): красный и с явным текстом.
        var color = badge.level === 'ok' ? 'var(--success, #5cb85c)'
            : (badge.level === 'error' ? 'var(--danger, #d9534f)'
                : (badge.level === 'warn' ? 'var(--warning, #e0a030)' : 'var(--text-muted)'));
        var icon = badge.level === 'ok' ? 'fa-circle-check'
            : (badge.level === 'error' ? 'fa-circle-xmark'
                : (badge.level === 'warn' ? 'fa-triangle-exclamation' : 'fa-circle-question'));
        var html = '<span class="imh-compat" title="' + escapeHtml(badge.hint || '') + '" style="color:' + color + ';font-size:var(--font-size-xs);white-space:nowrap;">' +
            '<i class="fas ' + icon + '"></i> ' + escapeHtml(badge.label || '') + '</span>';
        if (probePath) {
            html += ' <button class="btn btn-secondary btn-sm" data-imh-probe="' + escapeHtml(probePath) + '" ' +
                'title="' + escapeHtml(t('imageModels.probe_hint', 'Прочитать заголовок файла (без скачивания) и проверить, прочитает ли его движок')) + '">' +
                escapeHtml(t('imageModels.probe_btn', 'Проверить')) + '</button>';
        }
        return html;
    }

    function renderFiles() {
        var card = filesCard();
        var body = filesBody();
        if (!card || !body) return;
        // Свёрнуто (или репозиторий не выбран) — карточка припаркована и скрыта.
        // Без этого условия пред-проверка, доехавшая после сворачивания, снова
        // показала бы карточку внизу страницы — ровно тот дефект, что чиним.
        if (!state.expandedRepo || !state.selectedRepo) { card.style.display = 'none'; return; }
        card.style.display = '';
        var title = el('imHfRepoName');
        if (title) title.textContent = state.selectedRepo;

        if (state.filesLoading) {
            body.innerHTML = '<div class="gguf-empty-state imh-files-state">' +
                '<i class="fas fa-spinner fa-spin"></i> ' +
                escapeHtml(t('common.loading', 'Loading...')) + '</div>';
            return;
        }
        if (state.filesError) {
            body.innerHTML = '<div class="gguf-empty-state imh-files-state imh-files-error">' +
                '<i class="fas fa-triangle-exclamation" style="color:var(--danger);"></i>' +
                '<div style="margin-top:6px;font-weight:500;">' +
                    escapeHtml(t('imageModels.hf_files_failed', 'Не удалось прочитать файлы репозитория')) +
                '</div>' +
                '<div style="margin-top:4px;color:var(--text-muted);font-size:var(--font-size-xs);">' +
                    escapeHtml(state.filesError) + '</div></div>';
            return;
        }
        var files = Array.isArray(state.files) ? state.files : [];
        if (!files.length) {
            // ПУСТОЕ СОСТОЯНИЕ, А НЕ «ничего не произошло»: у модели реально нет
            // файлов (например, репозиторий только с карточкой модели), и оператор
            // должен понимать, что делать дальше, а не жать кнопку повторно.
            body.innerHTML = '<div class="gguf-empty-state imh-files-state imh-files-empty">' +
                '<i class="fas fa-folder-open" style="opacity:0.45;"></i>' +
                '<div style="margin-top:6px;font-weight:500;">' +
                    escapeHtml(t('imageModels.hf_no_files', 'В этом репозитории нет файлов для скачивания')) +
                '</div>' +
                '<div style="margin-top:4px;color:var(--text-muted);font-size:var(--font-size-xs);white-space:normal;">' +
                    escapeHtml(t('imageModels.hf_no_files_hint',
                        'Возьмите GGUF-квант этой модели: добавьте к запросу «-GGUF» или откройте репозиторий автора — веса могут лежать в соседнем репозитории.')) +
                '</div></div>';
            renderSelectionSummary();
            return;
        }
        // R755: матрица форматов (что движок читает в этой роли) — один запрос на
        // семейство; бейджи рисуются, когда ответ приедет, и не блокируют список.
        ensureFormatMatrix(formatFamily());
        // Предупреждение о неполном наборе (DiT-семейства), вердикт репозитория и
        // расхождение «DiT-файл, а в профиле all-in-one семейство».
        var hint = ditHint(files, state.selectedRepo);
        var repoBadge = repoCompatibility({ id: state.selectedRepo });
        var mainProbe = mainProbeResult(files);
        var header = '';
        var mismatch = diTFamilyWarning(mainProbe, state.bundleFamily);
        if (mismatch) {
            header += '<div class="gguf-local-card" style="margin-bottom:8px;border-left:3px solid var(--warning, #e0a030);">' +
                '<div class="gguf-local-info"><div class="gguf-local-meta" style="white-space:normal;">' +
                '<i class="fas fa-triangle-exclamation"></i> ' + escapeHtml(mismatch) + '</div></div></div>';
        }
        if (hint) {
            header += '<div class="gguf-local-card" style="margin-bottom:8px;border-left:3px solid var(--warning, #e0a030);">' +
                '<div class="gguf-local-info"><div class="gguf-local-meta" style="white-space:normal;">' +
                '<i class="fas fa-triangle-exclamation"></i> ' + escapeHtml(hint) + '</div></div></div>';
        }
        header += '<div style="margin-bottom:8px;font-size:var(--font-size-xs);color:var(--text-muted);">' +
            escapeHtml(t('imageModels.repo_verdict', 'Оценка репозитория')) + ': ' + compatBadgeHtml(repoBadge, '') + '</div>';
        // ПАСПОРТ НАБОРА: что обязательно, чего нет, режим движка и порядок
        // действий. Заменяет собой прежнюю догадку по автору репозитория: вердикт
        // теперь считается по факту (заголовок главного файла + состав файлов).
        header += planHtml(state.plan);
        // R755: «Форматы: …» — ответ на вопрос «примет ли это движок» ДО
        // скачивания, и предупреждение, если в репозитории есть MLX-кванты.
        header += formatSummaryHtml();
        header += formatMlxWarningHtml(files);
        if (mainProbe && mainProbe.verdict === 'supported') {
            header += '<div style="margin-bottom:8px;font-size:var(--font-size-xs);color:var(--text-muted);">' +
                escapeHtml(t('imageModels.main_file_verdict', 'Главный файл')) + ': ' + compatBadgeHtml(probeBadge(mainProbe), '') + '</div>';
        }
        if (state.familyNotice) {
            header += '<div class="gguf-local-card" style="margin-bottom:8px;border-left:3px solid var(--info, #4a90d9);">' +
                '<div class="gguf-local-info"><div class="gguf-local-meta" style="white-space:normal;">' +
                '<i class="fas fa-circle-info"></i> ' + escapeHtml(state.familyNotice) + '</div></div></div>';
        }

        header += files.map(function (f, idx) {
            var path = filePath(f);
            var sel = state.selection[path] || { checked: false, role: suggestedRole(f) || 'diffusion' };
            var role = sel.role;
            var suggested = suggestedRole(f);
            // Метка совместимости: результат пред-проверки, если он есть; иначе —
            // «не проверено» + кнопка «Проверить» (для файлов весов).
            var isWeight = /\.(gguf|safetensors|sft|ckpt)$/i.test(path);
            var probe = state.probes[path];
            // R696: роль по СОДЕРЖИМОМУ заголовка. Живой дефект: файл
            // `Qwen-Image-2.1-Turbo-Uncensored-AD-Q4_K.gguf` назван как модель, а
            // внутри — текстовый энкодер Qwen3-VL (general.architecture=qwen3vl);
            // имя подсказывало роль diffusion, и движок падал с «get sd version
            // from file failed». Показываем расхождение прямо у файла.
            var detected = probe && probe.role && probe.role !== role ? String(probe.role) : '';
            var detectedHint = detected
                ? ' <span style="font-size:var(--font-size-xs);color:var(--warning, #e0a030);" title="' +
                    escapeHtml(String(probe.reason || '')) + '">(' +
                    escapeHtml(t('imageModels.hf_detected_role', 'по заголовку: {role}', { role: roleLabel(detected) })) +
                    ')</span>'
                : '';
            // Расхождение роли и подсказки сервера показываем как есть: это
            // сигнал «оператор выбрал другое», а не ошибка. Подпись в подсказке
            // — та же карта ROLE_LABEL_KEYS, что и у селекта (R695): серверный
            // id («clip_l», «vae») оператору ничего не говорит, а рядом стояли
            // человеческие подписи — расхождение и было дефектом.
            var roleHint = suggested && suggested !== role
                ? ' <span style="font-size:var(--font-size-xs);color:var(--text-muted);">(' + escapeHtml(t('imageModels.hf_suggested', 'предложено')) + ': ' + escapeHtml(roleLabel(suggested)) + ')</span>'
                : '';
            // R755: бейдж формата — «примет ли движок этот файл в выбранной роли»
            // (GGUF/нужен GGUF/MLX…). Точный вердикт по содержимому даёт probe ниже.
            var formatBadge = formatBadgeHtml(path, role);
            var compat = '';
            if (probe) {
                compat = compatBadgeHtml(probeBadge(probe), isWeight ? path : '');
            } else if (state.probing[path]) {
                compat = '<span style="font-size:var(--font-size-xs);color:var(--text-muted);"><i class="fas fa-spinner fa-spin"></i> ' +
                    escapeHtml(t('imageModels.probe_running', 'проверяю заголовок…')) + '</span>';
            } else if (isWeight) {
                compat = compatBadgeHtml({ level: 'unknown', label: t('imageModels.probe_not_checked', 'не проверено'), hint: '' }, path);
            }
            return '<div class="gguf-file-item" data-imh-file="' + escapeHtml(path) + '">' +
                '<label class="gguf-file-name" style="display:flex;align-items:center;gap:8px;cursor:pointer;">' +
                    '<input type="checkbox" data-imh-check="' + escapeHtml(path) + '"' + (sel.checked ? ' checked' : '') + '>' +
                    '<span style="font-family:monospace;font-size:var(--font-size-xs);word-break:break-all;">' + escapeHtml(path) + '</span>' +
                '</label>' +
                '<select class="form-control" data-imh-role="' + escapeHtml(path) + '" style="max-width:200px;font-size:var(--font-size-xs);">' +
                    roleOptions(role) +
                '</select>' + roleHint + detectedHint + formatBadge +
                compat +
                '<span class="gguf-file-size">' + escapeHtml(formatBytes(fileSize(f))) + '</span>' +
            '</div>';
        }).join('');
        // R710: добавленные компоненты (из ДРУГИХ репозиториев) — отдельными
        // строками с указанием репозитория и кнопкой «убрать». Иначе оператор не
        // видел бы, что именно и откуда будет скачано вместе с набором.
        header += (state.extraComponents || []).map(function (c) {
            return '<div class="gguf-file-item imh-extra-component" data-imh-extra="' + escapeHtml(String(c.role || '')) + '">' +
                '<span class="gguf-file-name" style="display:flex;align-items:center;gap:8px;">' +
                    '<i class="fas fa-plus-circle" style="color:var(--success, #5cb85c);"></i>' +
                    '<span style="font-family:monospace;font-size:var(--font-size-xs);word-break:break-all;">' +
                        escapeHtml(String(c.filename || '')) + '</span>' +
                '</span>' +
                '<span style="font-size:var(--font-size-xs);color:var(--text-muted);">' +
                    escapeHtml(roleLabel(c.role)) + ' · ' + escapeHtml(String(c.repo || '')) +
                '</span>' + formatBadgeHtml(c.filename, c.role) +
                '<span class="gguf-file-size">' + escapeHtml(c.sizeBytes ? formatBytes(c.sizeBytes) : '-') + '</span>' +
                '<button class="btn btn-secondary btn-sm" data-icom-remove="' + escapeHtml(String(c.role || '')) + '"' +
                    ' title="' + escapeHtml(t('imageModels.component_remove_hint', 'Убрать этот компонент из набора')) + '">' +
                    '<i class="fas fa-xmark"></i></button>' +
            '</div>';
        }).join('');
        body.innerHTML = header;
        renderSelectionSummary();
        renderVerifyVerdict();
    }

    function selectedCount() {
        return Object.keys(state.selection || {}).filter(function (p) {
            return state.selection[p] && state.selection[p].checked;
        }).length;
    }

    /** selectedBytes — суммарный объём ОТМЕЧЕННЫХ файлов (замер по sizeBytes). */
    function selectedBytes() {
        var total = 0;
        var files = Array.isArray(state.files) ? state.files : [];
        Object.keys(state.selection || {}).forEach(function (p) {
            if (!state.selection[p] || !state.selection[p].checked) return;
            var f = files.filter(function (x) { return filePath(x) === p; })[0];
            total += fileSize(f);
        });
        return total;
    }

    /**
     * renderSelectionSummary — сводка выбора в ДВУХ местах: в шапке карточки
     * (как было) и в липкой панели действий (R696).
     *
     * ЗАЧЕМ ПОВТОР. Живой дефект: при 16+ файлах список уходил за нижнюю кромку
     * окна, а кнопка скачивания была в отдельной карточке НИЖЕ — оператор не
     * видел, что скачается всё отмеченное. Панель прилипает к верху списка и
     * показывает счётчик и объём без прокрутки; кнопка «Скачать выбранное»
     * отключается при пустом выборе, чтобы «скачать ничего» было невозможно.
     */
    function renderSelectionSummary() {
        var n = selectedCount();
        var total = selectedBytes();
        var label = t('imageModels.hf_selected_count', 'Выбрано файлов: {n}', { n: n }) +
            (n ? ' · ' + formatBytes(total) : '');
        var node = el('imHfSelectionSummary');
        if (node) node.textContent = label;
        var sticky = el('imHfSelectionSummarySticky');
        if (sticky) {
            sticky.textContent = n
                ? t('imageModels.hf_selected_summary', 'Выбрано: {n} файлов · {size}', { n: n, size: formatBytes(total) })
                : t('imageModels.hf_nothing_selected', 'Ничего не выбрано — скачается только отмеченное');
        }
        // R749: кнопка скачивания, построчный состав и состояние узла живут в
        // корзине наверху вкладки - обновляем их тем же вызовом, что и сводку.
        renderCart();
    }

    // =====================================================================
    // R749 (2026-11-25): корзина загрузки на верху вкладки HuggingFace
    // =====================================================================
    //
    // ЖИВОЙ ДЕФЕКТ (скриншот оператора). Имя bundle, семейство, HF-токен и
    // кнопка скачивания жили в ОТДЕЛЬНОЙ карточке ПОД списком файлов, а вверху
    // были только галочки. При 16+ файлах оператор отмечал набор и не видел:
    //   - КУДА скачает (имя набора, семейство, бэкенд, репозиторий);
    //   - ЧТО именно в корзине (роль, размер каждого файла);
    //   - ЧЕГО не хватает на узле (какие файлы уже лежат, какие нет).
    // Теперь цель, состав и состояние стоят одной карточкой над списком, а
    // кнопки «Скачать выбранное» / «Скачать недостающее» работают с этим же
    // составом. Карточка липкая (pages.css: .imh-cart).
    //
    // ОТКУДА БЕРЁТСЯ «ЕСТЬ НА УЗЛЕ». GET .../models (тот же прокси, что у
    // таблицы «Модели на диске») отдаёт модели с files[]: filename + localPath.
    // Индекс строится один раз на показ/смену бэкенда и сверяется по полному
    // пути и по имени файла (регистр не важен). Это подсказка о РЕЕСТРЕ узла, а
    // не инвентаризация диска: файл, положенный мимо реестра, здесь не виден, и
    // это честно написано в подсказке. Точная инвентаризация с докачкой и
    // перекачкой - следующий этап.

    /** normPathKey / basenameKey — ключи сверки путей (регистр не важен). */
    function normPathKey(path) {
        return String(path == null ? '' : path).replace(/\\/g, '/').replace(/^\.?\//, '').toLowerCase();
    }

    function basenameKey(path) {
        var s = String(path == null ? '' : path).replace(/\\/g, '/');
        return s.slice(s.lastIndexOf('/') + 1).toLowerCase();
    }

    /**
     * buildBackendFileIndex — индекс файлов узла по ответу GET /api/image/models.
     *
     * Формы ответа: массив моделей либо {models:[...]}; у модели files[] с
     * filename (имя внутри репозитория) и localPath (путь на диске после
     * bundle-загрузки). Кладём ОБА ключа и дополнительно ключ по имени файла:
     * оператор мог собрать bundle, где файл лежит в подкаталоге, а реестр знает
     * только имя. Совпадение по имени - подсказка, а не приговор (см. Stage 3).
     */
    function buildBackendFileIndex(data) {
        var arr = Array.isArray(data) ? data
            : (data && Array.isArray(data.models) ? data.models
                : (data && Array.isArray(data.data) ? data.data : []));
        var paths = Object.create(null);
        var models = 0;
        arr.forEach(function (m) {
            if (!m) return;
            var modelName = String(m.name || m.id || '');
            var files = Array.isArray(m.files) ? m.files : [];
            if (modelName && files.length) models++;
            files.forEach(function (f) {
                if (!f) return;
                var entry = { model: modelName, role: String(f.role || '') };
                [f.filename, f.localPath, f.local_path].forEach(function (p) {
                    var full = normPathKey(p);
                    if (full) paths[full] = entry;
                    var base = p ? basenameKey(p) : '';
                    if (base) paths['*' + base] = entry;
                });
            });
        });
        return { paths: paths, models: models, at: Date.now() };
    }

    /** presenceLookup — запись реестра узла для пути файла (или null). */
    function presenceLookup(index, path) {
        var paths = (index && index.paths) || {};
        var full = normPathKey(path);
        if (full && paths[full]) return paths[full];
        var base = basenameKey(path);
        return (base && paths['*' + base]) || null;
    }

    /** presenceOf — состояние файла на узле: known=false, пока реестр не прочитан. */
    function presenceOf(path) {
        if (!backendId()) return { known: false, present: false, model: '', role: '' };
        if (!state.backendFiles || state.backendFilesBackendId !== backendId()) {
            return { known: false, present: false, model: '', role: '' };
        }
        var hit = presenceLookup(state.backendFiles, path);
        return hit
            ? { known: true, present: true, model: hit.model, role: hit.role }
            : { known: true, present: false, model: '', role: '' };
    }

    /**
     * refreshBackendFiles — перечитать реестр узла.
     *
     * Один запрос на показ таба и повторно после завершения bundle (файлы
     * появились в реестре) либо по кнопке «Перечитать узел» (только когда реестр
     * не прочитан - постоянной кнопки обновления здесь нет, R493).
     */
    function refreshBackendFiles() {
        var id = backendId();
        if (!id) {
            state.backendFiles = null;
            state.backendFilesError = '';
            state.backendFilesBackendId = '';
            renderCart();
            return Promise.resolve(null);
        }
        if (state.backendFilesBusy) return state.backendFilesBusy;
        state.backendFilesError = '';
        // R749: запоминаем, ДЛЯ КАКОЙ машины идёт попытка, ещё до ответа. Иначе
        // при недоступном узле backendFilesBackendId оставался бы пустым, смена
        // бэкенда не сбрасывала бы ошибку, а ensureBackendFiles мог бы заново
        // запускать чтение на каждом рендере (шторм запросов к лежащему узлу).
        state.backendFilesBackendId = id;
        var p = request(backendUrl('models'), { timeoutMs: TIMEOUT_CONTROL_MS }).then(function (data) {
            state.backendFiles = buildBackendFileIndex(data);
            return state.backendFiles;
        }).catch(function (e) {
            state.backendFilesError = (e && e.message) || String(e);
            // Старый индекс не выбрасываем: он всё ещё описывает узел, но
            // подсветим, что прочитать свежий реестр не удалось.
            return null;
        }).then(function (idx) {
            state.backendFilesBusy = null;
            renderCart();
            return idx;
        });
        state.backendFilesBusy = p;
        return p;
    }

    /**
     * ensureBackendFiles — прочитать реестр, только если он ещё не прочитан.
     *
     * ОШИБКА НЕ ПОВТОРЯЕТСЯ САМА. После неудачи (узел недоступен) авточтение
     * молчит: иначе каждый рендер корзины порождал бы новый запрос к лежащему
     * узлу. Повтор — только действием оператора («Перечитать узел») или
     * «Скачать недостающее» (оно перечитывает реестр перед решением).
     */
    function ensureBackendFiles() {
        var id = backendId();
        if (!id) return;
        if (state.backendFiles && state.backendFilesBackendId === id) return;
        if (state.backendFilesBusy) return;
        // Смена бэкенда: индекс и ошибка относятся к прежней машине.
        if (state.backendFilesBackendId && state.backendFilesBackendId !== id) {
            state.backendFiles = null;
            state.backendFilesError = '';
        }
        if (state.backendFilesError) return;
        refreshBackendFiles();
    }

    /** backendLabel — человеческая подпись выбранного бэкенда для строки «Куда». */
    function backendLabel() {
        var id = backendId();
        if (!id) return '';
        var label = id;
        try {
            var pageState = window.ImagePage && window.ImagePage._state;
            var list = (pageState && pageState.backends) || [];
            list.forEach(function (b) {
                if (!b || b.id !== id) return;
                label = String(b.name || b.id || id);
                if (b.host) label += ' (' + b.host + (b.imagePort ? ':' + b.imagePort : '') + ')';
            });
        } catch (e) { /* страница моделей может быть не смонтирована */ }
        return label;
    }

    /**
     * cartEntries — состав корзины: отмеченные файлы репозитория + компоненты
     * из других репозиториев (R710). Порядок - как в списке файлов.
     */
    function cartEntries() {
        var out = [];
        var files = Array.isArray(state.files) ? state.files : [];
        Object.keys(state.selection || {}).forEach(function (p) {
            var s = state.selection[p];
            if (!s || !s.checked) return;
            var f = files.filter(function (x) { return filePath(x) === p; })[0];
            out.push({
                path: p,
                role: s.role || suggestedRole(f) || 'diffusion',
                sizeBytes: Number(s.sizeBytes || fileSize(f) || 0),
                repo: state.selectedRepo || '',
                probe: state.probes[p] || null,
                extra: false
            });
        });
        (state.extraComponents || []).forEach(function (c) {
            if (!c || !c.role || !c.filename) return;
            out.push({
                path: String(c.filename),
                role: String(c.role),
                sizeBytes: Number(c.sizeBytes || 0),
                repo: String(c.repo || ''),
                probe: null,
                extra: true
            });
        });
        return out;
    }

    /** cartStatusHtml — цветные метки строки: состояние на узле + пред-проверка. */
    function cartStatusHtml(entry) {
        var p = presenceOf(entry.path);
        var html = '';
        if (!p.known) {
            html += '<span class="imh-cart-status unknown" title="' +
                escapeHtml(t('imageModels.cart_unknown_hint',
                    'Список файлов узла не прочитан ({error}).', { error: state.backendFilesError || '' })) + '">' +
                escapeHtml(t('imageModels.cart_unknown', 'узел не прочитан')) + '</span>';
        } else if (p.present) {
            html += '<span class="imh-cart-status ok" title="' +
                escapeHtml(String(p.model || '') + (p.role ? ' · ' + roleLabel(p.role) : '')) + '">' +
                escapeHtml(t('imageModels.cart_present', 'есть на узле')) + '</span>';
        } else {
            html += '<span class="imh-cart-status absent">' +
                escapeHtml(t('imageModels.cart_absent', 'нет на узле')) + '</span>';
        }
        if (entry.probe) {
            var badge = probeBadge(entry.probe);
            html += ' <span class="imh-cart-status ' +
                (badge.level === 'ok' ? 'ok' : (badge.level === 'error' ? 'bad' : 'unknown')) +
                '" title="' + escapeHtml(badge.hint || '') + '">' +
                escapeHtml(badge.label || '') + '</span>';
        }
        return html;
    }

    /**
     * renderCart — цель, состав, итог и кнопки корзины.
     *
     * Ничего не запрашивает сама: реестр узла читает refreshBackendFiles (один
     * раз), этот рендер только показывает state. Поэтому отметка галочки
     * обновляет корзину мгновенно, без сети.
     */
    function renderCart() {
        var card = el('imHfCartCard');
        // Старая разметка (без корзины): тихо выходим - модуль обязан работать
        // и на странице, где карточки ещё нет.
        if (!card) return;
        ensureBackendFiles();

        var entries = cartEntries();
        var totalBytes = 0;
        var presentCount = 0;
        var missingCount = 0;
        var missingBytes = 0;
        var unknownCount = 0;
        entries.forEach(function (e) {
            totalBytes += e.sizeBytes;
            var p = presenceOf(e.path);
            if (!p.known) { unknownCount++; return; }
            if (p.present) { presentCount++; return; }
            missingCount++;
            missingBytes += e.sizeBytes;
        });

        // --- цель: имя, семейство, бэкенд, репозиторий ---
        var target = el('imHfCartTarget');
        if (target) {
            var nameInput = el('imgBundleName');
            var familySel = el('imgBundleFamily');
            var name = nameInput && nameInput.value ? String(nameInput.value).trim() : '';
            var family = familySel && familySel.value ? String(familySel.value) : (state.bundleFamily || '');
            var repo = state.selectedRepo || '';
            target.innerHTML =
                '<span class="imh-cart-scope-item"><strong>' +
                    escapeHtml(t('imageModels.cart_target', 'Куда')) + ':</strong> ' +
                    (name
                        ? '<span style="font-family:var(--font-family-mono);">' + escapeHtml(name) + '</span>'
                        : '<span style="color:var(--warning, #e0a030);">' +
                            escapeHtml(t('imageModels.cart_name_unset', 'имя не задано')) + '</span>') +
                '</span>' +
                '<span class="imh-cart-scope-item"><strong>' +
                    escapeHtml(t('imageModels.cart_family', 'Семейство')) + ':</strong> ' +
                    escapeHtml(family || '-') +
                '</span>' +
                '<span class="imh-cart-scope-item"><strong>' +
                    escapeHtml(t('imageModels.cart_backend', 'Бэкенд')) + ':</strong> ' +
                    escapeHtml(backendLabel() || '-') +
                '</span>' +
                '<span class="imh-cart-scope-item"><strong>' +
                    escapeHtml(t('imageModels.cart_repo', 'Репозиторий')) + ':</strong> ' +
                    (repo ? escapeHtml(repo)
                        : escapeHtml(t('imageModels.cart_repo_unset', 'не выбран'))) +
                '</span>';
        }

        // --- реестр узла: справочная строка в шапке списка ---
        var nodeState = el('imHfCartNodeState');
        if (nodeState) {
            var idx = state.backendFiles;
            if (state.backendFilesError && state.backendFilesBusy === null) {
                nodeState.textContent = t('imageModels.cart_scan_failed',
                    'Не удалось прочитать список файлов узла: {error}', { error: state.backendFilesError });
            } else if (!idx || state.backendFilesBackendId !== backendId()) {
                nodeState.textContent = t('imageModels.cart_unknown', 'узел не прочитан');
            } else {
                nodeState.textContent = t('imageModels.cart_registry',
                    'в реестре узла: {n} моделей', { n: idx.models });
            }
        }

        // --- состав ---
        var list = el('imHfCartList');
        if (list) {
            if (!entries.length) {
                list.innerHTML = '<div class="imh-cart-empty">' +
                    escapeHtml(t('imageModels.cart_empty',
                        'Корзина пуста: найдите репозиторий и отметьте файлы — здесь появится роль, размер и состояние каждого файла на узле.')) +
                    '</div>';
            } else {
                list.innerHTML = entries.map(function (e) {
                    var nameCell = escapeHtml(e.path);
                    if (e.extra) {
                        nameCell = '<i class="fas fa-plus-circle" style="color:var(--success, #5cb85c);"></i> ' +
                            nameCell + ' <span style="color:var(--text-muted);">(' +
                            escapeHtml(t('imageModels.cart_extra_source', 'из {repo}', { repo: e.repo })) +
                            ')</span>';
                    }
                    return '<div class="imh-cart-item" data-imh-cart-path="' + escapeHtml(e.path) + '">' +
                        '<span class="imh-cart-role">' + escapeHtml(roleLabel(e.role)) + '</span>' +
                        '<span class="imh-cart-name">' + nameCell + '</span>' +
                        '<span class="imh-cart-size">' + escapeHtml(formatBytes(e.sizeBytes)) + '</span>' +
                        '<span class="imh-cart-status-wrap">' + cartStatusHtml(e) + '</span>' +
                    '</div>';
                }).join('');
            }
        }

        // --- итог ---
        var totals = el('imHfCartTotals');
        if (totals) {
            if (!entries.length) {
                totals.innerHTML = escapeHtml(t('imageModels.cart_empty',
                    'Корзина пуста: найдите репозиторий и отметьте файлы — здесь появится роль, размер и состояние каждого файла на узле.'));
            } else {
                var parts = [t('imageModels.cart_totals', 'В корзине: {n} файлов · {size}',
                    { n: entries.length, size: formatBytes(totalBytes) })];
                if (!unknownCount) {
                    parts.push(t('imageModels.cart_on_node', 'на узле: {n}', { n: presentCount }));
                    if (missingCount) {
                        parts.push(t('imageModels.cart_missing_count', 'не хватает: {n} · {size}',
                            { n: missingCount, size: formatBytes(missingBytes) }));
                    } else {
                        parts.push(t('imageModels.cart_all_present', 'Всё выбранное уже есть на узле'));
                    }
                } else if (state.backendFilesError) {
                    parts.push(t('imageModels.cart_unknown_hint',
                        'Список файлов узла не прочитан ({error}).', { error: state.backendFilesError }));
                } else {
                    parts.push(t('imageModels.cart_unknown', 'узел не прочитан'));
                }
                var html = escapeHtml(parts.join(' · '));
                if (state.backendFilesError && state.backendFilesBusy === null) {
                    // Старый индекс ещё показывает состояние, но прочитать свежий
                    // реестр не удалось: говорим это прямо, а не молчим.
                    if (!unknownCount) {
                        html += ' · ' + escapeHtml(t('imageModels.cart_scan_failed',
                            'Не удалось прочитать список файлов узла: {error}',
                            { error: state.backendFilesError }));
                    }
                    html += ' <button class="btn btn-sm btn-secondary" data-imh-action="scan-backend">' +
                        escapeHtml(t('imageModels.cart_scan', 'Перечитать узел')) + '</button>';
                }
                totals.innerHTML = html;
            }
        }

        // --- кнопки ---
        var dl = el('imHfDownloadSelected');
        if (dl) {
            dl.disabled = entries.length === 0;
            dl.innerHTML = '<i class="fas fa-download"></i> ' +
                escapeHtml(t('imageModels.hf_download_selected', 'Скачать выбранное')) +
                ' (' + entries.length + (totalBytes ? ' · ' + formatBytes(totalBytes) : '') + ')';
        }
        var dm = el('imHfDownloadMissing');
        if (dm) {
            var registryKnown = !!(state.backendFiles && state.backendFilesBackendId === backendId() && !state.backendFilesError);
            dm.disabled = entries.length === 0 || !registryKnown || missingCount === 0;
            dm.innerHTML = '<i class="fas fa-circle-down"></i> ' +
                escapeHtml(t('imageModels.cart_download_missing', 'Скачать недостающее')) +
                ' (' + (registryKnown ? missingCount : '?') +
                (registryKnown && missingBytes ? ' · ' + formatBytes(missingBytes) : '') + ')';
            dm.title = !registryKnown
                ? t('imageModels.cart_unknown_hint', 'Список файлов узла не прочитан ({error}).',
                    { error: state.backendFilesError || '' })
                : (missingCount === 0 ? t('imageModels.cart_all_present', 'Всё выбранное уже есть на узле') : '');
        }
    }

    /**
     * downloadMissing — скачать только те отмеченные файлы, которых нет на узле.
     *
     * Реестр перечитывается ПЕРЕД решением: между составлением корзины и
     * нажатием файл мог доехать. Если реестр прочитать не удалось, обещать
     * «недостающее» нельзя - оператор получает ошибку, а не молчаливое
     * скачивание всего набора.
     */
    async function downloadMissing() {
        if (!requireBackend()) return false;
        var entries = cartEntries();
        if (!entries.length) {
            toast(t('imageModels.hf_none_selected', 'Отметьте хотя бы один файл модели'), 'warn');
            return false;
        }
        var idx = await refreshBackendFiles();
        if (!idx) {
            toast(t('imageModels.cart_scan_failed',
                'Не удалось прочитать список файлов узла: {error}', { error: state.backendFilesError || '' }), 'error');
            return false;
        }
        var missing = entries.filter(function (e) { return !presenceOf(e.path).present; });
        if (!missing.length) {
            toast(t('imageModels.cart_all_present', 'Всё выбранное уже есть на узле'), 'info');
            return false;
        }
        toast(t('imageModels.cart_missing_toast', 'Скачиваю недостающее: {n} файлов', { n: missing.length }), 'info');
        state.onlyMissing = true;
        try {
            return await startBundle({ onlyMissing: true });
        } finally {
            state.onlyMissing = false;
        }
    }

    // =====================================================================
    // R710 (2026-11-24): недостающие компоненты набора (VAE / text encoder)
    // =====================================================================
    //
    // ЖИВОЙ ДЕФЕКТ (скриншоты пользователя). Вердикт говорит «imageworker НЕ
    // сможет загрузить — не хватает: VAE, text encoder (LLM)», а в самом
    // репозитории (`Qwen-Image-Edit-2511`, `Qwen-Image-2.1-Turbo-Uncensored`)
    // этих файлов нет: там только diffusion. Подсказка советовала «добавьте их
    // файлами из другого репозитория», но открыть другой репозиторий и добавить
    // из него ОДИН файл к текущему набору было невозможно.
    //
    // ЧТО ЗДЕСЬ. Рядом со строкой «не хватает: …» появляются кнопки:
    //   «Добавить VAE (по умолчанию)»     — только если в каталоге есть default;
    //   «Добавить text encoder (…)»       — то же для каждой недостающей роли;
    //   «Скачать недостающее (N)»         — когда стандартных компонентов ≥ 2;
    //   «Выбрать из другого репозитория…» — ручной путь (репозиторий + файлы).
    // Каталог приходит с сервера (GET /api/v1/image/components/catalog):
    // репозитории, файлы, размеры и лицензии — ДАННЫЕ, а не хардкод в UI.
    //
    // ДВА РЕЖИМА (поле mode):
    //   selection — вкладка HF, набор ещё не скачан: компонент добавляется в
    //               ВЫБОР (state.extraComponents) и уходит в общий POST
    //               /api/hf/bundle. Показываем его отдельной строкой, чтобы
    //               оператор видел, что именно и откуда будет скачано;
    //   bundle    — страница «Модели на диске», модель уже на диске:
    //               POST /api/v1/image/models/{name}/components пишет файл в
    //               каталог модели и роль в profile.json, воркер пересканирует
    //               реестр, и вердикт пересчитывается.

    /** componentRoleRankVal — порядок кнопок: VAE, затем энкодеры. */
    function componentRoleRankVal(role) {
        var r = COMPONENT_ROLE_RANK[String(role)];
        return (r === undefined) ? 9 : r;
    }

    /**
     * missingComponentRoles — роли, которых не хватает и которые МОЖНО добавить.
     *
     * Источник — роли вердикта (compat.roles: required && !found), а не строка
     * missing: в ней лежат ПОДПИСИ («text encoder (LLM)»), а кнопке нужен id
     * роли, чтобы найти компонент в каталоге.
     */
    function missingComponentRoles(compat) {
        if (!compat || !Array.isArray(compat.roles)) return [];
        var out = [];
        compat.roles.forEach(function (r) {
            if (!r || !r.required || r.found) return;
            var role = String(r.role || '');
            if (COMPONENT_ROLES.indexOf(role) === -1) return; // diffusion — не компонент
            if (out.indexOf(role) === -1) out.push(role);
        });
        out.sort(function (a, b) { return componentRoleRankVal(a) - componentRoleRankVal(b); });
        return out;
    }

    /** normalizeComponent — единая форма компонента для UI (sizeBytes из sizeMb). */
    function normalizeComponent(c, family) {
        c = c || {};
        var files = Array.isArray(c.files) ? c.files.slice() : [];
        var sizeMb = Number(c.sizeMb || c.sizeMB || 0);
        return {
            family: String(c.family || family || ''),
            role: String(c.role || ''),
            label: String(c.label || ''),
            repo: String(c.repo || ''),
            files: files,
            filename: files.length ? String(files[0]) : '',
            sizeMb: sizeMb,
            sizeBytes: sizeMb * 1024 * 1024,
            default: c.default === true,
            note: String(c.note || ''),
            license: String(c.license || '')
        };
    }

    /** componentSizeText — «335 МБ» из МиБ каталога (0 → пустая строка). */
    function componentSizeText(sizeMb) {
        var mb = Number(sizeMb || 0);
        if (!mb || isNaN(mb)) return '';
        return formatBytes(mb * 1024 * 1024);
    }

    /** defaultComponentFor — стандартный компонент роли из каталога (или null). */
    function defaultComponentFor(catalog, family, role) {
        var list = (catalog && Array.isArray(catalog.components)) ? catalog.components : [];
        for (var i = 0; i < list.length; i++) {
            var c = list[i];
            if (c && String(c.role) === String(role) && c.default === true) {
                return normalizeComponent(c, family);
            }
        }
        return null;
    }

    /** componentTooltip — что именно и откуда (title кнопки). */
    function componentTooltip(comp) {
        comp = comp || {};
        var parts = [String(comp.repo || '')];
        if (comp.files && comp.files.length) parts.push(comp.files.join(', '));
        if (comp.sizeMb) parts.push(componentSizeText(comp.sizeMb));
        if (comp.license) parts.push(t('imageModels.component_license_prefix', 'лицензия') + ': ' + comp.license);
        return parts.join(' · ');
    }

    /**
     * componentButtonsHtml — кнопки «чем починить набор».
     *
     * ГЛАВНОЕ ПРАВИЛО: «Добавить X (по умолчанию)» рисуется ТОЛЬКО когда в
     * каталоге есть default-компонент этой роли. Нет каталога (неизвестное
     * семейство) — кнопок «по умолчанию» нет вовсе, остаётся ручной путь и
     * честная строка «каталога для семейства нет».
     *
     * Функция ЧИСТАЯ (каталог приходит параметром) — её проверяет тест
     * «кнопки только при наличии default», без DOM и сети.
     */
    function componentButtonsHtml(missing, opts) {
        opts = opts || {};
        var roles = Array.isArray(missing) ? missing : [];
        var catalog = opts.catalog || null;
        var mode = opts.mode === 'bundle' ? 'bundle' : 'selection';
        var family = String(opts.family || 'other');
        if (!roles.length) return '';
        var defaults = [];
        roles.forEach(function (role) {
            var comp = defaultComponentFor(catalog, family, role);
            if (comp) defaults.push(comp);
        });
        var html = '<div class="imh-component-actions" style="display:flex;gap:6px;flex-wrap:wrap;margin-top:8px;">';
        // data-icom-model нужен странице «Модели на диске»: там кнопки рисуются
        // по строке таблицы, а обработчик клика знает только нажатый элемент.
        var modelAttr = opts.model ? ' data-icom-model="' + escapeHtml(String(opts.model)) + '"' : '';
        defaults.forEach(function (comp) {
            html += '<button class="btn btn-primary btn-sm" data-icom-default="' + escapeHtml(comp.role) + '"' +
                ' data-icom-mode="' + escapeHtml(mode) + '"' + modelAttr +
                ' title="' + escapeHtml(componentTooltip(comp)) + '">' +
                '<i class="fas fa-plus"></i> ' +
                escapeHtml(t('imageModels.component_add_default', 'Добавить {role} (по умолчанию)',
                    { role: roleLabel(comp.role) })) +
                '</button>';
        });
        html += '<button class="btn btn-secondary btn-sm" data-icom-manual="1"' +
            ' data-icom-mode="' + escapeHtml(mode) + '"' + modelAttr +
            ' title="' + escapeHtml(t('imageModels.component_manual_hint', 'Указать репозиторий и файл вручную')) + '">' +
            '<i class="fas fa-folder-open"></i> ' +
            escapeHtml(t('imageModels.component_manual', 'Выбрать из другого репозитория…')) + '</button>';
        if (defaults.length > 1) {
            html += '<button class="btn btn-primary btn-sm" data-icom-all="1"' +
                ' data-icom-mode="' + escapeHtml(mode) + '"' + modelAttr + '>' +
                '<i class="fas fa-download"></i> ' +
                escapeHtml(t('imageModels.component_add_all', 'Скачать недостающее ({n})', { n: defaults.length })) +
                '</button>';
        }
        html += '</div>';
        if (!catalog || catalog.known === false) {
            html += '<div class="imh-verdict-reason" style="margin-top:4px;">' +
                escapeHtml(t('imageModels.component_no_catalog',
                    'Каталога стандартных компонентов для семейства «{family}» нет: укажите репозиторий и файл вручную.',
                    { family: family })) + '</div>';
        }
        return html;
    }

    /** componentConfirmText — подтверждение с ЧИСЛАМИ (репозиторий, файл, размер). */
    function componentConfirmText(comp, opts) {
        opts = opts || {};
        comp = comp || {};
        var lines = [];
        lines.push(t('imageModels.component_confirm_line',
            'Будет добавлено: {role} {file} ({size}) из {repo}', {
                role: roleLabel(comp.role),
                file: comp.filename || (comp.files || []).join(', '),
                size: componentSizeText(comp.sizeMb) || '?',
                repo: comp.repo
            }));
        lines.push(t('imageModels.component_confirm_standard',
            'Это стандартный для семейства {family} компонент.', { family: comp.family || opts.family || 'other' }));
        if (comp.license) {
            lines.push(t('imageModels.component_confirm_license', 'Лицензия: {license}', { license: comp.license }));
        }
        if (comp.note) lines.push(comp.note);
        lines.push(opts.mode === 'bundle'
            ? t('imageModels.component_confirm_bundle',
                'Текущие файлы набора не изменятся: файл скачается в каталог модели, роль пропишется в profile.json.')
            : t('imageModels.component_confirm_selection',
                'Файл добавится к набору и скачается вместе с остальными файлами.'));
        if (opts.more) lines.push(opts.more);
        return lines.join('\n');
    }

    /** componentWarningText — предупреждение «добавлено по умолчанию». */
    function componentWarningText(comp) {
        comp = comp || {};
        if (comp.license) {
            return t('imageModels.component_added_default',
                'Добавлено по умолчанию: {role} из {repo} (лицензия: {license}). Проверьте лицензию перед публикацией результатов.', {
                    role: roleLabel(comp.role), repo: comp.repo, license: comp.license
                });
        }
        return t('imageModels.component_added_default_no_license',
            'Добавлено по умолчанию: {role} из {repo}. Лицензия в карточке репозитория не указана — проверьте её сами.', {
                role: roleLabel(comp.role), repo: comp.repo
            });
    }

    /** componentConfirm — подтверждение в окне браузера (штатный путь WebUI). */
    function componentConfirm(text) {
        if (typeof window !== 'undefined' && typeof window.confirm === 'function') {
            return !!window.confirm(text);
        }
        if (typeof confirm === 'function') return !!confirm(text);
        return true; // окружение без confirm (тесты) — не блокируем действие
    }

    /** familyForComponents — семейство для каталога (вердикт → селект → other). */
    function familyForComponents(compat) {
        var fam = String((compat && compat.family) || '');
        if (fam && fam !== 'other') return fam;
        var sel = el('imgBundleFamily');
        var fromSelect = sel ? String(sel.value || '') : '';
        return fromSelSafe(fromSelect || state.bundleFamily || 'other');
    }

    /** fromSelSafe — пустое семейство не должно превращаться в '' (каталог по ''). */
    function fromSelSafe(value) {
        var v = String(value || '').trim();
        return v || 'other';
    }

    /** componentCatalogKeyOf — отпечаток запроса каталога (кэш + защита от цикла). */
    function componentCatalogKeyOf(family, roles) {
        return String(family || 'other') + '|' + (roles || []).slice().sort().join(',');
    }

    /**
     * loadComponentCatalog — GET /api/v1/image/components/catalog (кэш по family+roles).
     *
     * ПОЧЕМУ КЛЮЧ СТАВИТСЯ И ПРИ ОШИБКЕ: иначе каждый рендер вердикта повторял бы
     * упавший запрос (рендер дергается на каждую галочку), и страница молотила бы
     * сеть. Ошибка показывается один раз, повтор — только явным действием.
     */
    async function loadComponentCatalog(family, roles) {
        var key = componentCatalogKeyOf(family, roles);
        if (state.componentCatalogKey === key) return state.componentCatalog;
        if (state.componentCatalogBusy) return null;
        state.componentCatalogBusy = true;
        state.componentCatalogError = '';
        renderVerifyVerdict();
        var url = apiBase() + '/api/v1/image/components/catalog?family=' + encodeURIComponent(family);
        if (roles && roles.length) url += '&roles=' + encodeURIComponent(roles.join(','));
        try {
            var data = await request(url, { timeoutMs: TIMEOUT_CONTROL_MS });
            state.componentCatalog = data || { known: false, components: [] };
        } catch (e) {
            state.componentCatalog = null;
            state.componentCatalogError = (e && e.message) || String(e);
        }
        state.componentCatalogKey = key;
        state.componentCatalogBusy = false;
        renderVerifyVerdict();
        return state.componentCatalog;
    }

    /** componentBlockHtml — блок «чем починить набор» под строкой «не хватает». */
    function componentBlockHtml(compat) {
        var roles = missingComponentRoles(compat);
        var note = state.componentNotes && state.componentNotes[''] ? state.componentNotes[''] : '';
        if (!roles.length) {
            // Набор закрыт — но если компоненты уже добавлены к выбору, показать
            // их всё равно надо: оператор должен видеть, что скачается.
            return componentExtraSummaryHtml() + warningHtml(note);
        }
        var family = familyForComponents(compat);
        var key = componentCatalogKeyOf(family, roles);
        if (state.componentCatalogKey !== key && !state.componentCatalogBusy) {
            // Не блокируем рендер: сначала показываем «загружаю каталог», ответ
            // придёт и перерисует блок сам.
            loadComponentCatalog(family, roles);
        }
        var html = '';
        if (state.componentCatalogBusy || state.componentCatalogKey !== key) {
            html += '<div class="imh-verdict-reason" style="margin-top:8px;"><i class="fas fa-spinner fa-spin"></i> ' +
                escapeHtml(t('imageModels.component_catalog_loading', 'Ищу стандартные компоненты для семейства «{family}»…',
                    { family: family })) + '</div>';
        }
        if (state.componentCatalogError) {
            html += '<div class="imh-verdict-reason" style="margin-top:8px;color:var(--warning, #e0a030);">' +
                escapeHtml(t('imageModels.component_catalog_failed',
                    'Каталог компонентов недоступен: {error}. Укажите репозиторий и файл вручную.',
                    { error: state.componentCatalogError })) + '</div>';
        }
        html += componentButtonsHtml(roles, {
            catalog: state.componentCatalog,
            family: family,
            mode: 'selection'
        });
        html += componentManualHtml();
        html += componentExtraSummaryHtml();
        html += warningHtml(note);
        return html;
    }

    /** warningHtml — предупреждение «добавлено по умолчанию» (единая разметка). */
    function warningHtml(note) {
        if (!note) return '';
        return '<div class="imh-verdict-reason" style="margin-top:6px;color:var(--warning, #e0a030);">' +
            '<i class="fas fa-triangle-exclamation"></i> ' + escapeHtml(note) + '</div>';
    }

    /**
     * componentExtraSummaryHtml — что уже добавлено к набору и откуда.
     *
     * ЗАЧЕМ В ВЕРДИКТЕ, А НЕ ТОЛЬКО В СПИСКЕ ФАЙЛОВ: список файлов виден лишь у
     * раскрытой карточки модели, а вердикт — всегда. Требование «видеть, что
     * именно будет добавлено и откуда» не должно зависеть от того, раскрыл ли
     * оператор карточку.
     */
    function componentExtraSummaryHtml() {
        var comps = state.extraComponents || [];
        if (!comps.length) return '';
        var lines = comps.map(function (c) {
            return '<div style="display:flex;gap:8px;align-items:center;flex-wrap:wrap;">' +
                '<i class="fas fa-plus-circle" style="color:var(--success, #5cb85c);"></i>' +
                '<span>' + escapeHtml(roleLabel(c.role)) + '</span>' +
                '<span style="font-family:monospace;word-break:break-all;">' + escapeHtml(String(c.filename || '')) + '</span>' +
                '<span>' + escapeHtml(String(c.repo || '')) + '</span>' +
                (c.sizeBytes ? '<span style="color:var(--text-muted);">' + escapeHtml(formatBytes(c.sizeBytes)) + '</span>' : '') +
                '<button class="btn btn-secondary btn-sm" data-icom-remove="' + escapeHtml(String(c.role || '')) + '"' +
                    ' title="' + escapeHtml(t('imageModels.component_remove_hint', 'Убрать этот компонент из набора')) + '">' +
                    '<i class="fas fa-xmark"></i></button>' +
                '</div>';
        });
        return '<div class="imh-component-added" style="margin-top:8px;font-size:var(--font-size-xs);">' +
            lines.join('') + '</div>';
    }

    /**
     * componentManualHtml — ручная форма «репозиторий + файлы с ролями».
     *
     * Роли в списке — серверные (suggestedRole из GET /hf/files), как и на самой
     * вкладке HF: эвристика «какой файл есть VAE» живёт в одном месте (воркер),
     * иначе UI и воркер разъехались бы в определении роли.
     */
    function componentManualHtml() {
        var m = state.componentManual;
        if (!m) return '';
        var files = Array.isArray(m.files) ? m.files : [];
        var html = '<div class="gguf-local-card" style="margin-top:8px;border-left:3px solid var(--info, #4a90d9);">' +
            '<div class="gguf-local-info"><div class="gguf-local-meta" style="white-space:normal;">';
        html += '<div style="display:flex;gap:6px;align-items:flex-end;flex-wrap:wrap;">' +
            '<label style="display:flex;flex-direction:column;gap:2px;font-size:var(--font-size-xs);">' +
                escapeHtml(t('imageModels.component_manual_repo', 'Репозиторий HF')) +
                '<input class="form-control" data-icom-manual-repo="1" value="' + escapeHtml(m.repo || '') + '"' +
                ' placeholder="Comfy-Org/Qwen-Image_ComfyUI" style="min-width:280px;font-size:var(--font-size-xs);">' +
            '</label>' +
            '<button class="btn btn-secondary btn-sm" data-icom-manual-load="1">' +
                escapeHtml(t('imageModels.component_manual_show_files', 'Показать файлы')) + '</button>' +
            '</div>';
        if (m.error) {
            html += '<div style="color:var(--danger);margin-top:6px;font-size:var(--font-size-xs);">' +
                escapeHtml(m.error) + '</div>';
        }
        if (m.loading) {
            html += '<div style="margin-top:6px;font-size:var(--font-size-xs);color:var(--text-muted);">' +
                '<i class="fas fa-spinner fa-spin"></i> ' + escapeHtml(t('common.loading', 'Загрузка…')) + '</div>';
        }
        if (files.length) {
            html += '<div style="margin-top:8px;max-height:220px;overflow:auto;">' + files.map(function (f) {
                var path = filePath(f);
                var sel = (m.selection && m.selection[path]) || null;
                return '<label style="display:flex;align-items:center;gap:8px;font-size:var(--font-size-xs);padding:2px 0;cursor:pointer;">' +
                    '<input type="checkbox" data-icom-manual-pick="' + escapeHtml(path) + '"' + (sel && sel.checked ? ' checked' : '') + '>' +
                    '<span style="font-family:monospace;word-break:break-all;">' + escapeHtml(path) + '</span>' +
                    '<select class="form-control" data-icom-manual-role="' + escapeHtml(path) + '"' +
                    ' style="max-width:180px;font-size:var(--font-size-xs);">' +
                        roleOptions(sel && sel.role ? sel.role : (suggestedRole(f) || 'vae')) +
                    '</select>' +
                    '<span style="color:var(--text-muted);">' + escapeHtml(formatBytes(fileSize(f))) + '</span>' +
                    '</label>';
            }).join('') + '</div>';
            html += '<div style="margin-top:8px;display:flex;gap:6px;">' +
                '<button class="btn btn-primary btn-sm" data-icom-manual-add="1">' +
                    escapeHtml(t('imageModels.component_manual_add', 'Добавить к набору')) + '</button>' +
                '<button class="btn btn-secondary btn-sm" data-icom-manual-cancel="1">' +
                    escapeHtml(t('common.cancel', 'Отмена')) + '</button></div>';
        } else if (!m.loading) {
            html += '<div style="margin-top:8px;"><button class="btn btn-secondary btn-sm" data-icom-manual-cancel="1">' +
                escapeHtml(t('common.cancel', 'Отмена')) + '</button></div>';
        }
        return html + '</div></div></div>';
    }

    /** extraComponentRoles — роли, уже добавленные к выбору из других репозиториев. */
    function extraComponentRoles() {
        return (state.extraComponents || []).map(function (c) { return String((c && c.role) || ''); });
    }

    /** roleIsTakenInSelection — роль уже занята в выборе (галочкой или компонентом). */
    function roleIsTakenInSelection(role) {
        if (extraComponentRoles().indexOf(String(role)) !== -1) return true;
        var files = Array.isArray(state.files) ? state.files : [];
        for (var i = 0; i < files.length; i++) {
            var p = filePath(files[i]);
            var sel = state.selection[p];
            if (sel && sel.checked && String(sel.role) === String(role)) return true;
        }
        return false;
    }

    /** pushExtraComponent — добавить компонент в выбор (без сети). */
    function pushExtraComponent(comp) {
        comp = normalizeComponent(comp, comp && comp.family);
        if (!comp.role || !comp.repo || !comp.filename) return false;
        if (roleIsTakenInSelection(comp.role)) {
            toast(t('imageModels.component_role_taken', 'Роль «{role}» уже занята в наборе', { role: roleLabel(comp.role) }), 'warn');
            return false;
        }
        state.extraComponents.push({
            role: comp.role,
            repo: comp.repo,
            filename: comp.filename,
            revision: 'main',
            sizeBytes: comp.sizeBytes,
            sizeMb: comp.sizeMb,
            license: comp.license,
            note: comp.note,
            default: comp.default === true
        });
        setComponentNote('', comp.default === true ? componentWarningText(comp) : '');
        return true;
    }

    /** setComponentNote — предупреждение в карточке (ключ '' = выбор на HF-табе). */
    function setComponentNote(model, text) {
        state.componentNotes = state.componentNotes || {};
        if (text) state.componentNotes[String(model || '')] = String(text);
        else delete state.componentNotes[String(model || '')];
    }

    /** noteForModel — предупреждение по ключу ('' = выбор). */
    function noteForModel(model) {
        state.componentNotes = state.componentNotes || {};
        return state.componentNotes[String(model || '')] || '';
    }

    /** removeExtraComponent — убрать добавленный компонент из выбора. */
    function removeExtraComponent(role) {
        var before = (state.extraComponents || []).length;
        state.extraComponents = (state.extraComponents || []).filter(function (c) {
            return String((c && c.role) || '') !== String(role);
        });
        if (state.extraComponents.length === before) return false;
        setComponentNote('', '');
        renderFiles();
        renderVerifyVerdict();
        scheduleVerify();
        return true;
    }

    /**
     * addDefaultComponents — добавить стандартные компоненты к ВЫБОРУ (вкладка HF).
     *
     * Подтверждение ОДНО на все выбранные компоненты: оператор нажал «Скачать
     * недостающее», и видеть надо весь список с размерами и суммарным объёмом, а
     * не три окна подряд.
     */
    function addDefaultComponents(roles) {
        var missing = Array.isArray(roles) ? roles : [];
        var family = familyForComponents(state.verify);
        var comps = [];
        missing.forEach(function (role) {
            var comp = defaultComponentFor(state.componentCatalog, family, role);
            if (comp) comps.push(comp);
        });
        if (!comps.length) {
            toast(t('imageModels.component_nothing_default',
                'Для этого семейства нет стандартных компонентов — укажите репозиторий вручную'), 'warn');
            return false;
        }
        var texts = comps.map(function (c) { return componentConfirmText(c, { mode: 'selection' }); });
        var totalMb = comps.reduce(function (acc, c) { return acc + Number(c.sizeMb || 0); }, 0);
        var extra = t('imageModels.component_confirm_total',
            'Итого будет добавлено компонентов: {n} на {size}.', { n: comps.length, size: componentSizeText(totalMb) });
        if (!componentConfirm(texts.join('\n\n') + '\n\n' + extra)) return false;
        var added = 0;
        comps.forEach(function (c) { if (pushExtraComponent(c)) added++; });
        renderFiles();
        // Отдельный рендер вердикта: список файлов виден только у раскрытой
        // карточки, а сводка «что добавлено и откуда» должна появиться сразу.
        renderVerifyVerdict();
        scheduleVerify();
        if (added) {
            toast(t('imageModels.component_added_count', 'Добавлено компонентов: {n}', { n: added }), 'success');
        }
        return added > 0;
    }

    /** openComponentManual — открыть ручную форму (репозиторий + файлы). */
    function openComponentManual() {
        state.componentManual = { repo: '', loading: false, error: '', files: [], selection: {} };
        renderVerifyVerdict();
        return true;
    }

    /** closeComponentManual — закрыть ручную форму. */
    function closeComponentManual() {
        state.componentManual = null;
        renderVerifyVerdict();
        return true;
    }

    /** fetchRepoFiles — список файлов репозитория (для ручной формы). */
    async function fetchRepoFiles(repo) {
        var url = backendUrl('hf/files?modelId=' + encodeURIComponent(repo));
        var data = await request(url, { timeoutMs: TIMEOUT_SEARCH_MS });
        return (data && Array.isArray(data.files)) ? data.files : [];
    }

    /**
     * listRepoFiles — файлы ПРОИЗВОЛЬНОГО репозитория для страницы «Модели на диске».
     *
     * ЗАЧЕМ ЗДЕСЬ, А НЕ В image-page.js: обработка /api/hf/* целиком принадлежит
     * этому модулю (X-HF-Token, таймауты поиска, форма ответа с suggestedRole).
     * В image-page.js путь /api/hf/ запрещён тестом — именно чтобы не появилась
     * ВТОРАЯ копия HF-логики. backendId передаётся явно: у страницы моделей свой
     * селект бэкенда, и он не обязан совпадать с ctx таба HF.
     */
    async function listRepoFiles(backendIdArg, repo) {
        var id = String(backendIdArg || backendId() || '');
        var repoStr = String(repo || '').trim();
        if (!id || !repoStr) return [];
        var url = apiBase() + '/api/v1/image/backends/' + encodeURIComponent(id) +
            '/hf/files?modelId=' + encodeURIComponent(repoStr);
        var data = await request(url, { timeoutMs: TIMEOUT_SEARCH_MS });
        return (data && Array.isArray(data.files)) ? data.files : [];
    }

    /** loadComponentManualFiles — «Показать файлы» в ручной форме. */
    async function loadComponentManualFiles() {
        var m = state.componentManual;
        if (!m) return false;
        var repo = String(m.repo || '').trim();
        if (!repo) {
            m.error = t('imageModels.component_manual_repo_required', 'Укажите репозиторий HF (например Comfy-Org/Qwen-Image_ComfyUI)');
            renderVerifyVerdict();
            return false;
        }
        m.loading = true;
        m.error = '';
        m.files = [];
        m.selection = {};
        renderVerifyVerdict();
        try {
            m.files = await fetchRepoFiles(repo);
            if (!m.files.length) {
                m.error = t('imageModels.hf_no_files', 'В этом репозитории нет файлов для скачивания');
            }
        } catch (e) {
            m.error = (e && e.message) || String(e);
        }
        m.loading = false;
        renderVerifyVerdict();
        return true;
    }

    /** addManualComponents — добавить отмеченные файлы ручной формы к выбору. */
    function addManualComponents() {
        var m = state.componentManual;
        if (!m) return false;
        var repo = String(m.repo || '').trim();
        var picked = Object.keys(m.selection || {}).filter(function (p) {
            return m.selection[p] && m.selection[p].checked;
        });
        if (!picked.length) {
            toast(t('imageModels.hf_none_selected', 'Отметьте хотя бы один файл модели'), 'warn');
            return false;
        }
        var sizes = {};
        (m.files || []).forEach(function (f) { sizes[filePath(f)] = fileSize(f); });
        var lines = picked.map(function (p) {
            return t('imageModels.component_confirm_line',
                'Будет добавлено: {role} {file} ({size}) из {repo}', {
                    role: roleLabel(m.selection[p].role),
                    file: p,
                    size: formatBytes(sizes[p] || 0),
                    repo: repo
                });
        });
        lines.push(t('imageModels.component_confirm_selection',
            'Файл добавится к набору и скачается вместе с остальными файлами.'));
        if (!componentConfirm(lines.join('\n'))) return false;
        var added = 0;
        picked.forEach(function (p) {
            if (pushExtraComponent({
                role: m.selection[p].role,
                repo: repo,
                files: [p],
                sizeMb: Math.ceil(Number(sizes[p] || 0) / (1024 * 1024))
            })) added++;
        });
        state.componentManual = null;
        renderFiles();
        renderVerifyVerdict();
        scheduleVerify();
        if (added) toast(t('imageModels.component_added_count', 'Добавлено компонентов: {n}', { n: added }), 'success');
        return added > 0;
    }

    // ---------- Режим «bundle»: установка в уже скачанный набор -----------

    /** sleep — пауза между опросами прогресса. */
    function sleep(ms) {
        return new Promise(function (resolve) { setTimeout(resolve, ms); });
    }

    /** componentErrorText — понятный текст отказа POST components. */
    function componentErrorText(e) {
        var body = (e && e.body) || {};
        var code = String(body.code || '');
        if (code === 'role_already_present') {
            return t('imageModels.component_err_role_present',
                'Роль уже занята файлом «{file}»: сначала выгрузите/удалите его или выберите другую роль.',
                { file: String(body.currentFile || '') });
        }
        if (code === 'component_file_not_found') {
            var avail = Array.isArray(body.available) ? body.available.slice(0, 5).join(', ') : '';
            return t('imageModels.component_err_file_not_found',
                'Файла нет в репозитории. Что есть: {available}', { available: avail }) ;
        }
        if (code === 'component_too_large') {
            return t('imageModels.component_err_too_large',
                'Файл слишком большой для компонентной установки — возьмите меньший квант (см. каталог).');
        }
        if (code === 'bundle_not_found') {
            return t('imageModels.component_err_bundle_not_found',
                'У воркера нет каталога этой модели: сначала скачайте набор, потом добавляйте компоненты.');
        }
        if (code === 'invalid_path') {
            return t('imageModels.component_err_invalid_path',
                'Отказ: путь вне каталога моделей. Добавлять компоненты можно только в каталог самой модели.');
        }
        if (code === 'component_file_conflict') {
            return t('imageModels.component_err_conflict',
                'Файл «{file}» уже принадлежит другой роли набора — скачивание заменило бы её.',
                { file: String(body.currentFile || '') });
        }
        return (e && e.message) || String(e);
    }

    /**
     * installBundleComponent — добавить компонент к УЖЕ скачанному набору.
     *
     * opts: {backendId, model, family, comp, onProgress, onDone}.
     * Поток: подтверждение → POST /api/v1/image/models/{name}/components?backendId=
     * → опрос прогресса существующей ручкой /api/hf/progress (через прокси
     * бэкенда) → onDone() (перечитать список моделей: вердикт уже пересчитан
     * воркером, потому что «completed» ставится ПОСЛЕ записи profile.json).
     */
    async function installBundleComponent(opts) {
        opts = opts || {};
        var comp = normalizeComponent(opts.comp, opts.family);
        var model = String(opts.model || '');
        var backend = String(opts.backendId || '');
        if (!comp.role || !comp.repo || !comp.filename) {
            toast(t('imageModels.component_nothing_default', 'Компонент не выбран'), 'error');
            return false;
        }
        if (!model || !backend) {
            toast(t('image.no_backend_selected', 'Сначала выберите image-бэкенд'), 'error');
            return false;
        }
        if (!componentConfirm(componentConfirmText(comp, { mode: 'bundle' }))) return false;
        var url = apiBase() + '/api/v1/image/models/' + encodeURIComponent(model) +
            '/components?backendId=' + encodeURIComponent(backend);
        var body = JSON.stringify({
            role: comp.role,
            repo: comp.repo,
            files: comp.files,
            revision: 'main',
            default: comp.default === true
        });
        var res;
        try {
            res = await request(url, { method: 'POST', body: body, timeoutMs: TIMEOUT_CONTROL_MS });
        } catch (e) {
            toast(componentErrorText(e), 'error');
            return false;
        }
        state.componentBusy = {
            model: model, role: comp.role, status: 'started', pct: 0, error: '',
            trackingId: (res && res.trackingId) || ''
        };
        setComponentNote(model, '');
        toast(t('imageModels.component_started', 'Скачиваю компонент: {role}', { role: roleLabel(comp.role) }), 'info');
        if (typeof opts.onProgress === 'function') { try { opts.onProgress(); } catch (e1) { /* рендер не критичен */ } }
        pollBundleComponent(model, backend, state.componentBusy.trackingId, comp, opts);
        return true;
    }

    /** pollBundleComponent — опрос прогресса компонентной установки до терминала. */
    async function pollBundleComponent(model, backend, trackingId, comp, opts) {
        opts = opts || {};
        if (!trackingId) {
            state.componentBusy = null;
            return;
        }
        var url = apiBase() + '/api/v1/image/backends/' + encodeURIComponent(backend) +
            '/proxy/api/hf/progress?bundleId=' + encodeURIComponent(trackingId);
        // Компонент — это сотни мегабайт..гигабайты: 4 часа с большим запасом.
        var deadline = Date.now() + 4 * 60 * 60 * 1000;
        while (Date.now() < deadline) {
            await sleep(DEFAULT_POLL_MS);
            var p = null;
            try {
                p = await request(url, { timeoutMs: TIMEOUT_CONTROL_MS });
            } catch (e) {
                continue; // 404 = записи ещё нет; сеть моргнула — не повод сдаваться
            }
            if (!p) continue;
            state.componentBusy = {
                model: model, role: comp.role, trackingId: trackingId,
                status: String(p.status || ''), pct: progressPercent(p), error: String(p.errorMessage || '')
            };
            if (typeof opts.onProgress === 'function') { try { opts.onProgress(); } catch (e2) { /* рендер */ } }
            var status = String(p.status || '');
            if (status && status !== 'downloading') {
                state.componentBusy = null;
                if (status === 'completed') {
                    setComponentNote(model, comp.default === true ? componentWarningText(comp) : '');
                    toast(t('imageModels.component_done',
                        'Компонент добавлен ({role}). Вердикт пересчитан, состав набора обновлён.',
                        { role: roleLabel(comp.role) }), 'success');
                } else {
                    setComponentNote(model, '');
                    toast(t('imageModels.component_failed', 'Не удалось добавить компонент: {error}',
                        { error: p.errorMessage || status }), 'error');
                }
                if (typeof opts.onDone === 'function') { try { opts.onDone(); } catch (e3) { /* рендер списка */ } }
                return;
            }
        }
        state.componentBusy = null;
        toast(t('imageModels.component_failed', 'Не удалось добавить компонент: {error}',
            { error: 'timeout' }), 'error');
    }

    /** busyHtml — строка прогресса установки компонента для карточки модели. */
    function componentBusyHtml(model) {
        var b = state.componentBusy;
        if (!b || String(b.model || '') !== String(model || '')) return '';
        var line = t('imageModels.component_progress', 'Скачиваю {role}: {status} {pct}%', {
            role: roleLabel(b.role), status: b.status || '', pct: Number(b.pct || 0)
        });
        return '<div class="imh-verdict-reason" style="margin-top:4px;">' +
            '<i class="fas fa-spinner fa-spin"></i> ' + escapeHtml(line) + '</div>';
    }
    /**
     * renderVerifyVerdict — вердикт «сможет ли imageworker загрузить набор» по
     * выбранным файлам (R696), ДО скачивания.
     *
     * ПОЧЕМУ ВЫБОР, А НЕ РЕПОЗИТОРИЙ: пригодность набора = diffusion + VAE +
     * text encoder. Живой дефект: репозиторий AtomicChat/Qwen-Image-2.1-Turbo-
     * Uncensored-GGUF целиком состоит из квантов текстового энкодера Qwen3-VL —
     * «паспорт» считал набор собранным, оператор скачал 5 ГБ и получил
     * «get sd version from file failed».
     */
    function renderVerifyVerdict() {
        var node = el('imHfVerifyVerdict');
        if (!node) return;
        var compat = state.verify;
        if (state.verifyError) {
            node.style.display = '';
            node.style.borderLeftColor = 'var(--warning, #e0a030)';
            node.innerHTML = '<i class="fas fa-triangle-exclamation" style="color:var(--warning, #e0a030);"></i> ' +
                escapeHtml(t('imageModels.hf_verify_failed', 'Не удалось проверить набор: {error}', { error: state.verifyError }));
            return;
        }
        if (state.verifyBusy) {
            node.style.display = '';
            node.style.borderLeftColor = 'var(--border)';
            node.innerHTML = '<i class="fas fa-spinner fa-spin"></i> ' +
                escapeHtml(t('imageModels.hf_verify_running', 'Проверяю заголовки выбранных файлов…'));
            return;
        }
        if (!compat) { node.style.display = 'none'; node.innerHTML = ''; return; }
        var ok = compat.runnable !== false;
        var color = ok ? 'var(--success, #5cb85c)'
            : (compat.verdict === 'unknown' ? 'var(--text-muted)' : 'var(--danger, #d9534f)');
        var icon = ok ? 'fa-circle-check' : (compat.verdict === 'unknown' ? 'fa-circle-question' : 'fa-circle-xmark');
        var head = ok
            ? t('imageModels.compat_verdict_yes', 'imageworker сможет загрузить: да')
            : t('imageModels.compat_verdict_no', 'imageworker НЕ сможет загрузить');
        var html = '<div style="color:' + color + ';font-weight:600;"><i class="fas ' + icon + '"></i> ' +
            escapeHtml(head) + (compat.reason ? ' — ' + escapeHtml(String(compat.reason)) : '') + '</div>';
        var missing = (compat.missing && compat.missing.length)
            ? t('imageModels.compat_verdict_missing', 'не хватает: {roles}', { roles: compat.missing.join(', ') })
            : '';
        if (missing) html += '<div class="imh-verdict-missing">' + escapeHtml(missing) + '</div>';
        // R710: чем починить набор — кнопки «Добавить X (по умолчанию)»,
        // «Выбрать из другого репозитория…» и «Скачать недостающее».
        html += componentBlockHtml(compat);
        if (compat.engineMode && compat.engineMode !== 'unknown') {
            html += '<div class="imh-verdict-reason">' +
                escapeHtml(t('imageModels.plan_mode', 'Режим запуска')) + ': <code>' +
                escapeHtml(compat.engineMode === 'model' ? '--model (all-in-one)' : '--diffusion-model + VAE/text encoder') +
                '</code></div>';
        }
        if (compat.files && compat.files.length) {
            var lines = compat.files.map(function (f) {
                if (!f || !f.detectedRole) return '';
                var mark = f.match === false ? '⚠️' : '✅';
                return '<div>' + mark + ' ' + escapeHtml(String(f.filename || '')) + ' — ' +
                    escapeHtml(t('imageModels.role_' + f.detectedRole, f.detectedRole)) + '</div>';
            }).filter(Boolean);
            if (lines.length) html += '<div class="imh-verdict-reason">' + lines.join('') + '</div>';
        }
        node.style.display = '';
        node.style.borderLeftColor = color;
        node.innerHTML = html;
    }

    /** selectionKey — отпечаток выбранного набора (кэш вердикта). */
    function selectionKey() {
        var paths = Object.keys(state.selection || {}).filter(function (p) {
            return state.selection[p] && state.selection[p].checked;
        }).sort();
        // R710: добавленные компоненты — часть набора: без них вердикт после
        // «Добавить VAE» не пересчитался бы (отпечаток не изменился бы).
        var comps = (state.extraComponents || []).map(function (c) {
            return String((c && c.role) || '') + '=' + String((c && c.repo) || '') + '/' + String((c && c.filename) || '');
        }).sort();
        return [state.selectedRepo || '', state.bundleFamily || '',
            paths.map(function (p) { return p + ':' + ((state.selection[p] && state.selection[p].role) || ''); }).join(','),
            comps.join(',')
        ].join('|');
    }

    /** selectedFileSpecs — тело для POST /api/hf/verify по отмеченным файлам. */
    function selectedFileSpecs() {
        var out = [];
        (state.files || []).forEach(function (f) {
            var p = filePath(f);
            var sel = state.selection[p];
            if (!sel || !sel.checked) return;
            out.push({
                role: sel.role || suggestedRole(f) || 'diffusion',
                repo: state.selectedRepo,
                filename: p,
                revision: String(f.revision || 'main')
            });
        });
        // R710: компоненты из других репозиториев проверяются тем же вердиктом, что
        // и файлы самой модели — иначе после «Добавить VAE» оператор увидел бы
        // прежнее «не хватает VAE» (вердикт считается по набору, а набор вырос).
        (state.extraComponents || []).forEach(function (c) {
            if (!c || !c.role || !c.repo || !c.filename) return;
            out.push({
                role: String(c.role),
                repo: String(c.repo),
                filename: String(c.filename),
                revision: String(c.revision || 'main')
            });
        });
        return out;
    }

    /**
     * scheduleVerify — проверка набора с задержкой: галочки оператор ставит
     * пачкой, а каждый вызов ходит в HF за заголовками файлов (Range-запросы).
     * 400 мс склеивают серию кликов в одну проверку.
     */
    function scheduleVerify() {
        if (!state.selectedRepo) return;
        if (state.verifyTimer) clearTimeout(state.verifyTimer);
        state.verifyTimer = setTimeout(function () {
            state.verifyTimer = null;
            verifySelection(false);
        }, 400);
    }

    /**
     * verifySelection — вердикт по выбранному набору (POST /api/hf/verify).
     *
     * force=false — берём кэш, если отпечаток выбора не менялся.
     * force=true  — кнопка «Проверить совместимость» (перепроверить).
     */
    async function verifySelection(force) {
        if (!backendId() || !state.selectedRepo) { state.verify = null; renderVerifyVerdict(); return null; }
        var files = selectedFileSpecs();
        var key = selectionKey();
        if (!files.length) {
            state.verify = null;
            state.verifyKey = '';
            state.verifyError = '';
            renderVerifyVerdict();
            return null;
        }
        if (!force && state.verify && state.verifyKey === key) { renderVerifyVerdict(); return state.verify; }
        var seq = ++state.verifySeq;
        state.verifyBusy = true;
        state.verifyError = '';
        renderVerifyVerdict();
        try {
            var res = await request(backendUrl('hf/verify'), {
                method: 'POST',
                body: JSON.stringify({
                    name: state.selectedRepo,
                    family: state.bundleFamily || 'other',
                    files: files
                }),
                timeoutMs: TIMEOUT_SEARCH_MS
            });
            if (seq !== state.verifySeq) return state.verify;
            state.verify = (res && res.compat) || null;
            state.verifyKey = key;
        } catch (e) {
            if (seq !== state.verifySeq) return state.verify;
            state.verify = null;
            state.verifyError = (e && e.message) || String(e);
        }
        state.verifyBusy = false;
        renderVerifyVerdict();
        return state.verify;
    }

    function renderBundleProgress() {
        var wrap = el('imgBundleProgress');
        if (!wrap) return;
        var p = state.bundle;
        if (!p) { wrap.style.display = 'none'; return; }
        wrap.style.display = '';
        var pct = progressPercent(p);
        var label = el('imgBundleProgressLabel');
        if (label) {
            label.textContent = String(p.bundleId || '') + ': ' +
                t(bundleStatusKey(p.status), p.status || '') + ' - ' + pct + '% (' +
                formatBytes(p.downloaded) + (p.totalBytes ? ' / ' + formatBytes(p.totalBytes) : '') + ')' +
                (p.speedBps ? ' · ' + formatBytes(p.speedBps) + '/s' : '');
        }
        var fill = el('imgBundleProgressFill');
        if (fill) fill.style.width = pct + '%';
        var status = el('imgBundleStatus');
        if (status) {
            var lines = (p.files || []).map(function (f) {
                var fpct = progressPercent(f);
                return '<div>' + escapeHtml(String(f.role || '') + ': ' + (f.filename || f.sourcePath || '')) + ' - ' +
                    escapeHtml(t(bundleStatusKey(f.status), f.status || '')) +
                    (f.status === 'downloading' ? ' ' + fpct + '%' : '') +
                    (f.downloaded ? ' · ' + formatBytes(f.downloaded) : '') +
                    (f.error ? ' · ' + escapeHtml(f.error) : '') + '</div>';
            });
            status.innerHTML = lines.join('');
        }
    }

    // =====================================================================
    // Рендер: «Загрузки»
    // =====================================================================

    function renderProgressBar(pct) {
        return '<div class="gguf-progress-bar" style="height:6px;background:var(--bg-tertiary,#333);border-radius:3px;overflow:hidden;margin-top:6px;">' +
            '<div class="gguf-progress-fill" style="height:100%;width:' + Math.max(0, Math.min(100, Number(pct) || 0)) + '%;background:var(--success);"></div>' +
            '</div>';
    }

    function renderBundleItem(b, isActive) {
        b = b || {};
        var pct = progressPercent(b);
        var files = Array.isArray(b.files) ? b.files : [];
        var filesLine = files.map(function (f) {
            return escapeHtml(String(f.role || '') + ': ' + (f.filename || '') + ' - ' + t(bundleStatusKey(f.status), f.status || ''));
        }).join('<br>');
        return '<div class="gguf-download-item" data-imh-bundle="' + escapeHtml(String(b.bundleId || '')) + '">' +
            '<div class="dl-header" style="display:flex;justify-content:space-between;align-items:center;gap:8px;">' +
                '<span class="dl-name"><i class="fas fa-layer-group"></i> ' + escapeHtml(String(b.bundleId || '')) + '</span>' +
                '<span class="badge">' + escapeHtml(t(bundleStatusKey(b.status), b.status || '')) + '</span>' +
            '</div>' +
            '<div style="font-size:var(--font-size-xs);color:var(--text-muted);margin-top:4px;">' +
                pct + '% · ' + escapeHtml(formatBytes(b.downloaded)) + (b.totalBytes ? ' / ' + escapeHtml(formatBytes(b.totalBytes)) : '') +
                (b.speedBps ? ' · ' + escapeHtml(formatBytes(b.speedBps)) + '/s' : '') +
                (b.currentFile ? ' · ' + escapeHtml(String(b.currentFile)) : '') +
                (b.errorMessage ? ' · <span style="color:var(--danger);">' + escapeHtml(String(b.errorMessage)) + '</span>' : '') +
            '</div>' +
            renderProgressBar(pct) +
            (filesLine ? '<div style="font-size:var(--font-size-xs);color:var(--text-muted);margin-top:6px;">' + filesLine + '</div>' : '') +
            '<div class="dl-actions" style="display:flex;gap:6px;margin-top:8px;">' +
                (isActive
                    ? '<button class="btn btn-sm btn-secondary" data-imh-cancel="' + escapeHtml(String(b.bundleId || '')) + '">' +
                        '<i class="fas fa-ban"></i> ' + escapeHtml(t('gguf.cancel_download', 'Cancel')) + '</button>'
                    : (b.registered
                        ? '<span class="badge badge-success" title="' + escapeHtml(t('imageModels.bundle_registered', 'Bundle registered')) + '">' +
                            '<i class="fas fa-circle-check"></i> ' + escapeHtml(t('imageModels.bundle_registered', 'Bundle registered')) + '</span>'
                        : '')) +
            '</div>' +
        '</div>';
    }

    function renderFileItem(p, isActive) {
        p = p || {};
        var pct = progressPercent(p);
        return '<div class="gguf-download-item">' +
            '<div class="dl-header" style="display:flex;justify-content:space-between;align-items:center;gap:8px;">' +
                '<span class="dl-name"><i class="fas fa-file"></i> ' + escapeHtml(String(p.filename || '')) + '</span>' +
                '<span class="badge">' + escapeHtml(t(bundleStatusKey(p.status), p.status || '')) + '</span>' +
            '</div>' +
            '<div style="font-size:var(--font-size-xs);color:var(--text-muted);margin-top:4px;">' +
                escapeHtml(String(p.modelId || '')) + ' · ' + pct + '% · ' + escapeHtml(formatBytes(p.downloaded)) +
                (p.totalBytes ? ' / ' + escapeHtml(formatBytes(p.totalBytes)) : '') +
                (p.speedBps ? ' · ' + escapeHtml(formatBytes(p.speedBps)) + '/s' : '') +
                (p.errorMessage ? ' · <span style="color:var(--danger);">' + escapeHtml(String(p.errorMessage)) + '</span>' : '') +
            '</div>' +
            renderProgressBar(pct) +
            (isActive ? '<div class="dl-actions" style="margin-top:8px;">' +
                '<button class="btn btn-sm btn-secondary" data-imh-cancel-file="1" data-imh-model="' + escapeHtml(String(p.modelId || '')) + '" data-imh-filename="' + escapeHtml(String(p.filename || '')) + '">' +
                    '<i class="fas fa-ban"></i> ' + escapeHtml(t('gguf.cancel_download', 'Cancel')) + '</button></div>' : '') +
        '</div>';
    }

    function section(titleKey, titleFallback, html, count) {
        if (!html) return '';
        return '<div class="card" style="margin-bottom:12px;">' +
            '<div class="card-header"><h3>' + escapeHtml(t(titleKey, titleFallback)) +
                (count !== undefined ? ' <span class="badge">' + count + '</span>' : '') + '</h3></div>' +
            '<div class="card-body">' + html + '</div></div>';
    }

    function renderDownloads() {
        var host = el('imDownloadsHost');
        if (!host) return;
        if (state.downloadsError) {
            host.innerHTML = '<div class="gguf-empty-state" style="color:var(--danger);">' +
                escapeHtml(state.downloadsError) + '</div>';
            return;
        }
        var snap = downloadsSummary(state.downloads);
        if (!snap.hasAny) {
            host.innerHTML = '<div class="gguf-empty-state">' +
                '<i class="fas fa-cloud-arrow-down" style="opacity:0.4;"></i>' +
                '<div style="margin-top:6px;">' + escapeHtml(t('imageModels.no_downloads', 'Загрузок нет')) + '</div>' +
                '<div style="margin-top:4px;color:var(--text-muted);font-size:var(--font-size-xs);">' +
                escapeHtml(t('imageModels.no_downloads_hint', 'Запустите загрузку на табе HuggingFace — прогресс появится здесь.')) +
                '</div></div>';
            return;
        }
        var html = '';
        html += section('gguf.active_downloads', 'Active downloads',
            snap.bundles.map(function (b) { return renderBundleItem(b, true); }).join(''), snap.bundles.length || undefined);
        html += section('imageModels.dl_files_active', 'Active file downloads',
            snap.active.map(function (p) { return renderFileItem(p, true); }).join(''), snap.active.length || undefined);
        html += section('gguf.download_history', 'Download history',
            snap.bundleHistory.map(function (b) { return renderBundleItem(b, false); }).join('') +
            snap.history.map(function (p) { return renderFileItem(p, false); }).join(''),
            (snap.bundleHistory.length + snap.history.length) || undefined);
        if (snap.orphans.length) {
            var orphans = snap.orphans.map(function (o) {
                o = o || {};
                return '<div class="gguf-download-item">' +
                    '<div class="dl-header" style="display:flex;justify-content:space-between;align-items:center;gap:8px;">' +
                        '<span class="dl-name"><i class="fas fa-trash-can"></i> ' + escapeHtml(String(o.filename || o.path || '')) + '</span>' +
                        '<span style="font-size:var(--font-size-xs);color:var(--text-muted);">' + escapeHtml(formatBytes(o.size)) + '</span>' +
                    '</div>' +
                    '<div style="font-size:var(--font-size-xs);color:var(--text-muted);margin-top:4px;word-break:break-all;">' + escapeHtml(String(o.path || '')) + '</div>' +
                    '<div class="dl-actions" style="margin-top:8px;">' +
                        '<button class="btn btn-sm btn-danger" data-imh-orphan="' + escapeHtml(String(o.filename || '')) + '">' +
                            '<i class="fas fa-trash"></i> ' + escapeHtml(t('gguf.delete', 'Delete')) + '</button>' +
                    '</div>' +
                '</div>';
            }).join('');
            html += section('imageModels.orphans_title', 'Остаточные файлы',
                '<small style="display:block;margin-bottom:8px;color:var(--text-muted);font-size:var(--font-size-xs);">' +
                    escapeHtml(t('gguf.orphans_hint', 'Partial .download files from interrupted downloads.')) + '</small>' +
                orphans, snap.orphans.length);
        }
        host.innerHTML = html;
    }

    // =====================================================================
    // Действия
    // =====================================================================

    function requireBackend() {
        if (!backendId()) {
            toast(t('image.no_backend_selected', 'Select an image backend first'), 'error');
            return false;
        }
        return true;
    }

    async function search(query) {
        if (!requireBackend()) return false;
        var q = String(query === undefined ? (el('imHfQuery') ? el('imHfQuery').value : state.query) : query).trim();
        state.query = q;
        // Новый поиск = новый список карточек: раскрытая модель из прошлой выдачи
        // в нём может отсутствовать, поэтому раскрытие сбрасываем, а карточку
        // файлов паркуем на место (иначе она осталась бы висеть «в воздухе»).
        if (state.expandedRepo) {
            rememberRepo(state.expandedRepo);
            state.expandedRepo = '';
            parkFilesCard();
        }
        if (q.length < 2 && q.indexOf('/') === -1) {
            state.results = [];
            state.searchError = '';
            renderSearchResults();
            return false;
        }
        var taskSel = el('imHfTask');
        state.task = taskSel ? String(taskSel.value || '') : state.task;
        state.searching = true;
        state.searchError = '';
        renderSearchResults();
        try {
            var url = backendUrl('hf/search?query=' + encodeURIComponent(q) + '&limit=' + SEARCH_LIMIT);
            if (state.task) url += '&task=' + encodeURIComponent(state.task);
            var data = await request(url, { timeoutMs: TIMEOUT_SEARCH_MS });
            state.results = (data && Array.isArray(data.results)) ? data.results : [];
            if (!state.results.length) toast(t('gguf.no_results', 'No results'), 'warn');
        } catch (e) {
            state.results = [];
            state.searchError = (e && e.message) || String(e);
        }
        state.searching = false;
        renderSearchResults();
        return true;
    }

    /** Имя bundle по умолчанию — из имени репозитория (если поле ещё пустое). */
    function applyBundleName() {
        var nameInput = el('imgBundleName');
        if (!nameInput || nameInput.value) return;
        nameInput.value = String(String(state.selectedRepo || '').split('/').pop() || '').toLowerCase()
            .replace(/[^a-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 64);
    }

    /**
     * rememberRepo / restoreRepo — кэш одного репозитория.
     *
     * ЗАЧЕМ. Раскрытие — это ПЕРЕКЛЮЧАТЕЛЬ (повторный клик сворачивает). Без кэша
     * «свернул → раскрыл» заново тянуло бы /hf/files, /hf/plan и пред-проверки
     * (на живом репозитории это 1 + 1 + 3 запроса), а галочки оператора
     * сбрасывались бы в значения по умолчанию.
     *
     * ХРАНИМ ССЫЛКИ, а не копии: пред-проверка, доехавшая уже после сворачивания,
     * мутирует state.probes — и в кэше оказывается актуальный вердикт.
     */
    function rememberRepo(repo) {
        if (!repo) return;
        state.repoCache[repo] = {
            files: Array.isArray(state.files) ? state.files : [],
            selection: state.selection || {},
            probes: state.probes || {},
            plan: state.plan || null,
            familyNotice: state.familyNotice || ''
        };
    }

    function restoreRepo(repo) {
        var cached = repo ? state.repoCache[repo] : null;
        if (!cached) return false;
        state.files = cached.files;
        // empty → defaultSelection: у репозитория без файлов выбора нет, и это
        // нормальное состояние (пустое состояние рисует renderFiles).
        state.selection = cached.selection || {};
        state.probes = cached.probes || {};
        state.plan = cached.plan || null;
        state.familyNotice = cached.familyNotice || '';
        state.filesLoading = false;
        state.filesError = '';
        state.planLoading = false;
        applyBundleName();
        return true;
    }

    /**
     * collapseRepo — свернуть раскрытую модель.
     *
     * Галочки и выбранные роли НЕ выбрасываются: оператор свернул список, чтобы
     * посмотреть другую модель или дотянуться до кнопки скачивания, а не чтобы
     * заново отмечать файлы. Состояние живёт в кэше репозитория.
     */
    function collapseRepo() {
        rememberRepo(state.expandedRepo);
        state.expandedRepo = '';
        parkFilesCard();
        renderSearchResults();
        renderFiles();
        return true;
    }

    /**
     * pickRepo — раскрыть файлы репозитория В ЕГО ЖЕ карточке (R693).
     *
     * Поведение переключателя:
     *   - клик по свёрнутой карточке: раскрыть, при первом обращении — запрос
     *     /hf/files + /hf/plan + пред-проверка главного файла;
     *   - повторный клик по той же карточке: свернуть, НИ ОДНОГО запроса;
     *   - клик по другой карточке: раскрыть её (прежняя сворачивается);
     *   - повторное раскрытие ранее раскрытой: из кэша, без запросов.
     */
    async function pickRepo(repo) {
        if (!requireBackend()) return false;
        var target = String(repo || '');
        if (!target) return false;
        // Повторный клик по раскрытой карточке — свернуть (без сети).
        if (state.expandedRepo === target) return collapseRepo();
        // Уходим с предыдущего репозитория — сохраняем его состояние.
        rememberRepo(state.expandedRepo);
        state.expandedRepo = target;
        state.selectedRepo = target;
        renderSearchResults();
        // Номер попытки: клики по карточкам теперь переключатели, и оператор может
        // раскрыть вторую модель, пока грузится первая. Счётчик растёт на КАЖДОМ
        // переключении (в том числе на попадании в кэш): ответ устаревшей попытки
        // кладём в кэш под ЕГО репозиторием, а состояние (оно уже описывает
        // другую модель) не трогаем — иначе файлы одной модели показались бы под
        // именем другой.
        var seq = ++state.pickSeq;
        // Уже приходило — показываем из кэша (ни одного запроса).
        if (restoreRepo(target)) {
            renderFiles();
            revealFilesCard();
            return true;
        }
        state.files = [];
        state.filesError = '';
        state.filesLoading = true;
        state.selection = {};
        // Вердикты пред-проверки относятся к КОНКРЕТНОМУ репозиторию: при выборе
        // нового старые метки (и подставленное семейство) сбрасываем — иначе UI
        // показывал бы чужие имена тензоров как свои.
        state.probes = {};
        state.plan = null;
        state.planLoading = true;
        state.familyNotice = '';
        // «Загрузка…» появляется сразу в раскрытой карточке — оператор видит
        // реакцию на клик ещё до ответа воркера (живой поиск HF идёт секунды).
        renderFiles();
        revealFilesCard();
        var files = [];
        var fail = '';
        try {
            var data = await request(backendUrl('hf/files?modelId=' + encodeURIComponent(target) +
                '&revision=main'), { timeoutMs: TIMEOUT_SEARCH_MS });
            files = (data && Array.isArray(data.files)) ? data.files : [];
        } catch (e) {
            fail = (e && e.message) || String(e);
        }
        if (seq !== state.pickSeq) {
            // Попытка устарела: только кэш, без рендера и без плана/пред-проверок.
            state.repoCache[target] = {
                files: files,
                selection: defaultSelection(files, null),
                probes: {},
                plan: null,
                familyNotice: ''
            };
            return true;
        }
        state.files = files;
        // Отметки по умолчанию — по СЕРВЕРНЫМ ролям (suggestedRole).
        state.selection = defaultSelection(files, null);
        if (fail) state.filesError = fail;
        state.filesLoading = false;
        applyBundleName();
        rememberRepo(target);
        // Паспорт репозитория: комплектность набора + режим движка + шаги. Один
        // запрос к воркеру (он читает заголовок главного файла Range-запросом).
        loadPlan();
        renderFiles();
        // R696: вердикт по набору ДО скачивания (по умолчанию отмеченные файлы).
        scheduleVerify();
        // Пред-проверка главного файла: вердикт по репозиторию сразу, без кликов.
        autoProbeMainFile();
        return true;
    }

    /**
     * probeFile — пред-проверка файла: читает ТОЛЬКО заголовок (Range-запрос на
     * стороне воркера) и говорит, КАК движок увидит этот файл.
     *
     * ЗАЧЕМ: оператор скачал 4 ГБ GGUF и получил «get sd version from file
     * failed». Причина в семействе, а не в файле: DiT-модель, подключённая как
     * all-in-one (--model), движком не узнаётся. Теперь вердикт («движок узнаёт:
     * Qwen Image 2.1», DiT) видно рядом с файлом ещё до загрузки, а семейство
     * профиля подставляется автоматически (applyProbedFamily).
     */
    async function probeFile(path) {
        if (!path || !backendId()) return null;
        if (state.probing[path]) return state.probes[path] || null;
        state.probing[path] = true;
        renderFiles();
        try {
            var url = backendUrl('hf/probe?modelId=' + encodeURIComponent(state.selectedRepo) +
                '&filename=' + encodeURIComponent(path) + '&revision=main');
            var res = await request(url, { timeoutMs: TIMEOUT_SEARCH_MS });
            state.probes[path] = res || { verdict: 'unknown' };
        } catch (e) {
            state.probes[path] = { verdict: 'unknown', reason: (e && e.message) || String(e) };
        }
        delete state.probing[path];
        // DiT-файл + семейство all-in-one в профиле = «get sd version from file
        // failed» на загрузке. Если оператор не выбирал семейство сам — правим.
        var notice = applyProbedFamily(state.probes[path]);
        if (notice) state.familyNotice = notice;
        renderFiles();
        return state.probes[path];
    }

    /**
     * loadPlan — «паспорт репозитория» (GET /api/hf/plan).
     *
     * ЗАЧЕМ: поиск показывает файлы, а оператору нужно знать НАБОР и РЕЖИМ.
     * Живой случай 2026-10-06: скачан только diffusion Qwen-Image 2.1, движок
     * ответил «VAE tensor ... not in model metadata» и не поднялся. Паспорт
     * заранее говорит: режим --diffusion-model, нужны роли vae и llm, их нет —
     * и что делать по шагам (в том числе какое имя модели передавать инструменту).
     */
    async function loadPlan() {
        if (!state.selectedRepo || !backendId()) return null;
        var repo = state.selectedRepo;
        state.planLoading = true;
        renderFiles();
        try {
            var res = await request(backendUrl('hf/plan?modelId=' + encodeURIComponent(repo) + '&revision=main'),
                { timeoutMs: TIMEOUT_SEARCH_MS });
            if (state.selectedRepo === repo) state.plan = res || null;
        } catch (e) {
            if (state.selectedRepo === repo) {
                state.plan = { verdict: 'unknown', summary: (e && e.message) || String(e) };
            }
        }
        state.planLoading = false;
        renderFiles();
        return state.plan;
    }

    /**
     * planHtml — разметка паспорта: режим, комплектность обязательных ролей, шаги
     * и подсказка модели, которая поднимает модель инструментом.
     */
    function planHtml(plan) {
        if (!plan) {
            return state.planLoading
                ? '<div style="margin:10px 0;font-size:var(--font-size-xs);color:var(--text-muted);"><i class="fas fa-spinner fa-spin"></i> ' +
                    escapeHtml(t('imageModels.plan_loading', 'собираю паспорт набора…')) + '</div>'
                : '';
        }
        var verdictColor = plan.verdict === 'ready' ? 'var(--success, #5cb85c)'
            : (plan.verdict === 'incomplete' ? 'var(--warning, #e0a030)'
                : (plan.verdict === 'unsupported' ? 'var(--danger, #d9534f)' : 'var(--text-muted)'));
        var verdictIcon = plan.verdict === 'ready' ? 'fa-circle-check'
            : (plan.verdict === 'incomplete' ? 'fa-triangle-exclamation'
                : (plan.verdict === 'unsupported' ? 'fa-circle-xmark' : 'fa-circle-question'));
        var html = '<div style="margin:12px 0;padding:10px 12px;border-left:3px solid ' + verdictColor +
            ';background:var(--bg-secondary);border-radius:6px;font-size:var(--font-size-xs);">' +
            '<div style="color:' + verdictColor + ';font-weight:600;"><i class="fas ' + verdictIcon + '"></i> ' +
            escapeHtml(plan.summary || '') + '</div>';
        if (plan.engineMode && plan.engineMode !== 'unknown') {
            html += '<div style="margin-top:6px;color:var(--text-muted);">' +
                escapeHtml(t('imageModels.plan_mode', 'Режим запуска')) + ': <code>' +
                escapeHtml(plan.engineMode === 'model' ? '--model (all-in-one)' : '--diffusion-model + VAE/text encoder') +
                '</code></div>';
        }
        var required = (plan.roles || []).filter(function (r) { return r.required; });
        if (required.length) {
            html += '<div style="margin-top:6px;">' + required.map(function (r) {
                var mark = r.found ? '✅' : '❌';
                var files = (r.files || []).length ? ' — ' + escapeHtml(r.files.join(', ')) : '';
                var hint = (!r.found && r.hint) ? '<div style="color:var(--text-muted);margin-left:18px;">' +
                    escapeHtml(r.hint) + '</div>' : '';
                return '<div>' + mark + ' ' + escapeHtml(r.label) + files + '</div>' + hint;
            }).join('') + '</div>';
        }
        if (plan.steps && plan.steps.length) {
            html += '<div style="margin-top:8px;"><b>' + escapeHtml(t('imageModels.plan_steps', 'Порядок действий')) +
                ':</b><ol style="margin:4px 0 0 18px;padding:0;">' +
                plan.steps.map(function (s) { return '<li>' + escapeHtml(s) + '</li>'; }).join('') + '</ol></div>';
        }
        if (plan.modelHint) {
            html += '<div style="margin-top:8px;color:var(--text-muted);"><i class="fas fa-robot"></i> ' +
                escapeHtml(plan.modelHint) + '</div>';
        }
        return html + '</div>';
    }
    /**
     * mainWeightFile — файл, по которому судим о репозитории: самый крупный
     * DIFFUSION-файл (роль приходит с сервера), и только если такого нет — самый
     * крупный файл весов.
     *
     * ЗАЧЕМ НЕ ПРОСТО «САМЫЙ КРУПНЫЙ»: в DiT-репозиториях text encoder бывает
     * больше самой модели (Qwen-Image: 17.5 ГБ TE против 7.6 ГБ Qwen-2.1-Q8_0),
     * и «самый крупный» уводил пред-проверку на энкодер — вердикт «не
     * определяется», а семейство профиля не подставлялось (проверено живьём).
     */
    function mainWeightFile(files) {
        var weights = (files || []).filter(function (f) {
            return /\.(gguf|safetensors)$/i.test(filePath(f));
        });
        if (!weights.length) return null;
        var diffusion = weights.filter(function (f) { return suggestedRole(f) === 'diffusion'; });
        var pool = (diffusion.length ? diffusion : weights).slice();
        pool.sort(function (a, b) { return fileSize(b) - fileSize(a); });
        return pool[0];
    }

    /**
     * autoProbeMainFile — после выбора репозитория проверить главный файл И
     * крупные safetensors.
     *
     * ПОЧЕМУ НЕ ТОЛЬКО ГЛАВНЫЙ (живой случай 2026-10-06): виновником оказался
     * `qwen-image-2.1-UC-MLX-4bit.safetensors` — крупнейший файл репозитория, но
     * НЕ единственный, и вердикт по нему не появлялся, пока оператор не нажмёт
     * «Проверить». Проверяем главный файл плюс до двух самых крупных safetensors:
     * это 3 Range-запроса по 512 КБ, зато «формат не поддерживается» видно сразу.
     */
    async function autoProbeMainFile() {
        var targets = autoProbeTargets(state.files);
        var out = null;
        for (var i = 0; i < targets.length; i++) {
            var res = await probeFile(targets[i]);
            if (!out) out = res;
        }
        return out;
    }

    /** autoProbeTargets — какие файлы проверить автоматически (порядок стабилен). */
    function autoProbeTargets(files) {
        var list = Array.isArray(files) ? files : [];
        var main = mainWeightFile(list);
        var mainPath = main ? filePath(main) : '';
        var out = [];
        if (mainPath) out.push(mainPath);
        var sf = list.filter(function (f) {
            var p = filePath(f);
            return /\.safetensors$/i.test(p) && p !== mainPath;
        }).sort(function (a, b) { return fileSize(b) - fileSize(a); });
        sf.slice(0, 2).forEach(function (f) {
            var p = filePath(f);
            if (out.indexOf(p) === -1) out.push(p);
        });
        return out;
    }

    function setSelection(path, patch) {
        if (!path) return;
        var cur = state.selection[path] || { checked: false, role: suggestedRole({}), sizeBytes: 0 };
        state.selection[path] = {
            checked: patch && Object.prototype.hasOwnProperty.call(patch, 'checked') ? !!patch.checked : cur.checked,
            role: (patch && patch.role) ? patch.role : cur.role,
            sizeBytes: cur.sizeBytes || 0
        };
        renderSelectionSummary();
        // Счётчик обновился сразу (без сети); вердикт по набору — с задержкой,
        // потому что это Range-запросы к HF (R696).
        scheduleVerify();
    }

    /** Отметить по предложенным сервером ролям (кнопка «как предложено»). */
    function selectBySuggested() {
        state.selection = defaultSelection(state.files, null);
        renderFiles();
        renderSelectionSummary();
        scheduleVerify();
        return selectedCount();
    }

    function clearSelection() {
        Object.keys(state.selection).forEach(function (p) { state.selection[p].checked = false; });
        renderFiles();
        renderSelectionSummary();
        scheduleVerify();
        return 0;
    }

    function stopPolling() {
        if (state.poll) clearInterval(state.poll);
        state.poll = null;
    }

    function startBundlePolling(name) {
        stopPolling();
        state.pollIdleTicks = 0;
        var tick = async function () {
            if (!backendId() || !name) { stopPolling(); return; }
            // Прогресс скачивания — тоже опрос API: на паузе авто-обновления и в
            // скрытой вкладке пропускаем тик (правило одно для всего WebUI,
            // data-refresh.js). Состояние записи не теряется: следующий тик
            // после возврата покажет актуальный прогресс.
            if (window.DataRefresh && typeof window.DataRefresh.shouldPoll === 'function' &&
                !window.DataRefresh.shouldPoll()) {
                return;
            }
            try {
                var p = await request(backendUrl('hf/progress?bundleId=' + encodeURIComponent(name)), { timeoutMs: TIMEOUT_CONTROL_MS });
                if (!p) { state.pollIdleTicks++; }
                else {
                    state.pollIdleTicks = 0;
                    state.bundle = p;
                    renderBundleProgress();
                }
            } catch (e) {
                // 404 = записи ещё нет: не ошибка, но и не прогресс.
                state.pollIdleTicks++;
            }
            if (state.pollIdleTicks >= 20) { stopPolling(); return; }
            var status = state.bundle ? String(state.bundle.status || '') : '';
            if (status && status !== 'downloading') {
                stopPolling();
                if (status === 'completed') {
                    toast(t('image.bundle_all_done', 'All bundle files downloaded'), 'success');
                    // R749: файлы доехали - перечитаем реестр узла, чтобы корзина
                    // сразу показала «есть на узле», а не ждала перезахода на таб.
                    refreshBackendFiles();
                    refreshModels();
                } else {
                    toast(t('image.bundle_failed', 'Bundle download failed') + ': ' + status, 'error');
                }
                refreshDownloads();
            }
        };
        tick();
        state.poll = setInterval(tick, DEFAULT_POLL_MS);
    }

    /** После успешной загрузки список моделей изменился — просим его перечитать. */
    function refreshModels() {
        if (window.ImagePage && window.ImagePage._actions && typeof window.ImagePage._actions.refresh === 'function') {
            try { window.ImagePage._actions.refresh(); } catch (e) { /* страница может быть не смонтирована */ }
        }
    }

    /**
     * startBundle — запустить bundle-загрузку (POST /api/hf/bundle).
     *
     * opts.onlyMissing (R749) — отправить ТОЛЬКО те отмеченные файлы, которых нет
     * в реестре узла (кнопка «Скачать недостающее»). Если таких нет, запрос не
     * уходит вообще: «скачать ничего» не должно выглядеть как успех.
     */
    async function startBundle(opts) {
        opts = opts || {};
        if (!requireBackend()) return false;
        var nameInput = el('imgBundleName');
        var familySel = el('imgBundleFamily');
        var tokenInput = el('imgHfToken');
        if (tokenInput && tokenInput.value) saveHfToken(tokenInput.value);
        var built = buildFileSpecs(state.selectedRepo, state.selection, {
            name: nameInput ? nameInput.value : '',
            // R710: компоненты, добавленные к набору из других репозиториев
            // (VAE/text encoder), скачиваются в том же bundle — иначе после
            // загрузки вердикт снова сказал бы «не хватает VAE».
            components: state.extraComponents || []
        });
        if (built.errors.length) {
            built.errors.forEach(function (err) {
                var info = bundleErrorToI18n(err);
                toast(t(info.key, info.fallback, info.vars), 'error');
            });
            return false;
        }
        // R749: режим «только недостающее» фильтрует УЖЕ проверенный список.
        var filesToSend = built.files;
        if (opts.onlyMissing) {
            filesToSend = built.files.filter(function (f) {
                return !presenceOf(f.filename).present;
            });
            if (!filesToSend.length) {
                toast(t('imageModels.cart_all_present', 'Всё выбранное уже есть на узле'), 'info');
                return false;
            }
        }
        var family = familySel && familySel.value ? String(familySel.value) : 'other';
        var body = JSON.stringify({
            name: built.name,
            family: family,
            files: filesToSend.map(function (f) {
                // sizeBytes нужен воркеру, чтобы не перекачивать уже лежащие
                // файлы нужного размера (hf_bundle.go:128).
                return f.sizeBytes > 0
                    ? { role: f.role, repo: f.repo, filename: f.filename, revision: f.revision, sizeBytes: f.sizeBytes }
                    : { role: f.role, repo: f.repo, filename: f.filename, revision: f.revision };
            })
        });
        state.bundle = { bundleId: built.name, status: 'downloading', downloaded: 0, totalBytes: 0, files: [] };
        renderBundleProgress();
        try {
            await request(backendUrl('hf/bundle'), { method: 'POST', body: body, timeoutMs: TIMEOUT_CONTROL_MS });
        } catch (e) {
            state.bundle = null;
            renderBundleProgress();
            toast(t('image.bundle_failed', 'Bundle download failed') + ': ' + ((e && e.message) || String(e)), 'error');
            return false;
        }
        toast(t('image.bundle_started', 'Bundle download started'), 'success');
        startBundlePolling(built.name);
        // «Загрузки» — тот же модуль: обновим снимок, чтобы активная загрузка
        // была видна сразу, без перезахода на таб.
        refreshDownloads();
        return true;
    }

    async function refreshDownloads() {
        if (!backendId()) {
            state.downloads = null;
            renderDownloads();
            return null;
        }
        try {
            var data = await request(backendUrl('hf/downloads'), { timeoutMs: TIMEOUT_CONTROL_MS });
            state.downloads = data || null;
            state.downloadsError = '';
        } catch (e) {
            state.downloads = null;
            state.downloadsError = (e && e.message) || String(e);
        }
        renderDownloads();
        return state.downloads;
    }

    async function cancelBundle(name) {
        if (!name) return false;
        try {
            await request(backendUrl('hf/cancel'), {
                method: 'POST',
                body: JSON.stringify({ bundleId: name }),
                timeoutMs: TIMEOUT_CONTROL_MS
            });
            toast(t('gguf.download_cancelled', 'Download cancelled'), 'info');
        } catch (e) {
            toast(t('common.error', 'Error') + ': ' + ((e && e.message) || String(e)), 'error');
        }
        stopPolling();
        await refreshDownloads();
        return true;
    }

    async function cancelFile(modelId, filename) {
        try {
            await request(backendUrl('hf/cancel'), {
                method: 'POST',
                body: JSON.stringify({ modelId: modelId, filename: filename }),
                timeoutMs: TIMEOUT_CONTROL_MS
            });
            toast(t('gguf.download_cancelled', 'Download cancelled'), 'info');
        } catch (e) {
            toast(t('common.error', 'Error') + ': ' + ((e && e.message) || String(e)), 'error');
        }
        await refreshDownloads();
        return true;
    }

    /**
     * cleanupOrphan — удаление остаточного .download.
     *
     * DELETE /api/hf/cleanup принимает query-параметры (cmd/sdworker/handlers_hf.go:475),
     * поэтому шлём filename в query: имя может содержать "<bundle>/<file>" — его
     * нельзя класть в путь, только в параметр.
     */
    async function cleanupOrphan(filename) {
        if (!filename) return false;
        try {
            await request(backendUrl('hf/cleanup?filename=' + encodeURIComponent(filename)), {
                method: 'DELETE',
                timeoutMs: TIMEOUT_CONTROL_MS
            });
            toast(t('gguf.file_deleted', 'File deleted'), 'success');
        } catch (e) {
            toast(t('common.error', 'Error') + ': ' + ((e && e.message) || String(e)), 'error');
        }
        await refreshDownloads();
        return true;
    }

    // =====================================================================
    // Монтирование
    // =====================================================================

    function renderFamilySelect() {
        var sel = el('imgBundleFamily');
        if (!sel) return;
        var prev = sel.value || state.bundleFamily;
        sel.innerHTML = IMAGE_FAMILIES.map(function (f) {
            return '<option value="' + escapeHtml(f) + '">' + escapeHtml(f) + '</option>';
        }).join('');
        if (prev) sel.value = prev;
        state.bundleFamily = sel.value || 'sd15';
    }

    function onClick(ev) {
        var target = ev && ev.target;
        // R693: карточка файлов теперь физически лежит ВНУТРИ карточки результата,
        // а та кликабельна целиком (data-imh-repo). Без этой проверки клик по
        // галочке файла или по селекту роли всплывал бы до карточки и СВОРАЧИВАЛ
        // раскрытую модель прямо под рукой оператора. Действия внутри карточки
        // («Проверить», отмена загрузки) продолжают работать: пропускаем только
        // ветку «показать/свернуть файлы».
        var insideFiles = false;
        try {
            insideFiles = !!(target && filesCardNode &&
                typeof filesCardNode.contains === 'function' && filesCardNode.contains(target));
        } catch (e) { insideFiles = false; }
        while (target && target.getAttribute) {
            if (!insideFiles) {
                var repo = target.getAttribute('data-imh-repo') || target.getAttribute('data-imh-pick');
                if (repo) { pickRepo(repo); return; }
            }
            // Пред-проверка файла (заголовок, без скачивания).
            var probePath = target.getAttribute('data-imh-probe');
            if (probePath) { probeFile(probePath); return; }
            var cancel = target.getAttribute('data-imh-cancel');
            if (cancel) { cancelBundle(cancel); return; }
            var cancelFileBtn = target.getAttribute('data-imh-cancel-file');
            if (cancelFileBtn) {
                cancelFile(target.getAttribute('data-imh-model'), target.getAttribute('data-imh-filename'));
                return;
            }
            var orphan = target.getAttribute('data-imh-orphan');
            if (orphan) { cleanupOrphan(orphan); return; }
            var act = target.getAttribute('data-imh-action');
            if (act === 'search') { search(); return; }
            if (act === 'suggested') { selectBySuggested(); return; }
            if (act === 'clear') { clearSelection(); return; }
            if (act === 'download') { startBundle(); return; }
            // R749: скачать только отсутствующие на узле файлы.
            if (act === 'download-missing') { downloadMissing(); return; }
            // R749: реестр узла не прочитан - перечитать (постоянной кнопки
            // обновления здесь нет: она появляется только при ошибке чтения).
            if (act === 'scan-backend') { refreshBackendFiles(); return; }
            // R696: вердикт по выбранному набору до скачивания.
            if (act === 'verify') { verifySelection(true); return; }
            // R710: недостающие компоненты набора.
            var compDefault = target.getAttribute('data-icom-default');
            if (compDefault) {
                // В режиме selection (вкладка HF) «по умолчанию» добавляет файл в
                // выбор; режим bundle рисует страница «Модели на диске» и ведёт
                // его сама через window.ImageComponents.installBundle.
                addDefaultComponents([compDefault]);
                return;
            }
            if (target.getAttribute('data-icom-all')) {
                addDefaultComponents(missingComponentRoles(state.verify));
                return;
            }
            if (target.getAttribute('data-icom-manual')) { openComponentManual(); return; }
            if (target.getAttribute('data-icom-manual-load')) { loadComponentManualFiles(); return; }
            if (target.getAttribute('data-icom-manual-add')) { addManualComponents(); return; }
            if (target.getAttribute('data-icom-manual-cancel')) { closeComponentManual(); return; }
            var compRemove = target.getAttribute('data-icom-remove');
            if (compRemove) { removeExtraComponent(compRemove); return; }
            // R493 (2026-11-17): ветки 'refresh-downloads' больше нет — кнопка в
            // разметке удалена (одна кнопка обновления в шапке). Действие
            // зарегистрировано провайдером страницы image ниже в этом файле.
            target = target.parentNode;
        }
    }

    function onChange(ev) {
        var target = ev && ev.target;
        if (!target || !target.getAttribute) return;
        // R710: ручная форма компонента — репозиторий и отметки файлов.
        if (target.getAttribute('data-icom-manual-repo') !== null && state.componentManual) {
            state.componentManual.repo = String(target.value || '');
            return;
        }
        var manualPick = target.getAttribute('data-icom-manual-pick');
        if (manualPick !== null && manualPick !== undefined && manualPick !== '' && state.componentManual) {
            var prev = (state.componentManual.selection || {})[manualPick] || {};
            state.componentManual.selection[manualPick] = {
                checked: !!target.checked,
                role: prev.role || 'vae'
            };
            return;
        }
        var manualRole = target.getAttribute('data-icom-manual-role');
        if (manualRole && state.componentManual) {
            var cur = (state.componentManual.selection || {})[manualRole] || { checked: false };
            state.componentManual.selection[manualRole] = { checked: !!cur.checked, role: String(target.value || 'vae') };
            return;
        }
        var check = target.getAttribute('data-imh-check');
        if (check !== null && check !== undefined && check !== '') {
            // ЗАПРЕТ ВЫБОРА НЕПОДДЕРЖИВАЕМОГО ФАЙЛА (живой случай 2026-10-06):
            // оператор скачал 4 ГБ MLX-квантов и получил отказ загрузки. Если
            // пред-проверка уже сказала «формат не поддерживается» — не даём
            // отметить файл в bundle и объясняем, что взять вместо него.
            var pr = state.probes[check];
            if (target.checked && pr && pr.verdict === 'unsupported') {
                target.checked = false;
                toast(t('imageModels.probe_unsupported_block',
                    'Этот файл движок не прочитает: {reason}', { reason: pr.reason || '' }), 'error');
                return;
            }
            setSelection(check, { checked: !!target.checked });
            return;
        }
        var role = target.getAttribute('data-imh-role');
        if (role) {
            setSelection(role, { role: target.value });
            // D9 (2026-11-25): смена роли ОБЯЗАНА перерисовать список.
            //
            // Живой дефект: подсказка «(предложено: …)» и предупреждение о
            // неполном наборе собираются при отрисовке, а обработчик менял
            // только state и возвращался — оператор менял роль и не видел,
            // что подсказка/вердикт всё ещё относятся к прежнему выбору.
            // Тот же приём, что у селекта семейства (ниже): изменение
            // выбора = повод перерисовать.
            renderFiles();
            return;
        }
        if (target.id === 'imgBundleFamily') {
            state.bundleFamily = target.value;
            // Оператор выбрал семейство руками — автоподстановка больше не лезет.
            state.familyManual = true;
            // Предупреждение «DiT-файл против all-in-one семейства» зависит от
            // этого селекта: без перерисовки оператор увидел бы старую шапку.
            renderFiles();
        }
    }

    /**
     * onInput — ввод в корзине (R749).
     *
     * Имя bundle правится посимвольно, а строка «Куда» должна показывать его
     * сразу, не дожидаясь blur: иначе оператор видит «имя не задано» при уже
     * введённом имени и не понимает, куда уйдёт набор.
     */
    function onInput(ev) {
        var target = ev && ev.target;
        if (!target || target.id !== 'imgBundleName') return;
        renderCart();
    }

    /**
     * applyProbedFamily — подставить семейство профиля по заголовку файла.
     *
     * ЗАЧЕМ: именно промах по семейству даёт «get sd version from file failed».
     * DiT-файл (flux/sd3/qwen/z-image/chroma), подключённый как all-in-one
     * (--model), движок не узнаёт; при выборе DiT-семейства воркер сам передаст
     * --diffusion-model. Подставляем ТОЛЬКО если оператор не трогал селект.
     */
    function applyProbedFamily(probe) {
        if (!probe || probe.verdict !== 'supported' || !probe.family) return '';
        if (state.familyManual) return '';
        if (IMAGE_FAMILIES.indexOf(probe.family) === -1) return '';
        if (state.bundleFamily === probe.family) return '';
        var prev = state.bundleFamily;
        state.bundleFamily = probe.family;
        var sel = el('imgBundleFamily');
        if (sel) sel.value = probe.family;
        return t('imageModels.family_autoset',
            'Семейство профиля переключено на «{family}» по заголовку файла (было «{prev}»): иначе движок получит all-in-one флаг и ответит «get sd version from file failed».',
            { family: probe.family, prev: prev });
    }

    /** mainProbeResult — результат пред-проверки главного (diffusion) файла. */
    function mainProbeResult(files) {
        var main = mainWeightFile(files);
        if (!main) return null;
        return state.probes[filePath(main)] || null;
    }

    /**
     * renderActiveTab — перерисовать ДИНАМИКУ показанного таба после смены языка.
     *
     * ЗАЧЕМ ОТДЕЛЬНАЯ ФУНКЦИЯ, А НЕ render(ctx). render() на табе «Загрузки»
     * перечитывает снимок воркера (refreshDownloads) — на смену языка сеть ходить
     * не должна: данные те же, поменялся только текст. Здесь же ни одного запроса:
     * renderSearchResults/renderFiles/renderDownloads только пересобирают HTML из
     * уже лежащего в state (R693: карточка файлов переезжает в слот раскрытой
     * модели тем же узлом, а state.expandedRepo/selection/repoCache не трогаются,
     * поэтому повторного GET /hf/files не будет, раскрытие и отметки сохранятся).
     *
     * Неактивный таб перерисовывать не нужно: при возврате на него шелл зовёт
     * render(ctx) (image-models-page.js → showTab → renderTabs), и он соберётся
     * уже на новом языке. Это и есть «без повторного поиска».
     */
    function renderActiveTab() {
        var tab = state.ctx && state.ctx.tab;
        if (tab === 'downloads') {
            // Снимок тот же — только подписи статусов/заголовки на новом языке.
            if (state.downloads) renderDownloads();
            return false;
        }
        if (tab !== 'hf') return false;
        // R749: реестр узла нужен корзине - читаем один раз на показ таба.
        ensureBackendFiles();
        renderSearchResults();
        renderFiles();
        renderBundleProgress();
        renderCart();
        return true;
    }

    function mount() {
        if (mounted) return;
        mounted = true;
        var page = el('image-page') || document;
        if (page && page.addEventListener) {
            // Делегирование: контент табов перерисовывается, слушатели — одни.
            page.addEventListener('click', onClick);
            page.addEventListener('change', onChange);
            // R749: имя bundle в корзине обновляет строку «Куда» по вводу.
            if (!inputHandler) {
                inputHandler = onInput;
                page.addEventListener('input', inputHandler);
            }
        }
        // R694: динамические подписи табов HF/Загрузки собраны в JS, поэтому
        // штатный I18N.apply(document) их не переводит — перерисовываем сами.
        // Соседние модули (image-backends-page.js:1139, image-profiles.js:1202)
        // делают так же; отличие — обработчик хранится в переменной, чтобы
        // unmount() мог снять подписку и повторный mount не задвоил её.
        if (!i18nHandler && window.addEventListener) {
            i18nHandler = function () { renderActiveTab(); };
            window.addEventListener('i18n:changed', i18nHandler);
        }
        var query = el('imHfQuery');
        if (query && query.addEventListener && !keydownHandler) {
            keydownHandler = function (ev) {
                if (ev && ev.key === 'Enter') { ev.preventDefault(); search(); }
            };
            query.addEventListener('keydown', keydownHandler);
        }
        renderFamilySelect();
        var tokenInput = el('imgHfToken');
        if (tokenInput && !tokenInput.value) tokenInput.value = hfToken();
        // R493 (2026-11-17): таб «Загрузки» больше не имеет своей кнопки
        // «Обновить» — её действие (перечитать /api/hf/downloads) отдаётся
        // провайдеру страницы image, то есть общей кнопке в шапке. Регистрация
        // в mount, а не в render: mount идемпотентен (mounted), поэтому
        // провайдер не задвоится при повторных показах таба. Флаг нужен и после
        // unmount(): у реестра DataRefresh нет отписки, второй register()
        // заставил бы общую кнопку обновления дёргать «Загрузки» дважды.
        if (!dataRefreshBound && window.DataRefresh && typeof window.DataRefresh.register === 'function') {
            dataRefreshBound = true;
            window.DataRefresh.register('image', function () {
                // R749: общая кнопка обновления страницы обновляет и реестр узла
                // для корзины - отдельной кнопки на таб не заводим (R493).
                refreshBackendFiles();
                return refreshDownloads();
            });
        }
    }

    /**
     * unmount — снять подписку на window и вернуть модуль в «не смонтирован».
     *
     * В SPA страница живёт до перезагрузки, и шелл (image-models-page.js) не
     * выгружает табы; поэтому «выгрузка» здесь — явный вызов (тест, смена
     * страницы хостом в будущем). Паттерн тот же, что у mount: идемпотентность
     * через флаг. Без removeEventListener повторный mount навесил бы вторую
     * подписку, и одно i18n:changed перерисовывало бы выдачу дважды.
     *
     * ЧЕГО ЗДЕСЬ НЕТ: регистрация в DataRefresh не снимается — у реестра нет
     * unregister, поэтому она однократная (флаг dataRefreshBound), а не
     * «отписываемая»; врать про полный teardown хуже, чем назвать границу.
     */
    function unmount() {
        if (!mounted) return false;
        mounted = false;
        // R696: таймер отложенной проверки набора не должен дёргать сеть после
        // выгрузки модуля.
        if (state.verifyTimer) clearTimeout(state.verifyTimer);
        state.verifyTimer = null;
        if (i18nHandler && window.removeEventListener) {
            window.removeEventListener('i18n:changed', i18nHandler);
        }
        i18nHandler = null;
        var page = el('image-page');
        if (page && page.removeEventListener) {
            page.removeEventListener('click', onClick);
            page.removeEventListener('change', onChange);
            if (inputHandler) {
                page.removeEventListener('input', inputHandler);
                inputHandler = null;
            }
        }
        if (keydownHandler) {
            var query = el('imHfQuery');
            if (query && query.removeEventListener) query.removeEventListener('keydown', keydownHandler);
            keydownHandler = null;
        }
        return true;
    }

    /** render(ctx) — вызывается шеллом при показе таба. */
    function render(ctx) {
        if (ctx) state.ctx = ctx;
        mount();
        // R749: бэкенд мог смениться на странице - индекс реестра относится к
        // прежней машине и обязан быть сброшен, иначе корзина покажет чужое
        // «есть на узле».
        if (state.backendFilesBackendId && state.backendFilesBackendId !== backendId()) {
            state.backendFiles = null;
            state.backendFilesError = '';
            state.backendFilesBackendId = '';
        }
        var tab = state.ctx && state.ctx.tab;
        if (tab === 'downloads') {
            // Всегда перечитываем снимок: на этом табе данные меняются каждую
            // секунду, а показать прошлый снимок «как есть» — значит соврать
            // (например, показать «Загрузок нет» сразу после старта bundle).
            // Прошлый снимок рисуем сразу, чтобы не мигало пустое место.
            if (state.downloads) renderDownloads();
            refreshDownloads();
            return;
        }
        // Таб HF (и любой другой): только перерисовка из state, один код с
        // обработчиком i18n:changed — см. renderActiveTab.
        renderActiveTab();
    }

    window.ImageModelsHf = {
        mount: mount,
        // R694: симметричное снятие подписок (см. unmount выше).
        unmount: unmount,
        render: render,
        tabIds: TAB_IDS,
        _actions: {
            search: search,
            pickRepo: pickRepo,
            // R693: свернуть раскрытую модель и вернуть карточку файлов на место.
            collapseRepo: collapseRepo,
            setSelection: setSelection,
            selectBySuggested: selectBySuggested,
            clearSelection: clearSelection,
            startBundle: startBundle,
            refreshDownloads: refreshDownloads,
            cancelBundle: cancelBundle,
            cancelFile: cancelFile,
            cleanupOrphan: cleanupOrphan,
            stopPolling: stopPolling,
            saveHfToken: saveHfToken,
            probeFile: probeFile,
            autoProbeMainFile: autoProbeMainFile,
            // R696: вердикт по выбранному набору до скачивания и сводка выбора.
            verifySelection: verifySelection,
            scheduleVerify: scheduleVerify,
            selectedBytes: selectedBytes,
            renderSelectionSummary: renderSelectionSummary,
            renderVerifyVerdict: renderVerifyVerdict,
            // R749: корзина - состав, состояние узла и «скачать недостающее».
            renderCart: renderCart,
            cartEntries: cartEntries,
            presenceOf: presenceOf,
            refreshBackendFiles: refreshBackendFiles,
            downloadMissing: downloadMissing,
            // R710: компоненты набора (для тестов и для страницы «Модели на диске»).
            loadComponentCatalog: loadComponentCatalog,
            addDefaultComponents: addDefaultComponents,
            openComponentManual: openComponentManual,
            loadComponentManualFiles: loadComponentManualFiles,
            addManualComponents: addManualComponents,
            removeExtraComponent: removeExtraComponent
        },
        _state: state,
        pure: {
            escapeHtml: escapeHtml,
            formatBytes: formatBytes,
            filePath: filePath,
            fileSize: fileSize,
            suggestedRole: suggestedRole,
            isValidRole: isValidRole,
            defaultSelection: defaultSelection,
            buildFileSpecs: buildFileSpecs,
            bundleErrorToI18n: bundleErrorToI18n,
            progressPercent: progressPercent,
            bundleStatusKey: bundleStatusKey,
            downloadsSummary: downloadsSummary,
            IMAGE_ROLES: IMAGE_ROLES,
            ROLE_LABEL_KEYS: ROLE_LABEL_KEYS,
            roleLabel: roleLabel,
            roleOptions: roleOptions,
            IMAGE_FAMILIES: IMAGE_FAMILIES,
            AUTO_ROLES: AUTO_ROLES,
            repoCompatibility: repoCompatibility,
            probeBadge: probeBadge,
            diTFamilyWarning: diTFamilyWarning,
            ditHint: ditHint,
            compatBadgeHtml: compatBadgeHtml,
            DIT_FAMILIES: DIT_FAMILIES,
            mainWeightFile: mainWeightFile,
            // R86-follow-up: какие файлы проверять автоматически (главный + крупные
            // safetensors) — экспортируется, чтобы это правило было под тестом.
            autoProbeTargets: autoProbeTargets,
            planHtml: planHtml,
            // R696: отпечаток выбора — по нему кэшируется вердикт набора.
            selectionKey: selectionKey,
            selectedFileSpecs: selectedFileSpecs,
            // R710: компоненты набора — чистые помощники под тестом
            // (webui/tests/image-components-r710.test.js).
            COMPONENT_ROLES: COMPONENT_ROLES,
            missingComponentRoles: missingComponentRoles,
            normalizeComponent: normalizeComponent,
            defaultComponentFor: defaultComponentFor,
            componentButtonsHtml: componentButtonsHtml,
            componentConfirmText: componentConfirmText,
            componentWarningText: componentWarningText,
            componentSizeText: componentSizeText,
            componentRoleRankVal: componentRoleRankVal,
            componentErrorText: componentErrorText,
            // R749: сверка состава корзины с реестром узла - чистые помощники
            // под тестом (webui/tests/image-hf-cart-r749.test.js).
            buildBackendFileIndex: buildBackendFileIndex,
            presenceLookup: presenceLookup,
            normPathKey: normPathKey,
            basenameKey: basenameKey
        }
    };

    /**
     * window.ImageComponents — публичный контракт компонентов для страницы
     * «Модели на диске» (webui/js/modules/image-page.js).
     *
     * ЗАЧЕМ ОТДЕЛЬНОЕ ИМЯ, А НЕ ImageModelsHf: у страницы моделей своя логика
     * выбора бэкенда и своя таблица; ей нужны ровно три вещи — «какие кнопки
     * рисовать», «что спросить у оператора» и «как поставить загрузку». Держать
     * их в ImageModelsHf значило бы заставить чужую страницу знать про HF-табы.
     *
     * ПОЧЕМУ ЭКСПОРТ ЖИВЁТ ЗДЕСЬ, А НЕ В НОВОМ ФАЙЛЕ: модуль уже загружен на
     * странице «Image-модели» (там оба таба), и второй <script> в index.html
     * ради 5 функций — лишняя точка рассинхронизации сборки.
     */
    if (typeof window !== 'undefined') {
        window.ImageComponents = {
            roles: COMPONENT_ROLES,
            missingRoles: missingComponentRoles,
            buttonsHtml: componentButtonsHtml,
            confirmText: componentConfirmText,
            warningText: componentWarningText,
            sizeText: componentSizeText,
            errorText: componentErrorText,
            normalize: normalizeComponent,
            defaultFor: defaultComponentFor,
            busyHtml: componentBusyHtml,
            noteFor: noteForModel,
            loadCatalog: function (family, roles) {
                return loadComponentCatalog(fromSelSafe(family), roles || []);
            },
            listRepoFiles: listRepoFiles,
            installBundle: installBundleComponent
        };
    }
})();
