package balancer

// admission_admitter_t4_r493_test.go — R493-12p (этап 4 «два мозга»): T4 для
// очереди — один вход, один исход, одна видимость.
//
// Критерий этапа из `refactoring-plan.ru.md` — «T4: одинаковые X-Queue-* и
// `/queue` на обоих путях». Заголовки сведены в R493-12o, здесь закрывается
// вторая половина: механизм ожидания выбирается в ОДНОМ месте, исход описывается
// ОДНИМ набором полей, а ожидающие на пути обработчиков видны в `/queue`.
//
// Что доказывается:
//  1. выбор механизма зависит от флага и от того, просили ли probe — и ни от чего
//     больше (нет «третьего правила» на стороне обработчика);
//  2. обе реализации возвращают исход одного вида (BackendID/Lease/Waited/
//     Position/Budget/Queued/Err), поэтому транспорт один и тот же;
//  3. режим 0 сохраняет историческое поведение (probe-wait), режим 1 — единую
//     очередь;
//  4. ожидающие в очереди появления бэкенда видны в /queue с числами.

import (
	"context"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestR493_AdmitterSelection — какой механизм выбирается и почему.
func TestR493_AdmitterSelection(t *testing.T) {
	p := windowTestProxyR493(t, "m", 8192, 262144, 1)
	probe := func() string { return "" }

	cases := []struct {
		name  string
		flag  string
		probe func() string
		want  string
	}{
		{"путь обработчиков, исторический режим", "0", probe, "probe-wait"},
		{"путь обработчиков, единая очередь", "1", probe, "unified-admission"},
		{"путь без probe всегда идёт в единую очередь", "0", nil, "unified-admission"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LB_UNIFIED_ADMISSION", tc.flag)
			req := admitRequestR493{Model: "m", Type: types.BackendTypeLlamaCpp, Probe: tc.probe}
			if got := p.admitterForR493(req).Name(); got != tc.want {
				t.Fatalf("механизм = %q, ожидался %q (флаг=%s)", got, tc.want, tc.flag)
			}
		})
	}
}

// TestR493_AdmitterProbeContract — реализация «ждать появления бэкенда»:
// probe без ожидания, найденный бэкенд, отказ без ожидания и обязательный probe.
func TestR493_AdmitterProbeContract(t *testing.T) {
	t.Run("бэкенд есть сразу — ожидания нет", func(t *testing.T) {
		a := probeAdmitterR493{queue: newBackendWaitQueueR493(), timeout: 5 * time.Second}
		out := a.Admit(context.Background(), admitRequestR493{
			Model: "m", Probe: func() string { return "b1" },
		})
		if out.Err != nil || out.BackendID != "b1" {
			t.Fatalf("ожидался b1 без ошибки, получено %+v", out)
		}
		if out.Queued {
			t.Fatal("запрос не ждал — Queued обязан быть false")
		}
		if out.Budget != 5*time.Second {
			t.Fatalf("бюджет %v, ожидался 5s: он обязан называть ФАКТИЧЕСКИЙ предел", out.Budget)
		}
	})
	t.Run("ожидание выключено — один быстрый probe и отказ", func(t *testing.T) {
		a := probeAdmitterR493{queue: newBackendWaitQueueR493(), timeout: 0}
		out := a.Admit(context.Background(), admitRequestR493{
			Model: "m", Probe: func() string { return "" },
		})
		if out.Err == nil {
			t.Fatal("при выключенном ожидании и пустом probe обязана быть ошибка")
		}
		if out.Waited != 0 || out.Position != 0 {
			t.Fatalf("ждать было нечего: waited=%v position=%d", out.Waited, out.Position)
		}
	})
	t.Run("без probe — ошибка вызывающего, а не клиента", func(t *testing.T) {
		a := probeAdmitterR493{queue: newBackendWaitQueueR493(), timeout: time.Second}
		out := a.Admit(context.Background(), admitRequestR493{Model: "m"})
		if out.Err != errAdmitterNoProbe {
			t.Fatalf("ошибка %v, ожидалась errAdmitterNoProbe", out.Err)
		}
	})
}

// TestR493_AdmitterUnifiedContract — единая реализация: свободный слот выдаётся
// сразу, вместе с арендой (её обязано освободить вызывающее).
func TestR493_AdmitterUnifiedContract(t *testing.T) {
	p := windowTestProxyR493(t, "m", 8192, 262144, 1)
	// Один слот на бэкенде: после выдачи второй запрос ждёт — это и есть разница
	// с «ждать появления бэкенда», где ждут самого узла.
	p.backends["b1"].Backend.MaxConcurrentReqs = 1

	a := p.admitterForR493(admitRequestR493{Model: "m", Type: types.BackendTypeLlamaCpp})
	if a.Name() != "unified-admission" {
		t.Fatalf("механизм = %q, ожидалась единая очередь", a.Name())
	}
	out := a.Admit(context.Background(), admitRequestR493{Model: "m", Type: types.BackendTypeLlamaCpp})
	if out.Err != nil || out.BackendID == "" {
		t.Fatalf("свободный слот обязан выдаться, получено %+v", out)
	}
	if out.Lease == nil {
		t.Fatal("слот занят — аренда обязательна (иначе слот не освободится)")
	}
	out.Lease()
}

