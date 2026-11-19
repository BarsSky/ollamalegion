// loading_retry.go — helpers для прозрачного retry при 503 "model is loading".
//
// Проблема (2026-07-01): cppworker при первом inference-запросе после старта
// или после unload запускает lazy load и отвечает HTTP 503:
//
//	{"error":"model is loading: <name>","loading":true,
//	 "model":<name>,"elapsedMs":N,"retryAfterMs":3000}
//
// До этого фикса балансировщик проксировал 503 as-is → OpenWebUI/Cline
// видели оборванный стрим и не получали модель. WebUI позволял загрузить
// вручную через /api/v1/cluster/models/{name}/reload, но для inference-пути
// retry отсутствовал.
//
// Решение: при получении 503 с признаком "model is loading" балансировщик
// polling'ом опрашивает /api/models/load/progress (llama.cpp) или /api/ps
// (ollama) на том же бэкенде, ждёт завершения загрузки и повторяет основной
// запрос. Это делает lazy-load прозрачным для клиента.
//
// Лимиты:
//   - бюджет ожидания — ПО РАЗМЕРУ МОДЕЛИ (R547, T-9 аудита), а не плоские 30 с:
//     измеренные холодные загрузки 16–27B на стенде — 140–485 с, поэтому 30 с
//     обрывали ожидание раньше, чем загрузка успевала начаться. Явная настройка
//     `LB_LOADING_RETRY_MAX_SEC` сильнее оценки.
//   - interval 500ms (типичный poll на завершение lazy-load).
//   - early exit если context отменён клиентом.
package balancer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// Loading retry configuration.
const (
	// loadingRetryInterval — задержка между опросами /api/models/load/progress.
	loadingRetryInterval = 500 * time.Millisecond

	// loadingRetryHTTPTimeout — таймаут одного HTTP-опроса /progress (короткий,
	// потому что endpoint дешёвый и должен отвечать быстро).
	loadingRetryHTTPTimeout = 2 * time.Second

	// loadingRetryMinBudget — нижняя граница бюджета: даже для модели без
	// известного размера ждать меньше, чем длится прокрутка слотов, бессмысленно.
	loadingRetryMinBudget = 30 * time.Second
)

// loadingRetryBudgetFromEnv — явный opt-in оператора: `LB_LOADING_RETRY_MAX_SEC`.
//
// Читается напрямую (а не через envSeconds): тот уважает мастер-выключатель
// `LB_STREAMING_NEVER_TIMEOUT`, который про ТАЙМАУТЫ СТРИМА, а здесь речь об
// ожидании загрузки — смешивать их нельзя, иначе «стрим без таймаута» отключил бы
// и границу ожидания.
func loadingRetryBudgetFromEnv() (time.Duration, bool) {
	raw := strings.TrimSpace(os.Getenv("LB_LOADING_RETRY_MAX_SEC"))
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, false
	}
	d := time.Duration(n) * time.Second
	if d < loadingRetryMinBudget {
		d = loadingRetryMinBudget
	}
	return d, true
}

// loadingRetryBudget — сколько ждать завершения lazy-load (R547, T-9).
//
// ПОЧЕМУ НЕ КОНСТАНТА 30 с. До R547 бюджет был `60 попыток × 500 мс = 30 с`, а
// комментарий рядом утверждал «достаточно для cold-start 22GB». Живые числа
// того же стенда (шапка autoload_wait.go, docs/architecture/debts.ru.md):
// 16–27B грузятся 140–485 с. То есть клиент получал 503 «model is still loading
// after 30s» ЗАДОЛГО до того, как модель вообще успевала подняться.
//
// Источник числа тот же, что у границы первого байта (R498/R523): эстиматор по
// размеру модели. Явный `LB_LOADING_RETRY_MAX_SEC` сильнее.
func (p *Proxy) loadingRetryBudget(modelName string) (time.Duration, string) {
	if d, ok := loadingRetryBudgetFromEnv(); ok {
		return d, "env LB_LOADING_RETRY_MAX_SEC"
	}
	if p != nil {
		if size := p.getModelSizeBytes(modelName); size > 0 {
			d := EstimateFirstByteTimeoutFromModelSize(size)
			if d < loadingRetryMinBudget {
				d = loadingRetryMinBudget
			}
			return d, fmt.Sprintf("оценка по размеру модели (%d байт)", size)
		}
	}
	// Размер неизвестен — берём оценку для неизвестного размера (900 с), а не
	// прежние 30 с: «неизвестно» не значит «быстро».
	d := EstimateFirstByteTimeoutFromModelSize(0)
	if d < loadingRetryMinBudget {
		d = loadingRetryMinBudget
	}
	return d, "оценка для неизвестного размера"
}

