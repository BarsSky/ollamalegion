//go:build llama_stub

// cluster_config_disk_r700_test.go — R700 (2026-11-24): порог диска из WebUI —
// сохранение, проверка и обратимость.
//
// ЖИВОЙ ДЕФЕКТ ДО ПРАВКИ. PUT /api/v1/cluster/config принимал minFreeDisk как
// есть: `uint64(*req.MinFreeDisk)`. Отрицательное число превращалось в
// 18446744073709551615, и гейт диска начинал отбраковывать ВСЕ узлы — при
// ответе «success: true». Оператор не видел ни ошибки, ни причины.
//
// ЧТО ФИКСИРУЕТСЯ:
//  1. корректное значение сохраняется в config (тот же объект читает proxy) и
//     записывается на диск через configSaver;
//  2. GET отдаёт действующее значение и границы (minFreeDiskDefault/Max) —
//     панель не гадает числа для валидации и кнопки «по умолчанию»;
//  3. недопустимое значение — 400 и НИЧЕГО не меняется: конфиг, файл и гейт
//     остаются прежними.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// putMinFreeDiskR700 — PUT /api/v1/cluster/config с одним полем minFreeDisk.
func putMinFreeDiskR700(s *Server, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/cluster/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.clusterConfigHandler(rec, req)
	return rec
}

// TestR700_MinFreeDiskSavedAndReturned — порог сохраняется и отдаётся обратно.
func TestR700_MinFreeDiskSavedAndReturned(t *testing.T) {
	s := newAgentCapacityServer(t)
	// Заводской порог — как после config.Load.
	s.config.Resources.Disk.MinFreeMB = 10240
	saved := 0
	s.configSaver = func() error { saved++; return nil }

	rec := putMinFreeDiskR700(s, `{"minFreeDisk": 4096}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT minFreeDisk=4096 → HTTP %d (%s)", rec.Code, rec.Body.String())
	}
	if got := s.config.Resources.Disk.MinFreeMB; got != 4096 {
		t.Fatalf("config.Resources.Disk.MinFreeMB = %d, want 4096 — гейт читает именно это поле "+
			"(slot_manager.go, diskLowR696), значит порог не применился", got)
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
	if v, _ := resp["minFreeDisk"].(float64); int64(v) != 4096 {
		t.Errorf("GET minFreeDisk = %v, want 4096 — WebUI заполняет поле из этого ответа", resp["minFreeDisk"])
	}
	// Границы нужны панели: по ним строится валидация и кнопка «по умолчанию».
	if v, _ := resp["minFreeDiskDefault"].(float64); int64(v) != 10240 {
		t.Errorf("GET minFreeDiskDefault = %v, want 10240 (заводской порог из config.Load)", resp["minFreeDiskDefault"])
	}
	if v, _ := resp["minFreeDiskMax"].(float64); int64(v) != 1048576 {
		t.Errorf("GET minFreeDiskMax = %v, want 1048576 (верхняя граница валидации)", resp["minFreeDiskMax"])
	}
}

// TestR700_MinFreeDiskRejectsInvalid — мусорное значение не должно ни сохраняться,
// ни попадать в гейт.
func TestR700_MinFreeDiskRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"отрицательное", `{"minFreeDisk": -1}`},
		{"больше максимума", `{"minFreeDisk": 5000000}`},
		{"огромное", `{"minFreeDisk": 1e18}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newAgentCapacityServer(t)
			s.config.Resources.Disk.MinFreeMB = 10240
			saved := 0
			s.configSaver = func() error { saved++; return nil }

			rec := putMinFreeDiskR700(s, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("PUT %s → HTTP %d (%s), want 400: до R700 отрицательное значение "+
					"превращалось в uint64-переполнение и блокировало ВСЕ узлы по диску, "+
					"а ответ говорил success", tc.body, rec.Code, rec.Body.String())
			}
			if got := s.config.Resources.Disk.MinFreeMB; got != 10240 {
				t.Errorf("после отказа значение в конфиге = %d, want 10240: отказ должен НИЧЕГО не менять", got)
			}
			if saved != 0 {
				t.Errorf("configSaver вызван %d раз при недопустимом значении — на диск уйдёт мусор", saved)
			}
			if !strings.Contains(rec.Body.String(), "minFreeDisk") {
				t.Errorf("в теле ошибки нет поля minFreeDisk: %s", rec.Body.String())
			}
		})
	}
}
