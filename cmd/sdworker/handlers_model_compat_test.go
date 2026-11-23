// handlers_model_compat_test.go — R696 (2026-11-23): загрузка модели, которую
// imageworker заведомо не поднимет, отклоняется ДО запуска движка и с причиной.
//
// ЖИВОЙ ДЕФЕКТ (локальный стенд, imageworker r261120-r648): оператор скачал с
// вкладки HuggingFace ОДИН файл `Qwen-Image-2.1-Turbo-Uncensored-AD-Q4_K.gguf`,
// воркер синтезировал профиль по имени (роль diffusion), поднял sd-server как
// all-in-one и через полторы минуты в панель пришло только «модель не
// загрузилась». Причина — в логе движка:
//
//	[err] [ERROR] diffusion_engine.cpp:992 - get sd version from file failed
//	[err] [ERROR] main.cpp:93 - new_sd_ctx_t failed
//
// Проверки:
//   - POST /api/image/models/load → 409 bundle_unsupported с error/hint/compat,
//     движок НЕ запускается (ProcessRunner не вызывался);
//   - force=true — аварийный клапан оператора: загрузка идёт (202, движок стартует);
//   - GET /api/image/models отдаёт compat с вердиктом и причиной (видно в UI).
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/internal/sdbackend"
	"ollama-loadbalancer/pkg/types"
)