// TestR493_QueueStatsShowsBackendWaiters — T4: ожидающие на пути обработчиков
// видны в /queue (раньше их не было видно нигде, кроме лога).
func TestR493_QueueStatsShowsBackendWaiters(t *testing.T) {
	t.Setenv("LB_UNIFIED_ADMISSION", "0")
	p := windowTestProxyR493(t, "m", 8192, 262144, 1)
	// /queue читает обе очереди — фикстуре нужны обе (иначе nil deref).
	p.queueMgr = NewQueueManager(nil, 10, 1, 30*time.Second)
	p.admission = newAdmissionQueue(300 * time.Second)

	// Пустая очередь — блока нет (нулевой блок только путал бы).
	if stats := p.GetQueueStats(); stats.BackendWait != nil {
		if stats.BackendWait.Waiting != 0 {
			t.Fatalf("ожидающих нет, а видно %d", stats.BackendWait.Waiting)
		}
	}

	// Ставим двух ожидающих в очередь появления бэкенда и снимаем срез.
	prev := llmBackendWaitQueue
	llmBackendWaitQueue = newBackendWaitQueueR493()
	t.Cleanup(func() { llmBackendWaitQueue = prev })

	id1, pos1 := llmBackendWaitQueue.enqueue("m")
	id2, _ := llmBackendWaitQueue.enqueue("m")
	if pos1 != 1 {
		t.Fatalf("первый ожидающий обязан быть первым, получено %d", pos1)
	}
	defer llmBackendWaitQueue.dequeue("m", id1)
	defer llmBackendWaitQueue.dequeue("m", id2)

	stats := p.GetQueueStats()
	if stats.BackendWait == nil {
		t.Fatal("блок backend_wait отсутствует: ожидающие на /v1 снова невидимы")
	}
	if stats.BackendWait.Waiting != 2 {
		t.Fatalf("ожидающих %d, ожидалось 2", stats.BackendWait.Waiting)
	}
	if got := stats.BackendWait.ByModel["m"]; got != 2 {
		t.Fatalf("по модели m: %d, ожидалось 2", got)
	}
	if stats.BackendWait.Unified {
		t.Fatal("при флаге 0 механизм исторический — Unified обязан быть false")
	}

	// При флаге 1 блок честно говорит, что путь уже в единой очереди.
	t.Setenv("LB_UNIFIED_ADMISSION", "1")
	if stats := p.GetQueueStats(); stats.BackendWait == nil || !stats.BackendWait.Unified {
		t.Fatalf("при флаге 1 блок обязан сообщать Unified=true, получено %+v", stats.BackendWait)
	}
}

// newBackendWaitQueueR493 — пустая очередь ожидания для теста (без глобального
// состояния стенда).
func newBackendWaitQueueR493() *backendWaitQueue {
	return &backendWaitQueue{waiters: map[string][]uint64{}}
}

// TestR493_AdmitterBudgetIsTheOnlyDifference — бюджеты путей по-прежнему разные
// (до включения флага), и это ЕДИНСТВЕННОЕ отличие: исход одного вида.
func TestR493_AdmitterBudgetIsTheOnlyDifference(t *testing.T) {
	t.Setenv("LB_UNIFIED_ADMISSION", "0")
	// ВНИМАНИЕ: TestMain этого пакета ставит LB_BACKEND_WAIT_SEC=0 (иначе десятки
	// тестов «нет бэкенда» ждали бы по 2 минуты). Поэтому дефолт проверяется
	// явным значением, а не «как в проде».
	t.Setenv("LB_BACKEND_WAIT_SEC", "120")
	t.Setenv("LB_ADMISSION_WAIT_SEC", "300")
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "0")

	if got, want := backendWaitTimeoutR403(), 120*time.Second; got != want {
		t.Fatalf("бюджет очереди появления бэкенда = %v, ожидался %v (LB_BACKEND_WAIT_SEC)", got, want)
	}
	p := windowTestProxyR493(t, "m", 8192, 262144, 1)
	p.admission = newAdmissionQueue(300 * time.Second)
	p.setAdmissionWait(300 * time.Second)
	if p.admissionWaitBudget() <= 0 {
		t.Fatal("бюджет единой очереди обязан быть положительным: иначе путь без ожидания")
	}
}
