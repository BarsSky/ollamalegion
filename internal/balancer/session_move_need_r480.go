// session_move_need_r480.go — R480: «нужное окно» = потребность ЗАПРОСА и окно
// сессии в пределах TTL (P1-4, вариант C).
//
// ПРЕДЫДУЩЕЕ ПРАВИЛО (R459, 2026-11-06). Переезд сессии разрешался, только если
// копия-кандидат покрывает окно, с которым сессия уже работает. Короткий запрос
// поэтому никогда не уезжал с занятой «широкой» копии на свободную «узкую» —
// балансировка стояла, и это была жалоба «потеряна работа распределения».
//
// ПОЧЕМУ НЕЛЬЗЯ ПРОСТО ВЗЯТЬ ПОТРЕБНОСТЬ ЗАПРОСА. Сессия накапливает контекст:
// запрос с коротким новым сообщением всё равно тащит всю историю. Если увести
// её на узкую копию по «короткому» запросу, следующий ход той же сессии
// упрётся в окно и получит 413 либо будет переезжать обратно.
//
// ВАРИАНТ C (выбран 2026-11-09):
//
//	need = max(потребность запроса + резерв,
//	           окно текущей копии сессии, ЕСЛИ сессия активна в пределах TTL)
//
// То есть живая (активная) сессия держит широкую копию — пинг-понга нет. Как
// только сессия простояла дольше TTL, её окно перестаёт считаться требованием,
// и переезд решается потребностью конкретного запроса. Так закрываются обе
// беды: «короткий запрос ждёт за широкой копией» и «мёртвая сессия держит
// широкую копию вечно».
//
// Ручки (все — на балансере):
//
//	LB_R459_REQUEST_NEED       on|off (default on)  — учитывать потребность запроса
//	LB_R459_NEED_RESERVE       токены (default 4096) — резерв к промпту
//	LB_R459_SESSION_TTL_SEC    секунды (default 900) — TTL активности сессии;
//	                           0 = окно сессии учитывать всегда (поведение R459)
package balancer

import (
	"os"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

const (
	// sessionNeedReserveDefaultR480 — резерв к промпту по умолчанию. Почему
	// 4096: это типичный запас на ответ ассистента и шаблон чата у моделей
	// 7–14B; меньше — риск упереться в окно на генерации, больше — снова
	// «широкое требование на короткий запрос».
	sessionNeedReserveDefaultR480 = 4096
	// sessionNeedTTLDefaultR480 — TTL активности сессии по умолчанию (15 мин).
	// Согласовано с абсолютным TTL сессии в SessionManager (тоже 15 мин) —
	// дольше сессии всё равно не живут.
	sessionNeedTTLDefaultR480 = 900
)

// r459RequestNeedEnabledR480 — учитывать ли потребность запроса.
func r459RequestNeedEnabledR480() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("LB_R459_REQUEST_NEED")))
	switch v {
	case "off", "0", "false", "no":
		return false
	default:
		return true
	}
}

// r459NeedReserveR480 — резерв к промпту, токены. Отрицательное значение
// трактуется как 0 (резерв выключен), а не как «дефолт»: оператор, поставивший
// 0, хочет need = ровно промпт.
func r459NeedReserveR480() int {
	raw := strings.TrimSpace(os.Getenv("LB_R459_NEED_RESERVE"))
	if raw == "" {
		return sessionNeedReserveDefaultR480
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return sessionNeedReserveDefaultR480
	}
	return n
}

// r459SessionTTLR480 — TTL активности сессии. 0 (и мусор) = TTL выключен —
// окно сессии учитывается всегда, как в R459.
func r459SessionTTLR480() time.Duration {
	raw := strings.TrimSpace(os.Getenv("LB_R459_SESSION_TTL_SEC"))
	if raw == "" {
		return time.Duration(sessionNeedTTLDefaultR480) * time.Second
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return time.Duration(sessionNeedTTLDefaultR480) * time.Second
	}
	return time.Duration(n) * time.Second
}

