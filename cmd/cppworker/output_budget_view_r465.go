// output_budget_view_r465.go — R465 (2026-11-06): политика бюджета вывода
// ВИДНА оператору и клиентам API, а не только в коде.
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА. «num_predict в env выставлен, но в WebUI его значения
// не увидеть: поле в настройках бэкенда всегда 0, как и большинство других
// настроек, которые должны показывать актуальное состояние; на странице
// Загруженные среди выводимых параметров этого значения тоже нет».
//
// Причина была простая: `cppbackend.Config` не содержит полей про n_predict —
// значение живёт в ENV (`CPPWORKER_DEFAULT_N_PREDICT_REASONING`) и в константах
// (DefaultNPredictReasoning, потолок R463), поэтому API отдавал по этому полю
// пустоту, а WebUI показывал 0/ничего.
//
// Теперь политика публикуется ЯВНО: отдельным разделом в конфиге воркера
// (outputBudget) и полем на каждую загруженную модель (какой бюджет получит
// запрос без num_predict). Имена полей одинаковые на обоих эндпоинтах, чтобы
// WebUI не собирал их из разных мест.
package main

import (
	"os"
	"strings"
)

// outputBudgetViewR465 — то, что видит оператор: дефолт, потолок и их источник.
func outputBudgetViewR465() map[string]interface{} {
	envRaw := strings.TrimSpace(os.Getenv("CPPWORKER_DEFAULT_N_PREDICT_REASONING"))
	source := "default"
	if envRaw != "" {
		if v, ok := parsePositiveIntR465(envRaw); ok && v > 0 {
			source = "env"
		}
	}
	ceilingEnv := strings.TrimSpace(os.Getenv("CPPWORKER_MAX_N_PREDICT"))
	ceilingSource := "default"
	if ceilingEnv != "" {
		if v, ok := parsePositiveIntR465(ceilingEnv); ok && v > 0 {
			ceilingSource = "env"
		}
	}
	return map[string]interface{}{
		// reasoningDefault — бюджет, который получает reasoning-модель, когда
		// клиент не прислал num_predict и не прислал max_output_tokens.
		"reasoningDefault":       DefaultNPredictReasoning,
		"reasoningDefaultSource": source,
		"reasoningDefaultEnv":    envRaw,
		// ceiling — предохранитель R463: выше него бюджет не поднимается даже по
		// клиентской границе max_output_tokens.
		"ceiling":       maxNPredictCeilingR463(),
		"ceilingSource": ceilingSource,
		"ceilingEnv":    ceilingEnv,
		// upperBoundRaisesDefault — R463: клиентская граница МОЖЕТ поднять дефолт.
		"upperBoundRaisesDefault": true,
		"note": "num_predict без клиентского значения: reasoning-модель получает reasoningDefault, " +
			"max_output_tokens клиента может поднять бюджет до ceiling; явный num_predict/max_tokens клиента важнее.",
	}
}

// defaultNumPredictForModelR465 — бюджет, который получит запрос этой модели без
// явного num_predict (для страницы «Загруженные»).
func defaultNumPredictForModelR465(modelName string) int {
	budget := ResolveNPredict(0, modelName)
	if ceiling := maxNPredictCeilingR463(); ceiling > 0 && budget > ceiling {
		budget = ceiling
	}
	return budget
}

// parsePositiveIntR465 — строку в положительное число (мусор = false).
func parsePositiveIntR465(raw string) (int, bool) {
	n := 0
	if raw == "" {
		return 0, false
	}
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
		if n > 1_000_000 {
			return 0, false
		}
	}
	return n, n > 0
}
