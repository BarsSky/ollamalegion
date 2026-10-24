package balancer

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
)

// model_missing_r119.go — R119 (2026-10-15): «такой модели на бэкенде НЕТ» —
// терминальный ответ клиенту, а не бесконечный цикл перезагрузок.
//
// ЖИВОЙ ДЕФЕКТ (стенд). Клиент (OpenWebUI) запросил модель `llama-3-8b`, которой
// нет в каталоге. Что происходило:
//
//  1. preflight видел loaded_n_ctx=0 ≠ requested и планировал async-загрузку;
//  2. cppworker отвечал 404 `{"available_models":["gemma-4…","Qwen3.8-27B…"]}` —
//     то есть ПОНЯТНЫЙ терминальный отказ;
//  3. балансер его ИГНОРИРОВАЛ («reload returned non-OK status … status:404») и
//     на каждый следующий запрос снова планировал загрузку, а клиенту отдавал
//     503 «model "llama-3-8b" is being reloaded to n_ctx=2048 … retry in 30s»;
//  4. клиент повторял, ждал сотни секунд и падал по своему таймауту — оператор
//     видел «Internal Server Error» и не понимал причины.
//
// Замер: POST /api/generate через балансер для несуществующей модели не отвечал
// более 4 минут (прямой запрос в cppworker отвечал за 45 мс, но статусом 500 и
// без списка доступных моделей).
//
// ТЕПЕРЬ: отказ запоминается на (бэкенд, модель) — как это уже сделано для
// «окно физически недостижимо» (nctx_infeasible_r118.go). Пока запись жива,
// preflight НЕ планирует перезагрузку, а клиент сразу получает 404 с причиной и
// списком реально доступных моделей. Запись снимается по TTL, при успешной
// загрузке/перезагрузке и — главное — живёт недолго (модель могли только что
// скопировать на диск, а cppworker пересканирует каталог раз в 30 с).

// modelMissingTTL — сколько помним «модели нет». Короче, чем у n_ctx_infeasible:
// модель может появиться на диске в любую минуту (скопировали файл), и
// cppworker пересканирует каталог каждые 30 с. Минуты достаточно, чтобы
// клиентские ретраи не устраивали цикл загрузок, и мало, чтобы устаревший 404
// блокировал только что добавленную модель.
const modelMissingTTL = 60 * time.Second

// modelMissing — запомненный отказ «модели нет на этом бэкенде».
type modelMissing struct {
	// Reason — объяснение от cppworker (error / hint), для клиента и лога.
	Reason string
	// Available — список моделей, которые бэкенд реально знает. Оператор сразу
	// видит, что просить вместо отсутствующей.
	Available []string
	// At — когда отказали (для TTL).
	At time.Time
}

// modelMissingKey — ключ записи: бэкенд + модель (в нижнем регистре).
func modelMissingKey(backendID, modelName string) string {
	return backendID + "|" + strings.ToLower(strings.TrimSpace(modelName))
}

