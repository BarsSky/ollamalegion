package balancer

// empty_answer_r261.go — R261 (2026-10-28): пустой видимый ответ обязан быть
// ОБЪЯСНЁН, а не отдан молча.
//
// ЖИВАЯ ЖАЛОБА (клиент DSH, стенд r254→r260): «генерация в бэкенде идёт, но в
// ответ клиенту DSH ничего не приходит даже после окончания генерации».
//
// ФАКТЫ СО СТЕНДА (воспроизведено запросом к /v1/chat/completions, модель
// gemma-4-E4B-it-Q4_K_M, stream=true, max_tokens=160):
//
//	кадров 157 · role-чанков 2 · content_len=0 · reasoning_len=359 ·
//	[DONE] ровно один · finish_reason=length
//
// То есть модель ответила (359 символов), но ВЕСЬ её текст оказался в канале
// размышлений: R256 (`reasoning_egress_r256.go`) перекладывает мысли из
// delta.content в delta.reasoning_content — и делает это правильно, иначе
// размышления снова «мешаются с основным ответом» (жалоба Cline). Проблема не в
// фильтре, а в том, ЧТО мы отдаём в конце: клиент, который не рисует
// reasoning_content (headless-агенты, часть SDK), видит пустой ответ и считает,
// что ничего не пришло.
//
// ВТОРОЙ ДЕФЕКТ, найденный здесь же: хвост, удержанный как «возможный префикс
// маркера» (`reasoningSplitState.pending`), НИКОГДА не сбрасывался в конце
// стрима. Если ответ заканчивался символом, который может начинать маркер
// (`<`, `[`), последние байты терялись молча — текст ответа обрывался.
//
// РЕШЕНИЕ.
//  1. В конце стрима удержанный хвост сбрасывается в свой канал — ничего не
//     теряем.
//  2. Если видимого текста не было вовсе, а размышления были — клиент получает
//     ОДИН кадр-объяснение в content (с числами и что делать), а не пустоту.
//     Кадр ставится ПЕРЕД finish_reason: строгие клиенты не читают контент после
//     finish, поэтому finish-кадр мы придерживаем до конца стрима и отдаём
//     последним (это же снимает риск «контент после finish»).
//  3. В лог — одна строка с числами (visible_chars / reasoning_chars /
//     notice_sent / finish_reason), чтобы такие случаи были видны без раскопок.
//
// Почему объяснение, а не «отдать размышления как ответ»: размышления уже ушли
// клиенту отдельным каналом; продублировать их в content — значит показать
// клиенту один и тот же текст дважды (у Cline — «мысли» и «ответ» одинаковые).
// Объяснение же честно называет причину и цену решения.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"ollama-loadbalancer/pkg/logger"
)

// emptyAnswerNotice — текст, который клиент увидит ВМЕСТО молчаливой пустоты.
//
// Формулировка намеренно содержит цифры и действие: оператор (и агент) должны
// понимать, что ответ был, но ушёл в размышления, и что менять.
func emptyAnswerNotice(reasoningChars int, finishReason string) string {
	reason := "stop"
	if strings.TrimSpace(finishReason) != "" {
		reason = finishReason
	}
	hint := "Отключите размышления (reasoning) или увеличьте max_tokens."
	if reason == "length" {
		hint = "Бюджет ответа израсходован на размышления: увеличьте max_tokens или отключите reasoning."
	}
	return fmt.Sprintf(
		"⚠️ Модель сгенерировала только размышления (%d симв.), видимого ответа нет "+
			"(finish_reason=%s). %s", reasoningChars, reason, hint)
}

// finishReasonOfSSELine — finish_reason из SSE-кадра ("" если его нет).
//
// `finish_reason: null` (обычный вид промежуточного кадра) даёт пустую строку:
// именно так отличается «кадр со finish» от обычного.
func finishReasonOfSSELine(line []byte) string {
	payload := extractSSEDataPayload(bytes.TrimSpace(line))
	if payload == nil {
		return ""
	}
	var chunk struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil || len(chunk.Choices) == 0 {
		return ""
	}
	if chunk.Choices[0].FinishReason == nil {
		return ""
	}
	return *chunk.Choices[0].FinishReason
}

