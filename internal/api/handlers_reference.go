// handlers_reference.go — HTTP-ручки «Справка/рекомендации + шаблоны».
//
// Контракт: docs/plans/2026-11-17-reference-templates-design.md §3
// («Контракт API»). Префикс /api/v1/reference/...; регистрация — рядом с
// профилями моделей (routes.go), права — ровно те же, что у профилей:
// AuthMiddleware(RateLimitMiddleware(...)). Отдельного механизма прав нет:
// GET доступен роли viewer, мутации — только admin + CSRF
// (internal/api/auth.go:189-214, §1.7 документа).
//
// ЧЕГО ЭТИ РУЧКИ НЕ ДЕЛАЮТ:
//   - не подставляют значения из файла в профиль модели автоматически
//     (файл — справочник и заготовки, §7 п.1);
//   - не вводят второй валидатор: имена параметров проверяются по полям
//     types.LlamaCppModelProfile, границы — существующим validateModelProfile;
//   - не заводят второе хранилище: всё лежит в internal/reference (Store).
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"ollama-loadbalancer/internal/reference"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

const (
	// referenceRecommendationsPath — коллекция записей-рекомендаций.
	referenceRecommendationsPath = "/api/v1/reference/recommendations"
	// referenceTemplatesPath — коллекция шаблонов.
	referenceTemplatesPath = "/api/v1/reference/templates"
	// referenceBodyLimit — предел тела запроса (как у PUT профиля, 64 КБ:
	// настройки загрузки — это десятки полей, а не файл).
	referenceBodyLimit = 64 * 1024
)

// referenceRecommendationsResponse — GET /api/v1/reference/recommendations (§3.1).
type referenceRecommendationsResponse struct {
	Items         []reference.Recommendation `json:"items"`
	Total         int                        `json:"total"`
	SchemaVersion int                        `json:"schemaVersion"`
}

// referenceTemplatesResponse — GET /api/v1/reference/templates (§3.2).
type referenceTemplatesResponse struct {
	Items         []reference.Template `json:"items"`
	Total         int                  `json:"total"`
	SchemaVersion int                  `json:"schemaVersion"`
}

// referenceRelatedTemplate — ссылка «из этой записи вырос шаблон».
type referenceRelatedTemplate struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// referenceToTemplateRequest — тело POST …/recommendations/{id}/to-template.
type referenceToTemplateRequest struct {
	TemplateID string `json:"templateId"`
	Name       string `json:"name"`
}

// --- общие помощники ------------------------------------------------------

// referenceStoreForRequest — хранилище или 503 store_unavailable (§3.4).
func (s *Server) referenceStoreForRequest(w http.ResponseWriter) *reference.Store {
	if s == nil || s.referenceStore == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error": "store_unavailable",
			"message": "хранилище справки/шаблонов не инициализировано: проверьте " +
				reference.EnvPathOverride + " и каталог state.json (LB_STATE_PATH)",
		})
		return nil
	}
	return s.referenceStore
}

// referenceErrorBody — единый формат ошибки: `error` + человеческое `message`
// + машинные поля (field/allowed/got/current/sent).
func referenceErrorBody(code, message string, extra map[string]interface{}) map[string]interface{} {
	body := map[string]interface{}{"error": code}
	if message != "" {
		body["message"] = message
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// decodeReferenceBody — чтение тела с DisallowUnknownFields.
//
// Та же строгость, что у PUT профиля (handlers_cppworker_profiles.go:276):
// лишнее поле — это 400 invalid_json, а не молча проигнорированное значение.
func decodeReferenceBody(w http.ResponseWriter, s *Server, r *http.Request, out interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, referenceBodyLimit)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, referenceErrorBody("invalid_json",
			"не удалось прочитать тело запроса: "+err.Error(), nil))
		return false
	}
	if len(bytes.TrimSpace(bodyBytes)) == 0 {
		s.writeJSON(w, http.StatusBadRequest, referenceErrorBody("invalid_json",
			"тело запроса пустое", nil))
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(bodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		s.writeJSON(w, http.StatusBadRequest, referenceErrorBody("invalid_json", err.Error(), nil))
		return false
	}
	return true
}

