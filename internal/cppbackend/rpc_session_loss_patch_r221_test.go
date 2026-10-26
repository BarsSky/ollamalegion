//go:build llama_stub

// rpc_session_loss_patch_r221_test.go — R221 (2026-10-21).
//
// Почему тест нужен. Если узел раскладки умирает ПОСРЕДИ сессии (контейнер
// пересоздали, rpc-server перезапустился, сокет порван), мастер раньше падал:
//
//	ggml-rpc.cpp:567: Remote RPC server crashed or returned malformed response
//	SIGABRT -> контейнер cppworker перезапускается, клиент видит обрыв
//
// Причина — RPC_STATUS_ASSERT в клиентском потоке диспетчера. Правка живёт в
// патче к сабмодулю (не в Go-коде), поэтому её легко потерять: сборка не
// упадёт, просто мастер снова начнёт падать вместо явной ошибки.
//
// Контракт R221:
//  1. abort в диспетчере заменён на метку «сессия потеряна» + ошибку вызывающему;
//  2. очередь продолжает отдавать completion, иначе вызывающий повиснет навсегда
//     в future.wait() (и abort превратится в deadlock);
//  3. graph_compute возвращает GGML_STATUS_FAILED, то есть llama_decode
//     возвращает ошибку и воркер отдаёт клиенту внятный ответ, а не мусор;
//  4. патч применяется на сборке (glob-цикл в Dockerfile'ах, см. R214).
package cppbackend

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const r221PatchPath = "../../docker/cppworker/patches/0003-rpc-session-loss.patch"

func r221ReadPatch(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(r221PatchPath)
	if err != nil {
		t.Fatalf("патч R221 не найден (%s): %v", r221PatchPath, err)
	}
	return string(raw)
}

func TestR221_SessionLossPatchRemovesAbort(t *testing.T) {
	patch := r221ReadPatch(t)

	if !strings.Contains(patch, "ggml/src/ggml-rpc/ggml-rpc.cpp") {
		t.Fatal("патч должен править клиент раскладки (ggml/src/ggml-rpc/ggml-rpc.cpp)")
	}
	// Старый контракт: abort по любому сбою обмена. Он должен исчезнуть.
	if !strings.Contains(patch, "-#define RPC_STATUS_ASSERT") {
		t.Error("патч не убирает макрос RPC_STATUS_ASSERT — abort на потере сессии останется")
	}
	for _, line := range strings.Split(patch, "\n") {
		if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		if strings.Contains(line, "GGML_ABORT") {
			t.Errorf("патч добавляет GGML_ABORT: потеря узла снова убьёт мастер (%s)", strings.TrimSpace(line))
		}
	}
}

func TestR221_SessionLossPatchKeepsQueueDrained(t *testing.T) {
	patch := r221ReadPatch(t)

	// Метка сессии в диспетчере.
	if !strings.Contains(patch, "std::atomic_bool failed_flag") {
		t.Error("нет флага failed_flag: диспетчер не помнит, что сессия потеряна")
	}
	// Сбой обмена помечается, а не прерывает процесс.
	if !strings.Contains(patch, `fail("command "`) {
		t.Error("сбой команды не помечается как потеря сессии")
	}
	// КРИТИЧНО: completion должен ставиться всегда. Иначе send()/synchronize()
	// повиснут в future.wait() и мы получим deadlock вместо abort.
	if !strings.Contains(patch, " msg_ptr->completion.set_value();") {
		t.Error("патч убрал set_value() — вызывающий повиснет навсегда вместо ошибки")
	}
	// Буфер ответа обнуляется, чтобы вызывающий не читал неинициализированную
	// память (device_count, base_ptr и прочие ответы с output-буфером).
	if !strings.Contains(patch, "memset(msg_ptr->output, 0, msg_ptr->output_size);") {
		t.Error("ответ не обнуляется при потере сессии: чтение мусора из стека")
	}
}

func TestR221_SessionLossPatchSurfacesComputeError(t *testing.T) {
	patch := r221ReadPatch(t)

	if !strings.Contains(patch, "if (rpc_ctx->dispatcher->failed()) {") {
		t.Fatal("graph_compute не проверяет состояние сессии")
	}
	if !strings.Contains(patch, "return GGML_STATUS_FAILED;") {
		t.Error("graph_compute не возвращает GGML_STATUS_FAILED — llama_decode не увидит ошибку")
	}
	// Рукопожатие тоже не должно брать на себя abort: иначе узел, который принял
	// TCP и умер до HELLO, снова уронит мастер при добавлении RPC-устройства.
	if !strings.Contains(patch, "did not answer the hello handshake") {
		t.Error("сбой рукопожатия не переведён в возврат ошибки")
	}
	if strings.Contains(patch, "+    RPC_STATUS_ASSERT(status);") {
		t.Error("патч оставил RPC_STATUS_ASSERT в рукопожатии")
	}
}

// Патч обязан иметь те же переводы строк, что и правимый файл сабмодуля. На
// Windows-чекауте исходники CRLF; `git apply` в контейнере сборки не прощает
// несовпадения и падает `patch does not apply` — именно так первый вариант
// патча не попал в образ, хотя локально применялся.
func TestR221_PatchLineEndingsMatchSource(t *testing.T) {
	const srcPath = "../../c/llama.cpp/ggml/src/ggml-rpc/ggml-rpc.cpp"

	read := func(path string) []byte {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("не читается %s: %v", path, err)
		}
		return raw
	}
	// R245: в CI сабмодуль llama.cpp НЕ вычекан (workflow: submodules:false,
	// поэтому файла просто нет). Проверка осмысленна только когда сабмодуль на
	// месте; вместо красного CI — SKIP с объяснением.
	srcRaw, srcErr := os.ReadFile(srcPath)
	if srcErr != nil {
		if os.IsNotExist(srcErr) {
			t.Skipf("сабмодуль c/llama.cpp не вычекан (CI: submodules:false) — сравнивать переводы строк не с чем: %v", srcErr)
		}
		t.Fatalf("не читается %s: %v", srcPath, srcErr)
	}
	srcEOL := "\n"
	if strings.Contains(string(srcRaw), "\r\n") {
		srcEOL = "\r\n"
	}
	patch := string(read(r221PatchPath))
	patchEOL := "\n"
	if strings.Contains(patch, "\r\n") {
		patchEOL = "\r\n"
	}
	if srcEOL != patchEOL {
		t.Fatalf("переводы строк не совпадают: источник %q, патч %q (сборка упадёт на git apply)",
			srcEOL, patchEOL)
	}
	if mixed := strings.Contains(strings.ReplaceAll(patch, "\r\n", ""), "\n"); mixed {
		t.Error("в патче смешаны CRLF и одиночные LF")
	}
}

// Патч обязан попадать в образ тем же механизмом, что и предыдущие: каталог
// патчей копируется целиком и применяется в цикле. Явный список файлов уже
// один раз привёл к тому, что новый патч не попал в сборку.
func TestR221_DockerfilesApplyEveryPatch(t *testing.T) {
	entries, err := os.ReadDir(filepath.Dir(r221PatchPath))
	if err != nil {
		t.Fatalf("каталог патчей не читается: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.Name() == filepath.Base(r221PatchPath) {
			found = true
		}
	}
	if !found {
		t.Fatalf("в каталоге патчей нет %s", filepath.Base(r221PatchPath))
	}

	for _, path := range r214Dockerfiles() {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("не читается %s: %v", path, err)
		}
		if !strings.Contains(string(raw), "for p in /tmp/patches/*.patch") {
			t.Errorf("%s не применяет все патчи каталога — R221 может не попасть в образ", path)
		}
	}
}
