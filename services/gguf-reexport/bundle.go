// bundle.go — Фаза 4 (2026-11-24): цель `image_bundle` — «набор файлов → bundle».
//
// ЗАЧЕМ. Image-модель для imageworker — это КАТАЛОГ с profile.json (R696):
// движок получает --diffusion-model/--vae/--clip_l/--llm … по ролям из профиля,
// а роль файла определяется по ЗАГОЛОВКУ, а не по имени. Скачанный «набор
// файлов» без такого каталога для воркера либо невидим, либо регистрируется
// синтезированным профилем без явных ролей. Эта цель превращает каталог с уже
// скачанными GGUF/safetensors в bundle, который воркер загрузит: раскладывает
// файлы, пишет profile.json и sd-server.config.json, считает ТОТ ЖЕ вердикт
// совместимости, что покажет вкладка моделей (R696), и просит imageworker
// перечитать каталог.
//
// ПОЧЕМУ В GO, А НЕ В PYTHON-ДРАЙВЕРЕ. Полный ре-экспорт (hf_to_gguf) живёт в
// python, потому что это конвертер llama.cpp. Здесь конвертера нет вовсе: роли
// решает Go-парсер заголовков, раскладка — копирование файлов, вердикт — тот же
// код, что на воркере. Гонять ради этого python значило бы требовать образ с
// torch там, где хватает Go, поэтому цель доступна и в light-сборке
// (capabilities.go). Код возврата драйвера (runner.go) здесь не участвует.
//
// ЧТО ПЕРЕИСПОЛЬЗУЕТСЯ ИЗ ВОРКЕРА (правила не дублируются):
//   - imagemodel.SuggestRole — роль по имени/пути файла (та же функция, что у
//     страницы моделей и HF-вкладки);
//   - imagemodel.ProbeLocalFile — роль и семейство по ЗАГОЛОВКУ (R696);
//   - types.ValidateImageModelProfile — контракт профиля (роли, дефолты, лимиты);
//   - imagemodel.BuildBundleCompat — вердикт «воркер сможет загрузить набор».
//
// ЧЕМ ПОРЯДОК РОЛЕЙ ОТЛИЧАЕТСЯ ОТ imagemodel.SynthesizeProfile И ПОЧЕМУ.
// Синтез профиля ставит роль diffusion и файлу, чьё семейство выведено ТОЛЬКО из
// general.architecture: живой VAE FLUX (ae.safetensors, arch "Flux.1-AE")
// получает роль diffusion, настоящий diffusion-файл выпадает как «дубликат
// роли», и набор честно называется неполным (нет VAE). Здесь действует правило
// правки профиля (imagemodel.correctProfileByContent): содержимое перебивает имя
// ТОЛЬКО для не-diffusion ролей. Поэтому файл, названный компонентом
// (ae/vae/clip/t5), роль компонента сохраняет, а семейство bundle берётся из
// ТЕНЗОРОВ настоящего diffusion-файла. Отличие записано здесь, а не спрятано:
// на нём стоит живой прогон flux-набора (см. отчёт фазы 4).
//
// ГРАНИЦЫ (те же, что у остального сервиса):
//  1. ВЫХОД ТОЛЬКО ВНУТРИ ${MODELS_DIR} (resolveBundleOutput): bundle мимо тома
//     моделей воркер не увидит, поэтому отказ — path_not_allowed;
//  2. МЕСТО ПРОВЕРЯЕТСЯ ДО СТАРТА (space.go): копия набора — это «вход + выход +
//     запас», в режиме «манифест в существующем каталоге» писать нечего;
//  3. ПРОГРЕСС — контрактные стадии reading → writing → validating; проценты
//     стадии writing считаются по скопированным байтам;
//  4. ИСХОДНИКИ НЕ МЕНЯЮТСЯ НИКОГДА: файлы только читаются, манифест пишется в
//     каталог bundle (или в сам каталог набора, если он им и является).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"ollama-loadbalancer/internal/imagemodel"
	"ollama-loadbalancer/pkg/types"
)

// Имена файлов внутри bundle. profile.json — авторитетный источник ролей (его
// читает реестр воркера), sd-server.config.json — запись «что именно мы
// запускаем»: тот же файл, что пишет супервизор при загрузке
// (internal/sdbackend/supervisor.go:writeSidecar), поэтому формат общий.
const (
	bundleProfileName = imagemodel.ProfileFileName
	bundleSidecarName = "sd-server.config.json"
)

// bundleMaxDepth — та же граница вложенности, что у синтеза профиля воркера:
// DiT-репозитории глубже двух уровней (vae/, text_encoders/, split_files/vae/)
// не раскладывают, а случайные каталоги внутри набора сканировать незачем.
const bundleMaxDepth = 2

// bundleCopyChunk — размер буфера копирования и шаг вывода прогресса. 8 МиБ —
// компромисс: событие на каждые 8 МиБ не засоряет хвост лога и при этом 16 ГиБ
// файл даёт ~2000 обновлений, а не одно «копирую...» на десять минут.
const bundleCopyChunk = 8 << 20

// bundleRoleOrder — порядок файлов в profile.json: как у движка в argv
// (types.ImageModelProfile.ServerArgs), diffusion первым.
//
// ПОЧЕМУ БЕЗ ЭТОГО НЕЛЬЗЯ. Вердикт compat выводит семейство из ПЕРВОГО файла
// роли diffusion, у которого распознано семейство. Если первым окажется VAE с
// arch-подсказкой «Flux.1-AE», семейство помечается как выведенное из arch
// (движок его не читает), и честный ready превращается в unknown — то есть
// рабочему набору сообщается «движок может не понять». Diffusion первым — это
// порядок движка, а не перестановка ради вердикта: ровно так лежат файлы в
// профилях, которые пишет HF-слой воркера.
var bundleRoleOrder = []string{
	types.ImageFileRoleDiffusion,
	types.ImageFileRoleVae,
	types.ImageFileRoleClipL,
	types.ImageFileRoleClipG,
	types.ImageFileRoleT5xxl,
	types.ImageFileRoleLLM,
	types.ImageFileRoleClipVision,
	types.ImageFileRoleControlNet,
	types.ImageFileRoleIPAdapter,
	types.ImageFileRoleUpscaler,
	types.ImageFileRoleTaesd,
	types.ImageFileRoleLora,
}

// bundleRoleRank — вес роли в порядке файлов (неизвестная роль — в конец).
func bundleRoleRank(role string) int {
	for i, r := range bundleRoleOrder {
		if r == role {
			return i
		}
	}
	return len(bundleRoleOrder)
}