// referenceProfileBaseline — опорный профиль для проверки набора значений.
//
// ЗАЧЕМ. validateModelProfile требует contextLength > 0
// (handlers_cppworker_profiles.go:1212-1217), а набор параметров НЕ обязан
// задавать окно: при применении он мержится с текущим профилем модели.
// Поэтому границы проверяются на наборе, смерженном с валидной базой: всё, что
// задал набор, проверяется по-настоящему, а незаданное не выдумывает отказ.
//
// ВАЖНО: база участвует ТОЛЬКО в проверке. В применение уходит сырой набор
// параметров (мерж с настоящим профилем делает существующий путь PUT/apply).
func referenceProfileBaseline() types.LlamaCppModelProfile {
	return types.LlamaCppModelProfile{ContextLength: 8192, BatchSize: 512}
}

// decodeProfileUpdate — набор параметров → (значения, признак передачи).
//
// Два декодирования, ровно как в upsertModelProfile
// (handlers_cppworker_profiles.go:274-285): values нужны для значений, presence —
// чтобы отличать «поле не передано» от «передано false/0» (numGpuLayers=0 —
// валидное «только CPU», parallel=0 — «наследовать cppworker»).
func decodeProfileUpdate(raw []byte) (types.LlamaCppModelProfile, profilePresence, error) {
	var values types.LlamaCppModelProfile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&values); err != nil {
		return types.LlamaCppModelProfile{}, profilePresence{}, err
	}
	var presence profilePresence
	_ = json.Unmarshal(raw, &presence)
	return values, presence, nil
}

// jsonStructFieldRe — имя поля из ошибки encoding/json:
// `json: cannot unmarshal string into Go struct field LlamaCppModelProfile.contextLength of type int`.
var jsonStructFieldRe = regexp.MustCompile(`struct field [A-Za-z0-9_]+\.([A-Za-z0-9_]+)`)

// profileFieldFromJSONError — имя поля из ошибки декодирования ("" если нет).
func profileFieldFromJSONError(msg string) string {
	if m := jsonStructFieldRe.FindStringSubmatch(msg); len(m) == 2 {
		return m[1]
	}
	return ""
}

// profileFieldFromValidatorError — имя поля из текста validateModelProfile.
//
// Тексты валидатора начинаются с имени поля ("parallel must be in [0, 8], got 12"),
// поэтому имя вытаскивается сравнением с тем же списком имён, который даёт
// reflection по типу профиля, — второй таблицы соответствий не нужно.
func profileFieldFromValidatorError(msg string) string {
	for _, name := range reference.KnownParamNames() {
		if strings.HasPrefix(msg, name) {
			return name
		}
	}
	return ""
}

