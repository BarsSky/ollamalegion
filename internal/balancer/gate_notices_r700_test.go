// gate_notices_r700_test.go — R700 (2026-11-24): уведомление о сработавшем
// гейте с числами и дедупликацией.
//
// ЧТО ФИКСИРУЕТСЯ (и почему именно так).
//
//  1. СРАБОТАВШИЙ ГЕЙТ ДОХОДИТ ДО ПОЛЬЗОВАТЕЛЯ. Тест идёт НАСТОЯЩИМ путём
//     ожидания (waitForInferenceBackend), а не зовёт функцию уведомления
//     напрямую: до R700 этот путь публиковал только строку лога why_queued, и в
//     колокольчике не было ничего. Поэтому на ревизии без правки тест краснеет
//     не «не сошлась строка», а «события нет вовсе».
//
//  2. В ТЕКСТЕ ЕСТЬ ЧИСЛА. «Запрос ждёт» без чисел не отвечает на вопрос
//     оператора: 9480 МБ против порога 10240 — это действие (освободить 1 ГБ
//     или снизить порог), а «мало места» — нет.
//
//  3. ДЕДУП. Список причин строится на КАЖДОМ заходе в очередь; без дедупа
//     один заблокированный узел даёт событие несколько раз в секунду, SSE ring
//     вмещает 100 событий — полезная история вытеснялась бы потоком копий.
//
//  4. ПОРОГ ДИСКА — ГОРЯЧИЙ. PUT /api/v1/cluster/config мутирует тот же объект
//     конфига, который читает гейт; тест меняет значение ровно так же (через
//     указатель) и проверяет, что узел перестаёт отбраковываться БЕЗ рестарта.
//     Это же — обратимость настройки: вернуть порог = вернуть отказ.
package balancer

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

const (
	gateTestModelR700   = "Qwen3.8-27B"
	gateTestBackendR700 = "A10-126"
)

// gateTestProxyR700 — прокси с ОДНИМ живым узлом, на котором загружена модель,
// слот свободен, а свободное место на диске задаётся тестом.
//
// Форма взята с живого стенда (stand_replay_r696_test.go): именно так выглядел
// второй стенд, где заявка стояла в очереди при свободном узле — модель
// загружена, слоты есть, но диск 9480 МБ < порога 10240 МБ.
func gateTestProxyR700(t *testing.T, diskFreeMB, minFreeMB uint64) *Proxy {
	t.Helper()
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "127.0.0.1", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{{
			ID: gateTestBackendR700, Name: gateTestBackendR700,
			Host: "127.0.0.1", OllamaPort: 11500, CppWorkerPort: 11501,
			Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
			MaxConcurrentReqs: 1, Weight: 1,
		}},
		Balancing: types.BalancingSettings{
			ModelAffinity: true, SessionStickiness: true,
			FirstByteTimeout: 30, StreamingIdleTimeout: 120, RequestTimeout: 30,
			QueueMaxSize: 100, SessionTTL: 900, MetricsInterval: 30,
			ModelLoadTimeout: 30,
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 95, MaxVRAMUsagePercent: 95},
			CPU:    types.CPULimits{MaxUsagePercent: 95},
			Memory: types.MemoryLimits{MaxUsagePercent: 95},
			Disk:   types.DiskLimits{MinFreeMB: minFreeMB},
		},
	}
	p := newProxyWithCleanup(t, cfg)
	// Фоновый поллер в тесте только мешает: он перезаписывает кэш метрик
	// настоящими ответами несуществующего узла.
	if p.llamaCppMetricsPoller != nil {
		p.llamaCppMetricsPoller.Stop()
		p.llamaCppMetricsPoller.WaitStopped(2 * time.Second)
	}
	p.UpdateMetrics(gateTestBackendR700, &types.BackendMetrics{
		ID: gateTestBackendR700, Timestamp: time.Now(), Status: types.StatusHealthy,
		BackendType: types.BackendTypeLlamaCpp,
		GPU:         types.GPUMetrics{UsagePercent: 10, MemoryTotal: 22488, MemoryUsed: 20142, MemoryFree: 2346},
		System: types.SystemMetrics{
			CPUUsagePercent: 5, MemoryTotal: 100644, MemoryUsed: 16847, MemoryFree: 83797,
			DiskTotal: 194403, DiskFree: diskFreeMB, DiskUsed: 194403 - diskFreeMB,
		},
		LlamaCpp: types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppModel{{
				Name: gateTestModelR700, State: "loaded",
				Size:           uint64(16) * 1024 * 1024 * 1024,
				ContextLength:  131072,
				ContextPerSeq: 131072,
				MaxSlots:      1, Parallel: 1,
			}},
			VramKnown: true, AvailableVRAMMB: 2346,
		},
	})
	p.metricsMgr.MarkLlamaCppPolled(gateTestBackendR700, time.Now())
	return p
}

