// route_matrix_t1_test.go — T1 из плана «два мозга» (refactoring-plan.ru.md §3):
// матрица маршрутов. Предохранитель ДО рефакторинга.
//
// ЗАЧЕМ. План говорит прямо: «теста, фиксирующего, какой внутренний путь
// обслуживает /api/chat против /v1/chat/completions, НЕТ (поэтому расхождение и
// живёт)». Здесь эта матрица фиксируется как есть, на текущем коде, с двумя
// инвариантами, которые важны для будущих этапов:
//
//  1. РЕШЕНИЕ О ПУТИ НЕ ЗАВИСИТ ОТ ТЕЛА ЗАПРОСА. `stream:true|false|нет` и
//     `tools:да|нет` не меняют, кто обслуживает запрос — иначе клиент, не
//     приславший `stream`, попадает в другой путь, чем клиент с `stream:false`
//     (это и был источник расхождений R484).
//  2. АЛИАС НЕ МЕНЯЕТ РЕШЕНИЕ. `/ollama/api/chat` после нормализации R479 — тот же
//     путь, что `/api/chat`, значит и решение обязано совпадать.
//
// Что считается «решением»: `routeRequest` вернул true (запрос обслужил
// специализированный роутер — handler-путь) или false (запрос уходит в общий
// ServeHTTP-поток с selectBackend — generic-путь). Отдельно ловится маркер
// смешанного режима («mixed mode, falling through to selectBackend»), потому что
// именно он объясняет, ПОЧЕМУ запрос ушёл в generic-путь.
package balancer

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// routeDecisionT1 — чем закончился routeRequest для одного случая.
type routeDecisionT1 struct {
	handled     bool   // true: обслужил роутер (handler-путь)
	whyFallback string // маркер причины, если ушёл в generic-путь
}

// routeProbeT1 — один прогон routeRequest с захватом логов.
func routeProbeT1(t *testing.T, p *Proxy, path, body string) routeDecisionT1 {
	t.Helper()
	var buf bytes.Buffer
	restore := logger.CaptureToBuffer(&buf)
	defer restore()

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handled := p.routeRequest(w, req)
	logs := buf.String()

	dec := routeDecisionT1{handled: handled}
	switch {
	case strings.Contains(logs, "mixed mode, falling through to selectBackend"):
		dec.whyFallback = "mixed_mode_chat_generate"
	case !handled:
		dec.whyFallback = "not_handled_by_routers"
	}
	return dec
}

// matrixProxyT1 — прокси с заданным набором типов бэкендов. Адреса указывают на
// закрытый порт: тест проверяет РЕШЕНИЕ о пути, а не доставку, и не должен
// зависеть от живых серверов (иначе он флакал бы по таймаутам).
func matrixProxyT1(t *testing.T, backendTypes ...types.BackendType) *Proxy {
	t.Helper()
	backends := make([]types.Backend, 0, len(backendTypes))
	for i, bt := range backendTypes {
		backends = append(backends, types.Backend{
			ID:                string(rune('a'+i)) + "-t1",
			Name:              string(bt),
			Host:              "127.0.0.1",
			OllamaPort:        1, // закрыт: доставка не проверяется
			Type:              bt,
			Status:            types.StatusHealthy,
			MaxConcurrentReqs: 1,
			Weight:            1,
		})
	}
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "127.0.0.1", Port: 18080, APIPort: 18081},
		Backends:     backends,
	}
	p := newProxyWithCleanup(t, conf)
	p.SetQueueManagerProxy()
	t.Cleanup(func() {
		p.queueMgr.Stop()
		p.StopSessionManager()
	})
	return p
}

