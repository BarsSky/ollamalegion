// Package types provides type-safe models for the balancer ↔ cppworker API.
//
// Round 36 Phase 2 (2026-08-17): contract enforcement via Go types + strict
// JSON decoding. See docs/architecture/balancer-cppworker-contract.md for the full
// specification.
package types

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
)

// Common protocol errors. These are returned to clients in error responses.
var (
	// ErrBodyEmpty is returned when the request body has no content.
	ErrBodyEmpty = errors.New("empty request body")
	// ErrBodyTooLarge is returned when the request body exceeds maxBytes.
	// Caller should map this to HTTP 413.
	ErrBodyTooLarge = errors.New("request body too large")
)

// StrictDecoder enforces DisallowUnknownFields at the JSON layer to catch
// client-side bugs (typos, deprecated fields, version mismatches) at the
// boundary instead of silently ignoring them.
//
// Construct one with NewStrictDecoder, then call Decode.
type StrictDecoder struct {
	dec       *json.Decoder
	maxBytes  int64
	bodyBytes int64
}

// NewStrictDecoder wraps r with a JSON decoder that rejects unknown fields
// and enforces a max body size of maxBytes (use 0 for unlimited). The body
// is read fully into memory so the size can be enforced accurately.
func NewStrictDecoder(r io.Reader, maxBytes int64) (*StrictDecoder, error) {
	body, n, err := readRequestBodyBytes(r, maxBytes)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	return &StrictDecoder{dec: dec, maxBytes: maxBytes, bodyBytes: n}, nil
}

// readRequestBodyBytes читает тело целиком с ограничением размера и снимает
// ведущий UTF-8 BOM. Общий код для строгого и терпимого декодеров.
func readRequestBodyBytes(r io.Reader, maxBytes int64) ([]byte, int64, error) {
	if r == nil {
		return nil, 0, ErrBodyEmpty
	}
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, 0, fmt.Errorf("read body: %w", err)
	}
	if maxBytes > 0 && n > maxBytes {
		return nil, 0, ErrBodyTooLarge
	}
	if n == 0 {
		return nil, 0, ErrBodyEmpty
	}
	// R60.16 (2026-09-08): strip leading UTF-8 BOM (EF BB BF) if present.
	// PowerShell Out-File default, Notepad, and some older curl builds
	// add BOM → 400 "invalid character ï" without this. After the
	// body is fully in our buffer, this is a stateless in-place strip.
	body := buf.Bytes()
	if len(body) >= 3 && body[0] == 0xEF && body[1] == 0xBB && body[2] == 0xBF {
		body = body[3:]
		n -= 3
		if n == 0 {
			// BOM-only body (e.g. 0xEF 0xBB 0xBF and nothing else)
			// is treated as empty — same as `n == 0` before the strip.
			return nil, 0, ErrBodyEmpty
		}
	}
	return body, n, nil
}

// NewStrictDecoderFromBytes wraps raw JSON bytes with a strict decoder.
// Useful for testing or when the body was already read into memory.
func NewStrictDecoderFromBytes(data []byte) *StrictDecoder {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return &StrictDecoder{dec: dec, maxBytes: int64(len(data)), bodyBytes: int64(len(data))}
}

// Decode parses the next JSON value into v, rejecting unknown fields.
// Returns a json.UnmarshalTypeError-compatible error on type mismatches.
func (d *StrictDecoder) Decode(v interface{}) error {
	if d == nil || d.dec == nil {
		return ErrBodyEmpty
	}
	return d.dec.Decode(v)
}

// BodyBytes returns the number of bytes read from the body. Useful for
// request size accounting in metrics.
func (d *StrictDecoder) BodyBytes() int64 {
	if d == nil {
		return 0
	}
	return d.bodyBytes
}

// MaxRequestBodyBytes is the default maximum request body size. 32 MB is
// large enough for ~8K-token prompts with images, and small enough to
// prevent DoS via oversized bodies.
const MaxRequestBodyBytes = 32 * 1024 * 1024

// MaxStreamingBodyBytes is the maximum body size for streaming endpoints
// that may receive large message arrays. 4 MB is enough for ~1K messages.
const MaxStreamingBodyBytes = 4 * 1024 * 1024

