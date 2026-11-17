//go:build llama_stub

// tool_guard_r496_test.go — R496 (2026-11-18).
//
// ЗАЧЕМ. Живая жалоба владельца: на слабой модели агентский ход заканчивается
// «сам» — модель описывает, что сделает, инструмент не вызывает, клиент видит
// завершённый ответ. Сторож обязан отличать такой ход от здорового по уже
// измеряемым полям, а не по «кажется, пусто».
//
// Ключевое свойство, которое здесь закреплено: дверь открывается ровно один раз.
// Ложное срабатывание на содержательном ответе так же плохо, как пропущенный
// срыв: первое заставляет оператора игнорировать предупреждение.
package main

import (
	"net/http/httptest"
	"testing"
)

// TestR496_Classify — таблица вердиктов.
func TestR496_Classify(t *testing.T) {
	cases := []struct {
		name       string
		in         ToolGuardInputR496
		wantUnres  bool
		wantReason string
	}{
		{
			name: "инструменты не предлагали — сторожить нечего",
			in:   ToolGuardInputR496{ToolsOffered: false, DoneReason: "stop"},
		},
		{
			name: "вызов состоялся — ход результативный",
			in:   ToolGuardInputR496{ToolsOffered: true, ToolCalls: 1, DoneReason: "tool_calls"},
		},
		{
			name: "пустой ответ без вызова — срыв",
			in:   ToolGuardInputR496{ToolsOffered: true, DoneReason: "stop"},
			// final content пуст
			wantUnres:  true,
			wantReason: "empty_answer",
		},
		{
			name:       "обещание без действия — срыв",
			in:         ToolGuardInputR496{ToolsOffered: true, DoneReason: "stop", VisibleContent: "Сейчас создам файл и приступаю к правке."},
			wantUnres:  true,
			wantReason: "narration_without_action",
		},
		{
			name: "обещание по-английски — срыв",
			in:   ToolGuardInputR496{ToolsOffered: true, DoneReason: "stop", VisibleContent: "I'll update the file now."},
			// детектор не должен зависеть от языка клиента
			wantUnres:  true,
			wantReason: "narration_without_action",
		},
		{
			name: "содержательный ответ — НЕ срыв",
			in: ToolGuardInputR496{ToolsOffered: true, DoneReason: "stop",
				VisibleContent: "Готово: файл index.html содержит ссылки на модули, страница открывается."},
		},
		{
			name: "ответ с кодом — НЕ срыв",
			in: ToolGuardInputR496{ToolsOffered: true, DoneReason: "stop",
				VisibleContent: "Сейчас покажу содержимое:\n```html\n<div>ok</div>\n```"},
		},
		{
			name: "бюджет исчерпан — НЕ срыв (обрезали, а не сорвалась)",
			in: ToolGuardInputR496{ToolsOffered: true, DoneReason: "length",
				BudgetExhausted: true},
		},
		{
			name: "остановка по length без бюджета — не наш случай",
			in:   ToolGuardInputR496{ToolsOffered: true, DoneReason: "length"},
		},
		{
			name: "ошибка модели — не наш случай",
			in:   ToolGuardInputR496{ToolsOffered: true, DoneReason: "error"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyToolGuardR496(c.in)
			if got.Unresolved != c.wantUnres {
				t.Fatalf("Unresolved = %v, ожидалось %v (reason=%q, content=%q)",
					got.Unresolved, c.wantUnres, got.Reason, c.in.VisibleContent)
			}
			if c.wantUnres && got.Reason != c.wantReason {
				t.Fatalf("Reason = %q, ожидалось %q", got.Reason, c.wantReason)
			}
		})
	}
}

