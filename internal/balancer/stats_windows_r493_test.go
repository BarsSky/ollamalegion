// stats_windows_r493_test.go — R493-12a: окна времени статистики.
//
// Проверяем ровно два правила, из-за которых жаловались на «скорости не как в
// реальности»:
//   - в «время промпта» не входят очередь/загрузка/preflight (свой отрезок);
//   - в «время генерации» входят размышления (их токены всегда в eval_count).
//
// Плюс границы: нулевые времена (путь без стрима) не превращаются в выдуманные
// числа, а сумма отрезков не превышает total.
package balancer

import (
	"testing"
	"time"
)

func TestR493_WindowsSegments(t *testing.T) {
	now := time.Now()
	w := statsWindowsR493{
		RequestStart: now.Add(-120 * time.Second),
		UpstreamSent: now.Add(-100 * time.Second), // 20 с: очередь + загрузка
		FirstDelta:   now.Add(-95 * time.Second),  // 5 с: префилл
		End:          now,                         // 95 с: генерация
	}
	d := w.Durations()
	if d.Load != int64(20*time.Second) {
		t.Fatalf("load = %d нс, ожидалось 20 с", d.Load)
	}
	if d.PromptEval != int64(5*time.Second) {
		t.Fatalf("prompt_eval = %d нс, ожидалось 5 с", d.PromptEval)
	}
	if d.Eval != int64(95*time.Second) {
		t.Fatalf("eval = %d нс, ожидалось 95 с", d.Eval)
	}
	if d.Total != int64(120*time.Second) {
		t.Fatalf("total = %d нс, ожидалось 120 с", d.Total)
	}
	if d.Load+d.PromptEval+d.Eval != d.Total {
		t.Fatalf("load+prompt+eval = %d, total = %d", d.Load+d.PromptEval+d.Eval, d.Total)
	}
}

// TestR493_WindowsLegacyNoDispatch — вызывающий не знает момент отправки:
// поведение прежнее (load=0, префилл от начала запроса), а не «нет данных — ноль
// у всего».
func TestR493_WindowsLegacyNoDispatch(t *testing.T) {
	now := time.Now()
	d := statsWindowsR493{
		RequestStart: now.Add(-10 * time.Second),
		FirstDelta:   now.Add(-4 * time.Second),
		End:          now,
	}.Durations()
	if d.Load != 0 {
		t.Fatalf("load = %d, ожидалось 0 (момент отправки неизвестен)", d.Load)
	}
	if d.PromptEval != int64(6*time.Second) {
		t.Fatalf("prompt_eval = %d, ожидалось 6 с (от начала запроса)", d.PromptEval)
	}
	if d.Eval != int64(4*time.Second) {
		t.Fatalf("eval = %d, ожидалось 4 с", d.Eval)
	}
}

// TestR493_WindowsNoFirstDelta — ответ пришёл одним done-чанком без стрима:
// всё время после отправки — промпт, генерации не выделяем (делить нечем).
func TestR493_WindowsNoFirstDelta(t *testing.T) {
	now := time.Now()
	d := statsWindowsR493{
		RequestStart: now.Add(-30 * time.Second),
		UpstreamSent: now.Add(-25 * time.Second),
		End:          now,
	}.Durations()
	if d.Eval != 0 {
		t.Fatalf("eval = %d, ожидалось 0 (нет первого дельта-чанка)", d.Eval)
	}
	if d.PromptEval != int64(25*time.Second) {
		t.Fatalf("prompt_eval = %d, ожидалось 25 с", d.PromptEval)
	}
	if d.Total != int64(30*time.Second) {
		t.Fatalf("total = %d, ожидалось 30 с", d.Total)
	}
}

// TestR493_WindowsNoRequestStart — совсем без начала запроса: честные нули.
func TestR493_WindowsNoRequestStart(t *testing.T) {
	d := statsWindowsR493{End: time.Now()}.Durations()
	if d != (statsDurationsR493{}) {
		t.Fatalf("ожидались нули без RequestStart, получено %+v", d)
	}
}

// TestR493_WindowsClockSkew — часы скакнули назад: отрицательных длительностей
// в ответе клиенту быть не должно, и сумма не превышает total.
func TestR493_WindowsClockSkew(t *testing.T) {
	now := time.Now()
	d := statsWindowsR493{
		RequestStart: now,
		UpstreamSent: now.Add(-5 * time.Second), // раньше начала запроса
		FirstDelta:   now.Add(-2 * time.Second),
		End:          now,
	}.Durations()
	if d.Load < 0 || d.PromptEval < 0 || d.Eval < 0 || d.Total < 0 {
		t.Fatalf("отрицательные длительности: %+v", d)
	}
	if d.Load+d.PromptEval+d.Eval > d.Total {
		t.Fatalf("сумма отрезков больше total: %+v", d)
	}
}

