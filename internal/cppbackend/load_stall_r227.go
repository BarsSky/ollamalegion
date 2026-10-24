package cppbackend

// R227 (2026-10-22): диагностика зависшей загрузки модели.
//
// ЖИВОЙ ДЕФЕКТ. После «Применить и перезагрузить» с невалидными/недоступными
// параметрами воркер выгружал модель и дальше не грузил её: в логе не было ни
// «reload complete», ни «load failed», ни «rollback failed», а любая следующая
// загрузка (включая прямой POST /api/models/load) висела до перезапуска
// контейнера. `loadSingleFlight` при этом удерживался первой загрузкой, поэтому
// вешались все последующие.
//
// ЧТО ЗДЕСЬ. Сторож на время загрузки: если она не завершилась за
// CPPWORKER_LOAD_STALL_SEC (по умолчанию 300 с, минимум 30), в лог одним ERROR
// уходит стек всех горутин процесса. Это не «лечение» клинча, а инструмент:
// без стека причина неизвестна, а с ним видно, на каком вызове загрузка стоит
// (memfit/CUDA/RPC/GGUF). Сторож не вмешивается в работу и не прерывает load.

import (
	"errors"
	"os"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
)

// loadStallThreshold — сколько ждать до дампа стека.
func loadStallThreshold() time.Duration {
	const def = 300 * time.Second
	raw := strings.TrimSpace(os.Getenv("CPPWORKER_LOAD_STALL_SEC"))
	if raw == "" {
		return def
	}
	sec, err := strconv.Atoi(raw)
	if err != nil || sec <= 0 {
		return def
	}
	if sec < 30 {
		sec = 30
	}
	return time.Duration(sec) * time.Second
}

// goroutineStackDump — стек всех горутин процесса (как при SIGQUIT, но без паники).
func goroutineStackDump() string {
	var sb strings.Builder
	if err := pprof.Lookup("goroutine").WriteTo(&sb, 2); err != nil {
		return "pprof goroutine dump failed: " + err.Error()
	}
	return sb.String()
}

// loadStallWatchdog запускает сторож. done закрывается вызывающим по завершении
// загрузки (defer close(done)).
//
// dump — точка подмены для тестов (nil → goroutineStackDump).
func loadStallWatchdog(modelName string, stall time.Duration, done <-chan struct{}, dump func() string, onStall func()) {
	if stall <= 0 || done == nil {
		return
	}
	if dump == nil {
		dump = goroutineStackDump
	}
	go func() {
		select {
		case <-done:
			return
		case <-time.After(stall):
		}
		stack := dump()
		logger.Get().Errorw(i18n.T("cppbackend.load_stall.watchdog_fired"),
			"model", modelName, "stall_sec", int(stall.Seconds()), "stacks", stack)
		// R227 (шаг 2): не только сообщаем, но и пытаемся прекратить загрузку.
		// llama.cpp чтит флаг отмены на ближайшем checkpoint; если он висит до
		// первого callback, отмена не сработает — тогда следующий шаг лечится
		// дедлайном на захват loadSingleFlight (см. acquireLoadFlight).
		if onStall != nil {
			onStall()
		}
	}()
}

// loadFlightWait — сколько допустимо ждать освобождения предыдущей загрузки.
// Живой дефект: зависшая загрузка держала loadSingleFlight, и все следующие
// загрузки висели молча. Теперь ожидание ограничено и даёт явную ошибку.
func loadFlightWait() time.Duration {
	const def = 600 * time.Second
	raw := strings.TrimSpace(os.Getenv("CPPWORKER_LOAD_FLIGHT_WAIT_SEC"))
	if raw == "" {
		return def
	}
	sec, err := strconv.Atoi(raw)
	if err != nil || sec <= 0 {
		return def
	}
	if sec < 60 {
		sec = 60
	}
	return time.Duration(sec) * time.Second
}

// acquireLoadFlight — захват единственного слота загрузки с дедлайном.
func (b *Backend) acquireLoadFlight(wait time.Duration) error {
	if b.loadSingleFlight.TryLock() {
		return nil
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		if b.loadSingleFlight.TryLock() {
			return nil
		}
	}
	return errors.New(i18n.T("cppbackend.load_stall.blocked", wait))
}
