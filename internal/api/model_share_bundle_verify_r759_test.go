//go:build llama_stub

// model_share_bundle_verify_r759_test.go — R759 (2026-11-25): перенос
// image-bundle обязан доказывать, что доехали ВСЕ файлы группы.
//
// ЖИВАЯ ЖАЛОБА. «Не копируется полностью все файлы группы для модели image,
// когда делается перенос».
//
// Две тихие дыры, которые здесь закрыты и проверяются:
//
//  1. ПРИЁМНИКУ НЕ ПЕРЕДАВАЛИСЬ ОЖИДАЕМЫЕ ЧИСЛА. sdworker умеет проверять
//     `bytes`/`files` (или `size` для .gguf) и отклонять неполный поток, но
//     балансер этих параметров не слал — проверка существовала и не работала.
//     Поток, оборванный на ГРАНИЦЕ файла, tar-ридер принимает как нормальный
//     EOF, поэтому «меньше файлов» выглядело как успешный перенос.
//
//  2. НЕ БЫЛО СВЕРКИ РЕЗУЛЬТАТА. Даже если приёмник отчитался 200, никто не
//     спрашивал у него состав после импорта. Теперь балансер делает HEAD экспорта
//     приёмника и сравнивает число файлов и размер с источником.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// shareMockImageWorker — фейковый image-воркер: отдаёт bundle и принимает импорт.
type shareMockImageWorker struct {
	srv          *httptest.Server
	importQuery  string
	listModels   string
	content      []byte
	exportFiles  int
	exportBytes  int64
	importCalls  int
	importBody   int
	importStatus int
}

func newShareMockImageWorker(t *testing.T, content []byte, files int) *shareMockImageWorker {
	t.Helper()
	w := &shareMockImageWorker{content: content, exportFiles: files, exportBytes: int64(len(content)), listModels: `{"models":[]}`}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/image/models/export") && (r.Method == http.MethodHead || r.Method == http.MethodGet):
			rw.Header().Set("X-Bundle-Name", "bundle-a")
			rw.Header().Set("X-Bundle-Bytes", strconv.FormatInt(w.exportBytes, 10))
			rw.Header().Set("X-Bundle-FileCount", strconv.Itoa(w.exportFiles))
			rw.Header().Set("Content-Type", "application/x-tar")
			if r.Method == http.MethodHead {
				rw.WriteHeader(http.StatusOK)
				return
			}
			_, _ = rw.Write(w.content)
		case r.URL.Path == "/api/image/models/import" && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			w.importCalls++
			w.importQuery = r.URL.RawQuery
			w.importBody = len(body)
			if w.importStatus != 0 && w.importStatus != http.StatusOK {
				rw.WriteHeader(w.importStatus)
				_, _ = rw.Write([]byte(`{"error":"mock import rejected","code":"size_mismatch"}`))
				return
			}
			rw.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(rw).Encode(map[string]interface{}{"name": "bundle-a", "bytes": len(body), "fileCount": w.exportFiles})
		case r.URL.Path == "/api/image/models" && r.Method == http.MethodGet:
			rw.Header().Set("Content-Type", "application/json")
			_, _ = rw.Write([]byte(w.listModels))
		default:
			rw.WriteHeader(http.StatusNotFound)
			_, _ = rw.Write([]byte(`{"error":"unexpected path: ` + r.URL.Path + `"}`))
		}
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *shareMockImageWorker) port(t *testing.T) int {
	t.Helper()
	var port int
	if _, err := fmt.Sscanf(strings.TrimPrefix(w.srv.URL, "http://127.0.0.1:"), "%d", &port); err != nil {
		t.Fatalf("parse mock port from %s: %v", w.srv.URL, err)
	}
	return port
}

