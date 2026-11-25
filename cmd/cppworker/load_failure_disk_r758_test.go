// load_failure_disk_r758_test.go — R758 (2026-11-25): нехватка МЕСТА НА ДИСКЕ —
// отдельная причина провала загрузки.
//
// ЖИВОЙ РАЗБОР. На первом стенде загрузка не стартовала из-за 2.6 ГБ свободного
// места (порог resources.disk.minFreeMB = 10 ГБ). Единственное, что видел
// оператор, — строка в колокольчике WebUI; а причина в реестре провалов
// классифицировалась как insufficient_resources («не хватило ПАМЯТИ») или
// unknown, хотя лечится освобождением места, а не уменьшением окна.
//
// ЧТО ФИКСИРУЕТСЯ:
//  1. системные тексты ENOSPC дают КОД insufficient_disk (а не память/unknown);
//  2. «insufficient disk space» не перехватывается более ранней веткой
//     insufficient_resources (порядок проверок — часть контракта);
//  3. код валиден (isLoadFailureReason) и имеет severity=error: оператор
//     действует (удаляет GGUF), повтор загрузки не поможет.
package main

import (
	"errors"
	"testing"
)

// TestR758_ENOSPCTextsClassifiedAsDisk — системные тексты нехватки места.
func TestR758_ENOSPCTextsClassifiedAsDisk(t *testing.T) {
	cases := []string{
		"write /app/models/x.gguf: no space left on device",
		"Error: There is not enough space on the disk",
		"open /app/models/y.gguf.download: ENOSPC",
		"download failed: disk full",
		"insufficient disk space for model",
		"недостаточно места на диске",
		"мало места на диске для модели",
	}
	for _, text := range cases {
		got := classifyLoadFailureReason(errors.New(text), text)
		if got != LoadFailureInsufficientDisk {
			t.Errorf("%q → %q, want %q: иначе оператору скажут «не хватило памяти» "+
				"и он пойдёт уменьшать n_ctx вместо освобождения места",
				text, got, LoadFailureInsufficientDisk)
		}
	}
}

// TestR758_DiskBeatsMemoryBranch — «insufficient disk space» не должно попасть в
// ветку insufficient_resources (там маркер — просто "insufficient").
func TestR758_DiskBeatsMemoryBranch(t *testing.T) {
	text := "insufficient disk space: 2635 MB free"
	if got := classifyLoadFailureReason(errors.New(text), text); got == LoadFailureInsufficientResources {
		t.Fatalf("нехватка диска классифицирована как нехватка ПАМЯТИ (%q) — это разные "+
			"ресурсы и разные действия оператора", got)
	}
}

// TestR758_DiskReasonIsValidAndError — код проходит валидацию и требует внимания.
func TestR758_DiskReasonIsValidAndError(t *testing.T) {
	if !isLoadFailureReason(LoadFailureInsufficientDisk) {
		t.Fatalf("код %q не считается валидным — реестр переклассифицирует его по тексту "+
			"и оператор снова увидит «unknown»", LoadFailureInsufficientDisk)
	}
	if sev := loadFailureSeverity(LoadFailureInsufficientDisk); sev != "error" {
		t.Errorf("severity = %q, want \"error\": место само не появится, от повтора не "+
			"станет лучше", sev)
	}
}

// TestR758_MemoryTextStillMemory — регресс-гард: OOM по-прежнему про память.
func TestR758_MemoryTextStillMemory(t *testing.T) {
	for _, text := range []string{
		"out of memory",
		"failed to allocate 4096 MB",
		"insufficient memory for model",
	} {
		if got := classifyLoadFailureReason(errors.New(text), text); got != LoadFailureInsufficientResources {
			t.Errorf("%q → %q, want %q", text, got, LoadFailureInsufficientResources)
		}
	}
}
