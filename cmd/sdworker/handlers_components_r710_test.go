// handlers_components_r710_test.go — R710 (2026-11-24): HTTP-контракт добавления
// недостающих компонентов image-набора.
//
// ЖИВОЙ ДЕФЕКТ (скриншоты пользователя): вердикт говорит «не хватает VAE, text
// encoder (LLM)», в репозитории этих файлов нет, а добавить их из UI нельзя.
// Здесь проверяется HTTP-половина решения:
//
//	GET  /api/image/components/catalog            — каталог (default + size);
//	POST /api/image/models/{name}/components      — 202 + trackingId, затем
//	     прогресс существующей ручкой /api/hf/progress и ПЕРЕСЧЁТ вердикта в
//	     GET /api/image/models;
//	отказы: 409 role_already_present (+currentFile), 400 invalid_path,
//	        404 на неизвестный подпуть.
//
// ПОЧЕМУ HTTP, А НЕ ТОЛЬКО ПАКЕТНЫЕ ТЕСТЫ: контракт (пути, коды, имена полей)
// снят с UI, а не с Go-структур; расхождение пути/имени поля уже один раз
// сделало страницу «Изображения» нерабочей (см. шапку handlers_hf.go).
package main

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ollama-loadbalancer/internal/sdbackend"
	"ollama-loadbalancer/pkg/types"
)

// newComponentTestApp — приложение с HF-моком и ОДНИМ скачанным bundle
// (diffusion без VAE/энкодера: ровно состояние с скриншотов).
//
// bundle создаётся на диске вручную: profile.json с family=qwen_image и одной
// ролью diffusion. Заголовок .gguf при этом не разбирается (это не настоящие
// веса) — вердикту достаточно ролей профиля, а проверяем мы добавление
// компонента, а не пробу заголовков (она покрыта compat_test.go).
func newComponentTestApp(t *testing.T, mock *sdbackend.HFMockServer, bundle string) (*App, string) {
	t.Helper()
	app, reg := newHFTestApp(t, mock)
	modelsDir := reg.ModelsDir()
	dir := filepath.Join(modelsDir, bundle)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir bundle: %v", err)
	}
	diffusion := []byte("\x00diffusion-weights-for-r710")
	if err := os.WriteFile(filepath.Join(dir, bundle+"-Q4_K_M.gguf"), diffusion, 0o644); err != nil {
		t.Fatalf("write diffusion: %v", err)
	}
	profile := types.ImageModelProfile{
		Name:   bundle,
		Family: "qwen_image",
		Files: []types.ImageModelFile{{
			Role: types.ImageFileRoleDiffusion, Repo: "acme/qwen-image",
			Filename: bundle + "-Q4_K_M.gguf", Revision: "main",
			LocalPath: filepath.Join(dir, bundle+"-Q4_K_M.gguf"), SizeBytes: int64(len(diffusion)),
		}},
		Defaults: types.DefaultImageGenDefaults("qwen_image"),
	}
	raw, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		t.Fatalf("marshal profile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, sdbackend.ProfileFileName), raw, 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	if err := reg.Load(); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	return app, modelsDir
}

// decodeMap — JSON-объект ответа.
func decodeMap(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("ответ не JSON: %v (%s)", err, string(body))
	}
	return out
}

// modelInfoFrom — запись модели из ответа GET /api/image/models.
func modelInfoFrom(t *testing.T, client *http.Client, base, name string) map[string]any {
	t.Helper()
	code, body := hfDo(t, client, base, http.MethodGet, "/api/image/models", nil, "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/image/models: %d (%s)", code, string(body))
	}
	payload := decodeMap(t, body)
	list, _ := payload["models"].([]any)
	for _, item := range list {
		m, _ := item.(map[string]any)
		if m != nil && m["name"] == name {
			return m
		}
	}
	t.Fatalf("модель %q не в списке: %s", name, string(body))
	return nil
}

// compatOf — вердикт модели из ответа списка.
func compatOf(t *testing.T, client *http.Client, base, name string) map[string]any {
	t.Helper()
	m := modelInfoFrom(t, client, base, name)
	compat, _ := m["compat"].(map[string]any)
	if compat == nil {
		t.Fatalf("у модели %q нет вердикта compat: %+v", name, m)
	}
	return compat
}

