// npredict_protocol_parity_r411_test.go — R411 (2026-11-03): n_predict — одна
// политика на обоих протоколах.
//
// ЖИВОЙ ДЕФЕКТ. Cline в VS Code (провайдер Ollama) шлёт на /api/chat только
// max_output_tokens и не задаёт num_predict. Ollama-путь оставлял дефолт моста
// 4096 даже для reasoning-модели, gemma-4 обрывалась на 4096-м токене посреди
// ответа, клиент получал done_reason=length и запускал overflow-recovery
// (compaction с потерей части переписки). На /v1/chat/completions та же модель
// получала 8192 (ResolveNPredict → DefaultNPredictReasoning) — протоколы
// расходились в семантике одного поля.
//
// Инварианты, которые здесь закреплены:
//  1. reasoning-модель на Ollama-пути без явного num_predict получает
//     DefaultNPredictReasoning (8192), а не 4096;
//  2. явный num_predict/max_tokens клиента приоритетнее политики;
//  3. верхняя граница max_output_tokens только УМЕНЬШАЕТ результат;
//  4. не-reasoning модель сохраняет прежнее поведение (дефолт моста 4096).
package main

import (
	"os"
	"testing"
)

// ollamaChatParamsR411 — собрать GenerationParams ровно тем же путём, что
// /api/chat: chatRequest → buildGenerateRequestFromChat → buildGenerationParams.
func ollamaChatParamsR411(t *testing.T, body string) int {
	t.Helper()
	req := chatRequest{}
	decodeRaw(t, body, &req)
	genReq := buildGenerateRequestFromChat(req, "PROMPT")
	return buildGenerationParams(genReq).NPredict
}

// TestR411_OllamaReasoningDefaultMatchesOpenAIPath — главный инвариант: та же
// модель, тот же отсутствующий num_predict → 8192, как на /v1.
func TestR411_OllamaReasoningDefaultMatchesOpenAIPath(t *testing.T) {
	body := `{"model":"gemma-4-E4B-it-Q4_K_M","messages":[{"role":"user","content":"hi"}],` +
		`"max_output_tokens":26284}`
	if got := ollamaChatParamsR411(t, body); got != DefaultNPredictReasoning {
		t.Fatalf("NPredict = %d, ожидалось %d (reasoning-модель без явного num_predict "+
			"должна получать ту же политику, что и /v1: DefaultNPredictReasoning)",
			got, DefaultNPredictReasoning)
	}
}

// TestR411_OllamaExplicitNumPredictWins — явное значение клиента не перебивается
// политикой (и раньше было приоритетным).
func TestR411_OllamaExplicitNumPredictWins(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{
			name: "options.num_predict",
			body: `{"model":"gemma-4-E4B-it-Q4_K_M","messages":[{"role":"user","content":"hi"}],` +
				`"options":{"num_predict":123}}`,
			want: 123,
		},
		{
			name: "top-level max_tokens",
			body: `{"model":"gemma-4-E4B-it-Q4_K_M","messages":[{"role":"user","content":"hi"}],` +
				`"max_tokens":77}`,
			want: 77,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ollamaChatParamsR411(t, tc.body); got != tc.want {
				t.Fatalf("NPredict = %d, ожидалось %d (явное значение клиента приоритетнее политики)", got, tc.want)
			}
		})
	}
}

// TestR411_OllamaClientUpperBoundClampsPolicy — max_output_tokens меньше политики
// обязан её уменьшить (R83/v67: «приму до N» — граница, а не запрошенная длина).
func TestR411_OllamaClientUpperBoundClampsPolicy(t *testing.T) {
	body := `{"model":"gemma-4-E4B-it-Q4_K_M","messages":[{"role":"user","content":"hi"}],` +
		`"max_output_tokens":4096}`
	if got := ollamaChatParamsR411(t, body); got != 4096 {
		t.Fatalf("NPredict = %d, ожидалось 4096: верхняя граница клиента обязана уменьшить "+
			"политику reasoning-модели (%d)", got, DefaultNPredictReasoning)
	}
}

// TestR411_OllamaNonReasoningUnchanged — не-reasoning модель не должна получить
// 8192: прежнее поведение (дефолт моста 4096) сохраняется.
func TestR411_OllamaNonReasoningUnchanged(t *testing.T) {
	body := `{"model":"llama-3.1-8b-instruct","messages":[{"role":"user","content":"hi"}]}`
	if got := ollamaChatParamsR411(t, body); got != 4096 {
		t.Fatalf("NPredict = %d, ожидалось 4096 (не-reasoning модель не должна получать "+
			"reasoning-дефолт)", got)
	}
}

// TestR411_OllamaEnvOverride — операторский override политики действует и на
// Ollama-пути (CPPWORKER_DEFAULT_N_PREDICT_REASONING).
func TestR411_OllamaEnvOverride(t *testing.T) {
	t.Setenv("CPPWORKER_DEFAULT_N_PREDICT_REASONING", "16384")
	body := `{"model":"gemma-4-E4B-it-Q4_K_M","messages":[{"role":"user","content":"hi"}]}`
	if got := ollamaChatParamsR411(t, body); got != 16384 {
		t.Fatalf("NPredict = %d, ожидалось 16384 из CPPWORKER_DEFAULT_N_PREDICT_REASONING", got)
	}
	// Переменная не должна утекать в другие тесты (t.Setenv восстанавливает сам).
	if v := os.Getenv("CPPWORKER_DEFAULT_N_PREDICT_REASONING"); v != "16384" {
		t.Fatalf("неожиданное значение env в тесте: %q", v)
	}
}

// TestR415_Qwen38IsReasoning — R415 (2026-11-03): Qwen3.8 (и 27B, и 9B
// distill/uncensored) обязан классифицироваться как reasoning-модель.
//
// ЖИВОЙ ДЕФЕКТ. В списке ReasoningArchPrefixes не было "qwen3.8": 27B и 9B
// Qwen3.8 на /api/chat получали дефолт моста 4096 вместо 8192 reasoning-политики
// (R411). При thinking-трейсе это снова обрыв посреди ответа и overflow-recovery
// у Cline — то же, что уже чинили для gemma-4.
func TestR415_Qwen38IsReasoning(t *testing.T) {
	for _, name := range []string{
		"Qwen3.8-9B-Distill-uncensored-heretic.Q4_K_M",
		"Qwen3.8-27B-UD-Q4_K_M",
	} {
		if !IsReasoningModel(name) {
			t.Fatalf("%s не распознан как reasoning — n_predict останется 4096", name)
		}
		if got := ResolveNPredict(0, name); got != DefaultNPredictReasoning {
			t.Fatalf("%s: ResolveNPredict = %d, ожидалось %d", name, got, DefaultNPredictReasoning)
		}
	}
	if IsReasoningModel("llama-3.1-8b-instruct") {
		t.Fatal("llama-3.1-8b-instruct не должен считаться reasoning")
	}
}
