// stats_windows_wiring_r493_test.go — R493-12a: СТОРОЖ ПОДКЛЮЧЕНИЯ.
//
// ЗАЧЕМ. Формула в stats_windows_r493.go может быть идеальной, но если транспорт
// не передаёт окна (или снова ставит первый момент только на content-чанк), клиент
// получит прежние неверные числа. Юнит-тест на чистую функцию такую регрессию НЕ
// поймает: он подаёт окна сам.
//
// Поэтому здесь проверяется именно ПОДКЛЮЧЕНИЕ в llamacpp_transport.go — теми же
// приёмами, что steward-тесты стадий (stage_steward_r477_test.go) и проверка
// transport-only путей (readonly_no_inference_r118_test.go): читаем исходник и
// требуем, чтобы ключевые вызовы были на месте. Мутация (вернуть старую формулу
// окон) роняет этот тест.
package balancer

import (
	"os"
	"strings"
	"testing"
)

func TestR493_TransportPassesStatsWindows(t *testing.T) {
	const path = "llamacpp_transport.go"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("не прочитать %s: %v", path, err)
	}
	src := string(raw)

	required := []struct {
		needle string
		count  int
		why    string
	}{
		{
			needle: "upstreamSentTime = time.Now()",
			count:  1,
			why: "момент отправки воркеру обязан фиксироваться: без него очередь, " +
				"авто-загрузка и preflight снова попадут в «время промпта»",
		},
		{
			needle: "chunkHasStatsDeltaR493(",
			count:  2,
			why: "первый дельта-чанк ЛЮБОГО вида обязан фиксироваться на ОБОИХ " +
				"путях (нативный NDJSON и OpenAI SSE): только content-чанк " +
				"оставлял время размышлений в «времени промпта»",
		},
		{
			needle: "translateOpenAISSEDataToOllamaWindowsR493(",
			count:  1,
			why: "транспорт обязан звать перевод статистики с ЧЕТЫРЬМЯ окнами, " +
				"иначе load_duration снова станет нулём, а total — единственным окном",
		},
		{
			needle: "statsWindowsForTransportR493(",
			count:  1,
			why: "окна должны собираться одной функцией с правилом «первый дельта-чанк " +
				"любого вида»: собранные вручную в транспорте они уже один раз " +
				"потеряли размышления (и это не ловилось тестом)",
		},
	}
	for _, r := range required {
		if got := strings.Count(src, r.needle); got != r.count {
			t.Fatalf("%q встречается %d раз, ожидалось %d: %s", r.needle, got, r.count, r.why)
		}
	}
	// Обратная проверка: старой формы вызова (два времени подряд) быть не должно.
	if strings.Contains(src, "translateOpenAISSEDataToOllamaWithFlags(") {
		t.Fatal("транспорт снова зовёт translateOpenAISSEDataToOllamaWithFlags (две " +
			"точки времени вместо четырёх): ожидание и загрузка вернутся в prompt_eval_duration")
	}
}