// rawObjectValue — значение параметра как обычный JSON-примитив (для ответа).
func rawObjectValue(obj map[string]json.RawMessage, field string) interface{} {
	raw, ok := obj[field]
	if !ok {
		return nil
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}

// prepareReferenceParams — ЕДИНСТВЕННАЯ проверка набора значений, без второго валидатора.
//
// Три шага, каждый переиспользует существующее:
//  1. имена — только поля types.LlamaCppModelProfile (reference.UnknownParamNames,
//     reflection по json-тегам типа) → 400 unknown_param со списком allowed;
//  2. типы — декодирование в types.LlamaCppModelProfile с DisallowUnknownFields
//     (то же, что делает PUT профиля) → 400 с кодом codeValue;
//  3. границы — validateModelProfile (handlers_cppworker_profiles.go:1211-1266)
//     на наборе, смерженном с опорной базой → 400 с кодом codeValue.
//
// codeValue — "invalid_value" для записей-рекомендаций (§3.1) и
// "invalid_params" для шаблонов (§3.2); это единственное различие, поэтому
// валидатор остаётся ОДИН.
//
// Возвращает (смерженный профиль, тело ошибки). Тело ошибки != nil → ответ 400.
func (s *Server) prepareReferenceParams(obj map[string]json.RawMessage, codeValue string) (types.LlamaCppModelProfile, map[string]interface{}) {
	unknown := reference.UnknownParamNames(reference.ObjectNames(obj))
	if len(unknown) > 0 {
		return types.LlamaCppModelProfile{}, referenceErrorBody("unknown_param",
			fmt.Sprintf("неизвестное имя параметра %q: разрешены только имена полей профиля модели", unknown[0]),
			map[string]interface{}{
				"field":   unknown[0],
				"unknown": unknown,
				"allowed": reference.KnownParamNames(),
			})
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return types.LlamaCppModelProfile{}, referenceErrorBody("invalid_json",
			"не удалось перекодировать params: "+err.Error(), nil)
	}
	values, presence, decErr := decodeProfileUpdate(raw)
	if decErr != nil {
		field := profileFieldFromJSONError(decErr.Error())
		return types.LlamaCppModelProfile{}, referenceErrorBody(codeValue, decErr.Error(),
			map[string]interface{}{"field": field, "got": rawObjectValue(obj, field)})
	}
	merged := mergeProfileUpdate(referenceProfileBaseline(), values, presence)
	if valErr := validateModelProfile(merged); valErr != nil {
		field := profileFieldFromValidatorError(valErr.Error())
		return merged, referenceErrorBody(codeValue, valErr.Error(),
			map[string]interface{}{"field": field, "got": rawObjectValue(obj, field)})
	}
	return merged, nil
}

// asRevisionConflict — type-assert на *reference.RevisionConflictError.
func asRevisionConflict(err error, target **reference.RevisionConflictError) bool {
	if err == nil {
		return false
	}
	if conflict, ok := err.(*reference.RevisionConflictError); ok {
		*target = conflict
		return true
	}
	return false
}

// --- рекомендации ---------------------------------------------------------

// handleReferenceRecommendations — диспетчер /api/v1/reference/recommendations[/{id}[/to-template]].
func (s *Server) handleReferenceRecommendations(w http.ResponseWriter, r *http.Request) {
	store := s.referenceStoreForRequest(w)
	if store == nil {
		return
	}
	trimmed := strings.TrimPrefix(r.URL.Path, referenceRecommendationsPath)
	trimmed = strings.TrimPrefix(trimmed, "/")
	if trimmed == "" {
		if r.Method != http.MethodGet {
			s.writeJSON(w, http.StatusMethodNotAllowed, referenceErrorBody("method_not_allowed",
				"коллекция поддерживает только GET; правка — PUT/DELETE по id", nil))
			return
		}
		items := store.Recommendations()
		s.writeJSON(w, http.StatusOK, referenceRecommendationsResponse{
			Items:         items,
			Total:         len(items),
			SchemaVersion: reference.SchemaVersion,
		})
		return
	}
	parts := strings.SplitN(trimmed, "/", 2)
	id := parts[0]
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}
	switch {
	case sub == "" && r.Method == http.MethodGet:
		s.getReferenceRecommendation(w, store, id)
	case sub == "" && r.Method == http.MethodPut:
		s.putReferenceRecommendation(w, r, store, id)
	case sub == "" && r.Method == http.MethodDelete:
		s.deleteReferenceRecommendation(w, store, id)
	case sub == "to-template" && r.Method == http.MethodPost:
		s.recommendationToTemplate(w, r, store, id)
	default:
		s.writeJSON(w, http.StatusMethodNotAllowed, referenceErrorBody("method_not_allowed",
			"поддерживаются GET/PUT/DELETE по id и POST …/to-template", nil))
	}
}

// getReferenceRecommendation — GET …/recommendations/{id} (§3.1).
func (s *Server) getReferenceRecommendation(w http.ResponseWriter, store *reference.Store, id string) {
	rec, ok := store.Recommendation(id)
	if !ok {
		s.writeJSON(w, http.StatusNotFound, referenceErrorBody("recommendation_not_found",
			fmt.Sprintf("запись-рекомендация %q не найдена", id), map[string]interface{}{"id": id}))
		return
	}
	related := make([]referenceRelatedTemplate, 0)
	for _, tmpl := range store.Templates() {
		if tmpl.BasedOn == id {
			related = append(related, referenceRelatedTemplate{ID: tmpl.ID, Name: tmpl.Name})
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"recommendation":   rec,
		"relatedTemplates": related,
	})
}

