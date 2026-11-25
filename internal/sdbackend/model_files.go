// model_files.go — R751 (2026-11-25): управление ОТДЕЛЬНЫМИ файлами группы
// (image-bundle) на узле: состояние на диске, докачка, перекачка, удаление.
//
// ЗАЧЕМ. R749 показала состав корзины ДО скачивания, но после скачивания у
// оператора не было ни одного способа управлять файлами ВНУТРИ группы: в
// «Моделях на диске» была только таблица имён и кнопка «удалить bundle целиком»,
// а фраза «докачать недостающее» относилась лишь к выбору на вкладке HF. Живые
// последствия: удалённый вручную файл оставался в profile.json, вердикт совмести-
// мости отвечал «набор полный», и загрузка падала уже на движке; заменить квант
// diffusion без перекачки всего набора было нельзя (удаление всей группы и
// повторный сбор на HF — дороже на гигабайты трафика).
//
// ЧТО ЗДЕСЬ:
//
//   modelFilesWithDiskState — профиль + фактическое состояние диска: есть файл,
//     сколько он занимает, сколько лежит в `<файл>.download` (частичная
//     загрузка), и какие весовые файлы лежат в каталоге ВНЕ профиля;
//   StartModelFileInstall — положить файл из HF в СУЩЕСТВУЮЩУЮ группу под ролью
//     (замена файла роли, докачка отсутствующего, смена кванта diffusion);
//   DeleteModelFile — удалить НАЗВАННЫЙ файл группы, вычистить его запись из
//     profile.json и пересчитать реестр/вердикт.
//
// ЧЕГО ЗДЕСЬ НЕТ (осознанно): удаления группы целиком (это существующая
// POST /api/image/models/delete), установки компонента ИЗ КАТАЛОГА (R710,
// components_install.go) и правки ролей файлов «на месте» — оператор выбирает
// роль при скачивании, а не переназначает уже лежащий файл (такая правка без
// перекачки ломает argv движка неочевидно для него).
package sdbackend

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Состояние файлов на диске (для GET /api/image/models)
// ============================================================

// boolPtr — адрес bool для Present (см. комментарий к ImageModelFile.Present).
func boolPtr(b bool) *bool { return &b }

// modelFilesWithDiskState — файлы профиля с фактическим состоянием НА ДИСКЕ плюс
// весовые файлы каталога, которых в профиле нет.
//
// ГРАНИЦА: «вне профиля» — не ошибка, а состояние. Так выглядит файл, положенный
// в каталог вручную или оставшийся от прежнего кванта после замены diffusion.
// Показываем его, чтобы оператор мог удалить лишнее, а не искал гигабайты в
// консоли; роль у такого файла пустая, и движок его не получит.
func modelFilesWithDiskState(files []types.ImageModelFile, dir string) []types.ImageModelFile {
	out := make([]types.ImageModelFile, 0, len(files)+2)
	known := make(map[string]bool, len(files)*2)
	for _, f := range files {
		item := f
		local := f.LocalPath
		if local == "" && dir != "" && f.Filename != "" {
			local = filepath.Join(dir, filepath.FromSlash(f.Filename))
		}
		present, size := fileDiskState(local)
		item.Present = boolPtr(present)
		if present {
			item.DiskSizeBytes = size
		}
		if partial := fileSize(local + ".download"); partial > 0 {
			item.PartialBytes = partial
		}
		// Ключи «что уже учтено»: относительный путь и basename. Файлы bundle
		// плоские, поэтому одного basename достаточно; путь — страховка для
		// подкаталогов, с которыми приходят синтезированные профили.
		if f.Filename != "" {
			rel := filepath.ToSlash(filepath.Clean(filepath.FromSlash(f.Filename)))
			known[rel] = true
			known[filepath.Base(rel)] = true
		}
		out = append(out, item)
	}
	out = append(out, unregisteredModelFiles(dir, known)...)
	return out
}