// buildTextEncoderGGUF — GGUF текстового энкодера Qwen3-VL (архитектура +
// тензоры LLM). Ровно то, что лежит в файле из живой жалобы.
func buildTextEncoderGGUF() []byte {
	var buf bytes.Buffer
	buf.WriteString("GGUF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(3))
	tensors := []string{
		"model.embed_tokens.weight",
		"model.layers.0.self_attn.q_proj.weight",
		"model.norm.weight",
	}
	kvs := [][2]string{{"general.architecture", "qwen3vl"}, {"general.size_label", "8.2B"}}
	_ = binary.Write(&buf, binary.LittleEndian, uint64(len(tensors)))
	_ = binary.Write(&buf, binary.LittleEndian, uint64(len(kvs)))
	writeStr := func(s string) {
		_ = binary.Write(&buf, binary.LittleEndian, uint64(len(s)))
		buf.WriteString(s)
	}
	for _, kv := range kvs {
		writeStr(kv[0])
		_ = binary.Write(&buf, binary.LittleEndian, uint32(8))
		writeStr(kv[1])
	}
	for _, name := range tensors {
		writeStr(name)
		_ = binary.Write(&buf, binary.LittleEndian, uint32(2))
		_ = binary.Write(&buf, binary.LittleEndian, uint64(4096))
		_ = binary.Write(&buf, binary.LittleEndian, uint64(4))
		_ = binary.Write(&buf, binary.LittleEndian, uint32(0))
		_ = binary.Write(&buf, binary.LittleEndian, uint64(0))
	}
	return buf.Bytes()
}

// countingRunner — ProcessRunner, считающий запуски sd-server: отказ обязан быть
// ДО спавна, иначе движок займёт VRAM мёртвым набором.
type countingRunner struct{ calls int32 }

func (r *countingRunner) Start(_ context.Context, _ []string) (sdbackend.Process, error) {
	atomic.AddInt32(&r.calls, 1)
	return &fakeProcess{pid: 5555, done: make(chan struct{})}, nil
}

func (r *countingRunner) count() int { return int(atomic.LoadInt32(&r.calls)) }

// TestLoadModel_RefusesIncompatibleBundle — энкодер в роли diffusion: отказ до
// запуска движка, с понятной причиной в ответе.
func TestLoadModel_RefusesIncompatibleBundle(t *testing.T) {
	app, _ := newTestApp(t, map[string]types.ImageModelProfile{
		"qwen-te": {
			Family: "other",
			Files: []types.ImageModelFile{{
				Role: types.ImageFileRoleDiffusion, Repo: "local",
				Filename: "Qwen-Image-2.1-Turbo-Uncensored-AD-Q4_K.gguf",
			}},
		},
	})
	// Подменяем файл содержимым настоящего текстового энкодера (заголовок GGUF:
	// general.architecture=qwen3vl) и перечитываем реестр.
	modelsDir, err := app.svc.Config.ModelsDirAbs()
	if err != nil {
		t.Fatalf("models dir: %v", err)
	}
	bundleDir := filepath.Join(modelsDir, "qwen-te")
	tePath := filepath.Join(bundleDir, "Qwen-Image-2.1-Turbo-Uncensored-AD-Q4_K.gguf")
	if err := os.WriteFile(tePath, buildTextEncoderGGUF(), 0o644); err != nil {
		t.Fatalf("write te: %v", err)
	}
	// Профиль пишем ПОЛНЫЙ (фикстура newTestApp не заполняет defaults, а
	// ValidateImageModelProfile их требует): иначе реестр положил бы placeholder
	// без файлов, и вердикт не из чего было бы считать.
	prof := types.ImageModelProfile{
		Name:   "qwen-te",
		Family: "other",
		Files: []types.ImageModelFile{{
			Role: types.ImageFileRoleDiffusion, Repo: "local",
			Filename: "Qwen-Image-2.1-Turbo-Uncensored-AD-Q4_K.gguf",
		}},
		Defaults: types.DefaultImageGenDefaults("other"),
	}
	rawProfile, err := json.Marshal(prof)
	if err != nil {
		t.Fatalf("marshal profile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "profile.json"), rawProfile, 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	if err := app.svc.Registry.Load(); err != nil {
		t.Fatalf("registry reload: %v", err)
	}
	runner := &countingRunner{}
	app.svc.Sup.SetRunner(runner)

	router := app.setupRouter()
	rec := doJSON(t, router, http.MethodPost, "/api/image/models/load", map[string]any{"name": "qwen-te"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("load = %d body=%s, want 409", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"bundle_unsupported", "text encoder", "force=true", "compat", "qwen3vl"} {
		if !strings.Contains(body, want) {
			t.Fatalf("в ответе нет %q: %s", want, body)
		}
	}
	if got := runner.count(); got != 0 {
		t.Fatalf("sd-server запускался %d раз, хотя набор заведомо несовместим", got)
	}

	// Аварийный клапан: force=true → загрузка запускается (202), движок стартует.
	rec = doJSON(t, router, http.MethodPost, "/api/image/models/load",
		map[string]any{"name": "qwen-te", "force": true})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("force load = %d body=%s, want 202", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for runner.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if runner.count() == 0 {
		t.Fatalf("force=true не запустил sd-server (аварийный клапан не работает)")
	}

	// GET /api/image/models отдаёт вердикт: UI показывает его до нажатия кнопки.
	rec = doJSON(t, router, http.MethodGet, "/api/image/models", nil)
	var listed struct {
		Models []struct {
			Name   string `json:"name"`
			Compat *struct {
				Verdict  string   `json:"verdict"`
				Runnable bool     `json:"runnable"`
				Reason   string   `json:"reason"`
				Missing  []string `json:"missing"`
				Files    []struct {
					Filename     string `json:"filename"`
					DetectedRole string `json:"detectedRole"`
				} `json:"files"`
			} `json:"compat"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("json: %v", err)
	}
	var compat *struct {
		Verdict  string   `json:"verdict"`
		Runnable bool     `json:"runnable"`
		Reason   string   `json:"reason"`
		Missing  []string `json:"missing"`
		Files    []struct {
			Filename     string `json:"filename"`
			DetectedRole string `json:"detectedRole"`
		} `json:"files"`
	}
	for i := range listed.Models {
		if listed.Models[i].Name == "qwen-te" {
			compat = listed.Models[i].Compat
		}
	}
	if compat == nil {
		t.Fatalf("в списке моделей нет вердикта compat: %s", rec.Body.String())
	}
	if compat.Verdict != "unsupported" || compat.Runnable {
		t.Fatalf("verdict=%q runnable=%v, want unsupported", compat.Verdict, compat.Runnable)
	}
	if !strings.Contains(compat.Reason, "text encoder") {
		t.Fatalf("причина вердикта не называет text encoder: %s", compat.Reason)
	}
	if len(compat.Files) != 1 || compat.Files[0].DetectedRole != types.ImageFileRoleLLM {
		t.Fatalf("файл не помечен ролью llm по заголовку: %+v", compat.Files)
	}
}
