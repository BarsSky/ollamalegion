// stream_sink_r493.go — R493-12r (2026-11-13, этап 6 плана «два мозга»):
// ОДИН СТОК СТРИМА (флаг LB_STREAM_PIPE).
//
// ЖИВОЙ ФАКТ (план §1: «8 циклов стрима, 11 `for {}` в балансере»). Клиентский
// вывод писали, считали и флашили в КАЖДОМ цикле по-своему: в NDJSON-ветке
// `proxyRequestLlamaCpp` — `fmt.Fprintf(w, "%s\n", line)` плюс отдельный
// `bytesForwarded.Add` плюс отдельный `flusher.Flush()` (два места), в SSE-ветке
// OpenAI — свой счётчик и свой флаш, в hijack-пути — ещё один (`bufio.Writer`
// поверх сырого соединения). Каждое из этих мест можно забыть поправить, и
// расхождение видно клиенту: кадр без перевода строки склеивается со следующим,
// забытый флаш превращает поток в «замер на N секунд», а забытый счётчик делает
// сводку (`streamed_content_bytes`) ложью — ровно этим и объяснялись прошлые
// живые разборы «токены шли, а ответ не дошёл».
//
// ЧТО ЗДЕСЬ. Один интерфейс `streamSinkR493` — «записать строку клиенту» — и две
// реализации, которые уже существуют в коде:
//
//   - `plainStreamSinkR493` — обычный `http.ResponseWriter` (NDJSON/SSE без
//     hijack): пишет байты, считает их, флашит, если писатель умеет;
//   - `hijackStreamSinkR493` — сырое соединение после `Hijack()` (OpenAI SSE с
//     детектом отключения клиента): тот же контракт, другой писатель.
//
// Смысл не в том, чтобы «стало красивее», а в том, что РЕШЕНИЕ «писать в
// ResponseWriter или в соединение» и ПРАВИЛА записи (перевод строки, счётчик,
// флаш) живут в одном месте. Цикл стрима больше не знает, куда пишет.
//
// ФЛАГ `LB_STREAM_PIPE` (горячая настройка, группа `streaming`): 0 — историческое
// поведение (цикл пишет напрямую, как раньше), 1 — цикл пишет через сток. Откат —
// снять флаг.
//
// ГРАНИЦА ЭТОГО ШАГА (честно): сам цикл пока остаётся один на поверхность, и
// hijack-решение принимается там же, где принималось. Сведены ПРАВИЛА записи и
// счётчик; сведение самих циклов и перенос hijack-решения в сток — следующий шаг
// этапа.
package balancer

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"

	"ollama-loadbalancer/internal/settings"
)

// streamSinkR493 — куда и как пишется клиентский кадр стрима.
//
// Контракт намеренно узкий: строка (без перевода строки — его добавляет сток),
// счётчик отправленного и признак «терминатор уже ушёл». Всё остальное (что
// именно за кадр) — дело цикла.
type streamSinkR493 interface {
	// WriteFrame — записать РОВНО эти байты как один кадр клиенту и сбросить
	// буфер. Транспортное обрамление — дело стока: обычный ResponseWriter пишет
	// как есть (фрейминг chunked/Content-Length делает net/http), hijack-сток
	// сам обрамляет chunked-фреймом (R32 #16) и флашит. Цикл стрима форму кадра
	// больше не выбирает.
	WriteFrame(frame []byte) (int, error)
	// WriteLine — то же для NDJSON: перевод строки добавляет сток (клиенты Ollama
	// читают поток построчно). Возвращает число записанных байт (с переводом
	// строки) для сводки.
	WriteLine(line []byte) (int, error)
	// TerminatorSent — ушёл ли терминальный кадр (`[DONE]`/`done:true`). Факт о
	// потоке, а не о переменной цикла: ровно на этом расхождении R252 дописывал
	// второй `data: [DONE]`.
	TerminatorSent() bool
	// NoteTerminator — зафиксировать, что терминатор сейчас записан.
	NoteTerminator()
	// Written — сколько байт сток отдал клиенту (единственный счётчик кадра:
	// сводка `streamed_content_bytes` берётся отсюда, а не из переменной цикла).
	Written() int64
	// Flush — сбросить буфер, если реализация его держит.
	Flush()
}

// streamPipeEnabledR493 — включён ли единый сток.
func streamPipeEnabledR493() bool {
	v, ok := settings.HotInt("LB_STREAM_PIPE")
	return ok && v == 1
}

// plainStreamSinkR493 — обычный ResponseWriter (без hijack).
type plainStreamSinkR493 struct {
	w              io.Writer
	flusher        http.Flusher
	written        int64
	terminatorSent atomic.Bool
}

// newPlainStreamSinkR493 — сток поверх ResponseWriter.
func newPlainStreamSinkR493(w http.ResponseWriter) *plainStreamSinkR493 {
	s := &plainStreamSinkR493{w: w}
	if f, ok := w.(http.Flusher); ok {
		s.flusher = f
	}
	return s
}

