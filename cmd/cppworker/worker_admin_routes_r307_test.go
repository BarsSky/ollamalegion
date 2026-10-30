//go:build llama_stub

// worker_admin_routes_r307_test.go — R307 (2026-10-30): сторож «дыры в воркере нет».
//
// ЖИВАЯ ДЫРА, найденная при проверке R304: у cppworker под authMiddleware стояли
// только `/api/models/reload` и `/api/models/import`, а `/api/models/load`,
// `load-with-params`, `/api/models/unload`, `/api/models/delete` и алиасы
// (`/load`, `/api/delete`) — нет. Любой, кто видит порт 18092 в сети, мог
// загрузить модель в память (заняв VRAM), выгрузить чужую работающую модель или
// удалить файл с диска — в обход балансера и всех его проверок (роль донора,
// занятость, дедлайны).
//
// ЧТО ПРОВЕРЯЕТСЯ:
//  1. каждый обработчик, меняющий память/диск воркера, обёрнут authMiddleware в
//     router.go (статический сторож: новая ручка без обёртки ломает тест, а не
//     открывает дыру молча);
//  2. с настроенным токеном такой маршрут отвечает 401 без токена и не 401 с ним.
package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// adminHandlersR307 — обработчики, которые МЕНЯЮТ память или диск воркера.
// Список явный: он же служит контрактом для сторожевого теста.
var adminHandlersR307 = []string{
	"handleLoadModel",
	"handleLoadWithParams",
	"handleUnloadModel",
	"handleDeleteModel",
	"handleReloadModel",
	"handleImportModel",
	"handleExportModel",
}

// adminRoutesR307 — маршруты, которые обязаны быть под authMiddleware.
// Значение — имя обработчика, чтобы сторож ловил и подмену обработчика.
var adminRoutesR307 = map[string]string{
	"/api/models/load":             "handleLoadModel",
	"/api/models/load-with-params": "handleLoadWithParams",
	"/load":                        "handleLoadModel",
	"/api/models/unload":           "handleUnloadModel",
	"/api/models/delete":           "handleDeleteModel",
	"/api/delete":                  "handleDeleteModel",
	"/api/models/reload":           "handleReloadModel",
	"/api/models/import":           "handleImportModel",
	"/api/models/export":           "handleExportModel",
}

// TestR307_AdminRoutesAreWrapped — статический сторож по router.go.
func TestR307_AdminRoutesAreWrapped(t *testing.T) {
	src, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}
	text := string(src)

	for route, handler := range adminRoutesR307 {
		need := `"` + route + `", authMiddleware(` + handler + `)`
		if !strings.Contains(text, need) {
			t.Errorf("маршрут %s (%s) НЕ под authMiddleware: ручка воркера открыта", route, handler)
		}
	}

	// Обратный контроль: каждый обработчик из adminHandlersR307 обязан встречаться
	// хотя бы в одном закрытом маршруте. Иначе список выше разошёлся с кодом —
	// например, переименовали обработчик, и сторож молча перестал что-либо
	// проверять.
	for _, h := range adminHandlersR307 {
		if !strings.Contains(text, "authMiddleware("+h+")") {
			t.Errorf("обработчик %s не закрыт ни в одном маршруте — либо переименован, "+
				"либо новая ручка появилась без обёртки", h)
		}
	}
}

// TestR307_AdminRoutesRequireToken — поведение обёртки на реальном маршруте.
func TestR307_AdminRoutesRequireToken(t *testing.T) {
	os.Setenv("CPPWORKER_API_TOKEN", "r307-secret")
	os.Unsetenv("API_TOKEN")
	os.Unsetenv("BALANCER_API_TOKEN")
	t.Cleanup(func() {
		os.Unsetenv("CPPWORKER_API_TOKEN")
		os.Unsetenv("API_TOKEN")
		os.Unsetenv("BALANCER_API_TOKEN")
	})

	ok := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }

	for _, route := range []string{
		"/api/models/load", "/api/models/load-with-params", "/load",
		"/api/models/unload", "/api/models/delete", "/api/delete",
	} {
		t.Run(route+" без токена → 401", func(t *testing.T) {
			rec := httptest.NewRecorder()
			authMiddleware(ok)(rec, httptest.NewRequest("POST", route, nil))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("без токена: %d, ожидался 401 (ручка открыта)", rec.Code)
			}
		})

		t.Run(route+" с токеном → 200", func(t *testing.T) {
			req := httptest.NewRequest("POST", route, nil)
			req.Header.Set("X-API-Token", "r307-secret")
			rec := httptest.NewRecorder()
			authMiddleware(ok)(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("с токеном: %d, ожидался 200 (балансер не сможет загрузить модель)", rec.Code)
			}
		})
	}
}
