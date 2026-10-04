// schema.go — единственное описание инструмента `generate_image` для LLM.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ПАКЕТ. Инструмент нужен в ДВУХ местах:
//
//   - internal/api: документ контракта GET /api/v1/image/contract отдаёт схему
//     инструмента клиенту/агенту, чтобы тот собрал вызов сам;
//   - internal/balancer: инструмент автоматически подмешивается в запросы
//     /v1/chat/completions, а его вызов исполняет сам балансер (tool-loop).
//
// Держать две копии схемы нельзя: разъедутся имена полей (prompt/negative_prompt),
// границы (64…4096, кратно 64) и enum моделей — а это ровно то, по чему модель
// формирует валидный вызов. Но и импортировать друг друга эти пакеты не могут:
// internal/api → internal/balancer (уже есть), обратная зависимость дала бы цикл.
//
// ПОЭТОМУ здесь только НЕЙТРАЛЬНЫЕ типы: Spec собирается из того, что знает
// вызывающая сторона (агрегат возможностей в api, снимки image-бэкендов в
// балансере), а пакет не знает ни про кластер, ни про HTTP.
//
// Экспорт: Build, Defaults, Name, Tool, Parameters, Property, Spec.
package imagetool

import "fmt"

// Name — имя инструмента. Менять нельзя: именно его ждут агенты, оно попало в
// документацию контракта и в примеры.
const Name = "generate_image"

// Границы и дефолты движка — те же значения, что применяет sd.cpp
// (internal/sdbackend/normalize.go: MinImageSide/MaxImageSide/SizeMultiple/
// MinSteps/MaxSteps). Дублируются здесь осознанно: пакет не должен тянуть
// internal/sdbackend (тот тянет супервизор субпроцесса), а расхождение поймает
// тест schema_test.go.
const (
	DefaultMinSide      = 64
	DefaultMaxSide      = 4096
	DefaultSizeMultiple = 64
	DefaultMinSteps     = 1
	DefaultMaxSteps     = 100
)

// Spec — то, что известно о живом image-бэкенде в момент сборки схемы.
//
// Пустые/нулевые поля трактуются как «не сообщено» и заменяются дефолтами: схема
// обязана оставаться валидной даже когда ни один воркер не ответил.
type Spec struct {
	MinSide      int
	MaxSide      int
	SizeMultiple int
	MinSteps     int
	MaxSteps     int
	// Models — имена image-моделей кластера. Пустой список = enum НЕ добавляется:
	// пустой enum ломает валидацию у части tool-раннеров, а выдумывать имена нельзя.
	Models []string
	// Sync — генерацию исполняет сам балансер (модели не нужно ходить по HTTP).
	// В описании инструмента это меняет формулировку: вместо «отправь POST» —
	// «вызови инструмент, картинка вернётся ссылкой».
	Sync bool
}

// Defaults — границы движка без данных от воркеров.
func Defaults() Spec {
	return Spec{
		MinSide:      DefaultMinSide,
		MaxSide:      DefaultMaxSide,
		SizeMultiple: DefaultSizeMultiple,
		MinSteps:     DefaultMinSteps,
		MaxSteps:     DefaultMaxSteps,
	}
}

// Property — одно поле JSON-Schema.
type Property struct {
	Type        string      `json:"type"`
	Description string      `json:"description"`
	Minimum     interface{} `json:"minimum,omitempty"`
	Maximum     interface{} `json:"maximum,omitempty"`
	MultipleOf  interface{} `json:"multipleOf,omitempty"`
	Enum        []string    `json:"enum,omitempty"`
}

// Parameters — объект параметров инструмента (OpenAI function calling).
type Parameters struct {
	Type                 string              `json:"type"`
	Properties           map[string]Property `json:"properties"`
	Required             []string            `json:"required"`
	AdditionalProperties bool                `json:"additionalProperties"`
}

// Tool — схема инструмента в форме OpenAI function calling
// ({name, description, parameters}); её понимают OpenAI SDK, LangChain
// (n8n/LibreChat) и локальные агенты (Cline/Roo).
type Tool struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Parameters  Parameters `json:"parameters"`
}

