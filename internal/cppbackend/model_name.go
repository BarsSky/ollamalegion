// model_name.go — R83 (2026-09-25): нормализация ВНЕШНЕГО имени модели.
//
// ПРОБЛЕМА (живой лог с A10). Клиент (Cline, Ollama-провайдер) просит модель
// "qwen3.8:latest", а файл на диске — "Qwen3.8-27B-UD-Q4_K_M.gguf". Ни один
// путь резолва не снимал Ollama-тег ":latest", поэтому:
//
//	POST /api/models/load → cppworker открывал "models/qwen3.8:latest.gguf"
//	  → llama.cpp: failed to open GGUF file ... (No such file or directory)
//	  → HTTP 500 (вместо внятного 404 со списком доступных моделей);
//	GET /api/v1/cppworker/adaptive/strategy → 404 "model metadata not found",
//	  из-за чего балансер молча не получал стратегию загрузки.
//
// Важная деталь: в FindModelByPath УЖЕ есть регистронезависимый поиск по
// вхождению, который нашёл бы "qwen3.8" в "qwen3.8-27b-ud-q4_k_m.gguf". Мешает
// ровно тег — то есть достаточно научиться его снимать и пробовать варианты.
//
// ПОЧЕМУ ТЕГ НЕЛЬЗЯ ПРОСТО ОБРЕЗАТЬ ПО ПЕРВОМУ ':'. На Windows полное имя файла
// само содержит двоеточие ("C:\models\x.gguf"), а имена с разделителями пути —
// это уже не «имя модели». Поэтому нормализация применяется ТОЛЬКО к строкам,
// похожим на короткое имя модели, и никогда не трогает пути.
package cppbackend

import (
	"os"
	"path/filepath"
	"strings"
)

// maxOllamaTagLen — разумная длина тега Ollama ("latest", "q4_k_m", "v1.2").
// Более длинный «хвост» после ':' тегом не считаем.
const maxOllamaTagLen = 64

// looksLikePath — строка является путём, а не именем модели.
//
// Такие строки нормализации не подлежат: у них своя логика (filepath.IsAbs,
// filepath.Base) в FindModelByPath/ResolveModelPath.
func looksLikePath(s string) bool {
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
	if tag == "" || len(tag) > maxOllamaTagLen {
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

// StripOllamaTag убирает тег Ollama из имени модели.
//
//	"qwen3.8:latest"   → "qwen3.8"
//	"qwen3.8:q4_k_m"   → "qwen3.8"
//	"qwen3.8"          → "qwen3.8"           (тега нет)
//	"C:\models\x.gguf" → без изменений       (путь)
//	"models/x.gguf"    → без изменений       (путь)
//
// Если после ':' не тег (пробелы, кириллица, слишком длинный хвост) — строка
// возвращается как есть: лучше не найти модель и сказать об этом, чем молча
// обрезать настоящее имя.
func StripOllamaTag(name string) string {
	s := strings.TrimSpace(name)
	if s == "" || looksLikePath(s) {
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

// ModelNameVariants возвращает варианты имени для поиска файла: от наиболее
// точного к наиболее «прощающему». Порядок важен — первый найденный выигрывает,
// поэтому точное имя никогда не перебивается нормализованным.
//
// Дубликаты и пустые строки убираются: список идёт в цикл поиска по файлам.
func ModelNameVariants(name string) []string {
	base := strings.TrimSpace(name)
	if base == "" {
		return nil
	}
	stripped := StripOllamaTag(base)
	// Снимаем расширение, чтобы "qwen3.8.gguf" тоже находил
	// "Qwen3.8-27B-UD-Q4_K_M.gguf" через поиск по вхождению: клиенты присылают
	// имя и с расширением, и без него (Round 32 #14 уже сталкивался с дублями
	// "имя" / "имя.gguf" в WebUI).
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
	// Регистро-варианты: часть карт (ggufFiles/nameHistory) ключуется по
	// filename как он лежит на диске, а клиент может прислать другое написание.
	// Contains-поиск ниже и так регистронезависим, но точные попадания дешевле.
	for _, v := range out {
		add(strings.ToLower(v))
	}
	return out
}

// findModelByVariantsLocked ищет модель по вариантам имени. Вызывающий ОБЯЗАН
// держать m.mu (RLock).
//
// Возвращает (путь, каноническое имя, найдено).
func (m *ModelManager) findModelByVariantsLocked(name string) (string, string, bool) {
	for _, v := range ModelNameVariants(name) {
		// 1. История загрузок (name → path): переживает idle-unload и рестарт.
		if histPath, ok := m.nameHistory[v]; ok {
			if _, err := os.Stat(histPath); err == nil {
				return histPath, filepath.Base(histPath), true
			}
		}
		// 2. Точное совпадение по filename.
		if meta, ok := m.ggufFiles[v]; ok {
			return meta.Path, v, true
		}
		// 3. Совпадение без расширения.
		for fname, meta := range m.ggufFiles {
			if strings.TrimSuffix(fname, ".gguf") == v {
				return meta.Path, fname, true
			}
		}
		// 4. Вхождение без учёта регистра — тот самый шаг, который нашёл бы
		//    "qwen3.8" в "qwen3.8-27b-ud-q4_k_m.gguf".
		lowerV := strings.ToLower(v)
		for fname, meta := range m.ggufFiles {
			if strings.Contains(strings.ToLower(fname), lowerV) {
				return meta.Path, fname, true
			}
		}
	}
	return "", "", false
}

// FindModelByVariants — публичная обёртка: резолвит внешнее имя модели
// (например "qwen3.8:latest") в путь к .gguf с учётом нормализации.
//
// Возвращает (путь, каноническое имя файла, найдено). Пустое каноническое имя
// означает «не нашли» — вызывающий должен показать 404 со списком доступных
// моделей, а не отдавать несуществующий путь в llama.cpp.
func (m *ModelManager) FindModelByVariants(name string) (string, string, bool) {
	if m == nil || strings.TrimSpace(name) == "" {
		return "", "", false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.findModelByVariantsLocked(name)
}
