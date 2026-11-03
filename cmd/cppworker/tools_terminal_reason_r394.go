package main

// tools_terminal_reason_r394.go — R394 (2026-11-04): честная причина и текст
// терминального кадра, когда вывод упёрся в клиентский лимит.
//
// ЖИВОЙ ФАКТ (A10, Cline через балансер, /api/chat + tools). Лог воркера:
//
//	/api/chat stream response summary  streamed_content_bytes=456
//	  final_content_len=0  thinking_len=0  tool_calls=0
//	  done_reason=stop  raw_len=15812  elapsed_ms=455084
//
// Что произошло на самом деле. Клиент задал `num_predict=4096` (Cline ставит
// max_tokens на каждый запрос). Первая попытка за ~7 минут не дала видимого
// вывода (вся генерация ушла в скрытые размышления), R188 повторил генерацию, и
// вторая попытка отдала 456 байт видимого ответа — после чего модель упёрлась
// ровно в 4096 токенов (`raw_len=15812` ≈ бюджет при ~3.9 байта на токен).
//
// Почему это дефект. `done_reason` считался по R320 (только «блок размышлений
// не закрыт»), поэтому закрытый блок давал `stop` — по контракту Ollama это
// «модель закончила». Cline закрывал ход как полноценный ответ, хотя видимый
// ответ был ОБРЕЗАН на 456 байтах. Оператор описал это как «флаг завершения
// проскакивает раньше, чем приходит настоящий ответ»: настоящий ответ не
// приходил вовсе, потому что клиент считал ход законченным.
//
// Контракт (Ollama): усечение по бюджету вывода — `done_reason:"length"`,
// законченный ответ — `"stop"`, вызов инструмента — `"tool_calls"`.
//
// ВТОРАЯ ЧАСТЬ (R394.1): пояснение вместо пустого content. R320 ставил его
// только когда размышления ОТДАЮТСЯ (`reasoningOut != ""`). При явном
// `think:false` размышления скрыты, `reasoningOut == ""`, и клиент получал
// `content:""` + `stop` — ровно то молчание, от которого R314 защищает другие
// поверхности (в его же комментарии: «иначе клиент с think=false получает ровно
// то же молчание»). Признак «размышления были» считаем по СЫРОМУ выводу, до
// применения политики отдачи.

import (
	"strings"
)

// toolsBudgetExhaustedR394 — вывод упёрся в клиентский лимит n_predict.
//
// Считаем по числу токенов СЫРОГО вывода, полученному токенизатором модели
// (то же число, что уходит в eval_count), а НЕ по числу вызовов callback'а.
//
// ЖИВОЙ ПРОГОН (стенд, 2026-11-04): num_predict=16, eval_count=17, а
// callback'ов с непустым токеном — 14 (часть токенов приходит пустой
// строкой/служебной). Счётчик по callback'ам занижал вывод, и обрезанный ответ
// снова закрывался как done_reason=stop — ровно тот дефект, который чинит R394.
//
// n_predict <= 0 означает «лимита нет» (Ollama: -1/0 = дефолт): усечения по
// бюджету в этом случае быть не может, поэтому false.
//
// R409 (2026-11-03): ДОПУСК -1 ТОКЕН. Живой лог A10 (Cline, 27B):
//
//	n_predict=4096  generated_tokens=4095  budget_exhausted=false
//	done_reason=stop  streamed_content_bytes=416  final_content_len=0
//
// llama.cpp останавливается на n_predict декодированных токенов, но в
// детокенизированном выводе их может быть на один меньше (последний — EOS/
// служебный, текста не даёт). Строгое `>= nPredict` из-за этого не замечало
// упор в кап: клиент получал stop на обрезанном ответе, Cline закрывал ход
// («COMPLETE» сразу после начала размышлений), а R406 не срабатывал, потому
// что budget_exhausted=false.
func toolsBudgetExhaustedR394(nPredict, generatedTokens int) bool {
	if nPredict <= 0 || generatedTokens <= 0 {
		return false
	}
	if generatedTokens >= nPredict {
		return true
	}
	return generatedTokens >= nPredict-1
}

// toolsTerminalReasonR394 — причина остановки для терминального кадра
// tools-пути. Порядок правил: вызов инструмента → незакрытый reasoning (R320) →
// исчерпанный бюджет вывода (R394) → законченный ответ.
func toolsTerminalReasonR394(fullOutput, visibleOutput, prompt string, toolCalls, nPredict, generatedTokens int) string {
	if toolCalls > 0 {
		return "tool_calls"
	}
	if toolsReasoningUnclosedR320(fullOutput, visibleOutput, prompt) {
		return "length"
	}
	if toolsBudgetExhaustedR394(nPredict, generatedTokens) {
		return "length"
	}
	return "stop"
}

// reasoningCharsRawR394 — сколько символов размышлений было в СЫРОМ выводе.
//
// Нужно потому, что splitToolsOutput отдаёт reasoningOut пустым при явном
// think=false (политика отдачи), и по нему нельзя понять, ушёл ли бюджет в
// размышления. Здесь политика не применяется: считаем то, что модель реально
// сгенерировала.
func reasoningCharsRawR394(fullOutput string) int {
	rsn, _, has := SplitReasoningContent(fullOutput)
	if !has {
		return 0
	}
	return len([]rune(strings.TrimSpace(rsn)))
}

// toolsTerminalContentR394 — видимый текст терминального кадра: пояснение,
// если ответа нет, а размышления были (в том числе скрытые через think=false).
//
// Непустой ответ не трогаем никогда: иначе мы начали бы дописывать текст в
// чужие ответы (то же правило, что у emptyAnswerNoticeForNativeR314).
func toolsTerminalContentR394(visibleOutput, fullOutput string) string {
	if notice := emptyAnswerNoticeForNativeR314(visibleOutput, reasoningCharsRawR394(fullOutput)); notice != "" {
		return notice
	}
	return visibleOutput
}
