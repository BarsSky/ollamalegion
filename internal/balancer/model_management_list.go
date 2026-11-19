// model_management_list.go — R632 (2026-11-19): операции над списком моделей: ExecuteOperation, ListModels, listOllamaModels, listLlamaCppModels.
//
// ЗАЧЕМ ВЫНЕСЕНО. `model_management.go` держал 2172 строки: конструктор,
// операции, ожидание загрузки, отдельный 447-строчный цикл загрузки llama.cpp и
// общий транспорт. Здесь — один из слоёв; поведение не менялось, код перенесён как есть.
package balancer

import (
	"encoding/json"
	"fmt"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/types"
	"strings"
)

// ExecuteOperation — выполнение операции с моделью на указанном бэкенде
func (mm *ModelManager) ExecuteOperation(backendID string, req ModelOpRequest) *ModelOpResult {
	backend := mm.proxy.GetBackend(backendID)
	if backend == nil {
		return &ModelOpResult{
			Success:   false,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Error:     fmt.Sprintf("backend '%s' not found", backendID),
		}
	}

	host := backend.Host
	port := mm.resolveBackendPort(backend)
	isLlamaCpp := mm.isLlamaCppBackend(backend)

	// Проверяем, не выполняется ли уже такая операция
	if !mm.tryAcquireOp(req.Operation, req.ModelName, backendID) {
		return &ModelOpResult{
			Success:   false,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Error:     fmt.Sprintf("operation '%s' for model '%s' is already in progress on backend '%s'", req.Operation, req.ModelName, backendID),
		}
	}
	defer mm.releaseOp(req.Operation, req.ModelName, backendID)

	// Берём request_id из глобального fallback-context (ensureModelLoadedOnBackend
	// вызывает ExecuteOperation синхронно из HTTP-хендлера, и middleware ServeHTTP
	// уже положил request_id в lr_recentCtx). Если ctx пустой (вызов вне HTTP,
	// например из background-reload) — ridLog() вернёт обычный logger без полей.
	ridLog(lr_recentCtx()).Infow("executing model operation",
		"operation", req.Operation,
		"model", req.ModelName,
		"backend", backendID,
		"engine", string(types.ResolveEngine(backend.Engine, backend.Type)),
		"host", host,
		"port", port)

	var result *ModelOpResult
	op := strings.ToLower(req.Operation)

	// R292 (2026-10-29): ЕДИНЫЙ guard перед разрушительной операцией с моделью.
	//
	// Живой дефект (второй стенд, два агента, одна модель): второй агент вырвал
	// модель у первого — первый получил «stream ended by upstream before
	// completion». Guard R271 стоял только на путях ПЕРЕЗАГРУЗКИ (n_ctx-preflight,
	// AutoTune, операторский reload), а этот путь — «рядовой» load/unload через
	// ModelManager, которым ходят автозагрузка по запросу клиента и API — его не
	// проверял. Для cppworker `load` с другими параметрами = unload+load, то есть
	// выгрузка модели под живым стримом.
	//
	// Порядок важен: guard ДО tryAcquireOp не нужен (dedup дешёвый), но guard
	// ОБЯЗАН стоять до фактического запроса в воркер — иначе воркер начнёт
	// выгружать модель, а мы уже ничего не отменим.
	// R304 (2026-10-30): донор не поднимает СВОИХ копий моделей.
	//
	// ЗАЧЕМ ЗДЕСЬ, А НЕ ТОЛЬКО В cppworker. Это единственная дверь, через которую
	// балансер заказывает загрузку у воркера: рядовой load через API, bulk-операции,
	// прогрев, восстановление предыдущей модели. Новый воркер ответит 409, но
	// СТАРЫЙ образ приказ примет и поднимет копию рядом с раскладкой мастера — то
	// есть в окне между обновлением балансера и воркера вернулся бы ровно тот
	// дефект, ради которого писалась правка. Отказ обязан быть и на нашей стороне.
	//
	// unload/delete осознанно НЕ блокируются: убрать нарушающую копию с донора —
	// законное и нужное действие (именно к нему призывает предупреждение в панели).
	if isLlamaCpp && op == "load" && mm.proxy.cannotHostModelR304(backendID) {
		return &ModelOpResult{
			Success:     false,
			Operation:   req.Operation,
			ModelName:   req.ModelName,
			BackendID:   backendID,
			RoleRefused: true,
			Error:       i18n.T("balancer.donor.load_refused", backendID),
		}
	}

	markedDestructiveR292 := false
	if isLlamaCpp && (op == "load" || op == "unload") {
		if derr := mm.proxy.guardModelOpR292(backendID, req.ModelName, op); derr != nil {
			return &ModelOpResult{
				Success:       false,
				Operation:     req.Operation,
				ModelName:     req.ModelName,
				BackendID:     backendID,
				Deferred:      true,
				RetryAfterSec: derr.RetrySec,
				Error:         derr.Error(),
			}
		}
		mm.proxy.noteDestructiveOpR271(backendID, req.ModelName, destructiveKindForModelOpR292(op))
		markedDestructiveR292 = true
	}

	// pull/push — только для ollama-бэкендов
	if op == "pull" || op == "push" {
		if isLlamaCpp {
			return &ModelOpResult{
				Success:   false,
				Operation: req.Operation,
				ModelName: req.ModelName,
				BackendID: backendID,
				Error:     fmt.Sprintf("operation '%s' is not supported for llama.cpp backends; use HF download instead", req.Operation),
			}
		}
	}

	switch op {
	case "pull":
		result = mm.executePull(host, port, backendID, req)
	case "push":
		result = mm.executePush(host, port, backendID, req)
	case "delete":
		if isLlamaCpp {
			result = mm.executeLlamaCppDelete(host, port, backendID, req)
		} else {
			result = mm.executeDelete(host, port, backendID, req)
		}
	case "load":
		if isLlamaCpp {
			result = mm.executeLlamaCppLoad(host, port, backendID, req)
		} else {
			result = mm.executeLoad(host, port, backendID, req)
		}
	case "unload":
		if isLlamaCpp {
			result = mm.executeLlamaCppUnload(host, port, backendID, req)
		} else {
			result = mm.executeUnload(host, port, backendID, req)
		}
	default:
		result = &ModelOpResult{
			Success:   false,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Error:     fmt.Sprintf("unsupported operation: %s", req.Operation),
		}
	}

	// R292: пометку «модель выгружают» снимаем, если операция НЕ применилась.
	// Иначе она «залипала» бы и следующий оборванный стрим обвинялся бы в том,
	// чего не было (ровно этот класс разбирали в R283).
	if markedDestructiveR292 && (result == nil || !result.Success) {
		mm.proxy.clearDestructiveOpR271(backendID, req.ModelName)
	}

	return result
}

