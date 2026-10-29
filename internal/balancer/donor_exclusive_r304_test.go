// donor_exclusive_r304_test.go — R304 (2026-10-30): донор держит только память.
//
// ДОКТРИНА ОПЕРАТОРА: «если бэкенд становится донором, он уже не может подымать
// свои копии моделей — он становится именно донором мастера и может только
// делить память с мастером».
//
// ЧТО ЗДЕСЬ ЗАФИКСИРОВАНО, А НЕ ПЕРЕСКАЗАНО. До этой правки роль donor не
// проверял НИ ОДИН путь выбора: единственным фильтром «узел не держит модель
// целиком» был воркер логического бэкенда (R92), а он в обычном развёртывании не
// настроен. Тесты ниже — на каждый класс пути отдельно (обслуживание запроса,
// размещение вместо перезагрузки, загрузка с нуля по файлу на диске, эвакуация
// сессии), плюс сторож: точка выбора, забывшая общий предикат, ломает сборку
// тестов, а не живой стенд.
package balancer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/settings"
	"ollama-loadbalancer/pkg/types"
)

const (
	r304Main   = "worker-main"
	r304Donor  = "worker-donor"
	r304Free   = "worker-standalone"
	r304Model  = "gemma-4-E4B-it-Q4_K_M"
	r304Second = "qwen3-14b"
)

// newDonorProxyR304 — три узла раскладки в том виде, в каком они бывают живьём:
//
//	worker-main       main,       модель загружена
//	worker-donor      donor,      модель загружена (нарушение роли — так и было)
//	worker-standalone standalone, свободен, модель не загружена
//
// Сценарий «у донора осталась копия» выбран намеренно: именно его наблюдали на
// стенде («второй агент вырвал модель у первого»), и именно он обязан быть
// безопасным даже тогда, когда инвариант уже нарушен.
func newDonorProxyR304(t *testing.T) *Proxy {
	t.Helper()
	p := &Proxy{
		config:       &types.LoadBalancerConfig{},
		backends:     map[string]*BackendState{},
		metricsMgr:   NewMetricsManager(),
		modelCatalog: newModelCatalog(),
	}
	add := func(id, role, model string, active int) {
		state := &BackendState{Backend: &types.Backend{
			ID: id, Host: "10.0.0." + id[len(id)-1:], Type: types.BackendTypeLlamaCpp,
			Status: types.StatusHealthy, MaxConcurrentReqs: 2, Role: role,
		}}
		state.ActiveReqs = active
		p.backends[id] = state
		// Метрики есть у КАЖДОГО узла, даже без модели: опрошенный узел с пустым
		// списком моделей — это факт «модели нет», а отсутствие метрик — «не
		// опрошен». Разница важна: часть путей (эвакуация) без метрик считает
		// узел подходящим, и тест на такой фикстуре доказывал бы не то.
		metrics := &types.BackendMetrics{}
		if model != "" {
			metrics.Ollama.RunningModels = []types.RunningModel{{Name: model}}
			p.metricsMgr.llamaMetrics[id] = &types.LlamaCppMetrics{
				LoadedModels: []types.LlamaCppModel{{
					Name: model, State: "loaded", ContextLength: 32768, ContextPerSeq: 16384, MaxSlots: 2,
				}},
			}
		}
		p.metricsMgr.metrics[id] = metrics
	}
	// Донор свободнее всех (0 запросов) — по прежнему правилу «наименее
	// загруженная копия» он и выигрывал. Это предпосылка тестов, а не мнение.
	add(r304Main, types.BackendRoleMain, r304Model, 1)
	add(r304Donor, types.BackendRoleDonor, r304Model, 0)
	add(r304Free, types.BackendRoleStandalone, "", 0)
	p.rebuildDonorIndexR304()
	return p
}