// bundleFile — файл набора: путь в наборе, роль и разбор заголовка.
type bundleFile struct {
	// Rel — путь ВНУТРИ набора (слэши): он же Filename в profile.json.
	Rel string
	// Abs — путь источника (для чтения; в профиль не попадает).
	Abs string
	// Role — роль в bundle (после заголовка, правил и переопределений).
	Role string
	// Size — размер источника (для прогресса и раскладки места).
	Size int64
	// Probe — разбор заголовка (nil, если файл не прочитался: .ckpt/.pt и т.п.).
	Probe *imagemodel.HFProbeResult
}

// bundlePlan — что и куда кладём, ДО записи. Его печатает dry-run и по нему
// идёт копирование: ошибки раскладки видно до того, как диск начали трогать.
type bundlePlan struct {
	Name    string
	SrcDir  string
	OutDir  string
	Profile *types.ImageModelProfile
	Files   []bundleFile
	// InPlace — bundle собирается в каталоге самого набора (манифест на месте):
	// файлы не копируются, воркер видит набор по тому же пути.
	InPlace bool
	// Bytes — сколько скопировать (в режиме InPlace — 0).
	Bytes int64
	// Changes — человеческие строки «что изменилось» (их видит оператор).
	Changes []string
	// Source — происхождение ролей: profile_json | headers.
	Source string
}

// ---------------- исполнитель ----------------

// bundleRunner — исполнитель цели image_bundle (интерфейс jobRunner, как у
// pythonRunner: менеджер не знает, чем именно исполняется задача).
type bundleRunner struct {
	log *zap.SugaredLogger
	// worker — imageworker для пересканирования и проверки вердикта. nil =
	// REEXPORT_IMAGEWORKER_URL не задан: проверка остаётся локальной, и задача
	// ЧЕСТНО пишет worker_checked=false, а не «проверено воркером».
	worker bundleWorker
	now    func() time.Time
}

func newBundleRunner(cfg config, log *zap.SugaredLogger) *bundleRunner {
	var w bundleWorker
	if cfg.ImageWorkerURL != "" {
		w = &httpBundleWorker{
			baseURL: cfg.ImageWorkerURL,
			token:   cfg.APIToken,
			client:  &http.Client{Timeout: 30 * time.Second},
		}
	}
	return &bundleRunner{log: log, worker: w, now: time.Now}
}

// Run собирает bundle и отдаёт события контракта (progress/log/result/error).
//
// ПОРЯДОК ШАГОВ КРИТИЧЕН и повторяет контракт §5 плана:
//
//	reading    — заголовки файлов, роли, семейство, вердикт «до сборки»;
//	writing    — копирование файлов и запись profile.json + sd-server.config.json;
//	validating — вердикт по СОБРАННОМУ каталогу и проверка через imageworker.
//
// Раньше validating выглядел бы как «формальность»: он считается тем же кодом,
// что и reading, но по ДРУГОМУ каталогу — именно он доказывает, что манифест
// лёг рядом с файлами и воркер увидит тот же набор.
func (b *bundleRunner) Run(ctx context.Context, req runRequest, sink func(runEvent)) error {
	started := b.now()
	opts := req.Options
	// bundleName из пути результата: имя каталога — это имя модели для воркера,
	// и оно же попадает в profile.json (реестр ключует модели по каталогу).
	name := filepath.Base(req.OutPath)

	sink(runEvent{Type: "progress", Progress: jobProgress{Phase: "plan", Stage: "reading", Percent: 2}})
	plan, err := composeBundlePlan(name, req.SrcPath, req.OutPath, opts.Roles)
	if err != nil {
		return err
	}
	sink(runEvent{Type: "log", Line: fmt.Sprintf("набор: %d файлов, роли по %s; семейство %q",
		len(plan.Files), rolesSourceLabel(plan.Source), plan.Profile.Family)})
	for _, f := range plan.Files {
		line := fmt.Sprintf("роль %-10s ← %s", f.Role, f.Rel)
		if f.Probe != nil && f.Probe.Role != "" && f.Probe.Role != f.Role {
			line += fmt.Sprintf(" (по заголовку %s: %s)", f.Probe.Role, f.Probe.Reason)
		}
		sink(runEvent{Type: "log", Line: line})
	}

	// Вердикт ДО записи: он называется в итоге задачи и в отчёте, и на нём
	// видно, что раскладка ролей не выдумана — её подтверждает R696-код.
	before := imagemodel.BuildBundleCompat(plan.Profile, plan.SrcDir)
	sink(runEvent{Type: "log", Line: "вердикт по набору до сборки: " + compatLine(before)})

	if opts.DryRun {
		sink(runEvent{Type: "progress", Progress: jobProgress{Phase: "plan", Stage: "reading", Percent: 100}})
		sink(runEvent{Type: "result", Result: &jobResult{
			DurationMS: b.now().Sub(started).Milliseconds(),
			SrcBytes:   planBytes(plan),
			Facts:      bundleFacts(plan, before, nil, nil, false),
			Changes:    append([]string{"пробный прогон: файлы не копировались, манифест не писался"}, plan.Changes...),
			Verify:     bundleVerify(before, nil, nil, plan, false),
		}})
		return nil
	}

	// ---- writing ----
	sink(runEvent{Type: "log", Line: fmt.Sprintf("результат: %s", plan.OutDir)})
	written, reused, err := b.writeBundle(ctx, plan, sink)
	if err != nil {
		return err
	}
	manifestPath, err := writeBundleProfile(plan.OutDir, plan.Profile)
	if err != nil {
		return &jobError{Code: "validation_failed", Message: fmt.Sprintf(
			"файлы разложены, но %s не записан: %v", bundleProfileName, err)}
	}
	sink(runEvent{Type: "log", Line: "записан " + manifestPath})
	sidecarPath, err := writeBundleSidecar(plan.OutDir, plan.Profile)
	if err != nil {
		return &jobError{Code: "validation_failed", Message: fmt.Sprintf(
			"профиль записан, но %s не записан: %v", bundleSidecarName, err)}
	}
	sink(runEvent{Type: "log", Line: "записан " + sidecarPath})

	// ---- validating ----
	sink(runEvent{Type: "progress", Progress: jobProgress{Phase: "verify", Stage: "validating", Percent: 90}})
	after := imagemodel.BuildBundleCompat(plan.Profile, plan.OutDir)
	sink(runEvent{Type: "log", Line: "вердикт по собранному bundle: " + compatLine(after)})

	worker, rescanned, werr := b.checkWorker(ctx, name)
	// МОЖНО ЛИ ВЕРИТЬ ВЕРДИКТУ ВОРКЕРА КАК ОКОНЧАТЕЛЬНОМУ. Новый каталог bundle
	// меняет набор каталогов, и воркер перечитывает реестр даже обычным GET. Запись
	// «на месте» набор не меняет, поэтому воркер, собранный до Фазы 4 (без
	// POST /api/image/models/scan), покажет профиль, синтезированный ДО нашей
	// записи. Это не повод объявлять набор несобираемым — но и молчать нельзя.
	trusted := !plan.InPlace || rescanned
	changes := append([]string{}, plan.Changes...)
	changes = append(changes,
		fmt.Sprintf("profile.json: %d файл(ов), семейство %q", len(plan.Profile.Files), plan.Profile.Family),
		fmt.Sprintf("sd-server.config.json: роль → флаг движка (%s)", engineModeLabel(after)))
	if reused > 0 {
		changes = append(changes, fmt.Sprintf("уже были на месте, не копировались: %d файл(ов)", reused))
	}
	if worker != nil && worker.Found {
		via := "POST /api/image/models/scan"
		if !rescanned {
			via = "GET /api/image/models (принудительный скан воркер не умеет)"
		}
		changes = append(changes, fmt.Sprintf("imageworker видит bundle (%s): %s", via, worker.Verdict))
	}

	sink(runEvent{Type: "progress", Progress: jobProgress{Phase: "verify", Stage: "validating", Percent: 100}})
	sink(runEvent{Type: "result", Result: &jobResult{
		DurationMS: b.now().Sub(started).Milliseconds(),
		SrcBytes:   planBytes(plan),
		DstBytes:   written,
		Facts:      bundleFacts(plan, after, worker, werr, rescanned),
		Changes:    changes,
		Verify:     bundleVerify(after, worker, werr, plan, rescanned),
	}})

	// ИТОГ ПО ВЕРДИКТУ, А НЕ ПО «ФАЙЛЫ ЗАПИСАЛИСЬ». Задача, которая оставила
	// набор, заведомо не поднимаемый движком, обязана быть failed с кодом и
	// причиной: иначе оператор узнает об этом только при загрузке.
	if !after.Runnable {
		return &jobError{
			Code: "bundle_not_ready",
			Message: fmt.Sprintf("bundle %q собран, но загрузить его не получится: %s (не хватает: %s)",
				name, after.Reason, orMissing(after.Missing)),
			Detail: "profile.json записан; причина и роли — в result.facts",
		}
	}
	// ВОРКЕР — ОБЯЗАТЕЛЬНАЯ ЧАСТЬ ПРОВЕРКИ, если он настроен. «Собрали и не
	// проверили» — это не success по контракту §8 плана: файл может быть виден
	// нам и не виден воркеру (другой том, кэш каталога).
	if b.worker != nil {
		if werr != nil {
			return &jobError{
				Code: "worker_unavailable",
				Message: fmt.Sprintf("bundle собран и вердикт локально «%s», но imageworker не ответил: %v; "+
					"проверка не выполнена — файлы оставлены, повторите проверку после восстановления воркера", after.Verdict, werr),
				Detail: "REEXPORT_IMAGEWORKER_URL=" + workerURL(b.worker),
			}
		}
		if worker != nil && !worker.Found {
			return &jobError{
				Code: "bundle_not_visible",
				Message: fmt.Sprintf("imageworker не видит bundle %q: каталог создан, но в GET /api/image/models его нет — "+
					"проверьте, что воркер смотрит на тот же том моделей (%s)", name, plan.OutDir),
			}
		}
		if worker != nil && !worker.Runnable {
			if !trusted {
				// Воркер мог не перечитать профиль (запись на месте + старый образ).
				// Не врём «готово»: пишем это в итог задачи отдельным полем и
				// строкой лога, а вердикт оставляем локальным (тот же R696-код).
				sink(runEvent{Type: "log", Line: "ВНИМАНИЕ: imageworker видит старый профиль (без " +
					"POST /api/image/models/scan он не перечитывает запись «на месте»); его вердикт " +
					worker.Verdict + " — это не вердикт по собранному набору"})
				changes = append(changes, "воркер не умеет принудительный скан: его вердикт по записи «на месте» может быть устаревшим")
				return nil
			}
			return &jobError{
				Code: "bundle_not_ready",
				Message: fmt.Sprintf("imageworker видит bundle %q, но загрузить его не сможет: %s (не хватает: %s)",
					name, worker.Reason, orMissing(worker.Missing)),
			}
		}
	}
	return nil
}