// waitGateEventR700 — ждать первое уведомление гейта из шины событий.
func waitGateEventR700(t *testing.T, ch <-chan types.Event, timeout time.Duration) (types.Event, bool) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return types.Event{}, false
			}
			if ev.Type == types.EventNotification && ev.Source == "gate" {
				return ev, true
			}
		case <-deadline:
			return types.Event{}, false
		}
	}
}

// runQueueAttemptR700 — один заход заявки в единую очередь (путь, который до
// R700 писал только лог) и ожидание его завершения по таймауту ожидания.
func runQueueAttemptR700(p *Proxy) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, _, _, err := p.waitForInferenceBackend(
		ctx, gateTestModelR700, types.BackendTypeLlamaCpp, "sess-r700")
	return err
}

// TestR700_DiskGateNotifiesOnce — главный различающий тест: гейт диска даёт ОДНО
// уведомление с числами и узлом, а повторное ожидание по той же причине молчит.
func TestR700_DiskGateNotifiesOnce(t *testing.T) {
	p := gateTestProxyR700(t, 9480, 10240)
	p.setAdmissionWait(300 * time.Millisecond)
	_, ch := p.EventBus().Subscribe()

	done := make(chan error, 1)
	go func() { done <- runQueueAttemptR700(p) }()

	ev, ok := waitGateEventR700(t, ch, 5*time.Second)
	if !ok {
		t.Fatalf("ожидание по диску не опубликовало уведомление: до R700 оператор видел причину только в логе " +
			"(`why_queued`) и в теле 503, а колокольчик молчал")
	}
	if err := <-done; !errors.Is(err, errAdmissionTimeout) {
		t.Fatalf("заявка не дошла до таймаута ожидания: err=%v (тест должен воспроизводить живое ожидание)", err)
	}

	// --- текст уведомления ---
	if !strings.Contains(ev.Message, "9480") || !strings.Contains(ev.Message, "10240") {
		t.Errorf("в уведомлении нет чисел диска (свободно/минимум): %q", ev.Message)
	}
	if !strings.Contains(ev.Message, gateTestBackendR700) {
		t.Errorf("в уведомлении не назван узел %q: %q", gateTestBackendR700, ev.Message)
	}
	if ev.BackendID != gateTestBackendR700 {
		t.Errorf("BackendID события = %q, want %q (иначе UI не свяжет причину с узлом)",
			ev.BackendID, gateTestBackendR700)
	}
	if ev.Severity != types.SeverityWarning {
		t.Errorf("severity дисковой причины = %q, want warning (это предупреждение, а не отказ)", ev.Severity)
	}
	if gate, _ := ev.Data["gate"].(string); gate != gateKindDiskR700 {
		t.Errorf("Data[gate] = %v, want %q (по нему UI выбирает подпись)", ev.Data["gate"], gateKindDiskR700)
	}

	// --- дедуп: второй заход по той же причине не публикует второе событие ---
	done2 := make(chan error, 1)
	go func() { done2 <- runQueueAttemptR700(p) }()
	if ev2, got := waitGateEventR700(t, ch, time.Second); got {
		t.Fatalf("повторное ожидание по той же причине опубликовало ЕЩЁ одно уведомление (%q): "+
			"дедупа нет — очередь опрашивает условия несколько раз в секунду, SSE ring хранит 100 событий",
			ev2.Message)
	}
	<-done2

	notices := p.GateNoticesR700()
	if len(notices) != 1 {
		t.Fatalf("снимок причин = %d записей, want 1 (одна причина на гейт и узел): %+v", len(notices), notices)
	}
	if notices[0].Count < 2 {
		t.Errorf("Count = %d, want >= 2: схлопнутые наблюдения должны быть видны оператору («×N»)", notices[0].Count)
	}
}

