package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/sdbackend"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// HTTP-тесты HF-слоя sdworker (без реальной сети)
// ============================================================
//
// Проверяется КОНТРАКТ, который снят с UI (webui/js/modules/image-page.js):
// имена путей и полей ответа. Мок HF Hub (internal/sdbackend/hf_mock.go) уже
// проверяет механику загрузки в тестах sdbackend (файлы на диске, манифест,
// profile.json); здесь — HTTP-обвязка воркера: маршруты, 202 без ожидания
// загрузки, форма JSON, которую читает UI.

// newHFTestApp — приложение с HF-слоем на мок-сервере HF.
func newHFTestApp(t *testing.T, mock *sdbackend.HFMockServer) (*App, *sdbackend.Registry) {
	t.Helper()
	dir := t.TempDir()
	cfg := sdbackend.DefaultConfig()
	cfg.ModelsDir = filepath.Join(dir, "models")
	cfg.DownloadsDir = filepath.Join(dir, "downloads")
	cfg.HFMirror = mock.URL

	// ВАЖНО: реестр и менеджер обязаны смотреть в ОДИН каталог. Менеджер
	// резолвит ModelsDir в абсолютный путь (filepath.Abs), поэтому реестр
	// создаём уже по абсолютному — иначе модель скачается в temp, а реестр
	// будет сканировать относительный ./models и не увидит её.
	modelsDir, err := cfg.ModelsDirAbs()
	if err != nil {
		t.Fatalf("ModelsDirAbs: %v", err)
	}
	reg := sdbackend.NewRegistry(modelsDir)
	if err := reg.Load(); err != nil {
		t.Fatalf("registry: %v", err)
	}
	hf, err := sdbackend.NewHFManager(&cfg, reg)
	if err != nil {
		t.Fatalf("NewHFManager: %v", err)
	}

	// Сервис собираем ЦЕЛИКОМ (как newTestApp): HF-тесты проверяют и
	// GET /api/image/models, который читает Supervisor/Store/Metrics. Без них
	// хендлер паникует на nil — и тест «доказывал» бы поломку, которой нет.
	metrics := sdbackend.NewMetrics()
	store, err := sdbackend.NewImageStore(filepath.Join(dir, "images"), "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	sup := sdbackend.NewSupervisor(&cfg, reg, metrics)
	sup.SetReadinessPoll(2 * time.Millisecond)
	sup.SetPollEvery(2 * time.Millisecond)
	sup.SetReadinessTimeout(2 * time.Second)
	runner := sdbackend.NewJobRunner(&cfg, reg, sup, metrics, store)

	svc := &sdbackend.Service{
		Config: &cfg, Registry: reg, Sup: sup, Metrics: metrics,
		Queue: runner.Queue(), Store: store, Runner: runner,
		HF:        hf,
		Idle:      sdbackend.NewIdleUnloadManager(&cfg, reg, sup, metrics),
		StartedAt: time.Now(),
	}
	app := newApp(svc)
	cleanupSupervisor(t, app)
	// Порядок cleanup (LIFO): supervisor → ожидание HF → RemoveAll temp-каталога
	// (t.TempDir). Дожидаемся ФОНОВЫХ загрузок, иначе на Windows RemoveAll падает
	// с «directory is not empty»: bundle-загрузка асинхронна по контракту и
	// продолжает писать в .download после возврата теста.
	t.Cleanup(func() { waitHFIdle(t, hf, 10*time.Second) })
	return app, reg
}

// waitHFIdle — ждём остановки фоновых HF-загрузок (Close отменяет их, но
// горутина загрузчика завершается асинхронно и держит .download-файлы).
//
// R83-fix (2026-10-09): после исчезновения записей о загрузках проверяем ещё и
// «тишину» в каталоге моделей. Запись о bundle снимается раньше, чем писатель
// закрывает файлы, и на Windows TempDir RemoveAll падал:
//
//	TestHF_BOMBodyAccepted: TempDir RemoveAll cleanup: unlinkat ...\models\bom-bundle:
//	    The directory is not empty.
//
// (self-hosted Windows runner, 2026-10-09). Два одинаковых снимка каталога подряд
// означают, что писатели действительно закончили.
func waitHFIdle(t *testing.T, hf *sdbackend.HFManager, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	prev, prevOK := "", false
	for time.Now().Before(deadline) {
		snap := hf.ListDownloads()
		if len(snap.Bundles) == 0 && len(snap.Active) == 0 {
			cur := dirFingerprint(hf.ModelsDir())
			if prevOK && cur == prev {
				return
			}
			prev, prevOK = cur, true
			time.Sleep(250 * time.Millisecond)
			continue
		}
		prevOK = false
		time.Sleep(20 * time.Millisecond)
	}
}

// dirFingerprint — «отпечаток» дерева каталога (пути + размеры): меняется, пока
// писатель создаёт/дописывает файлы. Ошибки обхода игнорируются: цель — увидеть
// два одинаковых снимка подряд.
func dirFingerprint(root string) string {
	var b strings.Builder
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil //nolint:nilerr // неполный обход допустим: важен факт изменений
		}
		b.WriteString(p)
		b.WriteByte(':')
		b.WriteString(strconv.FormatInt(info.Size(), 10))
		b.WriteByte(';')
		return nil
	})
	return b.String()
}

