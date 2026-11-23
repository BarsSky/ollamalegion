// components_r710_test.go — R710 (2026-11-24): каталог компонентов и добавление
// недостающего компонента в СКАЧАННЫЙ набор.
//
// ЖИВОЙ ДЕФЕКТ, который проверяется (скриншоты пользователя): вердикт говорит
// «не хватает: VAE, text encoder (LLM)», в репозитории этих файлов нет, а
// добавить их из UI было нельзя. Здесь проверяется, что каталог знает реальные
// репозитории/файлы/размеры, а установка (1) скачивает файл в каталог bundle,
// (2) прописывает роль в profile.json, (3) не трогает существующие файлы,
// (4) не качает заново то, что уже лежит, (5) отказывает кодами, а не текстом.
//
// ЧЕГО ЗДЕСЬ НЕТ (сознательно): проверки «в интернете есть такой репозиторий».
// Сеть в тестах не нужна и не должна быть нужна — существование репозиториев
// проверено живым прогоном (отчёт R710) и живёт в комментарии к таблице
// componentCatalog. Здесь HF подменён моком (hf_mock.go) — проверяется механика.
package sdbackend

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// (а) Каталог: default + size по семейству
// ============================================================

// TestComponentCatalog_DefaultsAndSizes_R710 — каталог отвечает по семейству,
// в записях есть default и размер, сортировка — по приоритету.
func TestComponentCatalog_DefaultsAndSizes_R710(t *testing.T) {
	want := []string{"chroma", "flux", "flux2", "qwen_image", "sd15", "sd3", "sdxl", "z_image"}
	got := ComponentCatalogFamilies()
	if len(got) != len(want) {
		t.Fatalf("семейств в каталоге %d (%v), хотим %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("семейства каталога: got %v, want %v", got, want)
		}
	}

	// Для каждого семейства: записи полные, размер положительный, и на каждую
	// роль ровно ОДИН default (иначе кнопка «по умолчанию» была бы неоднозначной).
	requiredRoles := map[string][]string{
		"qwen_image": {types.ImageFileRoleVae, types.ImageFileRoleLLM},
		"flux":       {types.ImageFileRoleVae, types.ImageFileRoleT5xxl, types.ImageFileRoleClipL},
		"flux2":      {types.ImageFileRoleVae, types.ImageFileRoleLLM},
		"sd3":        {types.ImageFileRoleVae, types.ImageFileRoleT5xxl, types.ImageFileRoleClipL, types.ImageFileRoleClipG},
		"z_image":    {types.ImageFileRoleVae, types.ImageFileRoleLLM},
		"chroma":     {types.ImageFileRoleVae, types.ImageFileRoleT5xxl},
		"sdxl":       {types.ImageFileRoleVae},
		"sd15":       {types.ImageFileRoleVae},
	}
	for family, roles := range requiredRoles {
		list := ComponentCatalog(family, nil)
		if len(list) == 0 {
			t.Fatalf("семейство %q: каталог пуст", family)
		}
		defaults := map[string]int{}
		for _, c := range list {
			if c.Family != family {
				t.Errorf("%s/%s: family=%q, want %q", family, c.Role, c.Family, family)
			}
			if c.Repo == "" || len(c.Files) == 0 {
				t.Errorf("%s/%s: пустой repo/files (%+v)", family, c.Role, c)
			}
			if c.SizeMB <= 0 {
				t.Errorf("%s/%s: sizeMb=%d — размер обязателен (UI показывает его в подтверждении)", family, c.Role, c.SizeMB)
			}
			if !IsComponentRole(c.Role) {
				t.Errorf("%s: роль %q не компонентная", family, c.Role)
			}
			if c.Label == "" {
				t.Errorf("%s/%s: пустой label", family, c.Role)
			}
			if c.Default {
				defaults[c.Role]++
			}
		}
		for _, role := range roles {
			d, ok := DefaultComponent(family, role)
			if !ok {
				t.Errorf("%s: нет default-компонента для роли %q", family, role)
				continue
			}
			if d.SizeMB <= 0 || d.Repo == "" {
				t.Errorf("%s/%s: default без размера/репозитория: %+v", family, role, d)
			}
			if defaults[role] != 1 {
				t.Errorf("%s/%s: default-компонентов %d, want ровно 1", family, role, defaults[role])
			}
		}
	}

	// Фильтр по ролям: только запрошенное, и default — первым.
	onlyVae := ComponentCatalog("qwen_image", []string{types.ImageFileRoleVae})
	if len(onlyVae) != 2 {
		t.Fatalf("qwen_image/vae: %d записей, want 2 (default + альтернатива)", len(onlyVae))
	}
	for _, c := range onlyVae {
		if c.Role != types.ImageFileRoleVae {
			t.Fatalf("фильтр по роли не работает: %q", c.Role)
		}
	}
	if !onlyVae[0].Default {
		t.Fatal("default-компонент обязан быть ПЕРВЫМ: UI берёт первый для кнопки «по умолчанию»")
	}
	if onlyVae[0].SizeMB > onlyVae[1].SizeMB {
		t.Fatalf("внутри роли меньший размер должен идти первым: %d > %d", onlyVae[0].SizeMB, onlyVae[1].SizeMB)
	}

	// Неизвестное семейство — ПУСТОЙ, но не nil каталог и known=false.
	for _, family := range []string{"other", "", "wan", "pixart"} {
		list := ComponentCatalog(family, nil)
		if list == nil {
			t.Fatalf("семейство %q: каталог nil, нужен пустой срез (JSON-контракт UI)", family)
		}
		if len(list) != 0 {
			t.Fatalf("семейство %q: каталог не пуст (%d записей)", family, len(list))
		}
		if ComponentCatalogKnown(family) {
			t.Fatalf("семейство %q: known=true, а записей нет", family)
		}
		if _, ok := DefaultComponent(family, types.ImageFileRoleVae); ok {
			t.Fatalf("семейство %q: default-компонент нашёлся там, где каталога нет", family)
		}
	}

	// diffusion в каталоге быть не может: это сама модель.
	for _, family := range ComponentCatalogFamilies() {
		for _, c := range ComponentCatalog(family, nil) {
			if c.Role == types.ImageFileRoleDiffusion {
				t.Fatalf("%s: каталог предлагает diffusion как компонент", family)
			}
		}
	}
	if IsComponentRole(types.ImageFileRoleDiffusion) {
		t.Fatal("diffusion не должен считаться добавляемой ролью")
	}
}