// TestR700_GateMessagesCarryNumbers — каждый из пяти гейтов говорит числами.
//
// Проверяются ВСЕ пять причин: диск, RAM, VRAM, окно, слоты. Текст строится из
// gateNumbersR700, поэтому тест ловит и потерю чисел при рефакторинге, и
// случайное возвращение к «не прошёл по лимитам ресурсов».
func TestR700_GateMessagesCarryNumbers(t *testing.T) {
	p := gateTestProxyR700(t, 9480, 10240)
	_, ch := p.EventBus().Subscribe()

	cases := []struct {
		name    string
		reject  backendRejectionR460
		want    []string
		gate    string
		sev     types.EventSeverity
		backend string
	}{
		{
			name: "disk",
			reject: backendRejectionR460{
				BackendID: "A10-126", Reason: rejectDiskLowR460,
				Detail:  "мало места на диске: свободно 9480 МБ < minFreeMB 10240 МБ",
				Numbers: &gateNumbersR700{FreeMB: 9480, MinFreeMB: 10240},
			},
			want:    []string{"9480", "10240", "настройках"},
			gate:    gateKindDiskR700,
			sev:     types.SeverityWarning,
			backend: "A10-126",
		},
		{
			name: "ram",
			reject: backendRejectionR460{
				BackendID: "ASTRA-I9", Reason: rejectRAMCriticalR460,
				Detail:  "RAM занята 96% > порога 85%",
				Numbers: &gateNumbersR700{Percent: 96, Threshold: 85},
			},
			want:    []string{"96%", "85%"},
			gate:    gateKindRAMR700,
			sev:     types.SeverityCritical,
			backend: "ASTRA-I9",
		},
		{
			name: "vram",
			reject: backendRejectionR460{
				BackendID: "ASTRA-I9", Reason: rejectVRAMInsufficientR460,
				Detail:  "модель не влезает: need 16.1 GiB, free 4.9 GiB",
				Numbers: &gateNumbersR700{NeedMB: 16486, FreeMB: 5055},
			},
			want:    []string{"16486", "5055", gateTestModelR700},
			gate:    gateKindVRAMR700,
			sev:     types.SeverityWarning,
			backend: "ASTRA-I9",
		},
		{
			name: "window",
			reject: backendRejectionR460{
				BackendID: "A10-127", Reason: rejectWindowTooSmallR460,
				Detail:  "окно кандидата меньше нужного: candidate=65536 need=131072",
				Numbers: &gateNumbersR700{WindowHave: 65536, WindowNeed: 131072},
			},
			want:    []string{"65536", "131072"},
			gate:    gateKindWindowR700,
			sev:     types.SeverityWarning,
			backend: "A10-127",
		},
		{
			name: "slots",
			reject: backendRejectionR460{
				BackendID: "A10-126", Reason: rejectSlotsBusyR460,
				Detail:  "заняты все слоты (1/1)",
				Numbers: &gateNumbersR700{SlotsActive: 1, SlotsMax: 1},
			},
			want:    []string{"1/1"},
			gate:    gateKindSlotsR700,
			sev:     types.SeverityInfo,
			backend: "A10-126",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p.noteGateRejectionsR700(gateTestModelR700, []backendRejectionR460{tc.reject})
			ev, ok := waitGateEventR700(t, ch, 2*time.Second)
			if !ok {
				t.Fatalf("гейт %q не опубликовал уведомление", tc.name)
			}
			for _, w := range tc.want {
				if !strings.Contains(ev.Message, w) {
					t.Errorf("гейт %q: в тексте нет %q: %q", tc.name, w, ev.Message)
				}
			}
			if gate, _ := ev.Data["gate"].(string); gate != tc.gate {
				t.Errorf("гейт %q: Data[gate] = %v, want %q", tc.name, ev.Data["gate"], tc.gate)
			}
			if ev.Severity != tc.sev {
				t.Errorf("гейт %q: severity = %q, want %q", tc.name, ev.Severity, tc.sev)
			}
			if ev.BackendID != tc.backend {
				t.Errorf("гейт %q: backend = %q, want %q", tc.name, ev.BackendID, tc.backend)
			}
			// Повтор того же состояния — молчание (дедуп).
			p.noteGateRejectionsR700(gateTestModelR700, []backendRejectionR460{tc.reject})
			if ev2, got := waitGateEventR700(t, ch, 150*time.Millisecond); got {
				t.Errorf("гейт %q: повтор наблюдения опубликовал второе событие (%q) — дедуп не работает", tc.name, ev2.Message)
			}
		})
	}
}

