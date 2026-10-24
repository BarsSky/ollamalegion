// capacity_unknown_r117_test.go — R117 (2026-10-14): сброс слотов при выгрузке и
// бюджет ожидания в очереди.
//
// Два живых замера со стенда, которые эти тесты держат:
//
//  1. СТАРЫЕ СЛОТЫ. poller.adoptCapacityFromLoadedModels сбрасывал значение
//     только когда модель ЗАГРУЖЕНА (slots > 0). После unload он выходил рано, и
//     RuntimeModelSlots оставался от предыдущей модели: unload при прошлой
//     загрузке max_slots=2 → /api/v1/backends показывал maxConcurrentRequests=2
//     при count=0 на узле; следующая загрузка с parallel=1 оставляла там 1. То
//     есть число было следом истории, а не фактом.
//
//  2. 503 РАНЬШЕ ЗАГРУЗКИ. Два запроса к выгруженной модели: первый ушёл
//     загружать (загрузка ~345 с), второй встал в admission-очередь и получил 503
//     «waited in queue and timed out» ровно на 300-й секунде (LB_ADMISSION_WAIT_SEC),
//     за 45 с до готовности модели, хотя был первым в очереди за слотом.
//     Бюджет ожидания обязан быть не меньше ожидания авто-загрузки
//     (LB_AUTO_LOAD_WAIT_SEC).
package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestR117_UnloadResetsModelSlots — выгрузка модели обнуляет фактические слоты, и
// вместимость не остаётся от прошлой загрузки.
func TestR117_UnloadResetsModelSlots(t *testing.T) {
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "true")
	p := capacityHarness(t, 2, 0)
	poller := &llamaCppMetricsPoller{proxy: p}

	// Модель загружена с двумя слотами — вместимость 2.
	poller.adoptCapacityFromLoadedModels("bk", []types.LlamaCppModel{
		{Name: "m", State: "loaded", MaxSlots: 2},
	})
	if got := p.backends["bk"].Backend.EffectiveMaxConcurrentRequests(); got != 2 {
		t.Fatalf("после загрузки cap = %d, want 2", got)
	}

	// Модель выгружена: узла нет ни одной загруженной модели.
	poller.adoptCapacityFromLoadedModels("bk", nil)

	b := p.backends["bk"].Backend
	if b.RuntimeModelSlots != 0 {
		t.Errorf("RuntimeModelSlots = %d, want 0: слоты выгруженной модели недействительны",
			b.RuntimeModelSlots)
	}
	got, source := b.ResolveCapacity()
	if got != 1 || source != types.CapacitySourceModelSlotsUnknown {
		t.Errorf("ResolveCapacity() = (%d, %q), want (1, %q): после выгрузки балансер "+
			"обязан вернуться к консервативной вместимости, а не жить слотами прошлой модели",
			got, source, types.CapacitySourceModelSlotsUnknown)
	}
}

// TestR117_LoadingModelResetsSlots — пока модель ГРУЗИТСЯ, слотов тоже нет:
// cppworker отдаёт max_slots=1 как заглушку (parallel ещё 0), и раздавать два
// слота нельзя. Живой замер: во время загрузки max_slots=1, parallel=0.
func TestR117_LoadingModelResetsSlots(t *testing.T) {
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "true")
	p := capacityHarness(t, 2, 0)
	poller := &llamaCppMetricsPoller{proxy: p}

	poller.adoptCapacityFromLoadedModels("bk", []types.LlamaCppModel{
		{Name: "m", State: "loading", MaxSlots: 1, Parallel: 0},
	})

	b := p.backends["bk"].Backend
	if b.RuntimeModelSlots != 0 {
		t.Errorf("RuntimeModelSlots = %d, want 0: у loading-модели слотов ещё нет", b.RuntimeModelSlots)
	}
	if got := b.EffectiveMaxConcurrentRequests(); got != 1 {
		t.Errorf("EffectiveMaxConcurrentRequests() = %d, want 1 во время загрузки", got)
	}
}

// TestR117_AdmissionBudgetCoversAutoLoadWait — бюджет ожидания = max(admission,
// auto-load): запрос не должен получить 503 раньше, чем закончится загрузка, за
// которой он стоит.
func TestR117_AdmissionBudgetCoversAutoLoadWait(t *testing.T) {
	p := &Proxy{admission: newAdmissionQueue(300 * time.Second)}

	t.Setenv("LB_ADMISSION_WAIT_SEC", "300")
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "900")
	if got := p.admissionWaitBudget(); got != 900*time.Second {
		t.Errorf("admissionWaitBudget() = %s, want 900s (живой замер: загрузка ~345 c "+
			"при лимите очереди 300 c давала 503 за 45 c до готовности модели)", got)
	}

	// Ожидание авто-загрузки короче — берём лимит очереди.
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "60")
	if got := p.admissionWaitBudget(); got != 300*time.Second {
		t.Errorf("admissionWaitBudget() = %s, want 300s", got)
	}

	// Ожидание слотов выключено — бюджет 0 (прежнее «сразу 503»). Предел живёт в
	// очереди (читается из env при создании Proxy), поэтому выключаем его так же,
	// как это делает Shutdown/тесты.
	p.setAdmissionWait(0)
	if got := p.admissionWaitBudget(); got != 0 {
		t.Errorf("admissionWaitBudget() = %s, want 0 при выключенном ожидании", got)
	}
}

// TestR117_SetBackendModelSlotsAcceptsReset — 0 = сброс, отрицательное игнорируется.
func TestR117_SetBackendModelSlotsAcceptsReset(t *testing.T) {
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "true")
	p := capacityHarness(t, 2, 0)
	p.backends["bk"].Backend.RuntimeModelSlots = 3

	p.SetBackendModelSlots("bk", 0)
	if got := p.backends["bk"].Backend.RuntimeModelSlots; got != 0 {
		t.Errorf("RuntimeModelSlots = %d, want 0 (сброс)", got)
	}

	p.SetBackendModelSlots("bk", -1)
	if got := p.backends["bk"].Backend.RuntimeModelSlots; got != 0 {
		t.Errorf("RuntimeModelSlots = %d, want 0 (отрицательное значение не пишется)", got)
	}
}
