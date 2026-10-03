// handlers_hf.go — R-Image / Phase 4 (2026-09-28): HTTP-слой HF-загрузки
// image-bundle'ов (/api/hf/*) для sdworker.
//
// КОНТРАКТ СНЯТ С UI (webui/js/modules/image-page.js) — имена полей буквальные,
// менять их нельзя:
//
//	GET  /api/hf/search?q=<query>          — поиск репозиториев
//	GET  /api/hf/files?modelId=&revision=  — файлы репозитория (весовые форматы)
//	POST /api/hf/download                  — одиночный файл → 202
//	POST /api/hf/bundle                    — bundle целиком → 202 (асинхронно)
//	GET  /api/hf/progress?modelId=&filename=[&bundleId=]
//	GET  /api/hf/downloads                 — активные + история + orphans
//	POST /api/hf/cancel                    — {"bundleId"} | {"modelId","filename"}
//	POST|DELETE /api/hf/cleanup            — удалить скачанный/частичный файл
//
// ПОЧЕМУ ОТДЕЛЬНЫЙ ФАЙЛ, А НЕ В handlers_model.go: это самостоятельная
// подсистема (HF), у неё свой контракт и свой UI-потребитель; держать её рядом
// с load/unload моделей — значит смешивать два разных жизненных цикла
// (модель на диске vs. модель в VRAM).
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/sdbackend"
)

// ============================================================
// Тела запросов (контракт UI)
// ============================================================

// hfFileSpec — один файл bundle в запросе UI: {role, repo, filename, revision}.
//
// ПОЧЕМУ ПОЛЕ НАЗЫВАЕТСЯ repo, А НЕ modelId: так его шлёт UI
// (image-page.js:1565 — parseBundleRows собирает {role,repo,filename,revision}).
// Внутри воркера это ModelID загрузчика.
type hfFileSpec struct {
	Role     string `json:"role"`
	Repo     string `json:"repo"`
	Filename string `json:"filename"`
	Revision string `json:"revision"`
}

// hfDownloadRequest — тело POST /api/hf/download (одиночный файл).
type hfDownloadRequest struct {
	ModelID  string `json:"modelId"`
	Filename string `json:"filename"`
	Revision string `json:"revision"`
	Role     string `json:"role"`
}

// hfBundleRequest — тело POST /api/hf/bundle.
//
// Поддерживаем ДВЕ формы files: массив объектов и одиночный объект files[0]
// (UI исторически пробовал и то, и другое — см. BUNDLE_DOWNLOAD_PATHS).
type hfBundleRequest struct {
	Name   string       `json:"name"`
	Family string       `json:"family"`
	Files  []hfFileSpec `json:"files"`
	// Одиночный файл как альтернатива array.
	File *hfFileSpec `json:"file,omitempty"`
	// Legacy-поля одиночного пути: часть клиентов шлёт bundle как один файл.
	ModelID  string `json:"modelId,omitempty"`
	Filename string `json:"filename,omitempty"`
	Revision string `json:"revision,omitempty"`
	Role     string `json:"role,omitempty"`
}

// hfCancelRequest — тело POST /api/hf/cancel.
type hfCancelRequest struct {
	BundleID string `json:"bundleId"`
	ModelID  string `json:"modelId"`
	Filename string `json:"filename"`
}

// hfCleanupRequest — тело POST/DELETE /api/hf/cleanup.
type hfCleanupRequest struct {
	ModelID  string `json:"modelId"`
	Filename string `json:"filename"`
}

// ============================================================
// Хелперы
// ============================================================

// hfManager — HF-обёртка воркера или nil (503) с понятным текстом.
func (a *App) hfManager(w http.ResponseWriter) (*sdbackend.HFManager, bool) {
	hf := a.svc.Downloader()
	if hf == nil {
		writeError(w, http.StatusServiceUnavailable,
			"HF downloader is not available (worker was started without HF support)")
		return nil, false
	}
	return hf, true
}

// requestHFToken — токен HF из запроса: X-HF-Token (так шлёт UI, см.
// image-page.js:656), затем Authorization: Bearer, затем Authorization как есть.
// Пусто = «токена в запросе нет»: остаётся токен из конфига/HF_TOKEN.
func requestHFToken(r *http.Request) string {
	if t := strings.TrimSpace(r.Header.Get("X-HF-Token")); t != "" {
		return t
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[len("bearer "):])
	}
	return auth
}

