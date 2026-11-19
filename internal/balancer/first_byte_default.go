// first_byte_default.go — R189 (2026-10-20): граница на ПЕРВЫЙ БАЙТ по
// умолчанию для стриминговых запросов.
//
// ЖИВОЙ РАЗБОР (Cline через Ollama API, Qwen3.8-27B). Запрос `/api/chat` с
// `tools[]` не отдавал клиенту ни байта 300 секунд:
//
//	"proxyRequestLlamaCpp: upstream Do() failed ... duration_ms=300021, error_type=context_canceled"
//
// Раньше ResponseHeaderTimeout ставился только если профиль модели или
// окружение задали FirstByteTimeout ЯВНО. По умолчанию границы не было, и
// клиент молча ждал до собственного таймаута (а иногда — пока не оборвётся
// сессия). Снаружи это «балансер встал».
//
// Теперь для стрима действует значение по умолчанию: 180 секунд — на стенде
// это заведомо больше префилла 27B-модели с CPU-оффлоадом (наблюдали 2-6 минут
// на ПОЛНЫЙ ход, префилл — десятки секунд), но меньше «бесконечности».
// Отключается явно: LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC=0.
package balancer

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// defaultFirstByteTimeoutSec — граница «нет заголовков/данных» по умолчанию.
const defaultFirstByteTimeoutSec = 180

// defaultFirstByteTimeout — значение по умолчанию (0 = граница отключена).
func defaultFirstByteTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC"))
	if raw == "" {
		return defaultFirstByteTimeoutSec * time.Second
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return defaultFirstByteTimeoutSec * time.Second
	}
	return time.Duration(n) * time.Second
}

// defaultFirstByteDisabled — граница отключена явно (0).
func defaultFirstByteDisabled() bool {
	return defaultFirstByteTimeout() == 0
}

// defaultFirstByteTimeoutForModelR498 — граница на первый байт для конкретной
// модели: явная настройка → оценка по размеру весов → безопасный дефолт.
//
// R498 / аудит T-1 (2026-11-19). ЖИВОЙ ДЕФЕКТ: действующий дефолт был ровно
// 180 с ПЛОСКО, независимо от модели, а холодная загрузка 16–27B на этом же
// стенде занимает 140–485 с (autoload_wait_r325_1.go:15,
// llamacpp_transport_nonstream.go:130). Воркер отдаёт заголовки только ПОСЛЕ
// ленивой загрузки, поэтому таймаут фактически считал загрузку и обрывал её на
// 180-й секунде — «модель не загрузилась», хотя загрузка шла.
//
// ПОЧЕМУ ЭСТИМАТОР, А НЕ «0 = выключено». Полностью выключать границу нельзя:
// именно её отсутствие дало живой разбор R189 (клиент молча ждал 300 с и дольше).
// Эстиматор `EstimateFirstByteTimeoutFromModelSize` уже был написан для этого и
// не использовался НИГДЕ — теперь это единственный источник дефолта: 2 ГБ → 300 с,
// 5 ГБ → 600 с, 12 ГБ → 1200 с, 24 ГБ → 1800 с, неизвестный размер → 900 с.
//
// Явная настройка (LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC, в т.ч. 0 = выключено)
// по-прежнему сильнее оценки: оператор всегда может задать своё.
func (p *Proxy) defaultFirstByteTimeoutForModelR498(model string) time.Duration {
	if raw := strings.TrimSpace(os.Getenv("LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC")); raw != "" {
		return defaultFirstByteTimeout() // явное значение (0 = граница выключена)
	}
	var size int64
	if p != nil && model != "" {
		size = p.getModelSizeBytes(model)
	}
	return EstimateFirstByteTimeoutFromModelSize(size)
}
