// bundle_test.go — Фаза 4: цель `image_bundle` («набор файлов → bundle»).
//
// ЧТО ИМЕННО ЗАКРЕПЛЯЕТСЯ И ПОЧЕМУ.
//
//  1. РОЛИ ПО ЗАГОЛОВКАМ (R696), а не по имени: набор из четырёх файлов обязан
//     разложиться как diffusion/vae/clip_l/t5xxl и получить вердикт ready. Это
//     тот же вердикт, что покажет вкладка моделей, поэтому тест читает его из
//     result/verify, а не пересказывает лог.
//  2. ARCH-ПОДСКАЗКА НЕ КРАДЁТ РОЛЬ: живой `ae.safetensors` (arch "Flux.1-AE")
//     по заголовку выглядит diffusion-моделью. Если бы роль бралась только из
//     заголовка, настоящий diffusion-файл выпал бы «дубликатом», и набор честно
//     стал бы неполным — на этом стоял живой прогон flux-набора.
//  3. ЧЕСТНЫЙ ОТКАЗ: набор из одного текстового энкодера — это failed с кодом
//     `bundle_not_ready` и названной причиной, а НЕ «succeeded, а потом разберёмся».
//  4. ГРАНИЦЫ ПУТЕЙ: каталог bundle только внутри ${MODELS_DIR} (path_not_allowed),
//     источник — только внутри тома; файл вместо каталога — отказ.
//  5. МЕСТО ДО СТАРТА: копия набора требует «вход + выход + запас», в режиме
//     «манифест на месте» копии нет — и требовать гигабайты нельзя.
//  6. ВОРКЕР: если он настроен и не ответил — задача failed (`worker_unavailable`),
//     потому что «собрали, но не проверили» это не успех. Если не настроен —
//     worker_checked=false в verify (отдельное поле, а не враньё).
//  7. МАНИФЕСТЫ: profile.json с ролями и БЕЗ localPath (переносимый bundle) и
//     sd-server.config.json в форме супервизора.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/imagemodel"
	"ollama-loadbalancer/pkg/types"
)

// ---------------- строители набора ----------------

// writeBundleGGUF пишет минимальный GGUF с нужной architecture и тензорами.
//
// Полноценные веса тесту не нужны: роль и семейство определяются по ЗАГОЛОВКУ
// (R696), а значит проверяется ровно то, на что смотрит воркер. Строитель общий
// с gguf_test.go (ggufValueWriter), чтобы формат шапки не разъезжался.
func writeBundleGGUF(t *testing.T, path, arch string, tensors map[string]uint64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("каталог %s: %v", filepath.Dir(path), err)
	}
	w := &ggufValueWriter{}
	w.buf = append(w.buf, []byte(ggufMagic)...)
	w.u32(3) // версия
	w.u64(uint64(len(tensors)))
	w.u64(1) // одна KV-пара: general.architecture
	w.kvString("general.architecture", arch)
	names := make([]string, 0, len(tensors))
	for name := range tensors {
		names = append(names, name)
	}
	sort.Strings(names) // детерминированный порядок таблицы тензоров
	for _, name := range names {
		w.tensor(name, tensors[name])
	}
	if err := os.WriteFile(path, w.buf, 0o644); err != nil {
		t.Fatalf("запись %s: %v", path, err)
	}
}

// writeFluxSet — тот самый расклад набора: diffusion с тензорами double_blocks,
// VAE с arch-подсказкой «Flux.1-AE», CLIP-L и T5-энкодер.
func writeFluxSet(t *testing.T, dir string) {
	t.Helper()
	writeBundleGGUF(t, filepath.Join(dir, "flux1-schnell-q3_k.gguf"), "flux",
		map[string]uint64{"double_blocks.0.img_attn.qkv.weight": 64})
	writeBundleGGUF(t, filepath.Join(dir, "ae.safetensors.gguf"), "Flux.1-AE",
		map[string]uint64{"decoder.conv_in.weight": 4})
	writeBundleGGUF(t, filepath.Join(dir, "clip_l.gguf"), "clip",
		map[string]uint64{"text_model.encoder.layers.0.mlp.weight": 768})
	writeBundleGGUF(t, filepath.Join(dir, "t5-v1_1-xxl-encoder-Q4_K_M.gguf"), "t5encoder",
		map[string]uint64{"encoder.block.0.layer.0.SelfAttention.q.weight": 64})
}

// readProfile — profile.json собранного bundle.
func readProfile(t *testing.T, dir string) types.ImageModelProfile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, imagemodel.ProfileFileName))
	if err != nil {
		t.Fatalf("profile.json в %s: %v", dir, err)
	}
	var p types.ImageModelProfile
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("profile.json не разобран: %v", err)
	}
	return p
}

