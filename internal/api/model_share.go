// model_share.go — R89 (2026-10-08): перенос модели между бэкендами.
//
// ЗАЧЕМ. В кластере несколько машин, и модель лежит только на одной из них.
// До этой правки единственным способом получить её на второй машине была
// повторная загрузка из HuggingFace: интернет, HF-токен, время и риск другой
// ревизии файла. Теперь балансер СТРИМИТ файл из источника в приёмник:
//
//	источник  GET  /api/models/export?name=<file>        (cppworker, один .gguf)
//	          GET  /api/image/models/export?name=<bundle> (sdworker, tar каталога)
//	приёмник  POST /api/models/import?filename=...&size=...
//	          POST /api/image/models/import?name=...&bytes=...&files=...
//
// ПОЧЕМУ ЧЕРЕЗ БАЛАНСЕР, А НЕ «ПУСТЬ ЦЕЛЬ СКАЧАЕТ САМА»: у приёмника может не
// быть сети до источника (разные подсети, firewall) — а до балансера есть у всех,
// потому что все воркеры сами регистрируются на нём. Плюс так у нас есть единая
// точка прогресса, отмены и понятных ошибок.
//
// ТАЙМАУТЫ: капа на перенос НЕТ (доктрина проекта — duration-кап на работу
// запрещён, см. internal/balancer/timeout_policy.go). Задание живёт, пока не
// завершится или пока оператор не отменит его (POST .../cancel).
package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// Состояния задания и его целей (строки — часть контракта для WebUI).
const (
	shareStateRunning  = "running"
	shareStateDone     = "done"
	shareStatePartial  = "partial"
	shareStateFailed   = "failed"
	shareStateCanceled = "canceled"

	shareTargetPending = "pending"
	shareTargetRunning = "running"
	shareTargetDone    = "done"
	shareTargetSkipped = "skipped"
	shareTargetFailed  = "failed"
)

// shareProgressEvery — как часто обновляем прогресс (по байтам и по времени).
// Слишком часто = лишние блокировки; слишком редко = «замерший» прогресс-бар.
const (
	shareProgressBytes = 4 << 20 // 4 MiB
	shareProgressEvery = 500 * time.Millisecond
)

// R91 (2026-10-09): проверка свободного места у приёмника.
//
// ЗАЧЕМ. Перенос — это гигабайты, которые принимаются прямо в каталог моделей
// приёмника. До этой правки единственным признаком нехватки места был отказ
// ПОСЛЕ передачи всех байтов (или, хуже, забитый под ноль диск, на котором уже
// лежат другие модели — тогда приёмник роняет запись логов и временных файлов).
// Агент бэкенда сообщает свободное место (SystemMetrics.DiskFree, МБ), поэтому
// причину можно назвать сразу, с числами и без сети.
//
// РЕЗЕРВ. Оставлять приёмнику ноль свободного места нельзя: модели занимают
// почти весь объём, а ОС и воркеру нужен запас под логи, tmp и переиндексацию.
// Величина настраивается (LB_SHARE_DISK_RESERVE_MB), 0 = требовать ровно размер
// модели. Проверка НЕ блокирует перенос, когда телеметрии нет вовсе (нет агента
// или он не присылал диск): отсутствие цифры — не повод отказывать.
const (
	// DefaultShareDiskReserveMB — запас свободного места на приёмнике (МБ).
	DefaultShareDiskReserveMB = 512
	// EnvShareDiskReserveMB — переопределение запаса, МБ (0 = без запаса).
	EnvShareDiskReserveMB = "LB_SHARE_DISK_RESERVE_MB"
)

// shareDiskReserveMB — запас свободного места на приёмнике из окружения.
func shareDiskReserveMB() int64 {
	v := strings.TrimSpace(os.Getenv(EnvShareDiskReserveMB))
	if v == "" {
		return DefaultShareDiskReserveMB
	}
	mb, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		logger.Get().Warnw("LB_SHARE_DISK_RESERVE_MB is not a number, using default",
			"value", v, "default_mb", DefaultShareDiskReserveMB)
		return DefaultShareDiskReserveMB
	}
	if mb < 0 {
		return 0
	}
	return mb
}

