// jobs.go — задачи ре-экспорта: очередь, состояние, отмена, хранение.
//
// ПОЧЕМУ БЕЗ БД. Задачи — это единицы работы на одном узле (модельный том один),
// и их количество измеряется десятками. Файл на задачу в томе `REEXPORT_JOBS_DIR`
// даёт ровно то, что нужно: состояние переживает перезапуск контейнера, человек
// читает его `cat`, а сервис не тащит зависимость на СУБД ради таблицы из
// тридцати строк. Список собирается чтением каталога, поэтому «потерянная» задача
// видна как файл, а не как расхождение двух хранилищ.
//
// ПАРАЛЛЕЛИЗМ 1. Ре-экспорт — это чтение и запись десятков гигабайт на одном
// диске моделей. Две одновременные задачи не ускоряются (диск один), зато делят
// кэш страниц и вдвое увеличивают пик свободного места. Поэтому ровно один
// исполнитель, остальные стоят в очереди со статусом `queued` — оператор видит
// ожидание, а не «ничего не происходит».
//
// ЧТО ЗНАЧИТ «ПЕРЕЗАПУСК КОНТЕЙНЕРА». Задача, которая была `running`, после
// старта становится `interrupted` с объяснением: дочерний процесс python умер
// вместе с контейнером, и делать вид, что она продолжается, нельзя. Частичный
// файл при этом остаётся на диске — его показываем в ошибке, а не удаляем молча
// (решение «дописать или перезапустить» за оператором).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"ollama-loadbalancer/pkg/version"
)

// Статусы задачи. Строки (а не enum) — потому что это же значение уходит в JSON
// и его читает WebUI: одно слово в API, в файле состояния и в логе.
const (
	statusQueued      = "queued"
	statusRunning     = "running"
	statusSucceeded   = "succeeded"
	statusFailed      = "failed"
	statusCanceled    = "canceled"
	statusInterrupted = "interrupted"
)

// jobOptions — параметры задачи из тела POST /api/reexport/jobs.
type jobOptions struct {
	// OutPath — куда положить результат. Пусто = рядом с источником.
	OutPath string `json:"out_path,omitempty"`
	// FuseLoopQueries — разложить глобальные матрицы запросов по блокам ядра
	// (нужно loop-схеме: загрузчик ждёт blk.N.loop_msg_query.weight).
	FuseLoopQueries bool `json:"fuse_loop_queries,omitempty"`
	// DryRun — прочитать файл и показать план, ничего не записывая.
	DryRun bool `json:"dry_run,omitempty"`
	// Force — перезаписать существующий результат.
	Force bool `json:"force,omitempty"`
	// Verify — сверять результат после записи (по умолчанию да).
	Verify *bool `json:"verify,omitempty"`
	// OutType — тип квантования/выхода для цели hf_to_gguf (q8_0, f16, ...).
	OutType string `json:"outtype,omitempty"`
}

func (o jobOptions) verifyEnabled() bool { return o.Verify == nil || *o.Verify }

// jobProgress — прогресс, который видит оператор.
type jobProgress struct {
	// Phase — короткая метка стадии для WebUI (queued/start/plan/copy/verify/done).
	Phase string `json:"phase,omitempty"`
	// Stage — стадия конвейера из контракта (reading/writing/validating): по ней
	// видно, что задача не «зависла», а перешла к записи или сверке. Метка и
	// стадия не дублируют друг друга: метка — слово для человека, стадия —
	// значение, на которое можно смотреть кодом (и которое не меняется вместе
	// с переводами интерфейса).
	Stage      string  `json:"stage,omitempty"`
	Percent    float64 `json:"percent"`
	BytesDone  int64   `json:"bytes_done,omitempty"`
	BytesTotal int64   `json:"bytes_total,omitempty"`
}