// hfContext — контекст внешнего вызова HF (таймаут обязателен: запросы к
// huggingface.co из воркера не должны висеть на неопределённый срок).
func hfContext(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// toDownloadRequests — конвертация спецификаций UI в запросы загрузчика.
func toDownloadRequests(specs []hfFileSpec) []cppbackend.HFDownloadRequest {
	out := make([]cppbackend.HFDownloadRequest, 0, len(specs))
	for _, s := range specs {
		out = append(out, cppbackend.HFDownloadRequest{
			ModelID:  strings.TrimSpace(s.Repo),
			Filename: strings.TrimSpace(s.Filename),
			Revision: strings.TrimSpace(s.Revision),
			Role:     strings.TrimSpace(s.Role),
		})
	}
	return out
}

// ============================================================
// GET /api/hf/search
// ============================================================

// handleHFSearch — поиск репозиториев на HuggingFace.
//
// Принимаем и q (контракт UI), и query (как cppworker) — расхождение имён уже
// один раз стоило 404, поэтому поддерживаем оба.
func (a *App) handleHFSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	hf, ok := a.hfManager(w)
	if !ok {
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		query = strings.TrimSpace(r.URL.Query().Get("query"))
	}
	if query == "" {
		writeError(w, http.StatusBadRequest, "q parameter is required")
		return
	}
	limit := 20
	if l := strings.TrimSpace(r.URL.Query().Get("limit")); l != "" {
		if parsed, err := parseInt(l, 20); err == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		}
	}
	hf.SetToken(requestHFToken(r))

	ctx, cancel := hfContext(r, 60*time.Second)
	defer cancel()

	// task — необязательный фильтр HF pipeline_tag. Для image-воркера полезен
	// text-to-image; пусто = фильтра нет (совместимо с cppworker-поведением,
	// где library=gguf сужает выдачу до GGUF-репозиториев).
	results, err := hf.SearchModels(ctx, query, limit, strings.TrimSpace(r.URL.Query().Get("task")))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "search failed: "+err.Error())
		return
	}
	if results == nil {
		results = []cppbackend.HFModelRepo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"results": results, "count": len(results), "query": query,
	})
}

// ============================================================
// GET /api/hf/files
// ============================================================

// handleHFFiles — список файлов весов репозитория.
//
// Форматы — ModelWeightExtensions (.gguf/.safetensors/.sft/.ckpt): именно из них
// собирается bundle диффузионной модели (diffusion+VAE+text encoders).
func (a *App) handleHFFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	hf, ok := a.hfManager(w)
	if !ok {
		return
	}
	modelID := strings.TrimSpace(r.URL.Query().Get("modelId"))
	if modelID == "" {
		writeError(w, http.StatusBadRequest, "modelId parameter is required")
		return
	}
	revision := strings.TrimSpace(r.URL.Query().Get("revision"))
	if revision == "" {
		revision = "main"
	}
	hf.SetToken(requestHFToken(r))

	ctx, cancel := hfContext(r, 30*time.Second)
	defer cancel()

	files, err := hf.ListFiles(ctx, modelID, revision)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list files failed: "+err.Error())
		return
	}
	if files == nil {
		files = []cppbackend.HFFileInfo{}
	}
	// R-Image Phase 9: ПРЕДЛОЖЕННАЯ роль файла в bundle. UI рисует её рядом с
	// файлом и подставляет в строку bundle, чтобы оператор не выбирал роль
	// вслепую (diffusion vs vae vs text-encoder). Эвристика — та же функция
	// (sdbackend.SuggestRole → roleFromFilename), что применяется при чтении
	// готового bundle: правила не должны разъезжаться между сервером и UI.
	for i := range files {
		if files[i].SuggestedRole == "" {
			files[i].SuggestedRole = sdbackend.SuggestRole(files[i].Path)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"files": files, "count": len(files), "modelId": modelID, "revision": revision,
	})
}

// ============================================================
// POST /api/hf/download
// ============================================================

// handleHFDownload — одиночная загрузка файла → 202.
//
// Загрузка идёт в фоне средствами cppbackend (StartDownload не блокируется);
// отслеживание — GET /api/hf/progress?modelId=&filename=.
func (a *App) handleHFDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	hf, ok := a.hfManager(w)
	if !ok {
		return
	}
	var req hfDownloadRequest
	if err := decodeHFJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.ModelID) == "" {
		writeError(w, http.StatusBadRequest, "modelId is required")
		return
	}
	hf.SetToken(requestHFToken(r))

	progress, err := hf.StartFileDownload(cppbackend.HFDownloadRequest{
		ModelID:  strings.TrimSpace(req.ModelID),
		Filename: strings.TrimSpace(req.Filename),
		Revision: strings.TrimSpace(req.Revision),
		Role:     strings.TrimSpace(req.Role),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "download failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status": "started", "progress": progress,
	})
}

