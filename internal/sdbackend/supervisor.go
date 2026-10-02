package sdbackend

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Супервизор субпроцесса sd-server
// ============================================================
//
// КЛЮЧЕВОЙ ФАКТ (docs/research-sdcpp-lowvram-integration.md §4.5): у sd-server
// НЕТ hot-swap и НЕТ эндпоинта смены модели. sd_ctx создаётся ОДИН РАЗ в
// main() до listen(), все хендлеры держат указатель на него. Поэтому:
//
//	load   = spawn процесса с argv профиля (+ --listen-ip/--listen-port)
//	unload = kill процесса (SIGTERM → ожидание → hard kill)
//	reload = kill + spawn
//
// Смена модели = рестарт; «загружено» может быть ровно одно состояние —
// процесс либо есть, либо нет. Отсюда вся модель состояний воркера.

// processOutputLimit — сколько хвоста stdout/stderr держим для диагностики.
//
// 64 KiB: сообщение об ошибке флага/модели у движка короткое, а вот лог
// загрузки GGUF бывает длинным. Хвост нужен, чтобы показать оператору
// «почему упало» без вычитывания docker logs.
const processOutputLimit = 64 * 1024

// stopGracePeriod — сколько ждём корректного завершения после сигнала.
const stopGracePeriod = 5 * time.Second

// Process — запущенный субпроцесс (интерфейс ради тестируемости).
//
// ЗАЧЕМ ИНТЕРФЕЙС: запускать sd-server в unit-тестах нельзя (его нет в
// окружении сборки, а падение процесса — не детерминировано). ProcessRunner
// подменяется фейком, который «поднимает» httptest-сервер с мок-движком.
type Process interface {
	PID() int
	// Wait блокируется до выхода; возвращает ExitError, если код != 0.
	Wait() error
	// Stop — корректное завершение (SIGTERM / Kill на Windows) + ожидание.
	Stop(grace time.Duration) error
	// Kill — немедленное уничтожение.
	Kill() error
	// Output — накопленный хвост stdout/stderr.
	Output() string
}

// ProcessRunner запускает субпроцесс.
type ProcessRunner interface {
	Start(ctx context.Context, argv []string) (Process, error)
}

// ExecProcessRunner — боевая реализации через os/exec.
type ExecProcessRunner struct{}

// Start запускает argv. Логи построчно уходят в zap с префиксом, а также
// копятся в кольцевом буфере для диагностики readiness/exit.
func (ExecProcessRunner) Start(ctx context.Context, argv []string) (Process, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty argv")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	// Отдельная process group НЕ создаётся: sd-server — одиночный бинарь без
	// дочерних процессов, а на Windows Process.Kill() иначе не убьёт дерево.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &execProcess{cmd: cmd, prefix: filepath.Base(argv[0])}
	p.done = make(chan struct{})
	go p.pump(stdout, "out")
	go p.pump(stderr, "err")
	go func() {
		p.waitErr = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// execProcess — обёртка над *exec.Cmd с буферизацией вывода.
type execProcess struct {
	cmd     *exec.Cmd
	prefix  string
	mu      sync.Mutex
	buf     []byte
	waitErr error
	done    chan struct{}
	stopped bool
}

func (p *execProcess) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// pump построчно читает поток и складывает в буфер.
//
// Построчно (а не ReadAll): строки — естественная единица для лога движка
// («failed to allocate compute buffer»), и они не склеиваются в кашу.
// Каждая строка уходит в глобальный лог с префиксом sd-server[out|err], чтобы
// OOM/падения были видны в docker logs без открытия файлов.
func (p *execProcess) pump(r io.Reader, stream string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		p.appendOutput("[" + stream + "] " + line)
		sdLog().Debugw("sd-server output", "stream", stream, "line", line)
	}
}

// appendOutput добавляет строку в кольцевой буфер.
func (p *execProcess) appendOutput(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, line...)
	p.buf = append(p.buf, '\n')
	if len(p.buf) > processOutputLimit {
		p.buf = p.buf[len(p.buf)-processOutputLimit:]
	}
}

func (p *execProcess) Output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.TrimSpace(string(p.buf))
}

// Wait — ожидание выхода процесса.
func (p *execProcess) Wait() error {
	<-p.done
	return p.waitErr
}