// unregisteredModelFiles — весовые файлы в каталоге bundle, не описанные профилем.
func unregisteredModelFiles(dir string, known map[string]bool) []types.ImageModelFile {
	if dir == "" {
		return nil
	}
	var out []types.ImageModelFile
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		base := info.Name()
		if base == ProfileFileName || base == HFBundleManifestName ||
			strings.HasSuffix(base, ".download") || strings.HasSuffix(base, ".json") ||
			!cppbackend.HasModelWeightExtension(base) {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if known[relSlash] || known[filepath.Base(relSlash)] {
			return nil
		}
		out = append(out, types.ImageModelFile{
			Filename:      relSlash,
			LocalPath:     path,
			SizeBytes:     info.Size(),
			Present:       boolPtr(true),
			DiskSizeBytes: info.Size(),
			Unregistered:  true,
		})
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })
	return out
}

// fileDiskState — (есть ли обычный файл, его размер).
func fileDiskState(path string) (bool, int64) {
	if path == "" {
		return false, 0
	}
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false, 0
	}
	return true, fi.Size()
}

// fileSize — размер файла или 0.
func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return 0
	}
	return fi.Size()
}

// ============================================================
// Установка файла в существующую группу
// ============================================================

// Коды отказов файловых операций (машинные — их читает UI и тесты).
const (
	ModelFileErrPresent      = "file_already_present" // 409: файл уже лежит (нужен replace)
	ModelFileErrNotFound     = "file_not_found"       // 404: удалять нечего
	ModelFileErrInvalidName  = "invalid_filename"     // 400: путь вне группы / не весовой файл
	ModelFileErrRoleConflict = "file_role_conflict"   // 409: basename занят другой ролью
)

// ModelFileInstallRequest — тело POST /api/image/models/{name}/files.
//
// КОНТРАКТ UI (webui/js/modules/image-model-files.js):
//
//	{role, repo, filename, revision?, replace?}
//
//	filename — путь файла В РЕПОЗИТОРИИ (может содержать подкаталоги), в группу
//	           он ляжет под своим basename — ровно как bundle-загрузка;
//	replace  — «перекачать/заменить»: файл роли удаляется (вместе с .download) и
//	           качается заново; без него разрешена только докачка (файла нет).
type ModelFileInstallRequest struct {
	Bundle   string `json:"-"`
	Role     string `json:"role"`
	Repo     string `json:"repo"`
	Filename string `json:"filename"`
	Revision string `json:"revision,omitempty"`
	Replace  bool   `json:"replace,omitempty"`
}

// ModelFileInstall — ответ на постановку установки файла.
type ModelFileInstall struct {
	Status      string `json:"status"` // started | running | present
	Bundle      string `json:"bundle"`
	Role        string `json:"role"`
	Filename    string `json:"filename"` // basename в каталоге группы
	Repo        string `json:"repo"`
	Revision    string `json:"revision"`
	SizeBytes   int64  `json:"sizeBytes"`
	Replace     bool   `json:"replace,omitempty"`
	TrackingID  string `json:"trackingId,omitempty"`
	ProgressURL string `json:"progressUrl,omitempty"`
}

// ModelFileTrackingID — идентификатор фоновой загрузки файла (его же принимает
// GET /api/hf/progress?bundleId= и POST /api/hf/cancel).
func ModelFileTrackingID(bundle, role string) string {
	return "modelfile-" + bundle + "-" + role
}

