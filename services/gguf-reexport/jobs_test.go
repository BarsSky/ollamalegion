// jobs_test.go — очередь задач: сериализация, отмена, хранение состояния.
//
// ЗАЧЕМ ЭТИ ПРОВЕРКИ. Ре-экспорт — долгая операция на диске моделей, и все
// неприятные случаи здесь про состояние, а не про арифметику: задача уже идёт, а
// её отменили; вторая задача ждёт в очереди; контейнер перезапустили посреди
// копирования. Ошибка в любом из них выглядит как «кнопка не работает» или как
// «файл на 16 ГиБ непонятного вида в каталоге моделей», поэтому проверяем прямо:
// параллелизм 1, статусы, удаление частичного результата и то, что состояние
// читается с диска после перезапуска.
package main

import (
	"context"
	"errors"
	"go.uber.org/zap"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRunner — исполнитель-заглушка: тест управляет моментами «начал», «сообщил
// прогресс», «закончил» и «отменён».
type fakeRunner struct {
	started chan runRequest
	release chan struct{}
	emit    func(sink func(runEvent))
	runErr  error
	calls   int
}

func (f *fakeRunner) Run(ctx context.Context, req runRequest, sink func(runEvent)) error {
	f.calls++
	if f.started != nil {
		select {
		case f.started <- req:
		default:
		}
	}
	if f.emit != nil {
		f.emit(sink)
	}
	if f.release != nil {
		select {
		case <-ctx.Done():
			return &jobError{Code: "canceled", Message: "выполнение прервано отменой"}
		case <-f.release:
		}
	}
	return f.runErr
}

// newTestManager — менеджер на временных каталогах (модели + задачи).
func newTestManager(t *testing.T, runner jobRunner) (*Manager, config, context.CancelFunc) {
	t.Helper()
	root := t.TempDir()
	models := filepath.Join(root, "models")
	jobs := filepath.Join(root, "jobs")
	if err := os.MkdirAll(models, 0o755); err != nil {
		t.Fatalf("models: %v", err)
	}
	cfg := config{
		ModelsDir:    models,
		JobsDir:      jobs,
		ScanDepth:    2,
		ScanMaxFiles: 100,
	}
	// Небольшой настоящий GGUF: путь источника должен существовать.
	writeTestGGUF(t, filepath.Join(models, "inkling.gguf"), 32, 2, "inkling2")
	m := newManager(cfg, runner, zap.NewNop().Sugar())
	ctx, cancel := context.WithCancel(context.Background())
	if err := m.Load(ctx); err != nil {
		cancel()
		t.Fatalf("Load: %v", err)
	}
	return m, cfg, cancel
}

// emitMinimalResult — минимальный «успешный» итог: без него менеджер честно
// считает задачу проваленной («конвертер завершился без итоговых данных»), и
// тест проверял бы не сериализацию, а именно это правило.
func emitMinimalResult(sink func(runEvent)) {
	sink(runEvent{Type: "result", Result: &jobResult{DurationMS: 5, SrcBytes: 10, DstBytes: 20}})
}

func waitStatus(t *testing.T, m *Manager, id, want string, timeout time.Duration) jobView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last jobView
	for time.Now().Before(deadline) {
		v, ok := m.Get(id)
		if !ok {
			t.Fatalf("задача %s исчезла", id)
		}
		last = v
		if v.Status == want {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("задача %s не дошла до статуса %q за %s (последний: %q, ошибка: %v)",
		id, want, timeout, last.Status, last.Error)
	return last
}

// waitDone — дождаться терминального статуса, когда конкретный не важен.
//
// Нужен тестам, которые проверяют путь результата, а не итог: удалять временный
// каталог под работающим исполнителем нельзя — он ещё пишет файл состояния, и
// t.TempDir() падает на непустом каталоге.
func waitDone(t *testing.T, m *Manager, id string) jobView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last jobView
	for time.Now().Before(deadline) {
		v, ok := m.Get(id)
		if !ok {
			t.Fatalf("задача %s исчезла", id)
		}
		last = v
		switch v.Status {
		case statusSucceeded, statusFailed, statusCanceled, statusInterrupted:
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("задача %s не завершилась за 5 с (статус %q)", id, last.Status)
	return last
}

func TestJobRunsToSuccessAndPersists(t *testing.T) {
	runner := &fakeRunner{
		emit: func(sink func(runEvent)) {
			sink(runEvent{Type: "progress", Progress: jobProgress{Phase: "copy", Percent: 40, BytesDone: 4, BytesTotal: 10}})
			sink(runEvent{Type: "log", Line: "пишу результат"})
			sink(runEvent{Type: "result", Result: &jobResult{
				DurationMS: 1234, SrcBytes: 100, DstBytes: 2048,
				Facts:   map[string]interface{}{"arch": "inkling2"},
				Changes: []string{"general.architecture: inkling2 -> inkling2_loop"},
			}})
		},
	}
	m, cfg, cancel := newTestManager(t, runner)
	defer cancel()

	view, err := m.Create(createRequest{
		ModelDir: "inkling.gguf",
		Target:   "inkling2_loop",
		Options:  jobOptions{FuseLoopQueries: true},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if view.Status != statusQueued && view.Status != statusRunning {
		t.Fatalf("только что созданная задача в статусе %q", view.Status)
	}
	final := waitStatus(t, m, view.ID, statusSucceeded, 5*time.Second)
	if final.Result == nil || final.Result.DstBytes != 2048 {
		t.Fatalf("итог не сохранён: %+v", final.Result)
	}
	if final.Progress.Percent != 100 {
		t.Fatalf("успешная задача не отмечена 100%%: %v", final.Progress.Percent)
	}
	if final.OutPath != filepath.Join(cfg.ModelsDir, "inkling.inkling2_loop.gguf") {
		t.Fatalf("путь результата по умолчанию: %q", final.OutPath)
	}
	// Состояние обязано лежать на диске и читаться новым менеджером (перезапуск
	// контейнера — штатная ситуация).
	if _, err := os.Stat(filepath.Join(cfg.JobsDir, view.ID+".json")); err != nil {
		t.Fatalf("файл состояния не записан: %v", err)
	}
	m2 := newManager(cfg, runner, zap.NewNop().Sugar())
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	if err := m2.Load(ctx2); err != nil {
		t.Fatalf("Load после перезапуска: %v", err)
	}
	reloaded, ok := m2.Get(view.ID)
	if !ok {
		t.Fatal("после перезапуска задача не найдена")
	}
	if reloaded.Status != statusSucceeded {
		t.Fatalf("после перезапуска статус %q, ожидался succeeded", reloaded.Status)
	}
}

func TestRunningJobBecomesInterruptedAfterRestart(t *testing.T) {
	root := t.TempDir()
	cfg := config{ModelsDir: filepath.Join(root, "models"), JobsDir: filepath.Join(root, "jobs"), ScanDepth: 2, ScanMaxFiles: 10}
	if err := os.MkdirAll(cfg.ModelsDir, 0o755); err != nil {
		t.Fatalf("models: %v", err)
	}
	// Файл состояния «running», как будто контейнер убили посреди работы.
	stateJSON := `{"id":"rj-dead","status":"running","target":"inkling2_loop","model_dir":"inkling.gguf","out_path":"` +
		filepath.ToSlash(filepath.Join(cfg.ModelsDir, "x.gguf")) + `","created_at":"2026-11-23T10:00:00Z"}`
	if err := os.MkdirAll(cfg.JobsDir, 0o755); err != nil {
		t.Fatalf("jobs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfg.JobsDir, "rj-dead.json"), []byte(stateJSON), 0o644); err != nil {
		t.Fatalf("state: %v", err)
	}
	m := newManager(cfg, &fakeRunner{}, zap.NewNop().Sugar())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}
	v, ok := m.Get("rj-dead")
	if !ok {
		t.Fatal("задача не восстановлена")
	}
	if v.Status != statusInterrupted {
		t.Fatalf("статус %q, ожидался interrupted (задача не может продолжаться после перезапуска)", v.Status)
	}
	if v.Error == nil || v.Error.Code != "interrupted" {
		t.Fatalf("нет объяснения прерывания: %+v", v.Error)
	}
}

func TestOnlyOneJobRunsAtATime(t *testing.T) {
	release := make(chan struct{})
	started := make(chan runRequest, 2)
	runner := &fakeRunner{started: started, release: release, emit: emitMinimalResult}
	m, _, cancel := newTestManager(t, runner)
	defer cancel()

	first, err := m.Create(createRequest{ModelDir: "inkling.gguf", Target: "inkling2_loop"})
	if err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("первая задача не началась")
	}
	second, err := m.Create(createRequest{ModelDir: "inkling.gguf", Target: "inkling2_loop",
		Options: jobOptions{OutPath: "b.gguf"}})
	if err != nil {
		t.Fatalf("Create 2: %v", err)
	}
	// Вторая не должна стартовать, пока первая держит диск.
	if v, _ := m.Get(second.ID); v.Status != statusQueued {
		t.Fatalf("вторая задача в статусе %q, ожидался queued (параллелизм обязан быть 1)", v.Status)
	}
	select {
	case req := <-started:
		t.Fatalf("вторая задача стартовала параллельно: %s", req.JobID)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	waitStatus(t, m, first.ID, statusSucceeded, 5*time.Second)
	waitStatus(t, m, second.ID, statusSucceeded, 5*time.Second)
}

func TestCancelRunningJobKillsRunnerAndStatusIsCanceled(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	started := make(chan runRequest, 1)
	runner := &fakeRunner{started: started, release: release, emit: emitMinimalResult}
	m, _, cancel := newTestManager(t, runner)
	defer cancel()

	view, err := m.Create(createRequest{ModelDir: "inkling.gguf", Target: "inkling2_loop"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("задача не началась")
	}
	if _, err := m.Cancel(view.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	final := waitStatus(t, m, view.ID, statusCanceled, 5*time.Second)
	if final.Error == nil || final.Error.Code != "canceled" {
		t.Fatalf("нет причины отмены: %+v", final.Error)
	}
}

// TestCancelRunningJobRemovesPartialResult — отмена ОБЯЗАНА убирать недописанный
// результат.
//
// ЖИВОЙ ДЕФЕКТ (D11, прогон сценария в браузере). Контракт обещает «DELETE —
// процесс конвертера убит, недописанный файл удалён» (docs/operations/
// gguf-reexport.ru.md §3), а ветка отмены в runOne removePartial не звала:
// на стенде после отмены на 0.8 % в каталоге моделей остался
// out/cancel-demo.gguf (134 МБ). Такой огрызок выглядит для воркера моделью —
// он попадёт в инвентарь и упадёт на загрузке.
func TestCancelRunningJobRemovesPartialResult(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	started := make(chan runRequest, 1)
	runner := &fakeRunner{started: started, release: release}
	m, _, cancel := newTestManager(t, runner)
	defer cancel()

	view, err := m.Create(createRequest{ModelDir: "inkling.gguf", Target: "inkling2_loop"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var req runRequest
	select {
	case req = <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("задача не началась")
	}
	// Недописанный результат: файл уже создан, данные ещё пишутся.
	if err := os.WriteFile(req.OutPath, []byte("partial"), 0o644); err != nil {
		t.Fatalf("частичный результат: %v", err)
	}
	if _, err := m.Cancel(view.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	waitStatus(t, m, view.ID, statusCanceled, 5*time.Second)
	if _, err := os.Stat(req.OutPath); !os.IsNotExist(err) {
		t.Fatalf("недописанный результат остался на диске: %s (err=%v)", req.OutPath, err)
	}
}

func TestCancelQueuedJobNeverStarts(t *testing.T) {
	release := make(chan struct{})
	started := make(chan runRequest, 2)
	runner := &fakeRunner{started: started, release: release, emit: emitMinimalResult}
	m, _, cancel := newTestManager(t, runner)
	defer cancel()

	first, err := m.Create(createRequest{ModelDir: "inkling.gguf", Target: "inkling2_loop"})
	if err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	<-started
	second, err := m.Create(createRequest{ModelDir: "inkling.gguf", Target: "inkling2_loop",
		Options: jobOptions{OutPath: "b.gguf"}})
	if err != nil {
		t.Fatalf("Create 2: %v", err)
	}
	if _, err := m.Cancel(second.ID); err != nil {
		t.Fatalf("Cancel queued: %v", err)
	}
	if v, _ := m.Get(second.ID); v.Status != statusCanceled {
		t.Fatalf("отменённая в очереди задача в статусе %q", v.Status)
	}
	close(release)
	waitStatus(t, m, first.ID, statusSucceeded, 5*time.Second)
	time.Sleep(50 * time.Millisecond)
	if runner.calls > 1 {
		t.Fatalf("исполнитель вызван %d раз — отменённая в очереди задача всё равно запустилась", runner.calls)
	}
}

func TestCreateRejectsBadRequests(t *testing.T) {
	runner := &fakeRunner{}
	m, cfg, cancel := newTestManager(t, runner)
	defer cancel()

	if _, err := m.Create(createRequest{ModelDir: "inkling.gguf", Target: "nope"}); err == nil {
		t.Fatal("неизвестная цель принята")
	}
	if _, err := m.Create(createRequest{ModelDir: "missing.gguf", Target: "inkling2_loop"}); err == nil {
		t.Fatal("отсутствующая модель принята")
	}
	if _, err := m.Create(createRequest{ModelDir: "../outside.gguf", Target: "inkling2_loop"}); err == nil {
		t.Fatal("путь вне тома моделей принят")
	}
	// Результат вне разрешённых корней — отказ, а не запись куда угодно.
	if _, err := m.Create(createRequest{ModelDir: "inkling.gguf", Target: "inkling2_loop",
		Options: jobOptions{OutPath: filepath.Join(os.TempDir(), "evil.gguf")}}); err == nil {
		t.Fatal("результат вне корней принят")
	}
	// Существующий результат без force — конфликт.
	existing := filepath.Join(cfg.ModelsDir, "inkling.out.gguf")
	if err := os.WriteFile(existing, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := m.Create(createRequest{ModelDir: "inkling.gguf", Target: "inkling2_loop",
		Options: jobOptions{OutPath: "inkling.out.gguf"}}); err == nil {
		t.Fatal("перезапись существующего результата без force принята")
	}
	// unbuffered target: hf_to_gguf объявлена недоступной, пока проба не сказала иначе.
	targetRegistry.setAvailability("hf_to_gguf", false, "образ без torch")
	defer targetRegistry.setAvailability("hf_to_gguf", false, "образ без torch")
	if _, err := m.Create(createRequest{ModelDir: "inkling.gguf", Target: "hf_to_gguf"}); err == nil {
		t.Fatal("недоступная цель принята")
	}
}

// ---------------------------------------------------------------------------
// D10.1 — место проверяется ДО старта задачи
// ---------------------------------------------------------------------------

func TestJobRejectedWhenDiskIsShort(t *testing.T) {
	runner := &fakeRunner{emit: emitMinimalResult}
	m, _, cancel := newTestManager(t, runner)
	defer cancel()

	// Свободно 512 МиБ: вход + запас в 1 ГиБ сюда не влезает.
	const free = int64(512) << 20
	m.freeSpace = func(string) (int64, error) { return free, nil }

	view, err := m.Create(createRequest{ModelDir: "inkling.gguf", Target: "inkling2_loop"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	final := waitStatus(t, m, view.ID, statusFailed, 5*time.Second)
	if final.Error == nil || final.Error.Code != "insufficient_space" {
		t.Fatalf("код отказа %+v, ожидался insufficient_space", final.Error)
	}
	// Числа обязаны быть в ответе: «нужно X ГиБ, свободно Y ГиБ» — это то, по
	// чему оператор решает, освобождать том или уменьшать вход.
	info, err := os.Stat(view.SrcPath)
	if err != nil {
		t.Fatalf("источник: %v", err)
	}
	for _, want := range []string{"нужно", "свободно", formatGiB(info.Size() + spaceReserveBytes), formatGiB(free)} {
		if !strings.Contains(final.Error.Message, want) {
			t.Fatalf("в сообщении нет %q: %s", want, final.Error.Message)
		}
	}
	if final.Error.Detail == "" {
		t.Fatal("нет detail с байтами: разбирать «сколько именно» будет нечем")
	}
	// Задача не стартовала: исполнитель не вызван, время старта пусто.
	if runner.calls != 0 {
		t.Fatalf("исполнитель вызван %d раз, хотя места нет", runner.calls)
	}
	if final.StartedAt != nil {
		t.Fatalf("у незапущенной задачи есть started_at: %v", final.StartedAt)
	}
	if _, err := os.Stat(final.OutPath); !os.IsNotExist(err) {
		t.Fatalf("появился файл результата %s (err=%v)", final.OutPath, err)
	}
}

func TestJobStartsWhenSpaceIsEnough(t *testing.T) {
	runner := &fakeRunner{emit: emitMinimalResult}
	m, _, cancel := newTestManager(t, runner)
	defer cancel()

	m.freeSpace = func(string) (int64, error) { return 1 << 40, nil } // 1 ТиБ
	view, err := m.Create(createRequest{ModelDir: "inkling.gguf", Target: "inkling2_loop"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	final := waitStatus(t, m, view.ID, statusSucceeded, 5*time.Second)
	if final.StartedAt == nil {
		t.Fatal("места хватает, а задача не стартовала")
	}
	if runner.calls != 1 {
		t.Fatalf("исполнитель вызван %d раз, ожидался 1", runner.calls)
	}
}

// TestDryRunIgnoresSpace — план ничего не пишет, поэтому места не требует:
// иначе dry-run на полном томе отказывал бы там, где писать и не собирался.
func TestDryRunIgnoresSpace(t *testing.T) {
	runner := &fakeRunner{emit: emitMinimalResult}
	m, _, cancel := newTestManager(t, runner)
	defer cancel()

	m.freeSpace = func(string) (int64, error) { return 1, nil } // места нет совсем
	view, err := m.Create(createRequest{ModelDir: "inkling.gguf", Target: "inkling2_loop",
		Options: jobOptions{DryRun: true}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitStatus(t, m, view.ID, statusSucceeded, 5*time.Second)
}

// ---------------------------------------------------------------------------
// D10.2 — выход внутри каталога модели, том моделей — граница
// ---------------------------------------------------------------------------

func TestDefaultOutputIsInsideModelDirectory(t *testing.T) {
	runner := &fakeRunner{emit: emitMinimalResult}
	m, cfg, cancel := newTestManager(t, runner)
	defer cancel()

	sub := filepath.Join(cfg.ModelsDir, "inkling2")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("подкаталог модели: %v", err)
	}
	writeTestGGUF(t, filepath.Join(sub, "m.gguf"), 32, 2, "inkling2")

	view, err := m.Create(createRequest{ModelDir: filepath.Join("inkling2", "m.gguf"), Target: "inkling2_loop"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if want := filepath.Join(sub, "m.inkling2_loop.gguf"); view.OutPath != want {
		t.Fatalf("результат %q, ожидался рядом с моделью %q", view.OutPath, want)
	}
	waitDone(t, m, view.ID)
}

func TestRelativeOutPathStaysInModelDirectory(t *testing.T) {
	runner := &fakeRunner{emit: emitMinimalResult}
	m, cfg, cancel := newTestManager(t, runner)
	defer cancel()

	view, err := m.Create(createRequest{ModelDir: "inkling.gguf", Target: "inkling2_loop",
		Options: jobOptions{OutPath: filepath.Join("out", "deeper", "x.gguf")}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if want := filepath.Join(cfg.ModelsDir, "out", "deeper", "x.gguf"); view.OutPath != want {
		t.Fatalf("результат %q, ожидался подкаталог каталога модели %q", view.OutPath, want)
	}
	waitDone(t, m, view.ID)
}

func TestOutputEscapeIsPathNotAllowed(t *testing.T) {
	runner := &fakeRunner{}
	m, _, cancel := newTestManager(t, runner)
	defer cancel()

	cases := map[string]string{
		"выход из каталога модели": filepath.Join("..", "escape.gguf"),
		"абсолютный путь вне тома": filepath.Join(t.TempDir(), "evil.gguf"),
		"вложенный выход":          filepath.Join("out", "..", "..", "escape.gguf"),
	}
	for name, outPath := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := m.Create(createRequest{ModelDir: "inkling.gguf", Target: "inkling2_loop",
				Options: jobOptions{OutPath: outPath}})
			var je *jobError
			if !errors.As(err, &je) {
				t.Fatalf("путь %q принят (err=%v)", outPath, err)
			}
			if je.Code != "path_not_allowed" {
				t.Fatalf("код %q, ожидался path_not_allowed", je.Code)
			}
		})
	}
}

// TestLegacyOutDirCannotLeaveModelsVolume — REEXPORT_OUT_DIR оставлен
// legacy-fallback'ом, но выйти за том моделей не даёт.
func TestLegacyOutDirCannotLeaveModelsVolume(t *testing.T) {
	runner := &fakeRunner{}
	m, cfg, cancel := newTestManager(t, runner)
	defer cancel()

	// Источник вне тома моделей: так выглядит старая раскладка, ради которой
	// переменная и оставлена. Собираем путь напрямую — Create такой вход не примет.
	outsideDir := t.TempDir()
	src := filepath.Join(outsideDir, "outside.gguf")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatalf("источник: %v", err)
	}

	// 1) Переменная указывает за пределы тома: применять её нельзя.
	m.cfg.OutDir = filepath.Join(t.TempDir(), "second-disk")
	_, err := m.resolveOutput(src, "inkling2_loop", jobOptions{})
	var je *jobError
	if !errors.As(err, &je) || je.Code != "path_not_allowed" {
		t.Fatalf("результат вне тома разрешён (err=%v)", err)
	}
	if !m.cfg.legacyOutIgnored() {
		t.Fatal("непригодная переменная не отмечена как игнорируемая")
	}

	// 2) Переменная внутри тома: legacy-запрос продолжает работать.
	legacy := filepath.Join(cfg.ModelsDir, "legacy_out")
	m.cfg.OutDir = legacy
	got, err := m.resolveOutput(src, "inkling2_loop", jobOptions{})
	if err != nil {
		t.Fatalf("resolveOutput(legacy): %v", err)
	}
	if want := filepath.Join(legacy, "outside.inkling2_loop.gguf"); got != want {
		t.Fatalf("legacy-путь %q, ожидался %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// D10.3 — стадии драйвера видны в статусе задачи
// ---------------------------------------------------------------------------

// stepRunner — исполнитель, который проходит стадии по одной и ждёт, пока тест
// посмотрит состояние: после завершения задачи прогресс уже переписан на
// «done/100», и «стадия в статусе» иначе не проверить.
type stepRunner struct {
	stages  []string
	reached chan string
	proceed chan struct{}
}

func (s *stepRunner) Run(ctx context.Context, _ runRequest, sink func(runEvent)) error {
	for _, stage := range s.stages {
		sink(runEvent{Type: "progress", Progress: jobProgress{Stage: stage, Percent: 50}})
		s.reached <- stage
		select {
		case <-ctx.Done():
			return &jobError{Code: "canceled", Message: "выполнение прервано отменой"}
		case <-s.proceed:
		}
	}
	sink(runEvent{Type: "result", Result: &jobResult{DurationMS: 1, DstBytes: 2}})
	return nil
}

func TestJobProgressFollowsDriverStages(t *testing.T) {
	runner := &stepRunner{
		stages:  []string{"reading", "writing", "validating"},
		reached: make(chan string),
		proceed: make(chan struct{}),
	}
	m, _, cancel := newTestManager(t, runner)
	defer cancel()

	view, err := m.Create(createRequest{ModelDir: "inkling.gguf", Target: "inkling2_loop"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, want := range runner.stages {
		select {
		case got := <-runner.reached:
			if got != want {
				t.Fatalf("драйвер сообщил стадию %q, ожидалась %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("стадия %q не началась", want)
		}
		// Событие применено синхронно до этой точки — состояние обязано совпасть.
		status, ok := m.Get(view.ID)
		if !ok {
			t.Fatal("задача исчезла")
		}
		if status.Progress.Stage != want {
			t.Fatalf("в статусе задачи стадия %q, драйвер сообщил %q", status.Progress.Stage, want)
		}
		if status.Progress.Percent != 50 {
			t.Fatalf("в статусе процент %v, драйвер сообщил 50", status.Progress.Percent)
		}
		runner.proceed <- struct{}{}
	}
	final := waitStatus(t, m, view.ID, statusSucceeded, 5*time.Second)
	if final.Progress.Stage != "done" || final.Progress.Percent != 100 {
		t.Fatalf("итоговый прогресс %q/%v, ожидался done/100", final.Progress.Stage, final.Progress.Percent)
	}
}