// sseDeltaFrame — кадр с одним delta (content и/или reasoning_content).
func sseDeltaFrame(content, reasoning string) []byte {
	delta := map[string]interface{}{}
	if content != "" {
		delta["content"] = content
	}
	if reasoning != "" {
		delta["reasoning_content"] = reasoning
	}
	chunk := map[string]interface{}{
		"object": "chat.completion.chunk",
		"choices": []interface{}{
			map[string]interface{}{"index": 0, "delta": delta, "finish_reason": nil},
		},
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return nil
	}
	return []byte("data: " + string(b) + "\n\n")
}

// tryHoldFinishFrame — придержать finish-кадр до конца стрима.
//
// Нужен ровно для одного: поставить объяснение пустого ответа ПЕРЕД finish_reason.
// Возвращает true, если кадр удержан и клиенту его писать сейчас НЕЛЬЗЯ.
func (st *reasoningEgressState) tryHoldFinishFrame(line []byte) bool {
	if st == nil || st.heldFinish != nil {
		return false
	}
	if !isOpenAISSEDataLine(line) {
		return false
	}
	if finishReasonOfSSELine(line) == "" {
		return false
	}
	st.heldFinish = append([]byte(nil), line...)
	return true
}

// finalizeFrames — кадры, которые нужно отдать клиенту в КОНЦЕ стрима.
//
// Порядок: сброс удержанного хвоста → объяснение пустого ответа → finish-кадр.
// Идемпотентен: повторный вызов ничего не отдаёт.
func (st *reasoningEgressState) finalizeFrames() [][]byte {
	if st == nil || st.finalized {
		return nil
	}
	st.finalized = true

	var frames [][]byte

	// 1. Хвост, удержанный как возможный префикс маркера. До R261 он просто
	//    терялся: ответ обрывался на последнем символе.
	if st.split.pending != "" {
		tail := st.split.pending
		st.split.pending = ""
		if st.split.inReasoning {
			st.reasoningChars += len(tail)
			if fr := sseDeltaFrame("", tail); fr != nil {
				frames = append(frames, fr)
			}
		} else {
			st.visibleChars += len(tail)
			if fr := sseDeltaFrame(tail, ""); fr != nil {
				frames = append(frames, fr)
			}
		}
	}

	// 2. Пустой видимый ответ при непустых размышлениях — объясняем.
	noticeSent := false
	finishReason := ""
	if st.heldFinish != nil {
		finishReason = finishReasonOfSSELine(st.heldFinish)
	}
	if st.visibleChars == 0 && st.reasoningChars > 0 {
		if fr := sseDeltaFrame(emptyAnswerNotice(st.reasoningChars, finishReason), ""); fr != nil {
			frames = append(frames, fr)
			noticeSent = true
		}
	}

	// 3. Придержанный finish-кадр — последним.
	heldFinish := st.heldFinish != nil
	if heldFinish {
		frames = append(frames, st.heldFinish)
		st.heldFinish = nil
	}

	if logger.Get() != nil {
		logger.Get().Infow("stream egress finalize: видимый/скрытый текст",
			"visible_chars", st.visibleChars,
			"reasoning_chars", st.reasoningChars,
			"finish_reason", finishReason,
			"notice_sent", noticeSent,
			"held_finish", heldFinish,
			"frames_cut", st.cut)
	}
	return frames
}

// applyEmptyAnswerNotice — то же правило для СОБРАННОГО (non-stream) ответа.
//
// Auto-stream собирает ответ в память и отдаёт цельным JSON: если весь текст ушёл
// в reasoning, клиент получает `content:""`. Это тот же дефект, что и в стриме,
// поэтому правило одно — объяснить, а не молчать.
func (a *openAIStreamAccumulator) applyEmptyAnswerNotice() bool {
	if a == nil {
		return false
	}
	if a.content.Len() > 0 || a.reasoning.Len() == 0 {
		return false
	}
	a.content.WriteString(emptyAnswerNotice(a.reasoning.Len(), a.finishReason))
	if logger.Get() != nil {
		logger.Get().Warnw("non-stream ответ состоял только из размышлений — "+
			"клиенту отправлено объяснение",
			"model", a.model, "reasoning_len", a.reasoning.Len(),
			"finish_reason", a.finishReason)
	}
	return true
}
