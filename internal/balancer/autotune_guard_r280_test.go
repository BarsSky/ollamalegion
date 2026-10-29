// autotune_guard_r280_test.go — R280 (2026-10-29): AutoTune-перезагрузка
// подчиняется тому же правилу, что и reload по чужому окну n_ctx (R271).
//
// ЖИВОЙ ФАКТ (стенд r279). AutoTune сработал дважды за ~10 минут
// («kv_cache q4_0 → f16») и каждый раз выполнял unload + load модели. Путь
// triggerAutoTuneReload → executeAutoTuneReload → ApplyAutoTunePlan НЕ имел
// guard'а, который уже стоял в трёх местах nctx_reload_handlers.go, поэтому
// авто-перезагрузка могла выгрузить модель под живым стримом. Клиент в этот
// момент получал «stream ended by upstream before completion» — хотя upstream
// был жив, а модель выгрузили мы.
//
// Тесты фиксируют:
//   - под активным стримом перезагрузка НЕ начинается (и не считается сбоем);
//   - без стримов путь идёт дальше (ошибка будет уже от сети, не «deferred»);
//   - клиент видит причину balancer_autotune, а не обвинение воркера.
package balancer

import (
	"errors"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// autotuneProxyR280 — прокси с одним llama.cpp-бэкендом, у которого есть
// CppWorkerPort (иначе executeAutoTuneReload выходит раньше guard'а).
func autotuneProxyR280(t *testing.T) *Proxy {
	t.Helper()
	p := &Proxy{
		backends: map[string]*BackendState{
			"bk-1": {Backend: &types.Backend{
				ID: "bk-1", Host: "127.0.0.1", CppWorkerPort: 1,
				Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
				MaxConcurrentReqs: 1,
			}},
		},
		config: &types.LoadBalancerConfig{},
	}
	return p
}

// TestR280_AutoTuneReloadDeferredWhileStreamActive — главное: под живым
// стримом AutoTune не перезагружает модель, и это не считается сбоем.
func TestR280_AutoTuneReloadDeferredWhileStreamActive(t *testing.T) {
	p := autotuneProxyR280(t)

	// «Идёт стрим по этой модели на этом бэкенде» — ровно то состояние, при
	// котором R271 запрещает разрушительные операции.
	endStream := beginClientStreamR271(p, "bk-1", "qwen3.8")
	defer endStream()

	// LB_RELOAD_DEFER_MAX_SEC=0 → «не ждать»: тест не должен спать 120 с.
	t.Setenv("LB_RELOAD_DEFER_MAX_SEC", "0")

	plan := &AutoTuneReloadPlan{KVCacheType: "f16", Reason: "kv_cache q4_0 → f16"}
	err := p.executeAutoTuneReload("bk-1", "qwen3.8", plan, &AutoTuneCircuit{})

	if !errors.Is(err, errAutoTuneReloadDeferred) {
		t.Fatalf("под активным стримом ожидали errAutoTuneReloadDeferred, получили %v", err)
	}
	if kind, ok := p.recentDestructiveOpR271("bk-1", "qwen3.8", time.Minute); ok {
		t.Fatalf("отложенная перезагрузка не должна помечаться как разрушительная операция (kind=%q)", kind)
	}
}

// TestR280_AutoTuneReloadProceedsWithoutStreams — без стримов guard не мешает:
// путь идёт дальше и упирается уже в недоступный воркер, но это НЕ «deferred».
func TestR280_AutoTuneReloadProceedsWithoutStreams(t *testing.T) {
	p := autotuneProxyR280(t)
	t.Setenv("LB_RELOAD_DEFER_MAX_SEC", "0")

	plan := &AutoTuneReloadPlan{KVCacheType: "f16", Reason: "test"}
	err := p.executeAutoTuneReload("bk-1", "qwen3.8", plan, &AutoTuneCircuit{})

	if errors.Is(err, errAutoTuneReloadDeferred) {
		t.Fatalf("без активных стримов перезагрузка не должна откладываться: %v", err)
	}
	// БЫЛО (R280): здесь требовался выставленный маркер autotune — он ставится
	// перед apply и оставался навсегда, даже если выгрузка не состоялась.
	// СТАЛО (R283): маркер снимается, если модель НЕ была выгружена (воркер
	// недоступен / занят). Иначе следующий оборванный стрим получил бы причину
	// `balancer_autotune` без всякой перезагрузки — клиенту сообщали бы
	// неправду о причине обрыва. Положительный случай (выгрузка состоялась →
	// маркер есть) проверяется в TestR283_DestructiveMarkerKeptOnlyIfUnloaded.
	if kind, ok := p.recentDestructiveOpR271("bk-1", "qwen3.8", time.Minute); ok {
		t.Fatalf("при неудавшейся выгрузке маркер autotune не должен оставаться (kind=%q)", kind)
	}
}

// TestR280_AutotuneReasonReachesClient — клиент видит свою причину, а не
// «upstream ended»: иначе оператор идёт искать проблему в llama.cpp.
func TestR280_AutotuneReasonReachesClient(t *testing.T) {
	if got := abortReasonForDestructiveOpR271(destructiveAutotuneR271); got != streamAbortBalancerAutotune {
		t.Fatalf("причина autotune = %q, want %q", got, streamAbortBalancerAutotune)
	}
	if got := abortReasonForDestructiveOpR271(destructiveReloadR271); got != streamAbortBalancerReload {
		t.Fatalf("причина reload = %q, want %q", got, streamAbortBalancerReload)
	}
	if got := abortReasonForDestructiveOpR271(destructiveUnloadR271); got != streamAbortBalancerReload {
		t.Fatalf("причина unload = %q, want %q", got, streamAbortBalancerReload)
	}
	msg := streamAbortClientMessage(streamAbortBalancerAutotune)
	if msg == "" || msg == streamAbortClientMessage(streamAbortUpstreamEOF) {
		t.Fatalf("текст для autotune не должен совпадать с обвинением upstream: %q", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "autotune") {
		t.Fatalf("в тексте для клиента должно быть слово autotune (реальный повод): %q", msg)
	}
}
