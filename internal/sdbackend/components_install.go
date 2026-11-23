// components_install.go — R710 (2026-11-24): «добавить недостающий компонент в
// УЖЕ скачанный набор».
//
// ЗАЧЕМ. Каталог (components.go) говорит, ОТКУДА взять VAE/text encoder, а этот
// файл делает вторую половину работы: скачивает выбранные файлы в каталог
// существующего bundle и прописывает роль в profile.json, после чего вердикт
// совместимости (compat.go) пересчитывается. Живой дефект, который это чинит:
// репозиторий `Qwen-Image-Edit-2511` содержит ТОЛЬКО diffusion-файлы, вердикт
// честно говорит «не хватает VAE, text encoder (LLM)», а добавить их из UI было
// нельзя — оператору предлагалось «добавьте файлами из другого репозитория»
// вручную, вне страницы.
//
// ГРАНИЦЫ, КОТОРЫЕ ЗДЕСЬ СОБЛЮДАЮТСЯ (они же — проверки, за которые отвечает
// HTTP-слой):
//
//  1. ТОЛЬКО ВНУТРИ КАТАЛОГА МОДЕЛЕЙ. Имя bundle приходит из URL и становится
//     путём; `modelDir` (если задан) обязан указывать на каталог ЭТОГО bundle
//     внутри modelsDir. Никаких `..`, абсолютных путей наружу и симлинк-выходов
//     (проверка через filepath.Rel, а не «строковое начало пути»);
//  2. СУЩЕСТВУЮЩИЕ ФАЙЛЫ НЕ ТРОГАЮТСЯ. Роль diffusion добавить нельзя, файл с
//     basename, уже занятым другой ролью профиля, отвергается конфликтом: иначе
//     скачивание VAE поверх диффузии превратило бы рабочий набор в мусор;
//  3. РОЛЬ ЗАНИМАЕТСЯ ОДИН РАЗ. Если роль уже есть в профиле и её файл на
//     диске — 409 `role_already_present` с указанием текущего файла (движок
//     получил бы два --vae);
//  4. ИДЕМПОТЕНТНОСТЬ. Повторный вызов с теми же файлами не качает заново:
//     файлы, уже лежащие в bundle, пропускаются (это же умеет загрузчик
//     cppbackend по совпадению размера), а идущая установка той же роли
//     возвращает свой прогресс, а не вторую загрузку;
//  5. ЛИМИТЫ. Не больше ComponentInstallMaxFiles файлов и
//     ComponentInstallMaxSizeMB на запрос; существование репозитория и файла
//     проверяется ДО старта (HF tree API), чтобы «не нашёл репозиторий» был
//     ответом сразу, а не через минуту в прогрессе;
//  6. ПРОГРЕСС И ОТМЕНА — существующим механизмом загрузок: состояние
//     регистрируется в трекере HFManager под отдельным id, поэтому
//     GET /api/hf/progress?bundleId=<trackingId> и POST /api/hf/cancel работают
//     без новых ручек.
package sdbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Коды отказов (машинные — их читает UI и тесты)
// ============================================================

// Коды ComponentError.Code. HTTP-слой переводит их в статусы, UI показывает
// текст; тесты проверяют ИМЕННО код, чтобы формулировка могла меняться.
const (
	CompErrRoleInvalid      = "invalid_role"            // роль не из контракта или diffusion
	CompErrRolePresent      = "role_already_present"    // 409: роль уже занята файлом
	CompErrBundleNotFound   = "bundle_not_found"        // каталога модели нет
	CompErrInvalidPath      = "invalid_path"            // modelDir вне каталога моделей
	CompErrRepoRequired     = "repo_required"           // не указан репозиторий
	CompErrFilesRequired    = "files_required"          // не указаны файлы
	CompErrTooManyFiles     = "too_many_files"          // больше ComponentInstallMaxFiles
	CompErrBadFile          = "bad_component_file"      // не весовой файл / повтор basename
	CompErrFileConflicts    = "component_file_conflict" // basename занят другой ролью
	CompErrFileNotFound     = "component_file_not_found"
	CompErrRepoUnavailable  = "component_repo_unavailable"
	CompErrTooLarge         = "component_too_large"
	CompErrRoleNotRecommend = "role_not_recommended" // роли нет в каталоге семейства (не блокер)
)

