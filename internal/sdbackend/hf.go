// hf.go — R-Image / Phase 4 (2026-09-28): HF-загрузка image-bundle'ов в
// sdworker.
//
// ЗАЧЕМ ЭТОТ ФАЙЛ: загрузчик HuggingFace уже реализован в
// internal/cppbackend (hf_downloader.go + hf_bundle.go) и он УНИВЕРСАЛЬНЫЙ —
// ему всё равно, что качать: GGUF для llama.cpp или набор safetensors/gguf
// диффузионной модели. Поэтому здесь нет второй реализации HTTP-клиента,
// Range-resume, HF_TOKEN, зеркала и прогресса: HFManager — это ОБЁРТКА,
// которая добавляет ровно то, чего нет в cppbackend:
//
//  1. каталоги воркера (модели = Config.ModelsDir, темп — свой подкаталог,
//     чтобы .download-файлы диффузии (6+ GB) не путались с темпом llama-моделей
//     и попадали в orphan-отчёт своего воркера);
//  2. ПРОГРЕСС ПО ФАЙЛУ BUNDLE: cppbackend хранит прогресс по bundleId, а UI
//     «Изображения» опрашивает GET /api/hf/progress?modelId=<repo>&filename=...
//     по каждому файлу (см. webui/js/modules/image-page.js:1610-1621) — здесь
//     этот поиск по активным bundle и истории;
//  3. РЕГИСТРАЦИЮ МОДЕЛИ после успешной загрузки: profile.json в каталоге
//     bundle + перезагрузка реестра. Без этого скачанная модель не появится в
//     GET /api/image/models до рестарта воркера — ровно тот баг «скачали, а
//     модели нет», который в cppworker чинили подпиской onDownloadComplete
//     (см. hf_downloader.go:189-197).
//
// ЧТО НЕ ДЕЛАЕТ: не качает файлы сам (только через cppbackend), не хранит
// собственный прогресс (источник истины — downloader), не трогает реестр
// моделей напрямую (только Registry.Load).
package sdbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Константы
// ============================================================

// HFBundleManifestName — манифест bundle (реэкспорт контракта cppbackend):
// его наличие = «все файлы скачаны». Профиль модели пишется ТОЛЬКО после него.
const HFBundleManifestName = cppbackend.BundleManifestName

// hfProfileNoteStamp — метка в profile.json: профиль сгенерирован загрузчиком,
// а не задан оператором вручную. Нужна, чтобы при разборе инцидентов было
// видно происхождение файла (и чтобы UI мог показать это в Notes).
const hfProfileNoteStamp = "profile.json сгенерирован sdworker после HF-bundle-загрузки"

// hfBundleStaleManifestGrace — сколько ждать появления манифеста после
// возврата StartBundleDownload, прежде чем считать регистрацию провалившейся.
//
// ЗАЧЕМ: манифест пишется ВНУТРИ StartBundleDownload (до её возврата), поэтому
// на практике файл уже на диске; но на медленном/сетевом FS его видимость не
// гарантирована мгновенно. Ждём коротко и проверяем факт, а не «доверяем
// возвращённому статусу» — иначе можем записать profile.json для bundle,
// который на самом деле не зарегистрирован.
const hfBundleStaleManifestGrace = 2 * time.Second

// hfBundleDirPrefix — префикс каталога, который воркер создаёт под bundle, но
// чью регистрацию снял сам (cleanup). См. dropRegistrationIfEmpty.
const hfBundleDirPrefix = ".unregistered-"

// hfBundleDirRetention — как долго держать каталог снятой с регистрации модели
// перед полным удалением.
//
// ПОЧЕМУ НЕ УДАЛЯЕМ СРАЗУ: если удалили файл по ошибке, оператор может вернуть
// его на место в течение часа (файлы модели — это гигабайты, повторный pull
// дорог). Диск при этом не «течёт»: каталог снимается с регистрации (реестр его
// не видит) и удаляется при следующем запуске воркера.
const hfBundleDirRetention = time.Hour

// hfUnregisteredDirName — имя каталога со снятой регистрацией.
func hfUnregisteredDirName(bundleName string) string {
	return hfBundleDirPrefix + bundleName
}

// ============================================================
// HFManager
// ============================================================

// HFManager — HF-загрузчик воркера: downloader cppbackend + регистрация моделей.
type HFManager struct {
	// downloader — загрузчик cppbackend. Один на воркер: он хранит активные
	// загрузки, историю и orphan-файлы (см. ListDownloads).
	downloader *cppbackend.HuggingFaceDownloader

	// modelsDir — каталог моделей воркера (== Registry.ModelsDir()).
	// Схема bundle: <modelsDir>/<bundleName>/<файлы> (types.ImageModelProfile.BundleDir).
	modelsDir string

	// downloadsDir — каталог темповых .download-файлов ОДИНОЧНЫХ загрузок.
	// Bundle'ы кладут темп внутрь своего каталога (так решено в cppbackend,
	// см. ListOrphanBundleFiles): их .download виден через orphans.
	downloadsDir string

	// mirror — HF_MIRROR ("" = huggingface.co). Сюда же направляются тесты
	// (httptest-сервер) — реальная сеть в тестах не нужна.
	mirror string

	// registry — реестр моделей: после успешной регистрации перезагружаем его,
	// чтобы модель сразу появилась в GET /api/image/models.
	registry *Registry

	// autoFit — значение runtime.autoFit для автогенерируемых профилей
	// (on|off|""). Для слабых GPU включаем: движок сам ужмёт плейсмент под
	// доступную VRAM (см. Config.HFBundleAutoFit).
	autoFit string

	// registerMu сериализует РЕГИСТРАЦИЮ (profile.json + Registry.Load).
	// Две параллельные bundle-загрузки иначе писали бы профили и перезагружали
	// реестр наперегонки: Load сканирует каталог целиком, поэтому один вызов
	// может «не увидеть» профиль, записанный вторым (тот ещё не долетел до
	// диска) — и модель пропала бы из списка до следующего reload.
	registerMu sync.Mutex

	// baseCtx/baseCancel — контекст фоновых bundle-загрузок. Живёт дольше
	// HTTP-запроса: клиент закрывает вкладку, а pull 9 GB обязан доиграть
	// (иначе останутся .download-файлы и незарегистрированный bundle).
	baseCtx    context.Context
	baseCancel context.CancelFunc

	// ============================================================
	// Собственный трекер прогресса bundle
	// ============================================================
	//
	// ПОЧЕМУ НЕ GetBundleProgress ЗАГРУЗЧИКА: cppbackend отдаёт «снимок»
	// HFBundleProgress, но поле Files — СРЕЗ, и снимок делит массив с активной
	// загрузкой, которая пишет в него под своим мьютексом
	// (downloadBundleFile, hf_bundle.go:325/342). Любое чтение Files снаружи —
	// это data race (ловится `go test -race`), в бою дающий порванные значения
	// прогресса. Правильное место для исправления — внутри cppbackend
	// (глубокая копия под мьютексом), но он в зоне «только читать», поэтому
	// воркер ведёт СВОЙ трекер: фактические размеры файлов он читает с диска
	// (единственный источник, который ему и нужен для UI: «скачано N из M»).
	statesMu sync.Mutex
	states   map[string]*hfBundleState
}

