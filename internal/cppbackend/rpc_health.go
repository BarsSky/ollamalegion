// rpc_health.go — R114 (2026-10-13): готовность узлов раскладки (llama.cpp RPC).
//
// ЗАЧЕМ. Раскладка слоёв соединяет мастер с чужими GPU по протоколу
// `rpc-server`. Отказ узла в llama.cpp обрабатывается не ошибкой, а `abort()`:
// живой факт — «`SIGABRT: abort` + `signal arrived during cgo execution` во время
// `bridge_load_model`, после чего cppworker перезапускается и модель теряется»
// (см. `docs/architecture/layer-sharding.ru.md` §4.6).
//
// Отсюда два механизма:
//
//  1. ГЕЙТ ПЕРЕД ЗАГРУЗКОЙ (CheckRPCServers): недоступный узел обнаруживается
//     ДО того, как llama.cpp начнёт к нему подключаться. Загрузка отменяется
//     понятной ошибкой вместо падения процесса.
//  2. WATCHDOG У ЗАГРУЖЕННОЙ МОДЕЛИ: если узел пропал ПОСЛЕ загрузки, модель
//     выгружается в простое — иначе первый же запрос к ней уронит мастер внутри
//     C-моста. Следующий запрос загрузит модель заново (и гейт проверит узлы).
//
// Проверка — TCP-подключение: у `rpc-server` нет ручки здоровья (в образе
// cppworker его роль выполняет `warmup`-проба из `scripts/stand-ports.ps1`),
// поэтому «порт слушается» и есть доступность узла.
package cppbackend

import (
	"net"
	"strings"
	"time"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
)

// RPCHealthDialTimeout — сколько ждём TCP-подключение к узлу раскладки.
// Значение маленькое: гейт стоит на пути загрузки модели, а отказ узла — это
// «нет соединения», а не «медленно соединяется».
const RPCHealthDialTimeout = 2 * time.Second

// RPCWatchdogInterval — период проверки живости узлов у загруженной модели.
//
// 15 секунд: быстрее, чем заметит оператор, и медленнее, чем нужно, чтобы
// мешать генерации (проверка — один TCP-connect на узел).
const RPCWatchdogInterval = 15 * time.Second

// rpcNodeSilenceBudget — сколько ждём первый байт от узла при проверке живости.
//
// Тишина до дедлайна — НОРМА: настоящий rpc-server не говорит первым, он ждёт
// хэндшейка клиента. Отказ — немедленное закрытие соединения (так ведёт себя
// опубликованный порт без слушателя: замер 14 мс). Значение компромиссное:
// достаточно, чтобы поймать закрытие, и недорого на пути загрузки модели.
const rpcNodeSilenceBudget = 500 * time.Millisecond

// rpcWatchdogInterval — период, который реально использует watchdog.
// Переменная (не константа) ради тестов: живой таймер на 15 с сделал бы проверку
// выгрузки по потере узла минутной.
var rpcWatchdogInterval = RPCWatchdogInterval

// RPCNodeUnreachableError — узел раскладки недоступен.
//
// Отдельный тип, чтобы вызывающий мог отличить «узел пропал» (инфраструктура)
// от «модель не влезла в память» (планирование) и отдать клиенту понятный текст.
type RPCNodeUnreachableError struct {
	Endpoint string
	Reason   string
}

func (e *RPCNodeUnreachableError) Error() string {
	reason := e.Reason
	if reason == "" {
		reason = i18n.T("cppbackend.rpc_health.no_connection")
	}
	return i18n.T("cppbackend.rpc_health.node_unreachable_error", e.Endpoint, reason)
}

