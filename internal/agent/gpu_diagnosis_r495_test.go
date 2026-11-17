//go:build llama_stub

// gpu_diagnosis_r495_test.go — R495 шаг S1 (2026-11-18).
//
// ЗАЧЕМ ЭТИ ТЕСТЫ. Задача выросла из дефекта наблюдаемости: карта, упершаяся в
// полосу памяти, выглядела свободной, потому что балансер читал только
// `utilization.gpu` (SM busy). S1 довозит до балансера занятость памяти, причины
// троттлинга, PState и PCIe. Но у S1 есть два способа «сломаться тихо»:
//
//  1. старый формат вывода nvidia-smi (11 колонок) должен парситься КАК РАНЬШЕ —
//     иначе новые столбцы в середине запроса переиндексируют все поля и в
//     панели поедут температуры/частоты;
//  2. `N/A`/`[Not Supported]` от драйвера — это «не измерено», а НЕ ноль:
//     публикация нуля повторяет исходный дефект («память свободна»).
//
// Плюс ловушка слияния (collector.go mergeGPUMetrics): «локальный опрос
// состоялся» проверяется по старым полям, и карта может отдать только новые
// (mem busy / throttle) при нулевых temperature/clock — тогда новые поля
// обязаны выжить, иначе классификатор всегда видел бы unknown.
package agent