// putReferenceRecommendation — PUT …/recommendations/{id} (§3.1).
//
// Идемпотентный upsert: записи нет — создаётся (revision=1), есть — обновляется
// (revision растёт). Присланная в теле revision — необязательная оптимистичная
// блокировка: не совпала → 409 revision_conflict.
func (s *Server) putReferenceRecommendation(w http.ResponseWriter, r *http.Request, store *reference.Store, id string) {
	if err := reference.ValidateID(id); err != nil {
		s.writeJSON(w, http.StatusBadRequest, referenceErrorBody("invalid_id", err.Error(),
			map[string]interface{}{"id": id}))
		return
	}
	var rec reference.Recommendation
	if !decodeReferenceBody(w, s, r, &rec) {
		return
	}
	// id — из URL, не из тела: id задаётся при создании и не меняется (§2.3).
	if rec.ID != "" && rec.ID != id {
		logger.Get().Debugw("reference: id записи в теле игнорируется, используется id из URL",
			"path_id", id, "body_id", rec.ID)
	}
	rec.ID = id
	rec.Title = strings.TrimSpace(rec.Title)
	if rec.Title == "" {
		s.writeJSON(w, http.StatusBadRequest, referenceErrorBody("title_required",
			"title обязателен: запись без заголовка нельзя выбрать в UI",
			map[string]interface{}{"id": id}))
		return
	}
	if len(rec.Params) == 0 {
		s.writeJSON(w, http.StatusBadRequest, referenceErrorBody("params_required",
			"params обязателен: запись-рекомендация — это значения параметров с обоснованием, пустой набор бесполезен",
			map[string]interface{}{"id": id}))
		return
	}
	if dup := reference.DuplicateParamName(rec.Params); dup != "" {
		s.writeJSON(w, http.StatusBadRequest, referenceErrorBody("duplicate_param",
			fmt.Sprintf("параметр %q указан дважды: значение стало бы непредсказуемым", dup),
			map[string]interface{}{"field": dup}))
		return
	}
	obj, objErr := reference.ParamsToObject(rec.Params)
	if objErr != nil {
		s.writeJSON(w, http.StatusBadRequest, referenceErrorBody("invalid_value", objErr.Error(), nil))
		return
	}
	if _, bad := s.prepareReferenceParams(obj, "invalid_value"); bad != nil {
		s.writeJSON(w, http.StatusBadRequest, bad)
		return
	}
	saved, warn, err := store.PutRecommendation(rec)
	if err != nil {
		var conflict *reference.RevisionConflictError
		if asRevisionConflict(err, &conflict) {
			s.writeJSON(w, http.StatusConflict, referenceErrorBody("revision_conflict",
				"запись изменил кто-то другой: перечитайте её и повторите правку",
				map[string]interface{}{"current": conflict.Current, "sent": conflict.Sent, "id": id}))
			return
		}
		s.writeJSON(w, http.StatusInternalServerError, referenceErrorBody("store_write_failed", err.Error(), nil))
		return
	}
	body := map[string]interface{}{"status": "ok", "recommendation": saved}
	if warn != "" {
		// Конвенция upsertModelProfile: изменение в памяти — не отказ, но
		// предупреждение обязательно (иначе оператор считает его сохранённым).
		body["warning"] = warn
	}
	s.writeJSON(w, http.StatusOK, body)
}

// deleteReferenceRecommendation — DELETE …/recommendations/{id} (§3.1).
func (s *Server) deleteReferenceRecommendation(w http.ResponseWriter, store *reference.Store, id string) {
	existed, warn := store.DeleteRecommendation(id)
	if !existed {
		s.writeJSON(w, http.StatusNotFound, referenceErrorBody("recommendation_not_found",
			fmt.Sprintf("запись-рекомендация %q не найдена", id), map[string]interface{}{"id": id}))
		return
	}
	body := map[string]interface{}{"status": "ok", "id": id}
	if warn != "" {
		body["warning"] = warn
	}
	s.writeJSON(w, http.StatusOK, body)
}

// recommendationToTemplate — POST …/recommendations/{id}/to-template (§3.1).
//
// Превращает запись (список params с подписями) в шаблон (плоский объект в
// форме тела PUT профиля). Это единственный способ завести шаблон «из справки»:
// второй структуры набора параметров не заводим (§2.4).
func (s *Server) recommendationToTemplate(w http.ResponseWriter, r *http.Request, store *reference.Store, id string) {
	rec, ok := store.Recommendation(id)
	if !ok {
		s.writeJSON(w, http.StatusNotFound, referenceErrorBody("recommendation_not_found",
			fmt.Sprintf("запись-рекомендация %q не найдена", id), map[string]interface{}{"id": id}))
		return
	}
	var req referenceToTemplateRequest
	if r.ContentLength != 0 {
		if !decodeReferenceBody(w, s, r, &req) {
			return
		}
	}
	templateID := strings.TrimSpace(req.TemplateID)
	if templateID == "" {
		templateID = rec.ID
	}
	if err := reference.ValidateID(templateID); err != nil {
		s.writeJSON(w, http.StatusBadRequest, referenceErrorBody("invalid_id", err.Error(),
			map[string]interface{}{"id": templateID}))
		return
	}
	if _, exists := store.Template(templateID); exists {
		s.writeJSON(w, http.StatusConflict, referenceErrorBody("template_id_taken",
			fmt.Sprintf("шаблон с id %q уже есть: укажите другой templateId", templateID),
			map[string]interface{}{"id": templateID}))
		return
	}
	obj, objErr := reference.ParamsToObject(rec.Params)
	if objErr != nil {
		s.writeJSON(w, http.StatusBadRequest, referenceErrorBody("invalid_value",
			"запись содержит некорректный параметр: "+objErr.Error(), nil))
		return
	}
	if _, bad := s.prepareReferenceParams(obj, "invalid_params"); bad != nil {
		s.writeJSON(w, http.StatusBadRequest, bad)
		return
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, referenceErrorBody("encode_failed", err.Error(), nil))
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = rec.Title
	}
	tmpl := reference.Template{
		ID:      templateID,
		Name:    name,
		Version: 1,
		Params:  raw,
		Comment: rec.Comment,
		BasedOn: rec.ID,
		Author:  rec.Author,
	}
	saved, warn, putErr := store.PutTemplate(tmpl)
	if putErr != nil {
		s.writeJSON(w, http.StatusInternalServerError, referenceErrorBody("store_write_failed", putErr.Error(), nil))
		return
	}
	body := map[string]interface{}{"status": "created", "template": saved}
	if warn != "" {
		body["warning"] = warn
	}
	s.writeJSON(w, http.StatusCreated, body)
}

