//go:build llama_stub

// list_models_race_r493e_test.go — R493e (2026-11-17).
//
// ЗАЧЕМ. Красный CI (`Test (cmd packages, -race)`) назвал гонку, которую никто не
// искал:
//
//	WARNING: DATA RACE
//	Read at 0x... by goroutine 1999:
//	  cppbackend.(*Backend).ListModels()          backend.go:2611
//	Previous write at 0x... by goroutine 1994:
//	  cppbackend.(*Backend).LoadModelWithOpts()   backend.go:1556
//
// Смысл: /api/models КОПИРУЕТ inst.info под b.mu.RLock (так задумано — снимок
// должен отвечать мгновенно даже во время генерации), а загрузчик писал в тот же
// inst.info ДО взятия b.mu.Lock (tensor split, состояние mmproj, LayoutDegraded).
// Чтение под RLock без записи под Lock — не синхронизация, а гонка по контракту,
// который остальные 170 строк метода уже соблюдали.
//
// ПОЧЕМУ ТЕСТЫ ПОСТРОЕНЫ НА ОДНОМ ЧТЕНИИ И ОДНОЙ ЗАПИСИ. Детектор гонок хранит
// ограниченную историю обращений к слову (несколько ячеек). Цикл из сотен
// записей вытесняет ранние обращения, и позднее чтение может уже не дать пары —
// тест «на гонку», построенный на цикле, ложно зеленеет (проверено здесь же:
// цикл проходил при заведомо гоночной реализации). Поэтому обе стороны делают
// РОВНО по одному обращению к полю, синхронно выпущенные одним закрытием канала:
// отношение «случилось-до» между ними не появляется, и детектор обязан назвать
// пару, если реализация неверна.
package cppbackend

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
)

// racePairR493e выпускает двух участников одновременно и ждёт завершения.
func racePairR493e(t *testing.T, reader, writer func()) {
	t.Helper()
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); <-start; reader() }()
	go func() { defer wg.Done(); <-start; writer() }()
	close(start)
	wg.Wait()
}

// TestR493e_ListModelsDuringLoadNoRace — снимок и публикация под одним замком.
func TestR493e_ListModelsDuringLoadNoRace(t *testing.T) {
	b := NewBackend(Config{ModelsDir: t.TempDir()})
	b.mu.Lock()
	b.models["m"] = &modelInstance{info: ModelInfo{Name: "m", State: StateLoading}}
	b.mu.Unlock()

	racePairR493e(t,
		func() { _ = b.ListModels() },
		func() {
			// Так пишет загрузчик после публикации инстанса: под b.mu.
			b.mu.Lock()
			defer b.mu.Unlock()
			if inst := b.models["m"]; inst != nil {
				inst.info.LayoutDegraded = true
				inst.info.LayoutDegradedReason = "layout не применился"
				inst.info.LoadingProgressPct = 42
			}
		},
	)

	// Не пустой тест: снимок обязан ВИДЕТЬ опубликованное значение.
	models := b.ListModels()
	if len(models) != 1 {
		t.Fatalf("ListModels = %d моделей, ожидалась 1", len(models))
	}
	if !models[0].LayoutDegraded || models[0].LayoutDegradedReason == "" {
		t.Errorf("ListModels не видит опубликованный LayoutDegraded: %+v", models[0])
	}
	if models[0].LoadingProgressPct != 42 {
		t.Errorf("LoadingProgressPct = %d, ожидалось 42", models[0].LoadingProgressPct)
	}
}

// TestR493e_ModelStateUsesSameLockAsLoader — состояние модели читается тем же
// замком, что и пишется. Раньше читали под inst.mu: разные замки — такая же
// гонка, только её ещё не поймали.
func TestR493e_ModelStateUsesSameLockAsLoader(t *testing.T) {
	b := NewBackend(Config{ModelsDir: t.TempDir()})
	b.mu.Lock()
	b.models["m"] = &modelInstance{info: ModelInfo{Name: "m", State: StateLoading}}
	b.mu.Unlock()

	racePairR493e(t,
		func() { _, _ = b.ModelState("m") },
		func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if inst := b.models["m"]; inst != nil {
				inst.info.State = StateLoaded
			}
		},
	)

	// Не пустой тест: состояние реально доехало.
	if state, ok := b.ModelState("m"); !ok || state != StateLoaded {
		t.Fatalf("ModelState = (%v, %v), ожидалось (loaded, true)", state, ok)
	}
}

