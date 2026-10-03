/**
 * image-test-page.js — «Image-тест»: страница-инструмент для проверки настроек
 * модели генерации изображений (2026-10-03).
 *
 * ЗАЧЕМ ОТДЕЛЬНАЯ СТРАНИЦА. Страница «Image-модели» — про настройку и состояние
 * (это требование заказчика: в ней нет показа картинок). Но оператору нужно
 * ВРЕМЯ ОТ ВРЕМЕНИ проверить, как модель ведёт себя с текущими настройками
 * (steps/cfg/sampler/размер, профиль bundle), и увидеть результат. Поэтому показ
 * сгенерированного вынесен в отдельную страницу, которая:
 *   - видна только когда в кластере есть бэкенд типа image_cpp (та же логика, что
 *     у страницы «Image-модели»);
 *   - шлёт запрос КЛИЕНТСКИМ путём балансера (OpenAI `/v1/images/generations` или
 *     A1111 `/sdapi/v1/txt2img`) — то есть ровно так, как это сделает клиент:
 *     проходит VRAM-гейт, попадает в метрики и видно в Monitor;
 *   - показывает сам запрос (JSON) и ответ движка как есть (model/seed/
 *     parameters/info), чтобы «настройки применились или нет» было проверяемо,
 *     а не угадывалось по картинке.
 *
 * ЧЕГО ЗДЕСЬ НЕТ: localStorage-истории и галереи. История теста живёт в памяти
 * вкладки (до перезагрузки) и нужна только чтобы сравнить два прогона настроек;
 * постоянное хранилище картинок в WebUI мы сознательно не заводим.
 *
 * Экспорт: window.ImageTestPage.
 */
