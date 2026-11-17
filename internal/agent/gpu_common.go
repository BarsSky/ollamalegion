package agent

import (
	"os/exec"
	"strconv"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// nvidiaSmiFieldsR495 — порядок колонок запроса. ВЫНЕСЕН В КОНСТАНТУ, чтобы
// тест мог сверить его с индексами парсера.
//
// ЗАЧЕМ (R495 §3.1): новые колонки добавляются ТОЛЬКО В КОНЕЦ. Первые 11 —
// исторические (R88: мощность дробью и т.п.), и на их индексы опираются и парсер,
// и тесты. Вставка в середину ломает все поля сразу, причём молча: значения
// остаются числами, но означают другое. Мутация «переставить колонку» обязана
// краснить сторож — иначе такой сдвиг не поймает ни один тест парсера (он
// получает готовые CSV-строки и о запросе ничего не знает).
const nvidiaSmiFieldsR495 = "index,name,utilization.gpu,memory.total,memory.used,memory.free," +
	"temperature.gpu,power.draw,power.limit,clocks.gr,clocks.mem," + // 0..10 — исторические
	"utilization.memory,clocks_throttle_reasons.active,pstate," + // 11..13
	"pcie.link.gen.current,pcie.link.width.current" // 14..15

// executeNvidiaSmi - выполнение nvidia-smi и получение вывода
func executeNvidiaSmi() (string, error) {
	// Попытка выполнить nvidia-smi с нужными параметрами
	cmd := exec.Command("nvidia-smi",
		"--query-gpu="+nvidiaSmiFieldsR495,
		"--format=csv,noheader,nounits")

	output, err := cmd.Output()
	if err != nil {
		return "", err
	}

	return string(output), nil
}

// countGPUs - подсчет количества GPU из вывода nvidia-smi
func countGPUs(output string) int {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	return len(lines)
}

// collectGPUUUIDs — физические идентификаторы карт этой машины.
//
// ЗАЧЕМ (R-Image follow-up, 2026-10-07): балансер обязан отличать «две машины по
// одной карте» от «одна машина, два бэкенда». Имя хоста для этого не годится —
// контейнеры на одном хосте регистрируются под разными именами
// (cppworker-gpu, imageworker), поэтому балансер видел две «машины» и складывал
// память одной карты дважды. UUID карты у контейнеров одного хоста совпадает
// побайтово, а у разных физических карт — различается.
//
// Пустой результат означает «неизвестно» (нет nvidia-smi, нет прав, не-nvidia
// платформа): вызывающая сторона трактует это как «отдельная машина», то есть
// сохраняет прежнее поведение и ничего не ломает.
//
// Своя команда, а не общий запрос метрик: UUID — строковый столбец, и добавлять
// его в parseNvidiaSmiOutput значило бы переиндексировать все числовые поля
// (тесты и NVML-путь опираются на текущий порядок).
func collectGPUUUIDs() []string {
	cmd := exec.Command("nvidia-smi", "--query-gpu=uuid", "--format=csv,noheader")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var uuids []string
	for _, line := range strings.Split(string(out), "\n") {
		u := strings.TrimSpace(line)
		if u == "" {
			continue
		}
		uuids = append(uuids, u)
	}
	return uuids
}

// parseGPUMModels - парсинг названий GPU моделей
func parseGPUMModels(output string) []string {
	var models []string

	lines := strings.Split(strings.TrimSpace(output), "\n")
	for _, line := range lines {
		parts := strings.Split(line, ",")
		if len(parts) >= 2 {
			modelName := strings.TrimSpace(parts[1])
			if modelName != "" {
				models = append(models, modelName)
			}
		}
	}

	return models
}

// parseNvidiaSmiOutput - парсинг вывода nvidia-smi в метрики
func parseNvidiaSmiOutput(output string) types.GPUMetrics {
	metrics := types.GPUMetrics{}

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 {
		return metrics
	}

	// Парсим первую строку (первый GPU)
	// Формат: index, name, utilization.gpu, memory.total, memory.used, memory.free, temperature.gpu, power.draw, power.limit, clocks.gr, clocks.mem
	line := lines[0]
	parts := strings.Split(line, ",")

	if len(parts) >= 11 {
		// Usage percent
		if val, err := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64); err == nil {
			metrics.UsagePercent = val
		}

		// Memory total (MB)
		if val, err := strconv.ParseUint(strings.TrimSpace(parts[3]), 10, 64); err == nil {
			metrics.MemoryTotal = val
		}

		// Memory used (MB)
		if val, err := strconv.ParseUint(strings.TrimSpace(parts[4]), 10, 64); err == nil {
			metrics.MemoryUsed = val
		}

		// Memory free (MB)
		if val, err := strconv.ParseUint(strings.TrimSpace(parts[5]), 10, 64); err == nil {
			metrics.MemoryFree = val
		}

		// Temperature
		if val, err := strconv.Atoi(strings.TrimSpace(parts[6])); err == nil {
			metrics.Temperature = val
		}

		// Power draw (W).
		//
		// R88 (2026-10-08): nvidia-smi с `--format=csv,noheader,nounits` печатает
		// мощность ДРОБЬЮ («25.49»), поэтому strconv.Atoi здесь ВСЕГДА падал и в
		// панели на каждой карточке было «POWER 0W» — при том что nvidia-smi
		// внутри контейнера отдаёт реальные 22–47 W. Разбираем как float.
		if val, err := strconv.ParseFloat(strings.TrimSpace(parts[7]), 64); err == nil {
			metrics.PowerUsage = int(val + 0.5)
		}

		// Power limit (W) — та же дробная форма («100.00»).
		if val, err := strconv.ParseFloat(strings.TrimSpace(parts[8]), 64); err == nil {
			metrics.PowerLimit = int(val + 0.5)
		}

		// GPU Clock (MHz)
		if val, err := strconv.Atoi(strings.TrimSpace(parts[9])); err == nil {
			metrics.GPUClock = val
		}

		// Memory Clock (MHz)
		if val, err := strconv.Atoi(strings.TrimSpace(parts[10])); err == nil {
			metrics.MemClock = val
		}
	}

	// R495 (2026-11-18): новые столбцы — ТОЛЬКО В КОНЕЦ запроса (индексы 11+).
	//
	// Почему в конец, а не «по порядку»: на текущий порядок опираются и
	// NVML-путь, и тесты; вставка в середину переиндексировала бы все поля
	// (см. collectGPUUUIDs выше — UUID поэтому и собирается отдельной командой).
	// Старый вывод (11 колонок) обязан парситься как раньше: цикл ниже просто не
	// выполнится, и новые поля останутся 0 = «неизвестно».
	metrics.PState = -1 // «неизвестно»: 0 — это законный P0
	if len(parts) >= 12 {
		if val, err := strconv.ParseFloat(strings.TrimSpace(parts[11]), 64); err == nil {
			metrics.MemBusyPercent = val
		}
	}
	if len(parts) >= 14 {
		// clocks_throttle_reasons.active печатается как hex-маска
		// (например 0x0000000000000008) либо как «N/A»/«[Not Supported]».
		if mask, ok := parseThrottleMask(strings.TrimSpace(parts[12])); ok {
			metrics.ThrottleReasons = mask
			metrics.ThrottleKnown = true
			metrics.ThrottleReasonsList = throttleReasonNames(mask)
		}
		if val, err := strconv.Atoi(strings.TrimSpace(parts[13])); err == nil {
			metrics.PState = val
		}
	}
	if len(parts) >= 16 {
		if val, err := strconv.Atoi(strings.TrimSpace(parts[14])); err == nil {
			metrics.PCIELinkGen = val
		}
		if val, err := strconv.Atoi(strings.TrimSpace(parts[15])); err == nil {
			metrics.PCIELinkWidth = val
		}
	}

	return metrics
}

