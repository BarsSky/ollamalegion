// model_files_test.go — R751 (2026-11-25): управление отдельными файлами группы.
//
// ЧТО ПРОВЕРЯЕТСЯ (и почему именно это):
//  1. ИНВЕНТАРЬ: GET /api/image/models обязан говорить по каждому файлу, есть он
//     на диске или нет, сколько занимает и сколько лежит в `.download`, а весовые
//     файлы вне профиля — показывать отдельными строками. Живой дефект: удалённый
//     файл оставался «найденным» в профиле, вердикт — «набор полный», загрузка
//     падала на движке.
//  2. ВЕРДИКТ СМОТРИТ НА ДИСК: после удаления файла роль обязана стать
//     отсутствующей (incomplete), даже если profile.json ещё не переписан —
//     это страховка на случай удаления вручную.
//  3. УСТАНОВКА В ГРУППУ: файл ложится под basename, роль прописывается в
//     profile.json, существующие файлы не меняются, повтор без replace отвечает
//     present (идемпотентность), replace перекачивает, чужая роль на том же
//     basename — отказ кодом.
//  4. УДАЛЕНИЕ: файл исчезает вместе с записью в профиле, вердикт пересчитан,
//     повтор — noop, не-весовой файл и path traversal — отказ.
//
// HF подменён моком (hf_mock.go): сеть в тестах не нужна.
package sdbackend

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// writeProfileForTest — profile.json с перечисленными файлами (без записи байтов).
// Defaults берём из дефолтов семейства: профиль с нулевыми defaults невалиден, и
// реестр справедливо считал бы его «сломанным» (см. ValidateImageModelProfile).
func writeProfileForTest(t *testing.T, dir, name, family string, files []types.ImageModelFile) {
	t.Helper()
	p := types.ImageModelProfile{
		Name: name, Family: family, Files: files,
		Defaults: types.DefaultImageGenDefaults(family),
	}
	if err := persistBundleProfile(dir, &p); err != nil {
		t.Fatalf("persist profile: %v", err)
	}
}

// TestModelFilesInventory_DiskTruth_R751 — инвентарь и вердикт по фактическому диску.
func TestModelFilesInventory_DiskTruth_R751(t *testing.T) {
	mock := NewHFMockServer(t)
	hf, reg, modelsDir := newHFTestManager(t, mock)
	_ = hf
	dir := filepath.Join(modelsDir, "qwen-edit-2511")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// diffusion — на диске, VAE — только в профиле (удалён вручную), плюс
	// недокачанный `.download` и весовой файл вне профиля.
	writeBundleFile(t, dir, "qwen-edit-2511-Q4_K_M.gguf", qwenDiffusionGGUF(t))
	if err := os.WriteFile(filepath.Join(dir, "qwen_image_vae.safetensors.download"),
		[]byte("0123456789"), 0o644); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old-quant-Q8_0.gguf"),
		[]byte("GGUFold"), 0o644); err != nil {
		t.Fatalf("write extra: %v", err)
	}
	writeProfileForTest(t, dir, "qwen-edit-2511", "qwen_image", []types.ImageModelFile{
		{Role: types.ImageFileRoleDiffusion, Repo: "acme/qwen", Filename: "qwen-edit-2511-Q4_K_M.gguf"},
		{Role: types.ImageFileRoleVae, Repo: "acme/qwen", Filename: "qwen_image_vae.safetensors"},
	})
	if err := reg.Load(); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	profile, ok := reg.Profile("qwen-edit-2511")
	if !ok {
		t.Fatal("модель не зарегистрирована")
	}
	if len(profile.Files) != 2 {
		t.Fatalf("профиль не прочитан (files=%d, notes=%q): %+v", len(profile.Files), profile.Notes, profile.Files)
	}
	info := reg.Info(profile, "not_loaded", "", time.Time{}, time.Time{}, 0)

	byName := map[string]types.ImageModelFile{}
	for _, f := range info.Files {
		byName[f.Filename] = f
	}
	diff, ok := byName["qwen-edit-2511-Q4_K_M.gguf"]
	if !ok {
		t.Fatalf("в инвентаре нет diffusion: %+v", info.Files)
	}
	if diff.Present == nil || !*diff.Present || diff.DiskSizeBytes == 0 {
		t.Fatalf("diffusion обязан быть present с размером: %+v", diff)
	}
	vae, ok := byName["qwen_image_vae.safetensors"]
	if !ok {
		t.Fatalf("в инвентаре нет записи VAE (файла нет, запись профиля обязана быть): %+v", info.Files)
	}
	if vae.Present == nil || *vae.Present {
		t.Fatalf("VAE нет на диске, present обязан быть false: %+v", vae)
	}
	if vae.PartialBytes != 10 {
		t.Fatalf("partialBytes = %d, want 10 (размер .download)", vae.PartialBytes)
	}
	extra, ok := byName["old-quant-Q8_0.gguf"]
	if !ok || !extra.Unregistered {
		t.Fatalf("файл вне профиля обязан быть в инвентаре с unregistered=true: %+v", info.Files)
	}
	if extra.Present == nil || !*extra.Present || extra.DiskSizeBytes == 0 {
		t.Fatalf("unregistered-файл на диске обязан быть present: %+v", extra)
	}

	// Вердикт смотрит на ДИСК: VAE в профиле есть, файла нет → роль отсутствует.
	compat := info.Compat
	if compat == nil || compat.Verdict != CompatIncomplete {
		t.Fatalf("вердикт %v (%v), want incomplete: файла VAE нет", compat, compat.Missing)
	}
	foundVae := false
	for _, r := range compat.Roles {
		if r.Role == types.ImageFileRoleVae {
			foundVae = r.Found
		}
	}
	if foundVae {
		t.Fatalf("VAE считается найденным, хотя файла нет: %+v", compat.Roles)
	}
}