// ComponentError — отказ установки компонента с машинным кодом.
//
// ПОЧЕМУ ОТДЕЛЬНЫЙ ТИП, А НЕ fmt.Errorf: HTTP-слой обязан отдать РАЗНЫЕ статусы
// (409 на занятую роль, 413 на лимит, 404 на несуществующий файл), а клиент —
// разные кнопки. Разбирать текст ошибки регулярками — ровно тот класс хрупкости,
// из-за которого контракт UI и воркера уже разъезжался.
type ComponentError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// CurrentFile — для role_already_present: какой файл уже занимает роль.
	CurrentFile string `json:"currentFile,omitempty"`
	// Available — для component_file_not_found: что вообще есть в репозитории
	// (весовые файлы, до 20), чтобы оператор не гадал.
	Available []string `json:"available,omitempty"`
	// Repo/Files — что именно отвергнуто (для логов и UI).
	Repo  string   `json:"repo,omitempty"`
	Files []string `json:"files,omitempty"`
}

// Error — текст отказа (он же уходит в поле message конверта ошибки).
func (e *ComponentError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

// componentErr — конструктор отказа.
func componentErr(code, format string, args ...any) *ComponentError {
	return &ComponentError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ============================================================
// Запрос/ответ
// ============================================================

// ComponentInstallRequest — тело POST /api/image/models/{name}/components.
//
// ПОЛЯ — КОНТРАКТ UI (webui/js/modules/image-models-hf.js):
//
//	{role, repo, files:[...], revision?, default?, modelDir?}
//
//	role     — какую роль занимает скачанное (vae|llm|t5xxl|clip_l|clip_g|…);
//	repo     — репозиторий HF ("Comfy-Org/Qwen-Image_ComfyUI");
//	files    — пути ФАЙЛОВ в репозитории (могут быть с подкаталогами);
//	default  — компонент взят из каталога «по умолчанию» (для предупреждения в
//	           карточке модели: «добавлен по умолчанию — проверьте лицензию»);
//	modelDir — необязательный явный каталог bundle (обязан быть внутри
//	           modelsDir и совпадать с каталогом модели).
type ComponentInstallRequest struct {
	Bundle   string   `json:"-"` // из пути URL: /api/image/models/{name}/components
	Role     string   `json:"role"`
	Repo     string   `json:"repo"`
	Files    []string `json:"files"`
	Revision string   `json:"revision,omitempty"`
	Default  bool     `json:"default,omitempty"`
	ModelDir string   `json:"modelDir,omitempty"`
}

// ComponentExpectedFile — файл, который будет скачан (для подтверждения в UI).
type ComponentExpectedFile struct {
	Filename   string `json:"filename"`   // basename в каталоге bundle
	SourcePath string `json:"sourcePath"` // путь в репозитории HF
	SizeBytes  int64  `json:"sizeBytes"`
	SizeMB     int64  `json:"sizeMb"`
	// Present — файл уже лежит в каталоге bundle: скачиваться не будет.
	Present bool `json:"present,omitempty"`
}

// ComponentInstall — ответ на постановку установки компонента.
type ComponentInstall struct {
	Status    string   `json:"status"` // started | running
	Bundle    string   `json:"bundle"`
	Role      string   `json:"role"`
	RoleLabel string   `json:"roleLabel"`
	Repo      string   `json:"repo"`
	Files     []string `json:"files"`
	Revision  string   `json:"revision"`
	Default   bool     `json:"default"`
	// License — лицензия репозитория (если каталог её знает): UI показывает её
	// в предупреждении «проверьте лицензию».
	License string `json:"license,omitempty"`
	// TotalBytes/TotalMB — суммарный объём скачиваемого.
	TotalBytes int64 `json:"totalBytes"`
	TotalMB    int64 `json:"totalMb"`
	// Expected — по файлу: что именно и откуда качается.
	Expected []ComponentExpectedFile `json:"expected"`
	// Reused — файлы, которые уже были в bundle и не будут скачаны заново.
	Reused []string `json:"reused,omitempty"`
	// TrackingID/ProgressURL — прогресс и отмена существующим механизмом загрузок.
	TrackingID  string `json:"trackingId"`
	ProgressURL string `json:"progressUrl"`
	// Warning — предупреждение «добавлено по умолчанию, проверьте лицензию».
	Warning string `json:"warning,omitempty"`
}

// ComponentInstallTrackingID — id состояния загрузки для компонента.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ id, А НЕ имя bundle: состояние bundle-загрузки ключуется
// именем каталога, и компонентная установка затёрла бы прогресс/историю самого
// bundle (а повторный pull модели выглядел бы «уже идёт»). Символ `~` невозможен
// в имени bundle (validateHFBundleID пропускает только имена каталогов без
// спецсимволов), поэтому коллизия с настоящим bundle исключена.
func ComponentInstallTrackingID(bundle, role string) string {
	return strings.TrimSpace(bundle) + "~components~" + strings.TrimSpace(role)
}

// ============================================================
// Установка
// ============================================================

// StartComponentInstall — проверка запроса + запуск ФОНОВОЙ установки.
//
// Возвращается сразу (HTTP-слой отвечает 202): скачивание VAE — сотни мегабайт,
// text encoder'а — гигабайты, а контрольный таймаут UI — 15 с (TIMEOUT_CONTROL_MS).
// Прогресс — через уже существующий GET /api/hf/progress по TrackingID.
//
// ВСЕ ПРОВЕРКИ — ДО ГОРУТИНЫ: невалидный запрос обязан вернуть 4xx сразу, а не
// стать «фоновым провалом», о котором оператор узнает по прогрессу.
func (m *HFManager) StartComponentInstall(req ComponentInstallRequest) (*ComponentInstall, error) {
	bundle := strings.TrimSpace(req.Bundle)
	if err := validateHFBundleID(bundle); err != nil {
		return nil, &ComponentError{Code: CompErrBundleNotFound, Message: err.Error()}
	}

	// 1) Роль: только из контракта и только «компонентная» (не diffusion).
	role := strings.TrimSpace(req.Role)
	if err := validateImageFileRole(role); err != nil {
		return nil, componentErr(CompErrRoleInvalid, "%v", err)
	}
	if !IsComponentRole(role) {
		return nil, componentErr(CompErrRoleInvalid,
			"role %q cannot be added as a component: the %q file IS the model (adding a second one would silently replace the diffusion file)",
			role, types.ImageFileRoleDiffusion)
	}

	// 2) Каталог bundle: он обязан уже существовать (компонент добавляется к
	//    СКАЧАННОМУ набору, а не создаёт новый — иначе в modelsDir появился бы
	//    каталог с одним VAE без диффузии, который движок не поднимет).
	dir, err := m.componentBundleDir(bundle, req.ModelDir)
	if err != nil {
		return nil, err
	}

	// 3) Репозиторий и файлы.
	repo := strings.TrimSpace(req.Repo)
	if repo == "" {
		return nil, componentErr(CompErrRepoRequired, "repo is required (HF repository id, e.g. Comfy-Org/Qwen-Image_ComfyUI)")
	}
	files := normalizeComponentFiles(req.Files)
	if len(files) == 0 {
		return nil, componentErr(CompErrFilesRequired, "files is required: paths of the component files inside %s", repo)
	}
	if len(files) > ComponentInstallMaxFiles {
		return nil, componentErr(CompErrTooManyFiles,
			"too many files (%d): a single component request may add at most %d files",
			len(files), ComponentInstallMaxFiles)
	}
	seenBase := make(map[string]string, len(files))
	for _, f := range files {
		if !cppbackend.HasModelWeightExtension(f) {
			return nil, componentErr(CompErrBadFile, "file %q is not a model weight file (allowed: %s)",
				f, strings.Join(cppbackend.ModelWeightExtensions, ", "))
		}
		base := filepath.Base(f)
		if prev, dup := seenBase[base]; dup {
			return nil, componentErr(CompErrBadFile,
				"two files resolve to the same name %q (%s and %s): they would overwrite each other in the bundle",
				base, prev, f)
		}
		seenBase[base] = f
	}
	revision := strings.TrimSpace(req.Revision)
	if revision == "" {
		revision = "main"
	}

	// 4) Профиль: роль не должна быть занята, а basename'ы — не совпадать с
	//    файлами ДРУГИХ ролей (иначе скачивание затрёт рабочую диффузию).
	profile, err := m.loadBundleProfile(bundle, dir)
	if err != nil {
		return nil, err
	}
	// Каталог без profile.json (модель «просто положили на диск»): фиксируем
	// профиль на диске ДО загрузки. Иначе после скачивания VAE синтез профиля
	// увидел бы уже два файла и назначил роли заново — правка «добавить роль»
	// столкнулась бы с ролью, которую синтез только что присвоил сам, а две
	// параллельные установки (VAE + энкодер) перезаписывали бы друг друга.
	if _, statErr := os.Stat(filepath.Join(dir, ProfileFileName)); os.IsNotExist(statErr) {
		if perr := persistBundleProfile(dir, profile); perr != nil {
			return nil, perr
		}
	}
	for _, f := range profile.Files {
		if f.Role != role {
			continue
		}
		path := f.LocalPath
		if path == "" {
			path = filepath.Join(dir, f.Filename)
		}
		if fileExists(path) {
			ce := componentErr(CompErrRolePresent,
				"role %q is already present in bundle %q: %s", role, bundle, f.Filename)
			ce.CurrentFile = f.Filename
			return nil, ce
		}
	}
	roleByBase := make(map[string]string, len(profile.Files))
	for _, f := range profile.Files {
		roleByBase[filepath.Base(f.Filename)] = f.Role
	}
	for _, f := range files {
		base := filepath.Base(f)
		if other, taken := roleByBase[base]; taken && other != role {
			ce := componentErr(CompErrFileConflicts,
				"file %q already belongs to role %q in bundle %q: downloading it as %q would overwrite an existing model file",
				base, other, bundle, role)
			ce.CurrentFile = base
			return nil, ce
		}
	}

	// 5) Существование репозитория и файлов — ДО старта загрузки (HF tree API,
	//    тот же источник, что у GET /api/hf/files). Ошибка сети/404 → понятный
	//    код, а не «failed» в прогрессе через минуту.
	expected, total, reused, err := m.resolveComponentFiles(repo, revision, files, dir)
	if err != nil {
		return nil, err
	}
	if total > int64(ComponentInstallMaxSizeMB)*1024*1024 {
		return nil, componentErr(CompErrTooLarge,
			"component is too large: %d MiB (limit %d MiB); choose a smaller quant (see the component catalog)",
			total/(1024*1024), ComponentInstallMaxSizeMB)
	}

	// 6) Состояние + фон. beginBundleState идемпотентен: пока загрузка идёт, он
	//    возвращает СУЩЕСТВУЮЩЕЕ состояние, а не заводит второе — поэтому
	//    повторный вызов (двойной клик, повтор запроса после таймаута) не
	//    запускает вторую загрузку и не качает файл заново.
	//
	//    КАК ОПРЕДЕЛЯЕТСЯ «УЖЕ ИДЁТ»: состояние завели ДО горутины (как в
	//    StartBundle), поэтому status=="downloading" И совпадающий состав файлов
	//    означает именно наш запрос. Состав сравниваем, чтобы «VAE идёт, просят
	//    энкодер» не выглядело как повтор: ключ трекера включает роль, так что
	//    разные роли не пересекаются и этот случай — страховка.
	tID := ComponentInstallTrackingID(bundle, role)
	downloads := make([]cppbackend.HFDownloadRequest, 0, len(expected))
	for _, e := range expected {
		downloads = append(downloads, cppbackend.HFDownloadRequest{
			ModelID:   repo,
			Filename:  e.SourcePath,
			Revision:  revision,
			Role:      role,
			SizeBytes: e.SizeBytes,
		})
	}
	state, created := m.beginStateIfNew(tID, profile.Family, dir, downloads)
	status := "started"
	if !created {
		// Состояние уже было: это повторный вызов (двойной клик/retry), фон
		// второй раз не запускаем — возвращаем текущий прогресс.
		status = "running"
	}

	license, warning := componentCatalogFacts(profile.Family, role, repo, req.Default)
	out := &ComponentInstall{
		Status:      status,
		Bundle:      bundle,
		Role:        role,
		RoleLabel:   ComponentRoleTitle(role),
		Repo:        repo,
		Files:       append([]string(nil), files...),
		Revision:    revision,
		Default:     req.Default,
		License:     license,
		TotalBytes:  total,
		TotalMB:     (total + 1024*1024 - 1) / (1024 * 1024),
		Expected:    expected,
		Reused:      reused,
		TrackingID:  tID,
		ProgressURL: "/api/hf/progress?bundleId=" + tID,
		Warning:     warning,
	}

	if status == "started" {
		// Фон: ctx от baseCtx — отмена при shutdown воркера, но НЕ при закрытии
		// HTTP-соединения клиентом (иначе «закрыл вкладку» оставлял бы недокачанный
		// .download и роль, не прописанную в профиль).
		go m.runComponentInstall(m.baseCtx, state, req, downloads, dir, profile.Family)
	}
	return out, nil
}

// componentBundleDir — каталог bundle с проверкой «строго внутри modelsDir».
//
// ПОЧЕМУ НЕ filepath.Join + проверка префикса: строка
// `<modelsDir>/../../etc` тоже «начинается» с modelsDir после Clean только у
// наивной проверки. Здесь путь нормализуется, а принадлежность проверяется
// через filepath.Rel (единственный надёжный способ на всех ОС).
func (m *HFManager) componentBundleDir(bundle, modelDir string) (string, error) {
	root, err := filepath.Abs(m.modelsDir)
	if err != nil {
		return "", componentErr(CompErrInvalidPath, "resolve models dir: %v", err)
	}
	dir := filepath.Join(root, bundle)
	if strings.TrimSpace(modelDir) != "" {
		raw := strings.TrimSpace(modelDir)
		if !filepath.IsAbs(raw) {
			raw = filepath.Join(root, raw)
		}
		abs, aerr := filepath.Abs(raw)
		if aerr != nil {
			return "", componentErr(CompErrInvalidPath, "resolve modelDir: %v", aerr)
		}
		dir = filepath.Clean(abs)
	}
	rel, rerr := filepath.Rel(root, dir)
	if rerr != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", componentErr(CompErrInvalidPath,
			"modelDir %q is outside the models directory: components may only be added inside %s",
			modelDir, root)
	}
	// Имя в пути и имя модели — одно и то же: иначе компонент уехал бы в чужой
	// bundle (реестр знает модель по имени каталога).
	if filepath.Base(dir) != bundle {
		return "", componentErr(CompErrInvalidPath,
			"modelDir %q does not belong to model %q (expected <modelsDir>/%s)", modelDir, bundle, bundle)
	}
	info, serr := os.Stat(dir)
	if serr != nil || !info.IsDir() {
		return "", componentErr(CompErrBundleNotFound,
			"model %q has no bundle directory on this worker (%s): download the model first, then add components",
			bundle, dir)
	}
	return dir, nil
}

