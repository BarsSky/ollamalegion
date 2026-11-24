//go:build llama_stub

// cppworker_build_parity_r730_test.go — D6 (реестр
// docs/plans/2026-11-24-remaining-defects.md), 2026-11-24.
//
// ЗАЧЕМ СТРАЖ. D6 звучал как «gemma-4-E4B не грузится в CPU-образе: SIGSEGV при
// flash attention». Замеры (R730) показали, что падение принадлежит СБОРКЕ, а не
// нашим параметрам:
//
//	CPU-образ на пине b1b770d90d, /api/models/load gemma-4-E4B Q4_K_M, ctx=4096,
//	gpuLayers=0, flashAttn=1, kvCacheType=q4_0 →
//	  graph_reserve: reserving a graph for ubatch with n_tokens = 1, n_seqs = 1 …
//	  SIGSEGV: segmentation violation / signal arrived during cgo execution
//	  контейнер завершился с кодом 2;
//
//	тот же запрос на CPU-образах с пином b12642 — state=loaded (16–82 с) и
//	генерация 1.8 ток/с; ctx=32768 + промпт 1609 токенов — 88 с, без падения.
//
// То есть причина — дефект llama.cpp в gemma-4 (расчёт графа превосходил лимит
// split-inputs сборки), вылеченный обновлением пина. Наш вклад в то, чтобы это не
// вернулось, — паритет двух Dockerfile'ов: CPU-образ обязан собираться с ТЕМ ЖЕ
// `GGML_SCHED_MAX_SPLIT_INPUTS`, что GPU-образ (R700 добавил флаг именно затем:
// в ggml-backend.cpp это не только проверка, но и РАЗМЕР массива
// `prefetched[GGML_SCHED_MAX_SPLIT_INPUTS]`), и с запёчённой идентичностью пинa
// (`LLAMA_BUILD`), иначе «какой пин реально в образе» снова станет догадкой, а
// расхождение соберётся молча.
//
// Тест читает сами Dockerfile'ы: правка сборки — единственное место, где этот
// регресс может вернуться (в Go-коде его нет).
package tests

import (
	"os"
	"strings"
	"testing"
)

// dockerfileForR730 — обязательные фрагменты сборки cppworker, по файлам.
//
// `fragment` проверяется как подстрока: Dockerfile — не конфиг, и жёсткий формат
// сделал бы тест красным от переноса строки, ничего не говоря о смысле.
type dockerfileForR730 struct {
	path     string
	why      string
	fragment []string
}

var dockerfilesR730 = []dockerfileForR730{
	{
		path: "docker/cppworker/Dockerfile.cpu",
		why:  "CPU-образ падал на gemma-4 (D6), пока не получил тот же лимит split-inputs, что GPU",
		fragment: []string{
			"-DGGML_CUDA=OFF",
			"-DGGML_SCHED_MAX_SPLIT_INPUTS=128",
			`-DLLAMA_BUILD_COMMIT="${LLAMA_BUILD}"`,
		},
	},
	{
		path: "docker/cppworker/Dockerfile.gpu",
		why:  "GPU-образ — эталон профиля сборки, с которым CPU обязан совпадать",
		fragment: []string{
			"-DGGML_SCHED_MAX_SPLIT_INPUTS=128",
			`-DLLAMA_BUILD_COMMIT="${LLAMA_BUILD}"`,
		},
	},
}

// TestR730_CppworkerBuildProfilesStayInParity — CPU и GPU образы собираются с
// одинаковым лимитом split-inputs и с запёчённой идентичностью пинa.
//
// Красный тест означает одно из двух: из CPU-сборки убрали лимит (тогда снова
// возможна порча памяти на моделях с большим числом входов графа — ровно D6), либо
// перестали передавать `LLAMA_BUILD` (тогда `libllama` сообщает «unknown», и
// вопрос «на каком пине собран образ» снова без ответа — правило R486).
func TestR730_CppworkerBuildProfilesStayInParity(t *testing.T) {
	for _, df := range dockerfilesR730 {
		body, err := os.ReadFile("../" + df.path)
		if err != nil {
			t.Fatalf("не читается %s: %v", df.path, err)
		}
		text := string(body)
		if strings.TrimSpace(text) == "" {
			t.Fatalf("%s пуст — проверять нечего, это не «зелёный» случай", df.path)
		}
		for _, frag := range df.fragment {
			if !strings.Contains(text, frag) {
				t.Errorf("%s: нет %q — %s", df.path, frag, df.why)
			}
		}
	}
}

// TestR730_SplitInputsLimitIsNotLowered — лимит именно 128, а не «какой-нибудь».
//
// Отдельная проверка на смысл числа: понижение до значения меньше входов графа
// (для gemma-4 в живом срезе было 17 при дефолтных 30, но у гигантских MoE срезов
// больше) возвращает ту же порчу памяти, а «флаг есть» тест выше уже не поймает.
func TestR730_SplitInputsLimitIsNotLowered(t *testing.T) {
	const want = "-DGGML_SCHED_MAX_SPLIT_INPUTS=128"
	for _, path := range []string{"docker/cppworker/Dockerfile.cpu", "docker/cppworker/Dockerfile.gpu"} {
		body, err := os.ReadFile("../" + path)
		if err != nil {
			t.Fatalf("не читается %s: %v", path, err)
		}
		text := string(body)
		if !strings.Contains(text, want) {
			t.Errorf("%s: ожидался %s (D6: без него CPU-сборка падала на gemma-4 в graph_reserve)", path, want)
		}
		for _, lowered := range []string{"GGML_SCHED_MAX_SPLIT_INPUTS=16", "GGML_SCHED_MAX_SPLIT_INPUTS=30"} {
			if strings.Contains(text, lowered) {
				t.Errorf("%s: лимит понижен (%s) — это возвращает переполнение из D6", path, lowered)
			}
		}
	}
}
