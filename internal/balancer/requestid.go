package balancer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"go.uber.org/zap"
	"ollama-loadbalancer/pkg/logger"
)

// requestIDKey — ключ в context.Context, под которым хранится correlation-id
// каждого HTTP-запроса. Используется в debug-логах balancer и cppworker,
// чтобы можно было проследить всю цепочку Cline → balancer → cppworker.
type requestIDKey struct{}

// requestIDHeader — HTTP-заголовок, через который клиент (Cline, OpenWebUI,
// curl) может передать свой correlation-id. Если заголовка нет — генерируется
// новый. Если есть — используется клиентский.
const requestIDHeader = "X-Request-ID"

// newRequestID генерирует короткий (8 hex = 4 байта) correlation-id.
// 4 байта = 4.3 миллиарда уникальных значений — для трейсинга
// одиночных запросов более чем достаточно, и в логах такие id
// компактнее, чем полный UUID.
func newRequestID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// fallback — крайне маловероятно, но пусть будет детерминированно
		return "req00000"
	}
	return "req" + hex.EncodeToString(b[:])
}

// getOrGenerateRequestID возвращает request_id из incoming HTTP-заголовка
// X-Request-ID, либо генерирует новый. Затем кладёт его в контекст.
func getOrGenerateRequestID(r *http.Request) (string, context.Context) {
	rid := r.Header.Get(requestIDHeader)
	if rid == "" {
		rid = newRequestID()
	}
	ctx := context.WithValue(r.Context(), requestIDKey{}, rid)
	return rid, ctx
}

// RequestIDFromContext возвращает request_id из context.Context.
// Если id не заложен — возвращает пустую строку.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey{}).(string); ok {
		return v
	}
	return ""
}

// ridField возвращает zap.Field с request_id из ctx, для добавления
// во все log-записи по этому запросу. Если id нет — поле не добавляется.
func ridField(ctx context.Context) zap.Field {
	rid := RequestIDFromContext(ctx)
	if rid == "" {
		return zap.Skip()
	}
	return zap.String("request_id", rid)
}

// ridLog возвращает sugared-логгер с предзаполненным полем request_id
// для всех последующих .Debugw/.Infow/.Warnw/.Errorw вызовов.
// Удобно для добавления одной строкой в начале функции.
func ridLog(ctx context.Context) *zap.SugaredLogger {
	return logger.Get().With(ridField(ctx))
}