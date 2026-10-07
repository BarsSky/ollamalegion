// embedded.go — запуск агента ВНУТРИ процесса воркера (R-Image, 2026-10-07).
//
// ЗАЧЕМ. До этого метрики image-бэкенда собирал отдельный контейнер `agent`,
// который регистрировался в балансере как ОТДЕЛЬНАЯ запись (host=agent,
// backendType=llama_cpp) — то есть об одном физическом воркере появлялись две
// записи. Симптом на живой стойке: у imageworker в /api/v1/backends стояло
// hasAgent=false и agentPort=18032 (порт ЧУЖОГО контейнера), а gpuMemory и
// vramUsagePercent приходили нулями — страница модели в WebUI выглядела
// «странно», хотя воркер был жив и healthy.
//
// ЧТО ДЕЛАЕТ ЭТОТ ФАЙЛ. Даёт воркеру поднять агента в своём процессе, под ТЕМ ЖЕ
// ID, под которым воркер регистрирует себя сам. Балансер в ветке «backend
// exists» принимает регистрацию агента, выставляет HasAgent=true и начинает
// писать метрики в ту же запись. Дополнительный контейнер для image-воркера
// становится не нужен.
//
// ЧЕГО ЭТО НЕ ДЕЛАЕТ. Не меняет поведение внешнего агента: `cmd/agent` работает
// как раньше (Ollama-стенды, где Go-кода воркера нет вовсе).
//
// ЖИЗНЕННЫЙ ЦИКЛ. RunEmbedded НЕ фатален: если регистрация не удалась (балансер
// ещё не поднялся), воркер продолжает работать, а горутина-сторож повторяет
// попытку с интервалом. Ошибка сборки конфига тоже не фатальна — воркер
// сообщает о ней в лог и живёт дальше. Это принципиально: метрики — вспомогательная
// функция, из-за неё inference-воркер падать не должен.
package agent

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// EmbeddedOptions — параметры встроенного агента.
//
// Воркер знает о себе то, чего не знает отдельный контейнер: свой backendType,
// свой реальный порт и ID записи в балансере. Поэтому эти значения передаются
// явно, а не читаются из AGENT_*-переменных.
type EmbeddedOptions struct {
	// AgentID — ID записи бэкенда в балансере. Встроенный агент регистрируется
	// ПОД ЭТИМ ЖЕ ID, чтобы не создавать вторую запись о том же воркере.
	AgentID string
	// Host — хост воркера, каким его видит балансер (имя контейнера).
	Host string
	// BackendType — тип бэкенда: llama_cpp или image_cpp.
	BackendType types.BackendType
	// Port — порт HTTP API самого воркера (cppworker 18092, sdworker 18093).
	Port int
	// BalancerURL — базовый URL балансера (http://loadbalancer:18081).
	BalancerURL string
	// BalancerToken — X-API-Token для защищённых эндпоинтов балансера.
	BalancerToken string
	// CppWorkerAPIToken — токен, который ждёт сам воркер на защищённых
	// эндпоинтах (в балансер он уходит полем cppWorkerApiToken).
	CppWorkerAPIToken string
	// MetricsPort — порт, на котором встроенный агент поднимает /health и
	// отдаёт метрики для опроса балансером. 0 → types.DefaultEmbeddedAgentPort.
	MetricsPort int
	// MaxConcurrentRequests — вместимость воркера (<=0 = «не задано»).
	MaxConcurrentRequests int
	// GPUMode — auto/gpu/cpu.
	GPUMode string
	// Weight — приоритет бэкенда (<=0 → 1).
	Weight int
	// CollectInterval — период сбора метрик, секунды (<=0 → 5).
	CollectInterval int
	// HeartbeatInterval — период heartbeat, секунды (<=0 → 5).
	HeartbeatInterval int
}

// embeddedAgentBundle — то, что возвращает RunEmbedded.
type embeddedAgentBundle struct {
	agent  *Agent
	stopCh chan struct{}
	once   sync.Once
}

// Stop — останавливает встроенного агента (идемпотентно).
func (b *embeddedAgentBundle) Stop() {
	if b == nil {
		return
	}
	b.once.Do(func() {
		close(b.stopCh)
		if b.agent != nil {
			b.agent.Stop()
		}
	})
}