// ============================================================
// POST /api/hf/bundle (+ алиас /api/image/models/download)
// ============================================================

// handleHFBundle — bundle-загрузка image-модели → 202 немедленно.
//
// НЕБЛОКИРУЮЩИЙ ОТВЕТ: сама загрузка идёт в горутине (HFManager.StartBundle),
// а темп/статус видны в /api/hf/progress. Контрольный таймаут UI — 15 с
// (TIMEOUT_CONTROL_MS), а pull модели — десятки минут, поэтому синхронный
// ответ здесь невозможен в принципе.
func (a *App) handleHFBundle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	hf, ok := a.hfManager(w)
	if !ok {
		return
	}
	var req hfBundleRequest
	if err := decodeHFJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	files := req.Files
	if len(files) == 0 && req.File != nil {
		files = []hfFileSpec{*req.File}
	}
	// Legacy-форма: bundle из одиночных полей верхнего уровня.
	if len(files) == 0 && strings.TrimSpace(req.ModelID) != "" {
		files = []hfFileSpec{{
			Role: req.Role, Repo: req.ModelID, Filename: req.Filename, Revision: req.Revision,
		}}
	}
	if len(files) == 0 {
		writeError(w, http.StatusBadRequest, "files is required: [{role, repo, filename, revision}]")
		return
	}
	family := strings.TrimSpace(req.Family)
	if family == "" {
		family = "other"
	}
	hf.SetToken(requestHFToken(r))

	name := strings.TrimSpace(req.Name)
	if err := hf.StartBundle(name, family, toDownloadRequests(files)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":   "started",
		"bundleId": name,
		"name":     name,
		"family":   family,
		"files":    len(files),
		// Подсказка клиенту, где смотреть прогресс (UI опрашивает по файлам).
		// Экранируем: имя bundle — пользовательский ввод.
		"progressUrl": "/api/hf/progress?bundleId=" + url.QueryEscape(name),
	})
}

// ============================================================
// GET /api/hf/progress
// ============================================================

// handleHFProgress — прогресс одного файла или агрегат bundle.
//
// Формы:
//
//	?modelId=<repo>&filename=<file>  → HFDownloadProgress (контракт UI:
//	                                   status/totalBytes/downloaded/progressPct/speedBps)
//	?bundleId=<name>                 → HFBundleProgress (агрегат)
func (a *App) handleHFProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	hf, ok := a.hfManager(w)
	if !ok {
		return
	}
	modelID := strings.TrimSpace(r.URL.Query().Get("modelId"))
	filename := strings.TrimSpace(r.URL.Query().Get("filename"))
	bundleID := strings.TrimSpace(r.URL.Query().Get("bundleId"))

	// bundleId приоритетнее: он даёт агрегат, который не выводится из пары
	// (modelId, filename).
	if bundleID != "" {
		progress, err := hf.BundleProgress(bundleID)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, progress)
		return
	}
	if modelID == "" {
		writeError(w, http.StatusBadRequest, "modelId or bundleId is required")
		return
	}

	progress, found, err := hf.FileProgress(modelID, filename)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !found {
		// 404 = «записи ещё нет» (UI это ожидает и игнорирует, см.
		// image-page.js:1621). Отдавать нулевой прогресс нельзя: UI принял бы
		// его за «скачано 0 байт» и сбросил счётчик idle-тиков.
		writeError(w, http.StatusNotFound,
			"no download record for "+modelID+"/"+filename+" yet")
		return
	}
	writeJSON(w, http.StatusOK, progress)
}

// ============================================================
// GET /api/hf/downloads
// ============================================================

