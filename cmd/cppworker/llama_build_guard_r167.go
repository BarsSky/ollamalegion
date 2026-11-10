// llama_build_guard_r167.go — R167 (2026-10-17): сверка «коммит llama.cpp из env»
// с коммитом, ВШИТЫМ в бинарник.
//
// ЖИВОЙ СЛУЧАЙ. env-переменная CPPWORKER_LLAMA_BUILD — это НАМЕРЕНИЕ оператора
// (что он думает, что собрано). На стенде она уже расходилась с бинарником: агент
// донора отдавал «тот же коммит», потому что читал env, а собранная llama.cpp была
// старше. Проверка version_mismatch у узлов раскладки сравнивала env с env и
// соглашалась. При этом слои передаются между процессами, и работать они должны на
// одном коммите — то есть расхождение надо видеть.
//
// Теперь на старте воркер берёт коммит у САМОЙ llama.cpp (c/bridge:
// bridge_llama_commit → llama_commit() из common/build-info.cpp) и:
//   - пишет предупреждение в лог при расхождении;
//   - отдаёт в /api/info три поля: llama_build (факт из бинаря),
//     llama_build_env (намерение), llama_build_mismatch (истина/ложь).
package main

import (
	"os"
	"strings"

	"ollama-loadbalancer/pkg/version"
)

// llamaBuildCompatible — совместимы ли «намерение из env» и «факт из бинаря».
//
// Правила (осознанно мягкие, чтобы не шуметь там, где сравнивать нечего):
//   - env пуст → не ругаемся: оператор ничего не заявлял;
//   - факт пуст или "stub" (сборка без llama.cpp) → не ругаемся;
//   - иначе сравниваем по префиксу короткого хеша (env может нести суффикс).
func llamaBuildCompatible(envValue, binaryValue string) bool {
	env := strings.ToLower(strings.TrimSpace(envValue))
	bin := strings.ToLower(strings.TrimSpace(binaryValue))
	if env == "" || bin == "" || bin == "stub" {
		return true
	}
	if env == bin {
		return true
	}
	if len(env) > len(bin) && strings.HasPrefix(env, bin) {
		return true
	}
	if len(bin) > len(env) && strings.HasPrefix(bin, env) {
		return true
	}
	return false
}

// llamaBuildFacts — что показать в /api/info и в логе.
type llamaBuildFacts struct {
	// Binary — коммит, вшитый в бинарник (факт).
	Binary string
	// Env — значение CPPWORKER_LLAMA_BUILD (намерение оператора).
	Env string
	// Compatible — можно ли на этом узле участвовать в раскладке с другими.
	Compatible bool
	// Date — дата коммита llama.cpp (R305), запечённая в образ (YYYY-MM-DD).
	//
	// ЗАЧЕМ. Балансер сравнивает сборку узла с последним стабильным релизом. Пока
	// известен только коммит, сравнение с релизом (у которого есть тег и дата)
	// заканчивается «не с чем сравнивать», хотя ответ существует: сборка от
	// 2026-09-01 старше релиза от 2026-10-20. Пусто = старый образ, дата не
	// запечена (честное «неизвестно», а не выдуманная дата).
	Date string
}

// currentLlamaBuildFacts — снять факты текущего процесса.
//
// ФАКТ берём из CPPWORKER_LLAMA_BUILD_BAKED: это значение печёт Dockerfile из
// build-arg и compose НЕ переопределяет. Собственный CPPWORKER_LLAMA_BUILD —
// намерение (его переопределяет .env), а llama_commit() внутри образа возвращает
// синтетический docker-коммит (Dockerfile делает git init && git commit, чтобы
// build-info вообще собрался), поэтому для сверки он не годится.
func currentLlamaBuildFacts() llamaBuildFacts {
	baked := bakedLlamaCommitR486()
	env := strings.TrimSpace(getenvLlamaBuild())
	if baked == "" {
		// Старый образ: сверять не с чем. Не выдаём «совпало» — говорим, что
		// проверка недоступна, и оставляем право на сомнение.
		return llamaBuildFacts{Binary: "", Env: env, Compatible: true}
	}
	return llamaBuildFacts{
		Binary:     baked,
		Env:        env,
		Compatible: llamaBuildCompatible(env, baked),
		Date:       bakedLlamaDateR486(),
	}
}

// bakedLlamaCommitR486 — ФАКТ коммита llama.cpp, запечённый в образ.
//
// R486 (2026-11-10): приоритет у -X pkg/version.LlamaCommit (или
// /app/identity.json): это значение нельзя переопределить из .env, и оно
// совпадает с тем, что реально собрано в этом билде. CPPWORKER_LLAMA_BUILD_BAKED
// остаётся запасным путём для образов, собранных до R486.
func bakedLlamaCommitR486() string {
	if v := strings.TrimSpace(version.LlamaCommit); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv(envLlamaBuildBaked))
}

// bakedLlamaDateR486 — дата коммита llama.cpp (R305), тот же приоритет.
func bakedLlamaDateR486() string {
	if v := strings.TrimSpace(version.LlamaDate); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv(envLlamaBuildDateBaked))
}

// factsLlamaBuildSourceR486 — откуда взялся факт: нужно в логе, потому что
// «запечено в бинарь» и «старый образ с env» — разные степени доверия.
func factsLlamaBuildSourceR486() string {
	if strings.TrimSpace(version.LlamaCommit) != "" {
		return "ldflags"
	}
	if strings.TrimSpace(os.Getenv(envLlamaBuildBaked)) != "" {
		return "env_baked_legacy"
	}
	return "none"
}

// getenvLlamaBuild — env-значение (вынесено ради тестируемости).
func getenvLlamaBuild() string {
	return envLlamaBuildValue
}

// envLlamaBuildValue заполняется на старте (единственная точка чтения env, чтобы
// значения из .env и дефолт образа не расходились по коду).
var envLlamaBuildValue string

// envLlamaBuildBaked — R167: переменная, которую печёт Dockerfile из build-arg
// (env не переопределяет compose). Именно она — ФАКТ того, что собрано.
const envLlamaBuildBaked = "CPPWORKER_LLAMA_BUILD_BAKED"

// envLlamaBuildDateBaked — R305: дата того же коммита, запечённая в образ.
// Отдельная переменная, а не суффикс к коммиту: сверка «намерение vs собранное»
// (llamaBuildCompatible) сравнивает префиксы коммита, и суффикс с датой в той же
// строке сделал бы сравнение неоднозначным.
const envLlamaBuildDateBaked = "CPPWORKER_LLAMA_BUILD_DATE_BAKED"