// roleOf — роль файла по basename в профиле.
func roleOf(p types.ImageModelProfile, base string) string {
	for _, f := range p.Files {
		if filepath.Base(f.Filename) == base {
			return f.Role
		}
	}
	return ""
}

// fakeBundleWorker — imageworker-заглушка: тест задаёт ответ и ошибку.
type fakeBundleWorker struct {
	res map[string]bundleWorkerResult
	err error
	// rescanned — умеет ли «воркер» принудительный скан (POST /api/image/models/scan).
	// false = старый образ: перечитывание только при смене набора каталогов.
	rescanned bool
	calls     int
}

func (f *fakeBundleWorker) models(context.Context) (map[string]bundleWorkerResult, bool, error) {
	f.calls++
	return f.res, f.rescanned, f.err
}

func (f *fakeBundleWorker) url() string { return "http://imageworker.test:18093" }

// setBundleWorker — подменить воркера у исполнителя цели.
func setBundleWorker(t *testing.T, m *Manager, w bundleWorker) {
	t.Helper()
	br, ok := m.bundle.(*bundleRunner)
	if !ok || br == nil {
		t.Fatalf("у менеджера нет исполнителя image_bundle: %T", m.bundle)
	}
	br.worker = w
}

// newBundleRig — менеджер на временных каталогах + каталог набора внутри тома.
func newBundleRig(t *testing.T) (*Manager, config, string, context.CancelFunc) {
	t.Helper()
	m, cfg, cancel := newTestManager(t, &fakeRunner{})
	src := filepath.Join(cfg.ModelsDir, "staging", "flux-set")
	writeFluxSet(t, src)
	return m, cfg, src, cancel
}

// factsOf — result.facts задачи.
func factsOf(t *testing.T, v jobView) map[string]interface{} {
	t.Helper()
	if v.Result == nil {
		t.Fatalf("у задачи нет result (status=%q, error=%v)", v.Status, v.Error)
	}
	return v.Result.Facts
}

func verifyOf(t *testing.T, v jobView) map[string]interface{} {
	t.Helper()
	if v.Result == nil {
		t.Fatalf("у задачи нет result")
	}
	return v.Result.Verify
}

// ---------------- 1. раскладка и манифесты ----------------

