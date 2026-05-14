package rpccoordinator

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ========== Splitter Tests ==========

func TestNewSplitter_Defaults(t *testing.T) {
	s := NewSplitter("", 0)
	assert.Equal(t, "sentence", s.Strategy)
	assert.Equal(t, 512, s.MaxSegmentSize)
}

func TestNewSplitter_Custom(t *testing.T) {
	s := NewSplitter("token", 256)
	assert.Equal(t, "token", s.Strategy)
	assert.Equal(t, 256, s.MaxSegmentSize)
}

func TestSplitPrompt_SingleSegment(t *testing.T) {
	s := NewSplitter("sentence", 512)
	segments, err := s.SplitPrompt("hello world", 1)
	require.NoError(t, err)
	assert.Len(t, segments, 1)
	assert.Equal(t, "hello world", segments[0])
}

func TestSplitBySentence(t *testing.T) {
	s := NewSplitter("sentence", 512)
	segments, err := s.splitBySentence("First sentence. Second sentence. Third sentence.", 2)
	require.NoError(t, err)
	assert.Len(t, segments, 2)
	// Проверяем что все предложения распределены
	combined := strings.Join(segments, " ")
	assert.Contains(t, combined, "First sentence.")
	assert.Contains(t, combined, "Second sentence.")
	assert.Contains(t, combined, "Third sentence.")
}

func TestSplitBySentence_FewerThanSegments(t *testing.T) {
	s := NewSplitter("sentence", 512)
	segments, err := s.splitBySentence("One. Two.", 5)
	require.NoError(t, err)
	assert.Len(t, segments, 2)
}

func TestSplitByParagraph(t *testing.T) {
	s := NewSplitter("paragraph", 512)
	segments, err := s.splitByParagraph("Para one.\n\nPara two.\n\nPara three.", 2)
	require.NoError(t, err)
	assert.Len(t, segments, 2)
	combined := strings.Join(segments, "\n\n")
	assert.Contains(t, combined, "Para one")
	assert.Contains(t, combined, "Para two")
	assert.Contains(t, combined, "Para three")
}

func TestSplitByTokens(t *testing.T) {
	s := NewSplitter("token", 10) // маленький maxSize для гарантированного разбиения
	// ~1000 символов = ~250 токенов
	prompt := strings.Repeat("word ", 200)
	segments, err := s.splitByTokens(prompt, 2)
	require.NoError(t, err)
	assert.Len(t, segments, 2)
	// Проверяем что разбито примерно поровну
	assert.Greater(t, len(segments[0]), 0)
	assert.Greater(t, len(segments[1]), 0)
}

func TestSplitByTokens_SmallPrompt(t *testing.T) {
	s := NewSplitter("token", 10)
	segments, err := s.splitByTokens("small", 2)
	require.NoError(t, err)
	// Меньше MaxSegmentSize — возвращаем как есть
	assert.Len(t, segments, 1)
}

func TestSplitEqual(t *testing.T) {
	s := NewSplitter("equal", 512)
	segments, err := s.splitEqual("abcdef", 3)
	require.NoError(t, err)
	assert.Len(t, segments, 3)
	// "abcdef" / 3 = 2 символа на сегмент
	assert.Equal(t, "ab", segments[0])
	assert.Equal(t, "cd", segments[1])
	assert.Equal(t, "ef", segments[2])
}

func TestSplitPrompt_Sentence(t *testing.T) {
	s := NewSplitter("sentence", 512)
	segments, err := s.SplitPrompt("A. B. C. D. E.", 2)
	require.NoError(t, err)
	assert.Len(t, segments, 2)
}

func TestSplitPrompt_Paragraph(t *testing.T) {
	s := NewSplitter("paragraph", 512)
	segments, err := s.SplitPrompt("P1\n\nP2\n\nP3", 2)
	require.NoError(t, err)
	assert.Len(t, segments, 2)
}

func TestSplitPrompt_UnknownStrategy(t *testing.T) {
	s := NewSplitter("unknown", 512)
	// Должен fallback на sentence
	segments, err := s.SplitPrompt("A. B. C.", 2)
	require.NoError(t, err)
	assert.Len(t, segments, 2)
}

