// Package api — handlers для per-model profiles (Шаг 5 cppworker-preflight-nctx-session).
//
// CRUD endpoints для per-model profile, который определяет n_ctx (и другие
// параметры cppworker) по умолчанию для конкретной модели. Применяется в
// 3-tier resolver'е (Шаг 2) между per-request body и per-backend default.
//
// API:
//
//	GET    /api/v1/cppworker/model-profiles                       — список всех профилей
//	GET    /api/v1/cppworker/model-profiles/{name}                — профиль для одной модели
//	PUT    /api/v1/cppworker/model-profiles/{name}                — upsert профиль + save
//	DELETE /api/v1/cppworker/model-profiles/{name}                — удалить профиль
//	POST   /api/v1/cppworker/model-profiles/{name}/apply         — save + reload на всех бэкендах
//
// PUT и POST persist через s.configSaver() (config.json на диск).
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// modelProfileResponse — DTO для ответа (выделяем из config.LlamaCppModelProfiles).
type modelProfileResponse struct {
	Models map[string]types.LlamaCppModelProfile `json:"models"`
	Total  int                                   `json:"total"`
}

// modelProfileApplyResponse — результат apply (reload на бэкендах).
type modelProfileApplyResponse struct {
	Model    string                       `json:"model"`
	Profile  types.LlamaCppModelProfile   `json:"profile"`
	Backends []modelProfileApplyBackendResult `json:"backends"`
}

// modelProfileApplyBackendResult — результат reload на одном бэкенде.
type modelProfileApplyBackendResult struct {
	BackendID string `json:"backendId"`
	Status    string `json:"status"`            // "reloaded" | "skipped" | "error"
	Message   string `json:"message,omitempty"` // детали ошибки или причина skip
}

// handleListModelProfiles — GET /api/v1/cppworker/model-profiles.
//
// Возвращает все профили в отсортированном виде (по имени модели).
func (s *Server) handleListModelProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	models := s.config.LlamaCppModelProfiles
	if models == nil {
		models = make(map[string]types.LlamaCppModelProfile)
	}

	s.writeJSON(w, http.StatusOK, modelProfileResponse{
		Models: models,
		Total:  len(models),
	})
}

// handleModelProfile — диспетчер для /api/v1/cppworker/model-profiles/{name}.
//
// Поддерживает:
//   - GET    /api/v1/cppworker/model-profiles/{name}                → getModelProfile
//   - PUT    /api/v1/cppworker/model-profiles/{name}                → upsertModelProfile
//   - DELETE /api/v1/cppworker/model-profiles/{name}                → deleteModelProfile
//   - POST   /api/v1/cppworker/model-profiles/{name}/apply         → applyModelProfile
func (s *Server) handleModelProfile(w http.ResponseWriter, r *http.Request) {
	// Извлечение имени модели и подпути из URL.
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/v1/cppworker/model-profiles/")
	if trimmed == "" || trimmed == r.URL.Path {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model name required"})
		return
	}

	parts := strings.SplitN(trimmed, "/", 2)
	modelName := parts[0]
	// sub может быть пустым, "apply" и т.п.
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}

	switch {
	case sub == "" && r.Method == http.MethodGet:
		s.getModelProfile(w, r, modelName)
	case sub == "" && r.Method == http.MethodPut:
		s.upsertModelProfile(w, r, modelName)
	case sub == "" && r.Method == http.MethodDelete:
		s.deleteModelProfile(w, r, modelName)
	case sub == "apply" && r.Method == http.MethodPost:
		s.applyModelProfile(w, r, modelName)
	default:
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// getModelProfile — возвращает профиль для одной модели.
// 404 если профиля нет.
func (s *Server) getModelProfile(w http.ResponseWriter, _ *http.Request, modelName string) {
	profile, ok := s.proxy.GetModelProfile(modelName)
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "profile not found",
			"message": fmt.Sprintf("no profile configured for model %q", modelName),
			"model":   modelName,
		})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"model":   modelName,
		"profile": profile,
	})
}