// jobResult — итог: числа, а не пересказ.
type jobResult struct {
	DurationMS int64                  `json:"duration_ms"`
	SrcBytes   int64                  `json:"src_bytes,omitempty"`
	DstBytes   int64                  `json:"dst_bytes,omitempty"`
	Facts      map[string]interface{} `json:"facts,omitempty"`
	Changes    []string               `json:"changes,omitempty"`
	Verify     map[string]interface{} `json:"verify,omitempty"`
}

// jobError — ошибка с машинным кодом и человеческим объяснением.
//
// Код нужен WebUI (показать причину одним словом и подсказать действие), текст —
// оператору. «HTTP 500» без причины — это то, чего здесь быть не должно.
type jobError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

func (e *jobError) Error() string { return e.Message }

// jobView — сериализуемое состояние задачи (оно же файл в REEXPORT_JOBS_DIR).
type jobView struct {
	ID         string      `json:"id"`
	Status     string      `json:"status"`
	Target     string      `json:"target"`
	ModelDir   string      `json:"model_dir"`
	SrcPath    string      `json:"src_path,omitempty"`
	OutPath    string      `json:"out_path,omitempty"`
	Options    jobOptions  `json:"options"`
	CreatedAt  time.Time   `json:"created_at"`
	StartedAt  *time.Time  `json:"started_at,omitempty"`
	FinishedAt *time.Time  `json:"finished_at,omitempty"`
	Progress   jobProgress `json:"progress"`
	Result     *jobResult  `json:"result,omitempty"`
	Error      *jobError   `json:"error,omitempty"`
	LogTail    []string    `json:"log_tail,omitempty"`
}

// job — задача вместе с рантайм-состоянием (в файл не попадает).
type job struct {
	jobView
	mu     sync.Mutex
	cancel context.CancelFunc
	log    *ringLog
}

// ringLog — последние N строк вывода конвертера. Полный лог не нужен: оператор
// смотрит причину, а не историю; для истории есть `docker logs`.
type ringLog struct {
	max   int
	lines []string
}

func newRingLog(max int) *ringLog { return &ringLog{max: max} }

func (r *ringLog) add(line string) {
	line = strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(line) == "" {
		return
	}
	if len(r.lines) >= r.max {
		r.lines = append(r.lines[1:], line)
		return
	}
	r.lines = append(r.lines, line)
}

func (r *ringLog) snapshot() []string {
	if len(r.lines) == 0 {
		return nil
	}
	out := make([]string, len(r.lines))
	copy(out, r.lines)
	return out
}

// createRequest — нормализованный запрос на создание задачи.
type createRequest struct {
	ModelDir string     `json:"model_dir"`
	Target   string     `json:"target"`
	Options  jobOptions `json:"options"`
}

// Manager — очередь задач. Один исполнитель (см. шапку файла).
type Manager struct {
	cfg    config
	runner jobRunner
	log    *zap.SugaredLogger

	mu         sync.Mutex
	jobs       map[string]*job
	order      []string
	queue      chan string
	progressAt map[string]time.Time

	now   func() time.Time
	genID func() string
	// freeSpace — свободное место тома для каталога (шов для тестов: на машине
	// разработчика свободного места заведомо больше любого синтетического входа,
	// и «мало места» иначе не воспроизвести).
	freeSpace func(path string) (int64, error)
}

func newManager(cfg config, runner jobRunner, log *zap.SugaredLogger) *Manager {
	m := &Manager{
		cfg:        cfg,
		runner:     runner,
		log:        log,
		jobs:       map[string]*job{},
		queue:      make(chan string, 64),
		progressAt: map[string]time.Time{},
		freeSpace:  freeSpaceBytes,
		now:        time.Now,
		genID: func() string {
			return fmt.Sprintf("rj-%s-%04d", time.Now().UTC().Format("20060102-150405"), time.Now().Nanosecond()/1e6)
		},
	}
	return m
}

