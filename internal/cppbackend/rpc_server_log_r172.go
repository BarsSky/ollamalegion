// rpc_server_log_r172.go — R172 (2026-10-18, разбор падения ВСТРОЕННОГО узла).
//
// ЗАЧЕМ. R170–R171 поймали живую аварию раскладки между машинами: мастер
// подключает донора как ggml-устройство, и на загрузке модели клиент падает с
//
//	ggml-rpc.cpp:566 "Remote RPC server crashed or returned malformed response"
//	+ Aborted
//
// Сообщение это КЛИЕНТСКОЕ и врёт в обе стороны (см. ggml-rpc.cpp:35 —
// RPC_STATUS_ASSERT поднимает его на ЛЮБОЙ неуспешный ответ, а сервер
// закрывает сессию на первой же неудачной операции, ggml-rpc.cpp:1857+).
// Настоящая причина (нехватка VRAM, регион тензора вне буфера, неизвестный тип,
// рассинхрон протокола) пишется сервером в stderr через GGML_LOG_ERROR — и до
// сих пор пропадала: дочерний процесс писал в stdout контейнера, а этот вывод
// никто не сохранял и не показывал.
//
// ЧТО ЗДЕСЬ. Кольцевой буфер последних строк дочернего процесса плюс
// необязательный отладочный режим (GGML_RPC_DEBUG=1) — тот включает в ggml-rpc
// построчный LOG_DBG на каждую операцию протокола. Буфер не «теряется при
// падении»: он живёт в памяти воркера, поэтому последние слова узла доступны и
// после его смерти — в отличие от docker logs, которые перезапись образа
// контейнера (как случилось при обновлении донора) уже не вернёт.
package cppbackend

import (
	"bytes"
	"io"
	"sync"
	"time"
)

const (
	// rpcServerLogMaxLines — сколько последних строк держим. Достаточно, чтобы
	// увидеть начало аварии (ошибка аллокации + несколько предшествующих
	// операций), и мало, чтобы не выглядеть утечкой памяти.
	rpcServerLogMaxLines = 2000

	// rpcServerLogMaxLineLen — предел на строку. ggml-rpc печатает размеры
	// тензоров и указатели; строки короткие, но зависшая запись без \n не
	// должна съесть память.
	rpcServerLogMaxLineLen = 512

	// rpcServerLogMaxBytes — предел на накопленный фрагмент недописанной строки.
	rpcServerLogMaxBytes = 8 * 1024
)

// RPCServerLogLine — одна строка вывода узла.
//
// Время — момент, когда строка доехала до воркера (а не время внутри rpc-server:
// у того в строке времени нет). Префикс GGML_LOG_ERROR/DEBUG остаётся в тексте —
// по нему и видно, ошибка это или отладка.
type RPCServerLogLine struct {
	At   string `json:"at"`
	Text string `json:"text"`
}

// rpcServerLog — io.Writer для stdout/stderr дочернего процесса.
//
// Один экземпляр обслуживает ОБА потока дочернего процесса (cmd.Stdout и
// cmd.Stderr указывают сюда): строки разных потоков попадают в общий буфер в
// порядке arrivals, и восстановить хронологию можно по времени. Дублирование в
// sink (обычно os.Stdout контейнера) сохранено, чтобы `docker logs` продолжал
// работать как раньше — новый буфер ничего не отнимает у оператора.
type rpcServerLog struct {
	mu   sync.Mutex
	rows []RPCServerLogLine
	// dropped — сколько строк вытеснено переполнением. Показываем счётчиком, а не
	// прячем: «лог обрезан» — это факт, который оператор должен видеть.
	dropped int
	partial []byte
	sink    io.Writer
}

func newRPCServerLog(sink io.Writer) *rpcServerLog {
	return &rpcServerLog{
		rows: make([]RPCServerLogLine, 0, 64),
		sink: sink,
	}
}

// Write реализует io.Writer. Всегда возвращает полную длину: сбой дублирования
// в docker-log не должен ронять дочерний процесс (иначе мы получим ровно ту
// ошибку «pipe closed», которую разбираем).
func (l *rpcServerLog) Write(p []byte) (int, error) {
	if l == nil {
		return len(p), nil
	}
	n := len(p)
	if l.sink != nil {
		_, _ = l.sink.Write(p)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.partial = append(l.partial, p...)
	for {
		i := bytes.IndexByte(l.partial, '\n')
		if i < 0 {
			break
		}
		l.appendLocked(string(bytes.TrimRight(l.partial[:i], "\r")))
		rest := append([]byte(nil), l.partial[i+1:]...)
		l.partial = rest
	}
	// Недописанная строка не должна расти бесконечно: обрезаем и сбрасываем в
	// буфер — лучше показать обрезанное, чем вообще ничего.
	if len(l.partial) > rpcServerLogMaxBytes {
		l.appendLocked(string(bytes.TrimRight(l.partial[:rpcServerLogMaxBytes], "\r")) + "…(строка обрезана)")
		l.partial = l.partial[:0]
	}
	return n, nil
}

// appendLocked — добавление строки с вытеснением старейшей (вызывается под mu).
func (l *rpcServerLog) appendLocked(text string) {
	if len(text) > rpcServerLogMaxLineLen {
		text = text[:rpcServerLogMaxLineLen] + "…(обрезано)"
	}
	if len(l.rows) >= rpcServerLogMaxLines {
		copy(l.rows, l.rows[1:])
		l.rows = l.rows[:len(l.rows)-1]
		l.dropped++
	}
	l.rows = append(l.rows, RPCServerLogLine{
		At:   time.Now().UTC().Format(time.RFC3339Nano),
		Text: text,
	})
}

// Tail — последние n строк (при n<=0 — TailDefault). Порядок хронологический.
func (l *rpcServerLog) Tail(n int) []RPCServerLogLine {
	if l == nil {
		return nil
	}
	if n <= 0 {
		n = rpcServerLogTailDefault
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if n > len(l.rows) {
		n = len(l.rows)
	}
	out := make([]RPCServerLogLine, n)
	copy(out, l.rows[len(l.rows)-n:])
	return out
}

// Count — сколько строк сейчас в буфере.
func (l *rpcServerLog) Count() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.rows)
}

// Dropped — сколько строк вытеснено переполнением.
func (l *rpcServerLog) Dropped() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dropped
}

// rpcServerLogTailDefault — сколько строк отдаём в статусе воркера и в отчёте
// мастеру. Полная история — по ручке /api/v1/cppworker/rpc-server/log.
const rpcServerLogTailDefault = 10