// loadBundleProfile — профиль bundle с диска (profile.json, иначе синтез).
// Повторяет правила Registry.loadProfile, но БЕЗ правок по содержимому
// (correctProfileByContent): они уже применены при последней загрузке реестра, а
// здесь профиль нужен только чтобы понять «какие роли заняты» и «какое семейство».
func (m *HFManager) loadBundleProfile(bundle, dir string) (*types.ImageModelProfile, error) {
	path := filepath.Join(dir, ProfileFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read %s: %w", ProfileFileName, err)
		}
		p, serr := SynthesizeProfile(bundle, dir)
		if serr != nil {
			return nil, componentErr(CompErrBundleNotFound,
				"model %q has no readable profile and no model files: %v", bundle, serr)
		}
		return p, nil
	}
	var p types.ImageModelProfile
	if err := json.Unmarshal(stripBOM(data), &p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ProfileFileName, err)
	}
	if p.Name == "" {
		p.Name = bundle
	}
	if p.Family == "" {
		p.Family = "other"
	}
	for i := range p.Files {
		if p.Files[i].LocalPath == "" {
			p.Files[i].LocalPath = filepath.Join(dir, p.Files[i].Filename)
		}
	}
	return &p, nil
}

// persistBundleProfile — записать профиль на диск (атомарно, общий путь с
// writeProfile).
func persistBundleProfile(dir string, p *types.ImageModelProfile) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal profile: %w", err)
	}
	return writeProfileFile(dir, data)
}

