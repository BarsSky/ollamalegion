// aggregate_r715_test.go — R715 (2026-11-24): схлопывание повторов в кольцевом
// буфере логов (дефект D3 части 3 плана
// docs/plans/2026-11-24-remaining-defects.md).
//
// ПРОВЕРЯЕТСЯ НЕ «функция вызвалась», а то, ради чего шаг делался:
//   - повторяющееся событие занимает ОДНУ запись и несёт repeated/first/last;
//   - окно буфера на типовой нагрузке дефекта (154 строки/мин, 78% повторов)
//     перестаёт быть 13-минутным: при 5000 записей оно ≥ 60 минут;
//   - разные события НЕ склеиваются: request trace с разными request_id и
//     предупреждения разных бэкендов остаются отдельными записями;
//   - старый режим (NewLogBroker) не изменился — иначе поехали бы live-tail
//     тесты и смысл «число записей = число строк».
package logger

import (
	"fmt"
	"testing"
	"time"
)

// r715DefectEntry — строка дискового гейта из живого архива: именно она дала
// 1565 из 2000 записей. Число в поле disk_free_mb МЕНЯЕТСЯ от повтора к
// повтору (реально плавает) — схлопывание обязано это пережить, потому что в
// ключ входят только идентичность и текст без чисел.
func r715DefectEntry(backend string, freeMB int, at time.Time) LogEntry {
	return LogEntry{
		Time:    at,
		Level:   "warn",
		Message: "backend disk space critical, blocking",
		Source:  "balancer",
		Fields: map[string]interface{}{
			"backend":         backend,
			"disk_free_mb":    freeMB,
			"min_required_mb": 10240,
			"reason":          "disk_low",
		},
	}
}

// TestR715_AggregatingBrokerCollapsesRepeats — 1565 одинаковых WARN = одна
// запись с repeated=1565 и границами времени.
func TestR715_AggregatingBrokerCollapsesRepeats(t *testing.T) {
	b := NewAggregatingLogBroker(5000)
	base := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)

	for i := 0; i < 1565; i++ {
		b.Publish(r715DefectEntry("A10-126", 9480+i%11, base.Add(time.Duration(i)*time.Second)))
	}

	snap := b.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("записей в буфере %d, ожидалась 1 (1565 повторов должны схлопнуться)", len(snap))
	}
	got := snap[0]
	if got.Repeated != 1565 {
		t.Fatalf("repeated=%d, ожидалось 1565", got.Repeated)
	}
	if got.FirstOccurrence() != base {
		t.Errorf("first=%v, ожидалось %v", got.FirstOccurrence(), base)
	}
	wantLast := base.Add(1564 * time.Second)
	if !got.LastOccurrence().Equal(wantLast) {
		t.Errorf("last=%v, ожидалось %v", got.LastOccurrence(), wantLast)
	}
	// Текст и поля первого появления сохраняются как представитель события.
	if got.Message != "backend disk space critical, blocking" || got.Fields["backend"] != "A10-126" {
		t.Errorf("представитель записи потерян: %+v", got)
	}
	stats := b.Stats()
	if stats.PublishedTotal != 1565 || stats.CollapsedTotal != 1564 {
		t.Errorf("счётчики published=%d collapsed=%d, ожидалось 1565/1564",
			stats.PublishedTotal, stats.CollapsedTotal)
	}
}

// TestR715_RequestTracesStaySeparate — самая опасная ошибка схлопывания: у
// строки «request trace» постоянный текст, и склейка по тексту уничтожила бы
// след каждого запроса. Разные request_id обязаны оставаться разными записями.
func TestR715_RequestTracesStaySeparate(t *testing.T) {
	b := NewAggregatingLogBroker(500)
	base := time.Now()
	for i := 0; i < 100; i++ {
		b.Publish(LogEntry{
			Time:    base.Add(time.Duration(i) * time.Millisecond),
			Level:   "info",
			Message: "request trace",
			Fields: map[string]interface{}{
				"request_id": fmt.Sprintf("req-%d", i),
				"path":       "/api/chat",
				"status":     200,
			},
		})
	}
	if got := len(b.Snapshot()); got != 100 {
		t.Fatalf("записей %d, ожидалось 100: разные request_id склеились в один", got)
	}
}

