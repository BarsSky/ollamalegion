package sdbackend

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Джобы воркера (наша асинхронная обёртка вокруг img_gen)
// ============================================================
//
// ДВЕ РАЗНЫЕ СУЩНОСТИ, КОТОРЫЕ НЕЛЬЗЯ ПУТАТЬ:
//  1. jobID воркера (/api/image/jobs/{id}, /api/image/generate) — наш
//     идентификатор, TTL 600 с, статусы queued/processing/completed/failed/
//     cancelled;
//  2. jobID движка (/sdcpp/v1/jobs/{id}) — идентификатор внутри sd-server,
//     живёт 600 с после завершения (потом 410 Gone).
//
// Клиенту отдаём свой id: он не истечёт, пока мы держим результат, и не
// «протечёт» при рестарте sd-server (тогда джоба завершится ошибкой «движок
// остановлен», а не 410 из ниоткуда).
//
// АРХИТЕКТУРА ИСПОЛНЕНИЯ:
//
//	QueuedRunner.Queue (наша FIFO на 64 слота)
//	  ├── syncPath  — /v1/images/generations, /sdapi/v1/txt2img: ждём результат
//	  │               в самом HTTP-запросе (клиенты OpenAI/A1111 синхронны);
//	  └── asyncPath — /api/image/generate: 202 + id, клиент опрашивает
//	                  /api/image/jobs/{id}.
//
// Оба пути идут через один слот-семафор: движок всё равно сериализует
// исполнение, а «честная» очередь нужна, чтобы отдавать 429 + Retry-After
// вместо молчаливого зависания.

// Статусы джоб воркера.
const (
	JobStateQueued     = "queued"
	JobStateProcessing = "processing"
	JobStateCompleted  = "completed"
	JobStateFailed     = "failed"
	JobStateCancelled  = "cancelled"
)

// jobTTL — сколько держим завершённые джобы.
//
// 600 с — как TTL самого sd-server (completed_ttl_seconds). Согласованность
// важна: клиент, опрашивающий джобу раз в минуту, не должен упереться в «нашу»
// очистку раньше, чем в движковую.
const jobTTL = 600 * time.Second

// ErrNoModelConfigured — в реестре нет ни одной модели.
var ErrNoModelConfigured = errors.New("no image model available in models dir")