// StartModelFileInstall — поставить загрузку ОДНОГО файла в существующую группу.
//
// ПРАВИЛА (каждое — ответ на живой класс ошибок):
//  1. группа обязана существовать: файл кладётся в СКАЧАННЫЙ набор, а не создаёт
//     каталог с одним VAE без diffusion (ровно как components_install.go);
//  2. basename не может быть занят ДРУГОЙ ролью: иначе скачивание VAE поверх
//     diffusion молча заменило бы веса модели;
//  3. replace=true удаляет файл роли ДО загрузки (вместе с .download): иначе
//     загрузчик увидит файл того же размера и пропустит скачивание, а «перекачать»
//     осталось бы обещанием;
//  4. replace=false не трогает ничего: если файл уже на месте и размер совпал —
//     ответ present (сеть не нужна); если роль занята другим файлом — 409 с
//     текущим файлом, чтобы UI предложил именно замену;
//  5. профиль правится ТОЛЬКО после успешной загрузки. При ошибке старый файл
//     роли остаётся, набор не становится хуже, чем был;
//  6. lora — единственная роль, у которой файлов может быть много: её replace не
//     удаляет соседние lora того же каталога.
func (m *HFManager) StartModelFileInstall(req ModelFileInstallRequest) (*ModelFileInstall, error) {
	bundle := strings.TrimSpace(req.Bundle)
	if err := validateHFBundleID(bundle); err != nil {
		return nil, &ComponentError{Code: CompErrBundleNotFound, Message: err.Error()}
	}
	role := strings.TrimSpace(req.Role)
	if err := validateImageFileRole(role); err != nil {
		return nil, componentErr(CompErrRoleInvalid, "%v", err)
	}
	repo := strings.TrimSpace(req.Repo)
	if repo == "" {
		return nil, componentErr(CompErrRepoRequired,
			"repo is required: without it the file cannot be downloaded again (profile.json has no source repository)")
	}
	filename := strings.TrimSpace(filepath.ToSlash(filepath.FromSlash(req.Filename)))
	if filename == "" {
		return nil, componentErr(CompErrFilesRequired, "filename is required: path of the file inside %s", repo)
	}
	if strings.HasPrefix(filename, "../") || strings.Contains(filename, "/../") ||
		filepath.IsAbs(filepath.FromSlash(filename)) {
		return nil, componentErr(ModelFileErrInvalidName,
			"filename %q must be a relative path inside the repository", req.Filename)
	}
	revision := strings.TrimSpace(req.Revision)
	if revision == "" {
		revision = "main"
	}

	dir, err := m.componentBundleDir(bundle, "")
	if err != nil {
		return nil, err
	}
	profile, err := m.loadBundleProfile(bundle, dir)
	if err != nil {
		return nil, err
	}
	// Каталог без profile.json: фиксируем профиль ДО загрузки, иначе синтез после
	// скачивания увидит два файла и назначит роли заново (та же причина, что в
	// components_install.go).
	if _, statErr := os.Stat(filepath.Join(dir, ProfileFileName)); os.IsNotExist(statErr) {
		if perr := persistBundleProfile(dir, profile); perr != nil {
			return nil, perr
		}
	}

	expected, _, _, err := m.resolveComponentFiles(repo, revision, []string{filename}, dir)
	if err != nil {
		return nil, err
	}
	if len(expected) != 1 {
		return nil, componentErr(ModelFileErrInvalidName,
			"expected exactly one model file for %q, got %d", filename, len(expected))
	}
	target := expected[0]
	base := target.Filename
	targetPath := filepath.Join(dir, base)

	// Basename, занятый другой ролью, — отказ даже с replace: иначе подмена весов
	// модели выглядела бы как «докачка VAE».
	for _, f := range profile.Files {
		if filepath.Base(f.Filename) != base {
			continue
		}
		if f.Role != role {
			ce := componentErr(ModelFileErrRoleConflict,
				"file %q already belongs to role %q in bundle %q: downloading it as %q would overwrite an existing model file",
				base, f.Role, bundle, role)
			ce.CurrentFile = base
			return nil, ce
		}
	}

	// Что уже есть у роли на диске.
	var rolePresent []types.ImageModelFile
	var roleMissing []types.ImageModelFile
	for _, f := range profile.Files {
		if f.Role != role || filepath.Base(f.Filename) == base {
			continue
		}
		if present, _ := fileDiskState(fileLocalPath(f, dir)); present {
			rolePresent = append(rolePresent, f)
		} else {
			roleMissing = append(roleMissing, f)
		}
	}
	targetPresent, targetSize := fileDiskState(targetPath)
	if targetPresent && !req.Replace && target.SizeBytes > 0 && targetSize == target.SizeBytes {
		// Идемпотентность: файл уже лежит и совпал по размеру — качать нечего.
		return &ModelFileInstall{
			Status: "present", Bundle: bundle, Role: role, Filename: base,
			Repo: repo, Revision: revision, SizeBytes: target.SizeBytes,
		}, nil
	}
	if len(rolePresent) > 0 && !req.Replace && role != types.ImageFileRoleLora {
		ce := componentErr(CompErrRolePresent,
			"role %q is already present in bundle %q: %s", role, bundle, rolePresent[0].Filename)
		ce.CurrentFile = rolePresent[0].Filename
		return nil, ce
	}
	if targetPresent && !req.Replace {
		ce := componentErr(ModelFileErrPresent,
			"file %q is already on disk (%d bytes); use replace to download it again",
			base, targetSize)
		ce.CurrentFile = base
		return nil, ce
	}

	// replace: убираем старый файл роли (и его .download) ДО загрузки, иначе
	// загрузчик пропустит скачивание по совпадению размера.
	var oldFiles []string
	if req.Replace {
		if targetPresent {
			oldFiles = append(oldFiles, targetPath)
		}
		for _, f := range rolePresent {
			oldFiles = append(oldFiles, fileLocalPath(f, dir))
		}
		for _, p := range oldFiles {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				sdLog().Warnw("model file install: cannot remove old file before replace",
					"bundle", bundle, "role", role, "path", p, "error", err)
			}
		}
		_ = os.Remove(targetPath + ".download")
	}

	tID := ModelFileTrackingID(bundle, role)
	downloads := []cppbackend.HFDownloadRequest{{
		ModelID:   repo,
		Filename:  target.SourcePath,
		Revision:  revision,
		Role:      role,
		SizeBytes: target.SizeBytes,
	}}
	state, created := m.beginStateIfNew(tID, profile.Family, dir, downloads)
	out := &ModelFileInstall{
		Status: "started", Bundle: bundle, Role: role, Filename: base,
		Repo: repo, Revision: revision, SizeBytes: target.SizeBytes,
		Replace: req.Replace, TrackingID: tID,
		ProgressURL: "/api/hf/progress?bundleId=" + tID,
	}
	if !created {
		out.Status = "running"
		return out, nil
	}
	go m.runModelFileInstall(m.baseCtx, state, bundle, role, repo, revision, base, target.SourcePath,
		dir, profile.Family, oldFiles)
	return out, nil
}

