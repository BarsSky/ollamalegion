//go:build llama_stub

// dockerfiles_patch_guard_r226_test.go — R226 (2026-10-22).
//
// ЗАЧЕМ. Патчи к сабмодулю llama.cpp (keepalive, graceful connect, потеря сессии)
// применяются на сборке образа. Если в `docker/cppworker/` появится Dockerfile,
// который копирует `c/llama.cpp`, но НЕ прогоняет каталог патчей, собранный образ
// молча теряет все эти правки: мастер снова падает по SIGABRT, keepalive не
// работает, и никто не поймёт почему. Живой пример — `Dockerfile.gpu.fixed`
// (удалён в R226): копировал llama.cpp и патчей не применял.
//
// Тест обходит ВСЕ файлы `docker/cppworker/Dockerfile*`, поэтому новый вариант
// сборки нельзя добавить «забыв» патчи.
package cppbackend

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestR226_EveryLlamaDockerfileAppliesPatches(t *testing.T) {
	dir := "../../docker/cppworker"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("не читается %s: %v", dir, err)
	}
	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "Dockerfile") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("не читается %s: %v", e.Name(), err)
		}
		text := string(raw)
		if !strings.Contains(text, "COPY c/llama.cpp") {
			continue // stub/CPU-сборки без сабмодуля — не наш случай
		}
		checked++
		if !strings.Contains(text, "COPY docker/cppworker/patches /tmp/patches") {
			t.Errorf("%s: не копирует каталог патчей — собранный образ потеряет R214/R218/R221", e.Name())
		}
		if !strings.Contains(text, "for p in /tmp/patches/*.patch") {
			t.Errorf("%s: не применяет все патчи каталога (ожидается `for p in /tmp/patches/*.patch`)", e.Name())
		}
		if !strings.Contains(text, `git apply "$p"`) {
			t.Errorf("%s: патчи не применяются через git apply", e.Name())
		}
	}
	if checked == 0 {
		t.Fatal("не найдено ни одного Dockerfile с COPY c/llama.cpp — проверка ничего не проверила")
	}
	t.Logf("проверено Dockerfile'ов с llama.cpp: %d", checked)
}

// Каталог патчей обязан быть нумерованным: порядок применения задаётся именами,
// и «случайное» имя вроде fix.patch может примениться раньше/позже нужного.
func TestR226_PatchNamesAreOrdered(t *testing.T) {
	dir := "../../docker/cppworker/patches"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("не читается %s: %v", dir, err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".patch") {
			continue
		}
		if len(e.Name()) < 4 || e.Name()[0] < '0' || e.Name()[0] > '9' {
			t.Errorf("%s: имя патча должно начинаться с номера (0001-, 0002-, ...)", e.Name())
		}
		prefix := e.Name()[:4]
		if seen[prefix] {
			t.Errorf("префикс %s повторяется — порядок применения неоднозначен", prefix)
		}
		seen[prefix] = true
	}
	if len(seen) == 0 {
		t.Fatal("в каталоге патчей нет ни одного .patch")
	}
}