// TestR700_AdmissionUnavailableCarriesGateNotices — причина видна БЕЗ
// колокольчика: в теле 503 теми же словами и с теми же числами.
func TestR700_AdmissionUnavailableCarriesGateNotices(t *testing.T) {
	p := gateTestProxyR700(t, 9480, 10240)
	rec := httptest.NewRecorder()
	writeAdmissionUnavailable(rec, p, gateTestModelR700, "sess-r700", 3,
		5*time.Second, 5*time.Second)
	body := rec.Body.String()
	if !strings.Contains(body, "gate_notices") {
		t.Fatalf("в теле 503 нет gate_notices — оператор/клиент видит только машинные коды: %s", body)
	}
	for _, want := range []string{"9480", "10240", gateTestBackendR700, "why_queued_human"} {
		if !strings.Contains(body, want) {
			t.Errorf("в теле 503 нет %q: %s", want, body)
		}
	}
	// Снимок для страниц собирается тем же проходом — он обязан быть непуст.
	if notices := p.GateNoticesR700(); len(notices) == 0 {
		t.Fatal("снимок причин пуст после ответа 503: страница «Очередь» не покажет причину")
	}
}

// TestR700_DiskThresholdIsHotAndReversible — порог диска применяется без
// рестарта и так же обратимо.
//
// PUT /api/v1/cluster/config пишет в ТОТ ЖЕ объект config, который читает гейт
// (proxy и api получают один указатель из main.go), поэтому тест меняет поле
// через указатель — ровно то, что делает обработчик.
func TestR700_DiskThresholdIsHotAndReversible(t *testing.T) {
	p := gateTestProxyR700(t, 9480, 10240)

	blocked := p.backendRejectionsR460(gateTestModelR700, types.BackendTypeLlamaCpp)
	if !hasRejectionR700(blocked, rejectDiskLowR460) {
		t.Fatalf("при пороге 10240 и свободных 9480 МБ диск обязан отбраковывать узел: %+v", blocked)
	}
	p.noteGateRejectionsR700(gateTestModelR700, blocked)
	if len(p.GateNoticesR700()) == 0 {
		t.Fatal("снимок причин пуст, хотя гейт сработал")
	}

	// Снижаем порог — как PUT /api/v1/cluster/config {"minFreeDisk": 9000}.
	p.config.Resources.Disk.MinFreeMB = 9000
	after := p.backendRejectionsR460(gateTestModelR700, types.BackendTypeLlamaCpp)
	if hasRejectionR700(after, rejectDiskLowR460) {
		t.Fatalf("после снижения порога до 9000 МБ узел всё ещё отбракован по диску: %+v — "+
			"значит настройка не применяется без перезапуска", after)
	}
	if notices := p.noteGateRejectionsR700(gateTestModelR700, after); len(notices) != 0 {
		t.Fatalf("после снятия причины снимок всё ещё содержит %d записей: %+v — "+
			"на странице висел бы баннер о том, чего уже нет", len(notices), notices)
	}

	// Обратимость: вернули порог — вернулся отказ и новое уведомление.
	p.config.Resources.Disk.MinFreeMB = 10240
	back := p.backendRejectionsR460(gateTestModelR700, types.BackendTypeLlamaCpp)
	if !hasRejectionR700(back, rejectDiskLowR460) {
		t.Fatalf("после возврата порога 10240 отказ не вернулся: %+v", back)
	}
	p.noteGateRejectionsR700(gateTestModelR700, back)
	if len(p.GateNoticesR700()) == 0 {
		t.Fatal("после возврата порога причина не попала в снимок")
	}

	// Снятие причины — то, что делает PUT /api/v1/cluster/config, меняя порог:
	// страница и баннер не должны рассказывать об исключённом узле, который по
	// новому порогу проходит (иначе это выглядит как «настройка не применилась»).
	p.ClearGateNoticesR700(GateDiskR700)
	if notices := p.GateNoticesR700(); len(notices) != 0 {
		t.Fatalf("после ClearGateNoticesR700(%q) снимок не пуст: %+v", GateDiskR700, notices)
	}
}

// hasRejectionR700 — есть ли в списке причин отказ с указанным кодом.
func hasRejectionR700(list []backendRejectionR460, reason string) bool {
	for _, r := range list {
		if r.Reason == reason {
			return true
		}
	}
	return false
}