// resolveComponentFiles — проверка файлов в репозитории и их размеры.
//
// Один запрос к HF на репозиторий (ListModelFilesByFormat — тот же tree API, что
// у GET /api/hf/files). Возвращает ожидаемые файлы, суммарный размер и список
// «уже лежит в bundle» (их не качаем).
func (m *HFManager) resolveComponentFiles(repo, revision string, files []string, dir string) ([]ComponentExpectedFile, int64, []string, error) {
	ctx, cancel := context.WithTimeout(m.baseCtx, 60*time.Second)
	defer cancel()
	infos, err := m.downloader.ListModelFilesByFormat(ctx, repo, revision, cppbackend.ModelWeightExtensions)
	if err != nil {
		ce := componentErr(CompErrRepoUnavailable,
			"cannot list files of %s (revision %s): %v", repo, revision, err)
		ce.Repo = repo
		return nil, 0, nil, ce
	}
	byPath := make(map[string]cppbackend.HFFileInfo, len(infos))
	byBase := make(map[string]cppbackend.HFFileInfo, len(infos))
	available := make([]string, 0, len(infos))
	for _, info := range infos {
		byPath[info.Path] = info
		if _, ok := byBase[filepath.Base(info.Path)]; !ok {
			byBase[filepath.Base(info.Path)] = info
		}
		available = append(available, info.Path)
	}
	sort.Strings(available)
	if len(available) > 20 {
		available = available[:20]
	}

	expected := make([]ComponentExpectedFile, 0, len(files))
	reused := make([]string, 0, 1)
	var total int64
	for _, f := range files {
		info, ok := byPath[f]
		if !ok {
			info, ok = byBase[filepath.Base(f)]
		}
		if !ok {
			ce := componentErr(CompErrFileNotFound,
				"file %q is not in %s (revision %s)", f, repo, revision)
			ce.Repo, ce.Files, ce.Available = repo, append([]string(nil), files...), available
			return nil, 0, nil, ce
		}
		base := filepath.Base(info.Path)
		e := ComponentExpectedFile{
			Filename:   base,
			SourcePath: info.Path,
			SizeBytes:  info.SizeBytes,
			SizeMB:     (info.SizeBytes + 1024*1024 - 1) / (1024 * 1024),
		}
		if fi, serr := os.Stat(filepath.Join(dir, base)); serr == nil && !fi.IsDir() &&
			(info.SizeBytes == 0 || fi.Size() == info.SizeBytes) {
			e.Present = true
			reused = append(reused, base)
		}
		total += info.SizeBytes
		expected = append(expected, e)
	}
	return expected, total, reused, nil
}