// hfFileState — состояние одного файла bundle для UI.
type hfFileState struct {
	role      string
	repo      string
	filename  string // basename в каталоге bundle
	local     string // полный путь итогового файла
	size      int64  // ожидаемый размер (0 = неизвестен)
	status    string
	errText   string
	tempPath  string
	resumable bool
}

// hfBundleState — состояние одной bundle-загрузки (собственный трекер воркера).
type hfBundleState struct {
	id        string
	family    string
	targetDir string
	status    string // downloading|completed|failed|cancelled
	startedAt string
	endedAt   string
	errText   string
	// finalSnapshot — снимок, возвращённый StartBundleDownload. Заполняется
	// ТОЛЬКО из горутины загрузки (после её завершения) — в этот момент запись
	// в него уже прекращена, поэтому читать безопасно.
	finalSnapshot *cppbackend.HFBundleProgress
	files         []*hfFileState
}

// NewHFManager — сборка HF-обёртки над cppbackend-загрузчиком.
//
// token — HF_TOKEN из конфига (заголовок X-HF-Token от UI имеет приоритет и
// подставляется per-request через SetToken).
// mirror — HF_MIRROR (или httptest-URL в тестах).
func NewHFManager(cfg *Config, registry *Registry) (*HFManager, error) {
	if cfg == nil {
		return nil, fmt.Errorf("nil config")
	}
	if registry == nil {
		return nil, fmt.Errorf("nil registry")
	}

	modelsDir, err := cfg.ModelsDirAbs()
	if err != nil {
		return nil, fmt.Errorf("resolve models dir: %w", err)
	}
	downloadsDir, err := cfg.DownloadsDirAbs()
	if err != nil {
		return nil, fmt.Errorf("resolve downloads dir: %w", err)
	}

	m := &HFManager{
		downloader:   cppbackend.NewHuggingFaceDownloader(cfg.HFToken, cfg.HFMirror, downloadsDir, modelsDir),
		modelsDir:    modelsDir,
		downloadsDir: downloadsDir,
		mirror:       cfg.HFMirror,
		registry:     registry,
		autoFit:      cfg.HFBundleAutoFit,
		states:       map[string]*hfBundleState{},
	}
	// Создаём оба каталога сразу: ListOrphanDownloads/ListOrphanBundleFiles на
	// отсутствующем каталоге возвращают пустой список (не ошибку), но
	// InitializeDownloadDir даёт раннюю диагностику прав на запись — до того,
	// как пользователь нажмёт «скачать» и получит непонятную ошибку в середине
	// 6-гигабайтной загрузки.
	if err := m.downloader.InitializeDownloadDir(); err != nil {
		return nil, err
	}
	m.baseCtx, m.baseCancel = context.WithCancel(context.Background())
	return m, nil
}

// SearchModels — поиск репозиториев (task = фильтр HF pipeline_tag, "" = как в
// cppworker: только GGUF-репозитории).
func (m *HFManager) SearchModels(ctx context.Context, query string, limit int, task string) ([]cppbackend.HFModelRepo, error) {
	return m.downloader.SearchModelsTask(ctx, query, limit, task)
}

// ListFiles — файлы весов репозитория (ModelWeightExtensions).
func (m *HFManager) ListFiles(ctx context.Context, modelID, revision string) ([]cppbackend.HFFileInfo, error) {
	return m.downloader.ListModelFilesByFormat(ctx, modelID, revision, cppbackend.ModelWeightExtensions)
}

// ModelsDir — каталог моделей (абсолютный).
func (m *HFManager) ModelsDir() string { return m.modelsDir }

// PlanRepo — «паспорт репозитория»: комплектность набора + режим движка + шаги
// (см. cppbackend/hf_plan.go). Один Range-запрос на главный кандидат.
func (m *HFManager) PlanRepo(ctx context.Context, modelID, revision string) (*cppbackend.HFRepoPlan, error) {
	return m.downloader.PlanRepo(ctx, modelID, revision, nil)
}

// ProbeFile — пред-проверка файла модели до скачивания (см. cppbackend/hf_probe.go):
// читает заголовок Range-запросом и говорит, прочитает ли файл движок.
func (m *HFManager) ProbeFile(ctx context.Context, modelID, filename, revision string) (*cppbackend.HFProbeResult, error) {
	return m.downloader.ProbeFile(ctx, modelID, filename, revision)
}

// DownloadsDir — каталог темповых .download-файлов одиночных загрузок.
func (m *HFManager) DownloadsDir() string { return m.downloadsDir }

// SetToken — HF-токен из запроса (X-HF-Token / Authorization). Пустое значение
// НЕ затирает уже установленный токен из конфига: клиент без токена не должен
// «разлогинивать» воркер для параллельного запроса с токеном.
func (m *HFManager) SetToken(token string) {
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	m.downloader.SetToken(token)
}

// Close — остановка фоновых загрузок (вызывается из Service.Shutdown).
func (m *HFManager) Close() {
	if m.baseCancel != nil {
		m.baseCancel()
	}
	if m.downloader != nil {
		m.downloader.Close()
	}
}

// ============================================================
// Запуск загрузок
// ============================================================