// newHFClient — HTTP-клиент к настоящему mux воркера (проверяются маршруты).
func newHFClient(t *testing.T, app *App) (*httptest.Server, *http.Client) {
	t.Helper()
	srv := httptest.NewServer(app.setupRouter())
	t.Cleanup(srv.Close)
	return srv, &http.Client{Timeout: 20 * time.Second}
}

// hfDo — запрос к воркеру: body (nil = без тела), возвращает статус и тело.
func hfDo(t *testing.T, client *http.Client, base, method, path string, body any, token string) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-HF-Token", token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, data
}

// waitHTTPModelRegistered — ждём модель в реестре через HTTP-список.
func waitHTTPModelRegistered(t *testing.T, client *http.Client, base, name string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		code, body := hfDo(t, client, base, http.MethodGet, "/api/image/models", nil, "")
		if code == http.StatusOK && strings.Contains(string(body), `"`+name+`"`) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("модель %q не появилась в GET /api/image/models", name)
}

// ============================================================
// 1. Маршруты зарегистрированы (иначе UI получает 404 — исходный баг)
// ============================================================

func TestHF_RoutesRegistered(t *testing.T) {
	mock := sdbackend.NewHFMockServer(t)
	mock.AddFile("acme/z-image-turbo", "diffusion.gguf", 1024)
	mock.AddFile("acme/z-image-turbo", "vae.safetensors", 512)
	mock.SetSearchRepos([]string{"acme/z-image-turbo"})

	app, _ := newHFTestApp(t, mock)
	srv, client := newHFClient(t, app)

	// Все пути, которые дёргает UI, обязаны существовать (не 404/405).
	// expect404 — «эндпоинт есть, но записи нет» (это НЕ признак отсутствия
	// маршрута: UI именно 404 и ожидает, см. image-page.js:1621).
	cases := []struct {
		method    string
		path      string
		body      any
		want      []int
		expect404 bool
	}{
		{http.MethodGet, "/api/hf/search?q=z-image", nil, []int{200}, false},
		{http.MethodGet, "/api/hf/search?query=z-image", nil, []int{200}, false},
		{http.MethodGet, "/api/hf/files?modelId=acme/z-image-turbo&revision=main", nil, []int{200}, false},
		{http.MethodGet, "/api/hf/downloads", nil, []int{200}, false},
		{http.MethodGet, "/api/hf/progress?modelId=acme/none&filename=x.gguf", nil, []int{404}, true},
		{http.MethodPost, "/api/hf/cancel", map[string]any{"bundleId": "nope"}, []int{404}, true},
		{http.MethodPost, "/api/hf/cleanup", map[string]any{"filename": "nope.gguf"}, []int{200}, false},
		{http.MethodDelete, "/api/hf/cleanup?filename=nope.gguf", nil, []int{200}, false},
		// Алиасы bundle: UI пробует их по очереди (BUNDLE_DOWNLOAD_PATHS).
		{http.MethodPost, "/api/hf/bundle/download", map[string]any{}, []int{400}, false},
		{http.MethodPost, "/api/image/models/download", map[string]any{}, []int{400}, false},
	}
	for _, tc := range cases {
		code, body := hfDo(t, client, srv.URL, tc.method, tc.path, tc.body, "")
		ok := false
		for _, w := range tc.want {
			if code == w {
				ok = true
			}
		}
		if !ok {
			t.Errorf("%s %s = %d (want %v), body=%s", tc.method, tc.path, code, tc.want, body)
		}
		if !tc.expect404 && code == http.StatusNotFound {
			t.Errorf("%s %s: маршрут не зарегистрирован (404)", tc.method, tc.path)
		}
		if code == http.StatusMethodNotAllowed {
			t.Errorf("%s %s: маршрут не зарегистрирован (405)", tc.method, tc.path)
		}
	}
	// Проверяем, что поиск реально возвращает результаты (а не только 200).
	code, body := hfDo(t, client, srv.URL, http.MethodGet, "/api/hf/search?q=z-image", nil, "")
	if code != http.StatusOK {
		t.Fatalf("search = %d (%s)", code, body)
	}
	var search struct {
		Results []cppbackend.HFModelRepo `json:"results"`
		Count   int                      `json:"count"`
	}
	if err := json.Unmarshal(body, &search); err != nil {
		t.Fatalf("json search: %v (%s)", err, body)
	}
	if search.Count == 0 || len(search.Results) == 0 {
		t.Fatalf("поиск вернул пусто: %s", body)
	}
	if search.Results[0].ID != "acme/z-image-turbo" {
		t.Errorf("results[0].id = %q", search.Results[0].ID)
	}
}