// TestR304_IndexKnowsRolesAndMaster — снимок ролей отвечает на два вопроса:
// «это донор?» и «чей он». Второй нужен панели и логу: «донор» без мастера —
// это роль, которая ничего не отдаёт.
func TestR304_IndexKnowsRolesAndMaster(t *testing.T) {
	p := newDonorProxyR304(t)

	if !p.isDonorR304(r304Donor) {
		t.Fatal("донор не опознан по роли")
	}
	if p.isDonorR304(r304Main) || p.isDonorR304(r304Free) {
		t.Error("мастер/самодостаточный названы донорами")
	}
	if got := p.donorMasterIDR304(r304Donor); got != r304Main {
		t.Errorf("мастер донора = %q, want %q", got, r304Main)
	}

	// Смена роли обязана менять предикат СРАЗУ, а не через тик гарда (15 с):
	// в этом окне селектор успел бы выбрать бывшего донора исполнителем.
	p.backends[r304Free].Backend.Role = types.BackendRoleDonor
	p.rebuildDonorIndexR304()
	if !p.isDonorR304(r304Free) {
		t.Fatal("новая роль донора не попала в снимок")
	}

	// Донор без мастера — честное «мастер не назначен», а не пустая строка на
	// месте чужого ID.
	p.backends[r304Main].Backend.Role = types.BackendRoleStandalone
	p.rebuildDonorIndexR304()
	if got := p.donorMasterIDR304(r304Donor); got != "" {
		t.Errorf("без мастера donorMasterIDR304 = %q, want пусто", got)
	}
}

// TestR304_LoadedDonorCopyIsNotServed — главный живой случай: у донора осталась
// копия модели, и он свободнее мастера. Запрос всё равно обязан уйти мастеру.
func TestR304_LoadedDonorCopyIsNotServed(t *testing.T) {
	p := newDonorProxyR304(t)

	if got := p.findLeastLoadedBackendWithModel(r304Model, allowedLlama()); got != r304Main {
		t.Fatalf("исполнитель = %q, want %q (свободный донор с копией не исполнитель)", got, r304Main)
	}
	if got := p.findBackendWithModelExcluding(r304Model, map[string]bool{r304Main: true}, allowedLlama()); got == r304Donor {
		t.Fatal("findBackendWithModelExcluding вернул донора: память узла отдана мастеру")
	}
	if cands := p.collectModelCopyCandidates(r304Model, nil, allowedLlama()); len(cands) != 1 || cands[0].ID != r304Main {
		t.Fatalf("кандидаты копий = %+v, ожидалась только копия мастера", cands)
	}
	if got := p.selectBackendPreferWindow(r304Model, types.BackendTypeLlamaCpp, 8192); got == r304Donor {
		t.Fatal("выбор с окном отдал донора")
	}
}

// TestR304_DonorIsNotPlacementTarget — R293 (размещение вместо перезагрузки)
// обязан обходить донора: файл модели у него есть, VRAM свободна только на
// бумаге — её держит мастер.
func TestR304_DonorIsNotPlacementTarget(t *testing.T) {
	p := newDonorProxyR304(t)
	if p.metricsMgr.llamaMetrics[r304Donor] == nil {
		p.metricsMgr.llamaMetrics[r304Donor] = &types.LlamaCppMetrics{}
	}
	cands := p.collectPlacementCandidatesR293(r304Model, 8192, r304Main)
	for _, c := range cands {
		if c.ID == r304Donor {
			t.Fatalf("донор попал в кандидаты размещения: %+v", cands)
		}
	}

	// Загрузка с нуля по файлу на диске: каталог знает модель ТОЛЬКО у донора.
	p.modelCatalog.store(r304Donor, []string{r304Model}, "", time.Now())
	if got := p.findLeastLoadedBackendWithModelOnDisk(r304Model, allowedLlama()); got != "" {
		t.Fatalf("загрузка с нуля выбрала %q, а файл есть только у донора", got)
	}
	// Контроль: как только донор перестаёт быть донором, тот же путь его видит.
	p.backends[r304Donor].Backend.Role = types.BackendRoleStandalone
	p.rebuildDonorIndexR304()
	if got := p.findLeastLoadedBackendWithModelOnDisk(r304Model, allowedLlama()); got != r304Donor {
		t.Fatalf("после снятия роли выбор = %q, want %q (иначе тест не доказывает роль)", got, r304Donor)
	}
}

// TestR304_EvacuationSkipsDonor — эвакуация сессий с падающего узла. Слабейший
// фильтр в проекте (только healthy): донор проходил его насквозь, и сессии
// уезжали в память, занятую слоями мастера.
func TestR304_EvacuationSkipsDonor(t *testing.T) {
	p := newDonorProxyR304(t)

	// Уводим сессию с мастера; единственная копия модели — у донора.
	if got := p.findHealthyBackendForModel(r304Model, r304Main); got != "" {
		t.Fatalf("эвакуация увела сессию на %q, ожидалось «некуда»", got)
	}
	p.backends[r304Donor].Backend.Role = types.BackendRoleStandalone
	p.rebuildDonorIndexR304()
	if got := p.findHealthyBackendForModel(r304Model, r304Main); got != r304Donor {
		t.Fatalf("после снятия роли эвакуация = %q, want %q", got, r304Donor)
	}
}