// TestBundleCompat_RequiredComponentRoles_R710 — вердикт сам называет роли,
// которых не хватает (по ним UI ищет кнопки в каталоге).
func TestBundleCompat_RequiredComponentRoles_R710(t *testing.T) {
	_, _, compat := loadBundle(t, "qwen-edit-2511", map[string][]byte{
		"qwen-edit-2511-Q4_K_M.gguf": qwenDiffusionGGUF(t),
	})
	if compat.Verdict != CompatIncomplete {
		t.Fatalf("вердикт %q, want %q (diffusion без VAE/TE)", compat.Verdict, CompatIncomplete)
	}
	roles := compat.RequiredComponentRoles()
	if len(roles) != 2 || roles[0] != types.ImageFileRoleVae || roles[1] != types.ImageFileRoleLLM {
		t.Fatalf("недостающие роли = %v, want [vae llm]", roles)
	}
	for _, role := range roles {
		if _, ok := DefaultComponent(compat.Family, role); !ok {
			t.Fatalf("для роли %q нет default-компонента в семействе %q — кнопка «по умолчанию» не появится", role, compat.Family)
		}
	}
}

// ============================================================
// (б) Установка: роль прописана, существующие файлы не тронуты
// ============================================================

// componentFixture — каталог bundle с одним diffusion-файлом внутри modelsDir
// тестового HFManager (без profile.json — как «просто положили .gguf»).
func componentFixture(t *testing.T, mock *HFMockServer, bundle string) (*HFManager, *Registry, string, map[string]string) {
	t.Helper()
	hf, reg, modelsDir := newHFTestManager(t, mock)
	dir := filepath.Join(modelsDir, bundle)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir bundle: %v", err)
	}
	writeBundleFile(t, dir, bundle+"-Q4_K_M.gguf", qwenDiffusionGGUF(t))
	if err := reg.Load(); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	hashes := fileHashes(t, dir)
	return hf, reg, dir, hashes
}

