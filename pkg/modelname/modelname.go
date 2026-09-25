// Package modelname — R83 (2026-09-25): нормализация ВНЕШНЕГО имени модели.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ПАКЕТ. Одна и та же проблема встречается на двух сторонах:
//
//   - cppworker: клиент просит "qwen3.8:latest", файл на диске —
//     "Qwen3.8-27B-UD-Q4_K_M.gguf" → открывался несуществующий
//     "models/qwen3.8:latest.gguf" → llama.cpp: failed to open GGUF file → 500;
//   - балансер: matchCppWorkerModel сравнивал имена по точному совпадению, не
//     находил "qwen3.8:latest" среди загруженных и СЧИТАЛ МОДЕЛЬ НЕЗАГРУЖЕННОЙ,
//     после чего инициировал загрузку несуществующей модели.
//
// Обе стороны должны снимать тег одинаково, поэтому логика живёт здесь, а не
// дублируется в двух бинарниках. Пакет намеренно без зависимостей (даже от
// cppbackend): балансер не должен тянуть model manager только ради строковой
// нормализации.
package modelname

import "strings"

// MaxTagLen — разумная длина тега Ollama ("latest", "q4_k_m", "v1.2").
// Более длинный «хвост» после ':' тегом не считаем.
const MaxTagLen = 64

// LooksLikePath — строка является путём, а не именем модели.
//
// Такие строки нормализации не подлежат: у них своя логика (filepath.IsAbs,
// filepath.Base). На Windows полное имя файла само содержит двоеточие
// ("C:\models\x.gguf"), и обрезка по ':' его бы сломала.
func LooksLikePath(s string) bool {
	if strings.ContainsAny(s, `/\`) {
		return true
	}
	// Windows drive letter: "C:" / "d:".
	if len(s) >= 2 && s[1] == ':' {
		return true
	}
	return false
}

// isPlausibleTag — «хвост» после ':' похож на тег Ollama, а не на часть имени.
func isPlausibleTag(tag string) bool {
	if tag == "" || len(tag) > MaxTagLen {
		return false
	}
	for _, r := range tag {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// StripTag убирает тег Ollama из имени модели.
//
//	"qwen3.8:latest"   → "qwen3.8"
//	"qwen3.8:q4_k_m"   → "qwen3.8"
//	"qwen3.8"          → "qwen3.8"           (тега нет)
//	`C:\models\x.gguf` → без изменений       (путь)
//	"models/x.gguf"    → без изменений       (путь)
//
// Если после ':' не тег (пробелы, кириллица, слишком длинный хвост) — строка
// возвращается как есть: лучше не найти модель и сказать об этом, чем молча
// обрезать настоящее имя.
func StripTag(name string) string {
	s := strings.TrimSpace(name)
	if s == "" || LooksLikePath(s) {
		return s
	}
	idx := strings.LastIndex(s, ":")
	if idx <= 0 || idx == len(s)-1 {
		return s
	}
	if !isPlausibleTag(s[idx+1:]) {
		return s
	}
	return strings.TrimSpace(s[:idx])
}

// Variants возвращает варианты имени для поиска: от наиболее точного к наиболее
// «прощающему». Порядок важен — первый найденный выигрывает, поэтому точное имя
// никогда не перебивается нормализованным.
//
// Дубликаты и пустые строки убираются: список идёт в цикл поиска по файлам.
func Variants(name string) []string {
	base := strings.TrimSpace(name)
	if base == "" {
		return nil
	}
	stripped := StripTag(base)
	// Снимаем расширение, чтобы "qwen3.8.gguf" тоже находил
	// "Qwen3.8-27B-UD-Q4_K_M.gguf": клиенты присылают имя и с расширением, и без
	// (Round 32 #14 уже сталкивался с дублями "имя" / "имя.gguf" в WebUI).
	noExt := func(s string) string {
		if strings.HasSuffix(strings.ToLower(s), ".gguf") {
			return s[:len(s)-len(".gguf")]
		}
		return s
	}

	raw := []string{
		base,
		stripped,
		noExt(base),
		noExt(stripped),
		stripped + ".gguf",
		base + ".gguf",
	}
	out := make([]string, 0, len(raw)*2)
	seen := make(map[string]bool, len(raw)*2)
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	for _, v := range raw {
		add(v)
	}
	// Регистро-варианты: часть карт ключуется по имени файла как он лежит на
	// диске, а клиент может прислать другое написание. Поиск по вхождению ниже и
	// так регистронезависим, но точные попадания дешевле.
	for _, v := range out {
		add(strings.ToLower(v))
	}
	return out
}

// Equal — «эти два имени обозначают одну модель» с учётом тега, расширения и
// регистра. Используется балансером при сопоставлении запрошенного имени с тем,
// что реально загружено на бэкенде.
func Equal(a, b string) bool {
	return Canonical(a) == Canonical(b) && Canonical(a) != ""
}

// MinFuzzyLen — минимальная длина канонического имени для «прощающего»
// сопоставления по вхождению. Короче — уже случайное совпадение (имя "a"
// подошло бы почти к любому файлу).
const MinFuzzyLen = 3

// Matches — сопоставление запрошенного имени с кандидатом так же, как это делает
// cppworker в FindModelByPath: сначала точно (с учётом тега/расширения/регистра),
// затем по вхождению.
//
// ПОЧЕМУ ВХОЖДЕНИЕ. Клиент просит "qwen3.8:latest", на бэкенде загружена
// "Qwen3.8-27B-UD-Q4_K_M". Точного совпадения нет, но это одна и та же модель —
// ровно так её и резолвит cppworker. Если балансер останется строгим, он сочтёт
// модель незагруженной и будет инициировать загрузку на каждый запрос, расходясь
// с cppworker (который ответит already_loaded).
//
// Ограничение MinFuzzyLen защищает от ложных срабатываний на коротких именах.
func Matches(requested, candidate string) bool {
	if Equal(requested, candidate) {
		return true
	}
	req, cand := Canonical(requested), Canonical(candidate)
	if len(req) < MinFuzzyLen || len(cand) < MinFuzzyLen {
		return false
	}
	return strings.Contains(cand, req) || strings.Contains(req, cand)
}

// Canonical — имя без тега, без .gguf, в нижнем регистре, без пробелов.
func Canonical(name string) string {
	s := strings.ToLower(StripTag(strings.TrimSpace(name)))
	if strings.HasSuffix(s, ".gguf") {
		s = s[:len(s)-len(".gguf")]
	}
	return s
}