// componentCatalogFacts — лицензия и предупреждение для ответа.
//
// Предупреждение строится ТОЛЬКО по фактам: «взято по умолчанию» и «лицензия X».
// Никаких «проверьте лицензию» без лицензии: пустое предупреждение лучше
// бессодержательного.
func componentCatalogFacts(family, role, repo string, isDefault bool) (license, warning string) {
	for _, c := range componentCatalog[family] {
		if c.Role == role && c.Repo == repo {
			license = c.License
			break
		}
	}
	if isDefault {
		warning = "component added from the family catalog (default choice)"
		if license != "" {
			warning += ", license: " + license
		}
		warning += " — verify the license before publishing generated images"
	}
	return license, warning
}

// runComponentInstall — фоновая установка: скачивание + правка профиля + реестр.
//
// ПОРЯДОК КРИТИЧЕН, и он другой, чем у bundle-пути в одном месте:
//
//  1. download → nil. При ошибке профиль НЕ трогается (иначе роль была бы
//     прописана, а файла на диске не было: движок получил бы --vae с
//     несуществующим путём, и «неполный набор» превратился бы в «движок не
//     стартует»);
//  2. profile.json + sidecar + Registry.Load;
//  3. и только ПОСЛЕ этого — терминальный статус «completed».
//
// ЗАЧЕМ ИМЕННО ТАКОЙ ПОРЯДОК ШАГОВ 2 И 3 (это найдено тестом, а не «на всякий
// случай»): UI опрашивает прогресс и по «completed» перезапрашивает
// GET /api/image/models, чтобы пересчитать вердикт. Если бы статус ставился
// раньше записи профиля, UI увидел бы «готово» и СТАРЫЙ вердикт («не хватает
// VAE») — ровно та же кнопка осталась бы на месте, и оператор нажал бы её снова.
func (m *HFManager) runComponentInstall(ctx context.Context, state *hfBundleState, req ComponentInstallRequest, downloads []cppbackend.HFDownloadRequest, dir, family string) {
	log := sdLog()
	bundle, role := strings.TrimSpace(req.Bundle), strings.TrimSpace(req.Role)
	log.Infow("component install started",
		"bundle", bundle, "role", role, "repo", req.Repo,
		"files", len(downloads), "targetDir", dir)

	progress, err := m.downloader.StartComponentDownload(ctx, downloads, state.id, dir)
	if err != nil {
		log.Errorw("component install download failed, profile NOT changed",
			"bundle", bundle, "role", role, "error", err)
		m.finishBundleState(state, progress, err)
		return
	}
	if err := m.applyComponentToBundle(req, downloads, dir, family); err != nil {
		log.Errorw("component install: files are on disk, profile NOT changed",
			"bundle", bundle, "role", role, "error", err)
		m.finishBundleState(state, progress, err)
		return
	}
	m.finishBundleState(state, progress, nil)
	log.Infow("component added to image bundle",
		"bundle", bundle, "role", role, "repo", req.Repo, "files", len(downloads),
		"family", family, "default", req.Default)
}