// fileHashes — sha256 всех файлов каталога (для «до/после»).
func fileHashes(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		sum := sha256.Sum256(data)
		out[e.Name()] = hex.EncodeToString(sum[:])
	}
	return out
}

// waitComponentDone — ждём терминального статуса установки компонента.
func waitComponentDone(t *testing.T, hf *HFManager, trackingID string) *cppbackend.HFBundleProgress {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		p, err := hf.BundleProgress(trackingID)
		if err == nil && p != nil && p.Status != "downloading" {
			return p
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("установка компонента %q не завершилась за 30 с", trackingID)
	return nil
}

func TestComponentInstall_WritesRoleKeepsExistingFiles_R710(t *testing.T) {
	mock := NewHFMockServer(t)
	mock.AddFile("acme/qwen-image", "vae/qwen_image_vae.safetensors", 96*1024)
	hf, reg, bundleDir, before := componentFixture(t, mock, "qwen-edit-2511")

	compatBefore := reg.Compat("qwen-edit-2511")
	if compatBefore == nil || compatBefore.Verdict != CompatIncomplete {
		t.Fatalf("подготовка: вердикт %v, want incomplete", compatBefore)
	}

	install, err := hf.StartComponentInstall(ComponentInstallRequest{
		Bundle:  "qwen-edit-2511",
		Role:    types.ImageFileRoleVae,
		Repo:    "acme/qwen-image",
		Files:   []string{"vae/qwen_image_vae.safetensors"},
		Default: true,
	})
	if err != nil {
		t.Fatalf("StartComponentInstall: %v", err)
	}
	if install.TrackingID == "" || install.ProgressURL == "" {
		t.Fatalf("нет trackingId/progressUrl в ответе: %+v", install)
	}
	if install.TotalBytes != 96*1024 {
		t.Fatalf("totalBytes = %d, want %d (размер из HF tree)", install.TotalBytes, 96*1024)
	}
	if install.License == "" {
		// acme/* нет в каталоге — лицензия неизвестна, и это нормально: поле пустое.
		t.Log("лицензия не определена для репозитория вне каталога — ок")
	}

	progress := waitComponentDone(t, hf, install.TrackingID)
	if progress.Status != "completed" {
		t.Fatalf("статус установки %q (%s), want completed", progress.Status, progress.ErrorMessage)
	}

	// 1. Файл скачан в каталог bundle ПОД BASENAME (без подкаталога vae/).
	local := filepath.Join(bundleDir, "qwen_image_vae.safetensors")
	if fi, err := os.Stat(local); err != nil || fi.Size() != 96*1024 {
		t.Fatalf("файл компонента не скачан: %v", err)
	}

	// 2. Роль прописана в profile.json, путь — рабочий.
	if err := reg.Load(); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	profile, ok := reg.Profile("qwen-edit-2511")
	if !ok {
		t.Fatal("модель пропала из реестра после добавления компонента")
	}
	var vae *types.ImageModelFile
	roles := map[string]string{}
	for i := range profile.Files {
		roles[profile.Files[i].Role] = profile.Files[i].Filename
		if profile.Files[i].Role == types.ImageFileRoleVae {
			vae = &profile.Files[i]
		}
	}
	if vae == nil {
		t.Fatalf("роль vae не прописана в профиль: %v", roles)
	}
	if vae.Filename != "qwen_image_vae.safetensors" || vae.LocalPath == "" {
		t.Fatalf("запись vae некорректна: %+v", *vae)
	}
	if roles[types.ImageFileRoleDiffusion] != "qwen-edit-2511-Q4_K_M.gguf" {
		t.Fatalf("diffusion-роль потеряна/переименована: %v", roles)
	}

	// 3. Форма profile.json на диске — валидный JSON с обоими файлами.
	raw, err := os.ReadFile(filepath.Join(bundleDir, ProfileFileName))
	if err != nil {
		t.Fatalf("profile.json не записан: %v", err)
	}
	var onDisk types.ImageModelProfile
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("profile.json не парсится: %v", err)
	}
	if len(onDisk.Files) != 2 {
		t.Fatalf("files в profile.json = %d, want 2", len(onDisk.Files))
	}

	// 4. СУЩЕСТВУЮЩИЕ ФАЙЛЫ НЕ ТРОНУТЫ: хэши диффузии и остальных файлов совпадают.
	after := fileHashes(t, bundleDir)
	for name, sum := range before {
		if after[name] != sum {
			t.Fatalf("файл %q изменился после добавления компонента (%s → %s)", name, sum, after[name])
		}
	}
	// Новых файлов ровно два: сам компонент и profile.json (его не было — установка
	// фиксирует синтезированный профиль до загрузки), и НИ ОДНОГО .download:
	// темп загрузчика обязан исчезать (иначе он копится в каталоге bundle и
	// попадает в orphan-отчёт как «осиротевший темп»).
	newFiles := make([]string, 0, 3)
	for name := range after {
		if _, existed := before[name]; !existed {
			newFiles = append(newFiles, name)
		}
	}
	sortStringsAsc(newFiles)
	wantNew := []string{"profile.json", "qwen_image_vae.safetensors"}
	if len(newFiles) != len(wantNew) || newFiles[0] != wantNew[0] || newFiles[1] != wantNew[1] {
		t.Fatalf("новые файлы = %v, want %v", newFiles, wantNew)
	}

	// 5. ВЕРДИКТ ПЕРЕСЧИТАН: VAE больше не «не хватает», следующий шаг — энкодер.
	compatAfter := reg.Compat("qwen-edit-2511")
	if compatAfter == nil {
		t.Fatal("вердикт не пересчитан")
	}
	for _, r := range compatAfter.Roles {
		if r.Role == types.ImageFileRoleVae && !r.Found {
			t.Fatalf("вердикт всё ещё считает VAE отсутствующим: %+v", compatAfter.Roles)
		}
	}
	if got := compatAfter.RequiredComponentRoles(); len(got) != 1 || got[0] != types.ImageFileRoleLLM {
		t.Fatalf("следующий шаг = %v, want [llm] (VAE добавлен, энкодера нет)", got)
	}
	if compatAfter.Verdict != CompatIncomplete {
		t.Fatalf("вердикт = %q, want incomplete (не хватает энкодера)", compatAfter.Verdict)
	}
}

