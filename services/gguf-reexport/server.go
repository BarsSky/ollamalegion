// server.go — HTTP API сервиса ре-экспорта.
//
// ЧТО ЗДЕСЬ ПРИНЦИПИАЛЬНО. Ни одного 400 без объяснения и ни одного «пустого»
// 200: WebUI показывает оператору причину (`code` + `message`), поэтому ошибки
// отдаются структурой {"error":{"code","message"}} с осмысленным статусом.
// Список моделей отдаётся даже если часть файлов битая — поле read_error у
// конкретной модели информативнее, чем 500 на весь список.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

type server struct {
	cfg   config
	mgr   *Manager
	info  *infoProbe
	cache *modelsCache
	log   *zap.SugaredLogger
}

func newServer(cfg config, mgr *Manager, info *infoProbe, cache *modelsCache, log *zap.SugaredLogger) *server {
	return &server{cfg: cfg, mgr: mgr, info: info, cache: cache, log: log}
}

// routes собирает mux. Префикс /api/reexport/ — единственный публичный контракт:
// его проксирует nginx WebUI (webui/nginx.conf), наружу порт не публикуется.
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/api/version", s.handleVersion)
	mux.HandleFunc("/api/reexport/capabilities", s.handleCapabilities)
	mux.HandleFunc("/api/reexport/models", s.handleModels)
	mux.HandleFunc("/api/reexport/jobs", s.handleJobs)
	mux.HandleFunc("/api/reexport/jobs/", s.handleJobByID)
	return s.withAuth(s.withLogging(mux))
}

// withLogging — короткая строка на запрос: без неё «форме ничего не приходит»
// выясняется только по логам nginx.
func (s *server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Debugw("http", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration_ms", time.Since(start).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// withAuth — необязательный токен на изменяющие запросы.
//
// ЧТЕНИЕ ОСТАВЛЕНО БЕЗ ТОКЕНА ОСОЗНАННО: возможности/список моделей нужны
// странице, чтобы решить, показывать ли блок; гейтить их значит требовать токен
// от WebUI, у которого его по умолчанию нет (R222: браузер ходит сессией).
// Запись (создание/отмена задачи) — меняет файлы на модельном томе, поэтому при
// заданном REEXPORT_API_TOKEN требует его.
func (s *server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.APIToken == "" || r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("X-API-Token")
		if got == "" {
			got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.APIToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "нужен заголовок X-API-Token (REEXPORT_API_TOKEN)")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "только GET")
		return
	}
	active := 0
	for _, j := range s.mgr.List() {
		if j.Status == statusRunning || j.Status == statusQueued {
			active++
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"service": "gguf-reexport",
		"version": buildIdentity()["build_version"],
		"jobs":    map[string]int{"active": active},
	})
}

func (s *server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, buildIdentity())
}

func (s *server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "только GET")
		return
	}
	info := s.info.Get(r.Context())
	caps := buildCapabilities(info)
	caps.Limits.ModelsDir = s.cfg.ModelsDir
	// Отдаём действующий legacy-каталог (пусто, если переменная указывает за том):
	// ответ «out_dir: /второй/диск» при фактической записи в каталог модели был бы
	// ложью ровно о том, что чинит D10.2.
	caps.Limits.OutDir = s.cfg.legacyOutBase()
	caps.Limits.OutDirIgnored = s.cfg.legacyOutIgnored()
	caps.Limits.JobsDir = s.cfg.JobsDir
	caps.Limits.ScanDepth = s.cfg.ScanDepth
	caps.Limits.MaxListed = s.cfg.ScanMaxFiles
	caps.Limits.ImageWorker = s.cfg.ImageWorkerURL
	caps.Limits.ImageModelsDir = s.cfg.imageOutRoot()
	caps.Limits.ImageOutFallback = s.cfg.imageOutFallback()
	writeJSON(w, http.StatusOK, caps)
}