// TestR496_ModeFromEnv — флаг и его дефолт.
func TestR496_ModeFromEnv(t *testing.T) {
	cases := []struct {
		env  string
		want ToolGuardModeR496
	}{
		{"", ToolGuardOffR496},        // без флага поведение не меняется
		{"off", ToolGuardOffR496},     // явное выключение
		{"0", ToolGuardOffR496},       // числовая форма
		{"warn", ToolGuardWarnR496},   // рекомендованный дефолт после включения
		{"warn", ToolGuardWarnR496},   // повтор
		{"retry", ToolGuardRetryR496}, // осознанное включение продолжений
		{"RETRY", ToolGuardRetryR496}, // регистр не важен
		{"banana", ToolGuardWarnR496}, // неизвестное → warn, не молчание
	}

	for _, c := range cases {
		resetToolGuardModeR496()
		t.Setenv(toolGuardEnvR496, c.env)
		if got := toolGuardModeFromEnvR496(); got != c.want {
			t.Errorf("LB_TOOL_GUARD=%q → %v, ожидалось %v", c.env, got, c.want)
		}
	}
	resetToolGuardModeR496()
}

// TestR496_HeaderOnlyWhenUnresolved — заголовки не появляются ни при off, ни при
// здоровом ходе: иначе клиент начнёт видеть предупреждение там, где всё в порядке.
func TestR496_HeaderOnlyWhenUnresolved(t *testing.T) {
	verdict := ToolGuardVerdictR496{Unresolved: true, Reason: "empty_answer"}

	// off — заголовков нет вообще (ноль изменений на проводе).
	resetToolGuardModeR496()
	t.Setenv(toolGuardEnvR496, "off")
	w := httptest.NewRecorder()
	applyToolGuardHeaderR496(w, verdict)
	if got := w.Header().Get(toolGuardHeaderR496); got != "" {
		t.Errorf("при off выставлен %s=%q — поведение изменилось без флага", toolGuardHeaderR496, got)
	}

	// warn — заголовки есть.
	resetToolGuardModeR496()
	t.Setenv(toolGuardEnvR496, "warn")
	w = httptest.NewRecorder()
	applyToolGuardHeaderR496(w, verdict)
	if got := w.Header().Get(toolGuardHeaderR496); got != "unresolved" {
		t.Errorf("%s = %q, ожидалось unresolved", toolGuardHeaderR496, got)
	}
	if got := w.Header().Get(toolGuardHeaderReasonR496); got != "empty_answer" {
		t.Errorf("%s = %q, ожидалось empty_answer", toolGuardHeaderReasonR496, got)
	}

	// здоровый ход — заголовков нет даже в warn.
	w = httptest.NewRecorder()
	applyToolGuardHeaderR496(w, ToolGuardVerdictR496{})
	if got := w.Header().Get(toolGuardHeaderR496); got != "" {
		t.Errorf("заголовок выставлен для здорового хода: %q", got)
	}
	resetToolGuardModeR496()
}

// TestR496_CountersCountOnlyUnresolved — счётчик «проверено» растёт всегда, а
// «не разрешено» — только на срывах. Иначе оператор не поймёт долю срывов.
func TestR496_CountersCountOnlyUnresolved(t *testing.T) {
	resetToolGuardModeR496()
	resetToolGuardCountersR496()
	t.Cleanup(func() { resetToolGuardModeR496() })
	t.Setenv(toolGuardEnvR496, "warn")

	// Здоровый ход.
	observeToolGuardR496("m", "/api/chat", ToolGuardInputR496{ToolsOffered: true, ToolCalls: 2, DoneReason: "tool_calls"})
	// Срыв.
	observeToolGuardR496("m", "/api/chat", ToolGuardInputR496{ToolsOffered: true, DoneReason: "stop"})

	checked, unresolved := ToolGuardCountersR496()
	if checked != 2 {
		t.Errorf("checked = %d, ожидалось 2", checked)
	}
	if unresolved != 1 {
		t.Errorf("unresolved = %d, ожидалось 1", unresolved)
	}

	// Режим off не считает ничего (прежнее поведение).
	resetToolGuardModeR496()
	t.Setenv(toolGuardEnvR496, "off")
	observeToolGuardR496("m", "/api/chat", ToolGuardInputR496{ToolsOffered: true, DoneReason: "stop"})
	checked2, unresolved2 := ToolGuardCountersR496()
	if checked2 != 2 || unresolved2 != 1 {
		t.Errorf("в режиме off счётчики изменились: checked=%d unresolved=%d", checked2, unresolved2)
	}
}