// applyComponentToBundle — правка профиля и перезагрузка реестра под registerMu.
//
// Ошибка = профиль не изменён (файлы при этом остаются на диске: повторный
// вызов их не перекачает — загрузчик пропускает уже лежащие файлы).
func (m *HFManager) applyComponentToBundle(req ComponentInstallRequest, downloads []cppbackend.HFDownloadRequest, dir, family string) error {
	bundle, role := strings.TrimSpace(req.Bundle), strings.TrimSpace(req.Role)

	m.registerMu.Lock()
	defer m.registerMu.Unlock()

	// Профиль перечитывается ЗДЕСЬ (а не берётся из постановки): пока шла
	// загрузка, параллельная установка могла добавить другую роль, и правка
	// обязана лечь поверх неё, а не затереть.
	profile, perr := m.loadBundleProfile(bundle, dir)
	if perr != nil {
		return fmt.Errorf("read profile after download: %w", perr)
	}
	added := make([]types.ImageModelFile, 0, len(downloads))
	for _, d := range downloads {
		base := filepath.Base(d.Filename)
		local := filepath.Join(dir, base)
		fi, serr := os.Stat(local)
		if serr != nil || fi.IsDir() {
			return fmt.Errorf("downloaded file is missing on disk: %s", base)
		}
		added = append(added, types.ImageModelFile{
			Role: role, Repo: d.ModelID, Filename: base, Revision: d.Revision,
			LocalPath: local, SizeBytes: fi.Size(),
		})
	}
	newProfile, werr := applyComponents(profile, added, req)
	if werr != nil {
		return werr
	}
	if err := persistBundleProfile(dir, newProfile); err != nil {
		return fmt.Errorf("write profile: %w", err)
	}
	// Запись «что запускали» (sd-server.config.json) — АДДИТИВНО: файла может не
	// быть (модель ещё ни разу не грузили), тогда и трогать нечего.
	syncSidecarComponents(dir, req, added)
	if err := m.registry.Load(); err != nil {
		return fmt.Errorf("registry reload after component install: %w", err)
	}
	return nil
}

