// stream_openai_sink_r493.go — R493, этап 6 (2026-11-14): РЕШЕНИЕ О ТРАНСПОРТЕ
// живёт в стоке, а не в вызывающем.
//
// ЧТО БЫЛО. `proxyRequestOpenAIStreaming` сам проверял `w.(http.Hijacker)` и звал
// либо hijack-реализацию, либо legacy. Проверка интерфейса в вызывающем — это и
// есть «второй мозг» транспорта: любая новая ветка обязана помнить про оба пути, а
// ошибка внутри одной из них (R66c: повторный диспатч после неудачного Hijack =
// бесконечная рекурсия и `fatal error: stack overflow`, падение процесса) стоит
// дорого.
//
// Теперь транспорт открывает СТОК:
//
//	opened, err := p.openOpenAIStreamSinkR493(w, resp, backendID, nil)
//	defer opened.close()   // для перехваченного соединения — закрыть и снять опрос
//	err = p.streamOpenAIWithSinkR493(w, r, resp, backendID, opened)
//
// `opened` — либо chunked-сток поверх перехваченного соединения, либо обычный сток
// поверх ResponseWriter. Цикл (`runOpenAISSEStreamR493`) о транспорте не знает:
// спрашивает у стока форму кадра и пишет через него. Различие объявлено явно —
// `streamLineTransportR493`.
package balancer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// openedStreamSinkR493 — открытый транспорт стрима.
//
// Порядок полей — под fieldalignment (линт): указатели/каналы сначала, строки и
// скаляры потом.
type openedStreamSinkR493 struct {
	sink       streamSinkR493
	conn       net.Conn
	bufrw      *bufio.ReadWriter
	clientGone <-chan struct{}
	stopPoll   func()
	backendID  string
	hijacked   bool
	transport  streamLineTransportR493
}

// close — закрыть транспорт: терминатор chunked (R297) на ЛЮБОМ исходе, затем
// снять горутину опроса и закрыть перехваченное соединение.
//
// ЗАЧЕМ ТЕРМИНАТОР ЗДЕСЬ. Заголовки SSE объявляют Transfer-Encoding: chunked, и
// `0\r\n\r\n` писала только штатная концовка цикла. Любой ранний выход (сторож
// тишины, кадр-ошибка, ошибка записи) закрывал соединение БЕЗ терминатора, и
// strict-клиенты (Cline/undici/aiohttp) видели «unexpected EOF» / «Not enough data
// to satisfy transfer length header». Поэтому терминатор — обязанность транспорта,
// а не цикла: раньше он жил в defer внутри цикла и потерялся бы при слиянии, что и
// показал TestHijackStreamGolden_* (EOF вместо кадров).
func (o openedStreamSinkR493) close() {
	if o.hijacked && o.sink != nil {
		// Пустой кадр = терминатор; ошибку писать некуда (клиент мог уйти).
		_, _ = o.sink.WriteFrame(nil)
	}
	if o.stopPoll != nil {
		o.stopPoll()
	}
	if o.conn != nil {
		_ = o.conn.Close()
	}
}