// upsertModelProfile — PUT /api/v1/cppworker/model-profiles/{name}.
//
// Принимает JSON с полями profile, валидирует и сохраняет в config.
// Body: {"contextLength": N, "batchSize": N, "numGpuLayers": N, ...}
func (s *Server) upsertModelProfile(w http.ResponseWriter, r *http.Request, modelName string) {
	log := logger.Get()

	if modelName == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model name required"})
		return
	}

	// Ограничим размер body чтобы не класть большие файлы
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)

	var incoming types.LlamaCppModelProfile
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&incoming); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid JSON body",
			"message": err.Error(),
		})
		return
	}

	// Валидация (балансировщик и cppworker имеют одинаковые границы)
	if err := validateModelProfile(incoming); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid profile",
			"message": err.Error(),
		})
		return
	}

	// Upsert в config (in-memory)
	s.proxy.SetModelProfile(modelName, incoming)

	// Persist на диск
	if s.configSaver != nil {
		if err := s.configSaver(); err != nil {
			log.Errorw("upsertModelProfile: failed to save config to disk",
				"model", modelName, "error", err)
			// Не откатываем in-memory изменение — но сообщаем warning
			s.writeJSON(w, http.StatusOK, map[string]interface{}{
				"status":   "ok",
				"model":    modelName,
				"profile":  incoming,
				"warning":  "profile saved in memory, but config.json write failed: " + err.Error(),
			})
			return
		}
	} else {
		log.Warnw("upsertModelProfile: configSaver is nil — profile will NOT be persisted to disk",
			"model", modelName)
	}

	log.Infow("model profile upserted", "model", modelName, "contextLength", incoming.ContextLength)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"model":   modelName,
		"profile": incoming,
	})
}

// deleteModelProfile — DELETE /api/v1/cppworker/model-profiles/{name}.
func (s *Server) deleteModelProfile(w http.ResponseWriter, r *http.Request, modelName string) {
	log := logger.Get()

	if modelName == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model name required"})
		return
	}

	existed := s.proxy.DeleteModelProfile(modelName)
	if !existed {
		s.writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "profile not found",
			"message": fmt.Sprintf("no profile configured for model %q", modelName),
			"model":   modelName,
		})
		return
	}

	if s.configSaver != nil {
		if err := s.configSaver(); err != nil {
			log.Errorw("deleteModelProfile: failed to save config to disk",
				"model", modelName, "error", err)
			s.writeJSON(w, http.StatusOK, map[string]interface{}{
				"status":  "ok",
				"model":   modelName,
				"warning": "profile deleted in memory, but config.json write failed: " + err.Error(),
			})
			return
		}
	} else {
		log.Warnw("deleteModelProfile: configSaver is nil — profile will NOT be persisted to disk",
			"model", modelName)
	}

	log.Infow("model profile deleted", "model", modelName)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "ok",
		"model":  modelName,
	})
}

// applyModelProfile — POST /api/v1/cppworker/model-profiles/{name}/apply.
//
// Семантика:
//  1. Upsert профиля в config (если body содержит поля — мержим).
//  2. Reload (unload + load) модели на всех llama_cpp бэкендах, где она
//     загружена. Это критично, потому что n_ctx immutable после загрузки.
//
// Body (опционально): {"contextLength": N, "batchSize": N, ...} —
// можно комбинировать с существующим профилем. Если body отсутствует —
// используется текущий сохранённый профиль.
//
// Reload делается через cppworker endpoint POST /api/models/reload.
// Если модель не загружена ни на одном бэкенде — это не ошибка, просто
// в ответе будет "skipped" с reason "not loaded".
func (s *Server) applyModelProfile(w http.ResponseWriter, r *http.Request, modelName string) {
	log := logger.Get()

	if modelName == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model name required"})
		return
	}

	// Шаг 1: прочитать body (опционально) и смержить с текущим профилем
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var bodyUpdate types.LlamaCppModelProfile
	hasBody := false
	if r.ContentLength != 0 {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&bodyUpdate); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "invalid JSON body",
				"message": err.Error(),
			})
			return
		}
		hasBody = true
	}

	// Получаем текущий профиль (если есть)
	currentProfile, hasCurrent := s.proxy.GetModelProfile(modelName)
	merged := currentProfile
	if hasBody {
		merged = mergeModelProfile(currentProfile, bodyUpdate)
	}

	if !hasCurrent && !hasBody {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "no profile to apply",
			"message": "profile does not exist and no body provided to create one",
			"model":   modelName,
		})
		return
	}

	if err := validateModelProfile(merged); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid profile",
			"message": err.Error(),
		})
		return
	}

	// Шаг 2: сохранить merged профиль в config
	s.proxy.SetModelProfile(modelName, merged)
	if s.configSaver != nil {
		if err := s.configSaver(); err != nil {
			log.Errorw("applyModelProfile: failed to save config to disk",
				"model", modelName, "error", err)
		}
	}

	// Шаг 3: reload на всех llama_cpp бэкендах.
	// ВАЖНО: используем live backends из proxy (GetAllBackends), а не static config.
	// В bundled-режиме config.Backends пуст — cppworker регистрируется через
	// auto-registration, и его backend появляется только в runtime state proxy.
	// Если итерировать по s.config.Backends — apply всегда возвращает "skipped".
	results := make([]modelProfileApplyBackendResult, 0)
	for _, backend := range s.proxy.GetAllBackends() {
		backendID := backend.ID
		if backend.Type != types.BackendTypeLlamaCpp {
			results = append(results, modelProfileApplyBackendResult{
				BackendID: backendID,
				Status:    "skipped",
				Message:   "not a llama_cpp backend",
			})
			continue
		}

		// Проверяем, загружена ли модель на этом бэкенде
		if !s.proxy.IsLlamaCppModelLoaded(backendID, modelName) {
			results = append(results, modelProfileApplyBackendResult{
				BackendID: backendID,
				Status:    "skipped",
				Message:   "model not currently loaded on this backend",
			})
			continue
		}

		// Reload через cppworker
		if err := s.reloadModelOnCppWorker(backend, modelName, merged); err != nil {
			log.Warnw("applyModelProfile: reload failed on backend",
				"backend", backendID, "model", modelName, "error", err)
			results = append(results, modelProfileApplyBackendResult{
				BackendID: backendID,
				Status:    "error",
				Message:   err.Error(),
			})
			continue
		}
		results = append(results, modelProfileApplyBackendResult{
			BackendID: backendID,
			Status:    "reloaded",
		})
	}

	// Сортируем результаты по backendID для детерминированного ответа
	sort.Slice(results, func(i, j int) bool {
		return results[i].BackendID < results[j].BackendID
	})

	log.Infow("model profile applied",
		"model", modelName,
		"contextLength", merged.ContextLength,
		"backends", len(results))

	s.writeJSON(w, http.StatusOK, modelProfileApplyResponse{
		Model:    modelName,
		Profile:  merged,
		Backends: results,
	})
}

