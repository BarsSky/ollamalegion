// reload_params_r286_test.go — R286/R287 (2026-10-29): сравнение ВСЕХ значимых
// параметров загрузки и один непротиворечивый исход ответа.
//
// ЖИВЫЕ ДЕФЕКТЫ (стенд r284):
//  1. смена `kvCacheType` в WebUI давала `ok: already loaded` — параметр не
//     применялся, потому что сравнивалось только окно контекста;
//  2. ручной reload при занятой модели отвечал 500 «busy» ОДНОВРЕМЕННО с
//     `reloaded:true` — клиент не мог понять, применилось ли что-то.
package api

import (
	"net/http"
	"testing"
)

// strPtrR286 — локальный помощник (intPtr/boolPtr уже объявлены в других
// тестах этого пакета; дублировать их нельзя — компилятор справедливо ругается).
func strPtrR286(v string) *string { return &v }

// loadedExample — типовое «загружено»: окно 8192, kv f16, flash on, batch 512.
func loadedExample() loadedModelParams {
	fa := 1
	mmapOn := true
	return loadedModelParams{
		ContextSize:  8192,
		ContextKnown: true,
		KVCacheType:  "f16",
		FlashAttn:    &fa,
		Mmap:         &mmapOn,
		BatchSize:    512,
	}
}

// TestR286_LoadParamsDiff — таблица «что считается значимым отличием».
func TestR286_LoadParamsDiff(t *testing.T) {
	cases := []struct {
		name string
		req  clusterReloadModelRequest
		want []string // имена ожидаемых diff'ов, по порядку
	}{
		{
			name: "ничего не запрошено — перезагружать нечего",
			req:  clusterReloadModelRequest{},
			want: nil,
		},
		{
			name: "то же окно — не перезагружаем",
			req:  clusterReloadModelRequest{ContextSize: intPtr(8192)},
			want: nil,
		},
		{
			name: "другое окно (живой случай R281)",
			req:  clusterReloadModelRequest{ContextSize: intPtr(4096)},
			want: []string{"contextSize"},
		},
		{
			name: "ЖИВОЙ СЛУЧАЙ R286: тот же окно, другой kv-cache",
			req:  clusterReloadModelRequest{ContextSize: intPtr(8192), KVCacheType: strPtrR286("q4_0")},
			want: []string{"kvCacheType"},
		},
		{
			name: "kv-cache тот же (регистр не важен)",
			req:  clusterReloadModelRequest{KVCacheType: strPtrR286("F16")},
			want: nil,
		},
		{
			name: "flash-attn отличается",
			req:  clusterReloadModelRequest{FlashAttn: intPtr(0)},
			want: []string{"flashAttn"},
		},
		{
			name: "flash-attn = -1 (авто) не сравнивается",
			req:  clusterReloadModelRequest{FlashAttn: intPtr(-1)},
			want: nil,
		},
		{
			name: "mmap: было включено, просят выключить",
			req:  clusterReloadModelRequest{UseMmap: boolPtr(false)},
			want: []string{"useMmap"},
		},
		{
			name: "mmap: просят включить (недоказуемо, false = «не сообщил»)",
			req:  clusterReloadModelRequest{UseMmap: boolPtr(true)},
			want: nil,
		},
		{
			name: "batch отличается",
			req:  clusterReloadModelRequest{BatchSize: intPtr(1024)},
			want: []string{"batchSize"},
		},
		{
			name: "несколько отличий сразу — порядок детерминированный",
			req: clusterReloadModelRequest{
				ContextSize: intPtr(4096), KVCacheType: strPtrR286("q8_0"), BatchSize: intPtr(256),
			},
			want: []string{"contextSize", "kvCacheType", "batchSize"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			diffs := loadParamsDiff(loadedExample(), c.req)
			got := loadParamDiffNames(diffs)
			if len(got) != len(c.want) {
				t.Fatalf("diff = %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("diff[%d] = %q, want %q (весь: %v)", i, got[i], c.want[i], got)
				}
			}
			// В каждом diff'е обязаны быть оба значения: иначе оператор не поймёт,
			// что именно поменялось, и «перезагрузили» будет неотличимо от «нет».
			for _, d := range diffs {
				if d.Param == "" || d.Requested == "" {
					t.Fatalf("diff без имени/запрошенного: %+v", d)
				}
			}
		})
	}
}

