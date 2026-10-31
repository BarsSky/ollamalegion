// cancel_model_name_r353_test.go — R353 (2026-10-31).
//
// ЖИВОЙ ДЕФЕКТ (аудит abort §1.4 L1, живой разбор кнопки Stop). Балансер выбирает
// адресатов отмены «мягко»: регистр, путь, расширение .gguf, тег Ollama, частичное
// совпадение (modelNameMatches). Воркер сравнивал имена ТОЧНО
// (ActiveGenerations.CancelByModel: entry.model == model), а entry.model — это
// каноническое имя файла, под которым модель загружена.
//
// Итог: клиент жмёт Stop с "Qwen3-Instruct-2507-q4km.gguf" (или "gemma4:latest"),
// адресат выбран верно, приказ доставлен, воркер отвечает cancelled:0 — и стрим
// продолжается до конца. Снаружи: «кнопка Stop не работает». R343 честно печатал
// not_cancelled, но причину не называл.
//
// R353 закрывает это с ДВУХ сторон, и обе проверяются здесь:
//  1. воркер сравнивает имена в нормализованной форме (basename, регистр, .gguf);
//  2. балансер подставляет ЗАГРУЖЕННОЕ имя модели для конкретного бэкенда
//     (cancelModelNameForBackend) — тогда даже тег Ollama не мешает.
//
// Проверяется:
//  1. отмена находит генерацию, если имя отличается регистром;
//  2. отмена находит генерацию, если клиент добавил расширение .gguf;
//  3. отмена находит генерацию, если имя передано путём;
//  4. ТЕГ Ollama не срезается: иначе Stop погасил бы чужой вариант той же семьи;
//  5. балансер подставляет загруженное имя; если модель не в кэше — имя клиента.
package balancer

import (
	"context"
	"testing"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/types"
)

// r353Entry — зарегистрировать «генерацию» в трекере и вернуть её ctx.
func r353Entry(t *testing.T, ag *cppbackend.ActiveGenerations, id, model string) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// Add(requestID, userID, model, backend, cancel) — канонический способ регистрации.
	ag.Add(id, "", model, "cppworker-gpu", cancel)
	return ctx
}

// TestR353_WorkerCancelMatchesCaseInsensitive — регистр не должен мешать отмене.
func TestR353_WorkerCancelMatchesCaseInsensitive(t *testing.T) {
	ag := cppbackend.NewActiveGenerations()
	ctx := r353Entry(t, ag, "req-1", "Qwen3-14B-Q4_K_M")

	// Нормализация имён — часть контракта отмены (R353): регистр и .gguf не мешают.
	if got := ag.CancelByModel("", "qwen3-14b-q4_k_m.gguf"); got != 1 {
		t.Fatalf("CancelByModel(case+.gguf) = %d, want 1: клиент жмёт Stop с другим "+
			"регистром/расширением, и стрим продолжается (живой дефект §1.4 L1)", got)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("ctx генерации не отменён, хотя CancelByModel отчитался об отмене")
	}
}

// TestR353_WorkerCancelIgnoresTag — тег Ollama НЕ срезается при нормализации.
func TestR353_WorkerCancelIgnoresTag(t *testing.T) {
	ag := cppbackend.NewActiveGenerations()
	_ = r353Entry(t, ag, "req-1", "Qwen3-14B-Q4_K_M")

	if got := ag.CancelByModel("", "qwen3:14b"); got != 0 {
		t.Fatalf("CancelByModel по тегу отменил %d генераций: срезание тега погасило бы "+
			"чужой вариант той же семьи (на воркере имя файла тега не содержит)", got)
	}
}

// TestR353_WorkerCancelAcceptsPath — имя, переданное путём, тоже находит генерацию.
func TestR353_WorkerCancelAcceptsPath(t *testing.T) {
	ag := cppbackend.NewActiveGenerations()
	_ = r353Entry(t, ag, "req-1", "/app/models/gemma-4-E4B-it-Q4_K_M.gguf")

	if got := ag.CancelByModel("", "gemma-4-E4B-it-Q4_K_M"); got != 1 {
		t.Fatalf("CancelByModel(без пути и расширения) = %d, want 1: basename должен "+
			"уравнивать имена, зарегистрированные путём", got)
	}
}

// r353CancelProxy — прокси с одним llama.cpp-бэкендом и кэшем метрик.
func r353CancelProxy(t *testing.T, loaded []string) *Proxy {
	t.Helper()
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	p.backends["b1"] = &BackendState{Backend: &types.Backend{
		ID: "b1", Host: "10.0.0.5", CppWorkerPort: 18092,
		Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp,
	}}
	lm := &types.LlamaCppMetrics{}
	for _, name := range loaded {
		lm.LoadedModels = append(lm.LoadedModels, types.LlamaCppModel{Name: name, State: "loaded"})
	}
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics["b1"] = lm
	p.metricsMgr.mu.Unlock()
	return p
}

// TestR353_BalancerSendsLoadedName — балансер подставляет имя, под которым модель
// загружена у ЭТОГО бэкенда (единственный способ пережить тег Ollama).
func TestR353_BalancerSendsLoadedName(t *testing.T) {
	p := r353CancelProxy(t, []string{"Qwen3.8-27B-UD-Q4_K_M"})

	got := p.cancelModelNameForBackend("b1", "qwen3.8:latest")
	if got != "Qwen3.8-27B-UD-Q4_K_M" {
		t.Fatalf("cancelModelNameForBackend = %q, want загруженное «Qwen3.8-27B-UD-Q4_K_M»: "+
			"воркер сравнивает имена точно, и клиентский тег до него не доходит", got)
	}
}

// TestR353_BalancerKeepsClientNameWhenNotLoaded — модель не в кэше: не выдумываем имя.
func TestR353_BalancerKeepsClientNameWhenNotLoaded(t *testing.T) {
	p := r353CancelProxy(t, nil)

	got := p.cancelModelNameForBackend("b1", "unknown-model")
	if got != "unknown-model" {
		t.Fatalf("cancelModelNameForBackend = %q, want клиентское имя (воркер мог не "+
			"отчитаться о загрузке, подменять нечем)", got)
	}
}