// ResolveEmbeddedOptionsForWorker — собирает параметры встроенного агента из
// окружения конкретного воркера.
//
// ВЫКЛЮЧАТЕЛЬ. AGENT_EMBEDDED (on/true/1/yes) — по умолчанию ВЫКЛЮЧЕНО, чтобы
// раскатка новой сборки не меняла поведение стендов, где агент уже поднят
// отдельным контейнером (двойной сборщик метрик не нужен и мешает сравнению).
//
// ПАРАМЕТРЫ. `backendIDEnv` / `hostEnv` — имена переменных ИМЕННО этого воркера
// (SDWORKER_BACKEND_ID, CPPWORKER_BACKEND_ID): у воркеров своя схема имён, и
// подставлять чужие значения нельзя — агент обязан зарегистрироваться под тем же
// ID, что и сам воркер, иначе появится вторая запись о том же процессе.
//
// Значения по умолчанию для портов намеренно НЕ совпадают с портами внешнего
// агента (18032) — иначе в переходный период два агента подрались бы за порт.
func ResolveEmbeddedOptionsForWorker(backendType types.BackendType, backendIDEnv, hostEnv string, workerPort int) EmbeddedOptions {
	if !EmbeddedEnabled() {
		return EmbeddedOptions{}
	}
	opts := EmbeddedOptions{
		AgentID:               firstNonEmpty(os.Getenv("AGENT_ID"), envValue(backendIDEnv)),
		Host:                  firstNonEmpty(os.Getenv("AGENT_PUBLIC_HOST"), envValue(hostEnv)),
		BackendType:           backendType,
		BalancerURL:           strings.TrimSpace(os.Getenv("BALANCER_URL")),
		BalancerToken:         strings.TrimSpace(os.Getenv("BALANCER_TOKEN")),
		MetricsPort:           envIntAny([]string{"AGENT_EMBEDDED_PORT", "AGENT_PORT"}, types.DefaultEmbeddedAgentPort),
		MaxConcurrentRequests: envIntAny([]string{"AGENT_MAX_CONCURRENT_REQUESTS"}, 0),
		GPUMode:               strings.TrimSpace(os.Getenv("GPU_MODE")),
		Weight:                envIntAny([]string{"AGENT_WEIGHT"}, 1),
		CollectInterval:       envIntAny([]string{"METRICS_INTERVAL", "COLLECT_INTERVAL"}, 5),
		HeartbeatInterval:     envIntAny([]string{"HEARTBEAT_INTERVAL"}, 5),
		CppWorkerAPIToken: firstNonEmpty(
			os.Getenv("API_TOKEN"),
			os.Getenv("CPPWORKER_API_TOKEN"),
			os.Getenv("BALANCER_API_TOKEN"),
		),
		// Порт воркера: воркер знает его точно (cfg.Port). Env-переменная имеет
		// приоритет — оператор мог поменять порт в рантайме.
		Port: envIntAny([]string{"AGENT_WORKER_PORT"}, workerPort),
	}

	if opts.AgentID == "" {
		if h, err := os.Hostname(); err == nil && h != "" {
			opts.AgentID = h
		}
	}
	if opts.Host == "" {
		opts.Host = opts.AgentID
	}
	if opts.GPUMode == "" {
		opts.GPUMode = "auto"
	}
	if opts.Weight <= 0 {
		opts.Weight = 1
	}
	if opts.CollectInterval <= 0 {
		opts.CollectInterval = 5
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = 5
	}
	return opts
}

// envValue — значение переменной по имени (пустое имя → пустая строка).
func envValue(name string) string {
	if name == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv(name))
}

// EmbeddedEnabled — включён ли встроенный агент (AGENT_EMBEDDED).
func EmbeddedEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AGENT_EMBEDDED"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// embeddedContextOK — минимальная проверка, без которой запуск бессмысленен.
func (o EmbeddedOptions) validate() error {
	if strings.TrimSpace(o.AgentID) == "" {
		return fmt.Errorf("agent id is empty (AGENT_ID/SDWORKER_BACKEND_ID/CPPWORKER_BACKEND_ID/hostname)")
	}
	if strings.TrimSpace(o.BalancerURL) == "" {
		return fmt.Errorf("BALANCER_URL is empty")
	}
	if o.BackendType != types.BackendTypeLlamaCpp && o.BackendType != types.BackendTypeImage && o.BackendType != types.BackendTypeOllama {
		return fmt.Errorf("unsupported backend type %q", o.BackendType)
	}
	if o.MetricsPort <= 0 || o.MetricsPort > 65535 {
		return fmt.Errorf("invalid metrics port %d", o.MetricsPort)
	}
	return nil
}

