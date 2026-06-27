package agent

import (
	"strings"
	"testing"
)

// TestParseWmiVideoControllerVRAMBytes_SingleGPU — один дискретный GPU с 8 GB VRAM.
// Ожидаем 8589934592 bytes = 8 GB.
func TestParseWmiVideoControllerVRAMBytes_SingleGPU(t *testing.T) {
	csvOutput := `Node,AdapterRAM,Name
DESKTOP-ABC,8589934592,NVIDIA GeForce RTX 3070
`

	got := parseWmiVideoControllerVRAMBytes(csvOutput)
	want := uint64(8589934592)
	if got != want {
		t.Errorf("parseWmiVideoControllerVRAMBytes: got %d, want %d (8 GB)", got, want)
	}
	// Проверяем, что MB = bytes / 1024 / 1024 корректно.
	mb := got / 1024 / 1024
	if mb != 8192 {
		t.Errorf("MemoryTotal MB: got %d, want 8192", mb)
	}
}

// TestParseWmiVideoControllerVRAMBytes_MultiGPU — два дискретных GPU (RTX 3090 + RTX 4090).
// Сумма: 24 GB + 24 GB = 48 GB = 51539607552 bytes.
func TestParseWmiVideoControllerVRAMBytes_MultiGPU(t *testing.T) {
	csvOutput := `Node,AdapterRAM,Name
DESKTOP-ABC,25769803776,NVIDIA GeForce RTX 3090
DESKTOP-ABC,25769803776,NVIDIA GeForce RTX 4090
`

	got := parseWmiVideoControllerVRAMBytes(csvOutput)
	want := uint64(25769803776 * 2)
	if got != want {
		t.Errorf("parseWmiVideoControllerVRAMBytes: got %d, want %d (48 GB)", got, want)
	}
}

// TestParseWmiVideoControllerVRAMBytes_BasicDisplayExcluded — проверяет, что
// «Basic Display Adapter» (Windows built-in display stub) исключается из суммы.
// Реальная ситуация: RTX 3070 + Intel iGPU + Basic Display Adapter.
func TestParseWmiVideoControllerVRAMBytes_BasicDisplayExcluded(t *testing.T) {
	csvOutput := `Node,AdapterRAM,Name
DESKTOP-ABC,8589934592,NVIDIA GeForce RTX 3070
DESKTOP-ABC,0,Intel(R) UHD Graphics 630
DESKTOP-ABC,0,Basic Display Adapter
`

	got := parseWmiVideoControllerVRAMBytes(csvOutput)
	want := uint64(8589934592) // только RTX 3070, без iGPU и Basic Display
	if got != want {
		t.Errorf("parseWmiVideoControllerVRAMBytes: got %d, want %d (8 GB, Basic Display исключён)", got, want)
	}
}

// TestParseWmiVideoControllerVRAMBytes_OnlyBuiltinGPU — машина без дискретной карты,
// только Basic Display Adapter. Должно вернуть 0 (показывает, что dedicated GPU нет).
func TestParseWmiVideoControllerVRAMBytes_OnlyBuiltinGPU(t *testing.T) {
	csvOutput := `Node,AdapterRAM,Name
DESKTOP-ABC,0,Intel(R) UHD Graphics 630
DESKTOP-ABC,0,Basic Display Adapter
`

	got := parseWmiVideoControllerVRAMBytes(csvOutput)
	if got != 0 {
		t.Errorf("parseWmiVideoControllerVRAMBytes: got %d, want 0 (нет дискретного GPU)", got)
	}
}

// TestParseWmiVideoControllerVRAMBytes_EmptyOutput — пустой вывод wmic (команда
// не сработала, например, на non-Windows хосте при ручном тестировании).
func TestParseWmiVideoControllerVRAMBytes_EmptyOutput(t *testing.T) {
	got := parseWmiVideoControllerVRAMBytes("")
	if got != 0 {
		t.Errorf("parseWmiVideoControllerVRAMBytes: got %d, want 0", got)
	}
}

// TestParseWmiVideoControllerVRAMBytes_HeaderOnly — только заголовок без данных.
func TestParseWmiVideoControllerVRAMBytes_HeaderOnly(t *testing.T) {
	csvOutput := `Node,AdapterRAM,Name`
	got := parseWmiVideoControllerVRAMBytes(csvOutput)
	if got != 0 {
		t.Errorf("parseWmiVideoControllerVRAMBytes: got %d, want 0", got)
	}
}

