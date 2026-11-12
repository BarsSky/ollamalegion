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
		{
			needle: "applyStatsWindowsNativeR493(",
			count:  1,
			why: "нативный done-кадр обязан дополняться окнами балансера: без этого " +
				"клиент снова получит total_duration без времени загрузки модели " +
				"(живой замер: ждал 62 с, видел 2.15 с)",
		},
		{
			needle: `resp.Header.Get("X-Cppworker-Load-Duration-Ms")`,
			count:  1,
			why: "загрузку модели видит только воркер (она внутри запроса, после " +
				"client.Do). Без чтения его заголовка 40+ секунд снова не попадут " +
				"ни в один отрезок — это третий живой замер",
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
	// Старт «сколько ждал клиент» обязан брать момент ПРИЁМА запроса: llamaStartTime
	// ставится уже после очереди и авто-загрузки, и ожидание из него не видно.
	if !strings.Contains(src, "requestStartR493(r.Context())") {
		t.Fatal("транспорт не читает момент приёма запроса из контекста: ожидание " +
			"до отправки воркеру снова не попадёт в load_duration")
	}
	proxySrc, err := os.ReadFile("proxy.go")
	if err != nil {
		t.Fatalf("не прочитать proxy.go: %v", err)
	}
	if !strings.Contains(string(proxySrc), "withRequestStartR493(r.Context(), startTime)") {
		t.Fatal("Proxy.ServeHTTP не отмечает момент приёма запроса: транспорту нечего читать")
	}
}