// Load восстанавливает состояние с диска и запускает исполнителя.
//
// ЗАЧЕМ ЧИТАТЬ ФАЙЛЫ, А НЕ ДОВЕРЯТЬ ПАМЯТИ. Контейнер перезапускают (обновление
// образа, правка .env) — и оператор обязан увидеть, что стало с задачей, а не
// пустой список, из которого следует «ничего и не запускали».
func (m *Manager) Load(ctx context.Context) error {
	if err := os.MkdirAll(m.cfg.JobsDir, 0o755); err != nil {
		return fmt.Errorf("каталог задач %s: %w", m.cfg.JobsDir, err)
	}
	entries, err := os.ReadDir(m.cfg.JobsDir)
	if err != nil {
		return fmt.Errorf("чтение каталога задач %s: %w", m.cfg.JobsDir, err)
	}
	loaded := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(m.cfg.JobsDir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			m.log.Warnw("job state unreadable", "path", path, "error", err)
			continue
		}
		var v jobView
		if err := json.Unmarshal(data, &v); err != nil {
			// Битый файл не должен блокировать сервис: он не задача, а мусор.
			m.log.Warnw("job state malformed", "path", path, "error", err)
			continue
		}
		if v.ID == "" {
			continue
		}
		j := &job{jobView: v, log: newRingLog(40)}
		if j.Status == statusRunning || j.Status == statusQueued {
			j.Status = statusInterrupted
			now := m.now().UTC()
			j.FinishedAt = &now
			j.Error = &jobError{
				Code: "interrupted",
				Message: "сервис ре-экспорта был перезапущен, задача не завершена; " +
					"частичный файл мог остаться на диске — запустите задачу снова",
			}
			m.persist(j)
		}
		m.jobs[j.ID] = j
		m.order = append(m.order, j.ID)
		loaded++
	}
	m.log.Infow("job state loaded", "jobs", loaded, "dir", m.cfg.JobsDir)
	go m.worker(ctx)
	return nil
}

// Create проверяет запрос, готовит пути и ставит задачу в очередь.
func (m *Manager) Create(req createRequest) (*jobView, error) {
	spec, ok := targetByID(req.Target)
	if !ok {
		return nil, &jobError{Code: "unknown_target", Message: fmt.Sprintf(
			"неизвестная цель ре-экспорта %q; поддерживаются: %s", req.Target, strings.Join(targetIDs(), ", "))}
	}
	if !spec.Available {
		msg := spec.Reason
		if msg == "" {
			msg = "цель недоступна в этой сборке образа"
		}
		return nil, &jobError{Code: "target_unavailable", Message: msg}
	}
	if strings.TrimSpace(req.ModelDir) == "" {
		return nil, &jobError{Code: "invalid_request", Message: "не указан model_dir (файл или каталог модели в томе моделей)"}
	}
	src, err := m.resolveSource(req.ModelDir)
	if err != nil {
		return nil, err
	}
	outPath, err := m.resolveOutput(src, req.Target, req.Options)
	if err != nil {
		return nil, err
	}
	if !req.Options.DryRun {
		if _, err := os.Stat(outPath); err == nil && !req.Options.Force {
			return nil, &jobError{Code: "dst_exists",
				Message: fmt.Sprintf("результат уже существует: %s (включите force, чтобы перезаписать)", outPath)}
		}
		if dir := filepath.Dir(outPath); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, &jobError{Code: "dst_unwritable",
					Message: fmt.Sprintf("каталог результата %s недоступен для записи: %v", dir, err)}
			}
		}
	}

	id := m.genID()
	m.mu.Lock()
	for m.jobs[id] != nil {
		id = m.genID() + "-b"
	}
	j := &jobView{
		ID:        id,
		Status:    statusQueued,
		Target:    req.Target,
		ModelDir:  req.ModelDir,
		SrcPath:   src,
		OutPath:   outPath,
		Options:   req.Options,
		CreatedAt: m.now().UTC(),
		Progress:  jobProgress{Phase: "queued"},
	}
	m.jobs[id] = &job{jobView: *j, log: newRingLog(40)}
	m.order = append(m.order, id)
	m.persistLocked(id)
	m.mu.Unlock()

	select {
	case m.queue <- id:
	default:
		m.setStatus(id, statusFailed, &jobError{Code: "queue_full",
			Message: "очередь задач переполнена (64) — дождитесь завершения текущих"})
		return nil, &jobError{Code: "queue_full", Message: "очередь задач переполнена"}
	}
	m.log.Infow("job created", "id", id, "target", req.Target, "model", src, "out", outPath,
		"dry_run", req.Options.DryRun)
	v := m.snapshot(id)
	return &v, nil
}