// StartFileDownload — одиночная загрузка файла в каталог моделей воркера.
//
// АСИНХРОННО и идемпотентно (как в cppworker): если такая загрузка уже идёт,
// возвращается её текущий прогресс, а не ошибка — UI может спокойно повторять
// запрос при двойном клике.
func (m *HFManager) StartFileDownload(req cppbackend.HFDownloadRequest) (*cppbackend.HFDownloadProgress, error) {
	if err := validateHFDownloadRequest(req); err != nil {
		return nil, err
	}
	if req.Revision == "" {
		req.Revision = "main"
	}
	return m.downloader.StartDownload(req)
}

// StartBundle — канонический вход для POST /api/hf/bundle.
//
// ВАЖНО ПРО НЕБЛОКИРУЮЩИЙ ОТВЕТ: cppbackend.StartBundleDownload СИНХРОННАЯ
// (возвращается, когда скачаны/провалены все файлы — так задумано, см.
// hf_bundle.go:22-26). HTTP-хендлеру нужен 202 немедленно (загрузка 6-12 GB
// идёт десятки минут, а таймаут контроля у UI — 15 с), поэтому запускаем её в
// ГОРУТИНЕ и сразу возвращаемся. Прогресс — GetBundleProgress/GetFileProgress.
//
// Валидация делается ДО запуска горутины (fail fast, синхронный 400):
// невалидное имя/роль/состав не должны становиться «фоновым провалом»,
// о котором клиент узнаёт только по прогрессу.
func (m *HFManager) StartBundle(name, family string, files []cppbackend.HFDownloadRequest) error {
	name = strings.TrimSpace(name)
	if err := validateHFBundleID(name); err != nil {
		return err
	}
	family = strings.TrimSpace(family)
	if family == "" {
		family = "other"
	}
	if !types.IsValidImageFamily(family) {
		return fmt.Errorf("unknown family %q (allowed: %s)", family, strings.Join(types.ImageModelFamilies, ", "))
	}
	if len(files) == 0 {
		return fmt.Errorf("files is required: a diffusion bundle must contain at least one file")
	}
	seenRoles := make(map[string]bool, len(files))
	for i := range files {
		if err := validateHFDownloadRequest(files[i]); err != nil {
			return fmt.Errorf("files[%d]: %w", i, err)
		}
		if files[i].Revision == "" {
			files[i].Revision = "main"
		}
		role := files[i].Role
		if seenRoles[role] && role != types.ImageFileRoleLora {
			return fmt.Errorf("files[%d]: duplicate role %q", i, role)
		}
		seenRoles[role] = true
	}
	if !seenRoles[types.ImageFileRoleDiffusion] {
		return fmt.Errorf("bundle must contain a %q file", types.ImageFileRoleDiffusion)
	}

	// Заводим состояние СРАЗУ (до горутины): UI начинает опрашивать прогресс
	// через ~2 с после 202 и должен увидеть «downloading», а не 404.
	state := m.beginBundleState(name, family, filepath.Join(m.modelsDir, name), files)

	// Фон: ctx от baseCtx — отмена при shutdown воркера, но НЕ при закрытии
	// HTTP-соединения клиентом.
	go m.runBundle(m.baseCtx, state, files)
	return nil
}

// beginBundleState — регистрирует состояние bundle в собственном трекере.
//
// Повторный pull того же имени НЕ пересоздаёт состояние: cppbackend держит один
// активный bundle на bundleID и вернёт прогресс существующего (hf_bundle.go:141),
// поэтому перезапись стёрла бы наблюдаемые UI данные.
func (m *HFManager) beginBundleState(name, family, targetDir string, files []cppbackend.HFDownloadRequest) *hfBundleState {
	m.statesMu.Lock()
	defer m.statesMu.Unlock()
	m.pruneStatesLocked()
	if existing, ok := m.states[name]; ok && existing.status == "downloading" {
		return existing
	}

	state := &hfBundleState{
		id:        name,
		family:    family,
		targetDir: targetDir,
		status:    "downloading",
		startedAt: time.Now().UTC().Format(time.RFC3339),
		files:     make([]*hfFileState, 0, len(files)),
	}
	for _, f := range files {
		base := filepath.Base(f.Filename)
		state.files = append(state.files, &hfFileState{
			role:     f.Role,
			repo:     f.ModelID,
			filename: base,
			local:    filepath.Join(targetDir, base),
			status:   cppbackend.BundleFileStatusPending,
			tempPath: filepath.Join(targetDir, base+".download"),
		})
	}
	m.states[name] = state
	return state
}

// pruneStatesLocked — держим не больше hfMaxTrackedBundles состояний (старые
// завершённые вытесняются: история нужна только для отображения, а память
// трекера не должна расти бесконечно в долгоживущем воркере).
func (m *HFManager) pruneStatesLocked() {
	const hfMaxTrackedBundles = 50
	if len(m.states) < hfMaxTrackedBundles {
		return
	}
	var oldest string
	var oldestAt string
	for id, st := range m.states {
		if st.status == "downloading" {
			continue
		}
		if oldest == "" || st.startedAt < oldestAt {
			oldest, oldestAt = id, st.startedAt
		}
	}
	if oldest != "" {
		delete(m.states, oldest)
	}
}