// BuildEmbeddedConfig — AgentConfig для встроенного агента.
func BuildEmbeddedConfig(opts EmbeddedOptions) *types.AgentConfig {
	host := opts.Host
	if host == "" {
		host = opts.AgentID
	}
	cfg := &types.AgentConfig{
		AgentID:               opts.AgentID,
		BalancerURL:           opts.BalancerURL,
		BalancerToken:         opts.BalancerToken,
		BackendType:           opts.BackendType,
		MetricsPort:           opts.MetricsPort,
		CollectInterval:       opts.CollectInterval,
		HeartbeatInterval:     opts.HeartbeatInterval,
		PublicHost:            host,
		GPUMode:               types.PlatformMode(opts.GPUMode),
		NVMLEnabled:           true, // метрики GPU без NVML недостоверны
		MaxModels:             1,    // одна модель на процесс sd-server / cppworker
		Weight:                opts.Weight,
		CppWorkerApiToken:     opts.CppWorkerAPIToken,
		MaxConcurrentRequests: opts.MaxConcurrentRequests,
	}
	switch opts.BackendType {
	case types.BackendTypeLlamaCpp:
		cfg.CppWorkerHost = host
		cfg.CppWorkerPort = opts.Port
		cfg.CppWorkerURL = fmt.Sprintf("http://%s:%d", host, opts.Port)
	case types.BackendTypeImage:
		// Image-воркер: CppWorkerPort НЕ задаём — иначе балансер ушёл бы в
		// ветку de-dup «прикрепить к cppworker-бэкенду» и приклеил бы image-агента
		// к текстовой записи. Порт воркера уходит полем imagePort.
		cfg.ImagePort = opts.Port
		cfg.CppWorkerURL = fmt.Sprintf("http://%s:%d", host, opts.Port)
	}
	return cfg
}

// RunEmbedded — поднимает агента в текущем процессе и НЕ падает при неудаче.
//
// Возвращает bundle для остановки. nil означает «агент не запускался»: либо
// выключен (AGENT_EMBEDDED), либо конфиг неполон. Воркер обязан просто
// игнорировать nil и продолжать работу.
func RunEmbedded(opts EmbeddedOptions) *embeddedAgentBundle {
	if err := opts.validate(); err != nil {
		if logger.Get() != nil {
			logger.Get().Warnw("embedded agent disabled: invalid options", "error", err)
		}
		return nil
	}
	cfg := BuildEmbeddedConfig(opts)
	bundle := &embeddedAgentBundle{stopCh: make(chan struct{})}

	start := func() *Agent {
		instance := NewAgent(cfg)
		if err := instance.Start(); err != nil {
			if logger.Get() != nil {
				logger.Get().Warnw("embedded agent registration failed, will retry",
					"agentId", cfg.AgentID, "balancer", cfg.BalancerURL, "error", err)
			}
			return nil
		}
		return instance
	}

	instance := start()
	bundle.agent = instance
	if instance != nil && logger.Get() != nil {
		logger.Get().Infow("embedded agent started",
			"agentId", cfg.AgentID, "backendType", string(cfg.BackendType),
			"metricsPort", cfg.MetricsPort, "workerPort", opts.Port)
	}

	// Сторож: если первая регистрация не удалась — повторяем с интервалом.
	// Метрики вспомогательны: воркер не должен ни падать, ни ждать балансер.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-bundle.stopCh:
				return
			case <-ticker.C:
				if bundle.agent != nil {
					continue
				}
				if instance := start(); instance != nil {
					bundle.agent = instance
					if logger.Get() != nil {
						logger.Get().Infow("embedded agent registered after retry", "agentId", cfg.AgentID)
					}
				}
			}
		}
	}()

	return bundle
}

// envIntAny — первая корректная целочисленная env-переменная из списка.
func envIntAny(names []string, fallback int) int {
	for _, name := range names {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
	}
	return fallback
}

// firstNonEmpty — первое непустое значение (после TrimSpace).
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}