// missingOf — подписи недостающих ролей из вердикта.
func missingOf(compat map[string]any) []string {
	raw, _ := compat["missing"].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// ============================================================
// (а) Каталог по HTTP: default + size
// ============================================================

func TestComponents_CatalogHTTP_R710(t *testing.T) {
	mock := sdbackend.NewHFMockServer(t)
	app, _ := newComponentTestApp(t, mock, "qwen-edit-2511")
	srv, client := newHFClient(t, app)

	code, body := hfDo(t, client, srv.URL, http.MethodGet,
		"/api/image/components/catalog?family=qwen_image&roles=vae,llm", nil, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d (%s)", code, string(body))
	}
	payload := decodeMap(t, body)
	if payload["family"] != "qwen_image" {
		t.Fatalf("family = %v", payload["family"])
	}
	if payload["known"] != true {
		t.Fatalf("known = %v, want true для qwen_image", payload["known"])
	}
	list, _ := payload["components"].([]any)
	if len(list) == 0 {
		t.Fatalf("каталог пуст: %s", string(body))
	}
	// Первым идёт default-компонент, и у него есть репозиторий, файлы и размер.
	first, _ := list[0].(map[string]any)
	if first["default"] != true {
		t.Fatalf("первый компонент не default: %+v", first)
	}
	if first["role"] != "vae" {
		t.Fatalf("role = %v, want vae (VAE идёт первой ролью)", first["role"])
	}
	if repo, _ := first["repo"].(string); repo == "" {
		t.Fatalf("repo пуст: %+v", first)
	}
	if files, _ := first["files"].([]any); len(files) == 0 {
		t.Fatalf("files пуст: %+v", first)
	}
	if size, _ := first["sizeMb"].(float64); size <= 0 {
		t.Fatalf("sizeMb = %v, want > 0 (UI показывает объём в подтверждении)", first["sizeMb"])
	}
	// maxFiles/maxSizeMb — лимиты для подтверждения.
	if _, ok := payload["maxSizeMb"]; !ok {
		t.Fatalf("в ответе нет maxSizeMb: %s", string(body))
	}

	// Неизвестное семейство: known=false и ПУСТОЙ МАССИВ (не null) — UI по этому
	// решает не показывать кнопки «по умолчанию».
	code, body = hfDo(t, client, srv.URL, http.MethodGet,
		"/api/image/components/catalog?family=wan", nil, "")
	if code != http.StatusOK {
		t.Fatalf("wan: status = %d (%s)", code, string(body))
	}
	unknown := decodeMap(t, body)
	if unknown["known"] != false {
		t.Fatalf("wan: known = %v, want false", unknown["known"])
	}
	if comps, ok := unknown["components"].([]any); !ok || len(comps) != 0 {
		t.Fatalf("wan: components = %v, want []", unknown["components"])
	}

	// Неизвестная роль — 400 с кодом (не «тихо пустой каталог»).
	code, body = hfDo(t, client, srv.URL, http.MethodGet,
		"/api/image/components/catalog?family=qwen_image&roles=diffusion", nil, "")
	if code != http.StatusBadRequest {
		t.Fatalf("roles=diffusion: status = %d, want 400 (%s)", code, string(body))
	}
}

// ============================================================
// (б) POST: роль прописана, вердикт пересчитан, диффузия не тронута
// ============================================================

func TestComponents_PostAddsRoleHTTP_R710(t *testing.T) {
	mock := sdbackend.NewHFMockServer(t)
	mock.AddFile("acme/qwen-image", "split_files/vae/qwen_image_vae.safetensors", 64*1024)
	app, modelsDir := newComponentTestApp(t, mock, "qwen-edit-2511")
	srv, client := newHFClient(t, app)

	diffusionPath := filepath.Join(modelsDir, "qwen-edit-2511", "qwen-edit-2511-Q4_K_M.gguf")
	before, err := os.ReadFile(diffusionPath)
	if err != nil {
		t.Fatalf("read diffusion: %v", err)
	}
	sumBefore := sha256.Sum256(before)

	// Подготовка: вердикт ДО — не хватает VAE и энкодера (ровно как на скриншоте).
	compatBefore := compatOf(t, client, srv.URL, "qwen-edit-2511")
	if compatBefore["verdict"] != sdbackend.CompatIncomplete {
		t.Fatalf("вердикт до = %v, want %q", compatBefore["verdict"], sdbackend.CompatIncomplete)
	}
	missingBefore := missingOf(compatBefore)
	if len(missingBefore) != 2 {
		t.Fatalf("missing до = %v, want 2 роли (VAE, text encoder)", missingBefore)
	}

	code, body := hfDo(t, client, srv.URL, http.MethodPost,
		"/api/image/models/qwen-edit-2511/components", map[string]any{
			"role":    "vae",
			"repo":    "acme/qwen-image",
			"files":   []string{"split_files/vae/qwen_image_vae.safetensors"},
			"default": true,
		}, "")
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", code, string(body))
	}
	install := decodeMap(t, body)
	tracking, _ := install["trackingId"].(string)
	if tracking == "" {
		t.Fatalf("нет trackingId: %s", string(body))
	}
	if url, _ := install["progressUrl"].(string); url == "" {
		t.Fatalf("нет progressUrl: %s", string(body))
	}
	// Ожидаемый объём: UI показывает его в подтверждении до запуска.
	if totalMb, _ := install["totalMb"].(float64); totalMb <= 0 {
		t.Fatalf("totalMb = %v, want > 0", install["totalMb"])
	}

	// Прогресс — существующей ручкой (никаких новых эндпоинтов для UI).
	deadline := time.Now().Add(30 * time.Second)
	completed := false
	for time.Now().Before(deadline) {
		code, body = hfDo(t, client, srv.URL, http.MethodGet,
			"/api/hf/progress?bundleId="+tracking, nil, "")
		if code != http.StatusOK {
			t.Fatalf("progress: status = %d (%s)", code, string(body))
		}
		p := decodeMap(t, body)
		if p["status"] == "completed" {
			completed = true
			break
		}
		if p["status"] == "failed" {
			t.Fatalf("загрузка компонента провалилась: %s", string(body))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !completed {
		t.Fatal("прогресс не дошёл до completed за 30 с")
	}

	// Вердикт ПЕРЕСЧИТАН: VAE больше не в missing, следующим шагом — энкодер.
	missingAfter := missingOf(compatOf(t, client, srv.URL, "qwen-edit-2511"))
	if len(missingAfter) != 1 {
		t.Fatalf("missing после = %v, want ровно один (text encoder)", missingAfter)
	}
	if missingAfter[0] != sdbackend.CompatRoleLabel(types.ImageFileRoleLLM) {
		t.Fatalf("следующий шаг = %q, want %q", missingAfter[0],
			sdbackend.CompatRoleLabel(types.ImageFileRoleLLM))
	}

	// Роль видна в списке файлов модели.
	info := modelInfoFrom(t, client, srv.URL, "qwen-edit-2511")
	files, _ := info["files"].([]any)
	hasVae := false
	for _, item := range files {
		f, _ := item.(map[string]any)
		if f != nil && f["role"] == types.ImageFileRoleVae {
			hasVae = true
			if fn, _ := f["filename"].(string); fn != "qwen_image_vae.safetensors" {
				t.Fatalf("файл VAE назван %q, want basename", fn)
			}
		}
	}
	if !hasVae {
		t.Fatalf("роль vae не попала в файлы модели: %s", mustJSON(t, files))
	}

	// ДИФФУЗИЯ НЕ ТРОНУТА: содержимое совпадает побайтно.
	afterData, err := os.ReadFile(diffusionPath)
	if err != nil {
		t.Fatalf("diffusion пропал: %v", err)
	}
	if sha256.Sum256(afterData) != sumBefore {
		t.Fatal("содержимое diffusion-файла изменилось после добавления компонента")
	}
}

// ============================================================
// (г) 409 role_already_present (+ currentFile)
// ============================================================

func TestComponents_PostRoleAlreadyPresentHTTP_R710(t *testing.T) {
	mock := sdbackend.NewHFMockServer(t)
	mock.AddFile("acme/qwen-image", "ae.safetensors", 1024)
	app, modelsDir := newComponentTestApp(t, mock, "qwen-edit-2511")
	srv, client := newHFClient(t, app)

	// Дописываем VAE в профиль так, как это сделала бы первая установка.
	dir := filepath.Join(modelsDir, "qwen-edit-2511")
	if err := os.WriteFile(filepath.Join(dir, "existing_vae.safetensors"), make([]byte, 1024), 0o644); err != nil {
		t.Fatalf("write vae: %v", err)
	}
	profilePath := filepath.Join(dir, sdbackend.ProfileFileName)
	raw, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	var profile types.ImageModelProfile
	if err := json.Unmarshal(raw, &profile); err != nil {
		t.Fatalf("parse profile: %v", err)
	}
	profile.Files = append(profile.Files, types.ImageModelFile{
		Role: types.ImageFileRoleVae, Repo: "acme/qwen-image", Filename: "existing_vae.safetensors",
		LocalPath: filepath.Join(dir, "existing_vae.safetensors"), SizeBytes: 1024,
	})
	raw, err = json.MarshalIndent(profile, "", "  ")
	if err != nil {
		t.Fatalf("marshal profile: %v", err)
	}
	if err := os.WriteFile(profilePath, raw, 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}

	code, body := hfDo(t, client, srv.URL, http.MethodPost,
		"/api/image/models/qwen-edit-2511/components", map[string]any{
			"role":  "vae",
			"repo":  "acme/qwen-image",
			"files": []string{"ae.safetensors"},
		}, "")
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", code, string(body))
	}
	payload := decodeMap(t, body)
	if payload["code"] != sdbackend.CompErrRolePresent {
		t.Fatalf("code = %v, want %q", payload["code"], sdbackend.CompErrRolePresent)
	}
	if payload["currentFile"] != "existing_vae.safetensors" {
		t.Fatalf("currentFile = %v, want existing_vae.safetensors", payload["currentFile"])
	}
}

// ============================================================
// (д) Путь вне каталога моделей — 400 invalid_path
// ============================================================

func TestComponents_PostPathOutsideHTTP_R710(t *testing.T) {
	mock := sdbackend.NewHFMockServer(t)
	mock.AddFile("acme/qwen-image", "ae.safetensors", 1024)
	app, modelsDir := newComponentTestApp(t, mock, "qwen-edit-2511")
	srv, client := newHFClient(t, app)

	outside := filepath.Join(filepath.Dir(modelsDir), "evil")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	code, body := hfDo(t, client, srv.URL, http.MethodPost,
		"/api/image/models/qwen-edit-2511/components", map[string]any{
			"role":     "vae",
			"repo":     "acme/qwen-image",
			"files":    []string{"ae.safetensors"},
			"modelDir": outside,
		}, "")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", code, string(body))
	}
	if payload := decodeMap(t, body); payload["code"] != sdbackend.CompErrInvalidPath {
		t.Fatalf("code = %v, want %q", payload["code"], sdbackend.CompErrInvalidPath)
	}
}

// TestComponents_UnknownSubpath404_R710 — префиксный маршрут не должен
// «отвечать 200 на всё»: чужие подпути — 404, а точные ручки не перехвачены.
func TestComponents_UnknownSubpath404_R710(t *testing.T) {
	mock := sdbackend.NewHFMockServer(t)
	app, _ := newComponentTestApp(t, mock, "qwen-edit-2511")
	srv, client := newHFClient(t, app)

	code, body := hfDo(t, client, srv.URL, http.MethodPost,
		"/api/image/models/qwen-edit-2511/unknown", map[string]any{}, "")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", code, string(body))
	}
	if payload := decodeMap(t, body); payload["code"] != "unknown_model_path" {
		t.Fatalf("code = %v, want unknown_model_path", payload["code"])
	}
	// Существующая точная ручка (/api/image/models/load) не перехвачена префиксом.
	code, _ = hfDo(t, client, srv.URL, http.MethodGet, "/api/image/models/load", nil, "")
	if code == http.StatusNotFound {
		t.Fatal("точный маршрут /api/image/models/load перехвачен префиксным")
	}
}

// mustJSON — компактный JSON для сообщений об ошибках теста.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		return "<marshal error>"
	}
	return string(raw)
}