// resolveSource приводит model_dir к абсолютному пути ВНУТРИ тома моделей.
//
// Отдельно проверяется выход за корень: сервис работает с моделью, которую
// принёс оператор, и не должен читать произвольные файлы хоста по запросу из UI.
func (m *Manager) resolveSource(modelDir string) (string, error) {
	in := strings.TrimSpace(modelDir)
	candidates := []string{in}
	if !filepath.IsAbs(in) {
		candidates = []string{filepath.Join(m.cfg.ModelsDir, in)}
	}
	for _, cand := range candidates {
		abs, err := filepath.Abs(cand)
		if err != nil {
			continue
		}
		clean := filepath.Clean(abs)
		if !pathWithin(clean, m.cfg.ModelsDir) {
			return "", &jobError{Code: "out_of_models_dir", Message: fmt.Sprintf(
				"путь %q вне тома моделей %s", modelDir, m.cfg.ModelsDir)}
		}
		st, err := os.Stat(clean)
		if err != nil {
			continue
		}
		if st.IsDir() {
			// Каталог допустим только как HF-чекпойнт для полного ре-экспорта.
			if !fileExists(filepath.Join(clean, "config.json")) {
				return "", &jobError{Code: "model_not_found", Message: fmt.Sprintf(
					"%s — каталог без config.json: это не GGUF-файл и не HF-чекпойнт", clean)}
			}
			return clean, nil
		}
		return clean, nil
	}
	return "", &jobError{Code: "model_not_found", Message: fmt.Sprintf(
		"модель %q не найдена в томе моделей %s", modelDir, m.cfg.ModelsDir)}
}

// resolveOutput выбирает путь результата: явный out_path, иначе рядом с моделью.
//
// ВЫХОД ВСЕГДА ВНУТРИ КАТАЛОГА МОДЕЛИ. Раньше каталог результата выбирала
// глобальная REEXPORT_OUT_DIR, и одна и та же задача могла лечь куда угодно —
// в том числе за пределы тома моделей, где воркер её не найдёт. Теперь:
//
//   - по умолчанию — рядом с моделью: <каталог модели>/<имя>.<цель>.gguf;
//   - options.out_path — имя/подкаталог ВНУТРИ каталога модели (то, что план
//     называет «target.dir») — задача может выбрать подкаталог, но не соседний
//     том и не каталог модели другого человека;
//   - абсолютный out_path — разрешён только внутри ${MODELS_DIR} (явный выбор
//     оператора в пределах того же тома, который видит воркер);
//   - REEXPORT_OUT_DIR — только legacy-fallback (config.legacyOutBase) и никогда
//     не даёт выйти за ${MODELS_DIR}.
//
// Проверка отношения путей — filepath.Rel (как componentBundleDir в R710), а не
// «префикс строки»: иначе /models2 прошёл бы проверку на /models.
func (m *Manager) resolveOutput(src, target string, opts jobOptions) (string, error) {
	modelDir := filepath.Dir(filepath.Clean(src))
	requested := strings.TrimSpace(opts.OutPath)

	var cand string
	switch {
	case requested == "":
		cand = filepath.Join(modelDir, defaultResultName(src, target))
		// Legacy-fallback: если каталог модели оказался вне тома моделей (старая
		// раскладка), результат кладём в прежний глобальный каталог — но только
		// если он сам внутри ${MODELS_DIR}. Иначе путь отклоняется ниже.
		if !pathWithin(cand, m.cfg.ModelsDir) {
			if legacy := m.cfg.legacyOutBase(); legacy != "" {
				cand = filepath.Join(legacy, filepath.Base(cand))
			}
		}
	case filepath.IsAbs(requested):
		cand = filepath.Clean(requested)
	default:
		cand = filepath.Join(modelDir, requested)
		if !pathWithin(cand, modelDir) {
			return "", &jobError{Code: "path_not_allowed", Message: fmt.Sprintf(
				"каталог результата %q вне каталога модели %s: out_path — имя или подкаталог внутри каталога модели",
				requested, modelDir)}
		}
	}

	abs, err := filepath.Abs(cand)
	if err != nil {
		return "", &jobError{Code: "invalid_request", Message: fmt.Sprintf("путь результата %q: %v", cand, err)}
	}
	abs = filepath.Clean(abs)
	if pathWithin(abs, src) {
		return "", &jobError{Code: "invalid_request", Message: "результат совпадает с источником: ре-экспорт пишет новый файл, исходный не изменяется"}
	}
	if !pathWithin(abs, m.cfg.ModelsDir) {
		return "", &jobError{Code: "path_not_allowed", Message: fmt.Sprintf(
			"путь результата %q вне тома моделей %s: результат обязан быть виден воркеру (%s)",
			abs, m.cfg.ModelsDir, m.outRootHint())}
	}
	return abs, nil
}

