//go:build windows

package agent

import "testing"

// TestParseWMICCPUCSV_RealFormat — R66c (2026-09-22) регресс-тест.
//
// `wmic cpu get Name,NumberOfCores,NumberOfLogicalProcessors /format:csv`
// печатает 4 колонки в алфавитном порядке:
//
//	Node,Name,NumberOfCores,NumberOfLogicalProcessors
//
// Прежний код читал parts[2]/[3]/[4]: имя модели бралось из NumberOfCores,
// а parts[4] вообще выходил за границы среза —
// "index out of range [4] with length 4", и паника роняла весь процесс агента
// (вместе с метриками, которые он отправляет в балансер и WebUI).
func TestParseWMICCPUCSV_RealFormat(t *testing.T) {
	out := "Node,Name,NumberOfCores,NumberOfLogicalProcessors\r\n" +
		"NODE,AMD Ryzen 9 5950X 16-Core Processor,16,32\r\n"

	metrics := parseWMICCPUCSV(out)

	if metrics.Model != "AMD Ryzen 9 5950X 16-Core Processor" {
		t.Errorf("model: got %q, want AMD Ryzen 9 5950X 16-Core Processor", metrics.Model)
	}
	if metrics.CoreCount != 16 {
		t.Errorf("core count: got %d, want 16", metrics.CoreCount)
	}
	if metrics.ThreadCount != 32 {
		t.Errorf("thread count: got %d, want 32", metrics.ThreadCount)
	}
}

// TestParseWMICCPUCSV_NoPanicOnTruncatedLine — усечённая строка (VM без
// NumberOfLogicalProcessors) не должна паниковать.
func TestParseWMICCPUCSV_NoPanicOnTruncatedLine(t *testing.T) {
	out := "Node,Name,NumberOfCores,NumberOfLogicalProcessors\r\n" +
		"NODE,Intel(R) Xeon(R) CPU E5-2670,4\r\n"

	metrics := parseWMICCPUCSV(out)

	if metrics.Model != "Intel(R) Xeon(R) CPU E5-2670" {
		t.Errorf("model: got %q", metrics.Model)
	}
	if metrics.CoreCount != 4 {
		t.Errorf("core count: got %d, want 4", metrics.CoreCount)
	}
	if metrics.ThreadCount != 0 {
		t.Errorf("thread count: got %d, want 0 (колонки нет)", metrics.ThreadCount)
	}
}

// TestParseWMICCPUCSV_ColumnOrderIndependent — индексы берутся из заголовка,
// поэтому другой порядок колонок разбирается правильно.
func TestParseWMICCPUCSV_ColumnOrderIndependent(t *testing.T) {
	out := "Node,NumberOfLogicalProcessors,Name,NumberOfCores\r\n" +
		"NODE,8,AMD Ryzen 7 PRO 5750G,4\r\n"

	metrics := parseWMICCPUCSV(out)

	if metrics.Model != "AMD Ryzen 7 PRO 5750G" || metrics.CoreCount != 4 || metrics.ThreadCount != 8 {
		t.Errorf("порядок колонок не учтён: got model=%q cores=%d threads=%d",
			metrics.Model, metrics.CoreCount, metrics.ThreadCount)
	}
}

// TestParseWMICCPUCSV_Empty — пустой вывод и мусор не должны ломать разбор.
func TestParseWMICCPUCSV_Empty(t *testing.T) {
	for _, out := range []string{"", "\n\n", "не csv вовсе"} {
		metrics := parseWMICCPUCSV(out)
		if metrics.Model != "" || metrics.CoreCount != 0 || metrics.ThreadCount != 0 {
			t.Errorf("для %q ожидались нулевые метрики, got %+v", out, metrics)
		}
	}
}