// TestR715_BackendsStaySeparate — одинаковое WARN по разным бэкендам не должно
// превращаться в одну строку: именно по backend оператор находит узел.
func TestR715_BackendsStaySeparate(t *testing.T) {
	b := NewAggregatingLogBroker(200)
	base := time.Now()
	for i := 0; i < 10; i++ {
		b.Publish(r715DefectEntry("A10-126", 9480, base.Add(time.Duration(i)*time.Second)))
		b.Publish(r715DefectEntry("A10-127", 9480, base.Add(time.Duration(i)*time.Second)))
	}
	snap := b.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("записей %d, ожидалось 2 (по одной на бэкенд)", len(snap))
	}
	if snap[0].Fields["backend"] == snap[1].Fields["backend"] {
		t.Fatalf("бэкенды склеились: %v", snap[0].Fields["backend"])
	}
}

// TestR715_NumbersInMessageIgnored — числа В ТЕКСТЕ сообщения не мешают
// схлопыванию (нормализованный ключ), а слова — мешают.
func TestR715_NumbersInMessageIgnored(t *testing.T) {
	if k1, k2 :=
		AggregateKeyR715(LogEntry{Level: "warn", Message: "диск 9480 МБ"}),
		AggregateKeyR715(LogEntry{Level: "warn", Message: "диск 9471 МБ"}); k1 != k2 {
		t.Fatalf("числа в сообщении не вырезаны: %q != %q", k1, k2)
	}
	if k1, k2 :=
		AggregateKeyR715(LogEntry{Level: "warn", Message: "диск 9480 МБ"}),
		AggregateKeyR715(LogEntry{Level: "warn", Message: "память 9480 МБ"}); k1 == k2 {
		t.Fatalf("разные сообщения склеены: %q", k1)
	}
	if k1, k2 :=
		AggregateKeyR715(LogEntry{Level: "warn", Message: "x"}),
		AggregateKeyR715(LogEntry{Level: "info", Message: "x"}); k1 == k2 {
		t.Fatalf("разные уровни склеены: %q", k1)
	}
}

// TestR715_PlainBrokerUnchanged — исторический режим не тронут: 10 одинаковых
// строк в буфере на 3 записи дают ровно 3 записи, без repeated.
func TestR715_PlainBrokerUnchanged(t *testing.T) {
	b := NewLogBroker(3)
	for i := 0; i < 10; i++ {
		b.Publish(LogEntry{Level: "info", Message: "msg"})
	}
	snap := b.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("записей %d, ожидалось 3 (ring buffer cap)", len(snap))
	}
	for _, e := range snap {
		if e.Repeated != 0 {
			t.Fatalf("в историческом режиме появился repeated=%d", e.Repeated)
		}
	}
	if b.Aggregates() {
		t.Fatal("NewLogBroker не должен включать схлопывание")
	}
}

