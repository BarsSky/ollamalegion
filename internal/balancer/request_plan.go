package balancer

// request_plan.go — этап 1 рефакторинга: одно решение о запросе.
//
// ЗАЧЕМ. До этого решения о запросе принимались в разных местах и в разной
// форме: стрим разбирался в stream_flag.go (и 14 раз перепроверялся через
// isStreamingFromBody), модель — в parseRequestBody/requestModelHint, окно —
// `ExtractNumCtxFromBody` ПОМИМО parsedRequest.NumCtx (17 прямых вызовов), а
// лимит вывода не решался вообще: `max_tokens`/`num_predict` уходили как есть и
// читались потом в трёх местах с разной семантикой (preflight, авто-продолжение,
// кламп воркера). Инвентаризация — docs/architecture/request-plan-r493.md.
//
// ЧТО ЭТОТ ШАГ ДЕЛАЕТ И ЧЕГО НЕ ДЕЛАЕТ. Добавляет ЕДИНУЮ структуру плана и
// сборку в одной точке (parseRequestBody), где тело уже разобрано, и кладёт её в
// контекст. Поведение не меняется: ни один потребитель ещё не переключён —
// поэтому шаг безопасен и проверяется тестами, а не живым прогоном. Следующие
// шаги миграции (§4 инвентаризации): ResolveNumCtx → preflight → handler-пути →
// generic-путь последним.

import (
	"context"
	"encoding/json"
	"net/http"

	"ollama-loadbalancer/pkg/logger"
)

// requestPlanR493 — одно решение о запросе.
type requestPlanR493 struct {
	// Surface — какая поверхность приняла запрос (legacy 18080 / openai 18079).
	// Передаётся ТОЛЬКО явным аргументом сборки: иначе self-proxy получил бы
	// дефолт legacy и решение «стримить ли по умолчанию» разъехалось бы.
	Surface requestSurface

	// Stream — решение «клиент ждёт поток». Это НЕ транспорт: на /v1/*
	// транспорт выбирается по ответу upstream (stream_openai_sink.go), а
	// клиентское поле влияет только на авто-стрим R258.
	Stream bool
	// StreamSource — откуда решение: "client" (поле stream в теле) или
	// "surface-default" (поле не задано, взят дефолт поверхности).
	StreamSource string

	// Model — имя модели из тела (пусто, если клиент не указал).
	Model string

	// NCtx — окно, запрошенное клиентом (0 = не задавал). Второе число тот же
	// клиент видит как «окно на слот» — см. docs/architecture/context-numbers.ru.md.
	NCtx int

	// OutputLimit — верхняя граница вывода от клиента (0 = не ограничивал).
	OutputLimit int
	// OutputLimitSource — имя поля, которым клиент ограничил: "options.num_predict",
	// "max_tokens", "max_completion_tokens" или "" (не ограничивал).
	OutputLimitSource string
}

const (
	streamSourceClientR493  = "client"
	streamSourceDefaultR493 = "surface-default"
)

// buildRequestPlanR493 — единственная сборка плана. Чистая функция: никаких
// глобальных состояний и чтения тела — всё уже разобрано вызывающим.
func buildRequestPlanR493(surface requestSurface, model string, stream bool, streamSource string, nctx, outputLimit int, outputLimitSource string) *requestPlanR493 {
	if streamSource == "" {
		streamSource = streamSourceDefaultR493
	}
	if outputLimit < 0 {
		// Отрицательный num_predict (Ollama) — «без ограничения», это НЕ лимит.
		outputLimit = 0
		outputLimitSource = ""
	}
	return &requestPlanR493{
		Surface:           surface,
		Stream:            stream,
		StreamSource:      streamSource,
		Model:             model,
		NCtx:              nctx,
		OutputLimit:       outputLimit,
		OutputLimitSource: outputLimitSource,
	}
}

type requestPlanCtxKey struct{}

// withRequestPlanR493 кладёт план в контекст запроса.
func withRequestPlanR493(r *http.Request, plan *requestPlanR493) *http.Request {
	if r == nil || plan == nil {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), requestPlanCtxKey{}, plan))
}

