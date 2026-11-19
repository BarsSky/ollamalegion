// model_management_ops.go — R632 (2026-11-19): операции над жизненным циклом модели: pull/push/delete/load, профиль загрузки и его параметры.
//
// ЗАЧЕМ ВЫНЕСЕНО. `model_management.go` держал 2172 строки: конструктор,
// операции, ожидание загрузки, отдельный 447-строчный цикл загрузки llama.cpp и
// общий транспорт. Здесь — один из слоёв; поведение не менялось, код перенесён как есть.
package balancer

import (
	"fmt"
	"io"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
	"regexp"
	"sort"
	"strings"
	"time"
)

// extractQuantizationFromName — парсит GGUF quantization из имени файла.
//
// cppworker не сообщает quantization в /api/models (баг cppworker), но
// стандарт GGUF-квантизации всегда в имени файла после последнего "-"
// и до ".gguf": Qwen3-...-Q4_K_M.gguf → Q4_K_M.
//
// Поддерживает все стандартные квантизации llama.cpp:
//   - Legacy k-quants:    Q2_K, Q3_K_S/M/L, Q4_0, Q4_1, Q4_K_S/M, Q5_0, Q5_1, Q5_K_S/M, Q6_K, Q8_0
//   - I-quants (I-quant): IQ1_S/M, IQ2_XXS/XS/S/M, IQ3_XXS/XS/S/M, IQ4_XS/NL
//   - Trellis quants:     TQ1_0, TQ2_0
//   - F-types (precise):  F16, F32, BF16
//   - fpW types:          FP16, BF16
//
// Возвращает "" если не нашёл.
func extractQuantizationFromName(filename string) string {
	if filename == "" {
		return ""
	}
	// Убираем .gguf (case-insensitive).
	base := filename
	if i := strings.LastIndex(strings.ToLower(base), ".gguf"); i >= 0 {
		base = base[:i]
	}
	// Ищем последний "-<QUANT>" в имени.
	// Паттерн: -(IQ?|TQ?|Q|F|BF|FP)?<digits>(_[A-Z]+)?(_[A-Z]+)?
	// Например: -Q4_K_M, -IQ4_XS, -F16, -BF16, -Q5_K
	re := regexp.MustCompile(`-(IQ[1-4]_[A-Z]+|TQ[12]_[0-9]|Q[0-8]_[0-9K]|Q[0-8]_[A-Z]|F1[26]|F32|BF16|FP16)$`)
	m := re.FindStringSubmatch(base)
	if len(m) >= 2 {
		return m[1]
	}
	// Fallback: ищем "Q\d_K_[A-Z]" или "Q\d_\d" в любом месте.
	re2 := regexp.MustCompile(`(Q[0-8]_[K0-9](_[A-Z])?|IQ[1-4]_[A-Z]+|TQ[12]_[0-9])`)
	if m2 := re2.FindStringSubmatch(base); len(m2) >= 2 {
		return m2[1]
	}
	return ""
}

