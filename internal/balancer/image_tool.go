// image_tool.go — R84 (2026-10-03): инструмент генерации изображений для
// ТЕКСТОВЫХ моделей (cppworker) — объявление и серверное исполнение.
//
// ЗАЧЕМ. Оператор описал сценарий: текстовая модель на cppworker должна уметь
// «нарисовать картинку» по просьбе пользователя/агента, если в кластере поднят
// imageworker. Технически части уже были: cppworker инжектит инструменты клиента
// в system prompt и парсит tool_calls (cmd/cppworker/handlers_openai.go,
// tools_stream_r83.go), балансер умеет детектировать tool_calls (в т.ч. в SSE) и
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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/internal/imagetool"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
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
	// plans/2026-10-06-image-tool-catalog-autoload.md). `off` возвращает прежнее
	// поведение байт-в-байт: ни list_image_models, ни автозагрузки.
	imageToolEnvAllowLoad = "LB_IMAGE_TOOL_ALLOW_LOAD" // on | off (по умолчанию on)
	// imageToolEnvLoadTimeout — сколько ждать загрузку модели перед отказом.
	// Загрузка bundle'а в VRAM занимает секунды-минуты; бесконечное ожидание
	// держало бы и запрос клиента, и слот, поэтому таймаут обязателен, а его
	// истечение — честная ошибка в tool-результате (модель объяснит её
	// пользователю), а не молчаливый обрыв.
	imageToolEnvLoadTimeout = "LB_IMAGE_TOOL_LOAD_TIMEOUT_SEC"

	imageToolDefaultMaxCalls   = 2
	imageToolDefaultTimeoutSec = 600
	imageToolMaxCallsLimit     = 8
	// 600 с — как таймаут генерации: FLUX Q3_K на слабой карте грузится минуты.
	imageToolDefaultLoadTimeoutSec = 600
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
		}
	}
	if v := strings.TrimSpace(os.Getenv(imageToolEnvLoadTimeout)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.LoadTimeout = time.Duration(n) * time.Second
		}
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(os.Getenv(imageToolEnvBaseURL)), "/")
	return cfg
}

// imageToolTarget — бэкенд и модель, которыми балансер исполнит вызов.
type imageToolTarget struct {
	BackendID string
	Model     string
	// Models — все модели кластера (enum в схеме инструмента).
	Models []string
	// Families — семейства моделей кластера (enum параметра family у
	// list_image_models).
	Families []string
	// AllowLoad — вызову разрешено поднимать не загруженную модель.
	AllowLoad bool
	// catalog — снимок каталога, из которого выбрана модель. Нужен, чтобы
	// выбор модели в generateImageForTool не переспрашивал кластер.
	catalog *ImageCatalog
}

// imageToolTargetFor — гейт доступности: nil, если исполнять некому.
//
// ПОЧЕМУ ЧЕРЕЗ ensureFreshFor, А НЕ metricsSnapshot: решение принимается
// per-request, и протухший снимок стоит дорого в обе стороны — объявим
// инструмент по выгруженной модели и получим ошибку уже после вызова. Догрузка
// ленивая и переиспользует один опрос для подряд идущих запросов (staleAfter=5s).
//
// R85 (2026-10-06) — ГЕЙТ ИЗМЕНИЛСЯ. Было: здоровый image_cpp + ЗАГРУЖЕННАЯ
// модель. Стало: здоровый image_cpp + ЕСТЬ ЧТО ГРУЗИТЬ (в снимке есть модели) при
// LB_IMAGE_TOOL_ALLOW_LOAD=on. Причина: требование «модель уже в VRAM» не давало
// текстовой модели поднять нужную модель под запрос пользователя, а без
// каталога она вообще не знала, из чего выбирать.
//
// При ALLOW_LOAD=off правило прежнее (нужна загруженная модель) — это ровно то
// поведение, которое уже описано в документации и проверено тестами.
//
// Тонкая обёртка над imageToolDecision: здесь причина отказа не нужна, но нужна
// там, где решение логируется (см. logImageToolDecision).
func (p *Proxy) imageToolTargetFor(ctx context.Context) *imageToolTarget {
	target, _ := p.imageToolDecision(ctx)
	return target
}

// Причины отказа объявить инструмент (R86, 2026-10-06). Нужны, чтобы ответить
// на вопрос «почему модель не видит инструменты» ОДНОЙ строкой лога, а не
// раскопками исходников: раньше отказ жил на debug-уровне без причины.
const (
	imageToolSkipDisabled     = "tool_disabled"
	imageToolSkipNoBackend    = "no_image_backend"
	imageToolSkipNoModels     = "no_models_to_load"
	imageToolSkipAllowLoadOff = "no_loaded_model_and_allow_load_off"
	imageToolSkipClientNone   = "client_tool_choice_none"
	imageToolSkipClientOwns   = "client_owns_tool"
)

