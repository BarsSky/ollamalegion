// model_not_found.go — R83 (2026-09-25): внятный 404 вместо 500 от llama.cpp.
//
// ПРОБЛЕМА (живой лог с A10). Клиент просит "qwen3.8:latest", на диске лежит
// "Qwen3.8-27B-UD-Q4_K_M.gguf". resolveModelPath заканчивался шагом 4 — «просто
// склеить modelsDir/<name>.gguf» — и возвращал НЕСУЩЕСТВУЮЩИЙ путь. Дальше:
//
//	gguf_init_from_file: failed to open GGUF file models/qwen3.8:latest.gguf
//	                  (No such file or directory)
//	POST /api/models/load → HTTP 500
//
// То есть оператор получал 500 с сырым путём llama.cpp вместо «такой модели нет,
// вот доступные». Ещё хуже: балансер в этом случае поллил load до 3-15 минут
// (см. load_failures.go — ровно этот класс проблем уже описывали для gemma-4).
//
// ПОЧЕМУ 404, А НЕ 503: балансер ретраит только 503 с признаком «model is
// loading» (internal/balancer/model_management.go:1223); на прочие не-2xx он
// возвращает ошибку сразу (:1245 «Другая ошибка — не повторяем»). Поэтому 404
// доходит до клиента как причина, а не превращается в retry-шторм.
package main

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"

	"ollama-loadbalancer/internal/cppbackend"
)

// minSuggestionPrefix — минимальная длина общего префикса, при которой считаем
// совпадение осмысленным («может, вы имели в виду»). Короче — уже случайность.
const minSuggestionPrefix = 4

// resolveModelPathChecked — как resolveModelPath, но проверяет, что файл
// реально существует. ok=false означает: путь невалиден, вызывающий обязан
// отдать 404 (writeModelNotFoundResponse) и НЕ нести путь в llama.cpp.
func resolveModelPathChecked(modelName string) (string, bool) {
	path := resolveModelPath(modelName)
	if path == "" {
		return "", false
	}
	if fi, err := os.Stat(path); err != nil || fi.IsDir() {
		return path, false
	}
	return path, true
}

// availableModelNames — канонические имена моделей и алиасов (без .gguf), как их
// должен указывать клиент. Отсортированы — стабильный вывод для тестов и UI.
func availableModelNames() []string {
	if backend == nil {
		// Тесты и ранняя инициализация: отдаём пустой список, а не паникуем.
		return nil
	}
	mm := backend.ModelManager()
	if mm == nil {
		return nil
	}
	seen := make(map[string]bool, 8)
	out := make([]string, 0, 8)
	add := func(name string) {
		name = strings.TrimSpace(strings.TrimSuffix(name, ".gguf"))
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, f := range mm.ListModels() {
		add(f.Filename)
	}
	// Алиасы (POST /api/create) — тоже валидные имена для клиента.
	for _, a := range mm.ListAliases() {
		add(a.Name)
	}
	sort.Strings(out)
	return out
}

// modelNotFoundSuggestion — «возможно, вы имели в виду X» + список доступных.
//
// Подсказка строится по нормализованному имени (Ollama-тег снят), поэтому для
// живого кейса "qwen3.8:latest" она укажет на "Qwen3.8-27B-UD-Q4_K_M".
func modelNotFoundSuggestion(requested string, available []string) string {
	if len(available) == 0 {
		return "В каталоге моделей нет ни одного .gguf — скачайте модель (HuggingFace) и повторите загрузку."
	}
	list := strings.Join(available, ", ")
	key := strings.ToLower(strings.TrimSuffix(cppbackend.StripOllamaTag(requested), ".gguf"))
	if key == "" {
		return "Доступные модели: " + list + "."
	}

	best, bestScore := "", 0
	for _, name := range available {
		lower := strings.ToLower(strings.TrimSuffix(name, ".gguf"))
		var score int
		switch {
		case lower == key:
			score = 1000
		case strings.Contains(lower, key):
			score = 500 + len(key)
		case strings.Contains(key, lower):
			score = 400 + len(lower)
		default:
			if p := commonPrefixLen(lower, key); p >= minSuggestionPrefix {
				score = p
			}
		}
		if score > bestScore {
			bestScore, best = score, name
		}
	}

	if best == "" {
		return fmt.Sprintf("Модели %q нет. Доступные модели: %s. Клиенту нужно указывать имя из этого списка.", requested, list)
	}
	return fmt.Sprintf("Модели %q нет. Возможно, вы имели в виду %q. Доступные модели: %s.", requested, best, list)
}

// commonPrefixLen — длина общего префикса в байтах (имена моделей ASCII).
func commonPrefixLen(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

// writeModelNotFoundResponse — 404 со списком доступных моделей.
func writeModelNotFoundResponse(w http.ResponseWriter, requested, attemptedPath string) {
	available := availableModelNames()
	writeJSON(w, http.StatusNotFound, map[string]interface{}{
		"error":            fmt.Sprintf("model %q not found in %s", requested, derefString(modelsDir)),
		"code":             "model_not_found",
		"model":            requested,
		"path":             attemptedPath,
		"available_models": available,
		"suggestion":       modelNotFoundSuggestion(requested, available),
	})
}
