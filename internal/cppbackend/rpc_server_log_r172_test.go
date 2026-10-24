//go:build llama_stub

// rpc_server_log_r172_test.go — R172 (2026-10-18).
//
// Проверяется СЕРВЕРНАЯ ПОЛОВИНА наблюдаемости узла: вывод дочернего процесса
// сохраняется в памяти воркера (а не только уходит в docker logs контейнера,
// где он исчезал при перезаписи образа).
//
// ПОЧЕМУ ЭТО НЕ КОСМЕТИКА. Клиент раскладки на любую неудачную операцию
// печатает одно и то же — ggml-rpc.cpp:35 «Remote RPC server crashed or returned
// malformed response», — а настоящую причину пишет сервер и только в stderr
// (GGML_LOG_ERROR). Пока эти строки терялись, разбор аварии шёл по догадкам.
package cppbackend

import (
	"bytes"
	"strings"
	"testing"
)

// TestR172_Log_LineSplitting — строки режутся по \n, недописанный хвост ждёт
// следующей записи (дочерний процесс пишет в поток кусками, а не строками).
func TestR172_Log_LineSplitting(t *testing.T) {
	var sink bytes.Buffer
	l := newRPCServerLog(&sink)

	if _, err := l.Write([]byte("первая\nвторая\nтрет")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	tail := l.Tail(10)
	if len(tail) != 2 {
		t.Fatalf("строк в буфере %d, want 2 (третья строка ещё не завершена): %+v", len(tail), tail)
	}
	if tail[0].Text != "первая" || tail[1].Text != "вторая" {
		t.Fatalf("порядок/текст строк нарушен: %+v", tail)
	}

	// Завершаем третью строку — она должна появиться целиком, а не обрывком.
	if _, err := l.Write([]byte("ья\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	tail = l.Tail(10)
	if len(tail) != 3 || tail[2].Text != "третья" {
		t.Fatalf("недописанная строка собралась неверно: %+v", tail)
	}

	// Дублирование в sink (stdout контейнера) обязано сохраниться: `docker logs`
	// никуда не делся, буфер — добавка, а не замена.
	if !strings.Contains(sink.String(), "первая") || !strings.Contains(sink.String(), "третья") {
		t.Errorf("в sink попало не всё: %q", sink.String())
	}
}

// TestR172_Log_TailIsChronologicalAndBounded — Tail(n) отдаёт ПОСЛЕДНИЕ n строк в
// хронологическом порядке (по нему оператор читает «что было перед падением»).
func TestR172_Log_TailIsChronologicalAndBounded(t *testing.T) {
	l := newRPCServerLog(nil)
	for _, s := range []string{"a", "b", "c", "d", "e"} {
		_, _ = l.Write([]byte(s + "\n"))
	}
	tail := l.Tail(2)
	if len(tail) != 2 || tail[0].Text != "d" || tail[1].Text != "e" {
		t.Fatalf("Tail(2) = %+v, want [d e]", tail)
	}
	if all := l.Tail(0); len(all) != rpcServerLogTailDefault && len(all) != 5 {
		t.Errorf("Tail(0) вернул %d строк", len(all))
	}
	if got := l.Count(); got != 5 {
		t.Errorf("Count = %d, want 5", got)
	}
}

// TestR172_Log_RingKeepsLastLinesOnly — при переполнении выживают ПОСЛЕДНИЕ
// строки, а факт вытеснения виден счётчиком (молча обрезанный лог — это ложь
// «ничего не было»).
func TestR172_Log_RingKeepsLastLinesOnly(t *testing.T) {
	l := newRPCServerLog(nil)
	total := rpcServerLogMaxLines + 37
	for i := 0; i < total; i++ {
		_, _ = l.Write([]byte("строка\n"))
	}
	if got := l.Count(); got != rpcServerLogMaxLines {
		t.Fatalf("Count = %d, want %d", got, rpcServerLogMaxLines)
	}
	if got := l.Dropped(); got != 37 {
		t.Fatalf("Dropped = %d, want 37 (вытеснение обязано быть видно)", got)
	}
	tail := l.Tail(1)
	if len(tail) != 1 || tail[0].Text != "строка" {
		t.Fatalf("хвост потерян: %+v", tail)
	}
}

// TestR172_Log_LongLineTruncated — строка без \n не съедает память: она
// обрезается, и об этом сказано в тексте.
func TestR172_Log_LongLineTruncated(t *testing.T) {
	l := newRPCServerLog(nil)
	huge := strings.Repeat("x", rpcServerLogMaxLineLen*3)
	_, _ = l.Write([]byte(huge))
	_, _ = l.Write([]byte("\n"))
	tail := l.Tail(1)
	if len(tail) != 1 {
		t.Fatalf("строк нет: %+v", tail)
	}
	if len(tail[0].Text) > rpcServerLogMaxLineLen+32 {
		t.Errorf("строка длиной %d не обрезана", len(tail[0].Text))
	}
	if !strings.Contains(tail[0].Text, "обрезано") {
		t.Errorf("обрезка не отмечена в тексте: %q", tail[0].Text)
	}
}

// TestR172_Log_NilIsSafe — статус могут читать у ненастроенного узла (nil), и
// это не должно ронять воркер.
func TestR172_Log_NilIsSafe(t *testing.T) {
	var l *rpcServerLog
	if _, err := l.Write([]byte("x")); err != nil {
		t.Errorf("Write у nil: %v", err)
	}
	if got := l.Tail(5); got != nil {
		t.Errorf("Tail у nil = %+v", got)
	}
	if l.Count() != 0 || l.Dropped() != 0 {
		t.Errorf("Count/Dropped у nil не нулевые")
	}
}