// TestR304_MarkNamesMasterAndOwnCopies — пометка для панели: чей это донор,
// можно ли ему грузить и что у него уже загружено вопреки роли.
func TestR304_MarkNamesMasterAndOwnCopies(t *testing.T) {
	p := newDonorProxyR304(t)

	mark := p.DonorMarkR304(r304Donor)
	if !mark.Donor {
		t.Fatal("пометка не назвала донора")
	}
	if mark.MasterBackendID != r304Main {
		t.Errorf("мастер = %q, want %q", mark.MasterBackendID, r304Main)
	}
	if mark.CanHostOwnModels {
		t.Error("донору разрешено держать свои копии — это и есть дефект")
	}
	if len(mark.OwnCopies) != 1 || mark.OwnCopies[0] != r304Model {
		t.Errorf("ownCopies = %v, want [%s]", mark.OwnCopies, r304Model)
	}
	if mark.Note == "" {
		t.Error("нет пояснения для панели: «донор» без причины оператору ничего не говорит")
	}

	// Не-донор: пометки нет, копии разрешены.
	if m := p.DonorMarkR304(r304Free); m.Donor || !m.CanHostOwnModels || len(m.OwnCopies) != 0 {
		t.Errorf("пометка самодостаточного узла: %+v", m)
	}

	// Нарушение инварианта обязано попадать в отчёт, даже когда роль снята:
	// иначе «донор» рядом с загруженной копией выглядел бы противоречием UI.
	p.backends[r304Donor].Backend.Role = types.BackendRoleStandalone
	p.rebuildDonorIndexR304()
	if m := p.DonorMarkR304(r304Donor); m.Donor || !m.CanHostOwnModels {
		t.Errorf("после снятия роли: %+v", m)
	}
}

// TestR304_AllowFlagRestoresOldBehaviour — исключение оператора действует через
// ТОТ ЖЕ предикат. Иначе «выключено» оставляло бы половину путей с новым
// поведением, и флаг перестал бы быть обратимым.
func TestR304_AllowFlagRestoresOldBehaviour(t *testing.T) {
	reg := withSettingsRegistry(t)
	p := newDonorProxyR304(t)

	if p.cannotHostModelR304(r304Donor) != true {
		t.Fatal("по умолчанию донор не должен считаться хозяином копий")
	}
	if _, err := reg.Set("LB_DONOR_ALLOW_OWN_COPIES", 1); err != nil {
		t.Fatalf("Set LB_DONOR_ALLOW_OWN_COPIES: %v", err)
	}
	if p.cannotHostModelR304(r304Donor) {
		t.Fatal("флаг 1 не вернул прежнее поведение (донор с копией)")
	}
	if got := p.findLeastLoadedBackendWithModel(r304Model, allowedLlama()); got != r304Donor {
		t.Fatalf("с флагом 1 выбор = %q, want свободный донор %q", got, r304Donor)
	}
	if mark := p.DonorMarkR304(r304Donor); !mark.CanHostOwnModels {
		t.Error("пометка не отразила исключение оператора")
	}
}

// TestR304_SettingRegistered — настройка живёт в реестре, а не в env-константе:
// иначе её не видно в панели и нельзя изменить без рестарта.
func TestR304_SettingRegistered(t *testing.T) {
	reg := settings.Init("")
	t.Cleanup(func() { settings.Init("") })

	def, ok := reg.Definition("LB_DONOR_ALLOW_OWN_COPIES")
	if !ok {
		t.Fatal("LB_DONOR_ALLOW_OWN_COPIES не зарегистрирована")
	}
	if !def.HotReload {
		t.Error("HotReload = false, а обязана меняться без рестарта")
	}
	if def.Default != 0 {
		t.Errorf("Default = %d, want 0 (донор только память — доктрина, а не опция)", def.Default)
	}
	if def.Validate != nil {
		if err := def.Validate(def.Default); err != nil {
			t.Errorf("дефолт не проходит собственную проверку: %v", err)
		}
	}
}

// --- сторож: ни один путь выбора не обходит общий предикат ------------------