// TestR715_WindowAtTypicalLoad — ГЛАВНОЕ ЧИСЛО ДЕФЕКТА: было 13 минут.
//
// Профиль берётся из плана D3: 2000 строк за 13 минут (154 строки/мин), из них
// 1565 — повторы одного WARN дискового гейта (78%). Один и тот же поток событий
// подаётся ДВУМ брокерам: прежнему (2000 записей, без схлопывания) и новому
// (5000, со схлопыванием). Замеряется время, которое каждый реально покрывает.
//
// Поток продолжается, пока новый буфер не заполнится: иначе «окно» нового
// буфера ограничено длиной поданного потока, а не его ёмкостью.
func TestR715_WindowAtTypicalLoad(t *testing.T) {
	const (
		rawLines    = 2000.0        // строк за окно дефекта
		spanMin     = 13.0          // минут — живое окно дефекта
		rawPerMin   = rawLines / spanMin // ≈154 строки/мин
		dupRatio    = 1565.0 / 2000.0
		wantOver    = 60 * time.Minute
		maxOldPlus  = 20 * time.Minute
		newCapacity = 5000
	)
	interval := time.Duration(float64(time.Minute) / rawPerMin)
	base := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)

	oldBroker := NewLogBroker(2000)
	newBroker := NewAggregatingLogBroker(newCapacity)

	// 23000 строк ≈ 150 минут такого потока: этого достаточно, чтобы набить
	// 5000 записей нового буфера с тем же соотношением повторов.
	dupAt := 0
	for i := 0; i < 23000; i++ {
		at := base.Add(time.Duration(i) * interval)
		if float64(dupAt) < dupRatio*float64(i+1) {
			dupAt++
			e := r715DefectEntry("A10-126", 9480+i%7, at)
			oldBroker.Publish(e)
			newBroker.Publish(e)
			continue
		}
		e := LogEntry{
			Time:    at,
			Level:   "info",
			Message: "llamaCppMetricsPoller: updated",
			Source:  "balancer",
			Fields: map[string]interface{}{
				"backend":    "A10-126",
				"request_id": fmt.Sprintf("poll-%d", i),
			},
		}
		oldBroker.Publish(e)
		newBroker.Publish(e)
	}

	oldWin := windowOfBroker(oldBroker)
	newWin := windowOfBroker(newBroker)
	oldStats := oldBroker.Stats()
	newStats := newBroker.Stats()
	t.Logf("прежний буфер (2000, без схлопывания): записей %d, окно %s",
		len(oldBroker.Snapshot()), oldWin.Round(time.Second))
	t.Logf("новый буфер (5000, со схлопыванием):    записей %d, окно %s, published=%d collapsed=%d",
		len(newBroker.Snapshot()), newWin.Round(time.Second),
		newStats.PublishedTotal, newStats.CollapsedTotal)
	if oldStats.Aggregating || !newStats.Aggregating {
		t.Fatalf("режимы брокеров перепутаны: old.aggregating=%v new.aggregating=%v",
			oldStats.Aggregating, newStats.Aggregating)
	}

	// Контроль: прежний буфер обязан воспроизвести дефект (13 минут).
	if oldWin > maxOldPlus {
		t.Fatalf("контроль не воспроизвёл дефект: прежний буфер покрыл %s, ожидалось ~13 минут", oldWin)
	}
	if oldWin > 20*time.Minute {
		t.Fatalf("окно прежнего буфера %s — слишком велико для 154 строк/мин", oldWin)
	}
	if newWin < wantOver {
		t.Fatalf("окно логов %s < 60 минут (было %s): схлопывание не дало выигрыша",
			newWin.Round(time.Minute), oldWin.Round(time.Minute))
	}
	if newStats.CollapsedTotal == 0 {
		t.Fatal("счётчик схлопнутых записей = 0: повторы не считаются")
	}
}

// windowOfBroker — сколько времени покрывает буфер (от первого до последнего
// события).
func windowOfBroker(b *LogBroker) time.Duration {
	st := b.Stats()
	if st.FirstTime.IsZero() || st.LastTime.IsZero() {
		return 0
	}
	return st.LastTime.Sub(st.FirstTime)
}

// TestR715_WindowCoverageExtrapolated — то же окно, но с явным расчётом
// «сколько времени покрыл бы новый буфер при этой же скорости событий»: тест
// фиксирует не только факт ≥ 60 минут на конкретном прогоне, но и запас.
func TestR715_WindowCoverageExtrapolated(t *testing.T) {
	b := NewAggregatingLogBroker(5000)
	base := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	const interval = (13 * time.Minute) / 2000
	dup := 0
	for i := 0; i < 20000; i++ {
		at := base.Add(time.Duration(i) * interval)
		if i%10 < 8 {
			dup++
			b.Publish(r715DefectEntry("A10-126", 9480+i%7, at))
			continue
		}
		b.Publish(LogEntry{Time: at, Level: "info", Message: "tick", Source: "balancer",
			Fields: map[string]interface{}{"request_id": fmt.Sprintf("tick-%d", i)}})
	}
	st := b.Stats()
	covered := st.LastTime.Sub(st.FirstTime)
	perRecord := covered / time.Duration(st.Entries)
	fullWindow := perRecord * time.Duration(st.Capacity)
	t.Logf("на типовой нагрузке: %d разных событий за %s → %s на запись → окно буфера %d ≈ %s",
		st.Entries, covered, perRecord.Round(time.Second), st.Capacity, fullWindow.Round(time.Minute))
	if fullWindow < 60*time.Minute {
		t.Fatalf("расчётное окно %s < 60 минут", fullWindow.Round(time.Minute))
	}
}