// shareTargetDiskShortfall — сколько байт не хватает приёмнику под перенос.
//
// Возвращает (недостача_байт, свободно_МБ, известно_ли_место). Третий результат
// false означает «телеметрии о диске нет» — вызывающий пропускает проверку.
func (s *Server) shareTargetDiskShortfall(backendID string, needBytes int64) (int64, uint64, bool) {
	if s.proxy == nil || needBytes <= 0 {
		return 0, 0, false
	}
	mm := s.proxy.GetMetricsManager()
	if mm == nil {
		return 0, 0, false
	}
	m, ok := mm.SnapshotBackendMetrics(backendID)
	if !ok || m == nil || m.System.DiskFree == 0 {
		return 0, 0, false
	}
	freeMB := int64(m.System.DiskFree)
	needMB := (needBytes + (1 << 20) - 1) >> 20 // округляем вверх: половина мегабайта не влезет
	availableMB := freeMB - shareDiskReserveMB()
	if availableMB >= needMB {
		return 0, m.System.DiskFree, true
	}
	return (needMB - availableMB) << 20, m.System.DiskFree, true
}

// shareHumanBytes — размер для сообщения об ошибке (ГБ с одним знаком; меньшие
// значения — в МБ, чтобы «0.0 ГБ» не выглядело как «ничего не нужно»).
func shareHumanBytes(b int64) string {
	const gib = 1 << 30
	if b >= gib {
		return fmt.Sprintf("%.1f ГБ", float64(b)/float64(gib))
	}
	return fmt.Sprintf("%d МБ", (b+(1<<20)-1)>>20)
}

