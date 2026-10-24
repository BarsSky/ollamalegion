//go:build llama_stub

// panel_static_routes_r134_test.go — R134 (2026-10-16): панель балансера отдаёт
// шрифты и страницы дашборда, а не 404.
//
// ЖИВОЙ ДЕФЕКТ (жалоба оператора «в панели нет вкладки агенты и не отображаются
// все иконки»). Балансер умеет отдавать статику WebUI сам (`/app/webui`), но
// маршруты были зарегистрированы только для /js/, /css/ и /img/:
//
//	GET /webfonts/fa-solid-900.woff2  -> 404  => Font Awesome не рисует иконки
//	GET /index.html                   -> 404  => дашборда с навигацией
//	                                             (вкладка «Агенты») нет вовсе
//	GET /monitor                      -> 200  (маршрут был)
//
// Вторая половина дефекта — этих файлов не было и в образе
// (docker/balancer/Dockerfile) — закрыта там же; состав образа проверяет
// scripts/check_webui_assets.py. Здесь проверяется именно маршрутизация,
// потому что файлов в образе недостаточно: без маршрута они недостижимы.
package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestR134_PanelServesIconFontsAndDashboard — те самые пути, что отдавали 404.
func TestR134_PanelServesIconFontsAndDashboard(t *testing.T) {
	if _, ok := webuiStaticDir(); !ok {
		t.Skip("каталог webui не найден — проверка маршрутов имеет смысл только из репозитория")
	}
	s := newAgentCapacityServer(t)

	cases := []struct {
		path     string
		contains string // что обязано быть в теле (пусто = содержимое не проверяем)
		// redirect — Go-шный http.FileServer канонически перенаправляет
		// /index.html на /, поэтому 301 здесь ожидаем, а 404 — нет.
		redirect bool
	}{
		{"/webfonts/fa-solid-900.woff2", "", false},
		{"/index.html", "", true},
		{"/", `data-page="agents"`, false},
		{"/virtual-models.html", "", false},
		{"/rpc-status.html", "", false},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, c.path, nil)
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if c.redirect && rec.Code == http.StatusMovedPermanently {
			continue
		}
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200: маршрута нет — панель балансера отдаёт "+
				"иконки и страницы дашборда как 404 (живая жалоба оператора)", c.path, rec.Code)
			continue
		}
		if c.contains != "" && !strings.Contains(rec.Body.String(), c.contains) {
			t.Errorf("GET %s: в теле нет %q — отдана не та страница", c.path, c.contains)
		}
	}
}

// TestR134_PanelKeepsMonitorAndHealthRoutes — обратная сторона: у monitor.html и
// health.html свои обработчики (им нужны токен и параметры). Файловый сервер не
// должен их перехватить: иначе страница монитора отдавалась бы без проверок,
// которые в неё вложены.
func TestR134_PanelKeepsMonitorAndHealthRoutes(t *testing.T) {
	if _, ok := webuiStaticDir(); !ok {
		t.Skip("каталог webui не найден")
	}
	s := newAgentCapacityServer(t)
	for _, path := range []string{"/monitor", "/health"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200: обработчик страницы потерян", path, rec.Code)
		}
	}
}