// reloadModelOnCppWorker — POST /api/models/reload на конкретном бэкенде.
// Принимает modelName и profile. Применяет contextLength/batchSize/numGpuLayers
// к cppworker при reload.
func (s *Server) reloadModelOnCppWorker(backend types.Backend, modelName string, profile types.LlamaCppModelProfile) error {
	host := backend.Host
	port := backend.CppWorkerPort
	if port <= 0 {
		port = 18092
	}
	target := fmt.Sprintf("http://%s:%d/api/models/reload", host, port)

	body := map[string]interface{}{
		"name":          modelName,
		"contextSize":   profile.ContextLength,
		"batchSize":     profile.BatchSize,
		"numGpuLayers":  profile.NumGPULayers,
	}
	if profile.FlashAttn != nil {
		body["flashAttn"] = *profile.FlashAttn
	}
	if profile.NUMA != nil {
		body["numa"] = *profile.NUMA
	}
	if profile.UseMmap != nil {
		body["useMmap"] = *profile.UseMmap
	}
	// Session 16 (2026-06-27): прокидываем Parallel + KVCacheType в cppworker
	// reload endpoint. Cppworker принимает их в /api/models/reload body
	// (см. cmd/cppworker/handlers_model.go — handleLoadWithParams).
	// cppworker defaults применяются, если поля не переданы.
	if profile.Parallel > 0 {
		body["parallel"] = profile.Parallel
	}
	if profile.KVCacheType != "" {
		body["kvCacheType"] = profile.KVCacheType
	}
	// Round 7 (2026-07-09): прокидываем override-tensors в reload endpoint.
	// Применяется к MoE моделям для роутинга routed-expert тензоров на CPU
	// (освобождает VRAM для KV-cache). Параллельные массивы должны быть
	// согласованы по длине, иначе cppworker их игнорирует.
	if len(profile.OverrideTensors) > 0 && len(profile.OverrideTensors) == len(profile.OverrideTensorBufts) {
		body["overrideTensors"] = profile.OverrideTensors
		body["overrideTensorBufts"] = profile.OverrideTensorBufts
	}
	bodyBytes, _ := json.Marshal(body)

	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return fmt.Errorf("build reload request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Real-IP", "balancer")
	// Round 7 fix: cppworker /api/models/reload защищён authMiddleware,
	// который ожидает `Authorization: Bearer <token>`. Per-backend token берём
	// из backend.CppWorkerApiToken (если задан в конфиге). Если пусто — шлём
	// без Authorization (cppworker пропустит без authMiddleware).
	if backend.CppWorkerApiToken != "" {
		req.Header.Set("Authorization", "Bearer "+backend.CppWorkerApiToken)
	}

	httpClient := &http.Client{}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("reload request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf := make([]byte, 2048)
		n, _ := resp.Body.Read(buf)
		return fmt.Errorf("cppworker reload returned %d: %s", resp.StatusCode, string(buf[:n]))
	}
	return nil
}