// TestR715_LogEntryMemoryBudget — «поднять буфер, не съев память» числом:
// запись ~1 КБ, 5000 записей ≈ 5 МБ.
func TestR715_LogEntryMemoryBudget(t *testing.T) {
	entries := []LogEntry{
		r715DefectEntry("A10-126", 9480, time.Now()),
		{
			Time: time.Now(), Level: "info", Message: "request trace", Source: "balancer",
			Fields: map[string]interface{}{
				"request_id": "req-123456", "path": "/api/chat", "status": 200,
				"duration_ms": 1234, "backend": "A10-126", "model": "Qwen3.8-27B",
			},
		},
		{
			Time: time.Now(), Level: "warn", Message: "llamaCppMetricsPoller: updated", Source: "balancer",
			Fields: map[string]interface{}{
				"backend": "cppworker-gpu-bundled-agent", "loaded_models": 1,
				"models_reported": 1, "url": "http://cppworker-gpu:18092/api/models",
			},
		},
	}
	total := 0
	for _, e := range entries {
		total += LogEntryApproxBytesR715(e)
	}
	avg := total / len(entries)
	t.Logf("средняя запись %d байт; 5000 записей ≈ %d КиБ", avg, int64(total)/int64(len(entries))*5000/1024)
	if avg > 2048 {
		t.Fatalf("запись %d байт: 5000 записей съедят больше 10 МБ", avg)
	}
	if b := LogEntriesApproxBytesR715(entries) / len(entries) * 5000; b > 10<<20 {
		t.Fatalf("5000 записей = %d байт > 10 МБ", b)
	}
}

// TestR715_TrimKeepsIndexConsistent — после переполнения буфера индекс ключей
// не должен ссылаться на срезанные записи (иначе повтор «прилипал» бы к чужой
// строке).
func TestR715_TrimKeepsIndexConsistent(t *testing.T) {
	b := NewAggregatingLogBroker(100)
	base := time.Now()
	for i := 0; i < 1000; i++ {
		// Разные события различаются ИДЕНТИЧНОСТЬЮ (request_id), а не числом в
		// тексте: число в тексте схлопнулось бы — это и есть нормализация.
		b.Publish(LogEntry{
			Time: base.Add(time.Duration(i) * time.Millisecond), Level: "info",
			Message: "event", Source: "balancer",
			Fields: map[string]interface{}{"request_id": fmt.Sprintf("ev-%d", i)},
		})
	}
	snap := b.Snapshot()
	if len(snap) > 100 {
		t.Fatalf("после переполнения в буфере %d записей (ёмкость 100)", len(snap))
	}
	for _, e := range snap {
		if e.Repeated != 0 {
			t.Fatalf("неповторяющееся событие помечено repeated=%d: %q", e.Repeated, e.Message)
		}
	}
	// Повтор события, которое ещё в буфере, обязан схлопнуться с ним.
	last := snap[len(snap)-1]
	repeat := last
	repeat.Time = last.Time.Add(time.Second)
	repeat.Fields = map[string]interface{}{"request_id": last.Fields["request_id"]}
	b.Publish(repeat)
	snap = b.Snapshot()
	if got := snap[len(snap)-1]; got.Repeated != 2 {
		t.Fatalf("повтор последнего события не схлопнулся: repeated=%d (%q)", got.Repeated, got.Message)
	}
}
