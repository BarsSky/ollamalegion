// image_catalog_test.go — R85 (2026-10-06): каталог image-моделей и автозагрузка.
//
// ЧТО ИМЕННО ФИКСИРУЕМ:
//   - каталог собирается из снимков воркеров и профилей оператора, а описания
//     (strengths/notes) попадают в ответ — именно их читает текстовая модель,
//     выбирая модель под запрос;
//   - гейт объявления инструмента R85: при ALLOW_LOAD=on достаточно НАЛИЧИЯ
//     моделей (загружать умеем), при off — по-прежнему нужна загруженная модель;
//   - вызов не загруженной модели реально поднимает её (POST /api/image/models/load)
//     и возвращает loadSeconds, чтобы модель объяснила пользователю задержку;
//   - list_image_models отдаёт каталог и НЕ трогает GPU.
package balancer

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/config"
	"ollama-loadbalancer/pkg/types"
)

// imgResWorkerTwoModels — воркер со ЗАГРУЖЕННОЙ моделью и второй, лежащей на
// диске: ровно тот случай, ради которого делалась автозагрузка.
const imgResWorkerTwoModels = `{"models":[` +
	`{"name":"sd15-q8-0","state":"loaded","family":"sd15","size_bytes":1800000000,"vram_estimate_mb":2100,"active_queries":0},` +
	`{"name":"flux-schnell-q3-k","state":"not_loaded","family":"flux","size_bytes":5500000000,"vram_estimate_mb":5200,"active_queries":0}` +
	`],"state":"loaded","current_model":"sd15-q8-0"}`

const imgResWorkerOnlyDiskModel = `{"models":[` +
	`{"name":"flux-schnell-q3-k","state":"not_loaded","family":"flux","size_bytes":5500000000,"vram_estimate_mb":5200,"active_queries":0}` +
	`],"state":"not_loaded","current_model":""}`

// writeImageProfileFile — файл профилей оператора (путь через env, как в проде).
func writeImageProfileFile(t *testing.T, models map[string]types.ImageModelProfile) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "image-model-profiles.json")
	doc := map[string]interface{}{"version": 1, "profiles": models}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal profiles: %v", err)
	}
	if err := writeFileForTest(path, string(raw)); err != nil {
		t.Fatalf("запись профилей: %v", err)
	}
	t.Setenv(config.EnvImageModelProfilesPath, path)
}

// catalogProxy — стенд каталога: один image-бэкенд, ответ воркера задаётся
// телом modelsBody, профили — необязательно.
//
// ТАЙМАУТ ЗАГРУЗКИ ЗАНИЖЕН ОСОЗНАННО: продовый дефолт — 600 с, и тест, в
// котором модель так и не поднимается, ждал бы его целиком. 30 с хватает, чтобы
// успел сработать переход not_loaded → loaded, и мало, чтобы повесить пакет.
func catalogProxy(t *testing.T, modelsBody string) (*Proxy, *imgResStub) {
	t.Helper()
	t.Setenv("LB_IMAGE_TOOL_ALLOW_LOAD", "on")
	t.Setenv("LB_IMAGE_TOOL_LOAD_TIMEOUT_SEC", "30")
	ResetImageCatalogCache()
	t.Cleanup(ResetImageCatalogCache)
	p, stub := newImgResProxy(t, toolTestImageSettings())
	stub.setModels(modelsBody)
	return p, stub
}