// runModelFileInstall — фоновая загрузка одного файла + правка профиля.
//
// ПОРЯДОК КАК В components_install.go: download → profile → terminal status.
// Иначе UI по «completed» перечитал бы модели и увидел СТАРЫЙ состав.
func (m *HFManager) runModelFileInstall(
	ctx context.Context,
	state *hfBundleState,
	bundle, role, repo, revision, base, sourcePath, dir, family string,
	oldFiles []string,
) {
	log := sdLog()
	log.Infow("model file install started",
		"bundle", bundle, "role", role, "repo", repo, "file", base,
		"replace", len(oldFiles) > 0, "targetDir", dir)
	downloads := []cppbackend.HFDownloadRequest{{
		ModelID: repo, Filename: sourcePath, Revision: revision, Role: role,
	}}
	progress, err := m.downloader.StartComponentDownload(ctx, downloads, state.id, dir)
	if err != nil {
		log.Errorw("model file install download failed, profile NOT changed",
			"bundle", bundle, "role", role, "file", base, "error", err)
		m.finishBundleState(state, progress, err)
		return
	}
	if err := m.applyModelFileToBundle(bundle, role, repo, revision, base, dir, oldFiles); err != nil {
		log.Errorw("model file is on disk, profile NOT changed",
			"bundle", bundle, "role", role, "file", base, "error", err)
		m.finishBundleState(state, progress, err)
		return
	}
	m.finishBundleState(state, progress, nil)
	log.Infow("model file installed into image bundle",
		"bundle", bundle, "role", role, "repo", repo, "file", base, "family", family)
}

