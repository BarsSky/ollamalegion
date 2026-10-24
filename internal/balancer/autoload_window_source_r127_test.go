//go:build llama_stub

// autoload_window_source_r127_test.go — R127 (2026-10-16), остаток пункта 7.
//
// ЖИВОЙ ДЕФЕКТ (замер на стенде). Снимок метрик балансера несколько секунд после
// выгрузки продолжает считать модель загруженной. Запрос с num_ctx=4096 при этом
// уходил по ветке «не понижаем загруженное окно», и автозагрузка стартовала БЕЗ
// num_ctx:
//
//	ResolveNumCtx: R60.37 — body num_ctx < loaded, upgrading (4096 -> 32768)
//	ensureModelLoadedOnBackend: auto-loading model ... numCtx=null
//	→ provenance после загрузки: {"value":8192,"source":"profile"}
//
// То есть параметр запроса не определял загрузку вовсе — при том что требование
// пункта 7 ровно обратное: «если модель не загружена, выигрывают параметры из
// запроса, а при их отсутствии грузятся дефолты».
//
// Тест фиксирует ПРАВИЛО в исходнике: на пути автозагрузки окно берётся из
// резолвера (запрос → профиль → дефолт), а не из «загруженного» снимка метрик.
// Ветка была лишней ещё и потому, что резолвер сам поднимает клиентское значение
// до загруженного окна (R60.37 внутри ResolveNumCtx), а код исполняется только
// когда модель НЕ готова.
package balancer

import (
	"os"
	"strings"
	"testing"
)

// TestR127_AutoLoadWindowComesFromResolver — на путях автозагрузки (чат и
// completion) окно обязано браться из resolved.Value.
func TestR127_AutoLoadWindowComesFromResolver(t *testing.T) {
	raw, err := os.ReadFile("llamacpp_handlers_inference.go")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	src := string(raw)

	// Проверяем именно ДЕФЕКТНУЮ ветку: подстановку «загруженного» окна в запрос
	// автозагрузки. Другие обращения к снимку метрик законны — например запись
	// LastKnownNCtx координатору reload'а ПОСЛЕ загрузки (это не выбор окна).
	if strings.Contains(src, "loadOpts.NumCtx = loadedNCtx") {
		t.Error("на пути автозагрузки окно снова берётся из снимка метрик: на " +
			"устаревшем снимке он подменяет параметр запроса чужим окном (живой " +
			"случай: запрос num_ctx=4096, автозагрузка ушла с numCtx=null, модель " +
			"поднялась с профильными 8192)")
	}
	if strings.Contains(src, "keeping loaded n_ctx (not lowering)") {
		t.Error("вернулась ветка «не понижаем загруженное» — она исполняется только " +
			"когда модель НЕ готова, поэтому «загруженное» там либо то же число, " +
			"либо устаревшее")
	}
	if n := strings.Count(src, "loadOpts.NumCtx = resolved.Value"); n < 2 {
		t.Errorf("веток «окно из резолвера» найдено %d, want >= 2 (чат и completion)", n)
	}
}

// TestR127_ResolverUpgradesToLoadedWindow — регресс-защита: подъём клиентского
// окна до загруженного остаётся там, где он уместен — ВНУТРИ резолвера (R60.37).
// Иначе, убрав внешнюю ветку, мы потеряли бы защиту от 400 на уже загруженной
// модели с окном больше клиентского.
func TestR127_ResolverUpgradesToLoadedWindow(t *testing.T) {
	raw, err := os.ReadFile("num_ctx_resolver.go")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	src := string(raw)
	if !strings.Contains(src, "body num_ctx < loaded, upgrading") {
		t.Error("в резолвере пропал подъём клиентского окна до загруженного (R60.37): " +
			"без него запрос с меньшим num_ctx к уже загруженной модели получит 400 " +
			"prompt_too_long")
	}
}