// TestImageCatalog_CollectsModelsWithProfilesAndDescriptions — главный критерий
// приёмки: каталог отдаёт модели с параметрами, VRAM-оценкой, состоянием и
// ОПИСАНИЕМ из профиля оператора.
func TestImageCatalog_CollectsModelsWithProfilesAndDescriptions(t *testing.T) {
	writeImageProfileFile(t, map[string]types.ImageModelProfile{
		"flux-schnell-q3-k": {
			Name:           "flux-schnell-q3-k",
			Family:         "flux",
			Files:          []types.ImageModelFile{{Role: types.ImageFileRoleDiffusion, Repo: "r", Filename: "f.gguf", SizeBytes: 5700000000}},
			Defaults:       types.DefaultImageGenDefaults("flux"),
			VramEstimateMB: 5900,
			Notes:          "Q3_K, TE на CPU обязателен",
			Strengths:      "лучшее качество из каталога, 1024x1024 за 4 шага",
		},
	})
	p, _ := catalogProxy(t, imgResWorkerTwoModels)

	cat := p.ImageCatalogFor(context.Background())
	if len(cat.Backends) != 1 || cat.Backends[0].ID != "img-1" {
		t.Fatalf("бэкенды = %+v", cat.Backends)
	}
	if !cat.Backends[0].ContractOK || cat.Backends[0].State != imageStateLoaded {
		t.Errorf("состояние бэкенда не собрано: %+v", cat.Backends[0])
	}
	if !cat.HasLoadedModel {
		t.Error("hasLoadedModel=false при наличии загруженной модели")
	}
	if len(cat.Models) != 2 {
		t.Fatalf("модели = %+v, want 2", cat.Models)
	}

	byName := map[string]ImageCatalogModel{}
	for _, m := range cat.Models {
		byName[m.Name] = m
	}
	loaded := byName["sd15-q8-0"]
	if !loaded.Loaded || loaded.State != imageStateLoaded {
		t.Errorf("загруженная модель помечена неверно: %+v", loaded)
	}
	// Размер — от воркера (профилей/пресетов он не знает), VRAM — из каталога
	// пресетов: у sd15-q4-0/q8-0 он там указан, и он приоритетнее воркерской
	// оценки (см. комментарий в catalogModelFor).
	if loaded.SizeBytes != 1800000000 {
		t.Errorf("размер от воркера потерян: %+v", loaded)
	}
	if loaded.VramEstimateMB != 2100 || loaded.VramSource != "catalog" {
		t.Errorf("VRAM пресета должен приоритетнее воркерской оценки: %+v", loaded)
	}
	if loaded.Strengths == "" {
		t.Error("у модели должно быть описание (пресет или семейство) — иначе выбирать не из чего")
	}

	disk := byName["flux-schnell-q3-k"]
	if disk.Loaded || disk.State != imageStateNotLoaded {
		t.Errorf("not_loaded-модель помечена как загруженная: %+v", disk)
	}
	if disk.Strengths != "лучшее качество из каталога, 1024x1024 за 4 шага" {
		t.Errorf("strengths профиля не дошли до каталога: %q", disk.Strengths)
	}
	if disk.Notes != "Q3_K, TE на CPU обязателен" {
		t.Errorf("notes профиля не дошли до каталога: %q", disk.Notes)
	}
	if disk.Source != "profile" {
		t.Errorf("source = %q, want profile", disk.Source)
	}
	if disk.Defaults.Steps != 4 || disk.Defaults.Width != 1024 {
		t.Errorf("дефолты профиля не дошли: %+v", disk.Defaults)
	}
	if disk.VramSource != "profile" {
		t.Errorf("VRAM профиля приоритетнее воркерской оценки: %+v", disk)
	}

	if cat.Limits.MinSide != 64 || cat.Limits.MaxSide != 4096 || cat.Limits.SizeMultiple != 64 || cat.Limits.MaxSteps != 100 {
		t.Errorf("лимиты каталога = %+v", cat.Limits)
	}
}

// TestImageCatalog_PresetFallback — у модели нет профиля, но она есть в
// поставляемом каталоге пресетов: описания берутся оттуда.
func TestImageCatalog_PresetFallback(t *testing.T) {
	// Профилей нет вовсе (пустой путь) — работает только каталог пресетов.
	t.Setenv(config.EnvImageModelProfilesPath, filepath.Join(t.TempDir(), "no-such-file.json"))
	p, _ := catalogProxy(t, `{"models":[{"name":"sd15-q4-0","state":"not_loaded","family":"sd15","active_queries":0}],"state":"not_loaded","current_model":""}`)

	cat := p.ImageCatalogFor(context.Background())
	if len(cat.Models) != 1 {
		t.Fatalf("модели = %+v", cat.Models)
	}
	m := cat.Models[0]
	if m.Source != "catalog" {
		t.Fatalf("source = %q, want catalog (профиля нет, пресет есть)", m.Source)
	}
	if m.Strengths == "" || m.Notes == "" {
		t.Errorf("описания пресета не подхвачены: strengths=%q notes=%q", m.Strengths, m.Notes)
	}
	if m.Defaults.Steps != 25 || m.Defaults.Width != 512 {
		t.Errorf("дефолты пресета sd15-q4-0 = %+v", m.Defaults)
	}
}