// Stop — мягкое завершение + ожидание grace, затем hard kill.
//
// Почему SIGTERM первым: sd-server корректно освобождает VRAM/файлы; hard kill
// на Vulkan-драйвере (Mesa RADV) иногда оставляет устройство в «залипшем»
// состоянии, а следующий spawn падает на инициализации.
func (p *execProcess) Stop(grace time.Duration) error {
	if p.cmd.Process == nil {
		return nil
	}
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil
	}
	p.stopped = true
	p.mu.Unlock()

	_ = p.cmd.Process.Signal(os.Interrupt) // SIGINT/SIGTERM-совместимый путь
	select {
	case <-p.done:
		return p.waitErr
	case <-time.After(grace):
	}
	// Не завершился — жёстко.
	_ = p.cmd.Process.Kill()
	select {
	case <-p.done:
	case <-time.After(grace):
	}
	return p.waitErr
}

func (p *execProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

// ============================================================
// Supervisor
// ============================================================

// Supervisor управляет единственным активным sd-server.
type Supervisor struct {
	cfg      *Config
	registry *Registry
	metrics  *Metrics
	runner   ProcessRunner
	// clientFactory создаёт клиента движка по порту; подменяется в тестах
	// (там фиктивный процесс поднимает httptest-сервер на случайном порту).
	clientFactory func(ip string, port int) *SDServerClient
	// clientFactoryAddr — вариант с полным base URL (используется, когда порт
	// движка заранее неизвестен — например httptest-мок).
	clientFactoryAddr func(baseURL string) *SDServerClient
	// readinessTimeout — сколько ждать GET /sdcpp/v1/capabilities после spawn.
	readinessTimeout time.Duration
	// readinessPoll — интервал опроса readiness.
	readinessPoll time.Duration
	// PollEvery — интервал опроса джоб движка (в тестах 2 мс).
	PollEvery time.Duration

	mu       sync.Mutex
	state    string
	proc     Process
	client   *SDServerClient
	loaded   string // current model name ("" = none)
	prof     *types.ImageModelProfile
	loadErr  error
	loading  bool
	loadingModel string
	gen      uint64 // «поколение» процесса: инвалидирует отменённые загрузки
	inFlight uint64 // активные генерации (для idle-unload и метрик)
	caps     *Capabilities
}

// NewSupervisor — супервизор с боевыми зависимостями.
func NewSupervisor(cfg *Config, registry *Registry, metrics *Metrics) *Supervisor {
	return &Supervisor{
		cfg:              cfg,
		registry:         registry,
		metrics:          metrics,
		runner:           ExecProcessRunner{},
		clientFactory:    NewSDServerClient,
		readinessTimeout: time.Duration(cfg.StartupTimeoutSec) * time.Second,
		readinessPoll:    750 * time.Millisecond,
		PollEvery:        500 * time.Millisecond,
		state:            StateNotLoaded,
	}
}

// SetRunner — подмена раннера (тесты).
func (s *Supervisor) SetRunner(r ProcessRunner) { s.runner = r }

// SetClientFactory — подмена фабрики клиента (тесты).
func (s *Supervisor) SetClientFactory(f func(ip string, port int) *SDServerClient) {
	s.clientFactory = f
}

// SetClientFactoryAddr — подмена фабрики клиента по base URL (тесты с моком на
// случайном порту).
func (s *Supervisor) SetClientFactoryAddr(f func(baseURL string) *SDServerClient) {
	s.clientFactoryAddr = f
}

// SetSDServerBaseURL — направить супервизор на конкретный адрес движка.
//
// Нужно интеграционным тестам (и сценарию «sd-server на другом хосте»): порт в
// argv всё равно объявляется из конфига, но HTTP-клиент идёт по этому URL.
func (s *Supervisor) SetSDServerBaseURL(baseURL string) {
	s.clientFactoryAddr = func(string) *SDServerClient { return NewSDServerClientURL(baseURL) }
}

// newClient — клиент движка для текущего процесса.
func (s *Supervisor) newClient() *SDServerClient {
	if s.clientFactoryAddr != nil {
		return s.clientFactoryAddr(fmt.Sprintf("http://%s:%d", s.cfg.ListenIP, s.cfg.ServerPort))
	}
	return s.clientFactory(s.cfg.ListenIP, s.cfg.ServerPort)
}

// SetReadinessTimeout — таймаут readiness (тесты/оператор).
func (s *Supervisor) SetReadinessTimeout(d time.Duration) { s.readinessTimeout = d }

// SetReadinessPoll — интервал опроса readiness (тесты).
func (s *Supervisor) SetReadinessPoll(d time.Duration) { s.readinessPoll = d }

// SetPollEvery — интервал опроса статуса джобы (тесты).
func (s *Supervisor) SetPollEvery(d time.Duration) { s.PollEvery = d }

// Client — клиент активного процесса (nil, если процесс не поднят).
func (s *Supervisor) Client() *SDServerClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

// State — текущее состояние.
func (s *Supervisor) State() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// CurrentModel — имя загруженной модели ("" = нет).
func (s *Supervisor) CurrentModel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loaded
}