// ---------------- раскладка ----------------

// composeBundlePlan собирает профиль по файлам набора и план записи.
//
// ИСТОЧНИК РОЛЕЙ — ПО ПРИОРИТЕТУ:
//  1. profile.json В САМОМ НАБОРЕ: если он есть, его роли — выбор оператора/UI
//     (именно так выглядят наборы, скачанные HF-слоем воркера). Перебивать его
//     содержимым нельзя: это ровно тот случай, когда имя файла врёт, а профиль
//     (написанный после разбора заголовков и выбора оператора) — нет;
//  2. иначе — заголовки файлов (R696): роль по имени, поверх неё роль по
//     содержимому для всех НЕ-diffusion ролей (см. шапку файла).
//
// Затем применяются options.roles (явное переопределение оператора) — ДО
// устранения дубликатов: иначе файл, выпавший «лишним квантом», нельзя было бы
// вернуть в набор подсказкой роли.
func composeBundlePlan(name, srcDir, outDir string, overrides map[string]string) (*bundlePlan, error) {
	srcDir = filepath.Clean(srcDir)
	outDir = filepath.Clean(outDir)
	plan := &bundlePlan{Name: name, SrcDir: srcDir, OutDir: outDir, InPlace: samePath(srcDir, outDir)}

	files, source, err := scanBundleFiles(srcDir)
	if err != nil {
		return nil, &jobError{Code: "input_missing", Message: fmt.Sprintf("каталог набора %s не прочитан: %v", srcDir, err)}
	}
	if len(files) == 0 {
		return nil, &jobError{Code: "input_missing", Message: fmt.Sprintf(
			"в каталоге %s нет файлов весов (*.gguf/*.safetensors/*.ckpt/*.pt): собирать bundle не из чего", srcDir)}
	}
	plan.Source = source
	plan.Files = files

	if err := applyRoleOverrides(plan, overrides); err != nil {
		return nil, err
	}
	plan.Files, plan.Changes = dedupeBundleRoles(plan.Files, plan.Changes)
	plan.Files, plan.Changes = ensureDiffusionRole(plan.Files, plan.Changes)
	family, famNote := bundleFamily(plan.Files)
	if famNote != "" {
		plan.Changes = append(plan.Changes, famNote)
	}

	profileName := name
	if plan.InPlace {
		// Имя модели — имя КАТАЛОГА: реестр воркера ключует модели по нему, и
		// профиль с другим name() просто не нашёлся бы.
		profileName = filepath.Base(srcDir)
	}
	profile := &types.ImageModelProfile{
		Name:     profileName,
		Family:   family,
		Defaults: types.DefaultImageGenDefaults(family),
		Files:    make([]types.ImageModelFile, 0, len(plan.Files)),
	}
	// Дефолты и runtime из профиля набора сохраняются: это выбор оператора
	// (steps/cfg/плейсмент), и перезаписывать его нашими умолчаниями нельзя.
	if existing := existingSourceProfile(srcDir); existing != nil {
		if existing.Defaults.Steps > 0 {
			profile.Defaults = existing.Defaults
		}
		profile.Runtime = existing.Runtime
		profile.VramEstimateMB = existing.VramEstimateMB
		profile.TimeoutSec = existing.TimeoutSec
		profile.IdleUnloadMinutes = existing.IdleUnloadMinutes
		profile.Strengths = existing.Strengths
		if existing.Family != "" && existing.Family != "other" && types.IsValidImageFamily(existing.Family) {
			// Явное семейство профиля сильнее выведенного: движок по arch его не
			// читает, а оператор мог выбрать семейство руками.
			profile.Family = existing.Family
			profile.Defaults = existing.Defaults
			family = existing.Family
		}
	}
	for _, f := range plan.Files {
		profile.Files = append(profile.Files, types.ImageModelFile{
			Role: f.Role,
			// repo "local": файлы уже на диске, а не в репозитории HF. То же
			// значение пишет синтез профиля воркера.
			Repo:      "local",
			Filename:  f.Rel,
			SizeBytes: f.Size,
		})
	}
	sortBundleFiles(profile.Files)
	profile.Notes = bundleNotes(plan, family)

	if err := types.ValidateImageModelProfile(profile); err != nil {
		// Неполный набор — состояние, а не отказ (R696): профиль обязан лечь на
		// диск, чтобы воркер показал модель и объяснил, чего не хватает. Всё
		// остальное (неизвестное семейство, дубли ролей, лимиты дефолтов) —
		// наша ошибка раскладки, и её видно сразу.
		if !types.IsMissingRequiredRole(err) {
			return nil, &jobError{Code: "validation_failed", Message: fmt.Sprintf(
				"профиль набора не проходит контракт image-bundle: %v", err)}
		}
		profile.Notes = strings.TrimSpace(profile.Notes + " Набор неполный: " + err.Error() + ".")
	}
	plan.Profile = profile
	if !plan.InPlace {
		for _, f := range plan.Files {
			plan.Bytes += f.Size
		}
	}
	plan.Changes = append(plan.Changes, fmt.Sprintf("роли: %d файл(ов) → %s", len(plan.Files), rolesList(plan.Files)))
	return plan, nil
}

