// handlers_tokenize_r467.go — R467 (2026-11-06): точный подсчёт токенов по
// запросу, а не только внутри процесса.
//
// ЗАЧЕМ. Балансер решает про окно по ОЦЕНКЕ промпта (эвристика по символам,
// `pkg/tokencount`): на живых стендах она завышала в 2–3 раза (оценка 49731
// против реальных 9816 по токенизатору; после калибровки — 26973 против 9816).
// Из-за этого принимались решения о перезагрузке модели и (до R466) отказы 413
// по нашей же догадке. Токенизатор модели есть ТОЛЬКО в воркере (C-bridge,
// `llama_tokenize`), поэтому единственный честный способ — дать его наружу.
//
// ЧТО ВОЗВРАЩАЕТ:
//   - tokens   — число токенов ИМЕННО переданного текста (или суммы `messages`);
//   - source   — "tokenizer" (модель загружена, llama_tokenize ответил) или
//     "estimate" (модель не загружена: загружать её ради подсчёта нельзя —
//     это минуты и выгрузка чужой модели);
//   - model_loaded — был ли реальный токенизатор.
//
// ЧЕГО НАМЕРЕННО НЕ ДЕЛАЕТ: не собирает промпт и не применяет chat template.
// Это значило бы «посчитать не то»: шаблон, tools[] и системная преамбула
// добавляют токены, которых в `messages` нет. Клиент, которому нужен промпт,
// получает точные числа по СВОИМ строкам, а запас на шаблон задаёт сам.
package main

import (
	"encoding/json"
	"net/http"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/tokencount"
)

// tokenizeRequestR467 — тело POST /api/tokenize.
type tokenizeRequestR467 struct {
	Model string `json:"model"`
	Text  string `json:"text"`
	// Messages — альтернатива Text: содержимое сообщений. Роли и структура
	// сохраняются в ответе, чтобы клиент видел, где именно дорого.
	Messages []tokenizeMessageR467 `json:"messages"`
}

type tokenizeMessageR467 struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type tokenizeMessageResultR467 struct {
	Role   string `json:"role"`
	Tokens int    `json:"tokens"`
	Chars  int    `json:"chars"`
}

type tokenizeResponseR467 struct {
	Model          string                      `json:"model"`
	Tokens         int                         `json:"tokens"`
	Source         string                      `json:"source"`
	ModelLoaded    bool                        `json:"model_loaded"`
	Chars          int                         `json:"chars"`
	Messages       []tokenizeMessageResultR467 `json:"messages,omitempty"`
	Note           string                      `json:"note,omitempty"`
	TemplateNotice string                      `json:"template_notice"`
}

// handleTokenizeR467 — POST /api/tokenize.
func handleTokenizeR467(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"error": "method not allowed, use POST",
		})
		return
	}
	var req tokenizeRequestR467
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": "invalid JSON body: " + err.Error(),
		})
		return
	}
	if req.Text == "" && len(req.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": "nothing to tokenize: pass \"text\" or \"messages\"",
		})
		return
	}
	if req.Model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": "model is required",
		})
		return
	}

	loaded := modelLoadedForTokenizeR467(req.Model)
	resp := tokenizeResponseR467{
		Model:       req.Model,
		ModelLoaded: loaded,
		TemplateNotice: "chat template, tools[] and the system preamble are NOT applied — " +
			"these are the tokens of the given strings only",
	}

	if req.Text != "" {
		resp.Chars = len([]rune(req.Text))
		resp.Tokens = countTokensSafeR467(req.Model, req.Text)
		if loaded {
			resp.Source = "tokenizer"
		} else {
			resp.Source = "estimate"
			resp.Note = "model is not loaded: the real tokenizer is unavailable, " +
				"this is pkg/tokencount heuristics (use GET /api/models/load to load it first)"
		}
	}

	if len(req.Messages) > 0 {
		resp.Messages = make([]tokenizeMessageResultR467, 0, len(req.Messages))
		total := 0
		chars := 0
		for _, m := range req.Messages {
			n := 0
			if m.Content != "" {
				n = countTokensSafeR467(req.Model, m.Content)
			}
			c := len([]rune(m.Content))
			total += n
			chars += c
			resp.Messages = append(resp.Messages, tokenizeMessageResultR467{
				Role: m.Role, Tokens: n, Chars: c,
			})
		}
		resp.Tokens = total
		resp.Chars = chars
		if loaded {
			resp.Source = "tokenizer"
		} else if resp.Source == "" {
			resp.Source = "estimate"
		}
	}

	logger.Get().Infow("tokenize: counted",
		"model", req.Model, "tokens", resp.Tokens, "source", resp.Source,
		"model_loaded", loaded, "chars", resp.Chars,
		"messages", len(req.Messages))
	writeJSON(w, http.StatusOK, resp)
}

// countTokensSafeR467 — точный подсчёт, безопасный при неинициализированном backend
// (unit-тесты и самый ранний старт воркера): без живой модели отдаём оценку
// pkg/tokencount, а не падаем нулевым указателем.
func countTokensSafeR467(modelName, text string) int {
	if text == "" {
		return 0
	}
	if backend == nil {
		return tokencount.Estimate(text)
	}
	return countTokens(modelName, text)
}

// modelLoadedForTokenizeR467 — загружена ли модель (есть ли живой токенизатор).
// nil-safe: в unit-тестах backend может быть не инициализирован.
func modelLoadedForTokenizeR467(modelName string) bool {
	if backend == nil || modelName == "" {
		return false
	}
	_, err := backend.GetModel(modelName)
	return err == nil
}