// parseThrottleMask — разбор `clocks_throttle_reasons.active`.
//
// Формы, которые реально приходят: `0x0000000000000008` (hex), `8` (dec),
// `N/A`, `[Not Supported]`. Отсутствие значения — это НЕ «троттлинга нет»:
// возвращаем ok=false, и вызывающий оставляет ThrottleKnown=false.
func parseThrottleMask(raw string) (uint64, bool) {
	if raw == "" || strings.EqualFold(raw, "N/A") || strings.HasPrefix(raw, "[Not") {
		return 0, false
	}
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(raw), "0x") {
		if v, err := strconv.ParseUint(raw[2:], 16, 64); err == nil {
			return v, true
		}
		return 0, false
	}
	if v, err := strconv.ParseUint(raw, 10, 64); err == nil {
		return v, true
	}
	return 0, false
}

// throttleReasonNames — маска причин троттлинга словами (для панели и лога).
//
// Значения — те же биты, что у NVML (nvml.h): 1 gpu_idle, 2
// applications_clocks_setting, 4 sw_power_cap, 8 hw_slowdown, 64
// hw_thermal_slowdown, 128 hw_power_brake_slowdown. Список имён не берём из
// биндинга, чтобы Windows-путь (nvidia-smi) не тянул cgo-константы.
func throttleReasonNames(mask uint64) []string {
	if mask == 0 {
		return nil
	}
	type reason struct {
		bit  uint64
		name string
	}
	all := []reason{
		{1, "gpu_idle"},
		{2, "applications_clocks_setting"},
		{4, "sw_power_cap"},
		{8, "hw_slowdown"},
		{64, "hw_thermal_slowdown"},
		{128, "hw_power_brake_slowdown"},
	}
	var out []string
	for _, r := range all {
		if mask&r.bit != 0 {
			out = append(out, r.name)
		}
	}
	return out
}