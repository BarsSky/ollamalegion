// max_output_tokens_r69_test.go — R69 (2026-09-23): балансер должен видеть
// `max_output_tokens`, который шлёт реальный клиент Cline.
//
// Зачем: preflight считает required = prompt + n_predict + slack. Если n_predict
// читается только из options.num_predict/max_tokens, для Cline он оставался 0 и
// подставлялся дефолт 2048 — требуемый контекст недооценивался (в логах живого
// стенда: n_predict=0 при том, что тело содержало max_output_tokens).
package balancer

import "testing"

func TestExtractRequestMeta_MaxOutputTokens_R69(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
		want int
	}{
		{
			name: "Ollama /api/chat (Cline)",
			path: "/api/chat",
			body: `{"model":"gemma","messages":[{"role":"user","content":"hi"}],"max_output_tokens":4096}`,
			want: 4096,
		},
		{
			name: "OpenAI /v1/chat/completions",
			path: "/v1/chat/completions",
			body: `{"model":"gemma","messages":[{"role":"user","content":"hi"}],"max_output_tokens":2048}`,
			want: 2048,
		},
		{
			name: "Ollama /api/generate",
			path: "/api/generate",
			body: `{"model":"gemma","prompt":"hi","max_output_tokens":777}`,
			want: 777,
		},
		{
			name: "options.num_predict приоритетнее",
			path: "/api/chat",
			body: `{"model":"gemma","messages":[{"role":"user","content":"hi"}],"options":{"num_predict":123},"max_output_tokens":4096}`,
			want: 123,
		},
		{
			name: "max_tokens приоритетнее",
			path: "/api/chat",
			body: `{"model":"gemma","messages":[{"role":"user","content":"hi"}],"max_tokens":55,"max_output_tokens":4096}`,
			want: 55,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := ExtractRequestMeta([]byte(tc.body), tc.path)
			if meta == nil {
				t.Fatalf("ExtractRequestMeta вернул nil для %s", tc.body)
			}
			if meta.RequestedNPredict != tc.want {
				t.Errorf("RequestedNPredict = %d, ожидалось %d", meta.RequestedNPredict, tc.want)
			}
		})
	}
}
