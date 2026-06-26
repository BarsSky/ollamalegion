// Файл оставлен для обратной совместимости; см. cmd/rpcworker/main.go
// (newJSONRequest перенесён туда же, чтобы избежать дублирования package main).
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
)

// newJSONRequest — создаёт POST-запрос с JSON-телом.
func newJSONRequest(method, url string, payload interface{}) (*http.Request, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}