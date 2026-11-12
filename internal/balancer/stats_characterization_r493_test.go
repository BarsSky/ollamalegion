// stats_characterization_r493_test.go — ПРОВЕРКА РАСЧЁТА СКОРОСТИ ТОКЕНОВ
// (по просьбе оператора 2026-11-12: «агент получает значения очень сильно
// отличающиеся от действительности»).
//
// ЧТО ЭТО ЗА ТЕСТ. Это ХАРАКТЕРИЗУЮЩИЙ тест: он фиксирует, как статы считаются
// СЕЙЧАС, и явно называет два места, где результат не соответствует реальности.
// Он зелёный ровно потому, что фиксирует текущее поведение, — и обязан измениться
// вместе с исправлением. Такой тест нужен затем, чтобы правку нельзя было сделать
// «молча»: любое изменение формул видно здесь числом.
//
// НАЙДЕННЫЕ ДЕФЕКТЫ (разбор: docs/architecture/stats-and-context-check.ru.md):
//
//  1. В prompt_eval_duration попадает ВРЕМЯ ОЖИДАНИЯ, а не только префилл.
//     В llamacpp_transport.go статы считаются от llamaStartTime (строка 111) —
//     это время ДО preflight n_ctx, а preflight умеет синхронно перезагружать
//     модель (минуты на 27B). В результате «скорость промпта» = prompt_tokens /
//     (очередь + загрузка + префилл) — на 27B это занижение в десятки раз.
//
//  2. В eval_duration не попадает время РАЗМЫШЛЕНИЙ, хотя их токены входят в
//     eval_count. firstContentTime ставится на первом ЧАНКЕ CONTENT
//     (llamacpp_transport.go:1135, 1454), а reasoning-дельты его не двигают.
//     Значит prompt_eval_duration поглощает всё время размышлений, а eval_count
//     содержит и их токены → tokens_per_second завышается (в тесте ниже —
//     500 токенов за 5 секунд видимого контента = 100 t/s).
//
// ПОЧЕМУ НЕ ИСПРАВЛЕНО ЗДЕСЬ ЖЕ: правка меняет то, что видит КАЖДЫЙ клиент
// (OpenWebUI, Cline, панель). По правилу оператора («сначала причина, потом
// правка, и чтобы не убила остальное») сначала нужен архив со стенда: подтвердить,
// что агент читает именно эти поля и что смещение объясняется очередью/загрузкой,
// а не другим источником. Дефекты внесены в реестр (debts.ru.md 12a).
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

// TestR493_StatsIncludeQueueAndPreflight — дефект 1, зафиксирован числом.
//
// Сценарий живой: запрос ждал в очереди и/или модель грузилась 90 с, затем
// префилл 5 с и генерация 5 с. Клиент получает prompt_eval_duration ≈ 95 с,
// хотя префилл занял 5 с.
func TestR493_StatsIncludeQueueAndPreflight(t *testing.T) {
	now := time.Now()
	streamStart := now.Add(-100 * time.Second) // начало обработки запроса
	firstContent := now.Add(-5 * time.Second)  // первый видимый контент
	usage := map[string]interface{}{"prompt_tokens": 2000.0, "completion_tokens": 400.0, "total_tokens": 2400.0}

	chunk := translateUsageChunkToOllama("/api/chat", "m", usage, map[string]interface{}{},
		streamStart, firstContent, "answer", true)
	s := statsFromUsageR493(t, chunk)

	promptNs := s["prompt_eval_duration"]
	totalNs := s["total_duration"]
	evalNs := s["eval_duration"]

	// ХАРАКТЕРИСТИКА текущего поведения: в «время промпта» входит ожидание.
	if promptNs < 90e9 {
		t.Fatalf("prompt_eval_duration = %.0f нс, ожидалось ≈95 с: тест фиксирует, что "+
			"ожидание/загрузка попадают в эту величину; если формулу исправили — "+
			"обновите ожидание и docs/architecture/stats-and-context-check.ru.md", promptNs)
	}
	if totalNs < 99e9 {
		t.Fatalf("total_duration = %.0f нс, ожидалось ≈100 с", totalNs)
	}
	if evalNs < 4.9e9 || evalNs > 5.1e9 {
		t.Fatalf("eval_duration = %.0f нс, ожидалось ≈5 с", evalNs)
	}
	// Скорость промпта, которую увидит клиент: 2000 токенов за 95 с = 21 t/s,
	// при реальном префилле 5 с это было бы 400 t/s — занижение в 19 раз.
	if got := s["tokens_per_second"]; got > 0 {
		t.Logf("tokens_per_second = %.1f (это только генерация, без размышлений)", got)
	}
}

// TestR493_StatsDilutePromptSpeed — то же, но проверяем величину искажения.
func TestR493_StatsDilutePromptSpeed(t *testing.T) {
	now := time.Now()
	streamStart := now.Add(-120 * time.Second)
	firstContent := now.Add(-20 * time.Second)
	usage := map[string]interface{}{"prompt_tokens": 4000.0, "completion_tokens": 200.0, "total_tokens": 4200.0}

	chunk := translateUsageChunkToOllama("/api/chat", "m", usage, map[string]interface{}{},
		streamStart, firstContent, "answer", true)
	s := statsFromUsageR493(t, chunk)

	reportedPromptTPS := 4000.0 / (s["prompt_eval_duration"] / 1e9)
	// Реальный префилл — это первые ~20 секунд (в них же сидит ожидание); если бы
	// ожидания не было, скорость считалась бы по меньшему окну и была бы выше.
	if reportedPromptTPS > 100 {
		t.Fatalf("prompt t/s = %.1f — ожидалось сильно заниженное значение; "+
			"похоже, формулу исправили: обновите тест и документ", reportedPromptTPS)
	}
	t.Logf("клиент видит prompt t/s = %.1f (реально было бы выше)", reportedPromptTPS)
}

// TestR493_StatsReasoningTimeInflatesCompletionSpeed — дефект 2, зафиксирован числом.
//
// Модель думала 60 с (500 токенов размышлений), затем 5 с видимого ответа (20
// токенов). completion_tokens = 520. firstContentTime — момент первого ВИДИМОГО
// контента, поэтому eval_duration = 5 с, а в токенах — все 520:
// tokens_per_second = 104 t/s вместо реальных ~8 t/s.
func TestR493_StatsReasoningTimeInflatesCompletionSpeed(t *testing.T) {
	now := time.Now()
	streamStart := now.Add(-65 * time.Second)
	firstContent := now.Add(-5 * time.Second) // первый ВИДИМЫЙ контент после размышлений
	usage := map[string]interface{}{"prompt_tokens": 100.0, "completion_tokens": 520.0, "total_tokens": 620.0}

	chunk := translateUsageChunkToOllama("/api/chat", "m", usage, map[string]interface{}{},
		streamStart, firstContent, "answer", true)
	s := statsFromUsageR493(t, chunk)

	tps := s["tokens_per_second"]
	if tps < 90 {
		t.Fatalf("tokens_per_second = %.1f, ожидалось ≈104 (500 рассуждений + 20 ответа за "+
			"5 с видимого контента). Если размышления стали учитываться в eval_duration — "+
			"обновите тест и документ", tps)
	}
	realTPS := 520.0 / 65.0 // весь вывод за всё время генерации
	t.Logf("клиент видит %.0f t/s, по всему времени вывода — %.1f t/s (искажение в %.1f раза)",
		tps, realTPS, tps/realTPS)
}
