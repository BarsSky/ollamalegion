// handlers_image_resources_test.go — R84: настройка политики сосуществования
// (balancing.image) через API.
//
// ЧТО ФИКСИРУЕМ:
//  1. GET отдаёт действующие значения, встроенные (config.json) и источник;
//  2. PUT валидирует ДО записи: опечатка в политике не должна попадать ни в файл,
//     ни в работающий гейт (иначе «поменял, а эффекта нет»);
//  3. PUT применяется мгновенно — меняется тот самый конфиг, который читает гейт
//     (тот же указатель, что у прокси);
//  4. DELETE возвращает ИМЕННО значения config.json, а не пустую структуру.
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"ollama-loadbalancer/internal/config"
	"ollama-loadbalancer/pkg/types"
)

func newImageResourcesTestServer(t *testing.T, bundled types.ImageResourceSettings) (*Server, *config.ImageResourcesStore) {
	t.Helper()
	store := config.NewImageResourcesStore(filepath.Join(t.TempDir(), "image-resources.json"))
	cfg := &types.LoadBalancerConfig{}
	cfg.Balancing.Image = bundled
	s := &Server{config: cfg}
	s.SetImageResourcesStore(store, bundled)
	return s, store
}

func doImageResources(t *testing.T, s *Server, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	rec := httptest.NewRecorder()
	s.handleImageResources(rec, httptest.NewRequest(method, "/api/v1/image/resources", reader))
	return rec
}

func TestImageResources_GetShowsEffectiveAndDefaults(t *testing.T) {
	bundled := types.ImageResourceSettings{Coexistence: types.ImageCoexistenceExclusive, VramHeadroomMB: 512}
	s, store := newImageResourcesTestServer(t, bundled)

	rec := doImageResources(t, s, http.MethodGet, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("не JSON: %s", rec.Body.String())
	}
	if doc["overridden"] != false || doc["source"] != "config" {
		t.Fatalf("без файла источник должен быть config: %v", doc)
	}
	eff, _ := doc["effective"].(map[string]interface{})
	if eff["coexistence"] != string(types.ImageCoexistenceExclusive) {
		t.Fatalf("effective=%v", eff)
	}
	if doc["path"] != store.Path() {
		t.Errorf("путь файла не совпадает: %v != %v", doc["path"], store.Path())
	}
	// Подсказки по политикам нужны форме: UI не должен дублировать тексты.
	if pols, ok := doc["policies"].([]interface{}); !ok || len(pols) != 3 {
		t.Errorf("нет описания политик: %v", doc["policies"])
	}
}