// JobRecord — состояние джобы воркера (JSON-контракт /api/image/jobs/{id}).
type JobRecord struct {
	ID        string     `json:"id"`
	State     string     `json:"state"`
	Model     string     `json:"model"`
	EngineID  string     `json:"engine_job_id,omitempty"`
	Created   time.Time  `json:"created"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	// QueuePosition — сколько джоб впереди в НАШЕЙ очереди (0 = следующая).
	QueuePosition int    `json:"queue_position"`
	Error         string `json:"error,omitempty"`
	ErrorCode     string `json:"error_code,omitempty"`
	// Images — результат: b64_json и/или url (зависит от response_format).
	Images       []JobImage `json:"images,omitempty"`
	Width        int        `json:"width"`
	Height       int        `json:"height"`
	Steps        int        `json:"steps"`
	Seed         int64      `json:"seed"`
	BatchCount   int        `json:"batch_count"`
	OutputFormat string     `json:"output_format"`
	DurationMS   int64      `json:"duration_ms,omitempty"`
	Prompt       string     `json:"prompt,omitempty"`
	NegativePrompt string   `json:"negative_prompt,omitempty"`
	// Notes — что нормализация поправила (steps/n/batch/size).
	Notes []string `json:"notes,omitempty"`
}

// JobImage — одна картинка результата.
type JobImage struct {
	Index   int    `json:"index"`
	B64JSON string `json:"b64_json,omitempty"`
	URL     string `json:"url,omitempty"`
}

// submitFunc — что должна сделать джоба: отдать запрос движку и вернуть
// engine job id. Вынесено в поле, потому что синхронные поверхности
// (A1111-путь) идут не через img_gen — единый реестр джоб, разные исполнители.
type submitFunc func(ctx context.Context, engine *SDServerClient) (engineJobID string, err error)

type jobEntry struct {
	rec    JobRecord
	engine string
	submit submitFunc
	// urlMode — клиент просил response_format:"url" (сохраняем PNG и отдаём ссылку).
	urlMode bool
	// cancelled — отмена ДО отправки в движок (прерывать нечего).
	cancelled bool
	done      chan struct{}
}

// JobRunner — очередь + реестр джоб воркера.
type JobRunner struct {
	cfg      *Config
	registry *Registry
	sup      *Supervisor
	metrics  *Metrics
	queue    *Queue
	store    *ImageStore

	mu      sync.Mutex
	jobs    map[string]*jobEntry
	inQueue []string // FIFO постановки (для queue_position)
}

// NewJobRunner создаёт раннер джоб.
func NewJobRunner(cfg *Config, registry *Registry, sup *Supervisor, metrics *Metrics, store *ImageStore) *JobRunner {
	return &JobRunner{
		cfg:      cfg,
		registry: registry,
		sup:      sup,
		metrics:  metrics,
		queue:    NewQueue(cfg.MaxConcurrent),
		store:    store,
		jobs:     map[string]*jobEntry{},
	}
}

// Queue — доступ к очереди (метрики/health).
func (r *JobRunner) Queue() *Queue { return r.queue }

// NewJobID — идентификатор джобы воркера.
func NewJobID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("img_%d", time.Now().UnixNano())
	}
	return "img_" + hex.EncodeToString(b[:])
}

// ============================================================
// Общий исполнитель
// ============================================================

// GenerationResultView — публичное представление результата генерации для
// HTTP-слоя (cmd/sdworker не должен знать про внутренний generationResult).
type GenerationResultView = generationResult

// generationResult — результат одной генерации (до сериализации в API-форму).
type generationResult struct {
	Images       []JobImage
	OutputFormat string
	Seed         int64
	Width        int
	Height       int
	Steps        int
	BatchCount   int
	Duration     time.Duration
}

// runGeneration — единая точка: ensure loaded → submit → poll → materialize.
//
// injectSeedInPrompt — TRUE только для синхронных клиентских поверхностей
// (OpenAI/A1111), где движок seed не читает.
func (r *JobRunner) runGeneration(ctx context.Context, model string, urlMode, injectSeedInPrompt bool, n GenerationRequest) (*generationResult, error) {
	if model == "" {
		model = r.defaultModel()
	}
	if model == "" {
		return nil, ErrNoModelConfigured
	}
	r.sup.BeginRequest()
	defer r.sup.EndRequest()

	if err := r.ensureLoaded(ctx, model); err != nil {
		r.recordFailure(err)
		return nil, err
	}
	client := r.sup.Client()
	if client == nil {
		r.recordFailure(ErrNotLoaded)
		return nil, ErrNotLoaded
	}

	engineReq := BuildImgGenRequest(n, injectSeedInPrompt)
	start := time.Now()
	sub, err := client.SubmitImgGen(ctx, engineReq)
	if err != nil {
		r.recordFailure(err)
		return nil, err
	}
	job, err := client.WaitJob(ctx, sub.ID, r.generationTimeout(model), r.pollEvery())
	if err != nil {
		r.recordFailure(err)
		return nil, err
	}
	if job.Status == JobStatusFailed || job.Status == JobStatusCancelled {
		msg, code := jobErrorText(job)
		r.recordFailure(errors.New(msg))
		return nil, &EngineJobError{Status: job.Status, Code: code, Message: msg}
	}
	if job.Result == nil || len(job.Result.Images) == 0 {
		err := errors.New("sd-server returned no images")
		r.recordFailure(err)
		return nil, err
	}

	images := make([]JobImage, 0, len(job.Result.Images))
	for _, img := range job.Result.Images {
		out := JobImage{Index: img.Index, B64JSON: img.B64JSON}
		if urlMode {
			url, serr := r.store.Save(img.B64JSON, job.Result.OutputFormat)
			if serr != nil {
				r.recordFailure(serr)
				return nil, serr
			}
			out.URL = url
		}
		images = append(images, out)
	}
	res := &generationResult{
		Images:       images,
		OutputFormat: firstNonEmpty(job.Result.OutputFormat, n.OutputFormat),
		Seed:         n.Seed,
		Width:        n.Width,
		Height:       n.Height,
		Steps:        n.Steps,
		BatchCount:   n.BatchCount,
		Duration:     time.Since(start),
	}
	if r.metrics != nil {
		r.metrics.ObserveGeneration(res.Duration, len(images), nil)
	}
	sdLog().Infow("image generation completed",
		"model", model, "images", len(images), "seed", n.Seed,
		"size", fmt.Sprintf("%dx%d", n.Width, n.Height), "steps", n.Steps,
		"duration_ms", res.Duration.Milliseconds(), "engine_job", sub.ID)
	return res, nil
}

// recordFailure — единый учёт неуспеха (метрики + лог).
func (r *JobRunner) recordFailure(err error) {
	if r.metrics != nil {
		r.metrics.ObserveGeneration(0, 0, err)
		r.metrics.MarkError(err)
	}
}

// GenerateSync — синхронная генерация (OpenAI/A1111-поверхности).
//
// ctx НЕ пробрасывается в исполнение после захвата слота: клиент может
// отвалиться (закрыл вкладку браузера), но мы доводим генерацию до конца —
// иначе VRAM остаётся занята «ничьей» работой, а повтор начнёт её заново.
// Для этого используем отдельный background-контекст, а ctx — только на
// этапе ожидания слота (там отмена безопасна).
func (r *JobRunner) GenerateSync(ctx context.Context, model string, urlMode, injectSeedInPrompt bool, n GenerationRequest) (*generationResult, error) {
	release, err := r.queue.Acquire(ctx)
	if err != nil {
		if r.metrics != nil {
			r.metrics.QueueRejections.Add(1)
		}
		return nil, err
	}
	defer release()
	return r.runGeneration(context.Background(), model, urlMode, injectSeedInPrompt, n)
}

// ============================================================
// Асинхронные джобы (/api/image/generate)
// ============================================================

// Submit — ставит джобу в очередь и запускает её в горутине.
func (r *JobRunner) Submit(ctx context.Context, model string, urlMode, injectSeedInPrompt bool, n GenerationRequest) (*JobRecord, error) {
	if model == "" {
		model = r.defaultModel()
	}
	if model == "" {
		return nil, ErrNoModelConfigured
	}

	entry := &jobEntry{
		rec: JobRecord{
			ID:             NewJobID(),
			State:          JobStateQueued,
			Model:          model,
			Created:        time.Now().UTC(),
			Width:          n.Width,
			Height:         n.Height,
			Steps:          n.Steps,
			Seed:           n.Seed,
			BatchCount:    n.BatchCount,
			OutputFormat:  n.OutputFormat,
			Prompt:         n.Prompt,
			NegativePrompt: n.NegativePrompt,
			Notes:          n.Notes,
		},
		urlMode: urlMode,
		done:    make(chan struct{}),
	}

	// Слот берём ДО регистрации джобы: иначе при переполнении пришлось бы
	// удалять уже видимую клиенту запись, а клиент успел бы её увидеть в
	// состоянии queued с «замороженной» позицией.
	release, err := r.queue.Acquire(ctx)
	if err != nil {
		if r.metrics != nil {
			r.metrics.QueueRejections.Add(1)
		}
		return nil, err
	}

	r.mu.Lock()
	r.jobs[entry.rec.ID] = entry
	r.inQueue = append(r.inQueue, entry.rec.ID)
	entry.rec.QueuePosition = r.positionLocked(entry.rec.ID)
	rec := entry.rec
	r.mu.Unlock()

	go r.runAsync(entry, release, injectSeedInPrompt, n)
	return &rec, nil
}

// runAsync — исполнение асинхронной джобы.
func (r *JobRunner) runAsync(entry *jobEntry, release func(), injectSeedInPrompt bool, n GenerationRequest) {
	defer release()
	defer close(entry.done)

	r.mu.Lock()
	if entry.cancelled {
		r.mu.Unlock()
		return
	}
	entry.rec.State = JobStateProcessing
	now := time.Now().UTC()
	entry.rec.StartedAt = &now
	r.removeFromQueueLocked(entry.rec.ID)
	r.mu.Unlock()

	model := entry.rec.Model
	res, err := r.runGeneration(context.Background(), model, entry.urlMode, injectSeedInPrompt, n)
	r.finalize(entry, res, err)
}

// finalize — терминальное состояние асинхронной джобы.
func (r *JobRunner) finalize(entry *jobEntry, res *generationResult, err error) {
	end := time.Now().UTC()
	r.mu.Lock()
	entry.rec.EndedAt = &end
	if entry.rec.StartedAt != nil {
		entry.rec.DurationMS = end.Sub(*entry.rec.StartedAt).Milliseconds()
	}
	if err != nil {
		entry.rec.State = JobStateFailed
		entry.rec.Error = err.Error()
		entry.rec.ErrorCode = errorCodeOf(err)
	} else {
		entry.rec.State = JobStateCompleted
		entry.rec.Images = res.Images
		entry.rec.OutputFormat = res.OutputFormat
		entry.rec.Seed = res.Seed
	}
	rec := entry.rec
	r.mu.Unlock()

	sdLog().Infow("async image job finished",
		"job", rec.ID, "state", rec.State, "model", rec.Model,
		"images", len(rec.Images), "duration_ms", rec.DurationMS, "error", rec.Error)
	go r.collect(entry.rec.ID, jobTTL)
}

// Wait — ожидание терминального состояния джобы.
func (r *JobRunner) Wait(ctx context.Context, id string) (*JobRecord, error) {
	r.mu.Lock()
	entry, ok := r.jobs[id]
	r.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrJobNotFound, id)
	}
	select {
	case <-entry.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	rec := r.Get(id)
	if rec == nil {
		return nil, fmt.Errorf("%w: %s", ErrJobNotFound, id)
	}
	return rec, nil
}

// Get — снимок джобы.
func (r *JobRunner) Get(id string) *JobRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.jobs[id]
	if !ok {
		return nil
	}
	rec := entry.rec
	rec.QueuePosition = r.positionLocked(id)
	return &rec
}

// Cancel — отмена джобы.
//
// ЧЕСТНО: отменяем только то, что ещё не ушло в движок (state=queued). Для
// джобы, взятой движком в работу, sd-server отвечает 409 «cannot be
// interrupted yet» — мы это НЕ маскируем и не обещаем mid-flight cancel
// (features_by_mode.img_gen.cancel_generating == false).
func (r *JobRunner) Cancel(id string) error {
	r.mu.Lock()
	entry, ok := r.jobs[id]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrJobNotFound, id)
	}
	switch entry.rec.State {
	case JobStateQueued:		entry.cancelled = true
		now := time.Now().UTC()
		entry.rec.State = JobStateCancelled
		entry.rec.EndedAt = &now
		entry.rec.Error = "job cancelled by client"
		entry.rec.ErrorCode = "cancelled"
		r.removeFromQueueLocked(id)
		engineID := entry.engine
		// Асинхронная джоба держит слот до конца runAsync; закрывать done
		// здесь нельзя — иначе Wait вернётся, а release произойдёт позже.
		// Поэтому: если джоба ещё даже не стартовала (слот взят, горутина не
		// дошла), просто помечаем cancelled — runAsync сам выйдет.
		r.mu.Unlock()
		if engineID != "" {
			return nil
		}
		return nil
	case JobStateProcessing:
		engineID := entry.engine
		r.mu.Unlock()
		if engineID == "" {
			// Наш этап «processing», но движок ещё не получил запрос
			// (идёт загрузка модели) — прерывать нечем.
			return ErrCancelGenerating
		}
		client := r.sup.Client()
		if client == nil {
			return ErrCancelGenerating
		}
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if cerr := client.CancelJob(cctx, engineID); cerr != nil {
			// *UpstreamError 409/410 — пробрасываем КАК ЕСТЬ (через cause):
			// HTTP-слой обязан отдать клиенту 409 «cannot be interrupted»,
			// а не 502/500.
			return &EngineJobError{
				Status: JobStatusGenerating, Code: errorCodeOf(cerr),
				Message: cerr.Error(), cause: cerr,
			}
		}
		return nil
	default:
		r.mu.Unlock()
		return ErrCancelNotAllowed
	}
}

// registerJobInternal — регистрация записи джобы (внутренняя; инвариант
// «engine id всегда в обоих полях» держится здесь, чтобы отмена не зависела от
// того, какое из двух полей успели заполнить).
func (r *JobRunner) registerJobInternal(entry *jobEntry, engineID string) {
	entry.engine = engineID
	entry.rec.EngineID = engineID
	r.mu.Lock()
	r.jobs[entry.rec.ID] = entry
	r.mu.Unlock()
}

// List — все живые джобы (диагностика/UI).
func (r *JobRunner) List() []JobRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]JobRecord, 0, len(r.jobs))
	for id, e := range r.jobs {
		rec := e.rec
		rec.QueuePosition = r.positionLocked(id)
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// collect — удаление джобы по истечении TTL.
func (r *JobRunner) collect(id string, ttl time.Duration) {
	time.Sleep(ttl)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.jobs[id]; ok {
		delete(r.jobs, id)
	}
}

// ============================================================
// Вспомогательное
// ============================================================

// ensureLoaded — модель должна быть загружена.
//
// Почему автозагрузка здесь: без процесса sd-server генерация невозможна, а
// балансер может и не успеть вызвать /api/image/models/load. Повторный Load
// идемпотентен, параллельные запросы схлопываются single-flight внутри
// супервизора.
func (r *JobRunner) ensureLoaded(ctx context.Context, model string) error {
	if r.sup.CurrentModel() == model && r.sup.State() == StateLoaded {
		return nil
	}
	if _, ok := r.registry.Profile(model); !ok {
		// Модель не найдена: OpenAI-клиенты шлют dall-e-2/sd-cpp-local, которых
		// в реестре нет. Если модель в реестре ровно одна — используем её.
		if only := r.singleModel(); only != "" {
			model = only
		} else {
			return fmt.Errorf("%w: %q", ErrModelNotFound, model)
		}
	}
	if r.sup.State() == StateLoading {
		return fmt.Errorf("%w: %s", ErrLoading, model)
	}
	_, err := r.sup.Load(ctx, model, LoadOptions{})
	return err
}

// singleModel — единственная известная модель (или "").
func (r *JobRunner) singleModel() string {
	names := r.registry.Names()
	if len(names) == 1 {
		return names[0]
	}
	return ""
}

// defaultModel — модель по умолчанию для запроса без поля model.
//
// Приоритет: уже загруженная → предзагруженная конфигом → единственная в
// реестре. Пустая строка = явная ошибка ErrNoModelConfigured.
func (r *JobRunner) defaultModel() string {
	if cur := r.sup.CurrentModel(); cur != "" {
		return cur
	}
	if r.cfg.PreloadModel != "" {
		return r.cfg.PreloadModel
	}
	if names := r.registry.Names(); len(names) == 1 {
		return names[0]
	}
	return ""
}

// generationTimeout — таймаут ожидания для модели.
func (r *JobRunner) generationTimeout(model string) time.Duration {
	if p, ok := r.registry.Profile(model); ok && p.TimeoutSec > 0 {
		return time.Duration(p.TimeoutSec) * time.Second
	}
	if r.cfg.GenerationTimeoutSec > 0 {
		return time.Duration(r.cfg.GenerationTimeoutSec) * time.Second
	}
	return 0 // 0 = без таймаута (WaitJob ждёт терминального статуса)
}

// pollEvery — интервал опроса статуса джобы.
func (r *JobRunner) pollEvery() time.Duration {
	if r.sup.PollEvery > 0 {
		return r.sup.PollEvery
	}
	return 500 * time.Millisecond
}

// positionLocked — позиция джобы в нашей очереди.
func (r *JobRunner) positionLocked(id string) int {
	for i, queued := range r.inQueue {
		if queued == id {
			return i
		}
	}
	return 0
}

func (r *JobRunner) removeFromQueueLocked(id string) {
	for i, queued := range r.inQueue {
		if queued == id {
			r.inQueue = append(r.inQueue[:i], r.inQueue[i+1:]...)
			return
		}
	}
}

// EngineJobError — терминальный не-успех джобы движка.
//
// Unwrap() нужен, чтобы ошибка отмены (409 от sd-server) не теряла свой
// HTTP-статус: HTTP-слой обязан пробросить клиенту ровно 409 «cannot be
// interrupted», а не превратить его в 502.
type EngineJobError struct {
	Status  string
	Code    string
	Message string
	cause   error
}

func (e *EngineJobError) Unwrap() error { return e.cause }

func (e *EngineJobError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("sd-server job %s (%s): %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("sd-server job %s: %s", e.Status, e.Message)
}

// jobErrorText — текст/код ошибки движка.
func jobErrorText(job *Job) (string, string) {
	if job == nil {
		return "sd-server returned no job", "empty_job"
	}
	if job.Error != nil {
		code := job.Error.Code
		if code == "" {
			code = "generation_failed"
		}
		msg := job.Error.Message
		if msg == "" {
			msg = "sd-server reported " + job.Status
		}
		return msg, code
	}
	return "sd-server reported status " + job.Status, job.Status
}

// ErrorCodeOf — машинный код ошибки (публичная обёртка для HTTP-слоя).
func ErrorCodeOf(err error) string { return errorCodeOf(err) }

// errorCodeOf — машинный код ошибки для ответа клиенту.
func errorCodeOf(err error) string {
	switch {
	case errors.Is(err, ErrTimeout):
		return "generation_timeout"
	case errors.Is(err, ErrNotLoaded):
		return "model_not_loaded"
	case errors.Is(err, ErrLoading):
		return "model_loading"
	case errors.Is(err, ErrModelNotFound), errors.Is(err, ErrNoModelConfigured):
		return "model_not_found"
	case errors.Is(err, ErrQueueFull):
		return "queue_full"
	case errors.Is(err, ErrCancelGenerating):
		return "cannot_cancel_generating"
	case errors.Is(err, ErrCancelNotAllowed):
		return "job_already_finished"
	case errors.Is(err, ErrJobNotFound):
		return "job_not_found"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	// UpstreamError проверяем ДО EngineJobError: отмена generating-джобы
	// приходит именно как UpstreamError(409) и обязана сохранить свой статус.
	if ue, ok := AsUpstreamError(err); ok {
		switch ue.StatusCode {
		case 429:
			return "engine_queue_full"
		case 400:
			return "invalid_generation_params"
		case 404:
			return "job_not_found"
		case 409:
			return "cannot_cancel_generating"
		case 410:
			return "job_gone"
		}
		return "engine_error"
	}
	var eje *EngineJobError
	if errors.As(err, &eje) {
		if eje.Code != "" {
			return eje.Code
		}
		return "generation_failed"
	}
	var fi *FlagIncompatibleError
	if errors.As(err, &fi) {
		return "sd_server_incompatible"
	}
	var se *StartupError
	if errors.As(err, &se) {
		return "sd_server_startup_failed"
	}
	return "generation_failed"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ProfileFor — профиль модели для нормализации (nil, если модели нет).
func (r *JobRunner) ProfileFor(model string) *types.ImageModelProfile {
	if p, ok := r.registry.Profile(model); ok {
		return &p
	}
	return nil
}

// Visible — для тестов/диагностики: число записей в реестре джоб.
func (r *JobRunner) Visible() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.jobs)
}
