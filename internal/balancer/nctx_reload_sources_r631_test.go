//go:build llama_stub

// nctx_reload_sources_r631_test.go — R631 (2026-11-19): единый способ прочитать
// исходники слоя перезагрузки окна для сторожей.
//
// ЗАЧЕМ. `nctx_reload_handlers.go` (2316 строк) разложен на пять файлов: обработчики,
// preflight, кэш наблюдённого окна, исполнение перезагрузки, метрики окна. Сторож
// R301 искал `preflightNCtxReloadIfNeeded` в исходном файле по имени и покраснел бы
// не потому, что обход очереди вернулся, а потому, что функция переехала.
//
// Это тот же приём, что в `chat_sources_r627_test.go`, `cppworker_sources_r628_test.go`
// и `llamacpp_sources_r630_test.go`: сторож читает ВСЮ семью файлов слоя, а не один файл
// по имени, поэтому следующий перенос внутри слоя его не сломает.
//
// Список явный (не шаблон `nctx_*.go`): в пакете есть соседние файлы с похожими
// именами (`nctx_admin_cap.go`, `nctx_ceiling_*.go`), и склейка «всё, что начинается
// на nctx_» ломала бы СМЫСЛ счётчиков в сторожах, а не только их значения.
package balancer

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// nctxReloadLayerFilesR631 — файлы одного слоя (бывший nctx_reload_handlers.go).
var nctxReloadLayerFilesR631 = []string{
	"nctx_reload_handlers.go",
	"nctx_reload_preflight.go",
	"nctx_reload_cache.go",
	"nctx_reload_execute.go",
	"nctx_reload_metrics.go",
}

// readNCtxReloadSourcesR631 — склеенный текст всех файлов слоя.
//
// Пропавший файл — громкая ошибка с инструкцией, а не тихое «ноль вхождений»:
// иначе сторож молча ослабнет, а это хуже ложной тревоги.
func readNCtxReloadSourcesR631(t *testing.T) string {
	t.Helper()
	var out []byte
	names := append([]string(nil), nctxReloadLayerFilesR631...)
	sort.Strings(names)
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join(".", n))
		if err != nil {
			t.Fatalf("чтение %s: %v — если файл переименован или разложен дальше, обновите "+
				"nctxReloadLayerFilesR631 (сторож обязан знать про перенос)", n, err)
		}
		out = append(out, raw...)
		out = append(out, '\n')
	}
	return string(out)
}
