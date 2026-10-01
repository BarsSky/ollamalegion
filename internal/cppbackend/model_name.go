// model_name.go — R83 (2026-09-25): резолв ВНЕШНЕГО имени модели в файл.
//
// Сама нормализация (снятие Ollama-тега, варианты, регистр) живёт в
// pkg/modelname — её использует и балансер (сопоставление запрошенного имени с
// загруженным), а тянуть ради строковой операции cppbackend с model manager он
// не должен. Здесь остаётся только то, что требует доступа к каталогу моделей.
//
// ПРОБЛЕМА (живой лог с A10). Клиент (Cline, Ollama-провайдер) просит модель
// "qwen3.8:latest", а файл на диске — "Qwen3.8-27B-UD-Q4_K_M.gguf":
//
//	POST /api/models/load → cppworker открывал "models/qwen3.8:latest.gguf"
//	  → llama.cpp: failed to open GGUF file ... (No such file or directory)
//	  → HTTP 500 (вместо внятного 404 со списком доступных моделей);
//	GET /api/v1/cppworker/adaptive/strategy → 404 "model metadata not found".
//
// В FindModelByPath УЖЕ был регистронезависимый поиск по вхождению, который
// нашёл бы "qwen3.8" в "qwen3.8-27b-ud-q4_k_m.gguf" — мешал ровно тег.
package cppbackend

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ollama-loadbalancer/pkg/modelname"
)

// StripOllamaTag — обёртка над modelname.StripTag (см. пакет: там же правила и
// почему пути не трогаются).
func StripOllamaTag(name string) string { return modelname.StripTag(name) }

// ModelNameVariants — обёртка над modelname.Variants.
func ModelNameVariants(name string) []string { return modelname.Variants(name) }

// findModelByVariantsLocked ищет модель по вариантам имени. Вызывающий ОБЯЗАН
// держать m.mu (RLock).
//
// Возвращает (путь, каноническое имя, найдено).
func (m *ModelManager) findModelByVariantsLocked(name string) (string, string, bool) {
	for _, v := range modelname.Variants(name) {
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

// GetModelMetaResolved — GetModelMeta с резолвом внешнего имени.
//
// ЗАЧЕМ (R83, 2026-10-01). Клиент и балансер зовут модель «gemma-4-E4B-it-Q4_K_M»,
// а в кэше ggufFiles ключ — имя файла «gemma-4-E4B-it-Q4_K_M.gguf». Прямой
// GetModelMeta по внешнему имени возвращает ошибку, и вызывающий молча работает
// без метаданных. В checkVRAMForModel это означало «KV считать по всем слоям»:
// у gemma-4 3.17 GB вместо 294 MiB и 35 слоёв из 42 на GPU вместо полного
// оффлоада (замер стенда: 5-9 tok/s). Резолв по вариантам имени тут обязателен.
func (m *ModelManager) GetModelMetaResolved(name string) (*GGUFModelMeta, error) {
	if m == nil {
		return nil, fmt.Errorf("model manager is nil")
	}
	if meta, err := m.GetModelMeta(name); err == nil && meta != nil {
		return meta, nil
	}
	if _, canonical, ok := m.FindModelByVariants(name); ok && canonical != "" {
		if meta, err := m.GetModelMeta(canonical); err == nil && meta != nil {
			return meta, nil
		}
	}
	return nil, fmt.Errorf("model %s not found in models directory", name)
}