// imageToolDecision — гейт доступности ВМЕСТЕ с причиной отказа.
//
// Возвращает (nil, "") если инструмент объявлять можно; иначе (nil, причина) —
// причина для лога (см. imageToolSkipReason).
func (p *Proxy) imageToolDecision(ctx context.Context) (*imageToolTarget, string) {
	if p == nil {
		return nil, imageToolSkipDisabled
	}
	cfg := p.imageToolSettings()
	if !cfg.Enabled {
		return nil, imageToolSkipDisabled
	}
	if p.imageResources() == nil {
		return nil, imageToolSkipDisabled
	}
	states := p.filterBackendsByType(types.BackendTypeImage)
	if len(states) == 0 {
		logger.Get().Debugw("image tool: не объявляем — нет здоровых image_cpp-бэкендов")
		return nil, imageToolSkipNoBackend
	}

	// Каталог — единственный источник и имён, и состояний: он собран из тех же
	// снимков, что судит VRAM-гейт, поэтому «каталог показывает not_loaded» и
	// «гейт откажет без загрузки» не могут разойтись.
	catalog := p.ImageCatalogFor(ctx)

	var models, families []string
	var target *imageToolTarget
	for _, m := range catalog.Models {
		if m.Name != "" && !containsString(models, m.Name) {
			models = append(models, m.Name)
		}
		if m.Family != "" && !containsString(families, m.Family) {
			families = append(families, m.Family)
		}
	}

	// Приоритет — ЗАГРУЖЕННАЯ модель: генерация по ней не требует ни загрузки,
	// ни ожидания. Иначе (и только при разрешённой автозагрузке) — первая модель
	// каталога: её поднимет executeOneImageToolCall.
	for _, m := range catalog.Models {
		if m.Loaded {
			target = &imageToolTarget{BackendID: m.BackendID, Model: m.Name}
			break
		}
	}
	if target == nil && cfg.AllowLoad {
		if len(catalog.Models) > 0 {
			target = &imageToolTarget{BackendID: catalog.Models[0].BackendID, Model: catalog.Models[0].Name}
		}
	}

	if target == nil {
		if !cfg.AllowLoad {
			// Прежнее поведение: без загруженной модели исполнять нечего, потому
			// что LB_IMAGE_TOOL_ALLOW_LOAD=off запрещает поднимать её из вызова.
			logger.Get().Debugw("image tool: не объявляем — нет загруженной модели (LB_IMAGE_TOOL_ALLOW_LOAD=off)",
				"env", imageToolEnvAllowLoad)
			return nil, imageToolSkipAllowLoadOff
		}
		logger.Get().Debugw("image tool: не объявляем — ни на одном image-бэкенде нет моделей",
			"confident_backends", len(catalog.Backends))
		return nil, imageToolSkipNoModels
	}

	sortStringsInPlace(models)
	sortStringsInPlace(families)
	target.Models = models
	target.Families = families
	target.AllowLoad = cfg.AllowLoad
	target.catalog = catalog
	return target, ""
}

// imageToolOpenAITools — набор инструментов в форме OpenAI tools[].
//
// R85: при AllowLoad добавляется второй инструмент list_image_models (каталог).
// При off возвращается ровно один generate_image — прежняя проводная форма.
func imageToolOpenAITools(t *imageToolTarget) []map[string]interface{} {
	spec := imagetool.Defaults()
	spec.Sync = true
	if t != nil {
		spec.Models = t.Models
		spec.AllowLoad = t.AllowLoad
	}
	tools := imagetool.OpenAITools(spec)
	if t != nil && t.AllowLoad {
		// Семейства — из того же каталога, что и модели: enum параметра family
		// не должен предлагать семейство, которого в кластере нет.
		tools[1] = imagetool.BuildList(imagetool.ListSpec{Families: t.Families}).OpenAIFunction()
	}
	return tools
}

// imageToolOpenAI — схема generate_image в форме OpenAI tools[] (совместимость:
// используется contract-тестами и документом контракта).
func imageToolOpenAI(t *imageToolTarget) map[string]interface{} {
	return imageToolOpenAITools(t)[0]
}

// imageToolNamesHeader — значение заголовка X-Image-Tool: имена объявленных
// инструментов через запятую (в порядке схемы).
func imageToolNamesHeader(tools []map[string]interface{}) string {
	return strings.Join(imageToolNames(tools), ",")
}

// imageToolNames — имена инструментов набора (пустые пропускаются).
func imageToolNames(tools []map[string]interface{}) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if name := imageToolEntryName(tool); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// countClientTools — сколько инструментов прислал клиент (для диагностики).
func countClientTools(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(body, &doc); err != nil {
		return 0
	}
	tools, _ := doc["tools"].([]interface{})
	return len(tools)
}

// sortStringsInPlace — сортировка строк на месте (маленькие срезы: сортировка
// вставками дешевле sort.Strings и не тянет рефлексию).
func sortStringsInPlace(list []string) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j-1] > list[j]; j-- {
			list[j-1], list[j] = list[j], list[j-1]
		}
	}
}

// injectImageTool — добавить инструмент в тело запроса /v1/chat/completions.
//
// НЕ НАВЯЗЫВАЕМСЯ, ЕСЛИ:
//   - tool_choice="none" — клиент явно запретил инструменты в этом запросе;
//   - клиент САМ объявил инструмент с таким именем — тогда исполняет его
//     сторона, и перехватывать вызов нельзя (иначе клиент не получит свой
//     tool_call). Для набора R85 (generate_image + list_image_models) это
//     означает: любой из них, объявленный клиентом, отменяет инъекцию ЦЕЛИКОМ —
//     частичный набор сломал бы ожидания клиента.
//
// Возвращает исходное тело, если ничего не добавлено (байт-в-байт: путь без
// image-бэкенда не должен меняться вообще).
func injectImageTool(body []byte, tools []map[string]interface{}) ([]byte, bool, error) {
	var doc map[string]interface{}
	if err := json.Unmarshal(body, &doc); err != nil {
		return body, false, fmt.Errorf("parse chat body: %w", err)
	}
	if tc, ok := doc["tool_choice"].(string); ok && strings.EqualFold(strings.TrimSpace(tc), "none") {
		return body, false, nil
	}
	existing, _ := doc["tools"].([]interface{})
	if clientOwnsImageTool(existing, tools) {
		return body, false, nil
	}
	for _, tool := range tools {
		existing = append(existing, tool)
	}
	doc["tools"] = existing
	out, err := json.Marshal(doc)
	if err != nil {
		return body, false, fmt.Errorf("marshal chat body with tool: %w", err)
	}
	return out, true, nil
}

