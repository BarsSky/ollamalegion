// handlers_reference_apply.go — предпросмотр и применение шаблона.
//
// ГЛАВНОЕ ПРАВИЛО (§3.4, §7 п.2 документа): применение шаблона — это
// СОХРАНЕНИЕ ПРОФИЛЯ ПЛЮС существующий reload, а не новая операция. Поэтому
// здесь нет ни своего сохранения профиля, ни своего reload'а: код синтезирует
// запрос к СУЩЕСТВУЮЩИМ обработчикам профилей и переупаковывает их ответ:
//
//	mode:"profile-only"  → s.upsertModelProfile  (мерж + валидация + память +
//	                        персист; handlers_cppworker_profiles.go:251-433)
//	mode:"profile+reload"→ s.applyModelProfile   (…… + reload с force:true,
//	                        gpuLayers, токеном, ?backend= и busy→202;
//	                        :536-786, async — handlers_cppworker_apply_async.go)
//
// Так сохраняются все закрытые ранее дефекты: numGpuLayers вместо gpuLayers
// (:807-816), отсутствие force (:817-826), потеря токена cppworker (:912-938),
// скоуп «на все бэкенды» (:527-534). Второй путь = повторение этих шести.
//
// Предпросмотр (`…/templates/{id}/preview`) НИЧЕГО не сохраняет и reload не
// запускает: он только мержит набор с текущим профилем (§3.3).
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/internal/reference"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// Режимы применения шаблона (§3.4).
const (
	// referenceModeProfileOnly — только сохранить профиль модели.
	referenceModeProfileOnly = "profile-only"
	// referenceModeProfileReload — сохранить профиль и перезагрузить модель на
	// ЯВНО указанном бэкенде (решение человека №3: пустой backendId запрещён).
	referenceModeProfileReload = "profile+reload"
)

// referenceApplyRequest — тело preview/apply.
//
// Понимает поля обоих эндпоинтов: preview использует model/backendId, apply —
// все остальные. DisallowUnknownFields (как у PUT профиля) не даёт клиенту
// думать, что неизвестное поле сработало.
type referenceApplyRequest struct {
	Model     string `json:"model"`
	BackendID string `json:"backendId"`
	// Mode — обязателен для apply: пустое значение не угадывается (молчаливое
	// «сохранить без reload» выдавалось бы оператору за «применено»).
	Mode string `json:"mode"`
	// DryRun — вернуть отчёт предпросмотра, ничего не сохраняя.
	DryRun bool `json:"dryRun"`
	// ConfirmNonFeasible — подтверждение применения, когда вердикт памяти
	// cppworker говорит feasible=false.
	ConfirmNonFeasible bool `json:"confirmNonFeasible"`
	// Revision — необязательная оптимистичная блокировка шаблона.
	Revision int `json:"revision"`
}