// basename — выделяет имя файла из пути (поддержка '/' и '\').
func (mm *ModelManager) basename(p string) string {
	if p == "" {
		return ""
	}
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	if i := strings.LastIndex(p, `\`); i >= 0 {
		p = p[i+1:]
	}
	return p
}

// GetActiveOps — возвращает список активных операций
func (mm *ModelManager) GetActiveOps() []map[string]interface{} {
	mm.mu.RLock()
	defer mm.mu.RUnlock()

	var result []map[string]interface{}
	for key, backends := range mm.activeOps {
		// key = operation + ":" + modelName
		op := ""
		modelName := key
		if idx := strings.Index(key, ":"); idx > 0 {
			op = key[:idx]
			modelName = key[idx+1:]
		}
		for backendID, startedAt := range backends {
			result = append(result, map[string]interface{}{
				"operation": op,
				"modelName": modelName,
				"backendId": backendID,
				"startedAt": startedAt.Format(time.RFC3339),
				"duration":  time.Since(startedAt).String(),
				"status":    "running",
			})
		}
	}
	return result
}

// ===== Внутренние реализации =====

// executePull — загрузка модели на бэкенд
func (mm *ModelManager) executePull(host string, port int, backendID string, req ModelOpRequest) *ModelOpResult {
	url := fmt.Sprintf("http://%s:%d/api/pull", host, port)

	body := map[string]interface{}{
		"name":   req.ModelName,
		"stream": req.Stream,
	}
	if req.Insecure {
		body["insecure"] = true
	}

	return mm.sendOllamaRequest("POST", url, backendID, req, body)
}

// executePush — отправка модели с бэкенда
func (mm *ModelManager) executePush(host string, port int, backendID string, req ModelOpRequest) *ModelOpResult {
	url := fmt.Sprintf("http://%s:%d/api/push", host, port)

	body := map[string]interface{}{
		"name":   req.ModelName,
		"stream": req.Stream,
	}
	if req.Insecure {
		body["insecure"] = true
	}

	return mm.sendOllamaRequest("POST", url, backendID, req, body)
}

// executeDelete — удаление модели с бэкенда
func (mm *ModelManager) executeDelete(host string, port int, backendID string, req ModelOpRequest) *ModelOpResult {
	url := fmt.Sprintf("http://%s:%d/api/delete", host, port)

	body := map[string]interface{}{
		"name": req.ModelName,
	}

	return mm.sendOllamaRequest("DELETE", url, backendID, req, body)
}

// executeLoad — загрузка модели в память на бэкенде (через /api/generate с keep_alive)
func (mm *ModelManager) executeLoad(host string, port int, backendID string, req ModelOpRequest) *ModelOpResult {
	url := fmt.Sprintf("http://%s:%d/api/generate", host, port)

	body := map[string]interface{}{
		"model":      req.ModelName,
		"keep_alive": "5m",
		"prompt":     "", // пустой промпт для загрузки без генерации
		"stream":     false,
	}

	resp, err := mm.sendRawRequest("POST", url, body)
	if err != nil {
		return &ModelOpResult{
			Success:   false,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Error:     fmt.Sprintf("failed to load model: %v", err),
		}
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return &ModelOpResult{
			Success:   true,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Message:   fmt.Sprintf("Model '%s' loaded successfully on backend '%s'", req.ModelName, backendID),
		}
	}

	return &ModelOpResult{
		Success:   false,
		Operation: req.Operation,
		ModelName: req.ModelName,
		BackendID: backendID,
		Error:     fmt.Sprintf("ollama error (HTTP %d): %s", resp.StatusCode, string(respBody)),
	}
}

// Параметры retry при 503 "model is loading" от cppworker — чтобы не отдавать
// клиенту ошибку, если параллельный запрос уже инициировал загрузку.
const (
	llamaCppLoadMaxRetries    = 10 // ~30 секунд при retryInterval=3s
	llamaCppLoadRetryInterval = 3 * time.Second
	// Round 8 (2026-07-10): bumped default to 10 minutes. 21GB Qwen3-A3B takes
	// ~7 min на A10, плюс пользовательские модели могут быть больше.
	// Если клиент отвалится по timeout, graceful poll завершения
	// (pollLoadCompletionUntilLoaded) подхватит успех.
	llamaCppLoadDefaultTimeout = 600 * time.Second // 10 минут — для больших MoE/Qwen моделей
)

// getLoadTimeout — возвращает таймаут для POST /api/models/load из конфигурации.
// Приоритет: Balancing.ModelLoadTimeout → llamaCppLoadDefaultTimeout (120s).
// Раньше был хардкод 5s, что недостаточно для cold-start больших GGUF моделей.
func (mm *ModelManager) getLoadTimeout() time.Duration {
	if mm.proxy != nil && mm.proxy.config != nil {
		if mm.proxy.config.Balancing.ModelLoadTimeout > 0 {
			return time.Duration(mm.proxy.config.Balancing.ModelLoadTimeout) * time.Second
		}
	}
	return llamaCppLoadDefaultTimeout
}

// profileForLoad — per-model профиль для подстановки параметров загрузки.
//
// R66d (2026-09-23): поиск повторяет семантику GetModelProfileNumCtx
// (num_ctx_resolver.go:99) — сначала точное совпадение ключа, затем
// case-insensitive substring в обе стороны. Это нужно, потому что WebUI и
// клиенты передают имя файла с расширением ("gemma-4-E4B-it-Q4_K_M.gguf"), а
// профили в config.json записаны без него ("gemma-4-E4B-it-Q4_K_M").
//
// ВАЖНО: при нескольких совпадениях раньше возвращался первый из map-итерации
// (порядок в Go случайный) — здесь сначала берём точное совпадение, и только
// потом substring (по отсортированным ключам, чтобы результат был
// детерминированным).
func (mm *ModelManager) profileForLoad(modelName string) (types.LlamaCppModelProfile, bool) {
	if mm == nil || mm.proxy == nil || modelName == "" {
		return types.LlamaCppModelProfile{}, false
	}
	if prof, ok := mm.proxy.GetModelProfile(modelName); ok {
		return prof, true
	}
	// Точного совпадения нет — ищем substring-совпадение детерминированно.
	// R423: снимок карты профилей, а не прямое чтение config (см.
	// model_profiles_guard.go).
	profiles := mm.proxy.profilesSnapshotR423()
	if len(profiles) == 0 {
		return types.LlamaCppModelProfile{}, false
	}
	keys := make([]string, 0, len(profiles))
	for k := range profiles {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if containsFold(k, modelName) || containsFold(modelName, k) {
			return profiles[k], true
		}
	}
	return types.LlamaCppModelProfile{}, false
}

// applyProfileLoadParams — R83 (2026-09-29): подстановка полей профиля модели
// в запрос загрузки. Вынесено из executeLlamaCppLoad, чтобы правило было
// покрыто тестом (раньше тест дублировал логику копипастой и не ловил
// расхождения вроде «parallel из профиля не доезжал»).
//
// Приоритет: явные поля запроса > профиль > дефолт cppworker.
//
// IgnoreDefaults=true — профиль не подставляет НИЧЕГО: оператор явно отказался
// от скрытых дефолтов (env-значения cppworker должны выигрывать, см.
// docs/operations/deployment-stack.ru.md §7.0). Параллельность в этом режиме задаётся
// окружением контейнера (CPPWORKER_N_PARALLEL) — см. internal/cppbackend
// resolveNParallel, где то же число уходит и в C-bridge, и в SlotManager.
func applyProfileLoadParams(req *ModelOpRequest, prof types.LlamaCppModelProfile) {
	if req == nil {
		return
	}
	// R83/v62 (2026-10-02): ignoreDefaults БОЛЬШЕ НЕ отключает профиль модели.
	//
	// БЫЛО: при ignoreDefaults=true функция возвращалась ЗДЕСЬ — профиль модели не
	// применялся вообще, а следом применялся defaultModelProfile и заполнял всё.
	// «Игнорировать умолчания» на практике значило «игнорировать настройки
	// оператора»: parallel, KV-cache, слои и окно из WebUI перебивались дефолтом.
	// Живые случаи: gemma-4 грузился на 20 слоях из 43 при профиле с -1 (в дефолте
	// стояло 20); у модели с parallel=1 клиент получал окно на слот как при
	// parallel=2.
	//
	// ТЕПЕРЬ: профиль модели применяется всегда; ignoreDefaults гасит только
	// применение defaultModelProfile (см. executeLlamaCppLoad).
	if prof.IgnoreDefaults && logger.Get() != nil {
		logger.Get().Infow(i18n.T("balancer.model_management.profile_applied_ignore_defaults_info"),
			"model", req.ModelName,
			"profile_context_length", prof.ContextLength,
			"profile_num_gpu_layers", prof.NumGPULayers)
	}
	// R397 (2026-11-04): профиль — HINT ПЕРВИЧНОЙ загрузки, а не предел запроса
	// (R68). Пока окно подставлялось из профиля и при явном запросе, суммарное
	// contextLength перебивало клиентское окно, пришедшее как contextPerSeq:
	//
	//	живой прогон: клиент num_ctx=131072 → автозагрузка просит contextPerSeq=65536
	//	→ applyProfileLoadParams подставляет ContextSize=32768 из профиля →
	//	resolveDeclaredWindow выбирает суммарное (32768), per-seq выбрасывает →
	//	воркер грузит 32768, а запрос клиента на 131072 получает отказ.
	//
	// Теперь профиль заполняет окно ТОЛЬКО когда запрос не задал ни одного из
	// полей. Явный запрос (total и/или per-seq) идёт как есть.
	totalFromProfile := false
	if req.ContextSize == nil && req.ContextPerSeq == nil && prof.ContextLength > 0 {
		cs := prof.ContextLength
		req.ContextSize = &cs
		totalFromProfile = true
	}
	if req.BatchSize == nil && prof.BatchSize > 0 {
		bs := prof.BatchSize
		req.BatchSize = &bs
	}
	if req.GPULayers == nil && prof.NumGPULayers != 0 {
		gl := prof.NumGPULayers
		req.GPULayers = &gl
	}
	if req.FlashAttn == nil && prof.FlashAttn != nil {
		// profile.FlashAttn — *bool (человекочитаемый JSON), cppworker ждёт
		// *int (-1=auto, 0=off, 1=on). Та же конверсия, что в
		// handlers_cppworker_profiles.go:483 (apply path).
		fa := 0
		if *prof.FlashAttn {
			fa = 1
		}
		req.FlashAttn = &fa
	}
	if req.UseMmap == nil && prof.UseMmap != nil {
		um := *prof.UseMmap
		req.UseMmap = &um
	}
	if req.KVCacheType == nil && prof.KVCacheType != "" {
		kv := prof.KVCacheType
		req.KVCacheType = &kv
	}
	// R83/v62 (2026-10-02): reasoning из профиля модели (галочка WebUI).
	// Приоритет как у остальных полей: явный запрос > профиль модели > дефолт > env.
	if req.EnableReasoning == nil && prof.EnableReasoning != nil {
		v := *prof.EnableReasoning
		req.EnableReasoning = &v
	}
	// R83 (2026-09-29): parallel из профиля. Раньше это поле профиля
	// учитывалось ТОЛЬКО на пути POST .../model-profiles/{name}/apply
	// (handlers_cppworker_profiles.go:586) и терялось при обычной загрузке
	// (auto-load из /api/chat, POST /api/models/load*): cppworker получал
	// запрос без `parallel`, поднимал single-slot, и второй параллельный
	// клиент упирался в SlotManager (503/timeout).
	if req.Parallel == nil && prof.Parallel > 0 {
		par := prof.Parallel
		req.Parallel = &par
	}
	// R83 (2026-09-30): окно на клиента из профиля — как и parallel, работает и
	// при обычной загрузке (раньше такие поля терялись вне пути .../apply).
	perSeqFromProfile := false
	if req.ContextPerSeq == nil && prof.ContextPerSeq > 0 {
		per := prof.ContextPerSeq
		req.ContextPerSeq = &per
		perSeqFromProfile = true
	}

	// R255 (2026-10-28): ДВА поля профиля про одно и то же окно — согласовать
	// ПЕРЕД отправкой воркеру.
	//
	// ЖИВОЙ ДЕФЕКТ: оператор выставил в карточке модели суммарное окно 256000
	// (`contextLength`), но в профиле осталось старое «окно на клиента»
	// (`contextPerSeq`, R83). Балансер отправлял оба поля, а cppworker при
	// непустом contextPerSeq считает суммарное САМ: total = PAD256(perSeq) × слоты.
	// Итог — модель грузилась с 16384 × 2 = 32768, а настройка оператора
	// выглядела проигнорированной (живая жалоба).
	// R397: противоречие total vs per-seq разрешаем, только когда оба поля
	// реально пришли (или одно из них подставил профиль). Если запрос задал
	// ТОЛЬКО per-seq, суммарного окна для сравнения нет — не выдумываем его из
	// профиля, иначе клиентское окно снова будет урезано.
	reqHasBothWindows := req.ContextSize != nil && req.ContextPerSeq != nil
	if prof.ContextLength > 0 || prof.ContextPerSeq > 0 {
		if reqHasBothWindows || totalFromProfile || perSeqFromProfile {
			slots := 1
			if req.Parallel != nil && *req.Parallel > 0 {
				slots = *req.Parallel
			} else if prof.Parallel > 0 {
				slots = prof.Parallel
			}
			total, perSeq := 0, 0
			if req.ContextSize != nil {
				total = *req.ContextSize
			}
			if req.ContextPerSeq != nil {
				perSeq = *req.ContextPerSeq
			}
			resolvedTotal, resolvedPerSeq, note := resolveDeclaredWindow(total, perSeq, slots)
			if resolvedTotal > 0 {
				rt := resolvedTotal
				req.ContextSize = &rt
			}
			if resolvedPerSeq > 0 {
				rp := resolvedPerSeq
				req.ContextPerSeq = &rp
			} else {
				// Противоречие в пользу суммарного окна: per-seq НЕ отправляем,
				// иначе воркер снова пересчитает окно сам и заявленное значение
				// будет потеряно.
				req.ContextPerSeq = nil
			}
			// Одна строка с числами: заявлено → уйдёт в загрузку. Печатаем только
			// когда в профиле заданы ОБА поля (иначе это шум на каждой загрузке).
			if prof.ContextLength > 0 && prof.ContextPerSeq > 0 {
				logDeclaredWindow(req.ModelName, prof, slots, resolvedTotal, resolvedPerSeq, note)
			}
		}
	}
}

// resolveOverrideTensors — резолвит per-tensor override для load.
// Приоритет: явные поля req.OverrideTensors > сохранённый LlamaCppModelProfile.
// Возвращает согласованные по длине parallel arrays или пустые slices.
//
// Round 7 (2026-07-09): сохраняем override-tensors между перезагрузками через
// /api/profiles endpoint. Если у модели есть сохранённый профиль с override-tensors,
// каждая автоматическая загрузка применяет их (Qwen3-A3B → CPU offload экспертов).
func (mm *ModelManager) resolveOverrideTensors(req ModelOpRequest) ([]string, []string) {
	// 1) явное override в запросе — наивысший приоритет.
	if len(req.OverrideTensors) > 0 && len(req.OverrideTensors) == len(req.OverrideTensorBufts) {
		return req.OverrideTensors, req.OverrideTensorBufts
	}
	// 2) сохранённый профиль.
	if mm.proxy != nil {
		if prof, ok := mm.proxy.GetModelProfile(req.ModelName); ok {
			if len(prof.OverrideTensors) > 0 && len(prof.OverrideTensors) == len(prof.OverrideTensorBufts) {
				return prof.OverrideTensors, prof.OverrideTensorBufts
			}
		}
	}
	return nil, nil
}

// isTimeoutError — проверяет, является ли ошибка HTTP-клиента таймаутом
// (Client.Timeout exceeded while awaiting headers). Используется в Round 8
// graceful load timeout path.
func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "Client.Timeout exceeded") ||
		strings.Contains(s, "context deadline exceeded") ||
		strings.Contains(s, "i/o timeout")
}
