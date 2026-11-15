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

// R493 (2026-11-15, обновление llama.cpp до 1.0.0): наш патч 0002 снят —
// upstream реализовал ровно тот же контракт сам. Проверка теперь смотрит в
// САБМОДУЛЬ (фактический upstream), а не в текст патча: иначе она требует файла,
// которого больше нет, и падает на ровном месте.
//
// Контракт R218 остаётся тем же: недоступный RPC-узел не убивает мастер.
const r218SourcePath = "../../c/llama.cpp/ggml/src/ggml-rpc/ggml-rpc.cpp"

func TestR218_GracefulConnectUpstream(t *testing.T) {
	raw, err := os.ReadFile(r218SourcePath)
	if err != nil {
		// В CI сабмодуль llama.cpp не вычекан (workflow: submodules:false) —
		// проверка осмысленна только когда он на месте (так же, как R221).
		t.Skipf("сабмодуль llama.cpp недоступен (%s): %v", r218SourcePath, err)
	}
	src := string(raw)

	if !strings.Contains(src, "bool start(const std::string & endpoint);") {
		t.Error("upstream: start() обязан возвращать bool — иначе единственный способ сообщить о недоступном узле это abort")
	}
	if !strings.Contains(src, "dispatcher->start(endpoint)") || !strings.Contains(src, "return nullptr") {
		t.Error("upstream: get_dispatcher обязан вернуть nullptr, а не падать, если узел недоступен")
	}
	if !strings.Contains(src, "dispatcher == nullptr") {
		t.Error("upstream: get_device_count обязан обрабатывать nullptr (иначе null deref)")
	}
	// Подключение НЕ должно прерывать процесс: GGML_ABORT на connect не осталось.
	if strings.Contains(src, `GGML_ABORT("Failed to connect`) {
		t.Error("upstream всё ещё делает GGML_ABORT при неудачном подключении — R218 вернулся")
	}
}

func TestR218_OurPatchIsGoneOnPurpose(t *testing.T) {
	// Файл удалён ОСОЗНАННО при обновлении до 1.0.0. Тест держит это решение:
	// если кто-то вернёт патч, он должен снова проверить, не дублирует ли он upstream.
	if _, err := os.Stat("../../docker/cppworker/patches/0002-rpc-graceful-connect.patch"); err == nil {
		t.Error("патч 0002 вернулся, хотя upstream его уже реализовал — " +
			"проверьте, не дублирует ли он код llama.cpp")
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