// openOpenAIStreamSinkR493 — единственное место, где решается «перехватить
// соединение или писать в ResponseWriter», и где пишутся заголовки SSE.
//
// hj — перехватчик вызывающего (тесты и уже выполненная проверка интерфейса);
// nil = определить по `w`.
//
// Ошибки нет намеренно: неудачный Hijack — не ошибка, а «транспорт будет обычный».
// Раньше это был откат через повторный диспатч — и именно он давал stack overflow
// (R66c), потому что w продолжает реализовывать http.Hijacker.
func (p *Proxy) openOpenAIStreamSinkR493(w http.ResponseWriter, resp *http.Response, backendID string, hj http.Hijacker, forcePlain bool) openedStreamSinkR493 {
	out := openedStreamSinkR493{backendID: backendID}
	if forcePlain {
		hj = nil
	} else if hj == nil {
		if h, ok := w.(http.Hijacker); ok {
			hj = h
		}
	}
	if hj != nil {
		conn, bufrw, err := hj.Hijack()
		if err != nil {
			logger.Get().Warnw("hijack failed, using plain transport",
				"backend", backendID, "error", err)
		} else {
			// Round 31 #6: keepalive на перехваченном соединении.
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.SetKeepAlive(true)
				_ = tcp.SetKeepAlivePeriod(5 * time.Second)
			}
			// Заголовки SSE пишем вручную: после Hijack Go в фрейминг не вмешивается.
			//
			// Round 32 #16: chunked-фрейминг обязателен — Round 32 #10 удалил
			// Transfer-Encoding, и curl/raw socket это терпели, НО OpenWebUI aiohttp
			// строго требует либо Transfer-Encoding: chunked с правильным фреймингом,
			// либо Content-Length; без них — «Not enough data to satisfy transfer
			// length header» при обрыве стрима.
			statusText := http.StatusText(resp.StatusCode)
			if statusText == "" {
				statusText = "OK"
			}
			bufrw.WriteString(fmt.Sprintf("HTTP/1.1 %d %s\r\n", resp.StatusCode, statusText))
			bufrw.WriteString("Content-Type: text/event-stream\r\n")
			bufrw.WriteString("Cache-Control: no-cache\r\n")
			bufrw.WriteString("Transfer-Encoding: chunked\r\n")
			// R219 (2026-10-21): НЕ обещаем keep-alive: после стрима соединение
			// ЗАКРЫВАЕТСЯ, а клиенты с пулом (Node/OpenAI SDK/OpenWebUI) брали сокет
			// для следующего запроса и получали ECONNRESET.
			bufrw.WriteString("Connection: close\r\n")
			bufrw.WriteString("X-Accel-Buffering: no\r\n")
			// R66c: заголовки балансера из w.Header() (X-Backend-ID / X-Session-ID /
			// X-Model-* / CORS) + заголовки upstream'а, которых ещё нет.
			writeHijackedHeaders(bufrw, w, resp.Header, hijackSSEReservedHeaders)
			bufrw.WriteString("\r\n")
			bufrw.Flush()

			gone, stopPoll := startHijackDisconnectPollR493(conn, backendID)
			out.conn = conn
			out.bufrw = bufrw
			out.sink = newHijackStreamSinkR493(conn, bufrw.Writer)
			out.hijacked = true
			out.transport = streamLineTransportChunkedR493
			out.clientGone = gone
			out.stopPoll = stopPoll
			return out
		}
	}

	// Обычный транспорт: заголовки upstream'а копируем в w.Header() ДО WriteHeader —
	// ровно как делал legacy-путь (R60.15: upstream X-Request-Id → X-Upstream-Request-Id,
	// Vary: Origin отбрасывается, CORS/длина/кодирование — не наши).
	copyOpenAIUpstreamHeadersR493(w, resp)
	w.WriteHeader(resp.StatusCode)
	out.sink = newPlainStreamSinkR493(w)
	out.transport = streamLineTransportPlainR493
	return out
}

// startHijackDisconnectPollR493 — горутина опроса разрыва на перехваченном
// соединении (Round 31 #6: <100 мс против десятков секунд).
//
// `SetReadDeadline(time.Unix(0,0))` ставит дедлайн в прошлое, и read возвращается
// немедленно: у живого клиента это i/o timeout, у ушедшего — EOF/RST. Закрытие
// возвращённого канала — и есть «клиент ушёл»; stopPoll снимает горутину.
func startHijackDisconnectPollR493(conn net.Conn, backendID string) (<-chan struct{}, func()) {
	gone := make(chan struct{})
	stop := make(chan struct{})
	var stopOnce bool
	stopPoll := func() {
		if !stopOnce {
			stopOnce = true
			close(stop)
		}
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Get().Warnw("hijack polling goroutine panic",
					"backend", backendID, "recover", r)
			}
			close(gone)
		}()
		buf := make([]byte, 1)
		ticker := time.NewTicker(100 * time.Millisecond) // 10 опросов/с
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_ = conn.SetReadDeadline(time.Unix(0, 0))
				_, err := conn.Read(buf)
				if err != nil {
					var netErr net.Error
					if errors.As(err, &netErr) && netErr.Timeout() {
						continue // клиент жив
					}
					logger.Get().Infow("hijack polling: client disconnected (SetReadDeadline)",
						"backend", backendID, "error", err)
					return
				}
			}
		}
	}()
	return gone, stopPoll
}

