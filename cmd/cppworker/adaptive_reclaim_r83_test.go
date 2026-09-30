//go:build llama_stub

// adaptive_reclaim_r83_test.go — R83-fix (2026-09-30): бюджет адаптивной
// стратегии для ПЕРЕЗАГРУЗКИ уже загруженной модели.
//
// Живой дефект: балансер спрашивает стратегию перед reload'ом модели, которая
// занимает VRAM прямо сейчас. env.FreeVRAM её не учитывает, поэтому стратегия
// отвечала stage=cpu_only/gpuLayers=0 для gemma-4, работавшей с 19 слоями на
// GPU (RTX 3070, 4629 MB свободных, веса 4.0 GiB). Дальше балансер честно
// выполнял этот «план»: выгрузка + загрузка = 2–3 минуты простоя без выигрыша.
package main

import (
	"os"
	"path/filepath"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

func loadedGemma() cppbackend.ModelInfo {
	return cppbackend.ModelInfo{
		Name:      "gemma-4-E4B-it-Q4_K_M",
		State:     cppbackend.StateLoaded,
		NLayers:   42,
		GPULayers: 19,
		SizeBytes: 4215695776,
	}
}

// TestR83Fix_ReclaimableVRAM_LoadedModel — загруженная модель возвращает в
// бюджет свою VRAM: без этого на 8 GB карте стратегия видит «свободно 4.6 GB,
// веса 4.0 GiB + KV + overhead» и уходит в cpu_only при живой GPU-раскладке.
func TestR83Fix_ReclaimableVRAM_LoadedModel(t *testing.T) {
	got := reloadReclaimableVRAM([]cppbackend.ModelInfo{loadedGemma()}, "gemma-4-E4B-it-Q4_K_M")
	if got == 0 {
		t.Fatal("reclaim = 0 для загруженной модели — стратегия снова уйдёт в cpu_only " +
			"и балансер выгрузит работающую GPU-модель")
	}
	t.Logf("reclaim = %d MB", got/(1024*1024))
}

// TestR83Fix_ReclaimableVRAM_RegisteredByName — имя с диска и внешнее имя
// (регистр/расширение) должны разрешаться в одну запись.
func TestR83Fix_ReclaimableVRAM_RegisteredByName(t *testing.T) {
	if got := reloadReclaimableVRAM([]cppbackend.ModelInfo{loadedGemma()}, "GEMMA-4-E4B-IT-Q4_K_M"); got == 0 {
		t.Error("reclaim = 0 при другом регистре имени — reload снова без бюджета модели")
	}
	// Живой случай: handleAdaptiveStrategy резолвит внешнее имя в canonical
	// filename с .gguf, а /api/models отдаёт имя без расширения.
	if got := reloadReclaimableVRAM([]cppbackend.ModelInfo{loadedGemma()}, "gemma-4-E4B-it-Q4_K_M.gguf"); got == 0 {
		t.Error("reclaim = 0 для canonical имени с .gguf — именно на этом " +
			"сломался первый вариант фикса (проверено живым логом)")
	}
}

// TestR83Fix_ReclaimableVRAM_SizeFallbackViaStat — SizeBytes у cppbackend часто
// 0 (размер берётся из gguf-меты, которая заполнена не всегда): без os.Stat
// reclaim молча не срабатывал — именно это и наблюдалось на живом стенде v36.
func TestR83Fix_ReclaimableVRAM_SizeFallbackViaStat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gemma-4-E4B-it-Q4_K_M.gguf")
	if err := os.WriteFile(path, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatalf("не удалось создать файл-заглушку: %v", err)
	}
	m := loadedGemma()
	m.SizeBytes = 0 // как отдаёт /api/models, когда gguf-размер пуст
	m.Path = path

	if got := reloadReclaimableVRAM([]cppbackend.ModelInfo{m}, m.Name); got == 0 {
		t.Error("reclaim = 0 при SizeBytes=0 и существующем Path — os.Stat-fallback " +
			"не сработал, бюджет reload'а снова без памяти модели")
	}
}

// TestR83Fix_ReclaimableVRAM_NotLoaded — незагруженная модель ничего не
// освобождает: бюджет не должен раздуваться.
func TestR83Fix_ReclaimableVRAM_NotLoaded(t *testing.T) {
	m := loadedGemma()
	m.State = cppbackend.StateLoading
	if got := reloadReclaimableVRAM([]cppbackend.ModelInfo{m}, m.Name); got != 0 {
		t.Errorf("reclaim = %d для модели в state=loading, ожидался 0", got)
	}
}

// TestR83Fix_ReclaimableVRAM_OtherModel — чужая модель в бюджете не участвует.
func TestR83Fix_ReclaimableVRAM_OtherModel(t *testing.T) {
	if got := reloadReclaimableVRAM([]cppbackend.ModelInfo{loadedGemma()}, "Qwen3.8-27B-UD-Q4_K_M"); got != 0 {
		t.Errorf("reclaim = %d для другой модели, ожидался 0", got)
	}
	if got := reloadReclaimableVRAM([]cppbackend.ModelInfo{loadedGemma()}, ""); got != 0 {
		t.Errorf("reclaim = %d для пустого имени, ожидался 0", got)
	}
}
