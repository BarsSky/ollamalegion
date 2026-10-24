// rpc_server_child.go — R123 (2026-10-16, пункт 2 разбора стенда).
//
// ЗАЧЕМ. Мастер раскладки подключает чужие GPU как ggml-устройства через
// llama.cpp RPC, и подключить он может только то, что отдаёт `rpc-server`. До
// сих пор на машине-доноре для этого держали ОТДЕЛЬНЫЙ контейнер
// (`deployments/docker-compose.rpc-node.yml`: тот же образ cppworker с
// переопределённым entrypoint `["/app/rpc-server"]`). Оператор получал второй
// сервис, второй .env и второй набор проверок ради одного порта — при том что
// воркер с этой же картой рядом уже работает.
//
// ТЕПЕРЬ cppworker поднимает `rpc-server` ДОЧЕРНИМ процессом:
//   - включается флагом CPPWORKER_RPC_SERVER_ENABLED (env) — одной строкой в
//     .env, без второго контейнера;
//   - управляется на ходу из WebUI (runtime-ключ rpcServerEnabled), состояние
//     процесса видно в метриках воркера (RPCServerStatus);
//   - версия протокола совпадает со стороной мастера конструктивно: дочерний
//     бинарь берётся ИЗ ТОГО ЖЕ ОБРАЗА, что и C-мост мастера (правило «один
//     коммит llama.cpp на всех узлах раскладки»).
//
// ЧЕСТНО О ГРАНИЦАХ. Супервизор НЕ лезет в планирование раскладки: он лишь
// поднимает/гасит процесс и отдаёт его состояние. Список узлов у мастера
// (CPPWORKER_RPC_SERVERS) и гейты доступности остаются как были.
package cppbackend

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
)

// Значения по умолчанию для узла раскладки.
const (
	// DefaultRPCServerBind — слушать снаружи: мастер приходит по сети, поэтому
	// 127.0.0.1 сделал бы узел бесполезным (ровно та ошибка, от которой
	// предостерегает комментарий в docker-compose.rpc-node.yml).
	DefaultRPCServerBind = "0.0.0.0"
	// DefaultRPCServerPort — тот же порт, что в правилах фаервола стенда и в
	// RPC-эндпоинтах групп раскладки.
	DefaultRPCServerPort = 50052
	// DefaultRPCServerBin — путь внутри образа cppworker (лежит рядом с
	// llama-server; именно его запускал отдельный контейнер).
	DefaultRPCServerBin = "/app/rpc-server"

	// rpcServerRestartMaxPerHour / rpcServerRestartBaseDelay / rpcServerRestartMaxDelay — R172
	rpcServerRestartMaxPerHour = 5
	rpcServerRestartBaseDelay  = 500 * time.Millisecond
	rpcServerRestartMaxDelay   = 30 * time.Second
)

// Ожидания старта — ПЕРЕМЕННЫЕ, а не константы: тестам незачем ждать секундами
// порт процесса, который по замыслу падает сразу. В бою значения не меняются.
var (
	// rpcServerListenWait — сколько ждать открытия порта СРАЗУ после старта.
	//
	// ЗАЧЕМ (живой замер). Процесс стартует и открывает порт не мгновенно, а
	// Status() опрашивает TCP немедленно — сразу после включения узел показывал
	// «процесс запущен, но порт не отвечает», хотя через доли секунды порт
	// открывался. Оператор (и WebUI) читали это как отказ. Теперь Start ждёт
	// готовности порта, а «не отвечает» пишется только если порт не поднялся и
	// после grace-периода (rpcServerListenGrace).
	rpcServerListenWait = 2500 * time.Millisecond
	// rpcServerListenGrace — после этого времени молчащий порт у живого процесса
	// уже точно аномалия, и о ней надо сказать.
	rpcServerListenGrace = 3 * time.Second
)

