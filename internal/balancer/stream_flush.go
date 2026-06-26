// stream_flush.go — утилиты для flush streaming-ответов в балансировщике.
//
// Назначение: при проксировании SSE / NDJSON стримов от cppworker к клиенту
// балансер должен вызывать http.Flusher.Flush() после каждого записанного
// чанка. Без этого:
//
//  1. Данные копятся в Go HTTP буфере (default ~4KB).
//  2. Flush происходит только при заполнении буфера или при выходе из handler.
//  3. На быстрых моделях (Qwen3.6-35B-A3B, Llama-3.1-70B) финальный чанк
//     с finish_reason / done:true / [DONE] может уйти в TCP-сокет одним пакетом
//     с предпоследними content-чанками, и клиент (aiohttp, httpx, requests с
//     stream=True) парсит chunked-encoding с ошибкой:
//
//     Response payload is not completed:
//     <TransferEncodingError: 400, message='Not enough data to satisfy transfer length header.'>
//
//  4. На медленных моделях (gemma-4-E4B, 4-5 tok/s) TCP-буфер успевает
//     опустошаться между чанками через backpressure, и проблема не воспроизводится.
//
// Этот файл предоставляет единую точку для flush, чтобы избежать регрессий
// при добавлении новых streaming-эндпоинтов.
package balancer

import (
	"io"
	"net/http"
)

// flushIfPossible вызывает http.Flusher.Flush() на writer, если он это поддерживает.
//
// Используется после каждой записи SSE / NDJSON чанка в http.ResponseWriter.
// Если writer не поддерживает http.Flusher (например, обёрнут в какой-то
// middleware), вызов silently игнорируется — это безопасно, так как
// streaming-эндпоинты всегда возвращают http.ResponseWriter напрямую от net/http.
//
// Использование:
//
//	fmt.Fprintf(w, "data: %s\n\n", data)
//	balancer.Flush(w)
//
// или через обёртку:
//
//	fw := balancer.NewFlushWriter(w)
//	io.Copy(fw, upstreamResp.Body)
func Flush(w io.Writer) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// flushWriter — io.Writer, который вызывает http.Flusher.Flush() после каждой записи.
//
// Используется для оборачивания http.ResponseWriter в местах, где streaming
// делается через io.Copy от upstream Body к downstream writer. Без обёртки
// io.Copy копирует данные в Go HTTP буфер и flush'ит только при выходе,
// что приводит к TransferEncodingError на быстрых моделях.
//
// Использование:
//
//	upstreamBody := upstreamResp.Body
//	downstreamWriter := balancer.NewFlushWriter(w)
//	io.Copy(downstreamWriter, upstreamBody)
//
// Потокобезопасность: net/http гарантирует, что http.ResponseWriter
// используется только в одном goroutine на запрос, поэтому блокировки не нужны.
type flushWriter struct {
	w io.Writer
	f http.Flusher
}

// NewFlushWriter создаёт io.Writer, который вызывает Flush() после каждой записи.
//
// Если w не реализует http.Flusher, возвращает w как есть — без обёртки,
// чтобы не делать лишних проверок в hot path.
func NewFlushWriter(w io.Writer) io.Writer {
	if f, ok := w.(http.Flusher); ok {
		return &flushWriter{w: w, f: f}
	}
	return w
}

// Write реализует io.Writer. После каждой успешной записи вызывает Flush().
func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if n > 0 && fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}