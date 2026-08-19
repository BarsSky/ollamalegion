package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// register - регистрация на балансировщике
func (a *Agent) register() error {
	// Сбор информации о системе
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
	reqBody := map[string]interface{}{
		"agentId":     a.config.AgentID,
		"hostname":    hostname,
		"host":        registerHost,
		"ollamaPort":  ollamaPort,
		"agentPort":   a.config.MetricsPort,
		"gpuCount":    gpuInfo.Count,
		"name":        a.config.AgentID,
		"labels":      []string{osName, "amd64", string(a.platformMode)},
		"weight":        a.config.Weight,
		"backendType":   string(a.config.BackendType),
		"cppWorkerPort": registerCppWorkerPort,
		"nodeLabels":    a.config.NodeLabels,
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