// openAIStreamNon2xxR493 — не-2xx до открытия транспорта: тело читаем целиком и
// отдаём причину в форме, которую ждёт клиент (SSE — кадрами, иначе JSON).
//
// handled=false — статус 2xx, продолжай стрим.
func (p *Proxy) openAIStreamNon2xxR493(w http.ResponseWriter, resp *http.Response) (handled bool, err error) {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return false, nil
	}
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		body = []byte(fmt.Sprintf(`{"error":"upstream read error: %v"}`, readErr))
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(resp.StatusCode)
		fmt.Fprintf(w, "data: {\"error\":\"upstream returned HTTP %d\",\"choices\":[{\"delta\":{},\"finish_reason\":\"error\"}]}\n\n", resp.StatusCode)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return true, nil
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
	return true, nil
}

// streamOpenAIWithSinkR493 — тело ответа на открытом транспорте: не-SSE отдаём как
// есть, SSE — единым циклом (runOpenAISSEStreamR493).
func (p *Proxy) streamOpenAIWithSinkR493(w http.ResponseWriter, r *http.Request, resp *http.Response, backendID string, opened openedStreamSinkR493) error {
	defer resp.Body.Close()

	contentType := resp.Header.Get("Content-Type")
	isSSEResponse := strings.Contains(contentType, "text/event-stream") ||
		strings.Contains(contentType, "application/x-ndjson")
	if !isSSEResponse {
		return p.writeNonSSEStreamResponseR493(w, resp, opened)
	}

	model := ""
	if m, ok := r.Context().Value(modelContextKey).(string); ok {
		model = m
	}
	// R217/R271: учёт активных стримов — иначе graceful shutdown не ждёт ответ, а
	// перезагрузка модели по чужому запросу выгружает её под этим стримом.
	endClientStream := beginClientStreamR271(p, backendID, model)
	defer endClientStream()
	streamSess := startStreamSession(p, r, model)
	defer streamSess.Stop()

	// R360: id генерации на воркере — без него отмена гасит ВСЕ генерации этой
	// модели на узле, включая чужие.
	upstreamReqID := resp.Header.Get("X-Request-Id")

	run := sseStreamRunR493{
		clientGone: opened.clientGone,
		schemas:    p.toolSchemasForRequest(r),
		writeFrame: func(fr []byte) error {
			_, err := opened.sink.WriteFrame(fr)
			return err
		},
		heartbeat: func() error {
			// Форма keepalive — транспортная: SSE-комментарий в chunked-кадре или
			// как есть. Пустая строка-разделитель комментарию не нужна.
			if opened.hijacked {
				_, err := opened.sink.WriteFrame([]byte(": keepalive\r\n"))
				return err
			}
			_, err := opened.sink.WriteFrame([]byte(": keepalive\n\n"))
			return err
		},
		backendID: backendID,
		model:     model,
		transport: opened.transport,
		logTag:    "openai sse",
	}
	run.noteTerminator = opened.sink.NoteTerminator
	if !opened.hijacked {
		run.clientGone = r.Context().Done()
		run.writeError = func(message string) bool {
			ok := writePlainStreamError(w, message)
			opened.sink.NoteTerminator() // функция пишет [DONE] сама
			return ok
		}
		run.ensureDone = func(last []byte) bool {
			// Решение — по ФАКТУ записи терминатора (R252): последним кадром может
			// быть пустая строка-разделитель после [DONE].
			if opened.sink.TerminatorSent() {
				return false
			}
			if _, err := opened.sink.WriteFrame([]byte(sseDoneFrame)); err != nil {
				return false
			}
			opened.sink.NoteTerminator()
			return true
		}
		return p.runOpenAISSEStreamR493(run, resp)
	}
	run.writeError = func(message string) bool {
		ok := writeChunkedStreamError(opened.bufrw, message)
		opened.sink.NoteTerminator() // функция пишет [DONE] сама
		return ok
	}
	run.ensureDone = func(last []byte) bool {
		if opened.sink.TerminatorSent() {
			return false
		}
		done := ensureChunkedSSEDone(opened.bufrw, nil)
		if done {
			opened.sink.NoteTerminator()
		}
		return done
	}
	run.cancel = func(reason string) {
		p.cancelUpstreamGeneration(backendID, model, upstreamReqID, reason)
	}
	return p.runOpenAISSEStreamR493(run, resp)
}

