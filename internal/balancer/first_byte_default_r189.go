// first_byte_default_r189.go — R189 (2026-10-20): граница на ПЕРВЫЙ БАЙТ по
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
