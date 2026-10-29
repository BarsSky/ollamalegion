package balancer

import "bytes"

// sse_boundary_r277.go — R277 (2026-10-29): граница SSE-события для кадров,
// которые пишет САМ балансер.
//
// ЖИВОЙ ДЕФЕКТ (клиент DSH напрямую к OpenAI-порту 18079). Модель генерировала,
// но клиент падал с «Could not parse message into JSON» на хвосте потока, а затем
// отваливался. Причина видна в сырых байтах ответа:
//
//	data: {"choices":[{"delta":{"content":null,...,"finish_reason":"stop"}]}\n
//	data: [DONE]\n\n
//
// То есть придержанный finish-кадр (R261 держит его до конца стрима, чтобы
// объяснение пустого ответа шло ПЕРЕД finish_reason) уходил с ОДНИМ переводом
// строки. Остальные кадры балансер пишет как `data: ...}\n` + ещё один `\n`,
// восстанавливая границу события (Round 32 #17), а кадры из
// `egressFilter.finalizeFrames()` писались как есть — без второго перевода.
//
// Для SSE это не «лишний пробел», а склейка событий: строгий парсер (DSH/aiohttp/
// undici) читает событие до пустой строки, получает два `data:` в одном блоке и
// пытается распарсить их как одно JSON-сообщение → ошибка, поток считается битым.
// Клиенты, которые прощают это (Cline), работали, поэтому дефект и не всплывал
// раньше — ровно тот класс «новый путь обошёл общий», что и R266/R272.
//
// Правило простое и одно на все пути вывода: кадр, начинающийся с `data:`,
// обязан заканчиваться пустой строкой (`\n\n`). Комментарии (`: keepalive`) и
// уже размеченные кадры не трогаем.
func withSSEEventBoundary(frame []byte) []byte {
	if len(frame) == 0 {
		return frame
	}
	if !isOpenAISSEDataLine(frame) {
		// SSE-комментарии (`: keepalive\n`) границы события не требуют.
		return frame
	}
	if bytes.HasSuffix(frame, []byte("\n\n")) {
		return frame
	}
	out := make([]byte, 0, len(frame)+2)
	out = append(out, frame...)
	if !bytes.HasSuffix(out, []byte("\n")) {
		out = append(out, '\n')
	}
	out = append(out, '\n')
	return out
}