// applyModelFileToBundle — правка profile.json после успешной загрузки.
func (m *HFManager) applyModelFileToBundle(bundle, role, repo, revision, base, dir string, oldFiles []string) error {
	m.registerMu.Lock()
	defer m.registerMu.Unlock()

	profile, err := m.loadBundleProfile(bundle, dir)
	if err != nil {
		return fmt.Errorf("read profile after download: %w", err)
	}
	local := filepath.Join(dir, base)
	fi, err := os.Stat(local)
	if err != nil || fi.IsDir() {
		return fmt.Errorf("downloaded file is missing on disk: %s", base)
	}
	kept := make([]types.ImageModelFile, 0, len(profile.Files)+1)
	for _, f := range profile.Files {
		sameBase := filepath.Base(f.Filename) == base
		// У lora может быть много файлов, и replace не должен сносить соседние.
		if f.Role == role && role != types.ImageFileRoleLora && !sameBase {
			continue
		}
		if sameBase {
			continue
		}
		kept = append(kept, f)
	}
	kept = append(kept, types.ImageModelFile{
		Role: role, Repo: repo, Filename: base, Revision: revision,
		LocalPath: local, SizeBytes: fi.Size(),
	})
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].Role == kept[j].Role {
			return kept[i].Filename < kept[j].Filename
		}
		return kept[i].Role < kept[j].Role
	})
	profile.Files = kept
	note := fmt.Sprintf("файл %s роли %s обновлён из %s.", base, role, repo)
	if len(oldFiles) > 0 {
		note = fmt.Sprintf("файл %s роли %s заменён из %s (старый удалён).", base, role, repo)
	}
	profile.Notes = strings.TrimSpace(strings.TrimSpace(profile.Notes+" "+note))
	if err := types.ValidateImageModelProfile(profile); err != nil && !types.IsMissingRequiredRole(err) {
		return fmt.Errorf("profile is invalid after file install: %w", err)
	}
	if err := persistBundleProfile(dir, profile); err != nil {
		return fmt.Errorf("write profile: %w", err)
	}
	// Старые файлы роли удаляем ПОСЛЕ записи профиля: если удаление сорвётся,
	// реестр всё равно уже знает правильный состав, а лишний файл попадёт в
	// «вне профиля» и его можно удалить руками.
	for _, p := range oldFiles {
		if filepath.Clean(p) == filepath.Clean(local) {
			continue
		}
		if rerr := os.Remove(p); rerr != nil && !os.IsNotExist(rerr) {
			sdLog().Warnw("model file install: cannot remove replaced file",
				"bundle", bundle, "path", p, "error", rerr)
		}
		_ = os.Remove(p + ".download")
	}
	if err := m.registry.Load(); err != nil {
		return fmt.Errorf("registry reload after file install: %w", err)
	}
	return nil
}

// fileLocalPath — путь файла профиля на диске (LocalPath либо собранный из dir).
func fileLocalPath(f types.ImageModelFile, dir string) string {
	if f.LocalPath != "" {
		return f.LocalPath
	}
	if dir == "" || f.Filename == "" {
		return ""
	}
	return filepath.Join(dir, filepath.FromSlash(f.Filename))
}

// ============================================================
// Удаление отдельного файла группы
// ============================================================

// ModelFileDelete — результат DELETE /api/image/models/{name}/files.
type ModelFileDelete struct {
	Status        string   `json:"status"` // deleted | noop
	Bundle        string   `json:"bundle"`
	Filename      string   `json:"filename"`
	BytesFreed    int64    `json:"bytesFreed"`
	RemovedRoles  []string `json:"removedRoles,omitempty"`
	ProfileChange bool     `json:"profileChanged"`
}

