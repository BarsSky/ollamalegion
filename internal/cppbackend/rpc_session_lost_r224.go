package cppbackend

// R224 (2026-10-22): потеря RPC-сессии ВО ВРЕМЯ генерации — честный диагноз и
// авто-восстановление.
//
// ЗАЧЕМ. R221 закрыл падение мастера, когда сессия с узлом раскладки умирает
// (узел пересоздан, сеть порвана): вместо SIGABRT клиент получает ошибку. Но сама
// ошибка приходит из C-моста с подсказкой «likely KV-cache overflow ... or GPU OOM»
// — при ушедшем узле это ложный след: оператор идёт искать OOM и контекст.
// Хуже того, модель остаётся загруженной с мёртвым RPC-устройством, и каждая
// следующая генерация падает, пока её не выгрузят руками.
//
// ЧТО ЗДЕСЬ. По факту сбоя инференса (не по таймеру и не на каждом токене):
//  1. если у модели включена раскладка — проверить узлы по TCP (та же проверка,
//     что стоит на входе генерации, CheckRPCServers);
//  2. узел не отвечает → вернуть типизированную RPCSessionLostError с прямым
//     текстом («это не OOM и не n_ctx») и запустить выгрузку модели, чтобы
//     следующая загрузка собрала раскладку заново;
//  3. узлы отвечают → вернуть исходную ошибку: там действительно OOM/контекст.
//
// Выгрузка — в горутине: вызывающий уже получил ошибку, ждать выгрузку ему
// нельзя. Не чаще одного раза в rpcSessionUnloadCooldown на модель, иначе пачка
// параллельных запросов устроит гонку выгрузок.

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
)

const (
	// rpcSessionProbeTimeout — сколько ждём TCP-ответ узла при диагностике.
	// Значение то же, что у гейта перед генерацией: диагноз не должен добавлять
	// секунды к ошибке.
	rpcSessionProbeTimeout = 700 * time.Millisecond
	// rpcSessionUnloadCooldown — не чаще одной авто-выгрузки на модель.
	rpcSessionUnloadCooldown = 30 * time.Second
)

// RPCSessionLostError — сессия с узлом раскладки пропала во время генерации.
//
// Отдельный тип, а не RPCNodeUnreachableError: тот говорит про отказ ПЕРЕД
// загрузкой и советует «повторите запрос», этот — про уже работавшую модель,
// которую надо перезагрузить, чтобы раскладка собралась заново.
type RPCSessionLostError struct {
	Endpoint string
	Servers  []string
	Cause    error
}

func (e *RPCSessionLostError) Error() string {
	endpoint := strings.TrimSpace(e.Endpoint)
	if endpoint == "" && len(e.Servers) > 0 {
		endpoint = e.Servers[0]
	}
	return i18n.T("cppbackend.rpc_session_lost_r224.session_lost_error", endpoint)
}

// Unwrap сохраняет исходную причину для errors.Is/As.
func (e *RPCSessionLostError) Unwrap() error { return e.Cause }

// rpcSessionAutoUnloadDisabled — оператор может выключить авто-выгрузку
// (CPPWORKER_RPC_SESSION_AUTO_UNLOAD=0/false/off/no): тогда после потери сессии
// модель остаётся загруженной, и её выгружает оператор.
func rpcSessionAutoUnloadDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CPPWORKER_RPC_SESSION_AUTO_UNLOAD"))) {
	case "0", "false", "off", "no":
		return true
	}
	return false
}

// shouldAutoUnload — окно против гонки выгрузок для одной модели.
func (b *Backend) shouldAutoUnload(modelName string, now time.Time) bool {
	if b == nil || modelName == "" {
		return false
	}
	b.sessionLostMu.Lock()
	defer b.sessionLostMu.Unlock()
	if b.sessionLostAt == nil {
		b.sessionLostAt = make(map[string]time.Time)
	}
	if at, ok := b.sessionLostAt[modelName]; ok && now.Sub(at) < rpcSessionUnloadCooldown {
		return false
	}
	b.sessionLostAt[modelName] = now
	return true
}

// diagnoseInferenceFailure — уточнить причину сбоя инференса.
//
// Вызывается ТОЛЬКО на пути ошибки. Если раскладки нет или узлы отвечают,
// возвращает ошибку без изменений.
func (b *Backend) diagnoseInferenceFailure(inst *modelInstance, modelName string, err error) error {
	if err == nil || b == nil {
		return err
	}
	servers := b.activeRPCServers()
	if len(servers) == 0 {
		return err
	}
	// Отмена — это не потеря узла.
	if errors.Is(err, bridge.ErrAborted) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	// Уже классифицированные причины не перекрываем: у n_ctx/prompt своя
	// семантика (авто-reload, 413), подменять её диагнозом узла нельзя.
	if errors.Is(err, bridge.ErrNCtxNeedsReload) || errors.Is(err, bridge.ErrPromptTooLong) {
		return err
	}
	var alreadyTyped *RPCNodeUnreachableError
	if errors.As(err, &alreadyTyped) {
		return err
	}

	probeErr := CheckRPCServers(servers, rpcSessionProbeTimeout)
	if probeErr == nil {
		// Узлы живы — значит это действительно OOM/контекст, не наш случай.
		return err
	}
	endpoint := servers[0]
	var unreachable *RPCNodeUnreachableError
	if errors.As(probeErr, &unreachable) && strings.TrimSpace(unreachable.Endpoint) != "" {
		endpoint = unreachable.Endpoint
	}
	b.unloadAfterSessionLoss(inst, modelName)
	return &RPCSessionLostError{
		Endpoint: endpoint,
		Servers:  append([]string(nil), servers...),
		Cause:    err,
	}
}

// unloadAfterSessionLoss — выгрузить модель, чтобы следующая загрузка собрала
// раскладку заново. Асинхронно и не чаще окна shouldAutoUnload.
//
// ПОЧЕМУ ЭТО НЕ РВЁТ ПАРАЛЛЕЛЬНЫЕ ЗАПРОСЫ. UnloadModel берёт inst.mu — то есть
// ждёт, пока текущий инференс отпустит модель, и только потом освобождает handle.
// Запрос, который в это время ждал мьютекс, после освобождения увидит
// `handle.ptr == nil` и получит «model not loaded» (проверка есть в Infer и
// inferStreamWithAbortFlag), а не обращение к освобождённой памяти. Следующий
// запрос поднимет модель через lazy-load.
func (b *Backend) unloadAfterSessionLoss(inst *modelInstance, modelName string) {
	if b == nil || modelName == "" || rpcSessionAutoUnloadDisabled() {
		return
	}
	if !b.shouldAutoUnload(modelName, time.Now()) {
		return
	}
	logger.Get().Warnw(i18n.T("cppbackend.rpc_session_lost_r224.session_lost_unloading"),
		"model", modelName, "session_auto_unload", true)
	unload := b.unloadForRecovery
	go func() {
		var err error
		if unload != nil {
			err = unload(modelName)
		} else {
			// Только ЭТОТ инстанс: если за время сбоя модель успели перезагрузить,
			// unloadModel увидит другой указатель и ничего не тронет (R224).
			err = b.unloadModel(modelName, inst)
		}
		if err != nil {
			logger.Get().Errorw(i18n.T("cppbackend.rpc_session_lost_r224.auto_unload_failed"),
				"model", modelName, "error", err)
			return
		}
		logger.Get().Infow(i18n.T("cppbackend.rpc_session_lost_r224.unloaded_after_loss"),
			"model", modelName)
	}()
}
