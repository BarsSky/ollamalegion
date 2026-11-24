//go:build llama_stub

// webui_no_page_token_r222_test.go — R222 (2026-10-22).
//
// ЖИВОЙ ДЕФЕКТ. R201/R202 дали балансеру сессии, роли и CSRF, но entrypoint
// продолжал вшивать админский API-токен в config.js — и любой, кто открыл панель,
// действовал как администратор в обход входа: роль viewer ничего не ограничивала,
// потому что браузер шёл по токену. На перенесённом образе это выглядело как
// «обвязки доступа нет вообще».
//
// Контракт R222:
//  1. отданная страница не содержит токена (генерируемый config.js — API_TOKEN: ”);
//  2. токен не попадает НИ В ОДИН отдаваемый файл. С R743 подстановка токена
//     разрешена ровно в одном месте — server-side конфиг nginx (`location
//     /api/reexport/` добавляет X-API-Token к запросам в модуль ре-экспорта);
//     этот файл лежит в /etc/nginx и браузеру не отдаётся. Поэтому проверка
//     смотрит не «есть ли подстановка», а КУДА она идёт;
//  3. аварийный вход по токену остался, но значение вводит оператор и оно живёт в
//     sessionStorage вкладки (bootstrap в config.js читает именно этот ключ).
package api

import (
	"os"
	"strings"
	"testing"
)

const (
	r222Entrypoint = "../../docker/webui/entrypoint.sh"
	r222ConfigJS   = "../../webui/js/modules/config.js"
	r222AuthJS     = "../../webui/js/modules/auth.js"
)

func r222Read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("не читается %s: %v", path, err)
	}
	return string(raw)
}

// Главная проверка: страница не должна уносить админский токен.
func TestR222_PageHasNoEmbeddedToken(t *testing.T) {
	entry := r222Read(t, r222Entrypoint)

	// Токен, подставленный из окружения в config.js, — это и была дыра.
	if strings.Contains(entry, "API_TOKEN: '${API_TOKEN}'") {
		t.Error("entrypoint снова вшивает API_TOKEN в отдаваемую страницу — вход и роли обходятся")
	}
	// Значение обязано быть пустым: даже пустое поле должно браться из bootstrap.
	if !strings.Contains(entry, "API_TOKEN: ''") {
		t.Error("в генерируемом config.js нет пустого API_TOKEN — непонятно, откуда модули берут токен")
	}
	// R743: подстановка ${API_TOKEN} разрешена ТОЛЬКО в server-side конфиг nginx
	// (им location /api/reexport/ подставляет X-API-Token — иначе кнопки модуля
	// отвечают 401). Проверяем это по каждой строке: любое другое употребление
	// токена в entrypoint снова означало бы «токен уехал в отдаваемый файл».
	var tokenLines []string
	for _, line := range strings.Split(entry, "\n") {
		if strings.Contains(line, "${API_TOKEN}") {
			tokenLines = append(tokenLines, strings.TrimSpace(line))
		}
	}
	if len(tokenLines) == 0 {
		t.Error("в entrypoint нет подстановки ${API_TOKEN}: граница не сможет подставить X-API-Token (R743)")
	}
	for _, line := range tokenLines {
		if !strings.Contains(line, "s|\\${API_TOKEN}|") {
			t.Errorf("подстановка токена вне sed для nginx-конфига: %q — так он попадёт в браузер", line)
		}
	}
	// Конфиг nginx обязан собираться ВНЕ отдаваемого корня (страница — html-каталог).
	if !strings.Contains(entry, `NGINX_CONF="/etc/nginx/conf.d/default.conf"`) {
		t.Error("nginx-конфиг должен лежать в /etc/nginx/conf.d — вне served-корня")
	}
	if strings.Contains(entry, `NGINX_CONF="/usr/share/nginx/html`) {
		t.Error("nginx-конфиг пишется в served-корень — подставленный токен уедет в браузер")
	}
	// И в самих отдаваемых файлах подстановки быть не может.
	page := r222Read(t, "../../webui/index.html")
	if strings.Contains(page, "${API_TOKEN}") {
		t.Error("index.html содержит ${API_TOKEN} — токен окажется в браузере")
	}
}

func TestR222_BreakGlassTokenComesFromOperator(t *testing.T) {
	cfg := r222Read(t, r222ConfigJS)
	entry := r222Read(t, r222Entrypoint)

	for _, path := range []struct{ name, text string }{
		{"webui/js/modules/config.js", cfg},
		{"docker/webui/entrypoint.sh", entry},
	} {
		if !strings.Contains(path.text, "ol_break_glass_token") {
			t.Errorf("%s: нет bootstrap аварийного токена из sessionStorage", path.name)
		}
		if !strings.Contains(path.text, "API_TOKEN: ''") {
			t.Errorf("%s: API_TOKEN не пустой по умолчанию", path.name)
		}
	}

	auth := r222Read(t, r222AuthJS)
	if !strings.Contains(auth, "sessionStorage") {
		t.Error("auth.js не хранит аварийный токен в sessionStorage вкладки")
	}
	if strings.Contains(auth, "localStorage.setItem") {
		t.Error("auth.js кладёт токен в localStorage — он переживёт закрытие вкладки")
	}
}

// Устойчивость: панель не должна молча работать без входа. Если роли неизвестны,
// админские элементы скрываются (это проверяет и JS-тест), а экран/полоса
// объясняют состояние.
func TestR222_AuthUIExplainsUnconfiguredState(t *testing.T) {
	auth := r222Read(t, r222AuthJS)
	if !strings.Contains(auth, "showBanner") {
		t.Error("нет видимого объяснения, когда вход не настроен (только console.warn)")
	}
	if !strings.Contains(auth, "data-admin-only") {
		t.Error("auth.js больше не управляет видимостью data-admin-only")
	}
	if !strings.Contains(auth, "MutationObserver") {
		t.Error("нет наблюдателя за динамическими админскими узлами: гость увидит кнопки, отрисованные позже")
	}
}
