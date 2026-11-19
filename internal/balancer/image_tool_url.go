// image_tool_url.go — R637 (2026-11-19): публичный URL и имя файла из URL.
//
// ЗАЧЕМ ВЫНЕСЕНО. `image_tool.go` держал 1671 строку: настройки и цель
// инструмента, схему для клиента, разбор вызовов, загрузку модели, генерацию и
// помощники. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"net/http"
	"strings"
)

// imageToolPublicURL — ссылка на картинку, которую отдадим модели и клиенту.
//
// Почему публичный маршрут балансера, а не адрес воркера: воркер живёт на
// внутреннем порту и картинку наружу не отдаёт; маршрут /v1/images/files/{name}
// проксирует её через балансер и, как остальные клиентские поверхности, не
// требует токена (угадать имя файла нельзя — оно случайное).
//
// ВАЖНО: воркер может вернуть как относительный путь (`/images/x.png`), так и
// АБСОЛЮТНЫЙ адрес (`http://localhost:18093/images/x.png` — когда у него задан
// SDWORKER_BASE_URL). Первая версия просто отрезала префикс «/images/», и на
// живом стенде получилась ссылка
// `http://балансер/v1/images/files/http://localhost:18093/images/x.png` —
// поэтому имя файла достаём по последнему «/images/», а не по началу строки.
func imageToolPublicURL(r *http.Request, url string) string {
	name := imageFileNameFromURL(url)
	if name == "" {
		// Разобрать не удалось — лучше отдать как есть, чем склеить мусор.
		return url
	}
	// BaseURL живёт ТОЛЬКО в окружении (это адрес конкретного стенда, а не
	// балансировочная настройка), поэтому берём env-часть настроек — из конфига
	// сюда попадать нечему.
	base := imageToolSettingsFromEnv().BaseURL
	if base == "" {
		host := ""
		if r != nil {
			host = r.Host
		}
		if host == "" {
			host = "localhost"
		}
		base = "http://" + host
	}
	return base + "/v1/images/files/" + name
}

// imageFileNameFromURL — имя файла из ссылки воркера: последний сегмент после
// «/images/», без query/fragment. Возвращает "" , если имя небезопасно (слэши,
// «..») — тогда подставлять его в публичный маршрут нельзя.
func imageFileNameFromURL(url string) string {
	raw := strings.TrimSpace(url)
	if raw == "" {
		return ""
	}
	if idx := strings.LastIndex(raw, "/images/"); idx >= 0 {
		raw = raw[idx+len("/images/"):]
	}
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	raw = strings.Trim(raw, "/")
	if raw == "" || strings.ContainsAny(raw, "/\\") || strings.Contains(raw, "..") {
		return ""
	}
	return raw
}