// defaultResultName — имя результата по умолчанию: <модель>.<цель>.gguf.
func defaultResultName(src, target string) string {
	base := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	if isDir(src) {
		base = filepath.Base(src)
	}
	return base + "." + target + ".gguf"
}

// outRootHint — куда ещё можно писать, если основной корень не подошёл.
//
// Отдельно про legacy-переменную: если она задана, но лежит вне тома моделей,
// оператор обязан узнать это из отказа, а не искать причину в compose.
func (m *Manager) outRootHint() string {
	if m.cfg.legacyOutIgnored() {
		return fmt.Sprintf("REEXPORT_OUT_DIR=%s вне тома и не применяется", m.cfg.OutDir)
	}
	return "REEXPORT_OUT_DIR применяется только внутри тома моделей"
}

// isDir — истина, если путь существует и является каталогом.
func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// Get отдаёт состояние задачи.
func (m *Manager) Get(id string) (jobView, bool) {
	m.mu.Lock()
	j := m.jobs[id]
	m.mu.Unlock()
	if j == nil {
		return jobView{}, false
	}
	return m.snapshot(id), true
}

// List отдаёт задачи в порядке создания (свежие первыми).
func (m *Manager) List() []jobView {
	m.mu.Lock()
	ids := make([]string, 0, len(m.order))
	ids = append(ids, m.order...)
	m.mu.Unlock()
	out := make([]jobView, 0, len(ids))
	for i := len(ids) - 1; i >= 0; i-- {
		out = append(out, m.snapshot(ids[i]))
	}
	return out
}

// Cancel отменяет задачу. Возвращает обновлённое состояние.
func (m *Manager) Cancel(id string) (jobView, error) {
	m.mu.Lock()
	j := m.jobs[id]
	if j == nil {
		m.mu.Unlock()
		return jobView{}, &jobError{Code: "job_not_found", Message: "задача не найдена: " + id}
	}
	switch j.Status {
	case statusQueued:
		// Ещё не началась — снимаем из очереди немедленно; исполнитель,
		// увидев статус, пропустит её.
		m.mu.Unlock()
		m.setStatus(id, statusCanceled, &jobError{Code: "canceled", Message: "задача отменена до запуска"})
		m.log.Infow("job canceled (queued)", "id", id)
		return m.snapshot(id), nil
	case statusRunning:
		cancel := j.cancel
		m.mu.Unlock()
		if cancel != nil {
			cancel()
			m.log.Infow("job cancel requested", "id", id)
			// Статус выставит исполнитель, увидев отмену контекста: так
			// «отменено» не появится раньше, чем процесс реально убит.
			return m.snapshot(id), nil
		}
		return m.snapshot(id), nil
	default:
		m.mu.Unlock()
		return jobView{}, &jobError{Code: "job_finished", Message: fmt.Sprintf(
			"задача уже завершена (status=%s) — отменять нечего", j.Status)}
	}
}

