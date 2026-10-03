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
    // Потолок ожидания проверки: 1 шаг 64x64 укладывается в секунды даже с
    // холодной загрузкой модели, но кнопка не должна висеть вечно.
    var SELFTEST_TIMEOUT_MS = 180000;
    // Ожидание загрузки модели внутри проверки (гейт пускает генерацию только
    // с загруженной моделью) и интервал опроса её состояния.
    var SELFTEST_LOAD_TIMEOUT_MS = 180000;
    var SELFTEST_LOAD_POLL_MS = 1500;
    // Окно повторов на транзиентные отказы гейта: кэш состояния бэкенда на
    // балансере обновляется опросом, поэтому «модель ещё не загружена» может
    // прийти на несколько секунд позже, чем воркер реально её загрузил.
    var SELFTEST_GATE_WAIT_MS = 30000;
    var LOADED_STATE_ID = 'imLoadedState';
    var CLIENT_ACCESS_HOST_ID = 'imClientAccessHost';
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

    /**
     * renderClientAccess — таб «Обзор»: блок «Подключение клиентов» для
     * ВЫБРАННОГО image-бэкенда (ключ + эндпоинты картинок).
     *
     * Зачем здесь: оператор настраивает клиента, глядя на конкретный бэкенд, а
     * ключ лежал только в deployments/.env на хосте. Модуль тот же, что на
     * странице «Бэкенды» (`client-access.js`) — одна реализация на проект.
     */
    function renderClientAccess() {
        var host = byId(CLIENT_ACCESS_HOST_ID);
        if (!host) return;
        if (!window.ClientAccess || typeof window.ClientAccess.render !== 'function') {
            host.innerHTML = '';
            return;
        }
        var backendId = selectedBackendID();
        var backend = null;
        state.backends.forEach(function (b) { if (b.id === backendId) backend = b; });
        if (!backend) { host.innerHTML = ''; return; }
        host.innerHTML = window.ClientAccess.render(backend);
    }

    function render(backends) {
        state.backends = Array.isArray(backends) ? backends : [];
        renderCurrentModel();
        renderClientAccess();
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
     * getJson — GET + разбор ответа с текстом ошибки сервера.
     *
     * Читаем через text()+JSON.parse (а не r.json()), потому что воркер/балансер
     * всегда отдают JSON-конверт, а ошибку надо показать оператору текстом:
     * «HTTP 500» без тела не отличить от «моделей нет».
     */
    function getJson(url, headers) {
        return fetch(url, { headers: headers }).then(function (r) {
            return r.text().then(function (text) {
                var parsed = null;
                if (text) { try { parsed = JSON.parse(text); } catch (e) { parsed = null; } }
                if (!r.ok) {
                    var msg = (parsed && (parsed.error || parsed.message)) || ('HTTP ' + r.status);
                    var err = new Error(msg);
                    err.status = r.status;
                    err.body = parsed;
                    throw err;
                }
                return parsed;
            });
        });
    }

    /**
     * ensureModelLoaded — VRAM-гейт балансера пускает генерацию только когда
     * модель УЖЕ загружена (иначе 503 с подсказкой «no image model is loaded»),
     * поэтому проверка сначала грузит модель выбранного бэкенда, если её нет.
     *
     * ПОЧЕМУ ЭТО ПРАВИЛЬНО ДЛЯ «ПРОВЕРКИ»: оператор жмёт кнопку, чтобы понять
     * «заработает ли генерация». Загрузка модели — часть этого пути (её делает
     * и клиент, и оператор), поэтому проверка честно проходит оба шага:
     * загрузку (управляющий путь, разрешён без модели) и генерацию (гейт +
     * счётчики). Молча вернуть «Ошибка: модель не загружена» значило бы
     * показывать политику гейта как поломку стенда.
     *
     * @returns {Promise<string>} имя загруженной модели ('' если не удалось).
     */
    function ensureModelLoaded(backendId, backend) {
        var img = (backend && backend.image) || {};
        var state0 = String(img.state || '');
        if (img.currentModel && (state0 === 'loaded' || state0 === 'ready')) {
            return Promise.resolve(String(img.currentModel));
        }
        setSelfTestText(t('imageModels.selftest_loading_model', 'Загружаю модель...'), false);
        var headers = authHeaders({ 'Content-Type': 'application/json' });
        var base = apiBase() + '/api/v1/image/backends/' + encodeURIComponent(backendId);
        return getJson(base + '/models', headers).then(function (data) {
            var list = (data && (data.models || data)) || [];
            if (!Array.isArray(list) || !list.length) {
                throw new Error(t('imageModels.selftest_no_models', 'у бэкенда нет моделей на диске'));
            }
            var name = String(list[0].name || list[0].id || '');
            if (!name) throw new Error(t('imageModels.selftest_no_models', 'у бэкенда нет моделей на диске'));
            return fetch(base + '/models/load', {
                method: 'POST', headers: headers, body: JSON.stringify({ name: name })
            }).then(function (r) {
                return r.text().then(function (text) {
                    if (!r.ok) {
                        var parsed = null;
                        if (text) { try { parsed = JSON.parse(text); } catch (e) { parsed = null; } }
                        var msg = (parsed && (parsed.error || parsed.message)) || ('HTTP ' + r.status);
                        var err = new Error(msg);
                        err.body = parsed;
                        err.status = r.status;
                        throw err;
                    }
                    return name;
                });
            });
        }).then(function (name) {
            // Ждём state=loaded: генерацию гейт не пустит раньше.
            var deadline = Date.now() + SELFTEST_LOAD_TIMEOUT_MS;
            var poll = function () {
                return getJson(base + '/models', headers).then(function (data) {
                    var st = String((data && (data.state || (data.progress && data.progress.state))) || '');
                    var cur = String((data && (data.current_model || data.currentModel || (data.progress && data.progress.model))) || '');
                    if (st === 'loaded' || st === 'ready') return cur || name;
                    if (st === 'error' || st === 'failed') {
                        throw new Error(t('imageModels.selftest_load_failed', 'модель не загрузилась: {error}',
                            { error: String((data.progress && data.progress.error) || '') }));
                    }
                    if (Date.now() > deadline) {
                        throw new Error(t('imageModels.selftest_load_timeout', 'модель не загрузилась за {s} с',
                            { s: Math.round(SELFTEST_LOAD_TIMEOUT_MS / 1000) }));
                    }
                    return new Promise(function (res) { setTimeout(res, SELFTEST_LOAD_POLL_MS); }).then(poll);
                });
            };
            return poll();
        });
    }

    /**
     * runSelfTest — «Проверка бэкенда»: 1 шаг 64x64 через ТОТ ЖЕ путь, что у
     * клиентов — POST /v1/images/generations (OpenAI-поверхность балансера).
     *
     * ПОЧЕМУ ИМЕННО ЭТОТ ПУТЬ:
     *   - он считается метриками и проходит VRAM-гейт (internal/balancer/
     *     image_router.go: счёт ведётся по /v1/images/*, /sdapi/v1/*,
     *     /api/image/generate), поэтому собственная проверка оператора видна в
     *     Monitor и в /api/v1/metrics — а не только в логах воркера;
     *   - он же и клиентский: ровно сюда ходят SillyTavern/Open WebUI/n8n, так
     *     что проверка отвечает на вопрос «мой клиент заработает?»;
     *   - управляющий алиас /api/v1/image/backends/{id}/generate для этого не
     *     годится: он идёт мимо гейта и мимо счётчиков (следствие — проверка
     *     была не видна в Monitor, хотя реально занимала GPU).
     *
     * ПОЧЕМУ БЕЗ КАРТИНКИ: ответ приходит с base64 в data[0].b64_json, и мы его
     * СОЗНАТЕЛЬНО не рендерим — WebUI показывает только «движок отвечает» и
     * время. Иначе это была бы витрина генерации, которую из WebUI как раз убрали
     * (это же проверяется тестом: base64 не попадает в DOM).
     */
    /**
     * generateOnce — один клиентский запрос генерации 1 шага 64x64.
     * @returns {Promise<{parsed:object}>} либо бросает Error с .status/.body.
     */
    function generateOnce() {
        var body = {
            prompt: 'balancer self-test',
            size: '64x64',
            n: 1,
            steps: 1
        };
        var ctrl = (typeof AbortController !== 'undefined') ? new AbortController() : null;
        var timer = null;
        if (ctrl) timer = setTimeout(function () { ctrl.abort(); }, SELFTEST_TIMEOUT_MS);
        var init = {
            method: 'POST',
            headers: authHeaders({ 'Content-Type': 'application/json' }),
            body: JSON.stringify(body)
        };
        if (ctrl) init.signal = ctrl.signal;
        return fetch(apiBase() + '/v1/images/generations', init).then(function (resp) {
            if (timer) clearTimeout(timer);
            return resp.text().then(function (text) {
                var parsed = null;
                if (text) { try { parsed = JSON.parse(text); } catch (e) { parsed = null; } }
                if (!resp.ok) {
                    var err = new Error((parsed && (parsed.error || parsed.message)) || ('HTTP ' + resp.status));
                    err.body = parsed;
                    err.status = resp.status;
                    throw err;
                }
                return parsed;
            });
        }).catch(function (err) {
            if (timer) clearTimeout(timer);
            throw err;
        });
    }

    /**
     * isTransientGateReject — отказ гейта «подожди и повтори».
     *
     * ЗАЧЕМ ПОВТОР: гейт балансера судит по СВОЕМУ кэшу состояния бэкенда
     * (internal/balancer/image_resources.go: checkImageModelReady смотрит
     * s.loaded()/s.state), который обновляется периодическим опросом воркера.
     * Сразу после загрузки модели воркер уже отдаёт «loaded», а кэш балансера
     * ещё секунды живёт со старым «not_loaded» — и проверка получала 503
     * «no image model is loaded», хотя модель загружена. Это транзиентное
     * состояние, а не поломка: ждём Retry-After и повторяем (так же поступает
     * любой нормальный клиент).
     */
    function isTransientGateReject(err) {
        if (!err || err.status !== 503) return false;
        var b = err.body || {};
        var code = String(b.error || b.code || (b.error && b.error.code) || '');
        var text = String(b.message || b.hint || (b.error && (b.error.message || b.error)) || err.message || '');
        if (/model_not_loaded|model_loading|gpu_busy|image_gate/i.test(code)) return true;
        return /still loading|no image model is loaded|using the GPU/i.test(text);
    }

    /** Пауза: Retry-After от гейта (кап 5 с), иначе 2 с. */
    function gateRetryDelayMS(err) {
        var b = (err && err.body) || {};
        var sec = Number(b.retry_after_seconds || b.retryAfterSeconds || 0);
        if (!sec || isNaN(sec) || sec <= 0) sec = 2;
        if (sec > 5) sec = 5;
        return Math.round(sec * 1000);
    }

    /**
     * generateWithGateRetry — генерация с ограниченным числом повторов на
     * транзиентные отказы гейта (окно SELFTEST_GATE_WAIT_MS).
     */
    function generateWithGateRetry(onWait) {
        var deadline = Date.now() + SELFTEST_GATE_WAIT_MS;
        var attempt = function () {
            return generateOnce().catch(function (err) {
                if (!isTransientGateReject(err) || Date.now() > deadline) throw err;
                if (onWait) onWait();
                return new Promise(function (res) { setTimeout(res, gateRetryDelayMS(err)); }).then(attempt);
            });
        };
        return attempt();
    }

    function runSelfTest() {
        if (state.selftestBusy) return Promise.resolve(false);
        if (!state.backends.length) {
            setSelfTestText(t('imageModels.selftest_no_backend', 'Нет image-бэкендов'), true);
            return Promise.resolve(false);
        }
        state.selftestBusy = true;
        var backendId = selectedBackendID() || (state.backends[0] && state.backends[0].id) || '';
        var backend = null;
        state.backends.forEach(function (b) { if (b.id === backendId) backend = b; });
        var started = Date.now();
        var modelName = '';
        setSelfTestText(t('imageModels.selftest_running', 'Проверяю...'), false);
        return ensureModelLoaded(backendId, backend).then(function (name) {
            modelName = String(name || '');
            setSelfTestText(t('imageModels.selftest_running', 'Проверяю...'), false);
            return generateWithGateRetry(function () {
                setSelfTestText(t('imageModels.selftest_wait_gate', 'Жду готовности бэкенда...'), false);
            });
        }).then(function (parsed) {
            var ms = Date.now() - started;
            state.selftestBusy = false;
            var model = modelName || ((parsed && parsed.model) ? String(parsed.model) : '');
            setSelfTestText(t('imageModels.selftest_ok', 'OK') + ' — ' + ms + ' ' + t('imageModels.ms', 'мс') +
                (model ? ' · ' + model : ''), false);
            return true;
        }).catch(function (err) {
            state.selftestBusy = false;
            setSelfTestError(err);
            return false;
        });
    }

    /** Текст ошибки проверки: hint гейта информативнее кода. */
    function setSelfTestError(err) {
        var msg;
        if (err && err.name === 'AbortError') {
            msg = t('imageModels.selftest_timeout', 'нет ответа за {s} с', { s: Math.round(SELFTEST_TIMEOUT_MS / 1000) });
        } else {
            var b = err && err.body;
            msg = (b && (b.hint || b.message)) ||
                (b && b.error && (b.error.message || b.error)) ||
                ((err && err.message) || String(err));
        }
        setSelfTestText(t('imageModels.selftest_fail', 'Ошибка') + ': ' + msg, true);
    }

    // ============================================================
    // Монтирование
    // ============================================================

    /** mount — идемпотентно: обработчики табов и кнопки проверки. */
    function mount() {
        if (mounted) { restoreTabIfNeeded(); return; }
        mounted = true;

        // Делегированный обработчик «показать/скопировать ключ» — слушатель на
        // документе, поэтому монтируем один раз (идемпотентно внутри модуля).
        if (window.ClientAccess && typeof window.ClientAccess.mount === 'function') {
            window.ClientAccess.mount();
        }

        // Смена выбранного бэкенда в шапке — сразу перерисовать «Подключение
        // клиентов» и сводку загруженной модели (не ждать периодического refresh).
        var backendSel = byId(BACKEND_SELECT_ID);
        if (backendSel && backendSel.addEventListener) {
            backendSel.addEventListener('change', function () {
                renderClientAccess();
                renderCurrentModel();
            });
        }

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