// referenceViolation — одно нарушение validateModelProfile в форме списка.
type referenceViolation struct {
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// referenceValid — блок `valid` предпросмотра (§3.3).
type referenceValid struct {
	OK         bool                 `json:"ok"`
	Violations []referenceViolation `json:"violations"`
}

// referenceDiffEntry — «сейчас → станет» по одному параметру.
//
// Строится ТОЛЬКО по Name/Value: unit — подпись для человека и в сравнении не
// участвует (решение человека №5).
type referenceDiffEntry struct {
	Field   string      `json:"field"`
	Current interface{} `json:"current"`
	Next    interface{} `json:"next"`
}

// referenceTarget — где шаблон можно применить (проекция cluster/models/loaded,
// §3.5: новый эндпоинт «загруженные модели» не заводим).
type referenceTarget struct {
	BackendID     string `json:"backendId"`
	BackendType   string `json:"backendType,omitempty"`
	Status        string `json:"status,omitempty"`
	Loaded        bool   `json:"loaded"`
	ContextLength int    `json:"contextLength,omitempty"`
	// ActiveQueries — только для загруженной модели и только когда cppworker
	// ответил; nil = «неизвестно» (не подменяем 0, иначе «свободен» врёт).
	ActiveQueries *int64 `json:"activeQueries,omitempty"`
}

// referenceMemoryBlock — вердикт памяти из cppworker (§3.3, profile_memory_check_r116.go).
type referenceMemoryBlock struct {
	// Checked — удалось ли спросить cppworker (false = проверка недоступна).
	Checked  bool                   `json:"checked"`
	Feasible bool                   `json:"feasible"`
	Degraded bool                   `json:"degraded"`
	Stage    string                 `json:"stage,omitempty"`
	Message  string                 `json:"message,omitempty"`
	Detail   map[string]interface{} `json:"detail,omitempty"`
}

// referencePreviewResponse — ответ POST …/templates/{id}/preview (§3.3).
type referencePreviewResponse struct {
	OK              bool                       `json:"ok"`
	Model           string                     `json:"model"`
	TemplateID      string                     `json:"templateId"`
	TemplateVersion int                        `json:"templateVersion"`
	Profile         types.LlamaCppModelProfile `json:"profile"`
	Valid           referenceValid             `json:"valid"`
	Diff            []referenceDiffEntry       `json:"diff"`
	Targets         []referenceTarget          `json:"targets"`
	Memory          referenceMemoryBlock       `json:"memory"`
	Warnings        []string                   `json:"warnings"`
}

// referenceApplyResponse — синхронный ответ POST …/templates/{id}/apply (§3.4).
type referenceApplyResponse struct {
	Status          string                           `json:"status"`
	Model           string                           `json:"model"`
	TemplateID      string                           `json:"templateId"`
	TemplateVersion int                              `json:"templateVersion"`
	Mode            string                           `json:"mode"`
	Profile         types.LlamaCppModelProfile       `json:"profile"`
	Backends        []modelProfileApplyBackendResult `json:"backends"`
	Warning         string                           `json:"warning,omitempty"`
}

// --- предпросмотр ---------------------------------------------------------

// previewReferenceTemplate — POST …/templates/{id}/preview (§3.3).
//
// НИЧЕГО не пишет на диск и не запускает reload: ни Store, ни профили не
// трогаются, сохраняется только порядок вычислений.
func (s *Server) previewReferenceTemplate(w http.ResponseWriter, r *http.Request, store *reference.Store, id string) {
	tmpl, ok := store.Template(id)
	if !ok {
		s.writeJSON(w, http.StatusNotFound, referenceErrorBody("template_not_found",
			fmt.Sprintf("шаблон %q не найден", id), map[string]interface{}{"id": id}))
		return
	}
	var req referenceApplyRequest
	if !decodeReferenceBody(w, s, r, &req) {
		return
	}
	model := referenceNormalizeModelName(req.Model)
	if model == "" {
		s.writeJSON(w, http.StatusBadRequest, referenceErrorBody("model_required",
			"укажите модель: предпросмотр считается для конкретного профиля", nil))
		return
	}
	obj, objErr := reference.DecodeParamsObject(tmpl.Params)
	if objErr != nil {
		s.writeJSON(w, http.StatusBadRequest, referenceErrorBody("invalid_params",
			"params шаблона повреждены: "+objErr.Error(), map[string]interface{}{"id": tmpl.ID}))
		return
	}
	s.writeJSON(w, http.StatusOK, s.buildReferencePreview(model, tmpl, obj, strings.TrimSpace(req.BackendID)))
}

// buildReferencePreview — отчёт «что будет, если применить шаблон».
//
// Переиспользует: мерж — mergeProfileUpdate (как PUT профиля), границы —
// validateModelProfile, память — checkProfileMemory (cppworker
// GET /api/models/effective). Своего расчёта здесь нет ни одного.
func (s *Server) buildReferencePreview(model string, tmpl reference.Template, obj map[string]json.RawMessage, backendID string) referencePreviewResponse {
	existing, hasExisting := s.referenceExistingProfile(model)
	merged, _ := s.referenceMergeParams(existing, obj)

	raw, _ := json.Marshal(obj)

	resp := referencePreviewResponse{
		Model:           model,
		TemplateID:      tmpl.ID,
		TemplateVersion: tmpl.TemplateVersionOrDefault(),
		Profile:         merged,
		Valid:           referenceValid{OK: true, Violations: []referenceViolation{}},
		Diff:            referenceDiff(existing, merged, obj),
		Targets:         s.referenceTargets(model, backendID),
		Warnings:        make([]string, 0),
	}
	if _, _, decErr := decodeProfileUpdate(raw); decErr != nil {
		// Файл мог быть отредактирован руками — это не 4xx, а честный ok:false.
		resp.Valid.OK = false
		resp.Valid.Violations = append(resp.Valid.Violations, referenceViolation{
			Field:   profileFieldFromJSONError(decErr.Error()),
			Message: decErr.Error(),
		})
	}
	if valErr := validateModelProfile(merged); valErr != nil {
		resp.Valid.OK = false
		resp.Valid.Violations = append(resp.Valid.Violations, referenceViolation{
			Field:   profileFieldFromValidatorError(valErr.Error()),
			Message: valErr.Error(),
		})
	}
	if !hasExisting {
		resp.Warnings = append(resp.Warnings,
			"профиля этой модели ещё нет: набор создаст новый профиль с нуля")
	}
	resp.Memory = s.referenceMemoryBlockFor(model, merged)
	if resp.Memory.Checked && resp.Memory.Degraded {
		resp.Warnings = append(resp.Warnings, resp.Memory.Message)
	}
	resp.OK = resp.Valid.OK && resp.Memory.Feasible
	return resp
}

// referenceMemoryBlockFor — вердикт памяти (fail-open, как в PUT профиля).
func (s *Server) referenceMemoryBlockFor(model string, profile types.LlamaCppModelProfile) referenceMemoryBlock {
	mv, asked := s.checkProfileMemory(model, profile.ContextLength, profile.KVCacheType)
	block := referenceMemoryBlock{
		Checked:  asked,
		Feasible: mv.Feasible,
		Degraded: mv.Degraded,
		Message:  mv.Message,
		Detail:   mv.Memory,
	}
	if stage, ok := mv.Memory["stage"].(string); ok {
		block.Stage = stage
	}
	if !asked {
		block.Message = "проверка памяти недоступна: cppworker не ответил (сохранение не блокируется)"
	}
	return block
}

// referenceExistingProfile — текущий профиль модели (nil-proxy безопасно).
func (s *Server) referenceExistingProfile(model string) (types.LlamaCppModelProfile, bool) {
	if s == nil {
		return types.LlamaCppModelProfile{}, false
	}
	if s.proxy != nil {
		return s.proxy.GetModelProfile(model)
	}
	if snap := s.modelProfilesSnapshotR423(); snap != nil {
		profile, ok := snap[model]
		return profile, ok
	}
	return types.LlamaCppModelProfile{}, false
}

// referenceMergeParams — мерж набора с профилем ТЕМ ЖЕ mergeProfileUpdate,
// что использует PUT профиля (PATCH-like, presence для явных 0/false).
func (s *Server) referenceMergeParams(existing types.LlamaCppModelProfile, obj map[string]json.RawMessage) (types.LlamaCppModelProfile, profilePresence) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return existing, profilePresence{}
	}
	values, presence, decErr := decodeProfileUpdate(raw)
	if decErr != nil {
		// Типы уже проверены на PUT шаблона; сюда попадаем только на
		// отредактированном руками файле — предпросмотр покажет нарушение.
		return existing, profilePresence{}
	}
	return mergeProfileUpdate(existing, values, presence), presence
}

