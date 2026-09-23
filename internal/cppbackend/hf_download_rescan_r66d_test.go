//go:build llama_stub

// hf_download_rescan_r66d_test.go — R66d (2026-09-23).
//
// РЕГРЕСС. ModelManager.ListModels()/GetModelMeta()/FindModelByPath() работают
// по кэшу ggufFiles, который наполняется только в ScanModels(). Путь HF-загрузки
// (POST /api/hf/download) его не обновлял, поэтому скачанная модель не появлялась
// ни в GET /api/models/files (вкладка GGUF в WebUI), ни в списке моделей, которые
// можно загрузить, — «скачали, а модели нет» до рестарта cppworker.
//
// Живой кейс: скачивание gemma-4 (unsloth QAT, 4.2 ГБ) через /api/hf/download
// завершилось успешно, файл лежал в /app/models, но /api/models/files его не
// показывал, а POST /api/create отвечал «source model not found».

package cppbackend

import (
	"path/filepath"
	"testing"
	"time"
)

func TestHFDownloadComplete_RescansModelsDir_R66d(t *testing.T) {
	dir := t.TempDir()
	b := NewBackend(Config{
		ModelsDir:        dir,
		DefaultCtxSize:   512,
		DefaultBatchSize: 64,
	})
	t.Cleanup(func() {
		if mm := b.ModelManager(); mm != nil {
			mm.WaitForPendingWrites(2 * time.Second)
		}
	})

	// Предусловие: скан видит первый файл.
	if err := writeMinimalFile(filepath.Join(dir, "existing.gguf"), 4096); err != nil {
		t.Fatalf("writeMinimalFile: %v", err)
	}
	if _, err := b.ModelManager().ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}

	names := func() map[string]bool {
		out := make(map[string]bool)
		for _, m := range b.ModelManager().ListModels() {
			out[m.Filename] = true
		}
		return out
	}
	if !names()["existing.gguf"] {
		t.Fatalf("предусловие: ScanModels не увидел existing.gguf: %v", names())
	}

	// Свежий файл появляется в каталоге (как после HF-загрузки) и НЕ виден в кэше.
	const fresh = "gemma-4-E4B-it-qat-UD-Q4_K_XL.gguf"
	if err := writeMinimalFile(filepath.Join(dir, fresh), 8192); err != nil {
		t.Fatalf("writeMinimalFile: %v", err)
	}
	if names()[fresh] {
		t.Fatal("предусловие не выполнено: свежий файл уже в кэше — тест ничего не проверяет")
	}

	// Хук, который Backend вешает на загрузчик (NewBackend → SetOnDownloadComplete).
	b.HFDownloader().notifyDownloadComplete(fresh)

	if !names()[fresh] {
		t.Fatalf("после notifyDownloadComplete модель не появилась в ListModels: %v", names())
	}
}
