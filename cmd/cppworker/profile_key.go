// profile_key.go — D1 (2026-11-25): ЕДИНОЕ правило «имя модели → ключ профиля»
// на стороне воркера.
//
// ЖИВОЙ ДЕФЕКТ (реестр дефектов 2026-11-24, D1). В логе воркера:
//
//	conservative profile detected … profile_n_ctx=32768
//
// — при том, что на балансере для ключа `Qwen3.8-27B-UD-Q4_K_M` лежит профиль с
// `contextLength=131072`. Балансер растит окно по 131072, воркер «видит» 32768,
// и рекомендации с `feasible` расходятся.
//
// ПРИЧИНА. Резолвер профилей воркера (applyProfileOnLoad → pickProfileByKey,
// R83 §3.4) сравнивал НОРМАЛИЗОВАННЫЕ ключи по ПРЕФИКСУ в обе стороны и при
// нескольких совпадениях брал САМЫЙ ДЛИННЫЙ ключ. На стенде рядом живут
// профили-префиксы:
//
//	Qwen3.8-27B               32768   |  Qwen3.8-27B-UD-Q4_K_M        131072
//	gemma-4                   32768   |  gemma-4-E4B-it-Q4_K_M        32768
//	Qwen3-4B-Instruct-2507    32768   |  Qwen3-4B-Instruct-2507-Q4_K_M 32768
//
// Поэтому имя `M.gguf` (профиль `M` = 131072) резолвилось в СОСЕДСКИЙ профиль
// `M-mini` (32768): «самый длинный ключ» перебивал точное совпадение. Класс
// ошибки — «нашли чужой профиль», а не «не нашли»: оператор видит параметры,
// которых не задавал, и не видит параметров, которые задал.
//
// ПРАВИЛО (зеркало WebUI-правила `window.GgufModelKey` из R692 — там та же
// лестница, что и у серверной `balancer.NormalizeModelProfileKey`):
//
//  1. ТОЧНОЕ совпадение — имя как пришло (карточка/путь) и его basename;
//  2. ТОЧНОЕ + `.gguf` — записи профилей, сохранённые до R136, существуют, и
//     терять их нельзя (балансер их тоже дочитывает, см. GetModelProfile);
//  3. НОРМАЛИЗОВАННЫЙ ключ — `balancer.NormalizeModelProfileKey`: basename,
//     без `.gguf`, регистр СОХРАНЯЕТСЯ. Источник истины у этого шага ОДИН —
//     функция балансера; своей копии правила у воркера нет;
//  4. ТОЛЬКО РЕГИСТР — те же ключи в другом регистре; выбор детерминированный
//     (сортировка ключей, как в balancer.GetModelProfile), а не «как легла map».
//
// ПОДСТРОК И ПРЕФИКСОВ НЕТ ни на одном шаге. «Чужой профиль вместо пустого»
// хуже честного «профиля нет»: пустое значение оставляет env-дефолт воркера, а
// чужой профиль молча подменяет параметры оператора.
//
// ЧЕГО ЭТО ПРАВИЛО НЕ ДЕЛАЕТ. Оно не угадывает модель по ollama-тегу
// (`qwen3.8:latest` → профиль `Qwen3.8-27B`): именно такое угадывание по
// префиксу и подставляло чужой профиль (R83 §3.4 → дефект D1). На путях
// загрузки/перезагрузки воркер уже передаёт владельца ФАЙЛА
// (profileLookupNameForPath, R154): имя файла без расширения совпадает с ключом
// профиля ТОЧНО, без угадывания.
package main