// TestImageCatalog_CachesAndResets — каталог кэшируется (не бомбит воркер на
// каждый запрос чата) и сбрасывается явно.
func TestImageCatalog_CachesAndResets(t *testing.T) {
	p, stub := catalogProxy(t, imgResWorkerTwoModels)

	first := p.ImageCatalogFor(context.Background())
	if first.Cached {
		t.Error("первый сбор не может быть из кэша")
	}
	if _, hits, _ := stub.hitsAll(); hits != 1 {
		t.Fatalf("опросов воркера = %d, want 1", hits)
	}
	second := p.ImageCatalogFor(context.Background())
	if !second.Cached {
		t.Error("второй вызов обязан отдаваться из кэша")
	}
	if _, hits, _ := stub.hitsAll(); hits != 1 {
		t.Errorf("кэш не сработал: опросов воркера = %d", hits)
	}
	if len(second.Models) != len(first.Models) {
		t.Errorf("кэш вернул другой каталог: %d vs %d моделей", len(second.Models), len(first.Models))
	}

	ResetImageCatalogCache()
	third := p.ImageCatalogFor(context.Background())
	if third.Cached {
		t.Error("после Reset каталог должен собираться заново")
	}
}

// TestImageToolTargetFor_AllowLoadDeclaresWithDiskModel — новый гейт: при
// ALLOW_LOAD=on инструмент объявляется, даже когда модель только на диске.
func TestImageToolTargetFor_AllowLoadDeclaresWithDiskModel(t *testing.T) {
	t.Setenv("LB_IMAGE_TOOL_ALLOW_LOAD", "on")
	ResetImageCatalogCache()
	t.Cleanup(ResetImageCatalogCache)

	p, stub := newImgResProxy(t, toolTestImageSettings())
	stub.setModels(imgResWorkerOnlyDiskModel)

	target := p.imageToolTargetFor(context.Background())
	if target == nil {
		t.Fatal("ALLOW_LOAD=on: с моделью на диске инструмент обязан объявляться (иначе модель не сможет её поднять)")
	}
	if !target.AllowLoad {
		t.Error("target.AllowLoad=false при ALLOW_LOAD=on")
	}
	if target.Model != "flux-schnell-q3-k" || target.BackendID != "img-1" {
		t.Fatalf("target=%+v", target)
	}
	if len(target.Models) != 1 || len(target.Families) != 1 || target.Families[0] != "flux" {
		t.Errorf("enum моделей/семейств не собран: %+v", target)
	}
}

// TestImageToolTargetFor_AllowLoadOffRequiresLoadedModel — обратная
// совместимость: при ALLOW_LOAD=off правило прежнее (нужна загруженная модель).
func TestImageToolTargetFor_AllowLoadOffRequiresLoadedModel(t *testing.T) {
	t.Setenv("LB_IMAGE_TOOL_ALLOW_LOAD", "off")
	ResetImageCatalogCache()
	t.Cleanup(ResetImageCatalogCache)

	p, stub := newImgResProxy(t, toolTestImageSettings())
	stub.setModels(imgResWorkerOnlyDiskModel)

	if got := p.imageToolTargetFor(context.Background()); got != nil {
		t.Fatalf("ALLOW_LOAD=off: без загруженной модели инструмент объявлять нельзя: %+v", got)
	}

	// А с загруженной — объявляется, как раньше.
	stub.setModels(imgResWorkerTwoModels)
	ResetImageCatalogCache()
	p2, stub2 := newImgResProxy(t, toolTestImageSettings())
	stub2.setModels(imgResWorkerTwoModels)
	got := p2.imageToolTargetFor(context.Background())
	if got == nil || got.Model != "sd15-q8-0" {
		t.Fatalf("ALLOW_LOAD=off + загруженная модель: target=%+v", got)
	}
	if got.AllowLoad {
		t.Error("ALLOW_LOAD=off не должен разрешать загрузку")
	}
	if len(got.Models) != 2 {
		t.Errorf("enum моделей = %v, want обе модели кластера", got.Models)
	}
}

