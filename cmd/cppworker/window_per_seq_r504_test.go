//go:build llama_stub

// window_per_seq_r504_test.go — R504 (2026-11-19), аудит S4, пункт N-14.
//
// ДЕФЕКТ. `loadedModelNCtx` возвращал `ContextSize` — СУММАРНОЕ окно модели — и
// подставлял его как «окно запроса», когда клиент не прислал `num_ctx`
// (`NCtxOverride<=0`). При `parallel>1` слот получает `n_ctx/parallel`, то есть
// ровно вдвое (втрое…) меньше: проверка пропускала промпт, а слот его не
// вмещал — отказ приходил уже ПОСЛЕ HTTP 200, и клиент видел
// `finish_reason=error` с пустым ответом (класс «пустой ответ», ловится только
// классификацией декода в C-bridge).
package main

import (
	"os"
	"strings"
	"testing"
)

// TestR504_PerSeqUsesPublishedSlotWindow — N-14: если воркер опубликовал окно на
// слот (ContextPerSeq, R83), берём именно его.
func TestR504_PerSeqUsesPublishedSlotWindow(t *testing.T) {
	if got := perSeqWindowR504(16384, 8192, 2); got != 8192 {
		t.Fatalf("окно запроса = %d, ожидалось 8192 (окно слота из ContextPerSeq)", got)
	}
	// Опубликованное окно главнее собственного деления: воркер учитывает
	// выравнивание GGML_PAD, и его число точнее.
	if got := perSeqWindowR504(16384, 8448, 2); got != 8448 {
		t.Fatalf("окно запроса = %d, ожидалось 8448 (ContextPerSeq точнее деления)", got)
	}
}

// TestR504_PerSeqDividesWhenUnpublished — N-14: ContextPerSeq нет (старая сборка) —
// делим суммарное окно на число слотов, иначе обещаем слоту лишнее.
func TestR504_PerSeqDividesWhenUnpublished(t *testing.T) {
	const total = 16384
	for _, parallel := range []int{2, 4} {
		got := perSeqWindowR504(total, 0, parallel)
		if want := total / parallel; got != want {
			t.Fatalf("parallel=%d: окно запроса = %d, ожидалось %d (суммарное окно "+
				"нельзя отдавать одному слоту — аудит N-14)", parallel, got, want)
		}
		if got >= total {
			t.Fatalf("parallel=%d: окно запроса = %d не должно равняться суммарному", parallel, got)
		}
	}
}

// TestR504_PerSeqSingleSlotUnchanged — один слот: суммарное окно и есть окно слота.
//
// TestR504_LoadedModelNCtxUsesPerSeq — N-14 (связка): сама подстановка окна
// запроса обязана идти через perSeqWindowR504, а не отдавать ContextSize.
//
// Почему проверка по исходнику: `loadedModelNCtx` читает глобальный `backend`,
// который в stub-сборке не поднимает модель (GetModel → ошибка), поэтому
// поведенческий тест связки невозможен без подмены всего backend'а. Свойство,
// которое надо удержать, — «окно запроса считается по слоту»: оно проверено
// выше на чистой функции, а здесь — что вызов действительно стоит.
func TestR504_LoadedModelNCtxUsesPerSeq(t *testing.T) {
	src, err := os.ReadFile("inference.go")
	if err != nil {
		t.Fatalf("чтение inference.go: %v", err)
	}
	text := string(src)
	start := strings.Index(text, "func loadedModelNCtx(")
	if start < 0 {
		t.Fatal("loadedModelNCtx не найден в inference.go")
	}
	end := strings.Index(text[start:], "\n}")
	if end < 0 {
		t.Fatal("не найдена граница функции loadedModelNCtx")
	}
	body := text[start : start+end]
	if !strings.Contains(body, "perSeqWindowR504(") {
		t.Fatal("loadedModelNCtx считает окно запроса не через perSeqWindowR504: " +
			"суммарное окно снова подставляется как окно слота (аудит N-14)")
	}
	if strings.Contains(body, "return info.ContextSize") {
		t.Fatal("loadedModelNCtx возвращает СУММАРНОЕ окно (ContextSize) как окно " +
			"запроса — при parallel>1 промпт не влезает в слот (аудит N-14)")
	}
}
func TestR504_PerSeqSingleSlotUnchanged(t *testing.T) {
	if got := perSeqWindowR504(16384, 0, 1); got != 16384 {
		t.Fatalf("один слот: окно = %d, ожидалось 16384", got)
	}
	if got := perSeqWindowR504(16384, 0, 0); got != 16384 {
		t.Fatalf("parallel=0 (дефолт 1): окно = %d, ожидалось 16384", got)
	}
	if got := perSeqWindowR504(0, 0, 4); got != 0 {
		t.Fatalf("окно неизвестно: получено %d, ожидалось 0", got)
	}
}
