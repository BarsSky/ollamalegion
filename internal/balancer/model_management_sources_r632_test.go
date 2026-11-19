//go:build llama_stub

// model_management_sources_r632_test.go — R632 (2026-11-19): единый способ
// прочитать исходники менеджера моделей для сторожей.
//
// ЗАЧЕМ. `model_management.go` (2172 строки) разложен на шесть файлов: конструктор
// и таймауты, операции над списком моделей, операции жизненного цикла, ожидание
// загрузки, путь llama.cpp (там же 447-строчный `executeLlamaCppLoadOnce`) и общий
// транспорт. Сторож `TestR119_Provenance_WiredIntoLoadPath` читал исходный файл по
// имени и после переноса сообщал «происхождение параметров снова неизвестно» —
// хотя проводка была на месте, просто в другом файле слоя.
//
// Тот же приём, что в `chat_sources_r627_test.go`, `cppworker_sources_r628_test.go`,
// `llamacpp_sources_r630_test.go`, `nctx_reload_sources_r631_test.go`: сторож читает
// ВСЮ семью файлов слоя, и следующий перенос внутри слоя его не сломает.
//
// Список явный: в пакете есть `model_replication.go`, `model_*.go` и т.п., и склейка
// «всё, что начинается на model_» ломала бы смысл счётчиков, а не только значения.
package balancer

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// modelManagementLayerFilesR632 — файлы одного слоя (бывший model_management.go).
var modelManagementLayerFilesR632 = []string{
	"model_management.go",
	"model_management_list.go",
	"model_management_ops.go",
	"model_management_load.go",
	"model_management_llamacpp.go",
	"model_management_transport.go",
}

// readModelManagementSourcesR632 — склеенный текст всех файлов слоя.
func readModelManagementSourcesR632(t *testing.T) string {
	t.Helper()
	names := append([]string(nil), modelManagementLayerFilesR632...)
	sort.Strings(names)
	var out []byte
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join(".", n))
		if err != nil {
			t.Fatalf("чтение %s: %v — если файл переименован или разложен дальше, обновите "+
				"modelManagementLayerFilesR632 (сторож обязан знать про перенос)", n, err)
		}
		out = append(out, raw...)
		out = append(out, '\n')
	}
	return string(out)
}