// clientOwnsImageTool — объявил ли клиент хотя бы один из наших инструментов.
func clientOwnsImageTool(existing []interface{}, tools []map[string]interface{}) bool {
	if len(existing) == 0 || len(tools) == 0 {
		return false
	}
	ours := make(map[string]bool, len(tools))
	for _, tool := range tools {
		switch fn := tool["function"].(type) {
		case imagetool.Tool:
			ours[fn.Name] = true
		case map[string]interface{}:
			if name, _ := fn["name"].(string); name != "" {
				ours[name] = true
			}
		}
	}
	if len(ours) == 0 {
		return false
	}
	for _, t := range existing {
		if ours[openAIToolName(t)] {
			return true
		}
	}
	return false
}

// openAIToolName — имя функции в элементе tools[] (формат OpenAI и Ollama:
// обе поверхности описывают инструмент как {"type":"function","function":{...}}).
func openAIToolName(t interface{}) string {
	entry, ok := t.(map[string]interface{})
	if !ok {
		return ""
	}
	fn, ok := entry["function"].(map[string]interface{})
	if !ok {
		return ""
	}
	name, _ := fn["name"].(string)
	return name
}

// injectImageToolsOllama — добавить наши инструменты в тело /api/chat.
//
// ЧЕМ ОТЛИЧАЕТСЯ ОТ OpenAI-ПУТИ (сознательно):
//   - инструменты КЛИЕНТА СОХРАНЯЮТСЯ: в Open WebUI у пользователя обычно
//     включены свои инструменты (веб-поиск и т.п.), и «клиент что-то объявил →
//     ничего не добавляем» лишало бы его картинок;
//   - но если клиент объявил инструмент с ИМЕНЕМ ИЗ НАШЕГО НАБОРА
//     (generate_image / list_image_models) — не добавляем НИЧЕГО: исполняет его
//     сторона, а смешанный набор (наш каталог + его генерация) привёл бы к тому,
//     что часть вызовов исполняем мы, часть клиент, и никто не знает, кто;
//   - tool_choice="none" уважается: клиент явно запретил инструменты в этом
//     запросе (docs/AUDIT-client-api-fidelity-2026.md, C7).
//
// Возвращает (тело, имена добавленных инструментов). Пустой список = тело не
// менялось.
func injectImageToolsOllama(body []byte, tools []map[string]interface{}) ([]byte, bool, []string) {
	if len(tools) == 0 {
		return body, false, nil
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(body, &doc); err != nil {
		return body, false, nil
	}
	if tc, ok := doc["tool_choice"].(string); ok && strings.EqualFold(strings.TrimSpace(tc), "none") {
		return body, false, nil
	}

	existing, _ := doc["tools"].([]interface{})
	for _, t := range existing {
		if isImageToolName(openAIToolName(t)) {
			// Клиент объявил инструмент с нашим именем — исполняет он, не мы.
			return body, false, nil
		}
	}

	added := make([]string, 0, len(tools))
	for _, tool := range tools {
		existing = append(existing, tool)
		added = append(added, imageToolEntryName(tool))
	}
	if len(added) == 0 {
		return body, false, nil
	}
	doc["tools"] = existing
	out, err := json.Marshal(doc)
	if err != nil {
		return body, false, nil
	}
	return out, true, added
}

// imageToolEntryName — имя инструмента в элементе tools[] в любой из двух форм
// (собранная схема imagetool.Tool или уже сериализованный map).
func imageToolEntryName(tool map[string]interface{}) string {
	switch fn := tool["function"].(type) {
	case imagetool.Tool:
		return fn.Name
	case map[string]interface{}:
		name, _ := fn["name"].(string)
		return name
	}
	return ""
}

// logImageToolDecision — ЕДИНАЯ диагностическая строка решения об инструменте.
//
// ЗАЧЕМ (живой вопрос оператора, 2026-10-06): «модель не видит инструменты» нельзя
// было объяснить без чтения исходников — отказ объявления жил на debug-уровне, а
// причина «клиент прислал tool_choice=none» вообще нигде не логировалась. Строка
// уровня INFO на каждый tools-запрос отвечает сразу: каким ПУТЁМ пришёл запрос,
// что прислал клиент, что решил балансер и почему.
func logImageToolDecision(path, model string, clientTools int, injected bool, names []string, reason string) {
	log := logger.Get()
	if log == nil {
		return
	}
	fields := []interface{}{
		"path", path,
		"model", model,
		"client_tools", clientTools,
		"injected", injected,
	}
	if len(names) > 0 {
		fields = append(fields, "injected_tools", strings.Join(names, ","))
	}
	if reason != "" {
		fields = append(fields, "reason", reason)
	}
	switch {
	case injected:
		log.Infow("image tool: инструменты объявлены модели", fields...)
	default:
		// Отказ — не ошибка: чаще всего это «нечего грузить» или «клиент запретил».
		log.Infow("image tool: инструменты НЕ объявлены", fields...)
	}
}

// imageToolSkipReason — человеческая причина отказа объявления (для лога).
func imageToolSkipReason(skip string) string {
	switch skip {
	case imageToolSkipDisabled:
		return "LB_IMAGE_TOOL=off — инструмент отключён оператором"
	case imageToolSkipNoBackend:
		return "в кластере нет здорового image_cpp-бэкенда"
	case imageToolSkipNoModels:
		return "на image-бэкенде нет моделей (нечего генерировать)"
	case imageToolSkipAllowLoadOff:
		return "нет загруженной модели, а LB_IMAGE_TOOL_ALLOW_LOAD=off"
	case imageToolSkipClientNone:
		return `клиент прислал tool_choice="none" — инструменты запрещены в этом запросе`
	case imageToolSkipClientOwns:
		return "клиент сам объявил наши инструменты — исполняет его сторона"
	default:
		return skip
	}
}

// imageToolCall — разобранный вызов одного из наших инструментов.
type imageToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// isListCall — вызов каталога (list_image_models), а не генерации.
func (c imageToolCall) isListCall() bool { return c.Name == imagetool.ListName }

// extractImageToolCalls — вызовы наших инструментов в ответе модели (R85:
// generate_image И list_image_models).
//
// cppworker нормализует вывод в message.tool_calls (в т.ч. для Gemma/Hermes/
// Llama/Mistral форматов), но полагаться только на это нельзя: у части моделей
// вызов приходит JSON-ом в content. Поэтому проверяем оба места.
func extractImageToolCalls(message map[string]interface{}) []imageToolCall {
	var calls []imageToolCall
	if raw, ok := message["tool_calls"].([]interface{}); ok {
		for _, item := range raw {
			entry, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			fn, ok := entry["function"].(map[string]interface{})
			if !ok {
				continue
			}
			name, _ := fn["name"].(string)
			if !isImageToolName(name) {
				continue
			}
			args := stringifyToolArguments(fn["arguments"])
			id, _ := entry["id"].(string)
			calls = append(calls, imageToolCall{ID: id, Name: name, Arguments: args})
		}
	}
	if len(calls) == 0 {
		content, _ := message["content"].(string)
		if content != "" {
			if detected, _, found := detectAndExtractToolCallsFromContent(content); found {
				for _, item := range detected {
					entry, ok := item.(map[string]interface{})
					if !ok {
						continue
					}
					fn, ok := entry["function"].(map[string]interface{})
					if !ok {
						continue
					}
					name, _ := fn["name"].(string)
					if !isImageToolName(name) {
						continue
					}
					id, _ := entry["id"].(string)
					calls = append(calls, imageToolCall{
						ID:        id,
						Name:      name,
						Arguments: stringifyToolArguments(fn["arguments"]),
					})
				}
			}
		}
	}
	return calls
}

// isImageToolName — имя принадлежит нашей паре инструментов.
func isImageToolName(name string) bool {
	return name == imagetool.Name || name == imagetool.ListName
}

// imageToolArgs — аргументы вызова в терминах движка.
type imageToolArgs struct {
	Prompt         string
	NegativePrompt string
	Width          int
	Height         int
	Steps          int
	CFG            float64
	Seed           *int64
	Model          string
	// Family — параметр инструмента list_image_models (фильтр каталога по
	// семейству). У generate_image его нет, но разбор общий — поле просто пустое.
	Family string
	// List — вызов инструмента каталога (list_image_models): prompt не нужен,
	// генерации не будет. Введено, чтобы у каталога и генерации был один разбор
	// аргументов (одно место правды про имена полей JSON).
	List bool
}

// parseImageToolArgs разбирает JSON-аргументы вызова.
//
// list=true — аргументы инструмента каталога: prompt не требуется (он там не
// нужен вовсе), а семейство читается из того же разбора.
func parseImageToolArgs(raw string) (imageToolArgs, error) {
	return parseImageToolArgsFor(raw, false)
}

// parseImageToolArgsFor — общий разбор аргументов обоих инструментов.
//
// УСТОЙЧИВОСТЬ К ПУСТЫМ АРГУМЕНТАМ (живой дефект, найденный на стенде R85):
// модель вызывает инструмент без параметров (list_image_models) и присылает
// arguments как ПУСТУЮ СТРОКУ ("" или пробелы) либо как нестроковый JSON
// (null, {}). Первая версия возвращала «arguments не JSON: cannot unmarshal
// string into Go value of type map[string]interface {}» — то есть инструмент,
// который специально сделан без обязательных параметров, отказывал на самом
// естественном вызове. Пустое значение трактуем как «параметров нет».
func parseImageToolArgsFor(raw string, list bool) (imageToolArgs, error) {
	doc, err := decodeToolArguments(raw)
	if err != nil {
		return imageToolArgs{}, err
	}
	args := imageToolArgs{
		List:           list,
		Prompt:         stringField(doc, "prompt"),
		NegativePrompt: firstNonEmpty(stringField(doc, "negative_prompt"), stringField(doc, "negativePrompt")),
		Width:          intField(doc, "width"),
		Height:         intField(doc, "height"),
		Steps:          intField(doc, "steps"),
		CFG:            floatField(doc, "cfg"),
		Model:          stringField(doc, "model"),
		Family:         stringField(doc, "family"),
	}
	if v, ok := doc["seed"]; ok {
		switch t := v.(type) {
		case float64:
			s := int64(t)
			args.Seed = &s
		case string:
			if n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
				args.Seed = &n
			}
		}
	}
	if list {
		return args, nil
	}
	if strings.TrimSpace(args.Prompt) == "" {
		return args, errors.New("в аргументах нет prompt")
	}
	return args, nil
}