// TestR493_ChunkHasDelta — что считается «первым выводом».
func TestR493_ChunkHasDelta(t *testing.T) {
	cases := []struct {
		name  string
		chunk map[string]interface{}
		want  bool
	}{
		{
			name:  "content",
			chunk: map[string]interface{}{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{"content": "hi"}}}},
			want:  true,
		},
		{
			name:  "reasoning_content",
			chunk: map[string]interface{}{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{"reasoning_content": "думаю"}}}},
			want:  true,
		},
		{
			name:  "thinking",
			chunk: map[string]interface{}{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{"thinking": "думаю"}}}},
			want:  true,
		},
		{
			name:  "tool_calls",
			chunk: map[string]interface{}{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{"id": "c1"}}}}}},
			want:  true,
		},
		{
			name:  "completions text",
			chunk: map[string]interface{}{"choices": []interface{}{map[string]interface{}{"text": "hi"}}},
			want:  true,
		},
		{
			name:  "native content",
			chunk: map[string]interface{}{"message": map[string]interface{}{"content": "hi"}, "done": false},
			want:  true,
		},
		{
			name:  "native thinking",
			chunk: map[string]interface{}{"message": map[string]interface{}{"thinking": "думаю"}, "done": false},
			want:  true,
		},
		{
			name:  "native response",
			chunk: map[string]interface{}{"response": "hi", "done": false},
			want:  true,
		},
		{
			name:  "role only (служебный чанк)",
			chunk: map[string]interface{}{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{"role": "assistant"}}}},
			want:  false,
		},
		{
			name:  "пустая delta + finish_reason",
			chunk: map[string]interface{}{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{}, "finish_reason": "stop"}}},
			want:  false,
		},
		{
			name:  "heartbeat с пустым content",
			chunk: map[string]interface{}{"message": map[string]interface{}{"content": ""}},
			want:  false,
		},
		{
			name:  "usage-only",
			chunk: map[string]interface{}{"usage": map[string]interface{}{"prompt_tokens": 1.0}, "choices": []interface{}{}},
			want:  false,
		},
		{
			name:  "пусто",
			chunk: map[string]interface{}{},
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chunkHasStatsDeltaR493(tc.chunk); got != tc.want {
				t.Fatalf("chunkHasStatsDeltaR493 = %v, ожидалось %v", got, tc.want)
			}
		})
	}
}

// TestR493_FirstDeltaPrefersAnyDelta — при наличии обоих моментов берётся первый
// ЛЮБОЙ дельта-чанк: именно он и есть начало генерации.
func TestR493_FirstDeltaPrefersAnyDelta(t *testing.T) {
	now := time.Now()
	reasoning := now.Add(-60 * time.Second)
	content := now.Add(-5 * time.Second)
	if got := firstDeltaOrContentR493(reasoning, content); !got.Equal(reasoning) {
		t.Fatalf("выбран %v, ожидался первый дельта-чанк (размышление) %v", got, reasoning)
	}
	if got := firstDeltaOrContentR493(time.Time{}, content); !got.Equal(content) {
		t.Fatalf("без дельта-чанка должен браться первый контент, получено %v", got)
	}
}

// TestR493_TransportWindowsUseAnyDelta — ГЛАВНЫЙ сторож 12a на уровне сборки
// окон: если модель думала 65 с и только потом выдала 5 с ответа, окно
// генерации обязано начинаться с размышления. Именно эта граница и была
// дефектом (первый CONTENT-чанк), и именно её мутация должна ронять тест.
func TestR493_TransportWindowsUseAnyDelta(t *testing.T) {
	now := time.Now()
	w := statsWindowsForTransportR493(
		now.Add(-70*time.Second), // балансер начал
		now.Add(-70*time.Second), // сразу отправил воркеру
		now.Add(-65*time.Second), // первый дельта-чанк — РАЗМЫШЛЕНИЕ
		now.Add(-5*time.Second),  // первый ВИДИМЫЙ контент
		now,
	)
	d := w.Durations()
	if d.Eval < int64(60*time.Second) {
		t.Fatalf("eval = %d нс: граница генерации снова взята по видимому контенту "+
			"(5 с), и время размышлений уехало в prompt_eval (%d нс) — ровно дефект 12a",
			d.Eval, d.PromptEval)
	}
	// Префилл — ровно отрезок «отправка → первое размышление» (5 с), а не 70 с
	// и не 65 с: ни ожидание, ни время размышлений в него не входят.
	if d.PromptEval != int64(5*time.Second) {
		t.Fatalf("prompt_eval = %d нс, ожидалось 5 с (отправка → первое размышление)",
			d.PromptEval)
	}
	if d.Load != 0 {
		t.Fatalf("load = %d нс, ожидалось 0 (воркер принял запрос сразу)", d.Load)
	}
}
