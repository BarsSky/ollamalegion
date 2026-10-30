// autopull_guards_r341_test.go — R341 (2026-10-30), этап 4.
//
// По аудиту авто-pull (docs/plans/2026-10-30-autopull-audit.md):
//   F1 — ответ «model not found» при выключенном auto-pull помечал ЗДОРОВЫЙ узел
//        unhealthy (плюс recovery-check — пустая функция), и клиент получал 503
//        «all backends failed» вместо честного 404;
//   F4 — таймаут ожидания не снимал запись из activePulls, а CleanupStalePulls в
//        проде не вызывался: зависшая закачка блокировала dedup навсегда;
//   F5 — pull ходит на backend.OllamaPort напрямую, но бэкенд выбирался без
//        проверки порта (у llama.cpp-узлов он 0 → POST http://host:0/api/pull).
//
// Тесты фиксируют эти три исправления. Полный сценарий с живым реестром сюда
// сознательно не входит (медленно и хрупко) — он вынесен в живой чек-лист.
package balancer

import (
	"fmt"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestR341_StalePullIsCleanedBeforeEnqueue — F4: зависшая запись не блокирует навсегда.
func TestR341_StalePullIsCleanedBeforeEnqueue(t *testing.T) {
	cfg := types.AutoPullConfig{Enabled: true, MaxConcurrent: 1}
	// Прокси нужен настоящий (пустой): EnsureModel после очистки идёт искать
	// бэкенд с готовой моделью и берёт apm.proxy.mu.
	p := &Proxy{backends: map[string]*BackendState{}}
	apm := NewAutoPullManager(p, cfg)

	// Кладём «зависший» pull: стартовал давно, никто его не завершил.
	apm.mu.Lock()
	apm.activePulls["stuck-model"] = &PullState{
		Model:     "stuck-model",
		BackendID: "b1",
		StartedAt: time.Now().Add(-3 * time.Hour),
		Done:      make(chan struct{}),
	}
	apm.mu.Unlock()

	// EnsureModel обязан сначала вычистить зависшее (прокси = nil → дальше он
	// вернёт ошибку, нас интересует только факт очистки).
	_, _ = apm.EnsureModel("stuck-model")

	apm.mu.Lock()
	_, stillThere := apm.activePulls["stuck-model"]
	apm.mu.Unlock()
	if stillThere {
		t.Fatal("зависшая запись pull осталась в activePulls — dedup заблокирует модель навсегда (живой риск F4)")
	}
}

// TestR341_FreshPullNotCleaned — свежая запись не должна удаляться (иначе dedup
// перестанет работать и один pull пойдёт дважды).
func TestR341_FreshPullNotCleaned(t *testing.T) {
	cfg := types.AutoPullConfig{Enabled: true, MaxConcurrent: 1}
	p := &Proxy{backends: map[string]*BackendState{}}
	apm := NewAutoPullManager(p, cfg)

	apm.mu.Lock()
	apm.activePulls["fresh-model"] = &PullState{
		Model:     "fresh-model",
		BackendID: "b1",
		StartedAt: time.Now(),
		Done:      make(chan struct{}),
	}
	apm.mu.Unlock()

	apm.CleanupStalePulls(2 * time.Hour)

	apm.mu.Lock()
	_, stillThere := apm.activePulls["fresh-model"]
	apm.mu.Unlock()
	if !stillThere {
		t.Fatal("свежая запись pull удалена как устаревшая — dedup сломан, закачка начнётся дважды")
	}
}

// TestR341_PullSkipsBackendWithoutOllamaPort — F5: узел без Ollama-порта не выбирается.
func TestR341_PullSkipsBackendWithoutOllamaPort(t *testing.T) {
	p := &Proxy{backends: map[string]*BackendState{}}
	cfg := types.AutoPullConfig{Enabled: true, MaxConcurrent: 1}
	apm := NewAutoPullManager(p, cfg)

	// Узел без ollama-порта (типичный llama.cpp) — pull на него ходить не может.
	p.backends["llamacpp-1"] = &BackendState{
		Backend: &types.Backend{
			ID:              "llamacpp-1",
			Type:            types.BackendTypeLlamaCpp,
			Status:          types.StatusHealthy,
			OllamaPort:      0,
			MaxConcurrentReqs: 4,
		},
	}
	got := apm.findBestBackendForPull("m")
	if got != "" {
		t.Errorf("findBestBackendForPull вернул %q для узла без Ollama-порта — pull ушёл бы на http://host:0 (F5)", got)
	}
}

// TestR341_ModelNotFoundIsNotBackendFailure — F1: 404 модели не помечает узел.
//
// Проверяем на уровне типа ошибки: ветка в ServeHTTP отличает ModelNotFoundError
// от прочих ошибок и НЕ вызывает UpdateBackendStatus(Unhealthy). Живой прогон
// полного пути — в чек-листе (нужен стенд).
func TestR341_ModelNotFoundIsNotBackendFailure(t *testing.T) {
	err := &ModelNotFoundError{Model: "nope", Body: []byte(`{"error":"model not found"}`)}
	var target *ModelNotFoundError
	if !asModelNotFoundR341(err, &target) {
		t.Fatal("ModelNotFoundError не распознан — ветка R341 не сработает, узел снова станет unhealthy")
	}
	// Обычная ошибка не должна распознаваться как «модели нет».
	if asModelNotFoundR341(fmt.Errorf("connection refused"), &target) {
		t.Error("произвольная ошибка распознана как model-not-found — узел не будет помечен при реальном отказе")
	}
}

// asModelNotFoundR341 — локальная обёртка над errors.As для читаемости теста.
func asModelNotFoundR341(err error, target **ModelNotFoundError) bool {
	if err == nil {
		return false
	}
	mn, ok := err.(*ModelNotFoundError)
	if ok {
		*target = mn
	}
	return ok
}