// ============================================================
// 2. GET /api/hf/files — форматы весов и поле format
// ============================================================

func TestHF_FilesListsWeightFormats(t *testing.T) {
	mock := sdbackend.NewHFMockServer(t)
	mock.AddFile("acme/z-image-turbo", "z_image_turbo-Q3_K.gguf", 3_000_000)
	mock.AddFile("acme/z-image-turbo", "vae.safetensors", 300_000)
	mock.AddFile("acme/z-image-turbo", "config.json", 100)
	mock.AddFile("acme/z-image-turbo", "notes.txt", 10)

	app, _ := newHFTestApp(t, mock)
	srv, client := newHFClient(t, app)

	code, body := hfDo(t, client, srv.URL, http.MethodGet,
		"/api/hf/files?modelId=acme/z-image-turbo&revision=main", nil, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", code, body)
	}
	var resp struct {
		Files    []cppbackend.HFFileInfo `json:"files"`
		Count    int                     `json:"count"`
		ModelID  string                  `json:"modelId"`
		Revision string                  `json:"revision"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("json: %v (%s)", err, body)
	}
	if resp.Count != 2 || len(resp.Files) != 2 {
		t.Fatalf("files = %+v, want 2 весовых файла", resp.Files)
	}
	formats := map[string]string{}
	for _, f := range resp.Files {
		formats[f.Path] = f.Format
	}
	if formats["z_image_turbo-Q3_K.gguf"] != "gguf" {
		t.Errorf("format gguf не определён: %+v", resp.Files)
	}
	if formats["vae.safetensors"] != "safetensors" {
		t.Errorf("format safetensors не определён: %+v", resp.Files)
	}
	for _, f := range resp.Files {
		if strings.HasSuffix(f.Path, ".json") || strings.HasSuffix(f.Path, ".txt") {
			t.Errorf("невесовой файл попал в список: %s", f.Path)
		}
	}
	if resp.Revision != "main" || resp.ModelID != "acme/z-image-turbo" {
		t.Errorf("эхо параметров неверно: %+v", resp)
	}

	// R-Image Phase 9: рядом с файлом UI показывает ПРЕДЛОЖЕННУЮ роль, иначе
	// оператор собирает bundle вслепую (diffusion vs vae vs text-encoder).
	// Эвристика серверная (sdbackend.SuggestRole) — правила не должны
	// дублироваться в JS.
	roles := map[string]string{}
	for _, f := range resp.Files {
		roles[f.Path] = f.SuggestedRole
	}
	if roles["vae.safetensors"] != types.ImageFileRoleVae {
		t.Errorf("suggestedRole(vae.safetensors) = %q, want %q", roles["vae.safetensors"], types.ImageFileRoleVae)
	}
	if roles["z_image_turbo-Q3_K.gguf"] != types.ImageFileRoleDiffusion {
		t.Errorf("suggestedRole(веса) = %q, want %q (по умолчанию diffusion)",
			roles["z_image_turbo-Q3_K.gguf"], types.ImageFileRoleDiffusion)
	}
}

// ============================================================
// 3. POST /api/hf/bundle — 202, НЕ блокирует, модель регистрируется
// ============================================================

func TestHF_BundleReturns202AndRegistersModel(t *testing.T) {
	mock := sdbackend.NewHFMockServer(t)
	mock.AddFile("acme/z-image-turbo", "z_image_turbo-Q3_K.gguf", 2_000_000)
	mock.AddFile("acme/z-image-turbo", "vae.safetensors", 1_500_000)
	// Задержка превращает «синхронную загрузку» в измеримую.
	mock.SetFileLatency(1500 * time.Millisecond)

	app, reg := newHFTestApp(t, mock)
	srv, client := newHFClient(t, app)

	body := map[string]any{
		"name":   "z-image-turbo",
		"family": "other",
		"files": []map[string]any{
			{"role": "diffusion", "repo": "acme/z-image-turbo", "filename": "z_image_turbo-Q3_K.gguf", "revision": "main"},
			{"role": "vae", "repo": "acme/z-image-turbo", "filename": "vae.safetensors", "revision": "main"},
		},
	}

	start := time.Now()
	code, respBody := hfDo(t, client, srv.URL, http.MethodPost, "/api/hf/bundle", body, "")
	elapsed := time.Since(start)

	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body=%s", code, respBody)
	}
	// Загрузка занимает >=3 с (две задержки) — ответ обязан вернуться раньше.
	if elapsed > 1200*time.Millisecond {
		t.Fatalf("ответ занял %v: загрузка блокирует хендлер (UI ждёт 15 с и падает)", elapsed)
	}
	var accepted struct {
		Status   string `json:"status"`
		BundleID string `json:"bundleId"`
		Name     string `json:"name"`
		Family   string `json:"family"`
		Files    int    `json:"files"`
	}
	if err := json.Unmarshal(respBody, &accepted); err != nil {
		t.Fatalf("json: %v (%s)", err, respBody)
	}
	if accepted.Status != "started" || accepted.BundleID != "z-image-turbo" || accepted.Files != 2 {
		t.Fatalf("202-ответ = %+v", accepted)
	}

	// Фон доигрывает: модель появляется в реестре (и в GET /api/image/models).
	waitHTTPModelRegistered(t, client, srv.URL, "z-image-turbo")
	p, ok := reg.Profile("z-image-turbo")
	if !ok {
		t.Fatal("профиль не в реестре")
	}
	if len(p.Files) != 2 {
		t.Fatalf("files = %+v", p.Files)
	}
	if err := types.ValidateImageModelProfile(&p); err != nil {
		t.Fatalf("профиль невалиден: %v", err)
	}
}

// ============================================================
// 4. Алиасы bundle-пути (UI пробует их по очереди)
// ============================================================

func TestHF_BundleAliases(t *testing.T) {
	for _, path := range []string{"/api/hf/bundle", "/api/hf/bundle/download", "/api/image/models/download"} {
		mock := sdbackend.NewHFMockServer(t)
		mock.AddFile("acme/z-image-turbo", "diffusion.gguf", 512)
		app, _ := newHFTestApp(t, mock)
		srv, client := newHFClient(t, app)

		body := map[string]any{
			"name": "alias-bundle", "family": "other",
			"files": []map[string]any{
				{"role": "diffusion", "repo": "acme/z-image-turbo", "filename": "diffusion.gguf"},
			},
		}
		code, resp := hfDo(t, client, srv.URL, http.MethodPost, path, body, "")
		if code != http.StatusAccepted {
			t.Errorf("POST %s = %d, want 202 (body=%s)", path, code, resp)
		}
	}
}

// ============================================================
// 5. Прогресс: форма, которую читает UI
// ============================================================

func TestHF_ProgressShapeForUI(t *testing.T) {
	mock := sdbackend.NewHFMockServer(t)
	mock.AddFile("acme/z-image-turbo", "diffusion.gguf", 3*1024*1024)
	release := mock.BlockAfter(768 * 1024)
	defer release()

	app, _ := newHFTestApp(t, mock)
	srv, client := newHFClient(t, app)

	body := map[string]any{
		"name": "ui-progress-bundle", "family": "other",
		"files": []map[string]any{
			{"role": "diffusion", "repo": "acme/z-image-turbo", "filename": "diffusion.gguf"},
		},
	}
	if code, resp := hfDo(t, client, srv.URL, http.MethodPost, "/api/hf/bundle", body, ""); code != http.StatusAccepted {
		t.Fatalf("bundle = %d (%s)", code, resp)
	}

	path := "/api/hf/progress?modelId=" + "acme/z-image-turbo" + "&filename=diffusion.gguf"
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		code, raw := hfDo(t, client, srv.URL, http.MethodGet, path, nil, "")
		if code != http.StatusOK {
			time.Sleep(25 * time.Millisecond)
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("json: %v (%s)", err, raw)
		}
		downloaded, _ := payload["downloaded"].(float64)
		if downloaded <= 0 {
			time.Sleep(25 * time.Millisecond)
			continue
		}
		// Поля, которые UI читает буквально (image-page.js:409-437).
		for _, key := range []string{"status", "totalBytes", "downloaded", "progressPct"} {
			if _, ok := payload[key]; !ok {
				t.Fatalf("в прогрессе нет ключа %q: %s", key, raw)
			}
		}
		if payload["status"] != cppbackend.BundleFileStatusDownloading {
			t.Fatalf("status = %v, want downloading", payload["status"])
		}
		if payload["totalBytes"].(float64) != float64(3*1024*1024) {
			t.Fatalf("totalBytes = %v", payload["totalBytes"])
		}
		if pct := payload["progressPct"].(float64); pct <= 0 || pct > 100 {
			t.Fatalf("progressPct = %v, want (0,100]", pct)
		}
		// speedBps присутствует (UI показывает скорость) — значение может быть 0.
		if _, ok := payload["speedBps"]; !ok {
			t.Fatalf("в прогрессе нет ключа speedBps: %s", raw)
		}

		// Агрегат по bundleId — вторая поддерживаемая форма.
		code, rawBundle := hfDo(t, client, srv.URL, http.MethodGet,
			"/api/hf/progress?bundleId=ui-progress-bundle", nil, "")
		if code != http.StatusOK {
			t.Fatalf("?bundleId= = %d (%s)", code, rawBundle)
		}
		var bundlePayload map[string]any
		if err := json.Unmarshal(rawBundle, &bundlePayload); err != nil {
			t.Fatalf("json bundle: %v", err)
		}
		for _, key := range []string{"bundleId", "status", "totalBytes", "downloaded", "files"} {
			if _, ok := bundlePayload[key]; !ok {
				t.Fatalf("в агрегате нет ключа %q: %s", key, rawBundle)
			}
		}
		return
	}
	t.Fatal("прогресс файла не дошёл до состояния downloading")
}

// ============================================================
// 6. X-HF-Token доходит до загрузчика (Authorization: Bearer)
// ============================================================

func TestHF_TokenHeaderReachesDownloader(t *testing.T) {
	mock := sdbackend.NewHFMockServer(t)
	mock.AddFile("acme/gated-model", "diffusion.gguf", 128*1024)

	app, _ := newHFTestApp(t, mock)
	srv, client := newHFClient(t, app)

	body := map[string]any{
		"name": "gated-bundle", "family": "other",
		"files": []map[string]any{
			{"role": "diffusion", "repo": "acme/gated-model", "filename": "diffusion.gguf"},
		},
	}
	code, resp := hfDo(t, client, srv.URL, http.MethodPost, "/api/hf/bundle", body, "hf_ui_token")
	if code != http.StatusAccepted {
		t.Fatalf("bundle = %d (%s)", code, resp)
	}
	waitHTTPModelRegistered(t, client, srv.URL, "gated-bundle")

	if got := mock.LastAuthHeader(); got != "Bearer hf_ui_token" {
		t.Fatalf("Authorization = %q, want Bearer hf_ui_token", got)
	}
}

// ============================================================
// 7. Валидация тела: 400 и синхронный ответ (в фоне ничего не стартует)
// ============================================================

func TestHF_BundleValidation(t *testing.T) {
	mock := sdbackend.NewHFMockServer(t)
	app, _ := newHFTestApp(t, mock)
	srv, client := newHFClient(t, app)

	cases := []struct {
		name string
		body any
	}{
		{"пустое тело", map[string]any{}},
		{"нет имени", map[string]any{"files": []map[string]any{{"role": "diffusion", "repo": "a/b", "filename": "x.gguf"}}}},
		{"нет diffusion", map[string]any{"name": "m", "files": []map[string]any{{"role": "vae", "repo": "a/b", "filename": "v.gguf"}}}},
		{"неизвестная роль", map[string]any{"name": "m", "files": []map[string]any{{"role": "banana", "repo": "a/b", "filename": "x.gguf"}}}},
		{"неизвестное семейство", map[string]any{"name": "m", "family": "banana", "files": []map[string]any{{"role": "diffusion", "repo": "a/b", "filename": "x.gguf"}}}},
		{"не весовой файл", map[string]any{"name": "m", "files": []map[string]any{{"role": "diffusion", "repo": "a/b", "filename": "README.md"}}}},
		{"имя с separator", map[string]any{"name": "a/b", "files": []map[string]any{{"role": "diffusion", "repo": "a/b", "filename": "x.gguf"}}}},
		{"пустой repo", map[string]any{"name": "m", "files": []map[string]any{{"role": "diffusion", "repo": "", "filename": "x.gguf"}}}},
	}
	for _, tc := range cases {
		code, body := hfDo(t, client, srv.URL, http.MethodPost, "/api/hf/bundle", tc.body, "")
		if code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body=%s)", tc.name, code, body)
		}
	}

	// Метод не тот → 405.
	if code, _ := hfDo(t, client, srv.URL, http.MethodGet, "/api/hf/bundle", nil, ""); code != http.StatusMethodNotAllowed {
		t.Errorf("GET bundle = %d, want 405", code)
	}
	// Одиночная загрузка без modelId → 400.
	if code, _ := hfDo(t, client, srv.URL, http.MethodPost, "/api/hf/download", map[string]any{"filename": "x.gguf"}, ""); code != http.StatusBadRequest {
		t.Errorf("download без modelId = %d, want 400", code)
	}
}

// ============================================================
// 8. GET /api/hf/downloads: активные + история + orphans + bundles
// ============================================================

func TestHF_DownloadsEndpointShape(t *testing.T) {
	mock := sdbackend.NewHFMockServer(t)
	app, _ := newHFTestApp(t, mock)
	srv, client := newHFClient(t, app)

	code, body := hfDo(t, client, srv.URL, http.MethodGet, "/api/hf/downloads", nil, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	var resp struct {
		Active        []cppbackend.HFDownloadProgress `json:"active"`
		History       []cppbackend.HFDownloadProgress `json:"history"`
		Orphans       []cppbackend.OrphanDownloadFile `json:"orphans"`
		Bundles       []cppbackend.HFBundleProgress   `json:"bundles"`
		BundleHistory []cppbackend.HFBundleProgress   `json:"bundleHistory"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("json: %v (%s)", err, body)
	}
	// Пустые массивы, а не null: UI итерирует их без проверок.
	raw := string(body)
	for _, key := range []string{`"active":[]`, `"history":[]`, `"orphans":[]`, `"bundles":[]`, `"bundleHistory":[]`} {
		if !strings.Contains(raw, key) {
			t.Errorf("ожидался %s в ответе: %s", key, raw)
		}
	}
}

