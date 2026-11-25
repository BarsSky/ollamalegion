// handlers_model_files.go — R751 (2026-11-25): HTTP-контракт управления
// ОТДЕЛЬНЫМИ файлами скачанной image-группы.
//
//	POST   /api/image/models/{name}/files   — положить файл из HF в группу
//	     тело: {role, repo, filename, revision?, replace?}
//	     202 + trackingId (прогресс — существующий GET /api/hf/progress,
//	     отмена — существующий POST /api/hf/cancel);
//	DELETE /api/image/models/{name}/files?filename=<путь в группе>
//	     200 + сколько байт освобождено и какие роли ушли из профиля.
//
// ПОЧЕМУ ОТДЕЛЬНЫЙ ПУТЬ, А НЕ /api/hf/cleanup И НЕ components:
//   - cleanup удаляет файл, но профиль не правит (реестр продолжал считать роль
//     найденной — ровно этот дефект и чинит R751);
//   - components добавляет ТОЛЬКО недостающий компонент: роль diffusion и
//     замена/перекачка файла им запрещены by design (защита от подмены весов).
//
// Коды отказа — те же машинные, что у components (writeComponentError), плюс
// файловые: file_already_present, file_role_conflict, invalid_filename,
// file_not_found. UI показывает текст, тесты проверяют код.
package main

import (
	"net/http"
	"strings"

	"ollama-loadbalancer/internal/sdbackend"
)

// modelFileInstallRequest — тело POST (JSON-имена — контракт UI
// webui/js/modules/image-model-files.js).
type modelFileInstallRequest struct {
	Role     string `json:"role"`
	Repo     string `json:"repo"`
	Filename string `json:"filename"`
	Revision string `json:"revision,omitempty"`
	Replace  bool   `json:"replace,omitempty"`
}

// handleModelFiles — диспетчер /api/image/models/{name}/files.
func (a *App) handleModelFiles(w http.ResponseWriter, r *http.Request, name string) {
	hf, ok := a.hfManager(w)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodPost:
		var req modelFileInstallRequest
		if err := decodeHFJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		hf.SetToken(requestHFToken(r))
		install, err := hf.StartModelFileInstall(sdbackend.ModelFileInstallRequest{
			Bundle:   name,
			Role:     req.Role,
			Repo:     req.Repo,
			Filename: req.Filename,
			Revision: req.Revision,
			Replace:  req.Replace,
		})
		if err != nil {
			writeComponentError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, install)
	case http.MethodDelete:
		// filename — в query: путь файла может содержать подкаталоги, а имя
		// группы — нет; тело DELETE поддерживаем как запасной вариант (некоторые
		// клиенты не умеют query с кириллицей в пути).
		filename := strings.TrimSpace(r.URL.Query().Get("filename"))
		if filename == "" {
			var req struct {
				Filename string `json:"filename"`
			}
			if err := decodeHFJSON(r, &req); err == nil {
				filename = strings.TrimSpace(req.Filename)
			}
		}
		if filename == "" {
			writeError(w, http.StatusBadRequest, "filename is required (query param or body)")
			return
		}
		hf.SetToken(requestHFToken(r))
		res, err := hf.DeleteModelFile(name, filename)
		if err != nil {
			writeComponentError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	default:
		writeError(w, http.StatusMethodNotAllowed, "use POST or DELETE")
	}
}