// TestStartModelFileInstall_AddsAndReplaces_R751 — установка/перекачка файла в группу.
func TestStartModelFileInstall_AddsAndReplaces_R751(t *testing.T) {
	mock := NewHFMockServer(t)
	const repo = "acme/qwen-image"
	mock.AddFile(repo, "vae/qwen_image_vae.safetensors", 96*1024)
	hf, reg, bundleDir, before := componentFixture(t, mock, "qwen-edit-2511")

	install, err := hf.StartModelFileInstall(ModelFileInstallRequest{
		Bundle: "qwen-edit-2511", Role: types.ImageFileRoleVae,
		Repo: repo, Filename: "vae/qwen_image_vae.safetensors",
	})
	if err != nil {
		t.Fatalf("StartModelFileInstall: %v", err)
	}
	if install.Status != "started" || install.TrackingID == "" || install.ProgressURL == "" {
		t.Fatalf("ответ установки: %+v", install)
	}
	if install.Filename != "qwen_image_vae.safetensors" {
		t.Fatalf("filename в группе = %q, want basename", install.Filename)
	}
	progress := waitComponentDone(t, hf, install.TrackingID)
	if progress.Status != "completed" {
		t.Fatalf("установка не завершилась: %s (%s)", progress.Status, progress.ErrorMessage)
	}
	local := filepath.Join(bundleDir, "qwen_image_vae.safetensors")
	if fi, err := os.Stat(local); err != nil || fi.Size() != 96*1024 {
		t.Fatalf("файл не скачан в группу: %v", err)
	}
	if err := reg.Load(); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	profile, _ := reg.Profile("qwen-edit-2511")
	roleFile := ""
	for _, f := range profile.Files {
		if f.Role == types.ImageFileRoleVae {
			roleFile = f.Filename
		}
	}
	if roleFile != "qwen_image_vae.safetensors" {
		t.Fatalf("роль vae не прописана: %v", profile.Files)
	}
	// Существующий diffusion не тронут.
	after := fileHashes(t, bundleDir)
	if after["qwen-edit-2511-Q4_K_M.gguf"] != before["qwen-edit-2511-Q4_K_M.gguf"] {
		t.Fatal("установка VAE изменила diffusion-файл")
	}
	// Вердикт: VAE больше не отсутствует.
	compat := reg.Compat("qwen-edit-2511")
	for _, r := range compat.Roles {
		if r.Role == types.ImageFileRoleVae && !r.Found {
			t.Fatalf("после установки VAE вердикт всё ещё считает роль отсутствующей: %+v", compat.Roles)
		}
	}

	// Повтор БЕЗ replace: файл на месте нужного размера → present, без загрузки.
	again, err := hf.StartModelFileInstall(ModelFileInstallRequest{
		Bundle: "qwen-edit-2511", Role: types.ImageFileRoleVae,
		Repo: repo, Filename: "vae/qwen_image_vae.safetensors",
	})
	if err != nil {
		t.Fatalf("повторная установка: %v", err)
	}
	if again.Status != "present" {
		t.Fatalf("идемпотентность: status = %q, want present", again.Status)
	}

	// replace=true: файл перекачивается, в профиле остаётся ОДНА запись роли.
	replaced, err := hf.StartModelFileInstall(ModelFileInstallRequest{
		Bundle: "qwen-edit-2511", Role: types.ImageFileRoleVae,
		Repo: repo, Filename: "vae/qwen_image_vae.safetensors", Replace: true,
	})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if replaced.Status != "started" {
		t.Fatalf("replace: status = %q, want started", replaced.Status)
	}
	if p := waitComponentDone(t, hf, replaced.TrackingID); p.Status != "completed" {
		t.Fatalf("replace не завершился: %s (%s)", p.Status, p.ErrorMessage)
	}
	if fi, err := os.Stat(local); err != nil || fi.Size() != 96*1024 {
		t.Fatalf("после replace файл не на месте: %v", err)
	}
	profile, _ = reg.Profile("qwen-edit-2511")
	vaeCount := 0
	for _, f := range profile.Files {
		if f.Role == types.ImageFileRoleVae {
			vaeCount++
		}
	}
	if vaeCount != 1 {
		t.Fatalf("после replace записей vae = %d, want 1: %v", vaeCount, profile.Files)
	}

	// Чужая роль на том же basename — отказ кодом (защита от подмены весов).
	_, err = hf.StartModelFileInstall(ModelFileInstallRequest{
		Bundle: "qwen-edit-2511", Role: types.ImageFileRoleClipL,
		Repo: repo, Filename: "vae/qwen_image_vae.safetensors",
	})
	var ce *ComponentError
	if !errors.As(err, &ce) || ce.Code != ModelFileErrRoleConflict {
		t.Fatalf("ожидался отказ %s, получено %v", ModelFileErrRoleConflict, err)
	}
}

