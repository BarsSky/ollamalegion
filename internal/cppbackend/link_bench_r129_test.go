//go:build llama_stub

// link_bench_r129_test.go — R129 (пункт 3): замер пропускной способности между
// узлами. Проверяется, что число — ИЗМЕРЕННОЕ (байты/время), а не выдуманное, и
// что отказ не превращается в «0 МБ/с».
package cppbackend

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sink — приёмник замера: читает и отбрасывает тело, отвечает числами.
func sink(t *testing.T, status int) (*httptest.Server, *int64) {
	t.Helper()
	var received int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		received += n
		w.Header().Set("Content-Type", "application/json")
		if status >= 400 {
			w.WriteHeader(status)
		}
		fmt.Fprintf(w, `{"bytes":%d}`, n)
	}))
	t.Cleanup(srv.Close)
	return srv, &received
}

// TestR129_MeasureLinkSpeed_MeasuresRealBytes — приёмник получает ровно заявленный
// объём, а результат содержит измеренную скорость.
func TestR129_MeasureLinkSpeed_MeasuresRealBytes(t *testing.T) {
	srv, received := sink(t, http.StatusOK)

	res := MeasureLinkSpeed(context.Background(), srv.URL, 4, 30*time.Second)

	if res.Error != "" {
		t.Fatalf("замер не удался: %s", res.Error)
	}
	if got := *received; got != 4<<20 {
		t.Errorf("приёмник получил %d байт, want %d (4 МБ)", got, 4<<20)
	}
	if res.MB != 4 {
		t.Errorf("MB = %d, want 4", res.MB)
	}
	if res.MBps <= 0 {
		t.Errorf("MBps = %v, want > 0: на локальном приёмнике скорость не может быть нулевой", res.MBps)
	}
	// Скорость обязана быть согласована с измеренным временем.
	if res.DurationMS > 0 {
		want := float64(4) / (float64(res.DurationMS) / 1000.0)
		if res.MBps > want*1.5 || res.MBps < want*0.5 {
			t.Errorf("MBps = %v не согласована с durationMs = %d (ожидалось ~%.1f)",
				res.MBps, res.DurationMS, want)
		}
	}
}

// TestR129_MeasureLinkSpeed_FailureIsNotZeroSpeed — отказ приёмника НЕ должен
// выглядеть как измеренный ноль: ноль означал бы «сеть не передаёт ничего».
func TestR129_MeasureLinkSpeed_FailureIsNotZeroSpeed(t *testing.T) {
	srv, _ := sink(t, http.StatusInternalServerError)

	res := MeasureLinkSpeed(context.Background(), srv.URL, 2, 10*time.Second)
	if res.Error == "" {
		t.Fatal("отказ приёмника не отражён в Error — UI показал бы «0 МБ/с» как результат")
	}
	if res.MBps != 0 {
		t.Errorf("MBps = %v при отказе, want 0 (и обязательный Error)", res.MBps)
	}
	if !strings.Contains(res.Error, "500") {
		t.Errorf("в причине нет кода ответа: %q", res.Error)
	}
}

// TestR129_MeasureLinkSpeed_UnreachableIsReported — недоступный узел: причина, а
// не тихий ноль.
func TestR129_MeasureLinkSpeed_UnreachableIsReported(t *testing.T) {
	res := MeasureLinkSpeed(context.Background(), "http://127.0.0.1:1/link-bench", 1, 2*time.Second)
	if res.Error == "" {
		t.Fatal("недоступный узел не дал причины")
	}
	if res.MBps != 0 {
		t.Errorf("MBps = %v, want 0", res.MBps)
	}
}

// TestR129_MeasureLinkSpeed_ValidatesInput — пустой объём/адрес не запускают
// бессмысленный замер.
func TestR129_MeasureLinkSpeed_ValidatesInput(t *testing.T) {
	if res := MeasureLinkSpeed(context.Background(), "http://127.0.0.1:1/x", 0, time.Second); res.Error == "" {
		t.Error("нулевой объём замера принят без ошибки")
	}
	if res := MeasureLinkSpeed(context.Background(), "  ", 4, time.Second); res.Error == "" {
		t.Error("пустой адрес принят без ошибки")
	}
}

// TestR129_MeasureLinkSpeed_TimeoutIsReported — медленный приёмник упирается в
// таймаут и это видно, а не висит вечно.
func TestR129_MeasureLinkSpeed_TimeoutIsReported(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer slow.Close()

	res := MeasureLinkSpeed(context.Background(), slow.URL, 1, 300*time.Millisecond)
	if res.Error == "" {
		t.Error("таймаут не отражён в Error")
	}
}
