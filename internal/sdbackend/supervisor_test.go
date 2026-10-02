package sdbackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Тесты супервизора (без реального sd-server)
// ============================================================
//
// СХЕМА: FakeRunner возвращает FakeProcess, который «поднимает» httptest-мок на
// СЛУЧАЙНОМ порту; супервизор при этом думает, что процесс слушает cfg.ServerPort.
// Чтобы readiness прошёл, подменяем фабрику клиента (SetClientFactoryAddr) —
// это единственная подмена, весь остальной путь (поллинг, классификация
// ошибок, kill, состояние) проверяется настоящий.

// fakeBackend — адрес мок-движка, к которому ходит супервизор.
//
// ПОЧЕМУ ОТДЕЛЬНЫЙ ТИП: супервизор формирует URL из cfg.ListenIP:cfg.ServerPort,
// а httptest-мок слушает случайный порт. Подменой фабрики клиента мы «переключаем»
// супервизор на реально слушающий мок, не ослабляя проверку readiness — она
// проходит настоящий HTTP-цикл (поллинг, 503 «model is loading», таймауты).
type fakeBackend struct {
	baseURL string
}

// newFakeBackend поднимает мок и возвращает его адрес.
func newFakeBackend(t *testing.T) *fakeBackend {
	t.Helper()
	fake := newFakeServer()
	t.Cleanup(fake.Close)
	return &fakeBackend{baseURL: fake.URL}
}

// newSupervisorWithFake — супервизор, чей процесс «слушает» мок.
func newSupervisorWithFake(t *testing.T, models map[string]types.ImageModelProfile) (*Supervisor, *Registry, *FakeRunner) {
	t.Helper()
	dir := t.TempDir()
	reg := NewRegistry(dir)
	// Пишем профили как profile.json в подкаталогах — так же, как на диске.
	for name, p := range models {
		sub := filepath.Join(dir, name)
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		p.Name = name
		for i := range p.Files {
			if p.Files[i].LocalPath == "" {
				p.Files[i].LocalPath = filepath.Join(sub, p.Files[i].Filename)
			}
		}
		data, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal profile: %v", err)
		}
		if err := os.WriteFile(filepath.Join(sub, ProfileFileName), data, 0o644); err != nil {
			t.Fatalf("write profile: %v", err)
		}
	}
	if err := reg.Load(); err != nil {
		t.Fatalf("registry load: %v", err)
	}

	cfg := DefaultConfig()
	cfg.ModelsDir = dir
	cfg.ListenIP = "127.0.0.1"
	// Свободный порт, чтобы waitPortFree не конфликтовал с другими тестами.
	cfg.ServerPort = freePort(t)
	cfg.StartupTimeoutSec = 10

	fake := newFakeBackend(t)
	sup := NewSupervisor(&cfg, reg, NewMetrics())
	sup.SetReadinessPoll(5 * time.Millisecond)
	sup.SetPollEvery(2 * time.Millisecond)
	sup.SetReadinessTimeout(3 * time.Second)
	sup.SetClientFactoryAddr(func(string) *SDServerClient {
		return NewSDServerClientURL(fake.baseURL)
	})

	runner := NewFakeRunner(func(argv []string) (Process, error) {
		p := NewFakeProcess(4242, nil)
		p.AppendOutput("[out] sd-server starting")
		return p, nil
	})
	sup.SetRunner(runner)
	return sup, reg, runner
}

// freePort — свободный TCP-порт.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func simpleProfile(name string) types.ImageModelProfile {
	return types.ImageModelProfile{
		Name:   name,
		Family: "sd15",
		Files: []types.ImageModelFile{
			{Role: types.ImageFileRoleDiffusion, Repo: "local", Filename: "model.gguf"},
		},
		Defaults: types.DefaultImageGenDefaults("sd15"),
	}
}

// --- 1. Успешная загрузка -----------------------------------------------------