// handleHFDownloads — активные + история + orphans (+ bundle-загрузки).
func (a *App) handleHFDownloads(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	hf, ok := a.hfManager(w)
	if !ok {
		return
	}
	snap := hf.ListDownloads()
	// Пустые срезы, а не null: UI (и JSON-клиенты) ожидают массивы.
	if snap.Active == nil {
		snap.Active = []cppbackend.HFDownloadProgress{}
	}
	if snap.History == nil {
		snap.History = []cppbackend.HFDownloadProgress{}
	}
	if snap.Orphans == nil {
		snap.Orphans = []cppbackend.OrphanDownloadFile{}
	}
	if snap.Bundles == nil {
		snap.Bundles = []cppbackend.HFBundleProgress{}
	}
	if snap.BundleHistory == nil {
		snap.BundleHistory = []cppbackend.HFBundleProgress{}
	}
	writeJSON(w, http.StatusOK, snap)
}

// ============================================================
// POST /api/hf/cancel
// ============================================================

// handleHFCancel — отмена bundle (bundleId) или одиночного файла.
func (a *App) handleHFCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	hf, ok := a.hfManager(w)
	if !ok {
		return
	}
	var req hfCancelRequest
	if err := decodeHFJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.BundleID) == "" && strings.TrimSpace(req.ModelID) == "" {
		writeError(w, http.StatusBadRequest, "bundleId or modelId is required")
		return
	}
	if err := hf.Cancel(req.BundleID, req.ModelID, req.Filename); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// ============================================================
// POST|DELETE /api/hf/cleanup
// ============================================================

// handleHFCleanup — удаляет скачанный/частичный файл, освобождая диск.
//
// Поддерживает query-параметры (для DELETE) и тело (для POST) — как в
// cppworker: скрипты и UI используют разные способы.
//
// filename вида "<bundle>/<file>" адресует файл внутри каталога bundle
// (именно так их отдаёт /api/hf/downloads в orphans, см.
// cppbackend.ListOrphanBundleFiles).
func (a *App) handleHFCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete && r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use DELETE or POST")
		return
	}
	hf, ok := a.hfManager(w)
	if !ok {
		return
	}
	var req hfCleanupRequest
	query := r.URL.Query()
	req.ModelID = strings.TrimSpace(query.Get("modelId"))
	req.Filename = strings.TrimSpace(query.Get("filename"))
	if r.Method == http.MethodPost || (req.ModelID == "" && req.Filename == "") {
		var body hfCleanupRequest
		if err := decodeHFJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if req.Filename == "" {
			req.Filename = strings.TrimSpace(body.Filename)
		}
		if req.ModelID == "" {
			req.ModelID = strings.TrimSpace(body.ModelID)
		}
	}
	if req.Filename == "" {
		writeError(w, http.StatusBadRequest, "filename is required (query param or body)")
		return
	}

	result, err := hf.Delete(req.ModelID, req.Filename)
	if err != nil {
		// 200 с noop: для UI «файла нет» — не ошибка, а отсутствие работы.
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "noop", "message": err.Error(), "result": result,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "result": result})
}

// ============================================================
// Вспомогательное
// ============================================================

// maxHFJSONBody — потолок тела HF-запросов.
//
// 4 МБ с запасом: bundle-описание — это десятки строк JSON; большие тела здесь
// означают ошибку клиента, и читать их в память незачем (в отличие от
// /api/image/generate, где законно приходит base64-картинка).
const maxHFJSONBody = 4 * 1024 * 1024

// decodeHFJSON — декодирование JSON-тела с ограничением размера и снятием
// UTF-8 BOM.
//
// ПОЧЕМУ BOM: тот же класс бага, что чинили в cppworker (R60.16): PowerShell
// Out-File и Notepad пишут BOM, а json.Unmarshal падает на «invalid character
// 'ï'» и оператор видит 400 на валидном по смыслу теле.
func decodeHFJSON(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxHFJSONBody))
	if err != nil {
		return err
	}
	if len(body) >= 3 && body[0] == 0xEF && body[1] == 0xBB && body[2] == 0xBF {
		body = body[3:]
	}
	if len(strings.TrimSpace(string(body))) == 0 && !allowsEmptyHFBody(v) {
		return errEmptyBody
	}
	return json.Unmarshal(body, v)
}

// errEmptyBody — пустое тело там, где оно обязательно.
var errEmptyBody = &hfEmptyBodyError{}

type hfEmptyBodyError struct{}

func (e *hfEmptyBodyError) Error() string { return "empty request body" }

// allowsEmptyHFBody — для cleanup допустимо пустое тело (параметры могут быть
// в query string); для остальных эндпоинтов пустое тело = ошибка контракта.
func allowsEmptyHFBody(v any) bool {
	_, ok := v.(*hfCleanupRequest)
	return ok
}