// unreachableRPCServers — узлы из списка, к которым нет РАБОЧЕГО RPC-соединения.
//
// R130 (2026-10-16): одного TCP-соединения НЕДОСТАТОЧНО. Живой дефект: во время
// миграции узел донора остался «опубликованным» портом без слушателя внутри
// (контейнер пересоздавался), TCP-диалог проходил успешно — гейт считал узел
// доступным и отдавал его в C-мост, после чего llama.cpp вызывал abort() и
// МАСТЕР ПАДАЛ ЦЕЛИКОМ. Триггером был обычный GET /api/models (обработчик считает
// бюджет памяти → CollectDevices).
//
// ЖИВОЙ ЗАМЕР, на котором построено правило (мастер, dead-end против настоящего узла):
//
//	порт опубликован, слушателя нет : connected -> CLOSE через 14 мс
//	живой rpc-server донора         : connected -> SILENT (ждёт хэндшейка клиента)
//
// Поэтому после установления соединения читаем один байт: немедленное закрытие
// означает «за портом никого нет» (так ведёт себя проброс порта без слушателя), а
// тишина до дедлайна — «узел на связи и ждёт нашего хэндшейка». Сам протокол
// llama.cpp мы не воспроизводим и не пытаемся: проверяется именно то, что
// отличает живую сторону от мёртвой.
func unreachableRPCServers(servers []string, timeout time.Duration) []string {
	if timeout <= 0 {
		timeout = RPCHealthDialTimeout
	}
	var bad []string
	for _, addr := range servers {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		if !rpcNodeAnswers(addr, timeout) {
			bad = append(bad, addr)
		}
	}
	return bad
}

// rpcNodeAnswers — отвечает ли за адресом ЖИВОЙ rpc-узел (см. правило выше).
//
// Возвращает false, если: соединение не установилось, узел сразу закрыл его, или
// чтение упало. Возвращает true, если узел держит соединение молча (ждёт клиента)
// либо что-то прислал сам.
func rpcNodeAnswers(addr string, dialTimeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return false
	}
	defer conn.Close()

	// Дедлайн на чтение: живой узел молчит, ожидая нашего сообщения, поэтому
	// «тишина» — это норма, а не отказ. Ждём недолго: dead-end закрывается за
	// единицы миллисекунд (замер: 14 мс).
	readBudget := rpcNodeSilenceBudget
	if readBudget <= 0 {
		readBudget = 500 * time.Millisecond
	}
	_ = conn.SetReadDeadline(time.Now().Add(readBudget))
	buf := make([]byte, 1)
	n, err := conn.Read(buf)
	if n > 0 {
		// Узел сам что-то прислал — он точно живой.
		return true
	}
	if err == nil {
		return true
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		// Тишина до дедлайна: узел держит соединение и ждёт хэндшейка — это
		// поведение настоящего rpc-server.
		return true
	}
	// Закрытие (EOF/RST) — за портом слушателя нет: именно этот случай ронял
	// мастер через abort() в llama.cpp.
	return false
}

// CheckRPCServers — гейт перед загрузкой: все узлы раскладки должны отвечать.
//
// Возвращает *RPCNodeUnreachableError на первый недоступный узел, иначе nil.
// Пустой список (раскладка не настроена) — не ошибка.
func CheckRPCServers(servers []string, timeout time.Duration) error {
	bad := unreachableRPCServers(servers, timeout)
	if len(bad) == 0 {
		return nil
	}
	return &RPCNodeUnreachableError{
		Endpoint: bad[0],
		Reason:   i18n.T("cppbackend.rpc_health.tcp_no_answer_reason", len(bad), len(servers)),
	}
}

// --- Watchdog у загруженной модели ------------------------------------------

// rpcWatchdog — наблюдение за узлами одной загруженной модели.
type rpcWatchdog struct {
	stop     chan struct{}
	model    string
	servers  []string
	interval time.Duration
}

// startRPCWatchdog запускает наблюдение за узлами модели.
//
// Вызывается после успешной загрузки, если раскладка включена. Возвращает
// функцию остановки (идемпотентную).
func (b *Backend) startRPCWatchdog(model string, servers []string) func() {
	live := make([]string, 0, len(servers))
	for _, s := range servers {
		if s = strings.TrimSpace(s); s != "" {
			live = append(live, s)
		}
	}
	if len(live) == 0 {
		return func() {}
	}

	wd := &rpcWatchdog{
		stop:     make(chan struct{}),
		model:    model,
		servers:  live,
		interval: rpcWatchdogInterval,
	}

	b.rpcWatchMu.Lock()
	if b.rpcWatchdogs == nil {
		b.rpcWatchdogs = map[string]*rpcWatchdog{}
	}
	if old, ok := b.rpcWatchdogs[model]; ok && old != nil {
		close(old.stop)
	}
	b.rpcWatchdogs[model] = wd
	b.rpcWatchMu.Unlock()

	go b.runRPCWatchdog(wd)

	return func() {
		b.rpcWatchMu.Lock()
		if cur, ok := b.rpcWatchdogs[model]; ok && cur == wd {
			delete(b.rpcWatchdogs, model)
			close(wd.stop)
		}
		b.rpcWatchMu.Unlock()
	}
}