func TestSupervisor_Load_Success(t *testing.T) {
	sup, _, runner := newSupervisorWithFake(t, map[string]types.ImageModelProfile{
		"sd15-test": simpleProfile("sd15-test"),
	})

	ctx := context.Background()
	caps, err := sup.Load(ctx, "sd15-test", LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if caps == nil || caps.Limits.MaxWidth != 4096 {
		t.Fatalf("capabilities not read: %+v", caps)
	}
	if sup.State() != StateLoaded {
		t.Fatalf("state = %q, want %q", sup.State(), StateLoaded)
	}
	if sup.CurrentModel() != "sd15-test" {
		t.Fatalf("model = %q", sup.CurrentModel())
	}
	if sup.PID() != 4242 {
		t.Fatalf("pid = %d", sup.PID())
	}
	if runner.Started() != 1 {
		t.Fatalf("spawn count = %d, want 1", runner.Started())
	}
}

// --- 2. argv: ServerArgs профиля + наши сетевые флаги -------------------------

func TestSupervisor_Argv_ContainsNetworkFlagsAndProfileArgs(t *testing.T) {
	profile := simpleProfile("sd15-test")
	profile.Runtime.VaeTiling = true
	profile.Runtime.Threads = 6
	sup, _, runner := newSupervisorWithFake(t, map[string]types.ImageModelProfile{"sd15-test": profile})

	if _, err := sup.Load(context.Background(), "sd15-test", LoadOptions{}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	argvs := runner.Argvs()
	if len(argvs) != 1 {
		t.Fatalf("argv count = %d", len(argvs))
	}
	argv := strings.Join(argvs[0], " ")
	for _, want := range []string{
		"--model", "--vae-tiling", "--threads 6",
		"--listen-ip 127.0.0.1",
		fmt.Sprintf("--listen-port %d", sup.cfg.ServerPort),
		// ServerArgs ставит --seed -1 при SeedIsRandom (защита от дефолта 42).
		"--seed -1",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv missing %q: %s", want, argv)
		}
	}
}

// --- 3. Повторный load той же модели идемпотентен ----------------------------

func TestSupervisor_Load_Idempotent(t *testing.T) {
	sup, _, runner := newSupervisorWithFake(t, map[string]types.ImageModelProfile{
		"sd15-test": simpleProfile("sd15-test"),
	})
	ctx := context.Background()
	if _, err := sup.Load(ctx, "sd15-test", LoadOptions{}); err != nil {
		t.Fatalf("first load: %v", err)
	}
	pid := sup.PID()
	if _, err := sup.Load(ctx, "sd15-test", LoadOptions{}); err != nil {
		t.Fatalf("second load: %v", err)
	}
	if runner.Started() != 1 {
		t.Fatalf("second Load respawned the process (started=%d) — идемпотентность нарушена", runner.Started())
	}
	if sup.PID() != pid {
		t.Fatalf("pid changed: %d → %d", pid, sup.PID())
	}
}

// --- 4. Смена модели = kill + spawn ------------------------------------------

func TestSupervisor_Load_ModelSwitchRestarts(t *testing.T) {
	sup, _, runner := newSupervisorWithFake(t, map[string]types.ImageModelProfile{
		"a": simpleProfile("a"),
		"b": simpleProfile("b"),
	})
	ctx := context.Background()
	if _, err := sup.Load(ctx, "a", LoadOptions{}); err != nil {
		t.Fatalf("load a: %v", err)
	}
	if _, err := sup.Load(ctx, "b", LoadOptions{}); err != nil {
		t.Fatalf("load b: %v", err)
	}
	if runner.Started() != 2 {
		t.Fatalf("started=%d, want 2 (смена модели обязана быть kill+spawn)", runner.Started())
	}
	if sup.CurrentModel() != "b" {
		t.Fatalf("current = %q", sup.CurrentModel())
	}
}

// --- 5. Падение на неизвестном флаге → понятная ошибка про версию ------------

func TestSupervisor_Load_FlagIncompatibleError(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry(dir)
	writeProfile(t, dir, simpleProfile("m"))
	if err := reg.Load(); err != nil {
		t.Fatalf("registry: %v", err)
	}
	cfg := DefaultConfig()
	cfg.ModelsDir = dir
	cfg.ServerPort = freePort(t)
	cfg.StartupTimeoutSec = 5
	cfg.SDServerBin = "sd-server-new-build"
	sup := NewSupervisor(&cfg, reg, NewMetrics())
	sup.SetReadinessPoll(2 * time.Millisecond)
	sup.SetReadinessTimeout(2 * time.Second)
	sup.SetClientFactoryAddr(func(string) *SDServerClient {
		// Ничего не слушает: readiness упадёт, а процесс «умрёт» сам.
		return NewSDServerClientURL("http://127.0.0.1:1")
	})
	sup.SetRunner(NewFakeRunner(func(argv []string) (Process, error) {
		p := NewFakeProcess(1, nil)
		p.AppendOutput("[err] Could not convert: --listen-ip: unknown option")
		// Процесс падает сразу, как CLI11 при неизвестном флаге.
		go func() {
			time.Sleep(10 * time.Millisecond)
			p.Crash(errors.New("exit status 1"))
		}()
		return p, nil
	}))

	_, err := sup.Load(context.Background(), "m", LoadOptions{})
	if err == nil {
		t.Fatal("expected error")
	}
	var fi *FlagIncompatibleError
	if !errors.As(err, &fi) {
		t.Fatalf("error type = %T (%v), want *FlagIncompatibleError", err, err)
	}
	for _, want := range []string{types.PinnedSDServerRevision, "проверьте версию sd-server"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error text must mention %q: %s", want, err.Error())
		}
	}
	if sup.State() != StateError {
		t.Fatalf("state = %q, want %q", sup.State(), StateError)
	}
}