import (
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// nvidiaSmi11R495 — старый формат: 11 колонок, как до R495.
const nvidiaSmi11R495 = "0, NVIDIA GeForce RTX 3070, 55, 8192, 4096, 4096, 62, 41.53, 220.00, 1815, 7001\n"

// nvidiaSmi16R495 — новый формат: те же 11 колонок + 5 новых В КОНЦЕ.
const nvidiaSmi16R495 = "0, NVIDIA A10, 62, 23028, 20000, 3028, 58, 141.20, 150.00, 1695, 6251, 96, 0x0000000000000004, 2, 4, 16\n"

// TestR495_ParserElevenColumns — регрессия: старый вывод парсится как раньше,
// новые поля остаются «неизвестно» (mem busy 0, pstate -1, throttle не измерен).
func TestR495_ParserElevenColumns(t *testing.T) {
	m := parseNvidiaSmiOutput(nvidiaSmi11R495)

	if m.UsagePercent != 55 {
		t.Errorf("UsagePercent = %v, ожидалось 55 (переиндексация старого формата?)", m.UsagePercent)
	}
	if m.MemoryTotal != 8192 || m.MemoryUsed != 4096 || m.MemoryFree != 4096 {
		t.Errorf("память = %d/%d/%d, ожидалось 8192/4096/4096 — старый формат сломан",
			m.MemoryTotal, m.MemoryUsed, m.MemoryFree)
	}
	if m.PowerUsage != 42 || m.PowerLimit != 220 {
		t.Errorf("питание = %d/%d, ожидалось 42/220", m.PowerUsage, m.PowerLimit)
	}
	if m.GPUClock != 1815 || m.MemClock != 7001 {
		t.Errorf("частоты = %d/%d, ожидались 1815/7001", m.GPUClock, m.MemClock)
	}

	if m.MemBusyPercent != 0 {
		t.Errorf("MemBusyPercent = %v в старом формате, ожидалось 0 = «не измерено»", m.MemBusyPercent)
	}
	if m.ThrottleKnown {
		t.Error("ThrottleKnown=true в старом формате: отсутствие данных выдано за измерение")
	}
	if m.PState != -1 {
		t.Errorf("PState = %d в старом формате, ожидалось -1 = «неизвестно» (0 = законный P0)", m.PState)
	}
}

// TestR495_ParserSixteenColumns — новый вывод даёт занятость памяти, маску
// троттлинга (hex), PState и PCIe.
func TestR495_ParserSixteenColumns(t *testing.T) {
	m := parseNvidiaSmiOutput(nvidiaSmi16R495)

	if m.MemBusyPercent != 96 {
		t.Errorf("MemBusyPercent = %v, ожидалось 96", m.MemBusyPercent)
	}
	if !m.ThrottleKnown || m.ThrottleReasons != 4 {
		t.Errorf("троттлинг = (%v, %d), ожидалось (true, 4 = sw_power_cap)", m.ThrottleKnown, m.ThrottleReasons)
	}
	if len(m.ThrottleReasonsList) != 1 || m.ThrottleReasonsList[0] != "sw_power_cap" {
		t.Errorf("ThrottleReasonsList = %v, ожидалось [sw_power_cap]", m.ThrottleReasonsList)
	}
	if m.PState != 2 {
		t.Errorf("PState = %d, ожидалось 2", m.PState)
	}
	if m.PCIELinkGen != 4 || m.PCIELinkWidth != 16 {
		t.Errorf("PCIe = gen%d x%d, ожидалось gen4 x16", m.PCIELinkGen, m.PCIELinkWidth)
	}
	// Старые поля не поехали.
	if m.UsagePercent != 62 || m.MemoryTotal != 23028 || m.MemClock != 6251 {
		t.Errorf("старые поля сломаны новыми столбцами: usage=%v memTotal=%d memClock=%d",
			m.UsagePercent, m.MemoryTotal, m.MemClock)
	}
}

// TestR495_ParserNAStaysUnknown — `N/A` и `[Not Supported]` = «не измерено».
func TestR495_ParserNAStaysUnknown(t *testing.T) {
	line := "0, NVIDIA A10, 62, 23028, 20000, 3028, 58, 141.20, 150.00, 1695, 6251, N/A, [Not Supported], N/A, 4, 16\n"
	m := parseNvidiaSmiOutput(line)

	if m.MemBusyPercent != 0 {
		t.Errorf("MemBusyPercent = %v при N/A, ожидалось 0", m.MemBusyPercent)
	}
	if m.ThrottleKnown {
		t.Error("ThrottleKnown=true при [Not Supported]: «нет данных» выдано за «троттлинга нет»")
	}
	if m.PState != -1 {
		t.Errorf("PState = %d при N/A, ожидалось -1", m.PState)
	}
	// PCIe при этом измерен — не смешиваем: одно поле N/A не отменяет другие.
	if m.PCIELinkGen != 4 || m.PCIELinkWidth != 16 {
		t.Errorf("PCIe потерян из-за N/A в других колонках: gen%d x%d", m.PCIELinkGen, m.PCIELinkWidth)
	}
}

// TestR495_ThrottleMaskForms — маска приходит hex, dec или мусором.
func TestR495_ThrottleMaskForms(t *testing.T) {
	cases := []struct {
		raw  string
		want uint64
		ok   bool
	}{
		{"0x0000000000000004", 4, true},
		{"0x0000000000000048", 72, true}, // 8 hw_slowdown + 64 hw_thermal
		{"4", 4, true},
		{"N/A", 0, false},
		{"[Not Supported]", 0, false},
		{"", 0, false},
		{"мусор", 0, false},
	}
	for _, c := range cases {
		got, ok := parseThrottleMask(c.raw)
		if got != c.want || ok != c.ok {
			t.Errorf("parseThrottleMask(%q) = (%d, %v), ожидалось (%d, %v)", c.raw, got, ok, c.want, c.ok)
		}
	}
}

// TestR495_ThrottleReasonNames — маска → слова для панели.
func TestR495_ThrottleReasonNames(t *testing.T) {
	names := throttleReasonNames(4 | 64)
	if len(names) != 2 || names[0] != "sw_power_cap" || names[1] != "hw_thermal_slowdown" {
		t.Errorf("throttleReasonNames(4|64) = %v, ожидалось [sw_power_cap hw_thermal_slowdown]", names)
	}
	if got := throttleReasonNames(0); got != nil {
		t.Errorf("throttleReasonNames(0) = %v, ожидалось nil (нет причин — нет списка)", got)
	}
}

// TestR495_MergeKeepsNewFieldsWithoutOldOnes — ЛОВУШКА collector.go:974:
// локальный опрос «не состоялся» по старым полям, но новые измерены — они
// обязаны пережить слияние.
func TestR495_MergeKeepsNewFieldsWithoutOldOnes(t *testing.T) {
	engine := types.GPUMetrics{
		UsagePercent: 40,
		MemoryTotal:  23028,
		MemoryUsed:   20000,
		// Движок отдал снимок, но про память-полосу ничего не знает.
	}
	local := types.GPUMetrics{
		// temperature/clock/usage = 0 → localUsable == false
		MemBusyPercent:      96,
		ThrottleKnown:       true,
		ThrottleReasons:     4,
		ThrottleReasonsList: []string{"sw_power_cap"},
		PState:              0,
		PCIELinkGen:         4,
		PCIELinkWidth:       16,
	}

	merged := mergeGPUMetrics(engine, local)

	if merged.MemBusyPercent != 96 {
		t.Errorf("MemBusyPercent = %v после слияния — новые поля потеряны (ловушка :974)", merged.MemBusyPercent)
	}
	if !merged.ThrottleKnown || merged.ThrottleReasons != 4 {
		t.Errorf("троттлинг потерян при слиянии: known=%v mask=%d", merged.ThrottleKnown, merged.ThrottleReasons)
	}
	if merged.PState != 0 {
		t.Errorf("PState = %d, ожидалось 0 (P0 — законное значение, не «неизвестно»)", merged.PState)
	}
	if merged.PCIELinkGen != 4 || merged.PCIELinkWidth != 16 {
		t.Errorf("PCIe потерян при слиянии: gen%d x%d", merged.PCIELinkGen, merged.PCIELinkWidth)
	}
	// Снимок памяти движка сохраняется, если локально память не измерена.
	if merged.MemoryTotal != 23028 || merged.MemoryUsed != 20000 {
		t.Errorf("память движка затёрта: total=%d used=%d", merged.MemoryTotal, merged.MemoryUsed)
	}
}

// TestR495_MergeKeepsEngineValuesWhenLocalUnknown — обратная сторона: локально
// ничего не измерено → значения движка не обнуляются.
func TestR495_MergeKeepsEngineValuesWhenLocalUnknown(t *testing.T) {
	engine := types.GPUMetrics{
		UsagePercent:    40,
		MemBusyPercent:  88,
		ThrottleKnown:   true,
		ThrottleReasons: 8,
		PState:          2,
	}
	local := types.GPUMetrics{PState: -1} // «неизвестно», а не P0

	merged := mergeGPUMetrics(engine, local)

	if merged.MemBusyPercent != 88 {
		t.Errorf("MemBusyPercent = %v, ожидалось 88 (движковое значение затёрто нулём)", merged.MemBusyPercent)
	}
	if merged.PState != 2 {
		t.Errorf("PState = %d, ожидалось 2: локальное -1 («неизвестно») не должно затирать измеренное", merged.PState)
	}
	if !merged.ThrottleKnown || merged.ThrottleReasons != 8 {
		t.Errorf("троттлинг движка затёрт: known=%v mask=%d", merged.ThrottleKnown, merged.ThrottleReasons)
	}
}

// TestR495_QueryColumnsOnlyAppended — сторож ПОРЯДКА колонок запроса.
//
// ЗАЧЕМ ОТДЕЛЬНО. Тесты парсера получают готовые CSV-строки и про запрос ничего
// не знают: если новые колонки вставить в середину запроса `--query-gpu`, парсер
// начнёт читать чужие значения, а тесты парсера останутся зелёными (проверено
// мутацией: перестановка колонки в запросе не краснила ни один тест). Здесь
// проверяется ровно §3.1 плана: первые 11 колонок — исторические, новые идут
// СТРОГО после них и в объявленном порядке.
func TestR495_QueryColumnsOnlyAppended(t *testing.T) {
	historical := []string{
		"index", "name", "utilization.gpu", "memory.total", "memory.used",
		"memory.free", "temperature.gpu", "power.draw", "power.limit",
		"clocks.gr", "clocks.mem",
	}
	newOnes := []string{
		"utilization.memory", "clocks_throttle_reasons.active", "pstate",
		"pcie.link.gen.current", "pcie.link.width.current",
	}

	fields := strings.Split(nvidiaSmiFieldsR495, ",")
	if len(fields) != len(historical)+len(newOnes) {
		t.Fatalf("колонок в запросе %d, ожидалось %d: %v",
			len(fields), len(historical)+len(newOnes), fields)
	}
	for i, want := range historical {
		if fields[i] != want {
			t.Errorf("колонка %d = %q, ожидалась %q: исторический порядок сдвинут — "+
				"парсер читает чужие значения, все поля поедут молча", i, fields[i], want)
		}
	}
	for i, want := range newOnes {
		idx := len(historical) + i
		if fields[idx] != want {
			t.Errorf("новая колонка %d = %q, ожидалась %q: новые поля обязаны идти в конец "+
				"в объявленном порядке (парсер читает по индексам 11+)", idx, fields[idx], want)
		}
	}
}
