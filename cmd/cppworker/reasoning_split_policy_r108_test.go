//go:build llama_stub

// reasoning_split_policy_r108_test.go — R108 (2026-10-13): защита правила
// «разбор ответа — по формату, а не по имени модели».
//
// Живой дефект. Клиент Cline через балансер (:18080, модель Qwen3.8-27B)
// получал ответ, «перемешанный» с маркерами размышлений: пути ОТВЕТА решали,
// разделять ли (reasoning, content), по имени модели — IsReasoningModel(name)
// (whitelist префиксов) и IsReasoningEnabledForRequest(name). Qwen3.8 в список
// не входит, поэтому весь текст, включая ` thinking…`, уезжал в content.
//
// Правило (подробно — блок «Политика отдачи reasoning» в reasoning_content.go):
//  1. Разделение — всегда, по тегам формата (SplitReasoningContent /
//     ReasoningStreamState). Нет тегов — весь текст остаётся content'ом.
//  2. Имя модели и per-model флаг влияют ТОЛЬКО на промпт и дефолты n_predict.
//  3. Явный think=false — размышления не отдаются вовсе (см.
//     ReasoningWantedByClient).
//
// Здесь две проверки:
//   - поведенческая: поток модели ВНЕ whitelist не выпускает ни одного символа
//     маркера/текста размышлений в content, даже если тег разрезан между чанками;
//   - статическая: в путях ответа (handlers_chat.go, handlers_generate.go,
//     handlers_openai.go) нет гейтов по имени модели — иначе дефект вернётся.
package main

import (
	"os"
	"strings"
	"testing"
)

// TestR108_NonWhitelistedModelMarkersNeverReachContent — сквозная проверка
// правила на модели, которой нет ни в whitelist, ни в per-model состоянии.
func TestR108_NonWhitelistedModelMarkersNeverReachContent(t *testing.T) {
	const model = "Qwen3.8-27B-UD-Q4_K_M"

	// Фиксируем предпосылку: именно для такой модели старый гейт был false, и
	// ответ уходил в content целиком. Если предпосылка изменилась (модель попала
	// в whitelist), тест перестаёт проверять дефект.
	if IsReasoningModel(model) {
		t.Fatalf("предпосылка теста сломана: %q попала в IsReasoningModel whitelist — "+
			"выберите другое имя модели вне списка", model)
	}
	if IsReasoningEnabledForRequest(model) {
		t.Fatalf("предпосылка теста сломана: IsReasoningEnabledForRequest(%q)=true "+
			"(backend в тестах nil, ожидался fallback на whitelist=false)", model)
	}

	cases := []struct {
		name            string
		chunks          []string
		wantContent     string
		wantReasoningIn string
	}{
		{
			name:   "qwen-подобный <think>, тег разрезан между чанками",
			chunks: []string{"<thi", "nk>думаю ", "дол", "го</thi", "nk>Ответ: 4.", " всё"},
			// Маркеры приходят по частям — ни один чанк не должен утечь в content.
			wantContent:     "Ответ: 4. всё",
			wantReasoningIn: "думаю долго",
		},
		{
			name:            "gemma-4 native <|channel>thought, маркер разрезан",
			chunks:          []string{"<|chan", "nel>thou", "ght\nрассуж", "даю\n<chan", "nel|>От", "вет"},
			wantContent:     "Ответ",
			wantReasoningIn: "рассуждаю",
		},
		{
			name:            "без размышлений — весь текст остаётся ответом",
			chunks:          []string{"Просто ", "ответ ", "без тегов"},
			wantContent:     "Просто ответ без тегов",
			wantReasoningIn: "",
		},
	}

	markers := []string{
		"<think", "</think", "<thinking", "</thinking",
		"<reasoning", "</reasoning", "<analysis", "</analysis",
		"<|channel", "<channel|", "<|think", "<think|",
	}
	reasoningWords := []string{"думаю", "рассуждаю"}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Путь ОТВЕТА: парсер всегда включён (никакого гейта по имени модели).
			st := NewReasoningStreamState()
			var content, reasoning strings.Builder
			// Дельты контента проверяем по мере прихода — именно так их видит клиент.
			for _, chunk := range tc.chunks {
				rDelta, cDelta := st.Feed(chunk)
				for _, m := range markers {
					if strings.Contains(cDelta, m) {
						t.Errorf("маркер размышлений %q утёк в content-дельту %q "+
							"(модель %q вне whitelist — разбор обязан быть безусловным)",
							m, cDelta, model)
					}
				}
				for _, w := range reasoningWords {
					if strings.Contains(cDelta, w) {
						t.Errorf("текст размышлений %q утёк в content-дельту %q", w, cDelta)
					}
				}
				content.WriteString(cDelta)
				reasoning.WriteString(rDelta)
			}
			rTail, cTail := st.Finalize()
			content.WriteString(cTail)
			reasoning.WriteString(rTail)

			if got := content.String(); got != tc.wantContent {
				t.Errorf("content = %q, want %q", got, tc.wantContent)
			}
			if tc.wantReasoningIn == "" {
				if got := reasoning.String(); strings.TrimSpace(got) != "" {
					t.Errorf("reasoning = %q, want пусто (в выводе нет тегов)", got)
				}
				return
			}
			if got := reasoning.String(); !strings.Contains(got, tc.wantReasoningIn) {
				t.Errorf("reasoning = %q, ожидалось вхождение %q", got, tc.wantReasoningIn)
			}
		})
	}
}

