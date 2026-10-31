package balancer

// cancel_client_r229.go — R229 (2026-10-22): отмена генерации, инициированная
// КЛИЕНТОМ (кнопка Stop в OpenWebUI, Cline, скрипт), а не разрывом соединения.
//
// ЖИВОЙ ДЕФЕКТ (замер на стенде ДО правки). Клиент отправил длинный стрим и через
// 2 секунды — POST /api/cancel с именем модели. Балансер НЕ ответил (таймаут 15 с),
// а стрим продолжился до конца: 380 чанков, 62 с. То есть отмена клиента не
// доходила до воркера вовсе: балансер пытался маршрутизировать /api/cancel как
// обычную генерацию (выбор бэкенда, автозагрузка, ожидание слота), вместо того
// чтобы просто приказать воркеру прекратить работу.
//
// У воркера штатная отмена — POST /api/cancel {request_id|user_id|model}; R184 уже
// отправлял её при РАЗРЫВЕ клиента. Теперь обрабатывается и ЯВНАЯ отмена.
//
// Ответ клиенту быстрый и честный: сколько бэкендов приняли приказ. Если отменять
// нечего — 200 {"status":"no_active_generation", ...} (Ollama на пустом списке
// отвечает 200/404 в зависимости от версии; клиенты воспринимают это как успех).

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// clientCancelTimeout — сколько ждём ответа одного воркера на отмену. Отмена —
// служебный приказ: воркер может быть занят самомй генерацией, но приказ уже в
// сокете; ждать дольше незачем (клиенту нужен быстрый ответ).
const clientCancelTimeout = 2 * time.Second

// clientCancelBody — тело /api/cancel. Все поля необязательные: клиенты присылают
// разные наборы (OpenWebUI — model, Cline — request_id, скрипты — пустое тело).
type clientCancelBody struct {
	RequestID string `json:"request_id"`
	UserID    string `json:"user_id"`
	Model     string `json:"model"`
}

// isClientCancelPath — путь явной отмены на Ollama-поверхности. OpenAI-клиенты
// отменяют разрывом соединения (это R184, отдельный механизм), поэтому здесь
// только нативный путь.
func isClientCancelPath(path string) bool {
	return path == "/api/cancel" || path == "/api/cancel/"
}

// handleClientCancel — обработать POST /api/cancel от клиента.
func (p *Proxy) handleClientCancel(w http.ResponseWriter, r *http.Request) {
	if p == nil {
		writeClientCancelError(w, http.StatusServiceUnavailable, "balancer is not ready")
		return
	}
	if r.Method != http.MethodPost {
		writeClientCancelError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}

	var req clientCancelBody
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if len(bytes.TrimSpace(body)) > 0 {
		// Терпимое декодирование: лишние поля от клиентов не должны давать 400.
		_ = json.Unmarshal(body, &req)
	}
	req.Model = trimSpaceOnly(req.Model)

	targets := p.cancelTargets(req.Model)
	accepted := make([]string, 0, len(targets))
	results := make([]map[string]interface{}, 0, len(targets))
	for _, backendID := range targets {
		ok, detail := p.sendCancelToBackend(backendID, req)
		row := map[string]interface{}{"backend": backendID, "ok": ok}
		if detail != nil {
			// Воркер сообщает, сколько генераций реально отменено и почему не вышло
			// (R229: status/no_active_generation + подсказка про порт 50052).
			for k, v := range detail {
				row[k] = v
			}
		}
		results = append(results, row)
		if ok {
			accepted = append(accepted, backendID)
		}
	}

	// R343 (2026-10-30, аудит docs/plans/2026-10-30-abort-and-metrics-audit.md):
	// СТАТУС СЧИТАЕТСЯ ПО ФАКТУ ОТМЕНЫ, А НЕ ПО HTTP-КОДУ.
	//
	// Живой дефект: успехом считался любой 2xx, а воркер на пустом трекере
	// отвечает 200 + cancelled:0. Верхний status печатал "cancelled" и тогда,
	// когда генерация продолжалась. Плюс имя модели сверялось на двух концах
	// по-разному (балансер — мягко, воркер — точно), поэтому "доставлено" не
	// означало "отменено".
	status, hint := cancelStatusR343(results, accepted, len(targets))
	if logger.Get() != nil {
		logger.Get().Infow(i18n.T("balancer.cancel_client.broadcast_info"),
			"model", req.Model, "request_id", req.RequestID, "user_id", req.UserID,
			"targets", targets, "accepted", accepted, "status", status)
	}
	w.Header().Set("Content-Type", "application/json")
	responseBody := map[string]interface{}{
		"status":   status,
		"model":    req.Model,
		"backends": accepted,
		// results — детали по каждому воркеру: сколько отменено и статус. Клиенту
		// достаточно "cancelled", оператору нужен ответ «почему не остановилось».
		"results": results,
	}
	if hint != "" {
		responseBody["hint"] = hint
	}
	writeJSON(w, http.StatusOK, responseBody)
}

