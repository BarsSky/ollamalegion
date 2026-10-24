//go:build llama_stub

// rpc_keepalive_patch_r214_test.go — R214 (2026-10-21).
//
// Почему тест вообще нужен. Правка живёт НЕ в Go-коде, а в патче к сабмодулю
// llama.cpp, который применяется на сборке образа. Такой шаг легко потерять при
// переписывании Dockerfile: сборка не падает, rpc-server просто снова начинает
// держать VRAM после смерти мастера без закрытия сокета (питание, кабель), а
// мастер получает отказ узла и раскладку на локальную карту.
//
// Контракт R214:
//  1. патч существует и ставит SO_KEEPALIVE на сокет раскладки (transport.cpp);
//  2. все Dockerfile'ы, которые собирают llama.cpp, применяют его через
//     `git apply` до сборки.
package cppbackend

import (
	"os"
	"strings"
	"testing"
)

const r214PatchPath = "../../docker/cppworker/patches/0001-rpc-tcp-keepalive.patch"

// r214Dockerfiles — все сборки cppworker, которые компилируют llama.cpp.
func r214Dockerfiles() []string {
	return []string{
		"../../docker/cppworker/Dockerfile.gpu",
		"../../docker/cppworker/Dockerfile.gpu.86",
		"../../docker/cppworker/Dockerfile.cpu",
		"../../docker/cppworker/Dockerfile.r66",
	}
}

func TestR214_RPCKeepalivePatchExists(t *testing.T) {
	raw, err := os.ReadFile(r214PatchPath)
	if err != nil {
		t.Fatalf("патч R214 не найден (%s): %v", r214PatchPath, err)
	}
	patch := string(raw)

	if !strings.Contains(patch, "ggml/src/ggml-rpc/transport.cpp") {
		t.Error("патч должен править сокет раскладки (ggml/src/ggml-rpc/transport.cpp)")
	}
	if !strings.Contains(patch, "SO_KEEPALIVE") {
		t.Error("патч не ставит SO_KEEPALIVE: полумёртвая сессия снова будет держать VRAM узла")
	}
	// Linux - основной рантайм донора; без этих параметров keepalive срабатывает
	// по умолчанию 2 часа 11 минут, что для «узел занят весами» неотличимо от
	// вечной блокировки.
	if !strings.Contains(patch, "TCP_KEEPIDLE") {
		t.Error("нет TCP_KEEPIDLE: idle-таймаут останется системным (~7200 с)")
	}
	if !strings.Contains(patch, "TCP_KEEPCNT") || !strings.Contains(patch, "TCP_KEEPINTVL") {
		t.Error("нет TCP_KEEPCNT/TCP_KEEPINTVL: число и интервал проб останутся системными")
	}
}

func TestR214_DockerfilesApplyKeepalivePatch(t *testing.T) {
	for _, path := range r214Dockerfiles() {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("не читается %s: %v", path, err)
		}
		text := string(raw)
		// R218: Dockerfile'ы копируют КАТАЛОГ патчей и применяют их в цикле
		// (0001 keepalive + 0002 graceful + 0003 session loss). R221: список
		// файлов заменён на glob, иначе каждый новый патч надо дописывать в
		// четыре Dockerfile и легко забыть один.
		if !strings.Contains(text, "COPY docker/cppworker/patches /tmp/patches") {
			t.Errorf("%s не копирует каталог патчей в образ — llama.cpp соберётся без keepalive", path)
		}
		if !strings.Contains(text, "for p in /tmp/patches/*.patch") {
			t.Errorf("%s не применяет все патчи каталога (ожидается `for p in /tmp/patches/*.patch`)", path)
		}
		if !strings.Contains(text, `git apply "$p"`) {
			t.Errorf("%s не применяет патчи (git apply в цикле) до сборки llama.cpp", path)
		}
	}
}

// Контроль: патчи применяются ПОСЛЕ `COPY c/llama.cpp` (в каталоге сабмодуля), иначе
// git apply рухнет на сборке. Проверяем порядок в файле.
func TestR214_PatchAppliedAfterLlamaCopy(t *testing.T) {
	for _, path := range r214Dockerfiles() {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("не читается %s: %v", path, err)
		}
		text := string(raw)
		copyIdx := strings.Index(text, "COPY c/llama.cpp")
		applyIdx := strings.Index(text, `git apply "$p"`)
		if copyIdx < 0 || applyIdx < 0 {
			t.Errorf("%s: не найдены COPY c/llama.cpp или git apply патчей", path)
			continue
		}
		if applyIdx < copyIdx {
			t.Errorf("%s: патчи применяются до COPY c/llama.cpp — git apply не найдёт файл", path)
		}
	}
}