// decodeToolArguments — аргументы вызова как объект.
//
// Модели присылают их в четырёх видах: объектом JSON, JSON-СТРОКОЙ внутри
// строки (как того требует OpenAI), пустой строкой (вызов без параметров),
// null. Все четыре обязаны разбираться: иначе «инструмент без параметров»
// становится невызываемым, а это ровно тот случай, ради которого он делался.
func decodeToolArguments(raw string) (map[string]interface{}, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return map[string]interface{}{}, nil
	}
	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &doc); err == nil {
		if doc == nil {
			return map[string]interface{}{}, nil
		}
		return doc, nil
	}
	// Аргументы пришли JSON-СТРОКОЙ с JSON внутри (каноничная форма OpenAI).
	var inner string
	if err := json.Unmarshal([]byte(trimmed), &inner); err == nil {
		inner = strings.TrimSpace(inner)
		if inner == "" || inner == "null" {
			return map[string]interface{}{}, nil
		}
		var nested map[string]interface{}
		if err := json.Unmarshal([]byte(inner), &nested); err == nil {
			if nested == nil {
				return map[string]interface{}{}, nil
			}
			return nested, nil
		}
	}
	return nil, fmt.Errorf("arguments не JSON-объект: %s", shortForLog(trimmed))
}

// shortForLog — обрезать аргументы для текста ошибки (они уходят модели).
func shortForLog(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

func stringField(doc map[string]interface{}, key string) string {
	v, _ := doc[key].(string)
	return strings.TrimSpace(v)
}

func intField(doc map[string]interface{}, key string) int {
	switch t := doc[key].(type) {
	case float64:
		return int(t)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return n
		}
	}
	return 0
}

