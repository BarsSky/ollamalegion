// tool_history_templates_r246_test.go — R246 (2026-10-26): выбор формы
// tool-истории проверяется на РЕАЛЬНЫХ Jinja-шаблонах моделей, а не на догадках.
//
// Источник: c/llama.cpp/models/templates/*.jinja (68 шаблонов llama.cpp — Qwen,
// Llama-3, Mistral, DeepSeek, Gemma, Hermes, Command-R, Kimi, GLM, gpt-oss …).
// Сабмодуль может отсутствовать (CI чекаутит его без submodules) — тогда тест
// пропускается с причиной, как R221.
//
// Что фиксируется (инвариант «результат инструмента не теряется»):
//  1. шаблон сам рисует role="tool" (57 из 68) — прежний путь, НИЧЕГО не меняем;
//  2. шаблон рисует вызовы из tool_calls И рисует результат (gemma-4, Cohere,
//     Command-R7B) — структурная форма (tool_calls + tool_call_id);
//  3. шаблон читает tool_calls, но результата не рисует (Seed-OSS,
//     LFM2.5-8B-A1B) — ТЕКСТОВЫЙ фолбэк: результат уходит user-сообщением,
//     иначе шаблон молча его пропустит.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// templatesDir — реальные шаблоны llama.cpp в репозитории.
func templatesDir() string {
	return filepath.Join("..", "..", "c", "llama.cpp", "models", "templates")
}

// loadTemplates — шаблоны; пусто и причина, если каталога нет.
func loadTemplates(t *testing.T) (map[string]string, string) {
	t.Helper()
	dir := templatesDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "каталог шаблонов недоступен (сабмодуль не чекаутнут)"
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jinja") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		out[e.Name()] = string(data)
	}
	if len(out) == 0 {
		return nil, "в каталоге шаблонов нет .jinja файлов"
	}
	return out, ""
}

// TestR246_RealTemplatesChooseSafePath — по каждому реальному шаблону: форма
// tool-истории либо прежняя (без изменений), либо структурная, либо текст.
// Структурная НИКОГДА не выбирается, если шаблон не умеет рисовать результат.
func TestR246_RealTemplatesChooseSafePath(t *testing.T) {
	templates, reason := loadTemplates(t)
	if templates == nil {
		t.Skip("реальные шаблоны недоступны: " + reason)
	}

	var native, structured, text int
	for name, src := range templates {
		caps := analyzeTemplateToolCaps(src)
		structuredPath := usesStructuredToolHistory(caps)

		switch {
		case structuredPath:
			structured++
			if !caps.ConsumesToolResults {
				t.Errorf("%s: структурный путь выбран шаблону без отрисовки результата — "+
					"role=tool будет потерян", name)
			}
			if caps.StandaloneTool {
				t.Errorf("%s: структурный путь выбран шаблону, который сам рисует role=tool", name)
			}
		case caps.StandaloneTool:
			native++
		default:
			text++
		}

		// Инвариант: если шаблон вообще не умеет рисуть результат и не рисует
		// role=tool, в него нельзя отправлять role=tool — только текст.
		if !caps.StandaloneTool && !structuredPath && caps.ConsumesToolResults && caps.StructuredCalls {
			t.Errorf("%s: шаблон умеет и вызовы, и результат, но выбран текстовый путь", name)
		}
	}

	// Ожидаемое распределение по набору llama.cpp. Числа — предметные: если
	// шаблонов станет больше, тест подскажет пересмотреть правило.
	if native < 50 {
		t.Errorf("нативный путь у %d шаблонов, ожидалось не меньше 50", native)
	}
	if structured < 2 {
		t.Errorf("структурный путь у %d шаблонов, ожидалось хотя бы 2 (gemma-4)", structured)
	}
	if text < 3 {
		t.Errorf("текстовый фолбэк у %d шаблонов, ожидалось хотя бы 3 (gemma-2, Phi-3.5, …)", text)
	}
	t.Logf("шаблонов: %d, нативный role=tool: %d, структурный: %d, текстовый: %d",
		len(templates), native, structured, text)
}

// TestR246_KnownModelsRoute — маршрутизация тех семейств, которые стоят у
// оператора или прямо упомянуты в жалобах. Проверяем по РЕАЛЬНЫМ шаблонам.
func TestR246_KnownModelsRoute(t *testing.T) {
	templates, reason := loadTemplates(t)
	if templates == nil {
		t.Skip("реальные шаблоны недоступны: " + reason)
	}

	cases := []struct {
		file     string
		wantPath string // "structured" | "native" | "text"
	}{
		{"google-gemma-4-31B-it.jinja", "structured"},
		{"google-gemma-4-31B-it-interleaved.jinja", "structured"},
		{"CohereForAI-c4ai-command-r7b-12-2024-tool_use.jinja", "structured"},
		{"Qwen-Qwen3-0.6B.jinja", "native"},
		{"Qwen3.5-4B.jinja", "native"},
		{"Qwen3-Coder.jinja", "native"},
		{"meta-llama-Llama-3.1-8B-Instruct.jinja", "native"},
		{"mistralai-Mistral-Nemo-Instruct-2407.jinja", "native"},
		{"deepseek-ai-DeepSeek-V3.2.jinja", "native"},
		{"deepseek-ai-DeepSeek-R1-Distill-Qwen-32B.jinja", "native"},
		{"NousResearch-Hermes-3-Llama-3.1-8B-tool_use.jinja", "native"},
		{"openai-gpt-oss-120b.jinja", "native"},
		{"google-gemma-2-2b-it.jinja", "text"},
		{"microsoft-Phi-3.5-mini-instruct.jinja", "text"},
		{"ByteDance-Seed-OSS.jinja", "text"},
		{"LFM2.5-8B-A1B.jinja", "text"},
	}

	for _, tc := range cases {
		src, ok := templates[tc.file]
		if !ok {
			t.Logf("%s: шаблона нет в наборе, пропуск", tc.file)
			continue
		}
		caps := analyzeTemplateToolCaps(src)
		got := "text"
		if usesStructuredToolHistory(caps) {
			got = "structured"
		} else if caps.StandaloneTool {
			got = "native"
		}
		if got != tc.wantPath {
			t.Errorf("%s: путь %q, ожидался %q (caps=%+v)", tc.file, got, tc.wantPath, caps)
		}
	}
}

// TestR246_StructuredPathKeepsResultVisible — даже если шаблон читает tool_calls,
// но результата не рисует, результат обязан уйти текстом.
func TestR246_StructuredPathKeepsResultVisible(t *testing.T) {
	// Seed-OSS-подобный шаблон: tool_calls читает, role="tool" не рисует.
	const seedLike = `{%- for message in messages -%}
{%- if message['tool_calls'] -%}{{- '<seed:tool_call>' -}}{%- endif -%}
{%- endfor -%}`

	caps := analyzeTemplateToolCaps(seedLike)
	if usesStructuredToolHistory(caps) {
		t.Fatalf("структурный путь выбран шаблону без отрисовки результата: %+v", caps)
	}

	msgs := []chatMessage{
		{Role: "user", Content: "task"},
		{Role: "assistant", ToolCalls: []ollamaToolCall{{
			Function: ollamaToolCallFunction{Name: "read", Arguments: []byte(`{"file_path":"a"}`)},
		}}},
		{Role: "tool", Content: "file body"},
	}
	history := buildNativeToolHistory(msgs, caps)
	last := history[len(history)-1]
	if last.Role != "user" || !strings.Contains(last.Content, "file body") {
		t.Errorf("результат инструмента не доехал текстом: %+v", last)
	}
}
