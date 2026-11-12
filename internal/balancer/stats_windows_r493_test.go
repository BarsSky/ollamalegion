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
	"context"
	"encoding/json"
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

// TestR493_WindowsWorkerLoadMovedOutOfPrefill — загрузка, которую видел воркер,
// переносится из «промпта» в «загрузку», а не добавляется сверху.
//
// Живой случай: клиент ждал 44 с, из них 40 с — ленивая загрузка модели внутри
// запроса к воркеру. Без переноса сумма отрезков превысила бы total, и либо
// инвариант ломался, либо скорость генерации считалась по чужому окну.
func TestR493_WindowsWorkerLoadMovedOutOfPrefill(t *testing.T) {
	now := time.Now()
	w := statsWindowsR493{
		RequestStart: now.Add(-4 * time.Second), // начало обработки (очередь 2 с)
		UpstreamSent: now.Add(-2 * time.Second), // отправка воркеру
		FirstDelta:   now.Add(time.Second),      // через 3 с после отправки
		End:          now.Add(3 * time.Second),
		WorkerLoad:   int64(3 * time.Second), // из этих 3 с — загрузка модели воркером
	}
	d := w.Durations()
	if d.Load != int64(5*time.Second) {
		t.Fatalf("load = %d, ожидалось 5 с (2 с очереди + 3 с загрузки воркера)", d.Load)
	}
	if d.PromptEval != 0 {
		t.Fatalf("prompt_eval = %d, ожидалось 0: всё окно ушло на загрузку", d.PromptEval)
	}
	if d.Eval != int64(2*time.Second) {
		t.Fatalf("eval = %d, ожидалось 2 с", d.Eval)
	}
	if sum := d.Load + d.PromptEval + d.Eval; sum != d.Total {
		t.Fatalf("сумма отрезков %d != total %d", sum, d.Total)
	}
}

// TestR493_WindowsWorkerLoadPartialPrefill — загрузка меньше окна: остаток окна
// остаётся префиллом.
func TestR493_WindowsWorkerLoadPartialPrefill(t *testing.T) {
	now := time.Now()
	d := statsWindowsR493{
		RequestStart: now.Add(-10 * time.Second),
		UpstreamSent: now.Add(-10 * time.Second),
		FirstDelta:   now.Add(-4 * time.Second), // 6 с до первого дельта-чанка
		End:          now,
		WorkerLoad:   int64(5 * time.Second), // 5 с из них — загрузка
	}.Durations()
	if d.Load != int64(5*time.Second) {
		t.Fatalf("load = %d, ожидалось 5 с", d.Load)
	}
	if d.PromptEval != int64(1*time.Second) {
		t.Fatalf("prompt_eval = %d, ожидалось 1 с (6 с окна − 5 с загрузки)", d.PromptEval)
	}
	if d.Eval != int64(4*time.Second) {
		t.Fatalf("eval = %d, ожидалось 4 с", d.Eval)
	}
}

// TestR493_RequestStartTravelsInContext — момент приёма запроса ставит вход
// (Proxy.ServeHTTP), а читает транспорт: без этого ожидание до отправки воркеру
// (очередь, авто-загрузка, ожидание слота) не попадает ни в один отрезок.
func TestR493_RequestStartTravelsInContext(t *testing.T) {
	start := time.Now().Add(-30 * time.Second)
	ctx := withRequestStartR493(context.Background(), start)
	if got := requestStartR493(ctx); !got.Equal(start) {
		t.Fatalf("requestStartR493 = %v, ожидалось %v", got, start)
	}
	// Второй вызов не сдвигает начало: вложенные обёртки не должны «омолаживать»
	// момент приёма, иначе ожидание снова потеряется.
	if got := requestStartR493(withRequestStartR493(ctx, time.Now())); !got.Equal(start) {
		t.Fatalf("повторная установка сдвинула начало: %v", got)
	}
	if got := requestStartR493(context.Background()); !got.IsZero() {
		t.Fatalf("без значения ожидался zero, получено %v", got)
	}
	if got := requestStartR493(nil); !got.IsZero() {
		t.Fatalf("nil-контекст: ожидался zero, получено %v", got)
	}
}

