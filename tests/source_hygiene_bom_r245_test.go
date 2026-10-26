package tests

// source_hygiene_bom_r245_test.go — R245 (2026-10-27).
//
// ЧТО ЛОВИТ. Go-файлы репозитория, начинающиеся с UTF-8 BOM (EF BB BF).
//
// ПОЧЕМУ ЭТО ДЕФЕКТ, А НЕ СТИЛЬ. `go build` и `go test` такой файл принимают
// (BOM в начале допустим), но `go test -cover` — нет: инструментатор покрытия
// печатает файл заново, BOM оказывается уже не первым байтом, и сборка падает:
//
//	internal/balancer/llamacpp_content_filter.go:1:1: invalid BOM in the middle of the file
//
// Ровно это случилось в CI-шаге «Generate coverage» (go 1.25): шаг годами стоял
// за `if: success()`, поэтому дефект был невидим, пока тесты падали раньше него.
// Проверено минимальным примером: файл с BOM + `go test -cover` → та же ошибка.
//
// ВТОРАЯ ПРИЧИНА. BOM бесполезен: кодировка Go-исходников — UTF-8 по
// определению, а лишние три байта ломают сторонние инструменты (gofmt-диффы,
// регулярки по первой строке, генераторы).
//
// РЕШЕНИЕ. BOM удалён из всех .go (27 файлов в internal/, cmd/, tests/), а тест
// не даёт ему вернуться: редакторы на Windows любят добавлять его молча.

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bomPrefix — UTF-8 BOM.
var bomPrefix = []byte{0xEF, 0xBB, 0xBF}

func TestR245_NoUTF8BOMInGoSources(t *testing.T) {
	root := ".."
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("не найден go.mod рядом с tests/ (%v) — тест запускается из tests/", err)
	}

	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "dist", "vendor", "c":
				// c/ — сабмодуль llama.cpp, его исходники не наши.
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		head := make([]byte, len(bomPrefix))
		n, _ := f.Read(head)
		if n == len(bomPrefix) && bytes.Equal(head, bomPrefix) {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("обход дерева исходников: %v", err)
	}

	if len(offenders) > 0 {
		t.Fatalf("Go-файлы с UTF-8 BOM ломают `go test -cover` (error: invalid BOM in the middle of the file).\n"+
			"Удалите BOM (EF BB BF) из %d файл(ов):\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}
