//go:build llama_stub

// image_backend_selection_r88_test.go — R88 (2026-10-08).
//
// ЖИВОЙ СЛУЧАЙ. На стенде две машины: локальный `imageworker` (модель
// загружена, 2 bundle'а на диске) и удалённый `IMAGEWORKER-34`
// (192.0.2.11) — он зарегистрирован и здоров, но каталог моделей ПУСТ.
// Прежний выбор «по ресурсам» отдавал удалённому КАЖДЫЙ запрос (метрик у него
// нет → выглядел свободным), гейт отвечал image_model_not_loaded, и генерация не
// работала целиком, хотя на локальном воркере всё было готово.
//
// Правило: бэкенд с пустым каталогом не может обслужить генерацию НИКОГДА —
// исключаем его из выбора, но ТОЛЬКО по достоверному снимку (нет данных —
// не судим) и только если остался хоть один кандидат (иначе теряем внятную
// ошибку гейта).
package balancer

import (
	"context"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// r88EmptyCatalog — ответ воркера, на котором нет ни одного bundle'а.
const r88EmptyCatalog = `{"models":[],"state":"not_loaded","current_model":""}`

// r88EmptyImageBackend — второй image-бэкенд стенда (пустой каталог).
//
// Host намеренно 127.0.0.1: адрес должен указывать на локальный httptest-стаб,
// иначе опрос каталога не дойдёт и снимок получит lastErr (а по недостоверному
// снимку мы, по правилу, решение не принимаем — см. TestR88_FailedProbeDoesNotExclude).
func r88EmptyImageBackend(t *testing.T, stub *imgResStub) types.Backend {
	t.Helper()
	return types.Backend{
		ID:                "img-empty",
		Name:              "empty image worker",
		Host:              "127.0.0.1",
		ImagePort:         stub.port(t),
		Type:              types.BackendTypeImage,
		Status:            types.StatusHealthy,
		MaxConcurrentReqs: 2,
	}
}

// TestR88_SelectionSkipsImageBackendWithoutModels — главный сценарий: запрос
// уходит рабочему воркеру, а не «свободному, но пустому».
func TestR88_SelectionSkipsImageBackendWithoutModels(t *testing.T) {
	empty := newImgResStub(t)
	empty.setModels(r88EmptyCatalog)

	p, good := newImgResProxy(t, toolTestImageSettings(), r88EmptyImageBackend(t, empty))
	good.setModels(imgResWorkerTwoModels) // img-1: модель загружена + модель на диске

	res := p.imageResources()
	if s := res.refresh(context.Background(), "img-1"); s == nil || len(s.modelNames()) == 0 {
		t.Fatalf("снимок рабочего воркера пуст: %+v", s)
	}
	if s := res.refresh(context.Background(), "img-empty"); s == nil || len(s.modelNames()) != 0 {
		t.Fatalf("снимок пустого воркера должен быть без моделей: %+v", s)
	}

	ir := NewImageRouter(p)
	exclude := ir.backendsWithoutModels()
	if got := ir.selectImageBackend(); got != "img-1" {
		t.Fatalf("выбран %q, want img-1: у img-empty нет ни одной модели, запрос там невыполним", got)
	}
	if !exclude["img-empty"] {
		t.Errorf("пустой бэкенд не исключён: %v", exclude)
	}
	if exclude["img-1"] {
		t.Errorf("рабочий бэкенд исключён напрасно: %v", exclude)
	}
}

// TestR88_ModelsOnDiskAreEnoughToStayACandidate — «модель на диске» тоже
// кандидат: воркер умеет её поднять (ленивая загрузка/инструмент), а вот
// ПУСТОЙ каталог — нет. Не путать эти два случая.
func TestR88_ModelsOnDiskAreEnoughToStayACandidate(t *testing.T) {
	empty := newImgResStub(t)
	empty.setModels(r88EmptyCatalog)

	p, onlyDisk := newImgResProxy(t, toolTestImageSettings(), r88EmptyImageBackend(t, empty))
	onlyDisk.setModels(imgResWorkerOnlyDiskModel) // есть на диске, не загружена

	res := p.imageResources()
	res.refresh(context.Background(), "img-1")
	res.refresh(context.Background(), "img-empty")

	ir := NewImageRouter(p)
	if got := ir.selectImageBackend(); got != "img-1" {
		t.Fatalf("выбран %q, want img-1 (у него есть bundle, пусть и не загруженный)", got)
	}
	if exclude := ir.backendsWithoutModels(); exclude["img-1"] {
		t.Errorf("бэкенд с моделью на диске исключён: %v", exclude)
	}
}

// TestR88_NoSnapshotMeansNoJudgement — без достоверного снимка не исключаем:
// иначе исправный узел вылетал бы из выбора до первого опроса.
func TestR88_NoSnapshotMeansNoJudgement(t *testing.T) {
	empty := newImgResStub(t)
	empty.setModels(r88EmptyCatalog)
	p, _ := newImgResProxy(t, toolTestImageSettings(), r88EmptyImageBackend(t, empty))

	res := p.imageResources()
	res.mu.Lock()
	res.snapshots = map[string]*imageBackendMetrics{}
	res.mu.Unlock()

	if exclude := NewImageRouter(p).backendsWithoutModels(); exclude != nil {
		t.Fatalf("без снимков исключать нельзя, получено %v", exclude)
	}
}

// TestR88_FailedProbeDoesNotExclude — опрос упал (lastErr) ⇒ данные
// недостоверны, решение не принимаем.
func TestR88_FailedProbeDoesNotExclude(t *testing.T) {
	empty := newImgResStub(t)
	empty.setModels(r88EmptyCatalog)
	p, _ := newImgResProxy(t, toolTestImageSettings(), r88EmptyImageBackend(t, empty))

	res := p.imageResources()
	res.store(&imageBackendMetrics{backendID: "img-empty", lastErr: "dial tcp 192.0.2.11:18093: connect: connection refused"})

	if exclude := NewImageRouter(p).backendsWithoutModels(); exclude["img-empty"] {
		t.Fatalf("бэкенд с упавшим опросом исключён: %v — решение принято по недостоверным данным", exclude)
	}
}

// TestR88_AllEmptyStillYieldsACandidate — когда моделей нет нигде, выбираем
// хоть кого-то: гейт обязан отдать image_model_not_loaded С ПОДСКАЗКОЙ, а не
// «нет бэкенда», иначе оператор теряет причину.
func TestR88_AllEmptyStillYieldsACandidate(t *testing.T) {
	empty := newImgResStub(t)
	empty.setModels(r88EmptyCatalog)

	p, first := newImgResProxy(t, toolTestImageSettings(), r88EmptyImageBackend(t, empty))
	first.setModels(r88EmptyCatalog)

	res := p.imageResources()
	res.refresh(context.Background(), "img-1")
	res.refresh(context.Background(), "img-empty")

	if got := NewImageRouter(p).selectImageBackend(); got == "" {
		t.Fatal("при пустых каталогах нужен кандидат, иначе причина отказа не будет объяснена")
	}
}

// TestR88_SelectByResourcesExcluding_NilExcludeIsIdentity — исключение без
// исключений не должно менять прежнее поведение выбора.
func TestR88_SelectByResourcesExcluding_NilExcludeIsIdentity(t *testing.T) {
	p, _ := newImgResProxy(t, toolTestImageSettings())
	allowed := []types.BackendType{types.BackendTypeImage}
	if a, b := p.selectByResourcesExcluding(nil, allowed), p.selectByResources(allowed); a != b {
		t.Fatalf("selectByResourcesExcluding(nil) = %q, selectByResources = %q — nil-exclude обязан быть идентичен", a, b)
	}
}
