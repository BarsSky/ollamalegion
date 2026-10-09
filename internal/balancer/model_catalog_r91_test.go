package balancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// R91 (2026-10-09). Каталог моделей НА ДИСКЕ.
//
// Живой дефект (нагрузочный прогон, две машины): у CPPWORKER-34 каталог моделей
// пуст, у cppworker-gpu-bundled-agent лежит gemma-4. Запрос на незагруженную
// модель выбирал узел «первый healthy из обхода карты» и получал 503
// model_not_found; поднять модель на узле, где файл есть, тоже было нельзя —
// «где файл» балансер не знал.

// catalogStub — минимальный cppworker: отдаёт /api/models/files.
type catalogStub struct {
	srv *httptest.Server

	mu      sync.Mutex
	files   []string
	aliases []string
	hits    int
}

func newCatalogStub(t *testing.T, files, aliases []string) *catalogStub {
	t.Helper()
	s := &catalogStub{files: files, aliases: aliases}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models/files" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		s.mu.Lock()
		s.hits++
		files := append([]string{}, s.files...)
		aliases := append([]string{}, s.aliases...)
		s.mu.Unlock()

		f := make([]map[string]any, 0, len(files))
		for _, name := range files {
			f = append(f, map[string]any{"name": name, "sizeBytes": 1024})
		}
		a := make([]map[string]any, 0, len(aliases))
		for _, name := range aliases {
			a = append(a, map[string]any{"name": name, "parentModel": name})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"files": f, "aliases": a, "count": len(f)})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *catalogStub) port(t *testing.T) int {
	t.Helper()
	port, err := strconv.Atoi(strings.TrimPrefix(s.srv.URL, "http://127.0.0.1:"))
	if err != nil {
		t.Fatalf("parse stub port from %s: %v", s.srv.URL, err)
	}
	return port
}

func (s *catalogStub) hitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

// resetHits — обнулить счётчик опросов (для проверок интервала).
func (s *catalogStub) resetHits() {
	s.mu.Lock()
	s.hits = 0
	s.mu.Unlock()
}

// newCatalogProxy — прокси с двумя llama.cpp-бэкендами (по одному стабу на узел).
func newCatalogProxy(t *testing.T, a, b *catalogStub) *Proxy {
	t.Helper()
	p := newProxyWithCleanup(t, &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "localhost", Port: 8080, APIPort: 8081,
			StatePath: filepath.Join(t.TempDir(), "state.json"),
		},
		Backends: []types.Backend{
			{ID: "node-a", Name: "a", Host: "127.0.0.1", CppWorkerPort: a.port(t),
				Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy, MaxConcurrentReqs: 1},
			{ID: "node-b", Name: "b", Host: "127.0.0.1", CppWorkerPort: b.port(t),
				Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy, MaxConcurrentReqs: 1},
		},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmResourceAware, RequestTimeout: 10,
			QueueTimeout: 5, QueueMaxSize: 50, QueueWorkers: 2, OperatingMode: "standard",
		},
	})
	for _, id := range []string{"node-a", "node-b"} {
		p.UpdateBackendStatus(id, types.StatusHealthy)
	}
	return p
}

