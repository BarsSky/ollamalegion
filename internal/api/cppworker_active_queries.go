// Package api — active queries helpers (Round 26 v0.5.13).
//
// Эти helpers нужны для Bug #1+#2 v0.5.13: WebUI/balancer должны
// проверять, есть ли в полёте активные генерации для модели ПЕРЕД
// reload/apply. Без этого WebUI думает что «настройки заблокированы»
// при генерации ответа.
//
// Источник истины: cppworker /api/models/active-queries (Round 26).
// Этот файл — клиентская обёртка + кеш.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// GetActiveQueriesForModel — синхронный запрос к cppworker'у для
// получения active_queries count конкретной модели.
//
// Используется в applyModelProfile для детекции busy state.
// Возвращает (count, error). count=0 если модель не загружена или
// если backend недоступен (с warning в логе).
//
// Таймаут: 3 секунды — это lightweight endpoint, не должен висеть.
func (s *Server) GetActiveQueriesForModel(backendID, modelName string) (int64, error) {
	_, _, baseURL, err := s.resolveCppWorkerURL(backendID)
	if err != nil {
		return 0, err
	}

	url := fmt.Sprintf("%s/api/models/active-queries?model=%s", baseURL, modelName)
	// На случай special chars в model name: простой escape.
	if modelName != "" && containsUnsafeURLChars(modelName) {
		url = fmt.Sprintf("%s/api/models/active-queries?model=%s",
			baseURL, urlPathEscape(modelName))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	if backend := s.proxy.GetBackend(backendID); backend != nil && backend.CppWorkerApiToken != "" {
		req.Header.Set("Authorization", "Bearer "+backend.CppWorkerApiToken)
	}

	resp, err := httpClientForProxy.Do(req)
	if err != nil {
		return 0, fmt.Errorf("cppworker unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("cppworker returned %d", resp.StatusCode)
	}

	var payload struct {
		Model         string `json:"model"`
		ActiveQueries int64  `json:"activeQueries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, fmt.Errorf("decode: %w", err)
	}
	return payload.ActiveQueries, nil
}

// containsUnsafeURLChars — простая проверка: содержит ли строка символы,
// которые могут сломать URL.
func containsUnsafeURLChars(s string) bool {
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.' || c == '~':
		default:
			return true
		}
	}
	return false
}

// urlPathEscape — обёртка над url.PathEscape чтобы не импортировать net/url
// в этом helper-файле (используется только в одном месте).
func urlPathEscape(s string) string {
	// Go's stdlib net/url.PathEscape эквивалентен encodeURIComponent в JS
	// для path segments. Здесь используем прямой escape для известных символов
	// чтобы избежать дополнительного импорта.
	// NB: для active-queries model names обычно ASCII, так что этот fallback
	// редко используется.
	const hex = "0123456789ABCDEF"
	var result []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			result = append(result, c)
		} else {
			result = append(result, '%', hex[c>>4], hex[c&15])
		}
	}
	return string(result)
}

// activeQueriesCache — thread-safe in-memory кеш для /api/v1/cppworker/active-queries
// (если api server хочет раздавать WebUI'ю без проксирования каждого запроса).
// TTL: 1 секунда — для busy badge обновления каждые 2-3s этого достаточно.
type activeQueriesCacheEntry struct {
	count     int64
	timestamp time.Time
}

var (
	activeQueriesCacheMu sync.RWMutex
	activeQueriesCache   = make(map[string]activeQueriesCacheEntry) // key: backendID+"|"+modelName
)

const activeQueriesCacheTTL = 1 * time.Second

// GetCachedActiveQueries — возвращает кешированный count + bool (is fresh).
func GetCachedActiveQueries(backendID, modelName string) (int64, bool) {
	activeQueriesCacheMu.RLock()
	defer activeQueriesCacheMu.RUnlock()
	key := backendID + "|" + modelName
	entry, ok := activeQueriesCache[key]
	if !ok || time.Since(entry.timestamp) > activeQueriesCacheTTL {
		return 0, false
	}
	return entry.count, true
}

// setCachedActiveQueries — внутренний setter для кеша.
func setCachedActiveQueries(backendID, modelName string, count int64) {
	activeQueriesCacheMu.Lock()
	defer activeQueriesCacheMu.Unlock()
	activeQueriesCache[backendID+"|"+modelName] = activeQueriesCacheEntry{
		count:     count,
		timestamp: time.Now(),
	}
}

// httpClientForProxy — общий HTTP-клиент с разумными таймаутами.
// Не делаем custom Dial — стандартный net.Dial достаточен.
var httpClientForProxy = &http.Client{
	Timeout: 10 * time.Second,
}

// warnfBusy — convenience log helper.
func warnfBusy(backend, model string, count int64) {
	logger.Get().Debugw("active-queries check",
		"backend", backend,
		"model", model,
		"active", count)
}
