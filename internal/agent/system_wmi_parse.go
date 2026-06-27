package agent

import (
	"strconv"
	"strings"
)

// parseWmiVideoControllerVRAMBytes парсит CSV-вывод WMI-команды
//
//	wmic path Win32_VideoController get Name,AdapterRAM /format:csv
//
// и возвращает суммарный объём VRAM (в байтах) всех дискретных GPU.
// Исключает «Basic Display Adapter» — это всегда встроенная графика Windows
// с AdapterRAM = 0.
//
// Формат ввода (пример для одной RTX 3070 + Intel iGPU):
//
//	Node,AdapterRAM,Name
//	DESKTOP-ABC,8589934592,NVIDIA GeForce RTX 3070
//	DESKTOP-ABC,0,Intel(R) UHD Graphics 630
//	DESKTOP-ABC,0,Basic Display Adapter
//
// Возвращает сумму только ненулевых AdapterRAM, исключая Basic Display Adapter.
// Это позволяет тестировать парсер на любой платформе (Linux/macOS/Windows),
// потому что функция не зависит от ОС.
//
// Используется в getGPUMetricsWMIFallback (Windows) и в unit-тестах
// (internal/agent/system_wmi_parse_test.go).
func parseWmiVideoControllerVRAMBytes(csvOutput string) uint64 {
	var total uint64
	lines := strings.Split(csvOutput, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Node") {
			// Заголовок CSV-вывода wmic.
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 3 {
			// Неожиданный формат — пропускаем.
			continue
		}
		// Индексы в /format:csv:
		//   parts[0] = Node (hostname)
		//   parts[1] = AdapterRAM (bytes)
		//   parts[2] = Name
		name := strings.TrimSpace(parts[2])
		if name == "" {
			continue
		}
		// Пропускаем встроенную графику Windows без VRAM.
		if strings.Contains(strings.ToLower(name), "basic display") {
			continue
		}
		vramBytes, err := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil {
			// Нечисловое значение AdapterRAM (встречается на виртуалках
			// с динамической памятью) — пропускаем строку.
			continue
		}
		total += vramBytes
	}
	return total
}