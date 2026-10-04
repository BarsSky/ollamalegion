// image_file_route.go — R84 (2026-10-03): отдача готовых изображений через
// балансер: GET /v1/images/files/{name}.
//
// ЗАЧЕМ. Инструмент generate_image возвращает модели ссылку на картинку, а
// воркер раздаёт файлы у себя на внутреннем порту (`/images/<file>`,
// internal/sdbackend/store.go). Клиенту — особенно внешнему агенту — этот порт
// недоступен, поэтому балансер проксирует файл через клиентскую поверхность.
//
// ПОЧЕМУ БЕЗ ТОКЕНА: это та же поверхность, что /v1/images/generations и
// /sdapi/v1/*, где ключ по проектной политике не проверяется (контракт:
// internal/api/handlers_image_contract.go → auth.clientSurfaces). Секрета в
// картинке нет, а имя файла — случайные 8 hex + время (store.Save), перебором
// его не подобрать.
//
// ПУТЬ ЛЕЖИТ ПОД /v1/images/* СОЗНАТЕЛЬНО: так его не нужно добавлять в списки
// маршрутизации поверхностей (isImageEndpointPath уже знает префикс), а клиенты
// видят его рядом с генерацией.
package balancer

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// imageFilePathPrefix — префикс публичного маршрута картинок.
const imageFilePathPrefix = "/v1/images/files/"

// imageFileMaxBytes — предел отдачи файла (страховка от чужого сервиса на порту
// воркера: настоящие PNG/JPEG от sd.cpp — сотни КБ, лимит с большим запасом).
const imageFileMaxBytes = 64 << 20

// imageFileNameFromPath — имя файла из пути; ok=false, если путь не наш или имя
// небезопасно (слэши, «..», посторонние символы).
//
// Валидация ВАЖНА: проксируем GET на воркер, и пропустить «../../etc/passwd» в
// URL нельзя, даже если http.FileServer воркера от traversal защищён.
func imageFileNameFromPath(path string) (string, bool) {
	if !strings.HasPrefix(path, imageFilePathPrefix) {
		return "", false
	}
	name := strings.TrimPrefix(path, imageFilePathPrefix)
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return "", false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			return "", false
		}
	}
	return name, true
}

// serveImageFile — найти файл на живом image-бэкенде и отдать его клиенту.
//
// ПОЧЕМУ ПЕРЕБОР БЭКЕНДОВ: имя файла не говорит, какой воркер его создал, а
// каталог картинок у каждого свой. Спрашиваем по очереди здоровые image_cpp и
// отдаём первый 200. Бэкендов в кластере единицы, а файл запрашивают по ссылке
// из ответа модели, то есть сразу после генерации — попадание почти всегда
// первое.
func (ir *ImageRouter) serveImageFile(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		ir.writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed",
			"используйте GET для получения изображения")
		return
	}
	states := ir.proxy.filterBackendsByType(types.BackendTypeImage)
	if len(states) == 0 {
		ir.writeError(w, r, http.StatusServiceUnavailable, "image_backend_unavailable",
			"no healthy backend of type image_cpp is registered")
		return
	}

	w.Header().Set("Cache-Control", "public, max-age=86400")
	for _, st := range states {
		if st == nil || st.Backend == nil {
			continue
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "/images/"+name, nil)
		if err != nil {
			continue
		}
		resp, err := ir.proxy.proxyRequestToBackend(req, st.Backend.ID, imageFileTimeout)
		if err != nil {
			logger.Get().Debugw("image file: бэкенд не ответил",
				"backend", st.Backend.ID, "file", name, "error", err)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			continue
		}
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		if lm := resp.Header.Get("Last-Modified"); lm != "" {
			w.Header().Set("Last-Modified", lm)
		}
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			_ = resp.Body.Close()
			return
		}
		_, copyErr := io.Copy(w, io.LimitReader(resp.Body, imageFileMaxBytes))
		_ = resp.Body.Close()
		if copyErr != nil {
			logger.Get().Warnw("image file: клиент отвалился во время отдачи",
				"backend", st.Backend.ID, "file", name, "error", copyErr)
		}
		return
	}

	logger.Get().Infow("image file: файл не найден ни на одном бэкенде", "file", name)
	ir.writeError(w, r, http.StatusNotFound, "image_file_not_found",
		fmt.Sprintf("image %q not found on any image backend (файлы живут в каталоге воркера и могут быть очищены)", name))
}

// imageFileTimeout — таймаут отдачи файла. Внутренняя сеть, файл уже на диске:
// минуты не нужны, а зависший воркер не должен держать соединение клиента.
const imageFileTimeout = 30 * time.Second