// loadingRetryBudgetSeconds — тот же бюджет в секундах: нужен для текста отказа
// клиенту, чтобы он называл ФАКТИЧЕСКОЕ ожидание, а не константу.
func (p *Proxy) loadingRetryBudgetSeconds(modelName string) int {
	d, _ := p.loadingRetryBudget(modelName)
	return int(d.Seconds())
}

// loadingSignalFromBody — пытается распознать 503 "model is loading" в JSON-ответе
// от бэкенда. Возвращает (isLoading, modelName, retryAfterMs).
//
// Поддерживаемые форматы:
//   - cppworker: {"error":"model is loading: <name>","loading":true,
//     "model":<name>,"elapsedMs":N,"retryAfterMs":3000}
//   - ollama:    {"error":"model '<name>' is loading"} (loading:false) — НЕ считаем loading
//   - fallback:  строка содержит "model is loading" (case-insensitive) и status=503
func loadingSignalFromBody(statusCode int, body []byte) (bool, string, int) {
	if statusCode != http.StatusServiceUnavailable {
		return false, "", 0
	}
	bodyStr := strings.ToLower(string(body))
	if !strings.Contains(bodyStr, "model is loading") &&
		!strings.Contains(bodyStr, "\"loading\":true") {
		return false, "", 0
	}

	var parsed struct {
		Error        string `json:"error"`
		Loading      bool   `json:"loading"`
		Model        string `json:"model"`
		RetryAfterMs int    `json:"retryAfterMs"`
	}
	_ = json.Unmarshal(body, &parsed)

	if !parsed.Loading && !strings.Contains(strings.ToLower(parsed.Error), "model is loading") {
		// Body содержит слово, но JSON-структура не сигнализирует loading.
		// Это может быть другой 503 — лучше не retry, отдать клиенту as-is.
		return false, "", 0
	}

	modelName := parsed.Model
	if modelName == "" && strings.HasPrefix(parsed.Error, "model is loading: ") {
		modelName = strings.TrimPrefix(parsed.Error, "model is loading: ")
	}

	retryAfterMs := parsed.RetryAfterMs
	if retryAfterMs == 0 {
		retryAfterMs = 3000
	}

	return true, modelName, retryAfterMs
}

// waitForBackendModelLoaded — опрашивает бэкенд пока модель не будет загружена.
//
// Возвращает:
//   - true, nil — модель загружена, можно повторять основной запрос.
//   - false, nil — polling исчерпан (max attempts), клиенту нужно отдать 503.
//   - false, err — context отменён или критическая сетевая ошибка.
//
// Для llama.cpp — GET /api/models/load/progress?model=<name>; считаем loaded,
// если state=="loaded" или endpoint ответил 404 (модель не в loading-списке,
// значит, она уже загружена или никогда не грузилась через /api/models/load —
// тогда полагаемся на основной запрос, который сам вызовет lazy load).
//
// Для ollama — GET /api/ps; считаем loaded, если имя модели в списке loaded.
func (p *Proxy) waitForBackendModelLoaded(
	ctx context.Context,
	backend *types.Backend,
	modelName string,
	backendID string,
) (bool, error) {
	if backend == nil || modelName == "" {
		return false, nil
	}

	engine := p.resolveBackendEngine(backend)
	baseURL := p.getBackendBaseURL(backend)

	budget, budgetSource := p.loadingRetryBudget(modelName)

	log := logger.Get()
	log.Infow("waitForBackendModelLoaded: start polling",
		"backend", backendID, "model", modelName,
		"engine", string(engine),
		"budget_sec", int(budget.Seconds()), "budget_source", budgetSource,
		"interval_ms", loadingRetryInterval.Milliseconds())

	pollClient := &http.Client{Timeout: loadingRetryHTTPTimeout}

	start := time.Now()
	deadline := start.Add(budget)
	for attempt := 1; ; attempt++ {
		// Check context cancellation (client disconnected).
		select {
		case <-ctx.Done():
			log.Warnw("waitForBackendModelLoaded: context canceled",
				"backend", backendID, "model", modelName, "attempt", attempt)
			return false, ctx.Err()
		default:
		}

		loaded, err := p.checkModelLoadedOnBackend(ctx, pollClient, engine, baseURL, modelName)
		if err != nil {
			// Сетевая ошибка опроса (refused/timeout) — бэкенд недоступен,
			// повторять бесполезно. Возвращаем false, основной код упадёт на retry
			// с этой же ошибкой (см. client.Do ниже).
			log.Warnw("waitForBackendModelLoaded: poll error",
				"backend", backendID, "model", modelName,
				"attempt", attempt, "error", err)
			return false, err
		}

		if loaded {
			elapsed := time.Since(start)
			log.Infow("waitForBackendModelLoaded: model is now loaded",
				"backend", backendID, "model", modelName,
				"attempt", attempt, "elapsed_ms", elapsed.Milliseconds())
			return true, nil
		}

		// Логируем каждые 5 секунд для диагностики, чтобы не засорять логи.
		if attempt == 1 || attempt%10 == 0 {
			log.Debugw("waitForBackendModelLoaded: still loading",
				"backend", backendID, "model", modelName,
				"attempt", attempt, "elapsed_ms", time.Since(start).Milliseconds(),
				"budget_sec", int(budget.Seconds()))
		}

		// R547: выход по ДЕДЛАЙНУ (раньше — по числу попыток, что и давало
		// «после 30 с»). Проверяем ПОСЛЕ опроса: загрузка могла завершиться
		// ровно на границе.
		if !time.Now().Before(deadline) {
			break
		}

		// Sleep с возможностью прерывания по context.
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(loadingRetryInterval):
		}
	}

	log.Warnw("waitForBackendModelLoaded: timeout, model still not loaded",
		"backend", backendID, "model", modelName,
		"budget_sec", int(budget.Seconds()), "budget_source", budgetSource,
		"elapsed_ms", time.Since(start).Milliseconds())
	return false, nil
}