// stopRPCWatchdog останавливает наблюдение за моделью (если оно есть).
func (b *Backend) stopRPCWatchdog(model string) {
	b.rpcWatchMu.Lock()
	if wd, ok := b.rpcWatchdogs[model]; ok && wd != nil {
		delete(b.rpcWatchdogs, model)
		close(wd.stop)
	}
	b.rpcWatchMu.Unlock()
}

// runRPCWatchdog — цикл наблюдения: пропал узел → выгружаем модель в простое.
func (b *Backend) runRPCWatchdog(wd *rpcWatchdog) {
	ticker := time.NewTicker(wd.interval)
	defer ticker.Stop()

	lost := ""
	for {
		select {
		case <-wd.stop:
			return
		case <-ticker.C:
		}

		if lost == "" {
			bad := unreachableRPCServers(wd.servers, RPCHealthDialTimeout)
			if len(bad) == 0 {
				continue
			}
			lost = bad[0]
			logger.Get().Errorw(i18n.T("cppbackend.rpc_health.node_lost_unload_pending"),
				"model", wd.model, "endpoint", lost,
				"unreachable", strings.Join(bad, ","),
				"servers", strings.Join(wd.servers, ","))
			continue
		}

		// Узел пропал: ждём, пока модель освободится. Выгружать под активной
		// генерацией нельзя — это use-after-free в C-мосте (та же причина, по
		// которой /api/models/unload отказывает при активных запросах).
		if active := b.activeQueriesFor(wd.model); active > 0 {
			logger.Get().Warnw(i18n.T("cppbackend.rpc_health.node_lost_model_busy"),
				"model", wd.model, "endpoint", lost, "active_queries", active)
			continue
		}

		if state, ok := b.ModelState(wd.model); !ok {
			// Модель уже выгружена кем-то другим — наблюдать нечего.
			logger.Get().Infow(i18n.T("cppbackend.rpc_health.watchdog_model_already_unloaded"), "model", wd.model)
			return
		} else if state != StateLoaded {
			continue
		}

		if err := b.UnloadModel(wd.model); err != nil {
			logger.Get().Errorw(i18n.T("cppbackend.rpc_health.watchdog_unload_failed"),
				"model", wd.model, "endpoint", lost, "error", err)
			continue
		}
		logger.Get().Warnw(i18n.T("cppbackend.rpc_health.watchdog_model_unloaded"),
			"model", wd.model, "endpoint", lost)
		return
	}
}

// checkRPCNodesForInference — готовы ли узлы раскладки к инференсу.
//
// R114 (2026-10-13). Гейт перед ГЕНЕРАЦИЕЙ, а не только перед загрузкой:
// llama.cpp при обращении к мёртвому RPC-устройству вызывает abort(), то есть
// клиент получает не ошибку, а перезапуск мастера. Watchdog выгружает модель в
// простое, но между смертью узла и его тиком проходит до RPCWatchdogInterval —
// этот гейт закрывает окно.
//
// Стоимость — один TCP-connect на узел (единицы миллисекунд) на запрос; это
// дешевле, чем упавший процесс.
func (b *Backend) checkRPCNodesForInference() error {
	if b == nil || !b.cfg.RPCEnabled || len(b.cfg.RPCServers) == 0 {
		return nil
	}
	bad := unreachableRPCServers(b.cfg.RPCServers, RPCHealthDialTimeout)
	if len(bad) == 0 {
		return nil
	}
	logger.Get().Errorw(i18n.T("cppbackend.rpc_health.inference_not_started"),
		"unreachable", strings.Join(bad, ","),
		"servers", strings.Join(b.cfg.RPCServers, ","))
	return &RPCNodeUnreachableError{
		Endpoint: bad[0],
		Reason:   i18n.T("cppbackend.rpc_health.no_tcp_answer_before_generation"),
	}
}

// activeQueriesFor — число активных запросов к модели (0, если модели нет).
func (b *Backend) activeQueriesFor(name string) int {
	b.mu.RLock()
	inst, ok := b.models[name]
	b.mu.RUnlock()
	if !ok || inst == nil {
		return 0
	}
	return b.getActiveQueries(inst)
}
