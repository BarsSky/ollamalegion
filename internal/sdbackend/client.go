package sdbackend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ============================================================
// Клиент нативного API sd-server (/sdcpp/v1/*)
// ============================================================
//
// ПОЧЕМУ ИМЕННО /sdcpp/v1/*, А НЕ OpenAI/A1111-ветки движка (см.
// docs/research-sdcpp-lowvram-integration.md §4.4):
//   - только у нативного API есть очередь со статусами, TTL и cancel;
//   - только он отдаёт limits (границы, которые движок НЕ валидирует);
//   - только он принимает seed как поле верхнего уровня (в OpenAI-ветке seed
//     игнорируется вовсе и берётся default_gen_params.seed = 42).
// OpenAI/A1111-совместимость реализует наш воркер (§12.4 плана).
//
// СХЕМА ЗАПРОСА (проверено по examples/server/api.md, ревизия master-929):
//   POST /sdcpp/v1/img_gen  202 → {id, kind, status, created, poll_url}
//   GET  /sdcpp/v1/jobs/{id} → {status, result:{output_format, images:[{index,b64_json}]}, error}
//   POST /sdcpp/v1/jobs/{id}/cancel → 200 | 404 | 409 | 410
//   GET  /sdcpp/v1/capabilities    → {model, limits, samplers, schedulers, ...}

// sdcppRequestTimeout — таймаут одиночного HTTP-вызова к движку.
//
// ВАЖНО: это НЕ таймаут генерации. img_gen отвечает 202 сразу (джоба
// уезжает в очередь движка), а GET /jobs/{id} — мгновенный. Таймаут самой
// генерации живёт в Generations-слое (profile.TimeoutSec / config).
// 30 с хватает даже на медленный диск при первом обращении к capabilities
// (там сканируются LoRA/upscaler-каталоги).
const sdcppRequestTimeout = 30 * time.Second

// Capabilities — ответ GET /sdcpp/v1/capabilities (только нужные поля).
type Capabilities struct {
	Model        CapModel            `json:"model"`
	CurrentMode  string              `json:"current_mode"`
	SupportedModes []string          `json:"supported_modes"`
	Defaults     map[string]any      `json:"defaults"`
	DefaultsByMode map[string]any    `json:"defaults_by_mode"`
	OutputFormats  []string          `json:"output_formats"`
	OutputFormatsByMode map[string][]string `json:"output_formats_by_mode"`
	Features     map[string]any      `json:"features"`
	FeaturesByMode map[string]map[string]bool `json:"features_by_mode"`
	Samplers     []string            `json:"samplers"`
	Schedulers   []string            `json:"schedulers"`
	Loras        []CapLora           `json:"loras"`
	Upscalers    []CapUpscaler       `json:"upscalers"`
	Upscale      bool                `json:"upscale"`
	Limits       CapLimits           `json:"limits"`
	// raw — исходный JSON (отдаём клиенту агрегированно, без потери полей,
	// которых мы не знаем: API молод и расширяется каждую неделю).
	raw map[string]any
}

// CapModel — загруженная модель по мнению движка.
type CapModel struct {
	Name string `json:"name"`
	Stem string `json:"stem"`
	Path string `json:"path"`
}

// CapLora — доступная LoRA.
type CapLora struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// CapUpscaler — доступный апскейлер.
type CapUpscaler struct {
	Name         string `json:"name"`
	Model        bool   `json:"model"`
	ImageUpscale bool   `json:"image_upscale"`
}

// CapLimits — границы, которые движок сообщает сам.
//
// Читаем их ОБЯЗАТЕЛЬНО при readiness: это единственный источник правды о
// min/max размерах и размере очереди (в коде движка это константы, а не
// конфиг — при апгрейде они могут измениться).
type CapLimits struct {
	MinWidth         int `json:"min_width"`
	MaxWidth         int `json:"max_width"`
	MinHeight        int `json:"min_height"`
	MaxHeight        int `json:"max_height"`
	MaxBatchCount    int `json:"max_batch_count"`
	MaxQueueSize     int `json:"max_queue_size"`
	MaxUpscaleWidth  int `json:"max_upscale_width"`
	MaxUpscaleHeight int `json:"max_upscale_height"`
}