// Build собирает схему инструмента по Spec.
func Build(spec Spec) Tool {
	minSide := intOr(spec.MinSide, DefaultMinSide)
	maxSide := intOr(spec.MaxSide, DefaultMaxSide)
	multiple := intOr(spec.SizeMultiple, DefaultSizeMultiple)
	minSteps := intOr(spec.MinSteps, DefaultMinSteps)
	maxSteps := intOr(spec.MaxSteps, DefaultMaxSteps)

	props := map[string]Property{
		"prompt": {
			Type:        "string",
			Description: "Текстовое описание желаемого изображения (обязательно). Английский язык обычно даёт лучшее качество.",
		},
		"negative_prompt": {
			Type:        "string",
			Description: "Что НЕ должно попасть на изображение (blurry, extra fingers, watermark...). Если не задать — возьмётся из профиля модели.",
		},
		"width": {
			Type: "integer", Minimum: minSide, Maximum: maxSide, MultipleOf: multiple,
			Description: fmt.Sprintf("Ширина в пикселях, %d…%d, кратно %d. Приоритет над size.", minSide, maxSide, multiple),
		},
		"height": {
			Type: "integer", Minimum: minSide, Maximum: maxSide, MultipleOf: multiple,
			Description: fmt.Sprintf("Высота в пикселях, %d…%d, кратно %d.", minSide, maxSide, multiple),
		},
		"steps": {
			Type:        "integer",
			Minimum:     minSteps,
			Maximum:     maxSteps,
			Description: "Число шагов сэмплинга: больше — детальнее и медленнее. Для turbo/distilled моделей 4–8, для обычных 20–25.",
		},
		"cfg": {
			Type: "number", Minimum: 0,
			Description: "CFG scale (насколько строго следовать промпту). Для turbo/distilled — 1.0, для обычных — 6–8.",
		},
		"seed": {
			Type:        "integer",
			Description: "Зерно генерации. Если не указать (или ≤0) — подставится случайное: повторный вызов с тем же prompt даст ДРУГУЮ картинку. Укажите seed, чтобы воспроизвести результат.",
		},
		"model": {
			Type:        "string",
			Description: "Имя image-модели. Движок его игнорирует (одна модель на процесс), но по имени выбираются дефолты профиля.",
		},
	}
	if len(spec.Models) > 0 {
		p := props["model"]
		p.Enum = append([]string(nil), spec.Models...)
		props["model"] = p
	}

	return Tool{
		Name:        Name,
		Description: buildDescription(spec.Sync),
		Parameters: Parameters{
			Type:                 "object",
			Properties:           props,
			Required:             []string{"prompt"},
			AdditionalProperties: false,
		},
	}
}

// buildDescription — текст для модели. В sync-режиме (инструмент исполняет
// балансер) важно снять с модели заботу о транспорте: она просто вызывает
// инструмент, а ссылка на готовую картинку приходит в результат.
func buildDescription(sync bool) string {
	base := "Сгенерировать изображение по текстовому описанию (stable-diffusion.cpp на image-бэкенде OllamaLegion). "
	if sync {
		return base +
			"ВЫЗЫВАЙ ИНСТРУМЕНТ ТОЛЬКО когда пользователь просит нарисовать/сгенерировать картинку. " +
			"Генерация занимает от секунд до нескольких минут, поэтому один вызов на запрос. " +
			"Результат приходит готовой ссылкой на изображение — вставь markdown-картинку из поля markdown в ответ и кратко опиши, что нарисовано."
	}
	return base +
		"Возвращает изображение в base64 — его нужно декодировать и показать/сохранить на стороне клиента. " +
		"Генерация занимает от секунд до нескольких минут (CPU/слабая GPU), поэтому не вызывайте инструмент без необходимости."
}

// OpenAIFunction — обёртка для поля tools запроса /v1/chat/completions:
// {"type":"function","function":{…}}.
func (t Tool) OpenAIFunction() map[string]interface{} {
	return map[string]interface{}{
		"type":     "function",
		"function": t,
	}
}

func intOr(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}
