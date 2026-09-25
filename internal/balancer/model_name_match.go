package balancer

import (
	"strings"

	"ollama-loadbalancer/pkg/modelname"
)

// modelNameMatches — R66d (2026-09-23): «это одна и та же модель?» с
// нормализацией расширения .gguf и пути.
//
// ЗАЧЕМ. cppworker регистрирует загруженную модель под именем БЕЗ расширения
// (Name = basename(path) без ".gguf"), а WebUI-карточка локального файла (и
// часть клиентов, например Cline/OpenWebUI с явным указанием файла) отдаёт имя
// С расширением: "Qwen3-Instruct-2507-q4km.gguf". Точное сравнение строк
// ломалось, и это давало три разных симптома:
//
//  1. Балансер 5 поллов подряд не находил модель в /api/models и объявлял
//     загрузку провалившейся, хотя cppworker её реально загрузил → в WebUI
//     «Ошибка загрузки» рядом с успешно загруженной моделью (воспроизведено в
//     реальном браузере: клик «Загрузить модель» на карточке
//     Qwen3-Instruct-2507-q4km.gguf, ctx=131072, модель загружена, а UI показал
//     ошибку "did not appear in cppworker model list or load progress").
//  2. apply профиля пропускал reload с «model not currently loaded on this
//     backend», потому что IsLlamaCppModelLoaded сравнивал имена точно.
//  3. preflight n_ctx не видел фактический n_ctx загруженной модели.
//
// Семантика: точное совпадение → совпадение без учёта регистра/расширения/
// пути/тега Ollama → частичное совпадение в любую сторону (сохранено прежнее
// поведение containsFold: профиль/запрос "gemma-4" матчит
// "gemma-4-E4B-it-Q4_K_M").
func modelNameMatches(candidate, wanted string) bool {
	if candidate == "" || wanted == "" {
		return false
	}
	if candidate == wanted || strings.EqualFold(candidate, wanted) {
		return true
	}
	normCand := normalizeModelName(candidate)
	normWant := normalizeModelName(wanted)
	if normCand == "" || normWant == "" {
		return false
	}
	if normCand == normWant {
		return true
	}
	return containsFold(normCand, normWant) || containsFold(normWant, normCand)
}

// normalizeModelName — basename пути (если передан путь), без тега Ollama, без
// расширения .gguf, в нижнем регистре. Используется только для сравнения имён,
// не для вывода.
//
// R83 (2026-09-25): добавлено срезание тега Ollama. Клиент законно зовёт модель
// "qwen3.8:latest", а cppworker репортит имя файла "Qwen3.8-27B-UD-Q4_K_M". Без
// срезания тега нормализованное имя оставалось "qwen3.8:latest", совпадения не
// было, и вызывающий код считал, что модель не найдена. Для таймаут-эвристики
// это означало размер 0 → idle-дедлайн 120s вместо tier 1800s для 16.5 GB
// модели: датчик зависания обрывал живую медленную partial-offload генерацию
// 27B. Разбор — model_size_resolution_r83_test.go.
//
// Порядок важен: сначала basename (у пути на Windows своё двоеточие в букве
// диска), и только потом StripTag — он сам отказывается трогать строки с
// разделителями пути.
func normalizeModelName(name string) string {
	n := strings.TrimSpace(name)
	if i := strings.LastIndexAny(n, `/\`); i >= 0 {
		n = n[i+1:]
	}
	n = modelname.StripTag(n)
	n = strings.ToLower(n)
	return strings.TrimSuffix(n, ".gguf")
}
