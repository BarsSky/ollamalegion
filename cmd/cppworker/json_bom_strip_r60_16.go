// json_bom_strip_r60_16.go — R60.16 (2026-09-08) UTF-8 BOM stripper
// for JSON request bodies.
//
// Background: cppworker's `json.NewDecoder` doesn't strip a leading
// UTF-8 BOM (EF BB BF). Some Windows tools (PowerShell `Out-File`
// default, Notepad, older curl builds) prepend a BOM, which the
// decoder then sees as invalid JSON → 400 "invalid character ï
// looking for beginning of value".
//
// R60.15 Fix B (rolled back): wrapped r.Body with decodeBOMReader
// (a *bufio.Reader). That introduced production hangs under
// concurrent bridge calls (R60.15 Fix B/C rollback). The bug:
// the bufio.Reader pinned the request body, and the subsequent
// r.Body.Close() / connection-reuse path deadlocked.
//
// R60.16 (this file): read the body ONCE into a byte slice, strip
// the BOM in-place, and replace r.Body with a stateless
// io.NopCloser(bytes.NewReader(...)). No reader wrapping, no
// bufio, no stateful wrapper that pins the connection.
//
// Trade-off: we read the entire body into memory (bounded by
// maxBodySize). Most JSON request bodies are <1 MB, so this is fine.
// MaxBytesReader limits us to a sane size.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// utf8BOM is the 3-byte UTF-8 byte order mark: EF BB BF.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// defaultMaxJSONBodySize — default cap for JSON request bodies.
// 16 MB is enough for any reasonable chat/generate request. Larger
// uploads should go through a different mechanism (multipart,
// streaming, etc.).
const defaultMaxJSONBodySize = 16 * 1024 * 1024

// decodeJSONRequest reads the request body, strips a leading UTF-8
// BOM if present, replaces r.Body with a fresh io.NopCloser, and
// decodes the body as JSON into v.
//
// R60.16 (2026-09-08): replaces the R60.15 Fix B helper
// (decodeJSONRequest) that wrapped r.Body with a stateful reader
// and caused production hangs. This implementation is stateless
// after the call returns.
//
// maxBodySize: 0 = use defaultMaxJSONBodySize (16 MB).
//
// Behavior preserved for non-BOM bodies: identical to
// `json.NewDecoder(r.Body).Decode(v)` — full body read, JSON
// unmarshal, errors propagated as-is.
func decodeJSONRequest(r *http.Request, v interface{}, maxBodySize int64) error {
	if maxBodySize <= 0 {
		maxBodySize = defaultMaxJSONBodySize
	}
	// Read the body fully, bounded by maxBodySize+1 (so we can detect
	// oversize). http.MaxBytesReader needs a ResponseWriter for
	// error response headers; we don't have one in helper scope, so
	// use io.LimitReader and check the size after.
	limited := io.LimitReader(r.Body, maxBodySize+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	if int64(len(body)) > maxBodySize {
		return &oversizeBodyError{maxSize: maxBodySize}
	}
	// Strip leading UTF-8 BOM if present.
	if len(body) >= 3 && body[0] == utf8BOM[0] && body[1] == utf8BOM[1] && body[2] == utf8BOM[2] {
		body = body[3:]
	}
	// Replace r.Body with a fresh, stateless reader so the caller
	// (and any subsequent code) sees a clean body. No stateful wrapper.
	r.Body = io.NopCloser(bytes.NewReader(body))
	// Decode directly from the byte slice — avoids any reader churn.
	return json.Unmarshal(body, v)
}

// oversizeBodyError — returned when the request body exceeds maxBodySize.
// Distinct type so callers (or tests) can check for it.
type oversizeBodyError struct {
	maxSize int64
}

func (e *oversizeBodyError) Error() string {
	return "request body exceeds maximum allowed size"
}
