// hf_mock.go — управляемый мок HuggingFace Hub для тестов HF-загрузки.
//
// ЗАЧЕМ В ОБЫЧНОМ (не _test) ФАЙЛЕ: один и тот же мок нужен в двух пакетах —
// internal/sdbackend (механика: файлы на диске, profile.json, реестр) и
// cmd/sdworker (HTTP-контракт: маршруты, 202, форма прогресса). Go не позволяет
// импортировать _test-файлы, а вторая копия мока разъехалась бы с первой —
// ровно тот класс проблем, из-за которого контракт UI и воркера разошёлся в
// Phase 5. Поэтому мок экспортирован и живёт рядом с кодом, который тестирует.
//
// ФАЙЛ НЕ ВХОДИТ В БОЕВОЙ ПУТЬ: ничего, кроме net/http/httptest/json, не
// импортирует и не вызывается из продуктовых функций — только из тестов
// (hf_test.go здесь и handlers_hf_test.go в cmd/sdworker).
package sdbackend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// HFMockServer — мок HF Hub, отвечающий на три вида запросов:
//
//	GET /models/{repo}/tree/{rev}   — список файлов (tree API)
//	GET /models/{repo}              — fallback-список (siblings)
//	GET /{repo}/resolve/{rev}/{f}   — содержимое файла
//
// Пути БЕЗ префикса /api: cppbackend при непустом mirror использует прямые пути
// (см. getAPIURL в hf_downloader.go), поэтому зеркало (HF_MIRROR) в тестах —
// это URL мока.
type HFMockServer struct {
	*httptest.Server
	mu sync.Mutex
	// files — repo → filename → размер в байтах.
	files map[string]map[string]int64
	// failTree — репозитории, у которых tree отдаёт 404 (несуществующий репо).
	failTree map[string]bool
	// searchRepos — какие id вернуть на /models?search=...
	searchRepos []string
	// blockBytes/blocked/release — «залипание» передачи: сервер пишет
	// blockBytes байт, флашит и ждёт release. Нужно, чтобы поймать прогресс в
	// середине загрузки детерминированно, а не по таймингу.
	blockBytes int64
	blocked    chan struct{}
	release    chan struct{}
	// authHeaders — заголовки Authorization, пришедшие на скачивание файлов
	// (проверка, что X-HF-Token доехал до загрузчика).
	authHeaders []string
	// resolveHits — сколько раз запрашивали содержимое файла.
	resolveHits int
	// latency — искусственная задержка скачивания файла.
	latency time.Duration
	// abortBytes — оборвать ответ после N байт (имитация сетевого обрыва).
	abortBytes int64
}

// NewHFMockServer — запуск мока (закрывается автоматически по t.Cleanup).
func NewHFMockServer(t *testing.T) *HFMockServer {
	t.Helper()
	m := &HFMockServer{
		files:    map[string]map[string]int64{},
		failTree: map[string]bool{},
	}
	m.Server = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.Close)
	return m
}

// AddFile — объявить файл репозитория с заданным размером.
func (m *HFMockServer) AddFile(repo, name string, size int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.files[repo] == nil {
		m.files[repo] = map[string]int64{}
	}
	m.files[repo][name] = size
}

// FailRepo — репозиторий, для которого tree отдаёт 404.
func (m *HFMockServer) FailRepo(repo string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failTree[repo] = true
}

// SetSearchRepos — результаты поиска (/api/models?search=...).
func (m *HFMockServer) SetSearchRepos(ids []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.searchRepos = append([]string(nil), ids...)
}

// SetFileLatency — задержка перед отдачей файла (для проверки «не блокирует»).
func (m *HFMockServer) SetFileLatency(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.latency = d
}

// BlockAfter — залипнуть после bytes байт; возвращает release-функцию
// (идемпотентную: вызывать можно и из defer, и явно).
func (m *HFMockServer) BlockAfter(bytes int64) func() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blockBytes = bytes
	m.blocked = make(chan struct{})
	m.release = make(chan struct{})
	var once sync.Once
	return func() { once.Do(func() { close(m.release) }) }
}

// WaitBlocked — дождаться, что сервер РЕАЛЬНО начал «залипать».
//
// R-Image (2026-10-02): без этого тесты, которые проверяют статус
// "downloading"/частичный прогресс, флапали под нагрузкой: они сэмплировали
// состояние, не зная, успел ли сервер дойти до точки блокировки. Если ожидание
// истекло — возвращаем false, и тест обязан сказать об этом явно, а не падать
// на несвязанном утверждении.
func (m *HFMockServer) WaitBlocked(timeout time.Duration) bool {
	m.mu.Lock()
	blocked := m.blocked
	m.mu.Unlock()
	if blocked == nil {
		return false
	}
	select {
	case <-blocked:
		return true
	case <-time.After(timeout):
		return false
	}
}

// AbortAfter — разорвать соединение после bytes байт (имитация сетевого
// обрыва). Нужно, чтобы получить ЧАСТИЧНЫЙ .download-файл: cppbackend сохраняет
// его для resume при сетевой ошибке (в отличие от отмены, где темп удаляется).
func (m *HFMockServer) AbortAfter(bytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.abortBytes = bytes
}

// LastAuthHeader — последний Authorization, полученный при скачивании файла.
func (m *HFMockServer) LastAuthHeader() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.authHeaders) == 0 {
		return ""
	}
	return m.authHeaders[len(m.authHeaders)-1]
}