// ============================================================
// (в) Идемпотентность: повтор не качает заново
// ============================================================

// TestComponentInstall_DoesNotRedownloadPresentFile_R710 — файл уже лежит в
// bundle (роль ещё не прописана): скачивания нет, запись роли есть.
//
// ПРОФИЛЬ ПИШЕМ ЯВНО: без profile.json синтез назначил бы роль vae уже по имени
// файла (qwen_image_vae.safetensors), и установка честно ответила бы 409 «роль
// занята». Сценарий дефекта — другой: файл скачан/скопирован, но роль в профиле
// не прописана (профиль от старой версии воркера или правка руками).
func TestComponentInstall_DoesNotRedownloadPresentFile_R710(t *testing.T) {
	mock := NewHFMockServer(t)
	mock.AddFile("acme/qwen-image", "vae/qwen_image_vae.safetensors", 40*1024)
	hf, reg, bundleDir, _ := componentFixture(t, mock, "qwen-edit-2511")

	base, err := SynthesizeProfile("qwen-edit-2511", bundleDir)
	if err != nil {
		t.Fatalf("SynthesizeProfile: %v", err)
	}
	if err := persistBundleProfile(bundleDir, base); err != nil {
		t.Fatalf("persist profile: %v", err)
	}

	// Файл уже на диске (скачан вручную/другой версией воркера), в профиле его нет.
	present := make([]byte, 40*1024)
	for i := range present {
		present[i] = byte(i % 251)
	}
	writeBundleFile(t, bundleDir, "qwen_image_vae.safetensors", present)

	install, err := hf.StartComponentInstall(ComponentInstallRequest{
		Bundle: "qwen-edit-2511",
		Role:   types.ImageFileRoleVae,
		Repo:   "acme/qwen-image",
		Files:  []string{"vae/qwen_image_vae.safetensors"},
	})
	if err != nil {
		t.Fatalf("StartComponentInstall: %v", err)
	}
	if len(install.Reused) != 1 || install.Reused[0] != "qwen_image_vae.safetensors" {
		t.Fatalf("reused = %v, want [qwen_image_vae.safetensors]", install.Reused)
	}
	if !install.Expected[0].Present {
		t.Fatalf("expected[0].present = false, файл же на диске")
	}
	waitComponentDone(t, hf, install.TrackingID)
	if hits := mock.ResolveHits(); hits != 0 {
		t.Fatalf("файл скачивался %d раз, хотя уже был на диске (повтор не должен качать)", hits)
	}
	if err := reg.Load(); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	profile, _ := reg.Profile("qwen-edit-2511")
	found := false
	for _, f := range profile.Files {
		if f.Role == types.ImageFileRoleVae && f.Filename == "qwen_image_vae.safetensors" {
			found = true
		}
	}
	if !found {
		t.Fatal("роль vae не прописана, хотя файл уже был на диске")
	}
	// Содержимое «уже лежащего» файла не перезаписано.
	data, err := os.ReadFile(filepath.Join(bundleDir, "qwen_image_vae.safetensors"))
	if err != nil || len(data) != len(present) || data[0] != present[0] {
		t.Fatalf("существующий файл перезаписан: err=%v len=%d", err, len(data))
	}
}

