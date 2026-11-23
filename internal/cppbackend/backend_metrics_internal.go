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

// getInstanceForTokenize — инстанс для ТОКЕНИЗАЦИИ: не ждёт inference.
//
// R714 (D2, 2026-11-25). ЖИВОЙ ДЕФЕКТ: `POST /api/tokenize duration=46.155s` на
// локальном воркере при 45–47 мс на соседнем узле; следствие — балансер
// `exact prompt tokens unavailable … timeout` (1500/6000 мс) и работа «по
// оценке», завышающей примерно вдвое (`estimated 48121 → actual 33839`).
//
// ПРИЧИНА — ровно здесь, а не в токенизаторе. Прежний путь шёл через
// getModelInstance, который берёт inst.mu, а inst.mu удерживается ВСЮ генерацию
// (см. Generate). То есть подсчёт токенов ждал чужой запрос: 46 с — это не
// «медленный токенизатор», а «время до конца чужой генерации». Проверено
// замерами на изолированном контейнере (свободный слот — 1–3 мс, занятый —
// столько, сколько шёл inference).
//
// ЧЕГО ЗДЕСЬ НЕТ И ПОЧЕМУ: временный контекст/модель на запрос не создаются,
// словарь не переаллоцируется — C-мост токенизирует im->vocab уже загруженной
// модели (bridge_count_tokens). Нужен только замок ЖИЗНИ словаря — tokenMu:
// unload берёт его на запись вокруг FreeModel, токенизация — на чтение; inst.mu
// не берётся вовсе, поэтому занятость слота на подсчёт не влияет.
//
// Возвращает release: вызывающий ОБЯЗАН позвать его после токенизации (defer).
func (b *Backend) getInstanceForTokenize(name string) (*modelInstance, func(), error) {
	b.mu.RLock()
	inst, exists := b.models[name]
	b.mu.RUnlock()

	if !exists {
		return nil, nil, fmt.Errorf("model %s not found", name)
	}

	inst.tokenMu.RLock()
	if inst.handle == nil {
		inst.tokenMu.RUnlock()
		return nil, nil, fmt.Errorf("model %s handle is nil", name)
	}
	// State читается под tokenMu.RLock: и загрузка (публикация StateLoaded), и
	// выгрузка (StateUnloaded) пишут его под tokenMu.Lock — см. backend_load.go и
	// backend_unload.go. Поэтому «загружено» здесь означает «handle точно жив».
	if inst.info.State != StateLoaded {
		state := inst.info.State
		inst.tokenMu.RUnlock()
		return nil, nil, fmt.Errorf("model %s is not loaded (state: %s)", name, state)
	}
	return inst, inst.tokenMu.RUnlock, nil
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