// ListModels — получение списка моделей на бэкенде.
// Для ollama-бэкендов: /api/tags + /api/ps. Для llama.cpp-бэкендов:
// /api/models/files (список на диске) + /api/models/loaded (что в памяти).
func (mm *ModelManager) ListModels(backendID string) ([]ModelInfo, error) {
	backend := mm.proxy.GetBackend(backendID)
	if backend == nil {
		return nil, fmt.Errorf("backend '%s' not found", backendID)
	}

	if mm.isLlamaCppBackend(backend) {
		return mm.listLlamaCppModels(backend)
	}
	return mm.listOllamaModels(backend)
}

// listOllamaModels — список моделей Ollama-бэкенда через /api/tags + /api/ps.
func (mm *ModelManager) listOllamaModels(backend *types.Backend) ([]ModelInfo, error) {
	port := backend.OllamaPort
	tagsURL := fmt.Sprintf("http://%s:%d/api/tags", backend.Host, port)
	tagsResp, err := mm.client.Get(tagsURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch tags from backend %s: %w", backend.ID, err)
	}
	defer tagsResp.Body.Close()

	var tagsData struct {
		Models []struct {
			Name       string `json:"name"`
			Model      string `json:"model,omitempty"`
			Size       int64  `json:"size"`
			Digest     string `json:"digest"`
			ModifiedAt string `json:"modified_at"`
		} `json:"models"`
	}
	if err := json.NewDecoder(tagsResp.Body).Decode(&tagsData); err != nil {
		return nil, fmt.Errorf("failed to decode tags from backend %s: %w", backend.ID, err)
	}

	psURL := fmt.Sprintf("http://%s:%d/api/ps", backend.Host, port)
	loadedModels := make(map[string]bool)
	psResp, psErr := mm.client.Get(psURL)
	if psErr == nil {
		defer psResp.Body.Close()
		var psData struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if err := json.NewDecoder(psResp.Body).Decode(&psData); err == nil {
			for _, m := range psData.Models {
				loadedModels[m.Name] = true
			}
		}
	}

	models := make([]ModelInfo, 0, len(tagsData.Models))
	for _, t := range tagsData.Models {
		models = append(models, ModelInfo{
			Name:       t.Name,
			Model:      t.Model,
			Size:       t.Size,
			Digest:     t.Digest,
			ModifiedAt: t.ModifiedAt,
			Loaded:     loadedModels[t.Name],
		})
	}
	return models, nil
}