// TestParseWmiVideoControllerVRAMBytes_MalformedLine — строка с < 3 полей.
// Должна быть пропущена, а не вызвать panic.
func TestParseWmiVideoControllerVRAMBytes_MalformedLine(t *testing.T) {
	csvOutput := `Node,AdapterRAM,Name
DESKTOP-ABC,malformed_value,NVIDIA GeForce RTX 3070
INVALID_LINE_NO_COMMAS
DESKTOP-ABC,4294967296,AMD Radeon RX 7900 XT
`

	got := parseWmiVideoControllerVRAMBytes(csvOutput)
	want := uint64(4294967296) // только RX 7900 XT — «malformed_value» не парсится
	if got != want {
		t.Errorf("parseWmiVideoControllerVRAMBytes: got %d, want %d (только RX 7900 XT, malformed пропущена)", got, want)
	}
}

// TestParseWmiVideoControllerVRAMBytes_CRLF — wmic на Windows выводит \r\n.
// Должно корректно парситься (TrimSpace удаляет \r).
func TestParseWmiVideoControllerVRAMBytes_CRLF(t *testing.T) {
	csvOutput := "Node,AdapterRAM,Name\r\nDESKTOP-ABC,8589934592,NVIDIA GeForce RTX 3070\r\n"

	got := parseWmiVideoControllerVRAMBytes(csvOutput)
	want := uint64(8589934592)
	if got != want {
		t.Errorf("parseWmiVideoControllerVRAMBytes: got %d, want %d (CRLF)", got, want)
	}
}

// TestParseWmiVideoControllerVRAMBytes_RealisticSample — реалистичный пример
// с 3 GPU: NVIDIA RTX 4090 (24GB), NVIDIA RTX A6000 (48GB), Intel iGPU.
func TestParseWmiVideoControllerVRAMBytes_RealisticSample(t *testing.T) {
	csvOutput := `Node,AdapterRAM,Name
WORKSTATION-1,25769803776,NVIDIA GeForce RTX 4090
WORKSTATION-1,51539607552,NVIDIA RTX A6000
WORKSTATION-1,134217728,Intel(R) Arc(TM) A770 Graphics
WORKSTATION-1,0,Basic Display Adapter
`

	got := parseWmiVideoControllerVRAMBytes(csvOutput)
	want := uint64(25769803776 + 51539607552 + 134217728) // 24GB + 48GB + 128MB
	if got != want {
		t.Errorf("parseWmiVideoControllerVRAMBytes: got %d, want %d (24+48+0.125 GB)", got, want)
	}
	// Sanity: проверяем, что сумма не нулевая и парсинг не потерял данные.
	if got == 0 {
		t.Error("parseWmiVideoControllerVRAMBytes returned 0 — парсер потерял данные")
	}
}

// TestParseWmiVideoControllerVRAMBytes_LeadingTrailingWhitespace — строки с
// лишними пробелами вокруг значений (бывает в wmic-выводе).
func TestParseWmiVideoControllerVRAMBytes_LeadingTrailingWhitespace(t *testing.T) {
	csvOutput := `Node,AdapterRAM,Name
DESKTOP-ABC,  8589934592  ,NVIDIA GeForce RTX 3070
`

	got := parseWmiVideoControllerVRAMBytes(csvOutput)
	want := uint64(8589934592)
	if got != want {
		t.Errorf("parseWmiVideoControllerVRAMBytes: got %d, want %d (whitespace stripped)", got, want)
	}
}

// TestParseWmiVideoControllerVRAMBytes_RealWorldExample — пример из реального
// wmic-вывода на Windows Server 2022 (с трассировкой LF + возможные пустые строки).
func TestParseWmiVideoControllerVRAMBytes_RealWorldExample(t *testing.T) {
	// Типичный вывод wmic с пустой строкой в конце.
	csvOutput := strings.Join([]string{
		"Node,AdapterRAM,Name",
		"WIN-SRV-2022,8589934592,NVIDIA Quadro RTX 4000",
		"", // wmic часто добавляет пустую строку после данных
	}, "\n")

	got := parseWmiVideoControllerVRAMBytes(csvOutput)
	want := uint64(8589934592)
	if got != want {
		t.Errorf("parseWmiVideoControllerVRAMBytes: got %d, want %d (8 GB)", got, want)
	}
}