// handleModels — список GGUF в томе моделей с фактами схемы.
//
// `?refresh=1` сбрасывает кэш: после ре-экспорта файл изменился, и форма должна
// увидеть новый размер/схему, не дожидаясь TTL.
func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "только GET")
		return
	}
	if r.URL.Query().Get("refresh") != "" {
		s.cache.mu.Lock()
		s.cache.items = map[string]cachedFacts{}
		s.cache.mu.Unlock()
	}
	models := listModels(s.cfg.ModelsDir, s.cache, s.cfg.ScanDepth, s.cfg.ScanMaxFiles)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"models":   models,
		"root":     s.cfg.ModelsDir,
		"out_root": s.cfg.outRoot(),
		"count":    len(models),
	})
}

// handleJobs — GET список / POST создание.
func (s *server) handleJobs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{"jobs": s.mgr.List()})
	case http.MethodPost:
		var req createRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", "тело не разобрано: "+err.Error())
			return
		}
		view, err := s.mgr.Create(req)
		if err != nil {
			writeJobErr(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, view)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET или POST")
	}
}

// handleJobByID — GET статус / DELETE отмена.
func (s *server) handleJobByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/reexport/jobs/")
	id = strings.TrimSuffix(id, "/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusNotFound, "job_not_found", "ожидается /api/reexport/jobs/{id}")
		return
	}
	switch r.Method {
	case http.MethodGet:
		view, ok := s.mgr.Get(id)
		if !ok {
			writeError(w, http.StatusNotFound, "job_not_found", "задача не найдена: "+id)
			return
		}
		writeJSON(w, http.StatusOK, view)
	case http.MethodDelete:
		view, err := s.mgr.Cancel(id)
		if err != nil {
			writeJobErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET или DELETE")
	}
}

// writeJSON — ответ с явным Content-Type: WebUI разбирает JSON, а nginx проксирует
// поток как есть (без сжатия на коротких ответах это дешевле).
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError — единая форма ошибки.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]interface{}{"error": &jobError{Code: code, Message: message}})
}

// writeJobErr — ошибка менеджера → HTTP-статус.
//
// Коды выбраны так, чтобы их было ВИДНО в браузере при разборе: 404 — нет модели
// или задачи, 409 — конфликт состояния (результат существует, задача завершена),
// 422 — запрос понят, но невозможен (цель недоступна/путь вне корней), 507 —
// кончилось место. «500 на всё» лишает UI возможности подсказать действие.
func writeJobErr(w http.ResponseWriter, err error) {
	var je *jobError
	if !asJobError(err, &je) {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	status := http.StatusBadRequest
	switch je.Code {
	case "model_not_found", "job_not_found":
		status = http.StatusNotFound
	case "dst_exists", "job_finished":
		status = http.StatusConflict
	// 400 — запрос назвал явно недопустимый путь: это ошибка запроса, а не
	// состояния задачи, поэтому не 422 (там «понят, но невозможно»).
	case "path_not_allowed":
		status = http.StatusBadRequest
	case "unknown_target", "target_unavailable", "out_of_models_dir",
		"model_not_patchable", "input_missing":
		status = http.StatusUnprocessableEntity
	case "insufficient_space":
		status = http.StatusInsufficientStorage
	case "queue_full":
		status = http.StatusTooManyRequests
	case "unauthorized":
		status = http.StatusUnauthorized
	}
	writeJSON(w, status, map[string]interface{}{"error": je})
}

func asJobError(err error, out **jobError) bool {
	return errors.As(err, out)
}

// runServer — поднятие HTTP-сервера с корректным завершением.
func runServer(ctx context.Context, s *server, addr string, log *zap.SugaredLogger) error {
	srv := &http.Server{
		Addr:    addr,
		Handler: s.routes(),
		// ReadHeaderTimeout — против медленных клиентов из сети проекта.
		ReadHeaderTimeout: 10 * time.Second,
		// Таймаутов записи/чтения тела СОЗНАТЕЛЬНО нет: список моделей читает
		// заголовки десятка GGUF-файлов (секунды на холодном кэше), и обрывать
		// это по таймауту значило бы «форма мигает пустотой». Работа (сам
		// ре-экспорт) идёт вне HTTP-запроса — она возвращается 202 сразу.
	}
	errCh := make(chan error, 1)
	go func() {
		log.Infow("http listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}