// PID — pid субпроцесса (0 = нет процесса).
func (s *Supervisor) PID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc == nil {
		return 0
	}
	return s.proc.PID()
}

// Capabilities — снимок capabilities активного движка (может быть nil).
func (s *Supervisor) Capabilities() *Capabilities {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.caps
}

// LastError — последняя ошибка загрузки/процесса.
func (s *Supervisor) LastError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr == nil {
		return ""
	}
	return s.loadErr.Error()
}

// InFlight — сколько генераций сейчас выполняется (включая ожидание движка).
func (s *Supervisor) InFlight() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(s.inFlight)
}

// BeginRequest / EndRequest — учёт активных запросов воркера.
//
// Нужен idle-unload: выгружать процесс, пока идёт генерация, нельзя —
// повторный spawn конкурирует за ту же VRAM и за тот же порт.
func (s *Supervisor) BeginRequest() {
	s.mu.Lock()
	s.inFlight++
	s.mu.Unlock()
	if s.metrics != nil {
		s.metrics.ActiveGenerations.Add(1)
	}
}

// EndRequest — завершение активного запроса.
func (s *Supervisor) EndRequest() {
	s.mu.Lock()
	if s.inFlight > 0 {
		s.inFlight--
	}
	s.mu.Unlock()
	if s.metrics != nil {
		s.metrics.ActiveGenerations.Add(-1)
	}
}

// ============================================================
// Load / Unload / Reload
// ============================================================

// LoadOptions — параметры загрузки.
type LoadOptions struct {
	// ReadinessTimeout переопределяет конфиг (0 = из конфига).
	ReadinessTimeout time.Duration
	// Force — перезапустить процесс, даже если модель уже загружена
	// (используется кнопкой «Перезагрузить»/reload).
	Force bool
}