func TestImageResources_PutAppliesImmediatelyAndPersists(t *testing.T) {
	bundled := types.ImageResourceSettings{Coexistence: types.ImageCoexistenceExclusive}
	s, store := newImageResourcesTestServer(t, bundled)

	body := `{"coexistence":"offload","vramHeadroomMb":1024,"queueWaitTimeoutSec":120,"exclusiveLockTimeoutSec":900,"blockOnUnknownVramEstimate":true,"gateDisabled":false}`
	rec := doImageResources(t, s, http.MethodPut, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Применение мгновенное: тот же указатель читает гейт.
	if got := s.config.Balancing.Image; got.Coexistence != types.ImageCoexistenceOffload || got.VramHeadroomMB != 1024 {
		t.Fatalf("конфиг не обновлён: %+v", got)
	}
	// Файл записан → переживёт рестарт.
	fresh := config.NewImageResourcesStore(store.Path())
	if err := fresh.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !fresh.Present() || fresh.Settings().Coexistence != types.ImageCoexistenceOffload {
		t.Fatalf("переопределение не сохранено: %+v", fresh.Settings())
	}
}

func TestImageResources_PutRejectsInvalidWithoutWriting(t *testing.T) {
	s, store := newImageResourcesTestServer(t, types.ImageResourceSettings{})
	rec := doImageResources(t, s, http.MethodPut, `{"coexistence":"exlusive"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("опечатка в политике должна отклоняться: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(store.Path()); err == nil {
		t.Error("невалидное значение не должно попадать в файл")
	}
	if s.config.Balancing.Image.Coexistence != "" {
		t.Errorf("невалидное значение не должно применяться: %+v", s.config.Balancing.Image)
	}
}

func TestImageResources_DeleteRestoresBundledDefaults(t *testing.T) {
	bundled := types.ImageResourceSettings{Coexistence: types.ImageCoexistenceDedicated, VramHeadroomMB: 2048}
	s, _ := newImageResourcesTestServer(t, bundled)

	if rec := doImageResources(t, s, http.MethodPut, `{"coexistence":"exclusive"}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT: %s", rec.Body.String())
	}
	if s.config.Balancing.Image.Coexistence != types.ImageCoexistenceExclusive {
		t.Fatalf("PUT не применён: %+v", s.config.Balancing.Image)
	}

	rec := doImageResources(t, s, http.MethodDelete, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Возврат именно к config.json, а не к дефолту движка.
	if got := s.config.Balancing.Image; got.Coexistence != types.ImageCoexistenceDedicated || got.VramHeadroomMB != 2048 {
		t.Fatalf("сброс вернул не встроенные значения: %+v", got)
	}
	var doc map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	if doc["overridden"] != false || doc["source"] != "config" {
		t.Fatalf("после сброса источник должен быть config: %v", doc)
	}
}

func TestImageResources_MethodNotAllowedAndNoStore(t *testing.T) {
	s, _ := newImageResourcesTestServer(t, types.ImageResourceSettings{})
	if rec := doImageResources(t, s, http.MethodPatch, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PATCH: status=%d", rec.Code)
	}
	// Балансер без хранилища обязан честно сказать, что настройки недоступны.
	bare := &Server{config: &types.LoadBalancerConfig{}}
	rec := doImageResources(t, bare, http.MethodGet, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("без хранилища ждём 503, получили %d: %s", rec.Code, rec.Body.String())
	}
}

// TestImageResources_AllowToolLoadRoundTrip — R85: галочка «разрешить инструменту
// загружать модель» ходит через тот же API и различает три состояния.
//
// ПОЧЕМУ ЭТО ВАЖНО: у настройки три состояния (не задано / true / false), и
// «не задано» отличается от «выключено» — незаданное значение означает «действует
// флаг окружения LB_IMAGE_TOOL_ALLOW_LOAD». Обычный bool их бы склеил, и снятая
// галочка выглядела бы как «настройку не сохраняли».
func TestImageResources_AllowToolLoadRoundTrip(t *testing.T) {
	s, store := newImageResourcesTestServer(t, types.ImageResourceSettings{})

	// 1) Не задано: effective = true (дефолт), overridden = false, источник — env.
	rec := doImageResources(t, s, http.MethodGet, "")
	var doc map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("не JSON: %s", rec.Body.String())
	}
	atl, _ := doc["allowToolLoad"].(map[string]interface{})
	if atl == nil {
		t.Fatalf("в ответе нет блока allowToolLoad: %s", rec.Body.String())
	}
	if atl["effective"] != true || atl["overridden"] != false {
		t.Fatalf("незаданная настройка должна показывать effective=true/overridden=false: %v", atl)
	}
	if atl["env"] != "LB_IMAGE_TOOL_ALLOW_LOAD" {
		t.Errorf("UI должен знать имя флага окружения: %v", atl["env"])
	}

	// 2) Явное false: применяется сразу и сохраняется.
	if rec := doImageResources(t, s, http.MethodPut, `{"allowToolLoad":false}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT false: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := s.config.Balancing.Image.AllowToolLoad; got == nil || *got {
		t.Fatalf("выключение не применено: %+v", s.config.Balancing.Image.AllowToolLoad)
	}
	fresh := config.NewImageResourcesStore(store.Path())
	if err := fresh.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := fresh.Settings().AllowToolLoad; got == nil || *got {
		t.Fatalf("выключение не сохранено на диск: %+v", got)
	}

	// 3) PUT БЕЗ поля не должен затирать сохранённое значение.
	if rec := doImageResources(t, s, http.MethodPut, `{"vramHeadroomMb":256}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT без allowToolLoad: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := s.config.Balancing.Image.AllowToolLoad; got == nil || *got {
		t.Fatalf("PUT без поля обязан сохранить прежнее значение: %+v", got)
	}

	// 4) Явное true возвращает автозагрузку.
	if rec := doImageResources(t, s, http.MethodPut, `{"allowToolLoad":true}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT true: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := s.config.Balancing.Image.AllowToolLoad; got == nil || !*got {
		t.Fatalf("включение не применено: %+v", got)
	}
}

// TestImageResources_ToolLoadTimeoutRoundTrip — R86-follow-up: ожидание загрузки
// модели из вызова инструмента правится из WebUI и переживает рестарт.
//
// ЗАЧЕМ ЭТО В КОНФИГЕ: живой случай на стенде — модель 4.7 ГБ не поднялась за
// дефолтные 600 с, и оператору нужен способ поднять лимит без правки compose.
func TestImageResources_ToolLoadTimeoutRoundTrip(t *testing.T) {
	s, store := newImageResourcesTestServer(t, types.ImageResourceSettings{})

	// 1) Не задано: effective = дефолт (600), overridden = false, источник — env.
	rec := doImageResources(t, s, http.MethodGet, "")
	var doc map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("не JSON: %s", rec.Body.String())
	}
	tlt, _ := doc["toolLoadTimeout"].(map[string]interface{})
	if tlt == nil {
		t.Fatalf("в ответе нет блока toolLoadTimeout: %s", rec.Body.String())
	}
	if tlt["effectiveSec"] != float64(600) || tlt["overridden"] != false {
		t.Fatalf("незаданный таймаут: %v", tlt)
	}
	if tlt["env"] != "LB_IMAGE_TOOL_LOAD_TIMEOUT_SEC" {
		t.Errorf("UI должен знать имя переменной окружения: %v", tlt["env"])
	}
	limits, _ := doc["limits"].(map[string]interface{})
	if limits["minToolLoadTimeoutSec"] != float64(1) || limits["maxToolLoadTimeoutSec"] != float64(86400) {
		t.Errorf("границы таймаута не отданы форме: %v", limits)
	}

	// 2) Значение применяется и сохраняется.
	if rec := doImageResources(t, s, http.MethodPut, `{"toolLoadTimeoutSec":1800}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT 1800: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := s.config.Balancing.Image.ToolLoadTimeoutSec; got == nil || *got != 1800 {
		t.Fatalf("значение не применено: %+v", got)
	}
	fresh := config.NewImageResourcesStore(store.Path())
	if err := fresh.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := fresh.Settings().ToolLoadTimeoutSec; got == nil || *got != 1800 {
		t.Fatalf("значение не сохранено на диск: %+v", got)
	}

	// 3) PUT БЕЗ поля не должен затирать сохранённое значение.
	if rec := doImageResources(t, s, http.MethodPut, `{"vramHeadroomMb":128}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT без таймаута: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := s.config.Balancing.Image.ToolLoadTimeoutSec; got == nil || *got != 1800 {
		t.Fatalf("PUT без поля обязан сохранить прежнее значение: %+v", got)
	}

	// 4) Ноль («не ждать вовсе») отклоняется на входе: инструмент не смог бы
	//    поднять модель никогда, и это выглядело бы как «загрузка не работает».
	if rec := doImageResources(t, s, http.MethodPut, `{"toolLoadTimeoutSec":0}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("нулевой таймаут должен отклоняться: status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 5) Выше суток — тоже отказ (иначе вызов держал бы слот неограниченно).
	if rec := doImageResources(t, s, http.MethodPut, `{"toolLoadTimeoutSec":86401}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("слишком большой таймаут должен отклоняться: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := s.config.Balancing.Image.ToolLoadTimeoutSec; got == nil || *got != 1800 {
		t.Fatalf("невалидные значения не должны менять действующее: %+v", got)
	}
}
