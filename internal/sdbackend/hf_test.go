package sdbackend

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Тесты HF-загрузки bundle'ов (мок HF Hub — hf_mock.go, без реальной сети)
// ============================================================
//
// Проверяются три вещи, которые нельзя проверить «по коду»:
//  1. файлы bundle попадают ИМЕННО в каталог модели, и только после полного
//     успеха появляется profile.json + модель в реестре;
//  2. НЕПОЛНЫЙ bundle не регистрируется (атомарность);
//  3. прогресс ОДНОГО файла bundle отдаётся в форме HFDownloadProgress
//     (её читает UI: status/totalBytes/downloaded/progressPct/speedBps).

// newHFTestManager — HFManager на мок-сервере + реестр на temp-каталоге.
func newHFTestManager(t *testing.T, mock *HFMockServer) (*HFManager, *Registry, string) {
	t.Helper()
	dir := t.TempDir()
	modelsDir := filepath.Join(dir, "models")
	downloadsDir := filepath.Join(dir, "downloads")

	reg := NewRegistry(modelsDir)
	if err := reg.Load(); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	cfg := DefaultConfig()
	cfg.ModelsDir = modelsDir
	cfg.DownloadsDir = downloadsDir
	cfg.HFMirror = mock.URL // зеркало = httptest-сервер: реальной сети нет

	hf, err := NewHFManager(&cfg, reg)
	if err != nil {
		t.Fatalf("NewHFManager: %v", err)
	}
	t.Cleanup(hf.Close)
	return hf, reg, modelsDir
}

// bundleSpecs — запросы bundle: role → filename (repo фиксирован).
func bundleSpecs(files map[string]string) []cppbackend.HFDownloadRequest {
	roles := make([]string, 0, len(files))
	for role := range files {
		roles = append(roles, role)
	}
	sortStringsAsc(roles)
	out := make([]cppbackend.HFDownloadRequest, 0, len(roles))
	for _, role := range roles {
		out = append(out, cppbackend.HFDownloadRequest{
			ModelID:  "acme/z-image-turbo",
			Filename: files[role],
			Revision: "main",
			Role:     role,
		})
	}
	return out
}

