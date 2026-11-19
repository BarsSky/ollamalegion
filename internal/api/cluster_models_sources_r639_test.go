//go:build llama_stub

// cluster_models_sources_r639_test.go — R639 (2026-11-19): единый способ прочитать
// исходники HTTP-слоя кластерных моделей для сторожей.
//
// ЗАЧЕМ. `internal/api/handlers_cluster_models.go` (1628 строк) разложен на шесть
// файлов: агрегированные списки, перезагрузка модели, итог перезагрузки, состояние
// модели на бэкенде, массовые операции и восстановление. Три сторожа читали файл по
// имени и покраснели на переносе — при этом код на месте:
//
//	TestR119_HandlersUseEvictionPath        — «executeReloadOnBackend больше не использует loadWithEvictionOnMemoryFailure»
//	TestR300_HandlersKeepStillLoadingContract — «HTTP-слой не различает StillLoading»
//	TestR371STEWARD_HandlerUsesStatusHelper — «нет вызова opFailureStatusR371»
//
// Тот же приём, что в chat_sources_r627 / cppworker_sources_r628 / llamacpp_sources_r630 /
// nctx_reload_sources_r631 / model_management_sources_r632 / nctx_coordinator_sources_r634 /
// image_tool_sources_r637.
package api

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// clusterModelsLayerFilesR639 — файлы одного слоя (бывший handlers_cluster_models.go).
var clusterModelsLayerFilesR639 = []string{
	"handlers_cluster_models.go",
	"handlers_cluster_models_reload.go",
	"handlers_cluster_models_result.go",
	"handlers_cluster_models_state.go",
	"handlers_cluster_models_bulk.go",
	"handlers_cluster_models_restore.go",
}

// readClusterModelsSourcesR639 — склеенный текст всех файлов слоя.
func readClusterModelsSourcesR639(t *testing.T) string {
	t.Helper()
	names := append([]string(nil), clusterModelsLayerFilesR639...)
	sort.Strings(names)
	var out []byte
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join(".", n))
		if err != nil {
			t.Fatalf("чтение %s: %v — если файл переименован или разложен дальше, обновите "+
				"clusterModelsLayerFilesR639 (сторож обязан знать про перенос)", n, err)
		}
		out = append(out, raw...)
		out = append(out, '\n')
	}
	return string(out)
}
