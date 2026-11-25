// handlers_model_files_test.go — R751 (2026-11-25): HTTP-контракт управления
// отдельными файлами скачанной image-группы.
//
//	POST   /api/image/models/{name}/files   — 202 + trackingId
//	DELETE /api/image/models/{name}/files?filename=... — 200
//
// ЧТО ПРОВЕРЯЕТСЯ:
//  1. GET /api/image/models отдаёт по каждому файлу present/diskSizeBytes, а не
//     только запись профиля;
//  2. докачка отсутствующего файла в группу: 202, прогресс существующей ручкой,
//     файл на диске, роль в профиле, вердикт пересчитан;
//  3. перекачка diffusion новым квантом (replace=true): старый файл удалён, в
//     профиле ровно одна роль diffusion с новым именем;
//  4. удаление файла: 200 + bytesFreed + removedRoles, запись профиля ушла,
//     вердикт снова «не хватает роли», повтор — noop;
//  5. отказы кодами: неизвестная роль, не-весовой файл, path traversal,
//     несуществующая группа, неверный метод.
package main

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"ollama-loadbalancer/internal/sdbackend"
	"ollama-loadbalancer/pkg/types"
)

// fileEntryOf — запись файла из GET /api/image/models по имени файла.
func fileEntryOf(t *testing.T, info map[string]any, filename string) map[string]any {
	t.Helper()
	files, _ := info["files"].([]any)
	for _, item := range files {
		f, _ := item.(map[string]any)
		if f != nil && f["filename"] == filename {
			return f
		}
	}
	return nil
}