// --- 6. Readiness-таймаут → StartupError (не про версию) ---------------------

func TestSupervisor_Load_ReadinessTimeout(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry(dir)
	writeProfile(t, dir, simpleProfile("m"))
	if err := reg.Load(); err != nil {
		t.Fatalf("registry: %v", err)
	}
	cfg := DefaultConfig()
	cfg.ModelsDir = dir
	cfg.ServerPort = freePort(t)
	sup := NewSupervisor(&cfg, reg, NewMetrics())
	sup.SetReadinessPoll(2 * time.Millisecond)
	sup.SetClientFactoryAddr(func(string) *SDServerClient {
		return NewSDServerClientURL("http://127.0.0.1:1")
	})
	sup.SetRunner(NewFakeRunner(func([]string) (Process, error) {
		p := NewFakeProcess(7, nil)
		p.AppendOutput("[out] loading model…")
		return p, nil
	}))

	_, err := sup.Load(context.Background(), "m", LoadOptions{ReadinessTimeout: 150 * time.Millisecond})
	if err == nil {
		t.Fatal("expected readiness timeout")
	}
	var se *StartupError
	if !errors.As(err, &se) {
		t.Fatalf("error type = %T, want *StartupError: %v", err, err)
	}
	if strings.Contains(err.Error(), "проверьте версию") {
		t.Errorf("readiness timeout не должен выглядеть как несовместимость флагов: %v", err)
	}
	if sup.State() != StateError {
		t.Fatalf("state = %q", sup.State())
	}
}

// --- 7. Unload: состояние и повторный spawn ---------------------------------

func TestSupervisor_Unload(t *testing.T) {
	sup, _, runner := newSupervisorWithFake(t, map[string]types.ImageModelProfile{
		"sd15-test": simpleProfile("sd15-test"),
	})
	ctx := context.Background()
	if _, err := sup.Load(ctx, "sd15-test", LoadOptions{}); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := sup.Unload(ctx); err != nil {
		t.Fatalf("unload: %v", err)
	}
	if sup.State() != StateNotLoaded {
		t.Fatalf("state = %q, want %q", sup.State(), StateNotLoaded)
	}
	if sup.PID() != 0 || sup.CurrentModel() != "" {
		t.Fatalf("pid=%d model=%q", sup.PID(), sup.CurrentModel())
	}
	// Повторный unload — no-op без ошибок.
	if err := sup.Unload(ctx); err != nil {
		t.Fatalf("second unload: %v", err)
	}
	if runner.Started() != 1 {
		t.Fatalf("unload не должен был спавнить процесс")
	}
}

