// Package types provides type-safe models for the balancer ↔ cppworker API.
//
// Round 36 Phase 2 (2026-08-17): contract enforcement via Go types + strict
// JSON decoding. See docs/BALANCER_CPPWORKER_API_CONTRACT.md for the full
// specification.
package types

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	if r == nil {
		return nil, ErrBodyEmpty
	}
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if maxBytes > 0 && n > maxBytes {
		return nil, ErrBodyTooLarge
	}
	if n == 0 {
		return nil, ErrBodyEmpty
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
			return nil, ErrBodyEmpty
		}
		buf.Reset()
		buf.Write(body)
	}
	dec := json.NewDecoder(&buf)
	dec.DisallowUnknownFields()
	return &StrictDecoder{dec: dec, maxBytes: maxBytes, bodyBytes: n}, nil
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
	FieldModel              = "model"
	FieldMessages            = "messages"
	FieldPrompt              = "prompt"
	FieldStream              = "stream"
	FieldTools               = "tools"
	FieldToolChoice          = "tool_choice"
	FieldToolCalls           = "tool_calls"
	FieldToolCallID          = "tool_call_id"
	FieldName                = "name"
	FieldContent             = "content"
	FieldRole                = "role"
	FieldReasoning           = "reasoning"
	FieldReasoningContent    = "reasoning_content"
	FieldThinking            = "thinking"
	FieldTemperature         = "temperature"
	FieldTopP                = "top_p"
	FieldMaxTokens           = "max_tokens"
	FieldNumCtx              = "num_ctx"
	FieldNumPredict          = "num_predict"
	FieldSeed                = "seed"
	FieldStop                = "stop"
	FieldTopK                = "top_k"
	FieldRepeatPenalty       = "repeat_penalty"
	FieldFrequencyPenalty    = "frequency_penalty"
	FieldPresencePenalty     = "presence_penalty"
	FieldStreamOptions       = "stream_options"
	FieldIncludeUsage        = "include_usage"
	FieldKeepAlive           = "keep_alive"
	FieldContextSize         = "contextSize"
	FieldBatchSize           = "batchSize"
	FieldGpuLayers           = "gpuLayers"
	FieldKvCacheType         = "kvCacheType"
	FieldFlashAttn           = "flashAttn"
	FieldUseMmap             = "useMmap"
	FieldEnableReasoning     = "enableReasoning"
	FieldReasoningEffort     = "reasoning_effort"
	FieldN                   = "n"
	FieldUser                = "user"
	FieldResponseFormat      = "response_format"
	FieldLogitBias           = "logit_bias"
	FieldLogprobs            = "logprobs"
	FieldTopLogprobs         = "top_logprobs"
	FieldSuffix              = "suffix"
	FieldEcho                = "echo"
	FieldBestOf              = "best_of"
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
