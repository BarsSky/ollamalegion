package cppbackend

// rpc_reach_r229.go — R229 (2026-10-22): ИСКЛЮЧЕНИЕ НЕДОСТУПНОГО ДОНОРА на время
// работы с моделью.
//
// ЖИВОЙ СЦЕНАРИЙ. Модель разложена на донора; донор пропадает. Правила R114/R115
// запрещают дозваниваться до узлов RPC-опросом во время загрузки/инференса (опрос
// памяти через RPC роняет мастер на GGML_ABORT), поэтому планирование брало
// последний успешный снимок устройств — ВМЕСТЕ с мёртвым узлом. Дальше:
//
//   - планировщик считал «фантомную» память мёртвого узла и выбирал слишком много
//     GPU-слоёв;
//   - гейт перед загрузкой отменял загрузку ЦЕЛИКОМ (`RPCNodeUnreachableError`),
//     хотя без донора модель влезала и могла работать локально.
//
// Для оператора это выглядело как «донор умер — теперь ничего не грузится», хотя
// исправное поведение — исключить донора и работать на живых узлах.
//
// ЧТО ЗДЕСЬ. TCP-проба порта — БЕЗОПАСНАЯ альтернатива RPC-опросу: она не трогает
// протокол llama.cpp (не может вызвать abort), а различие «живой/мёртвый» уже
// отлажено в R114/R130 (живой узел держит соединение молча; мёртвый закрывает или
// не принимает). Результат кэшируется на rpcReachTTL, чтобы не платить 0.5 c
// тишины на каждый вызов планирования.
//
// Один и тот же отфильтрованный список используется И планированием (снимок
// устройств), И гейтом/мостом загрузки: иначе план считался бы по мёртвому узлу,
// а грузили бы без него.

import (
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// rpcReachTTL — сколько держим результат TCP-пробы. 10 с: донор, поднявшийся
// обратно, возвращается в раскладку в пределах одного-двух тиков сторожевого
// цикла, а планирование не платит за пробу на каждом вызове.
var rpcReachTTL = 10 * time.Second

type rpcReachResult struct {
	at    time.Time
	alive []string
	dead  []string
}

var (
	rpcReachMu    sync.Mutex
	rpcReachCache = map[string]rpcReachResult{}
)

// ResetRPCReachCacheForTest — сброс кэша доступности (только тесты).
func ResetRPCReachCacheForTest() {
	rpcReachMu.Lock()
	rpcReachCache = map[string]rpcReachResult{}
	rpcReachMu.Unlock()
}

// reachableRPCServerList — R229: разделить узлы раскладки на живые и мёртвые
// TCP-пробой (с кэшем). Порядок сохраняется; пустые адреса пропускаются.
func reachableRPCServerList(servers []string) (alive, dead []string) {
	clean := make([]string, 0, len(servers))
	for _, s := range servers {
		s = strings.TrimSpace(s)
		if s != "" {
			clean = append(clean, s)
		}
	}
	if len(clean) == 0 {
		return nil, nil
	}
	key := strings.Join(clean, ",")

	rpcReachMu.Lock()
	if e, ok := rpcReachCache[key]; ok && time.Since(e.at) < rpcReachTTL {
		a := append([]string(nil), e.alive...)
		d := append([]string(nil), e.dead...)
		rpcReachMu.Unlock()
		return a, d
	}
	rpcReachMu.Unlock()

	// Пробуем БЕЗ общей блокировки: 0.5 c тишины на живой узел не должны
	// сериализовать планирование всех моделей.
	for _, addr := range clean {
		if rpcNodeAnswers(addr, RPCHealthDialTimeout) {
			alive = append(alive, addr)
		} else {
			dead = append(dead, addr)
		}
	}

	rpcReachMu.Lock()
	rpcReachCache[key] = rpcReachResult{
		at:    time.Now(),
		alive: append([]string(nil), alive...),
		dead:  append([]string(nil), dead...),
	}
	rpcReachMu.Unlock()
	return alive, dead
}

// filterReachableRPC — R229: оставить только живые узлы и запомнить исключённые.
//
// Возвращает nil, если живых нет: все потребители (мост, memfit, сторож) трактуют
// nil как «раскладки нет» и планируют по локальным GPU — это и есть «исключить
// недоступного донора и работать без него».
func (b *Backend) filterReachableRPC(servers []string) []string {
	if b == nil || len(servers) == 0 {
		return nil
	}
	alive, dead := reachableRPCServerList(servers)
	if len(dead) == 0 {
		b.clearExcludedRPC()
		return alive
	}
	b.rememberExcludedRPC(dead)
	if logger.Get() != nil {
		logger.Get().Warnw("раскладка: недоступный узел исключён на время работы с моделью",
			"excluded", strings.Join(dead, ","),
			"alive", strings.Join(alive, ","),
			"hint", "llama.cpp на мёртвом узле падает abort(); модель будет работать на живых узлах локально")
	}
	if len(alive) == 0 {
		return nil
	}
	return alive
}

// rememberExcludedRPC — запомнить исключённые узлы для отчёта /api/models.
// Лог не дублируется: WARN пишется в filterReachableRPC при каждом обновлении
// кэша, а тут только факт для панели.
func (b *Backend) rememberExcludedRPC(dead []string) {
	b.excludedRPCMu.Lock()
	b.excludedRPC = append([]string(nil), dead...)
	b.excludedRPCAt = time.Now()
	b.excludedRPCMu.Unlock()
}

func (b *Backend) clearExcludedRPC() {
	b.excludedRPCMu.Lock()
	b.excludedRPC = nil
	b.excludedRPCAt = time.Time{}
	b.excludedRPCMu.Unlock()
}

// ExcludedRPCServers — узлы, исключённые как недоступные (для /api/models).
func (b *Backend) ExcludedRPCServers() []string {
	if b == nil {
		return nil
	}
	b.excludedRPCMu.Lock()
	defer b.excludedRPCMu.Unlock()
	if len(b.excludedRPC) == 0 {
		return nil
	}
	return append([]string(nil), b.excludedRPC...)
}

// ExcludedRPCAt — когда узел был исключён (для отчёта/тестов).
func (b *Backend) ExcludedRPCAt() time.Time {
	if b == nil {
		return time.Time{}
	}
	b.excludedRPCMu.Lock()
	defer b.excludedRPCMu.Unlock()
	return b.excludedRPCAt
}