// --- 8. Внезапная смерть процесса → state=error ------------------------------

func TestSupervisor_UnexpectedExit_SetsErrorState(t *testing.T) {
	sup, _, runner := newSupervisorWithFake(t, map[string]types.ImageModelProfile{
		"sd15-test": simpleProfile("sd15-test"),
	})
	ctx := context.Background()
	if _, err := sup.Load(ctx, "sd15-test", LoadOptions{}); err != nil {
		t.Fatalf("load: %v", err)
	}
	// Забираем процесс из раннера и «роняем» его (эмуляция OOM VAE).
	fp := runner.lastProcess()
	if fp == nil {
		t.Fatal("no process recorded")
	}
	fp.Crash(errors.New("exit status 137"))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sup.State() == StateError {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if sup.State() != StateError {
		t.Fatalf("state = %q, want %q (смерть процесса обязана переводить в error)", sup.State(), StateError)
	}
	if !strings.Contains(sup.LastError(), "exited unexpectedly") {
		t.Fatalf("last error = %q", sup.LastError())
	}
}

// --- 9. Нет модели в реестре → ErrModelNotFound ------------------------------

func TestSupervisor_Load_UnknownModel(t *testing.T) {
	sup, _, _ := newSupervisorWithFake(t, map[string]types.ImageModelProfile{
		"sd15-test": simpleProfile("sd15-test"),
	})
	_, err := sup.Load(context.Background(), "nope", LoadOptions{})
	if !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("err = %v, want ErrModelNotFound", err)
	}
}

// --- 10. sidecar-конфиг пишется ----------------------------------------------

func TestSupervisor_WritesSidecarConfig(t *testing.T) {
	sup, reg, _ := newSupervisorWithFake(t, map[string]types.ImageModelProfile{
		"sd15-test": simpleProfile("sd15-test"),
	})
	if _, err := sup.Load(context.Background(), "sd15-test", LoadOptions{}); err != nil {
		t.Fatalf("load: %v", err)
	}
	p, _ := reg.Profile("sd15-test")
	sidecar := filepath.Join(p.BundleDir(reg.ModelsDir()), "sd-server.config.json")
	data, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("sidecar not written: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("sidecar is not valid JSON: %v", err)
	}
	if parsed["seed_mode"] != "random" {
		t.Fatalf("seed_mode = %v", parsed["seed_mode"])
	}
}

// --- 11. Порт занят → внятная ошибка (а не «модель не грузится») --------------

func TestSupervisor_Load_PortBusy(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry(dir)
	writeProfile(t, dir, simpleProfile("m"))
	if err := reg.Load(); err != nil {
		t.Fatalf("registry: %v", err)
	}
	// Занимаем порт «чужим» процессом.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	cfg := DefaultConfig()
	cfg.ModelsDir = dir
	cfg.ListenIP = "127.0.0.1"
	cfg.ServerPort = port
	sup := NewSupervisor(&cfg, reg, NewMetrics())
	sup.SetClientFactoryAddr(func(string) *SDServerClient { return NewSDServerClientURL("http://127.0.0.1:1") })
	sup.SetRunner(NewFakeRunner(func([]string) (Process, error) { return NewFakeProcess(9, nil), nil }))

	_, err = sup.Load(context.Background(), "m", LoadOptions{})
	if err == nil {
		t.Fatal("expected port-busy error")
	}
	if !strings.Contains(err.Error(), "still busy") {
		t.Fatalf("err = %v, want port busy message", err)
	}
}

// --- helpers -----------------------------------------------------------------

func writeProfile(t *testing.T, dir string, p types.ImageModelProfile) {
	t.Helper()
	sub := filepath.Join(dir, p.Name)
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if p.Family == "" {
		p.Family = "sd15"
	}
	for i := range p.Files {
		if p.Files[i].LocalPath == "" {
			p.Files[i].LocalPath = filepath.Join(sub, p.Files[i].Filename)
		}
	}
	data, _ := json.Marshal(p)
	if err := os.WriteFile(filepath.Join(sub, ProfileFileName), data, 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}
}