// TestImageToolOpenAITools_SecondToolOnlyWithAllowLoad — в запрос к модели
// добавляется второй инструмент только тогда, когда им реально можно
// воспользоваться.
func TestImageToolOpenAITools_SecondToolOnlyWithAllowLoad(t *testing.T) {
	off := imageToolOpenAITools(&imageToolTarget{Models: []string{"m1"}, AllowLoad: false})
	if len(off) != 1 {
		t.Fatalf("ALLOW_LOAD=off: инструментов %d, want 1", len(off))
	}
	on := imageToolOpenAITools(&imageToolTarget{Models: []string{"m1"}, Families: []string{"sd15"}, AllowLoad: true})
	if len(on) != 2 {
		t.Fatalf("ALLOW_LOAD=on: инструментов %d, want 2 (generate_image + list_image_models)", len(on))
	}
	// Имена берём из проводной формы (marshal), как их увидит cppworker.
	rawOn, _ := json.Marshal(on)
	var wire []map[string]interface{}
	if err := json.Unmarshal(rawOn, &wire); err != nil {
		t.Fatalf("unmarshal tools: %v", err)
	}
	if got := imageToolNamesHeader(wire); got != "generate_image,list_image_models" {
		t.Errorf("заголовок X-Image-Tool = %q", got)
	}
	// Второй инструмент — каталог с enum семейств.
	if !strings.Contains(string(rawOn), "list_image_models") || !strings.Contains(string(rawOn), "sd15") {
		t.Errorf("схема list_image_models = %s", rawOn)
	}
}

// TestGenerateImageForTool_LoadsNotLoadedModel — вызов с model=X при X=not_loaded
// поднимает модель (POST /api/image/models/load), затем генерирует, а в
// результате есть время загрузки.
func TestGenerateImageForTool_LoadsNotLoadedModel(t *testing.T) {
	p, stub := catalogProxy(t, imgResWorkerOnlyDiskModel)
	// После загрузки воркер сообщает ЗАПРОШЕННУЮ модель как loaded.
	stub.setLoadTransition(`{"models":[{"name":"flux-schnell-q3-k","state":"loaded","family":"flux","size_bytes":5500000000,"vram_estimate_mb":5200,"active_queries":0}],"state":"loaded","current_model":"flux-schnell-q3-k"}`)
	stub.setLoadStatus(202)
	stub.setGenBody(`{"created":1,"model":"flux-schnell-q3-k","output_format":"png","seed":7,"width":1024,"height":1024,"steps":4,"duration_ms":2000,"data":[{"index":0,"url":"/images/img_9_zz.png"}]}`)

	target := p.imageToolTargetFor(context.Background())
	if target == nil {
		t.Fatal("нет target")
	}
	res, err := p.generateImageForTool(context.Background(), target, imageToolArgs{
		Prompt: "a cat", Model: "flux-schnell-q3-k",
	})
	if err != nil {
		t.Fatalf("генерация после автозагрузки: %v", err)
	}
	if res["status"] != "ok" || res["url"] != "/images/img_9_zz.png" {
		t.Fatalf("результат=%+v", res)
	}
	if res["model"] != "flux-schnell-q3-k" {
		t.Errorf("сгенерировали не запрошенную модель: %v", res["model"])
	}
	if _, ok := res["loadSeconds"].(float64); !ok {
		t.Errorf("в результате нет loadSeconds — модель не сможет объяснить задержку: %+v", res)
	}
	if res["loadedNow"] != true || res["modelState"] != imageStateLoaded {
		t.Errorf("нет признака свежей загрузки: %+v", res)
	}
	if _, _, loads := stub.hitsAll(); loads != 1 {
		t.Errorf("запросов на load = %d, want 1", loads)
	}
}

