//go:build llama_stub

// cluster_config_gputemp_r757_test.go — R757 (2026-11-25): порог температуры GPU
// из WebUI — сохранение, границы, отказ на мусор и видимость в состоянии кластера.
//
// ЖИВАЯ ЖАЛОБА. «В балансере постоянно фигурировала информация о возможном
// троттлинге из-за температуры выше пороговой, но явно об этом не сообщалось в
// WebUI и не понятно, какая на самом деле температура критична для видеокарты».
//
// Факт из архива второго стенда: балансер писал
// `backend GPU горячее заданного предела (soft, не блокирует) temperature=88
// threshold=85` 14 раз, а значения порога не было ни в одном endpoint'е панели.
//
// ЧТО ФИКСИРУЕТСЯ:
//  1. GET отдаёт действующее значение и границы (gpuMaxTemp/Default/Min/Max) —
//     панель не гадает числа;
//  2. PUT сохраняет значение в config.Resources.GPU.MaxTemperature (его читает
//     slot_manager.go) и пишет на диск через configSaver;
//  3. мусор (ниже 50, выше 120, не число) — 400 и НИЧЕГО не меняется;
//  4. 0 трактуется как «вернуть умолчание» (пустое поле в форме присылает 0);
//  5. состояние кластера несёт gpuMaxTemp — из него Monitor берёт порог для
//     подсветки температуры.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// putGpuMaxTempR757 — PUT /api/v1/cluster/config с одним полем gpuMaxTemp.
func putGpuMaxTempR757(s *Server, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/cluster/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.clusterConfigHandler(rec, req)
	return rec
}

// TestR757_GpuMaxTempSavedAndReturned — порог сохраняется и отдаётся обратно.
func TestR757_GpuMaxTempSavedAndReturned(t *testing.T) {
	s := newAgentCapacityServer(t)
	s.config.Resources.GPU.MaxTemperature = gpuMaxTempDefaultR757
	saved := 0
	s.configSaver = func() error { saved++; return nil }

	rec := putGpuMaxTempR757(s, `{"gpuMaxTemp": 92}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT gpuMaxTemp=92 → HTTP %d (%s)", rec.Code, rec.Body.String())
	}
	if got := s.config.Resources.GPU.MaxTemperature; got != 92 {
		t.Fatalf("config.Resources.GPU.MaxTemperature = %d, want 92 — soft-WARN перегрева "+
			"(slot_manager.go) читает именно это поле, значит порог не применился", got)
	}
	if saved == 0 {
		t.Error("configSaver не вызван: порог не сохранён в config.json и вернётся к прежнему после рестарта")
	}

	got := httptest.NewRecorder()
	s.clusterConfigHandler(got, httptest.NewRequest(http.MethodGet, "/api/v1/cluster/config", nil))
	if got.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/cluster/config → HTTP %d", got.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(got.Body.Bytes(), &resp); err != nil {
		t.Fatalf("GET-ответ не JSON: %v", err)
	}
	if v, _ := resp["gpuMaxTemp"].(float64); int(v) != 92 {
		t.Errorf("GET gpuMaxTemp = %v, want 92 — из этого ответа поле заполняется в Настройках", resp["gpuMaxTemp"])
	}
	// Границы нужны панели: по ним строится валидация поля.
	if v, _ := resp["gpuMaxTempDefault"].(float64); int(v) != gpuMaxTempDefaultR757 {
		t.Errorf("GET gpuMaxTempDefault = %v, want %d", resp["gpuMaxTempDefault"], gpuMaxTempDefaultR757)
	}
	if v, _ := resp["gpuMaxTempMin"].(float64); int(v) != gpuMaxTempMinR757 {
		t.Errorf("GET gpuMaxTempMin = %v, want %d", resp["gpuMaxTempMin"], gpuMaxTempMinR757)
	}
	if v, _ := resp["gpuMaxTempMax"].(float64); int(v) != gpuMaxTempMaxR757 {
		t.Errorf("GET gpuMaxTempMax = %v, want %d", resp["gpuMaxTempMax"], gpuMaxTempMaxR757)
	}
}

// TestR757_GpuMaxTempZeroResetsToDefault — 0 из пустого поля = «умолчание».
func TestR757_GpuMaxTempZeroResetsToDefault(t *testing.T) {
	s := newAgentCapacityServer(t)
	s.config.Resources.GPU.MaxTemperature = 95
	s.configSaver = func() error { return nil }

	rec := putGpuMaxTempR757(s, `{"gpuMaxTemp": 0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT gpuMaxTemp=0 → HTTP %d (%s), want 200: пустое поле формы присылает 0", rec.Code, rec.Body.String())
	}
	if got := s.config.Resources.GPU.MaxTemperature; got != gpuMaxTempDefaultR757 {
		t.Fatalf("после gpuMaxTemp=0 порог = %d, want %d (заводское значение)",
			got, gpuMaxTempDefaultR757)
	}
}

// TestR757_GpuMaxTempRejectsInvalid — мусорное значение не сохраняется.
//
// Назначение границ: порог SOFT, поэтому мусор не роняет кластер — он делает
// предупреждение всегда-истинным (или всегда-ложным), то есть невидимо ломает
// диагностику. Ровно так уже было с minFreeDisk (R700).
func TestR757_GpuMaxTempRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"ниже минимума", `{"gpuMaxTemp": 20}`},
		{"выше максимума", `{"gpuMaxTemp": 500}`},
		{"отрицательное", `{"gpuMaxTemp": -5}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newAgentCapacityServer(t)
			s.config.Resources.GPU.MaxTemperature = 85
			saved := 0
			s.configSaver = func() error { saved++; return nil }

			rec := putGpuMaxTempR757(s, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("PUT %s → HTTP %d (%s), want 400: значение вне [%d, %d] делает "+
					"предупреждение о перегреве бессмысленным, но не роняет кластер — "+
					"оператор обязан увидеть отказ",
					tc.body, rec.Code, rec.Body.String(), gpuMaxTempMinR757, gpuMaxTempMaxR757)
			}
			if got := s.config.Resources.GPU.MaxTemperature; got != 85 {
				t.Errorf("после отказа значение в конфиге = %d, want 85: отказ должен НИЧЕГО не менять", got)
			}
			if saved != 0 {
				t.Errorf("configSaver вызван %d раз при недопустимом значении — на диск уйдёт мусор", saved)
			}
			if !strings.Contains(rec.Body.String(), "gpuMaxTemp") {
				t.Errorf("в теле ошибки нет поля gpuMaxTemp: %s", rec.Body.String())
			}
		})
	}
}

// TestR757_ClusterStateCarriesGpuMaxTemp — порог едет в состоянии кластера.
//
// Monitor опрашивает /api/v1/cluster и берёт порог оттуда. Без этого поля
// температура рисовалась бы без оценки «горячо ли», то есть жалоба («не понятно,
// какая температура критична») осталась бы нерешённой.
func TestR757_ClusterStateCarriesGpuMaxTemp(t *testing.T) {
	s := newAgentCapacityServer(t)
	s.config.Resources.GPU.MaxTemperature = 88

	if s.proxy == nil {
		t.Skip("proxy не создан в этом окружении теста")
	}
	state := s.proxy.GetClusterState()
	if state == nil {
		t.Fatal("GetClusterState вернул nil")
	}
	if state.GPUMaxTemp != 88 {
		t.Fatalf("ClusterState.GPUMaxTemp = %d, want 88 — без этого поля панель не знает порог "+
			"и показывает температуру без оценки", state.GPUMaxTemp)
	}
}