// DecodeJSONRequest is a convenience function that combines strict decoder
// construction and decoding in one call. It returns:
//   - ErrBodyEmpty if the body is empty
//   - ErrBodyTooLarge if the body exceeds maxBytes
//   - a json error if parsing fails (caller maps to 400)
//   - nil on success
//
// Example:
//
//	var req chatRequest
//	if err := types.DecodeJSONRequest(r.Body, MaxRequestBodyBytes, &req); err != nil {
//	    switch {
//	    case errors.Is(err, types.ErrBodyEmpty):
//	        writeError(w, http.StatusBadRequest, "empty body")
//	    case errors.Is(err, types.ErrBodyTooLarge):
//	        writeError(w, http.StatusRequestEntityTooLarge, "body too large")
//	    default:
//	        writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
//	    }
//	    return
//	}
func DecodeJSONRequest(r io.Reader, maxBytes int64, v interface{}) error {
	dec, err := NewStrictDecoder(r, maxBytes)
	if err != nil {
		return err
	}
	return dec.Decode(v)
}

// DecodeJSONRequestLenient — декодирование тела БЕЗ DisallowUnknownFields.
//
// ЗАЧЕМ. Настоящая Ollama (и llama.cpp-сервер) декодируют запрос обычным
// encoding/json: неизвестные поля ИГНОРИРУЮТСЯ. Наш строгий декодер отвечал
// HTTP 400 на любое поле, которого нет в структуре, и это годами ломало живых
// клиентов на КЛИЕНТСКИХ (инференс) API:
//
//	options.top_k            — OpenWebUI (advanced params)
//	options.use_mmap         — OpenWebUI (advanced params)
//	options.num_gpu          — OpenWebUI
//	tool_choice / tool_name  — Cline (Ollama-провайдер)
//	max_output_tokens        — Cline
//	thinking / reasoning_content — история сессии, которую клиент вернул нам же
//
// Каждый раз это лечилось добавлением ОДНОГО поля — и повторялось со следующим.
// Причина не в поле, а в политике: клиентский API обязан быть терпимым (как у
// Ollama), иначе «работает одинаково для всех клиентов» недостижимо.
//
// Контракт:
//   - КЛИЕНТСКИЕ API (/api/chat, /api/generate, /api/embed*, /v1/*) — терпимы,
//     неизвестные поля игнорируются и возвращаются для лога;
//   - УПРАВЛЯЮЩИЕ API (/api/models/load*, /reload, /config/update, /api/hf/*) —
//     остаются строгими: там клиент — наш балансер, и 400 действительно ловит
//     наш собственный баг (см. docs/architecture/balancer-cppworker-contract.md).
//
// Возвращает имена неизвестных полей верхнего уровня (отсортированные) — чтобы
// диагностика не терялась: мы не отказываем клиенту, но пишем это в лог.
func DecodeJSONRequestLenient(r io.Reader, maxBytes int64, v interface{}) ([]string, error) {
	body, _, err := readRequestBodyBytes(r, maxBytes)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return nil, err
	}
	return UnknownTopLevelFields(body, v), nil
}