// TestGenerateImageForTool_NoLoadWhenAlreadyLoaded — загруженную модель не
// трогаем: ни запроса на load, ни loadSeconds в результате.
func TestGenerateImageForTool_NoLoadWhenAlreadyLoaded(t *testing.T) {
	p, stub := catalogProxy(t, imgResWorkerTwoModels)
	stub.setGenBody(`{"created":1,"output_format":"png","data":[{"index":0,"url":"/images/img_1_a.png"}]}`)

	target := p.imageToolTargetFor(context.Background())
	if target == nil {
		t.Fatal("нет target")
	}

	loaded := false
	for _, m := range target.catalog.Models {
		if m.Loaded {
			loaded = true
		}
	}
	if !loaded {
		t.Fatalf("гейт должен выбрать ЗАГРУЖЕННУЮ модель: %+v", target.catalog.Models)
	}
	res, err := p.generateImageForTool(context.Background(), target, imageToolArgs{Prompt: "a cat"})
	if err != nil {
		t.Fatalf("генерация: %v", err)
	}
	if _, ok := res["loadSeconds"]; ok {
		t.Errorf("загрузки не было — поля loadSeconds быть не должно: %+v", res)
	}
	if _, ok := res["loadedNow"]; ok {
		t.Errorf("поля loadedNow быть не должно: %+v", res)
	}
	if _, _, loads := stub.hitsAll(); loads != 0 {
		t.Errorf("запросов на load = %d, want 0", loads)
	}
}

// TestGenerateImageForTool_AllowLoadOffRefusesToLoad — при ALLOW_LOAD=off
// автозагрузки нет: честная ошибка вместо тихой попытки.
func TestGenerateImageForTool_AllowLoadOffRefusesToLoad(t *testing.T) {
	t.Setenv("LB_IMAGE_TOOL_ALLOW_LOAD", "on")
	ResetImageCatalogCache()
	t.Cleanup(ResetImageCatalogCache)

	p, stub := newImgResProxy(t, toolTestImageSettings())
	stub.setModels(imgResWorkerTwoModels)
	target := p.imageToolTargetFor(context.Background())
	if target == nil {
		t.Fatal("нет target")
	}
	// Оператор выключил автозагрузку ПОСЛЕ объявления инструмента.
	t.Setenv("LB_IMAGE_TOOL_ALLOW_LOAD", "off")

	_, err := p.generateImageForTool(context.Background(), target, imageToolArgs{
		Prompt: "a cat", Model: "flux-schnell-q3-k",
	})
	if err == nil {
		t.Fatal("ALLOW_LOAD=off: поднимать модель из вызова нельзя")
	}
	if !strings.Contains(err.Error(), "автозагрузка выключена") {
		t.Errorf("ошибка должна объяснять причину: %v", err)
	}
	if _, _, loads := stub.hitsAll(); loads != 0 {
		t.Errorf("запросов на load = %d, want 0", loads)
	}
}

