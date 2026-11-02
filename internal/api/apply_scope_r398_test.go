//go:build llama_stub

// apply_scope_r398_test.go — R398 (2026-11-04).
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «не применяется профиль по кнопкам „применить и
// перезагрузить“, затрагивается другой бэкенд, а не тот, на котором выбрал».
//
// ПРИЧИНА. `GetAllBackends()` собирает срез из map — порядок между двумя
// вызовами РАЗНЫЙ. Прежний reload-цикл проверял фильтр по
// `GetAllBackends()[i]`, а грузил `GetAllBackends()[i]` из ВТОРОГО вызова, то
// есть индекс мог указывать на другой узел: выбранный бэкенд не перезагружался,
// а перезагружался чужой/устаревший (в ответе apply появлялись cppworker-donor /
// fake-donor с connection refused).
//
// Тесты: (1) в applyModelProfile ровно ОДИН снимок состава; (2) удаление
// бэкенда доезжает до config.json — иначе после рестарта узел возвращается и его
// снова дёргают apply/перезагрузки.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR398_ApplyUsesSingleBackendSnapshot — стюард: второй снимок состава в
// applyModelProfile означал бы рассинхрон индекса и «чужой бэкенд».
func TestR398_ApplyUsesSingleBackendSnapshot(t *testing.T) {
	src, err := os.ReadFile("handlers_cppworker_profiles.go")
	if err != nil {
		t.Skipf("handlers_cppworker_profiles.go недоступен: %v", err)
	}
	text := string(src)
	start := strings.Index(text, "func (s *Server) applyModelProfile(")
	if start < 0 {
		t.Fatal("applyModelProfile не найдена — стюард нужно обновить")
	}
	// До следующей функции того же уровня (func (s *Server) reloadModelOnCppWorker).
	end := strings.Index(text[start:], "func (s *Server) reloadModelOnCppWorker(")
	if end < 0 {
		t.Fatal("не найден конец applyModelProfile")
	}
	body := text[start : start+end]
	if n := strings.Count(body, "s.proxy.GetAllBackends()"); n > 1 {
		t.Fatalf("в applyModelProfile %d вызовов s.proxy.GetAllBackends(): порядок среза нестабилен, "+
			"индекс из одного вызова указывает на другой узел — «применение» уходит на чужой бэкенд (R398)", n)
	}
}

// TestR398_DeleteRemovesBackendFromConfig — удалённый узел не должен
// воскресать после рестарта и попадать в apply/загрузки.
func TestR398_DeleteRemovesBackendFromConfig(t *testing.T) {
	s, id := newKeptRecordServer(t, "") // без роли: DELETE удаляет запись

	s.config.Backends = []types.Backend{
		{ID: id, Type: types.BackendTypeLlamaCpp},
		{ID: "keep-me", Type: types.BackendTypeLlamaCpp},
	}
	saved := 0
	s.configSaver = func() error { saved++; return nil }

	rec := httptest.NewRecorder()
	s.deleteBackend(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/backends/"+id, nil), id)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE вернул HTTP %d (%s)", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if rm, _ := resp["removedFromConfig"].(bool); !rm {
		t.Errorf("ответ не сообщает об удалении из config.json: %s", rec.Body.String())
	}
	for _, b := range s.config.Backends {
		if b.ID == id {
			t.Fatalf("удалённый бэкенд %q остался в config.Backends — после рестарта он вернётся", id)
		}
	}
	if len(s.config.Backends) != 1 || s.config.Backends[0].ID != "keep-me" {
		t.Fatalf("пострадали другие бэкенды конфига: %+v", s.config.Backends)
	}
	if saved == 0 {
		t.Error("configSaver не вызван — изменение конфига не сохранится на диск")
	}
}
