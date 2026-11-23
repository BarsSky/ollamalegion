// handlers_components.go — R710 (2026-11-24): «в наборе не хватает VAE/text
// encoder» → скачать недостающее ПРЯМО ИЗ ИНТЕРФЕЙСА.
//
// ЖИВОЙ ДЕФЕКТ (скриншоты пользователя). Вкладка HuggingFace → выбор файлов →
// вердикт: «imageworker НЕ сможет загрузить — движок запустит набор как DiT
// (--diffusion-model + отдельные VAE/text encoder), но не хватает: VAE, text
// encoder (LLM)». При этом в самом репозитории (`Qwen-Image-Edit-2511`,
// `Qwen-Image-2.1-Turbo-Uncensored`) этих файлов НЕТ — только diffusion. UI
// советовал «добавьте их файлами из другого репозитория (например VAE
// ae.safetensors)», но открыть другой репозиторий и добавить из него ОДИН файл к
// текущему набору было нельзя: ни ручки, ни кнопки.
//
// ЧТО ЗДЕСЬ:
//
//	GET  /api/image/components/catalog?family=<f>&roles=vae,llm
//	     — каталог «семейство → рекомендуемые компоненты по ролям» (данные из
//	       sdbackend/components.go, не хардкод в UI). Для неизвестного семейства
//	       — пустой массив и known=false (UI честно не показывает кнопки
//	       «по умолчанию»).
//	POST /api/image/models/{name}/components
//	     — добавить компонент в УЖЕ скачанный bundle: скачать файлы, прописать
//	       роль в profile.json, пересканировать реестр (вердикт пересчитается).
//	       202 + trackingId: прогресс/отмена — существующим механизмом
//	       GET /api/hf/progress?bundleId=… и POST /api/hf/cancel.
//
// ПОЧЕМУ ПУТЬ С ИМЕНЕМ МОДЕЛИ, А НЕ `{name}` В ТЕЛЕ: имя модели — это каталог
// внутри modelsDir, то есть часть идентичности ресурса; тело тогда отвечает
// только за «что добавить» ({role, repo, files}), и UI собирает URL из того, что
// уже показывает в таблице «Модели на диске».
package main

import (
	"errors"
	"net/http"
	"strings"

	"ollama-loadbalancer/internal/sdbackend"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// GET /api/image/components/catalog
// ============================================================

// handleComponentCatalog — каталог компонентов семейства.
//
// Параметры:
//
//	family — qwen_image|flux|flux2|sd3|z_image|chroma|sdxl|sd15|other
//	roles  — через запятую: vae,llm,t5xxl,clip_l,clip_g,clip_vision
//
// Ответ СОЗНАТЕЛЬНО отдаёт `known` (есть ли у семейства каталог) и `count`:
// пустой массив — это не ошибка, а «для этого семейства каталога нет», и UI
// обязан показать только ручной путь («Выбрать из другого репозитория…»).
func (a *App) handleComponentCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	q := r.URL.Query()
	family := strings.TrimSpace(q.Get("family"))
	if family == "" {
		family = "other"
	}
	roles := splitComponentRoles(q.Get("roles"))
	for _, role := range roles {
		if !types.IsValidImageFileRole(role) || !sdbackend.IsComponentRole(role) {
			writeJSONError(w, http.StatusBadRequest, sdbackend.CompErrRoleInvalid,
				"unknown component role "+role+
					" (allowed: vae, llm, t5xxl, clip_l, clip_g, clip_vision, taesd, upscaler, controlnet, ip_adapter)")
			return
		}
	}
	components := sdbackend.ComponentCatalog(family, roles)
	writeJSON(w, http.StatusOK, map[string]any{
		"family":     family,
		"roles":      roles,
		"known":      sdbackend.ComponentCatalogKnown(family),
		"count":      len(components),
		"components": components,
		// Подсказка клиенту: семейства, для которых каталог заполнен. Нужна не
		// для отрисовки (её рисует вердикт), а чтобы оператор/тест видели, что
		// «пусто» — это свойство семейства, а не упавший запрос.
		"families": sdbackend.ComponentCatalogFamilies(),
		// Лимиты — те же, что применит POST: UI показывает их в подтверждении,
		// чтобы «почему отказали» не выяснялось после скачивания.
		"maxFiles":  sdbackend.ComponentInstallMaxFiles,
		"maxSizeMb": sdbackend.ComponentInstallMaxSizeMB,
	})
}

// splitComponentRoles — "vae, llm" → ["vae","llm"] (порядок запроса, без дублей).
func splitComponentRoles(raw string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		role := strings.TrimSpace(part)
		if role == "" || seen[role] {
			continue
		}
		seen[role] = true
		out = append(out, role)
	}
	return out
}

// ============================================================
// POST /api/image/models/{name}/components
// ============================================================

