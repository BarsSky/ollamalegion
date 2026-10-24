//go:build llama_stub

// empty_output_retry_r110_test.go — R110 (2026-10-13): повтор генерации при
// «пустом» выводе, найденный прогоном настоящего Cline CLI.
//
// Живой случай (gemma-4-E4B-it-Q4_K_M, Cline, tools-путь): примерно в одном
// прогоне из трёх модель отвечала ОДНИМ служебным токеном `<end_of_turn>`, и
// клиент получал «model produced an empty response (inference succeeded but
// output is empty)». Причина — повтор срабатывал только при raw_len == 0, а тут
// сырой вывод непустой, но после чистки видимого текста нет.
package main

import (
	"context"
	"strings"
	"testing"
)

// TestR110_RetryOnServiceTokenOnlyOutput — тот самый живой случай.
func TestR110_RetryOnServiceTokenOnlyOutput(t *testing.T) {
	if !shouldRetryEmptyStreamingOutput("<end_of_turn>", 0, context.Background()) {
		t.Error("вывод из одного служебного токена обязан считаться пустым и повторяться " +
			"(иначе клиент получает «model produced an empty response»)")
	}
	if !shouldRetryEmptyStreamingOutput("", 0, context.Background()) {
		t.Error("raw_len=0 обязан повторяться (поведение R83/v59 не должно сломаться)")
	}
	if !shouldRetryEmptyStreamingOutput("\n<end_of_turn>\n<|end|>", 0, context.Background()) {
		t.Error("несколько служебных токенов подряд — тоже пустой вывод")
	}
}

// TestR110_NoRetryWhenSomethingWasSent — контракт безопасности: если клиенту уже
// отданы байты content, повтор запрещён (он получил бы два ответа подряд).
func TestR110_NoRetryWhenSomethingWasSent(t *testing.T) {
	if shouldRetryEmptyStreamingOutput("<end_of_turn>", 13, context.Background()) {
		t.Error("повтор запрещён, когда клиенту уже отправлен content")
	}
	if shouldRetryEmptyStreamingOutput("", 1, context.Background()) {
		t.Error("повтор запрещён даже при raw_len=0, если что-то уже ушло")
	}
}

// TestR110_NoRetryWhenAnswerExists — обычный ответ повторять нельзя.
func TestR110_NoRetryWhenAnswerExists(t *testing.T) {
	if shouldRetryEmptyStreamingOutput("ок", 0, context.Background()) {
		t.Error("непустой видимый ответ повторять нельзя")
	}
	if shouldRetryEmptyStreamingOutput("ок<end_of_turn>", 0, context.Background()) {
		t.Error("ответ со служебным токеном в хвосте — не пустой")
	}
}

// TestR110_NoRetryWhenClientGone — клиент ушёл: повтор бессмыслен (R83/v62).
func TestR110_NoRetryWhenClientGone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if shouldRetryEmptyStreamingOutput("", 0, ctx) {
		t.Error("при отменённом контексте повтор не делается")
	}
}

// TestR110_RetryDisabledByEnv — аварийный рубильник уважается.
func TestR110_RetryDisabledByEnv(t *testing.T) {
	t.Setenv("CPPWORKER_EMPTY_OUTPUT_RETRY", "off")
	if shouldRetryEmptyStreamingOutput("", 0, context.Background()) {
		t.Error("CPPWORKER_EMPTY_OUTPUT_RETRY=off обязан выключать повтор")
	}
}

// TestR110_SplitToolsOutput — разбор вывода: размышления отдельно, вызовы
// инструмента — из текста без размышлений; think=false скрывает размышления, но
// флаг «они были» сохраняется (нужен логу и повтору).
func TestR110_SplitToolsOutput(t *testing.T) {
	output := "<|channel>thought\nнадо посмотреть файлы\n<channel|>" +
		`<|tool_call>call:run_commands{commands:[<|"|>dir<|"|>]}<tool_call|>`

	reasoning, visible, calls, hasReasoning := splitToolsOutput(output, nil, nil)
	if !hasReasoning {
		t.Error("hasReasoning=false, хотя в выводе был блок размышлений")
	}
	if reasoning == "" {
		t.Error("размышления не отделены (reasoning пуст)")
	}
	if len(calls) != 1 || calls[0].Function.Name != "run_commands" {
		t.Fatalf("вызов не распознан: %+v", calls)
	}
	// Контракт: outputForParse — это текст БЕЗ размышлений, но с блоком вызова
	// (его и должен видеть парсер). Видимый ответ при найденном вызове пустым
	// делает обработчик (msg["content"] = ""), а не этот хелпер.
	if strings.Contains(visible, "надо посмотреть файлы") {
		t.Errorf("в тексте для парсера остались размышления: %q", visible)
	}
	if !strings.Contains(visible, "run_commands") {
		t.Errorf("в тексте для парсера нет самого вызова: %q", visible)
	}

	// think=false: размышления не отдаются, но hasReasoning остаётся true.
	no := false
	reasoning2, _, calls2, hasReasoning2 := splitToolsOutput(output, &no, nil)
	if reasoning2 != "" {
		t.Error("think=false обязан скрывать размышления")
	}
	if !hasReasoning2 {
		t.Error("hasReasoning обязан остаться true: модель размышляла, это важно для лога")
	}
	if len(calls2) != 1 {
		t.Errorf("вызов обязан распознаваться и при think=false, получено %d", len(calls2))
	}
}