// ImageResult — одна картинка в результате джобы.
type ImageResult struct {
	Index   int    `json:"index"`
	B64JSON string `json:"b64_json"`
}

// MaskImageParams — параметры сэмплирования (вложенный объект движка).
type MaskImageParams struct {
	Scheduler     string            `json:"scheduler,omitempty"`
	SampleMethod  string            `json:"sample_method,omitempty"`
	SampleSteps   int               `json:"sample_steps,omitempty"`
	Eta           *float64          `json:"eta,omitempty"`
	ShiftedTimestep int             `json:"shifted_timestep,omitempty"`
	FlowShift     *float64          `json:"flow_shift,omitempty"`
	Guidance      *GuidanceParams   `json:"guidance,omitempty"`
}

// GuidanceParams — guidance-блок (txt_cfg и т.п.).
type GuidanceParams struct {
	TxtCfg *float64 `json:"txt_cfg,omitempty"`
	ImgCfg *float64 `json:"img_cfg,omitempty"`
}

// LoraRef — структурированная LoRA в запросе (prompt-теги движок НЕ парсит).
type LoraRef struct {
	Path        string  `json:"path"`
	Multiplier  float64 `json:"multiplier,omitempty"`
	IsHighNoise bool    `json:"is_high_noise,omitempty"`
}

// ImgGenRequest — тело POST /sdcpp/v1/img_gen.
//
// Отправляем ТОЛЬКО те поля, которые у нас есть: nil-указатели и omitempty не
// попадают в JSON, поэтому дефолты движка (eta, flow_shift, img_cfg) остаются
// нетронутыми. Это важно: подстановка нулей вместо «не задано» меняет картинку.
type ImgGenRequest struct {
	Prompt         string       `json:"prompt"`
	NegativePrompt string       `json:"negative_prompt,omitempty"`
	ClipSkip       *int         `json:"clip_skip,omitempty"`
	Width          int          `json:"width,omitempty"`
	Height         int          `json:"height,omitempty"`
	Seed           int64        `json:"seed"`
	BatchCount     int          `json:"batch_count,omitempty"`
	SampleParams   *MaskImageParams `json:"sample_params,omitempty"`
	Lora           []LoraRef    `json:"lora,omitempty"`
	OutputFormat   string       `json:"output_format,omitempty"`
	OutputCompression *int      `json:"output_compression,omitempty"`
}

// JobResult — result завершённой img_gen-джобы.
type JobResult struct {
	OutputFormat string        `json:"output_format"`
	Images       []ImageResult `json:"images"`
	// Для vid_gen (не используем в Phase 3, но поле читаем — иначе
	// json.Unmarshal не пострадает, а отладка проще).
	B64JSON    string `json:"b64_json,omitempty"`
	MimeType   string `json:"mime_type,omitempty"`
	FrameCount int    `json:"frame_count,omitempty"`
}

// JobError — error завершённой джобы.
type JobError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Job — состояние джобы в движке.
type Job struct {
	ID            string     `json:"id"`
	Kind          string     `json:"kind"`
	Status        string     `json:"status"`
	Created       int64      `json:"created"`
	Started       *int64     `json:"started"`
	Completed     *int64     `json:"completed"`
	QueuePosition int        `json:"queue_position"`
	Result        *JobResult `json:"result"`
	Error         *JobError  `json:"error"`
}

// Статусы джобы движка.
const (
	JobStatusQueued     = "queued"
	JobStatusGenerating = "generating"
	JobStatusCompleted  = "completed"
	JobStatusFailed     = "failed"
	JobStatusCancelled  = "cancelled"
)

// SubmitResponse — ответ 202 на img_gen.
type SubmitResponse struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Status   string `json:"status"`
	Created  int64  `json:"created"`
	PollURL  string `json:"poll_url"`
}