// listLlamaCppModels — список моделей cppworker-бэкенда: /api/models/files (на диске)
// + /api/models (что в памяти через state="loaded"). Round 17 (2026-07-10).
// Round 18 (2026-07-10): пробрасывает runtime поля (contextLength, numGpuLayers,
// quantization, ggufPath, batchSize, state, architecture) из /api/models response.
// Также использует loadingSizeBytes как fallback для size (cppworker reports sizeBytes=0
// для загруженных моделей).
func (mm *ModelManager) listLlamaCppModels(backend *types.Backend) ([]ModelInfo, error) {
	port := mm.resolveBackendPort(backend)
	base := fmt.Sprintf("http://%s:%d", backend.Host, port)

	filesURL := base + "/api/models/files"
	filesResp, err := mm.client.Get(filesURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch models from cppworker %s: %w", backend.ID, err)
	}
	defer filesResp.Body.Close()

	// Round 17: cppworker returns 'name' (filename with .gguf), NOT 'path' + 'quantization'.
	// The previous schema caused name="" in response (Models tab bug — loaded models invisible).
	var filesData struct {
		Files []struct {
			Name       string `json:"name"`
			SizeBytes  int64  `json:"sizeBytes"`
			ModifiedAt string `json:"modifiedAt"`
		} `json:"files"`
	}
	if err := json.NewDecoder(filesResp.Body).Decode(&filesData); err != nil {
		return nil, fmt.Errorf("failed to decode models from cppworker %s: %w", backend.ID, err)
	}

	// Round 17+18: читаем /api/models (НЕ /api/models/loaded — он не реализован в cppworker).
	// Используем для определения loaded state И для пробрасывания runtime полей.
	type runtimeModel struct {
		Name             string `json:"name"`
		State            string `json:"state"`
		Path             string `json:"path,omitempty"`
		SizeBytes        int64  `json:"sizeBytes,omitempty"`
		LoadingSizeBytes int64  `json:"loadingSizeBytes,omitempty"` // cppworker: real file size (sizeBytes=0 для loaded)
		ContextSize      int    `json:"contextSize,omitempty"`
		GPULayers        int    `json:"gpuLayers,omitempty"`
		BatchSize        int    `json:"batchSize,omitempty"`
		Quantization     string `json:"quantization,omitempty"`
		Architecture     string `json:"architecture,omitempty"`
		VRAMUsage        uint64 `json:"vramUsage,omitempty"`
		RAMUsage         uint64 `json:"ramUsage,omitempty"`
		LoadedAt         string `json:"loadedAt,omitempty"`
		// Round 18+: architecture metadata для оценки VRAM/RAM на frontend.
		NLayers           int `json:"nLayers,omitempty"`
		NHeads            int `json:"nHeads,omitempty"`
		NKvHeads          int `json:"nKvHeads,omitempty"`
		HeadDimK          int `json:"headDimK,omitempty"`
		HeadDimV          int `json:"headDimV,omitempty"`
		NEmbd             int `json:"nEmbd,omitempty"`
		GGUFContextLength int `json:"ggufContextLength,omitempty"`
	}
	loadedSet := make(map[string]bool)
	// R257: признак «список загруженного взят из снапшота поллера» — по нему ниже
	// поправляется и поле State у записи, которая «загружена» только по мнению
	// воркера. Иначе панель читала бы State и снова показывала вытесненную модель
	// загруженной (флаг Loaded уже исправлен, State — нет).
	authoritativeLoaded := false
	// runtimeMap: ключ — basename (с .gguf и без), значение — runtime данные.
	// Используется для обогащения ModelInfo из /api/models/files.
	runtimeMap := make(map[string]runtimeModel)
	allURL := base + "/api/models"
	if lr, lerr := mm.client.Get(allURL); lerr == nil {
		defer lr.Body.Close()
		var ld struct {
			Models []runtimeModel `json:"models"`
		}
		if err := json.NewDecoder(lr.Body).Decode(&ld); err == nil {
			for _, m := range ld.Models {
				// Сохраняем runtime данные по трём ключам (name, basename, basename без .gguf).
				runtimeMap[m.Name] = m
				runtimeMap[mm.basename(m.Name)] = m
				runtimeMap[strings.TrimSuffix(m.Name, ".gguf")] = m
				if m.State == "loaded" && m.Name != "" {
					loadedSet[m.Name] = true
					loadedSet[mm.basename(m.Name)] = true
					loadedSet[strings.TrimSuffix(m.Name, ".gguf")] = true
				}
			}
		}
	}

	// R257 (2026-10-28): «загружено» — из СНАПШОТА поллера, а не из ответа воркера.
	//
	// ЖИВОЙ ДЕФЕКТ. После вытеснения Qwen3.8-27B моделью gemma-4-E4B этот путь
	// продолжал отдавать loaded=true у ОБЕИХ моделей (панель предлагала «загруженную»
	// модель, которой нет в памяти, и оператор грузил её повторно). Причина —
	// два независимых мнения об одном факте: здесь флаг собирался из ответа
	// /api/models НА МОМЕНТ запроса, а метрики/preflight/селектор читают снапшот
	// поллера. Снапшот первичен: он же решает, куда ставить запросы.
	//
	// Рантайм-поля (размер, слои, контекст) по-прежнему берём из ответа воркера —
	// там они свежее и подробнее.
	if snapshotLoaded, snapshotKnown := mm.proxy.loadedSetFromSnapshotR257(backend.ID); snapshotKnown {
		logLoadedDivergenceR257(backend.ID, loadedSet, snapshotLoaded, true)
		loadedSet = snapshotLoaded
		authoritativeLoaded = true
	}

	// R257: признак «список загруженного взят из снапшота» объявлен выше
	// (authoritativeLoaded) — здесь больше не дублируется.

	models := make([]ModelInfo, 0, len(filesData.Files))
	for _, f := range filesData.Files {
		// f.Name это полное имя файла (Qwen3...Q4_K_M.gguf).
		// Для UI используем basename БЕЗ .gguf чтобы совпадало с тем что
		// отдаёт /api/tags у Ollama.
		name := strings.TrimSuffix(f.Name, ".gguf")
		// Берём runtime данные (по трём возможным ключам).
		rt := runtimeMap[name]
		if rt.Name == "" {
			rt = runtimeMap[f.Name]
		}
		if rt.Name == "" {
			rt = runtimeMap[mm.basename(f.Name)]
		}

		// cppworker reports sizeBytes=0 для загруженных моделей — fallback на loadingSizeBytes.
		size := f.SizeBytes
		if size == 0 {
			size = rt.LoadingSizeBytes
		}
		if size == 0 {
			size = rt.SizeBytes
		}

		mi := ModelInfo{
			Name:       name,
			Model:      name,
			Size:       size,
			Digest:     "",
			ModifiedAt: f.ModifiedAt,
			Loaded:     loadedSet[name] || loadedSet[f.Name],
		}
		// Round 18: проброс runtime полей в ModelInfo (omitempty — пустые поля не уходят в JSON).
		if rt.Name != "" {
			mi.ContextLength = rt.ContextSize
			mi.NumGPULayers = rt.GPULayers
			mi.BatchSize = rt.BatchSize
			mi.Quantization = rt.Quantization
			mi.GGUFPath = rt.Path
			mi.State = rt.State
			mi.Architecture = rt.Architecture
			mi.VRAMUsage = rt.VRAMUsage
			mi.RAMUsage = rt.RAMUsage
			mi.LoadedAt = rt.LoadedAt
			// Round 18+: architecture metadata для оценки VRAM/RAM split на frontend.
			mi.NLayers = rt.NLayers
			mi.NKvHeads = rt.NKvHeads
			mi.NEmbd = rt.NEmbd
			mi.HeadDimK = rt.HeadDimK
			mi.HeadDimV = rt.HeadDimV
			mi.MaxContext = rt.GGUFContextLength
		}
		// R257: если список загруженного авторитетен (есть снапшот поллера), State
		// «загруженной» только по мнению воркера модели не сохраняем: панель читает
		// и State, и на нём тоже показывала вытесненную модель загруженной.
		if authoritativeLoaded && !mi.Loaded {
			mi.State = ""
		}
		// Round 18b (2026-07-10): cppworker reports quantization="" (баг в cppworker).
		// GGUF quantization всегда в имени файла (Q4_K_M, Q5_K_S, Q8_0, F16, IQ4_XS, ...).
		// Парсим из имени файла если cppworker не вернул.
		if mi.Quantization == "" {
			mi.Quantization = extractQuantizationFromName(f.Name)
		}
		models = append(models, mi)
	}
	return models, nil
}
