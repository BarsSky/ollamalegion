//go:build llama_stub

// reasoning_surfaces_r219_test.go — R219 (2026-10-21).
//
// Собраны ВСЕ поверхности, на которых размышления утекали в ответ или терялись
// (живые дефекты R216), чтобы регрессия ловилась одним прогоном:
//
//  1. Qwen3/gemma-4 дописывают открывающий маркер в ПРОМПТ (`<think>`,
//     `<|channel>thought`), поэтому вывод начинается внутри размышлений и
//     открывающего тега в нём нет;
//  2. модель может закрыть блок несколько раз или не закрыть вовсе
//     (обрезка по max_tokens);
//  3. служебные маркеры не должны попадать в content ни при каком варианте.
package main

import (
	"strings"
	"testing"
)

func TestR219_PromptPrefillMatrix(t *testing.T) {
	cases := []struct {
		name    string
		prompt  string
		output  string
		wantRsn string
		wantCnt string
	}{
		{
			name:   "Qwen3 prefill, блок не закрыт (обрезка max_tokens)",
			prompt: "<|im_start|>assistant\n<think>\n",
			output: "думаю про задачу и обрываюсь",
			// reasoning — весь вывод; content пуст: ответа ещё нет, это честно.
			wantRsn: "думаю про задачу и обрываюсь",
			wantCnt: "",
		},
		{
			name:    "Qwen3 prefill, закрыт посреди вывода",
			prompt:  "<|im_start|>assistant\n<think>\n",
			output:  "рассуждаю</think>\n\nОтвет: 391",
			wantRsn: "рассуждаю",
			wantCnt: "\n\nОтвет: 391",
		},
		{
			name:    "gemma-4 channel-префилл",
			prompt:  "<start_of_turn>model\n<|channel>thought\n",
			output:  "проверяю числа\n<channel|>Ответ: 391",
			wantRsn: "проверяю числа",
			wantCnt: "Ответ: 391",
		},
		{
			name:    "два закрытых блока подряд в выводе",
			prompt:  "<|im_start|>assistant\n<think>\n",
			output:  "первый</think>ответ</think>хвост",
			wantRsn: "первый",
			wantCnt: "ответхвост", // режем по ПЕРВОМУ закрывающему, остальные маркеры вырезаем
		},
		{
			name:    "префилла нет: обычный ответ не должен стать reasoning",
			prompt:  "<|im_start|>assistant\n",
			output:  "просто ответ",
			wantRsn: "",
			wantCnt: "просто ответ",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rsn, cnt, _ := SplitReasoningContentFromPrompt(tc.prompt, tc.output)
			if rsn != tc.wantRsn {
				t.Errorf("reasoning = %q, want %q", rsn, tc.wantRsn)
			}
			if cnt != tc.wantCnt {
				t.Errorf("content = %q, want %q", cnt, tc.wantCnt)
			}
			for _, tag := range []string{"<think>", "</think>", "<thinking>", "<|channel>", "<channel|>"} {
				if strings.Contains(cnt, tag) {
					t.Errorf("в content остался служебный маркер %q: %q", tag, cnt)
				}
			}
		})
	}
}

// PromptOpensReasoning должен видеть ПОСЛЕДНИЙ маркер: если блок уже закрыт в
// промпте, префилла нет.
func TestR219_PromptOpenDetectionLastMarkerWins(t *testing.T) {
	closed := "<|im_start|>assistant\n<think>старое</think>продолжение"
	if PromptOpensReasoning(closed) {
		t.Error("закрытый блок в промпте не должен считаться открытым")
	}
	opened := "<|im_start|>assistant\n<think>старое</think><think>\n"
	if !PromptOpensReasoning(opened) {
		t.Error("второй открытый блок в конце промпта должен считаться открытым")
	}
}

// Стриминговый парсер: reasoning до маркера, content после; без дублей.
func TestR219_StreamImpliedOpenNoDuplicate(t *testing.T) {
	st := NewReasoningStreamState()
	st.SetImpliedOpen(true)

	var rsn, cnt strings.Builder
	for _, tok := range []string{"думаю ", "про ", "17*23", "</think>", "\nответ: 391"} {
		r, c := st.Feed(tok)
		rsn.WriteString(r)
		cnt.WriteString(c)
	}
	rt, ct := st.Finalize()
	rsn.WriteString(rt)
	cnt.WriteString(ct)

	if got := rsn.String(); !strings.Contains(got, "17*23") {
		t.Errorf("reasoning потерял текст: %q", got)
	}
	if got := cnt.String(); !strings.Contains(got, "ответ: 391") {
		t.Errorf("content потерял видимый ответ: %q", got)
	}
	if strings.Contains(cnt.String(), "думаю") {
		t.Errorf("reasoning утёк в content: %q", cnt.String())
	}
	if strings.Count(cnt.String(), "ответ: 391") > 1 {
		t.Errorf("content продублирован: %q", cnt.String())
	}
}
