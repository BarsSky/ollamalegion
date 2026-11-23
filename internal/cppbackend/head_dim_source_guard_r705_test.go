//go:build llama_stub

// internal/cppbackend/head_dim_source_guard_r705_test.go — R705 (2026-11-24).
//
// ПРЕДМЕТ. Настоящий источник занижения жил в C-мосте: bridge_get_model_metadata
// ПРИСВАИВАЛ n_embd_head_k/v = n_embd / n_heads, и число уезжало в /api/models,
// оттуда в балансер. Go-тесты не могут загрузить llama.cpp (для это нужен живой
// образ), поэтому здесь стоят СТРУКТУРНЫЕ сторожа ровно на те три места, где
// прежняя формула и «невидимый источник» жили на проводе:
//
//  1. c/bridge/bridge.c — публикует ширину решением bridge_head_dim_resolve и
//     выставляет head_dim_source; прежней формулы-присваивания больше нет;
//  2. cmd/cppworker/handlers_model_list.go — /api/models отдаёт head_dim_source
//     вместе с head_dim_k/head_dim_v (поле, которое читает балансерный поллер);
//  3. internal/balancer/llamacpp_metrics_poller.go — поллер РАЗБИРАЕТ
//     head_dim_source и переносит его в types.LlamaCppModel (иначе признак
//     «оценка/факт» теряется на половине пути).
//
// ПОЧЕМУ СТОРОЖА, А НЕ ПОВЕДЕНЧЕСКИЙ ТЕСТ. Тот же приём, что у R493d
// (dockerfiles_no_arch_variants_r493d_test.go): проверяемое свойство — «в этом
// месте больше нет прежнего кода» — дешевле и точнее ловится чтением исходника,
// чем поднятием реального llama.cpp. Живое поведение проверяется отдельно:
// c/bridge/tests/test_head_dim.c (числа) и /api/models на реальном контейнере.
//
// НЕ-ВАКУУМНОСТЬ. Все три проверки красные на 63a17828: там в bridge.c стоит
// `metadata.n_embd_head_k = (_n_heads > 0) ? _n_embd / _n_heads`, а полей
// head_dim_source нет ни в ответе воркера, ни в поллере — сторож на каждом шаге
// падает с конкретным сообщением, а не «тест устарел».
package cppbackend

import (
	"os"
	"strings"
	"testing"
)

// collapseWhitespaceR705 — убрать переводы строк и лишние пробелы.
//
// Исходник моста разбит на строки, и проверять присваивание по строке нельзя:
// переносы/отступы меняются при форматировании. Сравниваем «сплющенный» текст.
func collapseWhitespaceR705(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// TestR705_BridgePublishesRealHeadDim — c/bridge/bridge.c.
func TestR705_BridgePublishesRealHeadDim(t *testing.T) {
	const path = "../../c/bridge/bridge.c"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("не читается %s: %v", path, err)
	}
	text := string(raw)
	flat := collapseWhitespaceR705(text)

	// (а) прежней формулы-присваивания быть не должно: именно она давала 80/320/96.
	if strings.Contains(flat, "metadata.n_embd_head_k = (_n_heads > 0) ? _n_embd / _n_heads") {
		t.Fatal("bridge_get_model_metadata снова считает head_dim как n_embd/n_heads — " +
			"Qwen3-4B опубликует 80 вместо 128 (KV занижен на 60 %)")
	}
	// (б) решение принимает bridge_head_dim_resolve (чистая функция, см. head_dim.c).
	if !strings.Contains(flat, "bridge_head_dim_resolve(") {
		t.Error("bridge_get_model_metadata не зовёт bridge_head_dim_resolve — " +
			"ширина головы больше не берётся из модели одним решением")
	}
	// (в) источник уезжает наружу.
	if !strings.Contains(flat, "metadata.head_dim_source = hd.source") {
		t.Error("в ModelMetadata не выставляется head_dim_source — " +
			"занижение снова станет невидимым для /api/models и балансера")
	}
	// (г) hparams читаются через фасад (в публичном llama.h аксессоров нет).
	if !strings.Contains(flat, "ollamalegion_model_head_dim_k_full(") ||
		!strings.Contains(flat, "ollamalegion_model_head_dim_v_full(") {
		t.Error("ширина не читается из llama_hparams (csrc/model_hparams.h) — " +
			"для gemma-4 это дало бы 320 вместо 512")
	}
}

// TestR705_CppworkerListModelsPublishesSource — /api/models.
func TestR705_CppworkerListModelsPublishesSource(t *testing.T) {
	const path = "../../cmd/cppworker/handlers_model_list.go"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("не читается %s: %v", path, err)
	}
	flat := collapseWhitespaceR705(string(raw))
	if !strings.Contains(flat, `"head_dim_source": m.HeadDimSource`) {
		t.Fatal("/api/models не отдаёт head_dim_source — балансер не отличит " +
			"измеренную ширину (gguf) от прежней формулы (computed)")
	}
	if !strings.Contains(flat, `"head_dim_k": m.HeadDimK`) {
		t.Error("/api/models больше не отдаёт head_dim_k — R659 потеряет геометрию")
	}
}

// TestR705_BalancerPollerParsesSource — потребитель в балансере.
func TestR705_BalancerPollerParsesSource(t *testing.T) {
	const path = "../../internal/balancer/llamacpp_metrics_poller.go"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("не читается %s: %v", path, err)
	}
	flat := collapseWhitespaceR705(string(raw))
	if !strings.Contains(flat, "json:\"head_dim_source,headDimSource,omitempty\"") {
		t.Fatal("поллер не разбирает head_dim_source из /api/models — " +
			"признак «оценка/факт» потеряется на пути к R659/AutoTune")
	}
	if !strings.Contains(flat, "HeadDimSource: m.HeadDimSource,") {
		t.Error("поллер не переносит HeadDimSource в types.LlamaCppModel")
	}
}