// Load поднимает sd-server с указанной моделью.
//
// Поведение:
//   - та же модель и процесс жив → идемпотентный no-op (повторный load из UI
//     или балансера не должен ронять работающий движок);
//   - другая модель → kill + spawn (hot-swap невозможен);
//   - параллельные Load на одну модель → single-flight: второй ждёт первого
//     и получает тот же результат (без двух sd-server на одном порту).
func (s *Supervisor) Load(ctx context.Context, name string, opts LoadOptions) (*Capabilities, error) {
	s.mu.Lock()
	if s.state == StateLoaded && s.loaded == name && s.proc != nil && !opts.Force {
		caps := s.caps
		s.mu.Unlock()
		return caps, nil
	}
	if s.loading {
		// Кто-то уже грузит. Если это другая модель — отдаём ErrLoading:
		// ждать «чужую» загрузку и получить не ту модель хуже, чем явная
		// ошибка «занят» (балансер повторит).
		loadingName := s.loadingName()
		s.mu.Unlock()
		if loadingName == name {
			if err := s.waitLoading(ctx); err != nil {
				return nil, err
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.caps, s.loadErr
		}
		return nil, fmt.Errorf("%w (loading %q → %q)", ErrLoading, loadingName, name)
	}
	gen := s.gen + 1
	s.gen = gen
	s.loading = true
	s.loadingModel = name
	s.loadErr = nil
	s.state = StateLoading
	prev := s.proc
	// Отпускаем процесс прошлой модели (если был) ДО spawn нового, но уже под
	// флагом loading=true: параллельный Load увидит «занят» и не полезет.
	s.proc, s.client, s.caps, s.loaded, s.prof = nil, nil, nil, "", nil
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.loading = false
		s.loadingModel = ""
		s.mu.Unlock()
	}()

	if prev != nil {
		_ = prev.Stop(stopGracePeriod)
		if s.metrics != nil {
			s.metrics.MarkUnloaded()
		}
	}

	profile, ok := s.registry.Profile(name)
	if !ok {
		err := fmt.Errorf("%w: %q", ErrModelNotFound, name)
		s.failLoad(err)
		return nil, err
	}
	if profile.Disabled {
		err := fmt.Errorf("image model %q is disabled in its profile", name)
		s.failLoad(err)
		return nil, err
	}

	// Порт должен быть свободен: после Stop процесс мог ещё не отпустить
	// сокет (TIME_WAIT/драйвер), а bind-ошибка выглядит как «модель не грузится».
	if err := s.waitPortFree(ctx, 5*time.Second); err != nil {
		s.failLoad(err)
		return nil, err
	}

	if err := s.writeSidecar(&profile); err != nil {
		// Не фатально: sidecar — для оркестратора, движку он не нужен.
		sdLog().Warnw("failed to write sd-server sidecar config", "model", name, "error", err)
	}

	argv := s.buildArgv(&profile)
	sdLog().Infow("spawning sd-server",
		"model", name, "bin", s.cfg.SDServerBin, "pid_port", s.cfg.ServerPort,
		"argv", strings.Join(argv, " "), "pinned_revision", types.PinnedSDServerRevision)

	proc, err := s.runner.Start(ctx, argv)
	if err != nil {
		err = &StartupError{Bin: s.cfg.SDServerBin, ExitCode: -1, Reason: "spawn failed: " + err.Error()}
		s.failLoad(err)
		return nil, err
	}

	client := s.newClient()
	timeout := opts.ReadinessTimeout
	if timeout <= 0 {
		timeout = s.readinessTimeout
	}
	caps, err := s.waitReady(ctx, proc, client, timeout)
	if err != nil {
		output := proc.Output()
		exitCode := -1
		_ = proc.Stop(stopGracePeriod)
		var waitErr error
		if we := proc.Wait(); we != nil {
			exitCode = exitCodeOf(we)
			waitErr = we
		}
		var final error
		if waitErr != nil {
			final = classifySpawnError(s.cfg.SDServerBin, exitCode, output, err)
		} else {
			final = &StartupError{Bin: s.cfg.SDServerBin, ExitCode: exitCode,
				Reason: err.Error(), Output: output}
		}
		s.failLoad(final)
		return nil, final
	}

	s.mu.Lock()
	s.proc, s.client, s.caps, s.loaded, s.prof, s.state = proc, client, caps, name, &profile, StateLoaded
	s.mu.Unlock()
	if s.metrics != nil {
		s.metrics.MarkLoaded()
	}

	// Следим за процессом: если sd-server умрёт сам (OOM, segfault VAE),
	// состояние обязано стать error — иначе воркер будет считать модель
	// загруженной и слать запросы в мёртвый порт.
	go s.watch(proc, gen, name)

	return caps, nil
}

