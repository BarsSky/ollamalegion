package main

// chat_sources_r627_test.go — R627 (2026-11-19): единый способ читать исходники
// «поверхности чата» в сторожах.
//
// ЗАЧЕМ. За R625–R627 `handlers_chat.go` разложен на четыре файла:
//
//	handlers_chat.go                   — HTTP-поверхность чата и обычный стрим
//	handlers_chat_prompt.go            — сборка промпта и параметров (R625)
//	handlers_chat_stream_tools.go      — стрим-ответ с инструментами (R626)
//	handlers_chat_stream_generation.go — генерация инструментов и журнал (R627)
//
// Сторожа, которые читают ОДИН файл по имени, после каждого такого переноса падают
// с ложной тревогой «дефект вернулся» (так было в R623, R626 и снова здесь). Поэтому
// чтение поверхности чата собрано в одну функцию: она читает ВСЕ `handlers_chat*.go`
// и склеивает их. Тогда перенос кода между этими файлами больше не ломает сторожей,
// а сами проверки остаются такими же строгими.
//
// `go/ast`-вариант возвращает объединённые Decls — для сторожей, которые ищут
// объявление функции по имени.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chatSourceFilesR627 — все файлы поверхности чата в текущем каталоге.
func chatSourceFilesR627(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob("handlers_chat*.go")
	if err != nil {
		t.Fatalf("glob handlers_chat*.go: %v", err)
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if strings.HasSuffix(m, "_test.go") {
			continue
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		t.Fatal("не найдено ни одного handlers_chat*.go — сторож потерял точку отсчёта")
	}
	return out
}

// readChatSurfaceR627 — склеенный текст всех файлов поверхности чата.
func readChatSurfaceR627(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, name := range chatSourceFilesR627(t) {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("не читается %s: %v", name, err)
		}
		b.Write(data)
		b.WriteString("\n")
	}
	return b.String()
}

// parseChatSurfaceR627 — объединённое AST всех файлов поверхности чата.
func parseChatSurfaceR627(t *testing.T) *ast.File {
	t.Helper()
	fset := token.NewFileSet()
	var merged *ast.File
	for _, name := range chatSourceFilesR627(t) {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("разбор %s: %v", name, err)
		}
		if merged == nil {
			merged = f
			continue
		}
		merged.Decls = append(merged.Decls, f.Decls...)
	}
	if merged == nil {
		t.Fatal("не удалось разобрать поверхность чата")
	}
	return merged
}
