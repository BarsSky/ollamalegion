package cppbackend

import (
	"errors"
	"fmt"
	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/tokencount"
	"unsafe"
)

func (b *Backend) getModelInstance(name string) (*modelInstance, error) {
	b.mu.RLock()
	inst, exists := b.models[name]
	b.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("model %s not found", name)
	}

	inst.mu.Lock()
	if inst.handle == nil || unsafe.Pointer(inst.handle) == nil {
		inst.mu.Unlock()
		return nil, fmt.Errorf("model %s handle is nil", name)
	}
	if inst.info.State != StateLoaded {
		inst.mu.Unlock()
		return nil, fmt.Errorf("model %s is not loaded (state: %s)", name, inst.info.State)
	}
	inst.mu.Unlock()

	return inst, nil
}

func (b *Backend) getActiveQueries(inst *modelInstance) int {
	// R83-fix (2026-09-30): БЕЗ inst.mu. Раньше здесь был inst.mu.Lock(), а
	// inst.mu удерживается всю генерацию — из-за этого /api/models и /api/info
	// висели ровно столько, сколько шёл inference (замерено: 2 м 51 с на
	// 16k-prompt Cline). Счётчик теперь атомарный, см. modelInstance.
	return inst.activeQueriesCount()
}

// completionTokens — число токенов в сгенерированном тексте.
//
// R66d (2026-09-22): закрывает «tokens=0 на всех запросах». Причина была в
// том, что метрика считалась как approximateTokens(approxResultLen(...)), а
// approxResultLen() — заглушка, всегда возвращавшая "" (её писали, когда
// результата под рукой ещё не было). Теперь результат есть (Generate его
// возвращает), поэтому считаем по-настоящему:
//
//   - exact=true и handle доступен: точный tokenizer модели (handle.CountTokens);
//   - иначе (или tokenizer вернул 0 — например, модель без словаря):
//     оценка pkg/tokencount.
//
// exact=true можно передавать ТОЛЬКО когда вызывающий держит inst.mu —
// CountTokens дёргает tokenizer llama.cpp и не потокобезопасен относительно
// параллельного Infer.
func completionTokens(handle *bridge.ModelHandle, text string, exact bool) int {
	if text == "" {
		return 0
	}
	if exact && handle != nil {
		if n := handle.CountTokens(text); n > 0 {
			return n
		}
	}
	return approximateTokens(text)
}

// isBackendFailure — считать ли ошибку отказом бэкенда для метрик.
//
// R66d (2026-09-22): RecordRequest(...) раньше вызывался с success=true
// безусловно, то есть отказы инференса вообще не попадали в error_rate.
// Cancel — отдельный случай: C-bridge отдаёт BRIDGE_ERR_ABORTED (→
// bridge.ErrAborted) когда клиент отвалился/нажал Stop, и сам bridge
// документирует, что cancel НЕ считается retryable-ошибкой. Если считать его
// отказом, error_rate в мониторе раздувается остановками пользователя.
func isBackendFailure(err error) bool {
	if err == nil {
		return false
	}
	return !errors.Is(err, bridge.ErrAborted)
}

// metricsOutcomeR358 — исход генерации для метрик: успех / отказ / отмена.
//
// ЗАЧЕМ ОТДЕЛЬНАЯ ФУНКЦИЯ. До R358 у метрики было два значения (success/error), и
// отмена приклеивалась к успеху: isBackendFailure() исключает bridge.ErrAborted из
// ОШИБОК, но никуда не записывает саму отмену. В итоге /metrics и
// /api/infer/metrics не показывали отмен вообще (аудит abort, A8), хотя кнопка Stop
// и обрыв клиента — самые частые причины незавершённой генерации.
//
// Три исхода важны оператору по-разному: успех — «всё работает»; ошибка — «бэкенд
// отказывает» (нужен разбор); отмена — «клиент не дождался» (вопрос к скорости или
// к поведению клиента, а не к доступности модели).
func metricsOutcomeR358(err error) Outcome {
	if err == nil {
		return OutcomeSuccess
	}
	if errors.Is(err, bridge.ErrAborted) {
		return OutcomeCancelled
	}
	return OutcomeError
}

// approximateTokens — оценка количества токенов для текста.
//
// DEPRECATED для точных сценариев: используйте Backend.CountTokens с реальным
// tokenizer. Эта функция нужна там, где токенизатора заведомо нет.
//
// R66: использует pkg/tokencount вместо len([]rune(text))/4.
//
// R66d: остаётся для внешних вызовов/совместимости; путь метрик
// (Generate/GenerateStream/batchedInferStream) переведён на completionTokens,
// который умеет точный tokenizer. Оговорка R66 про «всегда 0» больше не
// действует — approxResultLen() удалена.
func approximateTokens(text string) int {
	return tokencount.Estimate(text)
}

// ============================================================
// Хелперы для расчёта памяти и слоёв
// ============================================================
