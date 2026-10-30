// native_done_hygiene_r321.go — R321 (2026-10-30): контрактное завершение
// нативного NDJSON-стрима (/api/chat, /api/generate).
//
// ЖИВАЯ ЖАЛОБА (Cline через балансер). Поток обрывается «прямо посреди
// рассуждений»: модель уходит в thinking, бюджет вывода заканчивается, и клиент
// получает done:true с пустым message.content. Дальше клиент считает ход
// законченным, а следующий запрос «продолжи» показывает рассуждение как ответ.
//
// ПРИЧИНА В ВОРКЕРЕ (исправлено в R320): на tools-пути done_reason прошивался
// ЖЁСТКО в "stop". По контракту Ollama "stop" = «модель закончила», а остановка
// по бюджету — "length". С этим значением клиент не может отличить «ответ
// кончился» от «ответ обрезан», и закрывает ход.
//
// ЗАЧЕМ ЭТОТ СЛОЙ, ЕСЛИ ВОРКЕР УЖЕ ИСПРАВЛЕН. Воркер — отдельный образ; в
// момент, когда правка нужна на стенде, собранный образ может быть старше
// балансера (и наоборот). Правило клиентского контракта не должно зависеть от
// того, какой билд воркера сейчас в кластере, поэтому балансер приводит
// завершающий чанк в порядок САМ — но только в том случае, когда воркер нарушил
// контракт наблюдаемым образом:
//
//	done:true, done_reason:"stop", message.content пуст, а размышления в этом
//	стриме уже уехали клиенту.
//
// Всё остальное не трогаем: непустой ответ, уже честную причину ("length",
// "tool_calls", "error"), стримы без размышлений. Это делает слой идемпотентным:
// исправленный воркер (R320) отдаёт "length" сам, и здесь ничего не меняется.
package balancer

import (
	"encoding/json"
	"strings"

	"ollama-loadbalancer/pkg/logger"
)

// nativeDoneHygieneR321 — нормализовать завершающий NDJSON-чанк.
//
// Возвращает (строка, true), если чанк был исправлен. reasoningChars —
// сколько символов размышлений уже уехало клиенту в этом стриме (message.thinking
// или legacy message.reasoning).
//
// Почему пустой content заменяется пояснением: на нативном пути размышления
// приходят отдельным полем, и клиент, который их не рисует (Cline), до этого
// момента не видел НИЧЕГО. Пустой content в done-чанке читается как «модель
// ответила пустотой» — оператор идёт искать поломку. Пояснение называет факт и
// число символов.
func nativeDoneHygieneR321(line string, reasoningChars, visibleStreamedChars int) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || !strings.HasPrefix(trimmed, "{") {
		return line, false
	}
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &chunk); err != nil {
		return line, false
	}
	done, _ := chunk["done"].(bool)
	if !done {
		return line, false
	}
	// R328 (2026-10-30): причина обязана быть контрактным значением для Ollama.
	// Наш внутренний "truncated" (обрыв по НАШЕМУ таймауту/лимиту, R297) для
	// клиента означает «ответ не дописан» — это "length"; пустое значение — "stop".
	// Реестр — done_reason_registry_r328.go; здесь единственная точка, где кадр
	// воркера уходит клиенту без изменений, поэтому нормализация живёт тут.
	rawReason, _ := chunk["done_reason"].(string)
	reason, reasonChanged := normalizeDoneReasonForSurface("ollama", rawReason)
	if reasonChanged {
		chunk["done_reason"] = reason
		if logger.Get() != nil {
			logger.Get().Infow("proxyRequestLlamaCpp[native]: done_reason нормализован (R328)",
				"was", rawReason, "now", reason)
		}
	}
	// R321.2 (2026-10-30): ОТВЕТ УЖЕ УЕХАЛ ДЕЛЬТАМИ — трогать нельзя.
	//
	// ЖИВОЙ РЕГРЕСС (нашёл оператор сразу после выката R321): в конце КАЖДОГО
	// нормального ответа OpenWebUI появлялось «Модель сгенерировала только
	// размышления (1142 симв.)», хотя ответ приходил целиком.
	//
	// Причина ровно в этом пропущенном условии. Воркер отдаёт на tools-пути
	// финальный чанк с ПУСТЫМ content (текст ушёл дельтами) и с `thinking` —
	// то есть «пустой content + размышления» выглядит так же, как настоящий
	// обрыв по бюджету. Разница только одна: был ли видимый текст в дельтах.
	// Если был — это штатное завершение (R108: в done-чанке Ollama content
	// пустой), и подменять его нельзя ни причиной, ни пояснением.
	if visibleStreamedChars > 0 {
		if !reasonChanged {
			return line, false
		}
		out, err := json.Marshal(chunk)
		if err != nil {
			return line, false
		}
		return string(out), true
	}
	// Причина от воркера — факт, её не подменяем, КРОМЕ одного случая: "stop"
	// при пустом ответе после размышлений. Здесь "stop" означает «модель
	// закончила», хотя бюджет кончился, и клиент по этому значению закрывает ход.
	// Остальные значения ("tool_calls", "error", "cancelled") честные.
	if reason != "" && reason != "stop" && reason != "length" {
		if !reasonChanged {
			return line, false
		}
		out, err := json.Marshal(chunk)
		if err != nil {
			return line, false
		}
		return string(out), true
	}
	msg, _ := chunk["message"].(map[string]interface{})
	if msg == nil {
		// /api/generate держит текст в "response"; отдельное поле message не
		// обязательно. Его завершение разбираем ниже по response.
		msg = map[string]interface{}{}
	}
	content, _ := msg["content"].(string)
	if response, ok := chunk["response"].(string); ok {
		content = response
	}
	if strings.TrimSpace(content) != "" {
		// Ответ есть — нечего объяснять, и это не пустое завершение.
		return line, false
	}
	if reasoningChars <= 0 {
		// Размышлений в стриме не было: пустой ответ — это другая история
		// (R314/R261), не наша.
		return line, false
	}

	notice := emptyAnswerNotice(reasoningChars, "length")
	if notice == "" {
		return line, false
	}
	if _, hasResponse := chunk["response"]; hasResponse {
		chunk["response"] = notice
	} else {
		if msg == nil {
			msg = map[string]interface{}{}
		}
		msg["content"] = notice
		if _, ok := msg["role"]; !ok {
			msg["role"] = "assistant"
		}
		chunk["message"] = msg
	}
	// Контракт Ollama: остановка по бюджету — "length", а не "stop". Если воркер
	// уже сказал "length" — значение остаётся прежним, меняем только пояснение.
	chunk["done_reason"] = "length"

	out, err := json.Marshal(chunk)
	if err != nil {
		return line, false
	}
	return string(out), true
}
