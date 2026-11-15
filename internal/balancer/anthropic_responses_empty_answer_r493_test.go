// anthropic_responses_empty_answer_r493_test.go — R493, этап 6 (продолжение):
// «молчаливой пустоты» не должно быть на ВСЕХ поверхностях.
//
// КОНТРАКТ R261 (основной путь): если весь ответ ушёл в размышления, клиент
// получает кадр-объяснение, а не пустоту — иначе он рисует пустой ответ и
// пользователь считает, что модель сломалась. На поверхностях Anthropic
// (`forwardOpenAISSEToAnthropic`) и Responses (`forwardOpenAISSEToResponses`)
// транслятор выбрасывал reasoning_content и закрывал поток с ПУСТЫМ текстовым
// блоком: объяснения не было вовсе.
//
// Здесь проверяется именно это: поток, где есть только reasoning_content и
// finish_reason, обязан отдать клиенту непустой текст (объяснение), а обычный
// поток — не приобрести лишнего текста.
package balancer

import (
	"bytes"
	"strings"
	"testing"
)

// reasoningOnlySSE — поток, где весь ответ ушёл в размышления.
func reasoningOnlySSE() string {
	return `data: {"id":"c1","model":"m1","choices":[{"index":0,"delta":{"reasoning_content":"думаю очень долго"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"c1","model":"m1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"
}

// normalSSE — обычный ответ с текстом.
func normalSSE() string {
	return `data: {"id":"c1","model":"m1","choices":[{"index":0,"delta":{"content":"привет"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"c1","model":"m1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"
}

// TestR493Anthropic_ReasoningOnlyGetsNotice — на Anthropic-поверхности клиент
// обязан увидеть объяснение вместо пустого текстового блока.
func TestR493Anthropic_ReasoningOnlyGetsNotice(t *testing.T) {
	var out bytes.Buffer
	if err := forwardOpenAISSEToAnthropic(strings.NewReader(reasoningOnlySSE()), &out, nil, "m1"); err != nil {
		t.Fatalf("трансляция: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, "content_block_delta") {
		t.Fatalf("нет текстового блока вовсе — клиент увидит пустой ответ: %q", body)
	}
	if !strings.Contains(body, "размышлен") && !strings.Contains(body, "reasoning") && !strings.Contains(body, "пуст") {
		t.Fatalf("объяснения пустого ответа нет (R261 не доехал до Anthropic): %q", body)
	}
}

// TestR493Anthropic_NormalStreamHasNoNotice — обычный поток не должен получить
// лишний текст: иначе объяснение приклеится к нормальному ответу.
func TestR493Anthropic_NormalStreamHasNoNotice(t *testing.T) {
	var out bytes.Buffer
	if err := forwardOpenAISSEToAnthropic(strings.NewReader(normalSSE()), &out, nil, "m1"); err != nil {
		t.Fatalf("трансляция: %v", err)
	}
	body := out.String()
	if strings.Contains(body, "размышлен") {
		t.Fatalf("к обычному ответу приклеено объяснение пустого ответа: %q", body)
	}
	if !strings.Contains(body, "привет") {
		t.Fatalf("текст ответа потерян: %q", body)
	}
}

// TestR493Responses_ReasoningOnlyGetsNotice — то же для поверхности Responses.
func TestR493Responses_ReasoningOnlyGetsNotice(t *testing.T) {
	var out bytes.Buffer
	if err := forwardOpenAISSEToResponses(strings.NewReader(reasoningOnlySSE()), &out, nil, "m1"); err != nil {
		t.Fatalf("трансляция: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, "response.output_text.delta") {
		t.Fatalf("нет текстовой дельты — клиент увидит пустой ответ: %q", body)
	}
	if !strings.Contains(body, "размышлен") && !strings.Contains(body, "reasoning") && !strings.Contains(body, "пуст") {
		t.Fatalf("объяснения пустого ответа нет (R261 не доехал до Responses): %q", body)
	}
}

// TestR493Responses_NormalStreamHasNoNotice — контроль для Responses.
func TestR493Responses_NormalStreamHasNoNotice(t *testing.T) {
	var out bytes.Buffer
	if err := forwardOpenAISSEToResponses(strings.NewReader(normalSSE()), &out, nil, "m1"); err != nil {
		t.Fatalf("трансляция: %v", err)
	}
	body := out.String()
	if strings.Contains(body, "размышлен") {
		t.Fatalf("к обычному ответу приклеено объяснение пустого ответа: %q", body)
	}
	if !strings.Contains(body, "привет") {
		t.Fatalf("текст ответа потерян: %q", body)
	}
}
