//go:build llama_stub

// model_name_match_r66d_test.go — R66d (2026-09-23).
//
// РЕГРЕСС на ложную ошибку загрузки из WebUI, воспроизведённую в реальном
// браузере: карточка локального файла отдаёт имя С расширением
// ("Qwen3-Instruct-2507-q4km.gguf"), cppworker регистрирует загруженную модель
// БЕЗ расширения ("Qwen3-Instruct-2507-q4km"). Балансер сравнивал имена точно,
// 5 поллов подряд не находил модель и объявлял загрузку провалившейся:
//
//	success:false, error: auto-load failed: model "…q4km.gguf" did not appear in
//	cppworker model list or load progress for 5 consecutive polls
//
// при этом модель была загружена (в WebUI одновременно «Ошибка загрузки» и
// карточка загруженной модели с runtime-параметрами).
package balancer

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestModelNameMatches_R66d(t *testing.T) {
	tests := []struct {
		name      string
		candidate string
		wanted    string
		want      bool
	}{
		{"exact", "gemma-4", "gemma-4", true},
		{"case_insensitive", "Gemma-4", "gemma-4", true},
		{"gguf_suffix_on_request", "Qwen3-Instruct-2507-q4km", "Qwen3-Instruct-2507-q4km.gguf", true},
		{"gguf_suffix_on_candidate", "Qwen3-Instruct-2507-q4km.gguf", "Qwen3-Instruct-2507-q4km", true},
		{"gguf_suffix_both_case", "Qwen3-Instruct-2507-q4KM.GGUF", "qwen3-instruct-2507-q4km", true},
		{"path_on_candidate", "/app/models/gemma-4-E4B-it-Q4_K_M.gguf", "gemma-4-E4B-it-Q4_K_M", true},
		{"partial_profile_name", "gemma-4-E4B-it-Q4_K_M", "gemma-4", true},
		{"partial_request_name", "gemma-4", "gemma-4-E4B-it-Q4_K_M", true},
		{"different_models", "Qwen3.8-27B", "gemma-4-E4B-it-Q4_K_M", false},
		{"empty_candidate", "", "gemma-4", false},
		{"empty_wanted", "gemma-4", "", false},
		{"only_extension", "gemma-4.gguf", ".gguf", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelNameMatches(tc.candidate, tc.wanted); got != tc.want {
				t.Errorf("modelNameMatches(%q, %q) = %v, want %v", tc.candidate, tc.wanted, got, tc.want)
			}
		})
	}
}

// TestExecuteLlamaCppLoad_PollingAcceptsNameWithoutGGUF — главный регресс:
// загрузка, запрошенная по имени файла, не должна объявляться провалившейся,
// если cppworker отдаёт имя без расширения.
func TestExecuteLlamaCppLoad_PollingAcceptsNameWithoutGGUF(t *testing.T) {
	const requested = "Qwen3-Instruct-2507-q4km.gguf"
	const canonical = "Qwen3-Instruct-2507-q4km"

	loadPosts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/load", "/api/models/load-with-params":
			loadPosts++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"status":"loading","name":"` + canonical + `"}`))
		case "/api/models":
			// cppworker отдаёт КАНОНИЧЕСКОЕ имя (без .gguf) — как в живом стеке.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"count":1,"models":[{"name":"` + canonical + `","state":"loaded"}]}`))
		case "/api/models/load/progress":
			// Живое поведение: реестр прогресса ключуется каноническим именем,
			// поэтому запрос с ".gguf" не находит запись и отдаёт пустой ответ.
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("model") == canonical {
				_, _ = w.Write([]byte(`{"state":"loaded","model":"` + canonical + `"}`))
				return
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	host, port := parseTestHostPort(t, srv.URL)
	mm := NewModelManager(nil)

	res := mm.executeLlamaCppLoad(host, port, "test-backend", ModelOpRequest{
		Operation: "load",
		ModelName: requested,
	})
	if res == nil {
		t.Fatal("expected non-nil result")
	}
	if !res.Success {
		t.Fatalf("загрузка с именем .gguf объявлена провалившейся, хотя cppworker отдаёт имя без расширения: %s", res.Error)
	}
	if loadPosts == 0 {
		t.Fatal("ни одного load-запроса к cppworker — тест не воспроизводит сценарий")
	}
}
