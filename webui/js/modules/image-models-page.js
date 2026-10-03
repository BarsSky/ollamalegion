/**
 * image-models-page.js — R-Image Phase 9: страница «Image-модели»
 * (табы в стиле «GGUF модели»).
 *
 * ЧТО ЭТО. Шелл страницы: переключение табов, выбор image-бэкенда, «Проверка
 * бэкенда» и точка входа для табов-модулей. Сама работа с моделями/HF/профилями
 * живёт в существующих модулях (image-page.js, image-backends-page.js,
 * image-profiles.js) и в табах-модулях (image-models-hf.js,
 * image-models-list.js) — шелл их только показывает.
 *
 * ПОЧЕМУ ИМЕННО ТАК (границы):
 *   - разметка табов ВЗЯТА ИЗ index.html (id панелей imTab*), а не строится в JS:
 *     так разметку видно в HTML, а модули не переписывают друг другу DOM;
 *   - идентификаторы существующих карточек сохранены (imgModelsBody, imgBundle*,
 *     imageProfiles, imageBackendsBody) — иначе пришлось бы переписывать рабочие
 *     модули и тесты, а это ровно тот риск «потерять функциональность», от
 *     которого страхует план Phase 9;
 *   - видимость пункта меню и страницы остаётся за image-backends-page.js
 *     (syncVisibility): страница нужна только когда в кластере есть image_cpp,
 *     и это правило уже оттестировано.
 *
 * ЧЕГО ЗДЕСЬ СОЗНАТЕЛЬНО НЕТ: генерации и показа картинок. WebUI — для настройки
 * и понимания состояния системы; показ сгенерированного — задача клиентов
 * (OpenAI/A1111 на :18079). «Проверка бэкенда» генерирует 1 шаг 64x64 и
 * показывает ТОЛЬКО результат и время, без изображения.
 *
 * Экспорт: window.ImageModelsPage.
 */
