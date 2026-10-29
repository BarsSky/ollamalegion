package balancer

// stream_stall_r297.go — R297/R298 (2026-10-29): сторож зависшего стрима.
//
// ЖИВОЙ ДЕФЕКТ (стенд оператора). Поток «повис»: последний кадр в 15:01:20,
// дальше тишина — соединение открыто, терминатора (`done:true` / `[DONE]`) нет.
// Клиент (Cline) ждал бесконечно, в UI это выглядело как зависший ответ.
//
// ПОЧЕМУ НЕ ПОЙМАЛИ РАНЬШЕ. На стенде стоит `LB_STREAMING_NEVER_TIMEOUT=1` — он
// отключает ВСЕ idle/first-byte таймауты стрима, чтобы длинная генерация на CPU
// не убивалась. Вместе с ними исчез и единственный сторож тишины, поэтому
// мёртвый-но-не-закрытый upstream ждался вечно.
//
// ПОЧЕМУ ОТДЕЛЬНЫЕ НАСТРОЙКИ, А НЕ КОНСТАНТЫ:
//   - оператору нужно проверить поведение живьём за секунды, а не ждать пять
//     минут (поставить LB_STREAM_STALL_ABORT_SEC=10 и увидеть кадры);
//   - порог зависит от железа: на CPU-модели пауза между кадрами бывает долгой,
//     и «правильное» число — не наше дело решать за оператора.
//
// ОДНО МЕСТО ПРАВДЫ: значения живут в реестре настроек (internal/settings,
// регистрация — builtin_r278.go), как и остальные горячие лимиты стрима
// (LB_STREAM_MAX_LINE_BYTES). Отдельного чтения os.Getenv здесь нет намеренно:
// иначе WebUI/API показывали бы одно, а работало другое.
//
// R298 (2026-10-29): сторож переписан так, чтобы работать при БЛОКИРУЮЩЕМ
// чтении, и вынесен в ОДИН тип для всех путей.
//
// ЖИВОЙ ДЕФЕКТ R297. Сторож проверял тишину «на тике heartbeat» внутри
// main-loop hijack-пути. Пока upstream ЖИВ, тик приходил и проверка работала; но
// если воркер подвис (docker pause) — цикл всё равно продолжал крутиться, значит
// дело было не в этом. Проверка на стенде показала другое: сторож включён в
// ОДНОМ пути (hijack), а NDJSON-путь (`/api/chat`, которым ходит Cline через
// Ollama-протокол) сторожа не имел вообще: там блокирующий `scanner.Scan()`, и
// при `LB_STREAMING_NEVER_TIMEOUT=1` (без deadline) он висел вечно. Плюс порог
// «после первого кадра» означал, что пауза сразу после старта не ловилась.
//
// Поэтому: (1) сторож — отдельная горутина, которая не зависит от того, читает
// ли кто-то данные; (2) он один на все стриминговые пути (`streamStallWatch`);
// (3) добавлен ВТОРОЙ, отдельный порог для молчания ДО первого кадра
// (`LB_STREAM_FIRST_FRAME_ABORT_SEC`, по умолчанию 0 = выключен), потому что
// prefill на CPU — законная долгая пауза, и по умолчанию мы её не трогаем.

import (
	"sync"
	"time"

	"ollama-loadbalancer/internal/settings"
)

// streamStallAbortTimeoutDefault — 300 с тишины посреди уже начатого стрима.
const streamStallAbortTimeoutDefault = 300 * time.Second

// streamStallAbortTimeout — сколько тишины от upstream считать зависанием.
//
// 0 (или отрицательное) = сторож выключен: оператор сознательно вернул прежнее
// «ждать вечно».
func streamStallAbortTimeout() time.Duration {
	if v, ok := settings.HotInt("LB_STREAM_STALL_ABORT_SEC"); ok {
		return time.Duration(v) * time.Second
	}
	return streamStallAbortTimeoutDefault
}

// streamFirstFrameAbortTimeout — сколько тишины ДО первого кадра считать
// зависанием.
//
// По умолчанию 0 = выключено, и это осознанно: на CPU-модели prefill промпта
// идёт минутами, и обрывать его нельзя — ровно поэтому оператор и включил
// LB_STREAMING_NEVER_TIMEOUT. Отдельная настройка нужна для двух случаев:
//   - проверка живьём (пауза воркера сразу после старта): 10 секунд и видно
//     кадр-причину вместо бесконечного ожидания;
//   - стенды, где первый байт обязан приходить быстро (GPU).
func streamFirstFrameAbortTimeout() time.Duration {
	if v, ok := settings.HotInt("LB_STREAM_FIRST_FRAME_ABORT_SEC"); ok {
		return time.Duration(v) * time.Second
	}
	return 0
}

// stallEvent — срабатывание сторожа.
type stallEvent struct {
	// prefill=true — тишина была ДО первого кадра.
	prefill bool
	// stalled — сколько длилась тишина на момент срабатывания.
	stalled time.Duration
}