// runBundle — фоновая bundle-загрузка + АТОМАРНАЯ регистрация модели.
//
// ПОРЯДОК КРИТИЧЕН: profile.json пишется ТОЛЬКО после того, как
// StartBundleDownload вернула nil (все файлы на месте) И манифест bundle есть
// на диске. Иначе неполный bundle выглядел бы рабочей моделью: профиль есть,
// файлов нет → sd-server падает на старте, а оператор видит «модель доступна»
// (ровно тот сценарий, от которого защищается cppbackend-манифест).
func (m *HFManager) runBundle(ctx context.Context, state *hfBundleState, files []cppbackend.HFDownloadRequest) {
	log := sdLog()
	name, family, targetDir := state.id, state.family, state.targetDir

	log.Infow("hf bundle download started",
		"bundle", name, "family", family, "files", len(files), "targetDir", targetDir)

	// Ожидаемые размеры — ДО старта: после завершения файлы уже на диске, а
	// прогресс «N из M» нужен именно во время загрузки. Ошибку сети игнорируем:
	// UI деградирует до «скачано без знаменателя» (как ведёт себя и cppbackend,
	// когда HF не отдаёт Content-Length).
	m.fillExpectedSizes(ctx, state, files)

	progress, err := m.downloader.StartBundleDownload(ctx, files, name, targetDir)

	// Фиксируем снимок загрузчика в своём состоянии: с этого момента запись в
	// него прекращена (StartBundleDownload синхронна), значит читать безопасно
	// даже с учётом того, что Files — общий срез (см. комментарий у states).
	m.finishBundleState(state, progress, err)

	if err != nil {
		// Причина уже в трекере (Failed-статусы файлов) — регистрация НЕ
		// выполняется, каталог остаётся без profile.json и без манифеста.
		log.Errorw("hf bundle download failed, model NOT registered",
			"bundle", name, "error", err)
		return
	}
	if !cppbackend.IsBundleRegistered(targetDir) && !m.waitForManifest(targetDir) {
		log.Errorw("hf bundle manifest missing on disk, model NOT registered", "bundle", name)
		return
	}

	m.registerMu.Lock()
	defer m.registerMu.Unlock()

	profile, err := m.writeProfile(name, family, targetDir, files)
	if err != nil {
		// Загрузка успешна, но модель не зарегистрирована: файлы лежат в
		// каталоге bundle, оператор увидит ошибку в логе. Повторный pull
		// (файлы уже на диске → пропускаются) повторит регистрацию.
		log.Errorw("hf bundle registration failed (files are on disk, profile.json is not)",
			"bundle", name, "error", err)
		return
	}
	// Перезагрузка реестра: без неё модель не появится в GET /api/image/models
	// до рестарта воркера (реестр строится один раз в NewService).
	if err := m.registry.Load(); err != nil {
		log.Errorw("hf bundle registered but registry reload failed",
			"bundle", name, "error", err)
		return
	}
	log.Infow("hf bundle registered as image model",
		"bundle", name, "family", profile.Family, "files", len(profile.Files),
		"totalBytes", totalBytesOfProfile(profile))
}

// fillExpectedSizes — проставляет ожидаемые размеры файлов (для прогресса).
//
// Источники по приоритету:
//  1. размер файла на диске — если bundle уже качали, он и будет «готовым»
//     размером (перекачивать нечего);
//  2. tree API HF — авторитетный размер для новой загрузки.
func (m *HFManager) fillExpectedSizes(ctx context.Context, state *hfBundleState, files []cppbackend.HFDownloadRequest) {
	for i, f := range files {
		if i >= len(state.files) {
			break
		}
		fs := state.files[i]
		if fi, err := os.Stat(fs.local); err == nil && !fi.IsDir() {
			m.setStateFileSize(state.id, i, fi.Size())
			continue
		}
		listCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		infos, err := m.downloader.ListModelFilesByFormat(listCtx, f.ModelID, f.Revision, cppbackend.ModelWeightExtensions)
		cancel()
		if err != nil {
			continue
		}
		for _, info := range infos {
			if info.Path == f.Filename || filepath.Base(info.Path) == fs.filename {
				m.setStateFileSize(state.id, i, info.SizeBytes)
				break
			}
		}
	}
}

// finishBundleState — терминальное состояние bundle в трекере.
func (m *HFManager) finishBundleState(state *hfBundleState, progress *cppbackend.HFBundleProgress, err error) {
	m.statesMu.Lock()
	defer m.statesMu.Unlock()

	status := "completed"
	errText := ""
	if err != nil {
		status = "failed"
		errText = err.Error()
		if state.status == "cancelled" {
			status = "cancelled"
		}
	} else if progress != nil && progress.Status != "" {
		// cppbackend различает cancelled/interrupted — уважаем его статус.
		switch progress.Status {
		case "cancelled", "interrupted":
			status = progress.Status
		}
	}

	for _, fs := range state.files {
		if fs.status == cppbackend.BundleFileStatusPending {
			// Файл не начинали (предыдущий провалился/отмена): UI должен видеть
			// терминальный статус, иначе таблица навсегда останется «в процессе».
			if status == "completed" {
				fs.status = cppbackend.BundleFileStatusCompleted
			} else {
				fs.status = status
				fs.errText = errText
			}
		}
	}
	state.status = status
	state.errText = errText
	state.endedAt = time.Now().UTC().Format(time.RFC3339)
	state.finalSnapshot = progress
}