// referenceDiff — построчный diff по параметрам шаблона (unit не участвует).
func referenceDiff(existing, merged types.LlamaCppModelProfile, obj map[string]json.RawMessage) []referenceDiffEntry {
	before := profileFieldMap(existing)
	after := profileFieldMap(merged)
	out := make([]referenceDiffEntry, 0, len(obj))
	for _, name := range reference.ObjectNames(obj) {
		current, hasCurrent := before[name]
		if !hasCurrent {
			current = nil
		}
		next, hasNext := after[name]
		if !hasNext {
			next = nil
		}
		out = append(out, referenceDiffEntry{Field: name, Current: current, Next: next})
	}
	return out
}

// referenceTargets — на каких llama.cpp-бэкендах шаблон вообще применим.
//
// Проекция того же снимка состава и тех же проверок, что в apply
// (handlers_cppworker_profiles.go:646-678): новый эндпоинт «загруженные модели»
// не заводим (§3.5, §7 п.9).
func (s *Server) referenceTargets(model, backendID string) []referenceTarget {
	out := make([]referenceTarget, 0)
	if s == nil || s.proxy == nil {
		return out
	}
	for _, backend := range s.proxy.GetAllBackends() {
		if backend.Type != types.BackendTypeLlamaCpp {
			continue
		}
		if backendID != "" && backend.ID != backendID {
			continue
		}
		loaded := s.proxy.IsLlamaCppModelLoaded(backend.ID, model)
		target := referenceTarget{
			BackendID:   backend.ID,
			BackendType: string(backend.Type),
			Status:      string(backend.Status),
			Loaded:      loaded,
		}
		if loaded {
			target.ContextLength = s.proxy.LoadedContextForModel(model)
			if count, err := s.GetActiveQueriesForModel(backend.ID, model); err == nil {
				value := count
				target.ActiveQueries = &value
			}
		}
		out = append(out, target)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BackendID < out[j].BackendID })
	return out
}