// scanBundleFiles — файлы весов набора и ИСТОЧНИК их ролей.
//
// profile.json в наборе = роли оттуда (source="profile_json"), иначе роли
// считаются по заголовкам (source="headers").
func scanBundleFiles(srcDir string) ([]bundleFile, string, error) {
	if p := existingSourceProfile(srcDir); p != nil {
		files := make([]bundleFile, 0, len(p.Files))
		for _, f := range p.Files {
			rel := strings.TrimSpace(filepath.ToSlash(f.Filename))
			if rel == "" {
				continue
			}
			abs := filepath.Join(srcDir, filepath.FromSlash(rel))
			st, err := os.Stat(abs)
			if err != nil || st.IsDir() {
				return nil, "", &jobError{Code: "input_missing", Message: fmt.Sprintf(
					"%s %q из профиля набора: файла нет в %s", bundleProfileName, f.Filename, srcDir)}
			}
			files = append(files, bundleFile{Rel: rel, Abs: abs, Role: f.Role, Size: st.Size()})
		}
		if len(files) == 0 {
			return nil, "", &jobError{Code: "input_missing", Message: fmt.Sprintf(
				"%s в %s не содержит файлов весов", bundleProfileName, srcDir)}
		}
		probeBundleFiles(files)
		return files, "profile_json", nil
	}

	rels, err := bundleWeightFiles(srcDir)
	if err != nil {
		return nil, "", err
	}
	files := make([]bundleFile, 0, len(rels))
	for _, rel := range rels {
		abs := filepath.Join(srcDir, filepath.FromSlash(rel))
		st, err := os.Stat(abs)
		if err != nil || st.IsDir() {
			continue // нечитаемый файл не повод терять весь набор
		}
		files = append(files, bundleFile{Rel: rel, Abs: abs, Role: imagemodel.SuggestRole(rel), Size: st.Size()})
	}
	probeBundleFiles(files)
	// Содержимое сильнее имени — НО только для не-diffusion ролей (R696).
	// Файл, названный компонентом, роль компонента сохраняет: arch-подсказка
	// («Flux.1-AE» у VAE) не делает его diffusion-моделью, а настоящий
	// diffusion-файл не должен выпасть «лишним квантом».
	for i := range files {
		f := &files[i]
		if f.Probe == nil || f.Probe.Role == "" || f.Probe.Role == types.ImageFileRoleDiffusion {
			continue
		}
		f.Role = f.Probe.Role
	}
	return files, "headers", nil
}

