//go:build llama_stub

// text_arguments_dump_r474_test.go — R474 (2026-11-07): вызов инструмента,
// выданный ТЕКСТОМ, не уезжает клиенту как ответ.
//
// ЖИВОЙ ДЕФЕКТ (Cline + Qwen3.8-9B, /api/chat): сводка хода —
// final_content_len=7095, tool_calls=0, file_body_instead_of_call=false.
// Клиент получил ~7 КБ JavaScript как ответ, а в конце — сериализованные
// аргументы («…"path":"d:\cline-e2e-web\public\app.js"}]}}»). Файл не создан.
package main

import (
	"strings"
	"testing"
)

// liveCodeDumpR474 — форма живого вывода: код в начале, аргументы в хвосте.
func liveCodeDumpR474() string {
	code := strings.Repeat(
		"ctx.strokeStyle = getComputedStyle(document.documentElement).getPropertyValue('--muted').trim();\n"+
			"const x = 13, y = 13;\n"+
			"ctx.beginPath(); ctx.moveTo(padding.left, yPos); ctx.lineTo(width - padding.right, yPos);\n"+
			"function drawChart(data) { data.forEach((v, i) => { ctx.fillRect(i, v, 4, v); }); }\n", 8)
	return code + "\n\"path\":\"d:\\\\cline-e2e-web\\\\public\\\\app.js\"}]}}"
}

// TestR474_DumpedArgumentsDetected — живая форма распознаётся.
func TestR474_DumpedArgumentsDetected(t *testing.T) {
	text := liveCodeDumpR474()
	hint, tool, ok := looksLikeDumpedArgumentsR474(text)
	if !ok {
		t.Fatalf("живая форма не распознана (len=%d, tail=%q)", len(text), text[len(text)-80:])
	}
	if !strings.Contains(hint, "app.js") {
		t.Errorf("в подсказке нет пути файла: %q", hint)
	}
	_ = tool // имя инструмента может отсутствовать в тексте — не обязательное поле
}

// TestR474_NoticeReplacesCode — вместо кода клиент получает пояснение, и в нём
// сказано, что файл не создан и что нужен вызов инструмента.
func TestR474_NoticeReplacesCode(t *testing.T) {
	notice := dumpedArgumentsNoticeR474("editor", "d:\\cline-e2e-web\\public\\app.js")
	for _, want := range []string{"ТЕКСТОМ", "editor", "app.js", "файл не создан", "следующем ходу"} {
		if !strings.Contains(notice, want) {
			t.Errorf("в пояснении нет %q: %s", want, notice)
		}
	}
	// Без имени инструмента текст остаётся осмысленным.
	if got := dumpedArgumentsNoticeR474("", ""); !strings.Contains(got, "инструмент записи файла") {
		t.Errorf("без имени инструмента потерян смысл: %s", got)
	}
}

// TestR474_PlainCodeAnswerIsNotTouched — ответ на вопрос «покажи код» без хвоста
// аргументов обязан остаться ответом: иначе мы начнём выкидывать нормальные
// ответы с кодом.
func TestR474_PlainCodeAnswerIsNotTouched(t *testing.T) {
	code := strings.Repeat("function f() { return 1; }\nconst a = f();\n", 60)
	if _, _, ok := looksLikeDumpedArgumentsR474(code); ok {
		t.Fatal("чистый код без аргументов вызова распознан как выгрузка вызова")
	}
}

// TestR474_JsonInProseIsNotTouched — проза с примером JSON (ключ "path" внутри
// объяснения) не должна считаться вызовом: нужны и код, и закрывающая структура.
func TestR474_JsonInProseIsNotTouched(t *testing.T) {
	prose := strings.Repeat(
		"В документации указано, что поле \"path\" задаёт файл, а \"content\" — содержимое. ", 40)
	if _, _, ok := looksLikeDumpedArgumentsR474(prose); ok {
		t.Fatal("проза с ключами JSON распознана как выгрузка аргументов")
	}
}

// TestR474_TinySnippetIsNotTouched — короткий фрагмент кода с хвостом не считается
// выгрузкой: порог по размеру защищает от ложных срабатываний.
func TestR474_TinySnippetIsNotTouched(t *testing.T) {
	short := "const a = 1;\n\"path\":\"x.js\"}]}}"
	if _, _, ok := looksLikeDumpedArgumentsR474(short); ok {
		t.Fatal("короткий фрагмент не должен считаться выгрузкой файла")
	}
}