func floatField(doc map[string]interface{}, key string) float64 {
	switch t := doc[key].(type) {
	case float64:
		return t
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return f
		}
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func containsString(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// imageToolResolved — какая модель и на каком бэкенде будет исполнять вызов.
type imageToolResolved struct {
	BackendID string
	Model     string
	// Loaded — модель уже в VRAM (загрузка не нужна).
	Loaded bool
	State  string
	// NotFound — запрошенного имени нет в каталоге воркера. Нужно, чтобы вместо
	// непонятного 404 от движка отдать модели список доступных имён.
	NotFound bool
}

// resolveImageToolTarget — выбрать бэкенд и модель под конкретный вызов.
//
// ЗАЧЕМ ПЕРЕСМАТРИВАТЬ, А НЕ БРАТЬ target. По умолчанию используется модель из
// гейта (загруженная либо первая доступная). Но модель могла назвать ДРУГУЮ
// модель в аргументе model: тогда её надо найти в каталоге — и по имени, и по
// состоянию, и на живом бэкенде. Без этого «model=flux-schnell-q3-k» при
// загруженной sd15 означал бы «движок проигнорирует параметр и нарисует не то».
//
// Фолбэк при неизвестном имени: остаёмся на модели из гейта и НЕ отказываем —
// каталог мог не догрузиться, а генерация уже возможна. Имя при этом уходит в
// payload: воркер сам скажет, знает ли он такую модель (и код загрузки ниже
// вернёт честную ошибку).
func (p *Proxy) resolveImageToolTarget(ctx context.Context, target *imageToolTarget, requested string) imageToolResolved {
	res := imageToolResolved{BackendID: target.BackendID, Model: target.Model}
	if target.catalog != nil {
		for i := range target.catalog.Models {
			if m := &target.catalog.Models[i]; m.Name == res.Model {
				res.Loaded, res.State = m.Loaded, m.State
				break
			}
		}
	}
	requested = strings.TrimSpace(requested)
	if requested == "" || requested == res.Model {
		return res
	}

	catalog := p.ImageCatalogFor(ctx)
	for i := range catalog.Models {
		m := &catalog.Models[i]
		if m.Name != requested {
			continue
		}
		return imageToolResolved{
			BackendID: m.BackendID,
			Model:     m.Name,
			Loaded:    m.Loaded,
			State:     m.State,
		}
	}
	// Модель не найдена в каталоге: пробуем снимок напрямую (каталог мог быть
	// отдан из кэша до появления модели). Если и там нет — оставляем выбор гейта,
	// но с запрошенным ИМЕНЕМ, чтобы ошибка воркера была про конкретную модель.
	if res2 := p.imageResources(); res2 != nil {
		if snap := res2.ensureFreshFor(ctx, target.BackendID); snap != nil && snap.contractOK {
			for i := range snap.models {
				if snap.models[i].Name == requested {
					return imageToolResolved{
						BackendID: target.BackendID,
						Model:     requested,
						Loaded:    snap.models[i].State == imageStateLoaded,
						State:     snap.models[i].State,
					}
				}
			}
		}
	}
	logger.Get().Warnw("image tool: запрошенной модели нет в каталоге — отдаём имя воркеру как есть",
		"requested", requested, "known", len(catalog.Models))
	return imageToolResolved{BackendID: target.BackendID, Model: requested, State: imageStateNotLoaded, NotFound: true}
}

// imageToolUnknownModelError — понятная ошибка «такой модели нет».
//
// ЗАЧЕМ СВОЙ ТЕКСТ: ошибка воркера «image model not found: stable-diffusion:1.5»
// ничего не говорит модели о том, ЧТО ЖЕ доступно, и следующий её шаг — снова
// угадывать имя (живой случай 2026-10-06). Перечисляем доступные имена прямо в
// tool-сообщении: модель может исправиться на следующем turn.
func (p *Proxy) imageToolUnknownModelError(ctx context.Context, requested string) string {
	names := imageCatalogNames(p.ImageCatalogFor(ctx))
	if len(names) == 0 {
		return fmt.Sprintf("модели %q нет на image-воркере, а загруженных моделей нет вовсе: "+
			"вызови list_image_models и выбери из доступного, либо попроси оператора загрузить модель", requested)
	}
	return fmt.Sprintf("модели %q нет на image-воркере. Доступные модели (имя использовать ровно как есть): %s. "+
		"Вызови list_image_models, чтобы увидеть их описания, и повтори генерацию с одним из этих имён.",
		requested, strings.Join(names, ", "))
}

// ensureImageModelLoaded — поднять модель в VRAM и дождаться готовности.
//
// ПОЧЕМУ СИНХРОННО С ПОЛЛИНГОМ, А НЕ АСИНХРОННО. Инструмент обязан вернуть
// модели результат в том же вызове (tool-сообщение) — асинхронная загрузка
// означала бы «загружаю, вернись позже», чего tool-протокол не умеет. Поэтому
// POST load → опрос /api/image/models до state=loaded, с честным таймаутом.
//
// ВОЗВРАЩАЕТСЯ ВРЕМЯ ЗАГРУЗКИ. Оно уходит в tool-результат (loadSeconds): без
// него модель не может объяснить пользователю, почему ответ шёл минуту.
func (p *Proxy) ensureImageModelLoaded(ctx context.Context, backendID, model string) (time.Duration, error) {
	started := time.Now()
	cfg := p.imageToolSettings()

	// ВАЖНО про имя поля: контракт воркера — {"name": "<model>"}
	// (cmd/sdworker/handlers_model.go: loadRequest.Name), а НЕ "model". Ошибка
	// здесь выглядела бы как «модель не поднимается» при живом воркере.
	payload, err := json.Marshal(map[string]string{"name": model})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, imageModelLoadPath, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.proxyRequestToBackend(req, backendID, cfg.LoadTimeout)
	if err != nil {
		return 0, fmt.Errorf("не удалось отправить запрос на загрузку модели %q: %w", model, err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, imageToolResponseLimitBytes))
	_ = resp.Body.Close()
	if readErr != nil {
		return 0, fmt.Errorf("чтение ответа на загрузку модели %q: %w", model, readErr)
	}
	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("воркер отказал в загрузке модели %q (HTTP %d): %s",
			model, resp.StatusCode, upstreamErrorMessage(raw, resp.StatusCode))
	}

	// Ждём готовности. Опрос идёт по снимкам (ensureFreshFor сам делает опрос
	// при протухании), а НЕ по SSE-прогрессу: состояние loaded/error — это
	// то, чем живёт VRAM-гейт, и видеть его надо там же, где принимается решение
	// о генерации. Прогресс-поток (load/progress) остаётся для WebUI.
	deadline := started.Add(cfg.LoadTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return time.Since(started), fmt.Errorf("загрузка модели %q прервана: %w", model, err)
		}
		if time.Now().After(deadline) {
			return time.Since(started), fmt.Errorf(
				"модель %q не поднялась за %s (state=%s): см. логи воркера и настройку %s",
				model, cfg.LoadTimeout, imageStateLoading, imageToolEnvLoadTimeout)
		}

		snap := p.imageResources().refresh(ctx, backendID)
		if snap != nil {
			elapsed := time.Since(started)
			if m := snap.entryByName(model); m != nil {
				switch m.State {
				case imageStateLoaded:
					return elapsed, nil
				case imageStateError:
					return elapsed, fmt.Errorf("модель %q не поднялась: %s", model,
						firstNonEmptyStr(m.Error, "воркер сообщил state=error"))
				}
			} else if snap.loaded() != nil && snap.loaded().Name != model {
				// Воркер поднял другую модель (одна модель на процесс): подменять
				// её молча нельзя — пользователь просил конкретную.
				return elapsed, fmt.Errorf(
					"воркер поднял модель %q вместо запрошенной %q (одна модель на процесс; выгрузите текущую)",
					snap.loaded().Name, model)
			}
		}

		select {
		case <-ctx.Done():
			return time.Since(started), fmt.Errorf("загрузка модели %q прервана: %w", model, ctx.Err())
		case <-time.After(imageToolLoadPollInterval):
		}
	}
}

