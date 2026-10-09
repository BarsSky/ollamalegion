// model_share_disk_r91_test.go — R91 (2026-10-09): перенос модели не начинается,
// если у приёмника нет места.
//
// ЧТО БЫЛО: /api/v1/models/share не смотрел на свободное место приёмника вовсе.
// Приёмник принимает поток прямо в каталог моделей, поэтому нехватка места
// выяснялась только ПОСЛЕ передачи всех гигабайтов (или, хуже, приводила к
// забитому под ноль диску, на котором уже лежат другие модели). Агент бэкенда
// сообщает свободное место (SystemMetrics.DiskFree, МБ), так что причину можно
// назвать сразу, с числами и без сети.
package api

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// setTargetDiskFree — «агент приёмника прислал свободное место».
func setTargetDiskFree(t *testing.T, srv *Server, backendID string, freeMB uint64) {
	t.Helper()
	srv.proxy.UpdateMetrics(backendID, &types.BackendMetrics{
		ID: backendID,
		System: types.SystemMetrics{
			DiskTotal: 500_000,
			DiskUsed:  500_000 - freeMB,
			DiskFree:  freeMB,
		},
	})
}

// TestModelShareR91_RefusesWhenTargetDiskTooSmall — мало места: цель падает с
// объяснением ДО потока, источник даже не открывается.
func TestModelShareR91_RefusesWhenTargetDiskTooSmall(t *testing.T) {
	payload := bytes.Repeat([]byte("G"), 4<<20) // 4 MiB
	src := newShareMockWorker(t, payload, []string{"m.gguf"})
	dst := newShareMockWorker(t, nil, nil)
	server, ts := newShareTestServer(t, src, dst)
	setTargetDiskFree(t, server, "dst", 1) // 1 МБ свободно (и резерв 512 МБ)

	_, job := startShare(t, ts, `{"source":"src","model":"m.gguf","targets":["dst"]}`)
	done := waitShare(t, ts, job["id"].(string))

	target := firstTarget(t, done)
	if target["state"] != shareTargetFailed {
		t.Fatalf("состояние цели = %v, want failed (%v)", target["state"], target)
	}
	errText, _ := target["error"].(string)
	if !strings.Contains(errText, "места") {
		t.Errorf("в ошибке нет причины «мало места»: %q", errText)
	}
	// Числа обязаны быть в сообщении: оператор должен понимать, сколько нужно.
	if !strings.Contains(errText, "1 МБ") && !strings.Contains(errText, "свободно") {
		t.Errorf("в ошибке нет свободного места приёмника: %q", errText)
	}

	if _, imports, _ := dst.snapshot(); imports != 0 {
		t.Errorf("импортов на приёмнике = %d, want 0 (нехватка места известна заранее)", imports)
	}
	// Источник должен получить ровно один запрос — HEAD за размером (без него
	// неизвестно, сколько места нужно). Потока (GET файла) быть не должно: при
	// нехватке места проверка отсекает цель ДО открытия потока.
	if _, _, exports := src.snapshot(); exports != 1 {
		t.Errorf("запросов к источнику = %d, want 1 (HEAD за размером, без потока)", exports)
	}
}

// TestModelShareR91_AllowsWhenEnoughSpace — места достаточно: перенос идёт как обычно.
func TestModelShareR91_AllowsWhenEnoughSpace(t *testing.T) {
	payload := bytes.Repeat([]byte("H"), 3<<20) // 3 MiB
	src := newShareMockWorker(t, payload, []string{"m.gguf"})
	dst := newShareMockWorker(t, nil, nil)
	server, ts := newShareTestServer(t, src, dst)
	setTargetDiskFree(t, server, "dst", 100_000) // 100 ГБ

	_, job := startShare(t, ts, `{"source":"src","model":"m.gguf","targets":["dst"]}`)
	done := waitShare(t, ts, job["id"].(string))
	if done["state"] != shareStateDone {
		t.Fatalf("состояние = %v, want done (%v)", done["state"], done)
	}
	received, imports, _ := dst.snapshot()
	if imports != 1 || !bytes.Equal(received, payload) {
		t.Errorf("перенос не состоялся: импортов %d, байт %d (want 1, %d)", imports, len(received), len(payload))
	}
}

