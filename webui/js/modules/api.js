/**
 * API layer with centralized error handling and token auth
 */
const Api = (function () {
    const CFG = window.WEBUI_CONFIG || {};
    const API_BASE = CFG.API_BASE || '';

    // Internal: build fetch options with auth headers
    function getOptions(options = {}) {
        const headers = {
            'Content-Type': 'application/json',
            ...(options.headers || {})
        };
        if (CFG.API_TOKEN) {
            headers['X-API-Token'] = CFG.API_TOKEN;
        }
        return { ...options, headers };
    }

    // Internal: perform fetch with error handling
    async function request(url, options = {}) {
        try {
            const response = await fetch(url, getOptions(options));
            if (!response.ok) {
                const text = await response.text().catch(() => '');
                let body = text;
                try { body = JSON.parse(text); } catch {}
                const err = new Error(`HTTP ${response.status}: ${response.statusText}`);
                err.status = response.status;
                err.body = body;
                throw err;
            }
            return response;
        } catch (err) {
            // Network errors bubble up with message
            if (!err.status) {
                err.message = `Network error: ${err.message}`;
            }
            throw err;
        }
    }

    // Internal: generic GET returning JSON
    async function getJson(endpoint) {
        const response = await request(`${API_BASE}${endpoint}`);
        return response.json();
    }

    return {
        // Health check
        async health() {
            return getJson('/api/v1/health');
        },

        // Cluster state
        async cluster() {
            return getJson('/api/v1/cluster');
        },

        // Queue stats
        async queueStats() {
            return getJson('/api/v1/queue/stats');
        },

        // Queue details (pending tasks)
        async queueDetails() {
            return getJson('/api/v1/queue/details');
        },

        // Queue history (completed tasks)
        async queueHistory() {
            return getJson('/api/v1/queue/history');
        },

        // Sessions
        async sessions() {
            return getJson('/api/v1/sessions');
        },

        // Cluster config (settings)
        async config() {
            return getJson('/api/v1/cluster/config');
        },

        // Backend CRUD
        async createBackend(data) {
            const response = await request(`${API_BASE}/api/v1/backends`, {
                method: 'POST',
                body: JSON.stringify(data)
            });
            return response.ok;
        },

        async updateBackend(id, data) {
            const response = await request(`${API_BASE}/api/v1/backends/${id}`, {
                method: 'PUT',
                body: JSON.stringify(data)
            });
            return response.ok;
        },

        async deleteBackend(id) {
            const response = await request(`${API_BASE}/api/v1/backends/${id}`, {
                method: 'DELETE'
            });
            return response.ok;
        },

        async updateConfig(data) {
            const response = await request(`${API_BASE}/api/v1/cluster/config`, {
                method: 'PUT',
                body: JSON.stringify(data)
            });
            return response.ok;
        },

        async updateBackendLimits(id, maxConcurrentRequests) {
            const response = await request(`${API_BASE}/api/v1/backends/${id}/limits`, {
                method: 'PUT',
                body: JSON.stringify({ maxConcurrentRequests })
            });
            return response.ok;
        },

        // Get single backend config (with weight, maxConcurrentReqs etc.)
        async getBackend(id) {
            return getJson(`/api/v1/backends/${id}`);
        },

        async updateBackendLimitsFull(id, maxConcurrentRequests, maxModels) {
            const response = await request(`${API_BASE}/api/v1/backends/${id}/limits`, {
                method: 'PUT',
                body: JSON.stringify({ maxConcurrentRequests, maxModels })
            });
            return response.ok;
        },

        // Generic POST helper returning JSON
        async post(endpoint, data) {
            const response = await request(`${API_BASE}${endpoint}`, {
                method: 'POST',
                ...(data ? { body: JSON.stringify(data) } : {})
            });
            if (response.status === 204 || response.headers.get('content-length') === '0') return null;
            const ct = response.headers.get('content-type') || '';
            return ct.includes('application/json') ? response.json() : response.text();
        },

        // Proxy logs
        async proxyLogs(limit = 50) {
            return getJson(`/api/v1/proxy/logs?limit=${limit}`);
        },

        // Model Management API
        async backendModels(id) {
            return getJson(`/api/v1/backends/${id}/models`);
        },

        async backendModelOperation(id, operation, modelName, options = {}) {
            const payload = { operation, modelName, ...options };
            const response = await request(`${API_BASE}/api/v1/backends/${id}/models`, {
                method: 'POST',
                body: JSON.stringify(payload)
            });
            return response.json();
        },

        async modelOperationsStatus() {
            return getJson('/api/v1/models/operations');
        },

        // Agents API
        async agentsStats() {
            return getJson('/api/v1/agents/stats');
        },

        async agentInfo(id) {
            return getJson('/api/v1/agents/' + encodeURIComponent(id));
        },

        async restartAgent(backendId) {
            const response = await request(`${API_BASE}/api/v1/agents/${encodeURIComponent(backendId)}/restart`, {
                method: 'POST'
            });
            return response.json();
        },

        async agentLogs(backendId, limit = 100) {
            return getJson(`/api/v1/agents/${encodeURIComponent(backendId)}/logs?limit=${limit}`);
        },

        // Config reset to defaults
        async resetConfig() {
            const response = await request(`${API_BASE}/api/v1/config/reset`, {
                method: 'POST'
            });
            return response.json();
        },

        // GGUF backends info
        async fetchGgufBackends() {
            return getJson('/api/v1/gguf/backends');
        },

        // ===== Per-Model Profiles (Q3 W3-4 — Session 15, cppworker-params.js) =====
        // Управляются балансировщиком (НЕ отдельным cppworker'ом): список профилей
        // общий для всех llama_cpp бэкендов. Каждый профиль — это набор параметров
        // загрузки (n_ctx, batch_size, gpu_layers и т.д.), применяемый в 3-tier
        // resolver'е между per-request body и per-backend default. Endpoint POST
        // .../apply дополнительно делает reload модели на бэкендах, где она уже
        // загружена (n_ctx immutable после LoadModel).
        cppworkerModelProfiles: {
            /**
             * GET /api/v1/cppworker/model-profiles — список всех профилей.
             * @returns {Promise<{models: Object<string, LlamaCppModelProfile>, total: number}>}
             */
            async list() {
                return getJson('/api/v1/cppworker/model-profiles');
            },

            /**
             * GET /api/v1/cppworker/model-profiles/{name} — один профиль.
             * @param {string} modelName — имя модели (например, "gemma-4-E4B-it-Q4_K_M").
             * @returns {Promise<{model: string, profile: LlamaCppModelProfile}>}
             * @throws Error со status=404 если профиль не найден.
             */
            async get(modelName) {
                return getJson('/api/v1/cppworker/model-profiles/' + encodeURIComponent(modelName));
            },

            /**
             * PUT /api/v1/cppworker/model-profiles/{name} — создать или обновить.
             * @param {string} modelName — имя модели.
             * @param {Object} profile — {contextLength, batchSize, numGpuLayers, flashAttn?, numa?, useMmap?, notes?, streamingTimeoutSec?, ...}.
             * @returns {Promise<Object>}
             */
            async upsert(modelName, profile) {
                const response = await request(`${API_BASE}/api/v1/cppworker/model-profiles/${encodeURIComponent(modelName)}`, {
                    method: 'PUT',
                    body: JSON.stringify(profile)
                });
                return response.json();
            },

            /**
             * DELETE /api/v1/cppworker/model-profiles/{name} — удалить профиль.
             * @param {string} modelName — имя модели.
             * @returns {Promise<Object>}
             */
            async remove(modelName) {
                const response = await request(`${API_BASE}/api/v1/cppworker/model-profiles/${encodeURIComponent(modelName)}`, {
                    method: 'DELETE'
                });
                return response.json();
            },

            /**
             * POST /api/v1/cppworker/model-profiles/{name}/apply — save + reload
             * модели на всех llama_cpp бэкендах, где она загружена.
             * @param {string} modelName — имя модели.
             * @param {Object} [profile] — опциональное частичное обновление профиля (merge с существующим).
             * @returns {Promise<Object>} — синхронный ответ 200 (все бэкенды idle),
             *                                ИЛИ 202 + {applyId, progressUrl, ...} (async path при занятом бэкенде).
             */
            async apply(modelName, profile) {
                const options = { method: 'POST' };
                if (profile && Object.keys(profile).length > 0) {
                    options.body = JSON.stringify(profile);
                }
                const response = await fetch(
                    `${API_BASE}/api/v1/cppworker/model-profiles/${encodeURIComponent(modelName)}/apply`,
                    {
                        method: 'POST',
                        credentials: 'include',
                        headers: Object.assign(
                            { 'Content-Type': 'application/json' },
                            request._getAuthHeaders ? request._getAuthHeaders() : {}
                        ),
                        body: options.body || null
                    }
                );
                const data = await response.json();
                // Attach status to result so caller can distinguish sync (200) vs async (202)
                data._status = response.status;
                return data;
            }
        },

        // ===== Active Queries (Round 26 v0.5.13) =====
        // Используется для busy badge в WebUI: показывает, генерирует ли модель
        // ответ прямо сейчас. Помогает UX — пользователь видит, что apply нужно
        // подождать, а не сидеть с disabled формой "Save".
        cppworkerActiveQueries: {
            /**
             * GET /api/v1/gguf/backends/{id}/proxy/api/models/active-queries?model=<name>
             * Возвращает {model, activeQueries: N} для конкретной модели на конкретном бэкенде.
             * @param {string} backendId — ID cppworker'а.
             * @param {string} modelName — имя модели.
             * @returns {Promise<{model: string, activeQueries: number}>}
             */
            async get(backendId, modelName) {
                // Используем существующий proxyToCppWorker (GgufApi), не идём напрямую.
                if (!window.GgufApi || !window.GgufApi.requestViaBackend) {
                    throw new Error('GgufApi.requestViaBackend not available');
                }
                const path = `/api/models/active-queries?model=${encodeURIComponent(modelName)}`;
                const data = await window.GgufApi.requestViaBackend(backendId, path);
                return { model: data.model || modelName, activeQueries: data.activeQueries || 0 };
            }
        },

        // ===== Cancel Active Generation (Round 32 #2) =====
        // Отменяет активную inference-генерацию для конкретной модели.
        // Используется кнопкой "Cancel" на busy badge в WebUI (см. gguf-renderer.js).
        // cppworker AbortWatcher → bridge.RequestAbort → C-bridge прерывает
        // текущий llama_decode при следующей проверке abort флага (каждые
        // ~100ms после Round 32 n_batch=64).
        //
        // POST /api/cancel
        // Body: {"model": "...", "request_id": "..."} (request_id опционален)
        // Response: {"cancelled": N, "by": "model|user_id|all", "request_id": "..."}
        cppworkerCancelGeneration: {
            /**
             * POST /api/v1/gguf/backends/{id}/proxy/api/cancel
             * Отменяет активные генерации для указанной модели.
             * @param {string} backendId — ID cppworker'а.
             * @param {string} modelName — имя модели.
             * @param {string} [userId] — опционально, отменять только запросы конкретного user.
             * @returns {Promise<{cancelled: number, by: string, request_id: string}>}
             */
            async post(backendId, modelName, userId) {
                if (!window.GgufApi || !window.GgufApi.requestViaBackend) {
                    throw new Error('GgufApi.requestViaBackend not available');
                }
                const body = { model: modelName };
                if (userId) body.user_id = userId;
                const data = await window.GgufApi.requestViaBackend(backendId, '/api/cancel', {
                    method: 'POST',
                    body: JSON.stringify(body),
                });
                return {
                    cancelled: data.cancelled || 0,
                    by: data.by || 'model',
                    request_id: data.request_id || '',
                };
            }
        },

        // ===== Apply Profile Progress (Round 26 v0.5.13) =====
        // Async apply + SSE progress. handleAsyncApply возвращает 202 + applyId,
        // и UI подписывается на /api/v1/cppworker/model-profiles/{name}/apply/progress.
        cppworkerApplyProgress: {
            /**
             * GET /api/v1/cppworker/model-profiles/{name}/apply/progress?applyId=X
             * EventSource-совместимый SSE endpoint. Возвращает EventSource.
             * @param {string} modelName — имя модели.
             * @param {string} applyId — ID async apply job'а.
             * @param {Object} callbacks — {onProgress, onComplete, onError}.
             * @returns {EventSource}
             */
            streamProgress(modelName, applyId, callbacks) {
                if (typeof EventSource === 'undefined') {
                    // Fallback для старых браузеров
                    throw new Error('EventSource not supported');
                }
                const url = `${API_BASE}/api/v1/cppworker/model-profiles/${encodeURIComponent(modelName)}/apply/progress?applyId=${encodeURIComponent(applyId)}`;
                const es = new EventSource(url, { withCredentials: true });
                es.addEventListener('progress', (e) => {
                    try {
                        const data = JSON.parse(e.data);
                        if (callbacks.onProgress) callbacks.onProgress(data);
                    } catch (err) { /* ignore */ }
                });
                es.addEventListener('complete', (e) => {
                    try {
                        const data = JSON.parse(e.data);
                        if (callbacks.onComplete) callbacks.onComplete(data);
                    } catch (err) { /* ignore */ }
                    es.close();
                });
                es.onerror = (e) => {
                    if (callbacks.onError) callbacks.onError(e);
                    // EventSource auto-reconnects — close explicitly to stop
                    es.close();
                };
                return es;
            },

            /**
             * GET /api/v1/cppworker/model-profiles/{name}/apply/status/{applyId}
             * Одноразовый JSON snapshot. Для fallback если SSE не работает.
             * @param {string} modelName — имя модели.
             * @param {string} applyId — ID async apply job'а.
             * @returns {Promise<Object>}
             */
            async getStatus(modelName, applyId) {
                return getJson(`/api/v1/cppworker/model-profiles/${encodeURIComponent(modelName)}/apply/status/${encodeURIComponent(applyId)}`);
            },

            /**
             * Polling fallback: опрашивает /status каждые 1s пока terminal.
             * @param {string} modelName — имя модели.
             * @param {string} applyId — ID async apply job'а.
             * @param {number} [maxWaitMs=300000] — max 5 минут.
             * @returns {Promise<Object>}
             */
            async pollStatus(modelName, applyId, maxWaitMs = 300000) {
                const start = Date.now();
                while (Date.now() - start < maxWaitMs) {
                    const status = await this.getStatus(modelName, applyId);
                    if (status.terminal) return status;
                    await new Promise(r => setTimeout(r, 1000));
                }
                throw new Error('pollStatus: timeout after ' + maxWaitMs + 'ms');
            }
        },

        // ===== Cluster Model Management (Q3 W4 — Session 19, model details panel) =====
        // Управление моделями на уровне кластера через балансировщик (порт 18081),
        // без прямого обращения к каждому cppworker. Endpoint'ы:
        //   GET  /api/v1/cluster/models/loaded                    — список загруженных
        //   GET  /api/v1/cluster/models/loading                   — список загружающихся
        //   GET  /api/v1/cluster/models/{name}/info               — детали модели (/api/show)
        //   POST /api/v1/cluster/models/{name}/reload              — load/unload/reload
        clusterModels: {
            /**
             * GET /api/v1/cluster/models/loaded.
             * @returns {Promise<{count: number, models: Array, modelsPerBackend?: Object<string, number>}>}
             */
            async loaded() {
                return getJson('/api/v1/cluster/models/loaded');
            },

            /**
             * GET /api/v1/cluster/models/loading.
             * @returns {Promise<{count: number, models: Array}>}
             */
            async loading() {
                return getJson('/api/v1/cluster/models/loading');
            },

            /**
             * GET /api/v1/cluster/models/{name}/info.
             * Проксирует Ollama /api/show на каждый бэкенд в кластере и возвращает
             * агрегированный результат (details.family, details.format, details.parameter_size,
             * details.quantization_level, model_info.architecture, model_info.n_layers,
             * model_info.n_embd, model_info.context_size, model_info.gpu_layers, model_info.state).
             *
             * @param {string} modelName — имя модели (например, "gemma-4-E4B-it-Q4_K_M").
             * @returns {Promise<{model: string, count: number, okCount: number, backends: Array<{
             *     backendId: string,
             *     backendType: string,
             *     status: "ok" | "not_found" | "error" | "unavailable",
             *     info?: Object,
             *     error?: string,
             *     httpStatus?: number
             * }>}>}
             */
            async info(modelName) {
                return getJson('/api/v1/cluster/models/' + encodeURIComponent(modelName) + '/info');
            },

            /**
             * POST /api/v1/cluster/models/{name}/reload.
             * @param {string} modelName — имя модели.
             * @param {Object} [options] — { operation?, backendId?, contextSize?, gpuLayers?, reason?, ... }.
             * @returns {Promise<{model: string, operation: string, results: Array}>}
             */
            async reload(modelName, options = {}) {
                const response = await request(
                    `${API_BASE}/api/v1/cluster/models/${encodeURIComponent(modelName)}/reload`,
                    {
                        method: 'POST',
                        body: JSON.stringify(options)
                    }
                );
                return response.json();
            },

            /**
             * POST /api/v1/cluster/models/bulk — массовая операция над списком моделей.
             *
             * Используется для UI-операций Load/Unload Selected (после multi-select
             * в моделях — см. webui/js/modules/bulk-models.js). Для "unload" без явного
             * списка `models` сервер сам подберёт все загруженные модели из cluster state.
             *
             * @param {Object} body — { operation, models: [{model, operation?, backendId?, contextSize?, gpuLayers?}], backendId?, contextSize?, gpuLayers?, reason? }
             * @returns {Promise<{
             *     operation: string,
             *     total: number,
             *     succeeded: number,
             *     failed: number,
             *     results: Array<{model, operation, succeeded, results: Array<{backendId, status, message?, error?, httpStatus?}>}>,
             *     startedAt: string,  // RFC3339Nano
             *     durationMs: number
             * }>}
             */
            async bulk(body) {
                const response = await request(`${API_BASE}/api/v1/cluster/models/bulk`, {
                    method: 'POST',
                    body: JSON.stringify(body)
                });
                return response.json();
            }
        },

        // Generic error handler for UI

        handleError(err, fallbackMessage = 'Ошибка API') {
            console.error(err);
            const msg = err?.message || fallbackMessage;
            window.dispatchEvent(new CustomEvent('api-error', { detail: { message: msg, error: err } }));
            return msg;
        },

        // ===== Session 17 (2026-07-27): runtime overrides =====
        // Проверяет, есть ли активный sidecar override для llamaCpp-секции.
        // Используется для badge "Runtime overrides active" в UI.
        // @returns {Promise<{exists: boolean, enabled: boolean, config?: LlamaCppConfig}>}
        async getLlamaCppOverride() {
            const response = await request(`${API_BASE}/api/v1/cluster/llama-cpp/overrides`, {
                method: 'GET',
            });
            return response.json();
        },

        // Сбрасывает override (Reset to bundled defaults). In-memory state
        // восстанавливается к base значениям из config.json — restart не нужен.
        // @returns {Promise<{status: string, existed_before: boolean}>}
        async deleteLlamaCppOverride() {
            const response = await request(`${API_BASE}/api/v1/cluster/llama-cpp/overrides`, {
                method: 'DELETE',
            });
            return response.json();
        }
    };
})();

// Session 17 P.9 (2026-07-27): `const Api = ...` был локальной переменной IIFE.
// Другие модули (cppworker-params.js, setup-wizard.js) используют `window.Api.*`,
// но window.Api никогда не присваивался → "Cannot read properties of undefined
// (reading 'list')" в Per-Model Profile UI. Фикс: экспортируем в window.
if (typeof window !== 'undefined') {
    window.Api = Api;
}
