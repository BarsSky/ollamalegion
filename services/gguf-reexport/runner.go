// runner.go — запуск python-конвертера как дочернего процесса.
//
// ПОЧЕМУ PYTHON ОТДЕЛЬНО, А НЕ ЧЕРЕЗ CGO/БИБЛИОТЕКУ. Конвертер llama.cpp — это
// Python-экосистема (numpy; для полного ре-экспорта ещё torch и transformers).
// Тянуть её внутрь Go-процесса нельзя: падение импорта torch не должно ронять
// HTTP-сервис, а обновление конвертера не должно требовать пересборки Go.
// Поэтому граница — процесс: Go держит очередь, состояние, отмену и HTTP, python
// делает ровно одну работу и говорит на языке JSON-строк.
//
// ОТМЕНА. `exec.CommandContext` убивает дочерний процесс по отмене контекста.
// Этого достаточно, потому что вся долгая работа (копирование десятков гигабайт)
// идёт в python; Go ничего не держит в памяти и не ждёт «дочитать».
//
// ПРОГРЕСС. Конвертер печатает события построчно (`{"event":"progress",...}`),
// Go разбирает их и обновляет состояние задачи. Всё, что не JSON, попадает в
// хвост лога задачи — оператор видит текстовую причину отказа, а не только код.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"go.uber.org/zap"
)

// runRequest — что именно запускать.
type runRequest struct {
	JobID         string
	Target        string
	SrcPath       string
	OutPath       string
	Options       jobOptions
	ModelsDir     string
	ConverterPath string
}

// runEvent — событие конвертера, уже переведённое в термины задачи.
type runEvent struct {
	Type     string // "progress" | "log" | "result" | "error"
	Progress jobProgress
	Line     string
	Result   *jobResult
	Error    *jobError
}

// jobRunner — шов для тестов: менеджер не знает, python там или что-то ещё.
type jobRunner interface {
	Run(ctx context.Context, req runRequest, sink func(runEvent)) error
}

// driverEvent — строка, которую печатает py/reexport_job.py.
//
// Формат намеренно плоский: одна строка — одно событие. Так его можно читать
// потока `docker logs` и видеть прогресс без разбора многогигабайтных дампов.
type driverEvent struct {
	Event      string                 `json:"event"`
	Phase      string                 `json:"phase"`
	Percent    float64                `json:"percent"`
	BytesDone  int64                  `json:"bytes_done"`
	BytesTotal int64                  `json:"bytes_total"`
	Message    string                 `json:"message"`
	Code       string                 `json:"code"`
	Detail     string                 `json:"detail"`
	DurationMS int64                  `json:"duration_ms"`
	SrcBytes   int64                  `json:"src_bytes"`
	DstBytes   int64                  `json:"dst_bytes"`
	Facts      map[string]interface{} `json:"facts"`
	Changes    []string               `json:"changes"`
	Verify     map[string]interface{} `json:"verify"`
}

// pythonRunner — исполнитель через python-драйвер.
type pythonRunner struct {
	python     string
	driverPath string
	extraEnv   []string
	log        *zap.SugaredLogger
}

func newPythonRunner(cfg config, log *zap.SugaredLogger) *pythonRunner {
	return &pythonRunner{
		python:     cfg.PythonBin,
		driverPath: cfg.DriverPath,
		log:        log,
	}
}

