// cuda_arch_refusal_r185.go — R185 (2026-10-20): ОТКАЗ ВМЕСТО ПАДЕНИЯ И КРУЧЕНИЯ.
//
// ЖИВОЙ РАЗБОР. Образ cppworker был собран под CUDA_ARCH=86;120, а бэкенд стоял на
// Quadro RTX 4000 (sm_75). Страж архитектур воркера предупредил в логе, но:
//   - балансер это предупреждение не читал и продолжал просить загрузку;
//   - модели «загружались» (веса мапятся), а на ПЕРВОМ GPU-ядре процесс падал по
//     SIGABRT (CUDA error: no kernel image is available) — весь воркер целиком;
//   - клиент/UI видели бесконечные попытки без причины.
//
// Теперь балансер перед загрузкой спрашивает у воркера /api/info:
//   cuda_arch_ok=false (или непустой cuda_arch_warning) -> ЗАГРУЗКУ НЕ ЗАПУСКАЕМ,
//   возвращаем терминальную ошибку с причиной и что делать. Повтор бессмысленен:
//   от перезагрузки архитектура образа не изменится.
//
// Границы: старый образ поля не отдаёт -> отказ не срабатывает (поведение прежнее,
// менять его вслепую нельзя); недоступный /api/info -> тоже не блокируем, иначе
// один медленный воркер останавливал бы всю загрузку.
package balancer

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// cudaArchProbeTimeout — короткий: это предпроверка, а не полезная работа.
const cudaArchProbeTimeout = 2500 * time.Millisecond

// workerArchFacts — то, что нам нужно из /api/info про архитектуры.
type workerArchFacts struct {
	OK      *bool    `json:"cuda_arch_ok"`
	Warning string   `json:"cuda_arch_warning"`
	Built   []string `json:"cuda_arch_built"`
}

// cudaArchRefusal — нужно ли ОТКАЗАТЬ в загрузке из-за несовместимых архитектур.
//
// Возвращает (отказать, текст причины для клиента и лога).
func (p *Proxy) cudaArchRefusal(backendID, host string, port int) (bool, string) {
	if p == nil || host == "" || port <= 0 {
		return false, ""
	}
	token := ""
	if b := p.GetBackend(backendID); b != nil {
		token = b.CppWorkerApiToken
	}
	url := fmt.Sprintf("http://%s:%d/api/info", host, port)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return false, ""
	}
	if token != "" {
		req.Header.Set("X-API-Token", token)
	}
	client := &http.Client{Timeout: cudaArchProbeTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return false, ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, ""
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, ""
	}
	var facts workerArchFacts
	if err := json.Unmarshal(raw, &facts); err != nil {
		return false, ""
	}
	// Поле отсутствует (старый образ) -> не блокируем.
	if facts.OK == nil {
		return false, ""
	}
	if *facts.OK && facts.Warning == "" {
		return false, ""
	}
	detail := strings.TrimSpace(facts.Warning)
	if detail == "" {
		detail = "CUDA-архитектуры образа и карты не совпадают"
	}
	built := "неизвестно"
	if len(facts.Built) > 0 {
		built = strings.Join(facts.Built, ";")
	}
	return true, fmt.Sprintf(
		"бэкенд %s не может считать на этой карте: %s (образ собран под sm_%s). "+
			"Загрузка НЕ запущена, повторы бессмысленны: пересоберите образ с CUDA_ARCH, "+
			"включающим архитектуру карты (например 75;86;120), и перезапустите воркер [%s]",
		backendID, detail, built, cudaArchUnsupportedCode)
}

// cudaArchUnsupportedCode — машинный код отказа. Он же уходит клиенту полем
// `code` и по нему же отказ узнаётся в тексте ошибки авто-загрузки: текст
// проходит через несколько слоёв обёрток (waitForModelLoad добавляет
// "model load failed: ..."), поэтому строковый маркер — единственный признак,
// который доживает до HTTP-слоя без изменения сигнатур.
const cudaArchUnsupportedCode = "cuda_arch_unsupported"

// CUDAArchUnsupportedCode — тот же код для слоёв вне пакета balancer (API-слой
// решает по нему, что отдавать 400, а не 500).
const CUDAArchUnsupportedCode = cudaArchUnsupportedCode

// IsCUDAArchRefusalText — текст ошибки означает терминальный отказ по аркам.
// Нужен API-слою: ModelOpResult несёт только строку.
func IsCUDAArchRefusalText(s string) bool {
	return strings.Contains(s, cudaArchUnsupportedCode)
}