func TestBundle_BuildsReadyBundleAndWritesManifests(t *testing.T) {
	m, cfg, src, cancel := newBundleRig(t)
	defer cancel()

	view, err := m.Create(createRequest{ModelDir: "staging/flux-set", Target: imageBundleTarget,
		Options: jobOptions{Name: "flux-built"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	final := waitStatus(t, m, view.ID, statusSucceeded, 10*time.Second)

	out := bundleOut(cfg, "flux-built")
	if final.OutPath != out {
		t.Fatalf("каталог результата %q, ожидался %q", final.OutPath, out)
	}
	// Источник не тронут: сервис читает набор и никогда не переписывает его.
	for _, name := range []string{"flux1-schnell-q3_k.gguf", "ae.safetensors.gguf", "clip_l.gguf"} {
		if _, err := os.Stat(filepath.Join(src, name)); err != nil {
			t.Fatalf("исходный файл %s исчез: %v", name, err)
		}
	}

	p := readProfile(t, out)
	if p.Name != "flux-built" {
		t.Fatalf("profile.name=%q, ожидалось имя каталога bundle", p.Name)
	}
	if p.Family != "flux" {
		t.Fatalf("profile.family=%q, ожидалось flux (по тензорам diffusion-файла)", p.Family)
	}
	wantRoles := map[string]string{
		"flux1-schnell-q3_k.gguf":         types.ImageFileRoleDiffusion,
		"ae.safetensors.gguf":             types.ImageFileRoleVae,
		"clip_l.gguf":                     types.ImageFileRoleClipL,
		"t5-v1_1-xxl-encoder-Q4_K_M.gguf": types.ImageFileRoleT5xxl,
	}
	for base, want := range wantRoles {
		if got := roleOf(p, base); got != want {
			t.Fatalf("роль %s = %q, ожидалась %q", base, got, want)
		}
	}
	// Переносимость: путь из контейнера в профиль попасть не должен.
	for _, f := range p.Files {
		if f.LocalPath != "" {
			t.Fatalf("в profile.json попал localPath %q: bundle перестанет переноситься", f.LocalPath)
		}
	}
	if p.Defaults.Steps != 4 || p.Defaults.CFGScale != 1 {
		t.Fatalf("дефолты семейства flux не применились: %+v", p.Defaults)
	}
	// Файлы на месте.
	for base := range wantRoles {
		if _, err := os.Stat(filepath.Join(out, base)); err != nil {
			t.Fatalf("файл %s не скопирован в bundle: %v", base, err)
		}
	}

	// sd-server.config.json — в форме супервизора воркера.
	sidecarRaw, err := os.ReadFile(filepath.Join(out, bundleSidecarName))
	if err != nil {
		t.Fatalf("sd-server.config.json: %v", err)
	}
	var sidecar map[string]interface{}
	if err := json.Unmarshal(sidecarRaw, &sidecar); err != nil {
		t.Fatalf("sd-server.config.json не разобран: %v", err)
	}
	if sidecar["model"] != "flux-built" || sidecar["profile_source"] != bundleProfileName {
		t.Fatalf("sidecar: %v", sidecar)
	}
	if sidecar["family"] != "flux" || sidecar["seed_mode"] != "random" {
		t.Fatalf("sidecar без семейства/seed_mode: %v", sidecar)
	}
	built, _ := sidecar["built_by"].(map[string]interface{})
	if built == nil || built["target"] != imageBundleTarget {
		t.Fatalf("sidecar.built_by не описывает цель: %v", sidecar["built_by"])
	}

	facts := factsOf(t, final)
	if facts["compat_verdict"] != imagemodel.CompatReady {
		t.Fatalf("вердикт после сборки %v, ожидался ready (reason: %v)",
			facts["compat_verdict"], facts["compat_reason"])
	}
	if facts["ready"] != true {
		t.Fatalf("ready=%v", facts["ready"])
	}
	verify := verifyOf(t, final)
	if verify["worker_checked"] != false {
		t.Fatalf("воркер не настраивался, а verify говорит checked=%v", verify["worker_checked"])
	}
	if msg, _ := verify["worker_error"].(string); !strings.Contains(msg, "REEXPORT_IMAGEWORKER_URL") {
		t.Fatalf("причина «локальная проверка» не названа: %v", verify["worker_error"])
	}
}

// ---------------- 2. arch-подсказка не крадёт роль ----------------

func TestBundle_ArchHintDoesNotStealComponentRole(t *testing.T) {
	m, cfg, _, cancel := newBundleRig(t)
	defer cancel()

	// Имя файла «ae.safetensors.gguf» + arch "Flux.1-AE": по заголовку парсер
	// видит diffusion-семейство, а это VAE. Роль обязана остаться vae, иначе
	// настоящий diffusion-файл выпадет «дубликатом роли» и набор станет неполным.
	view, err := m.Create(createRequest{ModelDir: "staging/flux-set", Target: imageBundleTarget,
		Options: jobOptions{Name: "flux-arch-hint"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	final := waitStatus(t, m, view.ID, statusSucceeded, 10*time.Second)
	p := readProfile(t, bundleOut(cfg, "flux-arch-hint"))
	if got := roleOf(p, "ae.safetensors.gguf"); got != types.ImageFileRoleVae {
		t.Fatalf("VAE с arch-подсказкой получил роль %q: набор потерял бы VAE", got)
	}
	if got := roleOf(p, "flux1-schnell-q3_k.gguf"); got != types.ImageFileRoleDiffusion {
		t.Fatalf("настоящий diffusion-файл получил роль %q", got)
	}
	if facts := factsOf(t, final); facts["compat_verdict"] != imagemodel.CompatReady {
		t.Fatalf("вердикт %v, ожидался ready", facts["compat_verdict"])
	}
}

// ---------------- 3. честный отказ: набор из энкодера ----------------

func TestBundle_EncoderOnlySetFailsWithHonestVerdict(t *testing.T) {
	m, cfg, _, cancel := newBundleRig(t)
	defer cancel()

	src := filepath.Join(cfg.ModelsDir, "staging", "te-only")
	// ЖИВОЙ СЛУЧАЙ R696: имя выглядит diffusion-моделью, заголовок — qwen3vl.
	writeBundleGGUF(t, filepath.Join(src, "Qwen-Image-2.1-Turbo-Uncensored-AD-Q4_K.gguf"), "qwen3vl",
		map[string]uint64{"model.embed_tokens.weight": 64, "model.layers.0.self_attn.q_proj.weight": 64})

	view, err := m.Create(createRequest{ModelDir: "staging/te-only", Target: imageBundleTarget,
		Options: jobOptions{Name: "te-only"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	final := waitStatus(t, m, view.ID, statusFailed, 10*time.Second)
	if final.Error == nil || final.Error.Code != "bundle_not_ready" {
		t.Fatalf("код отказа %+v, ожидался bundle_not_ready", final.Error)
	}
	if !strings.Contains(final.Error.Message, "text encoder") && !strings.Contains(final.Error.Message, "diffusion") {
		t.Fatalf("в отказе не названо, чего не хватает: %s", final.Error.Message)
	}
	// Манифест записан: воркер обязан ПОКАЗАТЬ набор и ту же причину, а не
	// потерять каталог как «сломанный профиль».
	p := readProfile(t, bundleOut(cfg, "te-only"))
	if len(p.Files) != 1 || p.Files[0].Role != types.ImageFileRoleDiffusion {
		t.Fatalf("профиль не зарегистрирован честно: %+v", p.Files)
	}
	// Диагностика осталась в итоге задачи (файлы уже скопированы — это не повод
	// терять причину).
	if final.Result == nil || final.Result.Facts["compat_verdict"] != imagemodel.CompatUnsupported {
		t.Fatalf("в result нет вердикта unsupported: %+v", final.Result)
	}
}

// ---------------- 4. границы путей ----------------

func TestBundle_OutputEscapeIsPathNotAllowed(t *testing.T) {
	m, _, _, cancel := newBundleRig(t)
	defer cancel()

	// Абсолютный путь вне тома — каталог ВНЕ ${MODELS_DIR}: берём настоящий
	// временный каталог (/tmp на Linux, C:\…\Temp на Windows), а не «/tmp»:
	// на Windows "/tmp" не абсолютный, и проверка справедливо считает его
	// путём внутри тома.
	outside := t.TempDir()
	cases := []struct {
		name string
		opts jobOptions
	}{
		{"out_path с ..", jobOptions{OutPath: "../evil"}},
		{"out_path абсолютный вне тома", jobOptions{OutPath: filepath.Join(outside, "evil")}},
		{"имя bundle с ..", jobOptions{Name: "../evil"}},
		{"имя bundle со слэшем", jobOptions{Name: "sub/evil"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := m.Create(createRequest{ModelDir: "staging/flux-set", Target: imageBundleTarget, Options: c.opts})
			if err == nil {
				t.Fatalf("путь %+v принят: bundle можно записать мимо тома моделей", c.opts)
			}
			je, ok := err.(*jobError)
			if !ok {
				t.Fatalf("ошибка не jobError: %v", err)
			}
			if je.Code != "path_not_allowed" {
				t.Fatalf("код %q, ожидался path_not_allowed (%s)", je.Code, je.Message)
			}
		})
	}
}

func TestBundle_SourceMustBeDirectoryInsideModels(t *testing.T) {
	m, cfg, _, cancel := newBundleRig(t)
	defer cancel()

	// Один файл вместо каталога: цель собирает набор, а не переупаковывает файл.
	if _, err := m.Create(createRequest{ModelDir: "staging/flux-set/clip_l.gguf",
		Target: imageBundleTarget}); err == nil {
		t.Fatal("одиночный файл принят источником image_bundle")
	}
	// Каталог без весов.
	empty := filepath.Join(cfg.ModelsDir, "staging", "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatalf("каталог: %v", err)
	}
	if err := os.WriteFile(filepath.Join(empty, "readme.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("файл: %v", err)
	}
	if _, err := m.Create(createRequest{ModelDir: "staging/empty", Target: imageBundleTarget}); err == nil {
		t.Fatal("каталог без файлов весов принят источником")
	}
	// Путь за томом моделей.
	if _, err := m.Create(createRequest{ModelDir: "../outside", Target: imageBundleTarget}); err == nil {
		t.Fatal("источник вне тома моделей принят")
	}
}

// ---------------- 5. место до старта ----------------

func TestBundle_RejectedWhenDiskIsShort(t *testing.T) {
	m, _, src, cancel := newBundleRig(t)
	defer cancel()

	var inputBytes int64
	for _, f := range mustReadDir(t, src) {
		inputBytes += f
	}
	// Набор копируется целиком: нужно «вход + выход + запас».
	const free = int64(1) << 20
	m.freeSpace = func(string) (int64, error) { return free, nil }

	view, err := m.Create(createRequest{ModelDir: "staging/flux-set", Target: imageBundleTarget,
		Options: jobOptions{Name: "short"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	final := waitStatus(t, m, view.ID, statusFailed, 10*time.Second)
	if final.Error == nil || final.Error.Code != "insufficient_space" {
		t.Fatalf("код %+v, ожидался insufficient_space", final.Error)
	}
	if final.StartedAt != nil {
		t.Fatal("задача стартовала, хотя места заведомо не хватает")
	}
	for _, want := range []string{"нужно", "свободно", formatGiB(2*inputBytes + spaceReserveBytes)} {
		if !strings.Contains(final.Error.Message, want) {
			t.Fatalf("в сообщении нет %q: %s", want, final.Error.Message)
		}
	}
	if _, err := os.Stat(bundleOut(m.cfg, "short")); !os.IsNotExist(err) {
		t.Fatalf("каталог результата создан при отказе по месту (err=%v)", err)
	}
}

// needForTask для bundle: копия — «вход + выход + запас», манифест на месте — 0.
func TestBundle_SpaceEstimate(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "set")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("каталог: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.gguf"), make([]byte, 1000), 0o644); err != nil {
		t.Fatalf("файл: %v", err)
	}
	need, err := needForTask(imageBundleTarget, src, filepath.Join(dir, "out"), jobOptions{})
	if err != nil {
		t.Fatalf("needForTask: %v", err)
	}
	if want := 2*int64(1000) + spaceReserveBytes; need.Need != want {
		t.Fatalf("нужно %d, ожидалось «вход + выход + запас» = %d", need.Need, want)
	}
	// Манифест в существующем каталоге: копирования нет, места не требует.
	need, err = needForTask(imageBundleTarget, src, src, jobOptions{})
	if err != nil {
		t.Fatalf("needForTask(in-place): %v", err)
	}
	if need.Need != 0 {
		t.Fatalf("манифест на месте требует %d байт, хотя ничего не копируется", need.Need)
	}
}

// ---------------- 6. воркер ----------------

func TestBundle_WorkerReadyMakesVerifyChecked(t *testing.T) {
	m, cfg, _, cancel := newBundleRig(t)
	defer cancel()
	fw := &fakeBundleWorker{rescanned: true, res: map[string]bundleWorkerResult{
		"flux-worker": {Found: true, Family: "flux", Verdict: imagemodel.CompatReady,
			Runnable: true, Reason: "готов"},
	}}
	setBundleWorker(t, m, fw)

	view, err := m.Create(createRequest{ModelDir: "staging/flux-set", Target: imageBundleTarget,
		Options: jobOptions{Name: "flux-worker"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	final := waitStatus(t, m, view.ID, statusSucceeded, 10*time.Second)
	if fw.calls == 0 {
		t.Fatal("imageworker не опрашивался: пересканирование и вердикт не подтверждены")
	}
	verify := verifyOf(t, final)
	if verify["worker_checked"] != true || verify["worker_verdict"] != imagemodel.CompatReady {
		t.Fatalf("verify не подтверждает проверку воркером: %v", verify)
	}
	if verify["worker_rescan"] != "scan" {
		t.Fatalf("режим пересканирования %v, ожидался scan (принудительное перечитывание)", verify["worker_rescan"])
	}
	if _, err := os.Stat(filepath.Join(bundleOut(cfg, "flux-worker"), bundleProfileName)); err != nil {
		t.Fatalf("bundle не собран: %v", err)
	}
}

func TestBundle_WorkerUnavailableFailsHonestly(t *testing.T) {
	m, cfg, _, cancel := newBundleRig(t)
	defer cancel()
	setBundleWorker(t, m, &fakeBundleWorker{err: fmt.Errorf("dial tcp: connection refused")})

	view, err := m.Create(createRequest{ModelDir: "staging/flux-set", Target: imageBundleTarget,
		Options: jobOptions{Name: "flux-unreachable"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	final := waitStatus(t, m, view.ID, statusFailed, 10*time.Second)
	if final.Error == nil || final.Error.Code != "worker_unavailable" {
		t.Fatalf("код %+v, ожидался worker_unavailable: «собрали и не проверили» — не успех", final.Error)
	}
	if !strings.Contains(final.Error.Message, "connection refused") {
		t.Fatalf("причина недоступности воркера не названа: %s", final.Error.Message)
	}
	// Файлы оставлены: сборка прошла, недоступен только воркер.
	if _, err := os.Stat(filepath.Join(bundleOut(cfg, "flux-unreachable"), bundleProfileName)); err != nil {
		t.Fatalf("bundle удалён из-за недоступности воркера: %v", err)
	}
}

func TestBundle_WorkerSeesBundleButNotReady(t *testing.T) {
	m, _, _, cancel := newBundleRig(t)
	defer cancel()
	setBundleWorker(t, m, &fakeBundleWorker{res: map[string]bundleWorkerResult{
		"flux-inc": {Found: true, Family: "flux", Verdict: imagemodel.CompatIncomplete,
			Runnable: false, Reason: "набор неполный", Missing: []string{"VAE"}},
	}})

	view, err := m.Create(createRequest{ModelDir: "staging/flux-set", Target: imageBundleTarget,
		Options: jobOptions{Name: "flux-inc"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	final := waitStatus(t, m, view.ID, statusFailed, 10*time.Second)
	if final.Error == nil || final.Error.Code != "bundle_not_ready" {
		t.Fatalf("код %+v, ожидался bundle_not_ready", final.Error)
	}
	if !strings.Contains(final.Error.Message, "VAE") {
		t.Fatalf("вердикт воркера не назвал, чего не хватает: %s", final.Error.Message)
	}
}

func TestBundle_WorkerDoesNotSeeBundle(t *testing.T) {
	m, _, _, cancel := newBundleRig(t)
	defer cancel()
	setBundleWorker(t, m, &fakeBundleWorker{res: map[string]bundleWorkerResult{}})

	view, err := m.Create(createRequest{ModelDir: "staging/flux-set", Target: imageBundleTarget,
		Options: jobOptions{Name: "flux-invisible"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	final := waitStatus(t, m, view.ID, statusFailed, 10*time.Second)
	if final.Error == nil || final.Error.Code != "bundle_not_visible" {
		t.Fatalf("код %+v, ожидался bundle_not_visible: молчаливый success скрыл бы другой том", final.Error)
	}
}

// TestBundle_InPlaceWorkerWithoutScanDoesNotLie — запись «на месте» + воркер без
// POST /api/image/models/scan: его вердикт мог считаться по профилю, который
// синтезировали ДО нашей записи. Задача не должна ни падать («набор плохой»,
// хотя локальный вердикт ready), ни молчать: итог — succeeded с ЯВНЫМ флагом
// worker_stale_possible и предупреждением в логе.
func TestBundle_InPlaceWorkerWithoutScanDoesNotLie(t *testing.T) {
	m, cfg, _, cancel := newBundleRig(t)
	defer cancel()
	src := filepath.Join(cfg.ModelsDir, "image", "flux-inplace-old")
	writeFluxSet(t, src)
	setBundleWorker(t, m, &fakeBundleWorker{rescanned: false, res: map[string]bundleWorkerResult{
		"flux-inplace-old": {Found: true, Family: "flux", Verdict: imagemodel.CompatIncomplete,
			Runnable: false, Reason: "набор неполный", Missing: []string{"VAE"}},
	}})

	view, err := m.Create(createRequest{ModelDir: "image/flux-inplace-old", Target: imageBundleTarget})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	final := waitStatus(t, m, view.ID, statusSucceeded, 10*time.Second)
	verify := verifyOf(t, final)
	if verify["worker_rescan"] != "get" {
		t.Fatalf("режим пересканирования %v, ожидался get", verify["worker_rescan"])
	}
	if verify["worker_stale_possible"] != true {
		t.Fatalf("устаревание вердикта воркера не отмечено: %v", verify)
	}
	if msg, _ := verify["worker_warning"].(string); !strings.Contains(msg, "scan") {
		t.Fatalf("предупреждение не подсказывает, что делать: %v", verify["worker_warning"])
	}
	if facts := factsOf(t, final); facts["compat_verdict"] != imagemodel.CompatReady {
		t.Fatalf("локальный вердикт потерян: %v", facts["compat_verdict"])
	}
}

// TestBundle_InPlaceWorkerWithScanIsTrusted — тот же вердикт воркера, но воркер
// УМЕЕТ скан: теперь это вердикт по собранному набору, и «не загрузится» —
// честный отказ.
func TestBundle_InPlaceWorkerWithScanIsTrusted(t *testing.T) {
	m, cfg, _, cancel := newBundleRig(t)
	defer cancel()
	src := filepath.Join(cfg.ModelsDir, "image", "flux-inplace-new")
	writeFluxSet(t, src)
	setBundleWorker(t, m, &fakeBundleWorker{rescanned: true, res: map[string]bundleWorkerResult{
		"flux-inplace-new": {Found: true, Family: "flux", Verdict: imagemodel.CompatIncomplete,
			Runnable: false, Reason: "набор неполный", Missing: []string{"VAE"}},
	}})

	view, err := m.Create(createRequest{ModelDir: "image/flux-inplace-new", Target: imageBundleTarget})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	final := waitStatus(t, m, view.ID, statusFailed, 10*time.Second)
	if final.Error == nil || final.Error.Code != "bundle_not_ready" {
		t.Fatalf("код %+v, ожидался bundle_not_ready", final.Error)
	}
	if verify := verifyOf(t, final); verify["worker_stale_possible"] == true {
		t.Fatalf("скан был, а устаревание всё равно отмечено: %v", verify)
	}
}

// ---------------- 7. режимы и переопределения ----------------

func TestBundle_InPlaceWritesManifestOnly(t *testing.T) {
	m, cfg, _, cancel := newBundleRig(t)
	defer cancel()

	// Каталог bundle — каталог image-моделей (его читает воркер): только там
	// «манифест на месте» совпадает с источником.
	src := filepath.Join(cfg.ModelsDir, "image", "flux-inplace")
	writeFluxSet(t, src)
	before := fileSizes(t, src)

	view, err := m.Create(createRequest{ModelDir: "image/flux-inplace", Target: imageBundleTarget})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	final := waitStatus(t, m, view.ID, statusSucceeded, 10*time.Second)
	if final.OutPath != src {
		t.Fatalf("каталог результата %q, ожидался каталог набора %q", final.OutPath, src)
	}
	// Файлы не копировались: размеры те же, новых файлов весов нет.
	after := fileSizes(t, src)
	for name, size := range before {
		if after[name] != size {
			t.Fatalf("файл %s изменился: %d → %d", name, size, after[name])
		}
	}
	p := readProfile(t, src)
	if p.Name != "flux-inplace" {
		t.Fatalf("profile.name=%q: имя модели обязано совпадать с каталогом", p.Name)
	}
	if final.Result != nil && final.Result.DstBytes != 0 {
		t.Fatalf("манифест на месте, а записано %d байт файлов", final.Result.DstBytes)
	}
	if verify := verifyOf(t, final); verify["in_place"] != true {
		t.Fatalf("in_place не отмечен в verify: %v", verify)
	}
}

func TestBundle_DryRunWritesNothing(t *testing.T) {
	m, cfg, _, cancel := newBundleRig(t)
	defer cancel()

	view, err := m.Create(createRequest{ModelDir: "staging/flux-set", Target: imageBundleTarget,
		Options: jobOptions{Name: "flux-dry", DryRun: true}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	final := waitStatus(t, m, view.ID, statusSucceeded, 10*time.Second)
	if _, err := os.Stat(bundleOut(cfg, "flux-dry")); !os.IsNotExist(err) {
		t.Fatalf("пробный прогон создал каталог bundle (err=%v)", err)
	}
	facts := factsOf(t, final)
	if facts["compat_verdict"] != imagemodel.CompatReady {
		t.Fatalf("пробный прогон не показал вердикт: %v", facts["compat_verdict"])
	}
	roles, _ := facts["roles"].(map[string]string)
	if len(roles) != 4 {
		t.Fatalf("в плане роли %d файлов, ожидалось 4: %v", len(roles), roles)
	}
}

func TestBundle_RoleOverrideIsApplied(t *testing.T) {
	m, cfg, _, cancel := newBundleRig(t)
	defer cancel()

	view, err := m.Create(createRequest{ModelDir: "staging/flux-set", Target: imageBundleTarget,
		Options: jobOptions{Name: "flux-override",
			Roles: map[string]string{"clip_l.gguf": types.ImageFileRoleClipG}}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Переопределение — это выбор оператора, а не «почини набор»: манифест
	// пишется с указанной ролью, а вердикт честно говорит, что clip_l не стало.
	final := waitStatus(t, m, view.ID, statusFailed, 10*time.Second)
	if final.Error == nil || final.Error.Code != "bundle_not_ready" {
		t.Fatalf("код %+v, ожидался bundle_not_ready", final.Error)
	}
	p := readProfile(t, bundleOut(cfg, "flux-override"))
	if got := roleOf(p, "clip_l.gguf"); got != types.ImageFileRoleClipG {
		t.Fatalf("переопределение роли не применилось: %q", got)
	}
	// Файл, которого в наборе нет, — отказ ДО старта, а не тихое «переопределили
	// ничего и задача прошла».
	_, err = m.Create(createRequest{ModelDir: "staging/flux-set", Target: imageBundleTarget,
		Options: jobOptions{Name: "flux-bad", Roles: map[string]string{"nope.gguf": "vae"}}})
	if err == nil {
		t.Fatal("переопределение роли для несуществующего файла принято")
	}
	if je, ok := err.(*jobError); !ok || je.Code != "invalid_request" {
		t.Fatalf("код отказа %v, ожидался invalid_request", err)
	}
}

func TestBundle_ProfileFileIsAuthority(t *testing.T) {
	m, cfg, _, cancel := newBundleRig(t)
	defer cancel()

	// Набор уже с profile.json (так выглядит HF-bundle-загрузка): роли берутся
	// из него, а не пересчитываются по заголовкам — иначе выбор оператора
	// (VAE, назначенный вручную) потерялся бы.
	src := filepath.Join(cfg.ModelsDir, "staging", "with-profile")
	writeFluxSet(t, src)
	profile := types.ImageModelProfile{
		Name: "with-profile", Family: "flux",
		Defaults: types.DefaultImageGenDefaults("flux"),
		Files: []types.ImageModelFile{
			{Role: types.ImageFileRoleDiffusion, Repo: "local", Filename: "flux1-schnell-q3_k.gguf"},
			{Role: types.ImageFileRoleVae, Repo: "local", Filename: "ae.safetensors.gguf"},
			{Role: types.ImageFileRoleClipL, Repo: "local", Filename: "clip_l.gguf"},
			{Role: types.ImageFileRoleT5xxl, Repo: "local", Filename: "t5-v1_1-xxl-encoder-Q4_K_M.gguf"},
		},
	}
	data, err := json.Marshal(profile)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, imagemodel.ProfileFileName), data, 0o644); err != nil {
		t.Fatalf("profile.json: %v", err)
	}

	view, err := m.Create(createRequest{ModelDir: "staging/with-profile", Target: imageBundleTarget,
		Options: jobOptions{Name: "with-profile-built"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	final := waitStatus(t, m, view.ID, statusSucceeded, 10*time.Second)
	if facts := factsOf(t, final); facts["roles_source"] != "profile_json" {
		t.Fatalf("роли взяты не из profile.json: %v", facts["roles_source"])
	}
	p := readProfile(t, bundleOut(cfg, "with-profile-built"))
	if p.Family != "flux" || roleOf(p, "ae.safetensors.gguf") != types.ImageFileRoleVae {
		t.Fatalf("профиль набора потерян: %+v", p)
	}
}

// ---------------- 8. HTTP-контракт цели ----------------

// TestBundle_HTTPCreateAndStatus — та же ручка, что у остальных целей: 202 и
// статус задачи с целью image_bundle. Никакого отдельного «набора» ручек:
// WebUI/скрипты знают один контракт POST/GET /api/reexport/jobs.
func TestBundle_HTTPCreateAndStatus(t *testing.T) {
	m, cfg, _, cancel := newBundleRig(t)
	defer cancel()
	srv := newTestServer(t, m, cfg)

	rec, resp := postJob(t, srv, `{"model_dir":"staging/flux-set","target":"image_bundle",`+
		`"options":{"name":"flux-http"}}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("HTTP %d, ожидался 202 (тело: %s)", rec.Code, rec.Body.String())
	}
	if resp.Error != nil {
		t.Fatalf("в успешном ответе ошибка: %+v", resp.Error)
	}
	var created jobView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("ответ не разобран: %v", err)
	}
	if created.Target != imageBundleTarget {
		t.Fatalf("target в ответе %q", created.Target)
	}
	final := waitStatus(t, m, created.ID, statusSucceeded, 10*time.Second)
	if final.Progress.Stage != "done" {
		t.Fatalf("финальная стадия %q, ожидалась done", final.Progress.Stage)
	}
	// GET /api/reexport/jobs/{id} отдаёт то же состояние (задача живёт в сервисе).
	getRec := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/api/reexport/jobs/"+created.ID, nil)
	srv.routes().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET статуса: HTTP %d", getRec.Code)
	}
	var got jobView
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("статус не разобран: %v", err)
	}
	if got.Status != statusSucceeded || got.Result == nil {
		t.Fatalf("статус задачи %+v", got)
	}
}

// TestBundle_HTTPOutputEscapeIs400 — отказ по пути приходит на POST, а не
// «фоном»: оператор должен увидеть 400 path_not_allowed сразу.
func TestBundle_HTTPOutputEscapeIs400(t *testing.T) {
	m, cfg, _, cancel := newBundleRig(t)
	defer cancel()
	srv := newTestServer(t, m, cfg)

	rec, resp := postJob(t, srv, `{"model_dir":"staging/flux-set","target":"image_bundle",`+
		`"options":{"out_path":"../escape"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("HTTP %d, ожидался 400 (тело: %s)", rec.Code, rec.Body.String())
	}
	if resp.Error == nil || resp.Error.Code != "path_not_allowed" {
		t.Fatalf("код ошибки %+v, ожидался path_not_allowed", resp.Error)
	}
}

// ---------------- вспомогательное ----------------

// imageBundleTarget — ID цели из capabilities (проверяется ещё и capabilities_test).
const imageBundleTarget = targetImageBundle

// bundleOut — каталог bundle по умолчанию: каталог image-моделей, а не корень
// тома (imageworker смотрит только туда — SDWORKER_IMAGE_MODELS_DIR).
func bundleOut(cfg config, name string) string {
	return filepath.Join(cfg.ModelsDir, "image", name)
}

// mustReadDir — размеры файлов каталога (плоского, как набор в тестах).
func mustReadDir(t *testing.T, dir string) []int64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	out := make([]int64, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			t.Fatalf("Info %s: %v", e.Name(), ierr)
		}
		out = append(out, info.Size())
	}
	return out
}

// fileSizes — размеры файлов каталога по именам.
func fileSizes(t *testing.T, dir string) map[string]int64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	out := make(map[string]int64, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			t.Fatalf("Info %s: %v", e.Name(), ierr)
		}
		out[e.Name()] = info.Size()
	}
	return out
}
