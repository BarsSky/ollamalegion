// Package rpccoordinator — Split/Merge Engine для распределённого inference.
// Реализует pipeline parallelism: разбиение prompt на сегменты и сборку output.
package rpccoordinator

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// Splitter разбивает входные данные на сегменты для параллельной обработки.
type Splitter struct {
	// Strategy: "token" (по токенам), "sentence" (по предложениям), "paragraph" (по абзацам)
	Strategy string
	// MaxSegmentSize — максимальный размер сегмента (в токенах или символах)
	MaxSegmentSize int
}

// NewSplitter создаёт новый сплиттер.
func NewSplitter(strategy string, maxSize int) *Splitter {
	if strategy == "" {
		strategy = "sentence"
	}
	if maxSize <= 0 {
		maxSize = 512
	}
	return &Splitter{
		Strategy:       strategy,
		MaxSegmentSize: maxSize,
	}
}

// SplitPrompt разбивает prompt на сегменты.
// Для pipeline parallelism каждый сегмент получает полный prompt,
// но worker обрабатывает только свои слои.
// Для tensor/parallel parallelism — разбиение prompt на части.
func (s *Splitter) SplitPrompt(prompt string, numSegments int) ([]string, error) {
	if numSegments <= 1 {
		return []string{prompt}, nil
	}

	switch s.Strategy {
	case "sentence":
		return s.splitBySentence(prompt, numSegments)
	case "paragraph":
		return s.splitByParagraph(prompt, numSegments)
	case "token":
		return s.splitByTokens(prompt, numSegments)
	case "equal":
		return s.splitEqual(prompt, numSegments)
	default:
		return s.splitBySentence(prompt, numSegments)
	}
}

// splitBySentence разбивает по предложениям (по точкам).
func (s *Splitter) splitBySentence(prompt string, numSegments int) ([]string, error) {
	sentences := strings.Split(prompt, ".")
	// Фильтруем пустые
	var filtered []string
	for _, s := range sentences {
		s = strings.TrimSpace(s)
		if s != "" {
			filtered = append(filtered, s+".")
		}
	}

	if len(filtered) <= numSegments {
		return filtered, nil
	}

	// Распределяем предложения по сегментам
	segments := make([]string, numSegments)
	perSeg := len(filtered) / numSegments
	extra := len(filtered) % numSegments

	idx := 0
	for i := 0; i < numSegments; i++ {
		count := perSeg
		if i < extra {
			count++
		}
		var parts []string
		for j := 0; j < count && idx < len(filtered); j++ {
			parts = append(parts, filtered[idx])
			idx++
		}
		segments[i] = strings.Join(parts, " ")
	}

	return segments, nil
}

// splitByParagraph разбивает по абзацам (по пустым строкам).
func (s *Splitter) splitByParagraph(prompt string, numSegments int) ([]string, error) {
	paragraphs := strings.Split(prompt, "\n\n")
	var filtered []string
	for _, p := range paragraphs {
		p = strings.TrimSpace(p)
		if p != "" {
			filtered = append(filtered, p)
		}
	}

	if len(filtered) <= numSegments {
		return filtered, nil
	}

	segments := make([]string, numSegments)
	perSeg := len(filtered) / numSegments
	extra := len(filtered) % numSegments

	idx := 0
	for i := 0; i < numSegments; i++ {
		count := perSeg
		if i < extra {
			count++
		}
		var parts []string
		for j := 0; j < count && idx < len(filtered); j++ {
			parts = append(parts, filtered[idx])
			idx++
		}
		segments[i] = strings.Join(parts, "\n\n")
	}

	return segments, nil
}

// splitByTokens разбивает приблизительно по количеству символов.
func (s *Splitter) splitByTokens(prompt string, numSegments int) ([]string, error) {
	// Упрощённая эвристика: ~4 символа на токен
	approxTokens := len(prompt) / 4
	if approxTokens <= s.MaxSegmentSize {
		return []string{prompt}, nil
	}

	segSize := len(prompt) / numSegments
	segments := make([]string, numSegments)

	start := 0
	for i := 0; i < numSegments; i++ {
		end := start + segSize
		if i == numSegments-1 {
			end = len(prompt)
		}
		// Ищем ближайший пробел для чистого разбиения
		if end < len(prompt) {
			for end < len(prompt) && prompt[end] != ' ' {
				end++
			}
		}
		segments[i] = strings.TrimSpace(prompt[start:end])
		start = end
	}

	return segments, nil
}

// splitEqual разбивает на равные части.
func (s *Splitter) splitEqual(prompt string, numSegments int) ([]string, error) {
	segSize := len(prompt) / numSegments
	segments := make([]string, numSegments)

	start := 0
	for i := 0; i < numSegments; i++ {
		end := start + segSize
		if i == numSegments-1 {
			end = len(prompt)
		}
		segments[i] = prompt[start:end]
		start = end
	}

	return segments, nil
}

