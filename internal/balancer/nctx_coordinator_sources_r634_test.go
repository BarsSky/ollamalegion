//go:build llama_stub

// nctx_coordinator_sources_r634_test.go — R634 (2026-11-19): единый способ
// прочитать исходники координатора перезагрузки окна для сторожей.
//
// ЗАЧЕМ. `nctx_reload.go` (1865 строк) разложен на семь файлов: конфигурация и типы,
// состояние координатора, решения, HTTP-клиент, `DoReload`, метрики и опрос прогресса.
// Сторож `TestR505_DoReloadWritesPerSlotCache` читал исходный файл по имени и после
// переноса сообщил «не найден вызов SetLastKnownNCtx в nctx_reload.go» — хотя вызов
// на месте, просто в `nctx_reload_do.go`.
//
// Тот же приём, что в chat_sources_r627 / cppworker_sources_r628 / llamacpp_sources_r630 /
// nctx_reload_sources_r631 / model_management_sources_r632.
//
// НЕ ПУТАТЬ с nctx_reload_sources_r631_test.go: там другой слой (HTTP-обработчики
// перезагрузки окна), здесь — сам координатор. Два списка не пересекаются, и это
// осознанно: склейка «всё, что начинается на nctx_reload_», смешала бы два слоя и
// сломала смысл счётчиков.
package balancer

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// nctxCoordinatorLayerFilesR634 — файлы одного слоя (бывший nctx_reload.go).
var nctxCoordinatorLayerFilesR634 = []string{
	"nctx_reload.go",
	"nctx_reload_coordinator.go",
	"nctx_reload_decide.go",
	"nctx_reload_client.go",
	"nctx_reload_do.go",
	"nctx_reload_stats.go",
	"nctx_reload_progress.go",
}

// readNCtxCoordinatorSourcesR634 — склеенный текст всех файлов слоя координатора.
func readNCtxCoordinatorSourcesR634(t *testing.T) string {
	t.Helper()
	names := append([]string(nil), nctxCoordinatorLayerFilesR634...)
	sort.Strings(names)
	var out []byte
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join(".", n))
		if err != nil {
			t.Fatalf("чтение %s: %v — если файл переименован или разложен дальше, обновите "+
				"nctxCoordinatorLayerFilesR634 (сторож обязан знать про перенос)", n, err)
		}
		out = append(out, raw...)
		out = append(out, '\n')
	}
	return string(out)
}