// r304SelectionSites — точки выбора исполнителя, которые ОБЯЗАНЫ спрашивать
// cannotHostModelR304. Список явный, а не выведенный: переименование функции
// ломает тест громко, и это лучше, чем молча потерянный фильтр.
var r304SelectionSites = []struct{ file, fn string }{
	{"backend_selector.go", "findLeastLoadedBackendWithModel"},
	{"backend_selector.go", "findBackendWithModelExcludingLocked"},
	{"backend_selector.go", "selectByResourcesExcluding"},
	{"backend_selector.go", "findLessLoadedBackendWithModel"},
	{"backend_selector.go", "findLessLoadedBackendAny"},
	{"backend_selector.go", "findBackendWithModel"},
	{"backend_selector.go", "findWarmingBackendForModelUnsafe"},
	{"backend_selector.go", "findFreeBackendForModelUnsafe"},
	{"backend_selector.go", "selectFreeBackendAny"},
	{"backend_selector.go", "selectByResources"},
	{"candidate.go", "expandCandidates"},
	{"model_copy_window_r118.go", "collectModelCopyCandidates"},
	{"live_copy_probe_r303.go", "liveCopyCandidateIDsR303"},
	{"placement_over_reload_r293.go", "collectPlacementCandidatesR293"},
	{"placement_fit_r75.go", "placementFitForModel"},
	{"slot_manager.go", "findModelOnAnyBackendNoVRAMCheck"},
	{"slot_manager.go", "selectAnyHealthy"},
	{"setup_plan_r96.go", "planCandidatesR96"},
	{"model_catalog.go", "findLeastLoadedOnDisk"},
	{"backend_registry.go", "findHealthyBackendForModel"},
	{"llamacpp_backend_helpers.go", "findModelOnLlamaCppBackend"},
	{"llamacpp_backend_helpers.go", "selectAnyLlamaCppHealthy"},
	{"llamacpp_backend_helpers.go", "selectLlamaCppBackendExcluding"},
	{"ollama_router.go", "findBackendWithModel"},
	{"ollama_router.go", "selectAnyHealthy"},
}

// TestR304_SelectionSitesAskSharedPredicate — сторож против повторяющегося класса
// дефектов проекта: «новый путь выбора обошёл общий фильтр». Проверяет и наличие
// вызова, и то, что никто не спрашивает голый isGroupWorkerR92 мимо объединённого
// предиката.
func TestR304_SelectionSitesAskSharedPredicate(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	funcs := parseBalancerFuncsR304(t, dir)

	for _, site := range r304SelectionSites {
		fn, ok := funcs[site.file+"."+site.fn]
		if !ok {
			t.Errorf("%s: функция %s не найдена (переименована?) — обнови список сторожевого теста",
				site.file, site.fn)
			continue
		}
		if !fn.callsSharedPredicate {
			t.Errorf("%s: %s не спрашивает cannotHostModelR304 — донор снова может стать исполнителем",
				site.file, site.fn)
		}
	}

	// Голый isGroupWorkerR92 допустим только там, где он и есть ответ.
	allowedRaw := map[string]bool{
		"backend_group_r92.go":    true, // определение предиката
		"donor_exclusive_r304.go": true, // объединённый предикат
		"backend_disable_r250.go": true, // диагностика «а не выключил ли я всё сам»
	}
	for key, fn := range funcs {
		if !fn.callsRawWorkerOnly {
			continue
		}
		file := key[:strings.LastIndex(key, ".")]
		if allowedRaw[file] {
			continue
		}
		t.Errorf("%s: %s спрашивает isGroupWorkerR92 напрямую — путь выбора обязан спрашивать "+
			"cannotHostModelR304 (иначе донор проходит мимо фильтра)", file, fn.name)
	}
}

type r304FuncInfo struct {
	name                 string
	callsSharedPredicate bool
	callsRawWorkerOnly   bool
}

// parseBalancerFuncsR304 — разобрать не-тестовые файлы пакета и собрать вызовы
// интересующих предикатов по функциям.
func parseBalancerFuncsR304(t *testing.T, dir string) map[string]r304FuncInfo {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	out := map[string]r304FuncInfo{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			info := r304FuncInfo{name: fd.Name.Name}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "cannotHostModelR304", "donorHostsForbiddenR304":
					info.callsSharedPredicate = true
				case "isGroupWorkerR92":
					info.callsRawWorkerOnly = true
				}
				return true
			})
			out[name+"."+info.name] = info
		}
	}
	return out
}