// SDServerClient — низкоуровневый клиент sd-server.
type SDServerClient struct {
	baseURL string
	http    *http.Client
}

// NewSDServerClient — клиент на http://<ip>:<port> движка.
func NewSDServerClient(ip string, port int) *SDServerClient {
	return NewSDServerClientURL(fmt.Sprintf("http://%s:%d", ip, port))
}

// NewSDServerClientURL — клиент на произвольный base URL (нужно тестам и
// сценарию «sd-server на другом хосте»).
func NewSDServerClientURL(baseURL string) *SDServerClient {
	return &SDServerClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			Timeout: sdcppRequestTimeout,
			Transport: &http.Transport{
				// Движок живёт на loopback либо в той же сети; keep-alive
				// экономит TCP-хендшейк на каждом poll'е (а polling частый).
				MaxIdleConns:        16,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     90 * time.Second,
				DialContext: (&net.Dialer{
					Timeout: 5 * time.Second,
				}).DialContext,
			},
		},
	}
}

// BaseURL — базовый URL движка (для логов/диагностики).
func (c *SDServerClient) BaseURL() string { return c.baseURL }

// Capabilities — GET /sdcpp/v1/capabilities.
func (c *SDServerClient) Capabilities(ctx context.Context) (*Capabilities, error) {
	body, status, err := c.do(ctx, http.MethodGet, "/sdcpp/v1/capabilities", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, upstreamError(status, body)
	}
	var cap Capabilities
	if err := json.Unmarshal(body, &cap); err != nil {
		return nil, fmt.Errorf("parse capabilities: %w (body: %s)", err, truncate(string(body), 200))
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err == nil {
		cap.raw = raw
	}
	return &cap, nil
}

// Raw — исходный JSON capabilities (для агрегированного ответа клиенту).
func (c *Capabilities) Raw() map[string]any {
	if c == nil {
		return nil
	}
	return c.raw
}

// Ping — проверка доступности (используется readiness-поллером).
func (c *SDServerClient) Ping(ctx context.Context) error {
	_, err := c.Capabilities(ctx)
	return err
}

// SubmitImgGen — POST /sdcpp/v1/img_gen (202 Accepted).
func (c *SDServerClient) SubmitImgGen(ctx context.Context, req *ImgGenRequest) (*SubmitResponse, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal img_gen request: %w", err)
	}
	body, status, err := c.do(ctx, http.MethodPost, "/sdcpp/v1/img_gen", payload)
	if err != nil {
		return nil, err
	}
	if status != http.StatusAccepted && status != http.StatusOK {
		// 429 «job queue is full» — самая частая причина; пробрасываем как
		// UpstreamError, чтобы HTTP-слой отдал клиенту тот же смысл.
		return nil, upstreamError(status, body)
	}
	var sub SubmitResponse
	if err := json.Unmarshal(body, &sub); err != nil {
		return nil, fmt.Errorf("parse img_gen response: %w (body: %s)", err, truncate(string(body), 200))
	}
	if sub.ID == "" {
		return nil, fmt.Errorf("img_gen response has no job id (body: %s)", truncate(string(body), 200))
	}
	return &sub, nil
}

// Job — GET /sdcpp/v1/jobs/{id}.
func (c *SDServerClient) Job(ctx context.Context, id string) (*Job, error) {
	path := "/sdcpp/v1/jobs/" + url.PathEscape(id)
	body, status, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, upstreamError(status, body)
	}
	var job Job
	if err := json.Unmarshal(body, &job); err != nil {
		return nil, fmt.Errorf("parse job %s: %w (body: %s)", id, err, truncate(string(body), 200))
	}
	return &job, nil
}