// TestR493e_StateReadersUseRegistryLock — сторож того же класса, что и гонка в
// ListModels, но для читателей состояния (ModelState, заявки загрузки).
//
// ЗАЧЕМ СТАТИЧЕСКИ. Динамический тест с двумя одновременными обращениями не
// гарантирует, что детектор увидит пару: mutex-ребро «Unlock → Lock» может
// упорядочить обращения, и тогда красного не будет даже у гоночной реализации
// (проверено: мутант с `inst.mu` в ModelState проходил -race). Поэтому контракт
// проверяется по исходнику: читатели состояния обязаны держать b.mu, а не
// inst.mu — загрузчик пишет состояние под b.mu.
func TestR493e_StateReadersUseRegistryLock(t *testing.T) {
	cases := []struct {
		file    string
		fnStart string
	}{
		{"model_ready_wait.go", "func (b *Backend) ModelState(name string) (LoadState, bool) {"},
	}
	for _, c := range cases {
		raw, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatalf("не читается %s: %v", c.file, err)
		}
		src := string(raw)
		start := strings.Index(src, c.fnStart)
		if start < 0 {
			t.Fatalf("%s: не найдена функция %q — сторож потерял точку отсчёта", c.file, c.fnStart)
		}
		end := strings.Index(src[start:], "\n}\n")
		if end < 0 {
			t.Fatalf("%s: не найден конец функции %q", c.file, c.fnStart)
		}
		body := src[start : start+end]

		if !strings.Contains(body, "b.mu.RLock()") {
			t.Errorf("%s: %s не берёт b.mu.RLock — состояние читается без замка загрузчика (гонка R493e)",
				c.file, c.fnStart)
		}
		if !strings.Contains(body, "defer b.mu.RUnlock()") {
			t.Errorf("%s: %s не держит b.mu.RLock до конца (нужен defer) — чтение уходит за пределы замка (гонка R493e)",
				c.file, c.fnStart)
		}
		if strings.Contains(body, "inst.mu.Lock()") {
			t.Errorf("%s: %s синхронизируется через inst.mu, а загрузчик пишет состояние под b.mu — "+
				"два разных замка не дают отношения «случилось-до» (гонка R493e)", c.file, c.fnStart)
		}
	}
}

// TestR493e_LoaderWritesToInfoOnlyUnderRegistryLock — сторож ровно той строки,
// которую назвал -race: между публикацией инстанса в b.models и большой секцией
// под b.mu.Lock (её начало — «Обновляем информацию», сразу после GetMetadata)
// загрузчик НЕ имеет права писать в inst.info: в этот момент снимки уже могут
// читать его под b.mu.RLock.
//
// Статическая проверка, потому что живой путь требует реальной C-bridge загрузки
// GGUF: проверяется не «наличие функции», а порядок в файле — место, где дефект и
// был (backend.go:1556 писал LayoutDegraded до замка, и CI это поймал).
func TestR493e_LoaderWritesToInfoOnlyUnderRegistryLock(t *testing.T) {
	// R509 (шаг R4): загрузчик вынесен из backend.go в backend_load.go, поэтому
	// сторож ищет файл с публикацией инстанса по СОДЕРЖИМОМУ, а не по имени файла.
	// Проверка не ослаблена: те же два якоря, та же проверка окна между ними.
	src, path, err := findPackageSourceContaining(t, "b.models[name] = inst")
	if err != nil {
		t.Fatalf("не найден файл с публикацией инстанса: %v", err)
	}

	publish := strings.Index(src, "b.models[name] = inst")
	if publish < 0 {
		t.Fatalf("%s: не найдена публикация инстанса (`b.models[name] = inst`) — сторож потерял точку отсчёта", path)
	}
	// Конец окна — начало секции, которую загрузчик выполняет под b.mu.
	const tailAnchor = "// Обновляем информацию"
	tail := strings.Index(src[publish:], tailAnchor)
	if tail < 0 {
		t.Fatalf("не найден якорь %q после публикации инстанса — сторож потерял точку отсчёта", tailAnchor)
	}
	window := src[publish : publish+tail]

	// Разрешённая запись в этом окне — только счётчик прогресса: он под своим
	// коротким b.mu.Lock/b.mu.Unlock (goroutine прогресса, см. комментарий R215).
	found := 0
	for _, line := range strings.Split(window, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		idx := strings.Index(trimmed, "inst.info.")
		if idx < 0 {
			continue
		}
		rest := trimmed[idx:]
		if !strings.Contains(rest, "=") {
			continue
		}
		if strings.HasPrefix(rest, "inst.info.LoadingProgressPct = pct") {
			continue // под b.mu в goroutine прогресса (R215)
		}
		found++
		t.Errorf("запись в inst.info ДО секции под b.mu после публикации инстанса: %q\n"+
			"инстанс уже виден снимкам (/api/models копирует его под b.mu.RLock) — "+
			"это и была гонка R493e; считайте значение в локальную переменную и "+
			"публикуйте под замком", trimmed)
	}
	if found == 0 {
		t.Logf("окно публикации проверено: записей в inst.info нет (кроме прогресса под b.mu), строк %d",
			len(strings.Split(window, "\n")))
	}
}

// findPackageSourceContaining — исходник того файла ПАКЕТА, в котором встречается
// якорь (R509, шаг R4). Нужен сторожам, которые раньше читали backend.go по имени:
// после разбиения по ответственности код переехал, а проверяемое свойство — нет.
func findPackageSourceContaining(t *testing.T, anchor string) (src, path string, err error) {
	t.Helper()
	entries, readErr := os.ReadDir(".")
	if readErr != nil {
		return "", "", readErr
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names) // детерминированный порядок обхода
	for _, n := range names {
		raw, readFileErr := os.ReadFile(n)
		if readFileErr != nil {
			continue
		}
		if strings.Contains(string(raw), anchor) {
			return string(raw), n, nil
		}
	}
	return "", "", fmt.Errorf("якорь %q не встречается ни в одном боевом файле пакета", anchor)
}
