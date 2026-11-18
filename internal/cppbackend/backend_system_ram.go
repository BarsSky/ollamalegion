package cppbackend

import (
	"fmt"
	"os"
	"strings"
)

// getSystemRAMGB возвращает общий объём системной RAM в GB.
// На Linux читает /proc/meminfo; на других ОС возвращает fallback 16GB.
func getSystemRAMGB() uint64 {
	// Пытаемся прочитать /proc/meminfo (Linux)
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		// macOS или другие — fallback
		return 16
	}

	// Ищем строку MemTotal
	content := string(data)
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			// MemTotal:   16384000 kB  → число в kB
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				var kB uint64
				if _, err := fmt.Sscanf(parts[1], "%d", &kB); err == nil {
					return kB / 1024 / 1024 // kB → GB
				}
			}
		}
	}
	return 16 // fallback
}