// waitForManifest — короткое ожидание появления манифеста (см.
// hfBundleStaleManifestGrace). Возвращает true, если манифест найден.
func (m *HFManager) waitForManifest(targetDir string) bool {
	deadline := time.Now().Add(hfBundleStaleManifestGrace)
	for {
		if cppbackend.IsBundleRegistered(targetDir) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ============================================================
// Регистрация: profile.json
// ============================================================

// writeProfile пишет profile.json в каталог bundle'а и возвращает профиль.
//
// ПОЧЕМУ АТОМАРНО (tmp + rename): падение процесса в момент записи оставило бы
// обрезанный JSON — Registry.loadProfile вернул бы ошибку парсинга, и модель
// попала бы в реестр в состоянии error (см. models.go:118-126) вместо того,
// чтобы просто отсутствовать.
func (m *HFManager) writeProfile(name, family, targetDir string, files []cppbackend.HFDownloadRequest) (*types.ImageModelProfile, error) {
	profile := BuildImageModelProfile(name, family, targetDir, files, m.autoFit)
	if err := types.ValidateImageModelProfile(profile); err != nil {
		return nil, fmt.Errorf("validate generated profile: %w", err)
	}
	data, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal profile: %w", err)
	}
	tmp := filepath.Join(targetDir, ProfileFileName+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return nil, fmt.Errorf("write profile temp: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(targetDir, ProfileFileName)); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("rename profile: %w", err)
	}
	return profile, nil
}

// BuildImageModelProfile — профиль image-модели по составу bundle.
//
// LocalPath заполняется из фактических файлов на диске (а не из ожидаемых
// имён): если загрузчик переименовал/санитизировал basename, ServerArgs должен
// получить существующий путь. SizeBytes берётся фактический — профиль остаётся
// корректным и после ручной замены файла оператором.
//
// Публичная функция (а не метод) — чтобы её можно было тестировать отдельно от
// сети/диска и переиспользовать при синке профилей с балансера.
func BuildImageModelProfile(name, family, targetDir string, files []cppbackend.HFDownloadRequest, autoFit string) *types.ImageModelProfile {
	profile := &types.ImageModelProfile{
		Name:     name,
		Family:   family,
		Files:    make([]types.ImageModelFile, 0, len(files)),
		Defaults: types.DefaultImageGenDefaults(family),
		Runtime: types.ImageRuntime{
			// Разумные дефолты для СЛАБЫХ GPU (GTX 1060 / RTX 3060 4-6 GB):
			//   - MaxVRAM "-1" = не грузить всё в VRAM, распределять по доступной;
			//   - VaeTiling: пиковый потребитель памяти — decode VAE, тайлинг
			//     дешевле, чем --vae-on-cpu (~5x штраф, исследование §5.6);
			//   - SeedMode random: OpenAI-путь sd-server не читает seed из
			//     запроса и берёт 42 → без этого ВСЕ картинки одинаковые
			//     (ловушка №1, план §12.3).
			MaxVRAM:   "-1",
			VaeTiling: true,
			SeedMode:  "random",
		},
		Notes: hfProfileNoteStamp + "; runtime-дефолты для слабых GPU: " +
			"maxVram=-1, vaeTiling=true, seedMode=random. " +
			"Плейсмент под конкретное железо задаёт оператор (backend/paramsBackend/offloadToCpu).",
	}
	if autoFit != "" {
		profile.Runtime.AutoFit = autoFit
	}

	for _, f := range files {
		base := filepath.Base(f.Filename)
		item := types.ImageModelFile{
			Role:      f.Role,
			Repo:      f.ModelID,
			Filename:  base,
			Revision:  f.Revision,
			SizeBytes: f.SizeBytes,
		}
		local := filepath.Join(targetDir, base)
		if fi, err := os.Stat(local); err == nil && !fi.IsDir() {
			item.LocalPath = local
			item.SizeBytes = fi.Size()
		}
		profile.Files = append(profile.Files, item)
	}
	sort.SliceStable(profile.Files, func(i, j int) bool { return profile.Files[i].Role < profile.Files[j].Role })
	return profile
}

// ============================================================
// Прогресс
// ============================================================

// FileProgress — прогресс ОДНОГО файла bundle в форме HFDownloadProgress.
//
// ПОЧЕМУ СВОЙ ТРЕКЕР, А НЕ GetBundleProgress ЗАГРУЗЧИКА: (1) UI опрашивает
// прогресс по паре (modelId, filename) — так удобно рендерить строки таблицы
// (image-page.js:1617-1621), а карта загрузчика ключуется bundleID; (2) снимки
// cppbackend делят срез Files с активной загрузкой, то есть их чтение — data
// race (см. комментарий у states в структуре HFManager). Собственный трекер
// берёт размеры с ДИСКА: это ровно те байты, которые нужны UI.
//
// Возвращаем (nil,false,nil), если записи ещё нет: UI трактует это как
// «unknown» (не ошибка) — 404 заставил бы его считать, что эндпоинт недоступен,
// и прекратить опрос.
func (m *HFManager) FileProgress(modelID, filename string) (*cppbackend.HFDownloadProgress, bool, error) {
	if strings.TrimSpace(modelID) == "" {
		return nil, false, fmt.Errorf("modelId is required")
	}
	// Одиночная загрузка — авторитетнее (её ведёт StartDownload).
	if p, err := m.downloader.GetDownloadProgress(modelID, filename); err == nil && p != nil {
		cp := *p
		return &cp, true, nil
	}

	base := filename
	if base != "" {
		base = filepath.Base(base)
	}

	m.statesMu.Lock()
	var found *hfFileState
	var owner *hfBundleState
	for _, st := range m.states {
		for _, fs := range st.files {
			if fs.repo != modelID {
				continue
			}
			if base != "" && fs.filename != base {
				continue
			}
			found, owner = fs, st
			break
		}
		if found != nil {
			break
		}
	}
	if found == nil {
		m.statesMu.Unlock()
		return nil, false, nil
	}
	snap := *found   // копия: читаем без лока
	bundle := *owner // копия среза Files не нужна — читаем только скаляры
	m.statesMu.Unlock()

	downloaded := fileBytesOnDisk(snap.local, snap.tempPath)
	total := snap.size
	status := snap.status
	errText := snap.errText
	if status == cppbackend.BundleFileStatusPending {
		// UI умеет только «идёт/готово/провал»: pending показываем как downloading,
		// иначе aggregateDownloadProgress в UI не зачтёт файл ни в active, ни в
		// completed (webui/js/modules/image-page.js:417-437). Если bundle уже
		// завершился, а файла не тронули (предыдущий провалился) — отдаём
		// терминальный статус bundle, чтобы опрос в UI прекратился.
		if bundle.status == "downloading" {
			status = cppbackend.BundleFileStatusDownloading
		} else if bundle.status != "" {
			status = bundle.status
			errText = firstNonEmpty(errText, bundle.errText)
		}
	}
	if status == cppbackend.BundleFileStatusCompleted {
		if fi, err := os.Stat(snap.local); err == nil && !fi.IsDir() {
			total = fi.Size()
			downloaded = fi.Size()
		}
	}

	pct := 0.0
	if total > 0 {
		pct = float64(downloaded) / float64(total) * 100
		if pct > 100 {
			pct = 100
		}
	}
	if status == cppbackend.BundleFileStatusCompleted {
		pct = 100
	}

	return &cppbackend.HFDownloadProgress{
		ModelID:      snap.repo,
		Filename:     snap.filename,
		TotalBytes:   total,
		Downloaded:   downloaded,
		ProgressPct:  pct,
		SpeedBps:     speedFromSnapshot(bundle.finalSnapshot, snap.filename),
		Status:       status,
		ErrorMessage: errText,
		StartedAt:    bundle.startedAt,
		CompletedAt:  bundle.endedAt,
		TempPath:     snap.tempPath,
		FinalPath:    snap.local,
		Resumable:    fileExists(snap.tempPath),
	}, true, nil
}

// BundleProgress — агрегат bundle (для ?bundleId=<name>) из своего трекера.
//
// ПОЧЕМУ НЕ downloader.GetBundleProgress: см. комментарий у FileProgress (общий
// срез Files = data race). Скорость берём из финального снимка загрузчика —
// читать его безопасно, потому что загрузка уже завершена (см. finishBundleState).
// HasActiveDownloads — идёт ли сейчас загрузка (bundle или одиночный файл).
//
// ЗАЧЕМ (живой дефект 2026-10-06): пока качается bundle, каталог модели уже
// существует, а profile.json ещё не записан. Скан реестра в этот момент
// синтезирует «пустую» модель (family=other, files=[]), и она показывается
// оператору как готовая — а после завершения загрузки снимок уже не обновлялся
// (отпечаток тот же). Поэтому реестр перечитываем только когда загрузок нет.
func (m *HFManager) HasActiveDownloads() bool {
	if m == nil {
		return false
	}
	m.statesMu.Lock()
	defer m.statesMu.Unlock()
	for _, st := range m.states {
		if st == nil {
			continue
		}
		switch st.status {
		case "downloading", "queued", "starting":
			return true
		}
	}
	return false
}

// BundleProgress — прогресс bundle-загрузки по её id.
func (m *HFManager) BundleProgress(bundleID string) (*cppbackend.HFBundleProgress, error) {
	bundleID = strings.TrimSpace(bundleID)
	if bundleID == "" {
		return nil, fmt.Errorf("bundleId is required")
	}
	m.statesMu.Lock()
	st, ok := m.states[bundleID]
	if !ok {
		m.statesMu.Unlock()
		return nil, fmt.Errorf("no bundle download found for %s", bundleID)
	}
	cp := *st
	files := make([]*hfFileState, len(st.files))
	copy(files, st.files)
	cp.files = files
	m.statesMu.Unlock()

	out := &cppbackend.HFBundleProgress{
		BundleID:     cp.id,
		Status:       cp.status,
		TargetDir:    cp.targetDir,
		StartedAt:    cp.startedAt,
		CompletedAt:  cp.endedAt,
		ErrorMessage: cp.errText,
		Registered:   cppbackend.IsBundleRegistered(cp.targetDir),
		Files:        make([]cppbackend.HFBundleFileProgress, 0, len(cp.files)),
	}
	for _, fs := range cp.files {
		downloaded := fileBytesOnDisk(fs.local, fs.tempPath)
		total := fs.size
		status := fs.status
		if status == cppbackend.BundleFileStatusCompleted {
			if fi, err := os.Stat(fs.local); err == nil && !fi.IsDir() {
				total, downloaded = fi.Size(), fi.Size()
			}
		}
		out.Files = append(out.Files, cppbackend.HFBundleFileProgress{
			Role:       fs.role,
			ModelID:    fs.repo,
			Filename:   fs.filename,
			SourcePath: fs.filename,
			FinalPath:  fs.local,
			SizeBytes:  total,
			Downloaded: downloaded,
			Resumable:  fileExists(fs.tempPath),
			Status:     status,
			Error:      fs.errText,
		})
		out.TotalBytes += total
		out.Downloaded += downloaded
	}
	if out.TotalBytes > 0 {
		out.ProgressPct = float64(out.Downloaded) / float64(out.TotalBytes) * 100
		if out.ProgressPct > 100 {
			out.ProgressPct = 100
		}
	}
	if cp.status == "completed" {
		out.ProgressPct = 100
	}
	out.SpeedBps = bundleSpeed(cp.finalSnapshot)
	if cp.finalSnapshot != nil {
		out.CurrentFile = cp.finalSnapshot.CurrentFile
	}
	return out, nil
}

// setStateFileSize — ожидаемый размер файла в трекере (и в DownloadRequest,
// чтобы cppbackend не перекачивал уже готовый файл).
func (m *HFManager) setStateFileSize(bundleID string, idx int, size int64) {
	m.statesMu.Lock()
	defer m.statesMu.Unlock()
	st, ok := m.states[bundleID]
	if !ok || idx < 0 || idx >= len(st.files) {
		return
	}
	if size > 0 {
		st.files[idx].size = size
	}
}

// fileBytesOnDisk — сколько байт файла уже на диске: готовый файл, иначе
// .download-темп (тот же путь, что использует загрузчик).
func fileBytesOnDisk(finalPath, tempPath string) int64 {
	if fi, err := os.Stat(finalPath); err == nil && !fi.IsDir() {
		return fi.Size()
	}
	if fi, err := os.Stat(tempPath); err == nil && !fi.IsDir() {
		return fi.Size()
	}
	return 0
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// speedFromSnapshot — скорость текущего файла из финального снимка загрузчика
// (пусто, если снимка нет: скорость не критична для UI).
func speedFromSnapshot(p *cppbackend.HFBundleProgress, filename string) int64 {
	if p == nil {
		return 0
	}
	if p.CurrentFile != "" && p.CurrentFile != filename {
		return 0
	}
	return p.SpeedBps
}

func bundleSpeed(p *cppbackend.HFBundleProgress) int64 {
	if p == nil {
		return 0
	}
	return p.SpeedBps
}

// DownloadsSnapshot — срез состояния загрузок для GET /api/hf/downloads.
type DownloadsSnapshot struct {
	Active  []cppbackend.HFDownloadProgress `json:"active"`
	History []cppbackend.HFDownloadProgress `json:"history"`
	Orphans []cppbackend.OrphanDownloadFile `json:"orphans"`
	// Bundles/BundleHistory — bundle-загрузки из СОБСТВЕННОГО трекера воркера
	// (в Active одиночных загрузок их нет: это другая карта в cppbackend —
	// и её снимки нельзя читать без гонки, см. комментарий у states).
	Bundles       []cppbackend.HFBundleProgress `json:"bundles"`
	BundleHistory []cppbackend.HFBundleProgress `json:"bundleHistory"`
}

// ListDownloads — активные + история + orphans (+ bundle-загрузки).
//
// Orphans = одиночные .download в downloadsDir ПЛЮС .download внутри каталогов
// bundle: без второго списка прерванный pull диффузии (6+ GB) невидим в UI и
// занимает место до ручной чистки (см. cppbackend.ListOrphanBundleFiles).
func (m *HFManager) ListDownloads() DownloadsSnapshot {
	snap := DownloadsSnapshot{
		Active:  m.downloader.ListActiveDownloads(),
		History: m.downloader.ListDownloadHistory(),
		Orphans: m.downloader.ListOrphanDownloads(),
		// Пустые срезы, а не nil: JSON-ответ не должен содержать null —
		// UI итерирует эти массивы без проверок.
		Bundles:       []cppbackend.HFBundleProgress{},
		BundleHistory: []cppbackend.HFBundleProgress{},
	}
	for _, id := range m.trackedBundleIDs() {
		p, err := m.BundleProgress(id)
		if err != nil {
			continue
		}
		if p.Status == "downloading" {
			snap.Bundles = append(snap.Bundles, *p)
			continue
		}
		snap.BundleHistory = append(snap.BundleHistory, *p)
	}
	snap.Orphans = append(snap.Orphans, m.downloader.ListOrphanBundleFiles()...)
	sort.Slice(snap.Orphans, func(i, j int) bool { return snap.Orphans[i].Path < snap.Orphans[j].Path })
	return snap
}

// ============================================================
// Управление
// ============================================================

// trackedBundleIDs — идентификаторы bundle в трекере (стабильный порядок).
func (m *HFManager) trackedBundleIDs() []string {
	m.statesMu.Lock()
	defer m.statesMu.Unlock()
	ids := make([]string, 0, len(m.states))
	for id := range m.states {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Cancel — отмена загрузки: bundle (bundleID != "") или одиночного файла.
//
// ВНИМАНИЕ: обе функции cppbackend блокируются до фактической остановки
// загрузки (CancelBundleDownload ждёт task.completed). Для HTTP-хендлера это
// приемлемо: отмена — редкое управляющее действие, а ждать приходится до
// закрытия текущего соединения (доли секунды), не до конца файла.
func (m *HFManager) Cancel(bundleID, modelID, filename string) error {
	if strings.TrimSpace(bundleID) != "" {
		return m.downloader.CancelBundleDownload(strings.TrimSpace(bundleID))
	}
	if strings.TrimSpace(modelID) == "" {
		return fmt.Errorf("modelId or bundleId is required")
	}
	return m.downloader.CancelDownload(modelID, filename)
}

// Delete — удаление скачанного/частичного файла (освобождение места).
//
// filename трактуется двояко (как в контракте cleanup у cppworker):
//   - "<bundle>/<file>" — файл внутри каталога bundle: удаляем и сам файл, и
//     его .download-темп; при удалении последнего файла bundle снимается с
//     регистрации (см. dropRegistrationIfEmpty);
//   - "<file>" — одиночная загрузка: делегируем в downloader.DeleteDownload.
//
// Перед удалением активная загрузка ЭТОГО bundle отменяется: на Windows
// открытый .download-файл нельзя удалить («being used by another process»), а
// пользователь, нажавший «удалить», ожидает освобождения места, а не ошибки.
func (m *HFManager) Delete(modelID, filename string) (*cppbackend.DeleteDownloadResult, error) {
	filename = strings.TrimSpace(filename)
	if filename == "" {
		return nil, fmt.Errorf("filename is required")
	}
	if strings.ContainsAny(filename, `/\`) {
		return m.deleteBundleFile(modelID, filename)
	}
	return m.downloader.DeleteDownload(modelID, filename)
}

// deleteBundleFile — удаление файла внутри каталога bundle (path-safe).
func (m *HFManager) deleteBundleFile(modelID, rel string) (*cppbackend.DeleteDownloadResult, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return nil, fmt.Errorf("invalid filename %q: must be relative inside a bundle dir", rel)
	}
	full := filepath.Join(m.modelsDir, clean)
	// Защита от выхода за modelsDir (path traversal в теле запроса).
	if relCheck, err := filepath.Rel(m.modelsDir, full); err != nil || strings.HasPrefix(relCheck, "..") {
		return nil, fmt.Errorf("invalid filename %q: escapes models dir", rel)
	}

	// Отменяем активную загрузку этого bundle (если есть) — иначе файл занят.
	m.cancelActiveBundleFor(filepath.Dir(full))

	result := &cppbackend.DeleteDownloadResult{
		ModelID:  modelID,
		Filename: filepath.ToSlash(clean),
	}
	for _, candidate := range []string{full, full + ".download"} {
		fi, err := os.Stat(candidate)
		if err != nil || fi.IsDir() {
			continue
		}
		if err := os.Remove(candidate); err != nil {
			return result, fmt.Errorf("remove %s: %w", candidate, err)
		}
		result.BytesFreed += fi.Size()
		if strings.HasSuffix(candidate, ".download") {
			result.TempDeleted = true
		} else {
			result.FinalDeleted = true
		}
	}

	// Если это был последний файл bundle с манифестом — снимаем регистрацию:
	// иначе реестр показывал бы модель, у которой на диске нет файлов.
	m.dropRegistrationIfEmpty(filepath.Dir(full))

	if !result.TempDeleted && !result.FinalDeleted {
		return result, fmt.Errorf("no file found for %s", rel)
	}
	return result, nil
}

// cancelActiveBundleFor — отмена активной bundle-загрузки, владеющей каталогом.
//
// Ждём завершения (CancelBundleDownload блокируется до остановки): к моменту
// возврата файлы закрыты, и их можно удалять. Отменяем ТОЛЬКО по точному
// совпадению каталога — чужой параллельный pull трогать нельзя.
func (m *HFManager) cancelActiveBundleFor(bundleDir string) {
	m.statesMu.Lock()
	var id string
	for _, st := range m.states {
		if st.status == "downloading" && filepath.Clean(st.targetDir) == filepath.Clean(bundleDir) {
			id = st.id
			break
		}
	}
	m.statesMu.Unlock()
	if id == "" {
		return
	}
	if err := m.downloader.CancelBundleDownload(id); err != nil {
		sdLog().Debugw("hf cleanup: bundle was not active in downloader", "bundle", id, "error", err)
	}
}

// dropRegistrationIfEmpty — снимает регистрацию bundle, если в каталоге не
// осталось файлов модели.
//
// ЗАЧЕМ: удаление файла «дырявит» bundle, а Registry.Load() для каталога без
// файлов СИНТЕЗИРУЕТ модель (loadProfile → SynthesizeProfile), и она попадает в
// /api/image/models как «сломанная». Пользователь видел бы модель, которую
// нельзя загрузить (sd-server упал бы на старте).
//
// ПОЧЕМУ КАТАЛОГ ПЕРЕИМЕНОВЫВАЕТСЯ, А НЕ УДАЛЯЕТСЯ: реестр видит ТОЛЬКО
// подкаталоги modelsDir, поэтому переименование в «.unregistered-<name>» уже
// снимает модель с регистрации, но оставляет файлы на месте — восстановление
// после ошибочного удаления не требует повторной загрузки гигабайтов. Каталог
// удаляется целиком при следующем старте (SweepUnregisteredBundles).
func (m *HFManager) dropRegistrationIfEmpty(bundleDir string) {
	entries, err := os.ReadDir(bundleDir)
	if err != nil {
		return
	}
	hasModelFile := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == ProfileFileName || name == HFBundleManifestName ||
			strings.HasSuffix(name, ".download") || strings.HasSuffix(name, ".json") {
			continue
		}
		if cppbackend.HasModelWeightExtension(name) {
			hasModelFile = true
			break
		}
	}
	if hasModelFile {
		return // файл модели ещё есть — bundle остаётся зарегистрированным
	}

	m.registerMu.Lock()
	defer m.registerMu.Unlock()

	bundleName := filepath.Base(bundleDir)
	srcManifest := filepath.Join(bundleDir, HFBundleManifestName)
	dstDir := filepath.Join(filepath.Dir(bundleDir), hfUnregisteredDirName(bundleName))

	// Сначала гасим МАНИФЕСТ в исходном каталоге: он превращает каталог в
	// «зарегистрированную модель» для cppbackend-контракта, даже если профиль
	// уже удалён.
	_ = os.Remove(srcManifest)
	_ = os.Remove(filepath.Join(bundleDir, ProfileFileName))

	if _, err := os.Stat(dstDir); err == nil {
		// Каталог со снятой регистрацией уже есть (повторный cleanup): чтобы не
		// потерять файлы, просто удаляем исходный пустой каталог.
		if err := os.RemoveAll(bundleDir); err != nil {
			sdLog().Warnw("hf bundle: failed to remove empty bundle dir", "dir", bundleDir, "error", err)
		}
	} else if err := os.Rename(bundleDir, dstDir); err != nil {
		// Переименование может не сработать (каталог занят на Windows): в этом
		// случае удаляем пустой каталог, иначе он останется в реестре.
		if rmErr := os.RemoveAll(bundleDir); rmErr != nil {
			sdLog().Warnw("hf bundle: failed to unregister dir", "dir", bundleDir, "error", rmErr)
		}
	}
	if err := m.registry.Load(); err != nil {
		sdLog().Warnw("hf bundle: registry reload after cleanup failed", "dir", bundleDir, "error", err)
	}
}

// SweepUnregisteredBundles — удаляет каталоги моделей, чья регистрация была
// снята больше hfBundleDirRetention назад (вызывается при старте воркера).
//
// Возвращает количество удалённых каталогов. Ошибки не фатальны: место на диске
// — не тот ресурс, ради которого стоит не поднять воркер.
func (m *HFManager) SweepUnregisteredBundles() int {
	entries, err := os.ReadDir(m.modelsDir)
	if err != nil {
		return 0
	}
	removed := 0
	deadline := time.Now().Add(-hfBundleDirRetention)
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), hfBundleDirPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(deadline) {
			continue
		}
		full := filepath.Join(m.modelsDir, e.Name())
		if err := os.RemoveAll(full); err != nil {
			sdLog().Warnw("hf bundle: sweep failed", "dir", full, "error", err)
			continue
		}
		sdLog().Infow("hf bundle: swept stale unregistered bundle dir", "dir", full)
		removed++
	}
	return removed
}

// ============================================================
// Валидация
// ============================================================

// validateHFDownloadRequest — проверки одного файла ДО обращения к сети.
//
// Filename обязан быть basename: загрузчик использует filepath.Base, но
// молчаливое «обрезание» пути скрыло бы ошибку контракта (UI шлёт имя файла
// внутри репозитория, включая подкаталоги — например "unet/diffusion.safetensors";
// такой файл должен попасть в каталог bundle под своим basename, а не создать
// подкаталог с неожиданным содержимым).
func validateHFDownloadRequest(req cppbackend.HFDownloadRequest) error {
	if strings.TrimSpace(req.ModelID) == "" {
		return fmt.Errorf("repo/modelId is required")
	}
	if strings.TrimSpace(req.Filename) == "" {
		return fmt.Errorf("filename is required")
	}
	if !cppbackend.HasModelWeightExtension(req.Filename) {
		return fmt.Errorf("unsupported file type %q (allowed: %s)",
			req.Filename, strings.Join(cppbackend.ModelWeightExtensions, ", "))
	}
	if req.Role != "" {
		if err := validateImageFileRole(req.Role); err != nil {
			return err
		}
	}
	return nil
}

// validateImageFileRole — роль из замороженного набора types.ImageFileRole*.
//
// Почему дублируем проверку (в types она не экспортирована): хендлер обязан
// вернуть 400 с понятным текстом, а не «неизвестная роль» где-то в недрах
// генерации profile.json.
func validateImageFileRole(role string) error {
	switch role {
	case types.ImageFileRoleDiffusion, types.ImageFileRoleVae, types.ImageFileRoleClipL,
		types.ImageFileRoleClipG, types.ImageFileRoleT5xxl, types.ImageFileRoleLLM,
		types.ImageFileRoleClipVision, types.ImageFileRoleTaesd, types.ImageFileRoleLora,
		types.ImageFileRoleUpscaler, types.ImageFileRoleControlNet, types.ImageFileRoleIPAdapter:
		return nil
	}
	return fmt.Errorf("unknown role %q (allowed: diffusion|vae|clip_l|clip_g|t5xxl|llm|clip_vision|taesd|lora|upscaler|controlnet|ip_adapter)", role)
}

// validateHFBundleID — имя bundle становится ИМЕНЕМ КАТАЛОГА в modelsDir.
// Повторяем проверки cppbackend.validateBundleID (она не экспортирована) плюс
// запрещаем Windows-недопустимые символы: `:` и `*` ломают создание каталога, а
// виноватым выглядел бы загрузчик.
func validateHFBundleID(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("name is required (bundle name becomes the model directory)")
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("name %q must not contain path separators", name)
	}
	if name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return fmt.Errorf("name %q must not start with '.' or be a relative path element", name)
	}
	if strings.ContainsAny(name, `:*?"<>|`) {
		return fmt.Errorf("name %q contains characters not allowed in a directory name", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("name %q contains control characters", name)
		}
	}
	return nil
}

// totalBytesOfProfile — суммарный размер файлов профиля (для логов).
func totalBytesOfProfile(p *types.ImageModelProfile) int64 {
	if p == nil {
		return 0
	}
	var total int64
	for _, f := range p.Files {
		total += f.SizeBytes
	}
	return total
}
