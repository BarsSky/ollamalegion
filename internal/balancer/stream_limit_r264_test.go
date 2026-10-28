// stream_limit_r264_test.go — R264 (2026-10-28): один лимит строки SSE на все пути.
//
// ЖИВОЙ КЛАСС ДЕФЕКТА (R259 закрыл его только в стриме). Сканеры объявлялись
// каждый по-своему: стрим — 1 МиБ (потом 8 МиБ через env), не-стрим — 1 МиБ
// хардкодом, self-proxy диалога и auto-stream/anthropic/responses — 4 МиБ. Один
// большой SSE-чанк (крупный аргумент tool-call, длинная строка кода) обрывал
// сканер, и ответ молча резался — без ошибки клиенту и без причины в логе.
//
// Тесты держат два свойства: (1) лимит настраивается одной переменной; (2) строка
// больше лимита — это НАШ лимит (`bufio.ErrTooLong`), а не обрыв upstream, и он
// распознаётся отдельно.
package balancer

import (
	"bufio"
	"bytes"
	"errors"
	"strings"
	"testing"
)

// TestR264_StreamMaxLineBytesIsConfigurable — лимит берётся из env, дефолт 8 МиБ,
// мусор и слишком маленькие значения игнорируются (не «отключают» поток).
func TestR264_StreamMaxLineBytesIsConfigurable(t *testing.T) {
	t.Setenv("LB_STREAM_MAX_LINE_BYTES", "")
	if got, want := streamMaxLineBytes(), streamMaxLineBytesDefault; got != want {
		t.Errorf("дефолт = %d, want %d", got, want)
	}

	t.Setenv("LB_STREAM_MAX_LINE_BYTES", "2097152")
	if got, want := streamMaxLineBytes(), 2097152; got != want {
		t.Errorf("override = %d, want %d", got, want)
	}

	// Мусор и значения ниже 64 КиБ игнорируются: «0» или опечатка не должны
	// превращать поток в поток ошибок.
	for _, bad := range []string{"мусор", "0", "-1", "1024"} {
		t.Setenv("LB_STREAM_MAX_LINE_BYTES", bad)
		if got := streamMaxLineBytes(); got != streamMaxLineBytesDefault {
			t.Errorf("для %q получен лимит %d, ожидался дефолт %d", bad, got, streamMaxLineBytesDefault)
		}
	}
}

// TestR264_NonStreamScannerAcceptsLargeLine — линия больше прежнего хардкода
// (1 МиБ), но в пределах нового лимита, обязана читаться.
//
// Это регресс-сторож ровно на тот дефект, который R264 убирает: не-стрим путь
// держал `1024*1024` и на таком чанке терял остаток ответа.
func TestR264_NonStreamScannerAcceptsLargeLine(t *testing.T) {
	t.Setenv("LB_STREAM_MAX_LINE_BYTES", "")

	big := "data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("x", 1536*1024) + "\"}}]}"
	if len(big) <= 1024*1024 {
		t.Fatalf("тест не воспроизводит дефект: строка %d байт, должна быть больше 1 МиБ", len(big))
	}

	// Так же, как в llamacpp_transport_nonstream.go после R264.
	scanner := bufio.NewScanner(bytes.NewReader([]byte(big)))
	scanner.Buffer(make([]byte, 0), streamMaxLineBytes())
	if !scanner.Scan() {
		t.Fatalf("сканер не прочитал строку %d байт при лимите %d: %v",
			len(big), streamMaxLineBytes(), scanner.Err())
	}
	if got := len(scanner.Text()); got != len(big) {
		t.Errorf("прочитано %d байт из %d — ответ обрезан", got, len(big))
	}

	// Контроль: со старым хардкодом 1 МиБ та же строка даёт ErrTooLong — именно
	// так дефект и выглядел в жизни.
	old := bufio.NewScanner(bytes.NewReader([]byte(big)))
	old.Buffer(make([]byte, 0), 1024*1024)
	if old.Scan() {
		t.Fatal("со старым лимитом 1 МиБ строка неожиданно прочиталась — тест перестал ловить дефект")
	}
	if serr := old.Err(); !errors.Is(serr, bufio.ErrTooLong) {
		t.Errorf("ожидался bufio.ErrTooLong, получено %v", serr)
	}
}