// worker — единственный исполнитель (параллелизм 1).
func (m *Manager) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-m.queue:
			m.runOne(ctx, id)
		}
	}
}

// runOne выполняет одну задачу и фиксирует итог.
//
// ПРОВЕРКА МЕСТА — ДО queued→running. Считать «влезет ли» после старта значит
// показать оператору `running` с прогрессом и только потом отказ: он уже ждёт,
// а диск уже трогают. Поэтому требуемый объём считается здесь, пока задача ещё
// `queued`: не хватает — `failed`/`insufficient_space` с числами «нужно/свободно»,
// и исполнитель не запускается вовсе (StartedAt остаётся пустым).
func (m *Manager) runOne(ctx context.Context, id string) {
	m.mu.Lock()
	j := m.jobs[id]
	if j == nil || j.Status != statusQueued {
		m.mu.Unlock()
		return // отменена/перезапущена до того, как до неё дошла очередь
	}
	src, out, target, opts := j.SrcPath, j.OutPath, j.Target, j.Options
	m.mu.Unlock()

	if je := m.checkSpace(target, src, out, opts); je != nil {
		m.appendLog(id, "задача не запущена: "+je.Message)
		m.setStatus(id, statusFailed, je)
		m.log.Warnw("job rejected before start", "id", id, "code", je.Code, "detail", je.Detail)
		return
	}

	m.mu.Lock()
	j = m.jobs[id]
	if j == nil || j.Status != statusQueued {
		m.mu.Unlock()
		return // отменили, пока считали место
	}
	started := m.now().UTC()
	j.Status = statusRunning
	j.StartedAt = &started
	j.Progress = jobProgress{Phase: "start", Stage: "reading"}
	m.persistLocked(id)
	src, out, target, opts = j.SrcPath, j.OutPath, j.Target, j.Options
	m.mu.Unlock()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.mu.Lock()
	if jj := m.jobs[id]; jj != nil {
		jj.cancel = cancel
	}
	m.mu.Unlock()

	m.appendLog(id, fmt.Sprintf("старт: цель=%s источник=%s результат=%s", target, src, out))
	req := runRequest{
		JobID:         id,
		Target:        target,
		SrcPath:       src,
		OutPath:       out,
		Options:       opts,
		ModelsDir:     m.cfg.ModelsDir,
		ConverterPath: m.cfg.ConverterPath,
	}
	err := m.runner.Run(runCtx, req, func(ev runEvent) {
		m.applyEvent(id, ev)
	})

	canceled := runCtx.Err() != nil
	now := m.now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.jobs[id]
	if cur == nil {
		return
	}
	cur.cancel = nil
	cur.FinishedAt = &now
	switch {
	case canceled:
		cur.Status = statusCanceled
		cur.Error = &jobError{Code: "canceled", Message: "задача отменена оператором"}
		// НЕДОПИСАННЫЙ РЕЗУЛЬТАТ УБИРАЕМ И ПРИ ОТМЕНЕ.
		//
		// ЖИВОЙ ДЕФЕКТ (D11, прогон в браузере). Ветка отмены не звала
		// removePartial, хотя промис контракта обратный: «DELETE — процесс
		// конвертера убит, недописанный файл удалён» (docs/operations/
		// gguf-reexport.ru.md §3). На стенде после отмены на 0.8 % в каталоге
		// моделей остался out/cancel-demo.gguf (134 МБ) с именем, которое
		// выглядит как модель: воркер увидел бы «модель» и упал на загрузке —
		// ровно та причина, по которой removePartial и написан.
		m.removePartial(cur)
	case err != nil:
		cur.Status = statusFailed
		var je *jobError
		if errors.As(err, &je) {
			cur.Error = je
		} else {
			cur.Error = &jobError{Code: "converter_failed", Message: err.Error()}
		}
		m.removePartial(cur)
	case cur.Result == nil:
		// Раннер не прислал итог — это тоже отказ, а не «успех без чисел».
		cur.Status = statusFailed
		cur.Error = &jobError{Code: "no_result", Message: "конвертер завершился без итоговых данных"}
		m.removePartial(cur)
	default:
		cur.Status = statusSucceeded
		cur.Progress = jobProgress{Phase: "done", Stage: "done", Percent: 100,
			BytesDone: cur.Progress.BytesTotal, BytesTotal: cur.Progress.BytesTotal}
	}
	cur.LogTail = cur.log.snapshot()
	m.persistLocked(id)
	dur := int64(0)
	if cur.StartedAt != nil {
		dur = now.Sub(*cur.StartedAt).Milliseconds()
	}
	m.log.Infow("job finished", "id", id, "status", cur.Status, "duration_ms", dur,
		"bytes_dst", resultBytes(cur.Result))
}

