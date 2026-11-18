//go:build llama_stub

// handler_sources_r510_test.go — R510 (2026-11-19), шаг R3 плана читаемости.
//
// ЗАЧЕМ. Несколько сторожей проверяют код по ИСХОДНИКУ и читали конкретный файл
// `handlers_openai.go`. После разбиения этого файла на `handlers_openai*.go`
// (chat/stream/completions/models/usage) такие сторожа «потеряли точку отсчёта» и
// падали, хотя проверяемое свойство не менялось.
//
// РЕШЕНИЕ: сторож читает ПАКЕТНЫЙ набор файлов по префиксу и склеивает их —
// проверки (счётчики, запрещённые конструкции) остаются теми же, меняется только
// способ собрать текст. Это не ослабление: если код пропадёт из любого из файлов
// набора, проверка по-прежнему провалится.
package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// readHandlerSourcesR510 — склейка всех файлов пакета, имя которых начинается с
// prefix и которые не являются тестами. Порядок — алфавитный (детерминизм).
func readHandlerSourcesR510(t *testing.T, prefix string) string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("чтение каталога пакета: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		t.Fatalf("не найдено ни одного файла с префиксом %q — сторож потерял точку отсчёта", prefix)
	}
	sort.Strings(names)
	var sb strings.Builder
	for _, n := range names {
		raw, readErr := os.ReadFile(filepath.Clean(n))
		if readErr != nil {
			t.Fatalf("чтение %s: %v", n, readErr)
		}
		sb.Write(raw)
		sb.WriteString("\n")
	}
	return sb.String()
}