// TestEnsureImageModelLoaded_ConfigTimeoutBeatsEnv — R86-follow-up: ожидание
// загрузки из КОНФИГА (правится в WebUI) сильнее переменной окружения.
//
// ЗАЧЕМ: на стенде модель 4.7 ГБ не поднялась за дефолтные 600 с. Оператор должен
// иметь возможность поднять лимит из WebUI без правки compose и перезапуска —
// ровно как у галочки автозагрузки. Проверяем оба направления: конфиг задан —
// побеждает он; конфиг не задан (nil) — действует env.
func TestEnsureImageModelLoaded_ConfigTimeoutBeatsEnv(t *testing.T) {
	// env намеренно «щедрый»: если бы конфиг не побеждал, тест ждал бы его.
	t.Setenv("LB_IMAGE_TOOL_LOAD_TIMEOUT_SEC", "600")
	ResetImageCatalogCache()
	t.Cleanup(ResetImageCatalogCache)

	p, stub := newImgResProxy(t, toolTestImageSettings())
	stub.setModels(imgResWorkerOnlyDiskModel) // так и остаётся not_loaded
	stub.setLoadStatus(202)

	// 1) Заданное в конфиге значение перекрывает env.
	oneSec := 1
	p.config.Balancing.Image.ToolLoadTimeoutSec = &oneSec
	if got := p.imageToolSettings().LoadTimeout; got != time.Second {
		t.Fatalf("конфиг не перекрыл env: LoadTimeout=%v", got)
	}

	started := time.Now()
	if _, err := p.ensureImageModelLoaded(context.Background(), "img-1", "flux-schnell-q3-k"); err == nil {
		t.Fatal("ожидалась ошибка по таймауту из конфига")
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("таймаут из конфига не сработал: ждали %s", elapsed)
	}

	// 2) Конфиг не задан → действует env.
	p.config.Balancing.Image.ToolLoadTimeoutSec = nil
	if got := p.imageToolSettings().LoadTimeout; got != 600*time.Second {
		t.Fatalf("без настройки должен действовать env: LoadTimeout=%v", got)
	}

	// 3) Явное большое значение из конфига видно как есть (большая модель на
	//    медленном диске — оператор поднимает лимит).
	thirtyMin := 1800
	p.config.Balancing.Image.ToolLoadTimeoutSec = &thirtyMin
	if got := p.imageToolSettings().LoadTimeout; got != 30*time.Minute {
		t.Fatalf("значение 1800 с не применено: %v", got)
	}
}

// TestGenerateImageForTool_UnknownModelSubstitutesLoaded — модель назвала
// несуществующее имя (живой случай: "stable-diffusion.cpp"), но в VRAM уже есть
// готовая модель: рисуем ею, а подмену честно сообщаем. Пользователь просил
// картинку, а не конкретный файл, поэтому отказ тут — худший ответ.
func TestGenerateImageForTool_UnknownModelSubstitutesLoaded(t *testing.T) {
	p, stub := catalogProxy(t, imgResWorkerTwoModels) // загружена sd15-q8-0
	stub.setGenBody(`{"created":1,"model":"sd15-q8-0","output_format":"png","seed":5,"width":512,"height":512,"steps":8,"duration_ms":900,"data":[{"url":"/images/img_sub.png"}]}`)

	target := p.imageToolTargetFor(context.Background())
	if target == nil {
		t.Fatal("нет target")
	}
	res, err := p.generateImageForTool(context.Background(), target, imageToolArgs{
		Prompt: "лес в стиле Кандинского", Model: "stable-diffusion.cpp",
	})
	if err != nil {
		t.Fatalf("с загруженной моделью вызов должен пройти подменой, got %v", err)
	}
	if res["status"] != "ok" || res["url"] != "/images/img_sub.png" {
		t.Fatalf("результат=%+v", res)
	}
	if res["model"] != "sd15-q8-0" {
		t.Errorf("нарисовали не загруженной моделью: %v", res["model"])
	}
	if res["requestedModel"] != "stable-diffusion.cpp" {
		t.Errorf("не сообщили, какое имя запросила модель: %+v", res)
	}
	if note, _ := res["modelNote"].(string); !strings.Contains(note, "stable-diffusion.cpp") {
		t.Errorf("нет пояснения о подмене: %v", res["modelNote"])
	}
	// Никакой загрузки: модель уже была в VRAM.
	if _, _, loadHits := stub.hitsAll(); loadHits != 0 {
		t.Errorf("загрузка не требовалась, а запросов на load = %d", loadHits)
	}
}

