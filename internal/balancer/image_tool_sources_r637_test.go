//go:build llama_stub

// image_tool_sources_r637_test.go — R637 (2026-11-19): единый способ прочитать
// исходники инструмента генерации изображений для сторожей.
//
// ЗАЧЕМ. `image_tool.go` (1671 строка) разложен на двенадцать файлов: настройки и
// цель инструмента, схема для клиента, разбор вызовов, разрешение цели, валидация,
// загрузка image-модели, генерация, результат, каталог, помощники и публичный URL.
// Сторож `TestR119_ImageTool_GenerationForToolUsesGate` читал исходный файл по имени
// и после переноса сообщил, что инструмент «обходит проверку VRAM и эксклюзивный лок
// GPU» — хотя гейт на месте, просто в `image_tool_generate.go`.
//
// Тот же приём, что в chat_sources_r627 / cppworker_sources_r628 / llamacpp_sources_r630 /
// nctx_reload_sources_r631 / model_management_sources_r632 / nctx_coordinator_sources_r634.
package balancer

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// imageToolLayerFilesR637 — файлы одного слоя (бывший image_tool.go).
var imageToolLayerFilesR637 = []string{
	"image_tool.go",
	"image_tool_target.go",
	"image_tool_schema.go",
	"image_tool_calls.go",
	"image_tool_resolve.go",
	"image_tool_validate.go",
	"image_tool_load.go",
	"image_tool_generate.go",
	"image_tool_result.go",
	"image_tool_catalog.go",
	"image_tool_helpers.go",
	"image_tool_url.go",
}

// readImageToolSourcesR637 — склеенный текст всех файлов слоя.
func readImageToolSourcesR637(t *testing.T) string {
	t.Helper()
	names := append([]string(nil), imageToolLayerFilesR637...)
	sort.Strings(names)
	var out []byte
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join(".", n))
		if err != nil {
			t.Fatalf("чтение %s: %v — если файл переименован или разложен дальше, обновите "+
				"imageToolLayerFilesR637 (сторож обязан знать про перенос)", n, err)
		}
		out = append(out, raw...)
		out = append(out, '\n')
	}
	return string(out)
}