// bundleWeightFiles — пути файлов весов внутри набора (слэши, сортировка).
func bundleWeightFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d == nil {
			return nil //nolint:nilerr // недоступный каталог не повод терять набор
		}
		if d.IsDir() {
			rel, rerr := filepath.Rel(root, p)
			if rerr == nil && strings.Count(filepath.ToSlash(rel), "/") > bundleMaxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == bundleProfileName || d.Name() == bundleSidecarName {
			return nil
		}
		// Фильтр расширений — экспорт того же правила, что у синтеза профиля:
		// иначе в набор попал бы README/config.json.
		if !imagemodel.IsImageWeightFile(d.Name()) {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out, err
}

// existingSourceProfile — profile.json набора, если он есть и читается.
//
// Битый профиль — не повод падать: роль тогда определяется по заголовкам, как у
// набора без профиля, а о проблеме оператор узнает из вердикта compat (внешний
// профиль он видит в тексте). Ошибку чтения НЕ глотаем молча: она попадает в
// changes плана (см. bundleNotes).
func existingSourceProfile(srcDir string) *types.ImageModelProfile {
	path := filepath.Join(srcDir, bundleProfileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var p types.ImageModelProfile
	// BOM — типовая проблема Windows-редакторов (тот же приём, что у реестра).
	data = stripBOMBytes(data)
	if err := json.Unmarshal(data, &p); err != nil {
		return nil
	}
	if len(p.Files) == 0 {
		return nil
	}
	return &p
}

// stripBOMBytes снимает UTF-8 BOM.
func stripBOMBytes(data []byte) []byte {
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		return data[3:]
	}
	return data
}

// probeBundleFiles читает заголовок каждого файла (веса не трогаются).
func probeBundleFiles(files []bundleFile) {
	for i := range files {
		res, err := imagemodel.ProbeLocalFile(files[i].Abs, files[i].Rel)
		if err != nil || res == nil {
			continue
		}
		files[i].Probe = res
	}
}

// validateBundleOptions — проверки опций цели ДО постановки задачи в очередь.
//
// ЗАЧЕМ ДО СТАРТА. Роль с опечаткой и подсказка файлу, которого в наборе нет,
// обнаружились бы только внутри задачи: оператор получил бы 202, подождал, и
// увидел failed вместо «поправь имя в форме». Обе проверки дешёвые: карта ролей и
// имена файлов (заголовки не читаются — это делает задача).
func validateBundleOptions(src string, opts jobOptions) error {
	if len(opts.Roles) == 0 {
		return nil
	}
	keys := make([]string, 0, len(opts.Roles))
	for key := range opts.Roles {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	files, err := bundleWeightFiles(src)
	if err != nil {
		return nil // недоступный каталог объяснит исполнитель задачи
	}
	known := make(map[string]bool, len(files)*2)
	for _, rel := range files {
		known[rel] = true
		known[filepath.Base(rel)] = true
	}
	for _, key := range keys {
		role := strings.TrimSpace(opts.Roles[key])
		if !types.IsValidImageFileRole(role) {
			return &jobError{Code: "invalid_request", Message: fmt.Sprintf(
				"roles[%q]=%q: неизвестная роль image-bundle (допустимы: diffusion, vae, clip_l, clip_g, t5xxl, llm, clip_vision, …)", key, role)}
		}
		if !known[filepath.ToSlash(filepath.Clean(key))] && !known[filepath.Base(key)] {
			return &jobError{Code: "invalid_request", Message: fmt.Sprintf(
				"roles[%q]: файла нет в наборе %s (файлы весов: %s)", key, src, strings.Join(files, ", "))}
		}
	}
	return nil
}

// applyRoleOverrides — переопределения ролей оператором (options.roles).
//
// Ключ — имя файла в наборе (принимается и basename): форма отправляет ровно то
// имя, которое видно в отчёте. Роль обязана быть из контракта pkg/types, а файл
// — существовать в наборе: «переопределил несуществующее» иначе прошло бы
// молча и ничего не исправило.
func applyRoleOverrides(plan *bundlePlan, overrides map[string]string) error {
	if len(overrides) == 0 {
		return nil
	}
	byBase := make(map[string]int, len(plan.Files))
	for i := range plan.Files {
		byBase[filepath.Base(plan.Files[i].Rel)] = i
	}
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys) // детерминированный порядок изменений в отчёте
	for _, key := range keys {
		want := strings.TrimSpace(overrides[key])
		if !types.IsValidImageFileRole(want) {
			return &jobError{Code: "invalid_request", Message: fmt.Sprintf(
				"roles[%q]=%q: неизвестная роль image-bundle", key, want)}
		}
		idx := -1
		for i := range plan.Files {
			if plan.Files[i].Rel == filepath.ToSlash(filepath.Clean(key)) {
				idx = i
				break
			}
		}
		if idx < 0 {
			if i, ok := byBase[filepath.Base(key)]; ok {
				idx = i
			}
		}
		if idx < 0 {
			return &jobError{Code: "invalid_request", Message: fmt.Sprintf(
				"roles[%q]: файла нет в наборе (есть: %s)", key, knownFiles(plan.Files))}
		}
		if plan.Files[idx].Role != want {
			plan.Changes = append(plan.Changes, fmt.Sprintf(
				"роль %s переопределена оператором: %s → %s", plan.Files[idx].Rel, plan.Files[idx].Role, want))
			plan.Files[idx].Role = want
		}
	}
	return nil
}

// dedupeBundleRoles — одна роль = один файл (кроме LoRA): контракт
// ValidateImageModelProfile запрещает дубли, а движок получил бы два --vae.
func dedupeBundleRoles(files []bundleFile, changes []string) ([]bundleFile, []string) {
	sort.SliceStable(files, func(i, j int) bool {
		ri, rj := bundleRoleRank(files[i].Role), bundleRoleRank(files[j].Role)
		if ri != rj {
			return ri < rj
		}
		return files[i].Rel < files[j].Rel
	})
	kept := make([]bundleFile, 0, len(files))
	seen := map[string]bool{}
	var skipped []string
	for _, f := range files {
		if f.Role != types.ImageFileRoleLora && seen[f.Role] {
			skipped = append(skipped, f.Rel+" ("+f.Role+")")
			continue
		}
		seen[f.Role] = true
		kept = append(kept, f)
	}
	if len(skipped) > 0 {
		changes = append(changes, "в профиль не попали дубликаты роли (движок берёт один файл на роль): "+
			strings.Join(skipped, ", "))
	}
	return kept, changes
}

// ensureDiffusionRole — если diffusion-файла не нашлось, роль занимает самый
// крупный файл (правило синтеза профиля воркера).
//
// ЗАЧЕМ НЕ ОТКАЗЫВАТЬ СРАЗУ: профиль без diffusion не проходит контракт, каталог
// выпал бы из реестра, и оператор не увидел бы ни модели, ни причины. Так набор
// регистрируется, а вердикт compat честно говорит «роль занята не diffusion».
func ensureDiffusionRole(files []bundleFile, changes []string) ([]bundleFile, []string) {
	var biggest = -1
	for i := range files {
		if files[i].Role == types.ImageFileRoleDiffusion {
			return files, changes
		}
		if biggest < 0 || files[i].Size > files[biggest].Size {
			biggest = i
		}
	}
	if biggest < 0 {
		return files, changes
	}
	changes = append(changes, fmt.Sprintf(
		"diffusion-файл не определён: роль diffusion назначена крупнейшему файлу %s — проверьте роли",
		files[biggest].Rel))
	files[biggest].Role = types.ImageFileRoleDiffusion
	return files, changes
}

// bundleFamily — семейство по заголовкам diffusion-файлов.
//
// Правило синтеза профиля: семейство берётся из diffusion-файла, чьи ТЕНЗОРЫ
// распознаны; подсказка из general.architecture тоже принимается, но помечается
// (движок arch не читает — docs/features/image-generation.ru.md §8.2). Если
// кандидаты противоречат друг другу — "other": пусть движок судит сам, а вердикт
// compat скажет почему.
func bundleFamily(files []bundleFile) (string, string) {
	family, note := "", ""
	for _, f := range files {
		if f.Role != types.ImageFileRoleDiffusion || f.Probe == nil {
			continue
		}
		if f.Probe.Family == "" || f.Probe.Family == "other" || !types.IsValidImageFamily(f.Probe.Family) {
			continue
		}
		if family == "" {
			family = f.Probe.Family
			if f.Probe.FamilySource == imagemodel.ProbeFamilyFromArch {
				note = fmt.Sprintf("семейство %q выведено из general.architecture файла %s — это подсказка, "+
					"движок её не читает; проверьте семейство в профиле", family, f.Rel)
			}
			continue
		}
		if family != f.Probe.Family {
			return "other", fmt.Sprintf("diffusion-файлы указывают на разные семейства (%s и %s): "+
				"оставлено other, движок решит сам", family, f.Probe.Family)
		}
	}
	if family == "" {
		family = "other"
	}
	return family, note
}

// sortBundleFiles — порядок файлов профиля как у движка (bundleRoleOrder), иначе
// вердикт compat может вывести семейство из arch-подсказки VAE.
func sortBundleFiles(files []types.ImageModelFile) {
	sort.SliceStable(files, func(i, j int) bool {
		ri, rj := bundleRoleRank(files[i].Role), bundleRoleRank(files[j].Role)
		if ri != rj {
			return ri < rj
		}
		return files[i].Filename < files[j].Filename
	})
}

// ---------------- запись ----------------

// writeBundle — копирование файлов набора в каталог bundle.
//
// ВОЗВРАЩАЕТ скопированные байты и число уже лежавших файлов: повторный запуск
// идемпотентен (файл того же размера не копируется — как у компонентной
// установки R710), а оператор видит это в итоге задачи.
func (b *bundleRunner) writeBundle(ctx context.Context, plan *bundlePlan, sink func(runEvent)) (int64, int, error) {
	if plan.InPlace {
		// Манифест пишется в сам набор: копировать нечего, и это отдельный режим,
		// а не «оптимизация» — иначе 8 ГиБ копировались бы ради двух JSON.
		sink(runEvent{Type: "log", Line: "каталог набора уже является каталогом bundle: файлы не копируются"})
		if err := os.MkdirAll(plan.OutDir, 0o755); err != nil {
			return 0, 0, &jobError{Code: "dst_unwritable", Message: fmt.Sprintf("каталог %s: %v", plan.OutDir, err)}
		}
		return 0, 0, nil
	}
	if err := os.MkdirAll(plan.OutDir, 0o755); err != nil {
		return 0, 0, &jobError{Code: "dst_unwritable", Message: fmt.Sprintf("каталог bundle %s: %v", plan.OutDir, err)}
	}
	total := plan.Bytes
	var done, written int64
	reused := 0
	sink(runEvent{Type: "progress", Progress: jobProgress{Phase: "copy", Stage: "writing", Percent: 10,
		BytesTotal: total}})

	for _, f := range plan.Files {
		if err := ctx.Err(); err != nil {
			sink(runEvent{Type: "log", Line: "копирование прервано: bundle неполный, profile.json не записан"})
			return written, reused, &jobError{Code: "canceled", Message: "задача отменена оператором"}
		}
		dst := filepath.Join(plan.OutDir, filepath.FromSlash(f.Rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return written, reused, &jobError{Code: "dst_unwritable", Message: fmt.Sprintf("каталог %s: %v", filepath.Dir(dst), err)}
		}
		if st, err := os.Stat(dst); err == nil && !st.IsDir() && st.Size() == f.Size {
			reused++
			done += f.Size
			sink(runEvent{Type: "log", Line: fmt.Sprintf("уже на месте (%d Б), не копируем: %s", f.Size, f.Rel)})
			sink(runEvent{Type: "progress", Progress: bundleCopyProgress(done, total)})
			continue
		}
		n, err := copyBundleFile(ctx, f.Abs, dst, func(n64 int64) {
			sink(runEvent{Type: "progress", Progress: bundleCopyProgress(done+n64, total)})
		})
		if err != nil {
			if ctx.Err() != nil {
				sink(runEvent{Type: "log", Line: "копирование прервано: bundle неполный, profile.json не записан"})
				return written, reused, &jobError{Code: "canceled", Message: "задача отменена оператором"}
			}
			return written, reused, &jobError{Code: "input_missing", Message: fmt.Sprintf(
				"копирование %s → %s: %v", f.Rel, dst, err)}
		}
		done += n
		written += n
		sink(runEvent{Type: "log", Line: fmt.Sprintf("скопирован %s (%d Б)", f.Rel, n)})
	}
	sink(runEvent{Type: "progress", Progress: bundleCopyProgress(total, total)})
	return written, reused, nil
}

// copyBundleFile копирует файл порциями, сообщая о прогрессе.
//
// ПОЧЕМУ СВОЁ КОПИРОВАНИЕ, А НЕ os.Link: файлы источника и bundle могут лежать
// на разных дисках (и в разных контейнерах), поэтому жёсткая ссылка не гарантия.
// Заодно так виден прогресс — стадия writing обязана отражать запись файлов
// (требование контракта, §5 плана).
func copyBundleFile(ctx context.Context, src, dst string, progress func(int64)) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	// Результат пишется во временный файл и переименовывается: половина файла под
	// именем модели выглядела бы рабочей и падала на загрузке (тот же приём, что
	// у состояния задач).
	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	buf := make([]byte, bundleCopyChunk)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			out.Close()
			_ = os.Remove(tmp)
			return total, err
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				_ = os.Remove(tmp)
				return total, werr
			}
			total += int64(n)
			progress(total)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			_ = os.Remove(tmp)
			return total, rerr
		}
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return total, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return total, err
	}
	return total, nil
}