// streamStallWatch — сторож тишины на одном стриме.
//
// ЗАЧЕМ ОТДЕЛЬНАЯ ГОРУТИНА, А НЕ ПРОВЕРКА В ЦИКЛЕ ЧТЕНИЯ.
//
// Чтение из upstream блокирующее (`bufio.Scanner.Scan()` / `resp.Body.Read`).
// Пока upstream молчит, вызывающий код сидит ВНУТРИ чтения и никакую проверку
// выполнить не может — именно поэтому в R297 сторож на NDJSON-пути не сработал
// ни разу, а на hijack-пути зависел от того, придёт ли тик heartbeat. Сторож
// обязан жить независимо от чтения, а решение «пора рвать» — приходить либо
// каналом (select-циклы), либо закрытием тела ответа (блокирующие циклы).
type streamStallWatch struct {
	midFrameLimit   time.Duration
	firstFrameLimit time.Duration
	tick            time.Duration

	// onStall вызывается ОДИН раз из горутины сторожа. Нужен блокирующим путям:
	// закрыть resp.Body, чтобы разбудить чтение.
	onStall func(stallEvent)

	mu       sync.Mutex
	last     time.Time
	sawData  bool
	fired    bool
	lastPeek stallEvent
	stop     chan struct{}
	done     chan struct{}

	// events — срабатывание для select-циклов (буфер 1: второй раз не нужен).
	events chan stallEvent
}

// newStreamStallWatch создаёт сторож. Любой лимит <= 0 выключает соответствующую
// проверку; если выключены обе — горутина не запускается вообще (нулевая цена на
// путях, где сторож не нужен).
func newStreamStallWatch(midFrame, firstFrame time.Duration, onStall func(stallEvent)) *streamStallWatch {
	w := &streamStallWatch{
		midFrameLimit:   midFrame,
		firstFrameLimit: firstFrame,
		onStall:         onStall,
		last:            time.Now(),
		stop:            make(chan struct{}),
		done:            make(chan struct{}),
		events:          make(chan stallEvent, 1),
	}
	// Тик — четверть наименьшего активного лимита, чтобы срабатывание было
	// точным, но не молотило вхолостую. Границы: 200 мс .. 5 с.
	smallest := w.smallestLimit()
	switch {
	case smallest <= 0:
		w.tick = 0
	case smallest/4 < 200*time.Millisecond:
		w.tick = 200 * time.Millisecond
	case smallest/4 > 5*time.Second:
		w.tick = 5 * time.Second
	default:
		w.tick = smallest / 4
	}
	return w
}

// smallestLimit — наименьший ВКЛЮЧЁННЫЙ лимит (0, если оба выключены).
func (w *streamStallWatch) smallestLimit() time.Duration {
	smallest := time.Duration(0)
	for _, d := range []time.Duration{w.midFrameLimit, w.firstFrameLimit} {
		if d > 0 && (smallest == 0 || d < smallest) {
			smallest = d
		}
	}
	return smallest
}

// Enabled — включён ли хоть один лимит.
func (w *streamStallWatch) Enabled() bool {
	return w != nil && w.tick > 0
}

// Start запускает горутину сторожа. Повторный вызов — no-op.
func (w *streamStallWatch) Start() {
	if !w.Enabled() {
		close(w.done)
		return
	}
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(w.tick)
		defer ticker.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-ticker.C:
				if ev, ok := w.check(); ok {
					if w.onStall != nil {
						w.onStall(ev)
					}
					// Канал НЕ закрываем: читатель может появиться позже
					// (блокирующий путь узнаёт о срабатывании через Fired()).
					select {
					case w.events <- ev:
					default:
					}
					return
				}
			}
		}
	}()
}

// check — проверка тишины на текущий момент.
func (w *streamStallWatch) check() (stallEvent, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fired {
		return stallEvent{}, false
	}
	limit := w.midFrameLimit
	prefill := false
	if !w.sawData {
		// До первого кадра работает только отдельный (по умолчанию выключенный)
		// порог: молчание в начале — это prefill.
		limit = w.firstFrameLimit
		prefill = true
	}
	if limit <= 0 {
		return stallEvent{}, false
	}
	elapsed := time.Since(w.last)
	if elapsed < limit {
		return stallEvent{}, false
	}
	w.fired = true
	w.lastPeek = stallEvent{prefill: prefill, stalled: elapsed}
	return w.lastPeek, true
}

// Touch — «от upstream пришли данные»: сбросить отсчёт тишины.
func (w *streamStallWatch) Touch() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.last = time.Now()
	w.sawData = true
	w.mu.Unlock()
}

// TouchPrefill — «стрим стартовал, первого кадра ещё нет»: отсчёт с нуля, но
// режим prefill сохраняется (используется сразу после создания).
func (w *streamStallWatch) TouchPrefill() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.last = time.Now()
	w.mu.Unlock()
}

// Fired — сработал ли сторож (для блокирующих циклов, после выхода из чтения).
func (w *streamStallWatch) Fired() (stallEvent, bool) {
	if w == nil {
		return stallEvent{}, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.fired {
		return stallEvent{}, false
	}
	return w.lastPeek, true
}

// Events — канал срабатывания для select-циклов.
//
// Для выключенного сторожа возвращается ЗАКРЫТЫЙ канал: select по открытому
// каналу без отправителя заблокировался бы навсегда, то есть «сторож выключен»
// превратилось бы в «цикл завис на этом case». Закрытый канал ведёт себя как
// «событий не будет» — и select просто никогда не выберет эту ветку.
func (w *streamStallWatch) Events() <-chan stallEvent {
	if w == nil || !w.Enabled() {
		ch := make(chan stallEvent)
		close(ch)
		return ch
	}
	return w.events
}

// Stop — остановить сторож и дождаться горутины. Идемпотентно.
func (w *streamStallWatch) Stop() {
	if w == nil {
		return
	}
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	<-w.done
}
