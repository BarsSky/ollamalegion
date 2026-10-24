package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// operatorMaxConcurrentRequests — явно заданный оператором лимит
// (AGENT_MAX_CONCURRENT_REQUESTS), 0 = не задан.
//
// R119 (2026-10-15): читаем ПЕРЕМЕННУЮ, а не a.config.MaxConcurrentRequests.
// В a.config то же поле перезаписывается значением из ответа балансера
// (collector.go: "Heartbeat response: applied maxConcurrentRequests=…"), и по нему
// нельзя отличить «оператор задал» от «балансер вернул».
func operatorMaxConcurrentRequests() int {
	for _, name := range []string{"AGENT_MAX_CONCURRENT_REQUESTS"} {
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			continue
		}
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// claimMaxConcurrent — какую вместимость агент ЗАЯВЛЯЕТ балансеру при регистрации.
//
// ЖИВОЙ ДЕФЕКТ (стенд, 2026-10-15). cppworker честно отдавал `max_slots=1` и
// `enableBatchedParallel=false`, агент в логе писал «сообщаем балансеру 1» — а
// `/api/v1/backends` показывал `maxConcurrentRequests=2 src=static`, и второй
// клиент снова ждал молча внутри cppworker вместо очереди балансера.
//
// Причина — самоподдерживающаяся петля:
//
//	state.json (старое 2) → heartbeat отдаёт 2 → агент кладёт 2 в
//	a.config.MaxConcurrentRequests → регистрация заявляет 2 → балансер
//	сохраняет 2 → heartbeat снова отдаёт 2 → …
//
// Точно та же петля «эхо», что описана в R83, но починка R83 закрыла только
// ветку attach, а не источник значения: заявка по-прежнему бралась из поля,
// которое перезаписывает heartbeat.
//
// ПРАВИЛА:
//  1. явный операторский лимит (env) — приоритетен;
//  2. иначе ФАКТИЧЕСКАЯ способность узла (detected, учитывает
//     enableBatchedParallel — без планировщика она равна 1);
//  3. echo — только последний fallback, когда узел не ответил вообще.
func claimMaxConcurrent(operator, detected, echoed int) int {
	if operator > 0 {
		return operator
	}
	if detected > 0 {
		return detected
	}
	if echoed > 0 {
		return echoed
	}
	return 0
}

// register - регистрация на балансировщике
func (a *Agent) register() error { // Сбор информации о системе
	hostname, _ := os.Hostname()
	osName := os.Getenv("OS")
	if osName == "" {
		osName = "linux"
	}

	gpuInfo := a.collectGPUInfo()
	publicHost := a.getPublicHost()

	fmt.Printf("[%s] Detected public host for registration: %s\n",
		time.Now().Format(time.RFC3339), publicHost)

	// Извлекаем порт Ollama из конфигурации (OLLAMA_URL)
	ollamaPort := a.extractOllamaPort()

	// 2026-06-30: для host/cppWorkerPort в register-payload используем координаты
	// ФИЗИЧЕСКОГО cppworker-бэкенда, а не самого agent'а. Это критично для de-dup
	// по (host, port) в /api/v1/gguf/backends: и cppworker-bundled, и agent-бэкенд
	// должны попадать в одну группу (host=cppworker-gpu, port=18092) и WebUI будет
	// показывать ровно одну запись.
	//
	// publicHost (== AGENT_PUBLIC_HOST == имя контейнера agent'а) теперь используется
	// только для agentPort/healthcheck самого agent'а, а не для хоста бэкенда.
	registerHost := publicHost
	registerCppWorkerPort := 0
	if a.config.BackendType == types.BackendTypeLlamaCpp {
		registerHost = a.extractCppWorkerHost()
		registerCppWorkerPort = a.extractCppWorkerPort()
		fmt.Printf("[%s] Registering llama_cpp backend at %s:%d (agent at %s)\n",
			time.Now().Format(time.RFC3339), registerHost, registerCppWorkerPort, publicHost)
	}

	// Отправляем регистрацию напрямую в формате, который ожидает балансировщик
	//
	// R83 (2026-09-28): передаём реальную вместимость воркера.
	// Раньше поля не было, и балансер ставил MaxConcurrentReqs=10 константой —
	// для llama.cpp это завышение (n_parallel=1 в C-bridge), из-за которого
	// admission-очередь считала доступными 10 слотов. Значение <= 0 означает
	// «не задано»: балансер применит дефолт по типу бэкенда.
	//
	// R83-fix (2026-09-29): «одна настройка — одно место».
	//
	// Было: вместимость задавалась в ДВУХ местах — CPPWORKER_N_PARALLEL
	// (сколько слотов реально поднимает cppworker) и AGENT_MAX_CONCURRENT_REQUESTS
	// (сколько запросов пропускает балансер). Рассинхрон между ними давал ровно
	// тот дефект, из-за которого «второй клиент получает 503»: агент сообщал 1,
	// балансер ставил запросы в очередь, хотя воркер держал 2 слота — либо
	// наоборот, пропускал 2 при одном слоте.
	//
	// Теперь при незаданном AGENT_MAX_CONCURRENT_REQUESTS агент спрашивает у
	// cppworker его defaultNParallel (env CPPWORKER_N_PARALLEL) и сообщает
	// балансеру ИМЕННО ЭТО значение. Явно заданный AGENT_MAX_CONCURRENT_REQUESTS
	// остаётся операторским лимитом и имеет приоритет.
	// Опрос cppworker делается ОДИН раз: он ходит по HTTP и печатает строку в лог.
	detected := a.detectCppWorkerParallelism()
	maxConcurrent := claimMaxConcurrent(
		operatorMaxConcurrentRequests(),
		detected,
		a.config.MaxConcurrentRequests,
	)
	// R121 (2026-10-15): запоминаем СВОЮ заявку — её будет слать каждый heartbeat
	// отдельным полем, чтобы балансер восстановил вместимость после своего
	// перезапуска, не дожидаясь перезапуска агента. Значение из ответа балансера
	// (echo) сюда НЕ подмешивается — иначе вернулась бы петля «эхо».
	a.setNodeCapacityClaim(claimMaxConcurrent(
		operatorMaxConcurrentRequests(),
		detected,
		0,
	))
	reqBody := map[string]interface{}{
		"agentId":               a.config.AgentID,
		"hostname":              hostname,
		"host":                  registerHost,
		"ollamaPort":            ollamaPort,
		"agentPort":             a.config.MetricsPort,
		"gpuCount":              gpuInfo.Count,
		"name":                  a.config.AgentID,
		"labels":                []string{osName, "amd64", string(a.platformMode)},
		"weight":                a.config.Weight,
		"backendType":           string(a.config.BackendType),
		"cppWorkerPort":         registerCppWorkerPort,
		"nodeLabels":            a.config.NodeLabels,
		"maxConcurrentRequests": maxConcurrent,
		// R83 (2026-09-28): токен, который ждёт cppworker. Без него балансер
		// не может авторизоваться на защищённых эндпоинтах cppworker, и правка
		// параметров модели из WebUI через прокси балансера возвращает 401
		// («invalid or missing API token»), хотя токен задан во всех .env.
		"cppWorkerApiToken": a.config.CppWorkerApiToken,
		// R101 (2026-10-11): версия сборки llama.cpp от узла.
		//
		// НАХОДКА ПРИЁМКИ (находка 6): в бандл-стенде бэкенд регистрирует
		// встроенный агент, а поле buildInfo клала только Go-сторона cppworker,
		// чья регистрация в этом стеке выключена (CPPWORKER_REGISTER_DISABLE).
		// Из-за этого buildInfo у бэкендов всегда оставался пустым, и валидатор
		// композиции не мог сверить версии сторон RPC — писал version_unknown.
		"buildInfo": llamaBuildInfoR101Agent(),
	}

	// R-Image (2026-10-07): image-бэкенд (sdworker/sd-server).
	//
	// Агент, встроенный в image-воркер, регистрируется ПОД ТЕМ ЖЕ ID, что и
	// саморегистрация воркера, и обязан прислать тот же imagePort: иначе
	// балансер в ветке «backend exists» пересобрал бы запись без imagePort, и
	// EffectiveImagePort() упал бы в fallback 18093 — адрес воркера в WebUI
	// разошёлся бы с реальным (например, при нестандартном SDWORKER_PORT).
	//
	// backendType=image_cpp агент берёт из BACKEND_TYPE (BackendTypeImage) —
	// это то же значение, что шлёт сам sdworker.
	if a.config.BackendType == types.BackendTypeImage {
		reqBody["imagePort"] = a.resolveImagePort()
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal registration: %w", err)
	}

	// Round 41 (2026-08-19): authedRequest устанавливает Content-Type, X-Agent-ID,
	// и (опционально) X-API-Token. Раньше header X-API-Token не выставлялся
	// вообще → 401 на /api/v1/agents/register → restart loop. Теперь
	// cfg.BalancerToken (из env BALANCER_TOKEN) используется для auth.
	registerURL := fmt.Sprintf("%s/api/v1/agents/register", a.balancerURL)
	httpReq, err := a.authedRequest(context.Background(), http.MethodPost, registerURL, bytes.NewReader(data))
	if err != nil {
		return err
	}

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("registration failed with status %d: %s", resp.StatusCode, string(body))
	}

	// Балансировщик возвращает {success, action, agentId, backend, message}
	var registerResp map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&registerResp); err != nil {
		return err
	}

	// Round 12 (2026-07-10): отличаем "attached" (dedup) от "created".
	// В bundled-режиме agent attach'ится к cppworker-бэкенду, а не создаёт новый.
	// Это устраняет дублирование: раньше для одного физического inference endpoint
	// было 2 бэкенда (cppworker + agent), теперь 1.
	if action, ok := registerResp["action"].(string); ok {
		if action == "attached" {
			bid, _ := registerResp["backendId"].(string)
			// Round 13 (2026-07-10): сохраняем backendId для нового heartbeat endpoint.
			// При attached режиме agentID != backendID, поэтому старый /agents/heartbeat
			// обновлял неправильный бэкенд. Новый /backends/{backendId}/agent/heartbeat
			// использует правильный ID.
			a.config.BackendID = bid
			fmt.Printf("[%s] Agent ATTACHED to existing cppworker backend %q (dedup, single backend for host=%s cppWorkerPort=%d)\n",
				time.Now().Format(time.RFC3339), bid, registerHost, registerCppWorkerPort)
		} else if action == "created" {
			// Standalone mode: agent создал свой бэкенд, backendId == agentId.
			a.config.BackendID = a.config.AgentID
			fmt.Printf("[%s] Agent created NEW backend (no cppworker at %s:%d, standalone mode)\n",
				time.Now().Format(time.RFC3339), registerHost, registerCppWorkerPort)
		}
	}

	if success, ok := registerResp["success"].(bool); ok && !success {
		errorMsg := "unknown"
		if msg, ok := registerResp["error"].(string); ok {
			errorMsg = msg
		}
		return fmt.Errorf("registration rejected: %s", errorMsg)
	}

	fmt.Printf("[%s] Agent registered successfully: %s\n",
		time.Now().Format(time.RFC3339), a.config.AgentID)
	return nil
}

// healthCheckOllama - проверка доступности Ollama перед регистрацией
func (a *Agent) healthCheckOllama() error {
	client := &http.Client{
		Timeout: 5 * time.Second,
	}
	resp, err := client.Get(fmt.Sprintf("%s/api/version", a.getOllamaBaseURL()))
	if err != nil {
		return fmt.Errorf("ollama health-check failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama health-check returned status %d", resp.StatusCode)
	}
	return nil
}

// llamaBuildInfoR101Agent - версия сборки llama.cpp, которую узел сообщает
// балансеру при регистрации.
//
// Источник тот же, что у Go-стороны cppworker (env CPPWORKER_LLAMA_BUILD,
// проставляет образ): встроенный агент живёт в том же процессе и видит то же
// окружение. Внешнему контейнеру agent переменную нужно передать отдельно -
// иначе версия не доедет, и проверка совместимости сторон RPC в валидаторе
// композиции останется неподтверждённой (version_unknown).
func llamaBuildInfoR101Agent() string {
	return strings.TrimSpace(os.Getenv("CPPWORKER_LLAMA_BUILD"))
}