// writeBundleProfile — profile.json в каталог bundle (атомарно: tmp + rename).
//
// LocalPath НЕ ПИШЕТСЯ: путь вида /app/models/... верен только внутри контейнера,
// а профиль переживает переносимый bundle (тот же каталог копируют на другой
// воркер). Реестр и загрузчик сами разворачивают Filename относительно каталога
// модели (internal/sdbackend/models.go:loadProfile) — так профиль остаётся
// переносимым.
func writeBundleProfile(dir string, profile *types.ImageModelProfile) (string, error) {
	clean := *profile
	clean.Files = make([]types.ImageModelFile, len(profile.Files))
	for i, f := range profile.Files {
		f.LocalPath = ""
		clean.Files[i] = f
	}
	data, err := json.MarshalIndent(clean, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal %s: %w", bundleProfileName, err)
	}
	path := filepath.Join(dir, bundleProfileName)
	if err := writeFileAtomic(path, append(data, '\n')); err != nil {
		return "", err
	}
	return path, nil
}

// writeBundleSidecar — sd-server.config.json: запись «что запускаем».
//
// Формат — формат супервизора воркера (supervisor.go:writeSidecar): model,
// family, profile_source, defaults, runtime, seed_mode. Дополнительное поле
// built_by аддитивно и не ломает чтение: супервизор перезапишет файл при
// загрузке модели, и это нормально — файл описывает состояние запуска, а наше
// поле объясняет происхождение записи до первого запуска.
func writeBundleSidecar(dir string, profile *types.ImageModelProfile) (string, error) {
	seedMode := profile.Runtime.SeedMode
	if seedMode == "" {
		seedMode = "random"
	}
	sidecar := struct {
		Model         string                 `json:"model"`
		Family        string                 `json:"family"`
		ProfileSource string                 `json:"profile_source"`
		Defaults      types.ImageGenDefaults `json:"defaults"`
		Runtime       types.ImageRuntime     `json:"runtime"`
		SeedMode      string                 `json:"seed_mode"`
		BuiltBy       map[string]interface{} `json:"built_by,omitempty"`
	}{
		Model: profile.Name, Family: profile.Family, ProfileSource: bundleProfileName,
		Defaults: profile.Defaults, Runtime: profile.Runtime, SeedMode: seedMode,
		BuiltBy: map[string]interface{}{
			"tool":    "gguf-reexport",
			"target":  targetImageBundle,
			"builtAt": time.Now().UTC().Format(time.RFC3339),
		},
	}
	data, err := json.MarshalIndent(sidecar, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal %s: %w", bundleSidecarName, err)
	}
	path := filepath.Join(dir, bundleSidecarName)
	if err := writeFileAtomic(path, append(data, '\n')); err != nil {
		return "", err
	}
	return path, nil
}

