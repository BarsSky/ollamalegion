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

	"ollama-loadbalancer/internal/i18n"
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

// resolveImagePort — R-Image (2026-10-07): порт image-воркера для регистрации.
//
// Источники по приоритету:
//  1. AGENT_IMAGE_PORT — явная настройка (совпадает по смыслу с SDWORKER_PORT);
//  2. ImagePort из AgentConfig (его заполняет встроенный агент — воркер знает
//     свой реальный порт и передаёт его напрямую);
//  3. types.DefaultImageWorkerPort (18093).
//
// Значение <= 0 трактуется как «не задано»: порт 0 в записи бэкенда сделал бы
// EffectiveImagePort() неопределённым.
func (a *Agent) resolveImagePort() int {
	if v := strings.TrimSpace(os.Getenv("AGENT_IMAGE_PORT")); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 && p <= 65535 {
			return p
		}
	}
	if a.config.ImagePort > 0 {
		return a.config.ImagePort
	}
	return types.DefaultImageWorkerPort
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
		fmt.Printf("[%s] %s\n", time.Now().Format(time.RFC3339),
			i18n.T("agent.collector_network.cppworker_unavailable", err))
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	var parsed struct {
		Config struct {
			DefaultNParallel int `json:"defaultNParallel"`
			// R119 (2026-10-15): режим планировщика. Указатель, чтобы отличить
			// «cppworker явно сказал false» от «старая сборка поля не отдаёт»:
			// в последнем случае поведение менять нельзя.
			EnableBatchedParallel *bool `json:"enableBatchedParallel"`
		} `json:"config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return 0
	}
	if parsed.Config.DefaultNParallel > 0 {
		n := parsed.Config.DefaultNParallel
		// R119 (2026-10-15): n_parallel — это КОНФИГУРАЦИЯ, а не способность.
		//
		// ЖИВОЙ ЗАМЕР (стенд, CPPWORKER_ENABLE_BATCHED_PARALLEL=false,
		// CPPWORKER_N_PARALLEL=2, загружено parallel=2/max_slots=2). Два
		// одновременных /api/chat НАПРЯМУЮ в cppworker:
		//
		//	A(long):  headers=1.09 c  firstData=1.09 c  total=12.29 c
		//	B(short): headers=11.76 c firstData=11.76 c total=11.76 c  ← конец A
		//
		// То есть без batched-планировщика legacy-путь держит inst.mu на весь
		// инференс (Round 8 race-fix) и llama_decode сериализуется — ровно как
		// написано в ModelInfo.BatchedParallel («false = legacy Round 13
		// multi-slot path с serialized llama_decode»). Агент же сообщал балансеру
		// 2, балансер пропускал двух клиентов, и второй ждал МОЛЧА внутри
		// cppworker: без X-Queue-Wait-Ms, без позиции в очереди и без строки в
		// UI. Это и есть жалоба «два запроса с одной машины не разделяются».
		//
		// Поэтому при явном enableBatchedParallel=false заявляем 1: балансер
		// поставит второго клиента в admission-очередь, где ожидание ВИДНО.
		if n > 1 && parsed.Config.EnableBatchedParallel != nil && !*parsed.Config.EnableBatchedParallel {
			fmt.Printf("[%s] %s\n", time.Now().Format(time.RFC3339),
				i18n.T("agent.collector_network.batched_disabled", n))
			return 1
		}
		fmt.Printf("[%s] %s\n", time.Now().Format(time.RFC3339),
			i18n.T("agent.collector_network.parallelism_reported", n))
		return n
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
