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

	status := "cancelled"
	if len(accepted) == 0 {
		status = "no_active_generation"
	}
	if logger.Get() != nil {
		logger.Get().Infow("client cancel: отмена от клиента разослана воркерам",
			"model", req.Model, "request_id", req.RequestID, "user_id", req.UserID,
			"targets", targets, "accepted", accepted, "status", status)
	}
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":   status,
		"model":    req.Model,
		"backends": accepted,
		// results — детали по каждому воркеру: сколько отменено и статус. Клиенту
		// достаточно "cancelled", оператору нужен ответ «почему не остановилось».
		"results": results,
	})
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
			logger.Get().Warnw("client cancel: воркер не принял отмену",
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
