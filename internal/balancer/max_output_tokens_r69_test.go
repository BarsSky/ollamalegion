// max_output_tokens_r69_test.go — R69 (2026-09-23), переработан в R83/v67.
//
// ИСТОРИЯ. R69: балансер должен ВИДЕТЬ `max_output_tokens`, который шлёт реальный
// клиент Cline (иначе preflight считал n_predict=0 и подставлял дефолт 2048).
// Тогда его просто подставили в RequestedNPredict.
//
// R83/v67 (2026-10-02): это было неверно по существу. `max_output_tokens` — НЕ
// намерение «сгенерируй столько», а верхняя граница «столько я максимум приму»;
// Cline шлёт его большим «на всякий случай» (32000-64000). Подставленный в
// required = prompt + n_predict он раздувал требование к окну: балансер
// перезагружал модель в большее окно либо отвечал 413 на обычный короткий
// вопрос.
//
// ТЕПЕРЬ: значение по-прежнему читается (клиента надо видеть), но живёт
// отдельным полем MaxOutputTokensUpperBound и НЕ участвует ни в выборе n_ctx, ни
// в расчёте required, ни в решении о reload. RequestedNPredict заполняется только
// явным num_predict/max_tokens.
package balancer

import "testing"

func TestExtractRequestMeta_MaxOutputTokens_R69(t *testing.T) {
	cases := []struct {
		name         string
		path         string
		body         string
		wantNPredict int // ожидаемая ДЛИНА генерации (только явное намерение)
		wantUpper    *int // верхняя граница от клиента
	}{
		{
			name:         "Ollama /api/chat: только max_output_tokens → не намерение",
			path:         "/api/chat",
			body:         `{"model":"gemma","messages":[{"role":"user","content":"hi"}],"max_output_tokens":4096}`,
			wantNPredict: 0,
			wantUpper:    intPtr(4096),
		},
		{
			name:         "OpenAI /v1/chat/completions: только max_output_tokens",
			path:         "/v1/chat/completions",
			body:         `{"model":"gemma","messages":[{"role":"user","content":"hi"}],"max_output_tokens":2048}`,
			wantNPredict: 0,
			wantUpper:    intPtr(2048),
		},
		{
			name:         "Ollama /api/generate: только max_output_tokens",
			path:         "/api/generate",
			body:         `{"model":"gemma","prompt":"hi","max_output_tokens":777}`,
			wantNPredict: 0,
			wantUpper:    intPtr(777),
		},
		{
			name:         "options.num_predict — намерение клиента",
			path:         "/api/chat",
			body:         `{"model":"gemma","messages":[{"role":"user","content":"hi"}],"options":{"num_predict":123},"max_output_tokens":4096}`,
			wantNPredict: 123,
			wantUpper:    intPtr(4096),
		},
		{
			name:         "max_tokens — намерение клиента",
			path:         "/api/chat",
			body:         `{"model":"gemma","messages":[{"role":"user","content":"hi"}],"max_tokens":55,"max_output_tokens":4096}`,
			wantNPredict: 55,
			wantUpper:    intPtr(4096),
		},
		{
			name:         "Cline-подобный большой max_output_tokens не становится длиной",
			path:         "/v1/chat/completions",
			body:         `{"model":"gemma","messages":[{"role":"user","content":"2+2?"}],"max_output_tokens":64000}`,
			wantNPredict: 0,
			wantUpper:    intPtr(64000),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := ExtractRequestMeta([]byte(tc.body), tc.path)
			if meta == nil {
				t.Fatalf("ExtractRequestMeta вернул nil для %s", tc.body)
			}
			if meta.RequestedNPredict != tc.wantNPredict {
				t.Errorf("RequestedNPredict = %d, ожидалось %d (max_output_tokens не должен "+
					"становиться ожидаемой длиной ответа — иначе он раздувает требование к окну)",
					meta.RequestedNPredict, tc.wantNPredict)
			}
			upper := meta.MaxOutputTokensUpperBound
			if tc.wantUpper == nil {
				if upper != nil {
					t.Errorf("MaxOutputTokensUpperBound = %d, ожидалось nil", *upper)
				}
				return
			}
			if upper == nil {
				t.Fatalf("MaxOutputTokensUpperBound = nil, ожидалось %d (значение обязано "+
					"читаться хотя бы для диагностики)", *tc.wantUpper)
			}
			if *upper != *tc.wantUpper {
				t.Errorf("MaxOutputTokensUpperBound = %d, ожидалось %d", *upper, *tc.wantUpper)
			}
		})
	}
}

// TestRequestedNPredict_DoesNotUseUpperBound — отдельный страж инварианта:
// верхняя граница не должна влиять на требуемый контекст.
func TestRequestedNPredict_DoesNotUseUpperBound(t *testing.T) {
	body := []byte(`{"model":"gemma","messages":[{"role":"user","content":"hi"}],"max_output_tokens":64000}`)
	meta := ExtractRequestMeta(body, "/v1/chat/completions")
	if meta == nil {
		t.Fatal("ExtractRequestMeta вернул nil")
	}
	required := requiredNCtxForMeta(meta)
	if required >= 64000 {
		t.Fatalf("required n_ctx = %d: верхняя граница max_output_tokens просочилась в расчёт "+
			"окна (это вернуло бы лишний reload/413 на короткий запрос)", required)
	}
}

func intPtr(v int) *int { return &v }