// UnknownTopLevelFields возвращает имена полей верхнего уровня JSON-объекта,
// которых нет в структуре v (сравнение по json-тегам). Пустой список — тело
// точно соответствует структуре.
func UnknownTopLevelFields(body []byte, v interface{}) []string {
	if v == nil {
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	known := jsonFieldNames(reflect.TypeOf(v))
	var unknown []string
	for k := range raw {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	return unknown
}

// jsonFieldNames собирает имена полей структуры (json-теги) рекурсивно по
// вложенным и встроенным структурам.
func jsonFieldNames(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	var walk func(rt reflect.Type, depth int)
	walk = func(rt reflect.Type, depth int) {
		if rt == nil || depth > 4 {
			return
		}
		for rt.Kind() == reflect.Ptr || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Array {
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct {
			return
		}
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if f.PkgPath != "" && !f.Anonymous {
				continue // неэкспортируемое
			}
			tag := f.Tag.Get("json")
			name := strings.Split(tag, ",")[0]
			if name == "-" {
				continue
			}
			if name != "" {
				out[name] = true
			}
			if f.Anonymous || (name == "" && f.Type.Kind() == reflect.Struct) {
				walk(f.Type, depth+1)
			}
		}
	}
	walk(t, 0)
	return out
}

// IsJSONSyntaxError reports whether err is a JSON syntax error (e.g. the
// client sent invalid JSON). The returned wrappedError is suitable for
// HTTP 400 responses.
func IsJSONSyntaxError(err error) bool {
	if err == nil {
		return false
	}
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	var invalidUnmarshalErr *json.InvalidUnmarshalError
	return errors.As(err, &syntaxErr) ||
		errors.As(err, &typeErr) ||
		errors.As(err, &invalidUnmarshalErr) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}

// Common JSON field name constants. Use these constants everywhere instead
// of hardcoding field names to prevent typos that DisallowUnknownFields
// would catch (but only at the request boundary, not at the source).
const (
	FieldModel            = "model"
	FieldMessages         = "messages"
	FieldPrompt           = "prompt"
	FieldStream           = "stream"
	FieldTools            = "tools"
	FieldToolChoice       = "tool_choice"
	FieldToolCalls        = "tool_calls"
	FieldToolCallID       = "tool_call_id"
	FieldName             = "name"
	FieldContent          = "content"
	FieldRole             = "role"
	FieldReasoning        = "reasoning"
	FieldReasoningContent = "reasoning_content"
	FieldThinking         = "thinking"
	FieldTemperature      = "temperature"
	FieldTopP             = "top_p"
	FieldMaxTokens        = "max_tokens"
	FieldNumCtx           = "num_ctx"
	FieldNumPredict       = "num_predict"
	FieldSeed             = "seed"
	FieldStop             = "stop"
	FieldTopK             = "top_k"
	FieldRepeatPenalty    = "repeat_penalty"
	FieldFrequencyPenalty = "frequency_penalty"
	FieldPresencePenalty  = "presence_penalty"
	FieldStreamOptions    = "stream_options"
	FieldIncludeUsage     = "include_usage"
	FieldKeepAlive        = "keep_alive"
	FieldContextSize      = "contextSize"
	FieldBatchSize        = "batchSize"
	FieldGpuLayers        = "gpuLayers"
	FieldKvCacheType      = "kvCacheType"
	FieldFlashAttn        = "flashAttn"
	FieldUseMmap          = "useMmap"
	FieldEnableReasoning  = "enableReasoning"
	FieldReasoningEffort  = "reasoning_effort"
	FieldN                = "n"
	FieldUser             = "user"
	FieldResponseFormat   = "response_format"
	FieldLogitBias        = "logit_bias"
	FieldLogprobs         = "logprobs"
	FieldTopLogprobs      = "top_logprobs"
	FieldSuffix           = "suffix"
	FieldEcho             = "echo"
	FieldBestOf           = "best_of"
)

// Header name constants. Use these instead of string literals to prevent
// header name typos.
const (
	HeaderContentType      = "Content-Type"
	HeaderAuthorization    = "Authorization"
	HeaderXAPIToken        = "X-API-Token"
	HeaderXRequestID       = "X-Request-Id"
	HeaderXAccelBuffering  = "X-Accel-Buffering"
	HeaderLocation         = "Location"
	HeaderCacheControl     = "Cache-Control"
	HeaderConnection       = "Connection"
	HeaderRetryAfter       = "Retry-After"
	HeaderTransferEncoding = "Transfer-Encoding"
	HeaderContentLength    = "Content-Length"
)

// Content type values used by the contract. Some clients check exact
// strings, others are lenient. Use these constants everywhere to prevent
// "Content-Type" vs "content-type" mismatches.
const (
	ContentTypeJSON      = "application/json"
	ContentTypeSSE       = "text/event-stream"
	ContentTypeNDJSON    = "application/x-ndjson"
	ContentTypeFormURL   = "application/x-www-form-urlencoded"
	ContentTypeTextPlain = "text/plain; charset=utf-8"
)

// BearerPrefix is the prefix for `Authorization: Bearer <token>` headers.
const BearerPrefix = "Bearer "

// Common content type values that clients send in requests.
const (
	ContentTypeChatGPTHTML = "text/html; charset=utf-8" // legacy
	ContentTypeOctetStream = "application/octet-stream"
)