// TestGenerateImageForTool_UnknownModelListsAvailable — если загруженной модели
// НЕТ, выдуманное имя не должно превращаться в непонятный 404 от движка: в ошибке
// перечисляем доступные имена, чтобы модель исправилась.
func TestGenerateImageForTool_UnknownModelListsAvailable(t *testing.T) {
	p, stub := catalogProxy(t, imgResWorkerOnlyDiskModel) // загруженной модели нет
	stub.setGenBody(`{"created":1,"output_format":"png","data":[{"url":"/images/nope.png"}]}`)

	target := p.imageToolTargetFor(context.Background())
	if target == nil {
		t.Fatal("нет target")
	}

	before, _, _ := stub.hitsAll()
	_, err := p.generateImageForTool(context.Background(), target, imageToolArgs{
		Prompt: "a cat", Model: "stable-diffusion:1.5",
	})
	if err == nil {
		t.Fatal("неизвестное имя модели обязано давать ошибку, когда подставить нечего")
	}
	msg := err.Error()
	for _, want := range []string{"stable-diffusion:1.5", "flux-schnell-q3-k", "list_image_models"} {
		if !strings.Contains(msg, want) {
			t.Errorf("в ошибке нет %q: %s", want, msg)
		}
	}
	// Воркер не должен получать заведомо неизвестное имя — но генерации не было,
	// поэтому сравниваем счётчики генераций и загрузок.
	afterGen, _, afterLoad := stub.hitsAll()
	if afterGen != before || afterLoad != 0 {
		t.Errorf("воркер вызван для заведомо неизвестного имени (gen %d->%d, load %d)", before, afterGen, afterLoad)
	}
}