// writeNonSSEStreamResponseR493 — не-SSE тело на уже открытом транспорте: hijack
// пишет заголовки вручную (Content-Length по фактически прочитанному телу),
// обычный транспорт — через ResponseWriter.
func (p *Proxy) writeNonSSEStreamResponseR493(w http.ResponseWriter, resp *http.Response, opened openedStreamSinkR493) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read non-streaming response: %w", err)
	}
	// Round 31 #7: token usage для мониторинга.
	if resp.StatusCode < 400 {
		var openaiResp map[string]interface{}
		if json.Unmarshal(body, &openaiResp) == nil {
			if usage, ok := openaiResp["usage"].(map[string]interface{}); ok {
				var prompt, completion int64
				if v, ok := usage["prompt_tokens"].(float64); ok {
					prompt = int64(v)
				}
				if v, ok := usage["completion_tokens"].(float64); ok {
					completion = int64(v)
				}
				if prompt > 0 || completion > 0 {
					modelName, _ := openaiResp["model"].(string)
					p.recordTokenUsage(modelName, prompt, completion)
				}
			}
		}
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	if !opened.hijacked {
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
		return nil
	}
	bufrw := opened.bufrw
	statusText := http.StatusText(resp.StatusCode)
	if statusText == "" {
		statusText = "Unknown"
	}
	bufrw.WriteString(fmt.Sprintf("HTTP/1.1 %d %s\r\n", resp.StatusCode, statusText))
	bufrw.WriteString(fmt.Sprintf("Content-Type: %s\r\n", contentType))
	bufrw.WriteString(fmt.Sprintf("Content-Length: %d\r\n", len(body)))
	bufrw.WriteString("Connection: close\r\n")
	writeHijackedHeaders(bufrw, w, resp.Header, hijackReservedHeaders)
	bufrw.WriteString("\r\n")
	bufrw.Write(body)
	bufrw.Flush()
	return nil
}

// copyOpenAIUpstreamHeadersR493 — перенести заголовки upstream'а в w.Header() для
// обычного транспорта. Вынесено из legacy-цикла как есть, чтобы замена тела не
// изменила клиенту ни один заголовок.
func copyOpenAIUpstreamHeadersR493(w http.ResponseWriter, resp *http.Response) {
	for key, values := range resp.Header {
		keyLower := strings.ToLower(key)
		if keyLower == "transfer-encoding" || keyLower == "content-length" || keyLower == "connection" {
			continue
		}
		if strings.HasPrefix(keyLower, "access-control-") {
			continue
		}
		// R60.15: upstream X-Request-Id отдаём как X-Upstream-Request-Id, чтобы не
		// спорить с собственным X-Request-Id балансера.
		if keyLower == "x-request-id" {
			for _, value := range values {
				if existing := w.Header().Get("X-Upstream-Request-Id"); existing == "" {
					w.Header().Set("X-Upstream-Request-Id", value)
				}
			}
			continue
		}
		if keyLower == "vary" {
			for _, value := range values {
				if strings.ToLower(strings.TrimSpace(value)) == "origin" {
					continue
				}
				w.Header().Add(key, value)
			}
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
}