// TestModelShareR91_UnknownDiskDoesNotBlock — телеметрии о диске нет (нет
// агента): проверка пропускается, перенос не блокируется.
func TestModelShareR91_UnknownDiskDoesNotBlock(t *testing.T) {
	payload := bytes.Repeat([]byte("I"), 2<<20)
	src := newShareMockWorker(t, payload, []string{"m.gguf"})
	dst := newShareMockWorker(t, nil, nil)
	_, ts := newShareTestServer(t, src, dst) // метрики не выставляем вовсе

	_, job := startShare(t, ts, `{"source":"src","model":"m.gguf","targets":["dst"]}`)
	done := waitShare(t, ts, job["id"].(string))
	if done["state"] != shareStateDone {
		t.Fatalf("без телеметрии перенос обязан пройти: %v (%v)", done["state"], done)
	}
}

// TestModelShareR91_SkippedWhenModelAlreadyThereBeatsDiskCheck — «модель уже
// есть» остаётся skipped: переносить нечего, и нехватка места тут не при чём.
func TestModelShareR91_SkippedWhenModelAlreadyThereBeatsDiskCheck(t *testing.T) {
	src := newShareMockWorker(t, bytes.Repeat([]byte("J"), 1024), []string{"m.gguf"})
	dst := newShareMockWorker(t, nil, []string{"m.gguf"})
	server, ts := newShareTestServer(t, src, dst)
	setTargetDiskFree(t, server, "dst", 1)

	_, job := startShare(t, ts, `{"source":"src","model":"m.gguf","targets":["dst"]}`)
	done := waitShare(t, ts, job["id"].(string))
	target := firstTarget(t, done)
	if target["state"] != shareTargetSkipped {
		t.Fatalf("состояние цели = %v, want skipped (%v)", target["state"], target)
	}
}

// TestShareTargetDiskShortfall_ReserveEnv — резерв свободного места настраивается.
func TestShareTargetDiskShortfall_ReserveEnv(t *testing.T) {
	// 1 ГБ свободно, нужно 900 МБ.
	srv, _ := newShareTestServer(t, newShareMockWorker(t, nil, nil), newShareMockWorker(t, nil, nil))
	srv.proxy.UpdateMetrics("src", &types.BackendMetrics{
		ID:     "src",
		System: types.SystemMetrics{DiskFree: 1024},
	})
	need := int64(900) << 20

	// Дефолтный резерв 512 МБ: 1024 - 512 = 512 < 900 → не хватает.
	if shortfall, freeMB, known := srv.shareTargetDiskShortfall("src", need); !known || shortfall <= 0 {
		t.Errorf("с дефолтным резервом ожидалась нехватка: shortfall=%d freeMB=%d known=%v", shortfall, freeMB, known)
	}
	// Резерв 0: 1024 >= 900 → хватает.
	t.Setenv(EnvShareDiskReserveMB, "0")
	if shortfall, _, known := srv.shareTargetDiskShortfall("src", need); !known || shortfall != 0 {
		t.Errorf("с резервом 0 места достаточно: shortfall=%d known=%v", shortfall, known)
	}
	// Мусор в переменной не должен ломать проверку (падаем на дефолт).
	t.Setenv(EnvShareDiskReserveMB, "abc")
	if shortfall, _, known := srv.shareTargetDiskShortfall("src", need); !known || shortfall <= 0 {
		t.Errorf("нечисловой резерв должен приводить к дефолту: shortfall=%d known=%v", shortfall, known)
	}
}

// TestShareHumanBytes — сообщение с размером читаемо на обоих масштабах.
func TestShareHumanBytes(t *testing.T) {
	if got := shareHumanBytes(2 << 30); got != "2.0 ГБ" {
		t.Errorf("2 ГиБ → %q", got)
	}
	if got := shareHumanBytes(4 << 20); got != "4 МБ" {
		t.Errorf("4 МиБ → %q", got)
	}
	if got := shareHumanBytes(0); got != "0 МБ" {
		t.Errorf("0 → %q", got)
	}
	_ = http.StatusOK
}