// waitRegistered — ждём появления модели в реестре (регистрация асинхронна
// относительно StartBundle).
func waitRegistered(t *testing.T, reg *Registry, name string) types.ImageModelProfile {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if p, ok := reg.Profile(name); ok {
			return p
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("модель %q не появилась в реестре за 20 с", name)
	return types.ImageModelProfile{}
}

// ============================================================
// 1. Успешный bundle: файлы на диске + profile.json + модель в реестре
// ============================================================

func TestHFBundle_RegistersProfileAfterSuccess(t *testing.T) {
	mock := NewHFMockServer(t)
	mock.AddFile("acme/z-image-turbo", "z_image_turbo-Q3_K.gguf", 300*1024)
	mock.AddFile("acme/z-image-turbo", "vae.safetensors", 120*1024)

	hf, reg, modelsDir := newHFTestManager(t, mock)

	files := bundleSpecs(map[string]string{
		types.ImageFileRoleDiffusion: "z_image_turbo-Q3_K.gguf",
		types.ImageFileRoleVae:       "vae.safetensors",
	})
	if err := hf.StartBundle("z-image-turbo", "other", files); err != nil {
		t.Fatalf("StartBundle: %v", err)
	}

	profile := waitRegistered(t, reg, "z-image-turbo")

	targetDir := filepath.Join(modelsDir, "z-image-turbo")
	if _, err := os.Stat(filepath.Join(targetDir, ProfileFileName)); err != nil {
		t.Fatalf("profile.json не создан: %v", err)
	}
	// Манифест bundle (атомарная регистрация cppbackend) тоже на месте.
	if !cppbackend.IsBundleRegistered(targetDir) {
		t.Fatal("манифест bundle отсутствует после успешной загрузки")
	}
	// Файлы физически скачаны в каталог bundle.
	for _, name := range []string{"z_image_turbo-Q3_K.gguf", "vae.safetensors"} {
		fi, err := os.Stat(filepath.Join(targetDir, name))
		if err != nil {
			t.Fatalf("файл %s не скачан: %v", name, err)
		}
		if fi.Size() == 0 {
			t.Fatalf("файл %s пустой", name)
		}
	}
	// Профиль валиден по контракту pkg/types и содержит все роли.
	if err := types.ValidateImageModelProfile(&profile); err != nil {
		t.Fatalf("профиль невалиден: %v", err)
	}
	if len(profile.Files) != 2 {
		t.Fatalf("files = %d, want 2", len(profile.Files))
	}
	for _, f := range profile.Files {
		if f.LocalPath == "" {
			t.Errorf("LocalPath не заполнен для роли %q", f.Role)
		}
		if fi, err := os.Stat(f.LocalPath); err != nil || fi.Size() == 0 {
			t.Errorf("LocalPath %q не существует/пуст (err=%v)", f.LocalPath, err)
		}
	}
	// Дефолты и runtime для слабых GPU.
	if profile.Defaults.Width == 0 || profile.Defaults.Steps == 0 {
		t.Errorf("defaults не заполнены: %+v", profile.Defaults)
	}
	if profile.Runtime.SeedMode != "random" {
		t.Errorf("seedMode = %q, want random (иначе все картинки одинаковые)", profile.Runtime.SeedMode)
	}
	if !profile.Runtime.VaeTiling {
		t.Error("vaeTiling должен быть включён для слабых GPU")
	}
	// Реестр перезагружен: модель видна в списке (== GET /api/image/models).
	found := false
	for _, n := range reg.Names() {
		if n == "z-image-turbo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("модель не появилась в Registry.Names(): %v", reg.Names())
	}
}

// ============================================================
// 2. НЕПОЛНЫЙ bundle: профиль НЕ создаётся, модель НЕ регистрируется
// ============================================================

func TestHFBundle_IncompleteBundleNotRegistered(t *testing.T) {
	mock := NewHFMockServer(t)
	mock.AddFile("acme/z-image-turbo", "z_image_turbo-Q3_K.gguf", 200*1024)
	// Второй файл — из несуществующего репозитория: tree отдаёт 404.
	mock.FailRepo("acme/missing-vae")

	hf, reg, modelsDir := newHFTestManager(t, mock)

	files := []cppbackend.HFDownloadRequest{
		{ModelID: "acme/z-image-turbo", Filename: "z_image_turbo-Q3_K.gguf", Role: types.ImageFileRoleDiffusion},
		{ModelID: "acme/missing-vae", Filename: "vae.safetensors", Role: types.ImageFileRoleVae},
	}
	if err := hf.StartBundle("broken-bundle", "other", files); err != nil {
		t.Fatalf("StartBundle: %v", err)
	}

	targetDir := filepath.Join(modelsDir, "broken-bundle")
	deadline := time.Now().Add(20 * time.Second)
	manifestSeen := false
	for time.Now().Before(deadline) {
		if cppbackend.IsBundleRegistered(targetDir) {
			manifestSeen = true
			break
		}
		// Первый файл уже скачан к этому моменту — bundle провалился.
		if _, err := os.Stat(filepath.Join(targetDir, "z_image_turbo-Q3_K.gguf")); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if manifestSeen {
		t.Fatal("манифест появился у НЕПОЛНОГО bundle — атомарность нарушена")
	}
	// Даём шанс ошибочной регистрации проявиться, если она есть.
	time.Sleep(300 * time.Millisecond)

	if _, err := os.Stat(filepath.Join(targetDir, ProfileFileName)); err == nil {
		t.Fatal("profile.json создан для неполного bundle — модель выглядела бы рабочей")
	}
	if p, ok := reg.Profile("broken-bundle"); ok {
		t.Fatalf("неполный bundle зарегистрирован как модель: %+v", p)
	}
	if names := reg.Names(); len(names) != 0 {
		t.Fatalf("реестр не пуст: %v", names)
	}
	// При этом успешный файл остался на диске: его подхватит resume/повтор.
	if _, err := os.Stat(filepath.Join(targetDir, "z_image_turbo-Q3_K.gguf")); err != nil {
		t.Fatalf("успешно скачанный файл удалён (resume сломан): %v", err)
	}
}

// ============================================================
// 3. Прогресс ОДНОГО файла bundle отдаётся в форме HFDownloadProgress
// ============================================================

// waitBlockedOrFail — дождаться, что мок РЕАЛЬНО начал «залипать».
//
// R-Image (2026-10-02): тесты с BlockAfter проверяют состояние «загрузка идёт»
// (progress/списки/отмена). Без этой синхронизации под параллельной нагрузкой
// можно сэмплировать состояние до старта блокировки — отсюда флейки.
func waitBlockedOrFail(t *testing.T, mock *HFMockServer) {
	t.Helper()
	if !mock.WaitBlocked(10 * time.Second) {
		t.Fatal("мок не начал блокировать передачу — стенд сломан, а не продукт")
	}
}

func TestHFBundle_FileProgressShape(t *testing.T) {
	mock := NewHFMockServer(t)
	mock.AddFile("acme/z-image-turbo", "diffusion.gguf", 3*1024*1024)
	release := mock.BlockAfter(768 * 1024)
	defer release()

	hf, _, _ := newHFTestManager(t, mock)

	files := []cppbackend.HFDownloadRequest{
		{ModelID: "acme/z-image-turbo", Filename: "diffusion.gguf", Role: types.ImageFileRoleDiffusion},
	}
	if err := hf.StartBundle("progress-bundle", "other", files); err != nil {
		t.Fatalf("StartBundle: %v", err)
	}

	// R-Image (2026-10-02): сначала дожидаемся ФАКТИЧЕСКОЙ блокировки сервера,
	// иначе под нагрузкой можно сэмплировать состояние до/после «залипания» и
	// получить ложное падение (флейк ловился в параллельном прогоне пакетов).
	if !mock.WaitBlocked(10 * time.Second) {
		t.Fatal("мок не начал блокировать передачу — стенд сломан, а не продукт")
	}

	// Ждём, пока сервер отдаст первую часть и «залипнет».
	deadline := time.Now().Add(20 * time.Second)
	var snap *cppbackend.HFDownloadProgress
	for time.Now().Before(deadline) {
		p, ok, err := hf.FileProgress("acme/z-image-turbo", "diffusion.gguf")
		if err != nil {
			t.Fatalf("FileProgress: %v", err)
		}
		if ok && p.Downloaded > 0 {
			snap = p
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if snap == nil {
		t.Fatal("прогресс файла bundle не появился (UI не увидел бы загрузку)")
	}
	if snap.Status != cppbackend.BundleFileStatusDownloading {
		t.Errorf("status = %q, want downloading", snap.Status)
	}
	if snap.TotalBytes != 3*1024*1024 {
		t.Errorf("totalBytes = %d, want %d", snap.TotalBytes, 3*1024*1024)
	}
	if snap.Filename != "diffusion.gguf" {
		t.Errorf("filename = %q", snap.Filename)
	}
	if snap.ProgressPct <= 0 || snap.ProgressPct > 100 {
		t.Errorf("progressPct = %v, want (0,100]", snap.ProgressPct)
	}

	release()
	// После завершения — completed и 100%.
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		p, ok, _ := hf.FileProgress("acme/z-image-turbo", "diffusion.gguf")
		if ok && p.Status == cppbackend.BundleFileStatusCompleted {
			if p.ProgressPct != 100 {
				t.Errorf("progressPct после завершения = %v, want 100", p.ProgressPct)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("файл bundle не дошёл до status=completed")
}

// ============================================================
// 4. Токен HF доходит до загрузчика (Authorization: Bearer <token>)
// ============================================================

func TestHFManager_TokenReachesDownloader(t *testing.T) {
	mock := NewHFMockServer(t)
	mock.AddFile("acme/z-image-turbo", "diffusion.gguf", 64*1024)

	hf, reg, _ := newHFTestManager(t, mock)
	// Имитируем заголовок X-HF-Token, который обработал HTTP-слой.
	hf.SetToken("hf_secret_token")

	files := []cppbackend.HFDownloadRequest{
		{ModelID: "acme/z-image-turbo", Filename: "diffusion.gguf", Role: types.ImageFileRoleDiffusion},
	}
	if err := hf.StartBundle("token-bundle", "other", files); err != nil {
		t.Fatalf("StartBundle: %v", err)
	}
	waitRegistered(t, reg, "token-bundle")

	if got := mock.LastAuthHeader(); got != "Bearer hf_secret_token" {
		t.Fatalf("Authorization = %q, want %q (токен не доехал до загрузчика)", got, "Bearer hf_secret_token")
	}
}

// Пустой токен не должен затирать токен из конфига: клиент без заголовка не
// «разлогинивает» воркер для параллельных запросов.
func TestHFManager_EmptyTokenKeepsConfiguredToken(t *testing.T) {
	mock := NewHFMockServer(t)
	mock.AddFile("acme/z-image-turbo", "diffusion.gguf", 32*1024)

	dir := t.TempDir()
	reg := NewRegistry(filepath.Join(dir, "models"))
	if err := reg.Load(); err != nil {
		t.Fatalf("registry: %v", err)
	}
	cfg := DefaultConfig()
	cfg.ModelsDir = filepath.Join(dir, "models")
	cfg.DownloadsDir = filepath.Join(dir, "downloads")
	cfg.HFMirror = mock.URL
	cfg.HFToken = "hf_from_config"

	hf, err := NewHFManager(&cfg, reg)
	if err != nil {
		t.Fatalf("NewHFManager: %v", err)
	}
	t.Cleanup(hf.Close)

	hf.SetToken("") // запрос без X-HF-Token
	files := []cppbackend.HFDownloadRequest{
		{ModelID: "acme/z-image-turbo", Filename: "diffusion.gguf", Role: types.ImageFileRoleDiffusion},
	}
	if err := hf.StartBundle("cfg-token-bundle", "other", files); err != nil {
		t.Fatalf("StartBundle: %v", err)
	}
	waitRegistered(t, reg, "cfg-token-bundle")

	if got := mock.LastAuthHeader(); got != "Bearer hf_from_config" {
		t.Fatalf("Authorization = %q, want токен из конфига", got)
	}
}

// ============================================================
// 5. Валидация до сети: плохой bundle отклоняется синхронно
// ============================================================

func TestHFManager_StartBundleValidation(t *testing.T) {
	mock := NewHFMockServer(t)
	hf, _, _ := newHFTestManager(t, mock)

	cases := []struct {
		name   string
		bundle string
		family string
		files  []cppbackend.HFDownloadRequest
	}{
		{"пустое имя", "", "other", []cppbackend.HFDownloadRequest{{ModelID: "a/b", Filename: "x.gguf", Role: types.ImageFileRoleDiffusion}}},
		{"имя с separator", "a/b", "other", []cppbackend.HFDownloadRequest{{ModelID: "a/b", Filename: "x.gguf", Role: types.ImageFileRoleDiffusion}}},
		{"нет файлов", "ok", "other", nil},
		{"неизвестное семейство", "ok", "banana", []cppbackend.HFDownloadRequest{{ModelID: "a/b", Filename: "x.gguf", Role: types.ImageFileRoleDiffusion}}},
		{"неизвестная роль", "ok", "other", []cppbackend.HFDownloadRequest{{ModelID: "a/b", Filename: "x.gguf", Role: "banana"}}},
		{"нет diffusion", "ok", "other", []cppbackend.HFDownloadRequest{{ModelID: "a/b", Filename: "x.gguf", Role: types.ImageFileRoleVae}}},
		{"не весовой файл", "ok", "other", []cppbackend.HFDownloadRequest{{ModelID: "a/b", Filename: "README.md", Role: types.ImageFileRoleDiffusion}}},
		{"duplicate role", "ok", "other", []cppbackend.HFDownloadRequest{
			{ModelID: "a/b", Filename: "x.gguf", Role: types.ImageFileRoleDiffusion},
			{ModelID: "a/b", Filename: "y.gguf", Role: types.ImageFileRoleDiffusion},
		}},
	}
	for _, tc := range cases {
		if err := hf.StartBundle(tc.bundle, tc.family, tc.files); err == nil {
			t.Errorf("%s: ожидалась ошибка валидации", tc.name)
		}
	}
	// Ни одна валидация не должна была дойти до сети.
	if hits := mock.ResolveHits(); hits != 0 {
		t.Errorf("сеть тронута при невалидном запросе: resolveHits = %d", hits)
	}
}

// Одиночная загрузка: файл попадает в каталог моделей воркера.
func TestHFManager_StartFileDownload(t *testing.T) {
	mock := NewHFMockServer(t)
	mock.AddFile("acme/sdxl-turbo", "sdxl_turbo.safetensors", 50*1024)

	hf, _, modelsDir := newHFTestManager(t, mock)
	progress, err := hf.StartFileDownload(cppbackend.HFDownloadRequest{
		ModelID:  "acme/sdxl-turbo",
		Filename: "sdxl_turbo.safetensors",
		Role:     types.ImageFileRoleDiffusion,
	})
	if err != nil {
		t.Fatalf("StartFileDownload: %v", err)
	}
	if progress == nil || progress.Status != "downloading" {
		t.Fatalf("progress = %+v", progress)
	}

	target := filepath.Join(modelsDir, "sdxl_turbo.safetensors")
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if fi, err := os.Stat(target); err == nil && fi.Size() == 50*1024 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("файл %s не скачан", target)
}

// Удаление последнего файла bundle снимает регистрацию: иначе реестр показывал
// бы модель без файлов на диске (Registry.Load синтезирует профиль для каталога
// с файлами, а пустой каталог помечает как «сломанную модель»).
func TestHFManager_DeleteBundleFileDropsRegistration(t *testing.T) {
	mock := NewHFMockServer(t)
	mock.AddFile("acme/z-image-turbo", "diffusion.gguf", 64*1024)

	hf, reg, _ := newHFTestManager(t, mock)
	files := []cppbackend.HFDownloadRequest{
		{ModelID: "acme/z-image-turbo", Filename: "diffusion.gguf", Role: types.ImageFileRoleDiffusion},
	}
	if err := hf.StartBundle("del-bundle", "other", files); err != nil {
		t.Fatalf("StartBundle: %v", err)
	}
	waitRegistered(t, reg, "del-bundle")

	if _, err := hf.Delete("", "del-bundle/diffusion.gguf"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := reg.Profile("del-bundle"); ok {
		t.Fatal("модель осталась в реестре после удаления последнего файла")
	}
	// Каталог переименован в «снятый с регистрации»: реестр его не видит.
	unreg := filepath.Join(hf.ModelsDir(), hfUnregisteredDirName("del-bundle"))
	if fi, err := os.Stat(unreg); err != nil || !fi.IsDir() {
		t.Fatalf("каталог не переименован в %s: %v", unreg, err)
	}
	if _, err := os.Stat(filepath.Join(hf.ModelsDir(), "del-bundle")); err == nil {
		t.Fatal("исходный каталог остался в modelsDir (реестр снова его увидит)")
	}
	// Sweep с retention=час не трогает свежий каталог.
	if n := hf.SweepUnregisteredBundles(); n != 0 {
		t.Fatalf("sweep удалил свежий каталог (n=%d)", n)
	}
}

// Path traversal в cleanup отклоняется.
func TestHFManager_DeleteRejectsTraversal(t *testing.T) {
	mock := NewHFMockServer(t)
	hf, _, _ := newHFTestManager(t, mock)
	if _, err := hf.Delete("", "../../etc/passwd"); err == nil {
		t.Fatal("path traversal не отклонён")
	}
}

// BuildImageModelProfile — чистая сборка профиля (без диска и сети).
func TestBuildImageModelProfile_FillsLocalPathAndDefaults(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "diffusion.gguf"), []byte("1234"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	profile := BuildImageModelProfile("m1", "z_image", dir, []cppbackend.HFDownloadRequest{
		{ModelID: "acme/z-image", Filename: "sub/dir/diffusion.gguf", Role: types.ImageFileRoleDiffusion, SizeBytes: 999},
		{ModelID: "acme/z-image", Filename: "vae.safetensors", Role: types.ImageFileRoleVae, SizeBytes: 555},
	}, "on")
	if err := types.ValidateImageModelProfile(profile); err != nil {
		t.Fatalf("профиль невалиден: %v", err)
	}
	var diff *types.ImageModelFile
	for i := range profile.Files {
		if profile.Files[i].Role == types.ImageFileRoleDiffusion {
			diff = &profile.Files[i]
		}
	}
	if diff == nil {
		t.Fatal("diffusion-файл потерян")
	}
	if diff.Filename != "diffusion.gguf" {
		t.Errorf("filename = %q, want basename", diff.Filename)
	}
	if diff.LocalPath != filepath.Join(dir, "diffusion.gguf") {
		t.Errorf("localPath = %q", diff.LocalPath)
	}
	if diff.SizeBytes != 4 {
		t.Errorf("sizeBytes = %d, want фактический размер 4", diff.SizeBytes)
	}
	if profile.Runtime.AutoFit != "on" {
		t.Errorf("autoFit = %q, want on", profile.Runtime.AutoFit)
	}
	if profile.Defaults.Steps != types.DefaultImageGenDefaults("z_image").Steps {
		t.Error("defaults не соответствуют семейству")
	}
}

// Контекст фоновой загрузки живёт дольше HTTP-запроса: Close отменяет активный
// pull (иначе горутина переживёт shutdown воркера).
func TestHFManager_CloseCancelsActiveBundle(t *testing.T) {
	mock := NewHFMockServer(t)
	mock.AddFile("acme/z-image-turbo", "diffusion.gguf", 3*1024*1024)
	release := mock.BlockAfter(256 * 1024)
	defer release()

	hf, _, modelsDir := newHFTestManager(t, mock)
	files := []cppbackend.HFDownloadRequest{
		{ModelID: "acme/z-image-turbo", Filename: "diffusion.gguf", Role: types.ImageFileRoleDiffusion},
	}
	if err := hf.StartBundle("close-bundle", "other", files); err != nil {
		t.Fatalf("StartBundle: %v", err)
	}
	waitBlockedOrFail(t, mock)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if mock.ResolveHits() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	hf.Close()

	if _, err := os.Stat(filepath.Join(modelsDir, "close-bundle", ProfileFileName)); err == nil {
		t.Fatal("profile.json появился после отмены загрузки")
	}
	// baseCtx отменён — новые загрузки не стартуют.
	select {
	case <-hf.baseCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("baseCtx не отменён после Close")
	}
}

// Прогресс несуществующей загрузки: (nil,false,nil) — для UI это «unknown»,
// а не ошибка (иначе опрос прекратился бы).
func TestHFManager_FileProgressUnknown(t *testing.T) {
	mock := NewHFMockServer(t)
	hf, _, _ := newHFTestManager(t, mock)
	p, ok, err := hf.FileProgress("a/b", "nope.gguf")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok || p != nil {
		t.Fatalf("ожидалось отсутствие записи, got %+v ok=%v", p, ok)
	}
	if _, _, err := hf.FileProgress("", "x"); err == nil {
		t.Fatal("пустой modelId должен быть ошибкой")
	}
}

// FileProgress ищет файл в АКТИВНОМ bundle (не только в истории).
func TestHFManager_FileProgressFindsActiveBundle(t *testing.T) {
	mock := NewHFMockServer(t)
	mock.AddFile("acme/z-image-turbo", "diffusion.gguf", 3*1024*1024)
	release := mock.BlockAfter(512 * 1024)
	defer release()

	hf, _, _ := newHFTestManager(t, mock)
	files := []cppbackend.HFDownloadRequest{
		{ModelID: "acme/z-image-turbo", Filename: "diffusion.gguf", Role: types.ImageFileRoleDiffusion},
	}
	if err := hf.StartBundle("active-bundle", "other", files); err != nil {
		t.Fatalf("StartBundle: %v", err)
	}
	waitBlockedOrFail(t, mock)
	// Состояние появляется СРАЗУ после StartBundle (не по факту старта сети):
	// UI опрашивает прогресс через ~2 с и не должен получать 404 на «идёт».
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		p, ok, err := hf.FileProgress("acme/z-image-turbo", "diffusion.gguf")
		if err != nil {
			t.Fatalf("FileProgress: %v", err)
		}
		if ok && p.Status == cppbackend.BundleFileStatusDownloading {
			if p.Downloaded <= 0 {
				// Первые байты ещё не дошли — ждём.
				time.Sleep(20 * time.Millisecond)
				continue
			}
			if p.TotalBytes != 3*1024*1024 {
				t.Fatalf("totalBytes = %d, want ожидаемый размер из tree API", p.TotalBytes)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("прогресс активного bundle не найден")
}

// ListDownloads включает bundle-загрузки (их нет в active одиночных загрузок).
func TestHFManager_ListDownloadsIncludesBundles(t *testing.T) {
	mock := NewHFMockServer(t)
	mock.AddFile("acme/z-image-turbo", "diffusion.gguf", 3*1024*1024)
	release := mock.BlockAfter(256 * 1024)
	defer release()

	hf, _, _ := newHFTestManager(t, mock)
	files := []cppbackend.HFDownloadRequest{
		{ModelID: "acme/z-image-turbo", Filename: "diffusion.gguf", Role: types.ImageFileRoleDiffusion},
	}
	if err := hf.StartBundle("list-bundle", "other", files); err != nil {
		t.Fatalf("StartBundle: %v", err)
	}
	waitBlockedOrFail(t, mock)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		snap := hf.ListDownloads()
		if len(snap.Bundles) > 0 {
			if snap.Active == nil || snap.History == nil || snap.BundleHistory == nil {
				t.Fatal("срезы состояния не должны быть nil (JSON null)")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("bundle не попал в ListDownloads")
}

// ============================================================
// Отмена bundle: терминальный статус доходит до UI, регистрации нет
// ============================================================

func TestHFManager_CancelBundleMarksFilesCancelled(t *testing.T) {
	mock := NewHFMockServer(t)
	mock.AddFile("acme/z-image-turbo", "diffusion.gguf", 3*1024*1024)
	mock.AddFile("acme/z-image-turbo", "vae.safetensors", 512*1024)
	release := mock.BlockAfter(256 * 1024)
	defer release()

	hf, reg, modelsDir := newHFTestManager(t, mock)
	files := []cppbackend.HFDownloadRequest{
		{ModelID: "acme/z-image-turbo", Filename: "diffusion.gguf", Role: types.ImageFileRoleDiffusion},
		{ModelID: "acme/z-image-turbo", Filename: "vae.safetensors", Role: types.ImageFileRoleVae},
	}
	if err := hf.StartBundle("cancel-bundle", "other", files); err != nil {
		t.Fatalf("StartBundle: %v", err)
	}
	waitBlockedOrFail(t, mock)
	// Ждём начала передачи, чтобы отмена пришлась на активную загрузку.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && mock.ResolveHits() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if mock.ResolveHits() == 0 {
		t.Fatal("загрузка не началась")
	}

	if err := hf.Cancel("cancel-bundle", "", ""); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	// Терминальный статус должен быть виден UI (иначе опрос не остановится).
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		p, err := hf.BundleProgress("cancel-bundle")
		if err == nil && p.Status != "downloading" {
			if p.Status != "cancelled" && p.Status != "failed" {
				t.Fatalf("status = %q, want cancelled/failed", p.Status)
			}
			for _, f := range p.Files {
				if f.Status == cppbackend.BundleFileStatusPending {
					t.Fatalf("файл %s остался в pending после отмены", f.Filename)
				}
			}
			// Отменённый bundle не регистрируется как модель.
			if _, ok := reg.Profile("cancel-bundle"); ok {
				t.Fatal("отменённый bundle зарегистрирован как модель")
			}
			if _, err := os.Stat(filepath.Join(modelsDir, "cancel-bundle", ProfileFileName)); err == nil {
				t.Fatal("profile.json создан для отменённого bundle")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("статус bundle не стал терминальным после отмены")
}

// Отмена неизвестного bundle — понятная ошибка (хендлер отдаёт 404).
func TestHFManager_CancelUnknownBundle(t *testing.T) {
	mock := NewHFMockServer(t)
	hf, _, _ := newHFTestManager(t, mock)
	if err := hf.Cancel("nope", "", ""); err == nil {
		t.Fatal("ожидалась ошибка для неизвестного bundle")
	}
	if err := hf.Cancel("", "", ""); err == nil {
		t.Fatal("ожидалась ошибка без modelId/bundleId")
	}
}

// Cleanup: удаление частичного файла bundle освобождает место, а удаление
// активной загрузки предварительно её останавливает (на Windows открытый
// .download-файл иначе не удалить).
func TestHFManager_DeletePartialBundleFileRemovesTemp(t *testing.T) {
	// Сетевой обрыв (а не отмена): cppbackend сохраняет partial для resume —
	// только такой .download и попадает в cleanup на боевом стенде.
	mock := NewHFMockServer(t)
	mock.AddFile("acme/z-image-turbo", "diffusion.gguf", 3*1024*1024)
	mock.AbortAfter(256 * 1024)

	hf, _, modelsDir := newHFTestManager(t, mock)
	files := []cppbackend.HFDownloadRequest{
		{ModelID: "acme/z-image-turbo", Filename: "diffusion.gguf", Role: types.ImageFileRoleDiffusion},
	}
	if err := hf.StartBundle("partial-bundle", "other", files); err != nil {
		t.Fatalf("StartBundle: %v", err)
	}
	// NB: здесь AbortAfter (обрыв соединения), а не BlockAfter — ждать
	// WaitBlocked нельзя: сервер не «залипает», а рвёт передачу.

	// Ждём, пока .download реально появится и наполнится.
	tempPath := filepath.Join(modelsDir, "partial-bundle", "diffusion.gguf.download")
	deadline := time.Now().Add(20 * time.Second)
	partialSize := int64(0)
	for time.Now().Before(deadline) {
		if fi, err := os.Stat(tempPath); err == nil && fi.Size() > 0 {
			partialSize = fi.Size()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if partialSize == 0 {
		t.Fatal("частичный .download-файл не появился (нечего проверять)")
	}

	// Файл должен быть виден UI как interrupted/resumable.
	if p, ok, _ := hf.FileProgress("acme/z-image-turbo", "diffusion.gguf"); ok {
		if !p.Resumable {
			t.Errorf("resumable = false при сохранённом partial: %+v", p)
		}
	}

	res, err := hf.Delete("", "partial-bundle/diffusion.gguf")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !res.TempDeleted {
		t.Error("tempDeleted = false: .download не удалён (место не освобождено)")
	}
	if res.BytesFreed <= 0 {
		t.Error("bytesFreed = 0")
	}
	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Errorf(".download остался на диске: %v", err)
	}
}

// Cleanup во время АКТИВНОЙ загрузки: лок загрузчика снимается (файл удаляется
// либо самим загрузчиком при отмене, либо нашим os.Remove — в обоих случаях без
// ошибки «файл занят» на Windows).
func TestHFManager_DeleteActiveBundleFile(t *testing.T) {
	mock := NewHFMockServer(t)
	mock.AddFile("acme/z-image-turbo", "diffusion.gguf", 3*1024*1024)
	release := mock.BlockAfter(256 * 1024)
	defer release()

	hf, _, modelsDir := newHFTestManager(t, mock)
	files := []cppbackend.HFDownloadRequest{
		{ModelID: "acme/z-image-turbo", Filename: "diffusion.gguf", Role: types.ImageFileRoleDiffusion},
	}
	if err := hf.StartBundle("active-del-bundle", "other", files); err != nil {
		t.Fatalf("StartBundle: %v", err)
	}
	waitBlockedOrFail(t, mock)
	tempPath := filepath.Join(modelsDir, "active-del-bundle", "diffusion.gguf.download")
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if fi, err := os.Stat(tempPath); err == nil && fi.Size() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Удаление обязано само остановить активную загрузку: без этого на Windows
	// os.Remove вернул бы «The process cannot access the file».
	res, err := hf.Delete("", "active-del-bundle/diffusion.gguf")
	if err != nil {
		// Ошибки «файла нет» быть не должно — но если загрузчик сам убрал темп
		// (штатная отмена), Delete вернёт именно её и это тоже корректный итог
		// для пользователя: место уже освобождено.
		if !strings.Contains(err.Error(), "no file found") {
			t.Fatalf("Delete активной загрузки: %v", err)
		}
	} else if !res.TempDeleted && !res.FinalDeleted {
		t.Fatalf("Delete вернул пустой результат: %+v", res)
	}
	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Fatalf(".download остался после cleanup: %v", err)
	}
	// Прогресс после cleanup должен быть терминальным (UI не должен крутить опрос).
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		p, err := hf.BundleProgress("active-del-bundle")
		if err == nil && p.Status != "downloading" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("bundle остался в статусе downloading после cleanup")
}

// ============================================================
// Реестр: безопасная перезагрузка во время работы
// ============================================================

// Load идемпотентен и обновляет снимок: модель, появившаяся на диске после
// старта, видна без рестарта (сценарий Phase 4).
func TestRegistry_ReloadPicksUpNewModel(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry(dir)
	if err := reg.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(reg.Names()) != 0 {
		t.Fatalf("реестр не пуст: %v", reg.Names())
	}

	sub := filepath.Join(dir, "new-model")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "diffusion.gguf"), []byte("xx"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := reg.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := reg.Profile("new-model"); !ok {
		t.Fatalf("модель не подхвачена после reload: %v", reg.Names())
	}
}

// Параллельные Load и чтение не должны падать с «concurrent map read and map
// write»: именно этот сценарий возникает при HF-загрузке во время опроса
// /api/image/models.
func TestRegistry_ConcurrentLoadAndRead(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		sub := filepath.Join(dir, name)
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(sub, "diffusion.gguf"), []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	reg := NewRegistry(dir)
	if err := reg.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_ = reg.Load()
		}
	}()
	for i := 0; i < 400; i++ {
		_ = reg.Names()
		_ = reg.Profiles()
		_, _ = reg.Profile("a")
		_ = reg.Warnings()
	}
	<-done
}