// mergeModelProfile — мерж body update поверх existing. Zero-value поля
// в update НЕ перезаписывают поля в existing (это позволяет PATCH-like
// частичные обновления).
func mergeModelProfile(existing, update types.LlamaCppModelProfile) types.LlamaCppModelProfile {
	out := existing
	if update.ContextLength != 0 {
		out.ContextLength = update.ContextLength
	}
	if update.BatchSize != 0 {
		out.BatchSize = update.BatchSize
	}
	if update.NumGPULayers != 0 {
		out.NumGPULayers = update.NumGPULayers
	}
	if update.FlashAttn != nil {
		v := *update.FlashAttn
		out.FlashAttn = &v
	}
	if update.NUMA != nil {
		v := *update.NUMA
		out.NUMA = &v
	}
	if update.UseMmap != nil {
		v := *update.UseMmap
		out.UseMmap = &v
	}
	if update.Notes != "" {
		out.Notes = update.Notes
	}
	// Session 16 (2026-06-27): Parallel + KVCacheType.
	// zero-value (Parallel=0 или KVCacheType="") означает "не менять",
	// аналогично NumGPULayers/Notes. Это позволяет PATCH-like частичные
	// обновления без сброса уже настроенного parallel/kv.
	if update.Parallel != 0 {
		out.Parallel = update.Parallel
	}
	if update.KVCacheType != "" {
		out.KVCacheType = update.KVCacheType
	}
	// Round 7 (2026-07-09): override-tensors (parallel arrays).
	// nil в update означает "не менять"; [] в update (явно пустой массив)
	// означает "очистить". Непустой update перезаписывает целиком.
	// Должны быть согласованы по длине, иначе — игнорируем (warning в логе).
	if update.OverrideTensors != nil || update.OverrideTensorBufts != nil {
		if len(update.OverrideTensors) == len(update.OverrideTensorBufts) {
			out.OverrideTensors = update.OverrideTensors
			out.OverrideTensorBufts = update.OverrideTensorBufts
		} else {
			logger.Get().Warnw("mergeModelProfile: override-tensors length mismatch, keeping existing",
				"tensors", len(update.OverrideTensors),
				"bufts", len(update.OverrideTensorBufts))
		}
	}
	return out
}

// validateModelProfile — валидация профиля перед сохранением.
// Делегирует balancer.ValidateProfile (single source of truth), но также
// проверяет что contextLength задан (иначе профиль бесполезен для n_ctx).
func validateModelProfile(p types.LlamaCppModelProfile) error {
	if p.ContextLength <= 0 {
		return fmt.Errorf("contextLength is required and must be > 0")
	}
	if p.ContextLength < 256 || p.ContextLength > 262144 {
		return fmt.Errorf("contextLength must be in [256, 262144], got %d", p.ContextLength)
	}
	if p.BatchSize != 0 && p.BatchSize < 1 {
		return fmt.Errorf("batchSize must be >= 1, got %d", p.BatchSize)
	}
	if p.NumGPULayers < -1 {
		return fmt.Errorf("numGpuLayers must be >= -1 (-1 = all layers), got %d", p.NumGPULayers)
	}
	if p.NumGPULayers > 200 {
		return fmt.Errorf("numGpuLayers must be <= 200, got %d", p.NumGPULayers)
	}
	// Session 16 (2026-06-27): валидация Parallel и KVCacheType.
	// Parallel ∈ [0, 8] — cppworker не поддерживает > 8 параллельных слотов.
	// KVCacheType ∈ {"", "f16", "q8_0", "q4_0"} — только эти 4 значения валидны.
	if p.Parallel < 0 || p.Parallel > 8 {
		return fmt.Errorf("parallel must be in [0, 8], got %d", p.Parallel)
	}
	switch p.KVCacheType {
	case "", "f16", "q8_0", "q4_0":
		// OK
	default:
		return fmt.Errorf("kvCacheType must be one of ['', 'f16', 'q8_0', 'q4_0'], got %q", p.KVCacheType)
	}
	// Round 7 (2026-07-09): validate override-tensors.
	// Длины должны совпадать (parallel arrays). Каждый buft должен быть
	// одним из поддерживаемых значений (CPU / CUDA<N>).
	// Пустые массивы — допустимы (= inherit default, без override).
	if len(p.OverrideTensors) != len(p.OverrideTensorBufts) {
		return fmt.Errorf("overrideTensors and overrideTensorBufts length mismatch: %d vs %d",
			len(p.OverrideTensors), len(p.OverrideTensorBufts))
	}
	for i, buft := range p.OverrideTensorBufts {
		if buft != "CPU" && !strings.HasPrefix(buft, "CUDA") {
			return fmt.Errorf("overrideTensorBufts[%d]=%q is invalid (must be 'CPU' or 'CUDA<n>')", i, buft)
		}
	}
	return nil
}
