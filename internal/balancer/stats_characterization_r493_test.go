// stats_characterization_r493_test.go — ПРОВЕРКА РАСЧЁТА СКОРОСТИ ТОКЕНОВ
// (по просьбе оператора 2026-11-12: «агент получает значения очень сильно
// отличающиеся от действительности»).
//
// ИСТОРИЯ. Этот файл начинался как ХАРАКТЕРИЗУЮЩИЙ: он фиксировал, как статы
// считаются СЕЙЧАС, и называл числами два дефекта:
//
//  1. в prompt_eval_duration входило ОЖИДАНИЕ (очередь, авто-загрузка, preflight
//     n_ctx), потому что окна считались от llamaStartTime — начала обработки
//     запроса балансером: 2000 токенов за 95 с = 21 t/s вместо ~400;
//  2. в eval_duration не входило время РАЗМЫШЛЕНИЙ: первый контент-чанк ставился
//     только на чанке content, а reasoning-дельты его не двигали, хотя их токены
//     входят в eval_count: 520 токенов за 5 с = 104 t/s вместо ~8.
//
// СЕЙЧАС ФИКСИРУЕТ ИСПРАВЛЕННОЕ поведение (R493-12a, stats_windows_r493.go):
// у каждого отрезка свой источник, и тест падает, если кто-то вернёт старую
// формулу «одно окно на всё». Исторические числа оставлены в комментариях —
// чтобы было видно, ЧТО именно изменилось, а не только «стало зелено».
package balancer

import (
	"encoding/json"
	"testing"
	"time"
)

// statsFromUsageR493 — разобрать чанк, который ушёл клиенту, в числа.
func statsFromUsageR493(t *testing.T, chunk []byte) map[string]float64 {
	t.Helper()
	if len(chunk) == 0 {
		t.Fatalf("транслятор не вернул чанк — статы потеряны")
	}
	var m map[string]interface{}
	if err := json.Unmarshal(chunk, &m); err != nil {
		t.Fatalf("чанк не JSON: %v (%q)", err, string(chunk))
	}
	out := make(map[string]float64, len(m))
	for k, v := range m {
		if f, ok := v.(float64); ok {
			out[k] = f
		}
	}
	return out
}

// TestR493_StatsQueueAndLoadOutOfPromptWindow — дефект 1 закрыт.
//
// Живой сценарий: 5 с в очереди/на загрузку, 5 с префилл, 10 с генерация.
// ДО: prompt_eval_duration ≈ 95 с (ожидание внутри «промпта»).
// ПОСЛЕ: load_duration = 5 с, prompt_eval_duration = 5 с, eval = 10 с.
func TestR493_StatsQueueAndLoadOutOfPromptWindow(t *testing.T) {
	now := time.Now()
	w := statsWindowsR493{
		RequestStart: now.Add(-20 * time.Second), // балансер начал обработку
		UpstreamSent: now.Add(-15 * time.Second), // запрос ушёл воркеру (5 с очередь/загрузка)
		FirstDelta:   now.Add(-10 * time.Second), // конец префилла (5 с)
		End:          now,                        // конец генерации (10 с)
	}
	usage := map[string]interface{}{"prompt_tokens": 2000.0, "completion_tokens": 400.0, "total_tokens": 2400.0}

	chunk := translateUsageChunkToOllamaWindowsR493("/api/chat", "m", usage, map[string]interface{}{},
		w, "answer", true)
	s := statsFromUsageR493(t, chunk)

	approx := func(name string, got, want float64) {
		t.Helper()
		if got < want-0.2e9 || got > want+0.2e9 {
			t.Fatalf("%s = %.0f нс, ожидалось ≈%.0f нс", name, got, want)
		}
	}
	approx("total_duration", s["total_duration"], 20e9)
	approx("load_duration", s["load_duration"], 5e9)
	approx("prompt_eval_duration", s["prompt_eval_duration"], 5e9)
	approx("eval_duration", s["eval_duration"], 10e9)

	// Скорость промпта теперь считается по СВОЕМУ окну: 2000/5 = 400 t/s.
	// Клиент сам делит prompt_eval_count на prompt_eval_duration; проверяем окно.
	if reported := 2000.0 / (s["prompt_eval_duration"] / 1e9); reported < 300 {
		t.Fatalf("prompt t/s по отданному окну = %.0f, ожидалось ≈400 (старое поведение "+
			"давало 21 — в окно попадало ожидание)", reported)
	}
	// tokens_per_second — только генерация: 400/10 = 40 t/s.
	approx("tokens_per_second", s["tokens_per_second"], 40)
}

// TestR493_StatsReasoningTimeCountedInEval — дефект 2 закрыт.
//
// Модель думала 60 с (500 токенов размышлений), затем 5 с видимого ответа
// (20 токенов). completion_tokens = 520.
// ДО: eval_duration = 5 с → 104 t/s.
// ПОСЛЕ: eval_duration = 65 с → ≈8 t/s (время размышлений — это генерация).
func TestR493_StatsReasoningTimeCountedInEval(t *testing.T) {
	now := time.Now()
	w := statsWindowsR493{
		RequestStart: now.Add(-65 * time.Second),
		UpstreamSent: now.Add(-65 * time.Second),
		// Первый дельта-чанк — РАЗМЫШЛЕНИЕ (не content): именно это и было слепым
		// пятном старого кода.
		FirstDelta: now.Add(-65 * time.Second),
		End:        now,
	}
	usage := map[string]interface{}{"prompt_tokens": 100.0, "completion_tokens": 520.0, "total_tokens": 620.0}

	chunk := translateUsageChunkToOllamaWindowsR493("/api/chat", "m", usage, map[string]interface{}{},
		w, "answer", true)
	s := statsFromUsageR493(t, chunk)

	tps := s["tokens_per_second"]
	if tps > 10 || tps < 7 {
		t.Fatalf("tokens_per_second = %.1f, ожидалось ≈8 (520 токенов за 65 с). "+
			"104 означало бы, что время размышлений снова выпало из eval_duration", tps)
	}
	if evalNs := s["eval_duration"]; evalNs < 64e9 {
		t.Fatalf("eval_duration = %.0f нс, ожидалось ≈65 с (размышления внутри генерации)", evalNs)
	}
}

// TestR493_StatsSegmentsSumToTotal — сумма трёх отрезков равна total_duration.
// Клиент показывает их рядом; расхождение и есть «скорости не совпадают».
func TestR493_StatsSegmentsSumToTotal(t *testing.T) {
	now := time.Now()
	w := statsWindowsR493{
		RequestStart: now.Add(-100 * time.Second),
		UpstreamSent: now.Add(-90 * time.Second),
		FirstDelta:   now.Add(-80 * time.Second),
		End:          now,
	}
	usage := map[string]interface{}{"prompt_tokens": 1000.0, "completion_tokens": 200.0}
	s := statsFromUsageR493(t, translateUsageChunkToOllamaWindowsR493("/api/chat", "m", usage,
		map[string]interface{}{}, w, "x", true))

	sum := s["load_duration"] + s["prompt_eval_duration"] + s["eval_duration"]
	if diff := s["total_duration"] - sum; diff > 1e6 || diff < -1e6 {
		t.Fatalf("load+prompt+eval = %.0f, total = %.0f (расхождение %.0f нс)",
			sum, s["total_duration"], diff)
	}
}