// DeleteModelFile — удалить НАЗВАННЫЙ файл группы вместе с записью в профиле.
//
// ПОЧЕМУ НЕ /api/hf/cleanup: тот удаляет файл, но профиль не трогает — реестр и
// вердикт продолжали считать роль найденной. Здесь удаление и правка профиля —
// одна операция под registerMu, а реестр перечитывается до ответа.
//
// Разрешены только весовые файлы (и их `.download`): profile.json, манифест и
// конфиги движка удалять через этот путь нельзя.
func (m *HFManager) DeleteModelFile(bundle, filename string) (*ModelFileDelete, error) {
	bundle = strings.TrimSpace(bundle)
	if err := validateHFBundleID(bundle); err != nil {
		return nil, &ComponentError{Code: CompErrBundleNotFound, Message: err.Error()}
	}
	rel, err := cleanModelFilePath(filename)
	if err != nil {
		return nil, err
	}
	dir, derr := m.componentBundleDir(bundle, "")
	if derr != nil {
		return nil, derr
	}
	full := filepath.Join(dir, filepath.FromSlash(rel))
	// Path traversal: проверяем фактическим Rel, а не префиксом строки.
	if back, rerr := filepath.Rel(dir, full); rerr != nil || back == ".." ||
		strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return nil, componentErr(ModelFileErrInvalidName,
			"filename %q escapes the bundle directory", filename)
	}

	// Активная загрузка того же каталога держит файл открытым: отменяем её до
	// удаления (как deleteBundleFile в hf.go).
	m.cancelActiveBundleFor(dir)

	out := &ModelFileDelete{Status: "noop", Bundle: bundle, Filename: rel}
	for _, candidate := range []string{full, full + ".download"} {
		if size := fileSize(candidate); size > 0 {
			if rerr := os.Remove(candidate); rerr != nil && !os.IsNotExist(rerr) {
				return out, fmt.Errorf("remove %s: %w", candidate, rerr)
			}
			out.BytesFreed += size
			out.Status = "deleted"
		}
	}

	m.registerMu.Lock()
	defer m.registerMu.Unlock()
	profilePath := filepath.Join(dir, ProfileFileName)
	if _, statErr := os.Stat(profilePath); statErr == nil {
		profile, perr := m.loadBundleProfile(bundle, dir)
		if perr != nil {
			return out, fmt.Errorf("read profile after delete: %w", perr)
		}
		kept := make([]types.ImageModelFile, 0, len(profile.Files))
		removedRoles := make([]string, 0, 1)
		for _, f := range profile.Files {
			if modelFileSame(f.Filename, rel) {
				removedRoles = append(removedRoles, f.Role)
				continue
			}
			kept = append(kept, f)
		}
		if len(removedRoles) > 0 {
			profile.Files = kept
			if out.Status == "noop" {
				out.Status = "deleted"
			}
			profile.Notes = strings.TrimSpace(strings.TrimSpace(profile.Notes+
				" файл "+rel+" удалён с узла."))
			if werr := persistBundleProfile(dir, profile); werr != nil {
				return out, fmt.Errorf("write profile: %w", werr)
			}
			out.ProfileChange = true
			out.RemovedRoles = removedRoles
		}
	}
	if out.Status == "deleted" {
		if err := m.registry.Load(); err != nil {
			return out, fmt.Errorf("registry reload after file delete: %w", err)
		}
	}
	return out, nil
}

// modelFileSame — один и тот же путь файла в профиле и в запросе.
func modelFileSame(profileName, rel string) bool {
	a := filepath.ToSlash(filepath.Clean(filepath.FromSlash(profileName)))
	b := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	if a == b {
		return true
	}
	// Плоский bundle: в профиле может лежать basename, а пришёл путь с
	// подкаталогом (или наоборот) — сверяем и по basename.
	return filepath.Base(a) == filepath.Base(b)
}

// cleanModelFilePath — нормализация относительного пути файла группы.
//
// Разрешены только весовые файлы и их `.download`/`.tmp`-хвосты: остальное
// (profile.json, sd-server.config.json, манифест) — не «файл модели».
func cleanModelFilePath(filename string) (string, error) {
	raw := strings.TrimSpace(filepath.ToSlash(filepath.FromSlash(filename)))
	if raw == "" {
		return "", componentErr(ModelFileErrInvalidName, "filename is required (relative path inside the bundle)")
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(raw)))
	if filepath.IsAbs(filepath.FromSlash(raw)) || clean == "." || clean == ".." ||
		strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", componentErr(ModelFileErrInvalidName,
			"filename %q must be relative inside the bundle", filename)
	}
	weight := clean
	for _, suffix := range []string{".download", ".tmp"} {
		if strings.HasSuffix(weight, suffix) {
			weight = strings.TrimSuffix(weight, suffix)
			break
		}
	}
	if !cppbackend.HasModelWeightExtension(weight) {
		return "", componentErr(ModelFileErrInvalidName,
			"filename %q is not a model weight file (allowed: %s)",
			filename, strings.Join(cppbackend.ModelWeightExtensions, ", "))
	}
	return clean, nil
}

// ModelFilesInventory — экспорт инвентаря для менеджера (нужен тестам и
// сервисам, которым не нужен весь Registry). profileFiles — записи профиля.
func ModelFilesInventory(profileFiles []types.ImageModelFile, dir string) []types.ImageModelFile {
	return modelFilesWithDiskState(profileFiles, dir)
}
