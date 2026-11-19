// image_tool.go — R84 (2026-10-03): инструмент генерации изображений для
// ТЕКСТОВЫХ моделей (cppworker) — объявление и серверное исполнение.
//
// ЗАЧЕМ. Оператор описал сценарий: текстовая модель на cppworker должна уметь
// «нарисовать картинку» по просьбе пользователя/агента, если в кластере поднят
// imageworker. Технически части уже были: cppworker инжектит инструменты клиента
// в system prompt и парсит tool_calls (cmd/cppworker/handlers_openai.go,
// tools_stream_content.go), балансер умеет детектировать tool_calls (в т.ч. в SSE) и
// маршрутизировать генерацию (image_router.go), а снимки image-бэкендов с
// загруженной моделью уже кэшируются (image_resources.go). Не было ровно двух
// вещей: инструмент никто не ОБЪЯВЛЯЛ модели и никто не ИСПОЛНЯЛ его вызов.
//
// ПОЧЕМУ ИСПОЛНЯЕТ БАЛАНСЕР, А НЕ КЛИЕНТ. Инструмент объявляет прокси, а не
// клиент: клиент (Open WebUI, LibreChat, Cline) про него не знает и исполнить не
// может. Поэтому вызов перехватывается здесь же: генерируем картинку сами,
// подкладываем результат в диалог как tool-сообщение и просим модель закончить
// ответ — пользователь видит текст + markdown-ссылку на изображение.
//
// ГЕЙТ «ЕСТЬ КОМУ ИСПОЛНЯТЬ» (требование оператора: передавать инструмент,
// только когда imageworker поднят):
//   - есть ЗДОРОВЫЙ image_cpp (filterBackendsByType отдаёт только healthy);
//   - его снимок достоверен (contractOK, без lastErr);
//   - в снимке есть ЗАГРУЖЕННАЯ модель (state=loaded) — иначе генерация
//     упёрлась бы в «no image model is loaded» уже после вызова инструмента.
//
// Не выполнено хотя бы одно — инструмент не добавляется вовсе, и в лог уходит
// причина (debug-уровень, чтобы не спамить на каждый чат).
//
// СОВМЕСТИМОСТЬ: без живого image-бэкенда поведение /v1/chat/completions не
// меняется ни на байт (инструмент не добавляется, поток не буферизуется).
package balancer

import (
	"os"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
)