// entryByName — запись модели по имени (nil, если воркер её не сообщает).
func (s *imageBackendMetrics) entryByName(name string) *imageModelEntry {
	if s == nil {
		return nil
	}
	for i := range s.models {
		if s.models[i].Name == name {
			return &s.models[i]
		}
	}
	return nil
}

// generateImageForTool — сгенерировать картинку на выбранном image-бэкенде.
//
// Идём НАТИВНЫМ путём воркера (/api/image/generate, sync=true,
// responseFormat=url): он возвращает готовую ссылку /images/<file>, а сам файл
// воркер раздаёт через FileServer. Картинку в base64 в диалог не тащим — она
// раздула бы и ответ, и контекст модели.
//
// Гейт VRAM и лента запросов — те же, что у обычной генерации
// (imageResources.beforeGeneration + imageRequestStore): тул-генерации обязаны
// быть видны в мониторе и обязаны уважать занятость GPU.
//
// R85: если выбранная модель ещё не в VRAM, она поднимается ЗДЕСЬ (до гейта и до
// генерации), потому что воркер отвечает 409 model_not_loaded и сам не грузит.
// Гейт берётся ПОСЛЕ загрузки: до неё свободная VRAM считается без веса
// поднимаемой модели, и «влезает/не влезает» было бы посчитано неверно.
func (p *Proxy) generateImageForTool(ctx context.Context, target *imageToolTarget, args imageToolArgs) (map[string]interface{}, error) {
	cfg := p.imageToolSettings()
	resolved := p.resolveImageToolTarget(ctx, target, args.Model)
	model := resolved.Model

	var loadSeconds float64
	if !resolved.Loaded && model != "" {
		if !cfg.AllowLoad {
			return nil, fmt.Errorf(
				"модель %q не загружена, а автозагрузка выключена (%s=off): загрузите модель заранее",
				model, imageToolEnvAllowLoad)
		}
		if resolved.NotFound {
			// Имени нет в каталоге: не гоняем воркер за гарантированным 404, а
			// сразу отдаём модели список доступных — чтобы она исправилась.
			return nil, errors.New(p.imageToolUnknownModelError(ctx, model))
		}
		elapsed, err := p.ensureImageModelLoaded(ctx, resolved.BackendID, model)
		loadSeconds = elapsed.Seconds()
		if err != nil {
			return nil, err
		}
	}

	payload := map[string]interface{}{
		"prompt":         args.Prompt,
		"sync":           true,
		"responseFormat": "url",
	}
	if model != "" {
		payload["model"] = model
	}
	if args.NegativePrompt != "" {
		payload["negativePrompt"] = args.NegativePrompt
	}
	if args.Width > 0 {
		payload["width"] = args.Width
	}
	if args.Height > 0 {
		payload["height"] = args.Height
	}
	if args.Steps > 0 {
		payload["steps"] = args.Steps
	}
	if args.CFG > 0 {
		payload["cfg"] = args.CFG
	}
	if args.Seed != nil {
		payload["seed"] = *args.Seed
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/api/image/generate", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	res := p.imageResources()
	gate := res.beforeGeneration(ctx, resolved.BackendID)
	if !gate.Allowed {
		return nil, fmt.Errorf("генерация недоступна: %s", gate.Message)
	}
	defer gate.Release()

	brief := types.ImageRequestBrief{
		BackendID: resolved.BackendID,
		Surface:   imageToolSurface,
		Path:      imageToolPath,
		Model:     model,
		Prompt:    args.Prompt,
		Width:     args.Width,
		Height:    args.Height,
		Steps:     args.Steps,
	}
	handle := res.imageRequests().begin(resolved.BackendID, brief)

	started := time.Now()
	resp, err := p.proxyRequestToBackend(req, resolved.BackendID, cfg.Timeout)
	if err != nil {
		handle.finish(types.ImageRequestStatusFailed, http.StatusBadGateway, "image_backend_error", err.Error(), 0)
		return nil, fmt.Errorf("image backend request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, imageToolResponseLimitBytes))
	if err != nil {
		handle.finish(types.ImageRequestStatusFailed, resp.StatusCode, "image_backend_error", err.Error(), 0)
		return nil, fmt.Errorf("read image backend response: %w", err)
	}
	if resp.StatusCode >= 400 {
		msg := upstreamErrorMessage(raw, resp.StatusCode)
		handle.finish(types.ImageRequestStatusFailed, resp.StatusCode, "image_backend_error", msg, 0)
		return nil, fmt.Errorf("движок отказал (HTTP %d): %s", resp.StatusCode, msg)
	}

	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		handle.finish(types.ImageRequestStatusFailed, resp.StatusCode, "image_backend_bad_response", err.Error(), 0)
		return nil, fmt.Errorf("image backend returned invalid JSON: %w", err)
	}
	url := firstImageURL(doc)
	if url == "" {
		handle.finish(types.ImageRequestStatusFailed, resp.StatusCode, "image_backend_no_image", "no image in response", 0)
		return nil, errors.New("движок не вернул изображение")
	}

	handle.finish(types.ImageRequestStatusOK, resp.StatusCode, "", "", 1)
	out := map[string]interface{}{
		"status":     "ok",
		"url":        url,
		"markdown":   "![" + shortPrompt(args.Prompt) + "](" + url + ")",
		"model":      model,
		"seed":       doc["seed"],
		"width":      doc["width"],
		"height":     doc["height"],
		"steps":      doc["steps"],
		"seconds":    secondsOf(doc["duration_ms"], time.Since(started)),
		"output":     doc["output_format"],
		"generated":  true,
		"promptUsed": args.Prompt,
		// modelState — где модель оказалась к моменту генерации: модель обязана
		// понимать, что картинка сделана только что поднятой моделью, а не той,
		// что была в VRAM раньше.
		"modelState": imageStateLoaded,
		"backendId":  resolved.BackendID,
	}
	// loadSeconds присутствует ТОЛЬКО когда была загрузка: ноль в поле читался
	// бы как «загрузили мгновенно» и сбивал бы модель с толку.
	if loadSeconds > 0 {
		out["loadSeconds"] = loadSeconds
		out["loadedNow"] = true
	}
	return out, nil
}

// imageToolResponseLimitBytes — предел чтения ответа воркера: в url-режиме тело
// маленькое (метаданные + ссылка), но лимит нужен, чтобы битый/чужой сервис на
// порту не вылил в память гигабайты.
const imageToolResponseLimitBytes = 1 << 20

// firstImageURL — ссылка на первую картинку в ответе генерации.
func firstImageURL(doc map[string]interface{}) string {
	data, ok := doc["data"].([]interface{})
	if !ok || len(data) == 0 {
		return ""
	}
	item, ok := data[0].(map[string]interface{})
	if !ok {
		return ""
	}
	url, _ := item["url"].(string)
	return strings.TrimSpace(url)
}

// imageToolCatalogResult — результат list_image_models для tool-сообщения.
//
// ФОРМА ВЫБРАНА ПОД ЧТЕНИЕ МОДЕЛЬЮ: строка summary (одна строка на модель) —
// самое дешёвое по токенам представление, а полный JSON уходит в models[] для
// тех клиентов/агентов, которые захотят разобрать его программно. Никакой
// генерации и обращения к GPU здесь нет — только чтение уже собранного каталога.
func (p *Proxy) imageToolCatalogResult(ctx context.Context, family string) map[string]interface{} {
	family = strings.ToLower(strings.TrimSpace(family))

	catalog := p.ImageCatalogFor(ctx)
	models := make([]ImageCatalogModel, 0, len(catalog.Models))
	var sb strings.Builder
	for _, m := range catalog.Models {
		if family != "" && !strings.EqualFold(m.Family, family) {
			continue
		}
		models = append(models, m)
		sb.WriteString(imageCatalogModelLine(m))
		sb.WriteByte('\n')
	}

	out := map[string]interface{}{
		"status": "ok",
		"count":  len(models),
		"models": models,
		"limits": catalog.Limits,
		// loadedModel — что уже в VRAM: если оно подходит, генерация начнётся
		// сразу, без ожидания загрузки.
		"hasLoadedModel": catalog.HasLoadedModel,
	}
	if family != "" {
		out["family"] = family
	}
	if len(models) == 0 {
		// ФИЛЬТР НЕ ДАЛ НИЧЕГО (живой случай 2026-10-06): модель вызвала каталог с
		// family="stable-diffusion", получила пустой ответ и дальше придумала имя
		// модели из своих знаний ("stable-diffusion:1.5"), которого на воркере нет.
		// Поэтому в пустом ответе ОБЯЗАТЕЛЬНО перечисляем доступные имена и
		// говорим, что фильтр нужно снять.
		available := imageCatalogNames(catalog)
		if len(available) > 0 {
			out["status"] = "empty"
			out["availableModels"] = available
			out["summary"] = fmt.Sprintf(
				"По фильтру family=%q моделей нет. ДОСТУПНЫЕ модели (используй имя ровно как есть, без выдумывания): %s. Вызови list_image_models без параметра family, чтобы увидеть их описания и требования к VRAM.",
				family, strings.Join(available, ", "))
		} else {
			out["summary"] = "В каталоге нет моделей ни на одном image-бэкенде. Загрузите модель на image-воркер (WebUI → Image-модели → HuggingFace) или через POST /api/v1/image/backends/{id}/models/load."
		}
	} else {
		out["summary"] = fmt.Sprintf("Доступные image-модели (%d)%s:\n%s",
			len(models), familySuffix(family), strings.TrimRight(sb.String(), "\n"))
	}
	if loaded := imageCatalogLoadedLine(catalog); loaded != "" {
		out["loadedModel"] = loaded
	}
	return out
}

// imageCatalogModelLine — одна строка каталога для модели.
func imageCatalogModelLine(m ImageCatalogModel) string {
	parts := []string{"- " + m.Name}
	if m.Family != "" {
		parts = append(parts, "семейство "+m.Family)
	}
	state := m.State
	if state == "" {
		state = "unknown"
	}
	if m.Loaded {
		state += " (уже в VRAM)"
	}
	parts = append(parts, "состояние: "+state)
	if m.VramEstimateMB > 0 {
		parts = append(parts, fmt.Sprintf("~%d MB VRAM", m.VramEstimateMB))
	} else {
		parts = append(parts, "VRAM: неизвестно")
	}
	parts = append(parts, fmt.Sprintf("дефолт: %dx%d, %d шагов, cfg %g",
		m.Defaults.Width, m.Defaults.Height, m.Defaults.Steps, m.Defaults.CFGScale))
	if m.Strengths != "" {
		parts = append(parts, m.Strengths)
	}
	return strings.Join(parts, " | ")
}

// imageCatalogLoadedLine — что сейчас загружено (пусто, если ничего).
func imageCatalogLoadedLine(catalog *ImageCatalog) string {
	if catalog == nil {
		return ""
	}
	for _, m := range catalog.Models {
		if m.Loaded {
			return m.Name
		}
	}
	return ""
}

// imageCatalogNames — имена моделей каталога (для подсказки в tool-результате).
func imageCatalogNames(catalog *ImageCatalog) []string {
	if catalog == nil {
		return nil
	}
	names := make([]string, 0, len(catalog.Models))
	for _, m := range catalog.Models {
		if m.Name != "" {
			names = append(names, m.Name)
		}
	}
	return names
}

// familySuffix — « семейства X» для текста результата (пусто, если фильтра нет).
func familySuffix(family string) string {
	if family == "" {
		return ""
	}
	return " семейства " + family
}

// rawStringField — строковое поле из сырого JSON-аргумента (для аргументов,
// которые не разбирает imageToolArgs: например family у list_image_models).
func rawStringField(raw, key string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return ""
	}
	v, _ := doc[key].(string)
	return strings.TrimSpace(v)
}

// upstreamErrorMessage — короткий текст ошибки из ответа воркера.
func upstreamErrorMessage(raw []byte, status int) string {
	var doc map[string]interface{}
	if json.Unmarshal(raw, &doc) == nil {
		if s, _ := doc["error"].(string); s != "" {
			return s
		}
		if e, ok := doc["error"].(map[string]interface{}); ok {
			if s, _ := e["message"].(string); s != "" {
				return s
			}
		}
		if s, _ := doc["message"].(string); s != "" {
			return s
		}
	}
	text := strings.TrimSpace(string(raw))
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	if text == "" {
		text = http.StatusText(status)
	}
	return text
}

// secondsOf — длительность в секундах: из duration_ms воркера, иначе из замера.
func secondsOf(v interface{}, fallback time.Duration) float64 {
	if ms, ok := v.(float64); ok && ms > 0 {
		return ms / 1000
	}
	return fallback.Seconds()
}

// shortPrompt — подпись для alt-текста markdown (без переводов строк).
func shortPrompt(prompt string) string {
	s := strings.Join(strings.Fields(prompt), " ")
	if len(s) > 60 {
		s = s[:60] + "…"
	}
	if s == "" {
		return "generated image"
	}
	return s
}

// imageToolPublicURL — ссылка на картинку, которую отдадим модели и клиенту.
//
// Почему публичный маршрут балансера, а не адрес воркера: воркер живёт на
// внутреннем порту и картинку наружу не отдаёт; маршрут /v1/images/files/{name}
// проксирует её через балансер и, как остальные клиентские поверхности, не
// требует токена (угадать имя файла нельзя — оно случайное).
//
// ВАЖНО: воркер может вернуть как относительный путь (`/images/x.png`), так и
// АБСОЛЮТНЫЙ адрес (`http://localhost:18093/images/x.png` — когда у него задан
// SDWORKER_BASE_URL). Первая версия просто отрезала префикс «/images/», и на
// живом стенде получилась ссылка
// `http://балансер/v1/images/files/http://localhost:18093/images/x.png` —
// поэтому имя файла достаём по последнему «/images/», а не по началу строки.
func imageToolPublicURL(r *http.Request, url string) string {
	name := imageFileNameFromURL(url)
	if name == "" {
		// Разобрать не удалось — лучше отдать как есть, чем склеить мусор.
		return url
	}
	// BaseURL живёт ТОЛЬКО в окружении (это адрес конкретного стенда, а не
	// балансировочная настройка), поэтому берём env-часть настроек — из конфига
	// сюда попадать нечему.
	base := imageToolSettingsFromEnv().BaseURL
	if base == "" {
		host := ""
		if r != nil {
			host = r.Host
		}
		if host == "" {
			host = "localhost"
		}
		base = "http://" + host
	}
	return base + "/v1/images/files/" + name
}

// imageFileNameFromURL — имя файла из ссылки воркера: последний сегмент после
// «/images/», без query/fragment. Возвращает "" , если имя небезопасно (слэши,
// «..») — тогда подставлять его в публичный маршрут нельзя.
func imageFileNameFromURL(url string) string {
	raw := strings.TrimSpace(url)
	if raw == "" {
		return ""
	}
	if idx := strings.LastIndex(raw, "/images/"); idx >= 0 {
		raw = raw[idx+len("/images/"):]
	}
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	raw = strings.Trim(raw, "/")
	if raw == "" || strings.ContainsAny(raw, "/\\") || strings.Contains(raw, "..") {
		return ""
	}
	return raw
}