// ============================================================
// (г) 409 role_already_present
// ============================================================

func TestComponentInstall_RoleAlreadyPresent_R710(t *testing.T) {
	mock := NewHFMockServer(t)
	mock.AddFile("acme/qwen-image", "vae/qwen_image_vae.safetensors", 8*1024)
	hf, _, bundleDir, _ := componentFixture(t, mock, "qwen-edit-2511")

	// Роль уже занята файлом на диске (как после первой успешной установки).
	writeBundleFile(t, bundleDir, "existing_vae.safetensors", make([]byte, 8*1024))
	profile, err := SynthesizeProfile("qwen-edit-2511", bundleDir)
	if err != nil {
		t.Fatalf("SynthesizeProfile: %v", err)
	}
	profile.Files = append(profile.Files, types.ImageModelFile{
		Role: types.ImageFileRoleVae, Repo: "local",
		Filename:  "existing_vae.safetensors",
		LocalPath: filepath.Join(bundleDir, "existing_vae.safetensors"),
		SizeBytes: 8 * 1024,
	})
	if err := persistBundleProfile(bundleDir, profile); err != nil {
		t.Fatalf("persist profile: %v", err)
	}

	resolveBefore := mock.ResolveHits()
	_, err = hf.StartComponentInstall(ComponentInstallRequest{
		Bundle: "qwen-edit-2511",
		Role:   types.ImageFileRoleVae,
		Repo:   "acme/qwen-image",
		Files:  []string{"vae/qwen_image_vae.safetensors"},
	})
	var ce *ComponentError
	if !errors.As(err, &ce) {
		t.Fatalf("ошибка %v (%T), want *ComponentError", err, err)
	}
	if ce.Code != CompErrRolePresent {
		t.Fatalf("код %q, want %q", ce.Code, CompErrRolePresent)
	}
	if ce.CurrentFile != "existing_vae.safetensors" {
		t.Fatalf("currentFile = %q, want existing_vae.safetensors (UI показывает, что уже стоит)", ce.CurrentFile)
	}
	if hits := mock.ResolveHits(); hits != resolveBefore {
		t.Fatalf("отказ 409 сходил в сеть за файлом (%d → %d)", resolveBefore, hits)
	}
}

