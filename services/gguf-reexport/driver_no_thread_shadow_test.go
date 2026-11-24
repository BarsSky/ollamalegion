// driver_no_thread_shadow_test.go — страж водителя ре-экспорта (R732, Фаза 3).
//
// ЖИВОЙ ДЕФЕКТ, НАЙДЕННЫЙ ПРОГОНОМ КОНВЕРТАЦИИ. В `py/reexport_job.py` класс
// прогресса записи объявлен как `threading.Thread` и в `__init__` присваивал
//
//	self._stop = stop            # threading.Event
//
// В CPython `Thread._stop` — ВНУТРЕННИЙ МЕТОД, который `Thread.join()` вызывает
// на завершённом потоке (`_wait_for_tstate_lock` → `self._stop()`). Присваивание
// затирает его объектом Event, и `join()` падает:
//
//	File ".../threading.py", line 1171, in _wait_for_tstate_lock
//	    self._stop()
//	TypeError: 'Event' object is not callable
//
// То есть КАЖДАЯ задача `hf_to_gguf` падала в конце — уже после того, как
// конвертер отработал: драйвер ловил TypeError, писал водителю
// «непредвиденная ошибка драйвера» и задача получала `converter_failed`, даже
// когда файл был записан. Симптом коварный: `normalize` (без этого класса)
// работал, поэтому «конвертация не проверена» и «конвертация сломана» выглядели
// одинаково.
//
// ПОЧЕМУ ОБЫЧНЫЙ PYTHON-ЮНИТ ЗДЕСЬ НЕ ГОДИТСЯ. `go test ./services/gguf-reexport/`
// не гарантирует python3 в окружении CI; тест, который «пропускается, если
// python недоступен», на гейте ничего не ловит. Зато сама ошибка — это ЗАПРЕТ
// ИМЕНИ: водитель не вправе присваивать себе атрибуты, которые есть у
// `threading.Thread`. Проверка исходника ловит регресс всегда и не зависит от
// интерпретатора, а поведение (сборка конвертации) проверяется живым прогоном.
package main

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// lookPathPythonR732 — первый доступный интерпретатор (windows: python, unix: python3).
func lookPathPythonR732() (string, error) {
	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", exec.ErrNotFound
}

// runPythonR732 — выполнить скрипт из корня пакета и вернуть объединённый вывод.
func runPythonR732(py, script string) (string, error) {
	cmd := exec.Command(py, "-c", script)
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	return string(out), err
}


// threadShadowForbiddenR732 — атрибуты threading.Thread, которые водитель не
// должен перекрывать. Список узкий и объяснённый: именно `_stop` стал живым
// дефектом, остальные — внутренние методы CPython, которые join/is_alive зовут
// на завершении потока.
var threadShadowForbiddenR732 = []string{"_stop", "_wait_for_tstate_lock", "_is_stopped", "_tstate_lock"}

// TestR732_DriverDoesNotShadowThreadInternals — водитель не перекрывает
// внутренние атрибуты threading.Thread.
func TestR732_DriverDoesNotShadowThreadInternals(t *testing.T) {
	src, err := os.ReadFile("py/reexport_job.py")
	if err != nil {
		t.Fatalf("не читается водитель: %v", err)
	}
	if len(src) == 0 {
		t.Fatal("водитель пуст — проверять нечего, это не «зелёный» случай")
	}
	text := string(src)
	for _, name := range threadShadowForbiddenR732 {
		// `self._stop = ...` (присваивание) — запрещено. `self._stop_evt` не
		// совпадает: регулярное выражение требует конец имени.
		re := regexp.MustCompile(`self\.` + regexp.QuoteMeta(name) + `\s*=[^=]`)
		for i, line := range strings.Split(text, "\n") {
			// Комментарии не код: в них дефект объясняется («было self._stop =
			// stop»), и страж не должен краснеть на собственном объяснении.
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if loc := re.FindStringIndex(line); loc != nil {
				t.Fatalf("py/reexport_job.py:%d: присваивание self.%s перекрывает внутренний атрибут "+
					"threading.Thread; в CPython Thread.join() вызывает self._stop() на завершённом "+
					"потоке, и задача hf_to_gguf падает с TypeError: 'Event' object is not callable. "+
					"Переименуйте поле (например в _stop_evt)", i+1, name)
			}
		}
	}
}

// TestR732_ProgressThreadCanBeJoined — если python3 есть, класс прогресса можно
// запустить и присоединить. Это поведенческая половина стража: исходник может
// быть «чистым», а логика — ломающей join по другой причине.
//
// Без python3 тест пропускается, поэтому ГЭЙТОМ считается статическая проверка
// выше; здесь важно, что на машине разработчика и в полном образе работает ровно
// тот код, который уедет в релиз.
func TestR732_ProgressThreadCanBeJoined(t *testing.T) {
	py, err := lookPathPythonR732()
	if err != nil {
		t.Skip("python3 недоступен: поведенческая проверка пропущена, статический страж выше остаётся")
	}
	// В CPython 3.13 внутренности Thread.join изменились (join больше не зовёт
	// self._stop()), поэтому поведенческая проверка имеет смысл только на
	// интерпретаторе, который стоит в образе ре-экспорта (3.12).
	verOut, err := runPythonR732(py, "import sys; print('%d.%d' % sys.version_info[:2])")
	if err != nil {
		t.Fatalf("не удалось узнать версию python: %v (%s)", err, verOut)
	}
	if strings.TrimSpace(verOut) != "3.12" {
		t.Skipf("python %s: реализация Thread.join другая, поведенческая проверка относится к 3.12 (образ ре-экспорта)",
			strings.TrimSpace(verOut))
	}
	script := `
import importlib.util, sys, threading, time
spec = importlib.util.spec_from_file_location("driver", "py/reexport_job.py")
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)
stop = threading.Event()
t = mod._ConvertProgress("/nonexistent-file", 100, stop, interval=0.05)
t.start()
stop.set()
t.join(timeout=5)
if t.is_alive():
    print("thread still alive after join")
    sys.exit(1)
print("joined ok")
`
	out, err := runPythonR732(py, script)
	if err != nil {
		t.Fatalf("прогресс-поток не присоединяется (%v):\n%s", err, out)
	}
}
