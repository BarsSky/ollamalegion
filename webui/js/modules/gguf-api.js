/**
 * GGUF API client — communicates with cppworker on port 18092 by default
 * Provides methods for HuggingFace search, downloads, model management, GPU info
 *
 * Port handling:
 *   - 18092 — актуальный default для современных llama.cpp CppWorker
 *   - 18091 — legacy (старая инсталляция)
 *   - 18093 — альтернативный (доп. профиль)
 *   - 18090 — зарезервировано (на практике это agentPort, но в редких случаях тоже CppWorker)
 *
 * Если зарегистрированный бэкенд использует устаревший/неправильный порт, WebUI попытается
 * runtime-auto-detect рабочий порт через probe /info с кэшированием на 5 минут.
 */
const GgufApi = (function () {
    // Use nginx proxy URL when in Docker, fallback to localhost for dev
    let workerUrl = (window.WEBUI_CONFIG && window.WEBUI_CONFIG.CPPWORKER_URL) || 'http://localhost:18092';
    let connected = false;
    let connectionCallbacks = [];
    let hfToken = localStorage.getItem('ollamalegion_hf_token') || '';
    let apiToken = (window.WEBUI_CONFIG && window.WEBUI_CONFIG.API_TOKEN) || '';
    const REQUEST_TIMEOUT_MS = 10000; // 10-second timeout for all requests

    // Default ports per backend type (used when backend.url has no port).
    // ВАЖНО: 18092 (а не 18090/18091) — это правильный default для llama.cpp CppWorker.
    const DEFAULT_LLAMACPP_PORT = 18092;
    const DEFAULT_OLLAMA_PORT = 11434;
    // Список портов, которые пробуем при auto-detect. Первый — предпочтительный.
    const LLAMACPP_CANDIDATE_PORTS = [18092, 18091, 18093, 18090];
    // Кэш успешного порта по хосту, чтобы не делать probe на каждом запросе.
    // Структура: { 'host:preferredPort': { port: 18092, expiresAt: 1234567890 } }
    const _portCache = Object.create(null);
    const PORT_CACHE_TTL_MS = 5 * 60 * 1000; // 5 минут

    /**
     * Возвращает список хостов, которые имеет смысл пробить, если основной хост
     * из backend.url (например, host.docker.internal или IP контейнера)
     * недоступен из браузера. Первый — наиболее вероятный кандидат.
     *
     * Типичный сценарий: пользователь зарегистрировал бэкенд с host=host.docker.internal
     * (это работает внутри Docker-контейнера, но НЕ из браузера). В этом случае из браузера
     * доступен localhost (если CppWorker запущен на том же хосте, что и webui) или
     * исходный IP бэкенда (если он известен).
     *
     * @param {string} originalHost — хост из backend.url
     * @param {string} [fallbackHost] — оригинальный host из backend (если есть)
     * @returns {string[]} массив уникальных хостов для пробы
     */
    function _resolveHostCandidates(originalHost, fallbackHost) {
        var set = [];
        var seen = Object.create(null);
        function add(h) {
            if (h && typeof h === 'string' && h.length > 0 && !seen[h]) {
                seen[h] = true;
                set.push(h);
            }
        }
        add(originalHost);
        // host.docker.internal — Docker-специфичный алиас, в браузере не резолвится.
        // Если видим его — добавляем localhost как fallback.
        if (originalHost === 'host.docker.internal' || /host\.docker\.internal/.test(originalHost)) {
            add('localhost');
            add('127.0.0.1');
        }
        // Fallback host из backend (оригинальный, до Docker-преобразования)
        add(fallbackHost);
        return set;
    }

    /**
     * Probe /info на конкретном порту. Возвращает true, если порт отвечает.
     * Использует короткий таймаут (1500 мс) и AbortController.
     */
    function _probePort(host, port) {
        return new Promise(function (resolve) {
            try {
                const ctrl = new AbortController();
                const tid = setTimeout(function () { ctrl.abort(); }, 1500);
                fetch('http://' + host + ':' + port + '/info', {
                    signal: ctrl.signal,
                    mode: 'cors',
                    cache: 'no-store',
                    headers: { 'Accept': 'application/json' }
                }).then(function (resp) {
                    clearTimeout(tid);
                    resolve(!!(resp && resp.ok));
                }).catch(function () {
                    clearTimeout(tid);
                    resolve(false);
                });
            } catch (e) {
                resolve(false);
            }
        });
    }

    /**
     * Auto-detect рабочего {host, port} CppWorker.
     * Проверяет комбинации (host × port) в порядке:
     *   1. Сначала preferred host:preferred port (happy-path)
     *   2. Потом preferred host × альтернативные порты
     *   3. Потом host candidates (если preferred — host.docker.internal) × preferred port
     *   4. Потом host candidates × альтернативные порты
     *
     * Возвращает { host, port } или null, если ни одна комбинация не ответила.
     * Результат кэшируется на PORT_CACHE_TTL_MS.
     *
     * @param {string} host
     * @param {number} [preferredPort]
     * @param {string} [fallbackHost] — оригинальный host из backend (если есть)
     * @returns {Promise<{host:string,port:number}|null>}
     */
    async function _autoDetectWorkerHostPort(host, preferredPort, fallbackHost) {
        if (!host) return null;
        const cacheKey = host + ':' + (preferredPort || 0) + '|' + (fallbackHost || '');
        const cached = _portCache[cacheKey];
        const now = Date.now();
        if (cached && cached.expiresAt > now) {
            return cached.result;
        }

        const hostCandidates = _resolveHostCandidates(host, fallbackHost);
        const portOrder = [];
        if (preferredPort && preferredPort > 0) {
            portOrder.push(preferredPort);
        }
        for (let i = 0; i < LLAMACPP_CANDIDATE_PORTS.length; i++) {
            const p = LLAMACPP_CANDIDATE_PORTS[i];
            if (portOrder.indexOf(p) === -1) portOrder.push(p);
        }

        // Probe (host, port) pairs
        for (let h = 0; h < hostCandidates.length; h++) {
            const hst = hostCandidates[h];
            for (let p = 0; p < portOrder.length; p++) {
                const port = portOrder[p];
                // eslint-disable-next-line no-await-in-loop
                const ok = await _probePort(hst, port);
                if (ok) {
                    const result = { host: hst, port: port };
                    _portCache[cacheKey] = { result: result, expiresAt: now + PORT_CACHE_TTL_MS };
                    return result;
                }
            }
        }
        return null;
    }

    /**
     * Обратная совместимость: возвращает только port (для публичного autoDetectCppWorkerPort API).
     * @param {string} host
     * @param {number} [preferredPort]
     * @returns {Promise<number|null>}
     */
    async function _autoDetectWorkerPort(host, preferredPort) {
        const r = await _autoDetectWorkerHostPort(host, preferredPort, null);
        return r ? r.port : null;
    }

    /**
     * Sync-хелпер для построения URL бэкенда. Возвращает строку URL.
     * Используется в `buildBackendWorkerUrl` (sync) и `buildBackendWorkerUrlAsync` (async).
     * Правила:
     *  - Если `url` абсолютный и содержит порт — используем как есть.
     *  - Если `url` без порта — добавляем type-correct default (18092/11434) из port-полей.
     *  - Если нет ни url ни host — возвращаем workerUrl (fallback на прямую CppWorker-конфигурацию).
     */
    function _buildBackendWorkerUrlSync(backend) {
        if (!backend) return workerUrl;
        if (backend.url && backend.url.indexOf('http') === 0) {
            var baseUrl = backend.url.replace(/\/+$/, '');
            try {
                var u = new URL(baseUrl);
                if (u.port && u.port.length > 0) {
                    return baseUrl;
                }
                var port = 0;
                var bt = (backend.type || backend.backendType || '').toLowerCase();
                if (bt === 'llama_cpp' || bt === 'llamacpp' || bt === 'llama.cpp') {
                    port = (backend.cppWorkerPort && backend.cppWorkerPort > 0) ? backend.cppWorkerPort : DEFAULT_LLAMACPP_PORT;
                } else if (bt === 'ollama') {
                    port = (backend.ollamaPort && backend.ollamaPort > 0) ? backend.ollamaPort : DEFAULT_OLLAMA_PORT;
                } else if (backend.cppWorkerPort && backend.cppWorkerPort > 0) {
                    port = backend.cppWorkerPort;
                } else if (backend.ollamaPort && backend.ollamaPort > 0) {
                    port = backend.ollamaPort;
                }
                if (port > 0) {
                    return u.protocol + '//' + u.hostname + ':' + port + (u.pathname || '');
                }
                return baseUrl;
            } catch (e) {
                return baseUrl;
            }
        }
        if (backend.host) {
            var port2 = 0;
            var bt2 = (backend.type || backend.backendType || '').toLowerCase();
            if (bt2 === 'llama_cpp' || bt2 === 'llamacpp' || bt2 === 'llama.cpp') {
                port2 = (backend.cppWorkerPort && backend.cppWorkerPort > 0) ? backend.cppWorkerPort : DEFAULT_LLAMACPP_PORT;
            } else if (bt2 === 'ollama') {
                port2 = (backend.ollamaPort && backend.ollamaPort > 0) ? backend.ollamaPort : DEFAULT_OLLAMA_PORT;
            } else if (backend.cppWorkerPort && backend.cppWorkerPort > 0) {
                port2 = backend.cppWorkerPort;
            } else if (backend.ollamaPort && backend.ollamaPort > 0) {
                port2 = backend.ollamaPort;
            }
            if (port2 > 0) {
                return 'http://' + backend.host.replace(/^https?:\/\//, '').replace(/\/+$/, '') + ':' + port2;
            }
            return 'http://' + backend.host.replace(/^https?:\/\//, '').replace(/\/+$/, '');
        }
        return workerUrl;
    }

    function buildUrl(path) {
        return workerUrl.replace(/\/+$/, '') + path;
    }

    /**
     * Generic request with AbortController timeout.
     * All requests to cppworker go through this — no more hanging fetch.
     */
    async function request(path, options = {}) {
        const controller = new AbortController();
        const timer = setTimeout(function () { controller.abort(); }, REQUEST_TIMEOUT_MS);
        try {
            const headers = { 'Content-Type': 'application/json', ...(options.headers || {}) };
            // Pass HF token to HF endpoints
            if (hfToken && (path === '/api/hf/search' || path.indexOf('/api/hf/files') === 0 || path === '/api/hf/download')) {
                headers['X-HF-Token'] = hfToken;
            }
            // ВАЖНО (Session 17 P.4, 2026-07-27): не spread'им `...options` после headers,
            // иначе caller headers перезапишут наш X-HF-Token. Явный fetch.
            const fetchOptions = {
                method: options.method,
                headers: headers,
                signal: controller.signal,
            };
            if (options.body !== undefined) fetchOptions.body = options.body;
            const response = await fetch(buildUrl(path), fetchOptions);
            clearTimeout(timer);
            if (!response.ok) {
                const text = await response.text().catch(function () { return ''; });
                throw new Error('HTTP ' + response.status + ': ' + (text || response.statusText));
            }
            // Handle 204 No Content
            if (response.status === 204) return null;
            const ct = response.headers.get('content-type') || '';
            return ct.includes('application/json') ? response.json() : response.text();
        } catch (err) {
            clearTimeout(timer);
            if (err.name === 'AbortError') {
                throw new Error('Request timeout after ' + (REQUEST_TIMEOUT_MS / 1000) + 's — cppworker unreachable at ' + workerUrl);
            }
            if (!err.message.startsWith('HTTP')) {
                err.message = 'Network error: ' + err.message;
            }
            throw err;
        }
    }

    async function getJson(path) {
        return request(path, { method: 'GET' });
    }

    return {
        // ---- Connection Management ----

        /** Set the cppworker base URL */
        setUrl(url) {
            workerUrl = url || 'http://localhost:18092';
        },

        getUrl() {
            return workerUrl;
        },

        /** Set HuggingFace API token for gated model access */
        setHFToken(token) {
            hfToken = token || '';
            if (hfToken) {
                localStorage.setItem('ollamalegion_hf_token', hfToken);
            } else {
                localStorage.removeItem('ollamalegion_hf_token');
            }
        },

        /** Get current HuggingFace token */
        getHFToken() {
            return hfToken;
        },

        /** Test connection to cppworker */
        async testConnection() {
            try {
                const info = await getJson('/info');
                connected = true;
                connectionCallbacks.forEach(cb => cb(true));
                return { ok: true, info };
            } catch (err) {
                connected = false;
                connectionCallbacks.forEach(cb => cb(false));
                return { ok: false, error: err.message };
            }
        },

        isConnected() {
            return connected;
        },

        onConnectionChange(callback) {
            connectionCallbacks.push(callback);
        },

        // ---- Health & Info ----

        /** Get worker info (version, etc.) */
        async getInfo() {
            return getJson('/info');
        },

        /** Get GPU info */
        async getGpuInfo() {
            try {
                return await getJson('/api/gpu');
            } catch (e) {
                // Fallback to /info if /api/gpu not available
                const info = await getJson('/info');
                return info && info.gpu ? info.gpu : null;
            }
        },

        // ---- HuggingFace Search ----

        /** Search models on HuggingFace Hub */
        async searchModels(query, limit = 10) {
            return getJson('/api/hf/search?query=' + encodeURIComponent(query) + '&limit=' + limit);
        },

        /** List files for a HuggingFace model */
        async listModelFiles(modelId, revision = 'main') {
            return getJson('/api/hf/files?modelId=' + encodeURIComponent(modelId) + '&revision=' + encodeURIComponent(revision));
        },

        // ---- Downloads ----

        /** Start downloading a model file from HuggingFace */
        async startDownload(modelId, filename, revision = 'main') {
            return request('/api/hf/download', {
                method: 'POST',
                body: JSON.stringify({ modelId, filename, revision })
            });
        },

        /** Get download progress */
        async getDownloadProgress(modelId, filename) {
            return getJson('/api/hf/progress?modelId=' + encodeURIComponent(modelId) + '&filename=' + encodeURIComponent(filename));
        },

        /** List all active downloads */
        async listActiveDownloads() {
            return getJson('/api/hf/downloads');
        },

        /** Cancel a download */
        async cancelDownload(modelId, filename) {
            return request('/api/hf/cancel', {
                method: 'POST',
                body: JSON.stringify({ modelId, filename })
            });
        },

    // ---- Model Management via Balancer ----

        /**
         * Manage model on a registered backend via the balancer API.
         * @param {string} backendId — backend ID (e.g., 'llama_gpu')
         * @param {string} operation — 'load' or 'unload'
         * @param {string} modelName — model name/path
         * @param {object} [options] — load options (gpuLayers, ctxSize, etc.)
         */
        async manageModel(backendId, operation, modelName, options = {}) {
            var balancerUrl = (window.WEBUI_CONFIG && (window.WEBUI_CONFIG.API_BASE_URL || window.WEBUI_CONFIG.API_BASE)) ||
                (typeof window !== 'undefined' && window.location && window.location.origin) ||
                '/';
            const url = balancerUrl.replace(/\/+$/, '') + '/api/v1/backends/' + encodeURIComponent(backendId) + '/models';
            const body = {
                operation: operation,
                modelName: modelName,
                ...options
            };
            const controller = new AbortController();
            const timer = setTimeout(function () { controller.abort(); }, 30000);
            try {
                const headers = { 'Content-Type': 'application/json' };
                if (apiToken) {
                    headers['X-API-Token'] = apiToken;
                }
                const response = await fetch(url, {
                    method: 'POST',
                    headers: headers,
                    body: JSON.stringify(body),
                    signal: controller.signal
                });
                clearTimeout(timer);
                const data = await response.json();
                if (!response.ok) {
                    return { success: false, error: data.error || 'HTTP ' + response.status };
                }
                return data;
            } catch (err) {
                clearTimeout(timer);
                return { success: false, error: err.message };
            }
        },

        /**
         * Get models list for a specific backend via balancer API.
         * GET /api/v1/backends/{backendId}/models
         */
        async getBackendModels(backendId) {
            var balancerUrl = (window.WEBUI_CONFIG && (window.WEBUI_CONFIG.API_BASE_URL || window.WEBUI_CONFIG.API_BASE)) ||
                (typeof window !== 'undefined' && window.location && window.location.origin) ||
                '/';
            const url = balancerUrl.replace(/\/+$/, '') + '/api/v1/backends/' + encodeURIComponent(backendId) + '/models';
            const controller = new AbortController();
            const timer = setTimeout(function () { controller.abort(); }, REQUEST_TIMEOUT_MS);
            try {
                const headers = { 'Content-Type': 'application/json' };
                if (apiToken) {
                    headers['X-API-Token'] = apiToken;
                }
                const response = await fetch(url, {
                    headers: headers,
                    signal: controller.signal
                });
                clearTimeout(timer);
                if (!response.ok) {
                    const text = await response.text().catch(function () { return ''; });
                    throw new Error('HTTP ' + response.status + ': ' + (text || response.statusText));
                }
                return response.json();
            } catch (err) {
                clearTimeout(timer);
                throw err;
            }
        },

        // ---- Balancer Backends Info ----

        /**
         * Get registered llama.cpp backends info from balancer API.
         * This replaces the need to connect directly to individual cppworkers.
         * Returns: { backends: [...], total: N, backendType: "llama_cpp", operatingMode: "..." }
         */
        async getLlamaCppBackends() {
            // Use the balancer's API server URL instead of cppworker.
            // Priority: 1) WEBUI_CONFIG.API_BASE_URL, 2) WEBUI_CONFIG.API_BASE, 3) current origin (Docker/nginx), 4) relative path
            var balancerUrl = (window.WEBUI_CONFIG && (window.WEBUI_CONFIG.API_BASE_URL || window.WEBUI_CONFIG.API_BASE)) ||
                (typeof window !== 'undefined' && window.location && window.location.origin) ||
                '/';
            const url = balancerUrl.replace(/\/+$/, '') + '/api/v1/gguf/backends';
            console.log('[GGUF] Fetching backends from:', url);
            const controller = new AbortController();
            const timer = setTimeout(function () { controller.abort(); }, REQUEST_TIMEOUT_MS);
            try {
                const headers = { 'Content-Type': 'application/json' };
                if (apiToken) {
                    headers['X-API-Token'] = apiToken;
                }
                const response = await fetch(url, {
                    headers: headers,
                    signal: controller.signal
                });
                clearTimeout(timer);
                if (!response.ok) {
                    const text = await response.text().catch(function () { return ''; });
                    throw new Error('HTTP ' + response.status + ': ' + (text || response.statusText));
                }
                return response.json();
            } catch (err) {
                clearTimeout(timer);
                if (err.name === 'AbortError') {
                    throw new Error('Request timeout — balancer API unreachable');
                }
                throw err;
            }
        },

        // ---- Local Model Management ----

        /** List local GGUF files */
        async listLocalModels() {
            return getJson('/api/models/files');
        },

        /** Load a model */
        async loadModel(modelName, options = {}) {
            return request('/api/models/load', {
                method: 'POST',
                body: JSON.stringify({
                    name: modelName,
                    path: options.path || '',
                    ctxSize: options.ctxSize || 2048,
                    batchSize: options.batchSize || 512,
                    gpuLayers: options.gpuLayers !== undefined ? options.gpuLayers : -1,
                    flashAttn: options.flashAttn || false,
                    numa: options.numa || false,
                    useMmap: options.useMmap !== undefined ? options.useMmap : true,
                    tensorSplit: options.tensorSplit || null
                })
            });
        },

        /** Unload a model */
        async unloadModel(modelHandle) {
            return request('/api/models/unload', {
                method: 'POST',
                body: JSON.stringify({ modelHandle: modelHandle })
            });
        },

        /** List loaded models */
        async listLoadedModels() {
            return getJson('/api/models');
        },

        // ---- CppWorker direct calls (used for selected backend detail view) ----

        /**
         * Build full URL to a backend's cppworker.
         * Backend objects from /api/v1/gguf/backends expose `url` (host[:port]) and `cppWorkerPort`/`ollamaPort`.
         *
         * Rules:
         *  - If `url` is absolute and already has a port, use it as-is.
         *  - If `url` has no port, append the type-correct default port
         *    (18092 for llama_cpp, 11434 for ollama) from the corresponding port field.
         *  - If neither port is set explicitly, fall back to type default.
         *  - As a last resort, use the configured `workerUrl` (only valid for direct cppworker connections).
         *
         * NOTE: для случая, когда зарегистрированный порт неверен (например, устаревший 18091,
         * хотя реально CppWorker слушает 18092), используйте `buildBackendWorkerUrlAsync` —
         * он попробует probe /info на альтернативных портах и вернёт рабочий URL.
         */
        buildBackendWorkerUrl(backend) {
            return _buildBackendWorkerUrlSync(backend);
        },

        /**
         * Async-версия buildBackendWorkerUrl с runtime auto-detect.
         * Если URL из backend содержит порт, проверяет его через probe /info.
         * При неуспехе пробует:
         *   1) альтернативные порты на том же хосте (18092, 18091, 18093, 18090);
         *   2) альтернативные хосты (например, localhost вместо host.docker.internal)
         *      на preferred и альтернативных портах.
         * Возвращает первый рабочий (host, port). Результат кэшируется на 5 минут.
         *
         * @param {object} backend
         * @returns {Promise<string>} URL бэкенда (рабочий или fallback на sync-логику)
         */
        async buildBackendWorkerUrlAsync(backend) {
            var baseUrl = _buildBackendWorkerUrlSync(backend);
            if (!backend) return baseUrl;

            var urlHost = '';
            var urlPort = 0;
            try {
                var u = new URL(baseUrl);
                urlHost = u.hostname;
                urlPort = parseInt(u.port, 10) || 0;
            } catch (e) {
                return baseUrl;
            }
            if (!urlHost || !urlPort) return baseUrl;

            // Fast path: проверяем текущий URL
            var ok = await _probePort(urlHost, urlPort);
            if (ok) return baseUrl;

            // Fast path провалился — пробуем автодетект host+port
            var fallbackHost = backend.host || null;
            if (window.console && console.warn) {
                console.warn('[gguf-api] backend', (backend.id || backend.name || '?'),
                    'at', urlHost + ':' + urlPort, 'unreachable — trying auto-detect',
                    '(fallback host:', fallbackHost + ')');
            }
            var detected = await _autoDetectWorkerHostPort(urlHost, urlPort, fallbackHost);
            if (detected && (detected.host !== urlHost || detected.port !== urlPort)) {
                // Подменяем host:port в URL
                try {
                    var u2 = new URL(baseUrl);
                    u2.hostname = detected.host;
                    u2.port = String(detected.port);
                    var newUrl = u2.toString().replace(/\/+$/, '');
                    if (window.console && console.info) {
                        console.info('[gguf-api] auto-detected', detected.host + ':' + detected.port,
                            '(was', urlHost + ':' + urlPort + ')');
                    }
                    return newUrl;
                } catch (e) {
                    // ignore — вернём baseUrl
                }
            }
            // Auto-detect не помог — возвращаем исходный URL (он отвалится с понятной ошибкой)
            return baseUrl;
        },

        /**
         * Auto-detect рабочего порта CppWorker на хосте.
         * Публичный API: используется при регистрации бэкенда и при runtime-фоллбеке.
         * @param {string} host
         * @returns {Promise<number|null>} рабочий порт или null
         */
        autoDetectCppWorkerPort: function (host) {
            return _autoDetectWorkerPort(host, 0);
        },

        /**
         * Сброс кэша auto-detect (используется в тестах / при ручном retry).
         */
        clearPortCache: function () {
            for (var k in _portCache) {
                if (Object.prototype.hasOwnProperty.call(_portCache, k)) {
                    delete _portCache[k];
                }
            }
        },

        /**
         * Generic cppworker request against an arbitrary base URL.
         * Used when interacting with a specific backend selected in the master-detail view.
         */
        async requestAt(baseUrl, path, options = {}) {
            const controller = new AbortController();
            const timer = setTimeout(function () { controller.abort(); }, REQUEST_TIMEOUT_MS);
            try {
                const headers = { 'Content-Type': 'application/json', ...(options.headers || {}) };
                if (hfToken && (path === '/api/hf/search' || path.indexOf('/api/hf/files') === 0 || path === '/api/hf/download')) {
                    headers['X-HF-Token'] = hfToken;
                }
                const fullUrl = baseUrl.replace(/\/+$/, '') + path;
                const response = await fetch(fullUrl, {
                    headers: headers,
                    signal: controller.signal,
                    ...options
                });
                clearTimeout(timer);
                if (!response.ok) {
                    const text = await response.text().catch(function () { return ''; });
                    throw new Error('HTTP ' + response.status + ': ' + (text || response.statusText));
                }
                if (response.status === 204) return null;
                const ct = response.headers.get('content-type') || '';
                return ct.includes('application/json') ? response.json() : response.text();
            } catch (err) {
                clearTimeout(timer);
                if (err.name === 'AbortError') {
                    throw new Error('Request timeout after ' + (REQUEST_TIMEOUT_MS / 1000) + 's — cppworker unreachable at ' + baseUrl);
                }
                if (!err.message.startsWith('HTTP')) {
                    err.message = 'Network error: ' + err.message;
                }
                throw err;
            }
        },

        /** GET /info from a specific backend */
        async getInfoAt(baseUrl) {
            return this.requestAt(baseUrl, '/info');
        },

        /** GET /api/gpu from a specific backend */
        async getGpuInfoAt(baseUrl) {
            try {
                return await this.requestAt(baseUrl, '/api/gpu');
            } catch (e) {
                const info = await this.requestAt(baseUrl, '/info');
                return info && info.gpu ? info.gpu : null;
            }
        },

        /** List local GGUF files on a specific backend */
        async listLocalModelsAt(baseUrl) {
            return this.requestAt(baseUrl, '/api/models/files');
        },

        /** List loaded models on a specific backend */
        async listLoadedModelsAt(baseUrl) {
            return this.requestAt(baseUrl, '/api/models');
        },

        /** List active downloads on a specific backend */
        async listActiveDownloadsAt(baseUrl) {
            return this.requestAt(baseUrl, '/api/hf/downloads');
        },

        /** Load a model on a specific backend cppworker */
        async loadModelAt(baseUrl, modelName, options = {}) {
            return this.requestAt(baseUrl, '/api/models/load', {
                method: 'POST',
                body: JSON.stringify({
                    name: modelName,
                    path: options.path || '',
                    ctxSize: options.ctxSize || 2048,
                    batchSize: options.batchSize || 512,
                    gpuLayers: options.gpuLayers !== undefined ? options.gpuLayers : -1,
                    flashAttn: options.flashAttn || false,
                    numa: options.numa || false,
                    useMmap: options.useMmap !== undefined ? options.useMmap : true,
                    tensorSplit: options.tensorSplit || null
                })
            });
        },

        /** Unload a model on a specific backend cppworker */
        async unloadModelAt(baseUrl, modelHandle) {
            return this.requestAt(baseUrl, '/api/models/unload', {
                method: 'POST',
                body: JSON.stringify({ modelHandle: modelHandle })
            });
        },

        /**
         * Delete a model file on a specific backend cppworker.
         * Tries POST /api/models/delete first, falls back to POST /api/delete (Ollama-compatible).
         */
        async deleteModelAt(baseUrl, modelName) {
            const payload = JSON.stringify({ name: modelName });
            try {
                return await this.requestAt(baseUrl, '/api/models/delete', {
                    method: 'POST',
                    body: payload
                });
            } catch (e) {
                // Fallback to Ollama-compatible alias
                return this.requestAt(baseUrl, '/api/delete', {
                    method: 'POST',
                    body: payload
                });
            }
        },

        // ---- Balancer-side model operations ----

        /**
         * Delete a model on a registered backend via the balancer API.
         * POST /api/v1/backends/{id}/models with operation=delete.
         */
        async deleteModelOnBackend(backendId, modelName) {
            return this.manageModel(backendId, 'delete', modelName);
        },

        // ---- GGUF backend proxy via Balancer ----
        // Используется страницей GGUF в WebUI для скачивания HF-моделей и управления
        // локальными файлами. Работает через балансер (CORS-safe, не зависит от
        // host.docker.internal в браузере).

        /**
         * Build balancer proxy URL for a specific backend's CppWorker.
         * WebUI → /api/v1/gguf/backends/{id}/proxy/<cppworker-path>
         * @param {string} backendId — ID бэкенда
         * @param {string} path — путь относительно CppWorker (например '/api/hf/download')
         */
        buildBackendProxyUrl(backendId, path) {
            var balancerUrl = (window.WEBUI_CONFIG && (window.WEBUI_CONFIG.API_BASE_URL || window.WEBUI_CONFIG.API_BASE)) ||
                (typeof window !== 'undefined' && window.location && window.location.origin) ||
                '/';
            return balancerUrl.replace(/\/+$/, '') + '/api/v1/gguf/backends/' + encodeURIComponent(backendId) + '/proxy' + path;
        },

        /**
         * Запрос к CppWorker конкретного бэкенда через прокси балансера.
         * @param {string} backendId — ID бэкенда (например, 'llama_gpu')
         * @param {string} path — путь относительно CppWorker (например '/api/hf/download')
         * @param {object} [options] — fetch-опции (method, body, headers)
         * @returns {Promise<any>}
         */
        async requestViaBackend(backendId, path, options = {}) {
            if (!backendId) {
                throw new Error('backendId is required for requestViaBackend');
            }
            if (!path || path[0] !== '/') {
                throw new Error('path must start with /');
            }
            const controller = new AbortController();
            const timer = setTimeout(function () { controller.abort(); }, REQUEST_TIMEOUT_MS);
            try {
                const headers = { 'Content-Type': 'application/json', ...(options.headers || {}) };
                // Прокидываем API-токен для авторизации на балансировщике
                if (apiToken) {
                    headers['X-API-Token'] = apiToken;
                }
                // Прокидываем HF token в CppWorker через прокси
                if (hfToken && (path === '/api/hf/search' || path.indexOf('/api/hf/files') === 0 || path === '/api/hf/download')) {
                    headers['X-HF-Token'] = hfToken;
                }
                const fullUrl = this.buildBackendProxyUrl(backendId, path);
                // ВАЖНО (Session 17 P.4, 2026-07-27): НЕЛЬЗЯ использовать `...options` после
                // `headers: headers`, потому что spread object в JS перезаписывает
                // свойства слева направо. Если caller передал `options.headers` (например
                // `{ 'Content-Type': 'application/json' }`), то наш X-API-Token будет
                // затёрт → HTTP 401 на per-backend save.
                // Фикс: явно мерджим нужные поля, и НЕ spread'им options целиком.
                const fetchOptions = {
                    method: options.method,
                    headers: headers,
                    signal: controller.signal,
                };
                if (options.body !== undefined) {
                    fetchOptions.body = options.body;
                }
                // Поддержка AbortController и других опций которые мог пропустить
                // (credentials, mode, cache, redirect) — мерджим осторожно,
                // НЕ допуская override наших headers.
                if (options.credentials) fetchOptions.credentials = options.credentials;
                if (options.mode) fetchOptions.mode = options.mode;
                if (options.cache) fetchOptions.cache = options.cache;
                if (options.redirect) fetchOptions.redirect = options.redirect;
                const response = await fetch(fullUrl, fetchOptions);
                clearTimeout(timer);
                if (!response.ok) {
                    let body = '';
                    try { body = await response.text(); } catch (e) { /* ignore */ }
                    let msg = 'HTTP ' + response.status;
                    if (body) {
                        try {
                            const parsed = JSON.parse(body);
                            if (parsed && parsed.message) msg += ': ' + parsed.message;
                            else if (parsed && parsed.error) msg += ': ' + parsed.error;
                        } catch (e) {
                            msg += ': ' + body.substring(0, 200);
                        }
                    }
                    const err = new Error(msg);
                    err.status = response.status;
                    err.backend = backendId;
                    err.target = fullUrl;
                    // Структурированный код ошибки для UI (чтобы renderer
                    // мог показать специфичное сообщение, а не общий «HTTP 500»).
                    if (response.status === 401 || response.status === 403) {
                        // 401/403 от HF = нужен токен (gated/приватный репозиторий)
                        err.code = 'auth_required';
                    } else if (response.status === 404) {
                        // 404 = модель/файл не найден
                        err.code = 'not_found';
                    } else if (response.status === 409) {
                        // 409 = конфликт (например, дубликат загрузки — теперь
                        // сервер возвращает 200, но на всякий случай поддержим 409)
                        err.code = 'conflict';
                    } else if (response.status === 429) {
                        err.code = 'rate_limited';
                    } else if (response.status >= 500) {
                        err.code = 'server_error';
                    } else {
                        err.code = 'http_' + response.status;
                    }
                    throw err;
                }
                if (response.status === 204) return null;
                const ct = response.headers.get('content-type') || '';
                return ct.includes('application/json') ? response.json() : response.text();
            } catch (err) {
                clearTimeout(timer);
                if (err.name === 'AbortError') {
                    throw new Error('Request timeout after ' + (REQUEST_TIMEOUT_MS / 1000) + 's — proxy to backend ' + backendId + ' is unreachable');
                }
                throw err;
            }
        },

        /** GET /info from a specific backend via balancer proxy */
        async getInfoViaBackend(backendId) {
            return this.requestViaBackend(backendId, '/info');
        },

        /** GET /api/gpu from a specific backend via balancer proxy (fallback to /info.gpu) */
        async getGpuInfoViaBackend(backendId) {
            try {
                return await this.requestViaBackend(backendId, '/api/gpu');
            } catch (e) {
                const info = await this.requestViaBackend(backendId, '/info');
                return info && info.gpu ? info.gpu : null;
            }
        },

        /** List local GGUF files on a specific backend via balancer proxy */
        async listLocalModelsViaBackend(backendId) {
            return this.requestViaBackend(backendId, '/api/models/files');
        },

        /** List loaded models on a specific backend via balancer proxy */
        async listLoadedModelsViaBackend(backendId) {
            return this.requestViaBackend(backendId, '/api/models');
        },

        /**
         * Получить runtime-параметры загруженных моделей (n_ctx, gpu_layers,
         * batch_size, flash_attn, n_layers, n_embd и т.д.) с cppworker'а через
         * прокси балансировщика. Используется на вкладке GGUF Models в WebUI
         * чтобы показать рядом с дефолтами: «default: 8192, runtime: 32768».
         *
         * GET /api/v1/cppworker/config/runtime
         * Ответ: { loaded_models: [{name, path, state, context_size, gpu_layers, ...}], count, ... }
         */
        async getRuntimeConfigViaBackend(backendId) {
            return this.requestViaBackend(backendId, '/api/v1/cppworker/config/runtime');
        },

        /** List active downloads on a specific backend via balancer proxy */
        async listActiveDownloadsViaBackend(backendId) {
            return this.requestViaBackend(backendId, '/api/hf/downloads');
        },

        /** Start downloading a model file from HuggingFace via balancer proxy */
        async startDownloadViaBackend(backendId, modelId, filename, revision = 'main') {
            console.log('[gguf-api] startDownloadViaBackend: enter', { backendId, modelId, filename, revision });
            try {
                const result = await this.requestViaBackend(backendId, '/api/hf/download', {
                    method: 'POST',
                    body: JSON.stringify({ modelId: modelId, filename: filename, revision: revision })
                });
                console.log('[gguf-api] startDownloadViaBackend: ok', { backendId, modelId, filename, result });
                return result;
            } catch (e) {
                console.log('[gguf-api] startDownloadViaBackend: error', { backendId, modelId, filename, error: e && e.message, status: e && e.status });
                throw e;
            }
        },

        /** Get download progress via balancer proxy */
        async getDownloadProgressViaBackend(backendId, modelId, filename) {
            const qs = '?modelId=' + encodeURIComponent(modelId) + '&filename=' + encodeURIComponent(filename);
            return this.requestViaBackend(backendId, '/api/hf/progress' + qs);
        },

        /** Cancel a download via balancer proxy */
        async cancelDownloadViaBackend(backendId, modelId, filename) {
            return this.requestViaBackend(backendId, '/api/hf/cancel', {
                method: 'POST',
                body: JSON.stringify({ modelId: modelId, filename: filename })
            });
        },

        /** Search HF models via balancer proxy */
        async searchModelsViaBackend(backendId, query, limit = 10) {
            return this.requestViaBackend(backendId, '/api/hf/search?query=' + encodeURIComponent(query) + '&limit=' + limit);
        },

        /** List files for a HF model via balancer proxy */
        async listModelFilesViaBackend(backendId, modelId, revision = 'main') {
            return this.requestViaBackend(backendId, '/api/hf/files?modelId=' + encodeURIComponent(modelId) + '&revision=' + encodeURIComponent(revision));
        },

        /** Delete a model on a specific backend via balancer proxy */
        async deleteModelViaBackend(backendId, modelName) {
            return this.requestViaBackend(backendId, '/api/models/delete', {
                method: 'POST',
                body: JSON.stringify({ name: modelName })
            });
        },

        // ---- Load progress (for sidebar/loadedPane live updates) ----

        /**
         * Получить текущее состояние загрузок моделей с конкретного cppworker'а
         * по прямому baseUrl (когда мы работаем с backend напрямую, без прокси).
         *
         * GET /api/models/load/progress?model=<name>
         *  - без query: возвращает все модели в loading-состоянии
         *  - с query: возвращает конкретную модель
         *
         * Ответ: { models: [{name, path, state, loadingStartedAt, loadingSizeBytes, elapsedMs, error}], backend: "..." }
         */
        async getLoadProgressAt(baseUrl, modelName) {
            const qs = modelName ? '?model=' + encodeURIComponent(modelName) : '';
            return this.requestAt(baseUrl, '/api/models/load/progress' + qs, { method: 'GET' });
        },

        /**
         * Получить состояние загрузок с cppworker'а через прокси балансировщика.
         * Используется GgufLoadProgress в WebUI (CORS-safe, работает в Docker).
         *
         * GET /api/v1/gguf/backends/{id}/proxy/api/models/load/progress?model=<name>
         */
        async getLoadProgressViaBackend(backendId, modelName) {
            const qs = modelName ? '?model=' + encodeURIComponent(modelName) : '';
            return this.requestViaBackend(backendId, '/api/models/load/progress' + qs);
        }
    };
})();