// ============================================================
// (д) Путь вне каталога моделей отвергается
// ============================================================

func TestComponentInstall_RejectsPathOutsideModels_R710(t *testing.T) {
	mock := NewHFMockServer(t)
	mock.AddFile("acme/qwen-image", "vae.safetensors", 4*1024)
	hf, _, modelsDir, _ := componentFixture(t, mock, "qwen-edit-2511")

	outside := filepath.Join(filepath.Dir(modelsDir), "outside-bundle")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	cases := []struct {
		name     string
		modelDir string
		wantCode string
	}{
		{"абсолютный путь наружу", outside, CompErrInvalidPath},
		{"относительный путь наружу", filepath.Join("..", "outside-bundle"), CompErrInvalidPath},
		{"traversal внутри имени", filepath.Join(modelsDir, "..", "outside-bundle"), CompErrInvalidPath},
		{"чужой каталог внутри models", filepath.Join(modelsDir, "another-model"), CompErrInvalidPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := hf.StartComponentInstall(ComponentInstallRequest{
				Bundle: "qwen-edit-2511", Role: types.ImageFileRoleVae,
				Repo: "acme/qwen-image", Files: []string{"vae.safetensors"},
				ModelDir: tc.modelDir,
			})
			var ce *ComponentError
			if !errors.As(err, &ce) {
				t.Fatalf("ошибка %v (%T), want *ComponentError с кодом %q", err, err, tc.wantCode)
			}
			if ce.Code != tc.wantCode {
				t.Fatalf("код %q, want %q (%s)", ce.Code, tc.wantCode, ce.Message)
			}
		})
	}
	// Ничего не скачано в каталог снаружи.
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatalf("readdir outside: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("в каталоге снаружи появились файлы: %d", len(entries))
	}
}

// ============================================================
// Прочие отказы: мимо роли, файла нет, репозиторий недоступен, лимит
// ============================================================

func TestComponentInstall_RejectsDiffusionRole_R710(t *testing.T) {
	mock := NewHFMockServer(t)
	hf, _, _, _ := componentFixture(t, mock, "qwen-edit-2511")
	_, err := hf.StartComponentInstall(ComponentInstallRequest{
		Bundle: "qwen-edit-2511", Role: types.ImageFileRoleDiffusion,
		Repo: "acme/qwen-image", Files: []string{"diffusion.gguf"},
	})
	var ce *ComponentError
	if !errors.As(err, &ce) || ce.Code != CompErrRoleInvalid {
		t.Fatalf("ошибка %v, want код %q (diffusion — это сама модель, а не компонент)", err, CompErrRoleInvalid)
	}
}

func TestComponentInstall_FileNotFound_R710(t *testing.T) {
	mock := NewHFMockServer(t)
	mock.AddFile("acme/qwen-image", "vae/other_vae.safetensors", 1024)
	hf, _, _, _ := componentFixture(t, mock, "qwen-edit-2511")
	_, err := hf.StartComponentInstall(ComponentInstallRequest{
		Bundle: "qwen-edit-2511", Role: types.ImageFileRoleVae,
		Repo: "acme/qwen-image", Files: []string{"vae/qwen_image_vae.safetensors"},
	})
	var ce *ComponentError
	if !errors.As(err, &ce) || ce.Code != CompErrFileNotFound {
		t.Fatalf("ошибка %v, want код %q", err, CompErrFileNotFound)
	}
	if len(ce.Available) == 0 {
		t.Fatal("available пуст — UI не покажет, что вообще есть в репозитории")
	}
	if ce.Available[0] != "vae/other_vae.safetensors" {
		t.Fatalf("available = %v, want [vae/other_vae.safetensors]", ce.Available)
	}
}

