// handlers_image_resources_r91_test.go — R91 (2026-10-09): частичное обновление
// политики сосуществования не должно стирать остальные поля.
//
// НАБЛЮДЕНИЕ: PUT /api/v1/image/resources с одним полем (тумблер «разрешить
// загрузку из инструмента», curl оператора) заново сохранял политику целиком, а
// отсутствующие в теле поля брались нулевыми: coexistence → "" (=exclusive),
// vramHeadroomMb → 0, queueWaitTimeoutSec → 0, exclusiveLockTimeoutSec → 0,
// gateDisabled → false. Валидация это пропускает, ответ 200 — то есть потеря
// настроек была бесшумной.
package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"ollama-loadbalancer/internal/config"
	"ollama-loadbalancer/pkg/types"
)

func r91BoolPtr(b bool) *bool { return &b }
func r91IntPtr(i int) *int    { return &i }

// fullImageResourceSettings — все поля заданы, чтобы потеря любого была видна.
func fullImageResourceSettings() types.ImageResourceSettings {
	return types.ImageResourceSettings{
		Coexistence:                types.ImageCoexistenceOffload,
		VramHeadroomMB:             1024,
		QueueWaitTimeoutSec:        120,
		ExclusiveLockTimeoutSec:    900,
		BlockOnUnknownVRAMEstimate: true,
		GateDisabled:               true,
		AllowToolLoad:              r91BoolPtr(true),
		ToolLoadTimeoutSec:         r91IntPtr(1800),
	}
}

func TestImageResourcesR91_PartialPutKeepsOtherFields(t *testing.T) {
	bundled := fullImageResourceSettings()
	s, store := newImageResourcesTestServer(t, bundled)

	rec := doImageResources(t, s, http.MethodPut, `{"allowToolLoad":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: status=%d body=%s", rec.Code, rec.Body.String())
	}

	got := s.config.Balancing.Image
	if got.Coexistence != types.ImageCoexistenceOffload {
		t.Errorf("coexistence потерян: %q", got.Coexistence)
	}
	if got.VramHeadroomMB != 1024 {
		t.Errorf("vramHeadroomMb потерян: %d", got.VramHeadroomMB)
	}
	if got.QueueWaitTimeoutSec != 120 {
		t.Errorf("queueWaitTimeoutSec потерян: %d", got.QueueWaitTimeoutSec)
	}
	if got.ExclusiveLockTimeoutSec != 900 {
		t.Errorf("exclusiveLockTimeoutSec потерян: %d", got.ExclusiveLockTimeoutSec)
	}
	if !got.BlockOnUnknownVRAMEstimate {
		t.Error("blockOnUnknownVramEstimate потерян (сброшен в false)")
	}
	if !got.GateDisabled {
		t.Error("gateDisabled потерян (сброшен в false)")
	}
	if got.AllowToolLoad == nil || *got.AllowToolLoad {
		t.Errorf("присланное allowToolLoad=false не применено: %+v", got.AllowToolLoad)
	}
	if got.ToolLoadTimeoutSec == nil || *got.ToolLoadTimeoutSec != 1800 {
		t.Errorf("toolLoadTimeoutSec потерян: %+v", got.ToolLoadTimeoutSec)
	}

	// То же должно лежать на диске: иначе потеря вернётся после рестарта.
	fresh := config.NewImageResourcesStore(store.Path())
	if err := fresh.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	saved := fresh.Settings()
	if saved.VramHeadroomMB != 1024 || saved.ExclusiveLockTimeoutSec != 900 ||
		saved.Coexistence != types.ImageCoexistenceOffload || !saved.GateDisabled {
		t.Fatalf("на диск уехала урезанная политика: %+v", saved)
	}
}

func TestImageResourcesR91_PartialPutKeepsPresenceForAllFields(t *testing.T) {
	s, _ := newImageResourcesTestServer(t, fullImageResourceSettings())

	// Тело без «числовых» и «булевых» полей: меняем только политику.
	if rec := doImageResources(t, s, http.MethodPut, `{"coexistence":"dedicated"}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT: status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := s.config.Balancing.Image
	if got.Coexistence != types.ImageCoexistenceDedicated {
		t.Errorf("coexistence не применён: %q", got.Coexistence)
	}
	if got.VramHeadroomMB != 1024 || got.QueueWaitTimeoutSec != 120 {
		t.Errorf("числовые поля потеряны: %+v", got)
	}
	// Указатели (allowToolLoad/toolLoadTimeoutSec) — тот же контракт «нет поля = не менять».
	if got.AllowToolLoad == nil || !*got.AllowToolLoad {
		t.Errorf("allowToolLoad потерян: %+v", got.AllowToolLoad)
	}
	if got.ToolLoadTimeoutSec == nil || *got.ToolLoadTimeoutSec != 1800 {
		t.Errorf("toolLoadTimeoutSec потерян: %+v", got.ToolLoadTimeoutSec)
	}
}

// TestImageResourcesR91_ExplicitZeroStillApplies — «нет поля» и «поле = 0» —
// разные вещи: явный ноль оператора обязан применяться (0 у headroom — легальное
// значение, а не «не задано»).
func TestImageResourcesR91_ExplicitZeroStillApplies(t *testing.T) {
	s, _ := newImageResourcesTestServer(t, fullImageResourceSettings())

	if rec := doImageResources(t, s, http.MethodPut, `{"vramHeadroomMb":0,"gateDisabled":false,"queueWaitTimeoutSec":0}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT: status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := s.config.Balancing.Image
	if got.VramHeadroomMB != 0 || got.QueueWaitTimeoutSec != 0 {
		t.Errorf("явный ноль не применён: %+v", got)
	}
	if got.GateDisabled {
		t.Error("явный false не применён (gateDisabled всё ещё true)")
	}
	if got.ExclusiveLockTimeoutSec != 900 || got.Coexistence != types.ImageCoexistenceOffload {
		t.Errorf("поля без явного значения потеряны: %+v", got)
	}
}

// TestImageResourcesR91_ResponseEchoesFullPolicy — ответ на частичный PUT — та
// же полная политика (UI перерисовывает форму из ответа; урезанный ответ
// выглядел бы в форме как «настройки сброшены»).
func TestImageResourcesR91_ResponseEchoesFullPolicy(t *testing.T) {
	s, _ := newImageResourcesTestServer(t, fullImageResourceSettings())

	rec := doImageResources(t, s, http.MethodPut, `{"allowToolLoad":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Effective types.ImageResourceSettings `json:"effective"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("не JSON: %s", rec.Body.String())
	}
	if doc.Effective.VramHeadroomMB != 1024 || doc.Effective.ExclusiveLockTimeoutSec != 900 {
		t.Fatalf("в ответе урезанная политика: %+v", doc.Effective)
	}
}