// writeFileAtomic — tmp + rename: читатель видит либо прошлую версию, либо новую.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ---------------- вердикт и проверка через воркер ----------------

// bundleWorkerResult — что сказал imageworker про собранный bundle.
type bundleWorkerResult struct {
	Found    bool
	Family   string
	Verdict  string
	Runnable bool
	Reason   string
	Missing  []string
}

// bundleWorker — то, что сервису нужно от imageworker: список моделей с
// вердиктами R696 И факт «реестр действительно перечитан».
//
// ПОЧЕМУ ВТОРОЙ РЕЗУЛЬТАТ (rescanned) — НЕ ФОРМАЛЬНОСТЬ. У воркера есть два пути:
//
//	POST /api/image/models/scan — принудительное перечитывание (Фаза 4, добавлено
//	  вместе с этой целью: без него запись profile.json в СУЩЕСТВУЮЩИЙ каталог
//	  оставалась невидимой, потому что GET перечитывает реестр только при
//	  изменении набора каталогов — sdbackend.RefreshIfChanged);
//	GET /api/image/models — старый путь, он же fallback: на воркере, собранном до
//	  Фазы 4, scan-эндпоинта нет (404), и перечитывание случается только если
//	  каталог действительно новый.
//
// Разница решает, можно ли ВЕРИТЬ вердикту: для нового каталога GET честно
// перечитывает, для записи «на месте» — нет. Поэтому воркер, который не умеет
// scan, не объявляется лжецом: его ответ помечается worker_rescan="get" и
// возможная устарелость называется в verify.
type bundleWorker interface {
	models(ctx context.Context) (map[string]bundleWorkerResult, bool, error)
	url() string
}

// httpBundleWorker — imageworker по HTTP (REEXPORT_IMAGEWORKER_URL).
type httpBundleWorker struct {
	baseURL string
	token   string
	client  *http.Client
}

func (w *httpBundleWorker) url() string { return w.baseURL }