func TestComponentInstall_RepoTasksMissing_R710(t *testing.T) {
	mock := NewHFMockServer(t)
	mock.FailRepo("acme/missing-repo")
	hf, _, _, _ := componentFixture(t, mock, "qwen-edit-2511")
	_, err := hf.StartComponentInstall(ComponentInstallRequest{
		Bundle: "qwen-edit-2511", Role: types.ImageFileRoleVae,
		Repo: "acme/missing-repo", Files: []string{"vae.safetensors"},
	})
	var ce *ComponentError
	if !errors.As(err, &ce) || ce.Code != CompErrRepoUnavailable {
		t.Fatalf("ошибка %v, want код %q", err, CompErrRepoUnavailable)
	}
}

func TestComponentInstall_TooLarge_R710(t *testing.T) {
	mock := NewHFMockServer(t)
	// Объявляем размер больше лимита: отказ обязан случиться ДО скачивания.
	mock.AddFile("acme/qwen-image", "huge_encoder.safetensors", int64(ComponentInstallMaxSizeMB+512)*1024*1024)
	hf, _, bundleDir, _ := componentFixture(t, mock, "qwen-edit-2511")
	_, err := hf.StartComponentInstall(ComponentInstallRequest{
		Bundle: "qwen-edit-2511", Role: types.ImageFileRoleLLM,
		Repo: "acme/qwen-image", Files: []string{"huge_encoder.safetensors"},
	})
	var ce *ComponentError
	if !errors.As(err, &ce) || ce.Code != CompErrTooLarge {
		t.Fatalf("ошибка %v, want код %q", err, CompErrTooLarge)
	}
	if _, statErr := os.Stat(filepath.Join(bundleDir, "huge_encoder.safetensors")); !os.IsNotExist(statErr) {
		t.Fatal("огромный файл всё-таки начал качаться")
	}
}

func TestComponentInstall_UnknownBundle_R710(t *testing.T) {
	mock := NewHFMockServer(t)
	hf, _, _ := newHFTestManager(t, mock)
	_, err := hf.StartComponentInstall(ComponentInstallRequest{
		Bundle: "no-such-model", Role: types.ImageFileRoleVae,
		Repo: "acme/qwen-image", Files: []string{"vae.safetensors"},
	})
	var ce *ComponentError
	if !errors.As(err, &ce) || ce.Code != CompErrBundleNotFound {
		t.Fatalf("ошибка %v, want код %q", err, CompErrBundleNotFound)
	}
}

// TestComponentInstall_SidecarUpdated_R710 — sd-server.config.json (запись «что
// запускали») дополняется, а не перезаписывается.
func TestComponentInstall_SidecarUpdated_R710(t *testing.T) {
	mock := NewHFMockServer(t)
	mock.AddFile("acme/qwen-image", "vae.safetensors", 2*1024)
	hf, reg, bundleDir, _ := componentFixture(t, mock, "qwen-edit-2511")

	sidecar := `{"model":"qwen-edit-2511","family":"qwen_image","seed_mode":"random"}`
	if err := os.WriteFile(filepath.Join(bundleDir, "sd-server.config.json"), []byte(sidecar), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	install, err := hf.StartComponentInstall(ComponentInstallRequest{
		Bundle: "qwen-edit-2511", Role: types.ImageFileRoleVae,
		Repo: "acme/qwen-image", Files: []string{"vae.safetensors"},
	})
	if err != nil {
		t.Fatalf("StartComponentInstall: %v", err)
	}
	waitComponentDone(t, hf, install.TrackingID)
	if err := reg.Load(); err != nil {
		t.Fatalf("registry load: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(bundleDir, "sd-server.config.json"))
	if err != nil {
		t.Fatalf("sidecar пропал: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("sidecar не парсится: %v", err)
	}
	if doc["model"] != "qwen-edit-2511" || doc["seed_mode"] != "random" {
		t.Fatalf("существующие поля sidecar потеряны: %v", doc)
	}
	components, ok := doc["components"].([]any)
	if !ok || len(components) != 1 {
		t.Fatalf("sidecar.components = %v, want 1 запись", doc["components"])
	}
	entry, _ := components[0].(map[string]any)
	if entry["role"] != types.ImageFileRoleVae || entry["repo"] != "acme/qwen-image" {
		t.Fatalf("запись компонента в sidecar некорректна: %v", entry)
	}
}
