package balancer

// active_streams_r271.go — R271 (2026-10-29): чужой запрос не имеет права
// рвать чужой стрим.
//
// ЖИВОЙ ДЕФЕКТ (второй стенд, памяти хватает на две модели qwen3.8). Клиент
// Cline работал нормально, но когда параллельно запустили второго агента (DSH),
// первый получил `stream ended by upstream before completion` — то есть стрим
// оборвался на старте второго запроса.
//
// ЧТО ПРОИСХОДИЛО. Второй запрос потребовал другого окна n_ctx, preflight
// принял решение «перезагрузить модель» и вызвал executeAsyncReload →
// cppworker `/api/models/load` с другим n_ctx. Для воркера это «unload +
// reload», то есть модель под активным стримом первого клиента выгружается —
// llama.cpp закрывает поток без терминального маркера, наш R259-классификатор
// видит чистый EOF и честно пишет `upstream_eof`, а клиенту уходит
// «stream ended by upstream before completion». Формально правда, по сути —
// upstream был жив, модель под ним выгрузил наш же балансер по чужому запросу.
//
// ПОЧЕМУ НЕ ПОЙМАЛИ РАНЬШЕ. Учёт `p.activeStreams` — это sync.WaitGroup для
// graceful shutdown: он глобальный, и по нему нельзя ответить на вопрос «идёт
// ли стрим НА ЭТОМ бэкенде по ЭТОЙ модели». Поэтому разрушительные операции
// (reload) его вообще не смотрели.
//
// Здесь: учёт активных стримов ПО БЭКЕНДУ и ПО МОДЕЛИ, ожидание их завершения
// перед разрушительной операцией, отметка «кто выгрузил модель» и правдивая
// классификация обрыва (клиент видит нашу причину, а не обвинение воркера).

import (
	"context"
	"sync"
	"time"

	"ollama-loadbalancer/internal/settings"
	"ollama-loadbalancer/pkg/logger"
)

// Виды разрушительных операций (выгружают/перезапускают модель).
const (
	destructiveReloadR271 = "reload"
	destructiveUnloadR271 = "unload"
	// R280 (2026-10-29): AutoTune перезагружает модель тем же путём (unload +
	// load с новыми параметрами: kv-cache/контекст/слои) и обязан подчиняться
	// тому же правилу, что и reload по чужому окну n_ctx. Живой факт со стенда:
	// AutoTune сработал дважды за 10 минут и перезагрузил модель — под живым
	// стримом это ровно тот же обрыв, только повод другой.
	destructiveAutotuneR271 = "autotune"
)

// abortReasonForDestructiveOpR271 — как назвать клиенту причину обрыва,
// вызванного нашей разрушительной операцией.
//
// Одна таблица вместо if-ов в месте классификации: видов операций уже три, и
// каждый новый путь перезагрузки обязан добавить СВОЮ причину, а не молча
// свалиться в «upstream_eof» (именно это и произошло в R271: клиент получил
// «stream ended by upstream», хотя модель выгрузили мы).
func abortReasonForDestructiveOpR271(kind string) string {
	switch kind {
	case destructiveAutotuneR271:
		return streamAbortBalancerAutotune
	case destructiveReloadR271, destructiveUnloadR271:
		return streamAbortBalancerReload
	default:
		return streamAbortBalancerReload
	}
}

// streamRefKeyR271 — ключ «бэкенд + модель» для учёта стримов.
func streamRefKeyR271(backendID, model string) string {
	return backendID + "\x00" + model
}

// activeStreamsR271 — счётчики активных стримов и отметки разрушительных
// операций.
//
// Отдельная структура с собственным мьютексом, а не поля Proxy под p.mu:
// стрим начинается и заканчивается на горячем пути, а p.mu держат селекторы и
// реестр бэкендов — брать его на каждое начало/конец стрима незачем.
//
// Нулевое значение пригодно к использованию (карты создаются лениво): Proxy
// в тестах собирается как &Proxy{...} и не проходит через NewProxy.
type activeStreamsR271 struct {
	mu        sync.Mutex
	byBackend map[string]int
	byModel   map[string]int
	// destructive — последняя разрушительная операция по ключу backend+model:
	// нужна, чтобы объяснить клиенту настоящую причину обрыва.
	destructive map[string]destructiveOpR271
}

type destructiveOpR271 struct {
	kind string
	at   time.Time
}