// TestEnsureImageModelLoaded_TimesOut — незагружаемая модель заканчивается
// честной ошибкой с таймаутом, а не бесконечным ожиданием.
func TestEnsureImageModelLoaded_TimesOut(t *testing.T) {
	t.Setenv("LB_IMAGE_TOOL_LOAD_TIMEOUT_SEC", "1")
	ResetImageCatalogCache()
	t.Cleanup(ResetImageCatalogCache)

	p, stub := newImgResProxy(t, toolTestImageSettings())
	stub.setModels(imgResWorkerOnlyDiskModel) // так и остаётся not_loaded
	stub.setLoadStatus(202)

	started := time.Now()
	_, err := p.ensureImageModelLoaded(context.Background(), "img-1", "flux-schnell-q3-k")
	if err == nil {
		t.Fatal("модель не поднялась — ожидалась ошибка по таймауту")
	}
	if !strings.Contains(err.Error(), "не поднялась") {
		t.Errorf("ошибка должна говорить про таймаут: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("таймаут не сработал: ждали %s", elapsed)
	}
}

// TestEnsureImageModelLoaded_ReportsModelError — воркер сообщил state=error:
// ошибка загрузки уходит наверх вместе с причиной.
func TestEnsureImageModelLoaded_ReportsModelError(t *testing.T) {
	p, stub := newImgResProxy(t, toolTestImageSettings())
	stub.setModels(`{"models":[{"name":"broken","state":"error","family":"sd15","error":"new_sd_ctx_t failed"}],"state":"error","current_model":""}`)
	stub.setLoadStatus(202)

	_, err := p.ensureImageModelLoaded(context.Background(), "img-1", "broken")
	if err == nil {
		t.Fatal("state=error должен давать ошибку")
	}
	if !strings.Contains(err.Error(), "new_sd_ctx_t failed") {
		t.Errorf("причина от воркера потеряна: %v", err)
	}
}

// TestImageToolCatalogResult_ListsAndDoesNotTouchGPU — list_image_models отдаёт
// каталог, фильтрует по семейству и НЕ вызывает генерацию.
func TestImageToolCatalogResult_ListsAndDoesNotTouchGPU(t *testing.T) {
	p, stub := catalogProxy(t, imgResWorkerTwoModels)

	res := p.imageToolCatalogResult(context.Background(), "")
	if res["status"] != "ok" || res["count"] != 2 {
		t.Fatalf("каталог целиком: %+v", res)
	}
	summary, _ := res["summary"].(string)
	for _, want := range []string{"sd15-q8-0", "flux-schnell-q3-k", "уже в VRAM", "VRAM"} {
		if !strings.Contains(summary, want) {
			t.Errorf("в summary нет %q:\n%s", want, summary)
		}
	}
	if res["loadedModel"] != "sd15-q8-0" {
		t.Errorf("loadedModel = %v", res["loadedModel"])
	}

	filtered := p.imageToolCatalogResult(context.Background(), "FLUX")
	if filtered["count"] != 1 || filtered["family"] != "flux" {
		t.Fatalf("фильтр по семейству: %+v", filtered)
	}

	empty := p.imageToolCatalogResult(context.Background(), "nope")
	if empty["count"] != 0 {
		t.Fatalf("фильтр по несуществующему семейству: %+v", empty)
	}
	// Пустой фильтр обязан не молчать, а перечислить доступные имена: иначе модель
	// выдумывает имя из своих знаний (живой случай 2026-10-06: family=stable-diffusion
	// → пусто → generate_image с именем "stable-diffusion:1.5", которого нет).
	if s, _ := empty["summary"].(string); !strings.Contains(s, "моделей нет") {
		t.Errorf("пустой фильтр должен объясняться словами: %q", s)
	}
	if s, _ := empty["summary"].(string); !strings.Contains(s, "sd15-q8-0") {
		t.Errorf("в тексте ответа должны быть доступные имена: %q", s)
	}
	if avail, _ := empty["availableModels"].([]string); len(avail) != 2 {
		t.Errorf("в пустом ответе нет machine-readable перечня моделей: %+v", empty)
	}
	if status, _ := empty["status"].(string); status != "empty" {
		t.Errorf("status пустого фильтра = %q, want empty", status)
	}

	// GPU не тронут: ни одной генерации.
	if gens, _, _ := stub.hitsAll(); gens != 0 {
		t.Errorf("list_image_models вызвал генерацию (%d раз) — он не должен тратить GPU", gens)
	}
}

// TestImageToolLoop_ListCallDoesNotConsumeGenerationBudget — вызов каталога не
// съедает лимит генераций (иначе «каталог + картинка» теряли бы картинку).
func TestImageToolLoop_ListCallDoesNotConsumeGenerationBudget(t *testing.T) {
	t.Setenv("LB_IMAGE_TOOL_MAX_CALLS", "1")
	p, stub := catalogProxy(t, imgResWorkerTwoModels)
	stub.setGenBody(`{"created":1,"output_format":"png","data":[{"index":0,"url":"/images/img_1_a.png"}]}`)

	calls := []imageToolCall{
		{ID: "c0", Name: "list_image_models", Arguments: `{}`},
		{ID: "c1", Name: "generate_image", Arguments: `{"prompt":"a cat"}`},
	}
	if !calls[0].isListCall() || calls[1].isListCall() {
		t.Fatalf("isListCall определён неверно: %+v", calls)
	}
	target := p.imageToolTargetFor(context.Background())
	if target == nil {
		t.Fatal("нет target")
	}
	results := []map[string]interface{}{
		p.imageToolCatalogResult(context.Background(), ""),
		{"status": "ok", "url": "/images/img_1_a.png", "markdown": "![a cat](/images/img_1_a.png)"},
	}

	body := []byte(`{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"generate_image"}},{"type":"function","function":{"name":"list_image_models"}},{"type":"function","function":{"name":"get_weather"}}]}`)
	out, err := buildFollowUpBody(body, map[string]interface{}{"role": "assistant"}, calls, results)
	if err != nil {
		t.Fatalf("buildFollowUpBody: %v", err)
	}
	var doc map[string]interface{}
	_ = json.Unmarshal(out, &doc)

	// Оба наших инструмента убраны, клиентский остался.
	tools, _ := doc["tools"].([]interface{})
	if len(tools) != 1 || openAIToolName(tools[0]) != "get_weather" {
		t.Fatalf("наши инструменты не убраны из turn 2: %s", out)
	}
	// В истории — два tool-сообщения: каталог и картинка.
	msgs, _ := doc["messages"].([]interface{})
	var toolMsgs int
	for _, raw := range msgs {
		m, _ := raw.(map[string]interface{})
		if m["role"] == "tool" {
			toolMsgs++
			if content, _ := m["content"].(string); strings.Contains(content, "Доступные image-модели") && m["tool_call_id"] != "c0" {
				t.Errorf("каталог должен быть ответом на c0: %v", m)
			}
		}
	}
	if toolMsgs != 2 {
		t.Errorf("tool-сообщений %d, want 2: %s", toolMsgs, out)
	}
}