// TestR493_WindowsFromAcceptToFirstDelta — сквозной смысл: клиент ждал 30 с до
// отправки воркеру (очередь + загрузка узла), потом 5 с до первого токена.
// «Промпт» обязан быть 5 с, «загрузка» — 30 с.
func TestR493_WindowsFromAcceptToFirstDelta(t *testing.T) {
	now := time.Now()
	d := statsWindowsForTransportR493(
		now.Add(-40*time.Second), // балансер принял запрос
		now.Add(-10*time.Second), // отправил воркеру (30 с ожидания)
		now.Add(-5*time.Second),  // первый дельта-чанк (5 с префилла)
		now.Add(-5*time.Second),
		now,
	).Durations()
	if d.Load != int64(30*time.Second) {
		t.Fatalf("load = %d, ожидалось 30 с (очередь + авто-загрузка)", d.Load)
	}
	if d.PromptEval != int64(5*time.Second) {
		t.Fatalf("prompt_eval = %d, ожидалось 5 с", d.PromptEval)
	}
	if d.Eval != int64(5*time.Second) {
		t.Fatalf("eval = %d, ожидалось 5 с", d.Eval)
	}
	if d.Total != int64(40*time.Second) {
		t.Fatalf("total = %d, ожидалось 40 с", d.Total)
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

// TestR493_NativeDoneFrameGetsLoadDuration — живой случай стенда: модель
// грузилась 60 с, воркер отдал свои 2.15 с и ни слова про ожидание.
func TestR493_NativeDoneFrameGetsLoadDuration(t *testing.T) {
	now := time.Now()
	w := statsWindowsR493{RequestStart: now.Add(-62 * time.Second), UpstreamSent: now.Add(-2 * time.Second)}
	// Ровно то, что отдал воркер (замер со стенда 2026-11-12).
	frame := `{"model":"Qwen3-4B","done":true,"done_reason":"stop",` +
		`"prompt_eval_count":30,"eval_count":80,` +
		`"prompt_eval_duration":740000000,"eval_duration":1410000000,"total_duration":2150000000,` +
		`"message":{"role":"assistant","content":"ok"}}`

	out, changed := applyStatsWindowsNativeR493(frame, w)
	if !changed {
		t.Fatal("нативный done-кадр не тронут: клиент снова не узнает, что ждал 60 с")
	}
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("кадр перестал быть JSON: %v (%s)", err, out)
	}
	load := got["load_duration"].(float64)
	if load < 59e9 || load > 61e9 {
		t.Fatalf("load_duration = %.0f нс, ожидалось ≈60 с (ожидание + загрузка)", load)
	}
	prompt := got["prompt_eval_duration"].(float64)
	eval := got["eval_duration"].(float64)
	total := got["total_duration"].(float64)
	if prompt != 740000000 || eval != 1410000000 {
		t.Fatalf("отрезки воркера изменены: prompt=%.0f eval=%.0f (должны остаться его)", prompt, eval)
	}
	if total != load+prompt+eval {
		t.Fatalf("total=%.0f != load+prompt+eval=%.0f — инвариант нарушен", total, load+prompt+eval)
	}
	tps, ok := got["tokens_per_second"].(float64)
	if !ok {
		t.Fatal("tokens_per_second отсутствует — панель покажет N/A при непустом eval_count")
	}
	if want := 80.0 / 1.41; tps < want-0.2 || tps > want+0.2 {
		t.Fatalf("tokens_per_second = %.2f, ожидалось ≈%.2f", tps, want)
	}
	// Содержимое и структура кадра не должны пострадать.
	msg, _ := got["message"].(map[string]interface{})
	if msg == nil || msg["content"] != "ok" {
		t.Fatalf("message.content потерян при правке: %s", out)
	}
}

// TestR493_NativeDoneFrameAddsWorkerLoadDuration — 40 секунд ленивой загрузки
// видит ТОЛЬКО воркер (загрузка идёт внутри запроса, после client.Do):
// балансер обязан сложить их со своим ожиданием, а не перезаписать нулём.
func TestR493_NativeDoneFrameAddsWorkerLoadDuration(t *testing.T) {
	now := time.Now()
	// Балансер ждал 2 с (очередь/preflight), воркер грузил модель 40 с.
	w := statsWindowsR493{RequestStart: now.Add(-2 * time.Second), UpstreamSent: now}
	frame := `{"done":true,"eval_count":60,"prompt_eval_duration":140000000,` +
		`"eval_duration":1230000000,"total_duration":1370000000,` +
		`"load_duration":40000000000,"message":{"content":"ok"}}`
	out, changed := applyStatsWindowsNativeR493(frame, w)
	if !changed {
		t.Fatal("кадр не дополнен: ожидание балансера и загрузка воркера не сошлись")
	}
	var got map[string]interface{}
	_ = json.Unmarshal([]byte(out), &got)
	load := got["load_duration"].(float64)
	if load < 41e9 || load > 42e9 {
		t.Fatalf("load_duration = %.0f нс, ожидалось ≈42 с (2 с ожидания + 40 с загрузки), "+
			"а не только ожидание балансера", load)
	}
	prompt := got["prompt_eval_duration"].(float64)
	eval := got["eval_duration"].(float64)
	if total := got["total_duration"].(float64); total != load+prompt+eval {
		t.Fatalf("total=%.0f != load+prompt+eval=%.0f", total, load+prompt+eval)
	}
}

// TestR493_NativeDoneFrameUntouchedWithoutWindows — балансер не видел отправки:
// кадр воркера остаётся как есть (не выдумываем время).
func TestR493_NativeDoneFrameUntouchedWithoutWindows(t *testing.T) {
	frame := `{"done":true,"eval_count":5,"eval_duration":1000000000}`
	if out, changed := applyStatsWindowsNativeR493(frame, statsWindowsR493{}); changed || out != frame {
		t.Fatalf("кадр изменён без окон: %s", out)
	}
}

// TestR493_NativeStreamFrameUntouched — не-done кадры не трогаем вовсе.
func TestR493_NativeStreamFrameUntouched(t *testing.T) {
	now := time.Now()
	w := statsWindowsR493{RequestStart: now.Add(-5 * time.Second), UpstreamSent: now.Add(-1 * time.Second)}
	frame := `{"done":false,"message":{"role":"assistant","content":"привет"}}`
	if out, changed := applyStatsWindowsNativeR493(frame, w); changed || out != frame {
		t.Fatalf("промежуточный кадр переписан: %s", out)
	}
}

// TestR493_NativeDoneFrameWithoutWorkerDurations — старая сборка воркера не
// присылает длительностей: считаем нашими окнами, форму не выдумываем.
func TestR493_NativeDoneFrameWithoutWorkerDurations(t *testing.T) {
	now := time.Now()
	w := statsWindowsR493{
		RequestStart: now.Add(-30 * time.Second),
		UpstreamSent: now.Add(-25 * time.Second),
		FirstDelta:   now.Add(-20 * time.Second),
		End:          now,
	}
	out, changed := applyStatsWindowsNativeR493(`{"done":true,"eval_count":100}`, w)
	if !changed {
		t.Fatal("кадр без длительностей не дополнен нашими окнами")
	}
	var got map[string]interface{}
	_ = json.Unmarshal([]byte(out), &got)
	if got["load_duration"].(float64) != float64(5*time.Second) {
		t.Fatalf("load_duration = %.0f, ожидалось 5 с", got["load_duration"])
	}
	if got["eval_duration"].(float64) != float64(20*time.Second) {
		t.Fatalf("eval_duration = %.0f, ожидалось 20 с", got["eval_duration"])
	}
}

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