// applyComponents — правка профиля: добавить файлы компонента и записать на диск.
//
// ПРАВИЛА:
//   - роль не добавляется второй раз (защита от гонки двух установок);
//   - существующие записи профиля не меняются и не удаляются;
//   - Notes получает строку о происхождении компонента (её видно в UI);
//   - sd-server.config.json (запись «что запускали») дополняется полем
//     components — АДДИТИВНО, без потери существующих полей и без изменения
//     контракта самого файла (его читает наш же супервизор; движку он
//     передаётся только если оператор запускает вручную с --config).
func applyComponents(p *types.ImageModelProfile, added []types.ImageModelFile, req ComponentInstallRequest) (*types.ImageModelProfile, error) {
	if p == nil {
		return nil, fmt.Errorf("nil profile")
	}
	if len(added) == 0 {
		return p, nil
	}
	role := strings.TrimSpace(req.Role)
	for _, f := range p.Files {
		if f.Role == role {
			return nil, componentErr(CompErrRolePresent,
				"role %q appeared in the profile while the component was downloading: %s",
				role, f.Filename)
		}
	}
	p.Files = append(p.Files, added...)
	sort.SliceStable(p.Files, func(i, j int) bool { return p.Files[i].Role < p.Files[j].Role })
	note := "компонент добавлен из " + strings.TrimSpace(req.Repo) + ": роль " + role + " → "
	names := make([]string, 0, len(added))
	for _, f := range added {
		names = append(names, f.Filename)
	}
	note += strings.Join(names, ", ")
	if req.Default {
		note += " (стандартный компонент семейства — проверьте лицензию)"
	}
	p.Notes = strings.TrimSpace(strings.TrimSpace(p.Notes + " " + note + "."))
	if err := types.ValidateImageModelProfile(p); err != nil && !types.IsMissingRequiredRole(err) {
		return nil, fmt.Errorf("profile is invalid after adding the component: %w", err)
	}
	return p, nil
}