// modelShareTarget — одна цель переноса.
type modelShareTarget struct {
	BackendID  string    `json:"backendId"`
	State      string    `json:"state"`
	Bytes      int64     `json:"bytes"`
	Total      int64     `json:"total"`
	Percent    float64   `json:"percent"`
	SpeedBps   float64   `json:"speedBps"`
	StartedAt  time.Time `json:"startedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	Error      string    `json:"error,omitempty"`
	Note       string    `json:"note,omitempty"`
}

// modelShareJob — задание «перенести <model> с <source> на <targets>».
type modelShareJob struct {
	ID         string             `json:"id"`
	Source     string             `json:"source"`
	Model      string             `json:"model"`
	Kind       string             `json:"kind"` // text | image
	State      string             `json:"state"`
	Overwrite  bool               `json:"overwrite"`
	Targets    []modelShareTarget `json:"targets"`
	Error      string             `json:"error,omitempty"`
	StartedAt  time.Time          `json:"startedAt"`
	FinishedAt time.Time          `json:"finishedAt,omitempty"`

	// Filename — фактическое имя файла у источника (для текстовых моделей имя в
	// списке и на диске может отличаться регистром/суффиксом).
	Filename string `json:"filename,omitempty"`

	// mu защищает изменяемые поля от гонки между горутиной копирования и
	// HTTP-хендлером прогресса. Без него JSON-ответ мог быть «рваным»
	// (половина целей из старого состояния), а `go test -race` падал.
	mu sync.Mutex

	cancel context.CancelFunc
}

// modelShareManager — реестр заданий переноса.
type modelShareManager struct {
	mu      sync.Mutex
	jobs    map[string]*modelShareJob
	order   []string // порядок создания (для списка)
	seq     int64
	maxKept int
}

func newModelShareManager() *modelShareManager {
	return &modelShareManager{jobs: map[string]*modelShareJob{}, maxKept: 50}
}

// add — положить задание в реестр, вытеснив самые старые завершённые.
func (m *modelShareManager) add(job *modelShareJob) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	job.ID = fmt.Sprintf("share-%d-%d", time.Now().Unix(), m.seq)
	m.jobs[job.ID] = job
	m.order = append(m.order, job.ID)
	// Держим историю ограниченной: перенос — редкая операция, но задания
	// содержат состояние целей, и копить их вечно незачем.
	for len(m.order) > m.maxKept {
		oldest := m.order[0]
		m.order = m.order[1:]
		delete(m.jobs, oldest)
	}
}

func (m *modelShareManager) get(id string) *modelShareJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[id]
}

func (m *modelShareManager) list() []*modelShareJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*modelShareJob, 0, len(m.order))
	for i := len(m.order) - 1; i >= 0; i-- { // новые сверху
		if j := m.jobs[m.order[i]]; j != nil {
			out = append(out, j)
		}
	}
	return out
}

// snapshot — глубокая копия задания для JSON (без cancel-функции).
//
// Копируем цели под мьютексом: горутина копирования в этот момент обновляет
// байты/проценты, и отдавать ей же срез напрямую нельзя (гонка + рваный JSON).
func (j *modelShareJob) snapshot() map[string]interface{} {
	j.mu.Lock()
	targets := make([]modelShareTarget, len(j.Targets))
	copy(targets, j.Targets)
	id, source, model, kind, state := j.ID, j.Source, j.Model, j.Kind, j.State
	overwrite, filename, errMsg := j.Overwrite, j.Filename, j.Error
	startedAt, finishedAt := j.StartedAt, j.FinishedAt
	j.mu.Unlock()

	out := map[string]interface{}{
		"id": id, "source": source, "model": model, "kind": kind,
		"state": state, "overwrite": overwrite,
		"startedAt": startedAt.UTC().Format(time.RFC3339),
		"targets":   targets,
	}
	if filename != "" {
		out["filename"] = filename
	}
	if errMsg != "" {
		out["error"] = errMsg
	}
	if !finishedAt.IsZero() {
		out["finishedAt"] = finishedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// shareRequest — тело POST /api/v1/models/share.
type shareRequest struct {
	Source    string   `json:"source"`
	Model     string   `json:"model"`
	Targets   []string `json:"targets"`
	Overwrite bool     `json:"overwrite"`
}

// startModelShare — проверить запрос, создать задание и запустить фоновую копию.
//
// Возвращает задание сразу (HTTP отвечает 202): перенос 14 ГБ идёт минутами, и
// держать на нём HTTP-соединение клиента нельзя — прогресс клиент читает по
// GET /api/v1/models/share/{id}.
func (s *Server) startModelShare(req shareRequest) (*modelShareJob, error) {
	if s.proxy == nil {
		return nil, errors.New("proxy is not available")
	}
	src := strings.TrimSpace(req.Source)
	model := strings.TrimSpace(req.Model)
	if src == "" || model == "" {
		return nil, errors.New("source и model обязательны")
	}
	sourceBackend := s.proxy.GetBackend(src)
	if sourceBackend == nil {
		return nil, fmt.Errorf("бэкенд-источник %q не найден", src)
	}
	kind, err := shareKindOf(sourceBackend)
	if err != nil {
		return nil, err
	}
	targets := make([]string, 0, len(req.Targets))
	seen := map[string]bool{src: true}
	for _, t := range req.Targets {
		id := strings.TrimSpace(t)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		targets = append(targets, id)
	}
	if len(targets) == 0 {
		return nil, errors.New("нужен хотя бы один бэкенд-приёмник (кроме источника)")
	}

	job := &modelShareJob{
		Source: src, Model: model, Kind: kind, Overwrite: req.Overwrite,
		State: shareStateRunning, StartedAt: time.Now(),
	}
	for _, id := range targets {
		tb := s.proxy.GetBackend(id)
		if tb == nil {
			job.Targets = append(job.Targets, modelShareTarget{
				BackendID: id, State: shareTargetFailed,
				Error: fmt.Sprintf("бэкенд %q не найден", id)})
			continue
		}
		tKind, kerr := shareKindOf(tb)
		if kerr != nil || tKind != kind {
			job.Targets = append(job.Targets, modelShareTarget{
				BackendID: id, State: shareTargetFailed,
				Error: fmt.Sprintf("тип бэкенда не совпадает: %s vs %s (%s)", tKind, kind, strings.TrimSpace(errstr(kerr)))})
			continue
		}
		job.Targets = append(job.Targets, modelShareTarget{BackendID: id, State: shareTargetPending})
	}
	if !anyPending(job.Targets) {
		job.State = shareStateFailed
		job.Error = "все приёмники отклонены (нет подходящих бэкендов)"
		job.FinishedAt = time.Now()
		s.modelShare.add(job)
		return job, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	job.cancel = cancel
	s.modelShare.add(job)
	go s.runModelShare(ctx, job)
	return job, nil
}

func errstr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func anyPending(targets []modelShareTarget) bool {
	for _, t := range targets {
		if t.State == shareTargetPending {
			return true
		}
	}
	return false
}

// shareKindOf — какие файлы переносим: текстовую модель (один .gguf) или
// image-bundle (каталог). Определяется типом бэкенда: у него ровно один движок.
func shareKindOf(b *types.Backend) (string, error) {
	switch b.Type {
	case types.BackendTypeImage:
		return "image", nil
	case types.BackendTypeLlamaCpp, types.BackendTypeOllama:
		return "text", nil
	}
	return "", fmt.Errorf("бэкенд %q типа %q не поддерживает перенос моделей", b.ID, b.Type)
}

// runModelShare — фоновая копия по всем целям (последовательно).
//
// ПОСЛЕДОВАТЕЛЬНО, А НЕ ПАРАЛЛЕЛЬНО: источник читается с ОДНОГО диска, и
// параллельные потоки только рвут ему кэш; к тому же последовательный прогресс
// понятнее оператору («сейчас копирую на X, потом на Y»).
func (s *Server) runModelShare(ctx context.Context, job *modelShareJob) {
	defer func() {
		job.mu.Lock()
		job.FinishedAt = time.Now()
		switch {
		case job.State == shareStateFailed:
			// Уже проставлено (например ошибка probe источника) — финализатор
			// НЕ перетирает отказ на «done». Именно этот баг ловил тест
			// TestModelShare_API_ReportsSourceError: задание сообщало «готово»,
			// хотя модель даже не начинали копировать.
		case job.State == shareStateCanceled:
		case anyFailed(job.Targets):
			job.State = shareStatePartial
		default:
			job.State = shareStateDone
		}
		state := job.State
		job.mu.Unlock()
		if log := logger.Get(); log != nil {
			log.Infow("model share finished",
				"job", job.ID, "source", job.Source, "model", job.Model,
				"kind", job.Kind, "state", state)
		}
	}()

	// Размер источника узнаём ОДИН раз (HEAD): он одинаков для всех целей и нужен
	// прогресс-бару. Ошибка здесь означает, что переносить нечего: помечаем
	// задание и каждую цель отказом с одной и той же причиной (иначе цели
	// остались бы «pending» и UI показывал бы вечное ожидание).
	total, filename, err := s.probeShareSource(ctx, job)
	if err != nil {
		job.mu.Lock()
		job.State = shareStateFailed
		job.Error = err.Error()
		for i := range job.Targets {
			if job.Targets[i].State == shareTargetPending {
				job.Targets[i].State = shareTargetFailed
				job.Targets[i].Error = err.Error()
				job.Targets[i].FinishedAt = time.Now()
			}
		}
		job.mu.Unlock()
		return
	}
	job.mu.Lock()
	job.Filename = filename
	for i := range job.Targets {
		job.Targets[i].Total = total
	}
	job.mu.Unlock()

	for i := range job.Targets {
		if err := ctx.Err(); err != nil {
			job.mu.Lock()
			for k := range job.Targets {
				if job.Targets[k].State == shareTargetPending {
					job.Targets[k].State = shareTargetSkipped
					job.Targets[k].Note = "отменено оператором"
				}
			}
			job.State = shareStateCanceled
			job.mu.Unlock()
			return
		}
		job.mu.Lock()
		pending := job.Targets[i].State == shareTargetPending
		job.mu.Unlock()
		if !pending {
			continue
		}
		s.shareToOne(ctx, job, i)
	}
}

// shareToOne — перенос на один приёмник (по индексу в job.Targets).
func (s *Server) shareToOne(ctx context.Context, job *modelShareJob, idx int) {
	job.mu.Lock()
	t := &job.Targets[idx]
	t.State = shareTargetRunning
	t.StartedAt = time.Now()
	backendID := t.BackendID
	total := t.Total
	job.mu.Unlock()

	defer func() {
		job.mu.Lock()
		job.Targets[idx].FinishedAt = time.Now()
		job.mu.Unlock()
	}()

	// Предварительная проверка: если у приёмника модель УЖЕ есть и оператор не
	// просил перезапись — не гоняем гигабайты зря, помечаем skipped с причиной.
	if !job.Overwrite {
		if has, listErr := s.shareTargetHasModel(ctx, job, backendID); listErr == nil && has {
			job.mu.Lock()
			job.Targets[idx].State = shareTargetSkipped
			job.Targets[idx].Note = "модель уже есть на этом бэкенде (перезапись не запрошена)"
			job.Targets[idx].Percent = 100
			job.mu.Unlock()
			return
		}
	}

	// R91 (2026-10-09): проверка свободного места ДО потока — после проверки
	// «модель уже есть» (переносить нечего — это не отказ по месту) и до
	// открытия соединения с источником.
	if shortfall, freeMB, known := s.shareTargetDiskShortfall(backendID, total); known && shortfall > 0 {
		msg := fmt.Sprintf("на приёмнике мало места: нужно ~%s, свободно %s (резерв %d МБ)",
			shareHumanBytes(total), shareHumanBytes(int64(freeMB)<<20), shareDiskReserveMB())
		job.mu.Lock()
		job.Targets[idx].State = shareTargetFailed
		job.Targets[idx].Error = msg
		job.Targets[idx].Note = "перенос не начат: не хватает места на диске приёмника"
		job.mu.Unlock()
		if log := logger.Get(); log != nil {
			log.Warnw("model share target skipped: not enough disk space on receiver",
				"job", job.ID, "source", job.Source, "target", backendID, "model", job.Model,
				"need_bytes", total, "free_mb", freeMB, "shortfall_bytes", shortfall,
				"reserve_mb", shareDiskReserveMB())
		}
		return
	}

	srcURL, targetURL, err := s.shareEndpointURLs(job, backendID)
	if err != nil {
		job.mu.Lock()
		job.Targets[idx].State = shareTargetFailed
		job.Targets[idx].Error = err.Error()
		job.mu.Unlock()
		return
	}
	copied, err := s.streamModel(ctx, job, idx, srcURL, targetURL)
	job.mu.Lock()
	job.Targets[idx].Bytes = copied
	if err != nil {
		job.Targets[idx].State = shareTargetFailed
		job.Targets[idx].Error = err.Error()
		job.mu.Unlock()
		return
	}
	job.Targets[idx].State = shareTargetDone
	job.Targets[idx].Percent = 100
	job.mu.Unlock()
}

// streamModel — собственно копирование: GET источника → POST приёмнику.
//
// Тело POST'а — это тело ответа источника: ни файла на диске балансера, ни
// буфера в памяти (14 ГБ в RAM недопустимы). Go передаёт поток chunked'ом.
func (s *Server) streamModel(ctx context.Context, job *modelShareJob, idx int, srcURL, targetURL string) (int64, error) {
	srcReq, err := http.NewRequestWithContext(ctx, http.MethodGet, srcURL, nil)
	if err != nil {
		return 0, err
	}
	s.addWorkerAuth(srcReq, job.Source, job.Kind)

	resp, err := s.shareHTTPClient().Do(srcReq)
	if err != nil {
		return 0, fmt.Errorf("источник %s не отдал модель: %w", job.Source, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return 0, fmt.Errorf("источник %s вернул HTTP %d: %s", job.Source, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Полный размер — из заголовков источника (Content-Length либо X-Bundle-Bytes):
	// так прогресс верен даже если probe не сработал.
	job.mu.Lock()
	if job.Targets[idx].Total <= 0 {
		if n := headerInt64(resp.Header, "X-Bundle-Bytes"); n > 0 {
			job.Targets[idx].Total = n
		} else if resp.ContentLength > 0 {
			job.Targets[idx].Total = resp.ContentLength
		}
	}
	job.mu.Unlock()

	pr := &shareProgressReader{
		r: resp.Body, job: job, idx: idx,
		lastEmit: time.Now(), started: time.Now(),
	}
	targetReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, pr)
	if err != nil {
		return 0, err
	}
	targetReq.Header.Set("Content-Type", "application/octet-stream")
	s.addWorkerAuth(targetReq, job.Targets[idx].BackendID, job.Kind)

	targetResp, err := s.shareHTTPClient().Do(targetReq)
	if err != nil {
		return pr.n, fmt.Errorf("приёмник %s не принял поток: %w", job.Targets[idx].BackendID, err)
	}
	defer targetResp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(targetResp.Body, 8<<10))
	if targetResp.StatusCode != http.StatusOK {
		return pr.n, fmt.Errorf("приёмник %s вернул HTTP %d: %s",
			job.Targets[idx].BackendID, targetResp.StatusCode, strings.TrimSpace(string(body)))
	}
	return pr.n, nil
}

// shareProgressReader — считает переданные байты и обновляет прогресс задания.
//
// Обновляем не на каждый Read (их десятки тысяч): по байтам и по времени —
// иначе прогресс-бар дёргается, а мьютекс задания становится горячей точкой.
type shareProgressReader struct {
	r        io.Reader
	job      *modelShareJob
	idx      int
	n        int64
	lastEmit time.Time
	started  time.Time
}

func (p *shareProgressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.n += int64(n)
		now := time.Now()
		if p.n-p.job.Targets[p.idx].Bytes >= shareProgressBytes || now.Sub(p.lastEmit) >= shareProgressEvery {
			p.lastEmit = now
			p.job.mu.Lock()
			t := &p.job.Targets[p.idx]
			t.Bytes = p.n
			if t.Total > 0 {
				t.Percent = float64(p.n) / float64(t.Total) * 100
				if t.Percent > 99.5 { // финальные 100% ставим по факту ответа приёмника
					t.Percent = 99.5
				}
			}
			if elapsed := now.Sub(p.started).Seconds(); elapsed > 0.5 {
				t.SpeedBps = float64(p.n) / elapsed
			}
			p.job.mu.Unlock()
		}
	}
	return n, err
}

// probeShareSource — HEAD источника: полный размер и фактическое имя файла.
func (s *Server) probeShareSource(ctx context.Context, job *modelShareJob) (int64, string, error) {
	var path string
	switch job.Kind {
	case "image":
		path = "/api/image/models/export?name=" + urlQueryEscape(job.Model)
	default:
		path = "/api/models/export?name=" + urlQueryEscape(job.Model)
	}
	base, err := s.shareWorkerBaseURL(job.Source, job.Kind)
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, base+path, nil)
	if err != nil {
		return 0, "", err
	}
	s.addWorkerAuth(req, job.Source, job.Kind)
	resp, err := s.shareHTTPClient().Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("источник %s недоступен: %w", job.Source, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return 0, "", fmt.Errorf("на источнике %s нет модели %q: %s",
			job.Source, job.Model, strings.TrimSpace(string(body)))
	}
	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("источник %s вернул HTTP %d на запрос размера", job.Source, resp.StatusCode)
	}
	total := resp.ContentLength
	if n := headerInt64(resp.Header, "X-Bundle-Bytes"); n > 0 {
		total = n
	}
	filename := strings.TrimSpace(resp.Header.Get("X-Model-Filename"))
	return total, filename, nil
}

// shareEndpointURLs — URL источника (GET) и приёмника (POST) для этого задания.
func (s *Server) shareEndpointURLs(job *modelShareJob, targetID string) (string, string, error) {
	srcBase, err := s.shareWorkerBaseURL(job.Source, job.Kind)
	if err != nil {
		return "", "", err
	}
	dstBase, err := s.shareWorkerBaseURL(targetID, job.Kind)
	if err != nil {
		return "", "", err
	}
	if job.Kind == "image" {
		src := srcBase + "/api/image/models/export?name=" + urlQueryEscape(job.Model)
		q := "?name=" + urlQueryEscape(job.Model)
		if job.Overwrite {
			q += "&overwrite=1"
		}
		return src, dstBase + "/api/image/models/import" + q, nil
	}
	name := job.Filename
	if name == "" {
		name = job.Model
	}
	src := srcBase + "/api/models/export?name=" + urlQueryEscape(job.Model)
	q := "?filename=" + urlQueryEscape(name)
	if job.Overwrite {
		q += "&overwrite=1"
	}
	return src, dstBase + "/api/models/import" + q, nil
}

// shareWorkerBaseURL — базовый URL воркера бэкенда (по типу модели-переноса).
func (s *Server) shareWorkerBaseURL(backendID, kind string) (string, error) {
	if kind == "image" {
		_, _, base, err := s.resolveImageWorkerURL(backendID)
		return base, err
	}
	_, _, base, err := s.resolveCppWorkerURL(backendID)
	return base, err
}

// shareTargetHasModel — есть ли модель уже на приёмнике.
func (s *Server) shareTargetHasModel(ctx context.Context, job *modelShareJob, backendID string) (bool, error) {
	base, err := s.shareWorkerBaseURL(backendID, job.Kind)
	if err != nil {
		return false, err
	}
	path := "/api/models/files"
	if job.Kind == "image" {
		path = "/api/image/models"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return false, err
	}
	s.addWorkerAuth(req, backendID, job.Kind)
	resp, err := s.shareHTTPClient().Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return false, err
	}
	return shareBodyHasModel(body, job.Kind, job.Model, job.Filename), nil
}

// addWorkerAuth — токен воркера в заголовки (тем же способом, что и остальной
// сервер-серверный прокси: cpworker ждёт Bearer/X-API-Token; sdworker токен
// сейчас не проверяет, но лишний заголовок безвреден).
func (s *Server) addWorkerAuth(req *http.Request, backendID, kind string) {
	token := s.cppWorkerAPIToken(backendID)
	if kind == "image" {
		token = s.imageWorkerAPIToken(backendID)
	}
	if token == "" {
		return
	}
	req.Header.Set("X-API-Token", token)
	req.Header.Set("Authorization", "Bearer "+token)
}

// shareHTTPClient — клиент без капа времени: перенос 14 ГБ по 100 Мбит/с идёт
// ~20 минут, и любой «разумный» таймаут здесь был бы дефектом (доктрина проекта).
var shareHTTPClientOnce sync.Once
var shareHTTPClientInstance *http.Client

func (s *Server) shareHTTPClient() *http.Client {
	shareHTTPClientOnce.Do(func() {
		shareHTTPClientInstance = &http.Client{
			Timeout: 0,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          4,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: 0,
				ExpectContinueTimeout: time.Second,
			},
		}
	})
	return shareHTTPClientInstance
}

// anyFailed — есть ли провалившиеся цели.
func anyFailed(targets []modelShareTarget) bool {
	for _, t := range targets {
		if t.State == shareTargetFailed {
			return true
		}
	}
	return false
}

// headerInt64 — целочисленный заголовок или 0.
func headerInt64(h http.Header, name string) int64 {
	if v := strings.TrimSpace(h.Get(name)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// urlQueryEscape — escape для query-параметра. Имена моделей содержат точки и
// дефисы (они не требуют escape), но могут содержать и «+»/«&»/пробел — поэтому
// используем стандартный url.QueryEscape, а не ручную замену.
func urlQueryEscape(v string) string {
	return url.QueryEscape(v)
}

// shareBodyHasModel — есть ли модель в ответе инвентаря воркера.
//
// Разбираем минимально и терпимо: ответы cppworker (/api/models/files → files[].filename)
// и sdworker (/api/image/models → models[].name) отличаются, а жёсткая схема тут
// не нужна — важно лишь «есть/нет».
func shareBodyHasModel(body []byte, kind, model, filename string) bool {
	want := map[string]bool{}
	for _, v := range []string{model, filename} {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		want[v] = true
		want[strings.TrimSuffix(v, ".gguf")] = true
		want[strings.TrimSuffix(v, ".gguf")+".gguf"] = true
	}
	text := string(body)
	for name := range want {
		if strings.Contains(text, `"`+name+`"`) {
			return true
		}
	}
	return false
}

// osStatSize — размер файла или 0 (используется в тестах и диагностике).
func osStatSize(path string) int64 {
	if fi, err := os.Stat(path); err == nil {
		return fi.Size()
	}
	return 0
}

// sortedTargetIDs — детерминированный порядок целей (для тестов и логов).
func sortedTargetIDs(t []modelShareTarget) []string {
	out := make([]string, 0, len(t))
	for _, x := range t {
		out = append(out, x.BackendID)
	}
	sort.Strings(out)
	return out
}