// RPCServerStatus — состояние дочернего процесса rpc-server (для метрик и UI).
//
// Все поля сериализуемы: снимок уезжает в /api/v1/cppworker/config и в метрики,
// которые читает балансер/WebUI.
type RPCServerStatus struct {
	Enabled bool `json:"enabled"`
	Running bool `json:"running"`
	// PID и StartedAt — чтобы оператор видел, что процесс именно тот, что был
	// запущен, а не «когда-то был».
	PID       int    `json:"pid,omitempty"`
	Bind      string `json:"bind,omitempty"`
	Port      int    `json:"port,omitempty"`
	Bin       string `json:"bin,omitempty"`
	StartedAt string `json:"startedAt,omitempty"`
	UptimeSec int64  `json:"uptimeSec,omitempty"`
	// Listening — проверка TCP: «процесс запущен» и «порт слушается» не одно и
	// то же. Истина для мастера — доступность порта (его гейт CheckRPCServers),
	// поэтому статус обязан показывать её отдельно, а не выдавать запуск за
	// готовность узла.
	Listening bool   `json:"listening"`
	Restarts  int    `json:"restarts,omitempty"`
	LastError string `json:"lastError,omitempty"`
	// Addr — готовый адрес для строки узла у мастера (bind:port).
	Addr  string   `json:"addr,omitempty"`
	Notes []string `json:"notes,omitempty"`
	// External — R171 (2026-10-17): узлом управляет НЕ воркер (отдельный
	// контейнер, compose-профиль rpc-node). Тогда PID пуст, а running/listening
	// означает «внешний узел ответил на пробу», а не «мы его запустили».
	External bool `json:"external,omitempty"`

	// --- R172 (2026-10-18): наблюдаемость узла ---
	//
	// LastExit — как завершился ПРЕДЫДУЩИЙ процесс ("exit status 1",
	// "signal: segmentation fault (core dumped)"). Пусто = процесс ещё не падал.
	// Это первое, что отличает «сервер разбился» от «сервер ответил false»:
	// клиент в обоих случаях пишет одно и то же «malformed response».
	LastExit string `json:"lastExit,omitempty"`
	// LastExitAt — когда это было (RFC3339).
	LastExitAt string `json:"lastExitAt,omitempty"`
	// LastExitPID — чей это был процесс: после перезапуска PID новый, и без
	// этого поля «упал тот же узел или предыдущий» не различить.
	LastExitPID int `json:"lastExitPid,omitempty"`
	// Restarting — узел поднимается автоматически, и момент известен. Отличается
	// от running=false: это не «узел лежит», а «узел вернётся».
	Restarting     bool `json:"restarting,omitempty"`
	RestartInMS    int  `json:"restartInMs,omitempty"`
	RestartEnabled bool `json:"restartEnabled,omitempty"`
	// Debug — включён ли построчный лог протокола (GGML_RPC_DEBUG).
	Debug bool `json:"debug,omitempty"`
	// LogLines/LogDropped — размер и «обрезанность» буфера вывода процесса.
	// LogDropped>0 значит: в буфере только хвост, начало аварии вытеснено.
	LogLines   int `json:"logLines,omitempty"`
	LogDropped int `json:"logDropped,omitempty"`
	// LogTail — последние строки вывода (полная история — ручка
	// /api/v1/cppworker/rpc-server/log). Здесь намеренно коротко: статус
	// читается балансером при каждом опросе, и раздувать его историей нельзя.
	LogTail []string `json:"logTail,omitempty"`
}