(function () {
    'use strict';

    var TABS = ['overview', 'hf', 'models', 'loaded', 'downloads', 'settings'];
    var PAGE_ID = 'image-page';
    var TABS_ID = 'imTabs';
    var STORAGE_KEY = 'ollamalegion_image_models_tab';
    var SELFTEST_RESULT_ID = 'imSelfTestResult';
    var LOADED_STATE_ID = 'imLoadedState';
    var BACKEND_SELECT_ID = 'imgBackendSelect';

    var mounted = false;
    var activeTab = 'overview';
    var state = { backends: [], selftestBusy: false };

    // ============================================================
    // Утилиты (локальные: модуль обязан работать в node-тесте без DOM/i18n)
    // ============================================================

    function byId(id) {
        return (typeof document !== 'undefined' && document.getElementById) ? document.getElementById(id) : null;
    }

    function t(key, fallback) {
        if (window.I18N && typeof window.I18N.t === 'function') {
            var v = window.I18N.t(key);
            if (v && v !== key) return v;
        }
        return fallback || key;
    }

    /** apiBase — тот же расчёт, что в остальных модулях страницы. */
    function apiBase() {
        var cfg = window.WEBUI_CONFIG || {};
        var base = cfg.API_BASE_URL || cfg.API_BASE ||
            (window.location && window.location.origin) || '';
        return String(base).replace(/\/+$/, '');
    }

    /** authHeaders — токен из Api.getAuthHeaders(), fallback на конфиг/localStorage. */
    function authHeaders(extra) {
        var headers = (window.Api && typeof window.Api.getAuthHeaders === 'function')
            ? window.Api.getAuthHeaders()
            : { 'Content-Type': 'application/json' };
        if (extra) {
            Object.keys(extra).forEach(function (k) { headers[k] = extra[k]; });
        }
        if (!headers['X-API-Token']) {
            var tk = (window.WEBUI_CONFIG && window.WEBUI_CONFIG.API_TOKEN) || '';
            if (!tk) {
                try { tk = localStorage.getItem('apiToken') || ''; } catch (e) { tk = ''; }
            }
            if (tk) headers['X-API-Token'] = tk;
        }
        return headers;
    }

    /** selectedBackendID — выбранный в шапке image-бэкенд (или единственный). */
    function selectedBackendID() {
        var sel = byId(BACKEND_SELECT_ID);
        if (sel && sel.value) return String(sel.value);
        var ids = state.backends.map(function (b) { return b.id; });
        return ids.length === 1 ? ids[0] : '';
    }

    // ============================================================
    // Табы
    // ============================================================

    function panelId(tab) {
        return 'imTab' + tab.charAt(0).toUpperCase() + tab.slice(1);
    }

    /**
     * showTab — показать таб: класс .active на кнопке и на панели.
     *
     * Панели используют те же классы, что страница «GGUF модели»
     * (.gguf-tab-content / .active из pages.css) — визуальный язык одинаков, новых
     * стилей заводить не нужно.
     */
    function showTab(tab) {
        if (TABS.indexOf(tab) === -1) tab = 'overview';
        activeTab = tab;
        TABS.forEach(function (id) {
            var btn = document.querySelector ? document.querySelector('[data-im-tab="' + id + '"]') : null;
            if (btn && btn.classList) btn.classList.toggle('active', id === tab);
            var panel = byId(panelId(id));
            if (panel && panel.classList) panel.classList.toggle('active', id === tab);
        });
        try { localStorage.setItem(STORAGE_KEY, tab); } catch (e) { /* приватный режим */ }
        // Табы-модули получают шанс догрузить свои данные при первом показе.
        renderTabs();
        return tab;
    }

    function restoreTab() {
        var saved = '';
        try { saved = localStorage.getItem(STORAGE_KEY) || ''; } catch (e) { saved = ''; }
        showTab(TABS.indexOf(saved) !== -1 ? saved : 'overview');
    }

    // ============================================================
    // Рендер
    // ============================================================

    /** ctx — контекст для табов-модулей (единый контракт Phase 9). */
    function context() {
        var backendId = selectedBackendID();
        var backend = null;
        state.backends.forEach(function (b) { if (b.id === backendId) backend = b; });
        return {
            apiBase: apiBase(),
            headers: authHeaders(),
            backendId: backendId,
            backend: backend,
            backends: state.backends,
            tab: activeTab
        };
    }

    /** renderTabs — отдать контекст подключённым табам-модулям (если они есть). */
    function renderTabs() {
        var ctx = context();
        [window.ImageModelsList, window.ImageModelsHf].forEach(function (mod) {
            if (!mod || typeof mod.render !== 'function') return;
            var owns = !mod.tabIds || mod.tabIds.indexOf(activeTab) !== -1;
            if (!owns) return;
            try { mod.render(ctx); } catch (e) {
                if (window.console) console.warn('[ImageModelsPage] tab render failed:', e);
            }
        });
    }

    /**
     * renderCurrentModel — таб «Загруженные»: состояние воркера и текущая модель.
     *
     * Данные берём из уже загруженного списка бэкендов (GET /api/v1/cluster):
     * отдельный запрос на каждый тик не нужен, а «Загруженные» — это сводка.
     */
    function renderCurrentModel() {
        var el = byId(LOADED_STATE_ID);
        if (!el) return;
        var backendId = selectedBackendID();
        var backend = null;
        state.backends.forEach(function (b) { if (b.id === backendId) backend = b; });
        if (!backend || !backend.image) {
            el.textContent = t('imageModels.no_backend_selected', 'Image-бэкенд не выбран');
            return;
        }
        var img = backend.image;
        var parts = [];
        parts.push(t('imageBackends.col_state', 'Состояние') + ': ' + (img.state || '—'));
        if (img.currentModel) parts.push(t('imageBackends.col_model', 'Текущая модель') + ': ' + img.currentModel);
        if (img.vramFreeMb || img.vramTotalMb) {
            parts.push('VRAM: ' + (img.vramFreeMb || 0) + ' / ' + (img.vramTotalMb || 0) + ' MB');
        }
        // Прогресс загрузки рисует image-page.js (renderLoadProgress) в блок
        // #imgLoadProgress — он переехал в этот таб вместе с разметкой.
        el.textContent = parts.join(' · ');
    }

    function render(backends) {
        state.backends = Array.isArray(backends) ? backends : [];
        renderCurrentModel();
        renderTabs();
        return state.backends;
    }

    /**
     * syncVisibility — показать/скрыть пункт меню и страницу.
     *
     * Решение о видимости принимает image-backends-page.js (страница нужна
     * только при наличии image_cpp); здесь — тонкая обёртка, чтобы контракт
     * Phase 9 (у шелла есть syncVisibility) соблюдался без второй реализации
     * одного и того же правила.
     */
    function syncVisibility(backends) {
        if (window.ImageBackendsPage && typeof window.ImageBackendsPage.syncVisibility === 'function') {
            return window.ImageBackendsPage.syncVisibility(backends);
        }
        var list = Array.isArray(backends) ? backends : [];
        var has = list.some(function (b) {
            var ty = b && (b.backendType || b.type || b.backend_type);
            return ty === 'image_cpp';
        });
        var nav = document.querySelector ? document.querySelector('a[data-page="image"]') : null;
        if (nav) nav.style.display = has ? '' : 'none';
        var page = byId(PAGE_ID);
        if (page) page.style.display = has ? '' : 'none';
        return has;
    }

    // ============================================================
    // Проверка бэкенда (без показа картинки)
    // ============================================================

    function setSelfTestText(text, isError) {
        var el = byId(SELFTEST_RESULT_ID);
        if (!el) return;
        el.textContent = text;
        el.style.color = isError ? 'var(--danger)' : 'var(--text-muted)';
    }

    /**
     * runSelfTest — «Проверка бэкенда»: 1 шаг 64x64 через тот же путь, что и
     * клиенты (нативный /api/image/generate, sync=true).
     *
     * ПОЧЕМУ sync И ПОЧЕМУ БЕЗ КАРТИНКИ: sync даёт ответ в одном запросе (не надо
     * поллить джобу), а base64-изображение мы СОЗНАТЕЛЬНО не рендерим — WebUI не
     * показывает сгенерированное, только факт «движок отвечает» и время. Иначе
     * это была бы витрина генерации, которую из WebUI как раз убрали.
     */
    function runSelfTest() {
        if (state.selftestBusy) return Promise.resolve(false);
        var backendId = selectedBackendID();
        if (!backendId) {
            setSelfTestText(t('imageModels.selftest_no_backend', 'Выберите image-бэкенд'), true);
            return Promise.resolve(false);
        }
        state.selftestBusy = true;
        setSelfTestText(t('imageModels.selftest_running', 'Проверяю...'), false);
        var started = Date.now();
        var body = {
            prompt: 'balancer self-test',
            width: 64,
            height: 64,
            steps: 1,
            batch: 1,
            sync: true
        };
        return fetch(apiBase() + '/api/v1/image/backends/' + encodeURIComponent(backendId) + '/generate', {
            method: 'POST',
            headers: authHeaders({ 'Content-Type': 'application/json' }),
            body: JSON.stringify(body)
        }).then(function (resp) {
            return resp.text().then(function (text) {
                var parsed = null;
                if (text) { try { parsed = JSON.parse(text); } catch (e) { parsed = null; } }
                if (!resp.ok) {
                    var msg = (parsed && (parsed.error || parsed.message)) || ('HTTP ' + resp.status);
                    throw new Error(msg);
                }
                return parsed;
            });
        }).then(function () {
            var ms = Date.now() - started;
            state.selftestBusy = false;
            setSelfTestText(t('imageModels.selftest_ok', 'OK') + ' — ' + ms + ' ' + t('imageModels.ms', 'мс'), false);
            return true;
        }).catch(function (err) {
            state.selftestBusy = false;
            setSelfTestText(t('imageModels.selftest_fail', 'Ошибка') + ': ' + (err && err.message ? err.message : err), true);
            return false;
        });
    }

    // ============================================================
    // Монтирование
    // ============================================================

    /** mount — идемпотентно: обработчики табов и кнопки проверки. */
    function mount() {
        if (mounted) { restoreTabIfNeeded(); return; }
        mounted = true;

        var tabs = byId(TABS_ID);
        if (tabs && tabs.addEventListener) {
            // Делегирование: кнопки табов могут перерисоваться, слушатель — один.
            tabs.addEventListener('click', function (ev) {
                var el = ev && ev.target;
                while (el && el !== tabs) {
                    if (el.getAttribute && el.getAttribute('data-im-tab')) {
                        showTab(el.getAttribute('data-im-tab'));
                        return;
                    }
                    el = el.parentNode;
                }
            });
        }

        var btn = byId('imSelfTestBtn');
        if (btn && btn.addEventListener) btn.addEventListener('click', function () { runSelfTest(); });

        restoreTabIfNeeded();
    }

    function restoreTabIfNeeded() {
        // Восстанавливаем последний таб, но не перебиваем явный выбор пользователя
        // в текущей сессии (activeTab уже мог быть изменён).
        if (activeTab === 'overview') restoreTab();
    }

    window.ImageModelsPage = {
        mount: mount,
        render: render,
        syncVisibility: syncVisibility,
        showTab: showTab,
        runSelfTest: runSelfTest,
        tabIds: TABS,
        _state: state
    };
})();