// checkModelLoadedOnBackend — один опрос состояния загрузки.
//
// llama.cpp: GET /api/models/load/progress?model=<name>
//   - 200 OK с body.state=="loaded" — модель готова.
//   - 200 OK с body.state=="loading" — ещё грузится.
//   - 404 Not Found — модель не в loading-списке (вероятно уже загружена
//     через другой механизм), считаем loaded=true (best-effort).
//
// ollama: GET /api/ps
//   - 200 OK с body.models[] — ищем modelName в списке. Если есть — loaded.
//   - пустой список или нет модели — ещё не загружена.
func (p *Proxy) checkModelLoadedOnBackend(
	ctx context.Context,
	client *http.Client,
	engine types.BackendEngine,
	baseURL string,
	modelName string,
) (bool, error) {
	switch engine {
	case types.EngineLlamaCPP:
		return p.checkLlamaCppModelLoaded(ctx, client, baseURL, modelName)
	default:
		return p.checkOllamaModelLoaded(ctx, client, baseURL, modelName)
	}
}

func (p *Proxy) checkLlamaCppModelLoaded(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	modelName string,
) (bool, error) {
	url := fmt.Sprintf("%s/api/models/load/progress?model=%s", baseURL, urlPathEscape(modelName))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// 404 = модель не в loading-списке. Может быть уже загружена ранее
		// или никогда не грузилась через /api/models/load. Считаем loaded=true,
		// основной запрос либо успешно пройдёт, либо снова получит 503.
		return true, nil
	}

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("unexpected status %d from /api/models/load/progress", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}

	var progress struct {
		State string `json:"state"`
		Name  string `json:"name"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &progress); err != nil {
		// Best-effort: не парсится — считаем loaded=true (основной запрос
		// покажет реальный статус).
		return true, nil
	}

	switch progress.State {
	case "loaded":
		return true, nil
	case "loading":
		return false, nil
	case "error", "failed":
		// cppworker сообщил об ошибке загрузки — нет смысла retry, основной
		// запрос увидит ту же ошибку.
		return true, nil // best-effort: пусть основной запрос обработает
	default:
		// unknown state — best-effort loaded=true.
		return true, nil
	}
}

func (p *Proxy) checkOllamaModelLoaded(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	modelName string,
) (bool, error) {
	url := baseURL + "/api/ps"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("unexpected status %d from /api/ps", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}

	var ps struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &ps); err != nil {
		return false, err
	}

	// Ollama возвращает имя модели с тегами (например, "llama3:latest").
	// Сравниваем по точному совпадению И по префиксу (модель без тегов
	// должна матчить "model:latest" и "model").
	//
	// R66d (2026-09-23): для llama.cpp добавлена нормализация расширения .gguf.
	// Клиент (Cline/OpenWebUI) запрашивает "X.gguf", cppworker держит "X";
	// без нормализации балансер считал модель незагруженной и уходил в
	// ожидание/перезагрузку на каждом запросе.
	for _, m := range ps.Models {
		if m.Name == modelName || modelNameMatches(m.Name, modelName) {
			return true, nil
		}
		if strings.HasPrefix(m.Name, modelName+":") {
			return true, nil
		}
		if strings.HasPrefix(modelName, m.Name+":") {
			return true, nil
		}
	}

	return false, nil
}