func (s *plainStreamSinkR493) WriteFrame(frame []byte) (int, error) {
	if s == nil || s.w == nil {
		return 0, io.ErrClosedPipe
	}
	n, err := s.w.Write(frame)
	s.written += int64(n)
	s.Flush()
	return n, err
}

func (s *plainStreamSinkR493) WriteLine(line []byte) (int, error) {
	if s == nil || s.w == nil {
		return 0, io.ErrClosedPipe
	}
	// Один Write на кадр: байты и перевод строки уходят вместе, иначе кадр может
	// склеиться со следующим (клиент ждёт строку целиком).
	buf := line
	if n := len(line); n == 0 || line[n-1] != '\n' {
		buf = make([]byte, 0, len(line)+1)
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}
	n, err := s.w.Write(buf)
	s.written += int64(n)
	s.Flush()
	return n, err
}

func (s *plainStreamSinkR493) Written() int64 {
	if s == nil {
		return 0
	}
	return s.written
}

func (s *plainStreamSinkR493) TerminatorSent() bool { return s != nil && s.terminatorSent.Load() }

func (s *plainStreamSinkR493) NoteTerminator() {
	if s != nil {
		s.terminatorSent.Store(true)
	}
}

func (s *plainStreamSinkR493) Flush() {
	if s != nil && s.flusher != nil {
		s.flusher.Flush()
	}
}

// hijackStreamSinkR493 — сырое соединение после Hijack (OpenAI SSE с детектом
// отключения клиента). Контракт тот же: строка, счётчик, терминатор.
type hijackStreamSinkR493 struct {
	conn           net.Conn
	bw             *bufio.Writer
	written        int64
	terminatorSent atomic.Bool
}

func newHijackStreamSinkR493(conn net.Conn, bw *bufio.Writer) *hijackStreamSinkR493 {
	return &hijackStreamSinkR493{conn: conn, bw: bw}
}

// WriteFrame — chunked-фрейм HTTP/1.1: "{hex}\r\n{data}\r\n", затем flush.
//
// Ровно то, что делает writeChunkedFrame (R32 #16): после Hijack Go в фрейминг
// не вмешивается, и без этого OpenWebUI/aiohttp падают с
// "Not enough data to satisfy transfer length header" при обрыве стрима.
func (s *hijackStreamSinkR493) WriteFrame(frame []byte) (int, error) {
	if s == nil || s.bw == nil {
		return 0, io.ErrClosedPipe
	}
	if _, err := fmt.Fprintf(s.bw, "%x\r\n", len(frame)); err != nil {
		return 0, err
	}
	if len(frame) > 0 {
		if _, err := s.bw.Write(frame); err != nil {
			return 0, err
		}
	}
	if _, err := s.bw.WriteString("\r\n"); err != nil {
		return 0, err
	}
	s.written += int64(len(frame))
	if err := s.bw.Flush(); err != nil {
		return len(frame), err
	}
	return len(frame), nil
}

func (s *hijackStreamSinkR493) WriteLine(line []byte) (int, error) {
	if s == nil || s.bw == nil {
		return 0, io.ErrClosedPipe
	}
	n, err := s.bw.Write(line)
	if err == nil && (len(line) == 0 || line[len(line)-1] != '\n') {
		var m int
		m, err = s.bw.Write([]byte{'\n'})
		n += m
	}
	s.written += int64(n)
	s.Flush()
	return n, err
}

func (s *hijackStreamSinkR493) Written() int64 {
	if s == nil {
		return 0
	}
	return s.written
}

func (s *hijackStreamSinkR493) TerminatorSent() bool { return s != nil && s.terminatorSent.Load() }

func (s *hijackStreamSinkR493) NoteTerminator() {
	if s != nil {
		s.terminatorSent.Store(true)
	}
}

func (s *hijackStreamSinkR493) Flush() {
	if s != nil && s.bw != nil {
		// Ошибка флаша не возвращается наружу намеренно: вызывающий уже получил
		// ошибку записи (если она была), а «флаш не удался» на закрытом
		// соединении — обычный конец стрима, а не повод считать кадр потерянным.
		_ = s.bw.Flush()
	}
}

// newStreamSinkR493 — сток для этого ответа: hijack, если соединение уже
// захвачено вызывающим, иначе обычный ResponseWriter.
//
// РЕШЕНИЕ про hijack по-прежнему принимает вызывающий (у него есть `w` и
// `http.Hijacker`); сток лишь получает УЖЕ захваченное соединение. Так исчезает
// второй набор правил записи, но не переносится риск: поведение транспорта не
// меняется.
func newStreamSinkR493(w http.ResponseWriter) streamSinkR493 {
	return newPlainStreamSinkR493(w)
}