// TestDeleteModelFile_LastFileUnregistersBundle_R751 — удаление последнего файла
// снимает регистрацию, а не оставляет «сломанный» профиль без файлов.
func TestDeleteModelFile_LastFileUnregistersBundle_R751(t *testing.T) {
	mock := NewHFMockServer(t)
	hf, reg, modelsDir := newHFTestManager(t, mock)
	dir := filepath.Join(modelsDir, "solo-group")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeBundleFile(t, dir, "solo.gguf", []byte("GGUFsolo"))
	writeProfileForTest(t, dir, "solo-group", "other", []types.ImageModelFile{{
		Role: types.ImageFileRoleDiffusion, Repo: "acme/solo", Filename: "solo.gguf",
	}})
	if err := reg.Load(); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	res, err := hf.DeleteModelFile("solo-group", "solo.gguf")
	if err != nil {
		t.Fatalf("DeleteModelFile: %v", err)
	}
	if res.Status != "deleted" {
		t.Fatalf("status = %q, want deleted", res.Status)
	}
	// Каталог переименован в .unregistered-…, а не остался моделью без файлов.
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("каталог удалённой группы остался на месте: %v", statErr)
	}
	entries, _ := os.ReadDir(modelsDir)
	found := false
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), ".unregistered-") {
			found = true
		}
	}
	if !found {
		t.Fatalf("нет каталога .unregistered-* после удаления последнего файла")
	}
	if err := reg.Load(); err != nil {
		t.Fatalf("registry load после снятия регистрации: %v", err)
	}
	if _, ok := reg.Profile("solo-group"); ok {
		t.Fatal("модель без файлов осталась в реестре")
	}
	// Пустой переименованный каталог НЕ должен появляться «сломанной моделью».
	for name := range reg.Profiles() {
		if strings.HasPrefix(name, ".unregistered-") {
			t.Fatalf("каталог со снятой регистрацией попал в реестр как модель: %q", name)
		}
	}
}

