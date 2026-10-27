package main

import (
	"strings"
	"testing"
)

// R248 (2026-10-27): antiprompt'ы не должны обрывать ответ, когда модель
// ЦИТИРУЕТ стоп-токен в тексте.
//
// Живой симптом у Cline: ответ обрывается посреди фразы и оборачивается в
// COMPLETE, будто модель закончила, хотя она продолжала. Проверка antiprompt'ов
// суффиксная и не отличает «маркер хода» от «цитату в тексте». Ровно этот
// дефект был у gemma-4 с "<end_of_turn>" (Round 32 #27). Для Qwen убираем
// литералы, которые модель обсуждает в размышлениях (<|im_end|>, <|end|>),
// оставляя только маркеры НАЧАЛА следующего хода: EOS-токен завершает ход
// через EOG llama.cpp, без antiprompt'а.
func TestR248_QwenAntipromptsDoNotQuoteTruncate(t *testing.T) {
	for _, model := range []string{"Qwen3.8-27B-UD-Q4_K_M", "qwen3", "Qwen3.5-MoE", "qwen2.5-vl-3b"} {
		aps := defaultAntipromptsForModel(model)
		joined := strings.Join(aps, " ")
		for _, forbidden := range []string{"<|im_end|>", "<|end|>"} {
			if strings.Contains(joined, forbidden) {
				t.Fatalf("%s: antiprompt %q останавливает ответ на цитате в тексте: %v",
					model, forbidden, aps)
			}
		}
		if !strings.Contains(joined, "<|im_start|>user") {
			t.Fatalf("%s: потерян маркер начала хода пользователя: %v", model, aps)
		}
	}
}

// R248: у gemma-4 стоп-литерал "<end_of_turn>" остаётся выключенным (Round 32
// #27), а маркеры начала хода — включены.
func TestR248_Gemma4AntipromptsStillSafe(t *testing.T) {
	aps := defaultAntipromptsForModel("gemma-4-E4B-it-Q4_K_M")
	joined := strings.Join(aps, " ")
	if strings.Contains(joined, "<end_of_turn>") {
		t.Fatalf("gemma-4: <end_of_turn> возвращён в antiprompts — обрыв ответа вернётся: %v", aps)
	}
	if !strings.Contains(joined, "<start_of_turn>user") {
		t.Fatalf("gemma-4: потерян маркер начала хода: %v", aps)
	}
}