// ResolveHits — сколько раз запрашивали содержимое файлов.
func (m *HFMockServer) ResolveHits() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resolveHits
}

func (m *HFMockServer) handle(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(path, "/")

	// Поиск: /models?search=...
	if len(parts) == 1 && parts[0] == "models" {
		m.handleSearch(w)
		return
	}
	// /models/{author}/{name}/tree/{rev}
	if len(parts) == 5 && parts[0] == "models" && parts[3] == "tree" {
		m.handleTree(w, parts[1]+"/"+parts[2])
		return
	}
	// /models/{author}/{name} — fallback (siblings)
	if len(parts) == 3 && parts[0] == "models" {
		m.handleModelInfo(w, parts[1]+"/"+parts[2])
		return
	}
	// /{author}/{name}/resolve/{rev}/{file...}
	if idx := indexOfString(parts, "resolve"); idx == 2 && len(parts) >= 5 {
		repo := parts[0] + "/" + parts[1]
		m.handleResolve(w, r, repo, strings.Join(parts[4:], "/"))
		return
	}
	http.NotFound(w, r)
}

func (m *HFMockServer) handleSearch(w http.ResponseWriter) {
	m.mu.Lock()
	ids := append([]string(nil), m.searchRepos...)
	m.mu.Unlock()

	type repoJSON struct {
		ID          string `json:"id"`
		LastMod     string `json:"lastModified"`
		Downloads   int    `json:"downloads"`
		Likes       int    `json:"likes"`
		PipelineTag string `json:"pipeline_tag"`
	}
	out := make([]repoJSON, 0, len(ids))
	for i, id := range ids {
		out = append(out, repoJSON{
			ID: id, LastMod: "2026-09-28T00:00:00Z",
			Downloads: 1000 - i, Likes: 10, PipelineTag: "text-to-image",
		})
	}
	writeHFMockJSON(w, out)
}

func (m *HFMockServer) handleTree(w http.ResponseWriter, repo string) {
	m.mu.Lock()
	fail := m.failTree[repo]
	fileMap := m.files[repo]
	m.mu.Unlock()
	if fail || fileMap == nil {
		http.Error(w, "Repository not found", http.StatusNotFound)
		return
	}

	type entry struct {
		Type string `json:"type"`
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	names := make([]string, 0, len(fileMap))
	for name := range fileMap {
		names = append(names, name)
	}
	sortStringsAsc(names)
	// README.md добавлен специально: он не должен попасть в список весов.
	entries := []entry{{Type: "file", Path: "README.md", Size: 10}}
	for _, name := range names {
		entries = append(entries, entry{Type: "file", Path: name, Size: fileMap[name]})
	}
	writeHFMockJSON(w, entries)
}

func (m *HFMockServer) handleModelInfo(w http.ResponseWriter, repo string) {
	m.mu.Lock()
	fail := m.failTree[repo]
	fileMap := m.files[repo]
	m.mu.Unlock()
	if fail || fileMap == nil {
		http.Error(w, "Repository not found", http.StatusNotFound)
		return
	}
	siblings := make([]map[string]any, 0, len(fileMap))
	for name, size := range fileMap {
		siblings = append(siblings, map[string]any{"rfilename": name, "size": size})
	}
	writeHFMockJSON(w, map[string]any{"siblings": siblings})
}

func (m *HFMockServer) handleResolve(w http.ResponseWriter, r *http.Request, repo, name string) {
	m.mu.Lock()
	m.resolveHits++
	m.authHeaders = append(m.authHeaders, r.Header.Get("Authorization"))
	blockBytes := m.blockBytes
	blocked := m.blocked
	release := m.release
	latency := m.latency
	abortBytes := m.abortBytes
	size, ok := m.files[repo][name]
	m.mu.Unlock()

	if !ok {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	if latency > 0 {
		time.Sleep(latency)
	}

	// Range не поддерживаем: тестам достаточно 200 (см. требование задачи).
	// cppbackend корректно обрабатывает «сервер без Range» — начинает с нуля.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	w.WriteHeader(http.StatusOK)

	if abortBytes > 0 && abortBytes < size {
		// Обрыв: пишем часть и паникуем в хендлере, чтобы net/http разорвал
		// соединение (клиент увидит unexpected EOF и оставит partial для resume).
		writeHFMockPattern(w, abortBytes)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}

	written := int64(0)
	if blockBytes > 0 && blockBytes < size {
		written += writeHFMockPattern(w, blockBytes)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if blocked != nil {
			close(blocked)
		}
		if release != nil {
			// Страховка от подвисания теста: не ждём вечно.
			select {
			case <-release:
			case <-time.After(20 * time.Second):
			}
		}
	}
	for written < size {
		chunk := size - written
		if chunk > 64*1024 {
			chunk = 64 * 1024
		}
		n := writeHFMockPattern(w, chunk)
		if n == 0 {
			return
		}
		written += n
	}
}

func writeHFMockPattern(w http.ResponseWriter, n int64) int64 {
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = byte('a' + i%26)
	}
	nw, err := w.Write(buf)
	if err != nil {
		return 0
	}
	return int64(nw)
}

func writeHFMockJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func indexOfString(parts []string, want string) int {
	for i, p := range parts {
		if p == want {
			return i
		}
	}
	return -1
}

// sortStringsAsc — простая сортировка (детерминированный порядок в тестах).
func sortStringsAsc(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