// Merger собирает результаты срезов в финальный output.
type Merger struct {
	// Strategy: "concat" (конкатенация), "average" (усреднение embeddings), "last" (только последний)
	Strategy string
}

// NewMerger создаёт новый merger.
func NewMerger(strategy string) *Merger {
	if strategy == "" {
		strategy = "concat"
	}
	return &Merger{Strategy: strategy}
}

// MergeResults объединяет результаты срезов.
// Для pipeline parallelism: возвращает результат последнего среза (уже собранный).
// Для parallel: объединяет сегменты.
func (m *Merger) MergeResults(results [][]byte, strategy string) ([]byte, error) {
	if len(results) == 0 {
		return nil, fmt.Errorf("no results to merge")
	}

	switch strategy {
	case "concat":
		return m.mergeConcat(results)
	case "last":
		return results[len(results)-1], nil
	case "join_json":
		return m.mergeJSON(results)
	default:
		return m.mergeConcat(results)
	}
}

// mergeConcat конкатенирует результаты как строки.
func (m *Merger) mergeConcat(results [][]byte) ([]byte, error) {
	var output strings.Builder
	for i, r := range results {
		if i > 0 {
			output.WriteString(" ")
		}
		// Пытаемся декодировать как JSON (Ollama response)
		var resp map[string]interface{}
		if err := json.Unmarshal(r, &resp); err == nil {
			if text, ok := resp["response"].(string); ok {
				output.WriteString(text)
				continue
			}
			if done, ok := resp["done"].(bool); ok && done {
				// Финальный чанк — пропускаем повторение
			}
		}
		output.Write(r)
	}
	return []byte(output.String()), nil
}

// mergeJSON объединяет JSON-ответы от нескольких worker'ов.
func (m *Merger) mergeJSON(results [][]byte) ([]byte, error) {
	// Объединяем response-поля из всех JSON-ответов
	var responses []string
	for _, r := range results {
		var resp map[string]interface{}
		if err := json.Unmarshal(r, &resp); err != nil {
			responses = append(responses, string(r))
			continue
		}
		if text, ok := resp["response"].(string); ok {
			responses = append(responses, text)
		}
	}

	merged := map[string]interface{}{
		"response": strings.Join(responses, ""),
		"done":     true,
	}
	return json.Marshal(merged)
}

// MergeSliceOutputs объединяет output'ы срезов в pipeline.
// Каждый следующий срез получает output предыдущего как input.
// Этот метод используется для финальной сборки после pipeline.
func MergeSliceOutputs(sliceResults []*SliceResult) ([]byte, error) {
	if len(sliceResults) == 0 {
		return nil, fmt.Errorf("no slice results")
	}

	// Pipeline: результат = output последнего среза
	// (каждый срез уже получил input от предыдущего)
	last := sliceResults[len(sliceResults)-1]
	if last.Error != nil {
		return nil, fmt.Errorf("last slice failed: %w", last.Error)
	}
	return last.Output, nil
}

// ParallelMergeContext объединяет результаты параллельных срезов.
type ParallelMergeContext struct {
	mu       sync.Mutex
	results  map[int][]byte // ordinal → result
	errors   []error
	expected int
}

// NewParallelMergeContext создаёт контекст параллельного merge'а.
func NewParallelMergeContext(expected int) *ParallelMergeContext {
	return &ParallelMergeContext{
		results:  make(map[int][]byte, expected),
		expected: expected,
	}
}

// SetResult устанавливает результат для ординала.
func (pmc *ParallelMergeContext) SetResult(ordinal int, result []byte) {
	pmc.mu.Lock()
	defer pmc.mu.Unlock()
	pmc.results[ordinal] = result
}

// SetError добавляет ошибку.
func (pmc *ParallelMergeContext) SetError(err error) {
	pmc.mu.Lock()
	defer pmc.mu.Unlock()
	pmc.errors = append(pmc.errors, err)
}

// IsComplete проверяет, все ли результаты получены.
func (pmc *ParallelMergeContext) IsComplete() bool {
	pmc.mu.Lock()
	defer pmc.mu.Unlock()
	return len(pmc.results) == pmc.expected
}

// GetResults возвращает результаты в порядке ordinal.
func (pmc *ParallelMergeContext) GetResults() ([][]byte, error) {
	pmc.mu.Lock()
	defer pmc.mu.Unlock()

	if len(pmc.errors) > 0 {
		return nil, fmt.Errorf("parallel merge has %d errors", len(pmc.errors))
	}

	results := make([][]byte, pmc.expected)
	for i := 0; i < pmc.expected; i++ {
		r, ok := pmc.results[i]
		if !ok {
			return nil, fmt.Errorf("missing result for ordinal %d", i)
		}
		results[i] = r
	}
	return results, nil
}