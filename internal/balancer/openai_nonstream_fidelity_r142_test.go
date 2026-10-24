package balancer

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// R142 (2026-10-16): достоверность собранного non-stream ответа на OpenAI-пути.
//
// Режим LB_OPENAI_AUTO_STREAM=true (на стенде включён) означает, что апстрим
// отвечает ЧАНКАМИ даже на stream:false, а балансер склеивает их в один JSON.
// В этой склейке терялось/портилось два поля:
//
//   - object: последний чанк перезатирал "chat.completion" на
//     "chat.completion.chunk" — клиент получал не-стриминговый ответ, помеченный
//     как стриминговый чанк (строгие OpenAI-SDK выбирают разбор по object);
//   - duration_ms: у чанков его нет, поэтому поле исчезало совсем, и клиент
//     считал tokens/s по СВОИМ часам — а в них попадает ожидание перезагрузки
//     модели (живой замер: 65 с на запросе во время reload) и очередь. Отсюда
//     «нереальный расчёт токенов/с» при работающей генерации.
func TestOpenAIAccumulatorNonStreamFidelityR142(t *testing.T) {
	a := newOpenAIStreamAccumulator("gemma-4")
	a.addChunk(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1234,"model":"gemma-4","choices":[{"index":0,"delta":{"role":"assistant","content":"ок"},"finish_reason":null}]}`)
	a.addChunk(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1234,"model":"gemma-4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":19,"completion_tokens":1,"total_tokens":20}}`)
	time.Sleep(2 * time.Millisecond)

	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(a.toNonStreamResponse(false), &resp))

	assert.Equal(t, "chat.completion", resp["object"],
		"не-стриминговый ответ не должен быть помечен как стриминговый чанк")

	d, ok := resp["duration_ms"]
	assert.True(t, ok,
		"duration_ms обязан доехать до клиента: иначе он считает tokens/s по своим часам, куда попадает ожидание перезагрузки модели")
	if ok {
		ms, isNum := d.(float64)
		assert.True(t, isNum, "duration_ms должен быть числом, получено %T", d)
		assert.GreaterOrEqual(t, ms, float64(0))
	}

	// Смысл числа: это окно «отправка апстрим-запроса → последний чанк».
	// Проверяем на явно заданной точке отсчёта, иначе тест ловит только
	// присутствие поля и пропускает возврат нуля (живой дефект: 0 против 2033 мс
	// у воркера — аккумулятор создавался уже после прихода тела).
	b := newOpenAIStreamAccumulator("gemma-4")
	b.startedAt = time.Now().Add(-1500 * time.Millisecond)
	b.addChunk(`{"choices":[{"delta":{"content":"ок"},"finish_reason":"stop"}]}`)
	var respB map[string]interface{}
	assert.NoError(t, json.Unmarshal(b.toNonStreamResponse(false), &respB))
	msB, _ := respB["duration_ms"].(float64)
	assert.GreaterOrEqual(t, msB, float64(1400),
		"duration_ms должен отражать переданную точку отсчёта, а не момент склейки")
	assert.LessOrEqual(t, msB, float64(3000))

	// Склейка не должна пострадать от правки: content, finish_reason и usage на месте.
	choice := resp["choices"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, "stop", choice["finish_reason"])
	assert.Equal(t, "ок", choice["message"].(map[string]interface{})["content"])
	assert.Equal(t, float64(19), resp["usage"].(map[string]interface{})["prompt_tokens"])
}