// ============================================================
// 9. Без HF-слоя (сервис собран вручную) — 503, а не паника
// ============================================================

func TestHF_UnavailableWithoutManager(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{"sd15": newTestProfile("sd15", "sd15")})
	router := app.setupRouter()

	rec := doJSON(t, router, http.MethodGet, "/api/hf/downloads", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "HF downloader") {
		t.Fatalf("неинформативная ошибка: %s", rec.Body.String())
	}
}

// ============================================================
// 10. BOM в теле запроса не ломает декодирование
// ============================================================

func TestHF_BOMBodyAccepted(t *testing.T) {
	mock := sdbackend.NewHFMockServer(t)
	mock.AddFile("acme/z-image-turbo", "diffusion.gguf", 512)
	app, _ := newHFTestApp(t, mock)
	router := app.setupRouter()

	payload := []byte(`{"name":"bom-bundle","family":"other","files":[{"role":"diffusion","repo":"acme/z-image-turbo","filename":"diffusion.gguf"}]}`)
	withBOM := append([]byte{0xEF, 0xBB, 0xBF}, payload...)
	req := httptest.NewRequest(http.MethodPost, "/api/hf/bundle", bytes.NewReader(withBOM))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("BOM-тело: status = %d (body=%s), want 202", rec.Code, rec.Body.String())
	}
}