// --- применение -----------------------------------------------------------

// applyReferenceTemplate — POST …/templates/{id}/apply (§3.4).
func (s *Server) applyReferenceTemplate(w http.ResponseWriter, r *http.Request, store *reference.Store, id string) {
	tmpl, ok := store.Template(id)
	if !ok {
		s.writeJSON(w, http.StatusNotFound, referenceErrorBody("template_not_found",
			fmt.Sprintf("шаблон %q не найден", id), map[string]interface{}{"id": id}))
		return
	}
	var req referenceApplyRequest
	if !decodeReferenceBody(w, s, r, &req) {
		return
	}
	mode := strings.TrimSpace(req.Mode)
	if mode != referenceModeProfileOnly && mode != referenceModeProfileReload {
		s.writeJSON(w, http.StatusBadRequest, referenceErrorBody("invalid_mode",
			fmt.Sprintf("mode обязателен: %q или %q", referenceModeProfileOnly, referenceModeProfileReload),
			map[string]interface{}{
				"got":     req.Mode,
				"allowed": []string{referenceModeProfileOnly, referenceModeProfileReload},
			}))
		return
	}
	model := referenceNormalizeModelName(req.Model)
	if model == "" {
		s.writeJSON(w, http.StatusBadRequest, referenceErrorBody("model_required",
			"укажите модель, к которой применяется шаблон", nil))
		return
	}
	if req.Revision != 0 && req.Revision != tmpl.Revision {
		s.writeJSON(w, http.StatusConflict, referenceErrorBody("revision_conflict",
			"шаблон изменился между предпросмотром и применением: перечитайте его",
			map[string]interface{}{"current": tmpl.Revision, "sent": req.Revision, "id": tmpl.ID}))
		return
	}
	obj, objErr := reference.DecodeParamsObject(tmpl.Params)
	if objErr != nil {
		s.writeJSON(w, http.StatusBadRequest, referenceErrorBody("invalid_params",
			"params шаблона повреждены: "+objErr.Error(), map[string]interface{}{"id": tmpl.ID}))
		return
	}
	backendID := strings.TrimSpace(req.BackendID)

	// Решение человека №3: для profile+reload бэкенд ОБЯЗАТЕЛЕН.
	if mode == referenceModeProfileReload && backendID == "" {
		s.writeJSON(w, http.StatusBadRequest, referenceErrorBody("backend_required",
			"выберите бэкенд, где загружена модель: применение на все сразу задевает чужие/недоступные узлы",
			map[string]interface{}{"model": model}))
		return
	}
	if mode == referenceModeProfileReload {
		backend, found := s.referenceBackendByID(backendID)
		if !found {
			s.writeJSON(w, http.StatusNotFound, referenceErrorBody("backend_not_found",
				fmt.Sprintf("бэкенд %q не найден среди зарегистрированных", backendID),
				map[string]interface{}{"backendId": backendID}))
			return
		}
		if backend.Type != types.BackendTypeLlamaCpp {
			s.writeJSON(w, http.StatusBadRequest, referenceErrorBody("backend_not_llama_cpp",
				fmt.Sprintf("бэкенд %q не llama.cpp: профили загрузки к нему не применяются", backendID),
				map[string]interface{}{"backendId": backendID, "backendType": string(backend.Type)}))
			return
		}
	}

	if req.DryRun {
		// dryRun = предпросмотр: ничего не сохраняем и не перезагружаем.
		s.writeJSON(w, http.StatusOK, s.buildReferencePreview(model, tmpl, obj, backendID))
		return
	}

	// Границы и типы — ДО делегирования: существующий путь ответил бы
	// «invalid profile», а контракт шаблонов обещает invalid_params с полем.
	merged, bad := s.prepareReferenceParams(obj, "invalid_params")
	if bad != nil {
		s.writeJSON(w, http.StatusBadRequest, bad)
		return
	}

	if mode == referenceModeProfileReload {
		// Проверка памяти для reload-пути. У profile-only она уже есть ВНУТРИ
		// PUT профиля (handlers_cppworker_profiles.go:365-382) — второго гейта
		// не заводим, и confirmNonFeasible его не обходит (документировано).
		if block := s.referenceMemoryBlockFor(model, merged); block.Checked && !block.Feasible {
			if !req.ConfirmNonFeasible {
				s.writeJSON(w, http.StatusUnprocessableEntity, referenceErrorBody(
					"profile_does_not_fit_memory", block.Message,
					map[string]interface{}{"model": model, "memory": block.Detail}))
				return
			}
			logger.Get().Warnw("reference apply: применение с подтверждённым невыполнимым профилем",
				"template", tmpl.ID, "model", model, "message", block.Message)
		}
	}

	raw, err := json.Marshal(obj)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, referenceErrorBody("encode_failed", err.Error(), nil))
		return
	}
	if mode == referenceModeProfileOnly {
		captured := s.referenceRunProfileUpsert(r, model, raw)
		s.writeReferenceUpsertResult(w, model, tmpl, captured)
		return
	}
	captured := s.referenceRunProfileApply(r, model, backendID, raw)
	s.writeReferenceApplyResult(w, model, tmpl, captured)
}