// Флаги инструмента (соглашение проекта: LB_* читаются из окружения).
const (
	imageToolEnvEnabled  = "LB_IMAGE_TOOL"             // off | on (по умолчанию on)
	imageToolEnvMaxCalls = "LB_IMAGE_TOOL_MAX_CALLS"   // сколько картинок на один запрос
	imageToolEnvTimeout  = "LB_IMAGE_TOOL_TIMEOUT_SEC" // таймаут одной генерации
	imageToolEnvBaseURL  = "LB_IMAGE_TOOL_BASE_URL"    // внешний адрес балансера для ссылок
	// imageToolEnvAllowLoad — разрешено ли ВЫЗОВУ поднимать модель (R85).
	//
	// Было (R84): инструмент объявлялся только когда модель УЖЕ загружена, и
	// поднять её из вызова было нельзя. Стало: инструмент видит каталог и может
	// поднять выбранную модель сам (решение оператора, см.
	// docs/plans/2026-10-06-image-tool-catalog-autoload.md). `off` возвращает прежнее
	// поведение байт-в-байт: ни list_image_models, ни автозагрузки.
	imageToolEnvAllowLoad = "LB_IMAGE_TOOL_ALLOW_LOAD" // on | off (по умолчанию on)
	// imageToolEnvLoadTimeout — сколько ждать загрузку модели перед отказом.
	// Загрузка bundle'а в VRAM занимает секунды-минуты; бесконечное ожидание
	// держало бы и запрос клиента, и слот, поэтому таймаут обязателен, а его
	// истечение — честная ошибка в tool-результате (модель объяснит её
	// пользователю), а не молчаливый обрыв.
	imageToolEnvLoadTimeout = "LB_IMAGE_TOOL_LOAD_TIMEOUT_SEC"
	// imageToolEnvInject — КОМУ объявлять инструмент (R141, 2026-10-16):
	// always (по умолчанию, прежнее поведение) | auto.
	//
	// Живой дефект, ради которого появился режим: балансер объявлял инструмент
	// ВСЕМ клиентам, включая те, что не прислали ни одного tools[]. Воркер,
	// увидев непустой tools, вклеивает в промпт ~200-токенную преамбулу про
	// вызов инструментов — замер на стенде: prompt_tokens 19 без балансера
	// против 220 через балансер (+201 токен на КАЖДЫЙ запрос), has_tools=false
	// против true. Побочно модель уходит в reasoning: на «скажи ок» без
	// балансера ответ «Ок.», через балансер — `"The user simply said \""` с
	// reasoning_content, то есть бюджет ответа потрачен на размышление о
	// вызове инструмента, которого клиент не объявлял.
	//
	// Для клиента это ровно «нереальный расчёт токенов/с»: он видит 220
	// prompt-токенов там, где послал 19, и вдвое больший duration.
	//
	// auto — объявлять только когда клиент САМ прислал хотя бы один tools[]:
	// именно такой клиент умеет обрабатывать tool_calls. Стенды, где картинки
	// нужны и клиентам без инструментов, оставляют always (дефолт).
	//
	// R216 (2026-10-21): off — НЕ объявлять инструмент вовсе. Значение пришло из
	// живой практики: оператор выставляет LB_IMAGE_TOOL_INJECT=off, ожидая
	// «не добавляй tools», а прежний разбор знал только auto — любой другой
	// текст (включая off) молча означал always. Последствие на стенде: каждый
	// запрос OpenWebUI уходил в воркер с непустым tools[], попадал в
	// буферизированную ветку /v1 (см. handlers_openai.go: stream + tools), и
	// размышления приезжали клиенту как обычный content без отдельного
	// reasoning_content.
	imageToolEnvInject = "LB_IMAGE_TOOL_INJECT" // always | auto | off

	imageToolDefaultMaxCalls = 2
	// R88 (2026-10-08): капов по умолчанию НЕТ — та же доктрина, что у
	// текстового бэкенда (timeout_policy.go): duration-кап на РАБОТУ запрещён,
	// ждём терминального состояния. Раньше здесь стояли 600 с на генерацию и на
	// загрузку модели: генерация 2048x2048/40 шагов на стенде занимает 22m30s,
	// то есть инструмент обрывал легитимный вызов ровно посередине.
	// Кап взводится явно: LB_IMAGE_TOOL_TIMEOUT_SEC / LB_IMAGE_TOOL_LOAD_TIMEOUT_SEC
	// (или полем toolLoadTimeoutSec в настройках image-ресурсов).
	imageToolDefaultTimeoutSec     = 0
	imageToolMaxCallsLimit         = 8
	imageToolDefaultLoadTimeoutSec = 0
	// imageToolLoadPollInterval — период опроса состояния загрузки.
	// 2 с = «быстрый» интервал поллера метрик (imageMetricsFastInterval), то есть
	// состояние подхватывается с той же частотой, с какой его видит VRAM-гейт.
	imageToolLoadPollInterval = 2 * time.Second
)

// imageToolSurface — метка поверхности в ленте image-запросов (Monitor).
// Отдельная от openai/legacy: тул-генерации полезно отличать от прямых запросов
// клиентов — иначе в мониторе не понять, откуда взялась картинка.
const imageToolSurface = "chat-tool"

// imageToolPath — путь, которым тул-генерация видна в ленте запросов.
const imageToolPath = "/v1/chat/completions→generate_image"

// imageModelLoadPath — ручка воркера, которой поднимается модель (R85).
// Контракт: POST {"name":"<model>"} → 202, прогресс — /load/progress.
const imageModelLoadPath = "/api/image/models/load"

// imageToolAllowLoadEnv — имя флага окружения для автозагрузки. Экспортируется
// через const-переменную пакета, чтобы API-слой показывал в WebUI то же имя.
const imageToolAllowLoadEnv = imageToolEnvAllowLoad

// imageToolConfig — настройки инструмента.
type imageToolConfig struct {
	Enabled  bool
	MaxCalls int
	Timeout  time.Duration
	// BaseURL — внешний адрес балансера для ссылок на картинки. Пусто = берём
	// Host из запроса клиента (http://<Host>). Нужен, когда балансер стоит за
	// TLS-прокси: тогда схему и хост знает только оператор.
	BaseURL string
	// AllowLoad — вызову разрешено поднимать модель (LB_IMAGE_TOOL_ALLOW_LOAD).
	AllowLoad bool
	// LoadTimeout — сколько ждать загрузку модели.
	LoadTimeout time.Duration
	// InjectAuto — режим LB_IMAGE_TOOL_INJECT=auto: объявлять инструмент только
	// тем запросам, где клиент сам прислал непустой tools[]. См. imageToolEnvInject
	// и imageToolShouldInject.
	InjectAuto bool
	// InjectOff — R216 (2026-10-21): LB_IMAGE_TOOL_INJECT=off. Инструмент не
	// объявляется НИКОГДА. Прежний разбор знал только auto, поэтому off тихо
	// означал always (см. imageToolEnvInject).
	InjectOff bool
}

