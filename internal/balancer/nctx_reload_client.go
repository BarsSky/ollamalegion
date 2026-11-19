// nctx_reload_client.go — R634 (2026-11-19): HTTP-клиент перезагрузки: интерфейс, реализация по умолчанию, чтение тела из байтов.
//
// ЗАЧЕМ ВЫНЕСЕНО. `nctx_reload.go` держал 1865 строк: типы и конфигурацию,
// состояние координатора, решения о перезагрузке, HTTP-клиент, длинный DoReload,
// метрики и опрос прогресса. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"context"
	"errors"
	"io"
	"net/http"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
)

type NCtxReloadHTTPClient interface {
	PostReload(ctx context.Context, endpoint string, payload []byte) (*http.Response, error)
}

type DefaultNCtxReloadHTTPClient struct {
	HTTPClient *http.Client

	// APIToken — токен для аутентификации на cppworker.
	APIToken string

	// HeaderName — имя HTTP-заголовка для токена. Должно совпадать с
	// cfg.Auth.HeaderName балансера (по умолчанию "X-API-Token").
	// Round 25 (2026-08-05): раньше код хардкодил "Authorization: Bearer",
	// что НЕ совпадает с cppworker's auth middleware → reload получал 401
	// и auto-reload n_ctx для Cline был полностью сломан.
	HeaderName string
}

func (c *DefaultNCtxReloadHTTPClient) PostReload(ctx context.Context, endpoint string, payload []byte) (*http.Response, error) {
	client := c.HTTPClient
	if client == nil {
		// R83/v62 (2026-10-02): БЕЗ hardcoded 300 с. Timeout=0 в Go означает «нет
		// таймаута»: перезагрузку большой модели (A10, qwen3.8 15.7 ГБ) ждём по
		// терминальному состоянию, а не по часам. Кап — только явный opt-in
		// (LB_ALLOW_NCTX_RELOAD_TIMEOUT), и он уже применяется в
		// newNCtxReloadHTTPClient; сюда попадаем лишь если клиент не задан.
		client = &http.Client{Timeout: 0}
		if logger.Get() != nil {
			logger.Get().Warnw(i18n.T("balancer.nctx_reload.no_http_client_no_timeout_warn"),
				"endpoint", endpoint, "source", "PostReload fallback")
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Body = io.NopCloser(bytesReader(payload))
	req.ContentLength = int64(len(payload))
	if c.APIToken != "" {
		headerName := c.HeaderName
		if headerName == "" {
			headerName = "X-API-Token" // default для совместимости
		}
		req.Header.Set(headerName, c.APIToken)
	}
	return client.Do(req)
}

type bytesReaderImpl struct {
	buf []byte
	pos int
}

func bytesReader(b []byte) io.Reader { return &bytesReaderImpl{buf: b} }

func (r *bytesReaderImpl) Read(p []byte) (int, error) {
	if r.pos >= len(r.buf) {
		return 0, io.EOF
	}
	n := copy(p, r.buf[r.pos:])
	r.pos += n
	return n, nil
}

var ErrReloadInProgress = errors.New("n_ctx reload already in progress on this backend")