// cancelStatusR343 — честный статус отмены по фактическим результатам воркеров.
//
// Возвращает (status, hint):
//   - "cancelled"          — хотя бы одна генерация реально отменена;
//   - "not_cancelled"      — воркеры ответили, но не отменили ничего (нет активной
//     генерации либо имя модели не совпало точно) — с подсказкой, что проверить;
//   - "no_targets"         — отменять было некому (нет healthy llama.cpp-бэкендов);
//   - "unreachable"        — цели были, но ни один воркер не принял приказ.
//
// Считаем по сумме cancelled в ответах воркеров: HTTP 200 + cancelled:0 — это
// НЕ отмена (живой дефект, ради которого функция появилась).
func cancelStatusR343(results []map[string]interface{}, accepted []string, targetCount int) (status, hint string) {
	totalCancelled := 0
	for _, row := range results {
		switch v := row["cancelled"].(type) {
		case float64:
			totalCancelled += int(v)
		case int:
			totalCancelled += v
		}
	}
	switch {
	case totalCancelled > 0:
		return "cancelled", ""
	case targetCount == 0:
		// ВАЖНО: значение "no_active_generation" оставлено прежним — его уже
		// проверяет TestR229_ClientCancelNoBackendsIsNotTimeout и на него могут
		// смотреть клиенты. Менялось только то, что раньше ВРАЛО ("cancelled"
		// при cancelled:0), а не словарь статусов.
		return "no_active_generation", "на кластере нет доступных llama.cpp-бэкендов — отменять некому"
	case len(accepted) == 0:
		return "unreachable", "ни один воркер не принял приказ об отмене (сеть/токен/адрес); смотрите results[].ok"
	default:
		return "not_cancelled", "воркеры приняли приказ, но не отменили ни одной генерации: " +
			"либо активных генераций нет, либо имя модели не совпало с именем загруженной модели " +
			"(балансер сверяет мягко, воркер — точно)"
	}
}

// cancelTargets — кого просить прекратить генерацию.
//
//   - указана модель → бэкенды, где она ЗАГРУЖЕНА (по метрикам), иначе все
//     healthy llama.cpp: воркер сам скажет «нет активных» или отменит;
//   - модель не указана → все healthy llama.cpp (у воркера есть отмена по
//     request_id/user_id, а пустой приказ отменяет активные генерации модели).
func (p *Proxy) cancelTargets(model string) []string {
	out := make([]string, 0, 4)
	seen := map[string]struct{}{}
	add := func(id string) {
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	backends := p.GetAllBackends()
	for i := range backends {
		b := &backends[i]
		if normalizeBackendType(b.Type) != types.BackendTypeLlamaCpp {
			continue
		}
		if b.Status != types.StatusHealthy && b.Status != "" {
			continue
		}
		if model == "" {
			add(b.ID)
			continue
		}
		if lm := p.llamaMetricsForPresence(b.ID); lm != nil {
			for _, m := range lm.LoadedModels {
				if modelNameMatches(m.Name, model) {
					add(b.ID)
					break
				}
			}
		}
	}
	// Модель нигде не загружена — шлём всем: приказ дешёвый, а воркер, у которого
	// генерация всё-таки идёт, отменит её.
	if len(out) == 0 && model != "" {
		for i := range backends {
			b := &backends[i]
			if normalizeBackendType(b.Type) == types.BackendTypeLlamaCpp &&
				(b.Status == types.StatusHealthy || b.Status == "") {
				add(b.ID)
			}
		}
	}
	return out
}

// sendCancelToBackend — один воркер: POST /api/cancel с тела клиента.
func (p *Proxy) sendCancelToBackend(backendID string, req clientCancelBody) (bool, map[string]interface{}) {
	addr := p.backendHTTPAddrByID(backendID)
	if addr == "" {
		return false, nil
	}
	payload := map[string]interface{}{}
	if req.Model != "" {
		payload["model"] = req.Model
	}
	if req.RequestID != "" {
		payload["request_id"] = req.RequestID
	}
	if req.UserID != "" {
		payload["user_id"] = req.UserID
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false, nil
	}
	httpReq, err := http.NewRequest(http.MethodPost, addr+"/api/cancel", bytes.NewReader(body))
	if err != nil {
		return false, nil
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if b := p.GetBackend(backendID); b != nil && b.CppWorkerApiToken != "" {
		httpReq.Header.Set("X-API-Token", b.CppWorkerApiToken)
	}
	client := &http.Client{Timeout: clientCancelTimeout}
	resp, err := client.Do(httpReq)
	if err != nil {
		if logger.Get() != nil {
			logger.Get().Warnw(i18n.T("balancer.cancel_client.not_accepted_warn"),
				"backend", backendID, "model", req.Model, "error", err)
		}
		return false, nil
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	detail := map[string]interface{}{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &detail)
	}
	return resp.StatusCode >= 200 && resp.StatusCode < 300, detail
}

// writeClientCancelError — короткий JSON-отказ (клиенты ждут JSON, не текст).
func writeClientCancelError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, code, map[string]interface{}{"error": msg})
}

// trimSpaceOnly — убрать пробелы вокруг имени модели. РЕГИСТР НЕ МЕНЯЕМ:
// имена моделей бывают смешанными (Qwen3.8-27B), и приведение к нижнему регистру
// могло бы не совпасть на воркере.
func trimSpaceOnly(s string) string {
	return strings.TrimSpace(s)
}