// modelArchUnsupportedTTL — сколько помним отказ. Архитектура образа сама не
// меняется, но оператор пересобирает образ и перезапускает воркер: запись
// обязана сняться, иначе после исправления клиент ещё минуты получал бы отказ.
const modelArchUnsupportedTTL = 2 * time.Minute

// modelArchUnsupported — запомненный отказ «эта карта не поддерживается образом».
type modelArchUnsupported struct {
	Reason string
	Built  []string
	At     time.Time
}

func modelArchKey(backendID, modelName string) string {
	return backendID + "|" + strings.ToLower(strings.TrimSpace(modelName))
}

// recordModelArchUnsupported — запомнить терминальный отказ по (бэкенд, модель).
func (p *Proxy) recordModelArchUnsupported(backendID, modelName, reason string, built []string) {
	if p == nil || backendID == "" || modelName == "" {
		return
	}
	p.mu.Lock()
	if p.modelArchUnsupported == nil {
		p.modelArchUnsupported = make(map[string]modelArchUnsupported)
	}
	p.modelArchUnsupported[modelArchKey(backendID, modelName)] = modelArchUnsupported{
		Reason: reason, Built: built, At: time.Now(),
	}
	p.mu.Unlock()
}

// modelArchUnsupportedFor — действующая запись (ok=false, если нет или истекла).
func (p *Proxy) modelArchUnsupportedFor(backendID, modelName string) (modelArchUnsupported, bool) {
	if p == nil || backendID == "" || modelName == "" {
		return modelArchUnsupported{}, false
	}
	key := modelArchKey(backendID, modelName)
	p.mu.RLock()
	rec, ok := p.modelArchUnsupported[key]
	p.mu.RUnlock()
	if !ok {
		return modelArchUnsupported{}, false
	}
	if time.Since(rec.At) > modelArchUnsupportedTTL {
		p.mu.Lock()
		delete(p.modelArchUnsupported, key)
		p.mu.Unlock()
		return modelArchUnsupported{}, false
	}
	return rec, true
}

// autoLoadErrorSaysArchUnsupported — ошибка авто-загрузки это наш отказ по аркам.
func autoLoadErrorSaysArchUnsupported(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), cudaArchUnsupportedCode)
}

// writeModelArchUnsupportedResponseFor — терминальный ответ клиенту.
//
// СТАТУС 400, а не 503. Это главное в R185: 503 означает «повтори, станет
// лучше», и клиенты (Cline, deepseek harness, hermes, openai-python) уходят в
// цикл повторов — именно это оператор видел как «балансер бесконечно крутит».
// Архитектура карты от повтора не изменится. 4xx — единственный сигнал, который
// останавливает ретраи у всех трёх клиентов.
//
// Форма зависит от поверхности (R183): /v1/* получает конверт OpenAI, /api/* —
// нативную форму, как и у «модели нет».
func writeModelArchUnsupportedResponseFor(w http.ResponseWriter, requestPath, modelName, reason string) {
	if w == nil {
		return
	}
	body := map[string]interface{}{
		"error":    reason,
		"code":     cudaArchUnsupportedCode,
		"model":    modelName,
		"decision": "arch_unsupported",
	}
	if isOpenAISurface(requestPath) {
		body["error"] = map[string]interface{}{
			"message": reason,
			"type":    "invalid_request_error",
			"param":   "model",
			"code":    cudaArchUnsupportedCode,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Model-Load-Decision", "arch-unsupported")
	// Retry-After НЕ ставим намеренно: это не «подожди», а «дальше так же
	// не заработает».
	writeJSON(w, http.StatusBadRequest, body)
}

// writeModelArchUnsupportedIfKnownFor — единая точка для путей, которые
// сообщают «модель не загружена и авто-загрузка не удалась».
//
// Возвращает true, если ответ уже записан (вызывающий обязан сделать return).
func (p *Proxy) writeModelArchUnsupportedIfKnownFor(w http.ResponseWriter, requestPath, backendID, model string, loadErr error) bool {
	if p == nil || model == "" {
		return false
	}
	rec, known := p.modelArchUnsupportedFor(backendID, model)
	if !known && autoLoadErrorSaysArchUnsupported(loadErr) {
		p.recordModelArchUnsupported(backendID, model, loadErr.Error(), nil)
		rec, known = p.modelArchUnsupportedFor(backendID, model)
	}
	if !known {
		return false
	}
	if logger.Get() != nil {
		logger.Get().Errorw("auto-load: карта не поддерживается образом — отдаю 400 вместо 503 «повтори»",
			"backend", backendID, "model", model, "reason", truncateForLog(rec.Reason, 300))
	}
	writeModelArchUnsupportedResponseFor(w, requestPath, model, rec.Reason)
	return true
}