// models — пересканировать каталог и получить модели с вердиктами compat.
//
// Сначала POST /api/image/models/scan (принудительное перечитывание); если
// воркер собран до Фазы 4 и эндпоинта нет (404/405) — GET, и результат помечается
// как «перечитывание не гарантировано». Так модуль работает со старым образом
// стенда и честно говорит, чего именно не хватает, вместо «вердикт не сошёлся».
func (w *httpBundleWorker) models(ctx context.Context) (map[string]bundleWorkerResult, bool, error) {
	rescanned := true
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.baseURL+"/api/image/models/scan", nil)
	if err != nil {
		return nil, false, err
	}
	if w.token != "" {
		req.Header.Set("X-API-Token", w.token)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		resp.Body.Close()
		// Старый воркер: принудительного скана нет — читаем список как раньше.
		rescanned = false
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, w.baseURL+"/api/image/models", nil)
		if err != nil {
			return nil, false, err
		}
		if w.token != "" {
			req.Header.Set("X-API-Token", w.token)
		}
		resp, err = w.client.Do(req)
		if err != nil {
			return nil, false, err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("HTTP %d от %s/api/image/models", resp.StatusCode, w.baseURL)
	}
	var reply struct {
		Models []struct {
			Name   string `json:"name"`
			Family string `json:"family"`
			Compat *struct {
				Verdict  string   `json:"verdict"`
				Runnable bool     `json:"runnable"`
				Reason   string   `json:"reason"`
				Missing  []string `json:"missing"`
			} `json:"compat"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&reply); err != nil {
		return nil, rescanned, fmt.Errorf("ответ imageworker не разобран: %w", err)
	}
	out := make(map[string]bundleWorkerResult, len(reply.Models))
	for _, m := range reply.Models {
		res := bundleWorkerResult{Found: true, Family: m.Family}
		if m.Compat != nil {
			res.Verdict, res.Runnable, res.Reason, res.Missing = m.Compat.Verdict, m.Compat.Runnable, m.Compat.Reason, m.Compat.Missing
		}
		out[m.Name] = res
	}
	return out, rescanned, nil
}

// checkWorker — спросить imageworker про собранный bundle.
//
// Ошибка воркера НЕ превращается в «проверено»: возвращается отдельно, и задача
// либо падает с worker_unavailable (воркер настроен), либо пишет
// worker_checked=false (не настроен) — молчаливой подмены нет.
func (b *bundleRunner) checkWorker(ctx context.Context, name string) (*bundleWorkerResult, bool, error) {
	if b.worker == nil {
		return nil, false, nil
	}
	all, rescanned, err := b.worker.models(ctx)
	if err != nil {
		return nil, rescanned, err
	}
	res := all[name]
	if !res.Found {
		return &bundleWorkerResult{Found: false}, rescanned, nil
	}
	return &res, rescanned, nil
}

// workerURL — адрес воркера для сообщений (nil-safe).
func workerURL(w bundleWorker) string {
	if w == nil {
		return ""
	}
	return w.url()
}

// ---------------- факты, изменения, verify ----------------

// bundleFacts — факты итога: числа и роли, а не пересказ лога.
func bundleFacts(plan *bundlePlan, compat *imagemodel.BundleCompat, worker *bundleWorkerResult, werr error, rescanned bool) map[string]interface{} {
	roles := make(map[string]string, len(plan.Files))
	for _, f := range plan.Files {
		roles[f.Rel] = f.Role
	}
	facts := map[string]interface{}{
		"bundle":       plan.Name,
		"source":       plan.SrcDir,
		"bundle_path":  plan.OutDir,
		"in_place":     plan.InPlace,
		"roles_source": plan.Source,
		"family":       plan.Profile.Family,
		"files":        len(plan.Files),
		"roles":        roles,
	}
	if compat != nil {
		facts["compat_verdict"] = compat.Verdict
		facts["compat_reason"] = compat.Reason
		facts["engine_mode"] = compat.EngineMode
		facts["ready"] = compat.Verdict == imagemodel.CompatReady
		if len(compat.Missing) > 0 {
			facts["missing"] = compat.Missing
		}
	}
	facts["worker_checked"] = worker != nil && worker.Found && werr == nil
	// worker_rescan — КАК воркер перечитал каталог: scan (принудительно) или get
	// (старый образ — перечитывание только при смене набора каталогов). Поле
	// нужно, чтобы «unchecked» и «проверено» не сливались: разные причины.
	facts["worker_rescan"] = rescanLabel(rescanned)
	if werr != nil {
		facts["worker_error"] = werr.Error()
	}
	if worker != nil && worker.Found {
		facts["worker_verdict"] = worker.Verdict
		facts["worker_family"] = worker.Family
	}
	return facts
}

// bundleVerify — поле verify: то же, что факты, но в форме «чем доказано».
func bundleVerify(compat *imagemodel.BundleCompat, worker *bundleWorkerResult, werr error, plan *bundlePlan, rescanned bool) map[string]interface{} {
	out := map[string]interface{}{"bundle": plan.Name, "in_place": plan.InPlace}
	if compat != nil {
		out["compat_verdict"] = compat.Verdict
		out["compat_reason"] = compat.Reason
		out["compat_runnable"] = compat.Runnable
		out["ready"] = compat.Verdict == imagemodel.CompatReady
		if len(compat.Missing) > 0 {
			out["missing"] = compat.Missing
		}
	}
	if plan.InPlace {
		// Манифест в существующем каталоге: воркер уже знает этот каталог, скан
		// каталога модель не «переоткрывает». Это не ошибка — это режим.
		out["note"] = "манифест записан в существующий каталог набора"
	}
	if werr != nil {
		out["worker_checked"] = false
		out["worker_error"] = werr.Error()
		return out
	}
	if worker == nil {
		out["worker_checked"] = false
		out["worker_error"] = "REEXPORT_IMAGEWORKER_URL не задан: проверка вердикта только локальная"
		return out
	}
	out["worker_checked"] = worker.Found
	// worker_rescan — чем перечитан каталог: scan (принудительно) или get (старый
	// образ). Без этого поля «проверено» и «проверено, но, возможно, по старому
	// профилю» выглядели бы одинаково.
	out["worker_rescan"] = rescanLabel(rescanned)
	if worker.Found {
		out["worker_verdict"] = worker.Verdict
		out["worker_family"] = worker.Family
		// Запись «на месте» + воркер без POST /scan: его вердикт мог считаться по
		// синтезированному профилю. Флаг вместо умолчания — оператор видит, чем
		// именно подтверждён результат.
		if plan.InPlace && !rescanned {
			out["worker_stale_possible"] = true
			out["worker_warning"] = "воркер не перечитывает запись «на месте»: обновите образ воркера (POST /api/image/models/scan)"
		}
	}
	return out
}

// rescanLabel — как перечитывался каталог воркера (для фактов и verify).
func rescanLabel(rescanned bool) string {
	if rescanned {
		return "scan"
	}
	return "get"
}

// bundleNotes — Notes профиля: происхождение и оговорки (их читает оператор).
func bundleNotes(plan *bundlePlan, family string) string {
	notes := []string{
		"bundle собран модулем ре-экспорта (цель " + targetImageBundle +
			"): роли определены по " + rolesSourceLabel(plan.Source) + ".",
	}
	if plan.InPlace {
		notes = append(notes, "profile.json записан в существующий каталог набора.")
	}
	if family == "other" {
		notes = append(notes, "Семейство по заголовку не определяется: если это DiT-модель "+
			"(FLUX/SD3/Qwen-Image), укажите семейство в профиле — иначе движок получит --model.")
	}
	return strings.Join(notes, " ")
}

// rolesSourceLabel — откуда взяты роли.
func rolesSourceLabel(source string) string {
	if source == "profile_json" {
		return "profile.json набора (выбор оператора) и заголовкам файлов"
	}
	return "заголовкам файлов (R696)"
}

// rolesList — «diffusion, vae, clip_l…» для отчёта.
func rolesList(files []bundleFile) string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Role)
	}
	return strings.Join(out, ", ")
}

// knownFiles — имена файлов набора для сообщения об ошибке.
func knownFiles(files []bundleFile) string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Rel)
	}
	if len(out) > 20 {
		out = append(out[:20], "…")
	}
	return strings.Join(out, ", ")
}

// planBytes — суммарный размер набора (в отчёте — «сколько на входе»).
func planBytes(plan *bundlePlan) int64 {
	var total int64
	for _, f := range plan.Files {
		total += f.Size
	}
	return total
}

// bundleCopyProgress — событие прогресса по скопированным байтам.
//
// Диапазон 10…90 %: 10 % уже отданы стадии чтения, 90–100 % — сверке. 100 %
// печатает только стадия validating: как и у python-драйвера, «готово» имеет
// право сказать тот, кто уже сверил результат.
func bundleCopyProgress(done, total int64) jobProgress {
	percent := 90.0
	if total > 0 {
		percent = 10 + 80*float64(done)/float64(total)
		if percent > 90 {
			percent = 90
		}
	}
	return jobProgress{Phase: "copy", Stage: "writing", Percent: percent, BytesDone: done, BytesTotal: total}
}

// compatLine — вердикт одной строкой для хвоста лога.
func compatLine(c *imagemodel.BundleCompat) string {
	if c == nil {
		return "нет вердикта"
	}
	line := c.Verdict + " — " + c.Reason
	if len(c.Missing) > 0 {
		line += " (не хватает: " + strings.Join(c.Missing, ", ") + ")"
	}
	return line
}

// orMissing — «чего не хватает» одной строкой.
func orMissing(missing []string) string {
	if len(missing) == 0 {
		return "обязательные роли не найдены"
	}
	return strings.Join(missing, ", ")
}

// engineModeLabel — режим движка по вердикту (для отчёта).
func engineModeLabel(c *imagemodel.BundleCompat) string {
	if c == nil {
		return "не определён"
	}
	if c.EngineMode == imagemodel.CompatEngineModel {
		return "--model"
	}
	if c.EngineMode == imagemodel.CompatEngineDiffusion {
		return "--diffusion-model"
	}
	return "не определён"
}