// RPCServerChild — супервизор дочернего процесса rpc-server.
//
// Один экземпляр на процесс cppworker. Все методы потокобезопасны: Start/Stop
// зовут и старт приложения, и runtime-конфиг из WebUI, и graceful shutdown.
//
// Живость процесса определяется ГОРУТИНОЙ ОЖИДАНИЯ (reap): она обнуляет c.cmd,
// когда процесс завершился. Так не нужны ни сигналы, ни /proc — код одинаково
// работает и в контейнере (linux), и в тестах на Windows.
type RPCServerChild struct {
	mu   sync.Mutex
	bin  string
	bind string
	port int
	// enabled — желаемое состояние (то, что просил оператор). Отличается от
	// running: при ошибке запуска enabled=true и running=false с заполненным
	// lastErr — и это видно в статусе, а не молча превращается в «выключено».
	enabled bool

	cmd       *exec.Cmd
	stopCh    chan struct{}
	startedAt time.Time
	restarts  int
	lastErr   string
	// externalAddr — R171: адрес внешнего узла (пусто = дочерний процесс).
	externalAddr string
	// extraArgs — аргументы ПЕРЕД «-H bind -p port». Нужны тестам: позволяют
	// поднять в роли rpc-server сам тестовый бинарь (тот же приём, что
	// TestHelperProcess в стандартной библиотеке), не подменяя супервизор
	// заглушкой. В бою пусто.
	extraArgs []string

	// --- R172 (2026-10-18): наблюдаемость и живучесть узла ---
	//
	// log — последние строки вывода дочернего процесса. До R172 вывод уходил
	// только в stdout контейнера: когда узел падал на загрузке модели, причина
	// (GGML_LOG_ERROR сервера) оставалась в docker logs и терялась при первой же
	// перезаписи контейнера — а клиент сообщал лишь «Remote RPC server crashed».
	log *rpcServerLog
	// debug — отдавать дочернему процессу GGML_RPC_DEBUG=1 (построчный лог
	// операций протокола на стороне сервера).
	debug bool
	// restart — поднимать узел после нештатной смерти (паритет с restart:
	// unless-stopped у отдельного контейнера).
	restart bool
	// stopped — Stop() погасил процесс НАМЕРЕННО; автоперезапуск при этом
	// запрещён, иначе выключение из WebUI превращалось бы в «гасим-поднимаем».
	stopped bool
	// lastExit/lastExitAt/lastExitPID — как именно умер процесс. Это и отличает
	// «сервер упал» от «сервер жив, а протокол сломался» — два разных диагноза,
	// которые до R172 выглядели одинаково.
	lastExit    string
	lastExitAt  time.Time
	lastExitPID int
	// exitTimes — времена смертей за последний час: бюджет автоперезапусков.
	exitTimes []time.Time
	// restartAt — момент запланированного перезапуска (пусто = не запланирован).
	// Нужен статусу: «узел лежит» и «узел поднимается через 2 с» — не одно и то
	// же для оператора и для планировщика раскладки.
	restartAt time.Time
}

// NewRPCServerChild — супервизор по настройкам конфига.
func NewRPCServerChild(cfg Config) *RPCServerChild {
	bin := strings.TrimSpace(cfg.RPCServerBin)
	if bin == "" {
		bin = DefaultRPCServerBin
	}
	bind := strings.TrimSpace(cfg.RPCServerBind)
	if bind == "" {
		bind = DefaultRPCServerBind
	}
	port := cfg.RPCServerPort
	if port <= 0 {
		port = DefaultRPCServerPort
	}
	return &RPCServerChild{
		bin:          bin,
		bind:         bind,
		port:         port,
		enabled:      cfg.RPCServerEnabled,
		externalAddr: rpcNodeExternalAddr(cfg),
		log:          newRPCServerLog(os.Stdout),
		debug:        cfg.RPCServerDebug,
		restart:      cfg.RPCServerRestart,
	}
}

// Enabled — желаемое состояние (оператор включил узел).
func (c *RPCServerChild) Enabled() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enabled
}

// Port — порт узла (для отчёта мастеру и UI).
func (c *RPCServerChild) Port() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.port
}

// Addr — адрес, по которому узел слушает (bind:port).
func (c *RPCServerChild) Addr() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// R171: у внешнего узла адрес — не наш bind, а то, что задал оператор.
	if c.externalAddr != "" {
		return c.externalAddr
	}
	return net.JoinHostPort(c.bind, strconv.Itoa(c.port))
}

// SetEnabled — включить/выключить узел на ходу (runtime-конфиг из WebUI).
//
// Возвращает ошибку запуска, но СОСТОЯНИЕ остаётся честным: enabled
// выставляется в любом случае, а lastErr объясняет, почему running=false.
func (c *RPCServerChild) SetEnabled(on bool) error {
	if c == nil {
		return fmt.Errorf("rpc-server supervisor is nil")
	}
	c.mu.Lock()
	c.enabled = on
	c.mu.Unlock()
	if !on {
		return c.Stop()
	}
	return c.Start()
}

