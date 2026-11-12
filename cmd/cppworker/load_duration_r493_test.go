// load_duration_r493_test.go — R493-12a (2026-11-12): воркер сообщает, сколько
// отдал на загрузку модели.
//
// ЖИВОЙ ЗАМЕР на стенде: клиент ждал первый кадр 40.12 с, а в done-кадре было
// `load_duration=0`, `total_duration=1.37 с` — 40 секунд ленивой загрузки
// (ensureModelLoadedWithNCtx внутри запроса) не попадали ни в одно поле.
//
// Здесь проверяется механизм: ячейка в контексте запроса, её заполнение
// измерением и то, что кадры действительно берут значение ИЗ НЕЁ (иначе код
// снова разойдётся: измерение есть, а в кадр не попадает).
package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestR493_LoadScopeRecords(t *testing.T) {
	ctx := withLoadScopeR493(context.Background())
	if loadDurationNsR493(ctx) != 0 {
		t.Fatalf("свежая ячейка не пуста: %d", loadDurationNsR493(ctx))
	}
	loadScopeFromCtxR493(ctx).markLoadR493(40 * time.Second)
	if got := loadDurationNsR493(ctx); got != int64(40*time.Second) {
		t.Fatalf("loadDurationNsR493 = %d, ожидалось 40 с", got)
	}
	// Отрицательная/нулевая длительность не должна портить значение.
	loadScopeFromCtxR493(ctx).markLoadR493(0)
	if got := loadDurationNsR493(ctx); got != int64(40*time.Second) {
		t.Fatalf("нулевое измерение перезаписало прежнее: %d", got)
	}
}

func TestR493_LoadScopeIdempotent(t *testing.T) {
	ctx := withLoadScopeR493(context.Background())
	ctx2 := withLoadScopeR493(ctx)
	if loadScopeFromCtxR493(ctx) != loadScopeFromCtxR493(ctx2) {
		t.Fatal("повторный withLoadScopeR493 создал новую ячейку: измерение в одной, кадр читает другую")
	}
}

func TestR493_LoadScopeAbsentIsZero(t *testing.T) {
	if got := loadDurationNsR493(context.Background()); got != 0 {
		t.Fatalf("без ячейки ожидался 0, получено %d", got)
	}
	if got := loadDurationNsR493(nil); got != 0 {
		t.Fatalf("nil-контекст: ожидался 0, получено %d", got)
	}
}

// TestR493_DoneChunksReadLoadDuration — сторож подключения: кадры собираются с
// loadDurationNsR493(r.Context()) и обработчики ставят ячейку ДО загрузки.
func TestR493_DoneChunksReadLoadDuration(t *testing.T) {
	read := func(path string) string {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("не прочитать %s: %v", path, err)
		}
		return string(data)
	}
	chat := read("handlers_chat.go")
	if n := strings.Count(chat, `"load_duration":        loadDurationNsR493(r.Context()),`); n != 2 {
		t.Fatalf("в handlers_chat.go кадров с load_duration: %d, ожидалось 2 (обычный и tool-путь)", n)
	}
	if n := strings.Count(chat, "withLoadScopeR493(r.Context())"); n < 1 {
		t.Fatal("handleChat не ставит ячейку загрузки: измерение уйдёт в никуда")
	}
	gen := read("handlers_generate.go")
	if n := strings.Count(gen, "loadDurationNsR493(r.Context())"); n != 3 {
		t.Fatalf("в handlers_generate.go обращений к ячейке: %d, ожидалось 3 "+
			"(два стрим-кадра + нег-стрим ответ /api/ollama/generate)", n)
	}
	if n := strings.Count(gen, `"load_duration":`); n != 3 {
		t.Fatalf("кадров с полем load_duration: %d, ожидалось 3", n)
	}
	if n := strings.Count(gen, "withLoadScopeR493(r.Context())"); n != 2 {
		t.Fatalf("ячеек загрузки в handlers_generate.go: %d, ожидалось 2 (handleGenerate и handleOllamaGenerate)", n)
	}
	if !strings.Contains(gen, "loadModelMeasuredR493(") {
		t.Fatal("runGenerateCore не измеряет загрузку через loadModelMeasuredR493 — длительность останется нулём")
	}
	// Заголовок нужен там, где в теле ответа длительностей нет вовсе —
	// OpenAI-поверхность (/v1/*), через которую балансер и говорит с воркером.
	for _, path := range []string{"handlers_chat.go", "handlers_generate.go", "handlers_openai.go"} {
		if !strings.Contains(read(path), "markLoadHeaderR493(") {
			t.Fatalf("%s не выставляет заголовок загрузки: на OpenAI-поверхности "+
				"балансер не узнает, что модель грузилась", path)
		}
	}
	openai := read("handlers_openai.go")
	if n := strings.Count(openai, "loadModelMeasuredR493("); n != 2 {
		t.Fatalf("в handlers_openai.go измерений: %d, ожидалось 2 (v1/chat и v1/completions)", n)
	}
}