// waitReady — поллинг GET /sdcpp/v1/capabilities до успеха/смерти процесса.
//
// Почему capabilities, а не /health: у sd-server НЕТ /health (проверено по
// examples/server/api.md). Capabilities — единственный дешёвый GET, который
// подтверждает и «процесс слушает», и «модель загружена» (в ответе есть
// model.stem/path).
func (s *Supervisor) waitReady(ctx context.Context, proc Process, client *SDServerClient, timeout time.Duration) (*Capabilities, error) {
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	deadline := time.Now().Add(timeout)
	poll := s.readinessPoll
	if poll <= 0 {
		poll = 750 * time.Millisecond
	}
	var lastErr error
	for {
		// Сначала — жив ли процесс: если он уже упал, поллинг бессмысленен,
		// а ошибка должна быть про флаги/модель, а не про таймаут.
		if exited(proc) {
			return nil, fmt.Errorf("process exited before readiness (last probe error: %v)", lastErr)
		}
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		caps, err := client.Capabilities(probeCtx)
		cancel()
		if err == nil {
			return caps, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("readiness timeout after %s: %w", timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// watch отслеживает неожиданную смерть процесса.
func (s *Supervisor) watch(proc Process, gen uint64, name string) {
	err := proc.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen != gen || s.proc != proc {
		return // процесс заменён/выгружен намеренно — это не «смерть»
	}
	s.proc, s.client, s.caps, s.loaded, s.prof = nil, nil, nil, "", nil
	if err != nil {
		s.state = StateError
		s.loadErr = &StartupError{Bin: s.cfg.SDServerBin, ExitCode: exitCodeOf(err),
			Reason: "sd-server exited unexpectedly", Output: proc.Output()}
	} else {
		// Код 0 без нашего unload: движок закрылся сам (например `--help`
		// или внешний kill). Это тоже ошибка состояния.
		s.state = StateError
		s.loadErr = fmt.Errorf("sd-server for model %q exited with code 0 unexpectedly", name)
	}
	if s.metrics != nil {
		s.metrics.MarkUnloaded()
		s.metrics.MarkError(s.loadErr)
	}
	sdLog().Errorw("sd-server exited unexpectedly",
		"model", name, "exit_error", err, "output_tail", truncate(proc.Output(), 4000))
}

// Unload — kill субпроцесса и освобождение порта.
func (s *Supervisor) Unload(ctx context.Context) error {
	s.mu.Lock()
	proc := s.proc
	name := s.loaded
	s.gen++ // инвалидируем watch: это НАМЕРЕННАЯ остановка
	s.proc, s.client, s.caps, s.loaded, s.prof = nil, nil, nil, "", nil
	s.state = StateNotLoaded
	s.loadErr = nil
	s.mu.Unlock()

	if proc == nil {
		return nil
	}
	sdLog().Infow("unloading sd-server", "model", name, "pid", proc.PID())
	err := proc.Stop(stopGracePeriod)
	if s.metrics != nil {
		s.metrics.MarkUnloaded()
	}
	// Ждём, пока порт реально освободится: иначе следующий spawn упадёт на
	// bind, и это будет выглядеть как «модель не грузится».
	if portErr := s.waitPortFree(ctx, 5*time.Second); portErr != nil {
		sdLog().Warnw("port still busy after unload", "port", s.cfg.ServerPort, "error", portErr)
	}
	return err
}

// Reload — kill + spawn той же модели (кнопка «Перезагрузить»).
func (s *Supervisor) Reload(ctx context.Context, name string) (*Capabilities, error) {
	if err := s.Unload(ctx); err != nil {
		sdLog().Warnw("reload: unload failed, continuing with spawn", "model", name, "error", err)
	}
	return s.Load(ctx, name, LoadOptions{Force: true})
}

// failLoad фиксирует ошибку загрузки в состоянии.
func (s *Supervisor) failLoad(err error) {
	s.mu.Lock()
	s.state = StateError
	s.loadErr = err
	s.mu.Unlock()
	if s.metrics != nil {
		s.metrics.MarkError(err)
	}
	sdLog().Errorw("image model load failed", "error", err)
}

// loadingName — имя модели, которая грузится сейчас (для сообщения об ошибке).
func (s *Supervisor) loadingName() string { return s.loadingModel }

// waitLoading — ожидание завершения чужой загрузки (single-flight).
func (s *Supervisor) waitLoading(ctx context.Context) error {
	for {
		s.mu.Lock()
		loading := s.loading
		s.mu.Unlock()
		if !loading {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// buildArgv — argv sd-server: ServerArgs профиля + наши сетевые флаги.
//
// ЧТО НЕ ДУБЛИРУЕМ: файлы/плейсмент/скорость/дефолты собирает
// types.ImageModelProfile.ServerArgs() — это замороженный контракт, и его
// порядок аргументов проверяется своими тестами. Воркер добавляет только то,
// чего в профиле быть не может (привязка к порту) и каталоги уровня воркера.
func (s *Supervisor) buildArgv(p *types.ImageModelProfile) []string {
	argv := []string{s.cfg.SDServerBin}
	argv = append(argv, p.ServerArgs()...)
	// --listen-ip/--listen-port — ТОЛЬКО loopback по умолчанию: движок не
	// должен быть доступен извне воркера, иначе клиенты обойдут нормализацию
	// seed/размеров (и получат дефолтный seed 42 — «все картинки одинаковые»).
	argv = append(argv, "--listen-ip", listenIP(s.cfg.ListenIP), "--listen-port", fmt.Sprintf("%d", s.cfg.ServerPort))
	if s.cfg.LoraModelDir != "" {
		argv = append(argv, "--lora-model-dir", s.cfg.LoraModelDir)
	}
	if s.cfg.HiresUpscalersDir != "" {
		argv = append(argv, "--hires-upscalers-dir", s.cfg.HiresUpscalersDir)
	}
	if len(s.cfg.ExtraArgs) > 0 {
		argv = append(argv, s.cfg.ExtraArgs...)
	}
	return argv
}

// listenIP — адрес прослушивания движка.
//
// Если оператор выставил в конфиге 0.0.0.0, подменяем на 127.0.0.1: наружу
// обязан торчать ТОЛЬКО API воркера. Иначе seed/размеры не нормализуются.
func listenIP(configured string) string {
	switch strings.TrimSpace(configured) {
	case "", "0.0.0.0", "::", "[::]":
		return "127.0.0.1"
	}
	return configured
}

// writeSidecar сохраняет profile.json движка рядом с bundle'ом.
//
// Зачем: (1) запись «что именно мы запустили» для оркестратора/UI;
// (2) sd-server умеет читать JSON-конфиг (--config) — если оператор захочет
// запустить тот же инстанс вручную, файл уже лежит рядом с моделью.
func (s *Supervisor) writeSidecar(p *types.ImageModelProfile) error {
	sidecar := struct {
		Model         string                 `json:"model"`
		Family        string                 `json:"family"`
		ProfileSource string                 `json:"profile_source"`
		Defaults      types.ImageGenDefaults `json:"defaults"`
		Runtime       types.ImageRuntime     `json:"runtime"`
		SeedMode      string                 `json:"seed_mode"`
	}{
		Model: p.Name, Family: p.Family, ProfileSource: ProfileFileName,
		Defaults: p.Defaults, Runtime: p.Runtime, SeedMode: seedModeOf(p),
	}
	data, err := json.MarshalIndent(sidecar, "", "  ")
	if err != nil {
		return err
	}
	dir := p.BundleDir(s.registry.ModelsDir())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "sd-server.config.json"), data, 0o644)
}

// waitPortFree — ждёт, пока порт движка перестанет быть занятым.
func (s *Supervisor) waitPortFree(ctx context.Context, timeout time.Duration) error {
	addr := net.JoinHostPort(listenIP(s.cfg.ListenIP), fmt.Sprintf("%d", s.cfg.ServerPort))
	deadline := time.Now().Add(timeout)
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			_ = ln.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("port %s is still busy after %s: %w", addr, timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// ============================================================
// Вспомогательные функции процесса
// ============================================================

// exited — завершился ли процесс (не блокируясь).
func exited(p Process) bool {
	if ep, ok := p.(*execProcess); ok {
		select {
		case <-ep.done:
			return true
		default:
			return false
		}
	}
	if fp, ok := p.(interface{ Exited() bool }); ok {
		return fp.Exited()
	}
	return false
}

// exitCodeOf — код возврата из ошибки Wait.
func exitCodeOf(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err == nil {
		return 0
	}
	return -1
}

// flagIncompatiblePatterns — признаки того, что бинарь не понял argv.
//
// ЯЗЫКОНЕЗАВИСИМО (важно): sd.cpp использует CLI11, который на неизвестный
// флаг печатает "Could not convert"/"is not a valid argument" или
// "unknown option"; git-версии ggml-common печатают "unrecognized argument".
// Точный текст — не контракт, поэтому список широкий, а НЕГАТИВНЫЙ исход
// (обычный StartupError) остаётся корректным: мы не превращаем любую ошибку
// в «проверьте версию», только явные признаки парсинга флагов.
var flagIncompatiblePatterns = []string{
	"unknown option",
	"unknown argument",
	"unrecognized",
	"unrecognised",
	"is not a valid argument",
	"could not convert",
	"invalid argument",
	"flag provided but not defined",
	"no such option",
	"--help",
	"usage:",
}

// classifySpawnError — StartupError или FlagIncompatibleError.
//
// ЗАЧЕМ: задача прямо требует, чтобы падение бинаря на неизвестном флаге
// (типовой случай при смене версии sd.cpp) давало ошибку с указанием
// проверить версию и напоминанием про types.PinnedSDServerRevision.
func classifySpawnError(bin string, exitCode int, output string, cause error) error {
	low := strings.ToLower(output)
	for _, pat := range flagIncompatiblePatterns {
		if strings.Contains(low, pat) {
			return &FlagIncompatibleError{
				Bin: bin, ExitCode: exitCode, Output: truncate(output, 2000),
				Revision: types.PinnedSDServerRevision,
			}
		}
	}
	reason := "process exited before readiness"
	if cause != nil {
		reason = cause.Error()
	}
	return &StartupError{Bin: bin, ExitCode: exitCode, Reason: reason, Output: truncate(output, 2000)}
}
