// handlers_image_components.go — R710 (2026-11-24): плоскость БАЛАНСЕРА для
// добавления недостающих компонентов image-набора (VAE / text encoder).
//
// ЗАЧЕМ ЗДЕСЬ, А НЕ ТОЛЬКО НА ВОРКЕРЕ. Страница WebUI уже работает с
// management-плоскостью (/api/v1/image/...): список моделей, каталог, профили,
// ресурсы. Кнопка «Добавить VAE (по умолчанию)» появляется в двух местах (таб
// HuggingFace и «Модели на диске») и не должна требовать от JS знания адреса
// воркера и его токена — этим занимается балансер, как и для остальных ручек.
//
//	GET  /api/v1/image/components/catalog?family=<f>&roles=vae,llm
//	     — каталог «семейство → рекомендуемые компоненты». Отвечается ЛОКАЛЬНО
//	       (данные sdbackend/components.go — это статическое знание о мире, а не
//	       состояние кластера): каталог обязан работать и в кластере без единого
//	       image-бэкенда, иначе страница не смогла бы показать «откуда взять VAE».
//	POST /api/v1/image/models/{name}/components
//	     — добавить компонент к набору на КОНКРЕТНОМ image-воркере. Воркер
//	       выбирается так: ?backendId=<id> (страница его знает и всегда шлёт) либо
//	       поиском модели по всем image-бэкендам; тело уходит в воркер как есть,
//	       ответ — тоже (202 + trackingId, 409 role_already_present, …).
//
// ЧЕГО ЗДЕСЬ НЕТ: собственной логики скачивания и своей таблицы компонентов.
// Данные — sdbackend (там же, где вердикт совместимости, который их и требует),
// установка — sdbackend.HFManager на воркере. Балансер только доставляет.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/internal/sdbackend"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// GET /api/v1/image/components/catalog
// ============================================================

// handleImageComponentsCatalog — каталог компонентов для семейства.
//
// Ответ строится из sdbackend.ComponentCatalog: тот же код, что отдаёт воркер в
// GET /api/image/components/catalog, поэтому UI видит ОДИН и тот же список и на
// странице, и в прокси-пути.
func (s *Server) handleImageComponentsCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	q := r.URL.Query()
	family := strings.TrimSpace(q.Get("family"))
	if family == "" {
		family = "other"
	}
	roles := splitRolesParam(q.Get("roles"))
	components := sdbackend.ComponentCatalog(family, roles)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"family":     family,
		"roles":      roles,
		"known":      sdbackend.ComponentCatalogKnown(family),
		"count":      len(components),
		"components": components,
		"families":   sdbackend.ComponentCatalogFamilies(),
		"maxFiles":   sdbackend.ComponentInstallMaxFiles,
		"maxSizeMb":  sdbackend.ComponentInstallMaxSizeMB,
	})
}

// splitRolesParam — "vae,llm" → ["vae","llm"].
func splitRolesParam(raw string) []string {
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
// POST /api/v1/image/models/{name}/components
// ============================================================

// handleImageModelComponents — добавить компонент к набору на image-воркере.
//
// ПОИСК ВОРКЕРА. ?backendId= — предпочтительный путь (страница знает выбранный
// бэкенд и шлёт его явно). Без параметра балансер спрашивает каждый image-воркер
// (GET /api/image/models) и берёт первый, у которого модель есть. Если модель
// есть на нескольких — сортировка по id делает выбор детерминированным (иначе
// один и тот же клик уходил бы то на один, то на другой узел, и «вердикт
// обновился, а на другом узле нет»).
func (s *Server) handleImageModelComponents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	name, rest, ok := splitImageModelScopedPath(r.URL.Path)
	if !ok || name == "" || rest != "components" {
		s.writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "unknown_image_model_path",
			"message": "expected POST /api/v1/image/models/{name}/components",
		})
		return
	}
	backendID := strings.TrimSpace(r.URL.Query().Get("backendId"))
	if backendID == "" {
		backendID = strings.TrimSpace(r.URL.Query().Get("backend"))
	}
	if backendID == "" {
		resolved, err := s.findImageBackendWithModel(r.Context(), name)
		if err != nil {
			s.writeJSON(w, http.StatusNotFound, map[string]string{
				"error":   "image_model_not_found",
				"message": err.Error(),
				"model":   name,
			})
			return
		}
		backendID = resolved
	}
	// Путь воркера тот же (меняется только префикс плоскости): балансер не
	// «дорисовывает» контракт, а доставляет запрос как есть.
	s.proxyToImageWorker(w, r, backendID, "/api/image/models/"+name+"/components")
}

// splitImageModelScopedPath — разбор /api/v1/image/models/{name}/{rest}.
//
// Сознательно НЕ переиспользует splitBackendScopedPath: разные префиксы и разные
// правила (имя модели может содержать точки), а «универсальный» сплиттер с двумя
// режимами читается хуже, чем два простых.
func splitImageModelScopedPath(path string) (name, rest string, ok bool) {
	const prefix = "/api/v1/image/models/"
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

// findImageBackendWithModel — первый (по id) image-бэкенд, в чьём каталоге есть модель.
//
// Параллельно и с коротким таймаутом: страница ждёт этот ответ (контрольный
// таймаут UI — 15 с), а GET /api/image/models у воркера — дешёвый вызов.
func (s *Server) findImageBackendWithModel(ctx context.Context, name string) (string, error) {
	if s.proxy == nil {
		return "", fmt.Errorf("proxy unavailable")
	}
	backends := make([]types.Backend, 0)
	for _, b := range s.proxy.GetAllBackends() {
		if b.Type == types.BackendTypeImage {
			backends = append(backends, b)
		}
	}
	if len(backends) == 0 {
		return "", fmt.Errorf("no image backends registered in the cluster")
	}
	sort.Slice(backends, func(i, j int) bool { return backends[i].ID < backends[j].ID })

	var wg sync.WaitGroup
	var mu sync.Mutex
	found := ""
	for _, b := range backends {
		wg.Add(1)
		go func(backend types.Backend) {
			defer wg.Done()
			if !s.imageWorkerHasModel(ctx, backend, name) {
				return
			}
			mu.Lock()
			if found == "" || backend.ID < found {
				found = backend.ID
			}
			mu.Unlock()
		}(b)
	}
	wg.Wait()
	if found == "" {
		names := make([]string, 0, len(backends))
		for _, b := range backends {
			names = append(names, b.ID)
		}
		return "", fmt.Errorf("model %q is not in the catalog of any image backend (%s)",
			name, strings.Join(names, ", "))
	}
	return found, nil
}

// imageWorkerHasModel — есть ли модель в GET /api/image/models воркера.
//
// Ошибка/таймаут = «не знаю» = false: вызов идёт от действия пользователя, и
// выбрать узел, который не ответил, значило бы отдать непонятную ошибку вместо
// честного «модели нет ни на одном узле».
func (s *Server) imageWorkerHasModel(ctx context.Context, backend types.Backend, name string) bool {
	port := backend.EffectiveImagePort()
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	url := fmt.Sprintf("http://%s:%d/api/image/models", backend.Host, port)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	if token := s.imageWorkerAPIToken(backend.ID); token != "" {
		req.Header.Set(types.HeaderXAPIToken, token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Get().Debugw("image worker model probe failed", "backend", backend.ID, "error", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return false
	}
	var payload struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	for _, m := range payload.Models {
		if m.Name == name {
			return true
		}
	}
	return false
}
