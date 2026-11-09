// surface_alias_r479.go — R479 (2026-11-09): алиасы поверхностей `/ollama` и
// `/openai` снимаются ДО диспетчеризации по типам.
//
// ЖИВОЙ ДЕФЕКТ (аудит типов бэкендов, P0). Префикс `/ollama` нормализовал только
// LlamaCppRouter (`llamacpp_router.go`), а OllamaRouter — нет. Следствие на
// кластере без cppworker: `/ollama/api/tags` отдавал пустой список, а
// `/ollama/api/chat` → 503 «no llama.cpp backend available», хотя те же запросы
// по `/api/*` работали. OpenWebUI/клиенты с префиксом (дефолт некоторых
// инсталляций) видели «балансер сломан».
//
// ПРАВИЛО: нормализуем путь в самом начале ServeHTTP, до определения типа и
// выбора роутера. Тогда «алиас» и «прямой» путь идут одним кодом — расходиться
// им больше негде. Ответ получает `X-LB-Surface-Alias: 1`, чтобы в логе и у
// клиента было видно, что путь был переписан.
package balancer

import "strings"

// normalizeSurfaceAliasPathR479 — снять известный префикс-алиас.
//
//	/ollama/api/...  -> /api/...
//	/ollama/v1/...   -> /v1/...
//	/openai/v1/...   -> /v1/...
//
// Возвращает (нормализованный путь, был ли переписан). Неизвестные комбинации
// (например `/ollama/health`) не трогаем: пусть честно ответит 404 своей
// поверхности, а не превратится в чужой путь.
func normalizeSurfaceAliasPathR479(path string) (string, bool) {
	switch {
	case strings.HasPrefix(path, "/ollama/api/"):
		return strings.TrimPrefix(path, "/ollama"), true
	case strings.HasPrefix(path, "/ollama/v1/"):
		return strings.TrimPrefix(path, "/ollama"), true
	case strings.HasPrefix(path, "/openai/v1/"):
		return strings.TrimPrefix(path, "/openai"), true
	}
	return path, false
}