// Start — поднять процесс, если он ещё не поднят (идемпотентно).
func (c *RPCServerChild) Start() error {
	if c == nil {
		return fmt.Errorf("rpc-server supervisor is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	// R171 (2026-10-17): ВНЕШНИЙ УЗЕЛ. Воркер не порождает процесс (у него нет
	// docker-сокета и не должно быть), а только фиксирует желание и проверяет
	// доступность. Статус при этом честно помечен external=true.
	if c.externalAddr != "" {
		c.enabled = true
		reachable := rpcNodeProbeExternal(c.externalAddr, rpcServerListenGrace)
		if reachable {
			logger.Get().Infow(i18n.T("cppbackend.rpc_server_child.external_node_available"),
				"addr", c.externalAddr)
			c.lastErr = ""
			return nil
		}
		c.lastErr = i18n.T("cppbackend.rpc_server_child.external_unreachable_error", c.externalAddr)
		logger.Get().Warnw(i18n.T("cppbackend.rpc_server_child.external_node_unreachable"),
			"addr", c.externalAddr, "hint", c.lastErr)
		return fmt.Errorf("%s", c.lastErr)
	}

	if c.cmd != nil {
		// Уже запущен: повторный вызов не плодит второй процесс на том же порту
		// (второй экземпляр молча не займёт порт, и узел выглядел бы поднятым,
		// хотя слушает первый).
		return nil
	}
	if !c.startedAt.IsZero() {
		// Процесс уже запускался и завершился — это перезапуск.
		c.restarts++
	}

	stopCh := make(chan struct{})
	args := append(append([]string{}, c.extraArgs...), "-H", c.bind, "-p", strconv.Itoa(c.port))
	cmd := exec.Command(c.bin, args...)
	// R172 (2026-10-18): вывод дочернего процесса идёт в кольцевой буфер узла,
	// который ДУБЛИРУЕТ его в stdout контейнера (`docker logs` работает как
	// раньше) и одновременно сохраняет последние строки в памяти воркера. Пока
	// этого не было, причина падения узла (GGML_LOG_ERROR из ggml-rpc) жила
	// только в docker logs и исчезала при первой перезаписи контейнера — клиент
	// же сообщал лишь «Remote RPC server crashed or returned malformed response».
	if c.log == nil {
		c.log = newRPCServerLog(os.Stdout)
	}
	cmd.Stdout = c.log
	cmd.Stderr = c.log
	// R172: отладочный режим протокола. Env наследуется от воркера (как у
	// отдельного контейнера: там дочерний процесс получал ровно тот же env),
	// плюс GGML_RPC_DEBUG при включённом debug.
	cmd.Env = os.Environ()
	if c.debug {
		cmd.Env = append(cmd.Env, "GGML_RPC_DEBUG=1")
	}
	c.stopped = false
	c.restartAt = time.Time{}
	if err := cmd.Start(); err != nil {
		c.lastErr = i18n.T("cppbackend.rpc_server_child.start_failed_error", c.bin, strings.Join(args, " "), err)
		logger.Get().Errorw(i18n.T("cppbackend.rpc_server_child.start_failed"),
			"bin", c.bin, "bind", c.bind, "port", c.port, "error", err)
		return fmt.Errorf("%s", c.lastErr)
	}
	c.cmd = cmd
	c.stopCh = stopCh
	c.startedAt = time.Now()
	c.lastErr = ""

	logger.Get().Infow(i18n.T("cppbackend.rpc_server_child.node_started"),
		"bin", c.bin, "bind", c.bind, "port", c.port, "pid", cmd.Process.Pid,
		"note", i18n.T("cppbackend.rpc_server_child.node_started_note"))

	go c.reap(cmd, stopCh)

	// Ждём открытия порта: процесс стартовал ≠ узел готов принимать мастера.
	// Ждём ПОД замком (Start идемпотентен и не должен запускать второй процесс
	// параллельно), поэтому опрос короткий — rpcServerListenWait.
	if c.waitListeningLocked(rpcServerListenWait) {
		logger.Get().Infow(i18n.T("cppbackend.rpc_server_child.port_listening"),
			"addr", net.JoinHostPort(c.bind, strconv.Itoa(c.port)), "pid", cmd.Process.Pid)
	} else {
		// Не ошибка запуска: процесс жив, но порт молчит — говорим об этом
		// честно и оставляем решение мастеру (его гейт CheckRPCServers не
		// допустит узел в раскладку).
		logger.Get().Warnw(i18n.T("cppbackend.rpc_server_child.process_up_port_silent"),
			"addr", net.JoinHostPort(c.bind, strconv.Itoa(c.port)),
			"waited_ms", rpcServerListenWait.Milliseconds())
	}
	return nil
}

// waitListeningLocked — опрос порта (вызывается под c.mu, отсюда Locked).
func (c *RPCServerChild) waitListeningLocked(timeout time.Duration) bool {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(c.port))
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// reap ждёт завершения процесса, чтобы (а) не оставлять зомби и (б) не врать в
// статусе: «running=true» для умершего процесса — ровно тот класс дефектов,
// который вычищаем.
//
// R172 (2026-10-18): здесь же фиксируется ПРИЧИНА смерти (exit-статус, время,
// PID) и решается вопрос автоперезапуска. Причина смерти — не украшение: пока
// её не было, «узел упал» и «узел жив, но протокол сломался» выглядели
// одинаково (в обоих случаях мастер писал «Remote RPC server crashed or returned
// malformed response»), и разбор упирался в догадки.
func (c *RPCServerChild) reap(cmd *exec.Cmd, stopCh chan struct{}) {
	waitErr := cmd.Wait()

	c.mu.Lock()
	select {
	case <-stopCh:
		// Штатная остановка: не ошибка, статус уже сброшен Stop().
		c.mu.Unlock()
		return
	default:
	}
	if c.cmd == cmd {
		c.cmd = nil
		c.stopCh = nil
	}
	now := time.Now()
	c.lastExit = describeRPCServerExit(waitErr, cmd.ProcessState)
	c.lastExitAt = now
	c.lastExitPID = cmd.Process.Pid
	c.exitTimes = append(c.exitTimes, now)
	c.exitTimes = trimRPCServerExitTimes(c.exitTimes, now)
	if waitErr != nil {
		c.lastErr = i18n.T("cppbackend.rpc_server_child.exited_error", c.lastExit)
	} else {
		c.lastErr = i18n.T("cppbackend.rpc_server_child.exited_code_error", c.lastExit)
	}
	restart, delay := c.restartBudgetLocked(now)
	if restart {
		c.restartAt = now.Add(delay)
	}
	var tail []RPCServerLogLine
	if c.log != nil {
		tail = c.log.Tail(5)
	}
	c.mu.Unlock()

	// Хвост лога пишем В ЛОГ ВОРКЕРА сразу: даже если оператор никогда не
	// откроет ручку с логом узла, последние слова процесса останутся в docker
	// logs воркера, то есть не потеряются вместе с контейнером.
	fields := []interface{}{
		"pid", cmd.Process.Pid,
		"exit", c.lastExit,
		"restart_in_ms", delay.Milliseconds(),
		"restart", restart,
	}
	for i, ln := range tail {
		fields = append(fields, fmt.Sprintf("tail%d", i+1), ln.Text)
	}
	if waitErr != nil {
		logger.Get().Warnw(i18n.T("cppbackend.rpc_server_child.process_exited_abnormally"), fields...)
	} else {
		logger.Get().Warnw(i18n.T("cppbackend.rpc_server_child.process_exited"), fields...)
	}

	if restart {
		logger.Get().Infow(i18n.T("cppbackend.rpc_server_child.node_will_restart"),
			"pid", cmd.Process.Pid, "delay_ms", delay.Milliseconds(),
			"note", i18n.T("cppbackend.rpc_server_child.restart_budget_note", rpcServerRestartMaxPerHour))
		go c.restartAfter(delay)
	}
}

// describeRPCServerExit — человекочитаемый exit-статус.
//
// ProcessState.String() на linux отдаёт «signal: segmentation fault (core
// dumped)» / «exit status 1», на Windows — «exit status N»: этого достаточно, а
// разбор конкретных сигналов зависел бы от платформы и врал бы на Windows.
func describeRPCServerExit(waitErr error, ps *os.ProcessState) string {
	if ps != nil {
		if s := ps.String(); s != "" {
			return s
		}
	}
	if waitErr != nil {
		return waitErr.Error()
	}
	return i18n.T("cppbackend.rpc_server_child.exit_code_zero")
}

// trimRPCServerExitTimes — оставить только смерти за последний час (бюджет
// автоперезапусков считается по окну, а не за всю жизнь процесса: иначе после
// пяти падений за сутки узел перестал бы подниматься вообще).
func trimRPCServerExitTimes(times []time.Time, now time.Time) []time.Time {
	cut := now.Add(-time.Hour)
	out := times[:0]
	for _, t := range times {
		if t.After(cut) {
			out = append(out, t)
		}
	}
	return out
}

// restartBudgetLocked — можно ли поднимать узел и через сколько (под c.mu).
func (c *RPCServerChild) restartBudgetLocked(now time.Time) (bool, time.Duration) {
	if !c.restart || !c.enabled || c.stopped {
		return false, 0
	}
	n := len(trimRPCServerExitTimes(c.exitTimes, now))
	if n >= rpcServerRestartMaxPerHour {
		return false, 0
	}
	delay := rpcServerRestartBaseDelay
	for i := 1; i < n; i++ {
		delay *= 2
		if delay >= rpcServerRestartMaxDelay {
			return true, rpcServerRestartMaxDelay
		}
	}
	return true, delay
}

// restartAfter — поднять узел после паузы, если за это время его не выключили и
// не подняли вручную.
func (c *RPCServerChild) restartAfter(delay time.Duration) {
	if delay > 0 {
		time.Sleep(delay)
	}
	c.mu.Lock()
	skip := c.stopped || !c.enabled || c.cmd != nil
	c.restartAt = time.Time{}
	c.mu.Unlock()
	if skip {
		return
	}
	if err := c.Start(); err != nil {
		// Start уже записал lastErr и залогировал причину — здесь только факт,
		// что автоматический подъём не удался (молчать об этом нельзя).
		logger.Get().Errorw(i18n.T("cppbackend.rpc_server_child.restart_failed"), "error", err)
		return
	}
	logger.Get().Infow(i18n.T("cppbackend.rpc_server_child.restarted_after_crash"),
		"addr", net.JoinHostPort(c.bind, strconv.Itoa(c.port)))
}

// Stop — погасить процесс (идемпотентно).
func (c *RPCServerChild) Stop() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	// R171: чужой процесс не убиваем — снимаем только желание и говорим вслух.
	if c.externalAddr != "" {
		c.enabled = false
		c.mu.Unlock()
		logger.Get().Infow(i18n.T("cppbackend.rpc_server_child.external_left_running"),
			"addr", c.externalAddr)
		return nil
	}
	cmd, stopCh := c.cmd, c.stopCh
	// R172: намеренная остановка запрещает автоперезапуск — иначе выключение
	// узла из WebUI превращалось бы в «гасим и через полсекунды поднимаем».
	c.stopped = true
	c.restartAt = time.Time{}
	if cmd == nil || cmd.Process == nil {
		c.mu.Unlock()
		return nil
	}
	if stopCh != nil {
		close(stopCh)
	}
	c.cmd = nil
	c.stopCh = nil
	c.mu.Unlock()

	pid := cmd.Process.Pid
	if err := cmd.Process.Kill(); err != nil {
		// Процесс уже мёртв — цель достигнута, это не ошибка.
		logger.Get().Infow(i18n.T("cppbackend.rpc_server_child.process_already_exited"), "pid", pid, "error", err)
		return nil
	}
	logger.Get().Infow(i18n.T("cppbackend.rpc_server_child.node_stopped"), "pid", pid)
	return nil
}

// LogTail — последние n строк вывода дочернего процесса (n<=0 — дефолт).
//
// Отдельный метод, а не поле статуса: статус опрашивается постоянно, а лог
// нужен по запросу (ручка /api/v1/cppworker/rpc-server/log). Отдаём копию под
// собственным замком буфера, чтобы не держать замок супервизора во время
// сериализации ответа.
func (c *RPCServerChild) LogTail(n int) []RPCServerLogLine {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	l := c.log
	c.mu.Unlock()
	return l.Tail(n)
}

// LogStats — размер буфера вывода и число вытесненных строк.
func (c *RPCServerChild) LogStats() (lines, dropped int) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	l := c.log
	c.mu.Unlock()
	return l.Count(), l.Dropped()
}