// beginClientStreamR271 — учесть начало клиентского стрима.
//
// Возвращает функцию завершения: она снимает ОБА учёта (глобальный
// activeStreams для graceful shutdown и локальный для R271) и безопасна при
// повторном вызове. p == nil допустим (тесты и «пустой» прокси).
func beginClientStreamR271(p *Proxy, backendID, model string) func() {
	if p == nil {
		return func() {}
	}

	p.activeStreams.Add(1)
	if backendID == "" {
		// Без бэкенда локальный учёт не нужен, но глобальный обязателен.
		var once sync.Once
		return func() { once.Do(p.activeStreams.Done) }
	}

	key := streamRefKeyR271(backendID, model)
	s := &p.streamsR271
	s.mu.Lock()
	if s.byBackend == nil {
		s.byBackend = make(map[string]int, 4)
	}
	if s.byModel == nil {
		s.byModel = make(map[string]int, 4)
	}
	s.byBackend[backendID]++
	s.byModel[key]++
	s.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			if s.byBackend[backendID] > 0 {
				s.byBackend[backendID]--
				if s.byBackend[backendID] == 0 {
					delete(s.byBackend, backendID)
				}
			}
			if s.byModel[key] > 0 {
				s.byModel[key]--
				if s.byModel[key] == 0 {
					delete(s.byModel, key)
				}
			}
			s.mu.Unlock()
			p.activeStreams.Done()
		})
	}
}

// activeStreamsOnBackend — сколько стримов идёт на бэкенде (любая модель).
func (p *Proxy) activeStreamsOnBackend(backendID string) int {
	if p == nil || backendID == "" {
		return 0
	}
	s := &p.streamsR271
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byBackend[backendID]
}

// activeStreamsOnModel — сколько стримов идёт на бэкенде ИМЕННО по этой модели.
func (p *Proxy) activeStreamsOnModel(backendID, model string) int {
	if p == nil || backendID == "" {
		return 0
	}
	s := &p.streamsR271
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byModel[streamRefKeyR271(backendID, model)]
}

// noteDestructiveOpR271 — запомнить, что модель на бэкенде сейчас выгружают.
//
// Зачем: когда стрим оборвётся через секунды после этого, R259-классификатор
// должен сказать правду («балансер перезагрузил модель»), а не «upstream
// оборвался» — иначе оператор идёт искать проблему в llama.cpp.
func (p *Proxy) noteDestructiveOpR271(backendID, model, kind string) {
	if p == nil || backendID == "" {
		return
	}
	s := &p.streamsR271
	s.mu.Lock()
	if s.destructive == nil {
		s.destructive = make(map[string]destructiveOpR271, 4)
	}
	s.destructive[streamRefKeyR271(backendID, model)] = destructiveOpR271{kind: kind, at: time.Now()}
	s.mu.Unlock()
}

// recentDestructiveOpR271 — была ли по этой модели разрушительная операция за
// последние window (и какая).
func (p *Proxy) recentDestructiveOpR271(backendID, model string, window time.Duration) (string, bool) {
	if p == nil || backendID == "" {
		return "", false
	}
	s := &p.streamsR271
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.destructive[streamRefKeyR271(backendID, model)]
	if !ok {
		return "", false
	}
	if time.Since(op.at) > window {
		return "", false
	}
	return op.kind, true
}

// reloadDeferMaxWaitR271 — сколько ждать завершения чужих стримов перед
// перезагрузкой (LB_RELOAD_DEFER_MAX_SEC, дефолт 120 с).
//
// 0 означает «не ждать»: при активных стримах разрушительная операция
// пропускается сразу (полезно для тестов и для стендов, где ждать нельзя).
//
// R278: значение из реестра настроек (override из WebUI → env → дефолт). Это
// тот случай, где hot-apply особенно полезен: оператор может временно увеличить
// ожидание, не перезапуская контейнер и не теряя активные стримы.
func reloadDeferMaxWaitR271() time.Duration {
	v, ok := settings.HotInt("LB_RELOAD_DEFER_MAX_SEC")
	if !ok {
		return 120 * time.Second
	}
	if v < 0 {
		return 120 * time.Second
	}
	return time.Duration(v) * time.Second
}

// waitStreamsIdleR271 — дождаться, пока на бэкенде не останется активных
// стримов.
//
// Возвращает true, когда можно выполнять разрушительную операцию:
//   - стримов нет — true сразу (без ожидания);
//   - стримы есть — ждём до max; дождались — true, не дождались — false.
//
// Модель учитываем и по бэкенду: reload/unload в cppworker выгружает модель
// целиком, а слоты делят одну карту, поэтому стрим ПО ДРУГОЙ модели на том же
// бэкенде — тоже повод не выгружать (на этом и оборвался первый клиент).
func (p *Proxy) waitStreamsIdleR271(ctx context.Context, backendID, model string, max time.Duration) bool {
	if p == nil || backendID == "" {
		return true
	}
	if n := p.activeStreamsOnBackend(backendID); n == 0 {
		return true
	} else {
		logger.Get().Infow("R271: destructive op deferred — active streams on backend",
			"backend", backendID, "model", model, "active_streams", n,
			"max_wait_sec", max.Seconds())
	}
	if max <= 0 {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}

	deadline := time.Now().Add(max)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if p.activeStreamsOnBackend(backendID) == 0 {
				return true
			}
			if time.Now().After(deadline) {
				logger.Get().Warnw("R271: destructive op skipped — streams still active after wait",
					"backend", backendID, "model", model,
					"active_streams", p.activeStreamsOnBackend(backendID),
					"waited_sec", max.Seconds())
				return false
			}
		}
	}
}
