package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// getPublicHost - определение публичного хоста для регистрации
// Приоритет: AGENT_PUBLIC_HOST > auto-detected IP > hostname
func (a *Agent) getPublicHost() string {
	// Если явно задан PublicHost — используем его
	if a.config.PublicHost != "" {
		return a.config.PublicHost
	}

	// Попытка получить публичный IP
	publicIP := a.getOutboundIP()
	if publicIP != "" && publicIP != "127.0.0.1" {
		return publicIP
	}

	// Fallback на hostname
	hostname, _ := os.Hostname()
	if hostname != "" {
		return hostname
	}

	return "localhost"
}

// getOutboundIP - получение исходящего IP адреса (публичного интерфейса)
func (a *Agent) getOutboundIP() string {
	// Подключаемся к произвольному внешнему адресу для определения исходящего интерфейса
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	if localAddr == nil {
		return ""
	}
	return localAddr.IP.String()
}

// extractOllamaPort - извлечение порта Ollama из OllamaURL
func (a *Agent) extractOllamaPort() int {
	if a.config.OllamaURL != "" {
		// Парсим URL вида http://host:port или host:port
		urlStr := a.config.OllamaURL
		if !strings.HasPrefix(urlStr, "http://") && !strings.HasPrefix(urlStr, "https://") {
			urlStr = "http://" + urlStr
		}
		if u, err := url.Parse(urlStr); err == nil && u.Port() != "" {
			if port, err := strconv.Atoi(u.Port()); err == nil {
				return port
			}
		}
	}
	return 11434 // fallback
}

// getOllamaBaseURL - базовый URL Ollama из конфигурации
func (a *Agent) getOllamaBaseURL() string {
	if a.config.OllamaURL != "" {
		return a.config.OllamaURL
	}
	return "http://localhost:11434"
}

// cppWorkerProbeTimeout — сколько ждать ответ cppworker при определении
// вместимости. Переменная (а не константа), чтобы тест мог её укоротить.
var cppWorkerProbeTimeout = 5 * time.Second

// detectCppWorkerParallelism — R83-fix (2026-09-29): сколько параллельных
// запросов реально обслуживает cppworker.
//
// Источник — сам cppworker: GET /api/v1/cppworker/config → config.defaultNParallel
// (задаётся env CPPWORKER_N_PARALLEL). Эндпоинт открытый, авторизации не требует.
//
// ЗАЧЕМ. До этого вместимость задавалась в ДВУХ местах: CPPWORKER_N_PARALLEL
// (слоты воркера) и AGENT_MAX_CONCURRENT_REQUESTS (то, что агент сообщает
// балансеру). Рассинхрон давал ровно симптом «второй клиент получает 503»:
// агент сообщал 1, и балансер ставил второй запрос в очередь, хотя воркер держал
// два слота. Теперь при незаданном AGENT_MAX_CONCURRENT_REQUESTS агент берёт
// значение воркера, то есть настраивать надо ОДНО место.
//
// Возвращает 0, если узнать не удалось (тогда балансер применит свой дефолт).
func (a *Agent) detectCppWorkerParallelism() int {
	base := a.config.CppWorkerURL
	if base == "" {
		host := a.extractCppWorkerHost()
		port := a.extractCppWorkerPort()
		if host == "" || port <= 0 {
			return 0
		}
		base = fmt.Sprintf("http://%s:%d", host, port)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cppWorkerProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/cppworker/config", nil)
	if err != nil {
		return 0
	}
	client := a.httpClient
	if client == nil {
		client = &http.Client{Timeout: cppWorkerProbeTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("[%s] detectCppWorkerParallelism: cppworker недоступен (%v) — "+
			"вместимость оставляем на усмотрение балансера\n",
			time.Now().Format(time.RFC3339), err)
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	var parsed struct {
		Config struct {
			DefaultNParallel int `json:"defaultNParallel"`
		} `json:"config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return 0
	}
	if parsed.Config.DefaultNParallel > 0 {
		fmt.Printf("[%s] detectCppWorkerParallelism: cppworker сообщил defaultNParallel=%d\n",
			time.Now().Format(time.RFC3339), parsed.Config.DefaultNParallel)
		return parsed.Config.DefaultNParallel
	}
	return 0
}

// extractCppWorkerHost - возвращает host физического cppworker-бэкенда.
//
// 2026-06-30: раньше для отправки register() в балансер использовался
// `getPublicHost()` (== PublicHost == AGENT_PUBLIC_HOST == имя контейнера agent'а),
// что давало ключ (host=cppworker-gpu-bundled-agent, port=18091), отличный от
// ключа cppworker-бэкенда (host=cppworker-gpu, port=18092). De-dup в
// /api/v1/gguf/backends по (host, port) НЕ срабатывал, и WebUI показывал
// два бэкенда с одним физическим endpoint.
//
// Приоритет (для llama_cpp):
//  1. CppWorkerHost из AgentConfig (compose выставляет AGENT_CPPWORKER_HOST=cppworker-gpu).
//  2. Хост из CppWorkerURL (если задан).
//  3. PublicHost (как fallback — например, для локальной разработки).
//  4. "localhost" (последний resort).
func (a *Agent) extractCppWorkerHost() string {
	if a.config.CppWorkerHost != "" {
		return a.config.CppWorkerHost
	}
	if a.config.CppWorkerURL != "" {
		if u, err := url.Parse(a.config.CppWorkerURL); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	if a.config.PublicHost != "" {
		return a.config.PublicHost
	}
	return "localhost"
}

// extractCppWorkerPort - извлечение порта физического cppworker.
//
// 2026-06-30: раньше возвращал 18091 (legacy) как fallback для llama_cpp,
// хотя в bundled-compose cppworker слушает 18092. Это давало port=18091
// в register-payload и ломало de-dup с cppworker-бэкендом (port=18092).
//
// Приоритет:
//  1. CppWorkerPort из AgentConfig (compose выставляет AGENT_CPPWORKER_PORT=18092).
//  2. Порт из CppWorkerURL (если задан).
//  3. 18092 для llama_cpp (актуальный bundled-default).
//  4. 0 для остальных типов.
func (a *Agent) extractCppWorkerPort() int {
	if a.config.CppWorkerPort > 0 {
		return a.config.CppWorkerPort
	}
	if a.config.CppWorkerURL != "" {
		if u, err := url.Parse(a.config.CppWorkerURL); err == nil && u.Port() != "" {
			if port, err := strconv.Atoi(u.Port()); err == nil && port > 0 {
				return port
			}
		}
	}
	// Fallback: 18092 (актуальный дефолт для bundled llama.cpp, см. cmd/cppworker/main.go).
	if a.config.BackendType == types.BackendTypeLlamaCpp {
		return 18092
	}
	return 0
}