// RequiresRecreate — нужно ли пересоздать супервизор под новые настройки.
//
// R173 (2026-10-18): ЖИВАЯ АВАРИЯ, из-за которой метод появился. Приказ балансера
// (PUT /config/update с rpcServerEnabled/rpcServerPort) приходил на каждом
// согласовании роли, а обработчик гасил дочерний процесс просто потому, что поле
// присутствовало в запросе. Узел убивался прямо во время передачи весов мастером:
// мастер терял сессию и умирал, контейнер перезапускался. Теперь пересоздание —
// только когда меняется ИДЕНТИЧНОСТЬ процесса: адрес прослушивания, порт, бинарь,
// режим внешнего узла или отладочный режим (они «зашиты» в argv/env и иначе не
// применятся). Желание (включён/выключен) пересоздания НЕ требует — SetEnabled
// идемпотентен и живой процесс не трогает.
func (c *RPCServerChild) RequiresRecreate(cfg Config) bool {
	if c == nil {
		return true
	}
	bin := strings.TrimSpace(cfg.RPCServerBin)
	if bin == "" {
		bin = DefaultRPCServerBin
	}
	bind := strings.TrimSpace(cfg.RPCServerBind)
	if bind == "" {
		bind = DefaultRPCServerBind
	}
	port := cfg.RPCServerPort
	if port <= 0 {
		port = DefaultRPCServerPort
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bind != bind ||
		c.port != port ||
		c.bin != bin ||
		c.externalAddr != rpcNodeExternalAddr(cfg) ||
		c.debug != cfg.RPCServerDebug
}

// Status — снимок состояния для метрик и UI.
//
// Listening проверяется TCP-диалогом: «процесс запущен» и «порт слушается» — не
// одно и то же (rpc-server мог упасть между проверками, а порт занять чужой
// процесс). Тот же принцип, что у гейта раскладки мастера (CheckRPCServers):
// истина — доступность порта, а не факт запуска.
func (c *RPCServerChild) Status() RPCServerStatus {
	if c == nil {
		return RPCServerStatus{}
	}
	c.mu.Lock()
	if c.externalAddr != "" {
		enabled, addr := c.enabled, c.externalAddr
		c.mu.Unlock()
		// R171: у внешнего узла нет PID — статус строится пробой порта.
		return externalNodeStatus(enabled, addr, rpcServerListenGrace)
	}
	st := RPCServerStatus{
		Enabled:   c.enabled,
		Bind:      c.bind,
		Port:      c.port,
		Bin:       c.bin,
		Restarts:  c.restarts,
		LastError: c.lastErr,

		// R172: причина прошлой смерти и параметры наблюдаемости.
		LastExit:       c.lastExit,
		LastExitPID:    c.lastExitPID,
		RestartEnabled: c.restart,
		Debug:          c.debug,
	}
	if !c.lastExitAt.IsZero() {
		st.LastExitAt = c.lastExitAt.UTC().Format(time.RFC3339)
	}
	if !c.restartAt.IsZero() {
		st.Restarting = true
		st.RestartInMS = int(time.Until(c.restartAt).Milliseconds())
		if st.RestartInMS < 0 {
			st.RestartInMS = 0
		}
	}
	if c.cmd != nil && c.cmd.Process != nil {
		st.PID = c.cmd.Process.Pid
		st.Running = true
		st.StartedAt = c.startedAt.UTC().Format(time.RFC3339)
		st.UptimeSec = int64(time.Since(c.startedAt).Seconds())
	}
	if c.log != nil {
		st.LogLines = c.log.Count()
		st.LogDropped = c.log.Dropped()
		// Хвост кладём в статус НЕ ВСЕГДА: статус читают балансер и WebUI
		// постоянно, а нужен хвост только когда есть на что смотреть — узел
		// лежит/поднимается, он уже падал, или включён отладочный режим.
		// Полная история — по отдельной ручке с логом узла.
		if !st.Running || c.debug || c.lastExit != "" {
			for _, ln := range c.log.Tail(rpcServerLogTailDefault) {
				st.LogTail = append(st.LogTail, ln.Text)
			}
		}
	}
	c.mu.Unlock()

	st.Addr = net.JoinHostPort(st.Bind, strconv.Itoa(st.Port))
	if !st.Running {
		return st
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(st.Port)), 700*time.Millisecond)
	if err != nil {
		// Порт не отвечает при живом процессе: честно говорим об этом, НО только
		// после grace-периода — сразу после старта порт ещё открывается, и
		// «не отвечает» было бы ложной тревогой (живой замер: узел включали из
		// WebUI и видели «порт не отвечает» при полностью рабочем процессе).
		if st.UptimeSec*int64(time.Second) >= int64(rpcServerListenGrace) {
			st.Notes = append(st.Notes, i18n.T("cppbackend.rpc_server_child.status_port_not_answering", err.Error()))
		} else {
			st.Notes = append(st.Notes, i18n.T("cppbackend.rpc_server_child.status_port_opening"))
		}
		return st
	}
	_ = conn.Close()
	st.Listening = true
	return st
}