(function () {
    'use strict';

    var PAGE_ID = 'image-test-page';
    var NAV_SELECTOR = 'a[data-page="image-test"]';

    // Лимиты — те же, что у движка (совпадают с /api/v1/image/contract.limits).
    var LIMITS = { minSide: 64, maxSide: 4096, step: 64, minSteps: 1, maxSteps: 100, minCfg: 0, maxCfg: 30, minBatch: 1, maxBatch: 8 };
    var STATIC_SAMPLERS = ['euler', 'euler_a', 'dpm++2m', 'dpm++2s_a', 'lcm', 'ddim_trailing'];
    var STATIC_SCHEDULERS = ['discrete', 'karras', 'exponential', 'ays', 'gits', 'smoothstep'];

    var TIMEOUT_CONTROL_MS = 15000;
    var TIMEOUT_GENERATE_MS = 600000;
    // Сколько ждём результат загрузки модели: движок поднимается в фоне (воркер
    // отвечает 202 сразу), а падение видно только в прогрессе.
    var LOAD_WAIT_MS = 180000;
    var HISTORY_MAX = 6;

    var mounted = false;
    var state = {
        backends: [],
        backendId: '',
        models: [],
        caps: null,
        generated: null,   // { images: [...], meta: {...} }
        error: '',
        busy: false,
        history: [],       // [{ts, params, images, meta, surface}]
        notice: ''
    };

    // =====================================================================
    // Pure helpers (тесты: webui/js/modules/image-test-page.test.js)
    // =====================================================================

    function escapeHtml(text) {
        if (text === null || text === undefined) return '';
        return String(text)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }

    function clampInt(value, min, max, fallback) {
        var v = parseInt(value, 10);
        if (isNaN(v)) return fallback;
        if (v < min) return min;
        if (v > max) return max;
        return v;
    }

    /** Размер — кратный 64 и в пределах лимитов: движок иначе отвергнет запрос. */
    function snapDimension(value, limits) {
        var lim = limits || LIMITS;
        var v = parseInt(value, 10);
        if (isNaN(v)) v = 512;
        var step = lim.step || 64;
        v = Math.round(v / step) * step;
        if (v < lim.minSide) v = lim.minSide;
        if (v > lim.maxSide) v = lim.maxSide;
        return v;
    }

    /** Форма → тело запроса клиентской поверхности (A1111-совместимое). */
    function buildPayload(form, limits) {
        form = form || {};
        var lim = limits || LIMITS;
        var payload = {
            prompt: String(form.prompt || '').trim(),
            width: snapDimension(form.width, lim),
            height: snapDimension(form.height, lim),
            steps: clampInt(form.steps, lim.minSteps, lim.maxSteps, 20),
            batch_size: clampInt(form.batch, lim.minBatch, lim.maxBatch, 1)
        };
        var neg = String(form.negative || '').trim();
        if (neg) payload.negative_prompt = neg;
        var cfgRaw = String(form.cfg === null || form.cfg === undefined ? '' : form.cfg).trim();
        if (cfgRaw !== '') {
            var cfg = parseFloat(cfgRaw);
            if (!isNaN(cfg)) payload.cfg_scale = Math.min(lim.maxCfg, Math.max(lim.minCfg, cfg));
        }
        var seedRaw = String(form.seed === null || form.seed === undefined ? '' : form.seed).trim();
        var seed = seedRaw === '' ? -1 : parseInt(seedRaw, 10);
        payload.seed = isNaN(seed) ? -1 : seed;
        if (form.sampler) payload.sampler_name = String(form.sampler);
        if (form.scheduler) payload.scheduler = String(form.scheduler);
        return payload;
    }

    /** Настройки профиля модели → форма теста (чтобы проверять именно их). */
    function applyModelDefaults(form, model) {
        var out = {
            prompt: form.prompt || '',
            negative: form.negative || (model && model.defaults && model.defaults.negativePrompt) || '',
            width: (model && model.defaults && model.defaults.width) || form.width || 512,
            height: (model && model.defaults && model.defaults.height) || form.height || 512,
            steps: (model && model.defaults && model.defaults.steps) || form.steps || 20,
            cfg: (model && model.defaults && model.defaults.cfgScale) || form.cfg || 7,
            sampler: (model && model.defaults && model.defaults.sampler) || form.sampler || 'euler_a',
            scheduler: (model && model.defaults && model.defaults.scheduler) || form.scheduler || 'discrete',
            seed: form.seed === undefined ? '' : form.seed,
            batch: form.batch || 1
        };
        return out;
    }

    /** Путь клиентской поверхности балансера + её «форма» тела. */
    function surfaceInfo(surface, apiBase, host, openaiPort, proxyPort) {
        var base = apiBase || '';
        var externalHost = host || 'localhost';
        if (surface === 'a1111') {
            return {
                id: 'a1111',
                label: 'A1111 /sdapi/v1/txt2img',
                url: base + '/sdapi/v1/txt2img',
                externalUrl: 'http://' + externalHost + ':' + openaiPort + '/sdapi/v1/txt2img',
                bodyStyle: 'a1111'
            };
        }
        if (surface === 'native') {
            return {
                id: 'native',
                label: 'Нативный /api/image/generate',
                url: base + '/api/image/generate',
                externalUrl: 'http://' + externalHost + ':' + proxyPort + '/api/image/generate',
                bodyStyle: 'native'
            };
        }
        return {
            id: 'openai',
            label: 'OpenAI /v1/images/generations',
            url: base + '/v1/images/generations',
            externalUrl: 'http://' + externalHost + ':' + openaiPort + '/v1/images/generations',
            bodyStyle: 'openai'
        };
    }

    /**
     * Тело запроса под выбранную поверхность.
     *
     * OpenAI-поверхность принимает `size: "WxH"` и `n`, A1111 — `width/height` и
     * `batch_size`, нативный путь — `width/height` + `sync`. Показываем оператору
     * ровно то, что уйдёт в сеть: иначе «почему размер не тот» не проверить.
     */
    function payloadForSurface(payload, surface) {
        if (surface === 'openai') {
            return {
                prompt: payload.prompt,
                size: payload.width + 'x' + payload.height,
                n: payload.batch_size,
                steps: payload.steps,
                cfg_scale: payload.cfg_scale,
                negative_prompt: payload.negative_prompt,
                seed: payload.seed,
                sampler_name: payload.sampler_name,
                scheduler: payload.scheduler
            };
        }
        if (surface === 'native') {
            return {
                prompt: payload.prompt,
                negative_prompt: payload.negative_prompt,
                width: payload.width,
                height: payload.height,
                steps: payload.steps,
                cfg: payload.cfg_scale,
                seed: payload.seed,
                batch: payload.batch_size,
                sampler: payload.sampler_name,
                scheduler: payload.scheduler,
                sync: true
            };
        }
        return payload;
    }

    /** curl-пример для копирования (клиентский путь, с ключом). */
    function curlFor(surfaceInfoObj, payload, key) {
        var body = JSON.stringify(payload, null, 2);
        return 'curl -sS -X POST ' + surfaceInfoObj.externalUrl + ' \\\n' +
            "  -H 'Content-Type: application/json' \\\n" +
            "  -H 'Authorization: Bearer " + (key || '<ключ>') + "' \\\n" +
            "  -d '" + body.replace(/\n/g, '\n     ') + "'";
    }

    /** base64 → data URL (для <img src>). */
    function b64ToDataUrl(b64, format) {
        if (!b64 || typeof b64 !== 'string') return '';
        var s = b64.trim().replace(/\s+/g, '');
        if (!s) return '';
        if (s.indexOf('data:') === 0) return s;
        var mime = String(format || 'png').toLowerCase();
        if (mime === 'jpg') mime = 'jpeg';
        return 'data:image/' + mime + ';base64,' + s;
    }

    /** Картинки из ответа: OpenAI {data:[{b64_json}]}, A1111 {images:[...]}, нативный {images:[...]}. */
    function extractImages(resp) {
        var out = [];
        function push(item) {
            if (!item) return;
            if (typeof item === 'string') { out.push({ b64: item, format: 'png' }); return; }
            if (item.b64_json || item.b64) {
                out.push({ b64: item.b64_json || item.b64, format: item.output_format || item.format || 'png' });
                return;
            }
            if (item.url) out.push({ url: item.url, format: item.output_format || item.format || 'png' });
        }
        if (!resp) return out;
        if (Array.isArray(resp)) resp.forEach(push);
        else if (Array.isArray(resp.data)) resp.data.forEach(push);
        else if (Array.isArray(resp.images)) resp.images.forEach(push);
        else if (resp.data) push(resp.data);
        return out;
    }

    /** `/api/image/capabilities` → samplers/schedulers/limits с дефолтами. */
    function normalizeCapabilities(data) {
        var src = data || {};
        var raw = src.limits || {};
        var limits = {
            minSide: Number(raw.min_width || raw.minWidth || LIMITS.minSide),
            maxSide: Number(raw.max_width || raw.maxWidth || LIMITS.maxSide),
            maxHeight: Number(raw.max_height || raw.maxHeight || LIMITS.maxSide),
            step: Number(raw.size_multiple || raw.sizeStep || LIMITS.step),
            minSteps: Number(raw.min_steps || raw.minSteps || LIMITS.minSteps),
            maxSteps: Number(raw.max_steps || raw.maxSteps || LIMITS.maxSteps),
            minCfg: LIMITS.minCfg,
            maxCfg: Number(raw.max_cfg_scale || raw.maxCfgScale || LIMITS.maxCfg),
            minBatch: LIMITS.minBatch,
            maxBatch: Number(raw.max_batch_count || raw.maxBatchCount || LIMITS.maxBatch)
        };
        if (!limits.maxHeight) limits.maxHeight = limits.maxSide;
        return {
            samplers: (Array.isArray(src.samplers) && src.samplers.length) ? src.samplers.map(String) : STATIC_SAMPLERS.slice(),
            schedulers: (Array.isArray(src.schedulers) && src.schedulers.length) ? src.schedulers.map(String) : STATIC_SCHEDULERS.slice(),
            limits: limits
        };
    }

    /**
     * errorHint — человеческая подсказка к известным ошибкам движка.
     *
     * ЗАЧЕМ: движок отдаёт сырой stderr («get sd version from file failed»), и
     * оператор видит «не загрузилось» без понимания, что делать. Разбираем
     * несколько типовых случаев, которые уже ловили живьём.
     */
    function errorHint(text) {
        var s = String(text || '');
        if (!s) return '';
        if (/get sd version from file failed/i.test(s)) {
            return 'Файл модели не распознан: похоже, GGUF собран для ComfyUI (другие имена тензоров/метаданные), а sd.cpp его не читает. Возьмите сборку под stable-diffusion.cpp (например leejet/*, QuantStack/*).';
        }
        if (/is not supported|unsupported (model|architecture)/i.test(s)) {
            return 'Архитектура модели не поддерживается этой версией sd.cpp. Проверьте, что файл собран под sd.cpp, либо обновите движок в образе воркера.';
        }
        if (/no image model is loaded/i.test(s)) {
            return 'Модель не загружена: нажмите «Загрузить модель» (VRAM-гейт не пропускает генерацию без загруженной модели).';
        }
        if (/out of memory|failed to allocate|oom/i.test(s)) {
            return 'Не хватило VRAM: уменьшите размер/шаги/batch, включите тайлинг VAE или offload в профиле модели.';
        }
        if (/still busy|address already in use/i.test(s)) {
            return 'Порт движка ещё занят прошлым процессом: подождите (CUDA отпускает сокет не сразу) и повторите загрузку.';
        }
        return '';
    }

    // =====================================================================
    // Окружение
    // =====================================================================

    function byId(id) {
        return (typeof document !== 'undefined' && document.getElementById) ? document.getElementById(id) : null;
    }

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

    function apiBase() {
        var cfg = window.WEBUI_CONFIG || {};
        var base = cfg.API_BASE_URL || cfg.API_BASE ||
            (window.location && window.location.origin) || '';
        return String(base).replace(/\/+$/, '');
    }

    function authHeaders(extra) {
        var headers = (window.Api && typeof window.Api.getAuthHeaders === 'function')
            ? window.Api.getAuthHeaders()
            : { 'Content-Type': 'application/json' };
        if (!headers['X-API-Token']) {
            var tk = (window.WEBUI_CONFIG && window.WEBUI_CONFIG.API_TOKEN) || '';
            if (!tk) { try { tk = localStorage.getItem('apiToken') || ''; } catch (e) { tk = ''; } }
            if (tk) headers['X-API-Token'] = tk;
        }
        if (extra) Object.keys(extra).forEach(function (k) { headers[k] = extra[k]; });
        return headers;
    }

    function clientKey() {
        var cfg = window.WEBUI_CONFIG || {};
        var key = cfg.API_TOKEN || '';
        if (!key) { try { key = localStorage.getItem('apiToken') || ''; } catch (e) { key = ''; } }
        return String(key);
    }

    function request(url, opts) {
        opts = opts || {};
        var timeoutMs = opts.timeoutMs || TIMEOUT_CONTROL_MS;
        var ctrl = (typeof AbortController !== 'undefined') ? new AbortController() : null;
        var timer = null;
        if (ctrl) timer = setTimeout(function () { ctrl.abort(); }, timeoutMs);
        var init = { method: opts.method || 'GET', headers: authHeaders(opts.headers) };
        if (ctrl) init.signal = ctrl.signal;
        if (opts.body !== undefined) init.body = opts.body;
        return fetch(url, init).then(function (resp) {
            if (timer) clearTimeout(timer);
            return resp.text().then(function (text) {
                var parsed = null;
                if (text) { try { parsed = JSON.parse(text); } catch (e) { parsed = text; } }
                if (!resp.ok) {
                    var msg = (parsed && typeof parsed === 'object' && (parsed.error || parsed.message)) || ('HTTP ' + resp.status);
                    if (parsed && typeof parsed === 'object' && parsed.error && typeof parsed.error === 'object') {
                        msg = parsed.error.message || parsed.error.code || msg;
                    }
                    var err = new Error(String(msg));
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

    function toast(msg, kind) {
        if (window.showToast) { window.showToast(msg, kind || 'info'); return; }
        if (window.Toast && window.Toast.show) { window.Toast.show({ message: msg, type: kind || 'info' }); }
    }

    // =====================================================================
    // Состояние и выбор бэкенда/модели
    // =====================================================================

    function hasImageBackends(backends) {
        if (window.ImageBackendsPage && typeof window.ImageBackendsPage.pure === 'object' &&
            typeof window.ImageBackendsPage.pure.hasImageBackends === 'function') {
            return window.ImageBackendsPage.pure.hasImageBackends(backends);
        }
        return (backends || []).some(function (b) {
            var ty = String(b && (b.backendType || b.type || b.backend_type) || '').toLowerCase();
            return ty === 'image_cpp';
        });
    }

    /**
     * Показ/скрытие пункта меню и страницы.
     *
     * Правило одно на проект и живёт в image-backends-page.js (там же, где
     * видимость «Image-моделей»): обе страницы существуют только при наличии
     * image_cpp-бэкенда. Здесь — тонкая обёртка, чтобы контракт соблюдался без
     * второй реализации того же условия.
     */
    function syncVisibility(backends) {
        if (window.ImageBackendsPage && typeof window.ImageBackendsPage.syncVisibility === 'function') {
            return window.ImageBackendsPage.syncVisibility(backends);
        }
        var visible = hasImageBackends(backends);
        var nav = document.querySelector ? document.querySelector(NAV_SELECTOR) : null;
        if (nav) nav.style.display = visible ? '' : 'none';
        var page = byId(PAGE_ID);
        if (page) page.style.display = visible ? '' : 'none';
        if (!visible) leavePageIfActive(nav);
        return visible;
    }

    function leavePageIfActive(nav) {
        var active = false;
        try { active = !!(nav && nav.classList && nav.classList.contains('active')); } catch (e) { active = false; }
        if (!active) return;
        var ctx = window.App && window.App.context;
        if (ctx && typeof ctx.switchPage === 'function') { ctx.switchPage('backends'); return; }
        var backendsNav = document.querySelector ? document.querySelector('[data-page="backends"]') : null;
        if (backendsNav && typeof backendsNav.click === 'function') backendsNav.click();
    }

    function imageBackends(list) {
        return (list || []).filter(function (b) {
            var ty = String(b && (b.backendType || b.type || b.backend_type) || '').toLowerCase();
            return ty === 'image_cpp';
        });
    }

    function selectedModel() {
        var name = byId('imgTestModelSelect') ? byId('imgTestModelSelect').value : '';
        var found = null;
        state.models.forEach(function (m) { if (m.name === name) found = m; });
        return found;
    }

    function selectedBackend() {
        var id = byId('imgTestBackendSelect') ? byId('imgTestBackendSelect').value : state.backendId;
        var found = null;
        state.backends.forEach(function (b) { if (b.id === id) found = b; });
        return found;
    }

    // =====================================================================
    // Рендер
    // =====================================================================

    function renderBackendSelect() {
        var sel = byId('imgTestBackendSelect');
        if (!sel) return;
        var list = imageBackends(state.backends);
        sel.innerHTML = list.map(function (b) {
            var label = b.name || b.id;
            if (b.host) label += ' (' + b.host + ')';
            return '<option value="' + escapeHtml(b.id) + '"' + (b.id === state.backendId ? ' selected' : '') + '>' +
                escapeHtml(label) + '</option>';
        }).join('');
        if (!state.backendId && list.length) state.backendId = list[0].id;
    }

    function renderModelSelect() {
        var sel = byId('imgTestModelSelect');
        if (!sel) return;
        if (!state.models.length) {
            sel.innerHTML = '<option value="">' + escapeHtml(t('imageTest.no_models', 'нет моделей на диске')) + '</option>';
            return;
        }
        var current = sel.value;
        sel.innerHTML = state.models.map(function (m) {
            var suffix = m.state && m.state !== 'not_loaded' ? ' [' + m.state + ']' : '';
            return '<option value="' + escapeHtml(m.name) + '">' + escapeHtml(m.name + suffix) + '</option>';
        }).join('');
        if (current && state.models.some(function (m) { return m.name === current; })) sel.value = current;
    }

    function renderCaps() {
        var selS = byId('imgTestSampler');
        var selSch = byId('imgTestScheduler');
        var caps = state.caps || normalizeCapabilities(null);
        if (selS) {
            var prevS = selS.value;
            selS.innerHTML = caps.samplers.map(function (s) {
                return '<option value="' + escapeHtml(s) + '">' + escapeHtml(s) + '</option>';
            }).join('');
            selS.value = caps.samplers.indexOf(prevS) !== -1 ? prevS : (caps.samplers[0] || 'euler_a');
        }
        if (selSch) {
            var prevSch = selSch.value;
            selSch.innerHTML = caps.schedulers.map(function (s) {
                return '<option value="' + escapeHtml(s) + '">' + escapeHtml(s) + '</option>';
            }).join('');
            selSch.value = caps.schedulers.indexOf(prevSch) !== -1 ? prevSch : (caps.schedulers[0] || 'discrete');
        }
        var note = byId('imgTestCapsNote');
        if (note) {
            var l = caps.limits;
            note.textContent = t('imageTest.limits_note',
                'Лимиты движка: стороны {min}..{max} шагом {step}, шаги {minSteps}..{maxSteps}, batch до {maxBatch}. Источник: {src}.',
                { min: l.minSide, max: l.maxSide, step: l.step, minSteps: l.minSteps, maxSteps: l.maxSteps, maxBatch: l.maxBatch,
                  src: state.caps ? t('imageTest.caps_engine', 'возможности воркера') : t('imageTest.caps_static', 'значения по умолчанию') });
        }
    }

    function renderModelState() {
        var el = byId('imgTestModelState');
        if (!el) return;
        var m = selectedModel();
        if (!m) { el.textContent = '—'; return; }
        var parts = [String(m.state || 'not_loaded')];
        if (m.family) parts.push(m.family);
        if (m.active_queries) parts.push('q:' + m.active_queries);
        el.textContent = parts.join(' · ');
    }

    function notice(text, isError) {
        var el = byId('imgTestNotice');
        if (!el) return;
        if (!text) { el.style.display = 'none'; el.innerHTML = ''; return; }
        el.style.display = '';
        el.style.color = isError ? 'var(--danger)' : 'var(--text-muted)';
        el.innerHTML = (isError ? '<i class="fas fa-exclamation-triangle"></i> ' : '<i class="fas fa-circle-info"></i> ') + escapeHtml(text);
    }

    /** Текущая форма → payload выбранной поверхности (используется и для preview, и для запроса). */
    function currentRequest() {
        var form = {
            prompt: byId('imgTestPrompt') ? byId('imgTestPrompt').value : '',
            negative: byId('imgTestNegative') ? byId('imgTestNegative').value : '',
            width: byId('imgTestWidth') ? byId('imgTestWidth').value : 512,
            height: byId('imgTestHeight') ? byId('imgTestHeight').value : 512,
            steps: byId('imgTestSteps') ? byId('imgTestSteps').value : 20,
            cfg: byId('imgTestCfg') ? byId('imgTestCfg').value : 7,
            sampler: byId('imgTestSampler') ? byId('imgTestSampler').value : '',
            scheduler: byId('imgTestScheduler') ? byId('imgTestScheduler').value : '',
            seed: byId('imgTestSeed') ? byId('imgTestSeed').value : '',
            batch: byId('imgTestBatch') ? byId('imgTestBatch').value : 1
        };
        var caps = state.caps || normalizeCapabilities(null);
        var surface = byId('imgTestSurface') ? byId('imgTestSurface').value : 'openai';
        var payload = buildPayload(form, caps.limits);
        return { form: form, payload: payload, surface: surface, body: payloadForSurface(payload, surface) };
    }

    function renderRequestPreview() {
        var pre = byId('imgTestRequestPreview');
        if (!pre) return;
        var req = currentRequest();
        var host = (window.location && window.location.hostname) || 'localhost';
        var info = surfaceInfo(req.surface, apiBase(), host, 18079, 18080);
        pre.textContent =
            '# ' + info.label + '\n' +
            'POST ' + info.externalUrl + '  (через балансер: ' + info.url + ')\n' +
            'Authorization: Bearer ' + (clientKey() ? '<ключ из UI>' : '<ключ не задан>') + '\n\n' +
            JSON.stringify(req.body, null, 2);
    }

    function renderResult() {
        var host = byId('imgTestResultHost');
        var meta = byId('imgTestResultMeta');
        if (!host) return;
        if (state.error) {
            var hint = errorHint(state.error);
            host.innerHTML = '<div class="gguf-empty-state" style="color:var(--danger);align-items:flex-start;text-align:left;">' +
                '<div><i class="fas fa-exclamation-triangle"></i> <strong>' + escapeHtml(t('imageTest.failed', 'Генерация не удалась')) + '</strong></div>' +
                '<pre style="white-space:pre-wrap;font-size:11px;max-height:240px;overflow:auto;margin:8px 0 0;">' + escapeHtml(state.error) + '</pre>' +
                (hint ? '<div style="margin-top:8px;font-size:12px;color:var(--text-muted);">💡 ' + escapeHtml(hint) + '</div>' : '') +
            '</div>';
            if (meta) meta.textContent = '';
            return;
        }
        if (!state.generated) {
            host.innerHTML = '<div class="gguf-empty-state">' +
                '<i class="fas fa-image" style="opacity:0.4;"></i>' +
                '<div style="margin-top:6px;">' + escapeHtml(t('imageTest.no_result', 'Результата ещё нет — нажмите «Сгенерировать».')) + '</div>' +
                '<div style="margin-top:4px;color:var(--text-muted);font-size:12px;">' +
                escapeHtml(t('imageTest.only_here', 'Это единственная страница WebUI, где показывается сгенерированное изображение: она для проверки настроек, а не для работы с картинками.')) +
                '</div></div>';
            if (meta) meta.textContent = '';
            return;
        }
        var g = state.generated;
        var imgs = g.images.map(function (im, i) {
            var src = im.url ? im.url : b64ToDataUrl(im.b64, im.format);
            return '<div style="display:inline-block;margin:0 10px 10px 0;text-align:center;">' +
                '<img src="' + escapeHtml(src) + '" alt="' + escapeHtml('result ' + (i + 1)) + '" ' +
                    'style="max-width:100%;border-radius:6px;border:1px solid var(--border-color);" />' +
                '<div style="margin-top:4px;"><a class="btn btn-secondary btn-sm" download="image-test-' + (i + 1) + '.' + escapeHtml(im.format || 'png') +
                    '" href="' + escapeHtml(src) + '">' + escapeHtml(t('imageTest.download', 'Скачать')) + '</a></div>' +
            '</div>';
        }).join('');
        host.innerHTML = imgs || '<div class="gguf-empty-state">' + escapeHtml(t('imageTest.no_images_in_response', 'В ответе нет изображений')) + '</div>';
        if (meta) {
            var m = g.meta || {};
            meta.textContent = [
                g.surface,
                // 0 мс — тоже результат (мгновенный ответ из очереди), поэтому
                // проверяем на undefined, а не на truthy.
                (m.durationMs !== undefined && m.durationMs !== null) ? (m.durationMs + ' ' + t('imageModels.ms', 'мс')) : '',
                m.status ? ('HTTP ' + m.status) : '',
                m.model ? ('model: ' + m.model) : '',
                (m.seed !== undefined && m.seed !== null) ? ('seed: ' + m.seed) : '',
                m.size || ''
            ].filter(Boolean).join(' · ');
        }
    }

    function renderHistory() {
        var card = byId('imgTestHistoryCard');
        var host = byId('imgTestHistoryHost');
        if (!card || !host) return;
        if (!state.history.length) { card.style.display = 'none'; host.innerHTML = ''; return; }
        card.style.display = '';
        host.innerHTML = state.history.map(function (h, i) {
            var first = h.images && h.images[0];
            var src = first ? (first.url ? first.url : b64ToDataUrl(first.b64, first.format)) : '';
            var p = h.params || {};
            return '<div class="gguf-download-item" style="display:flex;gap:10px;align-items:flex-start;">' +
                (src ? '<img src="' + escapeHtml(src) + '" alt="" style="width:72px;height:72px;object-fit:cover;border-radius:6px;border:1px solid var(--border-color);" />' : '') +
                '<div style="flex:1;min-width:0;">' +
                    '<div style="font-size:12px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;" title="' + escapeHtml(p.prompt || '') + '">' +
                        escapeHtml(String(p.prompt || '').slice(0, 80)) + '</div>' +
                    '<div style="font-size:11px;color:var(--text-muted);margin-top:2px;">' +
                        escapeHtml([h.surface, (p.width + 'x' + p.height), ('steps ' + p.steps), ('cfg ' + (p.cfg_scale !== undefined ? p.cfg_scale : '-')),
                            (p.sampler_name || ''), (p.seed !== undefined ? ('seed ' + p.seed) : '')].filter(Boolean).join(' · ')) +
                    '</div>' +
                    '<div style="font-size:11px;color:var(--text-muted);margin-top:2px;">' +
                        escapeHtml(new Date(h.ts).toLocaleTimeString()) + (h.durationMs ? ' · ' + h.durationMs + ' ' + t('imageModels.ms', 'мс') : '') +
                    '</div>' +
                '</div>' +
                '<div style="display:flex;flex-direction:column;gap:4px;">' +
                    '<button class="btn btn-secondary btn-sm" data-imt-action="restore" data-idx="' + i + '">' +
                        escapeHtml(t('imageTest.restore', 'В форму')) + '</button>' +
                    '<button class="btn btn-secondary btn-sm" data-imt-action="remove" data-idx="' + i + '">' +
                        escapeHtml(t('common.delete', 'Удалить')) + '</button>' +
                '</div>' +
            '</div>';
        }).join('');
    }

    function renderAll() {
        renderBackendSelect();
        renderModelSelect();
        renderCaps();
        renderModelState();
        renderRequestPreview();
        renderResult();
        renderHistory();
    }

    // =====================================================================
    // Данные и действия
    // =====================================================================

    function backendUrl(rest) {
        var b = selectedBackend() || (imageBackends(state.backends)[0] || {});
        var id = (byId('imgTestBackendSelect') && byId('imgTestBackendSelect').value) || b.id || '';
        state.backendId = id;
        return apiBase() + '/api/v1/image/backends/' + encodeURIComponent(id) + '/' + rest;
    }

    async function refresh() {
        if (!hasImageBackends(state.backends)) {
            state.models = [];
            renderAll();
            return;
        }
        var id = (byId('imgTestBackendSelect') && byId('imgTestBackendSelect').value) || state.backendId ||
            ((imageBackends(state.backends)[0] || {}).id || '');
        state.backendId = id;
        try {
            var models = await request(backendUrl('models'));
            var arr = (models && Array.isArray(models.models)) ? models.models : [];
            state.models = arr.map(function (m) {
                return {
                    name: m.name || m.id || '',
                    state: String(m.state || 'not_loaded'),
                    family: m.family || '',
                    defaults: m.defaults || {},
                    active_queries: Number(m.active_queries || 0)
                };
            }).filter(function (m) { return !!m.name; });
            notice('');
        } catch (e) {
            state.models = [];
            notice(t('imageTest.backend_unreachable', 'Бэкенд недоступен: {error}', { error: (e && e.message) || String(e) }), true);
        }
        try {
            state.caps = normalizeCapabilities(await request(backendUrl('capabilities')));
        } catch (e) {
            state.caps = normalizeCapabilities(null);
        }
        renderAll();
    }

    /** Подставить дефолты профиля выбранной модели в форму (проверяем настройки модели). */
    function applyDefaults() {
        var m = selectedModel();
        if (!m) return;
        var form = currentRequest().form;
        var next = applyModelDefaults(form, m);
        ['imgTestWidth:width', 'imgTestHeight:height', 'imgTestSteps:steps', 'imgTestCfg:cfg', 'imgTestSeed:seed', 'imgTestBatch:batch',
         'imgTestNegative:negative'].forEach(function (pair) {
            var parts = pair.split(':');
            var el = byId(parts[0]);
            if (el) el.value = next[parts[1]] === undefined || next[parts[1]] === null ? '' : next[parts[1]];
        });
        var s = byId('imgTestSampler');
        if (s && next.sampler) s.value = next.sampler;
        var sch = byId('imgTestScheduler');
        if (sch && next.scheduler) sch.value = next.scheduler;
        renderRequestPreview();
        toast(t('imageTest.defaults_applied', 'Подставлены настройки профиля модели'), 'info');
    }

    async function generate() {
        if (state.busy) return false;
        var req = currentRequest();
        if (!req.payload.prompt) {
            notice(t('imageTest.prompt_required', 'Укажите промпт'), true);
            toast(t('image.prompt_required', 'Укажите промпт'), 'error');
            return false;
        }
        state.busy = true;
        state.error = '';
        notice(t('imageTest.running', 'Генерация идёт… (модель при необходимости загрузится сама)'), false);
        var host = (window.location && window.location.hostname) || 'localhost';
        var info = surfaceInfo(req.surface, apiBase(), host, 18079, 18080);
        var started = Date.now();
        try {
            var resp = await request(info.url, {
                method: 'POST',
                body: JSON.stringify(req.body),
                timeoutMs: TIMEOUT_GENERATE_MS
            });
            var imgs = extractImages(resp);
            var durationMs = Date.now() - started;
            state.generated = {
                images: imgs,
                surface: info.label,
                meta: {
                    durationMs: durationMs,
                    status: 200,
                    model: resp && resp.model,
                    seed: resp && (resp.seed !== undefined ? resp.seed : (resp.parameters && resp.parameters.seed)),
                    size: req.payload.width + 'x' + req.payload.height,
                    raw: resp
                }
            };
            state.history.unshift({
                ts: Date.now(),
                durationMs: durationMs,
                surface: info.label,
                params: req.payload,
                images: imgs
            });
            if (state.history.length > HISTORY_MAX) state.history.length = HISTORY_MAX;
            notice(t('imageTest.done', 'Готово за {ms} мс', { ms: durationMs }), false);
            if (!imgs.length) toast(t('imageTest.no_images_in_response', 'В ответе нет изображений'), 'warn');
        } catch (e) {
            state.generated = null;
            state.error = (e && e.body && typeof e.body === 'object')
                ? JSON.stringify(e.body, null, 2)
                : ((e && e.message) || String(e));
            var h = errorHint(state.error);
            notice(h || ((e && e.message) || String(e)), true);
        }
        state.busy = false;
        renderResult();
        renderHistory();
        // Счётчики/состояние воркера могли измениться (прогрев модели) — обновим список.
        refresh();
        return true;
    }

    /**
     * loadModel — загрузка выбранной модели + ОЖИДАНИЕ результата.
     *
     * ПОЧЕМУ ЖДЁМ И ПОЧЕМУ ПОКАЗЫВАЕМ ОШИБКУ: воркер отвечает на load сразу
     * (202), а движок поднимается в фоне — падение видно только в состоянии и в
     * прогрессе. Без ожидания оператор видел тост «загрузка начата», а затем
     * молчаливый `not_loaded` и не понимал причину (именно так выглядел разбор
     * с GGUF, собранным для ComfyUI: движок писал «get sd version from file
     * failed», а UI показывал только «не загрузилась»). Поэтому опрашиваем
     * прогресс и выводим текст движка + человеческую подсказку (errorHint).
     */
    async function loadModel() {
        var m = selectedModel();
        if (!m) { toast(t('imageTest.no_models', 'нет моделей на диске'), 'error'); return false; }
        state.error = '';
        renderResult();
        try {
            await request(backendUrl('models/load'), { method: 'POST', body: JSON.stringify({ name: m.name }) });
            toast(t('image.load_started', 'Загрузка модели {name}…', { name: m.name }), 'info');
        } catch (e) {
            var msg = (e && e.body && e.body.error) || (e && e.message) || String(e);
            toast(t('image.load_failed', 'Не удалось загрузить модель: {error}', { error: msg }), 'error');
            state.error = String(msg);
            renderResult();
            await refresh();
            return false;
        }
        notice(t('imageTest.loading_model', 'Загружаю модель…'), false);
        var deadline = Date.now() + LOAD_WAIT_MS;
        var final = null;
        while (Date.now() < deadline) {
            await new Promise(function (r) { setTimeout(r, 1500); });
            try {
                var prog = await request(backendUrl('models/load/progress'));
                var p = (prog && prog.progress) || prog || {};
                var st = String(p.state || prog.state || '');
                if (st === 'loaded' || st === 'ready') { final = { ok: true }; break; }
                if (st === 'error' || st === 'failed') {
                    final = { ok: false, error: String(p.error || prog.error || '') };
                    break;
                }
            } catch (e2) {
                // Прогресс может быть недоступен — не считаем это ошибкой загрузки.
            }
        }
        if (final && final.ok) {
            notice(t('imageTest.loaded', 'Модель загружена'), false);
            toast(t('gguf.load_success', 'Модель загружена в память'), 'success');
        } else if (final) {
            state.error = final.error || t('imageTest.load_failed_unknown', 'движок не сообщил причину (смотрите логи воркера)');
            notice(t('imageTest.load_failed', 'Модель не загрузилась'), true);
            renderResult();
        } else {
            notice(t('imageTest.load_timeout', 'Модель не загрузилась за {s} с', { s: Math.round(LOAD_WAIT_MS / 1000) }), true);
        }
        await refresh();
        return !!(final && final.ok);
    }

    async function unloadModel() {
        try {
            await request(backendUrl('models/unload'), { method: 'POST', body: JSON.stringify({}) });
            toast(t('gguf.model_unloaded', 'Модель выгружена'), 'success');
        } catch (e) {
            toast(t('image.unload_failed', 'Не удалось выгрузить модель: {error}', { error: (e && e.message) || String(e) }), 'error');
        }
        await refresh();
        return true;
    }

    function restoreHistory(idx) {
        var h = state.history[idx];
        if (!h) return false;
        var p = h.params || {};
        var set = function (id, v) { var el = byId(id); if (el) el.value = v === undefined || v === null ? '' : v; };
        set('imgTestPrompt', p.prompt);
        set('imgTestNegative', p.negative_prompt);
        set('imgTestWidth', p.width);
        set('imgTestHeight', p.height);
        set('imgTestSteps', p.steps);
        set('imgTestCfg', p.cfg_scale);
        set('imgTestSeed', p.seed);
        set('imgTestBatch', p.batch_size);
        var s = byId('imgTestSampler'); if (s && p.sampler_name) s.value = p.sampler_name;
        var sch = byId('imgTestScheduler'); if (sch && p.scheduler) sch.value = p.scheduler;
        renderRequestPreview();
        return true;
    }

    function copyCurl() {
        var req = currentRequest();
        var host = (window.location && window.location.hostname) || 'localhost';
        var info = surfaceInfo(req.surface, apiBase(), host, 18079, 18080);
        var text = curlFor(info, req.body, clientKey());
        if (navigator.clipboard && navigator.clipboard.writeText) {
            navigator.clipboard.writeText(text).then(function () {
                toast(t('clientAccess.copied_curl', 'curl скопирован'), 'success');
            }, function () { toast(t('clientAccess.copy_failed', 'Не удалось скопировать'), 'error'); });
        } else {
            toast(t('clientAccess.copy_failed', 'Не удалось скопировать'), 'error');
        }
        return text;
    }

    function clearHistory() {
        state.history = [];
        renderHistory();
        return true;
    }

    // =====================================================================
    // Монтирование
    // =====================================================================

    function onClick(ev) {
        var el = ev && ev.target;
        var act = null;
        while (el && el.getAttribute) {
            act = el.getAttribute('data-imt-action');
            if (act) break;
            el = el.parentNode;
        }
        if (!act) return;
        if (act === 'generate') { generate(); return; }
        if (act === 'copy-curl') { copyCurl(); return; }
        if (act === 'load-model') { loadModel(); return; }
        if (act === 'unload-model') { unloadModel(); return; }
        if (act === 'defaults') { applyDefaults(); return; }
        if (act === 'clear-history') { clearHistory(); return; }
        if (act === 'restore') { restoreHistory(parseInt(el.getAttribute('data-idx'), 10) || 0); return; }
        if (act === 'remove') {
            state.history.splice(parseInt(el.getAttribute('data-idx'), 10) || 0, 1);
            renderHistory();
        }
    }

    function mount() {
        if (mounted) return;
        mounted = true;
        var page = byId(PAGE_ID);
        if (page && page.addEventListener) page.addEventListener('click', onClick);
        // Живой предпросмотр запроса: любое изменение формы сразу видно в JSON.
        if (page && page.addEventListener) {
            page.addEventListener('input', function () { renderRequestPreview(); });
            page.addEventListener('change', function (ev) {
                var el = ev && ev.target;
                if (el && el.id === 'imgTestBackendSelect') { state.backendId = el.value; refresh(); return; }
                if (el && el.id === 'imgTestModelSelect') { renderModelState(); return; }
                renderRequestPreview();
            });
        }
    }

    function init() {
        mount();
        renderAll();
        refresh();
    }

    function render(backends) {
        state.backends = Array.isArray(backends) ? backends : [];
        if (!state.backendId) {
            var first = imageBackends(state.backends)[0];
            if (first) state.backendId = first.id;
        }
        renderAll();
        return state.backends;
    }

    window.ImageTestPage = {
        init: init,
        mount: mount,
        render: render,
        refresh: refresh,
        syncVisibility: syncVisibility,
        generate: generate,
        loadModel: loadModel,
        unloadModel: unloadModel,
        _state: state,
        _actions: {
            refresh: refresh,
            generate: generate,
            loadModel: loadModel,
            unloadModel: unloadModel,
            applyDefaults: applyDefaults,
            copyCurl: copyCurl,
            clearHistory: clearHistory,
            restoreHistory: restoreHistory,
            currentRequest: currentRequest,
            renderAll: renderAll
        },
        pure: {
            escapeHtml: escapeHtml,
            clampInt: clampInt,
            snapDimension: snapDimension,
            buildPayload: buildPayload,
            applyModelDefaults: applyModelDefaults,
            surfaceInfo: surfaceInfo,
            payloadForSurface: payloadForSurface,
            curlFor: curlFor,
            b64ToDataUrl: b64ToDataUrl,
            extractImages: extractImages,
            normalizeCapabilities: normalizeCapabilities,
            errorHint: errorHint,
            hasImageBackends: hasImageBackends,
            LIMITS: LIMITS,
            STATIC_SAMPLERS: STATIC_SAMPLERS,
            STATIC_SCHEDULERS: STATIC_SCHEDULERS
        }
    };
})();