// referenceBackendByID — бэкенд по id из живого снимка состава.
func (s *Server) referenceBackendByID(id string) (types.Backend, bool) {
	if s == nil || s.proxy == nil {
		return types.Backend{}, false
	}
	for _, backend := range s.proxy.GetAllBackends() {
		if backend.ID == id {
			return backend, true
		}
	}
	return types.Backend{}, false
}

// referenceNormalizeModelName — та же нормализация, что на границе API профилей
// (handlers_cppworker_profiles.go:165-178): снимаем `.gguf`, затем
// balancer.NormalizeModelProfileKey.
//
// Без неё появился бы ВТОРОЙ ключ профиля (§5.1 п.3): профиль сохранился бы под
// именем файла, а загрузка искала бы его по имени модели — «настройки не
// применяются» (живые дефекты R136/R138).
func referenceNormalizeModelName(name string) string {
	trimmed := strings.TrimSpace(name)
	trimmed = strings.TrimSuffix(trimmed, ".gguf")
	trimmed = strings.TrimSuffix(trimmed, ".GGUF")
	return balancer.NormalizeModelProfileKey(strings.TrimSpace(trimmed))
}

// --- переиспользование существующих обработчиков ---------------------------

// referenceCaptureWriter — минимальный http.ResponseWriter, которым мы
// вызываем СУЩЕСТВУЮЩИЕ обработчики профилей и разбираем их ответ.
//
// SetWriteDeadline реализован намеренно: applyModelProfile снимает серверный
// WriteTimeout через http.NewResponseController (write_deadline.go:35-41), и с
// обычной обёрткой он логировал бы «SetWriteDeadline failed» на каждый вызов.
type referenceCaptureWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

