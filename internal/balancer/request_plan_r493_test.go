package balancer

// request_plan_r493_test.go — этап 1: план запроса как единственное решение.
//
// ПОЧЕМУ ТЕСТЫ НА ЧИСТУЮ СБОРКУ, А НЕ НА ПОВЕДЕНИЕ. Этот шаг ничего не меняет у
// клиента: план только собирается и кладётся рядом с телом. Проверяем ровно то,
// что обещает дизайн: значения берутся из тела (а не из догадок), источник
// решения виден, и план доступен потребителям без повторного разбора тела.
// Переключение ResolveNumCtx/preflight/handler-путей на план — следующие шаги,
// у каждого будет свой поведенческий тест (docs/architecture/request-plan-r493.md §4).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestR493_OutputLimitFromBody(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		want  int
		wantS string
	}{
		{"пустое тело", ``, 0, ""},
		{"не JSON", `not json`, 0, ""},
		{"num_predict", `{"options":{"num_predict":256}}`, 256, "options.num_predict"},
		{"num_predict отрицательный = без лимита", `{"options":{"num_predict":-1}}`, 0, ""},
		{"max_tokens", `{"max_tokens":4096}`, 4096, "max_tokens"},
		{"max_completion_tokens", `{"max_completion_tokens":8192}`, 8192, "max_completion_tokens"},
		{"num_predict приоритетнее max_tokens", `{"options":{"num_predict":64},"max_tokens":4096}`, 64, "options.num_predict"},
		{"ноль = не ограничивал", `{"max_tokens":0}`, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, src := outputLimitFromBodyR493([]byte(c.body))
			if got != c.want || src != c.wantS {
				t.Fatalf("outputLimitFromBodyR493 = (%d, %q), want (%d, %q)", got, src, c.want, c.wantS)
			}
		})
	}
}

func TestR493_BuildRequestPlanNormalizesLimit(t *testing.T) {
	// Отрицательный num_predict — «без ограничения» по семантике Ollama: в плане
	// не должен превратиться в лимит (иначе авто-продолжение и клиентские
	// решения начнут считать, что клиент ограничил длину).
	plan := buildRequestPlanR493(surfaceOpenAI, "m", true, "", 8192, -5, "options.num_predict")
	if plan.OutputLimit != 0 || plan.OutputLimitSource != "" {
		t.Fatalf("отрицательный лимит не нормализован: %d %q", plan.OutputLimit, plan.OutputLimitSource)
	}
	if plan.StreamSource != streamSourceDefaultR493 {
		t.Fatalf("пустой источник стрима должен стать %q, got %q", streamSourceDefaultR493, plan.StreamSource)
	}
	if plan.Surface != surfaceOpenAI {
		t.Fatalf("поверхность потеряна: %v", plan.Surface)
	}
}

func TestR493_PlanAvailableWithoutReparsingBody(t *testing.T) {
	// План собирается в parseRequestBody — то есть ровно там, где тело читается
	// ОДИН раз (body_single_read_r493_test.go это уже стережёт). Здесь важно, что
	// потребитель получает план из контекста/parsedRequest, не разбирая тело снова.
	p := &Proxy{}
	body := `{"model":"qwen","stream":false,"options":{"num_ctx":16384,"num_predict":512}}`
	r := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))

	parsed := p.parseRequestBody(r)
	if parsed.Plan == nil {
		t.Fatal("parseRequestBody не собрал план")
	}
	if parsed.Plan.Model != "qwen" || parsed.Plan.NCtx != 16384 || parsed.Plan.OutputLimit != 512 {
		t.Fatalf("план собран неверно: %+v", parsed.Plan)
	}
	if parsed.Plan.Stream != false || parsed.Plan.StreamSource != streamSourceClientR493 {
		t.Fatalf("стрим/источник неверны: %v %q", parsed.Plan.Stream, parsed.Plan.StreamSource)
	}

	// Мост: план виден через контекст запроса, куда положили parsedRequest.
	r2 := withParsedRequest(r, parsed)
	if got := requestPlanOfR493(r2); got == nil || got.OutputLimit != 512 {
		t.Fatalf("requestPlanOfR493 не вернул план из parsedRequest: %+v", got)
	}
	// Пустой запрос — плана нет (nil безопасен для потребителей).
	if got := requestPlanOfR493(httptest.NewRequest(http.MethodGet, "/api/tags", nil)); got != nil {
		t.Fatalf("для запроса без тела план не должен существовать: %+v", got)
	}
}

func TestR493_StreamDefaultIsPerSurface(t *testing.T) {
	// Дефолт стрима — свойство ПОВЕРХНОСТИ, а не клиента: legacy 18080 стримит по
	// умолчанию, OpenAI 18079 — только явным полем. План обязан это фиксировать
	// источником, иначе на следующем шаге «клиент ждёт поток» смешается с
	// «сервер решил стримить».
	p := &Proxy{}
	legacy := p.parseRequestBody(httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"model":"m"}`)))
	if legacy.Plan == nil || !legacy.Plan.Stream || legacy.Plan.StreamSource != streamSourceDefaultR493 {
		t.Fatalf("legacy без поля stream: ожидался дефолт поверхности, got %+v", legacy.Plan)
	}
	openaiReq := withRequestSurface(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`)), surfaceOpenAI)
	openai := p.parseRequestBody(openaiReq)
	if openai.Plan == nil || openai.Plan.Stream || openai.Plan.Surface != surfaceOpenAI {
		t.Fatalf("openai без поля stream: ожидался не-стрим и surface=openai, got %+v", openai.Plan)
	}
}