// requestPlanOfR493 возвращает план запроса (nil, если он ещё не собран —
// например, во внутренних вызовах и старых тестах).
func requestPlanOfR493(r *http.Request) *requestPlanR493 {
	if r == nil {
		return nil
	}
	if v, ok := r.Context().Value(requestPlanCtxKey{}).(*requestPlanR493); ok {
		return v
	}
	// Мост на время миграции: план уже лежит в parsedRequest (собирается там же,
	// где тело разобрано один раз). Явный withRequestPlanR493 появится на шаге,
	// когда план начнут читать handler-пути.
	if parsed := parsedRequestOf(r); parsed != nil && parsed.Plan != nil {
		return parsed.Plan
	}
	return nil
}

// outputLimitFromBodyR493 — верхняя граница вывода из тела и ИМЯ поля, которым
// она задана (для логов и для отличия «клиент ограничил» от «дефолт воркера»).
//
// Правило и порядок полей — те же, что у ClientOutputCapped (auto_continue.go),
// но здесь нужны и значение, и имя. Один источник правды появится на следующем
// шаге миграции; пока функция дублирует разбор осознанно, чтобы не менять
// поведение существующего вызова.
//
// max_output_tokens (Responses-поверхность) переводится в max_tokens только
// адаптером responses_api.go:211, поэтому здесь он не учитывается: на этой
// поверхности тело уже приведено к OpenAI-форме к моменту сборки плана.
func outputLimitFromBodyR493(body []byte) (int, string) {
	if len(body) == 0 {
		return 0, ""
	}
	var doc struct {
		Options struct {
			NumPredict *int `json:"num_predict"`
		} `json:"options"`
		MaxTokens           *int `json:"max_tokens"`
		MaxCompletionTokens *int `json:"max_completion_tokens"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return 0, ""
	}
	if doc.Options.NumPredict != nil && *doc.Options.NumPredict > 0 {
		return *doc.Options.NumPredict, "options.num_predict"
	}
	if doc.MaxTokens != nil && *doc.MaxTokens > 0 {
		return *doc.MaxTokens, "max_tokens"
	}
	if doc.MaxCompletionTokens != nil && *doc.MaxCompletionTokens > 0 {
		return *doc.MaxCompletionTokens, "max_completion_tokens"
	}
	return 0, ""
}

// streamDecisionR493 — решение «клиент ждёт поток» из ОДНОГО места.
//
// Порядок: план запроса (этап 1) → уже разобранное тело (parsedRequest.Stream) →
// старый разбор через isStreamingFromBody. Два последних шага — только для
// миграции и внутренних вызовов, где план ещё не собран (тесты, self-proxy).
// Новые потребители обязаны получать решение отсюда, а не звать
// isStreamingFromBody напрямую: иначе копия №15 (см.
// request_plan_steward_r493_test.go).
func streamDecisionR493(r *http.Request, path string, body []byte) bool {
	if plan := requestPlanOfR493(r); plan != nil {
		return plan.Stream
	}
	if parsed := parsedRequestOf(r); parsed != nil {
		return parsed.Stream
	}
	// Fallback на время миграции: внутренние вызовы без разобранного тела.
	return isStreamingFromBody(path, body)
}

// logRequestPlanR493 — плановая строка в лог на debug-уровне: план есть у
// каждого запроса, поэтому info здесь был бы шумом (та же причина, по которой
// R493 убрал warn «force rebalance failed» с нормального пути).
func logRequestPlanR493(plan *requestPlanR493) {
	if plan == nil {
		return
	}
	logger.Get().Debugw("request plan: единое решение о запросе",
		"surface", plan.Surface.String(),
		"stream", plan.Stream,
		"stream_source", plan.StreamSource,
		"model", plan.Model,
		"nctx", plan.NCtx,
		"output_limit", plan.OutputLimit,
		"output_limit_source", plan.OutputLimitSource)
}