// newShareImageServerR759 — балансер с двумя image-бэкендами (источник и приёмник).
func newShareImageServerR759(t *testing.T, src, dst *shareMockImageWorker) (*Server, *httptest.Server) {
	t.Helper()
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
		Backends: []types.Backend{
			{ID: "isrc", Name: "image-src", Host: "127.0.0.1", ImagePort: src.port(t),
				Type: types.BackendTypeImage, Weight: 1, MaxConcurrentReqs: 4, Status: types.StatusHealthy},
			{ID: "idst", Name: "image-dst", Host: "127.0.0.1", ImagePort: dst.port(t),
				Type: types.BackendTypeImage, Weight: 1, MaxConcurrentReqs: 4, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{HealthCheckInterval: 10,
			MetricsInterval: 5, RequestTimeout: 30, QueueMaxSize: 100},
		API:  types.APISettings{RateLimit: 500, RateBurst: 1000},
		Auth: types.AuthConfig{Enabled: false},
	}
	proxy := balancer.NewProxy(config)
	server := NewServer(proxy, config, balancer.NewHealthChecker(proxy, 10*time.Second, 3))
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = proxy.Shutdown(context.Background()) })
	return server, ts
}

// TestR759_ShareImageBundle_SendsExpectedComposition — приёмник получает bytes и files.
//
// Без них проверка на приёмнике не работает вовсе: он сравнивает только то, что
// ему передали, а балансер раньше не передавал ничего.
func TestR759_ShareImageBundle_SendsExpectedComposition(t *testing.T) {
	src := newShareMockImageWorker(t, []byte("TAR-BYTES-1234"), 5)
	dst := newShareMockImageWorker(t, nil, 5)
	_, ts := newShareImageServerR759(t, src, dst)

	code, job := startShare(t, ts, `{"source":"isrc","model":"bundle-a","targets":["idst"]}`)
	if code != http.StatusAccepted && code != http.StatusOK {
		t.Fatalf("POST share: status=%d job=%v", code, job)
	}
	id, _ := job["id"].(string)
	final := waitShare(t, ts, id)

	if dst.importCalls == 0 {
		t.Fatalf("импорт не вызывался: job=%v", final)
	}
	q := dst.importQuery
	if !strings.Contains(q, "bytes=14") {
		t.Errorf("в URL импорта нет bytes=%d: %q", len(src.content), q)
	}
	if !strings.Contains(q, "files=5") {
		t.Errorf("в URL импорта нет files=5: %q — без него приёмник не отличит неполный bundle от полного", q)
	}
}

// TestR759_ShareImageBundle_VerifiesContents — сверка результата: меньше файлов → отказ.
func TestR759_ShareImageBundle_VerifiesContents(t *testing.T) {
	src := newShareMockImageWorker(t, []byte("TAR-BYTES-1234"), 5)
	// Приёмник «принял» поток, но после импорта у него 3 файла из 5 — ровно то,
	// что оператор видит как «скопировалось не всё».
	dst := newShareMockImageWorker(t, nil, 3)
	_, ts := newShareImageServerR759(t, src, dst)

	code, job := startShare(t, ts, `{"source":"isrc","model":"bundle-a","targets":["idst"]}`)
	if code != http.StatusAccepted && code != http.StatusOK {
		t.Fatalf("POST share: status=%d job=%v", code, job)
	}
	final := waitShare(t, ts, job["id"].(string))

	targets, _ := final["targets"].([]interface{})
	if len(targets) != 1 {
		t.Fatalf("ожидалась одна цель, получено %d: %v", len(targets), final)
	}
	tg, _ := targets[0].(map[string]interface{})
	if state, _ := tg["state"].(string); state != shareTargetFailed {
		t.Fatalf("состояние цели = %q, want %q: приёмник отчитался 200, но файлов доехало меньше — "+
			"перенос обязан быть отклонён, а не «готово» (target=%v)", state, shareTargetFailed, tg)
	}
	errText, _ := tg["error"].(string)
	if !strings.Contains(errText, "3") || !strings.Contains(errText, "5") {
		t.Errorf("в отказе нет чисел (сколько/сколько ожидалось): %q", errText)
	}
	if state, _ := final["state"].(string); state != shareStatePartial {
		t.Errorf("состояние задания = %q, want %q", state, shareStatePartial)
	}
}