// TestR108_ReasoningWantedByClient — правило 3: think=false скрывает
// размышления, отсутствие think — отдаёт их отдельным полем.
func TestR108_ReasoningWantedByClient(t *testing.T) {
	if !ReasoningWantedByClient(nil) {
		t.Error("think не задан → размышления должны отдаваться (true)")
	}
	yes, no := true, false
	if !ReasoningWantedByClient(&yes) {
		t.Error("think=true → размышления должны отдаваться (true)")
	}
	if ReasoningWantedByClient(&no) {
		t.Error("think=false → размышления не должны отдаваться (false)")
	}
}

// TestR108_VisibleContentCleanedOnAllSurfaces — статическая проверка: видимый
// текст на НЕпотоковых поверхностях проходит через cleanFinalContent.
//
// Живой дефект (2026-10-13, gemma-4 через балансер :18080): ответ пришёл как
// «ок<end_of_turn> » — служебный токен шаблона остался в content, потому что
// 1) за токеном был пробел (HasSuffix не срабатывал) и 2) на OpenAI-поверхности
// чистка вообще не применялась. Один и тот же ответ обязан выглядеть одинаково
// на всех поверхностях (требование «ollama api работает одинаково для всех»).
func TestR108_VisibleContentCleanedOnAllSurfaces(t *testing.T) {
	cases := []struct {
		file      string
		fragments []string
	}{
		{"handlers_chat.go", []string{"cleanFinalContent(output)"}},
		{"handlers_openai.go", []string{
			"cleanFinalContent(visibleOut)",
			"cleanFinalContent(result.Output)",
			"cleanFinalContent(textValue)",
		}},
		{"handlers_generate.go", []string{
			"cleanFinalContent(response)",
			"genParser.Feed(token)",
			"SplitReasoningContent(result.Output)",
		}},
	}
	for _, tc := range cases {
		src, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("не удалось прочитать %s: %v", tc.file, err)
		}
		text := string(src)
		for _, frag := range tc.fragments {
			if !strings.Contains(text, frag) {
				t.Errorf("%s: не найден вызов %s — видимый ответ на этой поверхности "+
					"не чистится от служебных токенов шаблона, и клиент увидит их как "+
					"текст ответа (дефект «ок<end_of_turn>»)", tc.file, frag)
			}
		}
	}
}

// TestR108_ResponsePathsDoNotGateReasoningByModelName — статическая защита:
// в путях ОТВЕТА разбор reasoning'а не зависит от имени модели.
//
// Разрешено (это политика ПРОМПТА/дефолтов, а не разбора):
//   - PerModelReasoningEnabled — решает, просить ли модель размышлять
//     (buildChatPromptWithOptions);
//   - IsReasoningModel внутри ResolveNPredict и самой reasoning_content.go.
//
// Запрещено: гейтить emit/разбор через IsReasoningModel /
// IsReasoningEnabledForRequest в обработчиках ответа.
func TestR108_ResponsePathsDoNotGateReasoningByModelName(t *testing.T) {
	files := []string{"handlers_chat.go", "handlers_generate.go", "handlers_openai.go"}
	forbidden := []string{"IsReasoningModel(", "IsReasoningEnabledForRequest("}

	for _, name := range files {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("не удалось прочитать %s: %v", name, err)
		}
		// Комментарии не считаем: в них эти имена упоминаются как «так было раньше».
		text := stripLineComments(string(src))
		for _, token := range forbidden {
			if idx := strings.Index(text, token); idx >= 0 {
				line := 1 + strings.Count(text[:idx], "\n")
				t.Errorf("%s:%d: найден гейт %s в пути ответа. Разбор reasoning'а обязан "+
					"быть безусловным и определяться тегами формата — иначе модель вне "+
					"whitelist отдаёт маркеры размышлений в content (дефект R108, Cline + "+
					"Qwen3.8-27B). Имя модели/флаг допустимы только для промпта и дефолтов "+
					"(PerModelReasoningEnabled, ResolveNPredict).",
					name, line, token)
			}
		}
	}
}
