// modelfamily_r268_test.go — R268: отказ «версия не поддержана для конвертации»
// на пути конвертации и МОЛЧАНИЕ на passthrough.
//
// ЧТО ЗАЩИЩАЕМ. Отказ полезен только там, где балансер действительно
// преобразует ответ (SSE → NDJSON Ollama). На passthrough (`/v1/*` и нативный
// Ollama-путь) ответ идёт как есть, и любой отказ там был бы запретом на
// исправную работу — именно это и проверяется вторым тестом.
package balancer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func newFamilyRefusalProxy(openAIPort int) *Proxy {
	return &Proxy{
		config: &types.LoadBalancerConfig{
			LoadBalancer: types.LoadBalancerSettings{OpenAIPort: openAIPort},
		},
	}
}

// TestR268_UnsupportedVersionRefusedOnConversionPath — понятный отказ вместо
// испорченного ответа.
func TestR268_UnsupportedVersionRefusedOnConversionPath(t *testing.T) {
	p := newFamilyRefusalProxy(18079)
	rec := httptest.NewRecorder()

	refused := p.refuseUnsupportedModelFamily(rec, "/api/chat", "qwen4.0-70b", true, true)
	if !refused {
		t.Fatal("ожидался отказ для qwen4.0 на пути конвертации")
	}
	body := rec.Body.String()
	for _, want := range []string{"не поддерживается", "Ollama API", "18079"} {
		if !strings.Contains(body, want) {
			t.Errorf("в ответе нет %q: %s", want, body)
		}
	}
	// Клиент Ollama получает ТЕРМИНАЛЬНЫЙ чанк: done:true обязателен, иначе
	// клиент будет ждать продолжения до собственного таймаута.
	if !strings.Contains(body, `"done":true`) {
		t.Errorf("нет терминального done:true в отказе: %s", body)
	}
	// Ошибка объясняет причину, а не просто «error».
	if !strings.Contains(body, `"error"`) {
		t.Errorf("нет поля error в отказе: %s", body)
	}
}

// TestR268_PassthroughNeverRefused — на passthrough отказ запрещён.
func TestR268_PassthroughNeverRefused(t *testing.T) {
	p := newFamilyRefusalProxy(18079)
	cases := []struct{ path, model string }{
		{"/v1/chat/completions", "qwen4.0-70b"}, // OpenAI passthrough
		{"/api/chat", "qwen4.0-70b"},            // нативный Ollama-путь (converting=false)
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		if p.refuseUnsupportedModelFamily(rec, c.path, c.model, true, false) {
			t.Errorf("%s: отказ на passthrough недопустим (модель %s)", c.path, c.model)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("%s: в passthrough что-то записали: %s", c.path, rec.Body.String())
		}
	}
}

// TestR268_SupportedAndUnknownNotRefused — исправная работа не ломается.
func TestR268_SupportedAndUnknownNotRefused(t *testing.T) {
	p := newFamilyRefusalProxy(18079)
	for _, model := range []string{"Qwen3.8-27B-UD-Q4_K_M", "gemma-4-E4B", "llama-3.1-8b", "my-custom-gguf"} {
		rec := httptest.NewRecorder()
		if p.refuseUnsupportedModelFamily(rec, "/api/chat", model, true, true) {
			t.Errorf("%s: отказ для поддержанной/неизвестной модели недопустим", model)
		}
	}
}

// TestR268_RefusalPortFallsBackToDefault — если порт не сконфигурирован, в
// тексте всё равно есть конкретное число (иначе отказ бесполезен оператору).
func TestR268_RefusalPortFallsBackToDefault(t *testing.T) {
	p := newFamilyRefusalProxy(0)
	rec := httptest.NewRecorder()
	if !p.refuseUnsupportedModelFamily(rec, "/api/chat", "gemma-5-27b", false, true) {
		t.Fatal("ожидался отказ для gemma-5")
	}
	if !strings.Contains(rec.Body.String(), "18079") {
		t.Errorf("в отказе нет порта по умолчанию 18079: %s", rec.Body.String())
	}
}

// TestR268_FamilyPreferenceForDetector — диалект из семейства доступен тракту
// (это точка расширения: новое семейство приносит свой формат вызова само).
func TestR268_FamilyPreferenceForDetector(t *testing.T) {
	p := newFamilyRefusalProxy(18079)
	if got := p.modelFamilyPreference("gemma-4-E4B", true, "/api/chat"); got != "tool_code" {
		t.Errorf("gemma: preference = %q, want tool_code", got)
	}
	if got := p.modelFamilyPreference("Qwen3.8-27B", true, "/api/chat"); got != "tool_calls_marker" {
		t.Errorf("qwen: preference = %q, want tool_calls_marker", got)
	}
	if got := p.modelFamilyPreference("unknown-model", true, "/api/chat"); got != "" {
		t.Errorf("unknown: preference = %q, want пусто (детектор работает как раньше)", got)
	}
}

var _ = http.MethodPost