import (
	"sort"
	"strings"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// Как нашёлся профиль — для тестов и диагностики (`matched` в ответе резолвера).
const (
	// profileMatchExact — имя как пришло (или его basename) есть в карте профилей.
	profileMatchExact = "exact"
	// profileMatchExactSuffixed — имя без расширения, а ключ профиля с `.gguf`.
	profileMatchExactSuffixed = "exact-suffixed"
	// profileMatchNormalized — совпал канонический ключ балансера.
	profileMatchNormalized = "normalized"
	// profileMatchFolded — совпал канонический ключ, но в другом регистре.
	profileMatchFolded = "folded"
)

// profileLookupStep — один шаг лестницы: что ищем и как называется шаг.
type profileLookupStep struct {
	Key string
	How string
}

// profileLookupD1 — результат резолва профиля.
type profileLookupD1 struct {
	// Profile — найденный профиль (копия: карта может смениться под нами).
	Profile types.LlamaCppModelProfile
	// Key — ключ карты, под которым профиль лежит.
	Key string
	// Matched — шаг лестницы, на котором совпало (см. profileMatch*).
	Matched string
}

// profileHasGGUFSuffix — то же условие длины, что у stripGGUF и у
// balancer.NormalizeModelProfileKey (len(name) > 5): имя `gguf` — это имя
// модели, а не расширение.
func profileHasGGUFSuffix(name string) bool {
	return len(name) > 5 && strings.EqualFold(name[len(name)-5:], ".gguf")
}

// profileBasename — последний элемент пути; Windows-разделители разбираются и на
// Linux (тот же приём, что в profileLookupNameForPath, R245).
func profileBasename(name string) string {
	p := strings.ReplaceAll(name, `\`, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// profileLookupCandidates — упорядоченная лестница вариантов ключа (без
// повторов). Порядок = приоритет: имя как есть → basename → с `.gguf` →
// канонический ключ балансера.
//
// Возвращается наружу (как и в GgufModelKey.candidates), чтобы правило можно
// было проверить без карты профилей и чтобы вызывающий мог показать, ЧТО именно
// искали.
func profileLookupCandidates(modelName string) []profileLookupStep {
	raw := strings.TrimSpace(modelName)
	if raw == "" {
		return nil
	}
	base := profileBasename(raw)
	out := make([]profileLookupStep, 0, 6)
	seen := make(map[string]bool, 6)
	push := func(key, how string) {
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, profileLookupStep{Key: key, How: how})
	}
	push(raw, profileMatchExact)
	push(base, profileMatchExact)
	// Профиль с расширением в ключе (записи до R136) — и для имени, и для
	// basename. Лишний `.gguf.gguf` дать невозможно: проверяем суффикс.
	if !profileHasGGUFSuffix(raw) {
		push(raw+".gguf", profileMatchExactSuffixed)
	}
	if !profileHasGGUFSuffix(base) {
		push(base+".gguf", profileMatchExactSuffixed)
	}
	push(balancer.NormalizeModelProfileKey(raw), profileMatchNormalized)
	push(balancer.NormalizeModelProfileKey(base), profileMatchNormalized)
	return out
}

// findModelProfileD1 — найти профиль модели в карте по правилу profile_key.go.
//
// nil = профиля нет (это ЗАКОННЫЙ ответ: не «подставить соседний», а «не
// задано»). Вызывающий обязан отличать nil от нулевого профиля.
func findModelProfileD1(cache map[string]types.LlamaCppModelProfile, modelName string) *profileLookupD1 {
	if len(cache) == 0 {
		return nil
	}
	for _, step := range profileLookupCandidates(modelName) {
		if p, ok := cache[step.Key]; ok {
			return &profileLookupD1{Profile: p, Key: step.Key, Matched: step.How}
		}
	}
	// Последний шаг — регистр. Детерминированно: сортировка ключей, как в
	// balancer.GetModelProfile (порядок map в Go не гарантирован).
	want := balancer.NormalizeModelProfileKey(modelName)
	if want == "" {
		return nil
	}
	keys := make([]string, 0, len(cache))
	for k := range cache {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.EqualFold(balancer.NormalizeModelProfileKey(k), want) {
			return &profileLookupD1{Profile: cache[k], Key: k, Matched: profileMatchFolded}
		}
	}
	return nil
}
