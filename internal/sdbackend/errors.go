package sdbackend

import (
	"errors"
	"fmt"

	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Ошибки воркера
// ============================================================
//
// Отдельные sentinel-ошибки, а не строки: HTTP-слой должен отдавать РАЗНЫЕ
// статусы для «модель не загружена» (409), «очередь переполнена» (429) и
// «движок не поднялся» (503) — иначе клиент (балансер, WebUI) не отличит
// «повтори позже» от «сломан конфиг».

var (
	// ErrNotLoaded — модель не загружена (процесс sd-server не запущен).
	ErrNotLoaded = errors.New("image model is not loaded")

	// ErrLoading — загрузка уже идёт (single-flight: запрос должен ждать,
	// а не спавнить второй sd-server).
	ErrLoading = errors.New("image model is loading")

	// ErrModelNotFound — профиль не найден в реестре.
	ErrModelNotFound = errors.New("image model not found")

	// ErrQueueFull — наша очередь генераций заполнена (HTTP 429).
	ErrQueueFull = errors.New("generation queue is full")

	// ErrTimeout — генерация не завершилась за отведённый таймаут.
	ErrTimeout = errors.New("generation timed out")

	// ErrJobNotFound — джоба неизвестна воркеру (или уже вычищена по TTL).
	ErrJobNotFound = errors.New("job not found")

	// ErrCancelGenerating — отмена для статуса generating невозможна:
	// sd-server не умеет прерывать генерацию в полёте
	// (features_by_mode.img_gen.cancel_generating == false, HTTP 409).
	// Мы НЕ обещаем mid-flight cancel и пробрасываем это как есть.
	ErrCancelGenerating = errors.New("job is currently generating and cannot be interrupted yet")

	// ErrCancelNotAllowed — джоба уже в терминальном статусе.
	ErrCancelNotAllowed = errors.New("job is already finished and cannot be cancelled")
)

// FlagIncompatibleError — бинарь sd-server не принял наш argv.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ТИП: релизы stable-diffusion.cpp выходят по нескольку раз в
// день, и имена флагов менялись (--host/--port → --listen-ip/--listen-port
// после master-600, см. types.PinnedSDServerRevision). Оператор, подсунувший
// свежий бинарь, обязан увидеть в ошибке ЯВНОЕ указание на несовместимость
// версии, а не «process exited with code 1».
type FlagIncompatibleError struct {
	Bin      string
	ExitCode int
	Output   string // хвост stdout/stderr процесса
	Revision string // types.PinnedSDServerRevision, под которую написан контракт
}

func (e *FlagIncompatibleError) Error() string {
	return fmt.Sprintf(
		"sd-server rejected the startup command line (exit code %d): проверьте версию sd-server — "+
			"контракт написан под %s; релизы sd.cpp выходят ежедневно и имена флагов менялись "+
			"(--host/--port → --listen-ip/--listen-port после master-600). "+
			"Бинарь: %s. Вывод процесса: %s",
		e.ExitCode, e.Revision, e.Bin, e.Output)
}

// StartupError — процесс sd-server упал/не поднялся до readiness.
type StartupError struct {
	Bin      string
	ExitCode int
	Output   string
	Reason   string
}

func (e *StartupError) Error() string {
	return fmt.Sprintf("sd-server failed to become ready (%s): exit code %d, bin=%s, output: %s",
		e.Reason, e.ExitCode, e.Bin, e.Output)
}

// UpstreamError — ошибка от самого sd-server, сохранённая вместе со статусом.
//
// Нужна, чтобы HTTP-слой прозрачно пробросил 429/409/410 от движка и при этом
// не потерял тело ответа (там `{"error":"..."}`).
type UpstreamError struct {
	StatusCode int    `json:"statusCode"`
	Code       string `json:"code,omitempty"`
	Message    string `json:"message"`
}

func (e *UpstreamError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("sd-server HTTP %d (%s): %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("sd-server HTTP %d: %s", e.StatusCode, e.Message)
}

// PinnedRevision — ревизия движка, под которую написан контракт воркера.
func PinnedRevision() string { return types.PinnedSDServerRevision }
