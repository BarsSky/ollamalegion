// autoload_wait_progress_r83_test.go — R83: ожидание загрузки продлевается, пока
// cppworker сообщает прогресс.
//
// ИНЦИДЕНТ (2026-09-25). Жёсткие 180 с (LB_AUTO_LOAD_WAIT_SEC) обрывали ожидание
// 16-гигабайтной модели: клиент получал «model load started, waiting for cppworker
// to finish», загрузка продолжалась, повторный запрос/автотюн запускал ВТОРУЮ
// загрузку — и процесс падал по SIGABRT с повреждением кучи glibc под
// переподпиской VRAM (free=0 MB при gpu_layers=29).
//
// Теперь timeout — это датчик БЕЗДЕЙСТВИЯ: пока elapsedMs растёт, дедлайн
// продлевается (общий предел — autoLoadHardCeiling).
package balancer

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// TestWaitForModelLoad_ExtendsOnProgress_R83 — бюджет бездействия истёк бы трижды,
// но загрузка прогрессирует, поэтому ожидание обязано дождаться state=loaded.
func TestWaitForModelLoad_ExtendsOnProgress_R83(t *testing.T) {
	var polls atomic.Int64

	lr, _ := newAutoloadWaitHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/load/progress":
			n := polls.Add(1)
			if n <= 6 {
				// Прогресс растёт: cppworker всё ещё грузит (elapsedMs увеличивается).
				_, _ = fmt.Fprintf(w, `{"state":"loading","elapsedMs":%d}`, n*1000)
				return
			}
			_, _ = w.Write([]byte(`{"state":"loaded"}`))
		case "/api/models":
			_, _ = w.Write([]byte(`{"count":1,"models":[{"name":"m","state":"loaded"}]}`))
		default:
			http.NotFound(w, r)
		}
	}, 100*time.Millisecond)

	start := time.Now()
	err := lr.waitForModelLoad(context.Background(), "test-bk", "m", 100*time.Millisecond)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("ожидание прервалось, хотя загрузка прогрессировала: %v (через %s)", err, elapsed)
	}
	if polls.Load() < 3 {
		t.Fatalf("опросов прогресса было %d — тест не воспроизводит продление", polls.Load())
	}
	if elapsed < 300*time.Millisecond {
		t.Fatalf("тест завершился за %s — продления дедлайна не было", elapsed)
	}
}