// TestR286_UnknownLoadedValuesNeverTriggerReload — «неизвестно» не повод
// перезагружать: догадка жгла бы минуты на каждой команде.
func TestR286_UnknownLoadedValuesNeverTriggerReload(t *testing.T) {
	unknown := loadedModelParams{
		ContextKnown: false, // окно не сообщается
		KVCacheType:  "",    // kv-cache не сообщается
	}
	// Окно запрошено, факт неизвестен — это правило R281 (перезагружаем),
	// поэтому проверяем остальные параметры отдельно.
	diffs := loadParamsDiff(unknown, clusterReloadModelRequest{
		KVCacheType: strPtrR286("q4_0"),
		FlashAttn:   intPtr(1),
		UseMmap:     boolPtr(false),
		BatchSize:   intPtr(1024),
		GPULayers:   intPtr(20),
	})
	if len(diffs) != 0 {
		t.Fatalf("при неизвестных фактах перезагрузки быть не должно, got %v", loadParamDiffNames(diffs))
	}

	// А если факт загруженного окна известен и ОТЛИЧАЕТСЯ — diff есть.
	known := loadedExample()
	known.ContextSize, known.ContextKnown = 4096, true
	if d := loadParamsDiff(known, clusterReloadModelRequest{ContextSize: intPtr(8192)}); len(d) != 1 {
		t.Fatalf("известное окно 4096 против 8192 обязано дать diff, got %v", loadParamDiffNames(d))
	}
}

// TestR287_DeferredIsNotSuccess — «отложено» не несёт полей успеха.
func TestR287_DeferredIsNotSuccess(t *testing.T) {
	res := deferredReloadResult("bk-1", "model reload deferred: model busy; retry shortly", "model busy")
	if res.Deferred != true {
		t.Fatal("Deferred должно быть true")
	}
	if res.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("HTTPStatus = %d, want 503", res.HTTPStatus)
	}
	if res.Reloaded || len(res.ChangedParams) != 0 || res.Status == "ok" {
		t.Fatalf("ответ «отложено» не должен содержать признаков успеха: %+v", res)
	}
	if res.RetryAfterSec <= 0 {
		t.Fatal("должен быть retryAfterSec, иначе клиент не знает, когда повторять")
	}
	if res.DeferredReason == "" {
		t.Fatal("должна быть причина")
	}
}

// TestR287_OverallStatus — один HTTP-код на весь ответ из per-backend исходов.
func TestR287_OverallStatus(t *testing.T) {
	ok := clusterReloadModelBackendResult{HTTPStatus: http.StatusOK, Status: "ok"}
	def := deferredReloadResult("bk-2", "deferred", "model busy")
	fail := clusterReloadModelBackendResult{HTTPStatus: http.StatusInternalServerError, Status: "error"}

	cases := []struct {
		name    string
		results []clusterReloadModelBackendResult
		want    int
	}{
		{"все успешно", []clusterReloadModelBackendResult{ok, ok}, http.StatusOK},
		{"частичный успех — 200", []clusterReloadModelBackendResult{ok, def}, http.StatusOK},
		{"все отложены — 503", []clusterReloadModelBackendResult{def, def}, http.StatusServiceUnavailable},
		// R287: общий код 200 при реальной ошибке — СОЗНАТЕЛЬНО сохранённый
		// контракт (per-backend status/httpStatus/error несут правду; на 200
		// опираются существующие тесты и WebUI). 503 отдаётся только когда
		// отложены ВСЕ узлы — это и был живой дефект «200 + success:false».
		{"реальная ошибка — 200 с per-backend httpStatus", []clusterReloadModelBackendResult{fail}, http.StatusOK},
		{"ошибка + отложено — 200", []clusterReloadModelBackendResult{def, fail}, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := overallReloadStatus(c.results); got != c.want {
				t.Fatalf("overallReloadStatus = %d, want %d", got, c.want)
			}
		})
	}
}

// TestR287_BusyTextClassification — «занято» распознаётся по тексту воркера,
// чтобы не путать с реальной ошибкой.
func TestR287_BusyTextClassification(t *testing.T) {
	busy := []string{
		"unload failed: model is busy with active inference requests",
		"model busy",
		"reload already in progress for model",
	}
	for _, s := range busy {
		if reloadBusyReason(s) == "" {
			t.Errorf("%q должно классифицироваться как «занято»", s)
		}
	}
	notBusy := []string{
		"model file not found",
		"context size exceeds limit",
		"",
	}
	for _, s := range notBusy {
		if reloadBusyReason(s) != "" {
			t.Errorf("%q не должно классифицироваться как «занято»", s)
		}
	}
}