// ImageToolAllowLoadEnv — имя флага окружения автозагрузки (для API/WebUI).
func ImageToolAllowLoadEnv() string { return imageToolAllowLoadEnv }

// ImageToolLoadTimeoutEnv — имя переменной окружения с ожиданием загрузки
// модели (для API/WebUI: оператор должен видеть, что перекрывается конфигом).
func ImageToolLoadTimeoutEnv() string { return imageToolEnvLoadTimeout }

// DefaultImageToolLoadTimeoutSec — дефолт ожидания загрузки модели в секундах.
// Нужен API-слою, чтобы показать действующее значение, когда настройка не задана.
const DefaultImageToolLoadTimeoutSec = imageToolDefaultLoadTimeoutSec

// imageToolSettings — настройки инструмента для КОНКРЕТНОГО прокси.
//
// ПОЧЕМУ МЕТОД, А НЕ ФУНКЦИЯ ОКРУЖЕНИЯ (R85/R86): разрешение на автозагрузку и
// таймаут загрузки должны переключаться из WebUI без рестарта. Значения живут в
// balancing.image (types.ImageResourceSettings.AllowToolLoad / ToolLoadTimeoutSec),
// которое правится через PUT /api/v1/image/resources и читается здесь на каждый
// вызов; окружение остаётся флагом-переключателем для тех стендов, где WebUI не
// используют.
//
// Приоритет: ЯВНО заданное значение в конфиге → флаг/значение окружения → дефолт.
// Так «поле в WebUI» сильнее env, а env сильнее встроенного дефолта.
func (p *Proxy) imageToolSettings() imageToolConfig {
	cfg := imageToolSettingsFromEnv()
	if p != nil && p.config != nil {
		if v := p.config.Balancing.Image.AllowToolLoad; v != nil {
			cfg.AllowLoad = *v
		}
		if v := p.config.Balancing.Image.ToolLoadTimeoutSec; v != nil && *v > 0 {
			cfg.LoadTimeout = time.Duration(*v) * time.Second
		}
	}
	return cfg
}

// imageToolSettingsFromEnv — настройки только из окружения.
func imageToolSettingsFromEnv() imageToolConfig {
	cfg := imageToolConfig{
		Enabled:     true,
		MaxCalls:    imageToolDefaultMaxCalls,
		Timeout:     imageToolDefaultTimeoutSec * time.Second,
		AllowLoad:   true,
		LoadTimeout: imageToolDefaultLoadTimeoutSec * time.Second,
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(imageToolEnvEnabled))) {
	case "0", "false", "no", "off":
		cfg.Enabled = false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(imageToolEnvAllowLoad))) {
	case "0", "false", "no", "off":
		cfg.AllowLoad = false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(imageToolEnvInject))) {
	case "auto":
		cfg.InjectAuto = true
	case "off", "false", "no", "0", "none":
		// R216: «не объявлять вовсе». До этого off молча превращался в always.
		cfg.InjectOff = true
	}
	if v := strings.TrimSpace(os.Getenv(imageToolEnvMaxCalls)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > imageToolMaxCallsLimit {
				n = imageToolMaxCallsLimit
			}
			cfg.MaxCalls = n
		}
	}
	if v := strings.TrimSpace(os.Getenv(imageToolEnvTimeout)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Timeout = time.Duration(n) * time.Second
			// Доктрина: взведённый кап обязан быть виден в логе, иначе на
			// вопрос «почему оборвалось» ответа нет (см. timeout_policy.go).
			if log := logger.Get(); log != nil {
				log.Warnw("image tool generation timeout armed (operator opt-in)",
					"env", imageToolEnvTimeout, "timeout_sec", n,
					"hint", i18n.T("balancer.image_tool.generation_timeout_hint"))
			}
		}
	}
	if v := strings.TrimSpace(os.Getenv(imageToolEnvLoadTimeout)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.LoadTimeout = time.Duration(n) * time.Second
			if log := logger.Get(); log != nil {
				log.Warnw("image tool model-load timeout armed (operator opt-in)",
					"env", imageToolEnvLoadTimeout, "timeout_sec", n,
					"hint", i18n.T("balancer.image_tool.load_timeout_hint"))
			}
		}
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(os.Getenv(imageToolEnvBaseURL)), "/")
	return cfg
}