// waitFileProgressCompleted — дождаться терминального статуса загрузки файла.
func waitFileProgressCompleted(t *testing.T, client *http.Client, base, tracking string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		code, body := hfDo(t, client, base, http.MethodGet, "/api/hf/progress?bundleId="+tracking, nil, "")
		if code != http.StatusOK {
			t.Fatalf("progress: %d (%s)", code, string(body))
		}
		p := decodeMap(t, body)
		switch p["status"] {
		case "completed":
			return
		case "failed":
			t.Fatalf("загрузка файла провалилась: %s", string(body))
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("прогресс %q не дошёл до completed за 30 с", tracking)
}

func TestModelFilesHTTP_InventoryInstallReplaceDelete_R751(t *testing.T) {
	mock := sdbackend.NewHFMockServer(t)
	const repo = "acme/qwen-image"
	mock.AddFile(repo, "vae/qwen_image_vae.safetensors", 8*1024)
	mock.AddFile(repo, "qwen-edit-2511-Q8_0.gguf", 16*1024)
	app, modelsDir := newComponentTestApp(t, mock, "qwen-edit-2511")
	srv, client := newHFClient(t, app)

	// 1. Инвентарь: у diffusion есть фактическое состояние на диске.
	info := modelInfoFrom(t, client, srv.URL, "qwen-edit-2511")
	diff := fileEntryOf(t, info, "qwen-edit-2511-Q4_K_M.gguf")
	if diff == nil {
		t.Fatalf("в files нет diffusion: %s", mustJSON(t, info))
	}
	if present, ok := diff["present"].(bool); !ok || !present {
		t.Fatalf("diffusion.present = %v, want true: %s", diff["present"], mustJSON(t, diff))
	}
	if size, _ := diff["diskSizeBytes"].(float64); size <= 0 {
		t.Fatalf("diffusion.diskSizeBytes = %v, want > 0", diff["diskSizeBytes"])
	}

	// 2. Докачка отсутствующего VAE в существующую группу.
	code, body := hfDo(t, client, srv.URL, http.MethodPost,
		"/api/image/models/qwen-edit-2511/files", map[string]any{
			"role":     "vae",
			"repo":     repo,
			"filename": "vae/qwen_image_vae.safetensors",
		}, "")
	if code != http.StatusAccepted {
		t.Fatalf("POST files: %d (%s)", code, string(body))
	}
	install := decodeMap(t, body)
	tracking, _ := install["trackingId"].(string)
	if tracking == "" {
		t.Fatalf("нет trackingId: %s", string(body))
	}
	if install["filename"] != "qwen_image_vae.safetensors" {
		t.Fatalf("filename = %v, want basename", install["filename"])
	}
	waitFileProgressCompleted(t, client, srv.URL, tracking)

	local := filepath.Join(modelsDir, "qwen-edit-2511", "qwen_image_vae.safetensors")
	if fi, err := os.Stat(local); err != nil || fi.Size() != 8*1024 {
		t.Fatalf("файл VAE не скачан: %v", err)
	}
	missing := missingOf(compatOf(t, client, srv.URL, "qwen-edit-2511"))
	if len(missing) != 1 || missing[0] != sdbackend.CompatRoleLabel(types.ImageFileRoleLLM) {
		t.Fatalf("missing после докачки VAE = %v, want [text encoder]", missing)
	}
	info = modelInfoFrom(t, client, srv.URL, "qwen-edit-2511")
	vae := fileEntryOf(t, info, "qwen_image_vae.safetensors")
	if vae == nil || vae["role"] != types.ImageFileRoleVae {
		t.Fatalf("VAE не попал в files: %s", mustJSON(t, info))
	}
	if present, _ := vae["present"].(bool); !present {
		t.Fatalf("VAE.present = %v, want true", vae["present"])
	}

	// 3. Перекачка diffusion новым квантом: replace=true удаляет старый файл.
	oldDiffusion := filepath.Join(modelsDir, "qwen-edit-2511", "qwen-edit-2511-Q4_K_M.gguf")
	code, body = hfDo(t, client, srv.URL, http.MethodPost,
		"/api/image/models/qwen-edit-2511/files", map[string]any{
			"role":     "diffusion",
			"repo":     repo,
			"filename": "qwen-edit-2511-Q8_0.gguf",
			"replace":  true,
		}, "")
	if code != http.StatusAccepted {
		t.Fatalf("replace diffusion: %d (%s)", code, string(body))
	}
	replaced := decodeMap(t, body)
	waitFileProgressCompleted(t, client, srv.URL, replaced["trackingId"].(string))
	if _, err := os.Stat(oldDiffusion); !os.IsNotExist(err) {
		t.Fatalf("старый diffusion не удалён после replace: %v", err)
	}
	newDiffusion := filepath.Join(modelsDir, "qwen-edit-2511", "qwen-edit-2511-Q8_0.gguf")
	if fi, err := os.Stat(newDiffusion); err != nil || fi.Size() != 16*1024 {
		t.Fatalf("новый diffusion не скачан: %v", err)
	}
	info = modelInfoFrom(t, client, srv.URL, "qwen-edit-2511")
	var diffusionNames []string
	files, _ := info["files"].([]any)
	for _, item := range files {
		f, _ := item.(map[string]any)
		if f != nil && f["role"] == types.ImageFileRoleDiffusion {
			if present, _ := f["present"].(bool); present {
				diffusionNames = append(diffusionNames, f["filename"].(string))
			}
		}
	}
	sort.Strings(diffusionNames)
	if len(diffusionNames) != 1 || diffusionNames[0] != "qwen-edit-2511-Q8_0.gguf" {
		t.Fatalf("роль diffusion после replace = %v, want новый файл", diffusionNames)
	}

	// 4. Удаление файла: файл + запись профиля, вердикт пересчитан.
	code, body = hfDo(t, client, srv.URL, http.MethodDelete,
		"/api/image/models/qwen-edit-2511/files?filename=qwen_image_vae.safetensors", nil, "")
	if code != http.StatusOK {
		t.Fatalf("DELETE files: %d (%s)", code, string(body))
	}
	del := decodeMap(t, body)
	if del["status"] != "deleted" || del["profileChanged"] != true {
		t.Fatalf("результат удаления: %s", string(body))
	}
	if freed, _ := del["bytesFreed"].(float64); freed != 8*1024 {
		t.Fatalf("bytesFreed = %v, want %d", del["bytesFreed"], 8*1024)
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatalf("файл VAE не удалён: %v", err)
	}
	missing = missingOf(compatOf(t, client, srv.URL, "qwen-edit-2511"))
	foundVaeMissing := false
	for _, m := range missing {
		if m == sdbackend.CompatRoleLabel(types.ImageFileRoleVae) {
			foundVaeMissing = true
		}
	}
	if !foundVaeMissing {
		t.Fatalf("после удаления VAE вердикт не видит нехватку: %v", missing)
	}

	// Повтор — noop, а не 404 (двойной клик не должен пугать).
	code, body = hfDo(t, client, srv.URL, http.MethodDelete,
		"/api/image/models/qwen-edit-2511/files?filename=qwen_image_vae.safetensors", nil, "")
	if code != http.StatusOK || decodeMap(t, body)["status"] != "noop" {
		t.Fatalf("повторное удаление: %d (%s)", code, string(body))
	}

	// 5. Отказы кодами.
	cases := []struct {
		name   string
		method string
		path   string
		body   map[string]any
		want   int
		code   string
	}{
		{"unknown role", http.MethodPost, "/api/image/models/qwen-edit-2511/files",
			map[string]any{"role": "bogus", "repo": repo, "filename": "vae/qwen_image_vae.safetensors"},
			http.StatusBadRequest, sdbackend.CompErrRoleInvalid},
		{"missing repo", http.MethodPost, "/api/image/models/qwen-edit-2511/files",
			map[string]any{"role": "vae", "filename": "vae/qwen_image_vae.safetensors"},
			http.StatusBadRequest, sdbackend.CompErrRepoRequired},
		{"unknown bundle", http.MethodPost, "/api/image/models/no-such-bundle/files",
			map[string]any{"role": "vae", "repo": repo, "filename": "vae/qwen_image_vae.safetensors"},
			http.StatusNotFound, sdbackend.CompErrBundleNotFound},
		{"delete profile", http.MethodDelete, "/api/image/models/qwen-edit-2511/files?filename=profile.json",
			nil, http.StatusBadRequest, sdbackend.ModelFileErrInvalidName},
		{"delete traversal", http.MethodDelete, "/api/image/models/qwen-edit-2511/files?filename=../evil.gguf",
			nil, http.StatusBadRequest, sdbackend.ModelFileErrInvalidName},
	}
	for _, tc := range cases {
		code, body := hfDo(t, client, srv.URL, tc.method, tc.path, tc.body, "")
		if code != tc.want {
			t.Fatalf("%s: status = %d, want %d (%s)", tc.name, code, tc.want, string(body))
		}
		if got := decodeMap(t, body)["code"]; got != tc.code {
			t.Fatalf("%s: code = %v, want %q (%s)", tc.name, got, tc.code, string(body))
		}
	}

	code, _ = hfDo(t, client, srv.URL, http.MethodGet, "/api/image/models/qwen-edit-2511/files", nil, "")
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("GET files: status = %d, want 405", code)
	}
}