// newReferenceCaptureWriter — пустой рекордер.
func newReferenceCaptureWriter() *referenceCaptureWriter {
	return &referenceCaptureWriter{header: make(http.Header)}
}

// Header — заголовки ответа.
func (c *referenceCaptureWriter) Header() http.Header { return c.header }

// Write — тело ответа.
func (c *referenceCaptureWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.Write(p)
}

// WriteHeader — код ответа (первый выигрывает, как у net/http).
func (c *referenceCaptureWriter) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

// SetWriteDeadline — «deadline снят»: длинная операция не будет обрезана.
func (c *referenceCaptureWriter) SetWriteDeadline(time.Time) error { return nil }

// referenceSyntheticRequest — копия исходного запроса под вызов обработчика.
//
// Меняются только метод/путь/query/тело; заголовки (включая токен и context)
// сохраняются — иначе вложенный путь потерял бы аутентификацию/таймауты.
func (s *Server) referenceSyntheticRequest(r *http.Request, method, path, rawQuery string, body []byte) *http.Request {
	req := r.Clone(r.Context())
	req.Method = method
	req.URL.Path = path
	req.URL.RawPath = ""
	req.URL.RawQuery = rawQuery
	req.RequestURI = ""
	req.Body = http.NoBody
	if body != nil {
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	req.ContentLength = int64(len(body))
	req.Header = r.Header.Clone()
	req.Header.Set("Content-Type", "application/json")
	return req
}

// referenceRunProfileUpsert — переиспользование PUT-пути профиля (без reload).
func (s *Server) referenceRunProfileUpsert(r *http.Request, model string, params []byte) *referenceCaptureWriter {
	captured := newReferenceCaptureWriter()
	path := "/api/v1/cppworker/model-profiles/" + url.PathEscape(model)
	req := s.referenceSyntheticRequest(r, http.MethodPut, path, "", params)
	s.upsertModelProfile(captured, req, model)
	return captured
}

// referenceRunProfileApply — переиспользование apply-пути профиля (save+reload).
//
// `?backend=<id>` — тот же фильтр скоупа, что у WebUI (matchesBackendFilter,
// handlers_cppworker_profiles.go:531-534): применение ограничено выбранным узлом.
func (s *Server) referenceRunProfileApply(r *http.Request, model, backendID string, params []byte) *referenceCaptureWriter {
	captured := newReferenceCaptureWriter()
	path := "/api/v1/cppworker/model-profiles/" + url.PathEscape(model) + "/apply"
	query := url.Values{}
	query.Set("backend", backendID)
	req := s.referenceSyntheticRequest(r, http.MethodPost, path, query.Encode(), params)
	s.applyModelProfile(captured, req, model)
	return captured
}

// writeReferenceUpsertResult — ответ PUT-пути в контракт шаблонов.
func (s *Server) writeReferenceUpsertResult(w http.ResponseWriter, model string, tmpl reference.Template, captured *referenceCaptureWriter) {
	switch captured.status {
	case http.StatusOK:
		var profileResponse struct {
			Status              string                     `json:"status"`
			Profile             types.LlamaCppModelProfile `json:"profile"`
			Warning             string                     `json:"warning"`
			ContextLengthSource string                     `json:"contextLengthSource"`
		}
		_ = json.Unmarshal(captured.body.Bytes(), &profileResponse)
		resp := referenceApplyResponse{
			Status:          "applied",
			Model:           model,
			TemplateID:      tmpl.ID,
			TemplateVersion: tmpl.TemplateVersionOrDefault(),
			Mode:            referenceModeProfileOnly,
			Profile:         profileResponse.Profile,
			// backends пуст намеренно: profile-only не перезагружает модель.
			Backends: []modelProfileApplyBackendResult{},
			Warning:  profileResponse.Warning,
		}
		if profileResponse.ContextLengthSource != "" {
			resp.Warning = joinWarning(resp.Warning,
				"contextLength взят из умолчаний ("+profileResponse.ContextLengthSource+"), а не из шаблона")
		}
		s.writeJSON(w, http.StatusOK, resp)
	default:
		s.writeReferenceRejected(w, captured)
	}
}

// writeReferenceApplyResult — ответ apply-пути в контракт шаблонов.
func (s *Server) writeReferenceApplyResult(w http.ResponseWriter, model string, tmpl reference.Template, captured *referenceCaptureWriter) {
	switch captured.status {
	case http.StatusAccepted:
		// Занятый бэкенд: 202 + applyId проксируются КАК ЕСТЬ — второй механизм
		// прогресса не заводим, UI продолжает опрашивать существующие
		// progress/status (§3.4, §7 п.11).
		for name, values := range captured.header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(captured.body.Bytes())
	case http.StatusOK:
		var applied modelProfileApplyResponse
		_ = json.Unmarshal(captured.body.Bytes(), &applied)
		allErrors := len(applied.Backends) > 0
		for _, backend := range applied.Backends {
			if backend.Status != "error" {
				allErrors = false
				break
			}
		}
		if allErrors {
			s.writeJSON(w, http.StatusBadGateway, referenceErrorBody("reload_failed",
				"ни один целевой бэкенд не перезагрузил модель: смотрите сообщения по бэкендам",
				map[string]interface{}{"model": model, "backends": applied.Backends}))
			return
		}
		s.writeJSON(w, http.StatusOK, referenceApplyResponse{
			Status:          "applied",
			Model:           model,
			TemplateID:      tmpl.ID,
			TemplateVersion: tmpl.TemplateVersionOrDefault(),
			Mode:            referenceModeProfileReload,
			Profile:         applied.Profile,
			Backends:        applied.Backends,
		})
	default:
		s.writeReferenceRejected(w, captured)
	}
}

// writeReferenceRejected — ошибка существующего пути профиля, переупакованная
// в контракт шаблонов (§3.4: тело 422 — как в PUT профиля, с блоком memory).
func (s *Server) writeReferenceRejected(w http.ResponseWriter, captured *referenceCaptureWriter) {
	var rejected struct {
		Error   string                 `json:"error"`
		Message string                 `json:"message"`
		Model   string                 `json:"model"`
		Memory  map[string]interface{} `json:"memory"`
	}
	_ = json.Unmarshal(captured.body.Bytes(), &rejected)
	status := captured.status
	if status == 0 {
		status = http.StatusBadGateway
	}
	code := rejected.Error
	if code == "" {
		code = "profile_rejected"
	}
	if status == http.StatusBadRequest {
		// 400 существующего PUT — это «набор не прошёл проверку».
		code = "invalid_params"
	}
	extra := map[string]interface{}{}
	if rejected.Memory != nil {
		extra["memory"] = rejected.Memory
	}
	if rejected.Model != "" {
		extra["model"] = rejected.Model
	}
	message := rejected.Message
	if message == "" {
		message = strings.TrimSpace(captured.body.String())
	}
	s.writeJSON(w, status, referenceErrorBody(code, message, extra))
}

// joinWarning — склейка предупреждений без потери первого.
func joinWarning(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}