// handleModelComponents — добавить недостающий компонент в скачанный bundle.
//
// Работа НЕ выполняется в этом вызове: валидация + постановка (202), сама
// загрузка идёт фоном (гигабайты, а контрольный таймаут UI — 15 с), прогресс
// отдаёт существующая ручка GET /api/hf/progress по trackingId.
//
// Коды отказа (их читает UI и проверяют тесты):
//
//	400 invalid_role            роль не из контракта или diffusion
//	400 repo_required|files_required|bad_component_file|too_many_files
//	400 invalid_path            modelDir вне каталога моделей
//	404 bundle_not_found        у воркера нет такого каталога модели
//	404 component_file_not_found файла нет в репозитории (+available)
//	409 role_already_present    роль уже занята (+currentFile)
//	409 component_file_conflict basename занят другой ролью (+currentFile)
//	413 component_too_large     больше sdbackend.ComponentInstallMaxSizeMB
//	502 component_repo_unavailable  HF недоступен/репозиторий не читается
func (a *App) handleModelComponents(w http.ResponseWriter, r *http.Request) {
	// Сначала ПУТЬ, потом метод: этот хендлер висит на префиксе
	// "/api/image/models/", и для чужих подпутей (опечатка клиента) честный
	// ответ — 404 «такого пути нет», а не 405 «не тот метод».
	name, rest, ok := splitModelScopedPath(r.URL.Path)
	if !ok || name == "" || rest != "components" {
		writeJSONError(w, http.StatusNotFound, "unknown_model_path",
			"expected POST /api/image/models/{name}/components")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	hf, ok := a.hfManager(w)
	if !ok {
		return
	}
	var req componentsRequest
	if err := decodeHFJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	hf.SetToken(requestHFToken(r))

	install, err := hf.StartComponentInstall(sdbackend.ComponentInstallRequest{
		Bundle:   name,
		Role:     req.Role,
		Repo:     req.Repo,
		Files:    req.Files,
		Revision: req.Revision,
		Default:  req.Default,
		ModelDir: req.ModelDir,
	})
	if err != nil {
		writeComponentError(w, err)
		return
	}
	// progressUrl в ответе — абсолютный путь воркера; UI ходит через прокси
	// балансера, поэтому путь относительный (так же, как у /api/hf/bundle).
	writeJSON(w, http.StatusAccepted, install)
}

// componentsRequest — тело POST (комментарий к полям — в
// sdbackend.ComponentInstallRequest; здесь только JSON-имена контракта UI).
type componentsRequest struct {
	Role     string   `json:"role"`
	Repo     string   `json:"repo"`
	Files    []string `json:"files"`
	Revision string   `json:"revision,omitempty"`
	Default  bool     `json:"default,omitempty"`
	ModelDir string   `json:"modelDir,omitempty"`
}

// splitModelScopedPath — разбор /api/image/models/{name}/{rest}.
//
// Имя модели может содержать точки и дефисы (qwen-image-2.1-turbo-q4_k_m), но не
// слэши: имя каталога задаётся валидатором загрузчика. Пустой rest или лишние
// сегменты — не наш путь.
func splitModelScopedPath(path string) (name, rest string, ok bool) {
	const prefix = "/api/image/models/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	tail := strings.TrimPrefix(path, prefix)
	if tail == "" {
		return "", "", false
	}
	if i := strings.Index(tail, "/"); i >= 0 {
		return tail[:i], strings.TrimPrefix(tail[i+1:], "/"), true
	}
	return tail, "", true
}

// writeComponentError — перевод ComponentError в HTTP-статус + поля ответа.
//
// ПОЧЕМУ ТАБЛИЦЕЙ, А НЕ «400 НА ВСЁ»: оператору и клиенту нужны разные действия
// (409 = роль занята, выбери другую; 413 = слишком большой, возьми меньший
// квант; 404 = файла нет в репозитории, посмотри available), а «400» не даёт
// понять, что делать.
func writeComponentError(w http.ResponseWriter, err error) {
	var ce *sdbackend.ComponentError
	if !errors.As(err, &ce) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	body := map[string]any{"error": ce.Message, "code": ce.Code}
	if ce.CurrentFile != "" {
		body["currentFile"] = ce.CurrentFile
	}
	if len(ce.Available) > 0 {
		body["available"] = ce.Available
	}
	if ce.Repo != "" {
		body["repo"] = ce.Repo
	}
	if len(ce.Files) > 0 {
		body["files"] = ce.Files
	}
	status := http.StatusBadRequest
	switch ce.Code {
	case sdbackend.CompErrRolePresent, sdbackend.CompErrFileConflicts:
		status = http.StatusConflict
	case sdbackend.CompErrBundleNotFound, sdbackend.CompErrFileNotFound:
		status = http.StatusNotFound
	case sdbackend.CompErrTooLarge:
		status = http.StatusRequestEntityTooLarge
	case sdbackend.CompErrRepoUnavailable:
		status = http.StatusBadGateway
	}
	writeJSON(w, status, body)
}