// removePartial убирает недописанный результат: половина 16-гигабайтного файла в
// каталоге моделей опаснее отсутствия файла — воркер увидит «модель» и упадёт на
// загрузке. Исходник не трогаем никогда.
func (m *Manager) removePartial(j *job) {
	if j.OutPath == "" {
		return
	}
	// Только внутри тома моделей: удалять что-то за его пределами сервис не
	// имеет права, даже если путь туда когда-то попал (старая запись задачи).
	if !pathWithin(j.OutPath, m.cfg.ModelsDir) {
		return
	}
	if _, err := os.Stat(j.OutPath); err != nil {
		return
	}
	if err := os.Remove(j.OutPath); err != nil {
		j.LogTail = append(j.LogTail, "частичный результат не удалён: "+err.Error())
		return
	}
	j.LogTail = append(j.LogTail, "частичный результат удалён: "+j.OutPath)
}

func resultBytes(r *jobResult) int64 {
	if r == nil {
		return 0
	}
	return r.DstBytes
}

// checkSpace — хватит ли места на целевом томе для этой задачи.
//
// Отказ возвращается ошибкой задачи (код + числа), а не «500»: оператор должен
// увидеть «нужно 33.0 ГиБ, свободно 12.0 ГиБ» и слагаемые, из которых это
// сложилось, — иначе непонятно, освобождать том или хватит уменьшить вход.
//
// НЕИЗМЕРЯЕМОЕ МЕСТО НЕ БЛОКИРУЕТ. Если диск не отвечает (экзотическая ФС, нет
// прав на каталог), задача идёт дальше: отказ «мы не смогли посчитать» хуже, чем
// отказ драйвера, который всё равно проверит место перед первой записью.
func (m *Manager) checkSpace(target, src, out string, opts jobOptions) *jobError {
	if m.freeSpace == nil {
		return nil
	}
	need, err := needForTask(target, src, opts)
	if err != nil {
		m.log.Warnw("space requirement not computed — задача продолжается", "target", target,
			"src", src, "error", err)
		return nil
	}
	if need.Need == 0 {
		return nil // dry-run: результат не пишется
	}
	dir := filepath.Dir(out)
	free, err := m.freeSpace(dir)
	if err != nil {
		m.log.Warnw("free space not measured — задача продолжается", "dir", dir, "error", err)
		return nil
	}
	if free >= need.Need {
		return nil
	}
	parts := []string{"вход " + formatGiB(need.Input)}
	if need.Output > 0 {
		parts = append(parts, "выход ~"+formatGiB(need.Output))
	}
	parts = append(parts, "запас "+formatGiB(spaceReserveBytes))
	return &jobError{
		Code: "insufficient_space",
		Message: fmt.Sprintf("недостаточно места для результата в %s: нужно %s, свободно %s (%s); "+
			"освободите том моделей (старые файлы, частичные результаты) или выберите вход меньше",
			dir, formatGiB(need.Need), formatGiB(free), strings.Join(parts, " + ")),
		Detail: fmt.Sprintf("need_bytes=%d free_bytes=%d reserve_bytes=%d out=%s",
			need.Need, free, spaceReserveBytes, out),
	}
}

