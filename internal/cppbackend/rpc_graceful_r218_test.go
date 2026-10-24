//go:build llama_stub

// rpc_graceful_r218_test.go — R218 (2026-10-21).
//
// ЖИВОЙ ДЕФЕКТ. Донор пропал во время загрузки модели — мастер-воркер умер по
// SIGABRT и перезапустился вместе со всеми загруженными моделями. В логе:
//
//	/build/llama.cpp/ggml/src/ggml-rpc/ggml-rpc.cpp:547:
//	  Failed to connect to 192.0.2.10:50052
//	SIGABRT: abort ... signal arrived during cgo execution
//
// Причина: llama.cpp rpc_dispatcher::start делает GGML_ABORT, если узел не
// отвечает. Донор — внешняя машина, её выключение не должно ронять мастер.
//
// Контракт R218:
//  1. патч 0002 к сабмодулю превращает неудачное подключение в исключение,
//     которое ловится в get_dispatcher (узел просто не участвует);
//  2. все Dockerfile'ы применяют ОБА патча (0001 keepalive + 0002 graceful);
//  3. мост проверяет TCP до llama.cpp (там connect блокирующий, без таймаута).
package cppbackend

import (
	"os"
	"strings"
	"testing"
)

const r218PatchPath = "../../docker/cppworker/patches/0002-rpc-graceful-connect.patch"

func TestR218_GracefulConnectPatchExists(t *testing.T) {
	raw, err := os.ReadFile(r218PatchPath)
	if err != nil {
		t.Fatalf("патч R218 не найден (%s): %v", r218PatchPath, err)
	}
	patch := string(raw)

	if !strings.Contains(patch, "ggml/src/ggml-rpc/ggml-rpc.cpp") {
		t.Error("патч должен править ggml-rpc.cpp")
	}
	if !strings.Contains(patch, "std::runtime_error") {
		t.Error("нет исключения вместо GGML_ABORT при неудачном подключении")
	}
	if !strings.Contains(patch, "return nullptr") {
		t.Error("get_dispatcher должен возвращать nullptr для недоступного узла")
	}
	if !strings.Contains(patch, "dispatcher == nullptr") {
		t.Error("get_device_count должен обрабатывать nullptr (иначе null deref)")
	}
}

func TestR218_DockerfilesApplyBothRpcPatches(t *testing.T) {
	// R221: список файлов заменён на glob по каталогу. Проверяем именно механизм:
	// каталог копируется целиком и применяется циклом, а наличие обоих патчей в
	// каталоге проверяют тесты R214/R218 на сами файлы.
	for _, path := range r214Dockerfiles() {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("не читается %s: %v", path, err)
		}
		text := string(raw)
		if !strings.Contains(text, "COPY docker/cppworker/patches /tmp/patches") {
			t.Errorf("%s не копирует каталог патчей", path)
		}
		if !strings.Contains(text, "for p in /tmp/patches/*.patch") {
			t.Errorf("%s не применяет все патчи каталога (ожидается `for p in /tmp/patches/*.patch`)", path)
		}
	}
}

// Мост обязан проверять TCP до llama.cpp: connect внутри RPC-клиента блокирующий
// и без таймаута, а на выключенном хосте это десятки секунд ожидания.
func TestR218_BridgeProbesRPCEndpoints(t *testing.T) {
	raw, err := os.ReadFile("../../c/bridge/bridge.c")
	if err != nil {
		t.Fatalf("не читается bridge.c: %v", err)
	}
	src := string(raw)
	if !strings.Contains(src, "bridge_rpc_tcp_probe") {
		t.Fatal("в мосте нет TCP-предпроверки узлов раскладки")
	}
	probeIdx := strings.Index(src, "!bridge_rpc_tcp_probe(endpoint")
	if probeIdx < 0 {
		t.Fatal("TCP-предпроверка объявлена, но не вызывается в цикле узлов")
	}
	// Проверка обязана стоять ДО add_server — иначе abort в llama.cpp случится раньше.
	loopStart := strings.LastIndex(src[:probeIdx], "for (int i = 0; i < config->rpc_endpoints_len")
	addIdx := strings.Index(src[probeIdx:], "add_server(endpoint)")
	if loopStart < 0 || addIdx < 0 {
		t.Fatal("не найден цикл узлов или вызов add_server")
	}
}