// --- шаблоны --------------------------------------------------------------

// handleReferenceTemplates — диспетчер /api/v1/reference/templates[/{id}[/preview|/apply]].
func (s *Server) handleReferenceTemplates(w http.ResponseWriter, r *http.Request) {
	store := s.referenceStoreForRequest(w)
	if store == nil {
		return
	}
	trimmed := strings.TrimPrefix(r.URL.Path, referenceTemplatesPath)
	trimmed = strings.TrimPrefix(trimmed, "/")
	if trimmed == "" {
		if r.Method != http.MethodGet {
			s.writeJSON(w, http.StatusMethodNotAllowed, referenceErrorBody("method_not_allowed",
				"коллекция поддерживает только GET; правка — PUT/DELETE по id", nil))
			return
		}
		items := store.Templates()
		s.writeJSON(w, http.StatusOK, referenceTemplatesResponse{
			Items:         items,
			Total:         len(items),
			SchemaVersion: reference.SchemaVersion,
		})
		return
	}
	parts := strings.SplitN(trimmed, "/", 2)
	id := parts[0]
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}
	switch {
	case sub == "" && r.Method == http.MethodGet:
		s.getReferenceTemplate(w, store, id)
	case sub == "" && r.Method == http.MethodPut:
		s.putReferenceTemplate(w, r, store, id)
	case sub == "" && r.Method == http.MethodDelete:
		s.deleteReferenceTemplate(w, store, id)
	case sub == "preview" && r.Method == http.MethodPost:
		s.previewReferenceTemplate(w, r, store, id)
	case sub == "apply" && r.Method == http.MethodPost:
		s.applyReferenceTemplate(w, r, store, id)
	default:
		s.writeJSON(w, http.StatusMethodNotAllowed, referenceErrorBody("method_not_allowed",
			"поддерживаются GET/PUT/DELETE по id, POST …/preview и POST …/apply", nil))
	}
}

// getReferenceTemplate — GET …/templates/{id} (§3.2): шаблон + кто им уже
// пользуется (профили моделей, значения которых совпадают с шаблоном).
func (s *Server) getReferenceTemplate(w http.ResponseWriter, store *reference.Store, id string) {
	tmpl, ok := store.Template(id)
	if !ok {
		s.writeJSON(w, http.StatusNotFound, referenceErrorBody("template_not_found",
			fmt.Sprintf("шаблон %q не найден", id), map[string]interface{}{"id": id}))
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"template": tmpl,
		"usage":    s.referenceTemplateUsage(tmpl),
	})
}

// referenceTemplateUsage — `{usedByProfiles:[...]}`: у каких профилей модели
// значения уже совпадают с шаблоном.
//
// Это ПОДСКАЗКА, а не «шаблон применён»: совпадение значений ничего не говорит
// о том, откуда они взялись (профиль — единственный источник истины, §7 п.1).
func (s *Server) referenceTemplateUsage(tmpl reference.Template) map[string]interface{} {
	used := make([]string, 0)
	obj, err := reference.DecodeParamsObject(tmpl.Params)
	if err != nil {
		return map[string]interface{}{"usedByProfiles": used}
	}
	for name, profile := range s.modelProfilesSnapshotR423() {
		if profileMatchesParams(profile, obj) {
			used = append(used, name)
		}
	}
	sort.Strings(used)
	return map[string]interface{}{"usedByProfiles": used}
}