// applyEvent применяет событие конвертера к состоянию задачи.
func (m *Manager) applyEvent(id string, ev runEvent) {
	m.mu.Lock()
	j := m.jobs[id]
	if j == nil {
		m.mu.Unlock()
		return
	}
	var persist bool
	switch ev.Type {
	case "progress":
		j.Progress = ev.Progress
		persist = true
	case "log":
		j.log.add(ev.Line)
		j.LogTail = j.log.snapshot()
	case "result":
		if ev.Result != nil {
			j.Result = ev.Result
		}
	case "error":
		if ev.Error != nil {
			j.log.add("ошибка: " + ev.Error.Message)
			j.LogTail = j.log.snapshot()
		}
	}
	m.mu.Unlock()
	if persist {
		// Прогресс пишем на диск throttled: файл состояния не должен
		// переписываться 200 раз за копирование 16 ГиБ.
		if m.progressDue(id) {
			m.mu.Lock()
			m.persistLocked(id)
			m.mu.Unlock()
		}
	}
}

func (m *Manager) appendLog(id, line string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j := m.jobs[id]; j != nil {
		j.log.add(line)
		j.LogTail = j.log.snapshot()
	}
}

func (m *Manager) setStatus(id, status string, jobErr *jobError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	if j == nil {
		return
	}
	now := m.now().UTC()
	j.Status = status
	j.Error = jobErr
	if status != statusQueued && status != statusRunning {
		j.FinishedAt = &now
	}
	j.LogTail = j.log.snapshot()
	m.persistLocked(id)
}

func (m *Manager) snapshot(id string) jobView {
	m.mu.Lock()
	j := m.jobs[id]
	if j == nil {
		m.mu.Unlock()
		return jobView{}
	}
	v := j.jobView
	v.LogTail = j.log.snapshot()
	m.mu.Unlock()
	return v
}

// progressDue — не чаще одного раза в 2 с на задачу (файл состояния не должен
// переписываться двести раз за копирование 16 ГиБ).
func (m *Manager) progressDue(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if t, ok := m.progressAt[id]; ok && now.Sub(t) < 2*time.Second {
		return false
	}
	m.progressAt[id] = now
	return true
}

// persist пишет состояние задачи атомарно (tmp + rename): читатель либо видит
// прошлую версию, либо новую, но никогда половину JSON.
func (m *Manager) persist(j *job) {
	data, err := json.MarshalIndent(j.jobView, "", "  ")
	if err != nil {
		m.log.Warnw("job marshal failed", "id", j.ID, "error", err)
		return
	}
	path := filepath.Join(m.cfg.JobsDir, j.ID+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		m.log.Warnw("job persist failed", "id", j.ID, "error", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		m.log.Warnw("job persist rename failed", "id", j.ID, "error", err)
	}
}

func (m *Manager) persistLocked(id string) {
	if j := m.jobs[id]; j != nil {
		m.persist(j)
	}
}

// pathWithin — путь внутри корня (после очистки и с учётом регистра на Windows,
// где сервис тоже запускают локально `go run`).
func pathWithin(path, root string) bool {
	if root == "" {
		return false
	}
	p := filepath.Clean(path)
	r := filepath.Clean(root)
	rel, err := filepath.Rel(r, p)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." {
		return false
	}
	return !filepath.IsAbs(rel)
}

// sortedIDs — для стабильного вывода диагностики.
func (m *Manager) sortedIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.jobs))
	for id := range m.jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// buildIdentity — запечённая идентичность для /api/version и capabilities.
func buildIdentity() map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range version.Get().Fields() {
		out[k] = v
	}
	return out
}
