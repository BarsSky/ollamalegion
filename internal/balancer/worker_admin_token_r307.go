package balancer

// worker_admin_token_r307.go — R307 (2026-10-30): балансер предъявляет токен
// воркеру на его административных ручках.
//
// ЖИВАЯ ДЫРА, найденная при проверке R304. У cppworker под `authMiddleware`
// стояли `/api/models/reload` и `/api/models/import`, а `/api/models/load`,
// `/api/models/load-with-params`, `/api/models/unload`, `/api/models/delete` и их
// алиасы (`/load`, `/api/delete`) — НЕТ. То есть любой, кто видит порт 18092 в
// сети, мог загрузить модель в память (заняв VRAM мастера), выгрузить чужую
// работающую модель или удалить файл с диска — в обход балансера и его проверок
// (роли донора, занятости, дедлайнов).
//
// ПОЧЕМУ ОДНО ПРАВИЛО, А НЕ ПРОВЕРКА НА КАЖДОМ ВЫЗОВЕ. Эти ручки вызываются из
// нескольких мест балансера: ModelManager (load/unload/delete), прогрев через
// `/load`, прокси клиентских запросов на `/api/models/*`. Пока токен ставится
// отдельно в каждом месте, новая ручка неизбежно появится без него — ровно тот
// класс «новая точка обхода общего пути», который в проекте уже ловили сторожем
// R304. Здесь правило одно: путь из workerAdminPathsR307 → токен ЭТОГО бэкенда.
//
// ЧЕГО ЭТО НЕ ДЕЛАЕТ. Не придумывает токен: у бэкенда его может не быть (старая
// регистрация), тогда заголовок не ставится, и воркер с настроенным токеном
// ответит 401. Это честный отказ, видимый в логе и ответе, а не тихая работа без
// аутентификации.

import (
	"net/http"
	"strings"
)

// workerAdminPathsR307 — административные ручки воркера, закрытые
// authMiddleware на его стороне (cmd/cppworker/router.go).
//
// Список обязан совпадать с тем, что обёрнуто там: если воркер закрывает ручку,
// а балансер не шлёт токен — ломается загрузка; если наоборот — дыра остаётся.
// Сторожевой тест сверяет оба списка (worker_admin_token_r307_test.go).
var workerAdminPathsR307 = map[string]bool{
	"/api/models/load":                       true,
	"/api/models/load-with-params":           true,
	"/load":                                  true,
	"/api/models/unload":                     true,
	"/api/models/delete":                     true,
	"/api/delete":                            true,
	"/api/models/reload":                     true,
	"/api/models/import":                     true,
	"/api/models/export":                     true,
	"/api/v1/cppworker/reset-reload-counter": true,
}

// workerAdminPathR307 — является ли путь административной ручкой воркера.
//
// Query-строка отбрасывается: `/api/models/unload?name=…&force=true` — та же
// ручка, что и `/api/models/unload`.
func workerAdminPathR307(path string) bool {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	return workerAdminPathsR307[strings.TrimSpace(path)]
}

// workerAdminToken — токен ЭТОГО воркера для административного вызова.
//
// Пусто = у бэкенда токена нет (старая регистрация или воркер без токена):
// вызывающий код тогда не ставит заголовок, и это видно по ответу воркера.
func (p *Proxy) workerAdminToken(backendID string) string {
	if p == nil || backendID == "" {
		return ""
	}
	if b := p.GetBackend(backendID); b != nil {
		return strings.TrimSpace(b.CppWorkerApiToken)
	}
	return ""
}

// applyWorkerAdminTokenR307 — поставить заголовок токена, если путь админский.
//
// Возвращает true, если заголовок поставлен. Отдельный результат нужен логам и
// тестам: «админская ручка вызвана без токена» — это факт, который надо видеть,
// а не норма, к которой привыкли.
func applyWorkerAdminTokenR307(h http.Header, path, token string) bool {
	if h == nil || !workerAdminPathR307(path) {
		return false
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	h.Set("X-API-Token", token)
	return true
}

// mmWorkerAdminToken — токен воркера из ModelManager (короткая обёртка).
//
// ModelManager не держит ссылку на бэкенд, но знает прокси; вызов идёт через него,
// чтобы правило «чей токен» жило в одном месте (см. workerAdminToken).
func (mm *ModelManager) workerAdminToken(backendID string) string {
	if mm == nil || mm.proxy == nil {
		return ""
	}
	return mm.proxy.workerAdminToken(backendID)
}