// profileMatchesParams — все значения шаблона совпадают с профилем модели.
func profileMatchesParams(profile types.LlamaCppModelProfile, params map[string]json.RawMessage) bool {
	fields := profileFieldMap(profile)
	for name, raw := range params {
		var want interface{}
		if err := json.Unmarshal(raw, &want); err != nil {
			return false
		}
		got, ok := fields[name]
		if !ok {
			// Поле с omitempty отсутствует в JSON профиля → значение нулевое.
			got = zeroJSONValue(want)
		}
		if !reflect.DeepEqual(want, got) {
			return false
		}
	}
	return true
}

// zeroJSONValue — нулевое значение того же JSON-типа.
func zeroJSONValue(v interface{}) interface{} {
	switch v.(type) {
	case string:
		return ""
	case bool:
		return false
	case float64:
		return float64(0)
	default:
		return nil
	}
}

// profileFieldMap — профиль как карта «имя поля → JSON-значение».
func profileFieldMap(profile types.LlamaCppModelProfile) map[string]interface{} {
	raw, err := json.Marshal(profile)
	if err != nil {
		return map[string]interface{}{}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return map[string]interface{}{}
	}
	out := make(map[string]interface{}, len(fields))
	for name, value := range fields {
		var v interface{}
		if err := json.Unmarshal(value, &v); err != nil {
			continue
		}
		out[name] = v
	}
	return out
}

// putReferenceTemplate — PUT …/templates/{id} (§3.2).
func (s *Server) putReferenceTemplate(w http.ResponseWriter, r *http.Request, store *reference.Store, id string) {
	if err := reference.ValidateID(id); err != nil {
		s.writeJSON(w, http.StatusBadRequest, referenceErrorBody("invalid_id", err.Error(),
			map[string]interface{}{"id": id}))
		return
	}
	var tmpl reference.Template
	if !decodeReferenceBody(w, s, r, &tmpl) {
		return
	}
	if tmpl.ID != "" && tmpl.ID != id {
		logger.Get().Debugw("reference: id шаблона в теле игнорируется, используется id из URL",
			"path_id", id, "body_id", tmpl.ID)
	}
	tmpl.ID = id
	tmpl.Name = strings.TrimSpace(tmpl.Name)
	if tmpl.Name == "" {
		tmpl.Name = id
	}
	obj, objErr := reference.DecodeParamsObject(tmpl.Params)
	if objErr != nil {
		s.writeJSON(w, http.StatusBadRequest, referenceErrorBody("invalid_params", objErr.Error(), nil))
		return
	}
	if len(obj) == 0 {
		s.writeJSON(w, http.StatusBadRequest, referenceErrorBody("params_required",
			"шаблон без параметров ничего не меняет — добавьте значения",
			map[string]interface{}{"id": id}))
		return
	}
	if _, bad := s.prepareReferenceParams(obj, "invalid_params"); bad != nil {
		s.writeJSON(w, http.StatusBadRequest, bad)
		return
	}
	saved, warn, err := store.PutTemplate(tmpl)
	if err != nil {
		var conflict *reference.RevisionConflictError
		if asRevisionConflict(err, &conflict) {
			s.writeJSON(w, http.StatusConflict, referenceErrorBody("revision_conflict",
				"шаблон изменил кто-то другой: перечитайте его и повторите правку",
				map[string]interface{}{"current": conflict.Current, "sent": conflict.Sent, "id": id}))
			return
		}
		s.writeJSON(w, http.StatusInternalServerError, referenceErrorBody("store_write_failed", err.Error(), nil))
		return
	}
	body := map[string]interface{}{"status": "ok", "template": saved}
	if warn != "" {
		body["warning"] = warn
	}
	s.writeJSON(w, http.StatusOK, body)
}

// deleteReferenceTemplate — DELETE …/templates/{id} (§3.2).
func (s *Server) deleteReferenceTemplate(w http.ResponseWriter, store *reference.Store, id string) {
	existed, warn := store.DeleteTemplate(id)
	if !existed {
		s.writeJSON(w, http.StatusNotFound, referenceErrorBody("template_not_found",
			fmt.Sprintf("шаблон %q не найден", id), map[string]interface{}{"id": id}))
		return
	}
	body := map[string]interface{}{"status": "ok", "id": id}
	if warn != "" {
		body["warning"] = warn
	}
	s.writeJSON(w, http.StatusOK, body)
}