// sidecarComponentsEntry — одна запись о добавленном компоненте в
// sd-server.config.json.
type sidecarComponentsEntry struct {
	Role    string   `json:"role"`
	Repo    string   `json:"repo"`
	Files   []string `json:"files"`
	Default bool     `json:"default,omitempty"`
	License string   `json:"license,omitempty"`
	AddedAt string   `json:"addedAt"`
}

// syncSidecarComponents — дополнить sd-server.config.json записью о компоненте.
//
// ЗАЧЕМ. profile.json — единственный АВТОРИТЕТНЫЙ источник ролей (его читает
// реестр и из него строятся флаги движка). Но рядом лежит sd-server.config.json —
// запись «что именно мы запускали», которую супервизор пишет при каждой загрузке
// (supervisor.go:writeSidecar). После добавления компонента она устаревает: в ней
// нет ни одного упоминания о новом VAE/энкодере. Поэтому дописываем АДДИТИВНОЕ
// поле `components` — существующие поля остаются байт-в-байт теми же, а состав
// набора становится виден и в этой записи.
//
// ЧЕГО ЗДЕСЬ СОЗНАТЕЛЬНО НЕТ: создания файла, если его нет. Файл — следствие
// ЗАПУСКА движка; выдумывать его при скачивании файла значило бы утверждать, что
// модель уже грузили.
func syncSidecarComponents(dir string, req ComponentInstallRequest, added []types.ImageModelFile) {
	path := filepath.Join(dir, "sd-server.config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var doc map[string]any
	if err := json.Unmarshal(stripBOM(data), &doc); err != nil || doc == nil {
		return
	}
	names := make([]string, 0, len(added))
	for _, f := range added {
		names = append(names, f.Filename)
	}
	raw, _ := doc["components"].([]any)
	raw = append(raw, map[string]any{
		"role":    strings.TrimSpace(req.Role),
		"repo":    strings.TrimSpace(req.Repo),
		"files":   names,
		"default": req.Default,
		"addedAt": time.Now().UTC().Format(time.RFC3339),
	})
	doc["components"] = raw
	out, merr := json.MarshalIndent(doc, "", "  ")
	if merr != nil {
		return
	}
	tmp := path + ".tmp"
	if werr := os.WriteFile(tmp, out, 0o644); werr != nil {
		return
	}
	if rerr := os.Rename(tmp, path); rerr != nil {
		_ = os.Remove(tmp)
	}
}

// normalizeComponentFiles — trim + отбрасывание пустых, без изменения порядка.
func normalizeComponentFiles(files []string) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		if s := strings.TrimSpace(f); s != "" {
			out = append(out, s)
		}
	}
	return out
}
