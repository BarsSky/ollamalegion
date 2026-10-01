// internal/balancer/model_size_resolution_r83_test.go — R83 (2026-09-25).
//
// idle/first-byte таймауты балансера — это ДАТЧИКИ ЗАВИСАНИЯ, а не лимит на
// генерацию: total stream timeout и request timeout по умолчанию выключены (0,
// принципы R60.26/R65c), а оставшиеся два дедлайна масштабируются по размеру
// GGUF-файла через EstimateIdleTimeoutFromModelSize:
//
//	< 2 GB → 120s | 2-5 GB → 600s | 5-12 GB → 1200s | 12-24 GB → 1800s | > 24 GB → 2400s
//
// Размер ищется по имени модели, которое прислал КЛИЕНТ (modelContextKey ←
// тело/путь запроса). Клиент законно называет модель ollama-именем с тегом
// (`qwen3.8:latest`), а cppworker репортит в /api/models имя файла
// (`Qwen3.8-27B-UD-Q4_K_M`). `getModelSizeBytes` матчит имена через
// `modelNameMatches`, который срезает только путь и `.gguf`, но НЕ срезает
// ollama-тег → размер не находится → idle падает в глобальный дефолт 120s.
//
// Последствие: медленная partial-offload генерация 27B (16.5 GB, 8 GB VRAM,
// единицы tok/s) обрывается «датчиком зависания» на 120-й секунде паузы, хотя
// ровно для этого случая в коде уже заложен tier 1800s. Это ровно тот класс
// отказов, который пользователь описывает как «балансер не должен рвать
// генерацию по таймауту».
//
// Должно быть: размер резолвится по любому легальному написанию имени, которым
// клиент называет модель, включая тег. Здесь же уместен тег-aware матчер
// `modelname.Matches`, который на стороне балансера уже используется для поиска
// модели на бэкенде (llamacpp_backend_helpers.go) — то есть таймаут-эвристика
// отстала от матчинга моделей.
//go:build llama_stub

package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// r83Qwen38Size — реальный размер Qwen3.8-27B-UD-Q4_K_M.gguf на стенде (байты).
const r83Qwen38Size = 16464440224

// r83ProxyWithLoadedQwen38 — Proxy с одним загруженным 27B, как его видит
// балансер после poll'а /api/models у cppworker.
func r83ProxyWithLoadedQwen38() *Proxy {
	p := setupProxyWithMetrics("b1", &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{
			{Name: "Qwen3.8-27B-UD-Q4_K_M", Size: r83Qwen38Size, State: "loaded"},
		},
	})
	p.config = &types.LoadBalancerConfig{}
	p.modelLatencyTracker = NewModelLatencyTracker()
	return p
}

// TestR83_ModelSizeResolution_AllClientSpellings — размер загруженной модели
// должен находиться по каждому написанию, которым клиент законно её называет.
func TestR83_ModelSizeResolution_AllClientSpellings(t *testing.T) {
	p := r83ProxyWithLoadedQwen38()

	for _, name := range []string{
		"Qwen3.8-27B-UD-Q4_K_M",        // имя файла без расширения (cppworker /api/models)
		"Qwen3.8-27B-UD-Q4_K_M.gguf",   // с расширением
		"qwen3.8",                      // короткое имя (containsFold)
		"qwen3.8:latest",               // ollama-имя с тегом — так зовёт OpenWebUI
		"Qwen3.8-27B-UD-Q4_K_M:latest", // тег на полном имени
	} {
		got := p.getModelSizeBytes(name)
		if got != r83Qwen38Size {
			t.Errorf("getModelSizeBytes(%q) = %d, want %d — ollama-тег не срезается, размер не резолвится, idle падает в 120s вместо 1800s",
				name, got, r83Qwen38Size)
		}
	}
}

// TestR83_IdleTimeout_ScalesWithModelSize — следствие дефекта резолвинга:
// для 16.5 GB модели эвристика tier'а обязана давать 30 мин.
//
// R83-доктрина (2026-10-01): эта эвристика больше НЕ применяется как таймаут по
// умолчанию — она осталась диагностической рекомендацией
// (ModelLatencyTracker.Recommended*). Проверяем обе вещи: сам расчёт tier'а и
// то, что рабочий idle-таймаут по умолчанию = 0 (нет таймаута), иначе медленная,
// но живая генерация обрывается посередине.
func TestR83_IdleTimeout_ScalesWithModelSize(t *testing.T) {
	if d := EstimateIdleTimeoutFromModelSize(r83Qwen38Size); d != 1800*time.Second {
		t.Fatalf("EstimateIdleTimeoutFromModelSize(16.5GB) = %v, want 30m (tier 12-24 GB)", d)
	}

	p := r83ProxyWithLoadedQwen38()
	for _, name := range []string{"Qwen3.8-27B-UD-Q4_K_M", "qwen3.8:latest"} {
		if got := p.getModelStreamingIdleTimeout(name); got != 0 {
			t.Errorf("getModelStreamingIdleTimeout(%q) = %v, want 0 (R83-доктрина: "+
				"таймаут — opt-in, по умолчанию его нет)", name, got)
		}
	}
}