func TestDeleteModelFile_RemovesFileAndProfileEntry_R751(t *testing.T) {
	mock := NewHFMockServer(t)
	const repo = "acme/qwen-image"
	mock.AddFile(repo, "vae/qwen_image_vae.safetensors", 4096)
	hf, reg, bundleDir, _ := componentFixture(t, mock, "qwen-edit-2511")
	install, err := hf.StartModelFileInstall(ModelFileInstallRequest{
		Bundle: "qwen-edit-2511", Role: types.ImageFileRoleVae,
		Repo: repo, Filename: "vae/qwen_image_vae.safetensors",
	})
	if err != nil {
		t.Fatalf("установка: %v", err)
	}
	if p := waitComponentDone(t, hf, install.TrackingID); p.Status != "completed" {
		t.Fatalf("установка не завершилась: %s", p.Status)
	}
	if err := reg.Load(); err != nil {
		t.Fatalf("registry load: %v", err)
	}

	res, err := hf.DeleteModelFile("qwen-edit-2511", "qwen_image_vae.safetensors")
	if err != nil {
		t.Fatalf("DeleteModelFile: %v", err)
	}
	if res.Status != "deleted" || res.BytesFreed != 4096 || !res.ProfileChange {
		t.Fatalf("результат удаления: %+v", res)
	}
	if len(res.RemovedRoles) != 1 || res.RemovedRoles[0] != types.ImageFileRoleVae {
		t.Fatalf("removedRoles = %v, want [vae]", res.RemovedRoles)
	}
	if _, err := os.Stat(filepath.Join(bundleDir, "qwen_image_vae.safetensors")); !os.IsNotExist(err) {
		t.Fatalf("файл не удалён: %v", err)
	}
	if err := reg.Load(); err != nil {
		t.Fatalf("registry load после удаления: %v", err)
	}
	profile, _ := reg.Profile("qwen-edit-2511")
	for _, f := range profile.Files {
		if f.Role == types.ImageFileRoleVae {
			t.Fatalf("запись vae осталась в профиле: %v", profile.Files)
		}
	}
	compat := reg.Compat("qwen-edit-2511")
	missingVae := false
	for _, r := range compat.Roles {
		if r.Role == types.ImageFileRoleVae && !r.Found {
			missingVae = true
		}
	}
	if !missingVae {
		t.Fatalf("после удаления VAE вердикт не видит нехватку: %+v", compat.Roles)
	}

	// Повтор — noop, а не ошибка (двойной клик не должен пугать оператора).
	res2, err := hf.DeleteModelFile("qwen-edit-2511", "qwen_image_vae.safetensors")
	if err != nil || res2.Status != "noop" {
		t.Fatalf("повторное удаление: res=%+v err=%v", res2, err)
	}

	// Не-весовой файл и выход из каталога — отказ кодом.
	for _, name := range []string{ProfileFileName, "../evil.gguf", "sub/../../evil.gguf"} {
		_, derr := hf.DeleteModelFile("qwen-edit-2511", name)
		var ce *ComponentError
		if !errors.As(derr, &ce) || ce.Code != ModelFileErrInvalidName {
			t.Fatalf("удаление %q: ожидался %s, получено %v", name, ModelFileErrInvalidName, derr)
		}
	}
}