// TestR759_ShareImageBundle_SucceedsWhenComplete — состав совпал → done и число файлов.
func TestR759_ShareImageBundle_SucceedsWhenComplete(t *testing.T) {
	src := newShareMockImageWorker(t, []byte("TAR-BYTES-1234"), 5)
	dst := newShareMockImageWorker(t, nil, 5)
	_, ts := newShareImageServerR759(t, src, dst)

	code, job := startShare(t, ts, `{"source":"isrc","model":"bundle-a","targets":["idst"]}`)
	if code != http.StatusAccepted && code != http.StatusOK {
		t.Fatalf("POST share: status=%d", code)
	}
	final := waitShare(t, ts, job["id"].(string))
	targets, _ := final["targets"].([]interface{})
	tg, _ := targets[0].(map[string]interface{})
	if state, _ := tg["state"].(string); state != shareTargetDone {
		t.Fatalf("состояние цели = %q, want %q (target=%v)", state, shareTargetDone, tg)
	}
	if got, _ := tg["filesCopied"].(float64); int(got) != 5 {
		t.Errorf("filesCopied = %v, want 5 — UI рисует «файлов N из M» по этому полю", tg["filesCopied"])
	}
	if got, _ := tg["totalFiles"].(float64); int(got) != 5 {
		t.Errorf("totalFiles = %v, want 5", tg["totalFiles"])
	}
}

// TestR759_ShareTextModel_SendsSize — для одного .gguf приёмнику уходит size.
//
// Свой минимальный мок (а не общий shareMockWorker из model_share_test.go):
// общий переиспользуется другими тестами, и добавлять в него поля ради одного
// случая значило бы менять чужой контракт (и его выравнивание структур).
func TestR759_ShareTextModel_SendsSize(t *testing.T) {
	content := []byte("GGUF-BYTES")
	importQuery := ""
	// filesJSON — инвентарь воркера: у источника файл есть, у приёмника пусто,
	// иначе балансер справедливо решит «модель уже на месте» и пропустит
	// перенос (проверка shareTargetHasModel).
	newWorker := func(filesJSON string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/api/models/files":
				rw.Header().Set("Content-Type", "application/json")
				_, _ = rw.Write([]byte(filesJSON))
			case r.URL.Path == "/api/models/export" && (r.Method == http.MethodHead || r.Method == http.MethodGet):
				rw.Header().Set("X-Model-Filename", "m.gguf")
				rw.Header().Set("Content-Length", strconv.Itoa(len(content)))
				if r.Method == http.MethodHead {
					rw.WriteHeader(http.StatusOK)
					return
				}
				_, _ = rw.Write(content)
			case r.URL.Path == "/api/models/import" && r.Method == http.MethodPost:
				importQuery = r.URL.RawQuery
				_, _ = io.ReadAll(r.Body)
				rw.Header().Set("Content-Type", "application/json")
				_, _ = rw.Write([]byte(`{"filename":"m.gguf"}`))
			default:
				rw.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	portOf := func(srv *httptest.Server) int {
		var port int
		if _, err := fmt.Sscanf(strings.TrimPrefix(srv.URL, "http://127.0.0.1:"), "%d", &port); err != nil {
			t.Fatalf("parse port: %v", err)
		}
		return port
	}
	srcSrv := newWorker(`{"files":[{"filename":"m.gguf","sizeBytes":10}]}`)
	dstSrv := newWorker(`{"files":[]}`)
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
		Backends: []types.Backend{
			{ID: "src", Name: "source", Host: "127.0.0.1", CppWorkerPort: portOf(srcSrv),
				Type: types.BackendTypeLlamaCpp, Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
			{ID: "dst", Name: "target", Host: "127.0.0.1", CppWorkerPort: portOf(dstSrv),
				Type: types.BackendTypeLlamaCpp, Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{HealthCheckInterval: 10, MetricsInterval: 5, RequestTimeout: 30, QueueMaxSize: 100},
		API:       types.APISettings{RateLimit: 500, RateBurst: 1000},
		Auth:      types.AuthConfig{Enabled: false},
	}
	proxy := balancer.NewProxy(config)
	server := NewServer(proxy, config, balancer.NewHealthChecker(proxy, 10*time.Second, 3))
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = proxy.Shutdown(context.Background()) })

	code, job := startShare(t, ts, `{"source":"src","model":"m","targets":["dst"]}`)
	if code != http.StatusAccepted && code != http.StatusOK {
		t.Fatalf("POST share: status=%d job=%v", code, job)
	}
	waitShare(t, ts, job["id"].(string))
	if importQuery == "" {
		t.Fatalf("импорт не вызывался")
	}
	if !strings.Contains(importQuery, "size=") {
		t.Errorf("в URL импорта нет size=: %q — приёмник не сверит размер принятого файла", importQuery)
	}
}
