package api

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// imageProfilesPathForTest — изолированный файл профилей для одного теста.
//
// Кэш хранилищ в handlers_image_profiles.go ключуется ПУТЁМ, поэтому уникальный
// временный файл = изолированное состояние (важно: profileStore — не поле Server).
func imageProfilesPathForTest(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "image-model-profiles.json")
	t.Setenv("LB_IMAGE_MODEL_PROFILES_PATH", path)
	return path
}

// sd15ProfileBody — минимальное валидное тело: только family+files. Остальное
// (steps/cfg/size/batch/seed) балансер обязан вывести из family — иначе
// оператору пришлось бы вручную прописывать 10 полей на каждую модель.
const sd15ProfileBody = `{
  "family": "sd15",
  "files": [
    {"role": "diffusion", "repo": "second-state/stable-diffusion-v1-5-GGUF",
     "filename": "stable-diffusion-v1-5-pruned-emaonly-Q8_0.gguf", "sizeBytes": 1763578176}
  ]
}`

func putImageProfile(t *testing.T, baseURL, name, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut,
		baseURL+"/api/v1/image/model-profiles/"+name, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func decodeProfileResponse(t *testing.T, resp *http.Response) types.ImageModelProfile {
	t.Helper()
	defer resp.Body.Close()
	var payload struct {
		Status  string                  `json:"status"`
		Model   string                  `json:"model"`
		Profile types.ImageModelProfile `json:"profile"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	return payload.Profile
}

// TestImageProfile_PutAppliesFamilyDefaults — PUT с одними files: дефолты
// берутся из types.DefaultImageGenDefaults(family), профиль персистится.
func TestImageProfile_PutAppliesFamilyDefaults(t *testing.T) {
	path := imageProfilesPathForTest(t)
	server, _, _ := createTestServer(t)
	defer server.Close()

	resp := putImageProfile(t, server.URL, "sd15-demo", sd15ProfileBody)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	profile := decodeProfileResponse(t, resp)

	assert.Equal(t, "sd15-demo", profile.Name)
	assert.Equal(t, "sd15", profile.Family)

	want := types.DefaultImageGenDefaults("sd15")
	assert.Equal(t, want.Steps, profile.Defaults.Steps)
	assert.Equal(t, want.Width, profile.Defaults.Width)
	assert.Equal(t, want.Height, profile.Defaults.Height)
	assert.Equal(t, want.Sampler, profile.Defaults.Sampler)
	assert.Equal(t, 1, profile.Defaults.BatchCount)
	assert.Equal(t, int64(-1), profile.Defaults.Seed, "новый профиль без seed → random (-1)")
	assert.Equal(t, "random", profile.Runtime.SeedMode)

	// Персистенция: файл на диске + переживание «рестарта» (GET читает store).
	require.FileExists(t, path)
	respGet, err := http.Get(server.URL + "/api/v1/image/model-profiles/sd15-demo")
	require.NoError(t, err)
	defer respGet.Body.Close()
	require.Equal(t, http.StatusOK, respGet.StatusCode)

	var payload struct {
		Profile    types.ImageModelProfile `json:"profile"`
		ServerArgs []string                `json:"serverArgs"`
	}
	require.NoError(t, json.NewDecoder(respGet.Body).Decode(&payload))
	require.NotEmpty(t, payload.ServerArgs, "serverArgs показывает, что уйдёт в sd-server")
	assert.Contains(t, payload.ServerArgs, "--model",
		"sd15 — all-in-one, значит --model (не --diffusion-model)")
	assert.Contains(t, payload.ServerArgs, "--seed")
}

// TestImageProfile_PutValidationDelegated — границы проверяет
// types.ValidateImageModelProfile (не дублируем правила).
func TestImageProfile_PutValidationDelegated(t *testing.T) {
	imageProfilesPathForTest(t)
	server, _, _ := createTestServer(t)
	defer server.Close()

	cases := []struct {
		name     string
		body     string
		wantCode int
		wantMsg  string
	}{
		{
			name:     "width not multiple of 64",
			body:     `{"family":"sd15","files":[{"role":"diffusion","repo":"a","filename":"m.gguf"}],"defaults":{"steps":20,"cfgScale":7,"width":100,"height":512,"batchCount":1}}`,
			wantCode: http.StatusBadRequest,
			wantMsg:  "multiple of 64",
		},
		{
			name:     "unknown family",
			body:     `{"family":"nope","files":[{"role":"diffusion","repo":"a","filename":"m.gguf"}]}`,
			wantCode: http.StatusBadRequest,
			wantMsg:  "unknown family",
		},
		{
			name:     "missing files",
			body:     `{"family":"sd15"}`,
			wantCode: http.StatusBadRequest,
			wantMsg:  "files is required",
		},
		{
			name:     "flux without vae",
			body:     `{"family":"flux","files":[{"role":"diffusion","repo":"a","filename":"m.gguf"}]}`,
			wantCode: http.StatusBadRequest,
			wantMsg:  "requires a separate",
		},
		{
			name:     "unknown JSON field",
			body:     `{"family":"sd15","files":[{"role":"diffusion","repo":"a","filename":"m.gguf"}],"bogus":1}`,
			wantCode: http.StatusBadRequest,
			wantMsg:  "invalid JSON body",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := putImageProfile(t, server.URL, "bad-profile", c.body)
			defer resp.Body.Close()
			assert.Equal(t, c.wantCode, resp.StatusCode)
			body, _ := io.ReadAll(resp.Body)
			assert.Contains(t, string(body), c.wantMsg)
		})
	}

	// Ни один невалидный профиль не должен появиться в списке.
	resp, err := http.Get(server.URL + "/api/v1/image/model-profiles")
	require.NoError(t, err)
	defer resp.Body.Close()
	var list imageModelProfileResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))
	assert.Equal(t, 0, list.Total)
}

// TestImageProfile_PutRejectsOversizedBody — MaxBytesReader 64KB (как у
// cppworker-профилей): защита от «профиля» с гигабайтом мусора.
func TestImageProfile_PutRejectsOversizedBody(t *testing.T) {
	imageProfilesPathForTest(t)
	server, _, _ := createTestServer(t)
	defer server.Close()

	big := `{"family":"sd15","notes":"` + strings.Repeat("x", 70*1024) + `","files":[{"role":"diffusion","repo":"a","filename":"m.gguf"}]}`
	resp := putImageProfile(t, server.URL, "huge", big)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestImageProfile_MergePreservesUnsetFields — PATCH-like семантика + presence
// для булевых: сохранение «только notes» не должно сбрасывать runtime, а
// `"offloadToCpu": false` должно флаг СНИМАТЬ.
func TestImageProfile_MergePreservesUnsetFields(t *testing.T) {
	imageProfilesPathForTest(t)
	server, _, _ := createTestServer(t)
	defer server.Close()

	full := `{
	  "family": "z_image",
	  "files": [
	    {"role":"diffusion","repo":"leejet/Z-Image-Turbo-GGUF","filename":"z_image_turbo-Q3_K.gguf"},
	    {"role":"vae","repo":"black-forest-labs/FLUX.1-schnell","filename":"ae.safetensors"},
	    {"role":"llm","repo":"unsloth/Qwen3-4B-Instruct-2507-GGUF","filename":"Qwen3-4B-Instruct-2507-Q4_K_M.gguf"}
	  ],
	  "defaults": {"steps":8,"cfgScale":1,"sampler":"euler","scheduler":"smoothstep","width":512,"height":1024,"batchCount":1,"seed":-1},
	  "runtime": {"offloadToCpu": true, "diffusionFa": true, "seedMode": "random"},
	  "vramEstimateMb": 3400,
	  "timeoutSec": 900,
	  "idleUnloadMinutes": 10,
	  "notes": "first"
	}`
	resp := putImageProfile(t, server.URL, "z-image-demo", full)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	decodeProfileResponse(t, resp)

	// 2) Только notes: runtime/файлы/vram должны сохраниться.
	resp = putImageProfile(t, server.URL, "z-image-demo", `{"notes":"second"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	p := decodeProfileResponse(t, resp)
	assert.Equal(t, "second", p.Notes)
	assert.True(t, p.Runtime.OffloadToCPU, "offloadToCpu must survive a partial update")
	assert.True(t, p.Runtime.DiffusionFA)
	assert.Equal(t, 3400, p.VramEstimateMB)
	assert.Equal(t, 900, p.TimeoutSec)
	assert.Equal(t, 10, p.IdleUnloadMinutes)
	assert.Len(t, p.Files, 3)
	assert.Equal(t, 512, p.Defaults.Width)

	// 3) Явное false снимает флаг (без presence это было бы невозможно).
	resp = putImageProfile(t, server.URL, "z-image-demo", `{"runtime":{"offloadToCpu":false}}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	p = decodeProfileResponse(t, resp)
	assert.False(t, p.Runtime.OffloadToCPU, "explicit false must clear the flag")
	assert.True(t, p.Runtime.DiffusionFA, "не переданный флаг не должен сбрасываться")

	// 4) Явный seed=0 (валидный фиксированный seed) сохраняется.
	resp = putImageProfile(t, server.URL, "z-image-demo", `{"runtime":{"seedMode":"fixed"},"defaults":{"seed":0}}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	p = decodeProfileResponse(t, resp)
	assert.Equal(t, int64(0), p.Defaults.Seed)
	assert.Equal(t, "fixed", p.Runtime.SeedMode)
	assert.False(t, p.SeedIsRandom())
}

// TestImageProfile_MergeCanZeroScalars — R-Image (2026-10-02): presence
// расширен на числовые/строковые скаляры, поэтому «0» и «""» теперь ЗНАЧЕНИЯ,
// а не «не менять». До этого из WebUI нельзя было обнулить timeoutSec/clipSkip
// или очистить backend/paramsBackend — редактор предупреждал подсказкой.
func TestImageProfile_MergeCanZeroScalars(t *testing.T) {
	imageProfilesPathForTest(t)
	server, _, _ := createTestServer(t)
	defer server.Close()

	full := `{
	  "family": "sd15",
	  "files": [{"role":"diffusion","repo":"second-state/SD1.5","filename":"sd15-q8.gguf"}],
	  "defaults": {"steps":20,"cfgScale":7,"sampler":"euler_a","scheduler":"discrete","width":512,"height":512,"batchCount":1,"seed":-1,"clipSkip":2},
	  "runtime": {"backend":"te=cpu","paramsBackend":"diffusion=disk","threads":8,"nGpuLayers":-1,"vaeTileSize":512},
	  "vramEstimateMb": 3000,
	  "timeoutSec": 600,
	  "idleUnloadMinutes": 15
	}`
	resp := putImageProfile(t, server.URL, "zero-demo", full)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	decodeProfileResponse(t, resp)

	// Явные нули/пустые строки ОБНУЛЯЮТ допустимые поля (а не «не меняют»),
	// и дефолты семейства их больше НЕ перетирают.
	zeroing := `{
	  "defaults": {"clipSkip":0,"sampler":"","scheduler":""},
	  "runtime": {"backend":"","paramsBackend":"","threads":0,"nGpuLayers":0,"vaeTileSize":0},
	  "vramEstimateMb": 0,
	  "timeoutSec": 0,
	  "idleUnloadMinutes": 0
	}`
	resp = putImageProfile(t, server.URL, "zero-demo", zeroing)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	p := decodeProfileResponse(t, resp)

	assert.Equal(t, 0, p.Defaults.ClipSkip)
	assert.Equal(t, "", p.Defaults.Sampler, "explicit empty sampler must stay empty")
	assert.Equal(t, "", p.Defaults.Scheduler)
	assert.Equal(t, "", p.Runtime.Backend)
	assert.Equal(t, "", p.Runtime.ParamsBackend)
	assert.Equal(t, 0, p.Runtime.Threads)
	assert.Equal(t, 0, p.Runtime.VaeTileSize)
	assert.Equal(t, 0, p.VramEstimateMB)
	assert.Equal(t, 0, p.TimeoutSec)
	assert.Equal(t, 0, p.IdleUnloadMinutes)

	// А поле, которого в теле НЕТ, по-прежнему не меняется (PATCH-семантика):
	// family/файлы/размеры остались с первого PUT.
	assert.Equal(t, "sd15", p.Family)
	assert.Len(t, p.Files, 1)
	assert.Equal(t, 20, p.Defaults.Steps, "steps без presence берётся из профиля/дефолта")

	// Явный невалидный ноль НЕ подменяется дефолтом молча — это 400 с границами
	// (steps: 0 физически невалиден: диапазон 1…100).
	resp = putImageProfile(t, server.URL, "zero-demo", `{"defaults":{"steps":0}}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode,
		"explicit steps=0 must be rejected, not silently replaced by the family default")
	assert.Contains(t, string(body), "steps")
}

// TestImageProfile_DeleteAndNotFound — DELETE/GET и их 404.
func TestImageProfile_DeleteAndNotFound(t *testing.T) {
	imageProfilesPathForTest(t)
	server, _, _ := createTestServer(t)
	defer server.Close()

	resp := putImageProfile(t, server.URL, "to-delete", sd15ProfileBody)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodDelete, server.URL+"/api/v1/image/model-profiles/to-delete", nil)
	delResp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	delResp.Body.Close()
	assert.Equal(t, http.StatusOK, delResp.StatusCode)

	getResp, err := http.Get(server.URL + "/api/v1/image/model-profiles/to-delete")
	require.NoError(t, err)
	getResp.Body.Close()
	assert.Equal(t, http.StatusNotFound, getResp.StatusCode)

	delResp2, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	delResp2.Body.Close()
	assert.Equal(t, http.StatusNotFound, delResp2.StatusCode, "повторный DELETE → 404")

	getUnknown, err := http.Get(server.URL + "/api/v1/image/model-profiles/never-existed")
	require.NoError(t, err)
	getUnknown.Body.Close()
	assert.Equal(t, http.StatusNotFound, getUnknown.StatusCode)
}

// TestImageProfile_List — список отдаёт все профили и корректный total.
func TestImageProfile_List(t *testing.T) {
	imageProfilesPathForTest(t)
	server, _, _ := createTestServer(t)
	defer server.Close()

	resp := putImageProfile(t, server.URL, "p1", sd15ProfileBody)
	resp.Body.Close()
	resp = putImageProfile(t, server.URL, "p2", sd15ProfileBody)
	resp.Body.Close()

	listResp, err := http.Get(server.URL + "/api/v1/image/model-profiles")
	require.NoError(t, err)
	defer listResp.Body.Close()
	require.Equal(t, http.StatusOK, listResp.StatusCode)

	var list imageModelProfileResponse
	require.NoError(t, json.NewDecoder(listResp.Body).Decode(&list))
	assert.Equal(t, 2, list.Total)
	assert.Contains(t, list.Models, "p1")
	assert.Contains(t, list.Models, "p2")
}

// TestImageProfile_ApplyAndProgress — apply сохраняет профиль, отдаёт applyId,
// а progress/status возвращают тот же результат (WebUI-контракт).
func TestImageProfile_ApplyAndProgress(t *testing.T) {
	imageProfilesPathForTest(t)
	server, _, _ := createTestServer(t)
	defer server.Close()

	// 1) Новый профиль без тела apply → 400 (нечего применять).
	resp, err := http.Post(server.URL+"/api/v1/image/model-profiles/ghost/apply", "application/json", nil)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// 2) Apply создаёт профиль из тела.
	resp, err = http.Post(server.URL+"/api/v1/image/model-profiles/sd15-apply/apply",
		"application/json", strings.NewReader(sd15ProfileBody))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var apply imageModelProfileApplyResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&apply))
	resp.Body.Close()
	assert.Equal(t, "completed", apply.Status)
	assert.NotEmpty(t, apply.ApplyID)
	assert.Equal(t, "sd15-apply", apply.Model)
	assert.Equal(t, types.DefaultImageGenDefaults("sd15").Steps, apply.Profile.Defaults.Steps)

	// 3) progress по имени модели.
	progResp, err := http.Get(server.URL + "/api/v1/image/model-profiles/sd15-apply/apply/progress")
	require.NoError(t, err)
	defer progResp.Body.Close()
	require.Equal(t, http.StatusOK, progResp.StatusCode)
	var progress imageModelProfileApplyResponse
	require.NoError(t, json.NewDecoder(progResp.Body).Decode(&progress))
	assert.Equal(t, apply.ApplyID, progress.ApplyID)

	// 4) status по applyId.
	statusResp, err := http.Get(server.URL + "/api/v1/image/model-profiles/sd15-apply/apply/status/" + apply.ApplyID)
	require.NoError(t, err)
	statusResp.Body.Close()
	assert.Equal(t, http.StatusOK, statusResp.StatusCode)

	// 5) Неизвестный applyId → 404.
	missingResp, err := http.Get(server.URL + "/api/v1/image/model-profiles/sd15-apply/apply/status/nope")
	require.NoError(t, err)
	missingResp.Body.Close()
	assert.Equal(t, http.StatusNotFound, missingResp.StatusCode)

	// 6) Progress для модели, которую никогда не применяли → 404.
	noneResp, err := http.Get(server.URL + "/api/v1/image/model-profiles/never/apply/progress")
	require.NoError(t, err)
	noneResp.Body.Close()
	assert.Equal(t, http.StatusNotFound, noneResp.StatusCode)
}

// TestImageModelCatalogEndpoint_ServesRepoCatalog — эндпоинт отдаёт РЕАЛЬНЫЙ
// каталог из репозитория (он же валидируется в internal/config).
func TestImageModelCatalogEndpoint_ServesRepoCatalog(t *testing.T) {
	imageProfilesPathForTest(t)
	t.Setenv("LB_IMAGE_MODEL_CATALOG_PATH", filepath.Join("..", "..", "config", "image-model-catalog.json"))

	server, _, _ := createTestServer(t)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/image/model-catalog")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var payload struct {
		Version int                       `json:"version"`
		Total   int                       `json:"total"`
		Notices []string                  `json:"notices"`
		Presets []types.ImageModelProfile `json:"presets"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))

	assert.Equal(t, 1, payload.Version)
	assert.GreaterOrEqual(t, payload.Total, 9)
	assert.NotEmpty(t, payload.Notices, "оговорки каталога (gated-репо, leejet-only) должны быть видны в API")

	names := map[string]bool{}
	for _, p := range payload.Presets {
		names[p.Name] = true
		require.NoError(t, types.ValidateImageModelProfile(&p), "пресет %q невалиден", p.Name)
	}
	for _, want := range []string{"sd15-q8-0", "sdxl-turbo-q8-0", "z-image-turbo-q3-k",
		"flux2-klein-4b-q4-0", "flux-schnell-q3-k"} {
		assert.True(t, names[want], "каталог должен содержать пресет %q", want)
	}
}

// TestImageModelCatalogEndpoint_MissingFileServesEmbedded — отсутствующий файл
// каталога НЕ ломает API: отдаётся вшитая в бинарь копия (R85).
//
// ПОЧЕМУ ТАК, А НЕ 404: путь по умолчанию относительный, и балансер, запущенный
// не из корня репозитория, иначе остался бы без каталога пресетов вообще — а
// каталог нужен и WebUI, и выбору модели инструментом. При этом невалидный файл
// (он ЕСТЬ, но битый) по-прежнему даёт 500: опечатку оператора скрывать нельзя.
func TestImageModelCatalogEndpoint_MissingFileServesEmbedded(t *testing.T) {
	imageProfilesPathForTest(t)
	t.Setenv("LB_IMAGE_MODEL_CATALOG_PATH", filepath.Join(t.TempDir(), "missing.json"))

	server, _, _ := createTestServer(t)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/image/model-catalog")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var payload struct {
		Total   int                       `json:"total"`
		Presets []types.ImageModelProfile `json:"presets"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	assert.GreaterOrEqual(t, payload.Total, 9, "вшитая копия каталога должна отдаваться целиком")
	names := map[string]bool{}
	for _, p := range payload.Presets {
		names[p.Name] = true
	}
	assert.True(t, names["sd15-q8-0"], "вшитая копия должна содержать штатные пресеты")
}

// TestImageModelCatalogEndpoint_InvalidFileIs500 — битый файл каталога = 500 с
// причиной (не 404 и не тихая подмена вшитой копией).
func TestImageModelCatalogEndpoint_InvalidFileIs500(t *testing.T) {
	imageProfilesPathForTest(t)
	bad := filepath.Join(t.TempDir(), "broken.json")
	require.NoError(t, os.WriteFile(bad, []byte(`{"presets":[{"name":"x"}]}`), 0o600))
	t.Setenv("LB_IMAGE_MODEL_CATALOG_PATH", bad)

	server, _, _ := createTestServer(t)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/image/model-catalog")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "invalid catalog")
}

// TestImageProfile_UnsafeNameRejected — имя профиля = имя каталога bundle;
// path traversal обязан быть отвергнут (иначе bundle уедет за modelsDir).
func TestImageProfile_UnsafeNameRejected(t *testing.T) {
	path := imageProfilesPathForTest(t)
	server, _, _ := createTestServer(t)
	defer server.Close()

	// Имя с точкой в начале (скрытый каталог) — отвергаем store'ом.
	resp := putImageProfile(t, server.URL, ".hidden", sd15ProfileBody)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// Файл профилей не должен содержать запись.
	data, err := os.ReadFile(path)
	if err == nil {
		assert.NotContains(t, string(data), ".hidden")
	}
}
