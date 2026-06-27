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
             * @returns {Promise<{model: string, profile: LlamaCppModelProfile, backends: Array<{backendId: string, status: string, message?: string}>}>}
             */
            async apply(modelName, profile) {
                const options = { method: 'POST' };
                if (profile && Object.keys(profile).length > 0) {
                    options.body = JSON.stringify(profile);
                }
                const response = await request(
                    `${API_BASE}/api/v1/cppworker/model-profiles/${encodeURIComponent(modelName)}/apply`,
                    options
                );
                return response.json();
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
        }
    };
})();