// bodiesT1 — варианты тела, которые НЕ должны влиять на решение о пути.
var bodiesT1 = []struct {
	name string
	body string
}{
	{"stream_absent", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`},
	{"stream_false", `{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`},
	{"stream_true", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
	{"tools", `{"model":"m","stream":false,"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"messages":[{"role":"user","content":"hi"}]}`},
}

// pathsT1 — пути, для которых фиксируется матрица.
var pathsT1 = []string{
	"/api/chat",
	"/api/generate",
	"/v1/chat/completions",
	"/v1/completions",
}

// TestT1_RouteDecisionIndependentOfBody — ключевой инвариант T1: для одного и того
// же пути с одним и тем же кластером решение одинаково при любом теле.
func TestT1_RouteDecisionIndependentOfBody(t *testing.T) {
	clusters := []struct {
		name     string
		backends []types.BackendType
	}{
		{"llama_only", []types.BackendType{types.BackendTypeLlamaCpp}},
		{"ollama_only", []types.BackendType{types.BackendTypeOllama}},
		{"mixed", []types.BackendType{types.BackendTypeLlamaCpp, types.BackendTypeOllama}},
	}

	for _, cl := range clusters {
		for _, path := range pathsT1 {
			base := routeProbeT1(t, matrixProxyT1(t, cl.backends...), path, bodiesT1[0].body)
			for _, b := range bodiesT1[1:] {
				got := routeProbeT1(t, matrixProxyT1(t, cl.backends...), path, b.body)
				if got != base {
					t.Errorf("кластер %s, путь %s: решение зависит от тела запроса "+
						"(без stream: %+v, тело %s: %+v) — клиент без поля stream попадёт "+
						"в другой путь, чем клиент с stream:false",
						cl.name, path, base, b.name, got)
				}
			}
		}
	}
}

// TestT1_AliasDoesNotChangeDecision — R479: алиас снимается до диспетчеризации,
// поэтому решение для /ollama/api/chat обязано совпадать с /api/chat.
func TestT1_AliasDoesNotChangeDecision(t *testing.T) {
	p := matrixProxyT1(t, types.BackendTypeLlamaCpp, types.BackendTypeOllama)

	direct := routeProbeT1(t, p, "/api/chat", bodiesT1[1].body)

	aliasPath, stripped := normalizeSurfaceAliasPathR479("/ollama/api/chat")
	if !stripped {
		t.Fatalf("R479: алиас /ollama/api/chat не распознан — нормализация сломана")
	}
	if aliasPath != "/api/chat" {
		t.Fatalf("R479: алиас нормализован в %q, ожидалось /api/chat", aliasPath)
	}

	aliased := routeProbeT1(t, matrixProxyT1(t, types.BackendTypeLlamaCpp, types.BackendTypeOllama),
		aliasPath, bodiesT1[1].body)
	if aliased != direct {
		t.Errorf("решение для алиаса %q (%+v) != решение для /api/chat (%+v)",
			aliasPath, aliased, direct)
	}
}

// TestT1_BackendTypePerSurface — R112: тип, который видит диспетчер, задаётся
// ПОВЕРХНОСТЬЮ, а не составом кластера.
//
//   - /api/*  → "" (getDefaultAllowedTypes() отдаёт три типа, len>1), поэтому
//     дальше решает selectBackend по модели/ресурсам;
//   - /v1/*   → llama_cpp ВСЕГДА: это «признак того, каким кодом обслуживается
//     путь», а не деление стенда по типу (комментарий R112 в proxy.go);
//   - image-путь → image независимо от поверхности.
//
// Проверка нужна отдельно от матрицы: матрица смотрит на ИСХОД (кто обслужил), и
// подмена внутреннего сигнала типа в ней не видна, хотя контракт R112 меняется.
func TestT1_BackendTypePerSurface(t *testing.T) {
	p := matrixProxyT1(t, types.BackendTypeLlamaCpp, types.BackendTypeOllama)

	cases := []struct {
		path string
		want types.BackendType
	}{
		{"/api/chat", ""},
		{"/api/generate", ""},
		{"/v1/chat/completions", types.BackendTypeLlamaCpp},
		{"/v1/completions", types.BackendTypeLlamaCpp},
		{"/v1/images/generations", types.BackendTypeImage},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(bodiesT1[1].body))
		got := p.determineRequestBackendType(req)
		if got != c.want {
			t.Errorf("%s: determineRequestBackendType = %q, ожидалось %q "+
				"(R112: тип задаёт поверхность, а не состав кластера)", c.path, got, c.want)
		}
	}
}

// TestT1_CurrentMatrix — ФИКСАЦИЯ текущей матрицы маршрутов.
//
// Это не «так должно быть» из документации, а «так ЕСТЬ СЕЙЧАС» — снимок, который
// обязан измениться только осознанно, вместе с этапом плана
// refactoring-plan.ru.md. Когда этап 5 (один runInference) сведёт пути, этот тест
// придётся править — и это сигнал, что маршрутизация действительно изменилась.
//
// ЧТО ИЗМЕРЕНО (и почему именно так — по коду, а не по памяти):
//   - на /api/* тип определяется `getDefaultAllowedTypes()`; с R112 ВСЕ режимы
//     дают три типа, то есть len(allowed) > 1 → тип "" → чат уходит в общий поток
//     с selectBackend. Значит решение не зависит от состава кластера;
//   - на /v1/* `determineRequestBackendType` возвращает llama_cpp ВСЕГДА (R112:
//     «это признак того, каким кодом обслуживается путь», а не деления по типу),
//     поэтому /v1/* обслуживает роутер, а не generic-путь.
//
// Отсюда и «два мозга»: /api/chat и /v1/chat/completions идут РАЗНЫМИ путями при
// одном и том же кластере и одном и том же теле запроса.
func TestT1_CurrentMatrix(t *testing.T) {
	type want struct {
		handled     bool
		whyFallback string
	}
	cases := []struct {
		cluster string
		path    string
		want    want
		note    string
	}{
		// /api/* — generic-путь (selectBackend) при любом составе кластера.
		{"llama_only", "/api/chat", want{false, "mixed_mode_chat_generate"}, "generic-путь: bt==\"\" (allowed>1)"},
		{"ollama_only", "/api/chat", want{false, "mixed_mode_chat_generate"}, "generic-путь: bt==\"\" (allowed>1)"},
		{"mixed", "/api/chat", want{false, "mixed_mode_chat_generate"}, "generic-путь: bt==\"\""},
		{"mixed", "/api/generate", want{false, "mixed_mode_chat_generate"}, "generic-путь: bt==\"\""},
		// /v1/* — handler-путь (роутер), тип llama_cpp по R112.
		{"llama_only", "/v1/chat/completions", want{true, ""}, "OpenAI-поверхность обслуживает роутер"},
		{"ollama_only", "/v1/chat/completions", want{true, ""}, "OpenAI-поверхность обслуживает роутер"},
		{"mixed", "/v1/chat/completions", want{true, ""}, "OpenAI-поверхность обслуживает роутер"},
		{"mixed", "/v1/completions", want{true, ""}, "OpenAI-поверхность обслуживает роутер"},
	}

	for _, c := range cases {
		var backends []types.BackendType
		switch c.cluster {
		case "llama_only":
			backends = []types.BackendType{types.BackendTypeLlamaCpp}
		case "ollama_only":
			backends = []types.BackendType{types.BackendTypeOllama}
		default:
			backends = []types.BackendType{types.BackendTypeLlamaCpp, types.BackendTypeOllama}
		}
		p := matrixProxyT1(t, backends...)
		got := routeProbeT1(t, p, c.path, bodiesT1[1].body)
		if got.handled != c.want.handled || got.whyFallback != c.want.whyFallback {
			t.Errorf("матрица изменилась: кластер %s, путь %s → handled=%v why=%q, "+
				"зафиксировано handled=%v why=%q (%s). Если это осознанный этап "+
				"refactoring-plan.ru.md — обновите ожидание и документ, иначе это регресс",
				c.cluster, c.path, got.handled, got.whyFallback,
				c.want.handled, c.want.whyFallback, c.note)
		}
	}
}

// TestT1_TwoBrainsAreMeasured — «два мозга» из плана подтверждаются одним
// прогоном: ОДИН кластер, ОДНО тело запроса, но /api/chat и /v1/chat/completions
// обслуживаются разными путями.
//
// Тест не «лечит» расхождение (это этапы 1–7 плана), а фиксирует, что оно есть и
// измерено: пока эта проверка зелёная, никто не может сказать, что пути уже
// сведены, и наоборот — если пути неожиданно совпадут, тест это заметит.
func TestT1_TwoBrainsAreMeasured(t *testing.T) {
	body := bodiesT1[1].body
	generic := routeProbeT1(t, matrixProxyT1(t, types.BackendTypeLlamaCpp, types.BackendTypeOllama),
		"/api/chat", body)
	handler := routeProbeT1(t, matrixProxyT1(t, types.BackendTypeLlamaCpp, types.BackendTypeOllama),
		"/v1/chat/completions", body)

	if generic.handled == handler.handled {
		t.Fatalf("пути СОШЛИСЬ (оба handled=%v) — если это результат этапов плана, "+
			"обновите этот тест и refactoring-plan.ru.md; иначе расхождение потеряно",
			generic.handled)
	}
	t.Logf("измерено: /api/chat → generic (%s), /v1/chat/completions → handler",
		generic.whyFallback)
}