// ========== Merger Tests ==========

func TestNewMerger_Default(t *testing.T) {
	m := NewMerger("")
	assert.Equal(t, "concat", m.Strategy)
}

func TestMergeResults_Concat(t *testing.T) {
	m := NewMerger("concat")
	results := [][]byte{
		[]byte("hello"),
		[]byte("world"),
	}
	merged, err := m.MergeResults(results, "concat")
	require.NoError(t, err)
	assert.Equal(t, "hello world", string(merged))
}

func TestMergeResults_Last(t *testing.T) {
	m := NewMerger("concat")
	results := [][]byte{
		[]byte("first"),
		[]byte("last"),
	}
	merged, err := m.MergeResults(results, "last")
	require.NoError(t, err)
	assert.Equal(t, "last", string(merged))
}

func TestMergeResults_JSON(t *testing.T) {
	m := NewMerger("concat")
	results := [][]byte{
		[]byte(`{"response": "hello", "done": true}`),
		[]byte(`{"response": " world", "done": true}`),
	}
	merged, err := m.MergeResults(results, "join_json")
	require.NoError(t, err)
	var resp map[string]interface{}
	err = json.Unmarshal(merged, &resp)
	require.NoError(t, err)
	// Проверяем что response-поля объединены
	assert.Contains(t, resp["response"], "hello")
}

func TestMergeResults_Empty(t *testing.T) {
	m := NewMerger("concat")
	_, err := m.MergeResults([][]byte{}, "concat")
	require.Error(t, err)
}

func TestMergeSliceOutputs(t *testing.T) {
	results := []*SliceResult{
		{WorkerID: "w1", Output: []byte("intermediate"), Error: nil},
		{WorkerID: "w2", Output: []byte("final"), Error: nil},
	}
	output, err := MergeSliceOutputs(results)
	require.NoError(t, err)
	assert.Equal(t, "final", string(output))
}

func TestMergeSliceOutputs_Empty(t *testing.T) {
	_, err := MergeSliceOutputs([]*SliceResult{})
	require.Error(t, err)
}

func TestMergeSliceOutputs_LastError(t *testing.T) {
	results := []*SliceResult{
		{WorkerID: "w1", Output: []byte("ok"), Error: nil},
		{WorkerID: "w2", Output: nil, Error: assert.AnError},
	}
	_, err := MergeSliceOutputs(results)
	require.Error(t, err)
}

// ========== ParallelMergeContext Tests ==========

func TestNewParallelMergeContext(t *testing.T) {
	pmc := NewParallelMergeContext(3)
	require.NotNil(t, pmc)
	assert.Equal(t, 3, pmc.expected)
}

func TestParallelMergeContext_SetResult(t *testing.T) {
	pmc := NewParallelMergeContext(2)
	pmc.SetResult(0, []byte("a"))
	pmc.SetResult(1, []byte("b"))

	assert.True(t, pmc.IsComplete())
	results, err := pmc.GetResults()
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("a"), []byte("b")}, results)
}

func TestParallelMergeContext_Incomplete(t *testing.T) {
	pmc := NewParallelMergeContext(3)
	pmc.SetResult(0, []byte("a"))
	assert.False(t, pmc.IsComplete())
}

func TestParallelMergeContext_GetResults_Missing(t *testing.T) {
	pmc := NewParallelMergeContext(2)
	pmc.SetResult(0, []byte("a"))
	// Не устанавливаем результат для ordinal 1
	_, err := pmc.GetResults()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing result")
}

func TestParallelMergeContext_GetResults_WithError(t *testing.T) {
	pmc := NewParallelMergeContext(2)
	pmc.SetResult(0, []byte("a"))
	pmc.SetError(assert.AnError)
	_, err := pmc.GetResults()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "errors")
}

func TestParallelMergeContext_Concurrent(t *testing.T) {
	pmc := NewParallelMergeContext(10)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			pmc.SetResult(idx, []byte(fmt.Sprintf("result-%d", idx)))
		}(i)
	}
	wg.Wait()

	assert.True(t, pmc.IsComplete())
	results, err := pmc.GetResults()
	require.NoError(t, err)
	assert.Len(t, results, 10)
}