// isModelMissingResponse — ответ cppworker про отсутствующую модель.
//
// ВАЖНО: ГОЛЫЙ 404 признаком НЕ является. 404 отдаёт и эндпоинт, которого нет
// (старая сборка cppworker, мок, чужой прокси-путь) — живой пример из
// существующего теста: мок отвечает «404 page not found» на /api/models/load, и
// первая версия этой проверки принимала это за «модели нет», из-за чего preflight
// переставал планировать рост окна (TestR83_StaleCache_ResolverUsesLiveWindow).
//
// Поэтому требуем ПОЛОЖИТЕЛЬНОЕ свидетельство, что ответ именно про модель:
//   - машинный код `model_file_not_found` (cppworker R119+);
//   - `available_models` со списком моделей бэкенда (так cppworker отвечал на
//     /api/models/load для неизвестного имени — тело из живого лога);
//   - текст «model … not found» / «no such file» / «модели … нет».
//
// Статус обязан быть ≥ 400: успешное тело со списком моделей — не отказ.
func isModelMissingResponse(status int, body []byte) bool {
	if status < 400 {
		return false
	}
	low := strings.ToLower(string(body))
	for _, marker := range []string{
		"model_file_not_found",
		"available_models",
		"not found in",
		"not found:",
		"no such file",
		"модели",
	} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

// modelMissingFromBody — вытащить причину и список доступных моделей из тела.
func modelMissingFromBody(body []byte) (string, []string) {
	var parsed struct {
		Error           string   `json:"error"`
		Hint            string   `json:"hint"`
		Suggestion      string   `json:"suggestion"`
		AvailableModels []string `json:"available_models"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return strings.TrimSpace(string(body)), nil
	}
	parts := make([]string, 0, 3)
	for _, s := range []string{parsed.Error, parsed.Suggestion, parsed.Hint} {
		if t := strings.TrimSpace(s); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " "), parsed.AvailableModels
}

// recordModelMissing — запомнить, что модели нет на бэкенде.
//
// Пустая причина допустима: ответ мог быть просто 404 с телом-заглушкой.
func (p *Proxy) recordModelMissing(backendID, modelName string, body []byte) {
	if p == nil || backendID == "" || modelName == "" {
		return
	}
	reason, available := modelMissingFromBody(body)
	key := modelMissingKey(backendID, modelName)
	p.mu.Lock()
	if p.modelMissing == nil {
		p.modelMissing = make(map[string]modelMissing)
	}
	p.modelMissing[key] = modelMissing{Reason: reason, Available: available, At: time.Now()}
	p.mu.Unlock()

	logger.Get().Warnw(i18n.T("balancer.model_missing.recorded_warn"),
		"backend", backendID, "model", modelName,
		"available_models", len(available), "reason", truncateForLog(reason, 200))
}

// modelMissingFor — действующая запись «модели нет» (ok=false, если нет или истекла).
func (p *Proxy) modelMissingFor(backendID, modelName string) (reason string, available []string, ok bool) {
	if p == nil || backendID == "" || modelName == "" {
		return "", nil, false
	}
	key := modelMissingKey(backendID, modelName)
	p.mu.RLock()
	rec, exists := p.modelMissing[key]
	p.mu.RUnlock()
	if !exists {
		return "", nil, false
	}
	if time.Since(rec.At) > modelMissingTTL {
		p.clearModelMissing(backendID, modelName)
		return "", nil, false
	}
	return rec.Reason, rec.Available, true
}

// clearModelMissing — снять запись (после успешной загрузки/перезагрузки).
func (p *Proxy) clearModelMissing(backendID, modelName string) {
	if p == nil || backendID == "" || modelName == "" {
		return
	}
	key := modelMissingKey(backendID, modelName)
	p.mu.Lock()
	if p.modelMissing != nil {
		delete(p.modelMissing, key)
	}
	p.mu.Unlock()
}

// modelMissingSnapshot — все действующие записи (для тестов и диагностики).
func (p *Proxy) modelMissingSnapshot() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, len(p.modelMissing))
	for k := range p.modelMissing {
		out = append(out, k)
	}
	return out
}

// formatModelMissingMessage — человекочитаемое объяснение (идёт в поле error,
// его показывают OpenWebUI/Cline как есть).
func formatModelMissingMessage(modelName, reason string, available []string) string {
	msg := "model " + quoteForMessage(modelName) + " not found on this backend"
	if reason != "" {
		msg += ": " + reason
	}
	if len(available) > 0 {
		const maxList = 11
		shown := available
		suffix := ""
		if len(shown) > maxList {
			shown = shown[:maxList]
			suffix = ", …"
		}
		msg += " (available: " + strings.Join(shown, ", ") + suffix + ")"
	}
	return msg
}

// writeModelMissingResponse — терминальный 404 клиенту.
//
// 404 (а не 503) — принципиально: 503 означает «повтори, станет лучше», и
// клиенты (OpenWebUI, Cline) уходят в retry-цикл. Модели, которой нет, от
// повторения не появится; Ollama для неизвестной модели тоже отдаёт 404.
//
// R183 (2026-10-19): ФОРМА ОШИБКИ ЗАВИСИТ ОТ ПОВЕРХНОСТИ.
//
// Живая приёмка (errors-matrix-r180) показала: на OpenAI-поверхности `/v1/*` ошибка
// уходила в ollama-форме — `{"error": "<строка>"}`. OpenAI-клиенты (deepseek harness,
// hermes, openai-python/js) читают `error.message`, поэтому текст причины терялся,
// и клиент видел невнятную ошибку или падал на разборе. Статус и текст были верны —
// неверна была форма.
//
// Теперь поверхность передаётся в писатель явно: `/v1/*` получает конверт OpenAI
// (`error.message/type/code`), нативная `/api/*` сохраняет прежнюю форму ровно как
// раньше (её читают Cline и OpenWebUI). Дополнительные поля (code/model/decision/
// availableModels) остаются в обоих случаях — они не конфликтуют с конвертом.
func writeModelMissingResponse(w http.ResponseWriter, modelName, reason string, available []string) {
	writeModelMissingResponseFor(w, "", modelName, reason, available)
}

// writeModelMissingResponseFor — то же, но с учётом поверхности запроса.
func writeModelMissingResponseFor(w http.ResponseWriter, requestPath, modelName, reason string, available []string) {
	if w == nil {
		return
	}
	msg := formatModelMissingMessage(modelName, reason, available)
	body := map[string]interface{}{
		"error":    msg,
		"code":     "model_not_found",
		"model":    modelName,
		"decision": "model_missing",
	}
	if isOpenAISurface(requestPath) {
		// Конверт OpenAI: message — человекочитаемая причина, type/param — как у
		// OpenAI (invalid_request_error + model), code — машинный код.
		body["error"] = map[string]interface{}{
			"message": msg,
			"type":    "invalid_request_error",
			"param":   "model",
			"code":    "model_not_found",
		}
	}
	if len(available) > 0 {
		body["availableModels"] = available
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-NCtx-Reload-Decision", "model-missing")
	writeJSON(w, http.StatusNotFound, body)
}

// quoteForMessage — обернуть имя модели в кавычки (как это делает Ollama).
func quoteForMessage(s string) string {
	return "\"" + s + "\""
}

// awaitModelMissing — R119: короткое ожидание появления записи «модели нет».
//
// ЗАЧЕМ. Фоновая загрузка отсутствующей модели падает за миллисекунды (cppworker
// отвечает 404 сразу), но запись в реестр ставится чуть ПОЗЖЕ момента, когда
// preflight уже принял решение отдать 503. Живой замер после первой правки:
// самый первый запрос к llama-3-8b получал 503 через 2 с, а следующий — 404 за
// 7 мс. Клиент не должен ловить эту гонку: ждём запись коротко (сотни
// миллисекунд) и, если она появилась, отдаём терминальный 404 с первого раза.
//
// Для РЕАЛЬНОЙ загрузки это лишь доли секунды перед тем же 503, поэтому
// заметной задержки не добавляет.
func (p *Proxy) awaitModelMissing(backendID, modelName string, timeout time.Duration) bool {
	if p == nil || timeout <= 0 {
		return false
	}
	deadline := time.Now().Add(timeout)
	for {
		if _, _, known := p.modelMissingFor(backendID, modelName); known {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// writeModelMissingIfKnown — R119 (2026-10-15): единая точка для путей, которые
// сообщают «модель не загружена и авто-загрузка не удалась».
//
// ЗАЧЕМ. Таких мест несколько (handleChat, handleOpenAICompletion,
// handleOpenAIEmbeddings, handleGenerate, v1/embeddings), и все они отвечали
// 503 + Retry-After. Для ОТСУТСТВУЮЩЕЙ модели это неверно: 503 означает
// «повтори, станет лучше», и клиент (OpenWebUI, DSH, Cline) уходит в retry-цикл,
// хотя модели от повторения не появится. Живой замер: /v1/chat/completions для
// llama-3-8b отдавал 503 «is not loaded and auto-load failed: … not found in
// ./models», и так на КАЖДЫЙ повтор.
//
// Правило: если про (бэкенд, модель) уже известно, что модели нет — 404. Если
// авто-загрузка только что упала с «не найдено» — фиксируем это в реестре прямо
// здесь и тоже отдаём 404, чтобы первый же запрос получил терминальный ответ.
//
// Возвращает true, если ответ уже записан (вызывающий обязан сделать return).
func (p *Proxy) writeModelMissingIfKnown(w http.ResponseWriter, backendID, model string, loadErr error) bool {
	return p.writeModelMissingIfKnownFor(w, "", backendID, model, loadErr)
}

// writeModelMissingIfKnownFor - то же, но с учётом поверхности запроса (R183):
// на /v1/* ошибка обязана быть в конверте OpenAI.
func (p *Proxy) writeModelMissingIfKnownFor(w http.ResponseWriter, requestPath, backendID, model string, loadErr error) bool {
	if p == nil || model == "" {
		return false
	}
	reason, available, known := p.modelMissingFor(backendID, model)
	if !known && autoLoadErrorSaysModelMissing(loadErr) {
		p.recordModelMissing(backendID, model, []byte(loadErr.Error()))
		reason, available, known = p.modelMissingFor(backendID, model)
	}
	if !known {
		return false
	}
	logger.Get().Warnw(i18n.T("balancer.model_missing.auto_load_404_warn"),
		"backend", backendID, "model", model, "available_models", len(available))
	writeModelMissingResponseFor(w, requestPath, model, reason, available)
	return true
}

// autoLoadErrorSaysModelMissing — ошибка авто-загрузки про отсутствующую модель.
//
// Формулировки, которые реально встречаются:
//   - модель балансера: `model "x" not found in ./models (tried "models/x.gguf")`
//   - cppworker:         `model file not found: /app/models/x.gguf`
//
// «model is loading» сюда НЕ попадает: это временное состояние, и его обязан
// обрабатывать прежний 503 + Retry-After.
func autoLoadErrorSaysModelMissing(err error) bool {
	if err == nil {
		return false
	}
	low := strings.ToLower(err.Error())
	if strings.Contains(low, "is loading") {
		return false
	}
	for _, marker := range []string{
		"not found", "no such file", "file not found", "model_file_not_found",
		"не найдена", "не найден",
	} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

// truncateForLog — обрезать длинный текст для лога.
func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// isOpenAISurface — путь принадлежит OpenAI-совместимой поверхности (/v1/...)?
//
// R183: нужно, чтобы одна и та же ошибка уходила в форме, которую понимает клиент
// этой поверхности: /v1/* — конверт OpenAI, /api/* — нативная форма Ollama.
func isOpenAISurface(requestPath string) bool {
	p := strings.TrimSpace(requestPath)
	if p == "" {
		return false
	}
	return strings.HasPrefix(p, "/v1/") || p == "/v1"
}