// sessionWindowCountsR480 — считается ли окно текущей копии требованием сессии.
//
// true — сессия активна (последний запрос не старше TTL) либо TTL выключен;
// false — сессию считаем остывшей, и её окно больше не диктует переезд.
func (p *Proxy) sessionWindowCountsR480(sessionID string, now time.Time) bool {
	if p == nil {
		return true
	}
	ttl := r459SessionTTLR480()
	if ttl <= 0 {
		return true
	}
	if sessionID == "" {
		// Нет ключа сессии — TTL применять не к чему: поведение R459 (окно
		// текущей копии — нижняя граница). Иначе пути без stickiness потеряли
		// бы защиту окна вовсе.
		return true
	}
	if p.sessionMgr == nil {
		return true
	}
	s, ok := p.sessionMgr.GetSnapshot(sessionID)
	if !ok || s.LastRequestAt.IsZero() {
		return false
	}
	return now.Sub(s.LastRequestAt) <= ttl
}

// promptTokensForMoveR480 — потребность запроса в токенах (промпт) + резерв.
//
// Оценка берётся тем же путём, что и в preflight (ExtractRequestMeta +
// калибровка R302), но БЕЗ side effect'ов: калибровку применяем напрямую, не
// записывая наблюдение (иначе каждый переезд «обучал» бы калибровку лишний раз).
func (p *Proxy) promptTokensForMoveR480(body []byte, path string) int {
	if len(body) == 0 {
		return 0
	}
	// Единственный разрешённый вход к оценке промпта — калиброванный
	// (R302: стерегущий тест TestR302_NoPathBypassesCalibration).
	tokens := 0
	if p != nil {
		tokens = p.calibratedEstimateOnly(body, path)
	}
	if tokens <= 0 {
		return 0
	}
	return tokens + r459NeedReserveR480()
}

// sessionMoveNeedR480 — итоговое «нужное окно» и его источник (для лога).
//
// Источники: "request" (потребность запроса), "session" (окно активной сессии),
// "client" (окно, названное клиентом), "unknown" (ничего не известно).
func (p *Proxy) sessionMoveNeedR480(
	fromBackend, model, sessionID string, requestedNCtx, requestTokens int,
) (need int, source string) {
	need = requestedNCtx
	if need > 0 {
		source = "client"
	}
	if r459RequestNeedEnabledR480() && requestTokens > need {
		need = requestTokens
		source = "request"
	}
	// Окно текущей копии — только пока сессия активна (или TTL выключен).
	if p.sessionWindowCountsR480(sessionID, time.Now()) {
		if w := p.loadedPerSeqNCtx(fromBackend, model); w > need {
			need = w
			source = "session"
		}
	}
	if need <= 0 {
		source = "unknown"
	}
	return need, source
}

// logMoveNeedR480 — одна строка с числами: почему переезд решён именно так.
// Без неё «не переехало» снова выясняется чтением исходников.
func (p *Proxy) logMoveNeedR480(fromBackend, model, sessionID string, requestedNCtx, requestTokens, need int, source string) {
	if logger.Get() == nil {
		return
	}
	ttlLeft := int64(-1)
	if ttl := r459SessionTTLR480(); ttl > 0 && p != nil && p.sessionMgr != nil && sessionID != "" {
		if s, ok := p.sessionMgr.GetSnapshot(sessionID); ok && !s.LastRequestAt.IsZero() {
			ttlLeft = int64((ttl - time.Since(s.LastRequestAt)).Seconds())
		}
	}
	logger.Get().Infow("R480: нужное окно для переезда сессии",
		"session", sessionID, "from", fromBackend, "model", model,
		"requested_n_ctx", requestedNCtx, "request_need", requestTokens,
		"session_window", p.loadedPerSeqNCtx(fromBackend, model),
		"session_ttl_left_sec", ttlLeft, "need", need, "need_source", source)
}