// TestNormalizeCatalogModelName — сопоставление имён: клиент просит без .gguf и
// в другом регистре, воркер отдаёт файл с расширением и полным путём.
func TestNormalizeCatalogModelName(t *testing.T) {
	cases := map[string]string{
		"gemma-4-E4B-it-Q4_K_M.gguf":             "gemma-4-e4b-it-q4_k_m",
		"gemma-4-E4B-it-Q4_K_M":                  "gemma-4-e4b-it-q4_k_m",
		"/app/models/gemma-4-E4B-it-Q4_K_M.gguf": "gemma-4-e4b-it-q4_k_m",
		"  Qwen3-Instruct-2507-q4km.gguf  ":      "qwen3-instruct-2507-q4km",
		"":                                       "",
		"C:\\models\\Qwen3.8-27B-UD-Q4_K_M.gguf": "qwen3.8-27b-ud-q4_k_m",
		"/app/models/sub/dir/Model.GGUF":         "model", // регистр расширения не важен
	}
	for in, want := range cases {
		if got := normalizeCatalogModelName(in); got != want {
			t.Errorf("normalizeCatalogModelName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestModelCatalog_RefreshStoresFilesAndAliases — обновление каталога читает
// files[] и aliases[]; «пусто на диске» и «не опрошен» различимы.
func TestModelCatalog_RefreshStoresFilesAndAliases(t *testing.T) {
	a := newCatalogStub(t, []string{"gemma-4-E4B-it-Q4_K_M.gguf"}, []string{"gemma-fast"})
	b := newCatalogStub(t, nil, nil)
	p := newCatalogProxy(t, a, b)

	// До опроса каталога не знаем ничего: известность false (не «файла нет»).
	if has, known := p.BackendModelOnDisk("node-a", "gemma-4-E4B-it-Q4_K_M"); known {
		t.Fatalf("до опроса known должен быть false (has=%v)", has)
	}

	p.RefreshModelCatalogNow()

	if has, known := p.BackendModelOnDisk("node-a", "gemma-4-E4B-it-Q4_K_M"); !known || !has {
		t.Errorf("node-a: файл не найден в каталоге (has=%v known=%v)", has, known)
	}
	if has, known := p.BackendModelOnDisk("node-a", "gemma-fast"); !known || !has {
		t.Errorf("node-a: алиас не попал в каталог (has=%v known=%v)", has, known)
	}
	if has, known := p.BackendModelOnDisk("node-b", "gemma-4-E4B-it-Q4_K_M"); !known || has {
		t.Errorf("node-b: каталог пуст, но файл «найден» (has=%v known=%v)", has, known)
	}
	// Регистр и путь не должны мешать сопоставлению.
	if has, _ := p.BackendModelOnDisk("node-a", "/app/models/GEMMA-4-e4b-it-q4_k_m.gguf"); !has {
		t.Error("сопоставление имени не устойчиво к регистру/пути")
	}
}

// TestSelectBackendForModel_PrefersNodeWithFileOnDisk — главный кейс: модель не
// загружена нигде, узел с файлом выбирается вместо «первого healthy».
func TestSelectBackendForModel_PrefersNodeWithFileOnDisk(t *testing.T) {
	a := newCatalogStub(t, []string{"gemma-4-E4B-it-Q4_K_M.gguf"}, nil) // файл есть
	b := newCatalogStub(t, nil, nil)                                    // каталог пуст
	p := newCatalogProxy(t, a, b)
	p.RefreshModelCatalogNow()

	got := p.llamaCppRouter.selectLlamaCppBackendForModel("gemma-4-E4B-it-Q4_K_M")
	if got != "node-a" {
		t.Fatalf("выбран %q, ожидался node-a (единственный узел с файлом на диске)", got)
	}
}

// TestSelectBackendForModel_NoCatalogKeepsOldBehaviour — каталога нет (опечатка в
// env, воркеры не опрошены): выбор остаётся прежним (любой healthy узел), а не
// пустым — отсутствие телеметрии не должно ломать стенд.
func TestSelectBackendForModel_NoCatalogKeepsOldBehaviour(t *testing.T) {
	t.Setenv(EnvModelCatalogRefreshSec, "0") // фоновая телеметрия выключена
	a := newCatalogStub(t, []string{"gemma-4-E4B-it-Q4_K_M.gguf"}, nil)
	b := newCatalogStub(t, []string{"gemma-4-E4B-it-Q4_K_M.gguf"}, nil)
	p := newCatalogProxy(t, a, b)

	got := p.llamaCppRouter.selectLlamaCppBackendForModel("gemma-4-E4B-it-Q4_K_M")
	if got != "node-a" && got != "node-b" {
		t.Fatalf("без каталога ожидался любой healthy узел, получено %q", got)
	}
	if a.hitCount()+b.hitCount() != 0 {
		t.Errorf("при LB_MODEL_CATALOG_REFRESH_SEC=0 фоновых опросов быть не должно (a=%d b=%d)",
			a.hitCount(), b.hitCount())
	}
}

// TestSelectBackendExcluding_PrefersNodeWithFileOnDisk — переезд: среди узлов,
// где файл лежит, выбирается не исключённый.
func TestSelectBackendExcluding_PrefersNodeWithFileOnDisk(t *testing.T) {
	a := newCatalogStub(t, []string{"gemma-4-E4B-it-Q4_K_M.gguf"}, nil)
	b := newCatalogStub(t, []string{"gemma-4-E4B-it-Q4_K_M.gguf"}, nil)
	p := newCatalogProxy(t, a, b)
	p.RefreshModelCatalogNow()

	if got := p.llamaCppRouter.selectLlamaCppBackendExcluding("gemma-4-E4B-it-Q4_K_M", nil); got != "node-a" {
		t.Fatalf("без исключений ожидался node-a (детерминированный tie-break по id), получено %q", got)
	}
	got := p.llamaCppRouter.selectLlamaCppBackendExcluding("gemma-4-E4B-it-Q4_K_M", map[string]bool{"node-a": true})
	if got != "node-b" {
		t.Fatalf("исключили node-a: ожидался node-b (там тоже есть файл), получено %q", got)
	}
}

// TestRefreshStaleModelCatalogs_RespectsInterval — «протух» ли снимок, решает
// LB_MODEL_CATALOG_REFRESH_SEC; при 0 фоновое обновление молчит.
func TestRefreshStaleModelCatalogs_RespectsInterval(t *testing.T) {
	// Интервал выключаем ДО создания прокси: poller метрик делает немедленный
	// первый проход, и с дефолтным интервалом он бы уже опросил воркеры.
	t.Setenv(EnvModelCatalogRefreshSec, "0")
	a := newCatalogStub(t, []string{"m.gguf"}, nil)
	b := newCatalogStub(t, nil, nil)
	p := newCatalogProxy(t, a, b)

	p.refreshStaleModelCatalogs()
	if a.hitCount() != 0 {
		t.Errorf("при интервале 0 опросов быть не должно (hits=%d)", a.hitCount())
	}

	t.Setenv(EnvModelCatalogRefreshSec, "300")
	a.resetHits()
	p.refreshStaleModelCatalogs() // снимка нет → считаем протухшим
	if a.hitCount() != 1 {
		t.Errorf("первый проход обязан опросить бэкенд (hits=%d)", a.hitCount())
	}
	p.refreshStaleModelCatalogs() // снимок свежий (300 с) → второй раз не опрашиваем
	if a.hitCount() != 1 {
		t.Errorf("свежий снимок не должен перечитываться (hits=%d)", a.hitCount())
	}

	// Искусственно старим снимок — обновление снова требуется.
	p.modelCatalog.mu.Lock()
	p.modelCatalog.byID["node-a"].fetchedAt = time.Now().Add(-10 * time.Minute)
	p.modelCatalog.mu.Unlock()
	p.refreshStaleModelCatalogs()
	if a.hitCount() != 2 {
		t.Errorf("протухший снимок обязан перечитаться (hits=%d)", a.hitCount())
	}
}

// TestModelCatalog_DroppedWithBackend — каталог удалённого бэкенда не остаётся в
// памяти (иначе пересозданная запись получила бы устаревший список файлов).
func TestModelCatalog_DroppedWithBackend(t *testing.T) {
	a := newCatalogStub(t, []string{"m.gguf"}, nil)
	b := newCatalogStub(t, nil, nil)
	p := newCatalogProxy(t, a, b)
	p.RefreshModelCatalogNow()
	if _, known := p.BackendModelOnDisk("node-a", "m"); !known {
		t.Fatal("подготовка: каталог node-a должен быть снимком")
	}
	if err := p.RemoveBackend("node-a"); err != nil {
		t.Fatalf("RemoveBackend: %v", err)
	}
	if _, known := p.BackendModelOnDisk("node-a", "m"); known {
		t.Error("каталог удалённого бэкенда остался в памяти")
	}
	if _, ok := p.ModelCatalogSnapshot()["node-a"]; ok {
		t.Error("снимок каталога удалённого бэкенда остался в админ-снимке")
	}
}