// CancelJob — POST /sdcpp/v1/jobs/{id}/cancel.
//
// Возвращает UpstreamError со статусом 409, если джоба уже generating —
// вызывающий ОБЯЗАН пробросить это клиенту как есть и не обещать
// mid-flight cancel (движок его не умеет).
func (c *SDServerClient) CancelJob(ctx context.Context, id string) error {
	path := "/sdcpp/v1/jobs/" + url.PathEscape(id) + "/cancel"
	body, status, err := c.do(ctx, http.MethodPost, path, nil)
	if err != nil {
		return err
	}
	if status >= 200 && status < 300 {
		return nil
	}
	return upstreamError(status, body)
}

// WaitJob — поллинг до терминального статуса.
//
// pollEvery — интервал опроса (в тестах 1–5 мс, в бою 500 мс). Первый опрос
// делаем сразу: для быстрых моделей (SD-Turbo 1 шаг) джоба часто завершается
// раньше первого интервала.
func (c *SDServerClient) WaitJob(ctx context.Context, id string, timeout time.Duration, pollEvery time.Duration) (*Job, error) {
	if pollEvery <= 0 {
		pollEvery = 500 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	if timeout <= 0 {
		deadline = time.Now().Add(24 * time.Hour)
	}
	var last *Job
	for {
		job, err := c.Job(ctx, id)
		if err != nil {
			return last, err
		}
		last = job
		switch job.Status {
		case JobStatusCompleted, JobStatusFailed, JobStatusCancelled:
			return job, nil
		}
		if time.Now().After(deadline) {
			return last, ErrTimeout
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}

// do — единая точка HTTP-вызовов: собирает URL, шлёт, читает тело.
func (c *SDServerClient) do(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("sd-server request %s %s failed: %w", method, path, err)
	}
	defer resp.Body.Close()
	// Ограничение на размер ответа: b64 одной картинки 512x512 ~ 0.5–2 MB,
	// base64 раздувает на ~33%; 64 MB с запасом хватает на batch=8 @4096.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read sd-server response: %w", err)
	}
	return data, resp.StatusCode, nil
}

// upstreamError — UpstreamError из тела {"error":"..."} или {"error":{...}}.
//
// ВАЖНО: sd-server отдаёт ошибку СТРОКОЙ (не OpenAI-конвертом), поэтому
// разбираем оба варианта и никогда не теряем исходный текст.
func upstreamError(status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	code := ""
	var flat struct {
		Error json.RawMessage `json:"error"`
		Code  string          `json:"code"`
	}
	if err := json.Unmarshal(body, &flat); err == nil {
		if flat.Code != "" {
			code = flat.Code
		}
		if len(flat.Error) > 0 {
			var s string
			if err := json.Unmarshal(flat.Error, &s); err == nil && s != "" {
				msg = s
			} else {
				var obj struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				}
				if err := json.Unmarshal(flat.Error, &obj); err == nil {
					if obj.Code != "" {
						code = obj.Code
					}
					if obj.Message != "" {
						msg = obj.Message
					}
				}
			}
		}
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &UpstreamError{StatusCode: status, Code: code, Message: msg}
}

// truncate — обрезка длинных тел для логов/ошибок.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// AsUpstreamError — приведение ошибки к *UpstreamError (для HTTP-слоя).
func AsUpstreamError(err error) (*UpstreamError, bool) {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue, true
	}
	return nil, false
}

// intPtr — хелпер для *int (omitempty не работает с нулём, а нам нужно
// отличать «не задано» от «0»: clip_skip -1 у движка означает «не задано»).
func intPtr(v int) *int { return &v }

// floatPtr — хелпер для *float64.
func floatPtr(v float64) *float64 { return &v }

// statusCodeOf — числовой код статуса для логов.
func statusCodeOf(err error) int {
	if ue, ok := AsUpstreamError(err); ok {
		return ue.StatusCode
	}
	return 0
}

// retryAfterSeconds — значение для заголовка Retry-After при 429.
func retryAfterSeconds(err error) int {
	if ue, ok := AsUpstreamError(err); ok && ue.StatusCode == http.StatusTooManyRequests {
		return 5
	}
	return 3
}

// atoiSafe — безопасный Atoi (используется в парсинге query-параметров).
func atoiSafe(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