// Run запускает драйвер и транслирует его события в sink.
func (r *pythonRunner) Run(ctx context.Context, req runRequest, sink func(runEvent)) error {
	args := []string{r.driverPath, "--json-events", "--job-id", req.JobID,
		"--target", req.Target, "--src", req.SrcPath, "--dst", req.OutPath}
	if req.Options.FuseLoopQueries {
		args = append(args, "--fuse-loop-queries")
	}
	if req.Options.DryRun {
		args = append(args, "--dry-run")
	}
	if req.Options.Force {
		args = append(args, "--force")
	}
	if !req.Options.verifyEnabled() {
		args = append(args, "--no-verify")
	}
	if strings.TrimSpace(req.Options.OutType) != "" {
		args = append(args, "--outtype", req.Options.OutType)
	}
	if req.ConverterPath != "" {
		args = append(args, "--converter", req.ConverterPath)
	}

	cmd := exec.CommandContext(ctx, r.python, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return &jobError{Code: "runner_init", Message: "не удалось открыть stdout драйвера: " + err.Error()}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return &jobError{Code: "runner_init", Message: "не удалось открыть stderr драйвера: " + err.Error()}
	}
	if len(r.extraEnv) > 0 {
		cmd.Env = append(cmd.Environ(), r.extraEnv...)
	}
	if err := cmd.Start(); err != nil {
		return &jobError{Code: "runner_start", Message: fmt.Sprintf(
			"не удалось запустить %s: %v (в образе есть python-драйвер %s?)", r.python, err, r.driverPath)}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		r.consumeStdout(stdout, sink)
	}()
	go func() {
		defer wg.Done()
		r.consumeStderr(stderr, sink)
	}()
	wg.Wait()
	waitErr := cmd.Wait()

	if ctx.Err() != nil {
		return &jobError{Code: "canceled", Message: "выполнение прервано отменой"}
	}
	if waitErr != nil {
		// Код возврата драйвера переводим в код ошибки: у него своя нумерация
		// (2 — нет файла, 3 — патч не применим, 4 — мало места, ...).
		if code, msg := driverExitMeaning(waitErr); code != "" {
			return &jobError{Code: code, Message: msg, Detail: waitErr.Error()}
		}
		return &jobError{Code: "converter_failed", Message: "конвертер завершился с ошибкой: " + waitErr.Error()}
	}
	return nil
}

func (r *pythonRunner) consumeStdout(rd io.Reader, sink func(runEvent)) {
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(strings.TrimSpace(line), "{") {
			sink(runEvent{Type: "log", Line: line})
			continue
		}
		var ev driverEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			sink(runEvent{Type: "log", Line: line})
			continue
		}
		switch ev.Event {
		case "progress":
			sink(runEvent{Type: "progress", Progress: jobProgress{
				Phase: ev.Phase, Percent: ev.Percent, BytesDone: ev.BytesDone, BytesTotal: ev.BytesTotal}})
		case "result":
			sink(runEvent{Type: "result", Result: &jobResult{
				DurationMS: ev.DurationMS, SrcBytes: ev.SrcBytes, DstBytes: ev.DstBytes,
				Facts: ev.Facts, Changes: ev.Changes, Verify: ev.Verify}})
		case "error":
			sink(runEvent{Type: "error", Error: &jobError{Code: ev.Code, Message: ev.Message, Detail: ev.Detail}})
		case "log":
			sink(runEvent{Type: "log", Line: ev.Message})
		default:
			sink(runEvent{Type: "log", Line: line})
		}
	}
}

func (r *pythonRunner) consumeStderr(rd io.Reader, sink func(runEvent)) {
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		// Прогресс копирования в конвертере печатается через \r — чистим.
		line := strings.TrimSpace(strings.TrimRight(sc.Text(), "\r"))
		if line == "" || strings.HasPrefix(line, "копирование данных:") {
			continue // своими экранами прогресса не засоряем хвост лога
		}
		sink(runEvent{Type: "log", Line: line})
	}
}

// driverExitMeaning — код возврата драйвера → (код, объяснение).
//
// Числа те же, что у scripts/gguf_loop_meta_patch.py: менять один без другого
// нельзя, поэтому соответствие продублировано здесь явно, а не выведено «по
// смыслу».
func driverExitMeaning(err error) (string, string) {
	code := 1
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	switch code {
	case 2:
		return "input_missing", "источник или каталог результата недоступен (нет файла, нет прав, файл уже есть — см. force)"
	case 3:
		return "model_not_patchable", "файл не подходит под патч: make_plan не нашёл loop-схему (см. хвост лога)"
	case 4:
		return "insufficient_space", "недостаточно свободного места для результата (см. хвост лога)"
	case 5:
		return "dst_exists", "результат уже существует: включите force, чтобы перезаписать"
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return "converter_failed", "конвертер завершился с кодом " + strconv.Itoa(ee.ExitCode())
	}
	return "", ""
}
