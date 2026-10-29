// R300 (2026-10-29): «модель ещё загружается» — это 202 Accepted в HTTP-слое
// кластерного reload, а не `status:"error"` + `httpStatus:500`.
//
// ЖИВОЙ ФАКТ (стенд b5b45c74, Qwen3.8-27B-UD-Q4_K_M, 16.4 ГБ). POST
// /api/v1/cluster/models/Qwen3.8-27B-UD-Q4_K_M/reload {"operation":"load"}
// вернул через 6m26s:
//
//	{"status":"error","httpStatus":500,
//	 "message":"модель «…» ещё загружается на бэкенде «…» (ждали 6m26s): … это не ошибка"}
//
// Текст говорил «это не ошибка», код — «сломалось». Формат R262/R299
// (StillLoading=true, Error пуст) в балансере уже был, но HTTP-слой смотрел
// только на Success.
//
// Там же: пока модель грузилась, повторный `{"operation":"load"}` отвечал за
// 0.22 с `200 {"status":"ok","message":"already loaded"}`, хотя в тот же момент
// `/api/v1/cluster/models/loaded` показывал `"state":"loading"`.
package api

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR300_StillLoadingResultIs202 — формат «ещё идёт»: 202 + stillLoading, и ни
// одного поля ошибки.
func TestR300_StillLoadingResultIs202(t *testing.T) {
	r := stillLoadingReloadResultR300("b1", "Qwen3.8-27B-UD-Q4_K_M", "")

	if r.HTTPStatus != http.StatusAccepted {
		t.Fatalf("httpStatus=%d, want 202 Accepted", r.HTTPStatus)
	}
	if !r.StillLoading {
		t.Error("stillLoading=false — клиент не отличит «ещё идёт» от ошибки")
	}
	if r.PollAfterSec <= 0 {
		t.Errorf("pollAfterSec=%d, want >0 (клиенту нужен совет, когда опросить)", r.PollAfterSec)
	}
	if r.Error != "" {
		t.Errorf("error=%q, want пусто: 202 не имеет права нести ошибку", r.Error)
	}
	if r.Status != "started" {
		t.Errorf("status=%q, want \"started\"", r.Status)
	}
	if !strings.Contains(r.Message, "не ошибка") {
		t.Errorf("message=%q — оператор должен прочитать, что это не провал", r.Message)
	}
	if r.Deferred {
		t.Error("deferred=true конфликтует с 202: отложенность — это 503 + reason (R287)")
	}
}

// TestR300_ModelLoadState — состояние берётся из cluster state, а не из факта
// наличия записи: именно эта разница и породила «already loaded» во время загрузки.
func TestR300_ModelLoadState(t *testing.T) {
	cs := &types.ClusterState{
		Backends: []types.BackendMetrics{
			{
				ID: "b1",
				LlamaCpp: types.LlamaCppMetrics{
					LoadedModels: []types.LlamaCppModel{
						{Name: "gemma-4-E4B-it-Q4_K_M", State: "loaded"},
					},
					LoadingModels: []types.LlamaCppModel{
						{Name: "Qwen3.8-27B-UD-Q4_K_M", State: "loading"},
					},
				},
			},
			{
				ID: "b2",
				LlamaCpp: types.LlamaCppMetrics{
					LoadedModels: []types.LlamaCppModel{
						{Name: "broken", State: "error"},
					},
				},
			},
		},
	}

	cases := []struct {
		name   string
		state  *types.ClusterState
		back   string
		model  string
		expect string
	}{
		{"загружена", cs, "b1", "gemma-4-E4B-it-Q4_K_M", "loaded"},
		{"грузится", cs, "b1", "Qwen3.8-27B-UD-Q4_K_M", "loading"},
		{"ошибка", cs, "b2", "broken", "error"},
		{"нет записи", cs, "b1", "нет-такой", ""},
		{"чужой бэкенд", cs, "b2", "gemma-4-E4B-it-Q4_K_M", ""},
		{"нет состояния", nil, "b1", "gemma-4-E4B-it-Q4_K_M", ""},
		{"с :tag", cs, "b1", "Qwen3.8-27B-UD-Q4_K_M:latest", "loading"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := modelLoadStateR300(c.state, c.back, c.model); got != c.expect {
				t.Fatalf("modelLoadStateR300=%q, want %q", got, c.expect)
			}
		})
	}
}

// TestR300_OverallStatusAccepted — общий код ответа: успех на любом узле важнее,
// иначе «ещё грузится» отдаётся как 202, а не 200.
func TestR300_OverallStatusAccepted(t *testing.T) {
	mk := func(codes ...int) []clusterReloadModelBackendResult {
		out := make([]clusterReloadModelBackendResult, 0, len(codes))
		for i, c := range codes {
			out = append(out, clusterReloadModelBackendResult{
				BackendID:  "b",
				HTTPStatus: c,
			})
			_ = i
		}
		return out
	}

	cases := []struct {
		name    string
		results []clusterReloadModelBackendResult
		want    int
	}{
		{"всё применилось", mk(200), http.StatusOK},
		{"успех + ещё грузится", mk(200, 202), http.StatusOK},
		{"только ещё грузится", mk(202), http.StatusAccepted},
		{"два узла ещё грузятся", mk(202, 202), http.StatusAccepted},
		{"ещё грузится + ошибка", mk(202, 500), http.StatusAccepted},
		{"все отложены", []clusterReloadModelBackendResult{
			{BackendID: "a", HTTPStatus: 503, Deferred: true},
			{BackendID: "b", HTTPStatus: 503, Deferred: true},
		}, http.StatusServiceUnavailable},
		{"ничего не выбрано", nil, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := overallReloadStatus(c.results); got != c.want {
				t.Fatalf("overallReloadStatus=%d, want %d", got, c.want)
			}
		})
	}
}

// TestR300_HandlersKeepStillLoadingContract — защита от отката: ветка
// StillLoading стоит в HTTP-слое ДО превращения в error, а «уже загружено»
// проверяется после состояния loading.
func TestR300_HandlersKeepStillLoadingContract(t *testing.T) {
	src, err := os.ReadFile("handlers_cluster_models.go")
	if err != nil {
		t.Fatalf("read handlers_cluster_models.go: %v", err)
	}
	text := string(src)

	if !strings.Contains(text, "if opResult.StillLoading {") {
		t.Fatal("HTTP-слой кластерного reload больше не различает StillLoading: " +
			"«модель ещё загружается» снова станет error/500 (живой дефект b5b45c74)")
	}
	if !strings.Contains(text, "modelLoadStateOnBackend(backendID, modelName); st == modelStateLoadingR300") {
		t.Fatal("пропала проверка состояния loading перед ответом «already loaded»")
	}

	// Порядок в теле executeReloadOnBackend: проверка «loading» должна стоять
	// ВЫШЕ ветки «already loaded», иначе грузящаяся модель снова получит «ok».
	body := text[strings.Index(text, "func (s *Server) executeReloadOnBackend"):]
	body = body[:strings.Index(body, "\nfunc (")]
	loadingAt := strings.Index(body, "if st := s.modelLoadStateOnBackend(backendID, modelName); st == modelStateLoadingR300")
	alreadyAt := strings.Index(body, `result.Message = "already loaded"`)
	if loadingAt < 0 || alreadyAt < 0 {
		t.Fatalf("не найдены маркеры: loading=%d alreadyLoaded=%d", loadingAt, alreadyAt)
	}
	if loadingAt > alreadyAt {
		t.Error("проверка «loading» стоит после «already loaded» — грузящаяся модель " +
			"получит ложное «уже загружено»")
	}
}
